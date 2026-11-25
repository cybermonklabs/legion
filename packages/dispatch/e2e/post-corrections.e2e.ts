import { type FakeSession, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProject,
  deleteComment,
  deleteMessage,
  editComment,
  editMessage,
  getCommentHistory,
  getMessageHistory,
  search,
} from "./api";
import { marginCard, openSpec } from "./editor";
import { hasReadingPane, inboxPane } from "./inbox-pane";
import {
  holdReadMarks,
  replyRowId,
  seedCommentThread,
  seedMessageThread,
  THREAD_SESSION,
} from "./inbox-threads";
import { expect, type Locator, test } from "./test";
import { asUser } from "./users";

/**
 * What a reader meets on a page once a post's author corrected it: an edited post carries
 * ` · edited` on its byline, and a deleted one carries the line `Deleted by <author> · <time>`
 * (`data-post-deleted`) where its text was, with no part of that text anywhere on the page
 * (`web/src/features/refs/PostMarks.tsx`).
 *
 * Six rows. Three read the four pages chk-pages names: the issue's Conversation and the margin
 * of its spec in one row, which read one deleted comment two ways from one seeding; the Inbox
 * thread of a reply to such a comment; and a session's own conversation page. Each of those
 * seeds through the API and then reads the page, because the mark is the server's record drawn
 * by the component rather than anything the browser can make on its own, and each pays a Go
 * server, a Postgres, a sign-in and a database reset for that, which is why the two pages that
 * read one comment are read by one row. That row also reads the two posts' rows on the
 * References list of a second issue's Artifacts tab, from a message of that issue that links to
 * both. Two more hold a page open while the post's author edits it and then deletes it through
 * the API - the Inbox's thread rows and the answers page - and read the marks without a reload:
 * the event each correction appends makes the page read its list again
 * (`web/src/api/event-refresh.ts`). The last holds the search palette open over a
 * comment's hit while its author deletes it. Every other surface that prints a body is covered
 * by a test of its component in `web/src`, which CI runs.
 *
 * No row here carries the ` @pr` tag, so the suite that answers a pull request schedules none
 * of them and the full lane, which the merge queue and the nightly run, holds all six:
 * `main` is gated exactly as it would be with the tag. The lane's real-stack share is full at
 * its 150 (`e2e/__tests__/pull-request-lane.test.ts`), and the stubbed layer cannot take these
 * rows, because each writes to the server — a `PATCH` of a message, a `DELETE` of a comment —
 * and then reads the page that write produced, which is the shape a recording answers with the
 * state it recorded before the write (`AGENTS.md`, "The stubbed layer"). The spec is run on the
 * pull request itself by the role driving the issue, which is what `chk-pages` asks for.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host
 * or project.
 */

const spec = "The quick brown fox jumps over the lazy dog.";

/** The session whose messages the Conversation rows write, and which answers the margin's
 *  thread: live, so a send to it has a listener. */
const planner: FakeSession = {
  capabilities: ["aside", "btw", "steer"],
  roles: [],
  session_id: "e2e-corrections-planner",
  title: "Release bot",
};

/** The options a write made as that session carries. */
const asPlanner = {
  actor: { id: planner.session_id, kind: "session" as const },
  as: "agent" as const,
};

/** The deleted line a post leaves behind, wherever the page draws it. */
function deletedLine(within: Locator): Locator {
  return within.locator("[data-post-deleted]");
}

