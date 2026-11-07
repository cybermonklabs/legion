#!/usr/bin/env bash
# Every `fireEvent` call under packages/dispatch/web/src carries a reason of its own.
#
# The Dispatch unit suite performs its interactions with `@testing-library/user-event`, and
# `biome.json` refuses the `fireEvent` import under `packages/dispatch/web/src/**`. A file that
# genuinely needs it keeps the import behind one `biome-ignore lint/style/noRestrictedImports`
# comment naming that file's reasons. That line is the file's claim, and Biome's rule reads imports
# rather than call sites, so once a file carries it the rule says nothing about how many
# `fireEvent` calls the file holds or why each is there.
#
# A forward merge of the default branch is where that hole opens. A base-side test added to a file
# whose exemption is already in place arrives with no conflict, no lint error, no type error, and no
# difference in the pull request's own diff fingerprint or file set, and its calls inherit reasons
# written for other sites — measured on cybermonklabs/legion#2884, where four presses of enabled
# buttons and an edit of an enabled field landed under an ignore line naming a paste, a drop of
# files, an IME composition key, an edit into a control a send holds disabled, same-batch pairs
# inside `act`, and a hand-made `beforeinput`. The sibling case, a base-side call in a file whose
# import the branch had removed, is an unbound name the typecheck names; this one nothing names.
# So the check asks each site for its reason instead of asking each file.
#
# What it buys and what it does not. It makes a `fireEvent` call impossible to add silently: the
# only measured way one arrives unexamined is inside a file whose exemption is already in place,
# and this refuses that. It is a presence check and nothing more — any comment satisfies it, so
# whether a reason is a true one, and whether user-event really has no form for that interaction,
# is the reviewer's to judge. A gate checks what a program can check; the judgement stays with a
# reader.
#
# A reason is a comment within the five lines above the call: the site's own one-line reason, or
# the `biome-ignore` comment where the call sits beside the import. The window is five lines rather
# than the line immediately above because a legitimate site is often wrapped — the reason sits above
# an `await act(async () => {` or a `const` the call needs — and a bare immediately-above rule would
# refuse that shape while buying nothing. It still refuses a reason written only at the top of the
# file, which is the case this exists for.
#
# A comment is a line whose trimmed text begins with `//`, `/*`, `*` or JSX's `{/*`, and nothing
# else is. A line holding `//` or `*` anywhere else is code: `const n = 3 * 2;` above a call, and a
# URL in a string on the call's own line, both used to satisfy this check and now do not. A call
# written inside such a comment is prose and is not counted, which is what lets a reason name
# `fireEvent.mouseDown` while explaining why a site keeps it.
#
# Both forms of the API count: `fireEvent.click(node)` and the direct `fireEvent(node, event)`.
#
# Run from anywhere: .github/scripts/check-fireevent-reasons.sh
# CI runs it in ci.yaml's lint job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path

ROOT = Path("packages/dispatch/web/src")
CALL = re.compile(r"\bfireEvent\b\s*[.(]")
WINDOW = 5
# A comment line, and the only thing this reads as one: the trimmed text begins a comment. A `//`
# or a `*` anywhere else on a line is code, which is what the two bypasses this closes relied on.
COMMENT = re.compile(r"^\s*(//|/\*|\*|\{/\*)")

sites = 0
bare: list[tuple[Path, int, str]] = []

for path in sorted(ROOT.rglob("*")):
    if path.suffix not in (".ts", ".tsx") or not path.is_file():
        continue
    lines = path.read_text().splitlines()
    for index, line in enumerate(lines):
        if CALL.search(line) is None or COMMENT.match(line):
            continue
        sites += 1
        above = lines[max(0, index - WINDOW) : index]
        if any(COMMENT.match(previous) for previous in above):
            continue
        bare.append((path, index + 1, line.strip()))

if bare:
    print(
        f"{len(bare)} of {sites} fireEvent calls under {ROOT} carry no reason within the "
        f"{WINDOW} lines above them:",
        file=sys.stderr,
    )
    for path, line, text in bare:
        print(f"::error file={path},line={line}::{path}:{line}: {text}")
        print(f"  {path}:{line}", file=sys.stderr)
        print(f"    {text}", file=sys.stderr)
    print(
        "\nEach one is an interaction `@testing-library/user-event` has no form for, or it is not "
        "one. Where user-event has a form, use it: `await user.click(…)`, `await user.type(…)`, "
        "`await user.selectOptions(…)`, with `userEvent.setup()`, or `userOnFakeClock()` from "
        "web/src/__tests__/user.ts under a held clock. Where it has none, write the reason on the "
        "line above the call. A file's `biome-ignore` line is the file's claim and reasons no "
        "single call.",
        file=sys.stderr,
    )
    sys.exit(1)

print(f"every one of the {sites} fireEvent calls under {ROOT} carries a reason")
PY
