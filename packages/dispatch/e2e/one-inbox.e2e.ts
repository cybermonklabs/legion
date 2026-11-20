import type { MeInboxEntry, MeInboxResponse } from "../web/src/api/types";
import { setLiveSessions } from "./agents";
import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  dashboardOrigin,
  declineAsk,
  getMeInbox,
  markThreadRead,
  patchIssue,
  sessionCookie,
} from "./api";
import { type BaseServer, baseBuild, bootBuild, stackCommand } from "./base-build";
import { inboxPane, selectRow } from "./inbox-pane";
import {
  agentRowId,
  asSession,
  replyRowId,
  seedAgentThread,
  seedAskThread,
  seedCommentThread,
  THREAD_SESSION,
} from "./inbox-threads";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";
import { expandedState } from "./settled";
import { expect, type Page, type Request, test } from "./test";
import { asUser } from "./users";

/**
 * dispatch://INFRA-1152's browser rows of the one read, GET /api/v1/me/inbox.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** The person whose reads the comparison is planted a mismatch in, and who reads the page. */
const reader = "alice";
/** Somebody else, who writes in the reader's threads and holds no row of their own. */
const other = "bob";
/** The person the first comparison does not reach. */
const excluded = "carol";

/** The links of the comment reply chain at the outbox walk's bound, the depth
 *  `internal/dispatch/inboxcompare`'s ChainLinks copies. */
const chainLinks = 32;

/** The corpus the comparison reads: what c-one-list seeds, and the shapes the gate requires
 *  beside it, every one through the API as a person and a session write it but the reply chain,
 *  which only the release before could write (POST comments climbs reply_to to the thread's
 *  head). The chain is written as that release wrote it, and the backfill then runs over it as a
 *  deployment's runs before anybody reads the new read. */
async function seedCorpus(): Promise<{ issue: string }> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Release checklist" });
  const closed = await createIssue({ project: "CORE", title: "Old rollout notes" });
  // An open ask a session asks, which waits on the reader, the project's default assignee.
  await createAsk(issue.key, { question: "Approve the release window?" }, asSession());
  // A comment thread the reader started, with a session's reply under it.
  await seedCommentThread(issue.key, "Is the checklist complete?", "Two items are still open.");
  // An issue message thread the reader started, with somebody else's reply under it.
  const root = await createMessage(issue.key, { body: "Who owns the rollback step?" });
  await createMessage(issue.key, { body: "I own it.", in_reply_to: root.id }, { login: other });
  // A direct message to a session, with that session's reply.
  await seedAgentThread({ body: "Please check the staging gate.", replies: ["The gate is open."] });
  // An ask the reader answered, with the session's reply in its thread.
  await seedAskThread(issue.key, "Ship on Friday?", "Friday it is.");
  // A comment by somebody else that names the reader, and one the reader wrote themselves.
  await createComment(issue.key, { body: `person:${reader}, please read this.` }, { login: other });
  await createComment(issue.key, { body: "A note to myself." });
  // A thread on the closed issue the reader wrote in and has already read.
  const read = await createComment(closed.key, { body: "Are the notes final?" });
  await createComment(closed.key, { body: "They are.", reply_to: read.id }, { login: other });
  await markThreadRead(read.id, new Date().toISOString());
  await patchIssue(closed.key, { status: "done" });
  // The excluded person signs in, so the database holds them among its people.
  await sessionCookie(excluded);
  await sql(`
    do $$
    declare
      parent uuid;
      link uuid;
    begin
      for depth in 0..${chainLinks} loop
        insert into comments (issue_key, author, body, reply_to, created_at)
        values (${sqlLiteral(issue.key)},
                case when depth % 2 = 0 then jsonb_build_object('kind', 'user', 'id', ${sqlLiteral(reader)})
                     else jsonb_build_object('kind', 'session', 'id', ${sqlLiteral(THREAD_SESSION.session_id)}) end,
                'A link of the chain, ' || depth || ' deep.', parent,
                now() - interval '1 hour' + depth * interval '1 second')
        returning id into link;
        parent := link;
      end loop;
    end $$`);
  const backfill = await stackCommand(["backfill-graph-store"]);
  expect(backfill.code, `backfill-graph-store:\n${backfill.stdout}${backfill.stderr}`).toBe(0);
  return { issue: issue.key };
}

