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

	// Open the tool chip details to trigger lazy-load of bg output
	page.MustElement(`.tool-chip summary`).MustClick()

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
	// Re-open tool chip details after reload to trigger lazy-load
	page.MustElement(`.tool-chip summary`).MustClick()
	time.Sleep(1 * time.Second) // wait for SSE stream to deliver content
	reloadBgText := page.MustEval(`() => document.querySelector('.tool-bg-output').textContent`).String()
	if !strings.Contains(reloadBgText, "step 2") {
		Screenshot(t, page, "bash_bg_reload_missing_output")
		t.Fatalf("after mid-stream reload, expected at least 'step 2' in bg output, got: %s", reloadBgText)
	}
	Screenshot(t, page, "bash_bg_after_reload")

	// Wait for all output to arrive
	WaitForText(t, page, ".tool-bg-output", "step 10", 30*time.Second)

	// Verify no duplicate output (each step should appear exactly once)
	finalBgText := page.MustEval(`() => document.querySelector('.tool-bg-output').textContent`).String()
	if cnt := strings.Count(finalBgText, "step 1\n"); cnt != 1 {
		Screenshot(t, page, "bash_bg_duplicated")
		t.Fatalf("expected 'step 1\\n' exactly once in bg output, got %d:\n%s", cnt, finalBgText)
	}
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

func TestReadImage(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "read_image.jsonl", testMode())

	// Create a minimal 1x1 red PNG (base64) inside the container.
	// This is a valid 67-byte PNG.
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwADhQGAWjR9awAAAABJRU5ErkJggg=="
	if out, err := f.DockerExec("sh", "-c", "echo '"+pngB64+"' | base64 -d > "+containerWorkdir+"/test.png"); err != nil {
		t.Fatalf("create test.png: %v\n%s", err, out)
	}

	page := f.Page()

	// Create session
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	// Ask Claude to read the image
	page.MustElement(`textarea[name="text"]`).MustInput("Read the file test.png and describe what you see")
	page.MustElement(`.send-btn`).MustClick()

	WaitForText(t, page, ".msg-user", "Read the file test.png", 30*time.Second)

	// Wait for the Read tool chip to appear
	WaitForElement(t, page, ".tool-chip", 120*time.Second)
	Screenshot(t, page, "read_image_tool_chip")

	// Wait for the image thumbnail to appear in the messages area.
	// The image comes as a tool_result with an <img> tag using a data: URL.
	WaitForElement(t, page, ".msg-tool .msg-img-thumb", 30*time.Second)
	Screenshot(t, page, "read_image_visible")

	// Verify the image src is a data:image/png URL
	src := page.MustEval(`() => document.querySelector('.msg-tool .msg-img-thumb').src`).String()
	if !strings.HasPrefix(src, "data:image/png;base64,") {
		t.Fatalf("expected data:image/png;base64,... src, got: %.80s...", src)
	}

	// Wait for assistant response (Claude describes the image)
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
	Screenshot(t, page, "read_image_complete")

	// --- Reload and verify image survives replay ---
	currentURL := page.MustEval(`() => window.location.href`).String()
	page.MustNavigate(currentURL).MustWaitStable()
	WaitForElement(t, page, ".msg-tool .msg-img-thumb", 10*time.Second)
	Screenshot(t, page, "read_image_after_reload")

	reloadSrc := page.MustEval(`() => document.querySelector('.msg-tool .msg-img-thumb').src`).String()
	if !strings.HasPrefix(reloadSrc, "data:image/png;base64,") {
		t.Fatalf("after reload, expected data:image/png;base64,... src, got: %.80s...", reloadSrc)
	}
}

// initGitRepo initializes a git repo inside the container at the given path.
func initGitRepo(t *testing.T, f *ContainerFixture, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "init", "-b", "main", dir},
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

