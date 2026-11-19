{
  description = "rein development environment";

  # nixpkgs is the only input, to keep the supply-chain surface minimal.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      # Linux only for now, like rein itself.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      srtFor = pkgs: pkgs.callPackage ./nix/sandbox-runtime { };
    in
    {
      packages = forAllSystems (pkgs: {
        sandbox-runtime = srtFor pkgs;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          name = "rein-dev";
          packages = [
            pkgs.go_1_26
            pkgs.gopls
            pkgs.goreleaser
            pkgs.git
            pkgs.gh
            # pyte (LGPLv3) is test-only: dev shell only, never a package closure.
            (pkgs.python3.withPackages (ps: [
              ps.pexpect
              ps.pyte
            ]))
            # srt carries its own node/socat/rg on its wrapper PATH. bwrap and tmux
            # stay host-provided: the AppArmor userns profile is keyed to
            # /usr/bin/bwrap (HANDOFF.md 1b), and the approval popup talks to the
            # user's running tmux server.
            (srtFor pkgs)
          ];
          GOTOOLCHAIN = "local";
        };
      });

      # The built srt runs and still carries its seccomp helper. Its version is
      # held to rein's PinnedVersion by a Go test (internal/srt/nixpin_test.go).
      checks = forAllSystems (
        pkgs:
        let
          srt = srtFor pkgs;
          arch = if pkgs.stdenv.hostPlatform.isAarch64 then "arm64" else "x64";
        in
        {
          srt = pkgs.runCommand "srt-check" { } ''
            pkg=$(dirname "$(dirname "$(readlink -f ${srt}/bin/srt)")")
            test -x "$pkg/vendor/seccomp/${arch}/apply-seccomp"
            ${srt}/bin/srt --help > /dev/null
            touch $out
          '';
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);
    };
}
