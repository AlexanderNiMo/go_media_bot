# AGENTS.md

Project knowledge for AI agents (and humans) working on this codebase.
Static facts, conventions, pitfalls, UX decisions, and the user's
collaboration style. The `README.md` is the user-facing overview; this
file is everything else an agent needs to be useful here.

---

## What this is

`mediad` is a self-hosted media-request bot. You name a film or a series
season, it checks Plex, hunts for a Russian-audio release on trackers,
hands it to qBittorrent, and tells you when it is done. The horizon is
two years: a request that has nothing to match against is parked and
re-checked on a backoff schedule. Single Go module, single binary at
`cmd/mediad`, three front-ends (Telegram bot, web UI, CLI) over one core.

---

## Build & test

The Makefile is the source of truth. The layers, top to bottom:

- `make fmt` — `gofmt -w` and `go mod tidy`.
- `make build` — the binary.
- `make test` — unit tests. They skip cleanly without env, so this is
  the cheap local loop.
- `make test-db` — DB-backed tests, gated on `MEDIAD_TEST_DSN`. The
  `mediad` role needs `CREATEDB`; tests create and drop their own
  throwaway databases.
- `make check-ci` — what CI runs. Adds `workflowcheck` (replay suite)
  and live qBittorrent tests gated on `MEDIAD_TEST_QBIT_URL` /
  `MEDIAD_TEST_QBIT_API_KEY`.
- `MEDIAD_REQUIRE_REPLAY=1` — turns the replay-corpus-coverage skip
  into a hard failure. Use it on a developer machine with Temporal
  reachable; CI sets it.

When the user says "tests failed", ask which target they ran — the
failure mode differs (`go test ./...` skips DB tests silently,
`make check-ci` does not).

---

## Codebase layout

```
cmd/mediad/            one binary, subcommands: version, config, migrate,
                       health, worker, api, bot, all, plus CLI: search,
                       add, list, status, cancel, retry, notifications
internal/
  config/              defaults; config_dev.yaml is the per-machine override
  domain/              MediaSpec, MediaRef, state machine
  release/             pure scoring & filtering (no I/O, no clock) — the
                       table-driven tests here are the spec for "is this
                       release acceptable"
  indexer/             Prowlarr + Jackett adapters
  plex/                TMDB-and-Plex matchers
  tmdb/                client
  qbit/                qBittorrent client
  storage/             pgx pool, migrations, projection view
  request/             Temporal workflow + activities + replay corpus
  temporalx/           fx options for Temporal connectivity
  web/                 HTTP server, html/template views, CSRF, sessions
    views/             templates, one file per page; layout.html + fragments
                       are parsed with every page
    views/static/      pico.min.css, htmx.min.js, style.css, favicon.svg
  telegram/            bot
  prompt/, logging/    small helpers
scripts/               capture-histories.sh and friends
test_dockers/          one-shot docker fixtures for qbit
```

DI is `go.uber.org/fx`. Long-running modes (`worker`, `api`, `all`,
`bot`) share `graph(...)` in `cmd/mediad/main.go`; connectivity
checks (`temporalConnectivity`, `qbitConnectivity`) are layered as
separate fx options per command.

---

## Migrations

`internal/storage/migrations/*.sql` is embedded via `//go:embed
migrations/*.sql` and run by `goose.NewProvider.Up`.

**Both `00004_manual_release.sql` and `00005_request_view_columns.sql`
are idempotent (use `IF NOT EXISTS`)** because `goose.NewProvider.Up`
replays already-applied statements under some configurations. New
migration files should follow the same pattern.

If `goose_db_version` after a manual fix-up is already at version 5,
the projection view (frozen at creation) is the likely cause — new
columns on `requests` are invisible until the view is recreated.

---

## Pitfalls (real bugs the project has hit)

### Nil dereferences hang instead of crashing

On this machine (macOS 27, Go 1.27.2), `var p *T; _ = p.Field` does
not print a runtime panic — the process stops and sits there until
something times out. The stack trace points at the function's opening
brace, the goroutine is `[runnable]`, it looks like an infinite loop.

**How to avoid:** in production code that calls something returning
`(value, error)`, check the pointer and return an error naming the
empty response. Do not let a nil pointer reach field access. In
tests, make stubs that cannot return nil with a nil error.

### `path` template helper does not interpolate nested `{{...}}`

`internal/web/render.go`'s `{{path base route}}` helper takes both
arguments by value, so `{{path $.BasePath "/requests/{{.ID}}"}}` does
**not** interpolate the inner `{{.ID}}` — the whole thing is the
literal route string and gets URL-escaped as `%7B%7B.ID%7D%7D`.

**Fix:** concat as separate template actions:
```
{{path $.BasePath "/requests/"}}{{.ID}}
{{path $.BasePath "/requests/"}}{{.ID}}/cancel
{{path $.BasePath "/add/tv/"}}{{$.Show.TMDBID}}/{{.Number}}
```

Inside `range`/`with`, the inner scope hides the page, so use `$.ID`
on the row. `TestRequestPagesRenderPopulated` catches most of this,
but new `{{path … "…{{…}}…"}}` patterns still need manual review.

### Replay tests fail with TMPRL1100 after a workflow change

