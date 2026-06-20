package mdrdev

import (
	"reflect"
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
	want := []string{"test", "-v", "-record", "./test/integration/", "-run", "TestFoo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args mismatch:\n got:  %v\n want: %v", got, want)
	}
}
