package tautulli

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/tautulli-remap/internal/remap"
)

// newTestClient creates a Client with fast retry for tests.
func newTestClient(url, apiKey string, httpClient *http.Client) *Client {
	c := New(url, apiKey, httpClient)
	c.RetryDelayUnit = time.Millisecond
	return c
}

// newBubbleClient points a Client at an in-memory httptest server for use
// inside a synctest bubble; RetryDelayUnit stays at the production default
// since the bubble's synthetic clock makes real backoff free.
func newBubbleClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	return New("http://tautulli.invalid", "k", srv.Client())
}

func TestAPI_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cmd") != "get_history" {
			t.Errorf("unexpected cmd: %s", r.URL.Query().Get("cmd"))
		}
		if r.URL.Query().Get("apikey") != "testkey" {
			t.Errorf("unexpected apikey: %s", r.URL.Query().Get("apikey"))
		}
		w.Write([]byte(`{"response":{"result":"success"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "testkey", srv.Client())
	body, err := c.API(t.Context(), "get_history", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "success") {
		t.Errorf("unexpected body: %s", body)
	}
}

func TestAPI_Non200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "testkey", srv.Client())
	_, err := c.API(t.Context(), "get_history", nil)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAPI_ExtraParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") != "100" {
			t.Errorf("expected start=100, got %s", r.URL.Query().Get("start"))
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	extra := url.Values{"start": {"100"}}
	_, err := c.API(t.Context(), "test", extra)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAPI_CancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newTestClient(srv.URL, "k", srv.Client())
	_, err := c.API(ctx, "test", nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestAPI_NonRetryable4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "testkey", srv.Client())
	_, err := c.APIWithRetry(t.Context(), "backup_db", nil)
	if err == nil {
		t.Fatal("expected error for 403")
	}
}

func TestAPI_InvalidURL(t *testing.T) {
	c := newTestClient("://invalid-url", "key", &http.Client{})
	_, err := c.API(t.Context(), "test", nil)
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestAPI_ExtraCannotOverrideBaseParams(t *testing.T) {
	var gotCmd, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCmd = r.URL.Query().Get("cmd")
		gotAPIKey = r.URL.Query().Get("apikey")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "base-key", srv.Client())
	// A caller-supplied extra must not clobber the command or the API
	// credential: requestURL applies cmd/apikey after merging extra.
	extra := url.Values{"cmd": {"override_cmd"}, "apikey": {"override-key"}}
	_, err := c.API(t.Context(), "base_cmd", extra)
	if err != nil {
		t.Fatal(err)
	}
	if gotCmd != "base_cmd" {
		t.Errorf("cmd = %q, want base_cmd (extra must not override the command)", gotCmd)
	}
	if gotAPIKey != "base-key" {
		t.Errorf("apikey = %q, want base-key (extra must not override the credential)", gotAPIKey)
	}
}

func TestAPI_ErrorDoesNotLeakAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			// Unreachable under httptest's HTTP/1.1 ResponseWriter; t.Errorf
			// (not t.Skip, whose Goexit would run on the server goroutine)
			// fails loudly if this assumption ever breaks.
			t.Errorf("httptest ResponseWriter does not implement http.Hijacker")
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "supersecretkey123", srv.Client())
	_, err := c.API(t.Context(), "test", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "supersecretkey123") {
		t.Errorf("error message leaked API key: %s", err.Error())
	}
}

func TestAPIWithRetry_SucceedsOnFirst(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	body, err := c.APIWithRetry(t.Context(), "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Errorf("unexpected body: %s", body)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

// TestAPIWithRetry_RetriesOnServerError runs in a synctest bubble so the
// production backoff (RetryDelayUnit unmodified) is paid in synthetic time.
func TestAPIWithRetry_RetriesOnServerError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		c := newBubbleClient(t, func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			if attempts < 3 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write([]byte(`ok`))
		})

		body, err := c.APIWithRetry(t.Context(), "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "ok" {
			t.Errorf("unexpected body: %s", body)
		}
		if attempts != 3 {
			t.Errorf("expected 3 attempts, got %d", attempts)
		}
	})
}

// TestAPIWithRetry_Exact3Attempts pins the attempt cap and, via the synctest
// bubble's synthetic time, that an exhausted retry chain at the real 5-second
// base delay still fits inside ClientTimeout.
func TestAPIWithRetry_Exact3Attempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := newBubbleClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusInternalServerError)
		})

		start := time.Now()
		_, err := c.APIWithRetry(t.Context(), "test_cmd", nil)
		elapsed := time.Since(start)
		if err == nil {
			t.Error("expected error from all-failing retries")
		}
		if calls != 3 {
			t.Errorf("calls = %d, want 3", calls)
		}
		// Bound only: httpx jitters each delay, so the exact figure varies.
		if elapsed >= ClientTimeout {
			t.Errorf("production retry schedule took %v, which does not fit inside the %v client timeout",
				elapsed, ClientTimeout)
		}
	})
}

// TestAPIWithRetry_SuccessOnSecondAttempt runs in a synctest bubble so the one
// production backoff between the two attempts is paid in synthetic time.
func TestAPIWithRetry_SuccessOnSecondAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := newBubbleClient(t, func(w http.ResponseWriter, _ *http.Request) {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write([]byte(`{"response":{"result":"success"}}`))
		})

		body, err := c.APIWithRetry(t.Context(), "test_cmd", nil)
		if err != nil {
			t.Fatal(err)
		}
		if body == nil {
			t.Error("expected non-nil body")
		}
		if calls != 2 {
			t.Errorf("calls = %d, want 2", calls)
		}
	})
}

func TestAPIWithRetry_CancelledContextStopsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newTestClient(srv.URL, "k", srv.Client())
	_, err := c.APIWithRetry(ctx, "test", nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestGetHistory_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"success","data":{
			"recordsFiltered":1,
			"data":[{"rating_key":42,"title":"Movie","year":2020,"media_type":"movie","guid":""}]
		}}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	page, err := c.GetHistory(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if page.RecordsFiltered != 1 {
		t.Errorf("RecordsFiltered = %d, want 1", page.RecordsFiltered)
	}
	if len(page.Rows) != 1 {
		t.Errorf("Rows = %d, want 1", len(page.Rows))
	}
}

func TestGetHistory_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"error","message":"bad request"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	_, err := c.GetHistory(t.Context(), nil)
	if err == nil {
		t.Fatal("expected error for non-success result")
	}
}

// TestRetryDelayUnit pins the zero/negative/positive fallback boundary
// directly and deterministically (the retry-timing tests only exercise it
// indirectly).
func TestRetryDelayUnit(t *testing.T) {
	tests := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"zero uses default", 0, defaultRetryDelayUnit},
		{"negative uses default", -time.Second, defaultRetryDelayUnit},
		{"positive value is returned unchanged", 2 * time.Millisecond, 2 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New("http://unused", "k", http.DefaultClient)
			c.RetryDelayUnit = tt.set
			if got := c.retryDelayUnit(); got != tt.want {
				t.Errorf("retryDelayUnit() with RetryDelayUnit=%v = %v, want %v", tt.set, got, tt.want)
			}
		})
	}
}

// TestAPIWithRetry_ErrorDoesNotLeakAPIKey guards the httpx adoption: httpx's
// StatusError embeds the request URL (which carries ?apikey=), so APIWithRetry
// must redact it from returned errors.
func TestAPIWithRetry_ErrorDoesNotLeakAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "supersecretkey123", srv.Client())
	_, err := c.APIWithRetry(t.Context(), "test", nil)
	if err == nil {
		t.Fatal("expected error from exhausted retries")
	}
	if strings.Contains(err.Error(), "supersecretkey123") {
		t.Errorf("error leaked API key: %s", err.Error())
	}
}

// TestAPIWithRetry_DoesNotLogAPIKey proves the scrubbing logger keeps httpx's
// diagnostic logging (which includes the request URL with ?apikey=) from
// leaking the key end-to-end.
func TestAPIWithRetry_DoesNotLogAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	// slog.SetDefault also redirects the log package's writer and flags and
	// skips that redirect for slog's own default handler, so all three are
	// saved here and slog is restored first.
	prev, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})

	c := newTestClient(srv.URL, "supersecretkey123", srv.Client())
	_, _ = c.APIWithRetry(t.Context(), "test", nil)

	logged := buf.String()
	if strings.Contains(logged, "supersecretkey123") {
		t.Errorf("retry logging leaked API key:\n%s", logged)
	}
	if !strings.Contains(logged, "retries exhausted") {
		t.Errorf("expected httpx retry diagnostics in log output, got:\n%s", logged)
	}
}

func TestUpdateMetadata_SendsParamsAndSucceeds(t *testing.T) {
	var gotCmd, gotOld, gotNew, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCmd = r.URL.Query().Get("cmd")
		gotOld = r.URL.Query().Get("old_rating_key")
		gotNew = r.URL.Query().Get("new_rating_key")
		gotType = r.URL.Query().Get("media_type")
		w.Write([]byte(`{"response":{"result":"success"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	if err := c.UpdateMetadata(t.Context(), "100", "200", remap.Movie); err != nil {
		t.Fatal(err)
	}
	if gotCmd != "update_metadata_details" {
		t.Errorf("cmd = %q, want update_metadata_details", gotCmd)
	}
	if gotOld != "100" || gotNew != "200" || gotType != "movie" {
		t.Errorf("params: old=%q new=%q type=%q, want 100/200/movie", gotOld, gotNew, gotType)
	}
}

func TestUpdateMetadata_NonSuccessReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"error","message":"boom"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	err := c.UpdateMetadata(t.Context(), "100", "200", remap.Movie)
	if err == nil {
		t.Fatal("expected error for non-success result")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to include the API message", err)
	}
}

func TestDeleteRecentlyAdded_SendsCmdAndSucceeds(t *testing.T) {
	var gotCmd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCmd = r.URL.Query().Get("cmd")
		w.Write([]byte(`{"response":{"result":"success"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	if err := c.DeleteRecentlyAdded(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gotCmd != "delete_recently_added" {
		t.Errorf("cmd = %q, want delete_recently_added", gotCmd)
	}
}

func TestDeleteRecentlyAdded_NonSuccessReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"error","message":"nope"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	if err := c.DeleteRecentlyAdded(t.Context()); err == nil {
		t.Fatal("expected error for non-success result")
	}
}

func TestGetHistory_MalformedBodyReturnsParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"data":{"data":[`)) // truncated JSON, HTTP 200
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	_, err := c.GetHistory(t.Context(), nil)
	if err == nil {
		t.Fatal("expected a parse error for a malformed 200 body")
	}
	if !strings.Contains(err.Error(), "parsing history") {
		t.Errorf("error = %v, want it wrapped with \"parsing history\"", err)
	}
}

func TestUpdateMetadata_MalformedBodyReturnsParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	err := c.UpdateMetadata(t.Context(), "100", "200", remap.Movie)
	if err == nil {
		t.Fatal("expected a parse error for a malformed body")
	}
	if !strings.Contains(err.Error(), "parsing update_metadata_details") {
		t.Errorf("error = %v, want it wrapped with \"parsing update_metadata_details\"", err)
	}
}

// TestUpdateMetadata_APIErrorPropagates asserts that a transport/HTTP-status
// failure on the live write surfaces to the caller instead of being swallowed.
// UpdateMetadata mutates Tautulli's DB, so a failed write reported as success
// would leave history broken while telling the operator everything is fine.
func TestUpdateMetadata_APIErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	err := c.UpdateMetadata(t.Context(), "100", "200", remap.Movie)
	if err == nil {
		t.Fatal("expected UpdateMetadata to surface the transport error from a failed live write")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %v, want the API-layer HTTP 500 error (swallowing it would fall through to checkResult and report a parse error instead)", err)
	}
}

