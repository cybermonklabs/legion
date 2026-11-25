---
title: "Join a Job to the CI Router: A Gate That Names What the Job Reads"
category: github
tags:
  - github-actions
  - path-filters
  - ci-router
  - caching
  - concurrency
  - merge-queue
date: 2026-10-27
status: active
module: ci
related_issues:
  - "INFRA-520"
symptoms:
  - "a change merged without a job that reads one of its files, and nothing re-ran the job"
  - "a new job never runs on a pull request, or runs on every pull request"
  - "ci-ok never appears on a pull request although every job finished"
  - "ci-ok stays failed after a re-run of failed jobs left a job changes selected skipped"
  - "ci-ok fails saying it does not know what selects a job"
  - "ci-ok fails on a draft pull request, saying to mark it ready for review"
  - "a pull request run writes an Actions cache entry"
---

# Join a Job to the CI Router

`.github/workflows/ci.yaml` (name `CI`) is the one workflow every pull request and every
merge-queue group runs, and the bypass check dispatches. It has no `paths:`. Its first job,
`changes`, computes one dorny/paths-filter filter per job from what each job reads, and selects
jobs by the paths the change touched. `ci-ok`
condenses every job's result into one check, and it is a job: the `ci-ok` job needs every other
job and fails when the verdict over their results is failure, so its own check run carries that
verdict (the section on `ci-ok` below). The deployment repository keeps its own router guide for
its own workflows; this page covers this repository only.

## The seven places a job joins

1. **A gate in `.github/ci-gates/<job id>.py`** that says what the job reads (the section below);
   the `changes` job computes the job's filter from it.
2. **An output of `changes`** for that filter.
   Every output carries the dispatched term, since a dispatched run skips the filter step and selects every job:
   `<filter>: ${{ github.event_name == 'workflow_dispatch' && 'true' || steps.filter.outputs.<filter> }}`.
   A slow suite carries the `full-ci` term too, with its own `lane:<filter>` label beside it (step 5 says why), and ends with the `'false'` that makes the skip `changes`' own:
   `<filter>: ${{ github.event_name == 'workflow_dispatch' && 'true' || (github.event_name != 'pull_request' || contains(github.event.pull_request.labels.*.name, 'full-ci') || contains(github.event.pull_request.labels.*.name, 'lane:<filter>')) && steps.filter.outputs.<filter> || 'false' }}`.
   Which jobs are slow suites is `.github/ci-notes/ci.md`'s rule ("Which lane a job runs in: the pull request or the merge queue"): a test job above 10 minutes at the 90th percentile of its pull-request runs. A build, vet, format, typecheck or lint check runs on every pull request whatever its time, so it never takes this term. No job runs in part: a job that mixes such checks with slow tests is split in two, the checks in a job of their own that a pull request runs and the tests in the job that keeps the old id and takes this term, so each runs all its steps wherever it runs (step 4 says where the second job's steps go).
3. **`needs: changes`** on the job, and
   `if: needs.changes.outputs.<filter> == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')`.
   The second term holds the job back on a re-run attempt of a merge-group run, where the group is tested already.
   Every job of `ci.yaml` but the two named for a required check carries a term of its own for that and `check-ci-filters.sh` fails one that does not:
   `gh run rerun --failed` leaves out a `changes` that succeeded and hands the attempt that job's outputs again, so no job may lean on `changes`' skip for it.
   `ci-ok` and `pr-title` must carry neither term and are refused one, since a skipped job's check run is a passing required check: each runs on every attempt and fails one above the first in a step of its own.
