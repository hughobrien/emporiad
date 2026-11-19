# emporiad — notes for agents

Prometheus exporter for Emporia Vue energy monitors, running on bigthink (Hugh's NixOS server). Siblings: `../kasad`, `../comfortd`, `../breezyd`. Follow their conventions.

## Hard rules

- **This repo is public.** No circuit names, site names, device ids, emails or account details in commits. Test fixtures are invented. Keep names out of `grafana/emporiad.json`. The only screenshot is `docs/screenshots/grafana.png`, taken from a temporary copy of the dashboard that relabels circuits `Circuit N` (label_replace on channel_num); check any new one for circuit, site and device names before committing.
- **The Emporia login** (email, then password) is `/persist/emporiad/credentials` on bigthink (root-only, on the persisted btrfs, passed in with LoadCredential); there is no copy on the Mac. For a local run, ask Hugh for a temporary file and delete it afterwards. Never print, log or commit it.
- Read-only: emporiad only GETs. Emporia's API can also switch smart plugs and EV chargers; don't add writes without Hugh agreeing.
- Dependencies: `prometheus/client_golang` only (`google.golang.org/protobuf`'s protowire, which client_golang already brings, is used directly for remote write). Ask before adding more. After a go.mod change, recompute `vendorHash` (set `pkgs.lib.fakeHash`, `just gate`, paste the `got:` hash).

## Layout

- `internal/cognito`: USER_SRP_AUTH and REFRESH_TOKEN_AUTH against Cognito's JSON API. The test's fake pool does the server half of SRP, so maths mistakes fail there.
- `internal/emporia`: `Session` (token cache, refresh, re-login), `Client` (`customers/devices`, `getChartUsage` per channel per minute, `getDeviceListUsages` per hour).
- `internal/promwrite`: remote write 1.0 by hand (protowire, literal-only snappy) and a "newest sample" query, so a restart resumes where Prometheus's data ends.
- `internal/exporter`: poll loop (every 15m: all minutes since the last pushed, then finished hours) and the `/metrics` collector (health only). `cmd/emporiad`: flags. `nix/module.nix`: the NixOS module (asserts Prometheus's remote write receiver is on; the host sets the flag once, as Prometheus refuses it repeated and kasad pushes too).
- `grafana/emporiad.json`: importable dashboard; `internal/exporter/dashboard_test.go` fails if it uses a metric that is neither served nor in `exporter.Pushed`.

## Things learned

- RespondToAuthChallenge rejects an empty `Session`; Emporia's pool does not issue one, so leave the field out.
- `getDeviceListUsages` at `scale=1MIN` answers on a two-minute grid whose phase moves: on 2026-11-19 it gave odd minutes in the morning (16:00Z asked gave 15:59) and even ones by afternoon (16:43 gave 16:42). Polling it every minute lost half the minutes. Never use it per minute; at `1H` it gives the hour containing `instant`, unfinished if current.
- `getChartUsage` (one `channel` per call; a comma list returns the sum) gives every minute, final once returned, about a week back. Its bucket for the minute containing `end` is wrong (low), so ask one minute past what you want and drop it. It only returns completed minutes. It has no `Balance` (400): Balance is mains minus every circuit, which matched Emporia's own to 4 decimals.
- Prometheus (remote write, no out-of-order window) refuses samples older than about an hour, and one refused sample fails the whole request, so never resend older than the newest it holds. The NixOS Prometheus module cannot set `out_of_order_time_window` (a `storage:` key with no flag).
- 240 V circuits have `channelMultiplier: 2` (set in the app), and usage replies already include it: Emporia's own Balance equals mains minus the returned circuit values. Don't multiply again.
- `energyUnit=Dollars` prices usage with the app's rate plan (`utilityRateGid` in locationProperties; the plan itself is not readable through the API). Checked against a year of the utility's bills: it matches the tariff per kWh except an optional per-kWh program charge, and moves between tiers by the app's billing-cycle start day. Hugh chose not to add fixed charges or optional programs.
- `/v1/...` endpoints take `Authorization: Bearer <id token>`, not `authtoken`; this account has no v1 sites.
- The circuit expansion (WAT001) has the same deviceGid as its Vue: they are one monitor. Usage replies list every channel, mains included, under that gid with the WAT001. So `Device.Parent` can equal `Device.Gid`; anything walking parents must stop at a repeat (it once spun forever).

## Commands

`go test -race ./...`; `just gate` runs the nix gate on bigthink (the Mac has no nix).
