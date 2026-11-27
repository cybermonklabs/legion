# The test_fixtures filter's gate: what job test-fixtures reads.
GATE = gate("test_fixtures", "test-fixtures", reads=reads(
    ("packages/test-fixtures/**", f"{at('test-fixtures', 'Typecheck the shared test fixtures')} and test in "
                                  "packages/test-fixtures (its own biome.json among them); its modules import only "
                                  "one another, node: and bun:"),
) + install_jj("test-fixtures") + bun_job("test-fixtures"))
