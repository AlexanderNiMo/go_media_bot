# mediad

Self-hosted media request bot. You name a film or a series season, it searches
TMDB, checks whether Plex already has it, and if not, keeps looking for a
good-quality release with Russian audio until one turns up — then hands it to
qBittorrent and tells you when it is done.

The point is the horizon. A request lives for **two years** and is re-checked on
a backoff schedule, so you can ask for something that does not exist yet and
forget about it. "A proper Russian dub finally appeared" is an event that can
arrive months later; nobody wants to watch for it by hand.

Three interfaces over one core: a Telegram bot, a web UI, and a CLI.

## Status

Under construction, being built in stages:

| Stage | What | State |
|---|---|---|
| 0 | Skeleton: config, logging, compose, Temporal connectivity | done |
| 1 | Database: pool, migrations, schema | done |
| 2 | Clients: TMDB, Plex, indexer, qBittorrent | done |
| 3 | Release selection: quality and Russian-audio scoring | done |
| 4 | Workflow, activities, task queues | done |
| 5 | Download watcher | done |
| 6 | Determinism checks in CI | done |
| 7 | CLI commands | done |
| 8 | Web UI | done |
| 9 | Telegram bot | done |
| 10 | Packaging, backups, alerts | next |

## Choosing a release

This is where the project will attract its bug reports — "why did it grab a cam
rip", "why did it skip a perfectly good release" — so `internal/release` is
pure functions with no I/O and no clock, and the tests are tables of real
release names. A pure function is the only kind that can be pinned down.

### Russian audio is a ranking, not a flag

"Has Russian audio" is not the interesting question. "Is this a proper dub or a
single-voice amateur reading" is, and the difference is obvious within thirty
seconds of pressing play. So audio is a tier:

```
dub > multi-voice > two-voice > single-voice > voice-over > subtitles only > none
```

**The case the whole detector exists for is subtitles.** A release named
`The.Matrix.1999.1080p | ENG + Rus subs` satisfies any check that asks "does
the name mention Russian", while being exactly what the user did not ask for.
A release carrying Russian subtitles *and* Russian audio is fine and common —
the markers only matter when no audio marker was found.

### The order of the checks is the substance

Studios first, because a credited studio is the strongest evidence a name
carries: a release attributed to LostFilm has LostFilm's audio, whatever else
the name says or omits. Then tier markers, most specific first — **the
multi-voice check has to run before the voice-over one**, or "Многоголосый
закадровый" is read as an amateur reading and ranked below one.

Then subtitles, and then the tracker. An unmarked release from RuTracker is
accepted with a penalty, because plenty of releases there are described only by
their technical parameters and refusing all of them would make the project
useless on exactly the trackers it targets.

That last rule is the risky one, and it is bounded by the same logic pointing
the other way: a release credited to an English scene group (`-RARBG`) has
English audio, so the tracker's nationality does not help. English groups are
only consulted when the name carries no Russian marker, which makes the cost of
a wrong entry a missed release rather than a wrong download.

### Audio outweighs resolution

Deliberately. A dub at the floor resolution is a better outcome for someone
asking for a film in Russian than a 4K release they cannot follow.

The minimum resolution also scores 0.5 rather than 0. That was a real bug found
by a test: scoring the floor at zero turned the resolution weight into a cliff
that overrode everything else, so a 4K release with an amateur voice-over beat
a 1080p one with a proper dub. The floor is what the user said they will
*accept*, not what they consider worthless.

### Hard filters versus scoring

Filters reject; scoring ranks. Rejected: cam/telesync/screener sources, a
resolution below the configured floor, too few seeders, a size outside the
configured range, an implausible size for the claimed resolution, a
blacklisted release, a wrong season, a single episode when a season was asked
for, and a clearly wrong year.

Two of those deserve a note:

- **A single episode is not a season.** An `S02E05` release is a tenth of what
  was asked for; taking it would fill the download slot and leave the request
  unfulfilled. A season pack that does not name its season is accepted — the
  indexer was asked for one specifically.
- **Seeders reported as unknown (-1) are not zero.** Prowlarr returns null for
  some indexers, and treating that as zero would make those indexers look
  permanently empty.

Every rejection carries a machine-readable reason, recorded in the event log.
That is what makes "there were results, why did nothing download" answerable
without reproducing anything.

## The workflow

One workflow is one film or one season of a series. There is no "the whole
series" request by design: a flat model keeps the workflow, the deduplication
key and the UI card all describing the same thing.

```
Plex check ─→ in the library? → done, skipped
     │
     └─→ loop
          wait until the next attempt is due     ← interruptible, so a cancel is instant
          search the indexer on a rate-limited queue
          score and pick a release               ← pure logic, an activity
          Plex check again                       ← cheap insurance before an irreversible step
          add to the client on its own queue
          poll the download every five minutes
          rescan Plex, notify the watchers
```

### What lives where, and why

The workflow contains orchestration and nothing else. Scoring, parsing and
delay computation live in activities or in the packages they call.

That is not tidiness. Editing logic *inside* a workflow breaks the replay of
every execution already running, and these run for two years. Worse, the
failure **looks like a hang rather than an error**: the default
`WorkflowPanicPolicy` is `BlockWorkflow`, so the task fails once and is then
retried forever. The workflow never completes and never appears in a list of
failures — with a two-year execution timeout, that means two years of silence.
Release selection is exactly the code that will change most often, so it is an
activity.

