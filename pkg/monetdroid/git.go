package monetdroid

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// GitCommonDir returns the path to the shared .git directory for a repo or worktree.
// For the main checkout this returns the .git dir; for worktrees it returns the main
// repo's .git dir. Returns "" if cwd is not a git repo.
func GitCommonDir(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	return p
}

// MainWorktree resolves a cwd (which may be a linked worktree) to the main
// worktree's root directory. Falls back to cwd if not a git repo.
func MainWorktree(cwd string) string {
	gcd := GitCommonDir(cwd)
	if gcd == "" {
		return cwd
	}
	return filepath.Dir(gcd) // /work/.git → /work
}

// GitToplevel returns the repository root directory.
func GitToplevel(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// GitDefaultBranch returns the default branch name (e.g. "main" or "master").
func GitDefaultBranch(cwd string) string {
	// Try the symbolic ref for origin/HEAD first.
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = cwd
	if out, err := cmd.Output(); err == nil {
		ref := strings.TrimSpace(string(out))
		// refs/remotes/origin/main → main
		if i := strings.LastIndex(ref, "/"); i >= 0 {
			return ref[i+1:]
		}
	}
	// Fallback: check if "main" exists, otherwise "master".
	for _, name := range []string{"main", "master"} {
		cmd = exec.Command("git", "rev-parse", "--verify", "refs/heads/"+name)
		cmd.Dir = cwd
		if cmd.Run() == nil {
			return name
		}
	}
	return "main"
}

// WorktreeDir returns the path where Monetdroid stores worktrees for a repo.
// Layout: ~/.monetdroid/worktrees/<repo-basename>/
func WorktreeDir(repoRoot string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".monetdroid", "worktrees", filepath.Base(repoRoot))
}

// CreateWorkstream creates a new branch off the default branch and a worktree for it.
// The branch's upstream is set to the default branch for stack topology inference.
// Returns the worktree path.
func CreateWorkstream(cwd, name string) (string, error) {
	repoRoot := GitToplevel(cwd)
	if repoRoot == "" {
		return "", fmt.Errorf("not a git repository: %s", cwd)
	}

	defaultBranch := GitDefaultBranch(repoRoot)
	wtPath := filepath.Join(WorktreeDir(repoRoot), name)

	cmd := exec.Command("git", "worktree", "add", "-b", name, "--track", wtPath, defaultBranch)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %s", strings.TrimSpace(string(out)))
	}

	return wtPath, nil
}

// BranchStatus holds git status information for a single branch.
type BranchStatus struct {
	Name         string // branch name
	Depth        int    // depth in the stack tree (0 = direct child of main)
	Upstream     string // upstream branch (e.g. "main" or "auth")
	AheadMain    int    // commits ahead of default branch
	BehindMain   int    // commits behind default branch
	AheadRemote  int    // commits ahead of remote tracking branch
	BehindRemote int    // commits behind remote tracking branch
	RemoteGone   bool   // remote tracking branch was deleted
	HasRemote    bool   // has a remote tracking branch at all
	Dirty        bool   // uncommitted changes in worktree
}

// WorkstreamStatus holds status for a Monetdroid-managed workstream.
type WorkstreamStatus struct {
	Name     string         // worktree directory name (= workstream name)
	Path     string         // absolute path to worktree
	Branches []BranchStatus // branch stack in topological order (root first)
}

// BranchPanel holds everything needed to render the branch list for a repo.
type BranchPanel struct {
	DefaultBranch string             // e.g. "main" or "master"
	MainDirty     bool               // uncommitted changes in main worktree
	RepoPath      string             // main worktree path (for actions)
	Workstreams   []WorkstreamStatus // workstreams with branch status
}

// AllWorkstreams returns workstream status grouped by repo, scanning ~/.monetdroid/worktrees/.
func AllWorkstreams() map[string]BranchPanel {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	baseDir := filepath.Join(home, ".monetdroid", "worktrees")
	repos, err := os.ReadDir(baseDir)
	if err != nil {
		return nil
	}
	result := make(map[string]BranchPanel)
	for _, repo := range repos {
		if !repo.IsDir() {
			continue
		}
		repoDir := filepath.Join(baseDir, repo.Name())
		ws := listWorkstreamsInDir(repoDir)
		if len(ws) == 0 {
			continue
		}
		repoPath := MainWorktree(ws[0].Path)
		defaultBranch := GitDefaultBranch(repoPath)
		mainDirty := false
		if files, err := GitStatusFiles(repoPath); err == nil && len(files) > 0 {
			mainDirty = true
		}
		result[repo.Name()] = BranchPanel{
			DefaultBranch: defaultBranch,
			MainDirty:     mainDirty,
			RepoPath:      repoPath,
			Workstreams:   ws,
		}
	}
	return result
}

// listWorkstreamsInDir returns status for all workstreams under a worktree directory.
func listWorkstreamsInDir(wtDir string) []WorkstreamStatus {
	entries, err := os.ReadDir(wtDir)
	if err != nil {
		return nil
	}

	// Find default branch from the first valid worktree.
	var defaultBranch string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "pool-") {
			continue
		}
		wtPath := filepath.Join(wtDir, e.Name())
		if db := GitDefaultBranch(wtPath); db != "" {
			defaultBranch = db
			break
		}
	}
	if defaultBranch == "" {
		return nil
	}

	var result []WorkstreamStatus
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "pool-") {
			continue
		}
		wtPath := filepath.Join(wtDir, e.Name())
		// Verify it's actually a git worktree.
		cmd := exec.Command("git", "rev-parse", "--git-dir")
		cmd.Dir = wtPath
		if cmd.Run() != nil {
			continue
		}
		branches := branchStack(wtPath, defaultBranch)
		ws := WorkstreamStatus{
			Name:     e.Name(),
			Path:     wtPath,
			Branches: branches,
		}
		result = append(result, ws)
	}
	return result
}

