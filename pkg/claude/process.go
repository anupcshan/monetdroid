package claude

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
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

// PermissionMode identifies a claude permission mode.
type PermissionMode string

const (
	PermDefault     PermissionMode = "default"
	PermAcceptEdits PermissionMode = "acceptEdits"
)

// PermissionModeFromString converts a string to PermissionMode. Only modes
// monetdroid explicitly supports are recognized; unknown modes return false.
func PermissionModeFromString(s string) (PermissionMode, bool) {
	switch s {
	case "default":
		return PermDefault, true
	case "acceptEdits":
		return PermAcceptEdits, true
	default:
		return PermissionMode(""), false
	}
}

// Process is the interface for driving a claude backend.
type Process interface {
	// SendUserMessage delivers user text and optional images to claude.
	SendUserMessage(text string, images []protocol.ImageData) error

	// WaitForSessionID blocks until the session ID is known or the context
	// is cancelled. Returns ErrProcessDead if the process exits first.
	WaitForSessionID(ctx context.Context) (string, error)

	// WaitForTurnDone blocks until the current turn completes or the context
	// is cancelled. Returns ErrProcessDead if the process exits first.
	WaitForTurnDone(ctx context.Context) error

	// Interrupt requests that the current turn be aborted.
	Interrupt() error

	// SetPermissionMode changes the permission mode mid-session.
	SetPermissionMode(mode PermissionMode) error

	// IsDead reports whether the process has exited and will no longer
	// respond to any method.
	IsDead() bool

	// Kill terminates the process and releases its resources. Idempotent.
	Kill()
}

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

	// HookRegistry, when set, enables Claude Code hooks on this process.
	// StartProcess generates a routing token, registers a handler with the
	// registry, and configures the CLI with --settings pointing at the
	// registry's URL for that token. Hooks are passive observers; the
	// handler updates internal state and calls OnHookEvent.
	HookRegistry HookRegistry

	// OnHookEvent, when set, is called for each hook event received via the
	// HookRegistry. The receiver pre-extracts the event name; Body is the
	// raw JSON. Requires HookRegistry to be set.
	OnHookEvent func(HookEvent)
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
	onHookEvent func(HookEvent)

	hookRegistry HookRegistry
	hookToken    string
	settingsPath string
	curlShimPath string
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

	// Hook setup: write a curl shim that POSTs to the registry's URL for a
	// per-process token, plus a --settings file pointing every hook event at
	// that shim. Command-type hooks are used rather than HTTP because HTTP
	// hooks do not deliver SessionStart or Setup.
	var hookToken, settingsPath, curlShimPath string
	if cfg != nil && cfg.HookRegistry != nil {
		var tokenBytes [16]byte
		if _, err := rand.Read(tokenBytes[:]); err != nil {
			return nil, fmt.Errorf("generate hook token: %w", err)
		}
		hookToken = hex.EncodeToString(tokenBytes[:])
		url := cfg.HookRegistry.HookURL(hookToken)
		var err error
		settingsPath, curlShimPath, err = writeHookConfig(url, 600)
		if err != nil {
			return nil, fmt.Errorf("write hook config: %w", err)
		}
	}

	// Any failure path after writeHookConfig succeeds must remove the temp
	// files and unregister the handler. Kill() handles the same cleanup on
	// the live-process path; both are idempotent.
	success := false
	defer func() {
		if success {
			return
		}
		if cfg != nil && cfg.HookRegistry != nil && hookToken != "" {
			cfg.HookRegistry.UnregisterHookHandler(hookToken)
		}
		if settingsPath != "" {
			os.Remove(settingsPath)
		}
		if curlShimPath != "" {
			os.Remove(curlShimPath)
		}
	}()

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
	if settingsPath != "" {
		args = append(args, "--settings", settingsPath)
	}
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
	cmd.Env = append(os.Environ(),
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		// Tasks are on by default in interactive mode; we run Claude with -p
		// (non-interactive), so we need this opt-in to keep them.
		"CLAUDE_CODE_ENABLE_TASKS=1",
		// Strip the built-in git status snapshot and commit/PR workflow
		// instructions from the system prompt. The snapshot is captured once
		// at process start, so it goes stale the moment anything changes git
		// state outside Claude (branch switch, shell edits). Claude can run
		// `git` directly when it needs current state.
		"CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS=1",
	)

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

	var permHandler PermissionHandler
	var onRawEvent func(protocol.RawStreamEvent)
	var onHookEvent func(HookEvent)
	var hookRegistry HookRegistry
	if cfg != nil {
		permHandler = cfg.PermissionHandler
		onRawEvent = cfg.OnRawEvent
		onHookEvent = cfg.OnHookEvent
		hookRegistry = cfg.HookRegistry
	}

	p := &ClaudeProcess{
		cmd:          cmd,
		stdin:        stdin,
		pending:      make(map[string]chan ctlRespPayload),
		turnDone:     make(chan struct{}, 1),
		dead:         make(chan struct{}),
		sessionIDCh:  make(chan string, 1),
		permHandler:  permHandler,
		onEvent:      onEvent,
		onRawEvent:   onRawEvent,
		onHookEvent:  onHookEvent,
		hookRegistry: hookRegistry,
		hookToken:    hookToken,
		settingsPath: settingsPath,
		curlShimPath: curlShimPath,
	}

	// Register before cmd.Start so a hook POSTed during claude's startup
	// reaches a registered handler.
	if hookRegistry != nil && hookToken != "" {
		hookRegistry.RegisterHookHandler(hookToken, p.HandleHookEvent)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
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

	success = true
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
			// User and assistant messages flow from hook payloads (see
			// HandleHookEvent). The system branch still carries sub-agent
			// task lifecycle events (task_started, task_progress,
			// task_notification). The result branch carries cost data and
			// signals turnDone. Sub-agent inner events flow only via hooks.
			// Their stdout sidechain user/assistant events are dropped here.
			if envelope.Type != "system" && envelope.Type != "result" {
				continue
			}
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
func (p *ClaudeProcess) SetPermissionMode(mode PermissionMode) error {
	return p.sendControlRequest(ctlSetPermModeRequest{Subtype: "set_permission_mode", Mode: string(mode)})
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

// Kill terminates the process and waits for exit.
func (p *ClaudeProcess) Kill() {
	p.stdin.Close()
	p.cmd.Process.Kill()
	p.cmd.Wait()
	<-p.dead // wait for scanner to finish

	if p.hookRegistry != nil && p.hookToken != "" {
		p.hookRegistry.UnregisterHookHandler(p.hookToken)
	}
	if p.settingsPath != "" {
		os.Remove(p.settingsPath)
	}
	if p.curlShimPath != "" {
		os.Remove(p.curlShimPath)
	}
}

// HandleHookEvent converts the hook payload into protocol.StreamEvents and
// dispatches them through onEvent. See hookToStreamEvents for the per-event
// mapping. Hooks are passive observers; this method never returns a
// decision body to claude.
func (p *ClaudeProcess) HandleHookEvent(body []byte) error {
	var env struct {
		EventName string `json:"hook_event_name"`
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("parse hook envelope: %w", err)
	}

	if env.SessionID != "" {
		select {
		case p.sessionIDCh <- env.SessionID:
		default:
		}
	}

	// Inner sub-agent events (agent_id set) skip hookToStreamEvents: their
	// tool_use blocks must not leak into the parent's main stream. They are
	// routed only via OnHookEvent, where the monetdroid layer broadcasts a
	// subagent section keyed by agent_id and renames it at parent
	// PostToolUse for Agent (the only payload carrying both agent_id and
	// the parent's tool_use_id).
	if env.AgentID == "" && p.onEvent != nil {
		for _, ev := range hookToStreamEvents(env.EventName, body) {
			p.onEvent(ev)
		}
	}

	if p.onHookEvent != nil {
		p.onHookEvent(HookEvent{Name: env.EventName, Body: body})
	}
	return nil
}
