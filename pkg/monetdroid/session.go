package monetdroid

import (
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

type Session struct {
	ID                string
	Label             string
	AutoLabel         bool
	Cwd               string
	Branches          []string
	PermissionMode    string
	Running           bool
	Interrupted       bool
	CreatedAt         time.Time
	JSONLPath         string
	Log               []ServerMsg
	QueuedText        string
	CostAccum         CostInfo
	Todos             []protocol.Todo
	SuppressedToolIDs map[string]string        // tool_use id → tool name, for suppressing results
	BgTaskStops       map[string]chan struct{} // tool_use id → stop channel for bg tailers
	BgTaskPaths       map[string]string        // tool_use id → output file path
	DiffStat          DiffStat
	EventLog          EventLog
	PermChans         map[string]chan protocol.PermResponse
	AgentDepth        int                      // nesting depth of active sub-agents
	AgentToolIDs      map[string]bool          // active Agent tool_use IDs
	AgentEvents       map[string][]ServerMsg   // buffered sub-agent events per parent Agent tool_use ID
	AgentStats        map[string]*AgentStat    // live stats per Agent tool_use ID
	AgentStops        map[string]chan struct{} // stop channels for agent detail streams
	StreamingText     string                   // accumulated text from text_delta events
	StreamingThinking string                   // accumulated text from thinking_delta events
	proc              *claude.ClaudeProcess
	mu                sync.Mutex
}

func (s *Session) Append(msg ServerMsg) {
	s.mu.Lock()
	s.Log = append(s.Log, msg)
	s.mu.Unlock()
}

// Close kills the session's claude process if running.
func (s *Session) Close() {
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc != nil && !proc.IsDead() {
		proc.Kill()
	}
}

func (s *Session) RemovePermission(permID string) {
	s.mu.Lock()
	for i, m := range s.Log {
		if m.Type == "permission_request" && m.PermID == permID {
			s.Log = append(s.Log[:i], s.Log[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
}

// --- Getters ---

func (s *Session) GetCwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Cwd
}

func (s *Session) GetLog() []ServerMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ServerMsg, len(s.Log))
	copy(out, s.Log)
	return out
}

func (s *Session) GetLabelAndCwd() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Label, s.Cwd
}

type TrackerInfo struct {
	Label     string
	AutoLabel bool
	Cwd       string
	Branches  []string
}

func (s *Session) GetTrackerInfo() TrackerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return TrackerInfo{
		Label:     s.Label,
		AutoLabel: s.AutoLabel,
		Cwd:       s.Cwd,
		Branches:  s.Branches,
	}
}

func (s *Session) GetCostBarInfo() (CostInfo, DiffStat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CostAccum, s.DiffStat
}

func (s *Session) Stats() (msgCount, ctxUsed, ctxWindow int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Log), s.CostAccum.ContextUsed, s.CostAccum.ContextWindow
}

func (s *Session) GetTodosCopy() []protocol.Todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.Todo, len(s.Todos))
	copy(out, s.Todos)
	return out
}

func (s *Session) GetPermChan(id string) (chan protocol.PermResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.PermChans[id]
	return ch, ok
}

func (s *Session) HasPendingPerms() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.PermChans) > 0
}

func (s *Session) GetProc() *claude.ClaudeProcess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc
}

func (s *Session) LastAssistantText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.Log) - 1; i >= 0; i-- {
		if s.Log[i].Type == "text" && s.Log[i].Text != "" {
			return s.Log[i].Text
		}
	}
	return ""
}

func (s *Session) IsTopLevelTool(toolUseID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Log {
		if m.Type == "tool_use" && m.ToolUseID == toolUseID {
			return true
		}
	}
	return false
}

func (s *Session) FindPermToolUseID(permID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Log {
		if m.Type == "permission_request" && m.PermID == permID {
			return m.ToolUseID
		}
	}
	return ""
}

func (s *Session) FindPermInput(permID string) *protocol.ToolInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.Log {
		if m.Type == "permission_request" && m.PermID == permID {
			return m.PermInput
		}
	}
	return nil
}

// --- Setters ---

func (s *Session) SetRunning(v bool) {
	s.mu.Lock()
	s.Running = v
	s.mu.Unlock()
}

func (s *Session) SetProc(proc *claude.ClaudeProcess) {
	s.mu.Lock()
	s.proc = proc
	s.mu.Unlock()
}

