package monetdroid

import "encoding/json"

// Control protocol types for communication with the Claude CLI subprocess.
// Based on the Claude Agent SDK's typed protocol definitions.

// --- Outgoing envelope (we send to CLI) ---

// ctlOutgoingRequest wraps a control request we send to the CLI.
type ctlOutgoingRequest struct {
	Type      string `json:"type"` // "control_request"
	RequestID string `json:"request_id"`
	Request   any    `json:"request"`
}

// ctlOutgoingResponse wraps a control response we send to the CLI.
type ctlOutgoingResponse struct {
	Type     string              `json:"type"` // "control_response"
	Response ctlOutgoingRespBody `json:"response"`
}

type ctlOutgoingRespBody struct {
	Subtype   string `json:"subtype"` // "success"
	RequestID string `json:"request_id"`
	Response  any    `json:"response"`
}

// --- Incoming envelope (CLI sends to us) ---

// ctlIncomingResponse is the response to our control requests.
type ctlIncomingResponse struct {
	Type     string         `json:"type"` // "control_response"
	Response ctlRespPayload `json:"response"`
}

type ctlRespPayload struct {
	Subtype   string `json:"subtype"` // "success" or "error"
	RequestID string `json:"request_id"`
	Error     string `json:"error,omitempty"`
}

// ctlIncomingRequest is a flat struct for all incoming control requests.
// Discriminate on Subtype; unused fields are zero for other subtypes.
type ctlIncomingRequest struct {
	Type      string `json:"type"`       // "control_request"
	RequestID string `json:"request_id"`
	// Nested inside "request" — use custom unmarshal
	Subtype               string `json:"-"`
	ToolName              string `json:"-"`
	Input                 any    `json:"-"`
	DecisionReason        string `json:"-"`
	PermissionSuggestions any    `json:"-"`
}

func (r *ctlIncomingRequest) UnmarshalJSON(data []byte) error {
	// First pass: envelope fields
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

	// Second pass: request body (flat struct with all possible fields)
	var req struct {
		Subtype               string `json:"subtype"`
		ToolName              string `json:"tool_name"`
		Input                 any    `json:"input"`
		DecisionReason        string `json:"decision_reason"`
		PermissionSuggestions any    `json:"permission_suggestions"`
	}
	if err := json.Unmarshal(envelope.Request, &req); err != nil {
		return err
	}
	r.Subtype = req.Subtype
	r.ToolName = req.ToolName
	r.Input = req.Input
	r.DecisionReason = req.DecisionReason
	r.PermissionSuggestions = req.PermissionSuggestions
	return nil
}

// --- Outgoing control requests (we send to CLI) ---

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

// --- Permission response (we send back) ---

type permAllowResponse struct {
	Behavior           string `json:"behavior"` // "allow"
	UpdatedInput       any    `json:"updatedInput"`
	UpdatedPermissions []any  `json:"updatedPermissions,omitempty"`
}

type permDenyResponse struct {
	Behavior string `json:"behavior"` // "deny"
	Message  string `json:"message"`
}

// --- Stream events (CLI stdout, non-control) ---

// streamEvent is the top-level envelope for all non-control events.
type streamEvent struct {
	Type       string                    `json:"type"` // "user", "assistant", "result", "system"
	SessionID  string                    `json:"session_id,omitempty"`
	Message    streamMessage             `json:"message"`
	Result     string                    `json:"result,omitempty"`
	TotalCost  float64                   `json:"total_cost_usd,omitempty"`
	ModelUsage map[string]modelUsageInfo `json:"modelUsage,omitempty"`
}

type streamMessage struct {
	Role    string               `json:"role,omitempty"`
	Content streamMessageContent `json:"content"`
	Model   string               `json:"model,omitempty"`
	Usage   *streamUsage         `json:"usage,omitempty"`
}

// streamMessageContent handles polymorphic content: string or array of blocks.
type streamMessageContent struct {
	Text   string        // set when content is a plain string
	Blocks []streamBlock // set when content is an array
}

func (c *streamMessageContent) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &c.Text) == nil {
		return nil
	}
	return json.Unmarshal(data, &c.Blocks)
}

type streamBlock struct {
	Type      string       `json:"type"`
	Text      string       `json:"text,omitempty"`
	Name      string       `json:"name,omitempty"`
	ID        string       `json:"id,omitempty"`
	Input     any          `json:"input,omitempty"`
	ToolUseID string       `json:"tool_use_id,omitempty"`
	Content   blockContent `json:"content,omitempty"`
}

type streamUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// --- User message (we send to CLI to start a turn) ---

type userMessageEnvelope struct {
	Type            string      `json:"type"`               // "user"
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

// --- Permission suggestion types ---

type permissionSuggestion struct {
	Type string `json:"type"` // "setMode", "addRules", etc.
	Mode string `json:"mode,omitempty"`
}

// --- AskUserQuestion tool input ---

type askUserQuestion struct {
	Question    string   `json:"question"`
	Options     []string `json:"options,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
}

// parseAskUserQuestions extracts the typed question metadata from an AskUserQuestion
// tool input. The raw input (any) is preserved for roundtripping back to the CLI.
func parseAskUserQuestions(rawInput any) []askUserQuestion {
	inputJSON, err := json.Marshal(rawInput)
	if err != nil {
		return nil
	}
	var parsed struct {
		Questions []askUserQuestion `json:"questions"`
	}
	json.Unmarshal(inputJSON, &parsed)
	return parsed.Questions
}

// buildAskUserResponse creates the updatedInput by copying the original input
// and adding the answers map, preserving all original fields.
func buildAskUserResponse(rawInput any, answers map[string]string) map[string]any {
	inputJSON, _ := json.Marshal(rawInput)
	var m map[string]any
	json.Unmarshal(inputJSON, &m)
	if m == nil {
		m = make(map[string]any)
	}
	m["answers"] = answers
	return m
}
