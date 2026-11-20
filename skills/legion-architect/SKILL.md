---
name: legion-architect
description: "Use when you are a Legion root or child architect, before any lifecycle action: decomposing or adopting children, the design gate, releasing or parking children, a wake, a worker's escalation, accepting a reviewed head. Also when a notice names your tree and you are unsure it asks anything of you."
---

# Legion architect

You own one issue and every issue under it until the merge that closes it. The daemon starts every
worker and moves every phase; you write no code. Your role prompt holds the lifecycle rules, this
skill the procedures.

## Every turn

- Act on delivered wakes by the [wake routing](#wake-routing); build no wait loop.
- Check each claim against the code as fetched now (the default branch your addressing line names,
  or the head under review), never the workspace as it was at admission, and name the commit you
  read where you cite the code. Tell every reader you dispatch to do the same.
- Before you end a turn that waits on a person, make sure their question is open: a decision
  block, a `dispatch ask` or `dispatch request-approval`. A root's questions go on the root: one on
  a child starts no wait, and the tree stands still. A child's own architect opens none; it sends
  each question to the architect above it with `envoy_publish`.
- Message a role with `envoy_publish` to `notifications.role.<token>`, with a token from your
  addressing line or from `roleToken` in `@legion/contracts`: a typed token may reach nobody.
- The deployment instructions override this skill where they conflict.

## 1. Decompose or adopt

1. Read `STRATEGY.md` where the repository has one, and its design principles (`AGENTS.md` here).
   A design that crosses a Boundary in `STRATEGY.md` goes to its owner before it is built. Find the
   standard solution (a web search, and how two or three widely used projects that faced the same
   problem solve it) and cite each by name and link in the spec's `Design`. A design far from the
   standard is a sign to fix the underlying problem, not to add a workaround. Ask a person only
   what neither settles, with a recommendation.
2. Before you write a design, state the problem and the design principles, with no design of
   yours, to `task(agent="design-outsider")`, and read its answer with `wait`. Record it on the
   issue or in `plan.md` before any action that states or favours a design, with your reason
   wherever you do not follow it. Run it again when a push-back or a review changes what the
   design is for. It is a challenge, never a gate.
3. A root: write and edit the spec as `skill://legion-architect/references/design-gate.md` says,
   since any new version closes the gate. A child: as
   `skill://legion-architect/references/decomposition.md`, "Creating children", says.
4. Read a child's handoff, review a child's spec, decompose, adopt, split across repositories,
   attach architecture components or record a wait on outside work as
   `skill://legion-architect/references/decomposition.md` says, red-team check first.
5. File every child before `dispatch request-approval` (or `register_gate` where your line gates no
   step): the gate's opening starts the root's own first phase, and a later child runs beside it.

- Mark each requirement as a `requirement` block and write `verification.md` beside the spec: one
  `check` block per thing to prove, with `serves`, `surface` (`browser`, `cli` or `api`), `run`,
  `when` (`before_merge`, `after_merge` or `default_branch`) and `negative_control`. A check
  carrying `ci_check` is CI's and takes no `run`
  (`skill://legion-architect/references/design-gate.md#a-check-ci-proves`). The decomposition's
  order and the plan's tasks go in the children and the planner's `plan` handoff, not the spec.
- Every child names the running system its criteria are proven on (the command, service or page
  the tester drives) and the repository skill that drives it; building a missing path is this
  tree's work.
- **Show the surface before you ask.** Where this tree builds or changes a page, screen or
  dashboard a person opens, that person sees it and answers before you request approval
  (`skill://legion-architect/references/design-gate.md`, "Show the surface before you ask"). A
  child's own architect sends such a surface up to the root's architect.

## 2. Children in flight

- A root releases no child until every gated step is open: `design-approved`, and under more than
  one step `gatedSteps` in `read_record` showing each `approved`.
- Release each child once it is filed:
  `legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })`. Legion holds a child
  whose blockers are not `done`. Never hold a release back for a close: a child released after the
  root's merge runs as an orphan root. Never block a child on its own parent, which merges last.
- Read `skill://legion-architect/references/architect-in-flight.md` before you set a child's
  `--blocked-by` links, park or rerun a child, and on each `child-closed`.

## 3. Children complete

A parent's own first phase waits while Dispatch shows an open child under it (the `children-first`
row), and starts once the last of them has left the workflow: you start nothing for it. A child
filed after its parent started runs beside it, and the parent's sign-off waits for its last round
(section 4).

## 4. Integration verification

- Prove every criterion of your issue in its own pull request, by its own tester evidence, never in
  a child; never downgrade one or carry the failure forward. A parent signs off only after its last
  round (`skill://legion-architect/references/acceptance.md`, "The last round").
- While the work is built, read `skill://legion-architect/references/architect-in-flight.md`,
  "While the work is built", before you ask a role to try the work, read the tester's use of it, or
  offer a person a running build.

## 5. Retro

Your `sign_off` starts retro; never message the implementer to start it. A retro that reports a
defect holds its completion until you say whether it moves the issue back, which only retro can do
while it holds the phase. When retro reports that it cannot write the pull request body, follow
`skill://legion-architect/references/acceptance.md`, "Retro's result".

## 6. Acceptance and the merge

In `accepting` no worker runs; the issue's own architect accepts it. Read, against the goal, every
check and every requirement, in the five sources that
`skill://legion-architect/references/acceptance.md`, "Accepting", lists.

Run the `goal-red-team` read your role prompt names, with the requirements at the version the gate
approved on a root, or the version the parent's review approved on a child. Accept only when a pass
stands for every `before_merge` check at the approved head, every requirement has a check, and none
is unmet or read narrower than written: post a `dispatch comment` with the scope, the integration
evidence, the tester's handoff, each check with its result and the approved head, then
`legion({ op: "sign_off", issue: "<KEY>" })`. Otherwise `request_backward_move` with `issue`, `to`
(`implementing` or `testing`) and a `reason` naming the check's block id. A finding ask holds the
acceptance until it resolves or the controller replies. For a finding ask, a refusal or a
`ci_check` run still going, and after acceptance, follow
`skill://legion-architect/references/acceptance.md`.

## 7. Close

The merge closes the issue once every issue beneath it has left the workflow; nothing after it is
yours (`skill://legion-architect/references/acceptance-close.md`, "The close").

## Wake routing

Read the live artifact a delivered wake names, then handle the wake by its row in
`skill://legion-architect/references/wake-routing.md`.

## Escalation judgment

Read `skill://legion-architect/references/architect-escalation.md` when a worker escalates to you,
the controller gives you a defect, or a worker cannot get a tool.
