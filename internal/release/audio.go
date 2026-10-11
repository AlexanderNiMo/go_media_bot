package release

import "strings"

// AudioTier ranks a release's Russian audio.
//
// Higher is better. The tiers exist rather than a boolean because "has Russian
// audio" is not the interesting question — "is this a proper dub or a
// single-voice amateur reading" is, and the difference is what a user notices
// within thirty seconds of pressing play.
type AudioTier int

const (
	// AudioNone means no Russian audio was detected. It may still be there —
	// release names are free text — but nothing in the name says so.
	AudioNone AudioTier = iota
	// AudioSubsOnly means the release advertises Russian subtitles and no
	// Russian audio. This is the case the whole detection exists for: a
	// "ENG + Rus subs" release satisfies a naive "does it mention Russian"
	// check while being exactly what the user did not ask for.
	AudioSubsOnly
	// AudioVoiceOver is an amateur or unlabelled voice-over.
	AudioVoiceOver
	// AudioSingle is a single-voice reading, including the author's own.
	AudioSingle
	// AudioDual is a two-voice reading.
	AudioDual
	// AudioMulti is a professional multi-voice reading.
	AudioMulti
	// AudioDub is a full dub, the best case.
	AudioDub
)

// String renders the tier for logs and the UI.
func (t AudioTier) String() string {
	switch t {
	case AudioSubsOnly:
		return "subtitles only"
	case AudioVoiceOver:
		return "voice-over"
	case AudioSingle:
		return "single-voice"
	case AudioDual:
		return "two-voice"
	case AudioMulti:
		return "multi-voice"
	case AudioDub:
		return "dub"
	default:
		return "none"
	}
}

// AudioTrack is what the detector concluded about a release's audio.
type AudioTrack struct {
	Tier AudioTier

	// Marker is the specific string that decided the tier, kept so a user
	// asking "why did it pick this one" gets an answer.
	Marker string
	// Studios are the dubbing studios named in the release.
	Studios []string

	// HasSubs records that Russian subtitles were advertised. It is not by
	// itself a problem: most good releases have them.
	HasSubs bool

	// ImpliedByTracker records that nothing in the name says Russian, but the
	// release came from a tracker that is Russian-language by nature.
	//
	// This is a weaker signal than a marker and is scored as such. It exists
	// because plenty of releases on those trackers are described only by their
	// technical parameters, and refusing every one of them would make the
	// project useless on exactly the trackers it targets.
	ImpliedByTracker bool

	// IndicatesEnglish records that the name explicitly advertises English
	// audio. Only meaningful when no Russian marker was found.
	IndicatesEnglish bool
}

// Usable reports whether the release can be downloaded at all.
func (t AudioTrack) Usable(requireRussian bool, allowImplied bool) bool {
	if !requireRussian {
		return true
	}
	switch {
	case t.Tier >= AudioVoiceOver:
		return true
	case t.ImpliedByTracker && allowImplied:
		// English is advertised explicitly, so the tracker's nationality does
		// not help.
		return !t.IndicatesEnglish
	default:
		return false
	}
}

// Describe explains the decision in one line, for the event log.
func (t AudioTrack) Describe() string {
	switch {
	case t.Tier >= AudioVoiceOver:
		s := t.Tier.String()
		if t.Marker != "" {
			s += " (" + t.Marker + ")"
		}
		if len(t.Studios) > 0 {
			s += " [" + strings.Join(t.Studios, ", ") + "]"
		}
		return s
	case t.ImpliedByTracker:
		return "no audio marker, but the tracker is Russian-language"
	case t.HasSubs:
		return "Russian subtitles but no Russian audio"
	default:
		return "no Russian audio detected"
	}
}

// DetectAudio inspects a release name for evidence of Russian audio.
//
// The order of the checks is the substance of this function, not an
// implementation detail:
//
//  1. Studios first, because a named studio is the strongest evidence there
//     is — a release credited to LostFilm has LostFilm's audio.
//  2. Explicit tier markers next, most specific first. "Многоголосый
//     закадровый" must be read as multi-voice, so the multi-voice check has to
//     run before the voice-over one.
//  3. Subtitles last, and only to distinguish "Russian subtitles" from
//     "nothing Russian at all". A release can legitimately have both Russian
//     audio and Russian subtitles.
func DetectAudio(name string, indexer string, profile Profile) AudioTrack {
	normalised := Normalise(name)
	track := AudioTrack{}

	studios, studioTier := profile.matchStudios(normalised)
	track.Studios = studios
	track.Tier = studioTier
	if len(studios) > 0 {
		track.Marker = studios[0]
	}

	if tier, marker := profile.matchAudioTier(normalised); tier > track.Tier {
		track.Tier = tier
		track.Marker = marker
	}

	track.HasSubs = profile.hasSubsMarker(normalised)
	track.IndicatesEnglish = profile.hasEnglishMarker(normalised)

	if track.Tier == AudioNone && track.HasSubs {
		track.Tier = AudioSubsOnly
	}

	if track.Tier == AudioNone && profile.IsRussianTracker(indexer) {
		track.ImpliedByTracker = true
	}

	return track
}
