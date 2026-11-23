# bagel: the independent credential scanner the credential_boundary journey
# runs on both sides of the sandbox. GPL-3.0, so it is a dev-shell tool ONLY:
# a standalone CLI, never linked into rein and never in a package closure
# (hard-constraint #4).
{
  lib,
  buildGoModule,
  fetchFromGitHub,
}:

buildGoModule (finalAttrs: {
  pname = "bagel";
  version = "0.7.1";

  src = fetchFromGitHub {
    owner = "boostsecurityio";
    repo = "bagel";
    tag = "v${finalAttrs.version}";
    hash = "sha256-1I0YkRTrSdkOkCuNKrQqel78BnGvIAmLc/+a8EdKQOA=";
  };

  vendorHash = "sha256-DU0Fp0p+NgNJsJ44d6vDFa8KyHQi4CskIfd0McskwJs=";
  subPackages = [ "cmd/bagel" ];
  doCheck = false;

  meta = {
    description = "CLI that inventories security-relevant metadata on developer workstations";
    homepage = "https://github.com/boostsecurityio/bagel";
    license = lib.licenses.gpl3Only;
    mainProgram = "bagel";
  };
})
