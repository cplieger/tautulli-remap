# Contributing to tautulli-remap

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

Every GUID a match compares goes through `remap.NormalizeGUID`, in `internal/plex` for Plex items and in `internal/remap` for history rows. A raw legacy agent GUID on one side never matches, so the item drops to the title methods or stays unmatched.
