# The envoy_session_stall filter's gate: what job envoy-session-stall reads. The rows of the
# artifact dry run run in a second job of the file it calls, artifact-dry-run, whose id is not
# ci.yaml's, so each citation of that job's steps names the file (STALL_FILE).
STALL_FILE = job_file(STALL)
STALL_DRY_RUN = "artifact-dry-run"
STALL_DRY_RUN_STEP = "The artifact dry run held to the call against a real Dispatch"
GATE = gate("envoy_session_stall", STALL, runs=[
    script_run("packages/envoy-client/test/envoy-stack.ts",
               'const spawned = Bun.spawn(["go", "build", "-o", `${build.bin}${path.sep}`, ...build.packages], {',
               f"buildGoBinaries, which both rigs of that directory call; this one's packages are the Envoy "
               f"stack's, built for {at(STALL, STALL_CLIENT)}, {at(STALL, 'Stall test of pi-envoy')} and "
               f"{at(STALL, 'Stall test of claude-envoy')}", ENVOY, ["./internal/testnats/cmd/natsd", "./cmd/listener"], mode="build"),
    script_run("packages/envoy-client/test/dispatch-stack.ts",
               'packages: ["./cmd/dispatch"],',
               f"the packages that rig hands buildGoBinaries, run by "
               f"{at(STALL, 'Design-gate advice of the real CLI against a real Dispatch')} and by "
               f"{at(STALL_DRY_RUN, STALL_DRY_RUN_STEP, STALL_FILE)}",
               ENVOY, ["./cmd/dispatch"], mode="build"),
    script_run("packages/envoy-client/src/__tests__/nats-credential.test.ts",
               'packages: ["./internal/testnats/cmd/natsd", "./internal/testnats/cmd/calloutd"],',
               f"the packages that test hands buildGoBinaries, run by "
               f"{at(STALL, 'Broker credential tests on a hub behind the auth service')}",
               ENVOY, ["./internal/testnats/cmd/natsd", "./internal/testnats/cmd/calloutd"], mode="build"),
    ceiling_run(STALL),
    ceiling_run(STALL_DRY_RUN, STALL_FILE),
], reads=reads(
    ("packages/envoy-client/**", f"{at(STALL, STALL_CLIENT)}: src/__tests__/nats-stall.test.ts and test/*.ts, which the other two import"),
    ("packages/pi-envoy/**", at(STALL, "Stall test of pi-envoy")),
    (TEST_FIXTURES_SRC, "packages/pi-envoy/extensions/envoy.test.ts imports @legion/test-fixtures/environment"),
    ("packages/claude-envoy/**", f"{at(STALL, 'Stall test of claude-envoy')} (tests/envoy-channel-stall.test.ts runs bin/envoy-channel.ts)"),
    ("packages/pi-shared/src/**", "packages/pi-envoy and packages/claude-envoy import @legion/pi-shared/*"),
    ("packages/contracts/**", f"{at(STALL, 'Build contracts for the stall suites')} and "
                              f"{at(STALL_DRY_RUN, 'Build contracts for the artifact dry run', STALL_FILE)}"),
    (BROKER_CLIENT_SRC, f"{at(STALL, 'Stall test of claude-envoy')} runs packages/claude-envoy/bin/envoy-channel.ts, whose bun build loads six of @legion/cipher-client's seven modules through packages/envoy-client/src/broker-credential.ts's imports of brokerTokenClient, chooseSignIn and isFinalRefusal"),
    (f"{ENVOY}/internal/dispatch/api/testdata/text-refusals.json", f"read at `packages/envoy-client/src/__tests__/artifact-dry-run.live.test.ts#rows` and `packages/envoy-client/src/__tests__/artifact-dry-run.live.test.ts#textRefusalsFile`, from the new URL textRefusalsFile holds, by {at(STALL_DRY_RUN, STALL_DRY_RUN_STEP, STALL_FILE)}: its rows are the list's rows in each combination it runs"),
) + ceiling_reads(STALL) + crash_safety_reads(STALL) + setup_go(STALL, f"{ENVOY}/go.mod") + bun_job(STALL)
  + ceiling_reads(STALL_DRY_RUN, STALL_FILE) + crash_safety_reads(STALL_DRY_RUN, STALL_FILE)
  + setup_go(STALL_DRY_RUN, f"{ENVOY}/go.mod", STALL_FILE) + bun_job(STALL_DRY_RUN, STALL_FILE))
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "filter envoy_session_stall held packages/envoy/** by hand before the filters were computed: it selected 1962 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/pi-shared/**', "filter envoy_session_stall held packages/pi-shared/** by hand before the filters were computed: it selected 13 file(s) no other read of this gate declares"),
])
