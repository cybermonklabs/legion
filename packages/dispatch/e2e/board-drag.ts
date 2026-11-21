/** A whole-card drag on the project board with the mouse, and the two waits it rests on: the
 *  board at rest before the press, and clicks delivered again after the drop. `board.e2e.ts` and
 *  `parent-status-choice.e2e.ts` drag cards with it, so each takes this one body. */

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

/** Drags with the mouse from the middle of `source` to `target` (a card or a column). */
export async function mouseDrag(page: Page, source: Locator, target: Locator, yOffset?: number) {
  await boardAtRest(page);
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
  await target.scrollIntoViewIfNeeded();
  // The scroll above waited for the target to be visible and still, so its box is there.
  const targetBox = await target.boundingBox();
  if (targetBox === null) {
    throw new Error("drag target is not visible");
  }
  await page.mouse.move(
    targetBox.x + targetBox.width / 2,
    yOffset === undefined ? targetBox.y + targetBox.height / 2 : targetBox.y + yOffset,
    { steps: 24 }
  );
  await page.mouse.up();
  await clicksDelivered(page);
}
