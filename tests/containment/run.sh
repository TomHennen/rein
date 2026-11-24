#!/usr/bin/env bash
# Containment probe harness driver (issue #136B/#141; tests/containment/README.md).
#
#   0. build a throwaway fixture $HOME (fake credentials, toolchain dirs) so
#      the report is the same on every machine; the real home is not probed
#   1. classify -targets from a captured settings.json          -> probe targets
#   2. `rein run --sandbox` runs sandbox-probe + the target probes IN-SANDBOX
#   3. classify judges every observation (and host-side write persistence)
#      against the probe run's own settings.json
#   4. compare the normalized rows to the committed golden
#      (REIN_UPDATE_GOLDEN=1 adopts; README "Golden and tolerated rows")
#
# Exit 1: a leak, or the harness itself failed. 4: golden drift or an "unknown"
# row tolerated.txt does not list. 5: a CONTAINMENT_WEAKEN run (never a pass).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
out_dir="${OUT_DIR:-$here/_out}"
golden="$here/golden-report.txt"
mkdir -p "$out_dir"
# A stale file from an earlier run must never stand in for this one's.
rm -f "${out_dir:?}"/{settings.json,sandbox-report.json,observations.json,report.txt,report.normalized.txt,golden.diff,run.log,capture.log}

die() { echo "run.sh: $*" >&2; exit 1; }

# Pinned: the real home is readable to this binary while the suite runs.
probe_version=v1.1.0
SANDBOX_PROBE="${SANDBOX_PROBE:-$(command -v sandbox-probe || echo "$HOME/go/bin/sandbox-probe")}"
[ -x "$SANDBOX_PROBE" ] || die "sandbox-probe not found/executable ($SANDBOX_PROBE). Install: go install github.com/controlplaneio/sandbox-probe@$probe_version (Apache-2.0; external process only, never in go.mod — hard-constraint #4)."
go version -m "$SANDBOX_PROBE" | grep -q "mod[[:space:]]github.com/controlplaneio/sandbox-probe[[:space:]]$probe_version[[:space:]]" \
  || die "$SANDBOX_PROBE is not sandbox-probe $probe_version (go install github.com/controlplaneio/sandbox-probe@$probe_version)"
rein_bin="${REIN_BIN:-$repo_root/bin/rein}"
[ -x "$rein_bin" ] || die "rein binary not found at $rein_bin (go build -o bin/ ./cmd/... , or set REIN_BIN)"
for tool in python3 curl timeout; do
  command -v "$tool" >/dev/null || die "$tool not found"
done
update_golden="${REIN_UPDATE_GOLDEN:-}"
weaken="${CONTAINMENT_WEAKEN:-}"
[ -z "$weaken" ] || [ "$update_golden" != "1" ] || die "refusing REIN_UPDATE_GOLDEN under CONTAINMENT_WEAKEN"

classify_bin="$out_dir/classify"
( cd "$repo_root" && go build -o "$classify_bin" ./tests/containment/cmd/classify ) || die "failed to build classify"

echo "run.sh: sandbox-probe=$SANDBOX_PROBE rein=$rein_bin out=$out_dir"

# --- 0. Fixture home ----------------------------------------------------------
# rein keeps finding its real App config and state through XDG_*, and still
# denies those dirs in-sandbox; everything else that is home-relative moves to
# the fixture. The developer's own relocations and every REIN_* knob (they
# change the emitted policy) are dropped so each box resolves the same policy.
root="$(cd "$(mktemp -d /tmp/rein-containment-XXXXXX)" && pwd -P)"
trap 'rm -rf "${root:?}"' EXIT
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
if [ -n "$weaken" ]; then
  echo "run.sh: *** CONTAINMENT_WEAKEN=$weaken: sandbox deliberately weakened; this run cannot pass ***" >&2
  eval "export $weaken"
fi

# --- 1. Config capture + targets ---------------------------------------------
# Targets must be in the working tree BEFORE launch, so settings.json is first
# captured from a minimal run; the probe run captures its own again.
settings="$out_dir/settings.json"
echo "run.sh: [1/4] capture settings + derive targets"
REIN_SRT_SETTINGS_COPY="$settings" REIN_SANDBOX_WORKDIR="$wd" \
  "$rein_bin" run --sandbox -- true > "$out_dir/capture.log" 2>&1 || die "settings-capture run failed (see $out_dir/capture.log)"
[ -s "$settings" ] || die "REIN_SRT_SETTINGS_COPY produced no settings.json (rein too old?)"
"$classify_bin" -settings "$settings" -targets > "$wd/targets.json" || die "targets emission failed"
rm -f "$settings"

# --- 2. In-sandbox probe run --------------------------------------------------
echo "run.sh: [2/4] in-sandbox probe run"
canary="rein-containment-canary-$RANDOM$RANDOM"
# Every canary name is set on the host, so the scrub is proven on machines
# that do not have the real variable.
canaries=()
while IFS= read -r name; do canaries+=("$name=$canary"); done \
  < <(python3 -c 'import json,sys; print("\n".join(json.load(open(sys.argv[1]))["env"]))' "$wd/targets.json")
