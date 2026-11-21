import { expect, test } from "bun:test";

import { NETWORK_CHANGED, repeatOnNetworkChange } from "../google-sign-in";

/** A navigation that fails with each of `failures` in turn, then answers "loaded". */
function navigation(failures: readonly string[]) {
  let made = 0;
  return {
    made: () => made,
    run: async () => {
      const failure = failures[made++];
      if (failure !== undefined) throw new Error(`page.goto: ${failure} at http://127.0.0.1:1/`);
      return "loaded";
    },
  };
}

test("a navigation the host's network change abandoned is made again, and each repeat is told", async () => {
  const told: string[] = [];
  const goto = navigation([NETWORK_CHANGED, NETWORK_CHANGED]);
  expect(await repeatOnNetworkChange("GET /", goto.run, (note) => told.push(note))).toBe("loaded");
  expect(goto.made()).toBe(3);
  expect(told).toEqual([
    `GET /: ${NETWORK_CHANGED}, made again`,
    `GET /: ${NETWORK_CHANGED}, made again`,
  ]);
});

test("a navigation that fails any other way, or keeps losing to network changes, fails as it did", async () => {
  const other = navigation(["net::ERR_CONNECTION_REFUSED"]);
  await expect(repeatOnNetworkChange("GET /", other.run, () => undefined)).rejects.toThrow(
    "net::ERR_CONNECTION_REFUSED"
  );
  expect(other.made()).toBe(1);

  const changing = navigation([NETWORK_CHANGED, NETWORK_CHANGED, NETWORK_CHANGED, NETWORK_CHANGED]);
  await expect(repeatOnNetworkChange("GET /", changing.run, () => undefined)).rejects.toThrow(
    NETWORK_CHANGED
  );
  expect(changing.made()).toBe(4);
});
