package claude

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// Hook payload types. Each struct matches exactly one hook event's
// documented schema. No fallback parsing.

type userPromptSubmitPayload struct {
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
}

type preToolUsePayload struct {
	SessionID string          `json:"session_id"`
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

type postToolBatchPayload struct {
	SessionID string              `json:"session_id"`
	ToolCalls []postToolBatchTool `json:"tool_calls"`
}

type postToolBatchTool struct {
	ToolUseID    string            `json:"tool_use_id"`
	ToolResponse toolResultContent `json:"tool_response"`
}

// stopPayload covers both Stop and StopFailure: both events carry
// last_assistant_message with the turn's final user-visible text (for
// StopFailure this is the API error text).
type stopPayload struct {
	SessionID    string `json:"session_id"`
	FinalMessage string `json:"last_assistant_message"`
}

// toolResultContent is PostToolBatch's tool_response: a serialized string
// or a content-block array of {type:"text",text} and
// {type:"image",source:{media_type,data}} entries. See PostToolBatch in
// https://docs.claude.com/en/docs/claude-code/hooks. Anything else is a
// contract violation and returns an error.
type toolResultContent struct {
	Text   string
	Images []protocol.ImageData
}

func (c *toolResultContent) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &c.Text); err == nil {
		return nil
	}
	var blocks []struct {
		Type   string `json:"type"`
		Text   string `json:"text,omitempty"`
		Source *struct {
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source,omitempty"`
	}
	if err := json.Unmarshal(data, &blocks); err != nil {
		return fmt.Errorf("tool_response: expected string or content-block array, got %s", string(data))
	}
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "image":
			if b.Source != nil && b.Source.MediaType != "" && b.Source.Data != "" {
				c.Images = append(c.Images, protocol.ImageData{
					MediaType: b.Source.MediaType,
					Data:      b.Source.Data,
				})
			}
		}
	}
	c.Text = strings.Join(texts, "\n")
	return nil
}

// HookHandlerFunc handles an incoming hook event POST from claude. The body
// is the raw event JSON. A non-nil error becomes HTTP 500 to the sender.
type HookHandlerFunc func(body []byte) error

// HookRegistry is the contract a hook receiver fulfills. ClaudeProcess uses
// it to register a per-process handler at startup and to resolve the URL
// claude should be configured to POST events to.
type HookRegistry interface {
	RegisterHookHandler(token string, handler HookHandlerFunc)
	UnregisterHookHandler(token string)
	HookURL(token string) string
}

// HookEvent is the envelope passed to ProcessConfig.OnHookEvent. Name is
// extracted from hook_event_name; Body is the raw JSON for the consumer to
// decode into a more specific shape.
type HookEvent struct {
	Name string
	Body []byte
}

