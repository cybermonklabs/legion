# Contributing to Legion

`README.md` has the commands that build and test the repository.
The `AGENTS.md` at the root and beside most packages holds the rules for working in that part.

## Pull requests and merging

- Write the pull request's title as a conventional commit subject:
  `main` takes squash merges through GitHub's merge queue, and the title becomes the commit's subject.

- Add the label `full-ci` (every slow suite, which a pull request otherwise leaves to the merge queue) only before the approval of a change to `packages/envoy/internal/dispatch/store/migrations/**` or `packages/envoy/internal/pgmigrate/**`, to check a fix for a slow-suite failure that took the pull request out of the queue, while a `ci_check` of the issue names a held-back job, or when a person asks; a `lane:<output>` label runs one slow suite instead, and `.github/ci-notes/ci.md` says when to take either off.

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
