package web

import (
	"testing"
	"time"

	"github.com/alex/mediad/internal/release"
	"github.com/alex/mediad/internal/storage"
)

// The vocabulary tests.
//
// The interface shows machine-readable codes — a state, a rejection reason —
// and this is the layer that turns them into words. The failure mode is
// silent: a code with no label reaches the page as "too_few_seeders", which
// looks like a bug to whoever reads it and like a technical detail to whoever
// wrote it.
//
// So the tests here are mostly about completeness rather than correctness.
// A new code upstream has to produce a decision here, or the test fails.

// TestEveryStateHasALabel checks that every state the system can be in has a
// Russian name.
//
// A state with no label falls through to its own code, so the check is that
// the label is not the code.
func TestEveryStateHasALabel(t *testing.T) {
	for _, s := range storage.AllStates() {
		label := stateLabel(s)
		if label == "" {
			t.Errorf("state %q has no label", s)
			continue
		}
		if label == string(s) {
			t.Errorf("state %q has no Russian label; the page would show %q", s, label)
		}
	}
}

// TestEveryReasonHasALabel checks the rejection reasons.
//
// This is the test that catches a reason added to the selector without a
// word for it.
func TestEveryReasonHasALabel(t *testing.T) {
	for _, r := range release.AllReasons() {
		code := string(r)
		label, ok := reasonLabels[code]
		if !ok {
			t.Errorf("reason %q has no Russian label; the page would show the code", code)
			continue
		}
		if label == "" {
			t.Errorf("reason %q has an empty label", code)
		}
		if got := reasonLabel(code); got != label {
			t.Errorf("reason %q renders as %q, want %q", code, got, label)
		}
	}
}

// TestEveryAudioTierHasALabel checks the audio tiers.
//
// The tiers are the difference somebody notices within thirty seconds of
// pressing play, so none of them may fall through.
func TestEveryAudioTierHasALabel(t *testing.T) {
	for tier := release.AudioNone; tier <= release.AudioDub; tier++ {
		label := audioLabel(tier)
		if label == "" {
			t.Errorf("audio tier %v has no label", tier)
			continue
		}
		if label == tier.String() {
			t.Errorf("audio tier %v has no Russian label; the page would show %q", tier, label)
		}
	}
}

// TestReasonToneAndOverride checks how a refusal is presented.
//
// The hard reasons are refusals on principle: a camrip, a blacklisted hash, a
// release with Russian subtitles and no Russian audio. Offering to download
// one of those would undo the decision the product exists to make, so those
// rows get no action and read as danger. The soft ones are refusals on
// quality, and the person may reasonably want the release anyway.
func TestReasonToneAndOverride(t *testing.T) {
	for _, r := range release.AllReasons() {
		code := string(r)
		tone := reasonTone(code)
		override := reasonAllowsOverride(code)

		switch tone {
		case "danger", "warn", "neutral":
		default:
			t.Errorf("reason %q has tone %q, which is not one of the three", code, tone)
		}

		if hardReasons[code] && (tone != "danger" || override) {
			t.Errorf("reason %q is refused on principle but reads as %q with override=%v",
				code, tone, override)
		}
		if !hardReasons[code] && code != "not_an_update" && tone == "danger" {
			t.Errorf("reason %q is refused on quality but reads as danger", code)
		}
	}
}

// TestSplitReason checks the shape of a stored rejection reason.
//
// The stored form is "code: detail", and the code never contains a colon, so
// the first one is the boundary.
func TestSplitReason(t *testing.T) {
	for _, tc := range []struct {
		stored string
		code   string
		detail string
	}{
		{"too_few_seeders: 2 seeders, minimum is 3", "too_few_seeders", "2 seeders, minimum is 3"},
		{"blacklisted", "blacklisted", ""},
		{"", "", ""},
		{"unknown_thing: some detail", "unknown_thing", "some detail"},
		{"no_russian_audio: the name mentions no Russian marker at all",
			"no_russian_audio", "the name mentions no Russian marker at all"},
	} {
		code, detail := splitReason(tc.stored)
		if code != tc.code || detail != tc.detail {
			t.Errorf("splitReason(%q) = (%q, %q), want (%q, %q)",
				tc.stored, code, detail, tc.code, tc.detail)
		}
	}
}

