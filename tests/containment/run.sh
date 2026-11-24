#!/usr/bin/env bash
# Containment probe harness driver (issue #136B/#141; tests/containment/README.md).
#
#   0. build a throwaway fixture $HOME (fake credentials, toolchain dirs) so
#      the report is the same on every machine; the real home is not probed
#   1. classify -targets from a captured settings.json          -> probe targets
#      (config-derived: deny paths, allow-backs, srt's own default write paths)
#   2. `rein run --sandbox` runs sandbox-probe + the target probes IN-SANDBOX,
#      writing results into the bound working tree
#   3. host-side write-persistence check (#153 channel)
#   4. classify judges every observation against the emitted settings.json
#   5. compare the normalized rows to the committed golden
#      (REIN_UPDATE_GOLDEN=1 adopts; README "Golden and tolerated rows")
#
# Any leak => classify exits 3 => this script fails. Golden drift, or an
# "unknown" row tolerated.txt does not list, also fails (exit 4).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
out_dir="${OUT_DIR:-$here/_out}"
golden="$here/golden-report.txt"
tolerated="$here/tolerated.txt"
mkdir -p "$out_dir"
# A stale file from an earlier run must never stand in for this one's.
rm -f "$out_dir"/{settings.json,sandbox-report.json,observations.json,report.txt,report.normalized.txt,golden.diff,run.log,targets.notes}

die() { echo "run.sh: $*" >&2; exit 1; }

SANDBOX_PROBE="${SANDBOX_PROBE:-$(command -v sandbox-probe || echo "$HOME/go/bin/sandbox-probe")}"
[ -x "$SANDBOX_PROBE" ] || die "sandbox-probe not found/executable ($SANDBOX_PROBE). Install: go install github.com/controlplaneio/sandbox-probe@latest (Apache-2.0; external process only, never in go.mod — hard-constraint #4)."
rein_bin="${REIN_BIN:-$repo_root/bin/rein}"
[ -x "$rein_bin" ] || die "rein binary not found at $rein_bin (go build -o bin/ ./cmd/... , or set REIN_BIN)"
for tool in python3 curl; do
  command -v "$tool" >/dev/null || die "$tool not found (the in-sandbox probe needs it)"
done
update_golden="${REIN_UPDATE_GOLDEN:-}"

classify_bin="$out_dir/classify"
( cd "$repo_root" && go build -o "$classify_bin" ./tests/containment/cmd/classify ) || die "failed to build the classify oracle CLI"

echo "run.sh: sandbox-probe=$SANDBOX_PROBE rein=$rein_bin out=$out_dir"

# --- 0. Fixture home ----------------------------------------------------------
# rein keeps finding its real App config and state through XDG_*, and still
# denies those dirs in-sandbox; everything else that is home-relative moves to
# the fixture. The developer's own relocations and every REIN_* knob (they
# change the emitted policy) are dropped so each box resolves the same policy.
root="$(mktemp -d /tmp/rein-containment-XXXXXX)"
trap 'rm -rf "$root"' EXIT
wd="$root/wd"
mkdir -p -m 700 "$wd" "$root/home/overlay"
real_config="${XDG_CONFIG_HOME:-$HOME/.config}"
real_state="${XDG_STATE_HOME:-$HOME/.local/state}"
unset GNUPGHOME GH_CONFIG_DIR CARGO_HOME XDG_DATA_HOME CLAUDE_CONFIG_DIR
for v in $(compgen -e | grep '^REIN_' || true); do unset "$v"; done
export XDG_CONFIG_HOME="$real_config" XDG_STATE_HOME="$real_state"
export HOME="$root/home"
# A writable bind nested inside the home deny.
export REIN_SANDBOX_CLAUDE_HOME_DIR="$HOME/overlay"
"$classify_bin" -fixture "$HOME" || die "failed to write the fixture home"
# CONTAINMENT_WEAKEN='VAR=VAL ...' re-applies knobs ($HOME is the fixture) to
# prove the suite fails when the sandbox is weakened (README "Checking the suite").
[ -z "${CONTAINMENT_WEAKEN:-}" ] || eval "export $CONTAINMENT_WEAKEN"

# --- 1. Config capture + targets ---------------------------------------------
# The settings.json is captured from a minimal sandboxed run
# (REIN_SRT_SETTINGS_COPY), then targets are derived from it. The main probe run
# below captures its own settings again — same box, same config; the two-step
# exists only because targets must be in the working tree BEFORE launch.
settings="$out_dir/settings.json"
echo "run.sh: [1/5] capture settings + derive targets"
REIN_SRT_SETTINGS_COPY="$settings" REIN_SANDBOX_WORKDIR="$wd" \
  "$rein_bin" run --sandbox -- true >/dev/null 2>&1 || die "settings-capture run failed"
[ -s "$settings" ] || die "REIN_SRT_SETTINGS_COPY produced no settings.json (rein too old?)"
"$classify_bin" -settings "$settings" -targets > "$wd/targets.json" 2> "$out_dir/targets.notes" \
  || die "targets emission failed: $(cat "$out_dir/targets.notes")"
rm -f "$settings"

# --- 2. In-sandbox probe run --------------------------------------------------
echo "run.sh: [2/5] in-sandbox probe run"
cp "$SANDBOX_PROBE" "$wd/sandbox-probe" # the working tree is bound rw in-sandbox
cat > "$wd/probe.sh" <<'PROBE'
#!/usr/bin/env bash
# Runs IN-SANDBOX. Emits partial observations (network/files/env + attempted
# writes) as JSON into the working tree; write PERSISTENCE is judged host-side.
set -eu
cd "$(dirname "$0")"
./sandbox-probe scan --fast --output_path sandbox-report.json >/dev/null 2>&1
python3 - <<'PY'
import json, os, socket, subprocess

