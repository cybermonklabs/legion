// The row `act-warnings.test.ts` runs in a child `bun test` of its own to prove the act rule of
// `setup.ts` against React's "overlapping act() calls" message. The file's name matches no test
// pattern, so the suite never runs it. It is a file and a process of its own because the overlap
// leaves react@19.2.8's act scope depth, which it keeps per process, one above where it started
// (`react.development.js`, `popActScope` sets it back to the depth each act began at, in the
// order the acts end), so every later act would run as one nested in it.
import { test } from "bun:test";
import { act } from "react";

// `popActScope` warns when an act ends at a depth other than one above the depth it began at. The
// first act begins at 0 and ends at 2, while the second is open; the second begins at 1 and ends
// at 0; so React prints the message twice. `Promise.all` calls `then` on both results at once, so
// React sees both awaited and prints no "without await" here.
test("two async acts that end out of order fail their row", async () => {
  await Promise.all([
    act(async () => {}),
    act(async () => {
      await Bun.sleep(10);
    }),
  ]);
});
