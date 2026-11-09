import { expect, test } from "bun:test";

import { createApiClient, type DispatchApiClient } from "./client";

// An argument the client itself refuses - a path parameter the route pattern has no value for
// (`routeUrl`), a repository that is not `owner/repo` (`repoParams`) - reaches the caller as a
// rejected promise, never as a synchronous throw, and no request is sent. A caller holds a
// refusal on the promise the call answers with: the write store catches a refused write on the
// promise the press returns, so a refusal thrown before any promise exists is caught nowhere and
// the press hangs. Every method below reaches one of the two refusals in its own frame, by
// building its URL there or by reading one of its arguments there, so each is held to that
// contract by name.

/** One call of one method with an argument the client refuses, and the message the refusal
 *  carries. The call is held as a thunk so a method that refuses synchronously throws inside the
 *  test rather than as the table is built. */
interface Refusal {
  readonly call: (api: DispatchApiClient) => Promise<unknown>;
  readonly method: string;
  readonly message: string;
}

const bounds = { key: "press-1" };
const timestamp = "2026-11-09T00:00:00.000Z";
const notOwnerRepo = "repository must be formatted as owner/repo";

const refusals: readonly Refusal[] = [
  {
    call: (api) => api.patchIssue("", { status: "done" }, bounds),
    message: "/api/v1/issues/{key} has no value for {key}",
    method: "patchIssue",
  },
  {
    call: (api) => api.getIssueEvents(""),
    message: "/api/v1/issues/{key}/events has no value for {key}",
    method: "getIssueEvents",
  },
  {
    call: (api) => api.listIssueAskThreads(""),
    message: "/api/v1/issues/{key}/asks has no value for {key}",
    method: "listIssueAskThreads",
  },
  {
    call: (api) => api.snoozeAsk("", timestamp, bounds),
    message: "/api/v1/me/asks/{id}/snooze has no value for {id}",
    method: "snoozeAsk",
  },
  {
    call: (api) => api.markThreadRead("", timestamp, bounds),
    message: "/api/v1/me/threads/{id}/read has no value for {id}",
    method: "markThreadRead",
  },
  {
    call: (api) => api.createAgentMessage("", { body: "Ready?", delivery: "btw" }, bounds),
    message: "/api/v1/agents/{session_id}/messages has no value for {session_id}",
    method: "createAgentMessage",
  },
  {
    call: (api) => api.putDocumentRead("", 3, bounds),
    message: "/api/v1/me/documents/{id}/read has no value for {id}",
    method: "putDocumentRead",
  },
  {
    call: (api) => api.getArtifactVersion("", 3),
    message: "/api/v1/artifacts/{id}/versions/{number} has no value for {id}",
    method: "getArtifactVersion",
  },
  {
    call: (api) => api.putIssueState("", { pinned: true }, bounds),
    message: "/api/v1/me/issues/{key}/state has no value for {key}",
    method: "putIssueState",
  },
  {
    call: (api) => api.putAgentState("", { cleared_before: timestamp }, bounds),
    message: "/api/v1/me/agents/{session_id}/state has no value for {session_id}",
    method: "putAgentState",
  },
  {
    call: (api) => api.putRepoProject("", { project: "ACME" }, bounds),
    message: notOwnerRepo,
    method: "putRepoProject",
  },
  {
    call: (api) => api.deleteRepoProject("", bounds),
    message: notOwnerRepo,
    method: "deleteRepoProject",
  },
  {
    call: (api) => api.putArchitectureSource("ACME", "", { branch: "main" }, bounds),
    message: notOwnerRepo,
    method: "putArchitectureSource",
  },
  {
    call: (api) => api.syncArchitectureSource("ACME", "", bounds),
    message: notOwnerRepo,
    method: "syncArchitectureSource",
  },
  {
    call: (api) => api.deleteArchitectureSource("ACME", "", bounds),
    message: notOwnerRepo,
    method: "deleteArchitectureSource",
  },
];

/** How one call settled: `rejected` with its message, which is the contract; `threw`, which is
 *  what a method refusing before any promise exists does - `.catch` on its result never runs; or
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

/** A client whose transport answers anything at all and records what it was asked for: the
 *  refusals here land before any request, so what it records must stay empty. */
function stubbedClient(): { api: DispatchApiClient; requests: string[] } {
  const requests: string[] = [];
  const api = createApiClient(async (input) => {
    requests.push(input.toString());
    return Response.json({});
  });
  return { api, requests };
}

for (const { call, message, method } of refusals) {
  test(`${method} rejects the argument it refuses, and sends no request`, async () => {
    const { api, requests } = stubbedClient();

    expect(await settledAs(() => call(api))).toBe(`rejected: ${message}`);
    expect(requests).toEqual([]);
  });
}
