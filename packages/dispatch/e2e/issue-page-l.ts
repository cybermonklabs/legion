import { expect, type Locator, type Page } from "@playwright/test";

import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  getArtifact,
  recordCheckResult,
} from "./api";
import { marginShell } from "./issue-page";
import { focusRow, READER, recorderActor, rowAct } from "./issue-page-g";
import { onScreen } from "./issue-page-i";

/**
 * What the browser rows of the page's writing (the spec file
 * `e2e/issue-page-l.e2e.ts`) read of the new issue page's box at the foot of the screen and of
 * its read mark, and the issues they seed. `e2e/issue-page.ts`, `e2e/issue-page-g.ts`
 * and `e2e/issue-page-i.ts` hold the page's frame, a node's row and the outline; this file
 * holds what those do not (decision 2 of the parts).
 *
 * What the box draws (`IssueComposer.tsx`, `slots/foot.tsx`):
 *
 * - **The box** `section[data-testid="issue-composer"]`, held at the foot of the screen by its
 *   slot's wrapper, `hidden` while it is not drawn (under a view or a document with no target).
 * - **A line** of it, a `form[data-issue-composer]`: the one that writes on the issue, a reply or
 *   a statement's comment, which stays mounted, hidden, while the line that edits a comment is
 *   drawn in its place. So a row reads the line that is drawn with `:visible`. No line answers a
 *   question (`OutlineWriter.answer`).
 * - In a line: the target line `[data-composer-target]` above it while it has a target, which
 *   names the target by its author and its kind and draws none of its words, with its control
 *   named `CONTROL_LABELS.write_on_issue`; the field, a `textarea`; the kinds' chips
 *   `[data-composer-kind]` (Comment, then Question) while it writes on the issue; and Send.
 * - **The read mark** `[data-outline-end]`, 1 px tall, after the outline's last row and moved
 *   down by the box's height, so it comes on screen only once that row is clear of the box.
 */

/** The box at the foot of the screen. */
export function composer(page: Page): Locator {
  return page.getByTestId("issue-composer");
}

/** The line of the box that is drawn: the one that writes on the issue stays mounted, hidden,
 *  while the line edits a comment. */
export function composerForm(page: Page): Locator {
  return composer(page).locator("[data-issue-composer]:visible");
}

/** The field of the line that is drawn, whatever it is set to write. */
export function composerField(page: Page): Locator {
  return composer(page).locator("textarea:visible");
}

/** The target line above the line that is drawn, which names what the line is aimed at by its
 *  author and its kind: the entry a reply answers, the statement a comment is on, the comment an
 *  edit rewrites. */
export function composerTarget(page: Page): Locator {
  return composer(page).locator("[data-composer-target]:visible");
}

/** The chip of one kind the line writes with no target. */
export function composerKind(page: Page, kind: "ask" | "comment"): Locator {
  return composer(page).locator(`[data-composer-kind="${kind}"]`);
}

/** The read mark at the outline's end. */
export function outlineEnd(page: Page): Locator {
  return page.locator("[data-outline-end]");
}

/** Writes `text` in the line at the foot of the outline and sends it with Enter, the line's own
 *  key: a comment on the issue, or the reply, the statement's comment or the edit the line is set
 *  to. The field is empty once the send is out. */
export async function sendFromComposer(page: Page, text: string): Promise<void> {
  const field = composerField(page).first();
  await field.fill(text);
  await field.press("Enter");
  await expect(field).toHaveValue("");
}

/** Replies to node `id` the way a reader does with the pointer: its row's Reply sets the line at
 *  the foot of the outline to it, and the line sends `text`. */
export async function replyFromRow(page: Page, id: string, text: string): Promise<void> {
  const row = await focusRow(page, id);
  await rowAct(row, "reply").click();
  await expect(composerTarget(page), `Reply on ${id} draws no target line`).toBeVisible();
  await sendFromComposer(page, text);
}

