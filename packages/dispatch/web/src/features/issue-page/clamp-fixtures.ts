import { act } from "@testing-library/react";

import type { IntersectionControl } from "../../__tests__/intersection-observer";

/** Brings the words of every row the page's one measurer watches (`clamp-measurer.ts`) within a
 *  screen of the viewport, as the browser's intersection observer reports them, and runs the
 *  batch that measures them, with every write it starts. The test installs `near`; no DOM of a
 *  unit test reports an intersection of its own. */
export async function measureNearWords(near: IntersectionControl): Promise<void> {
  act(() => {
    for (const element of near.observed()) {
      if (element.matches("[data-node-words]")) near.show(element);
    }
  });
  await act(() => {
    const { promise, resolve } = Promise.withResolvers<void>();
    window.requestAnimationFrame(() => resolve());
    return promise;
  });
}
