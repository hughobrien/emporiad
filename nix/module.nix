# SPDX-License-Identifier: GPL-3.0-or-later
#
# NixOS module for emporiad.
#
#   {
#     imports = [ inputs.emporiad.nixosModules.default ];
#     services.emporiad = {
#       enable = true;
#       credentialsFile = "/persist/emporiad/credentials";
#       prometheus.enable = true;
#     };
#   }

{ config, lib, pkgs, utils, ... }:

let
  cfg = config.services.emporiad;

  # The local metrics store, if any: VictoriaMetrics, else Prometheus. Both
  # take remote write at <url>/api/v1/write and answer PromQL at <url>/api/v1.
  vm = config.services.victoriametrics;
  prom = config.services.prometheus;
  store = if vm.enable then "victoriametrics" else if prom.enable then "prometheus" else null;
  storeUrl =
    if store == "victoriametrics" then
      "http://${lib.optionalString (lib.hasPrefix ":" vm.listenAddress) "127.0.0.1"}${vm.listenAddress}"
    else
      "http://${if prom.listenAddress == "0.0.0.0" || prom.listenAddress == "" then "127.0.0.1" else prom.listenAddress}:${toString prom.port}";
  pushLocal = cfg.prometheus.enable && store != null;
  targets = cfg.push // lib.optionalAttrs pushLocal { ${cfg.prometheus.pushName}.url = cfg.prometheus.url; };
  validName = n: builtins.match "[a-zA-Z_][a-zA-Z0-9_]*" n != null;

  # /metrics marks each reading it also pushed with pushed_<name>="true", so
  # the local store's scrape drops the copies it already has; any other
  # scraper keeps them.
  scrapeJob = {
    job_name = cfg.prometheus.jobName;
    scrape_interval = cfg.prometheus.scrapeInterval;
    static_configs = [{ targets = [ cfg.listen ]; }];
    metric_relabel_configs = lib.optional pushLocal {
      source_labels = [ "pushed_${cfg.prometheus.pushName}" ];
      regex = "true";
      action = "drop";
    };
  };

  # Generic systemd sandboxing, nothing specific to emporiad.
  sandbox = {
    NoNewPrivileges = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    PrivateTmp = true;
    PrivateDevices = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    ProtectControlGroups = true;
    ProtectClock = true;
    ProtectHostname = true;
    ProtectProc = "invisible";
    RestrictNamespaces = true;
    RestrictRealtime = true;
    RestrictSUIDSGID = true;
    LockPersonality = true;
    MemoryDenyWriteExecute = true;
    SystemCallArchitectures = "native";
  };
