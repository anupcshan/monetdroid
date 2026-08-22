package monetdroid

import (
	"log"
	"maps"
	"strconv"
	"sync"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// SessionModel holds all renderable state derived from the session event log.
// It is produced by folding the event log through Apply. The zero value
// is a valid empty model. BuildModel(base, log) calls Apply for each event.
//
// State is mutated only by the internal goroutine launched in BuildModel.
// External callers send events via HandleEvent, which is channel-based and
// never touches state directly.
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
	SubagentSections  map[string]*SubagentSection // parent Agent tool_use_id -> section state
	QueuedText        string                      // next user message queued for sending
	// Tip and LineParents are the model's active-branch chain, seeded from
	// the session at build time and extended live by Apply as messages
	// arrive.
	Tip         string
	LineParents map[string]string

	// Session metadata (set from base state, not from events).
	Cwd       string
	Label     string
	AutoLabel bool
	PermMode  claude.PermissionMode

	// pendingCommands stashes Bash commands from tool_use events so they
	// can be attached to BgTaskState when the tool_result arrives.
	pendingCommands map[string]string

	// Activity tracking: derived from event observations. turnActive is set by
	// the "running" broadcast, emitted from handleSend (a new session's first
	// turn), StartTurn, and waitAndDrainLoop. It is cleared by "done" (the
	// result event handler). processAlive is set true on session_started and
	// false on session_ended.
	turnActive   bool
	processAlive bool

	// Event channel management.
	sessionID string
	events    chan serverMsgEvent
	stopCh    chan struct{}
	wg        sync.WaitGroup

	// folding is set while BuildModel replays the log. During the fold the
	// chain comes whole from ModelBase, so Apply must not link live messages
	// into it. A uuid missing from the base chain is a transcript property,
	// not a live event to append after the tip.
	folding bool
}

// serverMsgEvent wraps a ServerMsg with its rendering context.
type serverMsgEvent struct {
	msg          ServerMsg
	todosChanged bool
	permUpgrades func([]DOMCmd) []DOMCmd // extra DOM commands to append after rendering
	push         func(string)            // callback to push rendered HTML to transport
	done         chan struct{}           // closed once processed. Set only by Sync
}

// ModelBase holds session-level state that is not derived from the event log.
type ModelBase struct {
	Cwd       string
	Label     string
	AutoLabel bool
	PermMode  claude.PermissionMode
	Cost      CostInfo // initial cost from session (includes ModelName set by history load)
	// Tip and LineParents seed the model's chain, which is then extended
	// live by Apply as messages arrive. ProcessAlive is the initial
	// process-liveness state, which session_started then maintains live.
	Tip          string
	LineParents  map[string]string
	ProcessAlive bool
}

// BuildModel folds a base state and an event log into a SessionModel.
// The returned model has its internal goroutine running.
func BuildModel(base ModelBase, log []ServerMsg, sessionID string) *SessionModel {
	// The model owns its chain, cloned here at the boundary, because the
	// session and the model mutate their chains live under different locks.
	lineParents := maps.Clone(base.LineParents)
	if lineParents == nil {
		lineParents = make(map[string]string)
	}
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
		Tip:               base.Tip,
		LineParents:       lineParents,
		pendingCommands:   make(map[string]string),
		sessionID:         sessionID,
		events:            make(chan serverMsgEvent, 256),
		stopCh:            make(chan struct{}),
		processAlive:      base.ProcessAlive,
		folding:           true,
	}
	for _, msg := range log {
		m.Apply(msg)
	}
	m.folding = false
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

// Sync blocks until every event enqueued before it has been applied and
// pushed. handleSend calls it before binding SSE clients to a new session,
// so no queued event's push can straddle the binding and append a message
// the full render sent after the binding already contains. If the model is
// closed while waiting, the goroutine that would process the marker is gone,
// so Sync returns without draining. That case cannot produce the duplicate:
// the closing path replaces the model with one built from the session log,
// which already holds every enqueued message.
func (m *SessionModel) Sync() {
	done := make(chan struct{})
	select {
	case m.events <- serverMsgEvent{done: done}:
	case <-m.stopCh:
		return
	}
	select {
	case <-done:
	case <-m.stopCh:
	}
}

func (m *SessionModel) sendEvent(ev serverMsgEvent) {
	select {
	case m.events <- ev:
	default:
		// Channel full; drop event. The model will be rebuilt on next
		// page load, so this is safe (no state corruption, just a
		// transient rendering gap).
		if ev.msg.Type == "task_done" {
			log.Printf("[model] DROPPED task_done for %s (channel full)", ev.msg.ToolUseID)
		}
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

// processEvent applies a single event and pushes rendered HTML through the
// event's push callback.
func (m *SessionModel) processEvent(ev serverMsgEvent) {
	if ev.done != nil {
		close(ev.done)
		return
	}
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
		if !s.Finished {
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

// Apply updates the model for a single event. It is called both by BuildModel
// (for page load) and by the internal goroutine (for live events).
func (m *SessionModel) Apply(msg ServerMsg) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Messages = append(m.Messages, msg)
	// A live message carrying a uuid not yet linked extends the active
	// chain by linking to the tip and advancing. This mirrors the session's tip
	// cursor, so full-page renders walk the same live tree. During the build
	// fold the chain arrives whole from ModelBase, so nothing links.
	if !m.folding && msg.UUID != "" && msg.AgentID == "" {
		if _, linked := m.LineParents[msg.UUID]; !linked {
			m.LineParents[msg.UUID] = m.Tip
			m.Tip = msg.UUID
		}
	}

	switch msg.Type {
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
				AgentID:     msg.AgentID,
				AgentType:   msg.AgentType,
				Description: msg.Description,
			}
		}

	case "subagent_finished":
		if msg.AgentID != "" {
			if s, ok := m.SubagentSections[msg.AgentID]; ok {
				s.Finished = true
				s.FinalText = msg.Text
				s.TotalTokens = msg.TotalTokens
				s.TotalToolUses = msg.TotalToolUses
				s.DurationMs = msg.DurationMs
				if msg.Description != "" {
					s.Description = msg.Description
				}
			}
		}
	}
}

// ChainSnapshot copies the active-branch tip and parent links under the
// model's mutex. RenderFull runs off the model goroutine and must not read
// the chain while Apply mutates it, since a map read racing a write is a
// runtime fatal.
func (m *SessionModel) ChainSnapshot() (string, map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Tip, maps.Clone(m.LineParents)
}
