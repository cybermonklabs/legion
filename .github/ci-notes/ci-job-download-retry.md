# Notes for the download-retry lanes' steps

The derivations and the history of `.github/workflows/ci-job-download-retry.yaml`, which points
into this file with `# why:` lines. A value's own arithmetic stays beside the value. What the
jobs' rigs are and how they work is in `.github/ci-budgets.md`, "The download-retry job's rig" and
"The download-retry-images job's rig"; the second rig is the download-retry-images job's and the
download-retry-images-worker job's both.

## Each image build's budget: the gate's price for that build

The `--budget` each report step of the download-retry-images and download-retry-images-worker
jobs holds its build to has three terms.

The first is the price the gate itself puts on that build: its measured build time plus the
longest single ladder of the Dockerfile it builds, the two terms the price comment of the key
holding that build carries where the image is built for real
(`.github/scripts/check-download-retry.sh`, rule 10). A build that runs past the price its own key
puts on it still fails.

The second is the probe's 20 s. None of those keys holds the probe, which is this rig's own:
`report` measures a build from the moment its proxy started, and the probe's two requests are
made between the two, each bounded at 10 s by the rig's PROBE_BOUND. Every site of the
download-retry job carries the same 20 s.

The third is what the injected fault can add to a build that passes (dispatch://INFRA-1545, round
64, INFRA-1310's architect's rule): for each retried call of its Dockerfile, the sleeps of that
call's ladder, and the time its failed attempts took in this head's run. Each arm's proxy runs
`--per-host-window`, so each host its `--hosts` lists answers 504 for 60 s from that host's own
first request, wherever in the build that request comes, and is passed on after. Until round 63
one window ran from the proxy's start, and a host the build first reached after its first minute
met no fault at all: the worker image's seven model-catalog hosts, which the final RUN's `legion
probe-image` reaches about 529 s in, were listed and never faulted. What a window per host costs
a build:

- docker build stops at the first RUN that fails, so at most one ladder runs out, and the build
  has failed by then. The longest single ladder in the first term pays for that.
- A retried call that passes spends at most its ladder's sleeps, however many listed hosts it
  meets: 10 + 20 + 40 + 80 = 150 s for the inline `retry()`, and 1 + 2 + 4 + 8 + 16 + 32 + 64 =
  127 s for a curl that makes its own eight attempts (`--retry 7`: the worker image's mise
  download and its notices stage's 24 reads). Under `--mode recover` a faulted host answers 504
  at once, so a failed attempt costs the work it did before it met that host, not its
  per-attempt bound, and the attempt that passed is in the build's measured time already. So the
  term is every retried call's sleeps plus its failed attempts' time, read from BuildKit's own
  times for each RUN's output in run 45383555145 at fbcc6647 (jobs 138409597173 and
  138409597020): a call's next attempt starts at its failed attempt's last line plus the sleep,
  and each failed attempt's time is its last line less its start.
- The unit is the retried call, not the RUN line. One RUN line can hold several calls, each with a
  ladder of its own: the worker image's toolchain RUN (#45 of job 138409597020) slept
  150 + 4 x 70 = 430 s across its uv, Node, AWS CLI, Go and kubectl downloads and passed, and its
  runtime-base RUN (#16) slept 3 x 70 = 210 s, so 150 s a RUN line bounds neither. Every call is
  counted, whether this head's run saw it meet a fault or not, since which call first reaches a
  host turns on the order BuildKit runs the stages in.

Until round 64 this term was 70 s for each listed host the build reached, which held only for a
host a call meets on its first attempt: the ladder starts its attempts at 0, 10, 30, 70 and 150
s, so a host first met on a later attempt costs more, up to the whole ladder (review round 38 of
dispatch://INFRA-1545).

THE LIMIT OF A WINDOW PER HOST. A call that meets listed hosts one after another, each first on a
later attempt, spends its ladder on them in turn, where a real outage is one window: two such
hosts cost a call its spare attempt, and three fail its arm although its retry works. In run
45383555145 at fbcc6647 these calls met two listed hosts in turn and passed only on their last
attempt:

- the worker image (job 138409597020): the go-licenses install (#22: github.com, then
  sum.golang.org), the go-modules download (#24: the go-get hosts, then sum.golang.org and
  modernc.org), the gopls install (#25: golang.org, then sum.golang.org), mise's curl (#43, on its
  eighth attempt of eight: github.com, then release-assets.githubusercontent.com), the toolchain
  RUN's uv download (#45: the same two hosts) and the azure-cli wheels (#94: pypi.org, then
  files.pythonhosted.org);
- the held-port image's `go mod download` (#10 of job 138409597173: proxy.golang.org and the go-get
  hosts, then sum.golang.org);
