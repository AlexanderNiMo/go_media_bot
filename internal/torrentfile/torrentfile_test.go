package torrentfile

import (
	"strings"
	"testing"
)

// A bencoded single-file torrent: one announce, two announce tiers, and an
// info dictionary naming one 1024-byte file.
//
// The hashes below are the SHA-1 of the info dictionary exactly as it is
// written here. They are pinned rather than computed, because the whole
// point of the parser is that the hash comes out of the bytes on disk — a
// test that recomputes it with the same code would agree with a broken
// implementation.
const (
	singleFileTorrent = `d8:announce25:http://tracker/a/announce13:announce-listll25:http://tracker/a/announceel25:http://tracker/b/announceee4:infod6:lengthi1024e4:name8:test.txt12:piece lengthi16384e6:pieces20:01234567890123456789eee`
	singleFileHash    = "4509e0512e22c9023ae6bd39dc46374461319617"

	multiFileTorrent = `d4:infod4:name5:files5:filesld6:lengthi100e4:pathl1:aeed6:lengthi200e4:pathl1:beeeee`
	multiFileHash    = "d820d30d76b28ba9ca882e453019c7a54016241f"
)

func TestParseReadsTheHashNameAndTrackers(t *testing.T) {
	f, err := Parse([]byte(singleFileTorrent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if f.InfoHash != singleFileHash {
		t.Errorf("InfoHash = %q, want %q", f.InfoHash, singleFileHash)
	}
	if f.Name != "test.txt" {
		t.Errorf("Name = %q, want %q", f.Name, "test.txt")
	}
	if f.Size != 1024 {
		t.Errorf("Size = %d, want 1024", f.Size)
	}
	want := []string{"http://tracker/a/announce", "http://tracker/b/announce"}
	if len(f.Trackers) != len(want) {
		t.Fatalf("Trackers = %v, want %v", f.Trackers, want)
	}
	for i, tr := range want {
		if f.Trackers[i] != tr {
			t.Errorf("tracker %d = %q, want %q", i, f.Trackers[i], tr)
		}
	}
}

func TestParseSumsTheSizesOfAMultiFileTorrent(t *testing.T) {
	f, err := Parse([]byte(multiFileTorrent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if f.InfoHash != multiFileHash {
		t.Errorf("InfoHash = %q, want %q", f.InfoHash, multiFileHash)
	}
	if f.Size != 300 {
		t.Errorf("Size = %d, want 300", f.Size)
	}
}

func TestMagnetCarriesTheHashNameAndTrackers(t *testing.T) {
	f, err := Parse([]byte(singleFileTorrent))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := "magnet:?xt=urn:btih:" + singleFileHash +
		"&dn=test.txt" +
		"&tr=http%3A%2F%2Ftracker%2Fa%2Fannounce" +
		"&tr=http%3A%2F%2Ftracker%2Fb%2Fannounce"
	if got := f.Magnet(); got != want {
		t.Errorf("Magnet() =\n%s\nwant\n%s", got, want)
	}
}

// A magnet without the trackers is useless on a private tracker: there is no
// other way in. The announce URL inside the file is the way in.
func TestMagnetKeepsTheAnnounceURL(t *testing.T) {
	magnet, err := MagnetURI([]byte(singleFileTorrent))
	if err != nil {
		t.Fatalf("MagnetURI: %v", err)
	}

	if !strings.Contains(magnet, "&tr=") {
		t.Errorf("the magnet has no tracker: %s", magnet)
	}
}

func TestMagnetEscapesTheName(t *testing.T) {
	// "a b&c" — a space and an ampersand, which is a parameter separator.
	f, err := Parse([]byte(`d4:infod4:name5:a b&c6:lengthi42eee`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	magnet := f.Magnet()
	if !strings.HasSuffix(magnet, "&dn=a%20b%26c") {
		t.Errorf("the name was not escaped: %s", magnet)
	}
}

func TestParseRejectsSomethingThatIsNotATorrent(t *testing.T) {
	for _, tc := range []struct {
		descr string
		data  string
	}{
		{"not bencode at all", "hello, world"},
		{"a truncated file", singleFileTorrent[:40]},
		{"a list rather than a dictionary", "l4:teste"},
		{"a dictionary without an info dictionary", "d8:announce4:teste"},
		{"an info dictionary that is not a dictionary", "d4:infod5:abcee"},
		{"an info dictionary that names no files", "d4:infod4:name3:abce"},
		{"an empty file", ""},
	} {
		t.Run(tc.descr, func(t *testing.T) {
			if _, err := Parse([]byte(tc.data)); err == nil {
				t.Errorf("%s: parsed, want an error", tc.descr)
			}
		})
	}
}
