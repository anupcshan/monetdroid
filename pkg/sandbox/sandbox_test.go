package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHasFlake(t *testing.T) {
	dir := t.TempDir()
	if HasFlake(dir) {
		t.Fatal("reported a flake without flake.nix")
	}
	if err := os.WriteFile(filepath.Join(dir, "flake.nix"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasFlake(dir) {
		t.Fatal("missed flake.nix")
	}
}

func TestWrapperQuotesPathDirs(t *testing.T) {
	dir := t.TempDir()
	weird := filepath.Join(t.TempDir(), "with space and 'quote")
	if err := os.MkdirAll(weird, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper, err := writeWrapper(dir, []string{weird, "", "/usr/local/bin"})
	if err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "env.sh")
	if err := os.WriteFile(envPath, []byte("export SANDBOX_TEST_VALUE=from-env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(wrapper, envPath, "sh", "-c",
		`printf '%s\n%s\n' "${PATH%%:*}" "$SANDBOX_TEST_VALUE"`).Output()
	if err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrapper output %q", out)
	}
	if lines[0] != weird {
		t.Errorf("first PATH entry = %q, want %q", lines[0], weird)
	}
	if lines[1] != "from-env" {
		t.Errorf("sourced value = %q, want %q", lines[1], "from-env")
	}
}

// TestSandboxPrepareAndRun drives the primitive with real podman and Nix. The
// workload requires the devShell, mount policy, a linked-worktree commit, and
// host networking. It then sleeps so the test can close the launcher pipe and
// check teardown.
func TestSandboxPrepareAndRun(t *testing.T) {
	const workload = `#!/bin/bash
set -e
test "$HOME" = "$SANDBOX_TEST_HOME"
test -f "$SANDBOX_TEST_RO_MARKER"
test -r "$SANDBOX_TEST_RO_MARKER"
test ! -w "$SANDBOX_TEST_RO_MARKER"
test ! -d "$SANDBOX_TEST_HOME_MARKER"
echo change > from-container.txt
git add from-container.txt
git -c user.name=Sandbox -c user.email=sandbox@example.com commit -q -m "from container"
curl -fsS --max-time 10 "$SANDBOX_TEST_URL/" >/dev/null
touch ready
sleep 300
`

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	base := t.TempDir()
	mainDir := filepath.Join(base, "main")
	worktree := filepath.Join(base, "worktree")

	run(t, "git", "init", "-q", mainDir)
	writeFlake(t, mainDir)
	if err := os.WriteFile(filepath.Join(mainDir, "README.md"), []byte("sandbox test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", mainDir, "add", ".")
	run(t, "git", "-C", mainDir, "-c", "user.name=Sandbox Test", "-c", "user.email=sandbox@example.com", "commit", "-q", "-m", "init")
	run(t, "git", "-C", mainDir, "worktree", "add", "-q", "-b", "sandbox", worktree)

	user, err := hostIdentity()
	if err != nil {
		t.Fatal(err)
	}
	modelDir := filepath.Join(base, "models")
	roDir := filepath.Join(base, "ro")
	for _, d := range []string{modelDir, roDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(base, "claude"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(modelDir, "test-model"), workload)
	roMarker := filepath.Join(roDir, "marker")
	if err := os.WriteFile(roMarker, []byte("read only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homeMarker, err := os.MkdirTemp(user.home, ".monetdroid-sandbox-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(homeMarker) })

	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer host.Close()

	podmanRun, err := Prepare(ctx, Config{
		Worktree:         worktree,
		GitCommonDir:     filepath.Join(mainDir, ".git"),
		Cwd:              worktree,
		ClaudeBinaryPath: filepath.Join(base, "claude"),
		PathMounts:       []string{modelDir},
		ExecutableName:   "test-model",
		ExtraEnv: []string{
			"SANDBOX_TEST_HOME=" + user.home,
			"SANDBOX_TEST_RO_MARKER=" + roMarker,
			"SANDBOX_TEST_HOME_MARKER=" + homeMarker,
			"SANDBOX_TEST_URL=" + host.URL,
		},
		ExtraMounts: []string{roDir},
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Cleanup(podmanRun.Cleanup)

	cmd := exec.Command(podmanRun.Args[0], podmanRun.Args[1:]...)
	cmd.ExtraFiles = []*os.File{podmanRun.LauncherPipe}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })

	waitForFile(t, filepath.Join(worktree, "ready"), time.Minute)
	cid, err := os.ReadFile(podmanRun.ContainerIDFile)
	if err != nil {
		t.Fatal(err)
	}

	podmanRun.Stop()
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(Grace + 10*time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("podman did not exit after the launcher pipe closed")
	}

	if err := waitForContainerGone(podmanRun.Args[0], strings.TrimSpace(string(cid)), 5*time.Second); err != nil {
		t.Error(err)
	}
	podmanRun.Cleanup()
	if _, err := os.Stat(podmanRun.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("session dir still exists after cleanup: %v", err)
	}
}

func waitForContainerGone(podman, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := exec.Command(podman, "container", "exists", id).Run()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return nil
			}
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s still exists", id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func writeFlake(t *testing.T, dir string) {
	t.Helper()
	system := map[string]string{
		"amd64": "x86_64-linux",
		"arm64": "aarch64-linux",
	}[runtime.GOARCH]
	if system == "" {
		t.Fatalf("unsupported GOARCH %s", runtime.GOARCH)
	}
	flake, err := os.ReadFile(filepath.Join("testdata", "flake", "flake.nix"))
	if err != nil {
		t.Fatal(err)
	}
	flake = []byte(strings.ReplaceAll(string(flake), "@SYSTEM@", system))
	if err := os.WriteFile(filepath.Join(dir, "flake.nix"), flake, 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := os.ReadFile(filepath.Join("testdata", "flake", "flake.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flake.lock"), lock, 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
