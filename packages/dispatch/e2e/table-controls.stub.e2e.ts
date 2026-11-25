/** The shared search, sort and filter controls as the six stubbable pages of INFRA-3135's R2
 *  draw them: one row of controls per page, nothing else between a page's title and its first
 *  row, nothing a reader presses moving that first row, exactly the controls the design gives
 *  each page, one look across two pages of a size family, each control at the size its page
 *  passes, and the Inbox still a list of rows with a reading pane beside it.
 *
 *  Every row reads what the page draws over one fixed answer - a box's height, a computed
 *  colour, the `data-control` values in the document - so each runs against a recording rather
 *  than against a harness (packages/dispatch/README.md, "The stubbed layer"). The Agents page
 *  and the Delivery page are read in `table-controls.e2e.ts` instead: a recording freezes a
 *  session's last-seen time and the delivery window's dates, so neither has a first row to
 *  measure to ten minutes after a re-recording. Everything that needs seeded counts, a write or
 *  a reload is in that file too.
 */
import { expect, type Locator } from "@playwright/test";

import { CONTROL, type ControlName, type ControlSize } from "../web/src/components/control-names";
import { control, controlRow } from "./filters";
import { stubbed } from "./stub/stub-test";
import {
  drawnControlNames,
  gapBoxes,
  heightOf,
  isPhoneProject,
  openSurface,
  resetScroll,
  STUBBABLE_SURFACES,
  SURFACE_CONTROLS,
  SURFACE_SEARCH_TERM,
  type Surface,
  seedSurfaces,
  straysBetween,
  topOf,
  widthsOf,
} from "./table-controls";
import { assertPageFits } from "./unclipped";

/** One project of six issues, each holding an open ask and a label, with three artifacts on the
 *  first: every surface below has a list with a first row and a narrowing that keeps it. */
const test = stubbed(seedSurfaces);

/** How far below its first reading the top of a list's first row may sit once a narrowing is
 *  on: one more line of chips, which is a 44 px control and the row's own 8 px gap
 *  (`components/ControlRow.tsx`). */
const CHIP_LINE = 52;

/** The share of the two columns the Inbox's reading pane takes, which is
 *  `DEFAULT_PANE_WIDTH` in `web/src/features/inbox/PaneDivider.tsx` - a file this change does
 *  not touch, so it is the share the pane has at the commit before it. */
const PANE_SHARE = 0.6;

/** The height each size draws a control at from `md` up and at 1280 px and over, where
 *  `web/src/styles.css` stops raising every button to 44 px (`@media (max-width: 1279px)`): the
 *  three recipes of `controlPadding` in `web/src/components/control-names.ts`, whose `min-h-11`
 *  is 44 px, whose compact `md:min-h-8` is 32 px and whose toolbar `xl:h-8.5` is 34 px, at
 *  Tailwind's 4 px a step. A column filter's funnel takes the compact two (`FILTER_TARGET`,
 *  `web/src/components/ColumnFilter.tsx`). */
const CONTROL_HEIGHT: Record<ControlSize, number> = { comfortable: 44, compact: 32, toolbar: 34 };

/** The padding of the selection bar's own box at each size - top, right, bottom and left - as
 *  `BAR_BOX` in `web/src/components/SelectionBar.tsx` draws it: `px-4 py-2`, and `px-2 py-1` at
 *  the compact and toolbar sizes. */
const BAR_PADDING: Record<ControlSize, string> = {
  comfortable: "8px 16px 8px 16px",
  compact: "4px 8px 4px 8px",
  toolbar: "4px 8px 4px 8px",
};

// SLOW(>=2s): six pages opened at each of this project's widths, which is twelve page loads.
test("each page draws one row of controls, and nothing else between its title and its first row @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  // Six pages read at every width of the project: measured at 26-33 s a run in an issue pod,
  // over half the project's 60 s, so it states its own budget as its sibling row below does.
  test.setTimeout(120_000);
  const phone = isPhoneProject(testInfo);
  const measured: string[] = [];
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of STUBBABLE_SURFACES) {
      const surface = await openSurface(page, name, scene, {
        phone,
        searchTerm: SURFACE_SEARCH_TERM,
      });
      const where = `${name} at ${size.width}x${size.height}`;
      await expect(surface.row, `${where}: not exactly one control row`).toHaveCount(1);
      const height = Math.round(await heightOf(surface.row));
      measured.push(`${where}: control row ${height}px`);
      expect(height, `${where}: the control row has no height`).toBeGreaterThan(0);
      const strays = await straysBetween(
        surface.root,
        await topOf(surface.head),
        await topOf(surface.firstRow)
      );
      expect(
        strays,
        `${where} (control row ${height}px): ${strays.join("; ")} is drawn between the title and the first row and is outside the control row`
      ).toEqual([]);
      // At the phone's own width the row wraps inside itself rather than widening the page.
      if (phone) await assertPageFits(page);
    }
  }
  await testInfo.attach("control row heights", {
    body: measured.join("\n"),
    contentType: "text/plain",
  });
});

