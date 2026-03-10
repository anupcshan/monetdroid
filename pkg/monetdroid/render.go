package monetdroid

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList),
	goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
)

func Esc(s string) string { return html.EscapeString(s) }

func RenderMarkdown(text string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return Esc(text)
	}
	return strings.ReplaceAll(buf.String(), "<a ", `<a target="_blank" rel="noopener" `)
}

func FormatToolInput(tool string, input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		j, _ := json.MarshalIndent(input, "", "  ")
		return string(j)
	}
	filePath, _ := m["file_path"].(string)
	if filePath == "" {
		filePath, _ = m["path"].(string)
	}
	switch tool {
	case "Bash":
		if cmd, _ := m["command"].(string); cmd != "" {
			return cmd
		}
	case "Read", "FileRead":
		if filePath != "" {
			return filePath
		}
	case "Write", "FileWrite":
		content, _ := m["content"].(string)
		if len(content) > 200 {
			content = content[:200]
		}
		return filePath + "\n" + content
	case "Edit", "FileEdit":
		var lines []string
		lines = append(lines, filePath)
		if old, _ := m["old_string"].(string); old != "" {
			lines = append(lines, "--- old ---", old)
		}
		if new_, _ := m["new_string"].(string); new_ != "" {
			lines = append(lines, "+++ new +++", new_)
		}
		return strings.Join(lines, "\n")
	case "Grep":
		if p, _ := m["pattern"].(string); p != "" {
			return p
		}
	case "Glob":
		if p, _ := m["pattern"].(string); p != "" {
			return p
		}
	}
	j, _ := json.MarshalIndent(input, "", "  ")
	return string(j)
}

func FormatPermDetail(tool string, input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		j, _ := json.MarshalIndent(input, "", "  ")
		return string(j)
	}
	filePath, _ := m["file_path"].(string)
	if filePath == "" {
		filePath, _ = m["path"].(string)
	}
	switch tool {
	case "Bash":
		cmd, _ := m["command"].(string)
		desc, _ := m["description"].(string)
		if desc != "" {
			return desc + "\n\n" + cmd
		}
		return cmd
	case "Edit", "FileEdit":
		var lines []string
		lines = append(lines, filePath)
		if old, _ := m["old_string"].(string); old != "" {
			lines = append(lines, "--- old ---", old)
		}
		if new_, _ := m["new_string"].(string); new_ != "" {
			lines = append(lines, "+++ new +++", new_)
		}
		return strings.Join(lines, "\n")
	case "Write", "FileWrite":
		content, _ := m["content"].(string)
		return filePath + "\n\n" + content
	case "Read", "FileRead":
		return filePath
	}
	j, _ := json.MarshalIndent(input, "", "  ")
	return string(j)
}

var chromaFormatter = chromahtml.New()
var chromaStyle = styles.Get("vim")

func stripTrailingWS(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(l, " \t\r") + "\n"
	}
	return out
}

func RenderEditDiffHTML(filePath, oldStr, newStr string) string {
	oldLines := stripTrailingWS(difflib.SplitLines(oldStr))
	newLines := stripTrailingWS(difflib.SplitLines(newStr))
	ud := difflib.UnifiedDiff{
		A:        oldLines,
		B:        newLines,
		FromFile: filePath,
		ToFile:   filePath,
		Context:  3,
	}
	diffText, err := difflib.GetUnifiedDiffString(ud)
	if err != nil || diffText == "" {
		// Fallback: show old/new as plain text
		return ""
	}
	return highlightDiff(diffText)
}

func highlightDiff(diffText string) string {
	lexer := lexers.Get("diff")
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	iterator, err := lexer.Tokenise(nil, diffText)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	if err := chromaFormatter.Format(&buf, chromaStyle, iterator); err != nil {
		return ""
	}
	return buf.String()
}

func editDiffFromInput(input any) (filePath, oldStr, newStr string, ok bool) {
	m, mok := input.(map[string]any)
	if !mok {
		return
	}
	filePath, _ = m["file_path"].(string)
	if filePath == "" {
		filePath, _ = m["path"].(string)
	}
	oldStr, _ = m["old_string"].(string)
	newStr, _ = m["new_string"].(string)
	ok = true
	return
}

func editSummary(filePath, oldStr, newStr string) string {
	ud := difflib.UnifiedDiff{
		A:       stripTrailingWS(difflib.SplitLines(oldStr)),
		B:       stripTrailingWS(difflib.SplitLines(newStr)),
		Context: 0,
	}
	diffText, _ := difflib.GetUnifiedDiffString(ud)
	added, removed := 0, 0
	for _, line := range strings.Split(diffText, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			added++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			removed++
		}
	}
	base := filepath.Base(filePath)
	return fmt.Sprintf("Edit %s −%d +%d", base, removed, added)
}

