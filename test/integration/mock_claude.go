package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// RunMockClaude runs a mock claude process that replays recorded stream-json events.
// Called when the test binary is re-exec'd with MOCK_CLAUDE=1.
//
// Environment:
//   - MOCK_FIXTURE: path to fixture file (one JSON event per line)
//   - MOCK_DELAY: optional delay between events (e.g. "50ms")
func RunMockClaude() {
	fixture := os.Getenv("MOCK_FIXTURE")
	if fixture == "" {
		fmt.Fprintln(os.Stderr, "MOCK_FIXTURE not set")
		os.Exit(1)
	}

	delay := 10 * time.Millisecond
	if d := os.Getenv("MOCK_DELAY"); d != "" {
		if parsed, err := time.ParseDuration(d); err == nil {
			delay = parsed
		}
	}

	f, err := os.Open(fixture)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open fixture: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	var events []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			events = append(events, line)
		}
	}

	stdinScanner := bufio.NewScanner(os.Stdin)
	stdinScanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	readStdin := func() map[string]any {
		if !stdinScanner.Scan() {
			return nil
		}
		var msg map[string]any
		json.Unmarshal([]byte(stdinScanner.Text()), &msg)
		return msg
	}

	writeStdout := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintln(os.Stdout, string(data))
	}

	// Read initialize request
	initReq := readStdin()
	if initReq == nil {
		return
	}
	reqID, _ := initReq["request_id"].(string)
	writeStdout(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype": "success", "request_id": reqID,
			"response": map[string]any{"pid": os.Getpid()},
		},
	})

	// Read user message
	readStdin()

	// Replay fixture events
	for _, eventJSON := range events {
		var event map[string]any
		if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
			continue
		}

		if event["type"] == "control_request" {
			fmt.Fprintln(os.Stdout, eventJSON)
			readStdin() // wait for control_response
			continue
		}

		time.Sleep(delay)
		fmt.Fprintln(os.Stdout, eventJSON)
	}
}