func TestQueueDuringStreaming(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "queue_streaming.jsonl", testMode())

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

	// Turn 1: run normally (no pause). Establishes the session.
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput("Read main.go and util.go and tell me what they do")
	page.MustElement(`.send-btn`).MustClick()

	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
	Screenshot(t, page, "queue_turn1_complete")

	// Pause the replayer: all API calls from here will block.
	f.Replayer.Pause()

	// Turn 2: send a message — Claude CLI sends an API call which blocks.
	page.MustElement(`textarea[name="text"]`).MustInput("What functions are defined in each file?")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for the running indicator (proves StartTurn ran and set Running=true).
	WaitForElement(t, page, "#stop-btn button", 10*time.Second)
	Screenshot(t, page, "queue_turn2_paused")

	// Send a third message while Claude is streaming — should be queued.
	page.MustElement(`textarea[name="text"]`).MustInput("Thanks for the help")
	page.MustElement(`.send-btn`).MustClick()

	// Queue bar should appear with the queued text.
	WaitForElement(t, page, ".queue-content", 5*time.Second)
	queueText := page.MustElement(`.queue-preview`).MustText()
	if !strings.Contains(queueText, "Thanks for the help") {
		Screenshot(t, page, "queue_wrong_text")
		t.Fatalf("expected queue to contain 'Thanks for the help', got: %s", queueText)
	}
	Screenshot(t, page, "queue_bar_visible")

	// Unpause: all API calls flow through.
	f.Replayer.Unpause()

	// Turn 2 completes, queue drains, turn 3 starts and completes.
	// Wait for the queued message to appear in chat.
	_, err := page.Timeout(120*time.Second).ElementR(".msg-user", "Thanks for the help")
	if err != nil {
		Screenshot(t, page, "queue_drain_fail")
		t.Fatalf("queued message never appeared in chat: %v", err)
	}

	// Queue bar should be gone.
	queueBarHTML := page.MustEval(`() => document.getElementById('queue-bar').innerHTML`).String()
	if queueBarHTML != "" {
		Screenshot(t, page, "queue_bar_not_cleared")
		t.Fatalf("queue bar should be empty after drain, got: %s", queueBarHTML)
	}

	// Wait for all turns to complete.
	WaitForElement(t, page, "#stop-btn:empty", 120*time.Second)
	Screenshot(t, page, "queue_all_complete")
}

func TestQueueEdit(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "queue_edit.jsonl", testMode())

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

	// Turn 1: run normally.
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput("Read main.go and util.go and tell me what they do")
	page.MustElement(`.send-btn`).MustClick()

	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Pause replayer.
	f.Replayer.Pause()

	// Turn 2: API call blocks.
	page.MustElement(`textarea[name="text"]`).MustInput("What functions are defined in each file?")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, "#stop-btn button", 10*time.Second)

	// Queue a message.
	page.MustElement(`textarea[name="text"]`).MustInput("placeholder text")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, ".queue-content", 5*time.Second)

	// Click Edit — cancels queue on backend, swaps to textarea + Save + ✕.
	page.MustElement(`.queue-btn`).MustClick()
	WaitForElement(t, page, `.queue-text`, 5*time.Second)

	// Edit the queued message.
	queueTA := page.MustElement(`.queue-text`)
	queueTA.MustSelectAllText().MustInput("What about error handling?")
	Screenshot(t, page, "queue_edited")

	// Unpause before clicking Send — so the API calls can flow.
	f.Replayer.Unpause()

	// Click Send — submits /send with the edited text.
	page.MustElement(`.queue-send`).MustClick()

	// The EDITED message (not the original) should appear in chat.
	_, err := page.Timeout(120*time.Second).ElementR(".msg-user", "error handling")
	if err != nil {
		Screenshot(t, page, "queue_edit_drain_fail")
		t.Fatalf("edited queued message never appeared in chat: %v", err)
	}

	// Original placeholder should NOT be in any user message.
	msgs := page.MustElements(".msg-user")
	for _, m := range msgs {
		if strings.Contains(m.MustText(), "placeholder text") {
			Screenshot(t, page, "queue_edit_original_sent")
			t.Fatal("original 'placeholder text' was sent instead of edited text")
		}
	}

	WaitForElement(t, page, "#stop-btn:empty", 120*time.Second)
	Screenshot(t, page, "queue_edit_complete")
}

