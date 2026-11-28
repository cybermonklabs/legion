/** The shared search, sort and filter controls where proving them needs a server: seeded counts,
 *  writes, reloads, the keyboard, and the two pages a recording cannot answer - the Agents page,
 *  whose groups are a session's last-seen time against the clock, and the Delivery page, whose
 *  window is a range of dates. The rows are those of the pages that draw the shared controls: the
 *  Agents page, an issue's Artifacts tab, the search bar, the project List, the Board and the
 *  Delivery page, with its settings form.
 */

import type { APIResponse, Locator, Route } from "@playwright/test";
import { AGENT_LIST_REFILLING, REQUEST_DEADLINE_MS } from "../web/src/api/client";
import { CONTROL } from "../web/src/components/control-names";
import {
  agentFilter,
  agentGroupToggle,
  agentRow,
  type FakeSession,
  getSentMessages,
  issueSearchIn,
  pickAgentFilter,
  plannerSession,
  reviewerSession,
  seedAgents,
  setLiveSessions,
  shownAgentRows,
  toggleAgentGroup,
} from "./agents";
import {
  changeIssueStatus,
  claimIssue,
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  listIssueArtifacts,
  mintAgentToken,
  patchIssue,
  putAgentState,
  replyToMessageDelivery,
} from "./api";
import {
  seedDeliveryFixture,
  seedSessionGroupsFixture,
  TIMELINE_FIXTURE_QUERY,
} from "./delivery-seed";
import { documentEditor, placeCaret } from "./editor";
import { seedFakeGithub } from "./fake-github-helpers";
import {
  control,
  controlRow,
  filterPicker,
  listOf,
  listTrigger,
  openFilterPanel,
  pickFromList,
} from "./filters";
import { focusHolder } from "./focus";
import { nextFrames, settledScrollTop } from "./frames";
import { focusThePage } from "./issue-page-g";
import { sql } from "./psql";
import { sessionIds } from "./session-ids";
import { expandedState, settledValue } from "./settled";
import {
  closeControlPanel,
  drawnControlNames,
  isPhoneProject,
  openDisplay,
  openIssuesTab,
  openSurface,
  REAL_STACK_SURFACES,
  resetScroll,
  SURFACE_CONTROLS,
  SURFACE_SESSION,
  seedSurfaces,
  topOf,
  widthsOf,
} from "./table-controls";
import { expect, type Page, test } from "./test";
import { assertPageFits } from "./unclipped";

/** How far below its first reading the top of a list's first row may sit once a narrowing is
 *  on: one more line of chips, a 44 px control and the row's own 8 px gap. */
const CHIP_LINE = 52;

/** The window the delivery fixture's two pull requests were merged in, with the list drawn. */
const DELIVERY_WINDOW = "from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

/** The seed the Agents and Delivery surfaces are read over.
 *
 *  The Delivery page's freshness rows are pinned to the verdict a reconcile pass reaches here,
 *  as `delivery.e2e.ts` and `review-seed.ts` pin them. The fake GitHub holds no App
 *  installation for acme/widgets, so every pass the server runs on its own five-minute tick
 *  fails with exactly the error written below, and the page's live socket carries that error to
 *  the PRs, Deploy runs and PR CI rows. Unpinned, a pass that landed between two reads moved
 *  the first row with no act of the row's own: at 1440x900 the Source freshness block grew from
 *  one line to two, 16 px to 36 px, between the narrowing's read and the panel's read, and the
 *  first row went from 681 px to 701 px. With the verdict written here, the page shows the same
 *  rows whether or not a pass has run. */
async function seedRealStackSurfaces(): Promise<{
  deliveryQuery: string;
  issue: string;
  project: string;
}> {
  const issue = await seedAgents();
  await seedDeliveryFixture();
  await sql(
    `UPDATE delivery_settings
       SET last_error = 'the GitHub App is not installed on acme/widgets'
     WHERE singleton`
  );
  return { deliveryQuery: DELIVERY_WINDOW, issue, project: "CORE" };
}