`internal/request/replay_test.go` replays every history under
`internal/request/testdata/histories/` against the **current** workflow
code. When the workflow scheduled a different set of activities, the
replayer reports `TMPRL1100 lookup failed for scheduledEventID to
activityID`. The test message blames the offset, not the cause.

**Fix:** recapture. The capture command is
`./scripts/capture-histories.sh <workflow-id>…` and needs a live
`docker compose up` Temporal at 7233. After a workflow change,
re-capture both `download-watcher` and one `movie-XXXX` history.

`TestReplayCorpusCoversEveryWorkflowType` skips when a history is
missing unless `MEDIAD_REQUIRE_REPLAY` is set. Capture needs Temporal,
which a developer without one cannot rebuild.

### qBittorrent runs on 8113, not 8080

Default in the codebase is `8080`. The live deployment is on `:8113`.
A "doesn't work in dev" complaint is almost always this port. The
Prowlarr indexer in this deployment is exactly one — `RuTracker.org`
(id=1) — and is enabled on demand; sessions that touch
`internal/indexer` or run the live CLI against Prowlarr should expect
the "no indexer" empty state until re-enabled.

---

## Live infrastructure (no secrets here)

Real services run on the home network at `192.168.1.107`. **Credentials
and tokens live in `.env` and `.zed/debug.json` (both gitignored) and
in `config_dev.yaml` (also gitignored).** Do not paste them into code,
commits, or this file.

| Service     | Port (live) | Codebase default | Where the dev value is  |
| ----------- | ----------- | ---------------- | ----------------------- |
| Plex        | 32400       | 32400            | `.env` → `PLEX_URL`     |
| Prowlarr    | 9696        | 9696             | `.env` → `PROWLARR_URL` |
| qBittorrent | **8113**    | 8080             | `.env` → `QBIT_URL`     |
| Jackett     | 9118        | 9118             | `.env` → `JACKETT_URL`  |

The classifier blocks Bash commands that interpolate these values
into a command line, so the safe path is "fix the URL in code" rather
than "run a live test against my real service".

Workflow IDs are namespaced per deployment: `movie-1280738` on this
network is «Живая ярость» (Huo zhe yan, 2025), not Matrix
Resurrections.

---

## UX decisions (recorded so they don't get re-litigated)

### The web sidebar is collapsible to icons, not fully hidden

`internal/web/views/layout.html` and `internal/web/views/static/style.css`.
Two states:

- `data-sidebar="open"` — 16 rem column, full labels.
- `data-sidebar="icons"` — 3.5 rem column, only SVG icons.

**Default on first visit is `icons`.** Mobile (≤768 px) keeps the
drawer-over-content behaviour: the `icons` state slides the drawer
off-screen, `open` slides it in. Persisted in `localStorage` under
`mediad.sidebar`; legacy `'closed'` values are silently replaced.

In `icons` mode, the settings sub-nav (`nav.sub`) is hidden entirely
— the page itself has its own tabs at `settings.html:4-12`, so the
duplication is not a loss. `aria-hidden` on the sidebar follows
visual availability (off-screen on mobile in `icons` ⇒ hidden, visible
on desktop ⇒ not hidden), not raw state.

The hamburger glyph (☰ / ✕) is rendered via CSS `::before { content }`,
so the toggle does not need a JS touch to update its icon.

### No further "make the navigation discoverable" proposals

The original sidebar was `data-sidebar="closed"` by default and the
hamburger was a 1.3 rem muted glyph. New users landed on an empty
dashboard with the only visible affordance being the brand. This was
caught and fixed; do not regress to a hidden default.

---

## Working with the user

The user steers in short, focused turns. Patterns that work:

- **Diagnose first, propose, then implement.** When the request is
  "X is broken" or "assess Y", produce a short diagnosis (one
  paragraph, root cause, file:line) before any change. They will
  accept or correct the diagnosis. Do not leap to the change.
- **Ask before implementing.** After the diagnosis and the
  proposed change, ask. "Implement this?" is a one-line question
  they will answer.
- **One fix at a time, then ask.** They commit only after they say
  "ready", and they commit test files separately from the
  implementation. Do not commit before they ask.
- **Narrow tests.** When a test command is running, prefer a single
  package or a `-run` filter over the full suite. A failing test
  that takes the whole suite down is a bigger problem than one that
  reports quickly.
- **They name their own stack.** Prowlarr on 192.168.1.107,
  qBittorrent on 8113, Jackett on 9118, the live TMDB key. When you
  don't have one, ask before guessing.
- **They notice design regressions.** UX feedback in their voice
  ("Главная навигация спрятана") is the kind of signal the
  codebase cannot produce on its own. Treat it as a real signal
  even if the code "works" — they are the one who has to live
  with it.
- **Use `interactive-input` for multi-choice questions, with a probe.**
  Wrap options in a ```` ```interactive ```` block (`Input.ChoiceSet`)
  rather than a numbered list when the choice set is enumerable. The
  skill was made available mid-session; load it with
  `skill_view(name='interactive-input')` and see `references/schema.md`
  for `TextBlock`, `Input.Text/Number/Toggle`, `Separator`, and
  `Action.Submit`. **Probe once whether the current client renders the
  block as native UI or falls back to a JSON code block.** If the user
  reports the latter, fall back to enumerated prose
  ("1) … 2) … 3) …") and accept text answers of the same shape — that
  is the form that has worked in this project. Do not assume the
  client supports the rich UI without checking.
