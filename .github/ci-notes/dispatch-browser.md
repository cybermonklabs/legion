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
GitHub puts before every job name of this file (`dispatch-browser-pr / real`) and which
starts every artifact name, so two callers of one run share no artifact but the one build the
references lane takes from the pull-request lane
([its section](#when-the-references-lane-takes-the-pull-request-lanes-build)).

`pull-request` runs four jobs: binaries, `real`, `stub` and merge. The two browser jobs run
disjoint projects under the same `@pr` filter. `full` runs seven jobs: binaries, both shards,
alone, multiprocess, visual and merge. The shards run the parallel projects with no `@pr`
filter, and alone runs the three wall-clock projects. A separate step runs the two
counting-embedder rows after the main browser step of `real` on pull requests and `alone` on
full. The merge job joins the blobs into a single report and publishes the flake rate. The
third lane, `references`, previews the reference rows and gates nothing
([its section](#the-third-lane-references)).

A reusable workflow, not a composite action, for the reason docs-site.yaml's head comment
gives: `container:` and `services:` are job-level keys and a composite action's step keys do
not include timeout-minutes. No job declares services: the harness starts the stacks'
PostgreSQL. The shard, real and alone jobs use the same browser image. Their step lists are
kept textually identical where they run the same commands, so a reader can diff them.

That reason is a composite action's, and not a script's: a step that calls a script keeps its
own timeout-minutes. So a body more than one of the shard, real, stub and alone jobs really
share is a script, written once and called from each, and that script's head is where what it does and why is written
down. There are two: .github/scripts/dispatch-browser-samples.sh for the memory and CPU
sampler and its two readings, and .github/scripts/dispatch-browser-postgres.sh for the
reading of the stacks' PostgreSQL, which stood as a copy in each job's own step list until a
review read the two together. Two more stood beside them, the Playwright install and the key
its apt archive cache was restored under: the shard and alone jobs run in the image this repository
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
verdict on the runner. On main's four legs its 11 minutes, the maximum plus 15 per cent, cut a
leg of run 44985914477 whose every row had passed, on a runner that ran the same rows 1.75
times slower than another of the same minute. Its budget is that factor times the largest leg
of this job's two legs in the full lane it was last read from, under main's split by time, times
the worst drift a row-time table up to a week old was measured to give, and
[the step's section](#why-the-shard-job-calls-playwright-directly-and-what-its-browser-steps-budget-is)
holds the derivation and the runs it was read from.

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
with main's parse-count walk beside them, on the binaries job, the shard job's two legs and the
merge job alone: four jobs, or three when it takes the pull-request lane's build. The full
lane runs seven and the pull-request lane runs four. The references lane lists 147 rows, so
a pull request that changes the reference framework gets those rows from CI at its head rather
than from a run by hand. It gates nothing:
.github/ci-notes/ci.md#the-reference-rows-preview-job
says why. The lane's spec files are written in one place, the `references)` arm of
.github/scripts/dispatch-browser-shard-suite.sh, the script the shard job's browser step runs.

Its caller, ci.yaml's dispatch-references job, can share a workflow run with
dispatch-browser-pr, on a pull request that selects both, and passes its own id as `job`, as
every caller does: so its artifacts are `dispatch-references-…` beside `dispatch-browser-pr-…`,
and each merge job reads only its own caller's legs. Where both run and dispatch-browser-pr's
binaries job succeeded, it builds nothing and runs that job's binaries and dashboard
([the next section](#when-the-references-lane-takes-the-pull-request-lanes-build));
every artifact it uploads is still `dispatch-references-…`.

Its merge job skips one step the other two lanes run: "Reading five, the lane's machine time",
whose `if:` holds it off this lane. That reading holds a lane to a committed ceiling, and R5 of
dispatch://INFRA-1109/spec bounds the full and pull-request lanes alone. This lane gates
nothing, and no run of it has been read to set a ceiling from. The reading refuses a lane it
holds no bound for (`.github/scripts/dispatch-browser-run-reads.sh#machine-time)`), so without
that `if:` every run of this lane would fail there.

## When the references lane takes the pull-request lane's build

The references lane builds what the pull-request lane builds, so where both run in one run it
takes that lane's build rather than making a second. In ci.yaml run 45850342764, attempt 1,
`dispatch-references / binaries` (job 140015838557) and `dispatch-browser-pr / binaries` (job
140015839531) uploaded the same artifacts: each binaries artifact held 4 files and each dashboard
artifact 186, and every file had the same sha256 in both. Their logs, compared on every `Run`
block, every env line, the go build lines and the cache lines, differed in the `lane:` input
alone, `references` against `pull-request`, and that input changes nothing the job builds. The
Go commands are fixed text of the step "Build the binaries and the dashboard the browser suite
runs", and .github/scripts/dispatch-browser-build-dashboards.sh builds web/dist-b, the second
rollout bundle, on the full lane alone, so every other lane builds web/dist and nothing beside
it. The second build was 215 s of the references lane's 588 machine-seconds in that run: binaries
215 s, shard (1) 157 s, shard (2) 189 s and merge 27 s, each job's `completed_at` less its
`started_at` as the jobs API gives them.

Two inputs carry it, both empty by default: `build-job`, the id of the caller whose binaries
job built what this call runs, and `connections`, that job's `connections` output. Given a
`build-job`, the binaries job skips by its `if:`, and the shard job runs past the skip, as the
attest job of the worker image's workflow runs past the build an attest-only call skips: its
`if:` is `!cancelled()` and a binaries job that succeeded, or one that skipped where a
`build-job` was given. Its first read of the binaries, its second read through
.github/scripts/dispatch-browser-fetch-binaries.sh and its dashboard download all name
`<build-job>-binaries` and `<build-job>-dashboard`, and its browser step takes the
`connections` input before the binaries job's output, which a skipped job leaves empty.

Nothing else moves. Every artifact the call uploads keeps its own `<job>-` name, so the
references lane's blob reports, records, merged report and failed rows' traces are still
`dispatch-references-…`, and its merge job still reads only its own legs. The verdict step
reads the skipped binaries job as it reads any skipped job, as no failure
(.github/scripts/dispatch-browser-verdict.sh), and here there is none: the build it skipped is
one the other caller's binaries job made and uploaded whole.

The file's own `connections` output is what a caller reads to know there is a build to take, and
it is not empty only once both artifacts are uploaded. The binaries job's `uploaded` output is
the `artifact-id` of its last step, the dashboard's upload, which actions/upload-artifact leaves
empty when the upload failed and which a step that never ran does not write; that step runs only
after the binaries' upload and every step before it succeeded. The file's output is the
binaries job's `connections` while `uploaded` is not empty, and empty otherwise, so a binaries
job that wrote its connection limit and then failed, was cancelled or skipped before both
uploads hands on nothing. That is the job's own success but for its post steps, and the one
condition the taking call needs: the two artifacts it downloads are there. A job output and not
`jobs.binaries.result`, which GitHub's `jobs` context holds too: actionlint 1.7.12, which the
ci-filters job and workflow-files.yaml run, knows that context's outputs alone and refuses the
result. ci.yaml's dispatch-references job is the one caller that fills the two inputs, both from
dispatch-browser-pr's output, and when that output is empty it passes them empty and builds as
it always did; .github/ci-notes/ci.md#the-reference-rows-preview-job says when each happens and
what waiting for that call costs. The full and pull-request lanes pass neither input. Their
jobs download from their own binaries job, which is why `build-job` is the references lane's
alone.

## What the job set costs, and the machine-time ceiling the run holds it to

What the job set of dispatch-browser.yaml costs, measured rather than estimated, on
2026-11-24 at dfc51f65. Its full lane there is run 45306966549, read over three attempts: a
workflow_dispatch of ci.yaml, and two re-runs of its browser jobs from the binaries job on,
which is the rule the architect set on 2026-11-23 after INFRA's controller, since a dispatch of
ci.yaml runs every suite it holds. The three cost 2,662, 2,450 and 2,494 s, each over eight jobs
for 1,732 rows: 1.537, 1.415 and 1.440 machine-seconds a row, in own times of 1,235, 1,191 and
1,216 s - each attempt's binaries job, longest leg and merge job, end to end - and wall times of
1,425, 1,198 and 1,223 s. Attempts 1 and 3 read no flaky row, and attempt 2 one of its 1,732
(`packages/dispatch/e2e/credential-expired.e2e.ts#an expired secret request is marked with its deadline, leads to no decision page, is not counted, and Dismiss takes it off for good`
at dfc51f65, on chromium, a row that compared a time's spelling
and now compares its moment). The same lane on the default branch that day, every green
merge-queue full lane of ci.yaml up to 02:17:35Z, is 3 runs of twelve jobs for 1,682 rows that
cost 3,740 to 3,816 s, a median of 3,763 s and 2.2372 s a row at the median of their per-row
figures. So the three attempts cost 0.707, 0.651 and 0.663 of that median, and spend 0.687,
0.632 and 0.644 of its row's machine time. The pull-request lane at the same commit is run
45306887784, read over three attempts the same way: 839, 725 and 857 s over five jobs for 290
rows, in own times of 538, 451 and 541 s.

The historical comparison still counts the jobs each run actually ran, including a separate
embedder where one ran. `lane-cost.ts base` lowers the minimum from eight to seven successful
jobs so it can admit the new full lane. It does not use the current workflow's job-name
filter. On the fixed window `2026-11-24T00:00:00Z..2026-11-24T02:17:35Z` with cutoff
`2026-11-24T02:17:35Z`, the two versions emitted identical JSON: runs 45300003979,
45297497405 and 45306390541, each with twelve jobs, at 3,763, 3,816 and 3,740 s respectively.
The median stayed 3,763 s. Reading five still uses the current run's job names and the same
3,146 s and 846 s lane budgets.

That cost is now held to a budget by the run itself, at the merge job's step "Reading five, the
lane's machine time": the jobs of a lane, this merge job apart, may cost that lane's committed
ceiling less that job's own worst case. Both are written in the
`.github/scripts/dispatch-browser-run-reads.sh#machine-time)` branch, as
`.github/scripts/dispatch-browser-run-reads.sh#bound` and
`.github/scripts/dispatch-browser-run-reads.sh#merge_maximum`. The worst case, `merge_maximum`,
is 54 s, job 138031009105 (run 45261148766 attempt 2),
as that branch records with the merge jobs it was read over and the figures it replaced.
The readings above, taken as the budget takes them, which is each lane less its own merge job,
are what the merge job's "Reading five" line printed at dfc51f65: 2,629, 2,410 and 2,446 s for
the three attempts of run 45306966549 (merge jobs of 33, 40 and 48 s), and 821, 690 and 821 s
for the three attempts of run 45306887784 (merge jobs of 18, 35 and 36 s). All are inside the
budget, which is 3,146 s on the full lane and 846 s on the pull-request lane.

That ceiling is a coarse guard and no reading of what dispatch://INFRA-1109 delivers: no job can
read another run, so a figure written here cannot be a ratio against the day's default branch,
which is what R4 and R5 of dispatch://INFRA-1109/spec are. It was 2,040 s while three runs of
2026-11-11 and 2026-11-12 were the newest readings (43045412941, 43074443876 and 43079610268,
2,025, 1,946 and 1,964 s at that step), 2,400 s once the machines got slower and this suite did
not, and 2,700 s once one slow pair of runs showed what an absolute figure near the measurement
costs: on 2026-11-15 two runs of one tree cost 2,252 and 2,450 s (43664675139, 43667557013) with
real-stack row medians of 2,746 and 3,126 ms, while the day's thirteen green merge-queue runs
of the default branch up to 15:03Z cost 2,975 to 3,347 s, with a median of 3,126 s. Those two
runs were 0.72 and 0.78 of that median, so the suite is as cheap as it was and the ceiling is
what moved. It became 3,200 s on 2026-11-17, when pull request #3513's full lanes at 61e5c402
and 1ba866a9 read 2,062 to 2,484 s at that step, the worst 6.5 per cent under 2,656 s. Read as
that step reads a lane, its merge job apart, the full lane's 3,146 s is 27 per cent above that
tree's worst reading (2,484 s), under one and a half times its median (2,231 s), and under the
median of the 27 green default-branch full lanes of 2026-11-16 (3,189 s), though 10 of those 27
read at or under it. The pull-request lane's is 846 s, its 900 s less the same 54 s.
The `.github/scripts/dispatch-browser-run-reads.sh#machine-time)` branch
carries the arithmetic and the runs each figure came from. Nothing else in this tree refused
a run that cost more than the one before it, which is how this suite went from about 25
runner-minutes to 45 to 49 in three days (dispatch://INFRA-1109, R12).

## Why GOPROXY falls through to direct

A `|` between proxy entries makes a go command fall through to the next source after ANY error,
where a `,` falls through only after a 404 or a 410 (Go's module reference, "GOPROXY protocol").
proxy.golang.org reset a download mid-stream and turned a Go lane red on a module whose hash
go.sum already holds (sjawhar/legion#2512); with `|direct` the command fetches the same bytes
straight from the source and checks them against go.sum, so this is a second path, not a weaker
check.

## Why the binaries job builds what the suite runs

Everything this run's other jobs execute, built once here and handed to them in two
artifacts: the Go commands the stacks run - the Dispatch server and the real broker's three,
whose dev-oidc-issuer is also the issuer the two-process sign-in step serves - and the
dashboard bundle every browser row reads. The shard, real, alone, multiprocess and visual
jobs take both artifacts; stub takes only the dashboard. None builds a dashboard bundle or a
Go command. Each builds `@legion/contracts` in its step `Build contracts for dispatch e2e`,
because Playwright's Node runner reads that package's `dist/` and the package is not in these
artifacts (the shard job's step of that name says why). The merge job downloads neither
artifact: what it takes from the run is the legs' blob reports.

Why the build is a job of this file rather than of each caller: an artifact is visible only
inside the run that uploaded it, and every caller runs this file - ci.yaml's dispatch-browser,
dispatch-browser-pr and dispatch-references jobs and dispatch-browser-nightly.yaml's
dispatch-browser job. A build left in one caller would hand the others' runs nothing, and their
shard, real and alone jobs would fail downloading an artifact their run never held. The two artifacts
are `<job>-binaries` and `<job>-dashboard`, named for the caller, so callers that share a run
each download their own, but the references lane where it takes the pull-request lane's
([its section](#when-the-references-lane-takes-the-pull-request-lanes-build)).

Why the shard, real and alone jobs do not build the binaries themselves: they run in a container, and
.github/scripts/ci_filter_coverage.py's shell_reason counts a `container:` as something that
can move what a go command compiles, so a go command there would fall out of
check-ci-filters.sh's reduction and be counted as reading every Go file of the repository,
which is the opposite of what the `dispatch_browser` filter is for. So the builds live in
this job, which takes no container, and the suite's own scripts execute what it built
instead of building it (packages/dispatch/e2e/run-server.sh's DISPATCH_E2E_SERVER_BIN and
run-broker.sh's DISPATCH_E2E_BROKER_BIN_DIR).

Why the dashboard is built here and not in each job that reads it: it is one bundle for the
whole run, and run 42809067358 built it eleven times - eight steps of their own, 242 s together,
one inside the embedder job's browser step and two inside the multiprocess job's. It is built
beside the Go commands rather than after them, in one step that waits on both, so at run
42809067358's figures (28 s for the bundle, 104 s for the go build) it is spent inside the go
build and costs this job no wall time of its own. That is what `./.github/actions/setup-bun` and
`bun install --frozen-lockfile` are doing in a job that runs no test.

Its one `if:`, `inputs.build-job == ''`, selects nothing: the caller has already decided the
call. ci.yaml's three callers hold the `dispatch_browser`, `dispatch_browser_pr` and
`dispatch_references` outputs and the merge-group attempt term, and the nightly's
dispatch-browser job calls it on its full-lane schedule and on every dispatch, by an `if:` of
its own, so a condition here on what changed could only hold back a run its caller had asked
for. The `if:` skips the build in a call that names another caller's build to run instead,
which only the references lane does, and the job builds in every other call
([the references lane's section](#when-the-references-lane-takes-the-pull-request-lanes-build)).

## The binaries job's budget

Measured over three dispatched runs, jobs 128520516081 (83 s), 128528093378 (84 s) and
128528969380 (69 s), whose build steps took 45, 52 and 56 s. The two steps
dispatch://INFRA-1109 put in this job, ./.github/actions/setup-bun and the frozen `bun
install`, had no reading of their own here and took the shard job's measured maxima for them,
19 s and 62 s, from the four-leg table this budget was written against (the shard job's head
names its 24 legs and five runs, and its two-leg table holds those steps' readings now);
the bundle builds the build step gained run beside the `go build` and are waited on with it,
so they add no wall of their own (28 s and about 30 s against the go build's 104 s in run
42809067358). No step here carries a budget, so the rule in this file's head gave this job the
measured maximum of every step, 84 + 81 = 165 s, rounded up: three minutes. At this shape the
build step took 154 s in job 132738250756 (run 43689921885), and the step maxima of the green
binaries jobs of pull request #3513's runs of 2026-11-15 that concluded by 20:50Z summed to
239 s, which the rule rounds up to four minutes. The job's `timeout-minutes: 10` is larger than
that on purpose and is not that arithmetic. Every run of that sample restored the Go cache; a run
that misses it builds four commands cold, which takes longer than any run of that sample, and
three minutes could cancel it. So the budget carries the cold build rather than the warm one, as
the multiprocess job's Playwright install step, "Install Playwright Chromium", carries its own
retry arithmetic rather than its measured maximum.

## Why the binaries job restores go-dispatch-browser

go-dispatch-browser, restored only: a family of this job's own, which
.github/workflows/ci-caches.yaml's dispatch-browser job fills on every push to main, on this
job's runner, by running this job's go commands as this job runs them: the build step's two `go
build`s over the same packages and the connections step's `go run`, each with CGO_ENABLED=0.
That fill restores the family's newest entry, runs the commands on it, which compile only what
changed since that entry was saved, keeps in the build cache only what they used, and saves the
result under this job's key followed by the commit (`-${{ github.sha }}`); ci-caches.yaml's
refill-superseded job then deletes the entries the new one superseded, so the family keeps one.
No entry carries this job's key itself, so
GitHub's matching, which takes the key as a prefix too, gives this job's restore the last fill's
entry. The restore-keys, the key's `go-dispatch-browser-<os>-<arch>-` part, are what the
restores of go-daemon, go-envoy and go-envoy-test in ci.yaml's jobs carry: after a Go bump and
before the next fill they find the entry of the Go before, whose modules still serve. No job
here saves it.

It restored go-envoy-test until dispatch://INFRA-1109 measured what that bought. ci-caches.yaml's
envoy-test job fills go-envoy-test with `go test -c -o /dev/null ./...` over the whole envoy
module on the runner's default CGO_ENABLED=1, since the runner has gcc, and the build step pins
CGO_ENABLED=0 (the next section says why). Go's build cache keys every object on the build's
configuration, cgo included, and net, os/user and runtime/cgo differ with it, as does every
package above them, so that entry held the step's modules and almost none of its objects, and
the step compiled the server nearly from nothing on every run. Measured in cluster Jobs on 4
CPUs with golang:1.27.3, building the step's commands as it builds them at b1634bff of #3513,
after a build cache filled by building the same commands at main's 278138ea, the commit main's
go-envoy-test entry was saved at on 2026-11-18: 95 s when that fill ran with cgo on (Job
infra-1109-gocgo-dv7gl, where b1634bff's cgo-on build after the same fill took 26 s), and 16 s,
12 s for the first `go build` and 4 s for the broker's, when it ran with CGO_ENABLED=0 (Job
infra-1109-gobuild-894l9). The same Job built them in 5 s on a build cache of b1634bff's own,
and in 69 s on an empty build cache with the modules already downloaded. On the runner the step
took 108 to 150 s across the full lanes of #3513 on 2026-11-19 and 20, and in run 44823289704
(job 136589534717) both Vite builds beside it took 2 min 5 s, where the same build alone takes
37 s on the same runner type ("built in 36.97s", dispatch-ui job 136589534807). The go build ran
beside them on the same four CPUs, and on a build cache that held almost none of its objects it
is the cold build those Jobs time at a minute and more. That job's restore of go-envoy-test read
723,666,370 bytes, in 19 s.

Why each push to main fills it, and not once per Go version, as the fill first did. That fill
ran only when the key, which names the Go version alone, was missing, on the ground that this
build is small enough for an aged entry to spare most of it: the 16 s above, against 69 s cold.
But the build cache behind the 16 s was filled at 278138ea, 171 of main's commits older than
8a954068, the main commit b1634bff had merged, and an entry filled once per Go version only ages
further until the next Go bump. ci.yaml's own families showed where that ends: the go-daemon,
go-envoy and go-envoy-test entries every job restored on 2026-11-22 had been saved on
2026-11-18, and from them a job at main's e7381b7d4c9 had to compile again 573 of the envoy
module's 618 packages
(.github/ci-notes/ci-caches.md#why-each-fill-builds-on-the-newest-entry-of-its-family holds
the record). Main's #5141 made those families fill on every push, and this family
follows the same design, so the entry this job restores is the last push's, which is as near as
an entry of main comes to the 5 s reading of one filled at the commit built.

The connections step's `go run` carries CGO_ENABLED=0 too, so it reuses the build step's
objects, the store package's closure among them, and the fill runs it as that step does.

go-envoy would serve this job no better: it is filled by a build of ./cmd/listener and
./internal/testnats/cmd/natsd alone, so a job that restores it and builds these four
downloads and compiles their closure on a key that hit: 115 `go: downloading` lines in
job 129055096326 of run 42585736615, where this build still sat inside the alone job's
3-minute browser step, against 4 with go-envoy-test in job 129071749416 of run 42590831386.

## The fill of go-dispatch-browser in ci-caches.yaml

Moved from `.github/workflows/ci-caches.yaml`, whose dispatch-browser job fills the family the
binaries job restores (the section above). In this section "this workflow", "the header" and a job
"above" or "below" are that workflow's, its head comment and its jobs, and "here" is that fill job.

go-dispatch-browser, the family of dispatch-browser.yaml's binaries job alone, its one reader.
It keys on packages/envoy/go.mod's Go, which that job sets up, and it builds what that job
builds, as that job builds it. That job's build step pins CGO_ENABLED=0 on its two `go build`s,
and Go's build cache keys every object on the build's configuration, cgo included: net,
os/user and runtime/cgo differ with it, and so does every package above them. So the
go-envoy-test entry that job restored before, which the envoy-test job above fills on the runner's
default CGO_ENABLED=1 (the runner has gcc), held its modules and almost none of its objects.
Measured in cluster Jobs on 4 CPUs with golang:1.27.3, that step's builds at b1634bff of #3513
took 95 s after a cgo-on build of the same commands at main's 278138ea, the commit main's
go-envoy-test entry was saved at (Job infra-1109-gocgo-dv7gl), and 16 s after the same fill
made with CGO_ENABLED=0 (Job infra-1109-gobuild-894l9);
.github/ci-notes/dispatch-browser.md#why-the-binaries-job-restores-go-dispatch-browser holds
the rest of the readings. The fill also runs that job's connections step's `go run`, with
CGO_ENABLED=0 as that step runs it, so that step reuses the build step's objects.

A job of its own on ubuntu-24.04, the binaries job's runner, and not a step of the x86_64 leg of
the envoy or envoy-test job: a save takes the machine's whole module cache and build cache, so in
such a leg, in whichever order the two builds ran, one entry would carry another family's objects
that its readers cannot use - go-envoy's or go-envoy-test's cgo-on objects in this entry, or this
build's cgo-off objects in theirs - and which it carried would turn on which keys that run
missed. Here the entry holds this build alone. Each push fills it again from its newest entry
(the header says why).

Its output, `go-dispatch-browser-x64`: the key this job saved, named by its architecture as the
other fills' outputs are. Its restore step, `go-dispatch-browser`: the newest entry of the family,
which the builds below compile on, found as the daemon job's restore finds its own.

Its build step, "Build what the binaries job of dispatch-browser.yaml builds": the binaries job's
go commands, run as it runs them, in packages/envoy: its build step's two `go build`s over the
same packages into the same outputs, and its connections step's `go run`, each with
CGO_ENABLED=0. They are copied here rather than run from one script both jobs call, because that
job's go commands stay in its steps' own pinned text
(.github/ci-notes/dispatch-browser.md#what-the-binaries-build-writes-and-why-it-is-static).
Nothing fails when the two drift apart - the binaries job just builds cold again - so a change to
them there is a change here too.

Its two steps around the build step, `.github/scripts/go-cache-working-set.sh begin` after the
restore and `end` before the save, keep in the build cache only what these commands used, as
every fill's pair does
(.github/ci-notes/ci-caches.md#why-each-fill-keeps-only-what-its-own-build-used). Both run on
the default GOCACHE, ~/.cache/go-build, which the build step uses and the restore and the save
name.
The `end` step also [deletes the module zips](ci-caches.md#why-the-fills-delete-the-module-zips),
which the binaries job does not read.

The record of what a family filled once per Go version had to compile again is
.github/ci-notes/ci-caches.md#why-each-fill-builds-on-the-newest-entry-of-its-family.
go-dispatch-browser was filled that way longer, on the ground that its build is small: the
binaries job's commands, built as that job builds them, took 69 s on an empty build cache, 16 s
on one filled with CGO_ENABLED=0 at main's 278138ea, and 5 s on one filled at the commit built
(all three in Job infra-1109-gobuild-894l9, which this section's second paragraph names, and
.github/ci-notes/dispatch-browser.md#why-the-binaries-job-restores-go-dispatch-browser holds the
readings). But 278138ea was 171 of main's commits older than 8a954068, the main commit the
built one had merged, and an entry filled once per Go version only ages further until the next
Go bump, drifting toward the cold build as the envoy module's had.

## What the binaries build writes, and why it is static

Everything the run's other jobs execute, built in one step: the four Go commands, the
dashboard bundle, and on the full lane the second rollout bundle beside them.

One `go build -o <directory>/` writes one binary per package, each named after it:
dispatch, dev-oidc-issuer and requestseed; a second build names the broker's, from
./cmd/cipherd, broker. CGO_ENABLED=0 is pinned on the build
rather than left to the runner's default, so each binary is static by construction: the
shard, real and alone jobs run them in a container of this repository's own image, and a
binary with a PT_INTERP has a glibc there to disagree with. They are written under
$RUNNER_TEMP, so the checkout stays clean.

`-ldflags='-s -w'` leaves out the symbol table and the DWARF debug information, which no
leg reads: a panic's stack trace still names each function, file and line (Go keeps its own
line table for that), and `go version -m` still reads the build information. Without them the
artifact every shard, real and alone leg downloads was 107,568,042 bytes zipped and 248,108,722
unzipped (run 45062410823); stripped, it was 55,287,947 bytes zipped (run 45081810035), 49%
less, both as the run's artifact list gives `size_in_bytes`. The download is where a leg's run
has failed on the network: on 2026-11-22 five leg downloads of about 4,330 stalled for the
step's whole 2 minutes, and three legs failed when the second attempt stalled too (shard 4 of
merge group 45062410823 among them). Fewer bytes shorten a read whose connection slows; they do
nothing for one whose connection stops, which is the second read's to end and resume (the
section on why the second read of the binaries is a script).

`bun run build:web` writes packages/dispatch/web/dist, the dashboard every browser job
of this run serves, and dist/THIRD_PARTY_NOTICES.txt with it: the build fails on a
bundled package whose license cannot be determined, and the listing shows it shipped.
`bun run build:web-b` writes web/dist-b, the second rollout bundle the multiprocess
job's two-builds rows read. DISPATCH_BUILD=e2e-b, which that script carries, is what
gives it a build id of its own: `packages/dispatch/web/vite.config.ts#__DISPATCH_BUILD__`
defaults the id to GITHUB_SHA, and
`packages/dispatch/e2e/two-builds.multiprocess.e2e.ts#test.beforeAll` fails when the
two entries match. It is built on the full lane alone, the only lane that runs the
multiprocess job, so a pull-request run pays nothing for it.

The builds run beside one another, each background job waited on by its own process id:
the step starts its two go builds, chained in one subshell, and
.github/scripts/dispatch-browser-build-dashboards.sh as two background jobs, and that
script starts the two bundle builds of the full lane (the dashboard alone on any other) as
jobs of its own. That is what puts the bundles' time inside the go builds' rather than after
it, and each `wait` is read on its own so that a child which failed fails this step and the
line printed beside it names which build it was: one `wait` under `set -e` would end the
step at the first failure and leave the others unreported. The step takes the runner's
default bash and writes no `shell:` key, since ci_filter_coverage.py's shell_reason counts
one as something that can move what a go command compiles, and the step's whole run: text is
pinned in .github/scripts/ci_job_reads.py's dispatch_browser gate.

Why the bundles are a script and the go builds are not. A `run:` body of more than ten lines is
a script (.github/ci-notes/README.md), and a body that held all four builds, each with its
wait and its error line, would run past ten. The go builds cannot be the script's: a go
command a script holds is a script Run of the dispatch_browser gate, and that gate can hold
none, since around_job reads every job of this workflow and the shard, real and alone jobs'
`container:` drops such a Run out of check-ci-filters.sh's reduction (ci_job_reads.py's
comment on the gate says so). So the two go commands stay in the step's own pinned text, and
the two bundle builds, which start no go command, are the script, with the same commands, the
same waits and the same error lines. The copy under $RUNNER_TEMP is made once both bundles
have built, whether or not the go builds have ended; a go build that fails still fails the
step, and the uploads after it, which carry no `if:`, do not run.

The two trees are copied under $RUNNER_TEMP for the upload after the step, so that archive
holds `dist/` and, where there is one, `dist-b/` at its root whichever lane ran: an upload of
two patterns takes its root from the least common ancestor of the patterns that matched,
which is a different directory on a lane that built no dist-b.

## Why the binaries job reads the stacks' connection limit for the whole run

The stacks' connection limit, which e2e/harness-postgres.ts would otherwise compile and
print with `go run` (DISPATCH_E2E_PG_CONNECTIONS): read here, once for the run, and taken
from this job's `connections` output by every job that starts stacks. The shard, real and alone
jobs' containers have no Go, and the multiprocess and visual jobs set none up, so
this is the one job of the file that runs it. The same figure is the file's `connections`
output, by which the references lane's call takes it with the pull-request lane's build, and
that call's shard job reads it through its `connections` input
([the references lane's section](#when-the-references-lane-takes-the-pull-request-lanes-build)).

## Why the binaries artifact's name holds no run attempt

One name per caller for the whole run, `<job>-binaries`, with no `github.run_attempt` in it: a
re-run of a run's failed jobs re-runs a shard or an alone leg without this one, and that attempt
must find the artifact this job uploaded on the attempt before.

## Why the dashboard artifact is one for the run, and where it lands

The dashboard beside the binaries, under the same rule and for the same reason: one name per
caller for the whole run, `<job>-dashboard`, with no `github.run_attempt` in it. A job downloads
it into packages/dispatch/web, where it becomes the web/dist that `bun run build:web` would have
written there, and on the full lane the web/dist-b beside it.

## The shard job, its stacks and its container

One leg of the lane's rows. Each leg runs the lane's projects with `--shard=N/2` over four
Playwright workers, and the run's `globalSetup` starts one stack per worker, each with a
Postgres, a harness and a file-store bucket of its own (packages/dispatch/e2e/
harness-pool.ts), so this job has no `postgres:` service and no job-level DATABASE_URL:
one database cannot serve stacks that truncate it per row, and a run of more than one
stack starts a server per stack and hands each its own (e2e/harness-postgres.ts).

A leg's half is a half of the lane's row time, not of its rows. Playwright's own `--shard`
cuts the rows into runs of equal count, one a leg, and rows do not cost the same: at main
f02514268a, on main's four legs, it gave the full lane's heaviest leg 1.115 to 1.225 times the
mean row time, over the per-row medians of two sets of 13 and 14 of main's runs (main's pull
request #4987 names them), and leg 2, all chromium rows, was the slowest leg of its run in 55
of the 85 runs of 2026-11-21/22 whose four legs all ran. packages/dispatch/e2e/shard-reporter.ts,
which the browser step names beside the failure reporter, takes the split over: every leg
places every file of every project the lane selects by the row times main measured
(packages/dispatch/e2e/shard-weights.json), heaviest first onto the lightest leg, and keeps its
own files. The same two sets put that placement at 1.007 and 1.008 times the mean on four
legs; it has not been read on two, which takes those runs' reports.
packages/dispatch/e2e/shard-split.ts says why Playwright 1.64.1 has no split by time of its own,
why the file is the unit, how every leg computes one placement, and what a file with no
measured time is placed by; `bun e2e/tools/shard-weights.ts --main <count>` reads the table
again. Each leg prints its own line before its first row, `shard N of 2, placed by measured row
time: ...`, with the rows of it that were placed by an estimate.

Each leg also writes two row lists into the rows/ directory its blob upload carries: every row
of the selection it read, and the rows Playwright gave it to run
(DISPATCH_E2E_LEG_ROWS_DIR in .github/scripts/dispatch-browser-shard-suite.sh). The merge
job's reading one holds the two legs' lists against each other and fails by name on a row of
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
binaries job's pinned build step, "Build the binaries and the dashboard the browser suite runs", is read under
dispatch-browser.yaml's own `env:` and defaults, so neither may grow a name outside that set.

The job's budget, and the browser step's before it became a hang guard, are measured at this
job's shape: two legs, in the image dispatch-browser.yaml pins (sha256:69c3a316). The reading is
GitHub's jobs API over the nine runs whose browser jobs ran at that shape, 18 legs in all -
43045412941, 43074443876, 43079610268, 43150379433, 43192333526, 43240954914, 43245766326,
43251567200 and 43347417247, the last at 90ee14dd - read with
`gh api 'repos/cybermonklabs/legion/actions/runs/<id>/jobs?filter=all&per_page=100'`, each
step's `completed_at` less its `started_at`. Seventeen legs ended green. Shard 2 of run
43150379433 (job 130958861121) ended red, and its browser step ran 597 s, inside the spread
of the others, so its seconds count too.

The legs got slower across those runs. Shard 1's browser step read 498 to 607 s in the
three runs of 2026-11-11, 745 s in run 43251567200 and 769 s in run 43347417247. Shard 2's
read 532 to 636 s on 2026-11-11 and 750 s in run 43251567200, about a quarter more. So the
budget takes the maximum over every run, as the rule says, and not a median. The maximum
each step reached over those 18 legs and every leg of pull request #3513's runs of
2026-11-15 - its eighteen full-lane runs (43656706589, 43664675139, 43667557013, 43669817025,
43670689833, 43672161759, 43674112121, 43676043057, 43687205491, 43689921885,
43692383387, 43716814270, 43720076901, 43723836932, 43731214579, 43744059348,
43747732723 and 43751151528) and the shard legs of its 24 pull-request runs that
concluded by 20:50Z - read the same way, in seconds:

  step                                                 max
  Set up job                                             2
  Initialize containers                                 50
  actions/checkout                                      13
  Start an S3-compatible server for uploaded files       1
  actions/download-artifact, the binaries               12
  Make the downloaded binaries executable                1
  ./.github/actions/setup-bun                            4
  bun install --frozen-lockfile                         15
  Build contracts for dispatch e2e                       1
  actions/download-artifact, the dashboard               3
  Read the lanes' PostgreSQL and time its provider       6
  Wait for the S3-compatible server                     10
  Browser-test dispatch dashboard                      835   (carries its own budget)
  upload-artifact, this leg's blob report                3
  What the run left behind                               1
  upload-artifact, the record directory                  2
  upload-artifact, the failed rows' traces               4
  Post Run ./.github/actions/setup-bun                   1
  Post Run actions/checkout                              1
  Stop containers                                        1
  Complete job                                           1
  the whole job                                        913

The rule is in this file's head. The step's maximum is 835 s: shard 2 of run 43667557013
(job 132671046743, 2026-11-15), above the 810 s of shard 2 of run 43692383387 (job
132746399858), the most any other leg of those runs read, and above every earlier
reading - 769 s in shard 1 of run 43347417247 (job 131631089585), 750 s in shard 2 (job
131300609469, run 43251567200), and the 780 s floor of job 131642902948, which the old
13 minutes stopped before its end. Taken at 835 s the rule gave the step 835 x 1.15 =
960.25 s -> 17 min, and the job that 1,020 s plus the maxima above of every step without a
budget, which sum to 132 s - each of the three uploads counted, since each is a step:
3 + 2 + 4, not 4. The new maxima are `Initialize containers` 50 s (job 132900324043, a
pull-request leg), the binaries download 12 s (job 132677831857) and `bun install` 15 s (job
132696863686, a pull-request leg). 1,020 + 132 = 1,152 s, 19 min 12 s, rounded up to 20
minutes: a backstop above the sum of the step budgets, as this file's head says a job budget
is, and above the 913 s the slowest leg took whole (job 132671046743), and the 860 s of job
131642902948, the leg the old 13 minutes stopped.

The browser step's 17 minutes are a hang guard of 32 now, for the reason this file's head
gives and
[the step's section](#why-the-shard-job-calls-playwright-directly-and-what-its-browser-steps-budget-is)
derives: on that step the rule's maximum is a verdict on the runner. The largest leg of the
three attempts of the full lane at e2399655, 985 s (shard 1 of run 45151841335, attempt 1), is
already past the 835 s the 17 minutes were read from, and on the slowest runner main's legs
met, which ran the same rows 1.75 times slower, it would run 1,724 s and be cut at 1,020 with
its rows passing. The job's budget below carries the 32.

THE TWO NETWORK READS, AT A BUSY HOUR. That sum read the binaries download at 12 s and the
image pull, `Initialize containers`, at 50 s, and on 2026-11-18 a leg of each job of the
default branch's shape stalled on one of them while its siblings did not: in run
44228250420 shard (4)'s download of dispatch-e2e-binaries (106 MB, 101 MiB) took 428 s
where the run's six other legs took 2 to 6 s, and the job budget cancelled the leg at about
13 minutes; in run 44213916667 alone (chromium-alone) spent 355 s pulling this image
(1,161 MB) where the two other alone legs took 38 and 39 s, and its budget cancelled it 3 s
into its checkout. The pull's time was one layer: the other nine arrived in 6 s and the
122 MB Firefox layer in 346 s, and shard (4) of that run, pulling in the same minute, took
200 s on two layers. A merge group drops its entry for a cancelled leg as for a failed one,
and every group behind it is built again. So the download now carries a budget of its own
and a second attempt
(.github/ci-notes/dispatch-browser.md#what-the-shard-job-downloads-in-place-of-a-go-build),
and the pull is counted at that 355 s: no step
holds it, so no step budget can cut it and no step can read it again, and Docker's own
retry ("Retrying in 5 seconds" in that log) answers a failed read, not a slow one. The
job's budget is the browser step's 1,920 s, the two download attempts' 120 s each, and the
steps without a budget at 132 - 12 - 50 + 355 = 425 s: 2,585 s, 43 min 5 s, rounded up to
the 44 minutes of the shard job's `timeout-minutes`, whose 55 s left over hold the step that
places the binaries. The browser step's own budget still fires before the job's.

The readings these replace were taken at other shapes, and no number of this section rests on
them now. The browser step's 13 minutes were derived by halving the browser steps of four legs,
in runs 42334424798 and 42316487530, since no two-leg run existed then. The table was the
maxima of 24 four-leg legs in five runs on the image build sha256:ba003193 (run
42480325123). Two rows moved most at this shape: `Wait for the S3-compatible server`, 24 s
there and 10 s here, now that the store starts just after the checkout and comes up while
the install runs, and `bun install --frozen-lockfile`, 62 s there and 15 s here. The browser
step's hang guard takes main's runner factor and drift from main's four-leg runs, and its leg
from this job's own two-leg full lane at e2399655, run 45151841335's attempts 1 to 3; its
section says which is which.

The rows: listed at `fc1baefb` on 2026-11-16 with `bunx playwright test --config
e2e/playwright.config.ts --list` from packages/dispatch, the suite holds 1,511 rows in 206
files (`--project='*'`), which is two listings rather than one: the three `-alone`
projects are out of the default selection since dispatch://INFRA-1280, so a command that
names no project lists 1,460 rows in 203 files and one that names those three lists the
other 51, in 3 files. The ten projects this job's shard command
(.github/scripts/dispatch-browser-shard-suite.sh) names on the full lane
hold 1,438 rows in 202 files. The four the pull-request lane names hold 1,325 in 200, of
which that lane's `@pr` grep selects 289, in 148 files. The two `visual` projects hold
22, which the visual job runs one row at a time, and the 51 wall-clock rows are the alone
job's.

## Why the leg count stands in three places

Two legs. The count is written in the shard job's `matrix`, in the `--shard` denominator of
the browser step (`.github/scripts/dispatch-browser-shard-suite.sh#--shard=$shard/2`) and
in that step's own name, "Browser-test dispatch dashboard (shard ${{ matrix.shard }} of 2)".
.github/scripts/ci_job_reads.py's BROWSER_SHARD_SPECS copies that name to cite the step by,
and prose repeats the count, among it the shard script's head (the step's name, and "this
leg's half of their row time") and its note on the projects it names ("these two machines"),
docs/testing.md's leg command (`--shard=1/2`) and the comment on
`packages/dispatch/e2e/accessibility.e2e.ts#scannedHere` ("one of its two
machines"). So the three move together and those copies with them: a denominator larger than
the leg list would leave the rows of the missing legs unrun, which the merge job's reading one
names only once the run has spent its legs (the next paragraph), and a name disagreeing with
the denominator would misreport every leg.

The split by row time (packages/dispatch/e2e/shard-reporter.ts) reads the count from that same
`--shard`, and packages/dispatch/e2e/__tests__/shard-split.test.ts, which lists the legs that
split gives against the real config, reads it from the shard job's `matrix`, so neither is a
fourth place. A denominator larger than the matrix is not silent any more: each leg then places
files on legs that never run, and the merge job's reading one names their rows as rows of the
selection that ran on no leg (packages/dispatch/e2e/tools/leg-rows.ts).

Four of them until dispatch://INFRA-1109, where four legs paid four container pulls,
four Bun installs and four dashboard builds to run one lane's rows.

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

It is started first of the job's own steps, right after the checkout, and polled for
readiness in the step just before the browser step, "Wait for the S3-compatible server",
where that poll has always been. It stood beside its poll, after the install, until
dispatch://INFRA-1109: the start costs 0 s and the poll cost 16 to 23 s a leg in run
42809067358, all of it the volume server coming up while nothing else was running. Started
here it comes up behind the two downloads and the Bun install, and the poll read at most 10 s
a leg in pull request #3513's runs of 2026-11-15 (job 132690968751). The poll is not weakened
by the move - it is the same bucket PUT, the same object write, read and delete, and the same
60-attempt ceiling.

Two flags after `-dir` are the image's, not weed's own defaults, and they come with the
binary because its entrypoint does not: that image's /entrypoint.sh adds
`-volume.max=0 -master.volumePreallocate -master.volumeSizeLimitMB=1024` to every
`server` it runs, and copying the binary out left them behind. `-volume.max=0` is the
one that bites. Without it a volume server takes 8 volumes and no more, the master
grows seven for the first bucket it is asked about, and every later bucket is refused
with `topo failed to pick 1 from 0 node candidates` and
`create 7 volume, created 0: Not enough data nodes found!`. The suite makes one bucket
per stack, so the readiness wait's own bucket worked and ten upload rows of run
42409479235 failed on 502s from the Dispatch server's PutObject, every one of them for
a stack's own bucket.

The third, `-master.volumePreallocate`, is left out. With it, each volume a bucket grows
reserves its whole 1,024 MB on disk as it is made, one volume at a time on the one volume
server, seven for each bucket's first write. On a runner whose disk is slow that reservation
holds every other bucket's first write behind it: in attempt 2 of run 45261148766 (shard 1,
job 138020620825, its record artifact's weed.log) each 1 GB volume took 2.5 to 3.5 s, one
bucket's grow took 16.8 s, the next bucket's assign was cancelled at 10 s ("failing to
assign a file id: rpc error: code = Canceled"), and `artifacts.e2e.ts` "the artifacts tab
filter narrows rows by name and kind" failed on its upload's 502 twice. In main's merge run
45242914629 and in attempt 1 of run 45261148766, every leg grew its 42 volumes with no grow
longer than 1.0 s. Without the reservation a volume is an empty file that fills as it is
written, as much as the suite's uploads need, and its 1,024 MB limit is unchanged.

The two flags after those, `-master.raftHashicorp -master.raftBootstrap`, are this job's own.
A one-node master on weed's default raft elects itself only after about 19 s: the log of
run 45048924209's shard 4 has "Starting RaftServer" at 17:33:08.6 and "is the leader" at
17:33:27.5, and the S3 gateway answers only after that, so on main, whose start step stood
just before its poll, the readiness wait below took 23 s at the median of 3,247 shard legs on
2026-11-22 (p90 24 s). With HashiCorp's raft and a bootstrap of the empty data directory each
run starts with, the master is leader within about a second of its start. Measured with the
weed 3.97 binary from that release on a loaded developer machine, start to an object written
and read back: 31 to 37 s with the default raft, 10.5 to 13 s with these two flags, leader at
1.4 s. Neither flag changes what the store keeps or serves: the data directory is new in every
run, so there is no earlier raft state to resume.

So, for whoever moves the seaweedfs row of .github/ci-images.yaml to a new version:
read that image's /entrypoint.sh again and reconcile the image's flags with its `server`
case - the two this job passes, and the one it leaves out for the reason above - because they
are the script's and this job runs the binary without it. The row pins a digest, so the
entrypoint cannot change under this job between bumps, which is what makes the rule a read at
bump time rather than a check on every run. Read it with
`crane export ghcr.io/cybermonklabs/legion/ci@<the row's digest> - | tar -xO entrypoint.sh`.

## What the shard job downloads in place of a Go build

What the binaries job built, in place of this job's own actions/setup-go
and go-envoy cache restore, which are gone: with no go command left here they bought
nothing, and the restore was there only for the build run-server.sh no longer does.
The directory is the one run-broker.sh built the broker's three binaries in, which is
the directory `packages/dispatch/e2e/real-broker.ts#requestseed` points into, so the suite runs requestseed from there; the server
binary rides along in it and is named by path in DISPATCH_E2E_SERVER_BIN (.github/scripts/dispatch-browser-shard-suite.sh).
In a call given `build-job` both attempts read that caller's artifact, `<build-job>-binaries`,
in place of the call's own
([the references lane's section](#when-the-references-lane-takes-the-pull-request-lanes-build)).

A BOUND OF ITS OWN AND A SECOND ATTEMPT. In run 44228250420 a read of this artifact took 2
to 6 s on every leg but one, which took 428 s (the budget note above), so a slow read is
answered by reading again rather than by waiting. Each attempt carries this file's floor of
2 minutes, twenty times the longest of that run's other reads. That is not this file's rule
of the measured maximum plus 15 per cent, which the 428 s read would put at 9 minutes: those
9 minutes are the wait this bound is here to cut, since the same bytes took seconds on that
run's other legs. The second attempt runs only when the first did not end in success, and a
second failure fails the job there, by name. The second attempt is a script and not the
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
given the id the action's read in its job names: `${{ inputs.build-job || inputs.job }}` in
the shard job and `${{ inputs.job }}` in the real and alone jobs. So callers that share a run - a pull
request labelled full-ci runs dispatch-browser beside dispatch-browser-pr, and one that changes
what the reference rows test runs dispatch-references beside it - each read their own build's
binaries, but the references lane where it takes the pull-request lane's, which both of its
reads then name ([its section](#when-the-references-lane-takes-the-pull-request-lanes-build)).
A re-run uploads them again under the same name, and the action takes the highest id
of the run's artifacts of that name (run 45306887784 held three by its attempt 3, and the highest
id was not the newest by `created_at`), so the script takes that same one and reads its address,
size and digest from it.

## What the shard job places, and what its budget leaves for it

The step budget note above does not count this step. Main's #4383 added it on 2026-11-18, with
the second download attempt, to copy the attempt that completed out of its own directory. It
copies the four binaries on the runner's disk (141 ms for the 106 MB artifact of the unstripped
build on a pod, when #4383 measured it on 2026-11-18), and the seconds the job's head leaves
over when it rounds the job's sum up to its `timeout-minutes` hold it.

## Why the shard job makes the downloaded binaries executable

actions/upload-artifact keeps no executable bit, so all four arrive at mode 0644.
Both run-server.sh, in its check of DISPATCH_E2E_SERVER_BIN, and run-broker.sh refuse a path
that is not executable, and that refusal would otherwise surface in a stack's own log, far
from this cause. The real, multiprocess and visual jobs also make the downloaded commands executable:
they download the same four binaries, which arrive at the same mode.

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
block leaves out - not ./legion-api, which no row loads. It is the one build left in this
job: the binaries job builds everything else this job runs, and contracts is not in that
artifact, since what Playwright resolves is the package inside this checkout.

## What the shard job downloads in place of its dashboard build

The dashboard the binaries job built, in place of this job's own `Build dispatch
dashboard` step: the artifact holds the trees themselves, so this writes web/dist where
`bun run build:web` writes it, which is what each stack's server serves. On the full lane
the archive also carries web/dist-b, the second rollout bundle only the multiprocess job
reads. In a call given `build-job` the download reads that caller's `<build-job>-dashboard`,
the same web/dist that caller's lane builds
([the references lane's section](#when-the-references-lane-takes-the-pull-request-lanes-build)).

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

## Why the store's readiness wait writes, reads and deletes an object

The readiness wait of the file store the step after the checkout started, kept here,
last before the browser step, where it has always been. An anonymous PUT makes the
bucket, as each stack makes its own (dispatch-e2e-files-worker-N) with the same
request (e2e/harness-pool.ts). Then an object is written, read back and deleted,
because a bucket PUT is answered by the filer alone and says nothing about the volume
server the objects land in: run 42404500479's job 128499619657 passed this wait with
a bucket and then failed ten upload rows nineteen minutes later, every one of them a
502 from the Dispatch server's own PutObject. A store that cannot take an object now
fails here, by this step's name, with the log the start step, "Start an S3-compatible
server for uploaded files", redirected -- which is what a job in a container has in place
of `docker logs`.

## Why the shard job calls Playwright directly, and what its browser step's budget is

`bun run e2e` builds the SPA itself, which would build it a second time in this run.
Calling Playwright directly runs the dashboard the binaries job built and the job's
dashboard download downloaded, and leaves this job no Vite build at all. Safe because that
build and one here would be the same
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
.github/scripts/dispatch-browser-samples.sh: MemAvailable and where the CPU goes, at the
interval its head names, for the length of this step, started by
.github/scripts/dispatch-browser-shard-suite.sh and read back at the end of it.
Its head says what each reading holds and what it does not; on this job `everything
else` holds the file store too, which runs beside the stacks. Both sample files are
written into the record directory that the step "What the run left behind" collects and
the upload after it uploads, under
$RUNNER_TEMP beside the stacks' own run directory: a file this step wrote into the
checkout would leave it dirty afterwards. The blob report goes beside the list
reporter's output, so a green run reports what it ran too.

The step's budget, `timeout-minutes: 32`, is a hang guard. It is not the maximum plus 15 per
cent of this file's head, because on this step that rule made the limit a verdict on the
runner: on main's four legs the 11 minutes it gave cut shard (4) of run 44985914477 (job
137096923636) with 401 rows passed and none failed, on a machine that ran them slower than
another of the same minute, and the 17 minutes it gave this job's two legs (835 x 1.15, the
job's head) would cut this job's largest leg, 985 s, on such a machine the same way. The
formula is main's (#4987): a leg's time, times the slowest runner seen, times the worst drift
a row-time table up to a week old was measured to give. The leg is this job's own; the runner
and the drift are main's, read from main's four-leg runs. How the 32 minutes are read:

- The leg: 985 s, the largest browser step of this job's two legs over the three attempts of
  the full lane at e2399655, under main's split by time: run 45151841335, a workflow_dispatch
  of ci.yaml on legion/INFRA-1109, and two re-runs of its browser jobs from the binaries job
  on, read on 2026-11-23 from each attempt's jobs in the jobs API, the step "Browser-test
  dispatch dashboard (shard N of 2)": attempt 1, shard 1 985 s and shard 2 667 s; attempt 2,
  901 s and 733 s; attempt 3, 838 s and 800 s. The heavier leg's step is 1.193, 1.103 and
  1.023 times the two legs' mean in those attempts, inside the 1.225 times the mean main's
  count split gave its heaviest leg, so e2e/shard-weights.json is not read again for them. The
  red full lane before them at the merged split, run 45122396660 at 53286717 (930 s and 817 s),
  is not counted: each of its legs ran a failing row's two attempts of 46 s each, a row fixed
  since.
- The factor: 1.75, the slowest runner seen. The leg that was cut ran its 401 rows at a
  per-row median 1.75 times the time job 137096931568 of run 44985911309 took for the same rows,
  started 3 s later (p10 1.42, p90 2.12; 660 s against 391 s of step). It is a figure of the
  runner, not of the leg count.
- The drift: 1.11, main's figure, the worst share of the mean the heaviest leg took under a
  row-time table (packages/dispatch/e2e/shard-weights.json) up to 7 days old, the lane placed
  on four legs. Read on 2026-11-22 with main's own code - each table built by
  `packages/dispatch/e2e/tools/shard-weights.ts#weightsOf` from the `report.json` of each run's
  `dispatch-browser-report-1` artifact, the lane placed by `placeUnits` - over two tables:
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
  so where those files land is close to chance. 1.11 is the worse of the two. It is a figure of
  four legs that has not been read for two: placing those two tables' lane on two legs needs
  the reports the tables were built from, which nobody has read for it. Two legs share the
  same misplaced files over twice the row time each, which should give the heaviest a smaller
  share rather than a larger one, so 1.11 is kept until it is read. Without it, the 985 s leg
  on the slowest runner seen under such a table would end at about 1,913 s, past the
  29-minute guard 985 x 1.75 = 1,724 s would give, and be cut with its rows passing.
- 985 s x 1.75 x 1.11 = 1,913 s, rounded up to 32 minutes, 1,920 s.

The guard was set three times before, and every one of those readings is replaced by the one
above. First it was carried over as main's own leg figure rescaled to two legs (main's p90 of
its 347 four-leg legs, 534 s, as 1,044 s for half main's row time, and 34 minutes), which
rested on main's rows and main's four-leg runs rather than on any leg this job ran. Then it
was set from this job's own legs before the merge 01ca837e brought main's split: 855 s, the
largest of the eight browser steps of four full lanes that ran under Playwright's count split
(runs 44997458055, 44997506117, 44997560086 and 45005495938, 666 to 855 s), and
855 x 1.75 x 1.11 = 1,661 s, 28 minutes. Third it was set from the first lanes that ran the
split this job runs now, the three green full lanes at a650b091, a6d8e108 and 5e22df0a (runs
45130372249, 45130540632 and 45130647066, whose six legs read 596 to 901 s): 901 s, shard 1 of
run 45130540632, and 901 x 1.75 x 1.11 = 1,750 s, 30 minutes, with the shard job's
1,800 + 120 + 120 + 425 = 2,465 s, 42 minutes. The full lane at e2399655 ran a larger leg under
the same split, 985 s, and that leg is the one above.

What a hang looks like to it. A row cannot hang it: each row ends at its own budget, the 60 s of
`packages/dispatch/e2e/playwright.config.ts#testTimeout` or the longer one a row sets, and the leg goes on. What
reaches 32 minutes is a leg that has stopped running rows - a stack that never comes up in
`globalSetup`, a teardown or a blob write that never returns, a worker that stops reporting -
whose log has stopped gaining rows. The runner cancels the step and names it (`The action
'<step name>' has timed out after 32 minutes.`). The leg's blob report is never written, since
Playwright writes it when the run ends: the always-run upload finds no file (run 44985914477's
cut shard 4 logged `No files were found with the provided path: /__w/_temp/leg-report`), and
the merge job says so by name - reading one's `no blob report arrived from: shard-4` in that
run, `shard-2` for this job's second leg, and reading three's `... uploaded no blob report, so
the merged report holds none of its rows` - and fails on the leg's result. No row of a green leg
rests on the guard: the 985 s leg on the slowest runner seen, under a table up to a week old,
ends at about 1,913 s, under it. A red leg can pass it: a failing row's retries lengthen its
leg. Red shard 1 of run 45122396660, 930 s, would end near 1,807 s on that runner under such a
table, under the guard now, but a red leg longer than about 988 s on an ordinary runner
(1,920 / 1.75 / 1.11) would pass it there. That leg is cut, and the merge job names it as a
leg with no blob report rather than naming its failing rows; its own log still holds them.
What the guard costs is time: a hung leg is cut 15 minutes later than at the 17 the rule gave
this job's legs.

The week is what the guard holds, so each leg says when its table is past it: below its
placement line it prints a `::warning::` when e2e/shard-weights.json was read more than 7 days
before the run, or when a tenth of its rows sit in files the table has no entry for
(`packages/dispatch/e2e/shard-split.ts#tableWarnings`). Either one means
`bun e2e/tools/shard-weights.ts --main 17` is due; past the week, at 1.21, the share main's
count split gave the heaviest of its four legs, the 985 s leg on that runner would end near
2,086 s, past the guard.

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

A failed row leaves its trace and the failure screenshot here. The trace holds the row's
actions, console and sources. It has no DOM view and no network: the config records it with
`snapshots: false` and `screenshots: false`, since a trace is recorded on every row's first
attempt and either one costs every row on a CPU-bound machine (the config's comment gives
the measurements), and Playwright 1.64.1 records a trace's network only beside its DOM
snapshots. The screenshot is the one picture of the page at the failure. A trace with the DOM
and the network comes from a local run of that row: set `snapshots: true` in the config's
`trace` option, then, from `packages/dispatch`, run
`bunx playwright test --config e2e/playwright.config.ts --project=<project> e2e/<file>:<line> --retries=0 --trace on`.
`--trace on` alone does not give one: it replaces the option's mode and keeps its
`snapshots: false`.

## The pull-request lane's real and stub jobs

Both jobs call .github/scripts/dispatch-browser-pr-suite.sh with four workers and the same
whole-token `@pr` grep as `tagged("pr")`. The `real` job runs `chromium` and `iphone` in the
browser image and starts four stacks and the same file store as a full shard. The bare
`ubuntu-24.04` `stub` job runs `stub` and `stub-phone` with `DISPATCH_E2E_STUB_ONLY=1`: the static
server serves the downloaded `web/dist` and the rows replay their recordings. It downloads no
Go binaries, builds only contracts and starts neither PostgreSQL nor SeaweedFS.

Both stub projects launch Chromium, so the runner installs Chromium without `--with-deps`
or apt. It checks that `fc-match sans-serif` selects the Book style of DejaVu Sans before
running the rows, including the 35 layout-rule rows.

Neither job passes `--shard`. Each writes a distinct blob and row count (`report-real.zip`
and `rows/real.txt`, or `report-stub.zip` and `rows/stub.txt`). Their uploads include the
caller and attempt, as the shards' do. Only the sharded lanes need selection and coverage
lists to check that their split assigned each row exactly once.

The browser step's 14 minutes are a hang guard, not a machine-time budget. Among 24 pairs of
successful PR shards on #3513 listed from November 24 onward through run 45896666935, the
largest sum of the two browser steps was 460 s (272 + 188 s in run 45771499770). Using the
sum for either leg and the shard guard's slow-runner factor gives 460 × 1.75 = 805 s, rounded
up to 14 minutes. This estimates rather than measures the new legs; there is no
shard-placement drift to allow for.

The real job borrows the shard job's two 120 s download attempts and 425 s for other steps.
It adds the 4-minute embedder step and 4 s for its failure upload, giving 1,749 s, rounded up
to 30 minutes. The stub job takes the 4-minute Chromium install and 103 s for the other
steps: 94 s borrowed from the bare multiprocess job, plus 9 s for the font check and the
record and blob uploads. That totals 1,183 s, rounded up to 20 minutes. The committed lane
ceiling stays 900 machine-seconds including merge.

## The alone job, and what it keeps the same as the shard job

The three projects whose rows are read from the wall clock, on one machine. They run one
after another at one worker, and a wall-clock project takes nothing else with it - it
names no `dependencies` since dispatch://INFRA-1280 - so nothing else is on the machine
while any of them measures. The full lane alone: a pull request runs no clock row,
since its real job's machine, like a shard job's, cannot hold one still enough to measure.

One machine for the three, where this job had a matrix leg each
(dispatch://INFRA-1109). What a leg bought was three machines reading the clock at once;
what it cost was three container pulls, three checkouts, three Bun installs and three
dashboard builds for 139 s of rows - in run 42809067358 the three browser steps took 50,
35 and 54 s inside jobs of 140, 134 and 153 s. The rows are read no differently here:
one worker, one project at a time. What the sampler's two readings cover did
change: the three projects run inside ONE browser step now, which is one sampler run and
one report, so the readings say whether the machine was quiet across the three together
and no longer whether it was quiet while each of them ran. A reading that shows the
machine busy therefore names the step and not the project, and which project was measured
against a loaded machine is read from the row times in the report beside it.

It shares its setup with the shard job, but starts one worker rather than four, runs the
wall-clock projects through .github/scripts/dispatch-browser-alone-suite.sh, and needs no
file store. The counting-embedder step runs only after the clock rows finish, so those rows
still measure on a quiet runner. The sampler and PostgreSQL reading are shared scripts.
The container, the `sh -e {0}` rule and the `env:` rule of the shard job's head hold here
too, and so do their reasons.

## The alone job's measured steps and its budget

This job takes the container because it runs webkit-alone and firefox-alone, which launch
the two engines the image exists for; chromium-alone runs in it beside them, which is what
keeps one job list and not two (dispatch://INFRA-1443). The shard job's head says what
each option is for and why the digest is a literal.

The budgets, the browser step's `timeout-minutes: 4` and the job's, are measured at this
job's shape: the three projects on one machine, in the image dispatch-browser.yaml pins
(sha256:69c3a316). The reading is the shard job's: GitHub's jobs API over the same nine runs,
43045412941, 43074443876, 43079610268, 43150379433, 43192333526, 43240954914, 43245766326,
43251567200 and 43347417247, the last at 90ee14dd, each step's `completed_at` less its
`started_at`. This job ran once in each of them, and green each time. Its browser step read
156, 123 and 135 s on 2026-11-11, 143, 170, 169, 140 and 170 s on 2026-11-12, and 135 s at
90ee14dd; the job read 192 to 233 s. The maximum each step reached over the nine jobs and the
18 of pull request #3513's full-lane runs of 2026-11-15 (43656706589 to 43751151528), read
the same way, in seconds:

  step                                                 max
  Set up job                                             2
  Initialize containers                                 46
  actions/checkout                                      10
  actions/download-artifact, the binaries                7
  Make the downloaded binaries executable                0
  ./.github/actions/setup-bun                            4
  bun install --frozen-lockfile                         12
  Build contracts for dispatch e2e                       1
  actions/download-artifact, the dashboard               3
  Read the lanes' PostgreSQL and time its provider       4
  Browser-test dispatch dashboard                      170   (carries its own budget)
  upload-artifact, this leg's blob report                2
  What the run left behind                               1
  upload-artifact, the record directory                  2
  upload-artifact, the failed rows' traces               1
  Post Run ./.github/actions/setup-bun                   1
  Post Run actions/checkout                              1
  Stop containers                                        1
  Complete job                                           1
  the whole job                                        233

The rule is in this file's head. The browser step's 170 s is run 43192333526 (job
131096351085) and run 43251567200 (job 131300609244): 170 x 1.15 = 196 s -> 4 min. These
are the projects whose rows are read from the wall clock, so a slower machine moves them
further than it moves a parallel row. The 15 per cent the rule adds is what carries that,
and both of the slowest readings are runs of 2026-11-12, the day the machines got slower
(this file's head). A step that outruns the budget fails by name, which is a reading of
its own. The job's budget is that 240 s plus the maxima above of every step without a
budget, which sum to 99 s - each of the three uploads counted, since each is a step: 2 + 2
+ 1, not 2. The day's runs moved seven of them: `Set up job` 2 s (job 132697146332),
`Initialize containers` 46 s (job 132904452897), the binaries download 7 s (job
132671046700), setup-bun 4 s (job 132662152627), `bun install` 12 s (job 132926233794),
the dashboard download 3 s (job 132680625798) and `What the run left behind` 1 s (job
132730913813); their browser step read at most 146 s (job 132671046700). 240 + 99 = 339 s,
5 min 39 s, rounded up to 6 minutes: a backstop above the sum of the step budgets, as this
file's head says a job budget is, and above the 233 s the slowest of these jobs took whole
(job 131262995896, run 43240954914).

The shard job's note "THE TWO NETWORK READS, AT A BUSY HOUR" holds here too, and one of its
two legs is this job's: in run 44213916667 alone (chromium-alone) spent 355 s pulling the
image, and the 6-minute budget cancelled it 3 s into its checkout. The download carries the
same budget and second attempt as there, and the pull is counted at that 355 s. The job's
budget is the browser step's 240 s, the two download attempts' 120 s each, and the steps
without a budget at 99 - 7 - 46 + 355 = 401 s. The second browser step adds 240 s and its failure
upload adds 4 s, for 1,125 s total, rounded up to 19 minutes. The wall-clock browser
step's 4-minute budget is unchanged.

The readings these replace were taken at other shapes, and no number here rests on them
now. The browser step's 8 minutes were derived from the sum of three legs' browser steps,
116 + 111 + 141 s in run 42316487530, when each project had a machine of its own. The
table was the maxima of nine three-leg legs on the image build sha256:ba003193, in runs
42481269619, 42483357068 and 42487610878, where `./.github/actions/setup-bun` read 20 s
against 3 s here.

## What the alone job downloads in place of a Go build

What the binaries job built, in place of this job's own actions/setup-go
and go-envoy cache restore, which are gone: the shard job's step of this name says why,
and this job runs the same two scripts from the same directory.
The shard job's steps of these names say why each attempt carries its own budget and
directory and why there is a second; the four steps are the same here.

## Why the alone job makes the downloaded binaries executable

actions/upload-artifact keeps no executable bit, so all four arrive at mode 0644.
Both run-server.sh, in its check of DISPATCH_E2E_SERVER_BIN, and run-broker.sh refuse a path
that is not executable, and that refusal would otherwise surface in a stack's own log, far
from this cause.

## Why the alone job builds contracts

The shard job's step of this name, for the reason it gives: Playwright's Node runner
reads `@legion/contracts` out of a build this checkout has to make before the config
loads, and the entries Dispatch imports are all it needs.

## What the alone job downloads in place of its dashboard build

The dashboard the binaries job built, in place of this job's own `Build dispatch
dashboard` step: the artifact holds the trees themselves, so this writes web/dist where
`bun run build:web` writes it, which is what this job's stack serves. The full lane is
the only lane this job runs on, so the archive also carries web/dist-b, the second
rollout bundle no row of this job reads.

## What the alone job's PostgreSQL reading records

The shard job's step of this name, on this job's one stack: what the image's PostgreSQL
is, who runs it and what the provider costs, read from the same binaries the stack uses.
That step says why it is read here or nowhere, and the script it calls says what each
line it prints is for. The argument is this job's stack count: one copy rather than
four, since this job runs one worker.

## Why the alone job runs one stack and no file store, and what its browser step's budget is

`bun run e2e` builds the SPA itself, which would build it a second time in this run.
Calling Playwright directly runs the dashboard the binaries job built and the job's
dashboard download downloaded, and leaves this job no Vite build at all. Safe because that
build and one here would be the same binary: `packages/dispatch` no longer has an E2E-only
build flag (removed with its sole, unconsumed consumer, the `window.__dispatchDocument`
inspection probe). The run starts its own servers: `globalSetup` starts this run's one
stack on a port the OS chooses and writes it into the environment the worker inherits,
and `globalTeardown` stops it (e2e/global-setup.ts, e2e/harness-pool.ts). The contracts
step, "Build contracts for dispatch e2e", is what Playwright's own Node runner needs, for
the reason that step gives.

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
`env:`, for the reason the job's head gives. One worker is also what runs the three
projects one after another rather than beside each other, so each of them has the
machine to itself while its own rows are timed.

No file store on this machine, and no DISPATCH_E2E_FILE_STORE_* names: every spec this
job runs is a clock spec, and no clock spec uploads or downloads anything.
chromium-alone runs both wall-clock specs (`packages/dispatch/e2e/configurations.ts#clockSpecNames`:
heavy-bodies and nested-directive, the list `clockSpecPattern` is built from, which is
that project's `testMatch`), firefox-alone and webkit-alone run heavy-bodies alone.
And `packages/dispatch/e2e/harness-pool.ts#stackBucket` makes no bucket at one stack: it
answers `undefined` on `count === 1`. A store serving nothing would be charged
to the very reading these rows exist for, which is why the `weed` the image carries is
started on the shard and real jobs' machines and not on this one. A clock spec that later
uploads fails here by name rather than quietly.

The memory and CPU sampler both this job and the shard job run is
.github/scripts/dispatch-browser-samples.sh: MemAvailable and where the CPU goes, at the
interval its head names, for the length of this step, started by
.github/scripts/dispatch-browser-alone-suite.sh and read back at the end of it.
Its head says what each reading holds and what it does not; on this job `everything
else` is also the reading that says the machine was quiet, which is the claim these
rows rest on. Both sample files are written into the record directory that the step
"What the run left behind" collects and the upload after it uploads, under $RUNNER_TEMP
beside the stack's own run directory: a file
this step wrote into the checkout would leave it dirty afterwards. The blob report
goes beside the list reporter's output, so a green run reports what it ran too.

The step's budget, `timeout-minutes: 4`, is the one the job's head gives this step by the
rule in this file's head: the step's measured maximum over the nine runs at this shape is
170 s, in run 43192333526 (job 131096351085) and run 43251567200 (job 131300609244), and
170 x 1.15 = 196 s, which rounds up to 4 min.

## Why the alone job's blob upload is always

This leg's half of the run's report. Always, not on failure alone: the merge job reads
every leg's blob, and a leg whose blob is missing fails it by name. One directory, as
the shard job's upload of this name is and for the reason it gives.

## What the alone job collects after a run

What the run left behind: the stack's own server logs, in the job's own log and collected
into the one directory the `${{ inputs.job }}-record-alone-${{ github.run_attempt }}` artifact takes, beside the sampler's two files, which
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
`trace: 'retain-on-first-failure'` keeps. That trace has no DOM view, as
[the shard job's section](#why-the-shard-job-uploads-the-failed-rows-traces-always) says.
A green run's upload is small.

## What the two-process job runs

The browser suite's two-process mode (packages/dispatch/e2e/harness-processes.ts): two
envoy-dispatch processes against one Postgres, behind e2e/round-robin-proxy.ts, B serving a
second build of the dashboard, which is what a rolling release puts in front of a browser. It
runs only the *.multiprocess.e2e.ts specs, in Chromium, in a job of its own so the default
suite's job does not wait on two more servers and the second bundle they serve, which the
binaries job builds for this job and no other.

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

The budgets of the install, dashboard and sign-in steps, and the job's `timeout-minutes` from
them, are measured: GitHub's
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
without one, rounded up: 4 + 4 + 2 min plus 135 s = 13 min. The job's Postgres then moved
from a `services:` container to the harness's own
(.github/ci-notes/dispatch-browser.md#what-the-two-process-job-runs says why), which put the
image's pull before the browser steps: it has no budget and counts at the 120 s the visual
job counts its pull of the same digest at (the time a browser step spent before its stack's
first output on the run that found the need; the pull took 13 s in run 44480885813), and the
job's budget adds it. The dashboard step now also starts the stack's container and builds
the stall library, and its four runs since on the default branch took 118 s, 114 s, 96 s and
106 s (runs 44480885813, 44501665975, 44528268121 and 44549420195), inside the 202 s its
4 min derives from. Those ran `bun run e2e:multiprocess`, whose step still built both
bundles; this job's step runs `e2e:multiprocess:run`, which builds neither. With budgets on
the steps, the
`if: failure()` upload of `${{ inputs.job }}-multiprocess-test-results-${{ github.run_attempt }}` still runs, which a cancelled job skips.

That 13 minutes stood through the change that took the server build out of this job and put
three steps in: two actions/download-artifact and the chmod beside them, which take the shard
job's maxima for the same steps in its four-leg table, 5 s a download and 1 s the chmod (the
shard job's head names that table's 24 legs and five runs). The 135 s above still carries the
build that left, so it is an over-estimate by that much, and 4 + 4 + 2 min plus 135 + 11 s is
12 min 26 s, which rounds up to the same 13 minutes. The dashboard step's own budget can only
have got roomier: the two Vite builds it ran are the binaries job's now.

The move into this workflow changed none of that: the job runs the same steps on the same
machine, and full-lane run 42316487530 falls inside every figure above - 9 s install,
191 s dashboard, 12 s sign-in, 96 s in the steps without a budget, 313 s whole. The first
full-lane run after this job's Go cache restore moved from go-envoy to go-envoy-test,
42614920253 (job 129152410448), is inside them too - 9 s install, 106 s dashboard, 8 s
sign-in, 96 s in the steps without a budget, 219 s whole - and that restore reads
go-envoy-test-Linux-go1.27.3, 677,668,128 bytes, 22 s of those 96.

WHY THIS JOB HAS NO `postgres:` SERVICE, AND WHAT IT PAYS INSTEAD. It had one until
dispatch://INFRA-1109 read what it cost: in run 43045412941, the first green full lane at this
shape, `Initialize containers` took 21 s of this job's 128 s and `Stop containers` 0 s, and
the same two steps took 22 s and 3 s of the embedder job's 68 s - 46 s of a lane measured at
2,061 s and now held to the full lane's committed ceiling
(`.github/scripts/dispatch-browser-run-reads.sh#bound`). A service container is a pull, a network, a
start and then a wait on `--health-interval 10s`, which no first probe can finish sooner than
that interval.

So the stacks start their own, through e2e/harness-postgres.ts's image provider, which is
the route the visual job already takes on the runner and not a new one: one
`docker run` of the same pgvector digest per stack, published on a loopback port the OS
chose, waited on by the harness's own `select 1` rather than by a ten-second health
interval, and removed with the stack. The shard, real and alone jobs' route is no route here -
their PostgreSQL 18 with pgvector is in the image they run in, and this job runs on the
runner, whose own PostgreSQL 16 carries no `vector` extension at all
(actions/runner-images installs postgresql-16 and libpq-dev and nothing else), so
DISPATCH_E2E_PG_BIN here would fail at `create extension vector` before the first row.
What it costs was measured on one sample, the visual job of that same run 43045412941: its
browser step began at 15:13:06.7 and the stack's first listener logged at 15:13:15.0 with
the server listening at 15:13:17.1, so 8.3 to 10.4 s covered the pull, the container and
the whole stack's boot. The jobs API gives a step's seconds whole and does not split the
provider out of them, so no later run replaces that sample. What the later runs do give is
this job's own steps, below.

The budget, by the rule in this file's head and from those figures: the step budgets are
unchanged at 4 + 4 + 2 min - the two browser steps absorb those 8.3 to 10.4 s inside
maxima (202 s and 13 s) measured when the dashboard step still ran two Vite builds this
job no longer runs - and the measured maximum of every step without one is the 135 s
above with the 11 s of the two downloads and the chmod added, less the
`Initialize containers` it carried and the setup-go and Go cache restore that left (21, 11
and 20 s, the last two read below): 135 + 11 - 21 - 31 = 94 s. 10 min plus 94 s is
11 min 34 s, which rounds up to 12 minutes.

The nine runs the shard job's head lists, the first of them still with this job's
`postgres:` service, read every step of this job inside those budgets, by the shard job's
command: the install at most 9 s (job 130958861058, run 43150379433), the dashboard step
at most 67 s (job 131096351080, run 43192333526), the sign-in step at most 13 s (job
131282160536, run 43245766326) and the whole job at most 141 s (job 131631089345, run
43347417247). By the rule those maxima alone would give 2 + 2 + 2 min. The budgets stay
on the larger maxima of the 335 jobs above, a wider reading than these nine runs, and no
budget here was moved by them.

Since those runs the job lost two steps without a budget, actions/setup-go and the Go
cache restore, which read at most 11 s (job 131262996021, run 43240954914) and 20 s (job
131300609277, run 43251567200) there: the dev OIDC issuer they served is the binaries
job's build now. Those are the 31 s the sum above takes off. Pull request #3513's eighteen
full-lane runs of 2026-11-15 read this job's steps without a budget at 33 s, each at its
maximum and summed (the checkout 9 s, job 132926233684, the largest), with the dashboard
step at most 99 s and the sign-in step 13 s (both job 132904452893), so the 12 minutes
stand as a backstop above the sum.

Main's change to the harness's own Postgres for this job and the embedder job (pull request
#4486) put one step before the browser steps here: the image's pull, which has no budget and
counts at 120 s, the figure the budget paragraph of this job's head gives it. Its connections
read is no step of this job: the binaries job reads that number once for the run, as for
every job of this file (e2e/harness-postgres.ts). 4 + 4 + 2 min plus 94 s and 120 s =
13 min 34 s -> 14 min, the multiprocess job's `timeout-minutes`.

## What the two-process job downloads in place of its builds

What the binaries job built. The server is named on DISPATCH_E2E_SERVER_BIN by both
browser steps, so e2e/run-server.sh executes it instead of building one in each
server's boot wait (run-server.sh's build for a caller that names no binary), which is what
the step `Build the Dispatch server` was here to make cheap; that step is gone with the
build it anticipated. The dev OIDC issuer is named on DISPATCH_E2E_SIGNIN_ISSUER_BIN by the
sign-in step, so e2e/run-signin-issuer.sh executes it instead of building one. The path is
the directory run-broker.sh serves its binaries from, as the shard job's step of this name
says - this job starts no broker (two-process mode runs none, e2e/harness-real-broker.ts),
and the server and the issuer ride along in it. Its chmod is the shard job's step of that
name, for the reason that step gives.

## The two dashboards the two-process job serves

Both dashboards this job serves, built once by the binaries job: web/dist is A's and
web/dist-b is B's, the second rollout bundle with the build id
`packages/dispatch/e2e/two-builds.multiprocess.e2e.ts#test.beforeAll` holds apart from A's. This job runs on the full lane
alone, which is the lane that bundle is built on.

## Why the two-process job sets up no Go

No actions/setup-go and no Go cache restore: no step of this job runs a go command. The
sign-in step's dev OIDC issuer was the last one, built by
packages/dispatch/e2e/run-signin-issuer.sh inside that step until the script took a
prebuilt binary on DISPATCH_E2E_SIGNIN_ISSUER_BIN. The binaries job builds the issuer
now, in the one `go build` that builds the server, and this job downloads it with the
server (dispatch://INFRA-1109). The restore it had, go-envoy-test, was there because this job
built the Dispatch server and the dev OIDC issuer itself, on the runner's default cgo, and that
family's entry was the one that held them; it went with the builds.

## Why the two-process job builds contracts

The shard job's step of this name, for the reason it gives: Playwright's Node runner
reads `@legion/contracts` out of a build this checkout has to make before the config
loads, and the entries Dispatch imports are all it needs. It is the one build left in this
job: the servers, the dev OIDC issuer and both dashboards are the binaries job's.

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
workflow to join: the shard and alone jobs restored one until they moved into the image
that carries the browsers, which the real job runs in too, and that move took their restores
and ci-caches.yaml's browsers job with them (dispatch://INFRA-1443). This job stays on the runner, because
nothing can make it launch another engine, and so keeps this one download.

## Why the two-process job pulls its stack's Postgres image

The image the harness starts each Playwright run's Postgres from, pulled here so the
browser steps of its job after it do not pay for it inside their own budgets, as the visual job pulls
it. The digest is the browser steps', written three times, and the three must stay the
same: a pull of another digest leaves the browser step to pull its own. Each new step's
name carries the lane, as the embedder job's server build step's did until
dispatch://INFRA-1109 took that step out with the build: .github/scripts/check-ci-filters.sh
names a step of this workflow by its own name, and the multiprocess job had a step that built
the same binary.

## What the two-process dashboard step runs against

Runs the multiprocess specs through the round-robin proxy on the two dashboards the
binaries job built. `e2e:multiprocess:run` and not `e2e:multiprocess`: that script
builds the contracts, A's dashboard and B's before it runs, which is what a person's
own command must still do, while this job makes the first in its contracts step and takes
the other two from the run (packages/dispatch/package.json). DISPATCH_E2E_SERVER_BIN and
the stack's Postgres image are set on the command line rather than in this step's `env:`,
for the reason the shard job's head gives.
DISPATCH_E2E_PG_STALL_DIR is the directory the harness builds e2e/fsync-stall.c into and
preloads into the stack's Postgres, which e2e/read-marker-disk-stall.multiprocess.e2e.ts
needs: on GitHub Actions (GITHUB_ACTIONS=true) the row fails without it, and elsewhere,
an agent's shell included, it is skipped. The runner's own cc builds it.

The image is pgvector, the copy in ghcr.io/cybermonklabs/legion/ci (.github/ci-images.yaml),
started per stack by e2e/harness-postgres.ts in place of the `postgres:` service
this job used to declare, for the reason and at the cost the job's head gives. The
digest is a literal, as the visual job writes it: .github/scripts/check-ci-images.sh
resolves a reference to a row of that file and cannot resolve one written as a workflow
expression. Actions reads an expression inside a `run:` body even in a comment, so none is
written in one. The sign-in step writes the same digest for the same reason.

## What the sign-in row across two processes reads

Both processes sign people in through the dev OIDC issuer (DISPATCH_E2E_SIGNIN=oidc,
e2e/harness-signin.ts), and its one spec starts a sign-in on A and completes it on B. B
serves A's build, which this spec does not tell apart. The issuer and the servers are
the binaries the run built: e2e/run-signin-issuer.sh executes the issuer that
DISPATCH_E2E_SIGNIN_ISSUER_BIN names instead of building one, as run-server.sh does
the server on DISPATCH_E2E_SERVER_BIN, and both are set on the command line rather
than in an `env:`, for the reason the shard job's head gives. This step runs
Playwright a second time, so it starts a stack of its own and a Postgres container
with it; the image the "Pull the multiprocess stack's Postgres image" step pulled is on the
machine by then, so what it pays is the container and the server's own boot.

The budget stays at the two-minute floor. The step's maximum over the nine runs the
shard job's head lists is 13 s (job 131282160536, run 43245766326), and those runs
built the issuer inside the step (12 s in job 131631089345), so the step can only be
shorter now.

## Why the two-process job's upload runs on failure alone

`if: failure()`, which this job can afford where the shard job cannot: its project sets
`retries: 0` (e2e/playwright.config.ts), so a row that fails fails this job and this
upload runs. The shard job's copy is `if: always()` because a retried row there can pass
and leave the job green.

## What the embedder's browser step runs against

The command bar's two counting-embedder rows need a fake that counts the server's requests.
The package script `e2e:embedder:run` selects those rows on Chromium under
`DISPATCH_E2E_EMBEDDER=fake`. It starts no build. The separate Playwright invocation runs in
`real` on pull requests and `alone` on full, reusing the binaries, dashboard, contracts and
Chromium already present. It starts its own single-worker stack using PostgreSQL 18 from
`/usr/lib/postgresql/18/bin` in the job container, not a Docker image or a service.

It runs after the main browser step even when that step fails, unless the job is cancelled.
The first invocation tears down its stacks when it exits. The embedder starts a fresh harness
with its own temporary directory and OS-assigned ports, and it does not receive the first
step's file-store environment. It uses the Playwright config's normal reporters, not the
first step's blob reporter. `--output="$RUNNER_TEMP/embedder-test-results"` keeps Playwright's
output-directory cleanup from removing the first step's `test-results`.

The 4-minute step timeout is unchanged. A failure after Playwright's retries fails the
containing job and the merge verdict, even with a green blob from the first step. The upload
after it runs only when this step itself failed, not on a job failed by the first step alone:
Playwright writes `.last-run.json` into the output directory on every run, a passing one too.
It uploads a separate attempt-qualified `<caller>-embedder-test-results-<attempt>`
artifact. The two embedder rows did not contribute to the merged report before the move
and still do not, so the merge row counts and flake rate have the same scope.

## The visual review, its docker run and its budgets

The containerised visual review: every page of packages/dispatch/e2e/review-routes.ts's shot
set compared against a committed baseline, in light and in dark, at a desktop width and two
of them again at a phone width. Twenty pages in two schemes and two pages in two, so
forty-four comparisons, which is the number of PNGs under
packages/dispatch/e2e/__screenshots__ (40 under visual/ and 4 under visual-phone/).

The full lane only. A snapshot difference holds the merge queue, which is the point: a palette
or a layout regression is refused at the merge rather than found the next morning, and the
nightly tells one person alone, the one GitHub notifies of a failed scheduled run (that
workflow's own head says who). The cost is that a flaky comparison would hold the queue
too, which is why the tolerance
is measured from zero rather than defaulted (packages/dispatch/e2e/visual.e2e.ts).

Why a `docker run` inside a step and not a job-level `container:`. A `container:` on any job
of this file drops every `go build` the suite starts out of check-ci-filters.sh's reduction
(`.github/scripts/ci_filter_coverage.py#shell_reason`, "the job runs in a container:"), which then counts
each as reading every Go file of the repository. The shard, real and alone jobs do take one,
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
a capture that knows `shard (N)`, `real`, `stub` and the bare `alone` alone - `alone (P)` was a job name of
the matrix dispatch://INFRA-1109 removed, and no run produces one now - and it fails the run
when the merged report holds rows no leg printed; the multiprocess job writes none for the
same reason. This job joins `merge`'s `needs:` and the verdict step's pair list instead, which
is what makes a red visual job fail the workflow.

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

The rule is in this file's head. The browser step's maximum was 106 s over those columns,
so 106 x 1.15 = 122 s -> 3 min, and the job budget was that step's 180 s plus the measured
maximum of every step without one, which is 333 s of the six columns together: 513 s ->
9 min.

The sixth column is the first full-lane run after this job's Go cache restore moved from
go-envoy to go-envoy-test, run 42614920253 (job 129152410516). That restore reads
go-envoy-test-Linux-go1.27.3, 677,668,128 bytes, where the five columns before it read
go-envoy: 17 s rather than the 1 to 2 s above, which is why that row's maximum moved and the
job budget still held.

The browser step then outran its budget on main, which is this rule's signal to derive it
again. Over the 17 merge-group runs of this job from 2026-11-14T08:40Z to 2026-11-15T06:25Z
that passed, it read 111-179 s, and two runs that day timed out on it at 183 s and 182 s
(run 43687635217, a merge group, and run 43691184536, a pull request). So the maximum over
every run is 183 s, and 183 x 1.15 = 211 s -> 4 min, the browser step's `timeout-minutes: 4`.
Those runs are of the default branch before pull request #3513, where e2e/run-server.sh built
the Dispatch server inside this step; since that pull request it builds nothing
(DISPATCH_E2E_SERVER_BIN and DISPATCH_E2E_BROKER_BIN_DIR, which
.github/scripts/dispatch-browser-visual-suite.sh exports), and its runs on that pull
request's branch read 37 to 78 s, at most 78 s (job 132830691048, run 43720076901), over every
run of this job there, 57 of them from 2026-11-11T12:42Z to 2026-11-16T04:08Z. The rule takes
the maximum over every run all the same, so 4 minutes stands, with room.

The job's sum is taken over every step of the job as it is now, each at its maximum over
the six columns above and those 57 runs, in seconds: `Set up job` 2 (job 132730913819), the
checkout 9, the binaries download 7 (job 130609653873), the chmod 0, the dashboard download
3, setup-bun 218 (job 128713801668), bun install 19 (job 132113751326), the contracts build
1, the image pull 55 (job 132915472142, run 43747732723), the upload 3 (job 132904452885),
`Post Run ./.github/actions/setup-bun` 1, `Post Run actions/checkout` 1 (job 132830691048)
and `Complete job` 1 (job 133073564329). Those sum to 320 s, against the 333 s of the six
columns: setup-go, the go cache restore and the dashboard build left this job
(dispatch://INFRA-1109), which takes off their 9, 17 and 37 s; the seven steps the columns do
not list add 15 s; and the 57 runs' higher maxima add 35 s (checkout, bun install, the
contracts build, the image pull and the upload). 333 - 63 + 15 + 35 = 320, and
240 + 320 = 560 s, 9 min 20 s, rounded up to 10 minutes.

The first run of the columns was the only one this job had when these budgets were
first written, and one run proved too thin a sample: 90 s gave a 2-minute bound by the rule's
own floor, and the next run came in at 106 s, 14 s inside it. A third run then read 78 s, so
the browser step's spread was 78-106 s over those three runs and the 15 per cent the rule
adds carries the rest.

The job budget was 5 min on the first three columns, where setup-bun read 1-2 s because the
runner image carried the pinned Bun. The last two columns are runs during a GitHub release
CDN outage on 2026-11-08, where that same step downloaded Bun in 218 s and 131 s, and the
5-minute job budget cancelled both jobs - one in `setup-bun`, one in the dashboard build -
before any step budget could fire. That inverts this file's rule, which says the job budget
is the backstop that never fires first, so it is re-derived above from the maximum those
runs measured. A download this job does not control may not decide a required check.

The step "Pull the stack's Postgres image" was added later, for the reason
.github/ci-notes/dispatch-browser.md#the-stacks-postgres-image-pulled-here-for-the-same-reason gives,
and its pull is counted at the 120 s the browser step spent before the stack's first output on
the run that found it, run 44215262087 (job 134503540243): 240 + 320 + 120 = 680 s, 11 min 20 s,
rounded up to the 12 minutes it had. The reverse-order browser step this job ran after it, with
its own 4 minutes, is gone: the section on the guard that took its place says why.

#4383 counted the steps without a budget at 314 s, the five columns' figure before the sixth
was added (240 + 314 + 120 = 674 s), where #2856 had already re-measured them at 333 s over the
six columns above, the sixth being run 42614920253 (job 129152410516). On the default branch's
step list of 2026-11-20 that sum is 240 + 333 + 120 = 693 s, 11 min 33 s, and the budget stays
12 minutes. With the three steps dispatch://INFRA-1109 took out of this job the sum is the
320 s above, 240 + 320 + 120 = 680 s, and the budget is the same 12 minutes.

## What the visual job downloads in place of its builds

What the binaries job built, in place of this job's own actions/setup-go and its
go-envoy-test restore, which are gone with the two builds that needed them: the browser
step's script names this server on DISPATCH_E2E_SERVER_BIN and this directory on
DISPATCH_E2E_BROKER_BIN_DIR, so e2e/run-server.sh and e2e/run-broker.sh execute what the
run built rather than building it inside the browser step, and no step of this job
starts a go command any more. The directory is the one run-broker.sh serves its three
binaries from, as the shard job's step of this name says. Its chmod is the shard job's
step of that name, for the reason that step gives.

## The dashboard the visual job serves

The dashboard the binaries job built, in place of this job's own `Build dispatch
dashboard` step: the artifact holds the trees themselves, so this writes web/dist where
`bun run build:web` writes it, which is what this job's server serves. On the full lane
it writes web/dist-b beside it, the second rollout bundle only the multiprocess job
reads; one artifact for both costs this job a few megabytes and keeps one name.

## Why the visual job builds contracts

The e2e helpers the visual specs import read some of contracts' values from its built
dist/, through Playwright's own Node resolution (the shard job's step of the same name,
which says why no hook of the suite can make this build, and why the entries Dispatch
imports are the ones it builds). The run inside the container reads it from this same
checkout, mounted there at its own path. It is the one build left in this job, which
downloads everything else it runs.

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

## Why a seed near a UTC midnight waits for it, and what that costs the budget

A seed in the last two minutes of a UTC day waits for midnight and seeds again
(packages/dispatch/e2e/visual-day.ts): at most 2 min, and one more seed, which with the
stack's start took under 7 s in job 135974266127, whose reverse-order step, since removed,
failed the two rows the wait is for. The green runs of the step "Browser-test dispatch
dashboard against its baselines" took 66 s and 81 s (jobs 135832072234 and 135974266127), so
with the wait and the seed added the step stays inside 4 minutes.

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

What the blob-producing legs together ran. It joins the reports from `real` and `stub` on
pull requests or both shards and `alone` on full (two shards only on references). It
publishes the rows, the flake rate, the legs that did not succeed, the row durations and the
lane's machine time. Each caller gets one verdict.

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

Every job of this workflow, the binaries job among them: every other job of this file
`needs: binaries`, so a failed build leaves its dependents skipped and no blob report.
Without the binaries job in this list the run would be reported as legs that uploaded
nothing rather than as the build that never handed them anything. The verdict step, "Fail
when a leg failed or the merged report holds a failed row", names it as it names the rest.
In a call that takes another caller's build the binaries job is skipped, and that step reads
the skip as it reads any skipped job, as no failure: the shard legs ran on a build that
succeeded in the other call
([the references lane's section](#when-the-references-lane-takes-the-pull-request-lanes-build)).

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
carried before was measured on another branch and is replaced by that sample. The worst case
the lane's bound takes off now is `merge_maximum`, 54 s whole, job 138031009105 (run
45261148766 attempt 2), the most of the 38 merge jobs of pull request #3513 that the
`machine-time` branch of .github/scripts/dispatch-browser-run-reads.sh records with the
figures it replaced. Over those 38 and the 10 merge jobs of main's 65 merge-queue runs listed
since main's #5175 merged (2026-11-23T21:37:21Z), 48 jobs read from the jobs API on 2026-11-24
up to 01:06Z, the steps without a budget sum to 48 s at their maxima: checkout 14 s,
`bun install` 8 s, the merge of the blob reports 4 s, setup-bun, the upload of the merged report
and "Reading three" 3 s each, Set up job and the blob download 2 s each, and every other step
1 s, but for the refused-download step, which every one of the 48 skipped.

Each step's own maximum over the 2026-11-08 sample, summed, is 40 s: Set up job 2 s, checkout 9 s,
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
has a measured maximum of 11 s, the most it took over the 48 merge jobs above, job
138031009105's among them (run 45261148766 attempt 2), which replaces the 4 s of the
2026-11-08 sample, and 11 x 1.15 = 12.65 s, so 210 + 12.65 = 222.65 s, at the next whole
minute above, is the 4 min that step carries. The rule's two-minute floor does not bind.
The step now also runs packages/dispatch/e2e/tools/leg-rows.ts over the shard legs' row lists,
measured by main at 0.02 to 0.08 s on the development box over three runs, over four legs'
lists of the full lane's 1,617 rows (1.1 MB: each leg lists the whole selection and the rows it
ran, so four legs write five selections' worth and this job's two write three, about 0.66 MB).
Job 138031009105 ran it, on the full lane at e15ee5e4, so that work is inside the 11 s and is
not added to it.

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
one step's 240 s, plus the measured maximum of every step without one: the 48 s of the 48 merge
jobs above, and the 1 s of "Report a refused blob download as a refused read", which runs one
`echo` and which none of them ran, is what this job's other one-command steps measure.
240 + 49 = 289 s, which is 4 min 49 s, so 5 min,
the figure `dispatch-browser.yaml` gives beside the job's `timeout-minutes`. The older sample
gives the same 5 min: its 40 s less reading one's 4 s is 36 s, and 1 s each for the three steps
it does not hold - that one, "Reading four, the row durations", which runs one jq over the
merged report, and "Reading five, the lane's machine time", which runs one jq over the jobs
file reading one already wrote and makes no request at all - makes 39 s, and 240 + 39 = 279 s.
The lane's budget takes this job's whole measured maximum off its bound, the 54 s of 2026-11-23
(job 138031009105) with these steps in it
(`.github/scripts/dispatch-browser-run-reads.sh#merge_maximum`). The job's budget is above the step's, so the step's own budget fires first and
names the step, which is the reason this file's head gives for putting budgets on steps.

What that sample does not hold: no browser step failed in any of those runs, so no blob
report in them carries a failed row's trace, and the download, the merge and reading
three's per-leg merge all grow with those. A job cancelled at this budget posts no ci-ok,
so the run is red and read again from the jobs API; it cannot pass a verdict it never
reached.

The job reads each attempt 30 jobs at a time. Run 45913688080 returned HTTP 502 at 100 jobs
per page and returned all 95 jobs at 30 per page. The read took 13.8 s in a pod on attempt 3,
inside the 30 s bound for all its pages. The script's head has the measurement.
.github/scripts/dispatch-browser-run-reads.test.sh checks that the full, pull-request and
references lanes use jobs from both pages, that both budgets include them, and that a read
whose later page never succeeds writes no jobs file.

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
The pattern starts with the caller's id, the workflow's `job` input, as the shard, real, stub and alone
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
nothing about whether those legs uploaded. Run 44754426131, attempt 1, is why: the job's limit
cancelled its `bun install`, both steps were skipped, and reading one named all four of that
run's shards as having sent no blob, though each blob had been uploaded before the job started.

## What the merged report's three outputs are for

The JSON report is the one packages/dispatch/e2e/tools/check-slow-rows.ts reads, and
this merged report is now its source: one run's rows, from every leg, in one file. The
HTML report is for a person; the list reporter prints the run's result into this step's
own log. It reads the selected directory the "Take the newest attempt of each leg's blob report" step wrote, which holds one attempt
per leg. The three output paths are exported here rather than in an `env:` block, for
the reason the shard job's head gives.
The step's body, those exports included, is .github/scripts/dispatch-browser-merge-reports.sh.

The step hands `merge-reports` a merge configuration whose one setting is testDir, the merge
job's own packages/dispatch/e2e; the script writes it into $RUNNER_TEMP and its head says why
that directory and why not the suite's own e2e/playwright.config.ts. A blob report records the
test directory its leg ran from and holds its paths relative to it, and with no configuration
Playwright 1.64.1 refuses blobs of two test directories. The pull-request lane's `real` leg
checks out in the CI container, under /__w/<repo>/<repo>, and `stub` on the bare runner, under
/home/runner/work/<repo>/<repo>, so run 45913634925's merge job failed on that refusal with
both legs green. Given a configuration, `merge-reports` skips the check and resolves every leg's
paths against its testDir: Playwright 1.64.1's merge takes the configuration's rootDir, which is
its testDir, as the one root of every blob. packages/dispatch/e2e/__tests__/merge-reports.test.ts
records two legs under the two roots and runs the script over them.

What that leaves as it was, read from Playwright's source at 1.64.1 and from the readers:

- The rows. The merge selects and drops none, each blob's rows stay under their own project,
  and the JSON report writes each spec's `file` relative to testDir, as each leg wrote it. So
  the readers that name a row by its project, file, line and title read the rows a merge of one
  root gave: reading two's flaky rows (.github/scripts/dispatch-browser-reading-flake-rate.sh),
  reading four's classes, read from the project name
  (.github/scripts/dispatch-browser-reading-row-durations.sh),
  `packages/dispatch/e2e/tools/check-slow-rows.ts#reportRows` and
  `packages/dispatch/e2e/tools/shard-weights.ts#rowTimes`. Reading one and the verdict step read
  the stats alone.
- Attachments and traces. A blob names each one `resources/<sha1>.<ext>`, which the merge
  extracts into the blob directory and resolves there, never against testDir; the HTML report
  copies each into its `data/`, and the JSON report names each under
  $RUNNER_TEMP/selected-blobs/blob-report/resources/.
- Which leg ran a row. The merged report never said; reading three reads it from each leg's own
  blob, and the HTML report's list of machines comes from each blob's own configuration.

What moves: the JSON report's `config.rootDir` and each project's `testDir` and `outputDir` name
the merge job's checkout rather than a leg's, and `config.configFile` names the written
configuration, where a merge with no configuration left it out. No reader reads any of them. The
full and references lanes, whose legs all run in the container and so share one root, merge as
before but for those values. The HTML report's code snippets, which the merge reads from each
step's file as resolved against testDir, now find those files; a container path such as
/__w/<repo>/<repo> does not exist on the bare runner the merge job runs on.

Reading three's per-leg merge (.github/scripts/dispatch-browser-reading-leg-exits.sh) takes no
configuration and is left so: it merges one leg's blob at a time, whose paths share one test
directory, and it reads `stats.unexpected` alone.

## Why each reading goes to the step summary and to the log

Reading one. Each reading is written to the step summary, for a person, and printed as a
`::notice::` line, for a script reading the log: the two readers want the same figures in
different places.

## Why a leg that exits red with no failed row is named apart

Reading three. A leg that ended red with no failed row in the report died outside the
suite - a budget, a runner fault, a step before the browser step - and that is a different
fault from a failing row, so it is named apart. The real and alone legs run the
counting-embedder step after the browser step their blob is from, and its rows are in no
blob, so on those two legs the line names that step as the other place the leg can have
failed.

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

## What reading five sums, and why it fails the job

Reading five. What this run has cost in machine time, which is the number
dispatch://INFRA-1109 exists to hold down and the one that grew from about 25
runner-minutes on 2026-11-07 to 45 to 49 on 2026-11-10 with nothing in the tree to
refuse it. The jobs reading one wrote are read once more here - the same
$RUNNER_TEMP/jobs.json, carrying each job's `started_at` and `completed_at` as the jobs
API already answers them - so this reading costs no request and no second route. Those are
the jobs of this merge job's caller alone, the ones named `<job> / …` (reading one's read
keeps no other), so on a pull request labelled `full-ci`, which runs dispatch-browser beside
dispatch-browser-pr in one run, dispatch-browser's merge job holds the full lane's jobs to the
full lane's budget and dispatch-browser-pr's holds the pull-request lane's to that lane's. The
references lane's merge job skips this step by its `if:`
([the lane's section](#the-third-lane-references) says why).

It is a reading that fails, as reading one does and readings two to four do not: above the
lane's budget the step exits 1 and the job is red. Each lane's budget is its committed
ceiling less this job's own worst case, both written in
`.github/scripts/dispatch-browser-run-reads.sh#bound` and
`.github/scripts/dispatch-browser-run-reads.sh#merge_maximum`, which a job
reading inside its own run cannot see of itself: its `completed_at` is null while it runs.
The script's `machine-time` branch carries that subtraction, the runs the measured 54 s of it
came from, and why a jobs file it cannot read refuses rather than passes.

The lane is this workflow's own input, which every other job of the file already reads, for
the work it does or for whether it runs; nothing new is passed in for it.

## What the verdict step fails on

The verdict, which readings two to four do not give: they report, and this fails. Two of
the job's failures are here - an upstream job that neither succeeded nor was skipped, the
binaries job among them, and a failed row in the merged report. The others fail where they
are found: the download of the legs' blob reports on a download it could not complete,
which the step after it names as a failed read and passes; the attempt selection
(.github/scripts/dispatch-browser-latest-blobs.sh) on an artifact whose name is no leg's
blob report or a call without its two directories; the merge step on no blob to merge or no
JSON report written; reading one on jobs it could not read
(`.github/scripts/dispatch-browser-run-reads.sh#jobs)`, its jobs mode), no leg to hold the report
against, a missing blob, a row of the selection that no shard leg ran or that two ran
(packages/dispatch/e2e/tools/leg-rows.ts over the legs' row lists), or a merged report that
dropped rows the legs ran; reading five, which runs on every lane but references, on a jobs
file that is missing, is not a list of jobs or holds no job that concluded with a duration, on
a lane that is neither full nor pull-request, and on a lane over its machine-time budget,
because a budget nothing fails on refuses nothing; and the ci-ok report after this step, on a
merge-group run, on an input or a ci.yaml setting it cannot read, or a ci-ok check run it could
not post (.github/scripts/ci-ok-report.sh and post-check-run.sh).

The step's body is .github/scripts/dispatch-browser-verdict.sh, given the seven upstream
job results in order: binaries, shard, real, stub, alone, multiprocess and visual.

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
