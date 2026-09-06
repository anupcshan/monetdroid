package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

// branchSessionIDFile is where the in-container branch producer writes the
// session id for the host test to cold-load via /test/read.
const branchSessionIDFile = "/tmp/monetdroid-branch-session-id"

// CLAUDE_PROTOCOL_IN_CONTAINER marks the in-container pass of a Claude Code
// protocol test. The host-side test re-invokes this binary inside the container
// with this env set and -test.run selecting the test. The in-container pass
// drives the real claude process and asserts the protocol contract. TestMain
// routes it to the Go test runner. See TestRewindConversation for the pattern.
const CLAUDE_PROTOCOL_IN_CONTAINER = "CLAUDE_PROTOCOL_IN_CONTAINER"

// TestRewindConversation verifies claude's rewind_conversation control
// request: rewinding an active user message makes the next message a sibling,
// branching at the target's parent, while the target stays in the transcript
// dormant, and rewinding a target that is no longer on the active branch is
// rejected.
//
// The test runs in two passes. On the host it stands up the container and
// re-invokes this binary inside it with -test.run selecting this test. In the
// container it drives the real claude process and asserts.
func TestRewindConversation(t *testing.T) {
	if os.Getenv(CLAUDE_PROTOCOL_IN_CONTAINER) == "1" {
		assertRewindContract(t)
		return
	}

	f := SetupWithContainer(t, AllProviders[0], "rewind.jsonl.zst", testMode())
	cmd := exec.Command("docker", "exec", "-e", CLAUDE_PROTOCOL_IN_CONTAINER+"=1",
		f.containerID, "/test", "-test.run=^TestRewindConversation$", "-test.v")
	out, err := cmd.CombinedOutput()
	t.Logf("rewind protocol test output:\n%s", out)
	if err != nil {
		t.Fatalf("rewind protocol test failed: %v", err)
	}
}

