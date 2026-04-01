package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// ErrProcessDead is returned by WaitForSessionID and WaitForTurnDone when the
// process exits before the awaited event occurs.
var ErrProcessDead = errors.New("process exited")

// PermissionHandler handles permission requests from the CLI. Called
// synchronously in a separate goroutine — the caller blocks until it returns.
type PermissionHandler func(req protocol.PermissionRequest) protocol.PermResponse

// ProcessConfig holds optional configuration for StartProcessWithConfig.
// All fields are optional; zero values preserve the default behavior.
type ProcessConfig struct {
	// PermissionHandler handles can_use_tool requests from the CLI.
	// When nil, all permission requests are denied.
	PermissionHandler PermissionHandler

	// AppendSystemPrompt is passed as --append-system-prompt to the CLI.
	AppendSystemPrompt string

	// AllowedTools is passed as --allowedTools to the CLI (glob pattern).
	AllowedTools string

	// MaxTurns is passed as --max-turns to the CLI.
	MaxTurns int

	// Command replaces the default "claude" binary. Command[0] is the
	// executable and Command[1:] are prepended before the process's own flags.
	// When nil/empty, defaults to ["claude"].
	Command []string

	// ExtraArgs are additional flags appended after the process's own flags.
	ExtraArgs []string

	// OnRawEvent, when set, is called for raw streaming deltas
	// (--include-partial-messages). Used for live text/thinking display.
	OnRawEvent func(protocol.RawStreamEvent)
}

// ClaudeProcess owns a long-running claude CLI subprocess.
// It is a pure subprocess + protocol wrapper with no session or UI concerns.
type ClaudeProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	mu      sync.Mutex // protects reqSeq and pending
	reqSeq  int
	pending map[string]chan ctlRespPayload // request_id → response channel

	writeMu sync.Mutex // serializes all writes to stdin

	turnDone    chan struct{} // sent (not closed) when a result event arrives
	dead        chan struct{} // closed when process exits (by scan goroutine)
	sessionIDCh chan string   // receives the ClaudeID from the first system event

	permHandler PermissionHandler
	onEvent     func(protocol.StreamEvent)
	onRawEvent  func(protocol.RawStreamEvent)
}

// StartProcess starts a new claude CLI subprocess with default configuration.
func StartProcess(cwd string, onEvent func(protocol.StreamEvent), resume string) (*ClaudeProcess, error) {
	return StartProcessWithConfig(cwd, onEvent, resume, nil)
}

// StartProcessWithConfig starts a new claude CLI subprocess with optional
// configuration. When cfg is nil, behaves identically to StartProcess.
func StartProcessWithConfig(cwd string, onEvent func(protocol.StreamEvent), resume string, cfg *ProcessConfig) (*ClaudeProcess, error) {
	binCmd := []string{"claude"}
	if cfg != nil && len(cfg.Command) > 0 {
		binCmd = cfg.Command
	}

	var args []string
	args = append(args, binCmd[1:]...)
	args = append(args,
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-prompt-tool", "stdio",
		"--permission-mode", "default",
	)
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	if cfg != nil {
		if cfg.AppendSystemPrompt != "" {
			args = append(args, "--append-system-prompt", cfg.AppendSystemPrompt)
		}
		if cfg.AllowedTools != "" {
			args = append(args, "--allowedTools", cfg.AllowedTools)
		}
		if cfg.MaxTurns > 0 {
			args = append(args, "--max-turns", fmt.Sprintf("%d", cfg.MaxTurns))
		}
		args = append(args, cfg.ExtraArgs...)
	}

	cmd := exec.Command(binCmd[0], args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	var permHandler PermissionHandler
	var onRawEvent func(protocol.RawStreamEvent)
	if cfg != nil {
		permHandler = cfg.PermissionHandler
		onRawEvent = cfg.OnRawEvent
	}

	p := &ClaudeProcess{
		cmd:         cmd,
		stdin:       stdin,
		pending:     make(map[string]chan ctlRespPayload),
		turnDone:    make(chan struct{}, 1),
		dead:        make(chan struct{}),
		sessionIDCh: make(chan string, 1),
		permHandler: permHandler,
		onEvent:     onEvent,
		onRawEvent:  onRawEvent,
	}

	logLabel := cwd
	if resume != "" {
		logLabel = resume
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("[claude stderr][%s] %s", logLabel, sc.Text())
		}
	}()

	go p.scan(stdout, logLabel)

	if err := p.sendControlRequest(ctlInitRequest{Subtype: "initialize"}); err != nil {
		p.Kill()
		return nil, fmt.Errorf("initialize failed: %w", err)
	}
	log.Printf("[claude process][%s] initialized", logLabel)

	return p, nil
}

