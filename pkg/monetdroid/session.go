package monetdroid

import (
	"fmt"
	"sync"
	"time"
)

type Session struct {
	ID             string
	ClaudeID       string
	Cwd            string
	PermissionMode string
	MessageCount   int
	Running        bool
	CreatedAt      time.Time
	JSONLPath      string
	Log            []ServerMsg
	QueuedText     string
	CostAccum      CostInfo
	PermChans      map[string]chan PermResponse
	WriteJSON      func(any)
	Mu             sync.Mutex
}

func (s *Session) Append(msg ServerMsg) {
	s.Mu.Lock()
	s.Log = append(s.Log, msg)
	s.Mu.Unlock()
}

type SessionManager struct {
	sessions map[string]*Session
	mu       sync.RWMutex
	counter  int
}

func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session)}
}

func (sm *SessionManager) Create(cwd string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.counter++
	id := fmt.Sprintf("s%d", sm.counter)
	s := &Session{ID: id, Cwd: cwd, CreatedAt: time.Now()}
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

func (sm *SessionManager) FindByClaudeID(claudeID string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, s := range sm.sessions {
		s.Mu.Lock()
		cid := s.ClaudeID
		s.Mu.Unlock()
		if cid == claudeID {
			return s
		}
	}
	return nil
}

func (sm *SessionManager) List() []*Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]*Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		out = append(out, s)
	}
	return out
}
