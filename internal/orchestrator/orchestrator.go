// Package orchestrator coordinates the tautulli-remap workflow, driving
// history collection, stale-key detection, Plex library indexing, matching,
// and metadata updates across the Tautulli and Plex APIs.
package orchestrator

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/tautulli-remap/internal/config"
	"github.com/cplieger/tautulli-remap/internal/remap"
	"github.com/cplieger/tautulli-remap/internal/tautulli"
	"golang.org/x/sync/errgroup"
)

const (
	maxConsecutiveFailures = 10
	plexParallelism        = 8
)

// defaultRunLockPath is the flock(2) lock file serializing remap passes
// across processes: the resident process and every `docker exec … trigger`
// child contend on the same container-private /tmp tmpfs path.
const defaultRunLockPath = "/tmp/.remap.lock"

// historyPageSize is the number of Tautulli history rows requested per page;
// the pagination cursor advances by the same amount.
const historyPageSize = 1000

// circuitBreaker tracks consecutive failures and trips when the threshold is reached.
type circuitBreaker struct {
	threshold   int
	consecutive int
}

// newCircuitBreaker creates a breaker that trips after threshold consecutive failures.
func newCircuitBreaker(threshold int) *circuitBreaker {
	return &circuitBreaker{threshold: threshold}
}

// record registers a success (ok=true) or failure (ok=false) and reports whether the breaker has tripped.
func (cb *circuitBreaker) record(ok bool) bool {
	if ok {
		cb.consecutive = 0
		return false
	}
	cb.consecutive++
	return cb.consecutive >= cb.threshold
}

// defaultPaginationDelay is the default pause between history pages.
const defaultPaginationDelay = 500 * time.Millisecond

// schedulerUnhealthyThreshold is the number of consecutive failed scheduled
// runs RunScheduler tolerates before flipping the health marker unhealthy.
// Damps transient Plex/Tautulli blips so a single failed run does not flap
// the container.
const schedulerUnhealthyThreshold = 3

// PlexClient defines the interface for Plex API interactions, shaped by
// what the orchestrator needs.
type PlexClient interface {
	ItemExists(ctx context.Context, ratingKey string) (bool, error)
	LibrarySections(ctx context.Context) ([]remap.Section, error)
	LibraryAll(ctx context.Context, sectionKey string) ([]remap.LibItem, error)
	ResolveEpisodeShow(ctx context.Context, episodeGUID string) (string, error)
}

// TautulliClient defines the interface for Tautulli API interactions, shaped
// by what the orchestrator needs.
type TautulliClient interface {
	APIWithRetry(ctx context.Context, cmd string, params url.Values) ([]byte, error)
	GetHistory(ctx context.Context, params url.Values) (*tautulli.HistoryPage, error)
	UpdateMetadata(ctx context.Context, oldKey, newKey string, mediaType remap.MediaType) error
	DeleteRecentlyAdded(ctx context.Context) error
}

// Orchestrator coordinates the remap workflow.
type Orchestrator struct {
	plex     PlexClient
	tautulli TautulliClient
	cfg      *config.Config

	// RunLockPath is the cross-process run lock file. Empty uses the default.
	RunLockPath string

	// PaginationDelay is the pause between history pages. Zero uses the default.
	PaginationDelay time.Duration
}

// New creates a new Orchestrator.
func New(p PlexClient, t TautulliClient, cfg *config.Config) *Orchestrator {
	return &Orchestrator{plex: p, tautulli: t, cfg: cfg}
}

// createBackup makes a Tautulli backup before any mutation; skipped (and
// reports success) in dry-run mode. In live mode a failed backup returns
// false so the caller aborts without mutating Tautulli — there would be no
// recovery point.
func (o *Orchestrator) createBackup(ctx context.Context) bool {
	if o.cfg.DryRun {
		slog.Info("dry run enabled, skipping backup")
		return true
	}
	slog.Info("creating Tautulli backup")
	if _, err := o.tautulli.APIWithRetry(ctx, "backup_db", nil); err != nil {
		if ctx.Err() != nil {
			slog.Info("backup interrupted by shutdown; aborting run", "cause", context.Cause(ctx))
		} else {
			slog.Error("backup failed after retries; aborting run without mutating Tautulli (no recovery point)",
				"error", err)
		}
		return false
	}
	return true
}

