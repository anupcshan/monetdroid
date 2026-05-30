## KB (Knowledge Base)

This project has a persistent knowledge base accessible via the `kb` CLI.
Run `kb --help` for usage.

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
