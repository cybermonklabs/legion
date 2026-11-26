import { MIN_REFETCH_INTERVAL_MS } from "../web/src/api/query-refresh";

import { getUnsubscribeCalls, setInterests, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  followAsk,
  getIssueEvents,
  listAskFollowers,
} from "./api";
import { askCard, backToList, readingView, selectRow } from "./inbox-pane";
import { expect, type Locator, type Page, type Route, test } from "./test";
import { asUser } from "./users";

// Two sessions write to one ask over the API with the agent bearer token: the asker and a
// replier. Both follow the ask (its answer and replies reach them directly) until a human
// removes one from the card.
const asker = { kind: "session" as const, id: "e2e-follow-asker" };
const replier = { kind: "session" as const, id: "e2e-follow-replier" };

// @pr: A write the server settles: the ask card lists every session that wrote to the ask, and an unfollow takes one off.
test("the ask card lists every session that wrote to the ask and a human can unfollow one @pr", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Who hears the answer" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  await createComment(
    issue.key,
    { ask_id: ask.id, body: "Tests are green on my side." },
    { actor: replier, as: "agent" }
  );
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: replier.id, title: "Replier (e2e)" },
  ]);
  expect(((await listAskFollowers(ask.id)).followers ?? []).map((row) => row.session_id)).toEqual([
    asker.id,
    replier.id,
  ]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    // The re-follow at the end of the row arrives over the event feed, whose refetch the page
    // holds to one per second (`MIN_REFETCH_INTERVAL_MS`): a page timer its own clock fires.
    await page.clock.install();
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const followers = card.getByRole("region", { name: "Recipients" });
    await expect(followers).toContainText("Reaches 2");
    // The routing list is folded at rest; the count names it, and the chips are inside.
    await followers.getByRole("button", { name: /^Reaches/ }).click();
    await expect(followers.getByText("Asker (e2e)", { exact: true })).toBeVisible();
    await expect(followers.getByText("Replier (e2e)", { exact: true })).toBeVisible();
    await expect(followers.getByRole("img", { exact: true, name: "Running" })).toHaveCount(2);
    await expect(followers.locator(`[title='${asker.id}']`)).toHaveCount(1);
    await expect(followers.locator(`[title='${replier.id}']`)).toHaveCount(1);

    // The human removes the replier; the card, the API, and the event log agree.
    await followers
      .getByRole("listitem")
      .filter({ has: page.locator(`[title='${replier.id}']`) })
      .getByRole("button", { name: "Unfollow" })
      .click();
    const dialog = page.getByRole("dialog", { name: "Unfollow" });
    await expect(dialog).toContainText(
      "Unfollow Replier (e2e) from this ask? Its answer and replies will no longer reach them. They will be told."
    );
    await dialog.getByRole("button", { name: "Confirm" }).click();

    await expect(followers).toContainText("Reaches 1");
    await expect(followers.locator(`[title='${replier.id}']`)).toHaveCount(0);
    await expect(followers.locator(`[title='${asker.id}']`)).toHaveCount(1);
    await expect
      .poll(
        async () => ((await listAskFollowers(ask.id)).followers ?? []).map((row) => row.session_id),
        // The server answered the unfollow before the card dropped the chip above, so this is
        // true within a round trip; the default ladder would not look again until 350 ms.
        { intervals: [50, 50, 100, 200, 500] }
      )
      .toEqual([asker.id]);
    const removed = (await getIssueEvents(issue.key)).find(
      (event) => event.type === "ask.follower_removed"
    );
    expect(removed).toMatchObject({
      actor: { id: "alice", kind: "user" },
      payload: { ask_id: ask.id, session_id: replier.id },
    });

    // The replier rejoins over the API; the open card picks it up from the live event feed.
    await followAsk(ask.id, replier.id, replier);
    await page.clock.fastForward(MIN_REFETCH_INTERVAL_MS);
    await expect(followers).toContainText("Reaches 2");
    await expect(followers.locator(`[title='${replier.id}']`)).toHaveCount(1);
  } finally {
    await alice.close();
  }
});

