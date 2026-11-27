import { act } from "@testing-library/react";

import type { IntersectionControl } from "../../__tests__/intersection-observer";

/** Brings the words of every row the page's one measurer watches (`clamp-measurer.ts`) within a
 *  screen of the viewport, as the browser's intersection observer reports them, and runs the
 *  batch that measures them, with every write it starts. The test installs `near`; no DOM of a
 *  unit test reports an intersection of its own.
 *  This one stays on the real clock (INFRA-983): the batch is the frame the measurer asks for as
 *  the rows come near, and a row that reads a row the batch left whole has no state to wait for
 *  but that frame; Bun's fake clock does not hold happy-dom's frame, which no move of it runs. */
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
