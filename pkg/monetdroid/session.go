package monetdroid

import (
	"context"
	"fmt"
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
	PermissionMode    claude.PermissionMode
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
	BgTaskCommands    map[string]string        // tool_use id → command string
	DiffStat          DiffStat
	EventLog          EventLog
	PermChans         map[string]chan protocol.PermResponse
	AgentDepth        int             // nesting depth of active sub-agents
	AgentToolIDs      map[string]bool // active Agent tool_use IDs (keyed by parent tool_use_id)
	// OutstandingBashID is the tool_use_id of the most recent Bash
	// PreToolUse that has not yet been connected by bashstreamer.
	// BashConnected is closed when bashstreamer connects, unblocking
	// the next PreToolUse.
	OutstandingBashID string
	BashConnected     chan struct{}
	// StreamedBashID is the tool_use_id of a bash command currently
	// being streamed. Set when bashstreamer connects, cleared at
	// tool_result time. At most one active at a time (sequential tools).
	StreamedBashID string
	// BashStreamCmd is the command string for the currently active
	// foreground Bash. Set at PreToolUse, consumed by the SSE handlers
	// to decide whether to route through an extractor.
	BashStreamCmd string
	// BashStreamLines holds the streaming buffer for the currently
	// active foreground Bash. Writes are non-blocking (append to slice);
	// the SSE handler drains from its own sequence number.
	BashStreamLines *BashStreamBuffer
	AgentStats      map[string]*AgentStat // live stats per Agent tool_use ID (from task_progress)
	// AgentDescriptions stashes the parent Agent tool's `description` from
	// PreToolUse, keyed by the parent's tool_use_id. Consumed at parent's
	// PostToolUse for Agent (the only payload that pairs that tool_use_id
	// with the sub-agent's agent_id).
	AgentDescriptions map[string]string
	// SubagentSections tracks sub-agent section state for log replay. Keyed
	// by agent_id (from SubagentStart). Each section is created live and
	// progressively populated. The replay path uses this state to render the
	// section in its final form on initial page load.
	SubagentSections  map[string]*SubagentSection
	StreamingText     string // accumulated text from text_delta events
	StreamingThinking string // accumulated text from thinking_delta events
	Model             *SessionModel
	proc              claude.Process
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
}

// SubagentSection holds the rendered state of a sub-agent section. Fields
// are filled in progressively: AgentID at SubagentStart, the rest at the
// parent's PostToolUse for Agent (link). Stopped flips at SubagentStop.
type SubagentSection struct {
	AgentID         string
	AgentType       string
	Linked          bool
	ParentToolUseID string
	Description     string
	FinalText       string
	TotalTokens     int
	TotalToolUses   int
	DurationMs      int
	Stopped         bool
}

func (s *Session) Append(msg ServerMsg) {
	s.mu.Lock()
	s.Log = append(s.Log, msg)
	s.mu.Unlock()
}

// Close kills the session's claude process if running.
func (s *Session) Close() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
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

func (s *Session) GetLabel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Label
}

func (s *Session) GetPermMode() claude.PermissionMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.PermissionMode
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

