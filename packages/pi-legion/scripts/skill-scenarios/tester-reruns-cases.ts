#!/usr/bin/env bun
/**
 * The cases of the tester-reruns scenario, each its own world (tester-reruns-world.ts), in the
 * order a batch runs them. The module imports nothing. score.ts imports it, and must not load
 * e2e/api.ts, which reads its Dispatch port when it loads: a `bun test` process that loaded
 * e2e/api.ts through score.ts before seed.test.ts set that port would send every seed.test.ts
 * request to port 0.
 *
 *   tester-reruns-cases.ts
 *     Prints each case, one a line (rig.sh's reruns_cases).
 */

/** The cases of tester-reruns. */
export const RERUNS_CASES = ["implementer-pass-broken", "carried-own-pass", "clean-push"] as const;
export type RerunsCase = (typeof RERUNS_CASES)[number];

if (import.meta.main) console.log(RERUNS_CASES.join("\n"));
