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

- `make fmt` — `gofmt -w`.
- `make fmt-check` — verify formatting without changing files.
- `make build` — the binary.
- `make test` — the full gate: formatting, module verification, vet, workflowcheck, build and all
  tests with `-race`. Loads `.env`; requires DB, Node, Chrome/Chromium and
  `MEDIAD_TEST_QBIT_URL`. Missing dependencies fail instead of silently
  skipping an integration suite. `MEDIAD_REQUIRE_REPLAY=1` is set automatically.
- `make test-db`, `make check`, `make check-ci` — aliases for `make test`,
  without duplicate test runs. The DB uses explicit `MEDIAD_TEST_DSN`, falling
  back to `MEDIAD_POSTGRES_DSN`; the role needs `CREATEDB` for throwaway DBs.
- `make check-commit` — hook gate: the same static, DB, replay and browser
  checks; qBittorrent live tests run only when configured. Missing DB/browser
  prerequisites still fail. The pre-commit hook calls this target.
- `make test-db-setup` — start local PostgreSQL and prepare its application role.
- Direct `go test ./...` — the cheap loop; integrations still skip when their
  environment is absent. Do not report it as the full gate.
- Chrome is detected on PATH or at its standard macOS app path;
  `MEDIAD_TEST_CHROME` overrides it. Live qBittorrent credentials use
  `MEDIAD_TEST_QBIT_API_KEY` or `MEDIAD_TEST_QBIT_USERNAME` / `MEDIAD_TEST_QBIT_PASSWORD`.
  Two read tests skip on an empty library; the synthetic stopped-torrent test
  adds/removes only its test torrent, with files kept.
- Replay uses committed histories without Temporal. Recapturing a missing
  history requires reachable Temporal; a missing corpus fails `make test`.

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
  torrentfile/         .torrent parser: info hash + trackers → magnet URI
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

### A form inside a form does not exist

HTML has no nested forms. The parser drops the inner `<form>` element,
and its controls — a submit button above all — become part of the outer
form. The button then submits the **outer** form: a cancel button
inside a GET filter form reloads the page and cancels nothing, and the
button's own `action` is nowhere in the DOM.

The dashboard had one filter form around the whole page, and the active
table's cancel form sat inside it. The source looked right, the rendered
HTML looked right, and only the parsed DOM showed the difference: two
forms where the template had three.

**How to avoid:** a form is a top-level element. When a page needs a
filter form *and* an action form, they are siblings — the dashboard now
has one GET form per filter row, each mirroring the other row's
controls as hidden inputs so the query string still round-trips.

`internal/web/render_test.go` checks this on every page:
`TestNoPageNestsAForm` reads the rendered source with the tokenizer
(the parser is what hides the problem), `TestCancelButtonsPostToTheCancelRoute`
checks that the cancel button is owned by a POST form pointing at
`/cancel`, and `TestDashboardFilterFormsCarryEveryControl` checks that
both filter forms still carry every filter control.

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

### The torrent client's add is idempotent by tag

`qbit.Add` looks for a torrent carrying the request's tag (`mediadbot:<id>`)
**before** it adds anything. If one is there and it is not in a failure state,
the add is a no-op and the *old* torrent's hash comes back.

That is what makes an add safe to retry. It is also why **replacing** a
torrent means deleting the old one first — `internal/request/workflow.go`'s
season watcher deletes the old torrent (without its files, so the episodes
already on disk survive and the new pack reuses them) and only then adds the
update. Add first and the update silently never lands.

The delete has to be checked too: if it fails, the add adopts the old
torrent, and the request would believe it had downloaded the update.

### The season watcher

A request for a season that is still being released gets a pack of the
episodes that exist so far. The request does not treat that as the season: it
compares the pack with the season (`internal/release/season.go`), and while
the tracker might still complete it, it waits for the update — the release
from the same forum thread with more episodes in it.

The pieces, in the order they matter:

