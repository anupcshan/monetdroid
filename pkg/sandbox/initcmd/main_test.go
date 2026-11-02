package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain runs the init when a test re-executes this binary with
// SANDBOX_INIT_MODE set, and the tests otherwise.
func TestMain(m *testing.M) {
	if os.Getenv("SANDBOX_INIT_MODE") == "1" {
		runInit()
		return
	}
	os.Exit(m.Run())
}

// proc is one running sandbox-init. write is the test's end of the pipe whose
// read end the init holds as fd 3.
type proc struct {
	cmd    *exec.Cmd
	write  *os.File
	stderr *bytes.Buffer
}

// start runs the init in a child process with a pipe on fd 3.
func start(t *testing.T, grace time.Duration, command ...string) *proc {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		r.Close()
		w.Close()
		t.Fatal(err)
	}
	args := append([]string{"-grace", grace.String(), "--"}, command...)
	cmd := exec.Command(self, args...)
	cmd.Env = append(os.Environ(), "SANDBOX_INIT_MODE=1")
	cmd.ExtraFiles = []*os.File{r}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		t.Fatalf("start: %v", err)
	}
	r.Close()
	p := &proc{cmd: cmd, write: w, stderr: stderr}
	t.Cleanup(func() {
		w.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return p
}

// wait waits up to timeout for the init to exit and returns its status. It
// kills the init and fails the test at the timeout.
func (p *proc) wait(t *testing.T, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return 0
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("wait: %v", err)
		}
		return exitErr.ExitCode()
	case <-time.After(timeout):
		p.cmd.Process.Kill()
		<-done
		t.Fatalf("init still running after %v. stderr:\n%s", timeout, p.stderr)
		return -1
	}
}

// waitForFile waits until the workload has created path.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workload never created %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPipeCloseSignalsWorkload verifies that closing the write end sends
// SIGTERM to the workload, which exits well before the grace deadline.
func TestPipeCloseSignalsWorkload(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ready")
	p := start(t, 5*time.Second, "sh", "-c",
		fmt.Sprintf(`trap 'exit 0' TERM; touch '%s'; while :; do sleep 1; done`, marker))
	waitForFile(t, marker, 5*time.Second)
	p.write.Close()
	code := p.wait(t, 5*time.Second)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0. stderr:\n%s", code, p.stderr)
	}
}

// TestGraceExpiryKillsWorkload verifies that a workload that ignores SIGTERM is
// killed when the grace period ends and the init reports the kill.
func TestGraceExpiryKillsWorkload(t *testing.T) {
	grace := 300 * time.Millisecond
	marker := filepath.Join(t.TempDir(), "ready")
	p := start(t, grace, "sh", "-c",
		fmt.Sprintf(`trap "" TERM; touch '%s'; while :; do sleep 1; done`, marker))
	waitForFile(t, marker, 5*time.Second)
	started := time.Now()
	p.write.Close()
	code := p.wait(t, 5*time.Second)
	if want := 128 + int(syscall.SIGKILL); code != want {
		t.Fatalf("exit code = %d, want %d. stderr:\n%s", code, want, p.stderr)
	}
	if elapsed := time.Since(started); elapsed < grace {
		t.Fatalf("init exited after %v, want at least the %v grace period", elapsed, grace)
	}
}

// TestForwardsSignal verifies that a signal sent to the init reaches the
// workload's process group.
func TestForwardsSignal(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ready")
	p := start(t, 5*time.Second, "sh", "-c",
		fmt.Sprintf(`trap 'exit 5' TERM; touch '%s'; while :; do sleep 1; done`, marker))
	waitForFile(t, marker, 5*time.Second)
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code := p.wait(t, 5*time.Second)
	if code != 5 {
		t.Fatalf("exit code = %d, want the workload's 5. stderr:\n%s", code, p.stderr)
	}
}

// TestRejectsNonPipe verifies that the init refuses to run without a pipe on
// fd 3.
func TestRejectsNonPipe(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-grace", "1s", "--", "true")
	cmd.Env = append(os.Environ(), "SANDBOX_INIT_MODE=1")
	cmd.ExtraFiles = []*os.File{devNull}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("exit = %v, want 1. stderr:\n%s", err, stderr)
	}
	if !strings.Contains(stderr.String(), "not a pipe or socket") {
		t.Fatalf("stderr = %q, want the fd validation message", stderr.String())
	}
}
