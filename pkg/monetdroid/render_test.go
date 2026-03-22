package monetdroid

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderBranchList_SingleBranchInSync(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "feature", Branches: []BranchStatus{{Name: "feature"}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 2)
	assertRow(t, rows[0], "ws-color-main", "main", "=", 0)
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "feature", "=", 0)
}

func TestRenderBranchList_AheadAndDirty(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "wip", Branches: []BranchStatus{{Name: "wip", AheadMain: 3, Dirty: true}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 2)
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "wip", "↑3 *", 0)
}

func TestRenderBranchList_BehindMain(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "stale", Branches: []BranchStatus{{Name: "stale", BehindMain: 5}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 2)
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "stale", "↓5", 0)
}

func TestRenderBranchList_Diverged(t *testing.T) {
	panel := BranchPanel{
		DefaultBranch: "main",
		Workstreams: []WorkstreamStatus{
			{Name: "diverged", Branches: []BranchStatus{{Name: "diverged", AheadMain: 2, BehindMain: 3}}},
		},
	}
	rows := branchRows(t, RenderBranchList(panel), 2)
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "diverged", "↑2 ↓3", 0)
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
	assertRow(t, rows[0], "ws-color-main", "master", "= *", 0)
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
	assertRow(t, rows[0], "ws-color-main", "main", "=", 0)
	assertRow(t, rows[1], "ws-child ws-color-a", "alpha", "=", 0)
	assertRow(t, rows[2], "ws-child ws-color-b", "beta", "↑1", 0)
	assertRow(t, rows[3], "ws-child ws-last ws-color-a", "gamma", "=", 0)
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
	assertRow(t, rows[0], "ws-color-main", "main", "=", 0)
	assertRow(t, rows[1], "ws-child ws-color-a", "auth", "↑3", 0)
	assertRow(t, rows[2], "ws-child ws-color-a", "auth-ui", "↑5", 1)
	assertRow(t, rows[3], "ws-child ws-color-a", "auth-tests", "↑6", 2)
	assertRow(t, rows[4], "ws-child ws-last ws-color-b", "perf", "=", 0)
}

// Depth pattern: 1, 2, 2, 3, 1 — fork at depth 1, one child goes deeper.
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
	assertRow(t, rows[0], "ws-color-main", "main", "=", 0)
	assertRow(t, rows[1], "ws-child ws-color-a", "auth", "↑3", 0)
	assertRow(t, rows[2], "ws-child ws-color-a", "auth-ui", "↑5", 1)
	assertRow(t, rows[3], "ws-child ws-color-a", "auth-api", "↑4", 1)
	assertRow(t, rows[4], "ws-child ws-color-a", "auth-api-test", "↑7", 2)
	assertRow(t, rows[5], "ws-child ws-last ws-color-b", "perf", "=", 0)
}

// Depth pattern: 1, 2, 3, 1 — linear 3-deep stack + separate workstream.
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
	assertRow(t, rows[0], "ws-color-main", "main", "=", 0)
	assertRow(t, rows[1], "ws-child ws-color-a", "feat", "=", 0)
	assertRow(t, rows[2], "ws-child ws-color-a", "feat-v2", "↑2", 1)
	assertRow(t, rows[3], "ws-child ws-color-a", "feat-v3", "↑5", 2)
	assertRow(t, rows[4], "ws-child ws-last ws-color-b", "bugfix", "↑1", 0)
}

// Depth pattern: 1, 2, 3 — single workstream, linear 3-deep stack.
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
	assertRow(t, rows[0], "ws-color-main", "main", "=", 0)
	assertRow(t, rows[1], "ws-child ws-last ws-color-a", "feat", "↑1", 0)
	assertRow(t, rows[2], "ws-child ws-last ws-color-a", "feat-v2", "↑3", 1)
	assertRow(t, rows[3], "ws-child ws-last ws-color-a", "feat-v3", "↑6", 2)
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

func assertRow(t *testing.T, row, classes, name, status string, depth int) {
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
	if depth > 0 {
		expected := fmt.Sprintf("margin-left:%dpx", depth*20)
		if !strings.Contains(row, expected) {
			t.Errorf("row missing depth indent %q:\n%s", expected, row)
		}
	} else if strings.Contains(row, "margin-left:") {
		t.Errorf("row should have no depth indent but has one:\n%s", row)
	}
}
