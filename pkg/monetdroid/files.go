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

	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Files · `)
	b.WriteString(Esc(ShortPath(cwd)))
	b.WriteString(`</title>
<script src="https://unpkg.com/htmx.org@2.0.4"></script>
<style>
  @import url('https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;600&family=DM+Sans:wght@400;500;600;700&display=swap');
  :root { --bg: #0c0c0e; --surface: #16161a; --surface2: #1e1e24; --border: #2a2a32; --text: #e2e0d8; --text2: #8b8a85; --accent: #d4a053; --tool: #5b8a72; --tool-bg: #1a2e24; --error: #c45c5c; --blue: #5b7a9e; }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { background: var(--bg); color: var(--text); font-family: 'DM Sans', sans-serif; }
  a { color: var(--accent); text-decoration: none; }
  a:hover { color: var(--text); }

  .files-header { display: flex; align-items: center; gap: 12px; padding: 12px 16px; border-bottom: 1px solid var(--border); background: var(--surface); position: sticky; top: 0; z-index: 10; }
  .files-header a { font-size: 14px; }
  .files-header h1 { font-size: 14px; font-weight: 600; color: var(--text); }

  .files-tabs { display: flex; gap: 0; border-bottom: 1px solid var(--border); background: var(--surface); position: sticky; top: 41px; z-index: 9; }
  .files-tab { padding: 8px 20px; font-size: 13px; font-weight: 500; color: var(--text2); text-decoration: none; border-bottom: 2px solid transparent; }
  .files-tab:hover { color: var(--text); }
  .files-tab.active { color: var(--accent); border-bottom-color: var(--accent); }

  .files-content { padding: 0; }
  .files-empty { padding: 40px; text-align: center; color: var(--text2); font-size: 14px; }

  /* Changes tab */
  .stage-bar { display: flex; gap: 8px; padding: 8px 16px; border-bottom: 1px solid var(--border); background: var(--surface2); }
  .stage-btn { background: var(--surface); border: 1px solid var(--border); color: var(--text2); padding: 4px 12px; font-size: 11px; font-family: 'DM Sans', sans-serif; border-radius: 4px; cursor: pointer; }
  .stage-btn:hover { color: var(--text); border-color: var(--text2); }

  .file-group-header { padding: 10px 16px 4px; font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.5px; color: var(--text2); }
  .file-row { display: flex; align-items: center; gap: 8px; padding: 4px 16px; font-family: 'JetBrains Mono', monospace; font-size: 12px; }
  .file-row:hover { background: var(--surface2); }
  .file-status { width: 18px; text-align: center; font-size: 10px; font-weight: 700; flex-shrink: 0; }
  .file-status-M { color: var(--accent); }
  .file-status-A { color: #589819; }
  .file-status-D { color: var(--error); }
  .file-status-R { color: var(--blue); }
  .file-status-U { color: var(--text2); }
  .file-name { flex: 1; }
  .file-name a { color: var(--blue); text-decoration: none; }
  .file-name a:hover { color: var(--text); }
  .file-action .stage-btn { font-size: 10px; padding: 2px 8px; }

  /* Diff view */
  .diff-nav { display: flex; align-items: center; gap: 12px; padding: 8px 16px; border-bottom: 1px solid var(--border); background: var(--surface2); }
  .diff-nav-back { color: var(--blue); font-size: 12px; }
  .diff-file-header { font-family: 'JetBrains Mono', monospace; font-size: 12px; color: var(--text2); padding: 12px 16px 8px; border-bottom: 1px solid var(--border); display: flex; align-items: center; gap: 8px; }
  .diff-file-header .stage-btn { font-size: 10px; padding: 2px 8px; }
  .diff-body { padding: 0 16px 24px; }
  .diff-body pre { border-radius: 6px; font-size: 11px; line-height: 1.4; overflow-x: auto; }

  /* Browse tab */
  .browse-crumbs { padding: 10px 16px; font-family: 'JetBrains Mono', monospace; font-size: 12px; border-bottom: 1px solid var(--border); background: var(--surface2); }
  .browse-crumbs a { color: var(--blue); }
  .browse-crumbs .crumb-sep { color: var(--text2); margin: 0 4px; }
  .browse-crumbs .crumb-current { color: var(--text); }

  .browse-entry { display: block; padding: 4px 16px; font-family: 'JetBrains Mono', monospace; font-size: 12px; color: var(--blue); text-decoration: none; }
  .browse-entry:hover { background: var(--surface2); color: var(--text); }
  .browse-entry .entry-icon { color: var(--text2); margin-right: 6px; display: inline-block; width: 16px; text-align: center; }

  .file-view { padding: 0 16px 24px; }
  .file-view pre { border-radius: 6px; font-size: 11px; line-height: 1.4; overflow-x: auto; }
  .file-view-header { font-family: 'JetBrains Mono', monospace; font-size: 12px; color: var(--text2); padding: 12px 16px 8px; border-bottom: 1px solid var(--border); display: flex; align-items: center; gap: 12px; }

  /* Search tab */
  .search-bar { padding: 12px 16px; border-bottom: 1px solid var(--border); background: var(--surface2); display: flex; gap: 8px; }
  .search-input { flex: 1; background: var(--bg); border: 1px solid var(--border); color: var(--text); padding: 6px 10px; font-family: 'JetBrains Mono', monospace; font-size: 12px; border-radius: 4px; outline: none; }
  .search-input:focus { border-color: var(--accent); }
  .search-btn { background: var(--surface); border: 1px solid var(--border); color: var(--text2); padding: 6px 16px; font-size: 12px; font-family: 'DM Sans', sans-serif; border-radius: 4px; cursor: pointer; }
  .search-btn:hover { color: var(--text); border-color: var(--text2); }

  .search-file-header { padding: 10px 16px 4px; font-family: 'JetBrains Mono', monospace; font-size: 12px; font-weight: 600; }
  .search-file-header a { color: var(--blue); }
  .search-line { display: flex; gap: 8px; padding: 2px 16px 2px 32px; font-family: 'JetBrains Mono', monospace; font-size: 11px; }
  .search-line:hover { background: var(--surface2); }
  .search-line a { color: var(--text2); text-decoration: none; min-width: 40px; text-align: right; }
  .search-line a:hover { color: var(--accent); }
  .search-line .match-text { color: var(--text); white-space: pre; overflow: hidden; text-overflow: ellipsis; }
  .search-count { padding: 12px 16px; font-size: 12px; color: var(--text2); }

  /* Commits tab */
  .commit-row { display: flex; align-items: baseline; gap: 10px; padding: 6px 16px; font-size: 12px; text-decoration: none; color: var(--text); }
  .commit-row:hover { background: var(--surface2); }
  .commit-hash { font-family: 'JetBrains Mono', monospace; font-size: 11px; flex-shrink: 0; color: var(--accent); }
  .commit-subject { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .commit-time { color: var(--text2); font-size: 11px; flex-shrink: 0; min-width: 80px; text-align: right; }
  .commit-detail-header { padding: 12px 16px; border-bottom: 1px solid var(--border); background: var(--surface2); }
  .commit-detail-subject { font-size: 14px; font-weight: 600; margin-bottom: 4px; }
  .commit-detail-meta { font-size: 11px; color: var(--text2); font-family: 'JetBrains Mono', monospace; }
  .commit-detail-files { padding: 8px 16px; border-bottom: 1px solid var(--border); }
  .commit-detail-files a { display: block; padding: 2px 0; font-family: 'JetBrains Mono', monospace; font-size: 12px; color: var(--blue); }
  .commit-detail-files a:hover { color: var(--text); }
  .diff-section { padding: 0 16px 24px; }
  .diff-section pre { border-radius: 6px; font-size: 11px; line-height: 1.4; overflow-x: auto; }
</style></head><body>
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
		b.WriteString(highlightDiff(diff))
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
		b.WriteString(highlightDiff(chunk))
		b.WriteString(`</div></div>`)
	}

	return b.String()
}
