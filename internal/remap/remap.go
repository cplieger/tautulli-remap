package remap

import (
	"slices"
	"strconv"
	"strings"
)

// maxEpisodeGUIDsPerShow caps how many distinct episode GUIDs are retained per
// show entry. Resolution needs only one to succeed; retaining a few gives
// resilience when the first-tried episode was removed from Plex, while bounding
// memory for shows with thousands of watched episodes.
const maxEpisodeGUIDsPerShow = 5

// NormalizeGUID extracts a canonical ID from a Plex GUID string.
// Returns empty string for unsupported formats.
func NormalizeGUID(guid string) string {
	for _, m := range guidMappings {
		if !strings.Contains(guid, m.Source) {
			continue
		}
		id := extractAfter(guid, m.Source)
		if m.StripPath {
			// Cut returns the whole input as before on no match, so a
			// show-level "thetvdb://<id>" with no season/episode path
			// needs no separate branch.
			id, _, _ = strings.Cut(id, "/")
		}
		if id == "" {
			return ""
		}
		return m.Canonical + id
	}
	return ""
}

// extractAfter returns the part after the prefix, trimming query params.
func extractAfter(s, prefix string) string {
	_, after, found := strings.Cut(s, prefix)
	if !found {
		return ""
	}
	// Cut returns the whole input as before on no match, so a GUID without a
	// query string passes through unchanged.
	after, _, _ = strings.Cut(after, "?")
	return after
}

// Fallbacks selects the title-based match strategies (config-driven; the
// GUID strategies always run). Two adjacent positional bools were the other
// half of the old signature hazard: swapped, they silently enabled the
// looser strategy instead of the stricter one.
type Fallbacks struct {
	// TitleYear enables the title+year fallback.
	TitleYear bool
	// TitleOnly enables the title-only fallback (the loosest strategy; the
	// one place a match's year can differ from the item's own).
	TitleOnly bool
}

// matchOne applies the episode-GUID → GUID → title+year → title-only strategy
// chain to a single stale item, returning the new Plex rating key and the
// method string. Returns ("", "") when no strategy matched.
func matchOne(
	item *TautulliEntry,
	oldKey string,
	resolved map[string]string,
	idx Index,
	fb Fallbacks,
) (newKey string, method matchMethod, matchedYear string) {
	// Strategy 0: episode-GUID resolution (shows only). The orchestrator has
	// already resolved one of this stale show's watched episode GUIDs against
	// Plex; prefer that exact result over any title/year heuristic.
	if rk, ok := resolved[oldKey]; ok && rk != oldKey {
		return rk, methodEpisodeGUID, ""
	}

	// Strategy 1: match by GUID. Index.ByGUID is not type-keyed (TMDB movies
	// and shows share the tmdb://<id> namespace), so guard media type here to
	// avoid remapping a stale Movie onto a same-id Show or vice versa.
	if item.GUID != "" {
		if pe, ok := idx.ByGUID[item.GUID]; ok && pe.RatingKey != oldKey && pe.Type == item.MediaType {
			return pe.RatingKey, MethodGUID, ""
		}
	}

	// Strategies 2-3: title-based fallbacks; their lookup keys already encode
	// the media type, so no separate guard is needed.
	return matchByTitle(item, oldKey, idx, fb)
}

// matchByTitle applies the title+year and (optionally) title-only fallback
// strategies, returning ("", "", "") when neither matches or the item has no
// usable title. matchedYear is the matched entry's release year, set only by
// the title-only strategy — the one place it can differ from the item's own
// year.
func matchByTitle(
	item *TautulliEntry,
	oldKey string,
	idx Index,
	fb Fallbacks,
) (newKey string, method matchMethod, matchedYear string) {
	normalizedTitle := normalizeTitle(item.Title.Raw())
	if normalizedTitle == "" {
		return "", "", ""
	}

	if fb.TitleYear {
		if pe, ok := idx.ByTitleYear[titleYearKey(normalizedTitle, item.Year, item.MediaType)]; ok && pe.RatingKey != oldKey {
			return pe.RatingKey, methodTitleYear, ""
		}
	}

	if fb.TitleOnly {
		if pe, ok := idx.ByTitle[titleKey(normalizedTitle, item.MediaType)]; ok && pe.RatingKey != oldKey {
			return pe.RatingKey, methodTitleOnly, pe.Year
		}
	}

	return "", "", ""
}

