package orchestrator

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/tautulli-remap/internal/config"
	"github.com/cplieger/tautulli-remap/internal/plex"
	"github.com/cplieger/tautulli-remap/internal/remap"
	"github.com/cplieger/tautulli-remap/internal/tautulli"
)

// --- Fake implementations for unit tests ---

type fakePlex struct{}

func (f *fakePlex) ItemExists(_ context.Context, _ string) (bool, error) { return true, nil }
func (f *fakePlex) LibrarySections(_ context.Context) ([]remap.Section, error) {
	return []remap.Section{{Key: "1", Title: "Movies", Type: "movie"}}, nil
}

func (f *fakePlex) LibraryAll(_ context.Context, _ string) ([]remap.LibItem, error) {
	return []remap.LibItem{{RatingKey: 1, Title: "Test", Year: 2020, GUIDs: []string{"imdb://tt0001"}}}, nil
}

func (f *fakePlex) ResolveEpisodeShow(_ context.Context, _ string) (string, error) { return "", nil }

type fakeTautulli struct{}

func (f *fakeTautulli) APIWithRetry(_ context.Context, _ string, _ url.Values) ([]byte, error) {
	return []byte(`{"response":{"result":"success","data":{"recordsFiltered":0,"data":[]}}}`), nil
}

func (f *fakeTautulli) GetHistory(_ context.Context, _ url.Values) (*tautulli.HistoryPage, error) {
	return &tautulli.HistoryPage{Rows: nil, RecordsFiltered: 0}, nil
}

func (f *fakeTautulli) UpdateMetadata(_ context.Context, _, _ string, _ remap.MediaType) error {
	return nil
}

func (f *fakeTautulli) DeleteRecentlyAdded(_ context.Context) error {
	return nil
}

func TestNew(t *testing.T) {
	o := New(&fakePlex{}, &fakeTautulli{}, &config.Config{})
	if o == nil {
		t.Fatal("expected non-nil orchestrator")
	}
}

func TestRun_DryRun_NoStale(t *testing.T) {
	ft := &fakeTautulli{}
	o := New(&fakePlex{}, ft, &config.Config{DryRun: true})
	o.PaginationDelay = time.Millisecond
	o.RunLockPath = filepath.Join(t.TempDir(), "remap.lock")
	ok := o.Run(t.Context())
	if !ok {
		t.Error("expected success when all keys are valid")
	}
}

// --- Integration tests using httptest servers ---

func testServer(t *testing.T, handler http.HandlerFunc) *config.Config {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &config.Config{
		TautulliURL:    srv.URL,
		TautulliAPIKey: "test-key",
		PlexURL:        srv.URL,
		PlexToken:      "test-token",
		DryRun:         true,
	}
}

func newOrch(t *testing.T, cfg *config.Config) *Orchestrator {
	t.Helper()
	pc, perr := plex.New(cfg.PlexURL, plex.Token(cfg.PlexToken))
	if perr != nil {
		t.Fatalf("plex.New: %v", perr)
	}
	tc := tautulli.New(cfg.TautulliURL, cfg.TautulliAPIKey, &http.Client{})
	tc.RetryDelayUnit = time.Millisecond
	o := New(pc, tc, cfg)
	o.PaginationDelay = time.Millisecond
	o.RunLockPath = filepath.Join(t.TempDir(), "remap.lock")
	return o
}

func TestCollectTautulliItems_SinglePage(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"success","data":{
			"recordsFiltered":2,
			"data":[
				{"rating_key":42,"title":"Movie A","year":2020,"media_type":"movie","guid":"imdb://tt1111111"},
				{"rating_key":99,"grandparent_rating_key":50,"title":"Ep 1","grandparent_title":"Show B","year":2021,"media_type":"episode","guid":"tvdb://271557"}
			]
		}}}`))
	})
	orch := newOrch(t, cfg)
	items, _ := orch.CollectTautulliItems(t.Context())
	if items == nil {
		t.Fatal("expected non-nil items")
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items["42"].Title != "Movie A" {
		t.Errorf("unexpected movie title: %s", items["42"].Title)
	}
	if items["50"].MediaType != remap.Show {
		t.Errorf("expected show, got %s", items["50"].MediaType)
	}
}

func TestCollectTautulliItems_CapturesEpisodeGUID(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":1,"data":[{"rating_key":99,"grandparent_rating_key":50,"title":"Ep 1","grandparent_title":"Show B","year":2021,"media_type":"episode","guid":"plex://episode/5d9c081be98e47001eb0d74f"}]}}}`))
	})
	orch := newOrch(t, cfg)
	items, captured := orch.CollectTautulliItems(t.Context())
	if items == nil {
		t.Fatal("expected non-nil items")
	}
	if captured != 1 {
		t.Errorf("episodeGUIDsCaptured = %d, want 1 (one plex://episode/ GUID retained for later show resolution)", captured)
	}
	guids := items["50"].EpisodeGUIDs
	if len(guids) != 1 || guids[0] != "plex://episode/5d9c081be98e47001eb0d74f" {
		t.Errorf("items[50].EpisodeGUIDs = %v, want [plex://episode/5d9c081be98e47001eb0d74f]", guids)
	}
}

func TestCollectTautulliItems_MultiPage(t *testing.T) {
	calls := 0
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		start := r.URL.Query().Get("start")
		if start == "0" {
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1500,
				"data":[{"rating_key":1,"title":"Movie 1","year":2020,"media_type":"movie","guid":""}]
			}}}`))
		} else {
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1500,
				"data":[{"rating_key":2,"title":"Movie 2","year":2021,"media_type":"movie","guid":""}]
			}}}`))
		}
	})
	orch := newOrch(t, cfg)
	items, _ := orch.CollectTautulliItems(t.Context())
	if items == nil {
		t.Fatal("expected non-nil items")
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
	if calls < 2 {
		t.Errorf("expected at least 2 API calls, got %d", calls)
	}
}

