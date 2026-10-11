package release

import "testing"

func TestDetectAudioTiers(t *testing.T) {
	p := DefaultProfile()

	for _, tc := range []struct {
		name  string
		tier  AudioTier
		descr string
	}{
		// Full dubs.
		{"Матрица (1999) 1080p BDRemux | Дубляж", AudioDub, "дубляж"},
		{"Матрица (1999) 1080p | Лицензия", AudioDub, "лицензия"},
		{"Матрица (1999) 1080p | Дублированный", AudioDub, "дублированный"},

		// Multi-voice, in the forms these actually appear in.
		{"Матрица (1999) 1080p | Многоголосый", AudioMulti, "многоголосый"},
		{"Матрица (1999) 1080p | Многоголосый закадровый", AudioMulti, "многоголосый закадровый"},
		{"Матрица (1999) 1080p | Многоголосая", AudioMulti, "inflected"},
		{"Матрица (1999) 1080p | MVO", AudioMulti, "mvo"},
		{"Матрица (1999) 1080p | МВО", AudioMulti, "мво"},

		// Two-voice.
		{"Матрица (1999) 1080p | Двухголосый", AudioDual, "двухголосый"},
		{"Матрица (1999) 1080p | DVO", AudioDual, "dvo"},

		// Single-voice.
		{"Матрица (1999) 1080p | Одноголосый", AudioSingle, "одноголосый"},
		{"Матрица (1999) 1080p | Авторский перевод", AudioSingle, "авторский"},
		{"Матрица (1999) 1080p | AVO", AudioSingle, "avo"},

		// Voice-over.
		{"Матрица (1999) 1080p | Закадровый", AudioVoiceOver, "закадровый"},
		{"Матрица (1999) 1080p | Любительский", AudioVoiceOver, "любительский"},

		// Studios.
		{"Игра престолов S02 1080p | LostFilm", AudioMulti, "lostfilm"},
		{"Игра престолов S02 1080p | HDrezka Studio", AudioMulti, "hdrezka"},
		{"Матрица (1999) 1080p | Кубик в Кубе", AudioDual, "two-voice studio"},
		{"Матрица (1999) 1080p | Амедиа", AudioDub, "dub studio"},

		// Nothing Russian.
		{"The.Matrix.1999.1080p.BluRay.x264-RARBG", AudioNone, "english only"},
	} {
		got := DetectAudio(tc.name, "RuTracker", p)
		if got.Tier != tc.tier {
			t.Errorf("%s (%q): tier = %v, want %v (marker %q)",
				tc.descr, tc.name, got.Tier, tc.tier, got.Marker)
		}
	}
}

// TestMultiVoiceWinsOverVoiceOver is the ordering that matters most inside the
// detector: "Многоголосый закадровый" carries a marker from both groups, and
// reading it as a plain voice-over would rank a professional multi-voice track
// below an amateur one.
func TestMultiVoiceWinsOverVoiceOver(t *testing.T) {
	p := DefaultProfile()

	for _, name := range []string{
		"Матрица (1999) 1080p | Многоголосый закадровый",
		"Матрица (1999) 1080p | Профессиональный многоголосый закадровый",
		"Матрица (1999) 1080p | Многоголосый любительский",
	} {
		if got := DetectAudio(name, "", p); got.Tier != AudioMulti {
			t.Errorf("%q: tier = %v, want multi-voice", name, got.Tier)
		}
	}
}

// TestSubtitlesAreNotAudio is the whole reason this detector exists.
//
// A release advertising "ENG + Rus subs" satisfies any check that asks "does
// the name mention Russian", while being exactly what the user did not ask
// for. Getting this wrong means every English release with subtitles gets
// downloaded and the filter looks broken.
func TestSubtitlesAreNotAudio(t *testing.T) {
	p := DefaultProfile()

	for _, name := range []string{
		"The.Matrix.1999.1080p.BluRay.x264 | ENG + Rus subs",
		"The.Matrix.1999.1080p.WEB-DL | RUS SUB",
		"The.Matrix.1999.1080p | Русские субтитры",
		"The.Matrix.1999.1080p | Rus Sub",
		"The.Matrix.1999.1080p | SRT",
	} {
		got := DetectAudio(name, "RuTracker", p)
		if got.Tier != AudioSubsOnly {
			t.Errorf("%q: tier = %v, want subtitles-only", name, got.Tier)
		}
		if got.Usable(true, true) {
			t.Errorf("%q: a subtitles-only release must not be usable when Russian audio is required", name)
		}
	}
}

