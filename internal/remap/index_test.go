package remap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/keyenc"
)

// collidingFetcher returns its sections and per-section items verbatim, so a test can place two
// distinct items in ONE section to exercise the index shadow/last-write-wins path deterministically.
type collidingFetcher struct {
	sections []Section
	items    map[string][]LibItem
}

func (f *collidingFetcher) LibrarySections(_ context.Context) ([]Section, error) {
	return f.sections, nil
}

func (f *collidingFetcher) LibraryAll(_ context.Context, sectionKey string) ([]LibItem, error) {
	return f.items[sectionKey], nil
}

// captureLogs installs a Debug-level text handler over a buffer as the default logger for the
// test and returns that buffer. Tests using it must NOT be parallel (the
// default logger is process-global).
//
// slog.SetDefault also points the log package at the installed handler and
// zeroes its flags, and skips that redirect for slog's own default handler, so
// restoring slog alone leaves log writing into a dead buffer. slog goes back
// first: reinstalling a non-default prev re-runs the redirect.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// TestCaptureLogs_restoresLogPackage pins all three globals slog.SetDefault
// mutates. A sentinel writer and a non-zero flag set are installed first so
// neither assertion can hold by accident: the incomplete restore leaves the
// log package aimed at the capture buffer with its flags zeroed, which
// silences every later slog call in the package.
func TestCaptureLogs_restoresLogPackage(t *testing.T) {
	prevWriter, prevFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	var sentinel bytes.Buffer
	log.SetOutput(&sentinel)
	log.SetFlags(log.Lshortfile)

	t.Run("capture", func(t *testing.T) {
		buf := captureLogs(t)
		slog.Info("captured")
		if buf.Len() == 0 {
			t.Fatal("captureLogs() captured nothing; the swap itself is broken, so the restore assertions below would be vacuous")
		}
	})

	if got := log.Writer(); got != io.Writer(&sentinel) {
		t.Errorf("after captureLogs cleanup, log.Writer() = %T, want the sentinel *bytes.Buffer", got)
	}
	if got := log.Flags(); got != log.Lshortfile {
		t.Errorf("after captureLogs cleanup, log.Flags() = %d, want %d", got, log.Lshortfile)
	}
}

func TestBuildPlexIndex_TitleYearCollisionRefusesToMatch(t *testing.T) {
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		items: map[string][]LibItem{
			"1": {
				{RatingKey: 1, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0001"}},
				{RatingKey: 2, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0002"}},
			},
		},
	}

	buf := captureLogs(t)

	idx, failed := BuildPlexIndex(t.Context(), fetcher, 1)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0 (fetcher never errors)", failed)
	}

	// A collision on the title+year and title keys means the slot is ambiguous,
	// so it is removed entirely (refuse to match) rather than resolved by the
	// last writer.
	if _, ok := idx.ByTitleYear[titleYearKey("heat", "1995", Movie)]; ok {
		t.Errorf("idx.ByTitleYear[(heat, 1995, movie)] = %q, want ABSENT (ambiguous slot must be removed)", idx.ByTitleYear[titleYearKey("heat", "1995", Movie)].RatingKey)
	}
	if _, ok := idx.ByTitle[titleKey("heat", Movie)]; ok {
		t.Errorf("idx.ByTitle[(heat, movie)] = %q, want ABSENT (ambiguous slot must be removed)", idx.ByTitle[titleKey("heat", Movie)].RatingKey)
	}
	// The two GUIDs are distinct, so neither collided; both are kept.
	if got := idx.ByGUID["imdb://tt0001"].RatingKey; got != "1" {
		t.Errorf("idx.ByGUID[tt0001].RatingKey = %q, want %q (distinct GUIDs both kept)", got, "1")
	}
	if got := idx.ByGUID["imdb://tt0002"].RatingKey; got != "2" {
		t.Errorf("idx.ByGUID[tt0002].RatingKey = %q, want %q (distinct GUIDs both kept)", got, "2")
	}
	logged := buf.String()
	if !strings.Contains(logged, "title+year index shadow") {
		t.Errorf("expected title+year shadow warn log on differing-key collision, got:\n%s", logged)
	}
	if !strings.Contains(logged, "title index shadow") {
		t.Errorf("expected title shadow debug log on differing-key collision, got:\n%s", logged)
	}
}