func TestCollectTautulliItems_APIError(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"error","message":"bad request"}}`))
	})
	orch := newOrch(t, cfg)
	items, _ := orch.CollectTautulliItems(t.Context())
	if items != nil {
		t.Errorf("expected nil on API error, got %v", items)
	}
}

func TestCollectTautulliItems_InvalidJSON(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`not json`))
	})
	orch := newOrch(t, cfg)
	items, _ := orch.CollectTautulliItems(t.Context())
	if items != nil {
		t.Errorf("expected nil on invalid JSON, got %v", items)
	}
}

func TestCollectTautulliItems_EmptyData(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":0,"data":[]}}}`))
	})
	orch := newOrch(t, cfg)
	items, _ := orch.CollectTautulliItems(t.Context())
	if items == nil {
		t.Fatal("expected non-nil map")
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}

func TestCollectTautulliItems_HTTPFailure(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	orch := newOrch(t, cfg)
	items, _ := orch.CollectTautulliItems(t.Context())
	if items != nil {
		t.Errorf("expected nil on HTTP failure, got %v", items)
	}
}

func TestCollectTautulliItems_ExceedsMaxRecords(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"success","data":{
			"recordsFiltered":500001,
			"data":[{"rating_key":1,"title":"M","year":2020,"media_type":"movie","guid":""}]
		}}}`))
	})
	orch := newOrch(t, cfg)
	items, _ := orch.CollectTautulliItems(t.Context())
	if items != nil {
		t.Errorf("expected nil when records exceed cap, got %d items", len(items))
	}
}

func TestCollectTautulliItems_ExactPageBoundary(t *testing.T) {
	calls := 0
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		start := r.URL.Query().Get("start")
		if start == "0" {
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1000,
				"data":[{"rating_key":1,"title":"M","year":2020,"media_type":"movie","guid":""}]
			}}}`))
		} else {
			w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":1000,"data":[]}}}`))
		}
	})
	orch := newOrch(t, cfg)
	items, _ := orch.CollectTautulliItems(t.Context())
	if items == nil {
		t.Fatal("expected non-nil items")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (start=1000 >= total=1000 should break)", calls)
	}
}

func TestCollectTautulliItems_PaginationIncrement(t *testing.T) {
	startValues := []string{}
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		startValues = append(startValues, r.URL.Query().Get("start"))
		start := r.URL.Query().Get("start")
		switch start {
		case "0", "1000":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":2500,
				"data":[{"rating_key":1,"title":"M","year":2020,"media_type":"movie","guid":""}]
			}}}`))
		case "2000":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":2500,
				"data":[{"rating_key":2,"title":"N","year":2021,"media_type":"movie","guid":""}]
			}}}`))
		default:
			w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":2500,"data":[]}}}`))
		}
	})
	orch := newOrch(t, cfg)
	items, _ := orch.CollectTautulliItems(t.Context())
	if items == nil {
		t.Fatal("expected non-nil items")
	}
	if len(startValues) != 3 {
		t.Errorf("expected 3 API calls, got %d (starts: %v)", len(startValues), startValues)
	}
	want := []string{"0", "1000", "2000"}
	for i, s := range want {
		if i < len(startValues) && startValues[i] != s {
			t.Errorf("call %d: start = %q, want %q", i, startValues[i], s)
		}
	}
}

func TestCollectTautulliItems_CancelDuringPagination(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			defer cancel()
		}
		w.Write([]byte(`{"response":{"result":"success","data":{
			"recordsFiltered":5000,
			"data":[{"rating_key":1,"title":"M","year":2020,"media_type":"movie","guid":""}]
		}}}`))
	})
	orch := newOrch(t, cfg)
	orch.PaginationDelay = 200 * time.Millisecond
	items, _ := orch.CollectTautulliItems(ctx)
	if items != nil {
		t.Errorf("expected nil on ctx cancel during pagination, got %d items", len(items))
	}
	if calls != 1 {
		t.Errorf("expected 1 API call before cancel, got %d", calls)
	}
}

func TestFindStaleKeys_IdentifiesStaleAndValid(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/library/metadata/42") {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	orch := newOrch(t, cfg)
	items := map[string]remap.TautulliEntry{
		"42":  {RatingKey: "42", Title: "Valid", MediaType: remap.Movie},
		"999": {RatingKey: "999", Title: "Stale", MediaType: remap.Movie},
	}
	stale, err := orch.FindStaleKeys(t.Context(), items)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale, got %d", len(stale))
	}
	if _, ok := stale["999"]; !ok {
		t.Error("expected key 999 to be stale")
	}
}

func TestFindStaleKeys_AllValid(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	orch := newOrch(t, cfg)
	items := map[string]remap.TautulliEntry{
		"1": {RatingKey: "1", Title: "A", MediaType: remap.Movie},
	}
	stale, err := orch.FindStaleKeys(t.Context(), items)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("expected 0 stale, got %d", len(stale))
	}
}

func TestFindStaleKeys_CancelledContext(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	orch := newOrch(t, cfg)
	items := map[string]remap.TautulliEntry{
		"1": {RatingKey: "1", Title: "A", MediaType: remap.Movie},
	}
	_, _ = orch.FindStaleKeys(ctx, items)
}

func TestFindStaleKeys_ProgressLogBoundary(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	orch := newOrch(t, cfg)
	items := map[string]remap.TautulliEntry{}
	for i := 1; i <= 250; i++ {
		k := strconv.Itoa(i)
		items[k] = remap.TautulliEntry{RatingKey: k, Title: "M", MediaType: remap.Movie}
	}
	stale, err := orch.FindStaleKeys(t.Context(), items)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stale) != 250 {
		t.Errorf("expected 250 stale, got %d", len(stale))
	}
}

