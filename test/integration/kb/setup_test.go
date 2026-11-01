package kb_test

import (
	"bytes"
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

// containerCLI is the container runtime the fixtures drive. Podman runs
// rootless when the test binary is not executed as root.
const containerCLI = "podman"

const containerImage = "kb-test"
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

func buildContainerImage(t *testing.T) {
	t.Helper()
	buildOnce.Do(func() {
		cmd := exec.Command(containerCLI, "build", "-t", containerImage, "test/integration/kb")
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("%s build failed: %v\n%s", containerCLI, err, out)
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

	if _, err := exec.LookPath(containerCLI); err != nil {
		t.Fatalf("%s not found", containerCLI)
	}

	buildContainerImage(t)

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	runCmd := exec.Command(containerCLI, "run", "--rm", "-d",
		"-v", testBinary+":/test:ro",
		containerImage,
		"sleep", "300",
	)
	var runStderr bytes.Buffer
	runCmd.Stderr = &runStderr
	out, err := runCmd.Output()
	if err != nil {
		t.Fatalf("%s run: %v\n%s", containerCLI, err, runStderr.String())
	}
	containerID := strings.TrimSpace(string(out))

	f := &Fixture{T: t, containerID: containerID}

	f.MustExec("git", "config", "--global", "user.email", "test@test.com")
	f.MustExec("git", "config", "--global", "user.name", "Test")
	f.MustExec("git", "init", containerWorkdir)
	f.MustExec("git", "-C", containerWorkdir, "commit", "--allow-empty", "-m", "init")

	t.Cleanup(func() {
		exec.Command(containerCLI, "stop", "-t", "2", containerID).Run()
	})

	return f
}

func (f *Fixture) KB(args ...string) (string, error) {
	f.T.Helper()
	cmdArgs := []string{"exec", "-e", "KB_CLI_MODE=kb", "-w", containerWorkdir, f.containerID, "/test"}
	cmdArgs = append(cmdArgs, args...)
	out, err := exec.Command(containerCLI, cmdArgs...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *Fixture) KBAdmin(args ...string) (string, error) {
	f.T.Helper()
	cmdArgs := []string{"exec", "-e", "KB_CLI_MODE=kbadmin", "-w", containerWorkdir, f.containerID, "/test"}
	cmdArgs = append(cmdArgs, args...)
	out, err := exec.Command(containerCLI, cmdArgs...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *Fixture) KBWithStdin(stdin string, args ...string) (string, error) {
	f.T.Helper()
	cmdArgs := []string{"exec", "-i", "-e", "KB_CLI_MODE=kb", "-w", containerWorkdir, f.containerID, "/test"}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command(containerCLI, cmdArgs...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *Fixture) MustExec(args ...string) string {
	f.T.Helper()
	cmdArgs := append([]string{"exec", f.containerID}, args...)
	out, err := exec.Command(containerCLI, cmdArgs...).CombinedOutput()
	if err != nil {
		f.T.Fatalf("exec %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
