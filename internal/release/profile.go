package release

import (
	"strings"

	"github.com/alex/mediad/internal/config"
)

// Profile is the acceptance policy: what counts as a good release.
//
// It is built once from configuration and then passed by value, so selection
// stays a pure function of its arguments.
type Profile struct {
	MinResolution       Quality
	PreferredResolution Quality

	MinSeeders int
	MinSizeGB  float64
	MaxSizeGB  float64

	// Per-type size caps. A 50 GB film is almost always a remux of a
	// theatrical master, and a 50 GB single episode is not a real
	// release. Zero means "use the general MaxSizeGB above", which
	// keeps old configurations behaving as before.
	MaxSizeGBMovies float64
	MaxSizeGBSeries float64

	ExcludedSources []Source

	RequireRussianAudio bool
	// AllowImpliedAudio accepts a release with no audio marker at all when it
	// comes from a Russian-language tracker. See AudioTrack.ImpliedByTracker.
	AllowImpliedAudio bool

	// studioTiers maps a dubbing studio to the audio tier its work implies.
	studioTiers map[string]AudioTier
	// audioMarkers are matched most specific tier first.
	audioMarkers []markerGroup
	subsMarkers  []string
	// englishMarkers and englishGroups are only consulted when no Russian
	// marker was found, so they can never override a release that does say it
	// has Russian audio.
	englishMarkers  []string
	englishGroups   []string
	russianTrackers []string
}

type markerGroup struct {
	tier    AudioTier
	markers []string
}

// DefaultProfile is the policy used when nothing overrides it.
//
// The marker tables are data rather than logic and are expected to be edited:
// they encode what Russian trackers actually write in release names, which is
// folk knowledge that shifts over time.
func DefaultProfile() Profile {
	return Profile{
		MinResolution:       Quality1080p,
		PreferredResolution: Quality2160p,

		MinSeeders: 3,
		MinSizeGB:  1.5,
		MaxSizeGB:  60,

		ExcludedSources: []Source{
			SourceCam, SourceTelesync, SourceScreener,
		},

		RequireRussianAudio: true,
		AllowImpliedAudio:   true,

		studioTiers:     defaultStudioTiers(),
		audioMarkers:    defaultAudioMarkers(),
		subsMarkers:     defaultSubsMarkers(),
		englishMarkers:  defaultEnglishMarkers(),
		englishGroups:   defaultEnglishGroups(),
		russianTrackers: defaultRussianTrackers(),
	}
}

// defaultStudioTiers maps studios to the tier their work implies.
//
// A studio credit is the strongest evidence a release name carries: a release
// attributed to LostFilm has LostFilm's voice track, whatever else the name
// says or omits. The tiers are a judgement about each studio's usual output,
// and the table is meant to be corrected when it is wrong.
func defaultStudioTiers() map[string]AudioTier {
	return map[string]AudioTier{
		// Full dubs.
		"амедиа":        AudioDub,
		"amedia":        AudioDub,
		"дублированный": AudioDub,

		// Two-voice studios. Кубик в Кубе is two readers by definition, which
		// is why it is not filed with the multi-voice houses.
		"кубик в кубе": AudioDual,
		"kubik v kube": AudioDual,

		// Multi-voice studios.
		"lostfilm":         AudioMulti,
		"hdrezka":          AudioMulti,
		"jaskier":          AudioMulti,
		"alexfilm":         AudioMulti,
		"newstudio":        AudioMulti,
		"anilibria":        AudioMulti,
		"baibako":          AudioMulti,
		"coldfilm":         AudioMulti,
		"gears media":      AudioMulti,
		"ideafilm":         AudioMulti,
		"kerob":            AudioMulti,
		"lakefilms":        AudioMulti,
		"newcomers":        AudioMulti,
		"paradox":          AudioMulti,
		"shiza project":    AudioMulti,
		"viruseproject":    AudioMulti,
		"zames":            AudioMulti,
		"gremlin":          AudioMulti,
		"студия кириллица": AudioMulti,
	}
}

