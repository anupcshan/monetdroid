## Coding Conventions

- **Zero custom JavaScript**: use HTMX attributes, SSE, and server-side rendering. Only add JS as a last resort.
- **Never use `map[string]any` for JSON parsing**: always define typed structs, even minimal ones. Use custom `UnmarshalJSON` for polymorphic fields.
- **Only facts in code**: comments and log/error messages must state what is observed or required, not guesses at cause. Any speculation must be explicitly marked (e.g. `// SPECULATION:` or `// unconfirmed:`) so readers know to verify before acting on it.
- **Don't reference kb in commit messages or code**: kb is not part of the git tree.
- **Commit messages explain WHY, not what**: anything visible in the diff or already commented in code doesn't belong in the message. Keep the body to what the diff can't show.
- **Design rationale lives in one canonical place**: pick a single home (file header, dedicated doc) and keep in-place comments to local mechanics. Don't sprinkle the same explanation across multiple sites.
- **Documentation describes the current state of the code**: not future intent or aspiration. When a change is in flight, update docs alongside the code.
- **Docs and tests describe the invariant, not the fix in progress**: comments, test docs, and assertions must read true after the change lands. State what the code guarantees and why, not the current breakage or the candidate fixes under consideration. "X targets a missing element" describes today's bug and misleads once fixed; "X must target an element that exists" describes the contract and stays accurate. Pin behavior so a test passes under any correct implementation.

## Running tests

Run tests with `go tool mdrdev test`:

  go tool mdrdev test ./... -count=1 -timeout 600s

Invoke it bare: no pipes, redirects, or shell expansions. Raw `go test` and
`go run` are denied.

Record integration cassettes with:

  go tool mdrdev record-cassette ./test/integration/ -run TestFoo

Cassettes are recorded API request/response streams, not Claude Code logs or UI
state. To debug behavior, read the screenshots (`*.png`) and DOM snapshots
(`*.html`) the test writes to `test/integration/testdata/screenshots/`, not the
cassettes.

Don't preflight-check for Docker or other prerequisites before running tests or
recording; the test itself reports what's missing. Checking beforehand adds
noise and guesses at causes.

**Never write to auto-memory unless the user says "write to your memory" verbatim.** Do not infer consent from corrections, preferences, or context.

@kb.md
