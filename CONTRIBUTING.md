# Contributing to Legion

`README.md` has the commands that build and test the repository.
The `AGENTS.md` at the root and beside most packages holds the rules for working in that part.

## Pull requests and merging

- Write the pull request's title as a conventional commit subject:
  `main` takes squash merges through GitHub's merge queue, and the title becomes the commit's subject.

- Add the label `full-ci` (every slow suite the change's paths select, which a pull request otherwise leaves to the merge queue) only before the approval of a change to `packages/envoy/internal/dispatch/store/migrations/**` or `packages/envoy/internal/pgmigrate/**`, to check a fix for a slow-suite failure that took the pull request out of the queue, while a `ci_check` of the issue names a held-back job, or when a person asks; a `lane:<output>` label runs one such suite instead.
  CI reads either label from the event that starts its run, and adding a label starts no run (`.github/workflows/ci.yaml` takes no `labeled` trigger), so:

  1. Put the label on before the push whose run must carry it.

  2. To run at a head that must not move, start a run with no commit, whose event carries the label: close the pull request, wait a minute and reopen it, or mark it a draft and then ready for review.

  3. Take the label off only after that run has started, and not while a `ci_check` of the issue names a held-back job, since every push's run must then carry it.

- Before you write that CI is green, in a pull request body or any other message, read the `not run at this head:` lines of the `ci-ok` job in the head's newest `pull_request` run of `ci.yaml`, after that run has concluded, and name each job they list: `ci-ok` passes over a job its paths selected and the pull request held back, so its pass says nothing of those jobs.
  `gh run list --workflow ci.yaml --commit <head> --event pull_request --limit 1 --json databaseId,status` names the run, `gh run view <run id> --json jobs --jq '.jobs[] | select(.name=="ci-ok") | .databaseId'` its `ci-ok` job, and `gh run view --job <job id> --log | grep 'not run at this head:'` prints the lines.

- A change worth a changelog line adds a changie fragment to each package it changes that `.github/changelog-packages` lists, and never edits a `CHANGELOG.md`:
  a release renders the fragments into its notes.

- A change to the daemon API (`packages/daemon/internal/api`, mirrored in `packages/contracts/src/legion-api.ts`) raises `DaemonAPIVersion` by the rule in the doc comment of `internal/api/version.go`.

- Changing Bun, a CI image (copied or built here) or a CI tool follows the order in `.github/ci-images.yaml`'s header.

- After a merge of `main` that touched `bun.lock`, take one side whole, and commit what `bun install` writes only when it holds just the lines the change explains, `lockfileVersion` has not moved and no entry names a registry other than npm's (`README.md`, "Development").

- Deploys come only from main: a fix a deployment needs merges to `main` and ships in the next release, and no long-lived integration or deployment branch is kept.
