package main

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/health/v2"
	appconfig "github.com/cplieger/tautulli-remap/internal/config"
)

// TestFinishTrigger pins the exit-code contract: a cancelled context means the
// pass did not verifiably complete, so it always exits exitInterrupted (never
// 0 or 1), regardless of Run's bool. Only a live context lets Run's bool pick
// exit 0 vs 1.
func TestFinishTrigger(t *testing.T) {
	tests := []struct {
		name     string
		cancel   bool
		ok       bool
		wantCode int
	}{
		{name: "interrupted pass returns exitInterrupted", cancel: true, ok: false, wantCode: exitInterrupted},
		{name: "interruption trumps ok=true (completion not verifiable)", cancel: true, ok: true, wantCode: exitInterrupted},
		{name: "success returns 0", cancel: false, ok: true, wantCode: 0},
		{name: "failure returns 1", cancel: false, ok: false, wantCode: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Background, not t.Context(): pre-cancelled on purpose to drive
			// the exitInterrupted path.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			if code := finishTrigger(ctx, tt.ok); code != tt.wantCode {
				t.Errorf("finishTrigger code = %d, want %d", code, tt.wantCode)
			}
		})
	}
}

func TestMarkerPath_is_the_apps_own_file_in_dev_shm(t *testing.T) {
	if want := "/dev/shm/tautulli-remap.healthy"; markerPath != want {
		t.Errorf("markerPath = %q, want %q", markerPath, want)
	}
}

// fakeServers answers as Tautulli and Plex at once: one history row whose
// rating key still exists in Plex, so a pass succeeds with nothing to fix.
func fakeServers(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("cmd") == "get_history":
			_, _ = w.Write([]byte(`{"response":{"result":"success","data":{
				"recordsFiltered":1,
				"data":[{"rating_key":42,"title":"Movie","year":2020,"media_type":"movie","guid":""}]
			}}}`))
		case strings.Contains(r.URL.Path, "/library/metadata/"):
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestDoTrigger_leaves_the_resident_health_file_alone pins that a trigger,
// run as a second process beside the resident one, never writes or removes
// the health file the resident process owns.
func TestDoTrigger_leaves_the_resident_health_file_alone(t *testing.T) {
	url := fakeServers(t)
	t.Setenv("TAUTULLI_URL", url)
	t.Setenv("PLEX_URL", url)
	t.Setenv("TAUTULLI_API_KEY", "test-key")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("DRY_RUN", "true")
	t.Setenv("REMAP_INTERVAL", "off")
	// doTrigger's orchestrator takes the image's own run lock path.
	const lock = "/tmp/.remap.lock"
	if _, err := os.Stat(lock); errors.Is(err, fs.ErrNotExist) {
		t.Cleanup(func() { _ = os.Remove(lock) })
	}

	saved := markerPath
	t.Cleanup(func() { markerPath = saved })
	markerPath = filepath.Join(t.TempDir(), "tautulli-remap.healthy")
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(markerPath, old, old); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// The swapped markerPath misses a trigger that builds the real path
	// itself, so plant that file too where this host has /dev/shm.
	resident := health.MarkerPath("tautulli-remap")
	planted := plantOldFile(t, resident, old)

	if code := doTrigger(); code != 0 {
		t.Fatalf("doTrigger() = %d, want 0 for a pass with nothing to fix", code)
	}
	checks := []string{markerPath}
	if planted {
		checks = append(checks, resident)
	}
	for _, path := range checks {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("health file %s after the trigger: %v, want it left in place", path, err)
			continue
		}
		if !info.ModTime().Equal(old) {
			t.Errorf("health file %s modified at %v, want it untouched at %v", path, info.ModTime(), old)
		}
	}
}

// plantOldFile creates path with mtime old and removes it at cleanup. It
// reports false, planting nothing, when the folder is missing or the file
// already exists, so a real resident process's file is never touched.
func plantOldFile(t *testing.T, path string, old time.Time) bool {
	t.Helper()
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Logf("skipping %s: %v", path, err)
		return false
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Logf("skipping %s: %v", path, err)
		return false
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if err := f.Close(); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	return true
}

// TestRunResident_keeps_the_health_file_while_it_runs_and_removes_it_at_shutdown
// covers both resident modes: the file appears while the process runs and is
// gone by the time runResident returns.
func TestRunResident_keeps_the_health_file_while_it_runs_and_removes_it_at_shutdown(t *testing.T) {
	for _, tt := range []struct {
		name     string
		interval time.Duration
	}{
		{name: "timed", interval: time.Hour},
		{name: "resident-idle", interval: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			url := fakeServers(t)
			cfg := &appconfig.Config{TautulliURL: url, TautulliAPIKey: "test-key", PlexURL: url, PlexToken: "test-token", DryRun: true, RemapInterval: tt.interval}
			orch, err := buildOrchestrator(cfg)
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			orch.RunLockPath = filepath.Join(t.TempDir(), "remap.lock")
			marker := filepath.Join(t.TempDir(), "tautulli-remap.healthy")

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				runResident(ctx, cfg, orch, marker)
			}()

			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					cancel()
					<-done
					t.Fatalf("health file %s never appeared while runResident ran", marker)
				}
				time.Sleep(5 * time.Millisecond)
			}

			cancel()
			<-done
			if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("health file after shutdown: %v, want it removed", err)
			}
		})
	}
}
