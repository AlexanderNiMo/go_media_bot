// Package release decides which of the torrents an indexer returned should be
// downloaded.
//
// Everything in this package is a pure function: no I/O, no clock, no
// configuration reached for at call time. That is deliberate. Release
// selection is where this project will attract its bug reports — "why did it
// grab a cam rip", "why did it skip a perfectly good release" — and a pure
// function is one that can be pinned down with table tests and reasoned about
// without a running system.
//
// It also keeps the logic out of the workflow. Selection changes far more often
// than orchestration, and code inside a workflow cannot be changed without
// breaking the replay of executions that are already running.
package release

import (
	"regexp"
	"strconv"
	"strings"
)

// Quality is the resolution of a release.
type Quality int

const (
	QualityUnknown Quality = iota
	Quality480p
	Quality576p
	Quality720p
	Quality1080p
	Quality1080i
	Quality1440p
	Quality2160p
)

// String renders the quality as it appears in a release name.
func (q Quality) String() string {
	switch q {
	case Quality480p:
		return "480p"
	case Quality576p:
		return "576p"
	case Quality720p:
		return "720p"
	case Quality1080p:
		return "1080p"
	case Quality1080i:
		return "1080i"
	case Quality1440p:
		return "1440p"
	case Quality2160p:
		return "2160p"
	default:
		return "unknown"
	}
}

// Height returns the vertical resolution, used for comparison.
func (q Quality) Height() int {
	switch q {
	case Quality480p:
		return 480
	case Quality576p:
		return 576
	case Quality720p:
		return 720
	case Quality1080p, Quality1080i:
		return 1080
	case Quality1440p:
		return 1440
	case Quality2160p:
		return 2160
	default:
		return 0
	}
}

// QualityFromHeight returns the closest quality for a vertical resolution.
func QualityFromHeight(h int) Quality {
	switch {
	case h >= 2000:
		return Quality2160p
	case h >= 1400:
		return Quality1440p
	case h >= 1000:
		return Quality1080p
	case h >= 700:
		return Quality720p
	case h >= 560:
		return Quality576p
	case h >= 400:
		return Quality480p
	default:
		return QualityUnknown
	}
}

// Source is where the release's video came from.
type Source int

const (
	SourceUnknown Source = iota
	SourceCam
	SourceTelesync
	SourceScreener
	SourceDVD
	SourceHDTV
	SourceWebRip
	SourceWebDL
	SourceBluRay
	SourceBDRip
	SourceBDRemux
)

// String renders the source as it appears in a release name.
func (s Source) String() string {
	switch s {
	case SourceCam:
		return "CAM"
	case SourceTelesync:
		return "TS"
	case SourceScreener:
		return "SCR"
	case SourceDVD:
		return "DVDRip"
	case SourceHDTV:
		return "HDTV"
	case SourceWebRip:
		return "WEBRip"
	case SourceWebDL:
		return "WEB-DL"
	case SourceBluRay:
		return "BluRay"
	case SourceBDRip:
		return "BDRip"
	case SourceBDRemux:
		return "BDRemux"
	default:
		return "unknown"
	}
}

// Codec is the video codec, which is a mild preference rather than a filter.
type Codec int

const (
	CodecUnknown Codec = iota
	CodecH264
	CodecH265
	CodecAV1
)

// String renders the codec.
func (c Codec) String() string {
	switch c {
	case CodecH264:
		return "H.264"
	case CodecH265:
		return "H.265"
	case CodecAV1:
		return "AV1"
	default:
		return "unknown"
	}
}

// Parsed is what could be read out of a release name.
//
// Every field is best-effort. A release name is free text written by a human
// uploading to a tracker, there is no schema, and a name that yields nothing is
// an ordinary outcome rather than an error — which is why nothing here returns
// one.
type Parsed struct {
	Quality Quality
	Source  Source
	Codec   Codec

	// Year is the release year when the name carries one.
	Year int
	// Season and Episode are set when the name follows the SxxExx convention
	// or spells the season out. HasEpisodePack distinguishes "S02" from
	// "S02E05": the first is a season pack, the second is one episode.
	Season         int
	Episode        int
	HasSeason      bool
	HasEpisode     bool
	HasEpisodePack bool

	// Episodes is the range of episodes the name claims to carry.
	//
	// It is what tells a pack of eight episodes from a single one, and — for a
	// season that is still being released — how far along the pack is: a name
	// that says "1-8 из 10" is eight episodes out of a season of ten.
	//
	// A name that says nothing about episodes leaves it unknown, which is a
	// different thing from a range of zero episodes.
	Episodes Span

	// HDR flags are not scored today but are recorded, because a release that
	// is HDR and one that is not are different downloads even at the same
	// resolution.
	HDR         bool
	DolbyVision bool

	// Normalised is the name with separators folded to spaces and lowercased,
	// which is what the marker tables are matched against.
	Normalised string
}

