"""journey_sandbox_filesystem — THE SANDBOX FILESYSTEM BOUNDARY, from inside (#59/#63/#64).

This is ONE journey. For what a journey IS, the golden-transcript rule, and how to
author the next one, read tests/interactive/CLAUDE.md — none of that lives here.

Where journey_write_ceremony shows the GitHub-WRITE boundary (declare -> confirm ->
verified push), THIS journey shows the FILESYSTEM boundary the sandbox draws around
an agent — the thing a reviewer of #59/#63/#64 needs to SEE rather than infer from
green Go tests. It drives ONE real sandboxed `rein run` whose in-sandbox agent is a
deterministic `bash` script (NOT real claude — its phrasing must be reproducible),
and walks the whole boundary in one coherent "here's the sandbox from inside" story:

  * THE CONTRACT the agent is handed. A non-claude agent has no system-prompt
    channel, so rein PRINTS the contract into the agent's own output — so the golden
    shows, verbatim, the rules the agent was given ($HOME ephemeral, NO credentials,
    declare-then-push), plus the machine-readable REIN_IN_SANDBOX_* mirror.
  * CREDENTIALS + $HOME HIDDEN (#59). `cat ~/.ssh/id_rsa`, `cat ~/.aws/credentials`,
    and rein's OWN app key all read as "No such file or directory" — real secrets
    seeded on the HOST, invisible in-sandbox. `ls ~` reads the allowlisted view.
  * $HOME EPHEMERAL (#59). A write under $HOME SUCCEEDS in-sandbox (writable tmpfs)
    and reads back — then the host-side check confirms it NEVER touched the host.
  * THE .git HOST-EXEC ESCAPE, CLOSED (#64). In a bound writable checkout, the
    rename-parent evasion `mv .git .git.aside` fails "Device or resource busy" (.git
    is a pinned mountpoint), a direct `.git/hooks/pre-commit` plant fails read-only,
    and `git config --local core.pager <payload>` fails busy (.git/config is pinned).
  * ORDINARY WORK STILL WORKS. Editing a tracked file, `git add`, `git commit` all
    succeed — the hardening pins only .git/hooks + .git/config, not the whole tree.

DELIVERABLE: a RAW, human-reviewable transcript at golden/sandbox_filesystem.txt —
real paths, real error strings, real object counts, so Tom SEES exactly what the
agent experiences. Determinism does NOT live in the file: a fresh run is compared to
the golden by normalizing BOTH sides first (reinharness.compare_golden), so a
different working-tree tmpdir / commit hash / clone counts still matches while a
genuinely new or changed line trips drift.

    python3 tests/interactive/journey_sandbox_filesystem.py          # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_sandbox_filesystem.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_sandbox_filesystem.py # also print the compare lens

Exit 0 = the boundary held AND the normalized transcript matches the golden. Exit 1 =
drift (RAW fresh transcript dropped to a scratch path; NORMALIZED diff printed). Exit
2 = the boundary itself broke (a hide leaked, an escape opened, or ordinary work
failed).

NOTE: the real-claude contract read-back is deliberately NOT here — an LLM's phrasing
varies, so it can never be golden material; it stays as the gated interactive test
tests/interactive/test_agent_contract.py. This journey's agent is the deterministic
bash script above, whose output IS reproducible.

SELF-CONTAINED: seeds fake host credentials and cleans them up in a `finally` (only
the ones it created — never a pre-existing real key); clones the throwaway repo for
the writable checkout and removes it. Touches only the throwaway (hard-constraint #1).
The repo is resolved the rein-init way (reinharness.resolve_throwaway_repo).
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "sandbox_filesystem.txt"

# Real credential paths on the HOST. Each is seeded (if absent) with fake secret
# content so the "hidden" claim is honest — a real file that exists on the host yet
# reads as absent in-sandbox — and each is compared in-sandbox by its ABSOLUTE path
# (stable on this box, so it matches verbatim across runs; not normalized).
HOME = os.path.expanduser("~")
SSH_KEY = os.path.join(HOME, ".ssh", "id_rsa")
AWS_CREDS = os.path.join(HOME, ".aws", "credentials")
# rein's OWN GitHub App private key — the credential store the whole project exists
# to protect. If the agent could read THIS, the model would be broken.
REIN_KEY = os.path.join(HOME, ".config", "rein-credentials", "app.pem")
# The $HOME-ephemerality probe writes here; the host must never see it.
EPHEMERAL_PROBE = os.path.join(HOME, "notes.md")

TRACKED = "README.md"  # a tracked file in the throwaway (the ordinary-work edit target)


# --------------------------------------------------------------------------
# The in-sandbox agent script — a deterministic bash "agent", every step tagged
# --------------------------------------------------------------------------


def sandbox_script() -> str:
    """A `bash -c` body run as the srt child. It cannot be puppeted line-by-line
    (one sandboxed process), so each STEP emits a tagged `@PHASE..` sentinel and the
    test asserts on those IN SEQUENCE. Commands go through `run`
    (reinharness.sandbox_preamble): it echoes each as `SBX| $ <command>` then tags
    its output, so the transcript reads like a real terminal — command, output,
    command. `cd "$0"` enters the writable checkout mount rein passes as the final
    arg; the hardening pins ITS `.git`.
    """
    return f"""
{H.sandbox_preamble()}
cd "$0"
emit "@SANDBOX_FACTS  (the machine-readable mirror of the contract rein injected)"
run sh -c 'env | grep ^REIN_IN_SANDBOX | sort'

