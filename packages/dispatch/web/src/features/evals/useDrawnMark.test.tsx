import { expect, test } from "bun:test";
import { renderHook } from "@testing-library/react";

import { useDrawnMark } from "./useDrawnMark";

const marks = (name: string) => performance.getEntriesByName(name, "mark");

test("a part marks the commit it first draws in, once, and clears its mark when it unmounts", () => {
  const name = "dispatch:evals-section:drawn-mark-test";
  performance.clearMarks(name);
  const { rerender, unmount } = renderHook(({ drawn }) => useDrawnMark(name, drawn), {
    initialProps: { drawn: false },
  });
  // Not drawn yet: nothing on the timeline.
  expect(marks(name)).toHaveLength(0);

  // The commit that draws it marks it, from its layout effect.
  const before = performance.now();
  rerender({ drawn: true });
  const drawn = marks(name);
  expect(drawn).toHaveLength(1);
  expect(drawn[0]?.startTime).toBeGreaterThanOrEqual(before);

  // A later commit that still draws it marks nothing more.
  rerender({ drawn: true });
  expect(marks(name)).toHaveLength(1);

  // It goes with the part.
  unmount();
  expect(marks(name)).toHaveLength(0);
});

test("a part that stops drawing clears its mark, and marks again when it draws again", () => {
  const name = "dispatch:evals-job-list:drawn-mark-test";
  performance.clearMarks(name);
  const { rerender, unmount } = renderHook(({ drawn }) => useDrawnMark(name, drawn), {
    initialProps: { drawn: true },
  });
  expect(marks(name)).toHaveLength(1);
  rerender({ drawn: false });
  expect(marks(name)).toHaveLength(0);
  rerender({ drawn: true });
  expect(marks(name)).toHaveLength(1);
  unmount();
  expect(marks(name)).toHaveLength(0);
});