func TestBuildPlexIndex_SameKeyReindexedNoShadow(t *testing.T) {
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		items: map[string][]LibItem{
			"1": {
				{RatingKey: 7, Title: "Dune", Year: 2021, GUIDs: []string{"imdb://tt1"}},
				{RatingKey: 7, Title: "Dune", Year: 2021, GUIDs: []string{"imdb://tt1"}},
			},
		},
	}

	buf := captureLogs(t)

	idx, _ := BuildPlexIndex(t.Context(), fetcher, 1)

	if got := idx.ByTitleYear[titleYearKey("dune", "2021", Movie)].RatingKey; got != "7" {
		t.Errorf("idx.ByTitleYear[(dune, 2021, movie)].RatingKey = %q, want %q", got, "7")
	}
	if strings.Contains(buf.String(), "shadow") {
		t.Errorf("did not expect a shadow log for an idempotent same-key re-add, got:\n%s", buf.String())
	}
}

// failingFetcher returns its sections verbatim but errors when LibraryAll is
// called for failSection, modeling a partial Plex outage where one section is
// briefly unavailable while others load.
type failingFetcher struct {
	sections    []Section
	items       map[string][]LibItem
	failSection string
}

func (f *failingFetcher) LibrarySections(_ context.Context) ([]Section, error) {
	return f.sections, nil
}

func (f *failingFetcher) LibraryAll(_ context.Context, sectionKey string) ([]LibItem, error) {
	if sectionKey == f.failSection {
		return nil, errors.New("section fetch failed")
	}
	return f.items[sectionKey], nil
}

// listErrorFetcher fails to even list the library sections.
type listErrorFetcher struct{}

func (listErrorFetcher) LibrarySections(_ context.Context) ([]Section, error) {
	return nil, errors.New("cannot list sections")
}

func (listErrorFetcher) LibraryAll(_ context.Context, _ string) ([]LibItem, error) {
	return nil, nil
}

func TestBuildPlexIndex_ReportsFailedSections(t *testing.T) {
	fetcher := &failingFetcher{
		sections: []Section{
			{Key: "1", Title: "Movies", Type: "movie"},
			{Key: "2", Title: "Movies 4K", Type: "movie"},
		},
		items: map[string][]LibItem{
			"1": {{RatingKey: 1, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0001"}}},
		},
		failSection: "2",
	}

	idx, failed := BuildPlexIndex(t.Context(), fetcher, 1)

	if failed != 1 {
		t.Errorf("failedSections = %d, want 1 (one section errored)", failed)
	}
	if _, ok := idx.ByTitleYear[titleYearKey("heat", "1995", Movie)]; !ok {
		t.Error("expected the section that loaded to still be indexed")
	}
}

func TestBuildPlexIndex_SectionListError(t *testing.T) {
	idx, failed := BuildPlexIndex(t.Context(), listErrorFetcher{}, 1)

	if failed == 0 {
		t.Error("expected non-zero failedSections when the section list fetch fails")
	}
	if len(idx.ByGUID) != 0 || len(idx.ByTitleYear) != 0 || len(idx.ByTitle) != 0 {
		t.Error("expected empty index when the section list fetch fails")
	}
}

func TestBuildPlexIndex_GUIDCollisionRefusesToMatch(t *testing.T) {
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		items: map[string][]LibItem{
			"1": {
				{RatingKey: 1, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0113277"}},
				{RatingKey: 2, Title: "Heat 4K", Year: 1995, GUIDs: []string{"imdb://tt0113277"}},
			},
		},
	}

	buf := captureLogs(t)

	idx, failed := BuildPlexIndex(t.Context(), fetcher, 1)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0 (fetcher never errors)", failed)
	}
	// The shared GUID collided on two different rating keys, so the slot is
	// ambiguous and removed (refuse to match) rather than kept as last-writer.
	if _, ok := idx.ByGUID["imdb://tt0113277"]; ok {
		t.Errorf("idx.ByGUID[imdb://tt0113277] = %q, want ABSENT (ambiguous GUID slot must be removed)", idx.ByGUID["imdb://tt0113277"].RatingKey)
	}
	logged := buf.String()
	if !strings.Contains(logged, "guid index shadow") {
		t.Errorf("expected guid index shadow debug log on same-GUID differing-key collision, got:\n%s", logged)
	}
	if strings.Contains(logged, "title index shadow") || strings.Contains(logged, "title+year index shadow") {
		t.Errorf("did not expect a title shadow when only the GUID collides, got:\n%s", logged)
	}
}

func TestBuildPlexIndex_ParallelismBelowOneStillIndexes(t *testing.T) {
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		items: map[string][]LibItem{
			"1": {{RatingKey: 1, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0113277"}}},
		},
	}

	idx, failed := BuildPlexIndex(t.Context(), fetcher, 0)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0", failed)
	}
	if got := idx.ByGUID["imdb://tt0113277"].RatingKey; got != "1" {
		t.Errorf("idx.ByGUID[imdb://tt0113277].RatingKey = %q, want %q (indexed despite parallelism=0)", got, "1")
	}
	if got := idx.ByTitleYear[titleYearKey("heat", "1995", Movie)].RatingKey; got != "1" {
		t.Errorf("idx.ByTitleYear[(heat, 1995, movie)].RatingKey = %q, want %q", got, "1")
	}
}

// cancelOnFetchFetcher cancels the run context during the library fetch and then
// returns an error, modeling a shutdown that lands mid-scan: the fetch fails, but
// because the cause is cancellation (not Plex), the section must not count as failed.
type cancelOnFetchFetcher struct {
	sections []Section
	cancel   context.CancelFunc
}

func (f *cancelOnFetchFetcher) LibrarySections(_ context.Context) ([]Section, error) {
	return f.sections, nil
}

func (f *cancelOnFetchFetcher) LibraryAll(_ context.Context, _ string) ([]LibItem, error) {
	f.cancel()
	return nil, errors.New("fetch aborted")
}

func TestBuildPlexIndex_CancelledBeforeScanIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		items: map[string][]LibItem{
			"1": {{RatingKey: 1, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0113277"}}},
		},
	}
	idx, failed := BuildPlexIndex(ctx, fetcher, 1)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0 (a context cancelled before the scan is shutdown, not a Plex failure)", failed)
	}
	if len(idx.ByGUID) != 0 {
		t.Errorf("idx.ByGUID has %d entries, want 0 (a cancelled scan must index nothing)", len(idx.ByGUID))
	}
}

