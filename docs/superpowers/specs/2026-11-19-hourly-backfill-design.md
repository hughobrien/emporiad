# Hourly history backfill — design

Issue: forgejo hughobrien/emporiad#2.

## Why

Emporia keeps hourly usage back to the monitor's install, but emporiad only
backfills the 7 days of minute data Emporia holds. Long-range views (daily
energy, cost over months) stop where emporiad started. Bringing Emporia's
hourly history into the existing hourly series fills them.

## Goals

- Every hour Emporia holds ends up in `emporia_hourly_energy_kwh` and
  `emporia_hourly_cost` for every channel, Balance included, in each target
  that accepts old samples.
- It runs by itself inside emporiad, survives restarts, and stops once
  Emporia has nothing older.
- The Energy dashboard's long-range energy panels read the hourly series.

Non-goals: a new granularity (no daily series), minute data older than
Emporia's week, fixed charges or other pricing changes.

Anti-goals: loading Emporia's backend ("slow and steady"); a separate tool
duplicating promwrite and the resume logic; any write to Emporia.

## What the probe found (2026-11-19, mains channel, read-only)

- History reaches back about seven months, to the monitor's install (daily
  data, no gaps after); a year ago is empty.
- `getChartUsage` at `scale=1H`: at most 800 h per call (HTTP 400 "larger
  than the allowed limit of PT800H"); at `1D`, 12000 h.
- 1H buckets are labelled by their start hour; an unaligned start snaps
  down to the hour. Asking `start..end` returns the buckets from start up to
  and including the one at `end`. Unlike 1MIN, the bucket at `end` is not
  wrong (it matched the same hour in a longer range), but the current hour
  comes back unfinished.
- `energyUnit=Dollars` at 1H equals `getDeviceListUsages` at 1H for the
  same hour, to every digit, in both kWh and cost: the hourly series
  can take either source.

## Approaches

1. **Backward walk inside the exporter** (chosen). Each poll, per target,
   fetch the week before the oldest hourly sample it holds, all channels,
   both units, and push it.
2. One-shot backfill tool. Rejected in the issue: duplicates promwrite and
   resume, and has to be run by hand.
3. Use `1D` for anything older. Rejected: a new granularity, and the
   dashboard would need to stitch two series.

## Design

- **Where a target's history starts.** On resume, ask each target for its
  oldest `emporia_hourly_cost{job,instance="",channel_num="1,2,3"}` sample
  (new `promwrite.Client.Oldest`, the mirror of `Newest`). With none, the
  walk starts where the forward hourly poll starts (now minus `Backfill`).
- **One week per poll** (Hugh: "slow and steady", rather than the 33 days
  the API would allow). The walk fetches the 168 hours before the target's
  oldest hour with `getChartUsage` at 1H, one call per channel per unit
  (17 channels × 2 = 34 calls today).
- **Spaced out.** Backfill calls are 20 s apart (a new `BackfillGap`,
  separate from the 2 s `CallGap` the fresh polls keep), so one poll's week
  takes about 11 minutes and finishes before the next 15-minute tick.
  About 30 polls (~7½ h) cover the history found; Emporia never sees a
  burst. Hugh: "this can be a very slow backfill".
- **Samples** sit at the end of their hour, as `pollHours` writes them, with
  the same labels. Balance is mains minus every circuit, per hour, for kWh
  and for cost; an hour where mains or any circuit is missing gets no
  Balance sample, as in `pollMinutes`.
- **Several targets** share fetches the way minutes do: fetch from the
  oldest point any unfinished target needs; each target is sent only the
  hours older than its own oldest.
- **Stop.** A week in which mains is empty in every hour ends the walk for
  that target (in memory). After a restart, the oldest-sample query puts it
  back at the start of history, and one empty mains call ends it again; the
  remaining channels are only fetched once mains has data. A target that
  refuses a backfill write (Prometheus, beyond its out-of-order window)
  also ends its walk.
- **Errors** behave like the hourly poll: an Emporia error ends this poll's
  backfill and it retries next poll; a failed write keeps that target where
  it was. Count them under the existing `usage`/`push` stages (no new
  metric family).
- **Order within a poll:** minutes and hours first, the backfill last, so it
  never delays fresh data.
- **Dashboard.** Panels that total energy over long ranges move to the
  hourly series: "Daily energy", and "Energy by circuit (time range)".
  Panels about the present (latest minute, power over time, baseload) stay
  on minutes. The hourly series lags the present by up to an hour plus
  `HourSettle`; that's acceptable for range totals.

## Left to the builder

- Chart-at-1H plumbing: extend `Client.Chart` with a scale or add a sibling;
  either way keep the "drop the bucket at end" rule only for 1MIN.
- `Oldest`'s lookback and step (hourly samples sit on the hour, so a 1h
  step inside the 5m lookback sees each one; a lookback of a few years).
- `promwrite` is identical across emporiad, kasad, comfortd and breezyd;
  `Oldest` is copied into all four (unused by the other three).
- README and CLAUDE.md "Things learned": the 1H/1D span limits and bucket
  labelling.
