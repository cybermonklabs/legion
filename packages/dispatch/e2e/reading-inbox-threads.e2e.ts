import {
  createAsk,
  createComment,
  createIssueArtifact,
  requestApproval,
  withOpenedAsk,
} from "./api";
import { askCard, inboxPane, inboxRow, selectRow } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";
import {
  asReadingSession,
  expectReadsReferences,
  firstLineForms,
  type ReadingWorld,
  readingQuestionBlock,
  readingText,
  scopedTo,
  seedReadingWorld,
} from "./reading-rows";
import { expect, own, rowPerson, test } from "./test";
import type { RowLogin } from "./users";

/**
 * The reading list's walk (`e2e/reading-rows.ts`) across the Inbox's questions
 * and threads: a reply under an ask, an approval's question in the reading column, a thread
 * turn's body in the reading column, and a thread row's title. Each is a record a reader meets
 * on `/`, so each must draw every form a stored text names a node by through the one renderer
 * (R6): as its node's current name, the party as `@` and its current name, and none left as the
 * characters written.
 *
 * Three of the four are records a reader reads, where each reference is a link. The fourth, a
 * thread row's title, is the link the row's `o` follows (`ThreadTitle.tsx`), so the references
 * in it draw as their names and link nothing.
 *
 * Every row runs at the `chromium` project's 1280 px, where the Inbox draws its reading column
 * (`e2e/inbox-pane.ts`'s `READING_PANE_WIDTH`), since that column is where two of these surfaces
 * are and where a thread row is the short row whose title is a link. No row carries `@phone`:
 * the `iphone` project's 390 px never draws the column.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

test.use({ signedIn: own("alice") });

/** Where a host thread's two comments are: the root, which is the thread's id and its Reply
 *  row's, and the reply under it. */
interface HostThread {
  readonly root: string;
  readonly reply: string;
}

/**
 * A comment thread on the host that the reader started and the reading session answered, which
 * makes it a Reply row of the reader's Inbox. It is the thread `e2e/inbox-threads.ts`'s
 * `seedCommentThread` writes, kept here for the reply's id as well, which the turn row reads.
 */
async function seedHostThread(
  world: ReadingWorld,
  root: string,
  reply: string,
  login: RowLogin
): Promise<HostThread> {
  const head = await createComment(world.host.key, { body: root }, { login });
  const answer = await createComment(
    world.host.key,
    { body: reply, reply_to: head.id },
    asReadingSession
  );
  return { reply: answer.id, root: head.id };
}

// SLOW(>=2s): the reading world, an ask with its reply, and the Inbox opened on that ask's
// card; measured at 4.3 s on one run in an issue pod on one worker.
test("ask-thread-reply: a reply under an ask draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld({ login: rowPerson("alice") });
  const ask = await createAsk(
    world.host.key,
    { options: [{ label: "Now" }, { label: "Later" }], question: "When should the cache warm?" },
    asReadingSession
  );
  await createComment(
    world.host.key,
    { ask_id: ask.id, body: readingText(world) },
    asReadingSession
  );

  await page.goto("/");
  const card = await askCard(page, ask.id);
  const reply = card.getByTestId(`thread-${ask.id}`).locator("li[data-reply]");
  await expect(reply).toHaveCount(1);
  await expectReadsReferences(reply, world, "link");
});

// SLOW(>=2s): the reading world, a document and its approval request, and the Inbox opened on
// that request's reading column, which reads the document under the question.
test("approval-ask-question: an approval's question in the reading column draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const seeded = await seedReadingWorld({ login: rowPerson("alice") });
  // The approval's question is read against the document it asks about, so that document
  // declares the question a `#id` in the question names, and the world is scoped to it.
  const notes = await createIssueArtifact(
    seeded.host.key,
    {
      content: `# Cache notes\n\nThe cache warms before the queue drains.\n\n${readingQuestionBlock()}\n`,
      name: "cache-notes.md",
    },
    asReadingSession
  );
  const world = await scopedTo(seeded, notes.artifact.id);
  // The server writes the question as its own `Approve …?` and then the summary, so the summary
  // is where the stored text goes.
  const approval = withOpenedAsk(
    await requestApproval(notes.artifact.id, { summary: readingText(world) }, asReadingSession)
  );

  await page.goto("/");
  await selectRow(page, approval.ask.id);
  const question = inboxPane(page).getByTestId("pane-approval-question");
  await expectReadsReferences(question, world, "link");
});

// SLOW(>=2s): the reading world, a comment thread, and the Inbox opened on that thread in the
// reading column; measured at 3.9 s on one run in an issue pod on one worker.
test("inbox-thread-turn: a thread turn's body in the reading column draws every form of a stored reference through the renderer", async ({
  page,
}) => {
  const me = rowPerson("alice");
  const world = await seedReadingWorld({ login: me });
  const thread = await seedHostThread(world, "Where does the cache warm?", readingText(world), me);

  await page.goto("/");
  await selectRow(page, replyRowId(thread.root));
  const turn = inboxPane(page).locator(`[data-inbox-thread-turn="${thread.reply}"]`);
  await expectReadsReferences(turn, world, "link");
});

// SLOW(>=2s): the reading world, a comment thread for each group of forms a first line holds,
// and the Inbox drawn with the threads' rows; measured at 2.8 s on one run in an issue pod on
// one worker.
test("inbox-thread-title: a thread row's title draws every form of a stored reference as a name inside its link", async ({
  page,
}) => {
  const me = rowPerson("alice");
  const world = await seedReadingWorld({ login: me });
  // The title is the first line of the thread root's own text, as the server cuts it, so the
  // stored text is the reader's root: one thread for each group of forms a first line holds
  // whole (`firstLineForms`).
  const groups = firstLineForms(world);
  const roots: string[] = [];
  for (const forms of groups) {
    const text = readingText(world, forms);
    roots.push((await seedHostThread(world, text, "It warms before the drain.", me)).root);
  }

  await page.goto("/");
  for (const [index, forms] of groups.entries()) {
    // The short row's title is the row's one owner link, the one `o` follows; the owner line
    // beside it links the issue without that mark.
    const title = inboxRow(page, replyRowId(roots[index] ?? "")).locator("a[data-inbox-owner]");
    await expect(title).toHaveCount(1);
    await expectReadsReferences(title, world, "inert", forms);
  }
});
