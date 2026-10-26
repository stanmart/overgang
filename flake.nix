{
  description = "ÖVERGÅNG: an IKEA TRÅDFRI gateway emulator on top of Home Assistant";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "overgang";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-UqZjP/eiTqQs/1pcAleHEmgKZYrm2vClekN43AWngz4=";
          subPackages = [ "cmd/overgang" ];
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
          ];
        };
      });
    };
}
