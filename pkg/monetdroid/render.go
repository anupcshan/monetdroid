package monetdroid

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList),
	goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
)

var imgDlgSeq atomic.Int64

func Esc(s string) string { return html.EscapeString(s) }

func RenderMarkdown(text string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return Esc(text)
	}
	return strings.ReplaceAll(buf.String(), "<a ", `<a target="_blank" rel="noopener" `)
}

// ToolChipSummary returns a compact one-line summary for the tool chip header.
func ToolChipSummary(tool string, input *ToolInput) string {
	if input == nil {
		return tool
	}
	filePath := input.ResolvedPath()
	short := func(p string) string {
		return filepath.Base(p)
	}
	switch tool {
	case "Read", "FileRead":
		if filePath == "" {
			return "Read"
		}
		name := short(filePath)
		if input.Offset > 0 && input.Limit > 0 {
			return fmt.Sprintf("Read %s:%d-%d", name, input.Offset, input.Offset+input.Limit)
		}
		if input.Offset > 0 {
			return fmt.Sprintf("Read %s:%d+", name, input.Offset)
		}
		return "Read " + name
	case "Write", "FileWrite":
		if filePath != "" {
			return "Write " + short(filePath)
		}
		return "Write"
	case "Grep":
		if input.Pattern == "" {
			return "Grep"
		}
		if filePath != "" {
			return fmt.Sprintf("Grep /%s/ in %s", input.Pattern, short(filePath))
		}
		return fmt.Sprintf("Grep /%s/", input.Pattern)
	case "Glob":
		if input.Pattern != "" {
			return "Glob " + input.Pattern
		}
		return "Glob"
	case "Bash":
		if input.Command != "" {
			return input.Command
		}
		return "Bash"
	}
	return tool
}

func FormatToolInput(tool string, input *ToolInput) string {
	if input == nil {
		return ""
	}
	filePath := input.ResolvedPath()
	switch tool {
	case "Bash":
		if input.Command != "" {
			return input.Command
		}
	case "Read", "FileRead":
		if filePath != "" {
			return filePath
		}
	case "Write", "FileWrite":
		content := input.Content
		if len(content) > 200 {
			content = content[:200]
		}
		return filePath + "\n" + content
	case "Edit", "FileEdit":
		var lines []string
		lines = append(lines, filePath)
		if input.OldString != "" {
			lines = append(lines, "--- old ---", input.OldString)
		}
		if input.NewString != "" {
			lines = append(lines, "+++ new +++", input.NewString)
		}
		return strings.Join(lines, "\n")
	case "Grep":
		if input.Pattern != "" {
			return input.Pattern
		}
	case "Glob":
		if input.Pattern != "" {
			return input.Pattern
		}
	}
	j, _ := json.MarshalIndent(input, "", "  ")
	return string(j)
}

func FormatPermDetail(tool string, input *ToolInput) string {
	if input == nil {
		return ""
	}
	filePath := input.ResolvedPath()
	switch tool {
	case "Bash":
		if input.Description != "" {
			return input.Description + "\n\n" + input.Command
		}
		return input.Command
	case "Edit", "FileEdit":
		var lines []string
		lines = append(lines, filePath)
		if input.OldString != "" {
			lines = append(lines, "--- old ---", input.OldString)
		}
		if input.NewString != "" {
			lines = append(lines, "+++ new +++", input.NewString)
		}
		return strings.Join(lines, "\n")
	case "Write", "FileWrite":
		return filePath + "\n\n" + input.Content
	case "Read", "FileRead":
		return filePath
	}
	j, _ := json.MarshalIndent(input, "", "  ")
	return string(j)
}

var chromaFormatter = chromahtml.New()
var chromaStyle = styles.Get("vim")

