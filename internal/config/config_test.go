package config

import (
	"bytes"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/envx/v2"
)

func TestLoad(t *testing.T) {
	t.Setenv("TAUTULLI_URL", "http://localhost:8181")
	t.Setenv("TAUTULLI_API_KEY", "test-key")
	t.Setenv("PLEX_URL", "http://localhost:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("DRY_RUN", "false")
	t.Setenv("FALLBACK_TITLE_YEAR", "true")
	t.Setenv("FALLBACK_TITLE_ONLY", "true")
	t.Setenv("REMAP_INTERVAL", "12h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.TautulliURL != "http://localhost:8181" {
		t.Errorf("TautulliURL = %q", cfg.TautulliURL)
	}
	if cfg.DryRun {
		t.Error("DryRun should be false")
	}
	if !cfg.FallbackTitleYear {
		t.Error("FallbackTitleYear should be true")
	}
	if !cfg.FallbackTitleOnly {
		t.Error("FallbackTitleOnly should be true")
	}
	if cfg.RemapInterval != 12*time.Hour {
		t.Errorf("RemapInterval = %v, want 12h", cfg.RemapInterval)
	}
	if cfg.TautulliAPIKey != "test-key" {
		t.Errorf("TautulliAPIKey = %q, want %q", cfg.TautulliAPIKey, "test-key")
	}
	if cfg.PlexURL != "http://localhost:32400" {
		t.Errorf("PlexURL = %q, want %q", cfg.PlexURL, "http://localhost:32400")
	}
	if cfg.PlexToken != "test-token" {
		t.Errorf("PlexToken = %q, want %q", cfg.PlexToken, "test-token")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("TAUTULLI_API_KEY", "key")
	t.Setenv("PLEX_TOKEN", "token")
	t.Setenv("DRY_RUN", "")
	t.Setenv("REMAP_INTERVAL", "")
	t.Setenv("FALLBACK_TITLE_YEAR", "")
	t.Setenv("FALLBACK_TITLE_ONLY", "")
	t.Setenv("TAUTULLI_URL", "")
	t.Setenv("PLEX_URL", "")
	t.Setenv("MAX_HISTORY_RECORDS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.TautulliURL != "http://tautulli:8181" {
		t.Errorf("TautulliURL = %q, want default", cfg.TautulliURL)
	}
	if cfg.PlexURL != "http://plex:32400" {
		t.Errorf("PlexURL = %q, want default", cfg.PlexURL)
	}
	if !cfg.DryRun {
		t.Error("DryRun should default to true")
	}
	if !cfg.FallbackTitleYear {
		t.Error("FallbackTitleYear should default to true")
	}
	if cfg.FallbackTitleOnly {
		t.Error("FallbackTitleOnly should default to false")
	}
	if cfg.MaxHistoryRecords != DefaultMaxHistoryRecords {
		t.Errorf("MaxHistoryRecords = %d, want %d (the default)", cfg.MaxHistoryRecords, DefaultMaxHistoryRecords)
	}
}

func TestLoad_MissingRequiredEnv(t *testing.T) {
	tests := []struct {
		name      string
		apiKey    string
		plexToken string
		wantKey   envx.Key
	}{
		{"missing api key", "", "token", "TAUTULLI_API_KEY"},
		{"missing plex token", "key", "", "PLEX_TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TAUTULLI_API_KEY", tt.apiKey)
			t.Setenv("PLEX_TOKEN", tt.plexToken)

			_, err := Load()
			var merr *envx.MissingError
			ok := errors.As(err, &merr)
			if !ok {
				t.Fatalf("Load() error = %v (%T), want *envx.MissingError", err, err)
			}
			if merr.Key != tt.wantKey {
				t.Errorf("MissingError.Key = %q, want %q", merr.Key, tt.wantKey)
			}
			if !strings.Contains(err.Error(), string(tt.wantKey)) {
				t.Errorf("error message %q does not mention %q", err.Error(), tt.wantKey)
			}
		})
	}
}

func TestLoadInvalidRemapInterval(t *testing.T) {
	t.Setenv("TAUTULLI_API_KEY", "key")
	t.Setenv("PLEX_TOKEN", "token")
	t.Setenv("REMAP_INTERVAL", "notaduration")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.RemapInterval != 0 {
		t.Errorf("RemapInterval = %v, want 0", cfg.RemapInterval)
	}
}

