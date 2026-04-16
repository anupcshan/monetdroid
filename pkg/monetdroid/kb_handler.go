package monetdroid

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/anupcshan/monetdroid/pkg/kb"
)

func (h *Hub) handleKB(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/kb/")
	if path == "" {
		path = "index.md"
	}

	cwd := r.URL.Query().Get("cwd")
	if cwd == "" {
		http.Error(w, "cwd parameter required", http.StatusBadRequest)
		return
	}
	k, err := kb.Resolve(cwd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	content, err := k.Read(path, 0, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	rendered := renderKBMarkdown(content, cwd)

	title := path

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(renderKBPage(title, rendered)))
}

var relLinkRe = regexp.MustCompile(`<a href="([^":#][^"]*\.md(?:#[^"]*)?)"`)
var extLinkRe = regexp.MustCompile(`<a href="https?://`)

func renderKBMarkdown(text, cwd string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return Esc(text)
	}

	cwdParam := ""
	if cwd != "" {
		cwdParam = "?cwd=" + Esc(cwd)
	}

	result := buf.String()
	result = relLinkRe.ReplaceAllStringFunc(result, func(match string) string {
		sub := relLinkRe.FindStringSubmatch(match)
		return `<a href="/kb/` + sub[1] + cwdParam + `"`
	})
	result = extLinkRe.ReplaceAllStringFunc(result, func(match string) string {
		return strings.Replace(match, "<a ", `<a target="_blank" rel="noopener" `, 1)
	})
	return result
}

func renderKBPage(title, content string) string {
	var b strings.Builder

	b.WriteString(`<!DOCTYPE html><html lang="en" class="kb-page"><head><meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>`)
	b.WriteString(Esc(title))
	b.WriteString(` · KB</title>
<link rel="stylesheet" href="/assets/styles.css">
</head><body>
`)

	fmt.Fprintf(&b, `<div class="files-header"><h1>%s</h1></div>`, Esc(title))
	b.WriteString(`<div class="kb-content">`)
	b.WriteString(content)
	b.WriteString(`</div>`)
	b.WriteString(`</body></html>`)
	return b.String()
}
