// The rows `act-warnings.test.ts` runs in a child `bun test` to prove the act rule of `setup.ts`
// against React's own messages. The file's name matches no test pattern, so the suite never runs
// it; a run names it by a path starting with `./`, which Bun needs to run such a file. Each row
// sets the state of a component of its own, so which row fails, and why, does not depend on the
// order the rows run in.
import { afterAll, expect, test } from "bun:test";
import { act, render, screen, waitFor } from "@testing-library/react";
import { act as reactAct, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";

import { quietConsoleError } from "./act-warnings";
import { type HeldCount, heldCount, useHeldCount } from "./held-count";

function CountSetInsideAct({ held }: { held: HeldCount }) {
  return <p>{useHeldCount(held)}</p>;
}

function CountSetOutsideAct({ held }: { held: HeldCount }) {
  return <p>{useHeldCount(held)}</p>;
}

function CountSetInWaitFor({ held }: { held: HeldCount }) {
  return <p>{useHeldCount(held)}</p>;
}

function CountSetWhileQuiet({ held }: { held: HeldCount }) {
  return <p>{useHeldCount(held)}</p>;
}

test("a state set inside act passes", () => {
  const held = heldCount();
  render(<CountSetInsideAct held={held} />);
  act(() => {
    held.set(1);
  });
  expect(screen.getByText("count 1")).toBeInTheDocument();
});

test("a state set outside act fails its row", () => {
  const held = heldCount();
  render(<CountSetOutsideAct held={held} />);
  held.set(1);
});

// The one sanctioned silencing of `console.error` drops React's line from the log, so the
// whole-output count `act-warnings.test.ts` reads finds none for this row, and still records the
// message, so the rule fails this row by its name.
test("a state set outside act fails its row while console.error is quiet", () => {
  const held = heldCount();
  render(<CountSetWhileQuiet held={held} />);
  const logged = quietConsoleError();
  try {
    held.set(1);
  } finally {
    logged.mockRestore();
  }
});

// Testing Library turns the act environment off while it waits. React's own `act` leaves it off,
// where Testing Library's would turn it on again, so the update inside it draws React's "not
// configured" message in place of passing.
test("an act inside a waitFor callback fails its row", async () => {
  const held = heldCount();
  render(<CountSetInWaitFor held={held} />);
  await waitFor(() => {
    // React's act over a synchronous callback has nothing to wait for: the row's verdict is the
    // message the update draws.
    void reactAct(() => {
      held.set(1);
    });
  });
});

/** Sets its own state from a timer its first effect starts, 20 ms after it mounts. */
function LateTimerCount() {
  const [landed, setLanded] = useState(false);
  useEffect(() => {
    const timer = setTimeout(() => setLanded(true), 20);
    return () => clearTimeout(timer);
  }, []);
  return <p>{landed ? "landed" : "waiting"}</p>;
}

// An update after the file's last row, whatever order the rows ran in: Bun runs this before the
// preload's `afterAll`, and the timer lands inside the wait. A row's component cannot give this
// case, because Testing Library's cleanup after each row unmounts it before its timer fires, and
// React then prints nothing; so this hook mounts its own component on a root it unmounts itself.
afterAll(async () => {
  const container = document.body.appendChild(document.createElement("div"));
  const root = createRoot(container);
  act(() => {
    root.render(<LateTimerCount />);
  });
  await Bun.sleep(80);
  act(() => {
    root.unmount();
  });
  container.remove();
});
