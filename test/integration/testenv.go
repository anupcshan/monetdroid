package integration

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

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

func TestdataDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "testdata")
}