func (s *Session) GetProc() claude.Process {
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

func (s *Session) SetProc(proc claude.Process) {
	s.mu.Lock()
	s.proc = proc
	s.mu.Unlock()
}

// AppendStreamingTextAtomically appends a delta and reports whether it was the
// first delta of a new stream. Check and mutation happen under one lock.
func (s *Session) AppendStreamingTextAtomically(delta string) (accumulated string, first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	first = s.StreamingText == ""
	s.StreamingText += delta
	return s.StreamingText, first
}

// AppendStreamingThinkingAtomically appends a thinking delta and reports whether
// it was the first delta of a new stream.
func (s *Session) AppendStreamingThinkingAtomically(delta string) (accumulated string, first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	first = s.StreamingThinking == ""
	s.StreamingThinking += delta
	return s.StreamingThinking, first
}

// ClearStreaming resets the streaming accumulators.
func (s *Session) ClearStreaming() {
	s.mu.Lock()
	s.StreamingText = ""
	s.StreamingThinking = ""
	s.mu.Unlock()
}

// DrainStreaming atomically reads and clears both streaming accumulators.
func (s *Session) DrainStreaming() (text string, thinking string) {
	s.mu.Lock()
	text = s.StreamingText
	thinking = s.StreamingThinking
	s.StreamingText = ""
	s.StreamingThinking = ""
	s.mu.Unlock()
	return
}

func (s *Session) GetDiffStat() DiffStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.DiffStat
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

func (s *Session) SetPermissionMode(mode claude.PermissionMode) {
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

func (s *Session) RegisterBgCommand(id, cmd string) {
	s.mu.Lock()
	if s.BgTaskCommands == nil {
		s.BgTaskCommands = make(map[string]string)
	}
	s.BgTaskCommands[id] = cmd
	s.mu.Unlock()
}

func (s *Session) GetBgCommand(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.BgTaskCommands[id]
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

// maxBashStreamLines is the maximum number of lines kept in the
// in-memory buffer. When exceeded, the oldest half is trimmed.
const maxBashStreamLines = 5000

// maxBashStreamLineBytes caps a single line read from bashstreamer's POST
// body. It matches bashstreamer.maxStreamLineBytes so a line the client
// accepts is never rejected here. A line exceeding it ends the scan and
// is reported rather than truncating the stream silently.
const maxBashStreamLineBytes = 1024 * 1024

// BashStreamBuffer is a bounded in-memory buffer for foreground Bash
// streaming output. Each line gets an implicit sequence number; the
// reader tracks its position via seq. When the buffer exceeds
// maxBashStreamLines, the oldest half is dropped and a truncation note
// is injected on the next reader catch-up. Writes and signals are
// non-blocking.
type BashStreamBuffer struct {
	mu       sync.Mutex
	lines    []string // newest at the end
	startSeq int64    // sequence number of lines[0]; also = total lines dropped
	done     bool
	ch       chan struct{}
}

func NewBashStreamBuffer() *BashStreamBuffer {
	return &BashStreamBuffer{ch: make(chan struct{}, 1)}
}

func (b *BashStreamBuffer) Append(line string) {
	b.mu.Lock()
	b.lines = append(b.lines, line)
	if len(b.lines) > maxBashStreamLines {
		drop := len(b.lines) / 2
		b.startSeq += int64(drop)
		b.lines = b.lines[drop:]
	}
	b.mu.Unlock()
	select {
	case b.ch <- struct{}{}:
	default:
	}
}

func (b *BashStreamBuffer) Close() {
	b.mu.Lock()
	b.done = true
	b.mu.Unlock()
	select {
	case b.ch <- struct{}{}:
	default:
	}
}

// Read returns lines with sequence number > seq. If seq is behind
// startSeq (reader fell behind due to trimming), the first returned
// line is a truncation note. newSeq is the sequence number to pass on
// the next call. done is true when the buffer is closed and all lines
// have been returned.
func (b *BashStreamBuffer) Read(seq int64) (lines []string, newSeq int64, done bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if seq < b.startSeq {
		dropped := b.startSeq - seq
		lines = append(lines, fmt.Sprintf("… [%d lines truncated] …", dropped))
		seq = b.startSeq
	}

	idx := int(seq - b.startSeq)
	if idx < len(b.lines) {
		lines = append(lines, b.lines[idx:]...)
		newSeq = b.startSeq + int64(len(b.lines))
	} else {
		newSeq = seq
	}
	return lines, newSeq, b.done
}

// Wait blocks until new data arrives (or the context is cancelled).
func (b *BashStreamBuffer) Wait(ctx context.Context) bool {
	select {
	case <-b.ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// StoreOutstandingBash records the tool_use_id and command of a Bash
// PreToolUse and creates a buffer that bashstreamer will write lines into.
func (s *Session) StoreOutstandingBash(id, cmd string) {
	s.mu.Lock()
	s.OutstandingBashID = id
	s.BashStreamCmd = cmd
	s.BashConnected = make(chan struct{})
	s.BashStreamLines = NewBashStreamBuffer()
	s.mu.Unlock()
}

// ConsumeOutstandingBash records the outstanding bash tool_use_id as
// StreamedBashID (for later cleanup at tool_result time) and closes
// BashConnected so a waiting PreToolUse can proceed. expected is the
// tool_use_id from the connecting bashstreamer; if it doesn't match the
// outstanding id, nothing is consumed and "" is returned. This guards
// against a stray connection attaching to the wrong buffer.
func (s *Session) ConsumeOutstandingBash(expected string) string {
	s.mu.Lock()
	id := s.OutstandingBashID
	if id == "" || id != expected {
		s.mu.Unlock()
		return ""
	}
	s.OutstandingBashID = ""
	ch := s.BashConnected
	s.BashConnected = nil
	s.StreamedBashID = id
	s.mu.Unlock()
	if ch != nil {
		close(ch)
	}
	return id
}

// WaitForBashConnected blocks until bashstreamer connects (closing the
// channel) or the timeout elapses. Returns true if connected.
func (s *Session) WaitForBashConnected(timeout time.Duration) bool {
	s.mu.Lock()
	ch := s.BashConnected
	s.mu.Unlock()
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

// ConsumeStreamedBash returns true and clears the mark if the given
// tool_use_id was streamed via bashstreamer. Called at tool_result time
// to decide whether to clear the streaming div.
func (s *Session) ConsumeStreamedBash(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.StreamedBashID == id {
		s.StreamedBashID = ""
		s.BashStreamCmd = ""
		s.BashStreamLines = nil
		return true
	}
	return false
}

// BashCmdForTool returns the command for the given Bash tool_use_id if it
// is the currently streaming or outstanding foreground Bash, else "".
// handleBashStreamConnect uses this to pick the extractor layout from the
// same field the SSE handler reads, so the layout and the event stream
// cannot disagree.
func (s *Session) BashCmdForTool(toolID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.StreamedBashID == toolID || s.OutstandingBashID == toolID {
		return s.BashStreamCmd
	}
	return ""
}

// AwaitBashStreamForTool resolves the streaming buffer and command for the
// given Bash tool_use_id. If bashstreamer has already connected, it returns
// immediately. If toolID is still the outstanding Bash (bashstreamer has
// not connected yet), it waits for the connection so an SSE client that
// attaches during the PreToolUse-to-connect window still receives output.
// Returns ok=false if toolID is not the active stream.
func (s *Session) AwaitBashStreamForTool(toolID string, ctx context.Context) (buf *BashStreamBuffer, cmd string, ok bool) {
	s.mu.Lock()
	if s.StreamedBashID == toolID && s.BashStreamLines != nil {
		buf, cmd = s.BashStreamLines, s.BashStreamCmd
		s.mu.Unlock()
		return buf, cmd, true
	}
	wait := s.OutstandingBashID == toolID
	ch := s.BashConnected
	s.mu.Unlock()

	if !wait || ch == nil {
		return nil, "", false
	}

	select {
	case <-ch:
	case <-ctx.Done():
		return nil, "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.StreamedBashID == toolID && s.BashStreamLines != nil {
		return s.BashStreamLines, s.BashStreamCmd, true
	}
	return nil, "", false
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

// StartAgent registers an Agent invocation in the AgentStats table. Called
// from stdout task_started, which carries the parent tool_use_id only. The
// associated sub-agent's agent_id is not yet known.
func (s *Session) StartAgent(toolUseID, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AgentToolIDs == nil {
		s.AgentToolIDs = make(map[string]bool)
	}
	if s.AgentStats == nil {
		s.AgentStats = make(map[string]*AgentStat)
	}
	if s.AgentToolIDs[toolUseID] {
		if description != "" && s.AgentStats[toolUseID] != nil && s.AgentStats[toolUseID].Description == "" {
			s.AgentStats[toolUseID].Description = description
		}
		return
	}
	s.AgentDepth++
	s.AgentToolIDs[toolUseID] = true
	s.AgentStats[toolUseID] = &AgentStat{Description: description}
}

// FinishAgent marks an Agent invocation as completed and decrements depth.
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

// StashAgentDescription stores the parent Agent tool's `description` from
// PreToolUse, keyed by the parent's tool_use_id. Consumed by
// TakeAgentDescription at parent's PostToolUse for Agent.
func (s *Session) StashAgentDescription(parentToolUseID, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.AgentDescriptions == nil {
		s.AgentDescriptions = make(map[string]string)
	}
	s.AgentDescriptions[parentToolUseID] = description
}

// TakeAgentDescription returns and removes the stashed description for a
// parent Agent tool_use_id.
func (s *Session) TakeAgentDescription(parentToolUseID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	desc := s.AgentDescriptions[parentToolUseID]
	delete(s.AgentDescriptions, parentToolUseID)
	return desc
}

// StartSubagent records a sub-agent section by agent_id (from SubagentStart).
// Idempotent on repeated calls with the same agent_id.
func (s *Session) StartSubagent(agentID, agentType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SubagentSections == nil {
		s.SubagentSections = make(map[string]*SubagentSection)
	}
	if _, ok := s.SubagentSections[agentID]; ok {
		return
	}
	s.SubagentSections[agentID] = &SubagentSection{AgentID: agentID, AgentType: agentType}
}

// LinkSubagent fills in the link-time fields on a sub-agent section. The
// hook contract guarantees SubagentStart (which creates the section) fires
// before the parent's PostToolUse for Agent (which triggers this call), so
// the section is always present.
func (s *Session) LinkSubagent(agentID, parentToolUseID, description, finalText string, tokens, toolUses, durationMs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec := s.SubagentSections[agentID]
	sec.Linked = true
	sec.ParentToolUseID = parentToolUseID
	sec.Description = description
	sec.FinalText = finalText
	sec.TotalTokens = tokens
	sec.TotalToolUses = toolUses
	sec.DurationMs = durationMs
}

// MarkSubagentStopped flips the Stopped bit on a sub-agent section.
func (s *Session) MarkSubagentStopped(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sec := s.SubagentSections[agentID]; sec != nil {
		sec.Stopped = true
	}
}

// GetSubagentSection returns a copy of the section state for agent_id.
func (s *Session) GetSubagentSection(agentID string) *SubagentSection {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec := s.SubagentSections[agentID]
	if sec == nil {
		return nil
	}
	cp := *sec
	return &cp
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
	if cost.ModelName != "" {
		s.CostAccum.ModelName = cost.ModelName
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

func (s *Session) InitLive(label string, autoLabel bool, proc claude.Process) {
	s.mu.Lock()
	s.Label = label
	s.AutoLabel = autoLabel
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

func (s *Session) SetPermModeAndGetProc(mode claude.PermissionMode) claude.Process {
	s.mu.Lock()
	s.PermissionMode = mode
	proc := s.proc
	s.mu.Unlock()
	return proc
}

func (s *Session) InterruptAndGetProc() claude.Process {
	s.mu.Lock()
	s.Interrupted = true
	proc := s.proc
	s.mu.Unlock()
	return proc
}

func (s *Session) ResetInterruptAndGetProc() claude.Process {
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
	if s.Model == nil || !s.Model.CanInterrupt() || len(s.PermChans) > 0 {
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
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		ID:                id,
		Cwd:               cwd,
		CreatedAt:         time.Now(),
		SuppressedToolIDs: make(map[string]string),
		BgTaskStops:       make(map[string]chan struct{}),
		PermChans:         make(map[string]chan protocol.PermResponse),
		ctx:               ctx,
		cancel:            cancel,
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
