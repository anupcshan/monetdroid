package integration

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/monetdroid/pkg/kbadmin"
	"github.com/anupcshan/monetdroid/pkg/kbcli"
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

// ensureDummyCredentials writes a dummy Claude subscription credential at the
// well-known path if nothing is already there. Record mode bind-mounts the
// real credentials file before the container starts, so this is a no-op then.
// In replay mode the file is absent and this provides it, keeping the CLI on
// the subscription code path (user:mcp_servers scope → same tools array as
// record mode) so recorded and live request bodies match.
func ensureDummyCredentials() {
	const path = "/root/.claude/.credentials.json"
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Fatalf("ensureDummyCredentials mkdir: %v", err)
	}
	expiresAt := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	creds := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"dummy-replay-access-token","refreshToken":"dummy-replay-refresh-token","expiresAt":%d,"scopes":["user:file_upload","user:inference","user:mcp_servers","user:profile","user:sessions:claude_code"],"subscriptionType":"max","rateLimitTier":"default_claude_max_5x"}}`, expiresAt)
	if err := os.WriteFile(path, []byte(creds), 0o600); err != nil {
		log.Fatalf("ensureDummyCredentials write: %v", err)
	}
}

func TestMain(m *testing.M) {
	switch os.Getenv("KB_CLI_MODE") {
	case "kb":
		if err := kbcli.NewApp().Run(context.Background(), os.Args); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	case "kbadmin":
		if err := kbadmin.NewApp().Run(context.Background(), os.Args); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if os.Getenv("MONETDROID_IN_CONTAINER") == "1" {
		// Inside the container: run the monetdroid server.
		os.MkdirAll(containerWorkdir, 0o755)
		ensureDummyCredentials()
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
		mux.HandleFunc("/test/session-log", func(w http.ResponseWriter, r *http.Request) {
			sessions := hub.Sessions.List()
			if len(sessions) == 0 {
				http.Error(w, "no sessions", 404)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(sessions[0].GetLog())
		})

		log.Printf("monetdroid server listening on :8222")
		if err := http.ListenAndServe(":8222", mux); err != nil {
			log.Fatal(err)
		}
		return
	}
	p := runtime.NumCPU() / 2
	if p > 2 {
		p = 2
	}
	flag.Set("test.parallel", fmt.Sprintf("%d", p))
	os.Exit(m.Run())
}

func TestEmptyState(t *testing.T) {
	t.Parallel()
	f := SetupWithSharedCassette(t, "tool_use.jsonl.zst", testMode())
	page := f.Page()

	WaitForText(t, page, ".empty-state", "No active workstreams", 5*time.Second)
	Screenshot(t, page, "empty_state")
}

func TestCreateSession(t *testing.T) {
	t.Parallel()
	f := SetupWithSharedCassette(t, "tool_use.jsonl.zst", testMode())
	page := f.Page()

	CreatePlainSession(t, page, containerWorkdir)
	Screenshot(t, page, "new_session_popover")

	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)
	Screenshot(t, page, "session_created")
}

// TestClearCommand verifies that /clear redirects to a fresh session.
// The Claude CLI's stream-json mode swallows /clear without actually
// resetting the conversation, so monetdroid intercepts it server-side
// and redirects to /?cwd=<cwd>.
func TestClearCommand(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "clear_command.jsonl.zst", testMode())
	page := f.Page()

	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	// Turn 1: ask an arithmetic question with a distinctive answer.
	page.MustElement(`textarea[name="text"]`).MustInput("What is 347 + 219? Just give me the number, nothing else.")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	url1 := page.MustEval(`() => window.location.href`).String()
	if !strings.Contains(url1, "session=") {
		t.Fatalf("expected session= in URL after first turn, got: %s", url1)
	}
	sessionID1 := url1[strings.Index(url1, "session=")+len("session="):]
	if i := strings.IndexAny(sessionID1, "&#"); i >= 0 {
		sessionID1 = sessionID1[:i]
	}
	Screenshot(t, page, "clear_turn1")

	// Turn 2: /clear should redirect to a fresh session at ?cwd=.
	page.MustElement(`textarea[name="text"]`).MustInput("/clear")
	page.MustElement(`.send-btn`).MustClick()
	if err := page.Timeout(10 * time.Second).Wait(rod.Eval(
		`() => window.location.search.includes('cwd=') && !window.location.search.includes('session=')`,
	)); err != nil {
		t.Fatalf("expected URL to become ?cwd=... after /clear: %v", err)
	}
	Screenshot(t, page, "clear_after_slash")

	// Turn 3: fresh session — ask about the prior arithmetic.
	page.MustElement(`textarea[name="text"]`).MustInput("What was the result of the last addition I asked about?")
	page.MustElement(`.send-btn`).MustClick()
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 120*time.Second)
	Screenshot(t, page, "clear_turn3")

	// Fresh session has no prior context, so the answer should NOT include 566.
	msgs := page.MustElements(".msg-assistant")
	if len(msgs) == 0 {
		t.Fatalf("no assistant messages found")
	}
	lastReply := msgs[len(msgs)-1].MustText()
	if strings.Contains(lastReply, "566") {
		t.Fatalf("after /clear, assistant still remembered prior sum (566): %q", lastReply)
	}

	// The new session should have a different id than the pre-/clear one.
	url3 := page.MustEval(`() => window.location.href`).String()
	if !strings.Contains(url3, "session=") {
		t.Fatalf("expected session= in URL after turn 3, got: %s", url3)
	}
	sessionID3 := url3[strings.Index(url3, "session=")+len("session="):]
	if i := strings.IndexAny(sessionID3, "&#"); i >= 0 {
		sessionID3 = sessionID3[:i]
	}
	if sessionID3 == sessionID1 {
		t.Fatalf("session id did not change after /clear: %s", sessionID1)
	}
}

func TestMultiTurn(t *testing.T) {
	t.Parallel()
	f := SetupWithSharedCassette(t, "multi_turn.jsonl.zst", testMode())

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
	page.MustElement(`textarea[name="text"]`).MustInput("Can main.go call the Add function from util.go? Just explain — don't modify any files.")
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
	f := SetupWithContainer(t, "tool_use.jsonl.zst", testMode())

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
	f := SetupWithContainer(t, "permission_flow.jsonl.zst", testMode())
	page := f.Page()

	// Create session
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	// Ask claude to create a file (triggers Write permission)
	page.MustElement(`textarea[name="text"]`).MustInput("Create a file called hello.txt containing 'Hello, World!'")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission prompt (inline inside tool chip)
	WaitForElement(t, page, ".perm-inline", 60*time.Second)
	Screenshot(t, page, "permission_prompt")

	// Click Allow
	page.MustElement(`.perm-allow`).MustClick()

	// Permission should be resolved — status shows in tool chip summary
	WaitForText(t, page, ".tool-name", "Allowed", 10*time.Second)
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
	f := SetupWithContainer(t, "ask_user.jsonl.zst", testMode())
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
	f := SetupWithContainer(t, "edit_diff.jsonl.zst", testMode())

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

	// Wait for permission with diff (inline inside tool chip)
	WaitForElement(t, page, ".perm-inline", 60*time.Second)
	Screenshot(t, page, "edit_diff_permission")

	// Allow it
	page.MustElement(`.perm-allow`).MustClick()

	// Wait for response
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	Screenshot(t, page, "edit_diff_complete")
}

func TestAcceptEdits(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "accept_edits.jsonl.zst", testMode())

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

	// Wait for permission prompt with Accept Edits button (inline inside tool chip)
	WaitForElement(t, page, ".perm-inline", 60*time.Second)
	Screenshot(t, page, "accept_edits_permission")

	// Click "Accept Edits" instead of plain "Allow"
	acceptBtn, err := page.Timeout(5*time.Second).ElementR("button", "Accept Edits")
	if err != nil {
		t.Fatalf("Accept Edits button not found: %v", err)
	}
	acceptBtn.MustClick()

	// Permission should be resolved — status shows in tool chip summary
	WaitForText(t, page, ".tool-name", "Allowed", 10*time.Second)
	Screenshot(t, page, "accept_edits_allowed")

	// Wait for first turn to complete
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
	Screenshot(t, page, "accept_edits_first_turn")

	// Second turn: another edit. Should NOT require permission now.
	prevAssistants := page.MustEval(`() => document.querySelectorAll('.msg-assistant').length`).Int()
	page.MustElement(`textarea[name="text"]`).MustInput("Now change 'goodbye world' to 'greetings world' in greeting.go")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for turn 2 to start rendering before waiting for it to end.
	// #stop-btn:empty alone matches the leftover empty span between turns
	// (from turn 1's done event), so we first gate on a new .msg-assistant
	// appearing, which only happens after the SSE init event has swapped
	// stop-btn to <button>.
	page.Timeout(60 * time.Second).MustWait(fmt.Sprintf(
		`() => document.querySelectorAll('.msg-assistant').length > %d`, prevAssistants))
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
	Screenshot(t, page, "accept_edits_second_turn")

	// Verify no second permission prompt appeared — after first was resolved,
	// the perm-inline was cleared, so there should be zero .perm-inline elements
	prompts, err := page.Elements(".perm-inline")
	if err != nil {
		t.Fatalf("failed to query perm-inlines: %v", err)
	}
	if len(prompts) > 0 {
		Screenshot(t, page, "accept_edits_unexpected_perm")
		t.Fatalf("expected 0 inline permission prompts after Accept Edits, got %d", len(prompts))
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
	f := SetupWithContainer(t, "multi_turn.jsonl.zst", testMode())

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
	page.MustElement(`textarea[name="text"]`).MustInput("Can main.go call the Add function from util.go? Just explain — don't modify any files.")
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

	// Should be scrolled to bottom (within 10px tolerance)
	distFromBottom := scrollHeight - scrollTop - clientHeight
	if distFromBottom > 10 {
		Screenshot(t, page, "session_reload_not_at_bottom")
		t.Fatalf("not scrolled to bottom: scrollTop=%d scrollHeight=%d clientHeight=%d distFromBottom=%d", scrollTop, scrollHeight, clientHeight, distFromBottom)
	}
	Screenshot(t, page, "session_reload_scrolled")
}

func TestDrawer(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "drawer.jsonl.zst", testMode())

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
	f := SetupWithSharedCassette(t, "drawer.jsonl.zst", testMode())

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
	f := SetupWithContainer(t, "bash_spinner.jsonl.zst", testMode())
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

	// Tool chip details is already open from the permission flow's auto-open,
	// so the bg-slot SSE lazy-load triggers as soon as it's populated.

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
	f := SetupWithContainer(t, "read_image.jsonl.zst", testMode())

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

// initSecondRepo creates a second git repo at /work2 with one workstream,
// so that multi-repo rendering is exercised by every workstream test.
func initSecondRepo(t *testing.T, f *ContainerFixture) {
	t.Helper()
	initGitRepo(t, f, "/work2")
	wsPath := "/root/.monetdroid/worktrees/work2/other-feature"
	for _, args := range [][]string{
		{"git", "-C", "/work2", "branch", "other-feature"},
		{"git", "-C", "/work2", "worktree", "add", wsPath, "other-feature"},
		{"git", "-C", wsPath, "branch", "--set-upstream-to", "main", "other-feature"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("initSecondRepo %v: %v\n%s", args, err, out)
		}
	}
}

func TestDrawerNewWorkstream(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "drawer_new_workstream.jsonl.zst", testMode())

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
	f := SetupWithContainer(t, "queue_streaming.jsonl.zst", testMode())

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
	f := SetupWithContainer(t, "queue_edit.jsonl.zst", testMode())

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

func TestPlanMode(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "plan_mode.jsonl.zst", testMode())

	// Create a file for Claude to plan around.
	f.WriteFile(containerWorkdir+"/main.go", `package main

