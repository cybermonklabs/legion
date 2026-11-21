import type { Issue } from "../web/src/api/types";
import { REF_PREVIEW_OPEN_DELAY_MS } from "../web/src/features/refs/ref-preview-timing";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  dashboardOrigin,
  patchIssue,
} from "./api";
import { documentEditor } from "./editor";
import { nextFrames } from "./frames";
import { inboxRow } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";
import { nodeRow, openIssue, rowWords } from "./issue-page-g";
import { withIssuePage } from "./issue-page-option";
import { RENDERED_BODY, referenceAgent } from "./reference-surfaces";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * How an issue reference reads (dispatch://INFRA-4009): a bare key, a `dispatch://` address and
 * a dashboard address of an issue's page or of a tab of it draw, once the issue's summary is
 * read, as the issue's status mark and its key on one line, with no title; the title is in the
 * peek that opens on the reference and in its accessible name, `KEY, <status words>, <title>`.
 * A link whose words its author wrote draws the mark and those words in a rendered body, and its
 * words alone in the document editor and in a row of the new issue page. The mark is one of
 * three shapes - a tick for done, a half-filled circle while the work moves, an empty circle for
 * every other status - named by the status's words, and it follows the status with no reload,
 * in a rendered comment, in the document editor and in a row of the new issue page. Plain text
 * writes the key alone.
 *
 * Each row writes its records through the API before the page loads, so the page reads what the
 * server stores and answers - the issue summary, the batched names read and the event that a
 * status or a title changed - which a recording would freeze.
 */

/** The statuses these rows put an issue in, and how its mark draws each one: its shape, its
 *  colour and its accessible name, the status's words (`statusText`). */
const MARKS = {
  done: { glyph: "✓", tone: "text-page-passed", words: "Done" },
  in_progress: { glyph: "◐", tone: "text-page-in-progress", words: "In progress" },
  todo: { glyph: "○", tone: "text-page-not-run", words: "Todo" },
} as const;

type Status = keyof typeof MARKS;

/** The words a link whose author worded it says, in every row. */
const WORDS = "the warm one";

/** The accessible name of a bare reference to `key`: its key, its status's words and its
 *  title. */
function bareName(key: string, status: Status, title: string): string {
  return `${key}, ${MARKS[status].words}, ${title}`;
}

/** An issue of CORE titled `title`, put in `status`. */
async function issueIn(status: Status, title: string): Promise<Issue> {
  const issue = await createIssue({ project: "CORE", title });
  return patchIssue(issue.key, { status });
}

/** Holds `reference` to the one mark it draws before its words: the shape, the colour and the
 *  name of `status`. */
async function expectMark(reference: Locator, status: Status): Promise<void> {
  const mark = reference.locator("svg[data-status-glyph]");
  await expect(mark, `the reference draws no mark of ${status}`).toHaveCount(1);
  await expect(mark).toHaveAttribute("data-status-glyph", MARKS[status].glyph);
  await expect(mark).toHaveAttribute("role", "img");
  await expect(mark).toHaveAttribute("aria-label", MARKS[status].words);
  await expect(mark).toHaveClass(new RegExp(`(^| )${MARKS[status].tone}( |$)`, "u"));
}

/** Holds the bare reference in `scope` to `issue` in `status`: one element named by its key, its
 *  status's words and its title, drawing its mark and its key, and no title. */
async function expectBare(
  scope: Locator,
  issue: Pick<Issue, "key" | "title">,
  status: Status
): Promise<Locator> {
  const reference = scope.getByRole("link", {
    exact: true,
    name: bareName(issue.key, status, issue.title),
  });
  await expect(reference, `${issue.key} is not named by its key, status and title`).toHaveCount(1);
  await expectMark(reference, status);
  await expect(reference).toHaveText(issue.key);
  return reference;
}

/** Holds the worded link in a row of the new issue page to its author's words alone: a row draws
 *  no mark on a link whose words its author wrote, so the link's name is those words. */
async function expectRowWordedLink(row: Locator): Promise<void> {
  const worded = row.getByRole("link", { exact: true, name: WORDS });
  await expect(worded, "the row's worded link is not named by its words alone").toHaveCount(1);
  await expect(worded.locator("svg[data-status-glyph]")).toHaveCount(0);
  await expect(worded).toHaveText(WORDS);
}

/** The comment's Conversation card, expanded: a collapsed card draws its one-line preview, and
 *  the comment itself, with live links, is what the expanded card holds. */
async function expandedComment(page: Page, id: string): Promise<Locator> {
  const card = page.locator(`[data-turn="comment:${id}"]`);
  await card.getByRole("button", { name: "Expand thread" }).click();
  await expect(card.getByRole("button", { name: "Collapse thread" })).toBeVisible();
  return card.locator(RENDERED_BODY).first();
}

/** Marks the page's document, so a check after a write can tell the page was not loaded again:
 *  a reload is a new document, which carries no mark. */
async function markDocument(page: Page): Promise<void> {
  await page.evaluate(() => {
    document.documentElement.dataset.referenceStatus = "loaded once";
  });
}

async function expectNotReloaded(page: Page): Promise<void> {
  await expect(page.locator("html")).toHaveAttribute("data-reference-status", "loaded once");
}

/** The document editor's drawn title of the bare reference to `key`. */
function editorTitle(page: Page, key: string): Locator {
  return documentEditor(page).locator(
    `a.dispatch-reference[data-dispatch-href="dispatch://${key}"]`
  );
}

// c-key-and-mark (R1, R3, R4).
// SLOW(>=2s): five issues, an ask and two comments seeded, and the Conversation drawn twice over:
// with every issue summary held back, and again once the summaries answer.
test("an issue reference draws the words written until its summary answers, then its status mark and its key, its title in its name, and a worded link its mark and its words", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const done = await issueIn("done", "Ship the cache");
  const moving = await issueIn("in_progress", "Warm the cache");
  const idle = await issueIn("todo", "Drain the queue");
  const host = await createIssue({ project: "CORE", title: "Status host" });
  const question = "Which cache warms first?";
  const ask = await createAsk(
    host.key,
    { options: [{ label: "Hot" }, { label: "Cold" }], question },
    referenceAgent
  );
  const cited = await createComment(
    host.key,
    { body: "The queue drains at noon." },
    referenceAgent
  );
  const missing = "CORE-99";
  const written = {
    address: `dispatch://${moving.key}`,
    ask: `dispatch://${host.key}/ask/${ask.id}`,
    comment: `dispatch://${host.key}/comment/${cited.id}`,
    page: `${dashboardOrigin}/issues/${idle.key}/conversation`,
  };
  const comment = await createComment(
    host.key,
    {
      body: [
        `Bare ${done.key}, address ${written.address}, page ${written.page},`,
        `worded [${WORDS}](${written.address}), ask ${written.ask},`,
        `comment ${written.comment} and missing ${missing}.`,
      ].join(" "),
    },
    referenceAgent
  );

  // Every issue summary the page asks for is held until the row lets it go.
  const held = new Set<string>();
  const { promise: released, resolve: release } = Promise.withResolvers<void>();
  await page.route("**/api/v1/issues/*/summary", async (route) => {
    held.add(new URL(route.request().url()).pathname);
    await released;
    await route.continue();
  });

  await page.goto(`/issues/${host.key}/conversation`);
  const body = await expandedComment(page, comment.id);
  await expect
    .poll(() => [...held].sort(), { message: "the page asked for no summary of the cited issues" })
    .toEqual(
      expect.arrayContaining([done, moving, idle].map(({ key }) => `/api/v1/issues/${key}/summary`))
    );
  // The ask's read answers while the summaries are held, so the body has drawn what it can.
  await expect(body.getByRole("link", { exact: true, name: question })).toHaveCount(1);
  // Until its summary answers, each issue reference stands as the words written, with no mark.
  for (const words of [done.key, written.address, written.page, WORDS]) {
    await expect(body).toContainText(words);
  }
  await expect(body.locator("svg[data-status-glyph]")).toHaveCount(0);
  for (const { title } of [done, moving, idle]) {
    await expect(body).not.toContainText(title);
  }

  release();
  // A bare key, an address and a dashboard address of a tab of the page: the mark of the
  // issue's group and its key, with no title, which is in the name.
  await expectBare(body, done, "done");
  await expectBare(body, moving, "in_progress");
  await expectBare(body, idle, "todo");
  for (const { title } of [done, moving, idle]) {
    await expect(body).not.toContainText(title);
  }
  // The worded link: the mark and the author's words, not the key, named by both.
  const worded = body.getByRole("link", {
    exact: true,
    name: `${MARKS.in_progress.words} ${WORDS}`,
  });
  await expect(worded).toHaveCount(1);
  await expectMark(worded, "in_progress");
  await expect(worded).toHaveText(WORDS);
  await expect(worded).not.toHaveAttribute("aria-label");
  // An ask and a comment draw their headlines, with no mark.
  for (const name of [question, "The queue drains at noon."]) {
    const other = body.getByRole("link", { exact: true, name });
    await expect(other).toHaveCount(1);
    await expect(other.locator("svg[data-status-glyph]")).toHaveCount(0);
  }
  // A key no issue holds stays the words written: no link and no mark.
  await expect(body).toContainText(`missing ${missing}.`);
  await expect(body.getByRole("link", { name: new RegExp(missing, "u") })).toHaveCount(0);
  // Four marks: the three bare references' and the worded link's.
  await expect(body.locator("svg[data-status-glyph]")).toHaveCount(4);
});

