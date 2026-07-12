"""journey_credential_boundary — DENY-$HOME, PROVED BY AN INDEPENDENT SCANNER (#59/#55).

This is ONE journey. For what a journey IS, the golden-transcript rule, the shared
runner, and how to author the next one, read tests/interactive/CLAUDE.md — none of
that lives here.

COMPLEMENTARY TO journey_sandbox_filesystem, not a duplicate. That journey walks the
filesystem boundary from inside by `cat`-ing SPECIFIC known paths (~/.ssh/id_rsa,
~/.aws/credentials, rein's app key) and asserting each reads absent — an enumerated
check. THIS journey proves the same deny-$HOME property with DIFFERENT evidence: an
external, third-party credential SCANNER (bagel) swept over the whole fixture, run
twice as a differential —

  * CONTROL  (rein run --direct, sandbox OFF): the scanner finds the planted
    credentials — ground truth that the box is loaded AND the scanner works. A bare
    "sandboxed found 0" is vacuous without this: a broken scanner looks identical.
  * SANDBOXED (rein run): the identical scan finds ZERO — deny-$HOME hid every one.

What the scanner adds over the enumerated cat-checks: it is INDEPENDENT (a tool with
its own idea of where credentials live), and it SWEEPS — so it would catch a
credential at a path nobody thought to enumerate. That is the #55 "unknown-unknown"
class, checked structurally rather than path-by-path.

CAPTURE IS STRUCTURAL (#82/#85): the two runs are run_journey STEPS and the runner
captures the COMPLETE pty session of both — the `$ rein run` echoes ARE the run
boundaries. The journey never hand-assembles the golden or bakes a computed claim
into it; the counts live only in the printed outcomes. Volatiles are handled by
normalize-on-compare.

THE SCANNER is `bagel` (github.com/boostsecurityio/bagel, GPL-3.0), invoked ONLY as
an external CLI a developer installs (`go install .../cmd/bagel@latest`). It is
deliberately NOT a go.mod dependency and MUST NOT become one. The journey SKIPS
cleanly (exit 0, no golden touched) when bagel is not on PATH. bagel is the right
tool because it reports only metadata — "Bagel never reads secret values" — so its
output is safe to commit as a golden; a raw secret-scanner would put secret material
in the repo.

THE FIXTURE is a throwaway tree of OBVIOUSLY-FAKE credentials planted directly under
the REAL $HOME at ~/rein-bagel-fixture. It MUST live under the real $HOME: rein's
deny-$HOME is what hides it, and (verified) a HOME relocated under /tmp is NOT hidden
— so an isolated-HOME harness would defeat the very property under test. The fixture
is NOT under any allow-back path, so deny-$HOME hides it. We scan the fixture via
--base-dirs and count only findings whose path is under it, so bagel's ambient probes
(gh-auth, env — which ignore --base-dirs) add no machine-dependent noise: CONTROL=4,
SANDBOXED=0.

It WRITES only throwaway paths under $HOME (hard-constraint #1). Note the CONTROL leg
runs bagel with the sandbox OFF, so bagel's ambient probes DO read the real
environment's credential *metadata* (gh login state, git config) — bagel never reads
secret VALUES, and the reduction keeps only fixture-scoped findings, so nothing real
reaches the golden. The SANDBOXED leg has no such access: that is the point.

    python3 tests/interactive/journey_credential_boundary.py            # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_credential_boundary.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_credential_boundary.py # also print the compare lens

Exit 0 = boundary held AND the normalized transcript matches the golden (or bagel
absent -> skip). Exit 1 = drift. Exit 2 = the boundary BROKE (sandboxed scan saw a
credential, or the control found none, or bagel did not run).
"""

from __future__ import annotations

import os
import re
import shutil
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "credential_boundary.txt"

