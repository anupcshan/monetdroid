package integration

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/monetdroid/pkg/monetdroid"
	"github.com/go-rod/rod"
)

var record = flag.Bool("record", false, "record new cassettes (requires subscription auth)")

func testMode() string {
	if *record {
		return "record"
	}
	return "replay"
}

func TestMain(m *testing.M) {
	if os.Getenv("MONETDROID_IN_CONTAINER") == "1" {
		// Inside the container: run the monetdroid server.
		os.MkdirAll(containerWorkdir, 0o755)
		hub := monetdroid.NewHub()
		mux := monetdroid.RegisterRoutes(hub)

		// Test-only endpoints for file I/O (no bind mount needed).
		mux.HandleFunc("/test/write", func(w http.ResponseWriter, r *http.Request) {
			path := r.FormValue("path")
			content := r.FormValue("content")
			dir := filepath.Dir(path)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
		})
		mux.HandleFunc("/test/read", func(w http.ResponseWriter, r *http.Request) {
			data, err := os.ReadFile(r.FormValue("path"))
			if err != nil {
				http.Error(w, err.Error(), 404)
				return
			}
			w.Write(data)
		})

		log.Printf("monetdroid server listening on :8222")
		if err := http.ListenAndServe(":8222", mux); err != nil {
			log.Fatal(err)
		}
		return
	}
	flag.Set("test.parallel", fmt.Sprintf("%d", runtime.NumCPU()/2))
	os.Exit(m.Run())
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

	CreatePlainSession(t, page, containerWorkdir)
	Screenshot(t, page, "new_session_popover")

	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)
	Screenshot(t, page, "session_created")
}

func TestMultiTurn(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "multi_turn.jsonl", testMode())

	// Write files for claude to explore
	f.WriteFile(containerWorkdir+"/main.go", `package main

import "fmt"

func main() {
	fmt.Println("hello world")
}
`)
	f.WriteFile(containerWorkdir+"/util.go", `package main

// Add returns the sum of two integers.
func Add(a, b int) int {
	return a + b
}
`)

	page := f.Page()

	// Create session
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

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
	f.WriteFile(containerWorkdir+"/main.go", `package main

import "fmt"

func main() {
	fmt.Println("hello world")
}
`)
	f.WriteFile(containerWorkdir+"/util.go", `package main

// Add returns the sum of two integers.
func Add(a, b int) int {
	return a + b
}
`)
	f.WriteFile(containerWorkdir+"/config.go", `package main

const AppName = "testapp"
const Version = "1.0.0"
`)

	page := f.Page()

	// Create session
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

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
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	// Ask claude to create a file (triggers Write permission)
	page.MustElement(`textarea[name="text"]`).MustInput("Create a file called hello.txt containing 'Hello, World!'")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission prompt
	WaitForElement(t, page, ".perm-prompt", 60*time.Second)
	WaitForText(t, page, ".perm-tool", "Write", 5*time.Second)
	Screenshot(t, page, "permission_prompt")

	// Click Allow
	page.MustElement(`.perm-allow`).MustClick()

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
	content := f.ReadFile(containerWorkdir + "/hello.txt")
	if !strings.Contains(content, "Hello") {
		t.Fatalf("hello.txt has unexpected content: %s", content)
	}
}

func TestAskUserQuestion(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "ask_user.jsonl", testMode())
	page := f.Page()

	// Create session
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

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
	f.WriteFile(containerWorkdir+"/greeting.go", `package main

import "fmt"

func main() {
	fmt.Println("hello world")
}
`)

	page := f.Page()

	// Create session
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	// Ask claude to edit the file
	page.MustElement(`textarea[name="text"]`).MustInput("Change the greeting in greeting.go from 'hello world' to 'goodbye world'")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission with diff
	WaitForElement(t, page, ".perm-prompt", 60*time.Second)
	Screenshot(t, page, "edit_diff_permission")

	// Allow it
	page.MustElement(`.perm-allow`).MustClick()

	// Wait for response
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	Screenshot(t, page, "edit_diff_complete")
}

func TestAcceptEdits(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "accept_edits.jsonl", testMode())

	// Create files for claude to edit
	f.WriteFile(containerWorkdir+"/greeting.go", `package main

import "fmt"

func main() {
	fmt.Println("hello world")
}
`)

	page := f.Page()

	// Create session
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

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
	content := f.ReadFile(containerWorkdir + "/greeting.go")
	if !strings.Contains(content, "greetings") {
		t.Fatalf("greeting.go should contain 'greetings', got: %s", content)
	}

	// Reset permission mode back to default
	page.MustElement(`.mode-reset`).MustClick()
	page.MustWait(`() => !document.querySelector('.mode-reset')`)
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
	content = f.ReadFile(containerWorkdir + "/greeting.go")
	if !strings.Contains(content, "howdy") {
		t.Fatalf("greeting.go should contain 'howdy', got: %s", content)
	}
}

