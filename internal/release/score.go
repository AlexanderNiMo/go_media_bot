package release

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/alex/mediad/internal/domain"
)

// Reason codes for rejecting a release.
//
// They are stable identifiers rather than prose because they are written to
// the event log and read back by a user asking "why did it skip this one".
// A string that changes when the message is reworded is not something anyone
// can filter on.
type Reason string

const (
	ReasonExcludedSource   Reason = "excluded_source"
	ReasonResolutionTooLow Reason = "resolution_too_low"
	ReasonUnknownQuality   Reason = "unknown_quality"
	ReasonTooFewSeeders    Reason = "too_few_seeders"
	ReasonTooSmall         Reason = "too_small"
	ReasonTooLarge         Reason = "too_large"
	ReasonImplausibleSize  Reason = "implausible_size"
	ReasonBlacklisted      Reason = "blacklisted"
	ReasonNoRussianAudio   Reason = "no_russian_audio"
	ReasonSubtitlesOnly    Reason = "subtitles_only"
	ReasonWrongSeason      Reason = "wrong_season"
	ReasonIsSingleEpisode  Reason = "single_episode"
	ReasonWrongYear        Reason = "wrong_year"
	ReasonNoDownloadLink   Reason = "no_download_link"
)

// Rejection pairs a release with the reason it was refused.
type Rejection struct {
	Release domain.Release
	Reason  Reason
	Detail  string
}

// Verdict is the outcome of evaluating a set of releases.
type Verdict struct {
	// Chosen is the best acceptable release. It is only meaningful when
	// Acceptable is true.
	Chosen domain.Release
	Score  float64
	// Audio and Parsed describe the chosen release, so the caller can record
	// why it won without re-parsing.
	Audio  AudioTrack
	Parsed Parsed

	Acceptable bool

	// Rejected holds every release that was refused, with the reason. It is
	// the answer to "there were results, why did nothing download".
	Rejected []Rejection
	// Considered is how many releases were evaluated.
	Considered int
}

// Summary renders a one-line explanation suitable for a log or a user.
func (v Verdict) Summary() string {
	if v.Acceptable {
		return fmt.Sprintf("chose %q (score %.2f, %s, %s)",
			v.Chosen.Title, v.Score, v.Parsed.Quality, v.Audio.Describe())
	}
	if v.Considered == 0 {
		return "no releases returned by the indexer"
	}
	return fmt.Sprintf("none of %d releases were acceptable: %s",
		v.Considered, v.topReasons(3))
}

// topReasons counts rejection reasons, most common first.
func (v Verdict) topReasons(n int) string {
	counts := map[Reason]int{}
	for _, r := range v.Rejected {
		counts[r.Reason]++
	}

	type pair struct {
		reason Reason
		count  int
	}
	pairs := make([]pair, 0, len(counts))
	for reason, count := range counts {
		pairs = append(pairs, pair{reason, count})
	}
	// Sorted by count, then by reason, so the output is stable across runs —
	// map iteration order is not.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].reason < pairs[j].reason
	})

	if len(pairs) > n {
		pairs = pairs[:n]
	}

	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%s x%d", p.reason, p.count))
	}
	return strings.Join(parts, ", ")
}

// Selector picks the release to download.
type Selector interface {
	Select(spec domain.MediaSpec, releases []domain.Release, blacklist map[string]bool) Verdict
}

// NewSelector returns the default selector.
func NewSelector(profile Profile) Selector { return selector{profile: profile} }

type selector struct {
	profile Profile
}

// Select evaluates every release and returns the best acceptable one.
//
// It never returns an error. "Nothing acceptable" is the ordinary outcome of a
// search — it is what most hourly runs will produce for the first months of a
// two-year request — and modelling it as an error would push callers into
// treating a normal state as a failure.
func (s selector) Select(spec domain.MediaSpec, releases []domain.Release, blacklist map[string]bool) Verdict {
	verdict := Verdict{Considered: len(releases)}

	for _, rel := range releases {
		parsed := Parse(rel.Title)
		audio := DetectAudio(rel.Title, rel.Indexer, s.profile)

		reason, detail := s.reject(spec, rel, parsed, audio, blacklist)
		if reason != "" {
			verdict.Rejected = append(verdict.Rejected, Rejection{
				Release: rel, Reason: reason, Detail: detail,
			})
			continue
		}

		score := s.score(rel, parsed, audio)
		if !verdict.Acceptable || score > verdict.Score {
			verdict.Acceptable = true
			verdict.Chosen = rel
			verdict.Score = score
			verdict.Audio = audio
			verdict.Parsed = parsed
		}
	}

	return verdict
}