// AppendStreamingText appends a text delta and returns the accumulated text.
func (s *Session) AppendStreamingText(delta string) string {
	s.mu.Lock()
	s.StreamingText += delta
	result := s.StreamingText
	s.mu.Unlock()
	return result
}

// AppendStreamingThinking appends a thinking delta and returns the accumulated text.
func (s *Session) AppendStreamingThinking(delta string) string {
	s.mu.Lock()
	s.StreamingThinking += delta
	result := s.StreamingThinking
	s.mu.Unlock()
	return result
}

// ClearStreaming resets the streaming accumulators.
func (s *Session) ClearStreaming() {
	s.mu.Lock()
	s.StreamingText = ""
	s.StreamingThinking = ""
	s.mu.Unlock()
}

func (s *Session) SetDiffStat(ds DiffStat) {
	s.mu.Lock()
	s.DiffStat = ds
	s.mu.Unlock()
}

func (s *Session) SetTodos(todos []protocol.Todo) {
	s.mu.Lock()
	s.Todos = todos
	s.mu.Unlock()
}

// AppendTaskFromCreate appends a new Todo for a TaskCreate event. The CLI
// assigns sequential int IDs starting at 1 (observed in result text like
// "Task #1 created successfully"); we mirror that scheme so TaskUpdate's
// taskId can find the entry.
func (s *Session) AppendTaskFromCreate(input *protocol.TaskCreateInput) {
	if input == nil {
		return
	}
	s.mu.Lock()
	s.Todos = append(s.Todos, protocol.Todo{
		ID:         strconv.Itoa(len(s.Todos) + 1),
		Content:    input.Subject,
		ActiveForm: input.ActiveForm,
		Status:     "pending",
	})
	s.mu.Unlock()
}

// UpdateTask applies a TaskUpdate to the matching Todo by ID. status="deleted"
// removes the entry. Unknown IDs are ignored.
func (s *Session) UpdateTask(input *protocol.TaskUpdateInput) {
	if input == nil || input.TaskID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.Todos {
		if t.ID != input.TaskID {
			continue
		}
		if input.Status == "deleted" {
			s.Todos = append(s.Todos[:i], s.Todos[i+1:]...)
			return
		}
		if input.Status != "" {
			s.Todos[i].Status = input.Status
		}
		return
	}
}

func (s *Session) SetPermissionMode(mode string) {
	s.mu.Lock()
	s.PermissionMode = mode
	s.mu.Unlock()
}

func (s *Session) ClearQueue() {
	s.mu.Lock()
	s.QueuedText = ""
	s.mu.Unlock()
}

func (s *Session) GetQueuedText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.QueuedText
}

// --- Map operations ---

func (s *Session) RegisterPermChan(id string) chan protocol.PermResponse {
	ch := make(chan protocol.PermResponse, 1)
	s.mu.Lock()
	s.PermChans[id] = ch
	s.mu.Unlock()
	return ch
}

func (s *Session) DeletePermChan(id string) {
	s.mu.Lock()
	delete(s.PermChans, id)
	s.mu.Unlock()
}

// HandlePermission registers a permission channel, broadcasts the request,
// and blocks until the user responds via the web UI.
func (s *Session) HandlePermission(req protocol.PermissionRequest, broadcast func(ServerMsg)) protocol.PermResponse {
	ch := s.RegisterPermChan(req.RequestID)
	msg := ServerMsg{
		Type:            "permission_request",
		SessionID:       s.ID,
		ToolUseID:       req.ToolUseID,
		PermID:          req.RequestID,
		PermTool:        req.ToolName,
		PermInput:       req.Input,
		PermReason:      req.DecisionReason,
		PermSuggestions: req.Suggestions,
	}
	s.Append(msg)
	broadcast(msg)
	resp := <-ch
	s.DeletePermChan(req.RequestID)
	return resp
}

func (s *Session) RegisterBgStop(id string, ch chan struct{}) {
	s.mu.Lock()
	s.BgTaskStops[id] = ch
	s.mu.Unlock()
}

func (s *Session) RegisterBgPath(id, path string) {
	s.mu.Lock()
	if s.BgTaskPaths == nil {
		s.BgTaskPaths = make(map[string]string)
	}
	s.BgTaskPaths[id] = path
	s.mu.Unlock()
}