// defaultAudioMarkers lists the tier markers, ordered most specific first.
//
// The stems are deliberate: "многоголос" matches многоголосый, многоголосая
// and многоголосном, and release names inflect freely.
//
// The ordering is load-bearing. "Многоголосый закадровый" contains both a
// multi-voice and a voice-over marker, and must be read as multi-voice — so
// the multi-voice group has to come first.
func defaultAudioMarkers() []markerGroup {
	return []markerGroup{
		{AudioDub, []string{"дубляж", "дублир", "лицензи", "полное дублирование", "dub"}},
		{AudioMulti, []string{"многоголос", "mvo", "мво"}},
		{AudioDual, []string{"двухголос", "двуголос", "dvo", "дво"}},
		{AudioSingle, []string{"одноголос", "авторск", "avo", "аво"}},
		{AudioVoiceOver, []string{"закадров", "любительск", "vo"}},
	}
}

// defaultSubsMarkers lists what advertises subtitles.
//
// Note that a release carrying Russian subtitles is not a problem in itself —
// most good releases have them. The markers matter only when no Russian audio
// marker was found, where they turn "nothing Russian" into the much more
// specific "Russian subtitles and no Russian audio".
func defaultSubsMarkers() []string {
	return []string{
		"субтитр", "sub", "subs", "srt", "пс",
		"rus sub", "ru sub", "rus subs", "ru subs",
	}
}

// defaultEnglishMarkers are explicit statements of English audio.
func defaultEnglishMarkers() []string {
	return []string{
		"eng", "english", "английский", "оригинал", "original",
		"original audio", "оригинальная звуковая дорожка",
	}
}

// defaultEnglishGroups lists release groups whose output is English-language.
//
// This is the same kind of evidence as a dubbing studio, pointing the other
// way: a release credited to RARBG has RARBG's usual English audio whatever
// tracker it was found on. Without it, the "the tracker is Russian, so an
// unmarked release is probably Russian" rule quietly accepts English scene
// releases from Russian trackers, which is precisely the failure the audio
// detection exists to prevent.
//
// Only consulted when the name carries no Russian marker, so the cost of a
// wrong entry is a missed release rather than a wrong download.
func defaultEnglishGroups() []string {
	return []string{
		"rarbg", "yts", "yify", "evo", "sparks", "geckos", "amiable",
		"psa", "galaxyrg", "intenso", "playweb", "cmrg", "edith", "tepes",
		"kogi", "santi", "nodlabs", "framestor", "trollhd", "qoq", "ntb",
		"w4f", "wf2", "decibel", "epsilon", "eureka", "success",
		"ntg", "btd", "stuttershock",
	}
}

// defaultRussianTrackers lists trackers that are Russian-language by nature.
func defaultRussianTrackers() []string {
	return []string{
		"rutracker", "rutor", "kinozal", "nnmclub", "tapochek",
		"megapeer", "mega-peer", "korsars", "unionpeer", "seedoff",
		"torrent9", "baibako", "lostfilm",
	}
}

// IsRussianTracker reports whether an indexer name is one of them.
func (p Profile) IsRussianTracker(indexer string) bool {
	if indexer == "" {
		return false
	}
	name := Normalise(indexer)
	for _, t := range p.russianTrackers {
		if strings.Contains(name, t) {
			return true
		}
	}
	return false
}

// matchStudios returns the studios named in a release and the best tier among
// them.
func (p Profile) matchStudios(normalised string) ([]string, AudioTier) {
	var (
		found []string
		tier  = AudioNone
	)
	for studio, studioTier := range p.studioTiers {
		if strings.Contains(normalised, studio) {
			found = append(found, studio)
			if studioTier > tier {
				tier = studioTier
			}
		}
	}
	return found, tier
}

// matchAudioTier returns the best tier any marker supports.
func (p Profile) matchAudioTier(normalised string) (AudioTier, string) {
	for _, group := range p.audioMarkers {
		for _, marker := range group.markers {
			if containsMarker(normalised, marker) {
				return group.tier, marker
			}
		}
	}
	return AudioNone, ""
}

func (p Profile) hasSubsMarker(normalised string) bool {
	for _, marker := range p.subsMarkers {
		if containsMarker(normalised, marker) {
			return true
		}
	}
	return false
}

func (p Profile) hasEnglishMarker(normalised string) bool {
	for _, marker := range p.englishMarkers {
		if containsMarker(normalised, marker) {
			return true
		}
	}
	// A scene release is suffixed with its group, so the group name has to be
	// matched as a token rather than as a substring: "rarbg" appears in
	// "Movie.2020.1080p-RARBG", where it is not delimited by spaces.
	for _, group := range p.englishGroups {
		if containsGroupSuffix(normalised, group) {
			return true
		}
	}
	return false
}