4. **The `ci-ok-report` step as the job's last step**, the same text in every job:

   ```yaml
   - name: Report ci-ok when this job decides a merge-group run
     if: always()
     uses: ./.github/actions/ci-ok-report
     with:
       changes: ${{ toJSON(needs.changes) }}
       job-status: ${{ job.status }}
   ```

   A job that calls a workflow passes it `ci-ok-changes: ${{ toJSON(needs.changes) }}`, and that
   workflow's job ends with the step under `job: <the calling job's id>` (docs-site.yaml and
   worker-image.yaml do). A job with its own `permissions:` block repeats `actions: read` and
   `checks: write`, which the step needs. It posts nothing but the failing `ci-ok` of a
   merge-group run this job's own result decides, so that a doomed group leaves the queue at the
   first failure; a job without it leaves that group waiting for every other job of the run.

   `ci.yaml` calls 45 unique reusable workflows, as many as `check-workflow-files.sh` allows, so
   a job split off another calls the file it was split from rather than a file of its own
   (`.github/ci-notes/ci.md`, "Two jobs that call one file: the reusable-workflow limit"). The
   file takes a `job` input, each of its two jobs carries exactly
   `if: inputs.job == '<its id>'` and ends with the step under `job: <its id>`, and each caller
   passes `job: <its own id>`; `ci_filter_coverage.py` refuses any other shape for a file two
   jobs call, and `ci_job_reads.py`'s `SHARED_FILE` maps the new job to that file.
5. **The job in one of `ci-ok`'s three classes**, all three in ci.yaml's `env:`, and in exactly
   one of them:

   - **`CI_OK_SELECTED_BY`**, mapped to the output its `if:` tests (`"<job>": "<filter>"`), and
     in `CI_OK_PULL_REQUEST_ONLY` too when its `if:` also admits only
     `github.event_name == 'pull_request'`. This is where a job gated by a `changes` output
     goes. It costs the job nothing: `ci-ok` passes when `changes` did not select it and it was
     skipped, and fails when it was selected and did not succeed.
   - **`CI_OK_ALWAYS_RUN`**, for a job no `changes` output gates, which therefore runs on every
     change (`changes` and `lint` are the two). It costs every change the job's whole run time,
     and `ci-ok` passes only when the job succeeded: a skip, a cancellation or an absence from
     the verdict's `needs:` fails the check. Put a job here only when every change must run it;
     anything a filter can select belongs in the first class.
   - **`CI_OK_EXEMPT`**, for a job outside `ci-ok`'s accounting altogether (`cancel-started`,
     `ci-ok`, `pr-title`). It costs the job its verdict: nothing judges its result, so it must
     carry no report step and stay out of the `ci-ok` job's `needs:`, and only a job whose
     result is read elsewhere — its own required check, or nothing at all — may go here.

   A job an event must hold back gets its `changes` output
   set false on that event instead of an `if:` that skips it, so `ci-ok` reads the skip as
   unselected: that is how the slow suites are held back on a pull request that carries neither
   the label `full-ci` nor the job's own `lane:<filter>` (step 2's term). `ci-ok` names such a job
   as not run at this head, with the label that would run it. `ci-ok` fails a job in none of the three classes, naming all three remedies, and
   `check-ci-filters.sh` fails when the map or a list is not exactly what the `if:`s test, when a
   job is in none of the classes or in two, when a job `CI_OK_ALWAYS_RUN` names is gated by a
   `changes` output after all, or when a
   job outside `CI_OK_EXEMPT` lacks the report step (the section on `ci-ok` below says why).
6. **The job in the `ci-ok` job's `needs:`**, which is every job of the file but `ci-ok` itself
   and those `CI_OK_EXEMPT` names. A job outside that list ends after a verdict that says nothing
   about it, so a red job would merge; `check-ci-filters.sh` fails naming it.
7. **`//ci:` lines in the job's tests** for what a test does outside its package (the section on
   the check below), where a test of the job reads such a file or starts a go command.

A Go job also restores its caches the way the section on caches below says.

## ci-ok is a job of the router, and fails a selected job that did not succeed

