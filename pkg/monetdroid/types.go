package monetdroid

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
	PermID          string           `json:"perm_id,omitempty"`
	PermTool        string           `json:"perm_tool,omitempty"`
	PermInput       *ToolInput       `json:"perm_input,omitempty"`
	PermReason      string           `json:"perm_reason,omitempty"`
	PermSuggestions []PermSuggestion `json:"perm_suggestions,omitempty"`
	PermMode        string           `json:"perm_mode,omitempty"`
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
	ID      string `json:"id"`
	Summary string `json:"summary"`
	ModTime string `json:"mod_time"`
	ModUnix int64  `json:"mod_unix"`
	NumMsgs int    `json:"num_msgs"`
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
