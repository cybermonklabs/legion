import { CONTROL } from "../web/src/components/control-names";
import { setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  listComments,
  listIssueMessages,
  markThreadRead,
} from "./api";
import { control, controlRow, displayChoice } from "./filters";
import { settledScrollTop } from "./frames";
import { drawer } from "./inbox-drawer";
import { inboxPane, paneScroller, selectRow } from "./inbox-pane";
import { asSession, holdReadMarks, replyRowId, THREAD_SESSION } from "./inbox-threads";
import { expect, type Locator, type Page, test } from "./test";
import { verticalOrder } from "./threads-order";
import { asUser } from "./users";

/**
 * The Thread order switch reorders the open thread at once (R5, check c-thread-order-live). A
 * pick in the Inbox's display menu reaches every reader of the order on the page: the thread on
 * the screen reverses its replies and moves its reply box to the other end, with no page load
 * and no scroll, and a fold the reader opened stays open. This holds for a comment thread, an
 * issue message thread and an open ask's thread, in the reading pane and in the row's card below
 * the pane's width. An open ask's box is its answer box, which sits where a reply box would. The
 * pick leaves the display menu open.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

type Order = "newest" | "oldest";

/** Set on the window before the first switch: a load of the page would drop it. */
const MARKER = "threadOrderMarker";

/** The line each reply of `kind` is found by, oldest first. */
function replyLines(kind: string, count: number): string[] {
  return Array.from(
    { length: count },
    (_, index) => `${kind} reply ${index + 1} on the replay budget.`
  );
}

/** A reply long enough that the open comment thread runs past the pane's foot: its line, then
 *  two paragraphs more. */
function reply(line: string): string {
  return [
    line,
    "The mirror replays the ledger from the last checkpoint, so the budget counts the hours since that checkpoint was written.",
    "A cold start reads the whole window again before it takes a write.",
  ].join("\n\n");
}

const COMMENT_ROOT = "How far back does the replay budget reach?";
const MESSAGE_ROOT = "Can the lease handover wait for the next window?";
const ASK_QUESTION = "Ship the mirror cutover?";

/** One thread as the page draws it: where it is drawn, its root's text where the thread draws
 *  its root, the line of each reply, oldest first, the box the order places at one end, and how
 *  many replies its fold holds. */
interface Drawn {
  readonly box: Locator;
  readonly folded: number;
  readonly name: string;
  readonly replies: readonly string[];
  readonly root?: string;
  readonly scope: Locator;
}

interface Seeded {
  readonly askId: string;
  readonly commentId: string;
  readonly messageId: string;
}

/**
 * Three rows of alice's: the Reply rows of a comment thread on an issue with six replies and of
 * an issue message thread with three, and the Ask row of an open question the session asks her,
 * whose thread holds three of the session's replies. The two Reply rows were read through some
 * of their replies, so each folds its read ones: the comment thread its first four, the message
 * thread its first. The Inbox hands an Ask row's card no read mark, so it folds by count alone:
 * every reply but the newest two, its first.
 */
async function seedThreads(): Promise<Seeded> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });

  const commentIssue = await createIssue({ project: "CORE", title: "Replay budget" });
  const comment = await createComment(commentIssue.key, { body: COMMENT_ROOT });
  for (const line of replyLines("Comment", 6)) {
    await createComment(commentIssue.key, { body: reply(line), reply_to: comment.id }, asSession());
  }

  const messageIssue = await createIssue({ project: "CORE", title: "Lease handover" });
  const message = await createMessage(messageIssue.key, { body: MESSAGE_ROOT });
  for (const line of replyLines("Message", 3)) {
    await createMessage(
      messageIssue.key,
      { body: reply(line), in_reply_to: message.id },
      asSession()
    );
  }

  const askIssue = await createIssue({ project: "CORE", title: "Mirror cutover" });
  const ask = await createAsk(
    askIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: ASK_QUESTION },
    asSession()
  );
  // Left open: an ask waiting on alice is her Ask row, and its card draws the answer form.
  for (const line of replyLines("Ask", 3)) {
    await createComment(askIssue.key, { ask_id: ask.id, body: reply(line) }, asSession());
  }

  // The replies of each thread, counted before the test acts, read in the order they were
  // written.
  const byTime = <Write extends { created_at: string }>(writes: readonly Write[]) =>
    [...writes].sort((left, right) => (left.created_at < right.created_at ? -1 : 1));
  const commentReplies = byTime(
    (await listComments(commentIssue.key)).filter((write) => write.reply_to === comment.id)
  );
  const messageReplies = byTime(
    (await listIssueMessages(messageIssue.key)).find((read) => read.message.id === message.id)
      ?.replies ?? []
  );
  const askReplies = byTime(
    (await listComments(askIssue.key)).filter((write) => write.ask_id === ask.id)
  );
  expect(commentReplies).toHaveLength(6);
  expect(messageReplies).toHaveLength(3);
  expect(askReplies).toHaveLength(3);

  await markThreadRead(comment.id, commentReplies[3]?.created_at ?? "");
  await markThreadRead(message.id, messageReplies[0]?.created_at ?? "");
  return { askId: ask.id, commentId: comment.id, messageId: message.id };
}

