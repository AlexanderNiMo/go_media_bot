package indexer

import (
	"regexp"

	"github.com/alex/mediad/internal/config"
)

// compiledPattern is one indexer's regex, ready to run against a torrent
// description.
type compiledPattern struct {
	indexer string
	re      *regexp.Regexp
}

// compileThreadPatterns turns the config list into a slice the indexer
// can iterate. A pattern that does not compile is dropped with no
// signal: a misconfigured pattern is the user's problem, and a
// panic on every search is a worse failure than silently skipping
// the tracker.
//
// The captured group must be a numeric id. Non-numeric captures
// (the whole URL, a path component) are deliberately not
// supported: the workflow uses the id to follow a thread across
// releases, and "viewtopic.php?t=12345" as a thread id is a string
// the storage layer would have to escape.
func compileThreadPatterns(patterns []config.ThreadPattern) []compiledPattern {
	out := make([]compiledPattern, 0, len(patterns))
	for _, p := range patterns {
		if p.Indexer == "" || p.Pattern == "" {
			continue
		}
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			continue
		}
		out = append(out, compiledPattern{indexer: p.Indexer, re: re})
	}
	return out
}

// defaultThreadFallback is the regex used when no per-tracker pattern
// matches. Captures the trailing numeric id of a phpBB-style
// "viewtopic.php?t=12345" or "viewtopic.php?id=12345" link, which
// covers RuTracker, Rutor via phpBB, and most of the trackers this
// project actually targets.
//
// A non-numeric capture is not a fallback failure: a "viewtopic.php"
// link with no id is rare enough to skip, and a tracker that uses
// some other shape can ship its own pattern in config.
var defaultThreadFallback = func() *regexp.Regexp {
	return regexp.MustCompile(`viewtopic\.(?:php)?\?(?:t|id)=(\d+)`)
}

// extractThreadID pulls the forum thread id out of a torrent's
// description.
//
// Per-tracker patterns win when the description matches and the
// captured group is numeric. The fallback covers the rest. An empty
// description, a description with no link, or a link that does not
// match any pattern, all return "". The caller treats an empty
// ThreadID as "this release has no thread to track", which is the
// safe default: the workflow keeps the release but does not try to
// follow it across releases.
func extractThreadID(description string, patterns []compiledPattern, fallback *regexp.Regexp) (string, string) {
	if description == "" {
		return description, ""
	}
	for _, p := range patterns {
		m := p.re.FindStringSubmatch(description)
		if len(m) < 2 {
			continue
		}
		if id := sanitizeThreadID(m[1]); id != "" {
			return description, p.indexer + ":" + id
		}
	}
	if fallback != nil {
		m := fallback.FindStringSubmatch(description)
		if len(m) >= 2 {
			if id := sanitizeThreadID(m[1]); id != "" {
				return description, id
			}
		}
	}
	return description, ""
}

// sanitizeThreadID returns the id as-is if it is all digits, "" otherwise.
// A non-numeric id would be a misconfigured pattern; reporting an empty
// string is a safe failure mode.
func sanitizeThreadID(s string) string {
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return s
}
