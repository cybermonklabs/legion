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

## Why the go-envoy-test fill starts each test binary

The fill used to build each test binary with `go test -c -o /dev/null ./...`, and that left out
one compile per package that envoy-go then made on every run: the compile of the package's test
main. go test compiles the test main of a binary it runs and then deletes without DWARF, and the
test main of a binary `-c` writes with it (cmd/go/internal/test/test.go of go1.27.3 sets the test
main's OmitDebug to `!testC && !testNeedBinary()`), and the compile's action ID hashes that
choice (cmd/go/internal/work/exec.go: `omitdebug %v`), so the two are different entries of
the build cache. The fill now runs `go test -count=1 -run '^$'`, which builds and starts each
binary as envoy-go's go test does, to the last cached action, and runs no test, since `^$`
matches none. Every TestMain of the module opens a database, a NATS server or a build only when
a test asks for one (storetest, testdb, testnats and cmdtest are lazy), so each binary starts and
exits; the fill has no Postgres, so a TestMain that opened one at its start would fail the fill
and say so. The natslive build covers the five packages envoy-go's natslive step tests, not
internal/testnats alone. -count=1 keeps the fill from saving test results, as every reader passes
it too.

Measured on one tree, each fill followed by an envoy-go that restored what it saved (scratch
runs 45722872667 before and 45722872215 after; the unit step under `go test -debug-actiongraph`):
after the `-c` fill the unit step compiled 157 of its 162 test mains again, and after this fill
none (jobs 139586633736 and 139586508026). The compiles are small, 4.9 s of compiler time in all,
so the unit step's wall time moved within its noise (1,224 s, then 1,211 s). Its 162 links run
either way, 78 to 80 s of linker time, since Go caches no test binary. From an empty cache this
fill's test binaries took 212 s and 199 s (x64, arm64; jobs 139585272567 and 139585272758, the
whole jobs 4.5 and 4.2 min), against 274 s and 214 s for the `-c` fill (jobs 139585274552 and
139585275017). On main's entry this fill's jobs took 2.9 and 2.7 min, an 8 s save included (jobs
139585272770 and 139585272357).

## Why the go-envoy-test fill deletes the module zips

Go reads a module it has extracted from its tree, ~/go/pkg/mod/<module>@<version>. The zip that
tree came from, under ~/go/pkg/mod/cache/download/<module>/@v/, is read only to extract the tree
again when it is missing, and by `go mod download` and `go mod verify`, which no job that
restores this entry runs: a build checks that the tree and its .ziphash exist
(cmd/go/internal/modfetch/fetch.go, DownloadDir), and the .info, .mod and .ziphash files beside
the zip stay. So the fill deletes the zips before it saves. A job that does need a zip downloads
it again, as it downloads a module the entry never held.

Measured in the same runs: the zips were 160 files and 272 MB of the arm64 entry's module cache
(150 and 210 MB on x64), stored as they are, since a zip does not compress again. The saved entry
went from 939 MB to 708 MB on arm64 and from 859 MB to 690 MB on x64, though this fill keeps
about 330 more build cache entries (the test mains and the natslive packages), and envoy-go's
restore from 20 s to 16 s. On the entry without zips, `GOPROXY=off go list -deps -test ./...`
passed, so every package of the module and of its tests is served from the trees; with one tree
removed from a copy of that cache it failed, naming the module it had to download, and on the
entry with zips the same removal passed, since the zip extracted the tree again.

The zips are not left out by a `!` line in the save's path list. actions/cache resolves its path
list with @actions/glob and `implicitDescendants: false`
(@actions/cache's internal/cacheUtils.ts, resolvePaths), so `~/go/pkg/mod` yields that one
directory, tar archives it whole, and a `!~/go/pkg/mod/cache/download/**/*.zip` line matches no
path the glob yields and excludes nothing: a save and restore of a module cache with that line
brought back both of its 2 zips (scratch run 45722872215, job 139585272669). It still changes the
entry's version, which hashes the path list (getCacheVersion), so every restore of the family
would have to carry the same line or miss.

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
