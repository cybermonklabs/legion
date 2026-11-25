# The envoy_client filter's gate: what job envoy-client reads.
GATE = gate("envoy_client", "envoy-client", reads=reads(
    ("packages/envoy-client/**", f"{at('envoy-client', 'Typecheck Envoy client')}, test and build in packages/envoy-client"),
    ("packages/cipher-client/**", f"{at('envoy-client', 'Typecheck Envoy client')} and its tests compile it; packages/envoy-client/src/broker-credential.ts imports brokerTokenClient from @legion/cipher-client/environment, chooseSignIn from @legion/cipher-client/signins and isFinalRefusal from @legion/cipher-client/token-client"),
    (CONTRACTS_SRC, f"{at('envoy-client', 'Build contracts for Envoy client')}; tsconfig paths @legion/contracts -> ../contracts/src/index.ts"),
    (f"{FIX}/dispatch-targeted-delivery.json", "packages/envoy-client/src/__tests__/delivery.test.ts:17, dispatch-text.test.ts:128"),
    (f"{FIX}/envoy-session-no-interests.json", "packages/envoy-client/src/__tests__/transport.test.ts:12"),
    (f"{FIX}/envelope-without-role.json", "packages/envoy-client/src/__tests__/transport.test.ts:11"),
    (f"{FIX}/ask-reply-recipients.json", "packages/envoy-client/src/__tests__/ask-reply-recipients.test.ts:61 renders the copies packages/envoy's TestAnAskReplyIsTheSessionsOwnOnlyForItsAskerAndFollowers writes"),
    (f"{FIX}/dispatch-public-url.json", "packages/envoy-client/src/__tests__/dispatch-config.test.ts:5"),
    (f"{FIX}/session-grant.json", "packages/envoy-client/test/nats-server.ts:4 imports it (sessionGrantConfig), which src/__tests__/session-reply-grant.test.ts runs"),
    (f"{ENVOY}/internal/dispatch/api/testdata/**", "packages/envoy-client/src/__tests__/askgate-new-asks.test.ts:23-30 reads the Dispatch server's table of decision-block inserts, which packages/envoy/internal/dispatch/api's TestDocumentEditAdviceCountsTheAskBlocksTheEditAdded reads too"),
    (f"{ENVOY}/internal/dispatch/api/testdata/status-change/*.json", "packages/envoy-client/src/__tests__/dispatch-status-rule.test.ts:49 readFileSync (FIXTURES :35): the answers of POST /api/v1/issues/{key}/status-change that packages/envoy/internal/dispatch/api's TestStatusChangeAnswersMatchTheirFixtures writes, which the test's fake Dispatch answers with"),
    ("skills/**", "packages/envoy-client/src/__tests__/dispatch-advice.test.ts:2244 reads skills/<skill>/SKILL.md for every skill a notice cites"),
    (TEST_FIXTURES_SRC, "packages/envoy-client/src/__tests__ import @legion/test-fixtures"),
    ("docs/site/src/content/docs/dispatch/for-agents.md", "packages/envoy-client/src/__tests__/plans-command.test.ts:314 readFileSync (REPO_ROOT :210): the guide for agents' Plans row, held to the contract's rating scale"),
    (f"{ENVOY}/internal/bus/testdata/private-stream-subjects.json", "packages/envoy-client/src/__tests__/stream-topics.test.ts:131 reads the listener's own cases by relative path (SHARED_CASES :118-121), which packages/envoy/internal/bus's TestReachesPrivateStreamSubjectBoundsInterests reads too"),
) + bun_job("envoy-client"))
