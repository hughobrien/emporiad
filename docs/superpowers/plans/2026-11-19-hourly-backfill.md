# Hourly History Backfill Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** emporiad walks back through Emporia's hourly history, one week per poll, and pushes it into the existing `emporia_hourly_energy_kwh` and `emporia_hourly_cost` series.

**Architecture:** A new `emporia.Client.ChartHours` reads `getChartUsage` at `scale=1H`; a new `promwrite.Client.Oldest` tells each target where its hourly history begins; a new `pollHistory` stage in the exporter runs after the fresh minutes and hours, fetches the week before the oldest hour any unfinished target holds, and sends each target the hours it lacks. The Energy dashboard's range-total panels move to the hourly series.

**Tech Stack:** Go (stdlib + `prometheus/client_golang`), NixOS module, Grafana JSON.

**Spec:** `docs/superpowers/specs/2026-11-19-hourly-backfill-design.md`

## Global Constraints

- This repo is public: no circuit, site or device names, device ids, emails or account details in commits, fixtures or the dashboard. Test fixtures are invented.
- Read-only against Emporia: only GETs.
- No new dependencies.
- No new metric family (`TestDashboardMatchesMetrics` and `exporter.Pushed` stay as they are).
- Backfill calls 20 s apart (`BackfillGap`); the fresh polls keep the 2 s `CallGap`.
- One week (168 h) per poll per walk (`HistorySpan`).
- `internal/promwrite` stays identical in emporiad, kasad, comfortd and breezyd apart from the SPDX header.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Work on branch `hourly-backfill`; open a PR on forgejo for Hugh to review.

## Review Focus

- A target that holds no hourly data at all: the walk starts exactly where the forward hourly poll starts, with no overlap and no gap.
- History start falling mid-week: the week is pushed (the non-empty hours), and only the following, wholly empty week ends the walk.
- Emporia error midway through a week's 34 calls: nothing from that week is pushed, and the next poll asks for the same week.
- Restart after the walk finished: one mains call, no other calls, nothing pushed.
- A slow poll (11 minutes of backfill) never delays fresh minutes/hours: they run first in each poll.

---

### Task 1: `ChartHours` in the Emporia client

**Files:**
- Modify: `internal/emporia/api.go`
- Test: `internal/emporia/emporia_test.go`

**Interfaces:**
- Produces: `func (c *Client) ChartHours(ctx context.Context, gid int, channel string, start, end time.Time, unit string) ([]*float64, error)` — one value per hour from `start` up to `end` (both truncated to the hour, UTC), oldest first, nil where Emporia has no data. Shorter than asked when Emporia returns fewer buckets.

- [x] **Step 1: Write the failing tests**

`TestChartHours`: server asserts `apiMethod=getChartUsage`, `scale=1H`, `start=2026-11-05T09:00:00Z`, `end=2026-11-05T12:00:00Z`, `energyUnit=Dollars`; replies `{"firstUsageInstant":"2026-11-05T09:00:00Z","usageList":[0.1,null,0.3,0.4]}`. Called with a start of 09:30Z expressed in a `-08:00` zone and end 12:00Z. Assert 3 values: `0.1, nil, 0.3` (the bucket at `end` dropped).

`TestChartHoursMisaligned`: reply with `firstUsageInstant` 08:00Z for a 09:00Z start → error.

- [x] **Step 2: Run them to see them fail**

Run: `go test ./internal/emporia -run ChartHours`
Expected: FAIL, `c.ChartHours undefined`.

- [x] **Step 3: Implement `ChartHours`**

`Chart` and `ChartHours` share one unexported helper parameterised by scale (`"1MIN"`/`"1H"`) and step (`time.Minute`/`time.Hour`). Doc comment records what the probe found: at 1H a call may span at most 800 h (HTTP 400 "PT800H"); buckets are labelled by their start hour; the bucket at `end` is right at 1H (unlike 1MIN) but dropping it keeps both the same; the current hour comes back unfinished.

