package kb_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestWriteAndRead(t *testing.T) {
	f := Setup(t)

	_, err := f.KBWithStdin("# Hello\nWorld\n", "write", "test.md")
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := f.KB("read", "test.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "# Hello\nWorld" {
		t.Fatalf("unexpected content: %q", out)
	}
}

func TestList(t *testing.T) {
	f := Setup(t)

	out, err := f.KB("list")
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty list, got: %q", out)
	}

	f.KBWithStdin("content", "write", "a.md")
	f.KBWithStdin("content", "write", "b.md")

	out, err = f.KB("list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	files := strings.Split(out, "\n")
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got: %v", files)
	}
}

func TestEdit(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("Hello World\n", "write", "test.md")

	_, err := f.KBWithStdin(`{"old": "World", "new": "KB"}`, "edit", "test.md")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}

	out, err := f.KB("read", "test.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "Hello KB" {
		t.Fatalf("unexpected content: %q", out)
	}
}

func TestEditAll(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("foo bar foo baz foo\n", "write", "test.md")

	_, err := f.KBWithStdin(`{"old": "foo", "new": "qux"}`, "edit", "--all", "test.md")
	if err != nil {
		t.Fatalf("edit --all: %v", err)
	}

	out, err := f.KB("read", "test.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "qux bar qux baz qux" {
		t.Fatalf("unexpected content: %q", out)
	}
}

func TestEditNotUnique(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("foo bar foo\n", "write", "test.md")

	_, err := f.KBWithStdin(`{"old": "foo", "new": "qux"}`, "edit", "test.md")
	if err == nil {
		t.Fatal("expected error for non-unique edit")
	}
}

func TestAppend(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("line1\n", "write", "test.md")
	f.KBWithStdin("line2\n", "append", "test.md")

	out, err := f.KB("read", "test.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "line1\nline2" {
		t.Fatalf("unexpected content: %q", out)
	}
}

func TestAppendCreatesFile(t *testing.T) {
	f := Setup(t)

	_, err := f.KBWithStdin("new content\n", "append", "new.md")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	out, err := f.KB("read", "new.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "new content" {
		t.Fatalf("unexpected content: %q", out)
	}
}

func TestRemove(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("content", "write", "doomed.md")
	_, err := f.KB("rm", "doomed.md")
	if err != nil {
		t.Fatalf("rm: %v", err)
	}

	out, err := f.KB("list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty list after rm, got: %q", out)
	}
}

func TestMove(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("content", "write", "old.md")
	_, err := f.KB("mv", "old.md", "new.md")
	if err != nil {
		t.Fatalf("mv: %v", err)
	}

	out, err := f.KB("list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out != "new.md" {
		t.Fatalf("expected new.md, got: %q", out)
	}
}

func TestSearch(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("alpha beta\ngamma delta\n", "write", "test.md")

	out, err := f.KB("search", "gamma")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out, "gamma delta") {
		t.Fatalf("expected search hit, got: %q", out)
	}
}

func TestReadOnlyMode(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("content", "write", "test.md")

	out, err := f.KBAdmin("mode")
	if err != nil {
		t.Fatalf("mode: %v", err)
	}
	if out != "readwrite" {
		t.Fatalf("expected readwrite, got: %q", out)
	}

	_, err = f.KBAdmin("mode", "readonly")
	if err != nil {
		t.Fatalf("set readonly: %v", err)
	}

	_, err = f.KBWithStdin("more", "write", "test.md")
	if err == nil {
		t.Fatal("expected error writing in readonly mode")
	}

	// Reads should still work.
	out, err = f.KB("read", "test.md")
	if err != nil {
		t.Fatalf("read in readonly: %v", err)
	}
	if out != "content" {
		t.Fatalf("unexpected content: %q", out)
	}

	_, err = f.KBAdmin("mode", "readwrite")
	if err != nil {
		t.Fatalf("set readwrite: %v", err)
	}

	_, err = f.KBWithStdin("updated", "write", "test.md")
	if err != nil {
		t.Fatalf("write after readwrite: %v", err)
	}
}

func TestReadWithOffsetLimit(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("line0\nline1\nline2\nline3\nline4\n", "write", "test.md")

	out, err := f.KB("read", "test.md", "--offset", "1", "--limit", "2")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "line1\nline2" {
		t.Fatalf("unexpected content: %q", out)
	}
}

func TestAutoCommit(t *testing.T) {
	f := Setup(t)

	f.KBWithStdin("v1", "write", "test.md")
	f.KBWithStdin("v2", "write", "test.md")

	// Check that the KB repo has commits.
	kbPath := f.MustExec("sh", "-c", "ls -d /root/.monetdroid/kb/*/")
	commitCount := f.MustExec("git", "-C", strings.TrimSpace(kbPath), "rev-list", "--count", "HEAD")
	// init + .gitignore commit + 2 writes = at least 3
	if commitCount < "3" {
		t.Fatalf("expected at least 3 commits, got: %s", commitCount)
	}
}

func TestGitCommonDirResolution(t *testing.T) {
	f := Setup(t)

	// Write from the main workdir.
	f.KBWithStdin("from main", "write", "test.md")

	// Create a worktree and read from there — should see the same KB.
	f.MustExec("git", "-C", containerWorkdir, "worktree", "add", "/work2", "-b", "test-branch")

	cmdArgs := []string{"exec", "-e", "KB_CLI_MODE=kb", "-w", "/work2", f.containerID, "/test", "read", "test.md"}
	out, err := exec.Command("docker", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("read from worktree: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "from main" {
		t.Fatalf("worktree should see same KB, got: %q", string(out))
	}
}
