# The tool scripts, in the order the job runs them: the fetch helper every install goes
# through, the filler that puts the bytes in the copy that helper reads, the gate that holds
# every row to its pin, and the rig that drives that helper against a fault in the
# tool-fallback job (main's #3576 gave that rig a harness of its own).
GATE = harness("ci_tools_test", "Test the tool fetch helper", f"{SCRIPTS}/ci-tools.test.sh", [
    (f"{SCRIPTS}/ci-tools.sh", "the script under test"),
    (".github/ci-tools.yaml", "ci-tools.test.sh:1278-1287 reads every row of this repository's own manifest"),
])
