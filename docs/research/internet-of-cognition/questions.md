# The questions this reading answers

This is the question list for dispatch://INFRA-3134.
It comes from our own code and our own issues, and it was written before any part of the Internet of Cognition stack was read deeply.
Every question names the file, the symbol or the Dispatch record that raises it.

The order of the work is fixed by this file: this commit holds this file alone, and every commit that adds a deep-read evidence file descends from it.
The reverse order prices the work by the line count of a repository we did not write.

Each question is in the reader's terms, and it is a question about our own fleet.
A question is answered by a candidate page of `judgement.md` on dispatch://INFRA-3134, or it is named on that document's front page with the reason it is not.

Every file, symbol and line below was read in the workspace of this issue at commit `9542c6fbbecd184ccee73523ab3defa39d6cf23a` on 2026-11-12.

## 1. When a role writes a fact it read somewhere else, what carries the way back to the record it read?

Raised by `packages/envoy/internal/contracts/generated.go:13-33`, the `Envelope` type, which has nineteen fields and no field that holds a Dispatch node reference.
An issue reaches the receiver only inside the `Topic` string, and a quoted fact travels as text in `Payload`.
`func (e Envelope) Validate() error` (`packages/envoy/internal/contracts/generated.go:43`) validates every other field of the envelope.

## 2. Where is the one record that says what a term means, so that two roles cannot use one word for two things?

Raised by dispatch://INFRA-323/artifact/defining-terms-draft-md, which is the draft already written for this, and by the measured failure of a text comparison on a number, a unit and a polarity in dispatch://INFRA-3134/artifact/ioc-design-2026-11-12-md.

## 3. What did one role's run cost, how long did it take, and which model ran it?

Raised by an absence: no table of `packages/daemon/internal/store/migrations` holds a span, a trace of its own, a cost, a token count or a model name.
The one handle the daemon keeps on a run's cost is the resource-attribute string it composes for the launch, `packages/daemon/internal/runtime/spendattrs.go:11-21`, whose two Legion keys the spend collector's keep-list drops today (`docs/kubernetes.md:5133`).

## 4. When the reviewer's two graders disagree at one head, what decides the round?

Raised by `skills/legion-worker/references/merge-gate.md:24-35`, which has the reviewer run `thermonuclear-deep-review` and `thermonuclear-code-quality` once at the reviewed head and "record the verdict from what those results say", and by `skills/legion-worker/references/merge-gate.md:80-87`, which asks for one review per round.
Two graders, two criteria, one head, one answer, and no written rule that combines them.

## 5. What does the next implementing round read to know what the last round found short?

Raised by `skills/legion-worker/references/pr-body.md:33-34`, the `Thermo` line, which records the pair as one verdict in prose, and by the review body that the round hands to the next round as prose.
Nothing of ours parses either, so no reader can say whether the next round answered the last round's shortfall.

## 6. What stops a reviewer completing a round with no pair result recorded at the head it reviewed?

Raised by `packages/daemon/internal/api/handoff.go`, where a tester completion with no verdict is refused, beside a reviewer completion that is refused for no such thing.
The answer today is the skill's prose and nothing else.

## 7. How would we know whether a change to a role's prompt or skill made Legion better?

Raised by an absence: nothing in this repository measures a role's prompt or skill against an outcome.
Each change to `skills/` and to `packages/daemon/internal/prompts/` is reviewed by reading, merged, and never measured.

## 8. Can Legion's own phase workflow be held as a specification rather than as Go?

Raised by `packages/daemon/internal/workflow/table.go:61`, `var Table`, which holds thirty-eight rows, each a from-phase, a trigger, a guard, a to-phase and its effects (`type Row struct`, `packages/daemon/internal/workflow/table.go:47-53`).
A guard is a Go function value, which is the part a specification format must hold or drop.

## 9. Does our own record hold a finding with its evidence and its verdict?

Raised by `packages/envoy/internal/dispatch/pmdoc/schema/blocks.json`, which declares `"version": 3` and five typed blocks, `callout`, `ask`, `requirement`, `plan_task` and `check`, and neither a finding nor a verdict among them, and by dispatch://INFRA-1098/spec and dispatch://INFRA-1253/spec, which specify the node model this fleet is moving to.

## 10. Where do our own coordination failures happen: across sessions, between an agent and a person, or inside one tree?

Raised by dispatch://INFRA-3134/comment/2833bda4-0f39-4ee3-a876-5eb0040e5e09 and dispatch://INFRA-3134/comment/326a044a-db33-4779-a98e-8271e29e710f, which assert that this fleet has the three alignment problems the stack addresses, and give no count.
The answer decides whether a change belongs in the Envoy envelope, in the Dispatch record, or in neither.

## 11. What enforces a contract at a system boundary that our own refusals do not?

Raised by the refusal codes a Dispatch write meets in `packages/envoy/internal/dispatch/api/` and the completion refusals in `packages/daemon/internal/api/handoff.go`, which are the whole of what we enforce at a boundary today.

## 12. Is there anything to adopt for the third problem, agreeing after the work that what was delivered meets what was agreed?

Raised by dispatch://INFRA-3134/comment/326a044a-db33-4779-a98e-8271e29e710f, which reports that the authors name the protocol for that tier as not released, and by this issue's own Verification document, which is where that agreement lives for us today.
