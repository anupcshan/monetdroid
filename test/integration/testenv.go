package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// WaitForTurnComplete asserts the "no pending turn" end state: a new
// assistant message has appeared and the turn has finished streaming.
// prevAssistantsCount is the .msg-assistant count captured before the turn
// started. The wait gates on a new assistant message first because
// #stop-btn:empty alone matches the leftover empty span between turns.
func WaitForTurnComplete(t *testing.T, page *rod.Page, prevAssistantsCount int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for len(page.MustElements(".msg-assistant")) <= prevAssistantsCount {
		if time.Now().After(deadline) {
			t.Fatalf("WaitForTurnComplete: no new assistant message after 60s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	WaitForElement(t, page, "#stop-btn:empty", 60*time.Second)
}

// WaitForPendingPermission asserts the "pending permissions" end state: the
// turn has ended blocked on an unresolved permission prompt, with its Allow
// control still on screen. No further API call can happen until the prompt
// is answered, so the test ends with the prompt unanswered and the cassette
// is complete. Use this or WaitForTurnComplete when a test starts a turn it
// does not otherwise drive to completion. Ending any other way leaves a
// turn that can still issue API calls the cassette does not contain.
func WaitForPendingPermission(t *testing.T, page *rod.Page) {
	t.Helper()
	WaitForElement(t, page, ".perm-inline .perm-allow", 60*time.Second)
}

// Screenshot captures the current page as a PNG and writes its HTML to a
// sibling .html file.
func Screenshot(t *testing.T, page *rod.Page, name string) {
	t.Helper()
	dir := filepath.Join(TestdataDir(), "screenshots")
	path := filepath.Join(dir, name+".png")
	os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := page.Timeout(10*time.Second).Screenshot(true, &proto.PageCaptureScreenshot{
		Format: proto.PageCaptureScreenshotFormatPng,
	})
	if err != nil {
		t.Logf("screenshot failed: %v", err)
	} else if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Logf("screenshot write failed: %v", err)
	}
	DumpDOM(t, page, name)
}

func ScreenshotOnFailure(t *testing.T, page *rod.Page, name string) {
	t.Helper()
	Screenshot(t, page, "FAIL_"+name)
}

// DumpDOM saves the page's HTML to a file.
func DumpDOM(t *testing.T, page *rod.Page, name string) {
	t.Helper()
	dir := filepath.Join(TestdataDir(), "screenshots")
	path := filepath.Join(dir, name+".html")
	os.MkdirAll(filepath.Dir(path), 0o755)
	html, err := page.Timeout(10 * time.Second).HTML()
	if err != nil {
		t.Logf("DOM dump failed: %v", err)
		return
	}
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		t.Logf("DOM dump write failed: %v", err)
		return
	}
}

// WaitForText waits for an element matching selector to contain text.
func WaitForText(t *testing.T, page *rod.Page, selector, text string, timeout time.Duration) {
	t.Helper()
	_, err := page.Timeout(timeout).ElementR(selector, text)
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

// assert carries the running test. Its helpers let error-returning calls
// chain like rod's Must* variants while failing the test instead of
// panicking. Prefer these helpers where failure is possible, since a Must*
// panic aborts the suite and destroys other tests' results.
type assert struct{ t *testing.T }

// Must returns v, failing the test when err is non-nil. The failure reports
// the error at the caller's line.
func (a assert) Must[T any](v T, err error) T {
	a.t.Helper()
	if err != nil {
		a.t.Fatal(err)
	}
	return v
}

// NoError fails the test when err is non-nil, for calls that return only an
// error. The failure reports the error at the caller's line.
func (a assert) NoError(err error) {
	a.t.Helper()
	if err != nil {
		a.t.Fatal(err)
	}
}

func TestdataDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "testdata")
}

// CreatePlainSession opens the header "+" popover, expands the "Plain session"
// details, fills in the cwd, and clicks Create.
func CreatePlainSession(t *testing.T, page *rod.Page, cwd string) {
	t.Helper()
	page.MustElement(`button[popovertarget="new-session-popover"]`).MustClick()
	WaitForElement(t, page, `#new-session-popover details summary`, 5*time.Second)
	page.MustElement(`#new-session-popover details summary`).MustClick()
	WaitForElement(t, page, `#new-session-popover details input[name="cwd"]`, 5*time.Second)
	page.MustElement(`#new-session-popover details input[name="cwd"]`).MustInput(cwd)
	page.MustElement(`#new-session-popover details .btn-create`).MustClick()
}

// SelectMode opens the mode picker popover and selects the named permission
// mode. mode is the raw CLI mode string, e.g. "acceptEdits" or "default".
func SelectMode(t *testing.T, page *rod.Page, mode string) {
	t.Helper()
	page.MustElement(`button[popovertarget="mode-picker-popover"]`).MustClick()
	btn := WaitForElement(t, page, fmt.Sprintf(`#mode-picker-popover form[data-mode="%s"] button`, mode), 5*time.Second)
	btn.MustClick()
}