func TestBuildPlexIndex_AllThreeIndexes(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[
				{"key":"1","title":"Movies","type":"movie"},
				{"key":"2","title":"TV Shows","type":"show"},
				{"key":"3","title":"Music","type":"artist"}
			]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"The Matrix","ratingKey":"42","year":1999,
				 "guid":"plex://movie/abc","Guid":[{"id":"imdb://tt0133093"}]}
			]}}`))
		case strings.Contains(r.URL.Path, "/sections/2/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Breaking Bad","ratingKey":"99","year":2008,
				 "guid":"plex://show/def","Guid":[{"id":"tvdb://81189"}]}
			]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	pc, perr := plex.New(cfg.PlexURL, plex.Token(cfg.PlexToken))
	if perr != nil {
		t.Fatalf("plex.New: %v", perr)
	}
	idx, failed := remap.BuildPlexIndex(t.Context(), pc, 8)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0 (all scanned sections returned 200)", failed)
	}

	if _, ok := idx.ByGUID["imdb://tt0133093"]; !ok {
		t.Error("expected imdb GUID in idx.ByGUID")
	}
	if _, ok := idx.ByGUID["plex://movie/abc"]; !ok {
		t.Error("expected plex movie GUID in idx.ByGUID")
	}
	if _, ok := idx.ByGUID["tvdb://81189"]; !ok {
		t.Error("expected tvdb GUID in idx.ByGUID")
	}
	// These assertions omit remap's private key encoding; they only check
	// that both a movie and a show got indexed into all three maps.
	if !hasIndexedEntry(idx.ByTitleYear, "The Matrix", "1999", remap.Movie) {
		t.Error("expected the movie (The Matrix, 1999) in idx.ByTitleYear")
	}
	if !hasIndexedEntry(idx.ByTitleYear, "Breaking Bad", "2008", remap.Show) {
		t.Error("expected the show (Breaking Bad, 2008) in idx.ByTitleYear")
	}
	if !hasIndexedEntry(idx.ByTitle, "The Matrix", "1999", remap.Movie) {
		t.Error("expected the movie (The Matrix) in idx.ByTitle")
	}
	if !hasIndexedEntry(idx.ByTitle, "Breaking Bad", "2008", remap.Show) {
		t.Error("expected the show (Breaking Bad) in idx.ByTitle")
	}
}

// hasIndexedEntry reports whether an index map holds an entry for the given
// title, year and media type, without depending on remap's key encoding.
func hasIndexedEntry(index map[string]remap.PlexEntry, title, year string, mediaType remap.MediaType) bool {
	for _, e := range index {
		if e.Title.Raw() == title && e.Year == year && e.Type == mediaType {
			return true
		}
	}
	return false
}

func TestBuildPlexIndex_SkipsNonMovieShowSections(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/library/sections") {
			w.Write([]byte(`{"MediaContainer":{"Directory":[
				{"key":"3","title":"Music","type":"artist"}
			]}}`))
		} else {
			t.Error("should not fetch library items for non-movie/show sections")
			w.WriteHeader(http.StatusNotFound)
		}
	})
	pc, perr := plex.New(cfg.PlexURL, plex.Token(cfg.PlexToken))
	if perr != nil {
		t.Fatalf("plex.New: %v", perr)
	}
	idx, failed := remap.BuildPlexIndex(t.Context(), pc, 8)
	if failed != 0 {
		t.Errorf("failedSections = %d, want 0 (non-movie/show sections are skipped, not failed)", failed)
	}
	if len(idx.ByGUID) != 0 || len(idx.ByTitleYear) != 0 || len(idx.ByTitle) != 0 {
		t.Error("expected empty indexes for music-only library")
	}
}

func TestBuildPlexIndex_CancelBetweenSections(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var sectionAllHits atomic.Int32
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[
				{"key":"1","title":"Movies","type":"movie"},
				{"key":"2","title":"TV","type":"show"}
			]}}`))
		case strings.Contains(r.URL.Path, "/sections/"):
			sectionAllHits.Add(1)
			cancel()
			w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
		}
	})
	pc, perr := plex.New(cfg.PlexURL, plex.Token(cfg.PlexToken))
	if perr != nil {
		t.Fatalf("plex.New: %v", perr)
	}
	idx, _ := remap.BuildPlexIndex(ctx, pc, 1)
	if got := sectionAllHits.Load(); got != 1 {
		t.Errorf("expected 1 section fetch before cancel, got %d", got)
	}
	if !idx.Empty() {
		t.Errorf("expected empty indexes on early cancel")
	}
}

func TestApplyRemappings_DryRun(t *testing.T) {
	calls := 0
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Write([]byte(`{}`))
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)
	matched := []remap.MatchResult{
		{Title: "Movie A", Year: "2020", OldKey: "100", NewKey: "200", MediaType: remap.Movie, Method: remap.MethodGUID},
	}
	orch.ApplyRemappings(t.Context(), matched, nil)
	if calls != 0 {
		t.Errorf("expected 0 API calls in dry run, got %d", calls)
	}
}

// captureLogs installs a text handler at the default Info level over a buffer as the default logger for the
// test and returns that buffer. Tests using it must NOT be parallel (the
// default logger is process-global).
//
// slog.SetDefault also points the log package at the installed handler and
// zeroes its flags, and skips that redirect for slog's own default handler, so
// restoring slog alone leaves log writing into a dead buffer. slog goes back
// first: reinstalling a non-default prev re-runs the redirect.
func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return &buf
}

func TestApplyRemappings_DryRunPreviewLoggedAtInfo(t *testing.T) {
	buf := captureLogs(t)

	o := New(&fakePlex{}, &fakeTautulli{}, &config.Config{DryRun: true})
	matched := []remap.MatchResult{
		{Title: "Movie A", Year: "2020", OldKey: "100", NewKey: "200", MediaType: remap.Movie, Method: remap.MethodGUID},
	}
	o.ApplyRemappings(t.Context(), matched, nil)

	out := buf.String()
	if !strings.Contains(out, "msg=remap") {
		t.Errorf("dry-run per-item preview must log at INFO so an operator previews changes at the default log level; got log:\n%s", out)
	}
	if !strings.Contains(out, "old_key=100") || !strings.Contains(out, "new_key=200") {
		t.Errorf("dry-run preview must name the would-be remap keys (old_key=100 new_key=200); got log:\n%s", out)
	}
	if !strings.Contains(out, "dry_run=true") {
		t.Errorf("dry-run preview must record dry_run=true; got log:\n%s", out)
	}
}

