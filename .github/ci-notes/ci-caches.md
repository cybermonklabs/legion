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

## Why the fills delete the module zips

Go reads a module it has extracted from its tree, ~/go/pkg/mod/<module>@<version>. The zip that
tree came from, under ~/go/pkg/mod/cache/download/<module>/@v/, is read only to extract the tree
again when it is missing, and by `go mod download` and `go mod verify`: a build checks that the
tree and its .ziphash exist (cmd/go/internal/modfetch/fetch.go, DownloadDir), and the .info, .mod
and .ziphash files beside the zip stay. No job that restores these entries runs `go mod verify`.
One step ran `go mod download` on a module they hold: daemon-go's forced-order step in its leg 2,
which restores go-daemon (packages/envoy/scripts/forced-order.sh; envoy-go-race runs the same
script on a module cache it restores from no entry). The script took nats-server's tree from
`go mod download -json`, which fetches the zip whenever the cache holds none
(cmd/go/internal/modcmd/download.go calls DownloadZip), so on an entry without zips it would have
fetched that zip on every run. The script now reads the tree's directory from `go list -m -json`,
which fetches nothing, and keeps `go mod download` for a version no cache holds (its -server
2.10.29 run). So every fill that saves a module cache deletes its zips before the save: the end
command of .github/scripts/go-cache-working-set.sh does it for go-daemon, go-notices, go-envoy and
go-envoy-test, and its zips command alone for go-envoy-infra, which builds from nothing and runs
no begin. The go-envoy-darwin-vet fill runs end too, on the go-envoy entry it restores for its
module cache; its save holds only its own darwin build cache directory, so the zips it deletes
are in no entry and the delete changes nothing it saves. A job that does need a zip downloads it
again, as it downloads a module the entry never held.

go-envoy-test's were measured first, in its scratch runs above: 160 zips and 272 MB of the arm64
entry's module cache (150 and 210 MB on x64), stored as they are, since a zip does not compress
again. The saved entry went from 939 MB to 708 MB on arm64 and from 859 MB to 690 MB on x64,
though that fill also kept about 330 more build cache entries, and envoy-go's restore from 20 s to
16 s. On the entry without zips, `GOPROXY=off go list -deps -test ./...` passed, so every package
of the module and of its tests is served from the trees; with one tree removed from a copy of
that cache it failed, naming the module it had to download, and on the entry with zips the same
removal passed, since the zip extracted the tree again.

The other four families, measured in scratch run 45743937976: each fill ran as written here, and
for each family one job restored main's newest entry before any fill there saved (before) and
one restored the entry the fill saved (after), on the same runner type. Saved entries, in bytes:

| family | runner | zips deleted | before | after | restore, before / after |
|---|---|---|---|---|---|
| go-daemon | x64 | 202, 275 MB | 1,240,276,083 | 972,221,494 | 34 s / 15 s |
| go-daemon | arm64 | 202, 275 MB | 811,184,022 | 543,878,606 | |
| go-notices | arm64 | 306, 485 MB | 2,444,774,162 | 1,974,345,271 | 63 s / 61 s |
| go-envoy | x64 | 32, 65 MB | 185,401,062 | 122,633,397 | 3 s / 3 s |
| go-envoy | arm64 | 39, 77 MB | 201,671,639 | 129,748,341 | |
| go-envoy-infra | x64 | 202, 310 MB | 1,465,443,294 | 1,167,226,291 | 28 s / 26 s |
| go-envoy-infra | arm64 | 202, 310 MB | 1,892,710,447 | 1,594,620,716 | |

Each restore is one sample. go-notices' restore is mostly the extraction of its 12 GB build
cache, so its zips' 470 MB moved it little. On each entry without zips the fill's own set listed
with GOPROXY=off: `go list -deps -test ./...` with and without the e2e tag for go-daemon,
go-release-notices.sh itself for go-notices, `go list -deps` of the listener and natsd for
go-envoy, and `go list -deps -test ./...` plain, with pulumilive,natslive and for darwin/arm64 for
go-envoy-infra. That set, not `./...` of the module, since go-envoy holds only the listener's and
natsd's modules. On a hardlinked copy of each entry with one of those modules' trees removed, the
same command failed, naming the module (jobs 139656698136, 139656780003, 139655579434 and
139659218601); on each entry with zips it passed, since the zip extracted the tree again.

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

