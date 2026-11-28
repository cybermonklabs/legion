# The check_fireevent_reasons_test filter's gate: what job test reads.
GATE = harness("check_fireevent_reasons_test", "Test the fireEvent reason check",
        f"{SCRIPTS}/check-fireevent-reasons.test.sh", [
    (f"{SCRIPTS}/check-fireevent-reasons.sh", "the script under test"),
])