### The workflow never touches the torrent client

It polls its own row in the projection, which a separate download watcher keeps
current. That gap is what makes a two-day download affordable: the interface
needs progress every thirty seconds, but the workflow only needs to know
done / stalled / expired, and polling a workflow at the progress cadence would
cost about fifty thousand history events — the termination limit — for a single
download.

It also means exactly one caller talks to qBittorrent, which matters because
its authentication is a session cookie with server-side invalidation and an IP
ban after repeated failed logins.

### The download watcher

A singleton workflow with a fixed ID, started by the worker and kept running
for the lifetime of the deployment. Each iteration runs one activity that lasts
an hour and polls the client every thirty seconds inside itself, so the fast
polling costs no history at all — the workflow sees roughly twenty-four events
a day.

It re-reads the list of active downloads on every cycle rather than being
handed one, which is what means a torrent added a second after the last poll is
picked up on the next one.

**The watcher writes facts and decides nothing.** How far along a download is,
what the client calls it, when it stopped moving — those go into the
projection. Every decision about what a fact *means* lives in one pure
function, `interpretDownload`, and that split is deliberate: the three
interesting cases differ in a way that is easy to get wrong.

| The client says | The release is | Why |
|---|---|---|
| `error`, `missingFiles` | blacklisted | it failed for a reason that will repeat |
| stalled or stopped, past the window | blacklisted | it is not going to finish at a usable rate |
| absent, having made progress | blacklisted | it got partway and died |
| absent, never having started | **not** blacklisted | that is the infrastructure's fault, and remembering it would let a client restart permanently poison good releases |

The last row is the one worth stating out loud. A torrent that vanishes without
ever starting usually means the client restarted with a wiped session, or the
add raced something — and blacklisting every release that was in flight when
that happened would quietly remove perfectly good releases from consideration
forever.

Separately, the workflow that asks for a release and the watcher that reports
on it write to the same row, so the columns are divided by ownership: the
workflow owns the process — phase, schedule, which release was chosen — and the
watcher owns what the client reported. `UpdateState` deliberately does not
write progress, because the workflow never sets it and doing so would have
reset an in-flight download's progress to zero on every state change.

### Task queues are the rate limit

One worker per queue, each with its own limits. The indexer queue is capped
server-side, which is what distinguishes it from a per-process limiter: the
limit holds across every replica.

**The limit is set below the real target.** Prowlarr fans every request out
across all configured trackers, and the server spreads activity tasks over
several partitions of a queue and can burst above the configured average. A
limit set exactly at a tracker's tolerance will occasionally exceed it, and the
consequence of that is a banned IP rather than a slow request.

Every activity names its queue explicitly. An activity scheduled without one
runs on the workflow's queue, silently bypassing the limit that queue exists to
enforce.

### What the failing tests found

Four real bugs, three of which were invisible until the tests actually
exercised the code:

- **A cancellation arriving while an activity was in flight was treated as a
  failure.** The activity is cancelled too, and its error looks like any other.
  The workflow then tried to write state with a context that was already done,
  that write failed, and the failure propagated out — so a user who cancelled
  got an error instead of a clean outcome.
- **The download deadline was recorded but never checked.** A torrent that ran
  for two days while still reporting progress was waited on indefinitely.
- **A stopped download was treated as still running.** A real library had one
  sitting at 98.6%, and the download loop would have waited out the full
  two-day deadline for something that was not moving. This project only ever
  adds torrents running, so a stopped one is a state nobody asked for; it now
  takes the ordinary stall path — abandon, blacklist, search again.
- **`LooksLikeVersion` did not exist**, and this one was found by accident
  rather than by a test: pointed at port 8080, which on the development machine
  is an unrelated web service, the client logged a successful connection and
  read an HTML page as the torrent client's version. The add-option name
  depends on the major version, so an unrecognised string silently falls back
  to the 4.x spelling and a torrent starts downloading when it was meant to
  wait. Startup now refuses a service that is not qBittorrent.
- Two test bugs that mattered for the same reason: a catch-all mock registered
  before a specific one silently won (testify matches the first), and a poll
  counter shared across download attempts made the second release stall too.

One thing to know when extending these tests: **mocking an activity by method
expression does not deliver its arguments.** `OnActivity((*Activities).Foo, …)`
hands the mock closure a zero value, while `OnActivity("Foo", …)` with a
`func(context.Context, In)` closure receives the real input. This is verified
by a probe rather than inferred, and it is the kind of thing that makes a test
pass for the wrong reason.

## External services

Four integrations, each with one decision worth knowing about.

### TMDB — written by hand

The Go libraries for TMDB expose no context-aware methods, and these calls
happen inside Temporal activities: an activity that cannot be cancelled when
its workflow is cancelled holds a worker slot until its timeout expires. Four
endpoints are all this project needs, so the client is `net/http` and every
method takes a `context.Context`.

One API quirk is handled in `Title.ToSpec`: when TMDB has no translation for a
title it returns the original string with **no flag saying so**. Recording that
as the Russian title would put an English name where the UI promises Russian,
and would make the release selector prefer Russian releases for a title nobody
asked for in Russian. So the Russian title is only stored when it differs from
the original.

