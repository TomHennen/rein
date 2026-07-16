"""journey_claude_resume — the #94 claude sandbox trust model, PROVEN with a REAL claude.

This is ONE journey. For what a journey IS, the golden-transcript rule, the shared
runner, and how to author the next one, read tests/interactive/CLAUDE.md — none of
that lives here.

WHAT #94 CHANGED, and what this journey proves LIVE (real `claude`, headless -p/-c):

  rein used to allow the host's ~/.claude back into the sandbox read-only and re-hide
  a hardcoded list of history subdirs (fail-OPEN: a new subdir leaked until noticed).
  #94 FLIPS it: the host ~/.claude / ~/.claude.json are DEFAULT-DENIED, and claude is
  repointed at a rein-owned PERSISTENT overlay via CLAUDE_CONFIG_DIR. rein seeds only
  .credentials.json (fresh, host-side, every launch) and authors its own minimal
  settings.json; the overlay is bound read-WRITE and PERSISTS across runs.

Three claims, each shown by a step below:

  (a) AUTHENTICATED IN-SANDBOX. `claude -p` answers a prompt — it can only do that if
      rein seeded its OAuth creds into the overlay (host ~/.claude is fully denied).
  (b) RESUME ACROSS TWO REIN SESSIONS. Run 1 tells claude a fact; run 2 (`claude -c`)
      is a SEPARATE `rein run` and recalls it — proof the overlay persisted the
      session. If resume were broken, run 2 would have no memory of run 1.
  (c) THE HOST'S REAL ~/.claude STAYS HIDDEN. A deterministic bash probe, in the same
      sandbox, sees an EMPTY ~/.claude (the developer's cross-project history/
      history.jsonl is not readable) while the overlay (CLAUDE_CONFIG_DIR) holds the
      seeded creds — the credential-boundary property still holds under the new model.

THREE ARTIFACTS SHAPE (see tests/interactive/CLAUDE.md): a REAL LLM is not
deterministic, so its prose is NEVER golden material.
  1. INVARIANTS — plain asserts, in code (below). The recall step recalled the magic
     word (resume), the probe saw the host tree empty + history unreadable (hiding) +
     the overlay creds seeded (auth). These are the regression oracle; a break is
     exit 2. Nothing about claude's wording is asserted.
  2. THE COMPARED GOLDEN (golden/claude_resume.txt) — DETERMINISTIC content only,
     built two ways by agent kind (split_at_agent_launch doctrine, CLAUDE.md rule #2):
     the two REAL-claude steps keep rein's launch surface VERBATIM through its
     `rein: running:` echo (so the claude-specific `--append-system-prompt` contract
     line is compared, not silently dropped by a prefix grep) then rein's own lines
     only; the DETERMINISTIC bash probe keeps its full SBX|-tagged transcript, like
     journey_sandbox_filesystem. claude's own -p/-c stdout is EXCLUDED, so a completely
     different claude session still compares clean. The magic word is a FIXED phrase
     (not a per-run nonce) precisely so run 1's `rein: running:` echo stays stable in
     the golden; the resume PROOF is the invariant, which reads run 2's live output.
     The golden is generated against the `rein init` keystore (healthy_app_env), matching
     every other sandbox journey — so no box-specific install-coverage warning bakes in.

Unlike the real-agent WRITE journey, this one needs NO tmux/pyte: `claude -p`/`-c`
are headless and line-oriented, so it drives three ordinary `rein run` steps through
the shared run_journey and reads rein's own lines back out. It DOES need `claude` on
PATH and a host claude login (~/.claude/.credentials.json) to seed — without either
there is nothing to exercise, so it SKIPs with exit 3 (a skip must never look like a
pass — the #68 footgun).

    python3 tests/interactive/journey_claude_resume.py            # exit 0 == matches (normalized)
    REIN_UPDATE_GOLDEN=1 python3 tests/interactive/journey_claude_resume.py   # write the RAW golden
    REIN_SHOW_NORMALIZED=1 python3 tests/interactive/journey_claude_resume.py

Exit 0 = the three claims held AND the normalized transcript matches the golden.
Exit 1 = golden drift. Exit 2 = a claim itself broke (no resume, a host leak, or
unauthenticated). Exit 3 = SKIPPED (`claude` or a host login absent).

QUOTA: this launches TWO real `claude` invocations and spends real API tokens; the
prompts are one line each on purpose.

SELF-CONTAINED: clones the throwaway repo for the writable checkout and removes it in
a `finally`; pins its own repo-only session so it never depends on the machine's
dev-session.yaml. It writes to rein's OWN persistent overlay (~/.config/rein-sandbox-
home/.claude) — that dir is the deliverable's persistent store and is meant to
survive, so it is deliberately NOT cleaned. Touches only the throwaway repo
(hard-constraint #1); the repo is resolved the rein-init way (resolve_throwaway_repo).
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile

import reinharness as H

GOLDEN_NAME = "claude_resume.txt"

# A FIXED, distinctive phrase (not a per-run nonce): run 2 can only produce it by
# RESUMING run 1's session, and being fixed keeps run 1's `rein: running:` echo stable
# in the compared golden. run 2's prompt never contains it — recalling it is the proof.
MAGIC_WORD = "quokka-overlay-persists-1994"


def store_prompt() -> str:
    return (
        f"Remember this exact token for later, I will ask for it: {MAGIC_WORD}. "
        f"Reply with just the word 'ok'."
    )


def recall_prompt() -> str:
    return (
        "Earlier in this conversation I gave you an exact token to remember. "
        "Reply with ONLY that token, nothing else."
    )


# The deterministic bash probe (claim c): every line SBX|-tagged (sandbox_preamble),
# so it lands in the compared golden and reads like a terminal session. It asserts the
# host tree is hidden and the overlay is usable — all via stable, sortable output.
def probe_script() -> str:
    return f"""
{H.sandbox_preamble()}
emit "@CLAUDE_CONFIG_DIR=$CLAUDE_CONFIG_DIR"
emit "@HOST_CLAUDE_ENTRIES=[$(ls -A ~/.claude 2>/dev/null | sort | tr '\\n' ' ')]"
emit "@HOST_HISTORY_JSONL_READABLE=$(test -r ~/.claude/history.jsonl && echo YES-LEAK || echo no)"
emit "@HOST_CLAUDE_JSON_READABLE=$(test -s ~/.claude.json && echo YES-LEAK || echo no)"
emit "@OVERLAY_CREDS_SEEDED=$(test -s "$CLAUDE_CONFIG_DIR/.credentials.json" && echo yes || echo no)"
emit "@OVERLAY_SETTINGS=$(tr -d '\\n ' < "$CLAUDE_CONFIG_DIR/settings.json")"
"""


def pinned_session(repo: str) -> str:
    """A temp repo-only session so the journey never depends on the machine's ambient
    dev-session.yaml (mirrors journey_realagent_write._pinned_session)."""
    d = tempfile.mkdtemp(prefix="rein-journey-sess-")
    path = os.path.join(d, "session.yaml")
    with open(path, "w") as f:
        f.write("id: sess_journey_claude_resume\nrole: implement\nrepos:\n" f"  - {repo}\n")
    return path


def clone_checkout(repo: str, env: dict) -> str:
    """A fresh normal checkout (a real `.git` DIR -> fully hardenable, so rein binds it
    writable). `rein-` prefix so its /tmp path normalizes to <TMP> in the compare."""
    d = tempfile.mkdtemp(prefix="rein-claude-resume-")
    subprocess.run(
        ["gh", "repo", "clone", repo, d, "--", "-q"],
        check=True, env=env, capture_output=True, text=True,
    )
    return d


def host_logged_in() -> bool:
    """The seed source: the host's real claude login. Without it rein cannot seed the
    overlay and claude would be unauthenticated in-sandbox — there is nothing to prove."""
    p = os.path.join(os.path.expanduser("~"), ".claude", ".credentials.json")
    try:
        return os.path.getsize(p) > 0
    except OSError:
        return False


def healthy_app_env(env: dict) -> dict:
    """Resolve the App from the `rein init` STATE path (state.json + the populated
    keystore[primary]), the documented journey world (tests/interactive/CLAUDE.md),
    rather than dev-env's REIN_APP_* env path. On a box whose dev-env points at an
    App key that is NOT present, the env path resolves but then fails the
    install-coverage probe with a `keystore[primary]: entry not found` warning —
    box-specific noise that would bake into the golden. Every OTHER sandbox journey's
    golden is clean because it runs against the populated keystore; this matches that.

    Only strips the REIN_APP_* override when a state.json actually exists (so a
    dev-env-only box still resolves via the env path). REIN_TEST_REPO_A and the rest
    of the env are untouched."""
    cfg_base = env.get("XDG_CONFIG_HOME") or os.path.join(os.path.expanduser("~"), ".config")
    if not os.path.exists(os.path.join(cfg_base, "rein", "state.json")):
        return env
    e = dict(env)
    for k in ("REIN_APP_ID", "REIN_APP_CLIENT_ID", "REIN_APP_INSTALLATION_ID",
              "REIN_APP_PRIVATE_KEY_PATH"):
        e.pop(k, None)
    return e


# THE COMPARED GOLDEN — deterministic content only. Two shapes, one per agent kind
# (tests/interactive/CLAUDE.md rule #2, and the split_at_agent_launch doctrine):
#   - the two REAL-claude steps: rein's launch surface VERBATIM through its
#     `rein: running:` echo, then rein's own lines only (split_at_agent_launch). Keeping
#     the launch surface whole — not a `rein: `-prefix grep — is load-bearing: rein's
#     banner body is INDENTED, and the claude-specific `--append-system-prompt` contract
#     line would silently stop being compared under a prefix filter. claude's own -p/-c
#     stdout is excluded, so a different claude session still compares clean.
#   - the DETERMINISTIC bash probe step: its full raw transcript, SBX|-tagged, exactly
#     like journey_sandbox_filesystem (reproducible, so it belongs in the golden whole).
def compared_golden(result, store_needle: str, recall_needle: str) -> tuple[str, bool]:
    def rein_only(label: str, step_text: str, needle: str) -> tuple[list[str], bool]:
        launch, tail, found = H.split_at_agent_launch(
            H.build_raw_transcript(step_text), needle)
        return [f"$ {label}"] + launch + tail, found

    lines0, f0 = rein_only("rein run -- claude -p <store the magic word>",
                           result.steps[0].text, store_needle)
    lines1, f1 = rein_only("rein run -- claude -c -p <recall the magic word>",
                           result.steps[1].text, recall_needle)
    probe = ["$ rein run -- bash -c <host-hidden / overlay-used probe> <workdir>"]
    probe += H.build_raw_transcript(result.steps[2].text).split("\n")
    text = "\n".join(lines0 + lines1 + probe).strip("\n") + "\n"
    return text, (f0 and f1)


def main() -> int:
    if shutil.which("claude") is None:
        print("SKIP: `claude` is not on PATH — this journey IS a real-claude resume run, "
              "so there is nothing to exercise without it. (Exit 3 = SKIPPED.)", flush=True)
        return 3
    if not host_logged_in():
        print("SKIP: no host claude login (~/.claude/.credentials.json) to seed into the "
              "overlay — without it claude is unauthenticated in-sandbox and there is "
              "nothing to prove. Run `claude` once to log in. (Exit 3 = SKIPPED.)", flush=True)
        return 3

    env = H.rein_env()
    repo = H.resolve_throwaway_repo(env)
    H.build_binaries(env)
    session = pinned_session(repo)

    print(f"journey: REAL-claude overlay resume on {repo} (#94 default-deny + persistent "
          f"CLAUDE_CONFIG_DIR overlay)", flush=True)

    workdir = None
    try:
        workdir = clone_checkout(repo, env)
        step_env = {"REIN_SESSION_FILE": session, "REIN_SANDBOX_WORKDIR": workdir}

        result = H.run_journey(
            steps=[
                # (a)+(b) store: a real claude records the magic word in the overlay session.
                H.JourneyStep(
                    argv=["run", "--", "claude", "-p", store_prompt()],
                    label="rein run -- claude -p <store the magic word>",
                    cwd=workdir, extra_env=step_env, timeout=240,
                ),
                # (b) resume: a SEPARATE rein run; `claude -c` continues the overlay session.
                H.JourneyStep(
                    argv=["run", "--", "claude", "-c", "-p", recall_prompt()],
                    label="rein run -- claude -c -p <recall the magic word>",
                    cwd=workdir, extra_env=step_env, timeout=240,
                ),
                # (c) hiding: a deterministic bash probe proves host ~/.claude is hidden
                # and the overlay is the one claude uses.
                H.JourneyStep(
                    argv=["run", "--", "bash", "-c", probe_script(), workdir],
                    label="rein run -- bash -c <host-hidden / overlay-used probe> <workdir>",
                    cwd=workdir, extra_env=step_env, timeout=180,
                ),
            ],
            env=healthy_app_env(env),  # populated keystore[primary], no coverage warning
            timeout=240,
        )

        raw, launch_found = compared_golden(result, store_prompt(), recall_prompt())
        recall_text = result.steps[1].text if len(result.steps) > 1 else ""
        probe_text = result.steps[2].text if len(result.steps) > 2 else ""

        # ---- 1) The three claims must hold, independent of the golden. ----
        invariants = [
            (result.reached_eof,
             "every rein run must reach EOF (no step hung / timed out)"),
            (launch_found,
             "rein's `running:` launch echo must be in BOTH claude steps — it is the "
             "boundary between rein's launch surface and claude's own output, and "
             "without it the golden would be silently truncated"),
            (MAGIC_WORD in recall_text,
             f"RESUME: run 2 (`claude -c`, a separate rein run) must recall {MAGIC_WORD!r} "
             f"from run 1 via the persistent overlay — it is not in run 2's prompt, so "
             f"recalling it proves the overlay session persisted"),
            ("@HOST_CLAUDE_ENTRIES=[]" in probe_text,
             "HIDING: the host's real ~/.claude must read as EMPTY in-sandbox (its "
             "cross-project history is denied)"),
            ("@HOST_HISTORY_JSONL_READABLE=no" in probe_text,
             "HIDING: the developer's ~/.claude/history.jsonl must NOT be readable "
             "in-sandbox"),
            ("@HOST_CLAUDE_JSON_READABLE=no" in probe_text,
             "HIDING: the host's ~/.claude.json must NOT be readable in-sandbox"),
            ("@OVERLAY_CREDS_SEEDED=yes" in probe_text,
             "AUTH: rein must have seeded .credentials.json into the overlay "
             "(CLAUDE_CONFIG_DIR) so claude authenticates"),
            ("skipDangerousModePermissionPrompt" in probe_text,
             "the overlay must carry rein's own minimal settings.json"),
        ]
        broken = [msg for ok, msg in invariants if not ok]
        if broken:
            print("CLAIM BROKE:", flush=True)
            for m in broken:
                print(f"  - {m}", flush=True)
            print("--- run 2 (recall) live output ---", flush=True)
            print(recall_text, flush=True)
            print("--- probe output ---", flush=True)
            print(probe_text, flush=True)
            print("--- rein's own output (the compared golden's content) ---", flush=True)
            print(raw, flush=True)
            return 2

        print()
        print(raw, flush=True)
        print("--- outcomes (asserted; not in the golden) ---", flush=True)
        print(f"  RESUME: run 2 recalled {MAGIC_WORD!r} from run 1's overlay session "
              f"(two separate `rein run` invocations)", flush=True)
        print("  HIDING: host ~/.claude read as EMPTY in-sandbox; history.jsonl + "
              "~/.claude.json not readable", flush=True)
        print("  AUTH: overlay .credentials.json seeded; claude authenticated in-sandbox",
              flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(raw), flush=True)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN_NAME, raw)
            print(f"[golden UPDATED] {p} (raw; COMPARED — rein's lines + the probe's "
                  f"SBX| output, no claude content)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN_NAME, raw)
        if ok:
            print(f"[golden OK] fresh run matches golden/{GOLDEN_NAME} (normalized) — a "
                  f"DIFFERENT claude session still compares clean", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "claude_resume.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != golden/{GOLDEN_NAME} (normalized) — re-review:",
              flush=True)
        print(diff, flush=True)
        print(f"raw fresh transcript written to {scratch}", flush=True)
        print("(if the change is intended: REIN_UPDATE_GOLDEN=1 to adopt the new RAW golden)",
              flush=True)
        return 1

    finally:
        if workdir and os.path.isdir(workdir):
            shutil.rmtree(workdir, ignore_errors=True)
        print("cleanup: checkout removed (the rein overlay persists by design)", flush=True)


if __name__ == "__main__":
    sys.exit(main())
