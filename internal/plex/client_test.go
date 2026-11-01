package plex

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The HTTP transport (retry, redirect refusal, status mapping, body caps)
// is github.com/cplieger/plexapi/v2 and is tested there. These tests pin the
// adapter: type mapping into remap structs, GUID normalization, and the
// workflow's fail-closed rules.

// newTestClient points the adapter at an httptest server.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRejectsBadURL(t *testing.T) {
	if _, err := New("not a url\x7f", "tok"); err == nil {
		t.Error("New accepted a garbage URL")
	}
}

func TestItemExists(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantExists bool
		wantErr    bool
	}{
		{name: "200 exists", status: http.StatusOK, wantExists: true},
		{name: "404 definitively gone", status: http.StatusNotFound},
		{name: "401 undetermined fails closed", status: http.StatusUnauthorized, wantErr: true},
		{name: "500 undetermined fails closed", status: http.StatusInternalServerError, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			})
			got, err := c.ItemExists(t.Context(), "123")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.wantExists {
				t.Errorf("exists = %v, want %v", got, tt.wantExists)
			}
		})
	}
}

// TestItemExists_RejectsNonNumericKeys pins the workflow rule: a
// non-numeric key can never be a real Plex key, so it is not-exists
// (false, nil) without any request reaching the server.
func TestItemExists_RejectsNonNumericKeys(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("request reached the server for a non-numeric key")
	})
	for _, key := range []string{"", "abc", "12abc", "1 2", "12/../13"} {
		got, err := c.ItemExists(t.Context(), key)
		if err != nil || got {
			t.Errorf("ItemExists(%q) = (%v, %v), want (false, nil)", key, got, err)
		}
	}
}

func TestLibrarySections(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/sections" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[
			{"key":"1","title":"Movies","type":"movie"},
			{"key":"2","title":"TV","type":"show"},
			{"key":"3","title":"Music","type":"artist"}]}}`))
	})
	got, err := c.LibrarySections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Key != "1" || got[1].Type != "show" || got[2].Title.Raw() != "Music" {
		t.Errorf("LibrarySections = %+v", got)
	}
}

func TestLibrarySections_ErrorPropagates(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if _, err := c.LibrarySections(t.Context()); err == nil {
		t.Error("nil error on 401 — a degraded Plex must not read as an empty library")
	}
}

func TestLibraryAll(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/library/sections/7/all") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[
			{"ratingKey":"100","title":"The Matrix","year":1999,
			 "guid":"com.plexapp.agents.imdb://tt0133093?lang=en",
			 "Guid":[{"id":"imdb://tt0133093"},{"id":"tmdb://603"}]},
			{"ratingKey":"not-numeric","title":"Broken"},
			{"ratingKey":"101","title":"NoGUIDs","year":2001}]}}`))
	})
	got, err := c.LibraryAll(t.Context(), "7")
	if err != nil {
		t.Fatal(err)
	}
	// The non-numeric entry is skipped; the other two map through.
	if len(got) != 2 {
		t.Fatalf("LibraryAll = %d items, want 2 (invalid key skipped): %+v", len(got), got)
	}
	m := got[0]
	if m.RatingKey != 100 || m.Title.Raw() != "The Matrix" || m.Year != 1999 {
		t.Errorf("item = %+v", m)
	}
	// Legacy-agent GUID normalized + Guid array entries preserved.
	if len(m.GUIDs) != 3 {
		t.Errorf("GUIDs = %v, want 3 normalized entries", m.GUIDs)
	}
	for _, g := range m.GUIDs {
		if strings.Contains(g, "com.plexapp.agents") || strings.Contains(g, "?") {
			t.Errorf("unnormalized GUID leaked through: %q", g)
		}
	}
	if got[1].GUIDs != nil {
		t.Errorf("NoGUIDs item carries GUIDs: %v", got[1].GUIDs)
	}
}

func TestLibraryAll_NonNumericSectionKey(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("request reached the server for an invalid section key")
	})
	if _, err := c.LibraryAll(t.Context(), "abc"); err == nil {
		t.Error("nil error for non-numeric section key")
	}
}

func TestLibraryAll_ErrorPropagates(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if _, err := c.LibraryAll(t.Context(), "7"); err == nil {
		t.Error("nil error on 503 — a partial outage must not read as missing content")
	}
}

func TestResolveEpisodeShow(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "unanimous grandparent resolves", want: "42", body: `{"MediaContainer":{"Metadata":[
			{"grandparentRatingKey":"42"},{"grandparentRatingKey":"42"}]}}`},
		{name: "ambiguous refuses to guess", want: "", body: `{"MediaContainer":{"Metadata":[
			{"grandparentRatingKey":"42"},{"grandparentRatingKey":"43"}]}}`},
		{name: "no matches", want: "", body: `{"MediaContainer":{"Metadata":[]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/library/all" || r.URL.Query().Get("guid") == "" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(tt.body))
			})
			got, err := c.ResolveEpisodeShow(t.Context(), "plex://episode/abc123")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("show = %q, want %q", got, tt.want)
			}
		})
	}
	t.Run("empty guid short-circuits", func(t *testing.T) {
		c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
			t.Error("request sent for empty GUID")
		})
		got, err := c.ResolveEpisodeShow(t.Context(), "")
		if err != nil || got != "" {
			t.Errorf("= (%q, %v), want empty no-op", got, err)
		}
	})
	t.Run("error fails closed", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		if _, err := c.ResolveEpisodeShow(t.Context(), "plex://episode/x"); err == nil {
			t.Error("nil error on 401")
		}
	})
}
