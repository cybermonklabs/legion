import type { MeInboxEntry, MeInboxResponse } from "../web/src/api/types";
import { setLiveSessions } from "./agents";
import {
  answerAsk,
  createAgentMessage,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  declineAsk,
  getIssueDecisions,
  getIssueEvents,
  getMeInbox,
  markThreadRead,
  patchIssue,
  replyToMessageDelivery,
} from "./api";
import { openFilterPanel } from "./filters";
import { inboxRow, READING_PANE_WIDTH } from "./inbox-pane";
import { agentRowId, asSession, replyRowId, sessionActor, THREAD_SESSION } from "./inbox-threads";
import { drawnIds } from "./inbox-view";
import { frontLiveView } from "./live-views";
import { inboxRead } from "./pick-writes";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";
import { expandedState } from "./settled";
import { expect, type Locator, type Page, type Request, test } from "./test";
import { asUser } from "./users";

/**
 * The browser rows of the one read, GET /api/v1/me/inbox.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** Somebody else, who writes in the reader's threads and holds no row of their own. */
const other = "bob";
/** A person a reference can name. A reference names a person by their login as Dispatch stores
 *  one, an email lowercased (`dispatch://person/<login>`), and no bare login, so `alice` and
 *  `bob`, the harness's own logins, are nobody a mention reaches. Dev sign-in records any login
 *  it is given, so this one signs in as they do. */
const named = "person-1@example.com";

/** Holds that the one read ties `login` to the comment `commentId` by a mention: the comment's
 *  mention of them landed as a mentions tuple into their person. Every row that asserts something
 *  of a mention reads this first, so a text grammar that leaves the comment naming nobody fails
 *  here, with that cause named, rather than as a row the page happens not to list. */
async function expectMentioned(login: string, commentId: string): Promise<void> {
  await expect
    .poll(
      async () =>
        tiesOf(await getMeInbox({ kind: "mentions" }, { login }), "comment", commentId) ?? [],
      {
        message: `the one read ties ${login} to comment ${commentId} by its mention of them (a reference names a person by an email-shaped login alone)`,
      }
    )
    .toContain("mentions inbound");
}

/** The row ids the page draws for the entries of one read of its own: an ask's own id, and a
 *  thread's `reply:` or `agent:` id, as the list names its rows (`rowId`). */
function pageRowIds(entries: readonly MeInboxEntry[] | null): string[] {
  return (entries ?? []).flatMap((entry) => {
    if (entry.ask !== undefined) return [entry.ask.id];
    if (entry.agent !== undefined) return [agentRowId(entry.agent.thread_id)];
    return entry.thread === undefined ? [] : [replyRowId(entry.thread.thread_id)];
  });
}

/** The Inbox's list column, found by its name: the region the page names `Inbox list`. */
function inboxListRegion(page: Page) {
  return page.getByRole("region", { name: "Inbox list" });
}

/** The relations that tie the reader to one node in an answer of the one read, each with its
 *  kind and direction; undefined where the answer holds no entry for that node. */
function relationsOf(
  read: MeInboxResponse,
  kind: string,
  id: string
): MeInboxEntry["relations"] | undefined {
  return (read.entries ?? []).find((entry) => entry.node.kind === kind && entry.node.id === id)
    ?.relations;
}

/** What ties the reader to one node in an answer of the one read, each relation as
 *  `<kind> <direction>`; undefined where the answer holds no entry for that node. */
function tiesOf(read: MeInboxResponse, kind: string, id: string): string[] | undefined {
  return relationsOf(read, kind, id)?.map((relation) => `${relation.kind} ${relation.direction}`);
}

/** The lists the page's rows are items of, each as the ids of its rows top to bottom. A row is an
 *  item of the nearest list around it (`ul`, `ol` or `role="list"`), so rows drawn in two lists,
 *  wherever on the page the second one sits, are two entries here. */
async function rowLists(page: Page): Promise<string[][]> {
  return page.locator("[data-inbox-row]").evaluateAll((rows) => {
    const lists = new Map<Element | null, string[]>();
    for (const row of rows) {
      const list = row.parentElement?.closest('ul, ol, [role="list"]') ?? null;
      lists.set(list, [...(lists.get(list) ?? []), row.getAttribute("data-inbox-row") ?? ""]);
    }
    return [...lists.values()];
  });
}

