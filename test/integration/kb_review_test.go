package integration

import (
	"strings"
	"testing"
	"time"
)

// kbReadAllowSettings auto-approves kb's read-only tools so Claude can
// explore the store, but leaves write and edit to prompt. Their diffs then
// render at the permission prompt, which is the surface under test.
const kbReadAllowSettings = `{
  "permissions": {
    "allow": [
      "mcp__kb__list",
      "mcp__kb__search",
      "mcp__kb__read"
    ]
  }
}
`

// kbNotesFile is a multi-line kb entry used as the edit target. The edit
// changes a line in the middle so the rendered diff has surrounding context.
const kbNotesFile = `# Notes

## Status

- Pending: wire up the health check.
- Done: initial scaffold.
- Next: add tests.

## Open question

Should we cache the result?
`

// TestKBEditReviewComment seeds a kb entry and asks Claude to edit one line of
// it through the kb edit tool. The permission prompt must render the edit as a
// full-file diff with context (proving it read the kb store), not raw JSON and
// not an isolated old/new snippet.
func TestKBEditReviewComment(t *testing.T) {
	t.Parallel()
	WithProviders(t, "kb_edit_review.jsonl.zst", func(t *testing.T, f *ContainerFixture) {

		f.WriteFile(containerWorkdir+"/main.go", "package main\n\nfunc main() {}\n")
		f.WriteFile(containerWorkdir+"/.claude/settings.json", kbReadAllowSettings)
		f.DockerExec("git", "init", containerWorkdir)
		f.RegisterKBMCP()
		f.KBWithStdin(containerWorkdir, kbNotesFile, "write", "projects/notes.md")

		page := f.Page()
		CreatePlainSession(t, page, containerWorkdir)
		WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

		page.MustElement(`textarea[name="text"]`).MustInput(
			"In the kb entry projects/notes.md, change the line '- Pending: wire up the health check.' to '- Done: wire up the health check.' Use the kb edit tool. Do not create or edit any other file.")
		page.MustElement(`.send-btn`).MustClick()

		// Permission prompt with a rendered diff.
		WaitForElement(t, page, ".perm-inline", 60*time.Second)
		WaitForElement(t, page, ".tool-chip[open] .diff-table", 10*time.Second)

		// Context rows prove the diff was built against the full kb file
		// (read via kb), not the bare old/new snippet fallback.
		ctxRows := page.MustElements(".tool-chip[open] .diff-ctx")
		if len(ctxRows) == 0 {
			Screenshot(t, page, "kb_edit_review_no_ctx")
			t.Fatal("kb edit diff should show context rows (read against the kb file)")
		}
		Screenshot(t, page, "kb_edit_review_diff")

		// Add an inline review comment while blocked.
		WaitForElement(t, page, ".tool-chip[open] .diff-ins .diff-line-num", 5*time.Second).MustClick()
		WaitForElement(t, page, ".review-form", 5*time.Second)
		page.MustElement(".review-textarea").MustInput("Confirm the health check is actually wired before marking this done")
		page.MustElement(".review-submit").MustClick()
		WaitForElement(t, page, ".review-comment", 5*time.Second)
		WaitForText(t, page, ".review-bar", "1 comment", 5*time.Second)
		Screenshot(t, page, "kb_edit_review_comment_added")

		// Allow the edit.
		page.MustElement(`.perm-allow`).MustClick()
		WaitForText(t, page, ".tool-name", "Allowed", 10*time.Second)
		WaitForElement(t, page, ".msg-assistant", 60*time.Second)
		WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

		// Send the review as a user message.
		page.MustElement(".review-send-btn").MustClick()
		_, err := page.Timeout(10*time.Second).ElementR(".msg-user", "Code Review Comments")
		if err != nil {
			Screenshot(t, page, "kb_edit_review_send_fail")
			t.Fatalf("review message never appeared: %v", err)
		}
		Screenshot(t, page, "kb_edit_review_sent")

		// The kb edit must have landed in the store.
		content := f.KB(containerWorkdir, "read", "projects/notes.md")
		if !strings.Contains(content, "- Done: wire up the health check.") {
			t.Fatalf("kb edit did not land in the store: %s", content)
		}
	})
}

