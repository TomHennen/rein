#!/usr/bin/env bash
# Manual run recipe for tests/interactive/test_confirm_shows_title.py (GATED).
#
# This test is TDD-red and targets DEFERRED work (issue #35, the agent-declared
# + human-confirmed issue-scoping model). It MUST fail today because the /dev/tty
# approval prompt shows no issue title BY DESIGN for CP1-CP4.5 (static sess.Issue,
# PLAN-1.md:363-379). It flips to an "unexpected success" once #35's fetch +
# display issue title/home-repo at confirm time lands.
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
# EXPECTED TODAY: "expected failure" (green suite) — the prompt lacks the title.
# AFTER THE FIX: "unexpected success" (red suite) — drop @unittest.expectedFailure
# and promote it to a real regression test.
set -euo pipefail
echo "This is a recipe, not an automated runner. Read the header and run the steps"
echo "in your own terminal. See tests/interactive/test_confirm_shows_title.py."
