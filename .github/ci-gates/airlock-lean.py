# The airlock_lean filter's gate: what job airlock-lean reads.
GATE = gate("airlock_lean", AIRLOCK_JOB, exact=[
    pinned_step(AIRLOCK_JOB, AIRLOCK_SEED_STEP, ENVOY, AIRLOCK_SEED_SCRIPT, [
        (ENVOY, AIRLOCK_PKGS, "the proxyrules package's model comparisons: FuzzAirlock over the committed seed corpus, TestModelAgreesOnHostShapes and TestTheModelRefusesAHostlessEntry", {}),
    ]),
    pinned_step(AIRLOCK_JOB, AIRLOCK_FUZZ_STEP, ENVOY, AIRLOCK_FUZZ_SCRIPT, [
        (ENVOY, AIRLOCK_PKGS, "the same tests, fuzzed against the model for 120 seconds", {}),
    ]),
], reads=reads(
    (".github/actions/install-elan/**", at(AIRLOCK_JOB, "./.github/actions/install-elan")),
    (".github/ci-tools.yaml", "`.github/actions/install-elan/action.yml#ci_tools_fetch` installs the tarball of the elan row, or "
                              "of elan-aarch64 on an arm64 runner"),
    (".github/scripts/ci-tools.sh", "`.github/actions/install-elan/action.yml#$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh` sources it, and ci_tools_fetch "
                                    "reads that row's two sources"),
    (f"{AIRLOCK_LEAN}/**", f"{at(AIRLOCK_JOB, AIRLOCK_BUILD_STEP)}: lake build reads the model's whole project, and "
                           f"{at(AIRLOCK_JOB, AIRLOCK_AXIOMS_STEP)} runs Axioms.lean over the library it builds"),
    (f"{AIRLOCK_LEAN}/lean-toolchain", f"{at(AIRLOCK_JOB, 'elan-cache')}: the elan cache's key is hashFiles of it"),
    (AIRLOCK_AXIOMS_SCRIPT, f"{at(AIRLOCK_JOB, AIRLOCK_AXIOMS_STEP)} is one line running it, and check k15 of "
                            f"{ENVOY}/scripts/airlock-check.sh runs the same script, so the two cannot drift"),
) + setup_go(AIRLOCK_JOB, f"{ENVOY}/go.mod") + go_test_runner(AIRLOCK_JOB))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/cipher/proxyrules/**', "filter airlock_lean held packages/envoy/internal/cipher/proxyrules/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
