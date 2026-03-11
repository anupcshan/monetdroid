package integration

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var record = flag.Bool("record", false, "record new cassettes (requires subscription auth)")

func testMode() string {
	if *record {
		return "record"
	}
	return "replay"
}

func TestEmptyState(t *testing.T) {
	f := SetupWithContainer(t, "simple_turn.jsonl", testMode())
	page := f.Page()

	WaitForText(t, page, ".empty-state", "Start a new session", 5*time.Second)
	Screenshot(t, page, "empty_state")
}

func TestCreateSession(t *testing.T) {
	f := SetupWithContainer(t, "simple_turn.jsonl", testMode())
	page := f.Page()

	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	Screenshot(t, page, "new_session_popover")

	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	WaitForText(t, page, "#session-label", f.WorkDir, 5*time.Second)
	Screenshot(t, page, "session_created")
}

func TestSimpleTurn(t *testing.T) {
	f := SetupWithContainer(t, "simple_turn.jsonl", testMode())
	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Send a simple message
	page.MustElement(`textarea[name="text"]`).MustInput("Say hello in exactly 3 words")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for user message to appear
	WaitForText(t, page, ".msg-user", "Say hello in exactly 3 words", 30*time.Second)

	// Wait for assistant response
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	Screenshot(t, page, "simple_turn_response")

	// Cost bar should show
	WaitForElement(t, page, "#cost-bar:not(:empty)", 10*time.Second)
}

func TestToolUse(t *testing.T) {
	f := SetupWithContainer(t, "tool_use.jsonl", testMode())

	// Write some files into workdir for claude to explore
	os.WriteFile(filepath.Join(f.WorkDir, "main.go"), []byte(`package main

import "fmt"

func main() {
	fmt.Println("hello world")
}
`), 0o644)
	os.WriteFile(filepath.Join(f.WorkDir, "util.go"), []byte(`package main

// Add returns the sum of two integers.
func Add(a, b int) int {
	return a + b
}
`), 0o644)
	os.WriteFile(filepath.Join(f.WorkDir, "config.go"), []byte(`package main

const AppName = "testapp"
const Version = "1.0.0"
`), 0o644)

	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Send a prompt that triggers multiple tool calls
	page.MustElement(`textarea[name="text"]`).MustInput("Read all three Go files and summarize what each one does")
	page.MustElement(`.send-btn`).MustClick()

	WaitForText(t, page, ".msg-user", "Read all three Go files", 30*time.Second)

	// Wait for assistant response (multiple API calls)
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)

	// Should have tool use chips
	WaitForElement(t, page, ".tool-chip", 10*time.Second)
	Screenshot(t, page, "tool_use_response")

	// Cost bar should show
	WaitForElement(t, page, "#cost-bar:not(:empty)", 10*time.Second)
}

func TestPermissionFlow(t *testing.T) {
	f := SetupWithContainer(t, "permission_flow.jsonl", testMode())
	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Ask claude to create a file (triggers Write permission)
	page.MustElement(`textarea[name="text"]`).MustInput("Create a file called hello.txt containing 'Hello, World!'")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission prompt
	WaitForElement(t, page, ".perm-prompt", 60*time.Second)
	WaitForText(t, page, ".perm-tool", "Write", 5*time.Second)
	Screenshot(t, page, "permission_prompt")

	// Click Allow
	page.MustElement(`.perm-allow`).MustClick()
	time.Sleep(1 * time.Second)

	// Permission should be resolved
	WaitForText(t, page, ".perm-actions", "Allowed", 10*time.Second)
	Screenshot(t, page, "permission_allowed")

	// Wait for completion
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	Screenshot(t, page, "permission_turn_complete")

	// Verify URL was updated to include session ID
	currentURL := page.MustEval(`() => window.location.href`).String()
	if !strings.Contains(currentURL, "session=") {
		t.Fatalf("URL should contain session= after turn, got: %s", currentURL)
	}

	// Verify hello.txt was created
	content, err := os.ReadFile(filepath.Join(f.WorkDir, "hello.txt"))
	if err != nil {
		t.Fatalf("hello.txt not created: %v", err)
	}
	if !strings.Contains(string(content), "Hello") {
		t.Fatalf("hello.txt has unexpected content: %s", content)
	}
}

func TestEditDiff(t *testing.T) {
	f := SetupWithContainer(t, "edit_diff.jsonl", testMode())

	// Create a file for claude to edit
	os.WriteFile(filepath.Join(f.WorkDir, "greeting.go"), []byte(`package main

import "fmt"

func main() {
	fmt.Println("hello world")
}
`), 0o644)

	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Ask claude to edit the file
	page.MustElement(`textarea[name="text"]`).MustInput("Change the greeting in greeting.go from 'hello world' to 'goodbye world'")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission with diff
	WaitForElement(t, page, ".perm-prompt", 60*time.Second)
	Screenshot(t, page, "edit_diff_permission")

	// Allow it
	page.MustElement(`.perm-allow`).MustClick()
	time.Sleep(1 * time.Second)

	// Wait for response
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	Screenshot(t, page, "edit_diff_complete")
}

func TestSessionReload(t *testing.T) {
	f := SetupWithContainer(t, "simple_turn.jsonl", testMode())
	page := f.Page()

	// Create session and do a turn
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	page.MustElement(`textarea[name="text"]`).MustInput("Say hello in exactly 3 words")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)

	// Get the session URL
	currentURL := page.MustEval(`() => window.location.href`).String()
	if !strings.Contains(currentURL, "session=") {
		t.Fatalf("URL should contain session=, got: %s", currentURL)
	}

	// Use a small viewport so the session content overflows
	page.MustSetViewport(800, 300, 1, false)
	page.MustNavigate(currentURL).MustWaitStable()
	WaitForElement(t, page, ".msg-assistant", 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	scrollTop := page.MustEval(`() => document.getElementById('messages').scrollTop`).Int()
	scrollHeight := page.MustEval(`() => document.getElementById('messages').scrollHeight`).Int()
	clientHeight := page.MustEval(`() => document.getElementById('messages').clientHeight`).Int()
	t.Logf("scroll state: scrollTop=%d scrollHeight=%d clientHeight=%d", scrollTop, scrollHeight, clientHeight)

	// Empty state should NOT be visible
	emptyVisible := page.MustEval(`() => {
		const el = document.querySelector('.empty-state');
		return el && el.offsetParent !== null;
	}`).Bool()
	if emptyVisible {
		Screenshot(t, page, "session_reload_empty_state_visible")
		t.Fatal("empty state should not be visible when loading a session")
	}

	if scrollHeight > clientHeight && scrollTop == 0 {
		Screenshot(t, page, "session_reload_stuck_at_top")
		t.Fatalf("messages stuck at top: scrollTop=%d scrollHeight=%d clientHeight=%d", scrollTop, scrollHeight, clientHeight)
	}
	Screenshot(t, page, "session_reload_scrolled")
}