// Run executes the remap workflow. Returns true on success.
//
// A cross-process flock serializes passes (the scheduled loop, an Ofelia
// job-exec trigger, and a manual `docker exec … trigger` all run Run in
// potentially separate processes sharing /tmp; Ofelia's no-overlap covers
// only its own job). A pass that finds the lock held refuses immediately,
// before any Tautulli or Plex call, and reports failure.
func (o *Orchestrator) Run(ctx context.Context) bool {
	lock, ok := o.acquireRunLock()
	if !ok {
		return false
	}
	defer lock.Unlock()

	// Step 1: Collect items from Tautulli history
	slog.Info("step 1: collecting items from Tautulli history")
	tautulliItems, episodeGUIDsCaptured := o.collectTautulliItems(ctx)
	if tautulliItems == nil {
		return false
	}
	slog.Info("step 1 done",
		"unique_items", len(tautulliItems),
		"episode_guids_captured", episodeGUIDsCaptured)
	if ctx.Err() != nil {
		slog.Info("run cancelled after history collection", "cause", context.Cause(ctx))
		return false
	}

	// Step 2: Find stale keys
	slog.Info("step 2: checking keys against Plex")
	stale, err := o.findStaleKeys(ctx, tautulliItems)
	if err != nil {
		if ctx.Err() != nil {
			slog.Info("run cancelled during stale check", "cause", context.Cause(ctx))
		} else {
			slog.Error("aborting run: Plex returned errors during stale-key check", "error", err)
		}
		return false
	}
	slog.Info("step 2 done", "stale", len(stale), "total", len(tautulliItems))
	if ctx.Err() != nil {
		slog.Info("run cancelled during stale check", "cause", context.Cause(ctx))
		return false
	}

	if len(stale) == 0 {
		slog.Info("all rating keys are valid, nothing to remap")
		logScanComplete(len(tautulliItems), 0, 0, 0, 0, 0, o.cfg.DryRun)
		return true
	}

	// Step 3: Build Plex library index
	idx, ok := o.buildIndex(ctx)
	if !ok {
		return false
	}

	// Step 3.5: resolve stale shows to their current key via episode GUID —
	// exact and collision-free; the index heuristics below remain as
	// fallbacks for movies and legacy shows.
	slog.Info("step 3.5: resolving shows via episode GUID")
	resolved := o.resolveStaleShows(ctx, stale)

	// Step 4: Match stale items
	slog.Info("step 4: matching stale items")
	matched, unmatched := remap.MatchStaleItems(stale, resolved, idx,
		remap.Fallbacks{TitleYear: o.cfg.FallbackTitleYear, TitleOnly: o.cfg.FallbackTitleOnly})

	// Step 4.5: backup, deferred until at least one mapping is ready so a
	// pass with nothing to mutate never spends one. It still precedes the
	// first write (applyRemappings and the recently-added clear).
	if len(matched) > 0 && !o.createBackup(ctx) {
		return false
	}

	// Step 5: Apply remappings
	updated, failed, aborted := o.applyRemappings(ctx, matched, unmatched)

	// A shutdown during apply must not report success: applyMatched returns
	// aborted=false on cancellation, so without this guard failed==0 would
	// read as a successful pass.
	if ctx.Err() != nil {
		slog.Info("run cancelled during remap phase", "cause", context.Cause(ctx))
		return false
	}

	// Step 6: Clear recently added (see clearIfNeeded for the policy).
	cleared := o.clearIfNeeded(ctx, updated, len(matched))

	logScanComplete(len(tautulliItems), len(stale), len(matched), len(unmatched), updated, failed, o.cfg.DryRun)

	// Success when nothing failed, or when at least one update landed: a
	// partial remap still made progress and must not flap the health marker.
	// Deliberately NOT strict-on-failures: per-item failures are usually
	// transient, and failing 499-of-500 would alert nightly on near-success;
	// a genuinely poisoned item surfaces once it becomes the pass's only work
	// (failed>0, updated==0). A tripped circuit breaker (aborted) always
	// fails the run. A failed recently-added clear also fails it — the next
	// pass retries while stale entries remain.
	return !aborted && cleared && (failed == 0 || updated > 0)
}

