import { expect, type Locator, type Page, test } from "@playwright/test";

import { createIssue, createProject, createProjectDocument } from "./api";
import { expectRootFont, LARGE_FONT, setDefaultFont, XL } from "./default-font";
import { nextFrames } from "./frames";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The shell's layout rows that run on the real stack, with the reason they are not stubbed rows
 * below. What this file held before - the Conversation's geometry at three widths and the
 * compact review sheet a comment deep link leaves reachable - is in `layout.stub.e2e.ts`.
 *
 * The first two rows read a whole page load on each of the two routes that draw a margin, and
 * one of those routes is a project document, whose page is the live document's: the margin sits
 * on that document's room, which no recording replays (packages/dispatch/README.md, "The stubbed
 * layer"). The resize row reads one of those loads, an issue's, on every frame of two resizes
 * across 1280 px.
 *
 * The two default-font rows read only the shell's navigation over the Inbox, which a recording
 * could answer. No recording of them is committed, and one is made only against the real
 * harness (`bun run e2e:stub:record`), so they stay here, on the stack they were written on.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

/** What one animation frame laid out, as the resize row below reads it. */
interface ResizeFrame {
  innerWidth: number;
  /** `<main>`'s width. */
  main: number;
  /** The margin's own box, the sheet the compact layout lays over the page or the column the
   *  desktop one lays beside it; null in a frame that draws no margin at all. */
  margin: { left: number; right: number; width: number } | null;
  scrollWidth: number;
}

declare global {
  interface Window {
    /** The box `<main>` was laid out at in the commit that first put it on the page, recorded
     *  from before the page's first script by the row below. */
    __mainAtFirstCommit?: string;
    /** The resize row's reading of the page as it is laid out at this moment. */
    __readResizeFrame?: () => ResizeFrame;
    /** Every animation frame the resize row has recorded, while `__resizeRecording` holds. */
    __resizeFrames?: ResizeFrame[];
    __resizeRecording?: boolean;
  }
}

// The shell's `<main>` is the column every route's content is laid out in, and on a route that
// draws a margin it is only as wide as it is because the margin's column stands beside it.
//
// The margin's own chunk waits for the route to be drawn (`TheRouteIsDrawn` in
// `web/src/app.tsx`), since a boundary suspended beside the route's own costs the page a
// 300 ms fallback throttle. `theRouteIsDrawn` is turned on by an effect, so the commit that
// first puts the shell on the page is always one commit short of it, and until the shell held
// the reserved column itself that commit laid `<main>` out across the margin's column. Whether
// the browser painted it was the scheduler's to decide: on one sweep of this pod every margin
// route painted 72 to 149 ms of it, and on another none did, so what this row reads is the
// commit rather than a frame. Anything the shell draws in it - the route's own skeleton, and
// on an issue document the blocks the server sent - is laid out at one width and re-wrapped at
// another whenever that paint does happen (dispatch://INFRA-341, R11).
//
// A `MutationObserver` is what reads it: its callback runs as a microtask at the end of the
// task React commits in, before the task React flushes its passive effects in, so the box it
// reads is the one that commit laid out whether or not a frame was ever drawn from it.
//
// Both of `routeHasMargin`'s answers are read here, an issue's tab and a project document,
// because the reservation is drawn from that one rule.
for (const route of ["the issue's Conversation tab", "a project document"] as const) {
  // SLOW(>=2s): a whole page load, read from before the page's first script until its margin
  // has arrived.
  test(`the shell lays main out at the box it keeps in its first commit on ${route}`, async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({
      project: "CORE",
      spec: "Review this issue.",
      title: "A margin route",
    });
    await createProjectDocument("CORE", {
      content: "# Ask blocks design\n\nThe ask is a node.\n",
      name: "ask-blocks-design.md",
    });
    const path =
      route === "the issue's Conversation tab"
        ? `/issues/${issue.key}/conversation`
        : "/projects/CORE/documents/ask-blocks-design-md";
    const context = await asUser(browser, "alice");
    const page = await context.newPage();
    try {
      await page.setViewportSize({ height: 900, width: 1280 });
      // From before the page's own first script: `addInitScript` runs at document start, ahead
      // of the inline scripts of `index.html` and of the entry chunk.
      await page.addInitScript(() => {
        const watch = new MutationObserver(() => {
          const main = document.querySelector('[data-testid="main-content"]');
          if (main === null) return;
          const box = main.getBoundingClientRect();
          window.__mainAtFirstCommit = `${Math.round(box.x)}..${Math.round(box.x + box.width)}`;
          watch.disconnect();
        });
        // `document` rather than `document.documentElement`: this runs before the parser has
        // created `<html>`, so the element is not there to observe yet.
        watch.observe(document, { childList: true, subtree: true });
      });
      await page.goto(path);

      // The page settled: the margin is in the column rather than the box held open for it.
      await expect(page.getByRole("complementary", { name: "Review margin" })).toBeVisible();
      await expect(page.locator("[data-margin-pending]")).toHaveCount(0);
      await expect(page.locator('[data-testid="main-content"]')).toBeVisible();
      // The wait above drew the shell's main content, and a drawn element has a box.
      const settled = await page.locator('[data-testid="main-content"]').boundingBox();
      if (settled === null) {
        throw new Error("the shell's main content is not visible");
      }
      const kept = `${Math.round(settled.x)}..${Math.round(settled.x + settled.width)}`;
      const first = await page.evaluate(() => window.__mainAtFirstCommit);

      expect(
        first,
        "main never reached the page, so this run says nothing about the shell's first commit"
      ).toBeDefined();
      expect(
        first,
        "the shell laid main out across the margin's column in the commit that first drew it, " +
          "so everything in that commit is re-wrapped under the reader as soon as the browser " +
          "paints it (`web/src/app.tsx`, the reservation beside `MarginColumn`)"
      ).toBe(kept);
    } finally {
      await context.close();
    }
  });
}

