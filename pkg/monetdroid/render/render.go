// Package render owns OOB/SSE formatting primitives for the monetdroid
// frontend. It imports only the standard library, providing a compiler-enforced
// single rendering path: no monetdroid package can construct OOB swaps or SSE
// events without going through this package.
package render

import (
	"fmt"
	"html"
	"strings"
)

// Cmd is an abstract DOM mutation independent of the transport (SSE,
// WebSocket, etc.). The SSE sink converts these to OOB-swap HTML; a future
// WebSocket sink would convert them to JSON.
type Cmd struct {
	Target   string
	Strategy string // "innerHTML", "beforeend", "outerHTML"
	Content  string
}

// OOB returns an HTMX OOB-swap div for the given target element, strategy,
// and content. The content is raw HTML inserted verbatim.
func OOB(id, strategy, content string) string {
	return fmt.Sprintf(`<div id="%s" hx-swap-oob="%s">%s</div>`, id, strategy, content)
}

// SSE formats an SSE event with the given event type and data string. The
// data is split on newlines and each line is prefixed with "data: ". A
// trailing blank line terminates the event.
func SSE(event, data string) string {
	data = strings.ReplaceAll(data, "\r", "")
	var buf strings.Builder
	buf.WriteString("event: ")
	buf.WriteString(event)
	buf.WriteString("\n")
	for line := range strings.SplitSeq(data, "\n") {
		buf.WriteString("data: ")
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
	return buf.String()
}

// Format converts a slice of Cmd values and optional extra raw OOB HTML
// strings into a single SSE "htmx" event payload. Returns an empty string
// when there are no commands and no extra OOBs.
func Format(cmds []Cmd, extraOOBs ...string) string {
	var oobs []string
	for _, c := range cmds {
		if c.Content == "" && c.Strategy == "outerHTML" {
			oobs = append(oobs, fmt.Sprintf(`<div id="%s" hx-swap-oob="%s"></div>`, c.Target, c.Strategy))
		} else {
			oobs = append(oobs, fmt.Sprintf(`<div id="%s" hx-swap-oob="%s">%s</div>`, c.Target, c.Strategy, c.Content))
		}
	}
	oobs = append(oobs, extraOOBs...)
	if len(oobs) == 0 {
		return ""
	}
	return SSE("htmx", strings.Join(oobs, "\n"))
}

// QueueBar returns an OOB swap for the queue bar. When text is empty the bar
// is cleared; otherwise it shows the queued message with Edit and Cancel
// buttons.
func QueueBar(sessionID, text string) string {
	if text == "" {
		return OOB("queue-bar", "innerHTML", "")
	}
	return OOB("queue-bar", "innerHTML", fmt.Sprintf(
		`<div class="queue-content">`+
			`<span class="queue-label">queued:</span>`+
			`<span class="queue-preview">%s</span>`+
			`<button class="queue-btn" hx-post="/cancel-queue" hx-vals='{"session_id":"%s","edit":"true"}' hx-target="#queue-bar" hx-swap="innerHTML">Edit</button>`+
			`<button class="queue-btn queue-cancel" hx-post="/cancel-queue" hx-vals='{"session_id":"%s"}' hx-target="#queue-bar" hx-swap="innerHTML">✕</button>`+
			`</div>`,
		html.EscapeString(text), html.EscapeString(sessionID), html.EscapeString(sessionID),
	))
}

// ReviewBarOOB returns an OOB swap for the review bar. When barHTML is empty,
// the bar is cleared; otherwise it replaces the bar contents.
func ReviewBarOOB(barHTML string) string {
	if barHTML == "" {
		return OOB("review-bar", "outerHTML", `<div class="review-bar" id="review-bar"></div>`)
	}
	return OOB("review-bar", "outerHTML", barHTML)
}
