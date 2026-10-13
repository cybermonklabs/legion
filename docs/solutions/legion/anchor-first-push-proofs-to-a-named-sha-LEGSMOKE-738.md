---
title: "Anchor a 'first push' criterion to one named commit SHA and prove bytes against the remote blob"
category: legion
tags:
  - acceptance-criteria
  - proof
  - first-push
  - byte-exact
date: 2026-10-13
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-738"
  - "sjawhar/legion-smoke#567"
---

# Anchor a 'first push' criterion to one named commit SHA and prove bytes against the remote blob

- When a criterion speaks of "the branch's first push", decide in the plan which commit that is (the first commit whose diff touches the file) and record its full SHA in the implement handoff as a field no later round rewrites (`firstPushSha`). Keep a separate `codeSha` that moves on every code push.
- Prove every later "unchanged" claim against the blob read back from the remote at `firstPushSha`, never against a remembered string: decode both heads from the contents API into `mktemp -d` files and compare with `cmp <(head -1 old) <(head -1 new)`.
- Check "ends in a newline" by bytes: `wc -l` counts line feeds and cannot tell `a\nb` from `a\nb\n`. Use `tail -c1 f | od -An -tx1` (expect ` 0a`) and `od -An -c f` for the whole file.
- Give every content proof a control that would actually trip the check: the same decode on a known two-line file, a copy with the final line feed stripped (`head -c -1`), a `cmp` against a deliberately altered literal, and (when a line is appended) a copy with the two lines swapped.

## Evidence

- LEGSMOKE-738, pull request sjawhar/legion-smoke#567. The branch `legion/LEGSMOKE-738` already existed at origin equal to `main`, and the planner's handoff-only push preceded any code, so the literal first push held no file. The plan fixed `<first-sha>` = the first commit touching `smoke/LEGSMOKE-738.md`; the implementer recorded `a277f6e7bda0ac9b1f6c171825d1062e3584a572` as `firstPushSha` and the tester and reviewer read criterion 2 there and nowhere else.
- Round 2 appended a second line in `f3d4aa4d77bd1dc504a98558547691086e2bf865`; the criterion-3 proof decoded both heads from GitHub, `cmp` of line 1 was silent, and the controls reported `differ: char 56, line 1` (altered literal), `wc -l 1` / last byte ` 29` (stripped line feed) and `differ: char 2, line 1` (swapped lines, the tester's addition).
- The same anchor pattern was recorded by the previous proof run (LEGSMOKE-728, #565); that note was removed from `main` by #566, which is why it is recorded again here.