func TestSessionReload(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "multi_turn.jsonl", testMode())

	// Write files (same as TestMultiTurn — needed for tool execution)
	f.WriteFile(containerWorkdir+"/main.go", `package main

import "fmt"

func main() {
	fmt.Println("hello world")
}
`)
	f.WriteFile(containerWorkdir+"/util.go", `package main

// Add returns the sum of two integers.
func Add(a, b int) int {
	return a + b
}
`)

	page := f.Page()

	// Create session and do two turns (generates enough content to overflow)
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

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
	page.MustWaitStable()

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

func TestDrawer(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "drawer.jsonl", testMode())

	// Two distinct work directories so they appear as separate history groups.
	// Create subdirectories via WriteFile (it creates parent dirs).
	f.WriteFile(containerWorkdir+"/project-alpha/.keep", "")
	f.WriteFile(containerWorkdir+"/project-beta/.keep", "")
	dir1 := containerWorkdir + "/project-alpha"
	dir2 := containerWorkdir + "/project-beta"

	page := f.Page()

	// --- Session 1 ---
	CreatePlainSession(t, page, dir1)

	// Wait for redirect to ?cwd= and session label to render
	WaitForText(t, page, "#session-label", "project-alpha", 5*time.Second)

	// Verify we're on a ?cwd= URL (no session yet)
	url1 := page.MustEval(`() => window.location.href`).String()
	if !strings.Contains(url1, "cwd=") {
		t.Fatalf("expected cwd= in URL before first message, got: %s", url1)
	}

	page.MustElement(`textarea[name="text"]`).MustInput("Say hello")
	page.MustElement(`.send-btn`).MustClick()

	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// URL should now have session= with a ClaudeID
	session1URL := page.MustEval(`() => window.location.href`).String()
	if !strings.Contains(session1URL, "session=") {
		t.Fatalf("expected session= in URL after first message, got: %s", session1URL)
	}
	Screenshot(t, page, "drawer_session1")

	// --- Session 2 ---
	CreatePlainSession(t, page, dir2)

	WaitForText(t, page, "#session-label", "project-beta", 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput("Say goodbye")
	page.MustElement(`.send-btn`).MustClick()

	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
	Screenshot(t, page, "drawer_session2")

	// --- Open drawer and verify contents ---
	page.MustElement(`button[popovertarget="drawer"]`).MustClick()
	WaitForElement(t, page, "#drawer-content .drawer-item", 5*time.Second)
	Screenshot(t, page, "drawer_open")

	// Exactly 2 active sessions (no orphan sN sessions)
	activeItems := page.MustElements("#drawer-content .drawer-item")
	if len(activeItems) != 2 {
		t.Fatalf("expected 2 active sessions in drawer, got %d", len(activeItems))
	}

	// Both session labels should appear
	drawerHTML := page.MustEval(`() => document.getElementById('drawer-content').innerHTML`).String()
	if !strings.Contains(drawerHTML, "Say hello") {
		t.Fatalf("drawer missing 'Say hello' session")
	}
	if !strings.Contains(drawerHTML, "Say goodbye") {
		t.Fatalf("drawer missing 'Say goodbye' session")
	}

	// Both directories should appear (in active sessions section)
	if !strings.Contains(drawerHTML, "project-alpha") {
		t.Fatalf("drawer missing project-alpha directory")
	}
	if !strings.Contains(drawerHTML, "project-beta") {
		t.Fatalf("drawer missing project-beta directory")
	}

	// --- Switch to session 1 via drawer ---
	// Find the drawer item that links to session 1
	session1Link, err := page.Timeout(5*time.Second).ElementR(".drawer-item", "Say hello")
	if err != nil {
		t.Fatalf("could not find session 1 link in drawer: %v", err)
	}
	session1Link.MustClick()

	// Session 1's content should be visible
	WaitForText(t, page, ".msg-user", "Say hello", 10*time.Second)

	// Should be back on session 1's URL
	currentURL := page.MustEval(`() => window.location.href`).String()
	if currentURL != session1URL {
		t.Fatalf("expected to switch back to session 1 URL %s, got %s", session1URL, currentURL)
	}
	Screenshot(t, page, "drawer_switched_back")
}

