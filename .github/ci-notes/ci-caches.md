# Notes for the cache filler's workflow

The derivations behind the budgets of `.github/workflows/ci-caches.yaml`, which point into this
file with `# why:` lines, as `.github/ci-notes/README.md` says, and the record of what the workflow
fills and why. Each value's own arithmetic stays on its line in the workflow file. The budgets of
that file's other jobs are still derived in the comments above their keys. Its other `# why:`
lines name sections of `.github/ci-notes/ci.md`.

## What this workflow fills, and why it runs on a push to main

It fills the Actions caches ci.yaml's jobs restore, so a pull request or a merge group finds
them warm. ci.yaml runs on a pull request, a merge group and a dispatch, and no longer on a push
to main: the queue tests every change on the tree it lands as, so a run on main would only test
that tree again (.github/ci-notes/ci.md#why-the-router-runs-on-these-events-and-no-others says
it). No step of that workflow saves a cache entry any more, and the actions it calls save only
in a dispatched run, so the families it restores need a filler of their own, and this workflow
is it.

Why the filler runs on a push: a run reads the cache scope of its own branch and that of the
default branch, and only an event that writes the default branch's scope (a push to main among
them) leaves an entry every later run can read. A save made in a merge group or on a pull
request lands in that run's own scope, which the next pull request's run cannot read, so it
would reach nobody.