/** Waits until the page shows the layout `side` of 1280 px draws, by the controls only that
 *  layout's render puts there, and then two frames more, so what is read next is the settled
 *  page rather than the frame the render landed in. */
async function settledAt(page: Page, side: "compact" | "desktop"): Promise<void> {
  if (side === "desktop") {
    await expect(
      page.getByRole("complementary", { exact: true, name: "Review margin" })
    ).toBeVisible();
    await expect(
      page.getByRole("complementary", { exact: true, name: "Navigation" })
    ).toBeVisible();
  } else {
    await expect(page.getByRole("button", { name: /^Open review panel/ })).toBeVisible();
    await expect(page.getByRole("button", { exact: true, name: "Open navigation" })).toBeVisible();
  }
  await expect(page.locator("[data-margin-pending]")).toHaveCount(0);
  await nextFrames(page);
}

// dispatch://INFRA-1630. The shell and the margin decided the 1280 px breakpoint on two clocks:
// the shell's `xl:flex` is CSS, which the browser applies in the frame the window crosses, and
// the margin's place was React state from `useMediaQuery`, which renders a task or two later.
// Crossing upward, the margin's wrapper was still `display: contents` in that gap, so its sheet
// was a full-width flex child of the desktop shell: `<main>` fell to almost nothing and the margin
// ran past the viewport for one or two frames, 16 to 39 ms. A reading of the settled page never
// sees that, so this row records every animation frame, from before the first resize to after
// the page settled from the last, and holds each one to the layout the page settles at for that
// frame's width: `<main>` as wide, the margin's box where it settles and inside the viewport, and
// nothing wider than the viewport.
// SLOW(>=2s): a whole page load, then two resizes, each read on every frame until it settles.
test("the resize row that crosses 1280 px in both directions and reads every animation frame: no frame draws the compact margin inside the desktop shell, and main keeps its width", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Review this issue.",
    title: "A margin route",
  });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(`/issues/${issue.key}`);
    await settledAt(page, "desktop");
    await page.evaluate(() => {
      window.__readResizeFrame = () => {
        const main = document.querySelector('[data-testid="main-content"]');
        const sheet = document
          .querySelector('[data-testid="margin-sheet"]')
          ?.getBoundingClientRect();
        return {
          innerWidth: window.innerWidth,
          main: main === null ? 0 : main.getBoundingClientRect().width,
          margin:
            sheet === undefined || sheet.width === 0
              ? null
              : { left: sheet.left, right: sheet.right, width: sheet.width },
          scrollWidth: document.documentElement.scrollWidth,
        };
      };
    });
    // The page as it settled, which `settledAt` has just waited for.
    const settledLayout = () =>
      page.evaluate(() => {
        const frame = window.__readResizeFrame?.();
        if (frame === undefined) throw new Error("the page's layout reading was not installed");
        return frame;
      });
    const desktop = await settledLayout();

    // From here to the end, one reading in every animation frame the page draws.
    await page.evaluate(() => {
      const frames: ResizeFrame[] = [];
      window.__resizeFrames = frames;
      window.__resizeRecording = true;
      const record = () => {
        const frame = window.__readResizeFrame?.();
        if (frame !== undefined) frames.push(frame);
        if (window.__resizeRecording) requestAnimationFrame(record);
      };
      requestAnimationFrame(record);
    });
    await page.setViewportSize({ height: 900, width: 1279 });
    await settledAt(page, "compact");
    const compact = await settledLayout();
    await page.setViewportSize({ height: 900, width: 1440 });
    await settledAt(page, "desktop");
    const frames = await page.evaluate(() => {
      window.__resizeRecording = false;
      return window.__resizeFrames ?? [];
    });

    expect(desktop.margin, "the settled desktop page draws its margin").not.toBeNull();
    expect(compact.margin, "the settled compact page draws its margin").not.toBeNull();
    // Every frame is at one of the two widths the row set, and both are read: a clause about
    // every frame is true of none at all.
    expect(frames.filter((frame) => frame.innerWidth === 1279).length).toBeGreaterThan(0);
    expect(frames.filter((frame) => frame.innerWidth === 1440).length).toBeGreaterThan(0);
    expect(
      frames.filter((frame) => frame.innerWidth !== 1279 && frame.innerWidth !== 1440)
    ).toEqual([]);
    // And `main` was drawn at both widths: a settled `main` of no width would let a frame whose
    // `main` is no width either through the `main` half of the clause below.
    expect(desktop.main, "the settled desktop page draws no main").toBeGreaterThan(0);
    expect(compact.main, "the settled compact page draws no main").toBeGreaterThan(0);
    const squeezed = frames
      .map((frame, index) => ({ frame, index }))
      .filter(({ frame }) => {
        const settled = frame.innerWidth === 1440 ? desktop : compact;
        return (
          frame.scrollWidth > frame.innerWidth ||
          Math.abs(frame.main - settled.main) > 1 ||
          frame.margin === null ||
          settled.margin === null ||
          frame.margin.left < 0 ||
          frame.margin.right > frame.innerWidth ||
          Math.abs(frame.margin.left - settled.margin.left) > 1 ||
          Math.abs(frame.margin.width - settled.margin.width) > 1
        );
      });
    expect(
      squeezed,
      `of ${frames.length} frames, these drew a layout other than the one the page settles at ` +
        `for their width (settled: ${JSON.stringify({ compact, desktop })}): the compact margin ` +
        "inside the desktop shell, or the desktop margin over the compact one"
    ).toEqual([]);
  } finally {
    await context.close();
  }
});

