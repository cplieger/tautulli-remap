# Configuration

This page covers the run modes, starting passes from your own scheduler, and reading the API key and token from files. Every setting and its default is in the README's [Configuration reference](../README.md#configuration-reference).

## Run modes

`REMAP_INTERVAL` decides when passes run.

- A duration, such as `24h`, `6h30m` or `30m`, turns on the built-in timer. The first pass runs when the container starts, then one runs every interval.
- `off`, `disabled`, `0` or `0s`, or no value at all, turns the timer off. The container stays running and healthy, and a pass runs only when something triggers it.
- A value that is not a valid duration, or a negative one, logs a warning and works like `off`.

In either mode you can start a pass by hand:

```sh
docker exec tautulli-remap /tautulli-remap trigger
```

The command waits for the pass and exits with its result, as listed in [Health and exit codes](how-it-works.md#health-and-exit-codes).

## Start passes from your own scheduler

Set `REMAP_INTERVAL` to `off` and let a scheduler run the trigger command. With [Ofelia](https://github.com/mcuadros/ofelia), add labels to the service. This example runs a pass at 03:00 every day:

```yaml
services:
  tautulli-remap:
    image: ghcr.io/cplieger/tautulli-remap:latest
    environment:
      REMAP_INTERVAL: "off"  # no built-in timer, wait for a trigger
      DRY_RUN: "false"
      # ... other env vars
    labels:
      ofelia.enabled: "true"
      ofelia.job-exec.tautulli-remap.schedule: "0 0 3 * * *"
      ofelia.job-exec.tautulli-remap.command: "/tautulli-remap trigger"
```

The container stays healthy between runs, and your scheduler reads each pass's exit code. A pass the scheduler starts while another pass runs is refused and exits with code 1, as [One pass at a time](how-it-works.md#one-pass-at-a-time) describes.

## Read the key and token from files

`TAUTULLI_API_KEY_FILE` and `PLEX_TOKEN_FILE` name a file that holds the value, such as a Docker or Podman secret.

- A file setting wins over the plain variable when both are set.
- One trailing line ending is removed from the file's content, and every other byte is kept.
- A file that is empty or holds only spaces and line breaks stops the container at start.
- The path must be written plainly, with no `..` and no doubled or trailing `/`.

With a compose secret named `tautulli_api_key`, set `TAUTULLI_API_KEY_FILE=/run/secrets/tautulli_api_key`, as [Secrets in files](https://github.com/cplieger/docs/blob/main/docs/hardening.md#secrets-in-files) shows.

A plain `TAUTULLI_API_KEY` or `PLEX_TOKEN` that holds only spaces is accepted. The app logs a warning at start, because Tautulli or Plex will reject it.

## Other settings

- `MAX_HISTORY_RECORDS` set to 0 or a negative number logs a warning and uses the default, 500,000.
- Logs are text at the info level, with times in UTC. `TZ` has no effect.