// SLOW(>=2s): the same six pages, each driven through four acts at each of this project's widths.
test("no narrowing, panel, display menu or selection moves the first row of a list @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  // Six pages, four acts each, read at every width of the project: measured at 33-47 s a run in
  // an issue pod, and once over the project's 60 s when three files shared the pod's browser, so
  // the row states a budget of its own rather than borrowing what a one-page row needs.
  test.setTimeout(180_000);
  const phone = isPhoneProject(testInfo);
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of STUBBABLE_SURFACES) {
      const surface = await openSurface(page, name, scene, {
        phone,
        searchTerm: SURFACE_SEARCH_TERM,
      });
      const where = `${name} at ${size.width}x${size.height}`;
      await resetScroll(page, surface.firstRow);
      const first = await topOf(surface.firstRow);
      const rowFirst = Math.round(await heightOf(surface.row));
      const gapFirst = await gapBoxes(surface.root, (await topOf(surface.row)) + rowFirst, first);

      await surface.narrow();
      await expect(surface.firstRow).toBeVisible();
      await resetScroll(page, surface.firstRow);
      const narrowed = await topOf(surface.firstRow);
      const rowNarrowed = Math.round(await heightOf(surface.row));
      const gapNarrowed = await gapBoxes(
        surface.root,
        (await topOf(surface.row)) + rowNarrowed,
        narrowed
      );
      expect(
        narrowed - first,
        `${where}: one narrowing moved the first row from ${Math.round(first)}px to ${Math.round(narrowed)}px, while the control row went from ${rowFirst}px to ${rowNarrowed}px; between the row and the first row, before: ${gapFirst.join("; ") || "nothing"}; after: ${gapNarrowed.join("; ") || "nothing"}`
      ).toBeLessThanOrEqual(CHIP_LINE);
      expect(
        narrowed,
        `${where}: one narrowing moved the first row up, from ${Math.round(first)}px to ${Math.round(narrowed)}px`
      ).toBeGreaterThanOrEqual(first - 0.5);

      // Each of the remaining acts draws over the page rather than in it, so each leaves the
      // top exactly where the narrowing left it, and within the same one line of the first
      // reading.
      for (const [act, run] of [
        ["the filter panel opened", surface.panel],
        ["the display menu opened", surface.display],
        ["a row selected", surface.select],
      ] as const) {
        if (run === undefined) continue;
        await run();
        await resetScroll(page, surface.firstRow);
        const after = await topOf(surface.firstRow);
        expect(
          after,
          `${where}: ${act} moved the first row from ${Math.round(narrowed)}px to ${Math.round(after)}px`
        ).toBeCloseTo(narrowed, 0);
        expect(
          Math.abs(after - first),
          `${where}: ${act} left the first row at ${Math.round(after)}px against a first reading of ${Math.round(first)}px`
        ).toBeLessThanOrEqual(CHIP_LINE);
        // Shut as a reader shuts it: the panel holds the focus once it opens, and Escape from
        // inside it closes it. Left open, it covers the display menu's trigger on a phone, and
        // the next act's press lands on the panel until one of its retries scrolls it away.
        if (run === surface.panel) {
          await page.keyboard.press("Escape");
          await expect(
            control(controlRow(surface.root), CONTROL.panelTrigger),
            `${where}: Escape left the filter panel open`
          ).toHaveAttribute("aria-expanded", "false");
        }
      }
    }
  }
});

