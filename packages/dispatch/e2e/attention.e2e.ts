import {
  createComment,
  createIssue,
  createMessage,
  createProject,
  getIssueEvents,
  putIssueState,
} from "./api";
import {
  historyScrollTop,
  historyViewport,
  scrollHistoryBy,
  scrollHistoryTo,
} from "./history-scroll";
import { historyEntries, historyPanel, tree } from "./issue-page";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

const session = {
  actor: { id: "e2e-attention", kind: "session" as const },
  as: "agent" as const,
};

/**
 * The issue's history, in Details (dispatch://INFRA-1196 R15): where the comments and messages
 * an agent wrote outside every thread a person wrote in are read, newest first, each thread one
 * entry carrying its own id in `data-history-entry`. The panel's list of the conversation these
 * rows were written against is gone (R14), and with it its follow of the newest card
 * (`conversation/use-follow-latest.ts`); the history holds the reader's place instead, in the
 * box Details scrolls in (`IssueHistory.tsx`'s `ViewportAnchor`), and these rows read that.
 */
const HISTORY = '[data-testid="issue-history"]';

/**
 * The row the reader is on, read the way `ViewportAnchor` reads it: the row nearest the visible
 * centre of the list, probed through `elementFromPoint`. `root` is the list's own labelled box,
 * `item` the attribute each of its rows carries, and `clip` the box the list is clipped to
 * (`history-scroll.ts`).
 */
async function readingAnchor(
  page: Page,
  root: string,
  item: string,
  clip: { bottom: number; top: number }
): Promise<string> {
  const value = await page.evaluate(
    ({ clipBox, itemAttribute, rootSelector }) => {
      const list = document.querySelector<HTMLElement>(rootSelector);
      if (list === null) {
        return null;
      }
      const rect = list.getBoundingClientRect();
      const x = rect.left + rect.width / 2;
      const y = (Math.max(rect.top, clipBox.top) + Math.min(rect.bottom, clipBox.bottom)) / 2;
      for (const offset of [0, 8, 16, 24, 32, 40, -8, -16, -24, -32, -40]) {
        const row = document
          .elementFromPoint(x, y + offset)
          ?.closest<HTMLElement>(`[${itemAttribute}]`);
        const found = row?.getAttribute(itemAttribute);
        if (found !== null && found !== undefined && found !== "") {
          return found;
        }
      }
      return null;
    },
    { clipBox: clip, itemAttribute: item, rootSelector: root }
  );
  expect(value).not.toBeNull();
  return value ?? "";
}

/** Opens `/activity` on `issueKey`, which opens Details at the history, and waits until the
 *  history holds `newest`. */
async function openHistory(page: Page, issueKey: string, newest: string): Promise<void> {
  await page.goto(`/issues/${issueKey}/activity`);
  await expect(historyPanel(page).getByText(newest)).toBeVisible();
}

// @pr: A delivery arriving: a new entry must not move the history under a reader who scrolled down it.
test("a reader scrolled down the history keeps their place when a new entry arrives @pr", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openHistory(page, issue.key, "Existing message 29");
    // The reader is put on an entry in the middle of the history rather than at a fixed
    // offset, because the panel's properties take the top of its box and a fixed offset need
    // not reach an entry at all - and a reader on no entry is not what this row is about.
    const clip = await historyViewport(page);
    await scrollHistoryBy(
      page,
      await page.evaluate((clipBox) => {
        const entries = [
          ...document.querySelectorAll('[data-testid="issue-history"] [data-history-entry]'),
        ];
        const middle = entries[Math.floor(entries.length / 2)]?.getBoundingClientRect();
        if (middle === undefined) return 0;
        return middle.top + middle.height / 2 - (clipBox.top + clipBox.bottom) / 2;
      }, clip)
    );

    const entry = await readingAnchor(page, HISTORY, "data-history-entry", clip);
    const anchor = historyPanel(page).locator(`[data-history-entry="${entry}"]`);
    const before = (await anchor.boundingBox())?.y;

    // The history is newest first, so the arrival lands above the reader.
    await createMessage(issue.key, { body: "Arrived while reading" }, session);

    await expect(historyPanel(page).getByText("Arrived while reading")).toBeAttached();
    await expect.poll(async () => (await anchor.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
});

// The panel's list followed a reader at its top down to the newest card (`use-follow-latest.ts`,
// removed with the list). The history is drawn newest first, so a reader at the top of Details
// has the newest entry at the history's head with nothing to follow: the arrival is the
// history's first entry, and the panel has not moved.
test("a reader at the top of Details finds a new entry at the history's head", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openHistory(page, issue.key, "Existing message 29");
    await scrollHistoryTo(page, 0);

    await createMessage(issue.key, { body: "Arrived at the top" }, session);

    await expect(historyEntries(page).first()).toContainText("Arrived at the top");
    expect(await historyScrollTop(page), "an arrival moved Details under a reader at its top").toBe(
      0
    );
  } finally {
    await context.close();
  }
});

