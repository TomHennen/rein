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

      # Date, not rev: commits on the same day share a store path.
      d = self.lastModifiedDate;
      version = "0-unstable-${builtins.substring 0 4 d}-${builtins.substring 4 2 d}-${builtins.substring 6 2 d}";

      # Only what go build/test reads, so docs, goldens and scripts do not rebuild.
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

      mkRein =
        pkgs:
        pkgs.buildGo126Module {
          pname = "rein";
          inherit version;
          src = goSrc;
          vendorHash = "sha256-mg/0jfgVYgsOrUNtVcycikjx8Ncasnw/NxAu52ytZJQ=";

          # rein init copies rein-git and rein-gh from next to the rein binary.
          subPackages = [
            "cmd/rein"
            "cmd/rein-git"
            "cmd/rein-gh"
          ];

          env.CGO_ENABLED = "0";
          # Mirrors .goreleaser.yml (-trimpath and -buildid= come from buildGoModule), minus
          # its -X main.* flags: cmd/rein defines no such variables yet.
          ldflags = [
            "-s"
            "-w"
          ];

          nativeCheckInputs = [ pkgs.git ];
          # The sandbox's /bin/sh is busybox, which dispatches on the spoofed argv[0]
          # ("git") this test relies on and fails with "applet not found".
          checkFlags = [ "-skip=^TestDetectFromProcTree_FindsGitPushAncestor$" ];
          # subPackages would limit tests to cmd/; test the whole module.
          checkPhase = ''
            runHook preCheck
            export GOFLAGS=''${GOFLAGS//-trimpath/}
            go test -p $NIX_BUILD_CORES $checkFlags ./...
            runHook postCheck
          '';

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
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux { sandbox-runtime = srtFor pkgs; }
      );

      checks = forAllSystems (
        pkgs:
        {
          rein = self.packages.${pkgs.stdenv.hostPlatform.system}.rein;
          fmt =
            pkgs.runCommand "rein-fmt-check"
              {
                nativeBuildInputs = [
                  pkgs.go_1_26
                  pkgs.nixfmt
                ];
                src = lib.fileset.toSource {
                  root = ./.;
                  fileset = lib.fileset.fileFilter (f: f.hasExt "go" || f.hasExt "nix") ./.;
                };
              }
              ''
                cd "$src"
                find . -name '*.nix' -exec nixfmt --check {} +
                unformatted=$(gofmt -l .)
                if [ -n "$unformatted" ]; then
                  echo "gofmt -l reports unformatted files:" >&2
                  echo "$unformatted" >&2
                  exit 1
                fi
                touch "$out"
              '';
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

      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);
    };
}
