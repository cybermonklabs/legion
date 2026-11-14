/** The shared search, sort and filter controls where proving them needs a server: seeded counts,
 *  writes, reloads, the keyboard, and the two pages a recording cannot answer - the Agents page,
 *  whose groups are a session's last-seen time against the clock, and the Delivery page, whose
 *  window is a range of dates. The rows that read only what a page draws over one fixed answer
 *  are in `table-controls.stub.e2e.ts`.
 */

import { IDEMPOTENCY_KEY_COMMITTED } from "@legion/contracts";
import type { Locator, Route } from "@playwright/test";
import { CONTROL } from "../web/src/components/control-names";
import { plannerSession, reviewerSession, seedAgents, setLiveSessions } from "./agents";
import {
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  mintAgentToken,
  patchIssue,
  putArchitectureSource,
  putRepoProject,
  replyToMessageDelivery,
} from "./api";
import {
  PIPELINE_AT,
  seedDeliveryFixture,
  seedPipelineFixture,
  seedSessionGroupsFixture,
} from "./delivery-seed";
import {
  setCredentialGrants,
  setPendingCredentialRequests,
  setSignIns,
} from "./fake-broker-helpers";
import { seedFakeGithub } from "./fake-github-helpers";
import { control, controlRow, openFilterPanel } from "./filters";
import { drawer, peekTrigger } from "./inbox-drawer";
import { openFilterFields } from "./inbox-filter";
import { askControls, inboxRow, selectRow } from "./inbox-pane";
import { replyRowId, seedCommentThread } from "./inbox-threads";
import {
  bandHeadings,
  bandLabel,
  countsOf,
  drawnIds,
  lastActivity,
  seedEveryField,
  sortedIds,
  valuesOf,
} from "./inbox-view";
import { sessionIds } from "./session-ids";
import {
  closeControlPanel,
  drawnControlNames,
  heightOf,
  isPhoneProject,
  openDisplay,
  openSurface,
  pickInboxField,
  REAL_STACK_SURFACES,
  resetScroll,
  SURFACE_CONTROLS,
  SURFACE_SESSION,
  seedSurfaces,
  straysBetween,
  topOf,
  widthsOf,
} from "./table-controls";
import { expect, type Page, test } from "./test";
import { assertPageFits } from "./unclipped";

/** How far below its first reading the top of a list's first row may sit once a narrowing is
 *  on: one more line of chips, a 44 px control and the row's own 8 px gap. */
const CHIP_LINE = 52;

/** The Inbox's own ceiling: the first item starts this far down the window or less, with no
 *  narrowing on, with a panel open and with the display menu open. */
const INBOX_TOP = 203;
/** And this far down with three narrowings on, which is a second line of chips. */
const INBOX_TOP_NARROWED = 255;

/** The window the delivery fixture's two pull requests were merged in, with the list drawn. */
const DELIVERY_WINDOW = "from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

/** The seed the Agents and Delivery surfaces are read over. */
async function seedRealStackSurfaces(): Promise<{
  deliveryQuery: string;
  issue: string;
  project: string;
}> {
  const issue = await seedAgents();
  await seedDeliveryFixture();
  return { deliveryQuery: DELIVERY_WINDOW, issue, project: "CORE" };
}

// SLOW(>=2s): two pages opened at each of this project's widths, over two seeded fixtures.
test("the Agents page and the Delivery page each draw one row of controls, with nothing else between the title and the first row @phone", async ({
  page,
}, testInfo) => {
  const scene = await seedRealStackSurfaces();
  const phone = isPhoneProject(testInfo);
  const measured: string[] = [];
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of REAL_STACK_SURFACES) {
      const surface = await openSurface(page, name, scene, { phone });
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
      if (phone) await assertPageFits(page);
    }
  }
  await testInfo.attach("control row heights", {
    body: measured.join("\n"),
    contentType: "text/plain",
  });
});

// SLOW(>=2s): the same two pages, each narrowed and opened at each of this project's widths.
test("no narrowing or panel on the Agents page or the Delivery page moves the first row @phone", async ({
  page,
}, testInfo) => {
  const scene = await seedRealStackSurfaces();
  const phone = isPhoneProject(testInfo);
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of REAL_STACK_SURFACES) {
      const surface = await openSurface(page, name, scene, { phone });
      const where = `${name} at ${size.width}x${size.height}`;
      await resetScroll(page, surface.firstRow);
      const first = await topOf(surface.firstRow);

      await surface.narrow();
      await expect(surface.firstRow).toBeVisible();
      await resetScroll(page, surface.firstRow);
      const narrowed = await topOf(surface.firstRow);
      expect(
        narrowed - first,
        `${where}: one narrowing moved the first row from ${Math.round(first)}px to ${Math.round(narrowed)}px, which is more than the one line of chips the rule allows`
      ).toBeLessThanOrEqual(CHIP_LINE);
      expect(
        narrowed,
        `${where}: one narrowing moved the first row up, from ${Math.round(first)}px to ${Math.round(narrowed)}px`
      ).toBeGreaterThanOrEqual(first - 0.5);

      // Neither page draws a display menu or allows a selection, so the panel is the one act
      // left: it is portalled over the page and takes no room in the layout.
      if (surface.panel !== undefined) {
        await surface.panel();
        await resetScroll(page, surface.firstRow);
        const after = await topOf(surface.firstRow);
        expect(
          after,
          `${where}: the filter panel opened moved the first row from ${Math.round(narrowed)}px to ${Math.round(after)}px`
        ).toBeCloseTo(narrowed, 0);
        await closeControlPanel(surface.root);
      }
    }
  }
});

test("the Agents page and the Delivery page draw every shared control their design gives them, and no other", async ({
  page,
}) => {
  const scene = await seedRealStackSurfaces();
  await page.setViewportSize({ height: 900, width: 1440 });
  for (const name of REAL_STACK_SURFACES) {
    const surface = await openSurface(page, name, scene);
    // A chip is drawn only under a narrowing; neither page allows a selection.
    await surface.narrow();
    const expected = [...SURFACE_CONTROLS[name]].sort();
    for (const wanted of expected) {
      await expect(control(surface.root, wanted), `${name} draws no ${wanted}`).not.toHaveCount(0);
    }
    const drawn = await drawnControlNames(surface.root);
    const unexpected = drawn.filter((value) => !(expected as string[]).includes(value)).sort();
    expect(
      unexpected,
      `${name} draws ${unexpected.join(", ")}, which the design does not give it`
    ).toEqual([]);
  }
});

/** Twenty-two questions, so the list is longer than either window, with no credential request
 *  waiting and nothing selected. Each ask waits on Alice, who holds every issue. */
async function seedLongInbox(): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const priorities = [0, 1, 2, 3] as const;
  const urgencies = ["blocking", "high", "med", "low"] as const;
  for (let index = 0; index < 22; index++) {
    const issue = await createIssue({ project: "CORE", title: `Long inbox item ${index}` });
    await patchIssue(issue.key, { priority: priorities[index % 4] });
    await createAsk(
      issue.key,
      { question: `Item ${index}: what is the rule?`, urgency: urgencies[index % 4] },
      SURFACE_SESSION
    );
  }
}

// SLOW(>=2s): twenty-two seeded questions, read at two widths through four states each.
test("the Inbox's first item starts within 203 px of the window, and within 255 px under three narrowings", async ({
  page,
}) => {
  await seedLongInbox();
  const firstRow = page.locator("[data-inbox-row]").first();
  const main = page.getByTestId("main-content");
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
  ]) {
    await page.setViewportSize(size);
    await page.goto("/");
    await expect(firstRow).toBeVisible();
    await resetScroll(page, firstRow);
    const plain = await topOf(firstRow);
    expect(
      plain,
      `${size.width}x${size.height}: the first item starts ${plain}px down`
    ).toBeLessThanOrEqual(INBOX_TOP);

    // A panel draws over the page, so it takes no room in the layout.
    await openFilterPanel(page, main);
    await resetScroll(page, firstRow);
    const panelled = await topOf(firstRow);
    expect(
      panelled,
      `${size.width}x${size.height}: with the filter panel open the first item starts ${panelled}px down`
    ).toBeLessThanOrEqual(INBOX_TOP);
    await closeControlPanel(main);

    await openDisplay(page, main);
    await resetScroll(page, firstRow);
    const menued = await topOf(firstRow);
    expect(
      menued,
      `${size.width}x${size.height}: with the display menu open the first item starts ${menued}px down`
    ).toBeLessThanOrEqual(INBOX_TOP);

    // Three narrowings, opened in one address so the chips are the only thing that changed.
    await page.goto("/?priority=P0&urgency=blocking&type=question");
    await expect(firstRow).toBeVisible();
    // Three chips, one per narrowing: **Clear filters** beside them is a button, not a chip.
    await expect(control(controlRow(main), CONTROL.chip)).toHaveCount(3);
    await resetScroll(page, firstRow);
    const narrowed = await topOf(firstRow);
    expect(
      narrowed,
      `${size.width}x${size.height}: with three narrowings on the first item starts ${narrowed}px down`
    ).toBeLessThanOrEqual(INBOX_TOP_NARROWED);
  }
});

/** Nine narrowings over the long Inbox that still keep seventeen of its twenty-two items: three
 *  priorities, the three urgencies the seed pairs with them, two kinds and the one project. */
const NINE_NARROWINGS =
  "/?priority=P0,P1,P2&urgency=blocking,high,med&type=question,approval&project=CORE";

// SLOW(>=2s): twenty-two seeded questions, read at three widths with and without nine narrowings.
test("however many narrowings are on, their chips take at most one more line of the Inbox's row, at every width", async ({
  page,
}) => {
  await seedLongInbox();
  const firstRow = page.locator("[data-inbox-row]").first();
  const main = page.getByTestId("main-content");
  const readings: string[] = [];
  const over: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    await page.goto("/");
    await expect(firstRow).toBeVisible();
    await resetScroll(page, firstRow);
    const plain = await topOf(firstRow);

    await page.goto(NINE_NARROWINGS);
    await expect(firstRow).toBeVisible();
    // One chip per value picked, as the row draws them, so nine chips.
    await expect(control(controlRow(main), CONTROL.chip)).toHaveCount(9);
    await resetScroll(page, firstRow);
    const narrowed = await topOf(firstRow);
    const where = `${size.width}x${size.height}: nine narrowings moved the first item from ${Math.round(plain)}px to ${Math.round(narrowed)}px, ${Math.round(narrowed - plain)}px down`;
    readings.push(where);
    if (narrowed - plain > CHIP_LINE) over.push(where);
  }
  await test.info().attach("first item with and without nine narrowings", {
    body: readings.join("\n"),
    contentType: "text/plain",
  });
  // R1: the chips of the narrowings that are on may take one more line of the row, at most
  // 52 px, on any page and at every width the suite drives - not one more line per few chips.
  expect(
    over,
    `${over.join("; ")}, more than the ${CHIP_LINE}px of one more line of chips`
  ).toEqual([]);
});

/** The seven labels every issue of the Board's seed carries: the Board narrows by every label
 *  it names at once, so each issue holds them all and stays on the Board under all seven. */
const BOARD_LABELS = ["alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"];

// SLOW(>=2s): four seeded issues, read at three widths with and without nine narrowings.
test("however many narrowings are on, their chips take at most one more line of the Board's row, at every width", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  let first = "";
  for (const title of ["Rollout one", "Rollout two", "Rollout three", "Rollout four"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { labels: BOARD_LABELS, status: "todo" });
    await createAsk(issue.key, { question: `${title}: what is the rule?` }, SURFACE_SESSION);
    if (first === "") first = issue.key;
  }
  const main = page.getByTestId("main-content");
  const firstCard = page.locator("[data-board-card]").first();
  // Seven labels, the search text and the needs-you flag: nine chips, and every issue kept.
  const nine = `/projects/CORE/issues?${BOARD_LABELS.map((label) => `label=${label}`).join("&")}&q=Rollout&needs-you=1`;
  const readings: string[] = [];
  const over: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    // The Board is the stored view from here on, so the address below opens on it too.
    await openSurface(page, "project Board", { issue: first, project: "CORE" });
    await resetScroll(page, firstCard);
    const plain = await topOf(firstCard);

    await page.goto(nine);
    await expect(firstCard).toBeVisible();
    await expect(control(controlRow(main), CONTROL.chip)).toHaveCount(9);
    await resetScroll(page, firstCard);
    const narrowed = await topOf(firstCard);
    const where = `${size.width}x${size.height}: nine narrowings moved the Board's first card from ${Math.round(plain)}px to ${Math.round(narrowed)}px, ${Math.round(narrowed - plain)}px down`;
    readings.push(where);
    if (narrowed - plain > CHIP_LINE) over.push(where);
  }
  await test.info().attach("first card with and without nine narrowings", {
    body: readings.join("\n"),
    contentType: "text/plain",
  });
  // R1's bound is on every page of R2, so the Board's chips are held to it as the Inbox's are.
  expect(
    over,
    `${over.join("; ")}, more than the ${CHIP_LINE}px of one more line of chips`
  ).toEqual([]);
});

/** The widths a reader's window takes, from the desktop down: R1's two, a tablet held upright,
 *  the `sm` breakpoint itself (640 px, where the row stops wrapping), and a phone. */
const READING_WIDTHS = [
  { height: 900, width: 1440 },
  { height: 800, width: 1280 },
  { height: 1024, width: 768 },
  { height: 800, width: 640 },
  { height: 664, width: 390 },
];

/** How much of `element` the reader can see: its box cut by every box above it that clips
 *  sideways, and by the window. */
function shownOf(element: Element): { left: number; right: number; width: number; shown: number } {
  const own = element.getBoundingClientRect();
  let left = 0;
  let right = window.innerWidth;
  for (let box = element.parentElement; box !== null; box = box.parentElement) {
    if (getComputedStyle(box).overflowX === "visible") continue;
    const clip = box.getBoundingClientRect();
    left = Math.max(left, clip.left);
    right = Math.min(right, clip.right);
  }
  return {
    left: own.left,
    right: own.right,
    width: own.width,
    shown: Math.max(0, Math.min(own.right, right) - Math.max(own.left, left)),
  };
}