# Fixed name (journeys run serially, so no collision) placed DIRECTLY under $HOME
# and NOT under any allow-back path, so deny-$HOME hides it. A fixed name keeps
# the transcript stable (no per-run nonce to normalize).
FIXTURE_DIRNAME = "rein-bagel-fixture"
SCAN_OUT_NAME = ".rein-bagel-scan.json"  # bagel's JSON; under $HOME => tmpfs-writable in-sandbox

# The reduction, as a jq filter: secret-type findings whose path is under the
# fixture. Filtering by the fixture dir NAME (not an absolute path) makes the
# count machine-independent AND drops bagel's ambient probes (gh-auth, env vars,
# git config) that ignore --base-dirs — so the count reflects ONLY the mount
# boundary. On the planted fixture this is a deterministic 4 (aws id + aws secret
# + docker + ssh); npmrc is intentionally omitted (bagel does not flag it under an
# arbitrary base dir).
_JQ_SEL = (
    '[.findings[]|select(.type=="secret" and '
    f'((.path|tostring)|contains("{FIXTURE_DIRNAME}")))]'
)
JQ_COUNT = f"{_JQ_SEL}|length"
JQ_IDS = f"{_JQ_SEL}|map(.id)|sort|unique|join(\",\")"
# Per-finding detail for the golden: severity, id, and the path RELATIVE to the
# fixture (strip everything up to & including the fixture dir, so no machine path
# leaks and it stays deterministic). sort_by makes the order stable. Scoped to the
# fixture ONLY — bagel's ambient probes read the REAL environment in the --direct
# leg, and that (real credential metadata) must never enter a committed golden.
JQ_DETAIL = (
    f"{_JQ_SEL}|sort_by(.path,.id)|.[]|"
    f'"\\(.severity) \\(.id) \\(.path|tostring|sub(".*{FIXTURE_DIRNAME}/";""))"'
)


def scan_script(bagel: str) -> str:
    """A `bash -c` body run identically in both steps. It runs bagel scoped to the
    fixture and REDUCES the JSON to tagged sentinels the test asserts on. The JSON
    goes to a fresh file under $HOME (truncated on open, so a crashed bagel yields
    an EMPTY file — never a stale read — and $HOME is tmpfs-writable in-sandbox).

    Proof-of-execution: valid JSON with a findings array means bagel actually RAN.
    Without it, a sandboxed bagel that CRASHED would also yield 0 findings and
    masquerade as a held boundary — so main() asserts the RAN sentinel on both
    legs. (No literal sentinel value appears in a comment here: rein echoes the
    whole script into its banner, and the parser anchors on the SBX tag to ignore
    that echo. --no-cache forces a live index; --disable-version-check keeps bagel
    off the network so it doesn't hit rein's egress allowlist.)
    """
    return f"""
{H.sandbox_preamble()}
FX="$HOME/{FIXTURE_DIRNAME}"
OUT="$HOME/{SCAN_OUT_NAME}"
emit '$ bagel scan --base-dirs ~/{FIXTURE_DIRNAME} --format json  (secrets, summarized)'
{bagel} scan --base-dirs "$FX" --format json --no-cache --disable-version-check \
  > "$OUT" 2>/dev/null
if jq -e 'has("findings")' "$OUT" >/dev/null 2>&1; then RAN=yes; else RAN=no; fi
COUNT=$(jq '{JQ_COUNT}' "$OUT" 2>/dev/null)
# Do NOT default a failed/empty count to 0 — that would let a jq failure
# (e.g. findings:null, which still passes has("findings")) read as "0 secrets
# found = boundary held" and PASS BY ACCIDENT. A non-numeric count => ERR, which
# main() cannot parse and so fails the run instead of silently passing.
case "$COUNT" in ''|*[!0-9]*) COUNT=ERR;; esac
IDS=$(jq -r '{JQ_IDS}' "$OUT" 2>/dev/null)
emit "@BAGEL_RAN=$RAN"
emit "@SECRET_COUNT=$COUNT"
emit "@SECRET_IDS=$IDS"
# Show WHAT bagel found under the fixture (severity/id/relative-path), so the
# golden lets a reviewer SEE the actual findings — 4 named creds in the CONTROL
# leg, none in the SANDBOXED leg — not just a bare count.
jq -r '{JQ_DETAIL}' "$OUT" 2>/dev/null | while IFS= read -r f; do emit "@FINDING $f"; done
emit "@SCAN_DONE"
"""


