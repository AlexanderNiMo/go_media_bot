package release

import (
	"strconv"
	"strings"
	"testing"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/domain"
)

// rel builds a release with a size in gigabytes.
func rel(title string, sizeGB float64, seeders int) domain.Release {
	return domain.Release{
		GUID:        "guid-" + title,
		InfoHash:    strings.ToLower(strings.ReplaceAll(title, " ", ""))[:12],
		Title:       title,
		SizeBytes:   int64(sizeGB * (1 << 30)),
		Seeders:     seeders,
		Indexer:     "RuTracker",
		DownloadURL: "https://tracker/dl/" + title,
	}
}

func movieSpec() domain.MediaSpec {
	return domain.MediaSpec{
		Ref:           domain.MovieRef(603),
		Title:         "The Matrix",
		TitleRU:       "Матрица",
		OriginalTitle: "The Matrix",
		Year:          1999,
		RuntimeMin:    136,
	}
}

func seasonSpec() domain.MediaSpec {
	return domain.MediaSpec{
		Ref:           domain.SeasonRef(1399, 2),
		Title:         "Game of Thrones",
		TitleRU:       "Игра престолов",
		OriginalTitle: "Game of Thrones",
		Year:          2011,
		RuntimeMin:    60,
	}
}

func selectorFor(t *testing.T) Selector {
	t.Helper()
	return NewSelector(DefaultProfile())
}

func TestSelectRejectsEachHardFilter(t *testing.T) {
	s := selectorFor(t)
	spec := movieSpec()

	for _, tc := range []struct {
		descr   string
		release domain.Release
		reason  Reason
	}{
		{
			"cam rip",
			rel("Матрица (1999) 1080p CAM | Дубляж", 2, 50),
			ReasonExcludedSource,
		},
		{
			"telesync",
			rel("Матрица (1999) 1080p TS | Дубляж", 2, 50),
			ReasonExcludedSource,
		},
		{
			"screener",
			rel("Матрица (1999) 1080p DVDScr | Дубляж", 2, 50),
			ReasonExcludedSource,
		},
		{
			"resolution below the floor",
			rel("Матрица (1999) 720p WEB-DL | Дубляж", 2, 50),
			ReasonResolutionTooLow,
		},
		{
			"no resolution in the name",
			rel("Матрица (1999) BDRip | Дубляж", 2, 50),
			ReasonUnknownQuality,
		},
		{
			"too few seeders",
			rel("Матрица (1999) 1080p BDRemux | Дубляж", 10, 1),
			ReasonTooFewSeeders,
		},
		{
			"too small",
			rel("Матрица (1999) 1080p WEB-DL | Дубляж", 0.3, 50),
			ReasonTooSmall,
		},
		{
			"too large",
			rel("Матрица (1999) 2160p BDRemux | Дубляж", 90, 50),
			ReasonTooLarge,
		},
		{
			"english with russian subtitles",
			rel("The Matrix 1999 1080p BluRay | ENG + Rus subs", 10, 50),
			ReasonSubtitlesOnly,
		},
		{
			"english only",
			rel("The.Matrix.1999.1080p.BluRay.x264-RARBG", 10, 50),
			ReasonNoRussianAudio,
		},
	} {
		t.Run(tc.descr, func(t *testing.T) {
			verdict := s.Select(spec, []domain.Release{tc.release}, nil)

			if verdict.Acceptable {
				t.Errorf("%s: was accepted, want rejection", tc.descr)
				return
			}
			if len(verdict.Rejected) != 1 {
				t.Fatalf("%s: %d rejections", tc.descr, len(verdict.Rejected))
			}
			if got := verdict.Rejected[0].Reason; got != tc.reason {
				t.Errorf("%s: reason = %q, want %q (detail: %s)",
					tc.descr, got, tc.reason, verdict.Rejected[0].Detail)
			}
		})
	}
}

func TestSelectAcceptsAGoodRelease(t *testing.T) {
	s := selectorFor(t)

	release := rel("Матрица (1999) 1080p BDRemux | Дубляж", 20, 100)
	verdict := s.Select(movieSpec(), []domain.Release{release}, nil)

	if !verdict.Acceptable {
		t.Fatalf("a good release was refused: %s", verdict.Summary())
	}
	if verdict.Chosen.Title != release.Title {
		t.Errorf("chose %q", verdict.Chosen.Title)
	}
	if verdict.Audio.Tier != AudioDub {
		t.Errorf("audio tier = %v", verdict.Audio.Tier)
	}
	if verdict.Parsed.Quality != Quality1080p {
		t.Errorf("quality = %v", verdict.Parsed.Quality)
	}
}

