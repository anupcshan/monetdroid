package monetdroid

// ServerMsg is the internal message type stored in session logs.
type ServerMsg struct {
	Type            string      `json:"type"`
	SessionID       string      `json:"session_id,omitempty"`
	Text            string      `json:"text,omitempty"`
	Tool            string      `json:"tool,omitempty"`
	Input           interface{} `json:"input,omitempty"`
	Output          string      `json:"output,omitempty"`
	Error           string      `json:"error,omitempty"`
	Cost            *CostInfo   `json:"cost,omitempty"`
	PermID          string      `json:"perm_id,omitempty"`
	PermTool        string      `json:"perm_tool,omitempty"`
	PermInput       interface{} `json:"perm_input,omitempty"`
	PermReason      string      `json:"perm_reason,omitempty"`
	PermSuggestions interface{} `json:"perm_suggestions,omitempty"`
	PermMode        string      `json:"perm_mode,omitempty"`
}

type CostInfo struct {
	InputTokens   int `json:"input_tokens"`
	OutputTokens  int `json:"output_tokens"`
	ContextUsed   int `json:"context_used,omitempty"`
	ContextWindow int `json:"context_window,omitempty"`
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
}

type HistoryMessage struct {
	Type   string      `json:"type"`
	Text   string      `json:"text,omitempty"`
	Tool   string      `json:"tool,omitempty"`
	Input  interface{} `json:"input,omitempty"`
	Output string      `json:"output,omitempty"`
}

type SessionUsage struct {
	ContextUsed   int
	ContextWindow int
}

type PermResponse struct {
	Allow       bool
	Permissions []interface{}
}
