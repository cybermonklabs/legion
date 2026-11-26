/** The shared search, sort and filter controls as the eight stubbable pages of INFRA-4948's R8
 *  (six of them INFRA-3135's R2) draw them: one row of controls per page, nothing else between a
 *  page's title and its first row but the Inbox's header line, nothing a reader presses moving
 *  that first row, exactly the controls the design gives each page, one look across two pages of
 *  a size family, each control at the size its page passes, the Inbox's header line one line, and
 *  the Inbox still a list of rows with a reading pane beside it.
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

import {
  CONTROL,
  type ControlName,
  type ControlSize,
  NARROW_BELOW,
} from "../web/src/components/control-names";
import { control, controlRow, openFilterPanel } from "./filters";
import { nextFrames } from "./frames";
import { COMPACT_VIEWPORT_WIDTH } from "./inbox-threads";
import { stubbed } from "./stub/stub-test";
import {
  closeControlPanel,
  drawnControlNames,
  expectBarDesign,
  expectNoGrowth,
  expectOneBarOn,
  gapBoxes,
  HEADER_LINE,
  heightOf,
  INBOX_PICKS,
  INBOXES,
  isPhoneProject,
  narrowedAddress,
  openSurface,
  type PlaceholderRoom,
  pickInboxField,
  placeholderRoom,
  REMOVE_CHIP,
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
import { assertNoSidewaysScroll, assertPageFits } from "./unclipped";

/** One project of six issues, each holding an open ask and a label, with three artifacts on the
 *  first, and one priority with one issue in flight under it: every surface below has a list
 *  with a first row and a narrowing that keeps it. */
const test = stubbed(seedSurfaces);

/** The share of the two columns the Inbox's reading pane takes, which is
 *  `DEFAULT_PANE_WIDTH` in `web/src/features/inbox/PaneDivider.tsx` - a file this change does
 *  not touch, so it is the share the pane has at the commit before it. */
const PANE_SHARE = 0.6;

/** One part of a header line as the row reads it: what it is, and its box. */
interface HeaderLinePart {
  readonly what: string;
  readonly height: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
}

/** The header line `root` draws, read: its parts in their order and its own height, in one pass,
 *  and its search box's room for its placeholder (`placeholderRoom`). A heading is named by its
 *  tag, so the page's `h1` reads as one. */
async function headerLineOf(root: Locator): Promise<
  PlaceholderRoom & {
    readonly height: number;
    readonly parts: readonly HeaderLinePart[];
  }
> {
  const line = root.locator(HEADER_LINE);
  await expect(line, "the surface draws no one header line").toHaveCount(1);
  const drawn = await line.evaluate((node) => {
    const words = (element: Element) => (element.textContent ?? "").trim();
    const parts = [...node.children].map((child) => {
      const box = child.getBoundingClientRect();
      const field = child.matches('[data-control="search-box"]')
        ? child
        : child.querySelector('[data-control="search-box"]');
      const tag = child.tagName.toLowerCase();
      const what =
        field !== null
          ? "search box"
          : /^h[1-6]$/.test(tag)
            ? `${tag} "${words(child)}"`
            : `${tag === "a" ? "link" : tag} "${words(child)}"`;
      return { height: box.height, left: box.left, right: box.right, top: box.top, what };
    });
    return { height: node.getBoundingClientRect().height, parts };
  });
  return { ...drawn, ...(await placeholderRoom(line)) };
}

/** `k-one-row`'s reading of a header line at `where`: the parts `expected` in that order from its
 *  start, each after the one before it, every part's top within 1 px of the others', the line as
 *  tall as its tallest part within 1 px, so it is one line, and room in its search box for the
 *  whole placeholder. */