func TestCloseSession(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "drawer.jsonl", testMode())

	// Create subdirectories via WriteFile (it creates parent dirs).
	f.WriteFile(containerWorkdir+"/project-alpha/.keep", "")
	f.WriteFile(containerWorkdir+"/project-beta/.keep", "")
	dir1 := containerWorkdir + "/project-alpha"
	dir2 := containerWorkdir + "/project-beta"

	page := f.Page()

	// --- Create two sessions ---
	CreatePlainSession(t, page, dir1)
	WaitForText(t, page, "#session-label", "project-alpha", 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput("Say hello")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	CreatePlainSession(t, page, dir2)
	WaitForText(t, page, "#session-label", "project-beta", 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput("Say goodbye")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Currently viewing session 2 (project-beta)

	// --- Close session 1 from drawer (should NOT redirect) ---
	page.MustElement(`button[popovertarget="drawer"]`).MustClick()
	WaitForElement(t, page, "#drawer-content .drawer-item", 5*time.Second)

	// Find the close button for session 1 (the row containing "Say hello")
	row1, err := page.Timeout(5*time.Second).ElementR(".drawer-item-row", "Say hello")
	if err != nil {
		t.Fatalf("could not find session 1 row in drawer: %v", err)
	}
	row1.MustElement(".drawer-close-btn").MustClick()

	// Wait for the row to be removed (drawer stays open for multi-close)
	if err := page.Timeout(10 * time.Second).Wait(rod.Eval(`() => ![...document.querySelectorAll('.drawer-item-row')].some(el => el.textContent.includes('Say hello'))`)); err != nil {
		t.Fatalf("drawer item 'Say hello' was not removed: %v", err)
	}
	Screenshot(t, page, "close_from_drawer")

	// Only 1 active session remaining in the still-open drawer
	activeItems := page.MustElements("#drawer-content .drawer-item")
	if len(activeItems) != 1 {
		t.Fatalf("expected 1 active session after closing from drawer, got %d", len(activeItems))
	}

	// Dismiss the drawer to access the header
	page.MustEval(`() => document.getElementById('drawer').hidePopover()`)

	// --- Close session 2 from header (should redirect to /) ---
	page.MustElement(`#close-btn button`).MustClick()

	// Should redirect to landing page showing tracked sessions
	WaitForText(t, page, ".queue-header", "SESSIONS", 5*time.Second)
	Screenshot(t, page, "close_from_header")
}

func TestBashSpinner(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "bash_spinner.jsonl", testMode())
	page := f.Page()

	// Create session
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

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

	// Spinner should still be present — bg command is still running
	if !page.MustHas(".tool-spinner") {
		Screenshot(t, page, "bash_spinner_gone_too_early")
		t.Fatal("spinner disappeared before bg task completed")
	}
	Screenshot(t, page, "bash_spinner_still_running")

	// Wait for streaming to start — at least 2 lines visible
	WaitForText(t, page, ".tool-bg-output", "step 2", 30*time.Second)

	// Immediately assert that the last step is NOT yet visible (proves partial streaming)
	bgText := page.MustEval(`() => document.querySelector('.tool-bg-output').textContent`).String()
	if strings.Contains(bgText, "step 10") {
		Screenshot(t, page, "bash_bg_not_partial")
		t.Fatal("step 10 already visible when step 2 first appeared — not partial streaming")
	}

	// Reload mid-stream — verify partial bg output survives reload
	Screenshot(t, page, "bash_bg_before_reload")
	currentURL := page.MustEval(`() => window.location.href`).String()
	page.MustNavigate(currentURL).MustWaitStable()
	WaitForElement(t, page, ".tool-bg-output", 10*time.Second)
	reloadBgText := page.MustEval(`() => document.querySelector('.tool-bg-output').textContent`).String()
	if !strings.Contains(reloadBgText, "step 2") {
		Screenshot(t, page, "bash_bg_reload_missing_output")
		t.Fatalf("after mid-stream reload, expected at least 'step 2' in bg output, got: %s", reloadBgText)
	}
	Screenshot(t, page, "bash_bg_after_reload")

	// Wait for all output to arrive
	WaitForText(t, page, ".tool-bg-output", "step 10", 30*time.Second)
	Screenshot(t, page, "bash_bg_output")

	// Spinner should be removed by task_notification — get a fresh reference
	spinner, err := page.Timeout(30 * time.Second).Element(".tool-spinner")
	if err == nil {
		if err := spinner.WaitInvisible(); err != nil {
			Screenshot(t, page, "bash_spinner_not_removed")
			t.Fatalf("spinner still visible after bg task completed: %v", err)
		}
	}
	// If Element returned error, spinner is already gone — that's fine
	Screenshot(t, page, "bash_spinner_complete")

	// Reload — verify spinners are stripped in replay and page stabilises
	currentURL = page.MustEval(`() => window.location.href`).String()
	page.MustNavigate(currentURL).MustWaitStable()
	WaitForElement(t, page, ".msg-assistant", 10*time.Second)

	reloadSpinners := page.MustEval(`() => document.querySelectorAll('.tool-spinner').length`).Int()
	if reloadSpinners != 0 {
		Screenshot(t, page, "bash_spinner_reload_fail")
		t.Fatalf("expected 0 spinners after reload, got %d", reloadSpinners)
	}
	Screenshot(t, page, "bash_spinner_reload")
}