// The shell's two halves read one breakpoint whatever the reader's default font size. The `xl:`
// classes that show the sidebar compile to `@media (width >= 80rem)`, and a media query's rem is
// the browser's default font size; the compact bar with its Menu follows
// `COMPACT_VIEWPORT_QUERY`, which is in rem too (`web/src/features/shell/useDialog.ts`). While
// that query was written in pixels, a reader who set a 20 px default font met a 1440 px window in
// which the sidebar waited for 1600 px and the Menu had stopped at 1279 px, so no way to the
// navigation was drawn at all, and at a 12 px font neither was drawn from 960 px to 1279 px
// (INFRA-341, testing round 17, at 1808df5f). This row reads, at those fonts inside those bands
// and at the browser's own 16 px either side of 1280 px, that the Menu, the rail or the sidebar
// is drawn, on a fresh load of the Inbox held to its font size each time.
test("a reader whose default font is 20 px or 12 px is drawn a way to the navigation at every width", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    for (const [fontSize, width] of [
      [20, 1440],
      [20, 1300],
      [12, 1100],
      [16, 1440],
      [16, 1100],
    ] as const) {
      const at = `at ${width} px with a ${fontSize} px default font`;
      await setDefaultFont(page, fontSize);
      await page.setViewportSize({ height: 900, width });
      await page.goto("/");
      await expect(page.getByTestId("main-content"), `${at}: main`).toBeVisible();
      await expectRootFont(page, fontSize, at);
      const ways = await page.evaluate(() => {
        const shown = (selector: string) =>
          [...document.querySelectorAll<HTMLElement>(selector)].some(
            (element) => element.checkVisibility() && element.getBoundingClientRect().width > 0
          );
        return {
          menu: shown('button[aria-label="Open navigation"]'),
          rail: shown('aside[aria-label="Collapsed sidebar"]'),
          sidebar: shown('aside[aria-label="Navigation"]:not([aria-modal])'),
        };
      });
      expect(
        ways.menu || ways.rail || ways.sidebar,
        `${at}: the Menu, the rail or the sidebar is drawn (${JSON.stringify(ways)})`
      ).toBe(true);
    }
  } finally {
    await context.close();
  }
});

/** Tailwind's `md` in rem (`--breakpoint-md` in its `theme.css`): where the pages' own `md:`
 *  classes and `MD_VIEWPORT_QUERY` switch. */
const MD = 48;

/** A box the page draws, in the page's px. */
interface Edges {
  readonly bottom: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
}

