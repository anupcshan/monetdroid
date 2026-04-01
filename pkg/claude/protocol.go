package claude

import (
	"encoding/json"

	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// Control protocol types for communication with the Claude CLI subprocess.
// These are internal to the claude package — consumers interact through
// ClaudeProcess methods and protocol.StreamEvent.

// --- Outgoing envelopes (we send to CLI) ---

type ctlOutgoingRequest struct {
	Type      string `json:"type"` // "control_request"
	RequestID string `json:"request_id"`
	Request   any    `json:"request"`
}

type ctlOutgoingResponse struct {
	Type     string              `json:"type"` // "control_response"
	Response ctlOutgoingRespBody `json:"response"`
}

type ctlOutgoingRespBody struct {
	Subtype   string `json:"subtype"` // "success"
	RequestID string `json:"request_id"`
	Response  any    `json:"response"`
}

// --- Incoming envelopes (CLI sends to us) ---

type ctlIncomingResponse struct {
	Type     string         `json:"type"` // "control_response"
	Response ctlRespPayload `json:"response"`
}

type ctlRespPayload struct {
	Subtype   string `json:"subtype"` // "success" or "error"
	RequestID string `json:"request_id"`
	Error     string `json:"error,omitempty"`
}

type ctlIncomingRequest struct {
	Type      string `json:"type"` // "control_request"
	RequestID string `json:"request_id"`
	// Nested inside "request" — use custom unmarshal
	Subtype               string                    `json:"-"`
	ToolName              string                    `json:"-"`
	ToolUseID             string                    `json:"-"`
	Input                 *protocol.ToolInput       `json:"-"`
	DecisionReason        string                    `json:"-"`
	PermissionSuggestions []protocol.PermSuggestion `json:"-"`
	BlockedPath           string                    `json:"-"`
}

func (r *ctlIncomingRequest) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	r.Type = envelope.Type
	r.RequestID = envelope.RequestID

	var req struct {
		Subtype               string                    `json:"subtype"`
		ToolName              string                    `json:"tool_name"`
		ToolUseID             string                    `json:"tool_use_id"`
		RawInput              json.RawMessage           `json:"input"`
		DecisionReason        string                    `json:"decision_reason"`
		PermissionSuggestions []protocol.PermSuggestion `json:"permission_suggestions"`
		BlockedPath           string                    `json:"blocked_path"`
	}
	if err := json.Unmarshal(envelope.Request, &req); err != nil {
		return err
	}
	r.Subtype = req.Subtype
	r.ToolName = req.ToolName
	r.ToolUseID = req.ToolUseID
	r.Input = protocol.ParseToolInput(req.ToolName, req.RawInput)
	r.DecisionReason = req.DecisionReason
	r.PermissionSuggestions = req.PermissionSuggestions
	r.BlockedPath = req.BlockedPath
	return nil
}

// --- Outgoing control requests ---

type ctlInitRequest struct {
	Subtype string `json:"subtype"` // "initialize"
}

type ctlInterruptRequest struct {
	Subtype string `json:"subtype"` // "interrupt"
}

type ctlSetPermModeRequest struct {
	Subtype string `json:"subtype"` // "set_permission_mode"
	Mode    string `json:"mode"`
}

// --- Permission response payloads ---

type permAllowResponse struct {
	Behavior           string                    `json:"behavior"` // "allow"
	UpdatedInput       *protocol.ToolInput       `json:"updatedInput"`
	UpdatedPermissions []protocol.PermSuggestion `json:"updatedPermissions,omitempty"`
}

type permDenyResponse struct {
	Behavior string `json:"behavior"` // "deny"
	Message  string `json:"message"`
}

// --- User message types ---

type userMessageEnvelope struct {
	Type            string      `json:"type"` // "user"
	SessionID       string      `json:"session_id"`
	Message         userMessage `json:"message"`
	ParentToolUseID *string     `json:"parent_tool_use_id"`
}

type userMessage struct {
	Role    string `json:"role"`    // "user"
	Content any    `json:"content"` // string or []userContentBlock
}

type userImageBlock struct {
	Type   string          `json:"type"` // "image"
	Source userImageSource `json:"source"`
}

type userImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type userTextBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}
