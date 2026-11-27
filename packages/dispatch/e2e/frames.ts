import type { Locator, Page } from "@playwright/test";

/** Waits two rendering frames in `page`. Whatever the browser has just been given - a key press,
 *  a click, a viewport change - its own handler and the render React queued for it are both done
 *  within them, so a row that reads the page after this reads what that event produced rather
 *  than what was on screen before it. */
export function nextFrames(page: Page): Promise<void> {
  return page.evaluate(() => {
    const frames = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
    return frames.promise;
  });
}

/** The `scrollTop` a scroll box settles at: `target`'s own when it is a locator, the document's
 *  when it is a page. It is read in the page once it has held still for `stillFrames` frames
 *  running. The wait is frames rather than time, since a wheel step's scroll and a smooth scroll
 *  a link started both land over frames, and so does a box the page pulls back. Ten frames is
 *  long enough for a smooth scroll to have ended; a caller whose scroll lands sooner passes fewer.
 *  A frame WebKit holds back is given up on after 100 ms, as an armed press gives it up
 *  (`FRAME_WAIT_MS`, components/focus-place.ts), and a box still moving after twenty times
 *  `stillFrames` frames is read where it stands. */
export function settledScrollTop(target: Locator | Page, stillFrames = 10): Promise<number> {
  // A page scrolls its document, whose offset its root element holds: every page of the app is
  // drawn in standards mode (`web/index.html` opens with its doctype).
  const box = "mainFrame" in target ? target.locator(":root") : target;
  return box.evaluate(async (node, needed) => {
    let last = node.scrollTop;
    for (let still = 0, frames = 0; still < needed && frames < needed * 20; frames++) {
      const drawn = Promise.withResolvers<void>();
      requestAnimationFrame(() => drawn.resolve());
      setTimeout(drawn.resolve, 100);
      await drawn.promise;
      still = node.scrollTop === last ? still + 1 : 0;
      last = node.scrollTop;
    }
    return last;
  }, stillFrames);
}
