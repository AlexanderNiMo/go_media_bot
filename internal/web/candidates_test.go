package web

import (
	"strings"
	"testing"

	"github.com/alex/mediad/internal/release"
	"github.com/alex/mediad/internal/storage"
)

// The candidate row.
//
// The candidate table is where a person decides whether the bot understood
// them, so a row carries the facts of the release and not only the verdict
// on it. These tests check that the facts are read out of the release's name
// — the same names the selector read them out of — and that the reason says
// why this release was refused, in the reader's language.

// TestTheCandidateRowReadsTheReleaseOutOfItsName checks what a row knows
// about a release.
//
// The facts come from the title and the indexer, which are the two things a
// candidate row stores. Nothing else is needed, and nothing else is stored:
// a second copy of the parse in the database would be a second copy of the
// rules, and the two copies would drift.
func TestTheCandidateRowReadsTheReleaseOutOfItsName(t *testing.T) {
	row := candidateRowFor(storage.Candidate{
		Rank:      1,
		Title:     "Матрица 1999 1080p BluRay | LostFilm",
		Indexer:   "rutracker",
		SizeBytes: 8 << 30,
		Seeders:   12,
	}, release.DefaultProfile())

	if row.QualityText != "1080p · BluRay" {
		t.Errorf("the quality reads %q, want %q", row.QualityText, "1080p · BluRay")
	}
	if row.AudioLabel != "многоголосый" {
		t.Errorf("the audio reads %q, want %q", row.AudioLabel, "многоголосый")
	}
	if row.AudioTone != "ok" {
		t.Errorf("the audio tone is %q, want %q", row.AudioTone, "ok")
	}
	if row.Studio != "Lostfilm" {
		t.Errorf("the studio reads %q, want %q", row.Studio, "Lostfilm")
	}
	if !strings.Contains(row.AudioTitle, "lostfilm") {
		t.Errorf("the audio title reads %q; it has to say what decided the tier",
			row.AudioTitle)
	}
	if row.Size != "8,0 ГиБ" {
		t.Errorf("the size reads %q, want %q", row.Size, "8,0 ГиБ")
	}
	if row.Seeders != "12" {
		t.Errorf("the seeders read %q, want %q", row.Seeders, "12")
	}
}

// TestTheCandidateRowSaysWhatItCouldNotRead checks the cells that have
// nothing to say.
//
// A name with no resolution in it gets a dash and not a guess: the cell
// reports what could not be read, and the reason cell says why the release
// was refused for it.
func TestTheCandidateRowSaysWhatItCouldNotRead(t *testing.T) {
	row := candidateRowFor(storage.Candidate{
		Rank:    1,
		Title:   "Матрица 1999",
		Indexer: "rutracker",
		// -1 is what the indexer reports when it does not know the
		// count, and it is not the same thing as zero.
		Seeders: -1,
	}, release.DefaultProfile())

	if row.QualityText != "—" {
		t.Errorf("the quality reads %q, want a dash", row.QualityText)
	}
	if row.QualityTitle == "" {
		t.Error("the quality cell says nothing about why it is empty")
	}
	if row.Seeders != "—" {
		t.Errorf("the seeders read %q, want a dash; the count was never reported",
			row.Seeders)
	}
	if !row.SeedersUnknown {
		t.Error("the row does not know that the seeders are unknown")
	}
}

// TestTheCandidateRowCountsTheEpisodesOutOfTheName checks the season pack.
//
// The count comes from the name and never from the torrent's file list: a
// pack of eight episodes and a single episode are told apart by what the
// release calls itself.
func TestTheCandidateRowCountsTheEpisodesOutOfTheName(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  string
	}{
		{"Матрица S02E01-E08 из 10 1080p", "8 эпизодов из 10"},
		{"Матрица S02E01-E08 1080p", "8 эпизодов"},
		{"Матрица S02E05 1080p", ""},
		{"Матрица 1080p", ""},
	} {
		row := candidateRowFor(storage.Candidate{
			Rank: 1, Title: tc.title, Indexer: "rutracker",
		}, release.DefaultProfile())

		if row.Episodes != tc.want {
			t.Errorf("%q reads %q episodes, want %q", tc.title, row.Episodes, tc.want)
		}
	}
}

