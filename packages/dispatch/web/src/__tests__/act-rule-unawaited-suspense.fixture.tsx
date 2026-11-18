// The row `act-warnings.test.ts` runs in a child `bun test` of its own to prove the act rule of
// `setup.ts` against React's "suspended inside an `act` scope" message. The file's name matches no
// test pattern, so the suite never runs it. It is a file and a process of its own because
// react@19.2.8 prints this message, and the "without await" one, once per process
// (`react.development.js`, `didWarnNoAwaitAct`).
import { test } from "bun:test";
import { render } from "@testing-library/react";
import { Suspense, use } from "react";

/** Never settles, so the component that reads it stays suspended for the whole row. */
const pending = new Promise<string>(() => {});

function ReadsPendingText() {
  return <p>{use(pending)}</p>;
}

// `render` runs in a synchronous act nothing awaits. React leaves the suspended work queued and,
// two microtasks after the act returns, prints the message (`exports.act`,
// `queueSeveralMicrotasks`); the row waits a macrotask past that, so the message prints in it.
test("a component that suspends inside an act nothing awaits fails its row", async () => {
  render(
    <Suspense fallback={<p>loading</p>}>
      <ReadsPendingText />
    </Suspense>
  );
  await Bun.sleep(0);
});
