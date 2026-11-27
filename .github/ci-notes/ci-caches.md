# Notes for the cache fills

The derivations and the history behind `.github/workflows/ci-caches.yaml` and
`.github/workflows/ci-caches-changes.yaml`, the workflows that fill the Actions caches ci.yaml's
jobs restore. A `# why:` line of either file that names this file names the section here that
holds the derivation or the record its text rests on; its other `# why:` lines name sections of
`.github/ci-notes/ci.md`.

## The Go families, their readers and their runners

Seven Go families, each one a restore step of ci.yaml's run names, keyed on the runner's
architecture and on the Go version the family's own module installs (go-envoy-infra on the hash
of its module files too, for the reason its job gives): go-notices (lint's notices step),
go-daemon (typecheck, daemon-go, plugin-pack and pi-legion), go-envoy (envoy-go-build,
envoy-go-checks, listener-server-change, envoy-e2e-local and envoy-session-stall, and the embedder
job of .github/workflows/dispatch-browser.yaml), go-envoy-test (envoy-go, envoy-go-claim-model,
which builds the API's test binary of that set, the binaries, multiprocess and visual jobs of
.github/workflows/dispatch-browser.yaml, which ci.yaml's dispatch-browser and dispatch-browser-pr
jobs call: those three build the Dispatch server and the real broker's commands, whose packages
the go-envoy entry's build of the listener and natsd alone does not hold, the two jobs of
.github/workflows/ci-job-dispatch-two-builds.yaml, whose ceiling step and twobuild test step build
out of that same set, and dispatch-memory and dispatch-multiprocess, whose ceiling step and tests
read its modules and objects), go-envoy-race (every leg of envoy-go-race, envoy-go-race-api and
envoy-go-race-docs, the envoy race lane), go-envoy-darwin-vet (envoy-go-build's darwin vet, whose
build cache is a directory of its own, the GOCACHE of that step's env) and go-envoy-infra
(envoy-infra-build and envoy-infra-go, and the build job of release-envoy-infra.yaml).

The key carries `runner.arch` because Go's build cache holds the objects of the architecture that
compiled them: an entry an x86_64 runner filled gives an arm64 job its modules and none of its
objects. So each family is filled on every architecture one of its readers runs on, one matrix
leg per runner: go-notices and go-envoy-race on ubuntu-24.04-arm alone, where lint and the race
lane run; go-daemon, go-envoy, go-envoy-test and go-envoy-infra on ubuntu-24.04-arm and on
ubuntu-24.04 both, since each has a reader on each (typecheck and the envoy lanes but one on the
first; daemon-go, plugin-pack, pi-legion, envoy-go-build, envoy-go-checks, envoy-go-claim-model,
the browser lane, the two-build jobs and the release builds on the second); and
go-envoy-darwin-vet on ubuntu-24.04 alone, where envoy-go-build runs. A leg builds what that
runner's readers build and no more: go-daemon's arm64 reader is typecheck alone, so its arm64 leg
runs typecheck's vet and leaves the test and race builds to the x86_64 leg.

go-notices carries the architecture before the OS (go-notices-<arch>-<os>-go<version>) and the
other six after it: the go-notices entry holds go-licenses, a program lint's notices step runs,
and a run of a workflow file older than the architecture in the key restores by go-notices-<os>-,
which an <os>-<arch> key starts with (ci-job-lint.yaml's restore step says what that did). One Go
setup per leg, since the key carries the version that module's go.mod names: the daemon's go.mod
for go-daemon and for go-notices, which lint installs the daemon's Go for; the envoy module's for
go-envoy, go-envoy-test, go-envoy-race and go-envoy-darwin-vet; the infra module's for
go-envoy-infra.

A fill's restore asks for the readers' key, with that key and a `-` as its restore-keys. GitHub
matches the key itself as a prefix too, so an entry of another Go version is restored only when
that version's text starts with this one's (go1.27.30 for go1.27.3), and Go keys every object of
its build cache on the toolchain that compiled it, so such an entry costs only its download. No
entry carries the readers' key itself, so GitHub's matching gives a reader the newest entry whose
key starts with it: the last fill's.

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

## The envoy race lane's family

go-envoy-race holds what the envoy race lane builds: the race build of every package of
ci_job_reads.py's ENVOY_RACE_LANE (envoy-go-race's, envoy-go-race-api's and envoy-go-race-docs'),
the modules they need, and the plain build of the connection sizing program each of those jobs'
pg-connection-ceiling.sh step runs (`go run ./internal/dispatch/store/cmd/connections`). The
envoy-race job fills it on ubuntu-24.04-arm, the runner of all three jobs, on every push to main,
as the other families are filled; every leg of the three jobs restores it. Before it, no cache
held a race object of the envoy module: envoy-go-race and envoy-go-race-api restored go-envoy-test
(since #5626; nothing before it), which holds the modules and the plain objects of every package,
and envoy-go-race-docs restored go-envoy, which holds the modules and the listener's plain objects.

Why, measured: the time until the longest package starts, from the race step's start to the start
of the test binary of the package that ends the step. That binary cannot start until it is
compiled and linked, so the time holds the compile from nothing, the link, the module downloads
and the wait for a free slot of go test's -p; it is not the compile alone. In three green merge
groups of 2026-11-26 (runs 45762627143,
45762613215 and 45762607670) it was 143 to 150 s of envoy-go-race's 1,069 to 1,075 s step (13 to
14%), 108 to 113 s of envoy-go-race-api's 1,562 to 1,715 s (7%) and 50 to 51 s of
envoy-go-race-docs' 1,370 to 1,388 s (4%). In legs it is paid once per leg, on a step that is
shorter: 51 to 101 s of each of envoy-go-race's five legs of 271 to 598 s (run 45739249062), 112 to
122 s of each of envoy-go-race-api's three legs of 539 to 743 s and 52 to 57 s of each of
envoy-go-race-docs' three of 503 to 547 s (run 45735800190), 9 to 23% of each leg. On the lane's
runner, from an empty module and build cache, `go test -count=1 -race -run '^$'` over each job's
packages took 106 s (envoy-go-race, 13 test binaries), 107 s (envoy-go-race-api) and 46 s
(envoy-go-race-docs), and the same command again on the warm cache 7.3 s, 3.5 s and 1.9 s: no
compile, only the links (probe run 45779631560), so the compile is most of that time. The sizing
program took 15 s more from nothing, and the module downloads 3 to 12 s.

With the family restored (scratch run 45785768352, the fill as written, then each job whole on
its entry): the time until the longest package starts fell to 20 s in envoy-go-race (job
139794598025), 3 s in envoy-go-race-api (job 139794598088) and 1 s in envoy-go-race-docs (job
139794597972), and the ceiling step to 3 s from 9 to 22 s, for a restore of 10 to 11 s. The race
steps took 937, 1,605 and 1,341 s, within the spread of the tests' own times. A restored entry
with internal/oidc's product code changed compiled exactly internal/oidc and its test main, and
the test binary ran the changed code (job 139794597881).

The entry's size. The race build of the whole lane leaves 1.52 GB of build cache and 1.03 GB of
module cache on disk, 327 MB and, without the module zips the fill deletes, 129 MB compressed
(probe run 45779631560, job 139771656168): about 0.46 GB saved, one entry, kept alone by
refill-superseded.
The repository held 18.6 GB in 369 entries on 2026-11-26 at 08:20Z, above the 10 GB GitHub's
default allows, and the limit set for it is not readable with a workflow's token
(actions/cache/storage-limit answers 403). Over its limit GitHub evicts the least recently used
entries first; on that day those were BuildKit blobs last read on 2026-11-25 at 21:10Z, not an
entry of a Go family, each of which is read on every run that restores it.

Why it is safe to restore an older entry. Go keys every object of its build cache on its inputs:
the source of the package and of everything it imports, the flags, the toolchain. An entry filled
on an older commit gives a reader the objects of every package that did not change since and
nothing else; a package that changed, or that imports one that did, compiles again, and the test
binary links from what is current. So a reader never links an object of another closure, whatever
entry it restored, and the per-push refill and refill-superseded keep the entry the last fill's:
what was wrong with the entries of 2026-11-18 was that nothing refilled them, not that they could
be read. After a Go version change the fill finds no entry of the new Go (its restore-keys carry
the version) and compiles everything; the readers' restore-keys carry no version, so they restore
the old Go's entry, whose modules they use and whose objects the new toolchain does not, and they
compile everything too, as every run did before this family.

forced-order.sh's builds (envoy-go-race's last two steps) compile a hooked copy of nats-server
under the odatrace tag; this entry gives them the race standard library and the modules below
nats-server, and they compile the hooked copy and what imports it as before.

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
One script ran `go mod download` on a module they hold: packages/envoy/scripts/forced-order.sh,
in daemon-go's leg 2, which restores go-daemon, and in envoy-go-race's two forced-order steps,
which restore go-envoy-race. The script took nats-server's tree from
`go mod download -json`, which fetches the zip whenever the cache holds none
(cmd/go/internal/modcmd/download.go calls DownloadZip), so on an entry without zips it would have
fetched that zip on every run. The script now reads the tree's directory from `go list -m -json`,
which fetches nothing, and keeps `go mod download` for a version no cache holds (its -server
2.10.29 run). So every fill that saves a module cache deletes its zips before the save: the end
command of .github/scripts/go-cache-working-set.sh does it for go-daemon, go-notices, go-envoy,
go-envoy-test and go-envoy-race, and its zips command alone for go-envoy-infra, which builds from nothing and runs
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

The entries of go-daemon, go-notices, go-envoy, go-envoy-test, go-envoy-race and
go-envoy-darwin-vet that the fills replaced, deleted once each fill has saved its new one. Each
fill saves a new entry, over a
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
those of envoy-infra-superseded, dispatch-builds-superseded and ci-caches-changes.yaml's
superseded job, run .github/scripts/cache-superseded.sh, which took the three step bodies out of
ci-caches.yaml when that file came near its 64,000 bytes (a file at that size moves its bodies to
.github/scripts). A cache key is text any build that saves on this ref chose, so the
script never splits or prints one raw: the list stays JSON, jq selects the numeric ids to delete,
and each id is checked to be a number before `gh cache delete` gets it. One leg per family and
architecture, each keeping the key that fill's leg saved, under the output name the fill gives it;
a leg whose fill saved nothing (it failed before its save) skips its steps, and the job runs when
any fill failed, so one failed fill does not stop the others' deletes. Every leg runs on
ubuntu-24.04-arm: gh and jq need nothing of an x64 machine.

## The two entries the changes job restores

.github/workflows/ci-caches-changes.yaml fills, on every push to main and on ubuntu-24.04-arm, the
runner of ci.yaml's changes job, the two entries that job restores. Rule 11 of check-ci-filters.sh
reads its saves as it reads ci-caches.yaml's (CACHE_WORKFLOWS in ci_filter_coverage.py), and holds
each save's key and path to ci.yaml's restore.

It is a workflow of its own, one run per commit in a concurrency group of its own, because
ci-caches.yaml runs one at a time and a newer push replaces its waiting run. There an entry keyed
on one commit would land only after the whole run of the push before it, a median of 7.4 min (p90
13.9) over main's 100 runs from 2026-11-25 12:00 UTC, and never for a replaced run, 12 of the 71
push runs of that window. Replayed over the 147 pull-request changes jobs of 2026-11-26 03:31-05:30
UTC, with an entry landing a fixed time after its commit's push, the job's base commit had its
entry when the job started in 135 of 147 runs, and in 79 of the 85 that generate a base tree, at 43
s, and in 133 and 78 at 97 s. Those are the two fills of demo run 45781883565: attempt 1 (job
139779073246) saved the entry 42.6 s after its run was created, its runner picked up in 4 s; attempt
2 (job 139786420141) saved it 39 s after its runner started, which it waited 58 s for. The replay
takes the delay from the push as a whole and models no pickup apart from it. 12 of the misses are
bases no push to main made, such as a stacked branch's. With the fill queued behind
ci-caches.yaml's runs instead, the same replay gives 131 and 78 at 43 s, and 130 and 77 at 97 s.

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
changes-git is saved under the key form of the refilled families, the readers' key and the commit,
though its fill restores nothing to build on. A restore that misses, or an entry the checkout
cannot use, leaves the checkout to fetch everything, as it did before.

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

## Which changes-git entry stays

ci-caches-changes.yaml's superseded job runs cache-superseded.sh's commit mode, as
refill-superseded does: it deletes this ref's changes-git entries created before the one its fill
saved, and none created after it. That workflow's runs of two commits can overlap, and a later
commit's fill can save first; then the earlier commit's job, which saved last, deletes the later
commit's entry. So the entry that stays is the one saved last, whichever commit it is. That is the
one the changes job's restore picks, as it takes the newest entry by prefix, and the checkout fetches
what that entry lacks, so a run that stays on an earlier commit's objects costs it only a larger
fetch. ci-caches.yaml's fills run one at a time, so there no entry is newer than the kept one and
the same mode deletes every other.
