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
	t.Parallel()
	f := SetupWithContainer(t, "tool_use.jsonl", testMode())
	page := f.Page()

	WaitForText(t, page, ".empty-state", "Start a new session", 5*time.Second)
	Screenshot(t, page, "empty_state")
}

func TestCreateSession(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "tool_use.jsonl", testMode())
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

func TestMultiTurn(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "multi_turn.jsonl", testMode())

	// Write files for claude to explore
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

	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// First turn: ask about the files
	page.MustElement(`textarea[name="text"]`).MustInput("Read main.go and util.go and tell me what they do")
	page.MustElement(`.send-btn`).MustClick()

	WaitForText(t, page, ".msg-user", "Read main.go", 30*time.Second)
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, ".tool-chip", 10*time.Second)
	Screenshot(t, page, "multi_turn_first_response")

	// Wait for first turn to fully complete (stop button disappears)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Cost bar should show
	WaitForElement(t, page, "#cost-bar:not(:empty)", 10*time.Second)

	// Second turn: follow-up question referencing the first turn's context
	page.MustElement(`textarea[name="text"]`).MustInput("Can main.go use the Add function from util.go? Show me how")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for second user message to render
	_, err := page.Timeout(30*time.Second).ElementR(".msg-user", "Add function")
	if err != nil {
		t.Fatalf("second user message never appeared: %v", err)
	}

	// Wait for second assistant response (it will reference Add/main.go)
	_, err = page.Timeout(120*time.Second).ElementR(".msg-assistant", "Add")
	if err != nil {
		t.Fatalf("second assistant response never appeared: %v", err)
	}
	Screenshot(t, page, "multi_turn_second_response")
}

func TestToolUse(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestAskUserQuestion(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "ask_user.jsonl", testMode())
	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Ask something that triggers AskUserQuestion
	page.MustElement(`textarea[name="text"]`).MustInput("I want to set up a new project. Use the AskUserQuestion tool to ask me what programming language I prefer, with options: Go, Python, Rust, TypeScript")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for the ask-user prompt to appear (radio buttons)
	WaitForElement(t, page, ".ask-user", 60*time.Second)
	WaitForElement(t, page, ".ask-option", 10*time.Second)
	Screenshot(t, page, "ask_user_prompt")

	// Select an option (click the radio button for "Go")
	goOption, err := page.Timeout(5*time.Second).ElementR(".ask-label", "^Go$")
	if err != nil {
		t.Fatalf("could not find Go option: %v", err)
	}
	goOption.MustParent().MustClick()
	time.Sleep(200 * time.Millisecond)
	Screenshot(t, page, "ask_user_selected")

	// Submit the answer
	page.MustElement(".ask-user button[type=submit]").MustClick()

	// Wait for answered summary (form replaced with answer text)
	WaitForElement(t, page, ".ask-answered", 10*time.Second)
	Screenshot(t, page, "ask_user_answered")

	// Wait for assistant response acknowledging the choice
	_, err = page.Timeout(120*time.Second).ElementR(".msg-assistant", "Go")
	if err != nil {
		t.Fatalf("assistant response acknowledging Go never appeared: %v", err)
	}
	Screenshot(t, page, "ask_user_complete")
}

