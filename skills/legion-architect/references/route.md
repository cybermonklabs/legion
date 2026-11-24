# The route to an approved ancestor

Part of `skill://legion-architect`, section 1.
Read it before a view sends a requirement up, as the ancestor's architect on a route request, and on a `basis-ended`, `basis-changes-requested` or `resting-root` notice.
The ancestor, a route version and a route request, and how a view is written and registered on the basis: `skill://legion-architect/references/view.md`.

## Sending a requirement up

Find the ancestor's owner with `read_record` on the ancestor and each issue above it: the nearest claim on an issue in the workflow whose `architect.state` is not `failed` or `retired`.
Read `treeRuns` on the record of the ancestor's tree root to learn whether the ancestor's tree runs.
While it runs, `envoy_publish` the requirement and why the root needs it to the ancestor architect's role topic, then:

1. The ancestor's architect adds the requirement as a block whose text starts `Delivered by dispatch://<ROOT>.`, keeps the how in the root's Design, and tells the root's architect the block's reference and version.
2. The root's architect writes its `Delivers:` line and check for the block as written, pinned to that version, and tells the ancestor's architect, naming missing parts or concerns without holding back the line or the registration.
3. Once told, the ancestor's architect runs both fresh reviews, requests the person's approval, registers its gate at that version where the ancestor is its tree's root, and tells the root's architect that the request is open.
4. The root's architect runs both fresh reviews, registers on that version as a pending basis, and ends its turn.

Where the ancestor's tree does not run, the root's architect writes the block, the `Delivers:` line and the check itself, reviews the ancestor's version and requests its approval, registers no ancestor gate, then registers as a pending basis.
Where the ancestor is on the backlog, tell the project's controller that the root rests on it, whether or not you send a requirement up.

While a route request waits on the person, send a second requirement or a further part for the next version; still register on the waiting version, and build that part only after registering on a version that holds it.
Any other root under the ancestor may register as a pending basis on a route version that waits on the person.

## For the ancestor's architect

- A route request's summary names each root, the requirement and the cost, whatever length `--help` asks for, and its last sentence gives both reviews' result.
  The cost says: a tree root's new version closes its gate until approval; a backlog root's approval re-admits its tree; a re-admitted root that waits in the line starts a tree that waits for this approval.
- On the route, the approval request, not a decision block, is the person's decision on an edit to the approved ancestor.
- At your start, read the ancestor's document and any open request before you act on an older `basis-changes-requested` or `resting-root` notice.
  When a start task asks you to request approval of a version a route request is already open on, leave that request alone and register the version it asks about.
- When you hand a new ancestor version back to the person, tell each root whose pending basis that version ended.
- A `Delivered by` requirement gets no task in the ancestor's plan and no check in its Verification document.
- When the root a `Delivered by` requirement names leaves undelivered, or a delivered requirement names no root, file a view child of the ancestor for it, rewrite the requirement with its name, and request approval; ask nothing first.
- On `resting-root`, where a wait ended on a new version or a changes request, tell that root of the next version once its request is open.
- On `SIGNOFF_RESTING_ROOT_IN_WORKFLOW`, do not move the resting root (its own architect or a person does), and sign off again once each one is done or out of the workflow.

## A changes request on the route

The ancestor's architect revises the version (an exact undo where the person keeps the requirement out), runs both reviews, requests approval again, and registers its gate where it is its tree's root.
It tells each root whose pending basis ended, each whose step opened on the version sent back, and each that sent the requirement: the version sent back, the new version, and that the request is open.
Where the ancestor's tree does not run, the root's architect does that itself, and finds the other roots by the `Delivered by` name, or by the checks that serve the block and `read_record`'s `tree`.

A root acts on a changes request only where the person's reason touches a requirement its `Delivers:` line names; every other root keeps its spec and its step.
While the ancestor's tree runs, a root waits for the ancestor's architect to name the new version and its open request before it acts.
Then:

- A root whose pending basis ended registers on the next version if it still states the requirement.
- One whose step opened on the version sent back changes work and check by a new spec version and registers pending on the revised one.
- Where the person keeps the requirement out, a root that can do without it takes the work and check out and registers pending on the restored version; one that still needs it asks for its own approval.

Before each registration on another ancestor version, pin the `Delivers:` line to that version and run both fresh reviews of the root's new version.
On `basis-ended`, read the ancestor's document and open request, then register again on a newer route version, follow this route, go on after the root's own approval, or ask for the root's own approval.

## The notices

- **`basis-ended`:** Your pending basis ended unopened.
  Read the ancestor's document and open request, then register again on a newer route version, follow the changes-request route, go on after the root's own approval, or ask for the root's own approval.
- **`basis-changes-requested`:** A person asked for changes on the ancestor version your step rests on.
  Act only where the reason touches a requirement your `Delivers:` line names; every other root keeps its spec and its step.
  To the ancestor's architect: revise as "A changes request on the route" above says.
- **`resting-root`:** To the ancestor's architect.
  Where a wait ended on a new version or a changes request, tell that root of the next version once its request is open.
  Where the root a `Delivered by` requirement names left undelivered, file a view child for it (above).
  Never move a resting root: its own architect or a person does.
