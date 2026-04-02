package monetdroid

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
)

var chromaFormatterLines = chromahtml.New(chromahtml.WithLineNumbers(true), chromahtml.WithLinkableLineNumbers(true, "L"))

// resolveFilesCwd extracts the working directory from session ID or cwd query param.
func (h *Hub) resolveFilesCwd(r *http.Request) (sessionID, cwd string, ok bool) {
	sessionID = r.URL.Query().Get("session")
	if sessionID != "" {
		s := h.Sessions.Get(sessionID)
		if s == nil {
			return "", "", false
		}
		cwd = s.GetCwd()
		return sessionID, cwd, true
	}
	cwd = r.URL.Query().Get("cwd")
	if cwd != "" {
		return "", cwd, true
	}
	return "", "", false
}

func (h *Hub) handleFiles(w http.ResponseWriter, r *http.Request) {
	sessionID, cwd, ok := h.resolveFilesCwd(r)
	if !ok {
		http.Error(w, "session or cwd required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "changes"
	}

	t := NewGitTrace("files-" + tab)
	defer t.Log()

	var content string
	switch tab {
	case "browse":
		browsePath := r.URL.Query().Get("path")
		content = renderBrowseContent(t, sessionID, cwd, browsePath)
	case "search":
		query := r.URL.Query().Get("q")
		content = renderSearchContent(t, sessionID, cwd, query)
	case "commits":
		if hash := r.URL.Query().Get("commit"); hash != "" {
			content = renderCommitDetail(t, sessionID, cwd, hash)
		} else {
			content = renderCommitsContent(t, sessionID, cwd)
		}
	default:
		content = renderChangesContent(t, sessionID, cwd)
	}

	w.Write([]byte(renderFilesPage(sessionID, cwd, tab, content)))
}

func (h *Hub) handleFilesStage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session")
	s := h.Sessions.Get(sessionID)
	if s == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	cwd := s.GetCwd()

	t := NewGitTrace("stage")
	defer t.Log()
	if r.FormValue("all") == "true" {
		GitStage(t, cwd, nil)
	} else if path := r.FormValue("path"); path != "" {
		GitStage(t, cwd, []string{path})
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(renderChangesContent(t, sessionID, cwd)))
}

func (h *Hub) handleFilesUnstage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session")
	s := h.Sessions.Get(sessionID)
	if s == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	cwd := s.GetCwd()

	t := NewGitTrace("unstage")
	defer t.Log()
	if r.FormValue("all") == "true" {
		GitUnstage(t, cwd, nil)
	} else if path := r.FormValue("path"); path != "" {
		GitUnstage(t, cwd, []string{path})
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(renderChangesContent(t, sessionID, cwd)))
}

// --- Page rendering ---

func renderFilesPage(sessionID, cwd, activeTab, content string) string {
	var b strings.Builder

	b.WriteString(`<!DOCTYPE html><html lang="en" class="files-page"><head><meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Files · `)
	b.WriteString(Esc(ShortPath(cwd)))
	b.WriteString(`</title>
<link rel="stylesheet" href="/assets/styles.css">
<script src="/assets/htmx-2.0.4.min.js"></script>
</head><body>
`)

	// Header
	backHref := "/"
	if sessionID != "" {
		backHref = "/?session=" + Esc(sessionID)
	}
	fmt.Fprintf(&b, `<div class="files-header"><a href="%s">← back</a><h1>%s</h1></div>`, backHref, Esc(ShortPath(cwd)))

	// Tabs
	baseURL := "/files?"
	if sessionID != "" {
		baseURL += "session=" + Esc(sessionID)
	} else {
		baseURL += "cwd=" + Esc(cwd)
	}
	b.WriteString(`<div class="files-tabs">`)
	for _, t := range []struct{ id, label string }{{"changes", "Changes"}, {"browse", "Browse"}, {"search", "Search"}, {"commits", "Commits"}} {
		cls := "files-tab"
		if t.id == activeTab {
			cls += " active"
		}
		href := baseURL
		if t.id != "changes" {
			href += "&tab=" + t.id
		}
		fmt.Fprintf(&b, `<a href="%s" class="%s">%s</a>`, href, cls, t.label)
	}
	b.WriteString(`</div>`)

	// Content
	b.WriteString(`<div class="files-content">`)
	b.WriteString(content)
	b.WriteString(`</div>`)

	b.WriteString(`</body></html>`)
	return b.String()
}

// --- Changes tab ---