/** A thread of the reader's as the release before writes it, through no writer of this build:
 *  their root and somebody else's reply under it, which the old trigger records as a Reply row of
 *  theirs and to which this build's writer appends no tuple. It is planted after the backfill ran,
 *  as a write an older build makes during a roll, so the one read does not answer it: the
 *  comparison's mismatch. */
async function plantMismatch(issue: string): Promise<{ root: string; reply: string }> {
  const author = (login: string) =>
    `jsonb_build_object('kind', 'user', 'id', ${sqlLiteral(login)})`;
  // Each insert answers its id from a select, since psql prints an insert's own command tag
  // beside what its returning clause answers.
  const root = await sql(`
    with written as (
      insert into comments (issue_key, author, body, created_at)
      values (${sqlLiteral(issue)}, ${author(reader)}, 'Written by the release before.', now())
      returning id)
    select id from written`);
  const reply = await sql(`
    with written as (
      insert into comments (issue_key, author, body, reply_to, created_at)
      values (${sqlLiteral(issue)}, ${author(other)}, 'Answered by the release before.',
              ${sqlLiteral(root)}::uuid, now() + interval '1 second')
      returning id)
    select id from written`);
  return { reply, root };
}

/** Takes the planted thread away again, with the Reply rows the old trigger wrote for it. */
async function clearMismatch(planted: { root: string; reply: string }): Promise<void> {
  await sql(
    `delete from user_threads where thread_id = ${sqlLiteral(planted.root)}::uuid`,
    `delete from comments where id in (${sqlLiteral(planted.reply)}::uuid, ${sqlLiteral(planted.root)}::uuid)`
  );
}

/** The four things the page states, read from the page as a person reads them. */
async function expectPageStates(
  page: Page,
  want: { differences: number; people: number; uncovered: number; met: boolean }
): Promise<void> {
  await page.goto("/inbox/comparison");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox comparison" })).toBeVisible();
  const run = page.getByRole("definition");
  await expect(run).not.toHaveCount(0);
  await expect
    .soft(page.getByTestId("comparison-differences"), "the difference count")
    .toHaveText(String(want.differences));
  await expect
    .soft(page.getByTestId("comparison-people-checked"), "the people checked")
    .toHaveText(String(want.people));
  await expect
    .soft(page.getByTestId("comparison-people-uncovered"), "the uncovered count")
    .toHaveText(String(want.uncovered));
  await expect
    .soft(page.getByTestId("comparison-gate"), "the sentence that says whether the gate is met")
    .toHaveText(want.met ? "The gate is met." : "The gate is not met.");
}

test("the comparison page states the gate in plain words", async ({ browser }, testInfo) => {
  // The base build is compiled once per run, which the first row to need it pays for.
  test.setTimeout(1_200_000);
  const build = await baseBuild();
  test.skip("skip" in build, "skip" in build ? build.skip : "");
  if ("skip" in build) return;
  const { issue } = await seedCorpus();
  const planted = await plantMismatch(issue);
  const people = Number(await sql("select count(*) from people"));

  let base: BaseServer | undefined;
  const alice = await asUser(browser, reader);
  try {
    base = await bootBuild(build.binary);
    const compare = async (exclude: readonly string[]) => {
      const run = await stackCommand([
        "inbox-compare",
        "--old-url",
        base?.url ?? "",
        "--new-url",
        dashboardOrigin,
        ...exclude.flatMap((login) => ["--exclude", login]),
      ]);
      await testInfo.attach(`inbox-compare ${exclude.length === 0 ? "clean" : "excluding"}`, {
        body: `exit ${run.code}\n${run.stdout}${run.stderr}`,
        contentType: "text/plain",
      });
      return run;
    };
    const page = await alice.newPage();

    const mismatched = await compare([excluded]);
    expect
      .soft(mismatched.code, `the first comparison:\n${mismatched.stdout}${mismatched.stderr}`)
      .toBe(1);
    await expectPageStates(page, { differences: 1, met: false, people, uncovered: 1 });

    await clearMismatch(planted);
    const clean = await compare([]);
    expect.soft(clean.code, `the second comparison:\n${clean.stdout}${clean.stderr}`).toBe(0);
    await expectPageStates(page, { differences: 0, met: true, people, uncovered: 0 });
  } finally {
    await alice.close();
    await base?.stop();
  }
});

