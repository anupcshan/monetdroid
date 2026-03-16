package monetdroid

import (
	"fmt"
	"io"
	"log"
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
			if err := readChunk(path, &offset, onChunk); err != nil {
				log.Printf("[bgtask] final read %s: %v", path, err)
			}
			return
		default:
		}

		onTick(time.Since(started))
		if err := readChunk(path, &offset, onChunk); err != nil {
			log.Printf("[bgtask] read %s: %v", path, err)
		}
		time.Sleep(pollInterval)
	}
}

func readChunk(path string, offset *int64, onChunk func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= *offset {
		return nil
	}

	if _, err := f.Seek(*offset, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, info.Size()-*offset)
	n, err := f.Read(buf)
	if n > 0 {
		*offset += int64(n)
		onChunk(string(buf[:n]))
	}
	return err
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
