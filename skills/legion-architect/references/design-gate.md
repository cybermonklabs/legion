# The root spec and the design gate

Read this before you write a root spec, before any edit to an approved one, and on
`design-approved` or `design-changes-requested`. A child's own architect runs no gate.

## Writing the root spec

- Write it as `skill://dispatch-writing-specs` says, extending the issue's primary document in
  place with `dispatch doc-edit`: a new version adds only the evidence each decision needs and
  what the person decides, each as a decision block. A decision that needs the person, yours or a
  worker's, is a decision block you write, never the worker.
- Never post a second spec artifact: `dispatch artifact --force` replaces a whole document, so pass
  it only where you mean to replace the whole text.
- Wave releases, child closures and your own status are visible from the issue tree and the
  handoffs: do not narrate them into the spec or a `dispatch message`.
- Upload `verification.md` beside it (`--force` from its second version). Each clause of a check's
  `negative_control` names the test row or run that drives it. A requirement over every member of
  a class iterates the class's list, never a sample.
- Every later role reads the checks: the implementer and the tester record a result against each
  check with no `ci_check`, and the reviewer approves only once a pass stands for every
  `before_merge` check at the head, which is the check's own answer in
  `dispatch results --issue <issue>`, never a pass at another commit, a pass recorded under an
  older wording of the check or a retracted one.
- Where a real recorded input exists (a stored record, a captured log, a real service's answer),
  hold at least one read-only check against it, or say in a decision block why none can be
  written. A requirement of the form "try X and see" is accepted by an observed real run.
- Cut each requirement carried over from an earlier issue that no source of this issue states:
  the words a person wrote in it, and what its `Filed for` line cites.

## A check CI proves

A check carrying `ci_check` is CI's: it names the GitHub check run that proves it at the head, and
takes no `run`. Read `skill://legion-architect/references/design-gate-ci-check.md` before you
write a check that carries `ci_check`, or a check whose `run` names tests.

## Show the surface before you ask

Where this tree builds or changes a surface a person opens (a page, a screen, a dashboard), that
person sees it and answers before you request approval: a spec for a surface they have not seen is
refused. Read `skill://legion-architect/references/design-gate-surface.md` when this tree builds or
changes a surface a person opens or reads: it says which changes need the round, how to run it,
and where the pictures go.

## Which steps are gated

The `Design gate steps` sentence of your `Legion addressing` line names the gated steps, or `none`.
Each step records one document of the root issue:

| Step | Document |
| --- | --- |
| `criteria` | the Verification document (`verification.md`) |
| `spec` | the primary document |

Under `none`, `register_gate` the spec at its current version with no `step`, and request nothing.

## The armed gate

For each gated step, once every child is filed:

1. Edit the document in place: `dispatch doc-edit --issue <root> --ops-json-file -`.
2. Settle every decision block (`skill://dispatch-writing-specs`, "Approval").
3. Give the current spec, the code and output it relies on, and the person's words to two fresh
   reviewers: `task(agent="plan-gap-analyst")` for its gaps, and `task(agent="plan-reviewer")`,
   told it is a spec, for each factual claim. `wait` for each, resolve every finding, and check
   with a command each claim either marks unverified: fix it when it is false, or take it out.
4. Request approval only once the person has agreed to every point; a new point gets its own
   decision block first. `--include-verification` covers `spec` and `criteria` in one request:

   ```bash
   dispatch request-approval --issue <root> --include-verification --summary '<what they approve> Reviews: <gap result>, <claim result>; <what they do not show>.'
   ```

   `--summary` says in one to three sentences what the person is approving and nothing else (no
   commentary and no open question), then one sentence with both reviews' results and what they
   do not show.
5. Register each step on its own document, with the id and version the request printed, never a
   slug. Leave `step` out only where your line names one step:

   ```ts
   legion({ op: "register_gate", issue: "<root>", step: "spec", artifactId: "<UUID>", version: <N> })
   ```

6. End your turn. The person's review brings your next task (the `design-approved` row of
   `skill://legion-architect/references/wake-routing.md`).

- A newer version moves the open request to it and to your turn, and the person stops seeing it:
  once they have agreed to that version, run `dispatch request-approval` again to give it back.
- `already approved at version <N>`: still `register_gate` that id and version. You never approve
  anything yourself.
- Refusals record nothing: `VERSION_NOT_IN_DISPATCH` (read the document's latest version, check
  it is the spec the person is asked to approve, and register that version), `GATE_STEP_REQUIRED`
  (name the step), `GATE_STEP_NOT_CONFIGURED` (register only gated steps),
  `GATE_STEP_DOCUMENT_MISMATCH` (register that step's own document).
- Never open a `dispatch ask` with an `Approve` option: the daemon ignores its answer.

## After approval

An approval is pinned to its version: any new version closes that step, with no wake. One step
closing holds the tree's work whatever the other steps stand at. Two versions keep it: one whose
only change is a person's answer to a decision block, and one equal to the approved text with no
decision block in it decided since. Undo an accidental edit of an approved root spec exactly:
when the spec then reads Approved at the new version, request nothing; when it does not (a block
in it was decided since), request approval again with steps 3 to 5.

Edit an approved spec only where its Summary, a requirement, a check, the scope or a settled
decision changes: put the problem to the person as its own decision block, with its evidence, at
the end of the section it changes, and add nothing else. If you edit the block before they
answer, the gate closes like any new version, but their answer still carries the approval to the
version it writes. When their answer changes text outside the block, rewrite it and repeat steps
3 to 5, with a `--summary` sentence naming what changed. Work in flight goes on; release no
further child until the next `design-approved`.

A change that keeps all five goes in the plan (`plan.md`), never the spec. A plan that would
overturn a settled decision goes back to the planner with the decision kept, unless the planner
brings evidence the person did not weigh: put that to them as a decision block. A person's later
request is a requirement only once written into the spec and approved; it is never another
issue's, and no comment or answer of theirs takes a requirement out. Later children and
re-scoping open children toward the same requirements need no spec edit and no new approval.

## A decision block for a read no pod can make

When a check needs evidence from a production or outside system, first check whether a credential
the pod carries reaches it: read the pod's environment (for example `AWS_CONFIG_FILE` or
`AGENT_SECRETS_URL`) and `skill://agent-secrets`. If none does, write the missing capability as a
decision block (which system, which read, why), and leave it open until the person resolves it;
never fold in a read another session made.
