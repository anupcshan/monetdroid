package monetdroid

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html"
	"net/url"
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
	short := filepath.Base
	switch {
	case input.Read != nil:
		if input.Read.FilePath == "" {
			return "Read"
		}
		name := short(input.Read.FilePath)
		if input.Read.Offset > 0 && input.Read.Limit > 0 {
			return fmt.Sprintf("Read %s:%d-%d", name, input.Read.Offset, input.Read.Offset+input.Read.Limit)
		}
		if input.Read.Offset > 0 {
			return fmt.Sprintf("Read %s:%d+", name, input.Read.Offset)
		}
		return "Read " + name
	case input.Write != nil:
		if input.Write.FilePath != "" {
			return "Write " + short(input.Write.FilePath)
		}
		return "Write"
	case input.Grep != nil:
		if input.Grep.Pattern == "" {
			return "Grep"
		}
		if input.Grep.Path != "" {
			return fmt.Sprintf("Grep /%s/ in %s", input.Grep.Pattern, short(input.Grep.Path))
		}
		return fmt.Sprintf("Grep /%s/", input.Grep.Pattern)
	case input.Glob != nil:
		if input.Glob.Pattern != "" {
			return "Glob " + input.Glob.Pattern
		}
		return "Glob"
	case input.Bash != nil:
		if input.Bash.Command != "" {
			return input.Bash.Command
		}
		return "Bash"
	case input.Agent != nil:
		var parts []string
		if input.Agent.SubagentType != "" {
			parts = append(parts, fmt.Sprintf("Agent (%s)", strings.ToLower(input.Agent.SubagentType)))
		} else {
			parts = append(parts, "Agent")
		}
		if input.Agent.Description != "" {
			parts = append(parts, input.Agent.Description)
		}
		return strings.Join(parts, ": ")
	}
	return tool
}

func FormatToolInput(tool string, input *ToolInput) string {
	if input == nil {
		return ""
	}
	switch {
	case input.Bash != nil:
		if input.Bash.Command != "" {
			return input.Bash.Command
		}
	case input.Read != nil:
		if input.Read.FilePath != "" {
			return input.Read.FilePath
		}
	case input.Write != nil:
		content := input.Write.Content
		if len(content) > 200 {
			content = content[:200]
		}
		return input.Write.FilePath + "\n" + content
	case input.Edit != nil:
		var lines []string
		lines = append(lines, input.Edit.FilePath)
		if input.Edit.OldString != "" {
			lines = append(lines, "--- old ---", input.Edit.OldString)
		}
		if input.Edit.NewString != "" {
			lines = append(lines, "+++ new +++", input.Edit.NewString)
		}
		return strings.Join(lines, "\n")
	case input.Grep != nil:
		if input.Grep.Pattern != "" {
			return input.Grep.Pattern
		}
	case input.Glob != nil:
		if input.Glob.Pattern != "" {
			return input.Glob.Pattern
		}
	}
	// Fallback: pretty-print raw JSON (works for unknown tools)
	if input.Raw != nil {
		var buf bytes.Buffer
		json.Indent(&buf, input.Raw, "", "  ")
		return buf.String()
	}
	return ""
}

func FormatPermDetail(tool string, input *ToolInput) string {
	if input == nil {
		return ""
	}
	switch {
	case input.Bash != nil:
		if input.Bash.Description != "" {
			return input.Bash.Description + "\n\n" + input.Bash.Command
		}
		return input.Bash.Command
	case input.Edit != nil:
		var lines []string
		lines = append(lines, input.Edit.FilePath)
		if input.Edit.OldString != "" {
			lines = append(lines, "--- old ---", input.Edit.OldString)
		}
		if input.Edit.NewString != "" {
			lines = append(lines, "+++ new +++", input.Edit.NewString)
		}
		return strings.Join(lines, "\n")
	case input.Write != nil:
		return input.Write.FilePath + "\n\n" + input.Write.Content
	case input.Read != nil:
		return input.Read.FilePath
	}
	// Fallback: pretty-print raw JSON (works for unknown tools)
	if input.Raw != nil {
		var buf bytes.Buffer
		json.Indent(&buf, input.Raw, "", "  ")
		return buf.String()
	}
	return ""
}

var chromaFormatter = chromahtml.New()
var chromaStyle = styles.Get("onedark")

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

