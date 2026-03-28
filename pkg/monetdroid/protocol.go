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
	ToolUseID             string           `json:"-"`
	Input                 *ToolInput       `json:"-"`
	DecisionReason        string           `json:"-"`
	PermissionSuggestions []PermSuggestion `json:"-"`
	BlockedPath           string           `json:"-"`
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
		ToolUseID             string           `json:"tool_use_id"`
		RawInput              json.RawMessage  `json:"input"`
		DecisionReason        string           `json:"decision_reason"`
		PermissionSuggestions []PermSuggestion `json:"permission_suggestions"`
		BlockedPath           string           `json:"blocked_path"`
	}
	if err := json.Unmarshal(envelope.Request, &req); err != nil {
		return err
	}
	r.Subtype = req.Subtype
	r.ToolName = req.ToolName
	r.ToolUseID = req.ToolUseID
	r.Input = ParseToolInput(req.ToolName, req.RawInput)
	r.DecisionReason = req.DecisionReason
	r.PermissionSuggestions = req.PermissionSuggestions
	r.BlockedPath = req.BlockedPath
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
	Type            string                    `json:"type"` // "user", "assistant", "result", "system"
	Subtype         string                    `json:"subtype,omitempty"`
	SessionID       string                    `json:"session_id,omitempty"`
	ToolUseID       string                    `json:"tool_use_id,omitempty"`
	Status          string                    `json:"status,omitempty"`
	ParentToolUseID *string                   `json:"parent_tool_use_id"`
	Message         streamMessage             `json:"message"`
	Result          string                    `json:"result,omitempty"`
	TotalCost       float64                   `json:"total_cost_usd,omitempty"`
	ModelUsage      map[string]modelUsageInfo `json:"modelUsage,omitempty"`
	ToolUseResult   *toolUseResult            `json:"tool_use_result,omitempty"`

	// Agent task fields (system events with subtype task_started/task_progress/task_notification)
	TaskID       string     `json:"task_id,omitempty"`
	TaskType     string     `json:"task_type,omitempty"`
	Description  string     `json:"description,omitempty"`
	Summary      string     `json:"summary,omitempty"`
	LastToolName string     `json:"last_tool_name,omitempty"`
	TaskUsage    *taskUsage `json:"usage,omitempty"`
}

// taskUsage carries agent task usage stats from task_progress/task_notification events.
type taskUsage struct {
	TotalTokens int `json:"total_tokens"`
	ToolUses    int `json:"tool_uses"`
	DurationMs  int `json:"duration_ms"`
}

// toolUseResult carries structured tool output from the CLI.
// Can be either a plain string or an object with stdout/stderr fields.
type toolUseResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

func (r *toolUseResult) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		r.Stdout = s
		return nil
	}
	type raw toolUseResult
	return json.Unmarshal(data, (*raw)(r))
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
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	RawInput  json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   blockContent    `json:"content,omitempty"`
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

// --- ToolInput: typed variants per tool + raw JSON for round-tripping ---

// ToolInput wraps per-tool typed structs for rendering/routing and preserves
// the original raw JSON for lossless round-tripping back to the CLI.
// Exactly one typed variant is set for known tools; Raw is always set.
type ToolInput struct {
	// Raw is the original JSON. Used for MarshalJSON (round-trip) and as
	// display fallback for unknown tools. Always set by ParseToolInput.
	Raw json.RawMessage

	// Typed variants — at most one is non-nil.
	Bash  *BashInput
	Read  *ReadInput
	Write *WriteInput
	Edit  *EditInput
	Grep  *GrepInput
	Glob  *GlobInput
	Todo  *TodoInput
	Ask   *AskInput
	Agent *AgentInput
}

// MarshalJSON returns the preserved raw JSON so unknown fields survive round-trips.
func (t ToolInput) MarshalJSON() ([]byte, error) {
	if t.Raw != nil {
		return t.Raw, nil
	}
	return []byte("{}"), nil
}

