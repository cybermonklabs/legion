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

- A Go package that only a held-back job tests has no CI result on a pull request whose run's event carried no label, so before the merge queue a run outside CI, such as a Kubernetes Job, is its only Go evidence.
  Such a run takes its packages from the change's files when it starts, never from a list written earlier: a list goes stale as the branch grows, and one that names only the daemon's `internal/daemon` says nothing of the envoy packages a later push changes.
  It prints that set beside its verdict, and a verdict with no set beside it, or with an empty set, is no Go evidence for the change.
  From the repository's root, at the head the run tests and under `set -euo pipefail`, this prints the set, one directory a line, which `go test -race $set` takes:

  ```sh
  set=$(git diff --name-only --diff-filter=d "$(git merge-base origin/main HEAD)" HEAD -- 'packages/daemon/*.go' 'packages/envoy/*.go' |
    awk '!/\/testdata\// && !/^packages\/envoy\/infra\// { sub("/[^/]*$", ""); print "./" $0 }' | sort -u)
  echo "$set"
  ```

  `packages/envoy/infra` is a module outside `go.work`, so a change to it is tested from inside that directory and named the same way.
  Give a long package, such as envoy's `internal/dispatch/api`, a Job of its own, so that its run cannot take one pod past its deadline before the other packages' verdict prints; each Job prints the part of the set it ran, and the parts together name the whole set.

- A pull request labelled `priority` merges first: the `priority-conflicts` check fails any other pull request, and any merge group, that would conflict with one that is approved or in the merge queue.
  When it names one, wait for that pull request to merge, then merge `main` and resolve the conflict.

- A change worth a changelog line adds a changie fragment to each package it changes that `.github/changelog-packages` lists, and never edits a `CHANGELOG.md`:
  a release renders the fragments into its notes.

- A change to the daemon API (`packages/daemon/internal/api`, mirrored in `packages/contracts/src/legion-api.ts`) adds one contract entry and edits no contract number:
  `bash scripts/daemon-contract-entry.sh <slug>` writes `packages/daemon/internal/api/contract_<id>_<slug>.go`, whose id is the UTC second the script ran, and the change writes there the entry's text (what it does to the contract, and why) and every row it adds to a table the daemon serves from.
  Then `go test ./internal/api -update`, run in `packages/daemon`, writes the entry's fixture, `packages/contracts/fixtures/daemon-api/contracts/<id>_<slug>.json`.
  A build's contract number is 275 plus its count of entries, and an entry's number is 275 plus its place in the order in which `main` merged the entries, an order the release and worker-image builds write into `legion`.
  `legion version --contracts` prints each entry's number in the build it runs from.
  So a pull request, a document or a changelog fragment names a new entry by its id, and the contract check refuses a numbered entry added to `internal/api/version.go`.
  A merged entry and its fixture are never renamed or deleted, and a change of what an entry means, a revert among them, is a new entry.
  An entry that changes a key another entry added must follow that entry: its branch stacks on the other's branch, and where its id is the earlier of the two, the script is run again for a new id.

- Changing Bun, a CI image (copied or built here) or a CI tool follows the order in `.github/ci-images.yaml`'s header.

- After a merge of `main` that touched `bun.lock`, take one side whole, and commit what `bun install` writes only when it holds just the lines the change explains, `lockfileVersion` has not moved and no entry names a registry other than npm's (`README.md`, "Development").

- Deploys come only from main: a fix a deployment needs merges to `main` and ships in the next release, and no long-lived integration or deployment branch is kept.
