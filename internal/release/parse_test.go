package release

import "testing"

func TestParseResolutions(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Quality
	}{
		{"BDRemux 1080p", "Матрица (1999) BDRemux 1080p", Quality1080p},
		{"UHD BluRay 2160p", "The.Matrix.1999.2160p.UHD.BluRay", Quality2160p},
		{"4K HDR", "The Matrix 1999 4K HDR", Quality2160p},
		{"WEB-DL 720p", "The Matrix 1999 720p WEB-DL", Quality720p},
		{"DVDRip 480p", "The Matrix 1999 480p DVDRip", Quality480p},
		{"HDTV 1080i", "The Matrix 1999 1080i HDTV", Quality1080i},
		{"no resolution", "The Matrix 1999 BDRip", QualityUnknown},
		{"no quality tag", "Матрица (1999) Лицензия", QualityUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.in).Quality; got != tc.want {
				t.Errorf("Parse(%q).Quality = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSources(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Source
	}{
		{"BDRemux", "Матрица (1999) BDRemux 1080p", SourceBDRemux},
		{"Blu-Ray Remux", "Матрица (1999) Blu-Ray Remux 1080p", SourceBDRemux},
		{"BluRay", "The.Matrix.1999.1080p.BluRay.x264", SourceBluRay},
		{"BDRip", "The.Matrix.1999.1080p.BDRip.XviD", SourceBDRip},
		{"WEB-DL", "The.Matrix.1999.1080p.WEB-DL.H264", SourceWebDL},
		{"WEBDL no dash", "The.Matrix.1999.1080p.WEBDL.H264", SourceWebDL},
		{"WEBRip", "The.Matrix.1999.1080p.WEBRip.x264", SourceWebRip},
		{"HDTV", "The.Matrix.1999.1080p.HDTV.x264", SourceHDTV},
		{"DVDRip", "The.Matrix.1999.DVDRip.XviD", SourceDVD},
		{"DVDScr", "The Matrix 1999 DVDScr XviD", SourceScreener},
		{"HDTS", "The Matrix 1999 HDTS 1080p", SourceTelesync},
		{"CAM", "The Matrix 1999 CAM 1080p", SourceCam},
		{"no source tag", "Матрица (1999) 1080p", SourceUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.in).Source; got != tc.want {
				t.Errorf("Parse(%q).Source = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseSourcePrefersTheMoreSpecific — "BDRemux" contains "BD" and "BDRip"
// contains "BDRip"; matching in the wrong order silently downgrades every remux
// to a plain BluRay.
func TestParseSourcePrefersTheMoreSpecific(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Source
	}{
		{"BDRemux", "Movie 2020 BDRemux 1080p", SourceBDRemux},
		{"BDRip", "Movie 2020 BDRip 1080p", SourceBDRip},
		{"WEB-DL", "Movie 2020 WEB-DL 1080p", SourceWebDL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.in).Source; got != tc.want {
				t.Errorf("Parse(%q).Source = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseCodecs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Codec
	}{
		{"x265", "Movie 2020 1080p x265", CodecH265},
		{"HEVC", "Movie 2020 1080p HEVC", CodecH265},
		{"H.265", "Movie 2020 1080p H.265", CodecH265},
		{"x264", "Movie 2020 1080p x264", CodecH264},
		{"H.264", "Movie 2020 1080p H.264", CodecH264},
		{"AVC", "Movie 2020 1080p AVC", CodecH264},
		{"AV1", "Movie 2020 1080p AV1", CodecAV1},
		{"no codec", "Movie 2020 1080p", CodecUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.in).Codec; got != tc.want {
				t.Errorf("Parse(%q).Codec = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseYear(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"Matrix 1999", "The Matrix 1999 1080p", 1999},
		{"Матрица 1999", "Матрица (1999) BDRip 1080p", 1999},
		{"Some film 2023", "Some Film 2023 2160p", 2023},
		{"no year", "No Year Here 1080p", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.in).Year; got != tc.want {
				t.Errorf("Parse(%q).Year = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseSeason covers the conventions Russian trackers actually use. The
// "2 сезон" word order is as common there as "Season 2" is elsewhere.
func TestParseSeason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		season  int
		has     bool
		episode bool
	}{
		{"S02", "Game of Thrones S02 1080p", 2, true, false},
		{"S2E05", "Game of Thrones S2E05 1080p", 2, true, true},
		{"Season 2", "Game of Thrones Season 2 1080p", 2, true, false},
		{"Сезон 2", "Игра престолов - Сезон 2 (2012) 1080p", 2, true, false},
		{"2 сезон", "Игра престолов 2 сезон 1080p", 2, true, false},
		{"no season", "The Matrix 1999 1080p", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in)
			if got.HasSeason != tc.has {
				t.Errorf("Parse(%q).HasSeason = %v, want %v", tc.in, got.HasSeason, tc.has)
			}
			if tc.has && got.Season != tc.season {
				t.Errorf("Parse(%q).Season = %d, want %d", tc.in, got.Season, tc.season)
			}
			if got.HasEpisode != tc.episode {
				t.Errorf("Parse(%q).HasEpisode = %v, want %v", tc.in, got.HasEpisode, tc.episode)
			}
		})
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
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"dots", "The.Matrix.1999.BDRemux.1080p", "the matrix 1999 bdremux 1080p"},
		{"underscores and brackets", "The_Matrix_[1999]_1080p", "the matrix 1999 1080p"},
		{"Cyrillic", "Матрица (1999) — Дубляж", "матрица 1999 — дубляж"},
		{"collapsed whitespace", "a  -  b", "a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalise(tc.in); got != tc.want {
				t.Errorf("Normalise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
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
