package monetdroid

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// SSEEvent is a rendered SSE event string tagged with a monotonic sequence number.
type SSEEvent struct {
	Seq        uint64
	Event      string
	CompactKey string // dedup key. events sharing this key supersede each other
	ParentKey  string // parent key. cascade-removed when the parent is superseded
}

// EventLog is an append-only log of rendered SSE events for a session.
// Broadcast appends to it; replay reads from it. This ensures both paths
// produce identical output. Events with a CompactKey are deduplicated:
// when a new event arrives with the same key, the older one is removed.
type EventLog struct {
	mu     sync.Mutex
	events []SSEEvent
	seq    uint64
}

// compactKey returns a dedup key for single-element OOB swaps that use
// innerHTML or outerHTML. Multi-swap events and beforeend appends return "".
func compactKey(event string) string {
	if strings.Count(event, "hx-swap-oob=") != 1 {
		return ""
	}
	if !strings.Contains(event, `hx-swap-oob="innerHTML"`) &&
		!strings.Contains(event, `hx-swap-oob="outerHTML"`) {
		return ""
	}
	idx := strings.Index(event, `id="`)
	if idx < 0 {
		return ""
	}
	start := idx + 4
	end := strings.IndexByte(event[start:], '"')
	if end < 0 {
		return ""
	}
	return event[start : start+end]
}

// Append adds an event to the log and returns its sequence number.
// key is an explicit compact key. When empty, one is derived from the event HTML.
// parent is an optional parent reference. Removed when a CompactKey match on
// parent is superseded.
func (l *EventLog) Append(event string, key string, parent string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if key == "" {
		key = compactKey(event)
	}
	if key != "" {
		for i := len(l.events) - 1; i >= 0; i-- {
			if i >= len(l.events) {
				continue
			}
			if l.events[i].CompactKey == key {
				l.events = append(l.events[:i], l.events[i+1:]...)
				// Remove all events parented to the superseded key.
				for j := len(l.events) - 1; j >= 0; j-- {
					if l.events[j].ParentKey == key {
						l.events = append(l.events[:j], l.events[j+1:]...)
					}
				}
			}
		}
	}
	l.seq++
	l.events = append(l.events, SSEEvent{Seq: l.seq, Event: event, CompactKey: key, ParentKey: parent})
	return l.seq
}

// Snapshot returns a copy of all events and the sequence number of the last one.
// Returns 0 if the log is empty.
func (l *EventLog) Snapshot() ([]SSEEvent, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]SSEEvent, len(l.events))
	copy(out, l.events)
	return out, l.seq
}

type SSEClient struct {
	id        string
	sessionID string
	cwd       string // set when ?cwd= connects before a session exists
	label     string // label from ?label= for pre-session state
	events    chan SSEEvent
	mu        sync.Mutex
}

func (c *SSEClient) Send(event SSEEvent) {
	select {
	case c.events <- event:
	default:
	}
}

func (c *SSEClient) ActiveSession() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *SSEClient) SetSession(id string) {
	c.mu.Lock()
	c.sessionID = id
	c.mu.Unlock()
}

// NotifyClient receives lightweight notification events (permission prompts, task completion).
type NotifyClient struct {
	events chan string
}

type Hub struct {
	clients       map[string]*SSEClient
	notifyClients map[string]*NotifyClient
	Sessions      *SessionManager
	Tracker       *SessionTracker
	Labels        *LabelStore
	Reviews       *ReviewStore
	// hookBaseURL is the http://host:port that prefixes every hook URL.
	hookBaseURL string
	hooks       hookRegistry
	// claudeCommand overrides the claude CLI invocation. Nil/empty means
	// use the default "claude" in PATH (resolved by
	// claude.StartProcessWithConfig). Set at construction; read-only after.
	claudeCommand []string
	hookLog       *hookLog
	mu            sync.RWMutex
}

// Close kills all active claude processes.
func (h *Hub) Close() {
	for _, s := range h.Sessions.List() {
		s.Close()
	}
}

func (h *Hub) AddNotifyClient(id string) *NotifyClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := &NotifyClient{events: make(chan string, 16)}
	h.notifyClients[id] = c
	return c
}

