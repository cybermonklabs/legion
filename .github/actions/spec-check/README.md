# Spec-file check

This composite action checks the Dispatch spec files of a repository by the rules of the package `specgraph`.
A repository that calls the action runs it in a workflow job, after a checkout of its own tree.
The action builds the `spec-check` command (`packages/envoy/cmd/spec-check`) from this repository at the commit the caller pins.
Then it runs `spec-check tree` over the spec directory of the caller's checkout.

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts .github/actions/spec-check/README.md` checks the layout, and `--write` fixes it.

## What it checks

The command takes every rule from the package `packages/envoy/internal/dispatch/specgraph`.

- **Which entries are spec files.**
  A spec file is a regular file whose name ends in `.yaml`, directly in the directory (`specgraph.IsFile`).
  Each other entry gives a warning (see [Warnings](#warnings)), and the command reads no text of it.

- **The size of each spec file.**
  The command refuses the tree when a spec file is larger than 1 MiB (1048576 bytes).
  In that case, the command parses no spec file.

- **The form of a file.**
  A spec file holds one or more nodes, one YAML document for each node.
  A document starts with a line that is exactly `---`, and its `type` is `requirement` (a statement), `check`, `component` (a component or, with `kind: interface`, an interface) or `term`.
  The command reads each document with the YAML library `gopkg.in/yaml.v3`.
  It checks the data of each document against one JSON Schema, `packages/envoy/internal/dispatch/specgraph/schema/spec-node.schema.json`, which gives the keys of each kind of node, their types, the keys that a node must have and the values that a key takes.
  Each fault of the form gives one line, with one of these rules:

  - `bad-yaml`: the YAML library cannot read the document, for example text that is not UTF-8, a control character or one key written two times, or the data has a key that is not a string.

  - `no-node`: the file holds no document.

  - `schema`: the data breaks the schema, with the place in the data as a JSON pointer and the message of the validator, for example `schema: node: additional properties 'surprise' not allowed`.

  - `not-printed`: the file is not exactly the documents that `specgraph`'s `Print` writes for its nodes, at the first line that differs.
    A key in another order, a comment, an anchor or an alias, a tag, a flow list, a block scalar, another indentation, other quotes, a space at the end of a line, a carriage return and a byte order mark each give this rule, so one node has one form in git.
    The line shows the text of that line in the file and the text that print writes there, each in Go's quotes, so an invisible change shows, and it names what differs, for example `not-printed: line 4 is "kind: behavior \n", and print writes "kind: behavior\n" there (a space at the end of the line); spec-check print writes each node in that form`.
    The command counts lines as the YAML library does, so a carriage return, U+0085, U+2028 and U+2029 each end a line too.
    The printed form is the one form that each YAML library reads alike, and another form can read as two texts in two libraries, so the `compare` subcommand makes no comparison of a file that is not in it, and its line names `spec-check print`.

  - `bad-time-limit`: a time limit that is not a positive Go duration.

- **How `spec-check print` writes the files.**
  `spec-check print` writes the documents in the form that the command reads back.
  The printer double-quotes each text that a YAML 1.1 or YAML 1.2 library reads, as a plain value, as another type than a string, such as `=`, `<<`, `yes`, `0x_` or `2026-11-16 10:00:00 Z`, so each standard library reads the same strings.
  On a system that gives no `flock`, `spec-check print` refuses to write, because it locks the directory with `flock` for each run, and the line says to run print on Linux or macOS.
  `spec-check print` changes all of its files or none, also when it is killed: it builds a new directory beside the spec directory, named `.<name>.spec-check-print-<number>`, and puts it in the place of the spec directory with one call.
  The new directory holds a second name of each spec file that the run does not change, or a copy of it with its mode where the system refuses that name, for example for another user's file.
  A file system that cannot exchange two directories refuses that call, and `spec-check print` then writes nothing and names the reason and the way out.
  For a spec directory that comes from the layers of a container image (overlayfs, `invalid cross-device link`), the line says to put a copy of the directory in its place in the container, `cp -a <dir> <dir>.copy && rm -rf <dir> && mv <dir>.copy <dir>`, or to mount the checkout into the container.
  For a spec directory that is itself a mount point (`device or resource busy`), the line says to mount a directory above it instead, such as the checkout.
  For a file system that does not exchange two directories (`invalid argument` on Linux, `operation not supported` on macOS), the line says to run print on a checkout on a local file system.
  `spec-check print` needs write permission on the directory above the spec directory, where it builds the new directory, and it refuses before any change when the process may not write there, with a line that says what to change, or, for a spec directory that is a mount point, to mount a directory above it.
  It gives the new directory the owner and the group of the spec directory, and where the system refuses that, the line says to run print as the owner of the spec directory, as a member of its group, or as root.

- **What a killed run of `spec-check print` leaves.**
  A run that is killed can leave the directory that it built beside the spec directory, or the old spec directory, and its record, `.<name>.spec-check-print-<number>.texts`, which `git status` shows; the next run of `spec-check print` on the spec directory removes it, or moves the entries that are no spec files back into the spec directory, and says so.
  From the old spec directory it removes a spec file only where the record holds its text, the text that the killed run read, or where it is a second name of a file of the spec directory.
  Where another program made or changed an entry there, or changed a spec file while print ran, print keeps that directory beside the spec directory under a new name, names it and the entry in its line, and exits 1, so that a person compares the entry with the spec directory; where the system refuses that name, the directory keeps its name and its record, and the next run keeps the change the same way.
  A process whose working directory was the spec directory before a run keeps the old directory, which the run removes, so it changes to the spec directory again to see the new files.
  A run of `spec-check print` changes only the nodes that it names, so it refuses a file where another node is not in its printed form, and the line names each node of that file, by its whole id, to print it whole.
  Each command reads the spec directory through one open directory, so a read while print runs reads the files as they were or as the run leaves them.
  The limits of `spec-check print`:
  a stop between the exchange and the last move leaves the entries that print does not own, such as a README, beside the spec directory until the next run puts them back;
  two long ids that differ only in their middle look alike in a line that cuts them;
  in a directory with the set-group-ID bit, a file that print writes takes the writer's group;
  an id over 131,072 bytes cannot be one argument, and a top-level id over 250 bytes makes a file name that the system refuses.

- **Every rule of `specgraph.Validate`.**
  These rules include ids (one id on two nodes, or an id that has a word that states a position), a parent or a relation to a node that the files do not hold, a loop, a leaf statement that no check serves, a statement that other statements serve and that no check serves, a text with no letter or digit, a claim or a component purpose of more than one sentence, and a name of code in a text, a term's name included.
  The rule on a name of code reads a word by its shape alone, so it also refuses a plain word or a product name of that shape: a slash between two ASCII letters (`I/O`, `and/or`, `client/server`, `https://example.com/docs`, but not `https://example.com/2024` or `https://10.0.0.1/api`), small ASCII letters and then an ASCII capital and a small letter in a word of ASCII letters and digits only (`mmHg` and `ownCloud`, but not `mmHg-based` or `véloCity`), a small word, a dot and a capital (`works.Then`), a file ending (`Three.js`, `https://bun.sh`), and the form of a Go test's name (`TestFlight`).
  Of the words of such a shape, only the product names that the rule lists, such as `Node.js`, pass.
  Say such a word in other words, for example "input and output" or "the client and the server".
  `go doc ./internal/dispatch/specgraph Validate` in `packages/envoy` lists each rule.

