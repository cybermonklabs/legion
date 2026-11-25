# The pi_shared filter's gate: what job pi-shared reads.
GATE = gate("pi_shared", "pi-shared", reads=reads(
    ("packages/pi-shared/**", f"{at('pi-shared', 'Typecheck pi-shared')} and test in packages/pi-shared"),
    # envoy-client's sources, and cipher-client's with them, are the shared read below.
    ("packages/envoy-client/test/fake-dispatch.ts", "packages/pi-shared/src/handoff-record.test.ts:10 imports @legion/envoy-client/test/fake-dispatch"),
    (CONTRACTS_SRC, "pi-shared and envoy-client import @legion/contracts (contracts exports.bun -> src)"),
    (TEST_FIXTURES_SRC, "packages/pi-shared's src and test suites import @legion/test-fixtures"),
    (f"{FIX}/daemon-api/**", "packages/pi-shared/src/daemon-client.test.ts:23, legion-claim-core.test.ts:17, legion-phase-ops.test.ts:32"),
    ("packages/pi-legion/src/**", "packages/pi-shared/src/daemon-client.test.ts:1749-1754,1899-1910 walks every .ts of it that is no test, holding LEGION_ANSWER_READS to the fields a read in it names"),
    ("packages/pi-legion/extensions/**", "packages/pi-shared/src/daemon-client.test.ts:1749-1754,1899-1910 walks every .ts of it that is no test, as above"),
    ("packages/claude-envoy/src/**", "packages/pi-shared/src/daemon-client.test.ts:1749-1754,1899-1910 walks every .ts of it that is no test, as above"),
    ("skills/**", "packages/pi-shared/test/skills-partition.test.ts:22,33-36,53-54 reads every skills/ directory and stages both partitions"),
    ("scripts/pi-plugin-prepack.sh", "packages/pi-shared/test/skills-guard.ts:17,34 runs scripts/pi-plugin-prepack.sh --stage-skills"),
    (f"{DAEMON}/internal/prompts/**/*.md", "packages/pi-shared/test/skills-partition.test.ts:70-76 reads every .md under packages/daemon/internal/prompts"),
    (".github/workflows/release.yaml", "packages/pi-shared/test/skills-partition.test.ts:95"),
    (f"{DAEMON}/internal/workspace/stale-git-lock-note.txt",
     "packages/pi-shared/src/git-lock.test.ts:63-64 reads it and holds git-lock.ts's copy to it"),
) + envoy_client_dep("packages/pi-shared/tsconfig.json paths @legion/envoy-client/errors; src imports "
                     "@legion/envoy-client/* (exports.bun -> src)")
  + install_jj("pi-shared") + bun_job("pi-shared"))
GATE = with_reads(GATE, [
    Read('packages/envoy-client/test/**', "filter pi_shared held packages/envoy-client/test/** by hand before the filters were computed: it selected 10 file(s) no other read of this gate declares"),
    Read('packages/daemon/internal/prompts/**', "filter pi_shared held packages/daemon/internal/prompts/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
