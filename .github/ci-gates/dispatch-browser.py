# The browser suite, every row of it: the binaries job, the shard legs, the three `-alone`
# projects, the two-process job, the visual job and the merge job, all in
# dispatch-browser.yaml, which ci.yaml's dispatch-browser job and the nightly both call
# (at_browser cites their steps).
#
# ci_filter_coverage.py's check_table reads a gate whose ci.yaml job calls a local workflow
# over the jobs of that workflow, so dispatch-browser.yaml's go commands, `env:` blocks and
# `shell:` keys are held here exactly as a job's own steps are: the binaries job's build
# step and the multiprocess job's are the exact entries below, and a name outside
# ENV_ALLOWED, or a `shell:`, in either job or in the file around it drops that step out of
# the check's reduction, which then counts it as reading every Go file of the repository.
# The gate keeps `workflow=CI`, as the docs gate does and for the same reason:
# dispatch-browser.yaml carries job ids of its own (binaries, shard, alone, multiprocess,
# merge) rather than one named for the caller, so ci.yaml is the file that holds a job named
# dispatch-browser, and each citation names the browser job it is in.
#
# This gate lists no script Run, and cannot: a Run pinned to a script's text is reduced only
# where nothing around the ci.yaml job can move what the script compiles, and around_job
# reads that over every job of the called workflow. The shard and alone jobs run in a
# container (dispatch://INFRA-1443), which shell_reason counts as exactly such a thing, so a
# script Run here would fall to reading every Go file of the repository. It costs this
# filter nothing. Those two jobs run no go command at all - run-server.sh and run-broker.sh
# execute the binaries the first entry below builds - and the scripts the multiprocess job
# and the visual job still start compile closures this filter already carries:
# run-server.sh, which both of them start, builds ./cmd/dispatch, which the multiprocess
# job's own pinned build step below names, and run-signin-issuer.sh builds
# ./cmd/dev-oidc-issuer, which that first entry names. No job's e2e/harness-postgres.ts runs
# a go command in CI: each job that starts stacks there hands it the connection limit that
# a pinned step below reads.
GATE = gate("dispatch_browser", BROWSER_JOB, exact=[
    # The four commands the shard and alone jobs' scripts built before those jobs moved
    # into a container, built once in the binaries job and downloaded by both: this is the
    # closure that build compiles and no other. The step names an output (-o) and pins
    # CGO_ENABLED on the line, neither of which the simple form reduces, so the step is
    # pinned whole; the Run carries that CGO_ENABLED, since the closure go lists without
    # cgo is the one this build compiles.
    pinned_step(BROWSER_BINARIES, BROWSER_BINARIES_STEP, ENVOY, BROWSER_BINARIES_SCRIPT, [
        (ENVOY, ["./cmd/dispatch"] + BROKER_PKGS,
         "CGO_ENABLED=0 go build of the Dispatch server, the real broker, its dev OIDC issuer and the seeding step",
         {"mode": "build", "env": {"CGO_ENABLED": "0"}}),
    ], workflow=BROWSER_SITE),
    # The stacks' connection limit, read on the host for the shard and alone jobs, whose
    # containers have no Go, and in the visual, multiprocess and embedder jobs before their
    # timed browser steps.
    *[pinned_step(job, step, ENVOY, BROWSER_CONNECTIONS_SCRIPT, [
        (ENVOY, ["./internal/dispatch/store/cmd/connections"],
         "go run ./internal/dispatch/store/cmd/connections, which prints testlimits.ServerConnections",
         {"mode": "build"}),
    ], workflow=BROWSER_SITE)
      for job, step in ((BROWSER_BINARIES, BROWSER_CONNECTIONS_STEP),
                        (BROWSER_VISUAL, BROWSER_VISUAL_CONNECTIONS_STEP),
                        (BROWSER_MP, BROWSER_MP_CONNECTIONS_STEP),
                        (BROWSER_EMBEDDER, BROWSER_EMBEDDER_CONNECTIONS_STEP))],
    # The multiprocess job builds the server once, so its two processes find it current and
    # start in seconds rather than waiting out run-server.sh's own build in each boot wait.
    # The line has an `-o`, which is no form this check reduces, so the entry pins the whole
    # script and names the one package it compiles - which is the closure run-server.sh
    # compiles in that same job.
    pinned_step(BROWSER_MP, BROWSER_MP_BUILD, ENVOY, "go build -o ./dispatch-e2e ./cmd/dispatch", [
        (ENVOY, ["./cmd/dispatch"], "go build -o ./dispatch-e2e ./cmd/dispatch", {"mode": "build"}),
    ], workflow=BROWSER_SITE),
    # The embedder job builds it once for the same reason, and names its step for its own
    # lane because this check looks a step up by name across the workflow.
    pinned_step(BROWSER_EMBEDDER, BROWSER_EMBEDDER_BUILD, ENVOY, "go build -o ./dispatch-e2e ./cmd/dispatch", [
        (ENVOY, ["./cmd/dispatch"], "go build -o ./dispatch-e2e ./cmd/dispatch", {"mode": "build"}),
    ], workflow=BROWSER_SITE),
], reads=reads(
    ("packages/dispatch/**", f"{BROWSER_SUITE_STEPS} run the suite, "
     f"{at_browser('Build dispatch dashboard')} builds the SPA the rows read, and "
     f"{at_browser(BROWSER_MERGE_STEP, BROWSER_MERGE)} merges their reports, in packages/dispatch"),
    ("packages/proof-editor/**", "packages/dispatch/web/vite.config.ts:124-150 (build:web)"),
    # packages/contracts/src is no read of this gate's either: the filter's listed block holds
    # the modules dispatch_ui's tsc programs compile and the dispatch command's bundle loads,
    # which the changes job lists, and ci_ts_programs.py's LISTED says why this suite loads
    # those and no other; each job's `Build contracts for dispatch e2e` step runs
    # `build:dispatch`, whose three entries compile nothing outside dispatch_ui's.
    ("packages/envoy-client/bin/dispatch.ts", "packages/dispatch/e2e/inbox-cli.e2e.ts:328 runs it through e2e/dispatch-cli.ts:8"),
    (f"{ENVOY}/internal/dispatch/pmdoc/schema/blocks.json", "packages/dispatch/e2e/engine-reading.ts:13"),
    (f"{ENVOY}/internal/dispatch/pmdoc/testdata/fixtures.json", "read at packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts:48, whose path is the new URL at :44, in the module the browser rows of the seven markdown parity families it holds import; the web unit tests that read the same table run in dispatch-ui, which declares it too"),
    ("scripts/e2e/lib/held-port.py", "packages/dispatch/e2e/run-server.sh:80,89 holds each stack's server port through it"),
    (f"{ENVOY}/scripts/lib/pg-url.sh", "packages/dispatch/e2e/run-broker.sh:62 sources it, and packages/dispatch/e2e/harness-listeners.ts:188,197 "
                                       f"start that script in a one-process run on the fake broker: {at_browser(BROWSER_SHARD_SPECS)} and "
                                       f"{at_browser(BROWSER_ALONE_SPECS, BROWSER_ALONE)}"),
    (".github/scripts/dispatch-browser-samples.sh",
     f"{at_browser(BROWSER_SHARD_SPECS)} and {at_browser(BROWSER_ALONE_SPECS, BROWSER_ALONE)} "
     "start it and read its two summaries"),
    (".github/scripts/dispatch-browser-postgres.sh",
     f"{at_browser(BROWSER_PG_STEP)} and {at_browser(BROWSER_PG_STEP, BROWSER_ALONE)} "
     "run it for the reading of the image's PostgreSQL their stacks start"),
    (".github/scripts/dispatch-browser-latest-blobs.sh", at_browser(BROWSER_SELECT_STEP, BROWSER_MERGE)),
    (".github/scripts/dispatch-browser-run-reads.sh",
     f"{at_browser(BROWSER_READ_ONE_STEP, BROWSER_MERGE)} runs its jobs mode over this run's "
     f"own attempt's jobs, and {at_browser(BROWSER_BLOBS_REFUSED_STEP, BROWSER_MERGE)} its "
     "blobs-refused mode, which prints the line for a blob download actions/download-artifact "
     "could not complete"),
    (".github/scripts/dispatch-browser-wait-for-store.sh",
     f"{at_browser(BROWSER_WAIT_STORE_STEP)} runs it to wait for the file store the step "
     "before it started, which it makes a bucket in and writes, reads back and deletes one "
     "object in"),
    (".github/scripts/dispatch-browser-shard-suite.sh",
     f"{at_browser(BROWSER_SHARD_SPECS)} runs it for this leg's quarter of the lane's row time"),
    (".github/scripts/dispatch-browser-shard-record.sh",
     f"{at_browser(BROWSER_RECORD_STEP)} runs it to print and collect the file store's log "
     "and the stacks' own logs"),
    (".github/scripts/dispatch-browser-alone-suite.sh",
     f"{at_browser(BROWSER_ALONE_SPECS, BROWSER_ALONE)} runs it for this leg's one "
     "unsharded project"),
    (".github/scripts/dispatch-browser-alone-record.sh",
     f"{at_browser(BROWSER_RECORD_STEP, BROWSER_ALONE)} runs it to print and collect the "
     "stack's own logs, there being no file store on that machine"),
    (".github/scripts/dispatch-browser-visual-suite.sh",
     f"{at_browser(BROWSER_VISUAL_SPECS, BROWSER_VISUAL)} runs it for the stack and the "
     "e2e/visual-run.sh the comparison against the baselines needs"),
    (".github/scripts/dispatch-browser-merge-reports.sh",
     f"{at_browser(BROWSER_MERGE_STEP, BROWSER_MERGE)} runs it to join the legs' blob "
     "reports into the one HTML and JSON report of the run"),
    (".github/scripts/dispatch-browser-reading-rows.sh",
     f"{at_browser(BROWSER_READ_ONE_STEP, BROWSER_MERGE)} runs it for the rows each leg's "
     "log printed against the rows the merged report holds, for the legs that owed a "
     "blob report and uploaded none, and for the shard legs' row lists held against each other"),
    (".github/scripts/dispatch-browser-reading-flake-rate.sh",
     f"{at_browser(BROWSER_READ_TWO_STEP, BROWSER_MERGE)} runs it for the count and the "
     "percentage of the rows that passed on a retry"),
    (".github/scripts/dispatch-browser-reading-leg-exits.sh",
     f"{at_browser(BROWSER_READ_THREE_STEP, BROWSER_MERGE)} runs it to name every leg that "
     "concluded anything but success beside what its own blob report holds"),
    (".github/scripts/dispatch-browser-reading-row-durations.sh",
     f"{at_browser(BROWSER_READ_FOUR_STEP, BROWSER_MERGE)} runs it for the row count, the "
     "median and the 95th percentile of each layer's passing rows"),
    (".github/scripts/dispatch-browser-verdict.sh",
     f"{at_browser(BROWSER_VERDICT_STEP, BROWSER_MERGE)} runs it to fail the job on an "
     "upstream result that is neither success nor skipped and on a failed row in the "
     "merged report"),
    (".github/scripts/gh-read.sh",
     "dispatch-browser-run-reads.sh:79 sources it, so the merge job's read of its own attempt's "
     "jobs takes its retry ladder, and the blobs-refused mode makes no request"),
    # The container the shard and alone jobs run in, and what decides which bytes it is.
    # No step reads these three: the `container:` key does, before the first step runs, so
    # the citation is the two jobs that carry that key rather than a step of either.
    (".github/ci-images.yaml",
     "dispatch-browser.yaml's shard and alone jobs run in the image at the digest the "
     "one `built:` row of this file carries, so a row that moves moves what those two jobs "
     "run inside"),
    (".github/docker/browser.Dockerfile",
     "the Dockerfile that image is built from: its bytes are the browsers, their host "
     "packages and the PostgreSQL the lanes of dispatch-browser.yaml's shard and alone jobs "
     "start"),
    (".github/workflows/browser-image.yaml",
     "the build that publishes that image: its labels, platforms and action pins decide the "
     "bytes of the image dispatch-browser.yaml's shard and alone jobs pin"),
    ("scripts/third-party-notices.ts", "packages/dispatch/web/vite.config.ts:12 (build:web writes dist/THIRD_PARTY_NOTICES.txt)"),
) + envoy_client_dep("imported by packages/envoy-client/bin/dispatch.ts, which "
                     "packages/dispatch/e2e/inbox-cli.e2e.ts:328 runs")
  + setup_go(BROWSER_BINARIES, f"{ENVOY}/go.mod", BROWSER_SITE)
  + setup_go(BROWSER_MP, f"{ENVOY}/go.mod", BROWSER_SITE) + bun_job(BROWSER_SHARD, BROWSER_SITE)
  + CI_SELF + BROWSER_SITE_SELF, workflow=CI)
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/delivery/**', "filter dispatch_browser held packages/envoy/internal/dispatch/delivery/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter dispatch_browser held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 276 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipher/policy/**', "filter dispatch_browser held packages/envoy/internal/cipher/policy/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
