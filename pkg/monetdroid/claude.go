package monetdroid

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"
)

// ClaudeCommand is the path to the claude binary. Override in tests.
var ClaudeCommand = "claude"

// PermissionTimeout is how long to wait for a permission response before auto-denying.
var PermissionTimeout = 5 * time.Minute

func RunClaudeTurn(s *Session, prompt string, broadcast func(ServerMsg)) {
	s.Mu.Lock()
	s.Running = true
	s.MessageCount++
	claudeID := s.ClaudeID
	cwd := s.Cwd
	s.PermChans = make(map[string]chan PermResponse)
	s.Mu.Unlock()

	defer func() {
		s.Mu.Lock()
		s.Running = false
		// Close any pending permission channels so waiters don't hang
		for id, ch := range s.PermChans {
			close(ch)
			delete(s.PermChans, id)
		}
		s.Mu.Unlock()
	}()

	args := []string{"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
		"--permission-mode", "default",
	}
	if claudeID != "" {
		args = append(args, "--resume", claudeID)
	}

	cmd := exec.Command(ClaudeCommand, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("stdin pipe: %v", err)})
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("stdout pipe: %v", err)})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("stderr pipe: %v", err)})
		return
	}

	if err := cmd.Start(); err != nil {
		broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: fmt.Sprintf("start: %v", err)})
		return
	}

	writeJSON := func(v any) {
		data, _ := json.Marshal(v)
		stdin.Write(append(data, '\n'))
	}

	s.Mu.Lock()
	s.WriteJSON = writeJSON
	s.Mu.Unlock()
	defer func() {
		s.Mu.Lock()
		s.WriteJSON = nil
		s.Mu.Unlock()
	}()

	writeJSON(map[string]any{
		"type": "control_request", "request_id": "init_1",
		"request": map[string]any{"subtype": "initialize"},
	})
	writeJSON(map[string]any{
		"type": "user", "session_id": "",
		"message":            map[string]any{"role": "user", "content": prompt},
		"parent_tool_use_id": nil,
	})

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("[claude stderr][%s] %s", s.ID, sc.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			log.Printf("[parse error][%s] %s: %s", s.ID, err, line[:min(len(line), 200)])
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "control_request":
			handleControlRequest(s, event, writeJSON, broadcast)
		case "control_response":
			// ignore
		case "result":
			handleStreamEvent(s, event, broadcast)
			stdin.Close()
		default:
			handleStreamEvent(s, event, broadcast)
		}
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("[claude exit][%s] %v", s.ID, err)
	}
	broadcast(ServerMsg{Type: "done", SessionID: s.ID})
}

func handleControlRequest(s *Session, event map[string]any, writeJSON func(any), broadcast func(ServerMsg)) {
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
	s.Mu.Lock()
	s.PermChans[requestID] = ch
	s.Mu.Unlock()

	broadcast(ServerMsg{
		Type: "permission_request", SessionID: s.ID,
		PermID: requestID, PermTool: toolName, PermInput: toolInput,
		PermReason: reason, PermSuggestions: suggestions,
	})

	var resp PermResponse
	select {
	case resp = <-ch:
	case <-time.After(PermissionTimeout):
		resp = PermResponse{Allow: false}
	}

	s.Mu.Lock()
	delete(s.PermChans, requestID)
	s.Mu.Unlock()

	if resp.Allow {
		payload := map[string]any{"behavior": "allow", "updatedInput": toolInput}
		if len(resp.Permissions) > 0 {
			payload["updatedPermissions"] = resp.Permissions
		}
		writeJSON(map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype": "success", "request_id": requestID, "response": payload,
			},
		})
	} else {
		writeJSON(map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype": "success", "request_id": requestID,
				"response": map[string]any{"behavior": "deny", "message": "User denied this action"},
			},
		})
	}
}

func handleStreamEvent(s *Session, event map[string]any, broadcast func(ServerMsg)) {
	eventType, _ := event["type"].(string)

	switch eventType {
	case "system":
		if sid, ok := event["session_id"].(string); ok && sid != "" {
			s.Mu.Lock()
			if s.ClaudeID == "" {
				s.ClaudeID = sid
			}
			s.Mu.Unlock()
		}

	case "assistant":
		msg, ok := event["message"].(map[string]any)
		if !ok {
			return
		}
		content, ok := msg["content"].([]any)
		if !ok {
			return
		}
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := b["type"].(string)
			switch blockType {
			case "text":
				text, _ := b["text"].(string)
				if text != "" {
					broadcast(ServerMsg{Type: "text", SessionID: s.ID, Text: text})
				}
			case "tool_use":
				name, _ := b["name"].(string)
				input := b["input"]
				broadcast(ServerMsg{Type: "tool_use", SessionID: s.ID, Tool: name, Input: input})
			}
		}
		if usage, ok := msg["usage"].(map[string]any); ok {
			inTok, _ := usage["input_tokens"].(float64)
			outTok, _ := usage["output_tokens"].(float64)
			cacheRead, _ := usage["cache_read_input_tokens"].(float64)
			cacheCreate, _ := usage["cache_creation_input_tokens"].(float64)
			contextUsed := int(inTok + cacheRead + cacheCreate + outTok)
			if inTok > 0 || outTok > 0 {
				broadcast(ServerMsg{
					Type: "cost", SessionID: s.ID,
					Cost: &CostInfo{InputTokens: int(inTok), OutputTokens: int(outTok), ContextUsed: contextUsed},
				})
			}
		}

	case "result":
		if text, ok := event["result"].(string); ok && text != "" {
			broadcast(ServerMsg{Type: "result", SessionID: s.ID, Text: text})
		}
		if sid, ok := event["session_id"].(string); ok && sid != "" {
			s.Mu.Lock()
			s.ClaudeID = sid
			s.Mu.Unlock()
		}
		if mu, ok := event["modelUsage"].(map[string]any); ok {
			for _, v := range mu {
				if info, ok := v.(map[string]any); ok {
					if cw, ok := info["contextWindow"].(float64); ok && cw > 0 {
						broadcast(ServerMsg{Type: "cost", SessionID: s.ID, Cost: &CostInfo{ContextWindow: int(cw)}})
					}
					break
				}
			}
		}

	case "user":
		msg, ok := event["message"].(map[string]any)
		if !ok {
			return
		}
		content, ok := msg["content"].([]any)
		if !ok {
			return
		}
		for _, block := range content {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if b["type"] == "tool_result" {
				output := ""
				switch c := b["content"].(type) {
				case string:
					output = c
				default:
					j, _ := json.Marshal(c)
					output = string(j)
				}
				broadcast(ServerMsg{Type: "tool_result", SessionID: s.ID, Output: Truncate(output, 2000)})
			}
		}
	}
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}
