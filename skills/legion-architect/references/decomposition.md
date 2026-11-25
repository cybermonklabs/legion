# Decomposing a tree

Read this before you act on a child or its handoff, split work across repositories, record a wait, or attach architecture components.

## Reading the tree

Read your own issue (`read_record`), its criteria, its children and their handoffs:
`dispatch handoff-read --issue <child> --phase <plan|implement|test|review>`, with `--version <n>` for an earlier one.

## Adopting children

Release a child a person created as it is, once you have reviewed its spec (no person did) and given it its plan: `legion({ op: "release_children", issues: ["LEGION-41"] })`.
The answer names `released`, `started` and `held`, each held child with what it waits on.
A release refused with a code below writes nothing: release the rest again without the child it names.

- `CHILD_NOT_ADOPTABLE`: it runs or ran as a tree of its own.
  Leave it, or ask the controller.

- `CHILD_IN_OTHER_TREE`: re-parent it back in Dispatch, or ask that tree's architect or the controller.

- `ISSUE_NOT_OWNED`: the move is another architect's, so send it to the architect above you.

- `CHILD_UNDER_ACCEPTED_PARENT`: the daemon has already accepted an ancestor's head, the head its task's last step passed.
  Only the agent of that ancestor's phase moves it back (`request_backward_move`): the retro's (you, at yours) or the merger.
  Else release again once it has closed, unless it is the tree's root.

## Creating children

1. Write each child's spec as `skill://dispatch-writing-specs` says, scaled to the child, each requirement a `requirement` block, with its `verification.md`: its workers read no other spec.
   Cite no item number of another document and copy no text that does not apply to the child.
   Weigh each separate pull request against combining its work: each round tests a large pull request whole again, and each small one costs a workspace, an admission wait, review, CI, a queue wait and a deploy.
   A repeated change is one child, and deleting what this tree replaced is this tree's work.

2. File each child:

   ```bash
   dispatch issue --project <project> --parent "$LEGION_ISSUE" --title '<child outcome>' --spec-file - <<'EOF'
   <the child's spec>
   EOF
   ```

3. Review the version you filed ("Reviewing a child's spec") before `release_children`.

- `--project` is the key's prefix (`LEGSMOKE-3` → `LEGSMOKE`).
  `--parent` is `$LEGION_ISSUE`, never a child of yours: that child's own architect files its children.

- Pass no `--assignee` unless a person names one: the default keeps the tree's questions in one Inbox.

- As a child's own architect, in the turn you file its children, send one `envoy_publish` to each agent that has run and holds its plan: each planner step's agent, else each implementer step's, else its first step's where that step is not yours, at `notifications.role.<token>` (below).
  The message names each child you filed, with its key and repository, and says that the plan now covers only this issue's own part and the join with those children's work.
  Send it even when you stop the task or give a later plan: each such agent keeps what it read of the whole issue.

- Every child names the running system its criteria are proven on, and the repository skill that drives it.
  That system is the command, service or page its implementation step proves them on and a tester step drives, where it has them.
  Building a missing path is this tree's work.

- Where the project has architecture components, give each child a `check` for the update of `.dispatch/architecture/<id>.md` of each component its pull request changes.

- Your `Production check` line says whether this project checks production after a merge.
  When it names a role, write one `after_merge` check for the production check in each child's Verification document, with its spec and while the child is open, because a closed issue refuses every new document.
  Under `Production check: off`, write none.

- A role token (`legion-acme-legion-41-architect`) comes from your addressing line or `roleToken` (`@legion/contracts`): never type one.
  A step keyed `owner` is the issue's own architect's, and `dispatch steps --issue <KEY>` gives each step run's `agent` token.

## Reviewing a child's spec

1. Read the child's spec and its Verification document, at the versions you review, against your own spec (a root's at its approved version, a child's at the version its parent approved).
   Ask whether the version serves each requirement of your spec that it names, whether it narrows or contradicts any of them, whether it adds scope your spec does not ask for, and whether each fact it states holds in the code, the docs or a read-only command.
   Use a reader (`plan-reviewer`, `plan-gap-analyst`) where the reading needs one.

2. Approve or request changes, with a reason either way.

3. Post one `dispatch comment` on the child whose first line is exactly one of these, and whose second line is `Verification document v<m>`,
   then tell the child's architect with `envoy_publish` once it runs:

   ```text
   Spec review of <child key> v<n> by <parent key>: approved
   Spec review of <child key> v<n> by <parent key>: changes requested
   ```

As the child's architect: tell the architect above you of each new version of each document.
Tell your workers of it only after its approving comment.
Answer `changes requested` with a new version.
A decision only a person can make goes to a person on your issue, never into your spec.

## Across repositories

Split a change across repositories into children of one repository each, choosing it at creation (`dispatch issue … --repo <owner/name>`).
A child that builds on another repository's change needs the merge, not an issue's close, so it takes no `--blocked-by`.
Release it only after that pull request merges, and name the merged commit in its task (`gh api repos/<owner>/<repo>/pulls/<n> --jq .merge_commit_sha`).
A repository that pins the other one by commit (a lock file or dependency pin) pins that merged commit.
Release it before an issue above it is accepted (`CHILD_UNDER_ACCEPTED_PARENT`).
If it cannot merge in time, ask a person ("Every turn") rather than release late.
Your `gh` reads every repository your App is installed on (`-R owner/repo`).

## Recording a wait on other work

When an issue must wait on a sibling or on an issue outside this tree, set its `blocked_by` list as `skill://dispatch`, "Choosing what to work on", says.
A blocker's close is its Dispatch issue reaching `done`, not a merge or a deploy: pick a blocker whose close is the fact the issue needs.
Legion starts the issue once its blockers close.

## Architecture components

A project's components are the `.dispatch/architecture/` files Dispatch imports from its source repositories (`skill://dispatch`).

- They exist: attach the root before you decompose, so every child inherits them:
  `dispatch issue-update --issue <root> --components-json '{"mode":"explicit","ids":[…]}'`.

- They do not: the root's pull request (else a child's) writes `.dispatch/architecture/<id>.md` for each planned component in the repository that holds it.
  The file has the front matter `title`, `parent`, `depends_on` and `external`, no `paths`, and an id no other source uses.
  Leave the root on `inherit` until that pull request merges (an earlier attach answers `400 COMPONENTS_INPUT`), then run `dispatch architecture-sync --project <KEY>` and attach.

- Give a child its own `--components-json` only for a narrower set, and `{"mode":"none","reason":"…"}` when it changes no code a component describes.