/** The Conversation turn an item link lands on, marked as the one it names (`useItemLanding`):
 *  `turn` is the turn's own key, `<kind>:<id>`. */
function landedTurn(page: Page, turn: string): Locator {
  return page
    .getByRole("list", { name: "Conversation turns" })
    .locator(`li[data-turn="${turn}"][aria-current="true"]`);
}

/** Narrows the page's own Type filter to the value named `label`, or takes that value off again:
 *  the option toggles. The control row's Filter panel (`openFilterPanel`, the dialog named
 *  `Filter`) and the Type trigger in it each say whether what they open is open, read once the
 *  page has drawn them (`expandedState`). */
async function toggleType(page: Page, label: string): Promise<void> {
  await openFilterPanel(page);
  const trigger = page.getByRole("button", { name: "Filter by type" });
  if ((await expandedState("the Type filter", trigger)) === "shut") await trigger.click();
  await page
    .getByRole("listbox", { name: "Type options" })
    .getByRole("option", { name: new RegExp(`^${label}, `) })
    .click();
  await page.keyboard.press("Escape");
}

// SLOW(>=2s): each of the four rows' links is followed to the page it opens and back, eight page
// loads, and the rows are read again below the reading column's width.
test("the inbox lists everything that reached a person in one list", async ({ browser }) => {
  // A server that has just started serves each of those page loads in about five seconds, which
  // takes the row past the 60 s budget (`testTimeout`); served warm, the same row takes 30 s.
  test.setTimeout(180_000);
  // The page reads as a person a reference can name (`named`), so the mention below reaches
  // them; every write of theirs is made as them.
  const asReader = { login: named };
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" }, asReader);
  const issue = await createIssue({ project: "CORE", title: "Release checklist" }, asReader);
  const closed = await createIssue({ project: "CORE", title: "Old rollout notes" }, asReader);
  // The issue waits on the reader, its assignee, so the ask below does too.
  await patchIssue(issue.key, { assignee: named }, asReader);
  // A comment thread the reader started, with the session's reply under it.
  const comment = await createComment(issue.key, { body: "Is the checklist complete?" }, asReader);
  const commentReply = await createComment(
    issue.key,
    { body: "Two items are still open.", reply_to: comment.id },
    asSession()
  );
  // An open ask a session asks on the first issue, after the comment thread's reply and before
  // the message thread's: each row's newest write is newer than the one written before it, so by
  // time the ask sits between two reply rows.
  const ask = await createAsk(issue.key, { question: "Approve the release window?" }, asSession());
  // An issue message thread the reader started, with somebody else's reply under it.
  const message = await createMessage(issue.key, { body: "Who owns the rollback step?" }, asReader);
  const messageReply = await createMessage(
    issue.key,
    { body: "I own the rollback step.", in_reply_to: message.id },
    { login: other }
  );
  // A direct message to the session, with that session's reply.
  const direct = (
    await createAgentMessage(
      THREAD_SESSION.session_id,
      { body: "Please check the staging gate.", delivery: "btw" },
      asReader
    )
  ).id;
  const directReply = await replyToMessageDelivery(
    direct,
    { attempt: 1, body: "The staging gate is open." },
    sessionActor()
  );
  // A reply with a body answers the message it stored; a delivery report answers the attempt.
  if (!("id" in directReply)) throw new Error("the session's reply stored no message");
  // A comment by somebody else that names the reader by the address the text grammar reads
  // (`dispatch://person/<login>`), and one the reader wrote themselves.
  const mention = await createComment(
    issue.key,
    { body: `dispatch://person/${named}, please read the checklist.` },
    { login: other }
  );
  await expectMentioned(named, mention.id);
  const own = await createComment(issue.key, { body: "A note to myself." }, asReader);
  // On the second issue, closed: a thread the reader wrote in and has read. Its shape is
  // somebody else's root and the reader's reply under it, with nothing written after: today's
  // Inbox lists a thread of the reader's from the first write somebody else makes after theirs,
  // closed issue or not, so this thread is one the page does not list by today's rules.
  const closedRoot = await createComment(
    closed.key,
    { body: "Are the rollout notes final?" },
    { login: other }
  );
  await createComment(closed.key, { body: "They are final.", reply_to: closedRoot.id }, asReader);
  await markThreadRead(closedRoot.id, new Date().toISOString(), asReader);
  await patchIssue(closed.key, { status: "done" }, asReader);

  const context = await asUser(browser, named);
  try {
    const page = await context.newPage();
    // Every read of the one read the page sends, to read what it asked for.
    const oneReads: URL[] = [];
    page.on("request", (request: Request) => {
      const url = new URL(request.url());
      if (url.pathname === "/api/v1/me/inbox") oneReads.push(url);
    });
    await page.goto("/");

    // One list of four rows: the open ask, the session's reply in the comment thread, the reply
    // in the issue message thread and the session's reply to the direct message.
    const rows = {
      ask: ask.id,
      comment: replyRowId(comment.id),
      direct: agentRowId(direct),
      message: replyRowId(message.id),
    };
    const four = Object.values(rows).sort();
    await expect
      .poll(async () => (await drawnIds(inboxListRegion(page))).sort(), {
        message: "the rows of the one list",
      })
      .toEqual(four);
    // The reply rows sit among the ask rows rather than in a second list: every row is an item of
    // the one list, and that list orders them by time, newest write first, as it orders every
    // row. None of the four has a priority, so the default grouping draws them in one band, and
    // the ask, written between the comment thread's reply and the message thread's, sits between
    // those two rows.
    await expect
      .poll(async () => (await rowLists(page)).length, {
        message: "the lists the rows are items of: one, the reply rows among the ask rows",
      })
      .toBe(1);
    await expect
      .poll(() => drawnIds(page), {
        message: "the one list, newest write first: the ask between the replies either side of it",
      })
      .toEqual([rows.direct, rows.message, rows.ask, rows.comment]);
    // Nothing reached the reader that the page does not list: their own comment, the thread on
    // the closed issue and the mention of them are no rows of the page.
    for (const absent of [replyRowId(own.id), replyRowId(closedRoot.id), replyRowId(mention.id)]) {
      await expect(page.locator(`[data-inbox-row="${absent}"]`), absent).toHaveCount(0);
    }
    // The reader's own comment is out of the list because nothing was delivered to its author:
    // the one read ties it to them as its author and by no delivery, where the session's reply in
    // the thread the reader started was delivered to them.
    const reached = await getMeInbox({}, asReader);
    const ownTies = relationsOf(reached, "comment", own.id)?.map((relation) => relation.kind);
    expect(ownTies, "what ties the reader to the comment they wrote themselves").toContain(
      "authored_by"
    );
    expect(ownTies, "a delivery of the reader's own comment to its author").not.toContain(
      "delivered_to"
    );
    expect(
      relationsOf(reached, "comment", commentReply.id)?.map((relation) => relation.kind),
      "what ties the reader to the session's reply in the comment thread they started"
    ).toContain("delivered_to");

    // Each row carries its first line and a link that opens the thread at its root: the link is
    // followed, and where it lands draws the root, marked as the item the address names.
    const expected: readonly {
      id: string;
      firstLine: string;
      path: string;
      root: (on: Page) => Locator;
    }[] = [
      {
        firstLine: "Approve the release window?",
        id: rows.ask,
        path: `/issues/${issue.key}/asks/${ask.id}`,
        root: (on) => landedTurn(on, `ask:${ask.id}`),
      },
      {
        firstLine: "Is the checklist complete?",
        id: rows.comment,
        path: `/issues/${issue.key}/comments/${comment.id}`,
        root: (on) => landedTurn(on, `comment:${comment.id}`),
      },
      {
        firstLine: "Who owns the rollback step?",
        id: rows.message,
        path: `/issues/${issue.key}/messages/${message.id}`,
        root: (on) => landedTurn(on, `message:${message.id}`),
      },
      {
        // A direct message's thread is the session's conversation, which the live view draws
        // with the reader's message in it under that message's own key.
        firstLine: "Please check the staging gate.",
        id: rows.direct,
        path: `/agents/${THREAD_SESSION.session_id}/live`,
        root: (on) => frontLiveView(on).locator(`[data-message-id="dispatch:${direct}"]`),
      },
    ];
    for (const row of expected) {
      const listed = inboxRow(page, row.id);
      await expect(listed, `${row.id}'s first line`).toContainText(row.firstLine);
      // The owner link names the thread at its root once the row has drawn it, read until it does.
      const owner = listed.locator("[data-inbox-owner]").first();
      await expect
        .poll(async () => (await owner.getAttribute("href")) ?? "", {
          message: `${row.id}'s link names its thread at the root`,
        })
        .toBe(row.path);
      await owner.click();
      await expect(page, `${row.id}'s link opens ${row.path}`).toHaveURL(
        (url) => url.pathname === row.path
      );
      await expect(row.root(page), `${row.id}'s link opens its thread at the root`).toContainText(
        row.firstLine
      );
      await page.goBack();
      await expect(inboxRow(page, row.id), `the Inbox again, after ${row.path}`).toBeVisible();
      // The pointer is where the link it followed was, and back on the list that is a link whose
      // hover card would open over the next row: it leaves the list before the next click.
      await page.mouse.move(0, 0);
    }

    // Each row carries its author. Beside the reading column a row is one line of what it is
    // about and whose it is, so the author is read where the row draws its card, below that
    // column's width: the asker in the question's card, and each reply's author on that reply's
    // own turn in the thread the row draws.
    const wide = page.viewportSize() ?? { height: 720, width: READING_PANE_WIDTH };
    await page.setViewportSize({ height: wide.height, width: READING_PANE_WIDTH - 1 });
    for (const row of [
      { author: THREAD_SESSION.title, id: rows.ask, on: `[data-testid="ask-${ask.id}"]` },
      {
        author: THREAD_SESSION.title,
        id: rows.comment,
        on: `[data-inbox-thread-turn="${commentReply.id}"]`,
      },
      { author: other, id: rows.message, on: `[data-inbox-thread-turn="${messageReply.id}"]` },
      {
        author: THREAD_SESSION.title,
        id: rows.direct,
        on: `[data-inbox-thread-turn="${directReply.id}"]`,
      },
    ]) {
      await expect(
        inboxRow(page, row.id).locator(row.on),
        `${row.id}'s author on the row`
      ).toContainText(row.author);
    }
    await page.setViewportSize(wide);

    // The page's own Type filter narrows the one list in the browser: Threads holds the two
    // Reply rows, and Agent message the one Agent row, out of the same list.
    await toggleType(page, "Threads");
    await expect
      .poll(async () => (await drawnIds(inboxListRegion(page))).sort())
      .toEqual([rows.comment, rows.message].sort());
    await toggleType(page, "Threads");
    await toggleType(page, "Agent message");
    await expect.poll(() => drawnIds(inboxListRegion(page))).toEqual([rows.direct]);
    await toggleType(page, "Agent message");
    await expect.poll(async () => (await drawnIds(inboxListRegion(page))).sort()).toEqual(four);

    // No narrowing reached the server: every read the page sent carries no relation kind.
    expect(oneReads.length, "the page's reads of the one read").toBeGreaterThan(0);
    for (const url of oneReads) {
      expect(url.searchParams.has("kind"), `${url.pathname}${url.search}`).toBe(false);
    }

    // The old Replies address lands on this list, narrowed to its threads, and on no page of its
    // own.
    await page.goto("/replies");
    await expect(page).toHaveURL(
      (url) => url.pathname === "/" && url.searchParams.get("type") === "reply"
    );
    await expect
      .poll(async () => (await drawnIds(inboxListRegion(page))).sort())
      .toEqual([rows.comment, rows.message].sort());
  } finally {
    await context.close();
  }
});

