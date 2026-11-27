# The plugin's test suite, the merge-queue half of the old pi-legion job; pi_legion_build
# (pi-legion-build.py) is the half a pull request runs, the typecheck and the pack.
GATE = gate("pi_legion", "pi-legion", runs=[
    script_run("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts",
               'const command = ["go", "run", "-p", BUILD_WIDTH, "-overlay", overlay, "./cmd/daemon-pane"];',
               f"daemon-pane.test.ts, run by {at('pi-legion', 'Test pi-legion')}", DAEMON, ["./cmd/daemon-pane"], mode="build",
               overlay={f"{DAEMON}/cmd/daemon-pane/main.go": "packages/pi-legion/scripts/skill-scenarios/daemon-pane.go"}),
], reads=reads(
    ("packages/pi-legion/**", f"{at('pi-legion', 'Test pi-legion')} in packages/pi-legion"),
    ("packages/pi-envoy/**", "`packages/pi-legion/extensions/legion.test.ts#envoyEntryDefault` and `packages/pi-legion/extensions/legion.test.ts#envoyEntry` and `packages/pi-legion/extensions/legion-role-claim.test.ts#envoyEntryDefault` and `packages/pi-legion/extensions/legion-role-claim.test.ts#envoyEntry` import ../../pi-envoy/extensions/envoy; `packages/pi-shared/test/omp-harness.ts#runLegionPane` loads it"),
    ("packages/claude-envoy/**", "`packages/pi-legion/src/skills-guard.test.ts#PLUGIN_BUILDER` and `packages/pi-legion/src/skills-guard.test.ts#no skill, agent or plugin file keeps work in the foreground` spawns packages/claude-envoy/scripts/build.ts and asserts an exact file list over the tree it writes, so that script and everything it reads - `packages/claude-envoy/scripts/build.ts#BUNDLE_ENTRYPOINTS` among them - are read by this job through a spawn, which carries no import and which no derivation sees"),
    ("packages/envoy-client/bin/dispatch.ts", "the dispatch entry of that builder's BUNDLE_ENTRYPOINTS, outside the package it builds: `packages/claude-envoy/scripts/build.ts#BUNDLE_ENTRYPOINTS` and `packages/claude-envoy/scripts/build.ts#../envoy-client/bin/dispatch.ts`"),
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
    (TEST_FIXTURES_SRC, "packages/pi-legion's extensions, src and scripts/skill-scenarios tests import @legion/test-fixtures; `packages/pi-legion/scripts/skill-scenarios/test-limit.ts#fileLimit` imports its row-clock, and the temporary-directories fixture runs its remove-trees.ts as a process of its own"),
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
    ("skills/**", "`packages/pi-legion/src/skills-guard.test.ts#stageSkills` and `packages/pi-legion/src/skills-guard.test.ts#the controller skill reads pull requests as the review App, and nothing says the controller holds no GitHub credential` stages them, and that test reads the roots it lists, skills/legion-controller among them; `packages/pi-legion/scripts/skill-scenarios/rig.test.ts#ste runs omp in the directory on its prompt, no plugins, asd-ste100 alone, the pinned model` and `packages/pi-legion/scripts/skill-scenarios/rig.test.ts#fails, naming the rig's root, when the root is no checkout`; prepack stages skills"),
    ("scripts/pi-plugin-prepack.sh", "`packages/pi-shared/test/skills-guard.ts#PREPACK_SCRIPT` and `packages/pi-shared/test/skills-guard.ts#stageSkills`"),
    ("scripts/third-party-notices.ts", "`scripts/pi-plugin-prepack.sh#$root/scripts/third-party-notices.ts`"),
    (f"{DAEMON}/internal/**", "`packages/pi-legion/src/skills-guard.test.ts#the controller skill reads pull requests as the review App, and nothing says the controller holds no GitHub credential` and "
                              '`packages/pi-legion/src/skills-guard.test.ts#path.join(REPO_ROOT, "packages/daemon/internal")`'
                              " sweeps every .md/.go/.ts/.yaml/.yml/.example/.sh under packages/daemon/internal, a root of that test's list"),
    ("docs/kubernetes.md", "`packages/pi-legion/src/skills-guard.test.ts#the controller skill reads pull requests as the review App, and nothing says the controller holds no GitHub credential` and "
                           '`packages/pi-legion/src/skills-guard.test.ts#path.join(REPO_ROOT, "docs/kubernetes.md")`'
                           " sweeps it, a root of that test's list"),
    ("docs/site/src/content/docs/**", "`packages/pi-legion/src/skills-guard.test.ts#the controller skill reads pull requests as the review App, and nothing says the controller holds no GitHub credential` and "
                                      '`packages/pi-legion/src/skills-guard.test.ts#path.join(REPO_ROOT, "docs/site/src/content/docs")`'
                                      " sweeps it, a root of that test's list"),
    ("deploy/kubernetes/daemon/controller.yaml.example", "`packages/pi-legion/src/skills-guard.test.ts#the controller skill reads pull requests as the review App, and nothing says the controller holds no GitHub credential` and "
                                                         '`packages/pi-legion/src/skills-guard.test.ts#path.join(REPO_ROOT, "deploy/kubernetes/daemon/controller.yaml.example")`'
                                                         " sweeps it, a root of that test's list"),
    ("AGENTS.md", "`packages/pi-legion/src/skills-guard.test.ts#the controller skill reads pull requests as the review App, and nothing says the controller holds no GitHub credential` and "
                  '`packages/pi-legion/src/skills-guard.test.ts#path.join(REPO_ROOT, "AGENTS.md")`'
                  " sweeps it, a root of that test's list"),
    ("README.md", "`packages/pi-legion/src/skills-guard.test.ts#the controller skill reads pull requests as the review App, and nothing says the controller holds no GitHub credential` and "
                  '`packages/pi-legion/src/skills-guard.test.ts#path.join(REPO_ROOT, "README.md")`'
                  " sweeps it, a root of that test's list"),
    ("scripts/e2e/lib/**", "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#scriptFunctions`, imported from scripts/e2e/lib/script-functions, and `packages/pi-legion/scripts/skill-scenarios/rig.test.ts#loopbackDsn`, which the services_up row sources; `packages/pi-legion/scripts/skill-scenarios/rig.sh#$root/scripts/e2e/lib/rig.sh` sources scripts/e2e/lib/rig.sh"),
    ("packages/envoy/scripts/lib/pg-url.sh", "`packages/pi-legion/scripts/skill-scenarios/rig.sh#$root/packages/envoy/scripts/lib/pg-url.sh` sources it for pg_sql and safe_url; "
                                             "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#safe_url drops the password of a URL and keeps the rest` and "
                                             "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#refuses a URL the runs cannot use before psql runs or the URL is written` source it, "
                                             "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#fails, naming the rig's root, when the root is no checkout` copies it beside rig.sh, and "
                                             "`packages/pi-legion/scripts/skill-scenarios/rig-isolation.test.ts#pgUrl` copies it into its scratch checkout beside rig.sh"),
    (".omp-pin", "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#fails, naming the rig's root, when the root is no checkout` copies .omp-pin"),
    ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts", "`packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts#runDaemonPane` writes the overlay and runs go run"),
) + envoy_client_dep("packages/pi-legion/tsconfig.json paths @legion/envoy-client/*; scripts/skill-scenarios/*.ts "
                     "import @legion/envoy-client/*")
  + install_jj("pi-legion") + install_omp("pi-legion") + setup_go("pi-legion", f"{DAEMON}/go.mod")
  + bun_job("pi-legion"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter pi_legion held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 277 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
