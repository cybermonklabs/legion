# The root spec and the design gate

Write the spec and its Verification document as `skill://dispatch-writing-specs` says.
A child's architect runs no gate.

## A check CI proves

1. Name the check run of the job that runs the proof, never a roll-up.
   On cybermonklabs/legion, never name `ci-ok`: it passes when the jobs it did not select were skipped (`.github/scripts/ci-ok.sh`).
2. A held-back job needs its label on the pull request while a check names it (on cybermonklabs/legion, `full-ci`, or the job's `lane:` label that `.github/workflows/ci.yaml` names beside it); without it the check reads `absent` or `skipped`.
3. Write in `negative_control` the case inside that job that fails when the property breaks.
   A step with `continue-on-error` keeps its job green.

## Show the surface before you ask

Where this tree builds or changes a surface a person opens (a page, a screen, a dashboard), that person sees it and answers before you request approval.
A new surface or a large visual change needs this round, and a flow needs a page they can click.
A small change inside a shape that stands needs none, unless you are not sure what they want.
For a surface that is only text (an alert, a report, printed output), put that literal text in the spec instead.

1. Build the surface as a page from real readings of the real system.
   Draw the empty state and the worst realistic data, or say which states you left out.
   Mark each invented figure in the picture.
   Draw an existing surface as it is now, beside the new one.
2. Take the picture with the pod's Chromium, and put it on the issue.
   For a flow, also put the clickable page on the issue as a `file` artifact.

   ```bash
   chromium --headless --disable-gpu --no-sandbox --window-size=1440,900 \
     --screenshot=<file.png> file://<page.html>
   dispatch artifact --issue <KEY> --name <name>.png --path <file.png>
   ```

3. Ask one question with two to four answers. One answer says that this is the wrong surface.
   Write no code for the round, and end your turn.
4. Put the agreed pictures in the spec, each with a line on what it shows and which readings are real.
   The spec's words still govern the build.

The round writes no spec version of its own.
A tree whose gate is open starts no round.
Under `Design gate steps: none`, put the picture in the spec and ask nobody.
A child's architect sends the surface to the root's architect, who runs the round.

## Which steps are gated

Your `Design gate steps` line names the gated steps, or `none`.
The step `spec` records the primary document, `criteria` the Verification document, and `decisions` the decisions document.
Where the line names `spec` and not `criteria`, register the Verification document as `criteria` too.
Under `none`, register the spec at its current version with no `step`, and request no approval: that registration opens the gate.
Then tell each phase holder of each later version of the root's documents yourself, with `envoy_publish`.

A root that is a view on an approved ancestor holds no `requirement` block of its own.
It registers each gated step on the ancestor's approval instead, with `register_gate`'s `basis` and `basisVersion`, and requests no approval of its own.

## The armed gate

Arm the gate only after every child is filed and the root has its plan.

1. Settle each ask block of the document.
2. Request approval only when the person has agreed to every point.
   A new point gets its own ask block first.
   `--include-verification` asks for the spec and the Verification document in one request.
3. Register each gated step on its own document, at the version that the request printed:
   `legion({ op: "register_gate", issue: "<root>", step: "spec", artifactId: "<id>", version: <N> })`.
   An answer of `already approved at version <N>` still needs that registration.
4. End your turn. The person's review brings your next wake.

Each new version of a document needs a new request and a new registration at that version.
Never open a `dispatch ask` with an `Approve` option: the daemon ignores its answer.

## After approval

A new version closes its step, and the daemon tells you and each phase holder.
Work in flight goes on; a child released while the gate is closed waits for it to open.

Change an approved spec only where its Summary, a requirement, a check, the scope or a settled decision changes.
Put that change to the person as an ask block at the end of the section it changes.
When the answer changes text outside the block, rewrite that text, request approval, and register again.
The `--summary` names what changed.

Every other change goes in the plan, never the spec.
A plan overturns a settled decision only with evidence the person did not weigh, put to them as an ask block.
