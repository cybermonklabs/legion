# The conflicts_script filter's gate: what job conflicts-script reads.
GATE = gate("conflicts_script", "conflicts-script", reads=reads(
    ("scripts/conflicts/**", f"{at('conflicts-script', 'Lint the conflict measurement script')}, typecheck (tsc -p scripts/conflicts) "
                             "and test (__tests__/ and __fixtures__/); its modules import only one another, node: and bun:"),
) + biome(at("conflicts-script", "Lint the conflict measurement script")) + bun_job("conflicts-script"))
