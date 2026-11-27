/** A whole-card drag on the project board with the mouse, and the waits it rests on: the board
 *  at rest before the press, the view at rest at the aim and at the release, and clicks
 *  delivered again after the drop. `board.e2e.ts` and `parent-status-choice.e2e.ts` drag cards
 *  with it, so each takes this one body, and `board.e2e.ts` waits on `viewAtRest` itself too. */

import type { Locator, Page } from "@playwright/test";

/**
 * Waits until no card is mid-flight. dnd-kit lands every reorder it did not animate itself - the
 * rollback and refetch after a rejected rank included - with a 200 ms transform transition, and a
 * card later in the DOM paints over the ones it crosses, so a press during that flight lifts
 * whichever card is passing the point rather than the one measured there.
 */
async function boardAtRest(page: Page): Promise<void> {
  await page.getByRole("region", { name: "Project board" }).evaluate(async (board) => {
    for (;;) {
      const running = board.getAnimations({ subtree: true });
      if (running.length === 0) {
        return;
      }
      await Promise.allSettled(running.map((animation) => animation.finished));
    }
  });
}

/**
 * Waits until a click reaches its target again. dnd-kit's pointer sensors stop every click at the
 * document (capture phase) from the lift until a 50 ms timer after the drop, and a busy main
 * thread runs that timer late, so a control clicked straight after a drop is silently ignored.
 * The probe is a node under `body` outside the React root: only that suppressor can stop it.
 */
async function clicksDelivered(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const probe = document.body.appendChild(document.createElement("div"));
    let delivered = false;
    probe.addEventListener("click", () => {
      delivered = true;
    });
    try {
      for (;;) {
        delivered = false;
        probe.dispatchEvent(new MouseEvent("click"));
        if (delivered) {
          return;
        }
        const turn = Promise.withResolvers<void>();
        setTimeout(turn.resolve, 0);
        await turn.promise;
      }
    } finally {
      probe.remove();
    }
  });
}

/**
 * Waits until `element`, every element around it and the window have held their scroll offsets
 * across two animation frames, and returns those offsets. dnd-kit scrolls a scroller a step every
 * 5 ms while the pointer or finger of a carried card is in an edge zone of it, and stops once it
 * leaves; a card in flight takes the snapping off the board's scrollers (`IssueBoard.tsx`), so no
 * fling and no snap follows that scrolling: one unchanged pair of frames is the view at rest.
 */
export async function viewAtRest(element: Locator): Promise<string> {
  return element.evaluate(async (start) => {
    const offsets = (): string => {
      const read = [`window ${window.scrollX},${window.scrollY}`];
      for (let node: Element | null = start; node !== null; node = node.parentElement) {
        read.push(`${node.tagName.toLowerCase()} ${node.scrollLeft},${node.scrollTop}`);
      }
      return read.join("; ");
    };
    for (;;) {
      const was = offsets();
      const frame = Promise.withResolvers<void>();
      requestAnimationFrame(() => requestAnimationFrame(() => frame.resolve()));
      await frame.promise;
      if (offsets() === was) {
        return was;
      }
    }
  });
}

/**
 * Drags with the mouse from the middle of `source` to `target` (a card or a column), or to
 * `yOffset` below the target's top. While the pointer of a carried card is within a fifth of a
 * scroller's height or width of its edge, dnd-kit scrolls that scroller on for as long as the
 * pointer stays there (`defaultThreshold` and `getScrollDirectionAndSpeed`, @dnd-kit/core 6.3.1's
 * `dist/core.esm.js:827-874`; the board's `DndContext` leaves auto-scroll on). A drop aimed at a
 * target only scrolled into view sits at the window's edge, and the card lands wherever the page
 * has scrolled to by the release, which a loaded runner delivers late. So the source and then the
 * target are each brought to the middle of every scroller around them, out of every edge zone, or
 * to a scroller's end, from which it can scroll no further; and the button is released only with
 * the view where it was when the drop was aimed.
 */
export async function mouseDrag(page: Page, source: Locator, target: Locator, yOffset?: number) {
  await boardAtRest(page);
  await source.evaluate((element) => element.scrollIntoView({ block: "center", inline: "center" }));
  await viewAtRest(source);
  // `hover` presses only once the card holds still and is the element under the pointer; a bare
  // `mouse.move` to a measured box would press on whatever is painted there.
  await source.hover();
  // The hover above waited for the card to be visible and still, so its box is there.
  const sourceBox = await source.boundingBox();
  if (sourceBox === null) {
    throw new Error("drag source is not visible");
  }
  await page.mouse.down();
  await page.mouse.move(sourceBox.x + sourceBox.width / 2 + 10, sourceBox.y + sourceBox.height / 2);
  await target.evaluate((element) => element.scrollIntoView({ block: "center", inline: "center" }));
  // The offsets are read once the view has held still, and the release compares against them.
  const aimedAt = await viewAtRest(target);
  await boardAtRest(page);
  // The view and the cards are at rest, so the box read now is where the drop lands.
  const targetBox = await target.boundingBox();
  if (targetBox === null) {
    throw new Error("drag target is not visible");
  }
  await page.mouse.move(
    targetBox.x + targetBox.width / 2,
    yOffset === undefined ? targetBox.y + targetBox.height / 2 : targetBox.y + yOffset,
    { steps: 24 }
  );
  // Read once the view has held still again: any change from the aim is a real scroll.
  const releasedAt = await viewAtRest(target);
  if (releasedAt !== aimedAt) {
    throw new Error(
      `the view scrolled under the drop: aimed at ${aimedAt}, released at ${releasedAt}`
    );
  }
  await page.mouse.up();
  await clicksDelivered(page);
}
