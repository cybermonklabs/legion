# The check_biome_tree_test filter's gate: what job test reads.
GATE = harness("check_biome_tree_test", "Test the whole-tree Biome check",
        f"{SCRIPTS}/check-biome-tree.test.sh", [
    (f"{SCRIPTS}/check-biome-tree.sh", "the script under test"),
])
# Every case but the stand-in ones runs the real Biome, the one bun.lock pins: the test job's
# frozen install puts it under node_modules, and each fixture links that directory in. So the
# step reads the install too, and a Biome upgrade runs it.
GATE = with_reads(GATE, bun_job("test"))
