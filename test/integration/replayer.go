package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Interaction is a single recorded HTTP request/response pair.
type Interaction struct {
	Request  RecordedRequest  `json:"request"`
	Response RecordedResponse `json:"response"`
}

// RecordedRequest stores the request metadata for a cassette entry.
type RecordedRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body"`
}

// RecordedResponse stores the response for a cassette entry.
type RecordedResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // raw response body (SSE stream for streaming endpoints)
}

// Replayer is an HTTP server that records or replays Anthropic API interactions.
type Replayer struct {
	mu           sync.Mutex
	interactions []Interaction
	nextIdx      int
	mode         string // "record" or "replay"
	upstream     string // upstream URL for record mode (e.g. "https://api.anthropic.com")
	cassettePath string
	t            *testing.T

	// pauseMu acts as a gate for incoming requests. Pause() locks it;
	// Unpause() unlocks it. Requests acquire and immediately release it,
	// so they pass through instantly when unpaused but block when paused.
	pauseMu sync.Mutex
}

// Pause blocks all future API requests until Unpause is called.
func (r *Replayer) Pause() { r.pauseMu.Lock() }

// Unpause releases all blocked API requests and resumes normal flow.
func (r *Replayer) Unpause() { r.pauseMu.Unlock() }

// NewReplayer creates a new replayer. In replay mode, it loads the cassette immediately.
// In record mode, upstream must be set (e.g. "https://api.anthropic.com").
func NewReplayer(t *testing.T, cassettePath, mode, upstream string) *Replayer {
	r := &Replayer{
		mode:         mode,
		upstream:     upstream,
		cassettePath: cassettePath,
		t:            t,
	}
	if mode == "replay" {
		r.loadCassette()
	}
	return r
}

func (r *Replayer) loadCassette() {
	data, err := os.ReadFile(r.cassettePath)
	if err != nil {
		r.t.Fatalf("load cassette %s: %v", r.cassettePath, err)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var interaction Interaction
		if err := json.Unmarshal(line, &interaction); err != nil {
			r.t.Fatalf("parse cassette line: %v", err)
		}
		r.interactions = append(r.interactions, interaction)
	}
	r.t.Logf("replayer: loaded %d interactions from %s", len(r.interactions), r.cassettePath)
}

func (r *Replayer) saveCassette() {
	f, err := os.Create(r.cassettePath)
	if err != nil {
		r.t.Errorf("save cassette: %v", err)
		return
	}
	defer f.Close()
	for _, interaction := range r.interactions {
		data, _ := json.Marshal(interaction)
		f.Write(data)
		f.Write([]byte("\n"))
	}
	r.t.Logf("replayer: saved %d interactions to %s", len(r.interactions), r.cassettePath)
}

func (r *Replayer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.mode == "record" {
		r.handleRecord(w, req)
	} else {
		r.handleReplay(w, req)
	}
}

func (r *Replayer) handleReplay(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	req.Body.Close()

	// Gate: blocks while paused, passes through instantly when unpaused.
	r.pauseMu.Lock()   //nolint:staticcheck // intentional gate pattern
	r.pauseMu.Unlock() //nolint:staticcheck

	r.mu.Lock()
	idx := r.nextIdx
	if idx >= len(r.interactions) {
		r.mu.Unlock()
		r.t.Logf("replayer: no more interactions (idx=%d, total=%d) for %s %s",
			idx, len(r.interactions), req.Method, req.URL.Path)
		http.Error(w, "no more recorded interactions", http.StatusInternalServerError)
		return
	}
	interaction := r.interactions[idx]
	r.nextIdx++
	r.mu.Unlock()

	r.t.Logf("replayer: serving interaction %d for %s %s (%d bytes request body)",
		idx, req.Method, req.URL.Path, len(body))

	responseBody := interaction.Response.Body

	// Plan mode: the CLI generates a random plan file name on each run.
	// The recorded response references the old name (fragmented across SSE
	// input_json_delta events). Detect the discrepancy and substitute.
	if old, nw := detectPlanFileSub(interaction.Request.Body, string(body)); old != "" {
		responseBody = substituteSSEDeltas(responseBody, old, nw)
		r.t.Logf("replayer: substituted plan file %q -> %q", old, nw)
	}

	for k, v := range interaction.Response.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(interaction.Response.Status)

	// For SSE responses, flush to ensure the client receives the stream
	if flusher, ok := w.(http.Flusher); ok {
		w.Write([]byte(responseBody))
		flusher.Flush()
	} else {
		w.Write([]byte(responseBody))
	}
}

