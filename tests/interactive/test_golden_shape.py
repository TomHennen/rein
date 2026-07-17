"""test_golden_shape — a CHEAP, STACK-FREE lint over the journey goldens.

Unlike the rest of the suite this needs NO srt, NO GitHub, NO tty, NO network —
it only reads files + runs pure string transforms. So it runs in the normal
`run.sh` sweep AND standalone with zero setup:

    python3 -m unittest test_golden_shape

It guards the invariants of the RAW-golden / normalize-on-compare model (PR #78):

  1. Every journey (`journeys/<name>/journey.py`) has a committed golden co-located
     at `journeys/<name>/golden.txt`, and there are no orphan goldens with no journey.
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

from tests.interactive import reinharness as H

HERE = Path(__file__).resolve().parent
JOURNEYS_DIR = HERE / "journeys"

# journeys/<name>/journey.py  <->  journeys/<name>/golden.txt (co-located)
JOURNEYS = sorted(JOURNEYS_DIR.glob("*/journey.py"))


def golden_for(journey: Path) -> Path:
    return journey.parent / "golden.txt"


def rel(p: Path) -> str:
    return str(p.relative_to(HERE))


class GoldenShape(unittest.TestCase):
    def test_at_least_one_journey_exists(self):
        self.assertTrue(JOURNEYS, "no journeys/*/journey.py found — the catalogue should have at least one")

    def test_every_journey_has_a_committed_golden(self):
        missing = [rel(j) for j in JOURNEYS if not golden_for(j).exists()]
        self.assertEqual(
            missing, [],
            f"journeys with no committed golden (run tests/interactive/run-journeys.sh): {missing}",
        )

    def test_no_orphan_goldens(self):
        goldens = sorted(JOURNEYS_DIR.glob("*/golden.txt")) if JOURNEYS_DIR.exists() else []
        orphans = [rel(g) for g in goldens if not (g.parent / "journey.py").exists()]
        self.assertEqual(orphans, [], f"golden.txt with no sibling journey.py: {orphans}")

    def test_goldens_are_nonempty(self):
        empty = [rel(golden_for(j)) for j in JOURNEYS
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
                unstable.append(rel(g))
        self.assertEqual(
            unstable, [],
            f"normalize_for_compare is NOT idempotent on: {unstable} — a rule rewrites its own output",
        )


if __name__ == "__main__":
    unittest.main()
