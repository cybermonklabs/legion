# The pi_envoy filter's gate: what job pi-envoy reads.
GATE = gate("pi_envoy", "pi-envoy", reads=reads(
    ("packages/pi-envoy/**", f"{at('pi-envoy', 'Typecheck pi-envoy')}, test and pack in packages/pi-envoy"),
    ("packages/pi-shared/src/**", "packages/pi-envoy/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts; the bundle inlines it"),
    ("packages/pi-shared/test/**", "packages/pi-envoy/extensions/dispatch-first-omp.test.ts:4-14, src/skills-guard.test.ts:9-23, scripts/prepack.test.ts:6"),
    (TEST_FIXTURES_SRC, "packages/pi-envoy's extensions and src tests import @legion/test-fixtures"),
    # envoy-client's sources, and cipher-client's with them, are the shared read below.
    ("packages/envoy-client/test/**", "packages/pi-envoy/extensions/envoy.test.ts:69-86 imports six of its modules, and extensions/askgate/decision-record.test.ts:9 its fake-dispatch"),
    ("packages/envoy-client/bin/dispatch.ts", "packages/pi-envoy/package.json:26 build and scripts/pi-plugin-prepack.sh:96 run build-dispatch-cli.sh, entry bin/dispatch.ts"),
    ("packages/envoy-client/scripts/build-dispatch-cli.sh", "packages/pi-envoy/package.json:26; scripts/pi-plugin-prepack.sh:96"),
    (CONTRACTS_SRC, f"{at('pi-envoy', 'Build contracts for pi-envoy')}; tsconfig paths @legion/contracts"),
    (f"{FIX}/dispatch-targeted-delivery.json", "packages/pi-envoy/extensions/envoy.test.ts:399-402"),
    (f"{FIX}/envelope-without-role.json", "packages/pi-envoy/extensions/envoy.test.ts:115-117"),
    (f"{FIX}/session-grant.json", "packages/pi-envoy/extensions/envoy.test.ts's reply-grant row runs sessionGrantConfig, which packages/envoy-client/test/nats-server.ts:4 imports it for"),
    ("skills/**", "packages/pi-envoy/src/skills-guard.test.ts:37-39 stages them; scripts/pi-plugin-prepack.sh:103 stages skills; extensions/envoy.ts's resolveSkillsDirectory"),
    ("scripts/pi-plugin-prepack.sh", f"packages/pi-envoy/package.json:27 prepack ({at('pi-envoy', 'Pack pi-envoy')}); packages/pi-envoy/scripts/prepack.test.ts:85"),
    ("scripts/third-party-notices.ts", "scripts/pi-plugin-prepack.sh:97,100 write dist/THIRD_PARTY_NOTICES with it"),
    ("scripts/e2e/lib/pack-plugin.sh", "packages/pi-envoy/scripts/prepack.test.ts:144-149"),
) + envoy_client_dep("packages/pi-envoy/tsconfig.json paths @legion/envoy-client/*; extensions/envoy.ts:19-131 imports")
  + install_omp("pi-envoy") + bun_job("pi-envoy"))
GATE = with_reads(GATE, [
    Read('packages/daemon/internal/prompts/**', "filter pi_envoy held packages/daemon/internal/prompts/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
