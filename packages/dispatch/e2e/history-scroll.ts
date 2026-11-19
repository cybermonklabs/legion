import type { Page } from "@playwright/test";

/**
 * Where a reader is in the issue's history, and how a test moves them.
 *
 * The history is drawn in Details (dispatch://INFRA-1196 R15), which scrolls in a box of its own
 * at every width (`features/margin/MarginSheet.tsx`'s drawer box, `overflow-y-auto`), and in the
 * page's own column wherever no drawer holds the panel. So where the reader is is that box's
 * scroll, and scrolling the window instead moves the history not at all. The box is the nearest
 * ancestor of the history whose `overflow-y` is `auto` or `scroll` and whose content is taller
 * than it, searched up to the root, or the window where there is none. The page's own search
 * (`features/issue-page/IssueHistory.tsx`'s `scrollingAncestor`, which its `ViewportAnchor`
 * holds the reader's place in) differs: it takes the nearest such box whether or not its
 * content is taller than it, and stops below `body`. The two find the same box where that
 * nearest box below `body` has content to scroll.
 */
const FIND_SCROLLPORT = `(() => {
  const history = document.querySelector('[data-testid="issue-history"]');
  for (let node = history?.parentElement; node != null; node = node.parentElement) {
    const overflow = getComputedStyle(node).overflowY;
    if ((overflow === "auto" || overflow === "scroll") && node.scrollHeight > node.clientHeight) {
      return node;
    }
  }
  return null;
})()`;

/** The history's own scroll offset, which is the window's where the page is that box. */
export function historyScrollTop(page: Page): Promise<number> {
  return page.evaluate(`(() => {
    const port = ${FIND_SCROLLPORT};
    return port === null ? window.scrollY : port.scrollTop;
  })()`) as Promise<number>;
}

/** Scrolls the history to `top`, in whichever box it scrolls in. */
export async function scrollHistoryTo(page: Page, top: number): Promise<void> {
  await page.evaluate(`(() => {
    const port = ${FIND_SCROLLPORT};
    if (port === null) window.scrollTo(0, ${top});
    else port.scrollTo({ top: ${top} });
  })()`);
}

/** Moves the reader `delta` pixels down the history, in whichever box it scrolls in. */
export async function scrollHistoryBy(page: Page, delta: number): Promise<void> {
  await page.evaluate(`(() => {
    const port = ${FIND_SCROLLPORT};
    if (port === null) window.scrollBy(0, ${delta});
    else port.scrollBy({ top: ${delta} });
  })()`);
}

/** The box the history is clipped to: the scrollport's own, or the window's where the page
 *  itself scrolls it. It is what the page measures the reader's place against
 *  (`features/shell/ViewportAnchor.tsx`'s `rowAtCenter`), so a test that reads where the
 *  reader is reads the same box. */
export function historyViewport(page: Page): Promise<{ bottom: number; top: number }> {
  return page.evaluate(`(() => {
    const port = ${FIND_SCROLLPORT};
    if (port === null) return { bottom: window.innerHeight, top: 0 };
    const box = port.getBoundingClientRect();
    return { bottom: box.bottom, top: box.top };
  })()`) as Promise<{ bottom: number; top: number }>;
}