## What it does not check

- An id that the approved spec removed.
  `spec-check tree` checks ids against such a list only with `--retired <file>`, and the action gives no list.
  So the log of each run that judges the tree, on exit 0 and on exit 1, holds the line `spec-check: <directory>: no --retired list, so no id is checked against the ids the approved spec removed`.

- Whether the statements are the statements that a person approved.
  The action reads only the files of the caller's checkout.

- Whether the test that a check names exists, or ran and passed.

## Inputs

- `path`: the spec directory.
  The default is `.dispatch/spec`.
  The action resolves a relative path against the caller's checkout, `$GITHUB_WORKSPACE`.
  It keeps an absolute path as it is.

- `allow-empty`: `true` or `false`.
  The default is `false`.
  With `true`, the action accepts an empty directory and a directory that does not exist as the empty tree, since git keeps no empty directory.
  Any other value stops the action with exit 2 and a line that names the input.

With `allow-empty: true`, the action does not refuse a directory that does not exist.
So a mistyped `path` or a wrong working directory also exits 0 with a count of 0.
A caller sets `allow-empty: true` only while it has no spec file, and reads `count`.

## Output

- `count`: the number of statements the command read.
  The action sets it whenever the command prints its count line, on exit 0 and on exit 1.
  After exit 1 the check step has failed, so a later step that reads `count` needs `if: failure()` or `if: always()`.

## Exit statuses

The check step exits with the status of the command.
A status other than 0 fails the step and the job.
Each line that the command prints about the directory or an entry of it names the directory by its absolute path.
`<counts>` below is `<n> statements, <n> checks, <n> components, <n> interfaces, <n> terms`, each noun in the singular for one, where `<n> components` counts the components other than interfaces and `<n> interfaces` counts the components with `kind: interface`.

- **Exit 0: the tree is valid.**
  The log holds the line `spec-check: <directory>: <counts>; the tree is valid`.

- **Exit 0: the tree is empty.**
  Only with `allow-empty: true`, for a directory that does not exist or holds no spec file.
  The log holds the line `spec-check: <directory>: 0 statements, 0 checks, 0 components, 0 interfaces, 0 terms; the tree is empty`.