func TestApplyRemappings_LiveRemap(t *testing.T) {
	calls := 0
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("cmd") != "update_metadata_details" {
			t.Errorf("unexpected cmd: %s", r.URL.Query().Get("cmd"))
		}
		w.Write([]byte(`{"response":{"result":"success"}}`))
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	matched := []remap.MatchResult{
		{Title: "Movie A", Year: "2020", OldKey: "100", NewKey: "200", MediaType: remap.Movie, Method: remap.MethodGUID},
	}
	orch.ApplyRemappings(t.Context(), matched, nil)
	if calls != 1 {
		t.Errorf("expected 1 API call, got %d", calls)
	}
}

func TestApplyRemappings_APIError(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	matched := []remap.MatchResult{
		{Title: "Movie A", Year: "2020", OldKey: "100", NewKey: "200", MediaType: remap.Movie, Method: remap.MethodGUID},
	}
	updated, failed, aborted := orch.ApplyRemappings(t.Context(), matched, nil)
	if updated != 0 {
		t.Errorf("updated = %d, want 0 (the only remap failed)", updated)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1 (the single 500 response is one failed remap)", failed)
	}
	if aborted {
		t.Error("aborted = true, want false (a single failure must not trip the 10-failure breaker)")
	}
}

func TestApplyRemappings_EmptyMatched(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("should not call API with empty matched")
	})
	orch := newOrch(t, cfg)
	orch.ApplyRemappings(t.Context(), nil, nil)
}

func TestApplyRemappings_CancelledContext(t *testing.T) {
	calls := 0
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Write([]byte(`{"response":{"result":"success"}}`))
	})
	cfg.DryRun = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	orch := newOrch(t, cfg)
	matched := []remap.MatchResult{
		{Title: "A", OldKey: "1", NewKey: "2", MediaType: remap.Movie, Method: remap.MethodGUID},
		{Title: "B", OldKey: "3", NewKey: "4", MediaType: remap.Movie, Method: remap.MethodGUID},
	}
	orch.ApplyRemappings(ctx, matched, nil)
	if calls > 1 {
		t.Errorf("expected at most 1 call with cancelled context, got %d", calls)
	}
}

func TestApplyRemappings_AbortsAfterConsecutiveFailures(t *testing.T) {
	calls := 0
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	matched := make([]remap.MatchResult, 15)
	for i := range matched {
		matched[i] = remap.MatchResult{
			Title: runesafe.Untrusted(fmt.Sprintf("Movie %d", i)), Year: "2020",
			OldKey: strconv.Itoa(i), NewKey: strconv.Itoa(100 + i),
			MediaType: remap.Movie, Method: remap.MethodGUID,
		}
	}
	updated, failed, aborted := orch.ApplyRemappings(t.Context(), matched, nil)
	if updated != 0 {
		t.Errorf("updated = %d, want 0", updated)
	}
	if calls > 10 {
		t.Errorf("calls = %d, want <= 10 (should abort after consecutive failures)", calls)
	}
	if failed != 10 {
		t.Errorf("failed = %d, want 10", failed)
	}
	if !aborted {
		t.Error("aborted = false, want true (breaker tripped after 10 consecutive failures)")
	}
}

func TestClearRecentlyAdded_DryRun(t *testing.T) {
	calls := 0
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)
	if !orch.ClearRecentlyAdded(t.Context()) {
		t.Error("ClearRecentlyAdded() = false in dry run, want true (nothing to fail)")
	}
	if calls != 0 {
		t.Errorf("expected 0 API calls in dry run, got %d", calls)
	}
}

func TestClearRecentlyAdded_Live(t *testing.T) {
	calls := 0
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("cmd") != "delete_recently_added" {
			t.Errorf("unexpected cmd: %s", r.URL.Query().Get("cmd"))
		}
		w.Write([]byte(`{"response":{"result":"success"}}`))
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if !orch.ClearRecentlyAdded(t.Context()) {
		t.Error("ClearRecentlyAdded() = false, want true on a successful clear")
	}
	if calls != 1 {
		t.Errorf("expected 1 API call, got %d", calls)
	}
}

func TestClearRecentlyAdded_HTTPError(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if orch.ClearRecentlyAdded(t.Context()) {
		t.Error("ClearRecentlyAdded() = true, want false when the API call fails (incomplete cleanup must be reported)")
	}
}

func TestRun_AllKeysValid(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("cmd") == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":42,"title":"Movie","year":2020,"media_type":"movie","guid":""}]
			}}}`))
		case strings.Contains(r.URL.Path, "/library/metadata/"):
			w.WriteHeader(http.StatusOK)
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)
	if !orch.Run(t.Context()) {
		t.Error("expected true when all keys are valid")
	}
}

func TestRun_StaleKeysTriggerRemap(t *testing.T) {
	remapCalled := false
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Stale Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case cmd == "update_metadata_details":
			remapCalled = true
			w.Write([]byte(`{"response":{"result":"success"}}`))
		case cmd == "delete_recently_added":
			w.Write([]byte(`{"response":{"result":"success"}}`))
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Stale Movie","ratingKey":"200","year":2020,
				 "guid":"plex://movie/x","Guid":[{"id":"imdb://tt1111111"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if !orch.Run(t.Context()) {
		t.Error("expected true on successful remap")
	}
	if !remapCalled {
		t.Error("expected update_metadata_details to be called")
	}
}

func TestRun_HistoryFailure(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)
	if orch.Run(t.Context()) {
		t.Error("expected false when history collection fails")
	}
}

func TestRun_DryRunSkipsBackup(t *testing.T) {
	backupCalled := false
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		if cmd == "backup_db" {
			backupCalled = true
		}
		if cmd == "get_history" {
			w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":0,"data":[]}}}`))
			return
		}
		w.Write([]byte(`{}`))
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)
	orch.Run(t.Context())
	if backupCalled {
		t.Error("backup_db should not be called in dry run")
	}
}

