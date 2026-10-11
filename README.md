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
| 4 | Workflow, activities, task queues | next |
| 4 | Workflow, activities, task queues | |
| 5 | Download watcher | |
| 6 | Determinism checks in CI | |
| 7 | Repositories and CLI commands | |
| 8 | Web UI | |
| 9 | Telegram bot | |
| 10 | Packaging, backups, alerts | |

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

### Indexer — Torznab, or Prowlarr's JSON API

Both produce the same `domain.Release`, so nothing downstream knows which is in
use. The difference that forced two implementations: **Jackett exposes an
aggregate Torznab endpoint that searches every tracker in one request; Prowlarr
does not** — it serves one indexer per endpoint. So pointing Torznab mode at
Prowlarr means covering one tracker per client, and Prowlarr mode uses its own
JSON search API, which aggregates server-side.

Capabilities are read from `t=caps` before searching. This is not a nicety: an
indexer that does not declare `imdbid` will not error on it, it will simply
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

## Requirements

- Go 1.26 or newer (the Temporal SDK requires it; this project is built on 1.27)
- Docker and Docker Compose
- A TMDB API key, a Plex token, a qBittorrent instance, and either Prowlarr or
  Jackett

## Getting started

```bash
cp .env.example .env
# fill in POSTGRES_PASSWORD and MEDIAD_DB_PASSWORD at minimum
make up
```

`make up` starts PostgreSQL, creates both Temporal databases and the
application database, starts the Temporal server and its web UI.
Then:

```bash
make migrate   # apply the schema
make config    # print the effective configuration, secrets redacted
make all       # worker and API in one process
```

The Temporal Web UI is at <http://localhost:8233>; the application's own UI
will be at <http://localhost:8080> once stage 8 lands.

During development it is usually easier to run the binary on the host and only
the infrastructure in Docker:

```bash
make up        # infrastructure only
make worker    # terminal 2
make api       # terminal 3
```

## Configuration

Settings come from three places, later ones winning:

1. built-in defaults — the binary runs with no configuration at all
2. `config.yaml` in the working directory, or the path given by `--config`
3. environment variables, `MEDIAD_<SECTION>_<KEY>`

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
```

Database tests are skipped unless `MEDIAD_TEST_DSN` is set, so `go test ./...`
stays green on a machine with nothing running. `make test-db` reads the DSN out
of `.env` and runs them. Each of those tests creates its own throwaway database
and drops it afterwards — the `mediad` role is granted `CREATEDB` for exactly
that reason, so a test run never touches your data.

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