func (h *Hub) RemoveNotifyClient(id string) {
	h.mu.Lock()
	delete(h.notifyClients, id)
	h.mu.Unlock()
}

func (h *Hub) notifyAll(event string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.notifyClients {
		select {
		case c.events <- event:
		default:
		}
	}
}

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".monetdroid")
}

// NewHub constructs a Hub. claudeCommand overrides the claude CLI
// invocation; nil/empty uses the default "claude" in PATH. When non-empty,
// claudeCommand[0] is validated with exec.LookPath so a missing binary
// fails here rather than at session start.
func NewHub(hookBaseURL string, claudeCommand []string) (*Hub, error) {
	return NewHubWithDataDir(hookBaseURL, defaultDataDir(), claudeCommand)
}

func NewHubWithDataDir(hookBaseURL, dataDir string, claudeCommand []string) (*Hub, error) {
	if len(claudeCommand) > 0 {
		if _, err := exec.LookPath(claudeCommand[0]); err != nil {
			return nil, fmt.Errorf("claude binary %q: %w", claudeCommand[0], err)
		}
	}
	go func() {
		t := NewGitTrace("warm-cache")
		defer t.Log()
		ScanHistory(t)
	}()
	h := &Hub{
		clients:       make(map[string]*SSEClient),
		notifyClients: make(map[string]*NotifyClient),
		Sessions:      NewSessionManager(),
		Tracker:       NewSessionTracker(dataDir),
		Labels:        NewLabelStore(dataDir),
		Reviews:       NewReviewStore(),
		hookBaseURL:   hookBaseURL,
		claudeCommand: append([]string(nil), claudeCommand...),
		hookLog:       newHookLog(500),
	}
	return h, nil
}

// NewBashstreamerEnv creates a per-process temp directory containing a
// CLAUDE_CODE_SHELL_PREFIX wrapper script and signal file. The wrapper
// reads the session ID from the signal and embeds it in the push URL so
// the server can route directly to the correct session. Returns the
// ExtraEnv entry, the temp directory (for cleanup), and the signal path.
// Returns nil env if bashstreamer is not on PATH or setup fails, so
// Claude falls back to its default shell without streaming.
func NewBashstreamerEnv(hookBaseURL string) (env []string, dir string, signal string) {
	if _, err := exec.LookPath("bashstreamer"); err != nil {
		return nil, "", ""
	}
	dir, err := os.MkdirTemp("", "monetdroid-bs-")
	if err != nil {
		return nil, "", ""
	}
	pushURL := hookBaseURL + "/bash-stream/"
	script := filepath.Join(dir, "bashstreamer-wrapper")
	signalFile := filepath.Join(dir, "signal")
	// CLAUDE_CODE_SHELL_PREFIX passes the full shell invocation as $1.
	// The signal file contains "session_id tool_use_id" (space-separated).
	// The wrapper extracts both and passes them to bashstreamer via the
	// push URL path, so handleBashStream can route directly. bashstreamer
	// is verified above, so the wrapper execs it unconditionally; there is
	// no fallback, since exec replaces the shell (an unreachable ||) and
	// bashstreamer forwards the child's exit code (a fallback would re-run
	// failed commands).
	content := fmt.Sprintf(`#!/bin/sh
if [ -f %s ]; then
    sid=$(cut -d' ' -f1 %s)
    tid=$(cut -d' ' -f2 %s)
    rm -f %s
    exec bashstreamer --push-url %s$sid/$tid -- -c "$1"
fi
exec /bin/bash -c "$1"
`, signalFile, signalFile, signalFile, signalFile, pushURL)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		os.RemoveAll(dir)
		return nil, "", ""
	}
	return []string{
		"CLAUDE_CODE_SHELL_PREFIX=" + script,
	}, dir, signalFile
}

func (h *Hub) RemoveClient(cid string) {
	h.mu.Lock()
	delete(h.clients, cid)
	h.mu.Unlock()
}

