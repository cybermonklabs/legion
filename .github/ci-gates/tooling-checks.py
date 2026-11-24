# The test job's other half, which a pull request runs (ci-job-test.yaml's tooling-checks
# job): the Claude Code plugin build, the changelog fold, the guides' layout, and the lint and
# typecheck of the script directories that are no workspace. No go command runs here. The
# typechecks' programs are check-ts-filters.sh's to list (ci_ts_programs.py); what is declared
# here is what a step reads outside any program.
GATE = gate("tooling_checks", TOOLING_JOB, reads=CLAUDE_ENVOY_BUILD + reads(
    (CONTRACTS_SRC, f"scripts/dispatch/ask-census.ts:3-9 ({at(TOOLING_JOB, 'Typecheck the Dispatch census script')}); "
                    f"scripts/e2e/lib/envoy-bridge.ts:9-10 ({at(TOOLING_JOB, 'Typecheck the plan ratings read test')})"),
    (TEST_FIXTURES_SRC, f"{at(TOOLING_JOB, 'Typecheck the Dispatch census script')} and "
                        f"{at(TOOLING_JOB, 'Typecheck the plan ratings read test')} compile tests that import "
                        "@legion/test-fixtures"),
    ("scripts/dispatch/**", f"{at(TOOLING_JOB, 'Lint the Dispatch census script')} and typecheck"),
    ("scripts/github-apps/**", f"{at(TOOLING_JOB, 'Lint the GitHub App registration script')} and typecheck"),
    ("scripts/sessions/**", f"{at(TOOLING_JOB, 'Lint the session message-wait script')} and typecheck; the suite "
                            f"itself is {SESSIONS_JOB}'s"),
    ("scripts/approval-eval/**", f"{at(TOOLING_JOB, 'Lint the approval-eval statistics')} and typecheck"),
    ("scripts/e2e/lib/**", f"{at(TOOLING_JOB, 'Typecheck the plan ratings read test')}: scripts/e2e/lib/tsconfig.json"),
    ("packages/envoy-client/test/fake-dispatch.ts", f"scripts/e2e/lib/agent-replay.test.ts:4-7 imports it by path, and "
                                                    f"{at(TOOLING_JOB, 'Typecheck the plan ratings read test')} compiles that test"),
    ("packages/pi-shared/src/**", "packages/claude-envoy's build bundles it; scripts/sessions and scripts/dispatch typecheck against it"),
    (".github/actions/install-changie/**", at(TOOLING_JOB, "./.github/actions/install-changie")),
    (".github/ci-tools.yaml", ".github/actions/install-changie/action.yml:28-29 installs the tarball of the changie row, "
                              "or of changie-aarch64 on an arm64 runner"),
    (".github/scripts/ci-tools.sh", ".github/actions/install-changie/action.yml:18 sources it, and ci_tools_fetch "
                                    "reads that row's two sources"),
    (".github/changelog-packages", f"{at(TOOLING_JOB, FOLD_CHECK_STEP)}: the step reads it as the package list"),
    ("packages/*/.changie.yaml", f"{at(TOOLING_JOB, FOLD_CHECK_STEP)}: fold-check renders each listed package's fragments with that package's own changie configuration"),
    ("packages/*/.changes/unreleased/**", f"{at(TOOLING_JOB, FOLD_CHECK_STEP)}: fold-check renders every fragment a listed package holds"),
) + envoy_client_dep("scripts/dispatch/ask-census.ts:10-18 (tsc -p scripts/dispatch)")
  + biome(at(TOOLING_JOB, "Lint the Dispatch census script")) + bun_job(TOOLING_JOB))
