import { describe, expect, test } from "bun:test";

import { loadRetryingNetworkChange } from "../layout-check";

/** A load that fails with each of `failures` in turn, then succeeds, and counts its calls. */
function scriptedLoad(failures: readonly string[]): {
  load: () => Promise<void>;
  calls: () => number;
} {
  let calls = 0;
  return {
    calls: () => calls,
    load: async () => {
      const failure = failures[calls];
      calls += 1;
      if (failure !== undefined)
        throw new Error(`page.goto: ${failure} at http://127.0.0.1:4377/agents`);
    },
  };
}

// The layout rows load each page through this loop: Chromium cancels a load in flight when it
// sees the host's network addresses change (`net::ERR_NETWORK_CHANGED`), and nothing was drawn
// then, so that load alone is issued again, at most twice.
describe("the layout rows' load retry", () => {
  test("loads again after one and after two network changes", async () => {
    for (const changes of [1, 2]) {
      const scripted = scriptedLoad(
        Array.from({ length: changes }, () => "net::ERR_NETWORK_CHANGED")
      );
      await loadRetryingNetworkChange(scripted.load);
      expect(scripted.calls()).toBe(changes + 1);
    }
  });

  test("throws a third network change, after three loads", async () => {
    const scripted = scriptedLoad(Array.from({ length: 3 }, () => "net::ERR_NETWORK_CHANGED"));
    await expect(loadRetryingNetworkChange(scripted.load)).rejects.toThrow(
      "net::ERR_NETWORK_CHANGED"
    );
    expect(scripted.calls()).toBe(3);
  });

  test("throws any other failure at the first load", async () => {
    for (const failure of [
      "net::ERR_CONNECTION_REFUSED",
      "net::ERR_INTERNET_DISCONNECTED",
      "Timeout 30000ms exceeded.",
    ]) {
      const scripted = scriptedLoad([failure]);
      await expect(loadRetryingNetworkChange(scripted.load)).rejects.toThrow(failure);
      expect(scripted.calls()).toBe(1);
    }
  });
});