import "fmt"

func main() {
	fmt.Println("hello world")
}
`)

	page := f.Page()

	// Create session.
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	// Send a prompt that triggers plan mode. Tell Claude not to implement
	// so the turn ends cleanly after ExitPlanMode without further permissions.
	page.MustElement(`textarea[name="text"]`).MustInput(
		"I want to add a fibonacci function to main.go. Enter plan mode, make a brief plan, then call ExitPlanMode with it. Do not ask any questions. Do not use AskUserQuestion. Do not implement without explicit instruction.")
	page.MustElement(`.send-btn`).MustClick()

	WaitForText(t, page, ".msg-user", "fibonacci", 30*time.Second)

	// EnterPlanMode executes without permission. Wait for the ExitPlanMode
	// permission prompt — the CLI sends a can_use_tool with the plan content.
	WaitForElement(t, page, ".perm-inline", 120*time.Second)
	// Scroll the Allow button into view so the screenshot captures the full prompt.
	page.MustElement(`.perm-allow`).MustScrollIntoView()
	Screenshot(t, page, "plan_mode_exit_permission")

	// Verify the ExitPlanMode tool chip exists and is open (perm-inline auto-opens it).
	_, err := page.Timeout(5*time.Second).ElementR(".tool-name", "ExitPlanMode")
	if err != nil {
		t.Fatalf("ExitPlanMode tool chip not found: %v", err)
	}

	// Allow the plan.
	page.MustElement(`.perm-allow`).MustClick()

	// Permission should be resolved.
	WaitForText(t, page, ".tool-name", "Allowed", 10*time.Second)
	Screenshot(t, page, "plan_mode_allowed")

	// Wait for turn to complete.
	WaitForElement(t, page, "#stop-btn:empty", 120*time.Second)
	Screenshot(t, page, "plan_mode_complete")

	// Dump the session log so we can inspect the raw ExitPlanMode wire format.
	msgs := f.SessionLog()
	for _, msg := range msgs {
		if msg.Tool == "ExitPlanMode" || msg.Tool == "EnterPlanMode" {
			raw, _ := json.Marshal(msg)
			t.Logf("WIRE %s: %s", msg.Tool, string(raw))
			if msg.Input != nil && msg.Input.Raw != nil {
				t.Logf("WIRE %s input: %s", msg.Tool, string(msg.Input.Raw))
			}
		}
		if msg.Type == "permission_request" && (msg.PermTool == "ExitPlanMode" || msg.PermTool == "EnterPlanMode") {
			raw, _ := json.Marshal(msg)
			t.Logf("WIRE perm %s: %s", msg.PermTool, string(raw))
			if msg.PermInput != nil && msg.PermInput.Raw != nil {
				t.Logf("WIRE perm %s input: %s", msg.PermTool, string(msg.PermInput.Raw))
			}
		}
	}
}

func TestRebaseWorkstream(t *testing.T) {
	t.Parallel()
	f := SetupWithSharedCassette(t, "tool_use.jsonl.zst", testMode())

	// Set up git repo with a main branch.
	initGitRepo(t, f, containerWorkdir)
	initSecondRepo(t, f)

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
	WaitForElement(t, page, "[id^=\"ws-panel-\"]", 5*time.Second)

	// Verify both repo panels are rendered.
	panels := page.MustElements("[id^=\"ws-panel-\"]")
	if len(panels) != 2 {
		Screenshot(t, page, "rebase_wrong_panel_count")
		t.Fatalf("expected 2 repo panels, got %d", len(panels))
	}
	Screenshot(t, page, "rebase_before")

	// Verify the branch shows ↓1.
	WaitForText(t, page, "#ws-panel-work .ws-behind", "↓1", 5*time.Second)

	// Verify rebase button is present and click it.
	btn := WaitForElement(t, page, "#ws-panel-work .ws-rebase-btn", 5*time.Second)
	btn.MustClick()

	// Wait for "done" in the output.
	WaitForText(t, page, "#ws-panel-work .ws-cmd-ok", "done", 10*time.Second)
	Screenshot(t, page, "rebase_done")

	// Click refresh to update the branch list.
	page.MustElement(`#ws-panel-work button.btn-sm[hx-get*="refresh"]`).MustClick()
	time.Sleep(1 * time.Second)
	Screenshot(t, page, "rebase_refreshed")

	// Branch should now be in sync (no ↓ indicator).
	nodes := page.MustElements("#ws-panel-work .ws-child")
	for _, node := range nodes {
		text := node.MustElement(".ws-branch-row").MustText()
		if strings.Contains(text, "↓") {
			t.Fatalf("expected branch to be in sync after rebase, got: %s", text)
		}
	}
}