// reject applies the hard filters, in order of how certain each one is.
//
// Ordering is for the quality of the recorded reason rather than for
// correctness: a release that is both a cam rip and short of seeders should be
// remembered as a cam rip, because that is the more fundamental problem and
// the one the user will care about.
func (s selector) reject(
	spec domain.MediaSpec,
	rel domain.Release,
	parsed Parsed,
	audio AudioTrack,
	blacklist map[string]bool,
) (Reason, string) {

	if rel.Link() == "" {
		return ReasonNoDownloadLink, "the indexer returned neither a magnet nor a download url"
	}

	if blacklist[rel.Identity()] {
		return ReasonBlacklisted, "this exact release was tried before and did not work"
	}

	if s.profile.Excludes(parsed.Source) {
		return ReasonExcludedSource, fmt.Sprintf("source %s is excluded", parsed.Source)
	}

	// A release with no resolution in its name usually means the indexer
	// returned something that is not a video release at all, or a name so
	// sloppy that nothing else in it can be trusted either.
	if parsed.Quality == QualityUnknown {
		return ReasonUnknownQuality, "no resolution found in the name"
	}
	if parsed.Quality.Height() < s.profile.MinResolution.Height() {
		return ReasonResolutionTooLow, fmt.Sprintf("%s is below the minimum %s",
			parsed.Quality, s.profile.MinResolution)
	}

	if err := s.checkAudio(audio); err != nil {
		return err.reason, err.detail
	}

	// seeders of -1 means the indexer did not report them. Treating that as
	// zero would discard every release from such an indexer, so it is allowed
	// through and the uncertainty is handled in scoring instead.
	if rel.Seeders >= 0 && rel.Seeders < s.profile.MinSeeders {
		return ReasonTooFewSeeders, fmt.Sprintf("%d seeders, minimum is %d",
			rel.Seeders, s.profile.MinSeeders)
	}

	if reason, detail := s.checkSize(rel, parsed); reason != "" {
		return reason, detail
	}

	if reason, detail := checkSeason(spec, rel, parsed); reason != "" {
		return reason, detail
	}

	if reason, detail := checkYear(spec, rel, parsed); reason != "" {
		return reason, detail
	}

	return "", ""
}

type audioError struct {
	reason Reason
	detail string
}

func (s selector) checkAudio(audio AudioTrack) *audioError {
	if !s.profile.RequireRussianAudio {
		return nil
	}

	if audio.Tier >= AudioVoiceOver {
		return nil
	}
	if audio.ImpliedByTracker && s.profile.AllowImpliedAudio && !audio.IndicatesEnglish {
		return nil
	}

	if audio.Tier == AudioSubsOnly || audio.HasSubs {
		return &audioError{
			ReasonSubtitlesOnly,
			"Russian subtitles but no Russian audio",
		}
	}
	return &audioError{
		ReasonNoRussianAudio,
		"no Russian audio detected in the release name",
	}
}

func (s selector) checkSize(rel domain.Release, parsed Parsed) (Reason, string) {
	if rel.SizeBytes <= 0 {
		// Nothing to check. An indexer that reports no size is not a reason to
		// refuse a release that is otherwise fine.
		return "", ""
	}

	sizeGB := float64(rel.SizeBytes) / (1 << 30)

	if sizeGB < s.profile.MinSizeGB {
		return ReasonTooSmall, fmt.Sprintf("%.2f GB is below the minimum %.2f GB",
			sizeGB, s.profile.MinSizeGB)
	}
	if sizeGB > s.profile.MaxSizeGB {
		return ReasonTooLarge, fmt.Sprintf("%.2f GB is above the maximum %.2f GB",
			sizeGB, s.profile.MaxSizeGB)
	}

	// Below the configured floor there is still a case worth catching: a file
	// claiming 1080p at a fraction of what 1080p can be. These are usually a
	// sample, a fake, or a mislabelled rip, and they waste a download slot.
	// Note this is a *floor for the claimed resolution*, not a quality
	// judgement — the configured minimum above is the user's actual
	// preference.
	if floor := plausibleFloorGB(parsed.Quality, parsed.Source); floor > 0 && sizeGB < floor {
		return ReasonImplausibleSize, fmt.Sprintf(
			"%.2f GB is not plausible for a %s %s release",
			sizeGB, parsed.Source, parsed.Quality)
	}

	return "", ""
}

