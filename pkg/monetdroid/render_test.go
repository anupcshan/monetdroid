package monetdroid

import (
	"strings"
	"testing"

	"github.com/anupcshan/monetdroid/pkg/claude/protocol"
)

func TestRenderBranchList_SingleBranchInSync(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "feature", Branches: []BranchStatus{{Name: "feature"}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 2)
	assertRow(t, rows[0], "ws-color-main", "main", "=")
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "feature", "=")
}

func TestRenderBranchList_AheadAndDirty(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "wip", Branches: []BranchStatus{{Name: "wip", AheadMain: 3, Dirty: true}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 2)
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "wip", "↑3 *")
}

func TestRenderBranchList_BehindMain(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "stale", Branches: []BranchStatus{{Name: "stale", BehindMain: 5}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 2)
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "stale", "↓5")
}

func TestRenderBranchList_Diverged(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "diverged", Branches: []BranchStatus{{Name: "diverged", AheadMain: 2, BehindMain: 3}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 2)
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "diverged", "↑2 ↓3")
}

func TestRenderBranchList_MainDirty(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "master",
		MainDirty:     true,
		Workstreams: []WorkstreamStatus{
			{Name: "feature", Branches: []BranchStatus{{Name: "feature"}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 2)
	assertRow(t, rows[0], "ws-color-main", "master", "= *")
}

func TestRenderBranchList_MultipleBranches(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "alpha", Branches: []BranchStatus{{Name: "alpha"}}},
			{Name: "beta", Branches: []BranchStatus{{Name: "beta", AheadMain: 1}}},
			{Name: "gamma", Branches: []BranchStatus{{Name: "gamma"}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 4)
	assertRow(t, rows[0], "ws-color-main", "main", "=")
	assertRow(t, rows[1], "ws-child ws-color-a", "alpha", "=")
	assertRow(t, rows[2], "ws-child ws-color-b", "beta", "↑1")
	assertRow(t, rows[3], "ws-child ws-last ws-color-a", "gamma", "=")
}

func TestRenderBranchList_StackedBranches(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "auth", Branches: []BranchStatus{
				{Name: "auth", Depth: 0, AheadMain: 3},
				{Name: "auth-ui", Depth: 1, AheadMain: 5},
				{Name: "auth-tests", Depth: 2, AheadMain: 6},
			}},
			{Name: "perf", Branches: []BranchStatus{{Name: "perf"}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 5)
	assertRow(t, rows[0], "ws-color-main", "main", "=")
	assertRow(t, rows[1], "ws-child ws-color-a", "auth", "↑3")
	assertRow(t, rows[2], "ws-child ws-last ws-color-a", "auth-ui", "↑5")
	assertRow(t, rows[3], "ws-child ws-last ws-color-a", "auth-tests", "↑6")
	assertRow(t, rows[4], "ws-child ws-last ws-color-b", "perf", "=")
}

// Depth pattern: 0, 1, 1, 2 (fork at depth 1, one child goes deeper).
func TestRenderBranchList_ForkedStack(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "auth", Branches: []BranchStatus{
				{Name: "auth", Depth: 0, AheadMain: 3},
				{Name: "auth-ui", Depth: 1, AheadMain: 5},
				{Name: "auth-api", Depth: 1, AheadMain: 4},
				{Name: "auth-api-test", Depth: 2, AheadMain: 7},
			}},
			{Name: "perf", Branches: []BranchStatus{{Name: "perf"}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 6)
	assertRow(t, rows[0], "ws-color-main", "main", "=")
	assertRow(t, rows[1], "ws-child ws-color-a", "auth", "↑3")
	assertRow(t, rows[2], "ws-child ws-color-a", "auth-ui", "↑5")
	assertRow(t, rows[3], "ws-child ws-last ws-color-a", "auth-api", "↑4")
	assertRow(t, rows[4], "ws-child ws-last ws-color-a", "auth-api-test", "↑7")
	assertRow(t, rows[5], "ws-child ws-last ws-color-b", "perf", "=")
}

// Depth pattern: 0, 1, 2 plus a separate workstream (linear 3-deep stack).
func TestRenderBranchList_DeepLinearStack(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "feat", Branches: []BranchStatus{
				{Name: "feat", Depth: 0},
				{Name: "feat-v2", Depth: 1, AheadMain: 2},
				{Name: "feat-v3", Depth: 2, AheadMain: 5},
			}},
			{Name: "bugfix", Branches: []BranchStatus{{Name: "bugfix", AheadMain: 1}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 5)
	assertRow(t, rows[0], "ws-color-main", "main", "=")
	assertRow(t, rows[1], "ws-child ws-color-a", "feat", "=")
	assertRow(t, rows[2], "ws-child ws-last ws-color-a", "feat-v2", "↑2")
	assertRow(t, rows[3], "ws-child ws-last ws-color-a", "feat-v3", "↑5")
	assertRow(t, rows[4], "ws-child ws-last ws-color-b", "bugfix", "↑1")
}

// Depth pattern: 0, 1, 2 in a single workstream (linear 3-deep stack).
func TestRenderBranchList_SingleDeepStack(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "feat", Branches: []BranchStatus{
				{Name: "feat", Depth: 0, AheadMain: 1},
				{Name: "feat-v2", Depth: 1, AheadMain: 3},
				{Name: "feat-v3", Depth: 2, AheadMain: 6},
			}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 4)
	assertRow(t, rows[0], "ws-color-main", "main", "=")
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "feat", "↑1")
	assertRow(t, rows[2], "ws-child ws-last ws-color-a", "feat-v2", "↑3")
	assertRow(t, rows[3], "ws-child ws-last ws-color-a", "feat-v3", "↑6")
}

