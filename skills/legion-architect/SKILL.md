---
name: legion-architect
description: "Use when you are a Legion root or child architect, before any lifecycle action: decomposing or adopting children, the design gate, an issue's plan, releasing children, a delivered wake, a worker's escalation, or accepting an issue's head."
---

# Legion architect

## Every turn

- Act on each delivered wake by its row in `skill://legion-architect/references/wake-routing.md`; build no wait loop.

- Check each claim against the code as fetched now (the default branch your addressing line names, or the head under review), and tell every reader you dispatch to do the same.

- Before you end a turn that waits on a person, make sure their question is open: a decision block, a `dispatch ask` or `dispatch request-approval`.
  A root's questions go on the root: one on a child starts no wait, and the tree stands still.
  A child's own architect opens none; it sends each question to the architect above it with `envoy_publish`.

- Message a role with `envoy_publish` to `notifications.role.<token>`, with a token from your addressing line or from `roleToken` in `@legion/contracts`: a typed token may reach nobody.

- The deployment instructions override this skill where they conflict.

- Ask a person only for what needs that person: their authority, their taste or risk appetite, or something only they can give.

- Fix a fault's cause where the cause is, and add no new gate, check or job for it.

## 1. Decompose or adopt

1. Read `STRATEGY.md` where the repository has one, and its design principles (`AGENTS.md` here).
   Find the standard solution (a web search, and how two or three well-regarded projects solve it) and cite it in the spec's `Design`.
   Ask a person only what neither settles, with a recommendation.

2. Where you doubt that a design is the right thing to build, state the problem to `task(agent="design-outsider")` and read its answer with `wait`: it asks whether the thing should exist at all, what the smallest thing that gives the person what they asked for is, and what to delete.

3. A root: write and edit the spec as `skill://legion-architect/references/design-gate.md` says, since a new version closes the gate, except three kinds it names.
   A child: as `skill://legion-architect/references/decomposition.md`, "Creating children", says.

4. Decompose, adopt, split across repositories or record a wait on outside work as `skill://legion-architect/references/decomposition.md` says.

5. Before you write or give an issue's plan, read `skill://legion-architect/references/plan.md`.
   Before you stop a task or change a plan Legion runs, read `skill://legion-architect/references/plan-changes.md`.

- Mark each requirement as a `requirement` block and write `verification.md` beside the spec:
  one `check` block per thing to prove, with `serves`, `surface` (`browser`, `cli` or `api`), `run`, `when` (`before_merge`, `after_merge` or `default_branch`) and `negative_control`.
  A check carrying `ci_check` is CI's and takes no `run` (`skill://legion-architect/references/design-gate.md#a-check-ci-proves`).

- Where this tree builds or changes a page, screen or dashboard a person opens, that person sees it and answers before you request approval (`skill://legion-architect/references/surface-round.md`).
  A child's own architect sends such a surface up to the root's architect.

## 2. Children in flight

- A root releases no child until every gated step is open:
  `design-approved`, and under more than one step `gatedSteps` in `read_record` showing each `approved`.

- Set a child's dependencies as `skill://legion-architect/references/decomposition.md`, "Recording a wait on other work", says.

- Release each child once it is filed: `legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })`.
  Never hold a release back for a close: a child released after the root's merge runs as an orphan root.
  Never block a child on its own parent, which merges last.

- On each `child-closed`, read the child's handoff, edit the specs of open children it changes.

## 3. Children complete

A parent's own first phase waits while Dispatch shows an open child under it (the `children-first` row).

## 4. Integration verification

- Prove every criterion of your issue in its own pull request, from the evidence its task's steps give (`skill://legion-architect/references/acceptance.md`, "Accepting"), never in a child; never downgrade one.
  A parent signs off only after its last round (`skill://legion-architect/references/acceptance.md`, "The last round").

- While the work is built, a role runs the application from the issue's branch and uses it as its user would:
  a passing check does not show that a person can do their job with it.

- For a page or a tool a person uses, offer a running build before the reviewer step, or the last if none. The deployment instructions say how a pod runs one.

## 5. Retro

Never message the implementer to start retro.
A retro that reports a defect holds its completion until you say whether it moves the issue back, which only retro can do while it holds the phase.
A retro that cannot write the body: `skill://legion-architect/references/acceptance.md`, "Retro's result".

## 6. Acceptance and the merge

In `accepting`, which only a task naming `acceptance:owner` enters, no worker holds the phase:
read, against the goal, every check and requirement in `skill://legion-architect/references/acceptance.md`, "Accepting".

Run `task(agent="goal-red-team")` as your role prompt says, then `wait` for its report.
Accept only when a pass stands for every `before_merge` check at the head the step before your acceptance passed, every requirement has a check (with no `before_merge` check, your comment says how each is proven), and none is unmet or read narrower than written:
post a `dispatch comment` naming each check and its result, then `legion({ op: "sign_off", issue: "<KEY>" })`.
Wait for a `ci_check` run still going; a name no run answers is yours to correct in the check, unless it names a job held back without its label, which has no pass standing until the implementer adds the label.
Finding asks, refusals and what follows: `skill://legion-architect/references/acceptance.md`.

## 7. Close

The merge closes the issue once every issue beneath it has left the workflow;
nothing after it is yours (`skill://legion-architect/references/acceptance.md`, "The close").

## Escalation judgment

- Carry a clear defect the controller gives you in this tree:
  in an open pull request, or in a child; reply under its evidence comment with the issue that carries it.
  It changes the root spec only where it changes the Summary, a requirement, a check or the scope (`skill://legion-architect/references/design-gate.md`, "After approval").

- A tool a worker cannot get is a pull request to the worker image (`skill://legion-worker/references/tools.md`).