// TestKBWriteReviewComment asks Claude to create a new kb entry through the kb
// write tool. The permission prompt must render the content as an all-additions
// diff, not raw JSON.
func TestKBWriteReviewComment(t *testing.T) {
	t.Parallel()
	WithProviders(t, "kb_write_review.jsonl.zst", func(t *testing.T, f *ContainerFixture) {

		f.WriteFile(containerWorkdir+"/main.go", "package main\n\nfunc main() {}\n")
		f.WriteFile(containerWorkdir+"/.claude/settings.json", kbReadAllowSettings)
		f.DockerExec("git", "init", containerWorkdir)
		f.RegisterKBMCP()

		page := f.Page()
		CreatePlainSession(t, page, containerWorkdir)
		WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

		page.MustElement(`textarea[name="text"]`).MustInput(
			"Create a new kb entry at projects/calc.md with a short plan for a CLI calculator supporting +, -, *, /. Use the kb write tool. Do not create or edit any other file.")
		page.MustElement(`.send-btn`).MustClick()

		WaitForElement(t, page, ".perm-inline", 60*time.Second)
		WaitForElement(t, page, ".tool-chip[open] .diff-table", 10*time.Second)

		// A new file renders as all-additions: insertions, no deletions.
		insRows := page.MustElements(".tool-chip[open] .diff-ins")
		if len(insRows) == 0 {
			Screenshot(t, page, "kb_write_review_no_ins")
			t.Fatal("expected insertion rows in kb write diff, got none")
		}
		delRows := page.MustElements(".tool-chip[open] .diff-del")
		if len(delRows) > 0 {
			Screenshot(t, page, "kb_write_review_unexpected_del")
			t.Fatalf("new kb file should have no deletions, got %d", len(delRows))
		}
		Screenshot(t, page, "kb_write_review_new_file_diff")

		// Inline comment while blocked.
		WaitForElement(t, page, ".tool-chip[open] .diff-ins .diff-line-num", 5*time.Second).MustClick()
		WaitForElement(t, page, ".review-form", 5*time.Second)
		page.MustElement(".review-textarea").MustInput("Note the division error case in the plan")
		page.MustElement(".review-submit").MustClick()
		WaitForElement(t, page, ".review-comment", 5*time.Second)
		WaitForText(t, page, ".review-bar", "1 comment", 5*time.Second)

		page.MustElement(`.perm-allow`).MustClick()
		WaitForText(t, page, ".tool-name", "Allowed", 10*time.Second)
		WaitForElement(t, page, ".msg-assistant", 60*time.Second)
		WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

		page.MustElement(".review-send-btn").MustClick()
		_, err := page.Timeout(10*time.Second).ElementR(".msg-user", "Code Review Comments")
		if err != nil {
			Screenshot(t, page, "kb_write_review_send_fail")
			t.Fatalf("review message never appeared: %v", err)
		}
		Screenshot(t, page, "kb_write_review_sent")

		// The new entry must exist in the store.
		list := f.KB(containerWorkdir, "list")
		if !strings.Contains(list, "projects/calc.md") {
			t.Fatalf("kb write did not create the entry; kb list: %s", list)
		}
	})
}

// kbSnippetFile is an existing kb entry for the overwrite test.
const kbSnippetFile = `# Snippet

package main

func Add(a, b int) int { return a + b }

func main() {}
`

// TestKBOverwriteReviewComment seeds a kb entry and asks Claude to rewrite it
// through the kb write tool. The permission prompt must render a real diff
// against the existing content (with context), proving the overwrite is
// reviewable, not raw JSON.
func TestKBOverwriteReviewComment(t *testing.T) {
	t.Parallel()
	WithProviders(t, "kb_overwrite_review.jsonl.zst", func(t *testing.T, f *ContainerFixture) {

		f.WriteFile(containerWorkdir+"/main.go", "package main\n\nfunc main() {}\n")
		f.WriteFile(containerWorkdir+"/.claude/settings.json", kbReadAllowSettings)
		f.DockerExec("git", "init", containerWorkdir)
		f.RegisterKBMCP()
		f.KBWithStdin(containerWorkdir, kbSnippetFile, "write", "projects/snippet.md")

		page := f.Page()
		CreatePlainSession(t, page, containerWorkdir)
		WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

		page.MustElement(`textarea[name="text"]`).MustInput(
			"Rewrite the kb entry projects/snippet.md using the kb write tool. Keep the Add function, add a Subtract(a, b int) int method, and keep the package declaration and main. Do not create or edit any other file.")
		page.MustElement(`.send-btn`).MustClick()

		WaitForElement(t, page, ".perm-inline", 60*time.Second)
		WaitForElement(t, page, ".tool-chip[open] .diff-table", 10*time.Second)

		// An overwrite shows context rows (a real diff against the existing
		// kb content), not the all-additions new-file view.
		ctxRows := page.MustElements(".tool-chip[open] .diff-ctx")
		if len(ctxRows) == 0 {
			Screenshot(t, page, "kb_overwrite_review_no_ctx")
			t.Fatal("kb overwrite diff should show context rows (real diff against the existing entry)")
		}
		Screenshot(t, page, "kb_overwrite_review_diff")

		// Inline comment while blocked.
		WaitForElement(t, page, ".tool-chip[open] .diff-ins .diff-line-num", 5*time.Second).MustClick()
		WaitForElement(t, page, ".review-form", 5*time.Second)
		page.MustElement(".review-textarea").MustInput("Make Subtract return (int, error) to match a future error case")
		page.MustElement(".review-submit").MustClick()
		WaitForElement(t, page, ".review-comment", 5*time.Second)
		WaitForText(t, page, ".review-bar", "1 comment", 5*time.Second)

		page.MustElement(`.perm-allow`).MustClick()
		WaitForText(t, page, ".tool-name", "Allowed", 10*time.Second)
		WaitForElement(t, page, ".msg-assistant", 60*time.Second)
		WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)

		page.MustElement(".review-send-btn").MustClick()
		_, err := page.Timeout(10*time.Second).ElementR(".msg-user", "Code Review Comments")
		if err != nil {
			Screenshot(t, page, "kb_overwrite_review_send_fail")
			t.Fatalf("review message never appeared: %v", err)
		}
		Screenshot(t, page, "kb_overwrite_review_sent")

		// The overwrite must have landed: Subtract now present.
		content := f.KB(containerWorkdir, "read", "projects/snippet.md")
		if !strings.Contains(content, "Subtract") {
			t.Fatalf("kb overwrite did not land in the store: %s", content)
		}
	})
}