/** The box `locator` draws, which the row has already waited to see drawn. */
async function edgesOf(locator: Locator, what: string): Promise<Edges> {
  // The row's wait drew each box before it asks, and a drawn element has a box, so it is there.
  const box = await locator.boundingBox();
  if (box === null) throw new Error(`${what} is not drawn`);
  return { bottom: box.y + box.height, left: box.x, right: box.x + box.width, top: box.y };
}

/** Whether two boxes share more than the pixel two boxes laid side by side meet within. */
function overlap(a: Edges, b: Edges): boolean {
  return (
    Math.min(a.right, b.right) - Math.max(a.left, b.left) > 1 &&
    Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1
  );
}

// The row above reads widths inside each layout. This one reads a 20 px default font at the
// widths the shell's layouts switch at, where `xl` is 1600 px and `md` 960 px, one pixel under
// each and at it, with a fresh load at each. At each it holds the layout that side of `xl` draws,
// the navigation reachable in it - the sidebar beside `<main>`, or the Menu and the drawer it
// opens - and nothing the shell draws over the page: the sidebar clear of `<main>` and of the
// page's first row, the compact bar above both.
test("at a 20 px default font the navigation is reachable and the shell draws nothing over the page just under and at md and xl: 959, 960, 1599 and 1600 px", async ({
  browser,
}) => {
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await setDefaultFont(page, LARGE_FONT);
    for (const width of [
      MD * LARGE_FONT - 1,
      MD * LARGE_FONT,
      XL * LARGE_FONT - 1,
      XL * LARGE_FONT,
    ]) {
      const at = `at ${width} px with a ${LARGE_FONT} px default font`;
      await page.setViewportSize({ height: 900, width });
      // The Inbox's own address: `/inbox` is no route of the app, and draws the not-found page.
      await page.goto("/");
      const main = page.getByTestId("main-content");
      // The Inbox page's first row: its heading, which the page draws before its list arrives.
      const firstRow = main.getByRole("heading", { exact: true, level: 1, name: "Inbox" });
      await expect(main, `${at}: main`).toBeVisible();
      await expect(firstRow, `${at}: the page's first row`).toBeVisible();
      // The page is drawn at the size the row says it reads, so a 16 px reading cannot pass
      // for this one.
      await expectRootFont(page, LARGE_FONT, at);
      const menu = page.getByRole("button", { exact: true, name: "Open navigation" });
      const sidebar = page.getByRole("complementary", { exact: true, name: "Navigation" });
      const rail = page.getByRole("complementary", { exact: true, name: "Collapsed sidebar" });
      if (width < XL * LARGE_FONT) {
        await expect(menu, `${at}: the compact bar's Menu`).toBeInViewport({ ratio: 1 });
        await expect(sidebar, `${at}: the sidebar beside the compact bar`).toBeHidden();
        await expect(rail, `${at}: the rail beside the compact bar`).toBeHidden();
        const bar = await edgesOf(page.locator("header").filter({ has: menu }), "the compact bar");
        expect(overlap(bar, await edgesOf(main, "main")), `${at}: the compact bar over main`).toBe(
          false
        );
        expect(
          overlap(bar, await edgesOf(firstRow, "the page's first row")),
          `${at}: the compact bar over the page's first row`
        ).toBe(false);
        await menu.click();
        const drawer = page.getByRole("dialog", { name: "Navigation" });
        await expect(
          drawer
            .getByRole("navigation", { name: "Navigation" })
            .getByRole("link", { name: "Inbox" }),
          `${at}: the drawer's navigation`
        ).toBeInViewport();
        // The drawer is a modal, so the page behind it is out of the accessibility tree while it
        // stands: close it, as a reader does, so the next width reads the page and not the drawer.
        await page.keyboard.press("Escape");
        await expect(drawer, `${at}: the drawer after Escape`).toBeHidden();
      } else {
        await expect(sidebar, `${at}: the sidebar`).toBeVisible();
        await expect(menu, `${at}: the compact bar's Menu beside the sidebar`).toBeHidden();
        await expect(
          sidebar
            .getByRole("navigation", { name: "Navigation" })
            .getByRole("link", { name: "Inbox" }),
          `${at}: the sidebar's navigation`
        ).toBeInViewport();
        const side = await edgesOf(sidebar, "the sidebar");
        expect(overlap(side, await edgesOf(main, "main")), `${at}: the sidebar over main`).toBe(
          false
        );
        expect(
          overlap(side, await edgesOf(firstRow, "the page's first row")),
          `${at}: the sidebar over the page's first row`
        ).toBe(false);
      }
    }
  } finally {
    await context.close();
  }
});
