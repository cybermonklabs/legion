import {
  type Browser,
  expect,
  type Locator,
  type Page,
  type TestInfo,
  test,
} from "@playwright/test";

import {
  CONTROL_LABELS,
  PRESET_NAMES,
  type Preset,
} from "../web/src/features/issue-page/page-words";
import { withIssuePage } from "./issue-page-option";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * How a browser row opens the new issue page and reads its frame: the
 * page's root, the view row, the icon rail, Details and the Contents pane and sheet.
 *
 * This is part D's file, and no later part changes it (decision 2 of the parts): each later part
 * adds the helpers its own rows read in a file of its own, `e2e/issue-page-<part>.ts`.
 *
 * - **The page's root** `[data-issue-page]`, which everything the new page draws is inside, a
 *   refused address included (`IssueOutlinePage.tsx`). `gotoIssue` waits for it.
 * - **The view row** `[data-filter-row]`, one line at every width, and in it the five controls
 *   `[data-testid="issue-presets"]`. From 1280 px they are a `toolbar` named
 *   `CONTROL_LABELS.presets` of five toggle `button`s carrying `aria-pressed`, each named by its
 *   view's word and described by its signal, whose count is `[data-preset-count="<view>"]`.
 *   Below 1280 px they are one `button` with `aria-haspopup="menu"`, opening a `menu` of five
 *   `menuitemradio`s (`IssuePresets.tsx`).
 * - **The icon rail** `nav[data-testid="icon-rail"]`, the shell's navigation on the new page
 *   from 768 px up (`IconRail.tsx`).
 * - **Details**, the shell's margin as a drawer over the right of the page,
 *   `[data-testid="desktop-margin-shell"]`, `hidden` while closed, which is how every load opens
 *   it; its head's Close is `[data-testid="details-close"]` (`features/margin/Margin.tsx`).
 * - **The Contents pane** `aside[data-testid="issue-contents"]` from 1024 px up with no view on,
 *   and below 1024 px the `button` named `CONTROL_LABELS.outline` beside the view control, which
 *   opens the same slot in `[data-testid="issue-outline-sheet"]` (`contents-frame.tsx`).
 */

/** The size every row's page opens at. The `iphone` project runs each `@phone` row too, so a
 *  row that asserts another width sets it itself. */
const DESKTOP = { height: 900, width: 1440 } as const;

/** The rows that have run a `phase`. The automatic `freshDatabase` fixture (`e2e/test.ts`)
 *  empties the database before each row, so a row's first `phase` seeds it as that fixture left
 *  it, and only a later `phase` of the same row empties it again. */
const phasedRows = new WeakSet<TestInfo>();

/**
 * One seeded issue, read by alice with the new page turned on: the database is empty, `seed`
 * writes what the row reads, alice signs in to a new context, `withIssuePage` turns the page on
 * for her, and a page opens at 1440 by 900. A row that reads two seeds runs a `phase` for each,
 * and each `phase` after the row's first resets the database, which ends every session.
 */
export async function phase<T>(
  browser: Browser,
  seed: () => Promise<T>,
  body: (page: Page, seeded: T) => Promise<void>
): Promise<void> {
  const row = test.info();
  if (phasedRows.has(row)) {
    await resetDatabase();
  }
  phasedRows.add(row);
  const seeded = await seed();
  const context = await asUser(browser, "alice");
  try {
    await withIssuePage(context, "alice");
    const page = await context.newPage();
    await page.setViewportSize(DESKTOP);
    await body(page, seeded);
  } finally {
    await context.close();
  }
}

/** Opens `path` and waits for the new page's root, so a row's first failure is an assertion of
 *  its own and not a navigation that never settled (decision 26 of the parts). The root is drawn
 *  in its first frame, while it still says "Loading issue…", so this waits on until the read has
 *  answered: for the loaded issue's column, or the refused address's heading. A check of absence
 *  made next reads the page, not its loading state. */
export async function gotoIssue(page: Page, path: string): Promise<void> {
  await page.goto(path);
  await expect(issuePage(page), `${path} drew no [data-issue-page]`).toHaveCount(1);
  await issueRead(page, path);
}

/** Waits until the page's read of its issue has answered, as `gotoIssue` does after `goto`: for
 *  a reload or a move back, which reach the page without it. */
