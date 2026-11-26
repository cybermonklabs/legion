# The Go daemon's own suite, in a file of its own (.github/workflows/ci-job-daemon-go.yaml):
# the plain step over every package of the module but the five concurrent ones and the text
# checks, and the Stage 1 proof, which builds ./cmd/legion and boots it, in the legs of its job
# daemon-go-legs, and the race step over those five in the legs of its job daemon-go-race-legs,
# every leg at the same time (.github/go-legs.yaml lists each leg's packages); its jobs daemon-go
# and daemon-go-race report each kind's legs under the check-run names the two steps had before.
# ci.yaml's daemon-go calls the file, so one gate holds all of it. The filter is
# daemon_go_tests, the list that already stood for exactly this closure while it was aliased
# into the test filter and gated nothing of its own. The tests
# the race step does not run are the plugin-pack job's (PLUGIN_PACK_TESTS), whose gate is
# plugin-pack.py: with them goes the Bun install, which no other test of this module needs, and with
# that the lockfile and the workspace manifests, and with them skills/ and the prepack script
# that stages each plugin's partition of it, which no other test of this module reads.
# The plain step leaves two tests out as well, internal/supervise's table pins
# (SUPERVISE_TABLE_TESTS), which the text-check job runs by name: they read that package's
# source and the source of everything it imports, which their type-checker reads from source
# too (`packages/daemon/internal/supervise/table_test.go#importer.ForCompiler`) and which of this repository
# is packages/daemon's own packages, so no read leaves this job with them.
# Every step here is in a job of the file daemon-go calls whose id is not ci.yaml's, so each
# citation names that file (DAEMON_FILE).
DAEMON_FILE = job_file("daemon-go")
GATE = gate("daemon_go_tests", "daemon-go", runs=[
    script_run("scripts/e2e/stage1-skeleton.sh", 'go build -o "$work/legion" ./cmd/legion',
               at(DAEMON_PLAIN_LEGS_JOB, "Stage 1 live proof", DAEMON_FILE), DAEMON, ["./cmd/legion"], mode="build"),
    forced_order_run(DAEMON_PLAIN_LEGS_JOB, DAEMON_FORCED_ORDER_STEP, DAEMON, DAEMON_FORCED_ORDER_PKGS, DAEMON_FILE),
    # The race legs size their Postgres for the Dispatch processes internal/daemon's
    # scratch-Dispatch tests start.
    ceiling_run(DAEMON_RACE_LEGS_JOB, DAEMON_FILE),
], exact=[
    # check-ci-filters.sh reduces only a one-line go test, so each legs step's whole script is
    # pinned, as the envoy-go unit step's is: its packages come from .github/scripts/go_legs.py
    # through xargs, and it carries a -skip besides, neither of which is a form that check
    # reduces. Each Run names every package of every leg of its kind, as go-legs.yaml lists them:
    # each leg runs the step over its own share, so the legs together compile and run those
    # packages, each once. What a leg compiles is every test file of every package it runs, the
    # skipped tests' included: -skip leaves a test out of the run, never out of the build, so the
    # closure here is the step's whole closure and only those tests' own reads (the plugin-pack
    # gate, plugin-pack.py beside this file) leave this gate. The plain Run names the packages
    # the legs list rather than the superset ./...: the race Run claims the five packages the
    # plain legs leave out, which are this gate's own, while the text checks are the text-check
    # job's, and a Run saying ./... would keep what only their tests read - docs/kubernetes.md -
    # on this job's filter. Only they read it: the way back checks in internal/daemon, which a
    # race leg runs, read upgrade_way_back_extract.txt and
    # upgrade_architect_entry_way_back_extract.txt beside them instead, and internal/textcheck's
    # way_back_doc_test.go holds those copies to the guide.
    pinned_step(DAEMON_PLAIN_LEGS_JOB, DAEMON_PLAIN_STEP, DAEMON, DAEMON_PLAIN_SCRIPT, [
        (DAEMON, [f"./{p}/" for p in DAEMON_PLAIN_PACKAGES],
         f"go test over each plain leg's packages of {GO_LEGS}, the supervise table's two pure pins skipped by name",
         {}),
    ], workflow=DAEMON_FILE),
    pinned_step(DAEMON_RACE_LEGS_JOB, DAEMON_RACE_STEP, DAEMON, DAEMON_RACE_SCRIPT, [
        (DAEMON, [f"./{p}/" for p in DAEMON_RACE_LEG_PACKAGES],
         f"go test -race over each race leg's packages of {GO_LEGS}, the plugin-pack tests skipped by name",
         {"race": True}),
    ], workflow=DAEMON_FILE),
    pinned_step(DAEMON_PLAIN_LEGS_JOB, DAEMON_RACE_IMPOSSIBLE_STEP, DAEMON, DAEMON_RACE_IMPOSSIBLE_SCRIPT, [
        (DAEMON, [f"./{under}"], f"go test -run of what the race build cannot run in ./{under}", {})
        for under in DAEMON_RACE_IMPOSSIBLE_PACKAGES
    ], workflow=DAEMON_FILE),
    # The shape gate's second run, stamped with HEAD's merge order: the step's whole script is
    # pinned (ci_job_reads.py's DAEMON_STAMPED_GATE_SCRIPT says why). What it compiles is
    # internal/api's test build, which plain leg 2's step compiles too; the stamp changes the link
    # alone.
    pinned_step(DAEMON_PLAIN_LEGS_JOB, DAEMON_STAMPED_GATE_STEP, DAEMON, DAEMON_STAMPED_GATE_SCRIPT, [
        (DAEMON, ["./internal/api/"], "go test of internal/api's two shape tests, linked with HEAD's stamp", {}),
    ], workflow=DAEMON_FILE),
], reads=reads(
    (GO_LEGS_SCRIPT, at(DAEMON_PLAIN_LEGS_JOB, DAEMON_PLAIN_STEP, DAEMON_FILE)),
    (GO_LEGS, f"{GO_LEGS_SCRIPT} reads each leg's packages from it"),
    (f"{GO_LEGS_GROUPS}/**", f"{GO_LEGS_SCRIPT} reads the groups of a split package's tests from "
                             f"{GO_LEGS_GROUPS}/<module>/<package>.yaml, one per leg that runs a share of it "
                             f"({at(DAEMON_RACE_LEGS_JOB, DAEMON_RACE_STEP, DAEMON_FILE)}, internal/daemon)"),
    ("scripts/daemon-contract-order.sh", at(DAEMON_PLAIN_LEGS_JOB, DAEMON_CONTRACT_ORDER_STEP, DAEMON_FILE)),
    ("scripts/e2e/stage1-skeleton.sh", at(DAEMON_PLAIN_LEGS_JOB, "Stage 1 live proof", DAEMON_FILE)),
    ("scripts/e2e/lib/rig.sh", "`scripts/e2e/stage1-skeleton.sh#$root/scripts/e2e/lib/rig.sh` sources it"),
    ("scripts/e2e/lib/loopback-dsn.sh", "scripts/e2e/lib/rig.sh sources it (its `. \"$root/scripts/e2e/lib/loopback-dsn.sh\"` line)"),
    ("scripts/e2e/lib/port-file.sh", "scripts/e2e/lib/rig.sh sources it (its `. \"$root/scripts/e2e/lib/port-file.sh\"` line)"),
    ("scripts/e2e/lib/held-port.py", "scripts/e2e/lib/rig.sh's keep_port and _held_exec (start_held, start_kept) run it to hold a port for the daemon"),
    ("scripts/e2e/lib/process-tree.sh", "scripts/e2e/lib/rig.sh sources it (its `. \"$root/scripts/e2e/lib/process-tree.sh\"` line, the last)"),
    ("scripts/e2e/lib/built-from.sh", "`scripts/e2e/stage1-skeleton.sh#$root/scripts/e2e/lib/built-from.sh` runs it over the checkout"),
    ("packages/pi-legion/package.json", "`scripts/e2e/stage1-skeleton.sh#{name, version, legion}` copies its name, version "
                                        "and legion block, with this tree's contract written in it, into the pi-legion plugin the proof's home holds"),
    ("packages/pi-envoy/package.json", "`scripts/e2e/stage1-skeleton.sh#jq '{name, version}'` copies its name and version "
                                       "into the pi-envoy plugin the proof's home holds"),
    (".github/actions/install-uv/**", at(DAEMON_PLAIN_LEGS_JOB, "./.github/actions/install-uv", DAEMON_FILE)),
    (".github/ci-tools.yaml", "`.github/actions/install-uv/action.yml#ci_tools_fetch uv` installs the uv row's binary"),
    (".github/scripts/ci-tools.sh", "`.github/actions/install-uv/action.yml#$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh` sources it, and ci_tools_fetch "
                                    "reads the uv row's two sources"),
    (".github/actions/pull-test-images/**", at(DAEMON_PLAIN_LEGS_JOB, "Pull the NATS and PgBouncer test container images", DAEMON_FILE)),
    (f"{DAEMON}/internal/testnats/nats.go", f"{at(DAEMON_PLAIN_LEGS_JOB, 'Pull the NATS and PgBouncer test container images', DAEMON_FILE)} "
                                            f"const Image, "
                                            f"{at(DAEMON_RACE_LEGS_JOB, 'Pull the NATS test container images', DAEMON_FILE)} "
                                            f"consts Image, SkippingImage"),
    (f"{ENVOY}/internal/testnats/nats.go", f"{at(DAEMON_RACE_LEGS_JOB, 'Pull the NATS test container images', DAEMON_FILE)} "
                                           f"const HubImage"),
    (f"{DAEMON}/internal/ompsessions/searchpath_test.go", f"{at(DAEMON_PLAIN_LEGS_JOB, 'Pull the NATS and PgBouncer test container images', DAEMON_FILE)} "
                                                          f"const PoolImage"),
) + forced_order_reads(DAEMON_PLAIN_LEGS_JOB, DAEMON_FORCED_ORDER_STEP, DAEMON_FILE)
  + ceiling_reads(DAEMON_RACE_LEGS_JOB, DAEMON_FILE)
  + service_settings_reads(DAEMON_PLAIN_LEGS_JOB, DAEMON_FILE) + service_settings_reads(DAEMON_RACE_LEGS_JOB, DAEMON_FILE)
  + install_jj(DAEMON_PLAIN_LEGS_JOB, DAEMON_FILE) + install_omp(DAEMON_PLAIN_LEGS_JOB, DAEMON_FILE)
  + setup_go(DAEMON_PLAIN_LEGS_JOB, f"{DAEMON}/go.mod", DAEMON_FILE)
  + go_modules(DAEMON_PLAIN_LEGS_JOB, DAEMON_FILE))
GATE = with_reads(GATE, [
    Read('packages/daemon/**', "filter daemon_go_tests held packages/daemon/** by hand before the filters were computed: it selected 171 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipher/policy/**', "filter daemon_go_tests held packages/envoy/internal/cipher/policy/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipherclient/**', "filter daemon_go_tests held packages/envoy/internal/cipherclient/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter daemon_go_tests held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 278 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/text/**', "filter daemon_go_tests held packages/envoy/internal/dispatch/text/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/delivery/**', "filter daemon_go_tests held packages/envoy/internal/dispatch/delivery/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter daemon_go_tests held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
