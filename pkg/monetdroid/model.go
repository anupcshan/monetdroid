package monetdroid

import (
	"strconv"
	"sync"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// SessionModel holds all renderable state derived from the session event log.
// It is produced by folding the event log through ApplyOne. The zero value
// is a valid empty model: BuildModel(base, log) calls ApplyOne for each event.
type SessionModel struct {
	mu sync.Mutex

	Messages          []ServerMsg
	Running           bool
	Todos             []protocol.Todo
	Cost              CostInfo
	DiffStat          DiffStat
	BgTasks           map[string]*BgTaskState
	ToolUseIndexes    map[string]int              // tool_use id -> log index
	ToolResultIndexes map[string]int              // tool_use id -> log index
	ToolResults       map[string]ServerMsg        // tool_use id -> tool_result message
	SuppressedIDs     map[string]bool             // tool_use ids for suppressed tools
	PendingPerms      map[string]ServerMsg        // unresolved inline permission_request
	SubagentSections  map[string]*SubagentSection // agent_id -> section state
	LastCompact       int                         // index of last compact_boundary, -1 if none
	QueuedText        string                      // next user message queued for sending

	// Session metadata (set from base state, not from events).
	Cwd       string
	Label     string
	AutoLabel bool
	PermMode  claude.PermissionMode

	// pendingCommands stashes Bash commands from tool_use events so they
	// can be attached to BgTaskState when the tool_result arrives.
	pendingCommands map[string]string
}

// ModelBase holds session-level state that is not derived from the event log.
type ModelBase struct {
	Cwd       string
	Label     string
	AutoLabel bool
	PermMode  claude.PermissionMode
	Cost      CostInfo // initial cost from session (includes ModelName set by history load)
}

// BuildModel folds a base state and an event log into a SessionModel.
func BuildModel(base ModelBase, log []ServerMsg) *SessionModel {
	m := &SessionModel{
		Cwd:               base.Cwd,
		Label:             base.Label,
		AutoLabel:         base.AutoLabel,
		PermMode:          base.PermMode,
		Cost:              base.Cost,
		BgTasks:           make(map[string]*BgTaskState),
		ToolUseIndexes:    make(map[string]int),
		ToolResultIndexes: make(map[string]int),
		ToolResults:       make(map[string]ServerMsg),
		SuppressedIDs:     make(map[string]bool),
		PendingPerms:      make(map[string]ServerMsg),
		SubagentSections:  make(map[string]*SubagentSection),
		LastCompact:       -1,
		pendingCommands:   make(map[string]string),
	}
	for _, msg := range log {
		m.Apply(msg)
	}
	return m
}

// Apply updates the model for a single event. It is called both by BuildModel
// (for page load) and by the live event path (for incremental updates).
func (m *SessionModel) Apply(msg ServerMsg) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Messages = append(m.Messages, msg)

	switch msg.Type {
	case "compact_boundary":
		m.LastCompact = len(m.Messages) - 1

	case "tool_use":
		if msg.ToolUseID == "" {
			break
		}
		if msg.AgentID == "" {
			m.ToolUseIndexes[msg.ToolUseID] = len(m.Messages) - 1
		}
		if suppressResultTools[msg.Tool] {
			m.SuppressedIDs[msg.ToolUseID] = true
		}
		if msg.Tool == "Bash" && msg.AgentID == "" && msg.Input != nil && msg.Input.Bash != nil {
			m.pendingCommands[msg.ToolUseID] = msg.Input.Bash.Command
		}
		switch msg.Tool {
		case "TodoWrite":
			if t := ParseTodos(msg.Input); t != nil {
				m.Todos = t
			}
		case "TaskCreate":
			if msg.Input != nil && msg.Input.TaskCreate != nil {
				m.Todos = append(m.Todos, protocol.Todo{
					ID:         strconv.Itoa(len(m.Todos) + 1),
					Content:    msg.Input.TaskCreate.Subject,
					ActiveForm: msg.Input.TaskCreate.ActiveForm,
					Status:     "pending",
				})
			}
		case "TaskUpdate":
			if msg.Input != nil && msg.Input.TaskUpdate != nil {
				for i, t := range m.Todos {
					if t.ID == msg.Input.TaskUpdate.TaskID {
						switch msg.Input.TaskUpdate.Status {
						case "deleted":
							m.Todos = append(m.Todos[:i], m.Todos[i+1:]...)
						case "":
						default:
							m.Todos[i].Status = msg.Input.TaskUpdate.Status
						}
						break
					}
				}
			}
		}

	case "tool_result":
		if msg.ToolUseID == "" || msg.AgentID != "" {
			break
		}
		m.ToolResults[msg.ToolUseID] = msg
		m.ToolResultIndexes[msg.ToolUseID] = len(m.Messages) - 1
		delete(m.PendingPerms, msg.ToolUseID)
		if bgPath := ParseBgTaskPath(msg.Output); bgPath != "" {
			m.BgTasks[msg.ToolUseID] = &BgTaskState{
				Command:    m.pendingCommands[msg.ToolUseID],
				OutputPath: bgPath,
			}
		}

	case "task_done":
		if msg.ToolUseID == "" {
			break
		}
		if st, ok := m.BgTasks[msg.ToolUseID]; ok {
			st.Completed = true
		}

	case "running":
		m.Running = true

	case "done":
		m.Running = false

	case "cost":
		if msg.Cost != nil {
			if msg.Cost.ContextUsed > 0 {
				m.Cost.ContextUsed = msg.Cost.ContextUsed
			}
			if msg.Cost.ContextWindow > 0 {
				m.Cost.ContextWindow = msg.Cost.ContextWindow
			}
			if msg.Cost.TotalCostUSD > 0 {
				m.Cost.TotalCostUSD = msg.Cost.TotalCostUSD
			}
			if msg.Cost.ModelName != "" {
				m.Cost.ModelName = msg.Cost.ModelName
			}
		}

	case "permission_request":
		if msg.PermTool != "AskUserQuestion" && msg.ToolUseID != "" {
			m.PendingPerms[msg.ToolUseID] = msg
		}

	case "permission_mode":
		// handled elsewhere (session runtime)

	case "subagent_started":
		if msg.AgentID != "" {
			m.SubagentSections[msg.AgentID] = &SubagentSection{
				AgentID:   msg.AgentID,
				AgentType: msg.AgentType,
			}
		}

	case "subagent_stopped":
		if msg.AgentID != "" {
			if s, ok := m.SubagentSections[msg.AgentID]; ok {
				s.Stopped = true
			}
		}

	case "subagent_linked":
		if msg.AgentID != "" {
			if s, ok := m.SubagentSections[msg.AgentID]; ok {
				s.Linked = true
				s.ParentToolUseID = msg.ParentToolUseID
				s.Description = msg.Description
				s.FinalText = msg.Text
				s.TotalTokens = msg.TotalTokens
				s.TotalToolUses = msg.TotalToolUses
				s.DurationMs = msg.DurationMs
			}
		}
	}
}