// captureLogs redirects the default slog logger into a buffer for the test and
// returns a closure yielding its contents. parseRemapInterval warns through the
// package default logger, so this lets a test assert on those side-effects.
//
// slog.SetDefault also points the log package at the installed handler and
// zeroes its flags, and skips that redirect for slog's own default handler, so
// restoring slog alone leaves log writing into a dead buffer. slog goes back
// first: reinstalling a non-default prev re-runs the redirect.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return buf.String
}

// TestParseRemapInterval covers the values parseRemapInterval accepts
// silently: the sentinels that select resident-idle mode (0), parseable
// positive durations that pass through unchanged, and parseable zero-valued
// durations. None of these may emit a REMAP_INTERVAL warning. Asserting the
// absence of a warning is what pins the sentinel switch: drop "off" from it and
// "off" falls through to time.ParseDuration, which fails and warns.
func TestParseRemapInterval(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"empty is resident-idle", "", 0},
		{"off sentinel", "off", 0},
		{"disabled sentinel", "disabled", 0},
		{"zero sentinel", "0", 0},
		{"zero-seconds sentinel", "0s", 0},
		{"uppercase OFF with surrounding spaces", "  OFF  ", 0},
		{"mixed-case Disabled", "Disabled", 0},
		{"whitespace only normalizes to empty", "   ", 0},
		{"positive duration passes through", "24h", 24 * time.Hour},
		{"compound duration passes through", "6h30m", 6*time.Hour + 30*time.Minute},
		{"surrounding spaces on a duration parse", " 24h ", 24 * time.Hour},
		{"parseable zero hours accepted silently", "0h", 0},
		{"parseable zero ms accepted silently", "0ms", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getLogs := captureLogs(t)
			got := parseRemapInterval(tt.raw)
			if got != tt.want {
				t.Errorf("parseRemapInterval(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			if logs := getLogs(); strings.Contains(logs, "REMAP_INTERVAL") {
				t.Errorf("parseRemapInterval(%q) emitted an unexpected warning: %q", tt.raw, logs)
			}
		})
	}
}

// TestParseRemapInterval_rejectsInvalid covers the values parseRemapInterval
// rejects: unparseable input and negative durations both default to off (0) and
// must warn. The negative case is the boundary partner of the silent "0h" case
// above: it pins `d < 0` against a `d <= 0` mutation, which would wrongly warn
// on and reject a zero-valued duration. The specific warning phrase
// distinguishes the two rejection reasons.
func TestParseRemapInterval_rejectsInvalid(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantWarning string
	}{
		{"unparseable value", "notaduration", "invalid REMAP_INTERVAL"},
		{"garbage with digits", "12parsecs", "invalid REMAP_INTERVAL"},
		{"negative hours", "-5h", "negative REMAP_INTERVAL"},
		{"negative compound", "-1h30m", "negative REMAP_INTERVAL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getLogs := captureLogs(t)
			got := parseRemapInterval(tt.raw)
			if got != 0 {
				t.Errorf("parseRemapInterval(%q) = %v, want 0 (off)", tt.raw, got)
			}
			if logs := getLogs(); !strings.Contains(logs, tt.wantWarning) {
				t.Errorf("parseRemapInterval(%q) missing %q warning; logs=%q", tt.raw, tt.wantWarning, logs)
			}
		})
	}
}

// TestLog_LogsRemapIntervalMode pins the interval-mode label Log emits:
// the "resident-idle" sentinel when the interval is zero, and the duration's
// String() form when scheduled. It guards the cfg.RemapInterval > 0 branch
// (a >= 0 mutation would log "0s" instead of "resident-idle").
func TestLog_LogsRemapIntervalMode(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		wantMode string
	}{
		{"resident-idle when interval is zero", 0, "resident-idle"},
		{"duration string when scheduled", 24 * time.Hour, "24h0m0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getLogs := captureLogs(t)
			(&Config{RemapInterval: tt.interval}).Log()
			if logs := getLogs(); !strings.Contains(logs, "remap_interval="+tt.wantMode) {
				t.Errorf("Log logged %q, want remap_interval=%q", logs, tt.wantMode)
			}
		})
	}
}