/** Sends `words` from the box with the keyboard alone: `r` on node `id`'s row, the words, Enter.
 *  Fails unless `r` puts the caret in the box. */
export async function sendWithKeys(page: Page, id: string, words: string): Promise<void> {
  await focusRow(page, id);
  await page.keyboard.press("r");
  await expect(composerField(page), "r put no caret in the box").toBeFocused();
  await page.keyboard.type(words);
  await page.keyboard.press("Enter");
}

/**
 * Whether `row` is on screen and clear of the box: I's `onScreen` (inside the viewport, under
 * what stays at the top of the screen and above the height the box publishes), and its bottom at
 * or above the top of the box's `section` as it is drawn now. The published height is what the
 * page places a focused row by; this reads the box itself, so a height published late or wrong
 * cannot pass a row the box covers. With the box hidden the viewport's foot bounds the row.
 */
export async function clearOfBox(row: Locator): Promise<boolean> {
  // Every caller reads this inside `expect.poll`, so each answer here is one try of a read the
  // poll repeats until the row stands clear of the box.
  if (!(await onScreen(row))) return false;
  return await row.first().evaluate((element) => {
    const box = document.querySelector<HTMLElement>('[data-testid="issue-composer"]');
    const top =
      box === null || box.closest("[hidden]") !== null
        ? window.innerHeight
        : box.getBoundingClientRect().top;
    return element.getBoundingClientRect().bottom <= top + 1;
  });
}

/** How wide `words` are drawn in `field`'s own font, measured with a canvas, beside the width
 *  the field draws text in: its inner width, less its padding. */
export function wordsInField(
  field: Locator,
  words: string
): Promise<{ readonly room: number; readonly width: number }> {
  return field.evaluate(async (element, text) => {
    const style = getComputedStyle(element);
    const font = `${style.fontStyle} ${style.fontWeight} ${style.fontSize} ${style.fontFamily}`;
    // The width of a font that has not loaded is the fallback's, so the reading waits for the
    // faces the words need. A `local()` fallback face this machine does not have refuses its
    // load, and the field and the canvas both draw with the faces that did load.
    await document.fonts.load(font, text).catch(() => []);
    await document.fonts.ready;
    const context = document.createElement("canvas").getContext("2d");
    if (context === null) throw new Error("the browser draws no 2d canvas to measure words in");
    context.font = font;
    return {
      room:
        element.clientWidth -
        Number.parseFloat(style.paddingLeft) -
        Number.parseFloat(style.paddingRight),
      width: context.measureText(text).width,
    };
  }, words);
}

/** Every place the page holds `words`: each text node that holds them, by the node row it is
 *  in (`null` outside any row), and each field whose value holds them, as `field`. */
export function placesHolding(page: Page, words: string): Promise<(string | null)[]> {
  return page.evaluate((text) => {
    const places: (string | null)[] = [];
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      if (!(node.textContent ?? "").includes(text)) continue;
      const row = node.parentElement?.closest("[data-node-row]") ?? null;
      places.push(row === null ? null : row.getAttribute("data-node-id"));
    }
    for (const field of document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>(
      "input, textarea"
    )) {
      if (field.value.includes(text)) places.push("field");
    }
    return places;
  }, words);
}

/** Where the outline's end stands against the box, in the viewport's pixels: the read mark's top
 *  as it is drawn, moved down by the box's height; the bottom of the row `lastRow` names; the top
 *  of the box's `section`; and the viewport's height. Null where the page draws one of them not. */
export function endPlace(
  page: Page,
  lastRow: string
): Promise<{
  readonly boxTop: number;
  readonly endTop: number;
  readonly lastBottom: number;
  readonly viewport: number;
} | null> {
  return page.evaluate((id) => {
    const end = document.querySelector("[data-outline-end]");
    const box = document.querySelector('[data-testid="issue-composer"]');
    const row = document.querySelector(`[data-issue-page] [data-node-row][data-node-id="${id}"]`);
    if (end === null || box === null || row === null) return null;
    return {
      boxTop: box.getBoundingClientRect().top,
      endTop: end.getBoundingClientRect().top,
      lastBottom: row.getBoundingClientRect().bottom,
      viewport: window.innerHeight,
    };
  }, lastRow);
}

