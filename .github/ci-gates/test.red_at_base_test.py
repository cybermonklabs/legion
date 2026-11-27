# The red_at_base_test filter's gate: what job test reads. The harness is not under
# .github/scripts and sources no test-lib.sh: it carries its own helpers. Its step runs after the
# job's setup-go, since the scratch repository it makes runs go test, and bun test, at a base
# commit.
GATE = harness("red_at_base_test", "Test the red-at-base tool (a scratch repository)",
               "scripts/red-at-base.test.sh", [
    ("scripts/red-at-base.sh", "the script under test, which the harness names"),
    ("scripts/red-at-base/**", "the Go programs the script under test builds (its build_program) and runs"),
], lib=False)
