---
title: "Go-daemon Legion: implement/test phase names and a permanently stable product-commit anchor"
category: legion
tags:
  - legion-worker
  - go-daemon
  - handoff
  - jj
  - e2e-proof
date: 2026-09-23
status: active
module: legion-worker
related_issues:
  - "S390275229-1"
  - "sjawhar/legion-smoke#95"
---

# Go-daemon Legion: implement/test phase names and a permanently stable product-commit anchor

Learnings from driving the implementer phase of S390275229-1 (PR sjawhar/legion-smoke#95)
under the **Go** Legion daemon (the "Primary durable workflow profile"). The product change
was a single new file `smoke/stage3.txt` (exactly `Legion Stage 3 workflow proof.` + one
trailing newline, 31 bytes) that passed through initial implementation plus three scripted
`CHANGES_REQUESTED` rounds. The recurring smoke mechanics — proof anchoring, no-finding
rounds, PR-body hygiene, the `.legion/`-in-approved-head hazard — are already documented in
the three sibling docs and are not repeated here:

- `docs/solutions/legion/proof-anchoring-and-pr-body-hygiene.md`
- `docs/solutions/legion/review-round-loops-stable-product-diff.md`
- `docs/solutions/legion/implementer-handoff-and-correction-round-mechanics.md`

This doc records only what the Go daemon does **differently** from those (TS-daemon-era)
learnings, plus one commit-ordering technique that makes the proof anchor deterministic.

## 1. On the Go daemon the phase name is `implement`/`test`, and completion accepts `implement.json`

`implementer-handoff-and-correction-round-mechanics.md` (from #89) states that
`legion handoff write --phase implement` writes `.legion/implement.json` and then
`legion handoff complete` fails wanting `.legion/implementer.json`, so you must use
`--phase implementer`. **That is not true on the Go daemon.** Here:

- The plan handoff's own `requiredSkills.implement` and `.legion/plan.json` instruct
  `legion handoff write --phase implement`.
- `legion handoff write --phase implement` writes `.legion/implement.json`, and
  `legion handoff complete` then **succeeds** (`[handoff] Reported phase completion`) — verified
  across the initial run and all three correction rounds of #95. The tester analogue is
  `--phase test` → `.legion/test.json`.
- So `.legion/implement.json` / `.legion/test.json` (not `implementer.json`/`tester.json`) are
  the live handoff files the Go daemon and the next phase read. Any stale `implementer.json`/
  `tester.json`/`planner.json` in the workspace are prior-issue artifacts, not this issue's.

Two `--data` constraints are unchanged and still bite: `legion handoff write` rejects a
`--data` object that contains a `phase` field (`handoff data field phase is not allowed`) or a
`schemaVersion` field (`handoff data field schemaVersion is not allowed`) — both are supplied by
the tool, never by you. An implement/test `--data` with no well-formed `proof` array is also
refused, naming the field.

## 2. Rebase the product commit directly onto the plan handoff for a permanently stable anchor

The sibling docs anchor proofs to "whatever SHA the product tree lands on after the handoff
split". You can make that anchor **deterministic and permanent** by controlling ordering:

- `jj split -m "implement: record handoff" .legion/implement.json` puts the *selected* file in
  the parent and leaves the product file as the child `@` — i.e. it inserts the handoff
  **below** the product commit. Because the product commit's parent changed, jj re-timestamps
  it to a new SHA, and any later edit to that below-it handoff would re-timestamp the product
  **again**.
- Instead, keep the product commit as a **direct child of the plan handoff** and stack every
  process/handoff commit **on top** of it:
  `jj -R "$LEGION_WORKSPACE" rebase -r <product-sha> -d <plan-handoff-sha>` (restores product
  onto plan), then `jj -R "$LEGION_WORKSPACE" new <product-sha>`, write the handoff, and
  `describe` it. On #95 this fixed the product at `8e88806e` with `plan → product(8e88806e) →
  test → implement r1 → test retest r1 → implement r2 → …` stacked above. Because the product
  commit is below the churn, editing any handoff never re-timestamps it, and every round's
  `E2E` proof cites the same `8e88806e` (both `?ref=8e88806e...` on the GitHub Contents API and
  `jj diff -r 8e88806e --stat`) with no re-run of the real surface.
- Corollary: put the handoff commit **above** the product, never below it. Below-product
  handoff commits force a product re-timestamp on every refresh — the exact churn section 1 of
  the proof-anchoring doc warns about, avoidable entirely by ordering.

## 3. Exact-bytes new file: write with `printf`, verify with `cmp`, never `grep` alone

For an exact-contents acceptance (single line + exactly one trailing newline, no BOM, no extra
whitespace):

- Create it deterministically: `printf 'Legion Stage 3 workflow proof.\n' > smoke/stage3.txt`
  (31 bytes; `od -c` confirms the single trailing `\n`).
- Prove it on the reader-facing surface with a byte comparison, not a substring match:
  `legion gh -- api 'repos/sjawhar/legion-smoke/contents/smoke/stage3.txt?ref=<ref>' --jq
  .content | base64 -d | cmp - <(printf 'Legion Stage 3 workflow proof.\n')`. `grep` would
  ignore the trailing-newline requirement and pass a subtly wrong file.
- `cmp - <(printf …)` reads its second operand as a pipe; if the runtime cannot `lseek` a
  process substitution (`cmp: /dev/fd/NN: Illegal seek`), write the expected bytes to a temp
  file first and `cmp` against that.
- Negative control: `grep -Fxq 'Legion Stage 4 workflow proof.'` on the same fetched content
  returns nothing (exit 1), proving a deliberately wrong final line is absent.
