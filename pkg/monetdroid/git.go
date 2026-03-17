package monetdroid

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

type DiffStat struct {
	Added   int
	Removed int
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

func RenderDiffStat(sessionID string, stat DiffStat) string {
	if stat.Added == 0 && stat.Removed == 0 {
		return fmt.Sprintf(`<a href="/files?session=%s" class="diff-stat-link" style="color:var(--text2)">files</a>`, Esc(sessionID))
	}
	return fmt.Sprintf(`<a href="/files?session=%s" class="diff-stat-link"><span class="diff-add">+%d</span> <span class="diff-rm">−%d</span></a>`,
		Esc(sessionID), stat.Added, stat.Removed)
}

// StatusFile represents a file from git status --porcelain.
type StatusFile struct {
	Path     string
	Index    byte // X column: staging area status
	Worktree byte // Y column: working tree status
}

func (f StatusFile) IsUntracked() bool { return f.Index == '?' }
func (f StatusFile) IsStaged() bool    { return f.Index != ' ' && f.Index != '?' }
func (f StatusFile) IsModified() bool  { return f.Worktree == 'M' || f.Worktree == 'D' }

// FileEntry represents a file or directory in a directory listing.
type FileEntry struct {
	Name  string
	IsDir bool
}

// SearchMatch represents a single match from git grep.
type SearchMatch struct {
	File string
	Line int
	Text string
}

// GitStatusFiles returns the list of files from git status --porcelain -u.
func GitStatusFiles(cwd string) ([]StatusFile, error) {
	cmd := exec.Command("git", "status", "--porcelain", "-u")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []StatusFile
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		files = append(files, StatusFile{
			Index:    line[0],
			Worktree: line[1],
			Path:     line[3:],
		})
	}
	return files, nil
}

// GitDiffFileContent returns the diff for a single file.
// mode is "staged" (--cached), "unstaged" (working tree), or "untracked" (--no-index).
func GitDiffFileContent(cwd, path, mode string) (string, error) {
	var cmd *exec.Cmd
	switch mode {
	case "staged":
		cmd = exec.Command("git", "diff", "--cached", "-w", "--", path)
	case "untracked":
		cmd = exec.Command("git", "diff", "--no-index", "-w", "--", "/dev/null", path)
	default:
		cmd = exec.Command("git", "diff", "-w", "--", path)
	}
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		// git diff --no-index exits 1 when files differ (normal)
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
			return string(out), nil
		}
		return "", err
	}
	return string(out), nil
}

// GitStage stages files. If paths is empty, stages all (git add -A).
func GitStage(cwd string, paths []string) error {
	var args []string
	if len(paths) == 0 {
		args = []string{"add", "-A"}
	} else {
		args = append([]string{"add", "--"}, paths...)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	return cmd.Run()
}

// GitUnstage unstages files. If paths is empty, unstages all (git reset HEAD).
func GitUnstage(cwd string, paths []string) error {
	var args []string
	if len(paths) == 0 {
		args = []string{"reset", "HEAD"}
	} else {
		args = append([]string{"reset", "HEAD", "--"}, paths...)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	return cmd.Run()
}

// GitListDir lists files and directories under dir (relative to cwd), respecting .gitignore.
func GitListDir(cwd, dir string) ([]FileEntry, error) {
	args := []string{"ls-files", "--cached", "--others", "--exclude-standard"}
	if dir != "" {
		args = append(args, dir+"/")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}

	seen := make(map[string]bool)
	var entries []FileEntry
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		rel := strings.TrimPrefix(line, prefix)
		if idx := strings.IndexByte(rel, '/'); idx >= 0 {
			dirName := rel[:idx]
			key := dirName + "/"
			if !seen[key] {
				seen[key] = true
				entries = append(entries, FileEntry{Name: dirName, IsDir: true})
			}
		} else {
			if !seen[rel] {
				seen[rel] = true
				entries = append(entries, FileEntry{Name: rel, IsDir: false})
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// GitGrep searches for a regex pattern across the repository.
func GitGrep(cwd, pattern string) ([]SearchMatch, error) {
	cmd := exec.Command("git", "grep", "-n", "-I", "-E", "--untracked", pattern)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		// git grep exits 1 when no matches
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var results []SearchMatch
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		// Format: file:line:content
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		lineNum, _ := strconv.Atoi(parts[1])
		results = append(results, SearchMatch{
			File: parts[0],
			Line: lineNum,
			Text: parts[2],
		})
	}
	// Limit results
	if len(results) > 500 {
		results = results[:500]
	}
	return results, nil
}
