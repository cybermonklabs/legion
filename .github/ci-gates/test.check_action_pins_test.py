# The check_action_pins_test filter's gate: what job test reads.
GATE = harness("check_action_pins_test", "Test the action pin check", f"{SCRIPTS}/check-action-pins.test.sh", [
    (f"{SCRIPTS}/check-action-pins.sh", "the script under test"),
])