// helpers

func branchRows(t *testing.T, html string, want int) []string {
	t.Helper()
	var rows []string
	for _, part := range strings.Split(html, "</div>") {
		if strings.Contains(part, "ws-branch-row") {
			rows = append(rows, part)
		}
	}
	if len(rows) != want {
		t.Fatalf("expected %d rows, got %d:\n%s", want, len(rows), html)
	}
	return rows
}

func assertRow(t *testing.T, row, classes, name, status string) {
	t.Helper()
	for _, c := range strings.Fields(classes) {
		if !strings.Contains(row, c) {
			t.Errorf("row missing class %q:\n%s", c, row)
		}
	}
	if !strings.Contains(row, ">"+name+"<") {
		t.Errorf("row missing name %q:\n%s", name, row)
	}
	for _, part := range strings.Fields(status) {
		if !strings.Contains(row, part) {
			t.Errorf("row missing status %q:\n%s", part, row)
		}
	}
}

func TestRenderPermSuggestions(t *testing.T) {
	msg := ServerMsg{
		SessionID: "sess-1",
		PermID:    "perm-1",
		PermTool:  "Bash",
		PermInput: &protocol.ToolInput{Bash: &protocol.BashInput{Command: "curl -s http://example.com"}},
		PermSuggestions: []protocol.PermSuggestion{
			{Type: "setMode", Mode: "acceptEdits"},
			{Type: "addRules", Rules: []protocol.PermissionRuleVal{{ToolName: "Bash", RuleContent: "curl -s http://example.com"}}},
			{Type: "addDirectories", Directories: []string{"/home/project/src"}},
		},
	}
	html := RenderPermission(msg)

	if strings.Contains(html, "Accept Edits") {
		t.Error("setMode suggestion should not render in permission prompt")
	}
	if !strings.Contains(html, "Always allow: curl -s http://example.com") {
		t.Error("addRules should render 'Always allow:' label")
	}
	if !strings.Contains(html, "Add /home/project/src") {
		t.Error("addDirectories should render 'Add' label")
	}
	if !strings.Contains(html, `type="checkbox" name="suggestion"`) {
		t.Error("suggestions should render as checkboxes")
	}
	if !strings.Contains(html, "Allow selected") {
		t.Error("should have 'Allow selected' button")
	}
	if !strings.Contains(html, "Deny") {
		t.Error("should have Deny button")
	}
	if !strings.Contains(html, "Allow once") {
		t.Error("should have 'Allow once' button")
	}
}

func TestRenderPermSuggestions_NoAddRules(t *testing.T) {
	msg := ServerMsg{
		SessionID:       "sess-1",
		PermID:          "perm-1",
		PermTool:        "Bash",
		PermInput:       &protocol.ToolInput{Bash: &protocol.BashInput{Command: "echo hi"}},
		PermSuggestions: nil,
	}
	html := RenderPermission(msg)
	if strings.Contains(html, "perm-suggestions") {
		t.Error("nil suggestions should not render perm-suggestions")
	}
	if strings.Contains(html, "Allow selected") {
		t.Error("nil suggestions should not render 'Allow selected'")
	}
}

func TestRenderPermSuggestions_OnlySetMode(t *testing.T) {
	msg := ServerMsg{
		SessionID: "sess-1",
		PermID:    "perm-1",
		PermTool:  "Bash",
		PermInput: &protocol.ToolInput{Bash: &protocol.BashInput{Command: "echo hi"}},
		PermSuggestions: []protocol.PermSuggestion{
			{Type: "setMode", Mode: "acceptEdits"},
		},
	}
	html := RenderPermission(msg)
	if strings.Contains(html, "perm-suggestions") {
		t.Error("only setMode should not render perm-suggestions")
	}
	if strings.Contains(html, "Allow selected") {
		t.Error("only setMode should not render 'Allow selected'")
	}
}

func TestRenderPermSuggestions_MultipleRulesOneCheckbox(t *testing.T) {
	msg := ServerMsg{
		SessionID: "sess-1",
		PermID:    "perm-1",
		PermTool:  "Bash",
		PermInput: &protocol.ToolInput{Bash: &protocol.BashInput{Command: "python3 script.py"}},
		PermSuggestions: []protocol.PermSuggestion{
			{Type: "addRules", Rules: []protocol.PermissionRuleVal{
				{ToolName: "Bash", RuleContent: "curl -s http://api.example.com/*"},
				{ToolName: "Bash", RuleContent: "python3 -c 'import requests'"},
			}},
		},
	}
	html := RenderPermission(msg)
	if !strings.Contains(html, "Always allow: curl -s http://api.example.com/*; python3 -c &#39;import requests&#39;") {
		t.Error("multiple rules in one suggestion should be joined with '; '")
	}
	checkboxes := strings.Count(html, `type="checkbox"`)
	if checkboxes != 1 {
		t.Errorf("one suggestion with multiple rules should be 1 checkbox, got %d", checkboxes)
	}
}