// The two marks on the pages a reader opens first: a build that prints a deleted comment's
// body, or leaves an edited message unmarked, fails here.
test("the issue page and the document margin mark an edited message and a deleted comment", async ({
  browser,
}) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", spec, title: "The export window" });
  const message = await createMessage(
    issue.key,
    { body: "The export runs at 02:00 local." },
    asPlanner
  );
  await editMessage(message.id, { body: "Corrected: the export runs at 02:00 UTC." }, asPlanner);
  const comment = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "brown" },
    body: "The ledger replays twice at 02:00.",
  });
  await createComment(
    issue.key,
    { body: "I will read the replay window.", reply_to: comment.id },
    asPlanner
  );
  await deleteComment(comment.id);
  // A post that cites the two corrected posts, in words, so each reads as an inline link.
  const citing = await createMessage(
    issue.key,
    {
      body: `The replay was dispatch://${issue.key}/comment/${comment.id} and the window is dispatch://${issue.key}/message/${message.id} now.`,
    },
    asPlanner
  );

  // The record behind the two marks: the message's earlier body is kept, and the deleted
  // comment's is given to no read, so the page has nothing of it to print.
  const edits = await getMessageHistory(message.id);
  expect(edits.entries?.map((entry) => entry.body)).toEqual(["The export runs at 02:00 local."]);
  const withdrawn = await getCommentHistory(comment.id);
  expect(withdrawn.deleted_at).not.toBeNull();
  expect(withdrawn.entries?.map((entry) => entry.body)).toEqual([""]);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);

    const messageTurn = page.locator(`[data-turn="message:${message.id}"]`);
    await expect(messageTurn).toContainText("Corrected: the export runs at 02:00 UTC.");
    await expect(messageTurn.getByText("edited")).toBeVisible();
    await expect(page.getByText("The export runs at 02:00 local.")).toHaveCount(0);

    // The thread keeps its place, and the reply under it keeps its own text.
    const commentTurn = page.locator(`[data-turn="comment:${comment.id}"]`);
    await expect(deletedLine(commentTurn)).toHaveText(/^Deleted by alice · /);
    await commentTurn.getByRole("button", { name: "Expand thread" }).click();
    const replies = commentTurn.getByRole("list", { name: "Replies" });
    await expect(replies.getByText("I will read the replay window.")).toBeVisible();
    await expect(page.getByText("The ledger replays twice at 02:00.")).toHaveCount(0);

    // A link to a corrected post prints it as the post's own turn does: the deleted comment's
    // link says it is deleted, where its emptied body's headline drew a link with no text and no
    // size, and the edited message's carries ` · edited` after its words.
    const citingTurn = page.locator(`[data-turn="message:${citing.id}"]`);
    const deletedLink = citingTurn.locator(
      `a[data-dispatch-ref="dispatch://${issue.key}/comment/${comment.id}"]`
    );
    await expect(deletedLink).toHaveText("Deleted by alice");
    await expect(deletedLink).toBeVisible();
    await expect(
      citingTurn.locator(`a[data-dispatch-ref="dispatch://${issue.key}/message/${message.id}"]`)
    ).toHaveText("Corrected: the export runs at 02:00 UTC. · edited");

    // The margin card draws the comment row's own body, so this row holds two layers at once:
    // the delete clears that body on the server, and the card draws the deleted line in place
    // of whatever body it is given. It fails only when both are cut (dispatch://INFRA-1523).
    await openSpec(page, issue.key, spec);
    const card = marginCard(page, comment.id);
    await expect(deletedLine(card)).toHaveText(/^Deleted by alice · /);
    await expect(page.getByText("The ledger replays twice at 02:00.")).toHaveCount(0);
    await expect(card.getByText(/^1 reply · last reply/)).toBeVisible();

    // The Artifacts tab's References rows name each node an issue's own content links to by the
    // head of its words. An issue's own posts are no rows of its own references read
    // (`refs/closure.go`), so a message of a second issue links to both: the deleted comment's
    // row is the deleted line, with none of its words, and the edited message's carries
    // ` · edited` after its words. Each stays a link to its post.
    const linking = await createIssue({ project: "CORE", title: "The replay audit" });
    await createMessage(
      linking.key,
      {
        body: `The audit reads dispatch://${issue.key}/comment/${comment.id} and dispatch://${issue.key}/message/${message.id} again.`,
      },
      asPlanner
    );
    await page.goto(`/issues/${linking.key}/artifacts`);
    await page.getByText("References (2)", { exact: true }).click();
    const references = page.getByRole("region", { exact: true, name: "References" });
    await expect(references.getByRole("listitem")).toHaveCount(2);
    const deletedRow = references.locator(`a[href="/issues/${issue.key}/comments/${comment.id}"]`);
    await expect(deletedLine(deletedRow)).toHaveText(/^Deleted by alice · /);
    await expect(page.getByText("The ledger replays twice at 02:00.")).toHaveCount(0);
    await expect(
      references.locator(`a[href="/issues/${issue.key}/messages/${message.id}"]`)
    ).toHaveText("Corrected: the export runs at 02:00 UTC. · edited");
  } finally {
    await alice.close();
  }
});