// hookToStreamEvents converts a Claude Code hook payload into one or more
// protocol.StreamEvent values:
//
//	UserPromptSubmit  -> "user" event with the prompt as text content
//	PreToolUse        -> "assistant" event with a single tool_use block
//	PostToolBatch     -> one "user" event per tool_call, each with a
//	                     tool_result block carrying the stringified response
//	Stop / StopFailure -> "assistant" event with a single text block
//	                     (last_assistant_message; for StopFailure this is
//	                     the API error text)
//
// Returns nil for hook events that have no downstream consumer here.
func hookToStreamEvents(eventName string, body []byte) []protocol.StreamEvent {
	switch eventName {
	case "UserPromptSubmit":
		// Emits a user-text event for the prompt claude has accepted.
		// monetdroid already broadcasts a user_message ServerMsg from
		// /send when the user submits text (it has the bytes locally); the
		// hook is the confirmation that those bytes made it through stdin
		// and were accepted by claude. handleStreamEvent's "user" case
		// today only renders tool_result blocks, so a text-only user event
		// is currently a no-op downstream. Kept emitted so a future
		// consumer can show the progression from "we think we sent it"
		// to "claude received it" without another protocol change here.
		var p userPromptSubmitPayload
		if err := json.Unmarshal(body, &p); err != nil {
			log.Printf("[hook] UserPromptSubmit parse: %v", err)
			return nil
		}
		if p.Prompt == "" {
			return nil
		}
		return []protocol.StreamEvent{{
			Type:      "user",
			SessionID: p.SessionID,
			Message: protocol.StreamMessage{
				Role:    "user",
				Content: protocol.StreamMessageContent{Text: p.Prompt},
			},
		}}

	case "PreToolUse":
		var p preToolUsePayload
		if err := json.Unmarshal(body, &p); err != nil {
			log.Printf("[hook] PreToolUse parse: %v", err)
			return nil
		}
		// The Agent tool is rendered as a sub-agent section in the main
		// timeline (see monetdroid handleHookEvent), not as a standalone
		// parent Agent tool_use chip. Emitting a tool_use StreamEvent for
		// Agent here would create an empty parent chip alongside the
		// sub-agent section.
		if p.ToolName == "Agent" {
			return nil
		}
		return []protocol.StreamEvent{{
			Type:      "assistant",
			SessionID: p.SessionID,
			Message: protocol.StreamMessage{
				Role: "assistant",
				Content: protocol.StreamMessageContent{
					Blocks: []protocol.StreamBlock{{
						Type:     "tool_use",
						Name:     p.ToolName,
						ID:       p.ToolUseID,
						RawInput: p.ToolInput,
					}},
				},
			},
		}}

	case "PostToolBatch":
		// PostToolBatch fires exactly once per batch (including single-tool
		// batches) and carries the stringified tool_response the model sees,
		// unlike PostToolUse which carries each tool's structured output.
		var p postToolBatchPayload
		if err := json.Unmarshal(body, &p); err != nil {
			log.Printf("[hook] PostToolBatch parse: %v", err)
			return nil
		}
		events := make([]protocol.StreamEvent, 0, len(p.ToolCalls))
		for _, tc := range p.ToolCalls {
			events = append(events, protocol.StreamEvent{
				Type:      "user",
				SessionID: p.SessionID,
				Message: protocol.StreamMessage{
					Role: "user",
					Content: protocol.StreamMessageContent{
						Blocks: []protocol.StreamBlock{{
							Type:      "tool_result",
							ToolUseID: tc.ToolUseID,
							Content: protocol.BlockContent{
								Text:   tc.ToolResponse.Text,
								Images: tc.ToolResponse.Images,
							},
						}},
					},
				},
			})
		}
		return events

	case "Stop", "StopFailure":
		var p stopPayload
		if err := json.Unmarshal(body, &p); err != nil {
			log.Printf("[hook] %s parse: %v", eventName, err)
			return nil
		}
		if p.FinalMessage == "" {
			return nil
		}
		return []protocol.StreamEvent{{
			Type:      "assistant",
			SessionID: p.SessionID,
			Message: protocol.StreamMessage{
				Role: "assistant",
				Content: protocol.StreamMessageContent{
					Blocks: []protocol.StreamBlock{{
						Type: "text",
						Text: p.FinalMessage,
					}},
				},
			},
		}}
	}
	return nil
}

// hookEvents lists every known Claude Code hook event.
var hookEvents = []string{
	"SessionStart", "Setup", "SessionEnd",
	"UserPromptSubmit", "UserPromptExpansion", "Stop", "StopFailure",
	"PreToolUse", "PostToolUse", "PostToolUseFailure", "PostToolBatch",
	"PermissionRequest", "PermissionDenied",
	"SubagentStart", "SubagentStop",
	"TaskCreated", "TaskCompleted", "TeammateIdle",
	"FileChanged", "CwdChanged", "ConfigChange", "InstructionsLoaded",
	"PreCompact", "PostCompact",
	"Elicitation", "ElicitationResult", "Notification",
	"WorktreeCreate", "WorktreeRemove",
}

// writeHookConfig emits a per-process curl shim script and a claude settings
// JSON file wiring every known hook event to that script. The shim forwards
// stdin to hookURL via curl. Returns (settingsPath, shimPath); the caller
// removes both on cleanup.
//
// Command-type hooks are used rather than HTTP because HTTP hooks do not
// deliver SessionStart or Setup.
func writeHookConfig(hookURL string, timeoutSeconds int) (settingsPath, shimPath string, err error) {
	shimPath, err = writeCurlShim(hookURL)
	if err != nil {
		return
	}
	defer func() {
		if err == nil {
			return
		}
		if settingsPath != "" {
			os.Remove(settingsPath)
			settingsPath = ""
		}
		os.Remove(shimPath)
		shimPath = ""
	}()

	type hookHandler struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	type matcherGroup struct {
		Hooks []hookHandler `json:"hooks"`
	}
	type config struct {
		Hooks map[string][]matcherGroup `json:"hooks"`
	}

	handler := hookHandler{Type: "command", Command: shimPath, Timeout: timeoutSeconds}
	s := config{Hooks: map[string][]matcherGroup{}}
	for _, ev := range hookEvents {
		s.Hooks[ev] = []matcherGroup{{Hooks: []hookHandler{handler}}}
	}

	var data []byte
	data, err = json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}

	var f *os.File
	f, err = os.CreateTemp("", "monetdroid-claude-settings-*.json")
	if err != nil {
		return
	}
	settingsPath = f.Name()
	if _, err = f.Write(data); err != nil {
		f.Close()
		return
	}
	err = f.Close()
	return
}

// writeCurlShim writes an executable bash script that POSTs stdin to url via
// curl. Returns the script path.
func writeCurlShim(url string) (string, error) {
	script := fmt.Sprintf("#!/bin/bash\nexec curl -s -X POST --data-binary @- %q\n", url)
	f, err := os.CreateTemp("", "monetdroid-claude-hook-*.sh")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
