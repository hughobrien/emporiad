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

{ config, lib, pkgs, ... }:

let
  cfg = config.services.emporiad;

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
        Keep it under 45m: Prometheus refuses samples older than about an hour.
      '';
    };

    prometheus = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          When true and `services.prometheus.enable` is also true, register
          a scrape job for emporiad's `/metrics` endpoint (health and device
          facts). emporiad pushes its per-minute data with remote write
          either way, so Prometheus needs `--web.enable-remote-write-receiver`
          in its `extraFlags`.
        '';
      };
      url = lib.mkOption {
        type = lib.types.str;
        default =
          let p = config.services.prometheus;
          in "http://${if p.listenAddress == "0.0.0.0" || p.listenAddress == "" then "127.0.0.1" else p.listenAddress}:${toString p.port}";
        defaultText = lib.literalExpression ''"http://''${services.prometheus.listenAddress}:''${toString services.prometheus.port}"'';
        description = ''
          Prometheus base URL. emporiad pushes to `<url>/api/v1/write` and
          queries `<url>/api/v1/query` to find where its last run stopped.
          The receiver must be on (`--web.enable-remote-write-receiver`).
        '';
      };
      jobName = lib.mkOption {
        type = lib.types.str;
        default = "emporiad";
        description = "Prometheus job_name for the auto-registered scrape.";
      };
      scrapeInterval = lib.mkOption {
        type = lib.types.str;
        default = "60s";
        description = "Scrape interval for /metrics, which only carries health and device facts.";
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
      after = [ "network-online.target" ] ++ lib.optional config.services.prometheus.enable "prometheus.service";
      wantedBy = [ "multi-user.target" ];

      serviceConfig = sandbox // {
        ExecStart = lib.concatStringsSep " " ([
          "${cfg.package}/bin/emporiad"
          "--listen"
          (lib.escapeShellArg cfg.listen)
          "--credentials"
          "%d/credentials"
          "--poll-interval"
          cfg.pollInterval
          "--prometheus-url"
          (lib.escapeShellArg cfg.prometheus.url)
          "--job"
          (lib.escapeShellArg cfg.prometheus.jobName)
        ] ++ map lib.escapeShellArg cfg.extraArgs);
        LoadCredential = [ "credentials:${cfg.credentialsFile}" ];
        User = "emporiad";
        Group = "emporiad";
        Restart = "on-failure";
        RestartSec = "10s";
        RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
        SystemCallFilter = [ "@system-service" "~@privileged" ];
      };
    };

    # emporiad pushes its per-minute data with remote write. The receiver
    # flag is set once by the host (Prometheus refuses a repeated flag, and
    # other exporters push too), so only check it is there.
    assertions = [{
      assertion = !(cfg.prometheus.enable && config.services.prometheus.enable)
        || lib.elem "--web.enable-remote-write-receiver" config.services.prometheus.extraFlags;
      message = "services.emporiad.prometheus.enable pushes data: add \"--web.enable-remote-write-receiver\" to services.prometheus.extraFlags.";
    }];

    # Auto-wire a Prometheus scrape job when both services are enabled.
    services.prometheus.scrapeConfigs = lib.mkIf
      (cfg.prometheus.enable && config.services.prometheus.enable) [{
      job_name = cfg.prometheus.jobName;
      scrape_interval = cfg.prometheus.scrapeInterval;
      static_configs = [{ targets = [ cfg.listen ]; }];
    }];
  };
}