// TestRun_NonDryRunCallsBackup pins the deferred-backup contract: a live run
// with a mapping ready backs up before the first write.
func TestRun_NonDryRunCallsBackup(t *testing.T) {
	var mu sync.Mutex
	var cmds []string
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		if cmd != "" {
			mu.Lock()
			cmds = append(cmds, cmd)
			mu.Unlock()
		}
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Stale Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case cmd == "update_metadata_details" || cmd == "delete_recently_added":
			w.Write([]byte(`{"response":{"result":"success"}}`))
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Stale Movie","ratingKey":"200","year":2020,
				 "guid":"plex://movie/x","Guid":[{"id":"imdb://tt1111111"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if !orch.Run(t.Context()) {
		t.Fatal("Run() = false, want true")
	}
	backupAt, updateAt := -1, -1
	mu.Lock()
	for i, c := range cmds {
		if c == "backup_db" && backupAt < 0 {
			backupAt = i
		}
		if c == "update_metadata_details" && updateAt < 0 {
			updateAt = i
		}
	}
	mu.Unlock()
	if backupAt < 0 {
		t.Fatal("backup_db not called on a live run with a mapping to apply")
	}
	if updateAt < 0 {
		t.Fatal("update_metadata_details not called")
	}
	if backupAt > updateAt {
		t.Errorf("backup_db called at index %d AFTER update_metadata_details at %d; backup must precede the first write", backupAt, updateAt)
	}
}

// TestRun_LiveNoWorkSkipsBackup pins the other half of the deferred-backup
// contract: a run with nothing stale never spends a backup.
func TestRun_LiveNoWorkSkipsBackup(t *testing.T) {
	backupCalled := false
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		if cmd == "backup_db" {
			backupCalled = true
		}
		if cmd == "get_history" {
			w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":0,"data":[]}}}`))
			return
		}
		w.Write([]byte(`{}`))
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if !orch.Run(t.Context()) {
		t.Fatal("Run() = false, want true when nothing is stale")
	}
	if backupCalled {
		t.Error("backup_db called on a live run with nothing to remap; backup is deferred until a mapping is ready")
	}
}

// TestRun_LiveStaleUnmatchedSkipsBackup exercises the backup gate itself: a
// stale item no strategy matches leaves zero mappings, so backup_db is never
// called.
func TestRun_LiveStaleUnmatchedSkipsBackup(t *testing.T) {
	backupCalled := false
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "backup_db":
			backupCalled = true
			w.Write([]byte(`{}`))
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Stale Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Entirely Different","ratingKey":"200","year":1999,
				 "guid":"plex://movie/y","Guid":[{"id":"imdb://tt9999999"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if !orch.Run(t.Context()) {
		t.Fatal("Run() = false, want true (an unmatched stale item is not a failure)")
	}
	if backupCalled {
		t.Error("backup_db called with zero mappings ready; the gate must skip it")
	}
}

func TestRun_EmptyPlexIndex(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Movie","year":2020,"media_type":"movie","guid":""}]
			}}}`))
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)
	if orch.Run(t.Context()) {
		t.Error("expected false when Plex library index is empty")
	}
}

func TestRun_BackupFailureAborts(t *testing.T) {
	var mu sync.Mutex
	callCounts := map[string]int{}
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		mu.Lock()
		callCounts[cmd]++
		mu.Unlock()
		switch {
		case cmd == "backup_db":
			w.WriteHeader(http.StatusInternalServerError)
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Stale Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Stale Movie","ratingKey":"200","year":2020,
				 "guid":"plex://movie/x","Guid":[{"id":"imdb://tt1111111"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if orch.Run(t.Context()) {
		t.Error("Run() = true, want false when backup_db fails in live mode (no recovery point, must abort)")
	}
	mu.Lock()
	defer mu.Unlock()
	if callCounts["backup_db"] != 3 {
		t.Errorf("expected backup_db called 3 times via retry, got %d", callCounts["backup_db"])
	}
	if callCounts["update_metadata_details"] != 0 {
		t.Errorf("expected update_metadata_details NOT called (backup failed, no recovery point), got %d", callCounts["update_metadata_details"])
	}
	if callCounts["delete_recently_added"] != 0 {
		t.Errorf("expected delete_recently_added NOT called after failed backup, got %d", callCounts["delete_recently_added"])
	}
}

// TestRun_ClearFailureFailsRun pins the cleanup-propagation contract: updates
// landing but the recently-added clear failing must report failure.
func TestRun_ClearFailureFailsRun(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Stale Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case cmd == "update_metadata_details":
			w.Write([]byte(`{"response":{"result":"success"}}`))
		case cmd == "delete_recently_added":
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Stale Movie","ratingKey":"200","year":2020,
				 "guid":"plex://movie/x","Guid":[{"id":"imdb://tt1111111"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if orch.Run(t.Context()) {
		t.Error("Run() = true, want false when the recently-added clear fails (documented cleanup incomplete)")
	}
}

// TestRun_RefusesOverlappingRun pins the single-flight contract: a concurrent
// Run refuses immediately while another holds the lock, making no calls;
// once released, the next pass proceeds normally.
func TestRun_RefusesOverlappingRun(t *testing.T) {
	release := make(chan struct{})
	firstEntered := make(chan struct{})
	var histCalls atomic.Int32
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cmd") == "get_history" {
			if histCalls.Add(1) == 1 {
				close(firstEntered)
				<-release
			}
			w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":0,"data":[]}}}`))
			return
		}
		w.Write([]byte(`{}`))
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)

	firstDone := make(chan bool)
	go func() { firstDone <- orch.Run(t.Context()) }()
	<-firstEntered // the first pass now holds the lock, blocked mid-collection

	if orch.Run(t.Context()) {
		t.Error("overlapping Run() = true, want false while another pass holds the lock")
	}
	if got := histCalls.Load(); got != 1 {
		t.Errorf("refused pass reached the API: get_history calls = %d, want 1 (only the holder's)", got)
	}

	close(release)
	if !<-firstDone {
		t.Error("holder Run() = false, want true (a refused contender must not affect the holder)")
	}

	// Lock released with the holder gone: the next pass proceeds normally.
	if !orch.Run(t.Context()) {
		t.Error("post-release Run() = false, want true (lock must be released after a pass)")
	}
	if got := histCalls.Load(); got != 2 {
		t.Errorf("get_history calls = %d, want 2 (holder + post-release pass)", got)
	}
}