func splitDiffByFile(fullDiff string) []string {
	var chunks []string
	var current strings.Builder
	for _, line := range strings.Split(fullDiff, "\n") {
		if strings.HasPrefix(line, "diff --git ") && current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}

func editDiffFromInput(input *ToolInput) (filePath, oldStr, newStr string, ok bool) {
	if input == nil || input.Edit == nil {
		return
	}
	filePath = input.Edit.FilePath
	oldStr = input.Edit.OldString
	newStr = input.Edit.NewString
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
			fmt.Fprintf(&content, `<dialog id="%s" class="img-dialog" onclick="this.close()"><img src="%s" onclick="event.stopPropagation()"></dialog>`, dlgID, src)
		}
		content.WriteString(strings.ReplaceAll(Esc(msg.Text), "\n", "<br>"))
		return fmt.Sprintf(`<div class="msg msg-user"><div class="msg-bubble">%s</div></div>`, content.String())
	case "thinking":
		if strings.TrimSpace(msg.Text) == "" {
			return ""
		}
		preview := msg.Text
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		return fmt.Sprintf(`<div class="msg msg-thinking"><details class="thinking-chip"><summary class="thinking-summary">%s</summary><div class="thinking-detail">%s</div></details></div>`, Esc(preview), Esc(msg.Text))
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
		if msg.Tool == "Bash" || msg.Tool == "Agent" {
			spinnerHTML = fmt.Sprintf(` <span class="tool-spinner" id="spinner-%s"><span class="spinner-dots"><span></span><span></span><span></span></span> <span class="tool-elapsed" id="elapsed-%s"></span></span>`, Esc(msg.ToolUseID), Esc(msg.ToolUseID))
		}
		summary := ToolChipSummary(msg.Tool, msg.Input)
		detail := FormatToolInput(msg.Tool, msg.Input)
		extraSlot := ""
		if msg.Tool == "Bash" {
			extraSlot = fmt.Sprintf(`<div id="bg-slot-%s"></div>`, Esc(msg.ToolUseID))
		}
		if msg.Tool == "Agent" {
			statsHTML := fmt.Sprintf(`<span class="agent-stats" id="agent-stats-%s"></span>`, Esc(msg.ToolUseID))
			agentSlot := RenderAgentSlot(msg.SessionID, msg.ToolUseID)
			return fmt.Sprintf(`<div class="msg msg-tool" id="tool-%s"><details class="tool-chip"><summary class="tool-name">⚙ %s%s %s</summary>%s</details></div>`,
				Esc(msg.ToolUseID), Esc(summary), spinnerHTML, statsHTML, agentSlot)
		}
		return fmt.Sprintf(`<div class="msg msg-tool" id="tool-%s"><details class="tool-chip"><summary class="tool-name">⚙ %s%s</summary><div class="tool-detail">%s</div>%s</details></div>`, Esc(msg.ToolUseID), Esc(summary), spinnerHTML, Esc(detail), extraSlot)
	case "tool_result":
		if len(msg.Images) > 0 {
			var content strings.Builder
			for _, img := range msg.Images {
				dlgID := fmt.Sprintf("img-dlg-%d", imgDlgSeq.Add(1))
				src := fmt.Sprintf("data:%s;base64,%s", Esc(img.MediaType), img.Data)
				fmt.Fprintf(&content, `<img src="%s" class="msg-img-thumb" onclick="document.getElementById('%s').showModal()">`, src, dlgID)
				fmt.Fprintf(&content, `<dialog id="%s" class="img-dialog" onclick="this.close()"><img src="%s" onclick="event.stopPropagation()"></dialog>`, dlgID, src)
			}
			return fmt.Sprintf(`<div class="msg msg-tool">%s</div>`, content.String())
		}
		return fmt.Sprintf(`<div class="msg msg-tool"><details class="tool-result-chip"><summary class="tool-result-summary">result</summary><div class="tool-result-full">%s</div></details></div>`, Esc(msg.Output))
	case "error":
		return fmt.Sprintf(`<div class="msg"><div class="msg-error">✗ %s</div></div>`, Esc(msg.Error))
	case "permission_request":
		return RenderPermission(msg)
	case "agent_progress":
		return "" // handled via OOB swap in hub.go
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
	if msg.PermInput == nil || msg.PermInput.Ask == nil || len(msg.PermInput.Ask.Questions) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<div class="perm-prompt ask-user" id="perm-%s">`, Esc(msg.PermID))
	fmt.Fprintf(&b, `<form hx-post="/perm-answer" hx-swap="none">`)
	fmt.Fprintf(&b, `<input type="hidden" name="session_id" value="%s">`, Esc(msg.SessionID))
	fmt.Fprintf(&b, `<input type="hidden" name="perm_id" value="%s">`, Esc(msg.PermID))

	for qi, q := range msg.PermInput.Ask.Questions {
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
	if input == nil || input.Ask == nil || len(input.Ask.Questions) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(`<div class="perm-prompt ask-user">`)
	for _, q := range input.Ask.Questions {
		b.WriteString(`<div class="ask-question">`)
		if q.Header != "" {
			fmt.Fprintf(&b, `<div class="ask-header">%s</div>`, Esc(q.Header))
		}
		if ans, ok := input.Ask.Answers[q.Question]; ok {
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
	c, ds := s.GetCostBarInfo()
	sid := s.ID
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
	parts = append(parts, RenderDiffStat(sid, ds))
	return strings.Join(parts, " · ")
}

func RenderQueueBar(sessionID, text string) string {
	if text == "" {
		return OobSwap("queue-bar", "innerHTML", "")
	}
	return OobSwap("queue-bar", "innerHTML", fmt.Sprintf(
		`<div class="queue-content">`+
			`<span class="queue-label">queued:</span>`+
			`<span class="queue-preview">%s</span>`+
			`<button class="queue-btn" hx-post="/cancel-queue" hx-vals='{"session_id":"%s","edit":"true"}' hx-target="#queue-bar" hx-swap="innerHTML">Edit</button>`+
			`<button class="queue-btn queue-cancel" hx-post="/cancel-queue" hx-vals='{"session_id":"%s"}' hx-target="#queue-bar" hx-swap="innerHTML">✕</button>`+
			`</div>`,
		Esc(text), Esc(sessionID), Esc(sessionID),
	))
}

func RenderQueueEdit(sessionID, text string) string {
	return fmt.Sprintf(
		`<div class="queue-content queue-editing">`+
			`<form hx-post="/send" hx-swap="none">`+
			`<input type="hidden" name="session_id" value="%s">`+
			`<textarea class="queue-text" name="text">%s</textarea>`+
			`<div class="queue-actions">`+
			`<button type="submit" class="queue-btn queue-send">Send</button>`+
			`<button type="button" class="queue-btn queue-cancel" hx-post="/cancel-queue" hx-vals='{"session_id":"%s"}' hx-target="#queue-bar" hx-swap="innerHTML">✕</button>`+
			`</div></form></div>`,
		Esc(sessionID), Esc(text), Esc(sessionID),
	)
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
	if input == nil || input.Todo == nil {
		return nil
	}
	return input.Todo.Todos
}

func RenderTodosSummary(todos []Todo) string {
	if len(todos) == 0 {
		return ""
	}
	done := 0
	var active string
	for _, t := range todos {
		if t.Status == "completed" {
			done++
		} else if t.Status == "in_progress" && active == "" {
			active = t.ActiveForm
			if active == "" {
				active = t.Content
			}
		}
	}
	summary := fmt.Sprintf("Todos (%d/%d)", done, len(todos))
	if active != "" {
		const maxLen = 50
		if len(active) > maxLen {
			active = active[:maxLen] + "…"
		}
		summary += " · " + Esc(active)
	}
	return summary
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

func renderBranchChips(branches []string) string {
	if len(branches) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="hi-branches">`)
	for _, br := range branches {
		fmt.Fprintf(&b, `<span class="branch-chip">%s</span>`, Esc(br))
	}
	b.WriteString(`</div>`)
	return b.String()
}