// SLOW(>=2s): six pages, each opened, narrowed and - where it allows one - selected.
test("each page draws every shared control its design gives it, and no other @pr", async ({
  page,
  scene,
}) => {
  for (const name of STUBBABLE_SURFACES) {
    const surface = await openSurface(page, name, scene, {
      searchTerm: SURFACE_SEARCH_TERM,
    });
    const expected = [...SURFACE_CONTROLS[name]].sort();
    // Every control the page draws as it opens, read before anything is pressed, so a page
    // pointed at a copy of a control is named here rather than found missing by the act that
    // would have driven it. The chip and the selection bar are not among them: a chip is drawn
    // only under a narrowing and a selection bar only under a selection.
    for (const wanted of expected) {
      if (wanted === CONTROL.chip || wanted === CONTROL.selectionBar) continue;
      await expect(control(surface.root, wanted), `${name} draws no ${wanted}`).not.toHaveCount(0);
    }
    await surface.narrow();
    if (surface.select !== undefined) await surface.select();
    for (const wanted of expected) {
      await expect(control(surface.root, wanted), `${name} draws no ${wanted}`).not.toHaveCount(0);
    }
    const drawn = await drawnControlNames(surface.root);
    const unexpected = drawn.filter((value) => !expected.includes(value as ControlName)).sort();
    expect(
      unexpected,
      `${name} draws ${unexpected.join(", ")}, which the design does not give it`
    ).toEqual([]);
  }
});

test("one search box, panel trigger and chip across the Inbox and the Board, and the List's compact row behaving alike @pr", async ({
  page,
  scene,
}) => {
  const look = async (name: "Inbox" | "project Board") => {
    const surface = await openSurface(page, name, scene);
    await surface.narrow();
    const drawnStyle = async (which: ControlName) =>
      control(controlRow(surface.root), which)
        .first()
        .evaluate((node) => {
          const style = getComputedStyle(node);
          return {
            background: style.backgroundColor,
            border: style.borderColor,
            font: `${style.fontFamily} ${style.fontSize} ${style.fontWeight}`,
            height: Math.round(node.getBoundingClientRect().height),
            radius: style.borderRadius,
          };
        });
    return {
      chip: await drawnStyle(CONTROL.chip),
      search: await drawnStyle(CONTROL.searchBox),
      trigger: await drawnStyle(CONTROL.panelTrigger),
    };
  };
  await page.setViewportSize({ height: 900, width: 1440 });
  const inbox = await look("Inbox");
  const board = await look("project Board");
  expect(board.search, "the Board's search box is not the Inbox's").toEqual(inbox.search);
  expect(board.trigger, "the Board's panel trigger is not the Inbox's").toEqual(inbox.trigger);
  expect(board.chip, "the Board's chip is not the Inbox's").toEqual(inbox.chip);

  // The List draws the compact size, so it is read for behaviour and for the shape of each
  // accessible name rather than for size.
  const list = await openSurface(page, "project List", scene);
  await list.narrow();
  const row = controlRow(list.root);
  await expect(
    control(row, CONTROL.searchBox),
    "the List's search box is no searchbox with a name of its own"
  ).toHaveAttribute("aria-label", /^Search /);
  const trigger = control(row, CONTROL.panelTrigger).first();
  await expect(trigger, "the List's panel trigger opens no panel").toHaveAttribute(
    "aria-haspopup",
    "dialog"
  );
  await expect(trigger).toHaveAttribute("aria-expanded", "false");
  const chip = control(row, CONTROL.chip).first();
  await expect(chip, "the List's chip is not named as the Board's is").toHaveAttribute(
    "aria-label",
    /^Remove .+ filter$/
  );
  // And it behaves as the Board's does: the press takes that narrowing off and the chip goes.
  const chips = await control(row, CONTROL.chip).count();
  await chip.click();
  await expect(control(row, CONTROL.chip)).toHaveCount(chips - 1);
});

