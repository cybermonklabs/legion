# Decomposing a tree

## Adopting children

Release a child that a person filed under your issue at once, as it is.
A refused release writes nothing: release the rest again without the child that the refusal names.
A child that runs as a tree of its own stays where it stands. A child that runs in another tree is that tree's architect's: ask it by a mention on its issue.

## Creating children

A child is never a to-do, a fix found in review, a proof of your own issue's acceptance or a part of your own issue's work.
A repeated change is one child.
Deleting what this tree replaced is this tree's work.

1. Write each child's spec and Verification document as `skill://dispatch-writing-specs` says, scaled to the child.
   Its workers read only these: copy no text that does not apply to the child, and cite no item number of another document.
2. Name in each child the running system its requirements are proven on (the command, the service or the page), and the repository skill that drives it.
   Building a missing path to that system is this tree's work.
3. File it with `dispatch issue --parent "$LEGION_ISSUE" ...`.
   The parent is always your own issue: a child's own architect files that child's children.
4. Review the version you filed ("Reviewing a child's spec") before you release it.

As a child's architect, in the turn you file children of your own issue, tell the agent that holds your issue's plan with `envoy_publish`: its planner where its task names one, else its implementer.
Name each child you filed, with its key and its repository.
Say that the plan now covers only your issue's own part and the join with those children's work.

## Reviewing a child's spec

No person reads a child's spec.
Review each version of a child's spec and of its Verification document, whoever wrote it, before the child's work goes on with that version.

1. Read the version against your own spec, at the version a person or your parent approved.
   Find whether it serves each requirement of yours that it names, narrows or contradicts one, or adds scope that your spec does not ask for.
   Find whether each fact it states holds in the code, the docs or a read-only command.
2. Post one `dispatch comment` on the child.
   Its first line is exactly one of the two lines below, and its second line is `Verification document v<m>`.
   Give the reason after them.
3. Tell the child's architect with `envoy_publish`.

```text
Spec review of <child key> v<n> by <parent key>: approved
Spec review of <child key> v<n> by <parent key>: changes requested
```

As a child's architect:

- Tell the architect above you of each new version, and name each document and its version.
- Tell your workers of a version only after its approving comment.
- Answer `changes requested` with a new version.
- Send a decision that only a person can make to the architect above you. Never put it in your spec.

## Recording a wait on other work

Make an issue wait on a sibling or on an issue outside this tree with its `blocked_by` list, never by holding back its release.
A blocker's close is its Dispatch issue reaching `done`, not a merge or a deploy: pick a blocker whose close is the fact the issue needs.
Legion starts the issue once its blockers close.
Never make a child wait on its own parent or on an ancestor: the parent merges last, so the two wait on each other for ever.

## Across repositories

Split a change across repositories into children of one repository each (`dispatch issue --repo <owner/name>`).
A child that builds on another repository's change: release it only after that pull request merges.
Never make it wait with `blocked_by`, which starts it at the blocker's close.
Name the merged commit in its task (`gh api repos/<owner>/<repo>/pulls/<n> --jq .merge_commit_sha`).
Where its repository pins the other one by commit (a lock file or a dependency pin), the pin is that merged commit.

## A root with no code of its own

When every change lands through children, the root's implementer opens its pull request on an empty commit.
Plan no code for the root, and create no child only to give it a commit.

## Architecture components

- Give each child a check for the update of `.dispatch/architecture/<id>.md` for each component its pull request changes.
- Where the project has no components yet, the root's first pull request writes `.dispatch/architecture/<id>.md` for each planned component, in the repository that holds it.
  Its front matter holds `title`, `parent`, `depends_on` and `external`, and no `paths`.
  Give each an id that no other source uses.
  Leave the root on `inherit` until that pull request merges, then run `dispatch architecture-sync --project <KEY>` and attach the root.