// SLOW(>=2s): the same two pages, each narrowed and opened at each of this project's widths, and
// the Agents page's rows selected.
test("no narrowing, panel or selection on the Agents page or the Delivery page moves the first row @phone", async ({
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

      // Selecting rows, on the page whose list allows it, before any narrowing, so the top is
      // held to the first reading itself. The Agents page marks rows for a broadcast: a
      // checkbox on each row (`Select <name> for broadcast`) and one over the list for every row
      // the filters match (`Select all matching agents`, `features/agents/AgentsPage.tsx`). A
      // mark draws the broadcast composer, which follows the list and sticks to the bottom of
      // the viewport rather than standing above the rows. The Delivery page's list marks no
      // row: `PRList` hands `useDataTable` no `selection` (`features/delivery/PRList.tsx`), and
      // a table without one selects nothing (`components/DataTable.tsx`); a press on a row opens
      // that one pull request's details, which marks nothing for an action.
      if (name === "Agents page") {
        const composer = surface.root.getByRole("region", { name: "Broadcast" });
        for (const [act, box] of [
          [
            "one row's checkbox ticked",
            surface.firstRow.getByRole("checkbox", { name: /^Select .+ for broadcast$/ }),
          ],
          [
            "the select-all checkbox ticked",
            surface.root.getByRole("checkbox", { exact: true, name: "Select all matching agents" }),
          ],
        ] as const) {
          await box.check();
          await expect(composer, `${where}: ${act} drew no broadcast composer`).toBeVisible();
          await resetScroll(page, surface.firstRow);
          const selected = await topOf(surface.firstRow);
          expect(
            selected,
            `${where}: ${act} moved the first row from ${Math.round(first)}px to ${Math.round(selected)}px`
          ).toBeCloseTo(first, 0);
        }
        // Unmarked again, so the narrowing below is read on the page as it opened.
        await surface.root.getByRole("button", { exact: true, name: "Clear selection" }).click();
        await expect(composer, `${where}: Clear selection left the composer drawn`).toHaveCount(0);
      }

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

      // Neither page draws a display menu, so the panel is the one act left: it is portalled
      // over the page and takes no room in the layout.
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
    // A chip is drawn only under a narrowing. The Agents page selects rows, for a broadcast, but
    // what a mark draws - the select-all header's count and the broadcast composer
    // (`features/agents/AgentsPage.tsx`) - carries no `data-control` and is no control the design
    // gives the page, so no row is marked here. The Delivery page's list selects none: `PRList`
    // hands `useDataTable` no `selection` (`features/delivery/PRList.tsx`).
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

/** The boxes of the control row `row`, the row's own among them, that a reader can scroll
 *  sideways: each whose `overflow-x` is auto or scroll and that holds more than its own width,
 *  named by its tag, its `data-` attributes and the two widths. */
function sidewaysScrollers(row: Element): string[] {
  return [row, ...row.querySelectorAll("*")].flatMap((box) => {
    const { overflowX } = getComputedStyle(box);
    if (overflowX !== "auto" && overflowX !== "scroll") return [];
    if (box.scrollWidth <= box.clientWidth + 1) return [];
    const marks = [...box.attributes]
      .filter((mark) => mark.name.startsWith("data-"))
      .map((mark) => `[${mark.name}]`)
      .join("");
    return [`${box.tagName.toLowerCase()}${marks} (${box.scrollWidth}px in ${box.clientWidth}px)`];
  });
}

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
  const row = controlRow(main);
  const firstCard = page.locator("[data-board-card]").first();
  // Seven labels, the search text and the needs-you flag: nine chips, and every issue kept.
  const nine = `/projects/CORE/issues?${BOARD_LABELS.map((label) => `label=${label}`).join("&")}&q=Rollout&needs-you=1`;
  const readings: string[] = [];
  const over: string[] = [];
  const sideways: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    // The Board is the stored view from here on, so the address below opens on it too.
    await openSurface(page, "project Board", { issue: first, project: "CORE" });
    await resetScroll(page, firstCard);
    // `openSurface` waited for the first card, and the scroll is reset, so this is the layout.
    const plain = await topOf(firstCard);

    await page.goto(nine);
    await expect(firstCard).toBeVisible();
    // The Board opens its Filter panel by itself while a narrowing is on, unless the reader shut
    // it before (`project.issue-filters`). That panel lies over the chips line and takes the
    // pointer, so it is shut first by its trigger, as a reader shuts it, and the chips are read
    // with no panel over the row. The panel is drawn over the page, so the layout stays the same.
    await closeControlPanel(main);
    await expect(
      page.getByRole("dialog", { name: "Filters" }),
      `${size.width}x${size.height}: the Board's Filter panel stayed open over the row`
    ).toHaveCount(0);
    // One chip per narrowing, so nine chips: the ones the chips line has room for, each whole,
    // and the rest in the panel its `+N more` opens.
    const chips = await chipsInLineAndPanel(row, 9, `${size.width}x${size.height}`);
    // Nothing in the row scrolls sideways. The chips were read above from a settled line, so
    // the boxes read here are the narrowed row's.
    const scrolling = await row.evaluate(sidewaysScrollers);
    sideways.push(...scrolling.map((box) => `${size.width}x${size.height}: ${box}`));
    await resetScroll(page, firstCard);
    // The nine chips were counted and the scroll reset above, so this is the narrowed layout.
    const narrowed = await topOf(firstCard);
    const where = `${size.width}x${size.height}: nine narrowings moved the Board's first card from ${Math.round(plain)}px to ${Math.round(narrowed)}px, ${Math.round(narrowed - plain)}px down`;
    readings.push(
      `${where}, with ${chips.line.length} chips in the line and ${chips.panel.length} behind +N more`
    );
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
  // Nothing in the row scrolls sideways, at any width: a chip the line has no room for is listed
  // behind its `+N more`, never scrolled to.
  expect(sideways, `${sideways.join("; ")} scroll sideways in the Board's row`).toEqual([]);
});

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

/** The names of the narrowings' chips `within` draws, `Remove <label> filter` as `filterChips`
 *  names each, in their order: the chips line's, or the panel's that `+N more` opens. */
function chipNames(within: Locator): Promise<string[]> {
  return within
    .getByRole("button", { name: /^Remove .* filter$/ })
    .evaluateAll((all) => all.map((chip) => chip.getAttribute("aria-label") ?? ""));
}

/** The controls of the chips line of the control row `row` that the reader does not see whole
 *  (`shownOf`, handed in as `measure`): each chip, `+N more` and Clear filters, by its name. */
function cutInChipsLine(row: Element, measure: string): string[] {
  const shown = new Function(`return (${measure})`)() as typeof shownOf;
  const line = row.lastElementChild;
  return [...(line?.querySelectorAll("a[href], button") ?? [])].flatMap((control) => {
    const reading = shown(control);
    if (reading.shown >= reading.width - 1) return [];
    const name = (control.getAttribute("aria-label") ?? control.textContent ?? "").trim();
    return [`"${name}" (${Math.round(reading.shown)} of ${Math.round(reading.width)}px shown)`];
  });
}

/** The name of the row's `+N more`, `<N> more filters`, as a screen reader hears what it opens,
 *  and of the panel it opens. */
const MORE_FILTERS = /^\d+ more filters?$/;

/** The chips line of the control row `row` once it stands for the `count` narrowings that are on:
 *  the chips it draws, by name and in their order, and how many its `+N more` names (`left`). The
 *  line is read until its chips and that number add up to `count`. A chip the line draws in part
 *  is in neither place for the reader, so a control the line cuts fails the row by its name
 *  (`cutInChipsLine`). The panel of `+N more` is not opened. */
async function settledChipsLine(
  row: Locator,
  count: number,
  where: string
): Promise<{ left: number; line: string[] }> {
  const more = row.getByRole("button", { name: MORE_FILTERS });
  let left = 0;
  await expect
    .poll(
      async () => {
        const [line, named] = await Promise.all([
          chipNames(row),
          more.evaluateAll((all) => all.map((chip) => chip.getAttribute("aria-label") ?? "")),
        ]);
        left = Number.parseInt(named[0] ?? "0", 10);
        return line.length + left;
      },
      {
        message: `${where}: the chips line and its +N more stand for another number of chips than the ${count} narrowings that are on`,
      }
    )
    .toBe(count);
  // Every chip was drawn when the poll above passed, and the line is laid out again only when its
  // room or a chip changes size, so what it cuts is read from a settled line.
  const cut = await row.evaluate(cutInChipsLine, shownOf.toString());
  expect(cut, `${where}: the chips line cuts ${cut.join(", ")}`).toEqual([]);
  return { left, line: await chipNames(row) };
}

/** The chips of the `count` narrowings that are on in the control row `row`, by name and in their
 *  order: the ones its chips line draws, then the ones its `+N more` lists in the panel it opens.
 *  The line is read once it stands for them all (`settledChipsLine`), and the panel is then opened
 *  by a pointer and shut by Escape, which hands the focus back to `+N more`. Another count, or a
 *  chip drawn twice, fails the row. */
async function chipsInLineAndPanel(
  row: Locator,
  count: number,
  where: string
): Promise<{ line: string[]; panel: string[] }> {
  const page = row.page();
  const settled = await settledChipsLine(row, count, where);
  let { line } = settled;
  let listed: string[] = [];
  if (settled.left > 0) {
    await row.getByRole("button", { name: MORE_FILTERS }).click();
    const panel = page.getByRole("dialog", { name: MORE_FILTERS });
    await expect(panel, `${where}: +N more opened no panel named for it`).toBeVisible();
    // The panel lists the chips the line leaves out in the commit that draws it, which was waited
    // for above, so the line and the panel are read here as one split of the chips.
    line = await chipNames(row);
    listed = await chipNames(panel);
    await page.keyboard.press("Escape");
    await expect(panel, `${where}: Escape left the panel of +N more open`).toHaveCount(0);
  }
  const all = [...line, ...listed];
  expect(
    `${all.length} chips, ${new Set(all).size} of them named once`,
    `${where}: the line draws ${line.join(", ")}, and the panel of +N more lists [${listed.join(", ")}]`
  ).toBe(`${count} chips, ${count} of them named once`);
  return { line, panel: listed };
}

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
    // The poll above waited up to two seconds for the focus to land, so this is where it settled.
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

    // The Delivery page's Window control: a new preset changes the window under the list's
    // trigger the reader is on, which stays mounted (`WindowControl.tsx`). The seeded window is a
    // custom range, the last option: ArrowUp opens the list on it, ArrowUp again moves to the last
    // preset, and Enter picks that. The pick hands the focus back to the trigger, so `after`'s
    // poll ends at once; the read still sees the press's result, because `toHaveURL` waits for
    // the commit that wrote the new window.
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
    const windowList = listTrigger(main, "Window");
    await windowList.focus();
    await page.keyboard.press("ArrowUp");
    await page.keyboard.press("ArrowUp");
    await page.keyboard.press("Enter");
    await expect(page).not.toHaveURL(/from=2024-06-01/);
    await after(`Delivery page at ${at}`, "Enter on the Window list's last preset");

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
    /** The key the project List of CORE keeps its record under, its interrupted action in it. */
    const tableRecord = "dispatch.project-table.CORE";
    const interruptedNotice = async (id: string) => {
      await page.goto("/projects/CORE/issues");
      await page.evaluate(
        ({ action, key }) => {
          localStorage.setItem(
            key,
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
          key: tableRecord,
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
      // Finish goes on writing after its notice goes, and a page load before its last write
      // stops it, so the List keeps the action and refuses the New epic below. The row waits
      // until the List has let go of the action's record.
      await expect
        .poll(
          () =>
            page.evaluate((key) => {
              const record: unknown = JSON.parse(localStorage.getItem(key) ?? "{}");
              return typeof record === "object" && record !== null && "action" in record
                ? (record.action ?? null)
                : null;
            }, tableRecord),
          { message: `project List at ${at}: the interrupted action's ${press} never ended` }
        )
        .toBeNull();
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
  // Each caller waited for the table's first row, so its header row is drawn, and the assertion
  // below fails a read that missed a filter.
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
  await openIssuesTab(page, "CORE", "Board");
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
  const knownChips = await chipNames(page.locator("main"));
  expect(knownChips, "?priority=: the known value's chip").not.toEqual([]);
  // A value that names no value of its facet is kept, as at the merge base (R5): Dispatch
  // selects no pull request by it, so beside a known value the list is the one the known value
  // alone lists, and alone it lists none. The control row draws its chip by its own word, so the
  // reader can take it off.
  expect(await listed("priority=P9&priority=0"), "?priority= beside an unknown value").toEqual([
    "acme/widgets#1",
  ]);
  expect(
    await chipNames(page.locator("main")),
    "?priority= beside an unknown value: the control row's chips"
  ).toEqual(["Remove Priority: P9 filter", ...knownChips]);
  for (const [query, chip] of [
    ["priority=P9", "Remove Priority: P9 filter"],
    ["rework=constructor", "Remove Rework: constructor filter"],
    ["deployed=bogus", "Remove Deployed: bogus filter"],
  ] as const) {
    // The count is the answer's for the address (`counted`), so the rows read after it are
    // Dispatch's answer, not a table drawn before one came.
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list&${query}`);
    await expect(page.getByText("0 PRs in current filter/window"), `?${query} alone`).toBeVisible();
    await expect(page.locator("[data-pr-row]"), `?${query} alone`).toHaveCount(0);
    expect(await chipNames(page.locator("main")), `?${query} alone: its chip`).toEqual([chip]);
  }
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
  await openIssuesTab(page, "CORE", "List");
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
  const perPage = listTrigger(pager, "Rows per page");
  await pickFromList(perPage, "100 per page");
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
  // The Updated header was waited for above, and the Epic header stands in that same header
  // row, so its box is drawn.
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

/** The Agents page's keep row's sessions, by id: UUIDs, which Dispatch's agent routes require. */
const KEPT_AGENT = {
  auditor: "01a0e812-3135-7000-8000-000000000003",
  planner: "01a0e812-3135-7000-8000-000000000001",
  reviewer: "01a0e812-3135-7000-8000-000000000002",
  sleeper: "01a0e812-3135-7000-8000-000000000005",
  tester: "01a0e812-3135-7000-8000-000000000004",
} as const;

/** The Agents page's four groups, in the order it draws them. */
const AGENT_GROUP_ORDER = ["needs-you", "active", "idle", "quiet"] as const;

/** A live session of that row. `last_seen` is taken when the row seeds, never at this module's
 *  scope, which a worker evaluates once and every later row of that worker ages. */
function keptAgent(
  sessionID: string,
  title: string,
  machine: string,
  role: string,
  silentFor = 5_000
): FakeSession {
  return {
    capabilities: ["aside", "btw"],
    dir: `/srv/${title.toLowerCase()}`,
    last_seen: Date.now() - silentFor,
    machine_id: machine,
    roles: [role],
    session_id: sessionID,
    title,
  };
}

/** The Agents page's round-trip row's sessions, by id: UUIDs, as for the keep row. */
const ONE = "01a0e812-6243-7000-8000-000000000001";
const TWO = "01a0e812-6243-7000-8000-000000000002";
const THREE = "01a0e812-6243-7000-8000-000000000003";

/** A live session of the Agents page's round-trip row, its `last_seen` taken when the row seeds,
 *  as `keptAgent`'s is. */
function live(
  id: string,
  title: string,
  machine: string,
  role: string,
  silentFor = 5_000
): FakeSession {
  return {
    capabilities: ["aside", "btw"],
    dir: `/srv/${title.toLowerCase()}`,
    last_seen: Date.now() - silentFor,
    machine_id: machine,
    roles: [role],
    session_id: id,
    title,
  };
}

/**
 * Five sessions over two machines and three roles, one in each group but Idle, which holds two
 * so a pin has an order to change: the Planner needs the reader (an open ask of its own waits on
 * them), has a question whose next move is its own and a reply the reader has not read; the
 * Reviewer wrote a comment a moment ago; the Auditor and the Tester have done nothing in
 * Dispatch; the Sleeper has not been heard from for an hour. Returns the issue the Planner's asks
 * name, which the search box finds it by.
 */
async function seedKeptAgents(): Promise<string> {
  await setLiveSessions([
    keptAgent(KEPT_AGENT.planner, "Planner", "box-1", "planner"),
    keptAgent(KEPT_AGENT.reviewer, "Reviewer", "box-1", "reviewer"),
    keptAgent(KEPT_AGENT.auditor, "Auditor", "box-2", "tester"),
    keptAgent(KEPT_AGENT.tester, "Tester", "box-2", "tester"),
    keptAgent(KEPT_AGENT.sleeper, "Sleeper", "box-2", "tester", 60 * 60_000),
  ]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Agents kept" });
  const asPlanner = {
    actor: { id: KEPT_AGENT.planner, kind: "session" as const },
    as: "agent" as const,
  };
  await createAsk(issue.key, { question: "Which release?" }, asPlanner);
  // The reader has answered the second, so its next move is the Planner's.
  const answered = await createAsk(issue.key, { question: "Which branch?" }, asPlanner);
  await createComment(issue.key, { ask_id: answered.id, body: "Which one do you mean?" });
  await createComment(
    issue.key,
    { body: "Reviewing the diff." },
    { actor: { id: KEPT_AGENT.reviewer, kind: "session" }, as: "agent" }
  );
  const asked = await createAgentMessage(KEPT_AGENT.planner, {
    body: "How far along?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    asked.id,
    { attempt: 1, body: "Nearly there." },
    { id: KEPT_AGENT.planner, kind: "session" }
  );
  return issue.key;
}

/** Reads each group heading's name and count, finding each heading by its test id, in
 *  `AGENT_GROUP_ORDER`. */
async function expectAgentGroups(
  page: Page,
  headings: readonly [string, string, string, string],
  what: string
): Promise<void> {
  for (const [index, group] of AGENT_GROUP_ORDER.entries()) {
    await expect(
      agentGroupToggle(page, group),
      `${what}: the ${group} group's heading and count`
    ).toHaveAccessibleName(headings[index] ?? "");
  }
}

// R6 on the Agents page. What the page offered at the merge base, dccaf8369449
// (`fork_point(main@origin | @)`), paths under `web/src/`, and how the row drives each:
// - Fields: the group (`features/agents/AgentsPage.tsx:932-949`), the machine (:950-962) and the
//   role (:963-975), each a multi-select, and the search box (:978-990), which matches a
//   session's title, directory, machine, id and the issues its open asks name
//   (`features/agents/agent-search.ts:67-76`). Each is put on and the rows it leaves read, with
//   the group counts under it, and its chip taken off again.
// - Sort: no control. In a group, pinned rows first, then who needs the reader, open asks, newest
//   activity and title (`AgentsPage.tsx:114-144`). Read as the rows' order, and again after a pin.
// - Grouping: Needs you, Active, Idle and Quiet (`features/agents/agent-groups.ts:21-67`,
//   `AgentsPage.tsx:146-173`), each folding (:854-895, :1523-1540). Each group's place is read,
//   and one is folded and opened.
// - Counts: each group heading's `(N)` and `(N, K selected)` (`AgentsPage.tsx:885-887`,
//   `features/agents/selection.ts:61-66`); the select-all header's `N matching` and
//   `K of N matching selected` (`AgentsPage.tsx:1032-1034`, `selection.ts:45-59`); a row's
//   `New reply N` (`AgentsPage.tsx:721-735`), `Needs you N` (:736-744) and `Waiting on agent N`
//   (:745-760); the composer's `Broadcast to N of M selected` (:1136). Each is read.
// - Views: none; the page offers no choice of view.
// - Bulk action: a row's checkbox (`AgentsPage.tsx:631-638`), select-all (:1022-1031), Clear
//   selection (:1035-1043) and the broadcast composer's Send (:1048-1219). Driven to a send, and
//   the fake Envoy read for the one message each selected session was sent.
// - Keys (`features/agents/keyboard.ts:148-244`): `/` (:155-161), `j` (:164-171), `k`
//   (:172-179), Enter (:180-189), `i` (:190-217), Shift+P (:218-224), `x` (:225-232) and Escape
//   (:233-243); and `g a`, the global binding that opens the page (`app.tsx:576`). Each pressed
//   and what it did read.
// - Stored for the reader: the pins (`features/agents/pins.ts:61-125`) and the read mark an
//   opened row writes (`features/agents/unread.ts:128-145`), both the reader's agent state in
//   Dispatch; the page kept nothing in the browser (no `useUserPreference`, no storage). Both are
//   found again after a reload.
// SLOW(>=2s): five seeded sessions worked through every narrowing, count, key, pin and a send.
test("the Agents page keeps every narrowing, grouping, count, key, pin and broadcast it had @phone", async ({
  page,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  const issue = await seedKeptAgents();
  const main = page.getByTestId("main-content");
  const titles = shownAgentRows(page).locator("h2");
  const planner = agentRow(page, KEPT_AGENT.planner);
  const plannerToggle = planner.getByRole("button", { exact: true, name: "Planner" });
  const reviewer = agentRow(page, KEPT_AGENT.reviewer);
  const tester = agentRow(page, KEPT_AGENT.tester);
  const everyRow = ["Planner", "Reviewer", "Auditor", "Tester", "Sleeper"];
  const opened = ["Needs you (1)", "Active (1)", "Idle (2)", "Quiet (1)"] as const;

  // `g a`, from another page.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("a");
  await expect(
    page.getByRole("heading", { level: 1, name: "Agents" }),
    "the g a key opened no Agents page"
  ).toBeVisible();
  await expect(planner).toBeVisible();
  if (phone) await assertPageFits(page);

  // The four groups with their counts, and the order inside them.
  await expectAgentGroups(page, opened, "the page as it opens");
  await expect(titles, "the rows' order").toHaveText(everyRow);
  await expect(
    main.getByText("5 matching", { exact: true }),
    "the select-all header's count"
  ).toBeVisible();
  await expect(
    planner.getByRole("link", { exact: true, name: "Needs you 1" }),
    "the row's Needs you count"
  ).toBeVisible();
  await expect(
    planner.getByRole("link", { exact: true, name: "Waiting on agent 1" }),
    "the row's Waiting on agent count"
  ).toBeVisible();
  const unread = planner.getByRole("button", { exact: true, name: "New reply 1 from Planner" });
  await expect(unread, "the row's count of replies not read").toBeVisible();

  // A group's fold.
  await toggleAgentGroup(page, "idle");
  await expect(titles, "the Idle group's fold").toHaveText(["Planner", "Reviewer", "Sleeper"]);
  await toggleAgentGroup(page, "idle");
  await expect(titles).toHaveText(everyRow);

  // The search box, by a title and by the issue an open ask names.
  const search = control(controlRow(main), CONTROL.searchBox);
  await search.fill("reviewer");
  await expect(titles, "the search box, by a title").toHaveText(["Reviewer"]);
  await expectAgentGroups(
    page,
    ["Needs you (0)", "Active (1)", "Idle (0)", "Quiet (0)"],
    "the search box"
  );
  await expect(
    main.getByText("1 matching", { exact: true }),
    "the select-all header's count under the search box"
  ).toBeVisible();
  await search.fill(issue);
  await expect(titles, "the search box, by an open ask's issue").toHaveText(["Planner"]);
  await search.fill("");
  await expect(titles).toHaveText(everyRow);

  // The three multi-selects, each with its chip.
  const chips = control(controlRow(main), CONTROL.chip);
  for (const [field, option, shown, headings] of [
    ["group", "Quiet", ["Sleeper"], ["Needs you (0)", "Active (0)", "Idle (0)", "Quiet (1)"]],
    [
      "machine",
      "box-2",
      ["Auditor", "Tester", "Sleeper"],
      ["Needs you (0)", "Active (0)", "Idle (2)", "Quiet (1)"],
    ],
    ["role", "reviewer", ["Reviewer"], ["Needs you (0)", "Active (1)", "Idle (0)", "Quiet (0)"]],
  ] as const) {
    await openFilterPanel(page, main);
    await expect(agentFilter(page, field), `the ${field} narrowing`).toBeVisible();
    await pickAgentFilter(page, field, option);
    await expect(titles, `the ${field} narrowing`).toHaveText(shown);
    await expectAgentGroups(page, headings, `the ${field} narrowing`);
    await expect(chips, `the ${field} narrowing's chip`).toHaveCount(1);
    await chips.click();
    await expect(titles, `the ${field} narrowing's chip taken off`).toHaveText(everyRow);
  }

  // The read mark: the count's own press opens the conversation, which reads it. The row is
  // shut again, and the focus left in it.
  await unread.click();
  await expect(unread, "opening the row left its replies unread").toHaveCount(0);
  await plannerToggle.click();

  // The page's own keys. `/` is the page's search box, shadowing the global search bar; `j` from
  // the box takes the first row, the one the press above left the focus in.
  await page.keyboard.press("/");
  await expect(search, "the / key").toBeFocused();
  await expect(page.getByRole("dialog", { name: "Search" })).toHaveCount(0);
  await search.blur();
  await page.keyboard.press("j");
  await expect(planner, "the j key").toBeFocused();
  await page.keyboard.press("j");
  await expect(reviewer, "the j key, again").toBeFocused();
  await page.keyboard.press("k");
  await expect(planner, "the k key").toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    planner.getByRole("textbox", { name: "Comment" }),
    "the Enter key, into the row's composer"
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(planner, "the Escape key, out of the composer").toBeFocused();
  const plannerMark = planner.getByRole("checkbox", { name: "Select Planner for broadcast" });
  await page.keyboard.press("x");
  await expect(plannerMark, "the x key").toBeChecked();
  await page.keyboard.press("x");
  await expect(plannerMark).not.toBeChecked();
  await page.keyboard.press("j");
  await expect(reviewer).toBeFocused();
  await page.keyboard.press("i");
  const picker = reviewer.getByRole("button", { name: "Choose issue" });
  await expect(picker, "the i key").toHaveAttribute("aria-expanded", "true");
  await expect(issueSearchIn(reviewer)).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(picker, "the Escape key, out of the Issue list").toHaveAttribute(
    "aria-expanded",
    "false"
  );
  await expect(reviewer).toBeFocused();
  await page.keyboard.press("j");
  await page.keyboard.press("j");
  await expect(tester).toBeFocused();
  await page.keyboard.press("Shift+P");
  await expect(
    tester.getByRole("button", { name: "Unpin Tester" }),
    "the Shift+P key"
  ).toBeVisible();
  const pinned = ["Planner", "Reviewer", "Tester", "Auditor", "Sleeper"];
  await expect(titles, "the pin's place at the head of its group").toHaveText(pinned);

  // The read mark, read where the page draws it. The Planner answers a second message while its
  // row is shut, which marks nothing read, so its badge counts that one reply; a mark that was
  // lost would count the first reply too.
  await plannerToggle.click();
  await expect(plannerToggle, "the Planner's row did not shut").toHaveAttribute(
    "aria-expanded",
    "false"
  );
  const askedAgain = await createAgentMessage(KEPT_AGENT.planner, {
    body: "And the tests?",
    delivery: "aside",
  });
  await replyToMessageDelivery(
    askedAgain.id,
    { attempt: 1, body: "Green." },
    { id: KEPT_AGENT.planner, kind: "session" }
  );
  const plannerBadge = planner.getByRole("button", { name: /^New repl/ });
  await expect(plannerBadge, "the Planner's reply to a shut row").toHaveAccessibleName(
    "New reply 1 from Planner"
  );

  // What the page stores for the reader, found again after a reload.
  await page.reload();
  await expect(
    tester.getByRole("button", { name: "Unpin Tester" }),
    "the pin, after a reload"
  ).toBeVisible();
  await expect(titles, "the pinned order, after a reload").toHaveText(pinned);
  // The badge is drawn from the read state the reload loads, so its number is read only once
  // that state has answered: one reply unread, the one written after the mark above.
  await expect(plannerBadge, "the read mark, after a reload").toHaveAccessibleName(
    "New reply 1 from Planner"
  );

  // The bulk action: select-all over a narrowing, Clear selection, then two rows and a send.
  await pickAgentFilter(page, "role", "tester");
  await main.getByRole("checkbox", { exact: true, name: "Select all matching agents" }).check();
  await expect(
    main.getByText("3 of 3 matching selected", { exact: true }),
    "select-all"
  ).toBeVisible();
  await expectAgentGroups(
    page,
    ["Needs you (0)", "Active (0)", "Idle (2, 2 selected)", "Quiet (1, 1 selected)"],
    "select-all"
  );
  const composer = page.getByRole("region", { name: "Broadcast" });
  await expect(composer, "select-all drew no broadcast composer").toBeVisible();
  await main.getByRole("button", { exact: true, name: "Clear selection" }).click();
  await expect(composer, "Clear selection").toHaveCount(0);
  await chips.click();
  await expect(titles).toHaveText(pinned);
  await reviewer.getByRole("checkbox", { name: "Select Reviewer for broadcast" }).check();
  await tester.getByRole("checkbox", { name: "Select Tester for broadcast" }).check();
  await expect(
    main.getByText("2 of 5 matching selected", { exact: true }),
    "two rows' checkboxes"
  ).toBeVisible();
  await expect(
    composer.getByRole("heading", { name: "Broadcast to 2 of 2 selected" }),
    "the composer's count"
  ).toBeVisible();
  await composer.getByRole("textbox", { name: "Broadcast message" }).fill("Report status.");
  await composer.getByRole("button", { name: "Send to 2" }).click();
  await page.waitForURL(/\/agents\/broadcasts\/[0-9a-f-]+$/);
  await expect
    .poll(
      async () =>
        (await getSentMessages())
          .filter((sent) => sent.message === "Report status.")
          .map((sent) => String(sent.target_session))
          .sort(),
      { message: "the broadcast sent no message to each selected session" }
    )
    .toEqual([KEPT_AGENT.reviewer, KEPT_AGENT.tester].sort());
});

// The Agents page's narrowings round-trip through the address: the group, machine and role
// picked are written to it and read back after a reload, a chip taken off leaves the address with
// the rest, a link narrows the page with an unknown group dropped, and the picks add no history
// entry, so Back leaves the page.
// SLOW(>=2s): three seeded live sessions, the page reloaded once and opened twice more.
test("the Agents page's group, machine and role round-trip through the address, a reload, a chip and Back", async ({
  page,
}) => {
  await setLiveSessions([
    live(ONE, "Alpha", "box-1", "planner"),
    live(TWO, "Bravo", "box-2", "tester"),
    live(THREE, "Charlie", "box-2", "tester", 60 * 60_000),
  ]);
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await page.goto("/agents");
  const main = page.getByTestId("main-content");
  const titles = shownAgentRows(page).locator("h2");
  await expect(titles).toHaveText(["Alpha", "Bravo", "Charlie"]);
  const params = () => new URL(page.url()).searchParams;
  const chips = control(controlRow(main), CONTROL.chip);

  await pickAgentFilter(page, "group", "Quiet");
  await expect(titles, "group Quiet").toHaveText(["Charlie"]);
  expect(params().get("group"), "?group=").toBe("quiet");
  await pickAgentFilter(page, "role", "tester");
  expect(params().get("role"), "?role=").toBe("tester");
  await pickAgentFilter(page, "machine", "box-2");
  expect(params().get("machine"), "?machine=").toBe("box-2");
  await expect(titles).toHaveText(["Charlie"]);
  await expect(chips).toHaveCount(3);

  await page.reload();
  await expect(titles, "the narrowing after a reload").toHaveText(["Charlie"]);
  await expect(chips, "the chips after a reload").toHaveCount(3);
  await expect(chips.nth(0)).toContainText("Group: Quiet");
  await expect(chips.nth(1)).toContainText("Machine: box-2");
  await expect(chips.nth(2)).toContainText("Role: tester");

  await chips.nth(0).click();
  await expect(titles, "the group chip taken off").toHaveText(["Bravo", "Charlie"]);
  expect(params().get("group"), "the group chip leaves the address").toBeNull();
  expect(params().get("role")).toBe("tester");
  expect(params().get("machine")).toBe("box-2");

  await page.goBack();
  await expect(page, "Back after the picks leaves the page").toHaveURL(/\/$/);

  await page.goto("/agents?group=bogus,quiet,needs-you");
  await expect(chips, "an unknown group is dropped").toHaveCount(2);
  await expect(chips.nth(0), "the groups in the page's order").toContainText("Group: Needs you");
  await expect(chips.nth(1)).toContainText("Group: Quiet");
  await expect(titles).toHaveText(["Charlie"]);
});

// R6 on an issue's Artifacts tab. What the tab offered at the merge base, dccaf8369449
// (`fork_point(main@origin | @)`), paths under `web/src/`, and how the row drives each:
// - Field: the one text box, which keeps an artifact whose name or kind holds the words
//   (`features/artifacts/ArtifactsTab.tsx:243`, :281-289, :306-317) and says so when none does
//   (:333-338). Worked by a name, by a kind and by words nothing holds.
// - Sort: no control. The primary document first, then the newest written, then the id
//   (:269-280). Read as the rows' order, and again once an upload is the newest.
// - Grouping: none.
// - Counts: each row's `N versions` (:196-202) and the references' `References (N)` (:116-150).
//   Both read; the references opened and their rows counted against the number.
// - Views: the references, shut until opened (:121-127). Opened.
// - Bulk action: none; a row's own controls (copy, approval, Download, Remove, :211-231) act on
//   that row alone.
// - The strip's two controls that add to the list: New document (:318-323,
//   `features/artifacts/CreateDocument.tsx`) and Upload (:324, `features/artifacts/ArtifactUpload.tsx`).
//   Each used, and what it added read in the list.
// - Keys: the tab's list answers none; `t a`, the issue page's binding that opens the tab
//   (`features/issue/IssuePage.tsx:153`, :324-330). Pressed.
// - Stored for the reader: nothing; the words were the tab's own state (:243), kept nowhere.
// SLOW(>=2s): four seeded documents, an upload and a new document, read in the tab.
test("an issue's Artifacts tab keeps its narrowing, its order, its counts, its strip and its key @phone", async ({
  page,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const home = await createIssue({ project: "CORE", title: "Runbook home" });
  const runbook = await createIssueArtifact(home.key, {
    content: "# Runbook\n\nThe steps, in order.\n",
    name: "runbook.md",
  });
  const issue = await createIssue({
    project: "CORE",
    spec: `# Kept spec\n\nThe steps are dispatch://${home.key}/artifact/${runbook.artifact.slug}.\n`,
    title: "Artifacts kept",
  });
  const plan = await createIssueArtifact(issue.key, {
    content: "# Plan\n\nThe first draft.\n",
    name: "plan.md",
  });
  const notes = await createIssueArtifact(issue.key, {
    content: "# Notes\n\nKept as written.\n",
    name: "notes.md",
  });
  // The plan takes a second version after the notes were written, so it is the newer of the two.
  await editArtifact(
    plan.artifact.id,
    { ops: [{ find: "first draft", op: "replace", with: "second draft" }] },
    { as: "agent" }
  );
  await expect
    .poll(async () => ((await getArtifact(plan.artifact.id)).versions ?? []).length)
    .toBe(2);
  const spec = (await listIssueArtifacts(issue.key)).find((artifact) => artifact.primary);
  if (spec === undefined) throw new Error("the issue holds no primary document");

  // `t a`, from the issue's own page. On a phone the review panel opens over the page as a
  // dialog, which holds the keyboard, so it is shut first.
  await page.goto(`/issues/${issue.key}`);
  await expect(page.getByTestId("issue-header")).toBeVisible();
  if (phone) {
    const reviewPanel = page.getByRole("button", { name: /review panel/i });
    if ((await expandedState("the review panel", reviewPanel)) === "open") {
      await reviewPanel.click();
    }
  }
  await page.locator("body").focus();
  await page.keyboard.press("t");
  await page.keyboard.press("a");
  await expect(page, "the t a key").toHaveURL(new RegExp(`/issues/${issue.key}/artifacts$`));
  const main = page.getByTestId("main-content");
  const rows = main.locator('[data-testid^="artifact-"]');
  const order = () =>
    rows.evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-testid") ?? ""));
  await expect(rows).toHaveCount(3);
  if (phone) await assertPageFits(page);

  // The order, and each row's count of versions.
  expect(await order(), "the order: the primary document, then the newest written").toEqual([
    `artifact-${spec.slug}`,
    `artifact-${plan.artifact.slug}`,
    `artifact-${notes.artifact.slug}`,
  ]);
  await expect(rows.first().getByText("Primary", { exact: true })).toBeVisible();
  await expect(
    main.getByTestId(`artifact-${plan.artifact.slug}`),
    "a row's count of versions"
  ).toContainText("doc · 2 versions");
  await expect(main.getByTestId(`artifact-${notes.artifact.slug}`)).toContainText(
    "doc · 1 version"
  );

  // The references' count, which is the number of rows they open on.
  const references = main.getByText(/^References \(\d+\)$/);
  await expect(references, "the references' count").toBeVisible();
  const counted = Number(/\((\d+)\)/.exec((await references.textContent()) ?? "")?.[1]);
  await references.click();
  const referenced = main.getByRole("region", { name: "References" });
  await expect(referenced.getByRole("listitem"), "the references' count").toHaveCount(counted);
  await expect(referenced.getByRole("link", { exact: true, name: "runbook.md" })).toBeVisible();

  // Upload, which adds a file - a kind the text box narrows by.
  await page.getByLabel("Upload artifact").setInputFiles({
    buffer: Buffer.from("one line\n"),
    mimeType: "text/plain",
    name: "trace.log",
  });
  await main.getByRole("button", { exact: true, name: "Upload" }).click();
  const trace = main.getByTestId("artifact-trace-log");
  await expect(trace, "Upload").toContainText("file · 1 version");
  expect(await order(), "the order, with the upload the newest written").toEqual([
    `artifact-${spec.slug}`,
    "artifact-trace-log",
    `artifact-${plan.artifact.slug}`,
    `artifact-${notes.artifact.slug}`,
  ]);

  // The text box, by a name, by a kind, and by words no artifact holds.
  const filter = control(controlRow(main), CONTROL.searchBox);
  await filter.fill("plan");
  await expect(rows, "the text box, by a name").toHaveCount(1);
  await expect(rows).toHaveAttribute("data-testid", `artifact-${plan.artifact.slug}`);
  await filter.fill("file");
  await expect(rows, "the text box, by a kind").toHaveCount(1);
  await expect(rows).toHaveAttribute("data-testid", "artifact-trace-log");
  await filter.fill("nothing holds this");
  await expect(rows).toHaveCount(0);
  await expect(
    main.getByText("No artifacts match this filter.", { exact: true }),
    "the text box's empty state"
  ).toBeVisible();
  await filter.fill("");
  await expect(rows).toHaveCount(4);

  // New document, which opens what it made; the tab then lists it.
  await main.getByRole("button", { exact: true, name: "New document" }).click();
  const form = page.getByRole("form", { name: "New document" });
  await form.getByRole("textbox", { exact: true, name: "Title" }).fill("Drafted");
  await form.getByRole("button", { exact: true, name: "Create" }).click();
  await expect(page, "New document").toHaveURL(
    new RegExp(`/issues/${issue.key}/artifacts/drafted`)
  );
  await page.goto(`/issues/${issue.key}/artifacts`);
  await expect(rows, "New document's row").toHaveCount(5);
  await expect(rows.nth(1), "New document's row").toContainText("Drafted");
});

// The Artifacts tab's words round-trip through the address: typed words are written to `?q=`,
// encoded where they must be, and read back after a reload; a link carrying them opens the tab
// narrowed; and typing adds no history entry, so Back leaves the tab.
// SLOW(>=2s): an issue seeded with two documents, the tab reloaded twice and opened by a link.
test("the Artifacts tab's words round-trip through ?q=, a reload, a link and Back", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Artifacts address" });
  const plan = await createIssueArtifact(issue.key, { content: "# Plan\n", name: "plan.md" });
  const notes = await createIssueArtifact(issue.key, { content: "# Notes\n", name: "notes.md" });
  await page.goto(`/issues/${issue.key}`);
  await expect(page.getByTestId("issue-header")).toBeVisible();
  await page.goto(`/issues/${issue.key}/artifacts`);
  const main = page.getByTestId("main-content");
  const rows = main.locator('[data-testid^="artifact-"]');
  await expect(main.getByTestId(`artifact-${plan.artifact.slug}`)).toBeVisible();
  const all = await rows.count();
  const filter = control(controlRow(main), CONTROL.searchBox);
  const q = () => new URL(page.url()).searchParams.get("q");

  await filter.fill("lan");
  await expect(rows, "lan narrows to the plan").toHaveCount(1);
  expect(q(), "the typed words in ?q=").toBe("lan");
  // A key typed at the start of the words lands there, and the caret stays where it was.
  await filter.press("Home");
  await filter.pressSequentially("p");
  await expect(filter, "a key typed at the caret").toHaveValue("plan");
  expect(q()).toBe("plan");
  await filter.pressSequentially("x");
  await expect(filter, "the caret after the typed key").toHaveValue("pxlan");
  await filter.press("Backspace");
  await expect(filter).toHaveValue("plan");

  await page.reload();
  await expect(filter, "the words after a reload").toHaveValue("plan");
  await expect(rows, "the narrowing after a reload").toHaveCount(1);
  await expect(rows).toHaveAttribute("data-testid", `artifact-${plan.artifact.slug}`);

  // Words that the address must encode come back whole.
  const odd = "a&b #c%25+d";
  await filter.fill(odd);
  expect(q(), "encoded words").toBe(odd);
  await page.reload();
  await expect(filter, "encoded words after a reload").toHaveValue(odd);
  await expect(main.getByText("No artifacts match this filter.", { exact: true })).toBeVisible();

  await filter.fill("");
  expect(q(), "an empty box leaves the address").toBeNull();
  await expect(rows).toHaveCount(all);

  // A link that carries the words opens the tab narrowed, whatever their case.
  await page.goto(`/issues/${issue.key}/artifacts?q=NOTES`);
  await expect(filter).toHaveValue("NOTES");
  await expect(rows).toHaveCount(1);
  await expect(rows).toHaveAttribute("data-testid", `artifact-${notes.artifact.slug}`);

  // Typing replaces the history entry: Back leaves the tab for the page before it.
  await filter.fill("note");
  await filter.fill("no");
  await page.goBack();
  await expect(page, "Back after typing").toHaveURL(new RegExp(`/issues/${issue.key}/artifacts$`));
});

/** The search bar's keep row's seed: four issues and a comment holding `harbour`, over two
 *  projects, two authors and two statuses; a project document holding it too, whose paragraph
 *  the writing cursor goes into; and twenty-two `beacon` issues, more than the bar lists at once
 *  (`searchDefaultLimit`, `packages/envoy/internal/dispatch/api/search.go`). */
async function seedKeptSearch(): Promise<{ documentSlug: string; rollout: string }> {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" }, { login: "bob" });
  const rollout = await createIssue({ project: "CORE", title: "Harbour rollout" });
  await createComment(rollout.key, { body: "The harbour tide decides the day." });
  const lights = await createIssue({ project: "CORE", title: "Harbour lights" });
  await patchIssue(lights.key, { status: "in_progress" });
  await createIssue({ project: "CORE", title: "Harbour pilot" }, { login: "bob" });
  await createIssue({ project: "OPS", title: "Harbour crane" }, { login: "bob" });
  const document = await createProjectDocument("CORE", {
    content: "# Harbour notes\n\nThe sextant sights the horizon.\n",
    name: "Harbour notes",
  });
  for (let index = 1; index <= 22; index++) {
    await createIssue({ project: "CORE", title: `Beacon ${index}` });
  }
  return { documentSlug: document.artifact.slug, rollout: rollout.key };
}

/** The owners the bar's hits are grouped under, each as its group is named: `KEY: title`. */
function hitOwners(bar: Locator): Promise<string[]> {
  return bar
    .locator(
      '#search-results [id^="search-group-issue:"], #search-results [id^="search-group-document:"]'
    )
    .evaluateAll((heads) => heads.map((head) => head.getAttribute("aria-label") ?? "").sort());
}

// R6 on the search bar. What the bar offered at the merge base, dccaf8369449
// (`fork_point(main@origin | @)`), paths under `web/src/`, and how the row drives each:
// - Fields: the words in the box (`features/search/SearchPalette.tsx:724-742`); Project
//   (`features/search/SearchFilterBar.tsx:149-156`), Status (:157-164), Author, taken on Enter
//   (:165-181), and Updated since (:182-198), each a removable chip once set (:137-148,
//   `features/search/search-filters.ts:65-89`) and each a parameter of the search (:91-104). Each
//   is set, the hits it leaves read, and its chip taken off again.
// - Sort: none to choose; the hits come in the order the search ranks them.
// - Grouping: the hits under the issue or document they belong to (`SearchPalette.tsx:472`,
//   :780-851, `features/search/search-model.ts:25-44`); the Recent, Actions and Projects groups
//   above them (:546-574), and More actions, This issue's agent and New issue below (:575-596).
//   The hits' groups, Recent, Actions, Projects and New issue are read. More actions holds what
//   the page's keys offer, and This issue's agent only a live session holding the issue.
// - Count: `Showing the best N of M matches` (:891-896). Read.
// - Views: the recent items while the box is empty, the hits while it holds words (:507-524).
//   Both read.
// - Bulk action: none.
// - Keys: in the box, ArrowDown and ArrowUp move the highlight (:685-700) and Enter opens it
//   (:701-704); Tab narrows the scope to the block being written in and Backspace on an empty
//   box widens it to the project (`features/search/useBarScope.tsx:72-90`); `$mod+k` shuts the
//   bar (`SearchPalette.tsx:433-441`) and so does Escape (`features/shell/useDialog.ts:124-137`);
//   and the three global bindings that open it, `$mod+k`, `/` and `g p` (`app.tsx:547-597`).
//   Each pressed and what it did read.
// - Stored for the reader: the recent items, in local storage under `search.recents`
//   (`features/search/recents.ts:26-87`). Found again after a reload.
// SLOW(>=2s): twenty-six seeded issues and a document, the bar opened six times over four pages.
test("the search bar keeps its narrowings, its groups, its count, its recent items and its keys @phone", async ({
  page,
}, testInfo) => {
  const phone = isPhoneProject(testInfo);
  if (!phone) await page.setViewportSize({ height: 900, width: 1440 });
  const seed = await seedKeptSearch();
  const bar = page.getByRole("dialog", { name: "Search" });
  const box = bar.getByRole("combobox", { name: "Search" });
  const scope = bar.getByTestId("search-scope");
  // Four issues, and the project document, which is grouped under its own name.
  const harbour = [
    "CORE-1: Harbour rollout",
    "CORE-2: Harbour lights",
    "CORE-3: Harbour pilot",
    "CORE: Harbour notes",
    "OPS-1: Harbour crane",
  ].sort();

  // The three keys that open the bar, and the two that shut it.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  for (const [keys, shut] of [
    [["/"], "Escape"],
    [["g", "p"], "Control+k"],
    [["Control+k"], "Escape"],
  ] as const) {
    await page.locator("body").focus();
    for (const key of keys) await page.keyboard.press(key);
    await expect(bar, `the ${keys.join(" ")} key`).toBeVisible();
    await expect(box).toBeFocused();
    await page.keyboard.press(shut);
    await expect(bar, `the ${shut} key in the bar`).toHaveCount(0);
  }

  // The words, and the hits grouped under what they belong to.
  await page.keyboard.press("Control+k");
  await box.fill("harbour");
  await expect.poll(() => hitOwners(bar), { message: "the words' hits" }).toEqual(harbour);
  await expect(
    bar.getByRole("group", { name: "CORE-1: Harbour rollout" }).getByRole("option"),
    "an issue's group, holding the issue and its comment"
  ).toHaveCount(2);
  if (phone) await assertPageFits(page);

  // The four narrowings, each from the one panel and each a chip.
  const chips = control(controlRow(bar), CONTROL.chip);
  const filters = page.getByRole("dialog", { name: "Filter" });
  const tomorrow = await page.evaluate(() => {
    const day = new Date();
    day.setDate(day.getDate() + 1);
    const pad = (value: number) => String(value).padStart(2, "0");
    return `${day.getFullYear()}-${pad(day.getMonth() + 1)}-${pad(day.getDate())}`;
  });
  const pick = async (list: string, option: string) => {
    await filters.getByRole("button", { exact: true, name: list }).click();
    await page
      .getByRole("listbox", { name: `${list} options` })
      .getByRole("option", { exact: true, name: option })
      .click();
  };
  for (const [narrowing, field, set, left] of [
    [
      "Project",
      filters.getByRole("button", { exact: true, name: "Project" }),
      () => pick("Project", "OPS"),
      ["OPS-1: Harbour crane"],
    ],
    [
      "Status",
      filters.getByRole("button", { exact: true, name: "Status" }),
      () => pick("Status", "In progress"),
      ["CORE-2: Harbour lights"],
    ],
    [
      "Author",
      filters.getByRole("searchbox", { name: "Author" }),
      async () => {
        await filters.getByRole("searchbox", { name: "Author" }).fill("bob");
        await page.keyboard.press("Enter");
      },
      ["CORE-3: Harbour pilot", "OPS-1: Harbour crane"],
    ],
    [
      "Updated since",
      filters.getByLabel("Updated since"),
      () => filters.getByLabel("Updated since").fill(tomorrow),
      [],
    ],
  ] as const) {
    await openFilterPanel(page, bar);
    await expect(field, `the ${narrowing} narrowing`).toBeVisible();
    await set();
    await closeControlPanel(bar);
    await expect(
      chips.filter({ hasText: new RegExp(`^${narrowing} `) }),
      `the ${narrowing} narrowing's chip`
    ).toHaveCount(1);
    await expect
      .poll(() => hitOwners(bar), { message: `the ${narrowing} narrowing` })
      .toEqual([...left].sort());
    await chips.click();
    await expect(chips).toHaveCount(0);
    await expect.poll(() => hitOwners(bar)).toEqual(harbour);
  }

  // The count of what the bar holds back, and the projects the words name.
  await box.fill("beacon");
  await expect(
    bar.getByText("Showing the best 20 of 22 matches", { exact: true }),
    "the count of matches"
  ).toBeVisible();
  await box.fill("ops");
  await expect(
    bar.getByRole("group", { name: "Projects" }).getByRole("option"),
    "the Projects group"
  ).toContainText(["Ops"]);

  // The arrows move the highlight over the hits, and Enter opens the one it is on.
  await box.fill("harbour");
  await expect.poll(() => hitOwners(bar)).toEqual(harbour);
  const first = await box.getAttribute("aria-activedescendant");
  await page.keyboard.press("ArrowDown");
  await expect(box, "the ArrowDown key").not.toHaveAttribute("aria-activedescendant", first ?? "");
  await page.keyboard.press("ArrowUp");
  await expect(box, "the ArrowUp key").toHaveAttribute("aria-activedescendant", first ?? "");
  await page.keyboard.press("Enter");
  await expect(bar, "the Enter key").toHaveCount(0);
  await expect(page, "the Enter key").toHaveURL(/\/(issues|projects)\//);

  // Over an issue: the page's actions on an empty box, Backspace widening the scope from the
  // issue to its project, and the new issue the bar offers words nothing holds, which it files
  // in the page's project.
  await page.goto(`/issues/${seed.rollout}`);
  await expect(page.getByTestId("issue-header")).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(scope).toHaveText(new RegExp(`Acting on ${seed.rollout}`));
  await expect(
    bar.getByRole("group", { name: "Actions" }).getByRole("option"),
    "the Actions group"
  ).not.toHaveCount(0);
  await page.keyboard.press("Backspace");
  await expect(scope, "the Backspace key").toHaveText(/Acting on the project CORE/);
  await box.fill("zebrafish");
  await expect(
    bar.getByRole("group", { name: "New issue" }).getByRole("option", {
      name: /Make a new issue from .zebrafish./,
    }),
    "the New issue group"
  ).toHaveCount(1);
  await page.keyboard.press("Escape");

  // Tab narrows it to the block the writing cursor is in.
  await page.goto(`/projects/CORE/documents/${seed.documentSlug}`);
  await expect(documentEditor(page)).toContainText("The sextant sights the horizon.");
  await placeCaret(page, "after", "sextant");
  await expect(documentEditor(page)).toBeFocused();
  await page.keyboard.press("Control+k");
  await expect(scope).toHaveText(/Harbour notes/);
  await page.keyboard.press("Tab");
  await expect(scope, "the Tab key").toHaveText(/The sextant sights the horizon/);
  await page.keyboard.press("Escape");

  // The recent items, kept in the browser and listed again after a reload.
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(box).toHaveValue("");
  const recent = bar.getByRole("group", { name: "Recent" });
  await expect(
    recent.getByRole("option", { name: new RegExp(seed.rollout) }),
    "a recent issue, after a reload"
  ).toHaveCount(1);
  await expect(
    recent.getByRole("option", { name: /Harbour notes/ }),
    "a recent document, after a reload"
  ).toHaveCount(1);
  expect(
    await page.evaluate(() => localStorage.getItem("dispatch.search.recents:alice")),
    "the recent items' stored value"
  ).toContain(`/issues/${seed.rollout}`);
});

/** How many frames running a table's box or a panel holds still before its `scrollTop` is read
 *  (`settledScrollTop`): a wheel step's scroll lands over frames, and every hand-off of a loss
 *  runs within a frame and a task of it, so a box pulled back shows within three. */
const WHEEL_STILL_FRAMES = 3;

/** The row holding the keyboard focus, by the value of `rowAttribute` it carries, or null where
 *  the focus is in no such row. */
async function focusedRowOf(page: Page, rowAttribute: string): Promise<string | null> {
  return page.evaluate(
    (attribute) =>
      document.activeElement?.closest(`[${attribute}]`)?.getAttribute(attribute) ?? null,
    rowAttribute
  );
}

// SLOW(>=2s): 120 issues and 120 pull requests seeded, and two tables each scrolled by 25 wheel
// steps with a settled read after each.
// `@firefox` and `@webkit`: a focused row a virtual table takes away fires no `focusout` in these
// engines, and each engine lands a wheel step's scroll over its own frames.
// INFRA-3135's review round 1 (thread PRRT_kwDORFy7ds63p6y_): with a row's checkbox focused, 20
// wheel steps over a virtual table pulled its box back from 690 px to 24 px and moved the focus
// onto another row. The reader's scroll is theirs: the box goes where the wheel takes it. The row
// they picked leaves with the drawn window and takes the focus with it, and nothing hands the
// focus to a row they did not pick, so `x` then marks none.
test("a table that holds the keyboard focus scrolls freely, and the focus moves to no row the reader did not pick @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(150_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  const main = page.getByTestId("main-content");

  /** Sends 25 wheel steps of 100 px with the pointer over `box`, reads its `scrollTop` once each
   *  has settled, and answers those reads with what went wrong: a read more than 1 px above the
   *  one before it, the focus in any row but `picked`, and a box that ended less than 1,500 px
   *  below where it started. It answers where the focus ended too. */
  const wheelOver = async (box: Locator, rowAttribute: string, picked: string) => {
    await box.scrollIntoViewIfNeeded();
    // `scrollIntoViewIfNeeded` waited above for the box to be visible and stable, so it has a box.
    const area = await box.boundingBox();
    if (area === null) throw new Error("the table's box is not on screen");
    // Near the box's left edge, in the first cell's padding, where no row draws a link: a pointer
    // resting on a row's link opens that link's hover card, which takes the wheel steps after it
    // for itself. Measured in WebKit with the pointer at the box's middle: the box stopped at
    // 400 px for the last 20 steps.
    await page.mouse.move(area.x + 4, area.y + area.height / 2);
    const readings = [await settledScrollTop(box, WHEEL_STILL_FRAMES)];
    const wrong: string[] = [];
    for (let step = 1; step <= 25; step++) {
      await page.mouse.wheel(0, 100);
      // `settledScrollTop` waits until the box held still for three frames: that is the reading.
      const top = await settledScrollTop(box, WHEEL_STILL_FRAMES);
      const before = readings.at(-1) ?? 0;
      if (top < before - 1) wrong.push(`step ${step} pulled the box back from ${before} to ${top}`);
      readings.push(top);
      // Read after the box held still for three frames, by which every hand-off of a loss has run
      // (`WHEEL_STILL_FRAMES`), so the focus read is where the step left it.
      const focused = await focusedRowOf(page, rowAttribute);
      if (focused !== null && focused !== picked) {
        wrong.push(`after step ${step} the focus is in ${focused}`);
      }
    }
    const moved = (readings.at(-1) ?? 0) - (readings[0] ?? 0);
    if (moved < 1_500) wrong.push(`the box ended ${moved} px below where it started`);
    const ended = await page.evaluate(() => document.activeElement?.tagName ?? "nothing");
    return { ended, readings, wrong };
  };

  // The project List, on one page of 120 issues, far more than its box draws. Each title differs
  // from every other in a word of its own, since the server refuses a near copy of a title it
  // holds (409 POSSIBLE_DUPLICATE).
  await createProject({ key: "CORE", name: "Core" });
  for (let batch = 0; batch < 120; batch += 10) {
    await Promise.all(
      Array.from({ length: 10 }, (_, offset) =>
        createIssue({ project: "CORE", title: `Scrolled row ${batch + offset + 1}` })
      )
    );
  }
  await openIssuesTab(page, "CORE", "List");
  const pager = control(main, CONTROL.pager);
  await expect(pager).toContainText("120 of 120 issues");
  const perPage = listTrigger(pager, "Rows per page");
  await pickFromList(perPage, "250 per page");
  await expect(page).toHaveURL(/[?&]size=250/);
  const listBox = control(main, CONTROL.dataTable);
  const listRows = listBox.locator("[data-issue-row]");
  // `getAttribute` waits for the third row to attach, and a row carries its key from its first
  // render, so the key read is that row's own.
  const picked = (await listRows.nth(2).getAttribute("data-issue-row")) ?? "";
  const pickedBox = listBox.getByRole("checkbox", { exact: true, name: `Select ${picked}` });
  await pickedBox.click();
  const bar = control(main, CONTROL.selectionBar);
  await expect(bar).toContainText("1 selected");
  expect(await focusedRowOf(page, "data-issue-row"), "the click left the focus in no row").toBe(
    picked
  );
  const list = await wheelOver(listBox, "data-issue-row", picked);
  console.log(
    `project List, ${picked}'s checkbox clicked: ${list.readings.join(" ")}; the focus ends on ${list.ended}`
  );
  expect(list.wrong, `project List: ${list.wrong.join("; ")}`).toEqual([]);
  // `x` marks no row the reader did not pick: the row they marked is still the one marked, and
  // the only one, once the box is back at its top.
  await page.keyboard.press("x");
  await listBox.evaluate((node) => {
    node.scrollTop = 0;
  });
  await expect(pickedBox).toBeChecked();
  await expect(bar).toContainText("1 selected");

  // The Delivery list, over the delivery fixture and 120 more pull requests merged in its
  // window. A press on one of its rows opens that row's details, so the row takes the focus as
  // its keys give it one. It marks nothing, so its scroll and its focus are all there is to read.
  await seedDeliveryFixture();
  await sql(
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     )
     SELECT 'acme/widgets', 100 + n, 'feat: scrolled widget ' || n,
            'https://github.com/acme/widgets/pull/' || (100 + n), 'octocat',
            timestamptz '2024-06-01T04:00:00Z' + n * interval '1 minute',
            timestamptz '2024-06-01T06:00:00Z' + n * interval '1 minute',
            1, 1, false, null, ARRAY[]::text[], false
       FROM generate_series(1, 120) AS n`
  );
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  const prBox = control(main, CONTROL.dataTable);
  const pickedRow = prBox.locator("tbody tr[data-pr-row]").nth(2);
  await expect(pickedRow).toBeVisible();
  const pickedPR = (await pickedRow.getAttribute("data-pr-row")) ?? "";
  await pickedRow.focus();
  const delivery = await wheelOver(prBox, "data-pr-row", pickedPR);
  console.log(
    `Delivery list, ${pickedPR} focused: ${delivery.readings.join(" ")}; the focus ends on ${delivery.ended}`
  );
  expect(delivery.wrong, `Delivery list: ${delivery.wrong.join("; ")}`).toEqual([]);
});

// SLOW(>=2s): the Delivery page opened four times, in each view, at two widths.
// INFRA-3135's review round 1 (threads PRRT_kwDORFy7ds63p6zB and PRRT_kwDORFy7ds63p6zE): on the
// Timeline, the view the page opens on, the row's panel held four of the nine facets and the row
// drew no chip or count for the other five, which `/delivery?author=x` still narrowed by; a
// value picked on a column's header in the List drew no chip in the row; and the List's arrows
// were answered while the list held any row, so ArrowDown with the focus on the page's body
// moved the focus into the list instead of scrolling the page.
test("the Delivery page reaches every facet from its row, in both views, and its list answers keys only on a focused row", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  // The delivery fixture with its reconcile verdict pinned, as `seedRealStackSurfaces` pins it,
  // and one more pull request merged in its window: another author's, written by a session, so
  // taking the Author chip off gives a pull request back and the Session facet has a value.
  await seedDeliveryFixture();
  await sql(
    `UPDATE delivery_settings
       SET last_error = 'the GitHub App is not installed on acme/widgets'
     WHERE singleton`,
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES
       ('acme/widgets', 3, 'feat: a hubot widget', 'https://github.com/acme/widgets/pull/3', 'hubot',
        '2024-06-01T04:00:00Z', '2024-06-01T05:00:00Z', 2, 1, false, null, ARRAY['e2e-delivery-row'], false)`
  );
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const trigger = control(row, CONTROL.panelTrigger);
  const shown = main.getByText(/^\d+ PRs in current filter\/window$/);

  // A link naming one of the five column facets, on the Timeline: its chip and its count are in
  // the row, and the chip takes it off.
  await page.goto(`/delivery?${DELIVERY_WINDOW}&author=octocat`);
  await expect(main.getByRole("img", { name: /^Delivery timeline/ })).toBeVisible();
  await expect(shown).toHaveText("2 PRs in current filter/window");
  const octocat = row.getByRole("button", { exact: true, name: "Remove Author: octocat filter" });
  await expect(octocat, "the Timeline draws no chip for ?author=").toBeVisible();
  await expect(trigger).toHaveText("Filter · 1");
  await octocat.click();
  await expect(shown).toHaveText("3 PRs in current filter/window");
  await expect(page).not.toHaveURL(/[?&]author=/);
  await expect(trigger).toHaveText("Filter · 0");

  // No column header is drawn on the Timeline, so its panel holds all nine.
  const panel = await openFilterPanel(page, main);
  for (const facet of [
    "Repository",
    "Session",
    "Priority",
    "Component",
    "Issue",
    "Parent agent",
    "Author",
    "Rework",
    "Deployed",
  ]) {
    await expect(
      panel.getByRole("button", { name: new RegExp(`^${facet}: `) }),
      `the Timeline's panel offers no ${facet}`
    ).toHaveCount(1);
  }
  await closeControlPanel(main);

  // In the List, Author is filtered on its column's header, and what it picks is a chip of the
  // row, whose press takes it off the header too.
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  const prs = page.locator("tbody tr[data-pr-row]");
  await expect(prs).toHaveCount(3);
  await main.getByRole("button", { exact: true, name: "Filter Author" }).click();
  await page
    .getByRole("listbox", { name: "Author options" })
    .getByRole("option", { name: /^hubot/ })
    .click();
  await page.keyboard.press("Escape");
  await expect(prs).toHaveCount(1);
  const hubot = row.getByRole("button", { exact: true, name: "Remove Author: hubot filter" });
  await expect(hubot, "the List draws no chip for its Author column's filter").toBeVisible();
  await expect(trigger).toHaveText("Filter · 1");
  await hubot.click();
  await expect(prs).toHaveCount(3);
  await expect(main.getByRole("button", { exact: true, name: "Filter Author" })).toBeVisible();

  // Below `xl` the list's box is three quarters of the window under the page's measures, so the
  // page scrolls. With the focus on the body, ArrowDown is the page's: it scrolls, and no row
  // takes the focus.
  await page.setViewportSize({ height: 768, width: 1024 });
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  await expect(prs).toHaveCount(3);
  await focusThePage(page);
  expect(await page.evaluate(() => window.scrollY)).toBe(0);
  await page.keyboard.press("ArrowDown");
  expect(
    await focusedRowOf(page, "data-pr-row"),
    "ArrowDown with the focus on the body put it in a row"
  ).toBeNull();
  await expect
    .poll(() => page.evaluate(() => window.scrollY), {
      message: "ArrowDown with the focus on the body did not scroll the page",
    })
    .toBeGreaterThan(0);
});

// INFRA-3135's review round 1, F2 item 1 (the architect's chip rule, restated 2026-11-15): a
// column filter showed only on its column's header, and the columns menu hides any column, so a
// reader could hide the column a link had narrowed by and be left with a list narrowed by a
// filter they could neither see nor take off. Every narrowing that is on is a chip of the row,
// whatever set it, and the row's trigger counts it.
test("a column filter is visible and removable from the row even while its column is hidden", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  for (const [title, label] of [
    ["Ledger rollout", "frontend"],
    ["Mirror retries", "backend"],
  ] as const) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { labels: [label] });
  }
  const main = page.getByTestId("main-content");
  const row = controlRow(main);
  const trigger = control(row, CONTROL.panelTrigger);
  const rows = page.locator("[data-issue-row]");
  const chip = row.getByRole("button", { exact: true, name: "Remove Label: frontend filter" });

  // A link narrowing by a label: one row, one chip, and the trigger counts it.
  await openIssuesTab(page, "CORE", "List");
  await page.goto("/projects/CORE/issues?label=frontend");
  await expect(rows).toHaveCount(1);
  await expect(chip).toBeVisible();
  await expect(trigger).toHaveText("Filters · 1");

  // The reader hides the Labels column: its header, and the filter control on it, leave the
  // page, and the narrowing stays on.
  await control(row, CONTROL.columnsMenu).click();
  await page.getByRole("checkbox", { exact: true, name: "Labels" }).click();
  await page.keyboard.press("Escape");
  await expect(main.getByRole("button", { name: /^Filter Labels/ })).toHaveCount(0);
  await expect(rows).toHaveCount(1);

  // The chip is the one place that narrowing is now, and its press takes it off.
  await expect(chip, "no chip is left for a filter whose column is hidden").toBeVisible();
  await expect(trigger).toHaveText("Filters · 1");
  await chip.click();
  await expect(rows).toHaveCount(2);
  await expect(page).not.toHaveURL(/[?&]label=/);
  await expect(trigger).toHaveText("Filters · 0");
  await expect(chip).toHaveCount(0);
});

