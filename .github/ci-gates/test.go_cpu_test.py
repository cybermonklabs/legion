# The go_cpu_test filter's gate: what job test reads. The harness runs the wrapper over small bash
# and Python binaries of its own and, in W12, the real go over a scratch module, and the runner's
# start and report over scratch directories and a stand-in docker, and its parsers over files
# captured from real runners; it reads no network.
GATE = harness("go_cpu_test", "Test the Go CPU record's wrapper and runner", f"{SCRIPTS}/go-cpu.test.sh", [
    (f"{SCRIPTS}/go-cpu-exec.py", "the wrapper under test"),
    (f"{SCRIPTS}/go-cpu-runner.py", "the runner under test"),
    (f"{SCRIPTS}/testdata/go-cpu/**", "the runners' files R1, R3 and R4 parse: lscpu, cpuinfo, /proc/stat, cpu.stat and the log lines printed from them"),
    (f"{DAEMON}/go.mod", "the test job's setup-go installs its Go from it, which W12 runs: it holds go test and go run to running each binary through the wrapper, and go vet and go list to ignoring -exec"),
])
