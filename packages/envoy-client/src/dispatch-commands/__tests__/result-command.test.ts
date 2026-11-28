// `dispatch result` (`resultCommand`) prints what it recorded. `dispatch result --retract` takes
// the id of a recorded result, so the printed text names that id beside the check, the commit and
// the role, in the form `dispatch results` prints each result in: "Result <id>".
import { expect, test } from "bun:test";
import type { Actor } from "@legion/contracts";
import { keyedLayer } from "../../../test/keyed-layer";
import { routeAnswer } from "../../../test/route-answers";
import { bearerCredential } from "../../bearer";
import { DispatchClient } from "../../dispatch-http";
import type { CommandContext } from "../context";
import { resultCommand } from "../issues";

const actor = { kind: "session", id: "session-result" } as Actor;
const commit = "1".repeat(40);

test("dispatch result prints the id of the result it recorded, as Result <id>", async () => {
  const id = "00000000-0000-4000-8000-0000000000a1";
  const answer = routeAnswer("POST /api/v1/issues/{key}/results", {
    line: `- Check k1 (before_merge): pass at ${commit.slice(0, 12)}, recorded by tester. ok`,
    result: {
      id,
      issue_key: "DSP-1",
      check_issue_key: "DSP-1",
      check_block_id: "k1",
      role: "tester",
      verdict: "pass",
      commit_sha: commit,
    },
  });
  // Behind Dispatch's keyed layer: the 201 names the write's key back.
  const fetchImpl = keyedLayer(async () => Response.json(answer, { status: 201 })) as typeof fetch;
  const client = new DispatchClient("http://dispatch.test", bearerCredential("secret"), fetchImpl, {
    sleep: async () => undefined,
  });
  // The command reads `input.dryRun` (main's `--dry-run`); this call records, so it names none.
  const context = {
    input: {},
    args: { issue: "DSP-1", check: "k1", role: "tester", verdict: "pass", commit },
    client,
    actor,
  } as unknown as CommandContext;
  const { text } = await resultCommand(context);
  expect(text.split("\n")[0]).toBe(
    `Recorded pass for check k1 of DSP-1 at ${commit}, as tester. Result ${id}.`
  );
});