// TestGetHistory_APIWithRetryErrorPropagates asserts that when the retrying
// read exhausts (Tautulli unavailable), GetHistory surfaces that error so the
// pipeline treats Tautulli as down rather than proceeding on an empty page.
func TestGetHistory_APIWithRetryErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	_, err := c.GetHistory(t.Context(), nil)
	if err == nil {
		t.Fatal("expected GetHistory to surface the retry-exhausted error")
	}
	if strings.Contains(err.Error(), "parsing history") {
		t.Errorf("error = %v, want the propagated APIWithRetry error, not a parse error (swallowing it would fall through to json.Unmarshal of a nil body)", err)
	}
}

// TestDeleteRecentlyAdded_APIErrorPropagates asserts that a transport/HTTP
// failure surfaces to the caller instead of being swallowed as success.
func TestDeleteRecentlyAdded_APIErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	err := c.DeleteRecentlyAdded(t.Context())
	if err == nil {
		t.Fatal("expected DeleteRecentlyAdded to surface the transport error")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %v, want the API-layer HTTP 500 error (swallowing it would fall through to checkResult and report a parse error instead)", err)
	}
}

// TestGetHistory_UnknownMediaTypeRowSkipped guards the fail-open wire
// boundary: an unknown media_type row must decode without error and be
// skipped by ProcessHistoryRow, while valid rows in the same page still process.
func TestGetHistory_UnknownMediaTypeRowSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"response":{"result":"success","data":{
			"recordsFiltered":3,
			"data":[
				{"rating_key":42,"title":"Movie","year":2020,"media_type":"movie","guid":"imdb://tt1234567"},
				{"rating_key":99,"grandparent_rating_key":50,"title":"Ep","grandparent_title":"Show","year":2021,"media_type":"episode","guid":"tvdb://271557"},
				{"rating_key":7,"title":"Song","year":2019,"media_type":"track","guid":""}
			]
		}}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "k", srv.Client())
	page, err := c.GetHistory(t.Context(), nil)
	if err != nil {
		t.Fatalf("GetHistory errored on a page containing an unknown media_type: %v", err)
	}
	if len(page.Rows) != 3 {
		t.Fatalf("decoded rows = %d, want 3 (the unknown-type row must still decode)", len(page.Rows))
	}

	items := map[string]remap.TautulliEntry{}
	for i := range page.Rows {
		remap.ProcessHistoryRow(&page.Rows[i], items)
	}
	if len(items) != 2 {
		t.Fatalf("processed items = %d, want 2 (movie + show; the track row is skipped)", len(items))
	}
	if items["42"].MediaType != remap.Movie {
		t.Errorf("movie row not processed: items[42] = %+v", items["42"])
	}
	if items["50"].MediaType != remap.Show {
		t.Errorf("episode row not processed into its show: items[50] = %+v", items["50"])
	}
	if _, ok := items["7"]; ok {
		t.Errorf("track row should have been skipped, but items[7] exists: %+v", items["7"])
	}
}
