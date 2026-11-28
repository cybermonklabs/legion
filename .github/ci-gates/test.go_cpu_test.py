# The go_cpu_test filter's gate: what job test reads. The harness runs the wrapper over small bash
# and Python binaries of its own and, in W12, the real go over a scratch module, and the runner's
# start and report over scratch directories and a stand-in docker; it reads no network.
GATE = harness("go_cpu_test", "Test the Go CPU record's wrapper and runner", f"{SCRIPTS}/go-cpu.test.sh", [
    (f"{SCRIPTS}/go-cpu-exec.py", "the wrapper under test"),
    (f"{SCRIPTS}/go-cpu-runner.py", "the runner under test"),
])