/** Scrolls the page so the outline's last row, `lastRow`, ends in the middle of the box at the
 *  foot: under the box, where the reader cannot read it. The place is the row's own, never the
 *  read mark's: where the mark is drawn against the box (`slots/foot.tsx`) is what a row reads
 *  after this, so it is no measure of where to scroll. */
export async function scrollEndUnderBox(page: Page, lastRow: string): Promise<void> {
  await page.evaluate((id) => {
    const last = document.querySelector(`[data-issue-page] [data-node-row][data-node-id="${id}"]`);
    const box = document.querySelector('[data-testid="issue-composer"]');
    if (last === null || box === null) throw new Error("the page draws no last row or no box");
    const boxTop = box.getBoundingClientRect().top;
    const height = window.innerHeight - boxTop;
    window.scrollBy({
      behavior: "instant",
      top: last.getBoundingClientRect().bottom - (boxTop + height / 2),
    });
  }, lastRow);
}

/** The entry of the panel (Details) whose words are `words`: a turn of the issue's history, whose
 *  Reply aims the box at it, or the reader's own comment, whose Edit opens its words in the box. */
export function panelEntry(page: Page, words: string): Locator {
  return marginShell(page).getByRole("listitem").filter({ hasText: words });
}

// ---------------------------------------------------------------------------------------------
// The seeds.
// ---------------------------------------------------------------------------------------------

/** The project every seed of this file writes its issues in. */
const PROJECT = "BOX";

/** The second login every write of the read mark's seeds is made as, so each is unread for the
 *  reader the rows sign in as (alice). */
const WRITER = "bob";

/** An agent of the seeds: it asks the question `seedFortyComments` writes, and writes the
 *  comment of `seedHistoryTurns` that no person wrote in. */
const AGENT = {
  actor: { id: "e2e-box-asker", kind: "session" as const },
  as: "agent" as const,
};

/** Writes `count` one-line comments on issue `key` as `WRITER`, the `at`th of them reading
 *  "Comment <at> of <of>, written by bob.", and hands back their ids, oldest first. */
async function writeComments(key: string, count: number, of: string): Promise<string[]> {
  const comments: string[] = [];
  for (let at = 1; at <= count; at += 1) {
    const comment = await createComment(
      key,
      { body: `Comment ${at} of ${of}, written by bob.` },
      { login: WRITER }
    );
    comments.push(comment.id);
  }
  return comments;
}

export interface FortyCommentsSeed {
  /** The forty comments' ids, oldest first: the last is the outline's last row. */
  readonly comments: readonly string[];
  readonly key: string;
  /** The open question asked before the comments, the outline's first row, on which `a` leaves
   *  the box as it is at rest. */
  readonly question: string;
}

/**
 * One issue whose outline runs well past the screen at every width: an open question with two
 * options, then forty one-line comments, all written by `WRITER`, so the outline's end starts
 * off screen and the issue is unread for the reader. The question is the first row, so the page
 * opens at its top whether or not it waits on the reader, and the issue holds an open question
 * for `a`.
 */
export async function seedFortyComments(): Promise<FortyCommentsSeed> {
  await createProject({ key: PROJECT, name: "The box at the foot" });
  const issue = await createIssue(
    { force: true, project: PROJECT, title: "Forty comments run past the screen" },
    { login: WRITER }
  );
  const question = await createAsk(
    issue.key,
    {
      options: [{ label: "Left" }, { label: "Right" }],
      question: "Which side does the box keep its controls on?",
    },
    AGENT
  );
  const comments = await writeComments(issue.key, 40, "forty");
  return { comments, key: issue.key, question: question.id };
}