emit "@CREDS_HIDDEN  (real host secrets read as absent in-sandbox)"
run cat {SSH_KEY}
run cat {AWS_CREDS}
run cat {REIN_KEY}
run ls ~

emit "@HOME_EPHEMERAL  (a write under $HOME succeeds into tmpfs, discarded at run end)"
run sh -c 'echo scratch > {EPHEMERAL_PROBE} && cat {EPHEMERAL_PROBE}'

emit "@GIT_ESCAPE_CLOSED  (.git is a pinned mountpoint; hooks + config read-only)"
run mv .git .git.aside
emit "@MV_RC=$?"
run sh -c 'echo evil > .git/hooks/pre-commit'
emit "@HOOK_RC=$?"
run git config --local core.pager 'sh -c pwned'
emit "@CFG_RC=$?"

emit "@CLEAN_TREE  git status is clean — srt's injected dotfiles are HIDDEN (#102)"
run git status --porcelain
emit "@UNTRACKED=$(git status --porcelain 2>/dev/null | grep -c '^??' || true)"

emit "@ORDINARY_WORK  (edit a tracked file, add, commit — all still work)"
run sh -c 'echo "// rein sandbox-filesystem journey: an ordinary agent edit" >> {TRACKED}'
run git add -A
emit "@ADD_RC=$?"
run git commit -q -m "sandbox-filesystem journey: an ordinary edit"
emit "@COMMIT_RC=$?"
run git log --oneline -1
emit "@SCRIPT_DONE"
"""


# --------------------------------------------------------------------------
# Host-side setup / teardown
# --------------------------------------------------------------------------


def seed_fake_creds() -> list[str]:
    """Seed fake secrets at the host cred paths IF absent. Returns the paths we
    created (and must clean up) — never touches a pre-existing real key."""
    created: list[str] = []
    seeds = {
        SSH_KEY: "-----BEGIN OPENSSH PRIVATE KEY-----\nFAKE-JOURNEY-KEY-DO-NOT-USE\n-----END OPENSSH PRIVATE KEY-----\n",
        AWS_CREDS: "[default]\naws_access_key_id = AKIAFAKEJOURNEY\naws_secret_access_key = fake/journey/secret\n",
    }
    for path, content in seeds.items():
        if os.path.exists(path):
            continue  # a real file already lives here; leave it, it's hidden all the same
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            f.write(content)
        os.chmod(path, 0o600)
        created.append(path)
    return created


def clone_checkout(repo: str, env: dict) -> str:
    """A fresh normal checkout whose .git is a real dir -> fully hardenable. Named
    with a `rein-` prefix so its /tmp path normalizes to <TMP> in the compare."""
    d = tempfile.mkdtemp(prefix="rein-sbxfs-")
    subprocess.run(
        ["gh", "repo", "clone", repo, d, "--", "-q"],
        check=True, env=env, capture_output=True, text=True,
    )
    return d


def _pinned_session(repo: str) -> str:
    """A temp repo-only session so the journey never depends on the machine's
    ambient dev-session.yaml (and never trips the retired `issue:` warning)."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_sandbox_fs\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


