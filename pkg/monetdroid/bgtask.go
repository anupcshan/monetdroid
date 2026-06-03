// Background Bash task lifecycle
//
// This file tracks bg Bash tasks through their full lifecycle:
//
//   Creation:
//     PreToolUse hook (run_in_background:true)
//       → PostToolUse hook (stdout empty, backgroundTaskId set)
//       → PostToolBatch hook (tool_response: "Output is being written to: PATH")
//     ParseBgTaskPath extracts the output path. BgTaskState is created in the
//     model via Apply() with Command and OutputPath set; Completed is false.
//
//   Completion:
//     <task-notification> XML is injected into the next UserPromptSubmit
//     prompt text with status="completed" and the tool_use_id. Parsed by
//     bgTaskNotificationPattern in claude.go's "user" handler. Broadcasts
//     task_done, which marks BgTask.Completed = true and RenderEvent
//     removes the tool chip spinner.
//
//   Output streaming:
//     handleBgOutputStream polls the .output file every 500ms. The stop
//     channel created at tool_result time is closed by CloseBgStop (called
//     from task_done or CloseAllBgStops on process death). The elapsed
//     timer goroutine runs until the stop channel closes.
//
//   Model state:
//     BgTaskState is derived from the session log by deriveBgTasks() for
//     cold/warm page loads. BuildModel() calls Apply() for each event.
//     task_done marks Completed = true, which stripSpinner() reads when
//     renderMessages() builds the page HTML. On cold load, all bg tasks
//     are marked completed (processes are dead).

package monetdroid

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
)

// BgTaskState tracks the lifecycle of a background Bash task,
// derived from the session message log by deriveBgTasks.
type BgTaskState struct {
	Command    string // the Bash command
	OutputPath string // the .output file path
	Completed  bool   // task_done has been seen
}

// deriveBgTasks scans the message log and builds a BgTaskState for every
// background Bash task. A task is only tracked once its tool_result confirms
// it via the "Output is being written to:" marker. ToolUseIDs not in the
// result map are not background tasks.
func deriveBgTasks(log []ServerMsg) map[string]*BgTaskState {
	commands := make(map[string]string) // tool_use id → command for all Bash tool_use
	bg := make(map[string]*BgTaskState)
	for _, msg := range log {
		switch msg.Type {
		case "tool_use":
			if msg.Tool == "Bash" && msg.AgentID == "" && msg.Input != nil && msg.Input.Bash != nil {
				commands[msg.ToolUseID] = msg.Input.Bash.Command
			}
		case "tool_result":
			if msg.ToolUseID == "" || msg.AgentID != "" {
				continue
			}
			if bgPath := ParseBgTaskPath(msg.Output); bgPath != "" {
				bg[msg.ToolUseID] = &BgTaskState{
					Command:    commands[msg.ToolUseID],
					OutputPath: bgPath,
				}
			}
		case "task_done":
			if msg.ToolUseID == "" {
				continue
			}
			if st, ok := bg[msg.ToolUseID]; ok {
				st.Completed = true
			}
		}
	}
	return bg
}

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

// RenderBgExtractorDiv returns the SSE-connected div for an extractor-backed
// bg task. The summary zone is replaced on each "summary" event; the raw
// toggle accumulates raw chunks on "raw" events. Both share one SSE connection.
func RenderBgExtractorDiv(sessionID, toolUseID string) string {
	return fmt.Sprintf(
		`<div hx-ext="sse" `+
			`sse-connect="/bg-output/stream?session=%s&tool_id=%s" `+
			`sse-close="done">`+
			`<div sse-swap="summary" hx-swap="innerHTML" class="bg-summary">`+
			`<div class="bg-loading">Running...</div>`+
			`</div>`+
			`<details class="bg-raw-toggle">`+
			`<summary>Show raw output</summary>`+
			`<div sse-swap="raw" hx-swap="beforeend" class="bg-raw"></div>`+
			`</details>`+
			`</div>`,
		url.QueryEscape(sessionID), url.QueryEscape(toolUseID))
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