func TestPullMain(t *testing.T) {
	t.Parallel()
	f := SetupWithSharedCassette(t, "tool_use.jsonl.zst", testMode())

	// Set up git repo with a remote.
	initGitRepo(t, f, containerWorkdir)
	initSecondRepo(t, f)

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
	WaitForElement(t, page, "[id^=\"ws-panel-\"]", 5*time.Second)
	Screenshot(t, page, "pull_before")

	// Click "Pull main" in the work panel.
	page.MustElement(`#ws-panel-work button.btn-sm[hx-get*="pull-main"]`).MustClick()

	// Wait for the SSE output to show done.
	WaitForText(t, page, "#ws-panel-work .ws-cmd-ok", "done", 15*time.Second)
	Screenshot(t, page, "pull_done")

	// Verify main advanced — the workstream should now show ↓1.
	page.MustElement(`#ws-panel-work button.btn-sm[hx-get*="refresh"]`).MustClick()
	time.Sleep(1 * time.Second)
	WaitForText(t, page, "#ws-panel-work .ws-behind", "↓1", 5*time.Second)
	Screenshot(t, page, "pull_branch_behind")
}

func TestMassSync(t *testing.T) {
	t.Parallel()
	f := SetupWithSharedCassette(t, "tool_use.jsonl.zst", testMode())

	initGitRepo(t, f, containerWorkdir)
	initSecondRepo(t, f)

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
	WaitForElement(t, page, "[id^=\"ws-panel-\"]", 5*time.Second)
	Screenshot(t, page, "mass_sync_before")

	// Both workstreams should show ↓1.
	behind := page.MustElements("#ws-panel-work .ws-behind")
	if len(behind) < 2 {
		t.Fatalf("expected at least 2 behind indicators, got %d", len(behind))
	}

	// Click "Sync all" in the work panel.
	syncBtn := WaitForElement(t, page, `#ws-panel-work button.btn-sm[hx-post*="mass-sync"]`, 5*time.Second)
	syncBtn.MustClick()

	// Wait for both sections to complete — one "done", one "aborted".
	WaitForText(t, page, "#ws-panel-work .ws-cmd-ok", "done", 10*time.Second)
	WaitForText(t, page, "#ws-panel-work .ws-cmd-err", "aborted", 10*time.Second)
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
	page.MustElement(`#ws-panel-work button.btn-sm[hx-get*="refresh"]`).MustClick()
	time.Sleep(1 * time.Second)
	Screenshot(t, page, "mass_sync_refreshed")
}

