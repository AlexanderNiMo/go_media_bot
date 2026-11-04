package web

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alex/mediad/internal/release"
	"github.com/alex/mediad/internal/storage"
)

// The candidate table: the releases a person chooses from when the selector
// refused all of them.
//
// It is the one place in the product where a person decides whether the bot
// understood them, so a row shows the facts of the release and not only the
// verdict on it. "Нет русской дорожки" is an answer only if the person can
// see what the release actually has.
//
// The facts are read out of the release's name at render time, with the same
// functions the selector used to reach its verdict. Nothing is stored twice:
// a row holds a title and an indexer, and those two are all the detector
// needs. A copy of the parse kept in the database would be a second copy of
// the rules, and the two copies would drift.

// candidateRow is one candidate as the table shows it.
type candidateRow struct {
	Rank     int
	Title    string
	Indexer  string
	InfoHash string

	// QualityText is the resolution and the source in one cell: "1080p ·
	// BluRay". They are two facts, and a person weighing two 1080p
	// releases against each other wants both.
	QualityText string
	// QualityTitle explains the cell when the name said too little to
	// read a resolution out of it.
	QualityTitle string

	// AudioLabel is the tier of the release's Russian audio, AudioTone is
	// how good that tier is, and AudioTitle says what decided it.
	AudioLabel string
	AudioTone  string
	AudioTitle string
	// Studio is the dubbing house the release is credited to.
	Studio string

	// Episodes is how many episodes the release's name claims to carry.
	// Empty when the name says nothing about them.
	Episodes string

	Size string

	Seeders        string
	SeedersUnknown bool

	// Reason is why the release was refused, in Russian, with the
	// release's own numbers.
	Reason string
	// ReasonTone is the severity of the reason, and ReasonTitle is the
	// machine's own record of it: the code, and the sentence the verdict
	// was reached with.
	ReasonTone  string
	ReasonTitle string

	// Pick is the action the row offers. A refused-on-principle row has
	// no action at all, and Refused says so.
	Pick    pickAction
	Refused bool
}

// candidateRows turns the stored candidates into the rows the table shows.
func candidateRows(candidates []storage.Candidate, p release.Profile) []candidateRow {
	rows := make([]candidateRow, 0, len(candidates))
	for _, c := range candidates {
		rows = append(rows, candidateRowFor(c, p))
	}
	return rows
}

// candidateRowFor turns one stored candidate into one row.
//
// The profile is the policy the request is searched under. It decides what
// counts as Russian audio, and the detector needs it for that — the marker
// tables live in the profile, not in the release package's globals.
func candidateRowFor(c storage.Candidate, p release.Profile) candidateRow {
	parsed := release.Parse(c.Title)
	audio := release.DetectAudio(c.Title, c.Indexer, p)

	row := candidateRow{
		Rank:           c.Rank,
		Title:          c.Title,
		Indexer:        c.Indexer,
		InfoHash:       c.InfoHash,
		QualityText:    qualityText(parsed),
		AudioLabel:     audioLabel(audio.Tier),
		AudioTone:      audioTone(audio.Tier),
		AudioTitle:     audioTitle(audio),
		Episodes:       episodeLabel(parsed.Episodes),
		Size:           humanBytes(c.SizeBytes),
		Seeders:        seeders(c.Seeders),
		SeedersUnknown: seedersUnknown(c.Seeders),
		Reason:         reasonText(c.RejectionReason, c, parsed),
		ReasonTone:     reasonTone(c.RejectionReason),
		ReasonTitle:    c.RejectionReason,
		Pick:           candidatePick(c.RejectionReason),
		Refused:        !canPick(c.RejectionReason),
	}

	// Nothing refused it. A candidate is a refusal by definition, but a
	// row that says so with a dash is better than a cell with nothing in
	// it.
	if c.RejectionReason == "" {
		row.Reason = "—"
	}

	if parsed.Quality == release.QualityUnknown {
		row.QualityTitle = "в названии нет разрешения"
	}
	if len(audio.Studios) > 0 {
		row.Studio = studioLabel(audio.Studios[0])
	}

	return row
}

// qualityText is the resolution and the source of a release, in one cell.
//
// "1080p · BluRay". A name that says neither gives a dash rather than a
// guess: the cell reports what could not be read, and the reason cell says
// why the release was refused for it.
func qualityText(parsed release.Parsed) string {
	quality := ""
	if parsed.Quality != release.QualityUnknown {
		quality = parsed.Quality.String()
	}
	source := ""
	if parsed.Source != release.SourceUnknown {
		source = parsed.Source.String()
	}

	switch {
	case quality == "" && source == "":
		return "—"
	case quality == "":
		return source
	case source == "":
		return quality
	}
	return quality + " · " + source
}

// audioTitle says what decided the audio tier.
//
// The marker is the word in the name that settled it — "lostfilm", "мво",
// "двухголос" — and it is what a person asking "why is this one refused"
// wants to see. The studio is in the same line: a release credited to a
// studio has that studio's audio, whatever else the name says or omits.
func audioTitle(audio release.AudioTrack) string {
	var parts []string
	if len(audio.Studios) > 0 {
		studios := make([]string, 0, len(audio.Studios))
		for _, s := range audio.Studios {
			studios = append(studios, studioLabel(s))
		}
		parts = append(parts, "студия: "+strings.Join(studios, ", "))
	}
	if audio.Marker != "" {
		parts = append(parts, "решено по слову «"+audio.Marker+"»")
	}
	switch {
	case audio.IndicatesEnglish:
		// The name says English. The tracker's nationality does not help
		// here — this is exactly the release the audio detection exists
		// to keep away from the library.
		parts = append(parts, "в названии указано английское аудио")
	case audio.ImpliedByTracker:
		parts = append(parts, "в названии нет метки, трекер русскоязычный")
	}
	return strings.Join(parts, "; ")
}

// studioLabel is the dubbing house a release is credited to.
//
// The marker tables are keyed in lower case — "lostfilm" — because that is
// how the names are matched against them. A page that says "lostfilm" is
// showing the matcher, not the studio.
func studioLabel(studio string) string {
	first, size := utf8.DecodeRuneInString(studio)
	if first == utf8.RuneError {
		return studio
	}
	return string(unicode.ToUpper(first)) + studio[size:]
}