// TestLog_neverLogsSecrets pins the documented "API tokens are never
// logged" contract: Log emits URLs, dry-run, fallbacks, and the remap
// interval, but must never place the Tautulli API key or Plex token into a log
// attribute.
func TestLog_neverLogsSecrets(t *testing.T) {
	const (
		apiKey = "SECRET-TAUTULLI-API-KEY-sentinel"
		token  = "SECRET-PLEX-TOKEN-sentinel"
	)
	getLogs := captureLogs(t)
	(&Config{
		TautulliURL:    "http://tautulli:8181",
		TautulliAPIKey: apiKey,
		PlexURL:        "http://plex:32400",
		PlexToken:      token,
		RemapInterval:  24 * time.Hour,
	}).Log()
	logs := getLogs()
	if strings.Contains(logs, apiKey) {
		t.Errorf("Log leaked the Tautulli API key into logs: %q", logs)
	}
	if strings.Contains(logs, token) {
		t.Errorf("Log leaked the Plex token into logs: %q", logs)
	}
}

func TestLog_logsConfigAttributes(t *testing.T) {
	live := Config{
		TautulliURL:       "http://tautulli:8181",
		PlexURL:           "http://plex:32400",
		DryRun:            false,
		FallbackTitleYear: true,
		FallbackTitleOnly: true,
		RemapInterval:     24 * time.Hour,
	}
	dry := Config{
		TautulliURL:       "http://localhost:8181",
		PlexURL:           "http://localhost:32400",
		DryRun:            true,
		FallbackTitleYear: false,
		FallbackTitleOnly: false,
	}
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"live mode logs tautulli_url", live, "tautulli_url=http://tautulli:8181"},
		{"live mode logs plex_url", live, "plex_url=http://plex:32400"},
		{"live mode logs dry_run=false", live, "dry_run=false"},
		{"live mode logs fallback_title_year=true", live, "fallback_title_year=true"},
		{"live mode logs fallback_title_only=true", live, "fallback_title_only=true"},
		{"dry-run mode logs dry_run=true", dry, "dry_run=true"},
		{"dry-run mode logs fallback_title_year=false", dry, "fallback_title_year=false"},
		{"dry-run mode logs fallback_title_only=false", dry, "fallback_title_only=false"},
		{"dry-run mode logs tautulli_url", dry, "tautulli_url=http://localhost:8181"},
		{"dry-run mode logs plex_url", dry, "plex_url=http://localhost:32400"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getLogs := captureLogs(t)
			cfg := tt.cfg
			cfg.Log()
			if logs := getLogs(); !strings.Contains(logs, tt.want) {
				t.Errorf("Log missing %q; logs=%q", tt.want, logs)
			}
		})
	}
}

// TestLoad_remapIntervalDefaultsToResidentIdle pins the getEnv("REMAP_INTERVAL", "off")
// fallback: with the env var unset, Load must default to resident-idle mode
// (RemapInterval == 0), not scheduled mode. TestLoadDefaults asserts every
// other default but omits this one, leaving a mutation of the "off" fallback to
// any parseable duration undetected.
func TestLoad_remapIntervalDefaultsToResidentIdle(t *testing.T) {
	t.Setenv("TAUTULLI_API_KEY", "key")
	t.Setenv("PLEX_TOKEN", "token")
	t.Setenv("REMAP_INTERVAL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.RemapInterval != 0 {
		t.Errorf("RemapInterval = %v, want 0 (resident-idle default)", cfg.RemapInterval)
	}
}

// TestLoad_whitespaceOnlySecretWarnsButProceeds pins the branch that separates a
// missing required secret (empty value -> MissingEnvError) from a present-but-blank
// one (whitespace-only -> loaded verbatim, with a warning). A whitespace-only
// TAUTULLI_API_KEY or PLEX_TOKEN must NOT fail Load; Load returns the value
// unchanged and emits the "contains only whitespace" warning naming that key. A
// non-blank secret must load without the warning. This guards each
// strings.TrimSpace(x) == "" check against a negation mutation, which would warn
// on every real secret and stay silent on the blank ones.
func TestLoad_whitespaceOnlySecretWarnsButProceeds(t *testing.T) {
	tests := []struct {
		name          string
		apiKey        string
		token         string
		wantAPIWarn   bool
		wantTokenWarn bool
	}{
		{"non-blank secrets warn on neither", "real-key", "real-token", false, false},
		{"whitespace api key warns on tautulli only", "   ", "real-token", true, false},
		{"tab-only plex token warns on plex only", "real-key", "\t", false, true},
		{"both blank warn on both", " ", "  ", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getLogs := captureLogs(t)
			t.Setenv("TAUTULLI_API_KEY", tt.apiKey)
			t.Setenv("PLEX_TOKEN", tt.token)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want nil (a whitespace-only secret must not fail Load)", err)
			}
			if cfg.TautulliAPIKey != tt.apiKey {
				t.Errorf("TautulliAPIKey = %q, want %q loaded verbatim", cfg.TautulliAPIKey, tt.apiKey)
			}
			if cfg.PlexToken != tt.token {
				t.Errorf("PlexToken = %q, want %q loaded verbatim", cfg.PlexToken, tt.token)
			}
			logs := getLogs()
			gotAPIWarn := strings.Contains(logs, "only whitespace") && strings.Contains(logs, "key=TAUTULLI_API_KEY")
			if gotAPIWarn != tt.wantAPIWarn {
				t.Errorf("TAUTULLI_API_KEY whitespace warning = %v, want %v; logs=%q", gotAPIWarn, tt.wantAPIWarn, logs)
			}
			gotTokenWarn := strings.Contains(logs, "only whitespace") && strings.Contains(logs, "key=PLEX_TOKEN")
			if gotTokenWarn != tt.wantTokenWarn {
				t.Errorf("PLEX_TOKEN whitespace warning = %v, want %v; logs=%q", gotTokenWarn, tt.wantTokenWarn, logs)
			}
		})
	}
}