/** What a row holds of the reads the page makes of one route: `hold` keeps every later GET the
 *  route names unanswered, `out` counts how many it kept, and `answer` sends on to Dispatch each
 *  kept GET the page still waits on and lets every later one through. A kept GET is answered by
 *  nothing else, so the page gives it up at its own 30 s deadline and asks again. */
interface BoundReads {
  readonly answer: () => Promise<void>;
  readonly hold: () => void;
  readonly out: () => number;
}

/** Routes the page's GETs that `matches` names, which go through until `hold` is called. */
async function routeBoundReads(page: Page, matches: (url: URL) => boolean): Promise<BoundReads> {
  let holding = false;
  let kept = 0;
  const waiting: Route[] = [];
  await page.route(matches, async (route: Route) => {
    if (!holding || route.request().method() !== "GET") return route.fallback();
    kept += 1;
    waiting.push(route);
    return undefined;
  });
  return {
    // A GET the page gave up has no answer left to take, and its route refuses the fallback.
    answer: async () => {
      holding = false;
      for (const route of waiting.splice(0)) await route.fallback().catch(() => undefined);
    },
    hold: () => {
      holding = true;
    },
    out: () => kept,
  };
}

// A session that never wrote to the ask still hears every event on it when its Envoy
// interests cover the owning issue's topic family. The card lists it beside the followers,
// and removing it goes through the issue-subscriber route: the one that stops delivery.
test("the ask card lists the issue's subscribers and removing one cuts the issue subscription", async ({
  browser,
}) => {
  const watcher = "e2e-issue-watcher";
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Who else hears the answer" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: asker, as: "agent" }
  );
  await setLiveSessions([
    { session_id: asker.id, title: "Asker (e2e)" },
    { session_id: watcher, title: "Watcher (e2e)" },
  ]);
  await setInterests([
    { session_id: watcher, topics: [`notifications.dispatch.issue.${issue.key}.>`] },
  ]);
  expect(((await listAskFollowers(ask.id)).followers ?? []).map((row) => row.session_id)).toEqual([
    asker.id,
  ]);

  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  try {
    await page.goto("/");
    const card = page.getByTestId(`ask-${ask.id}`);
    const recipients = card.getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 2");
    await recipients.getByRole("button", { name: /^Reaches/ }).click();
    await expect(
      recipients.getByRole("list", { name: "Followers" }).locator(`[title='${asker.id}']`)
    ).toHaveCount(1);
    const viaIssue = recipients.getByRole("list", { name: "Via issue subscription" });
    await expect(recipients).toContainText("via issue subscription");
    await expect(viaIssue.getByText("Watcher (e2e)", { exact: true })).toBeVisible();
    await expect(viaIssue.locator(`[title='${watcher}']`)).toHaveCount(1);
    await expect(viaIssue.getByRole("button", { name: "Unfollow" })).toHaveCount(0);

    const unsubscribed = page.waitForRequest(
      (request) =>
        request.method() === "DELETE" &&
        request.url().endsWith(`/api/v1/issues/${issue.key}/subscribers/${watcher}`)
    );
    await viaIssue.getByRole("button", { name: "Unsubscribe" }).click();
    const dialog = page.getByRole("dialog", { name: "Unsubscribe" });
    await expect(dialog).toContainText(
      `Unsubscribe Watcher (e2e) from ${issue.key}? They will be told.`
    );
    await dialog.getByRole("button", { name: "Confirm" }).click();
    await unsubscribed;

    await expect(recipients).toContainText("Reaches 1");
    await expect(recipients.locator(`[title='${watcher}']`)).toHaveCount(0);
    await expect(recipients.locator(`[title='${asker.id}']`)).toHaveCount(1);
    await expect
      .poll(async () => (await getUnsubscribeCalls()).some((call) => call.session_id === watcher))
      .toBe(true);
    // The follower route was never touched: the asker still follows.
    expect(((await listAskFollowers(ask.id)).followers ?? []).map((row) => row.session_id)).toEqual(
      [asker.id]
    );
  } finally {
    await alice.close();
  }
});

