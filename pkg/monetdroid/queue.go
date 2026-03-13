package monetdroid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// QueueItem represents a notification that needs user attention.
type QueueItem struct {
	ClaudeID  string `json:"claude_id"`
	Label     string `json:"label"`
	Status    string `json:"status"` // "completed" or "blocked"
	Result    string `json:"result"`
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
}

type queueFile struct {
	Queue []QueueItem `json:"queue"`
}

// NotificationQueue persists unacknowledged session events to disk.
type NotificationQueue struct {
	path  string
	items []QueueItem
	mu    sync.Mutex
}

func NewNotificationQueue() *NotificationQueue {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".monetdroid")
	os.MkdirAll(dir, 0o755)
	nq := &NotificationQueue{
		path: filepath.Join(dir, "queue.json"),
	}
	nq.load()
	return nq
}

func (nq *NotificationQueue) load() {
	data, err := os.ReadFile(nq.path)
	if err != nil {
		return
	}
	var f queueFile
	if json.Unmarshal(data, &f) == nil {
		nq.items = f.Queue
	}
}

func (nq *NotificationQueue) save() {
	data, _ := json.MarshalIndent(queueFile{Queue: nq.items}, "", "  ")
	os.WriteFile(nq.path, data, 0o644)
}

// Enqueue adds or updates a queue item for the given ClaudeID.
func (nq *NotificationQueue) Enqueue(item QueueItem) {
	nq.mu.Lock()
	defer nq.mu.Unlock()

	if item.Timestamp == "" {
		item.Timestamp = time.Now().Format(time.RFC3339)
	}

	// Replace existing item for same session
	for i, existing := range nq.items {
		if existing.ClaudeID == item.ClaudeID {
			nq.items[i] = item
			nq.save()
			return
		}
	}
	nq.items = append(nq.items, item)
	nq.save()
}

// Ack removes the queue item for the given ClaudeID.
func (nq *NotificationQueue) Ack(claudeID string) {
	nq.mu.Lock()
	defer nq.mu.Unlock()
	for i, item := range nq.items {
		if item.ClaudeID == claudeID {
			nq.items = append(nq.items[:i], nq.items[i+1:]...)
			nq.save()
			return
		}
	}
}

// List returns a copy of all queued items.
func (nq *NotificationQueue) List() []QueueItem {
	nq.mu.Lock()
	defer nq.mu.Unlock()
	out := make([]QueueItem, len(nq.items))
	copy(out, nq.items)
	return out
}

// LabelStore persists session labels (ClaudeID -> label) across server restarts.
type LabelStore struct {
	path   string
	labels map[string]string
	mu     sync.Mutex
}

func NewLabelStore() *LabelStore {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".monetdroid")
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