// SLOW(>=2s): 60 issues seeded, the List opened, a middle row marked and unmarked and then closed
// by a second person through the API.
// INFRA-3135's review round 1, follow-up F4 (thread PRRT_kwDORFy7ds63p6y_): a focused row a live
// update removes hands the focus to no other row and scrolls nothing, though the reader's last
// press was on that row: a press is the cause only of the update that answers it, and the
// reader's two clicks on the row's checkbox have been answered, the selection bar drawn and gone
// again, before the second person's close arrives. `@firefox` and `@webkit`: a focused row taken
// away fires no `focusout` in these engines, the clause of `playwright.config.ts` on a focused
// control taken away.
test("a row a live update removes while it holds the focus hands the focus to no other row and scrolls nothing @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  // More rows than the box draws, each title its own word, since the server refuses a near copy of
  // a title it holds (409 POSSIBLE_DUPLICATE).
  for (let batch = 0; batch < 60; batch += 10) {
    await Promise.all(
      Array.from({ length: 10 }, (_, offset) =>
        createIssue({ project: "CORE", title: `Removed row ${batch + offset + 1}` })
      )
    );
  }
  // The List opens on the open statuses and not done (`openIssueStatuses`), so closing an issue as
  // another person takes its row off this view; the List view is opened first, since a project
  // opens on whichever view the reader last left.
  await openIssuesTab(page, "CORE", "List");
  const main = page.getByTestId("main-content");
  const listBox = control(main, CONTROL.dataTable);
  await expect(listBox.locator("[data-issue-row]").first()).toBeVisible();
  // Scroll the box so a row in the middle is in sight, and pick it.
  await listBox.evaluate((node) => {
    node.scrollTop = Math.round(node.scrollHeight / 2);
  });
  await settledScrollTop(listBox, WHEEL_STILL_FRAMES);
  // Read after the box held still for three frames above, so the rows in sight are settled.
  const picked = await listBox.evaluate((node) => {
    const within = node.getBoundingClientRect();
    const mid = [...node.querySelectorAll<HTMLElement>("[data-issue-row]")].find((row) => {
      const box = row.getBoundingClientRect();
      return box.top >= within.top + 40 && box.bottom <= within.bottom - 40;
    });
    return mid?.dataset.issueRow ?? null;
  });
  if (picked === null) throw new Error("no row sits wholly in the middle of the box");
  const pickedBox = listBox.getByRole("checkbox", { exact: true, name: `Select ${picked}` });
  const bar = control(main, CONTROL.selectionBar);
  // The reader presses in the row itself: a click on its checkbox marks it, and a second click
  // unmarks it. WebKit leaves a clicked checkbox unfocused, so the focus is then put on it with no
  // press, which leaves it there in every engine alike.
  await pickedBox.click();
  await expect(bar).toBeVisible();
  await pickedBox.click();
  await pickedBox.evaluate((node) => (node as HTMLElement).focus({ preventScroll: true }));
  await expect(pickedBox).toBeFocused();
  // The second click's update is drawn - the selection bar gone - before the close is sent, so
  // that press has been answered by the time the live update takes the row away.
  await expect(bar).toHaveCount(0);
  const beforeTop = await listBox.evaluate((node) => node.scrollTop);
  const beforeY = await page.evaluate(() => window.scrollY);
  // A second person closes the issue through the API; the live update takes its row off the view.
  await patchIssue(picked, { status: "done" }, { login: "bob" });
  await expect(page.locator(`[data-issue-row="${picked}"]`)).toHaveCount(0);
  // The focus is in no row, and nothing scrolled.
  const inRow = await page.evaluate(
    () => document.activeElement?.closest("[data-issue-row]") != null
  );
  const afterTop = await listBox.evaluate((node) => node.scrollTop);
  const afterY = await page.evaluate(() => window.scrollY);
  expect(inRow, "the focus landed in a row the reader did not pick").toBe(false);
  expect(Math.abs(afterTop - beforeTop), "the box scrolled").toBeLessThanOrEqual(1);
  expect(Math.abs(afterY - beforeY), "the window scrolled").toBeLessThanOrEqual(1);
  // `x` marks nothing: no row holds the focus for it to act on, so no bar appears.
  await page.keyboard.press("x");
  await expect(bar).toHaveCount(0);
});

