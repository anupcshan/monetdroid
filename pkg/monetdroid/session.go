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
	PermissionMode    string
	Running           bool
	Interrupted       bool
	CreatedAt         time.Time
	JSONLPath         string
	Log               []ServerMsg
	QueuedText        string
	CostAccum         CostInfo
	Todos             []Todo
	SuppressedToolIDs map[string]string      // tool_use id → tool name, for suppressing results
	BgTaskStops       map[string]chan struct{} // tool_use id → stop channel for bg tailers
	DiffStat          DiffStat
	EventLog          EventLog
	PermChans         map[string]chan PermResponse
	proc              *ClaudeProcess
	Mu                sync.Mutex
}

func (s *Session) Append(msg ServerMsg) {
	s.Mu.Lock()
	s.Log = append(s.Log, msg)
	s.Mu.Unlock()
}

// Close kills the session's claude process if running.
func (s *Session) Close() {
	s.Mu.Lock()
	proc := s.proc
	s.Mu.Unlock()
	if proc != nil && !proc.IsDead() {
		proc.Kill()
	}
}

func (s *Session) RemovePermission(permID string) {
	s.Mu.Lock()
	for i, m := range s.Log {
		if m.Type == "permission_request" && m.PermID == permID {
			s.Log = append(s.Log[:i], s.Log[i+1:]...)
			break
		}
	}
	s.Mu.Unlock()
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