// SLOW(>=2s): twenty-two seeded questions, the row's keyboard order walked at five widths.
test("a chip of the Inbox's row that has the keyboard focus is shown whole, with its whole name, at every width", async ({
  page,
}) => {
  await seedLongInbox();
  const row = controlRow(page.getByTestId("main-content"));
  const cut: string[] = [];
  for (const size of READING_WIDTHS) {
    await page.setViewportSize(size);
    await page.goto(NINE_NARROWINGS);
    await expect(control(row, CONTROL.chip)).toHaveCount(9);
    await control(row, CONTROL.searchBox).focus();
    let chips = 0;
    // The search box, the filter trigger, nine chips and Clear filters, and the row's own
    // controls after them: twenty presses walk past the end of the row.
    for (let press = 0; press < 20; press++) {
      await page.keyboard.press("Tab");
      const reading = await page.evaluate((measure) => {
        const shown = new Function(`return (${measure})`)() as typeof shownOf;
        const focused = document.activeElement;
        if (!(focused instanceof HTMLElement)) return null;
        if (focused.closest('[data-control="control-row"]') === null) return null;
        if (focused.getAttribute("data-control") !== "chip") return { chip: false } as const;
        const name = focused.querySelector(".truncate");
        return {
          chip: true,
          label: focused.getAttribute("aria-label") ?? "",
          nameCut: name !== null && name.scrollWidth > name.clientWidth + 1,
          ...shown(focused),
        } as const;
      }, shownOf.toString());
      if (reading === null) break;
      if (!reading.chip) continue;
      chips++;
      const where = `${size.width}x${size.height}: "${reading.label}" has the focus`;
      if (reading.shown < reading.width - 1) {
        cut.push(
          `${where} and ${Math.round(reading.shown)} of its ${Math.round(reading.width)}px are shown`
        );
      } else if (reading.nameCut) {
        cut.push(`${where} and its name is cut short`);
      }
    }
    expect(chips, `${size.width}x${size.height}: the keyboard reached ${chips} of 9 chips`).toBe(9);
  }
  // The row's own promise (components/ControlRow.tsx): a chip the strip scrolled away is reached
  // by the Tab key and scrolled into view. A reader who cannot see the chip they are on cannot
  // tell which narrowing Enter takes off, which the chips showed whole before this change.
  expect(cut, cut.join("; ")).toEqual([]);
});

// SLOW(>=2s): twenty-two seeded questions, read at five widths.
test("Clear filters stays in view in the Inbox's row however many narrowings are on, at every width", async ({
  page,
}) => {
  await seedLongInbox();
  const row = controlRow(page.getByTestId("main-content"));
  const hidden: string[] = [];
  for (const size of READING_WIDTHS) {
    await page.setViewportSize(size);
    await page.goto(NINE_NARROWINGS);
    await expect(control(row, CONTROL.chip)).toHaveCount(9);
    const clear = row.getByRole("button", { exact: true, name: "Clear filters" });
    await expect(clear).toHaveCount(1);
    const reading = await clear.evaluate((button, measure) => {
      const shown = new Function(`return (${measure})`)() as typeof shownOf;
      return shown(button);
    }, shownOf.toString());
    if (reading.shown < reading.width - 1) {
      hidden.push(
        `${size.width}x${size.height}: ${Math.round(reading.shown)} of Clear filters' ${Math.round(reading.width)}px are in view`
      );
    }
  }
  // R6: Clear filters stays in the control row, and it is the one press that takes nine
  // narrowings off. A mouse's own wheel does not scroll a strip sideways, and the filter panel
  // holds no Clear filters, so a control scrolled out of view is one a pointer reader cannot find.
  expect(hidden, hidden.join("; ")).toEqual([]);
});

/** Which chips of the control row `row` a pointer reader sees whole, in the row's order: a chip
 *  that no box above it and not the window cuts (`shownOf`, handed in as `measure`), and that
 *  neither scroll button of the row covers, since each is laid over one end of the strip. */
function chipsSeenWhole(row: Element, measure: string): boolean[] {
  const shown = new Function(`return (${measure})`)() as typeof shownOf;
  const buttons = [...row.querySelectorAll('button[aria-label^="Scroll filters"]')].map((button) =>
    button.getBoundingClientRect()
  );
  return [...row.querySelectorAll('[data-control="chip"]')].map((chip) => {
    const reading = shown(chip);
    const covered = buttons.some(
      (box) => box.left < reading.right - 1 && box.right > reading.left + 1
    );
    return reading.shown >= reading.width - 1 && !covered;
  });
}

/** Where a chips strip comes to rest, as its `scrollLeft`: unchanged over five frames, since a
 *  scroll button's press scrolls it smoothly and a wheel may too. */
function stripAtRest(strip: Locator): Promise<number> {
  return strip.evaluate((node) => {
    const { promise, resolve } = Promise.withResolvers<number>();
    let last = node.scrollLeft;
    let still = 0;
    const frame = () => {
      still = node.scrollLeft === last ? still + 1 : 0;
      last = node.scrollLeft;
      if (still >= 5) resolve(last);
      else requestAnimationFrame(frame);
    };
    requestAnimationFrame(frame);
    return promise;
  });
}

// SLOW(>=2s): twenty-two seeded questions, the row's chips paged through at five widths.
test("a pointer reaches and removes every chip of the Inbox's row with the row's scroll buttons, at every width", async ({
  page,
}) => {
  await seedLongInbox();
  const row = controlRow(page.getByTestId("main-content"));
  const chips = control(row, CONTROL.chip);
  // The strip the chips scroll in is their own parent.
  const strip = chips.first().locator("..");
  const left = row.getByRole("button", { exact: true, name: "Scroll filters left" });
  const right = row.getByRole("button", { exact: true, name: "Scroll filters right" });
  const scrollLeft = () => strip.evaluate((node) => node.scrollLeft);
  // One press on Scroll filters right, waited out until the strip has moved and stopped.
  const pressRight = async (where: string) => {
    const from = await scrollLeft();
    await right.click();
    await expect
      .poll(scrollLeft, {
        message: `${where}: a press on Scroll filters right left the strip at ${from}px`,
      })
      .not.toBe(from);
    return stripAtRest(strip);
  };
  const readings: string[] = [];
  for (const size of READING_WIDTHS) {
    const where = `${size.width}x${size.height}`;
    await page.setViewportSize(size);
    await page.goto(NINE_NARROWINGS);
    await expect(chips).toHaveCount(9);
    expect(await scrollLeft(), `${where}: the strip does not open at its start`).toBe(0);
    const seen = new Set<number>();
    const look = async () => {
      const whole = await row.evaluate(chipsSeenWhole, shownOf.toString());
      for (const [index, shown] of whole.entries()) if (shown) seen.add(index);
    };
    await look();
    const atStart = seen.size;
    if (atStart === 9) {
      // Every chip is shown whole at once: there is nothing for either button to page to.
      await expect(
        right,
        `${where}: all 9 chips are shown whole, yet Scroll filters right is drawn`
      ).toHaveCount(0);
      await expect(
        left,
        `${where}: all 9 chips are shown whole, yet Scroll filters left is drawn`
      ).toHaveCount(0);
      readings.push(`${where}: all 9 chips shown whole at once, and neither scroll button drawn`);
      continue;
    }
    await expect(
      left,
      `${where}: the strip is at its start, yet Scroll filters left is drawn`
    ).toHaveCount(0);
    const stops: number[] = [];
    while (seen.size < 9) {
      const before = seen.size;
      await expect(
        right,
        `${where}: ${before} of 9 chips seen whole after ${stops.length} presses, and Scroll filters right is not drawn`
      ).toBeVisible();
      stops.push(Math.round(await pressRight(where)));
      await look();
      expect(
        seen.size,
        `${where}: press ${stops.length} on Scroll filters right (strip at ${stops.at(-1)}px) brought no more chip wholly into view: ${before} of 9 seen`
      ).toBeGreaterThan(before);
    }
    await expect(
      right,
      `${where}: every chip was seen whole after ${stops.length} presses (strip at ${stops.at(-1)}px), yet Scroll filters right is still drawn`
    ).toHaveCount(0);
    await expect(
      left,
      `${where}: the strip is paged to its end (${stops.at(-1)}px), and Scroll filters left is not drawn`
    ).toBeVisible();

    // A mouse's plain wheel scrolls the page, not the strip; Shift and the wheel, and a sideways
    // wheel, are the two wheel moves that scroll it, and each must.
    const wheeled: number[] = [];
    for (const [move, deltaX, deltaY, shift] of [
      ["Shift and a vertical wheel", 0, 200, true],
      ["a sideways wheel", 200, 0, false],
    ] as const) {
      await strip.evaluate((node) => {
        node.scrollLeft = 0;
      });
      await stripAtRest(strip);
      const box = await strip.boundingBox();
      if (box === null) throw new Error(`${where}: the strip has no box`);
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
      if (shift) await page.keyboard.down("Shift");
      await page.mouse.wheel(deltaX, deltaY);
      if (shift) await page.keyboard.up("Shift");
      await expect
        .poll(scrollLeft, { message: `${where}: ${move} over the strip left it at 0px` })
        .toBeGreaterThan(0);
      wheeled.push(Math.round(await stripAtRest(strip)));
    }
    readings.push(
      `${where}: ${atStart} of 9 chips shown whole at the start; every chip seen whole after ${stops.length} presses on Scroll filters right (strip at ${stops.join(", ")}px); Shift and the wheel moved the strip 0 to ${wheeled[0]}px, a sideways wheel 0 to ${wheeled[1]}px`
    );
  }

  // And the chip the pointer paged to is one it can take off, in place.
  await page.setViewportSize({ height: 664, width: 390 });
  await page.goto(NINE_NARROWINGS);
  await expect(chips).toHaveCount(9);
  let presses = 0;
  while (!(await row.evaluate(chipsSeenWhole, shownOf.toString())).at(-1)) {
    expect(
      presses,
      "390x664: nine presses on Scroll filters right never showed the last chip whole"
    ).toBeLessThan(9);
    await pressRight("390x664");
    presses++;
  }
  const last = chips.last();
  await expect(last).toHaveAttribute("aria-label", "Remove filter Project: CORE");
  await last.click();
  await expect(
    chips,
    "390x664: pressing the last chip did not leave 8 chips in the row"
  ).toHaveCount(8);
  const address = new URL(page.url());
  expect(address.searchParams.has("project"), "390x664: the address kept ?project=").toBe(false);
  expect(address.searchParams.get("priority"), "390x664: the press took the priorities").toBe(
    "P0,P1,P2"
  );
  readings.push(
    `390x664: the last chip shown whole after ${presses} presses, and pressed: 8 chips left`
  );
  await test.info().attach("the row's chips paged through by its scroll buttons", {
    body: readings.join("\n"),
    contentType: "text/plain",
  });
});

/** Where the keyboard focus is, as the chips line reads it: on a chip (by its name), on the
 *  row's filter trigger, on Clear filters, or anywhere else - the body included - with how much
 *  of the focused element the reader can see. */
async function focusInRow(
  row: Locator
): Promise<{ where: "chip" | "trigger" | "clear" | "elsewhere"; label: string; whole: boolean }> {
  return row.evaluate((root, measure) => {
    const shown = new Function(`return (${measure})`)() as typeof shownOf;
    const focused = document.activeElement;
    if (!(focused instanceof HTMLElement) || !root.contains(focused)) {
      return { where: "elsewhere", label: focused?.tagName ?? "nothing", whole: false } as const;
    }
    const reading = shown(focused);
    const whole = reading.shown >= reading.width - 1;
    const label = focused.getAttribute("aria-label") ?? focused.textContent?.trim() ?? "";
    const control = focused.getAttribute("data-control");
    if (control === "chip") return { where: "chip", label, whole } as const;
    if (control === "panel-trigger") return { where: "trigger", label, whole } as const;
    if (label === "Clear filters") return { where: "clear", label, whole } as const;
    return { where: "elsewhere", label, whole } as const;
  }, shownOf.toString());
}

/** The two pages of R2 whose chips this row presses: the Inbox with nine narrowings on, which
 *  draw nine chips, and the project List with a label, its search text and its three flags on,
 *  of which the three flags draw chips (the label is its column's filter, the text its box). */
const CHIP_FOCUS_SURFACES = [
  { name: "Inbox", path: NINE_NARROWINGS },
  {
    name: "project List",
    path: "/projects/CORE/issues?label=frontend&q=Rollout&needs-you=1&unread=1&unclaimed=1",
  },
] as const;

// SLOW(>=2s): two pages at two widths, every chip of each taken off from the keyboard.
test("a chip taken off from the keyboard hands the focus to the next chip, then the previous, then the filter trigger, and Clear filters hands it to the trigger", async ({
  page,
}) => {
  await seedLongInbox();
  const scene = await seedSurfaces();
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const chips = control(row, CONTROL.chip);
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const surface of CHIP_FOCUS_SURFACES) {
      const where = `${surface.name} at ${size.width}x${size.height}`;
      // The List is the stored view from here on, so its address below opens on it.
      if (surface.name === "project List") await openSurface(page, "project List", scene);
      await page.goto(surface.path);
      await expect(chips.first(), `${where}: no chip is drawn`).toBeVisible();
      // The keys a reader takes a chip off with: the last chip first, which has no next chip,
      // then the first chip by Space and by Delete, then Enter until no chip is left.
      let key = "Enter";
      let pick: "last" | "first" = "last";
      for (let removal = 0; removal < 20; removal++) {
        const before = await chips.evaluateAll((all) =>
          all.map((chip) => chip.getAttribute("aria-label") ?? "")
        );
        if (before.length === 0) break;
        const index = pick === "last" ? before.length - 1 : 0;
        await chips.nth(index).focus();
        await page.keyboard.press(key);
        const took = await expect
          .poll(() => chips.count(), { timeout: 5_000 })
          .toBe(before.length - 1)
          .then(() => true)
          .catch(() => false);
        let used = key;
        if (!took) {
          wrong.push(`${where}: ${key} on "${before[index]}" took no chip off`);
          await chips.nth(index).focus();
          await page.keyboard.press("Enter");
          await expect(chips).toHaveCount(before.length - 1);
          used = "Enter";
        }
        const left = before.filter((_, at) => at !== index);
        const expected =
          left.length === 0
            ? { where: "trigger", label: "the filter trigger" }
            : { where: "chip", label: left[Math.min(index, left.length - 1)] ?? "" };
        const focus = await focusInRow(row);
        const right =
          focus.where === expected.where &&
          (expected.where === "trigger" || focus.label === expected.label);
        if (!right) {
          wrong.push(
            `${where}: after ${used} took "${before[index]}" off the focus is on ${focus.where === "elsewhere" ? `${focus.label}, outside the row's controls` : `"${focus.label}"`}, not on ${expected.where === "chip" ? `"${expected.label}"` : expected.label}`
          );
        } else if (!focus.whole) {
          wrong.push(`${where}: after ${used} the focused "${focus.label}" is not shown whole`);
        }
        if (removal === 0) {
          key = "Space";
          pick = "first";
        } else if (removal === 1) {
          key = "Delete";
        } else {
          key = "Enter";
        }
      }
      // Clear filters unmounts itself, so the focus it leaves goes to the filter trigger.
      await page.goto(surface.path);
      const clear = row.getByRole("button", { exact: true, name: "Clear filters" });
      await clear.focus();
      await page.keyboard.press("Enter");
      await expect(chips, `${where}: Clear filters left chips on`).toHaveCount(0);
      const focus = await focusInRow(row);
      if (focus.where !== "trigger") {
        wrong.push(
          `${where}: after Clear filters the focus is on ${focus.where === "elsewhere" ? `${focus.label}, outside the row's controls` : `"${focus.label}"`}, not on the filter trigger`
        );
      }
    }
  }
  // R6 keeps every chip's removal and Clear filters, and these are now this change's own
  // components: a removal that drops the focus onto the page sends a keyboard reader back to the
  // page's first link for the next chip, which cost 66 presses for 13 chips at 1440 px.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): two pages at two widths, each page's selection bar taken away from the keyboard.