/** The Inbox page's opt-in to the one read, which these rows open the page with while the
 *  default still names the reads it replaces. The default flip (T17) empties it, and the rows run
 *  as they are on the default. */
const ONE_READ: Readonly<Record<string, string>> = { one: "1" };

/** `path` with `params`, and with the opt-in to the one read. */
function address(path: string, params: Readonly<Record<string, string>> = {}): string {
  const search = new URLSearchParams({ ...params, ...ONE_READ }).toString();
  return search === "" ? path : `${path}?${search}`;
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

/** Every row id the Inbox list holds, in its order. */
function listedRows(page: Page): Promise<string[]> {
  return page
    .locator("[data-inbox-row]")
    .evaluateAll((rows) => rows.map((row) => row.getAttribute("data-inbox-row") ?? ""));
}

/** Narrows the page's own Type filter to the value named `label`, or takes that value off again:
 *  the option toggles. The bar's Filter control and the Type trigger each say whether what they
 *  open is open, read once the page has drawn them (`expandedState`). */
async function toggleType(page: Page, label: string): Promise<void> {
  const filter = page.locator("[data-inbox-filter]");
  if ((await expandedState("the Inbox's Filter", filter)) === "shut") await filter.click();
  const trigger = page.getByRole("button", { name: "Filter by type" });
  if ((await expandedState("the Type filter", trigger)) === "shut") await trigger.click();
  await page
    .getByRole("listbox", { name: "Type options" })
    .getByRole("option", { name: new RegExp(`^${label}, `) })
    .click();
  await page.keyboard.press("Escape");
}

test("the inbox lists everything that reached a person in one list", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Release checklist" });
  const closed = await createIssue({ project: "CORE", title: "Old rollout notes" });
  // An open ask a session asks on the first issue, which waits on the reader, its assignee.
  await patchIssue(issue.key, { assignee: reader });
  const ask = await createAsk(issue.key, { question: "Approve the release window?" }, asSession());
  // A comment thread the reader started, with the session's reply under it.
  const comment = await seedCommentThread(
    issue.key,
    "Is the checklist complete?",
    "Two items are still open."
  );
  // An issue message thread the reader started, with somebody else's reply under it.
  const message = await createMessage(issue.key, { body: "Who owns the rollback step?" });
  await createMessage(
    issue.key,
    { body: "I own the rollback step.", in_reply_to: message.id },
    { login: other }
  );
  // A direct message to the session, with that session's reply.
  const direct = await seedAgentThread({
    body: "Please check the staging gate.",
    replies: ["The staging gate is open."],
  });
  // A comment by somebody else that names the reader, and one the reader wrote themselves.
  const mention = await createComment(
    issue.key,
    { body: `person:${reader}, please read the checklist.` },
    { login: other }
  );
  const own = await createComment(issue.key, { body: "A note to myself." });
  // On the second issue, closed: a thread the reader wrote in and has read. Its shape is
  // somebody else's root and the reader's reply under it, with nothing written after: today's
  // Inbox lists a thread of the reader's from the first write somebody else makes after theirs,
  // closed issue or not, so this thread is one the page does not list by today's rules.
  const closedRoot = await createComment(
    closed.key,
    { body: "Are the rollout notes final?" },
    { login: other }
  );
  await createComment(closed.key, { body: "They are final.", reply_to: closedRoot.id });
  await markThreadRead(closedRoot.id, new Date().toISOString());
  await patchIssue(closed.key, { status: "done" });

  const alice = await asUser(browser, reader);
  try {
    const page = await alice.newPage();
    // Every read of the one read the page sends, to read what it asked for.
    const oneReads: URL[] = [];
    page.on("request", (request: Request) => {
      const url = new URL(request.url());
      if (url.pathname === "/api/v1/me/inbox") oneReads.push(url);
    });
    const answered = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === "/api/v1/me/inbox" &&
        new URL(response.url()).searchParams.get("state") === "inbox"
    );
    await page.goto(address("/"));
    const entries = ((await (await answered).json()) as MeInboxResponse).entries;

    // One list of four rows: the open ask, the session's reply in the comment thread, the reply
    // in the issue message thread and the session's reply to the direct message, the replies
    // among the asks.
    const rows = {
      ask: ask.id,
      comment: replyRowId(comment),
      direct: agentRowId(direct),
      message: replyRowId(message.id),
    };
    const four = Object.values(rows).sort();
    await expect
      .poll(async () => (await listedRows(page)).sort(), { message: "the rows of the one list" })
      .toEqual(four);
    // Nothing reached the reader that the page does not list: their own comment, the thread on
    // the closed issue and the mention of them are no rows of the page.
    for (const absent of [replyRowId(own.id), replyRowId(closedRoot.id), replyRowId(mention.id)]) {
      await expect(page.locator(`[data-inbox-row="${absent}"]`), absent).toHaveCount(0);
    }

    // Each row carries its first line, its author, and a link that opens the thread at its root.
    const refOf = (id: string) => {
      const entry = (entries ?? []).find((candidate) => candidate.node.id === id);
      return entry?.thread?.ref ?? entry?.agent?.ref;
    };
    const expected: readonly {
      id: string;
      firstLine: string;
      author: string;
      href: (href: string) => boolean;
    }[] = [
      {
        author: THREAD_SESSION.title,
        firstLine: "Approve the release window?",
        href: (href) => href.endsWith(`/asks/${ask.id}`),
        id: rows.ask,
      },
      {
        author: THREAD_SESSION.title,
        firstLine: "Is the checklist complete?",
        href: (href) => href === refOf(comment) && href.includes(comment),
        id: rows.comment,
      },
      {
        author: other,
        firstLine: "Who owns the rollback step?",
        href: (href) => href === refOf(message.id) && href.includes(message.id),
        id: rows.message,
      },
      {
        author: THREAD_SESSION.title,
        firstLine: "Please check the staging gate.",
        href: (href) => href === refOf(direct),
        id: rows.direct,
      },
    ];
    for (const row of expected) {
      const listed = page.locator(`[data-inbox-row="${row.id}"]`);
      await expect(listed, `${row.id}'s first line`).toContainText(row.firstLine);
      // The owner link names the thread at its root once the row has drawn it, read until it does.
      const owner = listed.locator("[data-inbox-owner]").first();
      await expect
        .poll(async () => row.href((await owner.getAttribute("href")) ?? ""), {
          message: `${row.id}'s link opens its thread at the root`,
        })
        .toBe(true);
      await selectRow(page, row.id);
      await expect(inboxPane(page), `${row.id}'s author in the reading pane`).toContainText(
        row.author
      );
    }

    // The page's own Type filter narrows the one list in the browser: Threads holds the two
    // Reply rows, and Agent message the one Agent row, out of the same list.
    await toggleType(page, "Threads");
    await expect
      .poll(async () => (await listedRows(page)).sort())
      .toEqual([rows.comment, rows.message].sort());
    await toggleType(page, "Threads");
    await toggleType(page, "Agent message");
    await expect.poll(() => listedRows(page)).toEqual([rows.direct]);
    await toggleType(page, "Agent message");
    await expect.poll(async () => (await listedRows(page)).sort()).toEqual(four);

    // No narrowing reached the server: every read the page sent carries no relation kind.
    expect(oneReads.length, "the page's reads of the one read").toBeGreaterThan(0);
    for (const url of oneReads) {
      expect(url.searchParams.has("kind"), `${url.pathname}${url.search}`).toBe(false);
    }

    // The old Replies address lands on this list, narrowed to its threads, and on no page of its
    // own.
    await page.goto(address("/replies"));
    await expect(page).toHaveURL(
      (url) => url.pathname === "/" && url.searchParams.get("type") === "reply"
    );
    await expect
      .poll(async () => (await listedRows(page)).sort())
      .toEqual([rows.comment, rows.message].sort());
  } finally {
    await alice.close();
  }
});

