package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The accessibility guard.
//
// These are the defects a screen reader notices and a sighted person does
// not: a control with no name, an image that announces its filename, a table
// of data with no headers, a navigation hidden from assistive technology
// while visible on screen.
//
// They are checked against the rendered pages rather than the templates,
// because the rendered page is what the browser parses — and the parser is
// where a form inside a form disappears, which is the mistake the test
// beside this one already learned to look for.
//
// The rules are structural. They catch the part of accessibility that can be
// caught without a screen reader, and they run in `make test`, which is the
// only reason they keep holding: nobody re-reads a page's markup for the
// sake of it.

func TestRenderedPagesAreAccessible(t *testing.T) {
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}

	for name, data := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), name, data)

			doc, err := html.Parse(strings.NewReader(rec.Body.String()))
			if err != nil {
				t.Fatalf("parse the rendered page: %v", err)
			}

			targets := labelTargets(doc)

			checkImagesHaveAlt(t, doc)
			checkControlsHaveNames(t, doc)
			checkInputsHaveLabels(t, doc, targets)
			checkTablesHaveHeaders(t, doc)
			checkNothingFocusableIsHidden(t, doc)
			checkProgressBarsHaveValues(t, doc)
		})
	}
}

// checkImagesHaveAlt checks that every image says what it is.
//
// An empty alt is a deliberate statement — "this image is decorative" — and
// it is how the poster thumbnails are marked up, because the title beside
// them already carries the name. A missing alt is not a statement at all.
func checkImagesHaveAlt(t *testing.T, doc *html.Node) {
	t.Helper()

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			if _, ok := attrOf(n, "alt"); !ok {
				t.Errorf("an <img> has no alt attribute; a screen reader announces " +
					"the filename instead")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

// checkControlsHaveNames checks that every link and button is nameable.
//
// A link with an icon and no text is invisible to a screen reader unless it
// carries a label. The sidebar's icons are the case this was written for.
func checkControlsHaveNames(t *testing.T, doc *html.Node) {
	t.Helper()

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "a", "button":
				if !hasAccessibleName(n) {
					t.Errorf("a <%s> has no accessible name; it is announced as nothing", n.Data)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

// checkInputsHaveLabels checks that every field says what it is for.
//
// A placeholder is not a label: it disappears the moment somebody types, and
// it is not reliably announced. A field is labelled by a wrapping <label>,
// by a <label for> pointing at it, or by an aria-label of its own.
func checkInputsHaveLabels(t *testing.T, doc *html.Node, targets map[string]bool) {
	t.Helper()

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "input" {
			switch strings.ToLower(attrForTest(n, "type")) {
			case "hidden":
				// A hidden field carries no value a person sees.
			default:
				if !hasLabel(n, targets) {
					t.Errorf("an <input type=%q> has no label; a screen reader "+
						"announces the field name, which is nothing",
						attrForTest(n, "type"))
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

// checkTablesHaveHeaders checks that a table of data says what its columns
// are.
//
// A table without headers is read as a stream of unrelated cells, which is
// the case where the column that matters — the state, the progress — cannot
// be told apart from the one that does not.
func checkTablesHaveHeaders(t *testing.T, doc *html.Node) {
	t.Helper()

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "table" {
			if !hasHeader(n) {
				t.Error("a <table> has no <th> and no <caption>; its columns are " +
					"unlabelled")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

// checkNothingFocusableIsHidden checks that nothing reachable is inside an
// aria-hidden container.
//
// aria-hidden hides from assistive technology but not from the keyboard, so a
// focusable element inside one is focusable and invisible: the keyboard
// walks into it and a screen reader never says it is there.
//
// The sidebar used to be exactly this. Its aria-hidden was hard-coded to
// "true" in the markup, which on a desktop hid the whole navigation from
// screen readers and took it out of the tab order at the same time.
func checkNothingFocusableIsHidden(t *testing.T, doc *html.Node) {
	t.Helper()

	var walk func(*html.Node, bool)
	walk = func(n *html.Node, hidden bool) {
		if n.Type == html.ElementNode {
			if attrForTest(n, "aria-hidden") == "true" {
				hidden = true
			}
			if hidden && isFocusable(n) {
				t.Errorf("a focusable <%s> is inside an aria-hidden container; "+
					"the keyboard reaches it and a screen reader does not", n.Data)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, hidden)
		}
	}
	walk(doc, false)
}

// checkProgressBarsHaveValues checks that a progress bar can be read.
//
// A progress bar with no value is announced as "progress bar" and nothing
// else, which is worse than no bar: it tells the listener that something is
// happening without saying how much.
func checkProgressBarsHaveValues(t *testing.T, doc *html.Node) {
	t.Helper()

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && attrForTest(n, "role") == "progressbar" {
			if attrForTest(n, "aria-valuenow") == "" {
				t.Error("a progress bar has no aria-valuenow; it is announced " +
					"without a number")
			}
			if attrForTest(n, "aria-label") == "" && attrForTest(n, "aria-labelledby") == "" {
				t.Error("a progress bar has no accessible name")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// attrOf returns an attribute and whether it was there at all.
//
// The difference matters for alt: an empty alt is a statement, a missing one
// is not.
func attrOf(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// labelTargets collects the ids that a <label for> points at.
func labelTargets(doc *html.Node) map[string]bool {
	targets := map[string]bool{}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "label" {
			if id := attrForTest(n, "for"); id != "" {
				targets[id] = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return targets
}

// hasAccessibleName reports whether a control announces something.
func hasAccessibleName(n *html.Node) bool {
	if attrForTest(n, "aria-label") != "" {
		return true
	}
	if attrForTest(n, "title") != "" {
		return true
	}
	return strings.TrimSpace(textOf(n)) != ""
}

// hasLabel reports whether a field is labelled.
func hasLabel(n *html.Node, targets map[string]bool) bool {
	if attrForTest(n, "aria-label") != "" || attrForTest(n, "aria-labelledby") != "" {
		return true
	}
	if id := attrForTest(n, "id"); id != "" && targets[id] {
		return true
	}
	// A field inside a <label> is labelled by it, whatever the field is.
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "label" {
			return true
		}
	}
	// A button with a value carries its own name.
	if v := attrForTest(n, "value"); v != "" {
		switch strings.ToLower(attrForTest(n, "type")) {
		case "submit", "button", "reset":
			return true
		}
	}
	return false
}

// hasHeader reports whether a table has headers.
func hasHeader(table *html.Node) bool {
	found := false

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "th":
				found = true
			case "caption":
				found = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(table)

	return found
}

// isFocusable reports whether the keyboard can reach an element.
func isFocusable(n *html.Node) bool {
	switch n.Data {
	case "a":
		return attrForTest(n, "href") != ""
	case "button", "select", "textarea":
		return true
	case "input":
		return !strings.EqualFold(attrForTest(n, "type"), "hidden")
	}
	if v := attrForTest(n, "tabindex"); v != "" {
		return !strings.HasPrefix(v, "-")
	}
	return false
}