// branchStack returns the branch stack for a worktree, walking the upstream chain.
// Returns branches in topological order (root/closest-to-main first).
func branchStack(wtPath, defaultBranch string) []BranchStatus {
	// Get current branch.
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	currentBranch := strings.TrimSpace(string(out))
	if currentBranch == "HEAD" {
		return nil // detached HEAD
	}

	bs := branchStatus(wtPath, currentBranch, defaultBranch)

	// Check for dirty worktree.
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	if out, err := cmd.Output(); err == nil {
		if len(strings.TrimSpace(string(out))) > 0 {
			bs.Dirty = true
		}
	}

	return []BranchStatus{bs}
}

// branchStatus gathers status for a single branch.
func branchStatus(cwd, branch, defaultBranch string) BranchStatus {
	bs := BranchStatus{Name: branch}

	// Get upstream.
	cmd := exec.Command("git", "config", fmt.Sprintf("branch.%s.merge", branch))
	cmd.Dir = cwd
	if out, err := cmd.Output(); err == nil {
		ref := strings.TrimSpace(string(out))
		// refs/heads/main → main
		bs.Upstream = strings.TrimPrefix(ref, "refs/heads/")
	}

	// Ahead/behind default branch.
	cmd = exec.Command("git", "rev-list", "--left-right", "--count",
		fmt.Sprintf("%s...%s", defaultBranch, branch))
	cmd.Dir = cwd
	if out, err := cmd.Output(); err == nil {
		parts := strings.Fields(strings.TrimSpace(string(out)))
		if len(parts) == 2 {
			bs.BehindMain, _ = strconv.Atoi(parts[0])
			bs.AheadMain, _ = strconv.Atoi(parts[1])
		}
	}

	// Remote tracking info.
	cmd = exec.Command("git", "config", fmt.Sprintf("branch.%s.remote", branch))
	cmd.Dir = cwd
	remoteOut, remoteErr := cmd.Output()
	remote := strings.TrimSpace(string(remoteOut))
	if remoteErr != nil || remote == "" || remote == "." {
		// No remote or local-only upstream.
		return bs
	}

	// Check if remote tracking branch still exists.
	remoteBranch := remote + "/" + branch
	cmd = exec.Command("git", "rev-parse", "--verify", "refs/remotes/"+remoteBranch)
	cmd.Dir = cwd
	if cmd.Run() != nil {
		bs.HasRemote = true
		bs.RemoteGone = true
		return bs
	}

	bs.HasRemote = true
	// Ahead/behind remote.
	cmd = exec.Command("git", "rev-list", "--left-right", "--count",
		fmt.Sprintf("%s...%s", remoteBranch, branch))
	cmd.Dir = cwd
	if out, err := cmd.Output(); err == nil {
		parts := strings.Fields(strings.TrimSpace(string(out)))
		if len(parts) == 2 {
			bs.BehindRemote, _ = strconv.Atoi(parts[0])
			bs.AheadRemote, _ = strconv.Atoi(parts[1])
		}
	}

	return bs
}

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
		} else if !seen[rel] {
			seen[rel] = true
			entries = append(entries, FileEntry{Name: rel, IsDir: false})
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

// CommitEntry represents a single commit from git log.
type CommitEntry struct {
	Hash      string
	ShortHash string
	Subject   string
	Author    string
	TimeAgo   string
}

// GitLog returns recent commits.
func GitLog(cwd string, limit int) ([]CommitEntry, error) {
	cmd := exec.Command("git", "log", fmt.Sprintf("-%d", limit), "--format=%H%x00%h%x00%s%x00%an%x00%ar")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var commits []CommitEntry
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 5)
		if len(parts) < 5 {
			continue
		}
		commits = append(commits, CommitEntry{
			Hash:      parts[0],
			ShortHash: parts[1],
			Subject:   parts[2],
			Author:    parts[3],
			TimeAgo:   parts[4],
		})
	}
	return commits, nil
}

// GitShowCommit returns the diff for a specific commit.
func GitShowCommit(cwd, hash string) (string, error) {
	cmd := exec.Command("git", "show", "-w", "--format=", hash)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// GitLogOne returns metadata for a single commit by hash.
func GitLogOne(cwd, hash string) (CommitEntry, error) {
	cmd := exec.Command("git", "log", "-1", "--format=%H%x00%h%x00%s%x00%an%x00%ar", hash)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return CommitEntry{}, err
	}
	parts := strings.SplitN(strings.TrimRight(string(out), "\n"), "\x00", 5)
	if len(parts) < 5 {
		return CommitEntry{}, fmt.Errorf("unexpected git log output")
	}
	return CommitEntry{
		Hash:      parts[0],
		ShortHash: parts[1],
		Subject:   parts[2],
		Author:    parts[3],
		TimeAgo:   parts[4],
	}, nil
}

// GitShowCommitFiles returns the files changed in a specific commit.
func GitShowCommitFiles(cwd, hash string) ([]string, error) {
	cmd := exec.Command("git", "show", "--name-only", "--format=", hash)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}