`ci-ok` is the last job of `ci.yaml`. It needs every job of the file but itself and those
`CI_OK_EXEMPT` names, runs under `always()` so that a failed, skipped or cancelled job still
reaches a verdict, and fails when `.github/scripts/ci-ok.sh`'s verdict over `toJSON(needs)` is
failure, so GitHub's own check run for the job is the `ci-ok` that main's ruleset reads. It is a job
because a check run a job creates with `POST /repos/.../check-runs` under `GITHUB_TOKEN` joins
the Actions App's **first check suite at that commit**, not the posting run's: measured on pull
request #2860 at 57182d42, where CI's `ci-ok` landed in the suite of "Rebuild Claude Envoy
plugin" (114301219205) rather than CI's own (114301219822), and on #2847, where it landed in PR
Title's suite and the merge queue refused to enqueue with `Required status check "ci-ok" is
expected` while that `ci-ok` was green. A job's own check run is in its run's own suite by
construction. The runner it waits for is this shape's cost, and the figure first written here is
retracted: a median of 8 seconds over the 25 newest completed `pull_request` runs measured each
run's longest wait among jobs that are created with the run and queue together, which answers
another question.
A job gated on every other job is created then too, but starts only once the last of them ends,
which is when the pool is busiest with the next wave of runs.
Three readings of that wait have been taken for dispatch://INFRA-1066, and they do not agree:
26 minutes 37 seconds on the k27 probe of 2026-11-06, whose `ci-ok` job was created at 10:16:21Z
with every other job of its run already finished; 4 minutes 3 seconds on the tester's run of the
same issue; and about 50 minutes on this change's own head.
So the cost is a distribution rather than a number, and what sets it is how busy the account's
queue is at the moment the last of the verdict job's needs ends.
The verdict stays a job regardless, because a late verdict is right while a verdict filed in
another workflow's suite blocks the head until a person re-runs it.
Those three readings were taken while the job ran on `ubuntu-24.04`. It runs on
`ubuntu-24.04-arm` now, with `changes`, `pr-title` and `cancel-started`. That pool queues apart
from the x86_64 one, and by the time the last need ends the run's other arm64 jobs have long
started, so the wait is short: 14 s on the run the "Choose the runner" section below measures.

Two things still run outside that job. The `ci-ok-report` step every other job ends with posts
the failing `ci-ok` of a merge-group run that job's own result decides — its failure or its
cancellation, or a `changes` that failed or was cancelled — the moment that job ends
(`.github/scripts/ci-ok-report.sh`), because the `ci-ok` job waits for every job of the run and a
group that can no longer merge should leave the queue at the first failure. It posts only for the
queue's front group, whose base is the tip of main: the queue takes a group behind the front out
only once the groups in front of it end, and an early failing `ci-ok` there makes the queue build
another group on it, so that group's verdict waits for the `ci-ok` job, which holds a failing
verdict until the group is the front (`.github/scripts/ci-ok-hold.sh`) for the same reason.
Every check run it posts carries an `external_id` of `ci-ok-post:<that run's id>`. And the
`ci-ok` job's reconcile step (`.github/scripts/ci-ok-reconcile.sh`), its last on every run but a
merge group's first attempt whose verdict failed, where the hold ends the job, sets to what the
job concluded each `ci-ok`
check run on the head that is a post: one carrying that marker, or one with an empty
`external_id`, which the release before the marker posted. A posted check run is updated by no
re-run and can sit in another workflow's suite, so a run that was posted a failing `ci-ok` and
then re-run green would stay blocked by it.
A `ci-ok` whose `external_id` is a UUID is a job's own check run, possibly another workflow
run's — the bypass check dispatches `ci.yaml` on main's tip, which carries its merge group's
green `ci-ok` — and GitHub refuses a PATCH of one under `GITHUB_TOKEN` with "Check run status
and conclusions can only be updated internally by GitHub Actions" (measured on INFRA-1066's k28
probe, 2026-11-06), so the step leaves it alone rather than spend its retry ladder on a call
that cannot succeed; and a re-run of the same job supersedes its earlier attempt by itself,
since `statusCheckRollup` and the default `latest` filter read the newest alone.

The verdict is `.github/scripts/ci-ok.sh`'s. It passes only when `changes` succeeded, every job
`CI_OK_ALWAYS_RUN` names succeeded (each of those runs on every change), every job `changes`
selected succeeded, and every job it
did not select succeeded or was skipped. ci-images is one of the selected jobs: the `ci_images`
filter selects it, and `CI_OK_SELECTED_BY` maps it. A selected job that failed, was cancelled or
timed out fails it, and it prints every job's result and whether `changes` selected it. The
error says which of the three happened, because they are not one fault: a skip is the job's
`if:` and its filter disagreeing, which the author fixes in one of the two; a cancellation is
the job cut off — its own `timeout-minutes` (a job its limit cut off reports `cancelled`,
measured on probe run 41869007039), a cancelled run or a force-cancel — and sends the
author to the job's log and its limit, not to its filter; a failure is the job's own, and its
log says what broke.

