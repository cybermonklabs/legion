// The row `act-warnings.test.ts` runs in a child `bun test` of its own to prove the act rule of
// `setup.ts` against React's "without await" message. The file's name matches no test pattern, so
// the suite never runs it. It is a file and a process of its own because of two things
// react@19.2.8 keeps per process (`react.development.js`, `exports.act`): it prints this message,
// and the "suspended inside an `act` scope" one, once (`didWarnNoAwaitAct`); and an act whose
// result nothing awaits never lowers the act scope depth it raised, so every later act would run
// as one nested in it.
import { test } from "bun:test";
import { act } from "react";

// React checks two microtasks after `act` returns whether anything called `then` on its result
// (`queueSeveralMicrotasks`); the row waits a macrotask past that, so the message prints in it.
test("an async act nothing awaits fails its row", async () => {
  void act(async () => {});
  await Bun.sleep(0);
});
