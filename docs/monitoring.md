# Monitoring and alerts

tautulli-remap serves no metrics page and ships no alert rules. It hands each pass's verdict to whoever runs the pass. With `REMAP_INTERVAL` set to a duration, the built-in timer runs the passes, and the verdict is the container's Docker health. With your own scheduler, each `tautulli-remap trigger` exits with the pass's verdict, and the scheduler is the one party that must act on it.

## What you get

| You run | You get | You set up |
| --- | --- | --- |
| Prometheus, Mimir or VictoriaMetrics, with Alertmanager | No metrics and no alert rules from this app | Something that turns Docker health into an alert in timed mode, or an alert on your scheduler's failed jobs |
| Loki and Alertmanager, no metric store | Every pass's lines in the container log in timed mode, and no alert rules | The same, or your own log rule on the `run failed` line in timed mode |
| Docker or Unraid only | Docker health in timed mode, and the exit code of each `trigger` | Something that watches Docker health, or a scheduler that notifies you on a non-zero exit |

## Logs

tautulli-remap logs to standard error in `key=value` text, at INFO and above. Each pass that gets past its checks ends with a `scan complete` line. It counts the history entries read, the stale, matched and unmatched ones, and the updates made and failed. Every stale item no method could match gets a `no match` line with its title, year and old key, on every pass until you fix it. A pass that stops early logs its reason instead, and a failed timed pass adds a `run failed` line with `consecutive_failures`.

A pass started with `tautulli-remap trigger` writes its lines to the output of the command that ran it, which is your scheduler's log or your terminal. They never appear in `docker logs tautulli-remap`.

No alert rule reads the log. Search it by message when a pass fails. [Troubleshooting](../README.md#troubleshooting) names the usual ones.

## Knowing the app is alive

The app opens no port, so nothing outside the container can probe it.

**What healthy means.** With `REMAP_INTERVAL` set to a duration, the container is healthy while passes succeed. It turns unhealthy after 3 failed passes in a row, or when no pass has succeeded for 3 intervals, which is 72 hours at `24h`. The second case covers a pass that hangs and a timer that stopped. The next good pass makes it healthy again. With `REMAP_INTERVAL=off`, the container is healthy while it runs, and a failed trigger leaves it healthy. A scheduler that stops firing is not something the container can see. [When a pass counts as failed](how-it-works.md#when-a-pass-counts-as-failed) lists the failures.

**Docker health.** The image checks itself every 30 seconds with `tautulli-remap health`, which reads the health file the app keeps in `/tmp`. Docker marks the container unhealthy after 3 failures, and allows 15 seconds after a start. Docker's health status notifies nobody on its own, and Docker does not restart an unhealthy container.

**The exit code of a trigger.** `tautulli-remap trigger` exits 0 when the pass succeeded, 1 when it failed or another pass held the run lock, and 3 when a shutdown interrupted it. Set your scheduler to notify you on a non-zero exit, because nothing else does. A pass that hangs holds the run lock, so the next trigger is refused with exit code 1 within one schedule period. [Health and exit codes](how-it-works.md#health-and-exit-codes) explains the health file and the codes.

**Who is told.**

| You run | Passes keep failing | A pass hangs or passes stop |
| --- | --- | --- |
| The built-in timer | Docker health turns unhealthy, seen by something that watches it | The same, after 3 intervals |
| Your own scheduler | The scheduler's job result, from the exit code | The next trigger fails with exit code 1. A scheduler that stops firing is for the scheduler's own monitoring to catch |
| Docker or Unraid only, with neither watched | Nobody | Nobody |
