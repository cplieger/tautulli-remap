package remap

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeGUID(t *testing.T) {
	tests := []struct {
		name string
		guid string
		want string
	}{
		{"imdb legacy with query params", "com.plexapp.agents.imdb://tt1234567?lang=en", "imdb://tt1234567"},
		{"imdb bare", "imdb://tt9999999", "imdb://tt9999999"},
		{"themoviedb legacy agent", "com.plexapp.agents.themoviedb://12345?lang=en", "tmdb://12345"},
		{"tmdb new agent", "tmdb://67890", "tmdb://67890"},
		{"tmdb with query params", "tmdb://12345?lang=en", "tmdb://12345"},
		{"thetvdb legacy with season path", "com.plexapp.agents.thetvdb://271557/3/1?lang=en", "tvdb://271557"},
		{"thetvdb with deep path", "com.plexapp.agents.thetvdb://271557/3/1/5?lang=en", "tvdb://271557"},
		{"tvdb new agent", "tvdb://271557", "tvdb://271557"},
		{"plex movie", "plex://movie/5d776b59ad5437001f79c6f8", "plex://movie/5d776b59ad5437001f79c6f8"},
		{"plex episode", "plex://episode/5d9c135046115600200d30a2", "plex://episode/5d9c135046115600200d30a2"},
		{"mbid", "mbid://abcdef01-2345-6789-abcd-ef0123456789", "mbid://abcdef01-2345-6789-abcd-ef0123456789"},
		// A leading-slash id (index 0) strips to empty; empty id is
		// unsupported.
		{"thetvdb leading-slash id strips to empty", "com.plexapp.agents.thetvdb:///271557", ""},
		{"local unsupported", "local://616507", ""},
		{"agents.none unsupported", "com.plexapp.agents.none://632d404bf27d52a513ccd45e4df820cd276f3090?lang=xn", ""},
		{"unknown scheme", "custom://something", ""},
		{"empty string", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeGUID(tt.guid); got != tt.want {
				t.Errorf("NormalizeGUID(%q) = %q, want %q", tt.guid, got, tt.want)
			}
		})
	}
}

func TestNormalizeGUID_tvdb_strips_deep_paths(t *testing.T) {
	tests := []struct {
		name, guid, want string
	}{
		{"tvdb preserves path (new agent)", "tvdb://271557/3", "tvdb://271557/3"},
		{"tvdb with no path", "tvdb://271557", "tvdb://271557"},
		{"thetvdb strips path (legacy agent)", "thetvdb://12345/1/2?lang=en", "tvdb://12345"},
		{"thetvdb strips single path segment", "thetvdb://99999/1?lang=en", "tvdb://99999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeGUID(tt.guid); got != tt.want {
				t.Errorf("NormalizeGUID(%q) = %q, want %q", tt.guid, got, tt.want)
			}
		})
	}
}

func TestExtractAfter(t *testing.T) {
	tests := []struct {
		s, prefix, want string
	}{
		{"imdb://tt1234567", "imdb://", "tt1234567"},
		{"com.plexapp.agents.imdb://tt1234567?lang=en", "imdb://", "tt1234567"},
		{"tmdb://12345", "imdb://", ""},
		// A query separator at the remainder's start must still strip.
		{"imdb://?lang=en", "imdb://", ""},
		{"", "imdb://", ""},
	}
	for _, tt := range tests {
		if got := extractAfter(tt.s, tt.prefix); got != tt.want {
			t.Errorf("extractAfter(%q, %q) = %q, want %q", tt.s, tt.prefix, got, tt.want)
		}
	}
}

