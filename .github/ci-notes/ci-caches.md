# Notes for the cache fills

The derivations and the history behind `.github/workflows/ci-caches.yaml` and
`.github/workflows/ci-caches-changes.yaml`, the workflows that fill the Actions caches ci.yaml's
jobs restore. A `# why:` line of either file that names this file names the section here that
holds the derivation or the record its text rests on; its other `# why:` lines name sections of
`.github/ci-notes/ci.md`.
A derivation or a history of ci-caches.yaml comes here, and the workflow keeps a line or two beside
the value or the step it is about, with a `# why:` line that names the section. Its longer comment
blocks moved here when it neared the 64,000 bytes .github/scripts/check-workflow-files.sh holds each
workflow file to.

## Why a workflow of its own fills the caches, on a push to main

ci-caches.yaml fills the Actions caches ci.yaml's jobs restore, so a pull request or a merge group
finds them warm. ci.yaml runs on a pull request, a merge group and a dispatch, and no longer on a
push to main: the queue tests every change on the tree it lands as, so a run on main would only test
that tree again (.github/ci-notes/ci.md#why-the-router-runs-on-these-events-and-no-others says it).
No step of that workflow saves a cache entry any more, and the actions it calls save only in a
dispatched run, so the caches it restores need a filler of their own, and ci-caches.yaml is it.

Why the filler runs on a push: a run reads the cache scope of its own branch and that of the default
branch, and only an event that writes the default branch's scope - a push to main among them -
leaves an entry every later run can read. A save made in a merge group or on a pull request lands in
that run's own scope, which the next pull request's run cannot read, so it would reach nobody.

A leg of a matrix that fails leaves the other running (`fail-fast: false`, in every matrix of the
workflow): a cancelled leg saves nothing, and one architecture's fill has no reason to cost the
other's.

ci-caches.yaml does not fill go-docs: docs-pages.yaml saves it on this same push, from the build job
it shares with ci.yaml's docs job. Nor the Bun cache: .github/actions/setup-bun reads and writes it
on main, and the workflows that run there call it, ci-caches.yaml's dispatch-builds job too. The
entries it fills that are no Go family are in
.github/ci-notes/ci-caches.md#the-entries-that-are-no-go-family.

## When the workflow runs, and why it has no paths

A push to main is what writes the default branch's cache scope; workflow_dispatch fills a family by
hand, after a Go bump, a move of the Lean toolchain or an eviction. No `paths:`: the families filled
on every push hold the build of many packages of a module and of the modules it imports, so nearly
any change to Go code is theirs, and a push that changes none of it compiles nothing on the restored
entry; the other keys hold a Go version, that toolchain file's hash, the hash of
packages/envoy/go.sum, a Dispatch release's commit or, for go-envoy-infra, the hash of the infra
workspace's module files, not a file list, so what the push touched decides nothing for them.

A release is tagged minutes after its push to main (release-envoy-listener.yaml builds first), so
the push's own fill resolves the release before it, and the pull requests and merge groups after the
tag ask dispatch-two-builds for the new one. So that workflow's release_publish job, once it has
tagged a release, dispatches ci-caches.yaml on main with `only: dispatch-builds`, which runs the
dispatch-builds jobs alone: every other job's `if:` leaves that run out, since a release changes
none of their keys.

## One fill at a time per ref, and no cancellation

One fill at a time per ref, and no cancellation: a cancelled run saves nothing, so cutting the
running fill short would leave the caches where the fill before it left them, and the next fill
would compile both commits' changes. One at a time also keeps a superseded job's deletes from
meeting another fill's save. The default queue holds the newer run until this one ends, and a push
after it replaces the one waiting, since the newer commit's fill holds what the waiting one's would.
A dispatch-builds run has a group of its own, so it never replaces a waiting push fill, whose Go
families it would not fill; it can run beside a push fill, and dispatch-builds-superseded deletes no
main entry newer than the one it keeps, so neither run's deletes take the other's new entry.

## Why three jobs alone may delete entries

Reading the tree and the caches is all ci-caches.yaml does but in three jobs: the cache scope needs
no token scope of its own, and nothing in it reports a check or writes an artifact. The
refill-superseded, envoy-infra-superseded and dispatch-builds-superseded jobs alone take
`actions: write`, to delete the entries a fill replaced or this repository keeps no more, and build
nothing.

## The Go families, their readers and their runners

Eight Go families, each one a restore step of ci.yaml's run names, keyed on the runner's
architecture and on the Go version the family's own module installs (go-envoy-infra on the hash
of its module files too, for the reason its job gives): go-notices (lint's notices step),
go-daemon (typecheck, daemon-go, plugin-pack and pi-legion), go-envoy (envoy-go-build,
envoy-go-checks, listener-server-change, envoy-e2e-local, envoy-session-stall and envoy-refill,
and the embedder job of .github/workflows/dispatch-browser.yaml), go-envoy-test (envoy-go,
envoy-go-claim-model, which builds the API's test binary of that set, the binaries, multiprocess
and visual jobs of .github/workflows/dispatch-browser.yaml, which ci.yaml's dispatch-browser and
dispatch-browser-pr
jobs call: those three build the Dispatch server and the real broker's commands, whose packages
the go-envoy entry's build of the listener and natsd alone does not hold, the two jobs of
.github/workflows/ci-job-dispatch-two-builds.yaml, whose ceiling step and twobuild test step build
out of that same set, and dispatch-memory and dispatch-multiprocess, whose ceiling step and tests
read its modules and objects), go-envoy-smoke (envoy-smoke, whose three go commands build in two
workspaces: the smoke test in the root one, envoy-infra's host live tests and plugin in the infra
module's; .github/ci-notes/ci-caches.md#the-envoy-smoke-family), go-envoy-race (every leg of
envoy-go-race, envoy-go-race-api and
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
the browser lane, the two-build jobs and the release builds on the second); and go-envoy-smoke
and go-envoy-darwin-vet on ubuntu-24.04 alone, where envoy-smoke and envoy-go-build run. A leg
builds what that
runner's readers build and no more: go-daemon's arm64 reader is typecheck alone, so its arm64 leg
runs typecheck's vet and leaves the test and race builds to the x86_64 leg.

go-notices carries the architecture before the OS (go-notices-<arch>-<os>-go<version>) and the
other seven after it: the go-notices entry holds go-licenses, a program lint's notices step runs,
and a run of a workflow file older than the architecture in the key restores by go-notices-<os>-,
which an <os>-<arch> key starts with (ci-job-lint.yaml's restore step says what that did). One Go
setup per leg, since the key carries the version that module's go.mod names: the daemon's go.mod
for go-daemon and for go-notices, which lint installs the daemon's Go for; the envoy module's for
go-envoy, go-envoy-test, go-envoy-smoke, go-envoy-race and go-envoy-darwin-vet (go-envoy-smoke
builds in the infra module's workspace too, on that same Go, as envoy-smoke does); the infra
module's for go-envoy-infra.

A fill's restore asks for the readers' key, with that key and a `-` as its restore-keys. GitHub
matches the key itself as a prefix too, so an entry of another Go version is restored only when
that version's text starts with this one's (go1.27.30 for go1.27.3), and Go keys every object of
its build cache on the toolchain that compiled it, so such an entry costs only its download. No
entry carries the readers' key itself, so GitHub's matching gives a reader the newest entry whose
key starts with it: the last fill's.

## Why each fill builds on the newest entry of its family

Every Go family but go-envoy-infra is filled again on every push to main: the fill restores the
newest entry of its family, runs on it the build its readers run, and saves the result under the
readers' key followed by the commit (`-${{ github.sha }}`), so a reader's prefix match finds the
last fill's entry.

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
new Go (unless an older version's text starts with the new one's, as the section on the Go families
says) and builds from nothing.

## What the go-daemon fill builds on each leg

go-daemon keys on packages/daemon/go.mod's Go: ci.yaml's typecheck, daemon-go, plugin-pack and
pi-legion set Go up from it. One leg per runner, because typecheck runs on ubuntu-24.04-arm and the
other three on ubuntu-24.04, and the key carries the runner's architecture. Each leg builds what its
runner's readers build: typecheck's vet on both, and the test and race builds on the x86_64 leg
alone, since no job that reads the arm64 entry runs them. Each push fills it again from its newest
entry (.github/ci-notes/ci-caches.md#why-each-fill-builds-on-the-newest-entry-of-its-family).

What typecheck builds is the vet behind the e2e tag. It is the one build of this family an arm64 job
makes (.github/workflows/ci-job-typecheck.yaml), so both legs run it. typecheck's darwin vet is the
one build left out: its darwin objects, about 1.65 GB of build cache on a devbox, would ride in an
entry every job that restores go-daemon downloads, and the step builds them in about two minutes
(run 43990864191).

What daemon-go builds is the test binary of every package, and the race build of the five packages
daemon-go runs under the detector, whose objects are cache entries of their own. Those five are the
list .github/scripts/ci_job_reads.py's DAEMON_RACE_PACKAGES holds, which check-ci-filters.sh holds
daemon-go's race legs (.github/go-legs.yaml) to, so a package that joins or leaves the detector
there is the side the fill's command follows. The test binaries are compiled and never run (`go test
-c -o /dev/null` links each one and writes nothing): running them would want tmux, jj, Oh My Pi, uv,
container images and a Postgres service, and the build cache this entry holds is what a compile
leaves. The x86_64 leg alone: daemon-go, plugin-pack and pi-legion, which read these objects, run on
ubuntu-24.04.

## What the go-notices fill builds

go-notices is filled in a job of its own on ubuntu-24.04-arm, the runner of lint, whose notices step
the family belongs to. It keys on packages/daemon/go.mod's Go, which lint installs, though that step
also builds the envoy and infra modules' commands, and on the runner's architecture before the OS,
as lint's restore does: the entry holds the go-licenses that step runs, and an `<os>-<arch>` key
would start with the `go-notices-<os>-` prefix of a run of a workflow file older than the
architecture in the key, which then restores this arm64 program on x86_64.

The job runs the very script lint's notices step runs, .github/scripts/go-release-notices.sh: the
same loops over .github/go-release-targets.json, .github/agent-secrets-client-targets.json and
.github/envoy-infra-targets.json, with the same GOOS, GOARCH and CGO_ENABLED per target, so the
modules it downloads, the go-licenses it builds under ~/.cache/legion and the objects it leaves are
the ones lint restores. go-licenses sits there in a directory that names this runner's OS and CPU
(scripts/go-third-party-notices.sh), so a lint on another CPU that restores this entry finds no
go-licenses at its own path and builds one rather than run this one. One script and not a copy of
those loops, because a copy stops warming a target the day lint gains one, and nothing fails: lint
just builds those objects cold on every run.

## What the go-envoy fill builds, and why it runs no vet

go-envoy keys on packages/envoy/go.mod's Go, the module every job that restores it sets Go up from.
Each push fills it again from its newest entry
(.github/ci-notes/ci-caches.md#why-each-fill-builds-on-the-newest-entry-of-its-family). One leg per
runner: the envoy lanes that restore it run on ubuntu-24.04-arm, and envoy-go-build,
envoy-go-checks, the browser lane's embedder, the nightly browser re-record and ci-caches.yaml's
darwin vet restore it on ubuntu-24.04.

The envoy job builds the two binaries envoy-e2e-local's scripts/e2e-local.sh and
envoy-session-stall's test/envoy-stack.ts build, and whose packages listener-server-change compiles
too: the listener and the in-process NATS server's natsd. An -o ending in a separator names a
directory, as envoy-stack.ts's build does, so both main packages build in one command.
envoy-go-build's darwin vet is left out: its darwin objects, about 1.7 GB of build cache on a
devbox, would ride in an entry every job that restores go-envoy downloads. They have a family of
their own, go-envoy-darwin-vet, which the envoy-darwin-vet job fills.

The fill runs no go vet, on purpose (PR #5289): with one, full or -stdmethods alike, the saved x64
entry was 475 MB against 202 MB without, so a go-envoy reader that vets reruns cgo for net, os/user
and runtime/cgo, and that is the chosen cost.

The envoy job's time limit. Warm, on its newest entry: 37 s a leg (jobs 138290290861, 138290290676).
Cold, from an empty module and build cache: 61 s and 48 s, the build 37 s and 25 s, before a save of
about 0.5 GB (probe run 45375660275, jobs 138381154486 and 138381153841). 10 minutes is about nine
times the cold.

## Why go-envoy-test is filled in a job of its own

go-envoy-test is filled on go-envoy's Go and runners, in a job of its own, so its restore comes
before every build that writes what it restores: it restores its newest entry, then builds the
listener and natsd as the envoy job does and starts the test binary of every package, and saves. Its
entry holds go-envoy's objects too, built in that job on its own entry: envoy-go's tests build the
listener and natsd, and the browser lane's jobs that restore this family build commands of the same
module. In one job with the envoy fill, this restore unpacked over that fill's build and needed the
module cache made writable first.

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

## The envoy-smoke family

go-envoy-smoke holds what ci.yaml's envoy-smoke compiles, and no more: the smoke test
(`go test -tags smoke ./internal/smoke/`) in the root workspace, and envoy-infra's host live tests
(`go test -tags pulumilive ./host/`) and plugin (`go build ./cmd/pulumi-resource-envoy-infra`) in
the infra module's go.work. Before it the job restored no build cache and compiled all three from
nothing on every run. Over the 99 green merge-group
jobs of 2026-11-26T12:00Z to 2026-11-27T07:30Z the job took 689 s at the median; in 20 of their
logs the live step spent 182 s from its last `go: downloading` line to its first `=== RUN`, and
the smoke step 34 s. That compile is CPU-bound on the job's runner (ubuntu-24.04, 4 CPUs): in the
8 jobs of scratch run 45994580307, each of which built the host test binary from empty caches
after its own work, that build took 180 to 272 s at 340 to 355% CPU and wrote 2,959 MB of build
cache. The three jobs that had restored nothing read 211 to 216 s of that spread; the spread
itself is the runner's, and nothing of the arm the job ran.

Why a family of its own. No other family holds the build of these three packages. go-notices
builds in both workspaces too (ci-caches.yaml's notices job: "that step also builds the envoy and
infra modules' commands"), but what it holds is the release commands' objects, not these. What
decides it is the measurement. go-envoy-test restored in
14 to 22 s and took the smoke step's compile to 1 s, but left the live step's at 174 and 228 s: the
infra workspace chooses other versions of the modules the two share (golang.org/x/sys 0.49 there,
0.50 in the root workspace), and holds the Pulumi SDKs, which no envoy job builds. go-envoy-infra's
x86_64 entry holds the infra build, and took the live compile to 12 s, but restored in 32 to 56 s:
its 1,114 MB unpack to 1,565 MB of modules and 6,938 MB of build cache, since its fill also builds
the darwin vets and the natslive test binaries (ci-caches.yaml's envoy-infra job), and this job reads
about 3 GB of it. The restore's time is the unpacking: 42 of its 49 s in job 140490649374, the
download 7. Two
restores into one module cache do not both land
(.github/ci-notes/ci.md#a-cache-restore-comes-before-every-step-that-can-write-a-path-it-names).
The job's own entry, built from nothing with its three go commands, is 592 MB: 1,095 MB of modules
and 3,088 MB of build cache.

Measured on CI's runner, ubuntu-24.04, in scratch runs 45994580307 and 45998597537 (branch
probe/envoy-smoke-time, never merged). Each job ran envoy-smoke's own steps with one restore step
or none, and its time below leaves out the probe's own steps:

| restored | jobs | job, s | restore, s | live step, s (compile) | smoke step, s (compile) |
| --- | --- | --- | --- | --- | --- |
| nothing | 6 | 681.5 | - | 324.2 (191.6) | 112.8 (35.5) |
| go-envoy-test | 2 | 681.5 | 18.0 | 333.0 (200.8) | 76.5 (0.9) |
| go-envoy-infra | 6 | 522.7 | 49.3 | 138.2 (12.1) | 90.2 (13.7) |
| the job's own entry | 3 | 479.0 | 24.3 | 132.3 (6.0) | 76.7 (0.8) |

Run 45998597537 ran the first, third and fourth at once: 702.3, 526.3 and 479.0 s. Before the go
steps, `go list -deps -test -f '{{.Stale}}'` read 480 of the smoke test's 482 packages, 1,004 of
the live test's 1,005 and 947 of the plugin's 948 stale with nothing restored, and 1 of each on the
job's own entry. The tests themselves took the same time in every row (the live package 126 to
132 s, the smoke package 76 to 78 s), and all 17 jobs passed the same 8 smoke and 58 live tests
and subtests, none failed or skipped: the entry removes compile, not test. What the job has left
is mostly waits: once compiled, the live package ran at 19 to 25% of the VM's CPU.

The fill is go-envoy-test's: it restores the newest entry, marks it unused, builds and starts the
two test binaries as the job's go test does (`-run '^$'`, so no image or Docker is needed) and
builds the plugin, deletes what those builds did not use and the module zips, and saves under the
readers' key and the commit; refill-superseded deletes the entry it replaced. It runs on
ubuntu-24.04 alone, envoy-smoke's runner. Its key holds no module-file hash, as go-envoy-test's
holds none: the fill runs on every push, and Go's build cache is content-addressed. The scratch
fill built the two test binaries with `go test -c`, which leaves out the compile of each test main
(the section below): the one stale package of each test on that entry was its test main
(`internal/smoke.test` and `host.test`, `build ID mismatch`, read again with `go list -f
'{{.StaleReason}}'` on a cache built the same way), and the fill's `-run '^$'` start compiles it
too. The plugin's one stale package is the command itself, which no build installs. The entry holds no test result for a job to report as cached:
the fill's `-count=1` records none, and no run of ci.yaml saves a cache.

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

## What the go-envoy-darwin-vet fill builds

go-envoy-darwin-vet holds what envoy-go-build's "Vet envoy for darwin, its tagged tests included"
compiles, the standard library and every package the envoy module builds for darwin/arm64, in a
build cache directory of its own, the GOCACHE that step's env names, so neither this entry nor
go-envoy's carries the other's objects. Restored from nothing, that vet compiled all of it on every
run: 137.9 s of envoy-go-checks job 137308591036 (the vet was envoy-go-checks' step then), 172 s at
the median of 550 pull request runs; on a devbox, 803 s cold against 82 s on a warm cache of 1.7 GB.
Each push fills it again from its newest entry
(.github/ci-notes/ci-caches.md#why-each-fill-builds-on-the-newest-entry-of-its-family). The
envoy-darwin-vet job restores go-envoy first, as envoy-go-build does, for the module cache the vet
reads. The job runs on ubuntu-24.04 alone, the runner envoy-go-build runs on, and its key carries
that runner's architecture as every family's does. A job of its own, so neither this fill nor the
envoy jobs' can stop the other.

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
go-envoy-test, go-envoy-smoke and go-envoy-race, and its zips command alone for go-envoy-infra, which builds from nothing and runs
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

go-envoy-infra, elan and the three Dispatch builds are looked up first (`lookup-only`, and no
`restore-keys`: the question is whether that exact key exists, and the answer downloads nothing),
and only a miss runs the build and saves what it left. Each of their keys carries a hash of the
files that choose what the entry holds, and a run that finds its key builds nothing. Each save takes
the key its lookup computed (`steps.<id>.outputs.cache-primary-key`), never a hashFiles() of its
own.

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


On a hit the action restores the entry and compiles only the command's own packages that changed
since the entry was saved; on a miss, as after a go.sum change, it builds the command from the
newest entry of that Go version, or cold when there is none, and saves the entry. The
architecture-check job also checks main's notes, and the spec-check job main's spec files, with
allow-empty while main holds no spec file; each action saves before it checks, so a refused set does
not cost the entry.

Each is a job of its own. That is one of the two placements the architecture-check action's
README.md gives ("The Go toolchain"). The other, in the envoy job, would run the action first,
before that job's setup-go, and a refused note set on main would then stop the go-envoy fills. And
the two are separate jobs, so a refused note set on main cannot stop the spec-check fill and a
refused spec tree on main cannot stop the note check.

## The entries that are no Go family

Four entries ci-caches.yaml fills are no Go family: the elan home the airlock-lean job restores,
whose key is the content of the Lean toolchain the model names rather than a Go version, and the
three earlier Dispatch builds dispatch-two-builds restores, each keyed on a release commit and the
hash of the script that builds it, which the dispatch-builds job fills. The changes job's two
entries are ci-caches-changes.yaml's, which runs once per commit rather than one run at a time
(.github/ci-notes/ci-caches.md#the-two-entries-the-changes-job-restores). Two more went with the
jobs that read them.

## The two entries that went with the jobs that read them

Two entries that were no Go family went with the jobs that read them - the Playwright browsers and
the .deb files of the apt list the browser suite installed - because dispatch-browser.yaml's shard
and alone jobs run in the image this repository builds, which carries the browsers and their host
packages, so neither entry has a restore left to serve (dispatch://INFRA-1443). A save whose
restore is gone is what check-ci-filters.sh refuses, so the two jobs that wrote them are gone with
them.

## What the refill-superseded job deletes, and why it is a job of its own

The entries of go-daemon, go-notices, go-envoy, go-envoy-test, go-envoy-smoke, go-envoy-race and
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

## The go-envoy-infra fill, and the hash in its key

The infra module is not in the root go.work - its own go.work binds it and packages/envoy - so
go-envoy-infra keys on packages/envoy/infra/go.mod's Go and builds in a job of its own, as ci.yaml's
envoy-infra-build and envoy-infra-go do. Unlike the other families, its key also carries the hash of
the files that choose the module versions it builds: that go.work and its go.work.sum, and the
go.mod and go.sum of both modules it binds. An entry keyed on the Go version alone was refreshed
only by a Go bump and went stale as those files moved: restoring the entry of 2026-11-06, eleven
days old, the linux vet (envoy-infra-build's now) took 175, 229 and 177 s in runs 43990864191,
43996496498 and 44008055050, 83 to 85% of what its darwin vet took in the same run with nothing
cached, because google.golang.org/api, gax-go, the AWS SDK and nats had moved under it. With the
hash, a push to main that changes those files misses the lookup and fills a fresh entry; a pull
request that changes none of them restores one built from the same module versions, and one that
changes them restores the newest entry by prefix and builds what moved. One leg per runner:
envoy-infra-build and envoy-infra-go run on ubuntu-24.04-arm, and release-envoy-infra.yaml's build,
which restores the family too, on ubuntu-24.04. envoy-infra-superseded deletes the entry each leg's
fill replaced.

The job's outputs name the key each leg looked up, and whether it filled it, under names of that
leg's architecture: a matrix job's outputs are combined from its legs, and an output both legs set
holds the value of whichever leg ended last. envoy-infra-superseded keeps each filled key, deletes
the entries it supersedes (this ref's go-envoy-infra entries of the same runner and Go with an older
module-file hash), and runs only after a fill.

What the envoy-infra job builds is what envoy-infra-build, envoy-infra-go and the release build
build: the vet with both live-test tags, for linux and for darwin, the natslive binaries of the
three packages envoy-infra-go's live step runs, and the test binaries of each leg's reader. On
ubuntu-24.04-arm, envoy-infra-build's unit step's race build, through the script that step runs
(scripts/race-unit.sh, which the fill runs in its compile mode), so the entry holds what that step
links; check-ci-filters.sh holds this leg to that call and to no race build of its own. On
ubuntu-24.04, release-envoy-infra.yaml's plain `go test ./...`. Compiled, not run: the live tests
need nats-server, Postgres and container images, and the unit tests nothing a build cache keeps. The
darwin vet is in the fill because, built cold, it is the step that brought the one envoy-infra-go
job those halves were near its time limit.

## What envoy-infra-superseded deletes

The envoy-infra-superseded job deletes the go-envoy-infra entries the envoy-infra fill superseded,
once that fill has saved the new one. A module-file change gives the family a new key, and the entry
under the old key, about 1.4 GB, is then read by no run of this ref: left alone it would hold its
space until GitHub drops it after seven days unread, and over the repository's cache limit GitHub
evicts the least recently used entries, which can be another family's live one. An entry the new one
supersedes is one of this ref whose key is the new key but for its 64-hex module-file hash: the same
family, runner and Go, with older module files, the hashless key of the fill before this one
included. An entry of another runner or another Go is no such entry and stays, so a fill that runs
once per architecture deletes nothing the other leg saved. None is deleted unless the new key is
among this ref's entries, so a save that did not land leaves the old entry in place. A job of its
own, so the token that may delete caches is in no job that builds code; cache-superseded.sh says how
it reads the keys. One leg per architecture of the fill, each keeping the key that architecture's
leg saved, under the output names the envoy-infra job gives it; a leg whose architecture filled
nothing skips its step. Both legs run on ubuntu-24.04-arm: gh and jq need nothing of an x64 machine.

## The three Dispatch builds

The three earlier Dispatch builds ci.yaml's dispatch-two-builds job runs beside this tree
(.github/workflows/ci-job-dispatch-two-builds.yaml): the previous build, the newest legion-envoy-v*
release in main's history; the first-rollout build, the release that job pins by its commit
(FIRST_ROLLOUT_COMMIT); and the going-runs build, the release that job pins by its commit
(GOING_RELEASE_COMMIT), read and filled as the first-rollout build is. That job restores each,
builds it on a miss, and saves none: ci-caches.yaml's dispatch-builds job fills each in main's
scope, which every pull request and merge group reads. That job saved them itself before, in the
scope of its run's own ref, where a merge group's entry is read by no later run and a pull request's
by that pull request alone: on 2026-11-23 25 such entries held 12.2 GB of the repository's cache and
main held none, and on 2026-11-22 308 of 351 successful dispatch-two-builds jobs built the previous
release again (107 s at the median, 11 s to restore it). Each key is the key that job restores, from
the same steps: the previous build's commit from build-dispatch-build.sh, the first-rollout and
going-runs commits from that job's own env, each its one pin in CI, read by the fill rather than
copied. Each is looked up first, so a run after which neither the newest release nor the script
changed builds nothing. The dispatch-builds job runs on each push to main and again once
release-envoy-listener.yaml has tagged a release (the `only` input,
.github/ci-notes/ci-caches.md#when-the-workflow-runs-and-why-it-has-no-paths), so the release that
pull requests and merge groups resolve after the tag is filled within minutes of it. The
dispatch-builds job runs on ubuntu-24.04, the runner dispatch-two-builds runs on.

## What dispatch-builds-superseded deletes

The dispatch-builds-superseded job deletes the entries of the three Dispatch builds this repository
keeps no more, on main, once dispatch-builds has looked its keys up. For each build it keeps main's
entry under the key dispatch-builds found or saved, and every main entry created after it, which a
run that resolved a newer release beside this one saved (a release's dispatch-builds run can overlap
a push's). It deletes the main entries created before it, of an older release or an older script,
and every entry of that build, runner and architecture on any other ref, which only that ref's runs
could read: the entries dispatch-two-builds saved in its runs' own refs before it stopped saving.
That is a retention choice, not a proof that nothing reads them: a pull request whose merge base is
older than main's newest release still resolves the older release, misses, and builds it (107 s at
the median), as most runs did before. A run that is downloading a deleted entry at that moment gets
a cache miss: actions/cache/restore warns and the job builds the release, slower but not failed.
Main alone: dispatch-builds saves nothing on another branch. None is deleted unless the kept key is
among main's entries and has the shape dispatch-builds gives it (the build, the runner's OS and
architecture, the script's 64-hex hash, the 40-hex commit), from which the job's delete step takes
the prefix it deletes under; the entries come from GitHub newest first and that step reads every one
of them, so "created before" is GitHub's own order. The safety of envoy-infra-superseded, for the
same reasons (cache-superseded.sh).

## The elan home

The elan home ci.yaml's airlock-lean job restores, the Lean toolchain that job's `lake build` needs.
The elan job runs no setup-go and fills no Go family: the key is the runner's architecture, since
elan and the toolchain are binaries of it, and the content of the lean-toolchain the model names,
since that file is what decides which toolchain elan installs, so the entry is refreshed when the
model moves to another Lean and at no other time. install-elan puts elan under $RUNNER_TEMP/elan and
`lake build` downloads the named toolchain into it on a miss, which is the whole of what is saved.
The elan job runs on ubuntu-24.04-arm, the runner airlock-lean runs on.

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
