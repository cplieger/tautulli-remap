package remap

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/runesafe/v2"
)

func FuzzFlexIntUnmarshal(f *testing.F) {
	f.Add([]byte(`42`))
	f.Add([]byte(`"123"`))
	f.Add([]byte(`""`))
	f.Add([]byte(`null`))
	f.Add([]byte(`0.5`))
	f.Add([]byte(`"abc"`))
	f.Add([]byte(`99999999999999999`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var fi flexInt
		if err := fi.UnmarshalJSON(data); err != nil {
			return
		}
		// Round-trip stability for values within float64 safe integer range (±2^53).
		const maxSafe = 1 << 53
		v := int(fi)
		if v > -maxSafe && v < maxSafe {
			out, err := json.Marshal(fi)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			var fi2 flexInt
			if err := fi2.UnmarshalJSON(out); err != nil {
				t.Fatalf("re-unmarshal failed: %v", err)
			}
			if fi != fi2 {
				t.Errorf("round-trip mismatch: %d != %d (marshaled as %s)", fi, fi2, out)
			}
		}
	})
}

// FuzzRatingKey_IsValid pins the security contract of RatingKey.IsValid: the
// app interpolates rating keys straight into Plex/Tautulli URL paths, so any
// key IsValid accepts must be safe there. The invariant is that an accepted
// key is non-empty and contains only ASCII digits, which excludes every
// path-traversal or separator character ('/', '.', '?', whitespace, ...).
func FuzzRatingKey_IsValid(f *testing.F) {
	f.Add("42")
	f.Add("0")
	f.Add("")
	f.Add("-1")
	f.Add("abc")
	f.Add("../../../etc/passwd")
	f.Add("12/34")
	f.Add("9999999999999999999999")
	f.Fuzz(func(t *testing.T, s string) {
		if !RatingKey(s).IsValid() {
			return
		}
		if s == "" {
			t.Errorf("IsValid() accepted the empty string")
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				t.Errorf("IsValid(%q) accepted a key containing non-digit %q", s, c)
			}
		}
	})
}

func FuzzNormalizeGUID(f *testing.F) {
	f.Add("imdb://tt1234567")
	f.Add("com.plexapp.agents.imdb://tt1234567?lang=en")
	f.Add("tmdb://12345")
	f.Add("tvdb://271557")
	f.Add("com.plexapp.agents.thetvdb://271557/3/1?lang=en")
	f.Add("plex://movie/5d776b59ad5437001f79c6f8")
	f.Add("local://616507")
	f.Add("")
	f.Add("custom://something")
	f.Fuzz(func(t *testing.T, guid string) {
		result := NormalizeGUID(guid)
		if result == "" {
			return
		}
		// Idempotency check
		second := NormalizeGUID(result)
		if second != result {
			t.Errorf("not idempotent: NormalizeGUID(%q)=%q, NormalizeGUID(%q)=%q", guid, result, result, second)
		}
		// Canonical prefix check
		canonicalPrefixes := []string{"imdb://", "tmdb://", "tvdb://", "plex://", "mbid://"}
		hasCanonical := false
		for _, cp := range canonicalPrefixes {
			if strings.HasPrefix(result, cp) {
				hasCanonical = true
				break
			}
		}
		if !hasCanonical {
			t.Errorf("NormalizeGUID(%q) = %q does not start with a canonical prefix", guid, result)
		}
	})
}

