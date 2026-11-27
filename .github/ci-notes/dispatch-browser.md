# Notes for the Dispatch browser suite workflow

The derivations and the history of `.github/workflows/dispatch-browser.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that holds the derivation
or the record its step, its job or its key rests on, and the sections stand in the order their
comment blocks stood in the workflow file. A budget's own arithmetic stays beside the budget, in
the workflow file, where the comment that derived it held one.
In the text moved here, "this file" and "this workflow" refer to the workflow,
`.github/workflows/dispatch-browser.yaml`, and "this file's head" to its head comment, which is the
first sections below.

## The suite, its two lanes and the budget rule every job follows

Every browser job of the Dispatch suite, in one place. ci.yaml calls it from three jobs, each
with a literal lane: dispatch-browser-pr with the pull-request lane on a pull request,
dispatch-browser with the full lane in the merge queue, on a dispatched run and on a pull request
labelled `full-ci` or `lane:dispatch_browser`, and dispatch-references with the references lane
on a pull request that changes what the reference rows test; the nightly workflow calls it with
the full lane, under the job id dispatch-browser. Each caller passes its own id as `job`, which
GitHub puts before every job name of this file (`dispatch-browser-pr / shard (1)`) and which
starts every artifact name, so two callers of one run share no artifact. The rows are spread
over four shard machines, the three `-alone` projects get a machine each, the two-process job,
the fake-embedder job and the containerised visual review take one each, and one merge job
joins what the shard and alone legs wrote into a single report and publishes the run's flake
rate.

Two lanes, one input. `pull-request` runs the projects a pull request is held to - `chromium`,
`iphone` and the two stubbed ones - over the rows tagged `@pr` and no others, with the
fake-embedder job, which holds a check of the pull request it is on; `full` runs every
parallel project, the three `-alone` ones, the two-process job, the visual review and that
same embedder job, over every row. So a pull request pays for the rows that answer it, and the
merge queue, a labelled pull request, a workflow_dispatch and the nightly run pay for the rest,
each of which has the wall time for them. A third lane, `references`, is a preview of the
reference rows on a pull request that gates nothing
([its section](#the-third-lane-references)).

A reusable workflow, not a composite action, for the reason docs-site.yaml's head comment
gives: `services:` is a job-level key and a composite action's step keys do not include
timeout-minutes, and this file needs the second, on its steps; no job of it holds `services:` since
its two-process and embedder jobs' Postgres became the harness's own. That is also why the shard and alone jobs repeat
their step lists rather than sharing one - they are kept textually identical where they are the
same, so a reader can diff them.

That reason is a composite action's, and not a script's: a step that calls a script keeps its
own timeout-minutes. So a body the shard and alone jobs really share is a script, written
once and called from both, and that script's head is where what it does and why is written
down. There are two: .github/scripts/dispatch-browser-samples.sh for the memory and CPU
sampler and its two readings, and .github/scripts/dispatch-browser-postgres.sh for the
reading of the stacks' PostgreSQL, which stood as a copy in each job's own step list until a
review read the two together. Two more stood beside them, the Playwright install and the key
its apt archive cache was restored under: those two jobs run in the image this repository
builds now, which carries the three browsers and every host package they need, so both of
those scripts are gone with the steps that called them (dispatch://INFRA-1443).

The budget rule every job and step of dispatch-browser.yaml follows, this file being where it is written down now
that ci.yaml's dispatch job is gone: a step's budget is its measured maximum plus 15 per cent,
rounded up to the next whole minute, with a floor of two minutes; a job's budget is the sum of
its step budgets plus the measured maximum of every step without one. The budgets sit on the
steps and not on the job alone, because a step the runner cancels at its own budget fails that
step and names it (`The action '<step name>' has timed out after <N> minutes.`), where a job
budget cancels the job. So the `if: always()` and `if: failure()` upload steps still run and the
report, the memory samples and the CPU samples still reach the run, which a cancelled job loses.
A job budget is the backstop above the sum of its step budgets, so it never fires first.

One step follows another rule: the shard job's browser step, whose budget is a hang guard set
from the runners' spread rather than from the maximum, because on that step the maximum was a
verdict on the runner. Its 11 minutes, the maximum plus 15 per cent, cut a leg of run 44985914477
whose every row had passed, on a runner that ran the same rows 1.75 times slower than another of
the same minute. Its budget is that factor times the p90 of the leg the time split gives, times
the worst drift a row-time table up to a week old was measured to give, and
[the step's section](#why-the-shard-job-calls-playwright-directly-and-what-its-browser-steps-budget-is)
holds the derivation.

No permissions: block, and that is not what decides this workflow's scopes. A called workflow
runs under the calling job's `permissions:`, and a caller that declares none hands it the
default GITHUB_TOKEN permissions rather than its own workflow's block (GitHub's
reusable-workflow reference, "Supported keywords for jobs that call a reusable workflow").
ci.yaml's dispatch-browser and dispatch-browser-pr jobs each declare what these jobs need:
`contents: read` for every job's checkout, and the merge job's `actions: read` (it reads the
run's jobs through the API) and `checks: write` (the ci-ok report it ends with, which only
those two calls turn on). ci.yaml's dispatch-references job and dispatch-browser-nightly.yaml's
dispatch-browser job each declare the first two: neither call passes `ci-ok-changes`, so the
report step skips there and no check is posted.

## The third lane, references

`references` runs the `chromium` project over every row of the eleven spec files the
reference framework's Verification document names and the nine of the reading list's walk,
with main's parse-count walk beside them, on the four shard machines and the merge job alone,
so a pull request that changes the reference framework gets those rows from CI at its head
rather than from a run by hand. It gates nothing:
.github/ci-notes/ci.md#the-reference-rows-preview-job
says why. The lane's spec files are written in one place, the `references)` arm of
.github/scripts/dispatch-browser-shard-suite.sh, the script the shard job's browser step runs.

Its caller, ci.yaml's dispatch-references job, can share a workflow run with
dispatch-browser-pr, on a pull request that selects both, and passes its own id as `job`, as
every caller does: so its artifacts are `dispatch-references-…` beside `dispatch-browser-pr-…`,
and each merge job reads only its own caller's legs.

## Why GOPROXY falls through to direct

A `|` between proxy entries makes a go command fall through to the next source after ANY error,
where a `,` falls through only after a 404 or a 410 (Go's module reference, "GOPROXY protocol").
proxy.golang.org reset a download mid-stream and turned a Go lane red on a module whose hash
go.sum already holds (sjawhar/legion#2512); with `|direct` the command fetches the same bytes
straight from the source and checks them against go.sum, so this is a second path, not a weaker
check.

## Why the binaries job builds what the suite runs

The Go binaries the browser suite's stacks run: the Dispatch server and the real broker's
three commands, built once here and handed to the shard and alone jobs in one
artifact.

Why the build is a job of this file rather than of each caller: an artifact is visible only
inside the run that uploaded it, and every caller runs this file - ci.yaml's dispatch-browser,
dispatch-browser-pr and dispatch-references jobs and dispatch-browser-nightly.yaml's
dispatch-browser job. A build left in one caller would hand the others' runs nothing, and their
shard and alone jobs would fail downloading an artifact their run never held. The artifact is
`<job>-binaries`, named for the caller, so callers that share a run each download their own.

Why those two jobs do not build the binaries themselves: they run in a container, and
.github/scripts/ci_filter_coverage.py's shell_reason counts a `container:` as something that
can move what a go command compiles, so a go command there would fall out of
check-ci-filters.sh's reduction and be counted as reading every Go file of the repository,
which is the opposite of what the `dispatch_browser` filter is for. So the builds live in
this job, which takes no container, and the suite's own scripts execute what it built
instead of building it (packages/dispatch/e2e/run-server.sh's DISPATCH_E2E_SERVER_BIN and
run-broker.sh's DISPATCH_E2E_BROKER_BIN_DIR).

It carries no `if:` of its own, as no job of a reusable workflow needs one: the caller has
already decided the call. ci.yaml's three callers hold the `dispatch_browser`,
`dispatch_browser_pr` and `dispatch_references` outputs and the merge-group attempt term, and the
nightly calls this file unconditionally, so a condition here could only hold back a run its
caller had asked for.

## The binaries job's budget

Measured over three dispatched runs, jobs 128520516081 (83 s), 128528093378 (84 s) and
128528969380 (69 s), whose build steps took 45, 52 and 56 s. No step here carries a
budget, so the rule in this file's head gives this job the measured maximum of every
step, 84 s, rounded up: two minutes. The job's `timeout-minutes: 10` is larger than that
on purpose and is not that arithmetic. Every run of that sample restored the Go cache; a
run that misses it builds four commands cold, which the planner measured at 3m37s for
./cmd/dispatch alone, and two minutes would cancel it. So the budget carries the cold build
rather than the warm one, as the multiprocess job's Playwright install step, "Install Playwright Chromium", carries
its own retry arithmetic rather than its measured maximum.

## Why the binaries job restores go-envoy-test

go-envoy-test, restored only: this job builds the Dispatch server and the real broker's
three commands, and that family's entry is the one that holds them. go-envoy is filled
by a build of ./cmd/listener and ./internal/testnats/cmd/natsd alone
(.github/workflows/ci-caches.yaml), so a job that restores it and builds these four
downloads and compiles their closure on a key that hit: 115 `go: downloading` lines in
job 129055096326 of run 42585736615, where this build still sat inside the alone job's
3-minute browser step. go-envoy-test is filled by `go test -count=1 -run '^$' ./...` over
the whole envoy module, so it carries every package built here: 4 downloads and a 17 s
restore of 646 MB in job 129071749416 of run 42590831386, against 9 s of 153 MB. No job
here saves either one.

## What the binaries build writes, and why it is static

One `go build -o <directory>/` writes one binary per package, each named after it:
dispatch, dev-oidc-issuer and requestseed; a second build names the broker's, from
./cmd/cipherd, broker. CGO_ENABLED=0 is pinned on the build
rather than left to the runner's default, so each binary is static by construction: the
shard and alone jobs run them in a container of this repository's own image, and a
binary with a PT_INTERP has a glibc there to disagree with. They are written under
$RUNNER_TEMP, so the checkout stays clean.

`-ldflags='-s -w'` leaves out the symbol table and the DWARF debug information, which no
leg reads: a panic's stack trace still names each function, file and line (Go keeps its own
line table for that), and `go version -m` still reads the build information. Without them the
artifact every shard and alone leg downloads was 107,568,042 bytes zipped and 248,108,722
unzipped (run 45062410823); stripped, it was 55,287,947 bytes zipped (run 45081810035), 49%
less, both as the run's artifact list gives `size_in_bytes`. The download is where a leg's run
has failed on the network: on 2026-11-22 five leg downloads of about 4,330 stalled for the
step's whole 2 minutes, and three legs failed when the second attempt stalled too (shard 4 of
merge group 45062410823 among them). Fewer bytes shorten a read whose connection slows; they do
nothing for one whose connection stops, which is the second read's to end and resume (the
section on why the second read of the binaries is a script).

## Why the binaries artifact's name holds no run attempt

One name per caller for the whole run, `<job>-binaries`, with no `github.run_attempt` in it: a
re-run of a run's failed jobs re-runs a shard or an alone leg without this one, and that attempt
must find the artifact this job uploaded on the attempt before.

## The shard job, its stacks and its container

One leg of the lane's rows. Each leg runs the lane's projects with `--shard=N/4` over four
Playwright workers, and the run's `globalSetup` starts one stack per worker, each with a
Postgres, a harness and a file-store bucket of its own (packages/dispatch/e2e/
harness-pool.ts), so this job has no `postgres:` service and no job-level DATABASE_URL:
one database cannot serve stacks that truncate it per row, and a run of more than one
stack starts a server per stack and hands each its own (e2e/harness-postgres.ts).

A leg's quarter is a quarter of the lane's row time, not of its rows. Playwright's own
`--shard` cuts the rows into four runs of equal count, and rows do not cost the same: at main
f02514268a it gave the full lane's heaviest leg 1.115 to 1.225 times the mean row time, over
the per-row medians of two sets of 13 and 14 of main's runs (the pull request's body names them),
and leg 2, all chromium rows, was the slowest leg of its run in 55 of the 85 runs of
2026-11-21/22 whose four legs all ran. packages/dispatch/e2e/shard-reporter.ts, which the
browser step names beside the failure reporter, takes the split over: every leg places every
file of every project the lane selects by the row times main measured
(packages/dispatch/e2e/shard-weights.json), heaviest first onto the lightest leg, and keeps its
own files. The same two sets put that placement at 1.007 and 1.008 times the mean.
packages/dispatch/e2e/shard-split.ts says why Playwright 1.64.1 has no split by time of its own,
why the file is the unit, how all four legs compute one placement, and what a file with no
measured time is placed by; `bun e2e/tools/shard-weights.ts --main <count>` reads the table
again. Each leg prints its own line before its first row, `shard N of 4, placed by measured row
time: ...`, with the rows of it that were placed by an estimate.

Each leg also writes two row lists into the rows/ directory its blob upload carries: every row
of the selection it read, and the rows Playwright gave it to run
(DISPATCH_E2E_LEG_ROWS_DIR in .github/scripts/dispatch-browser-shard-suite.sh). The merge
job's reading one holds the four legs' lists against each other and fails by name on a row of
the selection that no leg ran or that two ran. Its row count cannot see either: a planted
reporter that put the selection's last file on no leg ran 1,616 of the 1,617 rows, and the legs'
count and the merged report both read 1,616.

The image this repository builds and publishes for every CI job that can launch WebKit or
Firefox (.github/docker/browser.Dockerfile, the one `built:` row of .github/ci-images.yaml,
tag browser-398fb2e8): Playwright's own browser host, which bakes Chromium, WebKit and
Firefox and every host package they need under /ms-playwright - where its
PLAYWRIGHT_BROWSERS_PATH points - with PostgreSQL 18 and pgvector, the SeaweedFS `weed`
binary, jq, python3 with PyYAML - which .github/scripts/ci-tools.sh imports to read a row
of .github/ci-tools.yaml, in the second-source step of ./.github/actions/setup-bun, which
runs before any browser step here - flock, a C/C++ toolchain for the dependency whose
install falls back to a source build, and a DejaVu core font added. So this job downloads
no browser and installs no browser host package from Ubuntu's mirror pool at run time
(dispatch://INFRA-1443). The digest is written out as a literal rather than through an
expression because .github/scripts/check-ci-images.sh reads a `container:` image out of
the parsed YAML and refuses one holding a `${{ }}`, which it cannot resolve to a row.

--user 1001 is the image's own pwuser, and the uid the runner owns $GITHUB_WORKSPACE and
every bind mount with: initdb refuses to run as root and refuses a uid with no passwd
entry, so the stacks' clusters need exactly this uid. It is not a sandbox decision: the
suite runs Chromium with `--no-sandbox` at any uid, as it did on the bare runner before
this change, because playwright-core 1.64.1 - the version bun.lock resolves - adds that
argument unless a launch passes `chromiumSandbox: true`
(lib/coreBundle.js, `if (options.chromiumSandbox !== true) chromeArguments.push("--no-sandbox")`),
and nothing under packages/dispatch/e2e or docs/site/media passes it. --ipc=host is
Playwright's own Docker guidance, and what it buys is shared memory and nothing else:
without it the container gets Docker's default 64 MB of /dev/shm and Chromium runs out of
shared memory mid-page. With it /dev/shm is the host's, 7.9 GB, measured on this
container's first runs, so `--shm-size=2g` is not needed beside it. --init gives the
container a pid 1 that reaps the browsers' orphans, which four stacks of them make many of.

## The shard job's shell rule, its measured steps and its budget


This job runs no `go` command, directly or through a script: the binaries job builds
every binary the suite's scripts would have built and hands them over in one artifact. It
has to be that way round, and that job's own header says why:
ci_filter_coverage.py's shell_reason counts a `container:` as something that can move what
a go command compiles, so a go command here would fall out of check-ci-filters.sh's
reduction and be counted as reading every Go file of the repository. For the same reason
this job pins no `defaults.run.shell` and no step-level `shell:`. So every `run:` of this
job is executed by `sh -e {0}`, not bash - measured in job 128496236572, whose every step
logs that shell - and each one must stay POSIX. A script a step calls carries its own
`#!/usr/bin/env bash` and is unaffected.

