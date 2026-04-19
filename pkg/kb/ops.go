package kb

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (kb *KB) List() ([]string, error) {
	if !kb.Exists() {
		return nil, nil
	}

	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = kb.Path
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var files []string
	for _, entry := range strings.Split(string(out), "\000") {
		if entry != "" && entry != ".gitignore" {
			files = append(files, entry)
		}
	}
	return files, nil
}

func (kb *KB) Read(path string, offset, limit int) (string, error) {
	if !kb.Exists() {
		return "", fmt.Errorf("kb not initialized")
	}
	data, err := os.ReadFile(kb.fullPath(path))
	if err != nil {
		return "", err
	}

	if offset == 0 && limit == 0 {
		return string(data), nil
	}

	lines := strings.Split(string(data), "\n")
	if offset >= len(lines) {
		return "", nil
	}
	lines = lines[offset:]
	if limit > 0 && limit < len(lines) {
		lines = lines[:limit]
	}
	return strings.Join(lines, "\n"), nil
}

type EditInput struct {
	Old string
	New string
}

// ParseEditInput parses stdin in the format:
//
//	<separator>
//	<old content>
//	<separator>
//	<new content>
//
// The first line is the separator, chosen by the caller to be any literal
// string that does not appear on a line by itself within the content.
// A single trailing newline (the heredoc artifact) is stripped from stdin
// before parsing.
func ParseEditInput(data []byte) (EditInput, error) {
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return EditInput{}, fmt.Errorf("empty input")
	}

	lines := strings.Split(s, "\n")
	sep := lines[0]
	if sep == "" {
		return EditInput{}, fmt.Errorf("first line must be a non-empty separator")
	}

	secondIdx := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == sep {
			secondIdx = i
			break
		}
	}
	if secondIdx == -1 {
		return EditInput{}, fmt.Errorf("separator %q not found after first line", sep)
	}

	old := strings.Join(lines[1:secondIdx], "\n")
	if old == "" {
		return EditInput{}, fmt.Errorf("old content is empty")
	}
	newContent := strings.Join(lines[secondIdx+1:], "\n")
	return EditInput{Old: old, New: newContent}, nil
}

func (kb *KB) Edit(path string, input EditInput, all bool) error {
	if err := kb.checkWritable(); err != nil {
		return err
	}

	fp := kb.fullPath(path)
	data, err := os.ReadFile(fp)
	if err != nil {
		return err
	}

	content := string(data)
	count := strings.Count(content, input.Old)
	if count == 0 {
		return fmt.Errorf("old string not found in %s", path)
	}
	if !all && count > 1 {
		return fmt.Errorf("old string not unique in %s (%d occurrences)", path, count)
	}

	content = strings.ReplaceAll(content, input.Old, input.New)
	if err := os.WriteFile(fp, []byte(content), 0644); err != nil {
		return err
	}
	return kb.autoCommit(path)
}

func (kb *KB) Write(path string, content string) error {
	if err := kb.checkWritable(); err != nil {
		return err
	}
	if err := kb.ensureRepo(); err != nil {
		return err
	}

	fp := kb.fullPath(path)
	if err := os.MkdirAll(filepath.Dir(fp), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(fp, []byte(content), 0644); err != nil {
		return err
	}
	return kb.autoCommit(path)
}

func (kb *KB) Append(path string, content string) error {
	if err := kb.checkWritable(); err != nil {
		return err
	}
	if err := kb.ensureRepo(); err != nil {
		return err
	}

	fp := kb.fullPath(path)
	if err := os.MkdirAll(filepath.Dir(fp), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(fp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return kb.autoCommit(path)
}

func (kb *KB) Remove(path string) error {
	if err := kb.checkWritable(); err != nil {
		return err
	}
	if err := os.Remove(kb.fullPath(path)); err != nil {
		return err
	}
	return kb.autoCommit("rm " + path)
}

func (kb *KB) Move(oldPath, newPath string) error {
	if err := kb.checkWritable(); err != nil {
		return err
	}

	dst := kb.fullPath(newPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := os.Rename(kb.fullPath(oldPath), dst); err != nil {
		return err
	}
	return kb.autoCommit("mv " + oldPath + " -> " + newPath)
}

func (kb *KB) Search(query string) (string, error) {
	if !kb.Exists() {
		return "", nil
	}

	cmd := exec.Command("git", "grep", "-n", "--no-color", query)
	cmd.Dir = kb.Path
	out, err := cmd.Output()
	if err != nil {
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return string(out), nil
}