### TMDB behind a proxy, and why the posters come through us

Where TMDB is not reachable directly — which is the ordinary case in some
regions — `TMDB_PROXY` sends both the API calls and the images through an HTTP
proxy. One setting, because everything goes through one client.

The images are the part that needs saying out loud. Linking a page straight to
`image.tmdb.org` works perfectly on the server and fails completely in the
browser: the browser is what fetches an `<img>`, and it is not configured with
the proxy. The page renders, every card is a broken image, and **nothing is
logged** — a failed image load is not something a page can report. It looks
like a rendering bug, not a network one.

So the browser asks us. Every poster URL the interface emits is
`/poster/{size}/{path}`, and that endpoint fetches through the same client, and
therefore the same proxy, as the API calls. Three consequences worth knowing:

- **It streams.** An `original` poster is a few megabytes; buffering one per
  concurrent viewer to then copy it to a socket is work with no purpose. The
  body is copied straight through with a flush per chunk.
- **It is behind the session**, like everything else. An open image proxy is a
  way to make the server fetch arbitrary images on demand.
- **The size and the path are validated against a whitelist**, because both are
  concatenated into an upstream URL. Without that check, a path of
  `../../search/movie` turns the image proxy into a general-purpose TMDB proxy
  with the API key attached to whatever the caller asked for.

A `404` from TMDB is cached for a day — the image is not going to appear — but
any other failure is `no-store`, so a transient outage cannot leave the
interface permanently convinced that every poster is missing.

### Plex — written by hand

The one usable Go library has no semver tags, so it can only be pinned to a
pseudo-version that never updates, and its search method calls a legacy
endpoint.

Two things matter here:

- **`GET /library/search` is not a local endpoint.** It is Plex Discover, a
  cloud service, and it knows nothing about your server's library. Existence
  checks go through `/library/sections/{key}/all?includeGuids=1` instead.
- **Matching is on the `tmdb://` guid, never on the title.** Titles are
  localisable, ambiguous and edited by hand, so a title match produces both
  false positives (a remake) and false negatives (a renamed file) — and both
  are invisible to the user. The title is only a pre-filter, so the server does
  not serialise an entire library.

For a series there is a wrinkle: **Plex records the guid on the show, not the
season**, so "season 2 arrived" cannot be checked by guid. The show is matched
by guid and the season is then located among its children by number, carrying
back the episode count that the post-download check compares against.

### Indexer — Jackett aggregate Torznab

The application speaks Torznab, the protocol every Jackett indexer publishes
on. Three front ends are supported: Jackett, Prowlarr, and a generic Torznab
endpoint. Jackett is the default.

The Jackett client uses the **aggregate** endpoint
(`/api/v2.0/indexers/all/results/torznab`), which fans out to every configured
tracker in one request. That endpoint advertises a narrow capability set —
only `t=search` is accepted; `t=movie` and `t=tvsearch` come back with Torznab
error 201 — and the client encodes that in the static `Caps()` it returns.
A spec that names a season or an IMDB id is searched with the free-text title
only, because that is the only thing the aggregate can carry. Russian
trackers match on the release name rather than on identifiers, which is what
this is calibrated for.

Prowlarr mode is the other one with a dedicated client, because Prowlarr's
own JSON `/api/v1/search` does the fan-out server-side and returns JSON that
needs no XML parsing. A generic `torznab` mode is also there for setups that
expose a per-indexer Torznab URL.

Capabilities are read from `t=caps` before searching. This is not a nicety:
an indexer that does not declare `imdbid` will not error on it, it will simply
match nothing — which is indistinguishable from "no releases exist".

### qBittorrent — idempotent by tag

`torrents/add` returns no hash on most versions, so an activity that is retried
after a worker crash cannot tell whether it already added the torrent. Every
torrent this project adds is tagged `mediadbot:<request_id>`, and `Add` looks
for the tag before adding anything.

The tag is also the only reliable link between a torrent and its request: for
hybrid v1/v2 torrents the client's infohash can differ from the one the indexer
advertised.

Two version differences are absorbed here so nothing above has to know about
them: qBittorrent 5.0 renamed `paused` to `stopped` and `pausedUP`/`pausedDL` to
`stoppedUP`/`stoppedDL`.

One thing the client library hides and this package documents: qBittorrent
answers a plain 200 with the body `Ok.` for an accepted torrent and `Fails.` for
a rejected one, and the library maps **both** to success because it does not
read the body. The HTTP response therefore cannot be trusted to say whether
anything was added, and the tag lookup is the only real confirmation.

The save path is deliberately never sent: the client decides where downloads
go.

Authentication prefers an API key (`qbittorrent.api_key`, added in qBittorrent
5.1) over a username and password. With a key no login request is made at all,
which also removes the failure mode where repeated failed logins get the
client's IP banned.

### Verified against a real client

The unit tests for this package run against a stub this project wrote, and a
stub cannot disagree with you: every field name, state string and version
difference it encodes is an assumption. `live_test.go` checks those assumptions
against a real server, read-only, and is skipped unless one is configured:

```bash
MEDIAD_TEST_QBIT_URL=http://host:8112 \
MEDIAD_TEST_QBIT_API_KEY=qbt_... \
  go test ./internal/qbit/ -run TestLive -v
```

