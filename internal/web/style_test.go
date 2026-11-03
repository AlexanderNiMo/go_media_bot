package web

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// TestColoursLiveInTheTokenLayer checks that a literal colour appears only
// where a token is declared.
//
// A colour written into a rule is a colour that does not follow the theme: it
// is the same in the light interface and in the dark one, which is how the
// warning flash ended up as a fixed amber on a dark page. The token layer is
// the answer, and this test is what keeps it the answer.
//
// The rules themselves name tokens. Only a custom property's own value may be
// a literal.
func TestColoursLiveInTheTokenLayer(t *testing.T) {
	hexColour := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	// A custom property and its value, up to the next ; or the end of the
	// block. Removing these leaves whatever the rule itself says.
	customProperty := regexp.MustCompile(`--[a-z0-9-]+\s*:\s*[^;{}]*`)

	check := func(name, src string) {
		t.Helper()

		for i, line := range strings.Split(src, "\n") {
			outside := customProperty.ReplaceAllString(line, "")
			if m := hexColour.FindString(outside); m != "" {
				t.Errorf("%s:%d has a literal colour %s outside the token layer: %s",
					name, i+1, m, strings.TrimSpace(line))
			}
		}
	}

	css, err := viewsFS.ReadFile("views/static/style.css")
	if err != nil {
		t.Fatalf("read the stylesheet: %v", err)
	}
	check("style.css", string(css))

	// The templates carry no colours either: a colour in a template is a
	// colour that no stylesheet can change.
	err = fs.WalkDir(viewsFS, "views", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".html" {
			return nil
		}
		body, err := viewsFS.ReadFile(p)
		if err != nil {
			return err
		}
		check(path.Base(p), string(body))
		return nil
	})
	if err != nil {
		t.Fatalf("walk the views: %v", err)
	}
}
