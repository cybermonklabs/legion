---
title: "Edit the pull request body in place from the live copy, never from a saved file"
category: legion
tags:
  - pr-body
  - handoff
  - implementer
  - tester
  - reviewer
date: 2026-10-13
status: active
module: pull-request body
related_issues:
  - "LEGSMOKE-760"
  - "sjawhar/legion-smoke#571"
---

# Edit the pull request body in place from the live copy, never from a saved file

Fetch the live body immediately before every edit: `gh api repos/<owner>/<repo>/pulls/<n> --jq .body > <dir>/before.md`, with `<dir>` a fresh `mktemp -d` outside the workspace. Copy it, change only the lines your role owns in the copy, and PATCH the copy.

Diff before you PATCH: `diff <dir>/before.md <dir>/body.md` must show only your own lines. Diff after you PATCH: fetch again and compare with what you sent; the only acceptable difference is GitHub's trailing newline.

Verify what you did not destroy, not what you wrote. After every PATCH grep the live body for the other roles' markers (`E2E (implementer)`, `E2E (tester)`, `Look at first`, `Not proven / risk`, `Production`) and compare the byte count with the pre-PATCH fetch. A check that greps only for your own lines cannot fail when you have erased someone else's.

Never keep a body file across rounds and PATCH from it. Another role has edited the body since you last read it; your saved copy is the body as it was, and sending it back deletes everything written in between.

When you damage another role's artifact, record it in your own handoff (`deviations`) in the same push, not only in an Envoy message: the next role reads your handoff and may never see the message.

## Evidence

Pull request `sjawhar/legion-smoke#571`. At 2026-10-13T22:57:24Z the implementer PATCHed the body from `/tmp/l760.vG054a/body.md`, a copy it had last written at 22:37Z and extended for review round 1. The live body at that moment (the reviewer's 22:54:49Z version, 10,887 characters) carried the tester's `E2E (tester)` block and the reviewer's filled `Look at first` and `Not proven / risk` lines; the PATCH sent 7,551 characters and dropped all three. The implementer's self-check, `grep -c` over six patterns it had itself written, printed `6` and it reported "Body patched (6/6 lines)". The tester noticed through the PR's `userContentEdits` (10,887 → 7,551), restored its block, and reported it over Envoy (message `97baea52cd382d79961cb9e892986f21`); the reviewer rewrote its two lines in round 2. The implementer's `implement.json` of that round (`completed` 22:58:00Z) has `deviations: []`; the incident is recorded only in the tester's `test.json`.

Nine minutes later the same implementer edited the body correctly for a thread resolution: fetched the live body, edited two `Threads` lines in place, `diff` showed `24c24` and `25a26` only, PATCHed, and the post-PATCH diff was `47a48 >` (GitHub's trailing newline).
