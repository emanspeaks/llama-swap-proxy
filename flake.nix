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

            config = lib.mkOption {
              type = lib.types.str;
              default = "/ai/llama-swap/config.yaml";
              description = ''
                Path to the llama-swap config.yaml file used by the /opencode
                endpoint when generating provider metadata.
              '';
            };

            sessionsDir = lib.mkOption {
              type = lib.types.str;
              default = "/ai/sessions";
              description = ''
                Directory used for centralized synchronized session storage.
                The service stores SQLite state at sessions.db under this directory.
              '';
            };

            defaultUser = lib.mkOption {
              type = lib.types.str;
              default = "user";
              description = ''
                Default username associated with synchronized session state when
                no authentication layer is configured.
              '';
            };

            isolateModelUserStates = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = ''
                When enabled, synchronized state is isolated per /upstream/<model>
                namespace; when disabled, all models share one state namespace.
              '';
            };

            opencodeHostname = lib.mkOption {
              type = lib.types.str;
              default = "";
              description = ''
                Custom host (and optional port) to use in /opencode response URLs,
                e.g. "myserver.local:5900".  When set, overrides the Host header of
                the incoming request.  Leave empty to use the request Host header.
              '';
            };

            opencodeIncludeModelType = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [];
              example = [ "llm" "vlm" ];
              description = ''
                List of metadata.model_type values to include in /opencode responses.
                When non-empty, only the listed model types are eligible unless they
                are also listed in opencodeExcludeModelType.
              '';
            };

            opencodeExcludeModelType = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [];
              example = [ "embedding" "sd" ];
              description = ''
                List of metadata.model_type values to exclude from /opencode responses.
                Exclusions take priority over opencodeIncludeModelType.
              '';
            };

            extraArgs = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [];
              description = "Additional arguments passed verbatim to llama-swap-proxy.";
            };

            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
              defaultText = lib.literalExpression "llama-swap-proxy flake package";
              description = "The llama-swap-proxy package to use.";
            };
          };

          config = lib.mkIf cfg.enable {
            users.groups.llama-swap-proxy = { };
            users.users.llama-swap-proxy = {
              isSystemUser = true;
              group = "llama-swap-proxy";
            };

            systemd.tmpfiles.rules = [
              "d ${cfg.sessionsDir} 0750 llama-swap-proxy llama-swap-proxy -"
            ];

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
                    "--config" cfg.config
                    "--sessions-dir" cfg.sessionsDir
                    "--default-user" cfg.defaultUser
                  ]
                  ++ lib.optionals cfg.isolateModelUserStates [
                    "--isolate-model-user-states"
                  ]
                  ++ lib.optionals (cfg.opencodeHostname != "") [
                    "--opencode-hostname" cfg.opencodeHostname
                  ]
                  ++ lib.optionals (cfg.opencodeIncludeModelType != []) [
                    "--opencode-include-model-type"
                    (lib.concatStringsSep "," cfg.opencodeIncludeModelType)
                  ]
                  ++ lib.optionals (cfg.opencodeExcludeModelType != []) [
                    "--opencode-exclude-model-type"
                    (lib.concatStringsSep "," cfg.opencodeExcludeModelType)
                  ]
                  ++ cfg.extraArgs
                );

                User = "llama-swap-proxy";
                Group = "llama-swap-proxy";
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
