// Package main is the entry point for tautulli-remap, a tool that repairs
// Tautulli watch history after Plex library reorganizations by finding and
// updating stale rating keys.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/cplieger/health"
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/slogx"
	appconfig "github.com/cplieger/tautulli-remap/internal/config"
	"github.com/cplieger/tautulli-remap/internal/orchestrator"
	"github.com/cplieger/tautulli-remap/internal/plex"
	"github.com/cplieger/tautulli-remap/internal/tautulli"
)

// Compile-time interface satisfaction checks.
var (
	_ orchestrator.PlexClient     = (*plex.Client)(nil)
	_ orchestrator.TautulliClient = (*tautulli.Client)(nil)
)

func main() {
	slogx.Setup(slogx.Options{Level: slog.LevelInfo})

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "health":
			// Resident-idle yields Interval 0, which disables the lease: idle is
			// healthy there, and a restart cannot fix a trigger that stopped firing.
			lease := health.Lease{Interval: appconfig.RemapInterval(), Cycles: 3}
			health.RunProbe(health.DefaultPath, health.WithMaxAge(lease.Duration()))
		case "trigger":
			runTrigger()
		default:
			slog.Error("unknown subcommand", "arg", os.Args[1], "valid", "health, trigger")
			os.Exit(2)
		}
	}

	cfg, err := appconfig.Load()
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}
	cfg.Log()

	orch, err := buildOrchestrator(cfg)
	if err != nil {
		slog.Error("failed to build Plex client", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)
	defer marker.Cleanup()

	if cfg.RemapInterval > 0 {
		orch.RunScheduler(ctx, marker.Set)
		return
	}

	// Resident-idle mode: no internal timer, wait for external triggers via
	// "docker exec ... tautulli-remap trigger". Healthy while idle.
	marker.Set(true)
	slog.Info("resident-idle mode", "reason", "REMAP_INTERVAL=off, awaiting external trigger")
	<-ctx.Done()
	slog.Info("shutting down", "mode", "resident-idle", "cause", context.Cause(ctx))
}

// runTrigger executes a single remap pass and exits — the target for external
// schedulers (Ofelia job-exec, cron). os.Exit lives here, free of pending
// defers; doTrigger holds the defers and returns a code.
func runTrigger() {
	os.Exit(doTrigger())
}

// doTrigger runs one remap pass and returns the process exit code.
func doTrigger() int {
	cfg, err := appconfig.Load()
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		return 1
	}
	cfg.Log()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No marker.Cleanup() here: in resident-idle mode the main process owns
	// /tmp/.healthy, and this trigger runs as a separate `docker exec` against
	// the same file — deleting it would mark the resident container unhealthy.
	marker := health.NewMarker(health.DefaultPath)

	orch, err := buildOrchestrator(cfg)
	if err != nil {
		slog.Error("failed to build Plex client", "error", err)
		return 1
	}

	ok := orch.Run(ctx)
	return finishTrigger(ctx, ok, marker.Set)
}

// exitInterrupted is the exit code for a pass interrupted by shutdown before
// completing: distinct from 0 (unverified success) and 1 (nothing failed;
// the pass is simply incomplete and safe to re-run). Code 2 is the
// unknown-subcommand usage error.
const exitInterrupted = 3

// finishTrigger records a completed trigger run's outcome and returns the
// process exit code. It checks ctx.Err() first — a signal arriving mid-run
// can otherwise make Run's bool return either value depending on timing, so
// checking the context first makes the exit code deterministic and reports
// exitInterrupted (RunScheduler.doRun treats this the same way: neither
// success nor a counted failure). Only when the context is still live does
// Run's bool decide: success marks the resident process healthy; failure
// leaves the marker untouched (this trigger runs as a separate `docker exec`
// against the resident process's marker) and signals failure via exit code 1.
func finishTrigger(ctx context.Context, ok bool, setHealthy func(bool)) int {
	if ctx.Err() != nil {
		slog.Info("trigger interrupted by shutdown; pass incomplete", "cause", context.Cause(ctx))
		return exitInterrupted
	}
	if ok {
		setHealthy(true)
	} else {
		slog.Warn("trigger run failed; health marker left unchanged, failure signalled via exit code")
	}
	slog.Info("shutting down", "mode", "trigger", "success", ok)
	if !ok {
		return 1
	}
	return 0
}

// buildOrchestrator constructs the Plex, Tautulli, and Orchestrator instances
// from cfg. The Plex client (plexapi) owns its own hardened transport;
// Tautulli keeps the app-built one (2-minute budget, refuse-all redirects so
// the API key never rides a hostile 3xx).
func buildOrchestrator(cfg *appconfig.Config) (*orchestrator.Orchestrator, error) {
	plexClient, err := plex.New(cfg.PlexURL, plex.Token(cfg.PlexToken))
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{
		Timeout:       tautulli.ClientTimeout,
		CheckRedirect: httpx.RefuseAllRedirects,
	}
	tautulliClient := tautulli.New(cfg.TautulliURL, cfg.TautulliAPIKey, httpClient)
	return orchestrator.New(plexClient, tautulliClient, cfg), nil
}
