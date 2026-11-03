package web

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/alex/mediad/internal/release"
	"github.com/alex/mediad/internal/storage"
)

// The user-facing vocabulary.
//
// Every word a person reads on a page is decided here rather than inlined in
// a template. Two reasons. A template that inlines Russian text cannot be
// checked, and the machine-readable forms — a state code, a rejection reason —
// leak into the interface one template at a time, until the page says
// "too_few_seeders" to somebody who just wanted to know why nothing arrived.
//
// The maps below are deliberately complete. A test asserts that every state,
// every rejection reason and every audio tier has a label, so a new code
// cannot be added upstream without somebody deciding what to call it here.

// stateLabel is the Russian name of a request state.
//
// The words are short because they sit in a pill, and several of them sit in a
// filter row side by side.
func stateLabel(s storage.State) string {
	switch s {
	case storage.StatePending:
		return "в очереди"
	case storage.StateSearching:
		return "ищет"
	case storage.StateAdding:
		return "добавляет"
	case storage.StateDownloading:
		return "качает"
	case storage.StateCompleted:
		return "готово"
	case storage.StateFailed:
		return "ошибка"
	case storage.StateCanceled:
		return "отменён"
	case storage.StateSkipped:
		return "пропущен"
	case storage.StateManual:
		return "ждёт выбора"
	case storage.StateOngoing:
		return "ждёт остальные серии"
	default:
		// An unknown state is shown as its own code rather than as nothing:
		// a blank cell says "this does not apply", which is a lie.
		return string(s)
	}
}

