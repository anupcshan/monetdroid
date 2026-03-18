package monetdroid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TrackedSession represents a session being tracked across its lifecycle.
type TrackedSession struct {
	ClaudeID  string `json:"claude_id"`
	Label     string `json:"label"`
	AutoLabel bool   `json:"auto_label,omitempty"`
	Status    string `json:"status"` // "running", "completed", or "blocked"
	Result    string `json:"result"`
	Cwd       string `json:"cwd"`
	UpdatedAtMillis int64 `json:"updated_at_millis"`
}

type trackerFile struct {
	Sessions []TrackedSession `json:"queue"` // JSON key kept as "queue" for backward compat
}

// SessionTracker persists tracked session state to disk.
type SessionTracker struct {
	path  string
	items []TrackedSession
	mu    sync.Mutex
}

func NewSessionTracker(dir string) *SessionTracker {
	os.MkdirAll(dir, 0o755)
	st := &SessionTracker{
		path: filepath.Join(dir, "queue.json"),
	}
	st.load()
	return st
}

func (st *SessionTracker) load() {
	data, err := os.ReadFile(st.path)
	if err != nil {
		return
	}
	var f trackerFile
	if json.Unmarshal(data, &f) == nil {
		st.items = f.Sessions
	}
}

func (st *SessionTracker) save() {
	data, _ := json.MarshalIndent(trackerFile{Sessions: st.items}, "", "  ")
	os.WriteFile(st.path, data, 0o644)
}

// Track adds or updates a tracked session for the given ClaudeID.
func (st *SessionTracker) Track(item TrackedSession) {
	st.mu.Lock()
	defer st.mu.Unlock()

	item.UpdatedAtMillis = time.Now().UnixMilli()

	// Replace existing item for same session
	for i, existing := range st.items {
		if existing.ClaudeID == item.ClaudeID {
			st.items[i] = item
			st.save()
			return
		}
	}
	st.items = append(st.items, item)
	st.save()
}

// Archive removes the tracked session for the given ClaudeID.
func (st *SessionTracker) Archive(claudeID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, item := range st.items {
		if item.ClaudeID == claudeID {
			st.items = append(st.items[:i], st.items[i+1:]...)
			st.save()
			return
		}
	}
}

// List returns a copy of all tracked sessions, most recently updated first.
func (st *SessionTracker) List() []TrackedSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]TrackedSession, len(st.items))
	copy(out, st.items)
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAtMillis > out[j].UpdatedAtMillis
	})
	return out
}

// LabelStore persists session labels (ClaudeID -> label) across server restarts.
type LabelStore struct {
	path   string
	labels map[string]string
	mu     sync.Mutex
}

func NewLabelStore(dir string) *LabelStore {
	os.MkdirAll(dir, 0o755)
	ls := &LabelStore{
		path:   filepath.Join(dir, "labels.json"),
		labels: make(map[string]string),
	}
	data, err := os.ReadFile(ls.path)
	if err == nil {
		json.Unmarshal(data, &ls.labels)
	}
	return ls
}

func (ls *LabelStore) save() {
	data, _ := json.MarshalIndent(ls.labels, "", "  ")
	os.WriteFile(ls.path, data, 0o644)
}

func (ls *LabelStore) Set(claudeID, label string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if label == "" {
		delete(ls.labels, claudeID)
	} else {
		ls.labels[claudeID] = label
	}
	ls.save()
}

func (ls *LabelStore) Get(claudeID string) string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.labels[claudeID]
}
