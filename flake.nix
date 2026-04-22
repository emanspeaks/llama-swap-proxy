{
  description = "llama-swap-proxy — Reverse proxy for llama-swap";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    gomod2nix = {
      url = "github:nix-community/gomod2nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, flake-utils, gomod2nix }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" ];
    in
    flake-utils.lib.eachSystem supportedSystems (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        inherit (pkgs) lib;
        inherit (gomod2nix.legacyPackages.${system}) buildGoApplication;

        llama-swap-proxy = buildGoApplication {
          pname = "llama-swap-proxy";
          version = lib.fileContents ./VERSION;
          src = ./.;
          modules = ./gomod2nix.toml;

          meta = {
            description = "Reverse proxy for llama-swap";
            license = lib.licenses.mit;
            platforms = lib.platforms.linux;
            mainProgram = "llama-swap-proxy";
          };
        };
      in
      {
        packages = {
          default = llama-swap-proxy;
          inherit llama-swap-proxy;
        };
      }
    )
    //
    {
      nixosModules.default = { config, lib, pkgs, ... }:
        let
          cfg = config.services.llama-swap-proxy;
        in
        {
          options.services.llama-swap-proxy = {
            enable = lib.mkEnableOption "llama-swap-proxy reverse proxy service";

            port = lib.mkOption {
              type = lib.types.port;
              default = 5900;
              description = "TCP port llama-swap-proxy listens on.";
            };

            upstream = lib.mkOption {
              type = lib.types.str;
              default = "http://localhost:${toString config.services.llama-swap.port}";
              defaultText = lib.literalExpression
                ''"http://localhost:''${toString config.services.llama-swap.port}"'';
              description = ''
                URL of the upstream llama-swap instance.  Defaults to the port
                configured by services.llama-swap.port so a port change there
                propagates automatically.
              '';
            };

            extraArgs = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [];
              description = "Additional arguments passed verbatim to llama-swap-proxy.";
            };

            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.system}.default;
              defaultText = lib.literalExpression "llama-swap-proxy flake package";
              description = "The llama-swap-proxy package to use.";
            };
          };

          config = lib.mkIf cfg.enable {
            systemd.services.llama-swap-proxy = {
              description = "llama-swap reverse proxy";
              wantedBy = [ "multi-user.target" ];
              after = [ "network.target" "llama-swap.service" ];

              serviceConfig = {
                ExecStart = lib.concatStringsSep " " (
                  [
                    "${lib.getExe cfg.package}"
                    "--listen" ":${toString cfg.port}"
                    "--upstream" cfg.upstream
                  ]
                  ++ cfg.extraArgs
                );

                DynamicUser = true;
                Restart = "on-failure";
                RestartSec = "5s";
                ProtectHome = true;
                PrivateTmp = true;
              };
            };
          };
        };
    };
}
