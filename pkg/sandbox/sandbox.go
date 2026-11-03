// Package sandbox prepares and launches a Claude session inside a rootless
// Podman container. The container sees an allowlist of host paths and an
// explicit environment. Project tools come from the repository's flake
// devShell, materialized on the host before the container starts.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Grace is how long the container's init waits for the workload to exit
	// after the launcher pipe closes, before it kills the workload.
	Grace = 10 * time.Second

	initContainerPath   = "/usr/local/bin/sandbox-init"
	claudeContainerPath = "/usr/local/bin/claude"
	containerPath       = "/usr/local/bin:/usr/bin:/bin"
	nixStore            = "/nix/store"
)

// Config describes the host paths and session values one sandbox container
// needs. Paths must be absolute and keep their host value inside the
// container.
type Config struct {
	Worktree string // repository worktree toplevel, mounted read-write

	// GitCommonDir is git rev-parse --git-common-dir. A normal checkout's
	// .git is under Worktree and covered by the worktree mount. A linked
	// worktree's common dir is the main repository's .git and is mounted
	// separately.
	GitCommonDir string

	Cwd string // process working directory inside Worktree

	// ClaudeBinaryPath is mounted at /usr/local/bin/claude.
	ClaudeBinaryPath string

	// PathMounts are mounted read-only at their host paths and placed on
	// PATH. A directory contributes itself. A file contributes its directory.
	PathMounts []string

	KBDir           string // repository kb store, mounted read-write, created when missing
	BashstreamerDir string // per-session bashstreamer files, mounted read-write

	// ExecutableName is the name of the program run in the container,
	// resolved on PATH. Callers append its arguments after PodmanRun.Args.
	ExecutableName string

	// SSHAuthSock, when set and present, has its directory mounted
	// read-write and is passed to the container.
	SSHAuthSock string

	ExtraEnv    []string // KEY=VALUE entries passed to the container with -e
	ExtraMounts []string // additional read-only host paths
}

// PodmanRun is one prepared podman run and the host resources it owns.
type PodmanRun struct {
	// Args is the podman argument vector, ending with the executable name.
	Args []string
	// LauncherPipe is the read end passed to the container as fd 3.
	LauncherPipe *os.File
	// ContainerIDFile is where podman writes the container ID.
	ContainerIDFile string

	dir      string
	stopPipe *os.File
	cleanup  sync.Once
}

// Stop closes the launcher pipe. The container's init sees EOF and stops the
// workload. Safe to call more than once.
func (r *PodmanRun) Stop() {
	r.stopPipe.Close()
}

// Cleanup removes the session directory. It must run after the container has
// exited so the devShell GC root outlives the workload. Safe to call more than
// once.
func (r *PodmanRun) Cleanup() {
	r.cleanup.Do(func() {
		r.Stop()
		r.LauncherPipe.Close()
		os.RemoveAll(r.dir)
	})
}

// Prepare builds the sandbox image, materializes the flake environment in
// cfg.Worktree, and returns the podman run for one session. It does not start
// anything. Call Cleanup after the container exits.
func Prepare(ctx context.Context, cfg Config) (*PodmanRun, error) {
	if !filepath.IsAbs(cfg.GitCommonDir) {
		cfg.GitCommonDir = filepath.Join(cfg.Cwd, cfg.GitCommonDir)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	user, err := hostIdentity()
	if err != nil {
		return nil, err
	}
	podman, err := exec.LookPath("podman")
	if err != nil {
		return nil, fmt.Errorf("podman: %w", err)
	}
	if err := buildImage(ctx, podman, user); err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "monetdroid-env-")
	if err != nil {
		return nil, fmt.Errorf("session env dir: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dir)
		}
	}()

	if err := materialize(ctx, cfg.Worktree, dir); err != nil {
		return nil, err
	}
	paths, err := cfg.pathDirs()
	if err != nil {
		return nil, err
	}
	wrapper, err := writeWrapper(dir, paths)
	if err != nil {
		return nil, err
	}
	ssh, err := sshSocket(cfg.SSHAuthSock)
	if err != nil {
		return nil, err
	}
	mounts, err := cfg.mounts(user, dir, ssh)
	if err != nil {
		return nil, err
	}
	cidfile := filepath.Join(dir, "cid")

	args := []string{podman, "run",
		"--rm",
		"-i",
		"--network=host",
		"--userns=keep-id",
		"--preserve-fds=1",
		"--security-opt=label=disable",
		"--workdir", cfg.Cwd,
		"--cidfile", cidfile,
	}
	args = append(args, mountArgs(mounts)...)
	for _, e := range cfg.environment(user, ssh) {
		args = append(args, "-e", e)
	}
	args = append(args, sandboxImage,
		initContainerPath,
		"--grace", Grace.String(),
		"--",
		wrapper,
		filepath.Join(dir, "env.sh"),
		cfg.ExecutableName,
	)

	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("launcher pipe: %w", err)
	}
	ok = true
	return &PodmanRun{
		Args:            args,
		LauncherPipe:    r,
		ContainerIDFile: cidfile,
		dir:             dir,
		stopPipe:        w,
	}, nil
}