func (s *Session) GetBgPath(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.BgTaskPaths[id]
}

func (s *Session) CloseBgStop(id string) {
	s.mu.Lock()
	if ch, ok := s.BgTaskStops[id]; ok {
		close(ch)
		delete(s.BgTaskStops, id)
	}
	s.mu.Unlock()
}

func (s *Session) CloseAllBgStops() {
	s.mu.Lock()
	for id, ch := range s.BgTaskStops {
		close(ch)
		delete(s.BgTaskStops, id)
	}
	s.mu.Unlock()
}

func (s *Session) SuppressTool(id, name string) {
	s.mu.Lock()
	s.SuppressedToolIDs[id] = name
	s.mu.Unlock()
}

func (s *Session) RemoveSuppressed(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.SuppressedToolIDs[id]
	if ok {
		delete(s.SuppressedToolIDs, id)
	}
	return ok
}

// --- Agent operations ---

// StartAgent registers a new sub-agent and increments the nesting depth.
func (s *Session) StartAgent(toolUseID, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AgentDepth++
	if s.AgentToolIDs == nil {
		s.AgentToolIDs = make(map[string]bool)
	}
	s.AgentToolIDs[toolUseID] = true
	if s.AgentStats == nil {
		s.AgentStats = make(map[string]*AgentStat)
	}
	s.AgentStats[toolUseID] = &AgentStat{Description: description}
	if s.AgentEvents == nil {
		s.AgentEvents = make(map[string][]ServerMsg)
	}
	if s.AgentStops == nil {
		s.AgentStops = make(map[string]chan struct{})
	}
	s.AgentStops[toolUseID] = make(chan struct{})
}

// FinishAgent marks an agent as completed and decrements depth.
func (s *Session) FinishAgent(toolUseID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AgentToolIDs[toolUseID] {
		s.AgentDepth--
		delete(s.AgentToolIDs, toolUseID)
	}
	if stat := s.AgentStats[toolUseID]; stat != nil {
		stat.Completed = true
	}
	if ch, ok := s.AgentStops[toolUseID]; ok {
		close(ch)
		delete(s.AgentStops, toolUseID)
	}
}

// UpdateAgentStat updates the live stats for an agent from a task_progress event.
func (s *Session) UpdateAgentStat(toolUseID string, usage *protocol.TaskUsage, description, lastTool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stat := s.AgentStats[toolUseID]
	if stat == nil {
		return
	}
	if usage != nil {
		stat.TotalTokens = usage.TotalTokens
		stat.ToolUses = usage.ToolUses
		stat.DurationMs = usage.DurationMs
	}
	if description != "" {
		stat.Description = description
	}
	if lastTool != "" {
		stat.LastToolName = lastTool
	}
}

// BufferAgentEvent appends a ServerMsg to the buffer for a sub-agent.
func (s *Session) BufferAgentEvent(parentToolUseID string, msg ServerMsg) {
	s.mu.Lock()
	s.AgentEvents[parentToolUseID] = append(s.AgentEvents[parentToolUseID], msg)
	s.mu.Unlock()
}

// GetAgentEvents returns a copy of the buffered events for an agent.
func (s *Session) GetAgentEvents(toolUseID string) []ServerMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.AgentEvents[toolUseID]
	out := make([]ServerMsg, len(events))
	copy(out, events)
	return out
}

// GetAgentStat returns a copy of the agent stat.
func (s *Session) GetAgentStat(toolUseID string) *AgentStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	stat := s.AgentStats[toolUseID]
	if stat == nil {
		return nil
	}
	cp := *stat
	return &cp
}

// GetAgentDepth returns the current agent nesting depth.
func (s *Session) GetAgentDepth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AgentDepth
}

// GetAgentStop returns the stop channel for an agent's detail stream, if still running.
func (s *Session) GetAgentStop(toolUseID string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AgentStops[toolUseID]
}

// --- Compound operations ---

func (s *Session) AccumulateCost(cost *CostInfo) {
	s.mu.Lock()
	if cost.TotalCostUSD > 0 {
		s.CostAccum.TotalCostUSD = cost.TotalCostUSD
	}
	if cost.ContextUsed > 0 {
		s.CostAccum.ContextUsed = cost.ContextUsed
	}
	if cost.ContextWindow > 0 {
		s.CostAccum.ContextWindow = cost.ContextWindow
	}
	s.mu.Unlock()
}