func TestEditDiff(t *testing.T) {
	t.Parallel()
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

func TestAcceptEdits(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "accept_edits.jsonl", testMode())

	// Create files for claude to edit
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

	// Ask claude to edit the file — triggers Edit permission with "Accept Edits" suggestion
	page.MustElement(`textarea[name="text"]`).MustInput("Change the greeting in greeting.go from 'hello world' to 'goodbye world'")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission prompt with Accept Edits button
	WaitForElement(t, page, ".perm-prompt", 60*time.Second)
	Screenshot(t, page, "accept_edits_permission")

	// Click "Accept Edits" instead of plain "Allow"
	acceptBtn, err := page.Timeout(5*time.Second).ElementR("button", "Accept Edits")
	if err != nil {
		t.Fatalf("Accept Edits button not found: %v", err)
	}
	acceptBtn.MustClick()
	time.Sleep(1 * time.Second)

	// Permission should be resolved
	WaitForText(t, page, ".perm-actions", "Allowed", 10*time.Second)
	Screenshot(t, page, "accept_edits_allowed")

	// Wait for first turn to complete
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
	Screenshot(t, page, "accept_edits_first_turn")

	// Second turn: another edit — should NOT require permission now
	page.MustElement(`textarea[name="text"]`).MustInput("Now change 'goodbye world' to 'greetings world' in greeting.go")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for second assistant response — should complete without permission prompt
	_, err = page.Timeout(120*time.Second).ElementR(".msg-assistant", "greetings")
	if err != nil {
		t.Fatalf("second assistant response never appeared: %v", err)
	}
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
	Screenshot(t, page, "accept_edits_second_turn")

	// Verify no second permission prompt appeared (only one .perm-prompt from first turn)
	prompts, err := page.Elements(".perm-prompt")
	if err != nil {
		t.Fatalf("failed to query perm-prompts: %v", err)
	}
	if len(prompts) > 1 {
		Screenshot(t, page, "accept_edits_unexpected_perm")
		t.Fatalf("expected at most 1 permission prompt after Accept Edits, got %d", len(prompts))
	}

	// Verify the file was edited
	content, err := os.ReadFile(filepath.Join(f.WorkDir, "greeting.go"))
	if err != nil {
		t.Fatalf("greeting.go not found: %v", err)
	}
	if !strings.Contains(string(content), "greetings") {
		t.Fatalf("greeting.go should contain 'greetings', got: %s", content)
	}

	// Reset permission mode back to default
	page.MustElement(`.mode-reset`).MustClick()
	time.Sleep(500 * time.Millisecond)
	Screenshot(t, page, "accept_edits_mode_reset")

	// Third turn: another edit — should require permission again after reset
	page.MustElement(`textarea[name="text"]`).MustInput("Now change 'greetings world' to 'howdy world' in greeting.go")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for a new permission prompt with active buttons (first one already shows "Allowed")
	_, err = page.Timeout(60*time.Second).ElementR(".perm-deny", "Deny")
	if err != nil {
		t.Fatalf("third turn permission prompt never appeared: %v", err)
	}
	Screenshot(t, page, "accept_edits_requires_perm_again")

	// Allow the third edit (find the latest Allow button)
	allowBtns, _ := page.Elements(".perm-allow")
	allowBtns[len(allowBtns)-1].MustClick()

	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
	Screenshot(t, page, "accept_edits_third_turn")

	// Verify final edit landed
	content, err = os.ReadFile(filepath.Join(f.WorkDir, "greeting.go"))
	if err != nil {
		t.Fatalf("greeting.go not found: %v", err)
	}
	if !strings.Contains(string(content), "howdy") {
		t.Fatalf("greeting.go should contain 'howdy', got: %s", content)
	}
}

func TestSessionReload(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "multi_turn.jsonl", testMode())

	// Write files (same as TestMultiTurn — needed for tool execution)
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

	page := f.Page()

	// Create session and do two turns (generates enough content to overflow)
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// First turn
	page.MustElement(`textarea[name="text"]`).MustInput("Read main.go and util.go and tell me what they do")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Second turn
	page.MustElement(`textarea[name="text"]`).MustInput("Can main.go use the Add function from util.go? Show me how")
	page.MustElement(`.send-btn`).MustClick()
	if _, err := page.Timeout(120*time.Second).ElementR(".msg-assistant", "Add"); err != nil {
		t.Fatalf("second assistant response never appeared: %v", err)
	}
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Get the session URL
	currentURL := page.MustEval(`() => window.location.href`).String()
	if !strings.Contains(currentURL, "session=") {
		t.Fatalf("URL should contain session=, got: %s", currentURL)
	}

	// Reload with small viewport — multi-turn content should overflow
	page.MustSetViewport(800, 300, 1, false)
	page.MustNavigate(currentURL).MustWaitStable()
	WaitForElement(t, page, ".msg-assistant", 10*time.Second)
	time.Sleep(500 * time.Millisecond)

	scrollTop := page.MustEval(`() => document.getElementById('messages').scrollTop`).Int()
	scrollHeight := page.MustEval(`() => document.getElementById('messages').scrollHeight`).Int()
	clientHeight := page.MustEval(`() => document.getElementById('messages').clientHeight`).Int()
	t.Logf("scroll state: scrollTop=%d scrollHeight=%d clientHeight=%d", scrollTop, scrollHeight, clientHeight)

	// Content should overflow at 300px height
	if scrollHeight <= clientHeight {
		Screenshot(t, page, "session_reload_no_overflow")
		t.Fatalf("expected content to overflow at 300px: scrollHeight=%d clientHeight=%d", scrollHeight, clientHeight)
	}

	// Should be scrolled to bottom, not stuck at top
	if scrollTop == 0 {
		Screenshot(t, page, "session_reload_stuck_at_top")
		t.Fatalf("messages stuck at top: scrollTop=%d scrollHeight=%d clientHeight=%d", scrollTop, scrollHeight, clientHeight)
	}
	Screenshot(t, page, "session_reload_scrolled")
}