func TestArchiveWorkstream(t *testing.T) {
	t.Parallel()
	f := SetupWithSharedCassette(t, "tool_use.jsonl.zst", testMode())

	// Set up git repo with a workstream.
	initGitRepo(t, f, containerWorkdir)
	initSecondRepo(t, f)
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
	WaitForElement(t, page, "[id^=\"ws-panel-\"]", 5*time.Second)

	// Verify the workstream is in the active branch list.
	WaitForText(t, page, "#ws-panel-work .ws-child .ws-branch-name", "test-archive", 5*time.Second)
	Screenshot(t, page, "archive_before")

	// Click archive button in the work panel.
	page.MustElement(`#ws-panel-work .ws-archive-btn`).MustClick()

	// Verify it appears in the archived section.
	WaitForText(t, page, "#ws-panel-work .ws-archived-list .ws-branch-name", "test-archive", 5*time.Second)
	Screenshot(t, page, "archive_after")

	// Verify the workstream is no longer in the active list.
	activeRows := page.MustElements("#ws-panel-work .ws-child .ws-branch-name")
	for _, row := range activeRows {
		if row.MustText() == "test-archive" {
			t.Fatal("workstream should not be in active list after archiving")
		}
	}

	// Click unarchive.
	page.MustElement(`#ws-panel-work .ws-archived-list .ws-archive-btn`).MustClick()

	// Verify the workstream is back in the active list.
	WaitForText(t, page, "#ws-panel-work .ws-child .ws-branch-name", "test-archive", 5*time.Second)
	Screenshot(t, page, "unarchive_after")

	// Verify the archived section is gone in the work panel.
	archivedHeaders := page.MustElements("#ws-panel-work .ws-archived-header")
	if len(archivedHeaders) > 0 {
		t.Fatal("archived section should be gone after unarchiving all")
	}
}