// initGitRepo initializes a git repo inside the container at the given path.
func initGitRepo(t *testing.T, f *ContainerFixture, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "config", "--global", "safe.directory", "*"},
		{"git", "init", dir},
		{"git", "-C", dir, "config", "user.email", "test@test.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
		{"git", "-C", dir, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("initGitRepo %v: %v\n%s", args, err, out)
		}
	}
}

func TestDrawerNewWorkstream(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "drawer.jsonl", testMode())

	initGitRepo(t, f, containerWorkdir)

	page := f.Page()

	// Create a plain session and do a turn so the repo appears in history.
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput("Say hello")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Open drawer — history group should have the repo.
	page.MustElement(`button[popovertarget="drawer"]`).MustClick()
	WaitForElement(t, page, ".history-group", 5*time.Second)
	Screenshot(t, page, "drawer_ws_history")

	// Click "+" on the history group to open the workstream popover.
	page.MustElement(`.new-session-btn`).MustClick()
	WaitForElement(t, page, `.ws-popover input[name="name"]`, 5*time.Second)
	Screenshot(t, page, "drawer_ws_popover")

	// Fill in the workstream name and submit.
	page.MustElement(`.ws-popover input[name="name"]`).MustInput("test-ws")
	page.MustElement(`.ws-popover .btn-create`).MustClick()

	// Should redirect to a session in the worktree with the label.
	WaitForText(t, page, "#session-label", "test-ws", 10*time.Second)
	Screenshot(t, page, "drawer_ws_created")

	// URL should contain the worktree path and label.
	currentURL := page.MustEval(`() => window.location.href`).String()
	if !strings.Contains(currentURL, "label=test-ws") {
		t.Fatalf("URL should contain label=test-ws, got: %s", currentURL)
	}
	if !strings.Contains(currentURL, "worktrees") {
		t.Fatalf("URL should contain worktrees path, got: %s", currentURL)
	}

	// Verify the branch was created inside the container.
	out, err := f.DockerExec("git", "-C", containerWorkdir, "branch", "--list", "test-ws")
	if err != nil {
		t.Fatalf("git branch --list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "test-ws") {
		t.Fatalf("branch test-ws not found, got: %s", out)
	}

	// Do a turn in the workstream so it generates a JSONL file.
	page.MustElement(`textarea[name="text"]`).MustInput("Say hello from workstream")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Open drawer — both sessions should be in the same history group.
	page.MustElement(`button[popovertarget="drawer"]`).MustClick()
	WaitForElement(t, page, ".history-group", 5*time.Second)
	Screenshot(t, page, "drawer_ws_grouped")

	// There should be exactly one history group (worktree merged with main repo).
	groups := page.MustElements(".history-group")
	if len(groups) != 1 {
		t.Fatalf("expected 1 history group, got %d", len(groups))
	}

	// Both tracked sessions should show /work as their path.
	items := page.MustElements(".drawer-item")
	if len(items) != 2 {
		t.Fatalf("expected 2 tracked sessions, got %d", len(items))
	}
	for i, item := range items {
		path := item.MustElement(".di-path").MustText()
		if strings.Contains(path, "worktrees") {
			t.Fatalf("session %d path should show main repo, not worktree: %s", i, path)
		}
		if !strings.Contains(path, "/work") {
			t.Fatalf("session %d path should contain /work, got: %s", i, path)
		}
	}
	// First session (most recent) should be the workstream.
	wsName := items[0].MustElement(".di-name").MustText()
	if !strings.Contains(wsName, "test-ws") {
		t.Fatalf("first tracked session should be test-ws, got: %s", wsName)
	}

	// Navigate to home page (no active session) and verify landing page cards.
	page.MustNavigate(f.ServerURL + "/")
	WaitForElement(t, page, ".queue-item", 10*time.Second)
	Screenshot(t, page, "drawer_ws_landing")

	qiCwds := page.MustElements(".qi-cwd")
	for i, el := range qiCwds {
		cwd := el.MustText()
		if strings.Contains(cwd, "worktrees") {
			t.Fatalf("landing page session %d should show main repo, not worktree: %s", i, cwd)
		}
	}
}
