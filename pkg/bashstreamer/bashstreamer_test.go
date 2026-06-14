package bashstreamer

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func TestStreamCommand(t *testing.T) {
	var lines []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scanner := bufio.NewScanner(r.Body)
		for scanner.Scan() {
			mu.Lock()
			lines = append(lines, scanner.Text())
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	code := streamCommand(srv.URL, []string{"-c", "echo hello && echo world"}, os.Stdin, os.Stdout, os.Stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}
	if lines[0] != "hello" {
		t.Errorf("line 0 = %q, want %q", lines[0], "hello")
	}
	if lines[1] != "world" {
		t.Errorf("line 1 = %q, want %q", lines[1], "world")
	}
}

func TestStreamCommandStderr(t *testing.T) {
	var lines []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scanner := bufio.NewScanner(r.Body)
		for scanner.Scan() {
			mu.Lock()
			lines = append(lines, scanner.Text())
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	code := streamCommand(srv.URL, []string{"-c", "echo out && echo err >&2"}, os.Stdin, os.Stdout, os.Stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(lines), lines)
	}
	// Both stdout and stderr lines should arrive (order may vary).
	got := make(map[string]bool)
	for _, l := range lines {
		got[l] = true
	}
	if !got["out"] {
		t.Errorf("missing stdout line %q in %v", "out", lines)
	}
	if !got["err"] {
		t.Errorf("missing stderr line %q in %v", "err", lines)
	}
}

func TestStreamCommandExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	code := streamCommand(srv.URL, []string{"-c", "exit 42"}, os.Stdin, os.Stdout, os.Stderr)
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}

func TestStreamCommandPartialOutput(t *testing.T) {
	// Validate that output arrives at the server while the child is
	// still running, not only after it exits. The child prints "step1",
	// sleeps 3s, then prints "step2". The server signals when "step1"
	// arrives; the test checks that this happens well before the child
	// exits (which would take >= 3s if streaming were broken).
	firstLine := make(chan string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scanner := bufio.NewScanner(r.Body)
		for scanner.Scan() {
			line := scanner.Text()
			select {
			case firstLine <- line:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	done := make(chan int, 1)
	go func() {
		done <- streamCommand(srv.URL, []string{"-c", "echo step1 && sleep 3 && echo step2"}, os.Stdin, os.Stdout, os.Stderr)
	}()

	// "step1" must arrive within 2s. If streaming is broken (cmd.Wait
	// closes pipes before goroutines read), the body will be empty and
	// this will timeout.
	select {
	case line := <-firstLine:
		if line != "step1" {
			t.Fatalf("first line = %q, want %q", line, "step1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first line; streaming is broken")
	}

	// Wait for command to finish and check exit code + second line.
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("streamCommand did not return in time")
	}
}

func TestStreamCommandConnectionRefused(t *testing.T) {
	// When the server is unreachable, streamCommand should still run
	// the command and return its exit code (graceful fallback).
	code := streamCommand("http://127.0.0.1:1", []string{"-c", "echo ok"}, os.Stdin, os.Stdout, os.Stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestStreamCommandConcurrentStdoutStderr(t *testing.T) {
	// Two subshells emit a fixed number of lines to stdout and stderr
	// at the same time, stressing concurrent Writes to the shared
	// io.PipeWriter. io.Pipe serializes Writes, so every line must
	// arrive; a lost write under concurrency would drop the count.
	const perStream = 100
	var n int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scanner := bufio.NewScanner(r.Body)
		for scanner.Scan() {
			mu.Lock()
			n++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cmd := fmt.Sprintf(
		"( for i in $(seq 1 %d); do echo out$i; done ) & "+
			"( for i in $(seq 1 %d); do echo err$i >&2; done ) & wait",
		perStream, perStream)
	code := streamCommand(srv.URL, []string{"-c", cmd}, os.Stdin, os.Stdout, os.Stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := perStream * 2; n != want {
		t.Fatalf("received %d lines, want %d (lines lost under concurrent writes?)", n, want)
	}
}