// The Inbox draws a thread from its own flattened view model, which drops a field it does not
// name, so a deleted root reaches it only while that model carries the mark.
test("the Inbox thread of a reply to a deleted comment reads that comment as deleted", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The replay window" });
  const root = await seedCommentThread(
    issue.key,
    "How far back does the replay go?",
    "Seven days, and the eighth is dropped."
  );
  await deleteComment(root);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const row = page.locator(`[data-inbox-row="${replyRowId(root)}"]`);
    await expect(row).toBeVisible();
    const body = hasReadingPane(page)
      ? inboxPane(page).locator("[data-inbox-thread-body]")
      : row.locator("[data-inbox-thread-body]");
    if (hasReadingPane(page)) {
      await row.locator("time").first().click();
      await expect(row).toHaveAttribute("data-inbox-selected", "true");
    }
    await expect(body).toBeVisible();

    const rootTurn = body.locator(`[data-inbox-thread-turn="${root}"]`);
    await expect(deletedLine(rootTurn)).toHaveText(/^Deleted by alice · /);
    await expect(body.getByText("Seven days, and the eighth is dropped.")).toBeVisible();
    await expect(page.getByText("How far back does the replay go?")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// An Inbox left open while a thread's root is edited and then deleted reads its rows again on
// each correction's event: the row prints the root, so it carries ` · edited` after the edit and
// the deleted line in place of the words after the delete, with no reload. One comment thread and
// one issue message thread, since each kind of post has correction events of its own. The page's
// read marks are held, so no mark of a thread the reading column shows reads the rows again in
// their place.
// SLOW(>=2s): two posts written, edited and deleted through the API on top of the sign-in, and
// each correction waited for on the open page through the live feed, which starts a read's
// refetches at least a second apart (`MIN_REFETCH_INTERVAL_MS`).
test("an open Inbox marks a thread's root edited and then deleted without a reload", async ({
  browser,
}) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The replay window" });
  const comment = await seedCommentThread(
    issue.key,
    "How far back does the replay go?",
    "Seven days, and the eighth is dropped."
  );
  const message = await seedMessageThread(
    issue.key,
    "Is the mirror caught up?",
    "Two hours behind, replaying."
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await holdReadMarks(page);
    await page.goto("/");
    const commentRow = page.locator(`[data-inbox-row="${replyRowId(comment)}"]`);
    const messageRow = page.locator(`[data-inbox-row="${replyRowId(message)}"]`);
    await expect(commentRow).toContainText("How far back does the replay go?");
    await expect(messageRow).toContainText("Is the mirror caught up?");
    await expect(commentRow).not.toContainText("edited");

    await editComment(comment, { body: "How far back does the nightly replay go?" });
    await editMessage(message, { body: "Is the mirror caught up yet?" });
    await expect(commentRow).toContainText("How far back does the nightly replay go? · edited");
    await expect(messageRow).toContainText("Is the mirror caught up yet? · edited");

    await deleteComment(comment);
    await deleteMessage(message);
    await expect(deletedLine(commentRow)).toHaveText("Deleted by alice");
    await expect(deletedLine(messageRow)).toHaveText("Deleted by alice");
    await expect(commentRow).not.toContainText("replay go");
    await expect(messageRow).not.toContainText("caught up");
    await expect(page.getByText(/replay go\?|caught up/)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// The answers page left open while its reader edits and then deletes a reply they wrote on an ask
// reads its rows again on each correction's event: the row marks the reply edited, then says it
// is deleted and prints none of it, with no reload.
// SLOW(>=2s): an ask and a reply written, then edited and deleted through the API on top of the
// sign-in, and each correction waited for on the open page through the live feed, which starts a
// read's refetches at least a second apart (`MIN_REFETCH_INTERVAL_MS`).
test("an open answers page marks a reply edited and then deleted without a reload", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Build question" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Nightly" }, { label: "Release" }], question: "Which build ships?" },
    asPlanner
  );
  const reply = await createComment(issue.key, { ask_id: ask.id, body: "Does nightly count?" });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/answers");
    const row = page.locator(`[data-answer-row="${ask.id}"]`);
    await expect(row).toContainText("Replied: Does nightly count?");

    await editComment(reply.id, { body: "Does the nightly build count?" });
    await expect(row).toContainText("Replied · edited: Does the nightly build count?");

    await deleteComment(reply.id);
    await expect(deletedLine(row)).toHaveText(/^Deleted by alice · /);
    await expect(row).not.toContainText("count?");
    await expect(row).not.toContainText("edited");
  } finally {
    await alice.close();
  }
});

