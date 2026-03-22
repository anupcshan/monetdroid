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
4. Branch upstream set to main (`git branch --set-upstream-to=main <name>`)
5. Session starts in the worktree

For stacked branches (created on top of another workstream branch), upstream points to the parent branch instead of main. This enables topology inference and correct rebase ordering.

### Resume
- Click a session → resume it in its original worktree
- Sessions never change cwd mid-session

### [ask] = Read-only Session
- Starts a read-only session in the workstream's worktree
- Restricted tool set via `--tools "Read,Glob,Grep,WebFetch,WebSearch"`
- Can run concurrent with a read-write session (no file conflicts)
- Visually distinct (dimmed or `?` prefix)

### Archive / Unarchive
- **Archive** = hide from sidebar. Worktree, branches, and sessions all stay on disk. Fully reversible.
- **Unarchive** = bring it back, exactly as it was.
- Archive is the low-stakes "I'm done looking at this" action. No data loss, no warnings needed.
- Blocked if a Claude process is actively running in the workstream.

### Prune
- The destructive cleanup action, separate from archive.
- Removes the worktree directory from disk.
- Deletes branches that are safe to delete (remote tracking branch gone, or no commits ahead of main).
- Warns about branches with unpushed commits (ahead of main, no remote or remote still exists).
- Sessions become non-resumable (cwd gone) but JSONL history remains in `~/.claude/projects/`.
- User confirms with full visibility into what will be deleted and what will be lost.

### Workstream Lifecycle

```
Create → Work → Archive (hide) → Prune (delete)
                    ↕
                 Unarchive
```

## Worktree Pool

- Pre-created worktrees at `~/.monetdroid/worktrees/<repo-basename>/pool-0, pool-1, ...`
- Each checked out at main, ready to go
- On workstream creation: `git worktree move pool-N <name>` + `git checkout -b <name>` (instant)
- On prune: worktree removed via `git worktree remove`
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
12. **Worktree pool**: pre-create, assign on workstream creation
13. **Read-only [ask] sessions**: restricted tool set, visual distinction
14. **Surface worktree path in session header**: pragmatic escape hatch until terminal exists
15. **Set upstream on branch creation**: `--set-upstream-to=main` (or parent branch for stacks)
16. **Workstream status view**: branch topology, ahead/behind, PR status, uncommitted changes
17. **Update main**: fetch origin main (standalone, no sync)
18. **Sync workstream**: rebase stack onto local main
19. **Mass sync**: sync all workstreams, abort+skip on conflict, continue to next
20. **Archive / unarchive**: hide/show workstreams in sidebar, no data deletion
21. **Prune**: delete worktree + merged branches, with safety checks and warnings

## Workstream Status View

Lives as a panel on the home/landing page. The sidebar stays compact (workstream name, running indicator); the home page panel is where the full picture lives.

Per-branch info:
- Ahead/behind main (commit count)
- Ahead/behind remote (pushed or not)
- Remote tracking status (`gone` = PR merged/branch deleted)
- Uncommitted changes (staged/unstaged)
- PR status (future — requires GitHub API, likely via `gh` for auth)

Stack visualization:
- Branch topology inferred from upstream chain
- Visual indicator of where sync broke, if applicable

Actions available from the panel:
- Update main (fetch)
- Sync workstream / mass sync
- Archive / unarchive
- Prune

## Sync

### Update main
- `git fetch origin main` — standalone action, done once
- Separate from sync so it can be done independently
- Subsequent syncs (including retries after fixing conflicts) use the already-fetched local main

### Single workstream sync
1. Walk the upstream chain to find the stack order
2. Rebase each branch onto its upstream, in topological order
3. On conflict: **stop, leave the broken state**, show conflicting files — user resolves manually
4. After resolving, user can re-run sync without fetching again

### Mass sync
1. For each workstream, attempt sync as above
2. On conflict in any branch: `git rebase --abort`, mark workstream as "needs manual sync"
3. Skip remaining branches in that stack (downstream of failure)
4. Continue to next independent workstream

```
✓ auth          synced to main
✗ auth-ui       rebase conflict (2 files) — sync manually
⊘ auth-tests    skipped (depends on auth-ui)
✓ perf          synced to main
```

## Prune

Global action — scans all workstreams and presents everything prunable in one view. User selects what to delete.

### Branch Safety Checks

| State | Remote gone? | Ahead of main? | Action |
|---|---|---|---|
| PR merged, branch deleted | yes | no | Safe to delete |
| PR open, work pushed | no | yes | Warn, leave alone |
| Locally merged into main | no remote | no | Safe to delete |
| Brand new, no work | no remote | no | Safe to delete (harmless) |
| Local work, never pushed | no remote | yes | Warn — unpushed commits |

Note: squash merges can cause false "ahead" counts since original commits aren't reachable from main. This is a known git limitation.

Sessions associated with the workstream are checked regardless of branch state — warn if there are sessions that will become non-resumable.

## Persistence

- Archive state stored as a JSON file (e.g. `~/.monetdroid/workstreams.json`)
- Sqlite is the long-term plan but deferred for now
- Existing `queue.json` and `labels.json` remain as-is for now

## Decisions

- Branches created outside Monetdroid (without upstream set) are not tracked — user manages those manually
- PR status deferred — start with git-only info, add GitHub API later (likely via `gh` CLI for auth)
- Worktree pool deferred — creation is slower without it but not a blocker

## Open Questions

- Pool size defaults and configuration
- Terminal in UI: eliminates need to surface worktree paths
