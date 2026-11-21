# Decomposing a tree

Read this before you read a child's handoff, create or adopt children, review a child's spec,
split work across repositories, record a wait on outside work, or attach architecture components.

## Reading the tree

Read your own issue (`read_record`), its criteria, its children and their handoffs:
`dispatch handoff-read --issue <child> --phase <plan|implement|test|review>`, with `--version <n>`
for an earlier one. The child's Conversation lists every version.

## Adopting children

Release a child a person created at once and as it is:
`legion({ op: "release_children", issues: ["LEGION-41", "LEGION-42"] })`. The answer names
`released`, `started` and `held`, each held child with what it waits on. Read
`skill://legion-architect/references/decomposition-refusals.md` when `release_children`,
`park_child` or `rerun_child` is refused.

Each child the daemon starts takes an admission slot of its own, so four started children can take
four of the deployment's cap; when a child holds none,
[Admission and the `legion` label](https://cybermonklabs.github.io/legion/legion/concepts/#admission-and-the-legion-label)
says.

## Creating children

1. Write each child's spec as `skill://dispatch-writing-specs` says, scaled to the child: its
   outline (a few sentences a part for a small child), each requirement a `requirement` block, a
   proposed answer to each point, and each term a new reader may not know explained. Cite no item
   number of another document (name a requirement of the root by what it says), and copy no text
   that does not apply to the child. Write its `verification.md` with it: its workers read only
   these. A child's primary document is its spec: extend it in place, because a second "spec"
   artifact replaces the issue's own document.
2. Red-team the list with a fresh `task(agent="oracle")`, and `wait` for its verdict. Give it your
   issue's spec, the tree's open children with their repositories and pull requests, and each
   proposed child's title and whole spec, with this mandate: "You did not write this list. Argue
   that each child should not exist. Name every child that is a to-do, a fix found in review, a
   proof of the acceptance of the issue it is filed under, or a part of the architect's own work,
   and say where it folds: that issue's own pull request or an open child's. Weigh each separate
   pull request against combining its work. Size pull requests to deliver the work most
   efficiently and reliably, with no preference on their number. Each round tests a large pull
   request whole again; one failure holds all the work, and the request merges late. It can also
   conflict with the default branch. Many small pull requests each cost a workspace, an admission
   wait, review, CI, a queue wait and a deploy. They can conflict with each other, and dependent
   slices run in sequence." A repeated change is one child, and deleting what this tree replaced
   is this tree's work.
3. Post the verdict and your folds as one `dispatch comment` on your own issue whose first line is
   exactly `Red-team verdict for <your issue key>:`. The controller's weekly audit reads that line.
4. Check each survivor's spec with a fresh `task(agent="plan-gap-analyst")` against
   `skill://dispatch-writing-specs`, `wait` for it, and fix what it flags.
5. File only the survivors, and reply under the verdict with their keys:

   ```bash
   dispatch issue --project <project> --parent "$LEGION_ISSUE" --title '<child outcome>' --spec-file - <<'EOF'
   <the child's spec>
   EOF
   ```

6. Before `release_children`, review the version you filed ("Reviewing a child's spec"). Check
   that every child you filed is named under a verdict comment, because a child filed without one
   skipped the check; a child a person created needs none. Check too that the current version of
   each child you filed has your approving `Spec review of` comment.

- `--project` is the key's prefix (`LEGSMOKE-3` → `LEGSMOKE`). `--parent` is `$LEGION_ISSUE`,
  never a child of yours: that child's own architect files its children.
- Pass no `--assignee` unless a person names one: the default keeps the tree's questions in one
  Inbox.
- A child's own architect starts beside its child's planner: decide first whether the child needs
  children. If it does, file and release them, then tell the planner its plan covers only the
  child's own part and the join of their work.
- Where the project has architecture components, give each child a `check` for the update of
  `.dispatch/architecture/<id>.md` of each component its pull request changes.
- A role token is `legion-<project>-<key>-<role>` with the daemon's project and the key
  lower-cased: `legion-acme-legion-41-architect`.

## Reviewing a child's spec

No person reads a child's spec: review every version of it and of its Verification document,
whoever wrote it, before the child's work goes on with that version.

1. Have a fresh `task(agent="plan-reviewer")`, told the documents are a child's spec and its
   Verification document, read them against your own spec (a root's at its approved version, a
   child's at the version its parent approved) with this mandate: "Does this version serve each
   requirement of the parent spec that it names? Does it narrow or contradict any? Does it add
   scope the parent spec does not ask for? Check each fact it states against the code, the docs or
   a read-only command." `wait` for its result.
2. Approve or request changes, with a reason either way.
3. Post one `dispatch comment` on the child whose first line is exactly one of these, and whose
   second line is `Verification document v<m>`; then tell the child's architect with
   `envoy_publish`:

   ```text
   Spec review of <child key> v<n> by <parent key>: approved
   Spec review of <child key> v<n> by <parent key>: changes requested
   ```

As the child's architect: tell the architect above you of each new version, naming each document
and its version; tell your workers of it only after its approving comment. Until then, they keep
to the last version the architect above approved, which the newest approving `Spec review of`
comment on your issue names. On `changes requested`, write a new version that answers each reason,
and tell the architect above again. A decision only a person can make goes to the architect above
you, never into your spec.

## Across repositories

Read `skill://legion-architect/references/decomposition-outside-work.md` when a change spans
repositories, before you create its children.

## Recording a wait on other work

Read `skill://legion-architect/references/decomposition-outside-work.md` when an issue must wait on
an issue outside this tree.

## A root with no code of its own

When every change lands through children, the root's branch has no commit of its own, and GitHub
opens no pull request for it. The root's implementer then pushes one empty commit (message
`<KEY>: the root of its children; no code of its own`), writes no trailer itself, and opens the
pull request on it, as its role prompt says; that push starts the required checks like any other.
Plan no code for the root, and create no child only to give it a commit.

## Architecture components

Read `skill://legion-architect/references/decomposition-components.md` before you decompose a root
whose project has architecture components or has none yet, and before you give a child its own
`--components-json`.
