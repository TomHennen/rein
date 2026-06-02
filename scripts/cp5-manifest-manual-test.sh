#!/usr/bin/env bash
# Phase 0.5 CP5 manual smoke test — GitHub App manifest flow end-to-end.
# Run from a REAL terminal on the dev VM. CI cannot do this; it requires
# a browser, a GitHub session, and human button-clicks at github.com.
#
# Pre-reqs:
#   - `go build -o bin/ ./...` in the rein repo so bin/rein and bin/rein-*
#     exist (the script will run the build for you).
#   - You have a GitHub account you can create App registrations under.
#   - Be prepared to delete the test Apps at GitHub UI afterwards — there
#     is no API to delete an App programmatically.
#
# What this verifies:
#   1. `rein init` (no env vars) takes the manifest flow path.
#   2. The browser opens twice in sequence (primary then audit).
#   3. PEMs land in ~/.config/rein/{primary,audit}.pem mode 0600.
#   4. state.json reaches phase=audit_done.
#   5. The key_fingerprint in state.json matches GitHub's settings-page
#      display (manual visual compare).
#   6. After installing an App on a repo, `rein doctor` reports green and
#      `rein run` mints successfully against the new App.
#
# CP5 watch-item: while doing step 6, observe the minted token in the
# helper log. If it's the new ghs_APPID_JWT format and ops succeed, the
# jferrl/go-githubauth opaque handling is good. If it fails, escalate
# upstream and pin a working go-githubauth version.

set -euo pipefail

REIN_REPO_ROOT="${REIN_REPO_ROOT:-$HOME/dev/rein}"
cd "$REIN_REPO_ROOT"

CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/rein"
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/rein"

echo "== Phase 0.5 CP5 manual smoke test =="
echo
echo "About to delete (so we can prove the fresh-install path):"
echo "  $CONFIG_DIR/state.json"
echo "  $CONFIG_DIR/primary.pem"
echo "  $CONFIG_DIR/audit.pem"
echo
read -r -p "Proceed? [y/N] " yn
[[ "$yn" == "y" || "$yn" == "Y" ]] || { echo "Aborted."; exit 0; }

rm -f "$CONFIG_DIR/state.json" "$CONFIG_DIR/primary.pem" "$CONFIG_DIR/audit.pem"

echo
echo "== Step 1: build =="
go build -o bin/ ./...

echo
echo "== Step 2: run \`rein init\` with REIN_APP_* UNSET =="
echo
echo "    The browser will open twice. Click 'Create GitHub App' on both."
echo "    Choose your personal account (not an org) for this smoke test."
echo
unset REIN_APP_CLIENT_ID REIN_APP_INSTALLATION_ID REIN_APP_PRIVATE_KEY_PATH
./bin/rein init --skip-mint-check

echo
echo "== Step 3: verify on-disk artifacts =="
ls -l "$CONFIG_DIR/state.json" "$CONFIG_DIR/primary.pem" "$CONFIG_DIR/audit.pem"
echo
echo "state.json:"
if command -v jq >/dev/null; then
    jq . "$CONFIG_DIR/state.json"
else
    cat "$CONFIG_DIR/state.json"
fi
echo
echo "Expect: phase=audit_done, source=manifest, primary.slug present,"
echo "        audit.slug present, both fingerprints present, installation_id"
echo "        absent (CP5 does not poll for install completion)."

echo
echo "== Step 4: cross-check fingerprint against GitHub UI =="
echo
echo "    Visit https://github.com/apps/<primary-slug>/edit (find the slug"
echo "    in state.json). Scroll to 'Private keys'. The displayed"
echo "    fingerprint should match state.json's primary.key_fingerprint."
echo
echo "    NOTE: if the GitHub UI shows the fingerprint as colon-separated"
echo "    hex bytes (e.g. 'SHA256:ab:cd:...') while state.json shows base64,"
echo "    the encoding is differently-formatted-same-bytes — decode either"
echo "    to raw bytes to compare. If they differ at the byte level, file"
echo "    an issue: keystore.Fingerprint format does not match GitHub's."
echo
read -r -p "Press enter once you have verified (or noted a mismatch)..."

echo
echo "== Step 5: install Primary App on a test repo =="
echo
echo "    Visit https://github.com/apps/<primary-slug>/installations/new"
echo "    and install it on your throwaway test repo (NOT a real repo)."
echo "    After install, copy the installation ID from the install URL"
echo "    (it's the number in /installations/<n>/)."
echo
read -r -p "Installation ID: " IID

echo
echo "== Step 6: re-run \`rein doctor\` with REIN_APP_INSTALLATION_ID set =="
echo
export REIN_APP_CLIENT_ID="$(jq -r .primary.client_id < "$CONFIG_DIR/state.json")"
export REIN_APP_INSTALLATION_ID="$IID"
export REIN_APP_PRIVATE_KEY_PATH="$CONFIG_DIR/primary.pem"
export REIN_TEST_REPO_A="${REIN_TEST_REPO_A:-octocat/Hello-World}"

./bin/rein doctor || true

echo
echo "    Expect: most checks green or warn-with-good-reason; app credentials"
echo "    should be green now (mint succeeded with the just-installed App)."
echo
echo "    If app credentials is RED with 'Bad credentials', double-check the"
echo "    REIN_APP_CLIENT_ID and REIN_APP_INSTALLATION_ID against the App"
echo "    settings page."

echo
echo "== Step 7: stateless installation-token format observation =="
echo
echo "    Tail the helper log while running a mint:"
echo "      tail -f $STATE_DIR/helper.log &"
echo "      ./bin/rein run -- git ls-remote https://github.com/\$REIN_TEST_REPO_A"
echo
echo "    Confirm the operation succeeds. The token format in the log should"
echo "    be opaque (no rein code asserts a prefix). If you see the new"
echo "    ghs_APPID_JWT format (long, 2 dots) and ops still work, the"
echo "    jferrl/go-githubauth opaque handling is fine. If they fail with"
echo "    a token-parse error, that's an upstream issue."

echo
echo "== Step 8: bridge state-machine spot checks =="
echo
echo "    a) row 5 (managed_externally + env absent): with state.json"
echo "       written from this run, set its phase to 'managed_externally'"
echo "       and re-run \`rein init\`. Expect non-zero exit with a"
echo "       'state.json says env-managed but REIN_APP_* are not set' error."
echo
echo "    b) row 1 (no state.json + env set): wipe state.json again"
echo "       (\`rm $CONFIG_DIR/state.json\`), keep the REIN_APP_* env vars"
echo "       set, and re-run \`rein init\`. Expect 'wrote managed_externally"
echo "       marker' in the progress log. cat state.json — should show"
echo "       phase=managed_externally, source=env."
echo
echo "    c) row 6 (env match): re-run \`rein init\` once more with the"
echo "       same env. Expect 'REIN_APP_* matches state.json' and no WARN."
echo
echo "== Done. =="
echo
echo "Cleanup: visit github.com/settings/apps and delete the two rein-* Apps"
echo "you created. There is no API for App deletion."