/** One more issue of the project `seedFortyComments` made, with `spec` as its spec where one is
 *  given and one comment by `WRITER`, so it is unread for the reader. Its outline is one row,
 *  shorter than the screen. */
export async function seedUnreadIssue(title: string, spec?: string): Promise<string> {
  const issue = await createIssue(
    { force: true, project: PROJECT, title, ...(spec === undefined ? {} : { spec }) },
    { login: WRITER }
  );
  await createComment(
    issue.key,
    { body: "One comment the reader has not read." },
    { login: WRITER }
  );
  return issue.key;
}

/** The block ids of the statements `seedStatements` writes. */
export const STATEMENT_BLOCKS = {
  /** One paragraph that cites another issue by its address. */
  cites: "r-cites",
  /** A statement whose words the spec also holds in a paragraph of its own. */
  twice: "r-twice",
  /** Two paragraphs. */
  two: "r-two",
} as const;

/** The words of those statements. */
export const STATEMENT_WORDS = {
  /** The words of `cites` before the address it cites. */
  citesLead: "A comment on this statement goes in the margin beside it",
  /** The first paragraph of `two`, which is the statement's first line. */
  first: "The first paragraph of this statement says what it holds.",
  /** The second paragraph of `two`. */
  second: "Its second paragraph says why it holds it.",
  /** The words of `twice`, which the spec holds a second time. */
  twice: "The draft stays in the box until it is sent.",
} as const;

/** The title of the issue `cites` cites by its address. */
export const CITED_TITLE = "The issue a statement cites";

/** The whole words of `cites`, as the spec holds them: the issue `cited` is named by its
 *  address, which the spec holds as text. */
export function citesWords(cited: string): string {
  return `${STATEMENT_WORDS.citesLead}, as dispatch://${cited} asks.`;
}

export interface StatementsSeed {
  /** The issue the cited statement names by its address. */
  readonly cited: string;
  readonly key: string;
  readonly specArtifactId: string;
  /** The spec's slug, which a statement's node id starts with (`blockNode`). */
  readonly specSlug: string;
}

/**
 * One issue whose spec holds three statements a comment can be written on from the box: one
 * paragraph citing another issue by its `dispatch://` address, so the words the box
 * quotes are read from a text that holds a reference; two paragraphs, so the quote spans two
 * blocks and the server anchors the comment to the statement itself; and one whose words the spec
 * holds again in a plain paragraph after it, so a quote with no occurrence is ambiguous and the
 * server refuses it.
 */
export async function seedStatements(): Promise<StatementsSeed> {
  await createProject({ key: PROJECT, name: "The box at the foot" });
  const cited = await createIssue({
    force: true,
    project: PROJECT,
    title: CITED_TITLE,
  });
  const spec = [
    `:::requirement{#${STATEMENT_BLOCKS.cites}}`,
    citesWords(cited.key),
    ":::",
    "",
    `:::requirement{#${STATEMENT_BLOCKS.two}}`,
    STATEMENT_WORDS.first,
    "",
    STATEMENT_WORDS.second,
    ":::",
    "",
    `:::requirement{#${STATEMENT_BLOCKS.twice}}`,
    STATEMENT_WORDS.twice,
    ":::",
    "",
    STATEMENT_WORDS.twice,
    "",
  ].join("\n");
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec,
    title: "A comment on a statement goes beside it",
  });
  const specArtifactId = issue.primary_artifact_id ?? "";
  return {
    cited: cited.key,
    key: issue.key,
    specArtifactId,
    specSlug: (await getArtifact(specArtifactId)).slug,
  };
}

/** How many top-level comments `seedPastFifty` writes: more than the 50 children of a node the
 *  outline draws before its control for the rest (root R40). */
const PAST_FIFTY = 55;

export interface PastFiftySeed {
  /** The comments' ids, oldest first: the last stands past the first 50 top-level rows. */
  readonly comments: readonly string[];
  readonly key: string;
}