/** One issue, CORE-2 of the rows below, cited by a host's spec and by a comment of the reader's
 *  on it, with a second cited issue beside it whose status no write moves. */
interface Cited {
  readonly cited: Issue;
  readonly other: Issue;
  readonly host: Issue;
  /** The reader's comment on the host, which cites both and words a link to the first. */
  readonly comment: string;
}

async function seedCited(status: Status = "todo"): Promise<Cited> {
  await createProject({ key: "CORE", name: "Core" });
  const cited = await issueIn(status, "Warm the cache");
  const other = await issueIn("todo", "Drain the queue");
  const host = await createIssue({
    project: "CORE",
    spec: `The plan waits on ${cited.key} and ${other.key}.`,
    title: "Status host",
  });
  // The reader's own comment: a thread no person wrote in draws no row on the new issue page.
  const comment = await createComment(host.key, {
    body: `Before the rollout, ${cited.key} waits on ${other.key}, as [${WORDS}](dispatch://${cited.key}) says.`,
  });
  return { cited, comment: comment.id, host, other };
}

/** The three surfaces that draw a bare issue reference, open side by side on the host: its
 *  comment in the Conversation, its spec in the document editor, and the comment's row on the
 *  new issue page, in a context of its own that turns that page on. */
interface Surfaces {
  readonly comment: Locator;
  readonly editor: Page;
  readonly row: Locator;
  readonly pages: readonly Page[];
}

