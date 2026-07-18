#!/usr/bin/env bash
# Containment probe harness driver (issue #136B, docs/containment-probe-harness.md).
#
# Method (from the design note):
#   1. Run sandbox-probe on the host, unconfined            -> host report
#   2. Run sandbox-probe through the REAL launch path        -> sandbox report
#      (rein run -- sandbox-probe ...), inheriting the exact scrubbed env/seccomp/binds
#   3. Diff; the delta is what confinement removed
#   4. Normalize both reports into the oracle's schema (tests/containment/README.md)
#   5. Oracle classifies each observation against rein's emitted settings.json
#   6. Emit the golden report; commit; a journey guards drift
#
# This script HARD-FAILS rather than fabricating results (task #136B: do NOT fake
# results). The pieces that are genuinely stubbed are marked TODO(#136B) and cause
# an explicit non-zero exit until wired.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
out_dir="${OUT_DIR:-$here/_out}"
mkdir -p "$out_dir"

die() { echo "run.sh: $*" >&2; exit 1; }

# --- Preconditions: real binaries, or fail closed (never fake) ---
SANDBOX_PROBE="${SANDBOX_PROBE:-$(command -v sandbox-probe || true)}"
REIN_BIN="${REIN_BIN:-$(command -v rein || true)}"

[ -n "$SANDBOX_PROBE" ] || die "sandbox-probe not found. Install github.com/controlplaneio/sandbox-probe (Apache-2.0) and set SANDBOX_PROBE=/path/to/sandbox-probe. It is a TEST-ONLY external process, never linked into the rein binary (hard-constraint #4)."
[ -x "$SANDBOX_PROBE" ] || die "SANDBOX_PROBE=$SANDBOX_PROBE is not executable"
[ -n "$REIN_BIN" ] || die "rein binary not found. Build it (go build -o rein ./cmd/rein) and set REIN_BIN=/path/to/rein."
[ -x "$REIN_BIN" ] || die "REIN_BIN=$REIN_BIN is not executable"

# Build the oracle CLI from source (in-module, no external deps).
classify_bin="$out_dir/classify"
( cd "$repo_root" && go build -o "$classify_bin" ./tests/containment/cmd/classify ) || die "failed to build the classify oracle CLI"

echo "run.sh: sandbox-probe=$SANDBOX_PROBE rein=$REIN_BIN"

# --- 1. Host baseline ---------------------------------------------------------
host_report="$out_dir/host-report.json"
echo "run.sh: [1/6] host baseline -> $host_report"
"$SANDBOX_PROBE" report --format json > "$host_report" || die "sandbox-probe host run failed"
# NOTE: 'report --format json' is a PLACEHOLDER invocation. Confirm sandbox-probe's
# real subcommand/flags once fetched and update here + below.

# --- 2. In-sandbox run through the real launch path ---------------------------
sandbox_report="$out_dir/sandbox-report.json"
echo "run.sh: [2/6] in-sandbox run -> $sandbox_report"
# TODO(#136B): wire the REAL launch. It must go through `rein run` so the probe
# inherits the exact scrubbed env, seccomp, and binds the agent gets — NOT a
# bespoke launcher (which would measure a different sandbox). Sketch:
#     "$REIN_BIN" run --workdir "$PWD" -- "$SANDBOX_PROBE" report --format json > "$sandbox_report"
# It also needs rein to expose the EMITTED settings.json for this run (step 5).
# Until both are confirmed against the current rein CLI, fail rather than fake.
die "TODO(#136B): in-sandbox launch + settings.json capture not yet wired. See comments above and README 'What is stubbed'."

# --- 3. Diff ------------------------------------------------------------------
# echo "run.sh: [3/6] diff host vs sandbox"
# diff <(jq -S . "$host_report") <(jq -S . "$sandbox_report") > "$out_dir/diff.txt" || true

# --- 4. Normalize into the oracle schema --------------------------------------
# TODO(#136B): map sandbox-probe's native JSON into tests/containment's normalized
# schema (network/files/env arrays; see README). This mapping is the one piece we
# cannot write until we see sandbox-probe's actual output shape. Emit:
#     normalized="$out_dir/observations.json"

# --- 5. Oracle classification -------------------------------------------------
# settings="$out_dir/settings.json"   # captured from the rein run in step 2
# "$classify_bin" -settings "$settings" -observations "$normalized" | tee "$out_dir/golden-report.txt"

# --- 6. Golden ----------------------------------------------------------------
# The classified report is the checked-in, human-reviewable artifact. Drift = red
# = re-review, same discipline as tests/interactive goldens.
