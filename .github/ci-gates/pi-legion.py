# The plugin's test suite, the merge-queue half of the old pi-legion job; pi_legion_build
# (pi-legion-build.py) is the half a pull request runs, the typecheck and the pack.
GATE = gate("pi_legion", "pi-legion", runs=[
    script_run("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts",
               'const result = Bun.spawnSync(["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args], {',
               f"daemon-pane.test.ts, run by {at('pi-legion', 'Test pi-legion')}", DAEMON, ["./cmd/daemon-pane"], mode="build",
               overlay={f"{DAEMON}/cmd/daemon-pane/main.go": "packages/pi-legion/scripts/skill-scenarios/daemon-pane.go"}),
], reads=reads(
    ("packages/pi-legion/**", f"{at('pi-legion', 'Test pi-legion')} in packages/pi-legion"),
    ("packages/pi-envoy/**", "packages/pi-legion/extensions/legion.test.ts:77,319 and legion-role-claim.test.ts:14,100 import ../../pi-envoy/extensions/envoy; packages/pi-shared/test/omp-harness.ts:1072"),
    ("packages/pi-shared/src/**", "packages/pi-legion/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts"),
    ("packages/pi-shared/test/**", "packages/pi-legion/extensions/legion-phase-stall-omp.test.ts:14, legion-role-tools-omp.test.ts:13, src/skills-guard.test.ts:20, scripts/prepack.test.ts:4"),
    ("packages/envoy-client/test/subscribe-answer.ts", "packages/pi-legion/extensions/legion-role-claim.test.ts:46 "
                                                       "imports @legion/envoy-client/test/subscribe-answer, and "
                                                       "packages/pi-shared/test/omp-harness.ts:17, which this "
                                                       "package's omp tests drive, imports it too"),
    # envoy-client's sources, and cipher-client's with them, are the shared read below.
    (CONTRACTS_SRC, f"{at('pi-legion', 'Build contracts for pi-legion')}; tsconfig paths @legion/contracts"),
    (TEST_FIXTURES_SRC, "packages/pi-legion's extensions and src tests import @legion/test-fixtures"),
    (f"{FIX}/daemon-api/**", "packages/pi-legion/extensions/legion.test.ts:1440,4080, legion-role-claim.test.ts:21, src/daemon-api-version.test.ts:14"),
    (f"{FIX}/capability-redaction.json", "packages/pi-legion/src/capability-report.test.ts:848"),
    (f"{FIX}/capability-redaction-plugin-report.json", "packages/pi-legion/src/capability-report.test.ts:969 (PLUGIN_REPORT :854)"),
    ("packages/dispatch/e2e/**", "packages/pi-legion/scripts/skill-scenarios/rig-isolation.test.ts:83 names packages/dispatch/e2e/run-server.sh in the checkout it builds, and seed.ts:111, which that test names at :112, imports packages/dispatch/e2e/api.ts"),
    ("packages/dispatch/web/src/api/types.ts", "packages/dispatch/e2e/api.ts:20"),
    ("packages/dispatch/web/src/api/paths.ts", "packages/dispatch/e2e/api.ts:8-19 imports PathsOf, Op, PathParams, QueryParams, RequestBody, Query, Body and Answer from it, since that client is keyed on the generated document's path patterns"),
    ("skills/**", "packages/pi-legion/src/skills-guard.test.ts:27-29 stages them and :48,60 read their skill:// links; scripts/skill-scenarios/rig.test.ts:1297,1540; prepack stages skills"),
    ("scripts/pi-plugin-prepack.sh", "packages/pi-shared/test/skills-guard.ts:17,34"),
    ("scripts/third-party-notices.ts", "scripts/pi-plugin-prepack.sh:97,100"),
    (f"{DAEMON}/internal/prompts/**/*.md", "packages/pi-legion/src/skills-guard.test.ts:48,60 read the skill:// links of the daemon's prompts (promptsRoot :24)"),
    ("docs/site/src/content/docs/**", "packages/pi-legion/src/skills-guard.test.ts:82 reads the guides under docs/site/src/content/docs/legion"),
    ("scripts/e2e/lib/**", "packages/pi-legion/scripts/skill-scenarios/rig.test.ts:19 imports scripts/e2e/lib/script-functions and :700 sources scripts/e2e/lib/loopback-dsn.sh for the services_up row; rig.sh:333 sources scripts/e2e/lib/rig.sh"),
    ("packages/envoy/scripts/lib/pg-url.sh", "packages/pi-legion/scripts/skill-scenarios/rig.sh:337 sources it for pg_sql and safe_url; "
                                             "rig.test.ts:653 sources it for the safe_url row, :699 for the services_up row and :1541 copies it beside rig.sh, and "
                                             "rig-isolation.test.ts:77-79 copies it into its scratch checkout beside rig.sh"),
    (".omp-pin", "packages/pi-legion/scripts/skill-scenarios/rig.test.ts:1538-1545 copies .omp-pin"),
    ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts", "writes the overlay and runs go run (:29-30)"),
) + envoy_client_dep("packages/pi-legion/tsconfig.json paths @legion/envoy-client/*; scripts/skill-scenarios/*.ts "
                     "import @legion/envoy-client/*")
  + install_jj("pi-legion") + install_omp("pi-legion") + setup_go("pi-legion", f"{DAEMON}/go.mod")
  + bun_job("pi-legion"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter pi_legion held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 277 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
