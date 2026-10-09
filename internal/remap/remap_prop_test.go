package remap

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/runesafe/v2"
	"pgregory.net/rapid"
)

func TestNormalizeGUID_idempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prefix := rapid.SampledFrom([]string{
			"imdb://", "tmdb://", "tvdb://", "plex://", "mbid://",
			"themoviedb://", "thetvdb://",
			"local://", "com.plexapp.agents.none://", "",
		}).Draw(t, "prefix")
		suffix := rapid.StringMatching(`[a-zA-Z0-9/_\-]{0,40}`).Draw(t, "suffix")
		guid := prefix + suffix
		first := NormalizeGUID(guid)
		second := NormalizeGUID(first)
		if first != second {
			t.Errorf("not idempotent: NormalizeGUID(%q)=%q, NormalizeGUID(%q)=%q", guid, first, first, second)
		}
	})
}

func TestNormalizeGUID_never_panics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		guid := rapid.String().Draw(t, "guid")
		_ = NormalizeGUID(guid)
	})
}

func TestNormalizeGUID_output_uses_canonical_prefix(t *testing.T) {
	canonicalPrefixes := []string{"imdb://", "tmdb://", "tvdb://", "plex://", "mbid://"}
	rapid.Check(t, func(t *rapid.T) {
		prefix := rapid.SampledFrom([]string{
			"imdb://", "tmdb://", "tvdb://", "plex://", "mbid://",
			"themoviedb://", "thetvdb://",
		}).Draw(t, "prefix")
		id := rapid.StringMatching(`[a-zA-Z0-9]{1,20}`).Draw(t, "id")
		guid := prefix + id
		result := NormalizeGUID(guid)
		if result == "" {
			return
		}
		hasCanonical := false
		for _, cp := range canonicalPrefixes {
			if strings.HasPrefix(result, cp) {
				hasCanonical = true
				break
			}
		}
		if !hasCanonical {
			t.Errorf("NormalizeGUID(%q) = %q, no canonical prefix", guid, result)
		}
	})
}

func TestExtractAfter_never_panics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		prefix := rapid.String().Draw(t, "prefix")
		_ = extractAfter(s, prefix)
	})
}

func TestExtractAfter_strips_query_params(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prefix := rapid.SampledFrom([]string{"imdb://", "tmdb://", "tvdb://"}).Draw(t, "prefix")
		id := rapid.StringMatching(`[a-zA-Z0-9]{1,20}`).Draw(t, "id")
		query := rapid.StringMatching(`[a-z]{1,10}=[a-z]{1,10}`).Draw(t, "query")
		input := prefix + id + "?" + query
		result := extractAfter(input, prefix)
		if strings.Contains(result, "?") {
			t.Errorf("extractAfter(%q, %q) = %q, still contains query params", input, prefix, result)
		}
		if result != id {
			t.Errorf("extractAfter(%q, %q) = %q, want %q", input, prefix, result, id)
		}
	})
}