test("the selection bar gone from the keyboard hands the focus to the row marked last, then the first row, never the body, on the Inbox and the project List", async ({
  page,
}) => {
  await seedLongInbox();
  const scene = await seedSurfaces();
  // Forty more issues, so the List's one page holds rows its table does not draw at all: past
  // the window, and past the rows it draws beyond each end of the window.
  for (let index = 0; index < 40; index++) {
    await createIssue({ project: "CORE", title: `Far row ${index}` });
  }
  const main = page.getByTestId("main-content");
  const wrong: string[] = [];
  /** Where the keyboard focus is - the focused element's name, or the tag of the body - and
   *  whether the window and every box that clips it show the whole of it. */
  const focusNow = () =>
    page.evaluate(() => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { label: focused?.tagName ?? "nothing", whole: false };
      }
      const own = focused.getBoundingClientRect();
      let top = 0;
      let bottom = window.innerHeight;
      let left = 0;
      let right = window.innerWidth;
      for (let box = focused.parentElement; box !== null; box = box.parentElement) {
        const style = getComputedStyle(box);
        const clip = box.getBoundingClientRect();
        if (style.overflowY !== "visible") {
          top = Math.max(top, clip.top);
          bottom = Math.min(bottom, clip.bottom);
        }
        if (style.overflowX !== "visible") {
          left = Math.max(left, clip.left);
          right = Math.min(right, clip.right);
        }
      }
      const whole =
        own.top >= top - 1 &&
        own.bottom <= bottom + 1 &&
        own.left >= left - 1 &&
        own.right <= right + 1;
      const label = focused.getAttribute("aria-label") ?? focused.textContent?.trim() ?? "";
      return { label: label === "" ? focused.tagName : label, whole };
    });
  /** Records a focus that is not on `wanted`, or is on it out of the reader's sight. The bar
   *  hands the focus on once the commit that took it away has settled, which for a write that
   *  lands rather than a key pressed is a moment after the bar is seen gone. */
  const focusOn = async (where: string, after: string, wanted: string) => {
    const landed = await expect
      .poll(async () => (await focusNow()).label, { timeout: 2_000 })
      .toBe(wanted)
      .then(() => true)
      .catch(() => false);
    const focus = await focusNow();
    if (!landed) {
      wrong.push(`${where}: after ${after} the focus is on ${focus.label}, not on "${wanted}"`);
    } else if (!focus.whole) {
      wrong.push(`${where}: after ${after} the focused "${wanted}" is not in view`);
    }
  };
  /** Scrolls the list until `row` is above the window, so a focus handed back to it has to bring
   *  it into view: the box around it that scrolls, or the window where none does. */
  const scrollAbove = (row: Locator) =>
    row.evaluate((node) => {
      const by = node.getBoundingClientRect().bottom + 16;
      for (let box = node.parentElement; box !== null; box = box.parentElement) {
        const { overflowY } = getComputedStyle(box);
        if (
          (overflowY === "auto" || overflowY === "scroll") &&
          box.scrollHeight > box.clientHeight
        ) {
          box.scrollTop += by;
          return;
        }
      }
      window.scrollBy(0, by);
    });
  /** Marks a row from the keyboard, by Space on its checkbox. */
  const mark = async (box: Locator) => {
    await box.focus();
    await page.keyboard.press("Space");
    await expect(box).toBeChecked();
  };
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);

    // The Inbox, whose Escape is the keyboard's Clear while no row holds the focus.
    let where = `Inbox at ${size.width}x${size.height}`;
    await page.goto("/");
    const rows = page.locator("[data-inbox-row]");
    await expect(rows.nth(3), `${where}: fewer than four rows`).toBeVisible();
    const inboxBar = page.getByRole("toolbar", { name: "Selected rows" });
    const inboxBox = (index: number) => rows.nth(index).getByRole("checkbox").first();
    const inboxClear = inboxBar.getByRole("button", { exact: true, name: "Clear selection" });
    // The third row and then the second: the row marked last, which is neither the list's first
    // nor the last marked in the list's order.
    await mark(inboxBox(2));
    await mark(inboxBox(1));
    await expect(inboxBar).toContainText("2 selected");
    await scrollAbove(rows.nth(1));
    await inboxClear.focus();
    await page.keyboard.press("Enter");
    await expect(inboxBar, `${where}: Clear selection left the bar up`).toHaveCount(0);
    await focusOn(
      where,
      "Enter on Clear selection",
      (await inboxBox(1).getAttribute("aria-label")) ?? ""
    );
    await mark(inboxBox(0));
    await mark(inboxBox(3));
    await scrollAbove(rows.nth(3));
    await inboxClear.focus();
    await page.keyboard.press("Escape");
    await expect(inboxBar, `${where}: Escape from the bar left the bar up`).toHaveCount(0);
    await focusOn(
      where,
      "Escape from the bar's Clear selection",
      (await inboxBox(3).getAttribute("aria-label")) ?? ""
    );
    // A snooze picked in the bar folds every marked row into Later, the row marked last with
    // them, and the bar goes once the write lands: the focus goes to the first row.
    await mark(inboxBox(2));
    await mark(inboxBox(1));
    const picker = inboxBar.getByRole("combobox", { name: "Snooze selected asks" });
    await picker.focus();
    await picker.selectOption("later-today");
    await expect(inboxBar, `${where}: the snooze left the bar up`).toHaveCount(0);
    await focusOn(
      where,
      "a snooze took the marked rows away",
      (await inboxBox(0).getAttribute("aria-label")) ?? ""
    );
    // The bar never held the focus, so it is not the bar's to move: on a control of the page's
    // own, Escape clears the selection and leaves the focus there.
    const trigger = control(controlRow(main), CONTROL.panelTrigger).first();
    await mark(inboxBox(1));
    await trigger.focus();
    await page.keyboard.press("Escape");
    await expect(inboxBar, `${where}: Escape on the filter trigger left the bar up`).toHaveCount(0);
    if (!(await trigger.evaluate((node) => node === document.activeElement))) {
      const focus = await focusNow();
      wrong.push(
        `${where}: Escape on the filter trigger moved the focus to ${focus.label}, off the trigger`
      );
    }
    // And on a row: `x` takes the one mark off the row in hand, the bar goes, and the focus stays
    // on the row. Escape is not this case: on a row it steps off the row first.
    await rows.nth(0).focus();
    await page.keyboard.press("x");
    await expect(inboxBox(0), `${where}: x marked another row than the focused one`).toBeChecked();
    await expect(inboxBar).toContainText("1 selected");
    await page.keyboard.press("x");
    await expect(inboxBar, `${where}: x on the only marked row left the bar up`).toHaveCount(0);
    if (!(await rows.nth(0).evaluate((node) => node === document.activeElement))) {
      const focus = await focusNow();
      wrong.push(`${where}: x on the focused row moved the focus to ${focus.label}, off the row`);
    }

    // The project List binds no Escape to clearing its selection (`issues-table-page.tsx`,
    // `IssuesTable.tsx`, `components/table-keys.ts`), so its bar goes by its button alone.
    where = `project List at ${size.width}x${size.height}`;
    await openSurface(page, "project List", scene);
    const issueRows = page.locator("[data-issue-row]");
    await expect(issueRows.nth(2), `${where}: fewer than three rows`).toBeVisible();
    const listBar = page.getByRole("toolbar", { name: "Selected issues" });
    const second = await issueRows.nth(1).getAttribute("data-issue-row");
    const third = await issueRows.nth(2).getAttribute("data-issue-row");
    await mark(page.getByRole("checkbox", { exact: true, name: `Select ${third}` }));
    await mark(page.getByRole("checkbox", { exact: true, name: `Select ${second}` }));
    await expect(listBar).toContainText("2 selected");
    await scrollAbove(issueRows.nth(1));
    await listBar.getByRole("button", { exact: true, name: "Clear selection" }).focus();
    await page.keyboard.press("Enter");
    await expect(listBar, `${where}: Clear selection left the bar up`).toHaveCount(0);
    await focusOn(where, "Enter on Clear selection", `Select ${second}`);
    // The row marked last, scrolled so far off that the table no longer draws it: the focus is
    // on its checkbox all the same, brought back into view, and not on a row that happens to be
    // drawn.
    const listBox = page.getByTestId("issues-table-scroll");
    await mark(page.getByRole("checkbox", { exact: true, name: `Select ${second}` }));
    await listBox.evaluate((node) => {
      node.scrollTop = node.scrollHeight;
    });
    await expect(
      page.locator(`[data-issue-row="${second}"]`),
      `${where}: the row marked is still drawn with the list scrolled to its end`
    ).toHaveCount(0);
    await listBar.getByRole("button", { exact: true, name: "Clear selection" }).focus();
    await page.keyboard.press("Enter");
    await expect(listBar, `${where}: Clear selection left the bar up`).toHaveCount(0);
    await focusOn(
      where,
      "Enter on Clear selection with the row marked scrolled out of the drawn rows",
      `Select ${second}`
    );
    // A select-all marks no row of the reader's own. Two rows listed: the reader marks the
    // second, the header's checkbox takes in the first, and the focus goes back to the second.
    await page.goto("/projects/CORE/issues?q=Rollout");
    await expect(issueRows, `${where}: the search does not list two rows`).toHaveCount(2);
    const taken = await issueRows.nth(0).getAttribute("data-issue-row");
    const own = await issueRows.nth(1).getAttribute("data-issue-row");
    await mark(page.getByRole("checkbox", { exact: true, name: `Select ${own}` }));
    await mark(page.getByRole("checkbox", { exact: true, name: "Select every row on this page" }));
    await expect(
      page.getByRole("checkbox", { exact: true, name: `Select ${taken}` })
    ).toBeChecked();
    await expect(listBar).toContainText("2 selected");
    await listBar.getByRole("button", { exact: true, name: "Clear selection" }).focus();
    await page.keyboard.press("Enter");
    await expect(listBar, `${where}: Clear selection left the bar up`).toHaveCount(0);
    await focusOn(where, "Enter on Clear selection after a select-all", `Select ${own}`);
  }
  // A bar that drops the focus onto the page sends a keyboard reader back to its first link, from
  // which the rows they were marking are a walk through every control above them.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): two widths, a thread dismissed from the bar and moved back to the Inbox from it.