func (h *Hub) BroadcastToSession(sessionID string, event string, key string, parent string) {
	s := h.Sessions.Get(sessionID)
	var seq uint64
	if s != nil {
		seq = s.EventLog.Append(event, key, parent)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	sseEvent := SSEEvent{Seq: seq, Event: event}
	for _, c := range h.clients {
		if c.ActiveSession() == sessionID {
			c.Send(sseEvent)
		}
	}
}

// Broadcast sends a ServerMsg to all clients viewing that session.
func (h *Hub) Broadcast(msg ServerMsg) {
	sessionID := msg.SessionID
	if sessionID == "" {
		return
	}
	s := h.Sessions.Get(sessionID)

	if msg.Type == "cost" && msg.Cost != nil && s != nil {
		s.AccumulateCost(msg.Cost)
	}

	// Persist and apply every non-streaming event. Appending to the log
	// here makes Broadcast the single funnel; callers do not need to
	// call s.Append separately.
	if s != nil && msg.Type != "text_delta" && msg.Type != "thinking_delta" {
		s.Append(msg)
	}

	// Update todos from TodoWrite/TaskCreate/TaskUpdate tool_use events
	todosChanged := false
	if msg.Type == "tool_use" && s != nil {
		switch msg.Tool {
		case "TodoWrite":
			if todos := ParseTodos(msg.Input); todos != nil {
				s.SetTodos(todos)
				todosChanged = true
			}
		case "TaskCreate":
			if msg.Input != nil {
				s.AppendTaskFromCreate(msg.Input.TaskCreate)
				todosChanged = true
			}
		case "TaskUpdate":
			if msg.Input != nil {
				s.UpdateTask(msg.Input.TaskUpdate)
				todosChanged = true
			}
		}
		// Stash the command for background task output extraction.
		if msg.Input != nil && msg.Input.Bash != nil && msg.Input.Bash.Command != "" {
			s.RegisterBgCommand(msg.ToolUseID, msg.Input.Bash.Command)
		}
	}

	// --- Streaming deltas (text_delta, thinking_delta) ---
	// First delta of a stream creates the container via innerHTML (key "streaming").
	// Subsequent deltas append text fragments via beforeend (parent "streaming",
	// cascade-removed when the next container replaces the parent).
	if msg.Type == "text_delta" && s != nil {
		_, first := s.AppendStreamingTextAtomically(msg.Text)
		if first {
			event := FormatSSE("htmx", strings.Join([]string{
				OobSwap("thinking", "innerHTML", ""),
				OobSwap("streaming", "innerHTML",
					fmt.Sprintf(`<div class="msg msg-assistant"><div class="msg-bubble streaming-text" id="streaming-detail">%s</div></div>`, Esc(msg.Text))),
			}, "\n"))
			h.BroadcastToSession(sessionID, event, "streaming", "")
		} else {
			event := FormatSSE("htmx", OobSwap("streaming-detail", "beforeend", Esc(msg.Text)))
			h.BroadcastToSession(sessionID, event, "", "streaming")
		}
		return
	}
	if msg.Type == "thinking_delta" && s != nil {
		accumulated, first := s.AppendStreamingThinkingAtomically(msg.Text)
		if first {
			preview := msg.Text
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
			event := FormatSSE("htmx", strings.Join([]string{
				OobSwap("thinking", "innerHTML", ""),
				OobSwap("streaming", "innerHTML",
					fmt.Sprintf(`<div class="msg msg-thinking"><details class="thinking-chip" open><summary class="thinking-summary" id="streaming-summary">%s</summary><div class="thinking-detail" id="streaming-detail">%s</div></details></div>`, Esc(preview), Esc(msg.Text))),
			}, "\n"))
			h.BroadcastToSession(sessionID, event, "streaming", "")
		} else {
			preview := accumulated
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
			event := FormatSSE("htmx", strings.Join([]string{
				OobSwap("streaming-detail", "beforeend", Esc(msg.Text)),
				OobSwap("streaming-summary", "innerHTML", Esc(preview)),
			}, "\n"))
			h.BroadcastToSession(sessionID, event, "", "streaming")
		}
		return
	}

	// When a final text/thinking message arrives, clear the streaming accumulator
	if (msg.Type == "text" || msg.Type == "thinking") && s != nil {
		s.ClearStreaming()
	}

	// Push to notification clients (Android app)
	if msg.Type == "permission_request" && s != nil {
		permLabel := msg.PermTool
		if msg.PermReason != "" {
			permLabel = msg.PermTool + ": " + msg.PermReason
		}
		info := s.GetTrackerInfo()
		data := fmt.Sprintf(`{"text":%q,"session":%q,"cwd":%q}`, permLabel, s.ID, ShortPath(info.Cwd))
		h.notifyAll(FormatSSE("permission", data))

		h.Tracker.Track(TrackedSession{
			ClaudeID:  s.ID,
			Label:     info.Label,
			AutoLabel: info.AutoLabel,
			Status:    "blocked",
			Result:    permLabel,
			Cwd:       info.Cwd,
			Branches:  info.Branches,
		})
	}
	if msg.Type == "done" && s != nil {
		info := s.GetTrackerInfo()
		result := s.LastAssistantText()
		data := fmt.Sprintf(`{"text":"task complete","session":%q,"cwd":%q}`, s.ID, ShortPath(info.Cwd))
		h.notifyAll(FormatSSE("done", data))

		if len(result) > 200 {
			result = result[:200] + "..."
		}
		h.Tracker.Track(TrackedSession{
			ClaudeID:  s.ID,
			Label:     info.Label,
			AutoLabel: info.AutoLabel,
			Status:    "completed",
			Result:    result,
			Cwd:       info.Cwd,
			Branches:  info.Branches,
		})
	}

	if msg.Type == "running" && s != nil {
		s.ClearStreaming()
		info := s.GetTrackerInfo()
		h.Tracker.Track(TrackedSession{
			ClaudeID:  s.ID,
			Label:     info.Label,
			AutoLabel: info.AutoLabel,
			Status:    "running",
			Cwd:       info.Cwd,
			Branches:  info.Branches,
		})
	}
	if msg.Type == "done" {
		// Refresh git diff stat before sending to model so the cost bar
		// rendered by the "done" event reflects the latest diff.
		if s != nil {
			cwd := s.GetCwd()
			t := NewGitTrace("diff-stat")
			defer t.Log()
			if ds, err := GitDiffStat(t, cwd); err == nil {
				s.SetDiffStat(ds)
				if s.Model != nil {
					s.Model.DiffStat = ds
				}
			}
		}
	}
	// Remove spinner when tool_result arrives
	if msg.Type == "tool_result" && msg.ToolUseID != "" {
		// For background Bash tasks, insert a lazy-load SSE slot for the output.
		if bgPath := ParseBgTaskPath(msg.Output); bgPath != "" {
			// Register the output path and stop channel for the stream endpoint.
			toolUseID := msg.ToolUseID
			stopCh := make(chan struct{})
			if s != nil {
				s.RegisterBgStop(toolUseID, stopCh)
				s.RegisterBgPath(toolUseID, bgPath)
			}
			// Run an elapsed timer that ticks every second until the task completes.
			go func() {
				started := time.Now()
				for {
					select {
					case <-stopCh:
						return
					case <-time.After(1 * time.Second):
						secs := int(time.Since(started).Seconds())
						h.BroadcastToSession(sessionID, FormatSSE("htmx",
							OobSwap("elapsed-"+toolUseID, "innerHTML", fmt.Sprintf("%ds", secs))), "", "")
					}
				}
			}()
		}
		// For foreground bash commands that were streamed via bashstreamer,
		// clear the streaming div now that the final result is available.
		if s != nil && s.ConsumeStreamedBash(msg.ToolUseID) {
			h.BroadcastToSession(sessionID, FormatSSE("htmx",
				OobSwap("streaming-bash-"+msg.ToolUseID, "innerHTML", "")), "", "")
		}
	}

	// Flush accumulated streaming deltas to the permanent log when a
	// displayable message arrives (text, thinking, tool_use, tool_result).
	if isStreamingFlushTrigger(msg) && s != nil {
		streamText, streamThinking := s.DrainStreaming()
		if streamText != "" {
			s.Append(ServerMsg{Type: "text", SessionID: s.ID, Text: streamText})
			flushHTML := RenderMsg(ServerMsg{Type: "text", SessionID: s.ID, Text: streamText})
			if flushHTML != "" {
				h.BroadcastToSession(sessionID, FormatSSE("htmx", OobSwap("msg-content", "beforeend", flushHTML)), "", "")
			}
		}
		if streamThinking != "" {
			s.Append(ServerMsg{Type: "thinking", SessionID: s.ID, Text: streamThinking})
			flushHTML := RenderMsg(ServerMsg{Type: "thinking", SessionID: s.ID, Text: streamThinking})
			if flushHTML != "" {
				h.BroadcastToSession(sessionID, FormatSSE("htmx", OobSwap("msg-content", "beforeend", flushHTML)), "", "")
			}
		}
	}

	// Send event to model for async state mutation and viewer push.
	// Streaming deltas bypass the model (they don't mutate state).
	if s != nil && s.Model != nil && msg.Type != "text_delta" && msg.Type != "thinking_delta" {
		// Build permission upgrade callback (session-dependent logic).
		var permUpgrades func([]DOMCmd) []DOMCmd
		if msg.Type == "permission_request" && msg.PermTool != "AskUserQuestion" && msg.ToolUseID != "" {
			toolUseID := msg.ToolUseID
			sess := s
			permUpgrades = func(cmds []DOMCmd) []DOMCmd {
				if sess.IsTopLevelTool(toolUseID) {
					upgraded := false
					switch msg.PermTool {
					case "Edit", "FileEdit":
						if fp, old, new_, replAll, ok := editDiffFromInput(msg.PermInput); ok {
							if diffHTML := RenderEditDiffTable(fp, old, new_, replAll, msg.SessionID, true); diffHTML != "" {
								cmds = append(cmds, DOMCmd{Target: "tool-detail-" + toolUseID, Strategy: "innerHTML", Content: diffHTML})
								upgraded = true
							}
						}
					case "Write", "FileWrite":
						if fp, content, ok := writeDiffFromInput(msg.PermInput); ok {
							if diffHTML := RenderWriteDiffTable(fp, content, msg.SessionID, true); diffHTML != "" {
								cmds = append(cmds, DOMCmd{Target: "tool-detail-" + toolUseID, Strategy: "innerHTML", Content: diffHTML})
								upgraded = true
							}
						}
					case "ExitPlanMode":
						if msg.PermInput != nil && msg.PermInput.PlanMode != nil && msg.PermInput.PlanMode.Plan != "" {
							cmds = append(cmds, DOMCmd{Target: "tool-detail-" + toolUseID, Strategy: "innerHTML", Content: RenderMarkdown(msg.PermInput.PlanMode.Plan)})
							upgraded = true
						}
					}
					if !upgraded {
						permDetail := FormatPermDetail(msg.PermTool, msg.PermInput)
						if permDetail != "" {
							cmds = append(cmds, DOMCmd{Target: "tool-detail-" + toolUseID, Strategy: "innerHTML", Content: Esc(permDetail)})
						}
					}
				} else {
					rendered := RenderPermission(msg)
					if rendered != "" {
						cmds = append(cmds, DOMCmd{Target: "msg-content", Strategy: "beforeend", Content: rendered})
					}
				}
				return cmds
			}
		}
		push := func(html string) {
			h.BroadcastToSession(sessionID, html, "", "")
		}
		switch {
		case permUpgrades != nil:
			s.Model.HandleEventWithUpgrades(msg, todosChanged, permUpgrades, push)
		case todosChanged:
			s.Model.HandleEventWithTodos(msg, todosChanged, push)
		default:
			s.Model.HandleEvent(msg, push)
		}
	}
}

// isStreamingFlushTrigger reports whether msg should flush accumulated
// streaming deltas to the permanent log before being displayed.
func isStreamingFlushTrigger(msg ServerMsg) bool {
	switch msg.Type {
	case "text", "thinking", "tool_use", "tool_result":
		return true
	}
	return false
}

func (h *Hub) StartTurn(s *Session, text string, images []protocol.ImageData) {
	proc := s.ResetInterruptAndGetProc()

	// Ensure process is alive
	if proc == nil || proc.IsDead() {
		broadcast := func(msg ServerMsg) {
			// Streaming deltas are ephemeral. Broadcast handles persistence.
			h.Broadcast(msg)
		}
		bsEnv, bsDir, bsSignal := NewBashstreamerEnv(h.hookBaseURL)
		var err error
		proc, err = claude.StartProcessWithConfig(s.GetCwd(), func(event protocol.StreamEvent) {
			handleStreamEvent(s, &event, broadcast)
		}, s.ID, &claude.ProcessConfig{
			Command: h.claudeCommand,
			PermissionHandler: func(req protocol.PermissionRequest) protocol.PermResponse {
				return s.HandlePermission(req, func(msg ServerMsg) { h.Broadcast(msg) })
			},
			OnRawEvent: func(raw protocol.RawStreamEvent) {
				handleRawStreamEvent(s, &raw, broadcast)
			},
			HookRegistry: h,
			OnHookEvent: func(ev claude.HookEvent) ([]byte, error) {
				return handleHookEvent(s, ev, broadcast, bsSignal)
			},
			ExtraEnv:        bsEnv,
			BashstreamerDir: bsDir,
		})
		if err != nil {
			if bsDir != "" {
				os.RemoveAll(bsDir)
			}
			h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
			return
		}
		s.SetProc(proc)
	}

	// Auto-label from first user message
	s.TryAutoLabel(text)

	// Turn start is signalled by the UserPromptSubmit hook, which fires
	// when proc.SendUserMessage delivers the message. No explicit
	// "running" broadcast is needed.
	h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: text, Images: images})

	go func() {
		if err := proc.SendUserMessage(text, images); err != nil {
			h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
			return
		}
		h.waitAndDrainLoop(s, proc)
	}()
}