# --------------------------------------------------------------------------
# The journey
# --------------------------------------------------------------------------


def _rc(text: str, name: str) -> int | None:
    m = re.search(rf"@{name}_RC=(\d+)", text)
    return int(m.group(1)) if m else None


def drive_journey(env, repo, workdir):
    """Drive the ONE sandboxed `rein run` through the shared runner (#82 / Tom's
    "run_journey is THE interface, single-run sandbox included" ruling).

    The sandbox launch is declared as a normal JourneyStep whose argv is the full
    `rein run -- bash -c <script> <workdir>`; a per-step `extra_env` points rein
    at the writable checkout (REIN_SANDBOX_WORKDIR) and pins the session, and a
    per-step `timeout` (180s) covers the slow srt launch. The in-sandbox script's
    `sandbox_preamble()`/`run` SBX| output is captured as session content, so the
    runner returns the COMPLETE session — banner, injected contract, and every
    tagged agent line — as `.transcript`, with no hand-slicing. This agent never
    declares, so the step has no answers (a declaring agent would list its
    Form-A (expect, answer) pairs here exactly like any other step).
    """
    step = H.JourneyStep(
        argv=["run", "--", "bash", "-c", sandbox_script(), workdir],
        # rein re-echoes the full script right below its banner, so keep the
        # boundary line concise instead of dumping the whole bash body twice.
        label=f"rein run -- bash -c <sandbox agent script> {workdir}",
        extra_env={
            "REIN_SESSION_FILE": _pinned_session(repo),
            "REIN_SANDBOX_WORKDIR": workdir,
        },
        timeout=180,
    )
    result = H.run_journey([step], env=env)
    # result.transcript is the whole session (already build_raw_transcript'd, with
    # the `$ rein run …` boundary line). result.steps[0].text is this step's raw
    # pty capture — used for the invariant scans below.
    return result, result.steps[0].text