func TestBuildPlexIndex_CancelledMidFetchIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	fetcher := &cancelOnFetchFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		cancel:   cancel,
	}
	_, failed := BuildPlexIndex(ctx, fetcher, 1)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0 (a fetch error caused by context cancellation is shutdown, not a Plex failure)", failed)
	}
}

func TestBuildPlexIndex_SkipsNonMovieShowSections(t *testing.T) {
	fetcher := &collidingFetcher{
		sections: []Section{
			{Key: "1", Title: "Movies", Type: "movie"},
			{Key: "2", Title: "Music", Type: "artist"},
		},
		items: map[string][]LibItem{
			"1": {{RatingKey: 1, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0113277"}}},
			"2": {{RatingKey: 9, Title: "Some Album", Year: 2001, GUIDs: []string{"mbid://abc"}}},
		},
	}
	idx, failed := BuildPlexIndex(t.Context(), fetcher, 1)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0", failed)
	}
	if _, ok := idx.ByGUID["imdb://tt0113277"]; !ok {
		t.Error("expected the movie section to be indexed")
	}
	if _, ok := idx.ByGUID["mbid://abc"]; ok {
		t.Error("non-movie/show (artist) section must be skipped, but its GUID was indexed")
	}
	// If the skip guard regressed, the artist item would be indexed with an
	// empty media type (ParseMediaType("artist") == ""), i.e. under the (some album, "") and
	// (some album, 2001, "") slots. Assert those exact slots stay absent.
	if _, ok := idx.ByTitle[titleKey("some album", MediaType(""))]; ok {
		t.Error("non-movie/show (artist) section must be skipped, but its title was indexed")
	}
	if _, ok := idx.ByTitleYear[titleYearKey("some album", "2001", MediaType(""))]; ok {
		t.Error("non-movie/show (artist) section must be skipped, but its title+year was indexed")
	}
}

func TestBuildPlexIndex_TitleOnlyCollisionKeepsTitleYearMatchable(t *testing.T) {
	// Same title, DIFFERENT years: the title-only key "dune" collides (two
	// distinct rating keys) and is refused, but the two title+year slots do
	// NOT collide and must stay matchable. Pins that the three ambiguous sets
	// are independent -- a collision in the less-specific title index must not
	// poison the more-specific title+year index.
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		items: map[string][]LibItem{
			"1": {
				{RatingKey: 1, Title: "Dune", Year: 2021, GUIDs: []string{"imdb://tt1"}},
				{RatingKey: 2, Title: "Dune", Year: 1984, GUIDs: []string{"imdb://tt2"}},
			},
		},
	}

	idx, failed := BuildPlexIndex(t.Context(), fetcher, 1)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0 (fetcher never errors)", failed)
	}
	if _, ok := idx.ByTitle[titleKey("dune", Movie)]; ok {
		t.Errorf("idx.ByTitle[(dune, movie)] = %q, want ABSENT (title-only slot is ambiguous)", idx.ByTitle[titleKey("dune", Movie)].RatingKey)
	}
	if got := idx.ByTitleYear[titleYearKey("dune", "2021", Movie)].RatingKey; got != "1" {
		t.Errorf("idx.ByTitleYear[(dune, 2021, movie)].RatingKey = %q, want %q (non-colliding title+year slot must stay matchable)", got, "1")
	}
	if got := idx.ByTitleYear[titleYearKey("dune", "1984", Movie)].RatingKey; got != "2" {
		t.Errorf("idx.ByTitleYear[(dune, 1984, movie)].RatingKey = %q, want %q (non-colliding title+year slot must stay matchable)", got, "2")
	}
	if got := idx.ByGUID["imdb://tt1"].RatingKey; got != "1" {
		t.Errorf("idx.ByGUID[imdb://tt1].RatingKey = %q, want %q", got, "1")
	}
	if got := idx.ByGUID["imdb://tt2"].RatingKey; got != "2" {
		t.Errorf("idx.ByGUID[imdb://tt2].RatingKey = %q, want %q", got, "2")
	}
}