/** The parts of a thread whose places the order decides: its root, each reply and its box. */
function parts(thread: Drawn): Record<string, Locator> {
  const named: Record<string, Locator> = {};
  if (thread.root !== undefined) named.root = thread.scope.getByText(thread.root).first();
  thread.replies.forEach((line, index) => {
    named[`reply-${index + 1}`] = thread.scope.getByText(line).first();
  });
  named.box = thread.box;
  return named;
}

/** Where `order` puts those parts, down the screen, with the fold open: the root on top, then
 *  the replies oldest first and the box at the foot, or the box under the root and the replies
 *  newest first. */
function placesIn(thread: Drawn, order: Order): string[] {
  const root = thread.root === undefined ? [] : ["root"];
  const replies = thread.replies.map((_, index) => `reply-${index + 1}`);
  return order === "oldest" ? [...root, ...replies, "box"] : [...root, "box", ...replies.reverse()];
}

async function expectOrder(thread: Drawn, order: Order): Promise<void> {
  await expect
    .poll(() => verticalOrder(parts(thread)), { message: `${thread.name}, ${order}` })
    .toEqual(placesIn(thread, order));
}

/** The fold over the earlier replies, opened. */
async function expectFoldOpen(thread: Drawn): Promise<void> {
  await expect(
    thread.scope.getByRole("button", { name: "Show fewer replies" }),
    `${thread.name}'s fold`
  ).toHaveAttribute("aria-expanded", "true");
}

/** Opens the thread's fold, so every reply is on the page, and reads it in the first order. */
async function openFold(thread: Drawn): Promise<void> {
  const count = thread.folded;
  await thread.scope
    .getByRole("button", { name: `Show ${count} earlier ${count === 1 ? "reply" : "replies"}` })
    .click();
  await expectFoldOpen(thread);
  await expectOrder(thread, "oldest");
}

/** Where the page stands: the window's scroll, and each of `boxes`' own scroll. */
async function scrollPositions(
  page: Page,
  boxes: Readonly<Record<string, Locator>>
): Promise<Record<string, number>> {
  const positions: Record<string, number> = { page: await settledScrollTop(page) };
  for (const [name, box] of Object.entries(boxes)) positions[name] = await settledScrollTop(box);
  return positions;
}

/**
 * The View trigger of the bar inside `bar`: the bar's own button where the bar draws it in its
 * line, else its item of **More**, where a bar under 640 px of its width holds it - the drawer's
 * (R8, `components/ControlRow.tsx`). **More** is opened with a script click, for the reason
 * `switchBothWays` gives, and handed back so it is shut the same way.
 */
async function viewTrigger(
  page: Page,
  bar: Locator | Page
): Promise<{ more: Locator | null; trigger: Locator }> {
  const row = controlRow(bar);
  const button = control(row, CONTROL.displayMenu).and(
    page.getByRole("button", { exact: true, name: "View" })
  );
  const more = control(row, CONTROL.moreMenu);
  await expect(button.or(more).first()).toBeVisible();
  // The wait above saw the button or More, and the bar lays itself out from its own width
  // before it first paints, so whether the button is drawn is settled once either shows.
  if ((await button.count()) > 0) return { more: null, trigger: button };
  await more.evaluate((node) => (node as HTMLElement).click());
  const item = page
    .getByRole("menu", { name: "More" })
    .getByRole("menuitem", { exact: true, name: "View" });
  await expect(item).toHaveCount(1);
  return { more, trigger: item };
}

/**
 * Picks Newest first, then Oldest first again, in the display menu of the bar in `bar`, with each
 * of `threads` open in Oldest first with its fold open. After each pick every thread reads in
 * the order picked, its fold is still open, nothing scrolled - neither the window nor any of
 * `boxes`, the boxes the threads scroll in - the page did not load again, and the menu is still
 * open with the order picked.
 *
 * The scroll is recorded before the menu opens, so the reads after a pick cover the whole
 * switch, the menu's opening too. The test opens and shuts the menu with a script click on
 * View, and on **More** where View is among its items. Playwright's own click scrolls a control
 * into view before it presses it. At 1024 px with the list scrolled, View is above the screen,
 * so that scroll would be the test's and not the page's. A script click scrolls nothing.
 */
