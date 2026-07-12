"""test_golden_shape — a CHEAP, STACK-FREE lint over the journey goldens.

Unlike the rest of the suite this needs NO srt, NO GitHub, NO tty, NO network —
it only reads files + runs pure string transforms. So it runs in the normal
`run.sh` sweep AND standalone with zero setup:

    python3 -m unittest test_golden_shape

It guards the invariants of the RAW-golden / normalize-on-compare model (PR #78):

  1. Every `journey_*.py` has a committed golden at the conventional path
     (`journey_<stem>.py` -> `golden/<stem>.txt`), and there are no orphan
     goldens with no journey.
  2. The comparison transform is WELL-FORMED on each golden: normalizing it is
     IDEMPOTENT (`normalize(normalize(g)) == normalize(g)`). If a rule were not a
     fixpoint — e.g. it rewrote its own output — the comparator would be unstable
     and drift detection unreliable. This replaces the old "no un-normalized
     volatiles" check, which INVERTED under PR #78: a raw golden legitimately
     contains real issue numbers, repos, and counts, so those are no longer
     errors — the golden is supposed to show reality.
"""

from __future__ import annotations

import unittest
from pathlib import Path

import reinharness as H

HERE = Path(__file__).resolve().parent
GOLDEN_DIR = HERE / "golden"

# journey_<stem>.py  <->  golden/<stem>.txt
JOURNEYS = sorted(HERE.glob("journey_*.py"))


def golden_for(journey: Path) -> Path:
    stem = journey.name[len("journey_"):-len(".py")]
    return GOLDEN_DIR / f"{stem}.txt"


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

    def test_goldens_are_nonempty(self):
        empty = [golden_for(j).name for j in JOURNEYS
                 if golden_for(j).exists() and not golden_for(j).read_text().strip()]
        self.assertEqual(empty, [], f"empty goldens: {empty}")

    def test_normalization_is_idempotent_on_each_golden(self):
        """The compare transform must reach a fixpoint: a stable comparator is
        what makes drift detection trustworthy."""
        unstable = []
        for j in JOURNEYS:
            g = golden_for(j)
            if not g.exists():
                continue
            raw = g.read_text()
            once = H.normalize_for_compare(raw)
            twice = H.normalize_for_compare(once)
            if once != twice:
                unstable.append(g.name)
        self.assertEqual(
            unstable, [],
            f"normalize_for_compare is NOT idempotent on: {unstable} — a rule rewrites its own output",
        )


if __name__ == "__main__":
    unittest.main()
