# Work outside this tree

Part of `skill://legion-architect/references/decomposition.md`. Read this when a change spans
repositories, or when an issue must wait on an issue outside this tree.

## Across repositories

Split a change across repositories into children of one repository each, choosing it at creation
(`dispatch issue … --repo <owner/name>`), and size their pull requests by the reasons of the
red-team mandate in `skill://legion-architect/references/decomposition.md`, "Creating children".
Release a child that builds on another repository's change only after that pull request merges,
never by `--blocked-by`, which starts it at the blocker's close, and name the merged commit in its
task (`gh api repos/<owner>/<repo>/pulls/<n> --jq .merge_commit_sha`); for a repository that pins
the other one by commit (a lock file or dependency pin), the pin is that merged commit. Your `gh`
reads every repository your App is installed on (`-R owner/repo`).

## Recording a wait on other work

Set an issue's `blocked_by` list when it must wait on an issue outside this tree
(`dispatch issue-update --issue <key> --blocked-by <a> --blocked-by <b>`, which replaces the whole
list; a credential scoped to one project cannot link another project's issue: `PROJECT_SCOPE`). A
blocker's close is its Dispatch issue reaching `done`, not a merge or a deploy: pick a blocker
whose close is the fact the issue needs, and where the fact arrives later than the close, set the
list again at that point rather than letting the issue start early. Legion starts the issue once
its blockers close.

An issue the daemon has already started returns its admission slot while a blocker of it is not
`done`, so the cap goes to work that can move, except for three roots that keep it: one in
`awaiting_person`, one at `admitted` whose design gate has never opened, and one that arrived from
another tree carrying the row it held there. Nothing of the waiting issue stops: its workers stay
live, its phase does not move, and its Dispatch status is not written. It takes a slot again by
itself; you do nothing for it.