// The twin of the row above, at every other address of the page. The list's follow pulled the
// panel down to the conversation under a reader reading a preset; the history follows nothing,
// so an entry arriving while the reader is on a filter leaves Details where they left it, and
// coming back leaves it there too.
test("a new entry arriving while the reader is on a filter leaves Details where they left it", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Away from the history" });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openHistory(page, issue.key, "Existing message 29");
    // Up to the panel's own top, off the history and onto the issue's properties.
    await scrollHistoryTo(page, 0);

    // Away to a filter the page draws in its own column; Details, and the history in it, stay
    // drawn beside it - that is what makes the arrival below reach a history the reader is not
    // reading.
    const spec = page
      .getByTestId("issue-presets")
      .getByRole("button", { exact: true, name: "Spec" });
    await spec.click();
    await expect(spec).toHaveAttribute("aria-pressed", "true");
    expect(await historyScrollTop(page)).toBe(0);

    await createMessage(issue.key, { body: "Arrived while away" }, session);

    await expect(historyPanel(page).getByText("Arrived while away")).toBeAttached();
    expect(
      await historyScrollTop(page),
      "an entry arriving while the reader was on a filter moved Details"
    ).toBe(0);

    // And they come back to the place they left, with the new entry in the history.
    await page.goBack();
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/activity$`, "u"));
    await expect(historyPanel(page).getByText("Arrived while away")).toBeAttached();
    expect(
      await historyScrollTop(page),
      "coming back to the history did not leave the reader where they left it"
    ).toBe(0);
  } finally {
    await context.close();
  }
});

// Paging is the history's: the outline reads each of its three lists whole and offers no
// `Load older` at all. Older events land beneath the entries already on screen, so the entry
// the reader is on does not move.
test("loading older history entries keeps the reader anchor", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Older events" });
  // One more than the history's 200-event page (`IssueHistory.tsx`), so a second page exists.
  for (let index = 0; index < 205; index += 1) {
    await createMessage(issue.key, { body: `Older message ${index}` }, session);
  }

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(tree(page)).toBeVisible();
    await expect(page.getByRole("button", { name: "Load older" })).toHaveCount(0);

    await page.goto(`/issues/${issue.key}/activity`);
    const loadOlder = historyPanel(page).getByRole("button", { name: "Load older" });
    await expect(loadOlder).toBeVisible();
    await loadOlder.scrollIntoViewIfNeeded();
    await page.evaluate(() => {
      const { promise, resolve } = Promise.withResolvers<void>();
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      return promise;
    });
    const clip = await historyViewport(page);
    const loadOlderBox = await loadOlder.boundingBox();
    expect(loadOlderBox).not.toBeNull();
    expect(loadOlderBox?.y ?? -1).toBeGreaterThanOrEqual(clip.top);
    expect((loadOlderBox?.y ?? 0) + (loadOlderBox?.height ?? 0)).toBeLessThanOrEqual(clip.bottom);

    const entry = await readingAnchor(page, HISTORY, "data-history-entry", clip);
    const anchor = historyPanel(page).locator(`[data-history-entry="${entry}"]`);
    const before = (await anchor.boundingBox())?.y;
    await loadOlder.click();

    // The proof the older page landed is the oldest entry of all: the issue's own creation,
    // which only the page beneath the first 200 holds.
    await expect(historyEntries(page).filter({ hasText: "created the issue" })).toHaveCount(1);
    await expect.poll(async () => (await anchor.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
});

// The read mark draws one line in the history, "New since you last read", where the mark falls
// among its entries (`history-model.ts`): a mark that moves adds that line above the entries the
// reader has not read, so the reader's own entry has to stay where it was. The reader is parked
// over a gap between two entries, where `ViewportAnchor`'s probe has to fall back to the entry
// below.
async function verifyGapDetection(
  browser: Parameters<typeof asUser>[0],
  viewport?: { height: number; width: number }
): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Attention test" });
  for (let index = 0; index < 20; index += 1) {
    await createComment(issue.key, { body: `Gap comment ${index}` }, session);
  }
  // Far beyond the issue's head, so nothing is unread and the history draws no line yet.
  await putIssueState(issue.key, { last_read_seq: 10_000 }, { login: "alice" });
  // The mark moves back to the fourth comment below: the sixteen after it are unread.
  const created = (await getIssueEvents(issue.key, { limit: 200 })).filter(
    (event) => event.type === "comment.created"
  );
  const fourth = created[3]?.seq;
  if (fourth === undefined) throw new Error("the seed wrote fewer than four comments");

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    if (viewport !== undefined) {
      await page.setViewportSize(viewport);
    }
    await openHistory(page, issue.key, "Gap comment 19");
    const divider = historyPanel(page).getByRole("listitem", { name: "New since you last read" });
    await expect(divider).toHaveCount(0);
    await expect(historyPanel(page).getByText(/^Gap comment \d+$/)).toHaveCount(20);

    // Where the gap nearest the middle of the history sits, as an offset from the centre of the
    // box it is clipped to: the reader is moved by that offset, so the gap lands on the very
    // point `ViewportAnchor` probes (`rowAtCenter`).
    const clip = await historyViewport(page);
    // The count above drew all twenty comments, so the gaps measured here are the whole list's.
    const gap = await page.evaluate((clipBox) => {
      const entries = [
        ...document.querySelectorAll<HTMLElement>(
          '[data-testid="issue-history"] [data-history-entry]'
        ),
      ];
      const gaps = [];
      for (let index = 0; index < entries.length - 1; index += 1) {
        const upper = entries[index]?.getBoundingClientRect();
        const lower = entries[index + 1]?.getBoundingClientRect();
        if (upper !== undefined && lower !== undefined && lower.top > upper.bottom) {
          gaps.push({
            fromCenter: (upper.bottom + lower.top) / 2 - (clipBox.top + clipBox.bottom) / 2,
            index,
            lowerEntry: entries[index + 1]?.getAttribute("data-history-entry"),
          });
        }
      }
      if (gaps.length === 0) {
        return null;
      }
      const middle = entries.length / 2;
      gaps.sort((left, right) => Math.abs(left.index - middle) - Math.abs(right.index - middle));
      return gaps[0];
    }, clip);
    expect(gap).not.toBeNull();
    if (gap === null || gap.lowerEntry === undefined || gap.lowerEntry === null) {
      throw new Error("no gap found between adjacent history entries");
    }

    await scrollHistoryBy(page, gap.fromCenter);
    // The history holds the comments waited for above, and the scroll above ran in the page
    // before this read, so the box read here is the history's as it now stands.
    const centre = await page.evaluate(
      ({ clipBox, root }) => {
        const history = document.querySelector(root);
        if (history === null) {
          return null;
        }
        const rect = history.getBoundingClientRect();
        const x = rect.left + rect.width / 2;
        const visibleTop = Math.max(rect.top, clipBox.top);
        const visibleBottom = Math.min(rect.bottom, clipBox.bottom);
        return { x, y: (visibleTop + visibleBottom) / 2 };
      },
      { clipBox: await historyViewport(page), root: HISTORY }
    );
    expect(centre).not.toBeNull();
    if (centre === null) {
      throw new Error("could not locate the history");
    }
    const centreHitsEntry = await page.evaluate((point) => {
      const element = document.elementFromPoint(point.x, point.y);
      return element !== null && element.closest("[data-history-entry]") !== null;
    }, centre);
    expect(centreHitsEntry).toBe(false);

    const lowerEntry = historyPanel(page).locator(`[data-history-entry="${gap.lowerEntry}"]`);
    const before = (await lowerEntry.boundingBox())?.y;
    // The reader's mark moves back, so the history gains its line above every unread entry.
    // Only a refocus re-reads it, as a tab left open overnight would.
    await putIssueState(issue.key, { last_read_seq: fourth }, { login: "alice" });
    await page.evaluate(() => {
      const now = Date.now();
      Date.now = () => now + 31_000;
      window.dispatchEvent(new Event("visibilitychange"));
      window.dispatchEvent(new Event("focus"));
    });

    await expect(divider).toHaveCount(1);
    await expect.poll(async () => (await lowerEntry.boundingBox())?.y).toBe(before);
  } finally {
    await context.close();
  }
}

test("a read mark moving under a reader parked in a history gap keeps the lower entry fixed", async ({
  browser,
}) => {
  await verifyGapDetection(browser);
});

test("a read mark moving in a narrow history keeps the lower entry fixed", async ({ browser }) => {
  await verifyGapDetection(browser, { height: 1024, width: 768 });
});
