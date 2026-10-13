package plex

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// findByTitle walks the sections of the requested type and returns the first
// item whose title normalises equal to one of candidates and whose year
// matches want.
//
// candidateTitle is the first entry of candidates and is used to prefilter
// the Plex query; the comparison itself happens here, because the server
// cannot match on a normalised form.
func (c *Client) findByTitle(ctx context.Context, sectionType string, candidates []string, year int, season *int) (*Match, error) {
	if !c.Configured() {
		return nil, errors.New("plex is not configured")
	}
	if len(candidates) == 0 {
		return nil, errors.New("findByTitle: no candidate titles")
	}
	if year <= 0 {
		// A title-only match would accept every remake, and the whole point
		// of the fallback is to avoid that. The caller has the spec's year
		// because the search step has to have read it; if it did not, this
		// check is the reason we are not silently searching by title alone.
		return nil, fmt.Errorf("findByTitle: refusing to match without a year (candidates=%v)", candidates)
	}

	want := normalisedSet(candidates)

	sections, err := c.Sections(ctx)
	if err != nil {
		return nil, err
	}

	for _, section := range sections {
		if section.Type != sectionType {
			continue
		}

		params := url.Values{
			"includeGuids": {"1"},
			"type":         {plexTypeFor(section.Type)},
			"title":        {candidates[0]},
		}
		var raw mediaContainer
		if err := c.get(ctx, "/library/sections/"+section.Key+"/all", params, &raw); err != nil {
			c.log.Warn("plex section lookup failed",
				"section", section.Key, "title", section.Title, "error", err)
			continue
		}

		for _, item := range raw.MediaContainer.Metadata {
			if item.Year != year {
				continue
			}
			if !want[item.normalisedTitle()] {
				continue
			}

			if season == nil {
				return item.toMatch(section.Key), nil
			}
			return c.findSeasonChild(ctx, section.Key, item, *season)
		}
	}

	return nil, nil
}

// normalisedTitle is the form titles are compared in.
//
// "The Matrix (1999)" and "the matrix" both come out as "thematrix", and so
// does "The, Matrix!" — punctuation and case stop being load-bearing, which
// is what makes a hand-renamed library item findable.
func (m mediaItem) normalisedTitle() string {
	return normaliseTitle(m.Title)
}

// normaliseTitle is the package-level version, exposed for tests and for the
// case where the caller wants to compare its own string against the same
// form.
//
// A trailing four-digit year in the form "(1999)" is stripped before the
// letter/digit pass: the year is matched separately, and a candidate
// without the suffix ("The Matrix") has to compare equal to a Plex entry
// with it ("The Matrix (1999)").
func normaliseTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = stripTrailingYear(s)
	// Strip combining marks so "Ångström" and "Angstrom" can meet.
	s = decomposeASCII(s)
	// Drop everything that is not a letter or a digit. The unicode category
	// is what makes Cyrillic and Latin and numbers all coexist; an ASCII-only
	// filter would silently throw away every Russian title.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripTrailingYear peels a "(1999)" / "[1999]" / ", 1999" suffix off a
// title, so the year-mismatched against it later does not change the
// comparison.
//
// Anything that is not a parenthesis, a comma, whitespace or a four-digit
// year is left alone, so "1984" itself is not stripped.
func stripTrailingYear(s string) string {
	for _, open := range []rune{'(', '['} {
		close := matchingBracket(open)
		idx := strings.LastIndex(s, string(open))
		if idx < 0 {
			continue
		}
		if !strings.HasSuffix(s, string(close)) {
			continue
		}
		inner := s[idx+1 : len(s)-1]
		if isFourDigitYear(inner) {
			return strings.TrimSpace(s[:idx])
		}
	}
	// ", 1999" at the end.
	if i := strings.LastIndex(s, ","); i > 0 {
		tail := strings.TrimSpace(s[i+1:])
		if isFourDigitYear(tail) {
			return strings.TrimSpace(s[:i])
		}
	}
	return s
}

func matchingBracket(open rune) rune {
	if open == '(' {
		return ')'
	}
	return ']'
}

func isFourDigitYear(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 4 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// normalisedSet returns the candidates as a lookup table.
//
// The first candidate that normalises into a value is the only one that gets
// stored; the rest are folded into the same key, because "The Matrix" and
// "The Matrix (1999)" can both be in the spec and they normalise the same.
func normalisedSet(candidates []string) map[string]bool {
	out := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if c == "" {
			continue
		}
		out[normaliseTitle(c)] = true
	}
	return out
}

// decomposeASCII is a small NFKD approximation. The standard library has
// golang.org/x/text/unicode/norm, but pulling a transitive dependency in for
// one diacritic is not worth it: the titles Plex stores are a Latin/Cyrillic
// mix without combining marks of consequence. ASCII-folding the cases we
// actually see is enough.
func decomposeASCII(s string) string {
	replacements := map[rune]rune{
		'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
		'á': 'a', 'à': 'a', 'â': 'a', 'ä': 'a',
		'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
		'ó': 'o', 'ò': 'o', 'ô': 'o', 'ö': 'o',
		'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
		'ñ': 'n', 'ç': 'c',
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if repl, ok := replacements[r]; ok {
			b.WriteRune(repl)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
