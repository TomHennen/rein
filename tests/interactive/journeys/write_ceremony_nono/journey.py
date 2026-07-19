"""write_ceremony_nono — the #35 write ceremony on the NONO backend.

Same declare -> confirm -> verified-push ceremony as journeys/write_ceremony,
but run under `REIN_SANDBOX=nono` (the nono pivot) instead of srt. The in-sandbox
steps and their `SBX|`-tagged output are identical across backends; only the
host-side launch banner + the nono containment gate differ, so this backend keeps
its OWN golden here. The journey body is shared — this module just re-invokes
write_ceremony.journey.main with sandbox="nono" and this dir's golden.

Authoring rules: tests/interactive/CLAUDE.md. Catalogue: tests/interactive/README.md.
"""

from __future__ import annotations

import sys

from pathlib import Path

from tests.interactive.journeys.write_ceremony import journey as wc

GOLDEN = Path(__file__).parent / "golden.txt"


def main() -> int:
    return wc.main(sandbox="nono", golden=GOLDEN)


if __name__ == "__main__":
    sys.exit(main())
