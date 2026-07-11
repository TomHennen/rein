#!/usr/bin/env bash
# Manual run recipe for tests/interactive/test_confirm_shows_title.py (GATED).
#
# Since issue #35 landed this is a REAL regression test (the old
# @unittest.expectedFailure is gone): the agent declares its issue
# (`rein declare <n>`), rein FETCHES it, and the Form A confirmation prompt
# must DISPLAY the fetched title + home repo (decision E — the load-bearing
# misattribution control, probe S1/S4/S5).
#
# It needs a REAL environment a human must provide:
#   - a working `rein init` App on this machine (see HANDOFF.md)
#   - the srt sandbox available (Linux userns) and a real controlling /dev/tty
#   - a THROWAWAY repo (the harness picks it from REIN_* env; see reinharness.py)
#   - a real issue on that throwaway with a distinctive word in its title
#
# Run it FROM YOUR REAL TERMINAL (not from inside an agent — the prompt reads
# /dev/tty, which an agent's tool subprocess cannot reach):
#
#   source ./dev-env                     # load REIN_* env
#   # create a throwaway issue with a memorable title, e.g.:
#   #   gh issue create -R <throwaway> -t "Wire up the widget carousel" -b "test"
#   export REIN_ITEST_TITLE_ISSUE=<the issue number>
#   export REIN_ITEST_TITLE_WORD=carousel   # a distinctive word from that title
#   cd tests/interactive
#   python3 -m unittest test_confirm_shows_title -v
#
# EXPECTED: PASS — the declare prompt shows the fetched title + home repo, you
# type the displayed number, and the verified push to agent/<n>/<nonce> lands.
# A FAILURE here is a regression in #35's Form A display.
set -euo pipefail
echo "This is a recipe, not an automated runner. Read the header and run the steps"
echo "in your own terminal. See tests/interactive/test_confirm_shows_title.py."
