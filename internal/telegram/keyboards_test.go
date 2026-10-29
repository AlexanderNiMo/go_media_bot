package telegram

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alex/mediad/internal/storage"
)

// The keyboard for a candidate list is built here rather than in the
// bot, so it can be checked without a database and without a chat.

// TestCandidatesKeyboard — one button per candidate, in rank order, each
// carrying the request id and the rank and nothing else.
func TestCandidatesKeyboard(t *testing.T) {
	const requestID = 42

	candidates := []storage.Candidate{
		{Rank: 1, Title: "Матрица 1080p", Seeders: 12, SizeBytes: 4 << 30},
		{Rank: 2, Title: "Матрица 720p", Seeders: 3, SizeBytes: 2 << 30},
	}

	markup := candidatesKeyboard(requestID, candidates)

	if len(markup.InlineKeyboard) != len(candidates) {
		t.Fatalf("%d buttons, want %d", len(markup.InlineKeyboard), len(candidates))
	}

	for i, c := range candidates {
		row := markup.InlineKeyboard[i]
		if len(row) != 1 {
			t.Fatalf("button %d sits in a row of %d, want one button per row", i, len(row))
		}
		button := row[0]

		want := fmt.Sprintf("%s%d:%d", candidatePrefix, requestID, c.Rank)
		if button.CallbackData != want {
			t.Errorf("button %d carries %q, want %q", i, button.CallbackData, want)
		}

		// Telegram caps callback_data at 64 bytes, and the title is in
		// the label where a human reads it.
		if len(button.CallbackData) > 64 {
			t.Errorf("button %d carries %d bytes of callback data, over the limit: %q",
				i, len(button.CallbackData), button.CallbackData)
		}

		if !strings.Contains(button.Text, c.Title) {
			t.Errorf("button %d reads %q, want the title %q in it", i, button.Text, c.Title)
		}
	}
}

// TestCandidateButtonLabel — what the button says. The rank and the title
// identify the release; the seeders and the size say whether it is worth
// taking. A field the release does not have is not mentioned at all.
func TestCandidateButtonLabel(t *testing.T) {
	for _, tc := range []struct {
		descr     string
		candidate storage.Candidate
		want      []string
		notWant   []string
	}{
		{
			"a release with seeders and a size",
			storage.Candidate{Rank: 1, Title: "Матрица 1080p", Seeders: 12, SizeBytes: 4 << 30},
			[]string{"1.", "Матрица 1080p", "12 сидеров", "4.0 ГБ"},
			nil,
		},
		{
			"a release with no seeders says nothing about seeders",
			storage.Candidate{Rank: 2, Title: "Матрица 720p", SizeBytes: 2 << 30},
			[]string{"2.", "Матрица 720p", "2.0 ГБ"},
			[]string{"сидеров"},
		},
		{
			"a release with no size says nothing about size",
			storage.Candidate{Rank: 3, Title: "Матрица", Seeders: 5},
			[]string{"3.", "Матрица", "5 сидеров"},
			[]string{"ГБ"},
		},
	} {
		t.Run(tc.descr, func(t *testing.T) {
			markup := candidatesKeyboard(42, []storage.Candidate{tc.candidate})
			label := markup.InlineKeyboard[0][0].Text

			for _, want := range tc.want {
				if !strings.Contains(label, want) {
					t.Errorf("the label %q does not mention %q", label, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(label, notWant) {
					t.Errorf("the label %q mentions %q, which it should not", label, notWant)
				}
			}
		})
	}
}

// TestCandidateButtonLabelIsTruncated — a long title is cut, and cut on a
// rune boundary: the titles here are Russian, and cutting by bytes would
// leave half a character on the button.
func TestCandidateButtonLabelIsTruncated(t *testing.T) {
	markup := candidatesKeyboard(42, []storage.Candidate{{
		Rank:  1,
		Title: strings.Repeat("я", 200),
	}})
	label := markup.InlineKeyboard[0][0].Text

	if n := len([]rune(label)); n > 60 {
		t.Errorf("the label is %d runes, want at most 60", n)
	}
	if !strings.HasSuffix(label, "…") {
		t.Errorf("the label %q does not end with an ellipsis", label)
	}
}

// TestCandidatesKeyboardOfNothing — a request with no candidates gets no
// buttons, rather than a keyboard with an empty row in it.
func TestCandidatesKeyboardOfNothing(t *testing.T) {
	markup := candidatesKeyboard(42, nil)

	if len(markup.InlineKeyboard) != 0 {
		t.Errorf("%d buttons, want none", len(markup.InlineKeyboard))
	}
}