/** The sessions of the rows below: the asker and the two repliers that follow its ask. */
const heldAsker = { kind: "session" as const, id: "e2e-held-asker" };
const heldFirst = { kind: "session" as const, id: "e2e-held-first" };
const heldSecond = { kind: "session" as const, id: "e2e-held-second" };

/** An open ask of alice's issue, written by `heldAsker`, that `heldFirst` and `heldSecond` wrote
 *  to, so its card reaches three sessions. Answers the ask's id. */
async function seedHeldFollowers(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Who hears the answer" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: heldAsker, as: "agent" }
  );
  for (const replier of [heldFirst, heldSecond]) {
    await createComment(
      issue.key,
      { ask_id: ask.id, body: `Noted by ${replier.id}.` },
      { actor: replier, as: "agent" }
    );
  }
  await setLiveSessions([
    { session_id: heldAsker.id, title: "Asker (held)" },
    { session_id: heldFirst.id, title: "First (held)" },
    { session_id: heldSecond.id, title: "Second (held)" },
  ]);
  return ask.id;
}

/** The ask's card open in the Inbox, its routing list unfolded, and `heldFirst`'s Unfollow
 *  pressed and confirmed with the read of the ask held: the press has landed, and the read the
 *  page asks after it is out. Answers the card's Recipients region. */
async function unfollowWithReadHeld(
  page: Page,
  ask: string,
  reads: BoundReads,
  step: (text: string) => string
): Promise<Locator> {
  const recipients = (await askCard(page, ask)).getByRole("region", { name: "Recipients" });
  await expect(recipients, step("the card reaches three sessions.")).toContainText("Reaches 3");
  const toggle = recipients.getByRole("button", { name: /^Reaches/ });
  await expect(toggle, step("the routing list is folded at rest.")).toHaveAttribute(
    "aria-expanded",
    "false"
  );
  await toggle.click();
  await expect(toggle, step("the routing list is unfolded.")).toHaveAttribute(
    "aria-expanded",
    "true"
  );
  reads.hold();
  const unfollowed = page.waitForResponse(
    (response) =>
      response.request().method() === "DELETE" &&
      new URL(response.url()).pathname === `/api/v1/asks/${ask}/followers/${heldFirst.id}`
  );
  await recipients
    .getByRole("listitem")
    .filter({ has: page.locator(`[title='${heldFirst.id}']`) })
    .getByRole("button", { name: "Unfollow" })
    .click();
  await page
    .getByRole("dialog", { name: "Unfollow" })
    .getByRole("button", { name: "Confirm" })
    .click();
  expect((await unfollowed).ok(), step("the unfollow lands at Dispatch.")).toBe(true);
  await expect
    .poll(reads.out, { message: step("the read of the ask the unfollow starts is out.") })
    .toBeGreaterThan(0);
  return recipients;
}

/** What the card says once the read of the ask after an unfollow has outlived its 30 s bound. */
const RECIPIENTS_UNREAD =
  "Could not load the recipients. Dispatch did not answer within 30 seconds.";