func TestPruneWorkstream(t *testing.T) {
	t.Parallel()
	f := SetupWithSharedCassette(t, "tool_use.jsonl.zst", testMode())

	// Set up git repo with a workstream.
	initGitRepo(t, f, containerWorkdir)
	initSecondRepo(t, f)
	wsPath := "/root/.monetdroid/worktrees/work/test-prune"
	work2WsPath := "/root/.monetdroid/worktrees/work2/other-feature"
	for _, args := range [][]string{
		{"git", "-C", containerWorkdir, "branch", "test-prune"},
		{"git", "-C", containerWorkdir, "worktree", "add", wsPath, "test-prune"},
		{"git", "-C", wsPath, "branch", "--set-upstream-to", "main", "test-prune"},
	} {
		if out, err := f.DockerExec(args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// Navigate to landing page, verify both workstreams visible.
	page := f.Page()
	WaitForElement(t, page, "[id^=\"ws-panel-\"]", 5*time.Second)
	WaitForText(t, page, "#ws-panel-work .ws-child .ws-branch-name", "test-prune", 5*time.Second)
	WaitForText(t, page, "#ws-panel-work2 .ws-child .ws-branch-name", "other-feature", 5*time.Second)

	// Archive both repos' workstreams via UI.
	page.MustElement(`#ws-panel-work .ws-archive-btn`).MustClick()
	WaitForText(t, page, "#ws-panel-work .ws-archived-list .ws-branch-name", "test-prune", 5*time.Second)
	page.MustElement(`#ws-panel-work2 .ws-archive-btn`).MustClick()
	WaitForText(t, page, "#ws-panel-work2 .ws-archived-list .ws-branch-name", "other-feature", 5*time.Second)
	Screenshot(t, page, "prune_archived")

	// Click Prune button on the work panel — should only show work repo's workstreams.
	btn := WaitForElement(t, page, `#ws-panel-work button.btn-sm[hx-get^="/prune"]`, 5*time.Second)
	btn.MustClick()
	WaitForElement(t, page, "#ws-panel-work .ws-prune-confirm", 5*time.Second)
	Screenshot(t, page, "prune_confirm")

	// Verify the confirmation shows test-prune as safe to delete (0 ahead).
	WaitForText(t, page, "#ws-panel-work .ws-prune-safe", "delete", 5*time.Second)

	// Verify the confirmation does NOT include the second repo's workstream.
	pruneNames := page.MustElements("#ws-panel-work .ws-prune-ws-name")
	for _, el := range pruneNames {
		if el.MustText() == "other-feature" {
			t.Fatal("prune confirmation should not include workstreams from other repos")
		}
	}

	// Click Confirm prune.
	page.MustElement(`#ws-panel-work .ws-prune-btn`).MustClick()

	// Wait for prune output.
	WaitForText(t, page, "#ws-panel-work .ws-cmd-ok", "done", 10*time.Second)
	Screenshot(t, page, "prune_done")

	// Verify the work repo's worktree directory is gone.
	out, err := f.DockerExec("test", "-d", wsPath)
	if err == nil {
		t.Fatalf("worktree directory should be deleted, but still exists: %s", out)
	}

	// Verify the work repo's branch is deleted.
	out, err = f.DockerExec("git", "-C", containerWorkdir, "branch", "--list", "test-prune")
	if err != nil {
		t.Fatalf("git branch --list: %v\n%s", err, out)
	}
	if strings.Contains(out, "test-prune") {
		t.Fatalf("branch test-prune should be deleted, but still exists: %s", out)
	}

	// Verify the workstream is no longer in the work panel.
	activeRows := page.MustElements("#ws-panel-work .ws-child .ws-branch-name")
	for _, row := range activeRows {
		if row.MustText() == "test-prune" {
			t.Fatal("workstream should not be in branch list after pruning")
		}
	}
	archivedRows := page.MustElements("#ws-panel-work .ws-archived-list .ws-branch-name")
	for _, row := range archivedRows {
		if row.MustText() == "test-prune" {
			t.Fatal("workstream should not be in archived list after pruning")
		}
	}

	// Verify the second repo's workstream was NOT pruned.
	if _, err = f.DockerExec("test", "-d", work2WsPath); err != nil {
		t.Fatalf("work2 worktree should still exist after pruning work repo, but it's gone")
	}
	WaitForText(t, page, "#ws-panel-work2 .ws-archived-list .ws-branch-name", "other-feature", 5*time.Second)
}

func TestBranchTree(t *testing.T) {
	t.Parallel()
	f := SetupWithSharedCassette(t, "tool_use.jsonl.zst", testMode())

	initGitRepo(t, f, containerWorkdir)
	initSecondRepo(t, f)

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
	WaitForElement(t, page, "[id^=\"ws-panel-\"]", 5*time.Second)
	Screenshot(t, page, "tree_before")

	// Verify all 5 tree nodes appear in the work panel (feat + 4 children).
	nodes := page.MustElements("#ws-panel-work .ws-child")
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
	nestedChildren := page.MustElements("#ws-panel-work .ws-child .ws-tree-children .ws-child")
	if len(nestedChildren) < 4 {
		Screenshot(t, page, "tree_not_nested")
		t.Fatalf("expected at least 4 nested child nodes, got %d", len(nestedChildren))
	}
	Screenshot(t, page, "tree_done")
}

func TestAgentSubagent(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "agent_subagent.jsonl.zst", testMode())

	// Create a multi-directory codebase large enough to trigger Agent tool usage.
	files := map[string]string{
		"/go.mod": "module myapp\ngo 1.21\n",
		"/cmd/server/main.go": `package main

import (
	"log"
	"net/http"
	"myapp/internal/api"
	"myapp/internal/db"
	"myapp/internal/middleware"
)

func main() {
	database := db.Connect("postgres://localhost:5432/myapp?sslmode=disable")
	router := api.NewRouter(database)
	handler := middleware.Chain(router, middleware.Logger, middleware.Auth)
	log.Println("starting server on :8080")
	log.Fatal(http.ListenAndServe(":8080", handler))
}
`,
		"/cmd/worker/main.go": `package main

import (
	"log"
	"myapp/internal/db"
	"myapp/internal/jobs"
)

func main() {
	database := db.Connect("postgres://localhost:5432/myapp?sslmode=disable")
	runner := jobs.NewRunner(database)
	log.Println("starting worker")
	runner.Run()
}
`,
		"/internal/api/router.go": `package api

import (
	"database/sql"
	"net/http"
)

func NewRouter(db *sql.DB) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/users", handleUsers(db))
	mux.HandleFunc("/api/users/", handleUserByID(db))
	mux.HandleFunc("/api/posts", handlePosts(db))
	mux.HandleFunc("/api/posts/", handlePostByID(db))
	mux.HandleFunc("/api/comments", handleComments(db))
	mux.HandleFunc("/api/auth/login", handleLogin(db))
	mux.HandleFunc("/api/auth/register", handleRegister(db))
	mux.HandleFunc("/api/auth/refresh", handleRefresh(db))
	mux.HandleFunc("/health", handleHealth)
	return mux
}
`,
		"/internal/api/users.go": `package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
)

func handleUsers(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		query := fmt.Sprintf("SELECT id, name, email FROM users WHERE name = '%s'", name)
		rows, err := db.Query(query)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		var users []map[string]any
		for rows.Next() {
			var id int; var n, email string
			rows.Scan(&id, &n, &email)
			users = append(users, map[string]any{"id": id, "name": n, "email": email})
		}
		json.NewEncoder(w).Encode(users)
	}
}

func handleUserByID(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/api/users/"):]
		row := db.QueryRow("SELECT id, name, email FROM users WHERE id = $1", id)
		var user struct{ ID int; Name, Email string }
		if err := row.Scan(&user.ID, &user.Name, &user.Email); err != nil {
			http.Error(w, "not found", 404)
			return
		}
		json.NewEncoder(w).Encode(user)
	}
}
`,
		"/internal/api/posts.go": `package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

func handlePosts(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query("SELECT id, title, body, user_id FROM posts ORDER BY created_at DESC LIMIT 50")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		var posts []map[string]any
		for rows.Next() {
			var id, userID int; var title, body string
			rows.Scan(&id, &title, &body, &userID)
			posts = append(posts, map[string]any{"id": id, "title": title, "body": body, "user_id": userID})
		}
		json.NewEncoder(w).Encode(posts)
	}
}

func handlePostByID(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/api/posts/"):]
		row := db.QueryRow("SELECT id, title, body, user_id FROM posts WHERE id = $1", id)
		var post struct{ ID, UserID int; Title, Body string }
		if err := row.Scan(&post.ID, &post.Title, &post.Body, &post.UserID); err != nil {
			http.Error(w, "not found", 404)
			return
		}
		json.NewEncoder(w).Encode(post)
	}
}
`,
		"/internal/api/comments.go": `package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

func handleComments(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		postID := r.URL.Query().Get("post_id")
		rows, err := db.Query("SELECT id, body, user_id FROM comments WHERE post_id = $1", postID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()
		var comments []map[string]any
		for rows.Next() {
			var id, userID int; var body string
			rows.Scan(&id, &body, &userID)
			comments = append(comments, map[string]any{"id": id, "body": body, "user_id": userID})
		}
		json.NewEncoder(w).Encode(comments)
	}
}
`,
		"/internal/api/auth.go": `package api

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

var jwtSecret = "supersecretkey123"

func handleLogin(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var creds struct{ Email, Password string }
		json.NewDecoder(r.Body).Decode(&creds)
		hash := md5.Sum([]byte(creds.Password))
		passHash := hex.EncodeToString(hash[:])
		row := db.QueryRow("SELECT id, name FROM users WHERE email = $1 AND password_hash = $2", creds.Email, passHash)
		var id int; var name string
		if err := row.Scan(&id, &name); err != nil {
			http.Error(w, "invalid credentials", 401)
			return
		}
		token := generateJWT(id, name, time.Now().Add(24*time.Hour))
		json.NewEncoder(w).Encode(map[string]string{"token": token})
	}
}

func handleRegister(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct{ Name, Email, Password string }
		json.NewDecoder(r.Body).Decode(&input)
		hash := md5.Sum([]byte(input.Password))
		passHash := hex.EncodeToString(hash[:])
		_, err := db.Exec("INSERT INTO users (name, email, password_hash) VALUES ($1, $2, $3)", input.Name, input.Email, passHash)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(201)
	}
}

func handleRefresh(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(501)
	}
}

func generateJWT(id int, name string, exp time.Time) string {
	return "fake-jwt-token"
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
`,
		"/internal/db/connection.go": `package db

import (
	"database/sql"
	"log"
	_ "github.com/lib/pq"
)

func Connect(dsn string) *sql.DB {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	return db
}
`,
		"/internal/db/migrations.go": `package db

import "database/sql"

func Migrate(db *sql.DB) error {
	_, err := db.Exec(` + "`" + `
		CREATE TABLE IF NOT EXISTS users (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS posts (
			id SERIAL PRIMARY KEY,
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			user_id INT REFERENCES users(id),
			created_at TIMESTAMP DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS comments (
			id SERIAL PRIMARY KEY,
			body TEXT NOT NULL,
			post_id INT REFERENCES posts(id),
			user_id INT REFERENCES users(id),
			created_at TIMESTAMP DEFAULT NOW()
		);
	` + "`" + `)
	return err
}
`,
		"/internal/db/queries.go": `package db

import "database/sql"

func GetUserPosts(db *sql.DB, userID int) ([]map[string]any, error) {
	rows, err := db.Query("SELECT id, title, body FROM posts WHERE user_id = $1", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var posts []map[string]any
	for rows.Next() {
		var id int; var title, body string
		rows.Scan(&id, &title, &body)
		posts = append(posts, map[string]any{"id": id, "title": title, "body": body})
	}
	return posts, nil
}
`,
		"/internal/middleware/auth.go": `package middleware

import (
	"net/http"
	"strings"
)

func Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/auth/") || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		token := r.Header.Get("Authorization")
		if token == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
		// TODO: validate JWT
		next.ServeHTTP(w, r)
	})
}
`,
		"/internal/middleware/logging.go": `package middleware

import (
	"log"
	"net/http"
	"time"
)

func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func Chain(h http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
`,
		"/internal/jobs/runner.go": `package jobs

import (
	"database/sql"
	"log"
	"time"
)

type Runner struct {
	db *sql.DB
}

func NewRunner(db *sql.DB) *Runner {
	return &Runner{db: db}
}

func (r *Runner) Run() {
	for {
		r.processNotifications()
		r.cleanupExpiredSessions()
		time.Sleep(30 * time.Second)
	}
}

func (r *Runner) processNotifications() {
	rows, _ := r.db.Query("SELECT id, user_id, message FROM notifications WHERE sent = false")
	if rows == nil { return }
	defer rows.Close()
	for rows.Next() {
		var id, userID int; var message string
		rows.Scan(&id, &userID, &message)
		log.Printf("sending notification %d to user %d: %s", id, userID, message)
		r.db.Exec("UPDATE notifications SET sent = true WHERE id = $1", id)
	}
}

func (r *Runner) cleanupExpiredSessions() {
	r.db.Exec("DELETE FROM sessions WHERE expires_at < NOW()")
}
`,
		"/internal/models/user.go": `package models

import "time"

type User struct {
	ID           int       ` + "`" + `json:"id"` + "`" + `
	Name         string    ` + "`" + `json:"name"` + "`" + `
	Email        string    ` + "`" + `json:"email"` + "`" + `
	PasswordHash string    ` + "`" + `json:"-"` + "`" + `
	CreatedAt    time.Time ` + "`" + `json:"created_at"` + "`" + `
}
`,
		"/internal/models/post.go": `package models

import "time"

type Post struct {
	ID        int       ` + "`" + `json:"id"` + "`" + `
	Title     string    ` + "`" + `json:"title"` + "`" + `
	Body      string    ` + "`" + `json:"body"` + "`" + `
	UserID    int       ` + "`" + `json:"user_id"` + "`" + `
	CreatedAt time.Time ` + "`" + `json:"created_at"` + "`" + `
}

type Comment struct {
	ID        int       ` + "`" + `json:"id"` + "`" + `
	Body      string    ` + "`" + `json:"body"` + "`" + `
	PostID    int       ` + "`" + `json:"post_id"` + "`" + `
	UserID    int       ` + "`" + `json:"user_id"` + "`" + `
	CreatedAt time.Time ` + "`" + `json:"created_at"` + "`" + `
}
`,
		"/pkg/logger/logger.go": `package logger

import (
	"log"
	"os"
)

var (
	Info  = log.New(os.Stdout, "[INFO] ", log.LstdFlags)
	Error = log.New(os.Stderr, "[ERROR] ", log.LstdFlags|log.Lshortfile)
	Debug = log.New(os.Stdout, "[DEBUG] ", log.LstdFlags|log.Lshortfile)
)
`,
		"/config/config.go": `package config

import "os"

type Config struct {
	DatabaseURL string
	Port        string
	JWTSecret   string
	Debug       bool
}

func Load() Config {
	return Config{
		DatabaseURL: getEnv("DATABASE_URL", "postgres://localhost:5432/myapp"),
		Port:        getEnv("PORT", "8080"),
		JWTSecret:   getEnv("JWT_SECRET", "supersecretkey123"),
		Debug:       getEnv("DEBUG", "") == "1",
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
`,
	}

	// Write files in a deterministic order — record and replay need identical
	// filesystem directory entry layouts so Glob inside the subagents returns
	// the same list. Go map iteration is randomized, which leaks directly
	// into the filesystem's readdir order.
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		f.WriteFile(containerWorkdir+path, files[path])
	}

	// Pin mtimes so any ls-la-style output the model solicits shows the same
	// timestamps between record and replay. Subagents in this test sometimes
	// reach for Bash's `ls -la` despite the "no Bash" prompt instruction.
	// `ls -la /work` includes a `..` entry showing the parent directory's
	// mtime — pin `/` too so that line is stable across runs.
	f.DockerExec("find", containerWorkdir, "-exec", "touch", "-d", "2020-01-01T00:00:00Z", "{}", "+")
	f.DockerExec("touch", "-d", "2020-01-01T00:00:00Z", "/")

	// No git init — this test's subagents scan source files; a .git
	// directory adds non-deterministic Glob results (object creation order
	// varies across runs even with a pinned commit SHA) without contributing
	// anything the test's assertions depend on.

	page := f.Page()

	// Create session
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	Screenshot(t, page, "agent_before_send")

	// Send a prompt that explicitly requests Agent tool usage for parallel investigation
	page.MustElement(`textarea[name="text"]`).MustInput(
		"Launch three parallel agents (Read/Grep/Glob only, no Bash) to investigate: 1) SQL injection vulnerabilities, 2) hardcoded secrets or credentials, 3) HTTP handler input validation. Report combined findings.")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for first tool chip (agent is working)
	WaitForElement(t, page, ".tool-chip", 120*time.Second)
	Screenshot(t, page, "agent_first_tool")

	// Wait for first assistant text
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	Screenshot(t, page, "agent_first_text")

	// Wait for turn to complete
	WaitForElement(t, page, "#stop-btn:empty", 120*time.Second)
	Screenshot(t, page, "agent_complete")

	// Scroll to top and click open the first Agent chip to inspect its detail
	page.MustEval(`() => document.querySelector('#messages').scrollTop = 0`)
	WaitForElement(t, page, ".tool-chip summary", 5*time.Second).MustClick()
	WaitForElement(t, page, ".agent-detail-slot:not(:empty)", 10*time.Second)
	Screenshot(t, page, "agent_detail_open")

	// Dump session event log for analysis
	events := f.SessionLog()
	t.Logf("=== SESSION EVENT LOG (%d events) ===", len(events))
	for i, e := range events {
		line := fmt.Sprintf("[%3d] type=%-20s tool=%-20s toolUseID=%s", i, e.Type, e.Tool, e.ToolUseID)
		if e.Cost != nil {
			line += fmt.Sprintf(" cost={used=%d window=%d usd=%.4f}", e.Cost.ContextUsed, e.Cost.ContextWindow, e.Cost.TotalCostUSD)
		}
		if e.Text != "" {
			text := e.Text
			if len(text) > 80 {
				text = text[:80] + "..."
			}
			line += fmt.Sprintf(" text=%q", text)
		}
		t.Logf("%s", line)
	}

	// --- Assertions ---

	// Agent tool_use events should be present
	agentCount := 0
	for _, e := range events {
		if e.Type == "tool_use" && e.Tool == "Agent" {
			agentCount++
		}
	}
	if agentCount == 0 {
		t.Fatal("expected Agent tool_use events in session log")
	}

	// No sub-agent events should leak into the main stream
	for _, e := range events {
		if e.ParentToolUseID != "" {
			t.Fatalf("sub-agent event leaked into main stream: type=%s tool=%s parent=%s", e.Type, e.Tool, e.ParentToolUseID)
		}
	}

	// Context usage should be monotonically non-decreasing — a drop means
	// a sub-agent's smaller context leaked into the parent's cost bar.
	var lastContextUsed int
	for _, e := range events {
		if e.Type == "cost" && e.Cost != nil && e.Cost.ContextUsed > 0 {
			if e.Cost.ContextUsed < lastContextUsed {
				t.Fatalf("context usage decreased: %d -> %d (sub-agent context leak)", lastContextUsed, e.Cost.ContextUsed)
			}
			lastContextUsed = e.Cost.ContextUsed
		}
	}

	// task_done events should appear for each agent
	taskDoneCount := 0
	for _, e := range events {
		if e.Type == "task_done" {
			taskDoneCount++
		}
	}
	if taskDoneCount != agentCount {
		t.Fatalf("expected %d task_done events (one per agent), got %d", agentCount, taskDoneCount)
	}

	// agent_progress events should be present
	progressCount := 0
	for _, e := range events {
		if e.Type == "agent_progress" {
			progressCount++
		}
	}
	if progressCount == 0 {
		t.Fatal("expected agent_progress events in session log")
	}
}