func renderChangesContent(t *GitTrace, sessionID, cwd string) string {
	files, err := GitStatusFiles(t, cwd)
	if err != nil || len(files) == 0 {
		return `<div class="files-empty">No uncommitted changes</div>`
	}

	var staged, modified, untracked []StatusFile
	for _, f := range files {
		if f.IsUntracked() {
			untracked = append(untracked, f)
		} else {
			if f.IsStaged() {
				staged = append(staged, f)
			}
			if f.IsModified() {
				modified = append(modified, f)
			}
		}
	}

	// Get all diffs in bulk (1 call each instead of per-file).
	stagedDiffs := splitDiffByFileMap(gitDiffAll(t, cwd, "staged"))
	unstagedDiffs := splitDiffByFileMap(gitDiffAll(t, cwd, "unstaged"))

	hasStaged := len(staged) > 0
	hasUnstaged := len(modified) > 0 || len(untracked) > 0

	var b strings.Builder

	// Stage All / Unstage All bar
	if hasStaged || hasUnstaged {
		b.WriteString(`<div class="stage-bar">`)
		if hasUnstaged {
			fmt.Fprintf(&b, `<button class="stage-btn" hx-post="/files/stage" hx-vals='{"session":"%s","all":"true"}' hx-target="#file-list" hx-swap="innerHTML">Stage All</button>`, Esc(sessionID))
		}
		if hasStaged {
			fmt.Fprintf(&b, `<button class="stage-btn" hx-post="/files/unstage" hx-vals='{"session":"%s","all":"true"}' hx-target="#file-list" hx-swap="innerHTML">Unstage All</button>`, Esc(sessionID))
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(`<div id="file-list">`)

	if len(staged) > 0 {
		fmt.Fprintf(&b, `<div class="file-group-header">Staged (%d)</div>`, len(staged))
		for _, f := range staged {
			renderInlineDiff(&b, sessionID, f.Path, string(f.Index), stagedDiffs[f.Path], "staged")
		}
	}

	if len(modified) > 0 {
		fmt.Fprintf(&b, `<div class="file-group-header">Modified (%d)</div>`, len(modified))
		for _, f := range modified {
			renderInlineDiff(&b, sessionID, f.Path, string(f.Worktree), unstagedDiffs[f.Path], "unstaged")
		}
	}

	if len(untracked) > 0 {
		fmt.Fprintf(&b, `<div class="file-group-header">Untracked (%d)</div>`, len(untracked))
		for _, f := range untracked {
			diff, _ := GitDiffFileContent(t, cwd, f.Path, "untracked")
			renderInlineDiff(&b, sessionID, f.Path, "?", diff, "untracked")
		}
	}

	b.WriteString(`</div>`)
	return b.String()
}

func renderInlineDiff(b *strings.Builder, sessionID, path, badge, diff, mode string) {
	action := "Stage"
	endpoint := "/files/stage"
	if mode == "staged" {
		action = "Unstage"
		endpoint = "/files/unstage"
	}
	fmt.Fprintf(b, `<div class="diff-section" id="%s">`, Esc(path))
	fmt.Fprintf(b, `<div class="diff-file-header"><span class="file-status file-status-%s">%s</span> %s`, Esc(badge), Esc(badge), Esc(path))
	if sessionID != "" {
		fmt.Fprintf(b, ` <button class="stage-btn" hx-post="%s" hx-vals='{"session":"%s","path":"%s"}' hx-target="#file-list" hx-swap="innerHTML">%s</button>`,
			endpoint, Esc(sessionID), Esc(path), action)
	}
	b.WriteString(`</div>`)
	if diff != "" {
		b.WriteString(`<div class="diff-body">`)
		b.WriteString(RenderDiffTableFromUnified(diff, sessionID, true))
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
}

// --- Browse tab ---

func renderBrowseContent(t *GitTrace, sessionID, cwd, browsePath string) string {
	var b strings.Builder

	baseURL := "/files?"
	if sessionID != "" {
		baseURL += "session=" + Esc(sessionID)
	} else {
		baseURL += "cwd=" + Esc(cwd)
	}
	baseURL += "&tab=browse"

	// Check if browsePath points to a file
	if browsePath != "" {
		fullPath := filepath.Join(cwd, browsePath)
		info, err := os.Stat(fullPath)
		if err == nil && !info.IsDir() {
			return renderFileView(t, &b, baseURL, sessionID, cwd, browsePath, fullPath, info)
		}
	}

	// Directory listing
	renderBreadcrumbs(&b, baseURL, browsePath)

	entries, err := GitListDir(t, cwd, browsePath)
	if err != nil {
		fmt.Fprintf(&b, `<div class="files-empty">Error: %s</div>`, Esc(err.Error()))
		return b.String()
	}
	if len(entries) == 0 {
		b.WriteString(`<div class="files-empty">Empty directory</div>`)
		return b.String()
	}

	for _, e := range entries {
		entryPath := e.Name
		if browsePath != "" {
			entryPath = browsePath + "/" + e.Name
		}
		href := baseURL + "&path=" + Esc(entryPath)
		if e.IsDir {
			fmt.Fprintf(&b, `<a class="browse-entry" href="%s"><span class="entry-icon">▸</span>%s/</a>`, href, Esc(e.Name))
		} else {
			fmt.Fprintf(&b, `<a class="browse-entry" href="%s"><span class="entry-icon"> </span>%s</a>`, href, Esc(e.Name))
		}
	}

	return b.String()
}

func renderBreadcrumbs(b *strings.Builder, baseURL, browsePath string) {
	b.WriteString(`<div class="browse-crumbs">`)
	if browsePath == "" {
		b.WriteString(`<span class="crumb-current">/</span>`)
	} else {
		fmt.Fprintf(b, `<a href="%s">/</a>`, baseURL)
		parts := strings.Split(browsePath, "/")
		for i, part := range parts {
			b.WriteString(`<span class="crumb-sep">›</span>`)
			if i == len(parts)-1 {
				fmt.Fprintf(b, `<span class="crumb-current">%s</span>`, Esc(part))
			} else {
				partial := strings.Join(parts[:i+1], "/")
				fmt.Fprintf(b, `<a href="%s&path=%s">%s</a>`, baseURL, Esc(partial), Esc(part))
			}
		}
	}
	b.WriteString(`</div>`)
}

func renderFileView(t *GitTrace, b *strings.Builder, baseURL, sessionID, cwd, browsePath, fullPath string, info os.FileInfo) string {
	renderBreadcrumbs(b, baseURL, browsePath)

	// File size limit: 1MB
	if info.Size() > 1<<20 {
		b.WriteString(`<div class="files-empty">File too large to display</div>`)
		return b.String()
	}

	content, err := os.ReadFile(fullPath)
	if err != nil {
		fmt.Fprintf(b, `<div class="files-empty">Error reading file: %s</div>`, Esc(err.Error()))
		return b.String()
	}

	// Check if file has uncommitted changes — link to diff
	var extraLinks string
	if sessionID != "" {
		diffContent, _ := GitDiffFileContent(t, cwd, browsePath, "unstaged")
		if diffContent != "" {
			diffHref := fmt.Sprintf("/files?session=%s&diff=%s", Esc(sessionID), Esc(browsePath))
			extraLinks = fmt.Sprintf(` <a href="%s" style="font-size:11px;color:var(--accent)">view diff</a>`, diffHref)
		}
	}

	fmt.Fprintf(b, `<div class="file-view-header">%s%s</div>`, Esc(filepath.Base(browsePath)), extraLinks)

	b.WriteString(`<div class="file-view">`)
	b.WriteString(highlightFile(filepath.Base(fullPath), string(content)))
	b.WriteString(`</div>`)

	return b.String()
}

func highlightFile(filename, content string) string {
	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	iterator, err := lexer.Tokenise(nil, content)
	if err != nil {
		return "<pre>" + Esc(content) + "</pre>"
	}
	var buf bytes.Buffer
	if err := chromaFormatterLines.Format(&buf, chromaStyle, iterator); err != nil {
		return "<pre>" + Esc(content) + "</pre>"
	}
	return buf.String()
}

// --- Search tab ---

func renderSearchContent(t *GitTrace, sessionID, cwd, query string) string {
	var b strings.Builder

	baseURL := "/files?"
	if sessionID != "" {
		baseURL += "session=" + Esc(sessionID)
	} else {
		baseURL += "cwd=" + Esc(cwd)
	}

	// Search form
	b.WriteString(`<div class="search-bar">`)
	fmt.Fprintf(&b, `<form action="/files" method="get" style="display:flex;gap:8px;flex:1">`)
	if sessionID != "" {
		fmt.Fprintf(&b, `<input type="hidden" name="session" value="%s">`, Esc(sessionID))
	} else {
		fmt.Fprintf(&b, `<input type="hidden" name="cwd" value="%s">`, Esc(cwd))
	}
	b.WriteString(`<input type="hidden" name="tab" value="search">`)
	fmt.Fprintf(&b, `<input type="text" name="q" class="search-input" placeholder="regex pattern..." value="%s" autofocus>`, Esc(query))
	b.WriteString(`<button type="submit" class="search-btn">Search</button>`)
	b.WriteString(`</form></div>`)

	if query == "" {
		return b.String()
	}

	results, err := GitGrep(t, cwd, query)
	if err != nil {
		fmt.Fprintf(&b, `<div class="files-empty">Error: %s</div>`, Esc(err.Error()))
		return b.String()
	}
	if len(results) == 0 {
		b.WriteString(`<div class="files-empty">No matches</div>`)
		return b.String()
	}

	browseURL := baseURL + "&tab=browse&path="

	// Group by file
	var currentFile string
	matchCount := 0
	for _, m := range results {
		if m.File != currentFile {
			currentFile = m.File
			fmt.Fprintf(&b, `<div class="search-file-header"><a href="%s%s">%s</a></div>`, browseURL, Esc(m.File), Esc(m.File))
		}
		fmt.Fprintf(&b, `<div class="search-line"><a href="%s%s#L%d">%d</a><span class="match-text">%s</span></div>`,
			browseURL, Esc(m.File), m.Line, m.Line, Esc(m.Text))
		matchCount++
	}

	fmt.Fprintf(&b, `<div class="search-count">%d matches`, matchCount)
	if matchCount >= 500 {
		b.WriteString(` (truncated)`)
	}
	b.WriteString(`</div>`)

	return b.String()
}

// --- Commits tab ---

func renderCommitsContent(t *GitTrace, sessionID, cwd string) string {
	commits, err := GitLog(t, cwd, 50)
	if err != nil || len(commits) == 0 {
		return `<div class="files-empty">No commits</div>`
	}

	baseURL := "/files?"
	if sessionID != "" {
		baseURL += "session=" + Esc(sessionID)
	} else {
		baseURL += "cwd=" + Esc(cwd)
	}
	baseURL += "&tab=commits"

	var b strings.Builder
	for _, c := range commits {
		fmt.Fprintf(&b, `<a class="commit-row" href="%s&commit=%s">`, baseURL, Esc(c.Hash))
		fmt.Fprintf(&b, `<span class="commit-hash">%s</span>`, Esc(c.ShortHash))
		fmt.Fprintf(&b, `<span class="commit-subject">%s</span>`, Esc(c.Subject))
		fmt.Fprintf(&b, `<span class="commit-time">%s</span>`, Esc(c.TimeAgo))
		b.WriteString(`</a>`)
	}
	return b.String()
}

func renderCommitDetail(t *GitTrace, sessionID, cwd, hash string) string {
	var b strings.Builder

	baseURL := "/files?"
	if sessionID != "" {
		baseURL += "session=" + Esc(sessionID)
	} else {
		baseURL += "cwd=" + Esc(cwd)
	}

	// Get commit metadata
	meta, err := GitLogOne(t, cwd, hash)
	if err != nil {
		return fmt.Sprintf(`<div class="files-empty">Error: %s</div>`, Esc(err.Error()))
	}

	// Navigation bar
	b.WriteString(`<div class="diff-nav">`)
	fmt.Fprintf(&b, `<a href="%s&tab=commits" class="diff-nav-back">← commits</a>`, baseURL)
	b.WriteString(`</div>`)

	// Commit header
	b.WriteString(`<div class="commit-detail-header">`)
	fmt.Fprintf(&b, `<div class="commit-detail-subject">%s</div>`, Esc(meta.Subject))
	fmt.Fprintf(&b, `<div class="commit-detail-meta">%s · %s · %s</div>`, Esc(meta.ShortHash), Esc(meta.Author), Esc(meta.TimeAgo))
	b.WriteString(`</div>`)

	// File list
	files, _ := GitShowCommitFiles(t, cwd, hash)
	if len(files) > 0 {
		b.WriteString(`<div class="commit-detail-files">`)
		for _, f := range files {
			fmt.Fprintf(&b, `<a href="#%s">%s</a>`, Esc(f), Esc(f))
		}
		b.WriteString(`</div>`)
	}

	// Full diff
	diffContent, err := GitShowCommit(t, cwd, hash)
	if err != nil || diffContent == "" {
		b.WriteString(`<div class="files-empty">No changes</div>`)
		return b.String()
	}

	// Split diff by file and render each section
	chunks := splitDiffByFile(diffContent)
	for _, chunk := range chunks {
		firstLine := chunk
		if idx := strings.Index(chunk, "\n"); idx >= 0 {
			firstLine = chunk[:idx]
		}
		name := ""
		if strings.HasPrefix(firstLine, "diff --git ") {
			fields := strings.Fields(firstLine)
			if len(fields) >= 4 {
				name = strings.TrimPrefix(fields[3], "b/")
			}
		}
		fmt.Fprintf(&b, `<div class="diff-section" id="%s">`, Esc(name))
		fmt.Fprintf(&b, `<div class="diff-file-header">%s</div>`, Esc(name))
		b.WriteString(`<div class="diff-body">`)
		b.WriteString(RenderDiffTableFromUnified(chunk, "", false))
		b.WriteString(`</div></div>`)
	}

	return b.String()
}
