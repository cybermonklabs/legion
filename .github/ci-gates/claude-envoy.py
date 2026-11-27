# The claude_envoy filter's gate: what job claude-envoy reads.
GATE = gate("claude_envoy", "claude-envoy", reads=CLAUDE_ENVOY_JOB + reads(
    (TEST_FIXTURES_SRC, "packages/claude-envoy/tests import @legion/test-fixtures"),
) + biome(at("claude-envoy", "Lint Claude Envoy plugin")) + bun_job("claude-envoy"))
