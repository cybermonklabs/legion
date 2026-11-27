# The two-build job's Go step alone, behind a gate of its own, so the TAG_RUNS pairing below
# holds envoy_go to what this step reads - cmd/dispatch's twobuild closure, the CLI
# twobuild_cli_test.go runs and the scripts the step leans on - and not to the browser half's
# reads, which a change to cmd/dispatch's untagged tests never needs.
GATE = gate("dispatch_two_builds_go", TWO_BUILDS_JOB, step=TWO_BUILDS_STEP, runs=[ceiling_run(TWO_BUILDS_JOB)],
     exact=[two_builds_go_step(), two_builds_same_step()], reads=two_builds_go_reads())
GATE = with_reads(GATE, [
    Read('packages/envoy/internal/dispatch/**', "filter dispatch_two_builds_go held packages/envoy/internal/dispatch/** by hand before the filters were computed: it selected 687 file(s) no other read of this gate declares", but=('**/*_test.go',)),
    Read('packages/envoy/internal/contracts/**', "filter dispatch_two_builds_go held packages/envoy/internal/contracts/** by hand before the filters were computed: it selected 3 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
