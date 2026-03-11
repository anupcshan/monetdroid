package integration

import (
	"flag"
	"os"
	"path/filepath"
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

func TestContainerSimpleTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container test in short mode")
	}

	f := SetupWithContainer(t, "container_simple_turn.jsonl", testMode())
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
	Screenshot(t, page, "container_simple_user_msg")

	// Wait for assistant response (longer timeout for container startup + API)
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	Screenshot(t, page, "container_simple_response")

	// Cost bar should show
	WaitForElement(t, page, "#cost-bar:not(:empty)", 10*time.Second)
	Screenshot(t, page, "container_simple_cost")
}

func TestContainerToolUse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container test in short mode")
	}

	f := SetupWithContainer(t, "container_tool_use.jsonl", testMode())

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

	// Send a prompt that should trigger multiple tool calls / subagents
	page.MustElement(`textarea[name="text"]`).MustInput("Read all three Go files and summarize what each one does")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for user message
	WaitForText(t, page, ".msg-user", "Read all three Go files", 30*time.Second)

	// Wait for assistant response (longer timeout — may involve multiple API calls)
	WaitForElement(t, page, ".msg-assistant", 120*time.Second)

	// Should have tool use chips
	WaitForElement(t, page, ".tool-chip", 10*time.Second)
	Screenshot(t, page, "container_tool_use_response")

	// Cost bar should show
	WaitForElement(t, page, "#cost-bar:not(:empty)", 10*time.Second)
	Screenshot(t, page, "container_tool_use_cost")
}