// waitAndDrainLoop waits for the current turn to complete, then drains
// any queued messages, sending each as a new turn. Loops until the queue
// is empty or the session is interrupted.
func (h *Hub) waitAndDrainLoop(s *Session, proc claude.Process) {
	for {
		// Stop/StopFailure hook already broadcast "done" during the
		// turn, so the model has already cleared turnActive.
		proc.WaitForTurnDone(context.Background())

		if proc.IsDead() {
			s.CloseAllBgStops()
		}

		interrupted, next := s.DrainQueue()
		if interrupted || next == "" {
			break
		}

		// UserPromptSubmit hook broadcasts "running" when
		// SendUserMessage delivers the queued message.
		h.BroadcastToSession(s.ID, FormatSSE("htmx", RenderQueueBar(s.ID, "")), "", "")
		h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: next})

		if err := proc.SendUserMessage(next, nil); err != nil {
			h.Broadcast(ServerMsg{Type: "error", SessionID: s.ID, Error: err.Error()})
			return
		}
	}
}

// renderContext holds precomputed metadata for rendering a slice of messages.
type renderContext struct {
	lastCompact       int
	toolResults       map[string]ServerMsg            // tool_use id → tool_result message
	toolUseIndexes    map[string]int                  // tool_use id → log index of its tool_use entry
	toolResultIndexes map[string]int                  // tool_use id → log index of its tool_result entry
	bgTasks           map[string]*BgTaskState         // tool_use id → bg task lifecycle state (nil ⇒ not a bg task)
	bgTaskResults     map[string]string               // tool_use id → output file path
	suppressedIDs     map[string]bool                 // tool_use ids for tools whose results are suppressed
	pendingPerms      map[string]ServerMsg            // tool_use id → unresolved inline permission_request
	subagentSections  map[string]*subagentRenderState // agent_id → section state derived from later events
}

