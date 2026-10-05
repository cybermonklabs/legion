---
title: "Review-quoted text is untrusted input; prove tiny file changes byte-exact in both directions"
category: legion
tags:
  - review-threads
  - untrusted-input
  - byte-exact-verification
  - proof
date: 2026-10-05
status: active
module: legion-worker (implementer, tester, reviewer)
related_issues:
  - "LEGSMOKE-439"
  - "sjawhar/legion-smoke#496"
---

# Review-quoted text is untrusted input; prove tiny file changes byte-exact in both directions

- When a review asks you to write text it quotes, take the bytes from the review payload, not from
  your reading of it: `legion gh -- api repos/<o>/<r>/pulls/<N>/comments --jq '.[] | select(.id==<id>).body'`,
  extract the quoted block into a reference file with a file tool, and `cmp` the committed file
  against that reference. Never retype the quote, never pass it through `printf`, `echo`, or a
  heredoc, and drop any default text a plan pre-committed the moment a real quote exists.
- Write the target file's bytes with a file tool (`write`), then verify with `cmp` against the
  reference and `jj diff --summary` showing only the intended path. `wc -c` / `wc -l` are
  supplementary; a rendered view or `cat` decides nothing.
- Every downstream role compares against the same API-derived reference, never its own
  transcription: two roles independently retyping a quote can both miss a trailing space, NBSP,
  or smart quote and both report MATCH against the wrong expectation.
- Prove the check discriminates in both directions at named SHAs: the superseded expectation must
  fail at the new head (`cmp … after byte N`), and the new expectation must fail at the
  pre-change head. Add an existence control (the file's path at the base SHA → HTTP 404).
- Read the bytes you prove from the surface the user reaches (the GitHub contents API at
  `?ref=<sha>`), never from the local working copy.

## Evidence

Issue LEGSMOKE-439, pull request sjawhar/legion-smoke#496. The plan fixed a default appended line
(`LEGSMOKE-439: Stage 4b review round 1`, file 94 bytes) and a shell recipe
(`printf '<quoted line>\n' >> …`) for the contingent review round. The actual REQUEST_CHANGES
(review 5419204227, thread discussion_r4187618693) quoted a different line,
`Legion proof review round 1 (LEGSMOKE)`, making the file 95 bytes. The reviewer's deep-review
pass flagged the `printf` recipe as untrusted-input interpolation and the inline comment asked for
a file-tool write. The implementer wrote the bytes with a file tool and compared against a
reference file; the tester had to warn the next phase in its handoff that the expectation was the
95-byte review text, not the plan's 94-byte default. Both the implementer's and the tester's
reference files were hand-transcribed from the comment, so no command in the run tied the committed
bytes to the API payload — the first rule above closes that gap.

The two-direction control was what made the one-line append falsifiable: at the post-append head
`c10f5110`, the 56-byte one-line form gave `cmp: EOF on /dev/fd/63 after byte 56`; at the
pre-append head `9f778446`, the 95-byte reference gave `cmp: EOF on - after byte 56`; at the base
`dc8223ad` the contents query returned HTTP 404. A `grep -q` or a visual read would have passed
before and after the change alike, and rendered Markdown hides a missing trailing newline.