test("each page's pager, selection bar and column filters take the size the page passes: compact on the List, comfortable on the Inbox @pr", async ({
  page,
  scene,
}) => {
  // At 1440 px the two sizes draw apart (`CONTROL_HEIGHT`); below 1280 px every button is 44 px.
  await page.setViewportSize({ height: 900, width: 1440 });
  /** Reads that every element `elements` finds, and it finds one at least, is `size` tall. */
  const expectSized = async (what: string, elements: Locator, size: ControlSize) => {
    await expect(elements.first(), `${what}: none is drawn`).toBeVisible();
    const drawn = await elements.evaluateAll((nodes) =>
      nodes.map((node) => ({
        height: Math.round(node.getBoundingClientRect().height),
        name: (node.getAttribute("aria-label") ?? node.textContent ?? "").trim(),
      }))
    );
    const off = drawn
      .filter(({ height }) => height !== CONTROL_HEIGHT[size])
      .map(({ height, name }) => `"${name}" is ${height}px`);
    expect(
      off,
      `${what}: ${off.join(", ")}, where the ${size} size is ${CONTROL_HEIGHT[size]}px`
    ).toEqual([]);
  };
  /** Marks the surface's first row and hands back the selection bar that draws. */
  const markOne = async (surface: Surface) => {
    if (surface.select === undefined) throw new Error(`${surface.name} offers no selection`);
    await surface.select();
    return control(surface.root, CONTROL.selectionBar);
  };
  /** The padding of a box, top, right, bottom and left. */
  const paddingOf = (box: Locator) =>
    box.evaluate((node) => {
      const style = getComputedStyle(node);
      return `${style.paddingTop} ${style.paddingRight} ${style.paddingBottom} ${style.paddingLeft}`;
    });

  // The List passes `compact` to its table, which hands it on to the Pager and to each column's
  // filter (`IssuesTable.tsx`, `DataTable.tsx`), and to its selection bar and every action in
  // it (`IssueTableSelection`, `issues-table-page.tsx`).
  const list = await openSurface(page, "project List", scene);
  await expectSized(
    "the List's pager",
    control(list.root, CONTROL.pager).getByRole("button"),
    "compact"
  );
  await expectSized(
    "the List's column filters",
    control(list.root, CONTROL.columnFilter),
    "compact"
  );
  const listBar = await markOne(list);
  await expectSized("the List's selection bar actions", listBar.getByRole("button"), "compact");
  expect(await paddingOf(listBar), "the List's selection bar box is not the compact size's").toBe(
    BAR_PADDING.compact
  );

  // The Inbox passes no size, so its selection bar is the comfortable one, and so is each action
  // the page puts in it (`BAR_CONTROL_CLASS`, `Inbox.tsx`). The bar's Snooze pick keeps the look
  // of every row's Snooze badge and is no such action; Dismiss is drawn for a marked thread
  // alone, and every seeded row is an ask.
  const inbox = await openSurface(page, "Inbox", scene);
  for (const absent of [CONTROL.pager, CONTROL.columnFilter]) {
    await expect(control(inbox.root, absent), `the Inbox draws a ${absent}`).toHaveCount(0);
  }
  const inboxBar = await markOne(inbox);
  const actions = inboxBar
    .getByRole("button", { exact: true, name: "Select all" })
    .or(inboxBar.getByRole("button", { exact: true, name: "Clear selection" }));
  await expect(
    actions,
    "the Inbox's selection bar lacks Select all or Clear selection"
  ).toHaveCount(2);
  await expectSized("the Inbox's selection bar actions", actions, "comfortable");
  expect(
    await paddingOf(inboxBar),
    "the Inbox's selection bar box is not the comfortable size's"
  ).toBe(BAR_PADDING.comfortable);
});

test("the Inbox is rows with their marks and no table, and opening one opens the reading pane at its own width @pr", async ({
  page,
  scene,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  const surface = await openSurface(page, "Inbox", scene);
  const rows = page.locator("[data-inbox-row]");
  await expect(rows, "the Inbox lists no rows").not.toHaveCount(0);
  expect(
    await page.locator("table").count(),
    "the Inbox draws a table, so its items are a grid of columns rather than rows"
  ).toBe(0);
  // Each item is one row element, carrying its own marks rather than a cell per field.
  const marked = await rows.evaluateAll((nodes) =>
    nodes.map((node) => node.querySelectorAll("td").length)
  );
  expect(marked, "an Inbox row holds table cells").toEqual(marked.map(() => 0));

  // One click on the row's age selects it, which is the gesture `e2e/inbox-pane.ts` uses: it
  // is the one part of a short row that is neither the owner link nor a control.
  await rows.first().locator("time").first().click();
  const pane = page.getByTestId("inbox-pane");
  await expect(pane, "opening an item opened no reading pane").toBeVisible();
  const share = await pane.evaluate((node) => {
    const columns = node.closest<HTMLElement>("div.grid");
    const list = columns?.firstElementChild as HTMLElement | null;
    const paneWidth = node.getBoundingClientRect().width;
    const listWidth = list?.getBoundingClientRect().width ?? 0;
    return { listWidth, paneWidth, share: paneWidth / (paneWidth + listWidth) };
  });
  expect(
    share.share,
    `the reading pane takes ${share.paneWidth}px beside a ${share.listWidth}px list, a share of ${share.share.toFixed(3)} rather than the ${PANE_SHARE} it has at the commit before this change`
  ).toBeCloseTo(PANE_SHARE, 2);
  await expect(surface.row, "the control row went with the reading pane").toHaveCount(1);
});
