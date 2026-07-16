"""init_then_run — `rein init` (real mint, no env) then a real `rein run` clone (#128 review).

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: seeds the box's REAL rein-init App (state.json + primary.pem) into an
isolated home and CLEARS REIN_APP_*, so init/run resolve from state.json. init runs
WITHOUT --skip-mint-check (a real mint check), then `rein run --direct` and a sandboxed
`rein run` each clone the throwaway through the broker. LIVE + real-App-gated: SKIPs
(exit 3) if no App is configured (the #68 rule). The clone proves a broker read only
because the throwaway is PRIVATE (an unauthenticated clone would 404); a landed .git
means a token was minted + injected. Hard-constraint #1: writes confined to a tempdir home.
"""

from __future__ import annotations

import json
import os
import shutil
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"

_APP_ENV_KEYS = [
    "REIN_APP_CLIENT_ID", "REIN_APP_ID", "REIN_APP_INSTALLATION_ID",
    "REIN_APP_PRIVATE_KEY_PATH", "REIN_TEST_REPO_A", "REIN_TEST_REPO_B",
    "REIN_DEV_MODE",
]


def _sandbox_clone_script(repo: str) -> str:
    """In-sandbox bash: clone the throwaway through the broker, tag the result.

    `cd "$0"` enters the writable workdir rein binds as the final argv arg (the
    sandbox_filesystem journey's convention) so the clone lands in the bound tree
    and persists to the host for the ground-truth check.
    """
    return (
        f"{H.sandbox_preamble()}\n"
        'cd "$0"\n'
        # --quiet: git's server-side `remote: Total …` transfer summary is emitted
        # nondeterministically (pack-reuse varies run to run); suppress it so the
        # golden is stable. The .git landing + RC tag still prove the read worked.
        f"run git clone --quiet https://github.com/{repo} clone\n"
        '[ -d clone/.git ] && emit "@SBX_CLONE_RC=0" || emit "@SBX_CLONE_RC=1"\n'
    )


def _real_app_files():
    """(state.json path, primary.pem path) if a usable rein-init App exists, else None."""
    cfg = H._real_config_dir()
    state, pem = cfg / "state.json", cfg / "primary.pem"
    try:
        s = json.loads(state.read_text())
        p = s.get("primary") or {}
        if p.get("client_id") and p.get("installation_id") and pem.exists():
            return state, pem
    except (OSError, ValueError):
        pass
    return None


