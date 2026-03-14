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
	Type      string `json:"type"` // "control_request"
	RequestID string `json:"request_id"`
	// Nested inside "request" — use custom unmarshal
	Subtype               string           `json:"-"`
	ToolName              string           `json:"-"`
	Input                 *ToolInput       `json:"-"`
	DecisionReason        string           `json:"-"`
	PermissionSuggestions []PermSuggestion `json:"-"`
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
		Subtype               string           `json:"subtype"`
		ToolName              string           `json:"tool_name"`
		Input                 *ToolInput       `json:"input"`
		DecisionReason        string           `json:"decision_reason"`
		PermissionSuggestions []PermSuggestion `json:"permission_suggestions"`
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
	Behavior           string           `json:"behavior"` // "allow"
	UpdatedInput       *ToolInput       `json:"updatedInput"`
	UpdatedPermissions []PermSuggestion `json:"updatedPermissions,omitempty"`
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
	Input     *ToolInput   `json:"input,omitempty"`
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

// --- ToolInput: flat struct for all tool inputs ---

// ToolInput is a "fat" struct containing fields from all known tools.
// Discriminate by tool name; unused fields are zero for other tools.
type ToolInput struct {
	// Bash
	Command     string `json:"command,omitempty"`
	Description string `json:"description,omitempty"`
	Timeout     int    `json:"timeout,omitempty"`
	// Read/Write/Edit + Grep/Glob
	FilePath  string `json:"file_path,omitempty"`
	Content   string `json:"content,omitempty"`
	OldString string `json:"old_string,omitempty"`
	NewString string `json:"new_string,omitempty"`
	Offset    int    `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	Path      string `json:"path,omitempty"`
	Glob      string `json:"glob,omitempty"`
	// TodoWrite
	Todos []Todo `json:"todos,omitempty"`
	// AskUserQuestion
	Questions []AskQuestion     `json:"questions,omitempty"`
	Answers   map[string]string `json:"answers,omitempty"`
}

// ResolvedPath returns FilePath if set, else Path.
func (t *ToolInput) ResolvedPath() string {
	if t.FilePath != "" {
		return t.FilePath
	}
	return t.Path
}

// AskQuestion represents a single question in AskUserQuestion.
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
	Options     []AskOption `json:"options,omitempty"`
}

// AskOption represents a selectable option within a question.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// --- Permission suggestion types ---

type PermSuggestion struct {
	Type        string   `json:"type"` // "setMode", "addRules", "addDirectories", etc.
	Mode        string   `json:"mode,omitempty"`
	Directories []string `json:"directories,omitempty"`
}

// buildAskUserResponse creates the updatedInput by copying the original input
// and adding the answers map, preserving all original fields.
func buildAskUserResponse(input *ToolInput, answers map[string]string) *ToolInput {
	out := *input
	out.Answers = answers
	return &out
}
