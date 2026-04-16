package kb_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/anupcshan/monetdroid/pkg/kbadmin"
	"github.com/anupcshan/monetdroid/pkg/kbcli"
)

const dockerImage = "kb-test"
const containerWorkdir = "/work"

var buildOnce sync.Once
var buildErr error

func TestMain(m *testing.M) {
	switch os.Getenv("KB_CLI_MODE") {
	case "kb":
		if err := kbcli.NewApp().Run(context.Background(), os.Args); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	case "kbadmin":
		if err := kbadmin.NewApp().Run(context.Background(), os.Args); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	os.Exit(m.Run())
}

func buildDockerImage(t *testing.T) {
	t.Helper()
	buildOnce.Do(func() {
		cmd := exec.Command("docker", "build", "-t", dockerImage, "test/integration/kb")
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("docker build failed: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal("not in a git repo")
	}
	return strings.TrimSpace(string(out))
}

type Fixture struct {
	T           *testing.T
	containerID string
}

func Setup(t *testing.T) *Fixture {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker not found")
	}

	buildDockerImage(t)

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	out, err := exec.Command("docker", "run", "--rm", "-d",
		"-v", testBinary+":/test:ro",
		dockerImage,
		"sleep", "300",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	containerID := strings.TrimSpace(string(out))
	t.Logf("started container %s", containerID[:12])

	f := &Fixture{T: t, containerID: containerID}

	f.MustExec("git", "config", "--global", "user.email", "test@test.com")
	f.MustExec("git", "config", "--global", "user.name", "Test")
	f.MustExec("git", "init", containerWorkdir)
	f.MustExec("git", "-C", containerWorkdir, "commit", "--allow-empty", "-m", "init")

	t.Cleanup(func() {
		exec.Command("docker", "stop", "-t", "2", containerID).Run()
	})

	return f
}

func (f *Fixture) KB(args ...string) (string, error) {
	f.T.Helper()
	cmdArgs := []string{"exec", "-e", "KB_CLI_MODE=kb", "-w", containerWorkdir, f.containerID, "/test"}
	cmdArgs = append(cmdArgs, args...)
	out, err := exec.Command("docker", cmdArgs...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *Fixture) KBAdmin(args ...string) (string, error) {
	f.T.Helper()
	cmdArgs := []string{"exec", "-e", "KB_CLI_MODE=kbadmin", "-w", containerWorkdir, f.containerID, "/test"}
	cmdArgs = append(cmdArgs, args...)
	out, err := exec.Command("docker", cmdArgs...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *Fixture) KBWithStdin(stdin string, args ...string) (string, error) {
	f.T.Helper()
	cmdArgs := []string{"exec", "-i", "-e", "KB_CLI_MODE=kb", "-w", containerWorkdir, f.containerID, "/test"}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command("docker", cmdArgs...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *Fixture) MustExec(args ...string) string {
	f.T.Helper()
	cmdArgs := append([]string{"exec", f.containerID}, args...)
	out, err := exec.Command("docker", cmdArgs...).CombinedOutput()
	if err != nil {
		f.T.Fatalf("exec %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
