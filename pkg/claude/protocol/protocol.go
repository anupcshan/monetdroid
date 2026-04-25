package protocol

import (
	"encoding/json"
	"strings"
)

// RawStreamEvent wraps an inner Anthropic SSE event forwarded by the CLI
// when --include-partial-messages is enabled. Used for streaming text/thinking deltas.
type RawStreamEvent struct {
	Type            string  `json:"type"` // "stream_event"
	SessionID       string  `json:"session_id,omitempty"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Event           struct {
		Type         string `json:"type"` // "content_block_start", "content_block_delta", "content_block_stop", ...
		Index        int    `json:"index"`
		ContentBlock struct {
			Type string `json:"type"` // "text", "thinking", "tool_use"
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"` // "text_delta", "thinking_delta", "input_json_delta"
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	} `json:"event"`
}

// StreamEvent is the top-level envelope for all non-control events from the CLI.
type StreamEvent struct {
	Type            string                    `json:"type"` // "user", "assistant", "result", "system"
	Subtype         string                    `json:"subtype,omitempty"`
	SessionID       string                    `json:"session_id,omitempty"`
	ToolUseID       string                    `json:"tool_use_id,omitempty"`
	Status          string                    `json:"status,omitempty"`
	ParentToolUseID *string                   `json:"parent_tool_use_id"`
	Message         StreamMessage             `json:"message"`
	Result          string                    `json:"result,omitempty"`
	TotalCost       float64                   `json:"total_cost_usd,omitempty"`
	ModelUsage      map[string]ModelUsageInfo `json:"modelUsage,omitempty"`
	ToolUseResult   *ToolUseResult            `json:"tool_use_result,omitempty"`

	// Agent task fields
	TaskID       string     `json:"task_id,omitempty"`
	TaskType     string     `json:"task_type,omitempty"`
	Description  string     `json:"description,omitempty"`
	Summary      string     `json:"summary,omitempty"`
	LastToolName string     `json:"last_tool_name,omitempty"`
	TaskUsage    *TaskUsage `json:"usage,omitempty"`
}

type TaskUsage struct {
	TotalTokens int `json:"total_tokens"`
	ToolUses    int `json:"tool_uses"`
	DurationMs  int `json:"duration_ms"`
}

type ToolUseResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

func (r *ToolUseResult) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		r.Stdout = s
		return nil
	}
	type raw ToolUseResult
	return json.Unmarshal(data, (*raw)(r))
}

type StreamMessage struct {
	Role    string               `json:"role,omitempty"`
	Content StreamMessageContent `json:"content"`
	Model   string               `json:"model,omitempty"`
	Usage   *StreamUsage         `json:"usage,omitempty"`
}

type StreamMessageContent struct {
	Text   string
	Blocks []StreamBlock
}

func (c *StreamMessageContent) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &c.Text) == nil {
		return nil
	}
	return json.Unmarshal(data, &c.Blocks)
}

type StreamBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	RawInput  json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   BlockContent    `json:"content,omitempty"`
}

type StreamUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type ModelUsageInfo struct {
	ContextWindow int `json:"contextWindow"`
}

// BlockContent handles the polymorphic tool_result content: plain string or
// array of content blocks (which may include text and image blocks).
type BlockContent struct {
	Text   string      // set when content is a plain string or concatenated text blocks
	Images []ImageData // set when content array includes image blocks
	Raw    string      // JSON string fallback for non-string, non-array content
}

func (c *BlockContent) UnmarshalJSON(data []byte) error {
	if json.Unmarshal(data, &c.Text) == nil {
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
	if json.Unmarshal(data, &blocks) == nil && len(blocks) > 0 {
		var texts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
			case "image":
				if b.Source != nil && b.Source.MediaType != "" && b.Source.Data != "" {
					c.Images = append(c.Images, ImageData{
						MediaType: b.Source.MediaType,
						Data:      b.Source.Data,
					})
				}
			}
		}
		c.Text = strings.Join(texts, "\n")
		return nil
	}
	c.Raw = string(data)
	return nil
}

func (c *BlockContent) String() string {
	if c.Text != "" {
		return c.Text
	}
	return c.Raw
}

// ImageData holds a base64-encoded image for the Messages API content blocks.
type ImageData struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ToolInput wraps per-tool typed structs for rendering/routing and preserves
// the original raw JSON for lossless round-tripping back to the CLI.
type ToolInput struct {
	Raw json.RawMessage

	Bash     *BashInput
	Read     *ReadInput
	Write    *WriteInput
	Edit     *EditInput
	Grep     *GrepInput
	Glob     *GlobInput
	Todo     *TodoInput
	Ask      *AskInput
	Agent    *AgentInput
	PlanMode *PlanModeInput
}

func (t ToolInput) MarshalJSON() ([]byte, error) {
	if t.Raw != nil {
		return t.Raw, nil
	}
	return []byte("{}"), nil
}

// UnmarshalJSON stores the incoming bytes in Raw. The per-tool typed
// fields (Bash, Read, etc.) stay nil and must be filled by calling
// ParseToolInput with the corresponding tool name; JSON alone doesn't
// carry the tool name, so generic unmarshal can't pick the right struct.
func (t *ToolInput) UnmarshalJSON(data []byte) error {
	t.Raw = append(json.RawMessage(nil), data...)
	return nil
}

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
	case "ExitPlanMode":
		t.PlanMode = &PlanModeInput{}
		json.Unmarshal(raw, t.PlanMode)
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

type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
	Options     []AskOption `json:"options,omitempty"`
}

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

type PlanModeInput struct {
	Plan         string `json:"plan,omitempty"`
	PlanFilePath string `json:"planFilePath,omitempty"`
}

type Todo struct {
	Content    string `json:"content"`
	ActiveForm string `json:"activeForm"`
	Status     string `json:"status"`
}

type PermSuggestion struct {
	Type        string              `json:"type"`
	Mode        string              `json:"mode,omitempty"`
	Directories []string            `json:"directories,omitempty"`
	Destination string              `json:"destination,omitempty"`
	Behavior    string              `json:"behavior,omitempty"`
	Rules       []PermissionRuleVal `json:"rules,omitempty"`
}

type PermissionRuleVal struct {
	ToolName    string `json:"toolName"`
	RuleContent string `json:"ruleContent,omitempty"`
}

// PermissionRequest is the exported view of an incoming can_use_tool request.
type PermissionRequest struct {
	// RequestID is the control protocol request ID. The handler must not
	// interpret it — it exists so the web UI can key permission channels
	// and HTML elements by a stable, unique ID.
	RequestID      string
	ToolName       string
	ToolUseID      string
	Input          *ToolInput
	DecisionReason string
	Suggestions    []PermSuggestion
}

// PermResponse is the response to a permission request.
type PermResponse struct {
	Allow        bool
	Permissions  []PermSuggestion
	UpdatedInput *ToolInput
}