// The tester's red row of round 22 (B2 (1): a hold's reason dropped when its caller is drawn
// again while its read is still out; the architect's ruling of 2026-11-26T23:11Z). Below 1280 px
// an ask's card is drawn in the full-width reading view only while its row is open
// (Inbox.tsx:2386), and the card's Unfollow holds through the read of the ask with a hold of its
// own (AskRecipients.tsx:310), so the view closed and opened again mounts a new card whose hold
// knows nothing of the unfollow. At e4ece294 the card says "Could not load the recipients.
// Dispatch did not answer within 30 seconds." at the bound, and says nothing once the view is
// open again, though the read is still out and no later write began. Measured in Chromium and
// Firefox, twice each.
// SLOW(>=2s): one ask seeded with three recipients, one Inbox page open at 1024 px, one unfollow
// written, the page's clock moved once past the 30 s bound, and the reading view closed and opened.
// @firefox: the timers of a read's own deadline and of the bound a held control waits on, which
// each engine runs.
test("an Unfollow in the reading view whose read the bound ended still says why once the view is opened again @firefox", async ({
  page,
}) => {
  const step = (text: string) => `Unfollow in the reading view, closed and opened: ${text}`;
  const ask = await seedHeldFollowers();
  const askReads = await routeBoundReads(page, (url) => url.pathname === `/api/v1/asks/${ask}`);
  await page.setViewportSize({ height: 900, width: 1024 });
  await page.clock.install();
  await page.goto("/");
  const recipients = await unfollowWithReadHeld(page, ask, askReads, step);
  await expect(
    readingView(page),
    step("the ask is open in the full-width reading view.")
  ).toHaveAttribute("data-inbox-reading-row", ask);
  const unreadLine = recipients
    .getByRole("alert")
    .filter({ hasText: /did not answer within 30 seconds/ });
  await page.clock.fastForward(30_000);
  await expect(
    unreadLine,
    step("at the 30 s bound the card says why the recipients are not read.")
  ).toHaveText([RECIPIENTS_UNREAD]);

  await backToList(page);
  await selectRow(page, ask);
  await expect(
    readingView(page),
    step("the same ask is open again in the reading view.")
  ).toHaveAttribute("data-inbox-reading-row", ask);
  await expect(recipients, step("the card drawn again counts its recipients.")).toContainText(
    "Reaches"
  );
  await expect(
    unreadLine,
    step(
      "the view opened again, with the read of the ask still out and no later write, the card still says why the recipients are not read."
    )
  ).toHaveText([RECIPIENTS_UNREAD]);

  await askReads.answer();
  await page.clock.runFor(1_000);
  await expect(unreadLine, step("the read of the ask answered takes the line away.")).toHaveCount(
    0
  );
});

// The tester's red row of round 22 (B2 (1), across 1280 px; the architect's ruling of
// 2026-11-26T23:11Z). From 1280 px the ask's card is drawn in the reading column beside the list
// (Inbox.tsx:2377), and below it in the reading view (Inbox.tsx:2386): a window narrowed across
// 1280 px mounts a new card whose hold (AskRecipients.tsx:310) knows nothing of the unfollow. At
// e4ece294 the card says why at the bound at 1440 px, and says nothing in the reading view at
// 1024 px, though the read is still out and no later write began. Measured in Chromium and
// Firefox, twice each.
// SLOW(>=2s): one ask seeded with three recipients, one Inbox page open at 1440 px, one unfollow
// written, the page's clock moved once past the 30 s bound, and the window narrowed to 1024 px.
// @firefox: the timers of a read's own deadline and of the bound a held control waits on, which
// each engine runs.
test("an Unfollow whose read the bound ended still says why once the window narrows below 1280 px @firefox", async ({
  page,
}) => {
  const step = (text: string) => `Unfollow in the reading column, then below 1280 px: ${text}`;
  const ask = await seedHeldFollowers();
  const askReads = await routeBoundReads(page, (url) => url.pathname === `/api/v1/asks/${ask}`);
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.clock.install();
  await page.goto("/");
  const recipients = await unfollowWithReadHeld(page, ask, askReads, step);
  const unreadLine = recipients
    .getByRole("alert")
    .filter({ hasText: /did not answer within 30 seconds/ });
  await page.clock.fastForward(30_000);
  await expect(
    unreadLine,
    step("at the 30 s bound the card in the reading column says why the recipients are not read.")
  ).toHaveText([RECIPIENTS_UNREAD]);

  await page.setViewportSize({ height: 900, width: 1024 });
  await askCard(page, ask);
  await expect(
    readingView(page),
    step("below 1280 px the ask is open in the full-width reading view.")
  ).toHaveAttribute("data-inbox-reading-row", ask);
  await expect(
    unreadLine,
    step(
      "below 1280 px, with the read of the ask still out and no later write, the card still says why the recipients are not read."
    )
  ).toHaveText([RECIPIENTS_UNREAD]);

  await askReads.answer();
  await page.clock.runFor(1_000);
  await expect(unreadLine, step("the read of the ask answered takes the line away.")).toHaveCount(
    0
  );
});