// TestSelectPrefersDubOverOtherTiers — the whole point of ranking audio rather
// than treating it as a flag.
func TestSelectPrefersDubOverOtherTiers(t *testing.T) {
	s := selectorFor(t)
	spec := movieSpec()

	// Everything except the audio is identical, so audio alone decides.
	candidates := []domain.Release{
		rel("Матрица (1999) 1080p BDRemux | Одноголосый", 20, 100),
		rel("Матрица (1999) 1080p BDRemux | Двухголосый", 20, 100),
		rel("Матрица (1999) 1080p BDRemux | Многоголосый", 20, 100),
		rel("Матрица (1999) 1080p BDRemux | Дубляж", 20, 100),
	}

	verdict := s.Select(spec, candidates, nil)

	if !verdict.Acceptable {
		t.Fatalf("nothing was acceptable: %s", verdict.Summary())
	}
	if !strings.Contains(verdict.Chosen.Title, "Дубляж") {
		t.Errorf("chose %q, want the dub", verdict.Chosen.Title)
	}
}

func TestSelectPrefersHigherResolution(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(movieSpec(), []domain.Release{
		rel("Матрица (1999) 1080p BDRemux | Дубляж", 20, 100),
		rel("Матрица (1999) 2160p BDRemux | Дубляж", 45, 100),
	}, nil)

	if !strings.Contains(verdict.Chosen.Title, "2160p") {
		t.Errorf("chose %q, want the 2160p release", verdict.Chosen.Title)
	}
}

// TestAudioOutweighsResolution — a dub at 1080p is a better outcome for a user
// than a 2160p release they cannot understand. The weights have to reflect
// that, and this is the assertion that pins them down.
func TestAudioOutweighsResolution(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(movieSpec(), []domain.Release{
		rel("Матрица (1999) 2160p BDRemux | Закадровый", 45, 100),
		rel("Матрица (1999) 1080p BDRemux | Дубляж", 20, 100),
	}, nil)

	if !strings.Contains(verdict.Chosen.Title, "Дубляж") {
		t.Errorf("chose %q; intelligible audio should outweigh resolution", verdict.Chosen.Title)
	}
}

func TestSelectPrefersBetterSource(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(movieSpec(), []domain.Release{
		rel("Матрица (1999) 1080p HDTV | Дубляж", 20, 100),
		rel("Матрица (1999) 1080p WEB-DL | Дубляж", 20, 100),
		rel("Матрица (1999) 1080p BDRemux | Дубляж", 20, 100),
	}, nil)

	if !strings.Contains(verdict.Chosen.Title, "BDRemux") {
		t.Errorf("chose %q, want the best source", verdict.Chosen.Title)
	}
}

func TestSelectPrefersMoreSeeders(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(movieSpec(), []domain.Release{
		rel("Матрица (1999) 1080p BDRemux | Дубляж A", 20, 4),
		rel("Матрица (1999) 1080p BDRemux | Дубляж B", 20, 200),
	}, nil)

	if !strings.HasSuffix(verdict.Chosen.Title, "B") {
		t.Errorf("chose %q, want the better-seeded release", verdict.Chosen.Title)
	}
}

// TestUnknownSeedersAreNotTreatedAsZero — an indexer that omits the field
// would otherwise have every one of its releases rejected, making it look
// permanently empty.
func TestUnknownSeedersAreNotTreatedAsZero(t *testing.T) {
	s := selectorFor(t)

	release := rel("Матрица (1999) 1080p BDRemux | Дубляж", 20, 0)
	release.Seeders = -1 // the indexer did not report them

	verdict := s.Select(movieSpec(), []domain.Release{release}, nil)
	if !verdict.Acceptable {
		t.Errorf("a release with unknown seeders was refused: %s", verdict.Summary())
	}
}

func TestBlacklistIsRespected(t *testing.T) {
	s := selectorFor(t)
	release := rel("Матрица (1999) 1080p BDRemux | Дубляж", 20, 100)

	t.Run("blacklisted by infohash", func(t *testing.T) {
		verdict := s.Select(movieSpec(), []domain.Release{release},
			map[string]bool{release.InfoHash: true})

		if verdict.Acceptable {
			t.Fatal("a blacklisted release was chosen")
		}
		if verdict.Rejected[0].Reason != ReasonBlacklisted {
			t.Errorf("reason = %q", verdict.Rejected[0].Reason)
		}
	})

	// The blacklist is also keyed on the guid when there is no infohash, which
	// is the case for magnet-only indexers.
	t.Run("blacklisted by guid (no infohash)", func(t *testing.T) {
		noHash := rel("Матрица (1999) 1080p BDRemux | Дубляж", 20, 100)
		noHash.InfoHash = ""

		verdict := s.Select(movieSpec(), []domain.Release{noHash},
			map[string]bool{noHash.GUID: true})
		if verdict.Acceptable {
			t.Error("a release blacklisted by guid was chosen")
		}
	})
}

