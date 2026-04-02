package monetdroid

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
)

// chromaFormatterBare formats tokens without a <pre> wrapper,
// suitable for embedding highlighted code inside table cells.
var chromaFormatterBare = chromahtml.New(chromahtml.PreventSurroundingPre(true))

// highlightLines tokenizes content with the given lexer and returns
// per-line HTML. Line indices are 0-based: result[0] is line 1 of the content.
func highlightLines(lexer chroma.Lexer, content string) []string {
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	iterator, err := lexer.Tokenise(nil, content)
	if err != nil {
		// Fallback: escape each line
		raw := strings.Split(content, "\n")
		for i := range raw {
			raw[i] = Esc(raw[i])
		}
		return raw
	}

	// Collect all tokens and split into per-line groups.
	allTokens := iterator.Tokens()
	lineGroups := chroma.SplitTokensIntoLines(allTokens)

	result := make([]string, len(lineGroups))
	for i, tokens := range lineGroups {
		// Strip trailing newline token if present.
		if len(tokens) > 0 {
			last := &tokens[len(tokens)-1]
			last.Value = strings.TrimRight(last.Value, "\n")
		}
		lit := chroma.Literator(tokens...)
		var buf bytes.Buffer
		chromaFormatterBare.Format(&buf, chromaStyle, lit)
		result[i] = buf.String()
	}
	return result
}

