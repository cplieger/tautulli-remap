# Security

This page covers how tautulli-remap handles your Tautulli API key and Plex token, a hardened compose setup, and what the image contains.

## Credentials and network

- tautulli-remap opens no port. It connects out only to the Tautulli and Plex addresses you set.
- It never logs the API key or the token. The startup line that lists the settings leaves both out.
- The Plex token travels in the `X-Plex-Token` request header, never in the address. Tautulli's API takes its key in the address, so the app removes the key from every error message and log line.
- Neither client follows a redirect, so the key and token never go to another host.
- A rating key must be a number before it goes into a request path, which blocks path traversal.
- Every request has a time limit and every answer a size limit. A Tautulli request stops after 2 minutes at most, and a Tautulli answer is read up to 30 MB.
- Reads from Tautulli and Plex are retried with growing delays, and a server's `Retry-After` is honored. Changes to Tautulli are never retried.
- Titles from Plex are treated as untrusted text before they reach the logs.
- The code uses no `unsafe`, `reflect` or `os/exec`. Besides the key and token files you name, it touches only its health file and run lock in `/tmp`.

## Hardened compose setup

These settings make the container's file system read-only, drop every Linux capability and block privilege gains. The 16 MB `/tmp` holds the health file and the run lock.

```yaml
services:
  tautulli-remap:
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - "no-new-privileges:true"
    tmpfs:
      - "/tmp:size=16m"
```

## What the image contains

The image is `gcr.io/distroless/static-debian13:nonroot` with one static Go binary, built from the `golang` Alpine image. It runs as the base image's `nonroot` user and has no shell or package manager. The binary links these modules:

- [cplieger/health](https://github.com/cplieger/health), the healthcheck file and probe
- [cplieger/httpx](https://github.com/cplieger/httpx), retrying Tautulli requests and key removal from errors
- [cplieger/plexapi](https://github.com/cplieger/plexapi), the Plex client
- [cplieger/scheduler](https://github.com/cplieger/scheduler), the run lock
- [cplieger/envx](https://github.com/cplieger/envx), reading the settings and secret files
- [cplieger/slogx](https://github.com/cplieger/slogx), the log setup
- [cplieger/runesafe](https://github.com/cplieger/runesafe), cleaning Plex titles for the logs
- [cplieger/keyenc](https://github.com/cplieger/keyenc), the keys of the title lookup tables
- [golang.org/x/sync](https://pkg.go.dev/golang.org/x/sync), running Plex requests in parallel

The tests also use [rapid](https://github.com/flyingmutant/rapid), which is not in the image. The license text of every bundled component is in the image under `/usr/share/licenses/`. [Renovate](https://github.com/renovatebot/renovate) keeps the dependencies up to date, pinned by digest or version.

Live scan results are on the repository's Security tab.
