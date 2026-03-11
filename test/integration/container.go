package integration

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/anupcshan/monetdroid/pkg/monetdroid"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

const dockerImage = "monetdroid-claude-test"

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
	ServerURL   string
	Browser     *rod.Browser
	Hub         *monetdroid.Hub
	WorkDir     string
	ReplayerURL string
}

// SetupWithContainer starts the server with claude running inside a docker container
// and an API replayer intercepting Anthropic API calls.
//
// mode is "record" or "replay".
// cassetteName is the filename under testdata/cassettes/ (e.g. "simple_hello.jsonl").
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

	// Start replayer
	upstream := "https://api.anthropic.com"
	replayer := NewReplayer(t, cassettePath, mode, upstream)
	replayerURL := replayer.Start()

	workDir := t.TempDir()

	// Named docker volume for persistent claude home across container invocations.
	// Needed for --resume to find the session file from a previous turn.
	volName := fmt.Sprintf("monetdroid-test-%s-%d", t.Name(), time.Now().UnixNano())
	out, err := exec.Command("docker", "volume", "create", volName).CombinedOutput()
	if err != nil {
		t.Fatalf("docker volume create: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("docker", "volume", "rm", volName).Run()
	})

	// Start server
	hub := monetdroid.NewHub()

	// Set BuildClaudeCmd on the hub to run claude in a container
	hub.BuildClaudeCmd = func(cwd string, args []string) *exec.Cmd {
		dockerArgs := []string{
			"run", "--rm", "-i",
			"--network=host",
			"-e", "ANTHROPIC_BASE_URL=" + replayerURL,
			"-v", cwd + ":/work",
			"-w", "/work",
			"-v", volName + ":/root/.claude",
		}

		if mode == "record" {
			// Bind-mount credentials into the persistent claude home for subscription auth
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
			// Replay mode: dummy API key so CLI doesn't complain
			dockerArgs = append(dockerArgs, "-e", "ANTHROPIC_API_KEY=dummy-replay-key")
		}

		dockerArgs = append(dockerArgs, dockerImage)
		// claude args follow the image name (ENTRYPOINT is "claude")
		dockerArgs = append(dockerArgs, args...)

		cmd := exec.Command("docker", dockerArgs...)
		cmd.Env = append(os.Environ(), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
		return cmd
	}
	mux := monetdroid.RegisterRoutes(hub)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	serverURL := fmt.Sprintf("http://%s", listener.Addr().String())

	// Wait for server
	for i := 0; i < 50; i++ {
		resp, err := http.Get(serverURL)
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Launch headless browser
	u := launcher.New().Headless(true).MustLaunch()
	browser := rod.New().ControlURL(u).MustConnect()
	t.Cleanup(func() { browser.MustClose() })

	return &ContainerFixture{
		T:           t,
		ServerURL:   serverURL,
		Browser:     browser,
		Hub:         hub,
		WorkDir:     workDir,
		ReplayerURL: replayerURL,
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
