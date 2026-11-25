# The go_work_sum filter's gate: what job test reads.
GATE = harness("go_work_sum", "Check that every checksum a Go download needs is committed",
        f"{SCRIPTS}/check-go-work-sum.sh", [
    ("go.work", "the workspace this check lists the module graph of"),
    ("go.work.sum", "the file it holds complete: a hash missing here sends every image build's download to "
                    "sum.golang.org, whose failure killed the worker image's job 138667218057 and the listener "
                    "image's 138687314598"),
    (f"{DAEMON}/go.mod", "a module of the workspace: its requirements decide the graph, and the test job's "
                         "setup-go installs the Go that resolves it"),
    (f"{DAEMON}/go.sum", "a module's own hashes, which this check reads beside go.work.sum"),
    (f"{ENVOY}/go.mod", "a module of the workspace, as the daemon's is"),
    (f"{ENVOY}/go.sum", "a module's own hashes, as the daemon's are"),
    (f"{ENVOY}/infra/go.mod", "the infra module of the workspace, which the held-port image's build compiles"),
    (f"{ENVOY}/infra/go.sum", "that module's own hashes"),
])
