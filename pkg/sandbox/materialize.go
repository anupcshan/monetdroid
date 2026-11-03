package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HasFlake reports whether toplevel declares a flake environment.
func HasFlake(toplevel string) bool {
	if toplevel == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(toplevel, "flake.nix"))
	return err == nil
}

// materialize writes the devShell environment script for toplevel into dir and
// creates the profile symlink that roots the closure.
func materialize(ctx context.Context, toplevel, dir string) error {
	envPath := filepath.Join(dir, "env.sh")
	f, err := os.Create(envPath)
	if err != nil {
		return fmt.Errorf("env script: %w", err)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "nix",
		"--extra-experimental-features", "nix-command flakes",
		"print-dev-env", "--profile", filepath.Join(dir, "profile"), ".")
	cmd.Dir = toplevel
	cmd.Stdout = f
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	closeErr := f.Close()
	if runErr != nil {
		return fmt.Errorf("nix print-dev-env in %s: %w: %s", toplevel, runErr, strings.TrimSpace(stderr.String()))
	}
	if closeErr != nil {
		return fmt.Errorf("env script: %w", closeErr)
	}
	return nil
}

// writeWrapper writes the per-session executable that sources the devShell
// environment and then execs the workload. The environment script runs with
// stdin closed and stdout on stderr, so a shellHook cannot consume claude's
// stdin or corrupt its stream-json stdout.
func writeWrapper(dir string, pathDirs []string) (string, error) {
	var quoted []string
	for _, d := range pathDirs {
		if d != "" {
			quoted = append(quoted, shellQuote(d))
		}
	}
	path := filepath.Join(dir, "workload")
	script := fmt.Sprintf(`#!/bin/bash
startdir=$PWD
. "$1" </dev/null >&2 || exit
shift
PATH=%s:"$PATH"
export PATH
export SHELL=/bin/bash
cd "$startdir" || exit
exec "$@"
`, strings.Join(quoted, ":"))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return "", fmt.Errorf("workload wrapper: %w", err)
	}
	return path, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
