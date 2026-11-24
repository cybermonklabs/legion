# Every case but the stand-in ones runs the real Biome, the one bun.lock pins: the test job's
# frozen install puts it under node_modules, and each fixture links that directory in. So the
# step reads the install too, and a Biome upgrade runs it.
GATE = gate("check_biome_tree_test", "test", step="Test the whole-tree Biome check", reads=reads(
    (f"{SCRIPTS}/check-biome-tree.test.sh", at("test", "Test the whole-tree Biome check")),
    (TEST_LIB, f"{SCRIPTS}/check-biome-tree.test.sh sources it for check/is/summary"),
    (f"{SCRIPTS}/check-biome-tree.sh", "the script under test"),
) + bun_job("test"))
