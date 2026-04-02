package monetdroid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ReviewComment represents a single inline comment on a diff line.
type ReviewComment struct {
	ID   string
	File string
	Line int
	Side string // "old" or "new"
	Text string
}

// ReviewStore holds review comments for sessions.
type ReviewStore struct {
	mu       sync.Mutex
	comments map[string][]ReviewComment // sessionID -> comments
}

func NewReviewStore() *ReviewStore {
	return &ReviewStore{comments: make(map[string][]ReviewComment)}
}

func (rs *ReviewStore) Add(sessionID string, c ReviewComment) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.comments[sessionID] = append(rs.comments[sessionID], c)
}

func (rs *ReviewStore) Remove(sessionID, commentID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	comments := rs.comments[sessionID]
	for i, c := range comments {
		if c.ID == commentID {
			rs.comments[sessionID] = append(comments[:i], comments[i+1:]...)
			return
		}
	}
}

func (rs *ReviewStore) List(sessionID string) []ReviewComment {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]ReviewComment(nil), rs.comments[sessionID]...)
}

func (rs *ReviewStore) Clear(sessionID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.comments, sessionID)
}

func (rs *ReviewStore) Count(sessionID string) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.comments[sessionID])
}

// FormatReviewMessage composes all comments for a session into a markdown message.
func (rs *ReviewStore) FormatReviewMessage(sessionID string) string {
	comments := rs.List(sessionID)
	if len(comments) == 0 {
		return ""
	}

	// Group by file
	byFile := make(map[string][]ReviewComment)
	var fileOrder []string
	for _, c := range comments {
		if _, seen := byFile[c.File]; !seen {
			fileOrder = append(fileOrder, c.File)
		}
		byFile[c.File] = append(byFile[c.File], c)
	}

	// Sort comments within each file by line number
	for _, cs := range byFile {
		sort.Slice(cs, func(i, j int) bool { return cs[i].Line < cs[j].Line })
	}

	var b strings.Builder
	b.WriteString("## Code Review Comments\n\n")
	for _, file := range fileOrder {
		fmt.Fprintf(&b, "### %s\n\n", file)
		for _, c := range byFile[file] {
			fmt.Fprintf(&b, "**Line %d:** %s\n\n", c.Line, c.Text)
		}
	}
	return b.String()
}

func randomID() string {
	var buf [6]byte
	rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// RenderReviewBar renders the review action bar for a session.
func RenderReviewBar(sessionID string, count int) string {
	if count == 0 {
		return ""
	}
	noun := "comment"
	if count > 1 {
		noun = "comments"
	}
	return fmt.Sprintf(`<div class="review-bar" id="review-bar">`+
		`<span class="review-count">%d %s</span>`+
		`<button class="review-send-btn" hx-post="/review/send" `+
		`hx-vals='{"session_id":"%s"}' hx-swap="none">Send Review</button>`+
		`</div>`, count, noun, Esc(sessionID))
}

// --- HTTP handlers ---

func (h *Hub) handleReviewCommentForm(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	file := r.URL.Query().Get("file")
	line := r.URL.Query().Get("line")
	side := r.URL.Query().Get("side")
	if side == "" {
		side = "new"
	}

	// Returns a <tr> that gets inserted after the clicked diff line via afterend swap.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<tr class="diff-comment-slot"><td colspan="3">`+
		`<form class="review-form" hx-post="/review/comment" hx-target="closest tr" hx-swap="outerHTML">`+
		`<input type="hidden" name="session_id" value="%s">`+
		`<input type="hidden" name="file" value="%s">`+
		`<input type="hidden" name="line" value="%s">`+
		`<input type="hidden" name="side" value="%s">`+
		`<textarea name="text" class="review-textarea" placeholder="Add review comment..." rows="3"></textarea>`+
		`<div class="review-form-actions">`+
		`<button type="submit" class="review-submit">Comment</button>`+
		`<button type="button" class="review-cancel" `+
		`hx-on:click="this.closest('tr').remove()">Cancel</button>`+
		`</div></form></td></tr>`,
		Esc(sessionID), Esc(file), Esc(line), Esc(side))
}

func (h *Hub) handleReviewComment(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	file := r.FormValue("file")
	lineStr := r.FormValue("line")
	side := r.FormValue("side")
	text := strings.TrimSpace(r.FormValue("text"))

	if sessionID == "" || file == "" || text == "" {
		w.WriteHeader(204)
		return
	}

	line, _ := strconv.Atoi(lineStr)

	c := ReviewComment{
		ID:   randomID(),
		File: file,
		Line: line,
		Side: side,
		Text: text,
	}
	h.Reviews.Add(sessionID, c)

	// Return rendered comment chip in a <tr> (replaces the form's <tr> via outerHTML swap)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<tr class="diff-comment-slot" id="rc-%s"><td colspan="3">`+
		`<div class="review-comment">`+
		`<span class="rc-meta">%s:%d</span>`+
		`<span class="rc-text">%s</span>`+
		`<button class="rc-delete" hx-post="/review/delete" `+
		`hx-vals='{"session_id":"%s","comment_id":"%s"}' `+
		`hx-target="closest tr" hx-swap="outerHTML">✕</button></div>`+
		`</td></tr>`,
		Esc(c.ID), Esc(file), line, Esc(text), Esc(sessionID), Esc(c.ID))

	// Broadcast updated review bar via OOB
	count := h.Reviews.Count(sessionID)
	barHTML := RenderReviewBar(sessionID, count)
	oob := OobSwap("review-bar", "outerHTML", barHTML)
	h.BroadcastToSession(sessionID, FormatSSE("htmx", oob))
}

func (h *Hub) handleReviewDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	commentID := r.FormValue("comment_id")

	h.Reviews.Remove(sessionID, commentID)

	// Return empty (removes the chip via outerHTML swap)

	// Broadcast updated review bar
	count := h.Reviews.Count(sessionID)
	barHTML := RenderReviewBar(sessionID, count)
	if barHTML == "" {
		barHTML = `<div class="review-bar" id="review-bar"></div>`
	}
	oob := OobSwap("review-bar", "outerHTML", barHTML)
	h.BroadcastToSession(sessionID, FormatSSE("htmx", oob))
}

func (h *Hub) handleReviewSend(w http.ResponseWriter, r *http.Request) {
	sessionID := r.FormValue("session_id")
	s := h.Sessions.Get(sessionID)
	if s == nil {
		w.WriteHeader(204)
		return
	}

	msg := h.Reviews.FormatReviewMessage(sessionID)
	if msg == "" {
		w.WriteHeader(204)
		return
	}

	h.Reviews.Clear(sessionID)

	// Send as a user message
	if queued, queuedText := s.EnqueueMessage(msg); queued {
		h.BroadcastToSession(s.ID, FormatSSE("htmx", RenderQueueBar(s.ID, queuedText)))
	} else if s.HasPendingPerms() {
		s.Append(ServerMsg{Type: "user_message", SessionID: s.ID, Text: msg})
		h.Broadcast(ServerMsg{Type: "user_message", SessionID: s.ID, Text: msg})
		if proc := s.GetProc(); proc != nil {
			proc.SendUserMessage(msg, nil)
		}
	} else {
		h.StartTurn(s, msg, nil)
	}

	// Clear the review bar
	barHTML := `<div class="review-bar" id="review-bar"></div>`
	oob := OobSwap("review-bar", "outerHTML", barHTML)
	h.BroadcastToSession(sessionID, FormatSSE("htmx", oob))

	w.WriteHeader(204)
}