The stacks' PostgreSQL binaries and this run's worker count are set on the browser step's
own command line, where they have always been, rather than in an `env:`.
That command line is in .github/scripts/dispatch-browser-shard-suite.sh, which the browser step runs.
DISPATCH_E2E_FILE_STORE_ENDPOINT and DISPATCH_E2E_FILE_STORE_BUCKET are in
ci_filter_coverage.py's ENV_ALLOWED and stay in the browser step's `env:`; the
multiprocess job's pinned build step, "Build the Dispatch server", is read under dispatch-browser.yaml's own `env:` and
defaults, so neither may grow a name outside that set.

The census the job's budget is read from, and the browser step's was: main's own full-lane runs, read from the
jobs API on 2026-11-21 near 16:15Z at main 75bd096e02. It takes every run of ci.yaml with
`event=merge_group`, and every run of dispatch-browser-nightly.yaml on branch `main`, that
GitHub's `created=2026-11-18..2026-11-21` filter lists and whose head commit is on main's
first-parent history (`git rev-list --first-parent 75bd096e02`), since the queue merges the
commit a merge-group run tested; every attempt of each; and every leg of each named
`dispatch-browser / shard (N)` or `full / shard (N)`. The queries:

  gh api --paginate 'repos/cybermonklabs/legion/actions/workflows/ci.yaml/runs?event=merge_group&created=2026-11-18..2026-11-21&per_page=100'
  gh api --paginate 'repos/cybermonklabs/legion/actions/workflows/dispatch-browser-nightly.yaml/runs?branch=main&created=2026-11-18..2026-11-21&per_page=100'
  gh api --paginate 'repos/cybermonklabs/legion/actions/runs/<id>/attempts/<n>/jobs?per_page=100'

545 runs listed, 278 of them on main's first-parent history, and 672 legs in those, every one of
which passed. The browser step over them, in seconds:

  created       legs  median  p95  max
  2026-11-18    188    344    448  510
  2026-11-19    176    363    457  493
  2026-11-20    164    404    476  516
  2026-11-21    144    414    507  531
  all           672    380    486  531