func TestSelectRejectsReleaseWithNoLink(t *testing.T) {
	s := selectorFor(t)

	release := rel("Матрица (1999) 1080p BDRemux | Дубляж", 20, 100)
	release.DownloadURL = ""
	release.MagnetURI = ""

	verdict := s.Select(movieSpec(), []domain.Release{release}, nil)
	if verdict.Acceptable {
		t.Fatal("a release with nothing to download was chosen")
	}
	if verdict.Rejected[0].Reason != ReasonNoDownloadLink {
		t.Errorf("reason = %q", verdict.Rejected[0].Reason)
	}
}

// ---------------------------------------------------------------------------
// Season handling
// ---------------------------------------------------------------------------

// TestSingleEpisodeIsNotASeason is the case that would otherwise silently fill
// the download slot with a tenth of what was asked for.
func TestSingleEpisodeIsNotASeason(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(seasonSpec(), []domain.Release{
		rel("Игра престолов S02E05 1080p WEB-DL | LostFilm", 3, 100),
	}, nil)

	if verdict.Acceptable {
		t.Fatal("a single episode was accepted for a whole-season request")
	}
	if verdict.Rejected[0].Reason != ReasonIsSingleEpisode {
		t.Errorf("reason = %q, want %q", verdict.Rejected[0].Reason, ReasonIsSingleEpisode)
	}
}

func TestSeasonPackIsAccepted(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(seasonSpec(), []domain.Release{
		rel("Игра престолов - Сезон 2 (2012) 1080p BDRip | LostFilm", 25, 100),
	}, nil)

	if !verdict.Acceptable {
		t.Fatalf("a season pack was refused: %s", verdict.Summary())
	}
	if verdict.Parsed.Season != 2 {
		t.Errorf("parsed season = %d", verdict.Parsed.Season)
	}
}

func TestWrongSeasonIsRejected(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(seasonSpec(), []domain.Release{
		rel("Игра престолов - Сезон 3 (2013) 1080p BDRip | LostFilm", 25, 100),
	}, nil)

	if verdict.Acceptable {
		t.Fatal("season 3 was accepted for a season 2 request")
	}
	if verdict.Rejected[0].Reason != ReasonWrongSeason {
		t.Errorf("reason = %q", verdict.Rejected[0].Reason)
	}
}

// TestSeasonlessReleaseIsAllowed — many season packs do not name the season,
// and the indexer was asked for one specifically. Refusing them all would
// discard most of the available releases.
func TestSeasonlessReleaseIsAllowed(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(seasonSpec(), []domain.Release{
		rel("Игра престолов (2011) 1080p BDRip | LostFilm | Все сезоны", 25, 100),
	}, nil)

	if !verdict.Acceptable {
		t.Errorf("a release with no season in its name was refused: %s", verdict.Summary())
	}
}

// ---------------------------------------------------------------------------
// Year handling
// ---------------------------------------------------------------------------

// TestRemakeIsRejected — asking for the 1999 film must not download the 2024
// one, which a title-only match would happily do.
func TestRemakeIsRejected(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(movieSpec(), []domain.Release{
		rel("Матрица (2024) 1080p WEB-DL | Дубляж", 10, 100),
	}, nil)

	if verdict.Acceptable {
		t.Fatal("a 2024 release was accepted for a 1999 request")
	}
	if verdict.Rejected[0].Reason != ReasonWrongYear {
		t.Errorf("reason = %q", verdict.Rejected[0].Reason)
	}
}

