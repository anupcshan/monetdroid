# KB — Per-repo knowledge base for Claude sessions

## Problem

Session context is ephemeral. Discarding a Claude session loses decisions, research, and open questions. Worktrees make repo-local files impractical for shared state.

## Design

A per-repo knowledge base stored outside any checkout, accessed via a CLI. Sessions become truly ephemeral — all durable state lives in the KB.

### Storage

- One KB (git repo) per code repo, stored under `~/.monetdroid/kb/<repo-identity>/`
- Repo identity derived from `git rev-parse --git-common-dir` — all worktrees resolve to the same KB
- Fallback: `KB_PATH` env var to force a specific path (undocumented)
- Markdown files, browsable with Obsidian or any markdown viewer
- Every write auto-commits (simple commit message, e.g. filename)

### CLI: `kb`

```
kb list                      # list files in this repo's KB
kb read <path>               # read a file
kb edit <path>               # JSON {old, new} on stdin
kb write <path>              # full file on stdin
kb rm <path>                 # delete a file
kb mv <old> <new>            # rename/move
kb search <query>            # search within this KB
```

- No project parameter — repo identity is implicit from cwd
- No init command — reads on non-existent KB return empty, first write creates it
- Claude calls it via Bash, help text is the only "schema"
- `kb edit` accepts JSON `{"old": "...", "new": "..."}` on stdin (same model as Claude Code's Edit tool)

### CLI: `kbadmin`

Separate tool for human administration. Claude never calls this.

```
kbadmin mode [readonly|readwrite]   # toggle write mode
```

### Write control

- Read-only mode: `kbadmin mode readonly` — kb rejects all writes, Claude can still read
- Read-write mode: let the model write freely, review via git log/diff later
- Auto-commit on every write means easy rollback of any individual change
- Version control is the safety net, not per-edit approval

### Discovery

- Claude reads KB proactively when it suspects relevant context exists
- Claude reads KB on request ("check the KB for...")
- INDEX.md at KB root for lightweight scanning (file list + one-line summaries)

### Content

Freeform markdown. The KB is a wiki, not a template system. Documents take whatever shape fits their purpose — feature trackers, migration runbooks, link directories, architecture references, research notes. The model structures each document appropriately.

Only convention: INDEX.md at the root for discovery (also freeform).
