{
  description = "rein: a local credential broker for AI coding agents";

  # nixpkgs is the only input, to keep the supply-chain surface minimal.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      linuxSystems = lib.filter (lib.hasSuffix "-linux") systems;
      forSystems = ss: f: lib.genAttrs ss (system: f nixpkgs.legacyPackages.${system});
      forAllSystems = forSystems systems;

      # srt is Linux-only here: rein's sandboxed mode is Linux-only today.
      srtFor = pkgs: pkgs.callPackage ./nix/sandbox-runtime { };
      seccompArch = pkgs: if pkgs.stdenv.hostPlatform.isAarch64 then "arm64" else "x64";

      # No release tags yet, so the package is named after the commit date
      # (YYYYMMDD), the nixpkgs convention for an untagged build.
      version = "0-unstable-${lib.substring 0 8 self.lastModifiedDate}";

      # Only what go build/test reads. Docs, scripts and the journey goldens
      # (read by the Python suite, not by go test) do not trigger a rebuild.
      goSrc = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          ./go.mod
          ./go.sum
          # internal/srt's test holds the Nix srt pin to PinnedVersion.
          ./nix/sandbox-runtime/package.json
          ./nix/sandbox-runtime/package-lock.json
          (lib.fileset.fileFilter (f: f.hasExt "go") ./.)
          ./tests/containment/testdata
        ];
      };

      # One formatter for `nix fmt` and the fmt check: nixfmt for *.nix, gofmt for *.go.
      treefmtFor =
        pkgs:
        pkgs.nixfmt-tree.override {
          runtimeInputs = [ pkgs.go_1_26 ];
          settings.formatter.gofmt = {
            command = "gofmt";
            options = [ "-w" ];
            includes = [ "*.go" ];
          };
        };

      mkRein =
        pkgs:
        pkgs.buildGo126Module {
          pname = "rein";
          inherit version;
          src = goSrc;
          # Hash of the Go module dependencies (what go.sum resolves to), not of
          # the Go toolchain. Refresh after any go.mod/go.sum change (CLAUDE.md).
          vendorHash = "sha256-mg/0jfgVYgsOrUNtVcycikjx8Ncasnw/NxAu52ytZJQ=";

          # Build and test every package; only these two test-helper commands are
          # left out, so bin/ holds rein, rein-git and rein-gh (rein init copies
          # the last two from next to rein).
          excludedPackages = [
            "tests/containment/cmd/classify"
            "tests/interactive/seedghread"
          ];

          env.CGO_ENABLED = "0";
          # Mirrors .goreleaser.yml (-trimpath and -buildid= come from buildGoModule), minus
          # its -X main.* flags: cmd/rein defines no such variables yet.
          ldflags = [
            "-s"
            "-w"
          ];

          # The suite assumes Linux paths (/home, /tmp are symlinks on macOS) and
          # rein is Linux-only for now (design 5.4), so darwin builds without it.
          doCheck = pkgs.stdenv.hostPlatform.isLinux;
          nativeCheckInputs = [ pkgs.git ];

          meta = {
            description = "Local credential broker issuing short-lived, scoped GitHub credentials to AI coding agents";
            homepage = "https://github.com/TomHennen/rein";
            license = lib.licenses.asl20;
            mainProgram = "rein";
            platforms = lib.platforms.linux ++ lib.platforms.darwin;
          };
        };
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          rein = mkRein pkgs;
        in
        {
          inherit rein;
          default = rein;
        }
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux rec {
          sandbox-runtime = srtFor pkgs;
          # What `nix profile add` installs on Linux: rein plus the pinned srt (which
          # carries its own node/socat/rg). bwrap is not included (HANDOFF.md 1b).
          default = pkgs.buildEnv {
            name = "rein-with-srt-${version}";
            paths = [
              rein
              sandbox-runtime
            ];
            pathsToLink = [ "/bin" ];
            meta = rein.meta;
          };
        }
      );

      checks = forAllSystems (
        pkgs:
        {
          rein = self.packages.${pkgs.stdenv.hostPlatform.system}.rein;
          fmt = (treefmtFor pkgs).check (
            lib.fileset.toSource {
              root = ./.;
              fileset = lib.fileset.fileFilter (f: f.hasExt "go" || f.hasExt "nix") ./.;
            }
          );
        }
        # The built srt runs and still carries its seccomp helper. Its version is
        # held to rein's PinnedVersion by a Go test (internal/srt/nixpin_test.go).
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          srt = pkgs.runCommand "srt-check" { } ''
            pkg=$(dirname "$(dirname "$(readlink -f ${srtFor pkgs}/bin/srt)")")
            test -x "$pkg/vendor/seccomp/${seccompArch pkgs}/apply-seccomp"
            ${srtFor pkgs}/bin/srt --help > /dev/null
            touch $out
          '';
        }
      );

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          name = "rein-dev";
          inputsFrom = [ self.packages.${pkgs.stdenv.hostPlatform.system}.rein ];
          packages = [
            pkgs.gopls
            pkgs.goreleaser
            pkgs.git
            pkgs.gh
            # pyte (LGPLv3) is test-only: dev shell only, never a package closure.
            (pkgs.python3.withPackages (ps: [
              ps.pexpect
              ps.pyte
            ]))
          ]
          # srt carries its own node/socat/rg on its wrapper PATH. bwrap and tmux
          # stay host-provided: the AppArmor userns profile is keyed to
          # /usr/bin/bwrap (HANDOFF.md 1b), and the approval popup talks to the
          # user's running tmux server.
          ++ lib.optional pkgs.stdenv.hostPlatform.isLinux (srtFor pkgs);
          GOTOOLCHAIN = "local";
        };
      });

      formatter = forAllSystems treefmtFor;
    };
}