var boringResultSuffixes = []string{
	"has been updated successfully.",
	"has been written successfully.",
}

func isBoringResult(output string) bool {
	s := strings.TrimSpace(output)
	for _, suffix := range boringResultSuffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

func RenderMsg(msg ServerMsg) string {
	switch msg.Type {
	case "user_message":
		var content strings.Builder
		for i, img := range msg.Images {
			dlgID := fmt.Sprintf("img-dlg-%s-%d", msg.SessionID, i)
			src := fmt.Sprintf("data:%s;base64,%s", Esc(img.MediaType), img.Data)
			fmt.Fprintf(&content, `<img src="%s" class="msg-img-thumb" onclick="document.getElementById('%s').showModal()">`, src, dlgID)
			fmt.Fprintf(&content, `<dialog id="%s" class="img-dialog" onclick="this.close()"><img src="%s"></dialog>`, dlgID, src)
		}
		content.WriteString(strings.ReplaceAll(Esc(msg.Text), "\n", "<br>"))
		return fmt.Sprintf(`<div class="msg msg-user"><div class="msg-bubble">%s</div></div>`, content.String())
	case "text":
		return fmt.Sprintf(`<div class="msg msg-assistant"><div class="msg-bubble">%s</div></div>`, RenderMarkdown(msg.Text))
	case "tool_use":
		if msg.Tool == "TodoWrite" {
			return ""
		}
		if msg.Tool == "Edit" || msg.Tool == "FileEdit" {
			if fp, old, new_, ok := editDiffFromInput(msg.Input); ok {
				diffHTML := RenderEditDiffHTML(fp, old, new_)
				if diffHTML != "" {
					summary := editSummary(fp, old, new_)
					return fmt.Sprintf(`<div class="msg msg-tool"><details class="tool-chip"><summary class="tool-name">⚙ %s</summary><div class="tool-detail">%s</div></details></div>`, Esc(summary), diffHTML)
				}
			}
		}
		detail := FormatToolInput(msg.Tool, msg.Input)
		return fmt.Sprintf(`<div class="msg msg-tool"><details class="tool-chip"><summary class="tool-name">⚙ %s</summary><div class="tool-detail">%s</div></details></div>`, Esc(msg.Tool), Esc(detail))
	case "tool_result":
		return fmt.Sprintf(`<div class="msg msg-tool"><details class="tool-result-chip"><summary class="tool-result-summary">result</summary><div class="tool-result-full">%s</div></details></div>`, Esc(msg.Output))
	case "error":
		return fmt.Sprintf(`<div class="msg"><div class="msg-error">✗ %s</div></div>`, Esc(msg.Error))
	case "permission_request":
		return RenderPermission(msg)
	case "result":
		return ""
	}
	return ""
}

func RenderPermission(msg ServerMsg) string {
	var detailHTML string
	if msg.PermTool == "Edit" || msg.PermTool == "FileEdit" {
		if fp, old, new_, ok := editDiffFromInput(msg.PermInput); ok {
			detailHTML = RenderEditDiffHTML(fp, old, new_)
		}
	}
	if detailHTML == "" {
		detailHTML = Esc(FormatPermDetail(msg.PermTool, msg.PermInput))
	}
	var suggBtns strings.Builder
	if suggestions, ok := msg.PermSuggestions.([]any); ok {
		for _, s := range suggestions {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			var label string
			switch sm["type"] {
			case "setMode":
				mode, _ := sm["mode"].(string)
				if mode == "acceptEdits" {
					label = "Accept Edits"
				} else {
					label = mode
				}
			case "addDirectories":
				dirs, _ := sm["directories"].([]any)
				var ds []string
				for _, d := range dirs {
					if ds_, ok := d.(string); ok {
						ds = append(ds, ds_)
					}
				}
				label = "Add " + strings.Join(ds, ", ")
			default:
				t, _ := sm["type"].(string)
				label = t
			}
			sJSON, _ := json.Marshal(s)
			fmt.Fprintf(&suggBtns,
				`<form hx-post="/perm" hx-swap="none" style="flex:1"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="perm_id" value="%s"><input type="hidden" name="allow" value="true"><input type="hidden" name="suggestion" value="%s"><button type="submit" class="perm-allow" style="width:100%%;font-size:11px">%s</button></form>`,
				Esc(msg.SessionID), Esc(msg.PermID), Esc(string(sJSON)), Esc(label),
			)
		}
	}

	return fmt.Sprintf(`<div class="perm-prompt" id="perm-%s">
<div class="perm-header">Permission Required</div>
<div class="perm-tool">⚙ %s</div>
%s
<div class="perm-detail">%s</div>
<div class="perm-actions" id="perm-actions-%s">
<form hx-post="/perm" hx-swap="none" style="flex:1"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="perm_id" value="%s"><input type="hidden" name="allow" value="false"><button type="submit" class="perm-deny" style="width:100%%">Deny</button></form>
<form hx-post="/perm" hx-swap="none" style="flex:1"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="perm_id" value="%s"><input type="hidden" name="allow" value="true"><button type="submit" class="perm-allow" style="width:100%%">Allow</button></form>
%s
</div></div>`,
		Esc(msg.PermID), Esc(msg.PermTool),
		func() string {
			if msg.PermReason != "" {
				return fmt.Sprintf(`<div class="perm-reason">%s</div>`, Esc(msg.PermReason))
			}
			return ""
		}(),
		detailHTML, Esc(msg.PermID),
		Esc(msg.SessionID), Esc(msg.PermID),
		Esc(msg.SessionID), Esc(msg.PermID),
		suggBtns.String(),
	)
}

func RenderCostBar(s *Session) string {
	s.Mu.Lock()
	c := s.CostAccum
	ds := s.DiffStat
	sid := s.ClaudeID
	s.Mu.Unlock()
	var parts []string
	if c.TotalCostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", c.TotalCostUSD))
	}
	if c.ContextUsed > 0 && c.ContextWindow > 0 {
		pct := 100 * c.ContextUsed / c.ContextWindow
		parts = append(parts, fmt.Sprintf("context %s/%s (%d%%)", FmtK(c.ContextUsed), FmtK(c.ContextWindow), pct))
	} else if c.ContextUsed > 0 {
		parts = append(parts, fmt.Sprintf("context %s", FmtK(c.ContextUsed)))
	}
	if ds.Added > 0 || ds.Removed > 0 {
		parts = append(parts, RenderDiffStat(sid, ds))
	}
	return strings.Join(parts, " · ")
}

