import { describe, expect, test } from "bun:test";
import { dispatchToolSchema, dispatchToolSpecs, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";
import { bearerCredential } from "../bearer";
import { executeDispatchTool } from "../dispatch-execute";
import { followedAsk, followNoticeText } from "../dispatch-subscribe";

// Agents reach Dispatch through the `dispatch` command, so no refusal or notice the command prints
// may name one of the native tools it replaced.
const toolNames = new RegExp(`\\b(${dispatchToolSpecs.map((spec) => spec.name).join("|")})\\b`);

const config = {
  enabled: true,
  url: "http://dispatch.test",
  credential: bearerCredential("secret"),
  error: null,
};
const noNetwork = (async () => {
  throw new Error("the refusal must come before any request");
}) as unknown as typeof fetch;

function requiredFields(spec: (typeof dispatchToolSpecs)[number]): string[] {
  const schema = z.toJSONSchema(dispatchToolSchema(spec, zodSchemaApi(z))) as {
    required?: string[];
  };
  return schema.required ?? [];
}

describe("no refusal or notice the dispatch command prints names a native Dispatch tool", () => {
  // The 30 s limit is a hang guard, not a budget: the row refuses every required field of every
  // tool spec in turn, and bun's unwritten 5,000 ms per-test default is a limit on that work
  // rather than on a hang. Measured in a 2-CPU pod: 0.14 s idle; under 16 busy loops (8 x nproc),
  // 3.71 s at most in a whole-suite run and 2.88 s with this file alone. It fires only if a
  // refusal never settles.
  test("every refusal of a spec's example with one required field removed", async () => {
    let refusals = 0;
    for (const spec of dispatchToolSpecs) {
      for (const field of requiredFields(spec)) {
        const { [field]: _removed, ...args } = spec.example as Record<string, unknown>;
        const refusal = await executeDispatchTool({
          tool: spec.name,
          args,
          cwd: "/workspace",
          host: "omp",
          sessionId: "session-1",
          config,
          env: {},
          fetchImpl: noNetwork,
        }).then(
          () => undefined,
          (error: unknown) => (error instanceof Error ? error.message : String(error))
        );
        expect(refusal, `${spec.name} without ${field}`).toBeDefined();
        expect(refusal, `${spec.name} without ${field}`).not.toMatch(toolNames);
        refusals++;
      }
    }
    expect(refusals).toBeGreaterThan(20);
  }, 30_000);

  test("the follow notice, for an issue and for a project document", () => {
    for (const details of [
      { issue: "DSP-1", follows: { ask: "ask-1" } },
      { document: "CORE/design-notes", follows: { ask: "ask-1" } },
    ]) {
      const followed = followedAsk(details);
      expect(followed).not.toBeNull();
      if (followed === null) continue;
      // With no subscription the notice names its envoy_subscribe line too.
      expect(followNoticeText(followed, [])).not.toMatch(toolNames);
    }
  });
});
