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

          checks.gate = emporiad-pkg.overrideAttrs (old: {
            pname = "emporiad-ci-gate";
            nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ [ pkgs.clang ];
            doCheck = true;
            checkPhase = ''
              runHook preCheck
              export HOME="$TMPDIR"
              go vet ./...
              drift=$(gofmt -l . | grep -v '^vendor/' || true)
              [ -z "$drift" ] || { echo "gofmt drift: $drift"; exit 1; }
              go test ./...
              CGO_ENABLED=1 CC=clang go test -race ./...
              runHook postCheck
            '';
          });

          formatter = pkgs.nixpkgs-fmt;
        });

      moduleSystem = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          self.nixosModules.default
          {
            boot.isContainer = true;
            system.stateVersion = "26.05";
            services.prometheus.enable = true;
            services.emporiad = {
              enable = true;
              credentialsFile = "/run/emporia";
              prometheus.enable = true;
            };
          }
        ];
      };
      # Evaluation fails (so the check does) if the options stop doing their job.
      moduleCheck =
        let c = moduleSystem.config; in
        assert c.systemd.services.emporiad.serviceConfig.LoadCredential == [ "credentials:/run/emporia" ];
        assert builtins.any (j: j.job_name == "emporiad") c.services.prometheus.scrapeConfigs;
        c.systemd.units."emporiad.service".unit;

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
      checks = forEachSystem (system: perSystem.${system}.checks // nixpkgs.lib.optionalAttrs (system == "x86_64-linux") {
        module = moduleCheck;
      });
      devShells = forEachSystem (system: perSystem.${system}.devShells);
      formatter = forEachSystem (system: perSystem.${system}.formatter);
    };
}
