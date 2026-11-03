package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type mount struct {
	source   string
	readOnly bool
}

// mounts builds the container filesystem allowlist. A later mount with the
// same destination replaces an earlier one, so callers can shadow a standard
// path with a read-only extra mount.
func (cfg Config) mounts(user hostUser, dir string, ssh bool) (map[string]mount, error) {
	mounts := map[string]mount{
		cfg.Worktree: {source: cfg.Worktree},
	}
	if !within(cfg.Worktree, cfg.GitCommonDir) {
		mounts[cfg.GitCommonDir] = mount{source: cfg.GitCommonDir}
	}

	// Transcripts and plans live here, so create it private.
	claudeDir := filepath.Join(user.home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		return nil, fmt.Errorf("claude dir: %w", err)
	}
	mounts[claudeDir] = mount{source: claudeDir}
	for _, p := range []string{
		filepath.Join(user.home, ".claude.json"),
		filepath.Join(user.home, ".gitconfig"),
		filepath.Join(user.home, ".ssh", "known_hosts"),
		filepath.Join(user.home, ".ssh", "config"),
	} {
		ok, err := exists(p)
		if err != nil {
			return nil, err
		}
		if ok {
			mounts[p] = mount{source: p, readOnly: true}
		}
	}

	for _, p := range cfg.PathMounts {
		mounts[p] = mount{source: p, readOnly: true}
	}
	if cfg.BashstreamerDir != "" {
		mounts[cfg.BashstreamerDir] = mount{source: cfg.BashstreamerDir}
	}
	mounts[dir] = mount{source: dir, readOnly: true}
	if cfg.KBDir != "" {
		if err := os.MkdirAll(cfg.KBDir, 0o755); err != nil {
			return nil, fmt.Errorf("kb dir: %w", err)
		}
		mounts[cfg.KBDir] = mount{source: cfg.KBDir}
	}
	if ssh {
		socketDir := filepath.Dir(cfg.SSHAuthSock)
		mounts[socketDir] = mount{source: socketDir}
	}

	ok, err := exists(nixStore)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s does not exist", nixStore)
	}
	mounts[nixStore] = mount{source: nixStore, readOnly: true}

	for _, p := range cfg.ExtraMounts {
		ok, err := exists(p)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("extra mount %s does not exist", p)
		}
		mounts[p] = mount{source: p, readOnly: true}
	}
	mounts[claudeContainerPath] = mount{source: cfg.ClaudeBinaryPath, readOnly: true}
	return mounts, nil
}

func mountArgs(mounts map[string]mount) []string {
	destinations := make([]string, 0, len(mounts))
	for destination := range mounts {
		destinations = append(destinations, destination)
	}
	// Lexicographic order mounts a parent before every path nested in it.
	slices.Sort(destinations)
	args := make([]string, 0, len(destinations)*2)
	for _, destination := range destinations {
		m := mounts[destination]
		spec := "type=bind,source=" + m.source + ",destination=" + destination
		if m.readOnly {
			spec += ",ro"
		}
		args = append(args, "--mount", spec)
	}
	return args
}

// sshSocket reports whether the configured agent socket exists. A stale
// SSH_AUTH_SOCK is dropped instead of failing the session.
func sshSocket(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	if !filepath.IsAbs(path) {
		return false, fmt.Errorf("ssh agent socket %q is not absolute", path)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("ssh agent socket: %w", err)
	}
	return true, nil
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func requireDir(path, name string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %s is not a directory", name, path)
	}
	return nil
}

func requireFile(path, name string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s %s is a directory", name, path)
	}
	return nil
}

// within reports whether child is parent or below it.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
