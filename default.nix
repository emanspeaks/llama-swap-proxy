{ lib, buildGoModule }:

buildGoModule {
  pname   = "llama-swap-proxy";
  version = lib.fileContents ./VERSION;
  src     = ./.;

  # run `go mod tidy` then `nix build` with lib.fakeHash to get the correct hash
  vendorHash = "sha256-g+yaVIx4jxpAQ/+WrGKxhVeliYx7nLQe/zsGpxV4Fn4=";

  meta = with lib; {
    description = "Reverse proxy for llama-swap";
    mainProgram = "llama-swap-proxy";
    license     = licenses.mit;
    platforms   = [ "x86_64-linux" ];
  };
}