// The session's own page reads its stored messages through a transcript of its live
// conversation, which carries the two marks as metadata rather than as fields of the row.
test("the agent conversation page marks an edited issue message", async ({ browser }) => {
  await setLiveSessions([planner]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The nightly export" });
  const message = await createMessage(
    issue.key,
    {
      body: "Planner, the export failed at 02:00 local.",
      delivery: "btw",
      target: `session:${planner.session_id}`,
    },
    { login: "bob" }
  );
  await editMessage(
    message.id,
    { body: "Planner, the export failed at 02:00 UTC." },
    { login: "bob" }
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/agents/${planner.session_id}/live`);
    const sent = page.getByTestId("agent-message-other");
    await expect(sent).toContainText("Planner, the export failed at 02:00 UTC.");
    await expect(sent.getByText("edited")).toBeVisible();
    await expect(page.getByText("Planner, the export failed at 02:00 local.")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// R3 of dispatch://INFRA-1523: the search palette left open over a comment's hit, and the same
// words typed again into a palette reopened at once, print none of the comment once its author
// deletes it, with no reload. The palette keeps each answer under its words for 30 s
// (`useTieredSearch.ts`, the page's staleTime), so a correction that refreshes no search leaves
// the deleted words on the page for as long as that answer is kept.
// SLOW(>=2s): a comment written and found by the API search before the palette opens, then
// deleted through the API and waited for on the open page.
test("an open search palette prints none of a comment its author deletes", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "The palette window" });
  const comment = await createComment(issue.key, {
    body: "The zarquonpalette window closes at noon.",
  });
  await expect
    .poll(async () => JSON.stringify(await search("zarquonpalette")), { timeout: 60_000 })
    .toContain(comment.id);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.getByRole("button", { name: /^search/i })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const box = page.getByRole("combobox", { name: "Search" });
    const dialog = page.getByRole("dialog", { name: "Search" });
    await expect(box).toBeFocused();
    // Both tiers answer before the delete: the keystroke's (`keywords_only`) and the pause's, so
    // no search the typing started can land after the delete and stand in for a refresh.
    const paused = page.waitForResponse(
      (response) =>
        /\/api\/v1\/search(\/results)?\?/.test(response.url()) &&
        response.url().includes("zarquonpalette") &&
        !response.url().includes("keywords_only")
    );
    await box.pressSequentially("zarquonpalette", { delay: 25 });
    await paused;
    await expect(dialog).toContainText("window closes at noon");

    await deleteComment(comment.id);
    await expect(dialog).not.toContainText("window closes at noon", { timeout: 15_000 });

    await page.keyboard.press("Escape");
    await page.keyboard.press("Control+k");
    await expect(box).toBeFocused();
    await box.fill("zarquonpalette");
    // The reopened palette has answered before the absence is read: its "No results" line is
    // drawn from the answer it holds for these words (SearchPalette.tsx).
    await expect(dialog).toContainText('No results for "zarquonpalette"');
    await expect(dialog).not.toContainText("window closes at noon");
  } finally {
    await alice.close();
  }
});