def main() -> int:
    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)

    print(f"journey: sandbox filesystem boundary on {repo}", flush=True)

    created_creds = seed_fake_creds()
    # Snapshot the ephemeral-probe path's pre-existing state so the "did NOT persist"
    # check can't be fooled by a file that was already there.
    probe_pre_existed = os.path.exists(EPHEMERAL_PROBE)
    workdir = None
    try:
        workdir = clone_checkout(repo, env)
        result, text = drive_journey(env, repo, workdir)

        # ---- 1) The boundary must hold, independent of the golden. ----
        mv_rc = _rc(text, "MV")
        hook_rc = _rc(text, "HOOK")
        cfg_rc = _rc(text, "CFG")
        add_rc = _rc(text, "ADD")
        commit_rc = _rc(text, "COMMIT")
        m_unt = re.search(r"@UNTRACKED=(\d+)", text)
        untracked = int(m_unt.group(1)) if m_unt else None

        # $HOME ephemerality: the write must have SUCCEEDED in-sandbox (scratch read
        # back) but left NOTHING on the host.
        wrote_in_sandbox = f"{H.SBX_TAG}scratch" in text
        host_leaked = (not probe_pre_existed) and os.path.exists(EPHEMERAL_PROBE)

        def hidden(path: str) -> bool:
            return f"cat: {path}: No such file or directory" in text

        invariants = [
            (hidden(SSH_KEY), f"~/.ssh/id_rsa must be hidden in-sandbox ({SSH_KEY})"),
            (hidden(AWS_CREDS), f"~/.aws/credentials must be hidden in-sandbox ({AWS_CREDS})"),
            (hidden(REIN_KEY), f"rein's own app key must be hidden in-sandbox ({REIN_KEY})"),
            ("REIN_IN_SANDBOX_HOME=ephemeral" in text, "the ephemeral-$HOME fact must reach the agent"),
            (wrote_in_sandbox, "a write under $HOME must succeed in-sandbox (tmpfs)"),
            (not host_leaked, f"the in-sandbox $HOME write must NOT persist on the host ({EPHEMERAL_PROBE})"),
            (mv_rc not in (None, 0), "mv .git must FAIL (the rename-parent escape)"),
            ("Device or resource busy" in text, "mv .git must fail EBUSY (a pinned mountpoint)"),
            (hook_rc not in (None, 0), ".git/hooks/pre-commit must NOT be writable"),
            (cfg_rc not in (None, 0), "git config --local must fail (.git/config is pinned)"),
            (untracked == 0, "git status must be CLEAN — srt's injected dotfiles (.bashrc, "
                             ".gitconfig, .mcp.json, …) hidden so they aren't noise or an "
                             "accidental `git add -A` commit (#102)"),
            (add_rc == 0, "ordinary `git add` must still succeed in a hardened tree"),
            (commit_rc == 0, "ordinary `git commit` must still succeed in a hardened tree"),
            # The injected contract must have reached the agent's own output.
            ("agent contract PRINTED to the agent's output" in text,
             "the banner must report the printed-contract channel for a non-claude agent"),
            ("$HOME is EPHEMERAL." in text, "the contract's ephemeral-$HOME rule must be shown"),
            ("There are NO credentials in this environment." in text,
             "the contract's no-credentials rule must be shown"),
            ("rein declare <n>" in text, "the contract's declare rule must be shown"),
            ("agent/<n>/<nonce>" in text, "the contract's branch convention must be shown"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if not result.reached_eof:
            broken.append("the sandbox step did not run to EOF (timed out / prompt missed)")
        if broken:
            print("BOUNDARY BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print(f"  rcs: mv={mv_rc} hook={hook_rc} cfg={cfg_rc} add={add_rc} commit={commit_rc}", flush=True)
            print("--- transcript ---", flush=True)
            print(text, flush=True)
            return 2

        # ---- 2) Compare the WHOLE captured session NORMALIZED. ----
        # run_journey already returns the complete session build_raw_transcript'd
        # (banner + injected contract + tagged agent output, no slicing) as
        # .transcript — that IS the golden, straight from the ONE interface.
        raw = result.transcript
        print()
        print(raw, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  creds hidden in-sandbox: ~/.ssh/id_rsa, ~/.aws/credentials, rein app key", flush=True)
        print(f"  $HOME write succeeded in-sandbox and did NOT persist on host "
              f"({'absent' if not host_leaked else 'LEAKED!'} at {EPHEMERAL_PROBE})", flush=True)
        print(f"  .git escape closed: mv rc={mv_rc}, hook rc={hook_rc}, config rc={cfg_rc}", flush=True)
        print(f"  ordinary work: git add rc={add_rc}, git commit rc={commit_rc}", flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(raw), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN_NAME, raw)
            print(f"[golden UPDATED] {p} (raw)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN_NAME, raw)
        if ok:
            print("[golden OK] fresh run matches golden/sandbox_filesystem.txt (normalized)", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "sandbox_filesystem.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print("[golden DRIFT] fresh run != golden/sandbox_filesystem.txt (normalized) — re-review:", flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)", flush=True)
        return 1

    finally:
        if workdir and os.path.isdir(workdir):
            shutil.rmtree(workdir, ignore_errors=True)
        for path in created_creds:
            try:
                os.remove(path)
            except OSError:
                pass
        # If the ephemeral probe somehow landed on the host and we did NOT inherit
        # it, remove it — the sandbox should never have written it, but be tidy.
        if not probe_pre_existed and os.path.exists(EPHEMERAL_PROBE):
            try:
                os.remove(EPHEMERAL_PROBE)
            except OSError:
                pass
        print("cleanup: checkout removed" + (f"; seeded creds removed: {len(created_creds)}" if created_creds else ""),
              flush=True)


if __name__ == "__main__":
    sys.exit(main())