func (s *Session) InitFromHistory(label, jsonlPath string, branches []string, cost CostInfo) {
	s.mu.Lock()
	s.Label = label
	s.JSONLPath = jsonlPath
	s.Branches = branches
	s.CostAccum = cost
	s.mu.Unlock()
}

func (s *Session) InitLive(label string, autoLabel, running bool, proc *claude.ClaudeProcess) {
	s.mu.Lock()
	s.Label = label
	s.AutoLabel = autoLabel
	s.Running = running
	s.proc = proc
	s.mu.Unlock()
}

// UpdateLabel sets the label and returns the display label (falls back to short cwd path).
func (s *Session) UpdateLabel(label string) string {
	s.mu.Lock()
	s.Label = label
	if label == "" {
		label = ShortPath(s.Cwd)
	}
	s.mu.Unlock()
	return label
}

// TryAutoLabel sets the label from text if no label is set yet.
func (s *Session) TryAutoLabel(text string) {
	s.mu.Lock()
	if s.Label == "" && text != "" {
		label := text
		if len(label) > 60 {
			label = label[:60] + "..."
		}
		s.Label = label
		s.AutoLabel = true
	}
	s.mu.Unlock()
}

func (s *Session) SetPermModeAndGetProc(mode string) *claude.ClaudeProcess {
	s.mu.Lock()
	s.PermissionMode = mode
	proc := s.proc
	s.mu.Unlock()
	return proc
}

func (s *Session) InterruptAndGetProc() *claude.ClaudeProcess {
	s.mu.Lock()
	s.Interrupted = true
	proc := s.proc
	s.mu.Unlock()
	return proc
}

func (s *Session) ResetInterruptAndGetProc() *claude.ClaudeProcess {
	s.mu.Lock()
	s.Interrupted = false
	proc := s.proc
	s.mu.Unlock()
	return proc
}

// EnqueueMessage adds text to the queue if actively streaming (running with
// no pending permission requests). Returns whether queued and the full queue text.
// When permission-blocked, returns false so the caller can inject immediately.
func (s *Session) EnqueueMessage(text string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Running || len(s.PermChans) > 0 {
		return false, ""
	}
	if s.QueuedText != "" {
		s.QueuedText += "\n" + text
	} else {
		s.QueuedText = text
	}
	return true, s.QueuedText
}

// DrainQueue returns the interrupted flag and queued text, clearing it if not interrupted.
func (s *Session) DrainQueue() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	interrupted := s.Interrupted
	next := s.QueuedText
	if !interrupted {
		s.QueuedText = ""
	}
	return interrupted, next
}

type SeedState struct {
	Log        []ServerMsg
	Running    bool
	PermMode   string
	Label      string
	AutoLabel  bool
	Cwd        string
	QueuedText string
}

func (s *Session) SeedSnapshot() SeedState {
	s.mu.Lock()
	defer s.mu.Unlock()
	log_ := make([]ServerMsg, len(s.Log))
	copy(log_, s.Log)
	return SeedState{
		Log:        log_,
		Running:    s.Running,
		PermMode:   s.PermissionMode,
		Label:      s.Label,
		AutoLabel:  s.AutoLabel,
		Cwd:        s.Cwd,
		QueuedText: s.QueuedText,
	}
}

type SessionManager struct {
	sessions map[string]*Session
	mu       sync.RWMutex
}

func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session)}
}

func (sm *SessionManager) Create(id, cwd string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s := &Session{
		ID:                id,
		Cwd:               cwd,
		CreatedAt:         time.Now(),
		SuppressedToolIDs: make(map[string]string),
		BgTaskStops:       make(map[string]chan struct{}),
		PermChans:         make(map[string]chan protocol.PermResponse),
	}
	sm.sessions[id] = s
	return s
}

func (sm *SessionManager) Get(id string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[id]
}

func (sm *SessionManager) FindByJSONLPath(path string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, s := range sm.sessions {
		if s.JSONLPath == path {
			return s
		}
	}
	return nil
}

func (sm *SessionManager) Remove(id string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s := sm.sessions[id]
	delete(sm.sessions, id)
	return s
}

func (sm *SessionManager) List() []*Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]*Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}
