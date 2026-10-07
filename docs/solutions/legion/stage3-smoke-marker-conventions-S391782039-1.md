---
title: "Legion smoke marker file: byte-exact conventions and review-round appends"
category: legion
tags:
  - legion
  - smoke-proof
  - review-rounds
  - marker-file
date: 2026-10-07
status: active
module: smoke
related_issues:
  - "S391782039-1"
  - "sjawhar/legion-smoke#512"
---

# Legion smoke marker file: byte-exact conventions and review-round appends

A Legion Stage-3 smoke proof adds one marker file, `smoke/<KEY>.md`, and nothing else.
Treat its contents as a byte-exact contract and its scope as a single repository path.

## Rules

- Line 1 is the bare issue key (`S391782039-1`), no title or em-dash suffix. The file ends
  with a single trailing newline and no blank or extra line.
- Each review round N appends exactly one line, `Legion proof review round N (S391782039)`
  (the round lines drop the `-1` issue suffix, carrying the project key only). Append-only:
  never rewrite earlier lines.
- Follow the reviewer's round instruction byte-for-byte, even when it diverges from the
  format the plan predicted. Here `plan.json` predicted `Stage 3 review round <N> (…)`, but
  the reviewer issued `Legion proof review round N (…)`; the reviewer's literal string wins.
- Enforce single-file scope with
  `jj diff --stat -r 'main@origin..<head>' | grep -v '\.legion/'`: the only non-`.legion`
  path must be `smoke/<KEY>.md`. The `.legion/*.json` handoff artifacts are expected and
  excluded from the scope check.
- Prove each line with `sed -n Np`, `cat -A`, and `wc -l`; a `sed -n <last+1>p` that returns
  empty is the negative control against a stray extra line.

## Bot review threads

Dispose of each bot review thread individually. The implementer replies on the finding
(`Declined: the file change is needed`, citing the acceptance criterion); the implementer's
reply closes nothing. The Legion reviewer then posts `Accepted:` as the newest comment, and
`legion threads resolve --pr <N> --repo <owner>/<repo>` resolves it on the reviewer's
acceptance. Finish the round with that command reporting zero unresolved.

## Evidence

PR sjawhar/legion-smoke#512 drove three review rounds. Final file is four byte-exact lines;
the diff touches only `smoke/S391782039-1.md` plus the expected `.legion/` handoffs. Tester
re-ran the byte-exact checks at the pushed head and recorded `implementerProof.verdict:
verified`; CI (`sleep 20 && exit 0`) green, review APPROVED. The one process smell: the plan's
conditional acceptance criterion kept the wrong predicted review-line literal — align a plan's
predicted strings with the reviewer's canonical wording next time.
