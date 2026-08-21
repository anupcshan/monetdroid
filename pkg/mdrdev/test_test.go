package mdrdev

import (
	"reflect"
	"strings"
	"testing"
)

func runProcessor(lines []string) []string {
	p := newProcessor()
	var out []string
	for _, line := range lines {
		out = append(out, p.Process(line)...)
	}
	return out
}

func TestProcessorParallelDeinterleave(t *testing.T) {
	// Two parallel tests whose logs are interleaved by go test -v. The
	// passing test's log must be dropped; the failing test's log must be
	// kept and emitted as one contiguous block before its --- line.
	in := []string{
		"=== RUN   TestPass",
		"=== PAUSE TestPass",
		"=== RUN   TestFail",
		"=== PAUSE TestFail",
		"=== CONT  TestPass",
		"=== CONT  TestFail",
		"=== NAME  TestPass",
		"    a_test.go:10: passing log that must be dropped",
		"=== NAME  TestFail",
		"    a_test.go:20: failing log one",
		"    a_test.go:21: failing log two",
		"--- PASS: TestPass (0.01s)",
		"--- FAIL: TestFail (0.02s)",
		"FAIL",
		"FAIL\texample.com/pkg\t0.500s",
	}
	want := []string{
		"--- PASS: TestPass (0.01s)",
		"    a_test.go:20: failing log one",
		"    a_test.go:21: failing log two",
		"--- FAIL: TestFail (0.02s)",
		"FAIL",
		"FAIL\texample.com/pkg\t0.500s",
	}
	if got := runProcessor(in); !reflect.DeepEqual(got, want) {
		t.Errorf("output mismatch:\n got:  %v\n want: %v", got, want)
	}
}

func TestProcessorSubtestReturnsToParent(t *testing.T) {
	// After a subtest completes, log lines belong to the parent again even
	// without a fresh === NAME marker for the parent. The parent's own
	// passing log must be dropped, not passed through inline.
	in := []string{
		"=== RUN   TestParent",
		"=== NAME  TestParent",
		"=== RUN   TestParent/sub",
		"=== NAME  TestParent/sub",
		"    a_test.go:5: sub log",
		"--- PASS: TestParent/sub (0.00s)",
		"    a_test.go:10: parent log after sub",
		"--- PASS: TestParent (0.00s)",
		"ok\tpkg\t0.1s",
	}
	want := []string{
		"--- PASS: TestParent/sub (0.00s)",
		"--- PASS: TestParent (0.00s)",
		"ok\tpkg\t0.1s",
	}
	if got := runProcessor(in); !reflect.DeepEqual(got, want) {
		t.Errorf("output mismatch:\n got:  %v\n want: %v", got, want)
	}
}

func TestProcessorSkippedLogsKept(t *testing.T) {
	in := []string{
		"=== RUN   TestSkip",
		"=== NAME  TestSkip",
		"    a_test.go:3: reason for skipping",
		"--- SKIP: TestSkip (0.00s)",
		"ok\tpkg\t0.0s",
	}
	want := []string{
		"    a_test.go:3: reason for skipping",
		"--- SKIP: TestSkip (0.00s)",
		"ok\tpkg\t0.0s",
	}
	if got := runProcessor(in); !reflect.DeepEqual(got, want) {
		t.Errorf("output mismatch:\n got:  %v\n want: %v", got, want)
	}
}

func TestProcessorFlushesUnreportedTestAtPackageLine(t *testing.T) {
	// A test that hangs until the timeout panic never gets a --- line. Its
	// held log lines and the panic block must surface at the package summary
	// line, ahead of it, in arrival order. The panic block matches the output
	// of a real timed-out run, including the tab-indented running list.
	in := `
=== RUN  TestHang
    a_test.go:10: log line before the hang
panic: test timed out after 1s
	running tests:
		TestHang (1s)

goroutine 8 [sleep]:
time.Sleep(30 * time.Second)
	/usr/local/go/src/runtime/time.go:368 +0x165
FAIL	example.com/pkg	1.004s
FAIL
`
	wantBlock := `
    a_test.go:10: log line before the hang
panic: test timed out after 1s
	running tests:
		TestHang (1s)

goroutine 8 [sleep]:
time.Sleep(30 * time.Second)
	/usr/local/go/src/runtime/time.go:368 +0x165
FAIL	example.com/pkg	1.004s
FAIL
`
	// Trim cuts only the newlines around the literals, since TrimSpace would
	// also eat the indentation of the first output line.
	got := strings.Join(runProcessor(strings.Split(strings.Trim(in, "\n"), "\n")), "\n")
	want := strings.Trim(wantBlock, "\n")
	if got != want {
		t.Errorf("output mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestProcessorFlushesUnreportedTestAtEndOfOutput(t *testing.T) {
	// A stream can end with no package summary line, for example when the go
	// command is killed. Held lines must still surface when the stream ends.
	in := `
=== RUN  TestHang
    a_test.go:10: log line before the hang
`
	wantBlock := `
    a_test.go:10: log line before the hang
`
	p := newProcessor()
	var got []string
	for line := range strings.SplitSeq(strings.Trim(in, "\n"), "\n") {
		got = append(got, p.Process(line)...)
	}
	held := p.flush()
	got = append(got, held...)
	gotText := strings.Join(got, "\n")
	want := strings.Trim(wantBlock, "\n")
	if gotText != want {
		t.Errorf("output mismatch:\ngot:\n%s\nwant:\n%s", gotText, want)
	}
}

func TestBuildGoTestArgsReplayInjectsVAndDedupes(t *testing.T) {
	got, err := buildGoTestArgs([]string{"./...", "-count=1", "-v", "-timeout", "60s"}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"test", "-v", "./...", "-count=1", "-timeout", "60s"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args mismatch:\n got:  %v\n want: %v", got, want)
	}
}

func TestBuildGoTestArgsReplayRejectsRecord(t *testing.T) {
	if _, err := buildGoTestArgs([]string{"./test/integration/", "-record"}, false); err == nil {
		t.Fatal("want error for -record in replay-only path, got nil")
	}
}

func TestBuildGoTestArgsRecordInjectsAndDedupes(t *testing.T) {
	got, err := buildGoTestArgs([]string{"./test/integration/", "-run", "TestFoo", "-record"}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"test", "-v", "./test/integration/", "-run", "TestFoo", "-record"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args mismatch:\n got:  %v\n want: %v", got, want)
	}
}