// TestTheAudioTitleSaysWhatDecidedTheTier checks the tooltip of the audio
// cell.
//
// It has to name the evidence: the studio, the marker, and — for a release
// that says English outright — that. A Russian tracker is weak evidence of
// Russian audio, and it is no evidence at all when the name says English.
func TestTheAudioTitleSaysWhatDecidedTheTier(t *testing.T) {
	english := candidateRowFor(storage.Candidate{
		Rank: 1, Title: "Матрица 1999 1080p CAMRip | ENG", Indexer: "rutracker",
	}, release.DefaultProfile())

	if !strings.Contains(english.AudioTitle, "английское") {
		t.Errorf("the audio title reads %q; a release that says English says so",
			english.AudioTitle)
	}

	marked := candidateRowFor(storage.Candidate{
		Rank: 1, Title: "Матрица 1999 1080p BluRay | LostFilm", Indexer: "rutracker",
	}, release.DefaultProfile())

	if !strings.Contains(marked.AudioTitle, "lostfilm") {
		t.Errorf("the audio title reads %q; it names the word that decided the tier",
			marked.AudioTitle)
	}
}

// TestACandidateWithNoReasonSaysSo checks the reason cell of a release
// nothing refused.
//
// A candidate is a refusal by definition, but a cell with nothing in it is a
// cell that says nothing.
func TestACandidateWithNoReasonSaysSo(t *testing.T) {
	row := candidateRowFor(storage.Candidate{
		Rank: 1, Title: "Матрица 1080p", Indexer: "rutracker",
	}, release.DefaultProfile())

	if row.Reason != "—" {
		t.Errorf("the reason reads %q, want a dash", row.Reason)
	}
}

// TestTheReasonCarriesTheReleasesOwnNumbers checks what the reason cell
// says.
//
// The numbers in it belong to the release: "2 сидера" is what this release
// has. The threshold it missed belongs to the policy, and the policy is one
// screen above, in «Требования».
//
// A threshold stored with the candidate would be a second copy of the rule,
// and it would go stale the moment the rule changed — a release refused under
// "three seeders" would go on claiming it was refused under "five".
func TestTheReasonCarriesTheReleasesOwnNumbers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored string
		cand   storage.Candidate
		title  string
		want   string
	}{
		{
			name:   "too few seeders",
			stored: "too_few_seeders: 2 seeders, minimum is 3",
			cand:   storage.Candidate{Seeders: 2},
			want:   "мало сидеров: 2",
		},
		{
			name:   "below the resolution floor",
			stored: "resolution_too_low: 720p is below the minimum 1080p",
			cand:   storage.Candidate{},
			title:  "Матрица 720p",
			want:   "ниже минимального разрешения: 720p",
		},
		{
			name:   "an excluded source",
			stored: "excluded_source: source CAM is excluded",
			cand:   storage.Candidate{},
			title:  "Матрица 1080p CAM",
			want:   "источник исключён: CAM",
		},
		{
			name:   "too small",
			stored: "too_small: 0.40 GB is below the minimum 1.50 GB",
			cand:   storage.Candidate{SizeBytes: 1 << 30},
			want:   "слишком маленький: 1,0 ГиБ",
		},
		{
			name:   "the wrong year",
			stored: "wrong_year: release is from 1999, request is from 2021",
			cand:   storage.Candidate{},
			title:  "Матрица 1999 1080p",
			want:   "другой год: 1999",
		},
		{
			name:   "a refusal with no number in it",
			stored: "blacklisted: this exact release was tried before",
			cand:   storage.Candidate{},
			want:   "в чёрном списке",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed := release.Parse(tc.title)
			got := reasonText(tc.stored, tc.cand, parsed)
			if got != tc.want {
				t.Errorf("the reason reads %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheReasonDoesNotRepeatThePolicy checks that the threshold stays out of
// the reason cell.
//
// The threshold is on the page already — «Требования» says what the request
// is looking for — and a second copy of it in the cell would be a second
// copy of the rule, free to disagree with the first.
func TestTheReasonDoesNotRepeatThePolicy(t *testing.T) {
	stored := "too_few_seeders: 2 seeders, minimum is 3"
	got := reasonText(stored, storage.Candidate{Seeders: 2}, release.Parsed{})

	if strings.Contains(got, "минимум") || strings.Contains(got, "3") {
		t.Errorf("the reason reads %q; the threshold belongs to «Требования»", got)
	}
}

// TestTheReasonFallsBackToTheLabel checks a reason with nothing to count.
//
// The label is the whole of it: "в чёрном списке" has no number in it, and
// inventing one would be worse than leaving it out.
func TestTheReasonFallsBackToTheLabel(t *testing.T) {
	got := reasonText("no_russian_audio: the name mentions no Russian marker at all",
		storage.Candidate{}, release.Parsed{})

	if got != "нет русской дорожки" {
		t.Errorf("the reason reads %q, want %q", got, "нет русской дорожки")
	}
}
