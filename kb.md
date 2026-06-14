## KB (Knowledge Base)

This project has a persistent knowledge base accessible via the `kb` CLI.

### When to use it

- **Resuming work.** If the user refers to a project by name, first
  `kb search` or `kb list` to find an existing entry under
  `projects/<slug>.md`, then `kb read` it to recover context before
  doing anything else.
- **Starting new work.** When the user asks you to plan or build
  something new, create `projects/<slug>.md` with a short plan and
  current status. Checkpoint meaningful progress with `kb append`
  or `kb edit` (enough detail that a future session can resume).

### Conventions

- One file per project at `projects/<slug>.md`.
- Entries should include a *Status* section listing what's done and
  what's next, so "resume" calls can pick up where work left off.
- **Keep kb current as work progresses.** When a phase ships, a
  finding changes, or a decision solidifies, update the relevant kb
  documents immediately. Don't wait to be asked. Stale kb is as
  harmful as stale code.
- Use kb to record project plans and progress. Do not use Claude
  Code's built-in plan mode (EnterPlanMode / ExitPlanMode) for
  project tracking; write the plan directly to `projects/<slug>.md`.

### Command reference

Full `kb --help` output:

```
NAME:
   kb - Per-repo knowledge base for Claude sessions

USAGE:
   kb [global options] [command [command options]]

DESCRIPTION:
   A persistent, per-repo store shared across Claude sessions working in this
   repo. Holds plain-text files. No tags, metadata, or structured fields.
   Subdirectories are supported and created automatically on write.

   EXAMPLES:
     List files:                    kb list
     Read a file:                   kb read foo.md
     Read a line range:             kb read foo.md --offset 10 --limit 20
     Search contents:               kb search "some phrase"
     Delete a file:                 kb rm topic/foo.md
     Move/rename:                   kb mv topic/foo.md topic/bar.md

     Write (creates parent dirs). Content on stdin via heredoc:

         kb write topic/foo.md <<'EOF'
         first line
         second line
         EOF

     Append (creates file if missing). Content on stdin via heredoc:

         kb append topic/foo.md <<'EOF'
         another line
         EOF

     Edit a file. First stdin line is the separator (any string not appearing
     on a line by itself in your content); old and new content follow:

         kb edit topic/foo.md <<'EOF'
         ===
         func Foo() {
             return 1
         }
         ===
         func Foo() {
             return 2
         }
         EOF


COMMANDS:
   list     List all tracked files (no filter; use 'search' to find content)
   read     Read a file (optionally --offset N --limit M for a line range)
   edit     Edit a file (stdin: separator, old, separator, new; fails if old is not unique unless --all)
   write    Write a file (content on stdin; creates parent dirs, overwrites)
   append   Append to a file (content on stdin; creates file and parent dirs if needed)
   rm       Delete a file
   mv       Move/rename a file
   search   Search file contents with git grep (basic regex; filenames not matched)
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --help, -h  show help
```