var (
	// Separators are folded to spaces before anything is matched, so that
	// "The.Matrix.1999.BDRemux.1080p" and "The Matrix 1999 BDRemux 1080p"
	// behave identically.
	separatorRe = regexp.MustCompile(`[._\-\[\](){}]+`)

	// Word boundaries work for ASCII, which is all these patterns need: every
	// Cyrillic marker is matched by substring instead, because RE2 treats \b
	// as ASCII-only and would never match the edge of a Cyrillic word.
	resolutionRe = regexp.MustCompile(`\b(2160p|1440p|1080p|1080i|720p|576p|480p|4k|uhd)\b`)
	yearRe       = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)

	seasonEpisodeRe = regexp.MustCompile(`\bs(\d{1,2})(?:e(\d{1,3}))?\b`)
	seasonWordRe    = regexp.MustCompile(`\bseason\s*(\d{1,2})\b`)
	// The Russian word order. There is no \b before "сезон" because RE2
	// treats word boundaries as ASCII-only and would never match the edge of a
	// Cyrillic word — the trailing boundary does work, because it sits between
	// an ASCII digit and a space.
	seasonRuRe    = regexp.MustCompile(`сезон\w*\s*(\d{1,2})\b`)
	episodeWordRe = regexp.MustCompile(`\be\d{1,3}\b`)
	episodeRuRe   = regexp.MustCompile(`серия|серии|эпизод`)

	// Episode ranges are matched against the raw name, not the normalised
	// one: the normalisation folds the hyphen that separates the two ends of
	// a range into a space, and "S02E01-E08" and "S02E01 E08" would become
	// indistinguishable.
	//
	// "S02E01-E08", "S02.E01.E08", "S02E01-08", "S02E01 E08".
	spanWithSeasonRe = regexp.MustCompile(`(?i)\bs(\d{1,2})\s*[._\- ]*\s*e(\d{1,3})\s*[._\-–— ]+\s*e?(\d{1,3})\b`)

	// "E01-E08", without a season marker.
	spanEpisodeRe = regexp.MustCompile(`(?i)\be(\d{1,3})\s*[._\-–— ]+\s*e?(\d{1,3})\b`)

	// "1x01-1x08" — the season-episode convention with an "x" between the
	// two numbers.
	spanXRe = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{1,3})\s*[-–—]\s*(?:\d{1,2}x)?(\d{1,3})\b`)

	// A single "1x05" — one episode of a season, in the same convention.
	singleXRe = regexp.MustCompile(`(?i)\b\d{1,2}x(\d{1,3})\b`)

	// "1-8 из 10" — the Russian tracker convention, where the range is
	// followed by the number of episodes the season has in total.
	spanWithTotalRe = regexp.MustCompile(`(?i)(\d\.)?\b(\d{1,2})\s*[-–—]\s*(\d{1,2})\s*(?:из|of)\s*(\d{1,2})\b`)

	// A bare "1-8". It is only an episode range when the name also says which
	// season it belongs to: on its own it is as likely to be a pair of years
	// or a resolution as a pair of episodes.
	//
	// The optional leading group catches the audio track specification:
	// "5.1-2.0" is two numbers with a hyphen between them, and reading it as
	// a range would make a single-episode release look like a pack of two.
	spanBareRe = regexp.MustCompile(`(?i)(\d\.)?\b(\d{1,2})\s*[-–—]\s*(\d{1,2})\b`)

	// The number of episodes the season has, when the name says so. It is
	// read on its own, because the total is worth having even when the range
	// is written in a form the range patterns do not match.
	totalRe = regexp.MustCompile(`(?i)(?:из|of)\s*(\d{1,2})\b`)

	// Sources are matched longest-first so that BDRemux wins over BDRip and
	// WEB-DL over WEB, which substring matching alone would get wrong.
	sourcePatterns = []struct {
		re     *regexp.Regexp
		source Source
	}{
		// "Blu-Ray" normalises to "blu ray", so the hyphen has to be optional
		// space rather than absent — otherwise every Blu-Ray Remux is silently
		// downgraded to an unknown source.
		{regexp.MustCompile(`\bbd\s?remux\b|\bblu\s?ray\s*remux\b`), SourceBDRemux},
		{regexp.MustCompile(`\bbdrip\b`), SourceBDRip},
		{regexp.MustCompile(`\bblu\s?ray\b|\bbd\b`), SourceBluRay},
		{regexp.MustCompile(`\bweb[- ]?dl\b|\bwebdl\b`), SourceWebDL},
		{regexp.MustCompile(`\bweb[- ]?rip\b|\bwebrip\b`), SourceWebRip},
		{regexp.MustCompile(`\bhdtv\b`), SourceHDTV},
		{regexp.MustCompile(`\bdvdrip\b|\bdvd\b`), SourceDVD},
		{regexp.MustCompile(`\bdvdscr\b|\bscr\b|\bscreener\b`), SourceScreener},
		{regexp.MustCompile(`\bhdts\b|\bts\b|\btelesync\b`), SourceTelesync},
		{regexp.MustCompile(`\bcam\b|\bcamrip\b`), SourceCam},
	}

	codecPatterns = []struct {
		re    *regexp.Regexp
		codec Codec
	}{
		// "H.265" normalises to "h 265" for the same reason: the dot becomes a
		// space before anything is matched.
		{regexp.MustCompile(`\bhevc\b|\bh\s?265\b|\bx\s?265\b`), CodecH265},
		{regexp.MustCompile(`\bavc\b|\bh\s?264\b|\bx\s?264\b`), CodecH264},
		{regexp.MustCompile(`\bav1\b`), CodecAV1},
	}

	hdrRe = regexp.MustCompile(`\bhdr10\+?\b|\bhdr\b`)
	dvRe  = regexp.MustCompile(`\bdolby\s*vision\b|\bdovi\b|\bdv\b`)
)

// Parse reads what it can out of a release name.
func Parse(name string) Parsed {
	p := Parsed{Normalised: Normalise(name)}

	if m := resolutionRe.FindStringSubmatch(p.Normalised); m != nil {
		p.Quality = qualityFromToken(m[1])
	}

	for _, sp := range sourcePatterns {
		if sp.re.MatchString(p.Normalised) {
			p.Source = sp.source
			break
		}
	}

	for _, cp := range codecPatterns {
		if cp.re.MatchString(p.Normalised) {
			p.Codec = cp.codec
			break
		}
	}

	if m := yearRe.FindStringSubmatch(p.Normalised); m != nil {
		p.Year, _ = strconv.Atoi(m[1])
	}

	p.parseSeason()
	p.Episodes = episodeSpan(name, p.HasSeason)

	// A name that carries a range of episodes is a pack, not a single
	// episode. The SxxExx pattern matched the first episode of the range
	// ("S02E01-E08" reads as season 2, episode 1), and without this the
	// selector would refuse a whole season as if it were one episode.
	if p.Episodes.Count() > 1 {
		p.HasEpisode = false
	}

	// "1x05" is one episode, in the same convention as "S02E05" — a naming
	// the season and episode without the "S" and the "E".
	if !p.HasEpisode && p.Episodes.Count() == 0 && singleXRe.MatchString(name) {
		p.HasEpisode = true
	}

	p.HDR = hdrRe.MatchString(p.Normalised)
	p.DolbyVision = dvRe.MatchString(p.Normalised)

	return p
}

// Span is the range of episodes a release name claims to carry.
//
// It is what tells a pack of eight episodes from a single one, and — for a
// season that is still being released — how far along the pack is: a name
// that says "1-8 из 10" is eight episodes out of a season of ten.
type Span struct {
	// From and To are the first and the last episode of the range.
	From int
	To   int
	// Total is the number of episodes the season is expected to have, when
	// the name says so ("1-8 из 10"). Zero when it does not.
	Total int
	// Known reports whether the name carried a range at all. A name that
	// says nothing about its episodes has a span that is not known, which is
	// a different thing from a range of zero episodes.
	Known bool
}

// Count returns how many episodes the span covers.
func (s Span) Count() int {
	if !s.Known || s.To < s.From {
		return 0
	}
	return s.To - s.From + 1
}

// episodeSpan reads the range of episodes out of a release name.
//
// hasSeason says whether the name names a season. It is what makes a bare
// "1-8" readable as an episode range: without it, the same two numbers are
// as likely to be a pair of years.
//
// The order of the attempts matters. The form with a total comes first
// because "1-8 из 10" also matches the bare range, and the total is
// information the bare form does not carry.
//
// The total is read on its own afterwards, so that a name which carries the
// total in a form the range patterns do not match — "S02E01-E08 из 10" —
// still says how big the season is.
func episodeSpan(name string, hasSeason bool) Span {
	lower := strings.ToLower(name)

	span := Span{}
	switch {
	case matchSpan(spanWithSeasonRe, lower, 2, 3, 0, &span):
	case matchSpan(spanXRe, lower, 2, 3, 0, &span):
	case matchSpan(spanWithTotalRe, lower, 2, 3, 1, &span):
	case matchSpan(spanEpisodeRe, lower, 1, 2, 0, &span):
	case hasSeason && matchSpan(spanBareRe, lower, 2, 3, 1, &span):
	}

	if span.Known {
		span.Total = episodeTotal(lower, span.To)
	}
	return span
}

// matchSpan reads a range out of a match, when the match is one.
//
// fromGroup and toGroup are the groups holding the two ends of the range.
// skipGroup, when it is not zero, is the group holding an optional prefix:
// a match whose prefix matched is the audio track specification
// ("5.1-2.0"), not a range of episodes.
func matchSpan(re *regexp.Regexp, name string, fromGroup, toGroup, skipGroup int, span *Span) bool {
	m := re.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	if skipGroup > 0 && m[skipGroup] != "" {
		return false
	}
	from, to := atoiOrZero(m[fromGroup]), atoiOrZero(m[toGroup])
	if !plausibleSpan(from, to) {
		return false
	}
	span.From, span.To, span.Known = from, to, true
	return true
}

// episodeTotal reads the number of episodes the season has, when the name
// says so ("1-8 из 10").
func episodeTotal(name string, atLeast int) int {
	m := totalRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	total := atoiOrZero(m[1])
	if total < atLeast {
		return 0
	}
	return total
}

// plausibleSpan reports whether two numbers can be the ends of an episode
// range.
//
// The upper bound is what keeps a resolution out of it: "S02E01.1080p" must
// not read as a range ending at episode 1080, and a season does not have a
// hundred and one episodes.
func plausibleSpan(from, to int) bool {
	return from >= 0 && to <= 99 && from <= to
}

// atoiOrZero parses a number that a regex has already guaranteed to be
// digits.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func (p *Parsed) parseSeason() {
	// The SxxExx convention is tried first because it is unambiguous.
	if m := seasonEpisodeRe.FindStringSubmatch(p.Normalised); m != nil {
		p.Season, _ = strconv.Atoi(m[1])
		p.HasSeason = true
		if m[2] != "" {
			p.Episode, _ = strconv.Atoi(m[2])
			p.HasEpisode = true
		}
		return
	}

	// "Season 2".
	if m := seasonWordRe.FindStringSubmatch(p.Normalised); m != nil {
		p.Season, _ = strconv.Atoi(m[1])
		p.HasSeason = true
		p.HasEpisode = episodeWordRe.MatchString(p.Normalised) ||
			episodeRuRe.MatchString(p.Normalised)
		return
	}

	// "Сезон 2" — the word first, which is how Russian trackers usually write
	// it.
	if m := seasonRuRe.FindStringSubmatch(p.Normalised); m != nil {
		p.Season, _ = strconv.Atoi(m[1])
		p.HasSeason = true
		p.HasEpisode = episodeRuRe.MatchString(p.Normalised)
		return
	}

	// "2 сезон" — the number first, which they also use.
	if s, ok := seasonBeforeWord(p.Normalised); ok {
		p.Season = s
		p.HasSeason = true
	}
}

// seasonBeforeWord recognises "2 сезон" and "2 серия".
func seasonBeforeWord(s string) (int, bool) {
	fields := strings.Fields(s)
	for i := 1; i < len(fields); i++ {
		switch fields[i] {
		case "сезон", "сезона", "сезоны", "season":
		default:
			continue
		}
		if n, err := strconv.Atoi(fields[i-1]); err == nil {
			return n, true
		}
	}
	return 0, false
}

func qualityFromToken(tok string) Quality {
	switch tok {
	case "2160p", "4k", "uhd":
		return Quality2160p
	case "1440p":
		return Quality1440p
	case "1080p":
		return Quality1080p
	case "1080i":
		return Quality1080i
	case "720p":
		return Quality720p
	case "576p":
		return Quality576p
	case "480p":
		return Quality480p
	default:
		return QualityUnknown
	}
}

// Normalise folds the separators release names use into single spaces and
// lowercases the result.
//
// Everything downstream matches against this rather than the raw name, so that
// punctuation differences between two uploads of the same release do not
// change the outcome.
func Normalise(name string) string {
	s := strings.ToLower(name)
	s = separatorRe.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// HasEpisodeNumber reports whether the name carries a specific episode, which
// is what distinguishes a season pack from a single episode.
func (p Parsed) HasEpisodeNumber() bool { return p.HasEpisode }
