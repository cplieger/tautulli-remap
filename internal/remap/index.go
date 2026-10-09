// Package remap implements the matching and indexing logic that maps stale
// Tautulli rating keys to current Plex rating keys after library reorganizations.
package remap

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cplieger/keyenc"
	"golang.org/x/sync/errgroup"
)

// PlexLibraryFetcher is the narrow interface needed by BuildPlexIndex.
type PlexLibraryFetcher interface {
	LibrarySections(ctx context.Context) ([]Section, error)
	LibraryAll(ctx context.Context, sectionKey string) ([]LibItem, error)
}

// normalizeTitle applies the canonical title normalization used for index
// keys and lookups. Both BuildPlexIndex and matchOne use this function,
// ensuring the invariant that index key == lookup key is enforced by
// construction.
func normalizeTitle(title string) string {
	return strings.ToLower(strings.TrimSpace(title))
}

// titleYearKey builds the composite lookup key for the byTitleYear index from a
// pre-normalized title, the year, and the media type. Folding the media type
// into the key keeps a same-title-and-year Movie/Show pair in distinct slots.
//
// Components are escaped with keyenc rather than concatenated because
// normalizedTitle is arbitrary operator-supplied text and can contain the
// separator ("Dune | Extended Edition"); a collision here merges two Plex
// items' identity in the index that decides what rating key gets written into
// Tautulli's history. No collision is reachable today (year and mediaType
// have constrained alphabets), but that safety is a property of the current
// field list, not of the key — keyenc makes it a property of the key instead.
func titleYearKey(normalizedTitle, year string, mediaType MediaType) string {
	return keyenc.Join(normalizedTitle, year, string(mediaType))
}

// titleKey builds the composite lookup key for the byTitle index. See
// titleYearKey for why the components are escaped rather than concatenated.
func titleKey(normalizedTitle string, mediaType MediaType) string {
	return keyenc.Join(normalizedTitle, string(mediaType))
}

// Index is the Plex library lookup MatchStaleItems consumes: three maps over
// one entry set, each keyed by a different match strategy, grouped in a
// struct so every assignment to a strategy is named rather than positional.
type Index struct {
	// ByGUID is keyed by the normalized global GUID. Deliberately NOT
	// type-keyed (TMDB movies and TV series share the tmdb://<id> namespace),
	// so matchOne guards media type at the lookup.
	ByGUID map[string]PlexEntry
	// ByTitleYear is keyed by titleYearKey (normalized title + year + media
	// type), so a hit is same-type and same-year by construction.
	ByTitleYear map[string]PlexEntry
	// ByTitle is keyed by titleKey (normalized title + media type).
	ByTitle map[string]PlexEntry
}

// Empty reports whether no strategy has any entry — the orchestrator's
// "cannot match anything" abort signal.
func (ix Index) Empty() bool {
	return len(ix.ByGUID) == 0 && len(ix.ByTitleYear) == 0 && len(ix.ByTitle) == 0
}

// plexIndex accumulates the three lookup maps while library sections are
// scanned concurrently; its mutex guards all three. The three ambiguous sets
// record any key for which two DIFFERENT rating keys ever competed; such keys
// are deleted from the lookup maps before BuildPlexIndex returns, so matchOne
// refuses to match on them deterministically instead of last-writer-wins.
type plexIndex struct {
	byGUID             map[string]PlexEntry
	byTitleYear        map[string]PlexEntry
	byTitle            map[string]PlexEntry
	ambiguousGUID      map[string]struct{}
	ambiguousTitleYear map[string]struct{}
	ambiguousTitle     map[string]struct{}
	mu                 sync.Mutex
}

// index snapshots the accumulator's three lookup maps into the exported
// Index shape — the one place the field-to-strategy mapping is written.
func (idx *plexIndex) index() Index {
	return Index{ByGUID: idx.byGUID, ByTitleYear: idx.byTitleYear, ByTitle: idx.byTitle}
}

func newPlexIndex() *plexIndex {
	return &plexIndex{
		byGUID:             map[string]PlexEntry{},
		byTitleYear:        map[string]PlexEntry{},
		byTitle:            map[string]PlexEntry{},
		ambiguousGUID:      map[string]struct{}{},
		ambiguousTitleYear: map[string]struct{}{},
		ambiguousTitle:     map[string]struct{}{},
	}
}