Run against qBittorrent 5.2.4 and a library of 56 torrents, it confirmed that
the version gate accepts a real client and picks `stopped` over `paused`, that
every torrent decodes, that all six states present in a real library are ones
the domain layer classifies, that the batched hash lookup returns real rows and
omits an invented hash, and that a tag matching nothing is an empty result
rather than an error — which is the read path the idempotent add depends on.

It also turned up the states that matter in practice: four torrents in
`missingFiles` and one in `error`, both of which this project already treats as
failures worth abandoning.

The write path is covered too, with a synthetic magnet — a random infohash
belonging to nothing, added stopped so no peer is ever contacted and no file is
created. It confirms that a second `Add` recognises the torrent the first one
added, which is the property Temporal's activity retries depend on, that the
version-derived pause option actually takes effect on a real 5.x server (a
torrent added stopped really does come back as `stoppedDL`), and that `Delete`
removes it. The test sweeps its own tag before and after, so an interrupted run
cannot leave anything behind.

One consequence of testing against a real library: creating a category leaves
one there permanently, because qBittorrent keeps empty categories. The live
test therefore sets only a tag, which is what identifies a torrent anyway.

## Requirements

- Go 1.26 or newer (the Temporal SDK requires it; this project is built on 1.27)
- Docker and Docker Compose
- A TMDB API key, a Plex token, a qBittorrent instance, and either Prowlarr or
  Jackett
- A Telegram bot token from @BotFather, if you want the bot — everything else
  works without it

## Install

The shortest path to a running `mediad`:

```bash
git clone <repo> mediad && cd mediad
cp .env.example .env
# edit .env — see "What you need" for the minimum set of secrets
make up          # PostgreSQL + Temporal
make migrate     # apply the schema once Postgres is reachable
make all         # worker + HTTP API in one process
```

`make up` waits until both databases are created and Temporal answers on
`:7233`. The first run of `make all` prints a redacted configuration to
stdout — if anything is missing, it surfaces here, before any request
goes looking for it. Open <http://localhost:8081> in a browser; the
setup wizard creates the administrator account.

For a split deployment (worker and API as separate processes):

```bash
make up         # terminal 1 — infrastructure
make worker     # terminal 2 — Temporal worker
make api        # terminal 3 — HTTP API + web UI
```

For a Docker-only deployment (application included in compose):

```bash
make up-all     # everything in containers
```

The Docker image is rebuilt by `make build`. The compose file pins
PostgreSQL 16 and a specific Temporal image because `auto-setup` is
deprecated; the schema is now created by the `temporal-schema-setup`
admin container.

The Telegram bot is the only optional piece. With
`MEDIAD_TELEGRAM_TOKEN` (from `@BotFather`) and
`MEDIAD_TELEGRAM_ENABLED=true` set, `make all` starts it; without them,
the bot stays quiet and the rest of the system carries on.

### Verifying the install

- <http://localhost:8081> — the web UI.
- <http://localhost:8233> — the Temporal Web UI.
- `make config` — effective configuration with secrets redacted.
- `make test` — unit tests (DB-backed ones skip without `MEDIAD_TEST_DSN`).

If something is wrong, `make logs` tails the Temporal log and the
default-level logs from the worker/API; the application itself logs to
stdout at the level set by `MEDIAD_LOG_LEVEL`.

For development it is usually easier to run the binary on the host and
only the infrastructure in Docker (so config changes are immediate, and
delve or a debugger can attach). Use `make up` for infrastructure, then
`make worker` and `make api` in separate terminals.

### A wart worth knowing about

The binary reads `MEDIAD_*` variables, but `.env.example` currently names the
secrets the short way (`TMDB_API_KEY`, `PLEX_TOKEN`, …) because that is what
docker compose interpolates. Running on the host therefore needs the prefixed
spelling, or the setting is silently absent and the default is used:

```bash
set -a && . ./.env && set +a
export MEDIAD_TMDB_API_KEY="$TMDB_API_KEY"
export MEDIAD_PLEX_TOKEN="$PLEX_TOKEN"
# ...and so on for the indexer, qBittorrent and Telegram
```

The names are normalised in stage 10 so one `.env` works for both paths.

## Configuration

Settings come from four places, later ones winning:

1. built-in defaults — the binary runs with no configuration at all
2. `config.yaml` in the working directory, or the path given by `--config`
3. `config_dev.yaml` in the working directory, layered on top of the file above
4. environment variables, `MEDIAD_<SECTION>_<KEY>`

`config_dev.yaml` is the per-developer override: a committed
`config_dev.yaml.example` ships the dev-tuned timings (seconds instead of
hours for the search ladder, a one-minute download deadline, `quality.russian_audio: false`)
and the local service hosts. Each developer copies it to `config_dev.yaml`
and tweaks from there. The file is in `.gitignore`, so what is on disk stays
local. Pointing `--config` at anything other than the default opts out of
this layering, so a developer who picks a specific file gets exactly that
file.

Only the first underscore after the prefix becomes a dot, so
`MEDIAD_POSTGRES_MAX_CONNS` sets `postgres.max_conns` and
`MEDIAD_SEARCH_CONTINUE_AS_NEW_HISTORY` sets `search.continue_as_new_history`.
The rule holds because every key is exactly two levels deep.

