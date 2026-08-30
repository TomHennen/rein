#!/usr/bin/env bash
# Containment probe harness driver (issue #136B/#141; tests/containment/README.md).
#
#   1. sandbox-probe on the host, unconfined                    -> host baseline
#   2. classify -targets from a captured settings.json          -> probe targets
#      (config-derived: allowlists, deny paths, srt's own default write paths)
#   3. `rein run --sandbox` runs sandbox-probe + the target probes IN-SANDBOX,
#      writing results into the bound working tree
#   4. host-side write-persistence check (#153 channel)
#   5. classify judges every observation against the emitted settings.json
#   6. compare to the committed golden (REIN_UPDATE_GOLDEN=1 adopts)
#
# Any leak => classify exits 3 => this script fails. Golden drift also fails.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
out_dir="${OUT_DIR:-$here/_out}"
golden="$here/golden-report.txt"
mkdir -p "$out_dir"

die() { echo "run.sh: $*" >&2; exit 1; }

SANDBOX_PROBE="${SANDBOX_PROBE:-$(command -v sandbox-probe || echo "$HOME/go/bin/sandbox-probe")}"
[ -x "$SANDBOX_PROBE" ] || die "sandbox-probe not found/executable ($SANDBOX_PROBE). Install: go install github.com/controlplaneio/sandbox-probe@latest (Apache-2.0; external process only, never in go.mod — hard-constraint #4)."
REIN_BIN="${REIN_BIN:-$repo_root/bin/rein}"
[ -x "$REIN_BIN" ] || die "rein binary not found at $REIN_BIN (go build -o bin/ ./cmd/... , or set REIN_BIN)"

classify_bin="$out_dir/classify"
( cd "$repo_root" && go build -o "$classify_bin" ./tests/containment/cmd/classify ) || die "failed to build the classify oracle CLI"

echo "run.sh: sandbox-probe=$SANDBOX_PROBE rein=$REIN_BIN out=$out_dir"

# --- 1. Host baseline ---------------------------------------------------------
host_report="$out_dir/host-report.json"
echo "run.sh: [1/6] host baseline -> $host_report"
"$SANDBOX_PROBE" scan --fast --output_path "$host_report" >/dev/null 2>&1 || die "sandbox-probe host run failed"

# --- 2. Config capture + targets ---------------------------------------------
# The settings.json for THIS box is captured from a minimal sandboxed run
# (REIN_SRT_SETTINGS_COPY), then targets are derived from it. The main probe run
# below captures its own settings again — same box, same config; the two-step
# exists only because targets must be in the working tree BEFORE launch.
wd="$(mktemp -d /tmp/rein-containment-XXXXXX)"
trap 'rm -rf "$wd"' EXIT
settings="$out_dir/settings.json"
echo "run.sh: [2/6] capture settings + derive targets"
REIN_SRT_SETTINGS_COPY="$settings" REIN_SANDBOX_WORKDIR="$wd" \
  "$REIN_BIN" run --sandbox -- true >/dev/null 2>&1 || die "settings-capture run failed"
[ -s "$settings" ] || die "REIN_SRT_SETTINGS_COPY produced no settings.json (rein too old?)"
"$classify_bin" -settings "$settings" -targets > "$wd/targets.json" || die "targets emission failed"

# --- 3. In-sandbox probe run --------------------------------------------------
echo "run.sh: [3/6] in-sandbox probe run"
cp "$SANDBOX_PROBE" "$wd/sandbox-probe" # the working tree is bound rw in-sandbox
cat > "$wd/probe.sh" <<'PROBE'
#!/usr/bin/env bash
# Runs IN-SANDBOX. Emits partial observations (network/files/env + attempted
# writes) as JSON into the working tree; write PERSISTENCE is judged host-side.
cd "$(dirname "$0")"
./sandbox-probe scan --fast --output_path sandbox-report.json >/dev/null 2>&1 || echo "probe-scan-failed" > sandbox-report.failed
python3 - <<'PY'
import json, os, subprocess

t = json.load(open("targets.json"))
obs = {"network": [], "files": [], "env": [], "writes_attempted": []}