func TestMatchOne(t *testing.T) {
	t.Run("guid match", func(t *testing.T) {
		item := &TautulliEntry{GUID: "imdb://tt1", Title: "M", Year: "2020", MediaType: Movie}
		byGUID := map[string]PlexEntry{"imdb://tt1": {RatingKey: "200", Type: Movie}}
		key, method, matchedYear := matchOne(item, "100", nil, Index{ByGUID: byGUID}, Fallbacks{TitleYear: true, TitleOnly: true})
		if key != "200" || method != MethodGUID {
			t.Errorf("got (%q, %q), want (200, guid)", key, method)
		}
		if matchedYear != "" {
			t.Errorf("matchedYear = %q, want empty (only title-only matches carry it)", matchedYear)
		}
	})

	t.Run("guid cross-type rejected", func(t *testing.T) {
		// byGUID is keyed by the bare GUID, not by type, so a stale Movie's
		// tmdb:// ID must not match a Show indexed under the same GUID.
		item := &TautulliEntry{GUID: "tmdb://12345", Title: "Quiz Show", Year: "1994", MediaType: Movie}
		byGUID := map[string]PlexEntry{"tmdb://12345": {RatingKey: "200", Year: "1994", Type: Show}}
		key, method, matchedYear := matchOne(item, "100", nil, Index{ByGUID: byGUID}, Fallbacks{TitleYear: true, TitleOnly: true})
		if key != "" || method != "" || matchedYear != "" {
			t.Errorf("got (%q, %q, %q), want empty (GUID strategy must reject a cross-type match)", key, method, matchedYear)
		}
	})

	t.Run("title+year fallback", func(t *testing.T) {
		item := &TautulliEntry{Title: "Movie", Year: "2020", MediaType: Movie}
		byTY := map[string]PlexEntry{titleYearKey("movie", "2020", Movie): {RatingKey: "200", Type: Movie}}
		key, method, matchedYear := matchOne(item, "100", nil, Index{ByTitleYear: byTY}, Fallbacks{TitleYear: true, TitleOnly: true})
		if key != "200" || method != methodTitleYear {
			t.Errorf("got (%q, %q), want (200, title+year)", key, method)
		}
		if matchedYear != "" {
			t.Errorf("matchedYear = %q, want empty (title+year matches are same-year by construction)", matchedYear)
		}
	})

	t.Run("no match", func(t *testing.T) {
		item := &TautulliEntry{Title: "X", Year: "2020", MediaType: Movie}
		key, method, matchedYear := matchOne(item, "100", nil, Index{}, Fallbacks{TitleYear: true, TitleOnly: true})
		if key != "" || method != "" || matchedYear != "" {
			t.Errorf("got (%q, %q, %q), want empty", key, method, matchedYear)
		}
	})

	t.Run("episode-guid resolution takes priority over GUID index", func(t *testing.T) {
		// A resolved show key is exact; it wins over a lower-confidence GUID match.
		item := &TautulliEntry{Title: "Show", Year: "2021", MediaType: Show, GUID: "tvdb://1"}
		byGUID := map[string]PlexEntry{"tvdb://1": {RatingKey: "999", Type: Show}}
		resolved := map[string]string{"100": "200"}
		key, method, matchedYear := matchOne(item, "100", resolved, Index{ByGUID: byGUID}, Fallbacks{TitleYear: true, TitleOnly: true})
		if key != "200" || method != methodEpisodeGUID {
			t.Errorf("got (%q, %q), want (200, episode-guid)", key, method)
		}
		if matchedYear != "" {
			t.Errorf("matchedYear = %q, want empty (only title-only matches carry it)", matchedYear)
		}
	})

	t.Run("episode-guid resolution to the same key is not a match", func(t *testing.T) {
		item := &TautulliEntry{Title: "Show", Year: "2021", MediaType: Show}
		resolved := map[string]string{"100": "100"}
		key, method, matchedYear := matchOne(item, "100", resolved, Index{}, Fallbacks{TitleYear: true, TitleOnly: true})
		if key != "" || method != "" || matchedYear != "" {
			t.Errorf("got (%q, %q, %q), want empty (a no-op resolution must not match)", key, method, matchedYear)
		}
	})
}

