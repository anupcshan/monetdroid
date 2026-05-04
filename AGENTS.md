## Coding Conventions

- **Zero custom JavaScript**: use HTMX attributes, SSE, and server-side rendering. Only add JS as a last resort.
- **Never use `map[string]any` for JSON parsing**: always define typed structs, even minimal ones. Use custom `UnmarshalJSON` for polymorphic fields.
- **Only facts in code**: comments and log/error messages must state what is observed or required, not guesses at cause. Any speculation must be explicitly marked (e.g. `// SPECULATION:` or `// unconfirmed:`) so readers know to verify before acting on it.
- **Don't reference kb in commit messages or code**: kb is not part of the git tree.
- **Commit messages explain WHY, not what**: anything visible in the diff or already commented in code doesn't belong in the message. Keep the body to what the diff can't show.
- **Design rationale lives in one canonical place**: pick a single home (file header, dedicated doc) and keep in-place comments to local mechanics. Don't sprinkle the same explanation across multiple sites.
- **Documentation describes the current state of the code**: not future intent or aspiration. When a change is in flight, update docs alongside the code.

**Never write to auto-memory unless the user says "write to your memory" verbatim.** Do not infer consent from corrections, preferences, or context.

@kb.md