// assertRewindContract drives claude through a rewind and asserts the resulting
// transcript structure. Runs in-container only.
func assertRewindContract(t *testing.T) {
	ensureWorkspaceTrust()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	proc, err := claude.StartProcess(containerWorkdir, func(protocol.StreamEvent) {}, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer proc.Kill()

	var sessionID string
	defer func() {
		if t.Failed() && sessionID != "" {
			dumpTranscript(t, sessionID)
		}
	}()

	sessionID, target, res := rewindAndResend(t, ctx, proc)

	// The contract: the response names the branch point (the target's
	// parent), and the next message attaches there.
	if res.PrecedingAssistantUUID != target.ParentUUID {
		t.Fatalf("precedingAssistantUuid %q != target parent %q",
			res.PrecedingAssistantUUID, target.ParentUUID)
	}
	if !strings.Contains(res.PrefillText, "22222") {
		t.Fatalf("prefillText %q does not contain the target message text", res.PrefillText)
	}

	// claude writes the transcript asynchronously after the turn completes, so
	// poll until the new message appears before asserting on it.
	messages := waitForUserMessages(ctx, sessionID, 3)
	if len(messages) < 3 {
		t.Fatalf("expected at least 3 user messages after rewind, got %d", len(messages))
	}

	// After the rewind the new message must branch as a sibling of the target,
	// and the target must remain in the transcript (dormant, not deleted).
	targetPresent := false
	siblings := 0
	for _, u := range messages {
		if u.UUID == target.UUID {
			targetPresent = true
			continue
		}
		if u.ParentUUID == target.ParentUUID {
			siblings++
		}
	}
	if !targetPresent {
		t.Fatal("rewound target should remain in the transcript")
	}
	if siblings == 0 {
		t.Fatal("new message should branch as a sibling of the target")
	}

	// The target is now dormant. Rewinding it again must be rejected. Claude
	// only rewinds messages on the active branch.
	if _, err := proc.RewindConversation(target.UUID); err == nil {
		t.Fatal("rewind of a dormant target should be rejected")
	}
}

// rewindAndResend drives proc through two turns, rewinds the second turn's
// user message, and sends a replacement. The replacement branches as a
// sibling of the rewound message, so the transcript holds the abandoned
// turn as dormant and the replacement as active. Returns the session id,
// the rewound target entry, and the rewind response.
func rewindAndResend(t *testing.T, ctx context.Context, proc claude.Process) (string, transcriptEntry, claude.RewindResult) {
	t.Helper()

	send := func(text string) {
		t.Helper()
		// An empty uuid lets claude mint its own. The test reads uuids
		// back from the transcript.
		if err := proc.SendUserMessage(text, nil, ""); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		if err := proc.WaitForTurnDone(ctx); err != nil {
			t.Fatalf("turn %q: %v", text, err)
		}
	}

	// The first message triggers claude to emit the session id, so it must
	// precede WaitForSessionID. This mirrors the hub's handleSend ordering.
	send("Reply with just the number 11111.")
	sessionID, err := proc.WaitForSessionID(ctx)
	if err != nil {
		t.Fatalf("session id: %v", err)
	}
	// A second user message to rewind. Its parent is a real assistant turn
	// (the first user message's parent is null), so the response's branch
	// point is comparable.
	send("Reply with just the number 22222.")

	messages := waitForUserMessages(ctx, sessionID, 2)
	if len(messages) < 2 {
		t.Fatalf("expected at least 2 user messages before rewind, got %d", len(messages))
	}
	target := messages[1]

	// Rewind the active target, then resend. The new message branches as a
	// sibling of the target.
	res, err := proc.RewindConversation(target.UUID)
	if err != nil {
		t.Fatalf("rewind active target: %v", err)
	}
	send("Reply with just the number 33333.")
	return sessionID, target, res
}

// waitForTranscriptFlush polls until the transcript holds the user line
// carrying marker followed by its assistant line. claude writes the
// transcript asynchronously after the turn, and the only process shutdown
// is a hard kill, so the producer confirms these lines are on disk before
// it exits. The result summary that follows contributes usage and session
// identity, not rendered messages. Returns whether the flush was
// confirmed before the context expired.
func waitForTranscriptFlush(ctx context.Context, sessionID, marker string) bool {
	for {
		entries := readTranscript(sessionID)
		seenMarker := false
		for _, e := range entries {
			if !seenMarker && e.Type == "user" && strings.Contains(e.Raw, marker) {
				seenMarker = true
				continue
			}
			if seenMarker && e.Type == "assistant" {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TestBranchedSessionColdLoad verifies the user-visible render of a
// branched transcript. The active turn must show and the abandoned turn
// must not. It reuses the rewind cassette and the same prompt sequence
// as TestRewindConversation.
//
// The test runs in two passes. On the host it stands up the container,
// re-invokes this binary inside it with -test.run selecting this test, and
// then drives the browser. In the container the pass prepares the branched
// session and writes the session id to a file for the host to read.
func TestBranchedSessionColdLoad(t *testing.T) {
	if os.Getenv(CLAUDE_PROTOCOL_IN_CONTAINER) == "1" {
		prepareBranchedSession(t)
		return
	}

	f := SetupWithSharedCassette(t, AllProviders[0], "rewind.jsonl.zst", testMode())
	cmd := exec.Command("docker", "exec", "-e", CLAUDE_PROTOCOL_IN_CONTAINER+"=1",
		f.containerID, "/test", "-test.run=^TestBranchedSessionColdLoad$", "-test.v")
	out, err := cmd.CombinedOutput()
	t.Logf("branched-session protocol output:\n%s", out)
	if err != nil {
		t.Fatalf("branched-session protocol pass failed: %v", err)
	}

	// The protocol pass writes the session id after confirming the transcript
	// flush, so the file's presence means the branch is on disk.
	resp, err := http.Get(f.ServerURL + "/test/read?path=" + branchSessionIDFile)
	if err != nil {
		t.Fatalf("read session id: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("read session id: status %d: %s", resp.StatusCode, body)
	}
	sessionID := strings.TrimSpace(string(body))
	if sessionID == "" {
		t.Fatal("protocol pass wrote an empty session id")
	}

	page := f.Page()
	page.MustNavigate(f.ServerURL + "/?session=" + sessionID)

	// Gate the absence check behind the resent message, which renders below
	// the dormant turn's position. Before it appears, absence proves nothing.
	WaitForText(t, page, "body", "33333", 30*time.Second)
	WaitForText(t, page, "body", "11111", 5*time.Second)
	html, err := page.HTML()
	if err != nil {
		t.Fatalf("page HTML: %v", err)
	}
	if strings.Contains(html, "22222") {
		t.Fatal("dormant turn rendered: 22222 is visible")
	}
}

// prepareBranchedSession runs in-container and prepares the branched session
// for the host to cold-load: it starts a claude process, drives the rewind
// and resend, waits for the transcript flush, and writes the session id to
// branchSessionIDFile.
func prepareBranchedSession(t *testing.T) {
	ensureWorkspaceTrust()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	proc, err := claude.StartProcess(containerWorkdir, func(protocol.StreamEvent) {}, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer proc.Kill()

	var sessionID string
	defer func() {
		if t.Failed() && sessionID != "" {
			dumpTranscript(t, sessionID)
		}
	}()

	sessionID, _, _ = rewindAndResend(t, ctx, proc)
	if !waitForTranscriptFlush(ctx, sessionID, "33333") {
		t.Fatal("transcript never flushed the resent turn")
	}

	if err := os.WriteFile(branchSessionIDFile, []byte(sessionID), 0o644); err != nil {
		t.Fatalf("write session id: %v", err)
	}
}

type transcriptEntry struct {
	Type       string          `json:"type"`
	UUID       string          `json:"uuid"`
	ParentUUID string          `json:"parentUuid"`
	Message    json.RawMessage `json:"message"`
	Raw        string
}

// readTranscript locates and parses the JSONL transcript for sessionID. It
// retries briefly while claude first creates the file.
func readTranscript(sessionID string) []transcriptEntry {
	home, _ := os.UserHomeDir()
	var path string
	for range 20 {
		matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
		if len(matches) > 0 {
			path = matches[0]
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []transcriptEntry
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e transcriptEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			e.Raw = line
			entries = append(entries, e)
		}
	}
	return entries
}

// userMessages returns the transcript's user-role entries in file order.
func userMessages(sessionID string) []transcriptEntry {
	var out []transcriptEntry
	for _, e := range readTranscript(sessionID) {
		var m struct {
			Role string `json:"role"`
		}
		json.Unmarshal(e.Message, &m)
		if m.Role == "user" {
			out = append(out, e)
		}
	}
	return out
}

// waitForUserMessages polls the transcript until it holds at least min user
// messages or ctx expires. claude writes the transcript asynchronously after a
// turn, so a single read can race the write.
func waitForUserMessages(ctx context.Context, sessionID string, min int) []transcriptEntry {
	for {
		msgs := userMessages(sessionID)
		if len(msgs) >= min {
			return msgs
		}
		select {
		case <-ctx.Done():
			return msgs
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// dumpTranscript logs the transcript for diagnosing assertion failures.
func dumpTranscript(t *testing.T, sessionID string) {
	entries := readTranscript(sessionID)
	t.Logf("=== transcript %s (%d entries) ===", sessionID, len(entries))
	for i, e := range entries {
		t.Logf("  [%d] %s", i, clip(e.Raw, 600))
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Probes for claude's native command queue over the stream-json control
// protocol: mid-turn sends enqueue server-side (multi-entry), state is
// observable through command_lifecycle frames, entries are removable via
// cancel_async_message, and an interrupt reports a queue receipt.
//
// Determinism comes from parking a turn on an unanswered permission prompt:
// the turn blocks indefinitely, so messages sent after the prompt arrives are
// provably mid-turn. Each probe owns its cassette.

// lifecycleLog records command_lifecycle frames in arrival order.
type lifecycleLog struct {
	mu     sync.Mutex
	events []protocol.CommandLifecycleEvent
	notify chan struct{}
}

func newLifecycleLog() *lifecycleLog {
	return &lifecycleLog{notify: make(chan struct{}, 1)}
}

func (l *lifecycleLog) onEvent(ev protocol.CommandLifecycleEvent) {
	l.mu.Lock()
	l.events = append(l.events, ev)
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// states returns the state sequence observed for uuid, in arrival order.
func (l *lifecycleLog) states(uuid string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ev := range l.events {
		if ev.CommandUUID == uuid {
			out = append(out, ev.State)
		}
	}
	return out
}

// waitForState blocks until uuid has reached state or the timeout expires.
func (l *lifecycleLog) waitForState(t *testing.T, uuid, state string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if slices.Contains(l.states(uuid), state) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("uuid %s never reached state %q; observed %v", uuid, state, l.states(uuid))
		}
		select {
		case <-l.notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// dump logs the full lifecycle sequence for diagnosis.
func (l *lifecycleLog) dump(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Logf("=== command_lifecycle sequence (%d events) ===", len(l.events))
	for i, ev := range l.events {
		t.Logf("  [%d] %s %s", i, clip(ev.CommandUUID, 8), ev.State)
	}
}

// parkedPermissions routes permission requests through channels so a test can
// park a turn by withholding the answer, then release it later.
type parkedPermissions struct {
	requests chan protocol.PermissionRequest
	release  chan protocol.PermResponse
}

func newParkedPermissions() *parkedPermissions {
	return &parkedPermissions{
		requests: make(chan protocol.PermissionRequest, 8),
		release:  make(chan protocol.PermResponse, 8),
	}
}

func (p *parkedPermissions) handler(req protocol.PermissionRequest) protocol.PermResponse {
	p.requests <- req
	return <-p.release
}

// waitForRequest blocks for the next permission request. A turn that has
// reached its permission prompt is parked: no further API call or queue drain
// happens until the test answers.
func (p *parkedPermissions) waitForRequest(t *testing.T) protocol.PermissionRequest {
	t.Helper()
	select {
	case req := <-p.requests:
		return req
	case <-time.After(60 * time.Second):
		t.Fatal("no permission request arrived within 60s")
		return protocol.PermissionRequest{}
	}
}

func (p *parkedPermissions) allow() {
	p.release <- protocol.PermResponse{Allow: true}
}

// startQueueProbe starts a claude process with lifecycle capture and parked
// permissions.
func startQueueProbe(t *testing.T) (*claude.ClaudeProcess, *lifecycleLog, *parkedPermissions) {
	t.Helper()
	ensureWorkspaceTrust()

	life := newLifecycleLog()
	perms := newParkedPermissions()
	proc, err := claude.StartProcessWithConfig(containerWorkdir, func(protocol.StreamEvent) {}, "", &claude.ProcessConfig{
		PermissionHandler:  perms.handler,
		OnCommandLifecycle: life.onEvent,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		life.dump(t)
		proc.Kill()
	})
	return proc, life, perms
}

const (
	queueUUIDA = "aaaaaaaa-0000-4000-8000-00000000000a"
	queueUUIDB = "bbbbbbbb-0000-4000-8000-00000000000b"
	queueUUIDC = "cccccccc-0000-4000-8000-00000000000c"
)

// TestNativeQueue probes the native command queue with a live turn: mid-turn
// sends enqueue server-side (multi-entry), command_lifecycle frames expose each
// entry's state, and cancel_async_message removes a queued entry by uuid.
//
// The test runs in two passes: on the host it stands up the container and
// re-invokes this binary inside it; in the container it drives the real
// claude process and asserts. See TestRewindConversation for the pattern.
func TestNativeQueue(t *testing.T) {
	if os.Getenv(CLAUDE_PROTOCOL_IN_CONTAINER) == "1" {
		assertNativeQueueContract(t)
		return
	}

	f := SetupWithContainer(t, AllProviders[0], "native_queue.jsonl.zst", testMode())
	cmd := exec.Command("docker", "exec", "-e", CLAUDE_PROTOCOL_IN_CONTAINER+"=1",
		f.containerID, "/test", "-test.run=^TestNativeQueue$", "-test.v")
	out, err := cmd.CombinedOutput()
	t.Logf("native queue protocol test output:\n%s", out)
	if err != nil {
		t.Fatalf("native queue protocol test failed: %v", err)
	}
}

func assertNativeQueueContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	proc, life, perms := startQueueProbe(t)

	// Turn A parks on the Write permission prompt.
	if err := proc.SendUserMessage("Create a file called probe_a.txt containing 'turn-a'", nil, queueUUIDA); err != nil {
		t.Fatalf("send A: %v", err)
	}
	req := perms.waitForRequest(t)
	if req.ToolName != "Write" {
		t.Fatalf("expected the Write permission to park turn A, got tool %q", req.ToolName)
	}
	life.waitForState(t, queueUUIDA, "started", 30*time.Second)

	// B and C are sent while turn A is parked, so they are provably mid-turn.
	// The queue holds both at once.
	if err := proc.SendUserMessage("Reply with just the number 11111.", nil, queueUUIDB); err != nil {
		t.Fatalf("send B: %v", err)
	}
	if err := proc.SendUserMessage("Reply with just the number 22222.", nil, queueUUIDC); err != nil {
		t.Fatalf("send C: %v", err)
	}
	life.waitForState(t, queueUUIDB, "queued", 30*time.Second)
	life.waitForState(t, queueUUIDC, "queued", 30*time.Second)

	// Cancelling B removes it from the queue and emits a terminal frame.
	cancelled, err := proc.CancelAsyncMessage(queueUUIDB)
	if err != nil {
		t.Fatalf("cancel B: %v", err)
	}
	if !cancelled {
		t.Fatal("cancel B reported cancelled=false while B was queued")
	}
	life.waitForState(t, queueUUIDB, "cancelled", 30*time.Second)

	// A second cancel of B and a cancel of an unknown uuid both report
	// false: neither is in the queue.
	cancelled, err = proc.CancelAsyncMessage(queueUUIDB)
	if err != nil {
		t.Fatalf("re-cancel B: %v", err)
	}
	if cancelled {
		t.Fatal("re-cancel B reported cancelled=true; B was already removed")
	}
	cancelled, err = proc.CancelAsyncMessage("bbbbbbbb-0000-4000-8000-000000000000")
	if err != nil {
		t.Fatalf("cancel unknown uuid: %v", err)
	}
	if cancelled {
		t.Fatal("cancel of a never-sent uuid reported cancelled=true")
	}

	// Release the permission. Turn A completes, then C drains. Whether C
	// folds into turn A's continuation or runs as its own turn, it must reach
	// a terminal completed state.
	perms.allow()
	if err := proc.WaitForTurnDone(ctx); err != nil {
		t.Fatalf("turn A never completed: %v", err)
	}
	life.waitForState(t, queueUUIDA, "completed", 60*time.Second)
	life.waitForState(t, queueUUIDC, "completed", 60*time.Second)

	// B must never run: its only terminal state is cancelled, and no user
	// message with B's text may exist in the transcript.
	if states := life.states(queueUUIDB); states[len(states)-1] != "cancelled" || len(states) > 2 {
		t.Fatalf("cancelled B must not run; observed states %v", states)
	}
	sessionID, err := proc.WaitForSessionID(ctx)
	if err != nil {
		t.Fatalf("session id: %v", err)
	}
	for _, m := range waitForUserMessages(ctx, sessionID, 2) {
		if strings.Contains(m.Raw, "11111") {
			t.Fatalf("cancelled message B ran anyway; transcript contains its text")
		}
	}
}

// TestNativeQueueInterrupt probes the interrupt queue receipt: a plain
// interrupt lists the uuids that survive under still_queued, and the queued
// messages then drain as a coalesced turn.
//
// The test runs in two passes; see TestRewindConversation for the pattern.
func TestNativeQueueInterrupt(t *testing.T) {
	if os.Getenv(CLAUDE_PROTOCOL_IN_CONTAINER) == "1" {
		assertNativeQueueInterruptContract(t)
		return
	}

	f := SetupWithContainer(t, AllProviders[0], "native_queue_interrupt.jsonl.zst", testMode())
	// The interrupt must arrive after the CLI dispatches the tool
	// permission and before the streamed response finishes. Outside that
	// window the CLI spells the interrupt marker differently, and the
	// changed text breaks the next request's cassette hash. The gap
	// recreates the window during replay.
	f.Replayer.ToolUseStreamGap = 500 * time.Millisecond
	cmd := exec.Command("docker", "exec", "-e", CLAUDE_PROTOCOL_IN_CONTAINER+"=1",
		f.containerID, "/test", "-test.run=^TestNativeQueueInterrupt$", "-test.v")
	out, err := cmd.CombinedOutput()
	t.Logf("native queue interrupt protocol test output:\n%s", out)
	if err != nil {
		t.Fatalf("native queue interrupt protocol test failed: %v", err)
	}
}

func assertNativeQueueInterruptContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	proc, life, perms := startQueueProbe(t)

	// Turn A parks on the Write permission prompt; B and C queue behind it.
	if err := proc.SendUserMessage("Create a file called probe_b.txt containing 'parked'", nil, queueUUIDA); err != nil {
		t.Fatalf("send A: %v", err)
	}
	perms.waitForRequest(t)
	if err := proc.SendUserMessage("Reply with just the number 33333.", nil, queueUUIDB); err != nil {
		t.Fatalf("send B: %v", err)
	}
	if err := proc.SendUserMessage("Reply with just the number 44444.", nil, queueUUIDC); err != nil {
		t.Fatalf("send C: %v", err)
	}
	life.waitForState(t, queueUUIDB, "queued", 30*time.Second)
	life.waitForState(t, queueUUIDC, "queued", 30*time.Second)

	// A plain interrupt aborts turn A and reports the survivors.
	stillQueued, err := proc.Interrupt()
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if !slices.Contains(stillQueued, queueUUIDB) || !slices.Contains(stillQueued, queueUUIDC) {
		t.Fatalf("interrupt still_queued %v does not list both queued uuids", stillQueued)
	}

	// The parked turn is gone; the queued messages drain as a coalesced turn
	// and both reach a terminal completed state.
	life.waitForState(t, queueUUIDB, "completed", 10*time.Second)
	life.waitForState(t, queueUUIDC, "completed", 90*time.Second)
	if err := proc.WaitForTurnDone(ctx); err != nil {
		t.Fatalf("drained turn never completed: %v", err)
	}
}