- **Exit 1: the tree is refused.**
  The log holds one line for each problem, `spec-check: <directory>/<file>:<line>: <node>: <rule>: <detail>`, which names the file, the line and the id of the node.
  A node id, or another value of a file, that is longer than 100 bytes shows as its first and its last 50 bytes, with `…` between them.
  Then it holds the line `spec-check: <directory>: <counts>; the tree is refused`.
  A spec file that is larger than 1 MiB gives the line `spec-check: file too large: <directory>/<file> is <n> bytes (limit 1048576)` in place of the problem lines.
  In that case, each count is 0, because the command parses no spec file.

- **Exit 2: the command did not judge the tree.**
  The directory does not exist or holds no spec file (without `allow-empty: true`), or the directory or a spec file in it cannot be read.
  The log holds a warning for each entry that is not a spec file (see [Warnings](#warnings)), then one line that names the directory and the case, and the file when a file cannot be read.
  The command prints no count line, so the action sets no `count`.

- **Exit 2: the command cannot write its output.**
  The file that holds the command's standard output does not take all of it, for example on a full disk.
  The log holds the line `spec-check: tree: cannot write standard output: <error>`.

## Warnings

An entry of the directory that is not a spec file gives a warning line, `spec-check: warning: <path>: not a spec file (<kind>); the importer passes over it`.
For a file, such as `notes.txt` or a spec file of the old `*.md` form, the line also says what a spec file is: `spec-check: warning: <path>: not a spec file (file): a spec file is a *.yaml file, which spec-check print writes; the importer passes over it`.
Other examples are a subdirectory and a symbolic link.
A warning fails nothing.

## What the runner reads from the check's lines

The runner of the job reads each line of the log for workflow commands: a line that starts with `::`, and `##[` anywhere in a line.
Each line that holds text from the directory starts with the fixed text `spec-check: `.
The command writes it with `runnerlog.Say` (`packages/envoy/internal/runnerlog`), the writer of the `architecture-check` command too.
It writes each control character, format character, line or paragraph separator and byte that is not UTF-8 as an escape, and the `[` of every `##[` as `\x5b`.
So text from the directory cannot start a new line of the log or name a workflow command.
[What the runner reads from the check's lines](../architecture-check/README.md#what-the-runner-reads-from-the-checks-lines) in the README of the architecture-note check gives the runner's code that each escape answers.

A problem matcher that an earlier step of the job added also reads every line that the check prints.
So a file name can become an annotation through that matcher.
That is one more reason to run the action in a job of its own, beside the reason that [The Go toolchain](#the-go-toolchain) gives.

## The Go toolchain

The action installs the Go version that `packages/envoy/go.mod` of this repository names, with `actions/setup-go`.
That action adds this Go and `$(go env GOPATH)/bin` to the start of `PATH` and sets `GOTOOLCHAIN=local` for the rest of the caller's job.
It also adds a problem matcher with the owner `go`, which makes an error annotation of each later line that holds `.go: `, with the rest of the line as its message.
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
The script `.github/scripts/build-envoy-command.sh` of this repository makes this build, and the build of the [architecture-note check](../architecture-check/README.md#the-go-toolchain) too, so the two actions build with the same settings.

## The cache

The action keeps the module cache and the build cache of the command under the key `spec-check-<os>-<arch>-go<version>-<hash>`.
`<hash>` is the SHA-256 of `packages/envoy/go.sum` of this repository at the commit that the caller pins.
It restores the cache on every run.
It saves the cache only on a miss, and only in a `push`, `schedule` or `workflow_dispatch` run.
A run of any other event can hold the code of another person, for example a `pull_request`, `pull_request_target`, `merge_group`, `issue_comment` or `workflow_run` run.
Go trusts the module cache and the build cache that it reads, so such a run restores the cache and never saves it.

On a miss, the restore falls back to the newest entry of the same Go version, which has the prefix `spec-check-<os>-<arch>-go<version>-`.
A fallback to an older entry is also a miss, so a `push`, `schedule` or `workflow_dispatch` run then saves the new key.

A pull request of a caller reads the cache only from a run on the default branch of the caller, or on its own base branch when that is another branch.
So a caller also runs the job on pushes to its default branch.
If it does not, each pull-request run downloads the modules of the command and builds it again.

## Runners

The action runs in a job without `container:`, on a runner such as `ubuntu-24.04`, which is what this repository tests.
In a job with `container:`, `github.action_path` and `runner.temp` are paths on the host and not paths in the container ([actions/runner#2185](https://github.com/actions/runner/issues/2185), [actions/runner#2498](https://github.com/actions/runner/issues/2498)).
The action gives both paths to `actions/setup-go` and to its cache steps, so do not use it in a job with `container:`.

## A job to copy

Pin the action by a full commit SHA of this repository, never by a branch or a tag.
Replace `<commit>` with that SHA, and replace `main` with the name of your default branch.

```yaml
name: Spec files

on:
  pull_request:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  spec-files:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          persist-credentials: false

      - id: spec-files
        uses: cybermonklabs/legion/.github/actions/spec-check@<commit>

      - name: Print the number of statements
        env:
          COUNT: ${{ steps.spec-files.outputs.count }}
        run: echo "The action read $COUNT statements."
```
