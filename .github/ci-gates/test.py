# What is left of the test job once the daemon's Go suite and the private-name port harness
# have jobs of their own, and its builds, lints and typechecks are tooling-checks' (the
# tooling-checks.py file beside this one): the script harnesses - each behind a gate of its own
# (the test.<filter>.py files beside this one), whose reads this job's filter carries too, since
# the job must be selected for the step to run in it (ci_filter_coverage.gate_read_sets adds
# them) - and the Bun suites of scripts/. Two go commands are left, the builds Bun tests start:
# the listener's, and the natsfloor command the pin scanner asks the NATS floor through.
GATE = gate("test", "test", runs=[
    script_run("scripts/e2e/lib/held-port.test.ts", 'const build = Bun.spawnSync(["go", "build", "-o", listener, "./cmd/listener"], {',
               at("test", "bun test scripts/e2e/lib"), ENVOY, ["./cmd/listener"], mode="build"),
    script_run("scripts/e2e/lib/nats-image.test.ts", 'const build = Bun.spawnSync(["go", "build", "-o", floor, "./natsversion/cmd/natsfloor"], {',
               at("test", "bun test scripts/e2e/lib"), ENVOY, ["./natsversion/cmd/natsfloor"], mode="build"),
], reads=reads(
    ("scripts/third-party-notices.test.ts", at("test", "Test third-party notices")),
    ("packages/dispatch/web/src/fonts/inter/**", "scripts/third-party-notices.test.ts:102-105 resolves the font "
                                                 "file's path, lists that directory and reads the package.json and "
                                                 "LICENSE.txt in it"),
    ("scripts/third-party-notices.ts", "scripts/third-party-notices.test.ts:5,139"),
    ("scripts/semantic-line-breaks.test.ts", at("test", "Test the semantic line-break checker")),
    ("scripts/semantic-line-breaks.ts", "scripts/semantic-line-breaks.test.ts:6,28 spawns it"),
    ("scripts/priorities-seed.test.ts", at("test", "Test the priorities seed")),
    ("scripts/priorities-seed.ts", "scripts/priorities-seed.test.ts:4 imports it"),
    ("scripts/e2e/**", f"{at('test', 'bun test scripts/e2e/lib')} (lib/script-functions.test.ts:105-108 scans scripts/e2e/*.sh and lib/*); "
                       f"{at('test', 'Test the e2e proof selector')}: the selector reads scripts/e2e/*.sh"),
    (CONTRACTS_SRC, "scripts/e2e/lib/envoy-bridge.ts:9-10, session-settings-services.ts:6; scripts/dispatch/ask-census.ts:3-9, "
                    f"which {at('test', 'Test the Dispatch census script')} loads; scripts/priorities-seed.ts:29"),
    (TEST_FIXTURES_SRC, f"{at('test', 'bun test scripts/e2e/lib')} and the other bun test steps import @legion/test-fixtures"),
    # The envoy-client sources, and cipher-client's with them, are the shared read below:
    # scripts/e2e/lib/nats-stream.ts:16-18, operator-dispatch.ts:17-22, envoy-bridge.ts:11
    # and scripts/dispatch/ask-census.ts:10-18 import @legion/envoy-client/*.
    ("packages/envoy-client/test/fake-dispatch.ts", "scripts/e2e/lib/agent-replay.test.ts:4-7 imports it by path"),
    ("scripts/dispatch/**", f"{at('test', 'Test the Dispatch census script')}; "
                            "scripts/priorities-seed.test.ts:3 imports __tests__/held-deadlines.ts"),
    ("scripts/approval-eval/**", at("test", "Test the approval-eval statistics")),
    ("scripts/search-measure/**", f"{at('test', 'Test the search-measure psql helper')}: pg_test.py imports pg.py"),
    (f"{ENVOY}/scripts/fixtures/pg-url.json", f"{at('test', 'Test the search-measure psql helper')}: scripts/search-measure/pg_test.py:86 reads it (CORPUS)"),
    ("skills/approval-review/references/never-decide.md",
     f"{at('test', 'Test the approval-eval statistics')}: scripts/approval-eval/never-decide.ts:184 reads "
     "it, through refusals.ts:725, which __tests__/refusals.test.ts:165-177 runs"),
    (f"{FIX}/daemon-api/**", "scripts/e2e/lib/stage4b-boot-capabilities.test.ts:17"),
    (f"{DAEMON}/internal/daemon/controller.go", "scripts/e2e/lib/controller-liveness-verdict.test.ts:344-345 reads it as text"),
    (f"{DAEMON}/internal/controller/liveness.go", "scripts/e2e/lib/controller-liveness-verdict.test.ts:347"),
    (f"{DAEMON}/internal/supervise/machine.go", "scripts/e2e/lib/controller-liveness-verdict.test.ts:350"),
    (f"{DAEMON}/internal/intake/consume.go", "scripts/e2e/lib/stage4b-consumers.test.ts:23-26"),
    (f"{DAEMON}/internal/capabilities/testdata/web-search-open-planner.json", "scripts/e2e/lib/stage4b-capabilities.test.ts:13-22"),
    (f"{DAEMON}/internal/runtime/sandbox/testdata/golden/root.json", "scripts/e2e/lib/stage4b-pods.test.ts:22-35"),
    (f"{DAEMON}/internal/runtime/sandbox/testdata/golden/root-operator-route.json", "scripts/e2e/lib/stage4b-pods.test.ts:180-190"),
    (".github/actions/install-changie/**", f"{at('test', './.github/actions/install-changie')}, which "
                                           "release-push.test.sh's fold-notes cases render with"),
    (".github/ci-tools.yaml", ".github/actions/install-changie/action.yml:28-29 installs the tarball of the changie row, "
                              "or of changie-aarch64 on an arm64 runner"),
    (".github/scripts/ci-tools.sh", ".github/actions/install-changie/action.yml:18 sources it, and ci_tools_fetch "
                                    "reads that row's two sources"),
) + envoy_client_dep("scripts/e2e/lib/nats-stream.ts:16-18, operator-dispatch.ts:17-22, envoy-bridge.ts:11; "
                     "scripts/dispatch/ask-census.ts:10-18; scripts/priorities-seed.ts:30-39 "
                     "and priorities-seed.test.ts:2")
  + setup_go("test", f"{DAEMON}/go.mod") + bun_job("test"))
