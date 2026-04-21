package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
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
	// BodyHash is a normalized hash of the full (pre-scrub) request body.
	// At replay time, the incoming body is hashed the same way and compared.
	// Mismatch means the model's inputs have drifted since recording.
	BodyHash string `json:"body_hash,omitempty"`
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
	// hashIndex maps a recorded body hash to the list of interaction
	// indices sharing that hash. Replay picks the first unconsumed index,
	// supporting cassettes whose requests arrive in a different order than
	// they were recorded (e.g. parallel subagent launches).
	hashIndex map[string][]int
	consumed  []bool
	// idMap holds recorded→live substitutions for per-session randoms (bash
	// background task IDs, session UUIDs). The CLI generates these locally
	// and embeds them in tool_result text; the model's canned response then
	// references them in subsequent tool_use inputs. Without substitution,
	// the live CLI would execute tool_use inputs against recorded paths
	// that don't exist in the live session.
	//
	// Populated after each successful hash match by comparing the live and
	// recorded request bodies; consulted when writing the recorded response
	// to the live client.
	idMap        map[string]string
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
	r.hashIndex = make(map[string][]int, len(r.interactions))
	r.consumed = make([]bool, len(r.interactions))
	r.idMap = make(map[string]string)
	for i, ix := range r.interactions {
		r.hashIndex[ix.Request.BodyHash] = append(r.hashIndex[ix.Request.BodyHash], i)
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

	liveHash := hashRequestBody(body)

	r.mu.Lock()
	idx := -1
	for _, candidate := range r.hashIndex[liveHash] {
		if !r.consumed[candidate] {
			idx = candidate
			break
		}
	}
	if idx < 0 {
		r.mu.Unlock()
		dir := r.dumpLiveBody(body)
		r.t.Errorf("replayer: no recorded interaction matches live request body hash %s for %s %s.\n  dump dir: %s\n  diff e.g.: diff %s/live.norm.json %s/recorded-1.norm.json\nRe-record with -record.",
			liveHash, req.Method, req.URL.Path, dir, dir, dir)
		http.Error(w, "no matching recorded interaction", http.StatusInternalServerError)
		return
	}
	r.consumed[idx] = true
	interaction := r.interactions[idx]
	r.learnIDMappings([]byte(interaction.Request.Body), body)
	idMapCopy := make(map[string]string, len(r.idMap))
	for k, v := range r.idMap {
		idMapCopy[k] = v
	}
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

	// Cross-turn ID substitution: swap recorded bash task IDs / session
	// UUIDs in the response body with the live CLI's values so subsequent
	// tool_use inputs reference paths that actually exist in the live
	// filesystem. tool_use inputs are SSE-fragmented across
	// input_json_delta events — substituteSSEDeltas reassembles, replaces,
	// and re-emits. A follow-up flat ReplaceAll catches any occurrences
	// outside the fragmented inputs (e.g. in text deltas).
	for rec, live := range idMapCopy {
		if rec == live {
			continue
		}
		responseBody = substituteSSEDeltas(responseBody, rec, live)
		responseBody = strings.ReplaceAll(responseBody, rec, live)
		r.t.Logf("replayer: substituted id %q -> %q", rec, live)
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

	// Build upstream request. Use a detached context so the drain continues
	// even if the test's request context is cancelled mid-stream by test
	// cleanup — otherwise the recorded SSE stream ends mid-event (no
	// terminating message_stop), which makes replay-mode CLIs think the turn
	// never ended and fire spurious follow-up POSTs that have no cassette
	// match. The 3-minute cap bounds how long a hung upstream can block
	// cleanup.
	upstreamCtx, cancelUpstream := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelUpstream()
	upstreamURL := r.upstream + req.URL.Path
	if req.URL.RawQuery != "" {
		upstreamURL += "?" + req.URL.RawQuery
	}
	proxyReq, err := http.NewRequestWithContext(upstreamCtx, req.Method, upstreamURL, bytes.NewReader(body))
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

	// Save interaction with scrubbed request body plus a hash of the full
	// (pre-scrub) body so replay can detect input drift.
	scrubbedBody, err := scrubRequestBody(body)
	if err != nil {
		r.t.Fatalf("replayer: %v", err)
	}
	bodyHash := hashRequestBody(body)

	r.mu.Lock()
	r.interactions = append(r.interactions, Interaction{
		Request: RecordedRequest{
			Method:   req.Method,
			Path:     req.URL.Path,
			Body:     scrubbedBody,
			BodyHash: bodyHash,
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
		if r.mode == "record" {
			// Graceful shutdown: wait for any in-flight handlers to finish
			// draining their upstream SSE streams before saving the cassette,
			// so the saved response bodies include the terminating
			// message_stop events. 3-minute cap matches the upstream request
			// timeout.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
			r.saveCassette()
		} else {
			server.Close()
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
		"model":              true,
		"messages":           true,
		"stream":             true,
		"max_tokens":         true,
		"thinking":           true,
		"output_config":      true,
		"temperature":        true,
		"context_management": true,
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

// hashRequestBody produces a stable hex-encoded sha256 of the request body
// after normalizing fields that vary between runs without reflecting input
// changes (billing CLI build hash, date injection, tool_use IDs, metadata,
// plan file paths). Used at record time to pin a cassette's model inputs
// and at replay time to detect drift.
func hashRequestBody(body []byte) string {
	normalized, err := normalizeRequestBody(body)
	if err != nil {
		// Non-JSON body: hash the raw bytes so comparison still works.
		sum := sha256.Sum256(body)
		return "raw:" + hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:])
}

// learnIDMappings extracts per-session random IDs (bash task IDs, session
// UUIDs) that appear in both the recorded and live request bodies and pairs
// them up by order of first appearance. Resulting map (recorded→live) is
// used at response-serve time to rewrite recorded identifiers to their live
// counterparts. Safe to call repeatedly as turns accumulate — new IDs are
// appended; existing mappings stay consistent because IDs are stable within
// a single session on each side.
//
// Pairing is by first-appearance index. This is sound because the live and
// recorded bodies matched by hash, i.e. they normalize to the same
// structure, so the positions of the sentinel-ID patterns correspond.
//
// Caller must hold r.mu.
func (r *Replayer) learnIDMappings(recorded, live []byte) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`tasks/([a-z0-9]{9})\.output`),
		regexp.MustCompile(`<task-id>([a-z0-9]{9})</task-id>`),
		regexp.MustCompile(`Command running in background with ID: ([a-z0-9]{9})`),
		regexp.MustCompile(`/tmp/claude-\d+/-work/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`),
	}
	extract := func(body []byte, re *regexp.Regexp) []string {
		seen := map[string]bool{}
		var out []string
		for _, m := range re.FindAllSubmatch(body, -1) {
			id := string(m[1])
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		return out
	}
	for _, re := range patterns {
		recIDs := extract(recorded, re)
		liveIDs := extract(live, re)
		n := len(recIDs)
		if len(liveIDs) < n {
			n = len(liveIDs)
		}
		for i := 0; i < n; i++ {
			if recIDs[i] != liveIDs[i] {
				r.idMap[recIDs[i]] = liveIDs[i]
			}
		}
	}
}

// dumpLiveBody writes the raw and normalized live request bodies to a tmp
// dir so a no-match replay failure leaves readable artifacts behind. Also
// dumps every recorded body (raw + normalized) from the cassette so the
// user can diff the normalized live body against each recorded normalized
// body to see exactly what's drifting. TEMP: remove once drift is resolved.
func (r *Replayer) dumpLiveBody(live []byte) string {
	dir, err := os.MkdirTemp("", "replayer-nomatch-*")
	if err != nil {
		return "<mkdir failed: " + err.Error() + ">"
	}
	write := func(name string, data []byte) {
		_ = os.WriteFile(dir+"/"+name, data, 0o644)
	}
	write("live.raw.json", live)
	if norm, err := normalizeRequestBody(live); err == nil {
		write("live.norm.json", norm)
	}
	for i, ix := range r.interactions {
		if ix.Request.Method != "POST" {
			continue
		}
		raw := []byte(ix.Request.Body)
		write(fmt.Sprintf("recorded-%d.raw.json", i), raw)
		if norm, err := normalizeRequestBody(raw); err == nil {
			write(fmt.Sprintf("recorded-%d.norm.json", i), norm)
		}
	}
	return dir
}

var (
	billingCchRe  = regexp.MustCompile(`cch=[A-Za-z0-9]+;`)
	currentDateRe = regexp.MustCompile(`Today's date is \d{4}-\d{2}-\d{2}\.`)
	toolUseIDRe   = regexp.MustCompile(`toolu_[A-Za-z0-9]+`)
	// Per-run IDs leaked into tool_result text by Claude Code's
	// Bash-background spawns. Observed shapes:
	//   "Command running in background with ID: bmyvedpdd"
	//   "tasks/bmyvedpdd.output"
	//   "<task-id>bmyvedpdd</task-id>"
	//   "/tmp/claude-0/-work/cba6ec33-bc88-4d03-b249-9e293b87a920/..."
	bgTaskIDPrefixRe = regexp.MustCompile(`(Command running in background with ID: |tasks/|<task-id>)([a-z0-9]{9})`)
	bgTaskIDSuffixRe = regexp.MustCompile(`([a-z0-9]{9})(\.output|</task-id>)`)
	sessionUUIDRe    = regexp.MustCompile(`/tmp/claude-\d+/-work/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	// Plan file paths: the CLI names new plan files with a random
	// adjective-noun-animal style identifier. The existing planFileRe only
	// matches the phrasing in the ExitPlanMode tool template ("create your
	// plan at ..."). The same path also appears as `"file_path":` inside a
	// Read/Write tool_use and as text in "File created successfully at:"
	// tool_results. This regex catches those.
	planFilePathRe = regexp.MustCompile(`/root/\.claude/plans/[a-z-]+\.md`)
	// Short git SHA printed at the start of a "Recent commits:\n<sha> ..."
	// line inside the CLI's <env> block in subagent system prompts. Every
	// fresh test repo gets a different commit timestamp → different SHA.
	recentCommitsSHARe = regexp.MustCompile(`(Recent commits:\n)([0-9a-f]{7,40})`)
	// Agent tool result identifier: a random 17-char id (prefix `a` + 16
	// hex chars) emitted in text like `agentId: a1234... (use SendMessage
	// with to: 'a1234...' to continue...)`. Context-scoped so we don't match
	// arbitrary hex strings elsewhere.
	agentIDRe = regexp.MustCompile(`(agentId: |to: ')(a[0-9a-f]{16})`)
	// Subagent wall-clock duration inside the Agent tool result's <usage>
	// block.
	agentDurationRe = regexp.MustCompile(`duration_ms: \d+`)
	// The CLI injects the user's email address from real credentials into
	// system-reminder context at record time. Replay-side dummy credentials
	// don't carry an email, so this whole block is absent there.
	userEmailBlockRe = regexp.MustCompile(`# userEmail\nThe user's email address is [^\n]+\.\n`)
)

func normalizeNoisyText(s string) string {
	s = billingCchRe.ReplaceAllString(s, "cch=<HASH>;")
	s = currentDateRe.ReplaceAllString(s, "Today's date is <DATE>.")
	s = toolUseIDRe.ReplaceAllString(s, "toolu_<ID>")
	s = planFileRe.ReplaceAllString(s, "create your plan at <PLAN>")
	s = bgTaskIDPrefixRe.ReplaceAllString(s, "${1}<BGID>")
	s = bgTaskIDSuffixRe.ReplaceAllString(s, "<BGID>${2}")
	s = sessionUUIDRe.ReplaceAllString(s, "/tmp/claude-<UID>/-work/<SESSION>")
	s = planFilePathRe.ReplaceAllString(s, "/root/.claude/plans/<PLANFILE>.md")
	s = recentCommitsSHARe.ReplaceAllString(s, "${1}<SHA>")
	s = agentIDRe.ReplaceAllString(s, "${1}<AGENT_ID>")
	s = agentDurationRe.ReplaceAllString(s, "duration_ms: <DUR>")
	s = userEmailBlockRe.ReplaceAllString(s, "")
	return s
}

// walkStrings visits every string value in a decoded JSON tree and replaces
// it via fn. Mutates the input.
func walkStrings(v any, fn func(string) string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = walkStrings(val, fn)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = walkStrings(val, fn)
		}
		return t
	case string:
		return fn(t)
	default:
		return v
	}
}

// normalizeRequestBody produces a canonical JSON representation of the body
// with per-run noise replaced by sentinels. Returns an error only if the
// body isn't JSON.
func normalizeRequestBody(body []byte) ([]byte, error) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	delete(parsed, "metadata")
	stripMCPTools(parsed)
	stripCacheControl(parsed)
	stripSystemRemindersInToolResults(parsed)
	sortToolResults(parsed)
	sortFileListToolResults(parsed)
	walkStrings(parsed, normalizeNoisyText)
	// json.Marshal sorts map keys alphabetically, giving canonical output.
	return json.Marshal(parsed)
}

// stripCacheControl removes all "cache_control" keys from the parsed tree.
// The CLI attaches cache_control as a prompt-caching boundary hint, but its
// placement is non-deterministic across runs (the same content often lands
// on different blocks between record and replay). Not semantically
// meaningful for test drift detection.
func stripCacheControl(v any) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "cache_control")
		for _, val := range t {
			stripCacheControl(val)
		}
	case []any:
		for _, val := range t {
			stripCacheControl(val)
		}
	}
}

