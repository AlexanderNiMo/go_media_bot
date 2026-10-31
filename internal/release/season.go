package release

import (
	"fmt"
	"time"

	"github.com/alex/mediad/internal/domain"
)

// This file holds the logic of a season that is still being released.
//
// A season pack is not a thing that exists or does not exist: a tracker
// publishes the episodes it has, and the pack grows as the season does. The
// question a request has to answer is therefore not "is there a release for
// this season" but "is the release I found everything the season will
// eventually be, and if not, is the rest of it on its way".
//
// Everything here is pure: no I/O, no clock, no configuration. The clock is
// passed in, so the same decision can be pinned down in a table test.

// SeasonState is what is known about a season's airing.
//
// It is the difference between "the season is over and this pack is short" —
// nothing to wait for — and "the season is still running and this pack is
// short" — the rest of it is on its way.
type SeasonState struct {
	// AiredCount is the number of episodes the season has aired.
	AiredCount int
	// PlannedCount is the number of episodes the season is expected to have.
	// Zero when that is not known, which is the ordinary case while a
	// season is still running.
	PlannedCount int
	// NewestAirDate is when the most recent episode of the season aired. Nil
	// when no episode carries an air date.
	NewestAirDate *time.Time
	// AirDate is when the season started. Nil when the season has no date.
	AirDate *time.Time
}

// SeasonWindows are the deadlines that decide whether a season is still
// being produced.
type SeasonWindows struct {
	// Airing is how long after its newest episode a season still counts as
	// running. A weekly show that aired three days ago is running; one that
	// aired four months ago is not.
	Airing time.Duration
	// Recent is how long after its premiere a season still counts as too
	// new to be finished.
	Recent time.Duration
	// Monitor is how long after its last episode the tracker may still
	// publish the rest of the season.
	Monitor time.Duration
}

// IsLive reports whether the season is still being produced.
//
// Three things say so: an episode came out recently enough that the next one
// is probably close behind, the season started so recently that it cannot
// possibly be over, and the episode count says there are more episodes than
// have aired.
func (s SeasonState) IsLive(now time.Time, w SeasonWindows) bool {
	if s.PlannedCount > s.AiredCount {
		return true
	}
	if s.NewestAirDate != nil && now.Sub(*s.NewestAirDate) <= w.Airing {
		return true
	}
	if s.AirDate != nil && now.Sub(*s.AirDate) <= w.Recent {
		return true
	}
	return false
}

// MightStillComplete reports whether the tracker might still publish the rest
// of the season.
//
// It is the question a request asks before it takes a short pack. A season
// that ended two months ago is not going to be completed, and waiting for it
// is a waste; a season that ended three weeks ago very well might be, since
// a tracker publishes the finished pack days after the last episode.
//
// The measure is the same one the wait itself is bounded by: the season's
// last episode plus the monitor window. A season that is still running keeps
// the answer "yes" however long the wait goes on, because every episode that
// airs moves the deadline with it.
func (s SeasonState) MightStillComplete(now time.Time, w SeasonWindows) bool {
	if s.IsLive(now, w) {
		return true
	}

	basis := s.NewestAirDate
	if basis == nil {
		basis = s.AirDate
	}
	if basis == nil {
		return false
	}
	return now.Sub(*basis) <= w.Monitor
}

// Expected returns the number of episodes the season is expected to have.
//
// The planned count is what it will have; the aired count is what it has. A
// season that is still running has no final number, and the aired count is
// the best answer available.
func (s SeasonState) Expected() int {
	if s.PlannedCount > 0 {
		return s.PlannedCount
	}
	return s.AiredCount
}

