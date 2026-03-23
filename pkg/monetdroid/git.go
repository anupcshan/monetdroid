package monetdroid

import (
	"encoding/json"
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
	Archived bool           // hidden from active list
	Branches []BranchStatus // branch stack in topological order (root first)
}

// BranchPanel holds everything needed to render the branch list for a repo.
type BranchPanel struct {
	DefaultBranch string             // e.g. "main" or "master"
	MainDirty     bool               // uncommitted changes in main worktree
	RepoPath      string             // main worktree path (for actions)
	Workstreams   []WorkstreamStatus // workstreams with branch status
}

// PruneBranch describes a branch's prune safety status.
type PruneBranch struct {
	Name    string
	Safe    bool   // safe to delete (merged or remote gone)
	Reason  string // human-readable reason
}

// PruneWorkstream describes a workstream ready for pruning.
type PruneWorkstream struct {
	Name     string
	Path     string
	Branches []PruneBranch
}

// PrunePlan describes what would be pruned across all archived workstreams.
type PrunePlan struct {
	Workstreams []PruneWorkstream
}

// BuildPrunePlan scans archived workstreams and classifies branches.
func BuildPrunePlan() PrunePlan {
	var plan PrunePlan
	for _, panel := range AllWorkstreams() {
		for _, ws := range panel.Workstreams {
			if !ws.Archived {
				continue
			}
			pw := PruneWorkstream{Name: ws.Name, Path: ws.Path}
			for _, br := range ws.Branches {
				pb := PruneBranch{Name: br.Name}
				switch {
				case br.RemoteGone:
					pb.Safe = true
					pb.Reason = "remote branch deleted"
				case br.AheadMain == 0:
					pb.Safe = true
					pb.Reason = "merged into " + panel.DefaultBranch
				case br.HasRemote:
					pb.Safe = false
					pb.Reason = fmt.Sprintf("%d unpushed commits, remote exists", br.AheadMain)
				default:
					pb.Safe = false
					pb.Reason = fmt.Sprintf("%d unpushed commits, no remote", br.AheadMain)
				}
				pw.Branches = append(pw.Branches, pb)
			}
			plan.Workstreams = append(plan.Workstreams, pw)
		}
	}
	return plan
}

// ExecutePrune deletes worktrees and safe branches for the given workstreams.
func ExecutePrune(paths []string) []string {
	var log []string
	for _, wsPath := range paths {
		repoRoot := MainWorktree(wsPath)
		defaultBranch := GitDefaultBranch(repoRoot)
		branches := branchStack(wsPath, defaultBranch)

		cmd := exec.Command("git", "worktree", "remove", wsPath)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			log = append(log, fmt.Sprintf("error removing worktree %s: %s", filepath.Base(wsPath), strings.TrimSpace(string(out))))
			continue
		}
		log = append(log, fmt.Sprintf("removed worktree %s", filepath.Base(wsPath)))

		// Delete safe branches.
		for _, br := range branches {
			safe := br.RemoteGone || br.AheadMain == 0
			if !safe {
				log = append(log, fmt.Sprintf("kept branch %s (%d commits ahead)", br.Name, br.AheadMain))
				continue
			}
			cmd = exec.Command("git", "branch", "-d", br.Name)
			cmd.Dir = repoRoot
			if out, err := cmd.CombinedOutput(); err != nil {
				log = append(log, fmt.Sprintf("error deleting branch %s: %s", br.Name, strings.TrimSpace(string(out))))
			} else {
				log = append(log, fmt.Sprintf("deleted branch %s", br.Name))
			}
		}

		// Clean up archived entry.
		UnarchiveWorkstream(wsPath)
	}
	return log
}

// workstreamArchivePath returns the path to the workstream archive JSON file.
func workstreamArchivePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".monetdroid", "archived_workstreams.json")
}