// plausibleFloorGB is the smallest size a release of a given quality can
// credibly be, in gigabytes.
//
// It is deliberately far below a *good* release of that quality: the purpose
// is to catch a mislabelled or fake file, not to express a preference. A
// 1080p feature that is 300 MB is not a low-bitrate encode, it is something
// else wearing a 1080p label.
func plausibleFloorGB(q Quality, src Source) float64 {
	base := 0.0
	switch q {
	case Quality2160p:
		base = 4
	case Quality1440p:
		base = 2.5
	case Quality1080p, Quality1080i:
		base = 1
	case Quality720p:
		base = 0.4
	case Quality576p, Quality480p:
		base = 0.15
	default:
		return 0
	}

	// A remux is the full disc and cannot be small; anything claiming to be
	// one below several gigabytes is mislabelled.
	if src == SourceBDRemux {
		base *= 3
	}
	return base
}

// checkSeason refuses a release that is for a different season.
//
// The case that matters is not a wrong season but a single episode: a request
// is for a whole season, and an "S02E05" release is one tenth of one. Grabbing
// it would fill the download slot and leave the request unfulfilled.
func checkSeason(spec domain.MediaSpec, rel domain.Release, parsed Parsed) (Reason, string) {
	if !spec.Ref.IsSeason() {
		if parsed.HasSeason && parsed.Season > 0 {
			// A film release naming a season is a different work entirely.
			return ReasonWrongSeason, fmt.Sprintf(
				"the request is for a film but the release names season %d", parsed.Season)
		}
		return "", ""
	}

	want := spec.Ref.SeasonNumber()

	if !parsed.HasSeason {
		// No season in the name. This is common for season packs on some
		// trackers and for collections, so it is not refused here — the
		// indexer was asked for a specific season and is trusted to have
		// honoured that. Scoring penalises the uncertainty.
		return "", ""
	}

	if parsed.Season != want {
		return ReasonWrongSeason, fmt.Sprintf("release is season %d, request is season %d",
			parsed.Season, want)
	}

	if parsed.HasEpisode {
		return ReasonIsSingleEpisode, fmt.Sprintf(
			"release is a single episode (S%02dE%02d), the request is for the whole season",
			parsed.Season, parsed.Episode)
	}

	return "", ""
}