// SLOW(>=2s): the search bar and its Filter panel opened twice, for a typed date and a set one.
// The Filter panel draws its Updated since box only while no date is set (`SearchPalette.tsx`),
// and the box takes a typed date once its year has four digits, so the reader's own date takes
// the focused box away. A date set or typed in the box is the reader's press, whether the engine
// fires `change` before the page drops the box (Chromium, for a typed date) or `input` alone
// (Firefox, as the last digit completes the date, and Playwright's fill(), the value set without
// keys as assistive technology sets it). A caused walk-back passes over Author, which takes typed
// text, so the reader's date keeps the focus inside the Filter panel on no text field; a loss
// misread as uncaused leaves the focus on the page's body, outside that panel. WebKit's date box
// takes no typed date from Playwright's keys, so the typed case is not read there. A focused
// control taken away, for which Firefox and WebKit fire no `focusout`: the clause of the `webkit`
// and `firefox` projects that names the page-own focus row (e2e/playwright.config.ts).
test("a date the reader gives the search bar's Updated since is their press, so the focus stays in the Filter panel @firefox @webkit", async ({
  browserName,
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Harbour lights" });
  await page.setViewportSize({ height: 900, width: 1440 });
  const wrong: string[] = [];
  for (const how of ["typed", "set"] as const) {
    if (how === "typed" && browserName === "webkit") continue;
    await page.goto("/");
    await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
    await page.keyboard.press("Control+k");
    const bar = page.getByRole("dialog", { name: "Search" });
    await expect(bar).toBeVisible();
    await page.keyboard.type("Harbour");
    await control(controlRow(bar), CONTROL.panelTrigger).click();
    const filters = page.getByRole("dialog", { name: "Filter" });
    const since = filters.getByLabel("Updated since");
    await expect(since).toBeVisible();
    const day = await page.evaluate(() => {
      const next = new Date();
      next.setDate(next.getDate() + 1);
      const pad = (value: number) => String(value).padStart(2, "0");
      return {
        iso: `${next.getFullYear()}-${pad(next.getMonth() + 1)}-${pad(next.getDate())}`,
        typed: `${pad(next.getMonth() + 1)}${pad(next.getDate())}${next.getFullYear()}`,
      };
    });
    if (how === "typed") {
      await since.focus();
      await page.keyboard.type(day.typed);
    } else {
      await since.fill(day.iso);
    }
    await expect(since).toHaveCount(0);
    // Every hand-off of the loss runs within a frame and a task of it, so it is done by now.
    await nextFrames(page);
    await nextFrames(page);
    // The answer is read now: every hand-off of the loss has run, and nothing else moves the focus.
    const holder = await focusHolder(page);
    // Read after the same hand-offs as the holder above, so where the focus is is settled.
    const inFilter = await filters.evaluate((panel) => panel.contains(document.activeElement));
    if (!inFilter)
      wrong.push(`a ${how} date: the focus is on ${holder.name}, outside the Filter panel`);
    else if (holder.takesText)
      wrong.push(`a ${how} date: the focus is in ${holder.name}, which takes text`);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the Session groups dialog opened at two widths, a group made and renamed in each.
// INFRA-3135, round 22 (the tester's, measured in builds Jobs infra-3135-p1-h and -p1-b at
// da0804df and a1c78223): the reader's focus is on a session group's Rename in the Session groups
// dialog, and Enter renames the group. The card holds its buttons while the write is out and
// until the live update carrying the rename has drawn the group again in place
// (`useHoldUntilDrawn`), and holds them by `aria-disabled` (`heldPress`), so Rename keeps the focus
// the reader's press left on it once the write has landed, at 1440 and 390 px
// (`features/delivery/SessionGroupsEditor.tsx`). A control held under the reader's press, which a
// native `disabled` takes the focus from: the clause of the `webkit` and `firefox` projects that
// names the page-own focus row (e2e/playwright.config.ts).
test("a group card's Rename by Enter keeps the keyboard focus once its write lands, in the Session groups dialog @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(90_000);
  // Once: the fixture's rows carry no ON CONFLICT. Each width groups a session of its own, since
  // a session one group holds is free for no other.
  await seedSessionGroupsFixture();
  for (const [height, width, session] of [
    [900, 1440, "Planner, first run"],
    [664, 390, "Planner, second run"],
  ] as const) {
    const name = `Crew at ${width}`;
    await page.setViewportSize({ height, width });
    await page.goto("/delivery?mode=list");
    await expect(page.getByText("3 PRs in current filter/window")).toBeVisible();
    await openFilterPanel(page);
    await page.getByRole("button", { name: "Edit groups" }).click();
    const editor = page.getByRole("dialog", { name: "Session groups" });
    await editor.getByRole("checkbox", { exact: true, name: session }).check();
    await editor.getByLabel("New group name").fill(name);
    await editor.getByRole("button", { name: "Create group" }).click();
    const card = editor.getByRole("group", { name });
    // Every button of a card is disabled while a write of the editor is out, and Delete group
    // by nothing else, so its coming back is the create's end.
    await expect(card.getByRole("button", { name: "Delete group" })).toBeEnabled();
    const renamed = `${name}, renamed`;
    await card.getByLabel("Name", { exact: true }).fill(renamed);
    await card.getByRole("button", { exact: true, name: "Rename" }).focus();
    await page.keyboard.press("Enter");
    const drawn = editor.getByRole("group", { name: renamed });
    // The card's hold ends once the live update carrying the rename has drawn it.
    await expect(
      drawn.getByRole("button", { name: "Delete group" }),
      `${width} px: the rename was never drawn`
    ).toBeEnabled();
    await expect(
      drawn.getByRole("button", { exact: true, name: "Rename" }),
      `${width} px: Rename lost the keyboard focus once its write landed`
    ).toBeFocused();
  }
});

// SLOW(>=2s): a save whose re-read is held offline until the 30 s request deadline ends its hold.
// INFRA-3135, round 22 (the tester's, measured in builds Jobs infra-3135-p1-h and -p1-b at
// da0804df and a1c78223): at 1440 px the reader presses Enter on Save delivery settings, the
// save lands, and the browser is offline before the page reads its answer, so the re-read that
// draws the stored record is one TanStack pauses. Save is disabled from the press, so the focus
// falls to the body. At da0804df the hold ends at the request deadline, 30 s after the save's
// answer, but the focus rescue those commits had hands the focus on at 30.03 s, before Save is
// enabled again, to the Preview section's New issue page checkbox, so Save gets nothing back and
// the reader's next Space toggles that setting. At a1c78223 the rescue moves the focus to the
// same checkbox at 30.07 s, with Save still held after 45 s. The architect's rulings of
// 2026-11-20 20:3xZ (open-items.md, "Review round 3"): the form keeps its identity through a
// save, so Save keeps the reader's focus, and every hold ends at a timer of its own while the
// browser is offline too. No engine tag: where the focus rests is decided by the hold's timer, in
// the page's own code, so another engine would repeat what Chromium shows.
test("Save delivery settings keeps the keyboard focus through a save whose re-read the browser cannot send", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await seedDeliveryFixture();
  // The save proves the GitHub App can read the deploy repository; the fake answers it.
  await seedFakeGithub({ "acme/widgets": { contents: "read", installation_id: 301 } });
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto("/settings");
  const save = page.getByRole("button", { name: "Save delivery settings" });
  await expect(save).toBeEnabled();
  await page.getByLabel("Deploy workflow").fill(".github/workflows/deploy-two.yml");
  const context = page.context();
  // The save reaches Dispatch and lands. The browser is offline before the page has its answer,
  // so the re-read the answer starts is one the browser cannot send.
  await page.route("**/api/v1/settings/delivery", async (route) => {
    if (route.request().method() !== "PUT") return route.fallback();
    const response = await route.fetch();
    await context.setOffline(true);
    await page.waitForFunction(() => !navigator.onLine);
    await route.fulfill({ response });
  });
  try {
    await save.focus();
    // A log, not one instant's read: every element the focus enters after the press.
    const entered = await save.evaluateHandle((button) => {
      const log: string[] = [];
      document.addEventListener(
        "focusin",
        (event) => {
          const target = event.target;
          if (target === button) {
            log.push("Save");
            return;
          }
          if (!(target instanceof HTMLElement)) return;
          const label = target instanceof HTMLInputElement ? target.labels?.[0]?.textContent : null;
          const named = target.getAttribute("aria-label") || label || target.textContent || "";
          log.push(`${target.tagName.toLowerCase()} "${named.trim().slice(0, 40)}"`);
        },
        true
      );
      return log;
    });
    await page.keyboard.press("Enter");
    // The hold's own contract is the request deadline, 30 s from the save's answer
    // (REQUEST_DEADLINE_MS, web/src/api/client.ts); 45 s holds it with room to spare.
    await expect(save).toBeEnabled({ timeout: 45_000 });
    // Every hand-off runs within a frame and a task of the commit that drew Save again, so it is
    // done by now.
    await nextFrames(page);
    await nextFrames(page);
    const wrong: string[] = [];
    // One read is the answer: Save is enabled again, and nothing else moves the focus now.
    const onSave = await save.evaluate((button) => button === document.activeElement);
    // Read after the same hand-offs as the read above, so where the focus is is settled.
    const holder = await focusHolder(page);
    if (!onSave) wrong.push(`1440 px, offline re-read: the focus is on ${holder.name}, not Save`);
    const others = (await entered.jsonValue()).filter((name) => name !== "Save");
    if (others.length > 0)
      wrong.push(
        `1440 px, offline re-read: after the press the focus entered ${others.join(", ")}`
      );
    expect(wrong, wrong.join("; ")).toEqual([]);
  } finally {
    await context.setOffline(false);
  }
});

// SLOW(>=2s): the Delivery page opened eight times, at two widths, by two presses, with its Window
// control scrolled wholly out of sight and left wholly in sight.
// INFRA-3135, round 22 (the tester's, measured in builds Jobs infra-3135-p3g-h and -p3g-b at
// da0804df and a1c78223, and this row in -p3-red-h at da0804df): with a brush window set - the
// address's `ws` and `we`, as a drag along the chart's time axis sets them - the Delivery page's
// header draws "clear brush window" in its generated line. Its press takes the button away with
// the brush, and the header's hand-off (`useFocusPlace` in DeliveryPage.tsx), finding no other
// control in that line, focuses the Window control on the line above with the browser's own
// scroll. With the page scrolled so that the select stands wholly above the window and the button
// wholly in it, the page scrolls back to the select, by Enter and by a click. With the select's
// foot 8 px above the window it scrolls 153 px in Chromium and Firefox and 52 px in WebKit at
// 390, and 62, 62 and 68 px at 1440, at da0804df and at a1c78223 alike; with its foot 2 px above,
// as this row places it, 147, 147 and 46 px at 390 and 56, 56 and 62 px at 1440 at da0804df, in
// both runs of each. The architect's ruling of 2026-11-21 12:25Z: a walk-back never scrolls to
// reach a control out of sight; it passes over those and lands on the region's own stop where
// none is in sight, and only a landing partly in sight may be scrolled, the least distance that
// shows it whole. With the select wholly in sight the focus lands on it and nothing scrolls. A
// focused control taken away, for which Firefox and WebKit fire no `focusout`, and each engine's
// own scroll of a focused control into view: the clause of the `webkit` and `firefox` projects
// that names the page-own focus row (e2e/playwright.config.ts).
test("clear brush window pressed with the Window control scrolled out of sight scrolls the Delivery page nothing and hands the focus to no control out of sight @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await seedDeliveryFixture();
  // A brush window inside the fixture's day, as a drag along the chart's time axis sets it.
  const brushed = `/delivery?${TIMELINE_FIXTURE_QUERY}&ws=2024-06-01T06%3A00%3A00Z&we=2024-06-01T18%3A00%3A00Z`;
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const placed of ["out of sight", "in sight"] as const) {
      for (const press of ["Enter", "click"] as const) {
        const at = `${size.width}x${size.height}, the Window control ${placed}, by ${press}`;
        await page.goto(brushed);
        const header = page.locator("header", {
          has: page.getByRole("heading", { level: 1, name: "Delivery timeline" }),
        });
        const clear = header.getByRole("button", { exact: true, name: "clear brush window" });
        const windowSelect = listTrigger(header, "Window");
        await expect(clear, `${at}: the brush drew no clear brush window`).toBeVisible();
        await expect(page.getByTestId("delivery-chart")).toBeVisible();
        /** Marks the box the page scrolls in, as the reader's scroll finds it: from `xl` up the
         *  page scrolls inside its own section where it overflows the window (main's #4631, the
         *  section's `xl:overflow-y-auto` in DeliveryPage.tsx), else the document. Answers its
         *  height, and whether it is the document. The reads below find the box by its mark. */
        const markScrollBox = () =>
          windowSelect.evaluate((node) => {
            for (const marked of document.querySelectorAll("[data-case-scroller]")) {
              marked.removeAttribute("data-case-scroller");
            }
            let scroller: Element = document.scrollingElement ?? document.documentElement;
            for (let box = node.parentElement; box !== null; box = box.parentElement) {
              const { overflowY } = getComputedStyle(box);
              if (
                (overflowY === "auto" || overflowY === "scroll") &&
                box.scrollHeight > box.clientHeight
              ) {
                scroller = box;
                break;
              }
            }
            scroller.setAttribute("data-case-scroller", "");
            return {
              document: scroller === document.scrollingElement,
              height: scroller.scrollHeight,
            };
          });
        // The page is drawn whole below the header, so it scrolls as far as the case needs: the
        // box it scrolls in, and that box's height, hold across two frames. The section overflows
        // only once the chart below has drawn, so the box is found again each time.
        await expect
          .poll(async () => {
            const was = await markScrollBox();
            await nextFrames(page);
            const now = await markScrollBox();
            return was.document === now.document && was.height === now.height;
          })
          .toBe(true);
        // The reader's own scroll, of that box: the select's foot 2 px above the box's visible
        // top, the button on the line under it wholly in sight; or the select's top 24 px under
        // that top. Read with the window's own scroll, so that a hand-off that scrolls either one
        // is caught at every width, as the row read the page before main's #4631.
        const before = await windowSelect.evaluate((node, out) => {
          const scroller =
            document.querySelector("[data-case-scroller]") ??
            document.scrollingElement ??
            document.documentElement;
          const sightTop =
            scroller === document.scrollingElement ? 0 : scroller.getBoundingClientRect().top;
          const box = node.getBoundingClientRect();
          scroller.scrollTop += out ? box.bottom - sightTop + 2 : box.top - sightTop - 24;
          return {
            box: Math.round(scroller.scrollTop),
            document: scroller === document.scrollingElement,
            window: Math.round(window.scrollY),
          };
        }, placed === "out of sight");
        /** The top of what the reader sees of the page: the marked scroll box's, or the window's. */
        const sightTop = await page.evaluate(() => {
          const scroller = document.querySelector("[data-case-scroller]");
          return scroller === null || scroller === document.scrollingElement
            ? 0
            : Math.round(scroller.getBoundingClientRect().top);
        });
        const select = await windowSelect.evaluate((node) => {
          const box = node.getBoundingClientRect();
          return { bottom: Math.round(box.bottom), top: Math.round(box.top) };
        });
        // One read places the case: the reader's scroll above is synchronous, and nothing else draws.
        const button = await clear.evaluate((node) => {
          const box = node.getBoundingClientRect();
          return { bottom: Math.round(box.bottom), top: Math.round(box.top) };
        });
        const selectAsPlaced =
          placed === "out of sight"
            ? select.bottom < sightTop
            : select.top >= sightTop && select.bottom <= size.height;
        if (!selectAsPlaced || button.top < sightTop || button.bottom > size.height) {
          wrong.push(
            `${at}: the page could not be scrolled to the case (scroll box ${before.box}, scrollY ${before.window}, select ${select.top}..${select.bottom} px, button ${button.top}..${button.bottom} px)`
          );
          continue;
        }
        if (press === "Enter") {
          // Focused with no scroll, as a Tab to the button before the reader's own scroll left it.
          await clear.evaluate((node) => {
            if (node instanceof HTMLElement) node.focus({ preventScroll: true });
          });
          await page.keyboard.press("Enter");
        } else {
          await clear.click();
        }
        await expect(clear, `${at}: clear brush window stayed drawn`).toHaveCount(0);
        await expect(page, `${at}: the brush stayed in the address`).not.toHaveURL(/[?&]ws=/);
        // The header's hand-off runs in the commit that takes the button away, so it is done by now.
        await nextFrames(page);
        await nextFrames(page);
        // One read is the answer: every hand-off has run, and nothing else moves the focus.
        const holder = await focusHolder(page);
        // Read after the same hand-offs as the read above, so where the focus is is settled.
        const landed = await page.evaluate((sightTop) => {
          const active = document.activeElement;
          const box = active?.getBoundingClientRect();
          const scroller =
            document.querySelector("[data-case-scroller]") ??
            document.scrollingElement ??
            document.documentElement;
          return {
            body: active === null || active === document.body,
            bottom: Math.round(box?.bottom ?? 0),
            meets:
              box !== undefined &&
              box.bottom > sightTop &&
              box.top < window.innerHeight &&
              box.right > 0 &&
              box.left < window.innerWidth,
            ownStop:
              active instanceof HTMLElement && active.matches('main, [data-testid="main-content"]'),
            scrolled: { box: Math.round(scroller.scrollTop), window: Math.round(window.scrollY) },
            top: Math.round(box?.top ?? 0),
          };
        }, sightTop);
        // Read after the same hand-offs as the reads above, so where the focus is is settled.
        const onSelect = await windowSelect.evaluate((node) => node === document.activeElement);
        const where = `${holder.name} at ${landed.top}..${landed.bottom} px, scroll box ${before.box} -> ${landed.scrolled.box}, scrollY ${before.window} -> ${landed.scrolled.window}`;
        if (!before.document && landed.scrolled.box !== before.box) {
          wrong.push(
            `${at}: the page's scroll box scrolled ${landed.scrolled.box - before.box} px, to ${where}`
          );
        }
        if (landed.scrolled.window !== before.window) {
          wrong.push(
            `${at}: the window scrolled ${landed.scrolled.window - before.window} px, to ${where}`
          );
        }
        if (landed.body) {
          wrong.push(`${at}: the focus is on the body, ${where}`);
        } else if (placed === "in sight" && !onSelect) {
          wrong.push(`${at}: the focus is on ${where}, not on the Window control`);
        } else if (!landed.ownStop && !landed.meets) {
          wrong.push(`${at}: the focus is on ${where}, out of sight`);
        }
      }
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): an epic of 44 issues seeded, and at two widths two of them taken out of it from the
// List's selection bar.
// INFRA-3135, round 22 (the tester's, measured in builds Jobs infra-3135-p5-h2 and -p5-b2 at
// da0804df and a1c78223): the project List, narrowed by its Epic filter to one epic, lists the
// epic and its 44 issues. The reader scrolls the table's box half way, marks two adjacent rows in
// sight with Space, and presses the bar's Take out of epic with Enter, which takes both rows out
// of the view. At da0804df the bar goes once the writes land, before the list drops the two rows.
// So its hand-off finds the row marked last still listed and focuses its checkbox. That row goes
// 30 to 60 ms later, and the focus with it: onto the body in Chromium, at 1440 px and 390 px in 2
// of 2 runs, and onto the pager's Rows per page in WebKit, 2 of 2 at each width, and in Firefox,
// 2 of 2 at 390 px and 1 of 2 at 1440 px, the other run ending on the body. At 390 px the pager's
// landing also scrolls the window by 22 px. At a1c78223 the same: the body or Rows per page. The
// focus never reaches the row now at the marked rows' place. The architect's ruling of 2026-11-21
// 06:0xZ (open-items.md, "The selection bar's return"): after a bulk action takes away the row
// marked last, the focus goes to the row that now stands at that row's place when it is in sight,
// else to the nearest row in sight before it, else to the list's own stop. It never scrolls and
// never goes to the body. A focused control taken away, for which Firefox and WebKit fire no
// `focusout`: the clause of the `webkit` and `firefox` projects that names the page-own focus
// row (e2e/playwright.config.ts).
test("a bulk action that takes the marked rows out of the project List's view hands the focus to the row now at their place, never the body or the pager @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 44; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  // The Epic filter on that epic: its value is the epic's key and title, as the column shows them.
  const view = `/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`;
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const markOf = (key: string) =>
    page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
  /** The issues the table draws, top to bottom, and of them those whose checkbox the reader sees:
   *  inside the window and the table's box, with nothing, such as the sticky head, drawn over its
   *  centre. */
  const drawn = () =>
    page.evaluate(() => {
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      const frame = scroller?.getBoundingClientRect();
      const listed = [...document.querySelectorAll<HTMLElement>("[data-issue-row]")];
      const topOf = (row: HTMLElement) => row.getBoundingClientRect().top;
      listed.sort((one, other) => topOf(one) - topOf(other));
      const seen = (row: HTMLElement) => {
        const mark = row.querySelector('input[type="checkbox"]');
        if (mark === null || frame === undefined) return false;
        const own = mark.getBoundingClientRect();
        const above = Math.max(0, frame.top);
        const below = Math.min(window.innerHeight, frame.bottom);
        if (own.top < above || own.bottom > below) return false;
        const hit = document.elementFromPoint(own.left + own.width / 2, own.top + own.height / 2);
        return hit !== null && row.contains(hit);
      };
      return {
        order: listed.map((row) => row.getAttribute("data-issue-row") ?? ""),
        shown: listed.filter(seen).map((row) => row.getAttribute("data-issue-row") ?? ""),
      };
    });
  /** Where the window and the table's box stand, and what holds the focus: its name, the row it
   *  is in, its top, and whether its box is inside the window and inside the table's box. */
  const landing = () =>
    page.evaluate(() => {
      const frame = document.querySelector<HTMLElement>('[data-testid="issues-table-scroll"]');
      const scrollTop = Math.round(frame?.scrollTop ?? 0);
      const scrollY = Math.round(window.scrollY);
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { inSight: false, name: "the body", row: null, scrollTop, scrollY, top: null };
      }
      const own = focused.getBoundingClientRect();
      const clip = frame?.getBoundingClientRect();
      const top = Math.max(0, clip?.top ?? 0);
      const bottom = Math.min(window.innerHeight, clip?.bottom ?? window.innerHeight);
      const label =
        focused instanceof HTMLInputElement || focused instanceof HTMLSelectElement
          ? focused.labels?.[0]?.textContent?.trim()
          : undefined;
      return {
        inSight:
          own.top >= top - 1 &&
          own.bottom <= bottom + 1 &&
          own.left >= -1 &&
          own.right <= window.innerWidth + 1,
        name:
          focused.getAttribute("aria-label") ||
          label ||
          focused.textContent?.trim().slice(0, 40) ||
          focused.tagName.toLowerCase(),
        row: focused.closest("[data-issue-row]")?.getAttribute("data-issue-row") ?? null,
        scrollTop,
        scrollY,
        top: Math.round(own.top),
      };
    });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    const where = `project List at ${size.width}x${size.height}, two rows taken out of the epic`;
    await page.setViewportSize(size);
    await page.goto(view);
    await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(6);
    // The window brings the table's box to just below the app's 56 px header, so its rows are in
    // the window at either width, and the box scrolls half way down its rows.
    await box.evaluate((node) => {
      window.scrollBy(0, node.getBoundingClientRect().top - 60);
      node.scrollTop = (node.scrollHeight - node.clientHeight) / 2;
    });
    await settledScrollTop(page);
    await settledScrollTop(box);
    await expect.poll(async () => (await drawn()).shown.length).toBeGreaterThanOrEqual(3);
    const { order, shown } = await drawn();
    // Two adjacent rows in the middle of what the reader sees, and the row after them.
    const at = order.indexOf(shown[Math.floor(shown.length / 2) - 1] ?? "");
    expect(
      at !== -1 && shown.includes(order[at + 1] ?? "") && at + 2 < order.length,
      `${where}: no two adjacent rows in sight mid-list (${shown.join(", ")})`
    ).toBe(true);
    const taken = [order[at] as string, order[at + 1] as string];
    const after = order[at + 2] as string;
    // The lower row first, so the row marked last is the upper one, whose place the focus takes.
    for (const key of [...taken].reverse()) {
      await markOf(key).focus();
      await page.keyboard.press("Space");
      await expect(markOf(key)).toBeChecked();
    }
    await expect(bar).toContainText("2 selected");
    await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
    await nextFrames(page);
    // One instant is the point: where the table and the window stand just before the press.
    const before = await landing();
    await page.keyboard.press("Enter");
    await expect(bar).toHaveCount(0);
    for (const key of taken) await expect(markOf(key)).toHaveCount(0);
    // The bar's hand-off runs with the commit that took the bar away, so it is done by now.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: every hand-off of the press has run, and nothing else moves.
    const now = await landing();
    const numbers = `window.scrollY ${before.scrollY} -> ${now.scrollY}, table scrollTop ${before.scrollTop} -> ${now.scrollTop}`;
    if (now.row === null || taken.includes(now.row)) {
      wrong.push(`${where}: the focus is on ${now.name}, in no listed row (${numbers})`);
    } else if (!now.inSight) {
      wrong.push(
        `${where}: the focus is on ${now.name}, out of sight at ${now.top} px (${numbers})`
      );
    } else if (now.row !== after) {
      wrong.push(
        `${where}: the focus is on ${now.name}, not on "Select ${after}", the row now at the marked rows' place (${numbers})`
      );
    }
    if (now.scrollY !== before.scrollY || now.scrollTop !== before.scrollTop) {
      wrong.push(`${where}: the press scrolled, with the focus on ${now.name}: ${numbers}`);
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): three live agents pinned again at each of two widths, and the Agents page opened
// at each.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-a5b-h and -a5b-b at
// 3fce6150 and da0804df, and infra-3135-a5m at main's merge base 143b57a7): three live agents
// are pinned, Alpha first, and the reader presses Enter on Alpha's Unpin on the Agents page. The
// page lists pinned agents first, in the order they were pinned (`orderAgents`,
// features/agents/AgentsPage.tsx), so Alpha's row moves to the end of the list, below Beta's and
// Gamma's, and the focus follows it to that row's Pin Alpha, in Chromium, Firefox and WebKit at
// both commits. At 1440x900 the three rows stand in the window and Pin Alpha with them, at
// 505 px. At 390x664 the rows are 126 px tall. In Chromium and Firefox the focus put on Unpin
// Alpha has scrolled the window to 328 px, and Pin Alpha lands at 546 px, in sight. In WebKit the
// window stands at 58 px, Pin Alpha lands at 816 px, below the 664 px window, and nothing
// scrolls, at both commits. Main does the same in WebKit: Pin Alpha at 505 px at 1440, and at
// 816 px with the window held at 58 px at 390. The architect's rulings: a landing the reader
// caused must be in view (2026-11-19 10:3xZ, WCAG 2.2's 2.4.11 Focus Not Obscured), and the one
// scroll a landing may make is to the nearest edge of a control partly in sight, the least
// distance that shows it whole (2026-11-21 12:3xZ). Which control the press lands on is not read
// here: 2026-11-19 22:3xZ names the same control in the row now at the pressed row's place, and
// main's page means the focus to follow the row to where it lands (AgentsPage.tsx,
// `focusAfterPin`). A focused control taken away, for which Firefox and WebKit fire no
// `focusout` (React moves the row's node): the clause of the `webkit` and `firefox` projects that
// names the page-own focus row (e2e/playwright.config.ts).
test("Unpin on a pinned agent keeps the keyboard focus on a control in sight, on the Agents page at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  const pinnedAgent = (sessionId: string, title: string): FakeSession => ({
    capabilities: ["aside", "btw", "steer"],
    dir: `/workspaces/${title.toLowerCase()}`,
    last_seen: Date.now() - 5_000,
    machine_id: "pin-host",
    roles: [],
    session_id: sessionId,
    title,
  });
  const alpha = pinnedAgent("01a0e200-4848-7473-acc5-0000000000a5", "Alpha");
  const beta = pinnedAgent("01a0e200-4848-7473-acc5-0000000000b5", "Beta");
  const gamma = pinnedAgent("01a0e200-4848-7473-acc5-0000000000c5", "Gamma");
  const agents = [alpha, beta, gamma];
  await setLiveSessions(agents);
  /** What holds the focus and where the window stands: the focused element's name, the agent row
   *  it is in, its top and height, and whether its box stands wholly inside the window. */
  const landing = () =>
    page.evaluate(() => {
      const scrollY = Math.round(window.scrollY);
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { height: 0, inSight: false, name: "the body", row: null, scrollY, top: null };
      }
      const own = focused.getBoundingClientRect();
      const words = (focused.getAttribute("aria-label") || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      return {
        height: Math.round(own.height),
        inSight:
          own.top >= -1 &&
          own.bottom <= window.innerHeight + 1 &&
          own.left >= -1 &&
          own.right <= window.innerWidth + 1,
        name: `${focused.tagName.toLowerCase()} "${words}"`,
        row: focused.closest("[data-agent-row]")?.getAttribute("data-agent-row") ?? null,
        scrollY,
        top: Math.round(own.top),
      };
    });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Agents page at ${size.width}x${size.height}`;
    // Pinned again in this order at each width, so Alpha is the first pinned row each time.
    for (const agent of agents) await putAgentState(agent.session_id, { pinned: false });
    for (const agent of agents) await putAgentState(agent.session_id, { pinned: true });
    await page.goto("/agents");
    const unpin = agentRow(page, alpha.session_id).getByRole("button", {
      exact: true,
      name: "Unpin Alpha",
    });
    await expect(unpin).toBeVisible();
    await expect(
      agentRow(page, gamma.session_id).getByRole("button", { exact: true, name: "Unpin Gamma" })
    ).toBeVisible();
    await unpin.focus();
    await nextFrames(page);
    // One instant is the point: where the window stands just before the press.
    const before = await landing();
    await page.keyboard.press("Enter");
    // The pin's write has landed once Alpha's row offers to pin it again.
    await expect(
      agentRow(page, alpha.session_id).getByRole("button", { exact: true, name: "Pin Alpha" })
    ).toBeVisible();
    // The poll waits, up to the expect timeout, for the focus the moved row took to land again.
    await expect
      .poll(async () => (await landing()).top !== null)
      .toBe(true)
      .catch(() => undefined);
    await settledScrollTop(page);
    // One read is the answer: every hand-off of the press has run, and the window holds still.
    const now = await landing();
    const rowTitle = agents.find((agent) => agent.session_id === now.row)?.title ?? now.row;
    const what = now.row === null ? now.name : `${now.name} in ${rowTitle}'s row`;
    const numbers = `window.scrollY ${before.scrollY} -> ${now.scrollY}`;
    if (now.top === null) {
      wrong.push(`${where}: after Enter on Unpin Alpha the focus is on the body (${numbers})`);
    } else if (!now.inSight) {
      wrong.push(
        `${where}: after Enter on Unpin Alpha the focus is on ${what}, out of sight at ${now.top} px in a ${size.height} px window (${numbers})`
      );
    }
    // The 12:3xZ allowance: the landing's own height, and a pixel for the rounding of a box.
    const allowed = now.height + 1;
    if (Math.abs(now.scrollY - before.scrollY) > allowed) {
      wrong.push(
        `${where}: after Enter on Unpin Alpha the window scrolled ${now.scrollY - before.scrollY} px, more than the landing's own height, to ${what} (${numbers})`
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

/** Where the keyboard focus is, read once: the focused element's tag and name, whether it is the
 *  body, whether it stands inside an element `inside` matches (a CSS selector, or none), whether
 *  its box stands wholly inside the window, and where the window stands. */
function focusPlace(
  page: Page,
  inside?: string
): Promise<{ body: boolean; inSight: boolean; inside: boolean; name: string; scrollY: number }> {
  return page.evaluate((inside) => {
    const scrollY = Math.round(window.scrollY);
    const focused = document.activeElement;
    if (!(focused instanceof HTMLElement) || focused === document.body) {
      return { body: true, inSight: false, inside: false, name: "the body", scrollY };
    }
    const own = focused.getBoundingClientRect();
    const words = (focused.getAttribute("aria-label") || focused.textContent || "")
      .replace(/\s+/g, " ")
      .trim()
      .slice(0, 40);
    return {
      body: false,
      inSight:
        own.top >= -1 &&
        own.bottom <= window.innerHeight + 1 &&
        own.left >= -1 &&
        own.right <= window.innerWidth + 1,
      inside: inside !== null && focused.closest(inside) !== null,
      name: `${focused.tagName.toLowerCase()} "${words}"`,
      scrollY,
    };
  }, inside ?? null);
}

// SLOW(>=2s): an issue's references read answered, failed over its answer and retried, at each
// of two widths.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-n2-h and -n2-b at 3fce6150
// and da0804df, and this row in -n-red at 3fce6150): an issue's Artifacts tab has read the issue's
// references once, and a message on another issue that mentions it makes the live feed read them
// again, which fails (500, three times by the shared retry policy). The tab says "References
// unavailable" with Retry, over the answer the read still holds. At the commits the row was
// measured on, Retry was natively disabled while its read was out (`disabled={retrying}` in
// components/QueryError.tsx, given `retrying={references.isFetching}` by
// features/artifacts/ArtifactsTab.tsx): Enter on Retry, with the read it sends held, drew Retry
// disabled, and the focus fell to the body for the whole hold, in Chromium, Firefox and WebKit at
// 1440x900 and 390x664, at both commits. A read that holds no answer yet is another path, which
// this row does not take: TanStack sets it back to pending the moment Retry sends, so the line and
// Retry go in the press's own render. The rule: a control held while the read its press started is
// out is held through components/held-press.ts, never natively disabled, so it keeps the keyboard
// focus (its own doc comment). A focused control disabled under the press, which each engine
// moves to the body at its own time: the clause of the `webkit` and `firefox` projects that names
// the page-own focus row (e2e/playwright.config.ts).
test("a references Retry on the Artifacts tab keeps the keyboard focus while its read is out @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with a read answered, failed through the shared policy's retries and held.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  // The references read of the issue the row is on: answered, failed or held as the row says.
  const reads = { held: [] as Route[], mode: "answer" as "answer" | "fail" | "hold", path: "" };
  await page.route(
    (url) => url.pathname === reads.path,
    async (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      if (reads.mode === "hold") {
        reads.held.push(route);
        return;
      }
      if (reads.mode === "answer") return route.continue();
      await route.fulfill({
        body: JSON.stringify({ code: "INTERNAL", error: "The references read failed." }),
        contentType: "application/json",
        status: 500,
      });
    }
  );
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `Artifacts tab at ${size.width}x${size.height}`;
    const issue = await createIssue({ project: "CORE", title: `Lease format at ${size.width}` });
    const source = await createIssue({ project: "OPS", title: `Mirror plan at ${size.width}` });
    await createMessage(source.key, { body: `Depends on dispatch://${issue.key}.` });
    reads.mode = "answer";
    reads.path = `/api/v1/issues/${issue.key}/references`;
    await page.goto(`/issues/${issue.key}/artifacts`);
    // The read has answered once the tab counts what references the issue.
    await expect(
      page.locator("summary").filter({ hasText: /^Referenced by \(1\)$/ })
    ).toBeVisible();
    reads.mode = "fail";
    await createMessage(source.key, { body: `Also dispatch://${issue.key}.` });
    // The live feed reads the references again, and the shared policy retries the 500 twice.
    const alert = page.getByRole("alert").filter({ hasText: "References unavailable" });
    await expect(alert).toBeVisible();
    await alert.getByRole("button", { exact: true, name: "Retry" }).focus();
    reads.mode = "hold";
    await page.keyboard.press("Enter");
    await expect.poll(() => reads.held.length, `${where}: Retry sent no read`).toBe(1);
    // The focus fixup of a disabled control runs in a rendering update; two frames hold one.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: Retry's read is held, and its press's renders have all run.
    const holding = await focusPlace(page);
    if (holding.body) {
      wrong.push(`${where}: with the read Retry sent held the focus is on the body`);
    }
    reads.mode = "answer";
    for (const route of reads.held.splice(0)) await route.continue();
    await expect(alert).toHaveCount(0);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

/** The record the row below keeps in the page of the project List's offline Take out of epic:
 *  whether the page has heard that the browser is offline, the animation frames counted from the
 *  press on, every element a `focusin` reached with the frame it reached it in, and the selection
 *  bar's going. */
interface OfflineTakeOut {
  offline: boolean;
  frame: number;
  readonly focusins: { readonly element: Element; readonly frame: number }[];
  /** Once the bar has left the page: the page's clock and the frame then, what held the focus at
   *  the end of the task that removed the bar, and what held it at the next animation frame. */
  going: {
    readonly at: number;
    readonly frame: number;
    readonly atGoing: Element | null;
    atNextFrame: Element | null;
    nextFrameSeen: boolean;
  } | null;
}

declare global {
  interface Window {
    __offlineTakeOut?: OfflineTakeOut;
  }
}

// SLOW(>=2s): an epic of 44 issues seeded, and at two widths two of them taken out of it with the
// browser offline, the 30 s request deadline waited out on the wall clock at each.
// INFRA-3135, round 24 (the tester's, measured in builds Jobs infra-3135-d-h, -d2-h, -d2h2 and
// -d-b, -d2-b at 3fce6150 and da0804df, on the wall clock): the project List, narrowed by its
// Epic filter to one epic of 44 issues, its box scrolled half way; the reader marks two adjacent
// rows in sight with Space, the upper one last, and presses the bar's Take out of epic with
// Enter. Both writes land at Dispatch, and the browser goes offline before the page has their
// answers, so the List's read after them cannot be sent and both rows stay listed. At 3fce6150
// the bar stays, its action held, until the read's bound runs out at the request deadline
// (`REQUEST_DEADLINE_MS`), about 30.3 s after the press, and then goes. At 1440x900 that held in
// 21 of 21 runs: the focus went to the checkbox of the row marked last in Chromium 7 of 7 and in
// Firefox 6 of 7, but to the pager's Rows per page once in Firefox and 5 times of 7 in WebKit,
// and it stayed on the body in WebKit twice. Where it reached the checkbox, it did so 5 to 12 ms
// after the bar went, a frame later, the body holding the focus at that frame, in 12 of 13 runs.
// At 390x664 the bar held to the deadline in 12 of 21 runs, and each time the checkbox took the
// focus in the frame the bar went in. In the other 9 the bar went as the writes landed, 0.28 to
// 0.41 s after the press, with no line saying the List could not be read and both rows still
// listed, and the focus went to the checkbox 5 times and to Rows per page 4 times, each time
// with the window scrolled 22 px. At da0804df the bar went as the writes landed in 24 of 24
// runs: the focus went to the checkbox in Chromium and Firefox 16 of 16 and in WebKit 2 of 8, and
// to Rows per page in WebKit 6 times, at 390 with the window scrolled 22 px. The architect's
// round-24 brief: past the deadline the focus goes to the marked row's checkbox, and the bar goes
// in the same render. The rulings of 2026-11-21 06:0xZ and 09:0xZ: the bar's hand-off goes to a
// row, never to the body, and scrolls nothing; the pager is no row. The row waits the deadline out
// on the wall clock, as the row that Save delivery settings keeps the keyboard focus through a
// save whose re-read the browser cannot send does. The page's own clock fakes
// `requestAnimationFrame`, on which the bar's frame and an armed press's wait of a frame and a
// task both rest (`afterComponents`, components/focus-place.ts), and on it WebKit's landing does
// not show: in builds Job infra-3135-dred-k5lxv at 3fce6150 this row on the page's clock passed
// 3 of 3 in WebKit, and failed in Chromium and Firefox at 1440 on the frame alone. The row counts
// the page's animation frames from the press on, as the measurements above did. Without that
// count it failed in WebKit 1 of 3 (infra-3135-dred2-bnb7t), so WebKit's landing rests on the
// frames the page draws; with it, it failed at 1440 9 of 9 (infra-3135-dred3-k9mrh): WebKit on
// Rows per page, Chromium and Firefox on the frame, the checkbox taking the focus a frame after
// the bar went. Whichever way the bar goes, the row holds the same rule and its message names the
// way. A focused control taken away with its bar, for which Firefox and WebKit fire no
// `focusout`: the clause of the `webkit` and `firefox` projects that names the page-own focus row
// (e2e/playwright.config.ts).
test("offline, Take out of epic hands the focus to the marked row's checkbox in the render the bar goes, never the body or the pager @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 44; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  // The Epic filter on that epic: its value is the epic's key and title, as the column shows them.
  const view = `/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`;
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const markOf = (key: string) =>
    page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
  /** The issues the table draws, top to bottom, and of them those whose checkbox the reader sees:
   *  inside the window and the table's box, with nothing, such as the sticky head, drawn over its
   *  centre. */
  const drawn = () =>
    page.evaluate(() => {
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      const frame = scroller?.getBoundingClientRect();
      const listed = [...document.querySelectorAll<HTMLElement>("[data-issue-row]")];
      const topOf = (row: HTMLElement) => row.getBoundingClientRect().top;
      listed.sort((one, other) => topOf(one) - topOf(other));
      const seen = (row: HTMLElement) => {
        const mark = row.querySelector('input[type="checkbox"]');
        if (mark === null || frame === undefined) return false;
        const own = mark.getBoundingClientRect();
        const above = Math.max(0, frame.top);
        const below = Math.min(window.innerHeight, frame.bottom);
        if (own.top < above || own.bottom > below) return false;
        const hit = document.elementFromPoint(own.left + own.width / 2, own.top + own.height / 2);
        return hit !== null && row.contains(hit);
      };
      return {
        order: listed.map((row) => row.getAttribute("data-issue-row") ?? ""),
        shown: listed.filter(seen).map((row) => row.getAttribute("data-issue-row") ?? ""),
      };
    });
  /** Where the window and the table's box stand, where the checkbox of issue `marked` stands on
   *  screen, and what holds the focus: its kind and name, and whether its box is inside the
   *  window and inside the table's box. */
  const landing = (marked: string) =>
    page.evaluate((key) => {
      const frame = document.querySelector<HTMLElement>('[data-testid="issues-table-scroll"]');
      const mark = document.querySelector(`[data-issue-row="${key}"] input[type="checkbox"]`);
      const markTop = mark === null ? null : Math.round(mark.getBoundingClientRect().top);
      const scrollTop = Math.round(frame?.scrollTop ?? 0);
      const scrollY = Math.round(window.scrollY);
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { inSight: false, markTop, name: "the body", scrollTop, scrollY };
      }
      const own = focused.getBoundingClientRect();
      const clip = frame?.getBoundingClientRect();
      const top = Math.max(0, clip?.top ?? 0);
      const bottom = Math.min(window.innerHeight, clip?.bottom ?? window.innerHeight);
      const kind =
        focused instanceof HTMLInputElement ? focused.type : focused.tagName.toLowerCase();
      const label =
        focused.getAttribute("aria-label") ||
        (focused as HTMLInputElement).labels?.[0]?.textContent?.trim() ||
        (focused.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
      return {
        inSight:
          own.top >= top - 1 &&
          own.bottom <= bottom + 1 &&
          own.left >= -1 &&
          own.right <= window.innerWidth + 1,
        markTop,
        name: `${kind} "${label}"`,
        scrollTop,
        scrollY,
      };
    }, marked);
  /** The page's record of the press (`OfflineTakeOut`), each element named as `landing` names
   *  the focus. */
  const barRecord = () =>
    page.evaluate(() => {
      const named = (element: Element | null) => {
        if (element === null || element === document.body) return "the body";
        const kind =
          element instanceof HTMLInputElement ? element.type : element.tagName.toLowerCase();
        const label =
          element.getAttribute("aria-label") ||
          (element as HTMLInputElement).labels?.[0]?.textContent?.trim() ||
          (element.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
        return `${kind} "${label}"`;
      };
      const kept = window.__offlineTakeOut;
      const going = kept?.going ?? null;
      return {
        atGoing: going === null ? null : named(going.atGoing),
        atNextFrame: going?.nextFrameSeen === true ? named(going.atNextFrame) : null,
        focusins: (kept?.focusins ?? []).map(
          (focusin) => `${named(focusin.element)} at frame ${focusin.frame}`
        ),
        goneAt: going?.at ?? null,
        goneFrame: going?.frame ?? null,
      };
    });
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    const where = `project List at ${size.width}x${size.height}, two rows taken out of the epic offline`;
    await page.setViewportSize(size);
    await page.goto(view);
    await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(6);
    // The window brings the table's box to just below the app's 56 px header, so its rows are in
    // the window at either width, and the box scrolls half way down its rows.
    await box.evaluate((node) => {
      window.scrollBy(0, node.getBoundingClientRect().top - 60);
      node.scrollTop = (node.scrollHeight - node.clientHeight) / 2;
    });
    await settledScrollTop(page);
    await settledScrollTop(box);
    await expect.poll(async () => (await drawn()).shown.length).toBeGreaterThanOrEqual(3);
    const { order, shown } = await drawn();
    // Two adjacent rows in the middle of what the reader sees.
    const at = order.indexOf(shown[Math.floor(shown.length / 2) - 1] ?? "");
    expect(
      at !== -1 && shown.includes(order[at + 1] ?? ""),
      `${where}: no two adjacent rows in sight mid-list (${shown.join(", ")})`
    ).toBe(true);
    const taken = [order[at] as string, order[at + 1] as string];
    // The upper row is marked last: the row whose checkbox the bar's hand-off is to focus, and
    // which the List still lists while the browser is offline.
    const last = taken[0] as string;
    for (const key of [...taken].reverse()) {
      await markOf(key).focus();
      await page.keyboard.press("Space");
      await expect(markOf(key)).toBeChecked();
    }
    await expect(bar).toContainText("2 selected");
    await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
    await nextFrames(page);
    const context = page.context();
    // The two writes go to Dispatch as the page sends them, and their answers are held until the
    // browser is offline. A read of the issue list made from the first write on is held with no
    // answer, so the List's read after the action cannot land while the browser is online.
    const written = new RegExp(`^/api/v1/issues/(${taken.join("|")})$`);
    const answers: { response: APIResponse; route: Route }[] = [];
    const heldReads: Route[] = [];
    let writing = false;
    let landed = false;
    await page.route(
      (url) => url.pathname === "/api/v1/issues" || written.test(url.pathname),
      async (route) => {
        const request = route.request();
        if (request.method() === "GET" && new URL(request.url()).pathname === "/api/v1/issues") {
          if (!writing) return route.fallback();
          heldReads.push(route);
          return;
        }
        if (request.method() !== "PATCH") return route.fallback();
        writing = true;
        answers.push({ response: await route.fetch(), route });
        // The action sends its writes together (`WRITES_IN_FLIGHT`, issues-table-model.ts), so
        // the second answer is the last one.
        if (answers.length < taken.length) return;
        await context.setOffline(true);
        // The page's query client hears the browser go offline by the `offline` event, and the
        // record's own listener, added after the app's, hears it after.
        await page.waitForFunction(() => window.__offlineTakeOut?.offline === true);
        for (const answer of answers) await answer.route.fulfill({ response: answer.response });
        landed = true;
      }
    );
    try {
      await page.evaluate(() => {
        const kept: OfflineTakeOut = { focusins: [], frame: 0, going: null, offline: false };
        window.__offlineTakeOut = kept;
        // The frames the page draws, counted by an animation frame callback in each.
        const tick = () => {
          kept.frame += 1;
          requestAnimationFrame(tick);
        };
        requestAnimationFrame(tick);
        window.addEventListener("offline", () => {
          kept.offline = true;
        });
        document.addEventListener("focusin", (event) => {
          if (event.target instanceof Element) {
            kept.focusins.push({ element: event.target, frame: kept.frame });
          }
        });
        const watcher = new MutationObserver(() => {
          if (document.querySelector('[role="toolbar"][aria-label="Selected issues"]') !== null)
            return;
          watcher.disconnect();
          const going = {
            at: Date.now(),
            atGoing: document.activeElement,
            atNextFrame: null as Element | null,
            frame: kept.frame,
            nextFrameSeen: false,
          };
          kept.going = going;
          // The next frame's callbacks run before it paints: what holds the focus then is what
          // that frame draws focused.
          requestAnimationFrame(() => {
            going.atNextFrame = document.activeElement;
            going.nextFrameSeen = true;
          });
        });
        watcher.observe(document.body, { childList: true, subtree: true });
      });
      // One instant is the point: where the table, the window and the marked row stand just
      // before the press.
      const before = await landing(last);
      // The press's time on the page's clock, against which the bar's going is timed.
      const pressed = await page.evaluate(() => Date.now());
      await page.keyboard.press("Enter");
      await expect
        .poll(() => landed, { message: `${where}: the two writes never both landed` })
        .toBe(true);
      // The row's claim is a span of time: the bar goes at the request deadline, 30 s after the
      // press, or sooner where the List's read settles first, and the row waits for its going on
      // the wall clock with room to spare. A bar that never goes is the record's to say, below.
      await expect(bar)
        .toHaveCount(0, { timeout: REQUEST_DEADLINE_MS + 15_000 })
        .catch(() => undefined);
      // The bar's hand-off runs after the commit that took the bar away, so it is done after
      // these frames.
      await nextFrames(page);
      await nextFrames(page);
      // The record's next frame after the bar went is what that frame drew focused; wait for
      // that frame to be noted, or for nothing where the bar never went.
      await page.waitForFunction(() => {
        const going = window.__offlineTakeOut?.going;
        return going == null || going.nextFrameSeen;
      });
      // One read once every hand-off has run and the frame after the bar's going was drawn:
      // nothing else moves the focus or scrolls the page while the browser stays offline.
      const log = await barRecord();
      // One read of where the focus rests, taken after the record and in the same still page.
      const now = await landing(last);
      const expected = `checkbox "Select ${last}"`;
      const numbers = `window.scrollY ${before.scrollY} -> ${now.scrollY}, table scrollTop ${before.scrollTop} -> ${now.scrollTop}, the checkbox of ${last} at ${before.markTop} -> ${now.markTop} px`;
      const way =
        log.goneAt === null
          ? "never"
          : log.goneAt - pressed < REQUEST_DEADLINE_MS
            ? `as its writes landed, ${log.goneAt - pressed} ms after the press, its read settled with the browser offline`
            : `at the request deadline, ${log.goneAt - pressed} ms after the press`;
      const moves = `the focus after the press: ${log.focusins.join(" -> ") || "nowhere"}`;
      if (log.goneAt === null) {
        wrong.push(
          `${where}: the selection bar never went; the focus rests on ${now.name} (${moves})`
        );
      } else if (log.atGoing !== expected && log.atNextFrame !== expected) {
        wrong.push(
          `${where}: the bar went ${way}, in frame ${log.goneFrame}, with the focus on ${log.atGoing}, and the next frame drew it on ${log.atNextFrame ?? "no frame seen"}, not on ${expected} (${moves})`
        );
      }
      if (now.name !== expected) {
        wrong.push(
          `${where}: once the bar went ${way}, the focus rests on ${now.name}, not on ${expected} (${moves}; ${numbers})`
        );
      } else if (!now.inSight) {
        wrong.push(
          `${where}: once the bar went ${way}, the focus rests on ${expected}, out of sight (${numbers})`
        );
      }
      if (now.scrollY !== before.scrollY) {
        wrong.push(`${where}: once the bar went ${way}, the window scrolled (${numbers})`);
      }
      // The table's box may rise by the height of the line that says the List could not be read,
      // drawn above it, only while the marked row keeps its place on screen.
      const markStood =
        now.markTop !== null &&
        before.markTop !== null &&
        Math.abs(now.markTop - before.markTop) <= 1;
      if (now.scrollTop !== before.scrollTop && !markStood) {
        wrong.push(`${where}: once the bar went ${way}, the table's box scrolled (${numbers})`);
      }
    } finally {
      await context.setOffline(false);
      for (const route of heldReads) {
        await route.abort("internetdisconnected").catch(() => undefined);
      }
      await page.unrouteAll({ behavior: "ignoreErrors" });
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the Delivery page opened three times in a fixture, with its live socket held, closed and open.
// INFRA-3135, round 25 (the tester's), by the architect's ruling of 2026-11-24 08:07Z: a count
// beside a value is drawn only once the data it counts has answered, never as a 0 that stands for
// "not read yet". Measured in builds Jobs infra-3135-dl-h2-pbn4c (head 683f1fdf) and
// infra-3135-dl-m2-hdf8z (main 642bc1ed), in Chromium, Firefox and WebKit at 1440x900: with the
// live socket held (nothing answers the timeline's subscribe) or closed by the network, the
// head's Filter panel offers Rework and Deployed, and every verdict reads "0 PRs"
// (DeliveryPage.tsx:415 `counts[value] ?? 0` over `data?.facet_counts`, :495, where `data` is
// undefined until the first answer). Main drew its FacetPanel only once the timeline answered.
// With the socket open the same fixture reads value 1, rework 1, deployed 1, waiting 1 and
// not tracked 0, which this row reads too, so it can tell a count from a placeholder.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-25 tester pinned (e2e/playwright.config.ts).
test("the Delivery page's Filter panel draws no count beside a verdict before the timeline answers, nor after its socket fails @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedDeliveryFixture();
  const main = page.getByTestId("main-content");
  /** The accessible name of each option of one facet of the open Filter panel, read once its
   *  list holds every verdict, so the read is of a list that is whole. */
  const verdicts = async (facet: "Rework" | "Deployed", count: number): Promise<string[]> => {
    const panel = await openFilterPanel(page, main);
    await panel.getByRole("button", { name: new RegExp(`^${facet}: `) }).click();
    const options = page.getByRole("listbox", { name: `${facet} options` }).getByRole("option");
    await expect(options).toHaveCount(count);
    const names = await options.evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("aria-label") ?? element.textContent ?? "")
    );
    await page.keyboard.press("Escape");
    await closeControlPanel(main);
    return names;
  };
  const wrong: string[] = [];
  // One route for the page's live socket, whose mode each load sets: open passes it through to
  // the server, held opens it and answers nothing, closed drops it as a network would.
  let mode: "open" | "held" | "closed" = "open";
  await page.routeWebSocket(/\/ws\/live$/u, (socket) => {
    if (mode === "open") socket.connectToServer();
    if (mode === "closed") void socket.close({ code: 1011, reason: "test network drop" });
  });
  // The control: the page with its socket open reads the fixture's own counts.
  await page.goto(`/delivery?${DELIVERY_WINDOW}`);
  await expect(main.getByRole("img", { name: /^Delivery timeline/ })).toBeVisible();
  expect(await verdicts("Rework", 2)).toEqual(["value, 1 PR", "rework, 1 PR"]);
  for (const how of ["held", "closed"] as const) {
    mode = how;
    await page.goto(`/delivery?${DELIVERY_WINDOW}`);
    for (const [facet, count] of [
      ["Rework", 2],
      ["Deployed", 3],
    ] as const) {
      const counted = (await verdicts(facet, count)).filter((name) => /\d+ PRs?$/u.test(name));
      wrong.push(
        ...counted.map(
          (name) => `socket ${how}: ${facet} offers "${name}" before the timeline answered`
        )
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): at two widths, a project List's Move refused for two open issues and a Move with
// only closed issues marked, each followed by another person's live update that empties the bar.
// INFRA-3135, round 25 (the tester's, from t23's B8, which the architect ruled into this round on
// 2026-11-24 03:2xZ: a hold that only says there is nothing to do keeps no press live). Measured in
// builds Jobs infra-3135-h-h2-thk26 at 683f1fdf, infra-3135-h-b2-gjpk7 at 3fce6150 and
// infra-3135-h-p8-4nhkv at ae48d8a1, the commit before a5464d6e. The reader marks two issues on
// the project List, picks an epic in Move to epic and presses Move by Enter; Move's own press
// leaves it held with no epic staged (`ariaDisabled={busy || epic === ""}`,
// features/project/issues-table-page.tsx:394-397). Then bob closes or reopens the marked issues,
// and his live update takes them off the view and empties the selection bar while Move holds the
// focus. That going is not the reader's, so the bar hands the focus nowhere (SelectionBar.tsx,
// "Nor does a going the reader did not cause ... leaves the focus on the body").
// - The Move refused (400) for two open issues: at 683f1fdf the focus stays on the body in
//   Chromium, Firefox and WebKit at 1440 and 390; at ae48d8a1 it lands on a row's checkbox
//   (`Select MVA-4`), which a5464d6e fixed.
// - The Move with only closed issues marked, which the List answers "Only open issues can change
//   epic." without a write: at 683f1fdf, 3fce6150 and ae48d8a1 the bar's hand-off lands the focus
//   on `TABLE:Project issues table`, the list's own stop, in all three engines at both widths.
//   `moveTo` returns at that answer before any action begins a hold on what answers the press
//   (features/project/issues-table-actions.ts:284-289), so nothing marks the press answered, and
//   `writeOut` (components/focus-place.ts:882-883) keeps counting Move's own hold, begun after the
//   arm, up to the 30 s write deadline: the press stays live, and the bar reads its going as the
//   reader's (`causedByReader`).
// The refused Move is the control: the row asserts that the closed-issues path ends where the
// refused path ends, so it pins no landing of its own.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-25 tester pinned (e2e/playwright.config.ts).
test("a Move with only closed issues marked keeps no press live: a live update that empties the List's bar then lands the focus where it lands after a refused Move, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  const sizes = [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ] as const;
  /** One project per width and path: an epic and six tasks, the first two closed on the closed
   *  path. Each task's title is its own words, since the server refuses a near copy of a title it
   *  holds (409 POSSIBLE_DUPLICATE). */
  const seeds: { epic: string; key: string; marked: [string, string] }[] = [];
  const words = ["Harbor", "Lantern", "Quarry", "Meadow", "Falcon", "Copper"];
  for (const [index, path] of ["refused", "closed", "refused", "closed"].entries()) {
    const key = `MV${String.fromCharCode(65 + index)}`;
    await createProject({ key, name: `Move ${path} ${index}` });
    const epic = await createIssue({ project: key, title: `Target epic ${key}` });
    await patchIssue(epic.key, { labels: ["epic"] });
    const tasks: string[] = [];
    for (const [at, word] of words.entries()) {
      tasks.push((await createIssue({ project: key, title: `${word} ${key} task ${at}` })).key);
    }
    if (path === "closed") {
      for (const task of tasks.slice(0, 2)) await patchIssue(task, { status: "done" });
    }
    seeds.push({ epic: `Target epic ${key}`, key, marked: [tasks[1] ?? "", tasks[0] ?? ""] });
  }
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const move = bar.getByRole("button", { exact: true, name: "Move" });
  /** What holds the focus: its name, or "the body", and the row and the table around it. */
  const landing = () =>
    page.evaluate(() => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) {
        return { inRow: null, inTable: false, name: "the body" };
      }
      return {
        inRow: focused.closest("[data-issue-row]")?.getAttribute("data-issue-row") ?? null,
        inTable: focused.closest("table") !== null,
        name: `${focused.tagName}:${(focused.getAttribute("aria-label") ?? focused.textContent ?? "").trim().slice(0, 40)}`,
      };
    });
  /** Marks the seed's two issues from the keyboard, the lower one first, and picks its epic. */
  const markAndPick = async (seed: (typeof seeds)[number]) => {
    for (const key of seed.marked) {
      const mark = page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
      await mark.focus();
      await page.keyboard.press("Space");
      await expect(mark).toBeChecked();
    }
    await expect(bar).toContainText("2 selected");
    await pickFromList(listTrigger(bar, "Move to epic"), seed.epic);
    await expect(move).not.toHaveAttribute("aria-disabled", "true");
    await move.focus();
  };
  /** bob's live update takes the marked issues off the view, and the bar goes with them. The
   *  bar's hand-off runs with the commit that took it away, which two frames see done. */
  const emptyTheBar = async (seed: (typeof seeds)[number], status: "done" | "todo") => {
    for (const key of seed.marked) await patchIssue(key, { status }, { login: "bob" });
    await expect(bar).toHaveCount(0);
    await nextFrames(page);
    await nextFrames(page);
  };
  const refuseUpdates = async (route: Route) => {
    if (route.request().method() !== "PATCH") return route.fallback();
    return route.fulfill({
      body: JSON.stringify({ code: "REFUSED_FOR_THE_ROW", error: "refused for the row" }),
      contentType: "application/json",
      status: 400,
    });
  };
  const wrong: string[] = [];
  for (const [step, size] of sizes.entries()) {
    await page.setViewportSize(size);
    const where = `project List at ${size.width}x${size.height}`;
    const refused = seeds[step * 2];
    const closed = seeds[step * 2 + 1];
    if (refused === undefined || closed === undefined)
      throw new Error("the seed holds four projects");

    // The control: a Move every write of which is refused. The action is answered and lets its
    // controls go, and Move stays held with no epic staged.
    await openIssuesTab(page, refused.key, "List");
    await markAndPick(refused);
    await page.route(/\/api\/v1\/issues\/[^/?]+$/, refuseUpdates);
    await page.keyboard.press("Enter");
    await expect(page.getByText(/Retry each failed change/).first()).toBeVisible();
    await expect(
      bar.getByRole("button", { exact: true, name: "Take out of epic" })
    ).not.toHaveAttribute("aria-disabled", "true");
    await page.unroute(/\/api\/v1\/issues\/[^/?]+$/, refuseUpdates);
    await expect(move).toHaveAttribute("aria-disabled", "true");
    await expect(move).toBeFocused();
    await nextFrames(page);
    await emptyTheBar(refused, "done");
    // One read is the answer: the bar went two frames back, its hand-off ran with that commit,
    // and nothing else moves the focus now.
    const afterRefused = await landing();

    // The closed path: the List answers the press without a write, and Move stays held with no
    // epic staged, as above.
    await openIssuesTab(page, closed.key, "List");
    await page.goto(`/projects/${closed.key}/issues?status=done`);
    await expect(page.locator("[data-issue-row]")).toHaveCount(2);
    await markAndPick(closed);
    await page.keyboard.press("Enter");
    await expect(page.getByText(/^Only open issues can change epic\./).first()).toBeVisible();
    await expect(move).toHaveAttribute("aria-disabled", "true");
    await expect(move).toBeFocused();
    await nextFrames(page);
    await emptyTheBar(closed, "todo");
    // One read is the answer: the bar went two frames back, its hand-off ran with that commit,
    // and nothing else moves the focus now.
    const afterClosed = await landing();

    if (afterClosed.name !== afterRefused.name || afterClosed.inRow !== afterRefused.inRow) {
      wrong.push(
        `${where}: after a Move with only closed issues marked and a live update that emptied the bar, the focus is on ${afterClosed.name}${afterClosed.inTable ? " (in the table)" : ""}${afterClosed.inRow === null ? "" : ` in row ${afterClosed.inRow}`}, where after a refused Move and the same update it is on ${afterRefused.name}${afterRefused.inRow === null ? "" : ` in row ${afterRefused.inRow}`}`
      );
    }
  }
  // A hand-off that runs for a loss the reader did not cause moves a keyboard reader's place to a
  // control they never chose.
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): twenty labelled issues and fifteen repositories' pull requests seeded, and at each of
// two widths a value of the project List's Labels filter ticked and one of the Delivery page's
// Repository list ticked and unticked, each by Enter, with the list redrawn after each.
// INFRA-3135, round 26 (the tester's, measured in builds Jobs infra-3135-p-hc-pmzhr and
// infra-3135-p-hfw-wx7zj at 8e7e463a and infra-3135-p-bc-td6pm and infra-3135-p-bfw-4tfm7 at
// 34dc6a07, and this row in infra-3135-p-red-7lb7z and infra-3135-p-red2-p8kpf at 8e7e463a): the
// reader walks a shared multi-select with ArrowDown until the option it marks active stands at the
// bottom of its box, and presses Enter. In the project List's Labels filter, with nothing ticked
// before, the tick draws the list's Clear foot, 61 px tall and stuck to the box's bottom, over that
// option: lab-07 stands wholly under it, and so does the ticked option of the Inbox Filter panel's
// Project list and of the Delivery page's Author list, in Chromium, Firefox and WebKit at 1440x900
// and 390x664. The Delivery page lists the picked repositories first, so the tick moves acme/r06 to
// the top of its Repository list, where the box, still scrolled 83 px, shows it nothing in Chromium
// and WebKit, and the untick moves it back to its own place, below what the box shows in Chromium
// (scrolled 39 px) and Firefox (scrolled back to its top at the tick). At 34dc6a07 the arrows
// scrolled the box nothing, so the active option stood below the box before the Enter already. The
// cause: Enter ticks or unticks the active option (components/MultiSelect.tsx:574-581, :349-353),
// the first tick draws the Clear foot (:805-821) and the Delivery page orders the picked values
// first (features/delivery/DeliveryPage.tsx:411), and nothing shows the active option again once
// the list has redrawn: only the arrows and a single pick's open scroll the box to it
// (`showOption`). The rule (the architect's ruling of 2026-11-24 08:07Z on round 25's Status row):
// the shared list shows the option it marks active, on every list it draws.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("Enter on a value of the shared multi-select keeps the option the list marks active wholly inside its box once the list has redrawn, in the project List's Labels filter and the Delivery page's Repository list, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  // Two widths, each with a Labels tick and a Repository tick and untick.
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  for (let label = 1; label <= 20; label += 1) {
    const labelled = await createIssue({ project: "CORE", title: `Labelled ${label}` });
    await patchIssue(labelled.key, { labels: [`lab-${String(label).padStart(2, "0")}`] });
  }
  await seedDeliveryFixture();
  // Fourteen repositories more in the fixture's window, a pull request of the population's each,
  // so the Repository list holds fifteen.
  const pullRequests = Array.from({ length: 14 }, (_, index) => {
    const repo = `acme/r${String(index + 1).padStart(2, "0")}`;
    const number = 100 + index;
    return `('${repo}', ${number}, 'feat: a change in ${repo}', 'https://github.com/${repo}/pull/${number}', 'octocat', '2024-06-01T04:00:00Z', '2024-06-01T05:00:00Z', 1, 1, false, null, ARRAY[]::text[], false)`;
  });
  await sql(
    `INSERT INTO delivery_pull_requests (
       repo, number, title, url, author, created_at, merged_at, additions, deletions, rework, issue_key, sessions, partial
     ) VALUES ${pullRequests.join(", ")}`
  );
  /** Where the option the open list marks active (its text box's `aria-activedescendant`) stands
   *  in the list's scroll box, in that box's content: the option's id, top and bottom, and the
   *  part of the content the box shows - below what it draws stuck to its top (the text box),
   *  above what it draws stuck to its bottom (the Clear foot), and cut to the window - with the
   *  box's scrollTop and height. `inside` is whether the option stands wholly in that part. */
  const activeOptionSight = () =>
    page.evaluate(() => {
      const id = document.activeElement?.getAttribute("aria-activedescendant") ?? "";
      const option = id === "" ? null : document.getElementById(id);
      const none = {
        bottom: 0,
        height: 0,
        id,
        inside: false,
        name: "no option",
        scrollTop: 0,
        shownBottom: 0,
        shownTop: 0,
        top: 0,
      };
      if (option === null) return none;
      const spoken = option.getAttribute("aria-label") ?? option.textContent ?? "";
      const name = `"${spoken.replace("✓", "").replace(/\s+/g, " ").trim()}"`;
      let found = option.parentElement;
      while (found !== null && !/(auto|scroll)/.test(getComputedStyle(found).overflowY)) {
        found = found.parentElement;
      }
      if (found === null) return { ...none, name };
      const box = found;
      const boxTop = box.getBoundingClientRect().top + box.clientTop;
      let shownTop = Math.max(boxTop, 0);
      let shownBottom = Math.min(boxTop + box.clientHeight, window.innerHeight);
      for (const child of box.children) {
        if (getComputedStyle(child).position !== "sticky") continue;
        const stuck = child.getBoundingClientRect();
        if (stuck.top <= boxTop + 1) shownTop = Math.max(shownTop, stuck.bottom);
        else if (stuck.bottom >= boxTop + box.clientHeight - 1) {
          shownBottom = Math.min(shownBottom, stuck.top);
        }
      }
      const own = option.getBoundingClientRect();
      const content = (y: number) => Math.round(y - boxTop + box.scrollTop);
      return {
        bottom: content(own.bottom),
        height: box.clientHeight,
        id,
        inside: own.top >= shownTop - 1 && own.bottom <= shownBottom + 1,
        name,
        scrollTop: Math.round(box.scrollTop),
        shownBottom: content(shownBottom),
        shownTop: content(shownTop),
        top: content(own.top),
      };
    });
  // Each list: how it is reached, its options, its Clear foot, the count its options carry once
  // the rows they count have answered, and the Enters pressed on the option six ArrowDown mark
  // active, which stands at the bottom of the box (its box shows five options).
  const lists = [
    {
      name: "the project List's Labels filter",
      listbox: "Labels options",
      options: 20,
      clear: "Clear labels",
      counted: /, \d+ issues?$/,
      enters: [{ change: "ticks it, the first value ticked", selected: "true", foot: 1 }],
      reach: async () => {
        await openIssuesTab(page, "CORE", "List");
        await expect(page.locator("[data-issue-row]").first()).toBeVisible();
        return filterPicker(page, "Labels");
      },
    },
    {
      name: "the Delivery page's Repository list",
      listbox: "Repository options",
      options: 15,
      clear: "Clear repository",
      counted: /, \d+ PRs?$/,
      enters: [
        { change: "ticks it, which lists it first", selected: "true", foot: 1 },
        { change: "unticks it, which lists it at its own place", selected: "false", foot: 0 },
      ],
      reach: async () => {
        await page.goto(`/delivery?${TIMELINE_FIXTURE_QUERY}&mode=list`);
        await expect(page.locator("[data-pr-row]").first()).toBeVisible();
        const panel = await openFilterPanel(page);
        return panel.getByRole("button", { name: /^Repository:/ });
      },
    },
  ];
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const list of lists) {
      const where = `${list.name} at ${size.width}x${size.height}`;
      const trigger = await list.reach();
      await expect(trigger).toBeVisible();
      await trigger.focus();
      await page.keyboard.press("Enter");
      const listbox = page.getByRole("listbox", { name: list.listbox });
      await expect(listbox.getByRole("option")).toHaveCount(list.options);
      for (let step = 0; step < 6; step += 1) {
        await page.keyboard.press("ArrowDown");
        await nextFrames(page);
      }
      // The Enter below is pressed on an option that stands whole at the bottom of the box.
      await expect
        .poll(
          () =>
            activeOptionSight().then((sight) => Math.abs(sight.shownBottom - sight.bottom) <= 1),
          { message: `${where}: six ArrowDown left the active option off the bottom of its box` }
        )
        .toBe(true);
      const marked = await activeOptionSight();
      const option = page.locator(`[id="${marked.id}"]`);
      const foot = listbox.locator("xpath=..").getByRole("button", { name: list.clear });
      for (const enter of list.enters) {
        await page.keyboard.press("Enter");
        // Redrawn: the option says what the Enter made of it, the foot stands or has gone, and
        // the option carries the count of the rows the new narrowing lists.
        await expect(option).toHaveAttribute("aria-selected", enter.selected);
        await expect(foot).toHaveCount(enter.foot);
        await expect(option).toHaveAttribute("aria-label", list.counted);
        await nextFrames(page);
        // One read is the answer: the list has redrawn, and nothing else scrolls the box.
        const sight = await activeOptionSight();
        if (!sight.inside) {
          wrong.push(
            `${where}: after ArrowDown to ${marked.name} and Enter that ${enter.change}, the list marks ${sight.name} active at ${sight.top}-${sight.bottom} px of its content, and its box shows ${sight.shownTop}-${sight.shownBottom} px of it (scrollTop ${sight.scrollTop}, height ${sight.height}): the active option is out of sight`
          );
        }
      }
      await page.keyboard.press("Escape");
      await expect(listbox).toHaveCount(0);
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the Delivery page opened over the session-groups fixture, and its window changed while the live socket holds the new window's answer.
// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-24 08:07Z: a count
// beside a value is drawn only once the data it counts has answered, and the class search's C14
// (a previous answer's counts are not shown after a change). Measured in builds Jobs
// infra-3135-c-h-n62l7 (head 8e7e463a), infra-3135-c-b-qzzxg (34dc6a07) and infra-3135-c-m-672j8
// (main 0dbe5048), in Chromium, Firefox and WebKit at 1440x900: with the window changed to Last 7
// days and its timeline answer held, the Filter panel and the Own colour list draw no count, but
// the session-groups editor its **Edit groups** opens lists "Sessions in this window" with the
// previous window's sessions and counts ("2 PRs"). **Edit groups** is offered whenever an answer is
// on screen (DeliveryPage.tsx:601-609, `data === undefined ? null`), with no `counted` gate, and
// the editor is handed that answer's counts (`counts={data.session_pr_counts ?? {}}`, :615-621),
// which it draws beside each session (SessionGroupsEditor.tsx:290, :312). Once the new window
// answers, the editor lists that window's own sessions and counts, which this row reads too, so it
// can tell a count from a placeholder.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("the Delivery page's session-groups editor draws no count from the previous window while a new window's answer is held @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedSessionGroupsFixture();
  const main = page.getByTestId("main-content");
  /** Whether a frame the page sends is a timeline view's subscribe, which `holding` keeps. */
  const timelineSubscribe = (message: string | Buffer): boolean => {
    try {
      const frame = JSON.parse(String(message)) as { type?: string; view?: { kind?: string } };
      return frame.type === "subscribe" && frame.view?.kind === "timeline";
    } catch {
      return false;
    }
  };
  // One route for the page's live socket: it passes every frame through to the server, and while
  // `holding` keeps each timeline subscribe the page sends, so the new window has no answer.
  let holding = false;
  const held: (() => void)[] = [];
  await page.routeWebSocket(/\/ws\/live$/u, (socket) => {
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      if (holding && timelineSubscribe(message)) {
        held.push(() => server.send(message));
        return;
      }
      server.send(message);
    });
  });
  /** Each row of the editor's "Sessions in this window", by its text, read once the list holds
   *  `count` rows, so the read is of a list that is whole. */
  const sessions = async (count: number): Promise<string[]> => {
    await openFilterPanel(page, main);
    await page.getByRole("button", { name: "Edit groups" }).click();
    const editor = page.getByRole("dialog", { name: "Session groups" });
    const rows = editor
      .getByRole("list", { name: "Sessions in this window" })
      .getByRole("listitem");
    await expect(rows).toHaveCount(count);
    const names = await rows.evaluateAll((elements) =>
      elements.map((element) => (element.textContent ?? "").replace(/\s+/g, " ").trim())
    );
    await editor.getByRole("button", { name: "Close session groups" }).click();
    await expect(editor).toHaveCount(0);
    await closeControlPanel(main);
    return names;
  };
  await page.goto("/delivery");
  await expect(main.getByText("3 PRs in current filter/window")).toBeVisible();
  // The control: the default window's answer counts each of its three sessions.
  const answered = await sessions(3);
  expect(answered.filter((name) => /\b1 PR$/u.test(name))).toHaveLength(3);
  holding = true;
  await pickFromList(listTrigger(page, "Window"), "Last 7 days");
  await expect(page).toHaveURL(/[?&]window=7d/u);
  await expect.poll(() => held.length).toBeGreaterThan(0);
  await expect(main.getByText("Counting PRs in current filter/window…")).toBeVisible();
  const wrong = (await sessions(3))
    .filter((name) => /\b\d+ PRs?\b/u.test(name))
    .map((name) => `window held: the editor offers "${name}" before the new window answered`);
  holding = false;
  for (const send of held.splice(0)) send();
  await expect(main.getByText("2 PRs in current filter/window")).toBeVisible();
  // The new window's own answer: its two sessions, each with its count, and no Reviewer.
  const sevenDays = await sessions(2);
  expect(
    sevenDays.filter((name) => /^Planner, (first|second) run\b.*\b1 PR$/u.test(name))
  ).toHaveLength(2);
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the project List opened with the agent list held, and its Claimed by filter read before and after the list answers.
// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-24 08:07Z and the
// round-23 rule (an instance in main's code, in a file this pull request changes, is fixed here).
// Measured in builds Jobs infra-3135-c-h-n62l7 (head 8e7e463a) and infra-3135-c-m-672j8 (main
// 0dbe5048), and infra-3135-c-b-qzzxg (34dc6a07), in Chromium, Firefox and WebKit at 1440x900: with
// the agent list (`GET /api/v1/agents`) held, the Claimed by filter offers "Nobody or lapsed, 1
// issue" over an unclaimed issue and one claimed by a session that is no longer running; once the
// list answers it offers "Nobody or lapsed, 2 issues". Which claims have lapsed is the agent
// list's answer (IssuesTable.tsx:196-199); until it answers every claim is judged held
// (issues-table-data.ts:150, `claimIsTakeable` over a registry whose states are `unknown`), and the
// table hands its header filters the counts with no gate (`filterCounts={openFacets}`, no
// `filterCounted`, IssuesTable.tsx:596).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("the project List's Claimed by filter draws no count of the lapsed claims before the agent list answers @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Nobody holds this" });
  const gone = await createIssue({ project: "CORE", title: "Held by a session that is gone" });
  const token = await mintAgentToken("claim-lapsed", "alice");
  const holder = { session_id: "e2e-claim-lapsed", title: "Lapsed holder" };
  await setLiveSessions([holder]);
  await claimIssue(gone.key, {
    actor: { id: holder.session_id, kind: "session", origin: { session_title: holder.title } },
    as: "agent",
    token,
  });
  await setLiveSessions([]);
  // The agent list is held until the row lets it through.
  let holding = true;
  const held: Route[] = [];
  await page.route(
    (url) => url.pathname === "/api/v1/agents",
    async (route) => {
      if (holding) {
        held.push(route);
        return;
      }
      await route.continue();
    }
  );
  /** Each option of the Claimed by filter, by its accessible name, read once it holds all three. */
  const claimOptions = async (): Promise<string[]> => {
    const trigger = page.getByRole("button", { name: /^Filter Claimed by(?:: \d+ picked)?$/u });
    await trigger.click();
    const options = listOf(trigger).getByRole("option");
    await expect(options).toHaveCount(3);
    const names = await options.evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("aria-label") ?? "")
    );
    await page.keyboard.press("Escape");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    return names;
  };
  await page.goto("/projects/CORE/issues");
  await expect(page.locator(`[data-issue-row="${gone.key}"]`)).toBeVisible();
  const wrong = (await claimOptions())
    .filter((name) => /^Nobody or lapsed, \d+ issues?$/u.test(name))
    .map((name) => `agent list out: Claimed by offers "${name}"`);
  // The agent list was asked for and is held: what the filter offered is the unanswered state.
  await expect.poll(() => held.length).toBeGreaterThan(0);
  holding = false;
  for (const route of held.splice(0)) await route.continue();
  // The control: once the list answers, the lapsed claim counts beside the unclaimed issue.
  await expect.poll(claimOptions).toContain("Nobody or lapsed, 2 issues");
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// SLOW(>=2s): the project List opened over two open and two closed issues, with Done added while the closed issues read is held.
// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-24 08:07Z (never a
// count of the rows read so far) and the round-23 rule. Measured in builds Jobs
// infra-3135-c3-h-pwg42 (head 8e7e463a), infra-3135-c3-b-w7gw5 (34dc6a07) and infra-3135-c3-m-9gdl4
// (main 0dbe5048), in Chromium, Firefox and WebKit at 1440x900: with Done added to the Status
// filter and the closed issues read (`GET /api/v1/issues?project=CORE` without `open`) held, the
// page says "Loading closed issues…" and Status offers "Done, not loaded", but Assignee offers
// "alice, 1 issue" and Priority "P1, 1 issue", counts of the open issues alone; once the closed
// issues answer they read "alice, 3 issues" and "P1, 3 issues". The table is drawn from the open
// read while the closed one is out (IssuesTable.tsx:211-213), only Done is marked unread
// (`DONE_UNLOADED`, :69, :597), and every other value is counted over the rows read so far
// (`filterCounts={openFacets}`, :596, with no `filterCounted`; the `counted` of :249 gates the
// Showing chip alone).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
test("the project List's header filters draw no count while the closed issues they count are out @firefox @webkit", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const open = await createIssue({ project: "CORE", title: "Open work for alice" });
  await patchIssue(open.key, { assignee: "alice", priority: 1 });
  const other = await createIssue({ project: "CORE", title: "Open work for bob" });
  await patchIssue(other.key, { assignee: "bob", priority: 2 });
  const closed: string[] = [];
  for (const title of ["Closed work one", "Closed work two"]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { assignee: "alice", priority: 1 });
    await changeIssueStatus(issue.key, { status: "done" });
    closed.push(issue.key);
  }
  // The closed issues read - the project's whole list, asked for once Done is picked - is held
  // until the row lets it through; the open issues read goes through.
  let holding = true;
  const held: Route[] = [];
  await page.route(
    (url) =>
      url.pathname === "/api/v1/issues" &&
      url.searchParams.get("project") === "CORE" &&
      !url.searchParams.has("open"),
    async (route) => {
      if (holding) {
        held.push(route);
        return;
      }
      await route.continue();
    }
  );
  /** Each option of one header filter, by its accessible name, read once it holds `count`. */
  const headerOptions = async (header: string, count: number): Promise<string[]> => {
    const trigger = page.getByRole("button", {
      name: new RegExp(`^Filter ${header}(?:: \\d+ picked)?$`, "u"),
    });
    await trigger.click();
    const options = listOf(trigger).getByRole("option");
    await expect(options).toHaveCount(count);
    const names = await options.evaluateAll((elements) =>
      elements.map((element) => element.getAttribute("aria-label") ?? "")
    );
    await page.keyboard.press("Escape");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    return names;
  };
  await page.goto("/projects/CORE/issues");
  await expect(page.locator(`[data-issue-row="${other.key}"]`)).toBeVisible();
  const status = page.getByRole("button", { name: /^Filter Status(?:: \d+ picked)?$/u });
  await status.click();
  await listOf(status)
    .getByRole("option", { name: /^Done\b/u })
    .click();
  await page.keyboard.press("Escape");
  await expect(page).toHaveURL(/[?&]status=done/u);
  await expect(page.getByText("Loading closed issues…")).toBeVisible();
  await expect.poll(() => held.length).toBeGreaterThan(0);
  const wrong: string[] = [];
  for (const header of ["Assignee", "Priority"]) {
    wrong.push(
      ...(await headerOptions(header, 2))
        .filter((name) => /, \d+ issues?$/u.test(name))
        .map((name) => `closed issues out: ${header} offers "${name}"`)
    );
  }
  holding = false;
  for (const route of held.splice(0)) await route.continue();
  await expect(page.getByText("Loading closed issues…")).toHaveCount(0);
  await expect(page.locator(`[data-issue-row="${closed[0]}"]`)).toBeVisible();
  // The control: with the closed issues read, each value counts them too.
  expect(await headerOptions("Assignee", 2)).toEqual(["alice, 3 issues", "bob, 1 issue"]);
  expect(await headerOptions("Priority", 2)).toEqual(["P1, 3 issues", "P2, 1 issue"]);
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// INFRA-3135, round 26 (the tester's): 5a0806a1 and 1946cb1c leave the pick the cause when a
// second press lands on a control the pick's write holds, and the round-25 red row holds the
// bar's landing then to where the pick alone puts it, never the body. Measured at 8e7e463a in
// builds Jobs infra-3135-f-h-w2-cpzw7 and infra-3135-f-h-d-q7gm7, and at 34dc6a07 in
// infra-3135-f-b-w2-tdtlg and infra-3135-f-b-d-tm7ct, in Chromium, Firefox and WebKit at 1440x900
// and 390x664: under the project List's Epic filter, with Take out of epic's writes held, a
// double click or two clicks on the held Start next of a marked row move the focus onto that
// Start next, and once the writes land and take the row away the focus is on the body; the same
// press with no second one puts it on the checkbox of the row after the marked two. The focus is
// then on the row's Start next and not in the bar, so the bar's hand-off has nothing to hand on
// (components/SelectionBar.tsx:375-381), and the List's row hand-off reads the loss as no press of
// the reader's (components/focus-place.ts:296): `causedByReader` (:1142-1144) reads the lost
// control, its rows and its region, which hold neither Take out of epic nor a root it was armed
// at, so nothing lands the focus. With the focus put back on Take out of epic after the clicks, as
// Safari's click leaves it, 8e7e463a lands on the row after the marked two and 34dc6a07 on the
// body, so 1946cb1c holds the press; it is the landing that is missing.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): two projects of thirteen issues, and six Take out of epic presses with held writes.
test("a double click or two clicks on a marked row's held Start next while Take out of epic's writes are out leave the focus where Take out of epic alone puts it once the bar goes, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  // Every issue write sent while `holding` is held until the row lets it through, so the second
  // presses land while Take out of epic's writes are out.
  let holding = false;
  const held: Route[] = [];
  await page.route(/\/api\/v1\/issues\/[^/?]+$/, async (route) => {
    if (route.request().method() !== "PATCH" || !holding) return route.fallback();
    held.push(route);
  });
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const rows = page.locator("[data-issue-row]");
  /** Where the keyboard focus rests: the body, or the focused element and the row it is in, by
   *  the names `slots` gives the rows read before the press. */
  const landingOf = (slots: Record<string, string>) =>
    page.evaluate((slots) => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) return "the body";
      const label =
        focused instanceof HTMLInputElement ? focused.labels?.[0]?.textContent : undefined;
      const words = (focused.getAttribute("aria-label") || label || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      const row = focused.closest("[data-issue-row]")?.getAttribute("data-issue-row");
      const control = row == null ? words : words.replace(row, "").trim();
      const name = `${focused.tagName.toLowerCase()} "${control}"`;
      if (row === undefined || row === null) return `${name} in no row`;
      return `${name} in ${slots[row] ?? `row ${row}`}`;
    }, slots);
  const got: string[] = [];
  const want: string[] = [];
  for (const [step, size] of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ].entries()) {
    await page.setViewportSize(size);
    // One project per width: an epic and twelve tasks under it, each in Legion's queue, so every
    // row but the queue's first draws a Start next.
    const key = step === 0 ? "SNA" : "SNB";
    await createProject({ key, name: `Start next ${size.width}` });
    const epic = await createIssue({ project: key, title: `Queue epic ${key}` });
    await patchIssue(epic.key, { labels: ["epic"] });
    for (let index = 0; index < 12; index++) {
      const task = await createIssue({
        parent: epic.key,
        project: key,
        title: `Queued ${key} task ${index}`,
      });
      await patchIssue(task.key, { labels: ["legion"], status: "todo" });
    }
    const view = `/projects/${key}/issues?f.epic=${encodeURIComponent(`${epic.key} · Queue epic ${key}`)}`;
    let alone = "";
    for (const press of ["no second press", "a double click", "two clicks"] as const) {
      const where = `project List at ${size.width}x${size.height} with ${press} on a marked row's held Start next`;
      await page.goto(view);
      await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(4);
      // The order the rows stand in once the poll above found them drawn: nothing moves them
      // before the marks below, and it only names the rows.
      const order = await rows.evaluateAll((nodes) =>
        nodes.map((node) => node.getAttribute("data-issue-row") ?? "")
      );
      const [above = "", upper = "", lower = "", after = ""] = order;
      const slots = {
        [above]: "the row above the marked two",
        [upper]: "the upper marked row",
        [lower]: "the lower marked row",
        [after]: "the row after the marked two",
      };
      // The lower row first, so the row marked last is the upper one, whose place the focus takes.
      for (const issue of [lower, upper]) {
        const mark = page.getByRole("checkbox", { exact: true, name: `Select ${issue}` });
        await mark.focus();
        await page.keyboard.press("Space");
        await expect(mark).toBeChecked();
      }
      await expect(bar).toContainText("2 selected");
      const takeOut = bar.getByRole("button", { exact: true, name: "Take out of epic" });
      await takeOut.focus();
      holding = true;
      await page.keyboard.press("Enter");
      await expect
        .poll(() => held.length, { message: `${where}: Take out of epic sent no write` })
        .toBeGreaterThan(0);
      const startNext = page
        .locator(`[data-issue-row="${upper}"]`)
        .getByRole("button", { exact: true, name: "Start next" });
      await expect(startNext, `${where}: the marked row's Start next is not held`).toHaveAttribute(
        "aria-disabled",
        "true"
      );
      if (press === "a double click") await startNext.dblclick({ force: true });
      if (press === "two clicks") {
        await startNext.click({ force: true });
        await startNext.click({ force: true });
      }
      await nextFrames(page);
      holding = false;
      for (const route of held.splice(0)) await route.continue();
      await expect(bar).toHaveCount(0);
      await expect(page.locator(`[data-issue-row="${upper}"]`)).toHaveCount(0);
      // The bar's hand-off runs after the commit that took the bar away, so it is done after
      // these frames.
      await nextFrames(page);
      await nextFrames(page);
      // One read is the answer: the writes have landed, the bar has gone and every hand-off of
      // the press has run, so nothing moves the focus after this.
      const landing = await landingOf(slots);
      if (press === "no second press") {
        alone = landing;
      } else {
        got.push(`${where}: ${landing}`);
        want.push(`${where}: ${alone}`);
      }
    }
  }
  // A keyboard reader whose second press of a held control lands their focus on the body starts
  // their next Tab from the top of the page, far from the rows they were working in.
  expect(got, got.join("; ")).toEqual(want);
});

// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-24 10:03Z ("Round 25
// failed"), which put into round 26 the armed hand-off's partly-in-sight landing, scrolled to its
// nearest edge (the ruling of 2026-11-21 12:3xZ). Measured at 8e7e463a in builds Job
// infra-3135-f-h-w2-cpzw7 and at 34dc6a07 in infra-3135-f-b-w2-tdtlg, in Chromium, Firefox and
// WebKit at 390x664: under the project List's Epic filter, with the mark of the row marked last
// cut by 30% of its height at the bottom edge of the table's scroll box, Take out of epic lands the
// focus on the checkbox of the row now at its place, "Select CORE-21", top 458 to bottom 471 in a
// box whose visible bottom is at 467, and scrolls nothing. The selection bar's hand-off takes the
// focus with no scroll and never shows its landing whole (components/SelectionBar.tsx:137), where
// the other hand-offs of this pull request land through `land`, which shows a control partly in
// sight whole at its nearest edge (components/focus-place.ts:486-503). This row makes the same cut
// at 1440x900 too.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-26 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): forty-five seeded issues, and two Take out of epic presses read once the box settles.
test("Take out of epic with the row marked last cut by the table box's bottom edge hands the focus to a control wholly inside every box that clips it, on the project List at 390 and 1440 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 44; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  const view = `/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`;
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const markOf = (key: string) =>
    page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
  /** The issues the table draws, top to bottom, and of them those whose checkbox the reader sees:
   *  inside the window and the table's box, with nothing, such as the sticky head, drawn over its
   *  centre. */
  const drawn = () =>
    page.evaluate(() => {
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      const frame = scroller?.getBoundingClientRect();
      const listed = [...document.querySelectorAll<HTMLElement>("[data-issue-row]")];
      const topOf = (row: HTMLElement) => row.getBoundingClientRect().top;
      listed.sort((one, other) => topOf(one) - topOf(other));
      const seen = (row: HTMLElement) => {
        const mark = row.querySelector('input[type="checkbox"]');
        if (mark === null || frame === undefined) return false;
        const own = mark.getBoundingClientRect();
        const above = Math.max(0, frame.top);
        const below = Math.min(window.innerHeight, frame.bottom);
        if (own.top < above || own.bottom > below) return false;
        const hit = document.elementFromPoint(own.left + own.width / 2, own.top + own.height / 2);
        return hit !== null && row.contains(hit);
      };
      return {
        order: listed.map((row) => row.getAttribute("data-issue-row") ?? ""),
        shown: listed.filter(seen).map((row) => row.getAttribute("data-issue-row") ?? ""),
      };
    });
  /** How a control stands against the window and every box around it that clips it: the mark of
   *  the row `key` names, else the focused element. `whole` says whether it lies wholly inside
   *  all of them, and `text` names it, its rect and each edge that cuts it. */
  const standing = (key?: string) =>
    page.evaluate((key) => {
      const element =
        key === null
          ? document.activeElement
          : document.querySelector(`[data-issue-row="${CSS.escape(key)}"] input[type="checkbox"]`);
      if (!(element instanceof HTMLElement) || element === document.body) {
        return { text: "the body", whole: false };
      }
      const round = (value: number) => Math.round(value * 10) / 10;
      const own = element.getBoundingClientRect();
      const label =
        element instanceof HTMLInputElement ? element.labels?.[0]?.textContent?.trim() : undefined;
      const name = element.getAttribute("aria-label") || label || element.tagName.toLowerCase();
      const cuts: string[] = [];
      const cutBy = (box: string, clip: Omit<DOMRectReadOnly, "toJSON" | "x" | "y">) => {
        if (own.top < clip.top - 0.5) cuts.push(`${box}'s top edge at ${round(clip.top)}`);
        if (own.bottom > clip.bottom + 0.5) {
          cuts.push(`${box}'s bottom edge at ${round(clip.bottom)}`);
        }
        if (own.left < clip.left - 0.5) cuts.push(`${box}'s left edge at ${round(clip.left)}`);
        if (own.right > clip.right + 0.5) cuts.push(`${box}'s right edge at ${round(clip.right)}`);
      };
      cutBy("the window", {
        bottom: window.innerHeight,
        height: window.innerHeight,
        left: 0,
        right: window.innerWidth,
        top: 0,
        width: window.innerWidth,
      });
      for (let around = element.parentElement; around !== null; around = around.parentElement) {
        const style = getComputedStyle(around);
        if (style.overflowX !== "visible" || style.overflowY !== "visible") {
          cutBy(
            `the box ${around.dataset.testid ?? around.tagName.toLowerCase()}`,
            around.getBoundingClientRect()
          );
        }
      }
      const rect = `top ${round(own.top)} to bottom ${round(own.bottom)}, left ${round(own.left)} to right ${round(own.right)}`;
      const verdict =
        cuts.length === 0
          ? "wholly inside every box that clips it"
          : `cut by ${cuts.join(" and ")}`;
      return {
        text: `${element.tagName.toLowerCase()} "${name}" at ${rect}, ${verdict}`,
        whole: cuts.length === 0,
      };
    }, key ?? null);
  const landings: { text: string; whole: boolean }[] = [];
  for (const size of [
    { height: 664, width: 390 },
    { height: 900, width: 1440 },
  ]) {
    await page.setViewportSize(size);
    const where = `project List at ${size.width}x${size.height}`;
    await page.goto(view);
    await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(12);
    // The window brings the table's box to just below the app's 56 px header, and the box
    // scrolls half way down its rows, so rows stand in it above and below the ones marked.
    await box.evaluate((node) => {
      window.scrollBy(0, node.getBoundingClientRect().top - 60);
      node.scrollTop = (node.scrollHeight - node.clientHeight) / 2;
    });
    await settledScrollTop(page);
    await settledScrollTop(box);
    await expect.poll(async () => (await drawn()).shown.length).toBeGreaterThanOrEqual(3);
    // The rows read once the box has held still and the poll above found them in sight; they
    // only name the two rows marked below, the middle one in sight and the one after it.
    const { order, shown } = await drawn();
    const at = order.indexOf(shown[Math.floor(shown.length / 2)] ?? "");
    const upper = order[at] ?? "";
    const lower = order[at + 1] ?? "";
    // The lower row first, so the row marked last is the upper one, whose place the focus takes.
    for (const key of [lower, upper]) {
      await markOf(key).focus();
      await page.keyboard.press("Space");
      await expect(markOf(key)).toBeChecked();
    }
    await expect(bar).toContainText("2 selected");
    // The box scrolls so that 30% of the upper row's mark stands below the box's visible bottom:
    // the row after the marked two moves up into that place when they go, cut the same way, and
    // the centre of its mark stays in sight, so the bar's hand-off takes it.
    await markOf(upper).evaluate((mark) => {
      const scroller = mark.closest<HTMLElement>('[data-testid="issues-table-scroll"]');
      const own = mark.getBoundingClientRect();
      const bottom = Math.min(window.innerHeight, scroller?.getBoundingClientRect().bottom ?? 0);
      scroller?.scrollBy(0, own.bottom - (bottom + own.height * 0.3));
    });
    await settledScrollTop(box);
    // One read is the answer: the box has held still, and the cut is checked before the press so
    // that a case the page cannot make fails here rather than passing.
    const arranged = await standing(upper);
    expect(arranged.whole, `${where}: the row marked last is not cut: ${arranged.text}`).toBe(
      false
    );
    await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
    await page.keyboard.press("Enter");
    await expect(bar).toHaveCount(0);
    await expect(markOf(upper)).toHaveCount(0);
    // The bar's hand-off runs after the commit that took the bar away, so it is done after these
    // frames.
    await nextFrames(page);
    await nextFrames(page);
    // One read is the answer: the writes have landed, the bar has gone and every hand-off of the
    // press has run, so nothing moves the focus or the box after this.
    const landing = await standing();
    landings.push({ text: `${where}: the focus is on ${landing.text}`, whole: landing.whole });
  }
  // A landing the box cuts is a control the reader cannot read whole: the press that took their
  // rows away leaves them looking for the focus at the box's edge.
  expect(
    landings.map((landing) => landing.whole),
    landings.map((landing) => landing.text).join("; ")
  ).toEqual(landings.map(() => true));
});