func TestRun_AllRemapsFail_ReturnsFalse(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Stale Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case cmd == "update_metadata_details":
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Stale Movie","ratingKey":"200","year":2020,
				 "guid":"plex://movie/x","Guid":[{"id":"imdb://tt1111111"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if orch.Run(t.Context()) {
		t.Error("Run() = true, want false when the only stale item failed to remap (updated=0, failed=1)")
	}
}

func TestPaginationDelay_DefaultWhenUnset(t *testing.T) {
	o := New(&fakePlex{}, &fakeTautulli{}, &config.Config{})
	if got := o.paginationDelay(); got != defaultPaginationDelay {
		t.Errorf("paginationDelay() = %v, want %v (default)", got, defaultPaginationDelay)
	}
}

func TestRun_AbortsOnPartialSectionFailure(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Stale Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[
				{"key":"1","title":"Movies","type":"movie"},
				{"key":"2","title":"Movies 4K","type":"movie"}
			]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Stale Movie","ratingKey":"200","year":2020,
				 "guid":"plex://movie/x","Guid":[{"id":"imdb://tt1111111"}]}
			]}}`))
		case strings.Contains(r.URL.Path, "/sections/2/all"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)
	// Section 1 loads (index is non-empty, so the all-empty guard passes) but
	// section 2 returns 500. The run must abort rather than remap against the
	// partial index.
	if orch.Run(t.Context()) {
		t.Error("Run() = true, want false when a Plex section fetch failed (partial index must not be trusted)")
	}
}

// TestFindStaleKeys_PlexErrorAborts pins the fail-closed rule: a Plex
// existence-check error (not a 404) must abort rather than mark items stale.
func TestFindStaleKeys_PlexErrorAborts(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	orch := newOrch(t, cfg)
	items := map[string]remap.TautulliEntry{
		"1": {RatingKey: "1", Title: "A", MediaType: remap.Movie},
	}
	stale, err := orch.FindStaleKeys(t.Context(), items)
	if err == nil {
		t.Error("expected non-nil error when Plex returns 500 during the stale-key check")
	}
	if len(stale) != 0 {
		t.Errorf("expected no items marked stale on error, got %d", len(stale))
	}
}

// TestRun_AbortsWhenPlexCheckErrors pins the same rule end-to-end: a
// persistent Plex error during the stale-key check aborts the run rather
// than remapping an unverifiable item.
func TestRun_AbortsWhenPlexCheckErrors(t *testing.T) {
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case strings.Contains(r.URL.Path, "/library/metadata/"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Movie","ratingKey":"200","year":2020,
				 "guid":"plex://movie/x","Guid":[{"id":"imdb://tt1111111"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)
	if orch.Run(t.Context()) {
		t.Error("Run() = true, want false when Plex returns errors during the stale-key check")
	}
}

// TestRun_BreakerTripAfterSuccessReturnsFalse pins that the run fails when
// the consecutive-failure breaker trips during remap, even after >=1 update
// already landed.
func TestRun_BreakerTripAfterSuccessReturnsFalse(t *testing.T) {
	const n = 12
	var updateCalls atomic.Int64
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			var sb strings.Builder
			sb.WriteString(`{"response":{"result":"success","data":{"recordsFiltered":12,"data":[`)
			for i := range n {
				if i > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, `{"rating_key":%d,"title":"Movie %d","year":2020,"media_type":"movie","guid":"imdb://tt%d"}`, 100+i, i, 1000+i)
			}
			sb.WriteString(`]}}}`)
			w.Write([]byte(sb.String()))
		case cmd == "update_metadata_details":
			// First succeeds; every later call fails, tripping the breaker
			// only after the success was counted.
			if updateCalls.Add(1) == 1 {
				w.Write([]byte(`{"response":{"result":"success"}}`))
			} else {
				w.WriteHeader(http.StatusInternalServerError)
			}
		case cmd == "delete_recently_added":
			w.Write([]byte(`{"response":{"result":"success"}}`))
		case strings.Contains(r.URL.Path, "/library/metadata/"):
			w.WriteHeader(http.StatusNotFound) // every item is stale
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			var sb strings.Builder
			sb.WriteString(`{"MediaContainer":{"Metadata":[`)
			for i := range n {
				if i > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, `{"title":"Movie %d","ratingKey":"%d","year":2020,"guid":"plex://movie/x%d","Guid":[{"id":"imdb://tt%d"}]}`, i, 200+i, i, 1000+i)
			}
			sb.WriteString(`]}}`)
			w.Write([]byte(sb.String()))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if orch.Run(t.Context()) {
		t.Error("Run() = true, want false when the circuit breaker tripped (even though >=1 update succeeded before the trip)")
	}
	if got := updateCalls.Load(); got < 11 {
		t.Errorf("update calls = %d, want >= 11 (1 success + 10 consecutive failures to trip the breaker)", got)
	}
}

// TestRunScheduler_ShutdownInterruptedRunNotCountedAsFailure verifies a
// scheduled run interrupted by shutdown is not logged as a failure, does not
// count toward the failure threshold, and does not flip the health marker.
func TestRunScheduler_ShutdownInterruptedRunNotCountedAsFailure(t *testing.T) {
	buf := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled on purpose; Background, not t.Context()

	var falseCount int
	setHealthy := func(healthy bool) {
		if !healthy {
			falseCount++
		}
	}

	o := New(&fakePlex{}, &fakeTautulli{}, &config.Config{DryRun: true, RemapInterval: time.Hour})
	o.RunLockPath = filepath.Join(t.TempDir(), "remap.lock")
	o.RunScheduler(ctx, setHealthy)

	out := buf.String()
	if strings.Contains(out, "run failed") {
		t.Errorf("shutdown-interrupted run logged a failure; want none. log:\n%s", out)
	}
	if falseCount != 0 {
		t.Errorf("setHealthy(false) called %d times; a shutdown-interrupted run must not flip the marker unhealthy", falseCount)
	}
}

// TestRun_CancelledContextReturnsFalse verifies a run cancelled mid-flight
// reports failure, not success, even if history collection itself succeeded.
func TestRun_CancelledContextReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := New(&fakePlex{}, &fakeTautulli{}, &config.Config{DryRun: true})
	o.PaginationDelay = time.Millisecond
	o.RunLockPath = filepath.Join(t.TempDir(), "remap.lock")
	if o.Run(ctx) {
		t.Error("Run() = true, want false when the context is cancelled (a shutdown-interrupted run must not report success)")
	}
}

func TestRun_PartialRemapFailureStillSucceeds(t *testing.T) {
	var updates atomic.Int64
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":2,
				"data":[
					{"rating_key":100,"title":"Movie A","year":2020,"media_type":"movie","guid":"imdb://tt1000001"},
					{"rating_key":101,"title":"Movie B","year":2021,"media_type":"movie","guid":"imdb://tt1000002"}
				]
			}}}`))
		case cmd == "update_metadata_details":
			// One failure is far below the breaker threshold, so the run
			// still makes progress.
			if updates.Add(1) == 1 {
				w.Write([]byte(`{"response":{"result":"success"}}`))
			} else {
				w.WriteHeader(http.StatusInternalServerError)
			}
		case cmd == "delete_recently_added":
			w.Write([]byte(`{"response":{"result":"success"}}`))
		case strings.Contains(r.URL.Path, "/library/metadata/"):
			w.WriteHeader(http.StatusNotFound) // both items are stale
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Movie A","ratingKey":"200","year":2020,"guid":"plex://movie/a","Guid":[{"id":"imdb://tt1000001"}]},
				{"title":"Movie B","ratingKey":"201","year":2021,"guid":"plex://movie/b","Guid":[{"id":"imdb://tt1000002"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if !orch.Run(t.Context()) {
		t.Error("Run() = false, want true: a partial remap (one update landed, one failed, breaker not tripped) made progress and must not flap the health marker")
	}
}

// scriptedScheduler drives RunScheduler deterministically: each GetHistory
// call consumes the next failPlan entry, cancelling once the plan is
// exhausted so RunScheduler returns instead of ticking forever.
type scriptedScheduler struct {
	fakeTautulli
	failPlan []bool
	next     int
	cancel   context.CancelFunc
}

func (f *scriptedScheduler) GetHistory(_ context.Context, _ url.Values) (*tautulli.HistoryPage, error) {
	i := f.next
	f.next++
	if i >= len(f.failPlan) {
		f.cancel()
		return nil, fmt.Errorf("scheduler stop after %d calls", i)
	}
	if f.failPlan[i] {
		return nil, fmt.Errorf("simulated tautulli failure on call %d", i)
	}
	return &tautulli.HistoryPage{}, nil
}

func TestRunScheduler_FlipsUnhealthyAfterConsecutiveFailures(t *testing.T) {
	buf := captureLogs(t)

	// Background, not t.Context(): cancel is both the scheduler's stop signal
	// and the Cleanup safety net, so its lifetime is tied to Cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ft := &scriptedScheduler{failPlan: []bool{true, true, true}, cancel: cancel}
	o := New(&fakePlex{}, ft, &config.Config{DryRun: true, RemapInterval: time.Millisecond})
	o.RunLockPath = filepath.Join(t.TempDir(), "remap.lock")

	trueCount, falseCount := 0, 0
	setHealthy := func(healthy bool) {
		if healthy {
			trueCount++
		} else {
			falseCount++
		}
	}

	o.RunScheduler(ctx, setHealthy)

	out := buf.String()
	if falseCount != 1 {
		t.Errorf("setHealthy(false) called %d times, want 1 (flip once after 3 consecutive failures); log:\n%s", falseCount, out)
	}
	if trueCount < 1 {
		t.Errorf("setHealthy(true) called %d times, want >=1 (initial mark healthy)", trueCount)
	}
	if !strings.Contains(out, "consecutive_failures=3") {
		t.Errorf("expected a 'run failed' log at consecutive_failures=3; log:\n%s", out)
	}
}

func TestRunScheduler_ResetsFailureCountOnSuccess(t *testing.T) {
	buf := captureLogs(t)

	// Background, not t.Context(): cancel is both the scheduler's stop signal
	// and the Cleanup safety net, so its lifetime is tied to Cleanup.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// fail, fail, succeed (resets the counter), fail: never reaches the
	// threshold of 3, so the marker must never flip unhealthy.
	ft := &scriptedScheduler{failPlan: []bool{true, true, false, true}, cancel: cancel}
	o := New(&fakePlex{}, ft, &config.Config{DryRun: true, RemapInterval: time.Millisecond})
	o.RunLockPath = filepath.Join(t.TempDir(), "remap.lock")

	falseCount := 0
	setHealthy := func(healthy bool) {
		if !healthy {
			falseCount++
		}
	}

	o.RunScheduler(ctx, setHealthy)

	out := buf.String()
	if falseCount != 0 {
		t.Errorf("setHealthy(false) called %d times, want 0 (a success between failures resets the counter below the threshold); log:\n%s", falseCount, out)
	}
	if !strings.Contains(out, "run complete") {
		t.Errorf("expected a 'run complete' log from the successful run; log:\n%s", out)
	}
}

func TestRun_ShutdownDuringRemapReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Stale Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case cmd == "update_metadata_details":
			cancel()
			w.Write([]byte(`{"response":{"result":"success"}}`))
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Stale Movie","ratingKey":"200","year":2020,
				 "guid":"plex://movie/x","Guid":[{"id":"imdb://tt1111111"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = false
	orch := newOrch(t, cfg)
	if orch.Run(ctx) {
		t.Error("Run() = true, want false when shutdown cancels the context during the remap phase (a cancelled run must not report success even if an update landed before the signal)")
	}
}

// shutdownOnUpdateTautulli cancels the run context on the first UpdateMetadata
// call and returns an error, simulating graceful shutdown arriving mid-remap.
type shutdownOnUpdateTautulli struct {
	fakeTautulli
	cancel context.CancelFunc
}

func (f *shutdownOnUpdateTautulli) UpdateMetadata(_ context.Context, _, _ string, _ remap.MediaType) error {
	f.cancel()
	return context.Canceled
}

func TestApplyRemappings_ShutdownDuringUpdateIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	o := New(&fakePlex{}, &shutdownOnUpdateTautulli{cancel: cancel}, &config.Config{DryRun: false})
	matched := []remap.MatchResult{
		{Title: "A", OldKey: "1", NewKey: "2", MediaType: remap.Movie, Method: remap.MethodGUID},
		{Title: "B", OldKey: "3", NewKey: "4", MediaType: remap.Movie, Method: remap.MethodGUID},
	}
	updated, failed, aborted := o.ApplyRemappings(ctx, matched, nil)
	if updated != 0 {
		t.Errorf("updated = %d, want 0 (the update was interrupted by shutdown, not completed)", updated)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0 (a shutdown-interrupted update is not a remap failure and must not count toward the consecutive-failure breaker)", failed)
	}
	if aborted {
		t.Error("aborted = true, want false (a shutdown breaks the loop cleanly; it does not trip the breaker)")
	}
}

func TestRun_DryRunWithMatch_PreviewsClear(t *testing.T) {
	buf := captureLogs(t)

	cfg := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		cmd := r.URL.Query().Get("cmd")
		switch {
		case cmd == "get_history":
			w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":100,"title":"Stale Movie","year":2020,"media_type":"movie","guid":"imdb://tt1111111"}]
			}}}`))
		case strings.HasSuffix(r.URL.Path, "/library/metadata/100"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/library/sections"):
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","title":"Movies","type":"movie"}]}}`))
		case strings.Contains(r.URL.Path, "/sections/1/all"):
			w.Write([]byte(`{"MediaContainer":{"Metadata":[
				{"title":"Stale Movie","ratingKey":"200","year":2020,
				 "guid":"plex://movie/x","Guid":[{"id":"imdb://tt1111111"}]}
			]}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	cfg.DryRun = true
	orch := newOrch(t, cfg)
	if !orch.Run(t.Context()) {
		t.Fatal("Run() = false, want true (a dry-run with a successful match is a successful pass)")
	}
	out := buf.String()
	if !strings.Contains(out, "would clear recently added") {
		t.Errorf("dry-run with a match must PREVIEW the recently-added clear so the preview does not hide a mutation the live run performs; log lacked the preview line:\n%s", out)
	}
	if strings.Contains(out, "skipping clear recently added") {
		t.Errorf("dry-run with a match must not take the no-updates skip branch; log:\n%s", out)
	}
}

// resolveFakePlex embeds fakePlex with a programmable ResolveEpisodeShow.
type resolveFakePlex struct {
	fakePlex
	resolve func(ctx context.Context, guid string) (string, error)
}

func (f *resolveFakePlex) ResolveEpisodeShow(ctx context.Context, guid string) (string, error) {
	return f.resolve(ctx, guid)
}

func TestResolveStaleShows(t *testing.T) {
	stale := map[string]remap.TautulliEntry{
		"10": {RatingKey: "10", MediaType: remap.Show, EpisodeGUIDs: []string{"plex://episode/a"}},
		"20": {RatingKey: "20", MediaType: remap.Movie, GUID: "imdb://tt1"},                        // movie: skipped
		"30": {RatingKey: "30", MediaType: remap.Show},                                             // show, no episode GUIDs: skipped
		"40": {RatingKey: "40", MediaType: remap.Show, EpisodeGUIDs: []string{"plex://episode/d"}}, // resolves to itself
	}
	fp := &resolveFakePlex{resolve: func(_ context.Context, guid string) (string, error) {
		switch guid {
		case "plex://episode/a":
			return "11", nil
		case "plex://episode/d":
			return "40", nil // same as the stale key: must be dropped
		default:
			return "", nil
		}
	}}
	o := New(fp, &fakeTautulli{}, &config.Config{})

	resolved := o.resolveStaleShows(t.Context(), stale)
	if len(resolved) != 1 {
		t.Fatalf("resolved = %v, want exactly {10:11}", resolved)
	}
	if resolved["10"] != "11" {
		t.Errorf("resolved[10] = %q, want 11", resolved["10"])
	}
	if _, ok := resolved["40"]; ok {
		t.Error("a resolution to the same (unchanged) key must not be recorded")
	}
}

func TestResolveOneShow_TriesUntilResolvedToleratingErrors(t *testing.T) {
	var calls int32
	fp := &resolveFakePlex{resolve: func(_ context.Context, guid string) (string, error) {
		atomic.AddInt32(&calls, 1)
		switch guid {
		case "plex://episode/err":
			return "", fmt.Errorf("transient boom") // error: logged, try next
		case "plex://episode/miss":
			return "", nil // no match: try next
		case "plex://episode/hit":
			return "500", nil
		}
		return "", nil
	}}
	o := New(fp, &fakeTautulli{}, &config.Config{})

	got := o.resolveOneShow(t.Context(),
		[]string{"plex://episode/err", "plex://episode/miss", "plex://episode/hit"})
	if got != "500" {
		t.Errorf("resolveOneShow = %q, want 500", got)
	}
	if calls != 3 {
		t.Errorf("resolve calls = %d, want 3 (each tried until the hit)", calls)
	}
}

func TestCircuitBreaker_record(t *testing.T) {
	tests := []struct {
		name      string
		threshold int
		sequence  []bool
		wantTrip  []bool
	}{
		{
			name:      "trips exactly at the threshold of consecutive failures",
			threshold: 3,
			sequence:  []bool{false, false, false},
			wantTrip:  []bool{false, false, true},
		},
		{
			name:      "a success resets the consecutive count so a following failure run restarts from zero",
			threshold: 3,
			sequence:  []bool{false, false, true, false, false},
			wantTrip:  []bool{false, false, false, false, false},
		},
		{
			name:      "successes before any failure keep the breaker closed",
			threshold: 2,
			sequence:  []bool{true, true, true},
			wantTrip:  []bool{false, false, false},
		},
		{
			name:      "the breaker trips again after a reset once the threshold is reached anew",
			threshold: 2,
			sequence:  []bool{false, true, false, false},
			wantTrip:  []bool{false, false, false, true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cb := newCircuitBreaker(tc.threshold)
			for i, ok := range tc.sequence {
				if got := cb.record(ok); got != tc.wantTrip[i] {
					t.Errorf("step %d: record(%v) = %v, want %v", i, ok, got, tc.wantTrip[i])
				}
			}
		})
	}
}
