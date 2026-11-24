# A root under an approved ancestor

Part of `skill://legion-architect`, section 1.
Read it before you write a root under an approved ancestor, register its gate on the ancestor's approval, or accept it.
Sending a requirement up, a route request, and the `basis-ended`, `basis-changes-requested` and `resting-root` notices: `skill://legion-architect/references/route.md`.

A root under an approved ancestor can be written as a view on it, and open its gated steps on the ancestor's approval with no approval of its own.
The ancestor is the nearest issue up the root's `child_of` chain (read with `dispatch read`) whose document for the step has a review or an open approval request; never take one farther up.
A route version is a version of the ancestor's document that adds, revises, restores or takes out a requirement a root sent up; its approval request is a route request.

## When the root is a view

Write the root as a view only when the ancestor carries the `legion` label, is not accepted, has a person's approval of its latest version (or a route version waiting on the person) and marks requirement blocks, and the root's own newest review is not a changes request.
Otherwise request a person's approval of the root's own document ("The armed gate" in `skill://legion-architect/references/design-gate.md`) and register it with no basis.
Do the same when the ancestor's text says the root's kind asks its own approval; when no person gave the ancestor's approval; when the ancestor's request is open and names no root; when its newest review is a changes request, or its approval is of an older version, with no route request waiting; or when the person kept out a requirement the root needs.
The root's spec states in its own words which of those cases holds: a pointer to decisions.md, to a list or to another passage does not count.
When a person asks for the root's own approval, request it first, then register that version with no basis: the person's request wins over a basis.

## Writing the view

- No document of a view holds a requirement block.
  One line under `## Requirements` starts `Delivers:` and names each ancestor block it delivers as `dispatch://<ANCESTOR>/spec#b-<id>@v<N>`; where it delivers only part of one, it says which part, for example the part in one repository.

- Its Verification document holds at least one check, no other document of the root holds a check, and each check's `serves` names only the ancestor's requirements it proves.
  A check the deployment asks of every change, such as an after-merge production check, serves the requirement whose change it deploys.
  When work leaves the spec, its check leaves the Verification document.

- A root that runs with requirements of its own becomes a view at its next spec change: each requirement the ancestor states becomes checks that serve the ancestor's block, and each one it does not state goes up.

- The view's Design decides how the root builds each requirement, also where it changes a Design line a person approved before: such a line is no requirement and does not go up.
  A requirement the root needs that the ancestor does not state goes into the ancestor's spec (`skill://legion-architect/references/route.md`), never into the root's.

- Where the view's work needs another child's pull request merged first, name that child in the spec.
  Stack on its branch where the deployment says to, and accept only a head with its merge; else link the root `blocked_by` it.
  Ask no person.

## Registering on the basis

A basis registration takes the place of steps 4 and 5 of "The armed gate" in `skill://legion-architect/references/design-gate.md`.
File every child before it.

1. Run steps 1 to 3 with both fresh reviews.
   Also give the `plan-reviewer` the ancestor's approved version, and send up each new service, credential, outside dependency or cost over a bound of that version that it finds the root's delivery brings in.

2. Register each gated step's own document with `basis` (the ancestor's document for that step), `basisVersion`, and a `reason` that names the ancestor's requirements the checks serve and the reviews' result:

   ```ts
   legion({ op: "register_gate", issue: "<root>", step: "spec", artifactId: "<UUID>", version: <N>,
     basis: "dispatch://<ANCESTOR>/spec", basisVersion: <M>, reason: "<requirements served; reviews>" })
   ```

3. In the same turn, after the registration, retract the root's own open approval request for that step: `dispatch resolve-ask --ask <id> --kind retracted --reason '<registered on the basis>'`.

4. After a registration on a version that waits on the person (a pending basis), end the turn.

Release the children once each gated step reads `approved` or `open_on_basis`, and act on a `design-approved` that names an ancestor's basis as on any other.
A relaunched architect does not register again, in the same generation, a step that opened on a basis.
A condition the deployment's instructions put on an approval request, such as an owner's approval link, also holds for a basis registration.
After a new version of the view's own spec, run both fresh reviews and register again with the basis, or request a person's approval.

## Accepting a view

Accept a view on the parts of the ancestor's requirements that its `Delivers:` line names at the pinned version, and never read its empty requirement list as a gap, in the `goal-red-team` read too.
Count a requirement that views deliver in parts only when the parts make the whole, and name each part in the acceptance comment with the issue and the check that proved it.
In a parent's acceptance, a view that roots a tree of its own is not a child of the tree: its own checks prove the parts its `Delivers:` line names.
