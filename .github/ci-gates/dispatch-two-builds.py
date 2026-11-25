# Its "Build the Dispatch server" step reads the commit it stamps the build with and names an
# output (-o), which the simple form does not reduce, so the step is pinned whole, as
# dispatch-browser-multiprocess's is. The Go step is a gate of its own below; this gate carries
# that step's reads too, since the job must be selected for the step to run in it.
GATE = gate("dispatch_two_builds", TWO_BUILDS_JOB, runs=[
    script_run(RUN_SERVER, DISPATCH_SERVER_BUILD, "started by packages/dispatch/e2e/harness-listeners.ts's dispatchServer (harness-start.ts, which "
               "e2e/global-setup.ts runs) in "
               + at(TWO_BUILDS_JOB, "Browser-test each build's page against the other build's server")
               + " and "
               + at(TWO_BUILDS_JOB, "Browser-test each build's page against the first-rollout build's server"),
               ENVOY, ["./cmd/dispatch"], mode="build"),
    ceiling_run(TWO_BUILDS_JOB),
], exact=[
    two_builds_go_step(),
    two_builds_same_step(),
    pinned_step(TWO_BUILDS_JOB, "Build the Dispatch server", ENVOY, DISPATCH_E2E_BUILD_SCRIPT, [
        (ENVOY, ["./cmd/dispatch"], DISPATCH_E2E_BUILD, {"mode": "build"}),
    ]),
    # The browser half's stack connection limit, read before its two browser steps, which
    # start the harness's own Postgres rather than using the service.
    pinned_step(TWO_BUILDS_JOB, TWO_BUILDS_CONNECTIONS_STEP, ENVOY, BROWSER_CONNECTIONS_SCRIPT, [
        (ENVOY, ["./internal/dispatch/store/cmd/connections"],
         "go run ./internal/dispatch/store/cmd/connections, which prints testlimits.ServerConnections",
         {"mode": "build"}),
    ]),
], reads=reads(
    ("packages/dispatch/**", at(TWO_BUILDS_JOB, "Browser-test each build's page against the other build's server")),
    ("packages/proof-editor/**", "`packages/dispatch/web/vite.config.ts#alias` (build:web), whose @legion/proof-editor entries resolve into it"),
    # packages/contracts/src is no read of this gate's either: the filter's listed block holds
    # the modules dispatch_ui's tsc programs compile and the dispatch command the Go step runs
    # loads, which the changes job lists, and ci_ts_programs.py's LISTED says why this job
    # loads that union and no other. Its `Build contracts for dispatch e2e` step runs
    # `build:dispatch`, whose three entries compile nothing outside it, and reads the package's
    # manifest, which bun_job declares.
    (f"{ENVOY}/internal/dispatch/pmdoc/schema/blocks.json", "`packages/dispatch/e2e/engine-reading.ts#blockSchema`"),
    ("scripts/e2e/lib/held-port.py", "`packages/dispatch/e2e/run-server.sh#held_port` holds the server's port through it"),
    ("scripts/third-party-notices.ts", "`packages/dispatch/web/vite.config.ts#thirdPartyNotices` (build:web writes dist/THIRD_PARTY_NOTICES.txt)"),
) + two_builds_go_reads())
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/**', "filter dispatch_two_builds held packages/envoy/internal/dispatch/** by hand before the filters were computed: it selected 687 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter dispatch_two_builds held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy-client/**', "filter dispatch_two_builds held packages/envoy-client/** by hand before the filters were computed: it selected 107 file(s) no other read of this gate declares"),
])
