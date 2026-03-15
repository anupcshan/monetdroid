package monetdroid

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
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

// TailBgTask tails a background task output file and calls onChunk with
// new content as it appears. Calls onTick with the elapsed duration on
// every poll iteration. Stops when stop is closed (via task_notification).
func TailBgTask(path string, stop <-chan struct{}, onChunk func(string), onTick func(time.Duration)) {
	var offset int64
	started := time.Now()
	const pollInterval = 500 * time.Millisecond

	for {
		select {
		case <-stop:
			readChunk(path, &offset, onChunk)
			return
		default:
		}

		onTick(time.Since(started))
		readChunk(path, &offset, onChunk)
		time.Sleep(pollInterval)
	}
}

func readChunk(path string, offset *int64, onChunk func(string)) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() <= *offset {
		return false
	}

	f.Seek(*offset, io.SeekStart)
	buf := make([]byte, info.Size()-*offset)
	n, err := f.Read(buf)
	if n <= 0 {
		return false
	}
	*offset += int64(n)
	onChunk(string(buf[:n]))
	return true
}

// RenderBgOutput formats a chunk of background task output as an OOB swap
// that appends to the tool chip's output area.
func RenderBgOutput(toolUseID, chunk string) string {
	// Escape and format
	escaped := Esc(strings.TrimRight(chunk, "\n"))
	if escaped == "" {
		return ""
	}
	divID := "bg-" + toolUseID
	return OobSwap(divID, "beforeend", fmt.Sprintf("<span>%s\n</span>", escaped))
}