Six Go families, each one a restore step of ci.yaml's run names, keyed on the runner's
architecture and on the Go version the family's own module installs (go-envoy-infra on the hash
of its module files too, for the reason its job gives): go-notices (lint's notices step),
go-daemon (typecheck, daemon-go, plugin-pack and pi-legion), go-envoy (envoy-go-build,
envoy-go-checks, envoy-go-race-docs, listener-server-change, envoy-e2e-local and
envoy-session-stall, and the embedder job of .github/workflows/dispatch-browser.yaml),
go-envoy-test (envoy-go, envoy-go-claim-model, which builds the API's test binary of that set,
the binaries, multiprocess and visual jobs of .github/workflows/dispatch-browser.yaml, which
ci.yaml's dispatch-browser and dispatch-browser-pr jobs call: those three build the Dispatch
server and the real broker's commands, whose packages the go-envoy entry's build of the listener
and natsd alone does not hold, and the two jobs of
.github/workflows/ci-job-dispatch-two-builds.yaml, whose sizing step and twobuild test step
build out of that same set), go-envoy-darwin-vet (envoy-go-build's darwin vet, whose build cache
is a directory of its own, the GOCACHE of that step's env) and go-envoy-infra (envoy-infra-build
and envoy-infra-go, and the build job of release-envoy-infra.yaml).
The key carries `runner.arch` because Go's build cache holds the objects of the architecture
that compiled them: an entry an x86_64 runner filled gives an arm64 job its modules and none of
its objects. So each family is filled on every architecture one of its readers runs on, one
matrix leg per runner: go-notices on ubuntu-24.04-arm alone, where lint runs, and go-daemon,
go-envoy, go-envoy-test and go-envoy-infra on ubuntu-24.04-arm and on ubuntu-24.04 both, since
each has a reader on each (typecheck and the envoy lanes but one on the first; daemon-go,
plugin-pack, pi-legion, envoy-go-build, envoy-go-checks, envoy-go-claim-model, the browser lane,
the two-build jobs and the release builds on the second), and go-envoy-darwin-vet on
ubuntu-24.04 alone, where envoy-go-build runs. A leg builds what that runner's readers build and
no more: go-daemon's arm64 reader is typecheck alone, so its arm64 leg runs typecheck's vet and
leaves the test and race builds to the x86_64 leg.
go-notices carries the architecture before the OS (go-notices-<arch>-<os>-go<version>) and the
other five after it: the go-notices entry holds go-licenses, a program lint's notices step
runs, and a run of a workflow file older than the architecture in the key restores by
go-notices-<os>-, which an <os>-<arch> key starts with (ci-job-lint.yaml's restore step says what
that did). One Go setup per leg, since the key carries the version that module's go.mod names:
the daemon's go.mod for go-daemon and for go-notices, which lint installs the daemon's Go for;
the envoy module's for go-envoy, go-envoy-test and go-envoy-darwin-vet; the infra module's for
go-envoy-infra.

go-daemon, go-notices, go-envoy, go-envoy-test and go-envoy-darwin-vet are filled again on
every push to main. The fill restores the newest entry of its family, runs on it the build the
ci.yaml job runs, which compiles only what changed since that entry was saved, and saves the
result under the key ci.yaml's readers restore followed by the commit (`-${{ github.sha }}`).
The fill's restore asks for the readers' key, with that key and a `-` as its restore-keys.
GitHub matches the key itself as a prefix too, so an entry of another Go version is restored
only when that version's text starts with this one's (go1.27.30 for go1.27.3), and Go keys every
object of its build cache on the toolchain that compiled it, so such an entry costs only its
download. No entry carries the readers' key itself any more, so GitHub's matching gives a reader
the newest entry whose key starts with it: the last fill's.
Why each fill builds on the newest entry of its family, and why it keeps only what its own
build used, are the two sections after this one.

go-envoy-infra, elan and the two dispatch builds are still looked up first (`lookup-only`, and
no `restore-keys`: the question is whether that exact key exists, and the answer downloads
nothing), and only a miss runs the build and saves what it left. Each of their keys carries a
hash of the files that choose what the entry holds, and a run that finds its key builds
nothing. Each save takes the key its lookup computed (`steps.<id>.outputs.cache-primary-key`),
never a hashFiles() of its own (#why-a-lookups-save-takes-the-lookups-key). A leg of a matrix
that fails leaves the other running
(`fail-fast: false`): a cancelled leg saves nothing, and one architecture's fill has no reason
to cost the other's.

Beside those six, the architecture-check and spec-check jobs fill the families of the
composite actions .github/actions/architecture-check and .github/actions/spec-check, which
restore and save their entries themselves (#the-architecture-check-and-spec-check-families).

Four families here are no Go family. The elan home the airlock-lean job restores, whose key is
the content of the Lean toolchain the model names rather than a Go version. The bun family,
~/.bun/bin, which .github/actions/setup-bun restores in every job that calls it and saves only
where its caller passes `save: 'true'` on a push to main: the bun job is the one caller that
does, the family's one saving job as each Go family has one. The key names the runner's
architecture, so each architecture needs an entry and a saving job of its own, and the bun job
runs a leg on each runner the action's callers run on, ubuntu-24.04 and ubuntu-24.04-arm. And
the two builds dispatch-two-builds runs the tree against, the previous release's and the
first-rollout release's, which that job restores and this workflow's dispatch-builds job builds
and saves. Two others that were no Go family went with the jobs that read them
(#the-two-entries-that-went-with-the-jobs-that-read-them).

go-docs is not here: docs-pages.yaml saves it on this same push, from the build job it shares
with ci.yaml's docs job.

## Why each fill builds on the newest entry of its family

These keys used to name the Go version alone, and the fill saved only when that exact key was
missing, so each family was filled once per Go version. The go-daemon, go-envoy and go-envoy-test
entries every job restored on 2026-11-22 had been saved on 2026-11-18 between 13:03 and 13:08Z
(go-notices' on 2026-11-19), and at main's e7381b7d4c9 a job that restored them had to compile
again 312 of the daemon module's 335 packages and 573 of the envoy module's 618 (`go list -test
-deps -f {{.Stale}}`); from an entry filled five merges earlier, 135 and 321. The
refill-superseded job deletes the other entries of the family once the new one is listed, three a
run at most, newest first, so that its deletes are three priced ladders inside the job's budget
that check-download-retry.sh can read (#the-refill-superseded-job); a common run supersedes one,
so each family keeps one entry per runner, as it did before. A job that is downloading a deleted
entry at that moment gets a cache miss: actions/cache/restore warns and the job goes on, compiling
what the entry would have given it, so it is slower, not failed.

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
new Go (unless an older version's text starts with the new one's, as the prefix note of
#what-this-workflow-fills-and-why-it-runs-on-a-push-to-main says) and builds from nothing.

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

## The Go module downloads before each fill's builds

Each Go fill downloads the modules its builds compile once, retried, before those builds (the
Retries note of .github/ci-budgets.md; the retried unit is the download, never the build and
never the test). On every run, as the builds run on every push now: the restored entry's module
cache holds what the last fill downloaded, so the call fetches only what moved since.

- The daemon job makes one call: the root go.work binds packages/daemon and packages/envoy, and a
  download in either reads both modules' requirements.
- The notices job makes two calls for the three module directories the notices script builds in:
  in workspace mode `go mod download` with no arguments downloads the pattern `all` of the whole
  workspace, so the first call, made in the root go.work, brings down both modules'
  requirements. packages/envoy/infra the root go.work does not bind (its own go.work binds it and
  packages/envoy), so it takes the second call. packages/daemon is downloaded there though the
  daemon job downloads it too: the two jobs answer different keys on runners of their own, so
  neither can lean on the other's module cache.
- The envoy job makes one call for its build, and the envoy-test job, a job of its own since
  main's #5334, one for its two, each after its job's restore, which would otherwise unpack over
  what the call wrote.
- The darwin vet's call runs on top of the module cache go-envoy's newest entry gave. `go mod
  download` with no arguments takes the whole build list, every platform's modules among them, so
  the darwin vet finds its own.
- The envoy-infra job's one call runs on that build's own condition, so a run that finds the key
  downloads nothing: packages/envoy/infra's go.work binds it and packages/envoy, and a download in
  it reads both modules' requirements. Its lookup is `lookup-only`, so no module cache is ever
  restored there and the cold figure below is what a miss pays.

180 s per attempt, for every one of them: the slowest cold `go mod download` measured is 113 s,
for packages/envoy/infra, against 81 s for packages/daemon and 77 s for packages/envoy, each on an
empty module cache on 2026-11-07, so the bound is 1.6 times the slowest of the three. It can be
that tight where a tarball fetch's could not: an attempt the bound kills leaves what it fetched in
the module cache for the next one to resume from. A run with no entry of its Go to restore, as
after a Go bump, starts from an empty module cache, and that cold figure is what it pays.

## When it runs, and one fill at a time

A push to main is what writes the default branch's cache scope; workflow_dispatch fills a family
by hand, after a Go bump, a move of the Lean toolchain or an eviction. No `paths:`: the five
families filled on every push hold the build of every package of a module and of the modules it
imports, so nearly any change to Go code is theirs, and a push that changes none of it compiles
nothing on the restored entry; the other keys hold a Go version, that toolchain file's hash, the
hash of packages/envoy/go.sum, the Bun version .bun-version pins, a release's commit beside the
hash of the script that builds it or, for go-envoy-infra, the hash of the infra workspace's
module files, not a file list, so what the push touched decides nothing for them, and a run
whose keys are all there is seconds for them, but for the dispatch-builds job's checkout of the
whole history, which its key needs.

A release is tagged minutes after its push to main (release-envoy-listener.yaml builds first),
so the push's own fill resolves the release before it, and the pull requests and merge groups
after the tag ask dispatch-two-builds for the new one. So that workflow's release_publish job,
once it has tagged a release, dispatches this one on main with `only: dispatch-builds`, which
runs the dispatch-builds jobs alone: every other job's `if:` leaves that run out, since a
release changes none of their keys.

One fill at a time per ref, and no cancellation: a cancelled run saves nothing, so cutting the
running fill short would leave the caches where the fill before it left them, and the next fill
would compile both commits' changes. One at a time also keeps a superseded job's deletes from
meeting another fill's save. The default queue holds the newer run until this one ends, and a
push after it replaces the one waiting, since the newer commit's fill holds what the waiting
one's would. A dispatch-builds run has a group of its own, so it never replaces a waiting push
fill, whose Go families it would not fill; it can run beside a push fill, and
dispatch-builds-superseded deletes no main entry newer than the one it keeps, so neither run's
deletes take the other's new entry.

## The refill-superseded job

It deletes the entries of go-daemon, go-notices, go-envoy, go-envoy-test and go-envoy-darwin-vet
that the fills replaced, once each fill has saved its new one. Each fill saves a new entry, over
a gigabyte for most families, so without this the entries would pile up: GitHub drops an entry
only after seven days unread, and over the repository's cache limit it evicts the least recently
used entries, which can be another family's live one. For each family and architecture it keeps
one entry of this ref, the one the fill just saved, and deletes the others of that family,
runner and Go, three a run at most (below): those whose key is the readers' key followed by
`-`, and the one under the readers' key itself, which the fills saved before they named the
commit and which would otherwise win every reader's restore as an exact match. The family then
holds what it held before the refill, one entry per runner, in a repository near its cache
limit. A job that is downloading a deleted entry at that moment gets a cache miss:
actions/cache/restore warns and the job goes on, compiling what the entry would have given it,
slower but not failed. An entry of another runner or another Go is no such entry and stays. None
is deleted unless the new key is among this ref's entries, so a save that did not land leaves
the old entries in place. It is a job of its own, so the token that may delete caches is in no
job that builds code. A cache key is text any build that saves on this ref chose, so the step
never splits or prints one raw: the list stays JSON, jq selects the numeric ids to delete, and
each id is checked to be a number before `gh cache delete` gets it. One leg per family and
architecture, each keeping the key that fill's leg saved, under the output name the fill gives
it; a leg whose fill saved nothing (it failed before its save) skips its steps, and the job runs
when any fill failed, so one failed fill does not stop the others' deletes. Every leg runs on
ubuntu-24.04-arm: gh and jq need nothing of an x64 machine.

Each read and each delete retries through `.github/scripts/ci-retry.sh` at 30 s an attempt, the
bound this repository gives a `gh` read of the API (dispatch://INFRA-1545, R1), run through
gh-read.sh's gh_wait_reset: the list is one ladder, 5 x 30 + 150 = 300 s, and each delete one
more. A leg deletes three entries at most, newest first, in a loop over three literal passes,
which is a count `.github/scripts/check-download-retry.sh` reads: a common run deletes one, the
entry the fill before it saved, and the first run after the keys took the commit deletes the
entry under the readers' key beside it; a third is room for one a failed run left. Any more
stay, a warning says how many, and the next fill deletes them, since each fill adds one entry
and may delete three. A delete whose first attempt landed and whose answer was lost is refused
on its retry as an entry that is gone, and fails the step by name; the next fill deletes what is
left. The step's own budget is 300 + 3 x 300 = 1200 s, a whole number of minutes, so it takes
the minute above, 21; the job's is 300 s, the 5 minutes main's #5141 gave it, plus that one
retried step, 300 + 1200 = 1500 s -> 26 min.

## The architecture-check job

architecture-check-<os>-<arch>-go<version>-<hash> is the family of the composite action
.github/actions/architecture-check, which the job runs as ci.yaml's envoy-go-build runs it. The
action sets its own Go up, from packages/envoy/go.mod, and restores and saves the entry itself.
It saves only in a push, schedule or workflow_dispatch run, so on a push to main it fills the
entry in main's scope, where every pull request and merge group of this repository finds it: on
a hit it restores the entry and compiles only the command's own packages that changed since the
entry was saved; on a miss, as after a go.sum change, it builds the command from the newest
entry of that Go version, or cold when there is none, and saves the entry. It also checks main's
notes; the action saves before it checks, so a refused set does not cost the entry. A job of its
own is one of the two placements the action's README.md gives ("The Go toolchain"). The other,
in the envoy job, would run the action first, before that job's setup-go, and a refused note set
on main would then stop the go-envoy fills, so it is a job of its own.

## The envoy-infra job

The infra module is not in the root go.work (its own go.work binds it and packages/envoy), so
go-envoy-infra keys on packages/envoy/infra/go.mod's Go and builds in a job of its own, as
ci.yaml's envoy-infra-build and envoy-infra-go do. Unlike the other families, its key also
carries the hash of the files that choose the module versions it builds: that go.work and its
go.work.sum, and the go.mod and go.sum of both modules it binds. An entry keyed on the Go
version alone was refreshed only by a Go bump and went stale as those files moved: restoring
the entry of 2026-11-06, eleven days old, the linux vet (envoy-infra-build's now) took 175,
229 and 177 s in runs 43990864191, 43996496498 and 44008055050, 83 to 85% of what its darwin
vet took in the same run with nothing cached, because google.golang.org/api, gax-go, the AWS
SDK and nats had moved under it. With the hash, a push to main that changes those files misses
the lookup and fills a fresh entry; a pull request that changes none of them restores one built
from the same module versions, and one that changes them restores the newest entry by prefix
and builds what moved. One leg per runner: envoy-infra-build and envoy-infra-go run on
ubuntu-24.04-arm, and release-envoy-infra.yaml's build, which restores the family too, on
ubuntu-24.04. The envoy-infra-superseded job deletes the entry each leg's fill replaced.

## The envoy-infra-superseded job

It deletes the go-envoy-infra entries the envoy-infra fill superseded, once it has saved the new
one. A module-file change gives the family a new key, and the entry under the old key, about
1.4 GB, is then read by no run of this ref: left alone it would hold its space until GitHub drops
it after seven days unread, and over the repository's cache limit GitHub evicts the least
recently used entries, which can be another family's live one. An entry the new one supersedes
is one of this ref whose key is the new key but for its 64-hex module-file hash: the same
family, runner and Go, with older module files, the hashless key of the fill before this one
included. An entry of another runner or another Go is no such entry and stays, so a fill that
runs once per architecture deletes nothing the other leg saved. None is deleted unless the new
key is among this ref's entries, so a save that did not land leaves the old entry in place. A
job of its own, so the token that may delete caches is in no job that builds code. A cache key
is text any build that saves on this ref chose, so the step never splits or prints one raw: the
list stays JSON, jq selects the numeric ids of the superseded entries, and each id is checked to
be a number before `gh cache delete` gets it. A key holding a newline then cannot add a line of
its own, such as `--all`, to that list. One leg per architecture of the fill, each keeping the
key that architecture's leg saved, under the output names the envoy-infra job gives it; a leg
whose architecture filled nothing skips its step. Both legs run on ubuntu-24.04-arm: gh and jq
need nothing of an x64 machine.

Each read and each delete retries through `.github/scripts/ci-retry.sh` at 30 s an attempt, the
bound this repository gives a `gh` read of the API (dispatch://INFRA-1545, R1): the list is one
ladder, 5 x 30 + 150 = 300 s, and each delete one more. A fill supersedes the one entry the fill
before it saved, so a common run deletes one, and the step's DECLARED_LOOPS entry in
`.github/scripts/check-download-retry.sh` says so: 300 + 300 = 600 s for the step. A delete
whose first attempt landed and whose answer was lost is refused on its retry as an entry that is
gone, and fails the step by name; the next fill deletes nothing more.

## The envoy and envoy-test jobs' budgets

Main's #5334 split the go-envoy-test fill out of the envoy job into a job of its own, envoy-test,
and keyed both from measurements. The envoy job: warm, on its newest entry, 37 s a leg (jobs
138290290861 and 138290290676); cold, from an empty module and build cache, 61 s and 48 s before
a save of about 0.5 GB (probe run 45375660275, jobs 138381154486 and 138381153841). Main gave it
10 minutes, about nine times the cold. The envoy-test job: warm, 5.5 and 4.1 min a leg, save
included (jobs 138290290807 and 138290290989); cold, 6.2 and 4.0 min before a save of 3.5 GB,
which took up to 1.9 min warm (probe run 45375660275, jobs 138381154602 and 138381154546). Main
gave it 30 minutes, about four times the cold job with its save.

Each job's first term is main's figure, and the Budgets note of `.github/ci-budgets.md` adds its
slowest retried step alone, its Go module download, 5 x 180 + 150 = 1050 s
(#the-go-module-downloads-before-each-fills-builds). The envoy job: 600 + 1050 = 1650 s -> 28
min. The envoy-test job: 1800 + 1050 = 2850 s -> 48 min.

## The elan job's budget

The job's measured term is 1800 s, the 30 minutes it was written with: one toolchain download and
one build of the model on a miss, where a run that finds the key costs the checkout alone. The
Budgets note of `.github/ci-budgets.md` adds its single slowest retried step, install-elan's one
`ci_tools_fetch` of the elan release tarball, 1708 s (that note's third worst case), above the Lean
toolchain step's 750 s. 1800 + 1708 = 3508 s -> 59 min.

The Lean toolchain step is airlock-lean's, run here on the same miss that runs the build:
`elan toolchain install` of the toolchain lean-toolchain names, through
`.github/scripts/ci-retry.sh` at 120 s an attempt, so the build after it downloads nothing and the
entry saved holds the toolchain airlock-lean's build reads.
.github/ci-notes/ci-job-airlock-lean.md#the-lean-toolchain-step-and-its-bound derives that
bound from four measured installs, this job's own among them: job 134958116292 of run 44348716140,
the first fill on ubuntu-24.04-arm, where elan's download took 4.8 s and the whole install 11.6 s.
5 x 120 + 150 = 750 s -> 13 min.

## The bun job's budget

The bun job is the bun family's one saving job (the job's own comment says why there is one leg
per runner architecture). No run of it is measured yet, so its first term is built from what it
runs: a checkout of one commit, 68 s at most over the 1,032 checkouts of the 25 ci.yaml runs
created from 2026-11-21T07:46Z to 08:23Z (median 4 s), and `.github/actions/setup-bun`, 5 s at
most over the 482 calls in those runs that restored the entry. A miss adds the release asset of
51,794,595 bytes and the save of what it installed. 600 s holds that with room, and the Budgets
note of `.github/ci-budgets.md` adds the job's one retried step, setup-bun's two `ci_tools_fetch`
calls, 2 x 1708 = 3416 s (that note's terms). 600 + 3416 = 4016 s -> 67 min, above that step's own
57.

## The dispatch-builds job

It fills the two builds ci.yaml's dispatch-two-builds job runs the tree against
(.github/workflows/ci-job-dispatch-two-builds.yaml), each made by
packages/envoy/scripts/build-dispatch-build.sh from a commit of this repository: the previous
build, the newest legion-envoy-v* release in main's history, and the first-rollout build, the
release that job pins by its commit (FIRST_ROLLOUT_COMMIT). That job restores each, builds it on
a miss, and saves neither, since an entry a pull request or a merge group saves lands in a scope
no other run reads (dispatch://INFRA-1545, R4): this job fills both in main's scope, which every
pull request and merge group reads. That job saved them itself before: on 2026-11-23 25 such
entries held 12.2 GB of the repository's cache and main held none, and on 2026-11-22 308 of 351
successful dispatch-two-builds jobs built the previous release again (107 s at the median, 11 s
to restore it).

Each key is the text of that job's restore, which check-ci-filters.sh holds this job's save to,
from the same steps: the previous build's commit from build-dispatch-build.sh, and the
first-rollout commit from that job's own env, its one pin in CI, read here with yq rather than
copied. Each is looked up first, so a run after which neither the newest release nor the script
changed builds nothing and sets neither Go nor Bun up. It runs on each push to main and again
once release-envoy-listener.yaml has tagged a release (the `only` input,
#when-it-runs-and-one-fill-at-a-time), so the release that pull requests and merge groups
resolve after the tag is filled within minutes of it. Main alone: a dispatched run on another
branch would save the builds in that branch's scope, which only that branch's runs read. On
ubuntu-24.04, the runner dispatch-two-builds runs on: the key names the runner's architecture,
and each build's server is a binary of it. The whole history and the tags, as that job checks
them out: the script resolves the previous release from the tags in the history of origin/main,
and refuses a commit older than its floor release, which it reads by its tag.

## The dispatch-builds job's budget

The job builds, on a miss of its key, each of the two builds ci.yaml's dispatch-two-builds job
restores, with the script and the bounds that job's build steps run
(.github/ci-notes/ci-job-dispatch-two-builds.md#the-build-steps-budgets): each build step is
1050 + 1050 + 116 = 2216 s -> 37 min.

No run of it is measured yet, so its first term is built from what it runs: the checkout of the
whole history, 96 s at most over the 753 checkouts of dispatch-two-builds that job's own comment
counts, and 68 s at most in the window the bun job's term above reads; the read of the
first-rollout pin out of ci-job-dispatch-two-builds.yaml with yq, seconds, which fetches nothing;
setup-go and setup-bun, the latter 5 s at most on a restore; and the two builds, 116 s at most
each in the window `.github/ci-notes/ci-job-dispatch-two-builds.md` reads, and 118 s at the 90th
percentile of the 2026-11-22 count main's #5182 gives (107 s at the median). That is under 400 s,
and 900 s holds it with room. Its slowest retried step is setup-bun's two `ci_tools_fetch` calls,
3416 s, above each build's 2216 s. 900 + 3416 = 4316 s -> 72 min, above every step's own budget.

Main's #5182 wrote a job of this name too, keyed at 30 minutes for two builds on a miss, with no
retried step priced. The merge of main at ae758e3b keeps one job: this one's steps and key, and
main's `if:` (main alone), its outputs, which dispatch-builds-superseded keeps, and its read of
the pin, which takes the place of a second copy of the commit in this job's env. None of those is
a retried step, so neither term moves.

## The dispatch-builds-superseded job

It deletes the entries of the two Dispatch builds this repository keeps no more, on main once
dispatch-builds has looked its keys up. For each build it keeps main's entry under the key
dispatch-builds found or saved, and every main entry created after it, which a run that resolved
a newer release beside this one saved (a release's dispatch-builds run can overlap a push's). It
deletes the main entries created before it, of an older release or an older script, and every
entry of that build, runner and architecture on any other ref, which only that ref's runs could
read: the entries dispatch-two-builds saved in its runs' own refs before it stopped saving. That
is a retention choice, not a proof that nothing reads them: a pull request whose merge base is
older than main's newest release still resolves the older release, misses, and builds it (107 s
at the median), as most runs did before. A run that is downloading a deleted entry at that
moment gets a cache miss: actions/cache/restore warns and the job builds the release, slower but
not failed. Main alone: dispatch-builds saves nothing on another branch. None is deleted unless
the kept key is among main's entries and has the shape dispatch-builds gives it (the build, the
runner's OS and architecture, the script's 64-hex hash, the 40-hex commit), from which the step
takes the prefix it deletes under; the entries come from GitHub newest first and the step reads
every one of them, so "created before" is GitHub's own order. The safety of
envoy-infra-superseded, for the same reasons: a job of its own, the list kept as JSON, jq
selecting numeric ids, each id checked to be a number before `gh cache delete` gets it.

A leg deletes three entries at most, newest first, in a loop over three literal passes, as
refill-superseded does and for its reason (#the-refill-superseded-job): each read and each delete
retries through `.github/scripts/ci-retry.sh` at 30 s an attempt, the bound this repository gives
a `gh` read of the API (dispatch://INFRA-1545, R1), so the list is one ladder,
5 x 30 + 150 = 300 s, each delete one more, and three of them are priced ladders inside the job's
budget that `.github/scripts/check-download-retry.sh` can read. Any more stay, a warning says how
many, and the next run deletes three more, so the 25 entries other refs held on 2026-11-23 go in
five runs at least, six a run. A delete whose first attempt landed and whose answer was lost is
refused on its retry as an entry that is gone, and fails the step by name; the next run deletes
what is left. The step's own budget is 300 + 3 x 300 = 1200 s, a whole number of minutes, so it
takes the minute above, 21; the job's is 300 s, the 5 minutes main's #5182 gave it, plus that one
retried step, 300 + 1200 = 1500 s -> 26 min.

## The notices step whose script the gate does not read

`Write every Go release archive's third-party notices and cross-build the commands`, in the notices
job, runs `.github/scripts/go-release-notices.sh`, the script ci.yaml's lint job runs, and so waits
for the one go-licenses install of `scripts/go-third-party-notices.sh` that
.github/ci-notes/ci-job-lint.md#the-notices-step-whose-script-the-gate-does-not-read derives,
1050 s, which `.github/scripts/check-download-retry.sh` does not price. Beside it is the step's
measured duration from a cold cache, which holds the cold build of every release target: 674 s at
most over the 2 runs of the step in the 100 successful runs of this workflow created from
2026-11-19T12:04Z to 2026-11-21T09:04Z (job 136376790855 of run 44762031448; every other run then
found the key and skipped it). Since main's #5141 the step runs on every push, on the family's
newest entry, where it compiles only what changed since that fill, so the cold figure is the most
a run of it takes. 674 + 1050 = 1724 s -> 29 min, under the job's 127.
