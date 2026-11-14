# The root spec and the design gate

Part of `skill://legion-architect`, section 1. Read it before you write the root spec, before any
edit to an approved root spec, and on `design-approved`, `design-changes-requested`, `basis-ended`,
`basis-changes-requested` or `resting-root`.

## Writing the root spec

Specifications written into Dispatch follow
[`dispatch-writing-specs`](skill://dispatch-writing-specs/SKILL.md#the-outline). Wave releases, child closures and your
own status are visible from the issue tree and the handoffs; do not narrate them into the spec or a
`dispatch message`.

Extend the primary document in place: a new version adds only the evidence each decision needs and
what the human decides, each as a [decision block](skill://dispatch/SKILL.md#asks-and-ask-blocks).
Never post a second "spec" artifact beside it: `dispatch artifact` with the primary document's name
replaces the human's document. The command refuses an upload under the name of a document the
issue already holds and names `--force`, so that flag is what writes over such a document: pass it
only where you mean to replace the whole text, and edit a document with `dispatch doc-edit`.

Mark each requirement of the spec as a `requirement` block, and write the issue's Verification
document (`verification.md`) with it, uploading each version after the first with `--force`: one
`check` block per thing that must be proved, each with
`serves` naming the requirements it serves, its `surface`, its `run`, its `when` and its
`negative_control`, or `ci_check` in place of its `run` (below). The human approves both
documents in one request (step 4 below), and every later role reads the checks: the implementer
and the tester record a result against each check with no `ci_check`, and the
reviewer approves only once a pass stands for every `before_merge` check at the head, which is the
check's own answer in `dispatch results --issue <issue>`, and never a pass at another commit, a
pass recorded under an older wording of the check or a retracted one.

A root under an approved ancestor can be a view on that ancestor instead: it marks no requirement
block, and its checks serve the ancestor's requirements. "A root that is a view on an approved
ancestor" below says when, and how its gate opens on the ancestor's approval.

On your first pass over the spec, cut each requirement carried over from an earlier issue's spec or
decisions that no source of this issue states: those were decided for that issue. This issue's
sources are the words a person wrote in it, and its `Filed for` line with what that line cites: the
answer, Slack message or direct message of a `Filed for <person> at their request:` line, or the
evidence of a controller's `Filed for Legion from <role> on <issue>:` line
([The `Filed for` line](skill://legion-controller/references/finding-asks.md#the-filed-for-line)).

## A check CI proves

A check carrying `ci_check` is CI's: `ci_check` names the GitHub check run that proves the check
at the head, and the check takes no `run`. Its `when` is `before_merge`, and it names its
`negative_control`. No role runs it or records a result for it. `dispatch results` derives its
one result from the named run (`skill://legion-worker/references/ci-checks.md`). Where a CI job
that runs on an ordinary pull request already makes a check's proof, write that check as
`ci_check` rather than as a command. Write such a check by three rules:

1. Name the check run of the job that runs the proof, never a roll-up run that stands for other
   jobs. A roll-up run can pass at a head where the job of the check did not run.
2. Name no job that the repository holds back from an ordinary pull request. Such a job gives no
   pass at any head the reviewer reads, so its proof stays a check with a `run`.
3. Write in `negative_control` the case inside that job that fails when the property breaks. A
   job's conclusion is not the property: a step with `continue-on-error` keeps its job green,
   and a test deleted from a suite takes its failure with it.

On cybermonklabs/legion: `ci-ok` passes when the jobs it did not select were skipped
(`.github/scripts/ci-ok.sh`), so never name `ci-ok`. Eight slow suites and the full browser lane
run only in the merge queue or under `full-ci`.

## Show the surface before you ask

Where this tree builds or changes a surface a person opens — a page, a screen, a dashboard — that
person sees it and answers before you ask them to approve the spec. A spec for a surface they have not
seen is refused, and the refusal costs a spec version, a closed gate, a second request and their time
twice.

Whether a change needs the round is your judgement. These readings inform it:

- A new surface a person opens, and a large visual change to one, need it. The larger the visual
  change, the more a page the person can click is worth over a picture: a person judges a layout from
  a picture, and judges a flow only by acting on it.
- A small change inside a shape that stands — a label, a copy change, a style fix, a bug fix — does
  not, unless you are unsure what the person wants, which is itself a reason to show them.
- A change whose only reader surface is text — an alert message, a report, a command's printed output
  — carries that literal text in the spec, at the values it will carry, and needs no round. The person
  reads the spec before the build; a pull request body reaches a reviewer after it.
- A change with no surface a person reads needs nothing, and nothing in this section applies to it.

The round is standard user research run on one reader: show the surface rather than describe it, let
them act on it where the flow is the question, ask what they expected rather than whether they
approve, and show another round on an answer that asks for changes
(https://www.nngroup.com/articles/ux-prototype-hi-lo-fidelity). Before you first ask the person to
approve the spec:

1. Build the surface as a page, from real readings of the real system. Draw the empty state and the
   worst realistic data, or say which states you did not draw. Mark every invented figure in the
   picture itself, the way a check names its negative control: a picture whose figures are all
   plausible is the hardest one to judge. Where the surface exists already, draw it as it is now
   beside the new one: a picture a person cannot place is a round spent for nothing.
2. Take the picture with the pod's own Chromium, then put each one on the issue:

   ```bash
   chromium --headless --disable-gpu --no-sandbox --window-size=1440,900 \
     --screenshot=<file.png> file://<page.html>
   dispatch artifact --issue <KEY> --name <name>.png --path <file.png>
   ```

   The upload stores it as an `image` artifact and prints its slug.
3. For a large visual change, or where the question is about a flow rather than a layout, build the
   page so the person can click it, put that one file up beside the pictures as a `file` artifact, and
   say in your question what works in it and what does not.
4. Ask one question with two to four answers, one of which says this is the wrong surface. Write no
   code on the branch for the round, and end your turn as for any question to a person.
5. The agreed pictures go in the spec, in line, each with a line of prose under it saying what it
   shows and which of its readings are real:

   ```md
   ![the priorities page with no priority set](dispatch://<KEY>/artifact/<slug>)
   The page as a reader opens it. The 529 issues and their ages are read from this deployment; the
   two ranks on the right are invented.
   ```

   One further line says what the pictures do not show: the states you did not draw. The words of the
   spec still govern the build, because a reader on the command line gets the link text and not the
   picture.

You end the round, not the person: when their answer is carried into the pictures or into the spec,
go on to the approval request with the pictures as they then stand. A person who stops answering
holds nothing up.

The picture is the cheapest thing that answers the question. A labelled sketch is enough where it
answers it. Where the real surface, built read-only, costs no more than the mock-up, build that and
show that.

The ordering, which this rule needs and does not change:

- A spec written before the picture is fine. What may not happen is reaching the gate with no picture.
- A tree whose gate is already open starts no round for the work in flight and writes no spec version
  for a picture: a new version closes the gate and stops the phase under it ("After approval").
- The round writes no spec version of its own. The agreed pictures go into the version you first
  submit for approval.
- A picture that must change after approval follows "After approval": it is a new version only when
  what is built changes, and then you request approval again.
- Where the `Design gate steps` sentence of your `Legion addressing` line says `none`, nobody approves
  the spec. Make the picture, put it in the spec for whoever builds it, and ask nobody.
- The surfaces this tree's children build are yours. Their pictures go in the root spec, and a
  sub-architect that finds a surface the root spec never drew tells the root architect.

## Which steps are gated

The `Design gate steps` sentence of your `Legion addressing` line names the steps this deployment
gates, in the order it gates them, each in backticks: ``Design gate steps: `spec`.``,
``Design gate steps: `decisions`, `criteria`.``, or `Design gate steps: none.` A sub-architect on
a child issue has no such sentence and never runs the gate.

Each step is one document of the root issue, and the daemon keeps one gate record for each step:

| Step | The document it records |
| --- | --- |
| `decisions` | the issue's decisions document (`decisions.md`) |
| `criteria` | its Verification document (`verification.md`) |
| `spec` | its primary document |

The tree's work starts when every step the sentence names is open, so run the armed sequence below
once for each of them, each on that step's own document. Under `Design gate steps: none.` write
the spec and register it with `register_gate` at its current version, with no `step` and no
approval step: do not request approval and do not wait for `design-approved`; the gate opens at
registration. File every child of your decomposition before that registration, since the gate's
opening starts the root's own first phase unless Dispatch shows an open child under it.

## The armed gate

Run this exact sequence for each gated step, before the tree's work starts, with every child of
your decomposition filed before step 4, since the gate's opening starts the root's own first
phase unless Dispatch shows an open child under it. One approval request covers the `criteria`
and the `spec` step together (step 4), and each of them is registered on its own document
(step 5); the `decisions` step is a request of its own.
A root that is a view on an approved ancestor runs steps 1 to 3, then registers on the ancestor's
approval in place of steps 4 and 5, with every child filed before that registration ("A root that
is a view on an approved ancestor", below).

1. Extend the primary document in place:

   ```bash
   dispatch doc-edit --issue <root issue> --ops-json-file - <<'EOF'
   ...
   EOF
   ```

2. Settle every decision block as
   [Approval](skill://dispatch-writing-specs/SKILL.md#approval) says;
   `dispatch request-approval` refuses while one is open. Each answer reaches you, since you follow
   every ask you open.
3. Before every approval request, give two fresh reviewers the current root spec, the code and
   command output it relies on, and the human's words: `task(agent="plan-gap-analyst")` finds its
   gaps, and `task(agent="plan-reviewer")`, told the document is a spec, checks each factual claim
   against the code, the docs or a read-only command it runs. Each runs as a job of its own, so
   `wait` for its result and read what it found before you act on it. Resolve every finding in the
   root spec. Check with a command each claim either reviewer marks unverified: fix it when it is
   false, or take it out.
4. Request approval only once the human has agreed to every point in the spec, because the human
   approves what they already agreed to and a point new in the request has no decision block for
   them to answer: give such a point its own decision block first, or take it out of the spec.

   ```bash
   dispatch request-approval --issue <root issue> --include-verification --summary '<what the human is approving> Reviews: <gap result>, <claim result>; <what they do not show>.'
   ```

   `--include-verification` asks for the spec and the Verification document in one request, each
   at its own current settled version, and the human's answer records a review on each: that one
   request covers the `criteria` and the `spec` step together. An issue with no `verification.md`
   is refused `NO_VERIFICATION_DOCUMENT`, and one whose Verification document has no settled
   version `NO_VERSION`.

   The `decisions` step is a request of its own, since `--include-verification` is for the issue's
   spec alone and any other document is refused `NOT_SPEC_DOCUMENT`:

   ```bash
   dispatch request-approval --issue <root issue> --artifact decisions.md --summary '<what the human is approving> Reviews: <gap result>, <claim result>; <what they do not show>.'
   ```

   `--summary` says in one to three sentences what the human is approving and nothing else (no
   commentary and no open question), then one sentence with both reviews' results and what they do
   not show, for example `Reviews: no open gap, 14 of 14 claims verified; neither shows that the
   design works once built.` The primary document is the default; the
   command prints `Approval requested for spec.md (document id <UUID>) at version <N> (ask <id>)`.
5. Register each gated step's document and version with the daemon, one call for each step:

   ```ts
   legion({
     op: "register_gate",
     issue: "<root issue>",
     step: "<decisions|criteria|spec>", // the step this document is for
     artifactId: "<UUID>", // that step's document id, which the request printed
     version: <N>, // the version the human is asked to approve
   })
   ```

   Copy `artifactId` and `version` from what `dispatch request-approval` printed. The document id is
   never the slug or file name you passed in (`spec`, `spec.md`): the daemon recognizes the
   document's approval events by that id alone; the `legion` tool looks a slug or file name up in
   Dispatch and hands the daemon the id.

   Each step records its own document, so register the spec's id and version under `step: "spec"`
   and the Verification document's under `step: "criteria"`: the request that named both sent an
   `artifact.approved` event for each, and each step opens on its own document's alone. Leave
   `step` out only where your line names one step, which is the whole list a one-step deployment
   gates. Register no step your line does not name: a step the deployment does not gate is
   refused, and a record of it would hold nothing open.

Facts about the request:

- `dispatch request-approval` opens a system question with the fixed options `Approve` and
  `Request changes`; a human answers it from the Inbox or approves from the document's header.
  Never open a `dispatch ask` with an `Approve` option yourself: the daemon ignores its answer.
- Repeating the request while it waits on the human, with the same `--summary`, changes nothing and
  returns it ("already waits on the human"); a different `--summary` is refused then.
- A newer version of a document moves an open approval request of it to that version and leaves it
  waiting on you: the human stops seeing it in their "Waiting on you" band, and nothing tells them.
  The write itself reports it. `dispatch doc-edit` and `dispatch artifact` print the request, each
  document that stands above the version the request asks about with both versions, and the call
  that hands it back. Once the human has agreed to every point in that version, run that call to
  hand the same request back. Run it whether or not your write printed the line: a pane prints it
  only once its Dispatch client carries that report.
- If the output reads `spec.md (document id <UUID>) is already approved at version <N>`, still call
  `register_gate` with that id and version: the daemon reads the approval from Dispatch as it
  registers, opens the gate and delivers `design-approved` at once, and only once even when the
  approval's own event opened the gate first. The same read covers a human who answers between
  your two calls: a changes request on that version arrives as `design-changes-requested`, once.
  You never approve anything yourself.
- `register_gate` refused 409 `VERSION_NOT_IN_DISPATCH` means the version you named is above the
  latest version Dispatch holds for that document (the message names both), and a version Dispatch
  never wrote is no registration: the gate is as it was. Read the document's latest version, check
  it is the spec the human is asked to approve, and `register_gate` that version.
- `register_gate` refused 400 `GATE_STEP_REQUIRED` means this deployment gates more than one step
  and your call named none; the message names the steps it gates. Call again with `step` naming
  the one that document is for.
- `register_gate` refused 409 `GATE_STEP_NOT_CONFIGURED` means the `step` you named is not one
  this deployment gates; the message names the steps it does gate. Register those and no other.
- `register_gate` refused 409 `GATE_STEP_DOCUMENT_MISMATCH` means the document you named is not
  that step's document of that issue; the message names the step and the document it records.
  Register that step's own document, as the table in "Which steps are gated" says.

Each of those four refusals is given before anything is recorded, so the gate is as it was.

A `design-approved` or `design-changes-requested` notice carries a version and a reason and names
no step. Read your own issue record with the `legion` tool's `read_record` to see which step the
review was of: `gatedSteps` carries one entry for each gated step, with its `artifactId`, its
`currentVersion`, its `approvedVersion`, its `status` (`unregistered`, `waiting`, `approved`,
`changes_requested`, `open_on_basis` or `pending_on_basis`), the `changesReason` of a step whose
reviewer asked for changes, and the `basis` of a step on an ancestor's approval. That list, and not
the notice, says which steps are open and which still hold the tree's work.

Then end your turn. The registrations hand those documents to the human: when nothing else of the
tree is at work, the root moves to `awaiting_person` and waits on its design gate, keeping the
admission slot it took at its admission, as a root question handed to the human does ("Every turn" in
`skill://legion-architect/SKILL.md`). Stay live to read the human's comments and replies. Their
approval or changes request delivers your next task, and so does a newer spec version once
Dispatch shows the request back on your turn. While the root waits on a question instead, the
human's approval or changes request of the spec answers that wait too, and the task says how the
spec stands. Your resume takes no slot, whether or not the gate opened, because the root holds
one all along: an issue holds one of the cap's slots from the moment the daemon starts it until
its work is past its review — `accepting`, `retro` and `merging` hold none — or
until it is done or Dispatch takes it out of the workflow. The answered root stands in
the admission line until the pass that resumes it, and nothing is sent to you about that wait:
your record's `waitingLine` gives the live place in the line and the line's length, and beside
them the line's own fields — `part` and `kind`, `partPlace` of `partLength` inside that part,
`holdsSlot`, true for your root, which has held its slot since its start, `since` for an issue
whose work came back or `rootRank`, `ownRank` and `rootKey` for one that has not started, and
`reason`, the daemon's own sentence for that place. `why` on any issue of your tree that is
not working now says the same in one word and one sentence: `claim_stopped`, a claim nothing
will wake; `waiting`, this line; `blocked`, a Dispatch link or an open child below it;
`unlisted`, an issue the record holds in the workflow that no admission list places. Your
resume task says what answered the wait, and you act on neither your place nor your wait,
because the daemon starts you again by itself on that pass.
Release no child until every gated step is open: a delivered wake shows
`design-approved` on the root, or a task says its design gate is open, and under more than one
step `gatedSteps` in your record shows each one `approved` or `open_on_basis`. The daemon sends a
root waiting on a person no `design-approved` notice, and
"this resume is your design-approved wake" is that wake.
On `design-changes-requested`: revise the document that step records as the human's reason asks,
run step 3's two reviews on the new version, resolve every finding, request approval again and
end your turn. A
`design-changes-requested` may name a version below the latest, or arrive after another for the
same version: it is a request the human made that reached the daemon late, and no approval came
after it. Each request arrives once. The gate is as the newest review left it; still read the
reason, and answer it in the spec if the latest version does not already.

## After approval

Each step's approval is pinned to the version of the document that step records: a new version of
that document closes that step again, with no wake to say so; you know from your own edit, or
from the `artifact.version` event on your issue. One step closing holds the tree's work whatever
the other steps stand at. The exceptions are the version a human's own answer to a decision block
writes when that answer is the whole change since the version they approved, and a version whose
text is exactly that of the version they approved, with no decision block in it decided
since, such as an accidental edit undone exactly: for either, Dispatch records an `approved`
review in their name at the new version and emits `artifact.approved`, and the daemon reopens the
gate and sends `design-approved`. Undo an accidental edit of an approved root spec exactly. When
the spec then reads Approved at the new version, request nothing; when it does not (a block in
it was decided since), request approval again with steps 3 to 5.

So edit an approved root spec only when its Summary, a requirement it marks, a check of its
Verification document, the tree's scope or a settled decision changes. Such a change is a point
the human has not agreed to: put the problem behind it to them as its own decision block, with
its evidence, at the end of the section it changes, and add nothing else. The route that adds a
root's requirement to an approved ancestor is the exception: there the approval request is the
person's decision ("A root that is a view on an approved ancestor", below). If you edit the block
before the human answers, the gate closes like any new version, but their answer still carries the
approval to the version it writes. Once they have answered:

- When the answer is the whole change, request nothing: the document's chip reads Approved at the
  new version and the `design-approved` the daemon sent opens nothing new. Carry on; release no
  child because of it.
- When the answer changes the Summary, a requirement, a check or the tree's scope, rewrite that
  text and repeat steps 3 to 5 on the new version, with a one-sentence `--summary` naming what
  changed since the earlier approval, then the reviews' sentence (the card shows `Approved at
  version N on <date> by <who>.`, and its `Show diff` control carries the detail). The rewrite
  closes the gate until they approve. Release no further child until the `design-approved` after
  it; work already in flight, including what the daemon started at the answer, continues through
  its merge.

A settled decision is the human's. A plan that would overturn one goes back to the planner with the
decision kept, unless the planner brings evidence the human did not weigh that would change it
(such as a measurement showing the settled choice cannot pass a check); then a decision
block names the decision and that evidence. A plan never overturns a settled decision on its own.
A person's request after the approval becomes a requirement only when you write it into the spec
and they approve that version; it is never another issue's, and no comment or answer of theirs
takes a requirement out.

A design change that leaves the Summary, the requirements, the checks, the scope and the settled
decisions intact (a planner's measurement finding a better way to the same outcome) goes in the
plan (the issue's `plan.md` and the planner's `plan` handoff), never into the approved spec, even
where the spec's text describes the older design; the reviewer reads the plan beside the spec.

When a planner's `phase-finished` names a departure, compare it with the approved spec: with the
Summary, the requirements, the checks, the scope and every settled decision unchanged, the plan
is the record and the tree carries on; otherwise change the root spec and request approval again.

Later children and re-scoping open children toward the same requirements need no spec edit and no
new approval. A child issue's spec is never gated, and its architect writes the child's own
Verification document.

## A root that is a view on an approved ancestor

A root with a Dispatch parent can open a gated step on an ancestor's approval, with no approval of
its own. Its spec is then a view on the ancestor: it holds no requirement of its own, and each of
its checks proves a requirement of the ancestor. The children of an approved issue together do all
of what it states, and only that. So a requirement that the root needs and the ancestor does not
state goes into the ancestor's spec, never into the root's. This section is the whole rule, for the
root's architect and for the ancestor's architect.

### When the root is a view

The ancestor is the nearest issue on the root's Dispatch `child_of` chain whose document for the
step has a review or an open approval request. Read the chain upward with
`dispatch read --issue <KEY>`. Its `Links:` list names the `child_of` parent of each issue. For the
`spec` step, its `Spec approval:` line shows the review or the request. An issue farther up never
counts while a nearer issue has a review or a request.

Write the root as a view when each of these is true:

1. The ancestor carries the `legion` label.
2. The ancestor is not accepted: Dispatch shows it neither `retro` nor `done`, and its record, where
   the daemon has one, is not in `retro` or `merging`. Every other status counts, `backlog` and
   `icebox` included.
3. A person approved the latest version of the ancestor's document for the step. An approval that
   Dispatch writes in a person's name is that person's. The one other case is a version that waits
   on the person on the route of "A requirement the ancestor does not state", below.
4. The ancestor's primary document marks `requirement` blocks.
5. The newest review of the root's own document for the step, at any version, is not a changes
   request. A root with no review can be a view. So can a root whose newest review approved an
   earlier version.

In every other case, ask for the root's own approval, as "When the root asks for its own approval"
below says. `register_gate` reads each condition again. It refuses a call that fails one and
records nothing, and its code and message name what Dispatch shows.

### How to write the view

- The spec says what the root builds and how, in its Summary, Problem and Design. No document of
  the root holds a `requirement` block: not its spec, its Verification document or its decisions
  document.
- One line of the spec, under `## Requirements`, starts `Delivers:`. It names each requirement block
  of the ancestor that the root delivers, by its reference pinned to the version you register on:
  `Delivers: dispatch://<ANCESTOR>/spec#b-<id>@v<N>`.
- Where the root delivers only part of a requirement, the line says which part, for example
  `dispatch://<ANCESTOR>/spec#b-<id>@v<N>, the part in <owner>/<repository>`.
- The root's Verification document holds at least one check, and no other document of the root
  holds a check. Each check names in `serves` the ancestor's requirements that it proves, and no
  other block.
- A check that the deployment's instructions ask of every change, such as an after-merge production
  check, serves the requirement whose change it deploys.
- A requirement that the root needs and the ancestor does not state never goes into the root. Send
  it up, as "A requirement the ancestor does not state" says.
- A root that already runs with requirements of its own becomes a view at its next spec change.
  Each of its requirements that the ancestor states becomes checks that serve the ancestor's
  requirement. Each one that the ancestor does not state goes up.
- When you take work out of the spec, take its check out of the Verification document too. A check
  that serves a block the ancestor no longer holds makes the registration refuse
  (`VIEW_SERVES_OUTSIDE_BASIS`).

Every phase task that the daemon gives a view says that its requirements are the parts of the
ancestor's requirements that its `Delivers:` line names. Your own acceptance reads them the same
way, and the view's empty requirement list is no gap
(`skill://legion-architect/references/acceptance.md`).

### Registering on the ancestor's approval

These steps take the place of steps 4 and 5 of "The armed gate":

1. Run steps 1 to 3 of "The armed gate", the two fresh reviews included. Also give the fresh
   `plan-reviewer` the ancestor's approved version. Tell it to say whether the way the root
   delivers each requirement brings in anything that version does not state: a new service, a new
   credential, a new outside dependency, or a cost over a bound that the version sets. Each such
   thing is a requirement that the ancestor does not state, so send it up.
2. Register each gated step's own document, with the ancestor's document for the same step as its
   basis. For the `spec` step:

   ```ts
   legion({
     op: "register_gate",
     issue: "<root issue>",
     step: "spec",
     artifactId: "<UUID>", // the root's own primary document
     version: <N>, // its latest version
     basis: "dispatch://<ANCESTOR>/spec", // the ancestor's document for the same step
     basisVersion: <M>, // the version a person approved, or the version a request of the route asks about
     reason: "<the ancestor's requirements that the checks serve, and the reviews' result>",
   })
   ```

   The reason names the ancestor's requirements that the root's checks serve, and the result of the
   two reviews. It has 1 to 1,000 characters and holds no decision block. The `legion` tool refuses
   the call while the root's own document holds an open decision block.
3. If you have an open approval request of the root's own document for that step, resolve it in
   the same turn, after the registration:
   `dispatch resolve-ask --ask <id> --kind retracted --reason '<the step is registered on the ancestor's approval>'`.
   The registration comes first, so that the resume of a root that waits on a person reads the step
   as the registration left it.

A registration on an approved version opens the step. The daemon posts a message on the root whose
first line is `Design gate opened on an ancestor's approval.` It sends you `design-approved` with
the ancestor's document and version as its basis. A root that waits on a person gets its resume
task instead. `gatedSteps` in your record shows the step `open_on_basis`, with its `basis`. Go on as
after any `design-approved`.

A registration on a version that waits on the person is a pending basis. The step stays closed and
shows `pending_on_basis`, with the request it waits on. The root keeps its slot and its phase. End
your turn. The person's approval of that version opens the step with `design-approved`. Every other
end of the wait comes as `basis-ended`.

A step that opened on a basis is registered. A relaunched architect does not register it again in
the same generation: its catch-up shows the step with its `basis`.

A condition that the deployment's instructions put on an approval request holds for a registration
on a basis too. For example, where a request waits for an owner's approval link in the spec, the
registration waits for that link too.

### When the root asks for its own approval

Request a person's approval of the root's own document, as "The armed gate" says, and register that
version with no basis, in each of these cases:

- The ancestor marks no requirement block.
- The ancestor's approved text says that the root's kind asks for its own approval.
- The ancestor's approval request is open, and its summary names no root and no requirement that a
  root sent up.
- The ancestor's newest review is a changes request, or its approval is of an older version, and no
  request of the route waits on the person.
- No person gave the ancestor's approval.
- The ancestor is `done` or accepted, or it carries no `legion` label.
- The person kept a requirement that the root needs out of the ancestor's spec.
- A changes request is the newest review of the root's own spec.
- A person asked for the root's own approval.

The root's spec then says which of these holds. When a person asks on the issue for the root's own
approval, request that approval first, then register that version with no basis. A person's request
for the root's own approval wins over the basis.

### A requirement the ancestor does not state

Send it, with why the root needs it, to the architect that owns the ancestor's issue. Find that
architect and whether its tree runs:

1. Read the ancestor's record with the `legion` tool's `read_record` (`issue` the ancestor), then
   the record of each issue above it. The owner is the nearest claim on an issue in Legion's
   workflow whose `architect.state` reads neither `failed` nor `retired`.
2. A claim that the daemon suspended when it released an idle pod reads `suspended` and still owns
   the issue. A message on its role topic wakes it. A claim that the operator suspended reads the
   same, and a message to it waits until the operator starts it again.
3. Read `treeRuns` on the record of the root that the ancestor's `tree` field names. `true` means
   the ancestor's tree runs, and `false` means it does not. An ancestor that the daemon does not
   record has no `treeRuns`, and its tree does not run. A root that a re-admission left at `todo`
   runs nothing, whatever its architect claim reads.

While the ancestor's tree runs, send the requirement on the role topic of the ancestor's
architect: `envoy_publish` to `notifications.role.<token>`, the token that `roleToken` computes for
that issue and `architect`. Three messages follow:

1. The ancestor's architect adds the requirement and tells you the block's reference and version.
2. You write the `Delivers:` line and the check of that block, pinned to that version, and tell the
   ancestor's architect.
3. The ancestor's architect requests the person's approval and tells you that the request is open.
   Then run the two fresh reviews, register on that version as a pending basis, and end your turn.

Where the ancestor's tree does not run, make each change to the ancestor's document yourself, as
"For the ancestor's architect" says, and request each approval yourself. Add the requirement block,
then write the `Delivers:` line and the check. Run the two fresh reviews of the ancestor's new
version, and request the person's approval of it with the summary that section names. Register no
gate of the ancestor: its own architect registers it at its start, where it reads your changes.
Then run the two fresh reviews of the root's spec, register on that version as a pending basis,
and end your turn.

Where the ancestor is on the backlog, tell the project's controller that the root rests on it, on
the controller topic your addressing line names. Do this whether or not you send a requirement up.

A version of the route is a version that adds, revises or restores a requirement that a root sent
up, put to the person for approval. Its request's summary names the roots and the requirement, so a
root that reads the request can tell it is one. Any other root under the ancestor whose
registration finds that version waiting on the person may register on it as a pending basis too.
While a request of the route waits on the person, a second requirement waits for that answer. It
then goes into the next version.

### For the ancestor's architect

When a root's architect sends you a requirement:

1. Add it to the ancestor's primary document as a `requirement` block. After its number, its text
   starts with the root that delivers it: `Delivered by dispatch://<ROOT>.`
2. Tell the root's architect the block's reference and the version.
3. Wait until the root's architect tells you that its `Delivers:` line and its check are written.
   The ancestor's Spec tab then shows the requirement with its check when the person reads it.
4. Run the two fresh reviews of step 3 of "The armed gate" on the new version.
5. Request the person's approval of that version:
   `dispatch request-approval --issue <ANCESTOR> --summary '<the roots, the requirement, the cost> Reviews: <gap result>, <claim result>; <what they do not show>.'`.
6. Where the ancestor is its tree's root, register its gate at that version, as step 5 of "The
   armed gate" says.
7. Tell the root's architect that the request is open.

The summary of each request of the route names each root and the requirement, and it states the
cost. Its last sentence gives the two reviews' result, as for every approval request. The roots,
the requirement and the cost are part of what the person approves. So they go in the summary,
although `dispatch request-approval --help` asks for one sentence that names what changed. State
the cost so:

- Where the ancestor is its tree's root, the new version closes its gate. Say that its tree merges
  nothing and starts no newly released child until the person approves.
- Where the ancestor is a Legion root on the backlog with no parent, say that the approval
  re-admits its tree. The new generation waits at `todo` for a slot. It takes a slot to start its
  architect, and it holds none once it waits on its open children.
- Where a person's approval re-admitted the ancestor already and it waits in the line, say that its
  tree, once it starts, waits for this approval.

Roots whose steps already rest on the ancestor stay open through the new version. "After approval"
puts each change to an approved spec to the person as a decision block. On this route, the approval
request is the person's decision instead.

More rules for the ancestor's architect:

- At your start, read the ancestor's document and any open approval request of it. Do this before
  you act on an older `basis-changes-requested` or `resting-root` notice. A root's architect may
  have changed both while your tree did not run.
- A start task can ask you to request approval of the latest version while a request of the route
  is already open on it. Leave that request alone, and register the version that it asks about.
- A new version of the ancestor's document ends each pending basis on it, whoever wrote it. When
  you hand such a version back to the person, tell each root whose pending basis it ended.
- A requirement that starts `Delivered by` is that root's. The ancestor's own plan gives it no task,
  and the ancestor's own Verification document holds no check of it. The root's checks serve it,
  and your acceptance reads them (`skill://legion-architect/references/acceptance.md`).
- Where the root that a `Delivered by` requirement names leaves the workflow without delivering it,
  rewrite the requirement without that name. Request the person's approval of that version, and
  let the ancestor's own plan give the requirement a task. That version is not a version of the
  route: its summary names no root, and a sibling root that finds its request open asks for its own
  approval.
- The daemon refuses the ancestor's `sign_off` and `close_root` while a root that rests on its
  approval is in the workflow (`SIGNOFF_RESTING_ROOT_IN_WORKFLOW`). That root's own architect, or a
  person, moves it.

### A changes request on the route

After a changes request, Dispatch carries no approval to a restored text. It carries an approval
only when the newest review is an approval. So an exact undo after a changes request needs a new
approval request. A requirement that the person keeps out, or asks to revise, costs a second
approval.

On a person's changes request on a version of the route, the ancestor's architect:

1. Revises the version as the person says. Where the person keeps the requirement out, it undoes the
   edit exactly.
2. Runs the two fresh reviews, requests the person's approval of that version again, and registers
   the ancestor's gate at it where the ancestor is its tree's root.
3. Tells each root whose pending basis the changes request ended, and each root whose step opened
   on the version the person now asks to change. The `resting-root` and `basis-changes-requested`
   notices name them.
4. Also tells each root that sent the requirement. A changes request that came before a root
   registered makes its call fail (`BASIS_NOT_APPROVED`, which names the changes request), so no
   notice names that root.

Where the ancestor's tree does not run, the root's architect makes those changes and requests each
approval itself. A review after an approval reaches it as `basis-changes-requested`. When it
revises a requirement that another root delivers, it finds that root by the block's `Delivered by`
name and by the ancestor's coverage. The coverage (`GET /api/v1/issues/<ANCESTOR>/coverage`)
counts the checks of the issues under the ancestor that serve each block. It tells that root on
its role topic.

A root acts only where the person's reason touches a requirement that its `Delivers:` line names.
Every other root keeps its spec and its step as they are. While the ancestor's tree runs, the root
waits until the ancestor's architect tells it the new version and that its request is open. Then:

- A root whose pending basis the changes request ended registers again on the next version, where
  that version still states its requirement.
- A root whose step opened on the version sent back changes its work and its check to the revised
  requirement by a new version of its spec. It registers on the revised version as a pending basis.
- Where the person keeps the requirement out and the root can do without it, the root takes that
  work and its check out by a new version of its spec. It registers on the restored version as a
  pending basis.
- Where the person keeps out a requirement that the root still needs, the root follows the
  person's words and asks for its own approval.
- A root that is told of a later revision of a requirement it delivers follows it by the same steps.

Each registration on another version of the ancestor first pins the `Delivers:` line to that
version, and runs the two fresh reviews of the root's new version.

### The notices

- `design-approved` that names an ancestor's document as its basis: an ancestor's approval opened
  the step, at your registration or at the person's approval of the version you waited on.
  `gatedSteps` shows it `open_on_basis`, and no person approved your own version. Go on as after any
  `design-approved`.
- `basis-ended`: your pending basis ended without opening. The step stays closed and is registered
  on your own document with no basis. The reason names what ended it. Read the ancestor's document
  and its open request before you act.
  - A newer version of the ancestor's document: register on it again when it is a version of the
    route.
  - A changes request: act as "A changes request on the route" says.
  - A person's approval of your own document: the step opened on that approval, so go on.
  - A new version or a changes request of your own document: register again after the two fresh
    reviews, or ask for your own approval.
  - Any other end: ask for your own approval where "When the root asks for its own approval" names
    the case.
- `basis-changes-requested`: a person asked for changes on the version that a step rests on, after
  its approval, and the step stays open. A root's architect and the ancestor's architect each act
  as "A changes request on the route" says.
- `resting-root`, to the ancestor's architect: a root's step rests on the ancestor's approval or
  waits on its request, that wait ended, or a resting root closed or left the workflow. It names
  the root, its step and the requirements that its checks serve. Where a wait ended on a new version
  or a changes request, tell that root of the next version once its request is open. Where a root
  that a `Delivered by` requirement names left the workflow, rewrite the requirement as above.
  Otherwise it asks nothing of you.

A root that waits on a person gets no `design-approved` and no `basis-ended` for its step. Its
resume task names the opening or the end.

### Later changes to a root on a basis

- A new version of the root's own spec closes the step, as a new version of an approved spec does.
  Then run the two fresh reviews and register again with the basis, or request a person's approval.
- A person's changes request on the root's own document closes the step, as on any step.
- A person's approval of the root's own document opens the step on that approval.
- Nothing else that is done to the ancestor's document moves an open step. Only a person's changes
  request on the version it rests on reaches you, as `basis-changes-requested`.

## A decision block for a read no pod can make

A check sometimes needs evidence from a production or external system before a
human can approve it: a measurement, a current count, a stored record. First check whether a
credential this tree's pods carry reaches it: the model route every pod has, and any identity the
operator's deployment grants pods. Read the pod's own environment (for example `AWS_CONFIG_FILE` or
`AGENT_SECRETS_URL`), because a credential can exist without any skill naming it;
`skill://agent-secrets` says how a pod's session uses the secrets broker's client.

When the evidence is not reachable, do not have another running session read it for this tree and
fold the result into the spec, because that hides the gap from the next tree that hits it. Write
the missing capability as its own decision block to the human (which external system, which read or
write, why the spec needs it), and leave it open until the human resolves it.