async function openSurfaces(page: Page, rows: Page, seeded: Cited): Promise<Surfaces> {
  await page.goto(`/issues/${seeded.host.key}/conversation`);
  const comment = await expandedComment(page, seeded.comment);
  const editor = await page.context().newPage();
  await editor.goto(`/issues/${seeded.host.key}/spec`);
  await withIssuePage(rows.context(), "alice");
  await openIssue(rows, seeded.host.key);
  const row = rowWords(nodeRow(rows, seeded.comment).first());
  return { comment, editor, pages: [page, editor, rows], row };
}

/** Holds each surface's reference to `issue` to the mark of `status` and the name it gives. */
async function expectEverywhere(
  surfaces: Surfaces,
  issue: Pick<Issue, "key" | "title">,
  status: Status
): Promise<void> {
  await expectBare(surfaces.comment, issue, status);
  const title = editorTitle(surfaces.editor, issue.key);
  await expect(title).toHaveCount(1);
  await expectMark(title, status);
  await expect(title).toHaveText(issue.key);
  await expect(title).toHaveAttribute("aria-label", bareName(issue.key, status, issue.title));
  await expectBare(surfaces.row, issue, status);
}

// c-mark-follows-status (R2).
// SLOW(>=2s): three pages open side by side - the Conversation, the spec's live editor and the
// new issue page - and two status writes that each reach all three through the event stream.
test("a cited issue moved to in progress and then closed moves its mark in a comment, the editor and a row with no reload, and no other mark", async ({
  browser,
  page,
}) => {
  const seeded = await seedCited();
  // An issue the page does not cite, whose status moves with the cited one's.
  const uncited = await issueIn("todo", "Rotate the keys");
  const rowsContext = await asUser(browser, "alice");
  try {
    const surfaces = await openSurfaces(page, await rowsContext.newPage(), seeded);
    await expectEverywhere(surfaces, seeded.cited, "todo");
    await expectEverywhere(surfaces, seeded.other, "todo");
    for (const shown of surfaces.pages) await markDocument(shown);

    for (const status of ["in_progress", "done"] as const) {
      // The uncited issue's write goes first, so the cited one's mark moving says its event was
      // handled too.
      await patchIssue(uncited.key, { status });
      await patchIssue(seeded.cited.key, { status });
      await expectEverywhere(surfaces, seeded.cited, status);
      // Neither the other cited issue's mark nor any other moved: the comment draws the two bare
      // references' marks and the worded link's, the editor the two bare references' alone.
      await expectEverywhere(surfaces, seeded.other, "todo");
      await expect(surfaces.comment.locator("svg[data-status-glyph]")).toHaveCount(3);
      await expect(documentEditor(surfaces.editor).locator("svg[data-status-glyph]")).toHaveCount(
        2
      );
    }
    for (const shown of surfaces.pages) await expectNotReloaded(shown);
  } finally {
    await rowsContext.close();
  }
});

