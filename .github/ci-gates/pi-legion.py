# The plugin's test suite, the merge-queue half of the old pi-legion job; pi_legion_build
# (pi-legion-build.py) is the half a pull request runs, the typecheck and the pack.
GATE = gate("pi_legion", "pi-legion", runs=[
    script_run("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts",
               '["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args],',
               f"daemon-pane.test.ts, run by {at('pi-legion', 'Test pi-legion')}", DAEMON, ["./cmd/daemon-pane"], mode="build",
               overlay={f"{DAEMON}/cmd/daemon-pane/main.go": "packages/pi-legion/scripts/skill-scenarios/daemon-pane.go"}),
], reads=reads(
    ("packages/pi-legion/**", f"{at('pi-legion', 'Test pi-legion')} in packages/pi-legion"),
    ("packages/pi-envoy/**", "packages/pi-legion/extensions/legion.test.ts:77,330 and legion-role-claim.test.ts:14,102 import ../../pi-envoy/extensions/envoy; packages/pi-shared/test/omp-harness.ts:1265"),
    ("packages/claude-envoy/**", "packages/pi-legion/src/skills-guard.test.ts:318 spawns packages/claude-envoy/scripts/build.ts (PLUGIN_BUILDER :29) and asserts an exact file list over the tree it writes, so that script and everything it reads - its BUNDLE_ENTRYPOINTS at scripts/build.ts:39-48 among them - are read by this job through a spawn, which carries no import and which no derivation sees"),
    ("packages/envoy-client/bin/dispatch.ts", "an entry of that builder's BUNDLE_ENTRYPOINTS (packages/claude-envoy/scripts/build.ts:47), outside the package it builds"),
    ("packages/pi-shared/src/**", "packages/pi-legion/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts"),
    ("packages/pi-shared/test/**", "packages/pi-legion/extensions/legion-phase-stall-omp.test.ts:14, legion-role-tools-omp.test.ts:13, src/skills-guard.test.ts:20, scripts/prepack.test.ts:4"),
    ("packages/envoy-client/test/subscribe-answer.ts", "packages/pi-legion/extensions/legion-role-claim.test.ts:48 "
                                                       "imports @legion/envoy-client/test/subscribe-answer, and "
                                                       "packages/pi-shared/test/omp-harness.ts:17, which this "
                                                       "package's omp tests drive, imports it too"),
    # envoy-client's sources, and cipher-client's with them, are the shared read below.
    (CONTRACTS_SRC, f"{at('pi-legion', 'Build contracts for pi-legion')}; tsconfig paths @legion/contracts"),
    (TEST_FIXTURES_SRC, "packages/pi-legion's extensions and src tests import @legion/test-fixtures"),
    (f"{FIX}/daemon-api/**", "packages/pi-legion/extensions/legion.test.ts:1452,4147, legion-role-claim.test.ts:22, src/daemon-api-version.test.ts:14"),
    (f"{FIX}/capability-redaction.json", "packages/pi-legion/src/capability-report.test.ts:848"),
    (f"{FIX}/capability-redaction-plugin-report.json", "packages/pi-legion/src/capability-report.test.ts:970 (PLUGIN_REPORT :854)"),
    ("packages/dispatch/e2e/**", "packages/pi-legion/scripts/skill-scenarios/rig-isolation.test.ts:127 names packages/dispatch/e2e/run-server.sh in the checkout it builds, and seed.ts:161, which that test names at :156, imports packages/dispatch/e2e/api.ts"),
    ("packages/dispatch/web/src/api/types.ts", "packages/dispatch/e2e/api.ts:20"),
    ("packages/dispatch/web/src/api/paths.ts", "packages/dispatch/e2e/api.ts:8-19 imports PathsOf, Op, PathParams, QueryParams, RequestBody, Query, Body and Answer from it, since that client is keyed on the generated document's path patterns"),
    ("skills/**", "packages/pi-legion/src/skills-guard.test.ts:44 stages them and :204-208 reads the roots at :194-202; scripts/skill-scenarios/rig.test.ts:9922 reads skills/asd-ste100/SKILL.md and :10182-10188 copies it; prepack stages skills"),
    ("scripts/pi-plugin-prepack.sh", "packages/pi-shared/test/skills-guard.ts:17,34"),
    ("scripts/third-party-notices.ts", "scripts/pi-plugin-prepack.sh:97,100"),
    (f"{DAEMON}/internal/**", "packages/pi-legion/src/skills-guard.test.ts:204-208 sweeps every .md/.go/.ts/.yaml/.yml/.example/.sh under packages/daemon/internal, a root of the list at :197"),
    ("docs/kubernetes.md", "packages/pi-legion/src/skills-guard.test.ts:204-208, a root of the list at :196"),
    ("docs/site/src/content/docs/**", "packages/pi-legion/src/skills-guard.test.ts:204-208, a root of the list at :198"),
    ("deploy/kubernetes/daemon/controller.yaml.example", "packages/pi-legion/src/skills-guard.test.ts:204-208, a root of the list at :199"),
    ("AGENTS.md", "packages/pi-legion/src/skills-guard.test.ts:204-208, a root of the list at :200"),
    ("README.md", "packages/pi-legion/src/skills-guard.test.ts:204-208, a root of the list at :201"),
    ("scripts/e2e/lib/**", "packages/pi-legion/scripts/skill-scenarios/rig.test.ts:32 imports scripts/e2e/lib/script-functions and :12458 sources scripts/e2e/lib/loopback-dsn.sh for the services_up rows; rig.sh:689 sources scripts/e2e/lib/rig.sh"),
    ("packages/envoy/scripts/lib/pg-url.sh", "packages/pi-legion/scripts/skill-scenarios/rig.sh:693 sources it for pg_sql and safe_url; "
                                             "rig.test.ts:9201 sources it for the safe_url rows, :12458 for the services_up rows and :10182-10188 copies it beside rig.sh, and "
                                             "rig-isolation.test.ts:121-124 copies it into its scratch checkout beside rig.sh"),
    (".omp-pin", "packages/pi-legion/scripts/skill-scenarios/rig.test.ts:10182-10188 copies .omp-pin"),
    ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts", "writes the overlay and runs go run (:123-128)"),
    ("scripts/e2e/plan-pointer-drive.ts", "packages/pi-legion/scripts/skill-scenarios/rig.test.ts:13139 typechecks it with tsc, a spawn no derivation sees, in the row \"plan-pointer-drive.ts typechecks, claim and all\""),
    ("packages/envoy/internal/testnats/nats.go", "scripts/e2e/lib/nats-image.sh:12 reads its HubImage, and packages/pi-legion/scripts/skill-scenarios/rig.sh:1861-1864 (read_nats_image) sources that file for services_up's NATS container and services_from (:2035), which the services_up rows run (rig.test.ts:12403)"),
) + envoy_client_dep("packages/pi-legion/tsconfig.json paths @legion/envoy-client/*; scripts/skill-scenarios/*.ts "
                     "import @legion/envoy-client/*")
  + install_jj("pi-legion") + install_omp("pi-legion") + setup_go("pi-legion", f"{DAEMON}/go.mod")
  + bun_job("pi-legion"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter pi_legion held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 277 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
