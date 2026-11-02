// sandbox-init is PID 1 in a sandbox container. It runs the workload and ends
// the container when the process that launched it dies.
//
// fd 3 is the read end of a pipe held open by the launcher, which never writes
// to it. When the launcher dies, including by SIGKILL, the kernel closes the
// write end and the read here returns EOF. The init then sends SIGTERM to the
// workload's process group, waits for the grace period, and exits. As PID 1,
// its exit kills whatever remains in the container.
//
// It also forwards signals to the workload's group and reaps exited children.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const pipeFD = 3

func main() {
	runInit()
}

// runInit is the entry point of the init binary. main and the test binary both
// call it, the latter when SANDBOX_INIT_MODE is set.
func runInit() {
	log.SetPrefix("sandbox-init: ")
	log.SetFlags(0)

	fs := flag.NewFlagSet("sandbox-init", flag.ContinueOnError)
	grace := fs.Duration("grace", 10*time.Second, "time the workload has to exit after the launcher dies")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}
	if fs.NArg() == 0 {
		log.Fatal("usage: sandbox-init [-grace 10s] -- command [args...]")
	}
	pipe, err := openPipe(pipeFD)
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(run(pipe, *grace, fs.Args()))
}

// run starts argv and returns the status to exit with. It returns when the
// workload exits or when the grace period after the pipe closes ends.
func run(pipe *os.File, grace time.Duration, argv []string) int {
	if os.Getpid() != 1 {
		log.Print("warning: not PID 1, so processes outside the workload's group survive its exit")
	}

	// Register before starting the workload so an early SIGCHLD is not lost.
	// Several children may exit together, and one notification covers them all.
	sigs := make(chan os.Signal, 32)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT,
		syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The own process group lets signals reach the workload and its children
	// without reaching PID 1.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		code := 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		log.Printf("start %s: %v", argv[0], err)
		return code
	}
	pgid := cmd.Process.Pid
	// Never call cmd.Wait. Reap collects every child.

	launcherGone := make(chan struct{})
	go func() {
		// The launcher never writes, so this blocks until EOF. A read error
		// is treated the same way, so the container fails closed.
		io.Copy(io.Discard, pipe)
		close(launcherGone)
	}()

	var deadline <-chan time.Time
	for {
		select {
		case <-launcherGone:
			launcherGone = nil
			log.Printf("launcher gone, sending SIGTERM with %v grace", grace)
			signalGroup(pgid, syscall.SIGTERM)
			deadline = time.After(grace)

		case <-deadline:
			log.Print("grace period over, killing the workload")
			signalGroup(pgid, syscall.SIGKILL)
			return 128 + int(syscall.SIGKILL)

		case sig := <-sigs:
			if sig != syscall.SIGCHLD {
				signalGroup(pgid, sig.(syscall.Signal))
				continue
			}
			if status, ok := reap(pgid); ok {
				return status
			}
		}
	}
}

// openPipe checks that fd is an inherited pipe or socket and marks it
// close-on-exec so the workload does not inherit it.
func openPipe(fd int) (*os.File, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("fd %d is not open (%v). Pass a pipe with podman run --preserve-fds=1", fd, err)
	}
	if mode := st.Mode & syscall.S_IFMT; mode != syscall.S_IFIFO && mode != syscall.S_IFSOCK {
		return nil, fmt.Errorf("fd %d is not a pipe or socket", fd)
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "pipe"), nil
}

// reap reports the workload's exit status once it has been collected. Other
// children are reaped and discarded.
func reap(workload int) (int, bool) {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if pid <= 0 {
			// 0 means nothing else has exited. -1 means no children left.
			return 0, false
		}
		if pid == workload {
			if ws.Signaled() {
				return 128 + int(ws.Signal()), true
			}
			return ws.ExitStatus(), true
		}
	}
}

func signalGroup(pgid int, sig syscall.Signal) {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("signal %v: %v", sig, err)
	}
}