// subagentRenderState holds the inner events and link metadata for one
// sub-agent section, gathered during the precompute pass and consumed when
// renderMessages encounters the section's subagent_started entry.
type subagentRenderState struct {
	Section     *SubagentSection
	InnerEvents []ServerMsg
}

// precomputeRenderContext scans the full log to build rendering metadata.
func precomputeRenderContext(log []ServerMsg) renderContext {
	rc := renderContext{
		lastCompact:       -1,
		toolResults:       make(map[string]ServerMsg),
		toolUseIndexes:    make(map[string]int),
		toolResultIndexes: make(map[string]int),
		bgTasks:           deriveBgTasks(log),
		bgTaskResults:     make(map[string]string),
		suppressedIDs:     make(map[string]bool),
		pendingPerms:      make(map[string]ServerMsg),
		subagentSections:  make(map[string]*subagentRenderState),
	}
	for i, msg := range log {
		if msg.Type == "compact_boundary" {
			rc.lastCompact = i
		}
		if msg.Type == "tool_use" && msg.ToolUseID != "" && msg.AgentID == "" {
			rc.toolUseIndexes[msg.ToolUseID] = i
		}
		if msg.Type == "tool_result" && msg.ToolUseID != "" && msg.AgentID == "" {
			rc.toolResults[msg.ToolUseID] = msg
			rc.toolResultIndexes[msg.ToolUseID] = i
			if bgPath := ParseBgTaskPath(msg.Output); bgPath != "" {
				rc.bgTaskResults[msg.ToolUseID] = bgPath
			}
		}
		if msg.Type == "tool_use" && suppressResultTools[msg.Tool] {
			rc.suppressedIDs[msg.ToolUseID] = true
		}
		if msg.Type == "permission_request" && msg.PermTool != "AskUserQuestion" && msg.ToolUseID != "" {
			rc.pendingPerms[msg.ToolUseID] = msg
		}
		if msg.AgentID == "" {
			continue
		}
		st := rc.subagentSections[msg.AgentID]
		if st == nil {
			st = &subagentRenderState{Section: &SubagentSection{AgentID: msg.AgentID}}
			rc.subagentSections[msg.AgentID] = st
		}
		switch msg.Type {
		case "subagent_started":
			st.Section.AgentType = msg.AgentType
		case "subagent_stopped":
			st.Section.Stopped = true
		case "subagent_linked":
			st.Section.Linked = true
			st.Section.ParentToolUseID = msg.ParentToolUseID
			st.Section.Description = msg.Description
			st.Section.FinalText = msg.Text
			st.Section.TotalTokens = msg.TotalTokens
			st.Section.TotalToolUses = msg.TotalToolUses
			st.Section.DurationMs = msg.DurationMs
		case "tool_use", "tool_result":
			st.InnerEvents = append(st.InnerEvents, msg)
		}
	}
	return rc
}

