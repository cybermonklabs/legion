"""The character-count check (.github/scripts/check-character-counts.sh runs it): its controls
first, then every file git lists against the ledger. --list prints every site with its decision."""
from __future__ import annotations

import sys

from character_counts_controls import run_controls
from character_counts_tree import disk_reader, git_listing, judge_tree


def main() -> int:
    names, failed = run_controls()
    if failed:
        print("CONTROLS FAILED: " + "; ".join(failed))
        return 1
    print(f"controls: {len(names)} of {len(names)} answered as they must")
    for name in names:
        print(f"  ok  {name}")
    code, report, _ = judge_tree(git_listing("."), disk_reader("."), listed="--list" in sys.argv[1:])
    for line in report:
        print(line)
    if code and not report[-1].startswith("UNKNOWN"):
        print(
            "Each count beside the word \"character\" or \"characters\" (\"char\", \"chars\") on a line needs a "
            "reviewed row in .github/character-counts.ledger: the counter that computes the count and the unit "
            "it counts. A message whose counter counts bytes or UTF-16 code units names that unit instead of "
            "characters. --list prints the start of each row a site needs.",
            file=sys.stderr,
        )
    return code


sys.exit(main())
