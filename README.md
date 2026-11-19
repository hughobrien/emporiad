# emporiad

A Prometheus exporter for Emporia Vue energy monitors. The Vue has no local
interface, so emporiad logs in to Emporia's cloud with the app account, reads
Emporia's per-minute history for every circuit every 15 minutes, and pushes
each minute to Prometheus with remote write, at the minute's own time. Health
and device facts stay on `/metrics` to be scraped. Go, with only
`prometheus/client_golang`, like kasad, comfortd and breezyd.

Why push: Emporia keeps every minute, and a reading taken on one timer and
scraped on another loses whatever falls between them. (Polling every minute
did lose half of them: Emporia's "usage at an instant" call answers on a
two-minute grid.) Pushing past minutes with their timestamps keeps them all,
and the poll rate only decides how far behind the data runs.

## Screenshot

The Grafana dashboard in `grafana/emporiad.json` (circuit names replaced with
numbers):

![Grafana dashboard: attention strip, latest minute, energy and cost today, baseload, rate, whole-house and stacked per-circuit power, power and energy by circuit](docs/screenshots/grafana.png)

## What it reads

Every device on the account, including circuit expansions nested under a Vue
and Emporia smart plugs.

Pushed, one sample per minute or hour, timestamped at the end of the
interval:

```
emporia_power_watts{job,device,model,channel,channel_num}        average over the minute
emporia_hourly_energy_kwh{job,device,model,channel,channel_num}  the hour's energy, from Emporia
emporia_hourly_cost{job,device,model,channel,channel_num}        the hour's cost, priced by Emporia
```

Scraped from `/metrics`:

```
emporia_price_per_kwh{device,model}                     the flat price set in the app
emporia_device_connected{device,model}                  Emporia's online flag
emporia_device_info{device,model,firmware}
emporia_last_success_timestamp_seconds                  last poll where every call worked
emporia_newest_minute_timestamp_seconds                 end of the newest minute pushed
emporia_poll_errors_total{stage}                        devices, resume, usage, cost or push
emporiad_build_info{version}
```

`channel` is the circuit name from the app. `channel_num` is `1,2,3` for the
mains, `1`..`16` for circuits and `Balance` for what no circuit sensor covers
(mains minus every circuit, which is how Emporia computes it). Each minute is
in Prometheus exactly once, so totals are plain sums, for example kWh today:
`sum_over_time(emporia_power_watts{channel_num="1,2,3"}[1d]) / 60 / 1000`, and
cost: `sum_over_time(emporia_hourly_cost{channel_num="1,2,3"}[1d])`.

The cost is Emporia's. With a utility rate plan selected in the app, Emporia
prices usage with it (tiers over the billing cycle, riders); without one it
uses the flat price. Fixed monthly charges and optional programs on the bill
are not in it. Cost comes per finished hour, so the newest hour is missing
until it ends.

Each poll asks for every minute since the last one pushed, one Emporia call
per channel (17 for a Vue with 16 circuits), two seconds apart. Only minutes
that ended two minutes ago are taken. A minute Emporia has not filled yet is
asked for again next poll; one still empty after 30 minutes is skipped.
On start emporiad asks Prometheus for the newest minute it holds and carries
on from there, so a restart leaves no gap and resends nothing. Prometheus
refuses samples older than about an hour, so after a longer outage the older
minutes are skipped (and logged).

## Login

Emporia uses AWS Cognito. `internal/cognito` implements the SRP login and the
token refresh with the standard library, so there is no AWS SDK. Tokens live
in memory: one login at start, a refresh every hour, and a fresh login only
when the refresh token stops working.

The credentials file has the email on the first line and the password on the
second.

## Running

```
emporiad --credentials /path/to/credentials --prometheus-url http://127.0.0.1:9090
```

Prometheus needs `--web.enable-remote-write-receiver`.

## NixOS

The flake has a package and a NixOS module. Add it as an input and put the
module in your system's `modules` list:

```nix
# flake.nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    emporiad = {
      url = "github:hughobrien/emporiad";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { nixpkgs, emporiad, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        emporiad.nixosModules.default # services.emporiad.package defaults to this flake's build
        ./configuration.nix
      ];
    };
  };
}
```

Then configure it, for example in `configuration.nix`:

```nix
{
  services.emporiad = {
    enable = true;
    credentialsFile = "/etc/emporiad/credentials";
    prometheus.enable = true; # scrape job and remote write receiver, if services.prometheus is on
  };
}
```

The credentials file holds the Emporia account email on the first line and
the password on the second. systemd hands it to the service
(`LoadCredential`), so it can stay root-only:

```sh
sudo install -d -m 700 /etc/emporiad
sudo install -m 600 /dev/null /etc/emporiad/credentials
sudoedit /etc/emporiad/credentials
```

After `nixos-rebuild switch`, the first minutes reach Prometheus within one
poll; health is on http://127.0.0.1:9882/metrics.

| Option | Default | |
|---|---|---|
| `listen` | `"127.0.0.1:9882"` | `/metrics` and `/healthz` |
| `credentialsFile` | (required) | Emporia account file |
| `pollInterval` | `"15m"` | a Go duration; keep it under 45m, as Prometheus refuses samples older than about an hour |
| `prometheus.enable` | `false` | scrape job for `/metrics`, and `--web.enable-remote-write-receiver` on Prometheus |
| `prometheus.url` | from `services.prometheus` | where to push and query |
| `prometheus.jobName`, `.scrapeInterval` | `"emporiad"`, `"60s"` | the `job` label (scraped and pushed), scrape interval |
| `extraArgs` | `[ ]` | extra flags |

`grafana/emporiad.json` is an importable dashboard.

## Development

`go test -race ./...`. `just gate` runs the CI gate (vet, gofmt, tests, race)
on a machine with nix.

The API shapes follow [PyEmVue](https://github.com/magico13/PyEmVue).

## Licence

GPL-3.0-or-later.
