# The private-projects acceptance drivers. The blob step runs project-access-blob.sh, which
# sources scripts/e2e/lib/two-builds.sh, whose head build compiles this tree's ./cmd/dispatch;
# its base build compiles the merge base's tree, taken out of git history, so it reads no file
# of this one. The --all-public and --private-by-sql steps run project-access.sh, which builds
# this tree's ./cmd/dispatch and NATS; the --private-by-sql step's walk runs the route Probes.
# The sweep's step runs restricted-sweep.test.ts, whose stacks build this tree's ./cmd/dispatch
# (e2e/run-server.sh) and run the route Probes, and whose control builds a commit out of git
# history, as the blob step's base build does; its Postgres connection limit is read first.
# why: .github/ci-notes/ci.md#the-private-projects-blob-drivers-job
GATE = gate("dispatch_project_access", PROJECT_ACCESS, exact=[
    pinned_step(PROJECT_ACCESS, PROJECT_ACCESS_CONNECTIONS_STEP, ENVOY, BROWSER_CONNECTIONS_SCRIPT, [
        (ENVOY, ["./internal/dispatch/store/cmd/connections"],
         "go run ./internal/dispatch/store/cmd/connections, which prints testlimits.ServerConnections",
         {"mode": "build"}),
    ]),
], runs=[
    script_run(TWO_BUILDS, TWO_BUILDS_HEAD_BUILD, f"run by {at(PROJECT_ACCESS, PROJECT_ACCESS_BLOB_STEP)} "
               "through packages/dispatch/e2e/acceptance/project-access-blob.sh's two_builds_build",
               ENVOY, ["./cmd/dispatch"], mode="build"),
    script_run(PROJECT_ACCESS_DRIVER, PROJECT_ACCESS_BUILD,
               f"run by {at(PROJECT_ACCESS, PROJECT_ACCESS_STEP)} and {at(PROJECT_ACCESS, PROJECT_ACCESS_PRIVATE_STEP)}",
               ENVOY, ["./cmd/dispatch", "./internal/testnats/cmd/natsd"], mode="build"),
    script_run(PROJECT_ACCESS_DRIVER, PROJECT_ACCESS_PROBES,
               f"run by {at(PROJECT_ACCESS, PROJECT_ACCESS_PRIVATE_STEP)}'s walk",
               ENVOY, ["./internal/dispatch/api/cmd/routeprobes"], mode="build"),
    ceiling_run(PROJECT_ACCESS),
], reads=reads(
    ("packages/dispatch/e2e/acceptance/project-access-blob.sh", at(PROJECT_ACCESS, PROJECT_ACCESS_BLOB_STEP)),
    (PROJECT_ACCESS_DRIVER, f"{at(PROJECT_ACCESS, PROJECT_ACCESS_STEP)} and {at(PROJECT_ACCESS, PROJECT_ACCESS_PRIVATE_STEP)}"),
    ("packages/dispatch/e2e/acceptance/project-access-walk.sh", f"{PROJECT_ACCESS_DRIVER} sources it"),
    ("packages/dispatch/e2e/fake-github.ts", f"{PROJECT_ACCESS_DRIVER}'s services section runs it with bun"),
    ("packages/dispatch/e2e/acceptance/socket-access.ts",
     f"{PROJECT_ACCESS_DRIVER}'s and packages/dispatch/e2e/acceptance/project-access-blob.sh's sockets sections run it with bun"),
    ("packages/dispatch/e2e/fake-envoy.ts", "`scripts/e2e/lib/two-builds.sh#two_builds_fake_envoy` runs it with bun"),
    ("packages/dispatch/e2e/harness-ports.ts", "`packages/dispatch/e2e/fake-envoy.ts#harnessPorts` imports it"),
    ("packages/dispatch/e2e/acceptance/access-lib.sh", "packages/dispatch/e2e/acceptance/project-access-blob.sh sources it"),
    (CONTRACTS_SRC, "`packages/dispatch/e2e/fake-envoy.ts#LEGION_ROLES` imports @legion/contracts, which Bun resolves to its "
                    "src (packages/contracts/package.json's bun export)"),
    ("packages/contracts/package.json", "Bun reads its exports to resolve the import of @legion/contracts that "
                                        "`packages/dispatch/e2e/fake-envoy.ts#LEGION_ROLES` makes"),
    (TWO_BUILDS, "packages/dispatch/e2e/acceptance/project-access-blob.sh sources it"),
    ("scripts/e2e/lib/rig.sh", f"{TWO_BUILDS} sources it"),
    ("scripts/e2e/lib/loopback-dsn.sh", "scripts/e2e/lib/rig.sh sources it"),
    ("scripts/e2e/lib/loopback-dsn.py", "`scripts/e2e/lib/loopback-dsn.sh#_loopback_dsn` runs it"),
    ("scripts/e2e/lib/port-file.sh", "scripts/e2e/lib/rig.sh sources it"),
    ("scripts/e2e/lib/held-port.py", "`scripts/e2e/lib/rig.sh#_held_exec` runs it for `scripts/e2e/lib/rig.sh#start_held`, "
                                     "which `scripts/e2e/lib/two-builds.sh#start_server` holds each Dispatch server's port "
                                     "through"),
    (TEST_FIXTURES_SRC, "packages/dispatch/e2e/__tests__/restricted-sweep.test.ts and restricted-sweep-fake.ts import "
                        "@legion/test-fixtures, and the sweep's step runs restricted-sweep.test.ts"),
    ("scripts/e2e/lib/process-tree.sh", "scripts/e2e/lib/rig.sh sources it, in its last line: "
                                        "`scripts/e2e/lib/rig.sh#start_process` records each service's start through its "
                                        "record_start, and packages/dispatch/e2e/acceptance/access-lib.sh's stop_all stops "
                                        "them through its signalable and rig.sh's stop_pid"),
) + ceiling_reads(PROJECT_ACCESS) + setup_go(PROJECT_ACCESS, f"{ENVOY}/go.mod") + bun_job(PROJECT_ACCESS))
# The paths the filter held by hand before the filters were computed, beyond the closure of the go
# commands above: what every caller reads is the drivers' subject, so a change anywhere in the
# module is one they exist to see, and the e2e tree and the contracts are what their Bun halves load.
GATE = with_reads(GATE, [
    Read('packages/envoy/**', "filter dispatch_project_access held packages/envoy/** by hand before the filters were computed: what every caller reads is the drivers' subject (.github/ci-notes/ci.md#the-private-projects-blob-drivers-job)", but=('**/*_test.go',)),
    Read('packages/dispatch/e2e/**', "filter dispatch_project_access held packages/dispatch/e2e/** by hand before the filters were computed: the drivers, the fakes they start and restricted-sweep.test.ts with the helpers it imports"),
    Read('packages/contracts/**', "filter dispatch_project_access held packages/contracts/** by hand before the filters were computed: packages/dispatch/e2e/fake-envoy.ts imports @legion/contracts"),
])
