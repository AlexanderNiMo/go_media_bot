# UI/UX Project Plan — `mediad`

**Date:** 2026-11-03; last reconciled with the code on 2026-11-04 (baseline `b0083a3`, plus Phase 3 working-tree changes)
**Status:** Phases 0–3 implemented; Phases 4–5 deferred. Phase 3 browser checks use fixtures; screen-reader verification remains open.
**Scope:** the web front-end (`internal/web`), plus the vocabulary it shares with Telegram
**Author:** Lead UI/UX Engineer / Product Designer (solo)

This document retains the original audit and design proposals as context.
The implementation status below and §7 describe the current code; original
`file:line` references are historical and may have moved. Research outcomes,
browser checks and usability metrics are not claimed without recorded evidence.

### Current implementation status — 2026-11-04

| Area | Current state | Evidence |
|---|---|---|
| Foundation | Russian state/reason/audio vocabulary, theme tokens, working accessible progress bar, structural accessibility guard | `internal/web/vocab.go`, `views/static/style.css`, `views/fragments.html`, `a11y_test.go` |
| Dashboard | Three buckets, one query per bucket; age, attempts and next check; OOB polling preserves bucket disclosures | `internal/web/views.go`, `handlers.go`, `views/dashboard.html`, `views/fragments.html` |
| Scope | Admin pinned to «Все»; others default to «Мои» and may widen | `internal/web/handlers.go` (`applyScope`) |
| Safety | State-dependent GET → POST cancel confirmation; hard candidate rejections blocked in the page and endpoint | `internal/web/views/cancel.html`, `handlers.go`, `vocab.go` |
| Request page | Requester from `requested_by` (migration 00012), status sentence, requirements, persisted «Расширенное» disclosure | `internal/web/views/request.html`, `views.go` |
| Candidate table | Quality, audio/studio, release-specific reason numbers, unknown seeders as «—», best-first order | `internal/web/candidates.go`, `views/request.html`, `internal/release/score.go` |
| Responsive layout | All data tables use `table-cards` with explicit table roles; narrow events render as a timeline | `internal/web/views/static/style.css`, `views/fragments.html`, `views/request.html` |
| Theme | Auto/light/dark stored per browser; auto removes `data-theme` | `internal/web/views/layout.html`, `views/settings.html` |
| Remaining | Phases 4–5 deferred: empty states, shared voice, poll focus/announcements, performance and user validation | §6 and §7.2 |

**Next planned phase: Phase 4** — empty states and voice consistency.
Phase 3 is implemented and checked in headless Chrome on rendered fixtures at
320 / 375 / 768 / 1024 / 1440 px in both themes, including both desktop sidebar
states. Screen-reader and measured contrast checks remain open.
`docs/ui-patterns.md` records the candidate table and Phase 3 mobile patterns.

Two things are **out of scope by construction** and are not revisited anywhere
below:

- The sidebar's two-state (`open` / `icons`) behaviour and its `icons` default.
  That decision is recorded in `AGENTS.md` as settled after a real regression.
- The technology constraint: `html/template` + vendored `htmx.min.js` +
  vendored `pico.min.css`, no build step. Every recommendation below works
  inside it.

### Decisions taken (2026-11-03)

