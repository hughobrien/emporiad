# emporiad

A Prometheus exporter for Emporia Vue energy monitors. The Vue has no local
interface, so emporiad logs in to Emporia's cloud with the app account, reads
Emporia's per-minute history for every circuit every 15 minutes, and pushes
each minute with remote write, at the minute's own time, to every target
given (VictoriaMetrics, or Prometheus with its receiver on). `/metrics` serves
the newest minute and hour too, plus health and device facts. Go, with only
`prometheus/client_golang`, like kasad, comfortd and breezyd.

Why push: Emporia keeps every minute, and a reading taken on one timer and
scraped on another loses whatever falls between them. (Polling every minute
did lose half of them: Emporia's "usage at an instant" call answers on a
two-minute grid.) Pushing past minutes with their timestamps keeps them all,
and the poll rate only decides how far behind the data runs.

## Screenshot

The Grafana dashboard in `grafana/emporiad.json` (circuit names replaced with
numbers):

![Grafana dashboard: attention strip, latest minute, energy and cost today, baseload, rate, stacked per-circuit power with a total line, power, energy and average daily cost by circuit](docs/screenshots/grafana.png)

## What it reads

Every device on the account, including circuit expansions nested under a Vue
and Emporia smart plugs.

Pushed, one sample per minute or hour, timestamped at the end of the
interval (and on `/metrics`, the newest of each, with its timestamp):

```
emporia_power_watts{job,device,model,channel,channel_num}        average over the minute
emporia_hourly_energy_kwh{job,device,model,channel,channel_num}  the hour's energy, from Emporia
emporia_hourly_cost{job,device,model,channel,channel_num}        the hour's cost, priced by Emporia
```

On `/metrics` only:

```
emporia_price_per_kwh{device,model}                     the flat price set in the app
emporia_device_connected{device,model}                  Emporia's online flag
emporia_device_info{device,model,firmware}
emporia_last_success_timestamp_seconds                  last poll where every call worked
emporia_newest_minute_timestamp_seconds                 end of the newest minute fetched
emporia_pushed_minute_timestamp_seconds{target}         end of the newest minute each target took
emporia_push_errors_total{target}                       failed writes, per target
emporia_poll_errors_total{stage}                        devices, resume, usage, cost or push
emporiad_build_info{version}
```

`channel` is the circuit name from the app. `channel_num` is `1,2,3` for the
mains, `1`..`16` for circuits and `Balance` for what no circuit sensor covers
(mains minus every circuit, which is how Emporia computes it). Each minute is
stored exactly once, so totals are plain sums, for example kWh today:
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
On start emporiad asks each target for the newest minute it holds and carries
on from there, so a restart leaves no gap and resends nothing; a target with
nothing gets the last week (about all Emporia keeps), 12 hours per poll
(Emporia refuses a minute chart longer than 13h20m). One
fetch serves every target, from the furthest behind, and each is sent only
what it lacks, so a target that is down catches up when it is back. (A
Prometheus target without an out-of-order window refuses samples older than
about an hour; VictoriaMetrics takes them.)

## Push and scrape

Pull still works: anything can scrape `/metrics` and get every reading's
newest sample. Each reading also pushed to a target carries
`pushed_<target>="true"`, so the target's own scrape of emporiad can drop
the copies it already has and store each minute once:

```yaml
metric_relabel_configs:
  - source_labels: [pushed_mytarget]
    regex: "true"
    action: drop
```

The NixOS module adds this to the scrape job it registers. A scraper that
takes no pushes keeps them.

## Login

Emporia uses AWS Cognito. `internal/cognito` implements the SRP login and the
token refresh with the standard library, so there is no AWS SDK. Tokens live
in memory: one login at start, a refresh every hour, and a fresh login only
when the refresh token stops working.

The credentials file has the email on the first line and the password on the
second.

## Running

```
emporiad --credentials /path/to/credentials --push local=http://127.0.0.1:8428
```

`--push name=url` can be repeated, or left out to only serve `/metrics`.
Prometheus as a target needs `--web.enable-remote-write-receiver`.

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
    prometheus.enable = true; # push to and be scraped by the local store
  };
  services.victoriametrics.enable = true;
  # Or Prometheus, with the receiver on. Set the flag once even if several
  # exporters push: Prometheus refuses it twice.
  # services.prometheus.enable = true;
  # services.prometheus.extraFlags = [ "--web.enable-remote-write-receiver" ];
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

After `nixos-rebuild switch`, the first minutes arrive within one poll;
`/metrics` is on http://127.0.0.1:9882/metrics.

| Option | Default | |
|---|---|---|
| `listen` | `"127.0.0.1:9882"` | `/metrics` and `/healthz` |
| `credentialsFile` | (required) | Emporia account file |
| `pollInterval` | `"15m"` | a Go duration |
| `push.<name>.url` | `{ }` | more remote write targets; their hosts' scrapes should drop `pushed_<name>` |
| `prometheus.enable` | `false` | push to the local `services.victoriametrics` (else `services.prometheus`) and register a scrape job that drops what it was pushed; for Prometheus, asserts `--web.enable-remote-write-receiver` |
| `prometheus.pushName` | host name, `-` and `.` as `_` | the local target's name |
| `prometheus.url` | from the local store | where to push and query |
| `prometheus.jobName`, `.scrapeInterval` | `"emporiad"`, `"60s"` | the `job` label (scraped and pushed), scrape interval |
| `extraArgs` | `[ ]` | extra flags |

`grafana/emporiad.json` is an importable dashboard.

## Development

`go test -race ./...`. `just gate` runs the CI gate (vet, gofmt, tests, race)
on a machine with nix.

The API shapes follow [PyEmVue](https://github.com/magico13/PyEmVue).

## Licence

GPL-3.0-or-later.
