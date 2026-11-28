# The plugin's test suite, the merge-queue half of the old pi-legion job; pi_legion_build
# (pi-legion-build.py) is the half a pull request runs, the typecheck and the pack.
GATE = gate("pi_legion", "pi-legion", runs=[
    script_run("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts",
               'const result = Bun.spawnSync(["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args], {',
               f"daemon-pane.test.ts and daemon-standin.test.ts, run by {at('pi-legion', 'Test pi-legion')}", DAEMON, ["./cmd/daemon-pane"], mode="build",
               overlay={f"{DAEMON}/cmd/daemon-pane/main.go": "packages/pi-legion/scripts/skill-scenarios/daemon-pane.go"}),
    script_run("packages/pi-legion/test/approval-message-capture.ts",
               'const child = Bun.spawn(["go", "run", "./internal/prompts/cmd/approval-message"], {',
               f"approval-message-omp.test.ts, run by {at('pi-legion', 'Test pi-legion')}", DAEMON,
               ["./internal/prompts/cmd/approval-message"], mode="build"),
    script_run("packages/pi-legion/scripts/skill-scenarios/label-texts.sh",
               'go test -c -o "$bin/" ./internal/workflow ./internal/daemon ./internal/api',
               "`label-texts.sh build`, which rig.sh batch-scored runs in a build pod's scored Job (scored-job.sh), "
               f"and in {at('pi-legion', 'Test pi-legion')} for each of the two batches of "
               "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#a kept notice`; label-texts.test.ts runs this line with a planted go",
               DAEMON, ["./internal/workflow", "./internal/daemon", "./internal/api"], mode="test"),
    # The kept-notice row runs a child owner's whole run through rig.sh: each batch builds the
    # Envoy listener and runs the connections program, and each run starts its own Dispatch
    # server through run-server.sh, all three of the envoy module.
    script_run("packages/pi-legion/scripts/skill-scenarios/rig.sh",
               '(cd "$root/packages/envoy" && go build -o "$S/envoy-listener" ./cmd/listener)',
               "rig.sh's listener_up, which each batch's services_up runs: "
               f"`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#a kept notice` runs two batches in {at('pi-legion', 'Test pi-legion')}",
               ENVOY, ["./cmd/listener"], mode="build"),
    script_run("packages/pi-legion/scripts/skill-scenarios/rig.sh",
               'per_run=$(cd "$root/packages/envoy" && go run ./internal/dispatch/store/cmd/connections -process) &&',
               "rig.sh's connections_needed, which each batch's services_up runs: "
               f"`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#a kept notice` runs two batches in {at('pi-legion', 'Test pi-legion')}",
               ENVOY, ["./internal/dispatch/store/cmd/connections"], mode="build"),
    script_run(RUN_SERVER, DISPATCH_SERVER_BUILD,
               "started by rig.sh's start_dispatch for each run's own Dispatch, each run of "
               f"`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#a kept notice` in {at('pi-legion', 'Test pi-legion')}",
               ENVOY, ["./cmd/dispatch"], mode="build"),
], reads=reads(
    ("packages/pi-legion/**", f"{at('pi-legion', 'Test pi-legion')} in packages/pi-legion"),
    ("packages/pi-envoy/**", "`packages/pi-legion/extensions/legion.test.ts#envoyEntryDefault` and `packages/pi-legion/extensions/legion.test.ts#envoyEntry` and `packages/pi-legion/extensions/legion-role-claim.test.ts#envoyEntryDefault` and `packages/pi-legion/extensions/legion-role-claim.test.ts#envoyEntry` import ../../pi-envoy/extensions/envoy; `packages/pi-shared/test/omp-harness.ts#runLegionPane` loads it"),
    ("packages/pi-shared/src/**", "packages/pi-legion/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts"),
    ("packages/pi-shared/test/**", "`packages/pi-legion/extensions/legion-phase-stall-omp.test.ts#@legion/pi-shared/test/omp-harness`, `packages/pi-legion/extensions/legion-role-tools-omp.test.ts#@legion/pi-shared/test/omp-harness`, `packages/pi-legion/src/skills-guard.test.ts#@legion/pi-shared/test/skills-guard`, `packages/pi-legion/scripts/prepack.test.ts#@legion/pi-shared/test/skills-guard`"),
    ("packages/envoy-client/test/subscribe-answer.ts", "`packages/pi-legion/extensions/legion-role-claim.test.ts#consumerDelivery` "
                                                       "imports @legion/envoy-client/test/subscribe-answer, and "
                                                       "`packages/pi-shared/test/omp-harness.ts#consumerDelivery`, which this "
                                                       "package's omp tests drive, imports it too"),
    ("packages/envoy-client/test/keyed-layer.ts", "`packages/pi-shared/test/omp-harness.ts#keyedLayer`, which this package's "
                                                  "omp tests drive and its typecheck compiles, serves its stand-in "
                                                  "behind keyedLayer"),
    ("packages/envoy-client/test/fake-dispatch.ts", "`packages/pi-legion/src/tools.test.ts#fakeDispatchFetch` imports "
                                                     "it from @legion/envoy-client/test/fake-dispatch"),
    # envoy-client's sources, and cipher-client's with them, are the shared read below.
    (CONTRACTS_SRC, f"{at('pi-legion', 'Build contracts for pi-legion')}; tsconfig paths @legion/contracts"),
    (TEST_FIXTURES_SRC, "packages/pi-legion's extensions and src tests import @legion/test-fixtures"),
    (f"{FIX}/daemon-api/**", "`packages/pi-legion/extensions/legion.test.ts#/legion-claim-controller in a phase worker's pane calls no daemon route and does not exit` and `packages/pi-legion/extensions/legion.test.ts#launchedController`, `packages/pi-legion/extensions/legion-role-claim.test.ts#registered`; "
                             "`packages/pi-legion/src/daemon-api-version.test.ts#package.json names no daemon API contract, and the plugin speaks this tree's` reads released-contracts.json and lists contracts/*.json; "
                             "and `packages/contracts/src/daemon-contract.ts#lastReleased` and `packages/contracts/src/daemon-contract.ts#entryFixtures` read and list the same two for "
                             "`packages/contracts/src/daemon-contract.ts#daemonContract` of the committed manifest, which names no contract, as "
                             "`packages/pi-legion/extensions/legion.ts#PLUGIN_CONTRACT`, `packages/pi-legion/src/claim-session.ts#PLUGIN_CONTRACT` and `packages/pi-legion/src/controller-session.ts#PLUGIN_CONTRACT` "
                             "and `packages/pi-shared/src/legion-phase-ops.ts#pluginBuildOf` for `packages/pi-legion/src/tools.ts#BUILD` take it when a test imports them, "
                             "and as the tests' own PLUGIN_CONTRACT takes it"),
    (f"{FIX}/capability-redaction.json", "`packages/pi-legion/src/capability-report.test.ts#redaction`"),
    (f"{FIX}/capability-redaction-plugin-report.json", "`packages/pi-legion/src/capability-report.test.ts#every row passes cleanDetail: each branch that quotes upstream text carries it redacted` and `packages/pi-legion/src/capability-report.test.ts#PLUGIN_REPORT`"),
    ("packages/dispatch/e2e/**", "`packages/pi-legion/scripts/skill-scenarios/rig-isolation.test.ts#packages/dispatch/e2e/run-server.sh` and `packages/pi-legion/scripts/skill-scenarios/rig-isolation.test.ts#packages/pi-legion/scripts/skill-scenarios/seed.ts` names packages/dispatch/e2e/run-server.sh and seed.ts in the checkout it builds, and `packages/pi-legion/scripts/skill-scenarios/seed.ts#../../../dispatch/e2e/api` imports packages/dispatch/e2e/api.ts"),
    ("packages/dispatch/web/src/api/types.ts", "`packages/dispatch/e2e/api.ts#../web/src/api/types`"),
    ("packages/dispatch/web/src/api/paths.ts", "`packages/dispatch/e2e/api.ts#../web/src/api/paths` imports PathsOf, Op, PathParams, QueryParams, RequestBody, Query, Body and Result from it, since that client is keyed on the generated document's path patterns"),
    ("skills/**", "`packages/pi-legion/src/skills-guard.test.ts#stageSkills` stages them and `packages/pi-legion/src/skills-guard.test.ts#skillLinks` reads their skill:// links; `packages/pi-legion/scripts/skill-scenarios/rig.test.ts#ste runs omp in the directory on its prompt, no plugins, asd-ste100 alone, the pinned model` and `packages/pi-legion/scripts/skill-scenarios/rig.test.ts#fails, naming the rig's root, when the root is no checkout`; prepack stages skills"),
    ("scripts/pi-plugin-prepack.sh", "`packages/pi-shared/test/skills-guard.ts#PREPACK_SCRIPT` and `packages/pi-shared/test/skills-guard.ts#stageSkills`; `packages/pi-legion/package.json#prepack`, which the pack of `packages/pi-legion/scripts/prepack.test.ts#packs from a copy` runs"),
    ("scripts/daemon-contract.sh", "`scripts/e2e/lib/pack-plugin.sh#$root/scripts/daemon-contract.sh` writes its number into the packed manifest, which `packages/pi-legion/scripts/prepack.test.ts#packs from a copy` holds to `daemonContract`"),
    ("scripts/third-party-notices.ts", "`scripts/pi-plugin-prepack.sh#$root/scripts/third-party-notices.ts`"),
    (f"{DAEMON}/internal/prompts/**/*.md", "`packages/pi-legion/src/skills-guard.test.ts#promptsRoot` is a root of the skill:// links `packages/pi-legion/src/skills-guard.test.ts#skillLinks` reads"),
    (f"{DAEMON}/internal/workflow/testdata/approval-entries/**", "`packages/pi-legion/test/approval-message-capture.ts#ENTRIES` names internal/workflow/testdata/approval-entries, whose entry files `packages/pi-legion/test/approval-message-capture.ts#sendEntry` and `packages/pi-legion/test/approval-message-capture.ts#cardCase` read"),
    ("docs/site/src/content/docs/legion/concepts.md", "`packages/pi-legion/src/skills-guard.test.ts#the guides say only a review that requests changes counts a round` reads it"),
    ("docs/site/src/content/docs/legion/troubleshooting.md", "`packages/pi-legion/src/skills-guard.test.ts#the guides say only a review that requests changes counts a round` reads it"),
    ("scripts/e2e/lib/**", "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#scriptFunctions`, imported from scripts/e2e/lib/script-functions, and `packages/pi-legion/scripts/skill-scenarios/rig.test.ts#loopbackDsn`, which the services_up row sources; `packages/pi-legion/scripts/skill-scenarios/rig.sh#$root/scripts/e2e/lib/rig.sh` sources scripts/e2e/lib/rig.sh; `packages/pi-legion/scripts/prepack.test.ts#scripts/e2e/lib/pack-plugin.sh` packs with it"),
    ("packages/envoy/scripts/lib/pg-url.sh", "`packages/pi-legion/scripts/skill-scenarios/rig.sh#$root/packages/envoy/scripts/lib/pg-url.sh` sources it for pg_sql and safe_url; "
                                             "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#safe_url drops the password of a URL and keeps the rest` and "
                                             "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#refuses a URL the runs cannot use before psql runs or the URL is written` source it, "
                                             "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#fails, naming the rig's root, when the root is no checkout` copies it beside rig.sh, and "
                                             "`packages/pi-legion/scripts/skill-scenarios/rig-isolation.test.ts#pgUrl` copies it into its scratch checkout beside rig.sh"),
    (".omp-pin", "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#fails, naming the rig's root, when the root is no checkout` copies .omp-pin"),
    ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts", "`packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts#runDaemonPane` writes the overlay and runs go run"),
    ("packages/pi-legion/scripts/skill-scenarios/label-texts.sh", "`packages/pi-legion/scripts/skill-scenarios/label-texts.sh#compile` runs go test -c of three daemon packages"),
) + envoy_client_dep("packages/pi-legion/tsconfig.json paths @legion/envoy-client/*; scripts/skill-scenarios/*.ts "
                     "import @legion/envoy-client/*")
  + install_jj("pi-legion") + install_omp("pi-legion") + setup_go("pi-legion", f"{DAEMON}/go.mod")
  + bun_job("pi-legion"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter pi_legion held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 277 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