func TestProcessHistoryRow(t *testing.T) {
	t.Run("movie with valid rating key", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 42, Title: "Test Movie",
			Year: 2020, MediaType: "movie",
			GUID: "imdb://tt1234567",
		}
		ProcessHistoryRow(row, items)
		if len(items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(items))
		}
		if items["42"].Title != "Test Movie" {
			t.Errorf("unexpected title: %s", items["42"].Title)
		}
	})

	t.Run("movie with zero rating key is skipped", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 0, Title: "Bad Movie",
			Year: 2020, MediaType: "movie",
		}
		ProcessHistoryRow(row, items)
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})

	t.Run("episode uses grandparent rating key", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 99, GrandparentRatingKey: 50,
			Title: "Episode Title", GrandparentTitle: "Show Title",
			Year: 2021, MediaType: "episode",
			GUID: "tvdb://271557",
		}
		ProcessHistoryRow(row, items)
		if len(items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(items))
		}
		entry := items["50"]
		if entry.Title != "Show Title" {
			t.Errorf("expected grandparent title, got %q", entry.Title)
		}
		if entry.MediaType != Show {
			t.Errorf("expected media type 'show', got %q", entry.MediaType)
		}
	})

	t.Run("episode with zero grandparent key is skipped", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 99, GrandparentRatingKey: 0,
			Title: "Episode", MediaType: "episode",
		}
		ProcessHistoryRow(row, items)
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})

	t.Run("episode captures plex episode GUID for resolution", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 99, GrandparentRatingKey: 50,
			Title: "Ep", GrandparentTitle: "Show",
			Year: 2021, MediaType: "episode",
			GUID: "plex://episode/5d9c135046115600200d30a2",
		}
		captured := ProcessHistoryRow(row, items)
		if !captured {
			t.Error("expected captured=true for plex episode GUID")
		}
		entry := items["50"]
		// An episode-scoped GUID must not be used as the show's own GUID...
		if entry.GUID != "" {
			t.Errorf("expected empty show GUID for plex episode, got %q", entry.GUID)
		}
		// ...but it must be retained so the show can be resolved through it.
		if want := []string{"plex://episode/5d9c135046115600200d30a2"}; !slices.Equal(entry.EpisodeGUIDs, want) {
			t.Errorf("EpisodeGUIDs = %v, want %v", entry.EpisodeGUIDs, want)
		}
	})

	t.Run("multiple episodes accumulate distinct GUIDs and dedup", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		mk := func(guid string) *HistoryItem {
			return &HistoryItem{
				RatingKey: 1, GrandparentRatingKey: 50, Title: "Ep",
				GrandparentTitle: "Show", Year: 2021, MediaType: "episode", GUID: guid,
			}
		}
		ProcessHistoryRow(mk("plex://episode/aaa"), items)
		ProcessHistoryRow(mk("plex://episode/bbb"), items)
		ProcessHistoryRow(mk("plex://episode/aaa"), items) // duplicate: ignored
		got := items["50"].EpisodeGUIDs
		if want := []string{"plex://episode/aaa", "plex://episode/bbb"}; !slices.Equal(got, want) {
			t.Errorf("EpisodeGUIDs = %v, want %v", got, want)
		}
	})

	t.Run("episode GUIDs are capped per show", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		for i := range maxEpisodeGUIDsPerShow + 3 {
			row := &HistoryItem{
				RatingKey: flexInt(i + 1), GrandparentRatingKey: 50, Title: "Ep",
				GrandparentTitle: "Show", Year: 2021, MediaType: "episode",
				GUID: fmt.Sprintf("plex://episode/%d", i),
			}
			ProcessHistoryRow(row, items)
		}
		if got := len(items["50"].EpisodeGUIDs); got != maxEpisodeGUIDsPerShow {
			t.Errorf("len(EpisodeGUIDs) = %d, want cap %d", got, maxEpisodeGUIDsPerShow)
		}
	})

	t.Run("legacy episode GUID becomes show GUID and is not captured", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 99, GrandparentRatingKey: 50, Title: "Ep",
			GrandparentTitle: "Show", Year: 2021, MediaType: "episode",
			GUID: "com.plexapp.agents.thetvdb://121361/6/1?lang=en",
		}
		if captured := ProcessHistoryRow(row, items); captured {
			t.Error("legacy episode GUID should not be captured as an episode GUID")
		}
		entry := items["50"]
		if entry.GUID != "tvdb://121361" {
			t.Errorf("show GUID = %q, want tvdb://121361", entry.GUID)
		}
		if len(entry.EpisodeGUIDs) != 0 {
			t.Errorf("EpisodeGUIDs = %v, want empty", entry.EpisodeGUIDs)
		}
	})

	t.Run("later legacy row fills a show GUID left empty by a plex episode row", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		ProcessHistoryRow(&HistoryItem{
			RatingKey: 1, GrandparentRatingKey: 50, Title: "Ep1",
			GrandparentTitle: "Show", Year: 2021, MediaType: "episode",
			GUID: "plex://episode/aaa",
		}, items)
		ProcessHistoryRow(&HistoryItem{
			RatingKey: 2, GrandparentRatingKey: 50, Title: "Ep2",
			GrandparentTitle: "Show", Year: 2021, MediaType: "episode",
			GUID: "thetvdb://121361/6/2",
		}, items)
		entry := items["50"]
		if entry.GUID != "tvdb://121361" {
			t.Errorf("show GUID = %q, want tvdb://121361 (filled by later legacy row)", entry.GUID)
		}
		if want := []string{"plex://episode/aaa"}; !slices.Equal(entry.EpisodeGUIDs, want) {
			t.Errorf("EpisodeGUIDs = %v, want %v", entry.EpisodeGUIDs, want)
		}
	})

	t.Run("episode falls back to episode title when grandparent empty", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 99, GrandparentRatingKey: 50,
			Title: "Fallback Title", GrandparentTitle: "",
			Year: 2021, MediaType: "episode",
		}
		ProcessHistoryRow(row, items)
		if items["50"].Title != "Fallback Title" {
			t.Errorf("expected fallback title, got %q", items["50"].Title)
		}
	})

	t.Run("duplicate key is not overwritten", func(t *testing.T) {
		items := map[string]TautulliEntry{
			"42": {Title: "First", Year: "2020", MediaType: Movie},
		}
		row := &HistoryItem{
			RatingKey: 42, Title: "Second",
			Year: 2021, MediaType: "movie",
		}
		ProcessHistoryRow(row, items)
		if items["42"].Title != "First" {
			t.Errorf("expected first entry preserved, got %q", items["42"].Title)
		}
	})

	t.Run("unknown media type is ignored", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 42, Title: "Music",
			Year: 2020, MediaType: "track",
		}
		ProcessHistoryRow(row, items)
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})

	t.Run("episode with zero grandparent flexInt is skipped", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 99, GrandparentRatingKey: 0,
			Title: "Episode", GrandparentTitle: "Show",
			Year: 2021, MediaType: "episode",
		}
		ProcessHistoryRow(row, items)
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})

	t.Run("episode duplicate grandparent not overwritten", func(t *testing.T) {
		items := map[string]TautulliEntry{
			"50": {Title: "First Show", Year: "2020", MediaType: Show},
		}
		row := &HistoryItem{
			RatingKey: 99, GrandparentRatingKey: 50,
			Title: "New Episode", GrandparentTitle: "Second Show",
			Year: 2021, MediaType: "episode",
		}
		ProcessHistoryRow(row, items)
		if items["50"].Title != "First Show" {
			t.Errorf("expected first entry preserved, got %q", items["50"].Title)
		}
	})

	t.Run("movie with flexInt rating key", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: 42, Title: "String Key Movie",
			Year: 2020, MediaType: "movie", GUID: "imdb://tt1234567",
		}
		ProcessHistoryRow(row, items)
		if len(items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(items))
		}
		if items["42"].Title != "String Key Movie" {
			t.Errorf("title = %q, want %q", items["42"].Title, "String Key Movie")
		}
	})

	t.Run("negative rating key is skipped", func(t *testing.T) {
		items := map[string]TautulliEntry{}
		row := &HistoryItem{
			RatingKey: flexInt(-1), Title: "Negative Key",
			Year: 2020, MediaType: "movie",
		}
		ProcessHistoryRow(row, items)
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})
}

