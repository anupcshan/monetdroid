package monetdroid

import (
	"strconv"
	"sync"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// Viewer receives rendered HTML from a Model.
type Viewer interface {
	Send(html string)
	Done() <-chan struct{}
}

// SessionModel holds all renderable state derived from the session event log.
// It is produced by folding the event log through Apply. The zero value
// is a valid empty model: BuildModel(base, log) calls Apply for each event.
//
// State is mutated only by the internal goroutine launched in BuildModel.
// External callers send events via HandleEvent and attach/detach via
// Attach/Detach; all of these are channel-based and never touch state
// directly.
type SessionModel struct {
	mu sync.Mutex

	Messages          []ServerMsg
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

	// Activity tracking: derived from event observations. turnActive is set by
	// "running"/"done" ServerMsgs (which map to UserPromptSubmit/Stop hook
	// observations). processAlive is set true on SessionStart and false when
	// the process dies.
	turnActive   bool
	processAlive bool

	// Event channel and viewer management.
	sessionID string
	events    chan serverMsgEvent
	viewers   map[string]Viewer
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// serverMsgEvent wraps a ServerMsg with its rendering context.
type serverMsgEvent struct {
	msg          ServerMsg
	todosChanged bool
	permUpgrades func([]DOMCmd) []DOMCmd // extra DOM commands to append after rendering
	push         func(string)            // callback to push rendered HTML to transport
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
// The returned model has its internal goroutine running.
func BuildModel(base ModelBase, log []ServerMsg, sessionID string) *SessionModel {
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
		sessionID:         sessionID,
		events:            make(chan serverMsgEvent, 256),
		viewers:           make(map[string]Viewer),
		stopCh:            make(chan struct{}),
		processAlive:      true, // live sessions always have a live process
	}
	for _, msg := range log {
		m.Apply(msg)
	}
	m.wg.Add(1)
	go m.run()
	return m
}

// Close stops the internal goroutine and drains the event channel.
func (m *SessionModel) Close() {
	close(m.stopCh)
	m.wg.Wait()
}

// HandleEvent sends an event to the model's internal goroutine for processing.
// push is called with rendered HTML after the event is applied.
func (m *SessionModel) HandleEvent(msg ServerMsg, push func(string)) {
	m.sendEvent(serverMsgEvent{msg: msg, push: push})
}

// HandleEventWithTodos sends an event plus a flag indicating whether todos changed.
func (m *SessionModel) HandleEventWithTodos(msg ServerMsg, todosChanged bool, push func(string)) {
	m.sendEvent(serverMsgEvent{msg: msg, todosChanged: todosChanged, push: push})
}

// HandleEventWithUpgrades sends an event plus a callback that appends extra
// DOM commands to the rendered output (used for permission detail upgrades).
func (m *SessionModel) HandleEventWithUpgrades(msg ServerMsg, todosChanged bool, permUpgrades func([]DOMCmd) []DOMCmd, push func(string)) {
	m.sendEvent(serverMsgEvent{msg: msg, todosChanged: todosChanged, permUpgrades: permUpgrades, push: push})
}

func (m *SessionModel) sendEvent(ev serverMsgEvent) {
	select {
	case m.events <- ev:
	default:
		// Channel full; drop event. The model will be rebuilt on next
		// page load, so this is safe (no state corruption, just a
		// transient rendering gap).
	}
}

// run is the model's internal goroutine. It is the only thing that mutates
// state or pushes HTML. All external input arrives via the events channel.
func (m *SessionModel) run() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case ev, ok := <-m.events:
			if !ok {
				return
			}
			m.processEvent(ev)
		}
	}
}

// processEvent applies a single event and pushes rendered HTML to all viewers.
func (m *SessionModel) processEvent(ev serverMsgEvent) {
	wasActive := m.HasActivity()
	wasStoppable := m.CanInterrupt()
	m.Apply(ev.msg)
	isActive := m.HasActivity()
	isStoppable := m.CanInterrupt()

	cmds := RenderEvent(m, ev.msg, m.sessionID)
	if ev.todosChanged {
		cmds = append(cmds,
			DOMCmd{Target: "todos-summary", Strategy: "innerHTML", Content: RenderTodosSummary(m.Todos)},
			DOMCmd{Target: "todos-body", Strategy: "innerHTML", Content: RenderTodosBody(m.Todos)},
		)
	}
	if ev.permUpgrades != nil {
		cmds = ev.permUpgrades(cmds)
	}

	// Push activity state transition if changed.
	if isActive != wasActive || isStoppable != wasStoppable {
		cmds = append(cmds, activeCmds(isActive, isStoppable)...)
	}

	if len(cmds) > 0 && ev.push != nil {
		event := FormatSSEDOM(cmds)
		if event != "" {
			ev.push(event)
		}
	}
}

// HasActivity reports whether the session is doing work. Activity is derived
// from event observations. Bg tasks and sub-agents contribute alongside
// turn activity and pending permissions.
func (m *SessionModel) HasActivity() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.processAlive {
		return false
	}
	if m.turnActive {
		return true
	}
	if len(m.PendingPerms) > 0 {
		return true
	}
	for _, bt := range m.BgTasks {
		if !bt.Completed {
			return true
		}
	}
	for _, s := range m.SubagentSections {
		if !s.Stopped {
			return true
		}
	}
	return false
}

// CanInterrupt reports whether the session has an in-progress turn or
// pending permission that can be interrupted.
func (m *SessionModel) CanInterrupt() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.processAlive && (m.turnActive || len(m.PendingPerms) > 0)
}

// Attach adds a viewer and sends a full snapshot from fromOffset.
func (m *SessionModel) Attach(v Viewer, fromOffset int) {
	m.mu.Lock()
	m.viewers["TODO"] = v                     // placeholder, actual ID management TBD
	snapCmds := RenderFull(m, m.sessionID, 0) // TODO: reviewCount
	if len(snapCmds) > 0 {
		event := FormatSSEDOM(snapCmds)
		if event != "" {
			v.Send(event)
		}
	}
	m.mu.Unlock()
}

// Detach removes a viewer.
func (m *SessionModel) Detach(viewerID string) {
	m.mu.Lock()
	delete(m.viewers, viewerID)
	m.mu.Unlock()
}

// Apply updates the model for a single event. It is called both by BuildModel
// (for page load) and by the internal goroutine (for live events).
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
		m.turnActive = true

	case "done":
		m.turnActive = false

	case "session_started":
		m.processAlive = true
		m.turnActive = false
		m.PendingPerms = make(map[string]ServerMsg)
		m.BgTasks = make(map[string]*BgTaskState)
		m.SubagentSections = make(map[string]*SubagentSection)

	case "session_ended":
		m.processAlive = false

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