// TestLoad_maxHistoryRecordsAcceptsPositiveValues pins the pass-through half of
// the MAX_HISTORY_RECORDS sanity cap: a positive value reaches
// Config.MaxHistoryRecords unchanged, and none of these values may trigger the
// non-positive fallback warning. A cap of 1 is the smallest legal value and has
// to survive; treating it as out of range would silently replace an operator's
// deliberate cap with the 500000 default and log a cause that does not apply.
func TestLoad_maxHistoryRecordsAcceptsPositiveValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"smallest positive cap", "1", 1},
		{"modest cap", "1000", 1000},
		{"cap above the default", "2000000", 2_000_000},
		{"surrounding spaces tolerated", "  250  ", 250},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getLogs := captureLogs(t)
			t.Setenv("TAUTULLI_API_KEY", "key")
			t.Setenv("PLEX_TOKEN", "token")
			t.Setenv("MAX_HISTORY_RECORDS", tt.raw)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.MaxHistoryRecords != tt.want {
				t.Errorf("Load() with MAX_HISTORY_RECORDS=%q: MaxHistoryRecords = %d, want %d",
					tt.raw, cfg.MaxHistoryRecords, tt.want)
			}
			if logs := getLogs(); strings.Contains(logs, "non-positive MAX_HISTORY_RECORDS") {
				t.Errorf("Load() with MAX_HISTORY_RECORDS=%q warned about a non-positive value; logs=%q",
					tt.raw, logs)
			}
		})
	}
}

// TestLoad_maxHistoryRecordsFallsBackToDefault pins the fallback half of the
// MAX_HISTORY_RECORDS sanity cap. A cap of zero or below would abort every run
// at the first history page, so it is replaced by the default and warned about;
// zero belongs to the replaced side, which is what separates a cap the operator
// merely typed wrong from one they meant. An unset or malformed value arrives at
// the same default through envx's own fallback and must NOT carry the
// non-positive warning, since that warning names a cause that does not apply.
func TestLoad_maxHistoryRecordsFallsBackToDefault(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantWarn bool
	}{
		{"zero is replaced", "0", true},
		{"negative is replaced", "-1", true},
		{"large negative is replaced", "-500000", true},
		{"unset falls back silently", "", false},
		{"malformed falls back silently", "notanumber", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getLogs := captureLogs(t)
			t.Setenv("TAUTULLI_API_KEY", "key")
			t.Setenv("PLEX_TOKEN", "token")
			t.Setenv("MAX_HISTORY_RECORDS", tt.raw)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.MaxHistoryRecords != DefaultMaxHistoryRecords {
				t.Errorf("Load() with MAX_HISTORY_RECORDS=%q: MaxHistoryRecords = %d, want %d (the default)",
					tt.raw, cfg.MaxHistoryRecords, DefaultMaxHistoryRecords)
			}
			logs := getLogs()
			gotWarn := strings.Contains(logs, "non-positive MAX_HISTORY_RECORDS")
			if gotWarn != tt.wantWarn {
				t.Errorf("Load() with MAX_HISTORY_RECORDS=%q: non-positive warning = %v, want %v; logs=%q",
					tt.raw, gotWarn, tt.wantWarn, logs)
			}
		})
	}
}
