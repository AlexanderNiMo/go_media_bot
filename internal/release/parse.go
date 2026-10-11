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

	p.HDR = hdrRe.MatchString(p.Normalised)
	p.DolbyVision = dvRe.MatchString(p.Normalised)

	return p
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