// INFRA-3135, round 26 (the tester's), by the architect's ruling of 2026-11-25 04:59Z: a click of
// the reader's that puts the focus on the body while a Board move's dry run is out is the reader
// going elsewhere, so nothing moves the focus once the move lands; the body is the place the press
// left only when the press's own card went away. Measured at 8e7e463a in builds Jobs
// infra-3135-f-h-ch-wlcgj (Chromium) and infra-3135-f-h-fw-7ssrl (WebKit) at 390x664, with a
// click that landed below the window's bottom, and in infra-3135-f-red2-qhtr5 with a click on a
// blank spot of the shell's header: a child's card moved by Shift+L, with the rule's dry run held
// and the focus put on the body by that click meanwhile, took the focus from the body to the
// moved card once the move landed. A click inside the main content puts the focus on the main
// content itself (`tabIndex={-1}`), which the effect leaves where it is. 34dc6a07
// (infra-3135-f-b-ch-cb98p, infra-3135-f-b-fw-t8fw8) sends no dry run, so the case cannot be made
// there. The move's focus effect counts any focus on the body, once the card it moved has left
// the page, as where the press left it (features/project/IssueBoard.tsx:521), so it cannot tell
// the reader's own click from the card's going. Firefox kept the focus on the card at the click
// the first Jobs sent, so the row runs in Chromium and WebKit, as the ruling asks.
// Its engines: the clause of the `webkit` project that names the rows INFRA-3135's round-26
// tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): two projects of three issues on the Board, and two keyboard moves with held dry runs.
test("a click that puts the focus on the body while a Board move's dry run is out keeps it there once the move lands, where the same move with no click takes it to the moved card, at 390 @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ height: 664, width: 390 });
  // Every dry run of a status change sent while `holding` is held until the row lets it through.
  let holding = false;
  const held: Route[] = [];
  await page.route(/\/api\/v1\/issues\/[^/?]+\/status-change$/, async (route) => {
    const dryRun = (route.request().postData() ?? "").includes('"dry_run":true');
    if (!holding || !dryRun) return route.fallback();
    held.push(route);
  });
  /** What holds the keyboard focus: the body, or its tag and name. */
  const focusName = () =>
    page.evaluate(() => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) return "the body";
      const words = (focused.getAttribute("aria-label") || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      return `${focused.tagName.toLowerCase()} "${words}"`;
    });
  /** A point of the window where the element drawn, and every element around it, takes no focus,
   *  outside the main content and the board's columns: a click there leaves no control focused,
   *  and Chromium and WebKit put the focus on the body. */
  const blankSpot = () =>
    page.evaluate(() => {
      const takesFocus =
        "a[href], button, input, select, textarea, summary, iframe, label, [tabindex], [contenteditable]";
      for (let y = 4; y < window.innerHeight; y += 8) {
        for (let x = 4; x < window.innerWidth; x += 8) {
          const hit = document.elementFromPoint(x, y);
          if (
            hit !== null &&
            hit.closest(takesFocus) === null &&
            hit.closest("main, [data-board-column]") === null
          ) {
            return { x, y };
          }
        }
      }
      return null;
    });
  for (const press of ["no click", "a click on a blank spot"] as const) {
    const where = `Board at 390x664 with ${press} while the dry run is out`;
    // One project per press: a parent in work and a child under it, so the child's move is one
    // the rule's dry run checks first, and one more card in the same column.
    const key = press === "no click" ? "BDNC" : "BDCL";
    await createProject({ key, name: `Board ${press}` });
    const parent = await createIssue({ project: key, title: `Parent ${key}` });
    const child = await createIssue({ parent: parent.key, project: key, title: `Child ${key}` });
    const other = await createIssue({ project: key, title: `Other ${key}` });
    for (const issue of [parent, child, other]) await patchIssue(issue.key, { status: "todo" });
    await openIssuesTab(page, key, "Board");
    const card = page.locator(`[data-board-card="${child.key}"]`);
    await card.focus();
    await expect(card).toBeFocused();
    holding = true;
    await page.keyboard.press("Shift+L");
    await expect
      .poll(() => held.length, { message: `${where}: the move sent no dry run` })
      .toBeGreaterThan(0);
    let afterClick = "";
    if (press === "a click on a blank spot") {
      // The reader scrolls the window back to the top, which moves no focus, and clicks a blank
      // spot of the shell's header there: the main content takes the focus at a click anywhere
      // inside it (`app.tsx` gives it `tabIndex={-1}`), and the header is outside it.
      await page.evaluate(() => window.scrollTo(0, 0));
      await settledScrollTop(page);
      // The spot is read once the window has held still at its top, and nothing is drawn over
      // the page while the dry run is held.
      const spot = await blankSpot();
      expect(spot, `${where}: no point of the window takes no focus`).not.toBeNull();
      await page.mouse.click(spot?.x ?? 0, spot?.y ?? 0);
      await nextFrames(page);
      // One read is the answer: the click's change of focus is done after the frames above, and
      // nothing else moves the focus while the dry run is held.
      afterClick = await focusName();
    }
    holding = false;
    for (const route of held.splice(0)) await route.continue();
    await expect(
      page.locator(`[data-board-column="in_progress"] [data-board-card="${child.key}"]`),
      `${where}: the card did not move to In progress`
    ).toHaveCount(1);
    // The move's focus effect runs with the render that draws the card in its column, so it is
    // done after these frames.
    await nextFrames(page);
    await nextFrames(page);
    if (press === "no click") {
      await expect(
        card,
        `${where}: the move did not take the focus to the moved card`
      ).toBeFocused();
    } else {
      // One read is the answer: the move has landed and its focus effect has run, so nothing
      // moves the focus after this.
      const afterMove = await focusName();
      expect(
        afterMove,
        `${where}: the click left the focus on ${afterClick}, and the move took it to ${afterMove}`
      ).toBe(afterClick);
    }
  }
});

