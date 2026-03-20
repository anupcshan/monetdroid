# Workstreams Plan

## End State Vision

Monetdroid manages parallel streams of work on the same repo. Users think in terms of branches and workstreams, not filesystem paths. Worktrees are an implementation detail Monetdroid fully owns.

## Concepts

- **Project** = git repo, identified by `git-common-dir`, groups all worktrees
- **Workstream** = a branch (+ any child branches) with its own worktree, managed by Monetdroid
- **Session** = a Claude conversation within a workstream
- One active session per workstream, older sessions collapsed
- Branch chips show which branches a session touched (from `gitBranch` in JSONL)

## Sidebar Layout

```
monetdroid                                              [+]

  main
    "fix SSE race"                                       1h ago
    ▸ 2 older sessions                                  [ask]

  auth-tests · auth-ui · auth
    "add permission checks"             ● running        2m ago
    ▸ 1 older session                                   [ask]

  perf
    "optimize SSE broadcast"                             3h ago
                                                        [ask]
```

## User Flows

### "+" = New Workstream
1. User clicks "+"
2. Enters a name (e.g. "auth-refactor")
3. Monetdroid creates branch off main + assigns a worktree from pool
4. Session starts in the worktree

### Resume
- Click a session → resume it in its original worktree
- Sessions never change cwd mid-session

### [ask] = Read-only Session
- Starts a read-only session in the workstream's worktree
- Restricted tool set via `--tools "Read,Glob,Grep,WebFetch,WebSearch"`
- Can run concurrent with a read-write session (no file conflicts)
- Visually distinct (dimmed or `?` prefix)

### Archive
- User archives a workstream → worktree returned to pool, cleaned up

## Worktree Pool

- Pre-created worktrees at `~/.monetdroid/worktrees/<repo-basename>/pool-0, pool-1, ...`
- Each checked out at main, ready to go
- On workstream creation: `git worktree move pool-N <name>` + `git checkout -b <name>` (instant)
- On archive: `git checkout main && git clean -fd && git reset --hard` → rename back to pool-N
- Pool topped up in background when running low
- Pool size: start with 1-2, configurable later

## Worktree Path Layout

```
~/.monetdroid/worktrees/
  monetdroid/
    auth-refactor/     ← assigned workstream
    perf-fixes/        ← assigned workstream
    pool-0/            ← free, checked out at main
  tscloudvpn/
    pool-0/
```

## Session Resume Constraints

- Claude CLI `-p --resume <id>` only searches `~/.claude/projects/<mangled-cmd.Dir>/`
- No worktree-aware lookup in `-p` mode (known CLI gap)
- **Workaround**: always start claude process in the session's original cwd (first `cwd` entry in JSONL)
- **Rule**: sessions must stick to one worktree — never change cwd mid-session
- We only manage worktrees we create; don't mess with user-created worktrees

## Build Order (Completed ✓ / Remaining)

### Done
1. ✓ Parse `gitBranch` from JSONL, show branch chips on all session views
2. ✓ Group history by `git-common-dir` (merge worktrees of same repo)
3. ✓ Fix cwd to use first JSONL entry (session identity)
4. ✓ Git helpers: `MainWorktree`, `GitDefaultBranch`, `WorktreeDir`, `CreateWorkstream`
5. ✓ `POST /new-workstream` handler
6. ✓ Drawer "+" opens per-group workstream popover; header popover flipped (workstream primary)
7. ✓ Test infra: in-container server, HTTP file I/O, `DockerExec` for git
8. ✓ Workstream integration test: create from drawer, verify branch, drawer grouping, landing page
9. ✓ `MainWorktree()` resolves worktree paths to main repo root in drawer + landing page
10. ✓ History group Dir uses `filepath.Dir(git-common-dir)` for stable repo-root labels

### Remaining
11. **Sub-group by worktree in sidebar**: workstream sections, collapse older sessions
12. **Worktree pool**: pre-create, assign on workstream creation, reclaim on archive
13. **Read-only [ask] sessions**: restricted tool set, visual distinction
14. **Surface worktree path in session header**: pragmatic escape hatch until terminal exists
15. **Worktree cleanup**: garbage collect for merged/deleted branches

## Open Questions

- Pool size defaults and configuration
- Stack visualization: showing branch topology for PR stacks (future)
- Terminal in UI: eliminates need to surface worktree paths