// scan reads stdout line-by-line and routes events.
func (p *ClaudeProcess) scan(stdout io.Reader, logLabel string) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			log.Printf("[parse error][%s] %s: %s", logLabel, err, string(line[:min(len(line), 200)]))
			continue
		}

		switch envelope.Type {
		case "control_response":
			var resp ctlIncomingResponse
			if err := json.Unmarshal(line, &resp); err != nil {
				log.Printf("[parse error][%s] control_response: %s", logLabel, err)
				continue
			}
			p.handleControlResponse(resp.Response)

		case "control_request":
			var req ctlIncomingRequest
			if err := json.Unmarshal(line, &req); err != nil {
				log.Printf("[parse error][%s] control_request: %s", logLabel, err)
				continue
			}
			go p.handleControlRequest(req)

		case "stream_event":
			var raw protocol.RawStreamEvent
			if err := json.Unmarshal(line, &raw); err != nil {
				log.Printf("[parse error][%s] stream_event: %s", logLabel, err)
				continue
			}
			if raw.SessionID != "" {
				select {
				case p.sessionIDCh <- raw.SessionID:
				default:
				}
			}
			if p.onRawEvent != nil {
				p.onRawEvent(raw)
			}

		default:
			var event protocol.StreamEvent
			if err := json.Unmarshal(line, &event); err != nil {
				log.Printf("[parse error][%s] stream event: %s", logLabel, err)
				continue
			}
			if event.SessionID != "" {
				select {
				case p.sessionIDCh <- event.SessionID:
				default:
				}
			}
			if p.onEvent != nil {
				p.onEvent(event)
			}
			if envelope.Type == "result" {
				select {
				case p.turnDone <- struct{}{}:
				default:
				}
			}
		}
	}

	log.Printf("[claude exit][%s] scanner ended", logLabel)
	close(p.dead)
}

func (p *ClaudeProcess) handleControlResponse(resp ctlRespPayload) {
	p.mu.Lock()
	ch, ok := p.pending[resp.RequestID]
	delete(p.pending, resp.RequestID)
	p.mu.Unlock()

	if ok {
		select {
		case ch <- resp:
		case <-time.After(5 * time.Second):
			log.Printf("[claude process] response timeout for %s", resp.RequestID)
		}
	}
}

func (p *ClaudeProcess) handleControlRequest(req ctlIncomingRequest) {
	if req.Subtype != "can_use_tool" {
		return
	}

	var resp protocol.PermResponse
	if p.permHandler != nil {
		resp = p.permHandler(protocol.PermissionRequest{
			RequestID:      req.RequestID,
			ToolName:       req.ToolName,
			ToolUseID:      req.ToolUseID,
			Input:          req.Input,
			DecisionReason: req.DecisionReason,
			Suggestions:    req.PermissionSuggestions,
		})
	}
	// When no handler is set, resp is zero value: Allow=false → deny.
	p.sendPermResponse(req.RequestID, req.Input, resp)
}

func (p *ClaudeProcess) sendPermResponse(requestID string, originalInput *protocol.ToolInput, resp protocol.PermResponse) {
	if resp.Allow {
		input := originalInput
		if resp.UpdatedInput != nil {
			input = resp.UpdatedInput
		}
		payload := permAllowResponse{
			Behavior:           "allow",
			UpdatedInput:       input,
			UpdatedPermissions: resp.Permissions,
		}
		p.sendControlResponse(requestID, payload)
	} else {
		payload := permDenyResponse{
			Behavior: "deny",
			Message:  "User denied this action",
		}
		p.sendControlResponse(requestID, payload)
	}
}

