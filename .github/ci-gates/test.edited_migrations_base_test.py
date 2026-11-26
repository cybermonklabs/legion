# The edited_migrations_base_test filter's gate: what job test reads.
GATE = harness("edited_migrations_base_test", "Test the edited-migrations check's choice of base (fixture git remotes; no GitHub)",
        f"{SCRIPTS}/edited-migrations-base.test.sh", [
    (EDITED_MIGRATIONS_BASE, "the script under test"),
    (f"{SCRIPTS}/fetch-depth-if-shallow.sh", "the script under test fetches every commit through it"),
])
