# The go_work_sum filter's gate: what job test reads.
GATE = harness("go_work_sum", "Check that every checksum a root workspace download needs is committed",
        f"{SCRIPTS}/check-go-work-sum.sh", [
    ("go.work", "the root workspace: its use list names the modules the check copies"),
    ("go.work.sum", "the root workspace's hashes: a missing hash sends a download there, such as the worker "
                    "image's, to sum.golang.org, whose failure killed job 138667218057"),
    (f"{DAEMON}/go.mod", "the daemon module's requirements, copied into the workspace for the download"),
    (f"{DAEMON}/go.sum", "the daemon module's hashes, copied beside its go.mod"),
    (f"{ENVOY}/go.mod", "the envoy module's requirements, copied into the workspace for the download"),
    (f"{ENVOY}/go.sum", "the envoy module's hashes, copied beside its go.mod"),
])
