package monetdroid

import (
	"sort"
	"sync"
	"time"
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
	Todos             []Todo
	SuppressedToolIDs map[string]string        // tool_use id → tool name, for suppressing results
	BgTaskStops       map[string]chan struct{} // tool_use id → stop channel for bg tailers
	DiffStat          DiffStat
	EventLog          EventLog
	PermChans         map[string]chan PermResponse
	proc              *ClaudeProcess
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

func (s *Session) GetTodosCopy() []Todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Todo, len(s.Todos))
	copy(out, s.Todos)
	return out
}

func (s *Session) GetPermChan(id string) (chan PermResponse, bool) {
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

func (s *Session) GetProc() *ClaudeProcess {
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

func (s *Session) FindPermInput(permID string) *ToolInput {
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

func (s *Session) SetProc(proc *ClaudeProcess) {
	s.mu.Lock()
	s.proc = proc
	s.mu.Unlock()
}

func (s *Session) SetDiffStat(ds DiffStat) {
	s.mu.Lock()
	s.DiffStat = ds
	s.mu.Unlock()
}

func (s *Session) SetTodos(todos []Todo) {
	s.mu.Lock()
	s.Todos = todos
	s.mu.Unlock()
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

func (s *Session) RegisterPermChan(id string) chan PermResponse {
	ch := make(chan PermResponse, 1)
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

func (s *Session) RegisterBgStop(id string, ch chan struct{}) {
	s.mu.Lock()
	s.BgTaskStops[id] = ch
	s.mu.Unlock()
}

func (s *Session) CloseBgStop(id string) {
	s.mu.Lock()
	if ch, ok := s.BgTaskStops[id]; ok {
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

func (s *Session) InitLive(label string, autoLabel, running bool, proc *ClaudeProcess) {
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

func (s *Session) SetPermModeAndGetProc(mode string) *ClaudeProcess {
	s.mu.Lock()
	s.PermissionMode = mode
	proc := s.proc
	s.mu.Unlock()
	return proc
}

func (s *Session) InterruptAndGetProc() *ClaudeProcess {
	s.mu.Lock()
	s.Interrupted = true
	proc := s.proc
	s.mu.Unlock()
	return proc
}

func (s *Session) ResetInterruptAndGetProc() *ClaudeProcess {
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
		PermChans:         make(map[string]chan PermResponse),
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
