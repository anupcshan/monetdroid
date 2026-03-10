package monetdroid

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/pmezard/go-difflib/difflib"
)

var (
	reCodeBlock  = regexp.MustCompile("(?s)```\\w*\n(.*?)```")
	reCodeBlock2 = regexp.MustCompile("(?s)```(.*?)```")
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	reBold       = regexp.MustCompile(`\*\*(.+?)\*\*`)
)

func Esc(s string) string { return html.EscapeString(s) }

func RenderMarkdown(text string) string {
	s := Esc(text)
	s = reCodeBlock.ReplaceAllString(s, "<pre>$1</pre>")
	s = reCodeBlock2.ReplaceAllString(s, "<pre>$1</pre>")
	s = reInlineCode.ReplaceAllString(s, "<code>$1</code>")
	s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = strings.ReplaceAll(s, "\n", "<br>")
	return s
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

func RenderEditDiffHTML(filePath, oldStr, newStr string) string {
	ud := difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldStr),
		B:        difflib.SplitLines(newStr),
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
		A:       difflib.SplitLines(oldStr),
		B:       difflib.SplitLines(newStr),
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
		content.WriteString(Esc(msg.Text))
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
	s.Mu.Unlock()
	if c.TotalCostUSD == 0 && c.ContextUsed == 0 {
		return ""
	}
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

func OobSwap(id, strategy, content string) string {
	return fmt.Sprintf(`<div id="%s" hx-swap-oob="%s">%s</div>`, id, strategy, content)
}
