"""init_steady_state — `rein init` re-run in the manifest-flow STEADY STATE, no REIN_APP_*.

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: this journey does the OPPOSITE of the other init journeys — it CLEARS
REIN_APP_* (blanked to "", which config.LoadAppConfig treats as absent) so init takes
the state path, and seeds a SYNTHETIC manifest-flow state.json (+ dummy PEM) per leg.
Every leg runs `--yes --no-alias --no-symlink --skip-mint-check`: --yes keeps it
non-interactive, --skip-mint-check keeps it offline (the fake PEM is never read). No
dev-env, no real App/repo/network (hard-constraint #1).
"""

from __future__ import annotations

import json
import os
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"

# App-identity env vars this journey CLEARS so init takes the state path (the
# whole point). Blanked to "" — config.LoadAppConfig treats "" as absent, so
# DecideBridge sees env-absent and returns BridgeUseState.
_APP_ENV_KEYS = [
    "REIN_APP_CLIENT_ID",
    "REIN_APP_ID",
    "REIN_APP_INSTALLATION_ID",
    "REIN_APP_PRIVATE_KEY_PATH",
    "REIN_TEST_REPO_A",
    "REIN_TEST_REPO_B",
    "REIN_DEV_MODE",
]


def seed_manifest_state(home: str, installation_id: int) -> None:
    """Write a synthetic completed-manifest-flow state.json (+ managed PEM) into
    `home`'s config dir — exactly what a `rein init` manifest run would leave.

    installation_id > 0 is the cached steady state; 0 is App-not-yet-installed.
    The PEM is present (mode 0600) so the cached leg's key pre-flight passes;
    --skip-mint-check means its (dummy) contents are never read.
    """
    config_dir = os.path.join(home, ".config", "rein")
    os.makedirs(config_dir, mode=0o700, exist_ok=True)
    state = {
        "phase": "audit_done",
        "primary": {
            "slug": "rein-demo-primary",
            "client_id": "Iv23liDEMOclientID",
            "installation_id": installation_id,
            "created_at": "2026-01-01T00:00:00Z",
        },
        "schema_version": 1,
    }
    # installation_id 0 is the "uncached" sentinel; omit it so it round-trips as 0.
    if installation_id == 0:
        del state["primary"]["installation_id"]
    with open(os.path.join(config_dir, "state.json"), "w") as f:
        json.dump(state, f, indent=2)
    pem = os.path.join(config_dir, "primary.pem")
    with open(pem, "w") as f:
        f.write("dummy-not-a-real-key\n")
    os.chmod(pem, 0o600)


def step_env(home: str) -> dict:
    """Isolated HOME/XDG with REIN_APP_* CLEARED and a pinned machine label."""
    env = dict(H.isolated_home_env(home))
    for k in _APP_ENV_KEYS:
        env[k] = ""  # "" == absent to config.LoadAppConfig => state path
    env["REIN_MACHINE_HOSTNAME"] = "demo-box"
    return env


