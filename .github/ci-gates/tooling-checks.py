# The test job's other half, which a pull request runs (ci-job-test.yaml's tooling-checks
# job): the Claude Code plugin build, the changelog fold, the guides' layout, and the lint and
# typecheck of the script directories that are no workspace. No go command runs here. The
# typechecks' programs are check-ts-filters.sh's to list (ci_ts_programs.py); what is declared
# here is what a step reads outside any program, and each file outside a program's directory
# that the program compiles, which check-ts-filters.sh holds this filter to.
GATE = gate("tooling_checks", TOOLING_JOB, reads=CLAUDE_ENVOY_BUILD + reads(
    (CONTRACTS_SRC, f"`scripts/dispatch/ask-census.ts#ArtifactReviewEventPayload` ({at(TOOLING_JOB, 'Typecheck the Dispatch census script')}); "
                    f"`scripts/e2e/lib/envoy-bridge.ts#EnvelopeSchema` and `scripts/e2e/lib/envoy-bridge.ts#githubSubject` ({at(TOOLING_JOB, 'Typecheck the plan ratings read test')})"),
    (TEST_FIXTURES_SRC, f"{at(TOOLING_JOB, 'Typecheck the Dispatch census script')} and "
                        f"{at(TOOLING_JOB, 'Typecheck the plan ratings read test')} compile tests that import "
                        "@legion/test-fixtures"),
    ("scripts/dispatch/**", f"{at(TOOLING_JOB, 'Lint the Dispatch census script')} and typecheck"),
    ("scripts/github-apps/**", f"{at(TOOLING_JOB, 'Lint the GitHub App registration script')} and typecheck"),
    ("scripts/sessions/**", f"{at(TOOLING_JOB, 'Lint the session message-wait script')} and typecheck; the suite "
                            f"itself is {SESSIONS_JOB}'s"),
    ("scripts/approval-eval/**", f"{at(TOOLING_JOB, 'Lint the approval-eval statistics')} and typecheck"),
    ("scripts/e2e/lib/**", f"{at(TOOLING_JOB, 'Typecheck the plan ratings read test')}: scripts/e2e/lib/tsconfig.json"),
    ("scripts/e2e/*.ts", f"{at(TOOLING_JOB, 'Typecheck the e2e drivers')}: scripts/e2e/tsconfig.json includes every *.ts of the directory"),
    ("scripts/e2e/tsconfig.json", f"{at(TOOLING_JOB, 'Typecheck the e2e drivers')}: the program it typechecks"),
    # The files outside scripts/e2e that the drivers' program compiles, each with the import that
    # brings it in (check-ts-filters.sh derives the list; these hold the filter to it).
    ("packages/dispatch/e2e/api.ts", f"`scripts/e2e/plan-pointer-drive.ts#editArtifact` imports it, and {at(TOOLING_JOB, 'Typecheck the e2e drivers')} compiles that driver"),
    *[(path, f"{token} imports it, and {at(TOOLING_JOB, 'Typecheck the e2e drivers')} compiles it with "
             "scripts/e2e/plan-pointer-drive.ts")
      for path, token in (
          ("packages/dispatch/web/src/api/paths.ts", "`packages/dispatch/e2e/api.ts#routeUrl`"),
          ("packages/dispatch/web/src/api/types.ts", "`packages/dispatch/e2e/api.ts#GatedStep`"),
          ("packages/dispatch/e2e/harness-ports.ts", "`packages/dispatch/e2e/api.ts#harnessPorts`"),
          ("packages/dispatch/e2e/harness-processes.ts", "`packages/dispatch/e2e/api.ts#pinnedTo`"),
          ("packages/dispatch/e2e/one-pixel-png.ts", "`packages/dispatch/e2e/api.ts#ONE_PIXEL_PNG`"),
          ("packages/dispatch/e2e/harness-embedder.ts", "`packages/dispatch/e2e/harness-ports.ts#usesFakeEmbedder`"),
          ("packages/dispatch/e2e/harness-real-broker.ts", "`packages/dispatch/e2e/harness-ports.ts#usesRealBroker`"),
          ("packages/dispatch/e2e/harness-signin.ts", "`packages/dispatch/e2e/harness-ports.ts#harnessSignIn`"),
          ("packages/dispatch/e2e/harness-broker.ts", "`packages/dispatch/e2e/harness-real-broker.ts#usesFakeBroker`"),
      )],
    ("packages/pi-envoy/src/dispatch-session.ts", "`scripts/e2e/dispatch-broker-clients.ts#sessionDispatchClient` imports it, and "
                                                  f"{at(TOOLING_JOB, 'Typecheck the e2e drivers')} compiles that driver"),
    ("packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts", "`scripts/e2e/plan-pointer-drive.ts#workerPane` imports it, and "
                                                                  f"{at(TOOLING_JOB, 'Typecheck the e2e drivers')} compiles that driver"),
    ("packages/envoy-client/test/fake-dispatch.ts", f"`scripts/e2e/lib/agent-replay.test.ts#startFakeDispatch` imports it by path, and "
                                                    f"{at(TOOLING_JOB, 'Typecheck the plan ratings read test')} compiles that test"),
    ("packages/envoy-client/test/keyed-layer.ts", "`scripts/dispatch/__tests__/moved-approval-production-check.test.ts#keyedLayer` "
                                                  f"imports it by path, and {at(TOOLING_JOB, 'Typecheck the Dispatch census script')} "
                                                  "compiles that test"),
    ("packages/envoy-client/test/route-answers.ts", "`scripts/dispatch/__tests__/moved-approval-production-check.test.ts#routeAnswer` "
                                                    f"imports it by path, and {at(TOOLING_JOB, 'Typecheck the Dispatch census script')} "
                                                    "compiles that test"),
    ("packages/pi-shared/src/**", "packages/claude-envoy's build bundles it; scripts/sessions and scripts/dispatch typecheck against it"),
    (".github/actions/install-changie/**", at(TOOLING_JOB, "./.github/actions/install-changie")),
    (".github/ci-tools.yaml", "`.github/actions/install-changie/action.yml#ci_tools_field` and `.github/actions/install-changie/action.yml#ci_tools_fetch` installs the tarball of the changie row, "
                              "or of changie-aarch64 on an arm64 runner"),
    (".github/scripts/ci-tools.sh", "`.github/actions/install-changie/action.yml#$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh` sources it, and ci_tools_fetch "
                                    "reads that row's two sources"),
    (".github/changelog-packages", f"{at(TOOLING_JOB, FOLD_CHECK_STEP)}: the step reads it as the package list"),
    ("packages/*/.changie.yaml", f"{at(TOOLING_JOB, FOLD_CHECK_STEP)}: fold-check renders each listed package's fragments with that package's own changie configuration"),
    ("packages/*/.changes/unreleased/**", f"{at(TOOLING_JOB, FOLD_CHECK_STEP)}: fold-check renders every fragment a listed package holds"),
) + envoy_client_dep("`scripts/dispatch/ask-census.ts#DispatchClient` (tsc -p scripts/dispatch)")
  + dispatch_api_types(at(TOOLING_JOB, "Typecheck the Dispatch census script"))
  + biome(at(TOOLING_JOB, "Lint the Dispatch census script")) + bun_job(TOOLING_JOB))
