package monetdroid

import (
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// ServerMsg is the internal message type stored in session logs.
type ServerMsg struct {
	Type            string                    `json:"type"`
	SessionID       string                    `json:"session_id,omitempty"`
	Text            string                    `json:"text,omitempty"`
	Images          []protocol.ImageData      `json:"images,omitempty"`
	Tool            string                    `json:"tool,omitempty"`
	ToolUseID       string                    `json:"tool_use_id,omitempty"`
	Cwd             string                    `json:"cwd,omitempty"`
	Input           *protocol.ToolInput       `json:"input,omitempty"`
	Output          string                    `json:"output,omitempty"`
	Error           string                    `json:"error,omitempty"`
	Cost            *CostInfo                 `json:"cost,omitempty"`
	ParentToolUseID string                    `json:"parent_tool_use_id,omitempty"`
	AgentID         string                    `json:"agent_id,omitempty"`
	AgentType       string                    `json:"agent_type,omitempty"`
	Description     string                    `json:"description,omitempty"`
	TotalTokens     int                       `json:"total_tokens,omitempty"`
	TotalToolUses   int                       `json:"total_tool_uses,omitempty"`
	DurationMs      int                       `json:"duration_ms,omitempty"`
	AgentStat       *AgentStat                `json:"agent_stat,omitempty"`
	PermID          string                    `json:"perm_id,omitempty"`
	PermTool        string                    `json:"perm_tool,omitempty"`
	PermInput       *protocol.ToolInput       `json:"perm_input,omitempty"`
	PermReason      string                    `json:"perm_reason,omitempty"`
	PermSuggestions []protocol.PermSuggestion `json:"perm_suggestions,omitempty"`
	PermMode        claude.PermissionMode     `json:"perm_mode,omitempty"`
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
	ModelName     string  `json:"model_name,omitempty"`
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
	Type      string               `json:"type"`
	Text      string               `json:"text,omitempty"`
	Images    []protocol.ImageData `json:"images,omitempty"`
	Tool      string               `json:"tool,omitempty"`
	ToolUseID string               `json:"tool_use_id,omitempty"`
	Input     *protocol.ToolInput  `json:"input,omitempty"`
	Output    string               `json:"output,omitempty"`
}

type SessionUsage struct {
	TotalCostUSD  float64
	ContextUsed   int
	ContextWindow int
	ModelName     string
}