/** The peek that opens on a reference (`RefPreview`): one card, wherever the page draws it. */
function peek(page: Page): Locator {
  return page.locator("#ref-preview");
}

// c-peek (R3).
// SLOW(>=2s): the Conversation, the spec's live editor and the new issue page, each loaded once,
// and five peeks each waited open past their open delay.
test("a reference's peek shows the title and the status on hover and keyboard focus in a comment, on hover in the editor and on keyboard focus alone in the new issue page's column, and a row names the title", async ({
  browser,
  page,
}) => {
  const seeded = await seedCited("in_progress");
  const { cited } = seeded;
  const rowsContext = await asUser(browser, "alice");
  try {
    const rows = await rowsContext.newPage();
    // The open delay a hover would wait out is fired on this clock rather than waited for.
    await rows.clock.install();
    const surfaces = await openSurfaces(page, rows, seeded);

    const reference = await expectBare(surfaces.comment, cited, "in_progress");
    await reference.hover();
    await expect(peek(page)).toBeVisible();
    await expect(peek(page)).toContainText(cited.title);
    await expect(peek(page)).toContainText(MARKS.in_progress.words);
    await expect(reference).toHaveAttribute("aria-describedby", "ref-preview");
    await page.keyboard.press("Escape");
    await expect(peek(page)).toHaveCount(0);

    // Keyboard focus opens the same peek.
    await page.mouse.move(0, 0);
    await reference.focus();
    await expect(peek(page)).toBeVisible();
    await expect(peek(page)).toContainText(cited.title);
    await expect(peek(page)).toContainText(MARKS.in_progress.words);
    await reference.blur();
    await expect(peek(page)).toHaveCount(0);

    // A worded link's peek shows the issue's title, not the author's words.
    const worded = surfaces.comment.getByRole("link", {
      exact: true,
      name: `${MARKS.in_progress.words} ${WORDS}`,
    });
    await worded.hover();
    await expect(peek(page)).toBeVisible();
    await expect(peek(page)).toContainText(cited.title);
    await expect(peek(page)).not.toContainText(WORDS);
    await page.mouse.move(0, 0);
    await expect(peek(page)).toHaveCount(0);

    // The editor's drawn title opens it on hover.
    const title = editorTitle(surfaces.editor, cited.key);
    await expect(title).toHaveAttribute(
      "aria-label",
      bareName(cited.key, "in_progress", cited.title)
    );
    await title.hover();
    await expect(peek(surfaces.editor)).toBeVisible();
    await expect(peek(surfaces.editor)).toContainText(cited.title);
    await expect(peek(surfaces.editor)).toContainText(MARKS.in_progress.words);

    // A row opens no peek, so its reference's name holds the title.
    await expectBare(surfaces.row, cited, "in_progress");
    await expectRowWordedLink(surfaces.row);

    // In the new issue page's column a hover opens no peek, and keyboard focus opens it, on a
    // link the renderer draws in a row's words: the worded one, whose peek is its issue's.
    const inColumn = rows
      .locator("main[data-no-hover-preview]")
      .locator(`[data-node-row][data-node-id="${seeded.comment}"] a[data-dispatch-ref]`, {
        hasText: WORDS,
      });
    await expect(inColumn).toHaveCount(1);
    await inColumn.hover();
    await rows.clock.runFor(REF_PREVIEW_OPEN_DELAY_MS * 2);
    await nextFrames(rows);
    await expect(peek(rows), "a hover in the column opened a peek").toHaveCount(0);
    await inColumn.focus();
    await expect(peek(rows)).toBeVisible();
    await expect(peek(rows)).toContainText(cited.title);
    await expect(peek(rows)).toContainText(MARKS.in_progress.words);
  } finally {
    await rowsContext.close();
  }
});

