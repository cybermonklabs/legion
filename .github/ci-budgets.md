# CI budgets, and the notes behind them

The arithmetic every retried step and job of this repository's CI is budgeted by, and the derivations behind the budgets whose figures are this branch's (dispatch://INFRA-1545).
These notes were moved out of `.github/workflows/ci.yaml` when that file neared GitHub's 500 KB cap; since main's #3675 the derivations and histories of each workflow's own jobs and steps are in `.github/ci-notes/<workflow>.md` (`.github/ci-notes/README.md` says how), the router's own notes - how it selects jobs, what it caches and which runner a job takes - among them in `.github/ci-notes/ci.md`.
What stays here is what many files cite: the Retries note and the Budgets note, and the budgets whose arithmetic is derived once for several steps or jobs.
Each step's and each job's own arithmetic stays on its line in the workflow file, where a reader judging that budget sees it.

In these notes `this file` and `this workflow` mean `.github/workflows/ci.yaml`, where this text was written, and so does a bare `below` or `above` wherever the thing it points at is a step, a job or a comment of that workflow.
A job's steps now sit in the reusable workflow `.github/workflows/ci-job-<id>.yaml` that ci.yaml's job of that id calls (main's #2945), so a step or a step's comment such a pointer names is in that job's own file.
A pointer that means a passage of this note says so in its own words - `of this note`, `this section`, `further down` - rather than relying on this paragraph, because a legend read by a hundred pointers is only as true as its least checked one, and this one has been wrong in both directions already.

## The Retries note

Retries: every step below that gets something over the network makes five bounded attempts rather than one, the last starting 150 s after the first (dispatch://INFRA-1545, R1).
The loop lives in one place, .github/scripts/ci-retry.sh, which a step sources and then calls as `retry_fetch <what> <per-attempt bound in seconds> -- <command...>`;
that file's own header says why it is sourced rather than run, and why each attempt is bounded on its own.
Where the verb is curl no helper is needed:
`--retry 7 --retry-all-errors --max-time <bound>` beside `-fsSL` makes 8 attempts whose last starts 127 s after the first, and .github/scripts/ci-tools.sh's `ci_tools_curl` carries the one full explanation of that spelling, `--fail` and its `--retry-max-time 300` included:
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
The test job's tmux install, and dispatch-ui's zsh install (main's #4158), retry both apt phases through .github/scripts/ci-retry.sh, in e2e.yaml's spelling for the same runner image and the same indexes: five attempts, the last starting 150 s after the first.
What bounds an attempt is the inner `sudo timeout`, not `timeout sudo`, so the signal reaches apt-get itself rather than a sudo holding it;
retry_fetch's own bound sits five seconds above it and backstops the sudo alone.
90 s for the index read and 120 s for one package of a few megabytes, both derived in .github/workflows/e2e.yaml's own apt step, which names the jobs those rates were measured in;
that derivation was .github/scripts/playwright-install.sh's head until the browser suite moved into an image this repository builds and took the script with it (cybermonklabs/legion#2938), and e2e.yaml is where it lives now.
The tmux and zsh steps cite it rather than repeating it.
The unpack after the download takes no network and stays bare, so an attempt the bound kills never stops a dpkg run part way.
The index read carries `-o APT::Update::Error-Mode=any`, which makes a failed index read fail the command instead of warning and exiting 0:
without it retry_fetch has nothing to retry.
.github/scripts/check-download-retry.sh's rule 5 is the whole reason.
A bound is per attempt, so neither a step's nor a job's `timeout-minutes` is a bound:
each is a sum of them, and the Budgets note of this note says how every one of them was computed. .github/workflows/ci-caches.yaml, which fills the families the Caches paragraph of .github/ci-notes/ci.md#the-router-every-check-a-pull-request-or-the-merge-queue-runs lists, carries the same rule at every download of its own.
Each `bun install --frozen-lockfile` step is named for the command it runs, which is the handle .github/scripts/ci_job_reads.py's bun_job cites a frozen install by.
Two lanes prove all of it against the fault it answers, rather than by reading, the jobs of .github/workflows/ci-job-download-retry.yaml, which ci.yaml's one download-retry job calls:
download-retry runs each of those sites — a curl of a release asset, install-omp's curl of its release archive through `retry_fetch` (a `mise install` until main's #4877), `bun add`, `bun install --frozen-lockfile`, `go mod download`, `bunx playwright install` and a `gh api` read
— behind .github/scripts/ci-fault-proxy.py, once with the proxy refusing for 60 s and then serving, where every site must recover, and once with it accepting the connection and sending nothing, where every site must end at its own bound instead of hanging;
download-retry-images and download-retry-images-worker do the same for the inline retries of the four Dockerfiles, the worker image's in the second job, which runs it five times as the five runs of one matrix, and the other three in the first, which builds the browser image twice.
The lanes are one ci.yaml job, gated on the download_retry filter, which is both lanes' filters together, and each lane is gated in that file on an input its caller fills from its own filter's output, download_retry_sites or download_retry_images, so each lane runs on exactly the changes its own filter selects:
ci.yaml calls as many reusable workflows as .github/scripts/check-workflow-files.sh allows it, and a call of a file of each lane's own would be one more.
Their filters name the retry loop, the fault, the rig and the composite actions or Dockerfiles whose own downloads run under that fault, and nothing those downloads read:
a manifest, a lockfile, a pin or a module file moves what comes down the wire, not whether it survives a faulting host, so a change that touches none of those files selects neither lane.
So the download_retry_sites filter holds no bun_install and no go_workspace anchor, no pin and no worker Dockerfile:
each of those is what an ordinary change touches, and a filter holding them would select a job of about two hours on every dependency bump.
The job that proves a version is the one that builds with it.
Both filters also name ci.yaml, because the lanes' own steps, bounds and budgets are reached from it:
a change that touches ci.yaml at all selects both of them by paths.
Since main's #5175 a pull request runs no slow suite without a label, and the lanes are one, 118.6 minutes at the 90th percentile of this pull request's own successful runs (.github/ci-notes/ci.md#which-lane-a-job-runs-in-the-pull-request-or-the-merge-queue):
download_retry, the output the job's `if:` tests, is false on a pull request without the label `full-ci` or `lane:download_retry`, so a change to a bound or a budget is proved against the fault by the run that makes it only when that run carries one of those labels, about two hours of it, however small the edit.
Neither lane runs in a merge group, whatever the change touches or the labels say
— the `changes` job's three outputs for them are 'false' there.
download-retry took 116.5 minutes at this branch's head, and a queue that waited for download-retry's two hours would hold every group behind it that long;
a labelled pull request's run, or a dispatched one, proves the machinery, on the same files the group merges, and it is the one held-back job whose result never comes from the queue.
The event test is in those outputs rather than in the job's `if:` because an `if:` that declines a merge group leaves the job selected and skipped, and .github/scripts/ci-ok.sh fails a selected job whose result is not success, reading that skip as a disagreement between the filter and the `if:` the author must fix;
it admits a skip only for a job `changes` did not select, which is what 'false' there makes this one.

## The Budgets note

Budgets: a retried download's worst case is five attempts at its own bound with the helper's sleeps of 10, 20, 40 and 80 s between them, `5 x <bound> + 150 s`;
or, where curl makes the attempts itself, eight at its own bound with curl's waits doubling from one second, `8 x <bound> + 127 s`.

A third worst case is a figure this file cites rather than derives, because the bound it multiplies is not a number anybody wrote at the call:
.github/scripts/ci-tools.sh's `ci_tools_curl` and `ci_tools__curl_held` pass `--max-time "$max_time"`, the manifest row's own field, so one call of either is priced by the DeclaredArithmetic entry for .github/scripts/ci-tools.sh `curl` in DECLARED_LOOPS of .github/scripts/check-download-retry.sh.
That entry writes 854 s for each of those two curls and charges one `ci_tools_fetch` both, 1708 s, which it derives there as four ladders
— the release asset, then the ghcr token, the blob request to the package and the read of the store that request's redirect points at
— each priced at the ceiling .github/scripts/ci-tools.sh's own `--retry-max-time` block derives, which is the one place that per-ladder figure is worked out.
It was three ladders, 1281 s, until main's #4214: since then a request of the package that carries a credential follows no redirect, so the blob request and the read of the store it points at are two ladders where one request had followed the redirect inside one.
The figure is the worst row's, a ghcr-mirror row with a release asset of its own (crane, changie and mise, and since main's #4313 their `-aarch64` rows, the arm64 builds an arm64 runner installs, which cost the same four ladders); a row whose `primary` is empty costs less, the jj or jj-aarch64 row's token, blob request and store at 3 x 427 = 1281 s and a bun row's one npm tarball at 427 s, and a site below that names one `ci_tools_fetch` is charged the 1708 s whichever row it reads, so a job main's #4313 moved to ubuntu-24.04-arm, which reads the `-aarch64` row where it read the x86_64 one, keeps every figure it carried.
A reader checks the arithmetic in those two places; no site below repeats either.

Two terms follow from that one, and every step below that holds one names which it holds and its sum rather than deriving either again.
A `uses:` step of an action that sources the helper in two of its steps carries 2 x 1708 = 3416 s:
.github/actions/setup-bun, whose npm fetch and whose digest check each source it, and .github/actions/install-jj, whose mise step and whose jj step each do.
A step holding a `retry_fetch` beside those calls adds that helper's own arithmetic, which is what makes install-jj's `uses:` step 3416 + 450 = 3866 s, its `mise install` of the jj release being 5 x 60 + 150 = 450 s.
One call on its own — an action that sources the helper once — is 1708 s.
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
Two halves of that are checked rather than read:
`.github/scripts/check-download-retry.sh`'s rule 8 refuses a job whose ceiling sits under a step's retry arithmetic, and refuses one whose ceiling is not above that step's own `timeout-minutes`, since the job holds that step and everything else it runs.
The measured term is in neither, so a job key is still a figure a person derives and a reviewer reads; what the check gives is the floor it may not stand under.
Twenty-two job keys stood below their own derivation while this check was green, because only the first of those two halves was read (dispatch://INFRA-1545).
Both terms are written beside every `timeout-minutes` below, with the step named.
The first term is the larger of the budget that job carried before dispatch://INFRA-1545
— a ceiling its own comment derives, and in a Go test job one that a step's `-timeout` has to fire inside
— and the job's measured maximum over the 20 most recent successful ci.yaml push runs on main, 2026-11-06T20:05:59Z to 2026-11-07T04:44:20Z, the last runs this workflow made on that push before the queue took its place (the `on:` note below).
Three second terms repeat: the workspace install, bounded at 180 s per attempt, is 1050 s;
a `go mod download`, bounded at 180 s per attempt too, is 1050 s;
and the ci-ok report's `gh api` reads (.github/scripts/ci-ok-report.sh's jobs read and its read of the queue's tip) with the POST of the check run it ends in (.github/scripts/post-check-run.sh), bounded at 30 s each, are 900 s together, in every job that ends with that step.
FOUR JOBS OF `ci.yaml` DO NOT: `changes`, which every other job waits on and which posts nothing;
`pr-title` and `cancel-started`, which carry checks of their own; and `ci-ok` itself, which posts the aggregate check rather than reporting to it.
The five jobs that call a workflow of their own
— `docs`, `dispatch-browser`, `dispatch-browser-pr` (main's #5175, the second caller of dispatch-browser.yaml), `worker-image` and `browser-image` — carry no step of this file at all, so the term belongs to the jobs of the workflow each of them calls (review of dispatch://INFRA-1545 counted them at this head).

A job's budget is rounded up to the whole minute too, and that rounding is what carries the step's margin:
where the slowest step's arithmetic is not itself a whole number of minutes the job's round-up already covers the step's, and where it is
— 300 s, 600 s, 2100 s — the step takes one more whole minute and the job takes it with it, so the job still outlives the step it names.
The ci-ok report takes more than that one minute, and this is the one place that says why, since every job of `.github/workflows/ci.yaml` carries that step:
its arithmetic is `ci-ok-report.sh`'s two `gh api` reads, of the run's jobs and of the tip of the queue's branch (main's #4983), and `post-check-run.sh`'s POST of the check run, 3 x (5 x 30 + 150) = 900 s, and two calls of `runner-shutdown.sh`'s `runner_shutdown_kills` (main's #2945).
The first is `ci-ok-report.sh`'s own, on a merge-group run where the reporting job failed, to tell a runner shutdown from a failure of the change;
where it finds none, the script runs `ci-ok.sh`, which makes the second for the run's failed jobs in a process of its own, so it reads the check runs again rather than reusing the first call's.
Each call is two reads: the check runs on the head, 5 x 30 + 150 = 300 s, and the annotations loop, which one deadline of 180 s bounds and `check-download-retry.sh`'s DECLARED_LOOPS prices at that deadline plus one read's whole ladder, 180 + 300 = 480 s.
That is 900 + 2 x (300 + 480) = 2460 s, which `check-download-retry.sh` charges, its DECLARED_LIBRARIES entry for the ci-ok-report action writing the two calls, 1560 s, since round 37 of dispatch://INFRA-1545, and the tip read's ladder since the forward merge of main's #4983 at round 55 put that read through retry_fetch, run through gh-read.sh's gh_wait_reset, which the check charges one ladder as it charges a bare retry_fetch, as it charges every other read and the post here since review round 36 put each of them through that wait as well;
until round 37 this note wrote one call, 600 + 300 + 480 = 1380 s, and a step whose first call found no shutdown went past it, and until round 55 it wrote no tip read, 600 + 2 x (300 + 480) = 2160 s, with a budget of 38 minutes.
2460 s is a whole 41 minutes, so rule 8 would take 42 minutes, 2520 s, and the budget is 2580 s, 43 minutes, the same one minute over that the 720 s and 660 s of the two ladders alone carried.
That step posts the required `ci-ok` check, so a budget that cut it between its ladders would lose the check run rather than report a failure, and the extra minute is what keeps the report itself from being the thing that goes missing.
A STATED LIMIT, which gh-read.sh's header states too: a rate-limit wait on one of these calls is held to the step's limit with that call's own ladder and 30 s after it, not with the calls after it, so a wait and then 5xx answers on those later calls can still run the step to its 43 minutes, which the runner reports as the step's timeout, never as a pass.
Every job's report step carries that 43, and its STEP_TIMEOUT_MINUTES with it, since each read's wait and the post's is held to it; a job whose slowest retried step is the report adds 2460 s, the arithmetic, to its first term; ci.yaml's ci-ok verdict step, which sources `runner-shutdown.sh` alone and makes one call, carries its two reads, 300 + 480 = 780 s -> 14 min, and its STEP_TIMEOUT_MINUTES with them.

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

## The ci-images job's ceiling and the tool copy

The tool copy is this job's slowest retried step, and its arithmetic is derived here once, for the steps' budgets and the job's ceiling both.
Every request .github/scripts/mirror-ci-tools.sh makes rides the one ladder .github/scripts/ci-tools.sh holds (dispatch://INFRA-1545, R2), and each of its requests costs the ceiling .github/scripts/ci-tools.sh's `--retry-max-time` block derives for that request's own bound
— 397 s at the 30 s a request carrying no asset bytes is given, 427 s at the `max_time: 60` of a request that carries them — and this paragraph derives neither.
A ghcr-mirror row a run fills costs nine requests, none of its HEADs following a redirect
— the two HEADs that read its state, the asset, the layer's POST and PUT, the config's POST and PUT, the manifest and the read-back, two of them carrying the asset's bytes
— which is 2 x 427 + 7 x 397 = 3633 s.

THE COPY IS FILLED ONE MACHINE AT A TIME, IN TWO STEPS, AND THE CEILING IS WHY.
Main's #4313 gave each of the five ghcr-mirror rows (crane, changie, mise, jj and elan) an arm64 build of its own, `<tool>-aarch64`, so the manifest holds ten.
One step that filled all ten would wait, cold, for 427 + 10 x 3633 = 36757 s, and the job's ceiling by the Budgets note's rule would be 1500 + 36757 = 38257 s -> 638 min, past the 360 minutes GitHub runs a job on one of its hosted runners: a ceiling the runner cannot honour is one the step's own ladder outlives, and the step would then end in GitHub's bare timeout line rather than naming the row that failed.
So the job runs the script once per machine, `Fill the copy of every x86_64 tool asset the package does not hold` and `Fill the copy of every aarch64 tool asset the package does not hold`, and each run fills the five rows of its machine with the one token request, 427 + 5 x 3633 = 18592 s.
Which rows a machine's run fills is read by ci-tools.sh's `ci_tools_mirror_rows`, the one reading of that split, which the tool-fallback job's two blob checks read their rows with too:
an `-aarch64` row is aarch64's and any other x86_64's, as the install actions read it from `uname -m`, and the asset the row's copy is filled from must name that machine as release assets do.
It refuses, by name and in either machine's run, a ghcr-mirror row the two readings do not agree on, which is how a third machine's row would read, and it refuses a machine no row is built for, so every ghcr-mirror row is filled by exactly one of the two steps or both fail; nothing is skipped in silence.
That is a cold fill, the run in which the package answers no request of a machine's five rows until every ladder is exhausted, and it is what each step can wait for.
.github/scripts/check-download-retry.sh charges it so since round 37 of dispatch://INFRA-1545: its DECLARED_LIBRARIES entry for .github/scripts/mirror-ci-tools.sh writes 18592 s for each step that runs the script, which replaces the one `ci_tools_fetch` it charged before, then 1281 s, a figure that left the step's loop of requests out.
Each step carries the next whole minute above 18592 s, 310 minutes, and the job's ceiling is the Budgets note's rule of one slowest step, 1500 + 18592 = 20092 s -> 335 min.
Two such steps in one job is the case that rule answers: a run in which both machines' fills meet a package that answers nothing is an outage of the package, where failing at the job's ceiling is the right end, and a fill that meets a transient fault spends its own ladder and no more.
Until round 37 this note left the term out of the ceiling, to keep the job near two hours for every change the ci_images filter selects, and the step's own budget of 22 minutes cut a cold fill inside its first row.

The package step's own budget is held by hand in `.github/workflows/ci-job-ci-images.yaml`, and the job's ceiling held it too until the tool copy above it was priced, because .github/scripts/check-download-retry.sh prices none of that step's reads:
those reads are urllib's and subprocess's, inside the Python that .github/scripts/check-ci-images.sh runs, and rule 8 puts a figure on no Python at all.
Its rule 6 does read a `urlopen` written in a heredoc of a `.sh` under .github/scripts, which is what holds these reads to the floor.
Python anywhere else
— a `.py` file, or a heredoc in a workflow's or an action's `run:`
— is read by no rule of that check, the floor included.

## The tool-fallback-arm job

The same two halves of the Bun install, proved again on an arm64 runner. .github/actions/setup-bun's bun-second-source step selects a row from `uname -m` — `bun-linux-aarch64` here
— and reads .github/ci-tools.yaml through .github/scripts/ci-tools.sh, which needs python3 with PyYAML.
Every job of ci.yaml that runs on ubuntu-24.04-arm and installs Bun makes that same read, so a runner image without PyYAML fails all of them;
what this job adds is the second half, the install from npm when the release asset serves nothing, which no other arm64 job drives and which the arm64 leg of .github/workflows/release-envoy-listener.yaml would otherwise meet first, on a push to main and so only after a merge.
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
both drive the same actions over the same manifest through the same helper, so one filter decides both and neither can drift from the other.
CI_OK_SELECTED_BY maps this job to that filter, and .github/scripts/ci_job_reads.py holds a gate of its own on it for what this job reads.
After the Bun halves it proves the other arm64 rows the same way (main's #4313): crane-aarch64, changie-aarch64, mise-aarch64, jj-aarch64 and elan-aarch64, each installed once from its release asset and once under the fault, where each must read its ghcr copy and each install's own 504 is read after a probe of its own (the called file's comments say how).
Those faulted installs read the copies the ci-images job fills, so this job needs that job for order and carries `!cancelled()`, as tool-fallback does and for the reason that job's comment gives.

The budget its key carries is added up from the slowest run this job can have rather than copied from another job:
the checkout and the runner's download of the actions (1), the healthy install of the 34 MB release zip (2), the fault and its probe, whose two requests are bounded at 15 s each (1), the faulted install
— oven-sh/setup-bun's own download against a proxy that refuses it at once, then one ladder of .github/scripts/ci-tools.sh, whose `--retry-max-time 300` allows new attempts for five minutes, then takes the last 64 s wait a decision made just under that limit still sleeps, and then one last attempt of the row's `max_time` 60 s: 424 s at worst, written as 427 s, the ceiling the fetch helper ci-tools.sh derives in its block on that option (8)
— the 107 MB npm tarball, its gunzip and the write (3), the two readings, each a sha256 of that binary (2), the report step (1), and the machine watch's start and its report (1).
That is 19 minutes, 1140 s, the term of the two Bun halves.
That ladder was (5) at the 120 s bound this change replaced, where it was 247 s, and the term was 16 minutes.

The arm64 rows' cases run after the Bun halves, and their faulted installs run behind the stand-in curl the tool-fallback job uses, which ends each refusal the fault arranges in about a second; the Bun halves run before it and are left as they were measured.
Their term is one whole ladder per fetch those cases make that can be slow and still be served.
Before the stand-in that is the release asset of each healthy install, which is the outage this job exists to survive; after it, the primary of each faulted install is refused at once by the fault and cut to about a second by the stand-in, and the slow source left is its ghcr copy.
install-jj makes two fetches each time, mise's archive and jj itself (mise's own download on the healthy half, the jj copy on the faulted half), so the four healthy and four faulted installs make ten.
Nine of them are ladders of .github/scripts/ci-tools.sh, at the 427 s its block derives for a row's `max_time` of 60 s;
the tenth, mise's own fetch of jj on the healthy half, is install-jj's `retry_fetch` at 60 s an attempt, whose slowest run that still ends served is four attempts at that bound, the 150 s of waits and a fifth attempt, 5 x 60 + 150 = 450 s.
9 x 427 + 450 = 4293 s, 72 minutes (72).
On the faulted half that same retry meets the fault five times before install-jj reads the jj copy: the fault answers each attempt at once, and the waits between them are 10 + 20 + 40 + 80 = 150 s (3), which every run of this job spends.
The four probes, each two requests bounded at 15 s (2), and the rest of these cases as measured, the readings, the stand-in and the removals, 25 s in all in run 44073112404 (1).
That is 78 minutes, 4680 s, and with the Bun halves' 1140 s it is 5820 s, 97 minutes, this job's unretried term;
a job its own limit cuts off reports `cancelled`, which ci-ok fails, on exactly the day the copies are doing their work.
Main's #4313 wrote the same construction at its own ladder of 124 s against a host that hangs and with no retry in front of mise's fetch, 10 x 124 s (21) + 2 + 1 = 24 and 15 + 24 = 39, and carried 40.
The comment above the job's key in `.github/workflows/ci-job-tool-fallback-arm.yaml` adds the job's slowest retried step to that term, either install-jj step's 3416 + 450 = 3866 s: 5820 + 3866 = 9686 s -> 162 min.

Measured, this job sits far inside the 162 minutes its key carries, and the ladders the term above allows for are not driven in the runs measured.
In five green runs on 2026-11-10, with the Bun halves alone, it took 41 s, 41 s, 43 s, 45 s and 50 s (jobs 129726973920, 129733754662, 129745140461, 129743510021 and 129721254112), where the tool-fallback job took 1,611 s to 2,007 s in those same five runs;
in run 44073112404 (2026-11-17), with the arm64 rows' cases and before mise's fetch was retried, the whole job took 76 s, 11:31:39Z to 11:32:55Z, the new cases 25 s of it and the two Bun halves 2 s and 33 s, as before them.
The reason is the shape of this job's faulted Bun install.
It reads ci_tools_fetch_fallback (.github/actions/setup-bun/action.yml's `ci_tools_fetch_fallback "$row" "$HOME/.bun/bin/bun"`), which reads the second source alone and tries no primary (the `ci_tools_fetch_fallback()` function of .github/scripts/ci-tools.sh), and the primary it does not try is fetched by oven-sh/setup-bun, a Node action that calls no curl.
The one fetch the helper makes there is of npm, which answers.
In the step list of job 129743510021 one step alone is above 3 s: the faulted setup-bun at 32 s, which is the 107 MB npm download.
The 427 s above stay as the headroom of the run where npm does not answer at once.
The Bun halves are therefore the control of the change INFRA-2621 makes to the tool-fallback job:
a stand-in curl shortens that job's arranged refusals, and those halves, whose measured runs drive no such ladder, run before this job's own stand-in and must not move.
What moves here is the ceiling and not the measurement:
15 minutes stood under this job's own slowest retried step, so the job would have cut that step before it could name its fetch (dispatch://INFRA-1545).

Each install step of the job declares its own ladder's arithmetic with the whole minute that figure takes: 57 for either setup-bun step's 3416 s, 65 for either install-jj step's 3866 s, and 29 for each other install's one `ci_tools_fetch`, 1708 s, where main's #4313 wrote 11 for each of the eight it added, the bound the tool-fallback job's installs with nowhere to go carry.
None carries `continue-on-error`, so a held install here is loud by itself:
the bound fails the job, the steps after it are skipped, and the job uploads its log.
The job's own limit reaches an install first only when the steps before it took more than that limit less the install's own budget: 162 - 65 = 97 minutes before an install-jj step, 162 - 57 = 105 before a setup-bun step and 162 - 29 = 133 before any other install.
The slowest run that still passes is 97 minutes in all, so the steps before any install take less than that, and the 76 s measured above says they take far less.
The reading either way is a job with a log, which is what the twenty-six installs of tool-fallback cannot give and why their clock exists.

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
install-changie, install-omp and the Go download run once per outage of their call's chain instead, each run faulting its outage's host, most of them with a window of its own (ci-fault-proxy.py's `--per-host-window`) which the site's own first request opens and the rig's probe never does, and the proxy.golang.org run with one window over every host, so that `|direct` cannot answer for the retry:
one window from the proxy's start had closed before those calls first reached release-assets.githubusercontent.com behind github.com's redirect, and storage.googleapis.com and sum.golang.org behind proxy.golang.org (round 63 of dispatch://INFRA-1545), and faulting a chain's hosts together made the calls meet their windows as a series no real outage is (round 64).
.github/ci-notes/ci-job-download-retry.md, "The sites lane's arms with a window per host", derives each run's budget and says what its report checks beyond the rest.

Every site carries continue-on-error, and its report step is what passes or fails the job:
under the stalling fault a site is supposed to fail, after five bounded attempts, and under the recovering one it is supposed to succeed.
Only the report knows which, so only the report decides.
The stall half's `::error::` annotations are the retry helper saying it exhausted its attempts, which is the behaviour under test.

The recover half runs first, on a runner whose caches are empty, and one step then empties what it filled, as a step between the Go runs empties the module cache for each:
a download that reads a warm cache never reaches the network, and a fault no download meets proves nothing.
The stall half's reports hold that cleaning to account — a site that fetched nothing ends in seconds, under the floor its attempts' sleeps alone cost.

No matrix, though two lanes would halve the wall time:
ci-ok's report is one artifact per job id (`ci-ok-<job>-<run attempt>-<status>`, .github/actions/ci-ok-report), so two lanes would upload one name twice, and ci-ok.sh refuses a reported job CI_OK_SELECTED_BY does not map, which a lane's name can never be (check-ci-filters.sh holds that map to ci.yaml's jobs).

The recover half of the job's derivation moved from 3640 s to 6740 s in round 30, when its arms were priced at the ladder their retry guarantees rather than at two bounds, to 7020 s in round 63, when three of them took 70 s for each host they first reach late, a figure that bounds only a host met on a call's first attempt, to 5962 s in round 64, when those three became seven runs of one outage each, and to 5652 s in round 65, when the Go download's sum.golang.org run was dropped, since that download never asks sum.golang.org while proxy.golang.org answers.
THE JOB'S KEY READ 221 UNTIL ROUND 28 OF dispatch://INFRA-1545, where that 221 came from 11310 + 1932 = 13242 s
— the measured term before the stall arm's budget grew by 20 s, and install-jj's two ladders before each `ci_tools_fetch` went from 741 s to 1281 s.
The lane is the one job whose cancellation this whole issue exists to prevent, and its derivation is written above the job's id in `.github/workflows/ci-job-download-retry.yaml` rather than above its key.
Rule 8 reads it there since round 35, from the block that prices that key, and refuses a key that does not strictly exceed the sum that block states;
it also refuses any job holding a retried step whose comments price none.

Each site's budget in that job carries 20 s more than it did before the probe:
`report` measures a site from the moment its proxy started, and the probe's two requests are made between the two, each bounded at 10 s by the rig's PROBE_BOUND.
Only a stalling fault spends a whole bound
— a 504 and the proxy's own 502 are both immediate
— but the term is the bound either way, since what a budget may not do is depend on the fault answering quickly.

A recover site's budget is the ladder its own retry guarantees, the same figure its stall twin carries:
every attempt at its bound, the sleeps between them, the probe and a minute.
The six runs of one outage each, install-changie's two, install-omp's two and the Go download's two, carry their ladder's sleeps, the call measured at fbcc6647 and the probe instead (153, 178 and 310 s).
THE RECOVER ARM CHECKS THAT THE RETRY RECOVERS - the step succeeds through the fault - AND NOT HOW FAST A SITE RECOVERS, nor that each attempt holds its bound, which the stall arms check;
each run of one faulted host also checks that its site met that host and that the call passed on a retry, before its last attempt.
Its report also holds the step's time to this budget, and in the four arms that carry their stall twin's figure that can never fire:
the faulted step's own `timeout-minutes` and the 20 s probe come to no more than the budget (install-claude-code and the workspace install 1100 s against 1130 s; Playwright 1700 s against 1730 s; the gh read 380 s against 380 s), so the step's own key ends it first.
The six runs' budgets are under their steps' keys with the probe, install-changie's 1760 s, install-omp's 500 s and the Go download's 1100 s, so their reports can fail them first.

The rule the recover arms' budgets were written to until round 30 of dispatch://INFRA-1545 was that an attempt in flight when the window closes runs to its bound and the next one succeeds, so each carried two bounds.
Two readings of the workspace install broke it:
250 s at round 29's head, one hung attempt more than that model allows, and 616 s in run 43357362345, where attempts 1, 2 and 3 each reached their 180 s bound
— attempts 2 and 3 having started after the fault had cleared, on a healthy network
— and attempt 4 installed.
The step succeeded both times, so the retry held; what was false was the arm's model of recovery.
All seven recover arms carried that rule; four now carry their stall twin's budget, and the other three are the six runs of one outage each.

Each stall site ends at its own bound instead of hanging: five attempts of the helper's loop, or curl's eight, and then the step fails.
The floor is what the sleeps between the attempts cost on their own (10 + 20 + 40 + 80 for the helper, curl's 127 for its eight), so a step that ended under it did not make its attempts;
the budget is the attempts at their bounds plus those sleeps plus the probe's two bounded requests plus a minute, so a step that ended over it did not hold its bounds.
The probe is in the budget because `report` measures a site from the moment its proxy started, and the probe is made between the two;
under the stalling fault its faulted request spends its whole 10 s bound, which is the reading
— a host that answers at once is the proxy bypassed.

## The ten stalls of install-changie

Ten stalls, and they are TWO LADDERS OF FIVE rather than one ladder of ten:
install-changie fetches through `ci_tools_fetch`, which reads the release asset first and the copy in the package second, and the stalling fault meets both halves (the asset on github.com, the blob behind ghcr.io's redirect to pkg-containers.githubusercontent.com, faulted by suffix in .github/scripts/ci-fault-proxy.py).
Against a host that spends every attempt's whole bound each ladder is five attempts, at 0, 62, 124, 188 and 256 s, the set .github/scripts/ci-tools.sh's own block derives;
against the RECOVERING fault an attempt ends the instant the proxy answers, so all eight are made, which is what the recovering arm reads.
THE STEP'S ASSERTIONS ARE READ FROM THIS BRANCH'S OWN RUN, not the 120 s bound's:
ten reaches from +15 s to +586 s and 646 s of step (run 43202546798, job 131129681795), against a floor of 640 s and the budget its own rule's 710 s.

640 s is the mechanism's own minimum and not a measurement, which is why it sits a few seconds under the readings and is no timing bet.
Every term of it is a bound this fault forces to be spent in full - the probe's faulted request to its 10 s PROBE_BOUND, then each stalled attempt to its `max_time` and each wait between them - so the floor is 10 + 2 x (5 x 60 + 15) s:
two ladders of five attempts, with 1 + 2 + 4 + 8 s of waits inside each.
What trips this floor is a run that ends EARLY: fewer attempts, or attempts that do not reach their bounds, which is exactly the regression it exists to catch.
The readings sit just above it, as they must, and they are read on two clocks that must not be confused:
`report` measures from the proxy's own start and so includes the probe, while a step's own duration does not.
On report's clock, which is the one this floor is compared with, run 43202546798's job 131129681795 reached the proxy ten times, from +15 s to +586 s, and the step ran 646 s.

THE FOUR THIS SECTION ASSERTED UNTIL ROUND 28 OF dispatch://INFRA-1545 WERE THE 120 s BOUND'S, and they are kept here because they are what makes the floor's purpose plain.
At `--retry-max-time 120` a stalling host gets two attempts a ladder, 61 s apart, so the whole drive was four stalls ending at about 258 s:
run 42573254973's job 129015393389 read them at +15, +76, +137 and +198 s, and a drive of this action's body on this issue's pod read 255 s on report's clock and 244 s on the step's own.
At `--retry-max-time 300` the same host gets five a ladder, at 0, 62, 124, 188 and 256 s.
So the assertions that were right at the old bound PASS THE OLD BOUND AND NOTHING ELSE:
four stalls and a floor of 252 s are met by a revert to 120, which is what made this arm a tautology once the bound moved, and why it now asserts ten and 640.
Both halves of that were driven against `report` before round 28 pushed:
with four stalls at 258 s it refuses, naming `the proxy was reached 4 time(s), fewer than the 10 attempts the retry makes` and `it ended after 258s, under the 640s its attempts' sleeps alone cost`, and with this head's ten at 646 s it passes.

An earlier reading of the old run called those four one ladder and asserted 2 with a 123 s floor.
That number passes a step whose retry is gone, because the fallback's own single attempt is the second stall:
it was the fixture of this site, which has one source, that made the mutation look as though it bit.

So the mutation is driven on the step itself.
On this issue's pod, with this action's own body run under a `--mode stall` proxy at the 120 s bound:
as it shipped then, the proxy is reached 4 times, at +11s, +72s, +133s and +194s, the step ends at 244 s and the report prints `as this mode promises` and exits 0;
with the retry taken out of `ci_tools_curl` in a scratch copy (`curl -fsSL --max-time "$max_time"`), the proxy is reached twice, at +11s and +72s - one attempt per source - and the report refuses it.
The pod reads the same stalls the runner does, which is also what settles the earlier account of a figure that differed by machine:
it differed by how many of the two ladders the drive ran.

## The download-retry-images job's rig

The same recovering fault, inside the four Dockerfiles whose network RUN lines carry the inline retry (dispatch://INFRA-1545, T7, check #23856666).
The rig is two jobs' since round 63: download-retry-images builds the Envoy and held-port images and the browser image twice (round 65), and download-retry-images-worker the worker image five times, as the five runs of one matrix, each a runner job of its own, because one job holding all the builds would come to 33922 s, above the 360 minutes GitHub gives a job on a hosted runner; .github/ci-notes/ci-job-download-retry.md#each-image-builds-budget-the-gates-price-for-that-build derives each budget and the two keys.
Each build's proxy runs ci-fault-proxy.py's `--coalesce-outage-gap <G>`: the first request of a listed host opens an outage that answers 504 for 60 s, a host first reached while it is open joins it, a host first reached less than G s after it ended is passed on, and the first host reached G s or more after it ended opens the next outage, however late in the build that comes.
A call that meets listed hosts one after another meets one outage, as a real outage is, where a window per host (round 63) spent its ladder on each in turn.
G is 190 s for the Envoy image, 290 for the held-port image, 380 for the browser image and 230 for the worker image: 80 s, the inline retry's last sleep, plus the longest single attempt of a retried call in that arm's own builds, rounded up to the next whole 10 s.
Every RUN line of a Dockerfile that reaches a listed host is meant to meet a fault in some run of its arm, which is why the browser image is built in two runs and the worker image in five, and the seven lines no run faults are stated: .github/ci-notes/ci-job-download-retry.md#the-image-arms-outages-and-their-coverage.
Each report prints, beside the requests its proxy passed on by host, the requests it faulted by host, with the second each host's first request came after the proxy started, and the outages the proxy opened and the hosts it left within G.
Each report also reads its build's own log, BuildKit's plain progress, which `build` keeps beside the job log, and the rig builds the committed Dockerfiles themselves, never a copy of them:
each inline retry() prints `retry: <what>: attempt <k> of 5 failed` on stderr for each attempt that fails, from an echo in the canonical text .github/scripts/check-download-retry.sh holds every definition to, and the report prints each call that failed an attempt with the last attempt it reached.
THE ATTEMPT CHECK OF AN IMAGE BUILD fails a build where a call's attempt 4 of 5 failed, since its last attempt then ran, and where a ladder printed `<what> failed after 5 attempts`;
where a RUN printed 7 `curl: (N)` lines of curl's own `--retry 7` (curl's 7th attempt starts about 63 s after its first, so a 60 s outage costs a call about 6, and a 7th means attempt 8 of 8 may have run), the lines a retry() attempt line follows being that attempt's own and not counted;
and where the log holds neither kind of line, since then the fault reached no retried call of the build.
download-retry-proof.sh's comment above that reading says why the curl lines are counted by RUN and where that fails closed.
A build reads none of the runner's environment, so each one is passed the proxy as http_proxy and https_proxy build arguments:
those are Docker's own predefined build arguments, which BuildKit sets as environment variables of every RUN and keeps out of the cache key, so no Dockerfile names a proxy anywhere.

How a build reaches the proxy: a RUN runs in a network namespace of its own, where 127.0.0.1 is the build container and not the runner, so the proxy binds to the docker bridge's gateway (ci-fault-proxy.py's --bind, resolved by `start --bind docker-bridge`) and the build is given that address.
The gateway is the build container's default route and needs no entitlement, where `--network=host` would need one the default docker driver does not grant.

What is faulted is what each build fetches, and never ghcr.io:
a FROM is resolved by the daemon, which reads its own configuration and not these build arguments, so no base image pull meets the fault.
download-retry-images pulls its builds' bases first, unfaulted, and builds the smoke image the held-port wrapper builds on first too, with no proxy at all;
download-retry-images-worker lets BuildKit pull the worker image's bases inside the build, as worker-image.yaml's own build does.
Every one of those bases is an image this repository publishes, so a failed pull is seen and not retried over (.github/scripts/check-download-retry.sh, exception (a)).
Each faulted build is --no-cache, since a reused layer fetches nothing and would meet no fault, and every build is --load into the runner's own daemon:
nothing is pushed, and no registry credential is held.

The builds are run through .github/scripts/download-retry-proof.sh rather than written into these steps, because .github/scripts/check-image-trigger-paths.sh refuses any image build it can read in a run step and cannot be told otherwise, and docker/build-push-action
— the one form it reads — would hold this job's trigger paths to everything the four builds read, which is the wide filter this narrow gate exists to avoid.
That check still proves the triggers of the builds that publish: worker-image.yaml's, browser-image.yaml's and the envoy-smoke job's.

The filter names the four Dockerfiles, the retry, the fault and the rig, and no source file of anything the builds compile:
worker-image, browser-image and envoy-smoke prove the builds, this job proves the retry inside them, and no source file changes that behaviour.

Each build's proxy is probed before the build runs, in the step that starts it, as the download-retry job's sites are:
a request to a host the fault names must be refused and one to that host under the reserved `.invalid` suffix must be answered by the proxy itself, and the step fails naming which reading was wrong.
A build reads none of the runner's environment, so no bypass of this job's own can carry a build past the fault;
what the probe answers for here is the proxy, which binds to the docker bridge gateway rather than to localhost
— a fault nothing can reach refuses nothing, and a build that fetched freely for 45 minutes would read like a build that survived one.
Each job clears the bypass for its own steps all the same, since the probe is one of them and a client honouring `no_proxy=*` would read past the proxy it is probing.

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

## The pi-legion job's ceiling

The budget, added up part by part from the slowest run the pi-legion job can have rather than copied from another job:
the job's set-up with the NATS service started and the checkout (15 s), setup-bun (2), install-jj (3), install-omp (6), setup-go and the go-daemon cache restore (26), the workspace install and contracts' build (14), the lint and the typecheck (16), the test step (300), the pack, the upload and the ci-ok report (4), the post steps with the containers stopped (5), and the seconds the job's own clock holds over the sum of its steps (5).
That is 396 s, 6 m 36 s, the floor a ceiling here has to clear.
Since main's #5175 the typecheck and the pack are pi-legion-build's, the other job of `.github/workflows/ci-job-pi-legion.yaml`, so this sum adds the job up as it was with them, a floor the job without them stays under, and it is still the first term of its key.
The test step runs the package's files in one worker per CPU (`bun test --parallel`, package.json, main's #5096), so it lasts as long as its slowest worker, which runs legion-stall-watchdog-omp.test.ts alone: its eight cases took 241.3 s, 281.5 s and 296.4 s of the step's 241.4 s, 281.8 s and 296.7 s in jobs 137348443705, 137353258170 and 137361573250 (2026-11-22), the 300 above being the slowest rounded up.
Three runs are no worst case. Run one after another, the files took a median 774 s that day.
Until #5096 the files ran one after another, and the test step was added up from its rows at their slowest over the four most recent runs of the whole suite, 708 s, for a sum of 804 s.
A job its own limit cuts off reports `cancelled`, which ci-ok fails, and 10 did that to the merge groups of 2026-11-08T23:22Z and 2026-11-09T00:53Z with every step of the job concluded `success`, when the files still ran one after another under a 10-minute limit.
The key is not the next minute above that sum: #2918 (dispatch://INFRA-1545) makes install-jj retry on a 3866 s ladder and gates a job's ceiling above its slowest retried step, so that ladder and not this suite is what decides the key, 396 + 3866 = 4262 s -> 72 min.
The 2 s above is a cold-Bun figure, measured before this branch:
it is the release asset setup-bun downloaded when its own cache was the only one and no-cache held it off every ref but main.
This branch restores ~/.bun/bin from a key of its own on every ref, so a pull request's run and a merge group's both start from a hit where main saved one, which moves that term down rather than up.
The two terms that key carries are derived in the comment above it in `.github/workflows/ci-job-pi-legion.yaml`.

## The size a workflow file may be

GitHub starts no run of a workflow file above a size, and the failure says nothing:
the run it creates holds zero jobs and concludes failure at once, so every required check of it is absent rather than red.
`.github/scripts/ci_workflow_rules.py` refuses any file of `.github/workflows/` above 500,000 bytes for that reason, which is the figure the documentation states.
What this repository measured is a bracket around GitHub's own threshold rather than the threshold itself:
a run STARTED at 501,500 bytes (run 42710088044) and STARTED NOTHING at 525,100 (run 42739729814).
So the gate sits below a size GitHub was seen to tolerate, deliberately, and the margin between 500,000 and 501,500 is not somewhere to spend.
`ci.yaml` reached 500,593 bytes merged with main during dispatch://INFRA-1545 and took three of its own jobs red with it, which is why the background behind that file's budgets lives in this note:
a step's own priced sentence stays beside its key, where the reader of that step and rule 8 both need it, and the measurements, run ids and reasoning behind it are here.

## The tool-fallback job's budget, part by part

The budget, added up from the slowest run this job can have, the way the tool-fallback-arm job below adds up its own:
the job's set-up, the two checkouts and the runner's download of the actions (1), the two steps that read the package and the manifest, and `stage`, which fetches about 320 MB over the real sources while the network is whole and so keeps the pinned ladder (2), the jj-primary case, which runs before the fault (1), the fault and its probe, whose two requests are bounded at 15 s each (1), the one case that keeps the ladder, the head's own `install-crane`, which measured 64 s at the 60 s bound this branch replaced and which the 300 s one prices at 300 + 64 + 60 = 424 s, written as 427 s for the seconds a second's rounding in each term can add - the ceiling the fetch helper ci-tools.sh derives in its own block on `--retry-max-time`, since curl decides whether to retry when an attempt ends and a decision taken just under 300 s still sleeps its last 64 s and starts one more attempt (8), the other steps whose primary the fault or a dead port holds, each now about a second because the stand-in shortens them:
job 129874475871 made 65 fetches through the stand-in, and 29 of them were shortened
— 19 of github.com, 9 of the dead address and 1 of get.pulumi.com behind the base-pulumi control's dead proxy.
The other 36 kept the pinned ladder: the kept case's own fetch at 63 s, which is that job's reading at the 60 s bound and reads 127 s at a bound above it, measured at the 120 s one (job 130417801504) and unchanged at 300 s, since a host that refuses at once makes its eight attempts either way, the probe's reserved control name, and the honest fetches of the package, npm, PyPI, get.pulumi.com and the local origin, none of which took more than a second (1), the bytes the installs and the eighteen cases move, over npm, PyPI, get.pulumi.com, the package and loopback, the two 107 MB Bun tarballs among them (3), and the readings, the `ladder-report` step, the stop-fault step and the report (1).
That is 18, three more than the 15 this list came to at the 120 s bound, because the kept case's own ladder moved from (5) to (8), and a job its own limit cuts off reports `cancelled`, which ci-ok fails, so the limit must stand above the sum rather than at it.
The helm row adds seven steps to that sum and no minute, the head's own `install-helm` and the corrupt, install and report steps of cases 17 and 18, for the reason .github/ci-notes/ci-job-tool-fallback.md's "The job's budget, and the bound every install step carries" gives: its tarball's four reads, about 70 MB, are less than one Bun tarball of item (3), and its two fetches of get.helm.sh under the fault are shortened to about a second each.
IT WAS NOT DERIVED FROM THAT SUM: 114 was the 104 this construction gives with ten minutes added, the same ten the 75 before it carried over 65, when what had to fit inside it was the staging step's 5124 s, 86 minutes.
That step is now 12383 s, 207 minutes, whose derivation is beside it in `.github/workflows/ci-job-tool-fallback.yaml` - far above these 18 minutes of steps - and the same construction gives 207 + 18 = 225, and 235 with the ten.
The key carried 115, one minute above that construction, so that it also cleared the Budgets note's sum, then 143, one minute above the 142 the staging step's old 114 minutes gave, and it now carries 236, one minute above the 235, for the same reason; the paragraphs below add that sum up.
The sum is what runs beside that step, not the thing the limit is built from.
This budget replaced 65 minutes with a limit of 75, then carried 114, then 115, then 143 and now 236, and 45 of those original 65 were the twenty-one steps whose primary the fault holds, allowed at 126 s each.
That 126 s is what a negative control costs, because it drives two ladders;
the other fourteen of those steps drive one and cost about 64 s, so the old allowance was about twice what two thirds of them could cost.
What one ladder costs, and how it is derived, is in the comment above ci_tools_curl of .github/scripts/ci-tools.sh.
What the change did: job 129743677408 ran 1,994 s and job 129885098900 ran 284 s, and the one case that still drives the real ladder measured 63 s inside it, at the 60 s bound those jobs ran under. #3470 set this limit to 22 minutes on those durations alone, and a measured duration is only the first term of the rule below:
the second is the job's slowest retried step, which at 22 minutes would be cut before it could name its fetch - the gate refused that job six ways then, naming `bun-second-source`'s ladder, 2 x 1281 = 2562 s at this change's bound, against 1320 s, and since main's #4214 that ladder is 2 x 1708 = 3416 s.
So the durations above stand and the limit does not (dispatch://INFRA-1545).

The rule every other budget of `.github/workflows/ci.yaml` is written to (the Budgets note)
— a job carries its measured duration plus the arithmetic of its single slowest retried step
— is what the tool-fallback job's key now clears as well as its construction:
the job's 28.7-minute run, job 129478173535, is 1722 s; with the helm row's seven steps added at the 5 s they took in job 137926467879 (`install-helm` 3 s, and cases 17 and 18 2 s in all, in the merge-group run of main's #5152), 1722 + 5 = 1727 s; and 1727 s and the staging step's 12383 s come to 14110 s, 50 s under the 14160 s of the 236 minutes the key carries.
It carried 114, the construction this section gave at the staging step's 86 minutes, until review round 29 of dispatch://INFRA-1545 read that 6 s under the sum, and the one minute over the construction that closed it is the minute the key has carried over its construction since, 236 over 235 now, at no cost to any run that finishes.
What keeps the job's retried steps diagnosable is still their position, which the paragraph after this one gives.
The slowest retried step there is the staging step, whose twenty-nine ladders its own comment derives as 12383 s under the 207 minutes that step carries, above either blob read's 4397 s (one step per machine's five ghcr-mirror rows, as "The ci-images job's ceiling and the tool copy" says why), install-jj's 3416 + 450 = 3866 s, either setup-bun step's 3416 s and the ci-ok report's 2460 s.
The three minutes of steps this section counts for the job are all that runs before it
— measured at 0.3 minutes in job 129478173535, a 28.7-minute run of the job
— so a staging step that spends its whole ladder ends on its own budget at about 207 minutes with the job's limit unspent, and that step names the fetch that failed rather than the job reporting GitHub's bare timeout line.

THAT GUARANTEE HOLDS BY POSITION AND NOT BY RULE, which is worth saying because the arithmetic above does not give it.
A retried step is diagnosable only where its whole ladder fits before the job's limit, so a step whose ladder is N minutes has to START before minute 236 - N:
the staging step's 12383 s, 207 minutes, needs to begin before minute 29, and a 29-minute install starting after minute 207 would be cut by the limit and would report GitHub's bare line rather than its own.
What keeps every retried step of the tool-fallback job diagnosable is that the long ones run early:
the staging step first, and no retried step of job 129478173535 reached past 41.6 minutes.
A step added late with a ladder of its own has to be read against the job's limit rather than assumed safe by it.

THE STAGING STEP'S BUDGET IS PRICED BY A DECLARATION, which is worth its own sentence:
rule 8 reads a call of ci_tools_fetch as a call of that function and not as a call of the file defining it, so on its own it would charge the step the library once, 1708 s, and since round 37 of dispatch://INFRA-1545 it refuses a loop of such calls unless a declaration writes its figure;
the DECLARED_LIBRARIES entry in .github/scripts/check-download-retry.sh writes this step's 12383 s, so rule 8 holds the step's 207 minutes and the job's 236 above that sum, and the job's slowest step on its `--jobs` row is this one.
At the 120 s bound this change replaced, the same derivation gave twelve ladders of 247 s, 2964 s, and that step's key read 50;
nothing but the bound moved it, and test round 39 of dispatch://INFRA-1545 is what found it standing 36 minutes under its own worst case.
Before main's #4214 put a fourth ladder in each `ci_tools_fetch`, the same derivation at the 300 s bound gave twelve ladders of 427 s, 5124 s, and the step's key read 86 and the job's 115.
Until review round 36 of dispatch://INFRA-1545 the derivation counted five of the ten staged rows, sixteen ladders, 6832 s, and the step's key read 114 and the job's 143: elan, uv, pulumi-docker and pulumi-cli, staged since #3280, and helm, staged since main's #5152, were in no figure, and their thirteen ladders are what 12383 s adds.

## What the tool-fallback job's eleven-minute installs are derived from

THIS CHANGE ADDS TEN STEPS TO THAT SUM AND NINE COST NOTHING A READER WOULD NOTICE:
eight marks, one `echo` each before an install with nowhere to go, and the watch's report, all 0 s in jobs 130015112773, 129940828354 and 129966328215 and at most 1 s in job 130289341628.
The tenth is the watch's start, 3 s, 5 s and 4 s in those three jobs and 9 s in job 130306447638, which the step list in "The tool-fallback job's budget, part by part" allows for;
the watch itself is a sampler beside the job (dispatch://INFRA-2605).

SEVEN OF THE EIGHT INSTALLS WITH NOWHERE TO GO DECLARE `timeout-minutes: 11`, the eighth, the docker provider plugin's, 29, its one `ci_tools_fetch`'s 1708 s.
Every other install of this job and of tool-fallback-arm carries its own ladder's arithmetic instead
— 29 for one `ci_tools_fetch`, 57 for setup-bun's two, 65 for install-jj's two and its `mise install`, each the figure this change's 300 s ladder gives at a `ci_tools_fetch`'s four ladders, where the same three were 22, 43 and 51 at its three before main's #4214 and 13, 25 and 33 at the 120 s bound
— and the twenty-six installs whose control accepts a failure are timed as well, since a step the runner ends at its own bound leaves `outcome=failure` behind it and only the wall time tells a hang from the refusal the case expects.
THE ELEVEN MINUTES IS DERIVED FROM MEASUREMENT:
over the green runs of these two jobs between 2026-11-09T19:38Z and 2026-11-10T10:51Z, 2422 install steps gave a median of 64 s, a 99th percentile of 128 s and a maximum of 326 s (`install-jj` on five slow runs), and eleven minutes is twice that maximum rounded up to the minute, so no step that would have finished is ended by it.
Since the ladders stopped sleeping every install sits under that 99th percentile;
the slowest in job 130289341628 is the kept ladder's own `install-crane` at 65 s, that job's reading at the 60 s bound this branch replaced.
THE BOUND WOULD HAVE CHANGED NOTHING IN THE INCIDENT THAT ASKED FOR IT, because a bound the runner enforces needs a runner that is alive.
It is here for the other failure: a command that hangs while the runner answers, where without it the job's own ceiling is the only thing that ends it, and the ceiling costs the merge queue the pull request it holds.
That the runner does enforce it on a `uses:` step of a composite action is measured and not assumed:
on this change's own first run, an action sleeping 180 s under `timeout-minutes: 1` was ended after 72.5 s and the steps after it ran.
The job record of that step (job 129786363688, step 3) gives the wall time, 10:57:03Z to 10:58:16Z, and `conclusion: success`, and holds no `outcome` at all;
its log gives `The action has timed out.`;
and the `outcome=failure` the caller's later steps read was measured in job 129872208213, where a later step printed `steps.<id>.outcome`.
The runner is about twelve seconds late, so a bound is a ceiling and not a stopwatch.
contents: read is the two checkouts'; actions: read and checks: write are the ci-ok-report step's.
Every read of the package here is the anonymous one an install makes, so this job needs no packages scope at all.

## The stand-in curl, and the one case that keeps the pinned ladder

The stand-in curl every step after this one runs, which shortens only a fetch this job arranged to fail:
every case here is a refusal this rig arranged, and those refusals rode retry ladders nobody reads (job 129743677408 ran 1994 s and spent 1952 s in the steps that pay for them).
Every other fetch keeps the pinned ladder, so a flap of npm, PyPI, get.pulumi.com or the local origin — none of them faulted — is still ridden out.
No case reading counts requests or durations, so a shortened ladder takes no statement away:
a case proves which source served its bytes and that the digest held, and the ladder-report step is the one reading of durations.
ONE CASE KEEPS THE PINNED LADDER, the head's own install-crane below, and it drives the whole claim the shortened cases give up:
a real ladder, a second source, then the digest check.
A negative control cannot be that case, since a control ends in failure and proves no fall-through, and the head's installs cannot move above the controls, because .github/actions/setup-bun takes its existing-install branch when a Bun of the pinned version is already on the runner
— which is what the base-bun control must not find.
It runs here, immediately before the fault comes up, and the window it may run in is the one between `stage` and that step.
It cannot run above `stage`, because `stage` fetches about 303 MB from the real sources while the network is whole and must keep the pinned ladder for them, and because $GITHUB_PATH reaches the steps after the step that writes it and not that step itself.
It cannot stay straight after `stage` either, which is where it ran before:
the jj-primary case runs in that window, and its install-jj makes a real fetch of github.com through mise which succeeds.
github.com is one of the hosts this rig faults, so a stand-in installed above that case shortened a fetch this job means to succeed and took the pinned ladder off it, while the fault that makes the shortening true was not up yet.
In job 129841257239 that fetch is the first one the ladder-report step printed, `github.com: shortened, curl exit 0, 0s`:
2 attempts over 1 s where the pinned ladder gives 8 over 127 s under the `--retry-max-time 300` this branch writes (7 over 63 s when that job ran, at the 60 s bound), so one short failure of github.com would redden this required job where the pinned ladder rode it out.
The step after this one starts the fault, so every fetch made from here goes through it and no fetch this job did not arrange to fail is ever shortened.
The ladder-report step refuses the job for exactly that reading
— a fetch that was shortened and then served the bytes — which is what holds this step at this position.
The remedy is the position and not a guard in the stand-in on a state file the fault writes:
a position cannot drift out of the truth it states, while such a guard can.