// systemReminderRe matches a `<system-reminder>...</system-reminder>` block
// (plus surrounding whitespace). The CLI appends these harness metadata
// blocks to the "freshest" tool_result in a parallel tool batch — which
// block gets it is non-deterministic across runs.
// Match only newlines around the reminder, never tabs or spaces — tool_result
// content (e.g. Read line-numbered output) can legitimately end with a tab
// that belongs to the phantom last line; greedy \s* would eat it.
var systemReminderRe = regexp.MustCompile(`(?s)\n*<system-reminder>.*?</system-reminder>\n*`)

// stripSystemRemindersInToolResults removes appended <system-reminder>
// blocks from tool_result content strings. The reminders are harness hints,
// not test signal.
func stripSystemRemindersInToolResults(parsed map[string]any) {
	messages, ok := parsed["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, c := range content {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := cm["type"].(string); t != "tool_result" {
				continue
			}
			s, ok := cm["content"].(string)
			if !ok {
				continue
			}
			s = systemReminderRe.ReplaceAllString(s, "")
			// When the CLI appends a <system-reminder> to a tool_result it
			// strips the Read tool's phantom-line trailing tab before
			// concatenation; when it doesn't, the tab stays. Across parallel
			// tool calls the reminder lands on a different block each run, so
			// the same block's content ends `...N\t` one run and `...N` the
			// next. TrimRight equalizes both to `...N`.
			s = strings.TrimRight(s, "\t\n")
			cm["content"] = s
		}
	}
}