t = json.load(open("targets.json"))
obs = {"network": [], "files": [], "env": [], "sockets": [], "writes_attempted": []}

for host in t["network"]:
    code = subprocess.run(
        ["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "10", f"https://{host}/"],
        capture_output=True, text=True).stdout.strip()
    obs["network"].append({"target": host, "reachable": code not in ("", "000")})

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

for f in t["files"]:
    # The row is reported under the policy entry; the probe is what is read.
    p = f["probe"]
    readable = dir_readable(p) if os.path.isdir(p) else file_readable(p)
    obs["files"].append({"target": f["target"], "probe": p, "reachable": readable})

# Without AF_UNIX creation no unix socket can be connected, whatever is visible.
creatable = False
for ty in (socket.SOCK_STREAM, socket.SOCK_DGRAM, socket.SOCK_SEQPACKET):
    try:
        socket.socket(socket.AF_UNIX, ty).close()
        creatable = True
    except OSError:
        pass
obs["sockets"].append({"target": "AF_UNIX", "reachable": creatable})

# sandbox-probe calls a dir "readable" when it can list it. A denied dir is an
# EMPTY tmpfs in-sandbox: listable, nothing in it. Report those so classify
# does not read the mount point itself as a leak.
flagged = set()
for f in json.load(open("sandbox-report.json"))["findings"]:
    if f.get("findingType") == "sensitive_readable_paths":
        flagged |= {v for v in f["value"] if isinstance(v, str)}
obs["empty_dirs"] = sorted(p for p in flagged if os.path.isdir(p) and not os.listdir(p))

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
# Every scrubbed env var is set to a canary on the host, so "correctly
# scrubbed" is proven on machines that do not have the real one.
canaries=()
while IFS= read -r name; do canaries+=("$name=rein-containment-canary"); done \
  < <(python3 -c 'import json,sys; print("\n".join(json.load(open(sys.argv[1]))["env"]))' "$wd/targets.json")
[ "${#canaries[@]}" -gt 0 ] || die "no env targets derived"
env "${canaries[@]}" REIN_SRT_SETTINGS_COPY="$settings" REIN_SANDBOX_WORKDIR="$wd" \
  timeout 420 "$rein_bin" run --sandbox -- bash "$wd/probe.sh" > "$out_dir/run.log" 2>&1 \
  || die "in-sandbox probe run failed (see $out_dir/run.log)"
[ -s "$settings" ] || die "the probe run emitted no settings.json"
[ -s "$wd/sbx-obs.json" ] || die "probe produced no sbx-obs.json (see $out_dir/run.log)"
[ -s "$wd/sandbox-report.json" ] || die "sandbox-probe produced no report in-sandbox (see $out_dir/run.log)"
cp "$wd/sandbox-report.json" "$out_dir/sandbox-report.json"

# --- 3. Write persistence (host-side, #153) -----------------------------------
echo "run.sh: [3/5] host-side write-persistence check"
python3 - "$wd" "$out_dir" <<'PY'
import json, os, sys
wd, out_dir = sys.argv[1], sys.argv[2]
obs = json.load(open(os.path.join(wd, "sbx-obs.json")))
writes = []
for p in obs.pop("writes_attempted", []):
    persisted = os.path.exists(p)
    writes.append({"target": p, "reachable": persisted})
    if persisted:
        try:
            os.remove(p)
        except OSError:
            pass
obs["writes"] = writes
json.dump(obs, open(os.path.join(out_dir, "observations.json"), "w"), indent=1)
PY

# --- 4. Classification --------------------------------------------------------
echo "run.sh: [4/5] oracle classification"
report="$out_dir/report.txt"
classify_rc=0
"$classify_bin" -settings "$settings" -observations "$out_dir/observations.json" \
  -probe-report "$out_dir/sandbox-report.json" \
  -tolerated "$tolerated" -normalized-out "$out_dir/report.normalized.txt" \
  -norm "WORKTREE=$wd" -norm "HOME=$HOME" -norm "FIXTURE=$root" \
  -norm "XDG_CONFIG=$real_config" -norm "XDG_STATE=$real_state" \
  > "$report" || classify_rc=$?
cat "$report"

# --- 5. Golden ----------------------------------------------------------------
# classify printed any leak and any untriaged row to stderr above.
case "$classify_rc" in
  0) ;;
  3) die "CONTAINMENT LEAK(S) — classify exit 3 (see $report)" ;;
  4) echo "run.sh: UNTRIAGED ROW(S) — review and add to $tolerated" >&2; exit 4 ;;
  *) die "classify failed (exit $classify_rc)" ;;
esac
echo "run.sh: [5/5] golden compare"
if [ "$update_golden" = "1" ]; then
  cp "$out_dir/report.normalized.txt" "$golden"
  echo "run.sh: [golden UPDATED] $golden"
elif ! diff -u "$golden" "$out_dir/report.normalized.txt" > "$out_dir/golden.diff"; then
  echo "run.sh: GOLDEN DRIFT (see $out_dir/golden.diff):" >&2
  cat "$out_dir/golden.diff" >&2
  exit 4
else
  echo "run.sh: [golden OK]"
fi
sed 's/^/run.sh: /' "$out_dir/targets.notes"
echo "run.sh: PASS"