func TestMatch_CrossTypeSameTitleYear_RecoversMovieMatch(t *testing.T) {
	// Plex holds a Movie "Dune" 2021 (key 10) AND a Show "Dune" 2021 (key 20).
	// Before the index keys folded in media type, both occupied the title+year
	// key (dune, 2021) and collided, so the slot was pruned (refuse-to-match) and
	// a stale Movie "Dune" 2021 whose GUID no longer resolves was MISSED. With
	// the media type in the key the two occupy distinct slots, so the stale Movie
	// now matches the Movie (key 10) and never the Show.
	fetcher := &collidingFetcher{
		sections: []Section{
			{Key: "1", Title: "Movies", Type: "movie"},
			{Key: "2", Title: "Shows", Type: "show"},
		},
		items: map[string][]LibItem{
			"1": {{RatingKey: 10, Title: "Dune", Year: 2021, GUIDs: []string{"imdb://tt-movie"}}},
			"2": {{RatingKey: 20, Title: "Dune", Year: 2021, GUIDs: []string{"tvdb://show"}}},
		},
	}

	idx, failed := BuildPlexIndex(t.Context(), fetcher, 1)
	if failed != 0 {
		t.Fatalf("failedSections = %d, want 0 (fetcher never errors)", failed)
	}
	// Both type-specific title+year slots survive: distinct keys, no collision.
	if got := idx.ByTitleYear[titleYearKey("dune", "2021", Movie)].RatingKey; got != "10" {
		t.Errorf("idx.ByTitleYear[(dune, 2021, movie)] = %q, want 10 (movie slot must survive cross-type coexistence)", got)
	}
	if got := idx.ByTitleYear[titleYearKey("dune", "2021", Show)].RatingKey; got != "20" {
		t.Errorf("idx.ByTitleYear[(dune, 2021, show)] = %q, want 20 (show slot must survive cross-type coexistence)", got)
	}

	// Stale Movie whose GUID no longer resolves (absent from idx.ByGUID) falls back
	// to title+year and must land on the Movie, not the Show.
	stale := map[string]TautulliEntry{
		"99": {RatingKey: "99", Title: "Dune", Year: "2021", MediaType: Movie, GUID: "imdb://stale-gone"},
	}
	matched, unmatched := MatchStaleItems(stale, nil, idx, Fallbacks{TitleYear: true, TitleOnly: false})
	if len(matched) != 1 || len(unmatched) != 0 {
		t.Fatalf("matched=%d unmatched=%d, want 1/0 (recovered same-type match)", len(matched), len(unmatched))
	}
	if matched[0].NewKey != "10" {
		t.Errorf("NewKey = %q, want 10 (the Movie, never the Show key 20)", matched[0].NewKey)
	}
	if matched[0].Method != MethodTitleYear {
		t.Errorf("Method = %q, want %q", matched[0].Method, MethodTitleYear)
	}
}