func TestBashToolForeground(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "bash_foreground.jsonl.zst", testMode())

	// Create a file with >2000 chars so `cat` output exceeds the truncation limit.
	var big strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&big, "line %03d: this is padding content to make the file large enough to exceed the two thousand char truncation limit\n", i)
	}
	f.WriteFile(containerWorkdir+"/bigfile.txt", big.String())

	page := f.Page()

	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput("Run `cat bigfile.txt` using the Bash tool. Do not use Read.")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for tool result and turn completion.
	WaitForElement(t, page, ".tool-result-chip", 60*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Expand the tool result.
	page.MustElement(".tool-result-chip summary").MustClick()
	Screenshot(t, page, "bash_fg_result_expanded")

	// Full output must be present — not truncated.
	resultText := page.MustElement(".tool-result-full").MustText()
	if !strings.Contains(resultText, "line 000") {
		t.Fatalf("bash output missing first line (line 000)")
	}
	if !strings.Contains(resultText, "line 050") {
		t.Fatalf("bash output missing middle line (line 050)")
	}
	if !strings.Contains(resultText, "line 099") {
		t.Fatalf("bash output missing last line (line 099)")
	}

	// Scroll to the end of the result for a visible screenshot.
	page.MustEval(`() => document.querySelector('.tool-result-full').scrollIntoView({block: 'end'})`)
	Screenshot(t, page, "bash_fg_result_end")
}