// stateClass is the CSS modifier for a state's pill.
//
// The code is the class, so the stylesheet names states the same way the code
// does and there is no second vocabulary to keep in sync. Anything that is not
// a letter, a digit or a dash is dropped: the value lands in a class
// attribute, and a stray quote there would break the page.
func stateClass(s storage.State) string {
	var b strings.Builder
	for _, r := range string(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// reasonLabels is the Russian name of every rejection reason the selector can
// produce.
//
// The keys are the codes from internal/release. A reason the selector invents
// later has no entry here, and the interface then shows the raw code — which
// is loud enough to be noticed and fixed, which is the point of the
// completeness test.
var reasonLabels = map[string]string{
	"excluded_source":    "источник исключён",
	"resolution_too_low": "ниже минимального разрешения",
	"unknown_quality":    "не удалось определить разрешение",
	"too_few_seeders":    "мало сидеров",
	"too_small":          "слишком маленький",
	"too_large":          "слишком большой",
	"implausible_size":   "размер не соответствует качеству",
	"blacklisted":        "в чёрном списке",
	"no_russian_audio":   "нет русской дорожки",
	"subtitles_only":     "только русские субтитры",
	"wrong_season":       "другой сезон",
	"single_episode":     "это одна серия, а запрошен сезон",
	"wrong_year":         "другой год",
	"no_download_link":   "нет ссылки на загрузку",
	"not_an_update":      "не обновление текущего пака",
}

// hardReasons are the reasons a release is refused on principle.
//
// The difference matters to the person choosing: a release refused because it
// is a camrip or because it is already in the blacklist is one the interface
// should not offer at all, while a release refused for having two seeders
// instead of three is one they may reasonably want anyway.
var hardReasons = map[string]bool{
	"excluded_source":    true,
	"resolution_too_low": true,
	"implausible_size":   true,
	"blacklisted":        true,
	"no_russian_audio":   true,
	"subtitles_only":     true,
	"wrong_season":       true,
	"single_episode":     true,
	"wrong_year":         true,
}

// reasonLabel is the Russian name of a rejection reason.
//
// The stored form is "code: detail" — the selector's machine code followed by
// an English sentence carrying the numbers. The code is what the map is keyed
// on; the detail is kept whole, for the tooltip, where the exact figures are
// what somebody deciding to override the rule wants to see.
func reasonLabel(stored string) string {
	code, _ := splitReason(stored)
	if label, ok := reasonLabels[code]; ok {
		return label
	}
	if code == "" {
		return stored
	}
	return code
}

// reasonTone is the severity of a rejection reason.
//
// "danger" is a refusal on principle: the interface offers no way to pick the
// release. "warn" is a refusal on quality: the release is worse than the
// policy asks for, and the person may want it anyway.
func reasonTone(stored string) string {
	code, _ := splitReason(stored)
	switch {
	case hardReasons[code]:
		return "danger"
	case code == "":
		return "neutral"
	default:
		return "warn"
	}
}

// reasonAllowsOverride reports whether a refused release may still be picked.
//
// The hard reasons are refusals the selector makes on principle — a camrip, a
// blacklisted hash, a release with Russian subtitles and no Russian audio —
// and offering to download one of those would undo the decision the product
// exists to make.
//
// Everything else may be overruled: a release refused for having two seeders
// instead of three is one the person may reasonably want anyway. A reason
// that was never recorded is on that side too — nothing refused it, so there
// is nothing to overrule.
func reasonAllowsOverride(stored string) bool {
	code, _ := splitReason(stored)
	return !hardReasons[code]
}

// pickAction is what the interface offers on a candidate row.
//
// The action follows the reason, and it is one decision rather than two: the
// button the page shows and the pick the endpoint accepts are the same
// question, and a rule that lives only in the markup is a rule with no
// teeth.
type pickAction struct {
	// Label is the button's text. Empty means there is no button at all.
	Label string
	// Secondary marks the softer of the two buttons: the release was
	// refused on quality, and the person may overrule that.
	Secondary bool
}

// candidatePick decides what a candidate row offers.
//
// Three cases, and the third is the one the interface used to get wrong:
//
//   - Nothing refused it, so the ordinary «Скачать это».
//   - It was refused on quality, so «Скачать всё равно» — dimmer, and it
//     says out loud that it is an overrule of the policy.
//   - It was refused on principle, so no button at all. Offering to
//     download a camrip or a blacklisted hash undoes the decision the
//     product exists to make.
func candidatePick(stored string) pickAction {
	code, _ := splitReason(stored)
	switch {
	case code == "":
		return pickAction{Label: "Скачать это"}
	case !reasonAllowsOverride(stored):
		return pickAction{}
	default:
		return pickAction{Label: "Скачать всё равно", Secondary: true}
	}
}

// canPick reports whether a candidate may be picked at all.
//
// The endpoint asks the same question the row asks, and gets the same
// answer: a release refused on principle is not downloadable through the
// interface, whatever the page shows.
func canPick(stored string) bool {
	return candidatePick(stored).Label != ""
}

// splitReason separates the machine code from the detail sentence.
//
// The stored form is "code: detail", and the codes are snake_case identifiers
// with no colon in them, so the first colon is the boundary.
func splitReason(stored string) (code, detail string) {
	code, detail, found := strings.Cut(stored, ": ")
	if !found {
		return strings.TrimSpace(stored), ""
	}
	return strings.TrimSpace(code), detail
}

// audioLabel is the Russian name of an audio tier.
//
// The tier is the difference somebody actually cares about. "Has Russian
// audio" is not the question — "is it a proper dub or an amateur reading" is,
// and that is the difference they notice within thirty seconds of pressing
// play.
func audioLabel(t release.AudioTier) string {
	switch t {
	case release.AudioSubsOnly:
		return "только субтитры"
	case release.AudioVoiceOver:
		return "закадровый"
	case release.AudioSingle:
		return "одноголосый"
	case release.AudioDual:
		return "двухголосый"
	case release.AudioMulti:
		return "многоголосый"
	case release.AudioDub:
		return "дубляж"
	default:
		return "не определён"
	}
}

// seeders is the number of seeders, as a person reads it.
//
// The indexer reports -1 when it does not know the count, which is not the
// same as zero: a release with no seeders is dead, and a release whose
// seeder count was never reported is merely unmeasured. Showing "-1" made
// the second look like the first, in the one column where the difference
// decides whether a download will move.
func seeders(n int) string {
	if n < 0 {
		return "—"
	}
	return strconv.Itoa(n)
}

// seedersUnknown reports whether the seeder count is missing rather than zero.
func seedersUnknown(n int) bool { return n < 0 }

// ---------------------------------------------------------------------------
// Scope
// ---------------------------------------------------------------------------

// The two views of the request list.
//
// "all" is every request in the installation — what the household shares.
// "mine" is the ones this person is watching. The administrator's scope is
// "all" and stays there: the requests nobody is watching are exactly the
// ones an operator has to notice.
const (
	scopeAll  = "all"
	scopeMine = "mine"
)

// scopeLabel is the Russian name of a scope.
//
// "Мои" is a deliberate word. A request has no owner: the person who asked
// for it is recorded in the event log, and anybody may attach themselves to
// one. The creator is added as a watcher on creation, so "мои" is a good
// proxy for "asked for by me" — but it is not the same thing, and the copy
// must not claim it is.
func scopeLabel(scope string) string {
	switch scope {
	case scopeMine:
		return "мои"
	default:
		return "все"
	}
}

// ---------------------------------------------------------------------------
// Russian plurals
// ---------------------------------------------------------------------------

// ruNoun is a Russian noun with its three plural forms.
//
// The accusative singular is kept separately because it differs from the
// nominative in one form only: минута becomes минуту after a verb, while
// минуты and минут are the same in both cases.
type ruNoun struct {
	nom [3]string
	acc string
}

// count renders n with the noun in the nominative: "5 минут".
func (n ruNoun) count(k int) string {
	return fmt.Sprintf("%d %s", k, n.nom[pluralForm(k)])
}

// countA renders n with the noun in the accusative: "5 минут", but
// "1 минуту".
//
// The accusative is what a duration takes after a verb or a preposition —
// "осталось 1 минуту", "1 минуту назад", "через 1 час".
func (n ruNoun) countA(k int) string {
	if pluralForm(k) == 0 {
		return fmt.Sprintf("%d %s", k, n.acc)
	}
	return fmt.Sprintf("%d %s", k, n.nom[pluralForm(k)])
}

var (
	nounSecond = ruNoun{nom: [3]string{"секунда", "секунды", "секунд"}, acc: "секунду"}
	nounMinute = ruNoun{nom: [3]string{"минута", "минуты", "минут"}, acc: "минуту"}
	nounHour   = ruNoun{nom: [3]string{"час", "часа", "часов"}, acc: "час"}
	nounDay    = ruNoun{nom: [3]string{"день", "дня", "дней"}, acc: "день"}
	nounMonth  = ruNoun{nom: [3]string{"месяц", "месяца", "месяцев"}, acc: "месяц"}
	nounYear   = ruNoun{nom: [3]string{"год", "года", "лет"}, acc: "год"}
)

// pluralForm picks which of the three Russian plural forms a count takes.
//
// Russian has three — 1 минута, 2 минуты, 5 минут — and the last two digits
// decide which, not just the last one: 21 takes the first form and 11 the
// third.
func pluralForm(n int) int {
	switch {
	case n%10 == 1 && n%100 != 11:
		return 0
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		return 1
	default:
		return 2
	}
}

// durationHuman renders a span of time in the fullest natural form.
//
// It is the age of something: "4 месяца 12 дней". Two units at most, because
// a person reads two and stops, and a unit that came out as zero is left out
// entirely — "1 год" rather than "1 год 0 месяцев".
//
// The month is thirty days and the year is 365. That is an approximation, and
// it is the right one here: the number is a glance, not a calendar.
func durationHuman(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	switch {
	case d < time.Minute:
		return "меньше минуты"
	case d < time.Hour:
		return nounMinute.count(int(d.Minutes()))
	case d < 48*time.Hour:
		return joinUnits(
			nounHour.count(int(d.Hours())),
			nounMinute.count(int(d.Minutes())%60))
	case d < 31*24*time.Hour:
		return joinUnits(
			nounDay.count(int(d.Hours()/24)),
			nounHour.count(int(d.Hours())%24))
	case d < 365*24*time.Hour:
		return joinUnits(
			nounMonth.count(int(d.Hours()/(24*30))),
			nounDay.count(int(d.Hours()/24)%30))
	default:
		return joinUnits(
			nounYear.count(int(d.Hours()/(24*365))),
			nounMonth.count(int(d.Hours()/(24*30))%12))
	}
}

// joinUnits puts two rendered units together, dropping an empty second one.
func joinUnits(first, second string) string {
	if second == "" || strings.HasPrefix(second, "0 ") {
		return first
	}
	return first + " " + second
}

// relativeTime renders a moment relative to now, in either direction.
//
// "через 6 ч" for a moment ahead, "6 ч назад" for one behind. The two
// directions are one function because a page that shows a schedule and a page
// that shows a history ask the same question of the same value.
func relativeTime(t time.Time) string {
	d := time.Until(t)
	if d < 0 {
		return since(t)
	}
	switch {
	case d < time.Minute:
		return "сейчас"
	case d < time.Hour:
		return "через " + nounMinute.countA(int(math.Ceil(d.Minutes())))
	case d < 48*time.Hour:
		return "через " + nounHour.countA(int(math.Ceil(d.Hours())))
	default:
		return "через " + nounDay.countA(int(math.Ceil(d.Hours()/24)))
	}
}