def main() -> int:
    env = H.rein_env()
    app = _real_app_files()
    if app is None:
        print("SKIP: no configured rein-init App (state.json primary + primary.pem) — "
              "this journey needs the box's own App to mint. Run `rein init` first. "
              "(Exit 3 = SKIPPED.)", flush=True)
        return 3
    state_src, pem_src = app
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)

    print(f"journey: rein init (real mint, no env) then rein run clone of {repo}", flush=True)

    # Isolated home seeded with the REAL App (state.json + keystore PEM) — exactly
    # what a rein-init box has, but confined to a tempdir. REIN_APP_* CLEARED so
    # init/run resolve from state.json, not env.
    home = H.isolated_home()
    cfg_dir = os.path.join(home, ".config", "rein")
    os.makedirs(cfg_dir, mode=0o700, exist_ok=True)
    shutil.copy(state_src, os.path.join(cfg_dir, "state.json"))
    dst_pem = os.path.join(cfg_dir, "primary.pem")
    shutil.copy(pem_src, dst_pem)
    os.chmod(dst_pem, 0o600)

    # A session scoped to the throwaway so the mint (init + run) has a repo scope.
    session_path = os.path.join(home, "session.yaml")
    with open(session_path, "w") as f:
        f.write("id: sess_init_then_run\nrole: implement\nrepos:\n"
                f"  - {repo}\n")

    # The direct clone lands in one fresh workdir; the sandboxed clone binds
    # another as its writable tree (REIN_SANDBOX_WORKDIR).
    direct_workdir = tempfile.mkdtemp(prefix="rein-journey-clone-")
    sbx_workdir = tempfile.mkdtemp(prefix="rein-journey-sbxclone-")

    step_env = {**H.isolated_home_env(home)}
    for k in _APP_ENV_KEYS:
        step_env[k] = ""              # no env App; resolve from the seeded state.json
    step_env["REIN_MACHINE_HOSTNAME"] = "demo-box"
    step_env["REIN_SESSION_FILE"] = session_path

    result = H.run_journey(
        [
            # init WITHOUT --skip-mint-check: the mint check proves the resolved
            # App actually works (mints a real read-only token), no env vars.
            H.JourneyStep(
                argv=["init", "--yes", "--no-alias", "--no-symlink"],
                label="rein init --yes --no-alias --no-symlink  (real mint check, App from state.json, NO env vars)",
                timeout=60,
            ),
            # DIRECT run: mint a git token + a real clone THROUGH the broker helper.
            H.JourneyStep(
                argv=["run", "--direct", "--", "git", "clone", "--quiet",
                      f"https://github.com/{repo}", "clone"],
                cwd=direct_workdir,
                label=f"rein run --direct -- git clone --quiet https://github.com/{repo} clone",
                timeout=120,
            ),
            # SANDBOXED run (the default mode): the same broker read, but the clone
            # runs INSIDE the srt sandbox. Proves both modes work after init.
            H.JourneyStep(
                argv=["run", "--", "bash", "-c", _sandbox_clone_script(repo), sbx_workdir],
                label=f"rein run -- bash -c <sandboxed git clone {repo}> <workdir>",
                extra_env={"REIN_SANDBOX_WORKDIR": sbx_workdir},
                timeout=180,
            ),
        ],
        env=env,
        extra_env=step_env,
    )
    text = result.transcript
    init_step, direct_step, sbx_step = result.steps

    invariants = [
        (result.reached_eof, "every driven command must run to completion"),
        (init_step.exitstatus == 0, f"init must exit 0; got {init_step.exitstatus}"),
        ("mint check: minting" in init_step.text and "ok (token expires" in init_step.text,
         "init must MINT a real token from the state.json App (no --skip-mint-check)"),
        ("missing env var" not in text and "keystore: entry not found" not in text,
         "no env-var demand and no dead-App keystore error (the #126 unblock)"),
        # NOTE: the clone invariants below prove a broker read only because the
        # throwaway is PRIVATE — an unauthenticated clone would 404, so a landed
        # .git means the broker minted + injected a token. The init leg's real
        # "mint check ... ok (token expires …)" corroborates that independently.
        # If the throwaway is ever made public these read-legs go vacuous.
        # DIRECT leg
        (direct_step.exitstatus == 0, f"direct rein run clone must exit 0; got {direct_step.exitstatus}"),
        (os.path.isdir(os.path.join(direct_workdir, "clone", ".git")),
         "direct: the clone must land a real .git (the broker-minted token drove a real read)"),
        # SANDBOXED leg
        ("@SBX_CLONE_RC=0" in sbx_step.text,
         "sandboxed: the in-sandbox `git clone` must succeed through the broker"),
        (os.path.isdir(os.path.join(sbx_workdir, "clone", ".git")),
         "sandboxed: the clone must land a real .git in the bound workdir"),
    ]
    broken = [msg for ok, msg in invariants if not ok]
    if broken:
        print("JOURNEY BROKE — init-then-run did not behave:", flush=True)
        for msg in broken:
            print(f"  - {msg}", flush=True)
        print("--- transcript ---", flush=True)
        print(text, flush=True)
        return 2

    print()
    print(text, flush=True)
    print("--- outcomes (asserted) ---", flush=True)
    print(f"  init:    minted a read-only token from the state.json App, NO env vars", flush=True)
    print(f"  direct:  `rein run --direct` minted a git token; `git clone {repo}` landed a real .git", flush=True)
    print(f"  sandbox: `rein run` (srt) cloned {repo} through the broker inside the sandbox", flush=True)

    if os.getenv("REIN_SHOW_NORMALIZED"):
        print("\n--- normalized (the comparison lens) ---", flush=True)
        print(H.normalize_for_compare(text), flush=True)

    if os.getenv("REIN_UPDATE_GOLDEN"):
        p = H.update_golden(GOLDEN, text)
        print(f"[golden UPDATED] {p} (raw)", flush=True)
        return 0

    ok, diff = H.compare_golden(GOLDEN, text)
    if ok:
        print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
        return 0
    scratch = os.path.join(tempfile.gettempdir(), "init_then_run.fresh.txt")
    with open(scratch, "w") as f:
        f.write(text)
    print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
