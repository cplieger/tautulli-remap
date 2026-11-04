// Package config loads and validates the application configuration from
// environment variables.
package config

import (
	"cmp"
	"log/slog"
	"strings"
	"time"

	"github.com/cplieger/go/envx"
)

// DefaultMaxHistoryRecords caps Tautulli history size a run will process;
// above it the run aborts (usually a filter regression, not a real library).
// Raise via MAX_HISTORY_RECORDS for genuinely larger histories.
const DefaultMaxHistoryRecords = 500_000

// Config holds the application configuration.
type Config struct {
	TautulliURL       string
	TautulliAPIKey    string
	PlexURL           string
	PlexToken         string
	RemapInterval     time.Duration // 0 = resident-idle (external trigger)
	MaxHistoryRecords int
	DryRun            bool
	FallbackTitleYear bool
	FallbackTitleOnly bool
}

// RemapInterval returns the effective REMAP_INTERVAL (0 = resident-idle).
// Exported separately so the health subcommand can derive its probe max-age
// without a full config load, which would fail on secrets the probe doesn't need.
func RemapInterval() time.Duration {
	return parseRemapInterval(cmp.Or(envx.String("REMAP_INTERVAL"), "off"))
}

// Load parses environment variables and returns the configuration.
func Load() (*Config, error) {
	interval := RemapInterval()

	apiKey, err := requireSecret("TAUTULLI_API_KEY")
	if err != nil {
		return nil, err
	}
	token, err := requireSecret("PLEX_TOKEN")
	if err != nil {
		return nil, err
	}

	return &Config{
		TautulliURL:       cmp.Or(envx.String("TAUTULLI_URL"), "http://tautulli:8181"),
		TautulliAPIKey:    apiKey,
		PlexURL:           cmp.Or(envx.String("PLEX_URL"), "http://plex:32400"),
		PlexToken:         token,
		RemapInterval:     interval,
		MaxHistoryRecords: maxHistoryRecords(),
		DryRun:            envx.Bool("DRY_RUN", true),
		FallbackTitleYear: envx.Bool("FALLBACK_TITLE_YEAR", true),
		FallbackTitleOnly: envx.Bool("FALLBACK_TITLE_ONLY", false),
	}, nil
}

// maxHistoryRecords falls back to the default on non-positive values, which
// would make every run abort at the first history page.
func maxHistoryRecords() int {
	n := envx.Int("MAX_HISTORY_RECORDS", DefaultMaxHistoryRecords)
	if n <= 0 {
		slog.Warn("non-positive MAX_HISTORY_RECORDS, using default",
			"value", n, "default", DefaultMaxHistoryRecords)
		return DefaultMaxHistoryRecords
	}
	return n
}

// parseRemapInterval accepts a Go duration or the sentinels
// "off"/"disabled"/"0"/"0s", all mapping to 0 (resident-idle). Unparseable
// values warn and default to off.
func parseRemapInterval(raw string) time.Duration {
	trimmed := strings.TrimSpace(raw)
	switch strings.ToLower(trimmed) {
	case "", "off", "disabled", "0", "0s":
		return 0
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		slog.Warn("invalid REMAP_INTERVAL, defaulting to off (resident-idle)",
			"value", raw, "error", err)
		return 0
	}
	if d < 0 {
		slog.Warn("negative REMAP_INTERVAL, defaulting to off", "value", raw)
		return 0
	}
	return d
}

// Log logs the active configuration at startup. It deliberately omits
// TautulliAPIKey and PlexToken: API tokens are never logged. Do not add them here.
func (cfg *Config) Log() {
	mode := "resident-idle"
	if cfg.RemapInterval > 0 {
		mode = cfg.RemapInterval.String()
	}
	slog.Info("configuration loaded",
		"tautulli_url", cfg.TautulliURL,
		"plex_url", cfg.PlexURL,
		"dry_run", cfg.DryRun,
		"fallback_title_year", cfg.FallbackTitleYear,
		"fallback_title_only", cfg.FallbackTitleOnly,
		"remap_interval", mode,
		"max_history_records", cfg.MaxHistoryRecords,
	)
}

// requireSecret reads a required secret via envx.Secret (its KEY_FILE
// companion wins when set) and returns *envx.MissingError if absent. A
// whitespace-only value is returned unchanged but warns, since it will fail
// upstream authentication.
func requireSecret(key envx.Key) (string, error) {
	v, err := envx.Secret(key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(v) == "" {
		slog.Warn("required secret is set but contains only whitespace; requests will fail authentication",
			"key", key)
	}
	return v, nil
}
