package integration

import (
	"strings"
	"testing"
	"time"
)

// serverFile is a ~50-line Go HTTP server used as the edit target.
// The edit test changes something in the middle of the file.
const serverFile = `package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

type Status struct {
	Uptime  string ` + "`" + `json:"uptime"` + "`" + `
	Version string ` + "`" + `json:"version"` + "`" + `
}

var startTime = time.Now()

func handleHealth(w http.ResponseWriter, r *http.Request) {
	status := Status{
		Uptime:  time.Since(startTime).String(),
		Version: "1.0.0",
	}
	json.NewEncoder(w).Encode(status)
}

func handleGreet(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	fmt.Fprintf(w, "Hello, %s!", name)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/greet", handleGreet)
	log.Printf("listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
`

func TestEditReviewComment(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "edit_review.jsonl", testMode())

	f.WriteFile(containerWorkdir+"/server.go", serverFile)

	page := f.Page()

	// Create session.
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	// Ask Claude to edit something in the middle of the file.
	page.MustElement(`textarea[name="text"]`).MustInput(
		"In server.go, change the handleGreet function to return 'Greetings, <name>!' instead of 'Hello, <name>!'. Use the Edit tool.")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission prompt with diff (inline inside tool chip).
	WaitForElement(t, page, ".perm-inline", 60*time.Second)

	// Diff table with clickable line numbers must be present.
	WaitForElement(t, page, ".diff-table", 10*time.Second)
	WaitForElement(t, page, ".diff-line-num", 10*time.Second)
	Screenshot(t, page, "edit_review_diff")

	// Click the line number on an added line to open the comment form.
	// There may be multiple Edit tool chips (Claude retries after errors).
	// Scope to the open one (the one with the perm-inline).
	activeChip := WaitForElement(t, page, ".tool-chip[open] .diff-ins .diff-line-num", 5*time.Second)
	activeChip.MustClick()

	// Fill in the review comment form.
	WaitForElement(t, page, ".review-form", 5*time.Second)
	page.MustEval(`() => document.querySelector('.review-form').scrollIntoView({block:'center'})`)
	Screenshot(t, page, "edit_review_form")

	page.MustElement(".review-textarea").MustInput("Consider using log.Printf instead of fmt.Fprintf for consistency with the rest of the file")
	page.MustElement(".review-submit").MustClick()

	// Comment chip should appear, and review bar should show 1 comment.
	WaitForElement(t, page, ".review-comment", 5*time.Second)
	WaitForText(t, page, ".review-bar", "1 comment", 5*time.Second)
	Screenshot(t, page, "edit_review_comment_added")

	// Allow the edit permission.
	page.MustElement(`.perm-allow`).MustClick()
	WaitForText(t, page, ".tool-name", "Allowed", 10*time.Second)

	// Wait for first turn to complete.
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Review bar should still be visible with the comment.
	WaitForText(t, page, ".review-bar", "1 comment", 5*time.Second)
	Screenshot(t, page, "edit_review_before_send")

	// Send the review.
	page.MustElement(".review-send-btn").MustClick()

	// Review message should appear as a user message.
	_, err := page.Timeout(10*time.Second).ElementR(".msg-user", "Code Review Comments")
	if err != nil {
		Screenshot(t, page, "edit_review_send_fail")
		t.Fatalf("review message never appeared: %v", err)
	}
	Screenshot(t, page, "edit_review_sent")

	// Review bar should be cleared.
	reviewBarHTML := page.MustEval(`() => document.getElementById('review-bar').innerHTML`).String()
	if strings.Contains(reviewBarHTML, "comment") {
		Screenshot(t, page, "edit_review_bar_not_cleared")
		t.Fatalf("review bar should be empty after send, got: %s", reviewBarHTML)
	}

	// Wait for Claude to respond to the review.
	WaitForElement(t, page, "#stop-btn:empty", 120*time.Second)
	Screenshot(t, page, "edit_review_response")
}