func RenderTrackedSessions(t *GitTrace, items []TrackedSession) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		statusIcon := "&#x2713;"
		statusClass := "queue-completed"
		statusLabel := "Completed"
		switch item.Status {
		case "blocked":
			statusIcon = "&#x25CF;"
			statusClass = "queue-blocked"
			statusLabel = "Blocked"
		case "running":
			statusIcon = "&#x25CB;"
			statusClass = "queue-running"
			statusLabel = "Running"
		}
		result := item.Result
		if len(result) > 120 {
			result = result[:120] + "..."
		}
		fmt.Fprintf(&b, `<div class="queue-item %s">`, statusClass)
		fmt.Fprintf(&b, `<div class="qi-header"><span class="qi-status">%s %s</span><span class="qi-time">%s</span></div>`,
			statusIcon, Esc(statusLabel), Esc(TimeAgo(time.UnixMilli(item.UpdatedAtMillis))))
		displayLabel := item.Label
		if item.AutoLabel && displayLabel != "" {
			displayLabel = "(auto) " + displayLabel
		}
		fmt.Fprintf(&b, `<div class="qi-label"><span>%s</span>%s</div>`, Esc(displayLabel), renderBranchChips(item.Branches))
		if item.Cwd != "" {
			fmt.Fprintf(&b, `<div class="qi-cwd">%s</div>`, Esc(ShortPath(MainWorktree(t, item.Cwd))))
		}
		if result != "" {
			fmt.Fprintf(&b, `<div class="qi-result">%s</div>`, Esc(result))
		}
		fmt.Fprintf(&b, `<div class="qi-actions">`+
			`<form hx-post="/close-session" hx-swap="none" hx-on::after-request="this.closest('.queue-item').remove()" style="display:inline">`+
			`<input type="hidden" name="claude_id" value="%s">`+
			`<button type="submit" class="qi-close">Close</button></form> `+
			`<a href="/?session=%s" class="qi-open">Open</a>`+
			`</div>`, Esc(item.ClaudeID), Esc(item.ClaudeID))
		b.WriteString(`</div>`)
	}
	return b.String()
}