// INFRA-3135, round 27 (the tester's), by the architect's ruling of 2026-11-25 13:01Z: "a count is
// drawn once every read it counts has answered; a read that a count does not count never hides it
// (the architect, 2026-11-25 13:01Z)". Measured in builds Jobs infra-3135-c-h-kwghj and
// infra-3135-c2-h-qmwvp (head 7deb75af) and infra-3135-c-b-pcmr9 and infra-3135-c2-b-25xqq
// (5ae79d88), in Chromium, Firefox and WebKit at 1440x900: with the agent list
// (`GET /api/v1/agents`) answered 500 every time and Nobody or lapsed picked, 7deb75af draws no
// count in any header filter for good, after the page has said that it could not check which
// claims have lapsed. Over three issues that no one claims, Assignee offers "alice" where 5ae79d88
// offers "alice, 3 issues", and Claimed by offers "Nobody" where 5ae79d88 offers "Nobody, 3
// issues". Those counts read the issues alone, which answered. The header filters wait for the
// agent registry whenever Claimed by asks for it (`readsRegistry`, IssuesTable.tsx:197), the
// registry's `known` stays false after a read that failed (features/refs/agent-registry.ts:160),
// and `filterCounted = counted && (!readsRegistry || registry.known)` (IssuesTable.tsx:449) then
// hides every count of every filter: the ones the registry decides and the ones it does not.
// 5ae79d88 draws both kinds, the registry's own among them ("Nobody or lapsed, 1 issue" over a
// claim whose session had gone, which it could not judge), so this row holds the counts the
// registry decides to none while it is refused. The control: with the agent list answered, the
// same pages draw every count, Nobody or lapsed and a holder's among them.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-27 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): two projects on the List, each read with the agent list refused and then answered.
test("with the agent list refused, the project List's header filters draw each count the list does not decide once the issues answer, and none that it decides @firefox @webkit", async ({
  page,
}) => {
  // Two projects, each read refused and then answered, five filters each.
  test.setTimeout(180_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  /** One project's three issues: the first and the third for alice at P1 labelled `alpha`, the
   *  second for bob at P2. No title shares a word with another, so none reads as a duplicate. */
  const seed = async (key: string, name: string, titles: readonly [string, string, string]) => {
    await createProject({ key, name });
    const alice = await createIssue({ project: key, title: titles[0] });
    await patchIssue(alice.key, { assignee: "alice", labels: ["alpha"], priority: 1 });
    const bob = await createIssue({ project: key, title: titles[1] });
    await patchIssue(bob.key, { assignee: "bob", priority: 2 });
    const more = await createIssue({ project: key, title: titles[2] });
    await patchIssue(more.key, { assignee: "alice", labels: ["alpha"], priority: 1 });
    return { alice: alice.key, bob: bob.key, more: more.key };
  };
  // CORE: no issue is claimed. HELD: one issue is claimed by a session that runs, so the answered
  // agent list keeps that claim held, and Nobody or lapsed lists the same two issues it lists
  // while the list is refused, where the page counts every claim as held.
  const core = await seed("CORE", "Core", ["Harbour crane", "Signal tower", "Ferry ramp"]);
  const held = await seed("HELD", "Held", ["Granite quarry", "Orchard fence", "Lantern repair"]);
  const holder = { session_id: "e2e-claim-running", title: "Running holder" };
  await setLiveSessions([holder]);
  const token = await mintAgentToken("claim-running", "alice");
  await claimIssue(held.more, {
    actor: { id: holder.session_id, kind: "session", origin: { session_title: holder.title } },
    as: "agent",
    token,
  });
  // The agent list answers 500 every time it is asked while `refusing`, and goes through after.
  let refusing = true;
  let served = 0;
  await page.route(
    (url) => url.pathname === "/api/v1/agents",
    async (route) => {
      if (refusing) {
        await route.fulfill({
          body: '{"error":{"code":"INTERNAL","message":"agent list refused"}}',
          contentType: "application/json",
          status: 500,
        });
        return;
      }
      served += 1;
      await route.continue();
    }
  );
  /** Each option of one header filter, by its accessible name, read once it holds `count`, in
   *  sorted order: an option that draws a count is named by its `aria-label` (`alice, 2 issues`),
   *  and one that draws none by its label's own text (`alice`), its check mark left out. */
  const headerFilterOptions = async (header: string, count: number): Promise<string[]> => {
    const trigger = page.getByRole("button", {
      name: new RegExp(`^Filter ${header}(?:: \\d+ picked)?$`, "u"),
    });
    await trigger.click();
    const options = listOf(trigger).getByRole("option");
    await expect(options).toHaveCount(count);
    const names = await options.evaluateAll((elements) =>
      elements.map(
        (element) =>
          element.getAttribute("aria-label") ??
          Array.from(element.children)
            .filter((child) => child.getAttribute("aria-hidden") !== "true")
            .map((child) => child.textContent ?? "")
            .join("")
            .trim()
      )
    );
    await page.keyboard.press("Escape");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    return names.sort();
  };
  /** An option's value, with its count taken off where it draws one. */
  const optionValue = (name: string) => name.replace(/, \d+ issues?$/u, "");
  /** Each of `wanted` that `offered` does not hold as it is written. */
  const missing = (offered: readonly string[], wanted: readonly string[]) =>
    wanted.filter((want) => !offered.includes(want));
  // Each filter's counts that read the issues alone, once Nobody or lapsed is picked: Status lists
  // every status, so only the one these issues stand in is named, and Claimed by's Nobody counts
  // the issues no one claims. `claimed` is everything Claimed by offers with the list answered.
  const cases = [
    {
      key: "CORE",
      listed: [core.alice, core.bob, core.more],
      hidden: [] as string[],
      filters: [
        { count: 2, header: "Assignee", wanted: ["alice, 2 issues", "bob, 1 issue"] },
        { count: 2, header: "Priority", wanted: ["P1, 2 issues", "P2, 1 issue"] },
        { count: 9, header: "Status", wanted: ["Triage, 3 issues"] },
        { count: 1, header: "Labels", wanted: ["alpha, 2 issues"] },
        { count: 2, header: "Claimed by", wanted: ["Nobody, 3 issues"] },
      ],
      claimed: ["Nobody or lapsed, 3 issues", "Nobody, 3 issues"],
    },
    {
      key: "HELD",
      listed: [held.alice, held.bob],
      hidden: [held.more],
      filters: [
        { count: 2, header: "Assignee", wanted: ["alice, 1 issue", "bob, 1 issue"] },
        { count: 2, header: "Priority", wanted: ["P1, 1 issue", "P2, 1 issue"] },
        { count: 9, header: "Status", wanted: ["Triage, 2 issues"] },
        { count: 1, header: "Labels", wanted: ["alpha, 1 issue"] },
        { count: 3, header: "Claimed by", wanted: ["Nobody, 2 issues"] },
      ],
      claimed: [
        "Nobody or lapsed, 2 issues",
        "Nobody, 2 issues",
        "Running holder (for alice), 1 issue",
      ],
    },
  ];
  const wrong: string[] = [];
  for (const { key, listed, hidden, filters, claimed } of cases) {
    const at = `${key}, agent list refused`;
    refusing = true;
    await page.goto(`/projects/${key}/issues?unclaimed=1`);
    for (const row of listed) await expect(page.locator(`[data-issue-row="${row}"]`)).toBeVisible();
    for (const row of hidden)
      await expect(page.locator(`[data-issue-row="${row}"]`)).toHaveCount(0);
    // The page says the agent list's read failed after its retries, so every read these filters
    // count has answered or been refused.
    await expect(page.getByTestId("unclaimed-unchecked")).toBeVisible();
    for (const { count, header, wanted } of filters) {
      await expect
        .poll(async () => missing(await headerFilterOptions(header, count), wanted), {
          timeout: 5_000,
        })
        .toEqual([])
        .catch(() => undefined);
      // The poll above waited up to five seconds for these counts after every read they count had
      // answered, so this reads what the filter settled on.
      const offered = await headerFilterOptions(header, count);
      wrong.push(
        ...missing(offered, wanted).map(
          (want) =>
            `${at}: ${header} offers "${offered.find((name) => optionValue(name) === optionValue(want)) ?? "no such value"}" where it should offer "${want}"`
        )
      );
    }
    // Read after Claimed by's own poll above, so the panel has drawn whatever it will draw: every
    // value but Nobody counts what the refused agent list decides, and draws no count.
    const claimedOffered = await headerFilterOptions("Claimed by", claimed.length);
    wrong.push(
      ...claimedOffered
        .filter((name) => optionValue(name) !== "Nobody" && optionValue(name) !== name)
        .map((name) => `${at}: Claimed by offers "${name}", a count the refused agent list decides`)
    );
    // The control: with the agent list answered, the same page draws every count, the ones the
    // list decides right.
    served = 0;
    refusing = false;
    await page.goto(`/projects/${key}/issues?unclaimed=1`);
    for (const row of listed) await expect(page.locator(`[data-issue-row="${row}"]`)).toBeVisible();
    await expect.poll(() => served).toBeGreaterThan(0);
    for (const { count, header, wanted } of filters) {
      await expect
        .poll(async () => missing(await headerFilterOptions(header, count), wanted), {
          message: `${key}, agent list answered: ${header} draws ${wanted.join(" and ")}`,
        })
        .toEqual([]);
    }
    await expect
      .poll(() => headerFilterOptions("Claimed by", claimed.length), {
        message: `${key}, agent list answered: Claimed by draws ${claimed.join(" and ")}`,
      })
      .toEqual(claimed);
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// INFRA-3135, round 27 (the tester's), by the architect's ruling of 2026-11-25 04:59Z: a reader
// who has gone elsewhere while a Board move's dry run is out keeps the focus where they put it,
// and the body counts as the place the press left only when the press's own card went away.
// Measured at 7deb75af in builds Job infra-3135-f-h1-qgpz2 and at 5ae79d88 in
// infra-3135-f-b1-q7ttr, in Chromium, Firefox and WebKit at 1440x900 and 390x664: a child's card
// moved by Shift+L (moved at once) or a parent's by Shift+H (asked, then Unlink them), with the
// rule's dry run held and a Tab meanwhile that lands on the moving card's own link, ends on the
// body once the move lands at 7deb75af, where 5ae79d88 takes it to the moved card. The move
// draws the card in its new column, which takes the link away with the card, and the move sets
// no focus to follow it: it does so only while the focus is the card itself, or the question
// took it from the card itself (features/project/IssueBoard.tsx:644, 1cb2c03e), and a focus on a
// control inside the card is neither, so the effect at :551 has no plan to run.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-27 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): eight projects of three issues on the Board, and eight keyboard moves with held dry runs.
test("a Tab onto the moving card's own link while a Board move's dry run is out leaves the focus on the moved card once the move lands, as the same move with the focus left on the card does, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  // Every dry run of a status change sent while `holding` is held until the row lets it through.
  let holding = false;
  const held: Route[] = [];
  await page.route(/\/api\/v1\/issues\/[^/?]+\/status-change$/, async (route) => {
    const dryRun = (route.request().postData() ?? "").includes('"dry_run":true');
    if (!holding || !dryRun) return route.fallback();
    held.push(route);
  });
  /** What holds the keyboard focus: the moved card, the body, or the focused element's tag and
   *  name. */
  const focusName = (moved: string) =>
    page.evaluate((moved) => {
      const focused = document.activeElement;
      if (!(focused instanceof HTMLElement) || focused === document.body) return "the body";
      if (focused.matches(`[data-board-card="${moved}"]`)) return "the moved card";
      const words = (focused.getAttribute("aria-label") || focused.textContent || "")
        .replace(/\s+/g, " ")
        .trim()
        .slice(0, 40);
      return `${focused.tagName.toLowerCase()} "${words}"`;
    }, moved);
  const got: string[] = [];
  const want: string[] = [];
  let made = 0;
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    for (const move of ["moved at once", "asked, then Unlink them"] as const) {
      for (const press of ["no Tab", "a Tab onto the card's own link"] as const) {
        const where = `Board at ${size.width}x${size.height}, a move ${move}, with ${press} while the dry run is out`;
        // One project per case: a parent and a child under it, both in To do, so the child's
        // move into work is one the rule's dry run checks and moves at once, and the parent's
        // move to the backlog, with its child in To do, is one it asks about.
        made += 1;
        const key = `BT${String.fromCharCode(64 + made)}`;
        await createProject({ key, name: `Board Tab ${made}` });
        const parent = await createIssue({ project: key, title: `Parent ${key}` });
        const child = await createIssue({
          parent: parent.key,
          project: key,
          title: `Child ${key}`,
        });
        const other = await createIssue({ project: key, title: `Other ${key}` });
        for (const issue of [parent, child, other]) await patchIssue(issue.key, { status: "todo" });
        await openIssuesTab(page, key, "Board");
        const moved = move === "moved at once" ? child.key : parent.key;
        const card = page.locator(`[data-board-card="${moved}"]`);
        await card.focus();
        await expect(card).toBeFocused();
        holding = true;
        await page.keyboard.press(move === "moved at once" ? "Shift+L" : "Shift+H");
        await expect
          .poll(() => held.length, { message: `${where}: the move sent no dry run` })
          .toBeGreaterThan(0);
        if (press === "a Tab onto the card's own link") {
          await page.keyboard.press("Tab");
          await expect(
            card.locator("a").first(),
            `${where}: the Tab did not land on the card's own link`
          ).toBeFocused();
        }
        holding = false;
        for (const route of held.splice(0)) await route.continue();
        if (move === "asked, then Unlink them") {
          const question = page.getByRole("dialog", { name: `Move ${moved} to Backlog?` });
          // The choice is disabled until its own dry run answers, and the click waits for that.
          await question.getByRole("button", { name: "Unlink them" }).click();
          await expect(question).toHaveCount(0);
        }
        const column = move === "moved at once" ? "in_progress" : "backlog";
        await expect(
          page.locator(`[data-board-column="${column}"] [data-board-card="${moved}"]`),
          `${where}: the card did not move`
        ).toHaveCount(1);
        // The move's focus effect runs with the render that draws the card in its column, and
        // the question gives the focus back as it closes, so both are done after these frames.
        await nextFrames(page);
        await nextFrames(page);
        // One read is the answer: the move has landed and every hand-off of it has run.
        got.push(`${where}: the focus is on ${await focusName(moved)}`);
        want.push(`${where}: the focus is on the moved card`);
      }
    }
  }
  // The Tab put the focus inside the card the press moved; the move took that link away with the
  // card, so a focus left on the body there is the press's doing, and a keyboard reader would
  // start their next Tab from the top of the page.
  expect(got, got.join("; ")).toEqual(want);
});

// INFRA-3135, round 27 (the tester's), by the architect's rulings of 2026-11-25 05:3xZ (every
// landing of the selection bar's return takes the least scroll, the marked row's checkbox
// wherever it stands) and 13:01Z. Measured at 7deb75af in builds Jobs infra-3135-f-h1-qgpz2 and
// infra-3135-f-hx-dl7bw, and at 5ae79d88 in infra-3135-f-b1-q7ttr and infra-3135-f-bx-hxjsw: on the
// project List, with the row marked last near the end of a 90-row list and the table's box
// scrolled back to its top, so the table's window does not draw that row, Clear selection lands
// the focus on its checkbox at top 737 to bottom 750 in a box whose visible bottom is 736 at
// 1440x900, and at 499 to 512 under a visible bottom of 494 at 390x664, in Chromium and Firefox,
// at both heads: wholly out of sight. In those Jobs' Epic-filtered view WebKit centred it at 1440
// and varied at 390; in this row's own view it failed every run of infra-3135-f-red3-8hflf at
// 7deb75af (746 to 758 under 736, 510 to 522 under 460), six of six. The return goes to the table's own focus of a row it has not drawn
// (features/project/IssuesTable.tsx:324, `model.focusRow`), which scrolls the virtualizer to the
// row with `align: "auto"` (components/table-keys.ts:107, components/DataTable.tsx:304) and focuses
// the checkbox with no scroll once the row is drawn (components/table-keys.ts:134). The
// virtualizer measures its rows from the box's top (DataTable.tsx:286-294, no `scrollMargin`),
// while the box draws the sticky header row above them (:651), so a row aligned to the box's end
// stands one header row below it.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-27 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): ninety seeded issues, and two Clear selection presses read once the box settles.
test("Clear selection with the row marked last below the view and not drawn by the project List's table hands the focus to its checkbox wholly inside the box under the sticky header, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await createProject({ key: "LONG", name: "Long list" });
  for (let index = 0; index < 90; index++) {
    await createIssue({ project: "LONG", title: `Long row ${index}` });
  }
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  /** How the focused element stands against the window, the table's box and the box's sticky
   *  header row: its name, its rect, the part of the box the reader sees under the header, and
   *  whether it lies wholly inside that part. */
  const standing = () =>
    page.evaluate(() => {
      const round = (value: number) => Math.round(value * 10) / 10;
      const focused = document.activeElement;
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      if (!(focused instanceof HTMLElement) || focused === document.body || scroller === null) {
        return { text: "the focus is on the body", whole: false };
      }
      const own = focused.getBoundingClientRect();
      const frame = scroller.getBoundingClientRect();
      const head = scroller.querySelector("thead th")?.getBoundingClientRect().bottom ?? frame.top;
      const top = Math.max(0, frame.top, head);
      const bottom = Math.min(window.innerHeight, frame.bottom);
      const name = focused.getAttribute("aria-label") || focused.tagName.toLowerCase();
      return {
        text:
          `the focus is on "${name}" at top ${round(own.top)} to bottom ${round(own.bottom)}, ` +
          `where the box's visible part under its header runs from ${round(top)} to ${round(bottom)}`,
        whole: own.top >= top - 0.5 && own.bottom <= bottom + 0.5,
      };
    });
  const landings: { text: string; whole: boolean }[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    const where = `project List at ${size.width}x${size.height}`;
    await openIssuesTab(page, "LONG", "List");
    await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(12);
    // The window brings the table's box to just below the app's header where the page is taller
    // than the window, and the box scrolls to its end, where the row marked last is drawn.
    await box.evaluate((node) => {
      window.scrollBy(0, node.getBoundingClientRect().top - 60);
      node.scrollTop = node.scrollHeight;
    });
    await settledScrollTop(page);
    await settledScrollTop(box);
    // The rows read once the box has held still at its end; they only name the row marked.
    const order = await rows.evaluateAll((nodes) =>
      nodes
        .map((node) => ({
          key: node.getAttribute("data-issue-row") ?? "",
          top: node.getBoundingClientRect().top,
        }))
        .sort((one, other) => one.top - other.top)
        .map((row) => row.key)
    );
    const marked = order[order.length - 4] ?? "";
    const mark = page.getByRole("checkbox", { exact: true, name: `Select ${marked}` });
    await mark.focus();
    await page.keyboard.press("Space");
    await expect(mark).toBeChecked();
    await expect(bar).toContainText("1 selected");
    // Back to the box's top: the table's window no longer draws the row marked last.
    await box.evaluate((node) => {
      node.scrollTop = 0;
    });
    await settledScrollTop(box);
    await expect(
      page.locator(`[data-issue-row="${marked}"]`),
      `${where}: the table still draws the row marked last`
    ).toHaveCount(0);
    await bar.getByRole("button", { exact: true, name: "Clear selection" }).focus();
    await page.keyboard.press("Enter");
    await expect(bar).toHaveCount(0);
    await expect(
      mark,
      `${where}: the focus is not on the checkbox of the row marked last`
    ).toBeFocused();
    await settledScrollTop(box);
    await nextFrames(page);
    // One read is the answer: the bar has gone, the focus has landed on the row's checkbox and
    // the box has held still, so nothing moves the focus or the box after this.
    const landing = await standing();
    landings.push({ text: `${where}: ${landing.text}`, whole: landing.whole });
  }
  // A checkbox below the box's visible bottom is a landing the reader cannot see: the focus is
  // on the row they marked, out of their sight, with no scroll that shows it.
  expect(
    landings.map((landing) => landing.whole),
    landings.map((landing) => landing.text).join("; ")
  ).toEqual(landings.map(() => true));
});

// INFRA-3135, round 27 (the tester's), by the architect's rulings of 2026-11-25 05:3xZ (every
// landing of the selection bar's return takes the least scroll, and a landing partly in sight is
// shown whole at its nearest edge) and 13:21Z, which counts this row. Measured at 7deb75af in
// builds Jobs infra-3135-f-h1-qgpz2 and infra-3135-f-hx-dl7bw, and at 5ae79d88 in
// infra-3135-f-b1-q7ttr and infra-3135-f-bx-hxjsw, in Chromium, Firefox and WebKit at 1440x900
// (the window at its top) and 390x664: under the project List's Epic filter, with the mark of the
// row marked last cut by 30% of its height at the table box's bottom edge or under its sticky
// header, Take out of epic lands the focus on the checkbox of the row now at its place and shows
// it whole at that edge (7deb75af; 5ae79d88 left it cut). Cut by 70%, the same row at the place
// is passed over: at the bottom edge the focus goes to the checkbox of the row before it, and
// under the header to the table itself, with no scroll. The hand-off takes a row's mark only
// where the centre of the mark is in the window and what the page draws there is that row
// (components/SelectionBar.tsx:341-343, `markShown`); the sticky header, or the box's clipped
// edge, stands over the centre of a mark cut by 70%, so the row at the place is passed over for
// the nearest row before it in sight (:278) and else the list's own stop (:155).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-27 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): sixty seeded issues, and eight Take out of epic presses read once the box settles.
test("Take out of epic with the row now at the place of the row marked last cut by 70% at the table box's bottom edge or under its sticky header hands the focus to that row's checkbox at its nearest edge, as a cut of 30% does, on the project List at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CUT", name: "Cut" });
  const epic = await createIssue({ project: "CUT", title: "Cut epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 60; index++) {
    await createIssue({ parent: epic.key, project: "CUT", title: `Cut row ${index}` });
  }
  const view = `/projects/CUT/issues?f.epic=${encodeURIComponent(`${epic.key} · Cut epic`)}`;
  const box = page.getByTestId("issues-table-scroll");
  const rows = page.locator("[data-issue-row]");
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const markOf = (key: string) =>
    page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
  /** The issues the table draws, top to bottom, and of them those whose checkbox the reader sees
   *  whole: inside the window and the box, below its sticky header. */
  const drawn = () =>
    page.evaluate(() => {
      const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
      const frame = scroller?.getBoundingClientRect();
      const head = scroller?.querySelector("thead th")?.getBoundingClientRect().bottom ?? 0;
      const listed = [...document.querySelectorAll<HTMLElement>("[data-issue-row]")];
      const topOf = (row: HTMLElement) => row.getBoundingClientRect().top;
      listed.sort((one, other) => topOf(one) - topOf(other));
      const seen = (row: HTMLElement) => {
        const mark = row.querySelector('input[type="checkbox"]');
        if (mark === null || frame === undefined) return false;
        const own = mark.getBoundingClientRect();
        return (
          own.top >= Math.max(0, frame.top, head) &&
          own.bottom <= Math.min(window.innerHeight, frame.bottom)
        );
      };
      return {
        order: listed.map((row) => row.getAttribute("data-issue-row") ?? ""),
        shown: listed.filter(seen).map((row) => row.getAttribute("data-issue-row") ?? ""),
      };
    });
  /** Where the focus is against the edge `edge` names: the checkbox of the row `key` names
   *  shown whole with that edge of it at most 2 px from the same edge of the part of the box a
   *  landing brings a control into (`ok`), and in words what holds the focus, its rect and that
   *  part. By the architect's ruling w12 of 2026-11-27 11:1xZ (open-items.md), a landing that
   *  scrolls a box brings its control below the larger of the sticky head's bottom and the box's
   *  own top edge plus its border and its computed scroll-padding-top (main's 64 px in
   *  DataTable.tsx, #3709), the place the browser's own j/k focus scroll keeps; the window's
   *  scroll padding is never read. */
  const landingOn = (key: string, edge: "bottom" | "header") =>
    page.evaluate(
      ([key, edge]) => {
        const round = (value: number) => Math.round(value * 10) / 10;
        const focused = document.activeElement;
        const scroller = document.querySelector('[data-testid="issues-table-scroll"]');
        if (!(focused instanceof HTMLElement) || focused === document.body || scroller === null) {
          return { ok: false, text: "the focus is on the body" };
        }
        const own = focused.getBoundingClientRect();
        const frame = scroller.getBoundingClientRect();
        const head =
          scroller.querySelector("thead th")?.getBoundingClientRect().bottom ?? frame.top;
        const padded =
          frame.top +
          scroller.clientTop +
          (Number.parseFloat(getComputedStyle(scroller).scrollPaddingTop) || 0);
        const top = Math.max(0, head, padded);
        const bottom = Math.min(window.innerHeight, frame.bottom);
        const name = focused.getAttribute("aria-label") || focused.tagName.toLowerCase();
        const whole = own.top >= top - 0.5 && own.bottom <= bottom + 0.5;
        const gap = edge === "bottom" ? bottom - own.bottom : own.top - top;
        return {
          ok: name === `Select ${key}` && whole && gap <= 2,
          text:
            `the focus is on "${name}" at top ${round(own.top)} to bottom ${round(own.bottom)}, ` +
            `in a visible part from ${round(top)} (the head's bottom ${round(head)}, the box's ` +
            `padding edge ${round(padded)}) to ${round(bottom)}`,
        };
      },
      [key, edge] as const
    );
  const landings: { ok: boolean; text: string }[] = [];
  for (const size of [
    { height: 900, lift: false, width: 1440 },
    { height: 664, lift: true, width: 390 },
  ]) {
    await page.setViewportSize({ height: size.height, width: size.width });
    for (const [edge, part] of [
      ["bottom", 0.3],
      ["bottom", 0.7],
      ["header", 0.3],
      ["header", 0.7],
    ] as const) {
      const where = `project List at ${size.width}x${size.height} with the mark of the row marked last cut by ${part * 100}% ${edge === "bottom" ? "at the box's bottom edge" : "under its sticky header"}`;
      await page.goto(view);
      await expect.poll(() => rows.count()).toBeGreaterThanOrEqual(12);
      // At 390 the window brings the table's box to just below the app's header; at 1440 the box
      // stands whole in the window, which stays at its top. The box scrolls half way down its rows.
      await box.evaluate((node, lift) => {
        if (lift) window.scrollBy(0, node.getBoundingClientRect().top - 60);
        node.scrollTop = (node.scrollHeight - node.clientHeight) / 2;
      }, size.lift);
      await settledScrollTop(page);
      await settledScrollTop(box);
      await expect.poll(async () => (await drawn()).shown.length).toBeGreaterThanOrEqual(3);
      // The rows read once the box has held still and the poll above found them in sight; they
      // only name the two rows marked below, the middle one in sight and the one after it, and
      // the row after those two, which takes their place once they have gone.
      const { order, shown } = await drawn();
      const at = order.indexOf(shown[Math.floor(shown.length / 2)] ?? "");
      const upper = order[at] ?? "";
      const lower = order[at + 1] ?? "";
      const after = order[at + 2] ?? "";
      // The lower row first, so the row marked last is the upper one, whose place the focus takes.
      for (const key of [lower, upper]) {
        await markOf(key).focus();
        await page.keyboard.press("Space");
        await expect(markOf(key)).toBeChecked();
      }
      await expect(bar).toContainText("2 selected");
      await markOf(upper).evaluate(
        (mark, [edge, part]) => {
          const scroller = mark.closest<HTMLElement>('[data-testid="issues-table-scroll"]');
          const own = mark.getBoundingClientRect();
          const head = scroller?.querySelector("thead th")?.getBoundingClientRect().bottom ?? 0;
          const bottom = Math.min(
            window.innerHeight,
            scroller?.getBoundingClientRect().bottom ?? 0
          );
          scroller?.scrollBy(
            0,
            edge === "bottom"
              ? own.bottom - (bottom + own.height * part)
              : own.top - (head - own.height * part)
          );
        },
        [edge, part] as const
      );
      await settledScrollTop(box);
      await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
      await page.keyboard.press("Enter");
      await expect(bar).toHaveCount(0);
      await expect(markOf(upper)).toHaveCount(0);
      // The bar's hand-off runs after the commit that took the bar away, and its scroll lands
      // in the frames after it, so both are done once the box has held still.
      await nextFrames(page);
      await settledScrollTop(box);
      // One read is the answer: the writes have landed, the bar has gone and every hand-off of the
      // press has run, so nothing moves the focus or the box after this.
      const landing = await landingOn(after, edge);
      landings.push({
        ok: landing.ok,
        text: `${where}: ${landing.text}, want "Select ${after}" whole at that edge`,
      });
    }
  }
  // The row now at the place stands partly in the reader's sight; passing it over puts the focus
  // on a row they did not mark the place of, or on the table with no row in hand.
  expect(
    landings.map((landing) => landing.ok),
    landings.map((landing) => landing.text).join("; ")
  ).toEqual(landings.map(() => true));
});

