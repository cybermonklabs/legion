# The go_legs_select_test filter's gate: what job test reads.
GATE = harness("go_legs_select_test", "Test which packages a change selects in the Go legs",
               f"{SCRIPTS}/go-legs-select.test.sh", [
    (f"{SCRIPTS}/go_legs_select.py", "the selection under test, run in a worktree of HEAD"),
    (f"{SCRIPTS}/go-legs-select.sh", "the affected job's wrapper, whose bases and switch a case drives"),
    (f"{SCRIPTS}/go_legs.py", "go_legs_select.py reads the legs file and each module's packages through it"),
    (GO_LEGS, "the legs the selection reads; a case moves a daemon package between two of its legs"),
    (CI, "a case changes the changes job and the affected job, which the selection compares"),
    (".github/workflows/ci-job-envoy-go-race.yaml", "a case changes a comment and the legs step of envoy-go-race"),
    ("packages/envoy/internal/bus/async.go", "a case changes the shared package's source"),
    ("packages/envoy/internal/bus/release_test.go", "a case changes the package's own test file"),
    ("packages/contracts/fixtures/session-grant.json", "a case changes the fixture natsgrant's //ci:reads names"),
    ("packages/envoy/natsgrant/*_test.go", "the //ci:reads line of session-grant.json, which the check case plants a "
                                           "dead glob in"),
    ("packages/dispatch/e2e/project-default-assignee.e2e.ts", "a case changes a dashboard file no legs test reads"),
    ("packages/envoy/go.mod", "a case adds a godebug line"),
    ("packages/envoy/go.sum", "a case adds a version of nats.go"),
])
