---
title: "Legion Stage 3 durable-workflow marker proof: handoff hygiene and head divergence"
category: legion
tags:
  - legion-workflow
  - handoffs
  - proof-discipline
  - go-daemon
date: 2026-09-28
status: active
module: legion
related_issues:
  - "S393569515-1"
  - "sjawhar/legion-smoke#420"
---

# Legion Stage 3 durable-workflow marker proof: handoff hygiene and head divergence

Reusable learnings from running the full Legion phase pipeline
(architect → plan → implement → test → review×4 → retro) for a single-file marker
change (`smoke/S393569515-1.md`) under the **Go daemon**. The change itself was trivial;
the durable value is in the workflow mechanics observed.

## Use the `legion` tool's `handoff_write`, never the bare CLI, to write a handoff

The `legion` tool's `handoff_write` validates the payload against the phase schema before
writing (an `implement` handoff without a well-formed `proof` is refused, naming the field).
The bare CLI `legion handoff write --phase implement --data '{"summary":"x"}'` performs **no
such validation** — it silently overwrote a complete `implement.json` with the stub. If you
must pipe an accumulated handoff through bash, re-emit the *whole* validated object; a partial
payload clobbers the good file. Prefer the tool.

## `retro` and `merge` are not handoff phases

`legion handoff write --phase retro` (and `--phase merge`) exits 2 with usage — only
`architect`, `plan`, `implement`, `test`, `review` are handoff phases. Retro writes **no
`.legion` file**: its only durable outputs are the learning document(s) under
`docs/solutions/` and one Dispatch message on the issue, and it reports completion with the
`legion` tool's `handoff_complete` alone. Do not probe the schema with a throwaway `--data`
against a real phase name — the CLI will write it.

## In the Go-daemon flow, handoffs stay on the branch and the head diverges from CI/approval

Unlike the standalone-worker skill's ordering (delete `.legion/` and re-approve the clean head
before retro), the Go-daemon Stage-3 flow commits each phase's `.legion/<phase>.json` onto the
issue branch and never deletes it during the branch's life — a post-merge fixture removes the
handoffs and learnings later (see prior `proof fixture: remove the handoffs and learnings`
commits). Consequences a future worker must expect:

- The PR diff carries the added `.legion/*.json` files, not just the product file.
- Every phase's handoff commit advances the head, so the **code head** (e.g. `0540e61c`),
  the **tester's verified head** (`c6769d5b`), and the **reviewer's approved head**
  (`a02e2267`) are three different SHAs, and the approval head may sit above the last head
  that carries fresh CI evidence. Always cite the *code* head in the E2E lines and record
  which SHA each gate was observed at.

## Proof discipline for a docs/marker diff

Even with no runtime code, the E2E proof is real: read the file through the GitHub contents
API at the branch ref (`...contents/smoke/<KEY>.md?ref=legion/<KEY> --jq .content | base64 -d`),
and pair it with a **two-sided 404 negative control** — a misnamed path on the branch, and the
correct path on `main` (absent pre-merge). `ce-simplify-code` and the thermonuclear pair are
correctly omitted as docs-only. The tester independently re-runs the implementer's exact
command and confirms `+N/-0` via `GET /pulls/<n>/files` that no existing file changed.