- `internal/release/parse.go` — `Parsed.Episodes` is the range of episodes a
  release name claims (`S02E01-E08`, `1-8 из 10`, `1x01-1x08`). It is also
  what tells a pack of eight episodes from a single one: without it
  `S02E01-E08` is read as episode 1 and refused as "a single episode".
- `internal/release/season.go` — `SeasonState` (is the season still
  running, is the pack short of it, might the tracker still complete it),
  `Tracked` and `IsUpdateOf` (is this release the tracked pack with more
  episodes in it).
- `internal/request/workflow.go` — `runOngoingCheck` decides whether to
  monitor, `monitorCycle` looks for the update, `waitForSeason` is where a
  finished partial pack goes instead of finishing the request.
- `storage.StateOngoing` is "waiting for the rest of the season". It is in
  `ActiveStates()` (a request nobody can see is a request nobody can cancel)
  and in the `ux_requests_active` index.

Three things about it are easy to get wrong:

- **The count an update is compared against comes from the pack's name**, not
  from the torrent client's file count. The update's count is read from its
  name too, and comparing a name against a file count lets a pack be
  recognised as an update of itself — the watcher would delete it and add it
  again, for as long as the season lasts.
- **The files of a season pack are not deleted** when the torrent goes: not
  on a cancel, not on a stall. The pack repeats the episodes that are already
  in Plex, and deleting its files would take them off the disk.
- **The three season columns** (`selected_thread_id`,
  `selected_video_count`, `selected_episode_count`) are written by
  `saveState` on every state change. They used to be dropped on the floor,
  which left the interface showing nothing about a request that was waiting.

### No seeders is not the same as unknown seeders

`internal/indexer/prowlarr.go` writes `-1` when the indexer reports a null
count, and the interface shows «—» for it — a release with no seeders is
dead, and a release whose count was never reported is merely unmeasured.

The zero value of a `storage.Candidate` is therefore **zero seeders**, not
unknown ones. A fixture that means "unknown" has to say `-1` explicitly, and
a real release with no seeders must not be shown as «—».

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

### The administrator's scope is «Все», and cannot be narrowed

`internal/web/handlers.go` (`applyScope`). The dashboard carries a
scope: «все» — every request in the installation — and «мои», the
requests the reader is watching (`RequestFilter.WatcherUserID`).

**An administrator is pinned to «Все»** and is offered no control at
all: the requests nobody is watching are exactly the ones an operator
has to notice, and a control that could hide them is a control somebody
would eventually leave narrowed. Everybody else starts on «Мои» and can
widen it in one click — the installation is shared, and that is
exactly the case where the wider view is useful.

The copy is «Мои», not «Запрошенные мной»: a request has no owner, and
the creator is only a proxy for it (people attach themselves to
requests). The polled fragment (`/fragments/active`) has to carry the
scope in its query, or it falls back to the reader's default and
quietly narrows the table back.

### Cancelling deletes files — the confirmation says so

`GET /requests/{id}/cancel` states the consequence, `POST` does it.
The consequence is **state-dependent**, and the code is the only
authority on it (`internal/request/workflow.go:1467`):

| State | Files on disk |
|---|---|
| `downloading` | **deleted** — the download was never filed into the library |
| `ongoing` (watching for the rest of a season) | **kept** — those episodes are already in Plex |
| anything else | nothing to delete |

An earlier draft of the design plan claimed the opposite in four
places. It was wrong in the direction that costs the user their files.
The copy is generated from the request's state, never hard-coded.

### The dashboard's table holds no form

The cancel action is a **link** to the confirmation page. This is not
cosmetic: the filter forms and the table were siblings, and a form
inside the table was the form-in-form bug from `b57d037`. With the
action as a link, the table carries no form at all.

### Tables become cards when the content column is narrow