// acquireRunLock takes the cross-process run lock. ok=false (with the reason
// already logged) means another pass is in flight or the lock file is broken;
// either way the caller must not proceed. The returned lock is non-nil only
// when ok is true.
func (o *Orchestrator) acquireRunLock() (*scheduler.Lock, bool) {
	lock, ok, err := scheduler.TryLock(o.runLockPath())
	if err != nil {
		slog.Error("cannot acquire run lock", "path", o.runLockPath(), "error", err)
		return nil, false
	}
	if !ok {
		logRunRefused(o.runLockPath())
		return nil, false
	}
	return lock, true
}

// clearIfNeeded runs the recently-added clear when the pass's outcome calls
// for it: live mode clears only when an update landed (idempotency), and
// dry-run previews the clear when matches exist so the preview does not hide
// a mutation a live run performs. Returns false when a needed clear failed.
func (o *Orchestrator) clearIfNeeded(ctx context.Context, updated, matched int) bool {
	switch {
	case updated > 0:
		return o.clearRecentlyAdded(ctx)
	case o.cfg.DryRun && matched > 0:
		return o.clearRecentlyAdded(ctx)
	default:
		slog.Info("skipping clear recently added", "reason", "no_updates")
		return true
	}
}

// runLockPath returns the configured run lock path or the default.
func (o *Orchestrator) runLockPath() string {
	if o.RunLockPath != "" {
		return o.RunLockPath
	}
	return defaultRunLockPath
}

// logRunRefused logs the refusal of an overlapping pass, including how long
// the current holder has been running when the lock file's holder timestamp
// is readable (observability only; correctness never depends on it).
func logRunRefused(path string) {
	if since, known := scheduler.ReadHolder(path); known {
		slog.Warn("another remap pass is already running; refusing overlapping run",
			"lock", path, "holder_age", time.Since(since).Round(time.Second).String())
		return
	}
	slog.Warn("another remap pass is already running; refusing overlapping run", "lock", path)
}

// buildIndex builds the Plex library index used for matching. Returns
// ok=false (reason logged) when the run must abort before matching: context
// cancellation, any failed library section, or a completely empty index.
func (o *Orchestrator) buildIndex(ctx context.Context) (remap.Index, bool) {
	slog.Info("step 3: building Plex library index")
	idx, failedSections := remap.BuildPlexIndex(ctx, o.plex, plexParallelism)
	if ctx.Err() != nil {
		slog.Info("run cancelled during plex indexing", "cause", context.Cause(ctx))
		return remap.Index{}, false
	}
	// Check failedSections before the all-empty check: a partial outage
	// yields a non-empty but incomplete index (a stale item whose correct
	// entry lived in a failed section could false-match a same-title+year
	// twin elsewhere), while a total outage yields an empty index the
	// all-empty guard would otherwise misreport as "library is empty".
	if failedSections > 0 {
		slog.Error("aborting run: Plex returned errors for some library sections",
			"failed_sections", failedSections)
		return remap.Index{}, false
	}
	if idx.Empty() {
		slog.Error("Plex library index is empty, cannot match stale items")
		return remap.Index{}, false
	}
	return idx, true
}

