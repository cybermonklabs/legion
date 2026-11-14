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
Each line that the command prints about the directory or an entry of it names the directory by its absolute path.

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

## What the runner reads from the check's lines

The runner of the job reads each line of the log for workflow commands.
It ends a line at `\n` and at `\r`.
It reads a workflow command in two forms.
The first form is a line that starts with `::` after the runner removes the white space at the start of the line.
The second form is `##[` anywhere in a line.
Each line that holds text from the directory starts with the fixed text of the command, `architecture-check: `.
The command writes every control character in each line that it prints as an escape, such as `\n`, `\r` or `\x1b`.
It also writes the line separator U+2028 as `\u2028` and the paragraph separator U+2029 as `\u2029`.
The runner does not end a line at these two characters, because `ProcessInvoker` reads each line with `StreamReader.ReadLine` ([`src/Runner.Sdk/ProcessInvoker.cs`](https://github.com/actions/runner/blob/8cfa1049f86781d141056082d73802d56cd2b433/src/Runner.Sdk/ProcessInvoker.cs)).
But a log viewer can show a line break at them, so the command escapes them for the person who reads the log, as it escapes format characters.
So no line break from the directory gets into the log, and text from the directory cannot start a new line.
The command also writes the `[` of every `##[` as `\x5b`.
The escapes hold no `#` and no `[`, so no `##[` stays in a line.
The runner finds `##[` with a search that can ignore some characters, such as U+034F.
But it reads the command name at a fixed three characters after the start of the match.
It accepts only a name of its registered commands, and it compares the name without regard to case.
So if the match includes an ignored character, the name starts with `#`, `[` or that character, and no command that the runner defines has that name.
In `actions/runner` at commit `8cfa1049f86781d141056082d73802d56cd2b433`, `ActionCommand.TryParse` reads the name at `cmdIndex = prefixIndex + Prefix.Length` ([`src/Runner.Common/ActionCommand.cs`](https://github.com/actions/runner/blob/8cfa1049f86781d141056082d73802d56cd2b433/src/Runner.Common/ActionCommand.cs)).
The registered commands are the `_registeredCommands` of `ActionCommandManager`, a set that compares with `StringComparer.OrdinalIgnoreCase` ([`src/Runner.Worker/ActionCommandManager.cs`](https://github.com/actions/runner/blob/8cfa1049f86781d141056082d73802d56cd2b433/src/Runner.Worker/ActionCommandManager.cs)).
Only a literal `##[` can start a command.
A problem whose text has several lines prints on one line, with each line break as `\n`.

The command also writes every format character as an escape, such as a soft hyphen as `\u00ad` or a zero-width joiner as `\u200d`.
A format character is invisible or changes the order of the text, so it can make one name look like a different name in the log.
This escape is for the person who reads the log, and it does not stop a command.
The command also writes every byte that is not UTF-8 as an escape, such as `\xff`.

The Go setup of the action, `actions/setup-go`, adds a problem matcher with the owner `go`, and the runner reads every later line of the job through it.
The matcher makes an error annotation of each line that holds `.go: ` or `.go:<line>:<column>: `, with the rest of the line as its message.
So a file name such as `cmd.go: forged annotation` in the notes directory would become an error annotation with the text of its author.
The action removes that matcher, as [The Go toolchain](#the-go-toolchain) says.

A problem matcher that an earlier step of the job added also reads every line that the check prints.
So a file name can become an annotation through that matcher too.
That is one more reason to run the action in a job of its own, beside the reason that [The Go toolchain](#the-go-toolchain) gives.

## The Go toolchain

The action installs the Go version that `packages/envoy/go.mod` of this repository names, with `actions/setup-go`.
That action adds this Go to `PATH` and sets `GOTOOLCHAIN=local` for the rest of the caller's job.
When it has added the Go `bin`, it also adds `$(go env GOPATH)/bin` to `PATH`.
The runner puts each new entry at the start of `PATH`, so this Go comes before a Go that an earlier step added, and `$(go env GOPATH)/bin` (by default `~/go/bin`) comes before the Go `bin` for the rest of the job.
In `actions/setup-go` at commit `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`, `addBinToPath` calls `core.addPath` on `<GOPATH>/bin` after the install added `<installDir>/bin` ([`src/main.ts`](https://github.com/actions/setup-go/blob/b7ad1dad31e06c5925ef5d2fc7ad053ef454303e/src/main.ts)).
It also adds the problem matcher with the owner `go` that [What the runner reads from the check's lines](#what-the-runner-reads-from-the-checks-lines) describes.
The action builds only its own command and needs no Go matcher.
So it removes the `go` matcher right after it sets up Go.

The runner keeps one problem matcher for each owner.
In a job that set up Go before the action, the Go setup of the action replaces the `go` matcher of the job, and the removal then deletes it.
So the steps of that job after the action have no Go matcher, and a Go compiler error in their output gets no annotation.
Put the action in a job of its own, or before the Go setup of the job, which adds the matcher again.
The job must have no `container:`, as [Runners](#runners) says.

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