A skipped job reports success, so accepting any skip is not enough. When `changes` fails, every
job its filters gate is skipped. GitHub's "Re-run failed jobs" then re-runs `changes`, and
`changes` can succeed while the jobs it now selects stay skipped. So `ci-ok.sh` decides what
`changes` selected from its outputs, through `CI_OK_SELECTED_BY` (one reading, in
`.github/scripts/ci-ok.jq`): a job is selected when the output it maps to is `'true'`, and for a
job in `CI_OK_PULL_REQUEST_ONLY` the run must also be a pull request's (`worker-image`, since
main's image builds are release.yaml's, and `dispatch-browser-pr`, whose rows the queue's full
lane runs). Re-run the whole workflow instead, on a
`pull_request` run. A merge-group run is never re-run: on a re-run attempt of one every job
skips but `ci-ok` and `pr-title`, which run and fail in a step, so the pull request is tested
again when it is queued again.

`check-ci-filters.sh` fails when `CI_OK_SELECTED_BY` is not exactly {job: the output its `if:`
tests} for every job whose `if:` tests an output of `changes`, when `CI_OK_PULL_REQUEST_ONLY` is
not exactly the jobs whose `if:` restricts to `github.event_name == 'pull_request'`, when a job
of the file is in none of the three classes or in two of them, when a job `CI_OK_ALWAYS_RUN`
names is gated by a `changes` output after all, when a job
but `changes` and those `CI_OK_EXEMPT` names does not end with the report step, when a job
`CI_OK_EXEMPT` names does carry it, when a job other than the one of that name is named `ci-ok`
or `pr-title`, which would post a required check itself (a skipped job posts a pass), when the
`ci-ok` job is missing, is outside `CI_OK_EXEMPT`, or does not need exactly every other
non-exempt job, and when the `pr-title` job is missing. A wrong entry would fail a job its `if:`
rightly skipped, pass one a re-run left skipped, or leave a job out of the verdict altogether; a
job in no class is one the verdict fails on every run, whatever the job did.

A fork pull request's run gets a read-only token, which can post no check run, and neither
required check needs one: `ci-ok` and `pr-title` are jobs of ci.yaml, a job's own check run costs
no token scope, so a fork's head carries both. What a fork cannot get is the early posted `ci-ok`
of a doomed merge-group run, which no fork is ever in: this repository takes no fork pull
requests, whose runs wait for an approval nobody gives.

A workflow whose own job is named for a required check cancels no run of its own, for the same
reason: a cancelled check run of a required name is no pass either, so a run a newer one cancels
leaves that shape on the head until the next event replaces it.
Measured on #2771 at `9a2a3f87`, where four edits and review replies arriving while PR Title
runs were in flight left four cancelled `pr-title` check runs beside the two that succeeded.
pr-title.yaml is the one such workflow, and its group therefore carries
`cancel-in-progress: false`; its runs are seconds long, so two of them finishing costs a runner
for those seconds and leaves two passing check runs.

A job whose main half must run on a push to main, as the docs site's Pages deploy must, cannot be
a job of the router: the router runs on no push. Its main half is a push-only workflow of its own
with a workflow-level group keyed on the ref (`cancel-in-progress: false`, and no `queue:` key,
so of the pushes waiting only the newest builds), whose `on.push.paths` equals the job's filter,
and `ci_job_reads.py`'s `PUSH_WORKFLOWS` pairs the filter with that workflow, so
`check-ci-filters.sh` fails naming each pattern one list has and the other lacks.
docs-pages.yaml is the one such workflow.

## What a gate declares

A gate is one file of `.github/ci-gates`, `GATE = gate("<filter>", "<job id>", ...)`, run in the
namespace of `.github/scripts/ci_job_reads.py`, which keeps the helpers every gate shares
(`reads`, `step_run`, `script_run`, `pinned_step`, `setup_go`, `bun_job`, `at`). The changes job
computes the job's filter from it when it runs (`.github/scripts/ci-filters-generate.sh`,
`check-ci-filters.sh generate`): no file of the repository holds the filters.

For a Go job, name its go commands, each a `Run` pinned to its text: `step_run` for a `run:` step
of the job, `script_run` for a script the job runs, pinned to the script's whole line. The filter
then takes, from `go list -deps -test`, every first-party directory the commands compile (its own
files, its `testdata/**` where the run tests it, every file it embeds), the closure of each go
command a compiled test starts, and the workspace's module files.