func TestMatchStaleItems(t *testing.T) {
	type check func(t *testing.T, matched []MatchResult, unmatched []UnmatchResult)

	matchCount := func(wantMatched, wantUnmatched int) check {
		return func(t *testing.T, matched []MatchResult, unmatched []UnmatchResult) {
			t.Helper()
			if len(matched) != wantMatched {
				t.Errorf("matched: got %d, want %d", len(matched), wantMatched)
			}
			if len(unmatched) != wantUnmatched {
				t.Errorf("unmatched: got %d, want %d", len(unmatched), wantUnmatched)
			}
		}
	}
	matchKey := func(idx int, key string) check {
		return func(t *testing.T, matched []MatchResult, _ []UnmatchResult) {
			t.Helper()
			if idx >= len(matched) {
				t.Fatalf("matched[%d] out of range (len=%d)", idx, len(matched))
			}
			if matched[idx].NewKey != key {
				t.Errorf("matched[%d].NewKey = %q, want %q", idx, matched[idx].NewKey, key)
			}
		}
	}
	wantMethod := func(idx int, method matchMethod) check {
		return func(t *testing.T, matched []MatchResult, _ []UnmatchResult) {
			t.Helper()
			if idx >= len(matched) {
				return
			}
			if matched[idx].Method != method {
				t.Errorf("matched[%d].Method = %q, want %q", idx, matched[idx].Method, method)
			}
		}
	}
	matchMethodPrefix := func(idx int, prefix string) check {
		return func(t *testing.T, matched []MatchResult, _ []UnmatchResult) {
			t.Helper()
			if idx >= len(matched) {
				return
			}
			if !strings.HasPrefix(string(matched[idx].Method), prefix) {
				t.Errorf("matched[%d].Method = %q, want prefix %q", idx, matched[idx].Method, prefix)
			}
		}
	}
	unmatchedKey := func(idx int, key string) check {
		return func(t *testing.T, _ []MatchResult, unmatched []UnmatchResult) {
			t.Helper()
			if idx >= len(unmatched) {
				return
			}
			if unmatched[idx].OldKey != key {
				t.Errorf("unmatched[%d].OldKey = %q, want %q", idx, unmatched[idx].OldKey, key)
			}
		}
	}

	tests := []struct {
		name        string
		stale       map[string]TautulliEntry
		byGUID      map[string]PlexEntry
		byTitleYear map[string]PlexEntry
		byTitle     map[string]PlexEntry
		checks      []check
		enableTY    bool
		enableTO    bool
	}{
		{
			name:        "guid match takes priority over title+year",
			stale:       map[string]TautulliEntry{"100": {Title: "The Matrix", Year: "1999", MediaType: Movie, GUID: "imdb://tt0133093"}},
			byGUID:      map[string]PlexEntry{"imdb://tt0133093": {RatingKey: "200", Year: "1999", Type: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("the matrix", "1999", Movie): {RatingKey: "300", Year: "1999", Type: Movie}},
			enableTY:    true, enableTO: true,
			checks: []check{matchCount(1, 0), matchKey(0, "200"), wantMethod(0, MethodGUID)},
		},
		{
			name:        "title+year fallback when no guid",
			stale:       map[string]TautulliEntry{"100": {Title: "Inception", Year: "2010", MediaType: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("inception", "2010", Movie): {RatingKey: "200", Year: "2010", Type: Movie}},
			enableTY:    true, enableTO: true,
			checks: []check{matchCount(1, 0), matchKey(0, "200"), wantMethod(0, methodTitleYear)},
		},
		{
			name:     "title-only fallback with matching type",
			stale:    map[string]TautulliEntry{"100": {Title: "Dune", Year: "2020", MediaType: Movie}},
			byTitle:  map[string]PlexEntry{titleKey("dune", Movie): {RatingKey: "200", Year: "2021", Type: Movie}},
			enableTY: true, enableTO: true,
			checks: []check{matchCount(1, 0), matchMethodPrefix(0, "title only")},
		},
		{
			name:     "title-only rejects type mismatch",
			stale:    map[string]TautulliEntry{"100": {Title: "Home Alone", Year: "2025", MediaType: Show}},
			byTitle:  map[string]PlexEntry{titleKey("home alone", Movie): {RatingKey: "200", Year: "1990", Type: Movie}},
			enableTY: true, enableTO: true,
			checks: []check{matchCount(0, 1)},
		},
		{
			// The title+year lookup key folds in media type, so a stale Movie
			// can only resolve to a Movie slot.
			name:        "title+year rejects type mismatch",
			stale:       map[string]TautulliEntry{"100": {Title: "Heat", Year: "1995", MediaType: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("heat", "1995", Show): {RatingKey: "200", Year: "1995", Type: Show}},
			enableTY:    true, enableTO: false,
			checks: []check{matchCount(0, 1)},
		},
		{
			name:        "case insensitive title matching",
			stale:       map[string]TautulliEntry{"100": {Title: "THE MATRIX", Year: "1999", MediaType: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("the matrix", "1999", Movie): {RatingKey: "200", Year: "1999", Type: Movie}},
			enableTY:    true, enableTO: true,
			checks: []check{matchCount(1, 0), matchKey(0, "200")},
		},
		{
			name:     "same key is not remapped",
			stale:    map[string]TautulliEntry{"200": {Title: "The Matrix", Year: "1999", MediaType: Movie, GUID: "imdb://tt0133093"}},
			byGUID:   map[string]PlexEntry{"imdb://tt0133093": {RatingKey: "200", Year: "1999", Type: Movie}},
			enableTY: true, enableTO: true,
			checks: []check{matchCount(0, 1)},
		},
		{
			name:     "no match produces unmatched",
			stale:    map[string]TautulliEntry{"100": {Title: "Nonexistent", Year: "2025", MediaType: Movie, GUID: "imdb://tt0000000"}},
			enableTY: true, enableTO: true,
			checks: []check{matchCount(0, 1), unmatchedKey(0, "100")},
		},
		{
			name:        "title+year disabled skips fallback",
			stale:       map[string]TautulliEntry{"100": {Title: "Inception", Year: "2010", MediaType: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("inception", "2010", Movie): {RatingKey: "200", Year: "2010", Type: Movie}},
			enableTY:    false, enableTO: false,
			checks: []check{matchCount(0, 1)},
		},

		{
			name:     "title-only disabled skips fallback",
			stale:    map[string]TautulliEntry{"100": {Title: "Dune", Year: "2020", MediaType: Movie}},
			byTitle:  map[string]PlexEntry{titleKey("dune", Movie): {RatingKey: "200", Year: "2021", Type: Movie}},
			enableTY: true, enableTO: false,
			checks: []check{matchCount(0, 1)},
		},
		{
			name:        "whitespace-only title does not match",
			stale:       map[string]TautulliEntry{"100": {Title: "   ", Year: "2020", MediaType: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("", "2020", Movie): {RatingKey: "200", Year: "2020", Type: Movie}},
			enableTY:    true, enableTO: true,
			checks: []check{matchCount(0, 1)},
		},
		{
			name:     "empty GUID skips GUID matching",
			stale:    map[string]TautulliEntry{"100": {Title: "Test", Year: "2020", MediaType: Movie}},
			byGUID:   map[string]PlexEntry{"": {RatingKey: "200", Year: "2020", Type: Movie}},
			enableTY: true, enableTO: true,
			checks: []check{matchCount(0, 1)},
		},
		{
			name: "multiple stale items mixed results",
			stale: map[string]TautulliEntry{
				"100": {Title: "Movie A", Year: "2020", MediaType: Movie, GUID: "imdb://tt1111111"},
				"200": {Title: "Movie B", Year: "2021", MediaType: Movie, GUID: "imdb://tt2222222"},
				"300": {Title: "Movie C", Year: "2022", MediaType: Movie, GUID: "imdb://tt3333333"},
			},
			byGUID:      map[string]PlexEntry{"imdb://tt1111111": {RatingKey: "101", Year: "2020", Type: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("movie c", "2022", Movie): {RatingKey: "301", Year: "2022", Type: Movie}},
			enableTY:    true, enableTO: true,
			checks: []check{
				matchCount(2, 1),
				unmatchedKey(0, "200"),
			},
		},
		{
			name:        "title with leading trailing whitespace",
			stale:       map[string]TautulliEntry{"100": {Title: "  The Matrix  ", Year: "1999", MediaType: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("the matrix", "1999", Movie): {RatingKey: "200", Year: "1999", Type: Movie}},
			enableTY:    true, enableTO: true,
			checks: []check{matchCount(1, 0), matchKey(0, "200")},
		},
		{
			name:        "guid match same key falls through to title",
			stale:       map[string]TautulliEntry{"100": {Title: "Movie", Year: "2020", MediaType: Movie, GUID: "imdb://tt1111111"}},
			byGUID:      map[string]PlexEntry{"imdb://tt1111111": {RatingKey: "100", Year: "2020", Type: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("movie", "2020", Movie): {RatingKey: "200", Year: "2020", Type: Movie}},
			enableTY:    true, enableTO: true,
			checks: []check{matchCount(1, 0), matchKey(0, "200"), wantMethod(0, methodTitleYear)},
		},
		{
			name:     "title only same key falls through to unmatched",
			stale:    map[string]TautulliEntry{"100": {Title: "Unique Movie", Year: "2020", MediaType: Movie}},
			byTitle:  map[string]PlexEntry{titleKey("unique movie", Movie): {RatingKey: "100", Year: "2020", Type: Movie}},
			enableTY: true, enableTO: true,
			checks: []check{matchCount(0, 1)},
		},
		{
			name:        "title year same key falls through to title only",
			stale:       map[string]TautulliEntry{"100": {Title: "Dune", Year: "2020", MediaType: Movie}},
			byTitleYear: map[string]PlexEntry{titleYearKey("dune", "2020", Movie): {RatingKey: "100", Year: "2020", Type: Movie}},
			byTitle:     map[string]PlexEntry{titleKey("dune", Movie): {RatingKey: "300", Year: "2021", Type: Movie}},
			enableTY:    true, enableTO: true,
			checks: []check{matchCount(1, 0), matchKey(0, "300"), matchMethodPrefix(0, "title only")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched, unmatched := MatchStaleItems(tt.stale, nil, Index{ByGUID: tt.byGUID, ByTitleYear: tt.byTitleYear, ByTitle: tt.byTitle}, Fallbacks{TitleYear: tt.enableTY, TitleOnly: tt.enableTO})
			for _, c := range tt.checks {
				c(t, matched, unmatched)
			}
		})
	}
}

func TestNormalizeGUID_emptyID_returnsEmpty(t *testing.T) {
	// Without this guard, an empty extracted id would return the bare
	// canonical prefix (e.g. "imdb://") instead of "".
	tests := []struct {
		name, guid, want string
	}{
		{"imdb bare prefix only", "imdb://", ""},
		{"imdb prefix with query only", "imdb://?lang=en", ""},
		{"tmdb bare prefix only", "tmdb://", ""},
		{"tmdb prefix with query only", "tmdb://?lang=en", ""},
		{"tvdb bare prefix only", "tvdb://", ""},
		{"plex bare prefix only", "plex://", ""},
		{"mbid bare prefix only", "mbid://", ""},
		{"themoviedb legacy prefix query only", "com.plexapp.agents.themoviedb://?lang=en", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeGUID(tt.guid); got != tt.want {
				t.Errorf("NormalizeGUID(%q) = %q, want %q", tt.guid, got, tt.want)
			}
		})
	}
}

func TestMatchOne_titleYearTakesPriorityOverTitleOnly(t *testing.T) {
	// Both indexes hold a valid match; the chain is ordered by increasing
	// aggressiveness, so title+year must win and title-only must never run.
	item := &TautulliEntry{Title: "Dune", Year: "2020", MediaType: Movie}
	byTitleYear := map[string]PlexEntry{titleYearKey("dune", "2020", Movie): {RatingKey: "200", Year: "2020", Type: Movie}}
	byTitle := map[string]PlexEntry{titleKey("dune", Movie): {RatingKey: "300", Year: "2021", Type: Movie}}
	key, method, matchedYear := matchOne(item, "100", nil, Index{ByTitleYear: byTitleYear, ByTitle: byTitle}, Fallbacks{TitleYear: true, TitleOnly: true})
	if key != "200" {
		t.Errorf("matchOne key = %q, want 200 (title+year must win over title-only)", key)
	}
	if method != methodTitleYear {
		t.Errorf("matchOne method = %q, want %q (strategy 2 precedes strategy 3)", method, methodTitleYear)
	}
	if matchedYear != "" {
		t.Errorf("matchedYear = %q, want empty when title+year wins", matchedYear)
	}
}

func TestMatchOne_titleOnlyCarriesYearTransition(t *testing.T) {
	// Title-only is the riskiest strategy and may land on a different year.
	// Method stays the closed methodTitleOnly value; drift is carried
	// separately in matchedYear for the operator-facing remap log line.
	item := &TautulliEntry{Title: "Dune", Year: "1984", MediaType: Movie}
	byTitle := map[string]PlexEntry{titleKey("dune", Movie): {RatingKey: "300", Year: "2021", Type: Movie}}
	key, method, matchedYear := matchOne(item, "100", nil, Index{ByTitle: byTitle}, Fallbacks{TitleYear: true, TitleOnly: true})
	if key != "300" {
		t.Errorf("matchOne key = %q, want 300", key)
	}
	if method != methodTitleOnly {
		t.Errorf("matchOne method = %q, want %q (closed enum, no formatted years)", method, methodTitleOnly)
	}
	if matchedYear != "2021" {
		t.Errorf("matchedYear = %q, want 2021 (the matched entry's year)", matchedYear)
	}
}

func TestMatchStaleItems_EpisodeGUIDResolution(t *testing.T) {
	// A stale show with resolved episode GUIDs must match via episode-guid,
	// ahead of any title/year index entry.
	stale := map[string]TautulliEntry{
		"100": {
			Title: "Show", Year: "2021", MediaType: Show,
			EpisodeGUIDs: []string{"plex://episode/aaa"},
		},
	}
	resolved := map[string]string{"100": "200"}

	matched, unmatched := MatchStaleItems(stale, resolved, Index{}, Fallbacks{TitleYear: true, TitleOnly: true})
	if len(matched) != 1 || len(unmatched) != 0 {
		t.Fatalf("matched=%d unmatched=%d, want 1/0", len(matched), len(unmatched))
	}
	if matched[0].NewKey != "200" || matched[0].Method != methodEpisodeGUID {
		t.Errorf("got (%q, %q), want (200, episode-guid)", matched[0].NewKey, matched[0].Method)
	}
}

// TestProcessHistoryRow_UnknownMediaTypeSkipped pins the fail-open contract:
// media_type decodes as a plain string, so a row with an unrecognized type
// ("track") is skipped by parseMediaType while movie/episode rows in the same
// batch still process.
func TestProcessHistoryRow_UnknownMediaTypeSkipped(t *testing.T) {
	items := map[string]TautulliEntry{}
	rows := []*HistoryItem{
		{RatingKey: 42, Title: "Movie", Year: 2020, MediaType: "movie", GUID: "imdb://tt1234567"},
		{RatingKey: 99, GrandparentRatingKey: 50, Title: "Ep", GrandparentTitle: "Show", Year: 2021, MediaType: "episode", GUID: "tvdb://271557"},
		{RatingKey: 7, Title: "Song", Year: 2019, MediaType: "track"},
	}
	for _, row := range rows {
		ProcessHistoryRow(row, items)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (movie + show; the track row is skipped)", len(items))
	}
	if items["42"].MediaType != Movie {
		t.Errorf("movie row not processed: items[42] = %+v", items["42"])
	}
	if items["50"].MediaType != Show {
		t.Errorf("episode row not processed into its show: items[50] = %+v", items["50"])
	}
	if _, ok := items["7"]; ok {
		t.Errorf("track row should have been skipped, but items[7] exists: %+v", items["7"])
	}
}

// TestIndexEmpty pins the orchestrator's "cannot match anything" abort signal:
// Empty is true only when all three strategies have zero entries.
func TestIndexEmpty(t *testing.T) {
	entry := map[string]PlexEntry{"k": {RatingKey: "1"}}
	cases := map[string]struct {
		idx  Index
		want bool
	}{
		"zero value is empty":            {Index{}, true},
		"empty non-nil maps are empty":   {Index{ByGUID: map[string]PlexEntry{}, ByTitleYear: map[string]PlexEntry{}, ByTitle: map[string]PlexEntry{}}, true},
		"one GUID entry is not empty":    {Index{ByGUID: entry}, false},
		"one title+year entry not empty": {Index{ByTitleYear: entry}, false},
		"one title entry is not empty":   {Index{ByTitle: entry}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.idx.Empty(); got != tc.want {
				t.Errorf("Index.Empty() = %v, want %v", got, tc.want)
			}
		})
	}
}
