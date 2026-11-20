import { expect, test } from "bun:test";

import { inVisualOrder, visualOrder } from "../visual-order";

// The visual review runs its rows in two orders (e2e/visual-order.ts says why); the variable that
// picks the order is read here, and an order nobody asked for fails rather than running.

test("the visual rows run in their listed order unless reverse is asked for", () => {
  expect(visualOrder({})).toBe("normal");
  expect(visualOrder({ DISPATCH_E2E_VISUAL_ORDER: "" })).toBe("normal");
  expect(visualOrder({ DISPATCH_E2E_VISUAL_ORDER: "normal" })).toBe("normal");
  expect(visualOrder({ DISPATCH_E2E_VISUAL_ORDER: "reverse" })).toBe("reverse");
});

test("an order that is neither normal nor reverse fails by its value", () => {
  expect(() => visualOrder({ DISPATCH_E2E_VISUAL_ORDER: "shuffle" })).toThrow(
    'DISPATCH_E2E_VISUAL_ORDER is "shuffle"; it must be unset, normal or reverse'
  );
});

test("reverse puts every pair of rows the other way round, and leaves the list it was given", () => {
  const rows = ["inbox", "answers", "broadcast", "issue-spec"] as const;
  expect(inVisualOrder(rows, "normal")).toEqual(rows);
  expect(inVisualOrder(rows, "reverse")).toEqual(["issue-spec", "broadcast", "answers", "inbox"]);
  expect(rows).toEqual(["inbox", "answers", "broadcast", "issue-spec"]);
});