// ParseToolInput creates a ToolInput from raw JSON, populating the appropriate
// typed variant based on tool name. Raw is always preserved.
func ParseToolInput(tool string, raw json.RawMessage) *ToolInput {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	t := &ToolInput{Raw: append(json.RawMessage(nil), raw...)}
	switch tool {
	case "Bash":
		t.Bash = &BashInput{}
		json.Unmarshal(raw, t.Bash)
	case "Read", "FileRead":
		t.Read = &ReadInput{}
		json.Unmarshal(raw, t.Read)
	case "Write", "FileWrite":
		t.Write = &WriteInput{}
		json.Unmarshal(raw, t.Write)
	case "Edit", "FileEdit":
		t.Edit = &EditInput{}
		json.Unmarshal(raw, t.Edit)
	case "Grep":
		t.Grep = &GrepInput{}
		json.Unmarshal(raw, t.Grep)
	case "Glob":
		t.Glob = &GlobInput{}
		json.Unmarshal(raw, t.Glob)
	case "TodoWrite":
		t.Todo = &TodoInput{}
		json.Unmarshal(raw, t.Todo)
	case "AskUserQuestion":
		t.Ask = &AskInput{}
		json.Unmarshal(raw, t.Ask)
	case "Agent":
		t.Agent = &AgentInput{}
		json.Unmarshal(raw, t.Agent)
	}
	return t
}

type BashInput struct {
	Command         string `json:"command,omitempty"`
	Description     string `json:"description,omitempty"`
	Timeout         int    `json:"timeout,omitempty"`
	RunInBackground *bool  `json:"run_in_background,omitempty"`
}

type ReadInput struct {
	FilePath string `json:"file_path,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type WriteInput struct {
	FilePath string `json:"file_path,omitempty"`
	Content  string `json:"content,omitempty"`
}

type EditInput struct {
	FilePath   string `json:"file_path,omitempty"`
	OldString  string `json:"old_string,omitempty"`
	NewString  string `json:"new_string,omitempty"`
	ReplaceAll *bool  `json:"replace_all,omitempty"`
}

type GrepInput struct {
	Pattern string `json:"pattern,omitempty"`
	Path    string `json:"path,omitempty"`
	Glob    string `json:"glob,omitempty"`
}

type GlobInput struct {
	Pattern string `json:"pattern,omitempty"`
	Path    string `json:"path,omitempty"`
}

type TodoInput struct {
	Todos []Todo `json:"todos,omitempty"`
}

type AskInput struct {
	Questions []AskQuestion     `json:"questions,omitempty"`
	Answers   map[string]string `json:"answers,omitempty"`
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

type AgentInput struct {
	Description  string `json:"description,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	SubagentType string `json:"subagent_type,omitempty"`
	Model        string `json:"model,omitempty"`
}

// --- Permission suggestion types ---

type PermSuggestion struct {
	Type        string              `json:"type"` // "setMode", "addRules", "replaceRules", "removeRules", "addDirectories", "removeDirectories"
	Mode        string              `json:"mode,omitempty"`
	Directories []string            `json:"directories,omitempty"`
	Destination string              `json:"destination,omitempty"` // "userSettings", "projectSettings", "localSettings", "session"
	Behavior    string              `json:"behavior,omitempty"`    // "allow", "deny", "ask"
	Rules       []PermissionRuleVal `json:"rules,omitempty"`
}

type PermissionRuleVal struct {
	ToolName    string `json:"toolName"`
	RuleContent string `json:"ruleContent,omitempty"`
}

// buildAskUserResponse creates the updatedInput by merging answers into the
// raw JSON, preserving all original fields for the CLI round-trip.
func buildAskUserResponse(input *ToolInput, answers map[string]string) *ToolInput {
	// Merge "answers" into Raw so MarshalJSON includes it
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input.Raw, &m); err != nil {
		m = make(map[string]json.RawMessage)
	}
	answersJSON, _ := json.Marshal(answers)
	m["answers"] = json.RawMessage(answersJSON)
	merged, _ := json.Marshal(m)

	ask := &AskInput{Answers: answers}
	if input.Ask != nil {
		ask.Questions = input.Ask.Questions
	}
	return &ToolInput{Raw: merged, Ask: ask}
}
