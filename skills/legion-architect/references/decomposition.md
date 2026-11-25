# Decomposing a tree

## Reading the tree

Read your own issue (`read_record`), its criteria, its children and their handoffs:
`dispatch handoff-read --issue <child> --phase <plan|implement|test|review>`, with `--version <n>` for an earlier one.

## Adopting children

Release a child a person created at once and as it is: `legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })`.
A release refused with a code below writes nothing: release the rest again without the child it names.

- `CHILD_NOT_ADOPTABLE`: it runs or ran as a tree of its own.
  Leave it, or ask the controller.

- `CHILD_IN_OTHER_TREE`: re-parent it back in Dispatch, or ask that tree's architect or the controller.

- `ISSUE_NOT_OWNED`: the issue is another architect's; send the move to the architect above you.

- `CHILD_UNDER_ACCEPTED_PARENT`: the daemon has already accepted an ancestor's head, the head its task's last step passed.
  Release again once it has closed, unless it is the tree's root.

## Creating children

1. Write each child's spec as `skill://dispatch-writing-specs` says, scaled to the child, each requirement a `requirement` block, with its `verification.md`: its workers read only these.
   Cite no item number of another document and copy no text that does not apply to the child.
   A repeated change is one child, and deleting what this tree replaced is this tree's work.

2. File each child:

   ```bash
   dispatch issue --project <project> --parent "$LEGION_ISSUE" --title '<child outcome>' --spec-file - <<'EOF'
   <the child's spec>
   EOF
   ```

3. Review the version you filed ("Reviewing a child's spec") before `release_children`.

- `--project` is the key's prefix (`LEGSMOKE-3` → `LEGSMOKE`).

- Pass no `--assignee` unless a person names one: the default keeps the tree's questions in one Inbox.

- As a child's own architect, in the turn you file its children, send one `envoy_publish` to the agent that holds its plan: its planner where its task names one, else its implementer, at `notifications.role.<token>` (the token is below).
  The message names each child you filed, with its key and repository, and says that the plan now covers only this issue's own part and the join with those children's work.
  Send it even when you also stop the task or give a later plan: that agent keeps its session and what it read of the whole issue.

- Every child names the running system its criteria are proven on (the command, service or page its implementer proves them on and a tester step drives) and the repository skill that drives it;
  building a missing path is this tree's work.

- Where the project has architecture components, give each child a `check` for the update of `.dispatch/architecture/<id>.md` of each component its pull request changes.

- A role token is `legion-<project>-<key>-<role>` with the daemon's project and the key lower-cased: `legion-acme-legion-41-architect`.

## Reviewing a child's spec

No person reads a child's spec: review every version of it and of its Verification document, whoever wrote it, before the child's work goes on with that version.

1. Read the child's spec and its Verification document, at the versions you review, against your own spec (a root's at its approved version, a child's at the version its parent approved).
   Ask whether the version serves each requirement of your spec that it names, whether it narrows or contradicts any of them, whether it adds scope your spec does not ask for, and whether each fact it states holds in the code, the docs or a read-only command.

2. Approve or request changes, with a reason either way.

3. Post one `dispatch comment` on the child whose first line is exactly one of these, and whose second line is `Verification document v<m>`;
   then tell the child's architect with `envoy_publish`:

   ```text
   Spec review of <child key> v<n> by <parent key>: approved
   Spec review of <child key> v<n> by <parent key>: changes requested
   ```

As the child's architect: tell the architect above you of each new version, naming each document and its version;
tell your workers of it only after its approving comment; answer `changes requested` with a new version.

## Across repositories

Split a change across repositories into children of one repository each, choosing it at creation (`dispatch issue … --repo <owner/name>`).
Release a child that builds on another repository's change only after that pull request merges, never by `--blocked-by`, which starts it at the blocker's close, and name the merged commit in its task (`gh api repos/<owner>/<repo>/pulls/<n> --jq .merge_commit_sha`);
for a repository that pins the other one by commit (a lock file or dependency pin), the pin is that merged commit.
Your `gh` reads every repository your App is installed on (`-R owner/repo`).

## Recording a wait on other work

When an issue must wait on a sibling or on an issue outside this tree, set its `blocked_by` list as `skill://dispatch`, "Choosing what to work on", says.
A blocker's close is its Dispatch issue reaching `done`, not a merge or a deploy: pick a blocker whose close is the fact the issue needs.

## A root with no code of its own

When every change lands through children, the root's implementer opens its pull request on an empty commit.
Plan no code for the root, and create no child only to give it a commit.

## Architecture components

A project's components are the `.dispatch/architecture/` files Dispatch imports from its source repositories (`skill://dispatch`).

- They exist: attach the root before you decompose, so every child inherits them:
  `dispatch issue-update --issue <root> --components-json '{"mode":"explicit","ids":[…]}'`.

- They do not: the root's first pull request writes `.dispatch/architecture/<id>.md` for each planned component (front matter `title`, `parent`, `depends_on`, `external`;
  no `paths`) in the repository that holds it, with an id no other source uses.
  Leave the root on `inherit` until it merges (an earlier attach answers `400 COMPONENTS_INPUT`), then run `dispatch architecture-sync --project <KEY>` and attach.

- Give a child its own `--components-json` only for a narrower set, and `{"mode":"none","reason":"…"}` when it changes no code a component describes.