async function switchBothWays(
  page: Page,
  bar: Locator | Page,
  threads: readonly Drawn[],
  boxes: Readonly<Record<string, Locator>>
): Promise<void> {
  const before = await scrollPositions(page, boxes);
  const menu = page.getByRole("dialog", { name: "View" });
  const { more, trigger } = await viewTrigger(page, bar);
  await trigger.evaluate((node) => (node as HTMLElement).click());
  await expect(menu).toHaveCount(1);
  for (const order of ["newest", "oldest"] as const) {
    const choice = displayChoice(menu, "Thread order", order);
    // A script click too. At 1024 px the menu is cut to the room under View and scrolls in
    // itself, and Thread order is its last section, out of sight. Playwright's click would
    // scroll the menu to it first, which is a scroll the test makes.
    await choice.evaluate((node) => (node as HTMLElement).click());
    for (const thread of threads) {
      await expectOrder(thread, order);
      await expectFoldOpen(thread);
    }
    expect(await scrollPositions(page, boxes), `the scroll after ${order}`).toEqual(before);
    // A load of the page would have dropped the mark the test set on the window.
    expect(
      await page.evaluate((name) => Reflect.get(window, name), MARKER),
      `the page after ${order}`
    ).toBe(true);
    await expect(menu, `the display menu after ${order}`).toBeVisible();
    await expect(choice).toBeChecked();
  }
  // Shut again, so the panel covers nothing the test drives next: View, then **More**, which
  // stays open under a menu opened from it.
  await trigger.evaluate((node) => (node as HTMLElement).click());
  await expect(menu).toHaveCount(0);
  if (more !== null) {
    await more.evaluate((node) => (node as HTMLElement).click());
    await expect(page.getByRole("menu", { name: "More" })).toHaveCount(0);
  }
}

// SLOW(>=2s): twelve long replies seeded over three threads, then the order switched both ways
// four times, three in the pane and once over three cards in the drawer, each read part by part.
test("the thread order switch reorders the open thread with no reload", async ({ browser }) => {
  // Three threads seeded, then three switches in the pane and one over three cards.
  test.slow();
  const seeded = await seedThreads();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The read marks are held, so each thread keeps the fold its row put it in while it is open.
    await holdReadMarks(page);
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto("/");
    await page.evaluate((name) => {
      Reflect.set(window, name, true);
    }, MARKER);

    // In the reading pane, one thread after another. A Reply row's thread is the pane's thread
    // body, with a reply box of its own; the Ask row's is the question's card, whose answer box
    // sits where a reply box would.
    const pane = inboxPane(page);
    const paneBody = pane.locator("[data-inbox-thread-body]");
    const paneCard = pane.getByTestId(`ask-${seeded.askId}`);
    const paneThreads: readonly [string, Drawn][] = [
      [
        replyRowId(seeded.commentId),
        {
          box: paneBody.getByRole("textbox", { name: "Reply" }),
          folded: 4,
          name: "the comment thread in the pane",
          replies: replyLines("Comment", 6),
          root: COMMENT_ROOT,
          scope: paneBody,
        },
      ],
      [
        replyRowId(seeded.messageId),
        {
          box: paneBody.getByRole("textbox", { name: "Reply" }),
          folded: 1,
          name: "the message thread in the pane",
          replies: replyLines("Message", 3),
          root: MESSAGE_ROOT,
          scope: paneBody,
        },
      ],
      [
        seeded.askId,
        {
          box: paneCard.getByRole("textbox", { name: "Your answer" }),
          folded: 1,
          name: "the open ask's thread in the pane",
          replies: replyLines("Ask", 3),
          scope: paneCard,
        },
      ],
    ];
    for (const [row, thread] of paneThreads) {
      await selectRow(page, row);
      await openFold(thread);
      await switchBothWays(page, page, [thread], { pane: paneScroller(page) });
    }

    // Below the pane's width the Inbox page lists short rows and opens one row's view in place
    // of the list and its bar (main's #5223), so the thread drawn in its row's card is the
    // drawer's, which keeps the card at every width. The drawer is offered on every page but the
    // Inbox, so the test leaves the Inbox by the navigation's own link: a route change, which
    // loads no page and keeps the mark on the window.
    await page
      .getByRole("navigation", { name: "Navigation" })
      .getByRole("link", { exact: true, name: "Agents" })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
    await page.setViewportSize({ height: 900, width: 1024 });
    await page.locator("body").focus();
    await page.keyboard.press("i");
    const peek = drawer(page);
    await expect(peek).toBeVisible();
    const row = (id: string) => peek.locator(`[data-inbox-row="${id}"]`);
    const commentCard = row(replyRowId(seeded.commentId)).locator("[data-inbox-thread-body]");
    const messageCard = row(replyRowId(seeded.messageId)).locator("[data-inbox-thread-body]");
    const askCard = row(seeded.askId).getByTestId(`ask-${seeded.askId}`);
    const cards: readonly Drawn[] = [
      {
        box: commentCard.getByRole("textbox", { name: "Reply" }),
        folded: 4,
        name: "the comment thread's card",
        replies: replyLines("Comment", 6),
        root: COMMENT_ROOT,
        scope: commentCard,
      },
      {
        box: messageCard.getByRole("textbox", { name: "Reply" }),
        folded: 1,
        name: "the message thread's card",
        replies: replyLines("Message", 3),
        root: MESSAGE_ROOT,
        scope: messageCard,
      },
      {
        box: askCard.getByRole("textbox", { name: "Your answer" }),
        folded: 1,
        name: "the open ask's card",
        replies: replyLines("Ask", 3),
        scope: askCard,
      },
    ];
    for (const thread of cards) await openFold(thread);
    await switchBothWays(page, peek, cards, { drawer: peek });
  } finally {
    await alice.close();
  }
});
