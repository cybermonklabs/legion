# The envoy_session_stall filter's gate: what job envoy-session-stall reads. Its steps are in the
# legs of envoy-session-stall-legs (STALL_LEGS) of the file it calls (STALL_FILE); the job
# envoy-session-stall there only restates their result. So each citation names the legs' job.
GATE = gate("envoy_session_stall", STALL, runs=[
    script_run("packages/envoy-client/test/envoy-stack.ts",
               'const spawned = Bun.spawn(["go", "build", "-o", `${build.bin}${path.sep}`, ...build.packages], {',
               f"buildGoBinaries, which both rigs of that directory call; this one's packages are the Envoy "
               f"stack's, built for {at(STALL_LEGS, STALL_CLIENT, STALL_FILE)}, "
               f"{at(STALL_LEGS, 'Stall test of pi-envoy', STALL_FILE)} and "
               f"{at(STALL_LEGS, 'Stall test of claude-envoy', STALL_FILE)}", ENVOY,
               ["./internal/testnats/cmd/natsd", "./cmd/listener"], mode="build"),
    script_run("packages/envoy-client/test/dispatch-stack.ts",
               'packages: ["./cmd/dispatch"],',
               f"the packages that rig hands buildGoBinaries, run by "
               f"{at(STALL_LEGS, 'Design-gate advice of the real CLI against a real Dispatch', STALL_FILE)}",
               ENVOY, ["./cmd/dispatch"], mode="build"),
    script_run("packages/envoy-client/src/__tests__/nats-credential.test.ts",
               'packages: ["./internal/testnats/cmd/natsd", "./internal/testnats/cmd/calloutd"],',
               f"the packages that test hands buildGoBinaries, run by "
               f"{at(STALL_LEGS, 'Broker credential tests on a hub behind the auth service', STALL_FILE)}",
               ENVOY, ["./internal/testnats/cmd/natsd", "./internal/testnats/cmd/calloutd"], mode="build"),
    ceiling_run(STALL_LEGS, STALL_FILE),
], reads=reads(
    ("packages/envoy-client/**", f"{at(STALL_LEGS, STALL_CLIENT, STALL_FILE)}: src/__tests__/nats-stall.test.ts and test/*.ts, which the other two import"),
    ("packages/pi-envoy/**", at(STALL_LEGS, "Stall test of pi-envoy", STALL_FILE)),
    (TEST_FIXTURES_SRC, "packages/pi-envoy/extensions/envoy.test.ts imports @legion/test-fixtures/environment"),
    ("packages/claude-envoy/**", f"{at(STALL_LEGS, 'Stall test of claude-envoy', STALL_FILE)} (tests/envoy-channel-stall.test.ts runs bin/envoy-channel.ts)"),
    ("packages/pi-shared/src/**", "packages/pi-envoy and packages/claude-envoy import @legion/pi-shared/*"),
    ("packages/contracts/**", at(STALL_LEGS, "Build contracts for the stall suites", STALL_FILE)),
    (BROKER_CLIENT_SRC, f"{at(STALL_LEGS, 'Stall test of claude-envoy', STALL_FILE)} runs packages/claude-envoy/bin/envoy-channel.ts, whose bun build loads six of @legion/cipher-client's seven modules through packages/envoy-client/src/broker-credential.ts's imports of brokerTokenClient, chooseSignIn and isFinalRefusal"),
) + ceiling_reads(STALL_LEGS, STALL_FILE) + crash_safety_reads(STALL_LEGS, STALL_FILE)
  + setup_go(STALL_LEGS, f"{ENVOY}/go.mod", STALL_FILE) + bun_job(STALL_LEGS, STALL_FILE))
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "filter envoy_session_stall held packages/envoy/** by hand before the filters were computed: it selected 1962 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/pi-shared/**', "filter envoy_session_stall held packages/pi-shared/** by hand before the filters were computed: it selected 13 file(s) no other read of this gate declares"),
])