func TestMatch_SameTypeTitleYearTwin_StillRefusesToMatch(t *testing.T) {
	// Companion to the cross-type recovery test: two Movies "Dune" 2021 (keys 10
	// and 11) genuinely collide on the type-keyed slot (dune, 2021, movie), so it
	// stays pruned and a stale Movie "Dune" 2021 remains unmatched. Folding the
	// media type into the key must not weaken same-type ambiguity detection.
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		items: map[string][]LibItem{
			"1": {
				{RatingKey: 10, Title: "Dune", Year: 2021, GUIDs: []string{"imdb://tt-a"}},
				{RatingKey: 11, Title: "Dune", Year: 2021, GUIDs: []string{"imdb://tt-b"}},
			},
		},
	}

	idx, failed := BuildPlexIndex(t.Context(), fetcher, 1)
	if failed != 0 {
		t.Fatalf("failedSections = %d, want 0 (fetcher never errors)", failed)
	}
	if _, ok := idx.ByTitleYear[titleYearKey("dune", "2021", Movie)]; ok {
		t.Errorf("idx.ByTitleYear[(dune, 2021, movie)] present, want ABSENT (same-type twins must refuse-to-match)")
	}

	stale := map[string]TautulliEntry{
		"99": {RatingKey: "99", Title: "Dune", Year: "2021", MediaType: Movie, GUID: "imdb://stale-gone"},
	}
	matched, unmatched := MatchStaleItems(stale, nil, idx, Fallbacks{TitleYear: true, TitleOnly: false})
	if len(matched) != 0 || len(unmatched) != 1 {
		t.Fatalf("matched=%d unmatched=%d, want 0/1 (ambiguous slot pruned)", len(matched), len(unmatched))
	}
}

func TestBuildPlexIndex_CrossSectionGUIDCollisionRefusesToMatch(t *testing.T) {
	// Two DIFFERENT movie sections each carry the same GUID with a different
	// rating key, scanned CONCURRENTLY (parallelism 2). The ambiguous-set
	// design must refuse to match regardless of which goroutine's add lands
	// last -- the order-independence the package claims over the former
	// last-writer-wins behavior. Every other BuildPlexIndex test runs at
	// parallelism 0 or 1, so this is the only exercise of the concurrent
	// fan-out path (and validates the add mutex under -race). Distinct titles
	// keep the collision isolated to idx.ByGUID.
	fetcher := &collidingFetcher{
		sections: []Section{
			{Key: "1", Title: "Movies", Type: "movie"},
			{Key: "2", Title: "Movies 4K", Type: "movie"},
		},
		items: map[string][]LibItem{
			"1": {{RatingKey: 1, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0113277"}}},
			"2": {{RatingKey: 2, Title: "Heat 4K", Year: 1995, GUIDs: []string{"imdb://tt0113277"}}},
		},
	}
	idx, failed := BuildPlexIndex(t.Context(), fetcher, 2)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0 (fetcher never errors)", failed)
	}
	if _, ok := idx.ByGUID["imdb://tt0113277"]; ok {
		t.Errorf("idx.ByGUID[imdb://tt0113277] = %q, want ABSENT (cross-section GUID collision must refuse-to-match under concurrency)", idx.ByGUID["imdb://tt0113277"].RatingKey)
	}
}

func TestBuildPlexIndex_AnnouncesRefusalSummaryWhenKeysAreAmbiguous(t *testing.T) {
	// Two same-type items share a title and year (with distinct GUIDs), so the
	// title+year and title slots are pruned as ambiguous. BuildPlexIndex must
	// emit the operator-facing summary naming that it refused ambiguous keys,
	// so an operator can see why those items will not be remapped. The summary
	// is distinct from the per-collision shadow lines and is the only signal
	// that surfaces the aggregate refusal counts.
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		items: map[string][]LibItem{
			"1": {
				{RatingKey: 1, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0001"}},
				{RatingKey: 2, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0002"}},
			},
		},
	}

	buf := captureLogs(t)

	BuildPlexIndex(t.Context(), fetcher, 1)

	if !strings.Contains(buf.String(), "refused to match ambiguous index keys") {
		t.Errorf("expected the ambiguity-refusal summary to be logged when keys are pruned, got:\n%s", buf.String())
	}
}

func TestBuildPlexIndex_OmitsRefusalSummaryWhenNoKeysAreAmbiguous(t *testing.T) {
	// A library with no colliding keys prunes nothing, so the operator-facing
	// ambiguity-refusal summary must stay silent: the operator is told about
	// refused keys only when there actually are some, not on every clean run.
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: "movie"}},
		items: map[string][]LibItem{
			"1": {
				{RatingKey: 1, Title: "Heat", Year: 1995, GUIDs: []string{"imdb://tt0001"}},
				{RatingKey: 2, Title: "Speed", Year: 1994, GUIDs: []string{"imdb://tt0002"}},
			},
		},
	}

	buf := captureLogs(t)

	BuildPlexIndex(t.Context(), fetcher, 1)

	if strings.Contains(buf.String(), "refused to match ambiguous index keys") {
		t.Errorf("did not expect the ambiguity-refusal summary for a collision-free index, got:\n%s", buf.String())
	}
}

