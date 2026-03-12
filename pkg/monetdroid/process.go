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
	pending map[string]chan map[string]any // request_id → response channel

	turnDone chan struct{} // sent (not closed) when a result event arrives
	dead     chan struct{} // closed when process exits
}

// StartProcess starts a new claude CLI subprocess for the given session.
// If sess.ClaudeID is set, passes --resume to restore conversation state.
func StartProcess(sess *Session, buildCmd func(cwd string, args []string) *exec.Cmd, broadcast func(ServerMsg)) (*ClaudeProcess, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--permission-mode", "default",
	}
	if sess.ClaudeID != "" {
		args = append(args, "--resume", sess.ClaudeID)
	}

	var cmd *exec.Cmd
	if buildCmd != nil {
		cmd = buildCmd(sess.Cwd, args)
	} else {
		cmd = exec.Command("claude", args...)
		cmd.Dir = sess.Cwd
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
		cmd:      cmd,
		stdin:    stdin,
		sess:     sess,
		pending:  make(map[string]chan map[string]any),
		turnDone: make(chan struct{}, 1),
		dead:     make(chan struct{}),
	}

	// Drain stderr in background
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("[claude stderr][%s] %s", sess.ID, sc.Text())
		}
	}()

	// Start stdout scanner
	go p.scan(stdout, broadcast)

	// Send initialize and wait for response
	initResp, err := p.SendControlRequest(map[string]any{"subtype": "initialize"})
	if err != nil {
		p.Kill()
		return nil, fmt.Errorf("initialize failed: %w", err)
	}
	log.Printf("[claude process][%s] initialized: %+v", sess.ID, initResp)

	return p, nil
}

// scan reads stdout line-by-line and routes events.
// Runs until the process exits (EOF).
func (p *ClaudeProcess) scan(stdout io.Reader, broadcast func(ServerMsg)) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			log.Printf("[parse error][%s] %s: %s", p.sess.ID, err, line[:min(len(line), 200)])
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "control_response":
			p.handleControlResponse(event)
		case "control_request":
			// Permission request from CLI — handle in goroutine to avoid blocking scanner
			go p.handleControlRequest(event, broadcast)
		case "result":
			handleStreamEvent(p.sess, event, broadcast)
			select {
			case p.turnDone <- struct{}{}:
			default: // don't block if channel already has a signal
			}
		default:
			handleStreamEvent(p.sess, event, broadcast)
		}
	}

	// Process exited
	log.Printf("[claude exit][%s] scanner ended", p.sess.ID)
	close(p.dead)
}

// handleControlResponse routes responses to our control requests (initialize, interrupt, etc).
func (p *ClaudeProcess) handleControlResponse(event map[string]any) {
	response, _ := event["response"].(map[string]any)
	requestID, _ := response["request_id"].(string)

	p.mu.Lock()
	ch, ok := p.pending[requestID]
	delete(p.pending, requestID)
	p.mu.Unlock()

	if ok {
		select {
		case ch <- response:
		case <-time.After(5 * time.Second):
			log.Printf("[claude process][%s] response timeout for %s", p.sess.ID, requestID)
		}
	}
}

// handleControlRequest handles permission prompts from the CLI.
// Runs in a separate goroutine so the scanner is never blocked.
func (p *ClaudeProcess) handleControlRequest(event map[string]any, broadcast func(ServerMsg)) {
	requestID, _ := event["request_id"].(string)
	request, _ := event["request"].(map[string]any)
	subtype, _ := request["subtype"].(string)

	if subtype != "can_use_tool" {
		return
	}

	toolName, _ := request["tool_name"].(string)
	toolInput := request["input"]
	reason, _ := request["decision_reason"].(string)
	suggestions := request["permission_suggestions"]

	ch := make(chan PermResponse, 1)
	p.sess.Mu.Lock()
	p.sess.PermChans[requestID] = ch
	p.sess.Mu.Unlock()

	broadcast(ServerMsg{
		Type: "permission_request", SessionID: p.sess.ID,
		PermID: requestID, PermTool: toolName, PermInput: toolInput,
		PermReason: reason, PermSuggestions: suggestions,
	})

	resp := <-ch

	p.sess.Mu.Lock()
	delete(p.sess.PermChans, requestID)
	p.sess.Mu.Unlock()

	var payload map[string]any
	if resp.Allow {
		payload = map[string]any{"behavior": "allow", "updatedInput": toolInput}
		if len(resp.Permissions) > 0 {
			payload["updatedPermissions"] = resp.Permissions
		}
	} else {
		payload = map[string]any{"behavior": "deny", "message": "User denied this action"}
	}

	p.SendControlResponse(requestID, payload)
}

// SendControlRequest sends a control request and waits for the response.
func (p *ClaudeProcess) SendControlRequest(request map[string]any) (map[string]any, error) {
	p.mu.Lock()
	p.reqSeq++
	id := fmt.Sprintf("req_%d", p.reqSeq)
	ch := make(chan map[string]any, 1)
	p.pending[id] = ch
	p.mu.Unlock()

	msg := map[string]any{
		"type":       "control_request",
		"request_id": id,
		"request":    request,
	}
	data, _ := json.Marshal(msg)
	p.stdin.Write(append(data, '\n'))

	select {
	case resp := <-ch:
		if resp["subtype"] == "error" {
			return nil, fmt.Errorf("control request error: %v", resp["error"])
		}
		response, _ := resp["response"].(map[string]any)
		return response, nil
	case <-p.dead:
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("process died")
	case <-time.After(30 * time.Second):
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("control request timeout")
	}
}

// SendControlResponse sends a control response (e.g. permission grant/deny).
func (p *ClaudeProcess) SendControlResponse(requestID string, response map[string]any) {
	msg := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   response,
		},
	}
	data, _ := json.Marshal(msg)
	p.stdin.Write(append(data, '\n'))
}

// SendUserMessage sends a user message to start a turn.
func (p *ClaudeProcess) SendUserMessage(text string, images []ImageData) error {
	var content any
	if len(images) > 0 {
		var blocks []map[string]any
		for _, img := range images {
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": img.MediaType,
					"data":       img.Data,
				},
			})
		}
		if text != "" {
			blocks = append(blocks, map[string]any{
				"type": "text",
				"text": text,
			})
		}
		content = blocks
	} else {
		content = text
	}

	msg := map[string]any{
		"type":               "user",
		"session_id":         "",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
	}
	data, _ := json.Marshal(msg)
	_, err := p.stdin.Write(append(data, '\n'))
	return err
}

// Interrupt sends an interrupt control request to abort the current turn.
func (p *ClaudeProcess) Interrupt() error {
	_, err := p.SendControlRequest(map[string]any{"subtype": "interrupt"})
	return err
}

// SetPermissionMode changes the permission mode mid-session.
func (p *ClaudeProcess) SetPermissionMode(mode string) error {
	_, err := p.SendControlRequest(map[string]any{"subtype": "set_permission_mode", "mode": mode})
	return err
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