export async function issueRead(page: Page, path = page.url()): Promise<void> {
  await expect(
    page
      .getByTestId("issue-column")
      .or(issuePage(page).getByRole("heading", { level: 1 }))
      .first(),
    `${path} is still loading its issue`
  ).toBeVisible();
}

export function issuePage(page: Page): Locator {
  return page.locator("[data-issue-page]");
}

/** The whole view row: the five controls or the one control, Outline, and Keys. */
export function filterRow(page: Page): Locator {
  return page.locator("[data-filter-row]");
}

/** The five controls, in either of their two shapes. */
export function presetRow(page: Page): Locator {
  return page.getByTestId("issue-presets");
}

/** The row of five, from 1280 px up. */
export function presetToolbar(page: Page): Locator {
  return page.getByRole("toolbar", { exact: true, name: CONTROL_LABELS.presets });
}

/** The five toggles of the row, from 1280 px up. */
export function presetToggles(page: Page): Locator {
  return presetRow(page).locator("button[aria-pressed]");
}

export function presetToggle(page: Page, preset: Preset): Locator {
  return presetRow(page).getByRole("button", { exact: true, name: PRESET_NAMES[preset] });
}

/** The count one control draws: beside its toggle from 1280 px up, and inside the one control
 *  below that. */
export function presetCount(page: Page, preset: Preset): Locator {
  return presetRow(page).locator(`[data-preset-count="${preset}"]`);
}

/** The one control the row is below 1280 px, named by the view that is on and its signal. */
export function presetChooser(page: Page): Locator {
  return presetRow(page).locator('button[aria-haspopup="menu"]');
}

/** The one menu the page has open: the page opens at most one at a time. */
export function openMenu(page: Page): Locator {
  return page.getByRole("menu");
}

/** One item of the one control's menu: a `menuitemradio`, checked where it is the view on,
 *  named by its view's word and then its signal. */
export function presetMenuItem(page: Page, preset: Preset): Locator {
  return openMenu(page).getByRole("menuitemradio", {
    name: new RegExp(`^${PRESET_NAMES[preset]}( |$)`),
  });
}

/** The icon rail the shell draws on the new page in place of its sidebar. */
export function iconRail(page: Page): Locator {
  return page.getByTestId("icon-rail");
}

/** The shell's margin: Details on the new page, and the column it always was on every other. */
export function marginShell(page: Page): Locator {
  return page.getByTestId("desktop-margin-shell");
}

export function marginResizeHandle(page: Page): Locator {
  return page.getByLabel("Resize margin");
}

/** Details' own Close, in its head: a card inside Details can carry a control of that name. */
export function detailsClose(page: Page): Locator {
  return marginShell(page)
    .getByTestId("details-close")
    .and(page.getByRole("button", { exact: true, name: CONTROL_LABELS.close }));
}

/** Opens Details with its key, `Shift+M`, unless it is open already. */
export async function openDetails(page: Page): Promise<void> {
  if (await marginShell(page).isVisible()) return;
  await page.keyboard.press("Shift+M");
  await expect(marginShell(page), "Shift+M did not open Details").toBeVisible();
}

/** Closes Details with its own Close, unless it is closed already, and waits out the frames in
 *  which the drawer hands the focus back, so a key pressed next reaches what the caller focuses
 *  and not the drawer's opener. */
export async function closeDetails(page: Page): Promise<void> {
  if (!(await marginShell(page).isVisible())) return;
  await expect(detailsClose(page), "Details draws no Close").toHaveCount(1);
  await detailsClose(page).click();
  await expect(marginShell(page), "Details did not close from its Close").toBeHidden();
  await page.evaluate(() => {
    const { promise, resolve } = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    return promise;
  });
  expect(
    await marginShell(page).evaluate((drawer) => drawer.contains(document.activeElement)),
    "closing Details left the focus inside it"
  ).toBe(false);
}

/** The Contents pane at the page's left, from 1024 px up with no view on. */
export function contentsPane(page: Page): Locator {
  return page.getByTestId("issue-contents");
}

/** The Outline control beside the view control, below 1024 px with no view on. */
export function outlineControl(page: Page): Locator {
  return page.getByRole("button", { exact: true, name: CONTROL_LABELS.outline });
}

/** The sheet the Outline control opens. */
export function outlineSheet(page: Page): Locator {
  return page.getByTestId("issue-outline-sheet");
}