// legacyTitleYearKey and legacyTitleKey are the exact pre-keyenc expressions
// the two index-key helpers used: '|' concatenation with no escaping. They are
// the oracles for the tests below, which pin what the keyenc adoption did and
// did not change.
func legacyTitleYearKey(normalizedTitle, year string, mediaType MediaType) string {
	return normalizedTitle + "|" + year + "|" + string(mediaType)
}

func legacyTitleKey(normalizedTitle string, mediaType MediaType) string {
	return normalizedTitle + "|" + string(mediaType)
}

// TestIndexKeysOrdinaryInputIsPlainSeparatorJoin pins the shape of both index
// keys for ordinary input: keyenc adds no escaping, no hashing and no other
// decoration to components carrying neither ':' nor '\', so each key is exactly
// its components joined by ':'. Equivalently, it is the legacy key with '|'
// swapped for ':' and nothing else altered — the one intended byte change of
// the adoption, free because these indexes are rebuilt in memory by every
// BuildPlexIndex call and never persisted or compared across runs.
//
// "Ordinary" is narrower here than at a site keyed on numeric IDs: because the
// new separator is ':', a title that contains a colon is NOT ordinary input, and
// film titles contain colons constantly ("Dune: Part Two"). Those keys gain a
// real escape — see TestIndexKeysColonBearingTitleIsEscapedAndFaithful, which
// pins that half.
func TestIndexKeysOrdinaryInputIsPlainSeparatorJoin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		title     string
		year      string
		mediaType MediaType
	}{
		{name: "movie", title: "the matrix", year: "1999", mediaType: Movie},
		{name: "show", title: "heat", year: "1995", mediaType: Show},
		{name: "episode", title: "pilot", year: "2008", mediaType: Episode},
		{name: "unknown media type", title: "some album", year: "2001", mediaType: MediaType("")},
		{name: "empty title", title: "", year: "2020", mediaType: Movie},
		{name: "title with spaces and punctuation", title: "dune, part two", year: "2024", mediaType: Movie},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wantTY := tt.title + ":" + tt.year + ":" + string(tt.mediaType)
			if got := titleYearKey(tt.title, tt.year, tt.mediaType); got != wantTY {
				t.Errorf("titleYearKey(%q, %q, %q) = %q, want %q", tt.title, tt.year, tt.mediaType, got, wantTY)
			}
			wantT := tt.title + ":" + string(tt.mediaType)
			if got := titleKey(tt.title, tt.mediaType); got != wantT {
				t.Errorf("titleKey(%q, %q) = %q, want %q", tt.title, tt.mediaType, got, wantT)
			}
			// Nothing but the separator changed relative to the old encoder.
			if got, want := titleYearKey(tt.title, tt.year, tt.mediaType),
				strings.ReplaceAll(legacyTitleYearKey(tt.title, tt.year, tt.mediaType), "|", ":"); got != want {
				t.Errorf("titleYearKey(%q, %q, %q) = %q, want the legacy key with '|' -> ':' = %q",
					tt.title, tt.year, tt.mediaType, got, want)
			}
			if got, want := titleKey(tt.title, tt.mediaType),
				strings.ReplaceAll(legacyTitleKey(tt.title, tt.mediaType), "|", ":"); got != want {
				t.Errorf("titleKey(%q, %q) = %q, want the legacy key with '|' -> ':' = %q",
					tt.title, tt.mediaType, got, want)
			}
		})
	}
}