For every job, declare its other reads in the gate's `reads=`:

- the composite actions it uses (`.github/actions/<name>/**`) and what they read, but for the
  `ci-ok-report` step every job ends with;
- the Bun install set when the job runs `bun install` (`bun_job`);
- a file a step reads, a script it runs, a fixture outside a package.

A gate reads `ci.yaml`, its own `ci-job-<id>.yaml` and its own gate file without saying so. A
glob a gate declares goes into the filter as written, so it also selects a file a change deletes
under it; `Read(glob, why, but=("**/*_test.go",))` leaves the Go test files under it to go list,
which says which of them the job compiles.

What a test does outside its own package is declared where the test is, in a block of `//ci:`
lines at the top of the test file, right after its package clause:

```go
package api

//ci:reads packages/contracts/fixtures/delivery/*.json TestDelivery reads the contract fixtures
//ci:builds packages/envoy ./cmd/dispatch TestCLI starts a go build of the dispatch command
```

`//ci:reads <glob> [!<glob>...] <why>`, `//ci:reads-set <name> <why>` (a set `READ_SETS` in
ci_job_reads.py computes) and `//ci:builds <directory> <package>... [NAME=value...] <why>`. A line
counts for a job only when go list says the job compiles that test file under its tags. Add the
line in the change that makes the test do it. Two pull requests that add reads of different tests
edit different files, where they used to edit one shared list.

For a job whose steps typecheck TypeScript, the program is derived rather than listed:
`.github/scripts/check-ts-filters.sh` runs `bunx tsc -p <project> --noEmit --listFilesOnly` for
each pair of a filter and a project (`ci_ts_programs.py`) and holds every file of the program to
the filter the changes job computed, so an import that reaches a new package fails on the pull
request that writes it. Add the pair when a job gains a typecheck; the files need no list. It
runs in `ci.yaml`'s lint job, which installs the dependencies and reads the run's computed filters.

What a job reaches by **spawning** a program is in neither derivation: a spawn carries no import,
so neither `go list` nor `tsc` sees it, and the reach is a declared read.
`packages/pi-legion`'s `src/skills-guard.test.ts` spawns `packages/claude-envoy/scripts/build.ts`
and asserts the tree it writes, which is why the `pi_legion` gate declares that package.

`worker_image` and `browser_image` are kept by hand, each an `EXCLUDED = excluded(...)` file of
`.github/ci-gates`: after the roots the image build reads come one `!` pattern for each thing the
`.dockerignore` keeps out of the build context inside those roots, and
`.github/scripts/check-image-trigger-paths.sh` proves each covers every file its build reads.

## When the changes job cannot compute the filters

`ci-filters-generate.sh` writes, when the computation fails for any reason, a file in which every
filter is `'**'`, under a first line `# FAIL-OPEN:` with the reason, and exits 0: every job runs.
The lint job's two readers of the filters refuse that file, so the run is red; the ci-filters job
fails too when the cause is the engine's own, but not for a Bun install or a TypeScript listing that
failed, which only the changes job runs. A change to the engine or the shared read table is held
against the base tree's filters: each filter it alters selects every path in that run.

A branch made before the gates and the `//ci:` lines moved keeps the old `RULES` and `GATES` in
`ci_job_reads.py`, and `.github/ci-filters.yaml`, when it merges main; both are refused by name.
`.github/scripts/ci-reads-convert`, run after that merge stops, moves what the branch added into the
gate files and the `//ci:` lines and deletes the old file.

## check-ci-filters.sh holds the table to ci.yaml

The `ci-filters` job runs `.github/scripts/check-ci-filters.sh`, which computes every filter as the
changes job does and fails when the table it computes them from drifts from `ci.yaml`. The check
fails when a job's `run:` step starts a go command (`go test`, `vet`, `build`, `run`, `list`,
`install`, `generate`) no gate claims. It reduces a claimed command only in its simple form: one
line of `go test|vet|build`, relative package paths and only `-v`, `-race`, `-count=N`, `-timeout
D`, `-buildvcs=false` and one `-tags T`, that is its step's whole `run:` script, with no `shell:`,
no `defaults.run.shell`, no `container:`, no `env:` name outside `ENV_ALLOWED`, no step of the job
writing `$GITHUB_ENV` or `$GITHUB_PATH` or running `go env -w`, and the step's working directory
the `Run`'s; its packages, tags, `-race` and mode must then be exactly the `Run`'s. Any other
claimed command counts as reading every Go file of the repository, every file under a Go module's
directory and every module file, so its filter takes them all, and the check's report says which
command and why. It fails wide, never narrow.