func (cfg Config) validate() error {
	for _, p := range []struct {
		name string
		path string
	}{
		{"worktree", cfg.Worktree},
		{"cwd", cfg.Cwd},
		{"git common dir", cfg.GitCommonDir},
		{"claude binary", cfg.ClaudeBinaryPath},
	} {
		if p.path == "" {
			return fmt.Errorf("%s is required", p.name)
		}
		if !filepath.IsAbs(p.path) {
			return fmt.Errorf("%s %q is not absolute", p.name, p.path)
		}
	}
	for _, p := range []struct{ name, path string }{
		{"kb dir", cfg.KBDir},
		{"bashstreamer dir", cfg.BashstreamerDir},
		{"ssh agent socket", cfg.SSHAuthSock},
	} {
		if p.path != "" && !filepath.IsAbs(p.path) {
			return fmt.Errorf("%s %q is not absolute", p.name, p.path)
		}
	}
	for _, p := range cfg.PathMounts {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("path mount %q is not absolute", p)
		}
	}
	if cfg.ExecutableName == "" {
		return fmt.Errorf("executable name is required")
	}
	if strings.ContainsRune(cfg.ExecutableName, filepath.Separator) {
		return fmt.Errorf("executable %q is a path, want a name on PATH", cfg.ExecutableName)
	}
	for _, m := range cfg.ExtraMounts {
		if !filepath.IsAbs(m) {
			return fmt.Errorf("extra mount %q is not absolute", m)
		}
	}
	for _, e := range cfg.ExtraEnv {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("extra env %q is not KEY=VALUE", e)
		}
	}

	if err := requireDir(cfg.Worktree, "worktree"); err != nil {
		return err
	}
	if err := requireDir(cfg.GitCommonDir, "git common dir"); err != nil {
		return err
	}
	if cfg.BashstreamerDir != "" {
		if err := requireDir(cfg.BashstreamerDir, "bashstreamer dir"); err != nil {
			return err
		}
	}
	if !within(cfg.Worktree, cfg.Cwd) {
		return fmt.Errorf("cwd %s is outside worktree %s", cfg.Cwd, cfg.Worktree)
	}
	if err := requireFile(cfg.ClaudeBinaryPath, "claude binary"); err != nil {
		return err
	}
	return nil
}

func (cfg Config) pathDirs() ([]string, error) {
	dirs := make([]string, 0, len(cfg.PathMounts)+1)
	for _, p := range cfg.PathMounts {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("path mount %s: %w", p, err)
		}
		if info.IsDir() {
			dirs = append(dirs, p)
		} else {
			dirs = append(dirs, filepath.Dir(p))
		}
	}
	return append(dirs, "/usr/local/bin"), nil
}

func (cfg Config) environment(user hostUser, ssh bool) []string {
	env := []string{
		"HOME=" + user.home,
		"USER=" + user.name,
		"LOGNAME=" + user.name,
		"PATH=" + containerPath,
		"LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
	}
	if ssh {
		env = append(env, "SSH_AUTH_SOCK="+cfg.SSHAuthSock)
	}
	return append(env, cfg.ExtraEnv...)
}
