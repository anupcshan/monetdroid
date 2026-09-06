package claude

import (
	"encoding/json"

	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// Control protocol types for communication with the Claude CLI subprocess.
// These are internal to the claude package. Consumers interact through
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
	Subtype   string          `json:"subtype"` // "success" or "error"
	RequestID string          `json:"request_id"`
	Error     string          `json:"error,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
}

type ctlIncomingRequest struct {
	Type      string `json:"type"` // "control_request"
	RequestID string `json:"request_id"`
	// Nested inside "request", extracted via custom unmarshal
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

type ctlRewindRequest struct {
	Subtype           string `json:"subtype"` // "rewind_conversation"
	TargetMessageUUID string `json:"target_message_uuid"`
}

type ctlCancelAsyncRequest struct {
	Subtype     string `json:"subtype"` // "cancel_async_message"
	MessageUUID string `json:"message_uuid"`
}

// ctlCancelAsyncResponse is the body of a cancel_async_message
// control_response. Cancelled reports whether claude removed the message
// from its queue. It is false when the message was already dequeued for
// execution or was never enqueued. A removed message emits a terminal
// cancelled command_lifecycle frame.
type ctlCancelAsyncResponse struct {
	Cancelled bool `json:"cancelled"`
}

// ctlInterruptResponse is the body of an interrupt control_response.
// StillQueued lists the uuids of user messages that were pending when the
// interrupt arrived. That covers messages still in the command queue and
// any batch already dequeued for the next turn. Only messages sent with a
// uuid appear. The list can carry uuids the client never sent, such as
// scheduled task triggers.
type ctlInterruptResponse struct {
	StillQueued []string `json:"still_queued"`
}

// ctlRewindResponse is the body of a rewind_conversation control_response.
// The outer subtype is "success" even when Rewound is false, so Rewound is the
// real success signal.
type ctlRewindResponse struct {
	Rewound                bool   `json:"rewound"`
	TargetMessageUUID      string `json:"targetMessageUuid"`
	PrefillText            string `json:"prefillText"`
	PrecedingAssistantUUID string `json:"precedingAssistantUuid"`
	Error                  string `json:"error,omitempty"`
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
	UUID            string      `json:"uuid,omitempty"` // caller-minted, adopted by claude as the message's uuid.
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
