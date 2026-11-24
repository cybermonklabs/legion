import { describe, expect, test } from "bun:test";

import { layoutRowTimeoutMs, loadRetryingNetworkChange, requiredGroupsOf } from "../layout-check";
import { layoutRoutes } from "../layout-routes";

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

  // Playwright reports some loads a network change cancelled only as a navigation to Chromium's
  // error page that interrupted them; the document request's own failure then says why.
  const interrupted =
    'Navigation to "http://127.0.0.1:4377/agents" is interrupted by another navigation to "chrome-error://chromewebdata/"';
  test("loads again after an interrupted load whose document request a network change failed", async () => {
    const scripted = scriptedLoad([interrupted]);
    await loadRetryingNetworkChange(scripted.load, async () => "net::ERR_NETWORK_CHANGED");
    expect(scripted.calls()).toBe(2);
  });

  test("throws an interrupted load whose document request failed for another reason, or none", async () => {
    for (const documentFailure of ["net::ERR_CONNECTION_REFUSED", undefined]) {
      const scripted = scriptedLoad([interrupted]);
      await expect(
        loadRetryingNetworkChange(scripted.load, async () => documentFailure)
      ).rejects.toThrow("chrome-error://chromewebdata/");
      expect(scripted.calls()).toBe(1);
    }
  });
});

describe("the layout rows' budget and required groups", () => {
  test("gives a route row a budget that follows its readings, 6 s each, and never under 60 s", () => {
    const state = { act: async () => undefined, name: "a state" };
    // Five widths and three states: 20 readings, 120 s.
    expect(layoutRowTimeoutMs({ states: [state, state, state] })).toBe(120_000);
    // Five widths and no state: 5 readings, which keep Playwright's 60 s.
    expect(layoutRowTimeoutMs({})).toBe(60_000);
  });

  // A state draws on the page its route loads, so a state reading requires the route's groups as
  // well as its own: a rule where the state's list replaced the route's would let the Agents rows
  // go unmarked in both states, and the Labels cell with two rows selected.
  test("requires a route's groups in each of its states, and the state's own beside them", () => {
    expect(
      requiredGroupsOf({ requiredGroups: ["labels"] }, { requiredGroups: ["chosen"] })
    ).toEqual(["labels", "chosen"]);
    expect(requiredGroupsOf({ requiredGroups: ["labels"] }, {})).toEqual(["labels"]);
    expect(requiredGroupsOf({})).toEqual([]);
    const groupsIn = (routeName: string, stateName: string) => {
      const route = layoutRoutes.find((candidate) => candidate.name === routeName);
      const state = route?.states?.find((candidate) => candidate.name === stateName);
      if (route === undefined || state === undefined) throw new Error(`${routeName}#${stateName}`);
      return requiredGroupsOf(route, state);
    };
    expect(groupsIn("agents", "peek-drawer")).toEqual(["roles"]);
    expect(groupsIn("agents", "all-agents-selected")).toEqual(["roles", "chosen-agents"]);
    expect(groupsIn("project-issues", "two-rows-selected")).toEqual(["labels"]);
  });
});
