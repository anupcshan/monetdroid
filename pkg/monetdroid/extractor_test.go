package monetdroid

import (
	"strings"
	"testing"
)

type step struct {
	chunk string
	want  string
}

func TestGoTestExtractor(t *testing.T) {
	output := strings.Join([]string{
		"=== RUN   TestPass",
		"--- PASS: TestPass (0.01s)",
		"=== RUN   TestFail",
		"--- FAIL: TestFail (0.02s)",
		"    foo_test.go:12: expected true, got false",
		"=== RUN   TestSkip",
		"--- SKIP: TestSkip (0.00s)",
		"ok      example.com/pkg1       0.123s",
		"=== RUN   TestFailTwo",
		"--- FAIL: TestFailTwo (0.05s)",
		"FAIL    example.com/pkg2       0.456s",
		"FAIL",
		"", // trailing newline creates empty last line after split
	}, "\n")

	var ext GoTestExtractor
	ext.Ingest(output)

	s := ext.Summary()

	checks := []string{
		`2 packages`,
		`1 passed`,
		`2 failed`,
		`1 skipped`,
		`TestFail (0.02s)`,
		`TestFailTwo (0.05s)`,
	}
	for _, want := range checks {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}

func TestGoTestExtractorIncremental(t *testing.T) {
	var ext GoTestExtractor

	// First chunk: partial data
	ext.Ingest("=== RUN   TestOne\n--- PASS: TestOne (0.01s)\n=== RUN   TestTw")

	s := ext.Summary()
	if !strings.Contains(s, "1 passed") {
		t.Errorf("expected 1 passed after first chunk:\n%s", s)
	}

	// Second chunk: completes the partial line
	ext.Ingest("o\n--- FAIL: TestTwo (0.03s)\n")

	s = ext.Summary()
	if !strings.Contains(s, "1 passed") {
		t.Errorf("expected 1 passed in final summary:\n%s", s)
	}
	if !strings.Contains(s, "1 failed") {
		t.Errorf("expected 1 failed in final summary:\n%s", s)
	}
	if !strings.Contains(s, "TestTwo (0.03s)") {
		t.Errorf("expected TestTwo in final summary:\n%s", s)
	}
}

func TestGoTestExtractorEmpty(t *testing.T) {
	var ext GoTestExtractor

	s := ext.Summary()
	if !strings.Contains(s, "Running...") {
		t.Errorf("expected Running... for empty state:\n%s", s)
	}
}

func TestGoTestExtractorParallel(t *testing.T) {
	var ext GoTestExtractor
	steps := []step{
		{"=== RUN   TestOne\n", `<div class="bg-loading">Running...</div>`},
		{"=== PAUSE TestOne\n", `<div class="bg-summary-header"><span class="bg-stat">0/1 passed</span></div>`},
		{"=== RUN   TestTwo\n", `<div class="bg-summary-header"><span class="bg-stat">0/2 passed</span></div>`},
		{"=== PAUSE TestTwo\n", `<div class="bg-summary-header"><span class="bg-stat">0/2 passed</span></div>`},
		{"=== RUN   TestThree\n", `<div class="bg-summary-header"><span class="bg-stat">0/3 passed</span></div>`},
		{"=== PAUSE TestThree\n", `<div class="bg-summary-header"><span class="bg-stat">0/3 passed</span></div>`},
		{"=== CONT  TestOne\n", `<div class="bg-summary-header"><span class="bg-stat">0/3 passed</span></div>`},
		{"--- PASS: TestOne (0.01s)\n", `<div class="bg-summary-header"><span class="bg-stat">1/3 passed</span></div>`},
		{"=== CONT  TestTwo\n", `<div class="bg-summary-header"><span class="bg-stat">1/3 passed</span></div>`},
		{"--- FAIL: TestTwo (0.02s)\n",
			`<div class="bg-summary-header"><span class="bg-stat">1/3 passed</span><span class="bg-stat bg-stat-fail">1 failed</span></div>` +
				`<details class="bg-failure"><summary>❌ TestTwo (0.02s)</summary></details>`},
		{"=== CONT  TestThree\n",
			`<div class="bg-summary-header"><span class="bg-stat">1/3 passed</span><span class="bg-stat bg-stat-fail">1 failed</span></div>` +
				`<details class="bg-failure"><summary>❌ TestTwo (0.02s)</summary></details>`},
		{"--- PASS: TestThree (0.01s)\n",
			`<div class="bg-summary-header"><span class="bg-stat">2/3 passed</span><span class="bg-stat bg-stat-fail">1 failed</span></div>` +
				`<details class="bg-failure"><summary>❌ TestTwo (0.02s)</summary></details>`},
		{"ok      example.com/pkg  0.123s\n",
			`<div class="bg-summary-header"><span class="bg-stat">1 packages</span><span class="bg-stat">2/3 passed</span><span class="bg-stat bg-stat-fail">1 failed</span></div>` +
				`<details class="bg-failure"><summary>❌ TestTwo (0.02s)</summary></details>`},
	}
	for i, st := range steps {
		ext.Ingest(st.chunk)
		got := ext.Summary()
		if got != st.want {
			t.Errorf("step %d after %q:\n  got:  %s\n  want: %s", i, st.chunk, got, st.want)
		}
	}
}

func TestGoTestExtractorSubtests(t *testing.T) {
	var ext GoTestExtractor
	steps := []step{
		{"=== RUN   TestOne\n", `<div class="bg-loading">Running...</div>`},
		{"=== RUN   TestOne/a\n", `<div class="bg-loading">Running...</div>`},
		{"=== PAUSE TestOne/a\n", `<div class="bg-summary-header"><span class="bg-stat">0/2 passed</span></div>`},
		{"=== RUN   TestOne/b\n", `<div class="bg-summary-header"><span class="bg-stat">0/3 passed</span></div>`},
		{"=== PAUSE TestOne/b\n", `<div class="bg-summary-header"><span class="bg-stat">0/3 passed</span></div>`},
		{"--- FAIL: TestOne (0.00s)\n",
			`<div class="bg-summary-header"><span class="bg-stat">0/3 passed</span><span class="bg-stat bg-stat-fail">1 failed</span></div>` +
				`<details class="bg-failure"><summary>❌ TestOne (0.00s)</summary></details>`},
		{"    --- PASS: TestOne/a (0.01s)\n",
			`<div class="bg-summary-header"><span class="bg-stat">1/3 passed</span><span class="bg-stat bg-stat-fail">1 failed</span></div>` +
				`<details class="bg-failure"><summary>❌ TestOne (0.00s)</summary></details>`},
		{"    --- FAIL: TestOne/b (0.02s)\n",
			`<div class="bg-summary-header"><span class="bg-stat">1/3 passed</span><span class="bg-stat bg-stat-fail">2 failed</span></div>` +
				`<details class="bg-failure"><summary>❌ TestOne (0.00s)</summary></details>` +
				`<details class="bg-failure"><summary>❌ TestOne/b (0.02s)</summary></details>`},
		{"ok      example.com/pkg  0.123s\n",
			`<div class="bg-summary-header"><span class="bg-stat">1 packages</span><span class="bg-stat">1/3 passed</span><span class="bg-stat bg-stat-fail">2 failed</span></div>` +
				`<details class="bg-failure"><summary>❌ TestOne (0.00s)</summary></details>` +
				`<details class="bg-failure"><summary>❌ TestOne/b (0.02s)</summary></details>`},
	}
	for i, st := range steps {
		ext.Ingest(st.chunk)
		got := ext.Summary()
		if got != st.want {
			t.Errorf("step %d after %q:\n  got:  %s\n  want: %s", i, st.chunk, got, st.want)
		}
	}
}

func TestMatchExtractor(t *testing.T) {
	if MatchExtractor("Bash", "go test ./...") == nil {
		t.Error("expected match for go test")
	}
	if MatchExtractor("Bash", "go build ./...") != nil {
		t.Error("expected no match for go build")
	}
	if MatchExtractor("Read", "go test") != nil {
		t.Error("expected no match for non-Bash tool")
	}
}