func (p *ClaudeProcess) writeStdin(data []byte) (int, error) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.stdin.Write(append(data, '\n'))
}

func (p *ClaudeProcess) sendControlRequest(request any) error {
	p.mu.Lock()
	p.reqSeq++
	id := fmt.Sprintf("req_%d", p.reqSeq)
	ch := make(chan ctlRespPayload, 1)
	p.pending[id] = ch
	p.mu.Unlock()

	msg := ctlOutgoingRequest{
		Type:      "control_request",
		RequestID: id,
		Request:   request,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal control request: %w", err)
	}
	if _, err := p.writeStdin(data); err != nil {
		return fmt.Errorf("write control request: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Subtype == "error" {
			return fmt.Errorf("control request error: %s", resp.Error)
		}
		return nil
	case <-p.dead:
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return fmt.Errorf("process died")
	case <-time.After(30 * time.Second):
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return fmt.Errorf("control request timeout")
	}
}

func (p *ClaudeProcess) sendControlResponse(requestID string, response any) {
	msg := ctlOutgoingResponse{
		Type: "control_response",
		Response: ctlOutgoingRespBody{
			Subtype:   "success",
			RequestID: requestID,
			Response:  response,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[claude process] marshal control response: %s", err)
		return
	}
	if _, err := p.writeStdin(data); err != nil {
		log.Printf("[claude process] write control response: %s", err)
	}
}

// SendUserMessage sends a user message to start a turn.
func (p *ClaudeProcess) SendUserMessage(text string, images []protocol.ImageData) error {
	var content any
	if len(images) > 0 {
		var blocks []any
		for _, img := range images {
			blocks = append(blocks, userImageBlock{
				Type: "image",
				Source: userImageSource{
					Type:      "base64",
					MediaType: img.MediaType,
					Data:      img.Data,
				},
			})
		}
		if text != "" {
			blocks = append(blocks, userTextBlock{Type: "text", Text: text})
		}
		content = blocks
	} else {
		content = text
	}

	msg := userMessageEnvelope{
		Type:    "user",
		Message: userMessage{Role: "user", Content: content},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal user message: %w", err)
	}
	_, err = p.writeStdin(data)
	return err
}

// Interrupt sends an interrupt control request to abort the current turn.
func (p *ClaudeProcess) Interrupt() error {
	return p.sendControlRequest(ctlInterruptRequest{Subtype: "interrupt"})
}

// SetPermissionMode changes the permission mode mid-session.
func (p *ClaudeProcess) SetPermissionMode(mode string) error {
	return p.sendControlRequest(ctlSetPermModeRequest{Subtype: "set_permission_mode", Mode: mode})
}

// IsDead returns true if the process has exited.
func (p *ClaudeProcess) IsDead() bool {
	select {
	case <-p.dead:
		return true
	default:
		return false
	}
}

// WaitForSessionID blocks until the CLI reports a session ID or the context
// is cancelled. Returns ErrProcessDead if the process exits first.
func (p *ClaudeProcess) WaitForSessionID(ctx context.Context) (string, error) {
	select {
	case id := <-p.sessionIDCh:
		return id, nil
	case <-p.dead:
		return "", ErrProcessDead
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// WaitForTurnDone blocks until the current turn completes (a result event
// arrives) or the context is cancelled. Returns ErrProcessDead if the
// process exits first.
func (p *ClaudeProcess) WaitForTurnDone(ctx context.Context) error {
	select {
	case <-p.turnDone:
		return nil
	case <-p.dead:
		return ErrProcessDead
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DrainTurnDone discards any stale turnDone signal from a previous turn.
// Call before starting a new turn to prevent WaitForTurnDone from returning
// immediately on a result event that nobody was waiting for (e.g. a message
// injected while permission-blocked).
func (p *ClaudeProcess) DrainTurnDone() {
	select {
	case <-p.turnDone:
	default:
	}
}

// Kill terminates the process and waits for exit.
func (p *ClaudeProcess) Kill() {
	p.stdin.Close()
	p.cmd.Process.Kill()
	p.cmd.Wait()
	<-p.dead // wait for scanner to finish
}
