"""test_golden_shape — a CHEAP, STACK-FREE lint over the journey goldens.

Unlike the rest of the suite this needs NO srt, NO GitHub, NO tty, NO network —
it only reads files. So it runs in the normal `run.sh` sweep AND standalone with
zero setup:

    python3 -m unittest test_golden_shape

It guards two invariants that a human forgetting to run `run-journeys.sh` would
otherwise break:

  1. Every `journey_*.py` has a committed golden at the conventional path
     (`journey_<stem>.py` -> `golden/<stem>.txt`), and there are no orphan
     goldens with no journey.
  2. No golden contains an UN-NORMALIZED volatile — a raw scratch path, run id,
     timestamp, or branch nonce that a normalization-rule regression would leak.
     (Issue numbers are deliberately NOT checked here: `agent/73/kx3q` is a
     literal EXAMPLE in rein's own error text, so a digit-based rule would false-
     positive. Un-normalized issue numbers are instead caught by the journey's
     determinism check — two runs with different issues must both match.)
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
GOLDEN_DIR = HERE / "golden"

# journey_<stem>.py  <->  golden/<stem>.txt
JOURNEYS = sorted(HERE.glob("journey_*.py"))


def golden_for(journey: Path) -> Path:
    stem = journey.name[len("journey_"):-len(".py")]
    return GOLDEN_DIR / f"{stem}.txt"


# Patterns that must NEVER survive into a golden: each has a placeholder it
# should have been normalized to. Unambiguous volatiles only.
FORBIDDEN = [
    (re.compile(r"/tmp/rein-[A-Za-z0-9]"), "scratch path (should be <TMP>)"),
    (re.compile(r"/run/user/\d"), "proxy socket path (should be <PROXY_SOCK>)"),
    (re.compile(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z"), "timestamp (should be <TS>)"),
    (re.compile(r"\d{8}-\d{6}-[0-9a-f]{6}"), "branch nonce (should be <NONCE>)"),
    (re.compile(r"\brun-[A-Za-z0-9_-]{16,}"), "run id (should be run-<RUNID>)"),
]


class GoldenShape(unittest.TestCase):
    def test_at_least_one_journey_exists(self):
        self.assertTrue(JOURNEYS, "no journey_*.py found — the catalogue should have at least one")

    def test_every_journey_has_a_committed_golden(self):
        missing = [j.name for j in JOURNEYS if not golden_for(j).exists()]
        self.assertEqual(
            missing, [],
            f"journeys with no committed golden (run tests/interactive/run-journeys.sh): {missing}",
        )

    def test_no_orphan_goldens(self):
        expected = {golden_for(j).name for j in JOURNEYS}
        actual = {p.name for p in GOLDEN_DIR.glob("*.txt")} if GOLDEN_DIR.exists() else set()
        orphans = sorted(actual - expected)
        self.assertEqual(orphans, [], f"golden files with no matching journey_*.py: {orphans}")

    def test_goldens_have_no_unnormalized_volatiles(self):
        problems = []
        for j in JOURNEYS:
            g = golden_for(j)
            if not g.exists():
                continue
            for i, line in enumerate(g.read_text().splitlines(), start=1):
                for pat, what in FORBIDDEN:
                    if pat.search(line):
                        problems.append(f"{g.name}:{i}: un-normalized {what}: {line.strip()!r}")
        self.assertEqual(problems, [], "un-normalized volatiles in goldens:\n" + "\n".join(problems))


if __name__ == "__main__":
    unittest.main()