func runDiff(filePath, oldStr, newStr string, context int) string {
	oldFile, err := os.CreateTemp("", "diff-old-*")
	if err != nil {
		return ""
	}
	defer os.Remove(oldFile.Name())
	newFile, err := os.CreateTemp("", "diff-new-*")
	if err != nil {
		return ""
	}
	defer os.Remove(newFile.Name())

	if !strings.HasSuffix(oldStr, "\n") {
		oldStr += "\n"
	}
	if !strings.HasSuffix(newStr, "\n") {
		newStr += "\n"
	}
	oldFile.WriteString(oldStr)
	oldFile.Close()
	newFile.WriteString(newStr)
	newFile.Close()

	cmd := exec.Command("diff", "-w",
		fmt.Sprintf("-U%d", context),
		fmt.Sprintf("--label=%s", filePath),
		fmt.Sprintf("--label=%s", filePath),
		oldFile.Name(), newFile.Name())
	out, _ := cmd.Output()
	// diff exits 1 when files differ, which is normal
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() > 1 {
		return ""
	}
	return string(out)
}

func RenderEditDiffHTML(filePath, oldStr, newStr string) string {
	diffText := runDiff(filePath, oldStr, newStr, 3)
	if diffText == "" {
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

func editDiffFromInput(input *ToolInput) (filePath, oldStr, newStr string, ok bool) {
	if input == nil {
		return
	}
	filePath = input.ResolvedPath()
	oldStr = input.OldString
	newStr = input.NewString
	ok = true
	return
}

func editSummary(filePath, oldStr, newStr string) string {
	diffText := runDiff(filePath, oldStr, newStr, 0)
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
		for _, img := range msg.Images {
			dlgID := fmt.Sprintf("img-dlg-%d", imgDlgSeq.Add(1))
			src := fmt.Sprintf("data:%s;base64,%s", Esc(img.MediaType), img.Data)
			fmt.Fprintf(&content, `<img src="%s" class="msg-img-thumb" onclick="document.getElementById('%s').showModal()">`, src, dlgID)
			fmt.Fprintf(&content, `<dialog id="%s" class="img-dialog" onclick="this.close()"><img src="%s"></dialog>`, dlgID, src)
		}
		content.WriteString(strings.ReplaceAll(Esc(msg.Text), "\n", "<br>"))
		return fmt.Sprintf(`<div class="msg msg-user"><div class="msg-bubble">%s</div></div>`, content.String())
	case "text":
		rendered := RenderMarkdown(msg.Text)
		if strings.TrimSpace(rendered) == "" {
			return ""
		}
		return fmt.Sprintf(`<div class="msg msg-assistant"><div class="msg-bubble">%s</div></div>`, rendered)
	case "tool_use":
		if msg.Tool == "TodoWrite" {
			return ""
		}
		if msg.Tool == "AskUserQuestion" {
			return RenderAskUserStatic(msg.Input)
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
		var spinnerHTML string
		if msg.Tool == "Bash" {
			spinnerHTML = fmt.Sprintf(` <span class="tool-spinner" id="spinner-%s"><span class="spinner-dots"><span></span><span></span><span></span></span> <span class="tool-elapsed" data-started="%d"></span></span>`, Esc(msg.ToolUseID), time.Now().UnixMilli())
		}
		summary := ToolChipSummary(msg.Tool, msg.Input)
		detail := FormatToolInput(msg.Tool, msg.Input)
		return fmt.Sprintf(`<div class="msg msg-tool" id="tool-%s"><details class="tool-chip"><summary class="tool-name">⚙ %s%s</summary><div class="tool-detail">%s</div></details></div>`, Esc(msg.ToolUseID), Esc(summary), spinnerHTML, Esc(detail))
	case "tool_result":
		return fmt.Sprintf(`<div class="msg msg-tool"><details class="tool-result-chip"><summary class="tool-result-summary">result</summary><div class="tool-result-full">%s</div></details></div>`, Esc(msg.Output))
	case "error":
		return fmt.Sprintf(`<div class="msg"><div class="msg-error">✗ %s</div></div>`, Esc(msg.Error))
	case "permission_request":
		return RenderPermission(msg)
	case "compact_boundary":
		return `<div class="compact-boundary"><span>context compacted</span></div>`
	case "result":
		return ""
	}
	return ""
}

func RenderPermission(msg ServerMsg) string {
	if msg.PermTool == "AskUserQuestion" {
		return RenderAskUser(msg)
	}
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
	for _, s := range msg.PermSuggestions {
		var label string
		switch s.Type {
		case "setMode":
			if s.Mode == "acceptEdits" {
				label = "Accept Edits"
			} else {
				label = s.Mode
			}
		case "addDirectories":
			label = "Add " + strings.Join(s.Directories, ", ")
		default:
			label = s.Type
		}
		sJSON, _ := json.Marshal(s)
		fmt.Fprintf(&suggBtns,
			`<form hx-post="/perm" hx-swap="none" style="flex:1"><input type="hidden" name="session_id" value="%s"><input type="hidden" name="perm_id" value="%s"><input type="hidden" name="allow" value="true"><input type="hidden" name="suggestion" value="%s"><button type="submit" class="perm-allow" style="width:100%%;font-size:11px">%s</button></form>`,
			Esc(msg.SessionID), Esc(msg.PermID), Esc(string(sJSON)), Esc(label),
		)
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

func RenderAskUser(msg ServerMsg) string {
	if msg.PermInput == nil || len(msg.PermInput.Questions) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<div class="perm-prompt ask-user" id="perm-%s">`, Esc(msg.PermID))
	fmt.Fprintf(&b, `<form hx-post="/perm-answer" hx-swap="none">`)
	fmt.Fprintf(&b, `<input type="hidden" name="session_id" value="%s">`, Esc(msg.SessionID))
	fmt.Fprintf(&b, `<input type="hidden" name="perm_id" value="%s">`, Esc(msg.PermID))

	for qi, q := range msg.PermInput.Questions {
		b.WriteString(`<div class="ask-question">`)
		if q.Header != "" {
			fmt.Fprintf(&b, `<div class="ask-header">%s</div>`, Esc(q.Header))
		}
		fmt.Fprintf(&b, `<div class="ask-text">%s</div>`, Esc(q.Question))

		fieldName := fmt.Sprintf("answer_%d", qi)
		inputType := "radio"
		if q.MultiSelect {
			inputType = "checkbox"
		}

		for oi, o := range q.Options {
			optID := fmt.Sprintf("opt-%s-%d-%d", msg.PermID, qi, oi)
			fmt.Fprintf(&b, `<label class="ask-option" for="%s">`, optID)
			fmt.Fprintf(&b, `<input type="%s" id="%s" name="%s" value="%s">`,
				inputType, optID, Esc(fieldName), Esc(o.Label))
			fmt.Fprintf(&b, `<span class="ask-label">%s</span>`, Esc(o.Label))
			if o.Description != "" {
				fmt.Fprintf(&b, `<span class="ask-desc">%s</span>`, Esc(o.Description))
			}
			b.WriteString(`</label>`)
		}

		// "Other" free-text option
		otherID := fmt.Sprintf("opt-%s-%d-other", msg.PermID, qi)
		fmt.Fprintf(&b, `<label class="ask-option" for="%s">`, otherID)
		fmt.Fprintf(&b, `<input type="%s" id="%s" name="%s" value="__other__">`,
			inputType, otherID, Esc(fieldName))
		fmt.Fprintf(&b, `<span class="ask-label">Other</span>`)
		b.WriteString(`</label>`)
		fmt.Fprintf(&b, `<input type="text" name="%s_other" class="ask-other-text" placeholder="Type your answer...">`,
			Esc(fieldName))

		b.WriteString(`</div>`)
	}

	fmt.Fprintf(&b, `<div class="perm-actions" id="perm-actions-%s">`, Esc(msg.PermID))
	b.WriteString(`<button type="submit" class="perm-allow" style="width:100%">Submit</button>`)
	b.WriteString(`</div>`)
	b.WriteString(`</form></div>`)
	return b.String()
}

// RenderAskUserStatic renders a read-only Q&A summary for history/replay.
func RenderAskUserStatic(input *ToolInput) string {
	if input == nil || len(input.Questions) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(`<div class="perm-prompt ask-user">`)
	for _, q := range input.Questions {
		b.WriteString(`<div class="ask-question">`)
		if q.Header != "" {
			fmt.Fprintf(&b, `<div class="ask-header">%s</div>`, Esc(q.Header))
		}
		if ans, ok := input.Answers[q.Question]; ok {
			fmt.Fprintf(&b, `<div class="ask-answered"><span class="ask-text">%s</span> <span style="color:var(--tool)">%s</span></div>`, Esc(q.Question), Esc(ans))
		} else {
			fmt.Fprintf(&b, `<div class="ask-text">%s</div>`, Esc(q.Question))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
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

// FormatTokens formats token counts like "62k" or "62k/200k".
func FormatTokens(used, window int) string {
	fmtK := func(n int) string {
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	if window > 0 {
		return fmtK(used) + "/" + fmtK(window)
	}
	return fmtK(used)
}

func ParseTodos(input *ToolInput) []Todo {
	if input == nil {
		return nil
	}
	return input.Todos
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

func RenderQueue(items []QueueItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		statusIcon := "&#x2713;"
		statusClass := "queue-completed"
		statusLabel := "Completed"
		if item.Status == "blocked" {
			statusIcon = "&#x25CF;"
			statusClass = "queue-blocked"
			statusLabel = "Blocked"
		}
		result := item.Result
		if len(result) > 120 {
			result = result[:120] + "..."
		}
		fmt.Fprintf(&b, `<div class="queue-item %s">`, statusClass)
		fmt.Fprintf(&b, `<div class="qi-header"><span class="qi-status">%s %s</span><span class="qi-time">%s</span></div>`,
			statusIcon, Esc(statusLabel), Esc(TimeAgo(parseTime(item.Timestamp))))
		displayLabel := item.Label
		if item.AutoLabel && displayLabel != "" {
			displayLabel = "(auto) " + displayLabel
		}
		fmt.Fprintf(&b, `<div class="qi-label">%s</div>`, Esc(displayLabel))
		if item.Cwd != "" {
			fmt.Fprintf(&b, `<div class="qi-cwd">%s</div>`, Esc(ShortPath(item.Cwd)))
		}
		if result != "" {
			fmt.Fprintf(&b, `<div class="qi-result">%s</div>`, Esc(result))
		}
		fmt.Fprintf(&b, `<div class="qi-actions">`+
			`<form hx-post="/ack" hx-swap="none" hx-on::after-request="this.closest('.queue-item').remove()" style="display:inline">`+
			`<input type="hidden" name="claude_id" value="%s">`+
			`<button type="submit" class="qi-dismiss">Dismiss</button></form> `+
			`<a href="/?session=%s" class="qi-open">Open</a>`+
			`</div>`, Esc(item.ClaudeID), Esc(item.ClaudeID))
		b.WriteString(`</div>`)
	}
	return b.String()
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// stripSpinner removes the spinner span from a rendered tool_use HTML string.
func stripSpinner(html, toolUseID string) string {
	// The spinner is: <span class="tool-spinner" id="spinner-...">...</span>
	tag := fmt.Sprintf(`<span class="tool-spinner" id="spinner-%s">`, Esc(toolUseID))
	start := strings.Index(html, tag)
	if start < 0 {
		return html
	}
	// Find the matching closing </span> — the spinner has nested spans,
	// so count open/close tags.
	depth := 0
	i := start
	for i < len(html) {
		if strings.HasPrefix(html[i:], "<span") {
			depth++
			i += 5
		} else if strings.HasPrefix(html[i:], "</span>") {
			depth--
			if depth == 0 {
				return html[:start] + html[i+7:]
			}
			i += 7
		} else {
			i++
		}
	}
	return html
}

func OobSwap(id, strategy, content string) string {
	return fmt.Sprintf(`<div id="%s" hx-swap-oob="%s">%s</div>`, id, strategy, content)
}