Secrets belong in the environment, never in the file. `make config` prints
everything with passwords and tokens masked, and its output is itself a valid
`config.yaml`.

The settings worth knowing about:

| Key | Default | Why |
|---|---|---|
| `search.search_deadline` | `17520h` (2 years) | How long a request keeps looking. |
| `search.backoff_ladder` | `1h,2h,4h,8h,12h,24h` | Check frequency. Strictly hourly would be 17 520 searches per request over two years, each fanning out across every tracker. The ladder gives ~735 for effectively the same result. |
| `download.deadline` | `48h` | A torrent that has not finished in two days is treated as dead: the release is blacklisted and the search resumes. |
| `indexer.rate_per_second` | `0.25` | One request per four seconds. Deliberately below the one-per-three target, because the server spreads activity tasks across several partitions and can burst above the configured average — and a burst that trips a tracker's limit gets the IP banned. |
| `tmdb.proxy` | unset | An HTTP proxy for TMDB, where the API is not reachable directly. Covers the images too: the browser is never sent to `image.tmdb.org`, it asks our own `/poster/` endpoint. |

## Command line

The CLI is a full interface, not a debugging aid: it talks to Postgres and
Temporal directly and works with neither the worker nor the API running. That
is deliberate, because the moment you most want to know what a request is doing
is the moment the web interface is down.

```bash
mediad search "матрица"           # ask, choose, request
mediad add movie 603              # by TMDB id, no questions
mediad add tv 1399 --season 2
mediad list                       # what is in flight
mediad list --all --json          # everything, for a script
mediad status 12                  # one request, and what it has done
mediad status movie-603 --events 50
mediad cancel 12
mediad retry 12
mediad notifications --unread
```

A few decisions worth knowing about:

**A series always needs a season.** There is no "the whole series" request, so
`search` asks which season and `add` requires `--season`. Season 0 is a real
season — TMDB numbers specials that way — so the absence of the flag is not
represented by the number zero anywhere.

**Nothing blocks on a question in a script.** The CLI asks only when stdin is a
terminal. Otherwise the ambiguity becomes an error naming the flag that would
have resolved it, rather than a menu read from a closed pipe and a command that
appears to hang.

**It asks once, and only when it decided for you.** A query that matched one
title and was resolved without a single question gets a confirmation before a
two-year request is created. A title you picked out of a menu, or named with
`--pick` or `add`, is already an answer and is not asked about again.

**A retry is a fresh run of the same request.** The row, its watchers and its
audit trail carry over — so "I asked in March, it failed, I asked again in
June" is one story — and the search window starts again from now, because the
two reasons to retry are that the deadline passed and that too many releases
were found wanting. Releases already known to be bad stay blacklisted, so a
retry looks for something different instead of picking the same torrent again.
The cost is that a hand-retried request can outlive its original two years;
that is a person deciding, not a loop.

**Reading notifications does not mark them read.** The same rows are the web
inbox and the queue the Telegram bot delivers from, and a glance in a terminal
should not clear what somebody has not seen on their phone yet.

`--json` is available on the read commands and on `add`, `search` and `retry`.
The shapes are defined in the CLI rather than mirrored from the storage layer,
so adding a column does not silently change the output.

## Web interface

`mediad api` serves it, and `mediad all` serves it alongside the worker. First
run shows a setup wizard that creates the administrator; after that the door is
closed and accounts exist only by invite.

| Page | What it is for |
|---|---|
| Dashboard | what is being downloaded right now, with a progress bar that updates every three seconds |
| Search | TMDB search, then a season picker for a series, then a confirmation |
| Requests | everything, filterable by state |
| One request | the release, the torrent, the deadlines, and the audit trail |
| Notifications | the same rows the Telegram bot delivers from |
| Settings | password change; invites and users for an administrator |

**Nothing on a render path talks to Temporal.** Every page reads the
projection. That is not an optimisation: SQL-backed visibility has no
`ORDER BY` at all — asking for one is a hard `InvalidArgument` — it is
eventually consistent by seconds, and a workflow query needs a live worker
polling the queue. An interface built on it shows an empty page whenever the
worker is restarting, which is exactly when somebody is looking.

**One piece of live behaviour, and it is HTMX.** The dashboard polls
`/fragments/active` every three seconds and swaps the table. Everything else is
a plain form post and a redirect, so the interface works with JavaScript
disabled except for the progress bar. `htmx.min.js` is vendored into
`internal/web/views/static` rather than loaded from a CDN — a self-hosted
instance should not need the internet to render its own pages, and it fetches
nothing from anywhere else.

**Accounts, and why there is no registration form.** This is somebody's home
server, and an open signup would be an open signup for their Plex library. The
first account is created by the setup wizard and is an administrator; every
later one needs a code an administrator generates, valid for seven days and
single-use.

**Sessions are stored hashed.** The cookie carries a random token and the
database holds its SHA-256, so a leaked backup or a `pg_dump` in a ticket does
not hand over live sessions. Changing a password deletes every session for that
account, including the one that made the change — a password change that leaves
other sessions alive does not do the thing it is for.

