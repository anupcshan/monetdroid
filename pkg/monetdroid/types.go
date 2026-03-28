package monetdroid

import "time"

// ImageData holds a base64-encoded image for the Messages API content blocks.
type ImageData struct {
	MediaType string `json:"media_type"` // e.g. "image/jpeg"
	Data      string `json:"data"`       // base64-encoded
}

// ServerMsg is the internal message type stored in session logs.
type ServerMsg struct {
	Type            string           `json:"type"`
	SessionID       string           `json:"session_id,omitempty"`
	Text            string           `json:"text,omitempty"`
	Images          []ImageData      `json:"images,omitempty"`
	Tool            string           `json:"tool,omitempty"`
	ToolUseID       string           `json:"tool_use_id,omitempty"`
	Input           *ToolInput       `json:"input,omitempty"`
	Output          string           `json:"output,omitempty"`
	Error           string           `json:"error,omitempty"`
	Cost            *CostInfo        `json:"cost,omitempty"`
	ParentToolUseID string           `json:"parent_tool_use_id,omitempty"`
	AgentStat       *AgentStat       `json:"agent_stat,omitempty"`
	PermID          string           `json:"perm_id,omitempty"`
	PermTool        string           `json:"perm_tool,omitempty"`
	PermInput       *ToolInput       `json:"perm_input,omitempty"`
	PermReason      string           `json:"perm_reason,omitempty"`
	PermSuggestions []PermSuggestion `json:"perm_suggestions,omitempty"`
	PermMode        string           `json:"perm_mode,omitempty"`
}

// AgentStat tracks live stats for a sub-agent invocation.
type AgentStat struct {
	Description  string `json:"description"`
	TotalTokens  int    `json:"total_tokens"`
	ToolUses     int    `json:"tool_uses"`
	DurationMs   int    `json:"duration_ms"`
	LastToolName string `json:"last_tool_name,omitempty"`
	Completed    bool   `json:"completed"`
}

type CostInfo struct {
	TotalCostUSD  float64 `json:"total_cost_usd,omitempty"`
	ContextUsed   int     `json:"context_used,omitempty"`
	ContextWindow int     `json:"context_window,omitempty"`
}

type HistoryGroup struct {
	Dir      string           `json:"dir"`
	DirKey   string           `json:"dir_key"`
	Sessions []HistorySession `json:"sessions"`
}

type HistorySession struct {
	ID            string    `json:"id"`
	Summary       string    `json:"summary"`
	Branches      []string  `json:"branches,omitempty"`
	ModTime       time.Time `json:"mod_time"`
	NumMsgs       int       `json:"num_msgs"`
	ContextUsed   int       `json:"context_used"`
	ContextWindow int       `json:"context_window"`
}

type HistoryMessage struct {
	Type      string      `json:"type"`
	Text      string      `json:"text,omitempty"`
	Images    []ImageData `json:"images,omitempty"`
	Tool      string      `json:"tool,omitempty"`
	ToolUseID string      `json:"tool_use_id,omitempty"`
	Input     *ToolInput  `json:"input,omitempty"`
	Output    string      `json:"output,omitempty"`
}

type SessionUsage struct {
	TotalCostUSD  float64
	ContextUsed   int
	ContextWindow int
}

type Todo struct {
	Content    string `json:"content"`
	ActiveForm string `json:"activeForm"`
	Status     string `json:"status"` // pending, in_progress, completed
}

type PermResponse struct {
	Allow        bool
	Permissions  []PermSuggestion
	UpdatedInput *ToolInput // non-nil for AskUserQuestion answers
}

// PermissionRequest is the exported view of an incoming can_use_tool request.
// It exposes only the fields needed for policy decisions, keeping the wire
// protocol types private.
type PermissionRequest struct {
	ToolName       string
	ToolUseID      string
	Input          *ToolInput
	DecisionReason string
}

// PermissionHandler handles permission requests directly, bypassing the
// broadcast-and-wait-on-channel flow. Called synchronously in the
// handleControlRequest goroutine. Return a PermResponse indicating
// allow/deny.
type PermissionHandler func(req PermissionRequest) PermResponse

// ProcessConfig holds optional configuration for StartProcessWithConfig.
// All fields are optional; zero values preserve the default behavior.
type ProcessConfig struct {
	// PermissionHandler, when set, handles can_use_tool requests directly
	// instead of broadcasting and waiting on the session's permission channel.
	PermissionHandler PermissionHandler

	// AppendSystemPrompt is passed as --append-system-prompt to the CLI.
	AppendSystemPrompt string

	// AllowedTools is passed as --allowedTools to the CLI (glob pattern).
	AllowedTools string

	// MaxTurns is passed as --max-turns to the CLI.
	MaxTurns int
}
