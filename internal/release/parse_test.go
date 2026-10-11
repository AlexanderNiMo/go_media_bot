package release

import "testing"

func TestParseResolutions(t *testing.T) {
	for name, want := range map[string]Quality{
		"Матрица (1999) BDRemux 1080p":     Quality1080p,
		"The.Matrix.1999.2160p.UHD.BluRay": Quality2160p,
		"The Matrix 1999 4K HDR":           Quality2160p,
		"The Matrix 1999 720p WEB-DL":      Quality720p,
		"The Matrix 1999 480p DVDRip":      Quality480p,
		"The Matrix 1999 1080i HDTV":       Quality1080i,
		"The Matrix 1999 BDRip":            QualityUnknown,
		"Матрица (1999) Лицензия":          QualityUnknown,
	} {
		if got := Parse(name).Quality; got != want {
			t.Errorf("Parse(%q).Quality = %v, want %v", name, got, want)
		}
	}
}

func TestParseSources(t *testing.T) {
	for name, want := range map[string]Source{
		"Матрица (1999) BDRemux 1080p":       SourceBDRemux,
		"Матрица (1999) Blu-Ray Remux 1080p": SourceBDRemux,
		"The.Matrix.1999.1080p.BluRay.x264":  SourceBluRay,
		"The.Matrix.1999.1080p.BDRip.XviD":   SourceBDRip,
		"The.Matrix.1999.1080p.WEB-DL.H264":  SourceWebDL,
		"The.Matrix.1999.1080p.WEBDL.H264":   SourceWebDL,
		"The.Matrix.1999.1080p.WEBRip.x264":  SourceWebRip,
		"The.Matrix.1999.1080p.HDTV.x264":    SourceHDTV,
		"The.Matrix.1999.DVDRip.XviD":        SourceDVD,
		"The Matrix 1999 DVDScr XviD":        SourceScreener,
		"The Matrix 1999 HDTS 1080p":         SourceTelesync,
		"The Matrix 1999 CAM 1080p":          SourceCam,
		"Матрица (1999) 1080p":               SourceUnknown,
	} {
		if got := Parse(name).Source; got != want {
			t.Errorf("Parse(%q).Source = %v, want %v", name, got, want)
		}
	}
}

// TestParseSourcePrefersTheMoreSpecific — "BDRemux" contains "BD" and "BDRip"
// contains "BDRip"; matching in the wrong order silently downgrades every remux
// to a plain BluRay.
func TestParseSourcePrefersTheMoreSpecific(t *testing.T) {
	if got := Parse("Movie 2020 BDRemux 1080p").Source; got != SourceBDRemux {
		t.Errorf("BDRemux parsed as %v, want BDRemux", got)
	}
	if got := Parse("Movie 2020 BDRip 1080p").Source; got != SourceBDRip {
		t.Errorf("BDRip parsed as %v, want BDRip", got)
	}
	if got := Parse("Movie 2020 WEB-DL 1080p").Source; got != SourceWebDL {
		t.Errorf("WEB-DL parsed as %v, want WEB-DL", got)
	}
}

func TestParseCodecs(t *testing.T) {
	for name, want := range map[string]Codec{
		"Movie 2020 1080p x265":  CodecH265,
		"Movie 2020 1080p HEVC":  CodecH265,
		"Movie 2020 1080p H.265": CodecH265,
		"Movie 2020 1080p x264":  CodecH264,
		"Movie 2020 1080p H.264": CodecH264,
		"Movie 2020 1080p AVC":   CodecH264,
		"Movie 2020 1080p AV1":   CodecAV1,
		"Movie 2020 1080p":       CodecUnknown,
	} {
		if got := Parse(name).Codec; got != want {
			t.Errorf("Parse(%q).Codec = %v, want %v", name, got, want)
		}
	}
}

func TestParseYear(t *testing.T) {
	for name, want := range map[string]int{
		"The Matrix 1999 1080p":      1999,
		"Матрица (1999) BDRip 1080p": 1999,
		"Some Film 2023 2160p":       2023,
		"No Year Here 1080p":         0,
	} {
		if got := Parse(name).Year; got != want {
			t.Errorf("Parse(%q).Year = %d, want %d", name, got, want)
		}
	}
}

// TestParseSeason covers the conventions Russian trackers actually use. The
// "2 сезон" word order is as common there as "Season 2" is elsewhere.
func TestParseSeason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		season  int
		has     bool
		episode bool
	}{
		{"Game of Thrones S02 1080p", 2, true, false},
		{"Game of Thrones S2E05 1080p", 2, true, true},
		{"Game of Thrones Season 2 1080p", 2, true, false},
		{"Игра престолов - Сезон 2 (2012) 1080p", 2, true, false},
		{"Игра престолов 2 сезон 1080p", 2, true, false},
		{"The Matrix 1999 1080p", 0, false, false},
	} {
		got := Parse(tc.name)
		if got.HasSeason != tc.has {
			t.Errorf("Parse(%q).HasSeason = %v, want %v", tc.name, got.HasSeason, tc.has)
			continue
		}
		if tc.has && got.Season != tc.season {
			t.Errorf("Parse(%q).Season = %d, want %d", tc.name, got.Season, tc.season)
		}
		if got.HasEpisode != tc.episode {
			t.Errorf("Parse(%q).HasEpisode = %v, want %v", tc.name, got.HasEpisode, tc.episode)
		}
	}
}

// TestParseIgnoresResolutionInsideNumbers guards the word boundaries: without
// them "1080p" matches inside a longer token and a hash or a group name can be
// mistaken for a resolution.
func TestParseIgnoresResolutionInsideNumbers(t *testing.T) {
	// A group name containing the digits, not a resolution.
	if got := Parse("Movie 2020 1080person x264").Quality; got != QualityUnknown {
		t.Errorf("quality = %v, want unknown", got)
	}
}

func TestNormalise(t *testing.T) {
	for input, want := range map[string]string{
		"The.Matrix.1999.BDRemux.1080p": "the matrix 1999 bdremux 1080p",
		"The_Matrix_[1999]_1080p":       "the matrix 1999 1080p",
		"Матрица (1999) — Дубляж":       "матрица 1999 — дубляж",
		"a  -  b":                       "a b",
	} {
		if got := Normalise(input); got != want {
			t.Errorf("Normalise(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestQualityHelpers(t *testing.T) {
	if QualityFromHeight(1080) != Quality1080p {
		t.Error("1080 should map to 1080p")
	}
	if QualityFromHeight(2160) != Quality2160p {
		t.Error("2160 should map to 2160p")
	}
	if Quality2160p.Height() <= Quality1080p.Height() {
		t.Error("2160p must rank above 1080p")
	}
	// 1080i is the same vertical resolution as 1080p and must not be treated
	// as a lower tier by the filter.
	if Quality1080i.Height() != Quality1080p.Height() {
		t.Error("1080i and 1080p should have the same height")
	}
}
