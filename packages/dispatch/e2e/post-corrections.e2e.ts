import { type FakeSession, setLiveSessions } from "./agents";
import {
  createComment,
  createIssue,
  createMessage,
  createProject,
  deleteComment,
  editMessage,
  getCommentHistory,
  getMessageHistory,
} from "./api";
import { marginCard, openSpec } from "./editor";
import { hasReadingPane, inboxPane } from "./inbox-pane";
import { replyRowId, seedCommentThread, THREAD_SESSION } from "./inbox-threads";
import { expect, type Locator, test } from "./test";
import { asUser } from "./users";

/**
 * What a reader meets on a page once a post's author corrected it: an edited post carries
 * ` · edited` on its byline, and a deleted one carries the line `Deleted by <author> · <time>`
 * (`data-post-deleted`) where its text was, with no part of that text anywhere on the page
 * (`web/src/features/refs/PostBody.tsx`).
 *
 * Three rows over the four pages chk-pages names: the issue's Conversation and the margin of
 * its spec in one row, which read one deleted comment two ways from one seeding; the Inbox
 * thread of a reply to such a comment; and a session's own conversation page. Each row seeds
 * through the API and then reads the page, because the mark is the server's record drawn by
 * the component rather than anything the browser can make on its own, and each pays a Go
 * server, a Postgres, a sign-in and a database reset for that, which is why the two pages that
 * read one comment are read by one row. Every other surface that prints a body is covered by a
 * test of its component in `web/src`, which CI runs.
 *
 * No row here carries the ` @pr` tag, so the suite that answers a pull request schedules none
 * of them and the full lane, which the merge queue and the nightly run, holds all three:
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

    // The margin draws the mark the server projects into the document, which carries the
    // thread's text rather than the comment row: the delete has to clear that too.
    await openSpec(page, issue.key, spec);
    const card = marginCard(page, comment.id);
    await expect(deletedLine(card)).toHaveText(/^Deleted by alice · /);
    await expect(page.getByText("The ledger replays twice at 02:00.")).toHaveCount(0);
    await expect(card.getByText(/^1 reply · last reply/)).toBeVisible();
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
