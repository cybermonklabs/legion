# CI budgets, and the notes behind them

These notes were moved out of `.github/workflows/ci.yaml`.
A workflow file larger than 500 KB triggers no run at all:
GitHub's limit is 500 KB per workflow file, and over it the only signal is a run named by the file's path that concludes `failure` at once with no job in it, so a file that grows past the limit stops testing anything.
The comments that had grown it there were the background ones:
the rules every budget is written to, the histories behind them, and the measurement campaigns they came from.
Each step's and each job's own arithmetic stays beside the step in that file, where a reader judging that budget sees it, and each run of prose that moved left a line there naming the section of this note that now holds it.

In these notes `this file` and `this workflow` mean `.github/workflows/ci.yaml`, where this text was written, and so does a bare `below` or `above` wherever the thing it points at is a step, a job or a comment of that workflow.
A pointer that means a passage of this note says so in its own words - `of this note`, `this section`, `further down` - rather than relying on this paragraph, because a legend read by a hundred pointers is only as true as its least checked one, and this one has been wrong in both directions already.

## The router

The router: every check a pull request or the merge queue runs, in one workflow.
Its first job, `changes`, reads the paths the change touched and selects, through one dorny/paths-filter filter per job, the jobs that read those paths;
each job runs only when its filter matches (lint runs on every change, and typecheck and test also run when `changes` itself failed).
ci-ok condenses every job's result into one check: it fails when a job `changes` selected did not succeed.
Main's ruleset requires ci-ok and pr-title.
ci-ok is the `ci-ok` job at the end of this file, which needs every job of it but itself and those CI_OK_EXEMPT names, and fails when .github/scripts/ci-ok.sh's verdict over their results is failure, so the verdict rides on that job's own check run
— a job's check run is in its run's own check suite, where a check run posted with `POST /repos/.../check-runs` joins the Actions App's first suite at that commit, which is another workflow's as often as not (.github/scripts/post-check-run.sh's header holds the measurement).
Every job but `changes` and those CI_OK_EXEMPT names still ends with the ci-ok-report step (.github/actions/ci-ok-report), which posts a failing ci-ok at once when that job's own result decides a merge-group run, so the queue drops a doomed group at the first failure rather than at the end of the run;
the ci-ok job's last step then sets every ci-ok check run on the head that is not its own to its own conclusion, so one verdict stands at the end of every attempt.
pr-title, the other check that ruleset requires, is the `pr-title` job at the end of this file, in the same shape and for the same reason:
it runs check-pr-text.sh on a pull request that is not a draft and on the merge queue's group, the two events this workflow runs that carry a pull request's text, and its own check run is the report, so it posts nothing.
pr-title.yaml carries that same check when the pull request is edited and when a review is submitted or edited, the two events this workflow does not run on, as its own job's check run too;
its header says why it runs on nothing else.
The docs site's build on a push to main, and its GitHub Pages deploy, are docs-pages.yaml's, a workflow of its own whose header says why.
Its paths are held to the docs filter below.

A new job joins the router in seven places:
a filter in `changes` that names every file the job reads, an output of `changes` for that filter, the job's `needs: changes` and an `if:` on that output carrying the term that skips it on a re-run attempt of a merge-group run (the comment above `changes` says why every job needs its own), an entry in CI_OK_SELECTED_BY (this file's env) mapping the job to that output (and in CI_OK_PULL_REQUEST_ONLY when its `if:` admits only pull_request), the ci-ok-report step as the job's last step (a job that calls a workflow passes `ci-ok-changes` to it, and that workflow's job ends with the step, as docs-site.yaml's and worker-image.yaml's do), the job's id in the ci-ok job's `needs:`, without which the verdict is reached before that job ends and says nothing about it, and a gate in .github/scripts/ci_job_reads.py.
An output an event must force is written in `changes`' outputs rather than left to its filter, as every output's workflow_dispatch case is, and so is one an event must hold back, as the eight slow suites' pull-request case is (the comment on them below).
The ci-filters job's .github/scripts/check-ci-filters.sh holds the filter to that gate, CI_OK_SELECTED_BY to the `if:`s, the report step to every job, the ci-ok job's `needs:` to every job of the file but the exempt ones, and the re-run term to every job but `ci-ok` and `pr-title`:
each of those two is named for a check main's ruleset requires, where a skip is a passing required check, so each refuses such an attempt in a step of its own instead.
docs/solutions/github/join-a-job-to-the-ci-router.md walks through it.

## Caches

