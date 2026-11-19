"""The character-count check (.github/scripts/check-character-counts.sh runs it): its controls
first, then every file git lists. --list prints every hit with its verdict."""
from __future__ import annotations

import os
import subprocess
import sys

from character_counts_controls import CONTROLS, control, counter_controls, tree_controls
from character_counts_tree import judge_tree


def main() -> int:
    counters = counter_controls()
    failed = [problem for problem in (control(*c) for c in CONTROLS) if problem] + \
        [problem for _, problem in counters if problem] + tree_controls()
    if failed:
        print("CONTROLS FAILED: " + "; ".join(failed))
        return 1
    total = len(CONTROLS) + len(counters) + 1
    print(f"controls: {total} of {total} answered as they must")
    for name, *_ in CONTROLS:
        print(f"  ok  {name}")
    for name, _ in counters:
        print(f"  ok  {name}")
    print("  ok  a count in a workflow, a test and a generated file fires; each class left out is counted; each rule that leaves"
          " files out has a case, and each path the check must read is read; an empty listing is UNKNOWN")
    listed = subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        capture_output=True, check=True,
    ).stdout.decode("utf-8", errors="replace").split("\0")
    listing = sorted({path for path in listed if path})

    def read_file(path: str) -> bytes | str | None:
        if os.path.islink(path):
            return "link"
        try:
            with open(path, "rb") as handle:
                return handle.read()
        except (FileNotFoundError, IsADirectoryError, NotADirectoryError):
            return None

    code, report, hits = judge_tree(listing, read_file)
    if "--list" in sys.argv[1:]:
        for hit in hits:
            why = f" {hit.why} ::" if hit.why else ""
            print(f"{hit.path}:{hit.line}: [{hit.verdict}]{why} {hit.text}")
    for line in report:
        print(line)
    if code and not report[-1].startswith("UNKNOWN"):
        print(
            "Give the numbers with no unit (\"title: 1002 of at most 1000\"), or name the unit the "
            "counter counts (\"UTF-16 code units\", \"bytes\"). Where the counter is where the check "
            "cannot follow it, write `characters: code points, <where>` or `characters: ASCII alone, "
            "<why>` in a comment on the line or the two lines above.",
            file=sys.stderr,
        )
    return code


sys.exit(main())
