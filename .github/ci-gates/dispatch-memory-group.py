# The dispatch_memory_group filter's gate: what job dispatch-memory-group reads. Its steps are a
# job of ci-job-dispatch-memory.yaml, beside the memory suite's (SHARED_FILE in ci_job_reads.py).
GATE = gate("dispatch_memory_group", "dispatch-memory-group", exact=[
    pinned_step("dispatch-memory-group", MEMORY_GROUP_STEP, ENVOY, MEMORY_GROUP_SCRIPT, [
        (ENVOY, ["./cmd/dispatch"], "go test ./cmd/dispatch", {}),
    ]),
], reads=reads(
    (".github/actions/pull-test-images/**", at("dispatch-memory-group", "Pull the container image the test runs the server in")),
) + setup_go("dispatch-memory-group", f"{ENVOY}/go.mod"))
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/**', "filter dispatch_memory_group held packages/envoy/internal/dispatch/** by hand before the filters were computed: it selected 900 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter dispatch_memory_group held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
