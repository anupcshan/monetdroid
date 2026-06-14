// Package bashstreamer wraps a shell command in shell mode, streaming
// stdout/stderr to a push URL over a single chunked POST while passing
// it through to the wrapper's own stdout. Signals are forwarded to the
// child process; the exit code matches the child's.
package bashstreamer

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// maxStreamLineBytes caps a single output line read from the child. A
// line longer than this ends the scan and is reported as a truncation
// rather than dropped silently. See streamToPipe.
const maxStreamLineBytes = 1024 * 1024

// Run executes the bashstreamer CLI in shell mode. It is called from
// both the standalone binary and the test multi-call binary. args is
// os.Args[1:]; the command to stream follows "--" (e.g.
// ["--push-url", URL, "--", "-c", "command"]).
func Run(args []string) error {
	fs := flag.NewFlagSet("bashstreamer", flag.ContinueOnError)
	pushURL := fs.String("push-url", "", "URL to POST the streamed output to")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: bashstreamer --push-url URL -- -c \"command\"\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	RunShell(*pushURL, fs.Args())
	return nil
}

// RunShell is the shell-replacement entry point. It connects to the
// monetdroid streaming endpoint, forks /bin/bash with the given args,
// and streams output line by line. Falls back to exec'ing real bash
// without streaming if pushURL is empty.
func RunShell(pushURL string, args []string) {
	if pushURL == "" {
		execBash(args)
		return
	}
	os.Exit(streamCommand(pushURL, args, os.Stdin, os.Stdout, os.Stderr))
}

// streamCommand forks /bin/bash with the given args, connects to
// pushURL, and streams stdout/stderr line by line over a single
// chunked POST. Returns the child's exit code.
func streamCommand(pushURL string, args []string, stdin io.Reader, consoleOut, consoleErr *os.File) int {
	pr, pw := io.Pipe()

	// Build the request before starting the child or any goroutine. On
	// failure nothing else is running, so the only pw.Close() in the
	// streaming lifecycle is the closer goroutine below: there is no
	// second close needed to unblock deadlocked writers.
	req, err := http.NewRequest("POST", pushURL, pr)
	if err != nil {
		pr.Close()
		pw.Close()
		return 1
	}
	req.Header.Set("Transfer-Encoding", "chunked")
	client := &http.Client{Timeout: 0}

	cmd := exec.Command("/bin/bash", args...)
	cmd.Stdin = stdin

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		pr.Close()
		pw.Close()
		return 1
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		pr.Close()
		pw.Close()
		return 1
	}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return 1
	}

	// Both goroutines write to the same pw concurrently. io.Pipe
	// serializes concurrent Writes, so each line is delivered whole
	// and the bytes reach the HTTP body in write order; only the
	// stdout/stderr interleaving is nondeterministic.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		streamToPipe(pw, consoleOut, stdout)
	}()
	go func() {
		defer wg.Done()
		streamToPipe(pw, consoleErr, stderr)
	}()
	// Single owner of pw.Close(): once both streamers finish, close so
	// the HTTP client sees EOF on the body.
	go func() {
		wg.Wait()
		pw.Close()
	}()

	// client.Do blocks until pw is closed (EOF on the body), so run it
	// in its own goroutine; the main goroutine drains the streamers
	// (wg.Wait) before cmd.Wait closes the stdout/stderr pipes.
	type httpResult struct {
		resp *http.Response
		err  error
	}
	ch := make(chan httpResult, 1)
	go func() {
		r, err := client.Do(req)
		ch <- httpResult{r, err}
	}()

	// Forward signals to the child process.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigs {
			if cmd.Process != nil {
				cmd.Process.Signal(sig)
			}
		}
	}()

	// Drain stdout/stderr before cmd.Wait closes those pipes.
	wg.Wait()

	cmd.Wait()
	signal.Stop(sigs)
	close(sigs)

	if hr := <-ch; hr.resp != nil {
		hr.resp.Body.Close()
	}

	return exitCode(cmd)
}

// exitCode extracts the exit code from a finished command.
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

// streamToPipe reads r line by line, writes each line to the console
// (out) and to the streaming pipe (pw). A line exceeding
// maxStreamLineBytes ends the scan; the truncation is reported on the
// console and through the pipe so the receiver sees why output stopped.
func streamToPipe(pw *io.PipeWriter, out *os.File, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLineBytes)
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		out.WriteString(line)
		pw.Write([]byte(line))
	}
	if err := scanner.Err(); err != nil {
		msg := fmt.Sprintf("bashstreamer: output truncated, a line exceeded %d bytes\n", maxStreamLineBytes)
		fmt.Fprint(os.Stderr, msg)
		pw.Write([]byte(msg))
	}
}

// execBash replaces the current process with /bin/bash and the given
// args. Used as a fallback when streaming is not available.
func execBash(args []string) {
	binary, err := exec.LookPath("/bin/bash")
	if err != nil {
		binary = "/bin/bash"
	}
	argv := append([]string{binary}, args...)
	syscall.Exec(binary, argv, os.Environ())
}