async function expectOneHeaderLine(
  root: Locator,
  where: string,
  expected: readonly string[]
): Promise<void> {
  // Every caller waited for its surface's first row before this, so the header line is drawn,
  // and the expect below fails unless it holds the parts `expected`, which the loop then walks.
  const line = await headerLineOf(root);
  const said = `${where}: the header line, ${Math.round(line.height)}px tall, holds ${line.parts
    .map(
      (part) =>
        `${part.what} at ${Math.round(part.left)}-${Math.round(part.right)}px, its top at ${Math.round(part.top)}px, ${Math.round(part.height)}px tall`
    )
    .join("; ")}`;
  expect(
    line.parts.map((part) => part.what),
    `${said}, not ${expected.join(", ")}`
  ).toEqual(expected);
  for (const [index, part] of line.parts.entries()) {
    const before = line.parts[index - 1];
    if (before === undefined) continue;
    expect(part.left, `${said}: ${part.what} is not after ${before.what}`).toBeGreaterThanOrEqual(
      before.right - 0.5
    );
  }
  const tops = line.parts.map((part) => part.top);
  expect(
    Math.max(...tops) - Math.min(...tops),
    `${said}: its parts do not stand in one line`
  ).toBeLessThanOrEqual(1);
  const tallest = Math.max(...line.parts.map((part) => part.height));
  expect(
    Math.abs(line.height - tallest),
    `${said}: the line is not the height of its tallest part, ${Math.round(tallest)}px`
  ).toBeLessThanOrEqual(1);
  expect(
    line.placeholderWidth,
    `${where}: the search box has ${Math.round(line.room)}px for its placeholder "${line.placeholder}", which takes ${Math.round(line.placeholderWidth)}px in the box's font`
  ).toBeLessThanOrEqual(line.room);
}

/** The page's header line: its `h1`, the search box, and the link to the answers page. */
const PAGE_HEADER_LINE = ['h1 "Inbox"', "search box", 'link "Answered by you"'] as const;

/** The drawer's header line: its title, the search box, and its close button. */
const DRAWER_HEADER_LINE = ['h2 "Inbox"', "search box", 'button "Close"'] as const;

/** The height each size draws a control at from `md` up and at 1280 px and over, where
 *  `web/src/styles.css` stops raising every button to 44 px (`@media (max-width: 1279px)`): the
 *  three recipes of `controlPadding` in `web/src/components/control-names.ts`, whose `min-h-11`
 *  is 44 px, whose compact `md:min-h-8` is 32 px and whose toolbar `xl:h-8.5` is 34 px, at
 *  Tailwind's 4 px a step. A column filter's funnel takes the compact two (`glyphTarget`, in the
 *  same file). */
const CONTROL_HEIGHT: Record<ControlSize, number> = { comfortable: 44, compact: 32, toolbar: 34 };

/** The padding of the selection bar's own box at each size - top, right, bottom and left - as
 *  `BAR_BOX` in `web/src/components/SelectionBar.tsx` draws it: `px-4 py-2`, and `px-2 py-1` at
 *  the compact and toolbar sizes. */
const BAR_PADDING: Record<ControlSize, string> = {
  comfortable: "8px 16px 8px 16px",
  compact: "4px 8px 4px 8px",
  toolbar: "4px 8px 4px 8px",
};

