package monetdroid

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type DiffStat struct {
	Added   int
	Removed int
}

type DiffFile struct {
	Status string // "M", "A", "D", "R", etc.
	Name   string
}

func GitDiffStat(cwd string) (DiffStat, error) {
	cmd := exec.Command("git", "diff", "HEAD", "-w", "--numstat")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return DiffStat{}, err
	}
	var stat DiffStat
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// Binary files show "-" for add/remove counts
		if a, err := strconv.Atoi(fields[0]); err == nil {
			stat.Added += a
		}
		if r, err := strconv.Atoi(fields[1]); err == nil {
			stat.Removed += r
		}
	}
	return stat, nil
}

func GitDiffFiles(cwd string) ([]DiffFile, error) {
	cmd := exec.Command("git", "diff", "HEAD", "-w", "--name-status")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []DiffFile
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) < 2 {
			continue
		}
		files = append(files, DiffFile{Status: fields[0], Name: fields[1]})
	}
	return files, nil
}

func GitDiffFull(cwd string) (string, error) {
	cmd := exec.Command("git", "diff", "HEAD", "-w")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func RenderDiffStat(sessionID string, stat DiffStat) string {
	if stat.Added == 0 && stat.Removed == 0 {
		return ""
	}
	return fmt.Sprintf(`<a href="/diff?session=%s" class="diff-stat-link"><span class="diff-add">+%d</span> <span class="diff-rm">−%d</span></a>`,
		Esc(sessionID), stat.Added, stat.Removed)
}