// TestIndexKeysColonBearingTitleIsEscapedAndFaithful covers the input class the
// previous test excludes, and it is the common case rather than an exotic one:
// a film title with a colon. Under the old '|' separator such a title was
// incidentally safe; under ':' it is the escaping that keeps the key correct,
// so this pins that the escape is applied and is faithful — the components come
// back out of the key exactly as they went in, and two colon-bearing titles
// that differ only in where the colon sits stay in distinct slots.
func TestIndexKeysColonBearingTitleIsEscapedAndFaithful(t *testing.T) {
	t.Parallel()
	const (
		title = "dune: part two"
		year  = "2024"
	)
	key := titleYearKey(title, year, Movie)

	if !strings.Contains(key, `\:`) {
		t.Errorf("titleYearKey(%q, ...) = %q, want the colon inside the title escaped", title, key)
	}
	if keyenc.IsHashed(key) {
		t.Errorf("titleYearKey(%q, ...) = %q, want an escaped join, not a hashed identity", title, key)
	}
	parts, err := keyenc.Split(key)
	if err != nil {
		t.Fatalf("keyenc.Split(%q) error = %v, want the key to be its own inverse", key, err)
	}
	want := []string{title, year, string(Movie)}
	if !slices.Equal(parts, want) {
		t.Errorf("keyenc.Split(%q) = %q, want %q (the components must survive the round trip)", key, parts, want)
	}

	// Two titles differing only in colon placement must not share a slot.
	if a, b := titleYearKey("mission: impossible", year, Movie),
		titleYearKey("mission", ": impossible", Movie); a == b {
		t.Errorf("titles differing in colon placement must not share a slot, both = %q", a)
	}
}