def plant_fixture(home: str) -> None:
    """Plant obviously-fake credentials under $HOME/rein-bagel-fixture."""
    fx = os.path.join(home, FIXTURE_DIRNAME)
    shutil.rmtree(fx, ignore_errors=True)
    os.makedirs(os.path.join(fx, ".aws"))
    os.makedirs(os.path.join(fx, ".ssh"))
    os.makedirs(os.path.join(fx, ".docker"))
    with open(os.path.join(fx, ".aws", "credentials"), "w") as f:
        f.write(
            "[default]\n"
            "aws_access_key_id = AKIAIOSFODNN7EXAMPLE\n"
            "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n"
        )
    with open(os.path.join(fx, ".ssh", "id_rsa"), "w") as f:
        f.write(
            "-----BEGIN OPENSSH PRIVATE KEY-----\n"
            "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZQ==\n"
            "-----END OPENSSH PRIVATE KEY-----\n"
        )
    with open(os.path.join(fx, ".docker", "config.json"), "w") as f:
        f.write('{"auths":{"https://index.docker.io/v1/":{"auth":"ZmFrZXVzZXI6ZmFrZXB3"}}}\n')


def cleanup_home(home: str) -> None:
    shutil.rmtree(os.path.join(home, FIXTURE_DIRNAME), ignore_errors=True)
    # The CONTROL leg wrote the scan JSON into the real $HOME; remove it. (The
    # SANDBOXED leg's copy went to an ephemeral tmpfs and is already gone.)
    try:
        os.remove(os.path.join(home, SCAN_OUT_NAME))
    except OSError:
        pass


def _pinned_session(repo: str) -> str:
    """A temp repo-only session so the journey never depends on the machine's
    ambient dev-session.yaml (whose path + any `issue:` warning would pollute the
    banner). No `issue:` line (#35 retired it)."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_credboundary\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


# The sentinel lines only ever appear tagged (SBX| ) in the real emitted output;
# anchoring on the tag ignores rein's echo of the script body in its banner.
_TAG = re.escape(H.SBX_TAG)


def _extract(step_text: str):
    """Pull (ran, count, ids) from ONE step's captured pty text. Missing/garbled
    sentinels -> (None, None, "") so main()'s invariant reports a clean break."""
    m_ran = re.search(_TAG + r"@BAGEL_RAN=(yes|no)", step_text)
    m_cnt = re.search(_TAG + r"@SECRET_COUNT=(\d+)", step_text)
    m_ids = re.search(_TAG + r"@SECRET_IDS=([^\r\n]*)", step_text)
    ran = m_ran.group(1) if m_ran else None
    count = int(m_cnt.group(1)) if m_cnt else None
    ids = m_ids.group(1).strip() if m_ids else ""
    return ran, count, ids