// MatchStaleItems matches all stale items against the Plex index, preferring
// the orchestrator-supplied episode-GUID resolutions (resolved: stale show key
// -> current show key) over the index-based heuristics.
func MatchStaleItems(
	stale map[string]TautulliEntry,
	resolved map[string]string,
	idx Index,
	fb Fallbacks,
) ([]MatchResult, []UnmatchResult) {
	var matched []MatchResult
	var unmatched []UnmatchResult

	for oldKey, item := range stale {
		newKey, method, matchedYear := matchOne(&item, oldKey, resolved, idx, fb)
		if newKey != "" {
			matched = append(matched, MatchResult{
				Title: item.Title, Year: item.Year,
				OldKey: oldKey, NewKey: newKey,
				MediaType: item.MediaType, Method: method,
				MatchedYear: matchedYear,
			})
			continue
		}
		unmatched = append(unmatched, UnmatchResult{
			Title: item.Title, Year: item.Year,
			OldKey: oldKey, MediaType: item.MediaType,
		})
	}

	return matched, unmatched
}

// ProcessHistoryRow extracts a TautulliEntry from a single history row and
// inserts or merges it into items (keyed by rating key). For episodes it keys
// by the grandparent (show) rating key and, when the row carries an
// episode-scoped plex:// GUID, retains that GUID on the show entry for later
// resolution. Returns true when an episode GUID was captured on this row.
func ProcessHistoryRow(row *HistoryItem, items map[string]TautulliEntry) bool {
	year := strconv.Itoa(int(row.Year))
	guid := NormalizeGUID(row.GUID)

	switch parseMediaType(row.MediaType) {
	case Movie:
		ratingKey := int(row.RatingKey)
		if ratingKey <= 0 {
			return false
		}
		key := strconv.Itoa(ratingKey)
		if _, ok := items[key]; !ok {
			items[key] = TautulliEntry{
				Title: row.Title, Year: year, MediaType: Movie, GUID: guid,
			}
		}
	case episode:
		grandparentRatingKey := int(row.GrandparentRatingKey)
		if grandparentRatingKey <= 0 {
			return false
		}
		key := strconv.Itoa(grandparentRatingKey)
		// An episode-scoped plex:// GUID cannot identify the show for
		// indexing, but is the durable handle used to resolve the show's
		// current key later. A legacy-agent GUID normalizes to a show-level
		// id and serves as the show GUID directly.
		var episodeGUID, showGUID string
		if strings.HasPrefix(guid, "plex://episode/") {
			episodeGUID = guid
		} else {
			showGUID = guid
		}
		return upsertShow(items, key, row, year, showGUID, episodeGUID)
	}
	return false
}

// upsertShow inserts or merges a Show entry keyed by its grandparent rating
// key. Title and year come from the first row seen; distinct episode GUIDs
// accumulate (bounded by maxEpisodeGUIDsPerShow) so resolution has several
// handles to try if the first-watched episode was later removed from Plex.
// Returns true when an episode GUID was newly captured on this row.
func upsertShow(items map[string]TautulliEntry, key string, row *HistoryItem, year, showGUID, episodeGUID string) bool {
	entry, exists := items[key]
	if !exists {
		title := row.GrandparentTitle
		if title == "" {
			title = row.Title
		}
		entry = TautulliEntry{Title: title, Year: year, MediaType: Show, GUID: showGUID}
	} else if entry.GUID == "" && showGUID != "" {
		entry.GUID = showGUID
	}

	captured := false
	if episodeGUID != "" && len(entry.EpisodeGUIDs) < maxEpisodeGUIDsPerShow &&
		!slices.Contains(entry.EpisodeGUIDs, episodeGUID) {
		entry.EpisodeGUIDs = append(entry.EpisodeGUIDs, episodeGUID)
		captured = true
	}

	items[key] = entry
	return captured
}