for host in t["network"]:
    code = subprocess.run(
        ["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "10", f"https://{host}/"],
        capture_output=True, text=True).stdout.strip()
    entry = {"target": host, "reachable": code not in ("", "000")}
    if host == "api.github.com" and entry["reachable"]:
        # Token heuristic: the injected read token raises the rate limit above
        # the anonymous 60. Observable evidence, not proxy introspection.
        out = subprocess.run(["curl", "-sS", "--max-time", "10", "https://api.github.com/rate_limit"],
                             capture_output=True, text=True).stdout
        try:
            entry["tokenInjected"] = json.loads(out)["rate"]["limit"] > 60
        except Exception:
            entry["tokenInjected"] = False
    obs["network"].append(entry)

def file_readable(path):
    try:
        with open(path, "rb") as fh:
            return bool(fh.read(1))
    except OSError:
        return False

def dir_readable(path, depth=3):
    # CONTENT reachability, not names: a deny tmpfs legitimately lists
    # allow-back scaffolding (#150), so "non-empty listing" is not a leak.
    if depth == 0:
        return False
    try:
        entries = os.listdir(path)
    except OSError:
        return False
    for e in entries:
        full = os.path.join(path, e)
        if os.path.isfile(full) and file_readable(full):
            return True
        if os.path.isdir(full) and dir_readable(full, depth - 1):
            return True
    return False

for p in t["files"]:
    readable = dir_readable(p) if os.path.isdir(p) else file_readable(p)
    obs["files"].append({"target": p, "reachable": readable})

for name in t["env"]:
    val = os.environ.get(name)
    present = val is not None
    # GH_TOKEN is deliberately a STUB in-sandbox (the real token is injected at
    # the proxy); only a non-stub value is a finding.
    if name == "GH_TOKEN" and present and "rein-sandbox-stub" in val:
        present = False
    obs["env"].append({"target": name, "reachable": present})

for p in t["writes"]:
    try:
        with open(p, "w") as fh:
            fh.write("rein-containment-probe")
    except OSError:
        pass
    obs["writes_attempted"].append(p)

json.dump(obs, open("sbx-obs.json", "w"), indent=1)
PY
PROBE
chmod +x "$wd/probe.sh"
REIN_SRT_SETTINGS_COPY="$settings" REIN_SANDBOX_WORKDIR="$wd" \
  timeout 420 "$REIN_BIN" run --sandbox -- bash "$wd/probe.sh" > "$out_dir/run.log" 2>&1 \
  || die "in-sandbox probe run failed (see $out_dir/run.log)"
[ -s "$wd/sbx-obs.json" ] || die "probe produced no sbx-obs.json (see $out_dir/run.log)"
[ -s "$wd/sandbox-report.json" ] || echo "run.sh: note: sandbox-probe scan produced no report in-sandbox (continuing with supplement only)"
cp "$wd/sandbox-report.json" "$out_dir/sandbox-report.json" 2>/dev/null || true

# --- 4. Write persistence (host-side, #153) -----------------------------------
echo "run.sh: [4/6] host-side write-persistence check"
python3 - "$wd" "$out_dir" <<'PY'
import json, os, sys
wd, out_dir = sys.argv[1], sys.argv[2]
obs = json.load(open(os.path.join(wd, "sbx-obs.json")))
writes = []
for p in obs.pop("writes_attempted", []):
    persisted = os.path.exists(p)
    writes.append({"target": p, "reachable": persisted})
    if persisted and not p.startswith(wd):
        pass  # left for classify to flag; cleaned up below either way
    if persisted:
        try:
            os.remove(p)
        except OSError:
            pass
obs["writes"] = writes
json.dump(obs, open(os.path.join(out_dir, "observations.json"), "w"), indent=1)
PY

# --- 5. Classification --------------------------------------------------------
echo "run.sh: [5/6] oracle classification"
report="$out_dir/report.txt"
probe_report_flag=()
[ -s "$out_dir/sandbox-report.json" ] && probe_report_flag=(-probe-report "$out_dir/sandbox-report.json")
classify_rc=0
"$classify_bin" -settings "$settings" -observations "$out_dir/observations.json" \
  "${probe_report_flag[@]}" > "$report" || classify_rc=$?
cat "$report"

# --- 6. Golden ----------------------------------------------------------------
# Normalize per-run/per-box volatiles (the temp working tree, $HOME) the same
# way the journey goldens do: both sides through the same lens.
# Collapse per-run and per-boot volatiles so the golden is stable: the temp
# working tree, $HOME, pids in /proc paths, and the stale srt-mux socket herd
# (each class keeps ONE representative row via the awk dedupe).
normalize() {
  sed -e "s|$wd|<WORKTREE>|g" -e "s|$HOME|<HOME>|g" \
      -e "s|/proc/[0-9]\{1,\}/|/proc/<PID>/|g" \
      -e "s|/tmp/srt-mux-[0-9-]*\.sock|/tmp/srt-mux-<N>.sock|g" \
      -e "s|/run/user/1000/rein/run-[A-Za-z0-9_-]*|/run/user/1000/rein/run-<RUNID>|g" \
      -e "s|^summary: .*|summary: <normalized — counts vary with the host socket herd>|" \
      -e "s|[0-9a-f]\{16\}|<HEX>|g" -e "s|[0-9a-f]\{8\}\.|<HEX>.|g" \
      -e "s|cc-daemon-1000/[0-9a-f]*|cc-daemon-1000/<HEX>|g" \
      "$1" | sort -u
}
echo "run.sh: [6/6] golden compare"
if [ "${REIN_UPDATE_GOLDEN:-}" = "1" ]; then
  normalize "$report" > "$golden"
  echo "run.sh: [golden UPDATED] $golden"
else
  if [ ! -f "$golden" ]; then
    die "no golden at $golden — run with REIN_UPDATE_GOLDEN=1 to adopt one"
  fi
  if ! diff -u "$golden" <(normalize "$report") > "$out_dir/golden.diff"; then
    echo "run.sh: GOLDEN DRIFT (see $out_dir/golden.diff):" >&2
    cat "$out_dir/golden.diff" >&2
    exit 4
  fi
  echo "run.sh: [golden OK]"
fi

if [ "$classify_rc" -ne 0 ]; then
  die "CONTAINMENT LEAK(S) — classify exit $classify_rc (see $report)"
fi
echo "run.sh: PASS"