- [x] **Step 4: Run the package tests**

Run: `go test -race ./internal/emporia`
Expected: PASS (TestChart and TestChartShortAndMisaligned unchanged).

- [x] **Step 5: Commit** `emporia: ChartHours, getChartUsage at 1H`

---

### Task 2: `Oldest` in promwrite

**Files:**
- Modify: `internal/promwrite/promwrite.go`, package doc comment included
- Test: `internal/promwrite/promwrite_test.go`

**Interfaces:**
- Produces: `func (c *Client) Oldest(ctx context.Context, selector string, lookback time.Duration) (t time.Time, ok bool, err error)`; query `min(min_over_time(timestamp(<selector>)[<lookback seconds>s:1h]))`. Note in its comment that the 1h step only sees samples sitting on the hour (each falls inside the 5m lookback of one step), which is where hourly samples sit.

- [x] **Step 1: Write the failing test**

`TestOldest`, shaped like `TestNewest`: reply value `"1762333200"` → `time.Unix(1762333200, 0)`, ok. Assert the query string is exactly ``min(min_over_time(timestamp(m{job="x"})[157680000s:1h]))`` for a lookback of `5*365*24*time.Hour`. An empty result → ok false, err nil.

- [x] **Step 2: Run it to see it fail**

Run: `go test ./internal/promwrite -run Oldest`
Expected: FAIL, `c.Oldest undefined`.

- [x] **Step 3: Implement**

Share the HTTP/JSON part with `Newest` through one unexported `query(ctx, q string)` helper returning `(time.Time, bool, error)`.

- [x] **Step 4: Run the package tests**

Run: `go test -race ./internal/promwrite`
Expected: PASS.

- [x] **Step 5: Commit** `promwrite: Oldest, the oldest sample a target holds`

---

### Task 3: History walk in the exporter

**Files:**
- Modify: `internal/exporter/exporter.go`
- Test: `internal/exporter/exporter_test.go`

**Interfaces:**
- Consumes: `ChartHours` (Task 1), `Oldest` (Task 2).
- Produces:
  - `API` gains `ChartHours(ctx, gid int, channel string, start, end time.Time, unit string) ([]*float64, error)`.
  - `Store` gains `Oldest(ctx, selector string, lookback time.Duration) (time.Time, bool, error)`.
  - `Config` gains `HistorySpan time.Duration` (default 168h) and `BackfillGap time.Duration` (default 20s).
  - `sink` gains `oldest time.Time` (time of the oldest hourly sample the target holds, or will hold) and `historyDone bool`.
  - `write` returns `(done, refused bool)`; existing callers use `done`.

