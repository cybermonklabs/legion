# The pi-legion plugin's typecheck and its pack as the release packs it, the half of the old
# pi-legion job a pull request runs (ci-job-pi-legion.yaml's pi-legion-build job). No go
# command runs here. The typecheck's program is check-ts-filters.sh's to list
# (ci_ts_programs.py); what is declared here is what the program reaches beside the package
# and what the pack reads.
GATE = gate("pi_legion_build", "pi-legion-build", reads=reads(
    ("packages/pi-legion/**", f"{at('pi-legion-build', 'Typecheck pi-legion')} and {at('pi-legion-build', 'Pack pi-legion')} in packages/pi-legion"),
    ("packages/pi-envoy/**", "the typecheck compiles `packages/pi-legion/extensions/legion.test.ts#envoyEntryDefault` and `packages/pi-legion/extensions/legion.test.ts#envoyEntry`, which tsconfig.json includes: both import ../../pi-envoy/extensions/envoy"),
    ("packages/pi-shared/src/**", "packages/pi-legion/tsconfig.json paths @legion/pi-shared/* and include ../pi-shared/src/omp-host.d.ts; the pack's bundle inlines it"),
    ("packages/pi-shared/test/**", "packages/pi-legion/tsconfig.json paths @legion/pi-shared/test/*, and the typecheck compiles `packages/pi-legion/extensions/legion-phase-stall-omp.test.ts#runLegionPane` and `packages/pi-legion/extensions/legion-role-tools-omp.test.ts#toolResultsIn`, which import it"),
    ("packages/envoy-client/test/subscribe-answer.ts", "`packages/pi-shared/test/omp-harness.ts#consumerDelivery` imports it, and the typecheck compiles that harness for the omp tests it includes"),
    # envoy-client's sources, and cipher-client's with them, are the shared read below.
    (CONTRACTS_SRC, f"{at('pi-legion-build', 'Build contracts for pi-legion')}; tsconfig paths @legion/contracts"),
    (TEST_FIXTURES_SRC, "the typecheck compiles packages/pi-legion's extensions and src tests, which import @legion/test-fixtures"),
    ("packages/dispatch/e2e/**", "`packages/pi-legion/scripts/skill-scenarios/seed.ts#listIssueMessages` imports packages/dispatch/e2e/api.ts (typecheck includes scripts/skill-scenarios/*.ts), and that program holds six of that directory's modules: api.ts, harness-ports.ts, harness-broker.ts, harness-processes.ts, harness-real-broker.ts and harness-signin.ts"),
    ("packages/dispatch/web/src/api/types.ts", "`packages/dispatch/e2e/api.ts#Actor` and `packages/dispatch/e2e/api.ts#GatedStep` imports it"),
    ("packages/dispatch/web/src/api/paths.ts", "`packages/dispatch/e2e/api.ts#PathsOf` imports PathsOf, Op, PathParams, QueryParams, RequestBody, Query, Body and Result from it, since that client is keyed on the generated document's path patterns"),
    ("skills/**", f"`scripts/pi-plugin-prepack.sh#stage_skills` stages the plugin's skills, run by {at('pi-legion-build', 'Pack pi-legion')}"),
    ("scripts/pi-plugin-prepack.sh", f"packages/pi-legion/package.json prepack ({at('pi-legion-build', 'Pack pi-legion')})"),
    ("scripts/daemon-contract.sh", f"{at('pi-legion-build', 'Pack pi-legion')} writes the number it prints into the packed manifest"),
    (f"{FIX}/daemon-api/**", "scripts/daemon-contract.sh reads released-contracts.json and lists contracts/*.json for the number the pack writes"),
    ("scripts/third-party-notices.ts", "`scripts/pi-plugin-prepack.sh#dist/THIRD_PARTY_NOTICES` writes that file with it"),
    ("scripts/e2e/lib/**", "`packages/pi-legion/scripts/skill-scenarios/rig.test.ts#scriptFunctions` imports scripts/e2e/lib/script-functions, and the typecheck includes scripts/skill-scenarios/*.ts"),
    # What the skills' guard reads beside the skills and the package, run by
    # "Check the skills against their guard": the daemon's prompts, whose skill:// links it
    # resolves, and the Legion guides it reads.
    (f"{DAEMON}/internal/prompts/**/*.md", f"{at('pi-legion-build', 'Check the skills against their guard')}: `packages/pi-legion/src/skills-guard.test.ts#promptsRoot` is a root of the skill:// links `packages/pi-legion/src/skills-guard.test.ts#skillLinks` reads"),
    ("docs/site/src/content/docs/**", f"{at('pi-legion-build', 'Check the skills against their guard')}: `packages/pi-legion/src/skills-guard.test.ts#the guides say only a review that requests changes counts a round` reads the Legion guides under it"),
) + envoy_client_dep("packages/pi-legion/tsconfig.json paths @legion/envoy-client/*; scripts/skill-scenarios/*.ts "
                     "import @legion/envoy-client/*")
  + bun_job("pi-legion-build"))
