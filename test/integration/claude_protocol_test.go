package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude"
	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

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
	sessionID, err = proc.WaitForSessionID(ctx)
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

	// Rewind the active target. The contract: the response names the branch
	// point (the target's parent), and the next message attaches there.
	res, err := proc.RewindConversation(target.UUID)
	if err != nil {
		t.Fatalf("rewind active target: %v", err)
	}
	if res.PrecedingAssistantUUID != target.ParentUUID {
		t.Fatalf("precedingAssistantUuid %q != target parent %q",
			res.PrecedingAssistantUUID, target.ParentUUID)
	}
	if !strings.Contains(res.PrefillText, "22222") {
		t.Fatalf("prefillText %q does not contain the target message text", res.PrefillText)
	}

	send("Reply with just the number 33333.")

	// claude writes the transcript asynchronously after the turn completes, so
	// poll until the new message appears before asserting on it.
	messages = waitForUserMessages(ctx, sessionID, len(messages)+1)

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

	// The target is now dormant. Rewinding it again must be rejected: claude
	// only rewinds messages on the active branch.
	if _, err := proc.RewindConversation(target.UUID); err == nil {
		t.Fatal("rewind of a dormant target should be rejected")
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
