/** The refusal of the first-rollout run (e2e/first-rollout.ts), which the second browser run of
 *  CI's dispatch-two-builds job gets through `DISPATCH_E2E_FIRST_ROLLOUT_COMMIT`. The run holds
 *  Dispatch B to that commit and to a live registry whose lowest schema version is 0, so a build
 *  that registers its schema numbers, or a build of another commit, fails the run instead of
 *  passing it as the first-rollout build. Without the variable nothing is refused. */
import { describe, expect, test } from "bun:test";

import { firstRolloutRefusal } from "../first-rollout";

const pin = "edb69e0d7588ec037847182e74a04a343a0664c9";
const later = "76bff46e983f725617e1039568ef0175b458084e";

describe("the first-rollout run's refusal", () => {
  test("refuses nothing when no commit is given, whatever B and the fleet are", () => {
    for (const commitB of [undefined, pin, later]) {
      for (const minVersion of [0, 1, 7]) {
        expect(firstRolloutRefusal(undefined, commitB, { maxCreatable: 7, minVersion })).toBe(
          undefined
        );
      }
    }
  });

  test("refuses nothing when B is the pinned commit and the fleet reads version 0", () => {
    expect(firstRolloutRefusal(pin, pin, { maxCreatable: 7, minVersion: 0 })).toBe(undefined);
    expect(firstRolloutRefusal(pin, pin, { maxCreatable: 0, minVersion: 0 })).toBe(undefined);
  });

  test("refuses a B of the pinned commit while the fleet reads version 1 or more", () => {
    for (const minVersion of [1, 6]) {
      const refusal = firstRolloutRefusal(pin, pin, { maxCreatable: 6, minVersion });
      expect(refusal).toContain(`lowest schema version of ${minVersion} `);
      expect(refusal).toContain("highest creatable number of 6 ");
      expect(refusal).toContain(pin);
      expect(refusal).toContain("not the first-rollout build");
    }
  });

  test("refuses a B of another commit, naming both commits", () => {
    for (const minVersion of [0, 6]) {
      const refusal = firstRolloutRefusal(pin, later, { maxCreatable: 6, minVersion });
      expect(refusal).toContain(pin);
      expect(refusal).toContain(later);
    }
  });

  test("refuses a B that has no COMMIT file, naming the pinned commit", () => {
    const refusal = firstRolloutRefusal(pin, undefined, { maxCreatable: 0, minVersion: 0 });
    expect(refusal).toContain(pin);
    expect(refusal).toContain("no COMMIT file");
  });
});
