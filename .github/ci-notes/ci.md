# Notes for the CI router

The derivations and the history of `.github/workflows/ci.yaml`, the router, and of the composite
actions under `.github/actions/` that hold its jobs' steps. Each `# why:` line of those files
names the section that holds the derivation or the record its job, its leg, its step or its key
rests on, and `.github/scripts/check-ci-layout.sh` (rule 33) refuses a section no such line names
and a line that names no section. A comment block of at most two lines stays where it is, beside
the step it is about; so does a value's own arithmetic, and a comment inside a `run:` body, which
is shell and stays with its command. The filters that select each job are
`.github/ci-areas.yaml`, one area per job, which the `changes` job's paths-filter step reads.
In the text moved here, "this file" and "this workflow" refer to `.github/workflows/ci.yaml`.

## The router: every check a pull request or the merge queue runs

The router: every check a pull request or the merge queue runs, in one workflow.
Its first job, `changes`, reads the paths the change touched and selects, through one
dorny/paths-filter filter per job (an area of .github/ci-areas.yaml, a package area each), the
jobs that read those paths; each job runs only when its output is true, and a changed file no
area names selects every job (lint runs on every change, and scripts, scripts-held, go-daemon,
plugin-pack, text-checks and notices also run when `changes` itself failed). A selected job runs
every leg of its matrix.
ci-ok condenses every job's result into one check: it fails when a job `changes` selected did
not succeed.
Main's ruleset requires ci-ok and pr-title. ci-ok is the `ci-ok` job
at the end of this file, which needs every job of it but itself and those CI_OK_EXEMPT names,
and fails when .github/scripts/ci-ok.sh's verdict over their results is failure, so the
verdict rides on that job's own check run — a job's check run is in its run's own check suite,
where a check run posted with POST /repos/.../check-runs joins the Actions App's first suite
at that commit, which is another workflow's as often as not (.github/scripts/post-check-run.sh's
header holds the measurement). Every job but `changes` and those CI_OK_EXEMPT names still
ends with the ci-ok-report step (.github/actions/ci-ok-report), which posts a failing ci-ok
at once when that job's own result decides a merge-group run of the queue's front group, so
the queue drops a doomed group at the first failure rather than at the end of the run; a group
behind the front posts nothing early, since the queue cannot drop it before the groups in front
of it end (.github/scripts/ci-ok-report.sh's header says what that costs), and the ci-ok job
holds a failing verdict of such a group until the group is the front
(.github/scripts/ci-ok-hold.sh), so the queue builds nothing on a failed entry; on every other
run the ci-ok job's last step then
sets every ci-ok check run on the head that is not its own to its own conclusion, so one
verdict stands at the end of every attempt. pr-title, the other check that ruleset requires, is
the `pr-title` job at the end of this file, in the same shape and for the same reason: it runs
check-pr-text.sh on a pull request that is not a draft and on the merge queue's group, the two
events this workflow runs that carry a pull request's text, and its own check run is the report,
so it posts nothing. pr-title.yaml carries that same check when the pull request is edited, the
event this workflow does not run on, as its own job's check run too; its header says why it runs
on nothing else.
The docs site's build on a push to main, and its GitHub Pages
deploy, are docs-pages.yaml's, a workflow of its own whose header says why: it builds on every
push to main, with no path filter.

What a workflow file of this repository holds, and where its longer shell bodies and its prose
go instead, is the rule .github/ci-notes/README.md writes down. Each `changes` output that hands
on a filter's verdict reads `steps.filter.outputs.<name>`, and rule 19 of
.github/scripts/check-ci-layout.sh holds `<name>` to an area of .github/ci-areas.yaml.

A new job joins the router in five places: an area in .github/ci-areas.yaml that names the
package directories and files the job reads; the `changes` output of that area's name, true when
its area matches or the unnamed-file step finds a file no area names; the job here, its
`needs: changes` and an `if:` on that one output carrying the term that skips it on a re-run
attempt of a merge-group run
(.github/ci-notes/ci.md#what-the-changes-job-selects-and-the-two-terms-every-job-carries
says why every job needs its own), a matrix with `fail-fast: false` whose legs each carry a
`name:` that is an expression giving the check-run name and reading a context (a job of one
name writes `${{ github.event_name && '<name>' }}`, since GitHub evaluates a name of string
literals alone even for a skipped job: #a-dispatched-runs-check-names), its steps in a
composite action .github/actions/<job>/ (one subfolder per leg with steps of its own) and its
last step the ci-ok-report step; and the job's id in the ci-ok job's `needs:`, without which
the verdict is reached before that job ends and says nothing about it. CI_OK_SELECTED_BY (this
file's env)
maps the job only where the output its `if:` tests is not named after the job (a carrier, or
kind-image on kind's output), and CI_OK_PULL_REQUEST_ONLY names it when its `if:` admits only
pull_request. A job that calls a workflow passes `ci-ok-changes: ${{ toJSON(needs.changes) }}`
to it under a `permissions:` block of its own, and that workflow's job ends with the report step.
An output an event must force is written in `changes`' outputs rather than left to its filter,
as every output's workflow_dispatch case is, and so is one an event must hold back, as the slow
suites' pull-request case is
(.github/ci-notes/ci.md#the-go-envoy-held-job-and-its-legs).
.github/scripts/check-ci-layout.sh holds all of it, its header numbering each rule, and
.github/scripts/check-workflow-files.sh holds this file to 32 jobs besides the carriers of a split
job's old name. The re-run term is on every job but `ci-ok` and
`pr-title`: each of those two is named for a check main's ruleset requires, where a skip is a
passing required check, so each refuses such an attempt in a step of its own instead.
A job split off another keeps its steps in that job's file instead: its caller passes
`job: <its id>` and its steps' job carries `if: inputs.job == '<its id>'`
(.github/ci-notes/ci.md#two-jobs-that-call-one-file).
docs/solutions/github/join-a-job-to-the-ci-router.md walks through it.

Caches: no step of the jobs this workflow calls saves an Actions cache entry itself; three
actions its jobs call can save one, and only in a dispatched run (setup-bun, and
architecture-check and spec-check at this paragraph's end). dispatch-two-builds restores the two
earlier Dispatch builds it runs (.github/actions/go-envoy-held/dispatch-two-builds) and builds one
on a miss; ci-caches.yaml's dispatch-builds job fills both on main (the two-build section below
says why).
setup-go caches nothing here (`cache: false` on every step, the two check actions'
included). The jobs that cache Go restore, in steps of their own, one of seven families with
actions/cache/restore: go-daemon (typecheck, daemon-go, plugin-pack — which compiles the same
internal/daemon test binary daemon-go's race step does —, daemon-live, pi-legion and legs of
agent-harness), go-docs (docs, and the build job of docs-pages.yaml, which saves it), go-envoy
(envoy-e2e-local, envoy-e2e-refill, envoy-session-stall and envoy-session-proofs, which build
the same listener and natsd, listener-server-change and envoy-go-build, which build the
listener, envoy-go-checks, whose script tests run under the setup they had in envoy-go-build's
job, envoy-go-race-docs, envoy-go-docs-finalizers, legs of agent-harness and the embedder job of
dispatch-browser.yaml), go-envoy-test (envoy-go, which builds the test binary of every package
of the envoy module, envoy-go-claim-model, which builds the API's of that set, a leg of
agent-harness, and the binaries, multiprocess and visual jobs of dispatch-browser.yaml, which
build the Dispatch server and the real broker's commands out of that same set),
go-envoy-darwin-vet (envoy-go-build's darwin vet, whose step's env gives it a build cache
directory of its own, so the standard library and the envoy module's dependencies compiled for
darwin ride in neither the go-envoy nor the go-envoy-test entry, which the other envoy jobs
download), go-envoy-infra (envoy-infra-build and envoy-infra-go) and go-notices (the notices
job). Every family but go-docs is filled by
.github/workflows/ci-caches.yaml, which builds those sets on a push to main and saves them;
go-docs stays with docs-pages.yaml, which saves it on that same push. Each family is kept per
runner architecture, keyed on the runner's architecture and on the Go version its job's
setup-go installed: a build cache holds the objects of the architecture that compiled them, so
an entry an x86_64 runner filled would give a job on ubuntu-24.04-arm its modules and none of
its objects, so each family is filled on every runner one of its jobs runs on.
go-notices puts the architecture before the OS in its key and the other families put it after:
its entry holds go-licenses, a program the notices job runs, and a run of a workflow file
older than the architecture in the key restores by go-notices-<os>-, which an <os>-<arch> key
starts with (the notices action's restore step says what that did). Every family ci-caches.yaml
fills but go-envoy-infra is filled again on every push to main: the fill starts from the newest
entry of its family, compiles what changed since, saves under its readers' key followed by the
commit, and deletes the entries the new one replaces, so a reader, whose restore matches its key
as a prefix, takes the last fill's (ci-caches.yaml's header says how). The key carries no hash of
the go.sum files: Go's build cache is content-addressed, so an older entry costs a run only what
changed, and a key on the go.sum files left a whole entry, up to a gigabyte, per change for seven
days. go-envoy-infra is the exception: what changed under it in eleven days cost its linux vet 83
to 85% of what the same vet for darwin took with nothing cached, so its key carries the hash of
its module files too, it is filled only when that key is missing, and ci-caches.yaml deletes the
entry each fill replaces (its envoy-infra job says how). The
fallback by
prefix, when the exact key misses, now applies on every run, a dispatched one on main
included: it was held off main so that the entry a run saved after a Go bump held nothing the
older Go built, and no step of this workflow saves a family any more (the two check actions
below fall back only to an entry of their own Go version, so an entry one saves holds nothing an
older Go built). There is no browser cache beside the Go
families any more: dispatch-browser.yaml's shard and alone jobs run in the image this
repository builds, which carries Chromium, WebKit and Firefox at /ms-playwright, so a
~/.cache/ms-playwright restore would carry nothing they do not already have, and
ci-caches.yaml's browsers job, the only writer of that entry, is gone with the restores
(dispatch://INFRA-1443). setup-bun reads and writes its cache only on main
(.github/actions/setup-bun), which of this workflow's events is the dispatched run alone, and
not in the tool-fallback and tool-fallback-arm jobs at all: that action reads the id of the
job that called it and passes no-cache for those two on every ref, so a restored Bun cannot
stand in for the install each of their cases is a reading of. An eighth family,
`architecture-check-<os>-<arch>-go<version>-<hash>`, is the composite action
.github/actions/architecture-check's own, and a ninth,
`spec-check-<os>-<arch>-go<version>-<hash>`, is .github/actions/spec-check's; envoy-go-build runs
both. Each action restores its family and saves it itself, from $RUNNER_TEMP/architecture-check
or $RUNNER_TEMP/spec-check and not from the Go caches above, keyed on the Go version its own
setup-go installed and on the SHA-256 of packages/envoy/go.sum (a miss restores the newest entry
of that Go version), and the ci-caches.yaml job of the action's name fills it on main. Each
action saves only in a push, schedule or workflow_dispatch run, so within this workflow, which
runs on no push and no schedule, only a dispatched run can save it (the bypass check dispatches
one at main), only on a miss of the exact key, and into the scope of the ref it was dispatched
on. check-ci-layout.sh's cache-family rule reads a workflow's own actions/cache steps and those of
the composite actions its jobs run, these two actions' restores and saves among them.

Runners: a job of this workflow, or of a workflow it calls, runs on ubuntu-24.04-arm unless it
needs an x86_64 machine, and runs on ubuntu-24.04 then. The two pools queue apart, and both
queue. On 2026-11-16 a job of this repository waited 456 s for an ubuntu-24.04 runner at the
median and 678 s at the 90th percentile, and the jobs every run waits on twice (`changes` at
the start, and `ci-ok` after the last job ends) paid that x86_64 wait both times. On the run
that first moved jobs here (run 44069656523, 2026-11-17), the 24 arm64 jobs waited 217 s at the
median and 542 s at most, the 19 x86_64 ones 890 s and 1428 s: an arm64 job that starts with
twenty others still waits for its turn, but the gating jobs started in 7 to 23 s (`lint` 7 s,
`ci-ok` 14 s, `changes` 17 s, `pr-title` 23 s). What keeps a job on
ubuntu-24.04, each a reason of its own machine and not of the job's code:
  * a tool with no arm64 row in .github/ci-tools.yaml: the Pulumi CLI, its docker plugin and
    uv, which only x86_64 jobs install (envoy-smoke, daemon-go, tool-fallback). A tool that has
    one is installed from its `-aarch64` row there, as changie (test and tooling-checks), crane
    (ci-images), mise and jj (pi-shared) and elan (airlock-lean) are;
  * the pinned Oh My Pi build, which .omp-pin names and whose release ships linux-x64 alone:
    daemon-go, plugin-pack, pi-envoy, pi-legion and envoy-session-proofs;
  * a browser: dispatch-browser (its container is an image built for linux/amd64 alone),
    dispatch-two-builds and docs (Playwright's Chromium on the runner's own libraries);
  * an image of this repository built for linux/amd64: worker-image, browser-image, and
    envoy-smoke;
  * envoy-go-build, whose pack step builds the release's listener archives with no
    CGO_ENABLED, as release-envoy-listener.yaml does on ubuntu-24.04: Go's default turns cgo on
    for a native build and off for a cross build, so only an x86_64 runner builds the release's
    own pair (.github/actions/go-envoy/envoy-go-build says what the arm64 runner built instead);
  * tool-fallback, which is the x86_64 half of the tool proof; tool-fallback-arm is its arm64
    half.
Every Go cache family carries the runner's architecture in its key, as the paragraph above
says, so a job moved from one runner to the other reads a cold entry until
.github/workflows/ci-caches.yaml fills its family on the runner it moved to.

## The steps and services every job repeats are YAML anchors

Three steps, three Postgres services and one `if:` stand word for word in many jobs of this file.
Each is written out once, in the first job that has it, under a YAML anchor, and every other job
names it with an alias:

  * `&checkout`, the checkout of the whole tree, and `&checkout-github`, the sparse checkout of
    `.github` alone that the carriers and claude-envoy-names make;
  * `&report`, the ci-ok-report step every job ends with (`- *report`);
  * the Postgres service in its three shapes: `&postgres-tcp`, whose health check reads the
    port (go-daemon, daemon-live); `&postgres`, whose health check reads the socket
    (agent-harness, airlock-arm); `&postgres-pg-legs`, started only for the legs whose matrix
    sets `pg` (go-envoy, envoy-held);
  * `&held-carrier`, the `if:` of go-envoy-held's six carriers, envoy-go to dispatch-memory: each
    runs under go-envoy-held's gate and `!cancelled()` (#the-carriers).

GitHub Actions reads anchors and aliases in a workflow file. It does not read the merge key
`<<:`, and this file does not use it. The other readers of this file expand each alias into the
node it names: actionlint 1.7.12; PyYAML, which every check under `.github/scripts/` that parses
this file loads it with (check-ci-layout.sh's ci_layout.py and UniqueKeyLoader,
check-workflow-files.sh, check-test-reach.sh, check-image-trigger-paths.sh, go_test_steps.py
and go-test-step-expansion.py, ci_router_outputs.py); and yq, through which ci-ok.sh and
ci-ok-report.sh read it. So a rule that reads a job's steps, services or `if:` reads the anchored
node in every job that names it. The aliases keep this file under the 63,000 bytes
check-workflow-files.sh holds it to (its rule 9; every other workflow file 64,000). An edit to
an anchored step changes that step in every job that names it. A job whose step or service must
differ writes its own out in full.

## Why the router runs on these events and no others

No `paths:`: every change starts this workflow, and `changes` selects what runs. No `branches:`
on pull_request: a pull request on any base is tested. No `edited`: a title or body edit starts
no run (pr-title.yaml checks those). `ready_for_review` starts the run a draft skipped. No
`closed`: tests-closed.yaml cancels a closed pull request's run
(.github/ci-notes/ci.md#the-routers-concurrency).
merge_group runs the router on the merge queue's commit of each queued group: `changes` selects
by what the queued change touches against the queue base (the group's base_sha..head_sha), and
ci-ok reports there, so the required checks pass on the merged tree before anything lands. No
`push`: the queue tests every change on the tree it lands as, so a run on main would only test
that tree again, and .github/workflows/ci-caches.yaml fills the Go caches on that push instead.
workflow_dispatch is the bypass check's (.github/workflows/main-bypass-check.yaml): it starts a
run at main's tip when a commit reached main without a passing merge-group ci-ok, and `changes`
selects every job there. A dispatch of any other ref is a full run started by hand (`gh
workflow run ci.yaml --ref <branch>`), and it reports under names of its own
(.github/ci-notes/ci.md#a-dispatched-runs-check-names).

## The migration-base input

The commit main pointed at before the push the bypass check read: envoy-go-build compares
every migration it holds with this run's tree, the range the push landed. A dispatch on
main without it fails that check, which cannot tell what the run lands. A dispatch on
another branch without it, a full run started by hand, compares with the branch's merge
base with main (.github/scripts/edited-migrations-base.sh).

## The kind-and-agent-harness input

The layout's changes job leaves kind and agent-harness out of a dispatched run unless this
input is true (main-bypass-check.yaml sets it), since every other dispatched run would add their
job-minutes.

## The router's concurrency

A newer push to a pull request cancels its older run, waiting or running. On pull_request the key
is the pull request's number: tests-closed.yaml joins this same group when the pull request
closes, cancelling what still waits or runs, and a merged pull request's closed event carries
github.ref = the base branch, so the number is the only key that names the pull request on every
event. A reopen within about a minute of the close can race tests-closed.yaml's run:
tests-closed.yaml says what happens and what to do. A merge group is a group of its own per
commit (its head) and is never cancelled: the queue waits for its ci-ok. A dispatched run keys on
the commit it runs at and is never cancelled either.

## Why the router's permissions block holds those two scopes

Read-only but for the two scopes the ci-ok steps use: `actions: read` reads the run's jobs,
which the ci-ok-report step names in the check it posts, and `checks: write` posts ci-ok (the
ci-ok job's reconcile step reads and PATCHes check runs with it too). The token also clones,
downloads pinned tools, and, in `changes`, reads the pull
request's file list, which this public repository serves without `pull-requests: read`. The
jobs whose scopes differ declare them in blocks of their own: pr-title (what check-pr-text.sh
and find-pr-payload.sh read, and neither of the two above, since it posts nothing), ci-images
(the ghcr.io push its copy makes) and worker-image (the image
push and its attestation's scopes). docs-pages.yaml publishes the site to GitHub Pages.

## The Go jobs' module proxy

The Go jobs' module proxy: a `|` makes a go command fall through to the next source after any
error, where a `,` falls through only after a 404 or a 410, so a download proxy.golang.org reset
mid-stream is fetched from the source and checked against go.sum (sjawhar/legion#2512).

## What CI_OK_SELECTED_BY holds, and which programs read it

ci-ok's exceptions: each job whose `if:` tests a `changes` output not named after the job's own
id, to that output (a carrier to its legs' job's, claude-envoy-names to pi's); every other job but
those CI_OK_ALWAYS_RUN names is selected by the output of its own id. Beside it, the list of the
jobs whose `if:` also admits only pull_request (ci-ok.sh and ci-ok.jq say how they decide a job's
selection). The ci-ok job's step takes both from this env; ci-ok-report.sh, which runs from a
composite action this file hands its job's own inputs alone, reads both out of this file at the
commit the run checked out, with the ci-ok job's `needs:`, the jobs ci-ok judges. Neither keeps a
list of check-run names: ci-ok.jq's check_run_jobs reads the name each leg and carrier gives
through its `name:` off this file at run time.

## The jobs that run on every change

The jobs that run on every change: no `changes` output gates one, so CI_OK_SELECTED_BY maps
none of them, and ci-ok passes only when each of them succeeded — a skipped, cancelled or
absent one fails the verdict, since nothing else says the job ran. `changes` is here because
it selects every other job, `lint` because every change is linted. ci-ok.sh reads this list
rather than knowing those two names, and check-ci-layout.sh holds every job of ci.yaml to
exactly one of ci-ok's three classes: gated on an output of changes (CI_OK_SELECTED_BY maps the exceptions), named by ci.yaml's
CI_OK_ALWAYS_RUN, or named by ci.yaml's CI_OK_EXEMPT. The list is a JSON array written over
lines rather than one long one, as ci.yaml's CI_OK_SELECTED_BY, a JSON object, is.

## The jobs outside ci-ok's accounting

The jobs outside ci-ok's accounting, each for a reason of its own. `cancel-started` exists
only to stop a merge-group run the queue has already thrown away, so it runs only on
`merge_group`, holds `actions: write`, runs no dependency code, reports no result to ci-ok
and is no job any verdict waits for. `ci-ok` is the verdict: it reports nothing to itself,
and the jobs it judges are its `needs:`, which check-ci-layout.sh holds to every job of this
file but these three. `pr-title` is the other check main's ruleset requires, carried by that
job's own check run: its result is nothing the verdict waits for, it carries no report step,
and the queue reads it as a check of its own, so a verdict that needed it would only make one
required check wait for the other.
That check asks none of the ci-ok rules of a job named here, and refuses
one that carries the ci-ok-report step: ci-ok.sh fails a result for a job CI_OK_SELECTED_BY
cannot map. The list is a JSON array written over lines rather than one long one, as ci.yaml's
CI_OK_SELECTED_BY, a JSON object, is.

## Why changes, pr-title, cancel-started and ci-ok run on arm64

changes, pr-title, cancel-started and ci-ok run on arm64: each runs JavaScript actions and
shell scripts over the image's own gh, jq, git and coreutils, and builds or fetches no binary,
so the architecture reaches nothing they compute; at busy hours the x64 pool queues a job for
about 15 minutes where the arm64 pool starts one in seconds (INFRA-4222).

## What the changes job selects, and the two terms every job carries

Selects the jobs that read what this change touched. Skipped on a draft pull request, so no
test job runs; marking it ready for review starts a run that selects by the same diff.

Also skipped on a re-run attempt of a merge-group run, by a term every job of this file but
the two named for a required check — `ci-ok` and `pr-title`, each of which refuses such an
attempt in a step instead — carries:
`(github.event_name != 'merge_group' || github.run_attempt == '1')` in
a job that runs on more than one event, and `github.run_attempt == '1'` in
`cancel-started`, which runs on merge_group alone. No job may lean on this one's skip
instead: `gh run rerun --failed` leaves out a `changes` that succeeded and hands the attempt
that job's outputs again, so an `if:` testing only `needs.changes.outputs.<x> == 'true'`
would run the job again in full, and its own report step would post a failing ci-ok on the
dead group's head the moment it failed. .github/scripts/check-ci-layout.sh holds every such
job to its term. A group is tested once: the queue tests a pull request it removed again
when it is added again, and a merge-group run `cancel-started` or
.github/workflows/dequeued.yaml force-cancelled tests a group the queue has thrown away, so
a passing re-run of it would put a ci-ok on a head no queue entry waits for. A re-run must
therefore test nothing and post nothing; the jobs of it that do run are `ci-ok` and
`pr-title`, neither of which can be skipped without leaving a passing required check behind,
and each concludes the failure such an attempt must get in a step instead
(.github/ci-notes/ci.md#why-a-re-run-attempt-of-a-merge-group-run-is-refused-in-a-step and
.github/ci-notes/ci.md#why-pr-title-refuses-a-re-run-attempt-in-a-step-of-its-own say why).

One output per filter of .github/ci-areas.yaml, named as the filter and read by the job of
that id (kind-image reads kind's, claude-envoy-names pi's, a carrier its legs' job's):

- pull_request: the job's filter matches, or the unnamed-file step found a changed file no
  filter names. The held jobs (go-daemon, go-envoy-held, envoy-held, scripts-held, pi-held,
  web-cold, dispatch-browser, agent-harness and airlock-arm) also need the full-ci label or one
  of the job's lane labels, the unnamed-file step inside that term
  (#a-lane-label-turns-on-one-held-job-on-a-pull-request); kind runs on no pull request.

- merge_group: every job.

- workflow_dispatch: every job but kind and agent-harness, which the kind-and-agent-harness
  input selects. dorny/paths-filter and the unnamed-file step are skipped there.

No job's if: but its own gate tests these; lint (with the test-reach search) tests none.

`unnamed_files` is whether a changed file matches no filter, which selected every job; ci-ok
reads it beside filter_verdicts on a merge group.

On a pull request the paths filter and the unnamed-file step read the file list from the API:
the filters and what the unnamed-file step runs are the whole checkout they need.

## The go-envoy-held job and its legs

The slow suites of the envoy module, whose runner time dwarfs every other job's: envoy-go, the
race lane (envoy-go-race, envoy-go-race-api and envoy-go-race-docs), envoy-go-claim-model,
envoy-go-docs-finalizers, dispatch-memory, dispatch-multiprocess, dispatch-two-builds and
envoy-smoke. The merge queue runs them on the tree the change lands as, and that is where their
result normally comes from; a pull request runs them only when it carries the label `full-ci` or
one of the job's lane labels (#a-lane-label-turns-on-one-held-job-on-a-pull-request). The job's
output is false on a pull-request run without such a label, so the skip is one `changes` chose:
ci-ok reads it as it reads any other output, and no job's `if:` knows about the label. The label
is read from the pull request the run's own event carries, so it takes effect at the next push,
reopen or ready-for-review, not when it is added: this workflow takes no `labeled` trigger,
which would start a run that joins this workflow's concurrency group and cancels the pull
request's run in progress. Take the label off once the run it was added for passes, since every
later push would run the slow suites again, unless a `ci_check` of the issue names one of them,
which reads `skipped` on a pull request without the label.

The suites are go-envoy-held's legs, one or more each; the jobs after it carry the six split
ones' old names. dispatch-two-builds-same, a merge group's alone, takes the defaults: an include
entry would add back what the exclude takes out (.github/actions/go-envoy-held/<leg>).

## The paths filter's own verdict for every filter

The paths filter's own verdict for every filter, before the label terms rewrite the outputs of
the nine held jobs (#a-lane-label-turns-on-one-held-job-on-a-pull-request). On a pull request
without `full-ci` or one of a held job's lane labels each of those nine is `false` whether its
paths matched or not, so a reader of the
outputs alone cannot tell "the filter said no" from "the label is absent"; with this
beside them ci-ok's report tells the two apart and says which
(.github/scripts/ci-ok.jq's `selection`). It gates no job and needs no plumbing of its
own: `toJSON(needs.changes)` carries every output of this job, so the ci-ok job and
ci-ok-report.sh both already have it. It is the one filter step's outputs as one object.
On a dispatched run that step is skipped and this is `{}`, which is right - the label
holds nothing back there, and the changes job's other outputs are all 'true'.

## The filter step

A dispatched run selects every job through the changes job's outputs, so there is nothing to
filter, and paths-filter has no diff base on workflow_dispatch: a step that failed here
would fail `changes`, whose result every job's `if:` reads.

## The lint job and its legs

lint reads the whole tree, so it runs on every non-draft run, also when `changes` failed, and
tests no output of it: the test-reach search (.github/scripts/check-test-reach.sh) is a step of its lint
leg. .github/actions/lint/<leg> holds a leg's steps; the private-names leg runs one script, the
one step that reads the deployment's term list.

## The lint job's scopes

The ci-ok-report step is the whole of this job's GitHub writing: it posts a failing ci-ok
with `checks: write` and names the run's jobs in it with `actions: read`. `contents: read`
is the checkout's. `pull-requests: read` is the daemon API contract check's: when it is
about to refuse a change it reads every open pull request of the repository, with the paths
each changes, to name a number no other one has taken. The pull request's text is the
`pr-title` job's, which declares the scopes its own routes read.

## The ci-images job

Copies every row .github/ci-images.yaml names into ghcr.io/cybermonklabs/legion/ci, then reads the
package to check that it holds every row. The order is the point: the read fails a row the
package does not hold, so the copy for this change runs first. The ci-images area selects
this job, and names the files that decide what its steps do: the rows, the scripts, the
module every reader of an image row goes through and the actions, and this file, which no area
names, selects every job. The gate's
other half reads the whole tree, and is a step of lint, which runs on every change: a file
that names a third-party registry again can be any file, and this filter names few of them.
This job is the one copy path: it runs on a pull request and on a merge group, which is every
event that can bring a new row in, and a failed copy is re-run with `gh run rerun`.
The copy of every tool asset is filled here too, in the step after the image half
(.github/scripts/mirror-ci-tools.sh): it is the same package, under the same login, and the
tool-fallback and tool-fallback-arm jobs read what this step filled, so that copy must
land before them.

## Why the ci-images job holds packages write

packages: write is the ghcr.io push, which is `crane copy` for an image row and the
registry API call .github/scripts/mirror-ci-tools.sh makes for a tool row; actions: read
and checks: write are the ci-ok-report step's.

## The tool-fallback job

The live proof of the second source every tool install of this repository now has: it faults
the host the first source is on, installs all nine tools through the source
.github/ci-tools.yaml names for each, and then serves eighteen corrupted payloads and reads
the refusal of every one of them.
It is permanent rather than a scaffold. A second source nothing exercises rots — a copy
nothing fills, a seam a refactor dropped, a digest nobody reads — and the hour of 504s this
work answers is the hour nobody wants to learn that in (dispatch://INFRA-1873). This
repository's own principle is that testing means booting and running it, so what says the
fallback works on the day the assets are down is a job that takes them down and installs
anyway.

Once the fault is up, no step of this job needs a GitHub release host to answer.
Every install that must succeed reads a source the fault does not hold — the ghcr copy, npm,
PyPI or get.pulumi.com — or, for the Pulumi CLI, whose primary is get.pulumi.com and whose
second source is the release asset, the origin serving that release's own staged bytes; the
bytes the corrupted cases serve were staged before the fault; and the one case about a
binary a third party installed seeds mise's install directory from that staged payload
rather than downloading one inside the fault.
That is what keeps ci-ok green during the very outage this job proves survivable: this job
is in ci-ok's `needs:`, and its area names .bun-version, the worker image's Dockerfile, the
copy's filler, the rig and its scripts and the actions, while a change to ci.yaml or to the
manifest, which no area names, selects every job, so a step of it that waited on a release
host would turn every pull request touching one of those files red on exactly the day the
fallback is doing its work.
.github/scripts/ci-tools-proof.sh is the rig, which this job and tool-fallback-arm drive;
its harness, which the scripts-held job runs, reads its ladder-report verb over logs of its own.
It needs ci-images for order, since that job fills the copy this proof reads, and carries
`!cancelled()` so a run that selected this job and not that one still runs: the ci-images
area names what the ci-images job reads and nothing more, so it is no superset of this
job's, and a skipped need would otherwise skip a job ci-ok has as selected. The file that
can bring a new row in, .github/ci-tools.yaml, is named by no area, so a change to it selects
both jobs, and the fill runs before this proof on every change that adds one.

WHAT THIS JOB DRIVES, AND WHAT IT ONLY REASONS ABOUT.
Everything a case is read on is driven here: the fault, the origin that stands in for all
four sources, the corrupted payloads, and the removals each case makes before it.
One thing is not. oven-sh/setup-bun, which .github/actions/setup-bun's second step pins,
restores a cached Bun into ~/.bun/bin/bun inside a step of its own, and the Actions cache it
restores from is on hosts this job's fault does not hold.
.bun-version holds an exact version, so that action builds its key and its URL without
asking any host which version to take, and nothing the fault refuses stands in its way.
Taking a bun away before that step is therefore no answer to the cache, since the step would
put a restored one back.
What keeps every case here a reading of an install is the wrapper's own `no-cache`, true for
every job whose id starts with `tool-fallback` — `github.job` in a composite action is the
id of the job that called it — so no call of that action in this job can be served by the
Actions cache, on any event and any ref.
That is read from the wrapper's expression rather than from a run of this job: a run that
told the action not to read the cache cannot measure what the cache would have served.
The removals the cases do make are about this job's own state: a case that must read the
second source takes ~/.bun/bin/bun and the bunx beside it away before its install, so what
it reads is not the bun an earlier case left.

READING THIS JOB WHEN IT IS RED AND HAS NO LOG AT ALL.
A job whose log GitHub answers `HTTP 404` with `<Code>BlobNotFound</Code>` for did not hang
and was not cancelled: it lost its runner. The log is written when the runner finalises it,
and a runner the service stops hearing from never finalises, while a job that was cancelled
keeps its log. What outlives the log is the check run's annotation, which the service
writes:
  gh api `repos/<owner>/<repo>/check-runs/<job id>/annotations`
On 2026-11-10 that answered, for this job: "The hosted runner lost communication with the
server. Anything in your workflow that terminates the runner process, starves it for
CPU/Memory, or blocks its network access can cause this error."
Read the job record beside it: a lost runner leaves one step `in_progress` and every step
after it `pending`, and the job ends before its own `timeout-minutes` would have fired.
Three wrong causes were offered for that one job before anyone ran that command
(dispatch://INFRA-2605). `docs/testing.md` says the same for any job of this repository.

BEFORE YOU PUT A BOUND ON A STEP OF THIS JOB: A BOUND BELONGS ONLY WHERE FIRING IT IS LOUD.
A step the runner ends at its `timeout-minutes` leaves the same `outcome` behind it as a
step that failed on its own, and GitHub gives a later step nothing that tells the two apart.
That is measured and not assumed: on this change's own run a step with `continue-on-error`
and `timeout-minutes: 1` whose action slept 180 s was ended after 72 s, and the step after
it read it as `outcome='failure' conclusion='success'`, so the failure is in the value an
assertion reads while the job's own result says nothing happened.
So on a step with no `continue-on-error` a bound is loud: it fails the job. This job's steps
are in .github/workflows/ci.yaml's tool-fallback job, and on one of the twenty-six installs
there whose control passes a step whose `outcome` is `failure`, a bound is silent: a hang
would satisfy that control and leave the job green, where the job's own ceiling would have
cancelled it and failed ci-ok. That is why each of those installs is timed as well as read -
the eight with nowhere to go by the control step "Check all eight installs with nowhere to go failed, and failed in their own time", the eighteen corrupted cases by
`report` itself - and why a new bound on a step some later assertion accepts the failure of
needs the same treatment or none at all (dispatch://INFRA-2605).

Each install is a step of ci.yaml's tool-fallback job (x64) or tool-fallback-arm job (arm64)
with its own 11-minute bound; two jobs rather than two legs of one, so no step carries a
`matrix.x64` or `matrix.arm` gate. .github/actions/tool-fallback/<x64|arm> runs what lies
between two of them.

## Why the tool-fallback job needs no packages scope

contents: read is the two checkouts'; actions: read and checks: write are the
ci-ok-report step's. Every read of the package here is the anonymous one an install makes,
so this job needs no packages scope at all.

## The arm64 half of the tool fallback proof

The same two halves of the Bun install, proved again on an arm64 runner.
.github/actions/setup-bun's third step selects a row from `uname -m` — `bun-linux-aarch64`
here — and reads .github/ci-tools.yaml through .github/scripts/ci-tools.sh, which needs
python3 with PyYAML. Every job of this file that runs on ubuntu-24.04-arm and installs Bun
makes that same read, so a runner image without PyYAML fails all of them; what this job adds is
the second half, the install from npm when the release asset serves nothing, which no other
arm64 job drives and which the arm64 leg of .github/workflows/release-envoy-listener.yaml would
otherwise meet first, on a push to main and so only after a merge.
It proves both halves: the healthy path, where the release asset serves Bun and the action's
fourth step holds it to the aarch64 row, and the npm path, under the fault the tool-fallback
job uses, where the release hosts answer nothing and the second source serves the same
binary. Each half ends in a reading of what this runner holds, printed for a person reading
the run: the machine, the version, the path, the row's own pins, and what ci_tools_verify
reads that binary as against the row.
The npm half says which source served it as well, because neither the version nor the
digest can: the release asset and the npm package are one binary by design, so a run where
the release host answered after all would read exactly as a run where npm served. What says
it is the fault proxy's own 504 for github.com after the probe, the refusal that sent the
install to its second source. The `ci-tools:` line the install wrote would say it too, and
no step can read it: a composite action writes that line to the job's log, which no later
step reads.
What .github/ci-notes/tool-fallback.md#the-steps-of-ciyamls-tool-fallback-job-in-actions-of-their-own
says of the Actions cache holds here too: the wrapper passes
no-cache for every job whose id starts with `tool-fallback`, so no call of it in this job
can be served by the cache either, on any event and any ref.
The npm half takes ~/.bun/bin away before its install all the same, so no Bun the healthy
half left behind satisfies it.
The tool-fallback area selects this job, through that job's output, as it selects that one:
both drive the same action over the same manifest through the same helper, so one area
decides both and neither can drift from the other. The job's `if:` tests that output, and
.github/scripts/check-ci-layout.sh holds its timed installs to their bounds (rule 31).
After the Bun halves it proves the other arm64 rows the same way: crane-aarch64,
changie-aarch64, mise-aarch64, jj-aarch64 and elan-aarch64, each installed once from its
release asset and once under the fault, where each must read its ghcr copy and each install's
own 504 is read after a probe of its own (the called file's comments say how). Those faulted
installs read the copies the ci-images job fills, so this job needs that job for order and
carries `!cancelled()`, as tool-fallback does and for the reason #the-tool-fallback-job gives.

## Why the tool-fallback-arm job needs no other scope

contents: read is the checkout's; actions: read and checks: write are the ci-ok-report
step's. Both sources serve this job's bytes without a credential, so it needs no other
scope.

## The fail-open jobs

lint, scripts, scripts-held, go-daemon and its two carriers, plugin-pack, text-checks and
notices are jobs ci-ok condenses; main's ruleset requires none of their names. Fail-open: when
`changes` did not succeed, all of them but lint, which runs on every change anyway, run rather
than skip. ci-ok fails then anyway (ci-ok.sh), so this buys signal: their results while
`changes` is broken. The set is main's fail-open typecheck (now a leg of lint) and test, every
job that took a suite from that test job, and notices, which was lint's step: the test job's
daemon Go tests are go-daemon's, the three tests plugin-pack runs by name are plugin-pack's,
its private-name port harness and `bun test scripts/sessions` are legs of scripts, as
tooling-checks and conflicts-script are, its harnesses and Bun suites are scripts-held's, and
the daemon's text checks are text-checks', so each of those carries the term too and a suite
that moves out of a fail-open job takes it with it. A re-run of only the failed jobs can leave
the jobs `changes` then selects skipped, and ci-ok fails a selected job that skipped rather than
passing with nothing tested: re-run every job instead.

The scripts job holds the checks of the tooling and of scripts/ a pull request runs, a leg each
named with its old job's check-run name (.github/actions/scripts/<leg>): tooling-checks,
private-names-port, conflicts-script and sessions-script. The script harnesses and scripts/'
Bun suites are the scripts-held job's, `test / test`
(.github/ci-notes/ci.md#what-the-test-job-runs-and-how-each-harness-step-is-gated).

## What the test job runs, and how each harness step is gated

The script harnesses and the Bun suites of scripts/: the scripts-held job, which keeps main's
check-run name `test / test` (.github/actions/scripts/test), a slow suite the merge queue runs,
and a pull request with full-ci or lane:test
(.github/ci-notes/ci.md#a-lane-label-turns-on-one-held-job-on-a-pull-request). No step of it
is gated: a selected job runs every harness, since a job's `if:` tests one output, so the area
of scripts and scripts-held names every file a harness reads, and when `changes` did not
succeed the job runs, as every fail-open job does. The plugin build, the changelog check, the
guides' layout step, the lints and the typechecks that ran beside these tests are the scripts
job's tooling-checks leg's, which a pull request runs.

## Two jobs that call one file

ci.yaml calls 4 unique reusable workflows, within the 10 .github/scripts/check-workflow-files.sh
allows it (its rule 5), and no file is shared between a job and its held half: each held half of
a split suite (scripts-held, pi-held, envoy-held, web-cold) is a job of ci.yaml that runs its
job's action. The one file two jobs call is dispatch-browser.yaml, whose callers are
dispatch-browser and dispatch-browser-pr, and every job of it runs under either: it takes the
caller's id as `job` for its artifact names, its read of its own jobs and its report, never to
pick a job
(.github/ci-notes/ci.md#the-browser-suite-job).

## What the tooling-checks job runs

The steps of the old test job that a pull request runs: the Claude Code plugin's build, check
and pack as the release runs them, the check that the plugin's build-input list covers what the
build reads, the render check of every changelog fragment with the changie it installs, the
guides' layout step, which runs whenever the job does (its area names the guides), and the
lints and typechecks of scripts/dispatch, scripts/github-apps, scripts/sessions and
scripts/approval-eval, with the typecheck of scripts/e2e/lib's plan ratings read test. They are
builds, lints and typechecks, which a pull request runs whatever their time, and they came to
1.0 minutes at the 90th percentile over 2026-11-22's pull-request runs (main's #5175, which
sorted each job by that time). It is the scripts job's tooling-checks leg
(.github/actions/scripts/tooling-checks) and fails open as that job does
(.github/ci-notes/ci.md#the-fail-open-jobs).

## The go-daemon job

The Go daemon's own suite: the plain step, the race step and the Stage 1 proof, with the
Postgres, the tools and the cache they need. It left the test job in dispatch://INFRA-1413,
where one filter had to cover both: a change to a shell script, to packages/claude-envoy or
to bun.lock ran 1,083 s (median) of Go tests that could not see it. Fail-open with typecheck
and test, for the reason #the-fail-open-jobs gives.

The daemon's Go suite runs in three legs of go-daemon: at two, one leg's -run would be about
136 KB of test names, past the 131,072 bytes Linux takes in one argument
(.github/actions/go-daemon says how each leg's bounds are read); daemon-go carries its old
check-run name. Its Postgres service runs ghcr.io/cybermonklabs/legion/ci, the copy of
pgvector/pgvector (.github/ci-images.yaml).

## Why the race step runs in legs of its own

go-daemon runs the race step in two legs of its own, daemon-go-race (1/2) and (2/2), beside its
three plain legs, as main runs it in a daemon-go-race job beside daemon-go (#5077), and the
daemon-go-race carrier keeps that job's check-run name, `daemon-go / daemon-go-race`. The plain
legs' package list is every package of the module less the race step's five (and the text
checks), as .github/actions/go-daemon builds both from one list, so the two steps share no
package and nothing orders them.

In one job their times added up. Over the 158 green daemon-go jobs of ci.yaml runs on 2026-11-22
from 13:46Z to 18:10Z (jobs API; the sample is the job list read at 18:57Z), the race step took
26.8 min at the median (36.3 at most) and the plain step 12.0 (15.0), after 1.4 min of setup, for
a job of 41.0 min. At the same time, the race job alone sets the wall time, and the second runner
costs the setup it repeats, about 1.4 min.

The split makes no test do less work. The race step's time is one package's: internal/daemon took
1,352 s of the step's 1,422 s in run 45048924209, and the four other packages of the step ended
inside it. That package's tests are where the rest of the time is.

## The carriers

A job split into legs keeps its old check-run name on a job of its own, which needs the legs'
job, runs under their gate and passes only when each of its own legs' newest record passed
(.github/scripts/leg-verdict.sh): a failure in another suite's leg leaves it passing. daemon-go
carries go-daemon's plain legs' name and daemon-go-race its race legs'; envoy-go, envoy-go-race,
envoy-go-race-api, envoy-go-race-docs, envoy-go-docs-finalizers and dispatch-memory
carry the old check-run names of go-envoy-held's split suites the same way. The legs of a split
job are named `<name> (<i>/<n>)`, each index from 1 to n once (check-ci-layout.sh's rule 34), so
the legs that read a sharded corpus (CORPUS_SHARD=<i>/<n>) read each of its documents once.

## The plugin-pack job

The three tests of the daemon's Go suite that this job's -run names: the one that packs the two
plugin releases and loads them, and the
two prompt probes of agentmodels_omp_test.go. They left daemon-go in
dispatch://INFRA-1413: the pack reads bun.lock and the two plugin packages, and the
agent-model probe stages skills/, so that filter named the lockfile and skills/, and a change
to either ran the daemon's whole Go suite. Here they run on their own inputs, and the
daemon-go race step skips them by name (check-ci-layout.sh's rule 22 holds the -skip to the
-run).

## The daemon-live job

The fifteen whole-daemon live tests of the daemon's Go suite that
this job's -run names: each boots the whole daemon over
this job's PostgreSQL and the package's test NATS server and drives it with no agent. They
were the drivers of scripts under scripts/e2e, which no job of this router ran; they
run here on each pull request and merge group whose paths select them, and the daemon-go race
step skips them by name (check-ci-layout.sh's rule 22 holds the two lists). Not
fail-open: their tests were in no job that `changes` failing runs.

Five of them came from main's scripts/e2e: the conflict past the review
(conflict_merger_live_test.go, #4979), the owner's plan and its client
(owner_steps_live_test.go and owner_steps_client_live_test.go, #4611), the group of each new
issue (plan_experiment_live_test.go, #4930) and part one of the approved version's news
(approved_version_news_live_test.go, #3995). Each lost its `e2e` tag and takes the package's
broker (workflowNATS), and its script went. The client test builds the plugin of the release
before from main's first-parent line, so the job checks out the whole history
(`fetch-depth: 0`): the test reads `origin/main` there and fetches nothing from GitHub. Part two
of the approved version's news is a tool, BenchmarkRealSessionsTellAndAnswerANewVersion, which
scripts/e2e/approved-version-news.sh runs and no job does. The package run's `-timeout 30m` and
the job's 40 minutes hold all fifteen: their run took 400 s in CI run 45359337842, the five 9 to
81 s each. Where the job's mean passes 22 minutes it splits into legs by explicit `-run` lists
whose union rule 22 holds.

## The agent-harness job and its legs

The agent harness: legs that each drive real agents, answered by model stand-ins, each well
inside its bound (means in CI runs 44816526076 to 44868985209: pane-drivers 10.3 minutes,
plugin-smokes 11.7, session-scripts 9.8, tmux-scripts 8.6, session-settings 7.1,
nats-callout-sessions 4.2). Each leg is named `agent-harness / <leg>` by an expression and keeps
the timeout-minutes its part had. Its steps are .github/actions/agent-harness/install, a step of
its own because its installs write the job's environment (LEGION_TEST_OMP, LEGION_TEST_CLAUDE,
LEGION_TEST_CODEX), and .github/actions/agent-harness, which runs the leg's steps and says what
each leg runs. pane-drivers runs the five live proofs of the daemon's Go suite that drive real Oh
My Pi panes, and tmux-scripts the Claude Code backend's proof on tmux beside its three scripts;
the go-daemon race step skips those six tests by name. Those two Go steps take the merge group's
retry, and each leg's bound holds the rule's sum: pane-drivers' step took 551 s at most, and one
run of each of its five tests is at most that again, so 9.2 + 9.2 + 3 minutes and 1.4 of set-up
come to 23 of its 40; tmux-scripts' job took 9.4 minutes at most and its one test 116 s, so 14.3
of its 45. Each "Install Playwright Chromium" keeps its 4-minute bound as `timeout 4m` around its
command. The merge queue runs the job, a dispatched run with the input kind-and-agent-harness,
and a pull request only with the label `full-ci` or `lane:agent_harness`
(#a-lane-label-turns-on-one-held-job-on-a-pull-request). Not fail-open: its tests were in no
job that `changes` failing runs.

session-scripts runs envoy-session-delivery.sh two more times against the base of the change, so
it fails when the head breaks the plugins that main ships. The step "Fetch the base the rollout
forms compare with" runs .github/scripts/fetch-compared-base.sh: it resolves the base from the run
(a merge group's base_sha, a pull request's base.sha, or main's head when a dispatched run
started) and fetches that one commit with its tree, at depth 1. Then two steps hand it to the
script by its id:
  * `--plugins-from <base>`: this checkout's hub with pi-envoy and claude-envoy built from the
    base, every window.
  * `--disruptions a --resume-plugins-from <base>`: both sessions start on the base's plugins, and
    window a's resume starts them on this checkout's.
Each runs once the fetch passed, whether or not an earlier step failed. Each prints its CHECK
lines and `ENVOY SESSION DELIVERY: PASS` or `FAIL`, and a failed check fails its step. In CI run
45440563929 the leg took 10.19 minutes, and the plain run 3.98 of them: 21 s of builds, 11 s for
the services and the sessions, and 3.4 minutes for the eight windows. Each form adds the base's
tree, its `bun install`, its pack and its claude-envoy build: 20 s or less (gVisor figures: the
install took 11 s, the pack and the build 2 s). So `--plugins-from` adds about 4.2 minutes, and
`--resume-plugins-from` with window a alone about 1.3. The fetch adds a few seconds. The leg is
expected to take about 16 minutes, under 25 and under its timeout of 30.

## The kind-image job

Builds the worker image the kind legs test, once per run, as worker-image.yaml's build builds it
(the same Dockerfile, context, BuildKit and main's layer cache, `cache-from:
type=gha,scope=worker-image`), into a docker archive each kind leg downloads
(.github/actions/kind/image). The kind job never runs on a pull request, so no run of it has a
worker-image build of its head to take the image from. It needs ci-images because its buildx
driver pulls BuildKit's row of .github/ci-images.yaml, and carries `!cancelled()`, as
tool-fallback does, so a ci-images job that `changes` skipped holds nothing back. It is the
`kind / image` part it was, named by `${{ github.event_name && 'kind / image' }}`, which reads a
context (#a-dispatched-runs-check-names), and reads the kind output.

It ends at its upload. The leak guard's test (scripts/e2e/leak-guard-image-probe.sh), which a
merge group runs in no worker-image job, ran here before the upload until CI run 45300865457,
where it took 115 s of this job's 8.4 minutes, 99 of them the `docker load` it needs, all of it
on the path every kind leg waits on; it runs on the kind job's leak-guard leg now.

The archive is zstd's, whole: its layers left uncompressed by the build, then one zstd frame at
level 3 with a 1 GiB window (`-T0 -3 --long=30`), which the leak-guard leg and each kind leg's
internal/kindtest decompress as `docker load` reads them (`zstd -dc --long=31`), zstd being the
runner image's own. Until then the artifact was the build's archive of gzip layers, 2,343.7 MB,
which zstd cannot shrink; each kind leg's download of it took 0.3 to 5.0 minutes in CI runs
45122591447, 45150132444 and 45165092344, and one of 45165092344's ran into its 10-minute bound.
On the uncompressed layers of main's worker image, 6,173 MB, zstd 1.5.5 wrote 1,957 MB at that
setting in 69 s of CPU, 16.5 % less than the gzip layers: level 3 without the long window wrote
2,170 MB, level 6 with it 1,877 MB for twice the CPU, level 9 without it 1,983 MB in 223 s
(measured on a devbox, not a runner).

## The kind job and its legs

The kind rig: legs that each bring up a kind cluster on the runner with Agent Sandbox's controller
and gVisor, a registry the node pulls from and the fake GitHub of
packages/daemon/internal/fakegithub, and run the Sandbox runtime's tests on it against the image
kind-image built. Each leg is named `kind / <leg>` by an expression and runs one test step of
.github/actions/kind, which says what each leg runs; both go-daemon Go steps skip every one of
those tests by name. Each leg's "Download the worker image the image job built" stays a step of
this job with its own 10-minute bound: one download in CI run 44829535701 stalled for 38 minutes.
The runtime legs take no retry (GO_TEST_RETRY=off), since a rerun of the root test would drop
the group the leg's -run selects; they keep 40 minutes against a slowest attempt of 23.0. Every
other leg takes it, with a timeout-minutes of at least its slowest set-up, its slowest test step,
one run of its root tests at their slowest and 3 minutes, in CI runs 44816526076 to 44868985209:
rig 22.0 (it keeps 25), shares 35.8 and burstable-and-sweep 38.3 (40), tree-claude 35.5 (36),
tree-stage4b-move 35.9 (36), tree-stage4b without the controller's restart 38.6 (39) and
tree-stage4b-restart without the listener's move 37.2 (38). The two new tree legs have no run of
their own yet; from their parts' times, tree-stage4b-controller 36.8 (37) and tree-stage4b-address
36.3 (37). burstable and sweep, burstable-and-sweep's two tests a leg each, keep its 40: each
one's set-up, test step and rerun is less than the two tests' together.

The leak-guard leg is no kind test: it downloads the archive as every leg does, loads it into the
runner's Docker and runs scripts/e2e/leak-guard-image-probe.sh in the image, as worker-image.yaml's
step after its build does, a merge group having no worker-image job, and brings up no rig, no
fence and no cluster; the action passes every other step over for it. Its 20 minutes hold the
download's own 10, the load and the test (115 s in kind-image) and the checkout.

Each rig leg builds its test binary beside the download: .github/actions/kind/build, a step of
this job before the download, sets up Go and gotestsum and starts in the background a `go test -c`
of the package the leg's test step runs, with that step's build flags, and the step before the
fence waits for it, prints its output and fails if it failed. The test step then finds every
package and its vet in the build cache and only links the binary. That step's own build took 96
to 125 s of each leg in CI run 45309357623, and the download 82 s in runtime-adopt and 115 s in
tree-stage4b. In that run the runner had 83 GB of its 145 free and the rig took about 20, yet
runtime-adopt waited 120 s for the background deletion of the runner image's unused toolchains, so
that deletion is gone; the fence step prints `df -h /` for each run. In each test,
internal/kindtest loads the worker image beside the cluster, Agent Sandbox, gVisor, the registry
and the stand-ins, 88 to 149 s against about 54 for those five in that run, and joins it before
the build FROM it.

Each rig leg fetches what the rig runs first (kind's build, gVisor's release, the node and registry
images, the kind network, the Go modules) and then runs its test step in a cgroup of its own, as
the runner's user, whose outbound connections, and those of every process it starts, reach
loopback and the kind network's bridge alone (.github/scripts/test-fence.sh, an iptables and an
ip6tables rule matched with `-m cgroup --path`); the step after it prints the rule, its match and
counters in both families, the connections it refused and Docker's image events since it began,
and fails on a missing match, a refusal or a pull, since Docker pulls as root, outside the rule.
The runner routes the cluster's pod subnet through the node's address on the kind network, so
the daemon a tree test runs reads each repository cache at its pod address, as the product does.
The node's containerd pulls from ghcr.io and the rig's registry alone, and CoreDNS answers
NXDOMAIN for every name outside cluster.local and the rig's stand-ins, the pods' web-search
provider among them; the rig's teardown names the refused names and each image the nodes pulled
(scripts/e2e/lib/kind-sandbox.sh), and the leg uploads each node's containerd journal and the
stand-ins' requests.

The chain `changes` → `ci-images` → `kind-image` → the longest leg → `ci-ok` was 32.2 to 34.0
minutes in those runs: 10.2 of it the jobs before and after the leg, 9.3 of that the image job,
whose builder's removal (1.7 minutes on average) cleanup: false leaves to the runner's end, and
22 to 23 the longest leg. So each leg whose mean passed 18 minutes is split: runtime-root into the
root and secrets groups, runtime-roles into roles and adopt, runtime-pod-update into pod-update
and pod-update-fleet (live_test.go's liveGroups, each group in one leg's -run), tree-stage4b's
controller restart into a test of its own, which reads nothing of the tree, and
tree-stage4b-restart's move of the listener's address into a test of its own, as the stream's move
is. Each new leg repeats the set-up, the compile and the in-test rig, 6 to 10 minutes. In CI run
45300865457 the chain was 30.4 minutes: changes 0.2, ci-images 0.6, kind-image 8.4 and
burstable-and-sweep 20.9, its two tests one after the other. So those two tests went to a leg
each, the deletion of the toolchains off each leg's path and the leak guard's test out of
kind-image: at that run's times kind-image takes 6.5 minutes and the longest leg, tree-stage4b,
about 19.3, about 26.8 for the chain. In CI run 45309357623 the chain was about 28.7 minutes:
changes 0.2, ci-images 0.5, kind-image 7.6 and runtime-adopt 20.1 (tree-stage4b 19.9). So the
test binary's build went before the download, the deletion went, and the worker image's load
runs beside the rig's first pieces: at that run's times the longest leg, tree-stage4b, takes
about 17.3 minutes and runtime-adopt about 16.0, about 25.9 for the chain. The merge queue runs
the job, and a dispatched run with the input kind-and-agent-harness; a pull request never does:
the kind output is false on every pull request, and no label lifts it
(#a-lane-label-turns-on-one-held-job-on-a-pull-request). Not fail-open: its tests were in no
job that `changes` failing runs.

## The private-names-port job, and the closure it took off the test filter

The harness that holds this repository's four private-name rules, as the `legion` binary of
this head runs them, to the frozen shell script's verdicts case for case
(.github/scripts/check-private-names-port.test.sh). It left the test job in
dispatch://INFRA-1413: the harness builds ./cmd/legion, so that job's filter had to name the
whole closure of that build, and a change to any package of the daemon ran thirty-odd shell
harnesses and the Bun suites of scripts/ that could not see it. Fail-open with typecheck,
test, daemon-go and plugin-pack, for the reason #the-fail-open-jobs gives, and as this step
ran while it was one of the test job's.

## The docs job

Builds the documentation site (docs/site) on every pull request and merge group that could
change it. No run of this workflow is a push to main: docs-pages.yaml builds and publishes
main's site to GitHub Pages, at the address .github/scripts/repo-paths.sh derives from the
repository it runs in (https://cybermonklabs.github.io/legion/ today), one push at a time in push
order. Both callers run the steps of docs-site.yaml, which holds them once. The site's
reference pages are generated from packages/ and skills/ at build time,
scripts/third-party-notices.ts writes the THIRD_PARTY_NOTICES.txt it publishes, and the built
site is checked with the private-name guard; the docs filter names all of them, docs-site.yaml
among them.

No timeout-minutes: GitHub's keywords for a job that calls a reusable workflow leave it out.
docs-site.yaml's own job carries the limit.

## The docs job's scopes

A calling job that declares no `permissions:` hands the called workflow the default
GITHUB_TOKEN permissions, not this file's block (GitHub's reusable-workflow reference,
"Supported keywords for jobs that call a reusable workflow"), so the scopes docs-site.yaml
needs are declared here. `contents: read` is its checkout's; `actions: read` and
`checks: write` are its last step's, the ci-ok report this call turns on.

## Why the session message-wait suite carries a database of its own

The session message-wait suite, which creates a session-store database of its own and
refuses without one. It was a step of the test job until dispatch://INFRA-1413: the one step
of that job that needs a database, which is why it has one here instead of a Postgres
service on a job whose other steps would start it for nothing.

## The pi-legion plugin's suite and its build

Two jobs over packages/pi-legion. pi-held, named `pi-legion / pi-legion`, runs
the plugin's test suite with what only it needs - jj, the pinned Oh My Pi, Go and the go-daemon
restore for daemon-pane's `go run`, and the NATS server controller-start-omp.test.ts's role
claim needs, which a step of .github/actions/pi/pi-legion starts with JetStream on (`-js`, which
a `services:` container cannot be given) for that file's two stream cases - on ubuntu-24.04,
since the Oh My Pi release ships linux-x64 alone. It is a slow
suite (14.8 minutes at the 90th percentile as the one job it was), held back behind its
`pi-held` output (.github/ci-notes/ci.md#a-lane-label-turns-on-one-held-job-on-a-pull-request).
The pi job's pi-legion-build leg builds contracts, typechecks the plugin and packs it as the
release does,
uploading the tarball as `pi-legion-pr`: a build and a typecheck, which a pull request runs
(0.8 minutes), on ubuntu-24.04-arm, since it installs no Oh My Pi.

## The ts job and its legs

The TypeScript packages, a leg each named with its old job's check-run name;
.github/actions/ts/<leg> holds each leg's steps.

## The pi job and its legs

The Oh My Pi plugins and the Claude Code plugin, a leg each named with its old job's check-run
name (.github/actions/pi/<leg>). The installs that write $GITHUB_ENV are steps of ci.yaml. The
NATS service of the pi-envoy leg, and the JetStream NATS the pi-legion job starts, run
ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml).

## The claude-envoy-names job

The packed plugin's private-name check (.github/actions/private-names-packed-plugin), a job of
its own: it reads the deployment's term list, which no job that runs dependency code holds.

## The dashboard's and the proof editor's lint, typecheck, unit tests, build and

The dashboard's and the proof editor's lint, typecheck, unit tests, build and asset checks.
They were split out of the browser suite (the dispatch-browser job), whose full lane - the one
the merge queue, the nightly and a pull request labelled `full-ci` or `lane:dispatch_browser`
run - is as slow as the suites the held jobs hold back, while these checks must come back on
every push.
No Postgres service and no Go, but contracts is built
before the suites: one case of the dashboard's test run spawns Playwright, whose own
resolution reads contracts' built dist/ (the comment on the step "Build contracts for the
suites that spawn Playwright" of .github/actions/ts/dispatch-ui says what failed
without it). tsc and
bun read @legion/contracts through the workspace's "bun" export condition and the dashboard's
Vite build aliases its sources, so that spawn is the whole of what the build is for.

## The browser suite job

Every browser row of the Dispatch suite, which .github/workflows/dispatch-browser.yaml
holds: the binaries job that builds the Go commands the lanes run, the shard legs, the three
`-alone` projects a machine each, the job of the suite's Bun rows that need a PostgreSQL, which
runs in the full lane alone, the fake-embedder job, which runs on both lanes, the re-recording
of the stubbed rows, the two-process job, the containerised visual review, and the merge job
that joins the shard and alone reports into one and posts its caller's ci-ok report.

Two lanes, a caller each, and which run pays for which. dispatch-browser-pr runs
`pull-request` on every pull request whose paths select the suite: `chromium`, `iphone` and the
two stubbed projects over the rows tagged `@pr`, with the fake-embedder job beside them, so such
a run proves those rows and no more - it is not the run that proves the suite before main.
dispatch-browser runs `full` - every project, the three `-alone` ones, the Bun rows that need a
PostgreSQL, the re-recording of the stubbed rows, the two-process job and the visual review -
in the merge group, a workflow_dispatch and a pull request carrying `full-ci` or
`lane:dispatch_browser`, which then runs both lanes. So the merge group is where a change meets
the whole suite before it lands, and the nightly
(.github/workflows/dispatch-browser-nightly.yaml, which calls the same file with the full
lane, from main, under the job id dispatch-browser) runs it again on main's tip.

Two inputs, each a literal of its caller's. `lane` decides the projects each job of the called
workflow runs, so nothing here chooses them a second time; it picks no browser to install any
more: the shard and alone jobs run in the image this repository builds, which carries all three
engines (dispatch://INFRA-1443), and the two-process job installs Chromium whatever the lane.
`job` is the caller's own id. GitHub names each job of a called workflow `<caller> / <job>`, so a
pull request's browser checks are `dispatch-browser-pr / shard (1)` and the like, and a
`ci_check` naming `dispatch-browser / merge` finds no run of that name on a pull request
without the label: the dispatch-browser job's own run concludes `skipped`, and no job of
dispatch-browser.yaml starts under it. The merge job reads only the jobs named under its own
caller, every artifact name starts with the id so the two callers of one run share none, and
the merge job's ci-ok report names it.

The matrix is in the called workflow and cannot be here. Every leg of a matrix shares one
job id, and .github/actions/ci-ok-report posts the failing ci-ok of a merge-group run from
the job whose own result decides that run, as that job's last step: legs written here would
each be `dispatch-browser`, so a decided group would take one such post per leg that failed,
each posting under a job id several legs answer to. A called workflow's legs are jobs of
that workflow, and one job of it - the merge job, which needs every other - posts the single
report its caller's id gets.

No timeout-minutes: GitHub's keywords for a job that calls a reusable workflow leave it out.
Each job of dispatch-browser.yaml carries its own limit.

## The dispatch-browser job's scopes

The same rule as the docs job's: a caller that declares no `permissions:` hands the
called workflow the default GITHUB_TOKEN permissions rather than this file's block, so
dispatch-browser and dispatch-browser-pr each declare the same three.
`contents: read` is every job of dispatch-browser.yaml's checkout; `actions: read` is the
merge job's `gh api .../actions/runs/<id>/jobs` read of this run's jobs, and `checks: write`
that job's last step, the ci-ok report.

## What the lane input reads

Each caller passes a literal: dispatch-browser-pr `pull-request`, and dispatch-browser and the
nightly `full`, so nothing reads a label or the event to choose the lane. A label decides only
whether the full lane runs on a pull request: dispatch-browser's output is a held job's, true
there only with `full-ci` or `lane:dispatch_browser` and a change its area selects
(#a-lane-label-turns-on-one-held-job-on-a-pull-request), while dispatch-browser-pr runs the
pull-request lane on every pull request its area selects. That is what lets a worker recheck a
slow-suite failure on a pull request: the label adds the full lane beside the pull-request one.

## The go-envoy job and its legs

envoy-go's fast half: the Go format check, the two generated-code checks, the vet of the module
as darwin compiles it, and the build and pack of
the envoy commands. It also runs the tests of internal/dispatch/architecture/notes,
internal/runnerlog, internal/dispatch/specgraph, cmd/architecture-check and cmd/spec-check,
whose one run this is (envoy-go's unit step leaves
them out), the architecture-note check over this repository's own .dispatch/architecture
through the composite action .github/actions/architecture-check, and the spec-file check over
its .dispatch/spec through the composite action .github/actions/spec-check, both of which
other repositories can call. The slow half, the envoy-go job (the module's unit tests, the
container-backed NATS matrix and the integration suite), is a suite of go-envoy-held, which
runs in the merge queue and on a pull request with `full-ci` or one of its lane labels, while
these checks must come back on every push. No Postgres service: nothing here opens a database. It
restores go-envoy rather than go-envoy-test, the family of the slow half (the test binary of
every package of the envoy module) and of dispatch-browser.yaml's Go-building jobs. It also
runs the guard that no CI file of this repository compares the spec files with the approved
spec.

go-envoy holds envoy's suites a pull request runs whenever its area selects them, a leg each
named with its old job's check-run name; .github/actions/go-envoy/<leg> holds the leg's steps.

envoy-go-checks - envoy's script tests, the load-test cluster's test with helm and the proof
editor's parity fuzz - is a leg of the envoy-held job, go-envoy's held part
(#the-envoy-held-job-and-its-legs): the one job it was took 23.4 to 25.5 minutes in CI runs
45122591447, 45150132444 and 45165092344, past the 10 minutes a pull request's job is held to.
The leg carries the old check-run name `envoy-go-checks / envoy-go-checks`, runs on
ubuntu-24.04, as the old job did, under the setup it had, and keeps 30 minutes
(.github/actions/go-envoy/envoy-go-checks).

## What the envoy-go job runs

The slow half: the unit tests of every package of the envoy module, the container-backed NATS
matrix and the integration suite, all against this job's Postgres. Its format, generated-code,
build and pack steps are envoy-go-build's, and its shell-script tests envoy-go-checks'.

The unit step, the envoy-go leg's share of the module's unit tests, is a step of ci.yaml to keep
a timeout-minutes of its own (main's #5084; .github/ci-notes/ci.md#the-envoy-go-unit-step-one-go-test-that-counts-the-brokers-skips).
TestStatementTimings of refs and of docs times nothing: each runs the statements its package's
BenchmarkStatementTimings times, once and untimed, on the database cmd/dispatch-seed fills, and
checks what they read, one package at a time after every Postgres-backed step of the leg (the step
"Check the statements on the seeded measurement database" of ci.yaml's go-envoy-held job). The
timings are the benchmarks', a tool's, since a runner's load decides a timed pair. docs's runs
under the cleanup detector (`-exec 'env GODEBUG=checkfinalizers=1'`), the mode main's
envoy-go-docs-finalizers job ran it in.

## The envoy-go unit step: one go test that counts the broker's skips

main's #5084 ran envoy-go's unit tests and the database broker's packages in one go test, where
they had been two steps, each waiting on its longest package with its `-p` slots idle behind it:
in merge-group run 45048924209 (job 137290624677) the unit step took 14.8 minutes behind
cmd/cipher's 690 s, and the broker's step 15.3 behind cmd/cipherd's 714 s. Each envoy-go leg
runs its share of that one go test, the step "Unit test envoy Go package" of ci.yaml's
go-envoy-held job, through packages/envoy/scripts/database-skips.sh:

  * `--module-except` adds to the broker's packages (./internal/cipher/... and ./cmd/cipherd/,
    less natscallout, the script's DEFAULT_PACKAGES) every package of the module the pattern
    keeps: the module less the race lanes' packages, internal/integration, internal/textcheck and
    the packages envoy-go-build tests. The script counts the skips of the
    broker's packages alone, and fails on any there but the airlock's model comparisons its
    EXPECTED_SKIPS names, and on a failed test of any package of the run;
  * `--run` is the leg's share, which .github/scripts/go-test-split.sh prints from the packages
    the envoy-go action's step "List the packages the unit step runs" lists with that same
    pattern into UNIT_PACKAGES, so the legs between them run each test of the run once;
  * `--skip` leaves out the web display fixture's two tests (envoy-go-build runs them), the
    backend count's five (its step runs them in the same job and env), pmdoc's six corpus tests
    (each leg runs them on its shard) and TestStatementTimings (the timing step runs it);
  * `DISPATCH_XID_REQUIRE_GAP_JUDGMENT=1`, as main's unit step set it: the refs package's xid
    window test runs here, so the step judges its statement-free gaps, and a runner whose kernel
    cannot show them fails that test naming why rather than passing it on the window's shape
    alone (storetest.XidRequireGapsEnv).

The `--timeout 15m` is the broker step's, which cmd/cipherd needed whole; a leg's share of it is
about a third. The step's 30 minutes leave a package that starts in its first 15 its whole
`--timeout 15m`, and end inside each leg's job limit, where main's 45 for the whole module's run
could never fire in the 40- and 41-minute legs, which reach the step about 4 and 9 minutes in.

## The envoy-infra module, its own workspace and its live tests

The envoy-infra Pulumi plugin's module (packages/envoy/infra). It is not in the root go.work:
its own go.work binds it and packages/envoy, so Pulumi's requirements never move the versions
the Envoy binaries and the daemon build with, and every go command of
.github/actions/go-envoy/envoy-infra-build and envoy-infra-go runs in that workspace. Vet
compiles the files behind the live-test tags (pulumilive, natslive) too, as
linux and as darwin compile them. The unit tests start no container; the natslive files run
in a step of their own against real
nats-server and Postgres, on testnats.Image, testnats.HubImage and testpg.Image, pulled first
for the reason .github/actions/pull-test-images gives: a slow suite (14.0 minutes at the 90th
percentile as the one job it was), held back behind the envoy-held job's output
(.github/ci-notes/ci.md#a-lane-label-turns-on-one-held-job-on-a-pull-request). That step runs only
the tests the tag adds, one call per package: envoy-infra-build's "Unit test envoy-infra" step
runs every untagged test of core, seed and gcp, so each call takes its -run from
.github/scripts/go-tagged-tests.sh, which reads the names off the source at run time, and
envoy-held's area in ci-areas.yaml is go-envoy's lines less what only go-envoy's legs read, so a
change that starts the tagged run starts the untagged one with it. The gcp
natslive tests pull no image: they install helm, nats-server, the config reloader and the
exporter with go install, at the versions the clustered hub runs, and render the vendored chart
in gcp/testdata. The per-target compile runs in the notices job.

## The text-checks job

The checks whose subject is text: each module's internal/textcheck package, the Dispatch API's
nine static source checks, which read the module's own source, and the two pure pins of
packages/daemon's supervise table. None of them opens a database or starts a container, so the
job carries no service and no `bun install`, and the three Go lanes (daemon-go, envoy-go and envoy-infra-build) leave those
packages, those seventeen tests and those two pins out of their own runs
(check-ci-layout.sh's rule 22 holds each -skip to the -run here). Its filter names the trees those tests
walk, so it is selected often, which is why the job is kept this cheap: five `go test` calls —
the four that preceded the supervise pins take 4 minutes 4 seconds with an empty build cache,
and the pins' call adds about a quarter more cold time
(.github/actions/text-checks's description).

## The notices job

The third-party notices of every Go release archive (.github/actions/notices), lint's step until
it took a filter of its own: the modules the release builds load, the target lists, its script.

## The race lane: what it runs, why it is three jobs, and what is not in it

The race lane: delivery (its intake handler reads the routing settings the poll loop
publishes, from NATS's delivery goroutine), the Dispatch store (the pool's one-connection
guard, and the hold mark of its own each concurrent read takes), the bus (two connects on one
SOCKS5 dialer record each dial's error in one record and read it back,
TestConnectsSharingADialerRecordItsLastError), the listener, where the role claim watcher, the
session and interest watchers and the request handlers meet, with the role claim view, its
lock and its epoch in the registry store and the watchers' completeness and rescans in kvwatch
(INFRA-141), the session cache, whose reads take its shared lock beside watcher writes and the
expiry timer's callback as kvwatch's resume point is read while a replaced watcher still
applies, natscred, whose service keeper probes with the connection's options while
internal/bus may be setting its handlers (natscred/probe_race_test.go, which only the race
build compiles, so this lane is its one run), the NATS auth service
(internal/cipher/natscallout), which starts a goroutine per request from nats.go's delivery
callback, counts those waiting for a decision slot and waits them out at Close, the sign-in
code flow (internal/oidc), whose sign-ins and refreshes use one shared x/oauth2 config at the
same time (TestCodeFlowGrantsUseTheDiscoveredConfigWithoutARace checks that use for a data
race), the Dispatch API and the document service - all under the race detector. A read
that walks a live document without its lock races the room's peers, which no ordinary run
shows: envoy-go's unit-test step runs without -race (LEGION-499). The lane sits beside
envoy-go because -race runs these packages several times slower, and each job carries a
package deadline of its own, since go test's ten-minute default is a whole package's.
envoy-go runs none of these packages plain any more (INFRA-1408): its unit step leaves out
exactly what the lane runs, and the one plain run it keeps of them is the step named "Test
what the race build cannot run at all or at full size", which runs the handful of
assertions the detector makes impossible or shrinks and nothing else.

The lane is three jobs: envoy-go-race (delivery, the Dispatch store, the bus, the listener,
the registry store, the session cache, kvwatch, natscred, the NATS auth service and the sign-in
code flow), envoy-go-race-api and envoy-go-race-docs, whose notes,
.github/ci-notes/ci.md#why-the-document-service-runs-alone-under-the-race-detector,
say why the document service runs alone.

api sits apart, on a runner of its own, because isolation is what made it fast: sharing a
runner, the race binaries contended for the four vCPUs, and alone api fell from 1099.487 s to
746.718 s. Running a heavy package apart also reads its seconds as its own job's, and leaves
the others' numbers standing when one is red.

The packages left in envoy-go-race share one go test, split by test name between two legs,
envoy-go-race (1/2) and (2/2): .github/scripts/go-test-split.sh gives each test to one leg by a
hash of its name, so each leg runs about half of each package. The envoy-go-race carrier keeps
the old check-run name, `envoy-go-race / envoy-go-race`. The packages shared one leg while the
heaviest of them were the bus, at 286.931 s, and the listener, at 365.787 s (run 40926978371).
In run 45396964994 that leg took 22.6 minutes, and in run 45401400508 22.2, past the 22 a leg is
held to: in the first, the listener took 1,088 s of the race step's 1,233 s, and it runs its
tests one at a time (their times add up to 1,082 s).

Each leg's time is predicted from the first run's log. The sums of the listener's test times give
leg 1 at most 617.4 s of the package and leg 2 at most 493.4 s, each with the package's 14.9 s
of start and end. No other package's share is longer: each package's time, scaled by its share
of its tests' times, is at most 291 s in a leg (delivery's in leg 2). Before the listener's
share starts, the step lists the tests (go-test-split.sh compiles the race binaries to list
them) and starts the go test: at most 2 x 144.4 s, the run's own compile up to the listener's
start, once for the listing and once for the go test, whose build is then cached (the API legs'
go test ran its first test 2.3 s after their listing ended). Leg 1: 3.3 s before the log
starts, 53.1 s of set-up, 288.7 s, 617.4 s, 2.6 s of upload and report and 2.0 s of report and
clean-up, 967.2 s or 16.1 minutes. Leg 2 also runs the forced-order steps, 62.8 s in that run:
3.3 + 53.1 + 288.7 + 493.4 + 2.6 + 62.8 + 2.0 = 906.1 s, 15.1 minutes. Run 45401400508's log, read
the same way, gives 946.1 s and 883.1 s, 15.8 and 14.7 minutes (its listener's shares 610.9 s
and 487.4 s, its compile up to the listener 137.0 s), so the timeouts below take the first run's.

The race step's -timeout is 21 minutes in both legs: the smallest whole minute at which the
step's slowest package, the listener's share in leg 1, takes at most 50 % of it (2 x 617.4 s is
20.6 minutes). Each leg's timeout-minutes is its predicted time plus that -timeout plus 6
minutes, rounded up: 16.1 + 27 gives 44 for leg 1, and 15.1 + 27 gives 43 for leg 2. Both are
above the retry's floor, the leg's time with one more run of each of its 10 longest tests
(419.6 s and 419.0 s) and 3 minutes: 26.1 and 25.1 minutes.

envoy-go-race-api's tests drive the Dispatch store, so its job carries the lane's Postgres
service.

pmdoc is not in this lane: its tests run plain only, in envoy-go's unit-test step
(INFRA-1408). It sat here because LEGION-571's flakes - pmdoc's own under -race and load, not
a race in production code - cost three review rounds when nothing ran the package under -race
before a PR claimed it. Its product code is still race-instrumented wherever a race job
compiles it, since envoy-go-race-docs imports pmdoc; what went is the second, race-built run
of pmdoc's own tests.

No job of this lane installs Bun: pmdoc's tests, which shell out to the generated TypeScript
fixtures under internal/dispatch/pmdoc/gen and refuse to run without bun in CI
(render_readback_test.go), were the only ones here that needed it, and they run in envoy-go's
unit step now, while the "Check pmdoc fixtures are up to date" step that keeps those fixtures
current is envoy-go-build's. delivery's, the bus's, natscred's and the auth service's NATS
runs in the test process (internal/testnats), so no job here needs a container runtime of its
own.

Delivery, the Dispatch store, the bus, the listener, the registry store, the session cache,
kvwatch, natscred, the NATS auth service and the sign-in code flow share this runner. The
Dispatch store's tests
(store_test.go:28) and delivery's take their databases from storetest.Open
(storetest.go:64-66), which reads DISPATCH_TEST_DATABASE_URL and skips without it, and
cmd/listener/dispatch_resolution_test.go:158 gets its database through internal/testpg, so the
job carries the lane's Postgres service; the registry store itself is the NATS KV view, and
natscred's, the auth service's and the code flow's tests open no store, so they need none.

## The mention sender's claim model alone, on a runner of its own

The mention sender's claim model alone, on a runner of its own: its search takes every core
the runner has for as long as it runs, which beside envoy-go's other steps would put its
minutes on theirs and theirs on its deadline. Its rules' test opens the store, so the job
carries a Postgres service.

## Why the document service runs alone under the race detector

The document service alone, under the race detector, with a deadline of its own. It is by far
the slowest package of the race lane - 1773 s of the 1842 s the first step of envoy-go-race
spent on main at 14d907b6 (job 123461966609) - and sharing a 30-minute step with the other
five groups left it no headroom: that step died on go test's own deadline at job 123503246603.
Its job is go-envoy-held, whose area names packages/envoy/** whole, so a change anywhere in the
module starts it. No Bun: no test file here runs it, and no
job of this lane installs it. It runs under -race because a read that walks a live document
without its lock races the room's peers, which no ordinary run shows
(TestReadsOfALiveDocumentRunBesideItsPeers, internal/dispatch/docs/live_read_race_test.go:39,
LEGION-499).

Its two legs, envoy-go-race-docs (1/2) and (2/2), split its tests by go-test-split.sh's hash, and
the hash gave leg 2 more of their time. The package runs its tests one at a time, so a leg's time
is its tests' time and a rest it repeats (set-up, the listing's compile, the go test's start and
end, the upload and the report). A test's time here is its `--- PASS` time, or, for the three
parallel tests, which wait for the end of the run, the time from the test before it to its own
end. In run 45396964994 leg 1 took 1,084 s, 978.0 s of them tests, and leg 2 1,295 s, 1,169.1 s
of them tests; in run 45401400508 leg 1 took 1,095 s (985.5 s) and leg 2 1,320 s (1,195.0 s). So
the legs' times add up to 2,379 s and 2,415 s, and the best two legs can do is 1,189.5 s and
1,207.5 s each, 19.8 and 20.1 minutes. Leg 2's rest holds 15 to 16 s that storetest's end takes
after its last test, which leg 1's does not.

The race step skips three render-reuse tests the hash gives leg 2, and leg 1 alone runs them in a
go test of their own: TestRenderReuseADocumentOutsideTheSchema (47.5 s and 49.2 s),
TestRenderReuseANilOriginCommitCreditsNoOne (37.0 s and 38.1 s) and
TestRenderReuseARecoveredReplicaRendersThePublish (15.5 s and 16.1 s), 100.0 s and 103.5 s. That
go test adds 18.2 s to leg 1: 2.3 s to start on the build the step before it cached (the
envoy-go-race-api legs' go test ran its first test 2.3 s after their listing), 14.6 s for the
store template its first test migrates (the longest first test of a leg in those runs) and
1.3 s to end, as leg 1's go test ends. Leg 1: 1,084 + 100.0 + 18.2 = 1,202.2 s, 20.0 minutes,
and 1,095 + 103.5 + 18.2 = 1,216.7 s, 20.3 minutes. Leg 2: 1,295 - 100.0 = 1,195.0 s, 19.9
minutes, and 1,320 - 103.5 = 1,216.5 s, 20.3 minutes. Where that go test ends as slowly as leg
2's, 15 s later, leg 1 takes 20.3 and 20.5 minutes.

The timeouts follow the race lane's rule (#the-race-lane-what-it-runs-why-it-is-three-jobs-and-what-is-not-in-it).
Leg 1's race step: 2 x 986.3 s, its go test in run 45401400508, is 32.9 minutes, so -timeout
33m; its moved tests' go test: 2 x (103.5 + 14.6 + 16.2) s is 4.5 minutes, so 5m; leg 2's race
step: 2 x (1,210.6 - 103.5) s is 36.9 minutes, so 37m. timeout-minutes: 20.5 + 33 + 6 = 59.5, so
60, and 20.3 + 37 + 6 = 63.3, so 64. Both are above the retry's floor, the leg's time with one
more run of each of its 10 longest tests and 3 minutes: 32.4 and 33.5 minutes. The legs' earlier
27 and 31 were below theirs (29.5 and 35.9 minutes in run 45401400508).

## The cleanup detector run of the document service

The same package under Go's cleanup detector, which names what the memory of envoy-go-race-docs' race run went to:
GODEBUG=checkfinalizers=1 makes the runtime report a cleanup or a finalizer whose closure or
argument reaches the object it is attached to, a cycle that keeps that object alive for the
life of the process, and that is the defect that held this package's memory
(dispatch://INFRA-1085). This is the only package of the repository that registers a cleanup,
so no other job needs it. It runs without -race: the detector is the runtime's own
bookkeeping, and a plain binary leaves the runner room the race binary does not. It reads the
same closure as envoy-go-race-docs, and runs in go-envoy-held beside it, on that job's `changes`
output. Its legs split the package's tests by name, but for its two corpus
tests, which every leg runs on its own shard of the repository's markdown: on every tracked
markdown file the two would take about 24 minutes (TestWriteCorpusReadsOnce on its 958 documents,
TestAMarginWriteMatchesTheWholeDocumentWrite on its 43), and the rest of the package 16, so one
leg would pass the time a leg may take.

## The real-process memory tests, and why they are a job of their own

The real-process memory tests (packages/envoy/cmd/dispatch/memory_test.go, LEGION-481): each
row starts a Dispatch server, drives it over HTTP and the document websocket and reads its
resident memory from /proc, and together they run past go test's default ten-minute deadline,
which took envoy-go red at 600 s (run 37116639565). They build only under the `memory` tag, so
envoy-go's unit run leaves them out and this job runs them with a deadline of its own. It runs
only the tests the tag adds: `-tags memory ./cmd/dispatch` would run the package's untagged
tests a second time, envoy-go's unit step having run them already, so the -run comes from
.github/scripts/go-tagged-tests.sh, which reads the names off the source at run time. The
assignment is a line of its own: under bash -e it carries the script's exit status, so an
empty or refused list fails the step instead of running -run "", which matches every test.

## The Dispatch server's two-process tests

The two-process tests (packages/envoy/cmd/dispatch/multiprocess_test.go, INFRA-66): each row
starts two real envoy-dispatch processes against one Postgres and one NATS - which is what
production runs - and drives them over HTTP, their own logs and NATS's own consumer and
stream reads. They build only under the `multiprocess` tag, so envoy-go's unit run leaves
them out, and they are a job of their own for the reason dispatch-memory is: envoy-go's unit
step holds each package's test binary to 15 minutes (database-skips.sh's `--timeout 15m`), the
memory tests took envoy-go red at go test's ten-minute default, 600 s (run 37116639565), when
it had no other, and one row here waits out a durable hand-over
or a maintenance loop's tick, tens of seconds each. Each row's NATS runs in its own test
process (internal/testnats), so the job needs no container runtime of its own. It runs only
the tests the tag adds, its -run from .github/scripts/go-tagged-tests.sh, for the reasons
#the-real-process-memory-tests-and-why-they-are-a-job-of-their-own gives.

## The two-build tests (packages/envoy/cmd/dispatch/twobuild_*_test.go): each row starts

The two-build tests (packages/envoy/cmd/dispatch/twobuild_*_test.go): each row starts
an envoy-dispatch of an earlier build and one of this tree against one Postgres and one NATS,
which is what a rolling release runs, and drives each build's CLI, stored documents, document
socket and bus traffic against the other (cmd/dispatch/README.md, "Two builds at once"). They
build only under the `twobuild` tag and are a job of their own for the reason
dispatch-multiprocess is: a row waits out durable hand-overs and settings polls, tens of seconds
each, and the earlier builds are built here first. Most rows run the previous build, the newest
legion-envoy-v* release in the history of origin/main; build-dispatch-build.sh resolves it,
which needs main's history and the release tags, and builds it from `git archive`. The job checks
out its own commit alone and then fetches main and the release tags with their history but none
of their files (`--filter=blob:none`), which `git archive` fetches for a commit when it builds it:
the whole repository, 3,242 branches and 3,710 tags on 2026-11-23, took the checkout to its
2-minute budget in three merge groups that day (.github/actions/go-envoy-held/dispatch-two-builds
has the numbers).
The first-rollout row runs the first-rollout build instead, the newest release before the first
that registers its schema numbers: the job pins it by its commit (FIRST_ROLLOUT_COMMIT), the
resolve step fails when its tag, legion-envoy-v30.20.2, names another commit, and the job builds
it the same way.
Each build is cached by its commit and the hash of the script that built it, with its src/ left
out, since the test reads only the parts the script copies out of it. The job restores each and
saves neither: .github/workflows/ci-caches.yaml's dispatch-builds job builds both on a push to
main, and again when release-envoy-listener.yaml has tagged a release (it dispatches
ci-caches.yaml with `only: dispatch-builds`), and saves them in main's scope, which every pull
request and merge group reads; its dispatch-builds-superseded job then deletes main's older
entries of each build and every entry of it on any other ref. The job saved them itself before,
in the scope of its run's own ref, where a merge group's entry is read by no later run and a pull
request's by that pull request alone: on 2026-11-23 25 such entries held 12.2 GB of the
repository's 25 GB cache and main held none, and on 2026-11-22 308 of 351 successful jobs built
the previous release again (107 s at the median, against 11 s to restore it). A run that resolves
a release main holds no entry of (one tagged minutes ago, or an older one a pull request's merge
base still names) builds it. Each row's NATS runs in its own test process (internal/testnats), so the job
needs no container runtime of its own. Then the browser half
(packages/dispatch/e2e/cross-build.multiprocess.e2e.ts), which runs twice: the browser suite's
two-process mode with Dispatch B run from the previous build's envoy-dispatch and web dist, then
from the first-rollout build's, and each direction's page served by one build against the other
build's server. Playwright installs as it does for dispatch-browser-multiprocess, and the rows
get a database of their own, since they truncate it. In a merge-group run the called file's
dispatch-two-builds-same job also runs the Go suite with this tree's own build as the previous
build, beside this job and not after it (that job's comment says why).

## The two-build legs' checkouts

The two-build legs check out this commit alone, 2 minutes an attempt and a second after a first
that did not succeed, then fetch main's history and the release tags without their files
(.github/scripts/fetch-release-history.sh), three attempts of 30 s
(.github/actions/go-envoy-held/dispatch-two-builds says why).

## The server-change soak

The server-change soak (packages/envoy/cmd/listener/server_change_test.go, INFRA-547): it
drives two real envoy-listener binaries over a three-server NATS cluster, with a session
stand-in moved between the servers by lame-duck shutdowns and forced kills and three senders
publishing through the listener, and holds every message to reaching its session or failing
its sender. It builds only under the `serverchange` tag, so no untagged run of the package
builds it, and it is a job of its own because it is slow and serial: it sends for at least a
minute and waits out each server's restart and the JetStream catch-up after it, which, beside
the listener's other tests, would hold up the job that runs them - envoy-go-race, which runs
cmd/listener under the race detector, where it would be slower still. It runs only the tests
the serverchange tag adds, since envoy-go-race runs the package's untagged tests already
(envoy-go's unit step leaves /cmd/listener$ out, for the lane to run under the detector), its
-run from .github/scripts/go-tagged-tests.sh for the reasons
#the-real-process-memory-tests-and-why-they-are-a-job-of-their-own gives. The cluster runs in
the test process (internal/testnats), so the job needs no container runtime and no database.

## The secrets broker's two-process tests

The two-process tests of the secrets broker (packages/envoy/cmd/cipherd/multiprocess_test.go,
INFRA-236): each row starts real cmd/cipherd binaries against one Postgres - which is what the
broker's Deployment runs once it carries more than one replica - and drives them over real
HTTP and their own logs. They race one refresh token and one device code against both
processes, migrate one empty database with both at once, prove a retag read by one process is
enforced by the other through the reread fan-out, and signal a process to watch it fail
readiness while it serves a request out past the old shutdown budget.

They build only under the `multiprocess` tag, so envoy-go's unit run leaves them out, and
they could not run there in any case: that step holds each package's test binary to 15 minutes
(database-skips.sh's `--timeout 15m`), the memory tests took envoy-go red at go test's
ten-minute default, 600 s (run 37116639565), when it had no other, and the
rows here wait out a migration race, a ten-second fan-out window and a twelve-second drain,
tens of seconds each. A leg of go-envoy rather than a step in dispatch-multiprocess, whose
Postgres service and shape this copies: the broker's two-process tests run on every pull
request go-envoy's area selects, while the Dispatch server's are a suite of go-envoy-held.
They need no NATS and no test-container pull - the broker starts nothing in a container - so
neither step is here. The step runs only the tests the tag adds, its -run from
.github/scripts/go-tagged-tests.sh, for the reasons
#the-real-process-memory-tests-and-why-they-are-a-job-of-their-own gives.

## The airlock-arm job

The airlock's check k9 and its lease test (.github/actions/airlock-arm), held as the slow suites
are, on arm64 as broker-multiprocess ran them. The job is a matrix of two legs, `airlock-arm / k9`
and `airlock-arm / lease`, each its own job of up to 45 minutes: the two together took 24.6 to
24.8 minutes in one job, against the 25-minute mean the layout holds a job to, before each
stack started in a step of its own and the scripts ran fenced. Its old check-run name,
`airlock-arm / airlock-arm`, goes with
the one job; neither leg is a split of one suite, so no carrier stands for the two.

Each leg starts its stacks as the runner's user, one step each, and runs the script against the
running stack (--stack-log) in a cgroup of its own, as the same user, whose outbound connections,
and those of every process it starts, reach loopback and the job's Postgres container alone
(.github/scripts/test-fence.sh); the last step prints that rule, its match and counters in both
families and what it refused. k9's stack serves github.com from a stand-in of its own
(dev-airlock.sh --git-stand-in, `dev-airlock-stands git` on 127.0.0.6:443), which k9's shallow
clone reads and must hold the commit of, and its negative control runs on a second stack once the
first is gone, since the two bind the same loopback addresses.

## The Lean model of the airlock, and why its fuzz run is two minutes

The Lean model of the airlock and the Go code it covers: the model is built, the axioms the
theorems Axioms.lean names rest on are printed and held to the three Lean's own kernel admits, and
FuzzAirlock drives Validate, Decide and Rewrite against the model's program, so a change made
on one side and not on the other fails here rather than in production.

The fuzz run is two minutes, because this is a required check and a required check stays
short. The ten-minute run the issue's verification asks for is the implementer's recorded
result on the head that merges, not this job.

## Why the container smoke test is a leg of its own

The container smoke test, the envoy-infra plugin's live tests, and the image both run. A leg of
go-envoy-held, carrying its old check-run name `envoy-smoke / envoy-smoke`, whose area names
what the image is built from, the Dispatch SPA and the Bun workspace as well as the Go sources,
so a Dispatch-only change, which changes the image, smokes it. When the smoke lived in
envoy-go, PR #1562 (packages/dispatch only) skipped envoy-go and never smoked the image it
changed (run 36494702052). .github/scripts/check-image-trigger-paths.sh fails the build when
that area misses a file the build reads.

## What the local end-to-end job runs

packages/envoy/scripts/e2e-local.sh: signed GitHub fixtures and a direct message through a real
listener and NATS, every frame rendered by both inbound renderers (renderInbound and Go's
Deliverer.Text) and checked against the acceptance contract.

## The session stall suites, the session state rig and the live Dispatch stack

The session stall suites (INFRA-548): each freezes a session's NATS path behind
envoy-client's stall proxy (test/stall-proxy.ts) on a real natsd and listener, which
test/envoy-stack.ts builds with go build from packages/envoy, and holds the client to a
reconnect within 60 s: envoy-client's shared connection options on their own, then the real
pi-envoy extension and the real claude-envoy channel server on consumer delivery, each
subscribing its relay subject again on the new connection, registering a new epoch, and showing
the message sent while it was cut off once. They run only with LEGION_E2E_ENVOY_STACK=1,
which the go-envoy job's envoy-session-stall leg and the envoy-held job's envoy-session-proofs
leg set, so the envoy-client, pi-envoy and claude-envoy jobs, which set up no Go, skip them.

The session state rig (dispatch://INFRA-392), packages/envoy-client/test/session-stall.ts, runs
on the same stack. Its first part needs no agent: it SIGSTOPs a child process carrying
envoy-client's own registration and heartbeat and holds the listener to `unresponsive` — the role
claim kept, the send stored — until the gone horizon. Its longest property waits out that horizon
and one reaper pass: the role claim reaper runs at ENVOY_ROLE_CLAIM_REAP_INTERVAL, which the rig
sets to two seconds, so the suite costs the horizon rather than the five minutes that reaper's own
default would cost it. envoy-session-stall runs that first part alone (`-t "^a session its box
stalls "`). The file's second part (what a stalled session keeps across a second listener, a bus
outage and Dispatch) and its third (the pinned Oh My Pi's and Claude Code's own sessions) run in
envoy-held's envoy-session-proofs leg, a step each, chosen by the name of each part's describe
block. With them this leg took 14.35 minutes in CI run 45440563929, past the 10 minutes a pull
request's job is held to. Without them it runs main's steps, 9.37 minutes in main's job
138232842142 on the same ubuntu-24.04-arm runner. No x86_64 runner is needed: the Oh My Pi and
Claude Code installs went with the third part.

The live Dispatch-stack suite rides this leg too (dispatch://INFRA-1727). It builds
packages/envoy's ./cmd/dispatch, makes a database of its own on DISPATCH_TEST_DATABASE_URL and
runs the real `dispatch` CLI against that server (packages/envoy-client/test/dispatch-stack.ts),
so it needs a PostgreSQL and a Go. The envoy-client job sets up neither, so a suite gated on
LEGION_E2E_DISPATCH_STACK would skip there for ever. The go-envoy area already names
packages/envoy/** and packages/envoy-client/**, so the suite brings no new path into this job's
closure. The approved-version bytes suite (src/__tests__/approved-version-bytes.live.test.ts)
uses the same stack in envoy-session-proofs.

The read-only token's live proof runs in envoy-session-proofs too:
packages/dispatch/e2e/read-token-live.ts against the two stacks its header names, the dev broker
(packages/envoy/scripts/dev-broker.sh, on the job's Postgres through DEV_BROKER_POSTGRES_URL, so
no Docker) and a `bun run dispatch:local` Dispatch that reads it. It needs the Postgres, Go, psql
and Bun that leg sets up. The envoy-held area names packages/dispatch/**, the script, its helpers,
and the SPA and e2e harness that Dispatch builds and runs.

## What the lint leg holds the CI layout and every workflow file to

Holds the layout of this file to the rules of .github/scripts/check-ci-layout.sh, whose header
numbers them: which jobs ci-ok judges and how, what an `if:` may test, the names a skipped job
carries, how a Go step runs its tests and retries them on a merge group, the bounds and runners it
keeps, and the notes these `why:` links name. Its rule 20 runs go list with GOPROXY=off over an
empty module cache, so the leg needs Go but downloads no module. It also holds every workflow file
to 64,000 bytes, to what GitHub's own parser reads, to 10 unique reusable workflows and to less
than 10,000 bytes of filter text, and this file to 63,000 bytes and 32 jobs besides its carriers
(.github/scripts/check-workflow-files.sh), which .github/workflows/workflow-files.yaml runs a
second time, in a file small enough to start when this one is too large to.

## What the proof editor's offline proof shows

The proof that no install of this repository fetches the proof editor's upstream sources over
the network: a frozen install runs behind a proxy that answers HTTP 504 for the hosts of the
old `github:sjawhar/proof-sdk#<sha>` route, and the job fails when the proxy's log holds a
single refusal. The steps, and why those hosts, are in the called file.

## The worker image's pull-request build

The worker image's pull-request build: worker-image.yaml builds the head and publishes its sha-
tag only. Main's image builds are release.yaml's call of the same workflow.

## The worker-image job's scopes

GitHub refuses a run whose calling job grants less than the called workflow's jobs declare,
whatever their `if:`
(.github/ci-notes/release.md#why-the-worker-image-call-declares-every-scope-worker-imageyaml-names
says the same): its workflow-level block
(the build's packages: write, and the actions: read and checks: write of its ci-ok-report
step) and its `attest` job's id-token and attestations: write. A pull request run still skips
`attest` (its `if:` requires `github.event_name != 'pull_request'`), and, passing no
`attest_digest`, leaves it empty, so the build job runs.

## The browser-image job

The browser image's build: browser-image.yaml builds .github/docker/browser.Dockerfile and
pushes it when the package does not already hold an image built from it. Unlike the worker-image
job, this job has no `pull_request` term: it runs on a merge group too, so the image a
merged head's `container:` jobs pin — the shard and alone jobs of
.github/workflows/dispatch-browser.yaml — is built from that head. browser-image.yaml carries
no `why:` line of its own: each of its bytes is part of the image's recipe
(.github/scripts/ci_images.py, `recipe_sha256`), so a comment added there moves the hash the
published image's label must equal, and the link lives on ci.yaml's call of it.

The job's scopes: GitHub refuses a run whose calling job grants less than the called workflow's
jobs declare, whatever their `if:`: its workflow-level block (the build's packages: write, and
the actions: read and checks: write of its ci-ok-report step) and its `attest` job's id-token
and attestations: write. That `attest` job runs whenever the build pushed, whatever the
event, a pull request's run included, because the two-push order .github/ci-images.yaml's
header describes has a pull request's run push the image the rows then pin
(browser-image.yaml's `attest` job says why).

## The pr-title job

pr-title, the other check main's ruleset requires of this router, and with `ci-ok` one of the
two jobs of this file named for a required check. A pull request's title, body and commit
messages reach main's squash commit, the generated release notes and GitHub's pages, and its
comments and reviews are public on its page, so this job checks them, and that the title is a
conventional commit subject, at every push to a pull request that is not a draft, when a
draft is marked ready, and on the merge queue's group.

It posts nothing: pr-title is this job's own check run, which GitHub puts in this run's own
check suite, where a check run created with POST /repos/.../check-runs under GITHUB_TOKEN
joins the Actions App's first suite at that commit instead, which has left a required check
green in another workflow's suite and the merge queue refusing to enqueue
(.github/scripts/post-check-run.sh's header holds those measurements). So no step here calls
that script, and the job holds no `checks: write`.

A merge group carries no pull request, only the queue branch it runs on, so
find-pr-payload.sh reads the pull request that branch names and writes it in the shape
check-pr-text.sh wants. pr-title.yaml runs on an edit alone, whose payload carries the pull
request, so a pull request that is opened, reopened or pushed to gets its
pr-title from this job, and a fork's head gets one too: a job's check run costs no token
scope, where a POST needs `checks: write`, which a fork's run's read-only GITHUB_TOKEN does
not carry. A dispatched run carries
neither a pull request nor a group and has no text to check, which is why the `if:` names the
two events this job runs on rather than excluding the draft alone. Skipped there, the job still
posts a check run, and a skipped one is a pass. GitHub does not evaluate the name of a job it
skips, so on every run that skips this one, a dispatch of any branch and a draft's run, the
check run carries its `name:` expression's own text and not `pr-title`
(.github/ci-notes/ci.md#a-dispatched-runs-check-names).

The job reads nothing of the tree but find-pr-payload.sh, check-pr-text.sh and the
check-private-names.sh that one runs, so the .github cone is the whole checkout it needs.

## What the pr-title job's scopes are for

What the two scripts this job runs, find-pr-payload.sh and check-pr-text.sh, read through gh, and nothing else, through
GitHub's GraphQL API, whose budget is apart from the REST one the rest of CI spends: the
pull request's commits, from GitHub's comparison of its base branch with its head, and the
commits of its base branch that touch .github/issue-key-cutoff (`contents: read`; the REST
compare route reads the commits instead when that comparison is short, as it is past 1000
commits), the pull request itself, its reviews and its review comments (`pull-requests: read`,
which find-pr-payload.sh's read of the pull request a queue branch names needs too), and its
issue comments (`issues: read`). The issue-key rule's releases and diffs are REST reads, made
only once that rule is active (`contents: read`, and `pull-requests: read` for the pull request's
files). No
`checks: write`, since this job posts no check run, and no `actions: read`, since it carries
no ci-ok-report step: CI_OK_EXEMPT holds it outside ci-ok's accounting (this file's env says
why).

## Why pr-title refuses a re-run attempt in a step of its own

A merge group is tested once: the queue builds a new group rather than re-running one, so
an attempt above the first checks a tree the queue is no longer waiting on. This job
therefore carries no `github.run_attempt == '1'` term, unlike every job of this file but
the verdict, and refuses such an attempt in a step instead. `ci-ok` carries that same step
for the same reason, and the two share the rule because they are the two jobs named for a
check main's ruleset requires: an `if:` can only take the job away, a skipped job's check
run is a passing required check, and a dead group must get a real failure rather than the
check concluded by nothing. It comes right after the sparse checkout, which brings
.github/scripts/refuse-run.sh, the script that prints the refusal (as ci-ok's two refusals
do), and before the check step, since no step after it runs once it has failed.

## The pr-title check step

PRIVATE_NAME_TERMS is the deployment's own list, which check-private-names.sh's fifth rule
reads and requires on a merge group (that script's header). Both scripts run by the
checkout's own path, the form every job that holds the list runs the check by;
check-pr-text.sh runs check-private-names.sh from its own directory.

## The merge queue's cancel act

The merge queue's cancel act. Main's queue builds each group on top of the group below it,
so when a group fails, every group built on it can no longer merge: the queue takes that
pull request out and rebuilds the groups above it. That removal is a dequeue, so
.github/workflows/dequeued.yaml force-cancels the removed group's own run, and the groups
built on the failed one keep their runners until the queue rebuilds them.

`cancel-started` runs at the start of every merge-group run, with no `needs:` so that it
starts at once, and it has one act: it cancels the groups of the same pull request the
queue has replaced, which test a tree that will not land. Its own queue branch names that
pull request (`gh-readonly-queue/<base>/pr-<n>-<parent sha>`), and
.github/scripts/merge-group-cancels.sh decides from it and the run list which runs those
are: the script makes no GitHub call and prints one `cancel <run id>` line per run to
force-cancel, in the order to act on them; its header says what it reads and why.

Each cancel is `POST /repos/{owner}/{repo}/actions/runs/{id}/force-cancel`, acted on in that
order. A force-cancel bypasses `if: always()` — the step of a running job is left skipped,
not run, measured on a probe run of this repository — so a force-cancelled run posts no
ci-ok, and its ci-ok job, which waits for every job of the run, is left unstarted or
cancelled rather than concluding one. That is what this act wants: a failing ci-ok on a
group the queue still holds could
take an innocent pull request out of the queue. The run of the front group, whose base is the
tip of main, posts that group's failing ci-ok from the job that failed, through the merge-group
branch of .github/scripts/ci-ok-report.sh, so the queue reads the verdict and rebuilds the
groups above it without waiting for the rest of that run. A group behind the front posts
nothing early: the queue takes its pull request out only once the groups in front of it have
ended, and a failing ci-ok makes the queue count the group as no longer building and build
another on it, which built 41 groups in 28 minutes behind one broken entry on 2026-11-22
(.github/scripts/ci-ok-report.sh's header). Its run's ci-ok job reaches its verdict at the end of
the run and holds a failing one until the group is the front, for the same reason
(.github/scripts/ci-ok-hold.sh). The cost: when the group becomes the front before that run
ends, the entry leaves at the run's end, or when a later job of the run fails once the group is
the front, rather than the moment the groups in front end.

The job follows these rules:
  * it runs only on `merge_group` and only on attempt 1
    (.github/ci-notes/ci.md#what-the-changes-job-selects-and-the-two-terms-every-job-carries
    says why a re-run attempt does nothing);
  * `permissions:` is `actions: write` and nothing else. `gh run list` and the force-cancel
    need it (a scope granted `write` grants its read); the checkout needs no
    `contents: read`, because this repository is public and GitHub serves a public
    repository's git data to any valid token — measured here with `git ls-remote` over https
    against a public repository the token holds no grant on at all, which succeeded;
  * it carries no ci-ok-report step and reports nothing to ci-ok, so its id is in
    CI_OK_EXEMPT in this file's env, outside ci-ok's accounting;
  * it runs this repository's own .github/scripts/cancel-started-act.sh, and the
    merge-group-cancels.sh that script runs, out of the run's own checkout and nothing else:
    no bun, no go, no node, no installed dependency, no npm or
    bunx tool. A merge-group run checks out the queued tree, so the script it runs is the
    queued change's copy, which that change's review has read;
  * its step's token is ${{ github.token }}, and its time limit is short: the act is a
    handful of API calls.

A force-cancel that fails leaves a warning rather than a red job: the run it names may have
completed between the list and the call, and an act that reclaims nothing is no failure of
the run it runs in.

The job reads nothing of the tree but .github/scripts/cancel-started-act.sh and the
merge-group-cancels.sh it runs, so the .github cone is the whole checkout it needs.

## The ci-ok job

The verdict, and one of the two checks main's ruleset requires of this router. It needs every
job of this file but itself, `cancel-started` and `pr-title`
(.github/ci-notes/ci.md#the-jobs-outside-ci-oks-accounting says why those three are outside
ci-ok's accounting), runs under `always()` so that a failed,
skipped or cancelled job still reaches a verdict, and fails when .github/scripts/ci-ok.sh's
verdict over `toJSON(needs)` is failure. ci-ok is therefore this job's own check run, which
GitHub puts in this run's own check suite, where a check run created with POST
/repos/.../check-runs under GITHUB_TOKEN joins the Actions App's first suite at that commit
instead, which is another workflow's as often as not (.github/scripts/post-check-run.sh's header
holds that measurement).

A job of its own waits for a runner, and that wait is the cost of this shape: it is created
with the run but starts only once the last of its needs has ended, when the pool is busiest
with the next wave. docs/solutions/github/join-a-job-to-the-ci-router.md holds every reading
of it. This job runs on ubuntu-24.04-arm because it needs nothing of an x86_64 machine, and
that pool queues apart from the x86_64 one and is idle by the time the last need ends: on the
run the "Runners" paragraph of .github/ci-notes/ci.md#the-router-every-check-a-pull-request-or-the-merge-queue-runs measures, it started 14 s after it was created. The shape
is kept because a late verdict is right while one in another workflow's
suite blocks the head until a person re-runs it. What this job cannot do, and the
ci-ok-report step every other job ends with still does, is post the failing ci-ok of a
merge-group run one job's result has already decided: this job waits for every job of the
run, and a group that can no longer merge should leave the queue at the first failure rather
than at the last job's end. What this job does do, when its verdict fails on a merge-group run,
is hold that failure until the group is the front of the queue: a failing ci-ok on a group
behind the front makes the queue build on the failed entry, and decides nothing sooner (the
"Hold a failing verdict until this group is the front of the queue" step says why).

This job's `if:` is `always()` and nothing else. Every other job of this file is skipped on a
draft pull request: `changes` carries `!github.event.pull_request.draft`, the jobs its
outputs gate skip with it, and lint, scripts, scripts-held, go-daemon and its two carriers,
plugin-pack, text-checks, notices and `pr-title`, whose `if:`s do not wait on `changes`, carry
that term themselves.
Every job but `pr-title` and this one is skipped on a re-run attempt of a merge-group run by
`github.run_attempt == '1'`
(.github/ci-notes/ci.md#what-the-changes-job-selects-and-the-two-terms-every-job-carries says
what both terms do for them); this
job carries neither. A job an `if:` holds back is skipped, and a skipped job's check run is a
passing required check, so either term would leave the head a ci-ok nothing judged: a merge
group would merge on it, and a draft would carry it as a pass. An `if:` can only take the job
away, while what both cases must get is a verdict, so the job runs on both and a step of it
concludes the failure: the two refusal steps, "Refuse a draft pull request" and "Refuse a re-run attempt of a merge-group run", one for a draft pull request and one for an
attempt above the first. `pr-title`, the other job named for a check that ruleset requires,
holds no attempt term for the same reason and refuses such an attempt in a step of its own too.

So on a draft this job is the whole run: every other job is skipped, there is nothing to
judge, and the failure of its draft refusal is what keeps a draft from carrying a passing
ci-ok. That failure names the remedy — marking the pull request ready for review starts a run
that selects by the same diff and concludes a real verdict over it.

`checks: write` and `actions: read`: the verdict step reads this run's check runs and their
annotations, to tell a job a runner shutdown killed from one the change broke (ci-ok.sh's
call of runner_shutdown_kills, runner-shutdown.sh), which `checks: read` covers and the
`checks: write` here carries, and
the reconcile step reads this head's ci-ok check runs and PATCHes them with that scope.
`actions: read` is the hold step's one read of this run's creation time. The checkout needs no
`contents: read`, as `cancel-started`'s does not — this repository is public, and GitHub serves
a public repository's git data to any valid token — and the hold step's `git ls-remote` of the
queue's refs needs no token at all.

Each Go step's gotestsum jsonfile and rerun report (.github/scripts/go-test.sh) is uploaded as a
go-test-* artifact, which the ci-ok job downloads and reads for its summary: each test that
passed only on its retry and each package that failed with no test failing. A run whose Go jobs
all skipped uploads none, and the step after the download says so; the report decides nothing of
the verdict.

## What ci-ok checks out

The job reads nothing of the tree but what its steps run: refuse-run.sh; the ci-ok-retries
action, its ci-ok-retries.sh and the go-test.jq that script includes; ci-ok.sh, the ci-ok.jq it
runs and this file, which it reads the check-run names and the label terms off; ci-ok-hold.sh;
ci-ok-reconcile.sh; and the gh-read.sh and runner-shutdown.sh those scripts source. So the
.github cone is the whole checkout it needs.

## Why a draft pull request is refused in a step

A draft pull request has asked for no test: every other job of this file is skipped
there, by the draft term `changes` carries or one of its own, so what this job would
judge is a run of nothing but skips. The refusal is a step and not a term of this job's
`if:`, as .github/ci-notes/ci.md#the-ci-ok-job
says: the `if:` would skip the job, and a skipped
job's check run is a passing required check, so the draft would carry a green ci-ok
nothing judged.

It comes before the merge-group refusal, the order a change meets the two cases in, and
the order decides nothing anyway: no run meets both. A merge_group event carries no
`github.event.pull_request`, so this term is false on the queue's runs, and a draft
cannot be queued.

## Why a re-run attempt of a merge-group run is refused in a step

A merge group is tested once: the queue builds a new group rather than re-running one, so
an attempt above the first judges a tree the queue is no longer waiting on, and the
verdict over it is failure whatever the jobs of the run did. Every other job of the file
is held back on such an attempt by a term of its own `if:`, so what this job would read
there is a run of skips; this step refuses it before the verdict step reads them.

The refusal is a step and not a term of this job's `if:` because a job an `if:` holds
back is skipped, and a skipped job's check run is a passing required check: the group
would read a ci-ok nothing judged and merge on it. An `if:` can only take the job away,
and what a dead group must get is a verdict, so the job runs and a step of it fails.
It comes after the checkout because the reconciliation step, "Set every other ci-ok on this head to this job's conclusion", runs under `always()`
and reads ci-ok-reconcile.sh out of the tree.

## What NEEDS holds

NEEDS is `toJSON(needs)`: `changes`' result and outputs with every other job's result,
which is the shape ci-ok.sh takes. A failure here fails the job, and the job's check run
is the verdict. GH_TOKEN and HEAD_SHA are what that script asks GitHub with: a job of
this run that ended `failure` is read against the check runs on this head, where the
runner's shutdown annotation tells a job the platform killed from one the change broke
(runner-shutdown.sh). That read is this job's `checks: write`, which carries the
`checks: read` it needs; the reconcile step, "Set every other ci-ok on this head to this job's conclusion", reads the same route with the same
token. GITHUB_REPOSITORY and GITHUB_RUN_ID, the rest of what it reads, are Actions' own.

## Why a failing verdict of a group behind the front waits for the front

On attempt 1 of a merge-group run whose verdict step failed, this step, right after the
verdict, keeps the job open until the group is the front of the queue: until the queue's
branch is at `github.event.merge_group.base_sha`, the group's parent commit. The job's own check
run is the ci-ok the queue reads, and it stays in progress while the step waits, so the queue
still counts the group as building. A failing ci-ok on a group behind the front decides nothing
sooner — the queue takes a failed entry out only once every group in front of it has ended —
and makes the queue build the next entry at once, on top of the failed one, a group the rebuild
then throws away. .github/scripts/ci-ok-hold.sh's header holds the measurement and says when the
step stops sooner: once the queue no longer holds the group (its gh-readonly-queue branch is
gone or moved), on a read GitHub refuses, which prefers the failing verdict as every refusal of
the ci-ok steps does, and when the group is CI_OK_HOLD_AGE_SECONDS, 170 minutes, old, counted
from its run's creation, so the job concludes inside the queue's own 180-minute check timeout.
It reads both refs with `git ls-remote` every 30 seconds, which spends none of the
installation's REST API budget; its one REST read, the run's creation time, waits for no
budget reset.

It keys on the verdict step's own outcome (`steps.verdict.outcome`), not on `failure()`, so a
passing verdict never waits whatever a later step does; `!cancelled()` lets it run after that
failed step. The refusal of a re-run attempt is the verdict of a tree the queue no longer waits
on, so the step skips any attempt above the first. The reconcile step after it is skipped in
exactly the case it runs, so it is the job's last step there and the job's check run concludes
the moment it ends; that reconcile would only set a posted ci-ok of this head to failure, which
every ci-ok a report step posts already is. Its own timeout-minutes, 175, lets a run created
moments before it hold for the whole age and its last read; the verdict and reconcile steps
carry 10 each, the whole job's limit before this step, so their wait for a spent API budget is
as long as it was.

## The reconcile step

A ci-ok the ci-ok-report step posted stays on the head as it was: no re-run updates a
check run a job created with POST, and it can sit in another workflow's check suite, so a
run that failed and was re-run green would stay blocked by it. This step sets every ci-ok
check run on the head but this job's own to what this job concluded, so one verdict stands
at the end of every attempt (ci-ok-reconcile.sh says what it passes over and why a refusal
only warns). `job.status` is this job's conclusion, and `always()` is what runs the step
after the verdict, the "Judge every job of this run" step, failed. On a dispatch of any ref but
exactly refs/heads/main the step is skipped: that run's verdict is not the head's ci-ok, so it
sets no ci-ok of the head either (the next section says why). On a merge group's first attempt
whose verdict failed it is skipped too: the hold step ends that job, and every ci-ok posted on
that head is failure already.

## A dispatched run's check names

A workflow_dispatch run of a ref other than main tests that ref's own tree, while a pull
request's run tests the merge of the branch with its base. Both put their check runs on the
same commit, the branch's head, which is the pull request's head, so under the plain names that
commit carried two ci-ok check runs with two verdicts over two trees, and a pr-title that the
dispatched run skips. Pull request #3513 at a368687e is the case, read on 2026-11-21: its
pull_request run 44860489650, over the merge with main, failed in dispatch-browser / binaries,
dispatch-ui and docs / build, and its ci-ok check run 136722779787 concluded failure at
03:58:53Z; workflow_dispatch run 44860519340 of the branch passed, and its ci-ok check run
136723497213 concluded success at 04:02:35Z; the same run posted pr-title check run
136712596880, skipped. GitHub's own reading of the pull request did not take the dispatched
run's: the commit's statusCheckRollup held 74 contexts, 73 check runs of pull_request runs and
one of CodeQL, and no check run of the dispatched run, ci-ok failure among them. That is
GitHub's documented rule (docs.github.com, "Troubleshooting
required status checks", "Checks from some workflow jobs are not evaluated": the checks of a
workflow_dispatch run's jobs do not appear in a pull request's checks and satisfy no required
check). A reader that takes the commit's check runs by name did:
`commits/<sha>/check-runs?check_name=ci-ok` lists both, the dispatched run's first, and a
record that keeps one check run per name and lets the higher id replace the lower, as
packages/envoy/internal/cistore's does (cistore.go's check-run update), would hold the
dispatched run's success as the head's ci-ok, and its skipped pr-title as the head's pr-title.

So on such a run the `ci-ok` job is named `ci-ok (dispatch)`, and the reconcile step, "Set every
other ci-ok on this head to this job's conclusion", is skipped, since every ci-ok that step could
set is the pull request's or the queue's. GitHub evaluates a job's name that reads a context only
when the job runs (a name of string literals alone it evaluates on a skipped job too, as the last
paragraph of this section shows).
ci-ok runs on every event, so it is `ci-ok` on a pull request, a merge group and the bypass
check's dispatch on main, and GitHub evaluates its name when the job is queued, after its needs:
on scratch run 44872165102 the queued check run 136750697832 was already named ci-ok. pr-title
runs only on a pull request that is not a draft and on a merge group, and is named pr-title
there. Every run that skips it, a dispatch of any ref, main's included, and a draft's run,
leaves its name unevaluated, and its check run carries the text of the expression,
`github.event_name == 'workflow_dispatch' && … || 'pr-title'`, with the conclusion skipped:
check run 136745386648 of scratch dispatch run 44871030917, and check run 136755360755 of draft
scratch run 44874260916, whose own ci-ok, 136755360774, concluded failure (read 2026-11-21).
Either way it is not named pr-title, so a draft's head carries no pr-title from this file, and
marking the pull request ready starts the run that checks it. (pr-title.yaml, which runs on an
edit of a pull request, draft or not, still posts its own.) pr-title needs no job, so its name
can read nothing but the event: `${{ github.event_name == 'workflow_dispatch' && 'pr-title
(dispatch)' || 'pr-title' }}`. Its dispatch arm is never evaluated, since the job runs on no
dispatch, and what keeps a skipped pr-title from posting a pass under that name is that its name
is an expression that reads a context.

Which dispatch is of main is read by a shell test, not by an expression. GitHub's expression
`==` and `!=` ignore case, so `github.ref != 'refs/heads/main'` is false for the branches
refs/heads/Main and refs/heads/MAIN, which git accepts (`git check-ref-format` passes both),
and `github.ref_name != 'main'` is false for those and for a tag named main too: a dispatch of
any of them would report ci-ok and run the reconcile step. The changes job's step "Read whether
this run's ref is exactly refs/heads/main" runs `[ "$GITHUB_REF" = refs/heads/main ]`, whose `=`
is case-sensitive, and writes is_main=true for exactly that ref and is_main=false for every
other; the job hands it on as the output `ref_is_main`. ci-ok's name is `${{ github.event_name ==
'workflow_dispatch' && needs.changes.outputs.ref_is_main != 'true' && 'ci-ok (dispatch)' ||
'ci-ok' }}` and the reconcile step's if: is `${{ always() && (github.event_name !=
'workflow_dispatch' || needs.changes.outputs.ref_is_main == 'true') && !(steps.verdict.outcome
== 'failure' && github.event_name == 'merge_group' && github.run_attempt == '1') }}`, whose last
term skips it where the hold step ends the job (#the-reconcile-step). The comparisons left are of
fixed names and of strings Actions or the step wrote, which case cannot change. A dispatch
whose changes job failed or was cancelled wrote no output, and reads as a dispatch of another
ref: it is named `ci-ok (dispatch)` and skips the reconcile step, main's included. Evaluated with
GitHub's expression library (@actions/expressions 0.3.61) and the step run by bash for each ref
(2026-11-21): the expression form read Main, MAIN and Main with changes failed as main, and the
shell form reads only exactly refs/heads/main as main.

The dispatched run still reaches its verdict and shows it on the head under its own name, so a
reader by name finds one ci-ok and one pr-title on a pull request's head, and they are the pull
request's own. Only a pull_request run, a merge-group run and the bypass check's dispatch on
main report ci-ok, and only the first two report pr-title: they are the runs the ruleset and the
queue read, and the bypass check's run posts its verdict on main's tip, which is no pull
request's head and where a person reading that commit looks for it. The name is chosen by the
event and the ref of the run alone, so no run has to read whether another run exists, and
nothing races. check-ci-layout.sh holds both jobs to their one `name:` (ci_layout.py's
DISPATCH_NAMES), the reconcile step to its one `if:` (RECONCILE_IF), the changes job to the
output and the exact step (REF_OUTPUT, REF_STEP_RUN), and every other job to a name that can
evaluate to neither check.

An expression of string literals alone is not enough: GitHub evaluates it even for a job it
skips, so the skipped record carries the name itself. Pull request run 45440587594 wrote check
run 138611450495, skipped, as `kind / image`, from kind-image's `${{ 'kind / image' }}`, newer
than dispatch run 45440563929's success 138610991474 under that name; that dispatch run wrote
skipped `worker-image` (138610455464) and `dispatch-browser-pr` (138610457467) from names of the
same form. A name that reads a context stays unevaluated: pr-title's left its expression's text
(check run 138610203753), and kind's legs left `kind / ${{ matrix.leg }}` (138611450560). So each
job a run can skip, in this file or in a workflow it calls, whose name is one string writes it
`${{ github.event_name && '<name>' }}`: github.event_name is a non-empty string on every event
(the caller's, in a called workflow), so the job's check run carries `<name>` when it runs, and
its skipped record the text `github.event_name && '<name>'`. check-ci-layout.sh's rule 25 refuses
a job a run can skip whose name is no expression or one that reads no context, and ci-ok.jq reads
the guarded form as the string it gives.

## A lane label turns on one held job on a pull request

The held jobs - go-daemon, go-envoy-held, envoy-held, scripts-held, pi-held, web-cold,
dispatch-browser's full lane, agent-harness and airlock-arm - run in the merge queue, and on a pull
request only where the label `full-ci` or one of the job's own lane labels names them, and where
the job's filter selects the change. Each label is a term of the job's output of the changes job,
`contains(github.event.pull_request.labels.*.name, 'lane:<name>')`, beside the `full-ci` term, so a
label never turns on an output whose filter does not select the change, and a label that names no
held job turns nothing on. main's labels keep their names (cybermonklabs/legion#4928 and #5175):
each lifts the hold of the job of this layout that runs its suite, with that job's other suites, so
`lane:envoy_go_race` runs go-envoy-held whole where on main it ran one suite. The held jobs that
run no suite of main take a label of their own: `lane:agent_harness`, `lane:airlock_arm` (the arm
job of airlock-check.sh k9) and `lane:web_cold`. ci-ok's line for a held job that did not run names
the labels that would have run it, read off the same terms (.github/scripts/ci-ok.jq,
hold_labels), so the two lists cannot differ; .github/scripts/ci-router-outputs.test.sh holds each
label to its job, one case a label. kind runs on no pull request and takes no label.

## The web-cold job

The held part of the ts job: the Dispatch dashboard's web tests once more with each lazy load held
back (packages/dispatch's `test:web:cold`). With it the dispatch-ui leg took 10.1 minutes in CI
run 45292147224, past the 10 minutes a pull request's job is held to, so it is a held job of
its own, one per job a held part comes from, with the label `lane:web_cold`
(.github/actions/ts/web-cold).

## The envoy-held job and its legs

go-envoy's held part, a held job of its own: main's queue halves of envoy-go-checks (envoy's script
tests, the load-test cluster's test with helm) and of envoy-infra-go (the natslive tests), the proof
editor's parity fuzz, and the proofs whose pull-request leg passed 10 minutes with them:
envoy-session-stall's claimant delivery, verification results and session title
(envoy-session-proofs; that leg took 16.2 minutes with them in CI run 45292147224) and
envoy-e2e-local's bucket refill (envoy-e2e-refill; 10.8 minutes in that run). The labels
`lane:envoy_go_checks` and `lane:envoy_infra_go` lift the job whole. Its legs keep the runners
of the legs they came from; the session proofs' leg takes the job's Postgres, sized for its
Dispatch processes.

envoy-session-proofs also runs the four proofs that took envoy-session-stall to 14.35 minutes in
CI run 45440563929 (#the-session-stall-suites-the-session-state-rig-and-the-live-dispatch-stack):
  * The session state rig's second part and its third part. Each is a step of its own that runs
    `bun test ./test/session-stall.ts -t "^<the part's describe name> "`.
  * The approved-version bytes of the real CLI against a real Dispatch.
  * The read-only token of the dev broker against a local Dispatch.
The rig's third part runs the pinned Oh My Pi and Claude Code, so this leg installs jj, Oh My Pi
and Claude Code, and stays on ubuntu-24.04.

Its last step is the role takeover smoke with the listener built from the base of the change and
every client built from this tree (`smoke-role-takeover.sh --base-rev`). It fails when the head
breaks a client that main ships. .github/scripts/fetch-compared-base.sh resolves the base from the
run: a merge group's base_sha, a pull request's base.sha, or main's head when a dispatched run
started. It fetches that one commit with its tree, at depth 1, into
refs/remotes/origin/compared-base. `jj git init --colocate` then makes the checkout a jj
repository that holds the commit, and the smoke gets it by its id and fetches nothing. A run in a
copy of the checkout of one commit, with no remote to fetch from, passed all 18 rows against the
listener of main's c227d04661 (2026-11-25).

Times, from CI run 45440563929 on ubuntu-24.04, added to this leg's 4.1 minutes there:
  * The rig's two parts: 2.71 minutes. The whole file took 4.09, and its first part 1.38 on main.
  * The approved-version bytes 0.12, the read-only token 1.95, and the three installs 0.12.
  * The base's fetch and the jj repository: under 0.1 each. The checkout's own fetch of one
    commit took 2 s. `jj git init --colocate` took 3.5 s in a gVisor pod.
  * The smoke against the base's listener: about 2.3. The smoke took 1.83 against this tree's
    listener, whose build hit the cache. The base's listener built in 22 s in a gVisor pod.
So the leg is expected to take about 11.5 minutes, under its 20.

## Why every job sets JOB_TIMEOUT_MINUTES

Each job and leg sets `JOB_TIMEOUT_MINUTES` in its `env:` to its `timeout-minutes` expression
exactly as written (`${{ matrix.timeout }}` for a matrix job), and each timed step that reaches
.github/scripts/gh-read.sh sets `STEP_TIMEOUT_MINUTES` to its own: gh-read.sh waits for a spent API
budget within those two numbers, measured from the job's start and the step's, and without them it
fails by name rather than wait (main's #4893). The ci-ok-report step every job ends with, and each
carrier's leg-verdict.sh, reach gh-read.sh. check-workflow-files.sh's rule 8 holds the two equal.
ci-ok's 188 minutes are its verdict step's 10 and its hold step's 175 (the reconcile step's 10
runs only when the hold does not), with three over for the checkout and the two refusals. The
hold's 175 are the age's 170 minutes (CI_OK_HOLD_AGE_SECONDS, 10,200, inside the queue's 180) and
the last pause and read's ladder, two minutes; CI_OK_HOLD_RUN_SECONDS, 3,330, is the longest
merge-group run of 2026-11-22 (44966798236).