// TestYearTolerance — a release name's year is often the festival première or
// the disc release rather than the theatrical one, and refusing a good release
// over a one-year difference would be worse than accepting an occasional
// remake.
func TestYearTolerance(t *testing.T) {
	s := selectorFor(t)

	for _, year := range []int{1998, 1999, 2000} {
		t.Run(strconv.Itoa(year), func(t *testing.T) {
			title := "Матрица (" + strconv.Itoa(year) + ") 1080p BDRemux | Дубляж"
			verdict := s.Select(movieSpec(), []domain.Release{rel(title, 20, 100)}, nil)
			if !verdict.Acceptable {
				t.Errorf("year %d was rejected: %s", year, verdict.Summary())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Verdict reporting
// ---------------------------------------------------------------------------

// TestVerdictSummaryCountsReasons — the summary is what a user sees when
// nothing downloaded, so it has to say why rather than just "nothing found".
func TestVerdictSummaryCountsReasons(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(movieSpec(), []domain.Release{
		rel("Матрица (1999) 1080p CAM | Дубляж", 2, 100),
		rel("Матрица (1999) 1080p TS | Дубляж", 2, 100),
		rel("The Matrix 1999 1080p | ENG + Rus subs", 10, 100),
	}, nil)

	summary := verdict.Summary()

	if !strings.Contains(summary, "3 releases") {
		t.Errorf("summary = %q, want it to mention the count", summary)
	}
	if !strings.Contains(summary, string(ReasonExcludedSource)) {
		t.Errorf("summary = %q, want it to name the commonest reason", summary)
	}
}

func TestVerdictSummaryWhenNoResults(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(movieSpec(), nil, nil)
	if got := verdict.Summary(); !strings.Contains(got, "indexer") {
		t.Errorf("summary = %q, want it to say the indexer returned nothing", got)
	}
}

func TestSelectPicksTheBestAmongMany(t *testing.T) {
	s := selectorFor(t)

	verdict := s.Select(movieSpec(), []domain.Release{
		rel("Матрица (1999) 1080p HDTV | Закадровый", 5, 10),
		rel("Матрица (1999) 1080p WEB-DL | Многоголосый", 12, 40),
		rel("Матрица (1999) 1080p CAM | Дубляж", 2, 200),             // excluded source
		rel("The Matrix 1999 1080p BluRay | ENG + Rus subs", 10, 99), // subtitles only
		rel("Матрица (1999) 2160p BDRemux | Дубляж", 45, 80),         // the winner
		rel("Матрица (1999) 720p WEB-DL | Дубляж", 2, 100),           // below the floor
	}, nil)

	if !verdict.Acceptable {
		t.Fatalf("nothing acceptable: %s", verdict.Summary())
	}
	if !strings.Contains(verdict.Chosen.Title, "2160p") {
		t.Errorf("chose %q", verdict.Chosen.Title)
	}
	if verdict.Considered != 6 {
		t.Errorf("considered = %d, want 6", verdict.Considered)
	}
	// Rejected: the cam rip, the subtitles-only release, and the 720p one. The
	// HDTV release with a voice-over passes — it is a poor release, not an
	// unacceptable one, and the score is what demotes it.
	if len(verdict.Rejected) != 3 {
		t.Errorf("rejected = %d, want 3", len(verdict.Rejected))
	}
}

// TestProfileFromConfig checks that configuration actually reaches the policy.
func TestProfileFromConfig(t *testing.T) {
	p := ProfileFromConfig(qualityConfig(720, 1080, 1, 0.5, 10))

	if p.MinResolution != Quality720p {
		t.Errorf("min = %v", p.MinResolution)
	}
	if p.PreferredResolution != Quality1080p {
		t.Errorf("preferred = %v", p.PreferredResolution)
	}
	if p.MinSeeders != 1 || p.MinSizeGB != 0.5 || p.MaxSizeGB != 10 {
		t.Errorf("numeric settings did not reach the profile: %+v", p)
	}
}

// TestPreferredResolutionBelowMinimumIsCorrected — a preferred resolution
// under the floor would make the score nonsensical, and a config typo should
// not produce that.
func TestPreferredResolutionBelowMinimumIsCorrected(t *testing.T) {
	p := ProfileFromConfig(qualityConfig(1080, 720, 3, 1, 60))

	if p.PreferredResolution < p.MinResolution {
		t.Errorf("preferred %v is below minimum %v", p.PreferredResolution, p.MinResolution)
	}
}

// TestExcludedSourcesFromConfig — the list is a user preference, so what the
// configuration says has to be what the selector applies.
func TestExcludedSourcesFromConfig(t *testing.T) {
	cfg := qualityConfig(1080, 2160, 3, 1, 60)
	cfg.Quality.ExcludedSources = []string{"hdtv", "webrip", "nonsense"}

	p := ProfileFromConfig(cfg)

	t.Run("HDTV excluded by config", func(t *testing.T) {
		if !p.Excludes(SourceHDTV) {
			t.Error("HDTV should be excluded when the configuration says so")
		}
	})
	t.Run("WEBRip excluded by config", func(t *testing.T) {
		if !p.Excludes(SourceWebRip) {
			t.Error("WEBRip should be excluded")
		}
	})
	// CAM is not in the configured list, so the default no longer applies.
	t.Run("CAM not in configured list", func(t *testing.T) {
		if p.Excludes(SourceCam) {
			t.Error("CAM should not be excluded when the configuration replaces the list")
		}
	})
}

// qualityConfig builds a configuration with the quality policy overridden.
func qualityConfig(minH, prefH, minSeeders int, minGB, maxGB float64) *config.Config {
	cfg := &config.Config{}
	cfg.Quality.MinResolution = minH
	cfg.Quality.PreferredResolution = prefH
	cfg.Quality.MinSeeders = minSeeders
	cfg.Quality.MinSizeGB = minGB
	cfg.Quality.MaxSizeGB = maxGB
	cfg.Quality.RequireRussianAudio = true
	return cfg
}