func (r *Replayer) handleRecord(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	req.Body.Close()

	r.t.Logf("replayer: recording %s %s (%d bytes request body)", req.Method, req.URL.Path, len(body))

	// Build upstream request
	upstreamURL := r.upstream + req.URL.Path
	if req.URL.RawQuery != "" {
		upstreamURL += "?" + req.URL.RawQuery
	}
	proxyReq, err := http.NewRequestWithContext(req.Context(), req.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		r.t.Logf("replayer: create proxy request: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Forward headers (including Authorization), but strip Accept-Encoding
	// so the upstream sends plain text — makes cassettes readable and avoids
	// storing gzip-compressed response bodies.
	for k, vals := range req.Header {
		if k == "Accept-Encoding" {
			continue
		}
		for _, v := range vals {
			proxyReq.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		r.t.Logf("replayer: upstream error: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Forward all response headers to the client (for correct behavior during recording),
	// but only save allowlisted headers to the cassette.
	for k := range resp.Header {
		w.Header().Set(k, resp.Header.Get(k))
	}
	w.WriteHeader(resp.StatusCode)

	respHeaders := make(map[string]string)
	for k := range resp.Header {
		if recordResponseHeader(k) {
			respHeaders[k] = resp.Header.Get(k)
		}
	}

	// Stream response to client while recording
	flusher, canFlush := w.(http.Flusher)
	var recorded bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			recorded.Write(buf[:n])
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}

	// Save interaction with scrubbed request body
	scrubbedBody, err := scrubRequestBody(body)
	if err != nil {
		r.t.Fatalf("replayer: %v", err)
	}

	r.mu.Lock()
	r.interactions = append(r.interactions, Interaction{
		Request: RecordedRequest{
			Method: req.Method,
			Path:   req.URL.Path,
			Body:   scrubbedBody,
		},
		Response: RecordedResponse{
			Status:  resp.StatusCode,
			Headers: respHeaders,
			Body:    recorded.String(),
		},
	})
	r.mu.Unlock()
}

// Start starts the replayer on a random port and returns its URL.
// The server is automatically stopped when the test ends.
func (r *Replayer) Start() string {
	// Listen on all interfaces so containers using bridge networking can reach
	// the replayer via host.docker.internal. This exposes the replayer on the
	// network during test runs. In record mode the replayer proxies to the real
	// Anthropic API using your credentials — only run on trusted networks.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		r.t.Fatalf("replayer listen: %v", err)
	}
	server := &http.Server{Handler: r}
	go server.Serve(listener)
	r.t.Cleanup(func() {
		server.Close()
		if r.mode == "record" {
			r.saveCassette()
		}
	})

	port := listener.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://host.docker.internal:%d", port)
	r.t.Logf("replayer: listening on :%d (mode=%s)", port, r.mode)
	return url
}

// recordResponseHeader returns true if a response header should be saved in the cassette.
func recordResponseHeader(k string) bool {
	return http.CanonicalHeaderKey(k) == "Content-Type"
}

var (
	// allowedRequestKeys are kept in the cassette as-is.
	allowedRequestKeys = map[string]bool{
		"model":         true,
		"messages":      true,
		"stream":        true,
		"max_tokens":    true,
		"thinking":      true,
		"output_config": true,
		"temperature":   true,
	}

	// deniedRequestKeys are stripped from the cassette silently.
	deniedRequestKeys = map[string]bool{
		"system":   true, // CLI system prompt (large, sensitive)
		"tools":    true, // tool definitions (large, static)
		"metadata": true, // contains user_id and account IDs
	}
)

// planFileRe matches the plan file path in the CLI's EnterPlanMode tool result.
// The CLI generates text like:
//
//	"create your plan at /root/.claude/plans/adjective-noun-animal.md"
//
// FRAGILE: depends on the Claude CLI's tool result template. If a CLI version
// bump changes this wording, update the regex and re-record affected cassettes.
var planFileRe = regexp.MustCompile(`create your plan at ([^\s\\]+\.md)`)

// detectPlanFileSub extracts the plan file path from the recorded and live
// request bodies. Returns (old, new) if they differ, or ("", "") if no
// substitution is needed.
func detectPlanFileSub(recorded, live string) (old, nw string) {
	rm := planFileRe.FindStringSubmatch(recorded)
	lm := planFileRe.FindStringSubmatch(live)
	if len(rm) < 2 || len(lm) < 2 {
		return "", ""
	}
	if rm[1] == lm[1] {
		return "", ""
	}
	return rm[1], lm[1]
}

// substituteSSEDeltas rewrites an SSE response body, replacing occurrences of
// old with new in tool input JSON that arrives fragmented across
// input_json_delta events.
//
// Approach: parse SSE events, accumulate partial_json values per content block,
// do the substitution on the concatenated string, then put the full result in
// the first delta and empty out the rest.
func substituteSSEDeltas(body, old, nw string) string {
	events := strings.Split(body, "\n\n")

	// Track which events are input_json_delta for each content block index.
	type deltaRef struct {
		eventIdx int
		partial  string
	}
	blockDeltas := map[int][]deltaRef{}

	for ei, event := range events {
		dataLine := sseDataLine(event)
		if dataLine == "" {
			continue
		}
		var parsed struct {
			Index int `json:"index"`
			Delta struct {
				Type    string `json:"type"`
				Partial string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(dataLine), &parsed) != nil {
			continue
		}
		if parsed.Delta.Type == "input_json_delta" {
			blockDeltas[parsed.Index] = append(blockDeltas[parsed.Index],
				deltaRef{eventIdx: ei, partial: parsed.Delta.Partial})
		}
	}

	for _, deltas := range blockDeltas {
		var buf strings.Builder
		for _, d := range deltas {
			buf.WriteString(d.partial)
		}
		original := buf.String()
		substituted := strings.ReplaceAll(original, old, nw)
		if substituted == original {
			continue
		}
		// Put the full substituted text in the first delta, empty the rest.
		for i, d := range deltas {
			p := ""
			if i == 0 {
				p = substituted
			}
			events[d.eventIdx] = sseReplacePartial(events[d.eventIdx], p)
		}
	}

	return strings.Join(events, "\n\n")
}

// sseDataLine extracts the JSON string after "data: " from an SSE event.
func sseDataLine(event string) string {
	for _, line := range strings.Split(event, "\n") {
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	return ""
}

// sseReplacePartial rebuilds an SSE event with a new partial_json value.
func sseReplacePartial(event, newPartial string) string {
	dataLine := sseDataLine(event)
	if dataLine == "" {
		return event
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(dataLine), &parsed) != nil {
		return event
	}
	delta, ok := parsed["delta"].(map[string]any)
	if !ok {
		return event
	}
	delta["partial_json"] = newPartial
	newData, err := json.Marshal(parsed)
	if err != nil {
		return event
	}
	// Rebuild: "event: ...\ndata: <new json>"
	var lines []string
	for _, line := range strings.Split(event, "\n") {
		if strings.HasPrefix(line, "data: ") {
			lines = append(lines, "data: "+string(newData))
		} else {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// scrubRequestBody removes sensitive/bulky fields from the API request body
// before saving to the cassette. The full body is still sent to the upstream
// during recording. Returns the scrubbed JSON and an error if unknown keys
// are encountered.
func scrubRequestBody(body []byte) (string, error) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return string(body), nil
	}

	var unknown []string
	for k := range parsed {
		if !allowedRequestKeys[k] && !deniedRequestKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return "", fmt.Errorf("unknown request body keys: %v — add them to allowedRequestKeys or deniedRequestKeys in replayer.go", unknown)
	}

	for k := range deniedRequestKeys {
		delete(parsed, k)
	}
	scrubbed, err := json.Marshal(parsed)
	if err != nil {
		return string(body), nil
	}
	return string(scrubbed), nil
}
