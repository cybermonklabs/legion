# Architecture-note check

This composite action checks the Dispatch architecture notes of a repository by the rules of the Dispatch server.
A repository that calls the action runs it in a workflow job, after a checkout of its own tree.
The action builds the `architecture-check` command (`packages/envoy/cmd/architecture-check`) from this repository at the commit the caller pins.
Then it runs the command over the notes directory of the caller's checkout.

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts .github/actions/architecture-check/README.md` checks the layout, and `--write` fixes it.

## What it checks

The command takes every rule from the package `packages/envoy/internal/dispatch/architecture/notes`.
The importer of the Dispatch server calls the same package.
So a note set that the action accepts is a note set that the server accepts, except for the checks in the next section.

- **Which entries are notes.**
  A note is a regular file whose name ends in `.md`, directly in the directory.

- **The size of each note.**
  The command refuses the set when a note is larger than 1 MiB (1048576 bytes).
  In that case, the command reads no note.

- **Every rule of `notes.Parse`.**
  These rules include the file name, the front matter and its keys, UTF-8 text, duplicate component ids, repository-relative `paths`, `parent` and `depends_on` edges that name no note, and containment cycles.

## What it does not check

- A component id that another source of the same Dispatch project already holds.
  The server reads the other sources of the project, and the action cannot.

- Whether a Dispatch source reads the caller's repository and branch.
  A Dispatch source names a repository and a branch, and the action does not read the sources.

- The limit of GitHub on a tree listing.
  The server reads the directory through GitHub, and GitHub refuses a tree listing above its own size limit.

## Inputs

- `path`: the notes directory.
  The default is `.dispatch/architecture`.
  The action resolves a relative path against the caller's checkout, `$GITHUB_WORKSPACE`.
  It keeps an absolute path as it is.

- `allow-empty`: `true` or `false`.
  The default is `false`.
  With `true`, the action accepts an empty directory and a directory that does not exist, as the server does.
  Any other value stops the action with exit 2 and a line that names the input.

With `allow-empty: true`, the action does not refuse a directory that does not exist.
So a mistyped `path` or a wrong working directory also exits 0 with a count of 0.
A caller sets `allow-empty: true` only while it has no note, and reads `count`.

## Output

- `count`: the number of notes the command judged.
  The action sets it whenever the command prints its count line, on exit 0 and on exit 1.
  After exit 1 the check step has failed, so a later step that reads `count` needs `if: failure()` or `if: always()`.

## Exit statuses

The check step exits with the status of the command.
A status other than 0 fails the step and the job.
Each line names the directory by its absolute path.
A runner reads a workflow command in the log in two forms: a line that starts with `::`, and `##[` anywhere in a line.
So the command escapes every control character, every format character (such as a soft hyphen or a zero-width joiner) and every byte that is not UTF-8 in each line that it prints, as `\n`, `\r`, `\x1b`, `\u00ad` or `\xff`.
The runner matches text in a way that can pass over a format character, so `#`, a soft hyphen and `#[` can read as `##[`.
The command also writes the `[` of every `##[` as `\x5b`.
So text from the directory can neither start a `::` line nor hold a `##[` command.
A problem whose text has several lines prints on one line, with each line break as `\n`.

- **Exit 0: the set is valid.**
  The log holds the line `architecture-check: <directory>: <n> notes, the set is valid`, with `note` for one note.

- **Exit 1: the set is refused.**
  The log holds one line for each problem, and each line names its note.
  Then it holds the line `architecture-check: <directory>: <n> notes, the set is refused`.

- **Exit 2: the command did not judge the set.**
  The directory does not exist, cannot be read, or holds no note.
  The log holds one line that names the directory and the case.
  The command prints no count line, so the action sets no `count`.

## Warnings

An entry of the directory that is not a note gives a warning line, which names the entry and its kind.
Examples are `notes.txt`, a subdirectory and a symbolic link.
A warning fails nothing, because the importer of the server also ignores such an entry and gives no message.
The warning tells the author that the server does not read the entry.

## The Go toolchain

The action installs the Go version that `packages/envoy/go.mod` of this repository names, with `actions/setup-go`.
That action puts this Go first on `PATH` and sets `GOTOOLCHAIN=local` for the rest of the caller's job.
So put the action in a job of its own, or before the Go setup of the job.
That job must have no `container:`, as [Runners](#runners) says.

The build ignores the user environment file of the go command (`GOENV=off`).
A caller that needs `GOPROXY`, `GOPRIVATE` or `GONOSUMDB` sets them as environment variables of the job.
The build also sets `GOWORK=off`, because the root `go.work` of this repository is above the action.
It sets `GOTOOLCHAIN=local` and an empty `GOFLAGS`, and it unsets `GOOS` and `GOARCH`.
So a Go flag or a target platform of the caller's job does not change the build.

## The cache

The action keeps the module cache and the build cache of the command under the key `architecture-check-<os>-<arch>-go<version>`.
It restores the cache on every run.
It saves the cache only on a miss, and only in a `push`, `schedule` or `workflow_dispatch` run.
A run of any other event can hold the code of another person, for example a `pull_request`, `pull_request_target`, `merge_group`, `issue_comment` or `workflow_run` run.
Go trusts the module cache and the build cache that it reads, so such a run restores the cache and never saves it.

A pull request of a caller reads the cache only from a run on the default branch of the caller, or on its own base branch when that is another branch.
A `push`, `schedule` or `workflow_dispatch` run on the default branch saves into the scope of that branch, and every pull request reads that scope.
A save on another branch reaches only the later runs on that branch and the pull requests whose base is that branch.
So a caller also runs the job on pushes to its default branch.
If it does not, each pull-request run downloads and builds the command again.
That is two small modules, `gopkg.in/yaml.v3` and `github.com/pmezard/go-difflib`, and a build of the Go standard library.

## Runners

The action runs in a job without `container:`, on a runner such as `ubuntu-24.04`, which is what this repository tests.
In a job with `container:`, `github.action_path` and `runner.temp` are paths on the host and not paths in the container.
Two open issues of the runner record this: [actions/runner#2185](https://github.com/actions/runner/issues/2185) and [actions/runner#2498](https://github.com/actions/runner/issues/2498).
The action gives both paths to `actions/setup-go` and to its cache steps, so do not use it in a job with `container:`.

## A job to copy

Pin the action by a full commit SHA of this repository, never by a branch or a tag.
Replace `<commit>` with that SHA, and replace `main` with the name of your default branch.

```yaml
name: Architecture notes

on:
  pull_request:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  architecture-notes:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          persist-credentials: false

      - id: architecture-notes
        uses: cybermonklabs/legion/.github/actions/architecture-check@<commit>

      - name: Print the number of notes
        env:
          COUNT: ${{ steps.architecture-notes.outputs.count }}
        run: echo "The action judged $COUNT architecture notes."
```
