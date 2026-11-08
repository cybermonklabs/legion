import { expect, test } from "bun:test";

import { createApiClient } from "./client";

// write-path-param.test.ts pins the five writes whose refusal comes from `routeUrl`. A write
// method can refuse before `routeUrl` too, from a helper that reads one of its own arguments:
// `repoParams` refuses a repository that is not `owner/repo`. That refusal belongs to the same
// contract — the write store catches a refused write on the promise the press returns, so a
// method that throws before any promise exists is caught nowhere and the press hangs — and the
// two methods of one route disagree about it.
//
// Measured at 06814780583f with the same input, the empty repository: `putRepoProject` throws
// `repository must be formatted as owner/repo` synchronously, because it is not `async`, while
// `deleteRepoProject`, which is, rejects with that same message. A sweep of all 127 methods of
// the client found this one and no other, so the pair below is the whole of what is left.
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

test("a write refusing its own argument rejects rather than throwing, on both methods of the route", async () => {
  const requests: string[] = [];
  const api = createApiClient(async (input) => {
    requests.push(input.toString());
    return Response.json({});
  });
  const bounds = { key: "press-1" };

  const settled = {
    putRepoProject: await settledAs(() => api.putRepoProject("", { project: "ACME" }, bounds)),
    deleteRepoProject: await settledAs(() => api.deleteRepoProject("", bounds)),
  };

  expect(settled).toEqual({
    putRepoProject: "rejected: repository must be formatted as owner/repo",
    deleteRepoProject: "rejected: repository must be formatted as owner/repo",
  });
  expect(requests).toEqual([]);
});