func TestKBWebView(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "kb_web_view.jsonl.zst", testMode())

	// KB needs a git repo (for git-common-dir resolution) with at least one commit (for git grep).
	f.DockerExec("git", "init", containerWorkdir)
	f.DockerExec("git", "-C", containerWorkdir, "commit", "--allow-empty", "-m", "init")
	f.KBWithStdin(containerWorkdir, "# Test KB\n\n- [Details](details.md)\n- [External](https://example.com)\n", "write", "index.md")
	f.KBWithStdin(containerWorkdir, "# Details Page\n\nSome details here.\n", "write", "details.md")

	// Start a session and send a message so the cost bar populates (includes KB link).
	page := f.Page()
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)
	page.MustElement(`textarea[name="text"]`).MustInput("hello")
	page.MustElement(`.send-btn`).MustClick()

	kbLink := WaitForElement(t, page, `a[href*="/kb/"]`, 30*time.Second)
	kbLink.MustClick()
	page.MustWaitStable()

	WaitForText(t, page, ".kb-content", "Test KB", 5*time.Second)
	Screenshot(t, page, "kb_index")

	// Click an internal link — should navigate within the KB.
	detailsLink := WaitForElement(t, page, `.kb-content a[href*="details.md"]`, 5*time.Second)
	detailsLink.MustClick()
	page.MustWaitStable()

	WaitForText(t, page, ".kb-content", "Details Page", 5*time.Second)
	Screenshot(t, page, "kb_details")

	// Go back to index and verify external link has target=_blank.
	page.MustNavigateBack()
	page.MustWaitStable()
	extLink := WaitForElement(t, page, `.kb-content a[href="https://example.com"]`, 5*time.Second)
	target, err := extLink.Attribute("target")
	if err != nil || target == nil || *target != "_blank" {
		t.Fatalf("external link should have target=_blank")
	}
}

