package monetdroid

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ClaudeProcess owns a long-running claude CLI subprocess.
// It persists across multiple turns in a session.
type ClaudeProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	sess  *Session

	mu      sync.Mutex
	reqSeq  int
	pending map[string]chan ctlRespPayload // request_id → response channel

	turnDone    chan struct{} // sent (not closed) when a result event arrives
	dead        chan struct{} // closed when process exits
	sessionIDCh chan string   // receives the ClaudeID from the first system event
	ready       chan struct{} // closed when session is bound and broadcasting can begin
}

// StartProcess starts a new claude CLI subprocess.
// sess may be nil for new sessions (before the ClaudeID is known).
// If resume is non-empty, passes --resume to restore conversation state.
func StartProcess(sess *Session, cwd string, buildCmd func(cwd string, args []string) *exec.Cmd, broadcast func(ServerMsg), resume string) (*ClaudeProcess, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--permission-mode", "default",
	}
	if resume != "" {
		args = append(args, "--resume", resume)
	}

	var cmd *exec.Cmd
	if buildCmd != nil {
		cmd = buildCmd(cwd, args)
	} else {
		cmd = exec.Command("claude", args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	}

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

	p := &ClaudeProcess{
		cmd:         cmd,
		stdin:       stdin,
		sess:        sess,
		pending:     make(map[string]chan ctlRespPayload),
		turnDone:    make(chan struct{}, 1),
		dead:        make(chan struct{}),
		sessionIDCh: make(chan string, 1),
		ready:       make(chan struct{}),
	}
	if sess != nil {
		close(p.ready) // session already bound, no need to wait
	}

	logLabel := cwd
	if resume != "" {
		logLabel = resume
	}

	// Drain stderr in background
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("[claude stderr][%s] %s", logLabel, sc.Text())
		}
	}()

	// Start stdout scanner
	go p.scan(stdout, broadcast, logLabel)

	// Send initialize and wait for response
	if err := p.sendControlRequest(ctlInitRequest{Subtype: "initialize"}); err != nil {
		p.Kill()
		return nil, fmt.Errorf("initialize failed: %w", err)
	}
	log.Printf("[claude process][%s] initialized", logLabel)

	return p, nil
}

// scan reads stdout line-by-line and routes events.
// Runs until the process exits (EOF).
func (p *ClaudeProcess) scan(stdout io.Reader, broadcast func(ServerMsg), logLabel string) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Peek at the type to route appropriately
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
			go p.handleControlRequest(req, broadcast)

		default:
			var event streamEvent
			if err := json.Unmarshal(line, &event); err != nil {
				log.Printf("[parse error][%s] stream event: %s", logLabel, err)
				continue
			}
			// Signal the ClaudeID from the first system/result event that carries it
			if event.SessionID != "" {
				select {
				case p.sessionIDCh <- event.SessionID:
				default:
				}
			}
			// Wait for session to be bound before broadcasting
			<-p.ready
			handleStreamEvent(p.sess, &event, broadcast)
			if envelope.Type == "result" {
				select {
				case p.turnDone <- struct{}{}:
				default:
				}
			}
		}
	}

	// Process exited
	log.Printf("[claude exit][%s] scanner ended", logLabel)
	close(p.dead)
}

// handleControlResponse routes responses to our control requests (initialize, interrupt, etc).
func (p *ClaudeProcess) handleControlResponse(resp ctlRespPayload) {
	p.mu.Lock()
	ch, ok := p.pending[resp.RequestID]
	delete(p.pending, resp.RequestID)
	p.mu.Unlock()

	if ok {
		select {
		case ch <- resp:
		case <-time.After(5 * time.Second):
			log.Printf("[claude process][%s] response timeout for %s", p.sess.ID, resp.RequestID)
		}
	}
}

// handleControlRequest handles permission prompts from the CLI.
// Runs in a separate goroutine so the scanner is never blocked.
func (p *ClaudeProcess) handleControlRequest(req ctlIncomingRequest, broadcast func(ServerMsg)) {
	if req.Subtype != "can_use_tool" {
		return
	}

	<-p.ready // wait for session to be bound
	ch := p.sess.RegisterPermChan(req.RequestID)

	broadcast(ServerMsg{
		Type: "permission_request", SessionID: p.sess.ID,
		PermID: req.RequestID, PermTool: req.ToolName, PermInput: req.Input,
		PermReason: req.DecisionReason, PermSuggestions: req.PermissionSuggestions,
	})

	resp := <-ch

	p.sess.DeletePermChan(req.RequestID)

	if resp.Allow {
		input := req.Input
		if resp.UpdatedInput != nil {
			input = resp.UpdatedInput
		}
		payload := permAllowResponse{
			Behavior:           "allow",
			UpdatedInput:       input,
			UpdatedPermissions: resp.Permissions,
		}
		p.sendControlResponse(req.RequestID, payload)
	} else {
		payload := permDenyResponse{
			Behavior: "deny",
			Message:  "User denied this action",
		}
		p.sendControlResponse(req.RequestID, payload)
	}
}

// sendControlRequest sends a control request and waits for the response.
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
	data, _ := json.Marshal(msg)
	p.stdin.Write(append(data, '\n'))

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

// sendControlResponse sends a control response (e.g. permission grant/deny).
func (p *ClaudeProcess) sendControlResponse(requestID string, response any) {
	msg := ctlOutgoingResponse{
		Type: "control_response",
		Response: ctlOutgoingRespBody{
			Subtype:   "success",
			RequestID: requestID,
			Response:  response,
		},
	}
	data, _ := json.Marshal(msg)
	p.stdin.Write(append(data, '\n'))
}

// SendUserMessage sends a user message to start a turn.
func (p *ClaudeProcess) SendUserMessage(text string, images []ImageData) error {
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
		Type:      "user",
		SessionID: "",
		Message:   userMessage{Role: "user", Content: content},
	}
	data, _ := json.Marshal(msg)
	_, err := p.stdin.Write(append(data, '\n'))
	return err
}

// Interrupt sends an interrupt control request to abort the current turn.
func (p *ClaudeProcess) Interrupt() error {
	return p.sendControlRequest(ctlInterruptRequest{Subtype: "interrupt"})
}

// BindSession sets the session on the process and unblocks broadcasting.
// Must be called exactly once for processes started without a session.
func (p *ClaudeProcess) BindSession(s *Session) {
	p.sess = s
	close(p.ready)
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

// Kill terminates the process and waits for exit.
func (p *ClaudeProcess) Kill() {
	p.stdin.Close()
	p.cmd.Process.Kill()
	p.cmd.Wait()
	<-p.dead // wait for scanner to finish
}
