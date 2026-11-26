# The root spec and the design gate

Read this before you write a root spec, before any edit to an approved one, and on
`design-approved` or `design-changes-requested`. A child's own architect runs no gate.

## Writing the root spec

- Write it as `skill://dispatch-writing-specs` says, extending the issue's primary document in
  place with `dispatch doc-edit`. Never post a second spec artifact: `dispatch artifact --force`
  replaces a whole document. Each point the person decides is an ask block.
- Upload `verification.md` beside it (`--force` from its second version). Each clause of a check's
  `negative_control` names the test row or run that drives it. A requirement over every member of
  a class iterates the class's list, never a sample.
- Your `Production check` line says whether this project checks production after a merge. When it
  names a role, write one `after_merge` check for the production check in the Verification
  document, with the spec and while the issue is open, because a closed issue refuses every new
  document. Under `Production check: off`, write none.
- Where a real recorded input exists (a stored record, a captured log, a real service's answer),
  hold at least one read-only check against it, or say in an ask block why none can be
  written. A requirement of the form "try X and see" is accepted by an observed real run.
- Cut each requirement carried over from an earlier issue that no source of this issue states:
  the words a person wrote in it, and what its `Filed for` line cites.

## A check CI proves

A check carrying `ci_check` is CI's (`skill://legion-worker/references/ci-checks.md`): it names
the check run that proves it at the head, takes no `run`, its `when` is `before_merge`, and no
role runs or records it. Where a CI job already makes a check's proof, write that check as
`ci_check` rather than as a command. A `run` that names tests names specific tests or files; a
whole suite is the CI job that runs it.

1. Name the check run of the job that runs the proof, never a roll-up. On cybermonklabs/legion
   never `ci-ok`, which passes when jobs it did not select were skipped
   (`.github/scripts/ci-ok.sh`).
2. Name it as GitHub writes it: `<caller> / <job>` for a job of a called workflow (`test / test`).
3. A held-back job needs its label (`full-ci` or `lane:<output>`) on the pull request while a
   check names it; without it the check reads `absent` or `skipped`.
4. Write in `negative_control` the case inside that job that fails when the property breaks: a
   step with `continue-on-error` keeps its job green.

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
2. Settle every ask block (`skill://dispatch-writing-specs`, "Approval").
3. Where the spec is large, or changed in a way you cannot check alone,
   `task(agent="plan-gap-analyst")` can find its gaps and `task(agent="plan-reviewer")`, told the
   document is a spec, can check its factual claims; each runs as a job of its own, so `wait` for
   its result.
4. Request approval only once the person has agreed to every point; a new point gets its own
   ask block first. `--include-verification` covers `spec` and `criteria` in one request:

   ```bash
   dispatch request-approval --issue <root> --include-verification --summary '<what they approve>'
   ```

   `--summary` says in one to three sentences what the person approves, and nothing else. A
   measurement or test result the design rests on goes in the document the request names.

5. Register each step on its own document, with the id and version the request printed, never a
   slug. Leave `step` out only where your line names one step:

   ```ts
   legion({ op: "register_gate", issue: "<root>", step: "spec", artifactId: "<UUID>", version: <N> })
   ```

6. End your turn. The person's review brings your next task (the `design-approved` row of
   `skill://legion-architect/references/wake-routing.md`).

- A version you write moves the request to your turn, and the person stops seeing it: once they
  agree, run `dispatch request-approval` again.
  Their answer, resolve or decline writes one version, which keeps it with them.
  The version carries an approval only when the person's answer, resolve or decline is its whole change.
  If text that a person was editing in the browser is settled into the same version, the approval does not carry, and the request goes back to its agent.
- `already approved at version <N>`: still `register_gate` that id and version.
- Refusals record nothing: `VERSION_NOT_IN_DISPATCH` (register the latest version),
  `GATE_STEP_REQUIRED` (name the step),
  `GATE_STEP_NOT_CONFIGURED` (register only gated steps, and `criteria` where the line names `spec` and not `criteria`),
  `GATE_STEP_DOCUMENT_MISMATCH` (register that step's own document).
- Never open a `dispatch ask` with an `Approve` option: the daemon ignores its answer.

## After approval

An approval is pinned to its version: any new version closes that step. The daemon tells the
architect and every phase holder when the gate closes. Four keep it: a version whose only change
is the approver's answer to an ask block, given after the approval at a browser or under a
credential with the approve scope, a block added since the approval included; one equal to the
approved text (an accidental edit undone exactly) with no ask block in it decided since; one whose
every change is a reading in a decision or an answered ask, a server-owned ask attribute or the
decisions step's `## Decisions` write; and, after a close and a reopen, the approved text with only
the questions the close withdrew open again. A close's own version keeps none. For each, Dispatch
records an `approved` review, the daemon sends `design-approved`, and you request nothing.

Edit an approved spec only where its Summary, a requirement, a check, the scope or a settled
decision changes: put the problem to the person as an ask block at the end of the section it
changes. When their answer changes text outside the block, rewrite it and repeat steps 4 and 5,
with a `--summary` sentence naming what changed. Work in flight goes on; release no further child
until the next `design-approved`.

A change that keeps all five goes in the plan (`plan.md`), never the spec. A plan overturns a
settled decision only with evidence the person did not weigh, put to them as an ask block. A
person's later request is a requirement only once written into the spec and approved.

## An ask block for a read no pod can make

When a check needs evidence from a production or outside system, first check whether a credential
the pod carries reaches it: read the pod's environment (for example `AWS_CONFIG_FILE` or
`AGENT_SECRETS_URL`) and `skill://agent-secrets`. If none does, write the missing capability as a
ask block (which system, which read, why); never fold in a read another session made.