// SLOW(>=2s): eight pages opened at each of this project's widths, which is sixteen page loads.
test("each page draws one row of controls, and nothing else between its title and its first row @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  // Eight pages read at every width of the project: six were measured at 26-33 s a run in an
  // issue pod, over half the project's 60 s, so it states its own budget as its sibling row below
  // does.
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
      // The Inbox and its drawer draw their search box in the header line, beside the title (R8),
      // and the header line and the control row are the two places between the title and the
      // first row that may draw a search box, a panel trigger or a chip. So there the line's box
      // is the page's search box, and the strays are read from under the line.
      const headed = INBOXES.includes(name);
      const line = surface.root.locator(HEADER_LINE);
      if (headed) {
        await expect(line, `${where}: not exactly one header line`).toHaveCount(1);
        await expect(
          control(line, CONTROL.searchBox),
          `${where}: the header line holds no search box`
        ).toHaveCount(1);
      }
      const strays = await straysBetween(
        surface.root,
        headed
          ? await line.evaluate((node) => node.getBoundingClientRect().bottom)
          : await topOf(surface.head),
        await topOf(surface.firstRow)
      );
      expect(
        strays,
        `${where} (control row ${height}px): ${strays.join("; ")} is drawn between the title and the first row and is outside the control row${headed ? " and the header line" : ""}`
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

// SLOW(>=2s): the same eight pages, each driven through four acts at each of this project's widths.
test("no narrowing, panel, display menu or selection moves the first row of a list @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  // Eight pages, four acts each, read at every width of the project: six were measured at 33-47 s
  // a run in an issue pod, and once over the project's 60 s when three files shared the pod's
  // browser, so the row states a budget of its own rather than borrowing what a one-page row
  // needs.
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
        Math.abs(narrowed - first),
        `${where}: one narrowing moved the first row from ${Math.round(first)}px to ${Math.round(narrowed)}px, while the control row went from ${rowFirst}px to ${rowNarrowed}px; between the row and the first row, before: ${gapFirst.join("; ") || "nothing"}; after: ${gapNarrowed.join("; ") || "nothing"}`
      ).toBeLessThanOrEqual(1);

      // Each of the remaining acts draws over the page rather than in it, so each leaves the
      // top exactly where the narrowing left it, which is where it was: the bar never grows (R8).
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
        ).toBeLessThanOrEqual(1);
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

// SLOW(>=2s): eight pages, each opened, narrowed and - where it allows one - selected.
test("each page draws every shared control its design gives it, and no other @pr", async ({
  page,
  scene,
}) => {
  for (const name of STUBBABLE_SURFACES) {
    const surface = await openSurface(page, name, scene, {
      searchTerm: SURFACE_SEARCH_TERM,
    });
    // An issue's Artifacts tab draws its two actions as `bar-action`s where its bar is 640 px wide
    // or wider and as items of **More** below (R8), so the one of the two its bar's width does not
    // give it is not looked for. The Inbox's bar below 640 px, the drawer's at every window this
    // row drives, holds its display menu among **More**'s items, which **More** opens over the
    // page rather than in the surface, so there **View** is read as an item of **More** instead.
    // Below 640 px of a bar with a trigger its chips stand at the top of the Filter panel, which
    // also opens over the page, so there the chip is read in the panel.
    if (name === "Artifacts tab" && (page.viewportSize()?.width ?? 0) >= COMPACT_VIEWPORT_WIDTH) {
      // The issue page can paint artifact rows before its desktop review margin takes width.
      await expect(
        page.getByTestId("desktop-margin-shell"),
        "the issue's review margin"
      ).toBeVisible();
    }
    const readLayout = async () => {
      // Let the bar's ResizeObserver apply the layout for its new width after each UI change.
      await nextFrames(page);
      const width = await surface.row.evaluate((node) => node.getBoundingClientRect().width);
      const narrow = width < NARROW_BELOW;
      const viewInMore = INBOXES.includes(name) && narrow;
      const chipsInPanel = narrow && SURFACE_CONTROLS[name].includes(CONTROL.panelTrigger);
      const notHere = narrow ? CONTROL.barAction : CONTROL.moreMenu;
      const expected = [...SURFACE_CONTROLS[name]]
        .filter(
          (wanted) => (name !== "Artifacts tab" && name !== "priorities page") || wanted !== notHere
        )
        .filter((wanted) => !viewInMore || wanted !== CONTROL.displayMenu)
        .filter((wanted) => !chipsInPanel || wanted !== CONTROL.chip)
        .sort();
      return { chipsInPanel, expected, viewInMore, width };
    };
    const opened = await readLayout();
    if (opened.viewInMore) {
      await control(surface.row, CONTROL.moreMenu).click();
      await expect(
        page
          .getByRole("menu", { name: "More" })
          .getByRole("menuitem", { exact: true, name: "View" }),
        `${name}: More holds no View`
      ).toBeVisible();
      await page.keyboard.press("Escape");
    }
    // Every control the page draws as it opens, read before anything is pressed, so a page
    // pointed at a copy of a control is named here rather than found missing by the act that
    // would have driven it. The chip and the selection bar are not among them: a chip is drawn
    // only under a narrowing and a selection bar only under a selection.
    const resting = await readLayout();
    for (const wanted of resting.expected) {
      if (wanted === CONTROL.chip || wanted === CONTROL.selectionBar) continue;
      await expect(
        control(surface.root, wanted),
        `${name} (${resting.width}px bar) draws no ${wanted}`
      ).not.toHaveCount(0);
    }
    await surface.narrow();
    if ((await readLayout()).chipsInPanel) {
      const panel = await openFilterPanel(page, surface.root);
      await expect(
        control(panel, CONTROL.chip),
        `${name}: its Filter panel lists no chip`
      ).not.toHaveCount(0);
      await closeControlPanel(surface.root);
    }
    if (surface.select !== undefined) await surface.select();
    const final = await readLayout();
    for (const wanted of final.expected) {
      await expect(
        control(surface.root, wanted),
        `${name} (${final.width}px bar) draws no ${wanted}`
      ).not.toHaveCount(0);
    }
    const drawn = await drawnControlNames(surface.root);
    const unexpected = drawn
      .filter((value) => !final.expected.includes(value as ControlName))
      .sort();
    expect(
      unexpected,
      `${name} draws ${unexpected.join(", ")}, which the design does not give it`
    ).toEqual([]);
  }
});

// R8 draws **Filter** in the bar's own look (`BarLook`), so the one trigger is read across two
// pages on the bar's slots, the Board and the List, by everything their two sizes share - its
// fill, its border, its type and its corners - and the Inbox, on the slots too, shares its search
// box and its chip with the Board.
test("one search box and chip across the Inbox and the Board, one bar trigger across the Board and the List, and the List's compact row behaving alike @pr", async ({
  page,
  scene,
}) => {
  const drawnStyle = async (scope: Locator, which: ControlName) =>
    control(scope, which)
      .first()
      .evaluate(async (node) => {
        // A narrowing put on tints the trigger through a CSS transition, so the style is read
        // once every transition of the control has ended, and never a colour between two.
        const running = () =>
          node.getAnimations().filter((animation) => animation.playState === "running");
        for (let now = running(); now.length > 0; now = running()) {
          await Promise.allSettled(now.map((animation) => animation.finished));
        }
        const style = getComputedStyle(node);
        return {
          background: style.backgroundColor,
          border: `${style.borderTopWidth} ${style.borderColor}`,
          font: `${style.fontFamily} ${style.fontSize} ${style.fontWeight}`,
          height: Math.round(node.getBoundingClientRect().height),
          // The leading corner: a trigger with Clear filters beside it in its group squares its
          // trailing ones, and the Board, which has no Clear filters, keeps all four round.
          radius: style.borderTopLeftRadius,
        };
      });
  const look = async (name: "Inbox" | "project Board" | "project List") => {
    const surface = await openSurface(page, name, scene);
    await surface.narrow();
    const row = controlRow(surface.root);
    return {
      chip: await drawnStyle(row, CONTROL.chip),
      // The Inbox's search box stands in its header line, the Board's in its control row (R8).
      search: await drawnStyle(
        name === "Inbox" ? surface.root.locator(HEADER_LINE) : row,
        CONTROL.searchBox
      ),
      trigger: await drawnStyle(row, CONTROL.panelTrigger),
    };
  };
  await page.setViewportSize({ height: 900, width: 1440 });
  const inbox = await look("Inbox");
  const board = await look("project Board");
  expect(board.search, "the Board's search box is not the Inbox's").toEqual(inbox.search);
  expect(board.chip, "the Board's chip is not the Inbox's").toEqual(inbox.chip);

  // The List's bar draws the toolbar size, so it is read for behaviour and for the shape of each
  // accessible name rather than for size, and its trigger for all but its height.
  const listed = await look("project List");
  const { height: _boardHeight, ...boardTrigger } = board.trigger;
  const { height: _listHeight, ...listTrigger } = listed.trigger;
  expect(listTrigger, "the List's bar trigger is not the Board's").toEqual(boardTrigger);
  const row = controlRow(page.getByTestId("main-content"));
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
    REMOVE_CHIP
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
  // The list is the grid's first column, the divider its second and the pane its third. The
  // list's own box reaches 20 px left of its column into the page's inset, so that its rows'
  // checkboxes are not clipped (`Inbox.tsx`): the list's width is the column's, the grid's own
  // first track.
  const share = await pane.evaluate((node) => {
    const columns = node.closest<HTMLElement>("div.grid");
    const tracks = columns === null ? [] : getComputedStyle(columns).gridTemplateColumns.split(" ");
    const paneWidth = node.getBoundingClientRect().width;
    const listWidth = Number.parseFloat(tracks[0] ?? "0");
    return { listWidth, paneWidth, share: paneWidth / (paneWidth + listWidth) };
  });
  expect(
    share.share,
    `the reading pane takes ${share.paneWidth}px beside a ${share.listWidth}px list, a share of ${share.share.toFixed(3)} rather than the ${PANE_SHARE} it has at the commit before this change`
  ).toBeCloseTo(PANE_SHARE, 2);
  await expect(surface.row, "the control row went with the reading pane").toHaveCount(1);
});

// SLOW(>=2s): the Inbox opened twice at each of this project's widths.
test("k-one-row: the Inbox's header line is one line, its search box after the h1 and before Answered by you @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    const surface = await openSurface(page, "Inbox", scene, { phone });
    const where = `the Inbox at ${size.width}x${size.height}`;
    await expectOneHeaderLine(surface.root, `${where}, no narrowing on`, PAGE_HEADER_LINE);
    await page.goto(narrowedAddress("Inbox", scene, 6));
    await expect(surface.firstRow).toBeVisible();
    await expectOneHeaderLine(surface.root, `${where}, six narrowings on`, PAGE_HEADER_LINE);
  }
});

