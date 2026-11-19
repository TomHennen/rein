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
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # Date, not rev: commits on the same day share a store path.
      d = self.lastModifiedDate;
      version = "0-unstable-${builtins.substring 0 4 d}-${builtins.substring 4 2 d}-${builtins.substring 6 2 d}";

      # Only what go build/test reads, so docs, goldens and scripts do not rebuild.
      goSrc = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          ./go.mod
          ./go.sum
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
      packages = forAllSystems (pkgs: rec {
        rein = mkRein pkgs;
        default = rein;
      });

      checks = forAllSystems (pkgs: {
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
      });

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
          ];
          # The sandbox stack (bwrap, srt and its node/socat/rg) and tmux stay
          # host-provided: srt resolves them by PATH, and a store copy would swap
          # out the hand-verified stack (and miss the AppArmor profile, HANDOFF.md 1b).
          GOTOOLCHAIN = "local";
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);
    };
}
