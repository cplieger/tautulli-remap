# tautulli-remap

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/tautulli-remap/badges/size.json)](https://github.com/cplieger/tautulli-remap/pkgs/container/tautulli-remap) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/tautulli-remap/pkgs/container/tautulli-remap) [![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)](https://github.com/cplieger/tautulli-remap/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/tautulli-remap/badges/mutation.json)](https://github.com/cplieger/tautulli-remap/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/tautulli-remap/releases)

<!-- hub-overview BEGIN -->
tautulli-remap reconnects your Tautulli watch history and statistics to the right movies and shows after you move, re-add or reorganize your Plex libraries. It updates Tautulli only, and never changes your Plex library or your files.

## What it does

tautulli-remap keeps your Tautulli history attached to your library, in four ways:

- Finds every history entry that points at a movie or show Plex no longer knows.
- Matches each one to your library by its TMDB, TVDB, IMDb or Plex metadata ID, then by title and year.
- Repairs every season and episode of a show once it finds the show.
- Previews every change by default, and backs up Tautulli before it writes.

It can run once, once a day, or whenever your own scheduler asks.

## Who it is for

tautulli-remap is built for people who run Tautulli beside a Plex Media Server and have moved files, re-added content or rebuilt a library. Plex then gives those items new internal IDs, called rating keys, while Tautulli keeps the old ones. It covers movie and TV libraries, not music or photos. Without it, you would open each broken item from Tautulli's history and click Fix Match on its page, one item at a time.

You need a Tautulli instance and its API key, and a Plex Media Server and a Plex token for it.

tautulli-remap is free software under the GPL-3.0-or-later license.
<!-- hub-overview END -->

## Quick start

The image is on GitHub Container Registry and Docker Hub, for `amd64` and `arm64`. This is the [`compose.yaml`](compose.yaml) in this repository.

```yaml
services:
  tautulli-remap:
    image: ghcr.io/cplieger/tautulli-remap:latest
    container_name: tautulli-remap
    restart: unless-stopped

    # Put these four values in a .env file beside this file before the first start.
    environment:
      - TAUTULLI_URL  # from .env, the address you open Tautulli at, such as http://192.0.2.10:8181
      - TAUTULLI_API_KEY  # from .env, the API key on Tautulli's Settings, Web Interface page
      - PLEX_URL  # from .env, the address you open Plex at, such as http://192.0.2.10:32400
      - PLEX_TOKEN  # from .env, find it with https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/
      - "REMAP_INTERVAL=24h"  # a pass at start, then once a day. "off" runs a pass only when you trigger one
      - "DRY_RUN=true"  # preview only. Set it to false to apply the changes
```

1. Create a folder named `tautulli-remap` and save the compose block above as `compose.yaml` in it.
2. In Tautulli, open Settings, then Web Interface, and copy the API key.
3. Find your Plex token with Plex's guide, [Finding an authentication token](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/).
4. Create a file named `.env` beside `compose.yaml` with these four lines:

   ```sh
   TAUTULLI_URL=http://192.0.2.10:8181
   TAUTULLI_API_KEY=your-tautulli-api-key
   PLEX_URL=http://192.0.2.10:32400
   PLEX_TOKEN=your-plex-token
   ```

   Use the addresses you open Tautulli and Plex at from another device on your network, with `http://` and the port, not `localhost`.
5. Run `docker compose up -d`.

The first pass starts right away and only previews. Run `docker logs tautulli-remap`. You should see one `remap` line for each item it would fix, with its title, old key, new key and match method. A `scan complete` line ends the pass. If you see `failed to get history`, check the Tautulli address and API key.

## Apply the changes

When the `remap` lines look right, set `DRY_RUN=false` in `compose.yaml` and run `docker compose up -d` again. Before its first change, each pass asks Tautulli to back up its database. If that backup fails, the pass stops without changing anything.

When two Plex items share a title and year, it leaves that entry alone rather than guess. After it updates history, it clears Tautulli's recently added list, so that list shows no entries that point at old items. Tautulli fills it again from Plex. A later pass finds nothing left to fix and changes nothing. [How tautulli-remap works](docs/how-it-works.md) explains each match method.

## Configuration reference

Settings are environment variables. The container reads them when it starts, so run `docker compose up -d` after a change.

| Variable | Description | Default |
| --- | --- | --- |
| `TAUTULLI_URL` | Address of your Tautulli instance, with `http://` and the port | `http://tautulli:8181` |
| `TAUTULLI_API_KEY` | Tautulli API key, from Settings, Web Interface. `TAUTULLI_API_KEY_FILE` reads it from a file instead | required |
| `PLEX_URL` | Address of your Plex Media Server, with `http://` and the port | `http://plex:32400` |
| `PLEX_TOKEN` | Plex token for that server. `PLEX_TOKEN_FILE` reads it from a file instead | required |
| `REMAP_INTERVAL` | Time between passes, such as `24h` or `6h30m`. `off` runs a pass only when you run `tautulli-remap trigger` | `off` |
| `DRY_RUN` | `true` only logs what would change. `false` applies it | `true` |
| `FALLBACK_TITLE_YEAR` | Match by title and year when no metadata ID matches | `true` |
| `FALLBACK_TITLE_ONLY` | Match by title alone as a last resort, which can pick the wrong item | `false` |
| `MAX_HISTORY_RECORDS` | A pass stops without changes when Tautulli's history holds more entries than this | `500000` |
| `TAUTULLI_API_KEY_FILE` | File holding the Tautulli API key, such as a Docker secret. It wins over `TAUTULLI_API_KEY` | _(unset)_ |
| `PLEX_TOKEN_FILE` | File holding the Plex token, on the same terms as `TAUTULLI_API_KEY_FILE` | _(unset)_ |

tautulli-remap needs no volume and opens no port. [Configuration](docs/configuration.md) covers the run modes, starting passes from your own scheduler, and reading the key and token from files.

## Security

tautulli-remap opens no port, and connects out only to the Tautulli and Plex addresses you set. It never logs your API key or Plex token. It sends the token in a request header, and removes the key from every error it logs. Over a plain `http://` address, both cross your network unencrypted. Set `TAUTULLI_API_KEY_FILE` and `PLEX_TOKEN_FILE` to keep them out of `docker inspect`. The image runs as a non-root user on a distroless base, which has no shell. [Security](docs/hardening.md) has a hardened compose setup and lists what the image contains.

## Troubleshooting

The healthcheck reads a file the app keeps in `/tmp`. With `REMAP_INTERVAL` set to a duration, the container turns unhealthy after 3 failed passes in a row. It also turns unhealthy when no pass has succeeded for 3 intervals. The next good pass makes it healthy again. With `REMAP_INTERVAL=off`, the container stays healthy while it runs, and `tautulli-remap trigger` reports each pass through its exit code.

- The container restarts in a loop with `failed to load configuration`. `TAUTULLI_API_KEY` or `PLEX_TOKEN` did not reach it. Check `.env`, then run `docker compose up -d`.
- The log says `failed to get history`. The Tautulli address or API key is wrong, or Tautulli is down.
- The log says `aborting run: Plex returned errors`. The Plex address or token is wrong, or Plex is down. The pass stops before it changes anything.
- The log says `another remap pass is already running`. A pass started while another one ran, and was refused. Start it again once the first one ends.

## Documentation

- [How tautulli-remap works](docs/how-it-works.md) explains each pass and match method, for anyone asking why an item did or did not move.
- [Configuration](docs/configuration.md) covers run modes, external schedulers and secret files.
- [Security](docs/hardening.md) covers credential handling, a hardened compose setup and what the image contains.

## Credits

tautulli-remap repairs history through the API of [Tautulli](https://github.com/Tautulli/Tautulli), and all credit for Tautulli goes to its maintainers. Its matching order, with title-and-year and title-only fallbacks, a dry run switch and a backup before writing, follows [SwiftPanda16's Tautulli rating key update script](https://gist.github.com/JonnyWong16/f554f407832076919dc6864a78432db2).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of
every bundled component under `/usr/share/licenses/`.