**Every form carries a CSRF token** (double submit: a cookie plus a matching
field). The session cookie is `SameSite=Lax`, which already blocks cross-site
POSTs in current browsers; the token is the layer that keeps working when that
stops being true. A test walks every rendered page and fails if a POST form has
no token, because the failure mode is a form that is rejected at submit and
looks like a broken button.

**Binding to the network over plain HTTP logs a warning.** It is a warning and
not a refusal because running behind a reverse proxy that terminates TLS is
normal, and the point is that doing it by accident is visible.

### Two things the interface will not do

**It will not start a workflow on a guess.** A series always needs a season
chosen explicitly, and the confirmation page is rendered from metadata re-read
from TMDB rather than from anything the browser posted. The form carries an
identity — type, tmdb id, season — and nothing else, because this spec is
frozen into a two-year workflow, and a client that could post the title could
post a wrong one.

**It will not show you a lie about a stuck request.** Which brings us to the
reconciler.

## The reconciler

A background loop in the worker, every ten minutes, comparing rows that claim
to be in progress against what the workflow engine says is running.

It exists because the two can disagree and the disagreement is **silent**. A
workflow that was terminated, or that was never claimable because no worker
registered its type, leaves a row reading `searching` with nothing behind it.
And `searching` is an active state, so the partial unique index refuses both a
new request for that media and a retry of the stuck one. Nothing but a
hand-written `UPDATE` can move it.

This is not hypothetical. It is how the defect in stage 7 was finally seen:
every request was being started under a workflow type no worker had registered,
so six executions existed that had never run once. The reconciler is the
instrument that would have said so on its own.

Three details decide whether it does more good than harm:

**It asks the engine, not the visibility store.** `DescribeWorkflowExecution`
is strongly consistent; listing workflows is eventually consistent, has no
ordering, and would report a workflow that started a second ago as absent.

**It only fires on rows older than fifteen minutes.** A request row is written
*before* its workflow is started, so there is a window in which a perfectly
healthy request has an active state and no execution. Without the grace period
the reconciler would eventually land in that window and mark somebody's
brand-new request as failed. The age is `updated_at`, which the workflow and
the download watcher both touch.

**A transient error is not a stale row.** `isNotFound` matches one concrete
service error type and nothing else. Widening it to "any Temporal error" reads
as defensive and inverts the failure: one blip would mark every in-flight
request as failed at once. A test pins each case.

A row it repairs goes to `failed` with the observed reason in `last_error`, and
an event with actor `reconciler` — a value the schema's vocabulary already had
room for. Failed rather than canceled: nobody decided it, and a user who sees
`failed` knows to retry, where a silent `canceled` looks like something they
did.

## Telegram bot

`mediad bot` runs it on its own; `mediad all` runs it alongside everything
else. Setting `MEDIAD_TELEGRAM_TOKEN` turns it on — with no token it logs that
it is disabled and the rest of the process carries on, and `mediad bot` refuses
outright rather than starting a process that does nothing.

| Message | What it does |
|---|---|
| any title | searches TMDB and offers the matches as buttons |
| `/list` | what this account is waiting on |
| `/cancel <id>` | stops one of them |
| `/link <code>` | attaches the chat to an account |
| `/help` | the same list |

**Long polling, not webhooks.** The service is self-hosted behind a home
connection, and a webhook would need a public hostname, a certificate and a
port forward. Polling needs none of them and works from behind anything.

**The buttons carry a session id, never a title.** Telegram caps
`callback_data` at 64 bytes, and a page of Russian titles is past that before
the list is full. The payload lives in `search_sessions` — in the database
rather than in memory, so a bot restart does not invalidate every button in
every chat, which the user experiences as pressing a button and being told it
expired.

**A series asks which season, in the same message.** The message is edited
rather than answered again, so a conversation stays two messages long instead
of growing one per tap, and editing removes the buttons — the choice has been
made. The season's own première date travels into the request, not the
series': season 6 of a show that began in 2011 is not media released in 2011,
and the workflow would search for something that has not aired.

### The notifications table is the outbox

The workflow's `NotifyWatchers` activity writes rows; the bot reads them. That
split is the whole design, and it is what makes the bot optional:

- switched off entirely, the rows simply accumulate and the web inbox shows
  them;
- restarted, it picks up where it left off;
- a send that fails leaves `delivered_telegram_at` unset, so the next pass
  tries again — the alternative loses a message to a transient failure, and the
  one thing this project must not do is fail to tell somebody their film is
  ready.

Rows are marked delivered in one statement **after** the sends, not before and
not one at a time: marking first would lose the whole batch to a crash
mid-loop.

### Linking a chat to an account

A chat is a Telegram identity, which is not the same thing as an account.
`/link <code>` joins them, and the code is the one an administrator generates
in the web interface — same table, same single-use semantics as an invite,
because the mechanism is identical and only the destination differs.

Without it, somebody who signed up on the web would get a second, unrelated
account the moment they talked to the bot: notifications would split between
two inboxes and `/list` would show half their requests. Until a chat is linked,
the bot says so rather than guessing, because there is no safe guess — creating
an account per chat is how the split happens in the first place.

**Cancelling is scoped to the watchers.** Request ids are small numbers, so
`/cancel 12` is trivially guessable; the handler checks that the account is one
of the request's watchers before doing anything. Without it, anybody could stop
a stranger's download by typing a number.

