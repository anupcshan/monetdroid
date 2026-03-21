package integration

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

const dockerImage = "monetdroid-claude-test"

const containerWorkdir = "/work"

// containerTimeout is the maximum lifetime of a test container.
// The container's entrypoint uses `timeout` to self-terminate after this duration,
// ensuring cleanup even if the test process crashes or CI is killed.
const containerTimeout = 300 // seconds

var buildOnce sync.Once
var buildErr error

// buildDockerImage builds the docker image once per test run.
func buildDockerImage(t *testing.T) {
	t.Helper()
	buildOnce.Do(func() {
		// Dockerfile is next to this source file
		_, thisFile, _, _ := runtime.Caller(0)
		dockerfileDir := filepath.Dir(thisFile)

		cmd := exec.Command("docker", "build", "-t", dockerImage, dockerfileDir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("docker build failed: %v\n%s", err, out)
			return
		}
		// Log but don't spam — only on first build
		t.Logf("built docker image %s", dockerImage)
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
}

// ContainerFixture holds everything needed for a container-based integration test.
type ContainerFixture struct {
	T           *testing.T
	containerID string
	ServerURL   string
	Browser     *rod.Browser
	ReplayerURL string
	Replayer    *Replayer
}

// WriteFile writes a file inside the container via the test HTTP endpoint.
func (f *ContainerFixture) WriteFile(path, content string) {
	f.T.Helper()
	resp, err := http.PostForm(f.ServerURL+"/test/write", map[string][]string{
		"path":    {path},
		"content": {content},
	})
	if err != nil {
		f.T.Fatalf("WriteFile(%s): %v", path, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		f.T.Fatalf("WriteFile(%s): status %d", path, resp.StatusCode)
	}
}

// ReadFile reads a file from the container via the test HTTP endpoint.
func (f *ContainerFixture) ReadFile(path string) string {
	f.T.Helper()
	resp, err := http.Get(f.ServerURL + "/test/read?path=" + path)
	if err != nil {
		f.T.Fatalf("ReadFile(%s): %v", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		f.T.Fatalf("ReadFile(%s): status %d: %s", path, resp.StatusCode, data)
	}
	return string(data)
}

// DockerExec runs a command inside the test container.
func (f *ContainerFixture) DockerExec(args ...string) (string, error) {
	f.T.Helper()
	cmdArgs := append([]string{"exec", f.containerID}, args...)
	out, err := exec.Command("docker", cmdArgs...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// SetupWithContainer starts a docker container running the monetdroid server
// (the test binary itself in server mode) with claude available as a subprocess,
// and an API replayer intercepting Anthropic API calls.
//
// mode is "record" or "replay".
// cassetteName is the filename under testdata/cassettes/ (e.g. "tool_use.jsonl").
func SetupWithContainer(t *testing.T, cassetteName, mode string) *ContainerFixture {
	t.Helper()

	// Check docker is available
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker not found — integration tests require docker")
	}

	// Build docker image (once per test run)
	buildDockerImage(t)

	cassettesDir := filepath.Join(TestdataDir(), "cassettes")
	os.MkdirAll(cassettesDir, 0o755)
	cassettePath := filepath.Join(cassettesDir, cassetteName)

	// In replay mode, cassette must exist
	if mode == "replay" {
		if _, err := os.Stat(cassettePath); err != nil {
			t.Skipf("cassette %s not found — record it first with -record flag", cassetteName)
		}
	}

	// Start replayer on the host
	upstream := "https://api.anthropic.com"
	replayer := NewReplayer(t, cassettePath, mode, upstream)
	replayerURL := replayer.Start()

	// Get the test binary path — we bind-mount it into the container
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	dockerArgs := []string{
		"run", "--rm", "-d",
		"--add-host=host.docker.internal:host-gateway",
		"-p", "0:8222",
		"-v", testBinary + ":/test:ro",
		"-e", "MONETDROID_IN_CONTAINER=1",
		"-e", "ANTHROPIC_BASE_URL=" + replayerURL,
		"-e", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}

	if mode == "record" {
		home, _ := os.UserHomeDir()
		credsFile := filepath.Join(home, ".claude", ".credentials.json")
		if _, err := os.Stat(credsFile); err == nil {
			dockerArgs = append(dockerArgs,
				"-v", credsFile+":/root/.claude/.credentials.json:ro",
			)
		} else {
			t.Logf("warning: credentials file %s not found, recording may fail", credsFile)
		}
	} else {
		dockerArgs = append(dockerArgs, "-e", "ANTHROPIC_API_KEY=dummy-replay-key")
	}

	dockerArgs = append(dockerArgs, dockerImage,
		"timeout", fmt.Sprintf("%d", containerTimeout), "/test",
	)

	out, err := exec.Command("docker", dockerArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	containerID := strings.TrimSpace(string(out))
	t.Logf("started container %s", containerID[:12])

	// Stream container logs to test output
	logCmd := exec.Command("docker", "logs", "-f", containerID)
	logCmd.Stdout = t.Output()
	logCmd.Stderr = t.Output()
	logCmd.Start()

	t.Cleanup(func() {
		exec.Command("docker", "stop", "-t", "5", containerID).Run()
		logCmd.Wait()
	})

	// Discover the host port mapped to container port 8222
	portOut, err := exec.Command("docker", "port", containerID, "8222").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	hostAddr := strings.TrimSpace(string(portOut))
	// Output is like "0.0.0.0:32768\n[::]:32768"; take the first line
	if i := strings.Index(hostAddr, "\n"); i >= 0 {
		hostAddr = hostAddr[:i]
	}
	serverURL := fmt.Sprintf("http://127.0.0.1:%s", hostAddr[strings.LastIndex(hostAddr, ":")+1:])

	// Wait for server to be ready
	ready := false
	for i := 0; i < 100; i++ {
		resp, err := http.Get(serverURL)
		if err == nil {
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("server not ready after 10s (check container output above)")
	}

	// Launch headless browser
	u := launcher.New().Headless(true).MustLaunch()
	browser := rod.New().ControlURL(u).MustConnect()
	t.Cleanup(func() { browser.MustClose() })

	return &ContainerFixture{
		T:           t,
		containerID: containerID,
		ServerURL:   serverURL,
		Browser:     browser,
		ReplayerURL: replayerURL,
		Replayer:    replayer,
	}
}

// Page creates a new browser page and auto-captures a screenshot on failure.
func (f *ContainerFixture) Page() *rod.Page {
	p := f.Browser.MustPage(f.ServerURL).MustWaitStable()
	f.T.Cleanup(func() {
		if f.T.Failed() {
			ScreenshotOnFailure(f.T, p, f.T.Name())
		}
	})
	return p
}
