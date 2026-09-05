{
  description = "diane - a git-backed note vault with a voice secretary";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # Everything diane shells out to. The wrapper puts exactly these on PATH.
      # llama-server is deliberately absent: it is a long-running process you
      # start yourself (serve-llm.sh), not something diane spawns.
      runtimeDeps = pkgs: with pkgs; [ git sox whisper-cpp piper-tts alsa-utils ];

      package = pkgs: pkgs.buildGoModule {
        pname = "diane";
        version = "0.2.0";
        src = ./.;
        vendorHash = null; # zero external Go dependencies; keep it that way
        nativeBuildInputs = [ pkgs.makeWrapper ];
        nativeCheckInputs = [ pkgs.git ];
        postInstall = ''
          install -m755 bin/* $out/bin/
          wrapProgram $out/bin/diane --prefix PATH : "${pkgs.lib.makeBinPath (runtimeDeps pkgs)}:$out/bin"
        '';
        meta.mainProgram = "diane";
      };
    in
    {
      packages = forAll (pkgs: { default = package pkgs; });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = runtimeDeps pkgs ++ (with pkgs; [
            go gopls jq curl
            # Vulkan build of llama.cpp; on RDNA4 (RX 9070 XT) less fuss than ROCm.
            (llama-cpp.override { vulkanSupport = true; })
          ]);
          shellHook = ''
            export PATH="$PWD/bin:$PATH"
            export DIANE_VAULT="''${DIANE_VAULT:-$HOME/vault}"
            echo "diane devshell: vault $DIANE_VAULT, backend ''${DIANE_BACKEND:-local}"
          '';
        };
      });

      # services.diane runs `diane serve`: the phone's endpoint. It needs to
      # push, so run it as the user whose ~/.ssh can reach the vault's remote.
      nixosModules.default = { config, lib, pkgs, ... }:
        let cfg = config.services.diane; in
        {
          options.services.diane = {
            enable = lib.mkEnableOption "diane note capture daemon";
            vault = lib.mkOption { type = lib.types.str; description = "Path to the vault clone."; };
            user = lib.mkOption { type = lib.types.str; description = "User owning the clone and the SSH key that can push it."; };
            port = lib.mkOption { type = lib.types.port; default = 7777; };
            environmentFile = lib.mkOption {
              type = lib.types.path;
              description = "File containing DIANE_TOKEN=<secret>. Mode 0400, outside the nix store.";
            };
            tailscaleOnly = lib.mkOption {
              type = lib.types.bool;
              default = true;
              description = "Open the port on tailscale0 only, never on a public interface.";
            };
          };

          config = lib.mkIf cfg.enable {
            systemd.services.diane = {
              description = "diane note capture daemon";
              wantedBy = [ "multi-user.target" ];
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];
              environment = {
                DIANE_VAULT = cfg.vault;
                DIANE_ADDR = "0.0.0.0:${toString cfg.port}";
              };
              serviceConfig = {
                ExecStart = "${lib.getExe self.packages.${pkgs.stdenv.hostPlatform.system}.default} serve";
                EnvironmentFile = cfg.environmentFile;
                User = cfg.user;
                Restart = "on-failure";
                RestartSec = 2;
                ProtectSystem = "strict";
                ReadWritePaths = [ cfg.vault ];
                PrivateTmp = true;
                NoNewPrivileges = true;
                # No ProtectHome: git needs ~/.ssh and ~/.gitconfig to push.
              };
            };
            networking.firewall.interfaces = lib.mkIf cfg.tailscaleOnly {
              tailscale0.allowedTCPPorts = [ cfg.port ];
            };
          };
        };
    };
}