/** One issue of more top-level rows than the outline draws before its control for the rest:
 *  `PAST_FIFTY` one-line comments, all written by `WRITER`, so the issue is unread for the
 *  reader, and no question, so the newest comment is the conversation's current point. */
export async function seedPastFifty(): Promise<PastFiftySeed> {
  await createProject({ key: PROJECT, name: "The box at the foot" });
  const issue = await createIssue(
    { force: true, project: PROJECT, title: "More comments than the outline draws at once" },
    { login: WRITER }
  );
  const comments = await writeComments(issue.key, PAST_FIFTY, `${PAST_FIFTY}`);
  return { comments, key: issue.key };
}

export interface ResultAfterActSeed {
  /** The comment that is the newest act on the issue, written before the result. */
  readonly comment: string;
  readonly key: string;
}

/** One issue whose newest act is a comment by `WRITER`, with a result of its one check recorded
 *  after that comment: a check's result is no act of the conversation. */
export async function seedResultAfterAct(): Promise<ResultAfterActSeed> {
  await createProject({ key: PROJECT, name: "The box at the foot" });
  const issue = await createIssue(
    {
      force: true,
      project: PROJECT,
      spec: ":::requirement{#r-one}\nThe box stays at the foot of the screen.\n:::\n",
      title: "A result recorded after the newest act",
    },
    { login: WRITER }
  );
  await createIssueArtifact(issue.key, {
    content:
      `:::check{#c1 serves="dispatch://${issue.key}/spec#b-r-one" surface="browser" ` +
      `run="bun run e2e" when="before_merge"}\nThe box is at the foot.\n:::\n`,
    name: "verification.md",
  });
  const comment = await createComment(
    issue.key,
    { body: "The newest act on the issue, written before the check's result." },
    { login: WRITER }
  );
  await recordCheckResult(
    issue.key,
    {
      check: "c1",
      commit: "a".repeat(40),
      negative_control: "Took the box off the foot and the run failed.",
      observed: "The box is at the foot.",
      role: "tester",
      verdict: "pass",
    },
    { actor: recorderActor, as: "agent" }
  );
  return { comment: comment.id, key: issue.key };
}

/** The Legion daemon, which writes on an issue as a session of its own. */
const DAEMON = {
  actor: { id: "legion-daemon", kind: "session" as const, origin: { session_title: "Legion" } },
  as: "agent" as const,
};

/** One entry `seedHistoryTurns` writes: its id and its words. */
export interface SeededEntry {
  readonly id: string;
  readonly words: string;
}

export interface HistoryTurnsSeed {
  /** A comment an agent wrote on the issue, tied to no text, in a thread no person wrote in,
   *  which the panel's history holds and the outline draws only once a person writes in it. */
  readonly agent: SeededEntry;
  /** The Legion daemon's message, the first turn of a thread it began, which the panel's
   *  history holds. */
  readonly daemon: SeededEntry;
  readonly key: string;
  /** A comment of the reader's own, which its Edit opens in the box. */
  readonly own: SeededEntry;
}

/** One issue holding the entries the panel's Reply and Edit act on: the daemon's message, an
 *  agent's comment no person wrote in, and a comment of the reader's own. */
export async function seedHistoryTurns(): Promise<HistoryTurnsSeed> {
  await createProject({ key: PROJECT, name: "The box at the foot" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    title: "The panel's history holds turns a reply can answer",
  });
  const daemonWords = "The planner's session was replaced; its claim moves to the new one.";
  const daemon = await createMessage(issue.key, { body: daemonWords }, DAEMON);
  const agentWords = "Status: the plan is drafted and waits on its review.";
  const agent = await createComment(issue.key, { body: agentWords }, AGENT);
  const ownWords = "A comment of the reader's own, which its writer alone edits.";
  const own = await createComment(issue.key, { body: ownWords }, { login: READER });
  return {
    agent: { id: agent.id, words: agentWords },
    daemon: { id: daemon.id, words: daemonWords },
    key: issue.key,
    own: { id: own.id, words: ownWords },
  };
}