func TestBashSpinner(t *testing.T) {
	// Not parallel: uses shared /tmp/claude-0 bind mount for background task output
	// t.Parallel()
	f := SetupWithContainer(t, "bash_spinner.jsonl", testMode())
	page := f.Page()

	// Create session
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	page.MustElement(`#new-session-popover input[name="cwd"]`).MustInput(f.WorkDir)
	page.MustElement(`#new-session-popover .btn-create`).MustClick()
	time.Sleep(500 * time.Millisecond)

	// Background Bash command — tests spinner lifecycle AND streaming output.
	// The sleep between steps gives time to observe partial output.
	page.MustElement(`textarea[name="text"]`).MustInput(
		"Start `for i in $(seq 1 10); do echo step $i; sleep 1; done` in the background and explain what the command does while it runs")
	page.MustElement(`.send-btn`).MustClick()

	// Spinner should appear on the Bash tool chip during execution
	WaitForElement(t, page, ".tool-spinner", 60*time.Second)
	Screenshot(t, page, "bash_spinner_active")

	// Command substitution ($()) triggers a permission prompt — allow it
	WaitForElement(t, page, ".perm-allow", 10*time.Second)
	page.MustElement(`.perm-allow`).MustClick()

	// Wait for turn to complete (Claude responds after submitting the bg task)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Spinner should be removed after completion
	spinners := page.MustEval(`() => document.querySelectorAll('.tool-spinner').length`).Int()
	if spinners != 0 {
		Screenshot(t, page, "bash_spinner_not_removed")
		t.Fatalf("expected 0 spinners after turn complete, got %d", spinners)
	}
	Screenshot(t, page, "bash_spinner_complete")

	// Wait for streaming to start — at least 2 lines visible
	WaitForText(t, page, ".tool-bg-output", "step 2", 30*time.Second)

	// Immediately assert that the last step is NOT yet visible (proves partial streaming)
	bgText := page.MustEval(`() => document.querySelector('.tool-bg-output').textContent`).String()
	if strings.Contains(bgText, "step 10") {
		Screenshot(t, page, "bash_bg_not_partial")
		t.Fatal("step 10 already visible when step 2 first appeared — not partial streaming")
	}

	// Wait for all output to arrive
	WaitForText(t, page, ".tool-bg-output", "step 10", 30*time.Second)
	Screenshot(t, page, "bash_bg_output")

	// Reload — verify spinners are stripped in replay and page stabilises
	currentURL := page.MustEval(`() => window.location.href`).String()
	page.MustNavigate(currentURL).MustWaitStable()
	WaitForElement(t, page, ".msg-assistant", 10*time.Second)

	reloadSpinners := page.MustEval(`() => document.querySelectorAll('.tool-spinner').length`).Int()
	if reloadSpinners != 0 {
		Screenshot(t, page, "bash_spinner_reload_fail")
		t.Fatalf("expected 0 spinners after reload, got %d", reloadSpinners)
	}
	Screenshot(t, page, "bash_spinner_reload")
}