// RenderWorkstreamStatus renders the workstream status panel for the landing page.
func RenderWorkstreamStatus(panel BranchPanel) string {
	if len(panel.Workstreams) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div id="ws-panel">`)
	b.WriteString(`<div class="queue-header">Workstreams</div>`)
	// Action buttons.
	b.WriteString(`<div class="ws-actions">`)
	fmt.Fprintf(&b, `<button class="btn-sm" hx-get="/pull-main?cwd=%s" hx-target="#ws-cmd-output" hx-swap="outerHTML">Pull main</button>`,
		url.QueryEscape(panel.RepoPath))
	b.WriteString(`<button class="btn-sm" hx-post="/mass-sync" hx-target="#ws-cmd-output" hx-swap="beforeend">Sync all</button>`)
	b.WriteString(`<button class="btn-sm" hx-get="/refresh-branches" hx-target="#ws-branch-list" hx-swap="outerHTML">Refresh</button>`)
	hasArchived := false
	for _, ws := range panel.Workstreams {
		if ws.Archived {
			hasArchived = true
			break
		}
	}
	if hasArchived {
		b.WriteString(`<button class="btn-sm" hx-get="/prune" hx-target="#ws-cmd-output" hx-swap="innerHTML">Prune</button>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(RenderBranchList(panel))
	b.WriteString(`<div id="ws-cmd-output" class="ws-cmd-output"></div>`)
	b.WriteString(`</div>`) // close #ws-panel
	return b.String()
}

