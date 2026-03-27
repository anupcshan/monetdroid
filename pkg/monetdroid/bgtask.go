package monetdroid

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
)

var bgTaskPattern = regexp.MustCompile(`Output is being written to: (.+\.output)`)

// ParseBgTaskPath extracts the output file path from a background Bash
// tool_result message. Returns empty string if not a background task.
func ParseBgTaskPath(output string) string {
	m := bgTaskPattern.FindStringSubmatch(output)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// ReadBgChunk reads new content from a bg task output file starting at offset.
// Returns the content, new offset, and any error.
func ReadBgChunk(path string, offset int64) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", offset, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", offset, err
	}
	if info.Size() <= offset {
		return "", offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", offset, err
	}
	buf := make([]byte, info.Size()-offset)
	n, err := f.Read(buf)
	if n > 0 {
		return string(buf[:n]), offset + int64(n), err
	}
	return "", offset, err
}

// RenderBgSlot returns a lazy-load trigger for a background task output.
// The actual SSE connection is only made when the element becomes visible
// (i.e., when the user opens the tool chip's <details>).
func RenderBgSlot(sessionID, toolUseID string) string {
	return fmt.Sprintf(
		`<div class="tool-bg-output" id="bg-%s" `+
			`hx-get="/bg-output/connect?session=%s&tool_id=%s" `+
			`hx-trigger="revealed once" hx-swap="innerHTML"></div>`,
		Esc(toolUseID), url.QueryEscape(sessionID), url.QueryEscape(toolUseID))
}

// RenderBgSSEDiv returns the SSE-connected div that streams bg task output.
// Returned by /bg-output/connect when the lazy-load trigger fires.
func RenderBgSSEDiv(sessionID, toolUseID string) string {
	return fmt.Sprintf(
		`<div hx-ext="sse" `+
			`sse-connect="/bg-output/stream?session=%s&tool_id=%s" `+
			`sse-swap="chunk" hx-swap="beforeend" sse-close="done"></div>`,
		url.QueryEscape(sessionID), url.QueryEscape(toolUseID))
}
