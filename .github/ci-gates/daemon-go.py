# The Go daemon's own suite, in a file of its own (.github/workflows/ci-job-daemon-go.yaml):
# the plain step over every package of the module but the five concurrent ones and the text
# checks, and the Stage 1 proof, which builds ./cmd/legion and boots it, in its job
# daemon-go, and the race step over those five in its job daemon-go-race, which runs at the
# same time; ci.yaml's daemon-go calls both, so one gate holds both. The filter is
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
GATE = gate("daemon_go_tests", "daemon-go", runs=[
    script_run("scripts/e2e/stage1-skeleton.sh", 'go build -o "$work/legion" ./cmd/legion',
               at("daemon-go", "Stage 1 live proof"), DAEMON, ["./cmd/legion"], mode="build"),
    forced_order_run("daemon-go", DAEMON_FORCED_ORDER_STEP, DAEMON, DAEMON_FORCED_ORDER_PKGS),
    # daemon-go-race sizes its Postgres for the Dispatch processes internal/daemon's
    # scratch-Dispatch tests start; the job is the file's second, which the citation names.
    ceiling_run("daemon-go-race", job_file("daemon-go")),
], exact=[
    # check-ci-filters.sh reduces only a one-line go test, so the step's whole script is
    # pinned, as the envoy-go unit step's is - and it carries a -skip besides, which is no
    # form that check reduces either. What it compiles is every test file of every package
    # it lists, the two skipped table pins' included: -skip leaves a test out of the run,
    # never out of the build, so the closure here is the step's whole closure. The Run names
    # the packages the step's own `grep -Ev` keeps (packages_kept over DAEMON_PLAIN_PATTERN),
    # rather than the superset ./...: the race step's Run below claims the five packages
    # this step leaves out, which are this job's own, while the text checks are the
    # text-check job's, and a Run saying ./... would keep what only their tests read -
    # docs/kubernetes.md - on this job's filter. Only they read it: the way back checks in
    # internal/daemon, which the race step runs, read upgrade_way_back_extract.txt and
    # upgrade_architect_entry_way_back_extract.txt beside them instead, and internal/textcheck's
    # way_back_doc_test.go holds those copies to the guide.
    pinned_step("daemon-go", DAEMON_PLAIN_STEP, DAEMON, DAEMON_PLAIN_SCRIPT, [
        (DAEMON, packages_kept(DAEMON, DAEMON_PLAIN_PATTERN),
         "go list ./... piped into go test, counted as go test over the packages its grep -Ev keeps, the "
         "supervise table's two pure pins skipped by name", {}),
    ]),
    # The race step carries a -skip, which is no form check-ci-filters.sh reduces either, so
    # its script is pinned too. What it compiles is every test file of the five packages,
    # the skipped tests' included: -skip leaves a test out of the run, never out of the
    # build, so the closure here is the step's whole closure and only those tests' own reads
    # (the plugin-pack gate, plugin-pack.py beside this file) leave this gate. The step is in
    # the file's second job, daemon-go-race, which the citation names.
    pinned_step("daemon-go-race", DAEMON_RACE_STEP, DAEMON, DAEMON_RACE_SCRIPT, [
        (DAEMON, DAEMON_RACE_PKGS, "go test -race over the five concurrent packages, the plugin-pack tests "
                                   "skipped by name", {"race": True}),
    ], workflow=job_file("daemon-go")),
    pinned_step("daemon-go", DAEMON_RACE_IMPOSSIBLE_STEP, DAEMON, DAEMON_RACE_IMPOSSIBLE_SCRIPT, [
        (DAEMON, [f"./{under}"], f"go test -run of what the race build cannot run in ./{under}", {})
        for under in DAEMON_RACE_IMPOSSIBLE_PACKAGES
    ]),
], reads=reads(
    ("scripts/e2e/stage1-skeleton.sh", at("daemon-go", "Stage 1 live proof")),
    ("scripts/e2e/lib/rig.sh", "`scripts/e2e/stage1-skeleton.sh#$root/scripts/e2e/lib/rig.sh` sources it"),
    ("scripts/e2e/lib/loopback-dsn.sh", "scripts/e2e/lib/rig.sh sources it (its `. \"$root/scripts/e2e/lib/loopback-dsn.sh\"` line)"),
    ("scripts/e2e/lib/port-file.sh", "scripts/e2e/lib/rig.sh sources it (its `. \"$root/scripts/e2e/lib/port-file.sh\"` line)"),
    ("scripts/e2e/lib/held-port.py", "scripts/e2e/lib/rig.sh's keep_port and _held_exec (start_held, start_kept) run it to hold a port for the daemon"),
    ("scripts/e2e/lib/process-tree.sh", "scripts/e2e/lib/rig.sh sources it (its `. \"$root/scripts/e2e/lib/process-tree.sh\"` line, the last)"),
    ("scripts/e2e/lib/built-from.sh", "`scripts/e2e/stage1-skeleton.sh#$root/scripts/e2e/lib/built-from.sh` runs it over the checkout"),
    ("packages/pi-legion/package.json", "`scripts/e2e/stage1-skeleton.sh#jq '{name, version, legion}'` copies its name, version "
                                        "and legion block into the pi-legion plugin the proof's home holds"),
    ("packages/pi-envoy/package.json", "`scripts/e2e/stage1-skeleton.sh#jq '{name, version}'` copies its name and version "
                                       "into the pi-envoy plugin the proof's home holds"),
    (".github/actions/install-uv/**", at("daemon-go", "./.github/actions/install-uv")),
    (".github/ci-tools.yaml", "`.github/actions/install-uv/action.yml#ci_tools_fetch uv` installs the uv row's binary"),
    (".github/scripts/ci-tools.sh", "`.github/actions/install-uv/action.yml#$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh` sources it, and ci_tools_fetch "
                                    "reads the uv row's two sources"),
    (".github/actions/pull-test-images/**", at("daemon-go", "Pull the NATS test container images")),
    (f"{DAEMON}/internal/testnats/nats.go", f"{at('daemon-go', 'Pull the NATS test container images')} const Image, "
                                            f"{at('daemon-go-race', 'Pull the NATS test container images', job_file('daemon-go'))} "
                                            f"consts Image, SkippingImage"),
    (f"{ENVOY}/internal/testnats/nats.go", f"{at('daemon-go-race', 'Pull the NATS test container images', job_file('daemon-go'))} "
                                           f"const HubImage"),
) + forced_order_reads("daemon-go", DAEMON_FORCED_ORDER_STEP) + ceiling_reads("daemon-go-race", job_file("daemon-go"))
  + install_jj("daemon-go") + install_omp("daemon-go") + setup_go("daemon-go", f"{DAEMON}/go.mod")
  + go_modules("daemon-go"))
GATE = with_reads(GATE, [
    Read('packages/daemon/**', "filter daemon_go_tests held packages/daemon/** by hand before the filters were computed: it selected 171 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipher/policy/**', "filter daemon_go_tests held packages/envoy/internal/cipher/policy/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/cipherclient/**', "filter daemon_go_tests held packages/envoy/internal/cipherclient/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter daemon_go_tests held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 278 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/text/**', "filter daemon_go_tests held packages/envoy/internal/dispatch/text/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/delivery/**', "filter daemon_go_tests held packages/envoy/internal/dispatch/delivery/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter daemon_go_tests held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
