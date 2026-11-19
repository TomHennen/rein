{
  description = "rein development environment";

  # nixpkgs is the only input, to keep the supply-chain surface minimal.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          name = "rein-dev";
          packages = [
            pkgs.go_1_26
            pkgs.gopls
            pkgs.goreleaser
            pkgs.git
            pkgs.gh
            pkgs.tmux
            pkgs.socat
            pkgs.ripgrep
            pkgs.nodejs
            # pyte (LGPLv3) is test-only: dev shell only, never a package closure.
            (pkgs.python3.withPackages (ps: [
              ps.pexpect
              ps.pyte
            ]))
          ];
          # bubblewrap and srt are deliberately absent: a store bwrap on PATH would
          # shadow the host one the AppArmor userns profile (HANDOFF.md 1b) is keyed to.
          GOTOOLCHAIN = "local";
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);
    };
}
