package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSidebarKeyboardAndViewportChanges(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("sidebar behavior check needs Node.js; no npm packages required")
	}
	s := &Server{tmpl: mustTemplates(t), log: discardLogger()}
	rec := httptest.NewRecorder()
	s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), "dashboard", fixtures(t)["dashboard"])
	body := rec.Body.String()
	start := strings.LastIndex(body, "<script>")
	if start < 0 {
		t.Fatal("missing sidebar script")
	}
	script, _, ok := strings.Cut(body[start+len("<script>"):], "</script>")
	if !ok {
		t.Fatal("unclosed sidebar script")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", sidebarBehaviorCheck)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sidebar behavior: %v\n%s", err, out)
	}
}

// Execute the rendered script against a small DOM stub. Browser layout and
// native Tab traversal are checked separately; these assertions cover handlers.
const sidebarBehaviorCheck = `
const assert = require('node:assert/strict');
const attrs = {'data-sidebar': 'icons'};
const handlers = {};
let viewportChange;
const media = {matches: true, addEventListener(type, fn) {
  assert.equal(type, 'change'); viewportChange = fn;
}};
function item(extra = {}) {
  return Object.assign({tabIndex: 0, disabled: false,
    getClientRects() { return [1]; },
    focus() { document.activeElement = this; },
    setAttribute(name, value) { this[name] = value; }}, extra);
}
const first = item(), last = item();
const hidden = item({getClientRects() { return []; }});
const disabled = item({disabled: true}), excluded = item({tabIndex: -1});
const items = [hidden, first, disabled, excluded, last];
const hamburger = item();
const sidebar = {
  inert: false,
  setAttribute(name, value) { this[name] = value; },
  querySelectorAll() { return items; },
  contains(element) { return items.includes(element); }
};
global.document = {
  documentElement: {
    getAttribute(name) { return attrs[name]; },
    setAttribute(name, value) { attrs[name] = value; }
  },
  activeElement: hamburger,
  getElementById(id) { return {sidebar, hamburger}[id]; },
  addEventListener(type, fn) { handlers[type] = fn; }
};
global.window = {matchMedia() { return media; }};
global.localStorage = {setItem() {}};
require('node:vm').runInThisContext(require('node:fs').readFileSync(0, 'utf8'));
function key(key, shiftKey = false) {
  const e = {key, shiftKey, prevented: false, preventDefault() { this.prevented = true; }};
  handlers.keydown(e);
  return e;
}
assert.equal(sidebar.inert, true);
assert.equal(sidebar['aria-hidden'], 'true');
mediadToggleSidebar();
assert.equal(sidebar.inert, false);
assert.equal(hamburger['aria-expanded'], 'true');
assert.equal(document.activeElement, first);
assert.equal(key('Tab', true).prevented, true);
assert.equal(document.activeElement, last);
assert.equal(key('Tab').prevented, true);
assert.equal(document.activeElement, first);
assert.equal(key('Tab').prevented, false);
hamburger.focus();
assert.equal(key('Tab').prevented, true);
assert.equal(document.activeElement, first);
assert.equal(key('Escape').prevented, true);
assert.equal(attrs['data-sidebar'], 'icons');
assert.equal(hamburger['aria-expanded'], 'false');
assert.equal(document.activeElement, hamburger);
assert.equal(sidebar.inert, true);
// A closed mobile drawer becomes an accessible desktop column.
media.matches = false;
viewportChange();
assert.equal(sidebar.inert, false);
assert.equal(sidebar['aria-hidden'], 'false');
assert.equal(key('Tab').prevented, false);
// Returning to mobile must not strand focus in the now hidden column.
first.focus();
media.matches = true;
viewportChange();
assert.equal(sidebar.inert, true);
assert.equal(document.activeElement, hamburger);
mediadToggleSidebar();
mediadMaybeCloseSidebar();
assert.equal(document.activeElement, hamburger);
assert.equal(sidebar.inert, true);
mediadToggleSidebar();
mediadToggleSidebar();
assert.equal(document.activeElement, hamburger);
assert.equal(sidebar.inert, true);
`
