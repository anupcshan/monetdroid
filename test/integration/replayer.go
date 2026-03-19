package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
}

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

	for k, v := range interaction.Response.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(interaction.Response.Status)

	// For SSE responses, flush to ensure the client receives the stream
	if flusher, ok := w.(http.Flusher); ok {
		w.Write([]byte(interaction.Response.Body))
		flusher.Flush()
	} else {
		w.Write([]byte(interaction.Response.Body))
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
	}

	// deniedRequestKeys are stripped from the cassette silently.
	deniedRequestKeys = map[string]bool{
		"system":   true, // CLI system prompt (large, sensitive)
		"tools":    true, // tool definitions (large, static)
		"metadata": true, // contains user_id and account IDs
	}
)

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
