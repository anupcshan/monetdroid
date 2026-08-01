package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/monetdroid/pkg/monetdroid"
)

// claudeUserConfig models the subset of /root/.claude.json the harness writes.
// Claude adds more fields (firstStartTime, machineID, etc.) itself. Merging
// happens before Claude starts, so only these fields are present at merge time.
type claudeUserConfig struct {
	Projects   map[string]claudeProject   `json:"projects,omitempty"`
	McpServers map[string]claudeMcpServer `json:"mcpServers,omitempty"`
}

type claudeProject struct {
	HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted,omitempty"`
}

type claudeMcpServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// kbAllowSettings is dropped at <cwd>/.claude/settings.json so Claude
// auto-approves every kb MCP tool call. Entries are the namespaced tool
// names Claude exposes for the kb server.
const kbAllowSettings = `{
  "permissions": {
    "allow": [
      "mcp__kb__list",
      "mcp__kb__search",
      "mcp__kb__read",
      "mcp__kb__write",
      "mcp__kb__edit",
      "mcp__kb__append",
      "mcp__kb__rm",
      "mcp__kb__mv"
    ]
  }
}
`

// testOnlyNoQuestionsAddendum is written to CLAUDE.md in TestKBNewProject
// only. Real users may want clarifying questions during planning; in a
// non-interactive cassette run there is no one to answer them, so we
// suppress them at the test fixture.
const testOnlyNoQuestionsAddendum = `## For this test

When recording a new project plan in kb, do not stop to ask the user
clarifying questions. Pick reasonable defaults, write them into the
kb entry, and note any open questions inside the entry itself.
`

// RegisterKBMCP adds the user-scope kb mcpServers entry so Claude spawns the
// kb MCP server. It merges into the existing /root/.claude.json rather than
// overwriting it, preserving the workspace trust entry written at container
// startup. The container's /usr/local/bin/kb shim resolves to the test binary
// running `kb mcp`. Must run before the session starts.
func (f *ContainerFixture) RegisterKBMCP() {
	var cfg claudeUserConfig
	if existing := strings.TrimSpace(f.ReadFile("/root/.claude.json")); existing != "" {
		if err := json.Unmarshal([]byte(existing), &cfg); err != nil {
			f.T.Fatalf("RegisterKBMCP: parse /root/.claude.json: %v", err)
		}
	}
	if cfg.McpServers == nil {
		cfg.McpServers = map[string]claudeMcpServer{}
	}
	cfg.McpServers["kb"] = claudeMcpServer{Command: "kb", Args: []string{"mcp"}}
	out, err := json.Marshal(cfg)
	if err != nil {
		f.T.Fatalf("RegisterKBMCP: marshal: %v", err)
	}
	f.WriteFile("/root/.claude.json", string(out))
}

// kbWritePath extracts the path argument from an mcp__kb__write tool_use
// event, or "" when the event is not a kb write or has no path.
func kbWritePath(e monetdroid.ServerMsg) string {
	if e.Type != "tool_use" || e.Tool != "mcp__kb__write" {
		return ""
	}
	if e.Input == nil || len(e.Input.Raw) == 0 {
		return ""
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(e.Input.Raw, &in); err != nil {
		return ""
	}
	return in.Path
}

// TestKBResumeProject seeds a KB entry at projects/foo.md describing
// partial project state that is not derivable from the repo contents, then
// prompts Claude to "resume work on foo" and asserts Claude consulted the
// KB through the kb MCP tools. Verifies an mcp__kb__* call appeared in the
// tool uses, and that the assistant text references a token only present
// in the seeded entry.
func TestKBResumeProject(t *testing.T) {
	t.Parallel()
	WithProviders(t, "kb_resume_project.jsonl.zst", func(t *testing.T, f *ContainerFixture) {

		f.WriteFile(containerWorkdir+"/main.go", `package main

import "fmt"

func main() {
	fmt.Println("calc")
}
`)
		f.WriteFile(containerWorkdir+"/.claude/settings.json", kbAllowSettings)
		f.DockerExec("git", "init", containerWorkdir)
		f.RegisterKBMCP()

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

		page.MustElement(`textarea[name="text"]`).MustInput("What's the status of project foo?")
		page.MustElement(`.send-btn`).MustClick()

		WaitForElement(t, page, ".msg-assistant", 120*time.Second)
		WaitForElement(t, page, "#stop-btn:empty", 180*time.Second)

		events := f.SessionLog()
		kbCalls := 0
		var toolNames []string
		var assistantText strings.Builder
		for _, e := range events {
			if e.Type == "tool_use" {
				toolNames = append(toolNames, e.Tool)
				if strings.HasPrefix(e.Tool, "mcp__kb__") {
					kbCalls++
				}
			}
			if e.Type == "text" && e.Text != "" {
				assistantText.WriteString(e.Text)
				assistantText.WriteString("\n")
			}
		}

		if kbCalls == 0 {
			t.Fatalf("expected at least one mcp__kb__* call; tools seen:\n%s", strings.Join(toolNames, "\n"))
		}

		text := assistantText.String()
		if !strings.Contains(text, "BigInt") && !strings.Contains(text, "big.Int") && !strings.Contains(text, "math/big") {
			t.Errorf("assistant response missing KB-only token (BigInt / big.Int / math/big); got:\n%s", text)
		}
	})
}

// TestKBNewProject starts from an empty KB and asks Claude to plan a new
// project. Asserts Claude created an entry under projects/ via the kb MCP
// write tool, matching the "track new work" guidance in that tool's
// description. Checkpoint behavior (append/edit) is not asserted here
// because the prompt only asks for a plan.
func TestKBNewProject(t *testing.T) {
	t.Parallel()
	WithProviders(t, "kb_new_project.jsonl.zst", func(t *testing.T, f *ContainerFixture) {

		f.WriteFile(containerWorkdir+"/go.mod", "module calc\n\ngo 1.23\n")
		f.WriteFile(containerWorkdir+"/.claude/settings.json", kbAllowSettings)
		f.WriteFile(containerWorkdir+"/CLAUDE.md", testOnlyNoQuestionsAddendum)
		f.DockerExec("git", "init", containerWorkdir)
		f.RegisterKBMCP()

		page := f.Page()
		CreatePlainSession(t, page, containerWorkdir)
		WaitForText(t, page, "#session-label", containerWorkdir, 5*time.Second)

		page.MustElement(`textarea[name="text"]`).MustInput("Start a new project: a CLI calculator supporting +, -, *, / on integer args. Walk through a plan. Record only the project plan in kb. Do not run go or build commands. Do not write, create, or edit any source code files. This task needs the plan, not an implementation.")
		page.MustElement(`.send-btn`).MustClick()

		WaitForElement(t, page, ".msg-assistant", 120*time.Second)
		WaitForElement(t, page, "#stop-btn:empty", 180*time.Second)

		events := f.SessionLog()
		var kbWritesProjects int
		var toolNames []string
		for _, e := range events {
			if e.Type != "tool_use" {
				continue
			}
			toolNames = append(toolNames, e.Tool)
			if strings.HasPrefix(kbWritePath(e), "projects/") {
				kbWritesProjects++
			}
		}

		if kbWritesProjects == 0 {
			t.Fatalf("expected an mcp__kb__write to projects/...; tools seen:\n%s", strings.Join(toolNames, "\n"))
		}

		list := f.KB(containerWorkdir, "list")
		if !strings.Contains(list, "projects/") {
			t.Errorf("kb list missing any projects/ entry; got:\n%s", list)
		}
	})
}