A step whose go commands are in no simple form (a loop, a pipe, `GOOS=... go build`) gets an
exact entry instead of `step_run`s: `pinned_step(job, step, workdir, script, runs)` in the gate's
`exact=[...]`, with `script` the step's whole `run:` text, and one `runs` item per go command the
script runs. A step off its pin is refused, naming the line that differs.

It fails too when a pinned text or an exact entry's step is gone, when a job or step is gated on a
filter no gate declares, when a read a gate or a `//ci:` line declares matches no file, and when a
`//ci:` line cannot be read. It runs go with `GOPROXY=off` over an empty module cache. Run it
locally from anywhere: `.github/scripts/check-ci-filters.sh` (its header gives the recipe for a
machine whose go is a bootstrap).

## Caches: no job of the router writes one

No step of a `ci.yaml` job saves an Actions cache entry; three actions its jobs call can, in a
dispatched run alone (setup-bun on main, architecture-check, spec-check). dispatch-two-builds
saved its two earlier Dispatch builds in its runs' own refs until 2026-11-23, when 25 such entries
held 12.2 GB of the repository's 25 GB and no later run read most of them; ci-caches.yaml's
`dispatch-builds` job now fills them on main. A pull request's entries are scoped to its ref,
no other pull request reads them, and they count against the repository's cache limit, where they
evict main's entries; a merge group's entries are no more readable than those. Only an event that
writes the default branch's scope — a push to main — leaves an entry every later run can read, so
the fills live in workflows that run there.