func RenderQueueBar(sessionID, text string) string {
	if text == "" {
		return OobSwap("queue-bar", "innerHTML", "")
	}
	return OobSwap("queue-bar", "innerHTML", fmt.Sprintf(
		`<div class="queue-content"><span class="queue-label">queued:</span> <span class="queue-text">%s</span><form hx-post="/cancel-queue" hx-swap="none" style="display:inline"><input type="hidden" name="session_id" value="%s"><button type="submit" class="queue-cancel">✕</button></form></div>`,
		Esc(text), Esc(sessionID),
	))
}

func FmtK(n int) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

func TimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
}

func ShortPath(p string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// --- Todos rendering ---

func ParseTodos(input any) []Todo {
	m, ok := input.(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := m["todos"].([]any)
	if !ok {
		return nil
	}
	var todos []Todo
	for _, item := range arr {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content, _ := t["content"].(string)
		activeForm, _ := t["activeForm"].(string)
		status, _ := t["status"].(string)
		if content != "" {
			todos = append(todos, Todo{Content: content, ActiveForm: activeForm, Status: status})
		}
	}
	return todos
}

func RenderTodosSummary(todos []Todo) string {
	if len(todos) == 0 {
		return ""
	}
	done := 0
	for _, t := range todos {
		if t.Status == "completed" {
			done++
		}
	}
	return fmt.Sprintf("Todos (%d/%d)", done, len(todos))
}

func RenderTodosBody(todos []Todo) string {
	if len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range todos {
		var icon string
		var cls string
		switch t.Status {
		case "completed":
			icon = "&#x2713;"
			cls = "todo-done"
		case "in_progress":
			icon = "&#x25CB;"
			cls = "todo-active"
		default:
			icon = "&#x25CB;"
			cls = "todo-pending"
		}
		label := t.Content
		if t.Status == "in_progress" && t.ActiveForm != "" {
			label = t.ActiveForm
		}
		fmt.Fprintf(&b, `<div class="todo-item %s"><span class="todo-icon">%s</span> %s</div>`, cls, icon, Esc(label))
	}
	return b.String()
}

// --- SSE format helpers ---

func FormatSSE(event, data string) string {
	// Strip \r — SSE treats \r as a line terminator, so stray CRs
	// (e.g. from CRLF textarea submissions) would split data lines
	// and silently drop content.
	data = strings.ReplaceAll(data, "\r", "")
	var buf strings.Builder
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\n")
	for _, line := range strings.Split(data, "\n") {
		buf.WriteString("data: ")
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
	return buf.String()
}

func splitDiffByFile(fullDiff string) []string {
	var chunks []string
	var current strings.Builder
	for _, line := range strings.SplitAfter(fullDiff, "\n") {
		if strings.HasPrefix(line, "diff --git ") && current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

func RenderDiffPage(sessionID, cwd string, files []DiffFile, fullDiff string) string {
	var b strings.Builder

	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Diff · `)
	b.WriteString(Esc(ShortPath(cwd)))
	b.WriteString(`</title>
<style>
  @import url('https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;600&family=DM+Sans:wght@400;500;600;700&display=swap');
  :root { --bg: #0c0c0e; --surface: #16161a; --surface2: #1e1e24; --border: #2a2a32; --text: #e2e0d8; --text2: #8b8a85; --accent: #d4a053; --tool: #5b8a72; --tool-bg: #1a2e24; --error: #c45c5c; --blue: #5b7a9e; }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { background: var(--bg); color: var(--text); font-family: 'DM Sans', sans-serif; }
  .diff-header { display: flex; align-items: center; gap: 12px; padding: 12px 16px; border-bottom: 1px solid var(--border); background: var(--surface); position: sticky; top: 0; z-index: 10; }
  .diff-header a { color: var(--accent); text-decoration: none; font-size: 14px; }
  .diff-header h1 { font-size: 14px; font-weight: 600; color: var(--text); }
  .diff-files { padding: 12px 16px; border-bottom: 1px solid var(--border); background: var(--surface2); }
  .diff-files a { color: var(--blue); text-decoration: none; font-family: 'JetBrains Mono', monospace; font-size: 12px; display: block; padding: 2px 0; }
  .diff-files a:hover { color: var(--text); }
  .diff-badge { display: inline-block; width: 16px; text-align: center; font-size: 10px; font-weight: 600; margin-right: 6px; border-radius: 3px; }
  .diff-badge-M { color: var(--accent); }
  .diff-badge-A { color: #589819; }
  .diff-badge-D { color: var(--error); }
  .diff-section { padding: 0 16px 24px; }
  .diff-section-header { font-family: 'JetBrains Mono', monospace; font-size: 12px; color: var(--text2); padding: 12px 0 8px; border-bottom: 1px solid var(--border); margin-bottom: 8px; }
  .diff-section pre { border-radius: 6px; font-size: 11px; line-height: 1.4; overflow-x: auto; }
  .diff-empty { padding: 40px; text-align: center; color: var(--text2); font-size: 14px; }
</style></head><body>
<div class="diff-header">
  <a href="/?session=`)
	b.WriteString(Esc(sessionID))
	b.WriteString(`">← back</a>
  <h1>`)
	b.WriteString(Esc(ShortPath(cwd)))
	b.WriteString(`</h1>
</div>`)

	if len(files) == 0 {
		b.WriteString(`<div class="diff-empty">No uncommitted changes</div>`)
		b.WriteString(`</body></html>`)
		return b.String()
	}

	// File list
	b.WriteString(`<div class="diff-files">`)
	for _, f := range files {
		badge := f.Status[:1]
		fmt.Fprintf(&b, `<a href="#%s"><span class="diff-badge diff-badge-%s">%s</span>%s</a>`,
			Esc(f.Name), Esc(badge), Esc(badge), Esc(f.Name))
	}
	b.WriteString(`</div>`)

	// Per-file diffs
	chunks := splitDiffByFile(fullDiff)
	for _, chunk := range chunks {
		// Extract filename from "diff --git a/foo b/foo"
		firstLine := chunk
		if idx := strings.Index(chunk, "\n"); idx >= 0 {
			firstLine = chunk[:idx]
		}
		name := ""
		if strings.HasPrefix(firstLine, "diff --git ") {
			parts := strings.Fields(firstLine)
			if len(parts) >= 4 {
				name = strings.TrimPrefix(parts[3], "b/")
			}
		}
		fmt.Fprintf(&b, `<div class="diff-section" id="%s">`, Esc(name))
		fmt.Fprintf(&b, `<div class="diff-section-header">%s</div>`, Esc(name))
		b.WriteString(highlightDiff(chunk))
		b.WriteString(`</div>`)
	}

	b.WriteString(`</body></html>`)
	return b.String()
}

func OobSwap(id, strategy, content string) string {
	return fmt.Sprintf(`<div id="%s" hx-swap-oob="%s">%s</div>`, id, strategy, content)
}