/**
 * `k-one-row`'s rule that the page does not scroll sideways, in the drawer: neither the drawer nor
 * its control bar holds more than its own width. The drawer is 448 px wide at 1440x900
 * (`md:max-w-md`), and a bar kept to one line by the window's width ran 34 px past the drawer's
 * edge there, so the drawer scrolled sideways (builds Job infra-3135-probe4-k88vm at 968e2b90).
 */
async function expectDrawerNotSideways(drawer: Locator, where: string): Promise<void> {
  await assertNoSidewaysScroll(drawer, `${where}: the drawer`);
  await assertNoSidewaysScroll(controlRow(drawer), `${where}: the drawer's control bar`);
}

// SLOW(>=2s): the drawer opened, and six narrowings picked in it, at each of this project's widths.
test("k-one-row: the drawer's header line is one line, its search box after its title and before its close button @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  // Six picks of the Filter panel at each width, each opening the panel and a field's popover
  // and shutting both, so the row states a budget of its own as its siblings above do.
  test.setTimeout(120_000);
  const phone = isPhoneProject(testInfo);
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    const surface = await openSurface(page, "Inbox drawer", scene, { phone });
    const where = `the Inbox drawer at ${size.width}x${size.height}`;
    await expectOneHeaderLine(surface.root, `${where}, no narrowing on`, DRAWER_HEADER_LINE);
    await expectDrawerNotSideways(surface.root, `${where}, no narrowing on`);
    for (const [field, option] of INBOX_PICKS) {
      await pickInboxField(page, field, option, surface.root);
    }
    await expect(surface.firstRow).toBeVisible();
    await expectOneHeaderLine(surface.root, `${where}, six narrowings on`, DRAWER_HEADER_LINE);
    await expectDrawerNotSideways(surface.root, `${where}, six narrowings on`);
  }
});