// TestTaskPanel drives a deterministic TaskCreate/TaskUpdate sequence and
// asserts the #todos-body panel rows reflect each task's final state.
func TestTaskPanel(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "task_panel.jsonl.zst", testMode())
	page := f.Page()

	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput(
		`Use ONLY the TaskCreate and TaskUpdate tools. Do not use Bash, Write, Edit, Read, Grep, Glob, or any other tool. ` +
			`Do exactly these calls in order:
1. TaskCreate with subject="first task", description="first task", activeForm="first task"
2. TaskCreate with subject="second task", description="second task", activeForm="second task"
3. TaskCreate with subject="third task", description="third task", activeForm="third task"
4. TaskUpdate with taskId="1", status="in_progress"
5. TaskUpdate with taskId="1", status="completed"
6. TaskUpdate with taskId="2", status="in_progress"
After step 6, reply with the single word "done" and stop.`,
	)
	page.MustElement(`.send-btn`).MustClick()

	WaitForElement(t, page, ".msg-assistant", 180*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 180*time.Second)
	Screenshot(t, page, "task_panel_done")

	// Summary should report 1 of 3 completed.
	summary := page.MustElement("#todos-summary").MustText()
	if !strings.Contains(summary, "1/3") {
		t.Errorf("summary should contain '1/3', got %q", summary)
	}

	// Open the <details> panel so MustText() can read the rows. Otherwise
	// rod treats the collapsed body as having no visible text.
	page.MustElement("#todos-panel").MustEval(`() => this.open = true`)

	// Three rows: completed, in_progress, pending — in that order.
	items := page.MustElements(".todo-item")
	if len(items) != 3 {
		t.Fatalf("expected 3 todo-item rows, got %d", len(items))
	}
	expected := []struct {
		cls   string
		label string
	}{
		{"todo-done", "first task"},
		{"todo-active", "second task"},
		{"todo-pending", "third task"},
	}
	for i, exp := range expected {
		clsAttr, _ := items[i].Attribute("class")
		got := ""
		if clsAttr != nil {
			got = *clsAttr
		}
		if !strings.Contains(got, exp.cls) {
			t.Errorf("row %d: class should contain %q, got %q", i, exp.cls, got)
		}
		text := items[i].MustText()
		if !strings.Contains(text, exp.label) {
			t.Errorf("row %d: text should contain %q, got %q", i, exp.label, text)
		}
	}
}