- Give `actions/setup-go` `cache: false` and `id: setup-go`.
- Restore with `actions/cache/restore`, keyed on the runner's architecture and the Go version
  setup-go installed
  (`key: <family>-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}`),
  with the family's prefix as `restore-keys`
  (`restore-keys: ${{ format('<family>-{0}-{1}-', runner.os, runner.arch) }}`; the families are
  `go-daemon`, `go-docs`, `go-envoy`, `go-envoy-test`, `go-envoy-darwin-vet` and `go-envoy-infra`,
  and `go-notices` in the order the item below gives; `go-envoy-darwin-vet` restores into the
  build cache directory its step's `GOCACHE` names, not into `~/.cache/go-build`). In
  `ci.yaml` that fallback applies on every run, a dispatched one on main included, because no job
  there saves. A job that does save keeps the prefix off main alone
  (`restore-keys: ${{ github.ref != 'refs/heads/main' && format('<family>-{0}-{1}-', runner.os, runner.arch) || '' }}`),
  so the entry it saves after a Go bump holds nothing the older Go built; docs-site.yaml's build,
  which docs-pages.yaml calls on main, is the one such job. The architecture is in the key because
  a build cache holds the objects of the architecture that compiled them: without it, a job on
  `ubuntu-24.04-arm` restores an entry an x86_64 runner filled and compiles every object again.
  Each family is kept per runner architecture and, but for `go-envoy-infra` and `go-docs`, filled
  again on every push to main, so a job restores the entry of the last fill that succeeded. Do not key
  on go.sum: every go.sum change would leave a whole entry, up to a gigabyte, for seven days. A
  go.sum change downloads only the changed modules, and Go's build cache is content-addressed, so a
  fill compiles only what changed. Share a family only with the jobs that build the same
  packages, or whichever job saves first owns the entry the others then miss.
- `go-notices` is the one family keyed with the architecture before the OS
  (`key: go-notices-${{ runner.arch }}-${{ runner.os }}-go${{ steps.setup-go.outputs.go-version }}`,
  `restore-keys: ${{ format('go-notices-{0}-{1}-', runner.arch, runner.os) }}`). Its entry holds
  go-licenses, a program the notices step runs, so an entry one CPU built is of no use to a reader
  on another. A run of a workflow file older than the architecture in the key restores by
  `go-notices-Linux-` on `ubuntu-24.04`, and `go-notices-Linux-ARM64-go1.27.3` starts with that
  prefix: such a run restored main's arm64 go-licenses on x86_64 and failed with `Exec format
  error` (run 44437023306). When the key of a family whose entry holds a program gains a part that
  separates readers who cannot run each other's program (the CPU, the OS), put that part before
  the end of the shortest `key` or `restore-keys` an older reader has, or rename the family:
  GitHub matches both as prefixes, so no `key` or `restore-keys` of an older reader that cannot
  run the new entries may be a prefix of their key. A part that readers can share, such as the Go
  version, may follow the shared prefix: go-licenses sits in a directory that names its own
  version and Go, so a restore of the last Go's entry still gives the modules, and the tool is
  built again at the new Go's path rather than the old one run. Entries saved under the old shape
  still match the older prefixes, and none is deleted by hand: GitHub removes an entry after 7
  days with no read, or earlier, oldest read first, once the repository's caches reach their size
  limit, and each older reader's restore is a read. An older reader that cannot run the entry,
  on the entry's Go and tool versions, then fails on every run until its runs come from a commit
  that carries the new key (a re-run keeps its old commit), or until a program deletes the entry
  the way ci-caches.yaml's `envoy-infra-superseded` job deletes the `go-envoy-infra` entries a
  fill superseded.
- Add no `actions/cache/save` step here, and no `if:` that tests `github.event_name == 'push'` or
  compares `github.ref` with a branch ref: the workflow runs on no push, so such a condition gates
  its step on what no run ever is, and `check-ci-filters.sh` refuses it. A family a new job
  restores is filled by `.github/workflows/ci-caches.yaml`, which runs on a push to main,
  restores the family's newest entry (the job's key, and that key and a `-` as `restore-keys`),
  runs on it what the job builds, and saves it under the key the job restores followed by
  `-${{ github.sha }}`, with the paths the restore names; its `refill-superseded` job then deletes
  every other entry of the family, so it keeps one entry per runner. No entry carries the job's
  key itself, so the job's restore takes the newest entry whose key starts with it.
  `check-ci-filters.sh` holds the
  save to the restore and fails when the two drift. The fill runs on every runner a job restoring
  that family runs on, one matrix leg per runner where there are two. Two families are the
  exception: `go-envoy-infra`, whose key carries a hash of its module files, is filled only when
  that key is missing, and `go-docs` is saved by docs-pages.yaml on that same push, from the build
  job it shares with ci.yaml's docs job.
- The release workflows' Go builds (release.yaml's `legion_build`, release-envoy-infra.yaml's
  `build`) restore `go-daemon` and `go-envoy-infra` and save nothing, so they keep the prefix as
  `restore-keys` on main too: an entry they never save cannot grow.
- `.github/actions/setup-bun` reads and writes its cache only on main; use it as it is.

## Choose the runner: ubuntu-24.04-arm unless the job needs an x86_64 machine

A new job runs on `ubuntu-24.04-arm`. The two hosted pools queue apart, and both queue. On
2026-11-16 a job waited 456 s for an `ubuntu-24.04` runner at the median and 678 s at the 90th
percentile. On the run that first moved this repository's jobs to arm64 (run 44069656523,
2026-11-17), the 24 `ubuntu-24.04-arm` jobs waited 217 s at the median and 542 s at most, against
890 s and 1428 s for the 19 `ubuntu-24.04` ones: an arm64 job created with twenty others waits its
turn too, and the gating jobs (`lint`, `ci-ok`, `changes`, `pr-title`) started in 7 to 23 s. So
the gain is a second pool, and gating jobs that no longer wait behind the x86_64 queue; it is not
zero wait. A job goes on `ubuntu-24.04` only for a reason of the machine,
and ci.yaml's "Runners" paragraph lists each one: a tool with no arm64 row in
`.github/ci-tools.yaml`, the pinned Oh My Pi build (linux-x64 alone), a browser, an image built
for linux/amd64, or a build that must match a release built on `ubuntu-24.04`. A Go build that
sets no `CGO_ENABLED` is that last kind: Go turns cgo on for a native build where gcc is present
and off for a cross build, so the same command links a different pair of binaries on each runner.
That is why envoy-go-build, which packs the release's listener archives that way, stays on
`ubuntu-24.04`. A job that installs a tool on arm64 needs that tool's `<tool>-aarch64` row, which
`check-ci-tools.sh` holds to the version of the `<tool>` row, an install action that picks the
row from `uname -m`, and a case of that row in the tool-fallback-arm job; a Go cache family it
restores needs a fill on that runner (the section above).

## The merge queue runs the router too

ci.yaml triggers on `merge_group` (`checks_requested`). On a group, `changes` selects by the
group's `base_sha..head_sha` diff, what the queued change touches against the queue base, and
`ci-ok` reports on the group's commit, so the required checks pass on the merged tree before
anything lands. A new job needs nothing extra for it. But a job whose `if:` names
`github.event_name` must admit `merge_group`, or it skips on the group while its output selects
it, and `ci-ok` fails the group: a selected job that skipped is a job the verdict cannot accept.
What skips a
group does so on purpose, and two jobs do, each pull request only and so in
`CI_OK_PULL_REQUEST_ONLY`: worker-image (main's image builds are release.yaml's) and
dispatch-browser-pr, the browser suite's pull-request lane, whose full lane the group runs in
dispatch-browser. The queued pull request's
text is checked on the group by ci.yaml's `pr-title` job, which finds it with
find-pr-payload.sh; pr-title.yaml runs on an edit alone, which a group never
carries. The slow suites go the
other way: the queue is where their result normally comes from, and a pull request runs them only
when it carries the label `full-ci`, or `lane:<filter>` for one of them. The
docs job runs on a group and on a pull request alike; docs-pages.yaml builds and publishes main's
site on a push trigger of its own.

Two things a group's run alone carries.
Every job of a re-run attempt skips but `ci-ok` and `pr-title`, which fail such an attempt in a step, so a merge-group run is never re-run: a group is tested once, and the pull request is tested again when it is queued again.
Each of the other jobs carries the term that does it, which step 3 above gives.
And the run of a group the queue has replaced is force-cancelled by a job of the workflow itself — `cancel-started`, which runs only on `merge_group`, reports nothing and is therefore named in `CI_OK_EXEMPT` rather than in `CI_OK_SELECTED_BY` — which cancels the groups of its own pull request the queue built before it.
A group that can no longer merge because the group below it failed is the queue's own to rebuild: a job that fails posts the failing `ci-ok` at once from its own report step, the queue takes that pull request out, and the groups above it are built again.

## Drafts run no test job, and a draft's `ci-ok` fails

`changes` and the jobs it selects skip on a draft pull request, and so do lint, typecheck, test
and ci.yaml's `pr-title`, whose `if:`s carry `!github.event.pull_request.draft` themselves.
`ci-ok` does not: its `if:` is `always()` and nothing else, so on a draft it runs with every job
it needs skipped, and a step of it refuses the draft with one `::error::` line and `exit 1`.
A draft's `ci-ok` is therefore that job's own **failing** check run, and the error names the
remedy: marking the pull request ready for review (`ready_for_review`) starts a run that selects
by the same diff and concludes a real verdict over it.
The refusal is a step and not a term of the `if:` because a job an `if:` holds back is skipped,
and a skipped job's check run is a passing required check: a draft would carry a green `ci-ok`
nothing judged.

A draft's `pr-title` is still a skipped job's check run, which GitHub and the Legion daemon count
as a pass, since ci.yaml's `pr-title` job does carry the draft term. A draft can be neither
queued nor merged, so nothing reads that pass to merge by, and pr-title.yaml checks a draft's
text all the same when it is edited or reviewed.

A job with `needs: changes` and a plain `if:` skips when `changes` skips, so it keeps the rule.
A job whose `if:` starts with a status function — `!cancelled()`, to run when `changes` failed as
lint, typecheck and test do, or `always()` — must also carry `!github.event.pull_request.draft`.
`ci-ok` is the one job that must not, for the reason above.

## A merge group's run is never cancelled by a newer one

A newer push to a pull request cancels its older run. A merge group is a concurrency group of its
own per commit and is never cancelled that way: the queue waits for its `ci-ok`, and a cancelled
run would leave the tree it tests unjudged. A dispatched run keys on the commit it runs at and is
never cancelled either. Only two acts stop such a run, both deliberate: `cancel-started` above,
when a newer group of the same pull request replaces it, and `.github/workflows/dequeued.yaml`,
when the group's pull request leaves the queue. Keep a new job inside the workflow's group; do
not give it a `concurrency:` of its own.
