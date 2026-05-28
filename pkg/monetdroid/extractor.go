package monetdroid

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Extractor transforms streaming task output into a structured summary.
type Extractor interface {
	CanHandle(toolName, command string) bool
	Name() string
	Ingest(chunk string)
	Summary() string
}

var extractorFactories []func() Extractor

func RegisterExtractor(factory func() Extractor) {
	extractorFactories = append(extractorFactories, factory)
}

func MatchExtractor(toolName, command string) Extractor {
	for _, factory := range extractorFactories {
		e := factory()
		if e.CanHandle(toolName, command) {
			return e
		}
	}
	return nil
}

// --- GoTestExtractor ---

var (
	goTestRunRe   = regexp.MustCompile(`^=== RUN\s+(.+)$`)
	goTestPauseRe = regexp.MustCompile(`^=== PAUSE\s+`)
	// The \s* prefix catches indented sub-test results. A test that logs a
	// line matching --- PASS:/FAIL:/SKIP: could produce a false match, but
	// this format is vanishingly unlikely in real test output.
	goTestEndRe     = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (.+) \(([\d.]+)s\)$`)
	goTestPkgOkRe   = regexp.MustCompile(`^ok\s+(\S+)\s+([\d.]+)s`)
	goTestPkgFailRe = regexp.MustCompile(`^FAIL\s+(\S+)\s+([\d.]+)s`)
)

type goTestResult struct {
	name     string
	status   string
	duration string
}

type goPkgResult struct {
	name   string
	status string
}

type GoTestExtractor struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	tests    []goTestResult
	pkgs     []goPkgResult
	pending  string
	runCount int
	sawPause bool
}

func (e *GoTestExtractor) CanHandle(toolName, command string) bool {
	return toolName == "Bash" && strings.Contains(command, "go test")
}

func (e *GoTestExtractor) Name() string { return "Test Results" }

func (e *GoTestExtractor) Ingest(chunk string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.buf.WriteString(chunk)

	data := e.pending + chunk
	e.pending = ""

	lines := strings.Split(data, "\n")
	if !strings.HasSuffix(chunk, "\n") {
		e.pending = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
	}

	for _, line := range lines {
		e.processLine(line)
	}
}

func (e *GoTestExtractor) processLine(line string) {
	if m := goTestRunRe.FindStringSubmatch(line); m != nil {
		e.runCount++
		return
	}
	if goTestPauseRe.MatchString(line) {
		e.sawPause = true
		return
	}
	if m := goTestEndRe.FindStringSubmatch(line); m != nil {
		e.tests = append(e.tests, goTestResult{
			status:   m[1],
			name:     m[2],
			duration: m[3],
		})
		return
	}
	if m := goTestPkgOkRe.FindStringSubmatch(line); m != nil {
		e.pkgs = append(e.pkgs, goPkgResult{name: m[1], status: "ok"})
		return
	}
	if m := goTestPkgFailRe.FindStringSubmatch(line); m != nil {
		e.pkgs = append(e.pkgs, goPkgResult{name: m[1], status: "FAIL"})
		return
	}
}

func (e *GoTestExtractor) Summary() string {
	e.mu.Lock()
	defer e.mu.Unlock()

	passed, failed, skipped := 0, 0, 0
	for _, t := range e.tests {
		switch t.status {
		case "PASS":
			passed++
		case "FAIL":
			failed++
		case "SKIP":
			skipped++
		}
	}

	pkgPassed, pkgFailed := 0, 0
	for _, p := range e.pkgs {
		switch p.status {
		case "ok":
			pkgPassed++
		case "FAIL":
			pkgFailed++
		}
	}

	if len(e.tests) == 0 && len(e.pkgs) == 0 {
		if e.sawPause && e.runCount > 0 {
			return fmt.Sprintf(`<div class="bg-summary-header"><span class="bg-stat">0/%d passed</span></div>`, e.runCount)
		}
		return `<div class="bg-loading">Running...</div>`
	}

	var b bytes.Buffer
	b.WriteString(`<div class="bg-summary-header">`)
	totalPkgs := pkgPassed + pkgFailed
	if totalPkgs > 0 {
		fmt.Fprintf(&b, `<span class="bg-stat">%d packages</span>`, totalPkgs)
	}
	if passed > 0 || failed > 0 || skipped > 0 {
		if e.sawPause && e.runCount > 0 {
			fmt.Fprintf(&b, `<span class="bg-stat">%d/%d passed</span>`, passed, e.runCount)
		} else {
			fmt.Fprintf(&b, `<span class="bg-stat">%d passed</span>`, passed)
		}
	}
	if failed > 0 {
		fmt.Fprintf(&b, `<span class="bg-stat bg-stat-fail">%d failed</span>`, failed)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, `<span class="bg-stat">%d skipped</span>`, skipped)
	}
	b.WriteString(`</div>`)

	for _, t := range e.tests {
		if t.status == "FAIL" {
			fmt.Fprintf(&b, `<details class="bg-failure"><summary>❌ %s (%ss)</summary></details>`,
				Esc(t.name), Esc(t.duration))
		}
	}

	return b.String()
}

func init() {
	RegisterExtractor(func() Extractor { return &GoTestExtractor{} })
}
