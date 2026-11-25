# The pi_envoy filter's gate: what job pi-envoy reads.
GATE = gate("pi_envoy", "pi-envoy", reads=reads(
    ("packages/pi-envoy/**", f"{at('pi-envoy', 'Typecheck pi-envoy')}, test and pack in packages/pi-envoy"),
    ("packages/pi-shared/src/**", "packages/pi-envoy/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts; the bundle inlines it"),
    ("packages/pi-shared/test/**", "`packages/pi-envoy/extensions/dispatch-first-omp.test.ts#@legion/pi-shared/test/omp-harness`, `packages/pi-envoy/src/skills-guard.test.ts#@legion/pi-shared/test/skills-guard`, `packages/pi-envoy/scripts/prepack.test.ts#@legion/pi-shared/test/skills-guard`"),
    (TEST_FIXTURES_SRC, "packages/pi-envoy's extensions and src tests import @legion/test-fixtures"),
    # envoy-client's sources, and cipher-client's with them, are the shared read below.
    ("packages/envoy-client/test/**", "`packages/pi-envoy/extensions/envoy.test.ts#@legion/envoy-client/test/` imports six of its modules, and `packages/pi-envoy/extensions/askgate/decision-record.test.ts#fakeRecordClient` and `packages/pi-envoy/extensions/askgate/decision-record.test.ts#recordNotFound` its fake-dispatch"),
    ("packages/envoy-client/bin/dispatch.ts", "`packages/pi-envoy/package.json#build-dispatch-cli.sh dist`, its build, and `scripts/pi-plugin-prepack.sh#build-dispatch-cli.sh` run build-dispatch-cli.sh, entry bin/dispatch.ts"),
    ("packages/envoy-client/scripts/build-dispatch-cli.sh", "`packages/pi-envoy/package.json#build-dispatch-cli.sh dist`; `scripts/pi-plugin-prepack.sh#build-dispatch-cli.sh`"),
    (CONTRACTS_SRC, f"{at('pi-envoy', 'Build contracts for pi-envoy')}; tsconfig paths @legion/contracts"),
    (f"{FIX}/dispatch-targeted-delivery.json", "`packages/pi-envoy/extensions/envoy.test.ts#targetedDispatchPayload`"),
    (f"{FIX}/envelope-without-role.json", "`packages/pi-envoy/extensions/envoy.test.ts#envelopeWithoutRole`"),
    (f"{FIX}/session-grant.json", "packages/pi-envoy/extensions/envoy.test.ts's reply-grant row runs sessionGrantConfig, which `packages/envoy-client/test/nats-server.ts#sessionGrantFixture` imports it for"),
    ("skills/**", "`packages/pi-envoy/src/skills-guard.test.ts#stageSkills` and `packages/pi-envoy/src/skills-guard.test.ts#repoSkillsRoot` stage them and hold their links to them; `scripts/pi-plugin-prepack.sh#stage_skills` stages skills; extensions/envoy.ts's resolveSkillsDirectory"),
    ("scripts/pi-plugin-prepack.sh", f"`packages/pi-envoy/package.json#prepack` ({at('pi-envoy', 'Pack pi-envoy')}); `packages/pi-envoy/scripts/prepack.test.ts#the prepack gives each bundle its own metafile and reads both for the notices`"),
    ("scripts/third-party-notices.ts", "`scripts/pi-plugin-prepack.sh#$root/scripts/third-party-notices.ts` writes dist/THIRD_PARTY_NOTICES with it, for each plugin"),
    ("scripts/e2e/lib/pack-plugin.sh", "`packages/pi-envoy/scripts/prepack.test.ts#the packed tarball` packs with it"),
) + envoy_client_dep("packages/pi-envoy/tsconfig.json paths @legion/envoy-client/*; `packages/pi-envoy/extensions/envoy.ts#@legion/envoy-client/` imports")
  + dispatch_api_types(at("pi-envoy", "Typecheck pi-envoy"))
  + install_omp("pi-envoy") + bun_job("pi-envoy"))
GATE = with_reads(GATE, [
    Read('packages/daemon/internal/prompts/**', "filter pi_envoy held packages/daemon/internal/prompts/** by hand before the filters were computed: it selected 1 file(s) no other read of this gate declares", but=('**/*_test.go',)),
])