| # | Decision | Consequence for this plan |
|---|---|---|
| D1 | **Several people use the installation** | The dashboard is a shared wall, not a personal list. Adds a «Все / Мои» scope filter (§3.2), renames «Ждёт вас» → «Ждёт выбора» (§3.2), adds «Запросил» to the request page (§3.3). The operator/asker split is real: `is_admin` marks the operators. |
| D2 | **`manual` handling stays web-only** | No Telegram work in this plan. The existing Telegram manual notification and candidate buttons (shipped in #6) are left untouched. One consequence is flagged in §8.1 — it is not acted on here. |
| D3 | **Ship auto / light / dark** | Dark mode comes free from `prefers-color-scheme` as soon as every token maps to a `--pico-*` variable (Phase 0). The tri-state toggle is pulled forward from Phase 4 into Phase 1 (§4.2). |
| D4 | **Only Phase 0 and Phase 1 now** | §7 is resequenced around Phase 0–1. Phases 2–5 are deferred, not deleted, and §7.2 lists what is being deferred and one item that **cannot** be deferred. |
| D5 | **The administrator sees everything** | The requirement arrived as "the admin must see all releases". It is implemented as the **scope rule**: the administrator's scope is «Все» and cannot be narrowed, everybody else starts on «Мои» and can widen it in one click. See §3.2.2 — the reading of "releases" that was rejected is recorded there, because a global catalogue of releases is a different product and needs a different data model. |

---

## 1. Executive Summary

**Product.** `mediad` names a film or a season, checks Plex, hunts a Russian-audio
release on trackers, hands it to qBittorrent, and tells you when it is done. The
distinguishing claim is **the horizon**: a request lives two years, is re-checked
on a backoff ladder, and "a proper dub finally appeared" is an event that can
arrive months later.

**Original diagnosis (before Phase 0; resolved items remain here as context).** The web front-end (13 templates, Pico v2.0.6 +
htmx + an 18.5 KB `style.css`) is a competent *download manager*. It is not yet
a product that embodies the horizon.

| Finding | Evidence |
|---|---|
| The dashboard models the product as "what is downloading right now" — two buckets: «Что качается» and «Недавно» | `internal/web/views/dashboard.html:26`, `:79` |
| A parked request is a *row in the active table*, indistinguishable from a transfer in progress | `.Active` = `storage.ActiveStates()` — `searching`, `adding`, `manual`, `ongoing` all land in one list (`internal/web/handlers.go:227`) |
| The product's core promise has **no on-screen representation** | `NextRunAt` is persisted per request (`internal/request/workflow.go:347`) but never rendered; `Deadlines` (`internal/web/views.go:444`) is one opaque string |
| The entire scoring apparatus is invisible | `AudioTier` (7 levels, `internal/release/audio.go:24`) is never rendered in any template; rejection reasons render as raw snake_case codes (`internal/web/views/request.html:88` — `{{.RejectionReason}}`) |
| **The download progress bar does not render** | `<div class="bar"><i style="width: X%"></i></div>` (`internal/web/views/fragments.html:9`); `.bar` is defined in neither `pico.min.css` nor `style.css`, and `<i>` is inline so `width` does not apply. Only the adjacent text survives |
| 3 of 10 request states have no styling | `searching`, `adding`, `manual` fall through to the default gray pill (`internal/web/views/static/style.css:70–80`). `manual` is the one state that *needs a human* |
| Every candidate offers the same "Скачать это" button, regardless of why it was rejected | `internal/web/views/request.html:90–95` — a `blacklisted` release and a `too_few_seeders` release are presented identically |
| "Unknown seeders" renders as `-1` | `internal/web/views/request.html:84` — `{{.Seeders}}` with no formatting |
| Six empty states, five of which are one line of muted text with no way forward | `dashboard.html:123,205`; `notifications.html:30`; `search.html:28`; `requests.html:35`; `seasons.html:24` |

**Design thesis: make waiting legible.** A request that has searched for four
months must look *patient*, not *stuck*. The UI's job is to answer three
questions a person actually has: **Is it still looking? What is it looking for?
Why hasn't it found it yet?** Everything else is secondary.

**Three design moves:**

1. **Waiting becomes a first-class visual state** — split the active table by
   *time-scale*, not by workflow phase. Transfers (seconds → hours) and parked
   requests (hours → two years) are different products wearing the same shirt.
2. **Policy becomes a visible contract** — the release tier, the studio that
   decided it, and the Russian-language rejection reason, rendered for humans.
   The machine-readable codes stay in the event log exactly as they are; the UI
   adds a layer *beside* them, never replacing them.
3. **Two audiences, one page** — the *asker* sees the story; the *operator* can
   expand the machine. Workflow ID, infohash, content path and the raw events
   table move behind one disclosure, not deleted.

**Five principles that decide every later argument:**

1. Waiting is a state, not an absence.
2. Never show a machine code to a human — translate, don't expose.
3. One status vocabulary across web and Telegram.
4. Progressive disclosure: story first, machinery second.
5. Every irreversible action states its consequence. (Cancelling a request
   **can delete what is already on disk** — the workflow removes the torrent's
   files unless the request is watching for the rest of a season. Today's
   "Отменить" is a bare button that says nothing about this, and the plan
   itself got the consequence wrong at first; see §7.3.)

**Non-negotiable constraints (from `AGENTS.md` and the code):** html/template +
htmx + Pico, no build step; the sidebar default stays `icons`; forms are
siblings, never nested; `{{path}}` does not interpolate nested `{{…}}`; the UI
is Russian.

---

## 2. Discovery & Research Plan

A solo designer's research budget is one person-weeks, not one quarter. Every
method below is chosen for what it can answer that the others cannot.

### 2.1 Instrument-first (Week 1, ~2 days) — before any user contact

**The event log is already a longitudinal dataset.** `request_events` logs every
state transition, every rejection, every attempt, for the life of every request.
Mine it before running a single study:

- **Time-to-first-acceptable-candidate** distribution (median, p90). This is the
  real waiting experience.
- **Rejection reason histogram** across all requests — the reason distribution
  tells you which rejection the UI must explain hardest. If 60% of rejections
  are `no_russian_audio`, the candidate table's audio column is the most
  important cell in the product; if 80% are `too_few_seeders`, the seeders
  rendering is.
- **% of requests that reach `manual`**, and how long they sit there
  unanswered.
- **Backoff ladder distribution** — how many requests are on the 24-hour rung,
  and therefore genuinely "parked".
- **Abandonment rate**: requests created and never cancelled, never completed. A
  high rate means the horizon *is* working (people forgot about them) — and the
  UI currently gives no signal of that success.

> **Deliverable:** one page of numbers. This page becomes the "why the redesign
> exists" artifact and it costs almost nothing.

### 2.2 Moderated usability testing (Weeks 1–2, ~4 days)

7 participants, 25–40, Russian-speaking, who own or use a Plex/media server and
have used a torrent client. Remote, 45 minutes, screen-recorded. Screener
question: *"when you want a film that isn't out yet, what do you do today?"* —
this filters for people with the actual problem, not people who think they have
it.

**The five tasks** (each with a written success criterion, so scoring is
objective rather than vibes):

| # | Task | What it tests | Success = |
|---|---|---|---|
| T1 | "Add a film you saw announced but isn't out yet." | The flagship horizon flow | User states that the bot will keep looking without them |
| T2 | "Add season 2 of a series that's currently airing." | Season-as-unit, partial packs | User picks a season, not the show |
| T3 | "A request has been searching three weeks and hasn't found anything — explain to me what the bot is doing." | **Legibility of waiting** | User mentions the next check / the age / that it's still trying |
| T4 | "Find out whether the release the bot picked has Russian audio or just Russian subtitles." | Audio-tier surfacing | User distinguishes dub from subtitles *without reading raw code* |
| T5 | "Cancel a request and tell me what happened to the files already on disk." | Consequence comprehension | User correctly says what happened to the files (or asks — partial credit, full credit for the *correct* answer after a confirm dialog) |

T3 and T5 are the two that decide the plan. Everything else is secondary.

### 2.3 Diary study (Weeks 2–4, ~1 day setup, then passive)

The only method that fits a two-year product. 6 participants, 21 days, one
Telegram message a day: *"Что вы сегодня хотели посмотреть? Нашли что-нибудь?"*

The bot is already the delivery channel (`internal/telegram`), so this costs
almost nothing and produces the one thing a lab session cannot: evidence about
**patience**. Does the user re-engage on day 3, or does the request become
invisible? If requests genuinely get forgotten (which the horizon premise
assumes), the UI's job is to make *checking back* painless, not to make the
wait interesting.

> **Deliverable:** a day-3 / day-7 / day-21 re-engagement curve. This directly
> informs whether the dashboard's default view should be "active" or "waiting".

### 2.4 Comparative audit (Week 1, ~1 day)

Six comparators, examined for one specific thing each — **how do they show "we're
still looking"?**

| Comparator | What to steal | What to reject |
|---|---|---|
| Sonarr / Radarr | The *missing vs. monitored* separation — Sonarr's closest analogue to the horizon, and it works | Its density; Sonarr assumes daily use |
| Overseerr / Jellyseerr | Search → request → Plex-aware "already have it" logic | Its request-once model; mediad's horizon is the differentiator |
| ITrack, Autobrr | Manual override affordances | Manual-first workflows — mediad's value is the opposite |
| Jellyfin "missing episodes" panel | A plain-language framing of absence | Its dismissal of "still looking" |
| A conventional torrent client | Progress and error clarity | A transfer-centric IA that mediad should *not* copy |

The structural finding to look for: every comparator treats "still searching" as
a transient spinner that dies after thirty seconds of silence. **mediad's wait
is measured in months.** That inversion is the design problem — most UI
conventions for "waiting" assume a wait short enough that a spinner suffices.

### 2.5 Key questions, and which method answers each

| Question | Method |
|---|---|
| Does the user understand the two-year promise at request time? | T1 + diary |
| Is a *month*-long wait legible in a UI built for *minute*-long waits? | T3 |
| Which rejection reason most needs a human explanation? | Event-log mining |
| Does the operator/asker split match reality, or is everyone an operator? | T3 + T5 + follow-up interview |
| Do people re-engage after a long wait, or is the horizon invisible? | Diary |
| Which state deserves the strongest visual signal? | Mining + T5 |
| Does "Отменить" cause data loss? | Code (answer known: yes, unless the request is watching for the rest of a season) |
| Does the current search handle Russian vs. original titles? | Code review + T1 (probe) |

---

## 3. Information Architecture & User Flows

### 3.1 Original IA, before Phase 1 (from `internal/web/module.go:92–154`)

```
/                         dashboard: «Что качается» + «Недавно»
/search                   TMDB search → cards
/add/{kind}/{id}          confirm (film, or series → season picker)
/add/tv/{id}/{season}     confirm (season)
/requests                 all requests (table, state filter)
/requests/{id}            request detail (facts, candidates, manual release, events)
/notifications            notification inbox
/settings?tab=… account / telegram / notifications / invites / users
/help /login /setup /join
```

### 3.2 Proposed IA — the same four nav items, reframed

The sidebar stays exactly as it is (four items, `icons` default). What changes
is what "Запросы" contains.

**The central move: split the active table by time-scale.**

| Bucket | States | Timescale | Visual treatment |
|---|---|---|---|
| **Качается** | `downloading`, `adding` | seconds → hours | Live progress bar, byte counts, ETA |
| **Ищет** | `searching`, `pending` | hours → 2 years | Age, next check, attempts count, no progress bar |
| **Ждёт выбора** | `manual`, `ongoing` | manual needs a human; ongoing waits for season updates | Choose action for `manual` only; `ongoing` remains automatic |

Before Phase 1a, all six lived in one table under «Что качается», which is a category error:
`searching` for four months and `downloading` at 90% are not the same kind of
thing, and mixing them makes both unreadable.

**Why "Ждёт выбора" and not "Ждёт вас" (D1).** Several people use the
installation, and a `manual` request is a *shared* queue: `handlePickCandidate`
is not owner-restricted (`internal/web/handlers.go:1142`), so any user can pick
a candidate for any request. A label that says "waiting for you" would be
wrong for everyone except the one person who happens to be looking. The bucket
names the *state*, not the audience.

**Within "Ищет", a new sort: "По ожиданию"** — longest-waiting first. This is the
one view that makes the two-year horizon real: a list of things you asked for a
year ago that are still quietly running.

### 3.2.1 The scope filter: «Все» / «Мои»

A request is global and users attach through `request_watchers`.
`requested_by` now records who asked (migration 00012); it does not define
ownership or scope. «Мои» means watched requests, using
`RequestFilter.WatcherUserID`.

The shipped rule (§3.2.2) supersedes the original proposal of «Все» for everyone:
administrators always see «Все», without a scope control; everybody else starts
on «Мои» and can switch to «Все». The polled fragment carries the same scope.

The creator is added as a watcher on creation
(`internal/request/module.go:396`), so "Мои" is a good proxy for "asked for by
me" — it is not the same thing (anyone can attach to a request), and the copy
should not claim it is.

**Implementation note.** The dashboard already carries two sibling GET forms
(the state pills in one, the shared filters in the other) that each mirror the
other's controls as hidden inputs, because a form cannot nest. A third filter
means a third mirror set: the scope toggle goes inside one of the two existing
forms as a radio group, and the other form mirrors it. The existing
`TestDashboardFilterFormsCarryEveryControl` test enforces exactly this and will
catch a missed mirror.

### 3.2.2 The scope rule — D5

**The requirement, and the two readings of it.** "The admin must see all
releases" was added to this plan on 2026-11-04. It has two possible meanings,
and they are not the same work:

| Reading | What it means | What it costs |
|---|---|---|
| **The scope rule** (chosen) | The administrator's dashboard is never narrowed: they see every request, and therefore every release that every request holds | One filter parameter and one rule |
| **A catalogue of releases** | A new page listing every release the installation has ever seen — chosen, rejected, blacklisted — across every request | A durable release log: today the candidate list is **overwritten** on every fruitless search and **deleted** after a pick, and the chosen release's resolution, audio tier and studio are computed at selection time and thrown away. Nothing is stored, so nothing can be listed |

The catalogue was rejected for now, and the reason is worth keeping: it is not
a page, it is a **storage change**, and it is a different product — an audit
of the tracker's output rather than a view of the household's requests. The
data it would need is also the data §3.4's «Качество» and «Аудио» columns
need, so the table is worth having — but it belongs with Phase 2's candidate
table, not with a scope filter.

> **Phase 2 corrected this.** The candidate table reads the release facts out
> of the name at render time rather than storing them, and that is the right
> answer for the table — but it means the catalogue's data was **not**
> captured. It could not have been: the candidate list is replaced on every
> fruitless search and deleted after a pick, so storing the facts on the
> candidate rows would not have made a catalogue possible anyway. The
> catalogue needs a log of every release the installation has ever seen,
> which grows with every search and is a decision of its own.

**What the rule is.** The scope is resolved from the role, not from the URL
alone:

| Who | Default | Can be narrowed? |
|---|---|---|
| Administrator | «Все» — every request in the installation | **No.** A scope that could be narrowed is a scope somebody would eventually leave narrowed |
| Everybody else | «Мои» — the requests they are watching | Yes, to «Все», in one click |

The copy is «Мои» and not «Запрошенные мной», because a request has no owner:
people attach themselves to one. The creator is added as a watcher on
creation, so «Мои» is a good proxy for «asked for by me» — but it is not the
same thing, and the copy must not claim it is.

**Three details that are easy to get wrong, and were:**

1. **The polled fragment has to carry the scope.** The dashboard polls
   `/fragments/active` every three seconds. A fragment that dropped the
   scope would fall back to the reader's default and quietly narrow the table
   back under the page it belongs to.
2. **The administrator is offered no control at all.** A locked «Все / Мои»
   toggle would say "you may narrow this" and then refuse. The control is not
   rendered, and the administrator's forms carry no scope parameter — which is
   why `TestDashboardFilterFormsCarryEveryControl` now runs twice, once per
   role, with a different expected set of controls.
3. **The scope is the dashboard's, not the whole interface's.** `/requests`
   stays the unfiltered report of everything that ever happened. A filter
   there without a control would be a surprise, and a control there is Phase
   1c's business if it is wanted at all.

### 3.3 Request detail page — proposed hierarchy

Before Phase 1c: a `<dl class="facts">` of 14 rows mixing story and machinery
(workflow ID, infohash, content path), then candidates, then a manual-release
form, then the events table.

Proposed order, with the *first screen* being the story:

**1. Header** — poster, title, state pill, **age** («4 мес. 12 дн.»), and
**«Запросил»** — the username of whoever asked for it.

«Запросил» is shipped from `requests.requested_by`, resolved to a username.
The original assumption that a create event held it was false: creation writes
no such event. Migration `00012_requested_by.sql` added the column and rebuilt
the projection view, retaining the three ongoing-season columns.

**1a. Watchers** — the existing «Кто смотрит» list, kept. In a shared install it
is the closest thing the product has to "who cares about this".

**2. Статус** — one plain-Russian sentence generated from state + attempt count +
`NextRunAt`:

| State | Generated line |
|---|---|
| `searching` | «Ищет релиз с русским звуком. Проверено 47 раз за 4 месяца. Следующая проверка через 6 ч.» |
| `downloading` | «Качает 12,4 ГБ из 14,1 ГБ (88%).» |
| `manual` | «Индексатор вернул 3 кандидата, авто-подбор не выбрал ни один. Нужен ваш выбор.» |
| `ongoing` | «Скачано 5 из 8 серий. Ждёт остальные — сезон ещё выходит.» |
| `failed` | «Поиск остановлен после 3 ошибок индексатора. [Запустить снова]» |

This single element is the highest-value item in the whole plan. It is the
difference between "the bot is doing something" and "the bot has been doing
something for four months".

**3. Требования** — the policy *as applied to this request*: resolution floor,
size range, minimum seeders, required audio tier, excluded sources. The user
currently cannot see why a release was refused, because they cannot see the rule
it violated.

**4. Релиз** (when found) — release name, resolution, **audio tier + studio +
the marker that decided it**, size, seeders, tracker.

**5. Кандидаты** (when `manual`) — the redesigned table (§3.4).

**6. Расширенное** — a `<details>` disclosure, **default collapsed**, holding what
is operator-facing today: workflow ID, infohash, qBittorrent hash, content path,
and the raw events table. Never deleted; one click away. Per-user open/closed
state in `localStorage`, exactly like the sidebar pattern already in
`layout.html`.

### 3.4 The candidate table — the highest-value component

**Current** (`internal/web/views/request.html:79–99`): `# | Релиз | Размер |
Сидеры | Отклонён по | [Скачать это]`. Every row offers the same button
regardless of why the release was refused. A blacklisted release and a perfectly
good one are indistinguishable, and the button invites the user to re-download
something the system deliberately refused.

**Proposed:**

```
Релиз (mono, 2-line clamp) │ Качество │ Аудио │ Размер │ Сидеры │ Причина │ [действие]
```

- **Качество** — resolution + source (`1080p · BluRay`).
- **Аудио** — tier badge (многоголосый / двухголосый / закадровый / только
  субтитры) plus the studio, plus a `title` carrying the deciding marker
  («решено по слову "LostFilm"»).
- **Сидеры** — `0` in danger tone; **`—`** in muted tone for unknown. Never `-1`,
  never a bare `0` for unknown. (The "unknown ≠ zero" rule must be *visible*,
  not just correct in the model.) *Shipped in Phase 0:* the indexer reports `-1`
  when it does not know the count (`internal/indexer/prowlarr.go:217`), and the
  selector treats it as distinct from zero (`internal/release/score.go:313`).
- **Причина** — the Russian label for the rejection code, in plain words:
  «только русские субтитры», «ниже 1080p», «в чёрном списке», «это одна серия, а
  запрошен сезон».
- **Действие** — **conditional**:
  - acceptable but not chosen → «Скачать это» (primary)
  - rejected for a *soft* reason the user may override (too few seeders, too
    large, unknown quality) → «Скачать всё равно» (secondary), reason shown inline
  - rejected for a *hard* reason (blacklisted, wrong season, single episode,
    excluded source) → **no action button**, row de-emphasised

Offering "Скачать это" on a blacklisted release is a defect, not a feature. The
action set must follow the reason.

> **Phase 1 must include a slice of this — see §7.3.** Phase 1 promotes
> `manual` requests into a visible bucket, and the current candidate table
> offers "Скачать это" on every row regardless of why the release was refused.
> Promoting the bucket without fixing the table would surface a
> blacklisted release as a one-click download. The minimum slice is: Russian
> reason labels, and no action button on a hard rejection. **Shipped in
> Phase 1b** — plus one thing the plan did not ask for: the endpoint refuses
> the pick as well, because a hidden button is not a rule.
>
> **The rest of the table shipped in Phase 2**: the quality column, the audio
> badge, the reason with the release's own numbers, the list ordered best
> first, and the card layout on a phone. See "Phase 2" in §7.

### 3.5 Reason vocabulary — the full mapping

The codes stay in the event log, machine-readable and unchanged. The UI adds a
translation layer, defined as a Go map *adjacent to the reason constants* so it
cannot drift.

| Code | UI label (RU) | Tone | Action allowed? |
|---|---|---|---|
| `excluded_source` | «источник исключён (CAM/TS/SCR)» | danger | no |
| `resolution_too_low` | «ниже минимального разрешения» | danger | no |
| `unknown_quality` | «не удалось определить разрешение» | warn | yes (override) |
| `too_few_seeders` | «мало сидеров» | warn | yes (override) |
| `too_small` | «слишком маленький» | warn | yes (override) |
| `too_large` | «слишком большой» | warn | yes (override) |
| `implausible_size` | «размер не соответствует качеству» | danger | no |
| `blacklisted` | «в чёрном списке» | danger | no |
| `no_russian_audio` | «нет русской дорожки» | danger | no |
| `subtitles_only` | «только русские субтитры» | danger | no |
| `wrong_season` | «другой сезон» | danger | no |
| `single_episode` | «это одна серия, а запрошен сезон» | danger | no |
| `wrong_year` | «другой год» | danger | no |
| `no_download_link` | «нет ссылки на загрузку» | warn | no |
| `not_an_update` | «не обновление текущего пака» | neutral | no |

### 3.6 The five journeys

**J1 — Add a film (happy path).** Search → result cards → confirm page → POST →
dashboard shows it under «Ищет». The confirm page already carries the two-year
promise in body copy (`internal/web/views/confirm.html:22`); the redesign moves
that promise to a first-class element, not a paragraph.

**J2 — Add a not-yet-released film (flagship).** Same path, but the request is
born parked. The moment after creation must say: *«Ищем. Проверяем каждый день,
до двух лет. Вы можете забыть — мы напишем.»* The dashboard's «Ищет» bucket
with age + next check is where this request lives for months.

**J3 — Add a season of an airing series.** Search → season picker
(`internal/web/views/seasons.html` already marks «ещё не вышел») → confirm →
request → eventually a partial pack → the request moves to `ongoing` and into
«Ждёт выбора» with «5 из 8 серий» (episode count read from the *pack's name*, per
`AGENTS.md` — never from the torrent's file count).

**J4 — Resolve a `manual` request.** «Ждёт выбора» → request → candidate table →
pick → `downloading`. The candidate table (§3.4) *is* this journey; if it's
wrong, the journey has no value.

**J5 — Cancel.** Request → cancel → **confirmation stating the consequence** →
cancelled. The consequence depends on the request: a cancelled download takes
its files off the disk, a request waiting for the rest of a season keeps the
episodes it already put in Plex. The confirmation says which one it is.

**Deliberately not in scope:** a browse/recommendations surface, a Kanban view
of states, a "similar titles" feature. The brief mentions "random media", but
the request is a *named* title; discovery is a different product.

---

## 4. Design System & Token Strategy

### 4.1 The constraint that shapes everything

There is **no build step**. Pico is vendored, `style.css` is a hand-written
override layer, templates are `html/template`. Therefore:

> **There will be no design system in the Figma/Storybook sense.** The design
> system *is* three layers of code: CSS custom properties → template partials →
> Go helper functions. Anything requiring npm, a bundler, or a component-demo
> app is out of reach by design, and a plan that assumes otherwise is fiction.

### 4.2 Layer 1 — Tokens

A single `:root` block in `style.css` that **overrides Pico's variables rather
than fighting them**. Pico v2.0.6 already exposes `--pico-primary`,
`--pico-card-background-color`, `--pico-muted-color`, `--pico-ins-color`,
`--pico-del-color`, `--pico-border-radius`. Today `style.css` hardcodes
fallbacks (`#2c7a4b`, `#c1272d`, `#c98300`) in eight places — consolidate them.

**Pico has no warning token.** `--pico-contrast-warning` is not defined
anywhere in `pico.min.css` v2.0.6, which is why the old `.flash.warn` rule
carried a literal `#c98300` fallback — a colour that never followed the theme.
The warning colour is therefore the one token this project owns, and it needs a
value per theme. Both values clear 4.5:1 against their own background.

```css
:root {
  /* semantic, not literal — every consumer names the role, not the colour */
  --m-bg: var(--pico-background-color);
  --m-surface: var(--pico-card-background-color);
  --m-border: var(--pico-muted-border-color);
  --m-text: var(--pico-color);
  --m-text-muted: var(--pico-muted-color);
  --m-accent: var(--pico-primary);
  --m-ok: var(--pico-ins-color);
  --m-danger: var(--pico-del-color);

  /* ours: Pico has no warning token. The dark value comes next, in both
     the media query and the [data-theme] rule, mirroring how Pico itself
     switches. */
  --m-warn: #8a5a00;

  /* the progress bar that does not currently exist */
  --m-bar-track: var(--pico-muted-border-color);
  --m-bar-fill: var(--pico-primary);

  /* spacing — 4px base */
  --m-space-1: .25rem; --m-space-2: .5rem; --m-space-3: .75rem;
  --m-space-4: 1rem;   --m-space-5: 1.5rem; --m-space-6: 2rem;

  --m-radius: var(--pico-border-radius);
  --m-radius-pill: 999px;
}
```

**Dark mode is nearly free, and it is free from Phase 0 (D3).** Pico v2.0.6
already ships `prefers-color-scheme` support *and* a `data-theme` attribute.
Because every token above maps to a `--pico-*` variable rather than a literal
colour, the whole interface follows the OS preference the moment the token
layer lands — no toggle, no extra CSS, no second palette to keep in sync. The
only new code is the manual override.

The tri-state toggle (auto / light / dark) is **pulled forward from Phase 4 into
Phase 1**: it is one attribute plus one `localStorage` entry — the same
persistence pattern the sidebar already uses (`mediad.sidebar` →
`mediad.theme`) — and the decision to ship all three states has now been taken.
It lives in Settings, next to the other account preferences.

The consequence for Phase 0: **dark mode is not a later polish pass, it is a
constraint on the token layer.** A literal hex anywhere in the CSS is a bug that
ships broken in dark mode, and a component whose colours were only checked in
light has not been checked at all.

**The state colour map** — completing the missing three.

The principle is **colour marks responsibility, not phase**. Ten states cannot
each have their own colour without the legend becoming the interface, and phase
is already carried by the bucket a row sits in (Качается / Ищет / Ждёт выбора)
and by the pill's label. So there are four tones:

| Tone | Meaning | States |
|---|---|---|
| **accent** | the machine is on it | `searching`, `adding`, `downloading`, `ongoing` |
| **warn** | **a human is required** | `manual` |
| **ok** | finished | `completed` |
| **danger** | broken | `failed` |
| **muted** | closed | `pending`, `canceled`, `skipped` |

```css
.state.searching   { color: var(--m-accent); border-color: var(--m-accent); }
.state.adding      { color: var(--m-accent); border-color: var(--m-accent); }
.state.manual      { color: var(--m-warn);   border-color: var(--m-warn); }
```

`manual` is the only state that gets the warning tone, and it gets it because it
is the only one that blocks on a person. `ongoing` keeps the accent (per the
existing comment at `style.css:77`), `completed` ok, `failed` danger,
`canceled`/`skipped`/`pending` muted.

**Note on contrast.** Pico v2.0.6 exposes no "info" text colour —
`--pico-secondary-background` is a *background* token (#525f7a light, #5d6b89
dark), and used as a text colour it reaches only ≈3.3:1 against Pico's dark
background, below the 4.5:1 floor. Every state tone above is therefore drawn
from a token that is already a foreground colour.

### 4.3 Layer 2 — Template partials

`internal/web/views/fragments.html` already exists for shared templates (that's
how `{{template "progress"}}` resolves across pages). Extend it:

| Partial | Contract |
|---|---|
| `{{template "state_pill" .}}` | state → class + Russian label + `title` with the raw code (operator detail) |
| `{{template "progress" .}}` | exists — add `role="progressbar"`, `aria-valuenow/min/max`, and **the missing CSS** |
| `{{template "reason" .}}` | reason code → Russian label + tone class |
| `{{template "audio_badge" .}}` | tier → badge + studio + marker tooltip |
| `{{template "empty" .}}` | one empty-state component: title, explanation, primary action, optional secondary. Replaces six ad-hoc `<p class="muted">` |
| `{{template "request_row" .}}` | shared by the desktop table and the mobile card — so the two cannot diverge |
| `{{template "age" .}}` / `{{template "next_check" .}}` | duration rendering, Russian plurals |

**Every new component ships with its CSS in the same commit.** The `.bar`
episode — a template emitting markup that no stylesheet defines — is exactly
the failure mode that a component contract prevents.

### 4.4 Layer 3 — Go helpers (`internal/web/vocab.go`)

Shipped as a dedicated file rather than inside `views.go`: the vocabulary is a
distinct concern from the view models, and a file of its own is the thing a
person opens when a word on a page is wrong.

```go
stateLabel(s storage.State) string        // "ищет", "качает", "ждёт выбора"
stateClass(s storage.State) string        // "searching", "manual", …
reasonLabel(stored string) string         // "только русские субтитры"
reasonTone(stored string) string          // "danger" | "warn" | "neutral"
reasonAllowsOverride(stored string) bool  // false for a refusal on principle
audioLabel(t release.AudioTier) string    // "многоголосый", "закадровый", …
seeders(n int) string                     // "—", "0", "12"
seedersUnknown(n int) bool                // true when the count is missing
durationHuman(d time.Duration) string     // "4 месяца 12 дней"
relativeTime(t time.Time) string          // "через 6 часов", "2 часа назад"
pluralForm(n int) int                     // which of the three Russian forms
```

The reason helpers take the **stored** string rather than a `release.Reason`,
because what is stored is `code + ": " + detail` and the code is recovered by
splitting on the first `": "`. A map keyed on the codes themselves is what
keeps the translation from drifting.

**Russian pluralisation is not a detail — it's a correctness issue.**
`1 час`, `2 часа`, `5 часов`; `1 минута`, `2 минуты`, `5 минут`. A naive
`fmt.Sprintf("%d ч", n)` is wrong and a Russian reader notices immediately. The
implementation splits the *form* from the *noun*: `pluralForm` picks one of
three, and a small table of nouns carries each one's nominative and
accusative singular — because "осталось 1 минуту" and "1 минута" are different
words and the copy needs both. Unit-tested against a table of cases (1, 2, 5,
11, 12, 14, 21, 22, 25, 101, 111).

### 4.5 Typography

Keep Pico's system stack; define only the sizes the app actually uses:

```
--m-text-xs: .75rem    table micro-labels, reason column
--m-text-sm: .85rem    secondary metadata
--m-text-base: 1rem
--m-text-lg: 1.15rem  h2
--m-text-xl: 1.5rem   h1
--m-text-2xl: 2rem    page title (mobile)
```

**Cyrillic-specific rule:** Russian release names are long and inflect freely —
«Многоголосый закадровый релиз» is ~33 characters where English would be ~22.
Release names get `--m-text-xs`, `line-clamp: 2`, `word-break: break-word`, and
**never** `white-space: nowrap`. A real multi-voice release name runs to ~120
characters; the table must survive it.

### 4.6 Token strategy rules

1. No hardcoded hex in a template or a Go file — ever. (Eight currently exist in
   `style.css` fallbacks.)
2. A token is added **only with a consuming component**. No speculative token
   layer.
3. Every token has a dark-mode counterpart, verified by toggling `data-theme` —
   not by reading the CSS.

---

## 5. Responsive & Mobile-First Strategy

### 5.1 Breakpoints

```
xs   0–479 phone
sm   480–767   large phone / small tablet
md   768–1023  tablet — sidebar becomes a column
lg   1024–1439 laptop
xl   1440+     desktop
```

**768px stays exactly where it is.** It's the sidebar drawer threshold
(`internal/web/views/layout.html` JS + `style.css:440+`), and moving it would
desynchronise the CSS and the JS that toggles `aria-hidden` against
`matchMedia('(max-width: 768px)')`. The new breakpoints are additive.

### 5.2 The table problem

The dashboard's five-column table on a 375 px viewport is the worst surface in
the product. But **horizontally-scrolling tables are not the fix** — they hide
the state column, which is the thing the user came for.

**Recommended approach: the `data-label` responsive-table pattern.** One DOM,
CSS-only transformation:

```html
<td data-label="Состояние"><span class="state searching">ищет</span></td>
```

```css
@media (max-width: 767px) {
  table, tbody, tr, td { display: block; width: 100%; }
  tr { padding: var(--m-space-3) 0; border-bottom: 1px solid var(--m-border); }
  td { display: grid; grid-template-columns: 7rem 1fr; gap: var(--m-space-2); }
  td::before { content: attr(data-label); color: var(--m-text-muted); font-size: var(--m-text-xs); }
}
```

**Why not Pico's `role="table"` pattern:** it relies on `display: block` on table
elements, which removes table semantics in Safari/VoiceOver. The mitigation is
to add explicit `role="table"` / `role="row"` / `role="cell"` to the elements —
but that means hand-writing ARIA that must then be kept in sync. **Decision to
make explicit:** either accept degraded table semantics on mobile, or add the
roles. Recommended: add the roles; the DOM order is unchanged, so the reading
order is still correct — only the *visual* order changes, which is safe because
the DOM order is already the logical order.

**Why not render both a table and a card list server-side:** it duplicates the
data in the DOM, doubles the template work, and duplicates every screen-reader
announcement. One DOM, one rendering mode, chosen by CSS.

### 5.3 Per-surface adaptation rules — implemented through Phase 3

| Surface | Wide content | Narrow content |
|---|---|---|
| Dashboard request rows | 5-column table | Card: 48×72 poster, title, state pill, one line of status, 44px action |
| Candidate table | 7 columns | Card: release name (2-line clamp), quality + audio badges, reason, action |
| Events table | 4 columns | Vertical timeline — the `detail` field is free text and belongs in a timeline, not a table |
| Filter pills | Wrapped row | Horizontally scrollable row, `scroll-snap`, edge fade |
| Filter selects | Inline | Full-width stacked |
| Request header | Poster 9rem beside title | Poster above title, title at `--m-text-2xl` |
| Poster source | Existing `w342` / `w500` fallback | `<picture>` selects `w185` at ≤767px through the same proxy |

The table mode now follows **available content width** (`@container
(max-width: 60rem)` on `main`), rather than just viewport width. Browser checks
found that the candidate table overflowed at both 768 and 1024 px, and an open
sidebar also reduces the usable width on a large screen. The sidebar's own
768 px threshold is unchanged. Header/filter/touch adaptations apply at ≤768 px.

### 5.4 Touch targets — implemented in Phase 3

Compact link actions have a 32 px minimum on desktop and a 44 px minimum
on mobile. Icon controls keep their small glyph inside a 44×44 target; the
notification checkbox has a wrapping 44×44 label. Pills, theme/notification
options, tabs and disclosures also have 44 px mobile targets.

A shared `:focus-visible` outline and label `:focus-within` outlines preserve
keyboard focus visibility. The native checkbox remains visible.

### 5.5 The sidebar (unchanged, deliberately)

Keep: two states (`open` / `icons`), `icons` default, `localStorage` under
`mediad.sidebar`, mobile drawer at ≤768 px, the sub-nav hidden in `icons` mode,
`aria-hidden` following visual availability.

This decision is recorded in `AGENTS.md` as settled and was the subject of a real
regression. **No proposal in this plan touches it.**

### 5.6 Polling on mobile

The dashboard polls every 3 s and replaces the table's `innerHTML`. On a phone,
where the user is scrolling that table, a full replacement three times a second
is worse than on a desktop. Two changes in Phase 5: pause polling when
`document.hidden`, and slow it when the pointer is over the table.

---

## 6. Accessibility & Edge-Case Plan

**Target: WCAG 2.2 AA.** Below, each item is either a verified defect in the
current code or a concrete requirement.

### 6.1 Perceivable

- [ ] **Contrast** — verify ≥4.5:1 for state-pill text in all 10 states, in
  light and dark. The muted color at `0.85em` on small text is the likeliest
  failure.
- [x] **Never colour alone** — state pills carry Russian labels (Phase 0).
- [ ] `prefers-reduced-motion` — the only motion today is the htmx swap; any
  skeleton shimmer or bar animation must respect it.
- [x] `prefers-color-scheme` — Pico already supports it; Phase 0's token layer
  makes it work, and Phase 1c adds the explicit auto/light/dark toggle (D3).

### 6.2 Operable

- [x] **Focus styling** — a global `:focus-visible` ring and label
  `:focus-within` rings use the accent token (Phase 3). Manual screen-reader
  and exhaustive keyboard navigation checks remain open.
  *(Corrected: the pills do **not** hide their checkbox — `.pill
  input[type="checkbox"]` only sets a margin — so the native ring survives
  there and needs no replacement.)*
- [x] **Mobile control targets ≥44×44** — link actions, icon buttons, tabs,
  labelled options and disclosures checked on fixtures in Chrome (Phase 3).
- [x] **Sidebar visibility** — no hard-coded `aria-hidden` on the navigation;
  JS follows visual availability on desktop/mobile (`views/layout.html`).
- [ ] **Focus preservation on poll.** Phase 1a preserves the bucket disclosures,
  but still replaces tables and counts every 3 s through OOB swaps. Verify
  keyboard focus and preserve it when a table contains the focused link.
  Row-level replacement alone does not guarantee focus survives a changed row.
- [ ] Escape closes the mobile drawer; the drawer traps focus while open.
- [ ] The notification page's «Выбрать все» / «Снять выбор» are `onclick`-only —
  no-JS users get dead buttons. Make them `<label for>`-driven or add a
  `<noscript>` path (the repo already does this correctly in
  `internal/web/views/requests.html:13`).

### 6.3 Understandable

- [x] `lang="ru"` — present (`internal/web/views/layout.html`).
- [x] **Russian state labels** — shipped in Phase 0.
- [x] **Cancellation states its consequence** — confirmation and flash follow
  the state: `downloading` deletes files, `ongoing` keeps files, other states
  have nothing to delete. GET confirms; POST performs the cancellation.

### 6.4 Robust

- [ ] **Status messages (4.1.3).** On a state transition, announce it in an
  `aria-live="polite"` region — but not the whole table. Debounce, and announce
  only meaningful transitions («Загрузка завершена: …»).
- [x] **`role="progressbar"`** with `aria-valuenow` / `aria-valuemin` /
  `aria-valuemax` and an accessible name on the progress element.
- [x] Explicit table roles on dashboard and candidate cards (§5.2). Events
  use the same roles in Phase 3's timeline layout.
- [x] **Automated regression guard** — `TestRenderedPagesAreAccessible` in
  `internal/web/a11y_test.go` implements the structural checks below.
  Original specification: The repo already proves that a rendered
  page can be correct to the eye and wrong to the parser
  (`TestNoPageNestsAForm`). Extend that pattern: a Go test that renders every
  page and asserts, using `golang.org/x/net/html`:
  - every `<img>` has an `alt` attribute
  - every `<button>` and `<a>` has an accessible name
  - every `<input>` has a label
  - every `<table>` has a `<th>` or `<caption>`
  - no focusable element has an `aria-hidden` ancestor
  - every `role="progressbar"` carries `aria-valuenow`

  This runs in `make test` and is a **hard gate**. It catches the structural 80%
  of accessibility defects, which is the only way a solo designer keeps a11y from
  regressing between phases. It is also the *only* handoff artifact a designer
  can guarantee lands.

### 6.5 Edge cases and empty states

The current six empty states (`dashboard.html:123,205`;
`notifications.html:30`; `search.html:28`; `requests.html:35`;
`seasons.html:24`) are one line of muted text with no way forward. Required
replacements:

| Situation | Content |
|---|---|
| No requests at all | One sentence on what the product does + a search field + «как это работает» |
| No requests match a filter | «Под фильтр ничего не подошло» + «сбросить фильтр» — the current copy implies there are no requests at all |
| Search returns nothing | Offer the original title, a shorter query, and a link to search TMDB manually |
| `manual`, zero candidates | «Индексатор ничего не вернул» + the manual magnet/.torrent form inline |
| `manual`, with candidates | The redesigned candidate table (§3.4) |
| Download stalled | A named state with last progress + retry — not a generic error |
| Indexer down | **Not an error.** «Индексатор недоступен, поиск продолжится» — the request is healthy, the dependency is not |
| Poster missing | Keep «постер недоступен», use a consistent silhouette |
| Long release name | 2-line clamp + full name in `title` |
| Seeders unknown vs zero | «—» muted vs «0» danger — **the rule made visible** |
| Two-year expiry | «Истёк срок поиска» + «начать заново» |
| Partial season pack | «5 из 8 серий», counted from the *pack's name* per `AGENTS.md` |
| Plex already has it | Shouldn't happen — but if it does: «уже есть в Plex» + close the request |

### 6.6 Error recovery rules

1. Every message states **what failed**, **why**, and **what to do**. Not
   «ошибка».
2. **Transient, self-healing failures are statuses, not errors.** An indexer
   being down is not a broken request. Distinguishing these is the difference
   between "your request failed" and "we're still looking" — and the whole
   product thesis lives in that distinction.
3. **Destructive actions confirm with their consequence.** «Отменить запрос?
   Торрент и уже скачанные файлы будут удалены с диска.» — and, for a request
   watching for the rest of a season, «Торрент будет удалён, но уже скачанные
   серии останутся на диске.» The wording is generated from the request's
   state, because the consequence is not the same for both.

---

## 7. Phased Rollout & Handoff Milestones

**Implemented: Phases 0, 1a, 1b, 1c, 2 and 3.** D4 was the original Phase 0–1
window; Phase 2 subsequently shipped as PR #12. Phases 4–5 remain deferred
in §7.2. Week numbers below are the original sequencing, not current deadlines.

**Every phase ships.** Each ends with something testable in the running app and
gated by `make test` / `make check-ci`.

The organising principle for this window is **Phase 1 must not ship a
regression**. Two of the three slices pulled forward from later phases (§7.3)
are there for exactly that reason: Phase 1 promotes things to prominence, and
promoting a broken thing is worse than not promoting it.

---

### Phase 0 — Foundation (Week 1) — **shipped**

The foundation phase. Nothing here changes what a user *sees* — except the
state pills, which get both new colours and Russian labels.

**Tokens**

- `:root` token block built **entirely on `--pico-*` variables** — no literal
  hex anywhere. Consolidate the eight hardcoded fallbacks currently in
  `style.css` (`#2c7a4b`, `#c1272d`, `#c98300`).
- The state colour map completing `searching`, `adding` and `manual` (§4.2).
- The `--m-bar-track` / `--m-bar-fill` pair for the progress bar that does not
  currently exist.
- **Verify the dark theme after this commit, not later.** `prefers-color-scheme`
  must flip the whole interface. A token that was checked only in light has not
  been checked at all (D3).

**Vocabulary (Go, `internal/web/vocab.go`)**

- `stateLabel` / `stateClass` — Russian label per state, plus the CSS class.
- `reasonLabel` / `reasonTone` / `reasonAllowsOverride` — Russian label and
  tone per reason code, from the §3.5 table.
- `audioLabel` — the tier names the user actually cares about («многоголосый»,
  «закадровый», «только субтитры»).
- `pluralForm` — Russian pluralisation. Table-tested against 1, 2, 5, 11, 21,
  22, 25, 101. **No `fmt.Sprintf("%d ч", n)` anywhere.**
- `durationHuman`, `relativeTime` — «4 месяца 12 дней», «через 6 часов».

**The guard**

- The a11y-lint Go test, added **failing first** (§6.4), so the guard exists
  before the changes it guards. It runs in `make test` as a hard gate.

> **Ship:** state pills in Russian with four tones; a token layer that follows
> the OS dark-mode preference; a vocabulary map; a test that fails.

#### What Phase 0 actually shipped

The list above is what was intended. What happened differs in a few places, and
the differences matter because they change what Phase 1 owes.

| Planned | Shipped | Why it differs |
|---|---|---|
| Token layer on `--pico-*` only | Same, plus `--m-warn` with an explicit light and dark value | **Pico v2.0.6 has no warning token at all** — `--pico-contrast-warning` is not in the file, so the old `.flash.warn` was always falling back to a fixed `#c98300`, a colour that did not follow the theme. The warning colour is now ours, with a value per theme |
| The progress-bar CSS in Phase 1a | Shipped in Phase 0 | A token with no consumer is a token nobody keeps. The bar and its tokens landed together — and the bar is fixed: it rendered as nothing, because `<i>` is inline and an inline element ignores a width |
| `plural` as one helper | `pluralForm` plus a `ruNoun` table | The accusative singular differs from the nominative in one form only, and the difference is exactly where the copy needs it: "осталось 1 минуту", "1 минуту назад" |
| The a11y-lint test added failing-first | It failed on three real defects within one run | The dashboard's filter search box, the search page's search box and the notifications' row checkboxes had no accessible name. All three are fixed with `aria-label` |
| — | A second guard: `TestColoursLiveInTheTokenLayer` | The token layer is only worth having if it stays the only place a literal colour lives. The test scans `style.css` and the templates and fails on a colour outside a custom property |
| — | `internal/release.AllReasons()` | The completeness test needs the closed set enumerated. A package that defines a closed set of codes should expose it |
| — | The next-check time no longer reads "только что" | `whenText` called `since()` on a *future* moment, and `since` renders anything under a minute as "только что". A request due to look again in six hours was showing a last-run time. This was the plan's headline datum, and it was wrong on screen |

**Two corrections to this document**, both found by reading the code rather
than the plan:

- §6.2 said the state pills hide their checkbox and lose the native focus
  ring. They do not — `.pill input[type="checkbox"]` only sets a margin, and
  the checkbox is visible inside the pill. No fix is needed there.
- §3.5 said the rejection reason renders as a raw code. It renders as
  `code: english-detail` — `too_few_seeders: 2 seeders, minimum is 3`. The
  label map splits on the first `": "`, and the detail is preserved whole in
  the cell's tooltip. **The numbers are not yet visible in the cell**, which
  was initially assigned to Phase 1b and assumed to need stored parameters.
  Phase 2 resolved it using the release's own values without a migration.
> Commit: `design: tokens and vocabulary`.

---

### Phase 1 — MVP: make the horizon visible (Weeks 2–4)

*The highest-value phase; it ships on its own if the rest never happens.* Split
into three sub-phases so it can be cut at a week boundary if it runs long.

#### Phase 1a — The horizon (Week 2) — **shipped**

- Dashboard restructured into **Качается / Ищет / Ждёт выбора** (§3.2).
- New row fields: **age**, **next check** (`NextRunAt`), attempt count.
- The generated «Статус» one-liner on the request page (§3.3).
- The **progress bar CSS** — the thing that currently does not exist. This is
  also a bug fix: today the bar renders nothing at all.

> **Success metric:** in a follow-up session, ≥80% of participants correctly
> state what the bot is doing right now (T3).

#### What Phase 1a actually shipped

| Planned | Shipped | Why it differs |
|---|---|---|
| Three buckets on the dashboard | Shipped, with **one query per bucket** | A single query with a limit lets a busy bucket starve a quiet one: fifty downloads in progress and nothing from the parking list |
| The poll replaces the table | The poll replaces **the tables and the counts, out of band**, and leaves the blocks alone | A poll that replaced the `<details>` blocks would throw away whether the reader has them open. htmx's `hx-swap-oob` swaps the contents in place; the requester carries `hx-swap="none"` |
| Age, next check, attempt count as row fields | Shipped, and each bucket carries **only the columns that mean something there** | A transfer has no age and no next check — there is nothing to wait for. One table template, switched on the bucket, so a table and its columns cannot drift apart |
| The generated «Статус» line | Shipped, one sentence per state | — |
| "По ожиданию" as a sort inside «Ищет» | Shipped as a **global** sort (`created_at ASC`) | The buckets share one query's sort. A per-bucket sort would need a second query, and the plan's own point — a list of the things you asked for a year ago — is served by the global one |
| — | The period filter **no longer touches the buckets** | It always did, and the fragment's own comment said it should not: "Since is intentionally dropped here: it is a window for the Recent block, not the Active one." A request that has been searching for four months must not disappear because somebody asked for the last day |
| — | The decimal separator is a **comma** | `humanBytes` and the progress text wrote "21.0 ГиБ" with a dot — the machine's separator, in a Russian interface. A size that says "21.0 ГиБ" reads as a machine talking |

**One thing the plan did not ask for.** The status line needed a sentence for
a request that failed, and the sentence needed a count: «Поиск остановлен
после 3 ошибок подряд.» That is the **genitive** plural — "после 3 ошибок",
not "после 3 ошибки" — and the vocabulary had no case for it. `ruNoun` grew a
`gen` field and a `countG`, because the noun table is where a case belongs,
and the alternative was a sentence that was wrong in a way only a Russian
speaker would notice.

> Commit: `web: the horizon — three buckets and a status line`.

#### Phase 1b — Safety slices (Week 3) — **shipped**

Everything here exists because **Phase 1a would otherwise make things worse**.
None of it is additive; each one removes a risk that 1a introduces.

- **Candidate table, minimum slice** (§7.3): Russian reason labels, and no
  action button on a hard rejection. Without this, promoting `manual` requests
  into a visible bucket surfaces a blacklisted release as a one-click download.
- **Dashboard table → card on mobile** (§5.2): the `data-label` transformation
  for the dashboard table only, with explicit `role="table"` / `role="row"` /
  `role="cell"`. Phase 1a adds columns; on a 375 px screen the table is already
  the worst surface in the product and would get worse.
- **Cancel confirmation** (§6.6): a small confirm page stating the consequence for the current
  state: downloading deletes files, ongoing keeps files, other states have
  nothing to delete. Implemented as a GET → POST pair, which also
  sidesteps the form-in-form constraint entirely — the cancel form currently
  lives inside the dashboard table.

#### What Phase 1b actually shipped

| Planned | Shipped | Why it differs |
|---|---|---|
| Russian reason labels on the candidate table | Already in Phase 0's vocabulary | The labels were never the hard part; the *action* was |
| No action button on a hard rejection | Shipped, **and the endpoint refuses the pick too** | A hidden button is not a rule: one POST would have downloaded the camrip the selector refused. The button and the endpoint now ask the same question — `candidatePick` / `canPick` in `internal/web/vocab.go` — and a refused row is dimmed |
| Dashboard table → card on mobile | Shipped for **both** dashboard tables | The «Недавно» table is on the same page and was the same width. The card layout is `table.table-cards` with `data-label` on every cell and explicit `role="table"` / `role="row"` / `role="cell"` / `role="columnheader"`, because `display: block` takes the table semantics away in Safari and VoiceOver |
| Cancel confirmation | Shipped as a GET → POST pair — `GET /requests/{id}/cancel` states the consequence, `POST` does it | See the correction below: **the copy this plan specified was wrong** |
| — | The dashboard's table now holds **no form at all** | The cancel action became a link. The form-in-form constraint is not "handled" here, it is gone: there is nothing left to swallow |

**One correction to this document, found by reading the workflow rather than
the plan.** This plan said, in four places, that cancelling a request does not
delete already-downloaded files. The code says the opposite:

```go
// internal/request/workflow.go:1467 — cancelRequest
DeleteFiles: !state.Monitoring,
```

A cancelled download has its files deleted, because it was never filed into
the library — "a cancelled download has not been filed into the library, so
there is nothing here the user is watching". The **exception** is a request
watching for the rest of a season (`state.Monitoring`, which is what the
`ongoing` state is): its pack is finished and already in Plex, and deleting
those files would take the episodes somebody is watching off the disk.

So the confirmation page states one of three consequences, generated from the
request's state, and the flash after the cancellation says the same thing in
one line. The plan's blanket promise would have been the more comfortable
sentence and the wrong one — and it would have been wrong in the direction
that costs the user their files.

> Commit: `web: the safety slices of phase 1b`.

#### Phase 1c — Shared installation and theme (Week 4) — **shipped**

Everything in this sub-phase follows from D1 (several people) and D3 (theme).

- **Scope filter «Все / Мои»** (§3.2.1, §3.2.2) — **shipped early**, as D5:
  `RequestFilter.WatcherUserID` already existed in storage and was used only
  by Telegram; the web now sets it. The scope control goes into one of the
  dashboard's two existing GET forms as a radio group, and the other form
  mirrors it — the `TestDashboardFilterFormsCarryEveryControl` test enforces
  the mirroring, and now runs once per role, because the administrator is
  offered no control at all.
- **«Запросил»** on the request page — the requester's name.
- **«Расширенное» disclosure** — workflow ID, qBittorrent hash, content path
  and the events table move behind one collapsed `<details>`, state
  remembered in `localStorage` like the sidebar.
- **«Требования» panel** — the policy as applied to this request: resolution
  floor, size range, minimum seeders, required audio tier, excluded sources.
- **Theme toggle** (auto / light / dark) in Settings, reusing the sidebar's
  persistence pattern (§4.2).

#### What Phase 1c actually shipped

| Planned | Shipped | Why it differs |
|---|---|---|
| «Запросил» — the requester's username, promoted out of the create event's raw JSON detail | Shipped, and it needed a **migration** | The plan was wrong about where the name was. The create path writes **no event at all**, and the retry path put the name in the retry event's detail — so for every request but a retried one there was nothing to promote. `00012_requested_by.sql` adds the column, and the interface resolves the id to a name at render time |
| The policy's size range | The cap that applies to **this** request | A film and a season are capped differently, and the panel shows the one that applies — the same number the selector uses, through `release.MaxSizeGBFor`, because a second copy of the rule would drift |
| Theme toggle in Settings | Shipped, and it is **client-side only** | The theme is a preference of a browser, not of an account: two people sharing a machine may want different ones. It is applied before the first paint, so a dark-theme reader never sees a light page first |
| — | The request page's story and machinery are now **separate lists** | The facts list mixed the two: the workflow id and the path on disk sat next to the release. The machinery moved behind the disclosure; nothing was deleted |

> Commit: `web: phase 1c — who asked, what it looks for, and the theme`.

---

### Phase 2 — The candidate table (Week 5) — **shipped**

The first phase that is not about the horizon. It is about the one place in
the product where a person decides whether the bot understood them.

- **The table redesigned** (§3.4): `Релиз │ Качество │ Аудио │ Размер │
  Сидеры │ Причина │ действие`.
- **The audio badge** — the tier of the release's Russian audio, the studio
  that decided it, and the word in the name that settled it.
- **The quality column** — resolution and source.
- **The reason with the release's own numbers** — «мало сидеров: 2».
- **The list ordered best first.**
- **The candidate table as a card on a phone** (§5.3).

#### What Phase 2 actually shipped

| Planned | Shipped | Why it differs |
|---|---|---|
| The release facts stored on the candidate row — "a storage change" | **Read out of the name at render time** | The plan called this a storage change, and it is not one. A candidate row already holds the title and the indexer, and `release.Parse` and `release.DetectAudio` are pure functions of exactly those two — so the facts are re-derived with the selector's own rules instead of being kept in a second copy that could drift from them |
| «Причина» — the Russian label | The label **and the release's own numbers** | The threshold a release missed is the policy's, and the policy is one screen above, in «Требования». A threshold stored with the candidate would be a second copy of the rule, and it would go stale the moment the rule changed — a release refused under "three seeders" would go on claiming it was refused under "five" |
| The list ordered by the selector's score | Shipped — **and the refusals are scored now** | Refused releases never reached the scorer: `verdict.Rejected` was appended in the indexer's order, so the rank said nothing about the quality. The storage comment claimed "selector-score order" and the code did not do it |
| The candidate table as a card on a phone (§5.3) | Shipped, and **pulled forward from Phase 3** | Phase 2 made the table two columns wider, and §7.2 had deferred the card layout to Phase 3 — which would have shipped exactly the regression §7.3 exists to prevent |
| — | The episode count of a season pack | The count comes from the pack's name, never from the torrent's file list (`AGENTS.md`). A pack of eight episodes and a single episode are told apart by what the release calls itself, and J3 is exactly that choice |
| — | The audio of a release that says English | A Russian tracker is weak evidence of Russian audio, and no evidence at all when the name says `ENG`. The tooltip says so instead of implying the tracker had vouched for it |
| — | The refusals are ordered **inside the selector**, so the Telegram keyboard follows | The order is the menu's order, and the bot's menu is the same menu |

**Two things the plan did not ask for.** The reason's numbers were the one
piece of Phase 0's debt that Phase 1b left behind; they turned out to need no
storage at all, because the numbers a reader wants are the release's own. And
the season pack's episode count was not on the list — but a person choosing
between a pack of eight episodes and a single episode needs it, and that
choice is the J3 journey.

**One consequence worth knowing.** The Telegram buttons carry the candidate's
rank, so a list replaced between the message and the press points at a
different release. That hazard is not new — the indexer's order varied between
searches too — and Phase 2 did not change its kind. It is recorded here
because Phase 2 changed which release a rank names.

> Commit: `web: phase 2 — the candidate table`.

---

### Phase 3 — Responsive surfaces and touch targets — **implemented**

- Events become a vertical timeline on phones, retaining one DOM and explicit
  table roles. Long paths and event details wrap instead of widening the page.
- Requests, notifications and all three settings tables reuse `table-cards`.
- Table/card mode follows the available content width (60 rem), so both sidebar
  states fit; a viewport-only 768 px rule left tablet candidates overflowing.
- Mobile headers place the poster above the title; facts stack, search/season
  grids can shrink to 320 px, selects stack, state pills scroll horizontally,
  and the invitation input/copy action wraps.
- Mobile controls have 44 px targets, including hamburger/logout, link actions,
  notification selection and labelled options. Focus outlines use existing tokens.
- Search and header posters use `<picture>` with a `w185` mobile source and the
  existing desktop fallback; deployment prefixes and escaped paths survive.

**Validation:** `go test ./internal/web -count=1`; structural table-label/role
and responsive-poster tests in `responsive_test.go`. Headless Chrome checked
17 fixture pages/tab variants across 320 / 375 / 768 / 1024 / 1440 px, both
light/dark and both desktop sidebar states (238 combinations). The fixtures
include long paths/event details and a long invitation URL. No document overflow
or undersized checked mobile control targets remained. Screenshots are recorded
in `docs/ui-patterns.md`. Live services, Safari/VoiceOver and measured contrast
were not checked; this is implementation/layout verification, not a user study.

---

### 7.2 Deferred — the backlog, explicitly not cancelled

| Deferred | From | Why it can wait |
|---|---|---|
| ~~Full candidate table redesign — audio badge, quality column, soft-override actions~~ | Phase 2 | **Shipped in Phase 2.** The soft-override actions were Phase 1b's |
| ~~Responsive tables, touch targets, poster sizes~~ | Phase 3 | **Implemented and fixture-verified in Phase 3.** Screen-reader validation remains open |
| Empty-state system — the six "Пусто." / "Ничего нет." replacements | Phase 4 | No new empty states are introduced by Phase 1 |
| Voice consistency across web and Telegram | Phase 4 | D2 puts `manual` in the web; no Telegram copy changes now |
| Poll robustness — pause on hidden tab, row-level swaps, focus preservation, `aria-live` | Phase 5 | Phase 1a does add rows to the polled table, so focus destruction becomes slightly more likely — worth watching, not worth fixing now |
| Performance — smaller poll payload and image/cache measurements | Phase 5 | Responsive poster sources already landed in Phase 3; payload and measurements remain |
| Second usability round and before/after metrics | Phase 5 | The diary study in §2.3 runs in parallel and gives signal sooner |

### 7.3 The protective slice pulled forward — shipped in Phase 1b

**The candidate table's minimum slice belongs in Phase 1b**, and the reasoning
is short enough to state:

> Before Phase 1b, every candidate row offered «Скачать это», whatever the rejection
> reason. Phase 1a promotes `manual` requests from a row buried in a mixed
> table to a named bucket with its own treatment — which makes that button more
> visible and more tempting. A user who trusts the bucket will press it on a
> `blacklisted` release, because the table gives them no reason not to.

So Phase 1b must deliver: **Russian reason labels** (from the §3.5 map, already
built in Phase 0) and **no action button on a hard rejection**. Everything else
about the redesigned table can wait for Phase 2. **Shipped** — and the endpoint
refuses the pick too, which the plan did not ask for and should have.

---

### Handoff model

There is no design-file handoff — the designer is the implementer, and the
artifacts *are* code:

| Artifact | Location | Contract |
|---|---|---|
| Tokens | `style.css` `:root` | Names roles, never literal colours |
| Components | `internal/web/views/fragments.html` | One partial per component; a comment stating the props in / markup out |
| Copy | Go string maps in `internal/web/views.go` | Russian user-facing copy, not debug strings — **this is also the seam a future i18n layer would attach to** |
| Patterns | `docs/ui-patterns.md` | A screenshot per pattern, written *after* implementation |
| Guard | Go a11y-lint test + the existing template test suite | Any new template must pass |

**Definition of done, per component:**

- [ ] Renders at 320 / 375 / 768 / 1440
- [ ] Renders correctly in light and dark
- [ ] Passes the a11y-lint test
- [ ] Keyboard-reachable, visible focus ring
- [ ] Empty / loading / error states defined
- [ ] No hardcoded hex — tokens only
- [ ] Copy is Russian and lives in the label map
- [ ] Screenshot appended to `docs/ui-patterns.md`

---

## 8. Risks & Mitigation

| # | Risk | Early signal | Mitigation |
|---|---|---|---|
| **R1** | **Scope creep** — a redesign brief on a self-hosted tool with ~5 real users | A ticket that isn't about a state the user can reach | Every item maps to a reachable state. **No feature without a state-machine entry and a user-visible moment.** |
| **R2** | **The plan becomes a wishlist nobody implements** | Phase 1 slips past week 4 | Phase 1 is 3 weeks and ships alone. Later phases are additive, never prerequisites. |
| **R3** | **Design/impl divergence** — the "design system" is CSS + templates, so every token change is a code change | A token with no consumer | Tokens are added only with a consuming component. No speculative layer. |
| **R4** | **Accessibility regresses** — no a11y tooling, no screen-reader pass on every change | The lint test gets skipped | The Go a11y-lint test runs in `make test` as a hard gate; one manual screen-reader pass per phase. |
| **R5** | **Russian plural/grammar bugs** — «2 ч», «1 мес.», «5 мес.» | Wrong forms appearing in the UI | One `plural()` helper, table-tested. No `fmt.Sprintf("%d ч", n)` anywhere. |
| **R6** | **The form-in-form constraint bites again** | A new page needs a filter *and* a row action | Forms are siblings by default; `TestNoPageNestsAForm` enforces it. Document the rule in the layout comment. |
| **R7** | **htmx polling fights the redesign** — the whole table is replaced every 3 s | Rows lose hover/focus; scroll jumps | Move to row-level `hx-swap="outerHTML"` **before** adding any interactive row content. Interactive rows are incompatible with a whole-table swap. |
| **R8** | **Long release names break the layout** | Horizontal scroll on a table | 2-line clamp + `word-break` from day one; test with a real 120-character multi-voice name. |
| **R9** | **The operator/asker split is wrong** — with several people, maybe everyone *is* an operator | Operators complain the machine data is now buried | The «Расширенное» disclosure is one click away and remembers its state. **Never delete the machine view — hide it behind one click.** D1 confirms the split is real: `is_admin` marks the operators, and the rest are askers. |
| **R10** | **Two channels drift** — web says «ждёт вас», Telegram says `manual` | A notification using different words | One shared vocabulary map imported by both `internal/web` and `internal/telegram`. |
| **R11** | **Dark mode breaks** — Pico's dark theme is barely used today | Contrast failures in dark | Theme tokens and the picker shipped in Phases 0/1c; a measured contrast pass in both themes remains open. |
| **R12** | **A two-year promise can't be validated in a short study** | Usability tests say "looks fine" | The 3-week diary and the event-log mining are the only honest validation. Treat the 45-minute test as testing the *representation*, not the *patience*. |
| **R13** | **Performance** — the 3 s poll ships the whole active table every time | Load time grows with request count | Ship only changed rows (R7); cap the active list. |
| **R14** | **i18n pressure** — every string is hardcoded Russian in templates | Someone asks for English | Out of scope. But keep user-facing strings in Go maps, not inline in templates — that one structural decision makes a future i18n layer near-free. |

### 8.1 A consequence of D1 + D2 that this plan does not act on

D1 (several people) and D2 (`manual` stays web-only) interact with a piece of
existing behaviour that the plan deliberately leaves alone.

`AddWatcher` inserts every watcher with `notify_mask = 31`
(`internal/storage/people.go:62`) — every bit set, including the `manual` bit.
But the `NotificationManual` doc comment directly above it
(`internal/storage/people.go:145`) still describes the *original* design:

> "Bit 8 of the watcher notify_mask; the default of 7 (started/completed/failed)
> leaves manual off, which is what a user who is not the request owner probably
> wants — they would see someone else's manual list and have nothing to pick."

Migrations `00008_watcher_manual_mask.sql` and `00011_season_notification.sql`
bumped every existing watcher to the full mask, so the comment no longer
describes the behaviour.

The consequence for a shared installation: **every watcher currently receives
the Telegram "pick a release" message for requests they did not ask for** —
which is the exact situation the comment says should not happen. With N people
and M requests in `manual`, that is N×M messages about other people's release
lists.

**Current correction:** Settings already exposes a per-user Telegram notification
mask (`views/settings.html`, `handleSettings`, `handleNotificationSettings`,
`Store.SetTelegramNotifyMask`). Do not schedule a second settings control.
The stale `NotificationManual` comment remains; verify how account and watcher
masks interact in delivery before proposing any notification behaviour change.

**Original scope decision:** It is a shipped feature (#6), D2 says the
`manual` flow is web-only, and changing the notification mask is outside the
Phase 0–1 window. But it is recorded here because D1 makes it visible, and
because the fix is small and already implied by the code's own intent: a
per-user notification mask in Settings, where a user can turn the `manual` kind
off. The stale comment at `people.go:145` should be corrected at the same time.

### What I would deliberately *not* do

- **Rebuild the navigation.** The sidebar decision is recorded and settled.
- **Add a JS framework or a build step.** Pico + htmx + `html/template` is the
  constraint, and it is sufficient for everything in this plan.
- **Add a Kanban view of the states.** Three time-scale buckets beat eleven
  columns.
- **Add browse/recommendations.** Different product.
- **Replace tables with a client-side grid library.** `data-label` + CSS is
  enough.
- **Rebuild the events table into something prettier** before the a11y-lint
  test exists.

---

## Appendix A — Decisions, resolved

Taken 2026-11-03. Each answer changed something specific in this document.

| # | Question | Answer | What it changed |
|---|---|---|---|
| D1 | Operator / asker split — one person, or a household where someone configures and others just request? | **Several people** | The dashboard becomes a shared wall with a «Все / Мои» scope filter (§3.2.1); the bucket is renamed «Ждёт выбора» because picking is not owner-restricted (§3.2); «Запросил» is promoted to the request header (§3.3); «Расширенное» stays a disclosure, with admin surfaces admin-gated (§7, Phase 1c). |
| D2 | Should `manual` get a Telegram prompt, or stay web-only? | **Web-only** | No Telegram work in this plan; the existing manual notification and candidate buttons (#6) are untouched. One consequence recorded in §8.1 and deliberately not acted on. |
| D3 | Theme — auto/light/dark, or dark only? | **auto / light / dark** | Dark mode becomes a Phase 0 constraint rather than a Phase 4 polish item (§4.2): the token layer must be built entirely on `--pico-*` variables so `prefers-color-scheme` works for free. The tri-state toggle is pulled forward into Phase 1c. |
| D4 | Phase 1 only, or the whole roadmap? | **Phase 0 and Phase 1 only** | §7 is resequenced around four weeks, split 1a / 1b / 1c so it can be cut at a week boundary. Phases 2–5 move to the §7.2 backlog. Two protective slices are pulled forward (§7.3) because Phase 1 would otherwise make things worse. |
| D5 | "The admin must see all releases" — which reading? | **The scope rule** | The administrator's scope is «Все» and cannot be narrowed; everybody else starts on «Мои» and can widen it (§3.2.2). The other reading — a catalogue of every release the installation has seen — is a storage change, not a page, and is **still deferred**: Phase 2 read the release facts out of the name instead of storing them, and the catalogue needs a log of every release ever seen, which the candidate list cannot be. |

**Open, and deliberately not blocking:** whether the §8.1 notification-mask
delivery audit should be scheduled (the settings control already exists), and whether the diary study (§2.3) is worth
recruiting 6 people for — both are answerable later without changing Phase 0–1.

## Appendix B — Historical verification notes

These observations were made before or during the named phase. Pre-Phase-0
claims below describe the old interface, not unresolved defects. The current
status table and §6 take precedence; later notes correct earlier assumptions.

Claims in this plan that were checked against the code rather than assumed:

- `.bar` is defined in neither `pico.min.css` (v2.0.6) nor `style.css`; the
  progress bar renders nothing.
- `.state` CSS covers 7 of 10 states; `searching`, `adding`, `manual` have no
  rule.
- Six empty states exist across the templates; five are one line of muted text.
- `internal/web/views/layout.html` hard-codes `aria-hidden="true"` on the
  sidebar.
- The candidate table renders `{{.Seeders}}` raw, so `-1` (unknown) is visible.
- `NextRunAt` is written by the workflow and read by no template.
- `AudioTier` appears in no template.
- The backoff ladder's longest rung is 24 h (`internal/config/config.go:395`),
  so a parked request is checked at most once a day.

Claims added by the D1–D4 revision:

- `RequestFilter.WatcherUserID` (`internal/storage/requests.go:399`) restricts to
  requests a user is watching. Its only caller is the Telegram bot
  (`internal/telegram/bot.go:340`) — **the web dashboard never filters by
  watcher**, so every web user sees every request.
- `handlePickCandidate` (`internal/web/handlers.go:1142`) is **not
  owner-restricted** — any authenticated user can pick a candidate for any
  request. This is why the `manual` bucket is a shared queue and not "Ждёт вас".
- `requests` has **no owner column**; the requester is recorded only in the
  event detail (`internal/storage/requests.go:371`), which the events table
  renders as raw JSON.
- `AddWatcher` inserts `notify_mask = 31` (`internal/storage/people.go:62`) —
  every kind, `manual` included — while the `NotificationManual` comment two
  paragraphs above still describes the old default of 7 and the old reasoning
  (see §8.1).
- Pico v2.0.6 exposes `--pico-secondary-background` (#525f7a light, #5d6b89
  dark) but no foreground "info" token; the secondary background reaches only
  ≈3.3:1 as text on Pico's dark background, below the 4.5:1 floor.
- The creator is added as a watcher on creation
  (`internal/request/module.go:396`), so "Мои" is a proxy for "asked for by me"
  — not the same thing, and the copy must not claim it is.

Added while implementing Phase 0:

- **`--pico-contrast-warning` does not exist in `pico.min.css` v2.0.6.** The
  old `.flash.warn` rule was therefore always falling back to its literal
  `#c98300`, which does not follow the dark theme. The warning colour is now
  the project's own token, with a light and a dark value.
- Pico's dark theme is `:root:not([data-theme])` inside
  `@media only screen and (prefers-color-scheme: dark)`, and Pico also ships
  `[data-theme=dark]` and `[data-theme=light]` blocks. A project token that
  needs a dark value has to be declared in both places, or the two themes
  disagree about which is in force.
- `--pico-secondary-background` (#525f7a light, #5d6b89 dark) is a *background*
  token; used as a text colour it reaches only ≈3.3:1 on Pico's dark
  background. There is no usable foreground "info" colour in Pico v2.0.6.
- The stored rejection reason is `code + ": " + detail`
  (`internal/request/activities.go:451`), e.g.
  `too_few_seeders: 2 seeders, minimum is 3` — a hybrid, not a bare code. The
  detail is English prose generated in `internal/release/score.go`.
- `whenText` called `since()` on a **future** `NextRunAt`, and `since` renders
  anything under a minute as "только что" — so a request due to look again in
  six hours showed "только что" in the column that says when it will look
  again.
- `.pill input[type="checkbox"]` sets only a margin; the checkbox is visible
  inside the pill, so the native focus ring survives there.

Added while implementing Phase 1b:

- **Cancelling deletes the files, unless the request is watching for the rest
  of a season.** `cancelRequest` asks for `DeleteFiles: !state.Monitoring`
  (`internal/request/workflow.go:1467`), and `state.Monitoring` is set exactly
  where the request enters `ongoing` (`workflow.go:866`). The plan's opposite
  claim was wrong in four places.
- **A release is not a durable object.** `StoreCandidates` replaces the list
  on every fruitless search (`storage/candidates.go:65`) and `ClearCandidates`
  deletes it after a pick or a 14-day timeout (`candidates.go:233`). The
  chosen release keeps only `selected_title / indexer / size / seeders /
  score / thread_id`; its resolution, audio tier and studio are computed at
  selection time and thrown away. This is why D5's "catalogue of releases"
  reading is a storage change rather than a page.
- **Pico v2.0.6 has no `.button` class**, but it styles `[role=button]`
  fully — so `<a role="button">` is the way to make an anchor look like one.
  The stylesheet's compact-button rule was written for `a.button.small`,
  which could never match anything, and now names `a[role="button"].small`.
- **A card-mode table needs explicit roles.** `display: block` on table
  elements removes the implicit table semantics in Safari and VoiceOver; the
  roles are written into the markup, and the DOM order is unchanged, so the
  reading order stays the logical one.
- The dashboard's active table is now **form-free**: the cancel action is a
  link to the confirmation page, so the filter forms and the table have
  nothing left to swallow each other.

Added while implementing Phase 1c:

- **The requester was recorded nowhere.** This document claimed, in §3.3 and
  in Appendix B, that the requester's name sat in the create event's detail
  and only needed promoting. It does not: the create path writes no event
  at all, and `requested_by` appears only in the *retry* event's detail
  (`internal/storage/requests.go:377`). For every request but a retried one
  there was nothing to promote — which is why «Запросил» needed migration
  `00012` rather than a template change.
- **The projection view is frozen at creation.** `request_view` was last
  rebuilt by `00010`, which appended three ongoing-season columns. A new
  migration that adds a column to `requests` and forgets the view leaves
  the column invisible to every reader — and the reader's
  `requestViewColumns` selects by *name*, so the failure is a scan error
  rather than a wrong value.
- **`release.MaxSizeGBFor` exists so the rule has one copy.** The selector
  picks the size cap per media type; the «Требования» panel shows the same
  number. Two copies of that rule would drift, and the page would promise a
  size the selector does not enforce.
- **Pico reads `data-theme` when it is there and falls back to
  `prefers-color-scheme` when it is not** — so "auto" is the *absence* of
  the attribute, not a third value of it. The theme script therefore
  removes the attribute for "auto" and sets it for the other two.

Added while implementing Phase 2:

- **The release facts do not need to be stored.** A candidate row holds the
  title and the indexer, and `release.Parse` and `release.DetectAudio` are
  pure functions of exactly those two — so the page can re-derive the
  resolution, the source, the audio tier, the studio and the deciding marker
  with the selector's own rules. The plan's "storage change" was a
  misreading of what the candidate row already holds.
- **`storage.Candidate.Rank`'s comment was wrong.** It said "in
  selector-score order", but a refused release never reached the scorer:
  `verdict.Rejected` was appended in the indexer's order
  (`internal/release/score.go`), so the rank said nothing about the quality.
- **The audio tier does not depend on the configuration.** The marker tables
  live in the profile, but they are code and not settings — `ProfileFromConfig`
  overrides only the thresholds — so a title parsed today is parsed the same
  way as it was on the day it was refused.
- **The seeders are `-1` when the count was never reported**, and `-1` is
  what `internal/indexer/prowlarr.go` writes for a null count. The zero value
  of a `storage.Candidate` is therefore *zero seeders*, not unknown ones: a
  fixture that means "unknown" has to say `-1` explicitly.
- **The reason's numbers belong to the release.** The threshold a release
  missed is the policy's, and the policy is on the same page, in
  «Требования». Storing the threshold with the candidate would be a second
  copy of the rule, free to disagree with the first the moment the rule
  changed.
- **The refusals are the menu, and the menu has an order.** `StoreCandidates`
  replaces the list on every fruitless search, so the rank of a candidate is
  only meaningful within the list it was shown in — which is the same window
  the Telegram buttons live in.
