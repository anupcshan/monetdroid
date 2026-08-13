package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		if err := proc.SendUserMessage(text, nil); err != nil {
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