// TestAudioAndSubtitlesCanCoexist — most good Russian releases carry Russian
// subtitles as well, and that must not turn them into a subtitles-only
// release.
func TestAudioAndSubtitlesCanCoexist(t *testing.T) {
	p := DefaultProfile()

	name := "Матрица (1999) 1080p BDRemux | Дубляж | Rus subs"
	got := DetectAudio(name, "", p)

	if got.Tier != AudioDub {
		t.Errorf("tier = %v, want dub: subtitles alongside audio are normal", got.Tier)
	}
	if !got.HasSubs {
		t.Error("HasSubs should still be recorded")
	}
}

func TestEnglishMarkerOnRussianTrackerIsRefused(t *testing.T) {
	p := DefaultProfile()

	// Nothing Russian in the name, but the tracker is Russian — this is
	// normally accepted. An explicit English marker overrides that, because
	// the release has told us what its audio is.
	got := DetectAudio("The Matrix 1999 1080p BluRay | Eng", "RuTracker", p)

	if !got.IndicatesEnglish {
		t.Fatal("the English marker was not detected")
	}
	if got.Usable(true, true) {
		t.Error("a release advertising English audio must not pass on tracker reputation alone")
	}
}

// TestImpliedByTracker — plenty of releases on Russian trackers are described
// only by their technical parameters. Refusing all of them would make the
// project useless on precisely the trackers it targets.
func TestImpliedByTracker(t *testing.T) {
	p := DefaultProfile()

	got := DetectAudio("Матрица (1999) 1080p BDRemux", "RuTracker", p)

	if got.Tier != AudioNone {
		t.Fatalf("tier = %v, want none: nothing in the name says Russian", got.Tier)
	}
	if !got.ImpliedByTracker {
		t.Error("the tracker should have been recognised as Russian")
	}
	if !got.Usable(true, true) {
		t.Error("an unmarked release from a Russian tracker should be usable when implied audio is allowed")
	}
	// But not when the policy says markers are required.
	if got.Usable(true, false) {
		t.Error("a tracker-implied release must not pass when implied audio is disallowed")
	}

	// And the same release from an international tracker is not usable.
	if got := DetectAudio("The Matrix 1999 1080p BDRemux", "1337x", p); got.Usable(true, true) {
		t.Error("an unmarked release from a non-Russian tracker must not be usable")
	}
}

func TestRequireRussianAudioOff(t *testing.T) {
	p := DefaultProfile()

	got := DetectAudio("The.Matrix.1999.1080p.BluRay.x264-RARBG", "1337x", p)
	if !got.Usable(false, false) {
		t.Error("with the requirement off, any release is usable")
	}
}

func TestIsRussianTracker(t *testing.T) {
	p := DefaultProfile()

	for _, name := range []string{"RuTracker", "rutracker.org", "Kinozal", "NNMClub", "rutor.info", "Tapochek"} {
		if !p.IsRussianTracker(name) {
			t.Errorf("%q should be recognised as a Russian tracker", name)
		}
	}
	for _, name := range []string{"1337x", "ThePirateBay", "YTS", "RARBG", ""} {
		if p.IsRussianTracker(name) {
			t.Errorf("%q should not be recognised as a Russian tracker", name)
		}
	}
}

// TestShortMarkersNeedWholeWords — "vo", "sub" and "srt" are ordinary letter
// sequences that turn up inside other words. Matching them as substrings
// produces constant false positives.
func TestShortMarkersNeedWholeWords(t *testing.T) {
	if containsMarker("the matrix providence", "vo") {
		t.Error(`"vo" matched inside "providence"`)
	}
	if containsMarker("subtitled release", "sub") {
		t.Error(`"sub" matched inside "subtitled"`)
	}
	if !containsMarker("matrix 1999 vo", "vo") {
		t.Error(`"vo" should match as a standalone token`)
	}

	// Long markers are stems and must match inflections.
	for _, form := range []string{"многоголосый", "многоголосая", "многоголосном"} {
		if !containsMarker(form, "многоголос") {
			t.Errorf("the stem should match %q", form)
		}
	}
}

func TestDescribeIsHumanReadable(t *testing.T) {
	p := DefaultProfile()

	got := DetectAudio("Матрица (1999) 1080p | Дубляж", "", p)
	if d := got.Describe(); d != "dub (дубляж)" {
		t.Errorf("Describe = %q", d)
	}

	got = DetectAudio("The Matrix 1080p | ENG + Rus subs", "", p)
	if d := got.Describe(); d != "Russian subtitles but no Russian audio" {
		t.Errorf("Describe = %q", d)
	}
}

func TestAudioTierOrdering(t *testing.T) {
	// The ordering is the contract the scorer depends on; asserting it here
	// means a reordering of the constants cannot pass unnoticed.
	ordered := []AudioTier{
		AudioNone, AudioSubsOnly, AudioVoiceOver,
		AudioSingle, AudioDual, AudioMulti, AudioDub,
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i] <= ordered[i-1] {
			t.Errorf("%v should rank above %v", ordered[i], ordered[i-1])
		}
	}
}