The browser step's budget was its `timeout-minutes: 11`, that maximum by the rule in this file's
head: 531 s, shard (2) of run 44917245139 (job 136888588252), and 531 x 1.15 = 611 s -> 11 min.
It is a hang guard now, 18 minutes, read from the legs of 2026-11-21 and 2026-11-22 rather than
from this census's maximum, for the reason
[the step's section](#why-the-shard-job-calls-playwright-directly-and-what-its-browser-steps-budget-is)
gives with its derivation. The step grows with the suite, not with its rows getting slower: run
44423948511 of 2026-11-18 ran 1,453 rows (363, 363, 364 and 363 a leg) and run 44909149810 at
67659e611c ran 1,600 (400 a leg), and the 1,260 rows both runs passed took 4,345 s and 4,393 s,
a ratio of 1.011. At the 9 minutes the 11 replaced, derived from the 419 s of the two runs of the
head that sharded this suite (42334424798 and 42316487530), 42 of the census's legs came within
60 s of the limit, 34 of the 144 of 2026-11-21, and in run 44908680493 of #4613 and run
44903205761 of #4199 three legs whose every row passed were cut at it.

The job budget, the shard job's `timeout-minutes`, follows the rule over the same census: one
window for every step. Each step without a budget of its own is counted at its maximum over
the census's 672 legs, and the image pull, `Initialize containers`, at the larger of that and
the 355 s busy-hour pull below, since no step holds the pull. The maxima, in seconds, with the
job of each (`run/job`):

  step                                                 max   leg
  Set up job                                             2   44917241841/136888799637
  Initialize containers                                176   44495468251/135462529348   (counted at 355, below)
  actions/checkout                                      20   44341608906/134936405641
  actions/download-artifact, first attempt              31   44754421273/136353201422   (carries its own budget)
  actions/download-artifact, second attempt              0   never ran                  (carries its own budget)
  Place the binaries of the attempt that completed       1   44917240344/136887948691
  Make the downloaded binaries executable                1   44917245139/136888588265
  ./.github/actions/setup-bun                            7   44495468251/135462529348
  bun install --frozen-lockfile                         52   44495468251/135462529348
  Build contracts for dispatch e2e                       2   44495468251/135462529348
  Build dispatch dashboard                              45   44430694686/135248042392
  Read the lanes' PostgreSQL and time its provider      10   44495468251/135462529348
  Start an S3-compatible server for uploaded files       1   44917243574/136888280815
  Wait for the S3-compatible server                     35   44495468251/135462529348
  Browser-test dispatch dashboard                      531   44917245139/136888588252   (carries its own budget)
  upload-artifact, this leg's blob report                5   44495468251/135462529269
  What the run left behind                               1   44909149810/136862590255
  upload-artifact, the record directory                  3   44318690162/134858314626
  upload-artifact, the failed rows' traces               8   44727905640/136261054321
  Post Run ./.github/actions/setup-bun                   6   44869329727/136741454057
  Post Run actions/checkout                              5   44759465127/136370617001
  Stop containers                                        5   44490607323/135446360115
  Complete job                                           1   44917238659/136889169686

The three uploads are three steps of this job and not one, so the sum counts each of them.
The steps without a budget sum to 565 s with the pull at 355 s.

The job's budget before this census was 540 s for the browser step plus the maxima of 24 legs
in five runs at the two heads that pinned sha256:ba003193 (CI runs 42481141876 and
42483222992, workflow_dispatch runs 42481269619, 42483357068 and 42487610878 of
dispatch-browser-nightly.yaml), 219 s, then the pull at 355 s, and it left out the contracts
build and the placing of the binaries, which this census counts. A measured table carries the
head it was measured at: each rebuild of the image is other bytes (layers 8 to 10 of the
builds of 47a222cd differ, 344 bytes apart over 1.16 GB), so the census's own legs, on the
digest each pinned, are the reading.

THE TWO NETWORK READS, AT A BUSY HOUR. The sum before this census read the binaries download at 5 s and the
image pull, `Initialize containers`, at 45 s, from quiet runs, and on 2026-11-18 a leg of
each job stalled on one of them while its siblings did not: in run 44228250420 shard (4)'s
download of dispatch-e2e-binaries (106 MB, 101 MiB) took 428 s where the run's six other
legs took 2 to 6 s, and the job budget cancelled the leg at about 13 minutes; in run
44213916667 alone (chromium-alone) spent 355 s pulling this image (1,161 MB) where the two
other alone legs took 38 and 39 s, and its budget cancelled it 3 s into its checkout. The
pull's time was one layer: the other nine arrived in 6 s and the 122 MB Firefox layer in
346 s, and shard (4) of that run, pulling in the same minute, took 200 s on two layers. A
merge group drops its entry for a cancelled leg as for a failed one, and every group behind
it is built again. So the download now carries a budget of its own and a second attempt
(.github/ci-notes/dispatch-browser.md#what-the-shard-job-downloads-in-place-of-a-go-build),
and the pull is counted at that 355 s, above the census's 176 s: no step holds it, so no step
budget can cut it and no step can read it again, and Docker's own retry ("Retrying in 5
seconds" in that log) answers a failed read, not a slow one. The job's budget is the
browser step's 1,080 s, the two download attempts' 120 s each, and the steps without a
budget at the census's 565 s: 1,885 s, 31 min 25 s, rounded up to the 32 minutes
of the shard job's `timeout-minutes`. The browser step's own budget still fires before the job's.

The rows behind those seconds were the suite of the head those runs measured. Listed
again at this change's head, the suite holds 1,427 rows in 194 files, which is two
listings rather than one: the three `-alone` projects are out of the default selection
since dispatch://INFRA-1280, so a command that names no project lists 1,373 rows in 191
files and one that names those three lists the other 54, in 3 files. The ten projects
this job's shard command (.github/scripts/dispatch-browser-shard-suite.sh) names hold 1,353 rows in 190 files; the four of those the
pull-request lane names hold 1,252, of which that lane's `@pr` grep selects 282. The two
`visual` projects hold 20, which the visual job runs one row at a time, and the 54
wall-clock rows are the alone job's.

## Why the leg count stands in two places

Four legs. The count is written here and in the `--shard` denominator of the browser step
(`.github/scripts/dispatch-browser-shard-suite.sh#--shard=$shard/4`), and nowhere else, so the two move together: a denominator larger than the leg list
would leave the rows of the missing legs unrun with nothing red. The split by row time
(packages/dispatch/e2e/shard-reporter.ts) reads the count from that same `--shard`, so it is no
third place.

## What the shard job downloads in place of a Go build

What the binaries job built, in place of this job's own actions/setup-go
and go-envoy cache restore, which are gone: with no go command left here they bought
nothing, and the restore was there only for the build run-server.sh no longer does.
The directory is the one run-broker.sh built the broker's three binaries in, which is
the directory `packages/dispatch/e2e/real-broker.ts#requestseed` points into, so the suite runs requestseed from there; the server
binary rides along in it and is named by path in DISPATCH_E2E_SERVER_BIN (.github/scripts/dispatch-browser-shard-suite.sh).

A BOUND OF ITS OWN AND A SECOND ATTEMPT. A read of this artifact took 2 to 6 s on every
leg but one, which took 428 s (the budget note above), so a slow read is answered by
reading again rather than by waiting. Each attempt carries this file's floor of 2
minutes, twenty times the longest of the other reads. That is not this file's rule of
the measured maximum plus 15 per cent, which the 428 s read would put at 9 minutes: those
9 minutes are the wait this bound is here to cut, since the same bytes took seconds on
every other leg. The second attempt runs only when the first did not end in success, and
a second failure fails the job there, by name. The second attempt is a script and not the
action again; the section after this one says why.

EACH ATTEMPT INTO A DIRECTORY OF ITS OWN. A step that runs past its budget in a
`container:` job ends the `docker exec` that started it, and the action's process inside
the container can outlive it (moby/moby#9098) and go on writing the files it was
extracting. Into a directory of its own, those writes reach nothing a later step reads:
the step after the two copies the attempt that succeeded into the directory
run-broker.sh serves from.

## Why the second read of the binaries is a script

On 2026-11-22, 5 of about 4,330 leg downloads of this artifact stalled for their step's whole 2
minutes (p50 3 s, p99 8 s): alone legs 137037296561 and 137185227791, shard legs 137092255904,
137105073720 and 137332367544. In 3 of them the second attempt, which was
actions/download-artifact again, stalled for its 2 minutes too and the leg failed: two
pull-request runs, and merge group 45062410823, which took #4234 out of the queue.

The stall was the leg's own connection to blob storage, not the artifact service: the
action had its answer's headers (it logs a deprecation warning from the unzip stream it
builds only after them) and then nothing for 120 s, while the other six legs of 45062410823
read the same blob at the same minute in 3 s. The stalls met three storage accounts and two
regions. Nor was it the unzip: the action's unzip-stream 0.3.4, fed that run's 107 MB zip in
random chunk sizes, finished 40 of 40 times in about 3 s on a developer machine.

The action cannot be told to do better. Its download (streamExtract, the same in v8.0.1 and
v8.0.2) ends a connection only after 30 s with no byte at all, starts again from byte 0, and
takes no input for either: a connection that goes slow rather than silent is never ended,
and one that is ended throws away what it read. So the second read is
.github/scripts/dispatch-browser-fetch-binaries.sh, which reads the same artifact by its REST
address with curl: an attempt whose connection stays under 1 MiB a second for 10 s ends
(`--speed-limit`, `--speed-time`), the next one opens a new connection and asks for the bytes
after the last one's (`--continue-at`), and the whole zip is held to the artifact's sha256
digest before it is unzipped. Each attempt prints its bytes, its speed and the TCP segments
the leg retransmitted during it, so the next stall says whether the connection was silent or
slow. Its first read stays actions/download-artifact, which reads in seconds on all but about
one leg in a thousand. The script runs in the step's 2 minutes, with the same directory of
its own as the action's attempt, and reads the artifact the action reads, `<job>-binaries`,
given the calling job's id (`${{ inputs.job }}`): callers that share a run - a pull request
labelled full-ci runs dispatch-browser beside dispatch-browser-pr, and one that changes what the
reference rows test runs dispatch-references beside it - each read only their own build's
binaries. A re-run uploads them again under the same name, and the action takes the highest id
of the run's artifacts of that name (run 45306887784 held three by its attempt 3, and the highest
id was not the newest by `created_at`), so the script takes that same one and reads its address,
size and digest from it.

## What the shard job places, and what its budget leaves for it

The census the job's budget is read from counts this step, at its maximum of 1 s (the
shard job's measured steps above); it copies the four binaries on the runner's disk (141 ms
for the 106 MB artifact of the unstripped build on a pod).

## Why the shard job makes the downloaded binaries executable

actions/upload-artifact keeps no executable bit, so all four arrive at mode 0644.
`packages/dispatch/e2e/run-server.sh#server_bin` and run-broker.sh refuse a path that is not executable, and that
refusal would otherwise surface in a stack's own log, far from this cause.

## Why every job that runs the suite builds contracts first

WHY EVERY JOB THAT RUNS THE SUITE CARRIES THIS STEP. Playwright loads
packages/dispatch/e2e/playwright.config.ts before it runs anything of this suite's own,
and that config imports e2e/api.ts, which imports `@legion/contracts`. Playwright's
runner is Node, which takes that package's `default` export condition - `dist/`, which is
gitignored - so a checkout that has never built it dies while the config loads, with
`Cannot find module @legion/contracts/dist/index.js`. No hook of the suite can be early
enough: `globalSetup` runs after the config has loaded. A workstation gets the build from
`bun run e2e`, which carries `build:contracts`; a job that runs Playwright itself needs
this step. It runs `build:dispatch`, the contracts entries Dispatch imports and no other
(`.github/workflows/ci-job-dispatch-ui.yaml#Build contracts for the suites that spawn Playwright` says
how that list grows), so it compiles no module the dispatch_browser filter's listed
block leaves out - not ./legion-api, which no row loads.

## What the shard job's PostgreSQL reading records, and why it is a script

What the stacks' databases are, and who runs them, read from the same binaries they use
and before they use them: packages/dispatch/e2e/harness-postgres.ts runs `initdb` once
per run and copies the data directory per stack, and the script starts one of those
clusters and asks it. The suite's own stacks leave nothing behind to read afterwards,
since the run stops them and loses their data directories when it ends
(e2e/global-teardown.ts), so the server version and the extension this job's image
carries are recorded here or nowhere. It also prints the `id` this job's steps run as,
which is what initdb stands or falls on, and times what the provider costs, which is the
figure harness-postgres.ts records for itself.

One script, called from here and from the alone job's step of this name, as this file's
head says a body the two jobs share must be: it stood as a copy in each job's step
list, the two differing in nothing but the number of copies they made, until a review
read them together (dispatch://INFRA-1443).
Its head says what each line it prints is for. The argument is this job's stack count,
the `--workers 4` the browser step passes Playwright (.github/scripts/dispatch-browser-shard-suite.sh), so the copy time it prints
is this job's own.

## The file store the shard job starts, and the flags it needs

The file store the browser suite uploads through: the server runs with
DISPATCH_FILE_STORE_BUCKET set (e2e/run-server.sh reads
DISPATCH_E2E_FILE_STORE_ENDPOINT), so every upload and download the dashboard makes goes
through the store, as production's does. CI ran it as a `docker run` of this
repository's copy of the SeaweedFS image; a job inside a container can neither start a
container nor reach one, so that image's `weed` binary is baked into this job's image
(.github/docker/browser.Dockerfile) and started here, in the background, on the same
127.0.0.1:18333. Its data directory and its log go under $RUNNER_TEMP, which the run
keeps and the checkout never sees.

The three flags after `-dir` are the image's, not weed's own defaults, and they come
with the binary because its entrypoint does not: that image's /entrypoint.sh adds
`-volume.max=0 -master.volumePreallocate -master.volumeSizeLimitMB=1024` to every
`server` it runs, and copying the binary out left them behind. `-volume.max=0` is the
one that bites. Without it a volume server takes 8 volumes and no more, the master
grows seven for the first bucket it is asked about, and every later bucket is refused
with `topo failed to pick 1 from 0 node candidates` and
`create 7 volume, created 0: Not enough data nodes found!`. The suite makes one bucket
per stack, so the readiness wait's own bucket worked and ten upload rows of run
42409479235 failed on 502s from the Dispatch server's PutObject, every one of them for
a stack's own bucket.

The two flags after those, `-master.raftHashicorp -master.raftBootstrap`, are this job's own.
A one-node master on weed's default raft elects itself only after about 19 s: the log of
run 45048924209's shard 4 has "Starting RaftServer" at 17:33:08.6 and "is the leader" at
17:33:27.5, and the S3 gateway answers only after that, so the readiness wait below took
23 s at the median of 3,247 shard legs on 2026-11-22 (p90 24 s). With HashiCorp's raft and
a bootstrap of the empty data directory each run starts with, the master is leader within
about a second of its start. Measured with the weed 3.97 binary from that release on a
loaded developer machine, start to an object written and read back: 31 to 37 s with the
default raft, 10.5 to 13 s with these two flags, leader at 1.4 s. Neither flag changes
what the store keeps or serves: the data directory is new in every run, so there is no
earlier raft state to resume.

So, for whoever moves the seaweedfs row of .github/ci-images.yaml to a new version:
read that image's /entrypoint.sh again and reconcile the three image flags with its `server`
case, because they are the script's and this job runs the binary without it. The row
pins a digest, so the entrypoint cannot change under this job between bumps, which is
what makes the rule a read at bump time rather than a check on every run. Read it with
`crane export ghcr.io/cybermonklabs/legion/ci@<the row's digest> - | tar -xO entrypoint.sh`.

## Why the store's readiness wait writes, reads and deletes an object

The readiness wait of the step that started it. An anonymous PUT makes the bucket, as
each stack makes its own (dispatch-e2e-files-worker-N) with the same request
(e2e/harness-pool.ts). Then an object is written, read back and deleted, because a
bucket PUT is answered by the filer alone and says nothing about the volume server the
objects land in: run 42404500479's job 128499619657 passed this wait with a bucket and
then failed ten upload rows nineteen minutes later, every one of them a 502 from the
Dispatch server's own PutObject. A store that cannot take an object now fails here, by
this step's name, with the log the "Start an S3-compatible server for uploaded files" step redirected -- which is what a job in a
container has in place of `docker logs`.

## Why the shard job calls Playwright directly, and what its browser step's budget is

`bun run e2e` builds the SPA itself, which would rebuild it a second time in this job.
Calling Playwright directly reuses the "Build dispatch dashboard" step's dist and
drops this job to one Vite build. Safe because that build and this one are now the same
binary: `packages/dispatch` no longer has an E2E-only build flag (removed with its sole,
unconsumed consumer, the `window.__dispatchDocument` inspection probe). The run starts
its own servers: `globalSetup` starts this run's stacks on ports the OS chooses and
writes them into the environment every worker inherits, and `globalTeardown` stops them
(e2e/global-setup.ts, e2e/harness-pool.ts). The contracts step, "Build contracts for dispatch e2e", is what
Playwright's own Node runner needs, for the reason that step gives.

The browser step's body, its command, flags and sampler included, is .github/scripts/dispatch-browser-shard-suite.sh.

`.github/scripts/dispatch-browser-shard-suite.sh#--workers 4` is how many stacks this leg starts, one per Playwright worker, each
a harness, a bucket and a Postgres of its own, that last one an initdb and a pg_ctl out
of DISPATCH_E2E_PG_BIN (e2e/harness-postgres.ts), this image's PostgreSQL 18
with pgvector. They are set on the command line, not in this step's `env:`, for the
reason the job's head gives.

The memory and CPU sampler both this job and the alone job run is
.github/scripts/dispatch-browser-samples.sh: MemAvailable and where the CPU goes,
every 5 s for the length of this step, started by .github/scripts/dispatch-browser-shard-suite.sh and read back at the end of it.
Its head says what each reading holds and what it does not; on this job `everything
else` holds the file store too, which runs beside the stacks. Both sample files are
written into the record directory that the step "What the run left behind" collects and
the upload after it uploads, under
$RUNNER_TEMP beside the stacks' own run directory: a file this step wrote into the
checkout would leave it dirty afterwards. The blob report goes beside the list
reporter's output, so a green run reports what it ran too.

The step's budget, `timeout-minutes: 18`, is a hang guard. It is not the maximum plus 15 per
cent of this file's head, because on this step that rule made the limit a verdict on the
runner: the 11 minutes it gave cut shard (4) of run 44985914477 (job 137096923636) with 401 rows
passed and none failed, on a machine that ran them slower than another of the same minute.
How the 18 minutes are read:

- The legs: every shard leg whose browser step ran to an end, of the merge-group runs of
  ci.yaml whose head holds 4f5bf22eb9 and the nightly on main, from 2026-11-21 17:51Z to
  2026-11-22 08:25Z, read from the jobs API of each run's attempts: 347 legs.
- What a leg's step time is: over the 68 legs of the 17 of those runs that are on main's
  first-parent history and whose merged report holds the same 1,617 rows as main at
  f02514268a, the step took 24.0 s + 0.2549 x the leg's row time (its rows' durations in that
  run's report, summed), with a residual of 6.8 s standard deviation and none past 18.7 s: the
  row time spread over four workers, plus the stacks' start.
- The leg the time split gives: each of the 347 legs rescaled to it, 24.0 + (t - 24.0) x
  1,698 / R, where R is the median row time of its leg under the count split (1,739, 2,062,
  1,383 and 1,607 s) and 1,698 s a quarter of their sum. Median 454 s, p90 534 s, maximum 696 s,
  the last the leg the 11 minutes cut.
- The factor: 1.75, the slowest runner seen. The leg that was cut ran its 401 rows at a
  per-row median 1.75 times the time job 137096931568 of run 44985911309 took for the same rows,
  started 3 s later (p10 1.42, p90 2.12; 660 s against 391 s of step).
- The drift: 1.11, the worst share of the mean the heaviest leg took under a row-time table
  (packages/dispatch/e2e/shard-weights.json) up to 7 days old. Read on 2026-11-22 with this
  change's own code - each table built by `packages/dispatch/e2e/tools/shard-weights.ts#weightsOf` from the
  `report.json` of each run's `dispatch-browser-report-1` artifact, the lane placed by
  `placeUnits` - over two tables:
  - a table of 2026-11-18, 4 days old, from ci.yaml merge-group runs 44409818750, 44418436030,
    44418744571, 44418795005, 44419183723, 44423948511, 44424277368 and 44424671868, placed the
    lane at 1,765, 1,520, 1,616 and 1,890 s: 1.113 times the mean;
  - a table of 2026-11-15, 7 days old, from runs 43742873016, 43756837053, 43760034112,
    43770938377, 43771792014 and 43774018034 (the six of that day's last ten on main's
    first-parent history that kept a report), at 1,692, 1,723, 1,712 and 1,665 s: 1.015;
  - the lane is the 1,617 rows the shard command lists at main f02514268a, each weighed by its
    median over main's 17 runs 44955285815, 44955287410, 44963209611, 44966750516,
    44966794515, 44966795777, 44966798236, 44973942358, 44979022185, 44979030337,
    44980842865, 44981988875, 44982147987, 44983017182, 44989017422, 44991587611 and
    44997869666; a table of those same runs places it at 1.001.

  The drift is not a function of age: the older table did better. Each old table lacks the
  files the issue page's specs were split into since (155 and 173 of the lane's rows), places
  them by the chromium median, and the greedy placement turns on small differences of weight,
  so where those files land is close to chance. 1.11 is the worse of the two. Without it, a p90
  leg on the slowest runner seen under such a table would end at about 1,040 s, past a 16-minute
  guard, and be cut with its rows passing.
- 534 s x 1.75 x 1.11 = 1,037 s, rounded up to 18 minutes, 1,080 s.

What a hang looks like to it. A row cannot hang it: each row ends at its own budget, the 60 s of
`packages/dispatch/e2e/playwright.config.ts#testTimeout` or the longer one a row sets, and the leg goes on. What
reaches 18 minutes is a leg that has stopped running rows - a stack that never comes up in
`globalSetup`, a teardown or a blob write that never returns, a worker that stops reporting -
whose log has stopped gaining rows. The runner cancels the step and names it (`The action
'<step name>' has timed out after 18 minutes.`). The leg's blob report is never written, since
Playwright writes it when the run ends: the always-run upload finds no file (run 44985914477's
cut shard 4 logged `No files were found with the provided path: /__w/_temp/leg-report`), and
the merge job says so by name - reading one's `no blob report arrived from: shard-4` and
reading three's `... uploaded no blob report, so the merged report holds none of its rows` - and
fails on the leg's result. No row's verdict rests on the guard: the p90 leg on the slowest
runner seen, under a table up to a week old, ends at about 1,037 s, under it, and the slowest of the
347 legs, rescaled, at 696 s. What it costs is time: a hung leg is cut seven minutes later than
at the 11.

The week is what the guard holds, so each leg says when its table is past it: below its
placement line it prints a `::warning::` when e2e/shard-weights.json was read more than 7 days
before the run, or when a tenth of its rows sit in files the table has no entry for
(`packages/dispatch/e2e/shard-split.ts#tableWarnings`). Either one means
`bun e2e/tools/shard-weights.ts --main 17` is due; past the week, at the count split's 1.21, a
p90 leg on that runner would end near 1,131 s.

## Why the file store's address is set on the step and not in the environment

The file store the "Start an S3-compatible server for uploaded files" step started. Set here, not written to $GITHUB_ENV, which
every later step would inherit, and which check-ci-filters.sh treats as reaching a
job's go commands unseen.

## Why the shard job's blob upload is always, and takes one directory

This leg's half of the run's report. Always, not on failure alone: the merge job reads
every leg's blob, and a leg whose blob is missing fails it by name.

One directory, not the two-line path this upload carried before the container: only the
first line of a multi-line path is translated into the container's own filesystem - a
`${{ runner.temp }}` is the runner's `/home/runner/work/_temp` where the job writes the
container's `/__w/_temp` - and upload-artifact reported `there will be 1 file uploaded`
for a four-line path whose four files all existed (runs 42407623876 and 42409479235).
The browser step writes blob-report/ and rows/ into one directory instead, so the
artifact holds the same two entries under the same names, which is what
.github/scripts/dispatch-browser-latest-blobs.sh and the merge job read.

## What the shard job collects after a run

What the run left behind: in the job's own log, where a reader of a failed row looks
first, and collected into the one directory the `${{ inputs.job }}-record-shard-${{ matrix.shard }}-${{ github.run_attempt }}` artifact takes. A failed row's
trace says what the browser saw; these say what its stack's own servers did, which is
where a stack-only failure (a port, a migration, a server that never came up) is legible.
CI read `docker logs dispatch-e2e-files` for the store before this job ran in a
container of its own; the store's log is a file now, and the stacks' own servers write
beside it. The sampler's two files are already in this directory, which is why the
same artifact carries them in place of the samples artifact this job uploaded before.

The log glob is the run directory e2e/harness-pool.ts made under $TMPDIR, which the browser step
(.github/scripts/dispatch-browser-shard-suite.sh) pointed at $RUNNER_TEMP; mkdtempSync gives the directory a random suffix and each
stack writes under `worker-<its index>/log`, so the glob matches them however many
stacks ran.

Always, not `if: failure()`: the config retries a failed row once on CI
(`retries: process.env.CI ? 1 : 0`), so a flaky row passes on its retry and this leg
ends green, and the memory and CPU readings are of the run that passed as much as of the
one that did not.

## Why the shard job uploads the failed rows' traces always

The traces of the rows that failed, which the "What the run left behind" step does not collect: they are
under the checkout, so this path needs no translation and stays as it was. Always, for
the reason that step gives - `if: failure()` would upload nothing of a row that failed
and passed on its retry, the trace the merge job's flake reading exists to explain among
it. The trace it carries is of the attempt that failed: the config's
`trace: 'retain-on-first-failure'` records the first attempt and keeps it when that
attempt failed. A green run's upload is small.

## The alone job, and what it keeps the same as the shard job

The three projects whose rows are read from the wall clock, one machine each. Each leg runs
one worker and names one project, and a wall-clock project takes nothing else with it - it
names no `dependencies` since dispatch://INFRA-1280 - so nothing else is on the machine
while it measures. The full lane alone: a pull request runs no clock row, since a shard
job's machine cannot hold one still enough to measure.

Its steps are the shard job's, with three differences - one worker rather than four, the
Playwright command of .github/scripts/dispatch-browser-alone-suite.sh, and no file store on the machine - and their commands are kept
the same everywhere else, so the two can be diffed; the two bodies that are really shared
are scripts both jobs call, the sampler and the PostgreSQL reading, which is part of what
keeps them so. They are not one composite action for the reason this file's head gives.
The container, the `sh -e {0}` rule and the `env:` rule of the shard job's head hold here
too, and so do their reasons.

## The alone job's measured steps and its budget

This job takes the container because its matrix holds webkit-alone and firefox-alone,
which launch the two engines the image exists for; chromium-alone is a leg of a container
job and rides in it, which is what keeps one job list and not two
(dispatch://INFRA-1443). The shard job's head says what each option is for and why the
digest is a literal.

The browser step's budget, its `timeout-minutes: 3`, is measured, on the full-lane run of the head that sharded
this suite, 42316487530. The pull-request lane runs no leg of this job, so there is one
run and three legs behind each figure, in seconds:

  step                                    chromium-alone  firefox-alone  webkit-alone
  Browser-test dispatch dashboard                    116            111           141
  every other step summed                             74             72            82

The rule is in this file's head. The browser step's maximum is webkit-alone's 141 s, so
141 x 1.15 = 162 s -> 3 min. Three legs of one run is a thin reading, and these are the
projects whose rows are read from the wall clock, so a slower machine moves them further
than it moves a parallel row. The 15 per cent the rule adds is what carries that, and a
leg that outran it fails the browser step by name, which is a reading of its own.

The job budget, the alone job's `timeout-minutes`, is the rule's arithmetic, read on sha256:ba003193 and not on the
digest this file pins, for the reason the shard job's head gives, which also says why a
reading taken on that build carries across the rebuilds after it: the figures it replaces
were read at 99bdf09cf435, which pinned the image from before build-essential was added to it
(dispatch://INFRA-1443), over three legs of one run. The two steps that moved most are
`./.github/actions/setup-bun`, 2 -> 20 s, and `Initialize containers`, 38 -> 45 s, the
larger image to pull; neither carries a budget of its own, so both land in the sum that
is this job's whole margin, and the old one left it below what the rule gives.

This reading is the jobs API over every leg of this job at the two heads that pin
sha256:ba003193, every attempt: nine legs, the three legs of the full lane in each of
workflow_dispatch runs 42481269619, 42483357068 and 42487610878 of
dispatch-browser-nightly.yaml. The pull-request lane runs no leg of this job, so CI runs
42481141876 and 42483222992 contribute none. The maximum each step reached over those
nine legs, in seconds:

  step                                                 max
  Set up job                                             2
  Initialize containers                                 45
  actions/checkout                                       9
  actions/download-artifact                              5
  Make the downloaded binaries executable                0
  ./.github/actions/setup-bun                           20
  bun install --frozen-lockfile                          9
  Build dispatch dashboard                              35
  Read the lanes' PostgreSQL and time its provider       3
  Browser-test dispatch dashboard                       69   (carries its own budget)
  upload-artifact, this leg's blob report                2
  What the run left behind                               0
  upload-artifact, the record directory                  2
  upload-artifact, the failed rows' traces               1
  Post Run ./.github/actions/setup-bun                   1
  Post Run actions/checkout                              1
  Stop containers                                        1
  Complete job                                           1

The three uploads are three steps here too, and the sum below counts each of them:
2 + 2 + 1, not 2.

The browser step's own 3 minutes above still holds: 69 s is the most it took over these
nine legs, webkit-alone's again, under the 141 s that budget was derived from, so the
pooled maximum the rule reads is still that 141 s.

The browser step was the one step of this job that carried a budget, so the job's budget was
that budget plus the maxima above of every step without one, which summed to 137 s:
180 + 137 = 317 s, 5 min 17 s, rounded up to 6 minutes.

The shard job's note "THE TWO NETWORK READS, AT A BUSY HOUR" holds here too, and one of its
two legs is this job's: in run 44213916667 alone (chromium-alone) spent 355 s pulling the
image, and the 6-minute budget cancelled it 3 s into its checkout. The download carries the
same budget and second attempt as there, and the pull is counted at that 355 s. The job's
budget is the browser step's 180 s, the two download attempts' 120 s each, and the steps
without a budget at 137 - 5 - 45 + 355 = 442 s: 862 s, 14 min 22 s, rounded up to the 15
minutes of the alone job's `timeout-minutes`, whose 38 s left over hold the new step that places the binaries
(.github/ci-notes/dispatch-browser.md#what-the-shard-job-places-and-what-its-budget-leaves-for-it).
The browser step's own budget is unchanged, and it still
fires first.

## What the alone job downloads in place of a Go build

What the binaries job built, in place of this job's own actions/setup-go
and go-envoy cache restore, which are gone: the shard job's step of this name says why,
and this job runs the same two scripts from the same directory.
The shard job's steps of these names say why each attempt carries its own budget and
directory and why there is a second; the four steps are the same here.

## Why the alone job makes the downloaded binaries executable

actions/upload-artifact keeps no executable bit, so all four arrive at mode 0644.
`packages/dispatch/e2e/run-server.sh#server_bin` and run-broker.sh refuse a path that is not executable, and that
refusal would otherwise surface in a stack's own log, far from this cause.

## Why the alone job builds contracts

The shard job's step of this name, for the reason it gives: Playwright's Node runner
reads `@legion/contracts` out of a build this checkout has to make before the config
loads, and the entries Dispatch imports are all it needs.

## What the alone job's PostgreSQL reading records

The shard job's step of this name, on this job's one stack: what the image's PostgreSQL
is, who runs it and what the provider costs, read from the same binaries the stack uses.
That step says why it is read here or nowhere, and the script it calls says what each
line it prints is for. The argument is this job's stack count: one copy rather than
four, since this job runs one worker.

## Why the alone job runs one stack and no file store, and what its browser step's budget is

`bun run e2e` builds the SPA itself, which would rebuild it a second time in this job.
Calling Playwright directly reuses the "Build dispatch dashboard" step's dist and
drops this job to one Vite build. Safe because that build and this one are now the same
binary: `packages/dispatch` no longer has an E2E-only build flag (removed with its sole,
unconsumed consumer, the `window.__dispatchDocument` inspection probe). The run starts
its own servers: `globalSetup` starts this run's stacks on ports the OS chooses and
writes them into the environment every worker inherits, and `globalTeardown` stops them
(e2e/global-setup.ts, e2e/harness-pool.ts). The contracts step, "Build contracts for dispatch e2e", is what
Playwright's own Node runner needs, for the reason that step gives.

The browser step's body, its command, flags and sampler included, is .github/scripts/dispatch-browser-alone-suite.sh.

One stack here, not four: it is one per Playwright worker, and this leg runs one
worker, which the command of .github/scripts/dispatch-browser-alone-suite.sh says in
`--workers=1` rather than leaning on the
configuration's own default of the same number (`workers: 1`,
e2e/playwright.config.ts). These projects' rows are read from the wall clock, and
three other stacks on the machine would be in the reading. That stack is a harness and
a Postgres of its own, that last one an initdb and a pg_ctl out of
DISPATCH_E2E_PG_BIN (e2e/harness-postgres.ts), this image's PostgreSQL 18 with
pgvector; the binaries directory is set on the command line, not in this step's
`env:`, for the reason the job's head gives.

No file store on this machine, and no DISPATCH_E2E_FILE_STORE_* names: every spec these
legs run is a clock spec, and no clock spec uploads or downloads anything.
chromium-alone runs both wall-clock specs (`packages/dispatch/e2e/configurations.ts#clockSpecNames`:
heavy-bodies and nested-directive, the list `clockSpecPattern` is built from, which is
that project's `testMatch`), firefox-alone and webkit-alone run heavy-bodies alone.
And `packages/dispatch/e2e/harness-pool.ts#stackBucket` makes no bucket at one stack: it
answers `undefined` on `count === 1`. A store serving nothing would be charged
to the very reading these rows exist for, which is why the `weed` the image carries is
started on the shard job's machines and not on these. A clock spec that later uploads
fails here by name rather than quietly.

The memory and CPU sampler both this job and the shard job run is
.github/scripts/dispatch-browser-samples.sh: MemAvailable and where the CPU goes,
every 5 s for the length of this step, started by .github/scripts/dispatch-browser-alone-suite.sh and read back at the end of it.
Its head says what each reading holds and what it does not; on this job `everything
else` is also the reading that says the machine was quiet, which is the claim these
rows rest on. Both sample files are written into the record directory that the step
"What the run left behind" collects and the upload after it uploads, under $RUNNER_TEMP
beside the stack's own run directory: a file
this step wrote into the checkout would leave it dirty afterwards. The blob report
goes beside the list reporter's output, so a green run reports what it ran too.

The step's budget, `timeout-minutes: 3`, is the 3 minutes the job's head derives from the sharded head's own
run, and the containerised reading taken on sha256:ba003193 does not move it: the most
this step took over the nine legs the job's head lists is 69 s, well under
webkit-alone's 141 s there, so the pooled maximum the rule reads is still 141 s.

## Why the alone job's blob upload is always

This leg's half of the run's report. Always, not on failure alone: the merge job reads
every leg's blob, and a leg whose blob is missing fails it by name. One directory, as
the shard job's upload of this name is and for the reason it gives.

## What the alone job collects after a run

What the run left behind: the stack's own server logs, in the job's own log and collected
into the one directory the `${{ inputs.job }}-record-alone-${{ matrix.project }}-${{ github.run_attempt }}` artifact takes, beside the sampler's two files, which
the browser step already wrote there. No file store runs on this machine, so there is no
store log to collect.

Always, not `if: failure()`: a green leg's logs are worth as much as a red one's here,
since they say what the stack's servers did while the clock was read, and the memory and
CPU readings are also what says the machine was quiet while it measured. These three
projects set `retries: 0` of their own (e2e/playwright.config.ts), so no row of this job
is retried and none is reported flaky.

## Why the alone job uploads the failed rows' traces

The traces of the rows that failed, which the "What the run left behind" step does not collect: they are
under the checkout, so this path needs no translation and stays as it was. A row that
fails fails the leg, and the trace it carries is of its one attempt, which the config's
`trace: 'retain-on-first-failure'` keeps. A green run's upload is small.

## What the two-process job runs

The browser suite's two-process mode (packages/dispatch/e2e/harness-processes.ts): two
envoy-dispatch processes against one Postgres, behind e2e/round-robin-proxy.ts, B serving a
second build of the dashboard, which is what a rolling release puts in front of a browser. It
runs only the *.multiprocess.e2e.ts specs, in Chromium, in a job of its own so the default
suite's job does not wait on a second Vite build and two more servers.

That Postgres is the harness's own, a `docker run` of the image the job's "Pull the multiprocess stack's Postgres image" step pulls, per Playwright run
(e2e/harness-postgres.ts), as the visual job's is, and not a `services:` container. The
harness starts its servers with crash safety off, so no commit waits for the runner's disk.
With it on, merge-group run 44456291758 (job 135332117623) failed here: the disk stopped
answering for about 5 s, the read mark a closed page left in flight could neither commit nor
be cancelled, and the next row's reset could not lock `user_document_reads` within its 5 s. A
service can be given those three settings too, on its `command:` line, as docs-site.yaml's is
(.github/scripts/postgres-service-settings.sh); this job takes the harness's server rather than a second copy of its
settings, and that server loads the stall library below as it starts, which a service,
started before any step has built the library, could do only after a restart. The suite's
row e2e/read-marker-disk-stall.multiprocess.e2e.ts stalls the disk on purpose, through the
library the harness preloads where DISPATCH_E2E_PG_STALL_DIR names a directory
(e2e/disk-stall.ts), and holds this job to the same reset.

## The two-process job's measured steps and its budgets

The budgets of the install, dashboard and sign-in steps, and the job's `timeout-minutes` from them, are
measured (the connections step's is the floor, as the end of this section says): GitHub's
jobs API over the 397 of the newest 400 ci.yaml runs that could be read, created between
2026-11-05T04:52:45Z and 2026-11-05T21:06:27Z (268 pull_request, 120 merge_group, 12
push), over the 335 jobs of this name that concluded success. That window is before
cybermonklabs/legion#2771 took ci.yaml's push trigger away, which is why it holds push runs
and a window measured today would hold none; the durations are of the same steps either way.

  step                                                          median   95th    max
  Install Playwright Chromium                                     20 s   34 s  339 s
  Browser-test dispatch dashboard against two server processes   102 s  188 s  202 s
  Browser-test a sign-in across two server processes              11 s   12 s   13 s
  every other step summed                                         68 s   94 s  135 s
  the whole job                                                  207 s  305 s  516 s

The rule, and why the budgets sit on the steps rather than on the job alone, are in this
file's head; this job states only what it measured and what that rule gives here. 202 s x
1.15 = 232 s -> 4 min for the dashboard step, 13 s x 1.15 = 15 s -> the two-minute floor
for the sign-in step, and 4 min for the install step, for the reason
.github/ci-notes/dispatch-browser.md#why-the-chromium-install-takes-no-apt-and-no-cache-and-what-its-budget-is gives.
The job budget is the sum of its step budgets plus the measured maximum of every step
without one, rounded up: 4 + 4 + 2 min plus 135 s = 13 min, until the job's Postgres moved
from a `services:` container to the harness's own (.github/ci-notes/dispatch-browser.md#what-the-two-process-job-runs says why). That added
two steps: the image's pull, which has no budget and counts at the 120 s the visual job
counts its pull of the same digest at (the time a browser step spent before its stack's
first output on the run that found the need; the pull took 13 s in run 44480885813), and the
connections read, which takes the two-minute floor until runs of it are measured. 4 + 4 +
2 + 2 min plus 135 s and 120 s = 16 min 15 s -> 17 min. The dashboard step now also starts
the stack's container and builds the stall library, and its four runs since took 118 s,
114 s, 96 s and 106 s (runs 44480885813, 44501665975, 44528268121 and 44549420195), inside
the 202 s its 4 min derives from. With budgets on the steps, the
`if: failure()` upload of `${{ inputs.job }}-multiprocess-test-results-${{ github.run_attempt }}` still runs, which a cancelled job skips.

The move into this workflow changed none of that: the job runs the same steps on the same
machine, and full-lane run 42316487530 falls inside every figure above - 9 s install,
191 s dashboard, 12 s sign-in, 96 s in the steps without a budget, 313 s whole. The first
full-lane run after this job's Go cache restore moved from go-envoy to go-envoy-test,
42614920253 (job 129152410448), is inside them too - 9 s install, 106 s dashboard, 8 s
sign-in, 96 s in the steps without a budget, 219 s whole - and that restore reads
go-envoy-test-Linux-go1.27.3, 677,668,128 bytes, 22 s of those 96.

## Why the two-process job restores go-envoy-test

go-envoy-test, restored only, for the reason the binaries job's step of this name gives:
this job builds the Dispatch server and the dev OIDC issuer, which only that family's
entry holds. It saves none.

## Why the two-process job builds contracts

The shard job's step of this name, for the reason it gives: Playwright's Node runner
reads `@legion/contracts` out of a build this checkout has to make before the config
loads, and the entries Dispatch imports are all it needs. `bun run e2e:multiprocess`
in the "Browser-test dispatch dashboard against two server processes" step makes the same build again (packages/dispatch's build:contracts).

## Why the two-process job builds the Dispatch server itself

run-server.sh builds it too, inside the harness's boot wait for each server
(e2e/harness-start.ts, bounded by the listener's bootMs); built here, both servers find it
current and start in seconds.

## Why the Chromium install takes no apt and no cache, and what its budget is

No apt in this step at all. Playwright's `chromium` entry in its ubuntu24.04-x64 table
adds no package this GitHub ubuntu-24.04 image lacks, so `--with-deps` installed only
Playwright's `tools` list: the CJK, Thai and Cyrillic font packages and xvfb. Measured
at the merge base in job 127214381394, that was `0 upgraded, 9 newly installed`, `Need
to get 21.1 MB of archives`, plus 12.8 MB of package indexes in `apt-get update`. All
of that is gone, and with it this step's removal of the runner image's chrome-stable
apt source, which it carried only because that source's index intermittently failed
`apt-get update`.

The fonts are not needed: this job's specs are every *.multiprocess.e2e.ts there is --
coedit, cross-build, read-marker-disk-stall, signin and two-builds today
(`packages/dispatch/e2e/playwright.config.ts#multiprocessSpecs` matches them by that suffix, so a new
one joins without an edit here) -- and they run
headless Chromium and compare no screenshot (none holds `toHaveScreenshot` or
`toMatchSnapshot`), and fonts-liberation and fonts-noto-color-emoji are on the image
already. A row that later needs CJK or Thai glyphs would fail visibly.

The 4-minute budget: this step downloads browsers only, and Playwright makes up to
five attempts, across its download hosts, inside it. Of the 339 s maximum above,
measured in job 127214381394, 317 s were the Ubuntu mirror serving 21.1 MB at
66.4 kB/s, which this step no longer asks for; Chromium, its headless shell and
ffmpeg downloaded in 6 s there. Healthy today is a 20 s median and a 34 s 95th
percentile for the whole step.

No browser cache here: it would save about 4 s, since the three archives - Chrome for
Testing, its headless shell and ffmpeg - download in 8 to 9 s, and it would add about
250 MiB to a cache store already holding 18.6 GB in 162 entries, where each existing
playwright-Linux- entry is 481 MiB. There is no browser cache anywhere in this
workflow to join: the shard and alone jobs restored one until they moved into the
image that carries the browsers, which took their restores and ci-caches.yaml's
browsers job with them (dispatch://INFRA-1443). This job stays on the runner, because
nothing can make it launch another engine, and so keeps this one download.

## Why the two-process job pulls its stack's Postgres image

The image the harness starts each Playwright run's Postgres from, pulled here so the
browser steps of its job after it do not pay for it inside their own budgets, as the visual job pulls
it. The digest is the browser steps', written three times, and the three must stay the
same: a pull of another digest leaves the browser step to pull its own. Each new step's
name carries the lane, as the embedder job's build step's does, for the reason
.github/ci-notes/dispatch-browser.md#why-the-embedder-jobs-server-build-carries-the-lane-in-its-name gives.

## Why the two-process job reads its stack's connection limit in a step of its own

The stack's connection limit, read outside the browser steps' budgets, where
e2e/harness-postgres.ts would otherwise compile it (DISPATCH_E2E_PG_CONNECTIONS), as the
visual job reads it. Two processes fit in it: it is testlimits.FullSizeServerConnections,
sized for eight (testlimits.MaxProcessesAtOnce), with the server's superuser reserve on top.

## What the two-process dashboard step runs against

Builds the dashboard twice (A's and B's, with distinct build ids) and runs the
multiprocess specs through the round-robin proxy, against one Postgres the harness
starts from the image the "Pull the multiprocess stack's Postgres image" step pulls. DISPATCH_E2E_PG_STALL_DIR is the directory the
harness builds e2e/fsync-stall.c into and preloads into that server, which
e2e/read-marker-disk-stall.multiprocess.e2e.ts needs: on GitHub Actions
(GITHUB_ACTIONS=true) the row fails without it, and elsewhere, an agent's shell
included, it is skipped. The runner's own cc builds it.

## What the sign-in row across two processes reads

Both processes sign people in through the dev OIDC issuer (DISPATCH_E2E_SIGNIN=oidc,
e2e/harness-signin.ts), and its one spec starts a sign-in on A and completes it on B. The
step before built the dashboard; B serves A's build, which this spec does not tell apart.
Its Postgres is a second one the harness starts from the same image.

## Why the two-process job's upload runs on failure alone

`if: failure()`, which this job can afford where the shard job cannot: its project sets
`retries: 0` (e2e/playwright.config.ts), so a row that fails fails this job and this
upload runs. The shard job's copy is `if: always()` because a retried row there can pass
and leave the job green.

## The embedder job, the lanes it runs on and its budgets

The rows that read what the bar costs a model, which no other lane runs. The two tiers of
the command bar are a claim about embeddings - a keystroke makes none and a pause makes
exactly one - and the only thing that can read a count back is the fake embedder
(e2e/fake-embedder.ts) the server calls as it calls Bedrock, under DISPATCH_E2E_EMBEDDER=fake
(e2e/harness-embedder.ts). That switch is the server's, chosen before Playwright starts, so
no project of another lane can carry it and e2e/search-tiers.e2e.ts skips everywhere else.
This job is where it runs.

On the pull-request and full lanes, where the alone and multiprocess jobs run on `full`
alone: this is one Chromium install and two rows of about 90 s, and it holds a check of the
pull request it is on (check k2 of the embedding bar's issue, the bar's embedding count) rather
than a clock reading or a second server shape. A lane that skipped it would leave the claim
unread until the merge queue. Not on `references`, which its `if:` holds it off: that lane
runs the spec files its arm of .github/scripts/dispatch-browser-shard-suite.sh names, and
e2e/search-tiers.e2e.ts is none of them, and this job's check stays the pull-request lane's,
which runs it.

Its rows upload no blob report, as the multiprocess job's do not: the merge job joins the
shard and alone legs, and this job's own conclusion is what the calling job (ci.yaml's
dispatch-browser or dispatch-browser-pr) reads. It is in the merge job's `needs` and in that job's name filter, where it gets a
null leg and is passed over by reading one, exactly as multiprocess is.

No budget measured on this file's own runs yet, so the embedder job's two step budgets are derived from the
multiprocess job's measured steps, which are the same steps on the same machine: its install
step's 4 min, and 4 min for a browser step whose two rows run in about 90 s locally. The job
budget is those two plus that job's measured 135 s maximum for every step without one,
rounded up: 4 + 4 min plus 135 s = 11 min. The first runs replace these with readings of
their own, as this file's head requires. Its Postgres then moved from a `services:`
container to the harness's own, for the reason .github/ci-notes/dispatch-browser.md#what-the-two-process-job-runs gives, which
added that job's two steps for it at that job's figures: the pull at 120 s and the
connections read at its two-minute floor. 4 + 4 + 2 min plus 135 s and 120 s = 14 min 15 s
-> 15 min.

## Why the embedder job builds contracts

The e2e helpers the specs import read some of contracts' values from its built dist/,
through Playwright's own Node resolution (the shard job's step of the same name), which
takes the entries Dispatch imports alone. `bun run e2e:embedder`, the "Browser-test the bar's two tiers against a counting embedder" step, makes the same
build again (packages/dispatch's build:contracts).

## Why the embedder job's server build carries the lane in its name

run-server.sh builds it too, inside the harness's boot wait; built here, the server
finds it current and starts in seconds. The name carries the lane because
.github/scripts/check-ci-filters.sh names a step of this workflow by its own name, and
the multiprocess job has a step that builds the same binary.

## The visual review, its docker run and its budgets

The containerised visual review: every page of packages/dispatch/e2e/review-routes.ts's shot
set compared against a committed baseline, in light and in dark, at a desktop width and two
of them again at a phone width. Eighteen pages and two, so forty comparisons, which is the
number of PNGs under `packages/dispatch/e2e/__screenshots__`.

The full lane only. A snapshot difference holds the merge queue, which is the point: a palette
or a layout regression is refused at the merge rather than found the next morning, and the
nightly tells nobody but the default branch's last committer (that workflow's own head says
so). The cost is that a flaky comparison would hold the queue too, which is why the tolerance
is measured from zero rather than defaulted (packages/dispatch/e2e/visual.e2e.ts).

Why a `docker run` inside a step and not a job-level `container:`. A `container:` on any job
of this file drops every `go build` the suite starts out of check-ci-filters.sh's reduction
(`.github/scripts/ci_filter_coverage.py#shell_reason`, "the job runs in a container:"), which then counts
each as reading every Go file of the repository. The shard and alone jobs do take one,
since the browsers they run are in it, and the `dispatch_browser` gate answers for them by
listing no script Run at all and pinning the binaries job's build whole instead
(.github/scripts/ci_job_reads.py's gate says why). This job needs no container of its own:
the seed its run starts has to stay on the host, where `psql` is, and a page in a
bridge-networked container would reach the harness's 127.0.0.1 listeners over another
address and so be no secure context, so only the Playwright command goes into the image
(packages/dispatch/e2e/visual-run.sh's head says both). Every variable this job's run needs
is exported on the command line rather than set in an `env:`, which keeps it to the same
shape as the shard job's browser step.
The browser step's body, those exports included, is .github/scripts/dispatch-browser-visual-suite.sh.

It writes no blob report. The merge job's row accounting reads legs out of the job names with
a capture that knows `shard (N)` and `alone (P)` alone, and it fails the run when the merged
report holds rows no leg printed; the multiprocess job writes none for the same reason. This
job joins `merge`'s `needs:` and the verdict step's pair list instead, which is what makes a
red visual job fail the workflow.

The visual job's budgets, its `timeout-minutes` and its browser step's, are measured, from these runs of this job, in seconds. Later runs do not
make this list stale: it names what the numbers were derived from, and a run that outran a
budget fails the step by name, which is the signal to derive them again.

  step                              job 128655164325  128661965627  128664453706  128713801668  128720753224  129152410516
  checkout                                         8             7             -             7             7             8
  setup-go                                         7             9             -             6             9             9
  the go cache restore                             1             2             -             2             2            17
  setup-bun                                        2             1             -           218           131             2
  bun install                                      5             7             -             3             -             7
  the contracts build                              0             0             -             0             -             0
  the dashboard build                             33            37             -            23             -            30
  the image pull                                  33            26            32            32             -            35
  the browser step                                90           106            78             -             -            62
  the upload                                       1             1             -             2             1             1
  the whole job                                  180           201           163           318           272           172

The rule is in this file's head. The browser step's maximum was 106 s, so 106 x 1.15 = 122 s
-> 3 min, and the job budget was that step's 180 s plus the measured maximum of every step
without one, which is 333 s of the six columns together: 513 s -> 9 min.

The sixth column is the first full-lane run after this job's Go cache restore moved from
go-envoy to go-envoy-test, run 42614920253 (job 129152410516). That restore reads
go-envoy-test-Linux-go1.27.3, 677,668,128 bytes, where the five columns before it read
go-envoy: 17 s rather than the 1 to 2 s above, which is why that row's maximum moved and the
job budget still held.

The browser step then outran its budget, which is this rule's signal to derive it again. Over
the 17 merge-group runs of this job from 2026-11-14T08:40Z to 2026-11-15T06:25Z that passed,
it read 111-179 s, and two runs that day timed out on it at 183 s and 182 s (run 43687635217,
a merge group, and run 43691184536, a pull request). So 183 x 1.15 = 211 s -> 4 min, and the
job budget is that step's 240 s plus the same 333 s: 573 s -> 10 min.

The first of those runs was the only one this job had when these budgets were first written,
and one run proved too thin a sample: 90 s gave a 2-minute bound by the rule's own floor, and
the next run came in at 106 s, 14 s inside it. A third run then read 78 s, so the spread of
the browser step is 78-106 s over three runs and the 15 per cent the rule adds carries the
rest.

The job budget was 5 min on the first three columns, where setup-bun read 1-2 s because the
runner image carried the pinned Bun. The last two columns are runs during a GitHub release
CDN outage on 2026-11-08, where that same step downloaded Bun in 218 s and 131 s, and the
5-minute job budget cancelled both jobs - one in `setup-bun`, one in the dashboard build -
before any step budget could fire. That inverts this file's rule, which says the job budget
is the backstop that never fires first, so it is re-derived above from the maximum those
runs measured. A download this job does not control may not decide a required check.

The step "Pull the stack's Postgres image" was added later, for the reason
.github/ci-notes/dispatch-browser.md#the-stacks-postgres-image-pulled-here-for-the-same-reason gives,
and its pull is counted at the 120 s the browser step spent before the stack's first output
on the run that found it: 240 + 314 + 120 = 674 s, 11 min 14 s, rounded up to the 12 minutes
it had. The reverse-order browser step this job ran after it, with its own 4 minutes, is gone:
the section on the guard that took its place says why.

#4383 counted the steps without a budget at 314 s, the five columns' figure before the sixth
was added (240 + 314 + 120 = 674 s), where #2856 had already re-measured them at 333 s over the
six columns above, the sixth being run 42614920253 (job 129152410516). So the sum is
240 + 333 + 120 = 693 s, 11 min 33 s, and the budget stays 12 minutes.

## Why the visual job restores go-envoy-test

go-envoy-test, restored only, for the reason the binaries job's step of this name gives:
this job's run-server.sh builds the Dispatch server, which only that family's entry
holds. It saves none.

## Why the visual job builds contracts

The e2e helpers the visual specs import read some of contracts' values from its built
dist/, through Playwright's own Node resolution (the shard job's step of the same name,
which says why no hook of the suite can make this build, and why the entries Dispatch
imports are the ones it builds). The run inside the container reads it from this same
checkout, mounted there at its own path.

## Why the visual job installs no browser

No Playwright install and neither browser cache: the image the "Pull the pinned Playwright copy" step pulls carries all three
engines at the Playwright version bun.lock pins, which is what makes its shots the ones
a person's `--update-snapshots` takes.

## Why the visual job pulls the pinned Playwright copy first

The copy in ghcr.io/cybermonklabs/legion/ci (.github/ci-images.yaml), named by the one
digest packages/dispatch/e2e/visual-run.sh runs, pulled here so the run of the "Browser-test dispatch dashboard against its baselines" step does not
pay for it inside the browser step's own budget. A failed pull is a fault of this
repository's own package, so it fails here and says so.

## The stack's Postgres image, pulled here for the same reason

THE STACK'S POSTGRES IMAGE, PULLED HERE FOR THE SAME REASON. e2e/harness-pool.ts's
startStack starts the stack's PostgreSQL with a `docker run` of DISPATCH_E2E_PG_IMAGE, the
digest the browser step exports (.github/scripts/dispatch-browser-visual-suite.sh), and a pull the runner had not made yet counts
inside that step's 4 minutes. On #4129 (run 44215262087, job 134503540243) the step
spent 120 s before the stack's first output, where five green visual jobs (134499880849,
134479592234, 134451447710, 134451520100, 134547177192) spent 7 to 15 s, and the step ran
past its budget at 250 s. Its log does not time the pull apart from the rest of that
start, so the 120 s is the bound this pull is given, not a measure of the pull alone. The
digest is written twice, here and in the browser step's script, and the two must stay the same:
a pull of another digest here leaves the browser step to pull its own inside its budget again.

## Why each visual row stops its page's writes, and runs once

One seed serves the whole visual run, so a page that wrote to the corpus as it drew would change
what every row after it compares, and a baseline would hold another row's write. So no row's page
may write: e2e/visual-guard.ts, installed on each row's page before it navigates, stops every
request to `/api/` or to the sign-in routes beside it (`/auth/`) with a write method, every
document-socket frame that would change a document
(a Yjs update, or a sync reply whose structs or deletions change it), and every live-socket frame
that is no subscribe or unsubscribe, and the row then fails with the list of what it stopped, by
its own name. A read mark passes only exactly as the seed wrote it before any row
(`markWhatTheShotPagesRead`, e2e/review-seed.ts, which writes the marks into the seed file): a
reply the seed marked read, the spec's read at the version the seed wrote. Any other version, a
lower one moving the read back included, is stopped. The editor's own stamps as it loads a
document - a heading's id, an ordered list item's label, which Milkdown derives from the text and
no rendering carries - are dropped without failing the row: the guard reads each frame against the
server's document as the page was sent it and drops one that changes only those
(`changesOnlyDerived`, web/src/features/doc/derived-attributes.ts, the classifier the editor's own
pending-edit store reads). The first run of the guard stopped an update of 34 to 54 bytes that each
of four documents sent as it loaded, with no person typing (job 137537803811).

What a page cannot stop is a write the server makes while it answers. Three things close that.
Each row resolves every API read its page made to the GET route the table serves it by (the
committed OpenAPI document, which openapi_test.go holds to the table, a literal segment over a
wildcard), and fails on a read of a route, or of no route, on a sign-in read, and on a live view
kind, that packages/envoy/internal/dispatch/api/testdata/visual-shot-reads.json does not name. The
Go test TestTheVisualShotReadsWriteOnlyTheirCaches (visual_shot_reads_test.go) sends every route
that list names as the dashboard does, signed in with a session cookie, runs what GET /auth/whoami
runs (the cookie's identity and the event feed's head, on a server whose event horizon nothing has
built yet, as the page's first read finds it), and subscribes every view kind it names on GET
/ws/live; it reads every statement each read, each view and the work they detach run, through
read_only_token_test.go's tracer of a request's statements (markedStatements, writesOf). A read
may write the display-tree cache, the sign-in read nothing, and a live view the event horizon and a
deploy run's warning record, each a table that test sees written on that channel; any other write
fails it by the read or view and the table. It opens and closes a document's room as a page does
and holds the room to writing nothing. And the seed waits, before any row draws, until the
document service owes no document a settlement, and holds that the corpus records no check result
and no pull request's
head (`awaitNothingOwed`, e2e/review-seed.ts): an owed settlement runs when a page opens the room
and writes a version the shot would draw, and a result's commit is one the Verification tab's read
asks GitHub the pull request of, writing what it learns.

This replaces a second run of every row in reverse order, on a new stack and a new seed, about 67
s a job (p50 of 2026-11-22's full-lane runs). That run caught a write only when some later row's
shot happened to draw it, and could not say a row passes alone; the guard names the row that
wrote, whatever any other row draws, and stops the write before any other row can see it.

## Why the visual job's upload is always

Always, not on failure alone: a reader of a green run wants the report too, and a run
that wrote a baseline no tree holds yet - a route this layer gained - leaves it here,
which is how that file reaches the pull request from a machine with no container. It
holds the run's results under test-results.

## Why the visual upload keeps a pattern that matches no file

The last pattern matches no file in THIS job and is not dead: it is what fixes the
archive's layout. This job runs one stack, and for one stack e2e/harness-pool.ts
leaves `logDir` unset, so the harness writes to stderr and no `worker-*/log/*.log`
is ever created. upload-artifact takes the archive root from the least common
ancestor of its patterns' search paths, so this `${{ runner.temp }}` pattern is what
keeps that root at the runner's work directory and the repository path inside the
archive. That path is the one e2e/visual-run.sh prints on a failed run, and the one
its header sends a reader to who takes a baseline from this artifact.
Delete it as unused and `test-results` moves to the top of the archive, off that path.

## What the merge job reports, and why it is never skipped

What the legs together ran. It joins every shard's and every alone leg's blob report into one
report, publishes four readings of it - the rows, the flake rate, the legs that did not
succeed and the row durations - and is the one job of this workflow that reports ci-ok, so a run gets one verdict
and not seven.

This job must never be skipped: .github/scripts/ci-ok.sh fails a selected job whose result is
`skipped`, and a shard leg that failed would otherwise skip it and take ci-ok with it. It
reads the upstream results itself instead, in its last check before the ci-ok report.

`!cancelled()` is what says that and no more. It is false only when the whole run is
cancelled, so a leg that failed, or that hit one of its own budgets, still gets this job.
`always()` reaches further and costs: it is not cancelled when the run is, so a pull-request
run that ci.yaml superseded (`cancel-in-progress`) would hold this job in the queue and then
run it, the superseded run would stay in progress, the next run of that group would queue
behind it, and the old head would collect a red ci-ok. ci.yaml writes `!cancelled()` on its
own jobs for the same reason. The step-level `if: always()` inside this job are a different
thing and stay: a step's condition is about the steps before it, not about the run.

## Why the merge job needs the binaries job

Every job of this workflow, the binaries job among them: a build that failed leaves the
shard and alone jobs skipped and no blob report anywhere, and without it in this list the
run would be reported as legs that uploaded nothing rather than as the build that never
handed them anything. The verdict step, "Fail when a leg failed or the merged report holds a
failed row", names it as it names the rest.

## Why the merge job runs on arm64

arm64, for the reason .github/ci-notes/ci.md#why-changes-pr-title-cancel-started-and-ci-ok-run-on-arm64
gives (INFRA-4222). This job
runs JavaScript actions, `bunx playwright merge-reports` and two scripts over gh and jq, with
the runner image's own node, python3, gh and jq. The one toolchain it installs is Bun, from
the aarch64 row of .github/actions/setup-bun that ci.yaml's tool-fallback-arm job proves.
Playwright's report merge starts no browser. At busy
hours the x64 pool held this 35-second job for about 35 minutes (run 44126172228 on
2026-11-17), and it holds every browser run's verdict, after the legs and before ci-ok.

## The merge job's measured steps and its budget

The budget is measured. Over the 32 concluded `dispatch-browser / merge` jobs of the last
43 completed ci.yaml runs, read from the jobs API on 2026-11-08, the job's maximum is
34 s: job 128737694112 of run 42481906495, 12:57:25Z to 12:57:59Z. The 37 s this comment
carried before was measured on another branch and is replaced by that sample.

Each step's own maximum over that sample, summed, is 40 s: Set up job 2 s, checkout 9 s,
setup-bun 3 s, `bun install` 7 s, Download every leg's blob report 2 s, Take the newest
attempt 0 s, Merge the legs' blob reports 3 s, Upload the merged report 3 s, Reading one
4 s, Reading two 1 s, Reading three 1 s, the verdict step 1 s, the ci-ok report 1 s, Post
setup-bun 1 s, Post checkout 1 s, Complete job 1 s.

One step carries a budget of its own: "Reading one, the rows", which calls
.github/scripts/dispatch-browser-run-reads.sh and so waits on gh-read.sh's ladder. The
ladder bounds that wait rather than measuring it - three attempts, each bounded by
`timeout 30`, with waits of 10 s and 20 s between them - and a bound takes no 15 per cent.
The read is paginated, so gh-read.sh can read no answer of its own: after an attempt that
exits 1 it reads GitHub's rate_limit route, bounded by its own `timeout 30`. So each attempt
takes up to 30 + 30 s, and the ladder 3 x 60 + 10 + 20 = 210 s at most. The step's own work
has a measured maximum of 4 s; it now also runs
packages/dispatch/e2e/tools/leg-rows.ts over the four shard legs' row lists (1.1 MB for the
full lane's 1,617 rows), measured at 0.02 to 0.08 s on the development box over three runs;
counted at a whole second, (4 + 1) x 1.15 = 5.75 s, and 210 + 5.75 = 215.75 s, at the next
whole minute above, is the 4 min that step carries. The rule's two-minute floor does not bind.

A spent API budget is not in that bound: when the rate_limit route shows the core budget this
read spends at 0, gh-read.sh's gh_wait_reset waits for its reset only when that falls at least
30 s before this step's limit and the job's, which the job's env passes as JOB_TIMEOUT_MINUTES
and the step's as STEP_TIMEOUT_MINUTES, and otherwise fails the step at once, naming the wait
and the limit. So the wait never outlives either budget, and the budgets need no room for it: a
step that would have to wait past them fails by name instead, and a re-run after the reset
reads the jobs again. A secondary rate limit is not waited for on this read: GitHub names its
wait in the answer's retry-after, which a paginated read leaves unread, so it is one of the
ladder's failed attempts.

So by the rule in this file's head the job's budget is the sum of its step budgets, that
one step's 240 s, plus the measured maximum of every step without one: 40 s less reading
one's 4 s is 36 s, and 1 s for "Report a refused blob download as a refused read", which
runs one `echo` and has no measurement of its own - 1 s is what this job's other
one-command steps measure. 240 + 37 = 277 s, which is 4 min 37 s, so 5 min. The job's
budget is above the step's, so the step's own budget fires first and names the step,
which is the reason this file's head gives for putting budgets on steps.

What that sample does not hold: no browser step failed in any of those runs, so no blob
report in them carries a failed row's trace, and the download, the merge and reading
three's per-leg merge all grow with those. A job cancelled at this budget posts no ci-ok,
so the run is red and read again from the jobs API; it cannot pass a verdict it never
reached.

## Why every leg's blob report is downloaded without merge-multiple, and the two layouts it lands in

Every leg's blob report and row count. A leg's artifact holds a blob-report/ directory, a
rows/ directory or both (a leg whose browser step wrote a count and no blob uploads rows/
alone), so the zips land together and the counts land beside them rather than among them:
`merge-reports` reads the zips of the directory it is given.

No `merge-multiple`, so when more than one artifact matches, each is extracted into a
directory named for it (dispatch-browser-blob-shard-1-2/blob-report/report-shard-1.zip, for the dispatch-browser caller)
and the step "Take the newest attempt of each leg's blob report" can tell the attempts
apart. A run's artifact list holds every attempt's artifacts and this pattern matches
them all; two attempts of one leg hold the same inner paths, so merging them into one
directory let one attempt overwrite the other in an order nothing fixes.
The pattern starts with the caller's id, the workflow's `job` input, as the shard and alone
jobs' upload names do: a references run beside a pull-request run in one workflow run must not
join the other's legs.

When exactly one artifact matches, the action extracts it into `path` itself, with no
directory named for it: v8.0.1's src/download-artifact.ts, at the pinned 3e5f45b2, writes
into `path` when `merge-multiple` is set or when `artifacts.length === 1`. blobs/ then
holds that artifact's blob-report/ and rows/ (blobs/blob-report/report-shard-1.zip). How
many artifacts the pattern matches, over every attempt of the run, is the run's - one leg
that uploaded a blob on one attempt makes one - so that step reads both layouts, and takes
a lone artifact whole: one artifact is one leg at one attempt, and there is nothing to
choose. That layout holds no artifact name, so no prefix is read there: the pattern is
the caller's own, so the one artifact it matched is that caller's.

## Why a refused blob download is reported as a refused read

A download `actions/download-artifact` could not complete is a failed read of the run's
artifacts and not a failed browser row, and the download step's own message says only
that the download failed: three merge jobs of pull request #2983 were first investigated
as test failures because of it (jobs 128735037875, 128729862923 and 128724678335, each
carrying `##[error]Unable to download artifact(s): Unable to download and extract
artifact: Artifact download failed after 5 retries.`, read 2026-11-08). This step says
which of the two it was and exits 0, the "Download every leg's blob report" step having already failed the
job. The script reads GITHUB_RUN_ATTEMPT, which Actions sets, so this step needs no
`env:` block and no token; it names the download step and that attempt and nothing
else, the action's own call and retries being in the download step's error line and
not anywhere this job can read.

This step is kept rather than deleted in favour of reading one's refused-download
branch (the download concluded `failure`), which names the refused download too, because
a run whose jobs read is refused as well exits reading one at its own refusal and never
reaches that branch, and then this step is the only line that names the refused download.

## Why only the newest attempt of each leg is merged

The newest attempt of each leg, and no other, into one directory of the shape that the steps
"Merge the legs' blob reports into one report", "Reading one, the rows" and "Reading three, the leg exits with no failed row" read. A leg a failed-only re-run did not run keeps the attempt that ran it, which
is what .github/scripts/ci-ok-report.sh already does with the jobs' ci-ok reports; the
script's head says what two attempts of one leg did to a merged report when they were
downloaded over each other. It runs from $GITHUB_WORKSPACE, this step's own directory.
Where several artifacts matched, its log names the attempt it took for each leg; where one
matched, it names the files it took. The third argument is the caller's id, the workflow's
`job` input, whose blob prefix the download step matched.

The step's id, `select-blobs`, is how "Reading one, the rows" reads its conclusion beside the
download's. Reading one reads only this step's directory; this step runs only after a download
that succeeded, and the merge step only after this step succeeded. So a leg is named as having
sent no blob only when both steps succeeded. A failed download's error keeps its own message,
the refused read that the section above describes, and the notice lists the legs it read
nothing of. When the download did not complete in another way, or this step did not complete
after a download that did, reading one names that step and how it ended, says that the job made
no merged report, and names the legs whose blob and whose count it did not read, claiming
nothing about whether those legs uploaded. Run
44754426131, attempt 1, is why: the job's limit cancelled its `bun install`, both steps were
skipped, and reading one named four shards as having sent no blob, though each blob had been
uploaded before the job started.

## What the merged report's three outputs are for

The JSON report is the one packages/dispatch/e2e/tools/check-slow-rows.ts reads, and
this merged report is now its source: one run's rows, from every leg, in one file. The
HTML report is for a person; the list reporter prints the run's result into this step's
own log. It reads the selected directory the "Take the newest attempt of each leg's blob report" step wrote, which holds one attempt
per leg. The three output paths are exported here rather than in an `env:` block, for
the reason the shard job's head gives.
The step's body, those exports included, is .github/scripts/dispatch-browser-merge-reports.sh.

## Why each reading goes to the step summary and to the log

Reading one. Each reading is written to the step summary, for a person, and printed as a
`::notice::` line, for a script reading the log: the two readers want the same figures in
different places.

## Why a leg that exits red with no failed row is named apart

Reading three. A leg that ended red with no failed row in the report died outside the
suite - a budget, a runner fault, a step before the browser step - and that is a different
fault from a failing row, so it is named apart.

## What reading four measures, and which rows it reads

Reading four. What a row of each layer costs, which nothing published before: the merge
job printed rows and the flake rate and no duration statistic at all, so
dispatch://INFRA-1407's k1 and dispatch://INFRA-983's R13 had no repeatable command to
read. The two classes are the `stub` and `stub-phone` projects' rows and every other
row, which is what "the project or tag the configuration gives them" names after that
change.

Read from the merged report's passing results alone, as the per-test budget in
e2e/playwright.config.ts is: a failed row's duration is the time it took to fail or to
reach its timeout, which says nothing about what the row costs, and a skipped row's is
not a run at all. A retried row contributes each attempt it passed, which is one.

## What the verdict step fails on

The verdict, which the four readings ("Reading one, the rows", "Reading two, the flake rate", "Reading three, the leg exits with no failed row" and "Reading four, the row durations") do not give: they report, and this fails. Two
of the job's four failures are here - an upstream job that neither succeeded nor was
skipped, the binaries job among them, and a failed row in the merged report - and the
other two are in reading one, where they are found: a missing blob, and a row of the
selection that no shard leg ran or that two ran.

## When the merge job posts ci-ok

ci.yaml's dispatch-browser and dispatch-browser-pr calls only: on a merge-group run whose group
the calling job's own result decides it posts the failing ci-ok at once, and on every other run
it posts nothing, the verdict being `.github/workflows/ci.yaml#ci-ok` job's own check run
(.github/actions/ci-ok-report says how). It reports under the caller's id, the workflow's `job`
input, so dispatch-browser and dispatch-browser-pr each report for themselves. This is the job
that posts for that call: it needs every other job of this workflow, and the step is its last,
which is the shape .github/scripts/ci_filter_coverage.py's check_ci_ok holds a called workflow
to. The nightly workflow passes no `ci-ok-changes`, and the step skips; nor does ci.yaml's
dispatch-references job, which is outside ci-ok on purpose (CI_OK_EXEMPT), and which
check_ci_ok refuses to let report.
