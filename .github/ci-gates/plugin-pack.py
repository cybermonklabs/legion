# The plugin-pack job: the three tests of internal/daemon in PLUGIN_PACK_TESTS, which the
# daemon-go race step skips by name - the one that packs the two plugin releases and loads
# them, and the two prompt probes of agentmodels_omp_test.go, one of which stages the prompts
# this checkout ships as the release stages them. The step runs those tests alone,
# and compiles the package they are in to do it, so this filter is that package's whole test
# closure as well as what the three read: the two plugin packages, what their prepack builds
# each bundle from, skills/, and the lockfile the frozen install reads. The job exists for
# those reads: with the tests here, a change to the lockfile or to one skill runs them
# instead of the daemon's whole Go suite (dispatch://INFRA-1413).
GATE = gate("plugin_pack", PLUGIN_PACK_JOB, exact=[
    pinned_step(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP, DAEMON, PLUGIN_PACK_SCRIPT, [
        (DAEMON, [f"./{PLUGIN_PACK_PACKAGE}/"], "go test -run over the package the three tests are in", {}),
    ]),
], reads=[
    Read(g, f"{at(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP)}: "
            "`packages/daemon/internal/daemon/promptreferences_omp_test.go#TestTheRealPluginsOnTheRealOhMyPi` and `packages/daemon/internal/daemon/promptreferences_omp_test.go#packedPlugin` "
            "packs packages/pi-envoy and packages/pi-legion with `bun pm pack`, which runs their prepack")
    for g in PACK_READS
] + reads(
    ("packages/envoy-client/scripts/build-dispatch-cli.sh",
     "`scripts/pi-plugin-prepack.sh#$root/packages/envoy-client/scripts/build-dispatch-cli.sh`, run by the pack "
     "in `packages/daemon/internal/daemon/promptreferences_omp_test.go#packedPlugin`"),
    # The prompts this checkout ships, laid out for the agent-model probe as the release lays
    # them out: these were a RULES row of agentmodels_omp_test.go while the daemon-go race
    # step ran that test, which put skills/ on that job's filter and ran 1,083 s of Go tests
    # that could not see a skill (dispatch://INFRA-1413).
    ("packages/pi-legion/agents/**", f"{at(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP)}: "
                                     "`packages/daemon/internal/daemon/agentmodels_omp_test.go#shippedPromptPlugin` copies it with copyDir"),
    ("scripts/pi-plugin-prepack.sh", f"{at(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP)}: "
                                     "`packages/daemon/internal/daemon/agentmodels_omp_test.go#stageSkills` runs it with --stage-skills"),
    ("skills/**", f"{at(PLUGIN_PACK_JOB, PLUGIN_PACK_STEP)}: `packages/daemon/internal/daemon/agentmodels_omp_test.go#stageSkills`, "
                  "whose prepack stages skills/<name>"),
) + install_omp(PLUGIN_PACK_JOB)
  + setup_go(PLUGIN_PACK_JOB, f"{DAEMON}/go.mod") + bun_job(PLUGIN_PACK_JOB) + go_test_runner(PLUGIN_PACK_JOB))
GATE = with_reads(GATE, [
    Read('packages/daemon/**', "filter plugin_pack held packages/daemon/** by hand before the filters were computed: it selected 865 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/delivery/**', "filter plugin_pack held packages/envoy/internal/dispatch/delivery/** by hand before the filters were computed: it selected 2 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/dispatch/pmdoc/**', "filter plugin_pack held packages/envoy/internal/dispatch/pmdoc/** by hand before the filters were computed: it selected 277 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