func TestWriteReviewComment(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "write_review.jsonl", testMode())

	page := f.Page()

	// Create session.
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	// --- Turn 1: Create a new file (Write to nonexistent path) ---

	page.MustElement(`textarea[name="text"]`).MustInput(
		"Create a new file called calculator.go with a Calculator struct that has Add, Subtract, Multiply, and Divide methods. Each method should take two float64 arguments and return (float64, error). Divide should return an error for division by zero. Include a package declaration and imports. Use the Write tool.")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission with all-additions diff.
	WaitForElement(t, page, ".perm-inline", 60*time.Second)

	// All-additions diff table with clickable line numbers.
	WaitForElement(t, page, ".diff-table", 10*time.Second)
	WaitForElement(t, page, ".diff-line-num", 10*time.Second)

	// Verify these are all insertion lines (Write diff is all green).
	insRows := page.MustElements(".diff-ins")
	if len(insRows) == 0 {
		Screenshot(t, page, "write_review_no_ins_rows")
		t.Fatal("expected insertion rows in Write diff, got none")
	}
	// No removal lines should exist in a Write diff.
	delRows := page.MustElements(".diff-del")
	if len(delRows) > 0 {
		Screenshot(t, page, "write_review_unexpected_del")
		t.Fatalf("expected no deletion rows in Write diff, got %d", len(delRows))
	}
	Screenshot(t, page, "write_review_new_file_diff")

	// Click line number on an added line (scope to open tool chip).
	WaitForElement(t, page, ".tool-chip[open] .diff-ins .diff-line-num", 5*time.Second).MustClick()

	// Fill review comment.
	WaitForElement(t, page, ".review-form", 5*time.Second)
	page.MustElement(".review-textarea").MustInput("Add godoc comments to the exported methods")
	page.MustElement(".review-submit").MustClick()

	// Verify comment chip and review bar.
	WaitForElement(t, page, ".review-comment", 5*time.Second)
	WaitForText(t, page, ".review-bar", "1 comment", 5*time.Second)
	Screenshot(t, page, "write_review_new_file_comment")

	// Allow the write.
	page.MustElement(`.perm-allow`).MustClick()
	WaitForText(t, page, ".tool-name", "Allowed", 10*time.Second)

	// Wait for first turn to complete.
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Send the review for the new file.
	page.MustElement(".review-send-btn").MustClick()
	_, err := page.Timeout(10*time.Second).ElementR(".msg-user", "Code Review Comments")
	if err != nil {
		Screenshot(t, page, "write_review_new_send_fail")
		t.Fatalf("review message for new file never appeared: %v", err)
	}
	Screenshot(t, page, "write_review_new_file_sent")

	// Claude acts on the review (reads the file, edits to add godoc comments).
	// Accept Edits so all follow-up permissions are auto-approved.
	acceptBtn, err := page.Timeout(120*time.Second).ElementR(".perm-allow", "Accept Edits")
	if err != nil {
		Screenshot(t, page, "write_review_followup_fail")
		t.Fatalf("follow-up permission never appeared: %v", err)
	}
	Screenshot(t, page, "write_review_followup_perm")
	acceptBtn.MustClick()

	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
	Screenshot(t, page, "write_review_response")

	// Verify the file was written.
	content := f.ReadFile(containerWorkdir + "/calculator.go")
	if !strings.Contains(content, "Calculator") {
		t.Fatalf("calculator.go should contain Calculator, got: %.200s", content)
	}
}

// calculatorFile is an existing file for the overwrite test.
const calculatorFile = `package main

import (
	"errors"
	"fmt"
)

type Calculator struct{}

func (c *Calculator) Add(a, b float64) (float64, error) {
	return a + b, nil
}

func (c *Calculator) Subtract(a, b float64) (float64, error) {
	return a - b, nil
}

func (c *Calculator) Multiply(a, b float64) (float64, error) {
	return a * b, nil
}

func (c *Calculator) Divide(a, b float64) (float64, error) {
	if b == 0 {
		return 0, errors.New("division by zero")
	}
	return a / b, nil
}

func main() {
	c := &Calculator{}
	result, _ := c.Add(2, 3)
	fmt.Println(result)
}
`

func TestOverwriteReviewComment(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "overwrite_review.jsonl", testMode())

	f.WriteFile(containerWorkdir+"/calculator.go", calculatorFile)

	page := f.Page()

	// Create session.
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	// Ask Claude to overwrite the file.
	page.MustElement(`textarea[name="text"]`).MustInput(
		"Overwrite calculator.go to add a Modulo method and godoc comments on all exported types and methods. Rewrite the entire file using the Write tool.")
	page.MustElement(`.send-btn`).MustClick()

	// Wait for permission with diff. An overwrite of an existing file should
	// show a real diff with context lines (old+new line numbers), not all-green.
	WaitForElement(t, page, ".perm-inline", 60*time.Second)
	WaitForElement(t, page, ".tool-chip[open] .diff-table", 10*time.Second)

	// Context lines have both old and new gutter numbers — proves this is a
	// real diff against the existing file, not the all-additions fallback.
	ctxRows := page.MustElements(".tool-chip[open] .diff-ctx")
	if len(ctxRows) == 0 {
		Screenshot(t, page, "overwrite_review_no_ctx_rows")
		t.Fatal("overwrite diff should show context rows (real diff against existing file)")
	}
	Screenshot(t, page, "overwrite_review_diff")

	// Click line number on an added line.
	WaitForElement(t, page, ".tool-chip[open] .diff-ins .diff-line-num", 5*time.Second).MustClick()

	// Fill review comment.
	WaitForElement(t, page, ".review-form", 5*time.Second)
	page.MustElement(".review-textarea").MustInput("The Modulo method should also check for division by zero")
	page.MustElement(".review-submit").MustClick()

	// Verify comment chip and review bar.
	WaitForElement(t, page, ".review-comment", 5*time.Second)
	WaitForText(t, page, ".review-bar", "1 comment", 5*time.Second)
	Screenshot(t, page, "overwrite_review_comment_added")

	// Allow the overwrite.
	page.MustElement(`.perm-allow`).MustClick()
	WaitForText(t, page, ".tool-name", "Allowed", 10*time.Second)

	// Wait for turn to complete.
	WaitForElement(t, page, ".msg-assistant", 60*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

	// Send the review.
	page.MustElement(".review-send-btn").MustClick()
	_, err := page.Timeout(10*time.Second).ElementR(".msg-user", "Code Review Comments")
	if err != nil {
		Screenshot(t, page, "overwrite_review_send_fail")
		t.Fatalf("review message never appeared: %v", err)
	}
	Screenshot(t, page, "overwrite_review_sent")

	// Wait for Claude's response.
	WaitForElement(t, page, "#stop-btn:empty", 120*time.Second)
	Screenshot(t, page, "overwrite_review_response")

	// Verify the file was overwritten.
	content := f.ReadFile(containerWorkdir + "/calculator.go")
	if !strings.Contains(content, "Modulo") {
		t.Fatalf("calculator.go should contain Modulo after overwrite, got: %.200s", content)
	}
}
