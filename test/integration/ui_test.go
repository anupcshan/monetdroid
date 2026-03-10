package integration

import (
	"os"
	"strings"
	"testing"
	"time"
)

// init strips non-test flags when the binary is re-exec'd as a mock claude process.
//
// The mock pattern works by re-executing the test binary itself as a subprocess
// (via monetdroid.ClaudeCommand = testBin). When re-exec'd, the binary receives
// claude's CLI flags (-p, --input-format, --output-format, etc.) as os.Args.
// Go's test framework calls flag.Parse() before TestMain, and flag.Parse() calls
// os.Exit(2) on unknown flags — there is no way to make it permissive.
//
// Stripping os.Args here allows flag.Parse() to succeed, so TestMain can then
// detect MOCK_CLAUDE and dispatch to RunMockClaude normally.
func init() {
	if os.Getenv("MOCK_CLAUDE") == "1" {
		os.Args = os.Args[:1]
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("MOCK_CLAUDE") == "1" {
		RunMockClaude()
		return
	}
	os.Exit(m.Run())
}

func TestEmptyState(t *testing.T) {
	f := Setup(t, "simple_turn.jsonl")
	page := f.Page()

	// Should show empty state with "New Session" button
	WaitForText(t, page, ".empty-state", "Start a new session", 5*time.Second)
	Screenshot(t, page, "empty_state")
}

func TestCreateSession(t *testing.T) {
	f := Setup(t, "simple_turn.jsonl")
	page := f.Page()

	// Click the + button to open new session popover
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	Screenshot(t, page, "new_session_popover")

	// Fill in cwd and create
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Session label should update
	WaitForText(t, page, "#session-label", f.WorkDir, 5*time.Second)
	Screenshot(t, page, "session_created")
}

func TestSimpleTurn(t *testing.T) {
	f := Setup(t, "simple_turn.jsonl")
	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Send a message
	page.MustElement(`textarea[name="text"]`).MustInput("what does main.go do?")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for user message to appear
	WaitForText(t, page, ".msg-user", "what does main.go do?", 10*time.Second)
	Screenshot(t, page, "simple_turn_user_msg")

	// Wait for assistant response
	WaitForText(t, page, ".msg-assistant", "simple Go program", 10*time.Second)
	Screenshot(t, page, "simple_turn_response")

	// Tool use should be visible
	WaitForElement(t, page, ".tool-chip", 5*time.Second)
	Screenshot(t, page, "simple_turn_with_tools")

	// Cost bar should show
	WaitForElement(t, page, "#cost-bar:not(:empty)", 5*time.Second)
	Screenshot(t, page, "simple_turn_cost")
}

func TestPermissionFlow(t *testing.T) {
	f := Setup(t, "permission_turn.jsonl")
	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Send message that triggers permission
	page.MustElement(`textarea[name="text"]`).MustInput("create hello.txt")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission prompt
	WaitForElement(t, page, ".perm-prompt", 10*time.Second)
	WaitForText(t, page, ".perm-tool", "Write", 5*time.Second)
	Screenshot(t, page, "permission_prompt")

	// Click Allow
	page.MustElement(`.perm-allow`).MustClick()
	time.Sleep(1 * time.Second)

	// Permission should be resolved
	WaitForText(t, page, ".perm-actions", "Allowed", 5*time.Second)
	Screenshot(t, page, "permission_allowed")

	// Wait for completion
	WaitForText(t, page, ".msg-assistant", "hello.txt", 10*time.Second)
	Screenshot(t, page, "permission_turn_complete")

	// Verify URL was updated to include session ID
	currentURL := page.MustEval(`() => window.location.href`).String()
	t.Logf("URL after turn: %s", currentURL)
	if !strings.Contains(currentURL, "session=") {
		t.Fatalf("URL should contain session= after turn, got: %s", currentURL)
	}

	// Reload and verify permission prompt is gone (resolved permissions should not reappear)
	page.MustReload().MustWaitStable()
	WaitForText(t, page, ".msg-assistant", "hello.txt", 10*time.Second)
	has, _, _ := page.Has(".perm-prompt")
	if has {
		Screenshot(t, page, "permission_prompt_after_reload")
		t.Fatal("permission prompt should not appear after reload")
	}
	Screenshot(t, page, "permission_reload_clean")
}

func TestEditDiff(t *testing.T) {
	f := Setup(t, "edit_turn.jsonl")
	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Send message
	page.MustElement(`textarea[name="text"]`).MustInput("update main.go")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission with diff
	WaitForElement(t, page, ".perm-prompt", 10*time.Second)
	WaitForText(t, page, ".perm-detail", "println", 5*time.Second)
	Screenshot(t, page, "edit_diff_permission")

	// Allow it
	page.MustElement(`.perm-allow`).MustClick()
	time.Sleep(1 * time.Second)

	// Wait for response
	WaitForText(t, page, ".msg-assistant", "fmt.Println", 10*time.Second)
	Screenshot(t, page, "edit_turn_complete")
}
