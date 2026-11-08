import { expect, test } from "bun:test";

import { createApiClient } from "./client";

// `write-argument-refusal.test.ts` pins the repo-project pair, the one the tester's sweep of the
// client reported. Two more methods refuse the same way and were missed, because `repoParams`
// reads their *second* argument rather than their first, so a sweep that empties the first
// argument of every method makes them refuse on `project` and never reaches the repository:
// `putArchitectureSource` and `syncArchitectureSource` both spread `repoParams(repo)` into the
// params of their call and were not `async`, so the refusal landed before any promise existed.
// Measured on the real client with a valid project and the empty repository, before the fix:
// both threw `repository must be formatted as owner/repo` where `deleteArchitectureSource`,
// which is `async`, rejected with that same message. Three methods of this route family, two
// refusing one way and one the other.
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

test("every architecture-source write rejects a repository it refuses rather than throwing", async () => {
  const requests: string[] = [];
  const api = createApiClient(async (input) => {
    requests.push(input.toString());
    return Response.json({});
  });
  const bounds = { key: "press-1" };

  const settled = {
    deleteArchitectureSource: await settledAs(() => api.deleteArchitectureSource("ACME", "", bounds)),
    putArchitectureSource: await settledAs(() =>
      api.putArchitectureSource("ACME", "", { branch: "main" }, bounds)
    ),
    syncArchitectureSource: await settledAs(() => api.syncArchitectureSource("ACME", "", bounds)),
  };

  expect(settled).toEqual({
    deleteArchitectureSource: "rejected: repository must be formatted as owner/repo",
    putArchitectureSource: "rejected: repository must be formatted as owner/repo",
    syncArchitectureSource: "rejected: repository must be formatted as owner/repo",
  });
  expect(requests).toEqual([]);
});
