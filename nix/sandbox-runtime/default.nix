# srt from the npm registry tarball (the same bytes `npm i -g` installs), so the
# prebuilt vendor/seccomp/*/apply-seccomp ships; nixpkgs' source build omits it.
# Every tarball is fetched by the sha512 integrity in package-lock.json.
{
  lib,
  importNpmLock,
  makeWrapper,
  nodejs_24,
  socat,
  ripgrep,
  which,
}:
let
  packageLock = lib.importJSON ./package-lock.json;
  version = packageLock.packages."node_modules/@anthropic-ai/sandbox-runtime".version;
  # socat/rg/which are prefixed so nothing on the caller's PATH shadows them;
  # bwrap is deliberately absent and still resolves from the host PATH (the
  # Ubuntu AppArmor userns profile is keyed to /usr/bin/bwrap, HANDOFF.md 1b).
  runtimePath = lib.makeBinPath [
    socat
    ripgrep
    which
  ];
in
importNpmLock.buildNodeModules {
  package = lib.importJSON ./package.json;
  inherit packageLock;
  nodejs = nodejs_24;
  derivationArgs = {
    pname = "sandbox-runtime";
    inherit version;
    nativeBuildInputs = [ makeWrapper ];
    # npmConfigHook runs `npm rebuild` after install; no dep needs a script.
    npmFlags = [ "--ignore-scripts" ];
    # apply-seccomp is a stripped static ELF: leave its bytes as published.
    dontStrip = true;
    dontPatchELF = true;
    # rein locates package.json by walking up from the resolved `srt`, so the
    # wrapper must live inside the package dir; bin/srt is a symlink to it.
    postInstall = ''
      pkg=$out/node_modules/@anthropic-ai/sandbox-runtime
      makeWrapper ${lib.getExe nodejs_24} $pkg/bin/srt \
        --add-flags $pkg/dist/cli.js \
        --prefix PATH : ${runtimePath}
      mkdir -p $out/bin
      ln -s $pkg/bin/srt $out/bin/srt
    '';
    meta = {
      description = "Anthropic sandbox-runtime (srt), pinned for rein";
      homepage = "https://github.com/anthropic-experimental/sandbox-runtime";
      license = lib.licenses.asl20;
      mainProgram = "srt";
      platforms = lib.platforms.linux;
    };
  };
}
