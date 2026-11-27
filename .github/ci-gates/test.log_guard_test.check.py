# A second gate on log_guard_test: the step that tests check-log-guard.sh. One filter selects both
# guard harness steps, as tool-fallback-arm.py's does both tool-fallback jobs, and
# check-ci-filters.sh keys a claim by (job, step), so the two gates claim different steps and do not
# collide. The check reads every workflow, every composite action and every shell script a step of
# them runs, and the harness runs it over a copy of each.
GATE = harness("log_guard_test", "Test the check that every network command a step runs is guarded",
        f"{SCRIPTS}/check-log-guard.test.sh", [
    (f"{SCRIPTS}/check-log-guard.sh", "the script under test, symlinked into every fixture"),
    (f"{SCRIPTS}/shell_commands.py", "the command scanner the check imports, symlinked in beside it"),
    (f"{SCRIPTS}/ci-log-guard.sh", "check-log-guard.test.sh sources it to run the reader's forms under bash"),
    (f"{SCRIPTS}/shell_reader_forms.py", "check-log-guard.test.sh runs the generated forms under bash and both readers"),
    (".github/shell-reader-misses.tsv", "shell_reader_forms.py holds the forms both readers miss to it"),
    (".github/log-guard-allow.tsv", "check-log-guard.test.sh runs the check over a copy of this repository's .github"),
    (".github/log-guard-unread.tsv", "check-log-guard.test.sh runs the check over a copy of this repository's .github"),
    (".github/workflows/**", "check-log-guard.test.sh runs the check over a copy of this repository's .github"),
    (".github/actions/**", "check-log-guard.test.sh runs the check over a copy of this repository's .github"),
    ("**/*.sh", "check-log-guard.test.sh runs the check over a copy of every script a step runs, which it reads"),
])
