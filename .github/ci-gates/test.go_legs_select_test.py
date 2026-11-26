# The go_legs_select_test filter's gate: what job test reads. The harness builds its own fixture
# trees and copies the scripts the selection runs into each, so it reads those scripts alone.
GATE = harness("go_legs_select_test", "Test which packages a change selects in the Go legs",
               f"{SCRIPTS}/go-legs-select.test.sh", [
    (f"{SCRIPTS}/go_legs_select.py", "the selection under test, copied into each fixture tree"),
    (f"{SCRIPTS}/go-legs-select.sh", "the affected job's wrapper, copied into each fixture tree, whose bases and "
                                     "switch a case drives"),
    (f"{SCRIPTS}/go_legs.py", "go_legs_select.py reads the legs file and each module's packages through it"),
    (f"{SCRIPTS}/ci_filter_coverage.py", "go_legs_select.py runs its go commands in its Session and reads the "
                                         "workflows through its readers"),
    (f"{SCRIPTS}/ci_filter_sources.py", "go_legs_select.py reads each //ci: line with its grammar"),
    (".github/ci-gates/**", "ci_filter_sources.py, which go_legs_select.py imports, loads every gate file"),
    (BUILD_TAGS, "go_legs_select.py decodes go list's stream with it, and ci_filter_coverage.py imports it"),
    (f"{SCRIPTS}/workflow_paths.py", "go_legs_select.py matches a //ci: glob with its glob_regex, and "
                                     "ci_filter_coverage.py imports it"),
])
