// Command bashstreamer wraps a shell command in shell mode, streaming
// stdout/stderr to a push URL over a single chunked POST while passing
// it through to the wrapper's own stdout. Signals are forwarded to the
// child process; the exit code matches the child's.
//
// Usage:
//
//	bashstreamer --push-url http://host:port/bash-stream/SESSION/TOOL_USE_ID -- -c "command"
package main

import (
	"fmt"
	"os"

	"github.com/anupcshan/monetdroid/pkg/bashstreamer"
)

func main() {
	if err := bashstreamer.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "bashstreamer: %v\n", err)
		os.Exit(1)
	}
}