func TestRebaseWorkstream(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "tool_use.jsonl", testMode())

	// Set up git repo with a main branch.
	initGitRepo(t, f, containerWorkdir)

	// Create a workstream: branch + worktree.
	wsPath := "/root/.monetdroid/worktrees/work/test-rebase"
	for _, args := range [][]string{
		{"git", "-C", containerWorkdir, "branch", "test-rebase"},
		{"git", "-C", containerWorkdir, "worktree", "add", wsPath, "test-rebase"},
		{"git", "-C", wsPath, "branch", "--set-upstream-to", "main", "test-rebase"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Add a commit to main so the workstream is behind.
	for _, args := range [][]string{
		{"git", "-C", containerWorkdir, "commit", "--allow-empty", "-m", "main advance"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Navigate to landing page.
	page := f.Page()
	WaitForElement(t, page, "#ws-panel", 5*time.Second)
	Screenshot(t, page, "rebase_before")

	// Verify the branch shows ↓1.
	WaitForText(t, page, ".ws-behind", "↓1", 5*time.Second)

	// Verify rebase button is present and click it.
	btn := WaitForElement(t, page, ".ws-rebase-btn", 5*time.Second)
	btn.MustClick()

	// Wait for "done" in the output.
	WaitForText(t, page, ".ws-cmd-ok", "done", 10*time.Second)
	Screenshot(t, page, "rebase_done")

	// Click refresh to update the branch list.
	page.MustElement(`button.btn-sm[hx-get*="refresh"]`).MustClick()
	time.Sleep(1 * time.Second)
	Screenshot(t, page, "rebase_refreshed")

	// Branch should now be in sync (no ↓ indicator).
	nodes := page.MustElements(".ws-child")
	for _, node := range nodes {
		text := node.MustElement(".ws-branch-row").MustText()
		if strings.Contains(text, "↓") {
			t.Fatalf("expected branch to be in sync after rebase, got: %s", text)
		}
	}
}

func TestPullMain(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "tool_use.jsonl", testMode())

	// Set up git repo with a remote.
	initGitRepo(t, f, containerWorkdir)

	// Create a bare remote and push to it.
	for _, args := range [][]string{
		{"git", "init", "--bare", "-b", "main", "/tmp/remote.git"},
		{"git", "-C", containerWorkdir, "remote", "add", "origin", "/tmp/remote.git"},
		{"git", "-C", containerWorkdir, "push", "-u", "origin", "main"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Create a workstream so the panel shows up.
	wsPath := "/root/.monetdroid/worktrees/work/test-pull"
	for _, args := range [][]string{
		{"git", "-C", containerWorkdir, "branch", "test-pull"},
		{"git", "-C", containerWorkdir, "worktree", "add", wsPath, "test-pull"},
		{"git", "-C", wsPath, "branch", "--set-upstream-to", "main", "test-pull"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Simulate upstream changes: commit directly to the bare remote.
	for _, args := range [][]string{
		{"git", "clone", "/tmp/remote.git", "/tmp/remote-clone"},
		{"git", "-C", "/tmp/remote-clone", "config", "user.email", "test@test.com"},
		{"git", "-C", "/tmp/remote-clone", "config", "user.name", "Test"},
		{"git", "-C", "/tmp/remote-clone", "commit", "--allow-empty", "-m", "upstream change"},
		{"git", "-C", "/tmp/remote-clone", "push"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Navigate to landing page.
	page := f.Page()
	WaitForElement(t, page, "#ws-panel", 5*time.Second)
	Screenshot(t, page, "pull_before")

	// Click "Pull main".
	page.MustElement(`button.btn-sm[hx-get*="pull-main"]`).MustClick()

	// Wait for the SSE output to show done.
	WaitForText(t, page, ".ws-cmd-ok", "done", 15*time.Second)
	Screenshot(t, page, "pull_done")

	// Verify main advanced — the workstream should now show ↓1.
	page.MustElement(`button.btn-sm[hx-get*="refresh"]`).MustClick()
	time.Sleep(1 * time.Second)
	WaitForText(t, page, ".ws-behind", "↓1", 5*time.Second)
	Screenshot(t, page, "pull_branch_behind")
}

func TestMassSync(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "tool_use.jsonl", testMode())

	initGitRepo(t, f, containerWorkdir)

	// Create two workstreams.
	wsOK := "/root/.monetdroid/worktrees/work/ws-ok"
	wsConflict := "/root/.monetdroid/worktrees/work/ws-conflict"
	for _, args := range [][]string{
		{"git", "-C", containerWorkdir, "branch", "ws-ok"},
		{"git", "-C", containerWorkdir, "worktree", "add", wsOK, "ws-ok"},
		{"git", "-C", wsOK, "branch", "--set-upstream-to", "main", "ws-ok"},
		{"git", "-C", containerWorkdir, "branch", "ws-conflict"},
		{"git", "-C", containerWorkdir, "worktree", "add", wsConflict, "ws-conflict"},
		{"git", "-C", wsConflict, "branch", "--set-upstream-to", "main", "ws-conflict"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Create a file on ws-conflict that will conflict with main.
	for _, args := range [][]string{
		{"sh", "-c", "echo 'conflict line' > " + wsConflict + "/file.txt"},
		{"git", "-C", wsConflict, "add", "file.txt"},
		{"git", "-C", wsConflict, "commit", "-m", "conflicting change"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Advance main with a change to the same file.
	for _, args := range [][]string{
		{"sh", "-c", "echo 'main line' > " + containerWorkdir + "/file.txt"},
		{"git", "-C", containerWorkdir, "add", "file.txt"},
		{"git", "-C", containerWorkdir, "commit", "-m", "main update"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Navigate to landing page.
	page := f.Page()
	WaitForElement(t, page, "#ws-panel", 5*time.Second)
	Screenshot(t, page, "mass_sync_before")

	// Both workstreams should show ↓1.
	behind := page.MustElements(".ws-behind")
	if len(behind) < 2 {
		t.Fatalf("expected at least 2 behind indicators, got %d", len(behind))
	}

	// Click "Sync all".
	syncBtn := WaitForElement(t, page, `button.btn-sm[hx-post*="mass-sync"]`, 5*time.Second)
	syncBtn.MustClick()

	// Wait for both sections to complete — one "done", one "aborted".
	WaitForText(t, page, ".ws-cmd-ok", "done", 10*time.Second)
	WaitForText(t, page, ".ws-cmd-err", "aborted", 10*time.Second)
	Screenshot(t, page, "mass_sync_done")

	// Verify the conflicting worktree is not mid-rebase.
	out, err := f.DockerExec("git", "-C", wsConflict, "status")
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if strings.Contains(out, "rebase in progress") {
		t.Fatalf("expected rebase to be aborted, but rebase is still in progress:\n%s", out)
	}

	// Click refresh and verify the successful workstream is now in sync.
	page.MustElement(`button.btn-sm[hx-get*="refresh"]`).MustClick()
	time.Sleep(1 * time.Second)
	Screenshot(t, page, "mass_sync_refreshed")
}

func TestArchiveWorkstream(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "tool_use.jsonl", testMode())

	// Set up git repo with a workstream.
	initGitRepo(t, f, containerWorkdir)
	wsPath := "/root/.monetdroid/worktrees/work/test-archive"
	for _, args := range [][]string{
		{"git", "-C", containerWorkdir, "branch", "test-archive"},
		{"git", "-C", containerWorkdir, "worktree", "add", wsPath, "test-archive"},
		{"git", "-C", wsPath, "branch", "--set-upstream-to", "main", "test-archive"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Navigate to landing page.
	page := f.Page()
	WaitForElement(t, page, "#ws-panel", 5*time.Second)

	// Verify the workstream is in the active branch list.
	WaitForText(t, page, ".ws-child .ws-branch-name", "test-archive", 5*time.Second)
	Screenshot(t, page, "archive_before")

	// Click archive button.
	page.MustElement(`.ws-archive-btn`).MustClick()

	// Verify it appears in the archived section.
	WaitForText(t, page, ".ws-archived-list .ws-branch-name", "test-archive", 5*time.Second)
	Screenshot(t, page, "archive_after")

	// Verify the workstream is no longer in the active list.
	activeRows := page.MustElements(".ws-child .ws-branch-name")
	for _, row := range activeRows {
		if row.MustText() == "test-archive" {
			t.Fatal("workstream should not be in active list after archiving")
		}
	}

	// Click unarchive.
	page.MustElement(`.ws-archived-list .ws-archive-btn`).MustClick()

	// Verify the workstream is back in the active list.
	WaitForText(t, page, ".ws-child .ws-branch-name", "test-archive", 5*time.Second)
	Screenshot(t, page, "unarchive_after")

	// Verify the archived section is gone (no archived header visible).
	archivedHeaders := page.MustElements(".ws-archived-header")
	if len(archivedHeaders) > 0 {
		t.Fatal("archived section should be gone after unarchiving all")
	}
}

func TestPruneWorkstream(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "tool_use.jsonl", testMode())

	// Set up git repo with a workstream.
	initGitRepo(t, f, containerWorkdir)
	wsPath := "/root/.monetdroid/worktrees/work/test-prune"
	for _, args := range [][]string{
		{"git", "-C", containerWorkdir, "branch", "test-prune"},
		{"git", "-C", containerWorkdir, "worktree", "add", wsPath, "test-prune"},
		{"git", "-C", wsPath, "branch", "--set-upstream-to", "main", "test-prune"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Navigate to landing page, verify workstream visible.
	page := f.Page()
	WaitForElement(t, page, "#ws-panel", 5*time.Second)
	WaitForText(t, page, ".ws-child .ws-branch-name", "test-prune", 5*time.Second)

	// Archive the workstream.
	page.MustElement(`.ws-archive-btn`).MustClick()
	WaitForText(t, page, ".ws-archived-list .ws-branch-name", "test-prune", 5*time.Second)
	Screenshot(t, page, "prune_archived")

	// Click Prune button — should show confirmation.
	btn := WaitForElement(t, page, `button.btn-sm[hx-get="/prune"]`, 5*time.Second)
	btn.MustClick()
	WaitForElement(t, page, ".ws-prune-confirm", 5*time.Second)
	Screenshot(t, page, "prune_confirm")

	// Verify the confirmation shows test-prune as safe to delete (0 ahead).
	WaitForText(t, page, ".ws-prune-safe", "delete", 5*time.Second)

	// Click Confirm prune.
	page.MustElement(`.ws-prune-btn`).MustClick()

	// Wait for prune output.
	WaitForText(t, page, ".ws-cmd-ok", "done", 10*time.Second)
	Screenshot(t, page, "prune_done")

	// Verify the worktree directory is gone.
	out, err := f.DockerExec("test", "-d", wsPath)
	if err == nil {
		t.Fatalf("worktree directory should be deleted, but still exists: %s", out)
	}

	// Verify the branch is deleted.
	out, err = f.DockerExec("git", "-C", containerWorkdir, "branch", "--list", "test-prune")
	if err != nil {
		t.Fatalf("git branch --list: %v\n%s", err, out)
	}
	if strings.Contains(out, "test-prune") {
		t.Fatalf("branch test-prune should be deleted, but still exists: %s", out)
	}

	// Verify the workstream is no longer in the branch list.
	activeRows := page.MustElements(".ws-child .ws-branch-name")
	for _, row := range activeRows {
		if row.MustText() == "test-prune" {
			t.Fatal("workstream should not be in branch list after pruning")
		}
	}
	archivedRows := page.MustElements(".ws-archived-list .ws-branch-name")
	for _, row := range archivedRows {
		if row.MustText() == "test-prune" {
			t.Fatal("workstream should not be in archived list after pruning")
		}
	}
}

func TestBranchTree(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "tool_use.jsonl", testMode())

	initGitRepo(t, f, containerWorkdir)

	// Create the workstream worktree on "feat" branch.
	wsPath := "/root/.monetdroid/worktrees/work/feat"
	for _, args := range [][]string{
		{"git", "-C", containerWorkdir, "branch", "feat"},
		{"git", "-C", containerWorkdir, "worktree", "add", wsPath, "feat"},
		{"git", "-C", wsPath, "branch", "--set-upstream-to", "main", "feat"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Add a commit on feat so it's ahead of main.
	for _, args := range [][]string{
		{"git", "-C", wsPath, "commit", "--allow-empty", "-m", "feat work"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Create child branches with local upstreams forming a forked tree:
	//   feat (depth 0)
	//     feat-ui (depth 1)
	//       feat-ui-tests (depth 2)
	//     feat-api (depth 1)
	//       feat-api-tests (depth 2)
	type childBranch struct {
		name     string
		upstream string
		commits  int // extra commits on top of parent
	}
	children := []childBranch{
		{"feat-ui", "feat", 1},
		{"feat-ui-tests", "feat-ui", 1},
		{"feat-api", "feat", 2},
		{"feat-api-tests", "feat-api", 1},
	}
	for _, ch := range children {
		for _, args := range [][]string{
			{"git", "-C", wsPath, "branch", ch.name, ch.upstream},
			{"git", "-C", wsPath, "config", fmt.Sprintf("branch.%s.remote", ch.name), "."},
			{"git", "-C", wsPath, "config", fmt.Sprintf("branch.%s.merge", ch.name), "refs/heads/" + ch.upstream},
		} {
			if out, err := f.DockerExec(args...); err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
		}
		// Add commits to the child branch.
		for i := 0; i < ch.commits; i++ {
			for _, args := range [][]string{
				{"git", "-C", wsPath, "checkout", ch.name},
				{"git", "-C", wsPath, "commit", "--allow-empty", "-m", fmt.Sprintf("%s commit %d", ch.name, i+1)},
			} {
				if out, err := f.DockerExec(args...); err != nil {
					t.Fatalf("%v: %v\n%s", args, err, out)
				}
			}
		}
	}
	// Switch back to feat (the worktree's primary branch).
	if out, err := f.DockerExec("git", "-C", wsPath, "checkout", "feat"); err != nil {
		t.Fatalf("checkout feat: %v\n%s", err, out)
	}

	// Navigate to landing page.
	page := f.Page()
	WaitForElement(t, page, "#ws-panel", 5*time.Second)
	Screenshot(t, page, "tree_before")

	// Verify all 5 tree nodes appear (feat + 4 children).
	nodes := page.MustElements(".ws-child")
	if len(nodes) != 5 {
		var names []string
		for _, n := range nodes {
			names = append(names, n.MustElement(".ws-branch-name").MustText())
		}
		Screenshot(t, page, "tree_wrong_count")
		t.Fatalf("expected 5 tree nodes, got %d: %v", len(nodes), names)
	}

	// Verify branch names and parent-relative ahead indicators.
	expected := []struct {
		name  string
		ahead string
	}{
		{"feat", "↑1"},
		{"feat-api", "↑2"},
		{"feat-api-tests", "↑1"},
		{"feat-ui", "↑1"},
		{"feat-ui-tests", "↑1"},
	}
	for i, exp := range expected {
		name := nodes[i].MustElement(".ws-branch-name").MustText()
		if name != exp.name {
			Screenshot(t, page, "tree_wrong_name")
			t.Fatalf("row %d: expected name %q, got %q", i, exp.name, name)
		}
		commits := nodes[i].MustElement(".ws-commits").MustText()
		if !strings.Contains(commits, exp.ahead) {
			Screenshot(t, page, "tree_wrong_ahead")
			t.Fatalf("row %d (%s): expected ahead %q, got %q", i, exp.name, exp.ahead, commits)
		}
	}

	// Verify nesting: feat-api and feat-ui should be inside a children
	// container under feat (their parent nodes are nested, not flat).
	nestedChildren := page.MustElements(".ws-child .ws-tree-children .ws-child")
	if len(nestedChildren) < 4 {
		Screenshot(t, page, "tree_not_nested")
		t.Fatalf("expected at least 4 nested child nodes, got %d", len(nestedChildren))
	}
	Screenshot(t, page, "tree_done")
}
