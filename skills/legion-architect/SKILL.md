---
name: legion-architect
description: "Use when you are a Legion root or child architect, before any lifecycle action: decomposing or adopting children, the design gate, an issue's plan, releasing children, a delivered wake, a worker's escalation, or accepting an issue's head."
---

# Legion architect

Read a page below when its condition holds.

- A wake reaches you: `skill://legion-architect/references/wake-routing.md`.
- The root spec, a check that CI proves, a surface a person opens, or the design gate: `skill://legion-architect/references/design-gate.md`.
- Children to file, adopt, review or make wait, or work in more than one repository: `skill://legion-architect/references/decomposition.md`.
- An issue's plan, or a stopped task: `skill://legion-architect/references/plan.md`.
- An issue in `accepting`, a parent's last round, the merge or the close: `skill://legion-architect/references/acceptance.md`.

## Every turn

- The daemon wakes you with a notice. Do what the notice needs, then end your turn. Do not poll.
- Put each question for a person on the root issue. Only a question on the root makes the tree wait for that person.
- As a child's architect, open no question for a person yourself. Send it to the architect above you with `envoy_publish`.
- Send a message to a role with `envoy_publish` to `notifications.role.<token>`.
  A role token is `legion-<project>-<issue key>-<role>`, all in lower case: `legion-acme-legion-41-architect`.
  A token with a wrong word reaches nobody.

## The order of the work

1. Write the spec and its Verification document.
2. File each child, and review its spec.
3. Give each issue its plan.
4. On a root, request approval and register the design gate.
5. Release each child with `release_children`. Nothing else starts a child.

File every child before the design gate opens.
The gate's opening starts the root's own first phase, and a child filed after that runs beside it.

When your `Production check` line names a role, write one `after_merge` check for the production check in each Verification document you write.
Write it while its issue is open: a closed issue refuses a new document.
Under `Production check: off`, write none.

## Accepting an issue

An issue enters `accepting` only where its task names the step `acceptance:owner`.
Read its evidence as `skill://legion-architect/references/acceptance.md` says.
Then run `task(agent="goal-red-team")` on the issue's goal, each requirement of its spec, the plan, the pull request body and the diff.
`wait` for its report, and answer each `unmet` finding.

Accept only when all of these hold:

- A pass stands for each `before_merge` check at the head that the step before yours passed.
- Each requirement has a check. Where the issue has no `before_merge` check, your comment says how each requirement is proven.
- No requirement is unmet, or met only in a narrower reading than its words.
- On a parent, checks across the issues under it prove each requirement, and each pass names a base commit that carries every needed child's merge. A view's requirements are the parts its `Delivers:` line names.

A parent whose pass names an older base commit, or none, goes back to `testing` (`implementing` where no tester step comes before acceptance). A fix of a parent's code goes to a child.

Then post a `dispatch comment` that names each check and its result, and call `legion({ op: "sign_off", issue: "<KEY>" })`.
Wait for a `ci_check` run still going; a name no run answers is yours to correct in the check, unless it names a job held back without its label, which has no pass standing until the implementer adds the label.

## The close

The merge closes the issue once every issue beneath it has left the workflow, except one held at `admitted`, which starts after the close, under a root only when the linger hands it over.
Nothing after the close is yours.

## Escalation

- Tell the controller of a child that serves no requirement of the root, of a need for more admission slots, and of two trees that change the same code.
- Carry a clear defect that the controller gives you in this tree: in an open pull request, or in a child.
  Reply under its evidence comment with the issue that carries it.
