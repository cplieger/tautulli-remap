# How tautulli-remap works

This page explains what one pass of tautulli-remap does, how it matches an item and what it changes in Tautulli. Read it when you want to know why an item did or did not move.

## Why history breaks

Plex gives every movie, show, season and episode an internal ID, its rating key. When you move files, re-add content or rebuild a library, Plex creates new items with new rating keys. Tautulli's history still points at the old keys, so those plays no longer link to anything in your library.

## One pass, step by step

1. It reads Tautulli's watch history for movies and TV episodes, 1,000 entries per request, half a second apart. Episodes are grouped under their show. If the history holds more entries than `MAX_HISTORY_RECORDS`, the pass stops here.
2. It asks Plex whether each rating key still exists, 8 at a time. A key Plex answers "not found" for is stale. Any other Plex error stops the pass, so an outage never reads as missing content.
3. It reads every movie and TV library in Plex to build its lookup tables. If one library fails to load, the pass stops, because an incomplete table could match an item to the wrong title.
4. It resolves stale shows through their episodes, then matches every stale item with the methods below.
5. With `DRY_RUN=false`, it backs up Tautulli's database and updates each match. With `DRY_RUN=true`, it only logs each match.
6. If it updated anything, it clears Tautulli's recently added list.

Each pass that gets past its checks ends with a `scan complete` line. A pass that stops early logs its reason instead. The `scan complete` line counts the entries read, the stale, matched and unmatched ones, and the updates made and failed. Every stale item no method could match gets its own `no match` line with its title, year and old key.

## Match methods, most precise first

The first method that finds a match wins.

1. Episode ID, for shows. tautulli-remap asks Plex which show now holds one of the show's watched episodes, by that episode's Plex metadata ID. It keeps up to 5 episode IDs per show to try. Tautulli's history stores each episode's own metadata ID, not the show's. This finds a show whose history holds only episode IDs, as long as one of them still resolves in Plex. The match is exact.
2. Metadata ID. The item's ID from its metadata source, such as themoviedb, thetvdb, IMDb or Plex. This covers movies, and shows whose history still carries a show-level ID, for example from the legacy `thetvdb` agent. The new item must be the same type, movie or show.
3. Title and year, when `FALLBACK_TITLE_YEAR` is `true`, the default. Titles are compared without case or surrounding spaces, within the same type.
4. Title only, when `FALLBACK_TITLE_ONLY` is `true`. It is off by default, because a remake or a reboot with the same title can match the wrong item. It stays within the same type.

No method ever returns the item's old key. Two Plex items can share one lookup value, such as the same title and year. That value is then dropped, and the entry stays unmatched rather than guessed. A shared title and year logs `title+year index shadow; refusing to match this ambiguous slot`.

## What changes in Tautulli

tautulli-remap works through Tautulli's standard API, so Tautulli's raw SQL access, the `api_sql` setting, stays off.

- Before the first update, it calls Tautulli's `backup_db` command, which saves a copy of Tautulli's database. If the backup fails, the pass stops without changing anything. A dry run or a pass with nothing to fix takes no backup.
- For each match, it calls `update_metadata_details` with the old and the new rating key. For a show, Tautulli also updates the show's seasons and episodes, so one match brings back the whole show.
- After 10 failed updates in a row, it stops the pass.
- After at least one update, it calls `delete_recently_added`, which empties Tautulli's whole recently added list. The API has no way to remove single entries. Tautulli fills the list again from Plex. A dry run logs `(dry run) would clear recently added items` instead.

A fixed item is no longer stale, so running a pass again is safe and changes nothing that is already right.

## When a pass counts as failed

A pass fails in these cases:

- another pass holds the run lock
- Tautulli or Plex returns an error before matching
- the backup fails
- 10 updates fail in a row
- clearing the recently added list fails
- updates failed and none succeeded

A pass where some updates succeed and some fail counts as a success, and the next pass retries what is left.

## One pass at a time

Passes take a lock on `/tmp/.remap.lock`, so the built-in timer, a scheduler's trigger and a manual `docker exec` never run two passes at once. A pass that finds the lock taken stops before it contacts Tautulli or Plex and logs `another remap pass is already running; refusing overlapping run`. A refused trigger exits with code 1. A refused timed pass counts toward the unhealthy threshold. Start the pass again once the running one ends.

## Health and exit codes

The image's healthcheck runs `tautulli-remap health` every 30 seconds, which exits with code 0 when healthy and 1 when not. It passes while the file `/tmp/.healthy` exists, and in timed mode while that file is also fresh.

- With `REMAP_INTERVAL` set to a duration, the app marks itself healthy at start. It does so again after each successful pass, including a pass with nothing to fix. After 3 failed passes in a row it marks itself unhealthy. If no pass succeeds for 3 intervals, the file goes stale and the healthcheck fails too. A pass that a shutdown interrupts counts as neither a success nor a failure.
- With `REMAP_INTERVAL=off`, the app marks itself healthy at start and stays healthy while it runs. A failed trigger does not change that. A successful trigger marks it healthy.

`tautulli-remap trigger` runs one pass and exits with one of these codes:

| Code | Meaning |
| --- | --- |
| `0` | The pass succeeded |
| `1` | The pass failed or was refused |
| `3` | A shutdown interrupted the pass before it finished. Run it again |
