# Notes for the cache fills

The derivations and the history behind `.github/workflows/ci-caches.yaml`, the workflow that fills
the Actions caches ci.yaml's jobs restore. A `# why:` line of that file that names this file names
the section here that holds the derivation or the record its text rests on; its other `# why:`
lines name sections of `.github/ci-notes/ci.md`.

## Why each fill builds on the newest entry of its family

These keys used to name the Go version alone, and the fill saved only when that exact key was
missing, so each family was filled once per Go version. The go-daemon, go-envoy and go-envoy-test
entries every job restored on 2026-11-22 had been saved on 2026-11-18 between 13:03 and 13:08Z
(go-notices' on 2026-11-19), and at main's e7381b7d4c9 a job that restored them had to compile
again 312 of the daemon module's 335 packages and 573 of the envoy module's 618 (`go list -test
-deps -f {{.Stale}}`); from an entry filled five merges earlier, 135 and 321. The
refill-superseded job deletes every other entry of the family once the new one is listed, so each
family keeps one entry per runner, as it did before. A job that is downloading a deleted entry at
that moment gets a cache miss: actions/cache/restore warns and the job goes on, compiling what the
entry would have given it, so it is slower, not failed.

## Why each fill keeps only what its own build used

Each fill keeps in the build cache only what its own build used: a step before the build marks
every restored entry unused, the build marks what it reads or writes, and a step after it deletes
the rest (.github/scripts/go-cache-working-set.sh). Without that, an entry grew with each fill: a
build adds the objects of every package that changed and nothing takes the replaced ones out, since
Go's own trim deletes only what no build has used for five days and every fill was within that.
go-envoy-test grew from 690 MB on 2026-11-22 to 2,039 MB by 19:18Z on 2026-11-23, and its restore
in dispatch-browser's binaries job from 15.5 s to 59 s at the median. The module cache keeps every
module version it has downloaded (Go removes none by itself), so it grows only with a change to a
module file, until a Go bump starts the family again: after a bump the fill finds no entry of the
new Go (unless an older version's text starts with the new one's, as the workflow's prefix note
says) and builds from nothing.

## Why a lookup's save takes the lookup's key

The build can change a hashed file (a go command may add the checksums it fetched to go.sum or
go.work.sum), so a hash taken after the build need not be the lookup's. go-envoy-infra's save
hashed its files again, and its key drifted: in run 45088325195 the lookup asked for
...-85ddfce1... and the save wrote ...-f2bbc22a..., so every push missed the lookup and built for
seven to ten minutes a leg.

## The architecture-check and spec-check families

The architecture-check job fills architecture-check-<os>-<arch>-go<version>-<hash>, the family of
the composite action .github/actions/architecture-check, and the spec-check job fills
spec-check-<os>-<arch>-go<version>-<hash>, the family of .github/actions/spec-check; ci.yaml's
envoy-go-build runs both actions. Neither is a lookup step of the workflow: each action restores
and saves its entry itself, keyed on the runner's architecture, on the Go version its own setup-go
installs from packages/envoy/go.mod and on the SHA-256 of packages/envoy/go.sum, so both jobs run
on ubuntu-24.04, the runner envoy-go-build runs on. On a miss it restores the newest entry of that
Go version (restore-keys). It saves only on a miss of the exact key in a push, schedule or
workflow_dispatch run: a run of any other event (pull_request, pull_request_target, merge_group,
issue_comment, workflow_run and the rest) may hold another person's code, so it restores the entry
and never saves it. The workflow's push to main is where the entry lands in the scope every later
run reads. check-ci-filters.sh's cache-family rule reads the workflow's own actions/cache steps and
not a composite action's, so it holds nothing of these two families; the "Caches" paragraph of
.github/ci-notes/ci.md#the-router-every-check-a-pull-request-or-the-merge-queue-runs is the record
of their restores and saves.

## The two entries that went with the jobs that read them

Two entries that were no Go family went with the jobs that read them - the Playwright browsers and
the .deb files of the apt list the browser suite installed - because dispatch-browser.yaml's shard
and alone jobs run in the image this repository builds, which carries the browsers and their host
packages, so neither entry has a restore left to serve (dispatch://INFRA-1443). A save whose
restore is gone is what check-ci-filters.sh refuses, so the two jobs that wrote them are gone with
them.

## What the refill-superseded job deletes, and why it is a job of its own

The entries of go-daemon, go-notices, go-envoy, go-envoy-test and go-envoy-darwin-vet that the
fills replaced, deleted once each fill has saved its new one. Each fill saves a new entry, over a
gigabyte for most families, so without this the entries would pile up: GitHub drops an entry only
after seven days unread, and over the repository's cache limit it evicts the least recently used
entries, which can be another family's live one. For each family and architecture this keeps one
entry of this ref, the one the fill just saved, and deletes every other one of that family, runner
and Go: those whose key is the readers' key followed by `-`, and the one under the readers' key
itself, which the fills saved before they named the commit and which would otherwise win every
reader's restore as an exact match. The family then holds what it held before this change, one
entry per runner, in a repository near its cache limit. A job that is downloading a deleted entry
at that moment gets a cache miss: actions/cache/restore warns and the job goes on, compiling what
the entry would have given it, slower but not failed. An entry of another runner or another Go is
no such entry and stays. None is deleted unless the new key is among this ref's entries, so a save
that did not land leaves the old entries in place.

A job of its own, so the token that may delete caches is in no job that builds code. A cache key is
text any build that saves on this ref chose, so the step never splits or prints one raw: the list
stays JSON, jq selects the numeric ids to delete, and each id is checked to be a number before
`gh cache delete` gets it. One leg per family and architecture, each keeping the key that fill's
leg saved, under the output name the fill gives it; a leg whose fill saved nothing (it failed
before its save) skips its steps, and the job runs when any fill failed, so one failed fill does
not stop the others' deletes. Every leg runs on ubuntu-24.04-arm: gh and jq need nothing of an x64
machine.
