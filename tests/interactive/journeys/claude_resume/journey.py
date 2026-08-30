"""claude_resume — the #94 claude sandbox trust model, PROVEN with a REAL claude.

See README.md for the full description; journey-authoring rules are in
tests/interactive/CLAUDE.md.

Code note: `claude -p`/`-c` are headless and line-oriented, so this needs NO tmux/pyte
— it drives three ordinary `rein run` steps through run_journey. A real LLM's prose is
NEVER golden material: the two claude steps contribute only rein's own launch surface
(split_at_agent_launch, so the claude-specific `--append-system-prompt` contract line
is compared, not dropped by a prefix grep); the resume PROOF is an INVARIANT that reads
run 2's live output; and the deterministic bash probe's SBX| output is golden whole
(like journeys/sandbox_filesystem). The magic word is a FIXED phrase so run 1's
`rein: running:` echo stays stable in the golden.

TWO artifacts (the realagent_write pattern): golden.txt is COMPARED (rein's surface
only — claude's stdout excluded, so a live model can't flake a byte-diff); session.txt
is SHOWN, never compared — it captures claude's ACTUAL replies (run 1's 'ok', run 2's
recalled token), the VISIBLE resume evidence a reviewer wants to read. Both are
regenerated under REIN_UPDATE_GOLDEN=1.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

from pathlib import Path
from tests.interactive import reinharness as H

GOLDEN = Path(__file__).parent / "golden.txt"       # COMPARED (rein's surface only)
SESSION = Path(__file__).parent / "session.txt"     # SHOWN, never compared (claude's replies)

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
emit "@HOST_CLAUDE_JSON_READABLE=$(test -s ~/.claude/claude.json -a -r ~/.claude/claude.json && echo YES-LEAK || echo no)"
emit "@HOST_CLAUDE_SENSITIVE_READABLE=[$(for f in history.jsonl claude.json .credentials.json settings.json projects sessions todos shell-snapshots; do test -s ~/.claude/"$f" -a -r ~/.claude/"$f" && printf '%s ' "$f"; done)]"
emit "@HOST_CLAUDE_WRITABLE=[$(for e in $(ls -A ~/.claude 2>/dev/null | sort); do p=~/.claude/"$e"; if test -d "$p"; then (touch "$p/.rein-write-probe" 2>/dev/null && rm -f "$p/.rein-write-probe" && printf '%s ' "$e"); elif test ! -c "$p" && test -w "$p"; then printf '%s ' "$e"; fi; done)]"
emit "@HOST_NPM_LOGS_WRITABLE=$(touch ~/.npm/_logs/.rein-write-probe 2>/dev/null && rm -f ~/.npm/_logs/.rein-write-probe && echo YES-PERSISTS || echo no)"
emit "@OVERLAY_CREDS_SEEDED=$(test -s "$CLAUDE_CONFIG_DIR/.credentials.json" && echo yes || echo no)"
emit "@OVERLAY_FORCES_SKIPDANGEROUS=$(grep -rqs skipDangerousModePermissionPrompt "$CLAUDE_CONFIG_DIR" && echo YES-BYPASS || echo no)"
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


def overlay_dir(env: dict) -> str:
    """rein's persistent CLAUDE_CONFIG_DIR overlay parent (the sibling of ConfigDir,
    config.SandboxClaudeHomeDir's parent). Cleaned at journey start so the run is
    deterministic and isolated from prior box state (e.g. a stale settings.json from an
    older rein). The resume proof still holds: run 1 creates the session run 2 resumes,
    both WITHIN this journey."""
    base = env.get("XDG_CONFIG_HOME") or os.path.join(os.path.expanduser("~"), ".config")
    return os.path.join(base, "rein-sandbox-home")


# The onboarding keys claude writes into the overlay's .claude.json once its
# first-run flow completes. The wipe must NOT take these with it (#151): claude
# 2.1.251 answers a virgin overlay with the theme picker and then a LOGIN screen
# that wants a browser OAuth round-trip, which no journey (and no sandbox) can
# complete — so wiping them leaves every later interactive run stuck on a wizard.
# rein deliberately authors no claude config, so this is the test layer's job.
ONBOARDING_KEYS = ("theme", "hasCompletedOnboarding", "lastOnboardingVersion")


def reset_overlay_preserving_onboarding(env: dict) -> dict:
    """Wipe the overlay for determinism, but carry the onboarding keys across.

    Returns the preserved keys (empty when the box has none yet, e.g. a first
    run — that box still hits the wizard, which is #151's open half)."""
    parent = overlay_dir(env)
    cfg = os.path.join(parent, ".claude", ".claude.json")
    kept: dict = {}
    try:
        with open(cfg) as fh:
            data = json.load(fh)
        kept = {k: data[k] for k in ONBOARDING_KEYS if k in data}
    except (OSError, ValueError):
        kept = {}

    shutil.rmtree(parent, ignore_errors=True)
    if kept:
        claude_dir = os.path.join(parent, ".claude")
        os.makedirs(claude_dir, mode=0o700, exist_ok=True)
        with open(cfg, "w") as fh:
            json.dump(kept, fh)
        os.chmod(cfg, 0o600)
    return kept


def host_logged_in() -> bool:
    """The seed source: the host's real claude login. Without it rein cannot seed the
    overlay and claude would be unauthenticated in-sandbox — there is nothing to prove."""
    p = os.path.join(os.path.expanduser("~"), ".claude", ".credentials.json")
    try:
        return os.path.getsize(p) > 0
    except OSError:
        return False


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


# Terminal reset/mode sequences claude emits on exit that strip_ansi leaves behind
# (charset ESC(B, kitty-keyboard ESC[<u, private ESC[>4m, cursor save/restore ESC7/ESC8,
# stray C0 controls). Scrubbed so session.txt is clean prose for a human reviewer.
_TERM_JUNK = re.compile(
    r"\x1b\[[0-9;<>?=]*[ -/]*[@-~]"  # CSI (incl. private > < ? =)
    r"|\x1b[ -/]*[0-~]"             # other ESC sequences (ESC(B, ESC7, ESC8, …)
    r"|[\x00-\x08\x0b-\x1f\x7f]"     # stray C0 controls (TAB/newline kept)
)


# claude's OWN reply for a -p/-c step: everything AFTER rein's `rein: running:` echo,
# minus rein's own trailing lines and terminal-reset junk. For session.txt only (SHOWN,
# never compared), so a loose extract is fine — the point is a human SEES claude's actual
# words, which the compared golden deliberately excludes (a live model would flake a diff).
def agent_reply(step_text: str, needle: str) -> str:
    lines = H.build_raw_transcript(step_text).split("\n")
    i = next((n for n, ln in enumerate(lines) if needle in ln), None)
    tail = lines[i + 1:] if i is not None else lines
    out = []
    for ln in tail:
        if H.REIN_LINE_RE.match(ln) or ln.strip() == "---":
            continue
        clean = _TERM_JUNK.sub("", ln).strip()
        if clean:
            out.append(clean)
    return "\n".join(out).strip() or "(no reply captured)"


def agent_session_text(result) -> str:
    """The SHOWN-not-compared record of claude's replies (the realagent_write pattern):
    run 1's reply ('ok') and run 2's reply — the RESUMED token — the VISIBLE resume
    evidence golden.txt keeps out on purpose."""
    return "\n".join([
        "This is the REAL claude's session — SHOWN, NOT COMPARED.",
        "",
        "Regenerated on every REIN_UPDATE_GOLDEN=1 adopt; never diffed (a live LLM's exact",
        "wording is not a regression signal — the resume PROOF is an INVARIANT in journey.py,",
        "a break is exit 2). It exists so a human can SEE claude's actual replies, which the",
        "compared golden.txt excludes on purpose (a live model would flake a byte-diff).",
        "",
        "--- run 1: `claude -p <store the magic word>` — claude's reply ---",
        agent_reply(result.steps[0].text, store_prompt()),
        "",
        "--- run 2: `claude -c -p <recall the magic word>` — claude's reply (the RESUMED "
        "token, recalled from run 1's overlay session) ---",
        agent_reply(result.steps[1].text, recall_prompt()),
        "",
    ]) + "\n"


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

    # Start from a clean overlay so the run is deterministic and isolated from prior
    # box state (resume is still proven WITHIN this journey: run 1 → run 2). Onboarding
    # state is the one thing carried across — see reset_overlay_preserving_onboarding.
    kept = reset_overlay_preserving_onboarding(env)
    if not kept:
        print("note: the overlay had no onboarding state to preserve — an INTERACTIVE "
              "claude in-sandbox will hit the first-run wizard (#151). Headless (-p), "
              "which this journey drives, is unaffected.", flush=True)

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
            env=env,  # rein_env resolves the App from state.json (#128); no dev-env
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
            # NOT an empty-listing assertion (#150): rein's own deny of the
            # ~/.claude.json SYMLINK lands a /dev/null tombstone at its resolved
            # target ~/.claude/claude.json, INSIDE the denied dir, so the listing
            # is legitimately non-empty. What the claim actually is: nothing there
            # is readable, and nothing there is writable.
            ("@HOST_CLAUDE_SENSITIVE_READABLE=[]" in probe_text,
             "HIDING: no sensitive entry under the developer's real ~/.claude may be "
             "READABLE in-sandbox (history, config, credentials, settings, per-project "
             "state) — the listing may be non-empty, the CONTENT may not be reachable"),
            ("@HOST_CLAUDE_WRITABLE=[]" in probe_text,
             "CONTAINMENT (#153): nothing under the developer's real ~/.claude may be "
             "WRITABLE in-sandbox. srt re-binds its own getDefaultWritePaths() over "
             "rein's $HOME deny, so ~/.claude/debug came back writable — agent writes "
             "persisted on the host and could repoint the `latest` symlink the host's "
             "claude writes through"),
            ("@HOST_NPM_LOGS_WRITABLE=no" in probe_text,
             "CONTAINMENT (#153): the host's ~/.npm/_logs is the other srt default "
             "write path under $HOME and must not be writable in-sandbox either"),
            ("@HOST_HISTORY_JSONL_READABLE=no" in probe_text,
             "HIDING: the developer's ~/.claude/history.jsonl must NOT be readable "
             "in-sandbox"),
            ("@HOST_CLAUDE_JSON_READABLE=no" in probe_text,
             "HIDING: the host's global claude config must NOT be readable in-sandbox "
             "(claude 2.1.x relocated it to ~/.claude/claude.json; ~/.claude.json is "
             "now just a symlink to it). Readability is tested as READABLE AND "
             "NON-EMPTY on purpose: rein's deny renders as a /dev/null tombstone at "
             "that path, which is world-rw by mode and yields nothing — mode bits "
             "would report a leak that does not exist (#150)"),
            ("@OVERLAY_CREDS_SEEDED=yes" in probe_text,
             "AUTH: rein must have seeded .credentials.json into the overlay "
             "(CLAUDE_CONFIG_DIR) so claude authenticates"),
            ("@OVERLAY_FORCES_SKIPDANGEROUS=no" in probe_text,
             "POSTURE: rein must NOT author a permission-bypassing settings.json — "
             "claude keeps its own permission prompts in-sandbox (defense-in-depth on "
             "top of the boundary; rein does not launch --dangerously-skip-permissions)"),
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
        print("  HIDING: nothing sensitive under host ~/.claude is readable in-sandbox "
              "(history.jsonl + the relocated claude.json included)", flush=True)
        print("  CONTAINMENT: nothing under host ~/.claude or ~/.npm/_logs is writable "
              "in-sandbox (#153: srt's default write paths are denied back)", flush=True)
        print("  AUTH: overlay .credentials.json seeded; claude authenticated in-sandbox",
              flush=True)

        if os.getenv("REIN_SHOW_NORMALIZED"):
            print("\n--- normalized (the comparison lens) ---", flush=True)
            print(H.normalize_for_compare(raw), flush=True)

        session = agent_session_text(result)

        if os.getenv("REIN_UPDATE_GOLDEN"):
            p = H.update_golden(GOLDEN, raw)
            s = H.write_agent_session(SESSION, session)
            print(f"[golden UPDATED] {p} (raw; COMPARED — rein's lines + the probe's "
                  f"SBX| output, no claude content)", flush=True)
            print(f"[session UPDATED] {s} (SHOWN, never compared — claude's actual "
                  f"replies incl. the recalled token)", flush=True)
            return 0

        ok, diff = H.compare_golden(GOLDEN, raw)
        if ok:
            print(f"[golden OK] fresh run matches {GOLDEN} (normalized) — a "
                  f"DIFFERENT claude session still compares clean", flush=True)
            print(f"  claude's replies (SHOWN, not compared): {SESSION}", flush=True)
            return 0
        scratch = os.path.join(tempfile.gettempdir(), "claude_resume.fresh.txt")
        with open(scratch, "w") as f:
            f.write(raw)
        print(f"[golden DRIFT] fresh run != {GOLDEN} (normalized) — re-review:",
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