`table.table-cards` in `internal/web/views/static/style.css`. One DOM, two
renderings: `data-label` on every cell becomes the field's label, and
the explicit `role="table"` / `role="row"` / `role="cell"` /
`role="columnheader"` keep the table semantics that `display: block`
takes away in Safari and VoiceOver. All data tables use this pattern,
including requests, notifications and settings. Phase 3 added the events table, rendered as a vertical timeline
on phones. Table/card mode follows the available width of `main` through
`@container (max-width: 60rem)`, so an open sidebar also gets a layout that fits.

**The class is `table-cards`, not `cards`.** `.cards` is already the
search-results grid (`display: grid`), and a table that inherits it turns
into a grid of cells. The two were confused once.

### The release facts are read out of the name, not stored

`internal/web/candidates.go`. A candidate row holds the title and the
indexer, and `release.Parse` + `release.DetectAudio` are pure functions
of exactly those two — so the page re-derives the resolution, the source,
the audio tier, the studio and the deciding marker with **the selector's
own rules**.

A stored copy of those facts would be a second copy of the rules, and the
two copies would drift. The plan called this "a storage change" and it was
not one.

### The reason's numbers belong to the release, not to the policy

The reason cell says «мало сидеров: 2» — the release's own count. The
threshold it missed is the *policy's*, and the policy is on the same page,
in «Требования». Storing the threshold with the candidate would be a
second copy of the rule, free to disagree with the first the moment the
rule changed — a release refused under "three seeders" would go on
claiming it was refused under "five".

### The candidate list is ordered best first

`internal/release/score.go` scores the releases it refuses, so the menu a
person picks from has its best item at the top. Before this, the refusals
were in the indexer's order — a refused release never reached the scorer —
and the rank said nothing about the quality. `storage.Candidate.Rank`'s
comment claimed "selector-score order" and the code did not do it.

### The dashboard is three buckets, and the poll swaps out of band

`internal/web/views.go` (`dashboardBuckets`), `internal/web/views/fragments.html`.

The buckets are split by **how long a thing takes**, not by workflow phase:
«Качается» (transfers), «Ищет» (parked requests), «Ждёт выбора» (the ones
that need a person). A request searching for four months and a download at
90% are different products.

Two things that are easy to break:

- **One query per bucket.** A single query with a limit lets a busy bucket
  starve a quiet one — fifty downloads in progress and nothing from the
  parking list.
- **The poll swaps out of band.** `/fragments/active` returns the three
  tables and the three counts with `hx-swap-oob="true"`, and the requester
  carries `hx-swap="none"`. The blocks stay on the page, so a poll never
  replaces a `<details>` the reader has collapsed. A renamed id breaks this
  *silently* — htmx drops an out-of-band element whose target is missing —
  which is why `TestTheFragmentCarriesTheBucketsOutOfBand` checks the ids
  in both directions.

The period filter («за сутки» and friends) applies to «Недавно» only. It
used to apply to the active list too, which made a request that had been
searching for four months disappear.

### The requester's name is a column, not an inference

`requests.requested_by` (migration `00012`) holds the id of the account
that asked for the request.

It is a column because the watcher list cannot answer the question:
anybody may attach themselves to a request, so the people who *watch* it
are not the people who *asked* for it. The create path writes no event at
all, so there was nothing to promote out of the event log either.

Two things to know when you touch it:

- **The projection view is frozen at creation.** `request_view` was last
  rebuilt by `00010`; a migration that adds a column to `requests` and
  forgets the view leaves the column invisible to every reader. The reader
  selects by *name*, so the failure is a scan error, not a wrong value.
- **A new migration must keep the three ongoing-season columns** in the
  view (`selected_thread_id`, `selected_video_count`,
  `selected_episode_count`) — 00010 added them and 00012 has to carry
  them forward.

### The theme is a browser preference

`localStorage["mediad.theme"]`, applied as `data-theme` on `<html>` before
the first paint, from the script in `layout.html`.

**«Auto» is the absence of the attribute**, not a third value of it: Pico
reads `data-theme` when it is there and falls back to `prefers-color-scheme`
when it is not. The picker in the settings therefore *removes* the
attribute for «Как в системе».

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
