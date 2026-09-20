{
  description = "familiar-fleet client";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in {
      packages = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system}; in {
          default = pkgs.buildGoModule {
            pname = "familiar-fleet";
            version = "0.1.0";
            src = self;
            vendorHash = null;
            ldflags = [ "-s" "-w" "-X main.version=0.1.0" ];
          };
        });

      devShells = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system}; in {
          default = pkgs.mkShell {
            packages = [ pkgs.go pkgs.gopls pkgs.gotools pkgs.openssh ];
          };
        });
    };
}
