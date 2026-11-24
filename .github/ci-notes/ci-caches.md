# Notes for the cache fills

The derivations and the history behind `.github/workflows/ci-caches.yaml`, the workflow that fills
the Actions caches ci.yaml's jobs restore. Each `# why:` line of that file names the section here
that holds the derivation or the record its text rests on.

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

## Why go-envoy's x86_64 fill runs envoy-go-build's go tests

envoy-go-build runs `go test` more often than any other reader of go-envoy (253 jobs between
2026-11-23T22:14Z and 2026-11-24T02:58Z). A go test vets each package it tests with go test's own
analyzers, and for every package under that one the vet reads two entries from the build cache:
the list of source files cmd/go writes beside each compile (`srcfiles`; `cacheSrcFiles` and
`loadCachedVet` in cmd/go/internal/work/exec.go of go1.27.3) and the facts of a vet of that package
with the same analyzers. A build reads no `srcfiles` list for a package it finds compiled, so the
prune deletes the lists of every package that did not change. No go vet can write the facts a go
test reads: cmd/go puts the vet flags in the vet action's key (`vet` in exec.go), a go vet always
passes `-json` to the vet tool (cmd/go/internal/vet/vet.go) and a go test never does, and for the
standard library go test adds `-unreachable=false`. So the x86_64 leg runs the go test commands
of envoy-go-build's steps themselves, with `-run '^$'`: each test package is compiled, linked and
vetted, and no test runs. `go test -c` is not the same: it builds the test main package with debug
information, which is part of that entry's key, so the reader would compile it again.

A plain `go vet` of the listener and natsd is no cheaper. It vets each package's test variant
(`TestPackagesFor` in vet.go), so it compiles the listener's test closure: 1,713 compile entries
(438 MB), and the module cache grows from 244 MB to 1,041 MB. That is why a fill with that vet
saved 475 MB on x86_64 against 202 MB without it (runs 45308079023 and 45283928874).

Measured in cluster Jobs legion-ci-gocache-readers-xgctc and legion-ci-gocache-readers-vxlvt
(dispatch://INFRA-1075; amd64, 4 CPUs, go1.27.3, cgo on). Each Job pruned with
go-cache-working-set.sh as this workflow runs it, then ran envoy-go-build's four go test steps.
The entries the reader wrote:

| what the fill runs after its build | build and module cache, zstd tar | entries the reader wrote | cgo runs | reader wall |
| --- | --- | --- | --- | --- |
| nothing | 142 MB | 3,908 (291 MB) | 12 | 53.9 s |
| these go test commands | 200 MB | 330 (5 MB), every one from the tests' own go commands | 0 | 27.6 s |
| `go vet` with go test's 12 analyzers | 175 MB | 2,810 (167 MB); all 606 vets again | 6 | 31.5 s |
| `go vet` of the listener and natsd | 381 MB | 3,008 (210 MB) | 6 | 50.0 s |

On a cache that held everything, the same reader took 26.5 s. The arm64 leg runs none of this:
envoy-go-build runs on ubuntu-24.04.

## Why the envoy job makes its module cache writable

Go leaves each module directory of its module cache read-only, and tar can replace no file in a
read-only directory: the go-envoy-test restore unpacks into the module cache the go-envoy restore
filled, and fails with "Cannot open: File exists" at every module the two entries share (GNU tar
1.35, a read-only directory unpacked over twice). Made writable, each such file is replaced, and
the restore gives the directories their read-only mode back.

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