// containsGroupSuffix reports whether a release group named at the end of a
// scene-style name.
//
// Scene names end with "-GROUP", which normalisation turns into " group", so
// that is what is looked for — not a bare substring, which would match a group
// name appearing anywhere in a title.
func containsGroupSuffix(normalised, group string) bool {
	return strings.Contains(normalised, " "+group) ||
		strings.HasSuffix(normalised, group)
}

// containsMarker looks for a marker in an already-normalised name.
//
// Short markers are matched as whole tokens and longer ones as substrings.
// The split matters: "vo" and "sub" are ordinary letter sequences that turn up
// inside other words, while "многоголос" is a stem that has to match
// inflections. Getting this backwards produces either constant false positives
// or constant misses.
func containsMarker(normalised, marker string) bool {
	if strings.ContainsRune(marker, ' ') {
		return strings.Contains(normalised, marker)
	}
	if len([]rune(marker)) <= 4 {
		for _, field := range strings.Fields(normalised) {
			if field == marker {
				return true
			}
		}
		return false
	}
	return strings.Contains(normalised, marker)
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

// ProfileFromConfig builds the policy from the application configuration.
//
// The marker tables are not exposed in configuration: they are domain
// knowledge about how Russian trackers write release names, not a per-user
// preference. What configuration does control is the policy built on top of
// them, plus additive extras for a tracker whose conventions a user knows
// better than this table does.
func ProfileFromConfig(cfg *config.Config) Profile {
	p := DefaultProfile()

	q := cfg.Quality
	if q.MinResolution > 0 {
		p.MinResolution = QualityFromHeight(q.MinResolution)
	}
	if q.PreferredResolution > 0 {
		p.PreferredResolution = QualityFromHeight(q.PreferredResolution)
	}
	// A preferred resolution below the minimum would make scoring
	// nonsensical; the minimum wins.
	if p.PreferredResolution < p.MinResolution {
		p.PreferredResolution = p.MinResolution
	}

	if q.MinSeeders > 0 {
		p.MinSeeders = q.MinSeeders
	}
	if q.MinSizeGB > 0 {
		p.MinSizeGB = q.MinSizeGB
	}
	if q.MaxSizeGBMovies > 0 {
		p.MaxSizeGBMovies = q.MaxSizeGBMovies
	}
	if q.MaxSizeGBSeries > 0 {
		p.MaxSizeGBSeries = q.MaxSizeGBSeries
	}
	if q.MaxSizeGB > 0 {
		p.MaxSizeGB = q.MaxSizeGB
	}
	if len(q.ExcludedSources) > 0 {
		p.ExcludedSources = parseSources(q.ExcludedSources)
	}

	p.RequireRussianAudio = q.RequireRussianAudio

	return p
}

// parseSources maps configured source names onto the enum.
//
// An unrecognised name is skipped rather than rejected: a typo in a config
// file should not stop the service, and the alternative — failing to start —
// is a worse outcome than excluding one source too few.
func parseSources(names []string) []Source {
	out := make([]Source, 0, len(names))
	for _, name := range names {
		if s, ok := sourceByName(name); ok {
			out = append(out, s)
		}
	}
	return out
}

func sourceByName(name string) (Source, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cam", "camrip":
		return SourceCam, true
	case "ts", "hdts", "telesync":
		return SourceTelesync, true
	case "scr", "screener", "dvdscr":
		return SourceScreener, true
	case "dvdrip", "dvd":
		return SourceDVD, true
	case "hdtv":
		return SourceHDTV, true
	case "webrip":
		return SourceWebRip, true
	case "web-dl", "webdl", "web":
		return SourceWebDL, true
	case "bluray", "blu-ray", "bd":
		return SourceBluRay, true
	case "bdrip":
		return SourceBDRip, true
	case "bdremux", "remux":
		return SourceBDRemux, true
	default:
		return SourceUnknown, false
	}
}

// Excludes reports whether a source is one the profile refuses.
func (p Profile) Excludes(s Source) bool {
	for _, excluded := range p.ExcludedSources {
		if excluded == s {
			return true
		}
	}
	return false
}
