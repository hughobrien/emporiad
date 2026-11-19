# emporiad — notes for agents

Prometheus exporter for Emporia Vue energy monitors, running on bigthink (Hugh's NixOS server). Siblings: `../kasad`, `../comfortd`, `../breezyd`. Follow their conventions.

## Hard rules

- **This repo is public.** No circuit names, site names, device ids, emails or account details in commits. Test fixtures are invented. Keep names out of `grafana/emporiad.json`.
- **The Emporia login** is the two-line file `/tmp/emporia` on the Mac (email, password) and `/persist/emporiad/credentials` on bigthink (root-only, on the persisted btrfs, passed in with LoadCredential). Never print, log or commit it.
- Read-only: emporiad only GETs. Emporia's API can also switch smart plugs and EV chargers; don't add writes without Hugh agreeing.
- Dependencies: `prometheus/client_golang` only. Ask before adding more. After a go.mod change, recompute `vendorHash` (set `pkgs.lib.fakeHash`, `just gate`, paste the `got:` hash).

## Layout

- `internal/cognito`: USER_SRP_AUTH and REFRESH_TOKEN_AUTH against Cognito's JSON API. The test's fake pool does the server half of SRP, so maths mistakes fail there.
- `internal/emporia`: `Session` (token cache, refresh, re-login), `Client` (`customers/devices`, `AppAPI?apiMethod=getDeviceListUsages`).
- `internal/exporter`: poll loop and collector. `cmd/emporiad`: flags. `nix/module.nix`: the NixOS module.
- `grafana/emporiad.json`: importable dashboard; `internal/exporter/dashboard_test.go` fails if it uses a metric the exporter does not export.

## Things learned

- RespondToAuthChallenge rejects an empty `Session`; Emporia's pool does not issue one, so leave the field out.
- Usage for the minute in progress comes back `null`; ask for one that ended a minute ago.
- 240 V circuits have `channelMultiplier: 2` (set in the app), and usage replies already include it: Emporia's own Balance equals mains minus the returned circuit values. Don't multiply again.
- `energyUnit=Dollars` prices usage with the app's rate plan (`utilityRateGid` in locationProperties; the plan itself is not readable through the API). Checked against a year of the utility's bills: it matches the tariff per kWh except an optional per-kWh program charge, and moves between tiers by the app's billing-cycle start day. Hugh chose not to add fixed charges or optional programs.
- `/v1/...` endpoints take `Authorization: Bearer <id token>`, not `authtoken`; this account has no v1 sites.
- Usage replies put every channel, mains included, under the circuit expansion's deviceGid (WAT001), not the Vue's.

## Commands

`go test -race ./...`; `just gate` runs the nix gate on bigthink (the Mac has no nix).
