// Package torrentfile reads a .torrent file and turns it into what the
// download client needs: the info hash that identifies the torrent, the
// trackers it announces to, and a magnet URI carrying both.
//
// The magnet is the point of the package. A tracker link cannot be fetched
// without the tracker's session, so the file a user hands over is the thing
// that can be turned into a link the client can use on its own: the announce
// URLs inside the file are the tracker's own, and on a private tracker they
// are what lets the client find a single peer.
//
// The magnet is built here rather than the file being handed to the client
// because the client's file upload needs a version this project cannot
// assume: only qBittorrent 5.0 and later accept a torrent file, and a
// magnet works on every version.
package torrentfile

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// File is what a .torrent file says about the torrent it carries.
type File struct {
	// InfoHash is the SHA-1 of the info dictionary, hex encoded. It is the
	// torrent's identity: the same release under two names has one hash.
	InfoHash string
	// Name is the display name the torrent carries.
	Name string
	// Trackers are the announce URLs, in the order the file lists them.
	Trackers []string
	// Size is the total size of the content in bytes. Zero when the file
	// does not say.
	Size int64
}

// Parse reads a .torrent file.
func Parse(data []byte) (*File, error) {
	p := &parser{buf: data}
	value, _, err := p.parseValue()
	if err != nil {
		return nil, fmt.Errorf("read the torrent file: %w", err)
	}

	top, ok := asDict(value)
	if !ok {
		return nil, errors.New("the file is not a torrent dictionary")
	}
	info, ok := top["info"]
	if !ok {
		return nil, errors.New("the torrent has no info dictionary")
	}
	infoDict, ok := asDict(info.value)
	if !ok {
		return nil, errors.New("the torrent's info is not a dictionary")
	}

	// A torrent says what it carries: one file with a length, or a list of
	// them. A dictionary with neither is not a torrent, and hashing it would
	// produce a magnet that the client rejects much later, with a much worse
	// message than this one.
	if !infoNamesContent(infoDict) {
		return nil, errors.New("the torrent's info dictionary names no files")
	}

	sum := sha1.Sum(info.raw)
	f := &File{InfoHash: hex.EncodeToString(sum[:])}

	if name, ok := infoDict["name"]; ok {
		if b, ok := asBytes(name.value); ok {
			f.Name = string(b)
		}
	}
	f.Size = infoSize(infoDict)

	f.Trackers = trackers(top)
	return f, nil
}

// infoNamesContent reports whether an info dictionary says what the torrent
// carries: a length for a single file, a file list for many.
func infoNamesContent(info map[string]entry) bool {
	if _, ok := info["length"]; ok {
		return true
	}
	_, ok := info["files"]
	return ok
}

// MagnetURI parses a .torrent file and returns the magnet URI it describes.
func MagnetURI(data []byte) (string, error) {
	f, err := Parse(data)
	if err != nil {
		return "", err
	}
	return f.Magnet(), nil
}

// Magnet returns the magnet URI for the torrent.
//
// The trackers go along as "tr" parameters. A magnet without them is still a
// valid magnet, and on a public tracker it works — the client finds peers
// through the DHT. On a private tracker it does not, because there is no
// other way in: the announce URL in the file is the way in.
func (f *File) Magnet() string {
	var b strings.Builder

	b.WriteString("magnet:?xt=urn:btih:")
	b.WriteString(f.InfoHash)
	if f.Name != "" {
		b.WriteString("&dn=")
		b.WriteString(escapeMagnetValue(f.Name))
	}
	for _, t := range f.Trackers {
		b.WriteString("&tr=")
		b.WriteString(escapeMagnetValue(t))
	}

	return b.String()
}

// trackers collects the announce URLs of a torrent.
//
// Two fields carry them: "announce" is a single URL and "announce-list" is a
// list of tiers, each a list of URLs. Both are read, and the result is
// deduplicated — a file that lists the same tracker in both is not unusual,
// and a magnet with the same tracker twice is noise the client has to
// sort out.
func trackers(top map[string]entry) []string {
	var out []string
	seen := map[string]bool{}

	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	if announce, ok := top["announce"]; ok {
		if b, ok := asBytes(announce.value); ok {
			add(string(b))
		}
	}

	if list, ok := top["announce-list"]; ok {
		if tiers, ok := asList(list.value); ok {
			for _, tier := range tiers {
				urls, ok := asList(tier)
				if !ok {
					continue
				}
				for _, u := range urls {
					if b, ok := asBytes(u); ok {
						add(string(b))
					}
				}
			}
		}
	}

	return out
}

// infoSize returns the total size of the content the torrent carries.
//
// A torrent either names one file ("length") or many ("files", each with
// its own length). A file that says neither is not an error — the size is
// only ever shown — so it comes back as zero.
func infoSize(info map[string]entry) int64 {
	if length, ok := info["length"]; ok {
		if n, ok := asInt(length.value); ok {
			return n
		}
	}

	files, ok := info["files"]
	if !ok {
		return 0
	}
	list, ok := asList(files.value)
	if !ok {
		return 0
	}

	var total int64
	for _, f := range list {
		dict, ok := asDict(f)
		if !ok {
			continue
		}
		length, ok := dict["length"]
		if !ok {
			continue
		}
		if n, ok := asInt(length.value); ok {
			total += n
		}
	}
	return total
}

// escapeMagnetValue percent-encodes a value for a magnet parameter.
//
// QueryEscape is the right shape but spells a space as "+", which in a
// magnet URI is not what a client expects to read back, so the pluses are
// spelled out as %20.
func escapeMagnetValue(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