// sortFileListToolResults sorts newline-separated path-like lines that appear
// as the leading part of a tool_result content string. Node's glob library
// (used by the Claude CLI's Glob tool) documents arbitrary ordering, so the
// same file set can come back in different orders between record and replay
// and drift the hash. We only sort the leading run of path-like lines up to
// the first blank line — trailing content (e.g. appended system-reminder
// blocks) is left as-is.
// Matches a line that starts with a path token, optionally followed by a
// grep-style `:line:content` suffix. Covers both Glob results (just paths)
// and Grep results (path:line:content).
var pathLineRe = regexp.MustCompile(`^[a-zA-Z0-9_./\-]+(?::\d+:.*)?$`)

func sortFileListToolResults(parsed map[string]any) {
	messages, ok := parsed["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, c := range content {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := cm["type"].(string); t != "tool_result" {
				continue
			}
			s, ok := cm["content"].(string)
			if !ok {
				continue
			}
			if sorted := sortPathListPrefix(s); sorted != s {
				cm["content"] = sorted
			}
		}
	}
}

// sortPathListPrefix detects a leading block of path-like lines (ending at
// the first blank line or end-of-string) and sorts it. Returns the input
// unchanged if the leading block doesn't look like a path list (at least
// 3 lines, all matching pathLineRe).
func sortPathListPrefix(s string) string {
	idx := strings.Index(s, "\n\n")
	head := s
	tail := ""
	if idx >= 0 {
		head = s[:idx]
		tail = s[idx:]
	}
	lines := strings.Split(head, "\n")
	if len(lines) < 3 {
		return s
	}
	for _, l := range lines {
		if l == "" || !pathLineRe.MatchString(l) {
			return s
		}
	}
	sorted := make([]string, len(lines))
	copy(sorted, lines)
	sort.Strings(sorted)
	return strings.Join(sorted, "\n") + tail
}

