# The dashboard's and the proof editor's fast checks
# (.github/ci-notes/ci.md#the-dashboards-and-the-proof-editors-lint-typecheck-unit-tests-build-and
# says what they are). No go command runs here.
GATE = gate("dispatch_ui", "dispatch-ui", reads=reads(
    ("packages/dispatch/**", f"{at('dispatch-ui', 'Typecheck dispatch dashboard')}, test, build, check:classes and check:asset-sizes in packages/dispatch"),
    ("packages/proof-editor/**", f"{at('dispatch-ui', 'Typecheck proof editor')} and test; `packages/dispatch/web/vite.config.ts#alias`, whose @legion/proof-editor entries resolve into it"),
    # packages/contracts/src is no read of this gate's: which of its modules this job compiles is
    # what its two tsc programs list, so the changes job lists them into the filter's listed block
    # and check-ts-filters.sh holds the block to them (ci_ts_programs.py, LISTED).
    # why: .github/ci-notes/ci.md#dispatch_ui-the-script-whose-references-arm-lists-the-references-lanes-spec-files
    (".github/scripts/dispatch-browser-shard-suite.sh", f"`packages/dispatch/e2e/reference-lane.ts#referencesLaneFiles` reads the spec files out of its `references)` arm, at the URL of `packages/dispatch/e2e/reference-lane.ts#LANE_SCRIPT`, for the reading walk and surface list tests that {at('dispatch-ui', 'Test dispatch dashboard')} runs"),
    # why: .github/ci-notes/ci.md#dispatch_references-the-reference-rows-preview-dispatch-references-the-eleven-spec-files
    (".github/ci-gates/dispatch-references.py", f"`packages/dispatch/web/src/features/refs/surface-lists.test.ts#referenceFilter` reads the dispatch_references filter's hand-kept patterns out of it, at the path `packages/dispatch/web/src/features/refs/surface-lists.test.ts#REFERENCE_GATE` names, patterns every covered box's file must match, and {at('dispatch-ui', 'Test dispatch dashboard')} runs it"),
    (f"{FIX}/live/**", "`packages/dispatch/web/src/features/delivery/lib/applyDelta.test.ts#fixtures/live/` and `packages/dispatch/e2e/__tests__/delivery-live-probe.test.ts#fixtures/live/` import the frames under it"),
    (f"{FIX}/decision-blocks/**", "`packages/dispatch/web/src/features/doc/decision-blocks.test.ts#fixture` and `packages/dispatch/web/src/features/inbox/AskCard.test.tsx#decisionBlockFixture` read the fixtures under it"),
    (f"{ENVOY}/internal/dispatch/pmdoc/schema/**", "`packages/dispatch/web/src/features/doc/schema.ts#manifest`, `packages/dispatch/web/src/features/refs/markdown-engine.test.ts#manifest` and `packages/dispatch/web/src/features/doc/ProofDocument.admission.test.tsx#manifest` import blocks.json, and `packages/proof-editor/tests/manifest-schema.test.ts#manifest`, `packages/proof-editor/tests/block-schema-fixture.ts#serverBlockSchema` and `packages/proof-editor/tests/nested-directive.test.ts#serverBlockSchema` read it; `packages/proof-editor/tests/ask-concurrent-edit.test.ts#serverBlockNodeTypes` reads block-node-types.json of the same directory"),
    (f"{ENVOY}/internal/dispatch/pmdoc/testdata/fixtures.json", "read by `packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts#pmdocFixtures` and `packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts#TABLE_PATH`, the second the new URL of its path, for the three suites this job runs over the one table: features/refs/fixture-table-readings.test.ts, markdown-engine.test.ts and markdown-surfaces.test.tsx"),
    (f"{ENVOY}/internal/dispatch/pmdoc/heavybodies/testdata/shared-bodies.json", "`packages/dispatch/e2e/__tests__/heavy-bodies-digests.test.ts#TABLE_PATH` reads it, and this job runs that suite: the digest table holding e2e/heavy-bodies.ts's bodies to the Go shapes that spell the same bytes"),
    (f"{ENVOY}/internal/dispatch/text/testdata/head-refs.json", f"`packages/dispatch/web/src/features/refs/head-refs.test.ts#HEAD_ROWS` reads it at a new URL: the head table the Go text package's HeadRefs is held to as well, and {at('dispatch-ui', 'Test dispatch dashboard')} runs it"),
    (f"{ENVOY}/internal/dispatch/api/testdata/visual-shot-reads.json", "read at module load by packages/dispatch/e2e/visual-guard.ts, the new URL of SHOT_READS_FILE, which e2e/__tests__/visual-guard.test.ts, a test this job runs, imports: the routes a visual row's page may read"),
    (f"{ENVOY}/scripts/fixtures/shell-quote.json", "`packages/dispatch/e2e/__tests__/local-instance-support.test.ts#quoteCases` reads it at a new URL"),
    ("packages/contracts/schemas/dispatch-openapi.json",
     "`packages/dispatch/e2e/__tests__/stub-recordings.test.ts#documentPath` reads it at a new URL: the generated API "
     "document every committed recording's body is held to, which this job's test step runs"),
    (f"{ENVOY}/internal/dispatch/api/testdata/status-change/*.json", "`packages/dispatch/web/src/features/issue/parent-status/status-change-fixtures.ts#recorded` reads each of them under `packages/dispatch/web/src/features/issue/parent-status/status-change-fixtures.ts#FIXTURES`: the answers of POST /api/v1/issues/{key}/status-change that packages/envoy/internal/dispatch/api's TestStatusChangeAnswersMatchTheirFixtures writes, which the parent-status question's unit tests and the board's answer with"),
    (f"{ENVOY}/going/testdata/**", "`packages/proof-editor/tests/going-answer-no-link.test.ts#fixturePath`"),
    ("scripts/third-party-notices.ts", "`packages/dispatch/web/vite.config.ts#thirdPartyNotices` (build:web writes dist/THIRD_PARTY_NOTICES.txt)"),
    ("scripts/e2e/lib/process-tree.sh", "`packages/dispatch/e2e/acceptance/replace-blocks.sh#$repo_root/scripts/e2e/lib/process-tree.sh` sources it, and "
                                        f"e2e/__tests__/replace-blocks-guard.test.ts, which {at('dispatch-ui', 'Test dispatch dashboard')} runs, runs that script"),
    (".github/workflows/dispatch-unit-repeat.yaml",
     "`packages/dispatch/e2e/__tests__/unit-repeat-budgets.test.ts#each budget of the repeat workflow is exactly its rule over the measurement beside it` reads it at a new URL and holds the workflow's "
     "budgets to the measurements beside them, and `packages/dispatch/e2e/__tests__/unit-repeat.test.ts#a step the run cancels names its running row, as GitHub's runner signals it` runs the test step's own "
     f"text as GitHub's runner cancels it, and {at('dispatch-ui', 'Test dispatch dashboard')} runs both"),
    (f"{ENVOY}/scripts/lib/pg-url.sh", "`packages/dispatch/e2e/__tests__/psql-password.test.ts#../../envoy/scripts/lib/pg-url.sh` sources it, and "
                                       "`packages/dispatch/e2e/acceptance/replace-blocks.sh#pg-url.sh` does, which e2e/__tests__/replace-blocks-guard.test.ts runs; "
                                       f"{at('dispatch-ui', 'Test dispatch dashboard')} runs both"),
    (f"{ENVOY}/scripts/fixtures/pg-url.json", "`packages/dispatch/e2e/__tests__/psql-password.test.ts#corpus` reads it at a new URL"),
    ("scripts/e2e/lib/held-port.py", "`packages/dispatch/e2e/__tests__/psql-password.test.ts#serverSignIns` runs packages/dispatch/e2e/run-server.sh, "
                                     f"which holds its server's port through it (`packages/dispatch/e2e/run-server.sh#held_port`); {at('dispatch-ui', 'Test dispatch dashboard')} runs that file"),
    ("packages/envoy-client/test/fake-dispatch.ts", f"`packages/dispatch/e2e/__tests__/delivery-live-probe.test.ts#startFakeDispatch` and `packages/dispatch/e2e/__tests__/local-instance-support.test.ts#startFakeDispatch` import @legion/envoy-client/test/fake-dispatch, and {at('dispatch-ui', 'Test dispatch dashboard')} runs both"),
    (TEST_FIXTURES_SRC, f"packages/dispatch's web and e2e/__tests__ suites import @legion/test-fixtures, and {at('dispatch-ui', 'Test dispatch dashboard')} runs them; packages/proof-editor/tests/build-upstream-tarball.test.ts imports its temporary-directories fixture, and {at('dispatch-ui', 'Test proof editor')} runs it"),
    ("packages/contracts/fixtures/shown-values.json", f"`packages/dispatch/web/src/features/refs/Timestamp.test.tsx#shownValues` and `packages/dispatch/web/src/features/project/board-model.test.ts#shownValues` import it, so {at('dispatch-ui', 'Typecheck dispatch dashboard')} compiles them and {at('dispatch-ui', 'Test dispatch dashboard')} runs those tests"),
) + envoy_client_dep("`packages/envoy-client/test/fake-dispatch.ts#DispatchServiceError` imports ../src/dispatch-http, which the two suites "
                     "above import, and `packages/dispatch/e2e/read-token-live.ts#brokerCredential` imports "
                     "@legion/envoy-client/broker-credential, so this job's program holds envoy-client's sources "
                     "(check-ts-filters.sh measures it)")
  + dispatch_api_types(at("dispatch-ui", "Typecheck dispatch dashboard")) + bun_job("dispatch-ui"))
GATE = with_reads(GATE, [
    Read('packages/envoy-client/test/**', "filter dispatch_ui held packages/envoy-client/test/** by hand before the filters were computed: it selected 10 file(s) no other read of this gate declares"),
])