// add indexes a single library item under all of its GUIDs, its
// (title, year, media type), and its (title, media type); byGUID stays keyed
// by the GUID alone. Any collision (two different rating keys competing for
// one key) marks that key ambiguous so it is later removed from the lookup
// maps (refuse-to-match) rather than silently remapped to the wrong key. A
// title+year shadow logs at warn rather than debug because that fallback is
// on by default, making an ambiguous slot there worth surfacing to the
// operator.
func (idx *plexIndex) add(li LibItem, mediaType MediaType) {
	entry := PlexEntry{
		RatingKey: strconv.Itoa(li.RatingKey),
		Year:      strconv.Itoa(li.Year),
		Type:      mediaType,
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	for _, g := range li.GUIDs {
		if prev, ok := idx.byGUID[g]; ok && prev.RatingKey != entry.RatingKey {
			slog.Debug("guid index shadow",
				"guid", g,
				"prev_key", prev.RatingKey, "new_key", entry.RatingKey)
			idx.ambiguousGUID[g] = struct{}{}
		}
		idx.byGUID[g] = entry
	}

	normalizedTitle := normalizeTitle(li.Title.Raw())
	tyKey := titleYearKey(normalizedTitle, entry.Year, mediaType)
	if prev, ok := idx.byTitleYear[tyKey]; ok && prev.RatingKey != entry.RatingKey {
		slog.Warn("title+year index shadow; refusing to match this ambiguous slot",
			"title", li.Title, "year", entry.Year,
			"prev_key", prev.RatingKey, "new_key", entry.RatingKey)
		idx.ambiguousTitleYear[tyKey] = struct{}{}
	}
	idx.byTitleYear[tyKey] = entry

	tKey := titleKey(normalizedTitle, mediaType)
	if prev, ok := idx.byTitle[tKey]; ok && prev.RatingKey != entry.RatingKey {
		slog.Debug("title index shadow",
			"title", li.Title,
			"prev_key", prev.RatingKey, "new_key", entry.RatingKey)
		idx.ambiguousTitle[tKey] = struct{}{}
	}
	idx.byTitle[tKey] = entry
}

// scanSection fetches one library section and indexes every item it returns.
// A cancelled context short-circuits before the fetch (reported as no
// failure). A non-nil return means the section's items could not be fetched.
func (idx *plexIndex) scanSection(ctx context.Context, plex PlexLibraryFetcher, sec Section) error {
	if ctx.Err() != nil {
		return nil
	}
	slog.Info("scanning library", "title", sec.Title)
	mediaType := parseMediaType(sec.Type)
	items, err := plex.LibraryAll(ctx, sec.Key)
	if err != nil {
		if ctx.Err() != nil {
			return nil // cancelled mid-fetch: clean short-circuit, not a section failure
		}
		slog.Error("failed to fetch Plex library section",
			"title", sec.Title, "key", sec.Key, "error", err)
		return err
	}
	for _, li := range items {
		idx.add(li, mediaType)
	}
	slog.Debug("scanned library section", "title", sec.Title, "items", len(items))
	return nil
}

// BuildPlexIndex builds the Index from the Plex library sections, fetching
// them concurrently up to parallelism. failedSections reports how many
// sections could not be fetched (including a total failure to list
// sections); a non-zero count means the index is incomplete.
func BuildPlexIndex(ctx context.Context, plex PlexLibraryFetcher, parallelism int) (idx Index, failedSections int) {
	acc := newPlexIndex()
	sections, err := plex.LibrarySections(ctx)
	if err != nil {
		slog.Error("failed to list Plex library sections", "error", err)
		return acc.index(), 1
	}

	var failed atomic.Int64

	g, gctx := errgroup.WithContext(ctx)
	// errgroup.SetLimit(0) deadlocks; negative => unbounded.
	parallelism = max(parallelism, 1)
	g.SetLimit(parallelism)

	for _, sec := range sections {
		if sec.Type != string(Movie) && sec.Type != string(Show) {
			continue
		}
		g.Go(func() error {
			if scanErr := acc.scanSection(gctx, plex, sec); scanErr != nil {
				failed.Add(1)
			}
			return nil
		})
	}

	_ = g.Wait()

	// Refuse to match on any ambiguous slot: a key for which two different
	// rating keys ever competed is removed from its lookup map, deterministic
	// and order-independent rather than the former last-writer-wins.
	for k := range acc.ambiguousGUID {
		delete(acc.byGUID, k)
	}
	for k := range acc.ambiguousTitleYear {
		delete(acc.byTitleYear, k)
	}
	for k := range acc.ambiguousTitle {
		delete(acc.byTitle, k)
	}

	if refused := len(acc.ambiguousGUID) + len(acc.ambiguousTitleYear) + len(acc.ambiguousTitle); refused > 0 {
		slog.Info("refused to match ambiguous index keys (multiple Plex items shared one lookup key)",
			"guid", len(acc.ambiguousGUID),
			"title_year", len(acc.ambiguousTitleYear),
			"title", len(acc.ambiguousTitle))
	}

	return acc.index(), int(failed.Load())
}
