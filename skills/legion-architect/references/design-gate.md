# The root spec and the design gate

Read this before you write a root spec, before any edit to an approved one, and on
`design-approved` or `design-changes-requested`. A child's own architect runs no gate.

## Writing the root spec

- Write it as `skill://dispatch-writing-specs` says, extending the issue's primary document in
  place with `dispatch doc-edit`. Never post a second spec artifact: `dispatch artifact --force`
  replaces a whole document. Each point the person decides is a decision block.
- Upload `verification.md` beside it (`--force` from its second version). Each clause of a check's
  `negative_control` names the test row or run that drives it. A requirement over every member of
  a class iterates the class's list, never a sample.
- Where a real recorded input exists (a stored record, a captured log, a real service's answer),
  hold at least one read-only check against it, or say in a decision block why none can be
  written. A requirement of the form "try X and see" is accepted by an observed real run.
- Cut each requirement carried over from an earlier issue that no source of this issue states:
  the words a person wrote in it, and what its `Filed for` line cites.

## A check CI proves

A check carrying `ci_check` is CI's: `ci_check` names the GitHub check run that proves it at the
head; it takes no `run`, its `when` is `before_merge`, and no role runs it or records its result
(`skill://legion-worker/references/ci-checks.md`). Where a CI job that runs on an ordinary pull
request already makes a check's proof, write that check as `ci_check` rather than as a command. A
`run` that names tests names specific tests or files; a whole suite is the CI job that runs it.

1. Name the check run of the job that runs the proof, never a roll-up. On cybermonklabs/legion
   never `ci-ok`, which passes when the jobs it did not select were skipped
   (`.github/scripts/ci-ok.sh`).
2. A job held back from ordinary pull requests needs its label on the pull request for as long as
   a check names it (`full-ci` on cybermonklabs/legion); without it the job reads `skipped`.
3. Write in `negative_control` the case inside that job that fails when the property breaks: a
   step with `continue-on-error` keeps its job green.

## Show the surface before you ask

Where this tree builds or changes a surface a person opens (a page, a screen, a dashboard), that
person sees it and answers before you request approval: a spec for a surface they have not seen is
refused. A new surface or a large visual change needs this round, and a flow needs a page they can
click; a small change inside a standing shape does not, unless you are unsure what they want. A
surface that is only text (an alert, a report, printed output) carries that literal text in the
spec instead.

1. Build the surface as a page from real readings of the real system. Draw the empty state and the
   worst realistic data, or say which states you left out. Mark every invented figure in the
   picture, and draw an existing surface as it is now beside the new one.
2. Take the picture with the pod's Chromium and put it on the issue, with a clickable page as a
   `file` artifact for a flow:

   ```bash
   chromium --headless --disable-gpu --no-sandbox --window-size=1440,900 \
     --screenshot=<file.png> file://<page.html>
   dispatch artifact --issue <KEY> --name <name>.png --path <file.png>
   ```

3. Ask one question with two to four answers, one of them that this is the wrong surface. Write no
   code for the round, and end your turn.
4. Put the agreed pictures in the spec, each with a line on what it shows and which readings are
   real, and one line on the states they do not show. The spec's words still govern the build.

You end the round, not the person. The round writes no spec version of its own, and a tree whose
gate is open starts none. Under `Design gate steps: none`, put the picture in the spec and ask
nobody. The surfaces children build are the root architect's.

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
   each unverified claim with a command or take it out.
4. Request approval only once the person has agreed to every point; a new point gets its own
   decision block first. `--include-verification` covers `spec` and `criteria` in one request:

   ```bash
   dispatch request-approval --issue <root> --include-verification --summary '<what they approve> Reviews: <gap result>, <claim result>; <what they do not show>.'
   ```

5. Register each step on its own document, with the id and version the request printed, never a
   slug. Leave `step` out only where your line names one step:

   ```ts
   legion({ op: "register_gate", issue: "<root>", step: "spec", artifactId: "<UUID>", version: <N> })
   ```

6. End your turn. The person's review brings your next task (the `design-approved` row of
   `skill://legion-architect/references/wake-routing.md`).

- A newer version moves the open request to it and to your turn, and the person stops seeing it:
  once they have agreed to that version, run `dispatch request-approval` again to give it back.
- `already approved at version <N>`: still `register_gate` that id and version.
- Refusals record nothing: `VERSION_NOT_IN_DISPATCH` (register the latest version),
  `GATE_STEP_REQUIRED` (name the step), `GATE_STEP_NOT_CONFIGURED` (register only gated steps),
  `GATE_STEP_DOCUMENT_MISMATCH` (register that step's own document).
- Never open a `dispatch ask` with an `Approve` option: the daemon ignores its answer.

## After approval

An approval is pinned to its version: any new version closes that step, with no wake. Two keep it:
a version whose only change is a person's answer to a decision block, and one equal to the approved
text (an accidental edit undone exactly).

Edit an approved spec only where its Summary, a requirement, a check, the scope or a settled
decision changes: put the problem to the person as a decision block at the end of the section it
changes. When their answer changes text outside the block, rewrite it and repeat steps 3 to 5,
with a `--summary` sentence naming what changed. Work in flight goes on; release no further child
until the next `design-approved`.

A change that keeps all five goes in the plan (`plan.md`), never the spec. A plan overturns a
settled decision only with evidence the person did not weigh, put to them as a decision block. A
person's later request is a requirement only once written into the spec and approved.

## A decision block for a read no pod can make

When a check needs evidence from a production or outside system, first check whether a credential
the pod carries reaches it: read the pod's environment (for example `AWS_CONFIG_FILE` or
`AGENT_SECRETS_URL`) and `skill://agent-secrets`. If none does, write the missing capability as a
decision block (which system, which read, why); never fold in a read another session made.