// INFRA-3135, round 28 (the tester's), by the architect's rulings of 2026-11-25 13:01Z ("a count
// is drawn once every read it counts has answered; a read that a count does not count never hides
// it") and 17:5xZ (F5: while a Claimed-by narrowing that needs the agent registry is on, and
// before the registry's first read answers or settles on its error, no column draws a count;
// after that the per-value rule applies). Measured at 5a444912 in builds Job
// infra-3135-f8-h1-sn5q6, in Chromium, Firefox and WebKit at 1440x900: with the agent list
// (`GET /api/v1/agents`) refused every time and Nobody or lapsed picked, the header filters draw
// their counts once the page says it could not check which claims have lapsed, and lose them
// again while each later poll of the list is out: Assignee offers "alice" where it offered
// "alice, 2 issues" a moment before. The list has never answered, so each poll's read puts the
// registry's query back to pending (`isError` false, `known` false), and
// `registryPending = wantsFree && !registry.known && !registry.isError` (IssuesTable.tsx:505)
// then hides every column's count, where the registry's first read settled long before. The rows
// the list holds are the same before and during that read: every claim counts as held while no
// read has answered. The control: before the poll, with the list's first read settled on its
// error, the same filters draw these counts.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-28 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): one project on the List, read with the agent list refused, and one poll of the list held.
test("with the agent list refused, the project List's header filters keep each count the list does not decide while a later poll of the list is out @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const alice = await createIssue({ project: "CORE", title: "Harbour crane" });
  await patchIssue(alice.key, { assignee: "alice", labels: ["alpha"], priority: 1 });
  const bob = await createIssue({ project: "CORE", title: "Signal tower" });
  await patchIssue(bob.key, { assignee: "bob", priority: 2 });
  const more = await createIssue({ project: "CORE", title: "Ferry ramp" });
  await patchIssue(more.key, { assignee: "alice", labels: ["alpha"], priority: 1 });
  // The agent list answers 500 while `holding` is false, and is held, unanswered, while it is true.
  let holding = false;
  const held: Route[] = [];
  await page.route(
    (url) => url.pathname === "/api/v1/agents",
    async (route) => {
      if (holding) {
        held.push(route);
        return;
      }
      await route.fulfill({
        body: '{"error":{"code":"INTERNAL","message":"agent list refused"}}',
        contentType: "application/json",
        status: 500,
      });
    }
  );
  /** Each option of one header filter, by its accessible name, read once it holds `count`, in
   *  sorted order: an option that draws a count is named by its `aria-label` (`alice, 2 issues`),
   *  and one that draws none by its label's own text (`alice`), its check mark left out. */
  const headerFilterOptions = async (header: string, count: number): Promise<string[]> => {
    const trigger = page.getByRole("button", {
      name: new RegExp(`^Filter ${header}(?:: \\d+ picked)?$`, "u"),
    });
    await trigger.click();
    const options = listOf(trigger).getByRole("option");
    await expect(options).toHaveCount(count);
    const names = await options.evaluateAll((elements) =>
      elements.map(
        (element) =>
          element.getAttribute("aria-label") ??
          Array.from(element.children)
            .filter((child) => child.getAttribute("aria-hidden") !== "true")
            .map((child) => child.textContent ?? "")
            .join("")
            .trim()
      )
    );
    await page.keyboard.press("Escape");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    return names.sort();
  };
  // The counts that read the issues alone, which answered.
  const filters = [
    { count: 2, header: "Assignee", wanted: ["alice, 2 issues", "bob, 1 issue"] },
    { count: 2, header: "Priority", wanted: ["P1, 2 issues", "P2, 1 issue"] },
    { count: 1, header: "Labels", wanted: ["alpha, 2 issues"] },
  ];
  /** What each filter offers that is not one of its wanted counts, written for a message. */
  const lacking = async (where: string): Promise<string[]> => {
    const wrong: string[] = [];
    for (const { count, header, wanted } of filters) {
      const offered = await headerFilterOptions(header, count);
      for (const want of wanted)
        if (!offered.includes(want))
          wrong.push(
            `${where}: ${header} offers ${offered.join(" | ")} where it should hold "${want}"`
          );
    }
    return wrong;
  };
  await page.goto("/projects/CORE/issues?unclaimed=1");
  for (const key of [alice.key, bob.key, more.key])
    await expect(page.locator(`[data-issue-row="${key}"]`)).toBeVisible();
  // The control: the list's first read has settled on its error, as the page says, and every
  // count the list does not decide is drawn.
  await expect(page.getByTestId("unclaimed-unchecked")).toBeVisible();
  await expect.poll(() => lacking("first read refused"), { timeout: 10_000 }).toEqual([]);
  // The shell's next poll of the list (every 15 s, `AGENT_REGISTRY_POLL_MS`) is held: its read is
  // out while the filters are read again.
  holding = true;
  await expect
    .poll(() => held.length, { message: "a later poll of the agent list", timeout: 20_000 })
    .toBeGreaterThan(0);
  const whileOut = await lacking("a later poll held");
  holding = false;
  for (const route of held.splice(0))
    await route
      .fulfill({
        body: '{"error":{"code":"INTERNAL","message":"agent list refused"}}',
        contentType: "application/json",
        status: 500,
      })
      .catch(() => undefined);
  expect(whileOut, whileOut.join("; ")).toEqual([]);
});

// INFRA-3135, round 28 (the tester's), by the architect's rulings of 2026-11-25 05:1xZ (the rule
// reads the checkbox, the control that takes the focus, and not its row), 05:3xZ (every landing of
// the selection bar's return takes the least scroll to its nearest edge, and a landing partly in
// sight is shown whole) and 20:3xZ, which counts this row at 390x664, a size the spec names.
// Measured at 5a444912 in builds Jobs infra-3135-f8-h3-wwf5b and
// infra-3135-infra-3135-probe28c-cqhhk and at 4f1ad776 in infra-3135-f8-b3-4prbk, in Chromium and
// WebKit at 390x664 (Firefox lands whole): on the project List of an epic's ninety issues, with
// the table's box standing 60 px below the window's top, the two rows marked last near the end of
// the list and the box scrolled back to its top, so neither is drawn, the box ends at y 494 while
// the selection bar stands, and CORE-4's checkbox stands at 483 to 496, cut by 2 px at that edge.
// Take out of epic lands the focus on that checkbox and scrolls the box 3 px, the least scroll that
// shows it whole in the box as it stood. In the frame the bar goes, the box's bottom edge moves up
// 20 px, to 474, while the rows move by the 3 px scroll alone, and the checkbox, now at 480 to 493, is left
// wholly below the box's edge. The landing measures the box before the bar's going has changed it,
// and does not read the checkbox again once it has.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-28 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): ninety issues of an epic seeded, and one Take out of epic read once the box settles.
test("at 390 Take out of epic with the rows marked last far below the view and not drawn lands the focus on a checkbox wholly in the box's sight, or on the table, by no more than the least scroll @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await page.setViewportSize({ height: 664, width: 390 });
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 90; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const box = page.getByTestId("issues-table-scroll");
  await page.goto(`/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`);
  await expect.poll(() => page.locator("[data-issue-row]").count()).toBeGreaterThanOrEqual(12);
  // The box's top 60 px below the window's top, and the box scrolled to its end, where it draws
  // the rows near the end of the list.
  await box.evaluate((node) => {
    window.scrollBy(0, node.getBoundingClientRect().top - 60);
    node.scrollTop = node.scrollHeight;
  });
  await settledScrollTop(box);
  await settledScrollTop(page);
  await nextFrames(page);
  const order = await page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>("[data-issue-row]")]
      .sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top)
      .map((row) => row.getAttribute("data-issue-row") ?? "")
  );
  const upper = order[order.length - 4] ?? "";
  const lower = order[order.length - 3] ?? "";
  // The lower row first, so the row marked last is the upper one.
  for (const key of [lower, upper]) {
    const mark = page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
    await mark.focus();
    await page.keyboard.press("Space");
    await expect(mark).toBeChecked();
  }
  await expect(bar).toContainText("2 selected");
  await box.evaluate((node) => {
    node.scrollTop = 0;
  });
  await settledScrollTop(box);
  await settledScrollTop(page);
  await nextFrames(page);
  await expect(page.locator(`[data-issue-row="${upper}"]`)).toHaveCount(0);
  /** The part of the window the box shows its rows in, below its sticky head; the box's and the
   *  window's scroll; and the rect of each drawn row's checkbox, by the row's key. */
  const takeOutView = () =>
    page.evaluate(() => {
      const node = document.querySelector<HTMLElement>('[data-testid="issues-table-scroll"]');
      const frame = node?.getBoundingClientRect();
      const head = node?.querySelector("thead")?.getBoundingClientRect();
      const marks: Record<string, { bottom: number; top: number }> = {};
      for (const row of document.querySelectorAll<HTMLElement>("[data-issue-row]")) {
        const own = row.querySelector('input[type="checkbox"]')?.getBoundingClientRect();
        if (own !== undefined)
          marks[row.getAttribute("data-issue-row") ?? ""] = { bottom: own.bottom, top: own.top };
      }
      const focused = document.activeElement;
      return {
        bottom: Math.min(window.innerHeight, frame?.bottom ?? 0),
        boxScroll: node?.scrollTop ?? 0,
        focusedRow:
          focused instanceof HTMLInputElement && focused.type === "checkbox"
            ? (focused.closest("[data-issue-row]")?.getAttribute("data-issue-row") ?? null)
            : null,
        onTable: focused !== null && focused === node?.querySelector("table"),
        focusedName: focused?.getAttribute("aria-label") ?? focused?.tagName ?? "nothing",
        marks,
        top: Math.max(0, head?.bottom ?? frame?.top ?? 0),
        windowScroll: window.scrollY,
      };
    });
  const before = await takeOutView();
  await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
  await page.keyboard.press("Enter");
  await expect(bar).toHaveCount(0, { timeout: 15_000 });
  await settledScrollTop(box);
  await settledScrollTop(page);
  await nextFrames(page);
  // One read is the answer: the bar has gone and the box and the window have held still, so
  // nothing moves the focus or the rows after this.
  const now = await takeOutView();
  const scrolled =
    Math.abs(now.boxScroll - before.boxScroll) + Math.abs(now.windowScroll - before.windowScroll);
  const landed = now.focusedRow === null ? undefined : now.marks[now.focusedRow];
  const was = now.focusedRow === null ? undefined : before.marks[now.focusedRow];
  // The least scroll that shows the landed checkbox whole, from where it stood before the press.
  const least =
    was === undefined ? 0 : Math.max(0, was.bottom - before.bottom, before.top - was.top);
  const where =
    `the focus is on ${now.focusedName}` +
    (landed === undefined
      ? ""
      : ` at top ${Math.round(landed.top)} to bottom ${Math.round(landed.bottom)}, in the box's sight from ${Math.round(now.top)} to ${Math.round(now.bottom)}`) +
    `, and the box and the window scrolled ${Math.round(scrolled)} px where the least is ${Math.round(least)} px`;
  const whole =
    landed !== undefined && landed.top >= now.top - 1 && landed.bottom <= now.bottom + 1;
  expect(now.focusedRow !== null || now.onTable, where).toBe(true);
  expect(now.onTable || (whole && scrolled <= least + 1), where).toBe(true);
});

// INFRA-3135, round 29 (the tester's), by the agent-registry commit's own words (07c9100e,
// features/refs/agent-registry.ts:202-204: "a view that says what it could not check names the
// same reason throughout, and never Envoy as unreachable while it is the agent list refilling"),
// F5 of the architect's ruling of 2026-11-26 04:4xZ (the failure's reason is kept per query client)
// and 13:01Z (a notice that comes and goes per poll is a defect). Measured at b04db619 in builds
// Job infra-3135-rg-h-4c9dg, in Chromium, Firefox and WebKit at 1440x900 with the shell's own 15 s
// poll: with the agent list answering 503 ENVOY_REFILLING from the start and Nobody or lapsed
// picked, the List says the agent list is refilling, and about five minutes in, for the length of
// one poll's read, says "Envoy listener unreachable." instead (Chromium at 311.6 s to 314.7 s,
// Firefox 313.4 s to 315.7 s, WebKit 313.9 s to 316.6 s). The reason is kept as the data of the
// query `["agents", "failure"]` (agent-registry.ts:180, written at :207-209), which no view
// observes, so the query client removes it at its garbage-collection time, five minutes after it
// was made, and `setQueryData` does not move that time; while the next poll is out the list's
// query has cleared its own error, so `refilling` (:210-212) reads nothing and the page names the
// listener as unreachable. The row drives the same five minutes on the page's clock, and reads the
// notice while a poll is held, in the first minute and five minutes on. The control: in the first
// minute the notice names the agent list refilling.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-29 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): one project on the List, its clock moved five minutes, and two polls held.
test("with the agent list refilling from the start, the project List still says the list is refilling while a poll is out five minutes on, as it does in the first minute @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const alice = await createIssue({ project: "CORE", title: "Harbour crane" });
  await patchIssue(alice.key, { assignee: "alice" });
  const bob = await createIssue({ project: "CORE", title: "Signal tower" });
  await patchIssue(bob.key, { assignee: "bob" });
  // The page's clock is the row's: the shell's 15 s poll and the query client's five-minute
  // collection both run on it, so the row moves it rather than waiting the minutes out.
  await page.clock.install();
  // The agent list answers that it is refilling while `holding` is false, and is held, unanswered,
  // while it is true.
  let holding = false;
  const held: Route[] = [];
  const refilling = {
    body: JSON.stringify({ code: "ENVOY_REFILLING", error: "The agent list is refilling." }),
    contentType: "application/json",
    headers: { "Retry-After": "1" },
    status: 503,
  };
  await page.route(
    (url) => url.pathname === "/api/v1/agents",
    async (route) => {
      if (holding) {
        held.push(route);
        return;
      }
      await route.fulfill(refilling);
    }
  );
  const notice = page.getByTestId("unclaimed-unchecked");
  const assignee = page.getByRole("button", { name: /^Filter Assignee(?:: \d+ picked)?$/u });
  /** Moves the page's clock by `ms` with the next poll of the agent list held, has the reader open
   *  and close the Assignee filter while that poll is out, and answers what the notice then says. */
  const noticeWhilePollHeld = async (ms: number, where: string): Promise<string> => {
    holding = true;
    await page.clock.fastForward(ms);
    // TanStack's notifyManager batches its delivery on a zero-delay page timer.
    await page.clock.runFor(1);
    await expect
      .poll(() => held.length, { message: `${where}: a poll of the agent list` })
      .toBeGreaterThan(0);
    await assignee.click();
    await expect(listOf(assignee).getByRole("option")).toHaveCount(2);
    await page.keyboard.press("Escape");
    await expect(assignee).toHaveAttribute("aria-expanded", "false");
    await page.clock.runFor(1);
    // Read once the reader's open and close have rendered, with the poll still held, so the
    // notice is what the page says while that poll is out.
    const said = (await notice.count()) === 0 ? "no notice" : ((await notice.textContent()) ?? "");
    holding = false;
    for (const route of held.splice(0)) await route.fulfill(refilling).catch(() => undefined);
    return said;
  };
  await page.goto("/projects/CORE/issues?unclaimed=1");
  for (const key of [alice.key, bob.key])
    await expect(page.locator(`[data-issue-row="${key}"]`)).toBeVisible();
  await expect(notice).toContainText(AGENT_LIST_REFILLING);
  // The control: in the first minute, with a poll out, the page still says the list is refilling.
  const first = await noticeWhilePollHeld(15_000, "in the first minute");
  expect(first, `in the first minute, with a poll out, the notice reads: ${first}`).toContain(
    AGENT_LIST_REFILLING
  );
  await expect(notice).toContainText(AGENT_LIST_REFILLING);
  // Five minutes on, past the query client's collection of an entry no view observes.
  const later = await noticeWhilePollHeld(300_000, "five minutes on");
  expect(later, `five minutes on, with a poll out, the notice reads: ${later}`).toContain(
    AGENT_LIST_REFILLING
  );
});

// INFRA-3135, round 29 (the tester's), by the architect's rulings of 2026-11-26 07:5xZ (a host's
// layout for the commit is current before its bar hands the focus on: nothing the reader sees
// changes but the frame in which the box takes its size) and 14:12Z, which counts this row. Measured
// at b04db619 in builds Jobs infra-3135-l-h5-2prxr and infra-3135-l-h1b-ptjnb, in Chromium, Firefox
// and WebKit at 1440x900: on the project List of an epic's ninety issues, with the two rows marked
// last near the top of the list and the box scrolled to its end, so neither is drawn, Take out of
// epic lands the focus on a checkbox while the box stands at 306 to 706 (its style height 400 px),
// and one frame later the box stands at 306 to 736 (430 px): its bottom edge moves 30 px after the
// landing. `fitHeight` (components/table-layout.ts:424-434, through `boxHeightNow` at :393-420,
// called from IssuesTable.tsx:371) writes the height while main's foot still holds 100 px of room
// for the bar, so the floor it writes is not the height the box keeps, and the foot's pass then has
// `measure` write 430. At 53f87411 nothing writes the box's style and it stands at 306 to 736
// throughout (infra-3135-l-b5-v65cm, infra-3135-l-b1b-9zdsr).
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-29 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): ninety issues of an epic seeded, and one Take out of epic read for a second after it lands.
test("after Take out of epic with the rows marked last far above the view lands, the project List's box keeps the frame it had when the focus landed @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 90; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const box = page.getByTestId("issues-table-scroll");
  await page.goto(`/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`);
  await expect.poll(() => page.locator("[data-issue-row]").count()).toBeGreaterThanOrEqual(12);
  // The box's top 60 px below the window's top, scrolled to its own top, where it draws the first rows.
  await box.evaluate((node) => {
    window.scrollBy(0, node.getBoundingClientRect().top - 60);
    node.scrollTop = 0;
  });
  await settledScrollTop(box);
  await settledScrollTop(page);
  await nextFrames(page);
  const order = await page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>("[data-issue-row]")]
      .sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top)
      .map((row) => row.getAttribute("data-issue-row") ?? "")
  );
  const upper = order[2] ?? "";
  const lower = order[3] ?? "";
  // The lower row first, so the row marked last is the upper one.
  for (const key of [lower, upper]) {
    const mark = page.getByRole("checkbox", { exact: true, name: `Select ${key}` });
    await mark.focus();
    await page.keyboard.press("Space");
    await expect(mark).toBeChecked();
  }
  await expect(bar).toContainText("2 selected");
  await box.evaluate((node) => {
    node.scrollTop = node.scrollHeight;
  });
  await settledScrollTop(box);
  await nextFrames(page);
  await expect(page.locator(`[data-issue-row="${upper}"]`)).toHaveCount(0);
  // Every frame from the press on, the page records the box's frame and the focused checkbox's top
  // at the first frame the focus stands on a checkbox of the table with the bar gone, and the box's
  // frame and that checkbox's top at every frame after.
  await box.evaluate((node) => {
    const w = window as unknown as {
      __afterLanding?: {
        first: number[] | null;
        last: number[] | null;
        landed: Element | null;
        on: boolean;
      };
    };
    const record = {
      first: null as number[] | null,
      last: null as number[] | null,
      landed: null as Element | null,
      on: true,
    };
    w.__afterLanding = record;
    const step = () => {
      if (!record.on) return;
      const barGone =
        document.querySelector('[role="toolbar"][aria-label="Selected issues"]') === null;
      const active = document.activeElement;
      if (
        record.landed === null &&
        barGone &&
        active instanceof HTMLInputElement &&
        active.type === "checkbox" &&
        node.contains(active)
      ) {
        record.landed = active;
      }
      if (record.landed !== null) {
        const frame = node.getBoundingClientRect();
        const now = [frame.top, frame.bottom, record.landed.getBoundingClientRect().top];
        if (record.first === null) record.first = now;
        record.last = now;
      }
      requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  });
  await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
  await page.keyboard.press("Enter");
  await expect(bar).toHaveCount(0, { timeout: 15_000 });
  // A second after the bar has gone, past the foot's pass and the box's next measure, so the page's
  // record holds every frame the box could still move in after the landing.
  await page.waitForTimeout(1000);
  // One read is the answer: the bar has gone a second ago and nothing the press started is out, so
  // the record's first and last frames after the landing are final.
  const seen = await page.evaluate(() => {
    const w = window as unknown as {
      __afterLanding?: { first: number[] | null; last: number[] | null; on: boolean };
    };
    const record = w.__afterLanding;
    if (record !== undefined) record.on = false;
    return { first: record?.first ?? null, last: record?.last ?? null };
  });
  const [topAt, bottomAt, markAt] = seen.first ?? [Number.NaN, Number.NaN, Number.NaN];
  const [topNow, bottomNow, markNow] = seen.last ?? [Number.NaN, Number.NaN, Number.NaN];
  const text =
    seen.first === null
      ? "the focus never stood on a checkbox of the table once the bar had gone"
      : `at the landing the box stood at ${Math.round(topAt)} to ${Math.round(bottomAt)}, a second later at ` +
        `${Math.round(topNow)} to ${Math.round(bottomNow)}, and the landed checkbox moved ${Math.round(markNow - markAt)} px`;
  expect(
    seen.first === null ||
      (Math.abs(topNow - topAt) <= 1 &&
        Math.abs(bottomNow - bottomAt) <= 1 &&
        Math.abs(markNow - markAt) <= 1),
    text
  ).toBe(true);
});

// INFRA-3135, round 29 (the tester's), by the architect's ruling of 2026-11-26 14:12Z on the
// selection bar's return (dispatch://INFRA-3135/artifact/bar-return-design-md version 7, whose
// contract names a row as leaving from the reader's press until it is no longer listed, never
// shorter). Measured at b04db619 in builds Jobs infra-3135-l-h2-zhxxw, infra-3135-l-h4-br4nn and
// infra-3135-l-h5-2prxr, in Chromium, Firefox and WebKit at 1440x900 and 390x664, 34 of 34 cases: on
// the project List of an epic's issues, with two rows marked, Take out of epic's writes held and
// both marks then unticked, so the bar goes by the reader's own untick, the focus stands on the
// checkbox of the row marked last; once the writes are let through the two rows leave the list and
// the focus falls to the page body. The List names a row as leaving only once its write has answered
// (`moved`, features/project/issues-table-actions.ts:242, read as `leaving` at :1106), and this round
// hands that to the bar's hand-off (IssuesTable.tsx:354, :473), against IssuesTable.tsx:343-345's
// own words that "a landing never stands on a row the press is taking away however long the write
// and its read take". The same at 53f87411, 18 of 18 (infra-3135-l-b2-nzlnk).
// Round 31 (the tester's), by the architect's request of 2026-11-28 01:04Z: below sm the List's
// selection bar stands where main's #5224 toolbar stands, fixed under the header and wrapped in
// main's groups (y 56 to 266 at 390x664, the implementer's probe Job l-p13312-6pg6g), over the
// rows this row unticks, so a pointer press at a mark lands on the bar. Each mark is unticked as it
// is marked, by the keyboard: the focus on the mark, then Space, which leaves the focus on the
// mark in every engine, as the measurement above reads it.
// Its engines: the clause of the `webkit` and `firefox` projects that names the rows
// INFRA-3135's round-29 tester pinned (e2e/playwright.config.ts).
// SLOW(>=2s): forty issues of an epic seeded, and two Take out of epic presses with their writes held.
test("with Take out of epic's writes held and both marks then unticked, the project List leaves the focus on a control of the list, never the page body, once the rows leave, at 1440 and 390 @firefox @webkit", async ({
  page,
}) => {
  test.setTimeout(180_000);
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  for (let index = 0; index < 40; index++) {
    await createIssue({ parent: epic.key, project: "CORE", title: `Epic row ${index}` });
  }
  // Each issue write is held while `holding` is true, and let through after.
  let holding = false;
  const held: Route[] = [];
  await page.route(
    (url) => /^\/api\/v1\/issues\/[^/]+$/u.test(url.pathname),
    async (route) => {
      if (route.request().method() !== "PATCH" || !holding) return route.fallback();
      held.push(route);
    }
  );
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  const box = page.getByTestId("issues-table-scroll");
  const wrong: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 664, width: 390 },
  ]) {
    const where = `project List at ${size.width}x${size.height}`;
    await page.setViewportSize(size);
    await page.goto(
      `/projects/CORE/issues?f.epic=${encodeURIComponent(`${epic.key} · Table epic`)}`
    );
    await expect.poll(() => page.locator("[data-issue-row]").count()).toBeGreaterThanOrEqual(8);
    await box.evaluate((node) => window.scrollBy(0, node.getBoundingClientRect().top - 60));
    await settledScrollTop(page);
    await nextFrames(page);
    const order = await page.evaluate(() =>
      [...document.querySelectorAll<HTMLElement>("[data-issue-row]")]
        .sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top)
        .map((row) => row.getAttribute("data-issue-row") ?? "")
    );
    const upper = order[2] ?? "";
    const lower = order[3] ?? "";
    const marks = [lower, upper].map((key) =>
      page.getByRole("checkbox", { exact: true, name: `Select ${key}` })
    );
    // The lower row first, so the row marked last is the upper one.
    for (const mark of marks) {
      await mark.focus();
      await page.keyboard.press("Space");
      await expect(mark).toBeChecked();
    }
    await expect(bar).toContainText("2 selected");
    holding = true;
    await bar.getByRole("button", { exact: true, name: "Take out of epic" }).focus();
    await page.keyboard.press("Enter");
    await expect
      .poll(() => held.length, { message: `${where}: the writes were never sent` })
      .toBeGreaterThan(0);
    for (const mark of marks) {
      await mark.focus();
      await page.keyboard.press("Space");
      await nextFrames(page);
    }
    await expect(bar).toHaveCount(0);
    holding = false;
    for (const route of held.splice(0)) await route.continue().catch(() => undefined);
    await expect(page.locator(`[data-issue-row="${upper}"]`)).toHaveCount(0, { timeout: 15_000 });
    await expect(page.locator(`[data-issue-row="${lower}"]`)).toHaveCount(0, { timeout: 15_000 });
    await settledScrollTop(box);
    await nextFrames(page);
    // One read is the answer: both rows have left the list and the box holds still, so nothing moves
    // the focus after this.
    const focus = await box.evaluate((node) => {
      const active = document.activeElement;
      return {
        body: active === null || active === document.body,
        inList: active !== null && node.contains(active),
        name:
          active === null
            ? "nothing"
            : `${active.tagName.toLowerCase()} "${(active.getAttribute("aria-label") ?? active.textContent ?? "").trim().slice(0, 60)}"`,
      };
    });
    if (focus.body || !focus.inList) {
      wrong.push(
        `${where}: once the rows left the focus is on ${focus.body ? "the page body" : focus.name}`
      );
    }
  }
  expect(wrong, wrong.join("; ")).toEqual([]);
});

// Pressing empty space inside a `tabindex="-1"` scroll box puts the focus on that box without a
// focus ring (only `:focus-visible` may draw one), and Tab from there moves to the next control
// with its focus ring drawn as usual.
// Its engines: `:focus-visible` and pointer-focus outline rules differ across Chromium, Firefox
// and WebKit (e2e/playwright.config.ts).
// SLOW(>=2s): two issues seeded, and the project List and the project Board opened at 1440.
test("a pointer press on empty space in the List's selection bar line or the Board's scroller focuses the box with no focus ring, and Tab draws the next control's ring @firefox @webkit", async ({
  browserName,
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "First issue" });
  await createIssue({ project: "CORE", title: "Second issue" });
  await patchIssue(first.key, { status: "todo" });

  /** A visible point inside `box`'s client area (above any scrollbar) where `elementFromPoint`
   *  hits `box` itself rather than a child control. */
  const findSpot = (box: Locator) =>
    box.evaluate((node) => {
      const rect = node.getBoundingClientRect();
      const left = Math.max(rect.left + 2, 1);
      const right = Math.min(rect.left + node.clientWidth - 2, window.innerWidth - 1);
      const top = Math.max(rect.top + 2, 1);
      const bottom = Math.min(rect.top + node.clientHeight - 2, window.innerHeight - 1);
      if (right <= left || bottom <= top) return null;
      const midY = (top + bottom) / 2;
      const candidates: [number, number][] = [
        [left + 4, bottom - 3],
        [(left + right) / 2, bottom - 3],
        [right - 4, bottom - 3],
      ];
      for (const child of node.children) {
        const childRect = child.getBoundingClientRect();
        candidates.push([childRect.right + 3, midY]);
        candidates.push([childRect.left - 3, midY]);
      }
      for (const [x, y] of candidates) {
        if (x < left || x > right || y < top || y > bottom) continue;
        if (document.elementFromPoint(x, y) === node) {
          return { x: Math.round(x), y: Math.round(y) };
        }
      }
      return null;
    });

  const emptySpotIn = async (what: string, box: Locator) => {
    await expect.poll(() => findSpot(box), { message: `no empty spot in ${what}` }).not.toBeNull();
    const settled = await settledValue(`${what} empty spot`, () => findSpot(box));
    if (settled === null) throw new Error(`no settled empty spot in ${what}`);
    return settled;
  };

  /** Reads `document.activeElement`, `:focus-visible`, and the computed `outline-style` and
   *  `box-shadow` relative to `box`. */
  const focusRingOn = (box: Locator) =>
    box.evaluate((node) => {
      const active = document.activeElement;
      const style = active instanceof HTMLElement ? getComputedStyle(active) : null;
      return {
        boxShadow: style?.boxShadow ?? "none",
        focusVisible: active?.matches(":focus-visible") ?? false,
        isBox: active === node,
        outlineStyle: style?.outlineStyle ?? "none",
        tag: active?.tagName ?? "",
      };
    });

  /** Presses empty space in `box` (`what`) and then Tab. The press puts the focus on `box` with
   *  no focus ring, and the Tab moves it on with its ring drawn: onto a control inside `holder`
   *  where `tabStaysIn` says so, else out of `holder`. */
  const pressEmptySpotThenTab = async (
    what: string,
    box: Locator,
    holder: Locator,
    tabStaysIn: boolean
  ) => {
    const spot = await emptySpotIn(what, box);
    await page.mouse.click(spot.x, spot.y);
    await expect
      .poll(() => focusRingOn(box))
      .toEqual({
        boxShadow: "none",
        focusVisible: false,
        isBox: true,
        outlineStyle: "none",
        tag: "DIV",
      });
    await page.keyboard.press("Tab");
    await expect
      .poll(async () => {
        const state = await focusRingOn(box);
        const inHolder = await holder.evaluate((node) => {
          const active = document.activeElement;
          return active !== null && active !== node && node.contains(active);
        });
        return {
          drewRing: state.outlineStyle !== "none" || state.boxShadow !== "none",
          focusVisible: state.focusVisible,
          inHolder,
          isBox: state.isBox,
        };
      })
      .toEqual({
        drewRing: true,
        focusVisible: true,
        inHolder: tabStaysIn,
        isBox: false,
      });
  };

  // Project List: selection bar's line (`tabindex="-1"`), whose Tab goes on to a control of the
  // bar.
  await page.goto("/projects/CORE/issues");
  const mark = page.getByRole("checkbox", { exact: true, name: `Select ${first.key}` });
  await expect(mark).toBeVisible();
  await mark.click();
  const bar = page.getByRole("toolbar", { name: "Selected issues" });
  await expect(bar).toBeVisible();
  const barLine = bar.locator('> div[tabindex="-1"]').first();
  await expect(barLine).toBeVisible();
  await pressEmptySpotThenTab("selection bar line", barLine, bar, true);

  // Project Board: flat board scroller (`tabindex="-1"`).
  await openIssuesTab(page, "CORE", "Board");
  const scroller = page.getByTestId("board-scroll-container");
  await expect(scroller).toBeVisible();
  await expect(scroller).toHaveAttribute("tabindex", "-1");
  await expect(page.locator(`[data-board-card="${first.key}"]`)).toBeVisible();
  // Tab from the focused scroller goes into the board's cards in Chromium and WebKit. Firefox
  // sends it to the page's first control (Skip to content), as it does on main, where the same
  // press leaves the focus on the main content and no tabindex is on the scroller (a throwaway
  // probe, builds Job infra-3135-i-b7p-2mvcn).
  await pressEmptySpotThenTab("board scroller", scroller, scroller, browserName !== "firefox");
});
