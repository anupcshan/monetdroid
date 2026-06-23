package mdrdev

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
)

// These regexes duplicate pkg/monetdroid/extractor.go's go test parser (kept
// separate so this dev tool does not import the web app). Change both
// together. contRe and nameRe are added here: the extractor never needed to
// attribute interleaved lines, but itest does.
var (
	runRe   = regexp.MustCompile(`^=== RUN\s+(.+)$`)
	pauseRe = regexp.MustCompile(`^=== PAUSE\s+`)
	contRe  = regexp.MustCompile(`^=== CONT\s+(.+)$`)
	nameRe  = regexp.MustCompile(`^=== NAME\s+(.+)$`)
	// Leading \s* matches indented subtest result lines.
	endRe     = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (.+) \(([\d.]+)s\)$`)
	pkgOkRe   = regexp.MustCompile(`^ok\s+(\S+)\s+([\d.]+)s`)
	pkgFailRe = regexp.MustCompile(`^FAIL\s+(\S+)\s+([\d.]+)s`)
)

// processor de-interleaves and condenses `go test -v` output. Each test's log
// lines are buffered and emitted as one contiguous block at that test's `---`
// completion line; passing-test logs are dropped, failing and skipped logs are
// kept. Because output is buffered per test anyway, de-interleaving parallel
// tests is the same mechanism, not a second pass.
type processor struct {
	// current is the test that owns subsequently arriving log lines, set by
	// the last RUN/CONT/NAME marker or restored to a parent on subtest exit.
	current string
	// buffers holds held log lines per test name, in arrival order.
	buffers map[string][]string
}

func newProcessor() *processor {
	return &processor{buffers: make(map[string][]string)}
}

// Process classifies one input line (no trailing newline) and returns the
// lines to emit now (also without trailing newlines).
func (p *processor) Process(line string) []string {
	if m := runRe.FindStringSubmatch(line); m != nil {
		p.current = m[1]
		p.ensure(m[1])
		return nil
	}
	if pauseRe.MatchString(line) {
		return nil
	}
	if m := contRe.FindStringSubmatch(line); m != nil {
		p.current = m[1]
		p.ensure(m[1])
		return nil
	}
	if m := nameRe.FindStringSubmatch(line); m != nil {
		p.current = m[1]
		p.ensure(m[1])
		return nil
	}
	if m := endRe.FindStringSubmatch(line); m != nil {
		status, name := m[1], m[2]
		held := p.buffers[name]
		delete(p.buffers, name)
		// On subtest completion control returns to the parent test, so set
		// current back to it; the parent's own log lines then buffer under
		// the parent rather than the now-finished subtest.
		if parent, ok := parentOf(name); ok {
			p.current = parent
		} else {
			p.current = ""
		}
		if status == "PASS" {
			// Passing logs are dropped; only the roster line is kept.
			return []string{line}
		}
		return append(held, line)
	}
	if pkgOkRe.MatchString(line) || pkgFailRe.MatchString(line) {
		return []string{line}
	}
	// A log line: hold it under the current test if that test is still
	// active. Otherwise (package-level output, panics, coverage, the bare
	// trailing FAIL) pass it straight through.
	if p.current != "" {
		if _, active := p.buffers[p.current]; active {
			p.buffers[p.current] = append(p.buffers[p.current], line)
			return nil
		}
	}
	return []string{line}
}

// ensure records that a test name has started, with an empty buffer, so later
// log lines are recognized as belonging to an active test.
func (p *processor) ensure(name string) {
	if _, ok := p.buffers[name]; !ok {
		p.buffers[name] = nil
	}
}

func parentOf(name string) (string, bool) {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i], true
	}
	return "", false
}

const maxLineBytes = 1024 * 1024

// runTest runs `go test -v`, condensing and de-interleaving the output, and
// returns go test's exit code. The -v flag is injected here; args are forwarded
// to go test as a Go slice (no shell, so no argument injection). go test runs
// in its own process group and received signals are forwarded to that group so
// a kill reaches the test binary and any processes it spawned.
//
// When record is false this is the replay-only path and -record is rejected,
// so the command structurally cannot record cassettes; recording is the
// separate record-cassette subcommand. When record is true -record is injected
// here.
func runTest(args []string, record bool) int {
	goArgs, err := buildGoTestArgs(args, record)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mdrdev: %v\n", err)
		return 2
	}
	cmd := exec.Command("go", goArgs...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mdrdev itest: %v\n", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "mdrdev itest: %v\n", err)
		return 1
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigs {
			if cmd.Process == nil {
				continue
			}
			if s, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(-cmd.Process.Pid, s)
			}
		}
	}()

	proc := newProcessor()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
	for scanner.Scan() {
		for _, out := range proc.Process(scanner.Text()) {
			fmt.Println(out)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "mdrdev itest: output truncated, a line exceeded %d bytes\n", maxLineBytes)
	}

	signal.Stop(sigs)
	close(sigs)
	_ = cmd.Wait()
	return exitCode(cmd)
}

// buildGoTestArgs injects -v and forwards the rest. Any -v the caller passed is
// dropped so it is not duplicated. In the replay-only path (record false) a
// caller-supplied -record is an error, since that path must be unable to record
// cassettes; in the record path (record true) -record is injected here and any
// caller copy is dropped.
func buildGoTestArgs(forwarded []string, record bool) ([]string, error) {
	args := []string{"test", "-v"}
	for _, a := range forwarded {
		switch {
		case a == "-v" || a == "-test.v":
			continue
		case isRecordFlag(a):
			if !record {
				return nil, fmt.Errorf("-record is not allowed with 'mdrdev test'; use 'mdrdev record-cassette' to record cassettes")
			}
			continue
		}
		args = append(args, a)
	}
	if record {
		// -record must follow the package argument. Placed before the package,
		// go test consumes the package path as -record's value and tests the
		// current directory instead of the requested package.
		args = append(args, "-record")
	}
	return args, nil
}

// isRecordFlag reports whether a is the -record test-binary flag in any of its
// forms.
func isRecordFlag(a string) bool {
	return a == "-record" || a == "--record" ||
		strings.HasPrefix(a, "-record=") || strings.HasPrefix(a, "--record=")
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 0
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if status.Signaled() {
			return 128 + int(status.Signal())
		}
		return status.ExitStatus()
	}
	if !cmd.ProcessState.Success() {
		return 1
	}
	return 0
}