def main() -> int:
    bagel = shutil.which("bagel")
    if not bagel:
        print("SKIP: bagel not on PATH — install with "
              "`go install github.com/boostsecurityio/bagel/cmd/bagel@latest` "
              "(external CLI only; do NOT add to go.mod, it is GPL-3.0).", flush=True)
        return 0

    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)  # rein-init way first; #40
    H.build_binaries(env)
    home = os.path.expanduser("~")

    # The SANDBOXED leg must not inherit an operator's home-widening env, or the
    # fixture could be re-exposed and the boundary would spuriously read as broken.
    # Scrubbing globally is harmless to the CONTROL (--direct) leg.
    env = {k: v for k, v in env.items()
           if k not in ("REIN_SANDBOX_SHOW_HOME", "REIN_SANDBOX_ALLOW_READ")}

    print(f"journey: credential boundary on {repo} (scanner: {bagel})", flush=True)

    # A non-git cwd so the #64 checkout-binding banner is identical on any machine.
    clean_cwd = tempfile.mkdtemp(prefix="rein-credbound-cwd-")
    session = _pinned_session(repo)
    script = scan_script(bagel)
    try:
        plant_fixture(home)

        # DECLARE STEPS ONLY — run_journey (#85: THE interface, sandbox steps
        # included) captures the COMPLETE session of both runs. Same script,
        # sandbox off then on; neither has prompts. Per-step cwd = a non-git dir
        # for a machine-stable banner; the slow sandbox launch gets timeout=180.
        result = H.run_journey(
            [
                H.JourneyStep(argv=["run", "--direct", "--", "bash", "-c", script],
                              label="rein run --direct -- bash -c <credential-scan>",
                              cwd=clean_cwd),
                H.JourneyStep(argv=["run", "--", "bash", "-c", script],
                              label="rein run -- bash -c <credential-scan>",
                              cwd=clean_cwd),
            ],
            env=env,
            extra_env={"REIN_SESSION_FILE": session},
            timeout=180,
        )
        text = result.transcript
        c_ran, c_count, c_ids = _extract(result.steps[0].text) if len(result.steps) > 0 else (None, None, "")
        s_ran, s_count, s_ids = _extract(result.steps[1].text) if len(result.steps) > 1 else (None, None, "")

        # 1) The boundary must hold — independent of the golden. The @BAGEL_RAN
        #    checks are load-bearing: a sandboxed bagel that CRASHED would also
        #    report 0 findings, so "0" only means "boundary held" once we know
        #    bagel actually executed and emitted valid JSON in BOTH legs.
        checks = [
            (result.reached_eof, "both runs must complete (no missed prompt / timeout)"),
            (c_ran == "yes", "CONTROL: bagel must actually run (valid JSON)"),
            (s_ran == "yes", "SANDBOXED: bagel must actually run (valid JSON), "
                             "else a crash masquerades as a held boundary"),
            # The count jq is a SEPARATE invocation from the @BAGEL_RAN guard, so
            # assert it produced a number. A failed count reduction emits ERR (not
            # a defaulted 0), which does not parse -> count is None -> this fails
            # LOUDLY instead of the sandboxed "== 0" check passing by accident.
            (c_count is not None, "CONTROL: @SECRET_COUNT must parse (the count jq produced a number)"),
            (s_count is not None, "SANDBOXED: @SECRET_COUNT must parse (the count jq produced a number)"),
            (c_count is not None and c_count > 0,
             "CONTROL must find the planted creds (scanner works, box is loaded)"),
            (s_count == 0,
             "SANDBOXED must find ZERO fixture creds (deny-$HOME hid them)"),
        ]
        broken = [msg for ok, msg in checks if not ok]
        if broken:
            print("BOUNDARY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  control:   ran={c_ran} count={c_count} ids=[{c_ids}]", flush=True)
            print(f"  sandboxed: ran={s_ran} count={s_count} ids=[{s_ids}]", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # 2) The golden IS the complete captured session (no hand-assembly, no
        #    baked claim). The counts are asserted above and echoed here only.
        print()
        print(text, flush=True)
        print("--- outcomes (asserted; not baked into the golden) ---", flush=True)
        print(f"  CONTROL   ran={c_ran} secrets={c_count} ids=[{c_ids}]", flush=True)
        print(f"  SANDBOXED ran={s_ran} secrets={s_count} ids=[{s_ids}]", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(text), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN_NAME, text)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN_NAME, text)
        if ok:
            print(f"[golden OK] fresh run matches golden/{GOLDEN_NAME} (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "credential_boundary.fresh.txt")
        with open(scratch, "w") as f:
            f.write(text)
        print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        cleanup_home(home)
        shutil.rmtree(clean_cwd, ignore_errors=True)
        shutil.rmtree(os.path.dirname(session), ignore_errors=True)
        print("cleanup: fixture + scratch removed", flush=True)


if __name__ == "__main__":
    sys.exit(main())