def main() -> int:
    env = H.rein_env()  # base env (for the go build); the steps CLEAR REIN_APP_*
    H.build_binaries(env)

    print("journey: rein init steady state (manifest-flow state.json, no REIN_APP_*)", flush=True)

    home_cached = H.isolated_home()
    home_uncached = H.isolated_home()
    home_stale = H.isolated_home()
    seed_manifest_state(home_cached, installation_id=12345)
    seed_manifest_state(home_uncached, installation_id=0)
    seed_manifest_state(home_stale, installation_id=12345)

    # The stale-PEM leg: identity vars still absent (so it's the state path), but
    # REIN_APP_PRIVATE_KEY_PATH is left over pointing at a file that does NOT
    # exist — the leftover a past `source ./dev-env` leaves behind. init must
    # validate the MANAGED keystore PEM (what the mint actually reads), not this
    # stale path; before the source-keyed pre-flight fix it false-failed here.
    stale_pem_path = "/nonexistent/rein-stale-dev-env-app.pem"
    stale_env = step_env(home_stale)
    stale_env["REIN_APP_PRIVATE_KEY_PATH"] = stale_pem_path

    init_flags = ["init", "--yes", "--no-alias", "--no-symlink", "--skip-mint-check"]
    result = H.run_journey(
        [
            H.JourneyStep(
                argv=init_flags,
                extra_env=step_env(home_cached),
                label="rein init --yes --no-alias --no-symlink --skip-mint-check  (state.json: audit_done, install-id cached)",
            ),
            H.JourneyStep(
                argv=init_flags,
                extra_env=step_env(home_uncached),
                label="rein init --yes --no-alias --no-symlink --skip-mint-check  (state.json: audit_done, install-id UNCACHED)",
            ),
            H.JourneyStep(
                argv=init_flags,
                extra_env=stale_env,
                label="rein init --yes --no-alias --no-symlink --skip-mint-check  (state path + STALE REIN_APP_PRIVATE_KEY_PATH)",
            ),
        ],
        env=env,
    )
    text = result.transcript
    cached, uncached, stale = result.steps

    # Invariants — the regression oracle for behavior, independent of the golden
    # (exit 2). Expected strings are INLINE LITERALS a reviewer reads right here.
    invariants = [
        (result.reached_eof, "every driven command must run to completion"),
        (cached.exitstatus == 0, f"cached-leg init must exit 0 with NO env vars; got {cached.exitstatus}"),
        (uncached.exitstatus == 0, f"uncached-leg init must exit 0 with NO env vars; got {uncached.exitstatus}"),
        # The bug's fingerprint must be ABSENT from BOTH legs.
        (
            "missing env var" not in text and "dev-env" not in text,
            "no leg may demand REIN_APP_* / mention dev-env (the fixed bug)",
        ),
        # CACHED: config resolved from state.json, app line printed with the
        # state's installation id (client_id/installation_id are normalized in the
        # golden, so assert the raw value here).
        (
            "state.json: audit_done (steady state from manifest flow)" in cached.text,
            "cached leg: init recognizes the manifest-flow steady state",
        ),
        (
            "installation_id=12345" in cached.text,
            "cached leg: App config resolved from state.json (installation_id=12345), not env",
        ),
        # UNCACHED: known intermediate state -> install hint, no app line, exit 0.
        (
            "not yet installed on a repo" in uncached.text
            and "rein-demo-primary" in uncached.text,
            "uncached leg: init prints the install-deep-link hint for the registered App",
        ),
        (
            "installation_id=" not in uncached.text,
            "uncached leg: init does NOT print an app config line (it awaits install first)",
        ),
        # STALE PEM: a leftover REIN_APP_PRIVATE_KEY_PATH must not false-fail the
        # state path — init validates the managed keystore PEM the mint reads.
        (
            stale.exitstatus == 0,
            f"stale-PEM leg: init must exit 0 despite a stale REIN_APP_PRIVATE_KEY_PATH; got {stale.exitstatus}",
        ),
        (
            "installation_id=12345" in stale.text and stale_pem_path not in stale.text,
            "stale-PEM leg: init resolves from state and never touches the stale env PEM path",
        ),
    ]
    broken = [msg for ok, msg in invariants if not ok]
    if broken:
        print("JOURNEY BROKE — steady-state init did not behave:", flush=True)
        for msg in broken:
            print(f"  - {msg}", flush=True)
        print("--- transcript ---", flush=True)
        print(text, flush=True)
        return 2

    print()
    print(text, flush=True)  # what actually happened, real output
    print("--- outcomes (asserted) ---", flush=True)
    print("  cached leg:    resolved App config from state.json (installation_id=12345), NO env vars", flush=True)
    print("  uncached leg:  printed the install hint and exited 0 (App not yet installed)", flush=True)
    print("  stale-PEM leg: ignored a stale REIN_APP_PRIVATE_KEY_PATH, resolved from state, exited 0", flush=True)

    if os.getenv("REIN_SHOW_NORMALIZED"):
        print("\n--- normalized (the comparison lens) ---", flush=True)
        print(H.normalize_for_compare(text), flush=True)

    if os.getenv("REIN_UPDATE_GOLDEN"):
        p = H.update_golden(GOLDEN, text)  # store RAW
        print(f"[golden UPDATED] {p} (raw)", flush=True)
        return 0

    ok, diff = H.compare_golden(GOLDEN, text)  # normalizes BOTH sides
    if ok:
        print(f"[golden OK] fresh run matches {GOLDEN} (normalized)", flush=True)
        return 0
    scratch = os.path.join(tempfile.gettempdir(), "init_steady_state.fresh.txt")
    with open(scratch, "w") as f:
        f.write(text)
    print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:", flush=True)
    print(diff, flush=True)
    print(f"raw fresh transcript written to {scratch}", flush=True)
    print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
