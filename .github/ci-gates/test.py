# What is left of the test job once the daemon's Go suite and the private-name port harness
# have jobs of their own, and its builds, lints and typechecks are tooling-checks' (the
# tooling-checks.py file beside this one): the script harnesses - each behind a gate of its own
# (the test.<filter>.py files beside this one), whose reads this job's filter carries too, since
# the job must be selected for the step to run in it (ci_filter_coverage.gate_read_sets adds
# them) - and the Bun suites of scripts/, all but three harnesses queued through the harness pool.
# One go command is left, the build a Bun test starts: the listener's. The natsfloor build
# nats-image.test.ts starts is lint's, which runs that file on every run; this job's bun test of
# scripts/e2e/lib leaves it out (E2E_LIB_TESTS).
GATE = gate("test", "test", runs=[
    script_run("scripts/e2e/lib/held-port.test.ts", 'const build = Bun.spawnSync(["go", "build", "-o", listener, "./cmd/listener"], {',
               at("test", E2E_LIB_TESTS), ENVOY, ["./cmd/listener"], mode="build"),
], reads=reads(
    # Every harness step but three queues its harness through the pool, and the wait step is the
    # verdict on them, so a change to the pool must start this job whatever gate it hits.
    (".github/scripts/harness-pool.sh",
     f"{at('test', HARNESS_POOL_WAIT_STEP)}, and each harness step above it but three queues its harness through it"),
    ("scripts/third-party-notices.test.ts", at("test", "Test third-party notices")),
    ("packages/dispatch/web/src/fonts/inter/**",
     "`scripts/third-party-notices.test.ts#lists the typeface the Dispatch web app ships, with the SIL Open Font License's text` "
     "resolves the font file's path, lists that directory and reads the package.json and LICENSE.txt in it"),
    ("scripts/third-party-notices.ts", "`scripts/third-party-notices.test.ts#thirdPartyNotices` and `scripts/third-party-notices.test.ts#the command lists every package any of its metafiles names, one bundle's alone included` imports it and runs it"),
    ("scripts/semantic-line-breaks.test.ts", at("test", "Test the semantic line-break checker")),
    ("scripts/semantic-line-breaks.ts", "`scripts/semantic-line-breaks.test.ts#Bun.spawnSync` spawns it as `scripts/semantic-line-breaks.test.ts#SCRIPT`"),
    ("scripts/e2e/**", f"{at('test', E2E_LIB_TESTS)} (`scripts/e2e/lib/script-functions.test.ts#readers` scans scripts/e2e/*.sh and lib/*); "
                       f"{at('test', 'Test the e2e proof selector')}: the selector reads scripts/e2e/*.sh"),
    (CONTRACTS_SRC, "`scripts/e2e/lib/envoy-bridge.ts#EnvelopeSchema` and `scripts/e2e/lib/envoy-bridge.ts#githubSubject`, "
                    "`scripts/e2e/lib/session-settings-services.ts#agentStreamControlSubject`; `scripts/dispatch/ask-census.ts#ArtifactReviewEventPayload`, "
                    f"which {at('test', 'Test the Dispatch census script')} loads"),
    (TEST_FIXTURES_SRC, f"{at('test', E2E_LIB_TESTS)} and the other bun test steps import @legion/test-fixtures"),
    # The envoy-client sources, and cipher-client's with them, are the shared read below:
    # `scripts/e2e/lib/nats-stream.ts#natsAuthOptions`, `scripts/e2e/lib/operator-dispatch.ts#sessionDispatchSettings`,
    # `scripts/e2e/lib/envoy-bridge.ts#natsAuthOptions` and `scripts/dispatch/ask-census.ts#DispatchClient` import
    # @legion/envoy-client/*.
    ("packages/envoy-client/test/fake-dispatch.ts", "`scripts/e2e/lib/agent-replay.test.ts#startFakeDispatch` imports it by path"),
    ("scripts/dispatch/**", at("test", "Test the Dispatch census script")),
    ("scripts/approval-eval/**", at("test", "Test the approval-eval statistics")),
    ("scripts/search-measure/**", f"{at('test', 'Test the search-measure psql helper')}: pg_test.py imports pg.py"),
    (f"{ENVOY}/scripts/fixtures/pg-url.json", f"{at('test', 'Test the search-measure psql helper')}: `scripts/search-measure/pg_test.py#CORPUS` reads it"),
    ("skills/approval-review/references/never-decide.md",
     f"{at('test', 'Test the approval-eval statistics')}: `scripts/approval-eval/never-decide.ts#neverDecideDefinition` reads "
     "it, through `scripts/approval-eval/refusals.ts#refusalRun`, which `scripts/approval-eval/__tests__/refusals.test.ts#the refusal run` runs"),
    (f"{FIX}/daemon-api/**", "`scripts/e2e/lib/stage4b-boot-capabilities.test.ts#fixture`"),
    (f"{DAEMON}/internal/daemon/controller.go", "`scripts/e2e/lib/controller-liveness-verdict.test.ts#the texts the verdict counts are the daemon's own` reads it as text"),
    (f"{DAEMON}/internal/controller/liveness.go", "`scripts/e2e/lib/controller-liveness-verdict.test.ts#the texts the verdict counts are the daemon's own`"),
    (f"{DAEMON}/internal/supervise/machine.go", "`scripts/e2e/lib/controller-liveness-verdict.test.ts#the texts the verdict counts are the daemon's own`"),
    (f"{DAEMON}/internal/intake/consume.go", "`scripts/e2e/lib/stage4b-consumers.test.ts#deletes every durable consumer the daemon opens for the project`"),
    (f"{DAEMON}/internal/record/outbox.go", "`scripts/e2e/lib/worker-notices.test.ts#daemonKinds` reads it as text"),
    (f"{DAEMON}/internal/capabilities/testdata/web-search-open-planner.json", '`scripts/e2e/lib/stage4b-capabilities.test.ts#"web-search-open-planner.json"`'),
    (f"{DAEMON}/internal/runtime/sandbox/testdata/golden/root.json", "`scripts/e2e/lib/stage4b-pods.test.ts#golden`"),
    (f"{DAEMON}/internal/runtime/sandbox/testdata/golden/root-operator-route.json", "`scripts/e2e/lib/stage4b-pods.test.ts#operatorRoute`"),
    (".github/actions/install-changie/**", f"{at('test', './.github/actions/install-changie')}, which "
                                           "release-push.test.sh's fold-notes cases render with"),
    (".github/ci-tools.yaml", "`.github/actions/install-changie/action.yml#ci_tools_field` and `.github/actions/install-changie/action.yml#ci_tools_fetch` installs the tarball of the changie row, "
                              "or of changie-aarch64 on an arm64 runner"),
    (".github/scripts/ci-tools.sh", "`.github/actions/install-changie/action.yml#$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh` sources it, and ci_tools_fetch "
                                    "reads that row's two sources"),
) + envoy_client_dep("`scripts/e2e/lib/nats-stream.ts#natsAuthOptions`, `scripts/e2e/lib/operator-dispatch.ts#sessionDispatchSettings`, "
                     "`scripts/e2e/lib/envoy-bridge.ts#natsAuthOptions`; `scripts/dispatch/ask-census.ts#DispatchClient`")
  + setup_go("test", f"{DAEMON}/go.mod") + bun_job("test"))
