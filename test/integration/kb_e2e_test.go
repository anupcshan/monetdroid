package integration

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
	"github.com/anupcshan/monetdroid/pkg/monetdroid"
)

// bashCommand extracts the Bash command from a tool_use event by
// re-parsing Input.Raw. ToolInput's UnmarshalJSON only fills Raw; the
// per-tool typed field (Input.Bash) needs the tool name to resolve,
// which generic JSON decode doesn't have.
func bashCommand(e monetdroid.ServerMsg) string {
	if e.Input == nil || len(e.Input.Raw) == 0 {
		return ""
	}
	var b protocol.BashInput
	if err := json.Unmarshal(e.Input.Raw, &b); err != nil {
		return ""
	}
	return b.Command
}

// kbVerbRe matches a `kb <verb>` invocation inside a Bash command string.
// The verb set is the full kb CLI surface as of the test's writing.
var kbVerbRe = regexp.MustCompile(`\bkb\s+(list|read|write|append|edit|rm|mv|search)\b`)

// kbAllowSettings is dropped at `<cwd>/.claude/settings.json` in each test's
// fixture so Claude auto-approves all kb invocations (including compound
// commands with stdin pipes used by `kb write` / `kb append` / `kb edit`).
const kbAllowSettings = `{
  "permissions": {
    "allow": [
      "Bash(kb list*)",
      "Bash(kb read*)",
      "Bash(kb search*)",
      "Bash(kb write*)",
      "Bash(kb append*)",
      "Bash(kb edit*)",
      "Bash(kb rm*)",
      "Bash(kb mv*)",
      "Bash(kb --help)"
    ]
  }
}
`

// testOnlyNoQuestionsAddendum is appended to the installed CLAUDE.md
// in TestKBNewProject only. Real users may want clarifying questions
// during planning; in a non-interactive cassette run there is no one
// to answer them, so we suppress them at the test fixture.
const testOnlyNoQuestionsAddendum = `
## For this test

When recording a new project plan in kb, do not stop to ask the user
clarifying questions. Pick reasonable defaults, write them into the
kb entry, and note any open questions inside the entry itself.
`

// TestKBResumeProject seeds a KB entry at `projects/foo.md` describing
// partial project state that is not derivable from the repo contents, then
// prompts Claude to "resume work on foo" and asserts Claude consulted the
// KB. Verifies a `kb` invocation appeared in the Bash tool uses, and that
// the assistant text references a token only present in the seeded entry.
func TestKBResumeProject(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "kb_resume_project.jsonl.zst", testMode())

	f.WriteFile(containerWorkdir+"/main.go", `package main

import "fmt"

func main() {
	fmt.Println("calc")
}
`)
	f.WriteFile(containerWorkdir+"/.claude/settings.json", kbAllowSettings)
	f.DockerExec("git", "init", containerWorkdir)
	f.DockerExec("kbadmin", "install", containerWorkdir+"/CLAUDE.md")

	f.KBWithStdin(containerWorkdir, `# Foo

## Status

- Decided: back numbers with Go's math/big BigInt for arbitrary precision.
- Done: parser for + - * /.
- Next: implement division; must return an error on divide-by-zero.
- Open question: add modulo support?
`, "write", "projects/foo.md")

	page := f.Page()
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput("Let's resume work on foo.")
	page.MustElement(`.send-btn`).MustClick()

	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 180*time.Second)

	events := f.SessionLog()
	kbCalls := 0
	var assistantText strings.Builder
	var bashCmds []string
	for _, e := range events {
		if e.Type == "tool_use" && e.Tool == "Bash" {
			cmd := bashCommand(e)
			bashCmds = append(bashCmds, cmd)
			if kbVerbRe.MatchString(cmd) {
				kbCalls++
			}
		}
		if e.Type == "text" && e.Text != "" {
			assistantText.WriteString(e.Text)
			assistantText.WriteString("\n")
		}
	}

	if kbCalls == 0 {
		t.Fatalf("expected at least one kb invocation; bash commands seen:\n%s", strings.Join(bashCmds, "\n---\n"))
	}

	text := assistantText.String()
	if !strings.Contains(text, "BigInt") && !strings.Contains(text, "big.Int") && !strings.Contains(text, "math/big") {
		t.Errorf("assistant response missing KB-only token (BigInt / big.Int / math/big); got:\n%s", text)
	}
}

// TestKBNewProject starts from an empty KB and asks Claude to plan a new
// project. Asserts Claude created an entry under `projects/`, matching
// the "track new work" guidance in the installed kb.md. Checkpoint
// behavior (append/edit) is not asserted here because the prompt only
// asks for a plan; checkpointing applies during implementation.
func TestKBNewProject(t *testing.T) {
	t.Parallel()
	f := SetupWithContainer(t, "kb_new_project.jsonl.zst", testMode())

	f.WriteFile(containerWorkdir+"/go.mod", "module calc\n\ngo 1.23\n")
	f.WriteFile(containerWorkdir+"/.claude/settings.json", kbAllowSettings)
	f.DockerExec("git", "init", containerWorkdir)
	f.DockerExec("kbadmin", "install", containerWorkdir+"/CLAUDE.md")
	f.WriteFile(containerWorkdir+"/CLAUDE.md", f.ReadFile(containerWorkdir+"/CLAUDE.md")+testOnlyNoQuestionsAddendum)

	page := f.Page()
	CreatePlainSession(t, page, containerWorkdir)
	WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

	page.MustElement(`textarea[name="text"]`).MustInput("Start a new project: a Go CLI calculator supporting +, -, *, / on integer args. Walk through a plan.")
	page.MustElement(`.send-btn`).MustClick()

	WaitForElement(t, page, ".msg-assistant", 120*time.Second)
	WaitForElement(t, page, "#stop-btn:empty", 180*time.Second)

	events := f.SessionLog()
	var kbWritesProjects int
	var bashCmds []string
	for _, e := range events {
		if e.Type != "tool_use" || e.Tool != "Bash" {
			continue
		}
		cmd := bashCommand(e)
		bashCmds = append(bashCmds, cmd)
		if strings.Contains(cmd, "kb write projects/") || strings.Contains(cmd, `kb write "projects/`) || strings.Contains(cmd, "kb write 'projects/") {
			kbWritesProjects++
		}
	}

	if kbWritesProjects == 0 {
		t.Fatalf("expected at least one `kb write projects/…` call; bash commands seen:\n%s", strings.Join(bashCmds, "\n---\n"))
	}

	list := f.KB(containerWorkdir, "list")
	if !strings.Contains(list, "projects/") {
		t.Errorf("kb list missing any projects/ entry; got:\n%s", list)
	}
}
