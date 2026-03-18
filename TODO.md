# Bash Tool Improvements

- [ ] Move permission prompt into the Bash tool chip (no separate box)
- [ ] Auto-scroll bg output div when open and new content arrives
- [ ] Show running/completed indicator on bg output
- [ ] Show elapsed time for bg task execution

# UI Refactor

## Multi-tab is broken

CID is a cookie (per-browser), but SSEClient is per-SSE-connection. Multiple tabs
in the same browser share the CID → same SSEClient → same channel. Events randomly
split between tabs. Closing one tab removes the client from the hub, zombifying the
other.

Different browsers work fine — each gets its own CID, its own SSEClient.
`BroadcastToSession` correctly sends to all clients viewing the same session.

## Remove cookie and implicit session lookup

Half the handlers already get session ID from form values (`/perm`, `/perm-answer`,
`/mode`, `/cancel-queue`, `/archive`). The cookie is only used by handlers that call
`client.ActiveSession()`: `/send`, `/stop`, `/label`, `/label-edit`, `/diff`.

For `/new`, `/switch`, `/load` the cookie is also used to route `ReplaySession` to
the right SSE connection. But these are navigation — they should redirect to
`/?session=XXX` instead. The SSE reconnect on page load already does `BuildReplay`
via the `?session=` query param, so `ReplaySession` becomes unnecessary.

- [ ] `/new`, `/switch`, `/load` — return `HX-Redirect: /?session=XXX` instead of pushing replay through SSE channel
- [ ] Delete `ReplaySession`
- [ ] Generate a random connID per `/events` SSE connection instead of using cookie CID
- [ ] `/send`, `/stop`, `/label`, `/label-edit`, `/diff` — read `session_id` from request instead of `client.ActiveSession()`
- [ ] Pass session ID client-side: OOB-swap a hidden field when session becomes active, include via `hx-include` on POST forms and `hx-vals` on GETs
- [ ] Delete `GetCID` and the cookie

## Route verb/design issues

| Handler | Method | Issue |
|---|---|---|
| `/load` | POST | Navigation, not mutation. Redundant with `/?session=XXX` |
| `/switch` | POST | Navigation, not mutation |
| `/perm` | POST | Overlaps with `/perm-answer` — both look up session+perm channel, send PermResponse |
| `/perm-answer` | POST | Same as `/perm`, different payload shape |
| `/label-edit` | GET | Name suggests mutation but returns the edit form |

## Running indicator gaps

- Home page has no visibility into running sessions
- Header dot only reflects the active session, disappears on navigate to home
- Drawer is the only place showing all running sessions, but must be explicitly opened
- `BuildReplay` sets the running dot but does NOT include the thinking indicator
  — mid-turn reload or late-connect shows dot but no ellipsis until next SSE event