// loadArchivedWorkstreams returns the set of archived workstream paths.
func loadArchivedWorkstreams() map[string]bool {
	p := workstreamArchivePath()
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var paths []string
	if json.Unmarshal(data, &paths) != nil {
		return nil
	}
	m := make(map[string]bool, len(paths))
	for _, path := range paths {
		m[path] = true
	}
	return m
}

// ArchiveWorkstream marks a workstream as archived.
func ArchiveWorkstream(wsPath string) error {
	archived := loadArchivedWorkstreams()
	if archived == nil {
		archived = make(map[string]bool)
	}
	archived[wsPath] = true
	return saveArchivedWorkstreams(archived)
}

// UnarchiveWorkstream removes the archived mark from a workstream.
func UnarchiveWorkstream(wsPath string) error {
	archived := loadArchivedWorkstreams()
	if archived == nil {
		return nil
	}
	delete(archived, wsPath)
	return saveArchivedWorkstreams(archived)
}

func saveArchivedWorkstreams(m map[string]bool) error {
	p := workstreamArchivePath()
	if p == "" {
		return fmt.Errorf("cannot determine archive path")
	}
	var paths []string
	for path := range m {
		paths = append(paths, path)
	}
	data, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0644)
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
	archived := loadArchivedWorkstreams()
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
		for i := range ws {
			if archived[ws[i].Path] {
				ws[i].Archived = true
			}
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

	// Build local upstream map: branch → upstream for branches with remote=".".
	upstreamOf := localUpstreamMap(wtPath)

	// Walk from current branch up to defaultBranch to find the root of the stack.
	root := currentBranch
	visited := map[string]bool{root: true}
	for {
		up := upstreamOf[root]
		if up == "" || up == defaultBranch {
			break
		}
		if visited[up] {
			break // cycle guard
		}
		visited[up] = true
		root = up
	}

	// Build children map (inverted upstream).
	childrenOf := map[string][]string{}
	for br, up := range upstreamOf {
		childrenOf[up] = append(childrenOf[up], br)
	}
	// Sort children for stable ordering.
	for _, kids := range childrenOf {
		sort.Strings(kids)
	}

	// DFS from root to collect all branches with depths.
	// DFS ensures children appear directly under their parent in the output,
	// which is required for the tree display to render correctly.
	type entry struct {
		name  string
		depth int
	}
	var ordered []entry
	seen := map[string]bool{}
	var walk func(name string, depth int)
	walk = func(name string, depth int) {
		if seen[name] {
			return
		}
		seen[name] = true
		ordered = append(ordered, entry{name, depth})
		for _, child := range childrenOf[name] {
			walk(child, depth+1)
		}
	}
	walk(root, 0)

	// Check for dirty worktree (applies only to current branch).
	dirty := false
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	if out, err := cmd.Output(); err == nil {
		dirty = len(strings.TrimSpace(string(out))) > 0
	}

	// Build result with status for each branch.
	result := make([]BranchStatus, 0, len(ordered))
	for _, e := range ordered {
		bs := branchStatus(wtPath, e.name, defaultBranch)
		bs.Depth = e.depth
		if e.name == currentBranch {
			bs.Dirty = dirty
		}
		result = append(result, bs)
	}
	return result
}

// localUpstreamMap returns a map of branch → upstream for all local branches
// that have a local upstream (remote = ".").
func localUpstreamMap(cwd string) map[string]string {
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	result := map[string]string{}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name == "" {
			continue
		}
		// Check if remote is "." (local upstream).
		cmd := exec.Command("git", "config", fmt.Sprintf("branch.%s.remote", name))
		cmd.Dir = cwd
		rOut, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(rOut)) != "." {
			continue
		}
		// Get the merge ref.
		cmd = exec.Command("git", "config", fmt.Sprintf("branch.%s.merge", name))
		cmd.Dir = cwd
		mOut, err := cmd.Output()
		if err != nil {
			continue
		}
		upstream := strings.TrimPrefix(strings.TrimSpace(string(mOut)), "refs/heads/")
		if upstream != "" {
			result[name] = upstream
		}
	}
	return result
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