// c-surfaces (R6, R7).
// SLOW(>=2s): four pages open side by side - the Conversation, the spec's live editor, the new
// issue page and the Inbox - and a rename that reaches each through the event stream.
test("a bare key draws its mark and its key in a comment, the editor, a row and an Inbox line, a thread's full title writes the key alone, and a rename moves only the peek and the name", async ({
  browser,
  page,
}) => {
  const seeded = await seedCited();
  const { cited } = seeded;
  // A thread the reader started on the host and a session answered: a Reply row of her Inbox,
  // titled by the first line of her root.
  const subject = `Where does ${cited.key} go, after [${WORDS}](dispatch://${cited.key})?`;
  const root = await createComment(seeded.host.key, { body: subject });
  await createComment(
    seeded.host.key,
    { body: "It warms before the drain.", reply_to: root.id },
    referenceAgent
  );
  const rowsContext = await asUser(browser, "alice");
  try {
    const surfaces = await openSurfaces(page, await rowsContext.newPage(), seeded);
    await expectEverywhere(surfaces, cited, "todo");
    await expectRowWordedLink(surfaces.row);

    const inbox = await page.context().newPage();
    await inbox.goto("/");
    const line = inboxRow(inbox, replyRowId(root.id)).locator("a[data-inbox-owner]");
    await expect(line).toHaveCount(1);
    // The line renders the thread's markup, so its reference draws the mark and the key, inside
    // the one link the row's `o` follows, named as everywhere else.
    const reference = line.locator(`[data-dispatch-ref="dispatch://${cited.key}"]`).first();
    await expectMark(reference, "todo");
    await expect(reference).toHaveText(cited.key);
    await expect(reference).toHaveAttribute("aria-label", bareName(cited.key, "todo", cited.title));
    // The thread's full title is plain text: the key alone, with no mark and no title.
    const fullTitle = `Where does ${cited.key} go, after ${WORDS}?`;
    await expect(line.locator("[data-markdown-preview]")).toHaveAttribute("title", fullTitle);

    const pages = [...surfaces.pages, inbox];
    for (const shown of pages) await markDocument(shown);
    const renamed = { key: cited.key, title: "Warm the cache first" };
    await patchIssue(cited.key, { title: renamed.title });
    // Each surface keeps the mark and the key, and its name takes the new title.
    await expectEverywhere(surfaces, renamed, "todo");
    await expect(reference).toHaveAttribute(
      "aria-label",
      bareName(cited.key, "todo", renamed.title)
    );
    await expect(reference).toHaveText(cited.key);
    await expect(line.locator("[data-markdown-preview]")).toHaveAttribute("title", fullTitle);
    // A worded link keeps its words: after the mark in a rendered comment, alone in a row.
    await expect(
      surfaces.comment.getByRole("link", { exact: true, name: `${MARKS.todo.words} ${WORDS}` })
    ).toHaveCount(1);
    await expectRowWordedLink(surfaces.row);
    // The peek shows the new title.
    await (await expectBare(surfaces.comment, renamed, "todo")).hover();
    await expect(peek(page)).toContainText(renamed.title);
    for (const shown of pages) await expectNotReloaded(shown);
  } finally {
    await rowsContext.close();
  }
});