- the Envoy image's go-licenses install (#30 of the same job: proxy.golang.org, then
  sum.golang.org).

Splitting the image builds per host would multiply builds that run up to an hour, so the limit
stays. Each report prints the requests it faulted by host, each with the second its first request
came, so a red arm whose call met three hosts in turn can be read for what it is.

The hosts below are the hosts each arm's `--hosts` lists that a RUN line of its Dockerfile
reaches, from round 61 and 62's table of every host the four builds fetch from, with the hosts
images job 138228770096 (at f3e50c3f) showed each proxy passing and the two run 45383555145
showed each proxy faulting besides. They are the evidence of what each arm faults, not a price:
the budgets count retried calls.

- The worker image, 64 hosts: deb.debian.org; registry.npmjs.org; github.com,
  objects.githubusercontent.com, release-assets.githubusercontent.com and
  raw.githubusercontent.com; mise.jdx.dev; nodejs.org; awscli.amazonaws.com; go.dev, pkg.go.dev
  and dl.google.com; dl.k8s.io and cdn.dl.k8s.io; storage.googleapis.com; releases.astral.sh; pypi.org
  and files.pythonhosted.org; www.postgresql.org and apt.postgresql.org; cdn.playwright.dev and
  playwright.download.prss.microsoft.com; azcliprod.blob.core.windows.net; proxy.golang.org,
  sum.golang.org, golang.org and google.golang.org; go.googlesource.com; cloud.google.com;
  dl-cdn.alpinelinux.org; registry.yarnpkg.com; the go-get hosts authelia.com and
  www.authelia.com, cel.dev, codeberg.org, dario.cat, dmitri.shuralyov.com, filippo.io,
  git.sr.ht, go.etcd.io, go.opencensus.io and contrib.go.opencensus.io, go.opentelemetry.io,
  go.uber.org, go.yaml.in, go4.org, gonum.org and www.gonum.org, gopkg.in, gotest.tools,
  honnef.co, k8s.io and sigs.k8s.io, modernc.org, mvdan.cc and rsc.io; gitlab.com; and Oh My
  Pi's model catalog, catalog.stencil.so, api.commandcode.ai, api.kilo.ai, api.venice.ai,
  coding-intl.dashscope.aliyuncs.com, hyper.charm.land and zenmux.ai. www.authelia.com is where
  authelia.com's go-get page sends the go command (the Envoy arm's proxy passed both), and
  www.gonum.org is where gonum.org's does. pkg.go.dev is the one the go.dev entry faulted in job
  138409597020 beside go.dev, first at 159.5 s.
- The Envoy image, 24 hosts: registry.npmjs.org; deb.debian.org; proxy.golang.org,
  sum.golang.org, golang.org and google.golang.org; storage.googleapis.com; github.com, which its
  proxy passed ten requests to in job 138228770096; and the go-get hosts authelia.com and
  www.authelia.com, cloud.google.com, dario.cat, dmitri.shuralyov.com, go.opencensus.io,
  go.opentelemetry.io, go.uber.org, go.yaml.in, gonum.org and www.gonum.org, gopkg.in,
  gotest.tools, honnef.co, k8s.io and rsc.io.
- The held-port image, 30 hosts: proxy.golang.org, sum.golang.org, golang.org and
  google.golang.org; storage.googleapis.com; and the go-get hosts authelia.com and
  www.authelia.com, cel.dev, charm.land, cloud.google.com, codeberg.org, dario.cat, git.sr.ht,
  go.mongodb.org, go.opencensus.io and contrib.go.opencensus.io, go.opentelemetry.io, go.uber.org,
  go.yaml.in, gocloud.dev, gonum.org and www.gonum.org, gopkg.in, gotest.tools, honnef.co,
  lukechampine.com, modernc.org, mvdan.cc, pgregory.net and rsc.io. Its golang image carries no
  git, so it clones nothing. contrib.go.opencensus.io's go-get page is one its `go mod download`
  asked for after a proxy error in job 138409597173, first at 87.7 s.
- The browser image, 5 hosts: www.postgresql.org and apt.postgresql.org;
  azure.archive.ubuntu.com and security.ubuntu.com; deb.nodesource.com.