// RenderBranchList renders the branch list with tree drawing.
func RenderBranchList(panel BranchPanel) string {
	// Split active and archived.
	var active, archived []WorkstreamStatus
	for _, ws := range panel.Workstreams {
		if ws.Archived {
			archived = append(archived, ws)
		} else {
			active = append(active, ws)
		}
	}

	var b strings.Builder
	b.WriteString(`<div id="ws-branch-list">`)
	b.WriteString(`<div class="ws-branch-list">`)
	// Main branch row.
	b.WriteString(`<div class="ws-branch-row ws-color-main">`)
	fmt.Fprintf(&b, `<span class="ws-branch-name">%s</span>`, Esc(panel.DefaultBranch))
	b.WriteString(`<span class="ws-commits ws-sync">=</span>`)
	if panel.MainDirty {
		b.WriteString(`<span class="ws-dirty">*</span>`)
	}
	b.WriteString(`</div>`)
	// Active workstream branches.
	for i, ws := range active {
		isLastWs := i == len(active)-1
		colorClass := "ws-color-a"
		if i%2 == 1 {
			colorClass = "ws-color-b"
		}
		renderBranchTree(&b, ws.Branches, colorClass, isLastWs, ws.Path)
	}
	b.WriteString(`</div>`) // close .ws-branch-list
	// Archived workstreams.
	if len(archived) > 0 {
		b.WriteString(`<div class="ws-archived-header">Archived</div>`)
		b.WriteString(`<div class="ws-branch-list ws-archived-list">`)
		for _, ws := range archived {
			b.WriteString(`<div class="ws-branch-row ws-color-archived">`)
			fmt.Fprintf(&b, `<span class="ws-branch-name">%s</span>`, Esc(ws.Name))
			fmt.Fprintf(&b, `<button class="ws-archive-btn" hx-post="/unarchive-workstream" hx-vals='{"cwd":"%s"}' hx-target="#ws-panel" hx-swap="outerHTML">unarchive</button>`,
				Esc(ws.Path))
			b.WriteString(`</div>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`) // close #ws-branch-list
	return b.String()
}

func renderBranchStatus(b *strings.Builder, br BranchStatus) {
	switch {
	case br.AheadMain > 0 && br.BehindMain > 0:
		fmt.Fprintf(b, `<span class="ws-commits"><span class="ws-ahead">↑%d</span> <span class="ws-behind">↓%d</span></span>`, br.AheadMain, br.BehindMain)
	case br.AheadMain > 0:
		fmt.Fprintf(b, `<span class="ws-commits ws-ahead">↑%d</span>`, br.AheadMain)
	case br.BehindMain > 0:
		fmt.Fprintf(b, `<span class="ws-commits ws-behind">↓%d</span>`, br.BehindMain)
	default:
		b.WriteString(`<span class="ws-commits ws-sync">=</span>`)
	}
	if br.Dirty {
		b.WriteString(`<span class="ws-dirty">*</span>`)
	}
}

// renderBranchTree renders a workstream's branches as a nested tree.
func renderBranchTree(b *strings.Builder, branches []BranchStatus, colorClass string, isLastWs bool, wsPath string) {
	prevDepth := -1
	for i, br := range branches {
		depth := br.Depth

		// Close previous sibling's node and any deeper children containers.
		if i > 0 && depth <= prevDepth {
			for d := prevDepth; d >= depth; d-- {
				b.WriteString(`</div>`) // close ws-child
				if d > depth {
					b.WriteString(`</div>`) // close ws-tree-children
				}
			}
		}

		// Open children container when depth increases.
		if i > 0 && depth > prevDepth {
			b.WriteString(`<div class="ws-tree-children">`)
		}

		// Determine ws-last class.
		lastClass := ""
		if depth == 0 {
			if isLastWs {
				lastClass = " ws-last"
			}
		} else if isLastSibling(branches, i) {
			lastClass = " ws-last"
		}

		// Open tree node.
		fmt.Fprintf(b, `<div class="ws-child%s %s">`, lastClass, colorClass)
		// Branch row.
		b.WriteString(`<div class="ws-branch-row">`)
		fmt.Fprintf(b, `<span class="ws-branch-name">%s</span>`, Esc(br.Name))
		renderBranchStatus(b, br)
		if br.BehindMain > 0 {
			fmt.Fprintf(b, `<button class="ws-rebase-btn" hx-post="/rebase-workstream" hx-vals='{"cwd":"%s"}' hx-target="#ws-cmd-output" hx-swap="beforeend">rebase</button>`,
				Esc(wsPath))
		}
		if depth == 0 {
			fmt.Fprintf(b, `<button class="ws-archive-btn" hx-post="/archive-workstream" hx-vals='{"cwd":"%s"}' hx-target="#ws-panel" hx-swap="outerHTML">archive</button>`,
				Esc(wsPath))
		}
		b.WriteString(`</div>`) // close ws-branch-row

		prevDepth = depth
	}

	// Close remaining open nodes and children containers.
	for d := prevDepth; d >= 0; d-- {
		b.WriteString(`</div>`) // close ws-child
		if d > 0 {
			b.WriteString(`</div>`) // close ws-tree-children
		}
	}
}

// isLastSibling returns true if branches[i] is the last branch at its depth level
// before the depth decreases or the list ends.
func isLastSibling(branches []BranchStatus, i int) bool {
	depth := branches[i].Depth
	for j := i + 1; j < len(branches); j++ {
		if branches[j].Depth <= depth {
			return branches[j].Depth < depth
		}
	}
	return true
}

// RenderPruneConfirmation renders the prune confirmation panel.
func RenderPruneConfirmation(plan PrunePlan) string {
	if len(plan.Workstreams) == 0 {
		return `<div class="ws-cmd-section"><div class="ws-cmd-line ws-cmd-ok">nothing to prune</div></div>`
	}
	var b strings.Builder
	b.WriteString(`<div id="ws-prune-confirm" class="ws-prune-confirm">`)
	b.WriteString(`<div class="ws-prune-title">Prune archived workstreams</div>`)
	b.WriteString(`<form hx-post="/prune-confirm" hx-target="#ws-panel" hx-swap="outerHTML">`)
	for _, ws := range plan.Workstreams {
		fmt.Fprintf(&b, `<input type="hidden" name="path" value="%s">`, Esc(ws.Path))
		fmt.Fprintf(&b, `<div class="ws-prune-ws">`)
		fmt.Fprintf(&b, `<div class="ws-prune-ws-name">%s</div>`, Esc(ws.Name))
		fmt.Fprintf(&b, `<div class="ws-prune-detail">worktree: %s — will be removed</div>`, Esc(ws.Path))
		for _, br := range ws.Branches {
			if br.Safe {
				fmt.Fprintf(&b, `<div class="ws-prune-branch ws-prune-safe">branch %s — delete (%s)</div>`,
					Esc(br.Name), Esc(br.Reason))
			} else {
				fmt.Fprintf(&b, `<div class="ws-prune-branch ws-prune-warn">branch %s — keep (%s)</div>`,
					Esc(br.Name), Esc(br.Reason))
			}
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`<div class="ws-prune-actions">`)
	b.WriteString(`<button type="submit" class="btn-sm ws-prune-btn">Confirm prune</button>`)
	b.WriteString(` <button type="button" class="btn-sm" onclick="this.closest('#ws-prune-confirm').remove()">Cancel</button>`)
	b.WriteString(`</div>`)
	b.WriteString(`</form>`)
	b.WriteString(`</div>`)
	return b.String()
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
		switch {
		case strings.HasPrefix(html[i:], "<span"):
			depth++
			i += 5
		case strings.HasPrefix(html[i:], "</span>"):
			depth--
			if depth == 0 {
				return html[:start] + html[i+7:]
			}
			i += 7
		default:
			i++
		}
	}
	return html
}

// CwdCopyButton returns the OOB-swappable cwd toggle button and cwd row HTML.
// Pass empty cwd to hide the button and clear the row.
func CwdCopyButton(cwd string) string {
	if cwd == "" {
		return `<button class="header-btn" id="cwd-copy" hx-swap-oob="outerHTML" style="display:none">📁</button>` +
			`<div class="cwd-row" id="cwd-row" hx-swap-oob="outerHTML"></div>`
	}
	return fmt.Sprintf(`<button class="header-btn" id="cwd-copy" hx-swap-oob="outerHTML" hx-on:click="this.closest('.header').classList.toggle('show-cwd')">📁</button>`+
		`<div class="cwd-row" id="cwd-row" hx-swap-oob="outerHTML">%s</div>`,
		Esc(ShortPath(cwd)))
}

func OobSwap(id, strategy, content string) string {
	return fmt.Sprintf(`<div id="%s" hx-swap-oob="%s">%s</div>`, id, strategy, content)
}

// FaviconOob returns an OOB swap that sets a colored SVG favicon based on the label.
// The color is derived from a hash of the label text, and the icon shows the first
// letter of the label for quick visual identification in browser tabs.
func FaviconOob(label string) string {
	if label == "" {
		// Reset to default empty favicon
		return `<link id="favicon" rel="icon" href="data:," hx-swap-oob="outerHTML">`
	}
	h := fnv.New32a()
	h.Write([]byte(label))
	hue := h.Sum32() % 360
	// Pick the first rune of the label for the icon letter
	letter := "?"
	for _, r := range label {
		letter = strings.ToUpper(string(r))
		break
	}
	svg := fmt.Sprintf(`<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'>`+
		`<rect width='32' height='32' rx='6' fill='hsl(%d,55%%,45%%)'/>`+
		`<text x='16' y='23' text-anchor='middle' fill='white' font-size='20' font-family='sans-serif' font-weight='600'>%s</text>`+
		`</svg>`, hue, html.EscapeString(letter))
	dataURL := "data:image/svg+xml," + url.PathEscape(svg)
	return fmt.Sprintf(`<link id="favicon" rel="icon" href="%s" hx-swap-oob="outerHTML">`, dataURL)
}

// TitleOob returns an OOB-swapped hidden element that sets document.title via htmx:load event.
func TitleOob(label string) string {
	title := "Monet Droid"
	if label != "" {
		title = label + " · Monet Droid"
	}
	escaped := html.EscapeString(title)
	// Use a hidden span; htmx:load fires when HTMX inserts it into the DOM.
	return fmt.Sprintf(`<span id="page-title" style="display:none" hx-swap-oob="outerHTML" hx-on:htmx:load="document.title=this.textContent">%s</span>`, escaped)
}
