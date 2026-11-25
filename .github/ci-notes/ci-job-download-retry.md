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
coalesced outages, `--coalesce-outage-gap <G>` (the section "The image arms' outages and their
coverage" below): the first request of a listed host opens an outage of 60 s, and the hosts a call
meets in turn join it or are passed on after it, wherever in the build their first request comes.
What the fault costs a build:

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

The builds do not fit one job. With its 300 s of base image pulls, the 900 s smoke build and about
200 s of checkout, bypass clearing and rig steps, one job holding the Envoy, held-port, browser
(twice) and worker builds would come to 1400 + 15833 + 4263 + 3360 + 2208 + 2208 = 29272 s, and
with the single slowest retried step that the Budgets note adds, the worker image's 4650 s ladder,
to 33922 s, above the 360 minutes GitHub gives a job on a hosted runner. So the worker image's
build runs in a job of its own, download-retry-images-worker, five times: the five runs of its
matrix are five runner jobs, so its one key bounds one run, at 200 + 15833 = 16033 s and
16033 + 4650 = 20683 s, 345 minutes. The other builds stay in download-retry-images, at
1400 + 4263 + 3360 + 2208 + 2208 = 13439 s and 13439 + 3150 = 16589 s, 277 minutes, which was 240
(11231 + 3150 = 14381 s) until round 65 added the browser image's second run; 3150 s is the longest
single ladder of the three Dockerfiles that job builds. Each key is the whole minute strictly
above its sum, 16589 s being 276.48 minutes and 20683 s 344.72, and each job's comment derives it.

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

## The image arms' outages and their coverage

THE COALESCED OUTAGE. Each image arm's proxy runs `--coalesce-outage-gap <G>`
(.github/scripts/ci-fault-proxy.py's header). The first request of a listed host opens an outage
of 60 s, and that host answers 504 until it ends. A host first reached while the outage is open
joins it and answers 504 until it ends too. A host first reached less than G s after it ended is
the same call going on, and is passed on. The first host reached G s or more after an outage
ended opens the next one. A host's first request decides it for the whole run: no later request
faults it again or extends an outage.
History, in one sentence: round 63 gave each host a window of its own, and round 65 replaced it
because a call that met listed hosts in turn spent its ladder on them, as the held-port image's
`go mod download` (RUN #10) did when it exhausted at e3e586f3 in job 138666740472.

A real outage is one stretch of time, and a call that comes through one asks for its later hosts
at most its ladder's last sleep and one attempt's work after the outage ended, so G is that: 80 s,
the last sleep of the inline `retry()`'s 10, 20, 40 and 80 s, plus the longest single attempt of a
retried call measured in that arm's own builds, rounded up to the next whole 10 s, and the
rounding is the margin. A host first reached later than that is some other call's, and may open
the next outage. Each arm's G:

- the Envoy image, 190 = 80 + 110 s: its `bun install`'s failed attempt took 108.5 s in job
  138409597173 (at fbcc6647), longer than its go-licenses install's passing attempt of 92 s in job
  138666740472 (at e3e586f3), and 110 s is 108.5 s rounded up;
- the held-port image, 290 = 80 + 210 s: its `go mod download`'s fifth attempt ran from about
  172.5 s to 373.5 s, about 201 s, in job 138666740472, which 210 s rounds up;
- the browser image, both runs, 380 = 80 + 300 s: two of its `apt-get install` attempts ran to
  their 300 s bound in job 136575882603, a whole 10 s already;
- the worker image, every run, 230 = 80 + 150 s: the go-modules stage's `go mod download`'s
  attempt 4 ran from 78.1 s to 224.3 s, 146.2 s, in job 138666740482 (at e3e586f3), which 150 s
  rounds up.

THE COVERAGE CRITERION. Every RUN line of an arm's Dockerfile that reaches a listed host meets a
faulted request in some run of that arm, but for the seven lines THE STATED LIMIT below names. It
is read from each build log's attempt lines: `retry: <what>: attempt <k> of 5 failed` for an
inline `retry()`, a `curl: (N)` line for a curl that makes its own attempts, or a bare tool's 504
line. Each report prints the outages its proxy opened and, for each host it decided, the second of
that host's first request and the outage it joined or `not faulted: after outage <N>, within <G>
s`, so the hosts it left within G are named, and a RUN line that reached a listed host and met no
fault can be found. The criterion counts RUN lines and not calls: a RUN line that makes several
calls meets a fault when one of them does.

THE HOSTS EACH ARM LISTS. They are the hosts each arm's `--hosts` lists that a RUN line of its
Dockerfile reaches, from round 61 and 62's table of every host the four builds fetch from, with
the hosts images job 138228770096 (at f3e50c3f) showed each proxy passing and the two run
45383555145 showed each proxy faulting besides. They are the evidence of what each arm faults,
not a price: the budgets count retried calls.

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
  138409597020 beside go.dev, first at 159.5 s. Run 1 lists all of them, and runs 2 to 5 the
  clusters THE FIVE WORKER RUNS names.
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
  azure.archive.ubuntu.com and security.ubuntu.com; deb.nodesource.com. Run 1 lists all five
  (`postgresql.org,ubuntu.com,nodesource.com`) and run 2 the four apt hosts without
  www.postgresql.org (`apt.postgresql.org,ubuntu.com,nodesource.com`).

Why each arm lists them. The Envoy and held-port images list the module hosts a go command falls
through `|direct` to after a proxy error: the go-get page of each module path's host that the
go.sum files of their Go fetches name (the Envoy image's two, packages/envoy's and go-licenses
v2.0.1's, which scripts/go-third-party-notices.sh installs; the held-port image's, those of
packages/envoy/infra's workspace: its go.sum and go.work.sum, and packages/envoy's), where
golang.org already holds google.golang.org. Their golang image carries no git, so the fall-through
reads those pages and clones nothing (dispatch://INFRA-1545, round 61). The Envoy image's notices
RUN's go-licenses report reads the same pages unretried, and a page it cannot read leaves only
that module's license URL unknown, which the notices do not print. proxy.golang.org serves its
module zips from storage.googleapis.com (googleapis.com), which images job 138228770096 showed
these builds' Go fetches reaching unfaulted (round 62). The browser image lists Ubuntu's apt mirror,
which its base's sources name (azure.archive.ubuntu.com and security.ubuntu.com), PostgreSQL's
signing key and apt repository, each behind one of the Dockerfile's inline retries, and
NodeSource's apt repository, deb.nodesource.com (nodesource.com), whose index the same retried
`apt-get update` reads: the base's own build wrote /etc/apt/sources.list.d/nodesource.list for its
Node 26 and left it there (round 61).

The worker image's hosts are apt, the npm registry, the Go module proxy and the release hosts of
mise, uv, Node, the AWS CLI, Go, kubectl, yq and Chromium, each behind one of the Dockerfile's
inline retries, and since main's #4377 four more, each behind one too: the Google Cloud SDK's
archive and component list from dl.google.com (google.com), uv's CPython from releases.astral.sh
(astral.sh; its fallback is github.com), azure-cli's index and wheels from pypi.org and
files.pythonhosted.org (pythonhosted.org), and Playwright's WebKit from cdn.playwright.dev and
playwright.download.prss.microsoft.com (playwright.dev, microsoft.com). windows.net is the one
host here no retry stands behind: `az version` reads the newest azure-cli versions from
azcliprod.blob.core.windows.net and exits 0 without them, as the Dockerfile's toolchain step
says, so a build that meets the fault there passes. Three kinds more, each behind a retry too
(round 61): go-base's `apk add --no-cache git`, which reads Alpine's index and git's packages from
dl-cdn.alpinelinux.org (alpinelinux.org); the corepack shims' default Yarn, 1.22.22, which Node
24.22.1's corepack 0.36.0 fetches from registry.yarnpkg.com under COREPACK_DEFAULT_TO_LATEST=0
(its default pnpm, 11.24.0, comes from the npm registry); and the module hosts a go command falls
through `|direct` to after a proxy error. Those are the go-get page of each module path's host
that the go.sum files of the build's three Go fetches name (go.work's modules, go-licenses v2.0.1
and gopls v0.23.0): authelia.com, cel.dev, codeberg.org, dario.cat, dmitri.shuralyov.com,
filippo.io, git.sr.ht, go.etcd.io, go.opencensus.io (contrib.go.opencensus.io with it),
go.opentelemetry.io, go.uber.org, go.yaml.in, go4.org, gonum.org, gopkg.in, gotest.tools,
honnef.co, k8s.io (sigs.k8s.io with it), modernc.org, mvdan.cc and rsc.io, where google.com and
golang.org already hold cloud.google.com and google.golang.org; and, since go-base carries git,
the repositories those pages name off github.com and go.googlesource.com: gitlab.com,
modernc.org's, and the ones codeberg.org, git.sr.ht, dmitri.shuralyov.com and gopkg.in serve
themselves. The go-notices stage's go-licenses report reads the same pages unretried, and a page
it cannot read leaves only that module's license URL unknown, which the notices do not print. And
Oh My Pi's model catalog, which images job 138228770096 showed passing (round 62):
api.commandcode.ai (commandcode.ai), api.kilo.ai (kilo.ai), api.venice.ai (venice.ai),
catalog.stencil.so (stencil.so), coding-intl.dashscope.aliyuncs.com (aliyuncs.com),
hyper.charm.land (charm.land) and zenmux.ai. They are Oh My Pi's background model refresh in the
sessions the final RUN's `legion probe-image` starts. It is a fetch at all because, in the Oh My
Pi that .omp-pin names, `createAgentSession` (packages/coding-agent/src/sdk.ts) calls
`modelRegistry.refreshInBackground()` when its caller passes no registry, and main.ts calls it
after each non-interactive session it starts; that refresh
(packages/coding-agent/src/config/model-registry.ts) is online-if-uncached, so it fetches
models.dev's catalog from catalog.stencil.so (`MODELS_DEV_URL`,
packages/catalog/src/provider-models/openai-compat.ts) and the model lists of the providers that
need no key. Neither call reads a setting or an environment value that turns the refresh off, and
a refresh that fails is logged as the warning "background model refresh failed" and fails
nothing.

THE BROWSER'S TWO RUNS. Its one RUN line makes three retried calls: the signing key's download
from www.postgresql.org, `apt-get update` and `apt-get install`. In run 1 the signing key's host
is the first listed host reached, at 5.4 s in job 138409597173, so it opens outage 1, which ends at
65.4 s. The apt hosts are first reached at 76.4 s, about 11 s after that, inside G, so they are
passed on and run 1 drives only the key. Run 2, the `browser-apt` arm, lists the apt hosts alone,
so `apt-get update` opens an outage of its own and the apt calls are driven.

THE FIVE WORKER RUNS. The worker image is built five times, once for each run of the matrix of
download-retry-images-worker, each run a job of its own. A stage runs its RUN lines in order, and
the RUN that meets an outage retries through it, so only a run's first outage lands where an
unfaulted build reaches the host: every fault before a RUN moves the start of each RUN after it,
and later outages move with every fault before them. A run that lists one cluster of hosts alone
opens its first outage at that cluster's first request, where the seconds below put it, predicted
from the RUN spans of job 138666740482 and the uncached spans of worker-image job 138666741929
(both at e3e586f3). That is why each cluster has a run of its own, but for the model catalog's
hosts, which THE STATED LIMIT names. The runs and the RUN lines each targets, with the second its
first request comes in a clean no-cache build:

- Run 1, `worker`, the job named download-retry-images-worker: the first-minute cluster,
  deb.debian.org and dl-cdn.alpinelinux.org, which the four apt RUNs of plugin-deps, tools,
  toolchain and runtime-base and go-base's `apk add` reach. It lists every host any run lists.
  Among them are registry.yarnpkg.com, which THE STATED LIMIT says no run reaches, and the 7
  hosts of runtime 40/40's `legion probe-image`, which it says no run faults.
- Run 2, `worker-2`, about 10-20 s in: go, npm, github and the toolchain downloads, which go-licenses
  2/2, go-modules 4/4, gopls 1/1, tools 3/5's mise curl, plugin-deps 19/26's bun install and
  toolchain 4/7's downloads and `npm ci` reach. It lists npmjs.org, github.com,
  objects.githubusercontent.com, release-assets.githubusercontent.com, jdx.dev, nodejs.org,
  awscli.amazonaws.com, go.dev, dl.k8s.io, googleapis.com, golang.org, googlesource.com and
  google.com, and the 22 go-get hosts and repository hosts authelia.com, cel.dev, codeberg.org,
  dario.cat, dmitri.shuralyov.com, filippo.io, git.sr.ht, gitlab.com, go.etcd.io,
  go.opencensus.io, go.opentelemetry.io, go.uber.org, go.yaml.in, go4.org, gonum.org, gopkg.in,
  gotest.tools, honnef.co, k8s.io, modernc.org, mvdan.cc and rsc.io.
- Run 3, `worker-3`, about 55-100 s in: notices 4/23's fetches, from raw.githubusercontent.com.
- Run 4, `worker-4`, about 116 s in: azure-cli's toolchain 6/7, from astral.sh, pypi.org and
  pythonhosted.org.
- Run 5, `worker-5`, about 204-250 s in: webkit-check 2/2, from playwright.dev and
  microsoft.com. It lists windows.net too, for runtime 26/40's `az version`, which stays bare (the
  Dockerfile says why) and which no run faults: run 5 first reaches
  azcliprod.blob.core.windows.net at 313.9 s in job 138996340699, within 230 s after its outage.

THE STATED LIMIT. Seven lines meet no fault in any run. No run can fault five of them: plugin-deps
23/26 and 26/26 (the second and third `bun install`; registry.npmjs.org is always first reached,
so decided, by 19/26), toolchain 5/7 (the Cloud SDK; dl.google.com is decided by 4/7's Go tarball
through go.dev's redirect), tools 5/5 (its omp, jj and gh fetches and `mise --version`; github.com
and release-assets.githubusercontent.com are decided by 3/5's mise curl or 4/7's uv, and
mise.jdx.dev by 5/5 itself after that outage) and runtime 26/40 (corepack's pnpm on
registry.npmjs.org). Each is a later RUN line on a host an earlier RUN line of the same build
already met in its outage, so a fault keyed to a host cannot single it out, and each uses the same
inline `retry()` that other lines prove under fault. The sixth is the Envoy image's build 6/12,
its `go mod download`, and unlike those five it is coverable. Build 4/12's go-licenses install
decides proxy.golang.org first, at 5.7 s in outage 1 of job 138996340640, and the download's other
hosts are first reached within 190 s after that outage ends (sum.golang.org at 76.0 s,
storage.googleapis.com at 99.9 s and its go-get hosts from 125.3 s), so none is faulted. An Envoy
run that listed storage.googleapis.com and those go-get hosts alone would open its outage at the
download's first request there and fault it. INFRA-1310's architect ruled at 07:29Z on 2026-11-25
that the arm adds no such run: it would run every lane for about two hours more and add its
build's 4263 s to download-retry-images' key, to prove the inline `retry()` that other RUN lines
prove under fault, the same `go mod download` among them in the held-port image's wrapper 4/4 and
the worker image's go-modules 4/4. The last run in which this line met a fault is round 63's run
45383555145 (job 138409597173, at fbcc6647), whose window per host faulted each Envoy host from its
own first request: build 6/12 failed three attempts, go's fall-through lines at 9.1, 19.2 and
39.3 s into the step, and passed on the fourth, done at 84.9 s. The seventh is the worker image's
runtime 40/40, `legion probe-image`, on commandcode.ai, kilo.ai, venice.ai,
stencil.so, aliyuncs.com, charm.land and zenmux.ai. What it fetches from them is Oh My Pi's
background model refresh, which is no retried call and fails nothing (the paragraph on the worker
image's hosts above cites the code). So a run that faults those hosts alone drives no retry, and
the report's attempt check refuses it: the build log holds no attempt line and no `curl: (N)`
line at all (.github/scripts/download-retry-proof.sh, `report`). Until INFRA-1310's architect's
ruling at 05:59Z on 2026-11-25, a sixth worker run listed those 7 hosts alone. In job 138996340757
of run 45551940814 (at c9788e014f22) it faulted all 7 hosts, 8 requests in all, in one outage
from 246.5 s to 306.5 s. The build reached its last stage, and the report failed on the attempt
check alone. That ruling removed the run and keeps the 7 hosts in run 1's `--hosts`. In run 1 (job
138996340654 of run 45551940814) they are first reached at 395.6 s, after outage 2 (305.0 s to
365.0 s) and within 230 s of its end, so they are not faulted.
registry.yarnpkg.com stays in run 1's `--hosts` by INFRA-1310's architect's ruling at 04:17Z
although no run reaches it: neither by-host list of job 138666740482 names it. And if a real
attempt runs longer than measured, a recovering call can meet a fresh outage, and the last-attempt
check then reds it.

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
ID of the form `report-<arm>`, where `<arm>` is its proxy name, but for the five runs of the
worker job, which are one job's steps and so share `report-worker` and name their proxies by the
matrix's `run`. The verdict checks all steps with that prefix. It reads `outcome`, the result
before `continue-on-error` changes a failed report's `conclusion` to `success`.

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
