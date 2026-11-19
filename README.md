# emporiad

A Prometheus exporter for Emporia Vue energy monitors. The Vue has no local
interface, so emporiad logs in to Emporia's cloud with the app account, reads
the last complete minute of every circuit once a minute, and serves it at
`/metrics`. Go, with only `prometheus/client_golang`, like kasad, comfortd and
breezyd.

## What it reads

Every device on the account, including circuit expansions nested under a Vue
and Emporia smart plugs:

```
emporia_power_watts{device,model,channel,channel_num}   average over the last minute
emporia_price_per_kwh{device,model}                     the price set in the app
emporia_device_connected{device,model}                  Emporia's online flag
emporia_device_info{device,model,firmware}
emporia_last_success_timestamp_seconds
emporia_poll_errors_total{stage}                        stage is devices or usage
emporiad_build_info{version}
```

`channel` is the circuit name from the app. `channel_num` is `1,2,3` for the
mains, `1`..`16` for circuits and `Balance` for what no circuit sensor covers.
Energy and cost come from the power series in PromQL, for example kWh today:
`sum_over_time(emporia_power_watts{channel_num="1,2,3"}[1d:1m]) / 60 / 1000`.

Emporia fills in the current minute late, so emporiad asks for the minute
that ended a minute ago (`--lag`). A channel with no data is left out rather
than reported as zero, and a channel's reading is dropped once it is older
than `--stale` (5 minutes), so a cloud outage shows as a gap.

## Login

Emporia uses AWS Cognito. `internal/cognito` implements the SRP login and the
token refresh with the standard library, so there is no AWS SDK. Tokens live
in memory: one login at start, a refresh every hour, and a fresh login only
when the refresh token stops working.

The credentials file has the email on the first line and the password on the
second.

## Running

```
emporiad --credentials /path/to/credentials --listen 127.0.0.1:9882
```

On NixOS:

```nix
{
  imports = [ inputs.emporiad.nixosModules.default ];
  services.emporiad = {
    enable = true;
    credentialsFile = "/persist/emporiad/credentials"; # root-only is fine
    prometheus.enable = true;                         # adds the scrape job
  };
}
```

`grafana/emporiad.json` is an importable dashboard.

## Development

`go test -race ./...`. `just gate` runs the CI gate (vet, gofmt, tests, race)
on a machine with nix.

The API shapes follow [PyEmVue](https://github.com/magico13/PyEmVue).

## Licence

GPL-3.0-or-later.