// SLOW(>=2s): eight pages at each of this project's widths, each read with no narrowing and with six.
test("k-one-row: each page on the bar's slots draws one bar of one line above its list or table, its search box first or in the Inbox's header line, its placeholder whole @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  test.setTimeout(320_000);
  const phone = isPhoneProject(testInfo);
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of STUBBABLE_SURFACES) await expectOneBarOn(page, name, scene, size, phone);
  }
});

// SLOW(>=2s): eight pages, each read at rest and under one, two, three or six narrowings, and the
// priorities page's three issue-list bars read at rest and with their chips.
test("k-bar: each page on the bar's slots draws its design's controls and no other, quiet at rest, with its trigger's count, its chips and its panel's list @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  test.setTimeout(420_000);
  const phone = isPhoneProject(testInfo);
  const size = phone
    ? (page.viewportSize() ?? { height: 664, width: 390 })
    : { height: 800, width: 1280 };
  if (!phone) await page.setViewportSize(size);
  for (const name of STUBBABLE_SURFACES) await expectBarDesign(page, name, scene, size, phone);
});

// SLOW(>=2s): eight pages at each of this project's widths, each driven through four acts.
test("k-no-growth: no narrowing, panel, display menu or selection moves the first row of a page on the bar's slots, in the layout its bar's width gives @phone @pr", async ({
  page,
  scene,
}, testInfo) => {
  test.setTimeout(420_000);
  const phone = isPhoneProject(testInfo);
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of STUBBABLE_SURFACES) await expectNoGrowth(page, name, scene, size, phone);
  }
});