Caches: no job of this workflow saves an Actions cache entry.
setup-go caches nothing here (`cache: false` on every step).
The jobs that cache Go restore one of six Go families with actions/cache/restore:
go-daemon (typecheck, test and pi-legion), go-docs (docs, and the build job of docs-pages.yaml, which saves it), go-envoy (envoy-e2e-local and envoy-session-stall, which build the same listener and natsd, listener-server-change and envoy-go-checks, which build the listener, and envoy-go-race-docs and envoy-go-docs-finalizers), go-envoy-test (envoy-go, which builds the test binary of every package of the envoy module, and the binaries, multiprocess and visual jobs of dispatch-browser.yaml, which build the Dispatch server and the real broker's commands out of that same set), go-envoy-infra (envoy-infra-go) and go-notices (lint's notices step).
Every family but go-docs is filled by .github/workflows/ci-caches.yaml, which builds those sets on a push to main and saves them;
go-docs stays with docs-pages.yaml, which saves it on that same push.
Each Go family keeps one entry, keyed on the Go version its job's setup-go installed, so it is refreshed only when Go is bumped.
A go.sum change downloads only the changed modules, and Go's build cache is content-addressed, so a stale entry costs a run only what changed;
a key on the go.sum files left a whole entry, up to a gigabyte, per change for seven days.
The fallback by prefix, when the exact key misses, now applies on every run, a dispatched one on main included:
it was held off main so that the entry a run saved after a Go bump held nothing the older Go built, and no run here saves any more.
There is no browser cache beside the Go families any more:
dispatch-browser.yaml's shard and alone jobs run in the image this repository builds, which carries Chromium, WebKit and Firefox at /ms-playwright, so a ~/.cache/ms-playwright restore would carry nothing they do not already have, and ci-caches.yaml's browsers job, the only writer of that entry, is gone with the restores (dispatch://INFRA-1443).
A seventh family, bun, holds ~/.bun/bin:
.github/actions/setup-bun restores it on every ref, keyed on the version .bun-version pins and with no restore-keys (a Bun binary of another version is one setup-bun rejects after paying for the restore, where a near-miss Go entry is still most of a module cache), and passes no-cache:
true on every ref, so setup-bun takes the restored binary as an existing installation and downloads the 51,794,595-byte release asset on a miss alone.
The action saves that family itself, on a push to main alone, which is no event of this workflow, and no job here writes any entry at all.
The browsers job called that action on every one of those pushes until it went, so what writes the entry now is the three workflows left that run on a push to main and call it
— docs-pages.yaml through docs-site.yaml, release.yaml and release-envoy-listener.yaml
— each on a push its own `paths:` selects, where the browsers job took every push to main.
The restore is made in every job but tool-fallback and tool-fallback-arm:
that action reads the id of the job that called it and holds its restore back for those two on every ref, so a restored Bun cannot stand in for the install each of their cases is a reading of.

## The Retries note

Retries: every step below that gets something over the network makes five bounded attempts rather than one, the last starting 150 s after the first (dispatch://INFRA-1545, R1).
The loop lives in one place, .github/scripts/ci-retry.sh, which a step sources and then calls as `retry_fetch <what> <per-attempt bound in seconds> -- <command...>`;
that file's own header says why it is sourced rather than run, and why each attempt is bounded on its own.
Where the verb is curl no helper is needed:
`--retry 7 --retry-all-errors --max-time <bound>` beside `-fsSL` makes 8 attempts whose last starts 127 s after the first, and .github/scripts/ci-tools.sh's `ci_tools_curl` carries the one full explanation of that spelling, `--fail` and its `--retry-max-time 120` included:
every tool install of .github/ci-tools.yaml fetches through it, and the actions that fetch anything else write the options themselves in the same shape.
What retries is the download and never the work after it:
a job's Go modules come down in a `go mod download` step of its own, so no `go build`, `go test`, `go vet` or `go run` is ever re-run by a retry.
Write that step after the job's cache restore and before its first Go step, which is where it does the work;
rule 11 of `.github/scripts/check-download-retry.sh` asks a job whose own `run:` body compiles Go for such a step, by presence and reading no order.
A job that compiles Go only through a script it calls, a composite action or a verb that rule does not read is outside it, and carries the step because a reader put it there:
that rule's reach paragraph says what was audited and by what method, and review round 17 of dispatch://INFRA-1545 found two jobs that lacked the step.
A `go install` or `go run` of a `module@version` is not covered by that step at all, whatever the job downloaded first:
it builds from that module's own go.mod and reads no go.work, so it is a download site of its own and retries where it is written (review round 18 of the same issue).
Four bounds are the same wherever that kind of fetch is made, because the kind rather than the step decides them (dispatch://INFRA-1545):
a `gh api` read is bounded at 30 s an attempt, which is ten times the about 3 s one takes;
a release asset or release archive at 60 s, a download of tens of megabytes, the Bun release zip measured at 51,794,595 bytes;
a `bun install` or `bun add` at 180 s, against 20.70 s measured cold on a pod and 11 s in CI over 180 observations;
and a `go mod download` at 180 s, 1.6 times the 113 s of the slowest cold run measured (packages/envoy/infra;
packages/daemon 81 s, packages/envoy 77 s, each on an empty module cache on 2026-11-07).
That last bound is the tightest of the four against its measurement, and it is the one that can afford to be:
an attempt its bound kills leaves what it fetched in the module cache and the next attempt resumes from there, so a `go mod download` heals itself across a retry where a tarball fetch starts its bytes over.
Every other bound is derived from its own step's measured maximum and names it beside the call.
A bound is per attempt, so neither a step's nor a job's `timeout-minutes` is a bound:
each is a sum of them, and the Budgets note of this note says how every one of them was computed. .github/workflows/ci-caches.yaml, which fills the families this note's Caches section lists, carries the same rule at every download of its own.
Each `bun install --frozen-lockfile` step is named for the command it runs, which is the handle .github/scripts/ci_job_reads.py's bun_job cites a frozen install by.
Two jobs below prove all of it against the fault it answers, rather than by reading:
download-retry runs each of those sites — a curl of a release asset, `mise install`, `bun add`, `bun install --frozen-lockfile`, `go mod download`, `bunx playwright install` and a `gh api` read
— behind .github/scripts/ci-fault-proxy.py, once with the proxy refusing for 60 s and then serving, where every site must recover, and once with it accepting the connection and sending nothing, where every site must end at its own bound instead of hanging;
download-retry-images does the same for the inline retries of the three Dockerfiles.
Their filters name the retry loop, the fault, the rig and the composite actions or Dockerfiles whose own downloads run under that fault, and nothing those downloads read:
a manifest, a lockfile, a pin or a module file moves what comes down the wire, not whether it survives a faulting host, so a change that touches none of those files selects neither job.
Both filters also name this file, because the two jobs' own steps, bounds and budgets are written here:
a change that touches ci.yaml at all selects both of them, and that pull request's run waits for the lane, about two hours of it, however small the edit.
That cost is taken on purpose, so that a change to a bound or a budget is proved against the fault by the run that makes it.
Neither job runs in a merge group, whatever the change touches
— the `changes` job's two outputs for them are 'false' there, and the comment above them says why a skip has to come from the output rather than from the job's `if:`.
A queue that waited for download-retry's two hours would hold every group behind it that long;
the pull request's own run proves the machinery, on the same files the group merges.

## The Budgets note

Budgets: a retried download's worst case is five attempts at its own bound with the helper's sleeps of 10, 20, 40 and 80 s between them, `5 x <bound> + 150 s`;
or, where curl makes the attempts itself, eight at its own bound with curl's waits doubling from one second, `8 x <bound> + 127 s`.

A third worst case is a figure this file cites rather than derives, because the bound it multiplies is not a number anybody wrote at the call:
.github/scripts/ci-tools.sh's `ci_tools_curl` passes `--max-time "$max_time"`, the manifest row's own field, so one call of it is priced by the DeclaredArithmetic entry for .github/scripts/ci-tools.sh `curl` in DECLARED_LOOPS of .github/scripts/check-download-retry.sh.
That entry writes 741 s and derives it there as three ladders
— the release asset, then the ghcr token and the blob
— each priced at the ceiling .github/scripts/ci-tools.sh's own `--retry-max-time` block derives, which is the one place that per-ladder figure is worked out.
A reader checks the arithmetic in those two places; no site below repeats either.

Two terms follow from that one, and every step below that holds one names which it holds and its sum rather than deriving either again.
A `uses:` step of an action that sources the helper in two of its steps carries 2 x 741 = 1482 s:
.github/actions/setup-bun, whose npm fetch and whose digest check each source it, and .github/actions/install-jj, whose mise step and whose jj step each do.
A step holding a `retry_fetch` beside those calls adds that helper's own arithmetic, which is what makes install-jj's `uses:` step 1482 + 450 = 1932 s, its `mise install` of the jj release being 5 x 60 + 150 = 450 s.
One call on its own — an action that sources the helper once — is 741 s.
A script a `run:` reaches is one call for each `ci_tools_fetch` it runs:
rule 8 charges it once, as its header's list of what it miscounts says, so a script that loops over rows is sized by hand.

A step that holds a retried download carries its own `timeout-minutes`, and that budget strictly exceeds the sum of its calls plus whatever unretried work it holds, by at least one whole minute.
Its calls are every retried download the step waits for:
the ones its own `run:` writes, the ones the scripts under .github/scripts/ that `run:` invokes write, and so on through every script those invoke in turn, and for a `uses:` step of one of this repository's composite actions the ones that action and the actions and scripts it reaches write.
A step whose `run:` is one call of a script carries that script's whole arithmetic, which is what the `pr-title` and `ci-ok` steps below each say beside their own budget.
Equal is not enough, and the reason is measured:
in run 42386541065's job 128445139635 a `gh api` read bounded at 30 s sat under `timeout-minutes: 5`, its own arithmetic of 5 x 30 + 150 = 300 s against a budget of 300 s, and the helper's `::error::` line beat the runner's kill by four milliseconds
— 17:22:16.7444966Z against 17:22:16.7483230Z.
A run whose attempts drift later loses that race, and the step then reports GitHub's bare `The action '<step name>' has timed out after <N> minutes.` instead of naming the download that failed and the attempts it made, which is the loud failure the retry exists to give.
GitHub reads this budget in whole minutes, so a whole minute is the smallest margin that can be written and the budget is the next whole minute strictly above the arithmetic. .github/scripts/check-download-retry.sh's rule 8 refuses a step that carries less, and a step that carries none at all:
a budget nobody wrote is a budget nothing judges, which is how this file's `airlock-lean` held the 8 x 60 + 127 = 607 s curl ladder .github/actions/install-elan carried before that action moved to `ci_tools_fetch`, with no term for it anywhere.
It holds for every retried step of this repository's workflows, and the browser suite's install steps in .github/workflows/dispatch-browser.yaml derive theirs the same way.

A job carries its measured duration plus the arithmetic of its single slowest retried step, and nothing more.
Both terms are written beside every `timeout-minutes` below, with the step named.
The first term is the larger of the budget that job carried before dispatch://INFRA-1545
— a ceiling its own comment derives, and in a Go test job one that a step's `-timeout` has to fire inside
— and the job's measured maximum over the 20 most recent successful ci.yaml push runs on main, 2026-11-06T20:05:59Z to 2026-11-07T04:44:20Z, the last runs this workflow made on that push before the queue took its place (the `on:` note below).
Three second terms repeat: the workspace install, bounded at 180 s per attempt, is 1050 s;
a `go mod download`, bounded at 180 s per attempt too, is 1050 s;
and the ci-ok report's `gh api` read (.github/scripts/ci-ok-report.sh) with the POST of the check run it ends in (.github/scripts/post-check-run.sh), bounded at 30 s each, are 600 s together, in every job that ends with that step, which is every job but `changes`.

A job's budget is rounded up to the whole minute too, and that rounding is what carries the step's margin:
where the slowest step's arithmetic is not itself a whole number of minutes the job's round-up already covers the step's, and where it is
— 300 s, 600 s, 2100 s — the step takes one more whole minute and the job takes it with it, so the job still outlives the step it names.
The ci-ok report takes more than that one minute, and this is the one place that says why, since every job of `.github/workflows/ci.yaml` carries that step:
its arithmetic is `ci-ok-report.sh`'s `gh api` read and `post-check-run.sh`'s POST of the check run, 2 x (5 x 30 + 150) = 600 s, and its budget is 720 s rather than the 660 s rule 8 would take.
That step posts the required `ci-ok` check, so a budget that cut it between those two ladders would lose the check run rather than report a failure, and the extra two minutes are what keep the report itself from being the thing that goes missing.

That check holds a job's own budget above each of its steps' arithmetic too, which is the half of this derivation a check can make:
it reads the attempts and not the measurement, and a job whose ceiling sits under one of its steps' ladders cuts that step before the step's own budget can name the site that failed.

The slowest step alone, and not the sum of every retried step, because the sum answers the wrong case.
A job whose every download exhausts every attempt at once has not met the transient fault a retry exists to survive
— one fetch, or a few, failing for about a minute, which is what both measured incidents were.
It has met an outage of the host, and in an outage failing at the job's budget is the right behaviour:
nothing the job retries into will answer, and holding a runner for hours buys cost and no green run.
So the job budget is where an outage stops, and each step's own budget is where that step's exhausted retry stops.

A budget is still a ceiling, not a spend:
a job reaches it only where its slowest retried step meets a host that accepts the connection and then sends nothing for the whole of every attempt.
What the job costs when nothing stalls is the measured maximum its first term names.
A step budget at or under that step's own arithmetic would cancel it in the middle of the retry the budget exists to allow, or in the same instant as its last attempt's own error.

## The browser_image filter's omissions

.github/ci-images.yaml is deliberately not named, and adding it back fails the build.
The build does read that file: its `row` step reads the registry and the digest of the row built from that Dockerfile out of it at run time, and its publish decision is that digest and the label the image at it carries.
But .github/scripts/check-image-trigger-paths.sh reads what a build reads from its context and its Dockerfile alone, and it refuses a path matched by a filter that decides nothing but an image build and that no build reads by that reading:
"browser_image matches .github/ci-images.yaml, which no image build reads, so a pull request touching only that builds an image for nothing; narrow the filter".
worker_image above leaves .github/scripts/repo-paths.sh out for that same reason, though its build job runs that script.
Nothing is lost by leaving it out.
A change to the row alone builds nothing, and the digest a row names is one the package already holds:
the ci-images job, whose filter does name that file, is what reads whether it does (.github/scripts/check-ci-images.sh package), and the push that writes the row needs no build of its own:
the image that row names was built and pushed by the earlier push, the one that changed the Dockerfile under .github/docker, which this filter does name.
The `container:` jobs that run the image are in .github/workflows/dispatch-browser.yaml, and this file declares no `container:`.
That check also fails the build when this list misses a file the image build reads;
it reads this filter only as a flat list, so it names no anchor. .github/scripts/ci_images.py is named, and is the one file here the build does not read as a file:
the build's `labels:` is `legion.browser-image.source-sha256=<the row step's source output>`, and that step computes the output with that file's recipe_sha256, so the file's own bytes decide a label of the image this build pushes.
Without it a change to the rule that defines the hash selected the ci-images job, whose rule 10 then refused the published image for carrying a hash this tree no longer computes, while no build ran and no row could move.
check-image-trigger-paths.sh reads it as an input of this build for that reason, from the COMPUTED_BUILD_INPUTS entry that writes the argument down, and that entry is what refuses this filter if it stops naming it.

## The ci-images job's ceiling and the tool copy

The tool copy step is the one term left out of that sum, and it is named here rather than left for a reader to miss.
Every request .github/scripts/mirror-ci-tools.sh makes rides the one ladder .github/scripts/ci-tools.sh holds (dispatch://INFRA-1545, R2), and each of its requests costs the ceiling .github/scripts/ci-tools.sh's `--retry-max-time` block derives for that request's own bound
— 217 s at the 30 s a request carrying no asset bytes is given, 247 s at the `max_time: 60` of a request that carries them — and this paragraph derives neither.
A ghcr-mirror row this run fills costs nine requests
— the two HEADs that read its state, the asset, the layer's POST and PUT, the config's POST and PUT, the manifest and the read-back, two of them carrying the asset's bytes
— which is 2 x 247 + 7 x 217 = 2013 s, and the four ghcr-mirror rows of that manifest with the one token request come to 247 + 4 x 2013 = 8299 s.
Carrying that term would put this job's ceiling near three hours (1500 + 8299 = 9799 s, by the Budgets note's rule of one slowest step) for every change the ci_images filter selects, to cover the one run in which the package answers no request of four rows until every ladder is exhausted
— an outage of the package this job has to fail in rather than wait out, since the copy it fills is this repository's own.
So the ceiling carries no term for that step.
What a run that does exhaust those ladders ends on is that step's own budget, and that budget is the figure .github/scripts/check-download-retry.sh prices rather than the sum this paragraph adds up:
the ladder's per-attempt bound is the row's `max_time`, a variable it cannot read, so its DECLARED_LOOPS entry for .github/scripts/ci-tools.sh `curl` writes what one call of that helper costs, 741 s, and the passes of a loop it cannot count are not multiplied into it.
The step below carries the next whole minute above that figure, and the nine requests a row really makes are why this job's ceiling is not raised to meet them.

## The tool-fallback job

The live proof of the second source every tool install of this repository now has:
it faults the host the first source is on, installs all eight tools through the source .github/ci-tools.yaml names for each, and then serves sixteen corrupted payloads and reads the refusal of every one of them.
It is permanent rather than a scaffold.
A second source nothing exercises rots — a copy nothing fills, a seam a refactor dropped, a digest nobody reads
— and the hour of 504s this work answers is the hour nobody wants to learn that in (dispatch://INFRA-1873).
This repository's own principle is that testing means booting and running it, so what says the fallback works on the day the assets are down is a job that takes them down and installs anyway.

Once the fault is up, no step of this job needs a GitHub release host to answer.
Every install that must succeed reads a source the fault does not hold
— the ghcr copy, npm, PyPI or get.pulumi.com
— or, for the Pulumi CLI, whose primary is get.pulumi.com and whose second source is the release asset, the origin serving that release's own staged bytes;
the bytes the corrupted cases serve were staged before the fault;
and the one case about a binary a third party installed seeds mise's install directory from that staged payload rather than downloading one inside the fault.
That is what keeps ci-ok green during the very outage this job proves survivable:
this job is in ci-ok's `needs:`, and its filter selects ci.yaml, .bun-version, the worker image's Dockerfile, the manifest and the helper beside it, and the eight actions, so a step of it that waited on a release host would turn every pull request touching one of those files red on exactly the day the fallback is doing its work. .github/scripts/ci-tools-proof.sh is the rig, and this job and tool-fallback-arm are its only callers.
It needs ci-images, which fills the copy this proof reads; the comment on the ci_images filter says why that filter is a superset of this job's.

WHAT THIS JOB DRIVES, AND WHAT IT ONLY REASONS ABOUT.
Everything a case is read on is driven here:
the fault, the origin that stands in for all four sources, the corrupted payloads, and the removals each case makes before it.
One thing is not.
oven-sh/setup-bun, which .github/actions/setup-bun's setup-bun step pins, restores a cached Bun into ~/.bun/bin/bun inside a step of its own, and the Actions cache it restores from is on hosts this job's fault does not hold. .bun-version holds an exact version, so that action builds its key and its URL without asking any host which version to take, and nothing the fault refuses stands in its way.
Taking a bun away before that step is therefore no answer to the cache, since the step would put a restored one back.
What keeps every case here a reading of an install is the wrapper's own `no-cache`, true for every job whose id starts with `tool-fallback` — `github.job` in a composite action is the id of the job that called it
— so no call of that action in this job can be served by the Actions cache, on any event and any ref.
That is read from the wrapper's expression rather than from a run of this job:
a run that told the action not to read the cache cannot measure what the cache would have served.
The removals the cases do make are about this job's own state:
a case that must read the second source takes ~/.bun/bin/bun and the bunx beside it away before its install, so what it reads is not the bun an earlier case left.

## The negative control's dead ports

The merge base's seven fail for two reasons, and which one is each install's depends on whether that install already read the helper there.
Its crane, changie, jj and Bun do, so their primary meets the fault and the ghcr and npm seams send them to a dead port.
Its elan, uv and Pulumi CLI read no seam at all, each being a `curl` of its own:
elan's and uv's one source is the release asset this fault holds, and the CLI's is get.pulumi.com, which this fault does not hold, so that step carries a dead proxy of its own instead.
Once this change is on main the base is this tree, and every one of the eight fails for the dead ports alone, which leave its second source nothing to reach.
A control that leaned on the merge base having one source each would expire the day this change lands on main:
the base would then be this tree, each primary would meet the fault, each second source would reach the real ghcr
— whose blobs redirect to pkg-containers.githubusercontent.com, which this fault leaves alone on purpose
— or the real npm, PyPI or get.pulumi.com, every one of them would succeed, and this step would fail every pull request and merge group that selects this job.
On a workflow_dispatch the ref below falls through to github.sha, the head itself, so that control is wrong already.
A dead port rather than a manifest with its `second_source` rows deleted:
a row with no second source makes ci_tools__second_source refuse the empty field, so the install would fail for the wrong reason
— it would prove the helper rejects an empty field, not that the install had nowhere to go, and a negative control must fail for the reason the check names.
A base that does not have the helper ignores both variables and fails as it always did.
The merge-base checkout stays.
It is this repository's only caller of the rule .github/scripts/check-image-trigger-paths.sh keeps about a local `uses:` under a second checkout of this same repository, and that check's own comment names this job for it.
After this change is on main the checkout is redundant to the dead ports below, which drive the control by themselves, so a later reader who deletes the merge base's seven steps is deleting that caller too and should move the rule's exercise rather than lose it.

## The tool-fallback-arm job

The same two halves of the Bun install, proved again on an arm64 runner. .github/actions/setup-bun's bun-second-source step selects a row from `uname -m` — `bun-linux-aarch64` here
— and reads .github/ci-tools.yaml through .github/scripts/ci-tools.sh, which needs python3 with PyYAML.
No other step of this repository that runs on ubuntu-24.04-arm imports yaml, so nothing says that runner image carries it until the arm64 leg of .github/workflows/release-envoy-listener.yaml runs, which happens on a push to main and so only after a merge.
This job says it on the pull request instead, and a release that would have failed on the first push fails here.
It proves both halves: the healthy path, where the release asset serves Bun and the action's setup-bun step holds it to the aarch64 row, and the npm path, under the fault the tool-fallback job uses, where the release hosts answer nothing and the second source serves the same binary.
Each half ends in a reading of what this runner holds, printed for a person reading the run:
the machine, the version, the path, the row's own pins, and what ci_tools_verify reads that binary as against the row.
The npm half says which source served it as well, because neither the version nor the digest can:
the release asset and the npm package are one binary by design, so a run where the release host answered after all would read exactly as a run where npm served.
What says it is the fault proxy's own 504 for github.com after the probe, the refusal that sent the install to its second source.
The `ci-tools:` line the install wrote would say it too, and no step can read it:
a composite action writes that line to the job's log, which no later step reads.
The tool-fallback job's comment about the Actions cache holds here too:
the wrapper passes no-cache for every job whose id starts with `tool-fallback`, so no call of it in this job can be served by the cache either, on any event and any ref.
The npm half takes ~/.bun/bin away before its install all the same, so no Bun the healthy half left behind satisfies it.
The `tool_fallback` filter selects this job, as it selects that one:
both drive the same action over the same manifest through the same helper, so one filter decides both and neither can drift from the other.
CI_OK_SELECTED_BY maps this job to that filter, and .github/scripts/ci_job_reads.py holds a gate of its own on it for what this job reads.
It needs the `changes` job alone: nothing it runs reads the ghcr copy the ci-images job fills, so it has no reason to wait for that job as tool-fallback does.

## The dispatch-browser job's two lanes

Every browser row of the Dispatch suite, which .github/workflows/dispatch-browser.yaml holds:
the binaries job that builds the Go commands the lanes run, the shard legs, the three `-alone` projects a machine each, the two-process job, the fake-embedder job that runs on both lanes, the containerised visual review, and the merge job that joins the shard and alone reports into one and posts this job's ci-ok report.

Two lanes, and which run pays for which.
A pull request without the label `full-ci` runs `pull-request`, the two projects a change is held to (chromium and iphone), so such a run proves those two rows and no more - it is not the run that proves the suite before main.
Every other run - the merge group, a workflow_dispatch and a pull request carrying `full-ci` - runs `full`:
every project, the three `-alone` ones and the two-process job.
So the merge group is where a change meets the whole suite before it lands, and the nightly (.github/workflows/dispatch-browser-nightly.yaml, which calls the same file with the full lane, from main) runs it again on main's tip.

One input decides that: the called workflow reads `lane` for the projects each job runs, so nothing here chooses them a second time.
It picks no browser to install any more:
the shard and alone jobs run in the image this repository builds, which carries all three engines (dispatch://INFRA-1443), and the two-process job installs Chromium whatever the lane.

The matrix is in the called workflow and cannot be here.
Every leg of a matrix shares one job id, and .github/actions/ci-ok-report posts the failing ci-ok of a merge-group run from the job whose own result decides that run, as that job's last step:
legs written here would each be `dispatch-browser`, so a decided group would take one such post per leg that failed, each posting under a job id several legs answer to.
A called workflow's legs are jobs of that workflow, and one job of it - the merge job, which needs every other - posts the single report this job id gets.

## The race lane

The race lane: delivery (its intake handler reads the routing settings the poll loop publishes, from NATS's delivery goroutine), the Dispatch store (the pool's one-connection guard, and the hold mark of its own each concurrent read takes), the bus (two connects on one SOCKS5 dialer record each dial's error in one record and read it back, TestConnectsSharingADialerRecordItsLastError), the listener, where the role claim watcher, the session and interest watchers and the request handlers meet, with the role claim view, its lock and its epoch in the registry store and the watchers' completeness and rescans in kvwatch (INFRA-141), the session cache, whose reads take its shared lock beside watcher writes and the expiry timer's callback as kvwatch's resume point is read while a replaced watcher still applies, the Dispatch API and the document service - all under the race detector.
A read that walks a live document without its lock races the room's peers, which no ordinary run shows:
envoy-go's unit-test step runs without -race (LEGION-499).
The lane sits beside envoy-go because -race runs these packages several times slower, and each job carries a package deadline of its own, since go test's ten-minute default is a whole package's.
envoy-go runs none of these packages plain any more (INFRA-1408):
its unit step leaves out exactly what the lane runs, and the one plain run it keeps of them is the step named "Test what the race build cannot run at all or at full size", which runs the handful of assertions the detector makes impossible or shrinks and nothing else.

The lane is three jobs: envoy-go-race (delivery, the Dispatch store, the bus, the listener, the registry store, the session cache and kvwatch), envoy-go-race-api and envoy-go-race-docs, whose own comment below says why the document service runs alone.

api sits apart because isolation is what made it fast:
sharing a runner, the race binaries contended for the four vCPUs, and alone api fell from 1099.487 s to 746.718 s.
The packages left in envoy-go-race are cheap enough to share one:
the two heaviest of those measured there, the bus at 286.931 s and the listener at 365.787 s (run 40926978371), are a quarter and a third of the lane's bar of 1080 s, which is 60 % of a 30-minute step's own timeout, so a runner each would buy nothing and cost about 8,000 replayed runner minutes.
Running a heavy package apart also reads its seconds as its own job's, and leaves the others' numbers standing when one is red.

pmdoc is not in this lane: its tests run plain only, in envoy-go's unit-test step (INFRA-1408).
It sat here because LEGION-571's flakes - pmdoc's own under -race and load, not a race in production code - cost three review rounds when nothing ran the package under -race before a PR claimed it.
Its product code is still race-instrumented wherever a race job compiles it, since envoy-go-race-docs imports pmdoc;
what went is the second, race-built run of pmdoc's own tests.

No job of this lane installs Bun: pmdoc's tests, which shell out to the generated TypeScript fixtures under internal/dispatch/pmdoc/gen and refuse to run without bun in CI (render_readback_test.go), were the only ones here that needed it, and they run in envoy-go's unit step now, while the "Check pmdoc fixtures are up to date" step that keeps those fixtures current is envoy-go-checks'.
delivery's and the bus's NATS runs in the test process (internal/testnats), so no job here needs a container runtime of its own.

Delivery, the Dispatch store, the bus, the listener, the registry store, the session cache and kvwatch share this runner.
The Dispatch store's tests (store_test.go:28, storetest.go:44) and delivery's (internal/dispatch/delivery/intake_test.go:39-41) read DISPATCH_TEST_DATABASE_URL and skip without it, and cmd/listener/dispatch_resolution_test.go:158 gets its database through internal/testpg, so the job carries the lane's Postgres service;
the registry store itself is the NATS KV view and needs none.

## The download-retry job's rig

Proves the retries of this workflow against the two faults they answer:
a host that refuses for a while and then serves, and one that accepts the connection and sends nothing.
Both are injected by .github/scripts/ci-fault-proxy.py in front of one step at a time, and every site below is the real composite action or the real install command, never a copy of one:
a copy would prove a loop, and what is under test is the site (dispatch://INFRA-1545, T7, checks #0946ed09 and #55bc6685).

Each site is three steps: one that starts a proxy of its own, probes it and prints its address, the site itself with that address in its own `env:` and nowhere else, and one that reads the proxy's log and prints which site it was, how many attempts reached the proxy, how far apart the first and the last were, and the seconds the step took against its budget (.github/scripts/download-retry-proof.sh).
The proxy variables are each step's own because a job-wide pair would reach every JavaScript action after it, the actions/upload-artifact inside .github/actions/ci-ok-report among them;
the step before the report checks that this job's environment holds none of them.

The one proxy variable this job does write for every step is the bypass, emptied: the first step below writes `no_proxy=` and `NO_PROXY=` to $GITHUB_ENV.
A pod of this build carries `no_proxy=*` and `NO_PROXY=*`, and a star is every host:
a client honouring either one fetches from the real host with the proxy sitting there untouched, so the site passes in about no time having met no fault, which is exactly what this job exists to prove it meets.
A GitHub runner leaves both unset today, so nothing here changes what the sites read;
what the step buys is that nothing a runner image ever sets can void the whole job in silence.

$GITHUB_ENV and not each step's own `env:`, for three reasons.
An `env:` map may hold one spelling of a name and not two (the run that proved it is named further down), and one spelling does not answer for the other:
a runner applies the map case-sensitively, so a `no_proxy: ''` beside an inherited `NO_PROXY=*` leaves the bypass live for every client that reads the upper-case one, Go's among them.
An `unset` written into a `run:` cannot reach the six sites that are `uses:` steps of a composite action.
And the probe runs in a step of its own in ci.yaml, so only a clearing the whole job carries makes its reading evidence about the step that follows it.
An emptied variable is no proxy and routes nothing, which is why the `clear` step before the ci-ok report refuses a proxy variable that carries a value and accounts for an emptied one rather than refusing it.

Clearing the bypass is necessary and not sufficient
— a client with a proxy configuration of its own bypasses with those variables cleared
— so each site's proxy is probed before the site runs, in the step that starts it:
one request to a host the fault names, which must be refused, and one to that same host under the reserved `.invalid` suffix, which the proxy must answer itself.
The pair is printed as it was read, the proxy's own log line for each request beside it, and the step fails naming which of the two was wrong.
A bypass then shows in the evidence whatever caused it:
a bypassed request reaches the real host and leaves no line in the proxy's log at all, so no reader has to reason about which client honours which variable.
The report step prints that pair again beside its own result, with the proxy's own line for the site's first faulted and first answered request.

Each step sets the lower-case pair alone.
GitHub reads an `env:` key case-insensitively and refuses a map that holds two spellings of one name, so http_proxy and HTTP_PROXY together do not start the workflow at all:
run 42367176473 of this repository failed before any job, with no job record of its own.
The upper-case pair is not needed, measured on 2026-11-07 against ci-fault-proxy.py with the lower-case pair alone set:
`bun install` answered `ProxyConnectFailed (504)`, `gh api` answered `Gateway Timeout`, and `go mod download` answered `Gateway Timeout` for proxy.golang.org.
curl reads the lower-case spelling for a plain-HTTP fetch and either for an HTTPS one, so one pair serves every client here.
That refusal is the parser reading one `env:` map, which is why the bypass this paragraph describes is cleared through $GITHUB_ENV instead:
an environment file is lines and not a map, so it carries both spellings of no_proxy without the workflow refusing to start.

A proxy per site, not one per job: a recovery window is counted from the proxy's first moment, so a second site under the first site's proxy would run after that window closed and would meet no fault.

Every site carries continue-on-error, and its report step is what passes or fails the job:
under the stalling fault a site is supposed to fail, after five bounded attempts, and under the recovering one it is supposed to succeed.
Only the report knows which, so only the report decides.
The stall half's `::error::` annotations are the retry helper saying it exhausted its attempts, which is the behaviour under test.

The recover half runs first, on a runner whose caches are empty, and one step then empties what it filled:
a download that reads a warm cache never reaches the network, and a fault no download meets proves nothing.
The stall half's reports hold that cleaning to account — a site that fetched nothing ends in seconds, under the floor its attempts' sleeps alone cost.

No matrix, though two lanes would halve the wall time:
ci-ok's report is one artifact per job id (`ci-ok-<job>-<run attempt>-<status>`, .github/actions/ci-ok-report), so two lanes would upload one name twice, and ci-ok.sh refuses a reported job CI_OK_SELECTED_BY does not map, which a lane's name can never be (check-ci-filters.sh holds that map to ci.yaml's jobs).

## The four stalls of install-changie

252 s is the mechanism's own minimum and not a measurement, which is why it sits a few seconds under the readings and is no timing bet.
Every term of it is a bound this fault forces to be spent in full - the probe's faulted request to its 10 s PROBE_BOUND, then each stalled attempt to its own 60 s with curl's 1 s wait between the two of a ladder - and `report` fails on `elapsed < floor` alone, so a slower runner only lengthens what it measures.
What trips this floor is a run that ends EARLY: fewer attempts, or attempts that do not reach their bounds, which is exactly the regression it exists to catch.
The readings sit just above it, as they must, and they are read on two clocks that must not be confused:
`report` measures from the proxy's own start and so includes the probe, while a step's own duration does not.
On report's clock, which is the one this floor is compared with, a drive of this action's body on this issue's pod read 255 s (review round 8 of dispatch://INFRA-1545) and the runner read 258 s in run 42573254973's job 129015393389.
The 244 s this section quotes further down is a drive of the same body on the step's own clock, which leaves out the probe's ten seconds, and is not a reading of this assertion.

An earlier reading of the same run called those four one ladder and asserted 2 with a 123 s floor.
That number passes a step whose retry is gone, because the fallback's own single attempt is the second stall:
it was the fixture of this site, which has one source, that made the mutation look as though it bit.

So the mutation is driven on the step itself.
On this issue's pod, with this action's own body run under a `--mode stall` proxy:
as it ships, the proxy is reached 4 times, at +11s, +72s, +133s and +194s, the step ends at 244 s and the report prints `as this mode promises` and exits 0;
with the retry taken out of `ci_tools_curl` in a scratch copy (`curl -fsSL --max-time "$max_time"`), the proxy is reached twice, at +11s and +72s - one attempt a ladder - the step ends at 121 s and the report fails on both halves, the count and the floor.
The pod reads the same four stalls the runner does, which is also what settles the earlier account of a figure that differed by machine:
it differed by how many of the two ladders the drive ran.

## The download-retry-images job's rig

The same recovering fault, inside the three Dockerfiles whose network RUN lines carry the inline retry (dispatch://INFRA-1545, T7, check #23856666).
A build reads none of the runner's environment, so each one is passed the proxy as http_proxy and https_proxy build arguments:
those are Docker's own predefined build arguments, which BuildKit sets as environment variables of every RUN and keeps out of the cache key, so no Dockerfile names a proxy anywhere.

How a build reaches the proxy: a RUN runs in a network namespace of its own, where 127.0.0.1 is the build container and not the runner, so the proxy binds to the docker bridge's gateway (ci-fault-proxy.py's --bind, resolved by `start --bind docker-bridge`) and the build is given that address.
The gateway is the build container's default route and needs no entitlement, where `--network=host` would need one the default docker driver does not grant.

What is faulted is what each build fetches, and never ghcr.io:
a FROM is resolved by the daemon, which reads its own configuration and not these build arguments, so every base image is pulled first, unfaulted, and the smoke image the held-port wrapper builds on is built first too, with no proxy at all.
Each faulted build is --no-cache, since a reused layer fetches nothing and would meet no fault, and every build is --load into the runner's own daemon:
nothing is pushed, and no registry credential is held.

The builds are run through .github/scripts/download-retry-proof.sh rather than written into these steps, because .github/scripts/check-image-trigger-paths.sh refuses any image build it can read in a run step and cannot be told otherwise, and docker/build-push-action
— the one form it reads — would hold this job's trigger paths to everything the three builds read, which is the wide filter this narrow gate exists to avoid.
That check still proves the triggers of the builds that publish: worker-image.yaml's and the envoy-smoke job's.

The filter names the three Dockerfiles, the retry, the fault and the rig, and no source file of anything the builds compile:
worker-image and envoy-smoke prove the builds, this job proves the retry inside them, and no source file changes that behaviour.

Each build's proxy is probed before the build runs, in the step that starts it, as the download-retry job's sites are:
a request to a host the fault names must be refused and one to that host under the reserved `.invalid` suffix must be answered by the proxy itself, and the step fails naming which reading was wrong.
A build reads none of the runner's environment, so no bypass of this job's own can carry a build past the fault;
what the probe answers for here is the proxy, which binds to the docker bridge gateway rather than to localhost
— a fault nothing can reach refuses nothing, and a build that fetched freely for 45 minutes would read like a build that survived one.
This job clears the bypass for its own steps all the same, since the probe is one of them and a client honouring `no_proxy=*` would read past the proxy it is probing.

## The proof-sdk-offline job

The proof that no install of this repository fetches the proof editor's upstream sources over the network:
packages/proof-editor/package.json installs proof-sdk-upstream from the tarball committed beside it, so a frozen install reads those bytes out of the checkout and asks no host for them.
This job runs that install behind a proxy that answers HTTP 504 for the hosts of the old `github:sjawhar/proof-sdk#<sha>` route - api.github.com, where Bun asks for a `github:` dependency's tarball, and codeload.github.com and objects.githubusercontent.com, which that request redirects to, and every subdomain of the three - and fails when the proxy's log holds a single refusal, on a cold install and on a warm one.
It refuses the behaviour rather than the text:
a gate on the string `github:sjawhar/proof-sdk` would pass the same dependency spelled another way, and this one asks what the install actually reaches.
It is permanent because a reintroduced remote dependency necessarily moves bun.lock, which the proof_sdk_offline filter selects on, and the install then asks the proxy for one of those hosts:
the refusal count goes above zero and the verdict step fails, printing every refused line.

github.com itself is not faulted, and that is measured rather than assumed.
A cold runner install makes one request to github.com and one to release-assets.githubusercontent.com, and none to any of the three hosts this section names further up (job 128647167946's own tally).
That pair is a release asset, which is how a dependency's install script fetches a prebuilt binary;
it is not this route, and faulting github.com turned the job red on it (job 128643900648).
The host this route has to use is api.github.com, which the merge-base drive showed, failing on https://api.github.com/repos/sjawhar/proof-sdk/tarball/dc6a852 with six refusals on that host and none on the others.
These three are the hosts this issue's check names.
The last step's tally of what the proxy passed through is where every other host the installs reach shows.

## The merge queue's cancel act

The merge queue's cancel act.
Main's queue builds each group on top of the group below it, so when a group fails, every group built on it can no longer merge:
the queue takes that pull request out and rebuilds the groups above it.
That removal is a dequeue, so .github/workflows/dequeued.yaml force-cancels the removed group's own run, and the groups built on the failed one keep their runners until the queue rebuilds them.

`cancel-started` runs at the start of every merge-group run, with no `needs:` so that it starts at once, and it has one act:
it cancels the groups of the same pull request the queue has replaced, which test a tree that will not land.
Its own queue branch names that pull request (`gh-readonly-queue/<base>/pr-<n>-<parent sha>`), and .github/scripts/merge-group-cancels.sh decides from it and the run list which runs those are:
the script makes no GitHub call and prints one `cancel <run id>` line per run to force-cancel, in the order to act on them; its header says what it reads and why.

Each cancel is `POST /repos/{owner}/{repo}/actions/runs/{id}/force-cancel`, acted on in that order.
A force-cancel bypasses `if: always()` — the step of a running job is left skipped, not run, measured on a probe run of this repository
— so a force-cancelled run posts no ci-ok, and its ci-ok job, which waits for every job of the run, is left unstarted or cancelled rather than concluding one.
That is what this act wants: a failing ci-ok on a group the queue still holds could take an innocent pull request out of the queue.
The run of a group whose own job failed posts that group's failing ci-ok from that job itself, through the merge-group branch of .github/scripts/ci-ok-report.sh, so the queue reads the verdict and rebuilds the groups above it without waiting for the rest of that run.

The job follows these rules:

- it runs only on `merge_group` and only on attempt 1 (the comment above `changes` says why a re-run attempt does nothing);

- `permissions:` is `actions: write` and nothing else.
  `gh run list` and the force-cancel need it (a scope granted `write` grants its read);
  the checkout needs no `contents: read`, because this repository is public and GitHub serves a public repository's git data to any valid token
  — measured here with `git ls-remote` over https against a public repository the token holds no grant on at all, which succeeded;

- it carries no ci-ok-report step and reports nothing to ci-ok, so its id is in CI_OK_EXEMPT in this file's env, outside ci-ok's accounting;

- it runs this repository's own .github/scripts/merge-group-cancels.sh out of the run's own checkout and nothing else:
  no bun, no go, no node, no installed dependency, no npm or bunx tool.
  A merge-group run checks out the queued tree, so the script it runs is the queued change's copy, which that change's review has read;

- its step's token is ${{ github.token }}, and its time limit is short: the act is a handful of API calls.

A force-cancel that fails leaves a warning rather than a red job:
the run it names may have completed between the list and the call, and an act that reclaims nothing is no failure of the run it runs in.

## The ci-ok job's shape

The verdict, and one of the two checks main's ruleset requires of this router.
It needs every job of this file but itself, `cancel-started` and `pr-title` (the CI_OK_EXEMPT comment above says why those three are outside ci-ok's accounting), runs under `always()` so that a failed, skipped or cancelled job still reaches a verdict, and fails when .github/scripts/ci-ok.sh's verdict over `toJSON(needs)` is failure.
ci-ok is therefore this job's own check run, which GitHub puts in this run's own check suite, where a check run created with `POST /repos/.../check-runs` under GITHUB_TOKEN joins the Actions App's first suite at that commit instead, which is another workflow's as often as not (.github/scripts/post-check-run.sh's header holds that measurement).

A job of its own waits for a runner, and that wait is the cost of this shape:
it is created with the run but starts only once the last of its needs has ended, when the pool is busiest with the next wave.
docs/solutions/github/join-a-job-to-the-ci-router.md holds every reading of it.
The shape is kept because a late verdict is right while one in another workflow's suite blocks the head until a person re-runs it.
What this job cannot do, and the ci-ok-report step every other job ends with still does, is post the failing ci-ok of a merge-group run one job's result has already decided:
this job waits for every job of the run, and a group that can no longer merge should leave the queue at the first failure rather than at the last job's end.

This job's `if:` is `always()` and nothing else.
Every other job of this file is skipped on a draft pull request:
`changes` carries `!github.event.pull_request.draft`, the jobs its outputs gate skip with it, and lint, typecheck, test and `pr-title`, whose `if:`s do not wait on `changes`, carry that term themselves.
Every job but `pr-title` and this one is skipped on a re-run attempt of a merge-group run by `github.run_attempt == '1'` (the comment above `changes` says what both terms do for them); this job carries neither.
A job an `if:` holds back is skipped, and a skipped job's check run is a passing required check, so either term would leave the head a ci-ok nothing judged:
a merge group would merge on it, and a draft would carry it as a pass.
An `if:` can only take the job away, while what both cases must get is a verdict, so the job runs on both and a step of it concludes the failure:
the two refusal steps below, one for a draft pull request and one for an attempt above the first.
`pr-title`, the other job named for a check that ruleset requires, holds no attempt term for the same reason and refuses such an attempt in a step of its own too.

So on a draft this job is the whole run:
every other job is skipped, there is nothing to judge, and the failure of its draft refusal is what keeps a draft from carrying a passing ci-ok.
That failure names the remedy — marking the pull request ready for review starts a run that selects by the same diff and concludes a real verdict over it.

`checks: write` and nothing else: the verdict step makes no GitHub call, and the step after it reads this head's ci-ok check runs and PATCHes them with that scope.
The checkout needs no `contents: read`, as `cancel-started`'s does not
— this repository is public, and GitHub serves a public repository's git data to any valid token.