[ "${#canaries[@]}" -gt 0 ] || die "no env canary names derived"
cp "$SANDBOX_PROBE" "$wd/sandbox-probe" # the working tree is bound rw in-sandbox
cat > "$wd/probe.sh" <<'PROBE'
#!/usr/bin/env bash
# Runs IN-SANDBOX: sandbox-probe, then the config-derived probes.
set -eu
cd "$(dirname "$0")"
./sandbox-probe scan --fast --output_path sandbox-report.json >/dev/null 2>&1
python3 - "$1" <<'PY'
import json, os, socket, subprocess, sys

canary = sys.argv[1]
t = json.load(open("targets.json"))
obs = {"network": [], "files": [], "env": [], "sockets": []}

for host in t["network"]:
    code = subprocess.run(
        ["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "10", f"https://{host}/"],
        capture_output=True, text=True).stdout.strip()
    obs["network"].append({"target": host, "reachable": code not in ("", "000")})
# No proxy, no DNS: the network namespace alone must stop this.
try:
    socket.create_connection(("1.1.1.1", 443), timeout=5).close()
    direct = True
except OSError:
    direct = False
obs["network"].append({"target": "1.1.1.1:443 direct", "reachable": direct})

def readable(path, skip):
    # CONTENT, not names: a deny tmpfs legitimately lists allow-back
    # scaffolding (#150), so a non-empty listing is not a leak.
    files = [path]
    if os.path.isdir(path):
        files = []
        for d, subdirs, names in os.walk(path):
            subdirs[:] = [s for s in subdirs if os.path.join(d, s) not in skip]
            files += [os.path.join(d, n) for n in names]
    for f in files:
        try:
            if f not in skip and open(f, "rb").read(1):
                print("readable:", f)
                return True
        except OSError:
            pass
    return False

for f in t["files"]:
    obs["files"].append({"target": f["target"], "reachable": readable(f["target"], set(f.get("except", [])))})

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

leaked = sorted(k for k, v in os.environ.items() if canary in v)
obs["env"] = [{"target": k, "reachable": True} for k in leaked] or \
    [{"target": "host canary", "reachable": False}]

for p in t["writes"]:
    try:
        open(p, "w").write("rein-containment-probe")
    except OSError:
        pass

json.dump(obs, open("sbx-obs.json", "w"), indent=1)
PY
PROBE
env "${canaries[@]}" REIN_SRT_SETTINGS_COPY="$settings" REIN_SANDBOX_WORKDIR="$wd" \
  timeout 420 "$rein_bin" run --sandbox -- bash "$wd/probe.sh" "$canary" > "$out_dir/run.log" 2>&1 \
  || die "in-sandbox probe run failed (see $out_dir/run.log)"
[ -s "$settings" ] || die "the probe run emitted no settings.json"
cp "$wd/sbx-obs.json" "$out_dir/observations.json" || die "probe produced no observations (see $out_dir/run.log)"
cp "$wd/sandbox-report.json" "$out_dir/sandbox-report.json" || die "sandbox-probe produced no report in-sandbox (see $out_dir/run.log)"

# --- 3. Classification --------------------------------------------------------
echo "run.sh: [3/4] oracle classification"
report="$out_dir/report.txt"
classify_rc=0
"$classify_bin" -settings "$settings" -observations "$out_dir/observations.json" \
  -probe-report "$out_dir/sandbox-report.json" \
  -tolerated "$here/tolerated.txt" -normalized-out "$out_dir/report.normalized.txt" \
  -norm "WORKTREE=$wd" -norm "HOME=$HOME" \
  -norm "XDG_CONFIG=$real_config" -norm "XDG_STATE=$real_state" \
  > "$report" || classify_rc=$?
cat "$report"

# --- 4. Golden ----------------------------------------------------------------
# classify printed any leak and any untriaged row to stderr above.
case "$classify_rc" in
  0) ;;
  3) die "CONTAINMENT LEAK(S): classify exit 3 (see $report)" ;;
  4) echo "run.sh: UNTRIAGED ROW(S): review and add to $here/tolerated.txt" >&2; exit 4 ;;
  *) die "classify failed (exit $classify_rc)" ;;
esac
echo "run.sh: [4/4] golden compare"
if [ "$update_golden" = "1" ]; then
  ! grep -qv '^\[ok ' "$out_dir/report.normalized.txt" || die "refusing to adopt a golden with a non-ok row (see $report)"
  cp "$out_dir/report.normalized.txt" "$golden"
  echo "run.sh: [golden UPDATED] $golden"
elif ! diff -u "$golden" "$out_dir/report.normalized.txt" > "$out_dir/golden.diff"; then
  echo "run.sh: GOLDEN DRIFT (see $out_dir/golden.diff):" >&2
  cat "$out_dir/golden.diff" >&2
  exit 4
else
  echo "run.sh: [golden OK]"
fi
if [ -n "$weaken" ]; then
  echo "run.sh: WEAKENED RUN NOT DETECTED ($weaken): not a pass" >&2
  exit 5
fi
echo "run.sh: PASS"