The four budgets, each call's sleeps and its failed attempts read from run 45383555145's two
images jobs as the third term above says:

- The worker image: the key of `.github/workflows/worker-image.yaml`'s `build` job, which holds
  that build and no other fetch but the ci-ok report: 1800 s, the 30 minutes the job carried,
  which holds its build, plus the `go` stage's `go mod download` of
  packages/daemon/docker/worker.Dockerfile, 5 x 900 + 150 = 4650 s. That key names no measured
  build of its own, so its 1800 s is the measured term here too. Its Dockerfile makes 39 inline
  retries in 17 RUN lines and 25 curl ladders in two (mise's tarball, and the notices stage's 17
  reads and 7 HEAD checks), and their failed attempts took 337.0 s in job 138409597020, 338 s
  rounded up: the go-modules download's four, 133.2 s; the plugin workspace install's one,
  56.8 s; the runtime-base RUN's apt and pgdg calls, 45.9 s; the three other `apt-get update`
  calls' three each, 64.2 s; the azure-cli wheels' four, 21.5 s; the WebKit download's three,
  8.1 s; and 7.3 s across the rest. 1800 + 4650 + 20 + 39 x 150 + 25 x 127 + 338 = 15833 s.
- The Envoy image: the key of `.github/workflows/ci-job-envoy-smoke.yaml`'s job, whose comment
  names this build's own 203 s (job 133743044741), plus the `bun install` and the `go mod
  download` of packages/envoy/docker/Dockerfile at 5 x 600 + 150 = 3150 s each. Its five retried
  calls are in four RUN lines (`apt-get update` and `apt-get install`, `bun install`, the
  go-licenses install and `go mod download`), and their failed attempts took 139.7 s in job
  138409597173, 140 s rounded up: the `bun install`'s one, 108.5 s, which bun's own retries of
  each tarball kept running across the window; the `apt-get update`'s three, 21.5 s; `go mod
  download`'s three, 9.3 s; and the go-licenses install's four, 0.5 s.
  203 + 3150 + 20 + 5 x 150 + 140 = 4263 s.
- The held-port image: the same key, whose comment names this build's 11 s in the same job, plus
  the wrapper stage's `go mod download` of packages/envoy/infra/host/heldportwrapper/Dockerfile,
  5 x 600 + 150 = 3150 s. That `go mod download` is its one retried call, and its four failed
  attempts took 28.7 s, 29 s rounded up. 11 + 3150 + 20 + 150 + 29 = 3360 s.
- The browser image: the key of `.github/workflows/browser-image.yaml`'s "Build and push" step:
  66 s, the slowest of the image's 33 cold builds measured in that job (job 128991520632 of run
  42565616054), plus the package set's `apt-get install` of .github/docker/browser.Dockerfile,
  5 x 300 + 150 = 1650 s, above the signing key's and the index read's 750 s each. Its one RUN
  line makes three retried calls, the signing key, `apt-get update` and `apt-get install`, and
  their failed attempts took 21.3 s, 22 s rounded up: the update's three, 21.2 s, and the key's
  three, 0.2 s. 66 + 1650 + 20 + 3 x 150 + 22 = 2208 s.

The four builds do not fit one job. With its 300 s of base image pulls, the 900 s smoke build
and about 200 s of checkout, bypass clearing and rig steps, the one job would come to
1400 + 15833 + 4263 + 3360 + 2208 = 27064 s, and with the single slowest retried step that the
Budgets note adds, the worker image's 4650 s ladder, to 31714 s, above the 360 minutes GitHub
gives a job on a hosted runner. So the worker image's build runs in a job of its own,
download-retry-images-worker, at 200 + 15833 = 16033 s and 16033 + 4650 = 20683 s, 345 minutes;
the other three stay in download-retry-images, at 1400 + 4263 + 3360 + 2208 = 11231 s and
11231 + 3150 = 14381 s, 240 minutes, where 3150 s is the longest single ladder of the three
Dockerfiles that job builds. Each key is the whole minute strictly above its sum, and each job's
comment derives it.

The Envoy and held-port arms take each build's own measured time, which envoy-smoke's key names,
rather than the 1200 s that key carries for its whole job: that term holds both builds and the
job's tests together, so charging it to each arm would count it twice.

Until job 136575882603 of the run at 95587c62 failed, these arms carried 2720, 920, 500 and 384 s,
each set by hand from one measured build and a margin. The browser's was 100 + 20 + 264 = 384 s:
the recovering fault lets the first attempt through no later than 100 s after the proxy starts,
the probe's 20 s, and four times the 66 s build, about the margin the Envoy image's 920 s kept
over its 239 s faulted build in job 133724372176. In job 136575882603 the browser arm took 915 s:
the injected fault on the PostgreSQL signing key recovered on its fourth attempt, then
azure.archive.ubuntu.com, outside the injected fault, served at 121 kB/s, so two `apt-get install`
attempts reached their 300 s bound and the third succeeded. The retry held; the budget had assumed
that every fetch after the fault meets a healthy host. That job's four arms took 540, 272, 130 and
915 s, each under the budget above.

From then until round 63 they carried the first two terms alone, 6470, 3373, 3181 and 1736 s, and
one job held the four builds at 347 minutes: 16160 + 4650 = 20810 s. In images job 138228770096
(at f3e50c3f) the four took 607, 264, 132 and 99 s under one window from each proxy's start.
Round 63 added 70 s for each listed host the build reached, 10880, 5053, 5211 and 2086 s, at
263 and 282 minutes for the two jobs, and in jobs 138409597020 and 138409597173 (at fbcc6647)
the four took 1024, 362, 213 and 192 s under a window per host.

## The sites lane's arms with a window per host

install-changie, install-omp and the Go module download each run once per outage of their call's
chain (dispatch://INFRA-1545, round 64, INFRA-1310's architect's ruling), each run faulting that
outage's host in a way only the call's retry passes:

- install-changie and install-omp: github.com; and release-assets.githubusercontent.com, where the
  release asset's address on github.com redirects. Each run lists its one host under
  `--per-host-window`.
- the Go download. With `|direct` in GOPROXY a go command falls through to the module's own source
  after an error, so a run whose direct sources answered would pass with no retry at all (check
  #0946ed09). The proxy.golang.org run faults every host in one window from the proxy's start,
  as recover-go did before round 63: the proxy and every direct source are down together for
  60 s, sum.golang.org among them, and storage.googleapis.com, first reached after that window,
  has a run of its own. The storage.googleapis.com run lists the zips' host and the direct
  sources of packages/envoy's modules under `--per-host-window`: the module path hosts of its
  go.sum and the workspace's, and the repository hosts their go-get pages name (github.com,
  go.googlesource.com, gitlab.com).
  A module's direct host is first asked a second or so after its zip's 504, in the same attempt,
  so the windows overlap into one outage. golang.org cannot be listed without faulting
  proxy.golang.org and sum.golang.org, so go.googlesource.com, where its modules' repositories
  are, stands for it; a module whose direct host is not listed is fetched directly, and the call
  still needs its retry for the others.
- No run faults sum.golang.org alone (round 65, INFRA-1310's architect's ruling). This download
  runs on a complete go.sum, and with proxy.golang.org answering it asks no checksum database:
  round 64's sum.golang.org run, in job 138666740400 of run 45455967236 (at e3e586f3), after the
  step before it emptied the module cache, passed 4 tunnels to proxy.golang.org and 1 to
  storage.googleapis.com, refused nothing, and passed on attempt 1, so its three checks failed it
  as built. sum.golang.org is asked only after a `|direct` fall-through, which the
  proxy.golang.org run already faults, so an outage of sum.golang.org alone cannot fail this
  download. The round-64 text here said go asks it about a module go.sum does not hold, which is
  true and is not this download's case: a run that fetched such a module would drive a download
  CI does not make.

Round 63 faulted each chain's hosts together, each in a window of its own, after job 138228769946
of run 45329962650 (at f3e50c3f) showed one window from the proxy's start closing before the call
first reached its second host: release-assets.githubusercontent.com at +68 s and +75 s, and
storage.googleapis.com and sum.golang.org at +80 s, none of them faulted. But a call that reaches
each host only once the last has passed then meets the windows in a series. In job 138409596939
of run 45383555145 (at fbcc6647) recover-go met three in a row, proxy.golang.org from +5.1 s,
sum.golang.org from +90.9 s and storage.googleapis.com from +167.4 s, and passed on attempt 5 of
5 with none to spare; recover-omp passed only after "attempt 4 of 5 failed", and recover-changie's
curl on its eighth attempt of eight. A real outage is one window, and one host a run is one
outage, under which each ladder keeps attempts to spare: changie's curl, its attempts at 0, 1, 3,
7, 15, 31 and 63 s from its first, passes on its seventh of eight once a 60 s window from its
first request has closed, and install-omp's retry_fetch, at 0, 10, 30 and 70 s, on its fourth of
five.

Each run's window opens on its site's own first request to its host, not on the probe's: under
`--per-host-window` the probe asks for `download-retry-probe.<host>`, a name the entry covers and
no site fetches (.github/scripts/download-retry-proof.sh, `probe`), and the proxy keys a window by
the host a request names. Each run's report checks two things beyond what every recover arm
checks (the same script's `report`), and fails naming which:

- the faulted-host check: at least one of the site's own requests for its one host was refused,
  the probe's not counted, so a run whose site met no fault fails;
- the attempt check: the call passed on a retry and before its last attempt, so a call that
  passed at once ("the call passed with no retry") fails, and so does one that passed on its last.
  Which attempt passed is read from the retry helper's own attempt log, which `start
  --attempt-log` names and the site's step passes on as CI_RETRY_ATTEMPT_LOG: retry_fetch writes
  it for install-omp's curl and the Go download (.github/scripts/ci-retry.sh), and ci-tools.sh's
  ci_tools_curl for the eight attempts install-changie's curl makes itself, from curl's own line
  for each attempt that failed. Neither is inferred from the proxy's log.

Each run's budget is its ladder's sleeps plus the call measured at fbcc6647, plus the probe's
20 s, as every site carries it (.github/ci-budgets.md, "The download-retry job's rig"). The
measured call is the arm's time from its proxy's start less the sleeps its ladder took before the
attempt that passed, which the helper's own lines count: one `curl: (22)` line, or one "attempt
n of 5 failed" line, for each attempt that failed.

- install-changie: recover-changie ran 133 s and its curl failed seven attempts, which slept
  1 + 2 + 4 + 8 + 16 + 32 + 64 = 127 s, so the call took 6 s. Each of its two runs:
  127 + 6 + 20 = 153 s.
- install-omp: recover-omp ran 158 s and failed four attempts, which slept 10 + 20 + 40 + 80 =
  150 s, so the call took 8 s. Each of its two runs: 150 + 8 + 20 = 178 s.
- the Go download: recover-go ran 290 s and failed four attempts, which slept 150 s, so the call
  took 140 s. Each of its two runs: 150 + 140 + 20 = 310 s. That arm faulted every host, each
  in a window of its own, so it is the measured call of both runs, and the change of round
  64 to how each run faults its hosts leaves the figure where it was.

The steps' own keys are unchanged, and each is above its runs' budget, so a run's report can now
fail it first: install-changie's 29 minutes, install-omp's 8 and the Go download's 18.
Between the Go runs a step empties the module cache, so no run reads what an earlier one fetched.
The job's key moves with the recover half's sum, from 7020 s to 5962 s in round 64 and to 5652 s
in round 65, which dropped the sum.golang.org run: .github/workflows/ci-job-download-retry.yaml's
comment above the job's id derives 13342 + 3416 = 16758 s and 280 minutes, where it read 303
before round 64 and 285 after it.

## Report verdict

Each report uses `continue-on-error: true` so a failed report does not skip later arms.
GitHub Actions includes only steps with an `id` in the `steps` context. Each report has an
ID of the form `report-<arm>`, where `<arm>` is its proxy name. The verdict checks all steps
with that prefix. It reads `outcome`, the result before `continue-on-error` changes a failed
report's `conclusion` to `success`.

The verdict does not count failures of the tested downloads or image builds directly.
A stalled download is expected to fail, and each report checks the site's outcome against
what its fault mode requires. Proxy start, probe, and cache clearing do not use
`continue-on-error`. A setup failure must fail the job and stop dependent steps.

Proxy cleanup runs even after a failure. It stops proxies a report did not stop and checks
that no proxy reaches the ci-ok report or its artifact upload. The only proxy-related entries
the jobs write to `$GITHUB_ENV` are the empty `no_proxy` and `NO_PROXY` values. Cleanup accepts
those empty values and rejects nonempty proxy or bypass values.

The verdict runs after cleanup unless the job is cancelled. The final step sends
`job.status`, which includes the verdict, to the caller's ci-ok reporter. The `ci-ok-changes`
input carries `ci.yaml`'s `toJSON(needs.changes)`. The reporter can post a failing ci-ok
immediately in a merge group, but neither lane runs in merge groups.