// NeedsMore reports whether a pack carrying files episodes is short of the
// season.
//
// The season's final size is usually unknown while it is still running: the
// tracker says "1-8 из 10" and TMDB lists the eight episodes that have
// aired. A pack that has everything aired so far is therefore still short
// while the season is running.
//
// claimed is the number of episodes the pack's own name says the season has
// ("1-8 из 10" says ten). It is the one number that can say the opposite —
// a pack that says it has all ten has all ten — so it is consulted first.
func (s SeasonState) NeedsMore(files, claimed int, now time.Time, w SeasonWindows) bool {
	if claimed > 0 {
		return files < claimed
	}
	if s.PlannedCount > 0 {
		return files < s.PlannedCount
	}
	if s.IsLive(now, w) {
		return true
	}
	return files < s.AiredCount
}

// Tracked is the pack a request is following: the one already added, and the
// identity a replacement has to match.
//
// It is what the season watcher looks for. A tracker does not publish a new
// torrent for a season that grew — it replaces the torrent in the same forum
// thread — so the thread is the identity that survives the infohash
// changing.
type Tracked struct {
	// ThreadID is the forum thread the pack came from. Empty when the
	// indexer did not report one, and then nothing can be recognised as an
	// update of it.
	ThreadID string
	// Episodes is how many episodes the pack carries, as the torrent client
	// counted them. It is the number a replacement has to beat.
	Episodes int
	// Title is the pack's name, kept for the audit trail.
	Title string
}

// IsUpdateOf reports whether a release is a newer version of the tracked
// pack.
//
// The test is deliberately narrow: the same thread, and more episodes. The
// thread says the two come from the same upload — the same encode, the same
// quality — and the episode count says the new one carries something the old
// one does not.
//
// A release whose name says nothing about its episodes cannot be recognised
// as an update. That is a real limitation rather than a shortcut: the
// tracker is the only place that knows what a pack contains.
func IsUpdateOf(candidate domain.Release, tracked Tracked) bool {
	if tracked.ThreadID == "" {
		return false
	}
	if candidate.ThreadID != tracked.ThreadID {
		return false
	}
	span := Parse(candidate.Title).Episodes
	if !span.Known {
		return false
	}
	return span.Count() > tracked.Episodes
}

// SelectUpdate picks the release that is a newer version of the pack a
// request is following.
//
// It applies the same hard filters and the same scoring as Select, and adds
// one requirement of its own: the release has to be an update of the tracked
// pack. A request waiting for the rest of a season must not wander off into
// a different release — a better encode of a different pack is not what it
// is waiting for.
func (s selector) SelectUpdate(
	spec domain.MediaSpec,
	releases []domain.Release,
	blacklist map[string]bool,
	tracked Tracked,
) Verdict {
	verdict := Verdict{Considered: len(releases)}

	accepted := make([]scored, 0, len(releases))
	for _, rel := range releases {
		parsed := Parse(rel.Title)
		audio := DetectAudio(rel.Title, rel.Indexer, s.profile)

		reason, detail := s.reject(spec, rel, parsed, audio, blacklist)
		if reason == "" && !IsUpdateOf(rel, tracked) {
			reason = ReasonNotAnUpdate
			detail = fmt.Sprintf("it is not a newer version of the tracked pack (%s)", tracked.Title)
		}
		if reason != "" {
			verdict.Rejected = append(verdict.Rejected, Rejection{
				Release: rel, Reason: reason, Detail: detail,
			})
			continue
		}
		accepted = append(accepted, scored{release: rel, parsed: parsed, audio: audio})
	}

	// The most complete update on offer: the one that carries the most of
	// what is still missing.
	mostEpisodes := 0
	allNamed := len(accepted) > 0
	for _, c := range accepted {
		n := c.parsed.Episodes.Count()
		if n == 0 {
			allNamed = false
			continue
		}
		if n > mostEpisodes {
			mostEpisodes = n
		}
	}

	for _, c := range accepted {
		score := s.score(c.release, c.parsed, c.audio)
		if allNamed && mostEpisodes > 0 && c.parsed.Episodes.Count() == mostEpisodes {
			score += bonusEpisodes
		}
		if !verdict.Acceptable || score > verdict.Score {
			verdict.Acceptable = true
			verdict.Chosen = c.release
			verdict.Score = score
			verdict.Audio = c.audio
			verdict.Parsed = c.parsed
		}
	}

	return verdict
}
