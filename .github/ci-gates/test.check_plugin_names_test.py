# The check_plugin_names_test filter's gate: what job test reads.
GATE = harness("check_plugin_names_test", "Test the plugin-name check", f"{SCRIPTS}/check-plugin-names.test.sh", [
    (f"{SCRIPTS}/check-plugin-names.sh", "the script under test"),
])