// renderMessages renders messages from log[start:end] into HTML.
func renderMessages(log []ServerMsg, start, end int, rc renderContext, sessionID string) string {
	var b strings.Builder
	if rc.lastCompact >= 0 && start <= rc.lastCompact {
		// We're rendering from inside the compacted region
		if start == 0 {
			b.WriteString(`<div class="compacted-context">`)
		}
	}
	for i := start; i < end; i++ {
		msg := log[i]
		if msg.Type == "compact_boundary" && i == rc.lastCompact {
			b.WriteString(`</div>`)
			b.WriteString(RenderMsg(msg))
			continue
		}
		// Sub-agent inner events (AgentID set on tool_use/tool_result) and
		// section lifecycle updates (subagent_linked/stopped) are folded
		// into the section block rendered at the subagent_started event.
		if msg.AgentID != "" {
			switch msg.Type {
			case "subagent_started":
				st := rc.subagentSections[msg.AgentID]
				if st == nil {
					b.WriteString(RenderSubagentSection(msg.AgentID, msg.AgentType, nil))
				} else {
					b.WriteString(renderFinalSubagentSection(st))
				}
			case "tool_use", "tool_result", "subagent_linked", "subagent_stopped":
				// Folded into the section block above.
			}
			continue
		}
		// Skip tool_results whose tool_use chip lives in the rendered slice:
		// they're nested inside that chip via result-slot injection below.
		if msg.Type == "tool_result" && msg.ToolUseID != "" {
			if useIdx, ok := rc.toolUseIndexes[msg.ToolUseID]; ok && useIdx >= start && useIdx < end {
				continue
			}
		}
		if msg.Type == "tool_result" && rc.suppressedIDs[msg.ToolUseID] {
			if len(msg.Images) == 0 {
				continue
			}
		}
		// Suppress bg task tool_results: their output is loaded lazily.
		if msg.Type == "tool_result" && rc.bgTaskResults[msg.ToolUseID] != "" {
			continue
		}
		// Skip inline permission_request messages: they're rendered inside tool chips.
		if msg.Type == "permission_request" && msg.PermTool != "AskUserQuestion" && msg.ToolUseID != "" {
			continue
		}
		rendered := RenderMsg(msg)
		// Strip spinners for tool_use events that are complete.
		// Background tasks complete at task_done, not at tool_result.
		if msg.Type == "tool_use" {
			if bt, ok := rc.bgTasks[msg.ToolUseID]; ok {
				if bt.Completed {
					rendered = stripSpinner(rendered, msg.ToolUseID)
				}
			} else if _, ok := rc.toolResults[msg.ToolUseID]; ok {
				rendered = stripSpinner(rendered, msg.ToolUseID)
			}
		}
		// Populate the bg-slot with a lazy-load trigger for bg task tool chips
		if msg.Type == "tool_use" && rc.bgTaskResults[msg.ToolUseID] != "" {
			emptySlot := fmt.Sprintf(`<div id="bg-slot-%s"></div>`, Esc(msg.ToolUseID))
			populatedSlot := fmt.Sprintf(`<div id="bg-slot-%s">%s</div>`, Esc(msg.ToolUseID), RenderBgSlot(sessionID, msg.ToolUseID))
			rendered = strings.Replace(rendered, emptySlot, populatedSlot, 1)
		}
		// Populate the result slot with the nested tool_result content.
		// Only inject when the tool_result also lies in [start, end): otherwise
		// the standalone tool_result chip in its own slice would render the
		// same content, producing a duplicate when pagination straddles a pair.
		// Skipped for bg tasks (lazy-loaded into a separate slot) and for
		// suppressed text-only results (Read, TodoWrite, etc.).
		if msg.Type == "tool_use" && msg.ToolUseID != "" && rc.bgTaskResults[msg.ToolUseID] == "" {
			if result, ok := rc.toolResults[msg.ToolUseID]; ok {
				resultIdx, hasIdx := rc.toolResultIndexes[msg.ToolUseID]
				inSlice := hasIdx && resultIdx >= start && resultIdx < end
				if inSlice && (!rc.suppressedIDs[msg.ToolUseID] || len(result.Images) > 0) {
					if inner := RenderToolResultInner(result); inner != "" {
						emptySlot := fmt.Sprintf(`<div class="tool-result-content" id="tool-result-slot-%s"></div>`, Esc(msg.ToolUseID))
						filledSlot := fmt.Sprintf(`<div class="tool-result-content" id="tool-result-slot-%s">%s</div>`, Esc(msg.ToolUseID), inner)
						rendered = strings.Replace(rendered, emptySlot, filledSlot, 1)
					}
				}
			}
		}
		// Populate the perm-slot with inline permission for unresolved permissions
		if msg.Type == "tool_use" && msg.ToolUseID != "" {
			if pm, ok := rc.pendingPerms[msg.ToolUseID]; ok {
				emptySlot := fmt.Sprintf(`<div id="perm-slot-%s"></div>`, Esc(msg.ToolUseID))
				filledSlot := fmt.Sprintf(`<div id="perm-slot-%s">%s</div>`, Esc(msg.ToolUseID), RenderInlinePermission(pm))
				rendered = strings.Replace(rendered, emptySlot, filledSlot, 1)
				// Force the tool chip <details> open so the permission buttons are visible
				rendered = strings.Replace(rendered, `<details class="tool-chip">`, `<details class="tool-chip" open>`, 1)
			}
		}
		b.WriteString(rendered)
	}
	return b.String()
}

// renderSentinel returns an HTML element that triggers loading older messages.
// Scroll position is preserved by the beforeSwap/afterSwap handlers on #messages.
func renderSentinel(sessionID string, beforeIdx int) string {
	return fmt.Sprintf(
		`<div id="load-older" hx-get="/messages/before?session=%s&idx=%d" `+
			`hx-trigger="intersect root:#messages threshold:0" hx-swap="outerHTML">`+
			`<span class="loading-older">Loading older messages...</span></div>`,
		url.QueryEscape(sessionID), beforeIdx)
}
