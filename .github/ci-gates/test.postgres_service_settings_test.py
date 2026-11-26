# The postgres_service_settings_test filter's gate: what job test reads.
GATE = harness("postgres_service_settings_test", "Test the Postgres service settings and their read-back",
               f"{SCRIPTS}/postgres-service-settings.test.sh", [
    (f"{SCRIPTS}/postgres-service-settings.sh", "the script under test"),
])
