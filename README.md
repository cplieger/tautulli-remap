# tautulli-remap

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/tautulli-remap/badges/size.json)](https://github.com/cplieger/tautulli-remap/pkgs/container/tautulli-remap) ![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue) ![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/tautulli-remap/badges/mutation.json)](https://github.com/cplieger/tautulli-remap/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/tautulli-remap/releases)

<!-- hub-overview BEGIN -->
Fix broken Tautulli watch history after reorganizing your Plex libraries.

## What it does

When you reorganize your Plex libraries (move files, re-add content, change folder structure), Plex assigns new internal IDs to your media. This breaks Tautulli's watch history: it can no longer link history entries to the right items. This tool automatically finds the correct new IDs and updates Tautulli's database, preserving your watch history and statistics.

For each stale entry, it finds the correct current rating key in Plex using a chain of strategies, most precise first:

1. **Episode-GUID resolution** (TV shows): resolves a show through one of its watched episodes' stable Plex GUIDs, which map directly to the show's current key. Exact and collision-free, and it restores the show's full watch history (all seasons and episodes).
2. **GUID match**: Plex's globally unique identifier; covers movies and shows whose history still carries a show-level GUID, for example from the legacy `thetvdb` agent.
3. **Title+year match** (fallback): matches by title and release year when no GUID resolves.
4. **Title-only with media type guard** (optional): last resort matching by title alone, restricted to the same media type to reduce false positives.

### Why this design

- **Three run modes**: `REMAP_INTERVAL` set to a Go duration like `24h` for a built-in timer, `REMAP_INTERVAL=off` for resident-idle (stays healthy, awaits `docker exec ... tautulli-remap trigger`), or `tautulli-remap trigger` for a one-shot pass that reports its outcome via its exit code.
- **Dry-run by default for safety**: no changes are applied until you explicitly set `DRY_RUN=false`, so you can always preview first.
- **Matching strategies with increasing aggressiveness**: starts with the exact ones (episode-GUID resolution for shows, GUID match for movies), falls back to title+year, and optionally title-only, giving you control over the risk/coverage tradeoff.
- **Stdlib-first, minimal dependencies**: pure Go on the standard library plus a first-party shared-lib set (`health`, `httpx`, `plexapi`, `scheduler`, `envx`, `slogx`, `runesafe`, `keyenc`) and `golang.org/x/sync`, minimizing supply-chain risk.
- **Distroless and rootless**: runs as `nonroot` on `gcr.io/distroless/static-debian13` with no shell or package manager.
<!-- hub-overview END -->

## Quick start

Images are published to both `ghcr.io/cplieger/tautulli-remap` and `docker.io/cplieger/tautulli-remap`; use whichever you prefer.

```yaml
services:
  tautulli-remap:
    image: ghcr.io/cplieger/tautulli-remap:latest
    container_name: tautulli-remap
    restart: unless-stopped

    environment:
      TAUTULLI_URL: "http://tautulli:8181"
      TAUTULLI_API_KEY: "your-tautulli-api-key"  # required
      PLEX_URL: "http://plex:32400"
      PLEX_TOKEN: "your-plex-token"  # required
      REMAP_INTERVAL: "24h"  # Go duration; "off" = resident-idle
      DRY_RUN: "true"  # set to false to apply changes
```

## Configuration reference

| Variable | Description | Default | Required |
| --- | --- | --- | --- |
| `TAUTULLI_URL` | Tautulli instance URL (Docker DNS name or LAN IP) | `http://tautulli:8181` | No |
| `TAUTULLI_API_KEY` | Tautulli API key (Settings → Web Interface → API Key). Also readable from a file: see `TAUTULLI_API_KEY_FILE` below | - | Yes |
| `PLEX_URL` | Plex Media Server URL (Docker DNS name or LAN IP) | `http://plex:32400` | No |
| `PLEX_TOKEN` | Plex authentication token (see Plex support article). Also readable from a file: see `PLEX_TOKEN_FILE` below | - | Yes |
| `TAUTULLI_API_KEY_FILE` | Path to a file holding the Tautulli API key (a Docker or Podman secret). When set it takes precedence over `TAUTULLI_API_KEY` and keeps the key out of the container environment, so it does not appear in `docker inspect`. One trailing line ending is stripped; a file holding only whitespace is rejected at startup | _(unset)_ | No |
| `PLEX_TOKEN_FILE` | Path to a file holding the Plex token, on the same terms as `TAUTULLI_API_KEY_FILE` | _(unset)_ | No |
| `REMAP_INTERVAL` | Go duration between remap runs (for example `24h`, `6h30m`). `off`/`disabled`/`0` = resident-idle (awaits external trigger via `tautulli-remap trigger`) | `off` | No |
| `FALLBACK_TITLE_YEAR` | Try title+year matching when GUID match fails | `true` | No |
| `FALLBACK_TITLE_ONLY` | Try title-only matching as last resort (risk of false matches) | `false` | No |
| `DRY_RUN` | Log what would change without applying; set to `false` to apply | `true` | No |
| `MAX_HISTORY_RECORDS` | Sanity cap on the Tautulli history size a run will process; runs abort above it. Raise it if your history is genuinely larger | `500000` | No |

## Subcommands

| Subcommand | Description |
| --- | --- |
| `tautulli-remap health` | Checks the `/tmp/.healthy` marker file. Used as the Docker `HEALTHCHECK`. Exits 0 (healthy) or 1 (unhealthy). |
| `tautulli-remap trigger` | Executes a single remap pass immediately. Exits 0 on success, 1 on failure, 3 when interrupted by shutdown before completing (retryable). Designed for `docker exec` or Ofelia `job-exec`. |

### One pass at a time

Remap passes are serialized by a cross-process lock (`/tmp/.remap.lock`): the
built-in timer, an external `trigger`, and a manual `docker exec` can never run
concurrent passes. A pass that finds another one already running refuses
immediately, before contacting Tautulli or Plex, and reports failure (a
trigger exits 1; a scheduled pass counts it toward the unhealthy threshold),
so a wedged pass surfaces through your scheduler's alerting instead of being
silently skipped. Since passes are idempotent, re-run once the active pass
finishes.

### Recommended deployment with external scheduling

Use `REMAP_INTERVAL=off` (resident-idle, one of the three [run modes](#why-this-design)) with an external scheduler like Ofelia:

```yaml
services:
  tautulli-remap:
    image: ghcr.io/cplieger/tautulli-remap:latest
    environment:
      REMAP_INTERVAL: "off"  # resident-idle, awaits trigger
      DRY_RUN: "false"
      # ... other env vars
    labels:
      ofelia.enabled: "true"
      ofelia.job-exec.tautulli-remap.schedule: "0 0 3 * * *"
      ofelia.job-exec.tautulli-remap.command: "/tautulli-remap trigger"
```

This keeps the container healthy (passing healthchecks) while delegating scheduling to Ofelia.

## Healthcheck

The container includes a built-in Docker healthcheck via the `/tautulli-remap health` subcommand, which checks for a marker file at `/tmp/.healthy`. What that marker reflects depends on the run mode:

- **Scheduled mode** (`REMAP_INTERVAL` set to a duration): the main process refreshes `/tmp/.healthy` after each run and marks the container unhealthy after 3 consecutive failed runs (Tautulli or Plex APIs unreachable, returning errors, or the remap logic failing), recovering automatically on the next successful run (including runs where nothing needs remapping).
- **Resident-idle mode** (`REMAP_INTERVAL=off`): the marker reflects the resident process's liveness. Each `tautulli-remap trigger` run reports its own outcome via its exit code (0 success / 1 failure / 3 interrupted by shutdown before completing) for the external scheduler to act on; a failed trigger deliberately does **not** mark the long-lived container unhealthy.

## Security

No network listener; the container connects outbound to Tautulli and Plex
only. Set `DRY_RUN=true` on first run to preview changes safely.

API tokens never reach the logs: the Plex token travels in the
`X-Plex-Token` header rather than the query string, and HTTP error messages
strip query parameters so the Tautulli API key cannot leak. Rating keys are
validated as numeric before URL interpolation, which blocks path traversal.
All HTTP calls use explicit timeouts and capped response bodies; transient
failures on reads are retried with bounded backoff, and mutating Tautulli
calls are never retried. The code uses no `unsafe`, `reflect`, or `os/exec`,
and its only file I/O is the health marker and run lock on `/tmp`.

The image runs as `nonroot` on a distroless base with no shell or package
manager. For a hardened deployment, add `read_only: true`, `cap_drop: [ALL]`,
`no-new-privileges:true`, and a small tmpfs for `/tmp` (16 MB covers the
health marker and run lock).

Live scan results are on the repository's Security tab. One accepted
finding: semgrep reports a single informational hit, a false positive.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Source | Role |
| --- | --- | --- |
| golang | [Go](https://hub.docker.com/_/golang) | Build image |
| gcr.io/distroless/static | [Distroless](https://github.com/GoogleContainerTools/distroless) | Runtime base image |
| health | [cplieger/health](https://github.com/cplieger/health) | File-marker healthcheck |
| httpx | [cplieger/httpx](https://github.com/cplieger/httpx) | Retrying HTTP + secret redaction (Tautulli) |
| plexapi | [cplieger/plexapi](https://github.com/cplieger/plexapi) | Plex API client |
| scheduler | [cplieger/scheduler](https://github.com/cplieger/scheduler) | Cross-process run lock |
| envx | [cplieger/envx](https://github.com/cplieger/envx) | Env var parsing |
| slogx | [cplieger/slogx](https://github.com/cplieger/slogx) | Logging setup |
| runesafe | [cplieger/runesafe](https://github.com/cplieger/runesafe) | Untrusted-string tagging (Plex titles) |
| keyenc | [cplieger/keyenc](https://github.com/cplieger/keyenc) | Escaped composite keys for the title indexes |
| golang.org/x/sync | [x/sync](https://pkg.go.dev/golang.org/x/sync) | Bounded concurrency (errgroup) |
| rapid | [pgregory.net/rapid](https://github.com/flyingmutant/rapid) | Property-based tests (test-only) |

## Credits

This is an original tool that builds upon [Tautulli](https://github.com/Tautulli/Tautulli).
Inspired by [SwiftPanda16's Tautulli rating key update script](https://gist.github.com/JonnyWong16/f554f407832076919dc6864a78432db2).

## Contributing

Issues and pull requests are welcome. Please open an issue first for
larger changes so the approach can be discussed before implementation.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of
every bundled component under `/usr/share/licenses/`.
