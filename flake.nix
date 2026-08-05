{
  description = "diane - append-only note capture daemon";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "diane";
          version = "0.1.0";
          src = ./.;
          vendorHash = null; # zero external dependencies
          meta.mainProgram = "diane";
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.gopls pkgs.git ];
        };
      });

      nixosModules.default = { config, lib, pkgs, ... }:
        let
          cfg = config.services.diane;
        in
        {
          options.services.diane = {
            enable = lib.mkEnableOption "diane note capture daemon";

            vault = lib.mkOption {
              type = lib.types.str;
              description = "Path to the git-tracked vault directory.";
            };

            user = lib.mkOption {
              type = lib.types.str;
              description = "User to run as. Must own the vault and be a valid git committer.";
            };

            port = lib.mkOption {
              type = lib.types.port;
              default = 7777;
            };

            environmentFile = lib.mkOption {
              type = lib.types.path;
              description = ''
                File containing DIANE_TOKEN=<secret>. Mode 0400, not in the nix store.
              '';
            };

            tailscaleOnly = lib.mkOption {
              type = lib.types.bool;
              default = true;
              description = "Open the port on tailscale0 only, never the public interface.";
            };
          };

          config = lib.mkIf cfg.enable {
            systemd.services.diane = {
              description = "diane note capture daemon";
              wantedBy = [ "multi-user.target" ];
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];
              path = [ pkgs.git ];

              environment = {
                DIANE_VAULT = cfg.vault;
                DIANE_ADDR = "0.0.0.0:${toString cfg.port}";
              };

              serviceConfig = {
                ExecStart = lib.getExe self.packages.${pkgs.stdenv.hostPlatform.system}.default;
                EnvironmentFile = cfg.environmentFile;
                User = cfg.user;
                Restart = "on-failure";
                RestartSec = 2;

                ProtectSystem = "strict";
                ReadWritePaths = [ cfg.vault ];
                PrivateTmp = true;
                NoNewPrivileges = true;
                ProtectKernelTunables = true;
                RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
              };
            };

            networking.firewall.interfaces = lib.mkIf cfg.tailscaleOnly {
              tailscale0.allowedTCPPorts = [ cfg.port ];
            };
          };
        };
    };
}
