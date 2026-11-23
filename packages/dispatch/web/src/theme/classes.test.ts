import { expect, test } from "bun:test";

import { scrollFadeTrailing, scrollFadeTrailingPx } from "./classes";

// Tailwind finds a class only by its literal text, so the class spells the fade's width out and
// `scrollFadeTrailingPx` says it a second time for code that keeps clear of the fade. A change to
// either one fails here until the other follows.
test("the trailing fade's class is as wide as scrollFadeTrailingPx says", () => {
  const widths = [...scrollFadeTrailing.matchAll(/(\d+)px/g)].map((match) => Number(match[1]));
  expect(widths).toEqual([scrollFadeTrailingPx]);
});