**Behaviour (from the spec):**
- `resume` also asks `Oldest(sel("emporia_hourly_cost"), 5 years)`. Found: `s.oldest` = that time. Not found: `s.oldest = s.hour + 1h` (the first sample `pollHours` will write). An error fails resume for that target as now.
- `poll` runs `pollHistory(ctx, trees)` after `pollHours`; its result does not affect `lastOK`.
- `pollHistory`: the sinks that are ready and not `historyDone`. `to` = the latest `oldest` among them; the week is the hours whose sample time is in `[to-HistorySpan, to)`, i.e. `ChartHours(start = to-HistorySpan-1h, end = to-1h)`, sample `i` at `start + (i+1)h`.
  - First every tree's mains in kWh. If every mains value is nil: every sink with `oldest > to-HistorySpan` gets `historyDone`, log `hourly history complete` with target and time; return.
  - Then the remaining calls (mains Dollars, each circuit in both units). Sleep `BackfillGap` before every history call (including the first, after the fresh polls). Any error: `fail("usage", …)`, push nothing, return (same week next poll).
  - Series: `emporia_hourly_energy_kwh` / `emporia_hourly_cost` with `e.labels`, nil hours skipped. Balance per unit = mains − every circuit, only for hours where mains and every circuit are non-nil.
  - Each sink with `oldest > to-HistorySpan` is sent the samples with `T < s.oldest`; on `done`, `s.oldest = to-HistorySpan`; on `refused`, also `historyDone`. A failed write leaves it where it was.
  - Do not call `remember` (old samples don't belong on /metrics).
- Fakes: `fakeAPI.hourly func(num, unit string, hour time.Time) *float64` (nil func → every hour nil) and `fakeAPI.hourCharts []hourCall{gid, num, unit, start, end}`; `fakeStore.oldest map[string]time.Time` matched by name prefix like `newest`. `setup` records sleeps in a slice so spacing can be asserted. Existing tests keep passing because the default fake has no history: each poll then costs one mains call and writes nothing.

- [x] **Step 1: Write the failing tests**

Let `H0 = 2026-11-05T09:00Z` be the target's oldest hourly sample (`store.oldest["emporia_hourly_cost"] = H0`), and let history start at `H0 - 200h` (the fake returns values for hours at or after that, nil before).
  - `TestHistoryWalksBackAWeekAPoll`: first poll → hour calls start `H0-169h`, end `H0-1h`, for `1,2,3` ×2 units and circuits `1`,`2` ×2 units (6 calls, mains kWh first); pushed `emporia_hourly_energy_kwh{1,2,3}` has 168 samples, the oldest at `H0-168h`, the newest at `H0-1h`. Second poll → window ends `H0-169h`; only the 32 hours from `H0-200h` on carry samples. Third poll → one mains call, nothing pushed; fourth poll → no hour calls at all. Then a fresh exporter on the same store with `store.oldest["emporia_hourly_cost"] = H0-199h` (a restart after finishing) → its first poll makes exactly one hour call (mains kWh) and pushes no history.
  - `TestHistoryBalance`: mains 0.6 kWh/0.15 $, Oven 0.12/0.03, circuit 2 0.06/0.015 → Balance 0.42 kWh and 0.105 $ (compare within 1e-9); with circuit 2 nil for one hour, that hour has no Balance sample in either unit.
  - `TestHistoryEmptyTargetStartsWhereHoursStart`: no `oldest`, no `newest` → the history's newest sample is at `s.hour` (one hour before the first sample `pollHours` writes, at `s.hour+1h`), and no sample time appears in both the forward hourly writes and the history writes.
  - `TestHistoryErrorRetriesTheWeek`: `chartErr` for circuit `2` on the hour fake → nothing from the history pushed, `emporia_poll_errors_total{stage="usage"}` goes up; clear the error, next poll asks the same window and pushes it.
  - `TestHistoryRefusedStops`: `store.writeErr = &promwrite.RefusedError{…}` → after one poll with data, the next poll makes no hour calls.
  - `TestHistorySpacedAndLast`: every sleep before an hour call is 20s; every chart (minute) call and usage call happens before the first hour call (record call order in one shared slice).
  - `TestHistoryTwoTargets`: target A oldest `H0`, target B oldest `H0-168h` → first poll fetches the week before `H0` and only A is written; second poll fetches the week before `H0-168h` and both are written, each only samples older than its own oldest.

- [x] **Step 2: Run them to see them fail**

Run: `go test ./internal/exporter -run History`
Expected: FAIL to compile (`hourly`, `oldest` fields and the interface methods don't exist).

- [x] **Step 3: Implement as described under Behaviour**

- [x] **Step 4: Run all tests**

Run: `go test -race ./...`
Expected: PASS, including every existing exporter test unchanged.

- [x] **Step 5: Commit** `exporter: walk back through Emporia's hourly history, a week a poll (#2)`

---

### Task 4: Dashboard and docs

**Files:**
- Modify: `grafana/emporiad.json`, `README.md`, `CLAUDE.md`, `cmd/emporiad/main.go` (flag help only if it mentions call counts)

- [x] **Step 1: Dashboard**
  - "Daily energy": expr `sum(sum_over_time(emporia_hourly_energy_kwh{channel_num="1,2,3"}[1d]))`; description: kWh per UTC day, from Emporia's hourly history.
  - "Energy by circuit (time range)": expr `sort_desc(sum by (channel) (sum_over_time(emporia_hourly_energy_kwh{channel_num!~"1,2,3|Balance"}[$__range])))`; description notes the current hour is not in it yet.
  - Nothing else changes.

- [x] **Step 2: Run the dashboard test**

Run: `go test ./internal/exporter -run Dashboard`
Expected: PASS.

- [x] **Step 3: README** (the resume paragraph, ~line 65): after the forward polls, each poll also fetches one week of Emporia's hourly history, older than anything the target holds, 20 s between calls, until a week comes back empty; Prometheus targets refuse it and stop.

- [x] **Step 4: CLAUDE.md "Things learned"**: `getChartUsage` at `1H` spans at most 800 h, at `1D` 12000 h; 1H buckets are labelled by their start hour and the bucket at `end` is right; 1H Dollars equals `getDeviceListUsages` 1H; hourly history reaches back to the monitor's install (no dates: account detail). Layout line for `internal/exporter` mentions the history walk.

- [x] **Step 5: Commit** `grafana, docs: hourly history`

---

### Task 5: Siblings, deploy, verify

- [x] **Step 1:** Copy `promwrite.go` and `promwrite_test.go` into kasad, comfortd and breezyd (keeping each file's own first lines: SPDX header where present). Each: `go test -race ./...`, branch, commit `promwrite: Oldest (synced from emporiad)`, PR on forgejo.
- [x] **Step 2:** Push `hourly-backfill`, open the emporiad PR. Gate: `just gate`.
  Expected: PASS.
- [ ] **Step 3:** Deploy to bigthink once Hugh has merged the PR: in the nixos repo `nix flake update emporiad`, copy to `/etc/nixos`, build, switch (the bigthink deploy routine). No firewall change: the nftables-rules store path must not change.
- [ ] **Step 4:** Verify live:
  - journal: `resuming` lines, then the history walk's first week pushed (no errors).
  - VictoriaMetrics: `min(min_over_time(timestamp(emporia_hourly_cost{instance="",channel_num="1,2,3"})[2y:1h]))` moves back 168 h per poll.
  - Continuity: the first walked week ends where the existing hourly data begins; the "Daily energy" render shows no step or gap there. (Values themselves were checked by the probe: 1H chart equals `getDeviceListUsages` 1H.)
  - Render the two changed panels with a 6-month range and look at them.
- [ ] **Step 5:** Live dashboard: patch the two panels' expr/description on the live Energy dashboard (fetch the panel list first, patch by current index, diff live vs repo by panel id afterwards).
- [ ] **Step 6:** After about 8 hours: the journal says `hourly history complete`; the oldest sample is in the week of the monitor's install. Update this plan with an Outcome section, close issue #2 with a comment linking the commits.

## Outcome (2026-11-19)

Built on branch `hourly-backfill` (PR #8): `2db7598` design, `5c80e50` plan, `b3e6579` ChartHours, `225a196` Oldest, `46b1ada` pollHistory, `bbb1dec` dashboard and docs, `aada8b3` review fixes. Siblings: kasad#1, comfortd#22, breezyd#255. `go test -race ./...` and `just gate` pass.

Deviations from the plan:
- The history fake has its own `hourErr` map; the tests are in `internal/exporter/history_test.go`.
- History runs only when this poll's fresh minutes and hours succeeded and no push failed.
- The second walked week in TestHistoryWalksBackAWeekAPoll has 31 hours, not 32 (arithmetic).
- `ChartHours` drops the bucket at `end` at 1H too, so both scales behave alike.
- Review fixes: after an outage longer than resume's 8-day lookback, the gap between the target's old newest hour and the forward start is walked first (TestHistoryFillsALongOutage); no history while a target is down (TestHistoryWaitsForADownTarget); real figures removed from the spec.

Review minors not acted on: issue #9.

Deploy (Task 5 steps 3–6) follows the merge.