The entries of go-daemon, go-notices, go-envoy, go-envoy-test, go-envoy-darwin-vet and changes-git
that the fills replaced, deleted once each fill has saved its new one. Each fill saves a new entry, over a
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

A job of its own, so the token that may delete caches is in no job that builds code. Its legs, and
those of envoy-infra-superseded and dispatch-builds-superseded, run
.github/scripts/cache-superseded.sh, which moved their three step bodies out of the workflow when
the changes job's fill joined it (the file is held to 64,000 bytes, and a file at that size moves
its bodies to .github/scripts). A cache key is text any build that saves on this ref chose, so the
script never splits or prints one raw: the list stays JSON, jq selects the numeric ids to delete,
and each id is checked to be a number before `gh cache delete` gets it. One leg per family and
architecture, each keeping the key that fill's leg saved, under the output name the fill gives it;
a leg whose fill saved nothing (it failed before its save) skips its steps, and the job runs when
any fill failed, so one failed fill does not stop the others' deletes. Every leg runs on
ubuntu-24.04-arm: gh and jq need nothing of an x64 machine.

## The two entries the changes job restores

ci-caches.yaml's changes job fills, on every push to main and on ubuntu-24.04-arm, the runner of
ci.yaml's changes job, the two entries that job restores. Rule 11 of check-ci-filters.sh holds
each save's key and path to ci.yaml's restore, as it holds every family of the file.

changes-git-<os>-<arch>-<commit> is the `.git` of a one-commit checkout of the pushed commit. The
changes job restores it before its checkout, by the key without the commit, which GitHub matches
as a prefix and answers with the newest entry; actions/checkout finds a repository of the same URL
there and fetches only what the run's commit adds to main's objects. The checkout's fetch was 13.0
s for 91 MB (18,368 objects at 15.45 MiB/s, run 45766372051, job fetch), most of the job's 15 s
checkout, and the restore of the 96 MB entry took 0.8 s, the fetch after it 1.0 s and the checkout
1.3 s (the same run, job restore). The fill keeps the commit under refs/ci-cache/main as well:
the fetch offers the server the refs it holds as the commits it has, never a detached HEAD, and
actions/checkout removes a remote branch that conflicts with the ref it checks out, so an entry
saved from a detached checkout was fetched whole again (13 s in demo run 45781883565's first
attempt), and one with a ref of that name in 0.3 s (its second attempt, job 139787204657).
changes-git is a refilled family: its key is the readers' key and the commit, and
refill-superseded's changes-git leg deletes this ref's older entries, so one entry stays. A restore
that misses, or an entry the checkout cannot
use, leaves the checkout to fetch everything, as it did before.

ci-filters-<os>-<arch>-<commit> is the filters the pushed commit's own `check-ci-filters.sh
generate` writes, with a file naming the commit. When a change touches a file the filters are
computed with, .github/scripts/ci-filters-generate.sh generates the base tree's filters with the
base tree's own engine, about 18 s on the runner, and a pull request measured against main runs
that in more than half of its runs (85 of 149 between 03:31 and 05:30 UTC on 2026-11-26). The
changes job restores the entry of its base commit, and the generation reads it in place of
generating the base tree, but only when its commit file names that base: the same command over
the same tree on the same runner, run with the base's own Go and Bun, so the file is the one the
generation would write. A base no push to main left, such as a merge group's base that is the group
ahead of it, has no entry and is generated as before. The fill looks the exact key up first and
computes nothing when the entry is there, as the lookups of ci-caches.yaml do. The lookup's key is
the changes job's restore key word for word, its `steps.base.outputs.sha` this commit, which the
fill's step of that id names, so rule 11 holds the two to one text. An entry is about 20 KB, and
GitHub drops each after seven days unread, so they are not deleted.