func TestMatchStaleItems_partition_property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 10).Draw(t, "n")
		stale := map[string]TautulliEntry{}
		byGUID := map[string]PlexEntry{}
		byTitleYear := map[string]PlexEntry{}
		byTitle := map[string]PlexEntry{}

		for i := range n {
			key := strconv.Itoa(100 + i)
			title := runesafe.Untrusted(rapid.StringMatching(`[A-Za-z ]{1,20}`).Draw(t, fmt.Sprintf("title_%d", i)))
			year := strconv.Itoa(rapid.IntRange(1990, 2025).Draw(t, fmt.Sprintf("year_%d", i)))
			mediaType := MediaType(rapid.SampledFrom([]string{"movie", "show"}).Draw(t, fmt.Sprintf("type_%d", i)))
			guid := ""
			if rapid.Bool().Draw(t, fmt.Sprintf("has_guid_%d", i)) {
				guid = "imdb://tt" + strconv.Itoa(rapid.IntRange(1000000, 9999999).Draw(t, fmt.Sprintf("guid_%d", i)))
			}
			stale[key] = TautulliEntry{Title: title, Year: year, MediaType: mediaType, GUID: guid}

			if guid != "" && rapid.Bool().Draw(t, fmt.Sprintf("in_guid_map_%d", i)) {
				newKey := strconv.Itoa(200 + i)
				byGUID[guid] = PlexEntry{RatingKey: newKey, Year: year, Type: mediaType}
			}
			t2 := strings.ToLower(strings.TrimSpace(title.Raw()))
			if t2 != "" && rapid.Bool().Draw(t, fmt.Sprintf("in_ty_map_%d", i)) {
				newKey := strconv.Itoa(300 + i)
				byTitleYear[titleYearKey(t2, year, mediaType)] = PlexEntry{RatingKey: newKey, Year: year, Type: mediaType}
			}
			if t2 != "" && rapid.Bool().Draw(t, fmt.Sprintf("in_t_map_%d", i)) {
				newKey := strconv.Itoa(400 + i)
				byTitle[titleKey(t2, mediaType)] = PlexEntry{RatingKey: newKey, Year: year, Type: mediaType}
			}
		}

		matched, unmatched := MatchStaleItems(stale, nil, Index{ByGUID: byGUID, ByTitleYear: byTitleYear, ByTitle: byTitle}, Fallbacks{TitleYear: true, TitleOnly: true})

		seen := map[string]bool{}
		for _, m := range matched {
			if seen[m.OldKey] {
				t.Errorf("duplicate matched key %s", m.OldKey)
			}
			seen[m.OldKey] = true
		}
		for _, u := range unmatched {
			if seen[u.OldKey] {
				t.Errorf("key %s in both matched and unmatched", u.OldKey)
			}
			seen[u.OldKey] = true
		}
		if len(seen) != len(stale) {
			t.Errorf("partition has %d items, stale has %d", len(seen), len(stale))
		}
		for _, m := range matched {
			if m.OldKey == m.NewKey {
				t.Errorf("self-remap OldKey == NewKey == %s", m.OldKey)
			}
		}
		for _, m := range matched {
			if m.Method == MethodGUID {
				entry := stale[m.OldKey]
				if entry.GUID == "" {
					t.Errorf("GUID method for empty GUID, key %s", m.OldKey)
				}
			}
		}
	})
}

func TestProcessHistoryRow_never_removes_entries(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		items := map[string]TautulliEntry{}
		nExisting := rapid.IntRange(0, 5).Draw(t, "n_existing")
		for i := range nExisting {
			key := strconv.Itoa(i + 1)
			items[key] = TautulliEntry{Title: runesafe.Untrusted(fmt.Sprintf("Existing %d", i)), Year: "2020", MediaType: Movie}
		}
		beforeLen := len(items)
		beforeKeys := map[string]runesafe.Untrusted{}
		for k, v := range items {
			beforeKeys[k] = v.Title
		}

		mediaType := rapid.SampledFrom([]string{"movie", "episode", "track", ""}).Draw(t, "media_type")
		row := &HistoryItem{
			RatingKey:            flexInt(rapid.IntRange(-1, 10).Draw(t, "rk")),
			GrandparentRatingKey: flexInt(rapid.IntRange(-1, 10).Draw(t, "grk")),
			Title:                runesafe.Untrusted(rapid.StringMatching(`[A-Za-z ]{0,15}`).Draw(t, "title")),
			GrandparentTitle:     runesafe.Untrusted(rapid.StringMatching(`[A-Za-z ]{0,15}`).Draw(t, "gp_title")),
			Year:                 flexInt(rapid.IntRange(2000, 2025).Draw(t, "year")),
			MediaType:            mediaType,
			GUID:                 rapid.SampledFrom([]string{"", "imdb://tt1234567", "plex://episode/abc", "local://123"}).Draw(t, "guid"),
		}

		ProcessHistoryRow(row, items)

		if len(items) < beforeLen {
			t.Errorf("map shrank from %d to %d", beforeLen, len(items))
		}
		for k, title := range beforeKeys {
			if items[k].Title != title {
				t.Errorf("existing entry %q changed from %q to %q", k, title, items[k].Title)
			}
		}
	})
}

func TestNormalizeTitle_idempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		once := normalizeTitle(s)
		twice := normalizeTitle(once)
		if once != twice {
			t.Errorf("normalizeTitle not idempotent: normalizeTitle(%q)=%q, normalizeTitle(%q)=%q", s, once, once, twice)
		}
	})
}