test("a thread dismissed from the selection bar, or moved back to the Inbox from it, hands the focus to the first row or the search box, never the body", async ({
  page,
}) => {
  await seedLongInbox();
  // Two threads the reader wrote in, each with a reply under it: the Inbox lists each as a row a
  // reader can mark and dismiss from the bar.
  for (const title of ["Nightly mirror", "Weekly digest"]) {
    const issue = await createIssue({ project: "CORE", title });
    await seedCommentThread(issue.key, `${title}: when does it run?`, "It moved to 03:00.");
  }
  const main = page.getByTestId("main-content");
  const rows = main.locator("[data-inbox-row]");
  const bar = main.locator(`[data-control="${CONTROL.selectionBar}"]`);
  const threads = main.locator('[data-inbox-row^="reply:"]');
  const wrong: string[] = [];
  /** The focused element's name, or the tag of what holds the focus when it is no control. */
  const focused = () =>
    page.evaluate(() => {
      const element = document.activeElement;
      if (!(element instanceof HTMLElement) || element === document.body) {
        return element?.tagName ?? "nothing";
      }
      return element.getAttribute("aria-label") ?? element.textContent?.trim() ?? element.tagName;
    });
  /** The focus once it lands on `wanted` within two seconds, or wherever it is then. */
  const settled = async (wanted: string) =>
    expect
      .poll(focused, { timeout: 2_000 })
      .toBe(wanted)
      .then(() => wanted)
      .catch(() => focused());
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Inbox at ${size.width}x${size.height}`;

    // Dismiss: the marked thread leaves this view with the bar, so the focus goes to the first
    // row's checkbox - the row marked last is gone.
    await page.goto("/");
    await expect(threads, `${where}: no thread row is listed`).toHaveCount(2);
    const thread = await threads.first().getAttribute("data-inbox-row");
    await threads.first().getByRole("checkbox").first().check();
    const dismiss = bar.getByRole("button", { exact: true, name: "Dismiss selected threads" });
    await dismiss.focus();
    await page.keyboard.press("Enter");
    await expect(main.locator(`[data-inbox-row="${thread}"]`)).toHaveCount(0);
    await expect(bar, `${where}: Dismiss left the bar up`).toHaveCount(0);
    const first = await rows.first().getByRole("checkbox").first().getAttribute("aria-label");
    // The hand-off may land a moment after the bar goes, so the focus is read until it settles.
    const afterDismiss = await settled(first ?? "");
    if (afterDismiss !== first) {
      wrong.push(
        `${where}: after Enter on Dismiss the focus is on ${afterDismiss}, not on "${first}"`
      );
    }

    // Move to Inbox, from the dismissed view: the thread leaves that view with the bar, so the
    // focus goes to the first row left there, or to the search box when none is.
    await page.goto("/?show=dismissed");
    const back = main.locator(`[data-inbox-row="${thread}"]`);
    await expect(back, `${where}: the dismissed thread is not listed`).toHaveCount(1);
    await back.getByRole("checkbox").first().check();
    const restore = bar.getByRole("button", {
      exact: true,
      name: "Move to Inbox selected threads",
    });
    await restore.focus();
    await page.keyboard.press("Enter");
    await expect(back).toHaveCount(0);
    await expect(bar, `${where}: Move to Inbox left the bar up`).toHaveCount(0);
    const left = await rows.count();
    const target =
      left > 0
        ? ((await rows.first().getByRole("checkbox").first().getAttribute("aria-label")) ?? "")
        : "Search asks";
    const afterRestore = await settled(target);
    if (afterRestore !== target) {
      wrong.push(
        `${where}: after Enter on Move to Inbox the focus is on ${afterRestore}, not on "${target}"`
      );
    }
  }
  // The ruling the bar's hand-off is built to: a focus the bar held goes to the row marked last,
  // then the first row, then the search box, and never to the body.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

/** What holds the keyboard focus, as a press's outcome reads: the body (the focus is lost), or
 *  the focused control's name and whether the window shows it. */
async function focusOutcome(
  page: Page
): Promise<{ lost: boolean; label: string; inView: boolean }> {
  return page.evaluate(() => {
    const element = document.activeElement;
    if (!(element instanceof HTMLElement) || element === document.body) {
      return { lost: true, label: element?.tagName ?? "nothing", inView: false };
    }
    const box = element.getBoundingClientRect();
    return {
      lost: false,
      label: (element.getAttribute("aria-label") ?? element.textContent ?? element.tagName)
        .trim()
        .slice(0, 60),
      inView:
        box.width > 0 &&
        box.height > 0 &&
        box.bottom > 0 &&
        box.top < window.innerHeight &&
        box.right > 0 &&
        box.left < window.innerWidth,
    };
  });
}

// SLOW(>=2s): three pages' own controls, each pressed from the keyboard at two widths.
test("the Agents page, the Delivery page and the project List keep the keyboard focus when a press takes its own control away, at 1440 and 390", async ({
  page,
}) => {
  await seedAgents();
  await setLiveSessions([plannerSession, reviewerSession]);
  await seedDeliveryFixture();
  const first = await createIssue({ project: "CORE", title: "Preserve the caller identity" });
  const second = await createIssue({ project: "CORE", title: "Document release verification" });
  const adopted = await createIssue({ project: "CORE", title: "Adopt the interrupted member" });
  const epic = await createIssue({ project: "CORE", title: "The interrupted action's epic" });
  // An issue whose title the new epic below repeats, so the server answers it as a possible
  // duplicate and the List draws its Create anyway and Cancel new epic notice.
  await createIssue({ project: "CORE", title: "Recover one created epic" });
  const main = page.getByTestId("main-content");
  const lost: string[] = [];
  /** Reads where the focus went once a press settles, and keeps the ones it lost. */
  const after = async (where: string, press: string) => {
    await expect
      .poll(async () => (await focusOutcome(page)).lost, { timeout: 2_000 })
      .toBe(false)
      .catch(() => undefined);
    const outcome = await focusOutcome(page);
    if (outcome.lost) lost.push(`${where}: after ${press} the focus is on ${outcome.label}`);
    else if (!outcome.inView) {
      lost.push(`${where}: after ${press} the focused "${outcome.label}" is out of view`);
    }
  };
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const at = `${size.width}x${size.height}`;

    // The Agents page's own Clear selection, in its select-all header: it goes with the selection.
    await page.goto("/agents");
    await main
      .getByRole("checkbox", { name: /^Select .* for broadcast$/ })
      .first()
      .check();
    const clear = main.getByRole("button", { exact: true, name: "Clear selection" });
    await clear.focus();
    await page.keyboard.press("Enter");
    await expect(clear).toHaveCount(0);
    await after(`Agents page at ${at}`, "Enter on Clear selection");

    // The Delivery page's Window control: a new preset remounts it under a new key. The seeded
    // window is a custom range, the last option, so ArrowUp picks the last preset.
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
    const windowSelect = main.getByRole("combobox", { name: "Window" });
    await windowSelect.focus();
    await page.keyboard.press("ArrowUp");
    await expect(page).not.toHaveURL(/from=2024-06-01/);
    await after(`Delivery page at ${at}`, "ArrowUp on the Window select");

    // The Delivery page's clear brush window: it goes with the brush.
    await page.goto(
      `/delivery?${DELIVERY_WINDOW}&mode=list&ws=2024-06-01T06%3A00%3A00Z&we=2024-06-01T18%3A00%3A00Z`
    );
    const brush = main.getByRole("button", { exact: true, name: "clear brush window" });
    await brush.focus();
    await page.keyboard.press("Enter");
    await expect(brush).toHaveCount(0);
    await after(`Delivery page at ${at}`, "Enter on clear brush window");

    // The project List's interrupted-action notice: Dismiss and Finish each take the notice
    // with them. The action is the record a stopped New epic leaves, here naming an open epic,
    // so Finish adopts its member and ends.
    const interruptedNotice = async (id: string) => {
      await page.goto("/projects/CORE/issues");
      await page.evaluate(
        ({ action }) => {
          localStorage.setItem(
            "dispatch.project-table.CORE",
            JSON.stringify({ action: { ...action, startedAt: Date.now() } })
          );
        },
        {
          action: {
            id,
            keys: [adopted.key],
            kind: "newEpic",
            project: "CORE",
            target: epic.key,
            writes: {},
          },
        }
      );
      await page.reload();
      const notice = page.getByRole("region", { name: "Interrupted table action" });
      await expect(notice).toHaveCount(1);
      return notice;
    };
    for (const press of ["Dismiss", "Finish"]) {
      const notice = await interruptedNotice(`interrupted-${press}-${at}`);
      await notice.getByRole("button", { exact: true, name: press }).focus();
      await page.keyboard.press("Enter");
      await expect(notice).toHaveCount(0);
      await after(`project List at ${at}`, `Enter on the interrupted action's ${press}`);
    }

    // The project List's possible-duplicate notice: Cancel new epic and Create anyway each take
    // the notice with them.
    for (const press of ["Cancel new epic", "Create anyway"]) {
      await page.goto("/projects/CORE/issues");
      await main.getByRole("checkbox", { exact: true, name: `Select ${first.key}` }).check();
      await main.getByRole("checkbox", { exact: true, name: `Select ${second.key}` }).check();
      await page.getByRole("button", { exact: true, name: "New epic from these" }).click();
      await page
        .getByRole("dialog", { name: "Name the new epic" })
        .getByRole("textbox", { name: "Name" })
        .fill("Recover one created epic");
      await page.getByRole("button", { exact: true, name: "Create epic" }).click();
      const button = main.getByRole("button", { exact: true, name: press });
      await expect(button, `project List at ${at}: no possible-duplicate notice`).toHaveCount(1);
      await button.focus();
      await page.keyboard.press("Enter");
      await expect(button).toHaveCount(0);
      await after(`project List at ${at}`, `Enter on ${press}`);
    }
  }
  // The architect's ruling ff1a60f5, on every control of the pages this change draws: a press
  // that takes its own focused control away hands the focus on, and never to the body.
  expect(lost, lost.join("; ")).toEqual([]);
});

/** The project List's narrowable columns and what shape each one's panel takes. No column of
 *  either table is a date or a number that narrows by two ends: Created and Updated narrow by
 *  the age bands they are grouped into, and Open asks by its own counts, so both are columns of
 *  values like the rest. */
const LIST_COLUMN_FILTERS: Record<string, "picks" | "text"> = {
  Assignee: "picks",
  "Claimed by": "picks",
  Created: "picks",
  "Created by": "picks",
  Epic: "picks",
  Key: "text",
  Labels: "picks",
  "Open asks": "picks",
  Priority: "picks",
  Queue: "picks",
  Status: "picks",
  Title: "text",
};

/** The Delivery list's, which are the five facets that are columns of that table. */
const DELIVERY_COLUMN_FILTERS: Record<string, "picks" | "text"> = {
  Author: "picks",
  Deployed: "picks",
  Issue: "picks",
  "Parent agent": "picks",
  Rework: "picks",
};

/** Reads one table's column filters: which headers carry one, that each sits inside its own
 *  header and at its right end, that a press opens exactly one panel, and what that panel
 *  holds. */
async function readColumnFilters(
  page: Page,
  table: string,
  expected: Record<string, "picks" | "text">
): Promise<void> {
  const headers = page.getByRole("columnheader");
  const named = await headers.evaluateAll((cells) =>
    cells.map((cell) => ({
      control: cell.querySelector('[data-control="column-filter"]') !== null,
      // The header's own label, which is its sort button's text or its plain text.
      name: (cell.querySelector("button")?.textContent ?? cell.textContent ?? "")
        .replace(/[▲▼]/g, "")
        .trim(),
      right: cell.getBoundingClientRect().right,
      filterRight: cell.querySelector('[data-control="column-filter"]')?.getBoundingClientRect()
        .right,
    }))
  );
  const carrying = named.filter((cell) => cell.control).map((cell) => cell.name);
  expect(
    [...carrying].sort(),
    `${table}: the headers carrying a filter are ${carrying.join(", ")}`
  ).toEqual(Object.keys(expected).sort());

  for (const cell of named) {
    if (!cell.control) continue;
    expect(
      (cell.filterRight ?? 0) - cell.right,
      `${table}: the ${cell.name} filter is not at the right end of its own header`
    ).toBeGreaterThanOrEqual(-24);
    expect((cell.filterRight ?? 0) - cell.right).toBeLessThanOrEqual(1);
  }

  for (const [name, kind] of Object.entries(expected)) {
    // A column that already narrows reads `Filter Status: 4 picked`, which is the project
    // List's own opening view.
    const trigger = page.getByRole("button", {
      name: new RegExp(`^Filter ${name}(: \\d+ picked)?$`),
    });
    await expect(trigger, `${table}: no filter control on ${name}`).toHaveCount(1);
    // A press on one header's funnel while another's panel is still shutting is taken by that
    // panel's own outside-dismiss, so the press is made until the trigger says it is open.
    await expect(async () => {
      if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
      expect(await trigger.getAttribute("aria-expanded")).toBe("true");
    }).toPass({ timeout: 10_000 });
    // One panel, and one only: a column of values opens a listbox over its options, a column
    // of free text a dialog holding the box to type in, and neither opens the other's.
    await expect(
      page.locator('[role="listbox"], [role="dialog"]'),
      `${table}: pressing the ${name} filter opened more than one panel`
    ).toHaveCount(1);
    if (kind === "text") {
      await expect(
        page.getByRole("dialog").getByRole("textbox", { name: `Filter ${name}` }),
        `${table}: the ${name} panel holds no text box`
      ).toHaveCount(1);
    } else {
      await expect(
        page.getByRole("combobox", { exact: true, name: `Search ${name}` }),
        `${table}: the ${name} panel holds no text box over its own options`
      ).toHaveCount(1);
    }
    await trigger.click();
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
  }

  // Nothing draws a filter field between a header and the first row: every column filter of
  // this table is inside a header cell.
  const stray = await page.evaluate(() =>
    [...document.querySelectorAll('[data-control="column-filter"]')]
      .filter((element) => element.closest("th") === null)
      .map((element) => element.getAttribute("aria-label") ?? "")
  );
  expect(stray, `${table}: ${stray.join(", ")} is drawn outside a column header`).toEqual([]);
}

// SLOW(>=2s): two tables, every narrowable column of each opened and read.
test("every column that narrows does it from its own header, and no page draws a filter field under one", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  const scene = await seedSurfaces();
  const list = await openSurface(page, "project List", scene);
  await expect(list.firstRow).toBeVisible();
  await readColumnFilters(page, "the project List", LIST_COLUMN_FILTERS);

  // Typing in a column of values leaves only the matching options.
  const status = page.getByRole("button", { name: /^Filter Status(: \d+ picked)?$/ });
  await status.click();
  // The filter's own options: every row of the table draws a priority `select`, whose options
  // carry the same role.
  const options = page.getByRole("listbox", { name: "Status options" }).getByRole("option");
  const before = await options.count();
  await page.getByRole("combobox", { exact: true, name: "Search Status" }).fill("tod");
  await expect(options).toHaveCount(1);
  expect(before, "the Status filter offered only the one option before any text").toBeGreaterThan(
    1
  );
  await expect(options.first()).toHaveAccessibleName(/^Todo/);
  await status.click();

  await seedDeliveryFixture();
  const delivery = await openSurface(page, "Delivery page", {
    ...scene,
    deliveryQuery: DELIVERY_WINDOW,
  });
  await expect(delivery.firstRow).toBeVisible();
  await readColumnFilters(page, "the Delivery list", DELIVERY_COLUMN_FILTERS);
});

// SLOW(>=2s): four pages, each opened and its one trigger read.
test("every narrowing that is no column opens from its page's one filter trigger", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedAgents();
  const scene = await seedSurfaces();
  // The session-groups fixture rather than the timeline one: its pull requests name sessions,
  // and the Session facet draws nothing while no pull request in the window names one
  // (`Facet`, `features/delivery/DeliveryPage.tsx`). Its merges are in the last seven days,
  // which is the window the page opens on, so the Delivery surface below names none.
  await seedSessionGroupsFixture();

  const pages: { controls: string[]; name: Parameters<typeof openSurface>[1] }[] = [
    {
      controls: [
        "Filter by priority",
        "Filter by urgency",
        "Filter by type",
        "Filter by project",
        "Filter by assignee",
      ],
      name: "Inbox",
    },
    { controls: ["Labels", "Needs you", "Unread", "Unclaimed"], name: "project Board" },
    { controls: ["Filter by group", "Filter by machine", "Filter by role"], name: "Agents page" },
    { controls: ["Repository", "Session", "Priority", "Component"], name: "Delivery page" },
  ];
  for (const { controls, name } of pages) {
    const surface = await openSurface(page, name, { ...scene, deliveryQuery: "" });
    const triggers = control(controlRow(surface.root), CONTROL.panelTrigger);
    await expect(triggers, `${name} draws more than one filter trigger in its row`).toHaveCount(1);
    const panel = await openFilterPanel(page, surface.root);
    for (const label of controls) {
      // A narrowing is named either on its own control or beside it, which is what the
      // Delivery page's facets do.
      await expect(
        panel
          .getByRole("button", { name: new RegExp(`^${label}`) })
          .or(panel.getByText(label, { exact: true })),
        `${name}: its panel holds no ${label}`
      ).not.toHaveCount(0);
    }
    await closeControlPanel(surface.root);
  }
});

test("selecting a row moves no row, and the bar that appears is fixed over the list", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  const scene = await seedSurfaces();
  for (const name of ["Inbox", "project List"] as const) {
    const surface = await openSurface(page, name, scene);
    await resetScroll(page, surface.firstRow);
    const before = await topOf(surface.firstRow);
    await surface.select?.();
    await resetScroll(page, surface.firstRow);
    const after = await topOf(surface.firstRow);
    expect(
      after,
      `${name}: selecting a row moved the first row from ${Math.round(before)}px to ${Math.round(after)}px`
    ).toBeCloseTo(before, 0);
    const bar = control(surface.root, CONTROL.selectionBar);
    await expect(bar, `${name}: no selection bar`).toBeVisible();
    await expect(bar, `${name}: the bar does not carry the count`).toContainText("1 selected");
    expect(
      await bar.evaluate((node) => getComputedStyle(node).position),
      `${name}: the selection bar is not fixed over the list`
    ).toBe("fixed");
  }
});