// RenderWriteDiffTable renders a Write tool's content as an all-additions diff
// table with syntax highlighting.
func RenderWriteDiffTable(filePath, content, sessionID string, reviewEnabled bool) string {
	lexer := lexers.Match(filePath)
	highlighted := highlightLines(lexer, content)
	lines := strings.Split(content, "\n")

	var b strings.Builder
	b.WriteString(`<div class="diff-file-wrap"><table class="diff-table">`)
	for i, hl := range highlighted {
		if i >= len(lines) {
			break
		}
		lineNum := i + 1
		fmt.Fprintf(&b, `<tr class="diff-line diff-ins">`)
		fmt.Fprintf(&b, `<td class="diff-gutter diff-gutter-old"></td>`)
		if reviewEnabled && sessionID != "" {
			writeCommentGutter(&b, fmt.Sprintf("%d", lineNum), sessionID, filePath, lineNum)
		} else {
			fmt.Fprintf(&b, `<td class="diff-gutter diff-gutter-new">%d</td>`, lineNum)
		}
		fmt.Fprintf(&b, `<td class="diff-code">%s</td>`, hl)
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</table></div>`)
	return b.String()
}

// RenderEditDiffTable renders an Edit tool diff as a table with line numbers
// and language-specific syntax highlighting.
func RenderEditDiffTable(filePath, oldStr, newStr, sessionID string, reviewEnabled bool) string {
	diffText := runDiff(filePath, oldStr, newStr, 3)
	if diffText == "" {
		return ""
	}
	files := ParseUnifiedDiff(diffText)
	if len(files) == 0 {
		// Fallback to old renderer
		return highlightDiff(diffText)
	}

	// Highlight old and new content as complete blocks for accurate
	// multi-line token recognition (block comments, strings, etc.).
	lexer := lexers.Match(filePath)
	oldHighlighted := highlightLines(lexer, oldStr)
	newHighlighted := highlightLines(lexer, newStr)

	return renderDiffFiles(files, sessionID, reviewEnabled, func(f *DiffFile, dl DiffLine) string {
		switch dl.Type {
		case DiffLineRemove:
			if dl.OldLine > 0 && dl.OldLine <= len(oldHighlighted) {
				return oldHighlighted[dl.OldLine-1]
			}
		case DiffLineAdd:
			if dl.NewLine > 0 && dl.NewLine <= len(newHighlighted) {
				return newHighlighted[dl.NewLine-1]
			}
		case DiffLineContext:
			if dl.OldLine > 0 && dl.OldLine <= len(oldHighlighted) {
				return oldHighlighted[dl.OldLine-1]
			}
		}
		return Esc(dl.Content)
	})
}

// RenderDiffTableFromUnified renders a unified diff (e.g. from git diff) as
// a table with line numbers. Uses per-hunk reconstruction for highlighting.
func RenderDiffTableFromUnified(diffText, sessionID string, reviewEnabled bool) string {
	files := ParseUnifiedDiff(diffText)
	if len(files) == 0 {
		return highlightDiff(diffText)
	}

	return renderDiffFiles(files, sessionID, reviewEnabled, nil)
}

// renderDiffFiles renders parsed DiffFiles as HTML tables.
// If lineHTML is nil, per-hunk reconstruction highlighting is used.
func renderDiffFiles(files []DiffFile, sessionID string, reviewEnabled bool, lineHTML func(*DiffFile, DiffLine) string) string {
	var b strings.Builder

	for fi := range files {
		f := &files[fi]
		fileName := f.NewName
		if fileName == "" {
			fileName = f.OldName
		}

		// For per-hunk highlighting when we don't have full old/new content
		var hunkHighlighter func(DiffLine) string
		if lineHTML == nil {
			lexer := lexers.Match(fileName)
			hunkHighlighter = makeHunkHighlighter(lexer, f)
		}

		b.WriteString(`<div class="diff-file-wrap">`)
		if len(files) > 1 {
			fmt.Fprintf(&b, `<div class="diff-file-name">%s</div>`, Esc(fileName))
		}

		if f.Binary {
			b.WriteString(`<div class="diff-binary">Binary file changed</div>`)
			b.WriteString(`</div>`)
			continue
		}

		b.WriteString(`<table class="diff-table">`)

		for _, hunk := range f.Hunks {
			// Hunk header row
			fmt.Fprintf(&b, `<tr class="diff-hunk"><td class="diff-gutter" colspan="2"></td><td class="diff-code diff-hunk-text">%s</td></tr>`, Esc(hunk.Header))

			for _, dl := range hunk.Lines {
				var lineClass, gutterOld, gutterNew string

				switch dl.Type {
				case DiffLineContext:
					lineClass = "diff-ctx"
					gutterOld = fmt.Sprintf("%d", dl.OldLine)
					gutterNew = fmt.Sprintf("%d", dl.NewLine)
				case DiffLineAdd:
					lineClass = "diff-ins"
					gutterNew = fmt.Sprintf("%d", dl.NewLine)
				case DiffLineRemove:
					lineClass = "diff-del"
					gutterOld = fmt.Sprintf("%d", dl.OldLine)
				}

				// Get highlighted HTML for this line's content
				var codeHTML string
				if lineHTML != nil {
					codeHTML = lineHTML(f, dl)
				} else {
					codeHTML = hunkHighlighter(dl)
				}

				fmt.Fprintf(&b, `<tr class="diff-line %s">`, lineClass)
				fmt.Fprintf(&b, `<td class="diff-gutter diff-gutter-old">%s</td>`, gutterOld)

				// New-line gutter: includes comment button when review is enabled
				if reviewEnabled && sessionID != "" && dl.Type != DiffLineRemove {
					writeCommentGutter(&b, gutterNew, sessionID, fileName, dl.NewLine)
				} else {
					fmt.Fprintf(&b, `<td class="diff-gutter diff-gutter-new">%s</td>`, gutterNew)
				}

				fmt.Fprintf(&b, `<td class="diff-code">%s</td>`, codeHTML)
				b.WriteString(`</tr>`)
			}
		}

		b.WriteString(`</table></div>`)
	}

	return b.String()
}

// writeCommentGutter writes a gutter cell with a tappable line number and hover comment button.
func writeCommentGutter(b *strings.Builder, display, sessionID, filePath string, lineNum int) {
	commentURL := fmt.Sprintf("/review/comment-form?session=%s&amp;file=%s&amp;line=%d&amp;side=new",
		Esc(sessionID), Esc(filePath), lineNum)
	fmt.Fprintf(b, `<td class="diff-gutter diff-gutter-new">`+
		`<span class="diff-line-num" hx-get="%s" hx-target="closest tr" hx-swap="afterend" hx-trigger="click">%s</span>`+
		`<button class="diff-comment-btn" hx-get="%s" hx-target="closest tr" hx-swap="afterend">+</button></td>`,
		commentURL, display, commentURL)
}

// makeHunkHighlighter builds a highlighter that reconstructs old/new content
// from hunk lines for accurate multi-line token highlighting.
// Falls back to per-line highlighting if reconstruction isn't possible.
func makeHunkHighlighter(lexer chroma.Lexer, f *DiffFile) func(DiffLine) string {
	if lexer == nil {
		lexer = lexers.Fallback
	}

	// Reconstruct old and new content from all hunks
	var oldContent, newContent strings.Builder
	type lineRef struct {
		oldIdx int // index in oldLines (0-based), or -1
		newIdx int // index in newLines (0-based), or -1
	}
	var refs []lineRef
	oldIdx, newIdx := 0, 0

	for _, hunk := range f.Hunks {
		for _, dl := range hunk.Lines {
			ref := lineRef{oldIdx: -1, newIdx: -1}
			switch dl.Type {
			case DiffLineContext:
				oldContent.WriteString(dl.Content)
				oldContent.WriteByte('\n')
				newContent.WriteString(dl.Content)
				newContent.WriteByte('\n')
				ref.oldIdx = oldIdx
				ref.newIdx = newIdx
				oldIdx++
				newIdx++
			case DiffLineRemove:
				oldContent.WriteString(dl.Content)
				oldContent.WriteByte('\n')
				ref.oldIdx = oldIdx
				oldIdx++
			case DiffLineAdd:
				newContent.WriteString(dl.Content)
				newContent.WriteByte('\n')
				ref.newIdx = newIdx
				newIdx++
			}
			refs = append(refs, ref)
		}
	}

	oldHL := highlightLines(lexer, oldContent.String())
	newHL := highlightLines(lexer, newContent.String())

	// Build flat line list matching the order of all hunk lines across all hunks
	refIdx := 0
	return func(dl DiffLine) string {
		if refIdx >= len(refs) {
			return Esc(dl.Content)
		}
		ref := refs[refIdx]
		refIdx++
		switch dl.Type {
		case DiffLineRemove:
			if ref.oldIdx >= 0 && ref.oldIdx < len(oldHL) {
				return oldHL[ref.oldIdx]
			}
		case DiffLineAdd:
			if ref.newIdx >= 0 && ref.newIdx < len(newHL) {
				return newHL[ref.newIdx]
			}
		case DiffLineContext:
			if ref.oldIdx >= 0 && ref.oldIdx < len(oldHL) {
				return oldHL[ref.oldIdx]
			}
		}
		return Esc(dl.Content)
	}
}
