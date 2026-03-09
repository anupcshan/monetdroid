package monetdroid

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
	"time"
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

func RenderMsg(msg ServerMsg) string {
	switch msg.Type {
	case "user_message":
		return fmt.Sprintf(`<div class="msg msg-user"><div class="msg-bubble">%s</div></div>`, Esc(msg.Text))
	case "text":
		return fmt.Sprintf(`<div class="msg msg-assistant"><div class="msg-bubble">%s</div></div>`, RenderMarkdown(msg.Text))
	case "tool_use":
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
	detail := FormatPermDetail(msg.PermTool, msg.PermInput)
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
		Esc(detail), Esc(msg.PermID),
		Esc(msg.SessionID), Esc(msg.PermID),
		Esc(msg.SessionID), Esc(msg.PermID),
		suggBtns.String(),
	)
}

func RenderCostBar(s *Session) string {
	s.Mu.Lock()
	c := s.CostAccum
	s.Mu.Unlock()
	if c.InputTokens == 0 && c.OutputTokens == 0 && c.ContextUsed == 0 {
		return ""
	}
	text := fmt.Sprintf("↓%s ↑%s", FmtK(c.InputTokens), FmtK(c.OutputTokens))
	if c.ContextUsed > 0 && c.ContextWindow > 0 {
		pct := 100 * c.ContextUsed / c.ContextWindow
		text += fmt.Sprintf(" · context %s/%s (%d%%)", FmtK(c.ContextUsed), FmtK(c.ContextWindow), pct)
	} else if c.ContextUsed > 0 {
		text += fmt.Sprintf(" · context %s", FmtK(c.ContextUsed))
	}
	return text
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