// SLOW(>=2s): eleven seeded rows read through each of the Inbox's own parameters.
test("each page's own parameters open the rows they name", async ({ page }) => {
  const { rows } = await seedEveryField();
  const at = await lastActivity();
  const idsWhere = (match: (row: (typeof rows)[number]) => boolean) =>
    rows.filter(match).map((row) => row.id);
  const expectRows = async (address: string, expected: readonly string[], why: string) => {
    await page.goto(address);
    await expect(page.locator("[data-inbox-row]")).toHaveCount(expected.length);
    expect([...(await drawnIds(page))].sort(), why).toEqual([...expected].sort());
  };

  // The Inbox: the four comma lists, the assignee, the text, the section and the agent.
  await expectRows(
    "/?assignee=everyone&type=question",
    idsWhere((row) => row.type === "question"),
    "?type= lists the questions"
  );
  await expectRows(
    "/?assignee=everyone&priority=P0",
    idsWhere((row) => row.priority === "P0"),
    "?priority= lists the P0 rows"
  );
  await expectRows(
    "/?assignee=everyone&urgency=blocking",
    idsWhere((row) => row.urgency === "blocking"),
    "?urgency= lists the blocking rows"
  );
  await expectRows(
    "/?assignee=everyone&project=MIRROR",
    idsWhere((row) => row.project === "MIRROR"),
    "?project= lists the Mirror rows"
  );
  await expectRows(
    "/?assignee=bob",
    idsWhere((row) => row.assignee === "bob"),
    "?assignee= lists one person's rows"
  );
  await expectRows(
    "/?assignee=everyone&q=Lease handover",
    idsWhere((row) => row.id === rows[2]?.id),
    "?q= lists the one row whose question holds those words"
  );
  // `?show=dismissed` is the reader's other list, which nothing has been dismissed into. The
  // reading is the **Show** control's own, not the empty list's: a list that has not drawn yet
  // is empty too, so a count of nothing would pass whatever the address said.
  await page.goto("/?assignee=everyone&show=dismissed");
  const showing = await openDisplay(page, page.getByTestId("main-content"));
  await expect(
    showing.getByRole("group", { name: "Show" }).locator('input[value="dismissed"]'),
    "?show=dismissed does not put the Show control on the dismissed list"
  ).toBeChecked();
  await page.keyboard.press("Escape");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(0);
  // `?sort=` and `?group=` keep every row and change its place.
  await page.goto("/?assignee=everyone&sort=priority&group=none");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
  expect(await drawnIds(page), "?sort=priority orders by priority").toEqual(
    sortedIds(rows, "priority", at)
  );
  await page.goto("/?assignee=everyone&group=type");
  // A band heading reads its plural and its count in brackets.
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
  expect(await bandHeadings(page), "?group=type bands by type").toEqual(
    valuesOf(rows, "type").map((value) => {
      const plural = {
        agent: "Agent messages",
        approval: "Approvals",
        decision: "Decisions",
        question: "Questions",
        reply: "Replies",
      }[value];
      return `${plural} (${rows.filter((row) => row.type === value).length})`;
    })
  );
  // `?section=needs-you` is the asks waiting on the reader; `?agent=` one session's asks.
  await page.goto("/?section=needs-you");
  // Read the list once it has drawn: an address read before the first row lands is an empty
  // list whatever the address said, which is how CI run 43270255462 read 0 rows here while a
  // faster machine read them all.
  await expect(
    page.locator("[data-inbox-row]").first(),
    "?section=needs-you lists the asks waiting on the reader"
  ).toBeVisible();
  const waiting = await drawnIds(page);
  expect(
    waiting,
    "?section=needs-you drops the one question handed back to its agent"
  ).not.toContain(rows[2]?.id);
  expect(waiting.length, "?section=needs-you lists the asks waiting on the reader").toBeGreaterThan(
    0
  );
  await page.goto(`/?assignee=everyone&agent=${encodeURIComponent("e2e-inbox-view")}`);
  await expect(page.locator("[data-inbox-row]")).toHaveCount(0);
});

test("the project List, the Board and the Agents page open the rows their parameters name", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await seedSurfaces();
  const listed = async (address: string): Promise<string[]> => {
    await page.goto(address);
    await expect(page.getByRole("table", { name: "Project issues table" })).toBeVisible();
    return page
      .locator("[data-issue-row]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-issue-row") ?? ""));
  };
  // Six issues, CORE-1 to CORE-6, every one of them `todo`, holding one open ask, the odd ones
  // labelled `frontend` and the even ones `backend`.
  const every = ["CORE-1", "CORE-2", "CORE-3", "CORE-4", "CORE-5", "CORE-6"];
  expect(await listed("/projects/CORE/issues?sort=-key"), "?sort= orders by a column").toEqual(
    [...every].reverse()
  );
  expect(await listed("/projects/CORE/issues?q=Rollout"), "?q= searches the rows").toEqual([
    "CORE-1",
    "CORE-2",
  ]);
  expect(await listed("/projects/CORE/issues?label=frontend"), "?label= picks a label").toEqual([
    "CORE-1",
    "CORE-3",
    "CORE-5",
  ]);
  expect(await listed("/projects/CORE/issues?status=todo"), "?status= picks a status").toEqual(
    every
  );
  expect(
    await listed("/projects/CORE/issues?f.assignee=alice"),
    "?f.<column>= picks another column's value"
  ).toEqual(every);
  expect(await listed("/projects/CORE/issues?needs-you=1"), "?needs-you=1 keeps the asked").toEqual(
    every
  );
  expect(await listed("/projects/CORE/issues?unread=1"), "?unread=1 keeps the unread").toEqual(
    every
  );
  expect(
    await listed("/projects/CORE/issues?unclaimed=1"),
    "?unclaimed=1 keeps the unclaimed"
  ).toEqual(every);
  expect(
    await listed("/projects/CORE/issues?nofilter=1&size=10&page=1"),
    "?nofilter=, ?size= and ?page= page the whole list"
  ).toEqual(every);
  await page.goto("/projects/CORE/issues?group=status");
  await expect(
    page.getByRole("button", { name: /^Todo/ }).first(),
    "?group= bands the table"
  ).toBeVisible();

  // The Board reads the same five of them.
  const carded = async (address: string): Promise<string[]> => {
    await page.goto(address);
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    return page
      .locator("[data-board-card]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-board-card") ?? ""));
  };
  await page.goto("/projects/CORE/issues");
  const board = page.getByRole("button", { exact: true, name: "Board" });
  if ((await board.getAttribute("aria-pressed")) !== "true") await board.click();
  expect(await carded("/projects/CORE/issues?label=backend"), "the Board's ?label=").toEqual([
    "CORE-2",
    "CORE-4",
    "CORE-6",
  ]);
  expect(await carded("/projects/CORE/issues?q=Ledger"), "the Board's ?q=").toEqual(["CORE-3"]);
  expect(await carded("/projects/CORE/issues?needs-you=1"), "the Board's ?needs-you=").toEqual(
    every
  );
  expect(await carded("/projects/CORE/issues?unread=1"), "the Board's ?unread=").toEqual(every);
  expect(await carded("/projects/CORE/issues?unclaimed=1"), "the Board's ?unclaimed=").toEqual(
    every
  );

  // The Agents page's own two.
  await setLiveSessions([plannerSession, reviewerSession]);
  await page.goto("/agents?q=Reviewer");
  await expect(page.locator(`[data-agent-row="${sessionIds.reviewer}"]`)).toBeVisible();
  await expect(page.locator(`[data-agent-row="${sessionIds.planner}"]`)).toHaveCount(0);
  await page.goto("/agents?machine=box-1");
  await expect(page.locator(`[data-agent-row="${sessionIds.reviewer}"]`)).toBeVisible();
  await expect(page.locator(`[data-agent-row="${sessionIds.planner}"]`)).toBeVisible();
  await page.goto("/agents?machine=nowhere");
  await expect(page.locator("[data-agent-row]")).toHaveCount(0);
});