// RunScheduler implements the long-running scheduled mode. The setHealthy
// callback controls the health marker.
func (o *Orchestrator) RunScheduler(ctx context.Context, setHealthy func(bool)) {
	interval := o.cfg.RemapInterval
	slog.Info("scheduled mode", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	setHealthy(true)

	failures := 0
	doRun := func() {
		ok := o.Run(ctx)
		if ctx.Err() != nil {
			// Shutdown interrupted the run; don't count it toward the
			// damping threshold or flip the marker (deferred Cleanup removes
			// it on exit).
			return
		}
		if ok {
			failures = 0
			setHealthy(true)
			slog.Info("run complete",
				"next_run_at", time.Now().Add(interval).UTC().Format(time.RFC3339))
			return
		}
		failures++
		slog.Warn("run failed",
			"consecutive_failures", failures,
			"retry_in", interval)
		if failures >= schedulerUnhealthyThreshold {
			setHealthy(false)
		}
	}

	doRun()

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down", "mode", "scheduled", "cause", context.Cause(ctx))
			return
		case <-ticker.C:
			doRun()
		}
	}
}

// collectTautulliItems retrieves all unique items from Tautulli watch history,
// returning a map of rating key to entry. Episodes are stored under their
// grandparent (show) key; their episode-scoped plex:// GUIDs are retained on
// the show entry (for later resolution) and counted in episodeGUIDsCaptured.
func (o *Orchestrator) collectTautulliItems(ctx context.Context) (items map[string]remap.TautulliEntry, episodeGUIDsCaptured int) {
	items = map[string]remap.TautulliEntry{}
	start := 0
	total := -1
	processed := 0

	for {
		page, ok := o.fetchHistoryPage(ctx, start)
		if !ok {
			return nil, 0
		}

		if total < 0 {
			total = page.RecordsFiltered
			slog.Info("total history records", "count", total)
			if total > o.maxHistoryRecords() {
				slog.Error("history record count exceeds sanity cap; raise MAX_HISTORY_RECORDS if this history is genuine",
					"count", total, "max", o.maxHistoryRecords())
				return nil, 0
			}
		}

		if len(page.Rows) == 0 {
			break
		}

		episodeGUIDsCaptured += addHistoryPage(page, items)
		processed += len(page.Rows)

		start += historyPageSize
		if start >= total {
			break
		}
		slog.Info("progress", "processed", processed, "total", total, "unique_keys", len(items))
		if err := httpx.SleepCtx(ctx, o.paginationDelay()); err != nil {
			return nil, 0
		}
	}

	return items, episodeGUIDsCaptured
}

// fetchHistoryPage requests one page of Tautulli history starting at start. On
// error it logs (distinguishing shutdown from a real failure) and returns
// ok=false so the caller stops paginating.
func (o *Orchestrator) fetchHistoryPage(ctx context.Context, start int) (*tautulli.HistoryPage, bool) {
	params := url.Values{
		"grouping":         {"0"},
		"include_activity": {"0"},
		"media_type":       {"movie,episode"},
		"order_column":     {"date"},
		"order_dir":        {"desc"},
		"start":            {strconv.Itoa(start)},
		"length":           {strconv.Itoa(historyPageSize)},
	}
	page, err := o.tautulli.GetHistory(ctx, params)
	if err != nil {
		if ctx.Err() != nil {
			slog.Info("history collection interrupted by shutdown", "cause", context.Cause(ctx))
		} else {
			slog.Error("failed to get history", "error", err)
		}
		return nil, false
	}
	return page, true
}

// addHistoryPage processes one page of Tautulli history rows into items,
// returning the number of episode GUIDs captured for later show resolution.
func addHistoryPage(page *tautulli.HistoryPage, items map[string]remap.TautulliEntry) int {
	captured := 0
	for i := range page.Rows {
		if remap.ProcessHistoryRow(&page.Rows[i], items) {
			captured++
		}
	}
	return captured
}