func FuzzProcessHistoryRow(f *testing.F) {
	f.Add([]byte(`{"rating_key":42,"title":"Test","year":2020,"media_type":"movie","guid":"imdb://tt1234567"}`))
	f.Add([]byte(`{"rating_key":"99","grandparent_rating_key":"50","title":"Ep","grandparent_title":"Show","year":2021,"media_type":"episode","guid":"tvdb://271557"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"rating_key":0,"title":"","year":0,"media_type":"track"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var row HistoryItem
		if err := json.Unmarshal(data, &row); err != nil {
			return
		}
		items := map[string]TautulliEntry{}
		// Must never panic
		ProcessHistoryRow(&row, items)
		// Validate outputs
		canonicalPrefixes := []string{"imdb://", "tmdb://", "tvdb://", "plex://", "mbid://"}
		for key, entry := range items {
			rk, err := strconv.Atoi(key)
			if err != nil || rk <= 0 {
				t.Errorf("invalid rating key %q in output", key)
			}
			// MediaType must be Movie or Show
			if entry.MediaType != Movie && entry.MediaType != Show {
				t.Errorf("unexpected MediaType %q for key %s", entry.MediaType, key)
			}
			// GUID if non-empty must start with canonical prefix
			if entry.GUID != "" {
				hasPrefix := false
				for _, cp := range canonicalPrefixes {
					if strings.HasPrefix(entry.GUID, cp) {
						hasPrefix = true
						break
					}
				}
				if !hasPrefix {
					t.Errorf("entry GUID %q does not start with canonical prefix", entry.GUID)
				}
			}
		}
	})
}

func FuzzMatchOne(f *testing.F) {
	f.Add("Test Movie", "imdb://tt1234567", "movie")
	f.Add("", "", "")
	f.Add("Show Title", "tvdb://12345", "show")
	// A movie history row carrying a show's GUID: matchOne strategy 1 must NOT
	// match it, because byGUID is keyed by the bare GUID (not media type) and
	// matchOne guards on type explicitly; without that guard it would wrongly
	// return the show's key. Seeding the cross-type case makes the type-safety
	// invariant below fail deterministically on every PR if the guard regresses,
	// not only under coverage-guided fuzzing.
	f.Add("Show Title", "tvdb://12345", "movie")
	f.Fuzz(func(t *testing.T, title, guid, mediaType string) {
		mt := parseMediaType(mediaType)
		item := &TautulliEntry{
			Title:     runesafe.Untrusted(title),
			Year:      "2020",
			MediaType: mt,
			GUID:      guid,
		}
		byGUID := map[string]PlexEntry{
			"imdb://tt1234567": {RatingKey: "200", Year: "2020", Type: Movie},
			"tvdb://12345":     {RatingKey: "300", Year: "2021", Type: Show},
		}
		byTitleYear := map[string]PlexEntry{
			titleYearKey("test movie", "2020", Movie): {RatingKey: "400", Year: "2020", Type: Movie},
		}
		byTitle := map[string]PlexEntry{
			titleKey("show title", Show): {RatingKey: "500", Year: "2021", Type: Show},
		}
		validKeys := map[string]bool{"200": true, "300": true, "400": true, "500": true}

		newKey, method, matchedYear := matchOne(item, "100", nil, Index{ByGUID: byGUID, ByTitleYear: byTitleYear, ByTitle: byTitle}, Fallbacks{TitleYear: true, TitleOnly: true})

		// A returned key is never invented: it must be one of the index entries.
		if newKey != "" && !validKeys[newKey] {
			t.Errorf("matchOne returned key %q which is not in the input maps", newKey)
		}
		// Old-key guard: every strategy must refuse to return the stale key itself.
		if newKey == "100" {
			t.Errorf("matchOne returned the old key (self-remap) for %+v", *item)
		}
		// Cross-type safety: byGUID is keyed by the bare GUID (not by media type),
		// so a GUID-method match must have passed matchOne's explicit media-type
		// guard -- a movie must never remap onto a same-GUID show, or vice versa.
		if method == MethodGUID {
			if pe := byGUID[item.GUID]; pe.Type != item.MediaType {
				t.Errorf("GUID match crossed media type: item type %q matched %q-typed entry %q",
					item.MediaType, pe.Type, newKey)
			}
		}
		// matchedYear is set only by title-only matches, and always to the
		// matched entry's own year.
		if matchedYear != "" && method != methodTitleOnly {
			t.Errorf("matchedYear %q set by non-title-only method %q", matchedYear, method)
		}
		if method == methodTitleOnly && matchedYear != byTitle[titleKey(normalizeTitle(item.Title.Raw()), item.MediaType)].Year {
			t.Errorf("matchedYear %q does not carry the matched entry's year", matchedYear)
		}
	})
}