// checkYear refuses a release from a clearly different year.
//
// Only a confident mismatch counts. The tolerance exists because a release
// name's year is often the festival première, the disc release, or simply
// wrong — and refusing a good release over that would be worse than accepting
// an occasional remake.
func checkYear(spec domain.MediaSpec, rel domain.Release, parsed Parsed) (Reason, string) {
	if spec.Year == 0 || parsed.Year == 0 {
		return "", ""
	}
	if diff := parsed.Year - spec.Year; diff < -1 || diff > 1 {
		return ReasonWrongYear, fmt.Sprintf("release is from %d, request is from %d",
			parsed.Year, spec.Year)
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// Scoring
// ---------------------------------------------------------------------------

// Weights are fixed in code rather than configured.
//
// They express what "better" means for this project and are not a matter of
// taste a user would tune; the things a user does care about — resolution
// floor, seeders, size, audio requirement — are all in the profile as filters.
//
// Audio outweighs resolution deliberately. A dub at the floor resolution is a
// better outcome for someone asking for a film in Russian than a 4K release
// they cannot follow, and the test that pins this down is
// TestAudioOutweighsResolution.
const (
	weightQuality = 0.30
	weightAudio   = 0.30
	weightSource  = 0.20
	weightSeeders = 0.12
	weightSize    = 0.08

	// bonusHEVC rewards a smaller file at the same quality, which for a
	// library that is streamed to a TV is a real benefit.
	bonusHEVC = 0.03
)

// score rates an acceptable release. Higher is better; the absolute value is
// meaningless and only the ordering matters.
func (s selector) score(rel domain.Release, parsed Parsed, audio AudioTrack) float64 {
	var total float64

	total += weightQuality * s.scoreQuality(parsed.Quality)
	total += weightAudio * scoreAudio(audio)
	total += weightSource * scoreSource(parsed.Source)
	total += weightSeeders * scoreSeeders(rel.Seeders)
	total += weightSize * s.scoreSize(rel, parsed)

	if parsed.Codec == CodecH265 {
		total += bonusHEVC
	}

	return total
}

// scoreQuality maps a resolution onto a score between the floor and the
// preference.
//
// The floor scores 0.5 rather than 0, and that is the point of the function.
// The minimum resolution is what the user said they will *accept*, not what
// they consider worthless — scoring it zero turned the resolution weight into
// a cliff that overrode every other consideration, so a 4K release with an
// amateur voice-over beat a 1080p one with a proper dub.
//
// Between the two ends the scale is linear in height rather than in rank, so
// the step from 1080p to 2160p counts for more than the step from 720p to
// 1080p, which is how the difference is perceived.
func (s selector) scoreQuality(q Quality) float64 {
	const floorScore = 0.5

	minH := float64(s.profile.MinResolution.Height())
	prefH := float64(s.profile.PreferredResolution.Height())

	if prefH <= minH {
		// No headroom to reward: everything acceptable scores the same.
		if float64(q.Height()) >= prefH {
			return 1
		}
		return floorScore
	}

	got := float64(q.Height())
	switch {
	case got >= prefH:
		return 1
	case got <= minH:
		return floorScore
	default:
		return floorScore + (1-floorScore)*(got-minH)/(prefH-minH)
	}
}

func scoreAudio(audio AudioTrack) float64 {
	switch audio.Tier {
	case AudioDub:
		return 1.0
	case AudioMulti:
		return 0.85
	case AudioDual:
		return 0.6
	case AudioSingle:
		return 0.4
	case AudioVoiceOver:
		return 0.25
	case AudioSubsOnly:
		return 0.05
	default:
		// No marker at all. A Russian tracker is weak evidence, not none.
		if audio.ImpliedByTracker {
			return 0.3
		}
		return 0
	}
}

func scoreSource(src Source) float64 {
	switch src {
	case SourceBDRemux:
		return 1.0
	case SourceBluRay:
		return 0.95
	case SourceBDRip:
		return 0.9
	case SourceWebDL:
		return 0.85
	case SourceWebRip:
		return 0.7
	case SourceHDTV:
		return 0.45
	case SourceDVD:
		return 0.3
	default:
		return 0.1
	}
}

// scoreSeeders is logarithmic, because the difference between 2 and 20 seeders
// decides whether a download completes while the difference between 200 and
// 220 does not.
func scoreSeeders(seeders int) float64 {
	switch {
	case seeders < 0:
		// Unknown. Scored between a poorly-seeded release and a well-seeded
		// one, so that an indexer which omits the field is neither penalised
		// into oblivion nor preferred over a release known to be healthy.
		return 0.5
	case seeders == 0:
		return 0
	default:
		return math.Min(1, math.Log1p(float64(seeders))/math.Log1p(100))
	}
}

// scoreSize rewards a size in the healthy band for the release's quality and
// runtime, and tapers towards the edges.
func (s selector) scoreSize(rel domain.Release, parsed Parsed) float64 {
	if rel.SizeBytes <= 0 {
		return 0.5
	}

	gb := float64(rel.SizeBytes) / (1 << 30)

	// Without a runtime there is no basis for judging, so everything in the
	// configured range scores the same.
	if s.profile.MaxSizeGB <= s.profile.MinSizeGB {
		return 0.5
	}

	// The midpoint of the configured range is treated as ideal. This is a
	// weak signal and is weighted as one.
	mid := (s.profile.MinSizeGB + s.profile.MaxSizeGB) / 2
	spread := (s.profile.MaxSizeGB - s.profile.MinSizeGB) / 2
	if spread <= 0 {
		return 0.5
	}

	deviation := math.Abs(gb-mid) / spread
	return math.Max(0, 1-deviation)
}