// sortToolResults canonicalizes the order of tool_result content blocks
// within each user message. When the model issues parallel tool calls, the
// CLI's follow-up message carries the results in whichever order the local
// reads completed — non-deterministic across runs. tool_use_id is generated
// by the API and carried through the cassette's response unchanged, so
// sorting by it produces the same order at record and replay time.
//
// Only consecutive runs of tool_result blocks are sorted; text/tool_use
// blocks keep their original positions.
func sortToolResults(parsed map[string]any) {
	messages, ok := parsed["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		i := 0
		for i < len(content) {
			if !isToolResult(content[i]) {
				i++
				continue
			}
			j := i
			for j < len(content) && isToolResult(content[j]) {
				j++
			}
			sort.SliceStable(content[i:j], func(a, b int) bool {
				return toolResultID(content[i+a]) < toolResultID(content[i+b])
			})
			i = j
		}
	}
}

func isToolResult(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	t, _ := m["type"].(string)
	return t == "tool_result"
}

func toolResultID(v any) string {
	m, _ := v.(map[string]any)
	id, _ := m["tool_use_id"].(string)
	return id
}

// stripMCPTools removes all mcp__* entries from the tools array before
// hashing. Rationale: in record mode the CLI enumerates the user's
// claude.ai MCP servers via an out-of-band call (not routed through
// ANTHROPIC_BASE_URL), so the recorded body includes tools like
// mcp__claude_ai_Gmail__authenticate. In replay mode the dummy credential
// can't auth against claude.ai, so those tools are absent — producing a
// 4KB-ish drift that's unrelated to anything the tests exercise. Stripping
// them on both sides before hashing lets the hash focus on meaningful
// input drift.
//
// Restore if integration tests ever need to validate MCP tool behavior
// (would also require intercepting the claude.ai enumeration call so replay
// mode produces the same tools as record).
func stripMCPTools(parsed map[string]any) {
	tools, ok := parsed["tools"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(tools))
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			kept = append(kept, t)
			continue
		}
		if name, _ := m["name"].(string); strings.HasPrefix(name, "mcp__") {
			continue
		}
		kept = append(kept, t)
	}
	parsed["tools"] = kept
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
