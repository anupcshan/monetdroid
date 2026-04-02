package monetdroid

import (
	"regexp"
	"strconv"
	"strings"
)

// DiffLineType classifies a line within a diff hunk.
type DiffLineType int

const (
	DiffLineContext DiffLineType = iota
	DiffLineAdd
	DiffLineRemove
)

// DiffLine represents a single line in a parsed diff.
type DiffLine struct {
	Type    DiffLineType
	Content string // line content without the +/-/space prefix
	OldLine int    // line number in old file (0 for additions)
	NewLine int    // line number in new file (0 for removals)
}

// DiffHunk represents a contiguous block of changes.
type DiffHunk struct {
	Header   string // the full @@ ... @@ line
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Lines    []DiffLine
}

// DiffFile represents changes to a single file.
type DiffFile struct {
	OldName string // path from --- line (or diff --git a/ prefix)
	NewName string // path from +++ line (or diff --git b/ prefix)
	Binary  bool   // true if "Binary files ... differ"
	Hunks   []DiffHunk
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParseUnifiedDiff parses unified diff text into structured types.
// Handles multi-file git diffs (diff --git) and single-file diffs (Edit tool).
func ParseUnifiedDiff(diffText string) []DiffFile {
	if diffText == "" {
		return nil
	}

	lines := strings.Split(diffText, "\n")
	// Remove trailing empty line from split
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	// Split into per-file sections
	sections := splitIntoFileSections(lines)
	if len(sections) == 0 {
		return nil
	}

	var files []DiffFile
	for _, section := range sections {
		if f := parseFileSection(section); f != nil {
			files = append(files, *f)
		}
	}
	return files
}

// splitIntoFileSections splits diff lines into per-file groups.
// Each group starts with "diff --git" or "---" (for single-file diffs).
func splitIntoFileSections(lines []string) [][]string {
	var sections [][]string
	var current []string

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			if len(current) > 0 {
				sections = append(sections, current)
			}
			current = []string{line}
		} else {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		sections = append(sections, current)
	}
	return sections
}

// parseFileSection parses a single file's diff section.
func parseFileSection(lines []string) *DiffFile {
	if len(lines) == 0 {
		return nil
	}

	f := &DiffFile{}

	i := 0

	// Parse "diff --git a/path b/path" header if present
	if strings.HasPrefix(lines[i], "diff --git ") {
		parts := strings.Fields(lines[i])
		if len(parts) >= 4 {
			f.OldName = strings.TrimPrefix(parts[2], "a/")
			f.NewName = strings.TrimPrefix(parts[3], "b/")
		}
		i++
	}

	// Skip index, old mode, new mode, similarity lines
	for i < len(lines) {
		line := lines[i]
		if strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "old mode ") ||
			strings.HasPrefix(line, "new mode ") ||
			strings.HasPrefix(line, "new file mode ") ||
			strings.HasPrefix(line, "deleted file mode ") ||
			strings.HasPrefix(line, "similarity index ") ||
			strings.HasPrefix(line, "rename from ") ||
			strings.HasPrefix(line, "rename to ") ||
			strings.HasPrefix(line, "copy from ") ||
			strings.HasPrefix(line, "copy to ") {
			i++
			continue
		}
		break
	}

	// Check for binary file marker
	if i < len(lines) && strings.HasPrefix(lines[i], "Binary files ") {
		f.Binary = true
		return f
	}

	// Parse --- and +++ lines
	if i < len(lines) && strings.HasPrefix(lines[i], "--- ") {
		name := strings.TrimPrefix(lines[i], "--- ")
		name = strings.TrimPrefix(name, "a/")
		if f.OldName == "" {
			f.OldName = name
		}
		i++
	}
	if i < len(lines) && strings.HasPrefix(lines[i], "+++ ") {
		name := strings.TrimPrefix(lines[i], "+++ ")
		name = strings.TrimPrefix(name, "b/")
		if f.NewName == "" {
			f.NewName = name
		}
		i++
	}

	// Parse hunks
	for i < len(lines) {
		if !strings.HasPrefix(lines[i], "@@") {
			i++
			continue
		}
		hunk, consumed := parseHunk(lines[i:])
		if hunk != nil {
			f.Hunks = append(f.Hunks, *hunk)
		}
		i += consumed
	}

	if len(f.Hunks) == 0 && !f.Binary {
		return nil
	}
	return f
}

// parseHunk parses a single hunk starting from an @@ line.
// Returns the parsed hunk and the number of lines consumed.
func parseHunk(lines []string) (*DiffHunk, int) {
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "@@") {
		return nil, 0
	}

	m := hunkHeaderRe.FindStringSubmatch(lines[0])
	if m == nil {
		return nil, 1
	}

	oldStart, _ := strconv.Atoi(m[1])
	oldCount := 1
	if m[2] != "" {
		oldCount, _ = strconv.Atoi(m[2])
	}
	newStart, _ := strconv.Atoi(m[3])
	newCount := 1
	if m[4] != "" {
		newCount, _ = strconv.Atoi(m[4])
	}

	hunk := &DiffHunk{
		Header:   lines[0],
		OldStart: oldStart,
		OldCount: oldCount,
		NewStart: newStart,
		NewCount: newCount,
	}

	oldLine := oldStart
	newLine := newStart
	i := 1

	for i < len(lines) {
		line := lines[i]

		// Next hunk or next file
		if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "diff --git ") {
			break
		}

		// No-newline marker — skip
		if strings.HasPrefix(line, `\ `) {
			i++
			continue
		}

		switch {
		case strings.HasPrefix(line, "+"):
			hunk.Lines = append(hunk.Lines, DiffLine{
				Type:    DiffLineAdd,
				Content: line[1:],
				NewLine: newLine,
			})
			newLine++
		case strings.HasPrefix(line, "-"):
			hunk.Lines = append(hunk.Lines, DiffLine{
				Type:    DiffLineRemove,
				Content: line[1:],
				OldLine: oldLine,
			})
			oldLine++
		default:
			// Context line: may start with " " or be empty (blank context line)
			content := line
			if len(line) > 0 && line[0] == ' ' {
				content = line[1:]
			}
			hunk.Lines = append(hunk.Lines, DiffLine{
				Type:    DiffLineContext,
				Content: content,
				OldLine: oldLine,
				NewLine: newLine,
			})
			oldLine++
			newLine++
		}
		i++
	}

	return hunk, i
}
