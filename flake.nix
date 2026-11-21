{
  description = "emporiad — Prometheus exporter for Emporia Vue energy monitors";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forEachSystem = f: nixpkgs.lib.genAttrs systems f;

      perSystem = forEachSystem (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          version = "0.1.0";

          emporiad-pkg = pkgs.buildGoModule {
            pname = "emporiad";
            inherit version;
            src = ./.;
            # When go.mod/go.sum change, set this to pkgs.lib.fakeHash, build, and
            # paste the `got:` hash.
            vendorHash = "sha256-MP1q99k5Vpi89ZYgDxcxzeL6KvOyNsIdwA7GEHgsGYA=";
            subPackages = [ "cmd/emporiad" ];
            ldflags = [ "-s" "-w" "-X main.version=${version}" ];
            doCheck = true;
            meta = with pkgs.lib; {
              description = "Prometheus exporter for Emporia Vue energy monitors";
              license = licenses.gpl3Plus;
              platforms = platforms.unix;
              mainProgram = "emporiad";
            };
          };
        in
        {
          packages = {
            default = emporiad-pkg;
            emporiad = emporiad-pkg;
          };

          apps.default = {
            type = "app";
            program = "${emporiad-pkg}/bin/emporiad";
          };

          devShells.default = pkgs.mkShell {
            packages = with pkgs; [ go gopls gotools just ];
          };

          checks.go-gate = emporiad-pkg.overrideAttrs (old: {
            pname = "emporiad-ci-gate";
            nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ pkgs.clang ];
            doCheck = true;
            checkPhase = ''
              runHook preCheck
              export HOME="$TMPDIR"
              go vet ./...
              drift=$(gofmt -l . | grep -v '^vendor/' || true)
              [ -z "$drift" ] || { echo "gofmt drift: $drift"; exit 1; }
              CGO_ENABLED=1 CC=clang go test -race ./...
              runHook postCheck
            '';
          });

          formatter = pkgs.nixpkgs-fmt;
        });

      moduleSystem = extra: nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          self.nixosModules.default
          {
            boot.isContainer = true;
            system.stateVersion = "26.05";
            networking.hostName = "my-host";
            services.emporiad = {
              enable = true;
              credentialsFile = "/run/emporia";
              prometheus.enable = true;
              push.offsite.url = "http://offsite:8428";
            };
          }
          extra
        ];
      };
      # Evaluation fails (so the check does) if the options stop doing their job.
      moduleCheck =
        let
          vm = (moduleSystem { services.victoriametrics.enable = true; }).config;
          prom = (moduleSystem {
            services.prometheus.enable = true;
            services.prometheus.extraFlags = [ "--web.enable-remote-write-receiver" ];
          }).config;
          lib = nixpkgs.lib;
          exec = c: c.systemd.services.emporiad.serviceConfig.ExecStart;
          job = jobs: lib.findFirst (j: j.job_name == "emporiad") null jobs;
          drops = j: lib.any (r: r.source_labels == [ "pushed_my_host" ] && r.regex == "true" && r.action == "drop") j.metric_relabel_configs;
        in
        assert vm.systemd.services.emporiad.serviceConfig.LoadCredential == [ "credentials:/run/emporia" ];
        assert lib.hasInfix "--push 'my_host=http://127.0.0.1:8428'" (exec vm);
        assert lib.hasInfix "--push 'offsite=http://offsite:8428'" (exec vm);
        assert drops (job vm.services.victoriametrics.prometheusConfig.scrape_configs);
        assert vm.services.prometheus.scrapeConfigs == [ ];
        assert lib.hasInfix "--push 'my_host=http://127.0.0.1:9090'" (exec prom);
        assert drops (job prom.services.prometheus.scrapeConfigs);
        vm.systemd.units."emporiad.service".unit;

      defaultModule = { pkgs, lib, ... }: {
        imports = [ ./nix/module.nix ];
        services.emporiad.package = lib.mkDefault
          self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      };
    in
    {
      nixosModules.default = defaultModule;
      nixosModules.emporiad = defaultModule;

      packages = forEachSystem (system: perSystem.${system}.packages);
      apps = forEachSystem (system: perSystem.${system}.apps);
      # CI and `just gate` build only checks.<system>.gate, so on x86_64-linux the
      # gate also pulls in the module check (evaluating it runs its assertions);
      # otherwise a broken NixOS module would pass CI. Other systems have no
      # NixOS evaluation, so their gate is just the Go gate.
      checks = forEachSystem (system:
        let
          checks = perSystem.${system}.checks;
          pkgs = nixpkgs.legacyPackages.${system};
        in
        checks // { gate = checks.go-gate; }
        // nixpkgs.lib.optionalAttrs (system == "x86_64-linux") {
          module = moduleCheck;
          gate = pkgs.linkFarm "emporiad-gate" [
            { name = "go"; path = checks.go-gate; }
            { name = "module"; path = moduleCheck; }
          ];
        });
      devShells = forEachSystem (system: perSystem.${system}.devShells);
      formatter = forEachSystem (system: perSystem.${system}.formatter);
    };
}