// findStaleKeys checks each item in the Tautulli history map against the Plex
// API and returns only the entries whose rating keys no longer exist in Plex.
// A non-nil error means at least one Plex check failed in a way that could
// not be resolved (a real outage, not a 404); the first such error cancels
// the remaining checks. The (possibly partial) stale map is returned
// alongside the error for diagnostics but must not be trusted.
func (o *Orchestrator) findStaleKeys(ctx context.Context, items map[string]remap.TautulliEntry) (map[string]remap.TautulliEntry, error) {
	var mu sync.Mutex
	stale := map[string]remap.TautulliEntry{}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(plexParallelism)

	for key, item := range items {
		g.Go(func() error {
			if gctx.Err() != nil {
				return nil
			}
			exists, err := o.plex.ItemExists(gctx, key)
			if err != nil {
				return err
			}
			if !exists {
				mu.Lock()
				stale[key] = item
				mu.Unlock()
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return stale, err
	}

	slog.Info("stale key check complete", "checked", len(items), "stale", len(stale))
	return stale, nil
}

// resolveStaleShows resolves stale Show entries to their current Plex rating
// key through one of their retained episode GUIDs. Movies and shows without
// episode GUIDs are skipped. Resolution is best-effort: a lookup error for
// one show falls through to the index-based fallbacks rather than aborting
// the run; a genuine Plex outage is already caught by the stale-key check and
// the index build.
func (o *Orchestrator) resolveStaleShows(ctx context.Context, stale map[string]remap.TautulliEntry) map[string]string {
	var mu sync.Mutex
	resolved := map[string]string{}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(plexParallelism)

	for oldKey, item := range stale {
		if item.MediaType != remap.Show || len(item.EpisodeGUIDs) == 0 {
			continue
		}
		g.Go(func() error {
			if gctx.Err() != nil {
				return nil
			}
			newKey := o.resolveOneShow(gctx, item.EpisodeGUIDs)
			if newKey != "" && newKey != oldKey {
				mu.Lock()
				resolved[oldKey] = newKey
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()

	if len(resolved) > 0 {
		slog.Info("resolved shows via episode GUID", "count", len(resolved))
	}
	return resolved
}

// resolveOneShow tries each episode GUID in turn and returns the first show
// rating key it resolves to, or "" if none resolve.
func (o *Orchestrator) resolveOneShow(ctx context.Context, episodeGUIDs []string) string {
	for _, guid := range episodeGUIDs {
		if ctx.Err() != nil {
			return ""
		}
		showKey, err := o.plex.ResolveEpisodeShow(ctx, guid)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("episode GUID resolution failed", "guid", guid, "error", err)
			}
			continue
		}
		if showKey != "" {
			return showKey
		}
	}
	return ""
}

// applyRemappings updates Tautulli metadata for each matched item and logs
// all unmatched items. Returns the count of successfully updated records, the
// count of failures, and whether the run was aborted by the consecutive-
// failure circuit breaker. In dry-run mode it logs what would change without
// writing to Tautulli.
func (o *Orchestrator) applyRemappings(
	ctx context.Context,
	matched []remap.MatchResult,
	unmatched []remap.UnmatchResult,
) (updated, failed int, aborted bool) {
	logUnmatched(unmatched)

	if len(matched) == 0 {
		slog.Info("no matches found for stale items")
		return 0, 0, false
	}

	if o.cfg.DryRun {
		slog.Info("dry run: would remap items", "count", len(matched))
	} else {
		slog.Info("remapping items", "count", len(matched))
	}

	return o.applyMatched(ctx, matched)
}

// logUnmatched logs every stale item that no strategy could match.
func logUnmatched(unmatched []remap.UnmatchResult) {
	if len(unmatched) == 0 {
		return
	}
	slog.Info("unmatched items", "count", len(unmatched))
	for _, u := range unmatched {
		slog.Warn("no match",
			"title", u.Title, "year", u.Year, "type", u.MediaType, "key", u.OldKey)
	}
}

// logScanComplete emits the end-of-run summary line. Both the nothing-to-remap
// early return and the full-run path log through it so the field set stays in a
// single place.
func logScanComplete(total, stale, matched, unmatched, updated, failed int, dryRun bool) {
	slog.Info("scan complete",
		"total", total,
		"stale", stale,
		"matched", matched,
		"unmatched", unmatched,
		"updated", updated,
		"failed", failed,
		"dry_run", dryRun)
}

// logRemap emits the per-item remap line. A title-only match may land on an
// entry with a different year; the transition rides a dedicated matched_year
// field appended only when it is informative (Method stays a closed enum).
func (o *Orchestrator) logRemap(m *remap.MatchResult) {
	attrs := []any{
		"title", m.Title, "year", m.Year, "type", m.MediaType,
		"old_key", m.OldKey, "new_key", m.NewKey,
		"method", m.Method, "dry_run", o.cfg.DryRun,
	}
	if m.MatchedYear != "" && m.MatchedYear != m.Year {
		attrs = append(attrs, "matched_year", m.MatchedYear)
	}
	slog.Info("remap", attrs...)
}

// applyMatched updates Tautulli metadata for each matched item, honoring
// dry-run mode, context cancellation, and the consecutive-failure circuit
// breaker. Returns the counts of updated and failed records, and whether the
// breaker tripped (aborted), which fails the run regardless of progress.
func (o *Orchestrator) applyMatched(ctx context.Context, matched []remap.MatchResult) (updated, failed int, aborted bool) {
	breaker := newCircuitBreaker(maxConsecutiveFailures)
	for i, m := range matched {
		o.logRemap(&m)
		if o.cfg.DryRun {
			continue
		}
		if ctx.Err() != nil {
			slog.Warn("remapping interrupted", "remaining", len(matched)-i)
			break
		}
		if err := o.tautulli.UpdateMetadata(ctx, m.OldKey, m.NewKey, m.MediaType); err != nil {
			if ctx.Err() != nil {
				slog.Warn("remapping interrupted", "remaining", len(matched)-i)
				break
			}
			slog.Error("remap failed",
				"title", m.Title, "old_key", m.OldKey, "error", err)
			failed++
			if breaker.record(false) {
				slog.Error("aborting remap phase",
					"reason", "too_many_consecutive_failures",
					"consecutive", breaker.consecutive,
					"remaining", len(matched)-i-1)
				return updated, failed, true
			}
			continue
		}
		updated++
		breaker.record(true)
	}
	return updated, failed, false
}

// clearRecentlyAdded removes all entries from Tautulli's recently-added table
// to prevent stale entries from appearing in the UI after a remap. No-op in
// dry-run mode. Returns false when the live clear failed, so the run result
// reflects the incomplete cleanup rather than silently reporting success.
func (o *Orchestrator) clearRecentlyAdded(ctx context.Context) bool {
	if o.cfg.DryRun {
		slog.Info("(dry run) would clear recently added items")
		return true
	}
	slog.Info("clearing recently added items")
	if err := o.tautulli.DeleteRecentlyAdded(ctx); err != nil {
		if ctx.Err() != nil {
			slog.Info("clear recently added interrupted by shutdown", "cause", context.Cause(ctx))
		} else {
			slog.Error("failed to clear recently added", "error", err)
		}
		return false
	}
	return true
}

// paginationDelay returns the configured pagination delay or the default.
func (o *Orchestrator) paginationDelay() time.Duration {
	if o.PaginationDelay > 0 {
		return o.PaginationDelay
	}
	return defaultPaginationDelay
}

// maxHistoryRecords returns the configured history sanity cap, falling back
// to the default when the config carries no positive value (a zero-value
// Config must not abort every run at the first page).
func (o *Orchestrator) maxHistoryRecords() int {
	if o.cfg.MaxHistoryRecords > 0 {
		return o.cfg.MaxHistoryRecords
	}
	return config.DefaultMaxHistoryRecords
}