test("the Delivery page's facet parameters open the pull requests they name", async ({ page }) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await seedDeliveryFixture();
  const listed = async (query: string): Promise<string[]> => {
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list&${query}`);
    await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
    await expect(page.locator("table")).not.toHaveCount(0);
    return page
      .locator("[data-pr-row]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-pr-row") ?? ""));
  };
  // Two pull requests in the window: #1, shipped and attributed to ACME-1, and #2, a rework
  // with no issue. The page spells each facet in the address with its own key, not the
  // server's: `parentAgent`, never `parent_agent`.
  // The list opens newest merge first, so #2, merged at 03:00, is above #1, merged at 01:00.
  const both = ["acme/widgets#2", "acme/widgets#1"];
  expect(await listed("repo=acme%2Fwidgets"), "?repo=").toEqual(both);
  expect(await listed("author=octocat"), "?author=").toEqual(both);
  expect(await listed("issue=ACME-1"), "?issue=").toEqual(["acme/widgets#1"]);
  expect(await listed("rework=rework"), "?rework=").toEqual(["acme/widgets#2"]);
  expect(await listed("deployed=deployed"), "?deployed=").toEqual(["acme/widgets#1"]);
  expect(await listed("parentAgent=__no_session__"), "?parentAgent=").toEqual(both);
  // The fixture's two pull requests name no session, and the server counts none under the
  // placeholder, so this parameter narrows the list to nothing where `parentAgent` keeps both:
  // the two are read apart.
  expect(await listed("session=__no_session__"), "?session=").toEqual([]);
  // ACME-1, the issue #1 is attributed to, is the window's one P0; #2 links no issue at all.
  expect(await listed("priority=0"), "?priority=").toEqual(["acme/widgets#1"]);
  // ACME-1 carries no component, and #2 links no issue at all, so the server counts only the
  // attributed pull request under the placeholder.
  expect(await listed("component=__no_component__"), "?component=").toEqual(["acme/widgets#1"]);
  // And the window and the mode: a preset window outside the fixture's day lists nothing, and
  // without `mode=list` there is no table at all.
  await page.goto(`/delivery?window=7d&mode=list`);
  await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  await expect(page.locator("[data-pr-row]")).toHaveCount(0);
  await page.goto(`/delivery?${DELIVERY_WINDOW}`);
  await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  await expect(page.locator("[data-pr-row]")).toHaveCount(0);
});

// SLOW(>=2s): four parameters, each set, read in the address and found again after a reload.
test("the four parameters this change adds ride in the address and survive a reload, and the drawer writes none", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedAgents();
  const scene = await seedSurfaces();
  await seedDeliveryFixture();

  // The Agents page's `?group=` and `?role=`, neither in the address until its narrowing is on.
  await page.goto("/agents");
  await expect(page).not.toHaveURL(/[?&]group=/);
  await expect(page).not.toHaveURL(/[?&]role=/);
  const agents = page.getByTestId("main-content");
  await openFilterPanel(page, agents);
  for (const [name, option] of [
    ["group", "Needs you"],
    ["role", "planner"],
  ] as const) {
    const trigger = page.getByRole("button", { name: `Filter by ${name}` });
    if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
    await page.getByRole("option", { exact: true, name: option }).click();
    await trigger.click();
  }
  await closeControlPanel(agents);
  await expect(page).toHaveURL(/[?&]group=/);
  await expect(page).toHaveURL(/[?&]role=planner/);
  const agentsAddress = new URL(page.url()).search;
  await page.reload();
  expect(new URL(page.url()).search, "the Agents page's narrowing is gone after a reload").toBe(
    agentsAddress
  );
  await expect(control(controlRow(agents), CONTROL.chip)).not.toHaveCount(0);

  // The Artifacts tab's `?q=`.
  await page.goto(`/issues/${scene.issue}/artifacts`);
  await expect(page).not.toHaveURL(/[?&]q=/);
  const tab = page.getByTestId("main-content");
  await control(controlRow(tab), CONTROL.searchBox).fill("plan");
  await expect(page).toHaveURL(/[?&]q=plan/);
  await page.reload();
  await expect(page, "the Artifacts tab's narrowing is gone after a reload").toHaveURL(
    /[?&]q=plan/
  );
  await expect(control(controlRow(tab), CONTROL.searchBox)).toHaveValue("plan");
  await expect(page.locator('[data-testid^="artifact-"]')).toHaveCount(1);

  // The Delivery page's `?sort=`, absent until a header is pressed.
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  await expect(page.locator("[data-pr-row]").first()).toBeVisible();
  await expect(page).not.toHaveURL(/[?&]sort=/);
  await page.getByRole("button", { exact: true, name: "Author" }).click();
  await expect(page).toHaveURL(/[?&]sort=author/);
  await page.reload();
  await expect(page, "the Delivery page's sort is gone after a reload").toHaveURL(
    /[?&]sort=author/
  );

  // The drawer's own state is its own: narrowing it over the project List writes no parameter.
  await page.goto(`/projects/${scene.project}/issues`);
  const list = page.getByRole("button", { exact: true, name: "List" });
  if ((await list.getAttribute("aria-pressed")) !== "true") await list.click();
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  const before = page.url();
  await peekTrigger(page).click();
  const peek = drawer(page);
  await expect(peek.locator("[data-inbox-row]").first()).toBeVisible();
  await pickInboxField(page, "type", /^Question,/, peek);
  await expect(control(controlRow(peek), CONTROL.chip)).not.toHaveCount(0);
  expect(page.url(), "the drawer wrote its narrowing into the page's address").toBe(before);
});

// SLOW(>=2s): eleven seeded rows worked through every field, sort, grouping and view.
test("the Inbox keeps every field, sort, grouping, view and clear it had", async ({ page }) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  const { rows } = await seedEveryField();
  const at = await lastActivity();
  const main = page.getByTestId("main-content");
  const chips = control(controlRow(main), CONTROL.chip);
  await page.goto("/?assignee=everyone");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);

  // Each of the four fields in turn: only the matching rows are left, and a chip for it is in
  // the control row.
  for (const [field, value, matches] of [
    ["type", /^Question,/, (row: (typeof rows)[number]) => row.type === "question"],
    ["priority", /^P0,/, (row: (typeof rows)[number]) => row.priority === "P0"],
    ["urgency", /^Blocking,/, (row: (typeof rows)[number]) => row.urgency === "blocking"],
    ["project", /^CORE,/, (row: (typeof rows)[number]) => row.project === "CORE"],
  ] as const) {
    await page.goto("/?assignee=everyone");
    await pickInboxField(page, field, value, main);
    const expectedIds = rows
      .filter(matches)
      .map((row) => row.id)
      .sort();
    // The list is read once it holds the narrowed count, never in the frame the pick landed in.
    await expect(page.locator("[data-inbox-row]")).toHaveCount(expectedIds.length);
    expect([...(await drawnIds(page))].sort(), `the ${field} field`).toEqual(expectedIds);
    await expect(chips.filter({ hasText: new RegExp(`^${field}`, "i") })).not.toHaveCount(0);
  }

  // The count beside each value of another field is the count under the narrowing that is on.
  await page.goto("/?assignee=everyone&priority=P0");
  const fields = await openFilterFields(page);
  const typeTrigger = page.getByRole("button", { name: "Filter by type" });
  await typeTrigger.click();
  const names = await page
    .getByRole("listbox", { name: "Type options" })
    .getByRole("option")
    .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("aria-label") ?? ""));
  expect(
    names.find((name) => name.startsWith("Question")),
    "the Type counts are not taken under the priority narrowing"
  ).toBe(
    `Question, ${rows.filter((row) => row.priority === "P0" && row.type === "question").length} rows`
  );
  await typeTrigger.click();
  await expect(fields).toBeVisible();
  await closeControlPanel(main);

  // The assignee control.
  await page.goto("/?assignee=bob");
  const bobsIds = rows
    .filter((row) => row.assignee === "bob")
    .map((row) => row.id)
    .sort();
  await expect(page.locator("[data-inbox-row]")).toHaveCount(bobsIds.length);
  expect([...(await drawnIds(page))].sort(), "the assignee control").toEqual(bobsIds);

  // The six sorts and the seven groupings, worked from the display menu.
  await page.goto("/?assignee=everyone&group=none");
  for (const sort of ["newest", "priority", "urgency", "type", "project", "assignee"] as const) {
    const menu = await openDisplay(page, main);
    await menu.getByRole("group", { name: "Sort by" }).locator(`input[value="${sort}"]`).click();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
    if (sort !== "assignee") {
      expect(await drawnIds(page), `the ${sort} sort`).toEqual(sortedIds(rows, sort, at));
    }
  }
  await page.keyboard.press("Escape");
  // Each banding draws the bands the fixture holds, each with its own count: the five fields
  // one band per value they hold, the turn bands the four rows waiting on a person and the one
  // handed back to its session, and None no band at all.
  for (const group of [
    "priority",
    "urgency",
    "type",
    "project",
    "assignee",
    "waiting-on",
    "none",
  ] as const) {
    const menu = await openDisplay(page, main);
    await menu.getByRole("group", { name: "Group by" }).locator(`input[value="${group}"]`).click();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
    const expected =
      group === "none"
        ? []
        : group === "waiting-on"
          ? [`Waiting on a person (${rows.length - 1})`, "Waiting on agents (1)"]
          : valuesOf(rows, group).map(
              (value) => `${bandLabel(group, value)} (${countsOf(rows, group).get(value)})`
            );
    expect(await bandHeadings(page), `the ${group} grouping`).toEqual(expected);
  }
  await page.keyboard.press("Escape");

  // The dismissed view and the thread order.
  const menu = await openDisplay(page, main);
  await menu.getByRole("group", { name: "Show" }).locator('input[value="dismissed"]').click();
  await expect(page).toHaveURL(/show=dismissed/);
  await menu.getByRole("group", { name: "Show" }).locator('input[value="inbox"]').click();
  await menu.getByRole("group", { name: "Thread order" }).locator("input").last().click();
  await expect(
    menu.getByRole("group", { name: "Thread order" }).locator("input").last()
  ).toBeChecked();
  await page.keyboard.press("Escape");

  // Mark all read, and the fold of one group.
  await page.goto("/?assignee=everyone&group=type");
  const markAll = page.locator("[data-inbox-mark-all]");
  await expect(markAll).toBeVisible();
  await markAll.click();
  await expect(markAll).toHaveCount(0);
  const band = page.getByTestId("inbox-list").locator("h2 button").first();
  await band.click();
  await expect(band).toHaveAttribute("aria-expanded", "false");

  // Clear filters takes off the four fields, the assignee, the agent and the needs-you section,
  // and leaves the text, the sort, the grouping and the view.
  await page.goto(
    "/?assignee=bob&type=question&priority=P0&urgency=blocking&project=CORE&section=needs-you&q=rule&sort=priority&group=type&show=dismissed"
  );
  await controlRow(main).getByRole("button", { exact: true, name: "Clear filters" }).click();
  const address = new URL(page.url());
  for (const gone of ["assignee", "type", "priority", "urgency", "project", "section", "agent"]) {
    expect(address.searchParams.has(gone), `Clear filters left ?${gone}=`).toBe(false);
  }
  expect(address.searchParams.get("q"), "Clear filters took the text").toBe("rule");
  expect(address.searchParams.get("sort"), "Clear filters took the sort").toBe("priority");
  expect(address.searchParams.get("group"), "Clear filters took the grouping").toBe("type");
  expect(address.searchParams.get("show"), "Clear filters took the view").toBe("dismissed");
});

// SLOW(>=2s): the List's whole layout worked and then read back after a reload.
test("the project List keeps its grouping, columns, pager and layout, and the Board its disclosure", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < 60; index++) {
    const issue = await createIssue({ project: "CORE", title: `Listed work ${index}` });
    await patchIssue(issue.key, { labels: ["legion"], status: "todo" });
  }
  await page.goto("/projects/CORE/issues");
  const list = page.getByRole("button", { exact: true, name: "List" });
  if ((await list.getAttribute("aria-pressed")) !== "true") await list.click();
  const table = page.getByRole("table", { name: "Project issues table" });
  await expect(table).toBeVisible();
  const main = page.getByTestId("main-content");

  // The bar of status counts, which this change leaves alone.
  await expect(page.getByRole("button", { name: /^Todo/ }).first()).toBeVisible();

  // The virtual rows: the table draws a window of its page, never all sixty.
  const rows = page.locator("[data-issue-row]");
  const drawn = await rows.count();
  expect(drawn, "the table drew every row of the page at once").toBeLessThan(50);
  expect(drawn, "the table drew no rows").toBeGreaterThan(0);

  // The pager: its own words, and a second page.
  const pager = control(main, CONTROL.pager);
  await expect(pager).toContainText("60 of 60 issues");
  await pager.getByRole("button", { name: "Next" }).click();
  await expect(page).toHaveURL(/[?&]page=2/);
  await pager.getByRole("combobox").selectOption("100");
  await expect(page).toHaveURL(/[?&]size=100/);

  // The grouping, from the display menu.
  const menu = await openDisplay(page, main);
  await menu.getByRole("group", { name: "Group by" }).locator('input[value="status"]').click();
  await expect(page).toHaveURL(/[?&]group=status/);
  await page.keyboard.press("Escape");

  // The column visibility, from the columns menu.
  await control(controlRow(main), CONTROL.columnsMenu).click();
  await page
    .getByRole("dialog", { name: "Columns" })
    .getByRole("checkbox", { exact: true, name: "Updated" })
    .check();
  await page.keyboard.press("Escape");
  const updated = page
    .getByRole("columnheader")
    .filter({ has: page.getByRole("button", { exact: true, name: /^Updated/ }) });
  await expect(updated).toHaveCount(1);

  // The column resize, dragged on the header's own edge.
  const header = page
    .getByRole("columnheader")
    .filter({ has: page.getByRole("button", { exact: true, name: /^Epic/ }) });
  const before = await header.boundingBox();
  if (before === null) throw new Error("the Epic column has no box");
  await page.mouse.move(before.x + before.width - 2, before.y + before.height / 2);
  await page.mouse.down();
  await page.mouse.move(before.x + before.width + 60, before.y + before.height / 2, { steps: 8 });
  await page.mouse.up();
  const after = await header.boundingBox();
  expect(after?.width ?? 0, "the Epic column did not resize").toBeGreaterThan(before.width + 20);

  // The column reorder. The header moves a column on HTML5 drag events - `dragstart` on its
  // sort button, `dragover` and `drop` on the header it lands in (`components/DataTable.tsx`) -
  // which a synthesised mouse drag never produces, so the events are dispatched as the browser
  // would, sharing one `DataTransfer` so the drop reads what the start wrote.
  const order = async () =>
    page
      .getByRole("columnheader")
      .evaluateAll((cells) =>
        cells.map((cell) => (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, ""))
      );
  const orderBefore = await order();
  // The start and the drop are frames apart: the header cell takes a drop only once React has
  // re-rendered with the dragged column in hand, which it has not done in the tick the
  // `dragstart` was dispatched in.
  await page.evaluate(async () => {
    const headerOf = (label: string): HTMLElement | undefined =>
      [...document.querySelectorAll<HTMLElement>("th")].find((cell) =>
        (cell.querySelector("button")?.textContent ?? "").startsWith(label)
      );
    const handle = headerOf("Epic")?.querySelector("button");
    const to = headerOf("Labels");
    if (handle === null || handle === undefined || to === undefined) {
      throw new Error("the Epic and Labels headers are not both drawn");
    }
    const dataTransfer = new DataTransfer();
    handle.dispatchEvent(new DragEvent("dragstart", { bubbles: true, dataTransfer }));
    for (let frame = 0; frame < 3; frame++) {
      const drawn = Promise.withResolvers<void>();
      requestAnimationFrame(() => drawn.resolve());
      await drawn.promise;
    }
    to.dispatchEvent(new DragEvent("dragover", { bubbles: true, cancelable: true, dataTransfer }));
    to.dispatchEvent(new DragEvent("drop", { bubbles: true, dataTransfer }));
    handle.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer }));
  });
  await expect.poll(order, { message: "the columns did not reorder" }).not.toEqual(orderBefore);

  // Reloaded, the stored layout is as it was left.
  const widths = await page
    .getByRole("columnheader")
    .evaluateAll((cells) => cells.map((cell) => Math.round(cell.getBoundingClientRect().width)));
  const kept = await order();
  await page.reload();
  await expect(table).toBeVisible();
  await expect
    .poll(order, { message: "the stored column order is not as it was left" })
    .toEqual(kept);
  expect(
    await page
      .getByRole("columnheader")
      .evaluateAll((cells) => cells.map((cell) => Math.round(cell.getBoundingClientRect().width))),
    "the stored column widths are not as they were left"
  ).toEqual(widths);

  // The Board: the filters open, the last chip cleared, and the disclosure as it was left.
  const board = page.getByRole("button", { exact: true, name: "Board" });
  await board.click();
  await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
  await page.goto("/projects/CORE/issues?needs-you=1&unread=1");
  const boardChips = control(controlRow(main), CONTROL.chip);
  await expect(boardChips).toHaveCount(2);
  await boardChips.last().click();
  await expect(boardChips).toHaveCount(1);
  const edges = page.getByRole("button", { name: /Icebox & Done$/ });
  const edgesLabel = await edges.textContent();
  await edges.click();
  await page.reload();
  await expect(
    page.getByRole("button", { name: /Icebox & Done$/ }),
    "the Board's disclosure is not as it was left"
  ).not.toHaveText(edgesLabel ?? "");
});

test("the Delivery list still answers the keyboard and stays windowed, and each page's own bindings still run", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await seedDeliveryFixture();
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  const prs = page.locator("tbody tr[data-pr-row]");
  await expect(prs).toHaveCount(2);
  await prs.first().focus();
  await page.keyboard.press("ArrowDown");
  await expect(prs.nth(1)).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(prs.first()).toBeFocused();
  await page.keyboard.press("End");
  await expect(prs.nth(1)).toBeFocused();
  await page.keyboard.press("Home");
  await expect(prs.first()).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog", { name: "Details" })).toBeVisible();
  await page.keyboard.press("Escape");
  // The rows are drawn inside the shared table's scroll box, which is the thing that windows
  // them. This fixture holds two pull requests, fewer than any window would cut, so nothing
  // read here could fail for a window that was lost; what the windowing itself is held to is
  // the project List's row above, which seeds 60 issues and reads a drawn count under 50.
  await expect(
    control(page.getByTestId("main-content"), CONTROL.dataTable).locator("tbody tr[data-pr-row]"),
    "the Delivery list is not drawn in the shared table"
  ).toHaveCount(2);

  // The global binding that opens each page, and each page's own list binding.
  await seedAgents();
  await seedSurfaces();
  await page.goto("/");
  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("a");
  await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
  await page.keyboard.press("g");
  await page.keyboard.press("i");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  // The Inbox's own list keys: `j` moves the hand down the list and `x` marks the row.
  await page.locator("body").focus();
  await page.keyboard.press("j");
  await page.keyboard.press("x");
  await expect(control(page.getByTestId("main-content"), CONTROL.selectionBar)).toBeVisible();
  // The project page's own `v`, which flips List and Board.
  await page.goto("/projects/CORE/issues");
  await page.locator("body").focus();
  const boardBefore = await page
    .getByRole("button", { exact: true, name: "Board" })
    .getAttribute("aria-pressed");
  await page.keyboard.press("v");
  await expect(page.getByRole("button", { exact: true, name: "Board" })).not.toHaveAttribute(
    "aria-pressed",
    boardBefore ?? "false"
  );
});

// SLOW(>=2s): a seeded inbox of asks, threads and one credential request, read twice.
test("the control in the row carries what the banner carried, and narrows the Inbox to what waits on the reader", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  // Three asks waiting on Alice, one waiting on Bob, and one handed back to its agent.
  const mine: string[] = [];
  for (const title of ["Ledger rollout", "Mirror retries", "Clock drift"]) {
    const issue = await createIssue({ project: "CORE", title });
    const ask = await createAsk(issue.key, { question: `${title}?` }, SURFACE_SESSION);
    mine.push(ask.id);
  }
  const bobs = await createIssue({ project: "CORE", title: "Lease handover" });
  await patchIssue(bobs.key, { assignee: "bob" });
  await createAsk(bobs.key, { question: "Whose lease?" }, SURFACE_SESSION);
  const answered = await createIssue({ project: "CORE", title: "Replay budget" });
  const answeredAsk = await createAsk(
    answered.key,
    { question: "How many replays?" },
    SURFACE_SESSION
  );
  // Handed back to its agent, so it waits on nobody here.
  await createComment(
    answered.key,
    { ask_id: answeredAsk.id, body: "Checking the budget now.", turn: "agent" },
    SURFACE_SESSION
  );
  // One unread thread, which the sidebar's own number adds and the control does not: a reply
  // under a comment the reader wrote, which is what makes it a thread rather than a comment.
  const thread = await createIssue({ project: "CORE", title: "Nightly mirror" });
  await seedCommentThread(
    thread.key,
    "Is the nightly job still at 02:00?",
    "It moved to 03:00 last week."
  );
  // And one credential request waiting on her.
  await setPendingCredentialRequests([
    {
      approver: "alice",
      identifiers: ["ANTHROPIC_API_KEY"],
      kind: "agent_secret",
      record_id: "record-table-controls",
      requested_at: "2026-09-27T00:00:00Z",
      session: { enrollment: null, request: null },
    },
  ]);

  await page.goto("/");
  const main = page.getByTestId("main-content");
  const list = page.getByTestId("inbox-list");
  await expect(list).toBeVisible();
  // No element above the list carries the words the banner carried.
  await expect(
    main.getByText(/^Blocked on you: /),
    "the old banner's words are still drawn above the Inbox's list"
  ).toHaveCount(0);

  // The control carries the asks waiting on her plus the credential request: three and one.
  const control_ = page.locator("[data-inbox-needs-you]");
  await expect(control_).toHaveText("Blocked on you · 4");
  await expect(control_).toHaveAttribute("aria-label", /^Blocked on you: 4 items/);
  const sidebar = page.getByRole("navigation", { name: "Navigation" }).getByRole("link", {
    name: /^Inbox/,
  });
  const sidebarCount = Number((await sidebar.textContent())?.replace(/\D+/g, "") ?? "0");
  expect(sidebarCount, "the sidebar's number does not add the unread thread").toBeGreaterThan(4);

  // A press narrows the list to the asks waiting on her, keeps the credential section, and puts
  // `section=needs-you` in the address while taking `assignee` out of it.
  await page.goto("/?assignee=bob");
  await page.locator("[data-inbox-needs-you]").click();
  await expect(page).toHaveURL(/section=needs-you/);
  await expect(page).not.toHaveURL(/assignee=/);
  const credentials = page.getByRole("region", { name: "Credential requests" });
  await expect(
    credentials,
    "the credential requests section went with the narrowing"
  ).toBeVisible();
  await expect(page.locator("[data-inbox-row]")).toHaveCount(mine.length);
  const drawn = await drawnIds(page);
  expect([...drawn].sort(), "the narrowed list is not the asks waiting on the reader").toEqual(
    [...mine].sort()
  );
  expect(
    drawn.length + (await credentials.getByRole("listitem").count()),
    "the page lists a different number of items than the control carries"
  ).toBe(4);

  // On the project page the same component's banner is drawn as it is today.
  await page.goto("/projects/CORE/issues");
  await expect(
    page.getByRole("link", { name: /^Blocked on you · \d+$/ }),
    "the project page's pill is gone"
  ).toBeVisible();
});

// SLOW(>=2s): seven tables across three pages, each seeded and read.
test("every table with no controls draws its rows in the shared markup and offers no search, filter or sort", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await createProject({ key: "CORE", name: "Core" });
  await putRepoProject("owner/repo", { project: "CORE" });
  await mintAgentToken("table-controls", "alice");
  await seedFakeGithub({
    "legion/arch": {
      contents: "read",
      files: { "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n" },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  await setSignIns([
    {
      created_at: "2026-10-28T09:00:00Z",
      last_used_at: "2026-11-01T18:30:00Z",
      machine: "workstations@example-project.iam.gserviceaccount.com",
      person: "alice",
      signin_id: "11111111-1111-4111-8111-111111111111",
      thumbprint: "aaaabbbbccccddddeeeeffffgggghhhhiiiijjjjkkk",
    },
  ]);
  await setCredentialGrants([
    {
      approver: "alice",
      created_at: "2026-10-03T12:07:00Z",
      decided_by: "alice",
      enrollment: { kind: "pod", operator: "", runtime_id: "pod-7", slot: null },
      expires_at: "2026-10-03T12:22:00Z",
      grant_id: "22222222-2222-4222-8222-222222222222",
      granted: "approval",
      listedFor: "alice",
      names: ["BILLING_KEY"],
      record_id: "bb22cc33dd44ee55ff6677889900aa11bb22cc33dd44ee55ff6677889900aa11",
    },
  ]);

  /** Reads one plain table: it is inside the shared markup, it has rows, and it offers no
   *  search box, no panel trigger and no sort control of its own. */
  const readPlainTable = async (name: string): Promise<void> => {
    const shell = page.locator(`[data-control="${CONTROL.tableShell}"]`).filter({
      has: page.getByRole("table", { name }),
    });
    await expect(shell, `${name} is not drawn in the shared table markup`).toHaveCount(1);
    await expect(
      shell.getByRole("row"),
      `${name} draws no rows at all, not even a head`
    ).not.toHaveCount(0);
    for (const forbidden of [CONTROL.searchBox, CONTROL.panelTrigger, CONTROL.columnFilter]) {
      await expect(
        shell.locator(`[data-control="${forbidden}"]`),
        `${name} draws a ${forbidden}`
      ).toHaveCount(0);
    }
    // What a sort control is: the header button `DataTable`'s `TableHeaderCell` draws for a
    // sortable column. `th[aria-sort]` would say nothing here, since that attribute is written
    // only while a column IS sorted, so a freshly drawn sortable table carries none either.
    await expect(shell.locator("thead button"), `${name} draws a sort control`).toHaveCount(0);
    await expect(
      control(shell, CONTROL.dataTable),
      `${name} is drawn by the shared TanStack table rather than the plain shell`
    ).toHaveCount(0);
  };

  await page.goto("/settings");
  for (const name of [
    "Projects",
    "Repository mappings",
    "Agent tokens",
    "Architecture sources",
    "Sign-ins",
    "Live grants",
  ]) {
    await readPlainTable(name);
  }

  await seedPipelineFixture();
  await page.goto(`/pipeline?at=${PIPELINE_AT}`);
  await expect(page.getByRole("heading", { level: 1, name: "Pipeline" })).toBeVisible();
  for (const name of ["PR CI: slowest jobs", "PR CI: jobs that failed in the last 24 hours"]) {
    await readPlainTable(name);
  }

  // No second delivery fixture: `seedPipelineFixture` already wrote the one delivery settings
  // row, and a second insert is refused by its primary key. The measures table is behind the
  // panel's own fold, which the page opens shut.
  await page.goto(`/delivery?${DELIVERY_WINDOW}`);
  await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  await page.getByRole("button", { name: /Definitions & alternatives/ }).click();
  await readPlainTable("Per day (UTC), newest first");
});

// SLOW(>=2s): three pages at two widths, a press in each shared control that takes away, hides or
// holds the control it was made on.
test("no press in the shared controls drops the keyboard focus onto the page, at every width", async ({
  page,
}) => {
  const scene = await seedSurfaces();
  // Enough issues for two of the List's pages at 25 a page.
  await Promise.all(
    Array.from({ length: 24 }, (_, index) =>
      createIssue({ project: "CORE", title: `Paged row ${index}` })
    )
  );
  // Two issues in Legion's queue: the second's row offers a Start next of its own, which moves it
  // to the head at the first width, so at the second the other row offers it.
  for (const title of ["Queued first", "Queued second"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { labels: ["legion"], status: "todo" });
  }
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const wrong: string[] = [];
  /** Where each target has already been missed: a target missed once at a width is not waited
   *  for again there, so a row whose every hand-off is gone still ends inside its budget. */
  const missed = new Set<string>();
  /** Holds every issue write until `release`, as a slow server would, so a control can be read
   *  while its write is out; `stop` takes the hold off once the writes have landed. */
  const holdIssueWrites = async () => {
    const { promise, resolve } = Promise.withResolvers<void>();
    const pattern = "**/api/v1/issues/*";
    await page.route(pattern, async (route) => {
      if (route.request().method() !== "PATCH") return route.fallback();
      await promise;
      return route.fallback();
    });
    return { release: resolve, stop: () => page.unroute(pattern) };
  };
  /** The focused element's name, or the tag of the body when nothing is focused. */
  const focusNow = () =>
    page.evaluate(() => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return focused?.tagName ?? "nothing";
      }
      const label =
        focused.getAttribute("aria-label") ??
        (focused instanceof HTMLInputElement ? focused.labels?.[0]?.textContent?.trim() : null) ??
        focused.textContent?.trim() ??
        "";
      return `"${label === "" ? focused.tagName : label}"`;
    });
  /** Records a focus that has not landed on `wanted` within a second: the element named, or
   *  whatever a check reads off the page. Each check follows the change it is about - the
   *  control gone, the page turned, the bar gone - and a hand-off lands in that same commit or
   *  the frame after it, so a second is ample. */
  const focusOn = async (
    where: string,
    press: string,
    wanted: Locator | (() => Promise<boolean>),
    name: string
  ) => {
    if (missed.has(`${where} ${name}`)) return;
    const landed = await (typeof wanted === "function"
      ? expect.poll(wanted, { timeout: 1_000 }).toBe(true)
      : expect(wanted).toBeFocused({ timeout: 1_000 })
    ).then(
      () => true,
      () => false
    );
    if (!landed) {
      missed.add(`${where} ${name}`);
      wrong.push(`${where}: after ${press} the focus is on ${await focusNow()}, not on ${name}`);
    }
  };

  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const at = `${size.width}x${size.height}`;

    // The Inbox's Mark all read, a page control in the row, goes once nothing is left unread:
    // the focus goes to the next control in its slot, Blocked on you, else to the trigger. A
    // thread of its own at each width, since the press at the first marks the first one read.
    let where = `Inbox at ${at}`;
    const thread = await createIssue({ project: "CORE", title: `Nightly mirror at ${at}` });
    await seedCommentThread(thread.key, "Is the nightly job still at 02:00?", "It moved to 03:00.");
    await page.goto("/");
    const markAll = row.getByRole("button", { exact: true, name: "Mark all read" });
    const blocked = row.getByRole("button", { name: /^Blocked on you/ });
    const trigger = control(row, CONTROL.panelTrigger);
    await expect(markAll, `${where}: Mark all read is not drawn`).toBeVisible();
    const besideMarkAll = (await blocked.count()) > 0;
    await markAll.focus();
    await page.keyboard.press("Enter");
    await expect(markAll, `${where}: Enter on Mark all read left it drawn`).toHaveCount(0);
    await focusOn(
      where,
      "Enter on Mark all read",
      besideMarkAll ? blocked : trigger,
      besideMarkAll ? "Blocked on you" : "the filter trigger"
    );

    // A scroll button of the chips line, pressed by a pointer while a chip holds the focus: the
    // press takes no focus, and the button that goes at the strip's end takes none with it.
    await page.goto(NINE_NARROWINGS);
    const chips = control(row, CONTROL.chip);
    await expect(chips, `${where}: the nine narrowings draw no nine chips`).toHaveCount(9);
    const strip = chips.first().locator("..");
    const chip = chips.first();
    const chipName = (await chip.getAttribute("aria-label")) ?? "the first chip";
    await chip.focus();
    for (const side of ["right", "left"] as const) {
      const button = row.getByRole("button", { exact: true, name: `Scroll filters ${side}` });
      await expect(
        button,
        `${where}: Scroll filters ${side} is not drawn, so no chip is out of view that way`
      ).toBeVisible();
      for (let press = 1; (await button.count()) > 0; press++) {
        expect(
          press,
          `${where}: Scroll filters ${side} never reached the strip's end`
        ).toBeLessThan(12);
        const from = await strip.evaluate((node) => node.scrollLeft);
        await button.click();
        await expect
          .poll(() => strip.evaluate((node) => node.scrollLeft), {
            message: `${where}: press ${press} on Scroll filters ${side} left the strip at ${from}px`,
          })
          .not.toBe(from);
        await stripAtRest(strip);
        await focusOn(where, `press ${press} on Scroll filters ${side}`, chip, `"${chipName}"`);
      }
    }

    // The List's pager: Next pressed until the last page and Previous until the first, each
    // refused there rather than disabled, so the focus stays on the button pressed.
    where = `project List at ${at}`;
    await openSurface(page, "project List", scene);
    await page.goto("/projects/CORE/issues?size=25");
    const pager = control(main, CONTROL.pager);
    await expect(pager, `${where}: the List draws no first page`).toContainText(/Page 1 of \d/);
    const pages = Number(/Page 1 of (\d+)/.exec((await pager.textContent()) ?? "")?.[1] ?? "1");
    expect(pages, `${where}: the seed fills fewer than two pages`).toBeGreaterThan(1);
    for (const [step, name, last] of [
      ["Next", "Next ›", pages],
      ["Previous", "‹ Previous", 1],
    ] as const) {
      const button = pager.getByRole("button", { exact: true, name });
      await button.focus();
      for (let press = 1; press < pages; press++) {
        const to = step === "Next" ? press + 1 : pages - press;
        await page.keyboard.press("Enter");
        await expect(pager, `${where}: Enter on ${step} did not reach page ${to}`).toContainText(
          `Page ${to} of ${pages}`
        );
        await focusOn(where, `Enter on ${step} to page ${to}`, button, `"${name}"`);
      }
      await expect(button, `${where}: ${step} on page ${last} is still pressable`).toBeDisabled();
    }

    // A bar action: Take out of epic, held busy while its write is out and pressable by nobody
    // meanwhile. The focus stays on it while it is busy, and once the write lands and the bar
    // goes with the selection it hands the focus to the row marked.
    const first = await page.locator("[data-issue-row]").first().getAttribute("data-issue-row");
    const box = page.getByRole("checkbox", { exact: true, name: `Select ${first}` });
    await box.focus();
    await page.keyboard.press("Space");
    await expect(box, `${where}: Space did not mark ${first}`).toBeChecked();
    const bar = page.getByRole("toolbar", { name: "Selected issues" });
    const takeOut = bar.getByRole("button", { exact: true, name: "Take out of epic" });
    const writes = await holdIssueWrites();
    await takeOut.focus();
    await page.keyboard.press("Enter");
    await expect(
      takeOut,
      `${where}: Take out of epic is pressable while its write is out`
    ).toBeDisabled();
    await focusOn(
      where,
      "Enter on Take out of epic, while its write is out",
      takeOut,
      `"Take out of epic"`
    );
    writes.release();
    await expect(bar, `${where}: the write landed and left the bar up`).toHaveCount(0);
    await focusOn(where, "Take out of epic's write landed", box, `"Select ${first}"`);
    await writes.stop();

    // A row's own Start next: held while its write is out, then gone once its issue starts next,
    // while the row stays. The focus stays on it while it is busy, then goes to the control now
    // at its place in the row, else the one before it, else the row itself.
    const offering = page
      .locator("[data-issue-row]")
      .filter({ has: page.getByRole("button", { exact: true, name: "Start next" }) });
    await expect(
      offering.first(),
      `${where}: no row offers a Start next of its own`
    ).toBeAttached();
    const queued = await offering.first().getAttribute("data-issue-row");
    const queuedRow = page.locator(`[data-issue-row="${queued}"]`);
    const startNext = queuedRow.getByRole("button", { exact: true, name: "Start next" });
    const rank = await holdIssueWrites();
    await startNext.focus();
    await page.keyboard.press("Enter");
    await expect(
      startNext,
      `${where}: ${queued}'s Start next is pressable while its write is out`
    ).toBeDisabled();
    await focusOn(
      where,
      `Enter on ${queued}'s Start next, while its write is out`,
      startNext,
      `${queued}'s "Start next"`
    );
    rank.release();
    await expect(startNext, `${where}: Start next did not start ${queued} next`).toHaveCount(0);
    await focusOn(
      where,
      `Start next started ${queued} next`,
      () => queuedRow.evaluate((node) => node.contains(document.activeElement)),
      "that row or a control in it"
    );
    await rank.stop();

    // The search bar's Filter panel: a pick from its Project menu and from its Status menu, and
    // an author taken, each take their own control away; the focus goes to the next control in
    // the panel.
    where = `search bar at ${at}`;
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const palette = page.getByRole("dialog", { name: "Search" });
    await expect(palette, `${where}: Control+K opened no search bar`).toBeVisible();
    await control(controlRow(palette), CONTROL.panelTrigger).focus();
    await page.keyboard.press("Enter");
    const filters = page.getByRole("dialog", { name: "Filter" });
    await expect(filters, `${where}: Enter on Filter opened no panel`).toBeVisible();
    const author = filters.getByRole("searchbox", { name: "Author" });
    const panelSteps = [
      {
        menu: "Project",
        option: scene.project,
        next: filters.getByRole("button", { exact: true, name: "Status" }),
        nextName: "the Status menu",
      },
      { menu: "Status", option: "Todo", next: author, nextName: "the Author box" },
    ] as const;
    for (const { menu, option, next, nextName } of panelSteps) {
      const opener = filters.getByRole("button", { exact: true, name: menu });
      await opener.focus();
      await page.keyboard.press("Enter");
      const pick = page
        .getByRole("dialog", { name: menu })
        .getByRole("button", { exact: true, name: option });
      await pick.focus();
      await page.keyboard.press("Enter");
      await expect(
        opener,
        `${where}: Enter on the ${menu} menu's ${option} left the ${menu} menu drawn`
      ).toHaveCount(0);
      await focusOn(where, `Enter on the ${menu} menu's ${option}`, next, nextName);
    }
    await author.fill("alice");
    await page.keyboard.press("Enter");
    await expect(author, `${where}: Enter on the Author box left it drawn`).toHaveCount(0);
    await focusOn(
      where,
      "Enter on the Author box",
      filters.getByLabel("Updated since"),
      "the Updated since box"
    );
  }
  // A press that drops the focus onto the page sends a keyboard reader back to its first link,
  // from which the control they were using is a walk through everything above it.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): seven pages at two widths, one press of each page's own controls that disables,