test("an answered ask stays in the threads a person takes part in", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Rollout gates" });
  await patchIssue(issue.key, { assignee: reader });
  const asked = (question: string) =>
    createAsk(issue.key, { options: [{ label: "Yes" }, { label: "No" }], question }, asSession());
  const answeredByReader = await asked("Open the gate on Monday?");
  const answeredByOther = await asked("Keep the old gate?");
  const retracted = await asked("Freeze the schedule?");
  const declined = await asked("Page the on-call?");
  await answerAsk(answeredByReader.id, { expected_edited_at: null, selected: ["Yes"] });
  await answerAsk(
    answeredByOther.id,
    { expected_edited_at: null, selected: ["No"] },
    { login: other }
  );
  await answerAsk(retracted.id, { expected_edited_at: null, selected: ["Yes"] });
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
          'actor', jsonb_build_object('kind', 'user', 'id', ${sqlLiteral(reader)}))
    where id = ${sqlLiteral(retracted.id)}::uuid`);
  await declineAsk(declined.id, { note: "Not mine to call." });
  // A thread the reader wrote in and has read, on the same issue: somebody else's root, the
  // reader's reply under it, nothing written after, so today's Inbox does not list it.
  const thread = await createComment(
    issue.key,
    { body: "Is the gate log kept?" },
    { login: other }
  );
  await createComment(issue.key, { body: "It is kept.", reply_to: thread.id });
  await markThreadRead(thread.id, new Date().toISOString());
  // The thread the reader was only mentioned in, and never wrote in, is the mention case: it
  // lands with the mention tuples (dispatch://INFRA-1152's T13), beside the thread above.
  await patchIssue(issue.key, { status: "done" });

  // The one read asked as a caller, for the threads the reader takes part in.
  const taking = await getMeInbox({ kind: "participates_in" }, { login: reader });
  const tied = (kind: string, id: string) =>
    (taking.entries ?? []).find((entry) => entry.node.kind === kind && entry.node.id === id);
  const asks = [answeredByReader, answeredByOther, retracted, declined];
  const left = asks.find((ask) => tied("ask", ask.id) === undefined);
  expect(
    left,
    `the ask "${left?.question}" (${left?.id}) left the threads the reader takes part in`
  ).toBeUndefined();
  for (const ask of asks) {
    expect(
      tied("ask", ask.id)?.relations?.map(({ direction, kind }) => `${kind} ${direction}`),
      `what holds the reader in "${ask.question}"`
    ).toContain("participates_in outbound");
  }
  expect(
    tied("comment", thread.id)?.relations?.map(({ direction, kind }) => `${kind} ${direction}`),
    "what holds the reader in the thread they wrote in and read"
  ).toContain("participates_in outbound");

  // The page lists neither thread: these are answers of the read for its callers, and the Inbox
  // lists the rows it lists today, which are the rows its own read answered.
  const alice = await asUser(browser, reader);
  try {
    const page = await alice.newPage();
    const answered = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname === "/api/v1/me/inbox" &&
        new URL(response.url()).searchParams.get("state") === "inbox"
    );
    await page.goto(address("/"));
    const drawn = pageRowIds(((await (await answered).json()) as MeInboxResponse).entries);
    // The list the page draws once both its reads have answered - rows or its empty state - is
    // there before the rows are compared: an empty answer would otherwise match the page before
    // it drew anything.
    await expect(page.getByTestId("inbox-list"), "the Inbox page drew its list").toBeAttached();
    await expect.poll(async () => (await listedRows(page)).sort()).toEqual([...drawn].sort());
    expect(drawn, "the rows the page draws").not.toContain(replyRowId(thread.id));
  } finally {
    await alice.close();
  }
});
