package integration

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/monetdroid/pkg/monetdroid"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Fixture holds everything needed for an integration test.
type Fixture struct {
	T         *testing.T
	ServerURL string
	Browser   *rod.Browser
	Hub       *monetdroid.Hub
	WorkDir   string // temp directory for use as session cwd
}

// Setup starts the server with a mock claude binary and launches a headless browser.
func Setup(t *testing.T, fixtureName string) *Fixture {
	t.Helper()

	fixturePath := fixtureFile(t, fixtureName)

	// Re-exec test binary as mock claude
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	monetdroid.ClaudeCommand = testBin
	t.Cleanup(func() { monetdroid.ClaudeCommand = "claude" })

	t.Setenv("MOCK_CLAUDE", "1")
	t.Setenv("MOCK_FIXTURE", fixturePath)

	workDir := t.TempDir()

	// Start server
	hub := monetdroid.NewHub()
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

	return &Fixture{
		T:         t,
		ServerURL: serverURL,
		Browser:   browser,
		Hub:       hub,
		WorkDir:   workDir,
	}
}

// Page creates a new browser page and auto-captures a screenshot on failure.
func (f *Fixture) Page() *rod.Page {
	p := f.Browser.MustPage(f.ServerURL).MustWaitStable()
	f.T.Cleanup(func() {
		if f.T.Failed() {
			ScreenshotOnFailure(f.T, p, f.T.Name())
		}
	})
	return p
}

// Screenshot captures the current page state.
func Screenshot(t *testing.T, page *rod.Page, name string) {
	t.Helper()
	dir := filepath.Join(TestdataDir(), "screenshots")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, name+".png")
	data, err := page.Screenshot(true, &proto.PageCaptureScreenshot{
		Format: proto.PageCaptureScreenshotFormatPng,
	})
	if err != nil {
		t.Logf("screenshot failed: %v", err)
		return
	}
	os.WriteFile(path, data, 0o644)
	t.Logf("screenshot saved: %s", path)
}

func ScreenshotOnFailure(t *testing.T, page *rod.Page, name string) {
	t.Helper()
	Screenshot(t, page, "FAIL_"+name)
}

// WaitForText waits for an element to contain text.
func WaitForText(t *testing.T, page *rod.Page, selector, text string, timeout time.Duration) {
	t.Helper()
	err := rod.Try(func() {
		el := page.Timeout(timeout).MustElement(selector)
		if got := el.MustText(); !containsText(got, text) {
			panic(fmt.Sprintf("element %q text %q does not contain %q", selector, got, text))
		}
	})
	if err != nil {
		t.Fatalf("WaitForText(%q, %q): %v", selector, text, err)
	}
}

// WaitForElement waits for an element to exist.
func WaitForElement(t *testing.T, page *rod.Page, selector string, timeout time.Duration) *rod.Element {
	t.Helper()
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		t.Fatalf("WaitForElement(%q): %v", selector, err)
	}
	return el
}

func containsText(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func fixtureFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(TestdataDir(), name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture not found: %s", path)
	}
	return path
}

func TestdataDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "testdata")
}