// TestReasonLabelFallsBackToTheCode checks what an unknown reason shows.
//
// An untranslated reason shows its code rather than nothing: a blank cell
// says "this does not apply", which is a lie, and a raw code is loud enough
// to be noticed and fixed.
func TestReasonLabelFallsBackToTheCode(t *testing.T) {
	if got := reasonLabel("some_reason_nobody_translated"); got != "some_reason_nobody_translated" {
		t.Errorf("an unknown reason renders as %q, want the raw code", got)
	}
	if got := reasonLabel(""); got != "" {
		t.Errorf("an empty reason renders as %q, want nothing", got)
	}
}

// TestPluralForms checks the Russian plural rules.
//
// The last two digits decide the form, not just the last one: 21 takes the
// first form and 11 the third.
func TestPluralForms(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "0 минут"},
		{1, "1 минута"},
		{2, "2 минуты"},
		{4, "4 минуты"},
		{5, "5 минут"},
		{11, "11 минут"},
		{12, "12 минут"},
		{14, "14 минут"},
		{21, "21 минута"},
		{22, "22 минуты"},
		{25, "25 минут"},
		{101, "101 минута"},
		{111, "111 минут"},
	} {
		if got := nounMinute.count(tc.n); got != tc.want {
			t.Errorf("nounMinute.count(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestPluralAccusative checks the singular form after a verb.
//
// "осталось 1 минуту" and "1 минуту назад" — the accusative differs from the
// nominative in the singular only.
func TestPluralAccusative(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{1, "1 минуту"},
		{2, "2 минуты"},
		{5, "5 минут"},
		{21, "21 минуту"},
		{22, "22 минуты"},
	} {
		if got := nounMinute.countA(tc.n); got != tc.want {
			t.Errorf("nounMinute.countA(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}

	// The other nouns take the same form in both cases.
	if got := nounHour.countA(1); got != "1 час" {
		t.Errorf("nounHour.countA(1) = %q, want %q", got, "1 час")
	}
	if got := nounDay.countA(5); got != "5 дней" {
		t.Errorf("nounDay.countA(5) = %q, want %q", got, "5 дней")
	}
}

// TestDurationHuman checks the age rendering.
func TestDurationHuman(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "меньше минуты"},
		{5 * time.Minute, "5 минут"},
		{90 * time.Minute, "1 час 30 минут"},
		{2 * time.Hour, "2 часа"},
		{3 * 24 * time.Hour, "3 дня"},
		{45 * 24 * time.Hour, "1 месяц 15 дней"},
		{400 * 24 * time.Hour, "1 год 1 месяц"},
		{-time.Hour, "меньше минуты"},
	} {
		if got := durationHuman(tc.d); got != tc.want {
			t.Errorf("durationHuman(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestRelativeTime checks a moment rendered against the clock.
//
// The future direction is the one the product lives on: a parked request says
// when it will look again, and "только что" for a check six hours out — which
// is what the old helper rendered — says the opposite.
func TestRelativeTime(t *testing.T) {
	now := time.Now()

	if got := relativeTime(now.Add(6 * time.Hour)); got != "через 6 часов" {
		t.Errorf("relativeTime(+6h) = %q, want %q", got, "через 6 часов")
	}
	if got := relativeTime(now.Add(30 * time.Minute)); got != "через 30 минут" {
		t.Errorf("relativeTime(+30m) = %q, want %q", got, "через 30 минут")
	}
	if got := relativeTime(now.Add(-2 * time.Hour)); got != "2 часа назад" {
		t.Errorf("relativeTime(-2h) = %q, want %q", got, "2 часа назад")
	}
}