test("an answered ask stays in the threads a person takes part in", async ({ browser }) => {
  // The reader is a person a reference can name (`named`), so the mention below reaches them;
  // every write of theirs is made as them.
  const asReader = { login: named };
  await createProject({ key: "CORE", name: "Core" }, asReader);
  // Two asks wait on somebody else, on an issue assigned to them, so nothing holds the reader in
  // either but their own answer of the one and their decline of the other. The other two are on
  // an issue assigned to the reader, who takes part in each as the person it waits on: one
  // somebody else answered, and one the reader answered whose answer was then retracted.
  const elsewhere = await createIssue({ project: "CORE", title: "Rollout pages" }, asReader);
  await patchIssue(elsewhere.key, { assignee: other }, asReader);
  const issue = await createIssue({ project: "CORE", title: "Rollout gates" }, asReader);
  await patchIssue(issue.key, { assignee: named }, asReader);
  const asked = (key: string, question: string) =>
    createAsk(key, { options: [{ label: "Yes" }, { label: "No" }], question }, asSession());
  const answeredByReader = await asked(elsewhere.key, "Open the gate on Monday?");
  const declined = await asked(elsewhere.key, "Page the on-call?");
  const answeredByOther = await asked(issue.key, "Keep the old gate?");
  const retracted = await asked(issue.key, "Freeze the schedule?");
  // Before the reader answers the one and declines the other, the one read ties them to neither,
  // read once it ties them to the last ask opened, which waits on them.
  await expect
    .poll(async () => tiesOf(await getMeInbox({}, asReader), "ask", retracted.id) ?? [], {
      message: `the one read ties the reader to "${retracted.question}", which waits on them`,
    })
    .toContain("participates_in outbound");
  const before = await getMeInbox({}, asReader);
  for (const ask of [answeredByReader, declined]) {
    expect(
      tiesOf(before, "ask", ask.id),
      `what ties the reader to "${ask.question}", which waits on ${other}, before they answer or decline it`
    ).toBeUndefined();
  }
  await answerAsk(answeredByReader.id, { expected_edited_at: null, selected: ["Yes"] }, asReader);
  await answerAsk(
    answeredByOther.id,
    { expected_edited_at: null, selected: ["No"] },
    { login: other }
  );
  await answerAsk(retracted.id, { expected_edited_at: null, selected: ["Yes"] }, asReader);
  // No route retracts an answer: an answered ask is closed, a resolve takes only an open one, and
  // an issue's reopen opens again only the asks its close withdrew. So the seed answers the third
  // ask through the API, which appends its ask.answered event and its answer tuples, and then
  // takes the answer off its row here, as the state a retraction leaves: resolved as retracted,
  // with no current answer, while the event and the tuples stay.
  await sql(`
    update asks
    set answer = null, state = 'resolved',
        resolution = jsonb_build_object(
          'kind', 'retracted', 'reason', 'The schedule moved.', 'at', now(),
          'actor', jsonb_build_object('kind', 'user', 'id', ${sqlLiteral(named)}))
    where id = ${sqlLiteral(retracted.id)}::uuid`);
  await declineAsk(declined.id, { note: "Not mine to call." }, asReader);
  // A thread the reader wrote in and has read, on the issue assigned to them: somebody else's root,
  // the reader's reply under it, nothing written after, so today's Inbox does not list it.
  const thread = await createComment(
    issue.key,
    { body: "Is the gate log kept?" },
    { login: other }
  );
  await createComment(issue.key, { body: "It is kept.", reply_to: thread.id }, asReader);
  await markThreadRead(thread.id, new Date().toISOString(), asReader);
  // A thread the reader was only mentioned in, and never wrote in: somebody else's comment that
  // names them by address. Its mention lands as a mentions tuple into their person.
  const mentioned = await createComment(
    issue.key,
    { body: `dispatch://person/${named}, the gate log moved.` },
    { login: other }
  );
  await expectMentioned(named, mentioned.id);
  await patchIssue(elsewhere.key, { status: "done" }, asReader);
  await patchIssue(issue.key, { status: "done" }, asReader);

  // The one read asked as a caller, after both closes, for the threads the reader takes part in.
  const taking = await getMeInbox({ kind: "participates_in" }, asReader);
  const asks = [answeredByReader, answeredByOther, retracted, declined];
  const left = asks.find((ask) => tiesOf(taking, "ask", ask.id) === undefined);
  expect(
    left,
    `the ask "${left?.question}" (${left?.id}) left the threads the reader takes part in`
  ).toBeUndefined();
  for (const ask of asks) {
    expect(tiesOf(taking, "ask", ask.id), `what holds the reader in "${ask.question}"`).toContain(
      "participates_in outbound"
    );
  }
  expect(
    tiesOf(taking, "comment", thread.id),
    "what holds the reader in the thread they wrote in and read"
  ).toContain("participates_in outbound");
  // The thread they were only mentioned in is the read's answer after the close too: read whole,
  // it ties the reader to that comment by its mention of them.
  expect(
    tiesOf(await getMeInbox({}, asReader), "comment", mentioned.id),
    "what ties the reader, after the close, to the thread they were only mentioned in"
  ).toContain("mentions inbound");

  // The answer and the decline as a caller reads them, after the close. The retracted ask's answer
  // survives in its ask.answered event and in its answer tuples: the answered_by and decided_by
  // into the reader, and the answer its node holds, which the Decisions read answers. The declined
  // ask has neither: a decline appends no answer tuple and raises no ask.answered event.
  const answeredEvents = async (key: string, askId: string) =>
    (await getIssueEvents(key, { limit: 200 }, asReader)).flatMap((event) =>
      event.type === "ask.answered" && event.payload.id === askId
        ? [[event.payload.answer?.user, event.payload.answer?.selected]]
        : []
    );
  expect(
    await answeredEvents(issue.key, retracted.id),
    `the ask.answered events of "${retracted.question}"`
  ).toEqual([[named, ["Yes"]]]);
  expect(
    await answeredEvents(elsewhere.key, declined.id),
    `the ask.answered events of "${declined.question}"`
  ).toEqual([]);
  const whole = await getMeInbox({}, asReader);
  expect(
    tiesOf(whole, "ask", retracted.id),
    `what ties the reader to "${retracted.question}" after the close`
  ).toEqual(expect.arrayContaining(["answered_by inbound", "decided_by inbound"]));
  const retractedAnswer = (await getIssueDecisions(issue.key, asReader)).entries?.find(
    (entry) => entry.node.kind === "ask" && entry.node.id === retracted.id
  );
  expect(
    [retractedAnswer?.answered_by, retractedAnswer?.selected],
    `the answer the node of "${retracted.question}" holds`
  ).toEqual([named, ["Yes"]]);
  expect(
    (tiesOf(whole, "ask", declined.id) ?? []).filter(
      (tie) => tie === "answered_by inbound" || tie === "decided_by inbound"
    ),
    `the answer tuples of "${declined.question}" into the reader`
  ).toEqual([]);

  // The page lists neither thread and none of the four asks: these are answers of the read for
  // its callers, and the Inbox lists the rows it lists today, which are the rows its own read
  // answered.
  const context = await asUser(browser, named);
  try {
    const page = await context.newPage();
    const answered = page.waitForResponse((response) => inboxRead(new URL(response.url())));
    await page.goto("/");
    const drawn = pageRowIds(((await (await answered).json()) as MeInboxResponse).entries);
    // The list the page draws once both its reads have answered - rows or its empty state - is
    // there before the rows are compared: an empty answer would otherwise match the page before
    // it drew anything.
    await expect(inboxListRegion(page), "the Inbox page drew its list").toBeAttached();
    await expect
      .poll(async () => (await drawnIds(inboxListRegion(page))).sort())
      .toEqual([...drawn].sort());
    expect(drawn, "the rows the page draws").not.toContain(replyRowId(thread.id));
    expect(drawn, "the rows the page draws").not.toContain(replyRowId(mentioned.id));
    for (const absent of [replyRowId(thread.id), replyRowId(mentioned.id)]) {
      await expect(page.locator(`[data-inbox-row="${absent}"]`), absent).toHaveCount(0);
    }
    // None of the four asks is among the rows the page draws at `/`: the page keeps the rows it
    // has, and an ask answered, retracted or declined on a closed issue is none of them.
    const listed = await drawnIds(page);
    for (const ask of asks) {
      expect(listed, `the Inbox at / lists the ask "${ask.question}" (${ask.id})`).not.toContain(
        ask.id
      );
    }
  } finally {
    await context.close();
  }
});