## Architecture

One binary; the subcommand decides what it is.

```
mediad worker   Temporal worker — polls the task queues
mediad api      HTTP API and web UI
mediad all      both, for a small deployment
mediad migrate  apply migrations and exit
mediad config   print the effective configuration
```

### Where state lives

Two stores, with a deliberate split:

**Temporal** owns the process: which phase a request is in, when the next
attempt is due, which release was chosen, retries, timers, and the full audit
trail of what happened.

**PostgreSQL** owns the things people and relationships are made of: users,
sessions, who is watching which request, the read model the UI lists from, and
the cached answer to "is this in Plex yet".

The consequence worth stating: **workflows do not track watchers.** The list of
people to notify lives in Postgres, and the `NotifyWatchers` activity reads it.
So "user B asks for what user A is already downloading" is one `INSERT` and no
signal to the workflow at all.

### Deduplication

Three layers, each covering a different hole:

1. The `media` table is unique on `(media_type, tmdb_id, coalesce(season,-1))`,
   and `requests` has a partial unique index on `media_id` for active states.
   This is the arbiter. It is atomic, has no check-then-act window, and does not
   depend on Temporal's semantics.
2. The workflow ID is `movie-603` / `tv-1399-s2`, so Temporal guarantees a
   single execution even if two starts race past the database.
3. `WorkflowIDReusePolicy: ALLOW_DUPLICATE` lets a *closed* workflow ID be
   reused — otherwise re-requesting a film you deleted from Plex would be
   impossible for two years.

### Plex without a library mirror

There is no local copy of the Plex library. Presence is recorded on the `media`
row and checked only for titles someone actually asked for: a targeted
`/library/sections/{key}/all?title=…&includeGuids=1`, matched on the `tmdb://`
guid and never on the title. The result is cached with a TTL.

This costs the ability to answer "what do I have?" without asking the server.
Nothing in the project needs that.

For TV there is a wrinkle worth knowing: Plex attaches the `tmdb://` guid to the
*show*, not the season, so "season 2 arrived" cannot be checked by guid. The
episode count of that season is snapshotted before the download and the check is
that it grew.

## Development

```bash
make check     # format, vet, build, test
make test-db   # the same, including the tests that need Postgres
make check-ci  # everything CI runs, including the determinism checks
```

Database tests are skipped unless `MEDIAD_TEST_DSN` is set, so `go test ./...`
stays green on a machine with nothing running. `make test-db` reads the DSN out
of `.env` and runs them. Each of those tests creates its own throwaway database
and drops it afterwards — the `mediad` role is granted `CREATEDB` for exactly
that reason, so a test run never touches your data.

## Changing workflow code

This is the one part of the project where a mistake is invisible until it is
expensive, so it gets its own section.

### The rule

Workflow code is replayed from history, so it has to make the same decisions it
made before. In practice:

**Forbidden in a workflow:** `time.Now`, `time.Sleep`, `math/rand`,
`crypto/rand`, `uuid.New()`, `os.Getenv`, the `go` statement, native channels,
native `select`, `sync.Mutex`, ranging over a map, and any I/O.

**Use instead:** `workflow.Now`, `workflow.NewTimer`, `workflow.Go`,
`workflow.NewChannel`, `workflow.NewSelector`, `workflow.GetLogger`,
`workflow.DeterministicKeys`. For a random value, `workflow.SideEffect` records
it in history so replay returns the same one — `workflow.NewRandom` does not
exist.

One that catches people out: **`workflow.Context` is not `context.Context`.**
Its `Done()` returns a `workflow.Channel`, not a `<-chan struct{}`, and it
cannot be converted. A shared helper that takes a `context.Context` therefore
cannot be called from a workflow at all — which is the reason scoring, parsing
and delay computation live in activities rather than in helper functions the
workflow calls.

### What to do when you have to change one

Most changes never need to touch a workflow. The list is worth keeping short,
because everything in it is a decision that cannot be taken back:

1. **Change the activity, not the workflow.** Almost everything belongs here —
   selection rules, delays, parsing, thresholds. An activity's implementation
   can change freely; a history records its *result*, not how it got there.
2. **Let `ContinueAsNew` carry it.** A workflow that restarts its history picks
   up the new code on the next run. These restart roughly every ten days, so a
   change that can wait that long needs no versioning at all — this is the
   documented approach for long-running workflows with `ContinueAsNew`, and it
   is why the project uses one.
3. **`workflow.GetVersion` for the rest.** When a change genuinely cannot wait
   for a restart, it has to be gated:

   ```go
   v := workflow.GetVersion(ctx, "search-fanout", workflow.DefaultVersion, 1)
   if v == workflow.DefaultVersion {
       // the old path, for executions that already made that decision
   } else {
       // the new path
   }
   ```

   Keep the call site, raise the maximum, and never reorder the branches.

### The three checks

Each catches something the others cannot.

**`make workflowcheck`** — static analysis. It traces through the call graph
and flags a forbidden call in workflow code at the moment it is written, which
is the cheapest possible place to catch it. It cannot see a change in the
*order* of commands, and it deliberately ignores a timer's duration.