// takes away or draws again the control it was made on.
test("no page-own press drops the keyboard focus onto the page, on any page of R2, at 1440 and 390", async ({
  page,
}) => {
  // Seven pages at two widths take about 40 s on one worker. Each of the 24 presses that loses
  // the focus waits out its 2 s read, so with every hand-off gone the row spends up to 48 s more,
  // and it still ends by naming each press it lost rather than at the budget.
  test.setTimeout(150_000);
  const issue = await seedAgents();
  await seedDeliveryFixture();
  await createIssueArtifact(issue, {
    content: "# plan.md\n\nThe rollout plan.\n",
    name: "plan.md",
  });
  const listFirst = await createIssue({ project: "CORE", title: "Epic member one" });
  const listSecond = await createIssue({ project: "CORE", title: "Epic member two" });
  const boardCard = await createIssue({ project: "CORE", title: "Card that changes lane" });
  await patchIssue(boardCard.key, { priority: 0, status: "todo" });
  const main = page.getByTestId("main-content");
  const lost: string[] = [];
  /** Reads where the focus went once a press settles, and keeps the ones it lost. */
  const after = async (where: string, press: string) => {
    await expect
      .poll(async () => (await focusOutcome(page)).lost, { timeout: 2_000 })
      .toBe(false)
      .catch(() => undefined);
    const outcome = await focusOutcome(page);
    if (outcome.lost) lost.push(`${where}: after ${press} the focus is on ${outcome.label}`);
    else if (!outcome.inView) {
      lost.push(`${where}: after ${press} the focused "${outcome.label}" is out of view`);
    }
  };
  /** A question waiting on Alice, with two options to answer it by. */
  const question = async (title: string) => {
    const owner = await createIssue({ project: "CORE", title });
    const ask = await createAsk(
      owner.key,
      { options: [{ label: "Yes" }, { label: "No" }], question: `${title}?` },
      SURFACE_SESSION
    );
    return { askId: ask.id, issueKey: owner.key };
  };
  /** Opens the project's Issues tab in `view`: the tab opens in the view it was last left in. */
  const openIssues = async (view: "List" | "Board") => {
    await page.goto("/projects/CORE/issues");
    const button = page.getByRole("button", { exact: true, name: view });
    await expect(button).toBeVisible();
    if ((await button.getAttribute("aria-pressed")) !== "true") await button.click();
    await expect(button).toHaveAttribute("aria-pressed", "true");
  };

  for (const [step, size] of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ].entries()) {
    await page.setViewportSize(size);
    const at = `${size.width}x${size.height}`;

    // The Inbox. Each press takes its row, or its own control, off the list. A width of its own
    // seeds each, since the first width's press used it up.
    let where = `Inbox at ${at}`;
    const threads = await createIssue({ project: "CORE", title: `Mirror lag at ${at}` });
    const dismissed = await seedCommentThread(threads.key, `Lagging at ${at}?`, "Caught up.");
    const keyed = await seedCommentThread(threads.key, `Replayed at ${at}?`, "It did.");
    const assigned = await question(`Who takes the lease at ${at}`);
    const snoozed = await question(`Rotate the keys at ${at}`);
    const answered = await question(`Ship the mirror at ${at}`);

    // A thread's Dismiss, by Enter: it holds the focus while its write is out, then goes with
    // its row.
    await page.goto("/");
    const dismissedRow = inboxRow(page, replyRowId(dismissed));
    await dismissedRow.locator("[data-inbox-dismiss]").focus();
    await page.keyboard.press("Enter");
    await expect(dismissedRow, `${where}: Enter on Dismiss kept its row`).toHaveCount(0);
    await after(where, "Enter on a thread's Dismiss");

    // `e` on the thread row in hand: the row holding the focus goes.
    const keyedRow = inboxRow(page, replyRowId(keyed));
    await selectRow(page, replyRowId(keyed));
    await keyedRow.focus();
    await page.keyboard.press("e");
    await expect(keyedRow, `${where}: e kept its row`).toHaveCount(0);
    await after(where, "e on a focused thread row");

    // Assign to me: it holds the focus while its write is out, then goes once the issue is the
    // reader's. Every project names a default assignee, so no seed makes a question nobody
    // holds; the first read of the Inbox is answered as if this one were, and every read after
    // it is the server's.
    const unassign = async (route: Route) => {
      const response = await route.fetch();
      const rows = (await response.json()) as { id: string }[];
      await route.fulfill({
        json: rows.map((row) =>
          row.id === assigned.askId ? { ...row, waiting_on_person: null } : row
        ),
        response,
      });
    };
    await page.route("**/api/v1/inbox", unassign);
    await page.goto("/?assignee=everyone");
    const assign = (await askControls(page, assigned.askId)).getByRole("button", {
      name: `Assign ${assigned.issueKey} to me`,
    });
    await expect(assign, `${where}: the question draws no Assign to me`).toBeVisible();
    await page.unroute("**/api/v1/inbox", unassign);
    await assign.focus();
    await page.keyboard.press("Enter");
    await expect(assign, `${where}: Assign to me stayed once it landed`).toHaveCount(0);
    await after(where, "Enter on Assign to me");

    // A Snooze pick: the row moves into Later, which starts folded, and leaves the list once
    // the snooze lands.
    await page.goto("/");
    const snooze = (await askControls(page, snoozed.askId)).getByRole("combobox", {
      name: /^Snooze /,
    });
    await snooze.focus();
    await snooze.selectOption({ index: 1 });
    await expect(inboxRow(page, snoozed.askId), `${where}: the snooze kept its row`).toHaveCount(0);
    await after(where, "a Snooze pick");

    // An Answer on a card row: the answered row leaves the list. The Inbox page draws its
    // questions as cards below the reading column's width; above it, the drawer does.
    if (size.width >= 1280) {
      where = `Inbox drawer at ${at}`;
      await openIssues("List");
      await expect(page.locator("[data-issue-row]").first()).toBeVisible();
      await peekTrigger(page).click();
    } else {
      await page.goto("/");
    }
    const card = (size.width >= 1280 ? drawer(page) : main).getByTestId(`ask-${answered.askId}`);
    await card.getByRole("radio", { name: "Yes" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).focus();
    await page.keyboard.press("Enter");
    await expect(card, `${where}: Answer kept its card`).toHaveCount(0);
    await after(where, "Enter on a card row's Answer");

    // The project List's unanswered-epic notice: its Dismiss goes with it. The epic's create is
    // answered as one Dispatch made and kept no answer for (`409 KEY_COMMITTED`), which is what
    // draws the notice.
    where = `project List at ${at}`;
    const keyCommitted = async (route: Route) => {
      if (route.request().method() !== "POST") return route.fallback();
      return route.fulfill({
        json: { code: IDEMPOTENCY_KEY_COMMITTED, error: "the key's write was made" },
        status: 409,
      });
    };
    await page.route("**/api/v1/issues", keyCommitted);
    await openIssues("List");
    await main.getByRole("checkbox", { exact: true, name: `Select ${listFirst.key}` }).check();
    await main.getByRole("checkbox", { exact: true, name: `Select ${listSecond.key}` }).check();
    await page.getByRole("button", { exact: true, name: "New epic from these" }).click();
    await page
      .getByRole("dialog", { name: "Name the new epic" })
      .getByRole("textbox", { name: "Name" })
      .fill(`The unanswered epic at ${at}`);
    await page.getByRole("button", { exact: true, name: "Create epic" }).click();
    const epicNotice = main.getByText(/The selected issues have not been moved/);
    await expect(epicNotice, `${where}: no unanswered-epic notice`).toBeVisible();
    await epicNotice.getByRole("button", { exact: true, name: "Dismiss" }).focus();
    await page.keyboard.press("Enter");
    await expect(epicNotice).toHaveCount(0);
    await after(where, "Enter on the unanswered epic's Dismiss");
    await page.unroute("**/api/v1/issues", keyCommitted);

    // The project Board in swimlanes: a priority pick moves the card into another lane, where it
    // is drawn again under a new parent.
    where = `project Board at ${at}`;
    await openIssues("Board");
    const showLanes = page.getByRole("button", { name: "Show lanes" });
    if ((await showLanes.count()) > 0) await showLanes.click();
    await expect(page.getByRole("button", { name: "Hide lanes" })).toBeVisible();
    const lane = `P${step + 1}`;
    const priority = page.getByLabel(`Priority of ${boardCard.key}`);
    await priority.focus();
    await priority.selectOption(String(step + 1));
    await expect(
      page.getByRole("region", { name: `${lane} lane` }).getByLabel(`Priority of ${boardCard.key}`),
      `${where}: the pick left ${boardCard.key} outside the ${lane} lane`
    ).toHaveCount(1);
    await after(where, `a pick of ${lane} on ${boardCard.key}`);

    // The Agents page: the Unread replies pill goes once its press opens the row and reads them,
    // and a recipient's ✕ chip goes with that recipient. A reply of the width's own keeps a pill.
    where = `Agents page at ${at}`;
    const asked = await createAgentMessage(plannerSession.session_id, {
      body: `How far along at ${at}?`,
      delivery: "aside",
    });
    await replyToMessageDelivery(
      asked.id,
      { attempt: 1, body: "Halfway." },
      { id: plannerSession.session_id, kind: "session" }
    );
    await page.goto("/agents");
    const pill = main.getByRole("button", { name: /^New repl.* from Planner$/ });
    await pill.focus();
    await page.keyboard.press("Enter");
    await expect(pill, `${where}: the replies stayed unread`).toHaveCount(0);
    await after(where, "Enter on the Unread replies pill");

    const selectBoxes = main.getByRole("checkbox", { name: /^Select .* for broadcast$/ });
    await selectBoxes.nth(0).check();
    await selectBoxes.nth(1).check();
    const chips = main.getByRole("list", { name: "Selected agents" }).getByRole("button");
    await expect(chips).toHaveCount(2);
    await chips.first().focus();
    await page.keyboard.press("Enter");
    await expect(chips).toHaveCount(1);
    await after(where, "Enter on a recipient's ✕ chip");

    // The Delivery page's Window: Apply on a custom range sets a new window.
    where = `Delivery page at ${at}`;
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
    await main.getByLabel("To", { exact: true }).fill(`2024-06-0${step + 3}`);
    await main.getByRole("button", { exact: true, name: "Apply" }).focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`to=2024-06-0${step + 4}`));
    await after(where, "Enter on Apply over a custom range");

    // The search bar: Retry under a search that failed goes once the search it retries lands.
    // The bar asks twice more before it says a search failed, so the search fails until then.
    where = `search bar at ${at}`;
    let failing = true;
    const isSearch = (url: URL) =>
      url.pathname === "/api/v1/search" || url.pathname === "/api/v1/search/results";
    const failSearch = (route: Route) =>
      failing
        ? route.fulfill({ json: { code: "INTERNAL", error: "search is down" }, status: 500 })
        : route.fallback();
    await page.route(isSearch, failSearch);
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const palette = page.getByRole("dialog", { name: "Search" });
    await control(controlRow(palette), CONTROL.searchBox).fill("rollout");
    const failure = palette.getByRole("alert").filter({ hasText: "Search failed." });
    await expect(failure, `${where}: the search never failed`).toBeVisible();
    failing = false;
    await failure.getByRole("button", { exact: true, name: "Retry" }).focus();
    await page.keyboard.press("Enter");
    await expect(failure, `${where}: Retry left the failure up`).toHaveCount(0);
    await after(where, "Enter on a failed search's Retry");
    if (!(await palette.evaluate((node) => node.contains(document.activeElement)))) {
      lost.push(`${where}: after Enter on Retry the focus is outside the search bar`);
    }
    await page.unroute(isSearch, failSearch);

    // The Artifacts tab: a staged file's Upload holds the focus while the upload is out, and
    // then goes with the staged row.
    where = `Artifacts tab at ${at}`;
    await page.goto(`/issues/${issue}/artifacts`);
    await page.getByLabel("Upload artifact").setInputFiles({
      buffer: Buffer.from(`# Notes at ${at}\n`),
      mimeType: "text/markdown",
      name: `notes-${size.width}.md`,
    });
    const summary = main.getByRole("textbox", { exact: true, name: "Summary (optional)" });
    await expect(summary, `${where}: the file was not staged`).toBeVisible();
    await main.getByRole("button", { exact: true, name: "Upload" }).focus();
    await page.keyboard.press("Enter");
    await expect(summary, `${where}: the upload left the file staged`).toHaveCount(0);
    await after(where, "Enter on a staged file's Upload");
  }
  // The architect's ruling ff1a60f5, on every page of R2: a press that disables, takes away or
  // draws again the control it was made on hands the focus on, and never to the body.
  expect(lost, lost.join("; ")).toEqual([]);
});
