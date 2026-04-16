package kb

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type KB struct {
	Path string
}

func Resolve() (*KB, error) {
	if p := os.Getenv("KB_PATH"); p != "" {
		return &KB{Path: p}, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getwd: %w", err)
	}

	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not a git repository")
	}

	gcd := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gcd) {
		gcd = filepath.Join(cwd, gcd)
	}
	gcd = filepath.Clean(gcd)

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	slug := manglePath(gcd)
	return &KB{Path: filepath.Join(home, ".monetdroid", "kb", slug)}, nil
}

func manglePath(p string) string {
	r := strings.NewReplacer("/", "-", ".", "-")
	return r.Replace(p)
}

func (kb *KB) Exists() bool {
	_, err := os.Stat(filepath.Join(kb.Path, ".git"))
	return err == nil
}

func (kb *KB) ReadOnlyPath() string {
	return filepath.Join(kb.Path, ".readonly")
}

func (kb *KB) IsReadOnly() bool {
	_, err := os.Stat(kb.ReadOnlyPath())
	return err == nil
}

func (kb *KB) fullPath(path string) string {
	return filepath.Join(kb.Path, path)
}

func (kb *KB) checkWritable() error {
	if kb.IsReadOnly() {
		return fmt.Errorf("kb is read-only")
	}
	return nil
}

func (kb *KB) ensureRepo() error {
	if kb.Exists() {
		return nil
	}

	if err := os.MkdirAll(kb.Path, 0755); err != nil {
		return err
	}

	cmd := exec.Command("git", "init")
	cmd.Dir = kb.Path
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %s", strings.TrimSpace(string(out)))
	}

	if err := os.WriteFile(filepath.Join(kb.Path, ".gitignore"), []byte(".readonly\n"), 0644); err != nil {
		return err
	}

	return kb.autoCommit("init")
}

func (kb *KB) autoCommit(msg string) error {
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = kb.Path
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %s", strings.TrimSpace(string(out)))
	}

	cmd = exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = kb.Path
	if cmd.Run() == nil {
		return nil
	}

	cmd = exec.Command("git", "commit", "-m", msg)
	cmd.Dir = kb.Path
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
