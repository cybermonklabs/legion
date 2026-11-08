import { expect, test } from "bun:test";

import { createApiClient, type DispatchApiClient } from "./client";

/** A write method whose path parameter the caller left empty: the route pattern `routeUrl`
 *  refuses to build, and the call itself, held as a thunk so a method that is not `async` throws
 *  inside this test rather than as the list is built. */
interface EmptyPathParamWrite {
  readonly call: () => Promise<unknown>;
  readonly method: string;
}

/** Every write method that sends a request its builder makes (`this.send(someRequest(…))`), called
 *  with an empty path parameter. Each builder reaches `routeUrl` as it is called, so these are the
 *  methods whose refusal is a rejection only while they are `async`. */
function emptyPathParamWrites(api: DispatchApiClient): readonly EmptyPathParamWrite[] {
  const bounds = { key: "press-1" };
  return [
    { call: () => api.patchIssue("", { status: "done" }, bounds), method: "patchIssue" },
    { call: () => api.snoozeAsk("", "2026-11-09T00:00:00.000Z", bounds), method: "snoozeAsk" },
    {
      call: () => api.createAgentMessage("", { body: "Ready?", delivery: "btw" }, bounds),
      method: "createAgentMessage",
    },
    { call: () => api.putIssueState("", { pinned: true }, bounds), method: "putIssueState" },
    {
      call: () => api.putAgentState("", { cleared_before: "2026-11-08T00:00:00.000Z" }, bounds),
      method: "putAgentState",
    },
  ];
}

/** How one call settled: `rejected` with its message, which is the contract every caller holds
 *  these methods to; `threw`, which is what a method that is not `async` does - it refuses before
 *  any promise exists, so `.catch` on its result never runs and the press is left hanging; or
 *  `resolved`. */
async function settledAs(call: () => Promise<unknown>): Promise<string> {
  let pending: Promise<unknown>;
  try {
    pending = call();
  } catch (error) {
    return `threw: ${error instanceof Error ? error.message : String(error)}`;
  }
  try {
    return `resolved: ${JSON.stringify(await pending)}`;
  } catch (error) {
    return `rejected: ${error instanceof Error ? error.message : String(error)}`;
  }
}

// The write store catches a refused write on the promise the press returns; a write method that
// refuses synchronously is caught nowhere, so this pins each one's refusal as a rejection and
// names the method that broke it. The URL fails to build before any request, so the fetch here
// answers nothing but records that it was never asked.
test("every write built from a request rejects an empty path parameter rather than throwing", async () => {
  const requests: string[] = [];
  const api = createApiClient(async (input) => {
    requests.push(input.toString());
    return Response.json({});
  });

  const settled: Record<string, string> = {};
  for (const { call, method } of emptyPathParamWrites(api)) {
    settled[method] = await settledAs(call);
  }

  expect(settled).toEqual({
    createAgentMessage:
      "rejected: /api/v1/agents/{session_id}/messages has no value for {session_id}",
    patchIssue: "rejected: /api/v1/issues/{key} has no value for {key}",
    putAgentState: "rejected: /api/v1/me/agents/{session_id}/state has no value for {session_id}",
    putIssueState: "rejected: /api/v1/me/issues/{key}/state has no value for {key}",
    snoozeAsk: "rejected: /api/v1/me/asks/{id}/snooze has no value for {id}",
  });
  expect(requests).toEqual([]);
});
