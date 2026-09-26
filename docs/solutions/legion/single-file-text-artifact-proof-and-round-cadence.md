---
title: "Legion proof discipline and review-round cadence for a single-file text-artifact change"
category: legion
tags:
  - legion-worker
  - implementer
  - e2e-proof
  - negative-control
  - jj
  - review-rounds
date: 2026-09-26
status: active
module: legion-worker
related_issues:
  - "S390834035-1"
  - "sjawhar/legion-smoke#312"
---

# Legion proof discipline and review-round cadence for a single-file text-artifact change

Distilled from PR #312 (issue S390834035-1): a text-only change that appended one proof line
plus three review-round lines to the repo-root file `smoke.txt`. No runtime code, so
`ce-simplify-code` and the thermonuclear review pair were correctly skipped. The learnings below
are the reusable parts for any future Legion phase worker driving a small, verifiable artifact.

## The proof head SHA is not the PR head — verify at the live head

Every phase commits its own `.legion/<phase>.json` sidecar into the branch, so the branch head
advances after your code commit. On this PR the implementer recorded its round-3 proof at
`abff9751b63f`, but by test time the PR head was `429f40114c81` (implement + test sidecars on
top), and the reviewer approved at `f6185fa2882f`.

- Record your own proof at the head you actually produced, but read the current head from
  `legion gh -- pr view <n> --json headRefOid` / `jj log` at verification time — never assume
  your commit SHA equals the PR head.
- The tester must both (a) record its own proof at the real PR head and (b) re-run the
  implementer's literal command at the implementer's recorded SHA to confirm an identical result.

## Negative-control template for a marker append

A reusable, strong control has two halves:

1. A same-class wrong string that must count `0` — the next unrequested sequential marker
   (`Stage 3 review round 4 ...`) or a wrong-date variant (`2026-09-27`). This proves the line
   you added is uniquely the one requested, not a neighbouring plausible one.
2. Absence of the target line at the parent commit before the append (`grep -cx <line>` at the
   parent → `0`). This proves the append introduced it.

Count with exact whole-line match `grep -cx` (each marker == 1), not substring `grep -c`, so a
duplicated or partial line is caught.

## jj commit discipline: one atomic append per round, sidecar split separately

For a one-file text change per iteration:

1. Append the line, `jj describe -m "<KEY>: append <what> line to <file>"` the working copy as the
   code commit, then `jj new` to leave a clean empty working copy.
2. Push the code commit (`bookmark set legion/<KEY> -r @-` then `jj git push`).
3. `handoff_write`, then `jj split -m "implement: record handoff (review round N)" .legion/implement.json`
   so the sidecar is its own commit, and push again.

Keeping each append atomic (one line per commit, one commit per round) is what lets the tester
attach a clean per-round proof entry and the reviewer anchor feedback to a specific commit. Do
not squash rounds into the append.

## Each review round is its own implementing phase

A push alone does not finish a round. Every review round is a fresh `implementing` phase that must
end with the `legion` tool's `handoff_complete`. Treat identical boilerplate review comments that
request the next sequential marker as smoke-cadence steps: append exactly the requested line,
change nothing else, re-test, complete the phase — and watch for the terminal `APPROVE` rather
than inferring done from CI green. The "one extra line permitted per review round" allowance
stacks once per round, so budget the artifact and verification for N rounds, not one.

## Tooling gotcha: reading review threads through the `gh` shim

`legion gh -- api graphql -F query=@file` is refused by the worker `gh` shim with
`Legion never merges a pull request ...`: the `-F` flag reads like a mutation to the shim's
heuristic. To inspect review threads/comments without tripping it:

- List review comments with a plain REST GET:
  `legion gh -- api repos/<owner>/<repo>/pulls/<n>/comments --jq '.[] | {id,user:.user.login,path,line,body}'`.
- Disposition threads with the purpose-built command:
  `legion threads resolve --pr <n> --repo <owner>/<repo>` (prints `resolved`/`left open`, or
  `no unresolved threads`), and paste its output into the PR body's `Threads` section.
