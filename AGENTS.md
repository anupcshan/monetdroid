## Coding Conventions

- **Zero custom JavaScript** — use HTMX attributes, SSE, and server-side rendering. Only add JS as a last resort.
- **Never use `map[string]any` for JSON parsing** — always define typed structs, even minimal ones. Use custom `UnmarshalJSON` for polymorphic fields.
- **Only facts in code** — comments and log/error messages must state what is observed or required, not guesses at cause. Any speculation must be explicitly marked (e.g. `// SPECULATION:` or `// unconfirmed:`) so readers know to verify before acting on it.

**Never write to auto-memory unless the user says "write to your memory" verbatim.** Do not infer consent from corrections, preferences, or context.

@kb.md