in
{
  options.services.emporiad = {
    enable = lib.mkEnableOption "emporiad — Prometheus exporter for Emporia Vue energy monitors";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.emporiad or (throw
        "services.emporiad.package is unset. Set it explicitly, e.g. `services.emporiad.package = inputs.emporiad.packages.\${pkgs.system}.default;`.");
      defaultText = lib.literalExpression "pkgs.emporiad";
      description = "The emporiad package providing /bin/emporiad.";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:9882";
      description = "HTTP listen address for /metrics.";
    };

    credentialsFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        File with the Emporia account: email on the first line, password on
        the second. Read by systemd (LoadCredential), so it can be root-only.
      '';
    };

    pollInterval = lib.mkOption {
      type = lib.types.str;
      default = "15m";
      description = ''
        How often to fetch the minutes since the last poll (a Go duration).
        Each poll makes one Emporia call per channel; every minute is kept
        whatever the interval, so this only sets how far behind the data runs.
      '';
    };

    push = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule {
        options.url = lib.mkOption {
          type = lib.types.str;
          example = "http://metrics.example:8428";
          description = ''
            Base URL of a remote write receiver: emporiad pushes to
            `<url>/api/v1/write` and asks `<url>/api/v1/query` where its
            data ends.
          '';
        };
      });
      default = { };
      example = lib.literalExpression ''{ offsite.url = "http://metrics.example:8428"; }'';
      description = ''
        Remote write targets, by name. /metrics labels each reading it also
        pushed `pushed_<name>="true"`; the scrape job on that target's host
        should drop those (`metric_relabel_configs`, action `drop`), as the
        one this module registers locally does. Names must be valid label
        name parts: letters, digits and `_`, not starting with a digit.
      '';
    };

    ambientStation = lib.mkOption {
      type = lib.types.nullOr (lib.types.strMatching "([0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}");
      default = null;
      example = "00:11:22:AA:BB:CC";
      description = ''
        MAC address of a weather station shared on ambientweather.net (no
        key: it is what the shared page asks with), pushed every five
        minutes as `weather_temperature_celsius{source="ambient"}`, up to 180
        days back for a target that has none. Null: no station. It locates the station; keep it out of anything public.
      '';
    };

    prometheus = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          When true and a local metrics store is enabled
          (`services.victoriametrics`, else `services.prometheus`), push to it
          as target `pushName` and register a scrape job for /metrics that
          drops the readings it was pushed. Prometheus needs
          `--web.enable-remote-write-receiver` in its `extraFlags`.
        '';
      };
      pushName = lib.mkOption {
        type = lib.types.str;
        default = lib.replaceStrings [ "-" "." ] [ "_" "_" ] config.networking.hostName;
        defaultText = lib.literalExpression ''the host name, with - and . as _'';
        description = "Target name for the local store (the `pushed_<name>` label).";
      };
      url = lib.mkOption {
        type = lib.types.str;
        default = storeUrl;
        defaultText = lib.literalExpression ''"http://<listen address of services.victoriametrics or services.prometheus>"'';
        description = "Base URL of the local store.";
      };
      jobName = lib.mkOption {
        type = lib.types.str;
        default = "emporiad";
        description = "job_name for the auto-registered scrape, and the job label on pushed series.";
      };
      scrapeInterval = lib.mkOption {
        type = lib.types.str;
        default = "60s";
        description = "Scrape interval for /metrics.";
      };
    };

    extraArgs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Extra command-line arguments for emporiad.";
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.emporiad = {
      isSystemUser = true;
      group = "emporiad";
      description = "emporiad daemon user";
    };
    users.groups.emporiad = { };

    systemd.services.emporiad = {
      description = "Emporia Vue Prometheus exporter";
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ] ++ lib.optional (store != null) "${store}.service";
      wantedBy = [ "multi-user.target" ];

      serviceConfig = sandbox // {
        # systemd's quoting, not the shell's: a % or $ in a URL must not be
        # expanded. Only %d (the credentials directory) is left for systemd.
        ExecStart = lib.concatStringsSep " " (
          map utils.escapeSystemdExecArg [
            "${cfg.package}/bin/emporiad"
            "--listen"
            cfg.listen
            "--credentials"
          ]
          ++ [ "%d/credentials" ]
          ++ map utils.escapeSystemdExecArg ([
            "--poll-interval"
            cfg.pollInterval
            "--job"
            cfg.prometheus.jobName
          ] ++ lib.concatLists (lib.mapAttrsToList (n: t: [ "--push" "${n}=${t.url}" ]) targets)
          ++ lib.optionals (cfg.ambientStation != null) [
            "--ambient-mac"
            cfg.ambientStation
          ]
          ++ cfg.extraArgs)
        );
        LoadCredential = [ "credentials:${cfg.credentialsFile}" ];
        User = "emporiad";
        Group = "emporiad";
        Restart = "on-failure";
        RestartSec = "10s";
        RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
        SystemCallFilter = [ "@system-service" "~@privileged" ];
      };
    };

    warnings = lib.optional (cfg.prometheus.enable && store == null)
      "services.emporiad.prometheus.enable is set but there is no local metrics store: enable services.victoriametrics or services.prometheus, or nothing is pushed locally and no scrape job is registered.";

    assertions = [
      {
        assertion = !(pushLocal && cfg.push ? ${cfg.prometheus.pushName});
        message = "services.emporiad.push.${cfg.prometheus.pushName} also names the local store (services.emporiad.prometheus.pushName); rename one of them.";
      }
      {
        # Prometheus refuses the flag repeated and other exporters push too,
        # so the host sets it once; only check it is there.
        assertion = !(pushLocal && store == "prometheus")
          || lib.elem "--web.enable-remote-write-receiver" prom.extraFlags;
        message = "services.emporiad.prometheus.enable pushes to Prometheus: add \"--web.enable-remote-write-receiver\" to services.prometheus.extraFlags.";
      }
      {
        assertion = lib.all validName (lib.attrNames targets);
        message = "services.emporiad push target names must be letters, digits and _, not starting with a digit: ${lib.concatStringsSep ", " (lib.attrNames targets)}";
      }
    ];

    services.prometheus.scrapeConfigs = lib.mkIf (pushLocal && store == "prometheus") [ scrapeJob ];
    services.victoriametrics.prometheusConfig.scrape_configs = lib.mkIf (pushLocal && store == "victoriametrics") [ scrapeJob ];
  };
}