**`make replay`** — replays every history in `internal/request/testdata/histories`
against the current code. This is the only thing that catches what the static
check cannot: a reordered activity, an added timer, a changed branch. The
corpus is recorded from real runs; capture more with:

```bash
make capture IDS="download-watcher movie-603"
make inspect-histories        # decode what you just captured
```

**Read what you capture before committing it, and do not do it by grepping.**
The Temporal CLI writes payloads as base64-encoded JSON, so a search for a title
or a path finds nothing — the values are there, just not in a form you can read.
`make inspect-histories` decodes every payload that crossed the workflow
boundary: the input, and the arguments to and results from every activity.

What matters for replay is the *sequence* of commands, not the values, so a
history naming something you would rather not publish should be scrubbed rather
than dropped — the corpus is only useful if it still replays. The committed
corpus here names one film and nothing else: no paths, no tracker names, no
credentials.

CI fails if the corpus is empty. A fresh clone may legitimately have none,
which is why the test skips locally — but a repository that is supposed to have
them and does not has quietly lost this coverage, and that is worth failing
over.

**An alert on `temporal_workflow_task_execution_failed{failure_reason="NonDeterminismError"}`.**
The last line of defence, and not optional: because the default policy leaves a
workflow **stuck rather than failed**, nothing else will tell you. It will not
appear in a list of failures, it will not return an error to its caller, and it
will not complete. It just stops, and with a two-year execution timeout it
stops for two years.

### Two things that will bite you

**Postgres data lives in `./data/postgres`.** Two consequences. On Linux the
directory must be owned by uid 999, the postgres user inside the image:

```bash
sudo chown -R 999:999 ./data/postgres
```

And it must be a **local** disk. Postgres relies on locking and fsync semantics
that NFS and SMB do not guarantee, and the failure mode is silent corruption
rather than a startup error.

`PGDATA` points at a `pgdata` subdirectory of the mount. That is not cosmetic:
`initdb` refuses to run in a non-empty directory, and a host directory almost
never is — a `.DS_Store`, a `lost+found`, anything.

**Temporal history limits are real and low.** A workflow warns at 10 240 events
and is **terminated** at 51 200. An hourly loop costs about 12 events per
iteration, so a two-year request would blow through it several times over. The
workflow checks `GetCurrentHistoryLength()` on every iteration and calls
`ContinueAsNew` past the configured threshold, carrying its state explicitly.
That is also why the download progress poller is a separate singleton workflow
rather than a loop inside each request.

### Starting a workflow, and two ways to make one that never runs

Both of these report success. Neither produces an error, a failed execution or
anything in a log. The request simply never moves, and with a two-year
execution timeout it never moves for two years.

**Start by type name, never by function.** `ExecuteWorkflow` takes either. Given
a function it uses the function's own Go name and ignores the alias the worker
registered it under, because it looks aliases up in the *client's* registry —
and aliases live on workers. The function here is called `Workflow`, so passing
it starts executions of type `Workflow`, while every worker registers
`RequestWorkflow`:

```go
// wrong: starts type "Workflow", which no worker knows
c.ExecuteWorkflow(ctx, opts, Workflow, in)

// right
c.ExecuteWorkflow(ctx, opts, WorkflowType, in)
```

The symptom is a workflow that is `Running` in the Temporal UI with an empty
history and a workflow task nothing ever claims. `mediad status` shows a
request waiting for its first search, forever.

**`WorkflowIDConflictPolicy: FAIL` on its own does nothing.** A retry must not
attach to an execution that is already running, so it asks the server to refuse
instead. But the Go client takes that refusal and converts it into a handle to
the running execution — with a nil error — unless
`WorkflowExecutionErrorWhenAlreadyStarted` is also set. Measured, not assumed:
`FAIL` alone returns the running run's id and no error.

```go
opts.WorkflowIDConflictPolicy = enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL
opts.WorkflowExecutionErrorWhenAlreadyStarted = true   // without this, the above is a no-op
```

`internal/request/service_test.go` pins both. It is a unit test with a fake
client and no server, which is enough: what needs checking is which arguments
reach the engine, and that is observable without one.

### Upgrading Temporal

Running workflows survive a server upgrade — state is in Postgres, not in the
process — but the rules are strict and breaking them is unrecoverable:

1. **Schema first, then the server.** The `temporal-schema-setup` container
   exists precisely so this is a two-line change: bump the `admin-tools` tag,
   let it run, then bump the `server` tag.
2. **One minor version at a time.** `1.32.x → 1.33.x`, never skipping. Skipping
   can leave old data unreadable and the upgrade permanently broken.
3. `NUM_HISTORY_SHARDS` is fixed at first start and cannot be changed later.

### Backups

```bash
docker compose exec -T postgresql pg_dump -U temporal -Fc temporal            > temporal-$(date +%F).dump
docker compose exec -T postgresql pg_dump -U temporal -Fc temporal_visibility > temporal-vis-$(date +%F).dump
docker compose exec -T postgresql pg_dump -U mediad   -Fc mediad              > mediad-$(date +%F).dump
```

Back up both Temporal databases or neither: losing visibility alone leaves
workflows running but invisible. Use `pg_dump` rather than copying
`./data/postgres` — the data directory is bind-mounted for transparency and
rsync, but copying it from a running server gives an inconsistent snapshot.
