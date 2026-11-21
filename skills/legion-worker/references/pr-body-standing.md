# When a result stands

Read it when you judge whether a result stands: an `after_merge` or `default_branch` result, a result `dispatch results` reads as at none of the compared commits, or a child's result against a root's check. Every path it cites is in cybermonklabs/legion.

## A result proves the head it was recorded at

- A `before_merge` result stands as `skill://legion-worker/references/pr-body.md#what-a-result-is` says. An `after_merge` result proves a commit the branch its own issue's pull request merged into carries, or the repository's default branch does — that default branch alone where Dispatch knows of no merge of that issue — and a `default_branch` result only a commit the repository's default branch carries.
- Where a result's commit stands is read, not stored. A `before_merge` result is compared only with the heads of the pull requests Dispatch holds for the check's issue, open or merged (those attributed to it, and those its external links name in another repository), with no fallback to the default branch.
- Dispatch attributes a pull request to the first issue its five ranked sources name that resolves to the pull request's repository (`packages/envoy/internal/dispatch/delivery/attribution.go`): the issue whose external link is the pull request, the issue linked to a GitHub issue its body cites, a key in its head branch, a key in its title or body, and a key in its commit messages.
- So `dispatch results` reads a result's commit as none of the compared commits when it is an earlier head, or when no attributed head is its commit, as when the pull request is attributed to another issue. It says the check's words moved when any field of the check changed after the result was recorded. In either case a pass does not stand.
- Under each check, `dispatch results --issue <issue>` says whether a pass stands and from which roles. That answer leaves out a pass at another commit, a pass recorded under an older wording of the check and a retracted pass, each of which the command still prints; a fail recorded after a pass leaves that pass standing until someone retracts it.

## An issue is proved by its own checks

Approval is gated by the `before_merge` checks of its own Verification document, read at its own head, so a result a child recorded proves nothing for a root — it was recorded at the child's head — and a child never records the result that proves a root's check. A child carrying part of a root's requirement proves it with a check on its own Verification document whose `serves` names that requirement. A root's own checks are proven at its own head, or, for an `after_merge` check, on the branch the root's pull request merged into or on the repository's default branch, in its last round.