// TestIndexKeysSeparatorCannotForgeAnotherSlot pins the property the adoption
// buys: no component's content can spell another tuple's key. The title is the
// free-form component — lower-cased and trimmed Plex metadata, arbitrary
// operator- and agent-supplied text — and under plain concatenation a title
// carrying the separator could spell the rest of another entry's key.
//
// A collision in these maps is a merged identity, not a cache miss: two
// different Plex items landing in one slot either get pruned by the ambiguity
// guard (silently refusing a legitimate remap) or resolve a history item to the
// wrong entry, and live mode then writes that wrong rating key into Tautulli's
// database.
//
// Reachability, stated honestly: the forging cases below need TWO components to
// carry the separator, and today only the title can — year is strconv.Itoa of an
// int and mediaType is one of ParseMediaType's values. So these tuples are not
// producible by the current pipeline, and the test guards the encoder against
// exactly the change titleYearKey's doc comment warns about (a free-form
// component appended, or year widened to a range), where the failure would be a
// silent wrong database write.
func TestIndexKeysSeparatorCannotForgeAnotherSlot(t *testing.T) {
	t.Parallel()

	t.Run("titleYearKey: separator shifted across the title/year boundary", func(t *testing.T) {
		t.Parallel()
		// ("dune|2021", "extended") vs ("dune", "2021|extended"): same legacy
		// bytes, because '|' inside a component was indistinguishable from the
		// '|' the encoder inserted.
		legacyA := legacyTitleYearKey("dune|2021", "extended", Movie)
		legacyB := legacyTitleYearKey("dune", "2021|extended", Movie)
		if legacyA != legacyB {
			t.Fatalf("premise broken: the legacy form was expected to collide, got %q and %q", legacyA, legacyB)
		}
		gotA := titleYearKey("dune|2021", "extended", Movie)
		gotB := titleYearKey("dune", "2021|extended", Movie)
		if gotA == gotB {
			t.Errorf("a title carrying the separator must not forge another slot, both = %q", gotA)
		}
		// And the same must hold for the separator keyenc actually joins on.
		if a, b := titleYearKey("dune:2021", "extended", Movie),
			titleYearKey("dune", "2021:extended", Movie); a == b {
			t.Errorf("a title carrying ':' must not forge another slot either, both = %q", a)
		}
	})

	t.Run("titleKey: separator shifted across the title/type boundary", func(t *testing.T) {
		t.Parallel()
		legacyA := legacyTitleKey("dune|movie", MediaType(""))
		legacyB := legacyTitleKey("dune", MediaType("movie|"))
		if legacyA != legacyB {
			t.Fatalf("premise broken: the legacy form was expected to collide, got %q and %q", legacyA, legacyB)
		}
		gotA := titleKey("dune|movie", MediaType(""))
		gotB := titleKey("dune", MediaType("movie|"))
		if gotA == gotB {
			t.Errorf("a title carrying the separator must not forge another pair, both = %q", gotA)
		}
		if a, b := titleKey("dune:movie", MediaType("")),
			titleKey("dune", MediaType("movie:")); a == b {
			t.Errorf("a title carrying ':' must not forge another pair either, both = %q", a)
		}
	})

	t.Run("distinct tuples stay distinct across both indexes", func(t *testing.T) {
		t.Parallel()
		type tuple struct {
			title     string
			year      string
			mediaType MediaType
		}
		tuples := []tuple{
			{"dune", "2021", Movie},
			{"dune", "2021", Show},
			{"dune|2021", "movie", MediaType("")},
			{"dune:2021", "movie", MediaType("")},
			{`dune\`, "2021", Movie},
			{`dune\:2021`, "movie", MediaType("")},
			{"dune", "2021", MediaType("")},
			{"", "2021", Movie},
			{"dune 2021", "", Movie},
			{"dune: part two", "2024", Movie},
			{"dune", ": part two:2024", Movie},
		}
		seenTY := make(map[string]tuple, len(tuples))
		seenT := make(map[string]tuple, len(tuples))
		for _, tp := range tuples {
			ty := titleYearKey(tp.title, tp.year, tp.mediaType)
			if prev, dup := seenTY[ty]; dup {
				t.Errorf("titleYearKey collapsed distinct tuples %+v and %+v onto %q", prev, tp, ty)
			} else {
				seenTY[ty] = tp
			}
			// The ByTitle index drops the year, so only tuples differing in
			// (title, mediaType) are required to differ here.
			k := titleKey(tp.title, tp.mediaType)
			if prev, dup := seenT[k]; dup && (prev.title != tp.title || prev.mediaType != tp.mediaType) {
				t.Errorf("titleKey collapsed distinct pairs %+v and %+v onto %q", prev, tp, k)
			} else if !dup {
				seenT[k] = tp
			}
		}
	})
}

// TestIndexKeysBuilderAndLookupAgree pins that the key BuildPlexIndex stores
// under is the same key matchOne looks up by — a regression guard against a
// hand-built key at either end, where the failure is silent (every lookup
// misses).
func TestIndexKeysBuilderAndLookupAgree(t *testing.T) {
	t.Parallel()
	fetcher := &collidingFetcher{
		sections: []Section{{Key: "1", Title: "Movies", Type: string(Movie)}},
		items: map[string][]LibItem{
			"1": {{RatingKey: 42, Title: "Dune | Extended Edition", Year: 2021}},
		},
	}
	idx, failed := BuildPlexIndex(t.Context(), fetcher, 1)
	if failed != 0 {
		t.Fatalf("failedSections = %d, want 0", failed)
	}

	// Rebuild the lookup key exactly as matchOne does, from the same raw title.
	normalized := NormalizeTitle("Dune | Extended Edition")
	if got, ok := idx.ByTitleYear[titleYearKey(normalized, "2021", Movie)]; !ok || got.RatingKey != "42" {
		t.Errorf("idx.ByTitleYear lookup for a separator-bearing title = (%+v, %v), want rating key 42 present", got, ok)
	}
	if got, ok := idx.ByTitle[titleKey(normalized, Movie)]; !ok || got.RatingKey != "42" {
		t.Errorf("idx.ByTitle lookup for a separator-bearing title = (%+v, %v), want rating key 42 present", got, ok)
	}
}
