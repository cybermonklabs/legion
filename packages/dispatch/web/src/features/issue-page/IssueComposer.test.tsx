import { afterEach, beforeAll, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { dispatchWritingBox } from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: a paste of a file into the box, which user-event's paste cannot carry
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";

import { jump, onRealTurns, realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerRequests } from "../../__tests__/network";
import { plannerAgent, searchResponse } from "../../__tests__/search-fixture";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api } from "../../api/client";
import type { Actor, CommentWithTrees, IssueAskRow, SearchResult } from "../../api/types";
import {
  pageFailedText,
  pageMutedText,
  pagePathRing,
  pagePathRingFolded,
  pagePointRing,
} from "../../theme/classes";
import type { UploadResult } from "../artifacts/ArtifactUpload";
import { heldSends } from "../conversation/held-sends";
import { KEYED_SEND_LATE_TEXT } from "../conversation/send-request";
import { unsentDraftsOf } from "../conversation/unsent-drafts";
import { composerNameOf } from "../conversation/useKeptDrafts";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { nodeRouteFromHref, shortForm } from "../refs/routes";
import { SEND_DEADLINE_MS } from "../writes/keyed-write";
import {
  ALICE,
  answerBoxReads,
  askNode,
  askRecord,
  BOB,
  BOBS_COMMENT,
  type BoxPage,
  type BoxReads,
  commentNode,
  commentRecord,
  composerBox,
  drawBoxPage,
  goneDrafts,
  KEPT_FOR_ISSUE,
  KEPT_FOR_STATEMENT,
  KEY,
  keptLines,
  keptReplyLine,
  messageNode,
  node,
  outline,
  outlineRewritten,
  outlineWithout,
  type Place,
  READER,
  replyingTo,
  rowOf,
  SPEC_SOURCE,
  STATEMENT_QUOTE,
  STATEMENT_REWRITTEN,
  statementNode,
  targetNamed,
  targetWords,
  writeField,
} from "./composer-fixture";
import { FOOT_HEIGHT_PROPERTY } from "./identity-block";
import type { TreeNode } from "./issue-tree";
import * as roomTarget from "./model-rows/room-target";
import type { OutlineWriter } from "./outline-writer";
import { ANSWER_LINES, AUTHOR_KIND_WORDS, CONTROL_LABELS } from "./page-words";

/**
 * The new issue page's one box to write in, drawn as
 * the page draws it: under the page's writer and its words (`slots/providers.tsx`), at the foot of
 * the outline the body slot draws (`IssueTree`), or of a view's rows where a view is on. Each
 * case presses what a reader presses - a row's `r` and `a`, the box's own controls and keys - and
 * reads what the box draws and what it sends. Every read and write a case makes is answered by the
 * case (`packages/dispatch/AGENTS.md`): the reads below from `reads`, the writes by each case.
 */

/** When the reader answered ask-2, the answer a change of it is measured against. */
const ANSWERED_AT = "2026-11-06T00:00:00Z";

/** A mention of the session the registry lists as Planner, which a plain line reads by that
 *  name (`plannerAgent`). */
const PLANNER_MENTION = "[@Old](dispatch://agent/planner-session)";

/** The session the registry lists as Planner (`plannerAgent`), as the author of a comment. */
const PLANNER: Actor = { id: "planner-session", kind: "session" };

/** The mention the box writes for the planner when the reader replies to its comment: the same
 *  address, worded as the page names an author, by the title the registry lists. The reference
 *  picker writes the same link for a pick of the planner. */
const PLANNER_SEED = PLANNER_MENTION.replace("[@Old]", "[@Planner]");

/** The draft a Reply on the planner's comment starts the box with: its mention of the planner,
 *  and the one space after it that a seed ends with (`replyPrefill`), so the reader's words
 *  typed at once follow that space. */
const PLANNER_PREFILL = `${PLANNER_SEED} `;

/** The reads the box and the writer draw from: each case's own (`composer-fixture.tsx`). */
let reads: BoxReads;

/** ask-2 as the reads hold it once `user` has answered it at `at` with `text`. */
function answeredAsk(at: string, text: string, user = READER): IssueAskRow {
  return askRecord("ask-2", { answer: { at, selected: ["No"], text, user }, state: "answered" });
}

/** The page signed in as the reader (`drawBoxPage`), every read the writer and the box make
 *  answered from `reads` before it mounts. A page drawn with an earlier page's `client` is that
 *  page mounted again in the same tab, its held sends with it. */
function renderPage(
  place: Place = {},
  root: TreeNode = outline(),
  client = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  })
) {
  client.setQueryData(["whoami"], { kind: "user", login: READER });
  return { client, ...drawBoxPage(client, reads, place, root) };
}

/** Holds the page's frames: each `requestAnimationFrame` callback waits until `run`, which runs
 *  those held so far and the ones they ask for, so a frame a call schedules can run inside `act`
 *  once React has drawn what the call changed. `release` gives the page its own frames back. */
function holdFrames(): { readonly release: () => void; readonly run: () => void } {
  const real = window.requestAnimationFrame;
  let held: FrameRequestCallback[] = [];
  window.requestAnimationFrame = (callback) => {
    held.push(callback);
    return held.length;
  };
  return {
    release: () => {
      window.requestAnimationFrame = real;
    },
    run: () => {
      for (let round = 0; round < 30 && held.length > 0; round += 1) {
        const now = held;
        held = [];
        for (const callback of now) callback(performance.now());
      }
    },
  };
}

/** Opens the edit of comment `id`, its words `body`, on `page`'s writer, the page's frames held
 *  while the edit draws and run inside `act` after (`holdFrames`): the writer puts the caret in
 *  the edit's field on a frame (`OutlineWriter.focus`), once the edit is drawn. The page gets its
 *  own frames back whether or not the edit draws. */
async function editHoldingFrames(
  page: Pick<BoxPage, "writer">,
  id: string,
  body: string
): Promise<void> {
  const frames = holdFrames();
  try {
    await act(async () => page.writer().edit(id, body));
    act(() => frames.run());
  } finally {
    frames.release();
  }
}

/** The kinds the drawn line shows as chips, in their order, each with whether it is pressed: the
 *  write line's own stay in the page, hidden, while the box edits. */
function kinds(): { kind: string | undefined; name: string | null; pressed: string | null }[] {
  return [...composerBox().querySelectorAll<HTMLElement>("[data-composer-kind]")]
    .filter((chip) => chip.closest("[hidden]") === null)
    .map((chip) => ({
      kind: chip.dataset.composerKind,
      name: chip.querySelector("[data-page-chrome]")?.textContent ?? null,
      pressed: chip.getAttribute("aria-pressed"),
    }));
}

/** The node id of the outline's row that holds the focus, if one does. */
function focusedRowId(): string | undefined {
  return (
    document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId ?? undefined
  );
}

function footHeight(): string {
  return document.documentElement.style.getPropertyValue(FOOT_HEIGHT_PROPERTY);
}

/** Every spy a case makes, restored after it. */
const restores: (() => void)[] = [];
function stub<T extends { mockRestore: () => void }>(spy: T): T {
  restores.push(() => spy.mockRestore());
  return spy;
}

/** The issue's comments read, which a saved edit reads again; and the search the reference
 *  picker's `#` door reads. */
let commentsRead: Mock<typeof api.listComments>;
let searchRead: Mock<typeof api.search>;

beforeAll(async () => {
  await warmMarkdownRenderer();
});

beforeEach(() => {
  reads = {
    asks: [askRecord("ask-1"), answeredAsk(ANSWERED_AT, "Old reason")],
    comments: [
      commentRecord("comment-1", BOB, "Earlier words"),
      commentRecord("comment-2", ALICE, "My own words"),
    ],
  };
  // What a write makes the page read again, answered from the case's own reads, and the reads
  // behind the reference picker and an edit's blocks, which the box hosts.
  const answered = answerBoxReads(reads);
  commentsRead = answered.commentsRead;
  searchRead = answered.searchRead;
  restores.push(answered.restore);
});

afterEach(() => {
  cleanup();
  for (const restore of restores.splice(0)) restore();
});

// R2: with no target the box writes one of two kinds, Comment on at first and Question, drawn
// as chips before Send; no chip with a reply's or a statement's target, and none on a closed
// issue. A Question pressed while the issue was open sends a comment once it is closed, rather
// than a question the issue refuses (IC:153).
test("the box draws two kinds, and none with a target or on a closed issue", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockResolvedValue(
      commentRecord("comment-9", ALICE, "Is the box done?")
    )
  );
  // A question the closed issue would refuse, as Dispatch refuses it; the box must send none.
  const createAsk = stub(
    spyOn(api, "createAsk").mockRejectedValue(
      new ApiError(409, { code: "ISSUE_CLOSED", error: "This issue is closed." })
    )
  );
  const page = renderPage();

  expect(kinds()).toEqual([
    { kind: "comment", name: CONTROL_LABELS.composer_comment, pressed: "true" },
    { kind: "ask", name: CONTROL_LABELS.composer_question, pressed: "false" },
  ]);
  const send = within(composerBox()).getByRole("button", { name: CONTROL_LABELS.send });
  for (const chip of composerBox().querySelectorAll("[data-composer-kind]")) {
    expect(chip.compareDocumentPosition(send) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  }

  const writeOnIssue = async () => {
    await user.click(
      within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
    );
    await waitFor(() => expect(targetWords()).toBeNull());
  };
  // A reply's target, then a statement's: no chip while either is held.
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  expect(kinds()).toEqual([]);
  await writeOnIssue();
  expect(kinds()).toHaveLength(2);
  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo(undefined, "requirement")));
  expect(kinds()).toEqual([]);
  await writeOnIssue();
  expect(kinds()).toHaveLength(2);

  await user.click(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.composer_question })
  );
  expect(kinds().map((chip) => chip.pressed)).toEqual(["false", "true"]);
  await user.click(writeField());
  await user.keyboard("Is the box done?");
  page.redraw({ closed: true });
  expect(kinds()).toEqual([]);
  await user.click(writeField());
  await user.keyboard("{Enter}");
  await waitFor(() =>
    expect(createComment).toHaveBeenCalledWith(KEY, { body: "Is the box done?" }, keyedAttempt)
  );
  expect(createAsk).not.toHaveBeenCalled();
});

// R1: while a view is on or a document is open the box is hidden, never unmounted, so what the
// reader typed is there when it is drawn again; it is drawn there once the reader asks to write -
// a Reply or an Edit, which put the caret in its field (IC:83) - or while a send of the line's
// own is out. An Answer asks for nothing here: the box sends no answer
// (`OutlineWriter.answer`).
test("under a view or a document the box is hidden until the reader asks to write, and its draft is kept", async () => {
  const user = userEvent.setup();
  const page = renderPage();
  expect(composerBox()).not.toHaveAttribute("hidden");
  await user.click(writeField());
  await user.keyboard("Half a thought");

  page.redraw({ viewOn: true });
  expect(composerBox()).toHaveAttribute("hidden");
  expect(writeField().value).toBe("Half a thought");
  page.redraw({ documentOpen: true });
  expect(composerBox()).toHaveAttribute("hidden");
  expect(writeField().value).toBe("Half a thought");
  page.redraw({});
  expect(composerBox()).not.toHaveAttribute("hidden");
  expect(writeField().value).toBe("Half a thought");

  // An Answer on a view's row leaves the box hidden, its draft kept, and the focus on the Answer.
  page.redraw({ viewOn: true });
  const answer = screen.getByRole("button", { name: "Answer" });
  await user.click(answer);
  await waitFor(() => expect(page.writer().answer?.askId).toBe("ask-1"));
  expect(composerBox()).toHaveAttribute("hidden");
  expect(answer).toHaveFocus();
  expect(writeField().value).toBe("Half a thought");
  await act(async () => page.writer().clear());

  // With a document open, Edit and Reply each draw the box with the caret in their field, and
  // ending them hides it again.
  page.redraw({ documentOpen: true });
  // The writer puts the caret in the line's field on a frame, once the edit is drawn
  // (`OutlineWriter.focus`), so the edit is opened with the frames held (`editHoldingFrames`).
  await editHoldingFrames(page, "comment-2", "My own words");
  await waitFor(() =>
    expect(within(composerBox()).getByRole("textbox", { name: "Edit comment" })).toHaveFocus()
  );
  expect(composerBox()).not.toHaveAttribute("hidden");
  await user.keyboard("{Escape}");
  await waitFor(() => expect(composerBox()).toHaveAttribute("hidden"));
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(composerBox()).not.toHaveAttribute("hidden");
  // The Reply, a move of the page's, carries nothing: it keeps the words the box held for the
  // issue they were written for, and the box's own end of the reply brings them back.
  expect(writeField().value).toBe("");
  expect(keptLines()).toEqual([KEPT_FOR_ISSUE]);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(composerBox()).toHaveAttribute("hidden"));
  expect(writeField().value).toBe("Half a thought");

  // A send made at rest is still out when a view opens: the box stays drawn over it until
  // Dispatch answers, and is hidden once it has.
  const out = Promise.withResolvers<CommentWithTrees>();
  stub(spyOn(api, "createComment").mockReturnValue(out.promise));
  page.redraw({});
  await user.click(writeField());
  await user.keyboard("{Control>}a{/Control}Sent at rest{Enter}");
  await waitFor(() => expect(api.createComment).toHaveBeenCalledTimes(1));
  page.redraw({ viewOn: true });
  expect(composerBox()).not.toHaveAttribute("hidden");
  // The send lands a task after its answer (TanStack Query notifies on a zero-delay timer), and
  // the box then hides and gives the focus back: both inside `act`.
  await act(async () => {
    out.resolve(commentRecord("comment-9", ALICE, "Sent at rest"));
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  await waitFor(() => expect(composerBox()).toHaveAttribute("hidden"));
});

// The kept lines name the way back by a control's words: the box's control that writes on the
// issue for what was written there, and a statement's Reply, pressed on that statement, for what
// was written on one. Each line holds the words that control draws (`CONTROL_LABELS`), so a kept
// line never names a control the page draws under other words.
test("the kept lines name the Reply and the write-on-issue control by the words those controls draw", () => {
  expect(ANSWER_LINES.kept_on_issue).toContain(CONTROL_LABELS.write_on_issue);
  expect(ANSWER_LINES.kept_on_statement).toContain(`press ${CONTROL_LABELS.reply} `);
});

// The reviewer's finding 4593511272: a Reply is a move of the page's, which carries nothing, so
// it keeps what the reader has typed in the box at rest for the issue it was written for. The
// box says the comment is kept and the way back, and opens on the reply with that row's own
// start; the box's own end of the reply, Escape, brings the comment back.
test("a Reply keeps a comment half-typed in the box at rest", async () => {
  const user = userEvent.setup();
  const page = renderPage();
  await user.click(writeField());
  await user.keyboard("Half a thought");
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(writeField().value).toBe("");
  expect(keptLines()).toEqual([KEPT_FOR_ISSUE]);

  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(writeField().value).toBe("Half a thought");
  expect(keptLines()).toEqual([]);
});

// The same finding: a Reply on another row is a move of the page's, which carries nothing, so a
// reply typed to one row is kept for that row when the reader turns the box to another row with
// its Reply, or with `r` on a statement, each named above the line. The box says whose comment
// the kept reply answers and opens on each new target with that target's own start, and the
// first row's Reply brings the reply back.
test("a Reply on another row keeps the reply typed for its own row, and that row's Reply brings it back", async () => {
  const user = userEvent.setup();
  const page = renderPage();
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("My reply to bob");

  // The Reply of the question the reader answered, pressed.
  await act(async () => rowOf("ask-2").focus());
  const reply = rowOf("ask-2").querySelector<HTMLElement>('[data-row-act="reply"]');
  if (reply === null) throw new Error("the question's row drew no Reply");
  await user.click(reply);
  await waitFor(() => expect(targetWords()).toBe(replyingTo("carol", "ask")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(page.writer().reply?.id).toBe("ask-2");
  expect(writeField().value).toBe("");
  expect(keptLines()).toEqual([keptReplyLine("bob")]);

  // `r` on a statement, and back to bob's comment.
  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo(undefined, "requirement")));
  expect(writeField().value).toBe("");
  expect(keptLines()).toEqual([keptReplyLine("bob")]);
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  expect(writeField().value).toBe("My reply to bob");
  expect(keptLines()).toEqual([]);
});

/** What the reader writes in the box for a target the cases below take away. */
const KEPT_WORDS = "Words for a target that goes";

/** The statement's words as a later version writes them, after `STATEMENT_REWRITTEN`. */
const REWRITTEN_AGAIN = "The page draws **a single** box, at the foot.";

/** The outline with a message of bob's after its other rows. */
function messageOutline(): TreeNode {
  const root = outline();
  return { ...root, children: [...root.children, messageNode("message-1", BOB, "A message")] };
}

/** Each target the box keeps a draft for that the outline can stop drawing: the row a Reply aims
 *  the box at, the outline drawn with it and the one drawn once it is gone, the line the box says
 *  of the draft while the target is drawn, and the words it leads the draft with once it is not. */
const GONE_TARGETS = [
  {
    drawn: outline,
    gone: () => outlineWithout("comment-1"),
    kept: keptReplyLine("bob"),
    lead: "The comment by bob that your reply answered is no longer listed. Your reply is kept here:",
    name: "a reply kept for a comment a read then deletes",
    row: "comment-1",
  },
  {
    drawn: messageOutline,
    gone: () => outlineWithout("message-1", messageOutline()),
    kept: "Your reply to the message by bob is kept. Press Reply on that message to go on with it.",
    lead: "The message by bob that your reply answered is no longer listed. Your reply is kept here:",
    name: "a reply kept for a message a read then deletes",
    row: "message-1",
  },
  {
    drawn: outline,
    gone: () => outlineWithout("ask-1"),
    // The kept line names a question's thread as a comment's (`KeptReply`).
    kept: keptReplyLine("carol"),
    lead: "The ask by carol that your reply answered is no longer listed. Your reply is kept here:",
    name: "a reply kept for a question a read then deletes",
    row: "ask-1",
  },
  {
    drawn: outline,
    gone: () => outlineRewritten(STATEMENT_REWRITTEN),
    kept: KEPT_FOR_STATEMENT,
    lead: ANSWER_LINES.kept_statement_gone,
    name: "a comment kept for a statement whose words a new version changes",
    row: "spec#b-r1",
  },
] as const;

/** Writes `words` in the box aimed at the row of `nodeId` with its `r`, then turns the box to the
 *  issue with the issue's own Reply, a move of the page's, which keeps them for that row. Answers
 *  the target they are kept for, as the writer aimed the box at it. */
async function keepFor(
  user: UserEvent,
  page: DrawnPage,
  nodeId: string,
  words: string
): Promise<Pick<OutlineWriter, "reply" | "statement">> {
  await page.press(user, nodeId, "r");
  await waitFor(() => expect(targetWords()).not.toBeNull());
  await waitFor(() => expect(writeField()).toHaveFocus());
  const { reply, statement } = page.writer();
  await user.clear(writeField());
  await user.keyboard(words);
  await act(async () => page.writer().writeOnIssue());
  await waitFor(() => expect(targetWords()).toBeNull());
  return { reply, statement };
}

// The reviewer's finding 6011026760: a read that takes away the entry a kept reply answers takes
// its Reply with it, and a new version that rewrites a statement's words makes its Reply aim the
// box at the new words, so the way back the kept line names is gone. The box draws such a draft
// itself beside the line, with Discard my draft and, at rest on the issue with nothing of its
// own, Use it here, as the Conversation draws a reply kept for a comment it no longer lists.
// Discard forgets the draft in the tab too; Use it here puts it in the field. While the target is
// drawn the draft keeps its line.
for (const target of GONE_TARGETS) {
  for (const press of ["Discard my draft", "Use it here"] as const) {
    test(`${target.name} is drawn whole with its presses, and ${press} ${press === "Use it here" ? "puts it in the field" : "forgets it"}`, async () => {
      const user = userEvent.setup();
      const page = renderPage({}, target.drawn());
      const { reply, statement } = await keepFor(user, page, target.row, KEPT_WORDS);
      const drawn = { gone: goneDrafts(), kept: keptLines() };

      page.redrawOutline(target.gone());
      await waitFor(() => expect(goneDrafts()).toHaveLength(1));
      const gone = { gone: goneDrafts(), kept: keptLines() };
      await user.click(within(composerBox()).getByRole("button", { name: press }));
      await waitFor(() => expect(goneDrafts()).toEqual([]));
      const stored = unsentDraftsOf(page.client).readAway(
        composerNameOf(["issue-composer", KEY]),
        reply,
        undefined,
        statement
      );
      expect({
        after: { field: writeField().value, kept: keptLines(), stored },
        drawn,
        gone,
      }).toEqual({
        after: { field: press === "Use it here" ? KEPT_WORDS : "", kept: [], stored: undefined },
        drawn: { gone: [], kept: [target.kept] },
        gone: {
          gone: [
            { draft: KEPT_WORDS, lead: target.lead, presses: ["Use it here", "Discard my draft"] },
          ],
          kept: [],
        },
      });
    });
  }
}

// The same finding: each kept line and each draft drawn whole is keyed by its draft's whole
// address, so two the box keeps on one statement's row - for its words before a new version and
// for those after - are two, both drawn, and React is handed no two under one key.
test("two drafts kept on one statement's row, for its words before a new version and after, are both drawn", async () => {
  const user = userEvent.setup();
  const errors = stub(spyOn(console, "error"));
  const page = renderPage();
  await keepFor(user, page, "spec#b-r1", "On the first words");
  page.redrawOutline(outlineRewritten(STATEMENT_REWRITTEN));
  await waitFor(() => expect(goneDrafts()).toHaveLength(1));
  await keepFor(user, page, "spec#b-r1", "On the second words");
  const once = { gone: goneDrafts().map(({ draft }) => draft), kept: keptLines() };

  page.redrawOutline(outlineRewritten(REWRITTEN_AGAIN));
  await waitFor(() => expect(goneDrafts()).toHaveLength(2));
  expect({
    keys: errors.mock.calls
      .map((call) => call.map(String).join(" "))
      .filter((said) => said.includes("same key")),
    once,
    twice: { gone: goneDrafts().map(({ draft }) => draft), kept: keptLines() },
  }).toEqual({
    keys: [],
    once: { gone: ["On the first words"], kept: [KEPT_FOR_STATEMENT] },
    twice: { gone: ["On the first words", "On the second words"], kept: [] },
  });
});

/** The outline with the planner's comment after its others: a Reply on it starts the box with a
 *  mention of the planner. */
function plannerOutline(): TreeNode {
  const root = outline();
  return {
    ...root,
    children: [...root.children, commentNode("comment-3", PLANNER, "The relay is fixed")],
  };
}

/** The page with the planner's comment in the outline (`plannerOutline`) and in the comments
 *  read. */
function renderWithPlannerComment() {
  reads.comments.push(commentRecord("comment-3", PLANNER, "The relay is fixed"));
  return renderPage({}, plannerOutline());
}

/** Replies to the planner's comment, which writes the planner's mention into the empty box. */
async function replyToPlanner(user: UserEvent, page: DrawnPage): Promise<void> {
  await page.press(user, "comment-3", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("Planner", "comment")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(writeField().value).toBe(PLANNER_PREFILL);
}

// The reviewer's finding 5942007779: a Reply on an agent's comment writes the agent's mention into
// the box, which is how the reply reaches the agent. A Reply on a person's comment is a move of
// the page's, which carries nothing: it opens the box on that comment with its own start, so a
// reply under a person's comment is not delivered to the agent the reader no longer replies to.
test("a Reply on a person's comment after one on an agent's takes the agent's mention out of the send", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockResolvedValue(commentRecord("comment-9", ALICE, "Thanks"))
  );
  const page = renderWithPlannerComment();
  await replyToPlanner(user, page);

  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  const draft = writeField().value;
  await user.keyboard("Thanks{Enter}");
  await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
  // The request is what would reach the agent: a reply under bob's comment, delivered to nobody.
  const [key, input] = createComment.mock.calls[0] ?? [];
  expect(key).toBe(KEY);
  expect(input).toEqual({ body: "Thanks", reply_to: "comment-1" });
  expect(draft).toBe("");
});

// The same finding: the line's control that writes on the issue instead, Escape, and `r` on a
// statement each end the reply to the agent's comment, and the agent's mention ends with it.
test("ending a reply to an agent's comment takes the agent's mention out of the box", async () => {
  const user = userEvent.setup();
  const page = renderWithPlannerComment();
  await replyToPlanner(user, page);
  await user.click(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
  );
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(writeField().value).toBe("");

  await replyToPlanner(user, page);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(writeField().value).toBe("");

  await replyToPlanner(user, page);
  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo(undefined, "requirement")));
  expect(writeField().value).toBe("");
});

/** Turns the box to bob's comment with `r` and back to the planner's with its `r`: moves of the
 *  host's, which keep the reply to the planner for the planner's comment, open the box on bob's
 *  with that comment's own start, and bring the reply back, since a move of the host's carries
 *  nothing. Answers what the box held on bob's row and the lines it drew there of what it keeps. */
async function toBobAndBack(
  user: UserEvent,
  page: DrawnPage
): Promise<{ held: string; kept: string[] }> {
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  const away = { held: writeField().value, kept: keptLines() };
  await page.press(user, "comment-3", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("Planner", "comment")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  return away;
}

/** What `toBobAndBack` finds on bob's row: the box empty there, and its line for the reply it
 *  keeps for the planner's comment. */
const KEPT_FOR_PLANNER = { held: "", kept: [keptReplyLine("Planner")] };

// The same finding: the words the reader typed after the agent's mention are theirs. A Reply on
// a person's comment is a move of the page's, which carries nothing: it keeps them for the
// agent's comment with the mention, and its Reply brings them back as they were; the reader's own
// end of the reply then keeps them on the issue and takes out the mention the box wrote, even
// with the words typed straight after its link and no space between them: the reader deletes the
// space the box wrote after it.
test("ending a reply to an agent's comment keeps the words typed straight after the agent's mention, and takes the mention out", async () => {
  const user = userEvent.setup();
  const page = renderWithPlannerComment();
  await replyToPlanner(user, page);
  await user.keyboard("{Backspace}please look again");
  expect(writeField().value).toBe(`${PLANNER_SEED}please look again`);

  expect(await toBobAndBack(user, page)).toEqual(KEPT_FOR_PLANNER);
  expect(writeField().value).toBe(`${PLANNER_SEED}please look again`);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(writeField().value).toBe("please look again");
});

// The same finding: a mention of the agent the reader wrote themself is their own aim. A Reply on
// a person's comment is a move of the page's, which carries nothing: it keeps the reply to the
// agent whole for the agent's comment, both mentions in it, and its Reply brings it back as it
// was. The reader's own end of the reply then carries it to the issue, where the mention the box
// wrote goes and the reader's own stays, so the comment reaches the agent the reader named.
test("a mention of the agent the reader wrote stays when the reader ends the reply, after the box turned to a person's comment and back", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockResolvedValue(commentRecord("comment-9", ALICE, "cc"))
  );
  const page = renderWithPlannerComment();
  await replyToPlanner(user, page);
  // The reader picks the planner after their own word, which writes the link the box wrote.
  await user.keyboard("cc ");
  await user.paste(PLANNER_SEED);
  const written = `${PLANNER_PREFILL}cc ${PLANNER_SEED}`;
  expect(writeField().value).toBe(written);

  expect(await toBobAndBack(user, page)).toEqual(KEPT_FOR_PLANNER);
  expect(writeField().value).toBe(written);
  await user.click(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
  );
  await waitFor(() => expect(targetWords()).toBeNull());
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(writeField().value).toBe(`cc ${PLANNER_SEED}`);

  // The comment reaches the agent the reader named.
  await user.keyboard("{Enter}");
  await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
  expect(createComment.mock.calls[0]?.[1]).toEqual({
    body: `cc ${PLANNER_SEED}`,
    delivery: "steer",
  });
});

// A finding of the reviewer's on the same rule: the box takes out only the mention it wrote,
// while that mention still stands at the front of the draft as written. A reader who empties the
// box and writes the planner's link after a word of their own names the planner themself: the
// reply goes to bob's comment's row and back whole, and the reader's own end of the reply keeps
// the link, so the comment on the issue still reaches the planner.
test("a link to the agent the reader writes after their own word, in place of the box's, stays through a new target", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockResolvedValue(commentRecord("comment-9", ALICE, "cc"))
  );
  const page = renderWithPlannerComment();
  await replyToPlanner(user, page);
  // The reader empties the box, then picks the planner after their own word, which writes the
  // link the box wrote.
  await user.keyboard("{Control>}a{/Control}{Backspace}cc ");
  await user.paste(PLANNER_SEED);
  expect(writeField().value).toBe(`cc ${PLANNER_SEED}`);

  expect(await toBobAndBack(user, page)).toEqual(KEPT_FOR_PLANNER);
  expect(writeField().value).toBe(`cc ${PLANNER_SEED}`);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  const draft = writeField().value;
  await user.keyboard("{Enter}");
  await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
  // The comment reaches the agent the reader named.
  expect(createComment.mock.calls[0]?.[1]).toEqual({
    body: `cc ${PLANNER_SEED}`,
    delivery: "steer",
  });
  expect(draft).toBe(`cc ${PLANNER_SEED}`);
});

/** The draft of a reader who empties the box after a Reply on the planner's comment and writes
 *  the planner's link at its front themself, exactly as the box wrote it, then their words. */
const OWN_LINK_IN_FRONT = `${PLANNER_SEED} the reader wrote this link.`;

/** Empties the box and writes `OWN_LINK_IN_FRONT` in it: the reader's own link, in place of the
 *  box's. */
async function writeOwnLinkInFront(user: UserEvent): Promise<void> {
  await user.keyboard("{Control>}a{/Control}{Backspace}");
  await user.paste(PLANNER_SEED);
  await user.keyboard(" the reader wrote this link.");
  expect(writeField().value).toBe(OWN_LINK_IN_FRONT);
}

// The tester's finding on the same rule: a mention the box wrote stops being the box's once the
// reader's own edit leaves the draft no longer opening with it. A reader who deletes the box's
// link and writes the same link at the front themself names the planner: the reader's own end of
// the reply keeps that link, though it reads exactly as the box's did.
test("a link to the agent the reader writes at the front, in place of the box's they deleted, stays when they end the reply", async () => {
  const user = userEvent.setup();
  const page = renderWithPlannerComment();
  await replyToPlanner(user, page);
  await writeOwnLinkInFront(user);

  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(writeField().value).toBe(OWN_LINK_IN_FRONT);
});

// The same finding through a move of the host's: the draft kept for the reply keeps no mention of
// the box's that the reader deleted, so the move to bob's comment's row and back brings none back
// with it, and the reader's own end of the reply keeps the reader's link at the front.
test("a link to the agent the reader writes at the front, in place of the box's they deleted, stays through a new target", async () => {
  const user = userEvent.setup();
  const page = renderWithPlannerComment();
  await replyToPlanner(user, page);
  await writeOwnLinkInFront(user);

  expect(await toBobAndBack(user, page)).toEqual(KEPT_FOR_PLANNER);
  expect(writeField().value).toBe(OWN_LINK_IN_FRONT);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(writeField().value).toBe(OWN_LINK_IN_FRONT);
});

// The mentions the box wrote for a reply are read where a draft is kept, as the box holds them
// then. The reader's words in front of the agent's mention leave it no longer opening the draft,
// so the reader's own end of the reply carries the draft to the issue whole, the mention the
// reader's to keep, and lets go of what the box wrote. The draft the page keeps for the issue as
// it goes then holds no mention of the box's, for the box drawn on the issue again to take out.
test("a reply carried to the issue with the reader's words before the agent's mention keeps no mention of the box's as the page goes", async () => {
  const user = userEvent.setup();
  const page = renderWithPlannerComment();
  await replyToPlanner(user, page);
  await user.keyboard("{Home}cc ");
  expect(writeField().value).toBe(`cc ${PLANNER_PREFILL}`);
  await user.click(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
  );
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(writeField().value).toBe(`cc ${PLANNER_PREFILL}`);

  page.view.unmount();
  const kept = unsentDraftsOf(page.client).readAway(composerNameOf(["issue-composer", KEY]), null);
  expect(kept?.draft.body).toBe(`cc ${PLANNER_PREFILL}`);
  expect(kept?.seeds).toBeUndefined();
});

/** The draft of the refused reply to the planner's comment in the grid below, by who wrote its
 *  link: the planner's link the Reply wrote, then the reader's words; or the reader's own link to
 *  the planner in its place, in the words the reader wrote it in, then theirs. */
const PLANNER_HELLO = `${PLANNER_PREFILL}hello`;
const OWN_HELLO = `${PLANNER_MENTION} hello`;

/** What Send says on bob's row while the refused reply, let go, is held for the issue. */
const WRITTEN_FOR_ISSUE = `This message was written for ${KEY}. Go back to it to send it, or discard it.`;

/** What the reader does with the refused reply in the grid below: Retry it as it was; end it -
 *  Escape, or the target line's control - and Retry the words on the issue; or move it - let it
 *  go with Escape and press `r` on bob's comment, or let it go with the target line's control and
 *  press that row's Reply - then press Enter there, and go back to the issue with its own Reply
 *  and Retry the words there. A refused reply holds the box on the comment it answers until the
 *  reader lets it go in the box (`redirectedReplyTarget`), so a move lets it go first. */
type AfterRefusal =
  | "retry"
  | "end with Escape"
  | "end with its control"
  | "move with r"
  | "move with Reply";

/**
 * One case of the grid below. The reader replies to the planner's comment, whose Reply writes the
 * planner's link into the box, and types their words after it; where `ownLink`, they first take
 * that link out and write their own in its place (`OWN_HELLO`). Dispatch refuses the reply once.
 * Where `remount`, the page then mounts again in the same tab over the refused reply, as coming
 * back to the issue or a reload brings the box back. Then the reader does `then`. Answers the
 * draft the box holds as the second request goes, that request, and for a move what the box
 * held on bob's row: its words, what Send said there, and whether it offered Retry.
 */
async function afterRefusedPlannerReply(
  then: AfterRefusal,
  { ownLink, remount }: { readonly ownLink: boolean; readonly remount: boolean }
) {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment")
      .mockRejectedValueOnce(
        new ApiError(400, { code: "INVALID_INPUT", error: "The reply was refused." })
      )
      .mockResolvedValue(commentRecord("comment-9", ALICE, "hello"))
  );
  const first = renderWithPlannerComment();
  await replyToPlanner(user, first);
  if (ownLink) {
    await user.keyboard("{Control>}a{/Control}{Backspace}");
    await user.paste(OWN_HELLO);
  } else {
    await user.keyboard("hello");
  }
  const written = ownLink ? OWN_HELLO : PLANNER_HELLO;
  expect(writeField().value).toBe(written);
  await user.keyboard("{Enter}");
  await waitFor(() =>
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent("The reply was refused.")
  );
  expect(createComment.mock.calls[0]?.[1]).toEqual({
    body: written,
    delivery: "steer",
    reply_to: "comment-3",
  });

  let page = first;
  if (remount) {
    first.view.unmount();
    page = renderPage({}, plannerOutline(), first.client);
    await waitFor(() => expect(targetWords()).toBe(replyingTo("Planner", "comment")));
    expect(page.writer().reply?.id).toBe("comment-3");
    expect(writeField().value).toBe(written);
  }

  if (then !== "retry") {
    // The reply let go, the reader's own move: the box writes on the issue, and the refused
    // draft and its refusal go with it.
    if (then === "end with Escape" || then === "move with r") {
      await user.click(writeField());
      await user.keyboard("{Escape}");
    } else {
      await user.click(
        within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
      );
    }
    await waitFor(() => expect(targetWords()).toBeNull());
  }
  let away: { refused: boolean; retry: boolean; text: string } | undefined;
  if (then === "move with r" || then === "move with Reply") {
    // A Reply on bob's comment, the host's move: the refusal stays with the issue it is held for,
    // and the box on bob's row says so and sends nothing.
    if (then === "move with r") await page.press(user, "comment-1", "r");
    else {
      await act(async () => rowOf("comment-1").focus());
      const reply = rowOf("comment-1").querySelector<HTMLElement>('[data-row-act="reply"]');
      if (reply === null) throw new Error("bob's row drew no Reply");
      await user.click(reply);
    }
    await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
    await waitFor(() => expect(writeField()).toHaveFocus());
    await user.keyboard("{Enter}");
    const send = within(composerBox()).getByRole("button", { name: CONTROL_LABELS.send });
    away = {
      refused: send.getAttribute("aria-disabled") === "true",
      retry: within(composerBox()).queryByRole("button", { name: "Retry" }) !== null,
      text: writeField().value,
    };
    expect(send).toHaveAccessibleDescription(WRITTEN_FOR_ISSUE);
    expect(createComment).toHaveBeenCalledTimes(1);
    // The issue's own Reply, the way back to where the draft was written for. The outline draws
    // the issue as its header rather than as a row, so the case calls what the issue's row's
    // Reply calls (`RowActs`), from the row that holds the focus.
    await act(async () => rowOf("comment-1").focus());
    await act(async () => page.writer().writeOnIssue());
    await waitFor(() => expect(targetWords()).toBeNull());
  }
  const draft = writeField().value;
  await user.click(within(composerBox()).getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
  return { away, draft, sent: createComment.mock.calls[1]?.[1] };
}

// The reviewer's findings 5945015185 and PRRT_kwDORFy7ds64qGzx on the same rule, as a grid: the
// same box or one mounted again over the refused reply, times the planner's link written by the
// box or by the reader, times a move, an end or a Retry. The reader's own end of the reply
// carries the draft and its refusal to the issue, and takes out the link the box wrote, so a
// comment on the issue reaches no agent the reader did not name; a Reply on bob's comment after
// it is a move of the page's, which carries nothing: it leaves the refusal held for the issue -
// the box on bob's row says so and sends nothing - and the issue's own Reply brings it back; a
// Retry sends the refused reply as it was, the link the box wrote in it. The reader's own link
// stays in every case: the box takes nothing out of the draft that it did not write, whether or
// not it mounted again over the send (`SentRequest.seeds`).
test("a refused reply let go stays on the issue through a Reply on bob's comment, without the agent's link the box wrote", async () => {
  const { away, draft, sent } = await afterRefusedPlannerReply("move with r", {
    ownLink: false,
    remount: false,
  });
  expect(away).toEqual({ refused: true, retry: false, text: "hello" });
  expect(draft).toBe("hello");
  expect(sent).toEqual({ body: "hello" });
});

test("a refused reply ended takes out the agent's link the box wrote", async () => {
  const { draft, sent } = await afterRefusedPlannerReply("end with its control", {
    ownLink: false,
    remount: false,
  });
  expect(draft).toBe("hello");
  expect(sent).toEqual({ body: "hello" });
});

test("a refused reply retried keeps the agent's link the box wrote", async () => {
  const { draft, sent } = await afterRefusedPlannerReply("retry", {
    ownLink: false,
    remount: false,
  });
  expect(draft).toBe(PLANNER_HELLO);
  expect(sent).toEqual({ body: PLANNER_HELLO, delivery: "steer", reply_to: "comment-3" });
});

test("a refused reply let go stays on the issue through a Reply on bob's comment, with the reader's own link to the agent", async () => {
  const { away, draft, sent } = await afterRefusedPlannerReply("move with Reply", {
    ownLink: true,
    remount: false,
  });
  expect(away).toEqual({ refused: true, retry: false, text: OWN_HELLO });
  expect(draft).toBe(OWN_HELLO);
  expect(sent).toEqual({ body: OWN_HELLO, delivery: "steer" });
});

test("a refused reply ended keeps the reader's own link to the agent", async () => {
  const { draft, sent } = await afterRefusedPlannerReply("end with Escape", {
    ownLink: true,
    remount: false,
  });
  expect(draft).toBe(OWN_HELLO);
  expect(sent).toEqual({ body: OWN_HELLO, delivery: "steer" });
});

test("a refused reply retried keeps the reader's own link to the agent", async () => {
  const { draft, sent } = await afterRefusedPlannerReply("retry", {
    ownLink: true,
    remount: false,
  });
  expect(draft).toBe(OWN_HELLO);
  expect(sent).toEqual({ body: OWN_HELLO, delivery: "steer", reply_to: "comment-3" });
});

test("a box mounted again over a refused reply let go keeps it on the issue through a Reply on bob's comment, without the agent's link the box wrote", async () => {
  const { away, draft, sent } = await afterRefusedPlannerReply("move with r", {
    ownLink: false,
    remount: true,
  });
  expect(away).toEqual({ refused: true, retry: false, text: "hello" });
  expect(draft).toBe("hello");
  expect(sent).toEqual({ body: "hello" });
});

test("a box mounted again over a refused reply ended takes out the agent's link the box wrote", async () => {
  const { draft, sent } = await afterRefusedPlannerReply("end with its control", {
    ownLink: false,
    remount: true,
  });
  expect(draft).toBe("hello");
  expect(sent).toEqual({ body: "hello" });
});

test("a box mounted again over a refused reply retried keeps the agent's link the box wrote", async () => {
  const { draft, sent } = await afterRefusedPlannerReply("retry", {
    ownLink: false,
    remount: true,
  });
  expect(draft).toBe(PLANNER_HELLO);
  expect(sent).toEqual({ body: PLANNER_HELLO, delivery: "steer", reply_to: "comment-3" });
});

test("a box mounted again over a refused reply let go keeps it on the issue through a Reply on bob's comment, with the reader's own link to the agent", async () => {
  const { away, draft, sent } = await afterRefusedPlannerReply("move with Reply", {
    ownLink: true,
    remount: true,
  });
  expect(away).toEqual({ refused: true, retry: false, text: OWN_HELLO });
  expect(draft).toBe(OWN_HELLO);
  expect(sent).toEqual({ body: OWN_HELLO, delivery: "steer" });
});

// The architect's case: the finding's own probe.
test("the reader's own link at the front survives a remount and an ended target", async () => {
  const { draft, sent } = await afterRefusedPlannerReply("end with Escape", {
    ownLink: true,
    remount: true,
  });
  expect(draft).toBe(OWN_HELLO);
  expect(sent).toEqual({ body: OWN_HELLO, delivery: "steer" });
});

test("a box mounted again over a refused reply retried keeps the reader's own link to the agent", async () => {
  const { draft, sent } = await afterRefusedPlannerReply("retry", {
    ownLink: true,
    remount: true,
  });
  expect(draft).toBe(OWN_HELLO);
  expect(sent).toEqual({ body: OWN_HELLO, delivery: "steer", reply_to: "comment-3" });
});

// The tester's finding through a box mounted again over a refused reply: what the reader writes
// over the refusal is kept with the send, and so are the box's mentions as the box holds them
// then. A reader who deletes the box's link after the refusal and writes the same link at the
// front themself leaves the send no mention of the box's, so the box mounted again over it keeps
// the reader's link when they end the reply.
test("a link to the agent the reader writes at the front over a refused reply, in place of the box's they deleted, stays when they end the reply in a box mounted again", async () => {
  const user = userEvent.setup();
  stub(
    spyOn(api, "createComment").mockRejectedValueOnce(
      new ApiError(400, { code: "INVALID_INPUT", error: "The reply was refused." })
    )
  );
  const first = renderWithPlannerComment();
  await replyToPlanner(user, first);
  await user.keyboard("hello{Enter}");
  await waitFor(() =>
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent("The reply was refused.")
  );
  await user.click(writeField());
  await writeOwnLinkInFront(user);

  first.view.unmount();
  const page = renderPage({}, plannerOutline(), first.client);
  await waitFor(() => expect(targetWords()).toBe(replyingTo("Planner", "comment")));
  expect(page.writer().reply?.id).toBe("comment-3");
  expect(writeField().value).toBe(OWN_LINK_IN_FRONT);
  await user.click(writeField());
  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(writeField().value).toBe(OWN_LINK_IN_FRONT);
});

// The same finding through a box mounted again over the draft kept for the reply, with no
// refusal: the draft is kept for the planner's comment as the page goes, with the box's mentions
// as the box holds them then, which is none once the reader deleted the box's link. The box
// mounted again brings the draft back with the planner's Reply, and the reader's own end of the
// reply keeps the reader's link at the front.
test("a link to the agent the reader writes at the front, in place of the box's they deleted, stays when they end the reply in a box mounted again over the kept draft", async () => {
  const user = userEvent.setup();
  const first = renderWithPlannerComment();
  await replyToPlanner(user, first);
  await writeOwnLinkInFront(user);

  first.view.unmount();
  const page = renderPage({}, plannerOutline(), first.client);
  await page.press(user, "comment-3", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("Planner", "comment")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(writeField().value).toBe(OWN_LINK_IN_FRONT);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(writeField().value).toBe(OWN_LINK_IN_FRONT);
});

// R5: the box answers no ask (`OutlineWriter.answer`). The writer's answer to an open question,
// its change of the reader's own answer and an option pressed on a question's row each leave the
// box as it is with no target: its write line drawn with its kinds and its draft, no target line,
// no field but its own, and the caret where the reader left it, on a row outside the box. The
// frames the writer puts a caret in the box on are held and run inside `act`, so a caret asked
// for lands before the box is read.
test("the writer's answer, its change and an option's press open nothing in the box and put no caret in it", async () => {
  const user = userEvent.setup();
  const page = renderPage();
  await user.click(writeField());
  await user.keyboard("Half a thought");
  const presses = [
    ["an answer", () => page.writer().answerNode(askNode("ask-1", "open"))],
    ["a change of answer", () => page.writer().changeAnswer(askNode("ask-2", "answered"))],
    ["an option", () => page.writer().pick(askNode("ask-1", "open"), "Yes")],
  ] as const;
  const frames = holdFrames();
  try {
    for (const [name, press] of presses) {
      await act(async () => rowOf("comment-1").focus());
      await act(async () => press());
      act(() => frames.run());
      expect(page.writer().answer, name).toBeDefined();
      expect(document.activeElement, name).toBe(rowOf("comment-1"));
      expect(composerBox().contains(document.activeElement), name).toBe(false);
      expect(composerBox(), name).not.toHaveAttribute("hidden");
      expect(writeField().closest("[hidden]"), name).toBeNull();
      expect(within(composerBox()).getAllByRole("textbox"), name).toEqual([writeField()]);
      expect(targetWords(), name).toBeNull();
      expect(kinds(), name).toHaveLength(2);
      expect(writeField().value, name).toBe("Half a thought");
    }
  } finally {
    frames.release();
  }
});

// R5: the writer holding the reader's own comment (an Edit of it), the box holds its words, and
// Save writes the words the reader left there; the edit then ends on the comment's row.
test("an edit saves the comment's words", async () => {
  const user = userEvent.setup();
  const editComment = stub(
    spyOn(api, "editComment").mockImplementation(async (id, input) =>
      commentRecord(id, ALICE, input.body ?? "")
    )
  );
  const page = renderPage();

  await act(async () => page.writer().edit("comment-2", "My own words"));
  const field = within(composerBox()).getByRole<HTMLTextAreaElement>("textbox", {
    name: "Edit comment",
  });
  await waitFor(() => expect(field).toHaveFocus());
  expect(field.value).toBe("My own words");
  expect(targetWords()).toBe(targetNamed(CONTROL_LABELS.editing, READER, "comment"));
  expect(kinds()).toEqual([]);
  expect(within(composerBox()).getByRole("button", { name: CONTROL_LABELS.save })).toBeTruthy();

  const readsBefore = commentsRead.mock.calls.length;
  await user.keyboard("{Control>}a{/Control}My better words{Enter}");
  await waitFor(() =>
    expect(editComment).toHaveBeenCalledWith("comment-2", { body: "My better words" }, keyedAttempt)
  );
  await waitFor(() => expect(page.writer().editing).toBeUndefined());
  // The outline draws the comment again from the read that answers after the save.
  expect(commentsRead.mock.calls.length).toBeGreaterThan(readsBefore);
  await waitFor(() => expect(focusedRowId()).toBe("comment-2"));
});

// R7: with the look, the hint under the field and the list of mention targets are off screen and
// still read out - the hint as the field's description, the list by its name - and a row of the
// reference picker carries no `title`, the page drawing no tooltips (`NoTooltips`). A session's
// row, whose name two sessions can share, draws the session's full id on screen instead, in a
// line of the row that is its description and no part of its name.
test("the look keeps the hint and the targets off screen, and a picker row has no title", async () => {
  const user = userEvent.setup();
  const hit: SearchResult = {
    href: "/issues/CORE-2/comments/comment-7",
    id: "comment-7",
    kind: "comment",
    owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
    rank: 0.1,
    snippet: "The relay drops a reply",
  };
  searchRead.mockResolvedValue(searchResponse([hit]));
  renderPage();
  const field = writeField();

  const described = (field.getAttribute("aria-describedby") ?? "").split(/\s+/).flatMap((id) => {
    const line = id === "" ? null : document.getElementById(id);
    return line === null ? [] : [line];
  });
  const hint = described.find(
    (line) => line.textContent === dispatchWritingBox("issue-page-composer", "comment").hint
  );
  expect(hint).toBeDefined();
  expect(hint).toHaveClass("sr-only");
  expect(composerBox().contains(hint ?? null)).toBe(true);

  await user.click(field);
  await user.paste(`${PLANNER_MENTION} `);
  const targets = within(composerBox()).getByRole("list", { name: "Mention targets" });
  expect(targets).toHaveClass("sr-only");
  expect(targets).toHaveTextContent("session:planner-session");

  await user.keyboard("#relay");
  const list = await within(composerBox()).findByRole("listbox", { name: "References" });
  const row = await within(list).findByRole("option", { name: /The relay drops a reply/ });
  const route = nodeRouteFromHref(hit.href);
  if (route === undefined) throw new Error("the hit names no address");
  // The row draws its detail line, and nothing in the list carries a title for it.
  expect(within(row).getByText(shortForm(route))).toBeInTheDocument();
  expect(list.querySelectorAll("[title]")).toHaveLength(0);

  // The space closes the `#` door, and an `@` query lists the planner's session row.
  await user.keyboard(" @Plan");
  const parties = await within(composerBox()).findByRole("listbox", { name: "References" });
  const session = await within(parties).findByRole("option", { name: /^Planner/ });
  expect(parties.querySelectorAll("[title]")).toHaveLength(0);
  expect(session).toHaveAccessibleDescription(PLANNER.id);
  expect(session).not.toHaveAccessibleName(new RegExp(PLANNER.id));
  const line = within(session).getByText(PLANNER.id);
  expect(line).not.toHaveClass("sr-only");
  expect(line).toHaveAttribute("aria-hidden", "true");
});

// R7, and the root spec's R6 (no tooltip on the page) and R19 (nothing lost): today's composers
// show a session's full id as its picker row's tooltip, and the box draws none, so a sighted
// reader tells two sessions apart only by what the row draws on screen. Two live sessions the
// registry lists under one title and one directory, seen at the same moment, are two rows of the
// box's picker, and an off-screen line is no part of what a sighted reader reads (the tester's
// red test, round 12).
test("two sessions that share a title draw words on screen in the box's picker that tell them apart", async () => {
  const user = userEvent.setup();
  const seen = Date.now();
  const at = { last_activity: new Date(seen).toISOString(), last_seen: seen };
  const twins = [plannerAgent(at), plannerAgent({ ...at, session_id: "planner-session-2" })];
  // The case's reads answer the agent list with the planner alone (`answerBoxReads`).
  (api.listAgents as unknown as Mock<typeof api.listAgents>).mockResolvedValue(twins);
  const { client } = renderPage();
  act(() => client.setQueryData(["agents"], twins));
  await user.click(writeField());
  await user.keyboard("@Plan");
  const parties = await within(composerBox()).findByRole("listbox", { name: "References" });
  const rows = await within(parties).findAllByRole("option", { name: /^Planner/ });
  expect(rows).toHaveLength(2);
  // What the row draws on screen, and a line that describes it where that line is drawn too.
  const drawnWords = (element: Element): string => {
    const drawn = element.cloneNode(true) as Element;
    for (const hidden of drawn.querySelectorAll(".sr-only")) hidden.remove();
    return (drawn.textContent ?? "").replace(/\s+/g, " ").trim();
  };
  const onScreen = rows.map((row) => {
    const line = document.getElementById(row.getAttribute("aria-describedby") ?? "");
    const described = line === null || line.classList.contains("sr-only") ? "" : drawnWords(line);
    return `${drawnWords(row)} ${described}`.trim();
  });
  expect(
    onScreen[0],
    "the two sessions' rows draw the same words on screen, so a sighted reader cannot tell which session each is"
  ).not.toBe(onScreen[1]);
});

// The Design's "focused row": the box publishes how far up from the screen's foot it reaches
// (`FOOT_HEIGHT_PROPERTY`) while it is drawn, keeps it as the box changes size, and takes it
// away while it is hidden, so a row brought on screen is brought clear of it.
test("the box sets the foot height while drawn and removes it while hidden", async () => {
  const page = renderPage({ viewOn: true });
  const section = composerBox();
  expect(section).toHaveAttribute("hidden");
  expect(footHeight()).toBe("");

  // happy-dom lays nothing out, so the section answers with the place the page gives it: its top
  // `reach` px above the screen's foot.
  let reach = 58;
  section.getBoundingClientRect = () => new DOMRect(0, window.innerHeight - reach, 1000, reach);
  page.redraw({});
  expect(section).not.toHaveAttribute("hidden");
  expect(footHeight()).toBe("58px");
  // A window resized: user-event has no form for it, so the window is told as a browser tells it.
  reach = 96;
  act(() => {
    window.dispatchEvent(new Event("resize"));
  });
  expect(footHeight()).toBe("96px");

  page.redraw({ viewOn: true });
  expect(section).toHaveAttribute("hidden");
  expect(footHeight()).toBe("");
  await act(async () => page.writer().replyTo(BOBS_COMMENT));
  await waitFor(() => expect(section).not.toHaveAttribute("hidden"));
  expect(footHeight()).toBe("96px");
  await act(async () => page.writer().clear());
  expect(section).toHaveAttribute("hidden");
  expect(footHeight()).toBe("");
});

// The reach the box publishes is its own height wherever it stands. At the outline's end the read
// mark, which the reach moves down (`slots/foot.tsx`), can make the page longer than the box's
// place, so a box that has just shrunk stands above the screen's foot; a reach read off that
// place would hold the mark there, and the box above the foot, until the box next changed size.
test("the box publishes its own height while it stands above the screen's foot", () => {
  const page = renderPage({ viewOn: true });
  const section = composerBox();
  // A box of one line, 54 px tall, whose foot stands 15 px above the screen's.
  section.getBoundingClientRect = () => new DOMRect(0, window.innerHeight - 69, 1000, 54);
  page.redraw({});
  expect(section).not.toHaveAttribute("hidden");
  expect(footHeight()).toBe("54px");
});

// IC:92: the line that writes on the issue stays mounted while the box edits a comment, so what
// the reader was typing there is still there when the edit ends.
test("a comment typed in the box is still there after an edit ends", async () => {
  const user = userEvent.setup();
  const page = renderPage();
  await user.click(writeField());
  await user.keyboard("Half a thought");

  // An edit, left with Escape. The writer puts the caret in the edit's field on a frame, which
  // takes it from the write line's field, so the edit is opened with the frames held.
  await editHoldingFrames(page, "comment-2", "My own words");
  const edit = within(composerBox()).getByRole("textbox", { name: "Edit comment" });
  await waitFor(() => expect(edit).toHaveFocus());
  expect(writeField().closest("[hidden]")).not.toBeNull();
  expect(writeField().value).toBe("Half a thought");
  await user.keyboard("{Escape}");
  await waitFor(() => expect(page.writer().editing).toBeUndefined());
  expect(writeField().closest("[hidden]")).toBeNull();
  expect(writeField().value).toBe("Half a thought");
});

// An empty criterion line's Send is refused and says what to type, on the line its description
// names (`RefusableButton`); its press writes nothing. The line is drawn only once the issue's
// tree read names its tree artifact, and this file's page caches no tree read, so the room's facts
// stand in for that read here; the line is aimed at bob's comment directly, since no row offers
// Write a criterion without that read.
test("an empty criterion line's Send says why it sends nothing", async () => {
  const user = userEvent.setup();
  stub(
    spyOn(roomTarget, "useRoomFacts").mockReturnValue({
      converted: false,
      issueKey: KEY,
      tree: "artifact-tree",
      viewOf: null,
    })
  );
  // Every write of a criterion, which the line appends to the tree artifact.
  const written: string[] = [];
  answerRequests(
    http.post("/api/v1/artifacts/:id/edits", ({ request }) => {
      written.push(new URL(request.url).pathname);
      return Response.json({});
    })
  );
  const page = renderPage();

  await act(async () =>
    page.writer().criterionUnder(commentNode("comment-1", BOB, "Earlier words"))
  );
  const line = composerBox().querySelector<HTMLFormElement>("form[data-criterion-under]");
  if (line === null) throw new Error("the box drew no criterion line");
  const field = within(line).getByRole<HTMLTextAreaElement>("textbox", {
    name: CONTROL_LABELS.write_criterion,
  });
  expect(field.value).toBe("");
  const send = within(line).getByRole("button", { name: CONTROL_LABELS.send });
  expect(send).toHaveAccessibleDescription(CONTROL_LABELS.criterion_placeholder_phone);
  expect(send).toHaveAttribute("aria-disabled", "true");

  await user.click(send);
  expect(written).toEqual([]);
  // No write went out: Send still reads as itself rather than as a write under way, and the line
  // keeps its aim.
  expect(send).toHaveTextContent(CONTROL_LABELS.send);
  expect(page.writer().criterion?.id).toBe("comment-1");
});

// The criterion line's target line names the node the criterion goes under as the box's own
// target line names a reply's entry: "Criterion under:", the node's author and the page's word
// for its kind - a comment, a message, a question - and none of its words, which stand whole in
// the node's own row. Its control ends the aim. The room's facts stand in for the tree read, as
// in the case above.
test("the criterion line's target line names the node by its author and its kind, and draws none of its words", async () => {
  const user = userEvent.setup();
  stub(
    spyOn(roomTarget, "useRoomFacts").mockReturnValue({
      converted: false,
      issueKey: KEY,
      tree: "artifact-tree",
      viewOf: null,
    })
  );
  const page = renderPage({}, messageOutline());
  for (const { author, kind, node, words } of [
    {
      author: "bob",
      kind: "comment",
      node: commentNode("comment-1", BOB, "Earlier words"),
      words: "Earlier words",
    },
    {
      author: "bob",
      kind: "message",
      node: messageNode("message-1", BOB, "A message"),
      words: "A message",
    },
    { author: "carol", kind: "ask", node: askNode("ask-1", "open"), words: "Question ask-1?" },
  ] as const) {
    await act(async () => page.writer().criterionUnder(node));
    await waitFor(() =>
      expect(targetWords(), node.id).toBe(
        targetNamed(CONTROL_LABELS.criterion_target, author, kind)
      )
    );
    expect(page.writer().criterion?.id, node.id).toBe(node.id);
    expect(targetWords(), node.id).not.toContain(words);
    // The target line drawn is the criterion line's own: the write line is hidden under it.
    const line = composerBox().querySelector(`form[data-criterion-under="${node.id}"]`);
    expect(line?.querySelector("[data-composer-target]"), node.id).not.toBeNull();
  }

  await user.click(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
  );
  await waitFor(() => expect(page.writer().criterion).toBeUndefined());
  expect(composerBox().querySelector("form[data-criterion-under]")).toBeNull();
});

// Departure 1 of the plan: under a view no outline is drawn to consume a landing, so a write sent
// there leaves none, and the focus goes back to the control the reader came to the box from.
test("under a view a sent write leaves no landing and gives the focus back where it came from", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockResolvedValue(
      commentRecord("comment-9", ALICE, "Under the view", { reply_to: "comment-1" })
    )
  );
  const page = renderPage({ viewOn: true });
  expect(composerBox()).toHaveAttribute("hidden");

  const reply = screen.getByRole("button", { name: "Reply" });
  await user.click(reply);
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(composerBox()).not.toHaveAttribute("hidden");
  await user.keyboard("Under the view{Enter}");
  await waitFor(() =>
    expect(createComment).toHaveBeenCalledWith(
      KEY,
      { body: "Under the view", reply_to: "comment-1" },
      keyedAttempt
    )
  );
  await waitFor(() => expect(reply).toHaveFocus());
  expect(page.writer().landing).toBeUndefined();
  expect(page.writer().reply).toBeNull();
  expect(composerBox()).toHaveAttribute("hidden");
});

// R11: `r` on a statement aims the box at it: the target line names it by the page's word for its
// kind and draws none of its words, and the placeholder says the comment is on the
// statement. The comment sent carries the statement's whole words as its anchor's quote, with no
// occurrence, so words the document holds twice are refused rather than anchored to the wrong
// block. Once it lands the reader is back on the statement's own row; the outline draws the
// comment as a row under the issue, at its end, and no margin beside the statement draws it yet.
test("a statement's target line names its kind, and its comment carries its words as the quote and lands on its row", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockResolvedValue(commentRecord("comment-9", ALICE, "Agreed"))
  );
  const page = renderPage();

  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await waitFor(() => expect(targetWords()).toBe(replyingTo(undefined, "requirement")));
  expect(writeField()).toHaveAttribute("placeholder", CONTROL_LABELS.statement_placeholder);
  expect(kinds()).toEqual([]);

  await user.keyboard("Agreed{Enter}");
  await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
  const [key, input, bounds] = createComment.mock.calls[0] ?? [];
  expect(key).toBe(KEY);
  expect(bounds).toEqual(keyedAttempt);
  expect(input).toEqual({ anchor: { artifact: "spec", quote: STATEMENT_QUOTE }, body: "Agreed" });
  expect(Object.keys(input?.anchor ?? {}).sort()).toEqual(["artifact", "quote"]);
  await waitFor(() => expect(page.writer().statement).toBeUndefined());
  // The landing names the statement's own row, which the outline takes at once: it waits for no
  // new row under the statement.
  expect(page.writer().landing).toBeUndefined();
  await waitFor(() => expect(focusedRowId()).toBe("spec#b-r1"));
});

// R11: the target line names a statement by the author its node holds, where it holds one, and
// by its kind alone where it holds none, never by the document's maker; either way it draws none
// of its words, which stand whole in the statement's own row.
test("a statement's target line names the first author its node lists, and no one where the list is empty", async () => {
  const user = userEvent.setup();
  const words = "The box sits at the foot of the outline.";
  const drawn = outline();
  const authored = node({
    author: BOB,
    id: "spec#b-r2",
    reference: `dispatch://${KEY}/spec#b-r2`,
    source: SPEC_SOURCE,
    statement: true,
    text: words,
    type: "requirement",
  });
  const page = renderPage({}, { ...drawn, children: [...drawn.children, authored] });

  await page.press(user, "spec#b-r2", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "requirement")));
  expect(page.writer().statement?.row).toBe("spec#b-r2");
  expect(targetWords()).not.toContain(words);

  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo(undefined, "requirement")));
  expect(page.writer().statement?.row).toBe("spec#b-r1");
  for (const part of STATEMENT_QUOTE.split(". ")) expect(targetWords()).not.toContain(part);
});

// The reviewer's finding 4593511280: a move of the page's carries nothing, so a refusal stays
// with the address it was written for, and the box's address names the statement it comments on
// (`OutlineLook.anchor`), so at its own statement a refused comment is no draft written elsewhere
// and its Retry sends it. A comment on a statement Dispatch refused holds the box on the
// statement - a Reply on another row puts the caret back in it on that statement, and an Answer
// leaves it there with the caret on the row, its refused words and its Retry kept either way -
// and a box that mounts again over the refused send, as one does when the reader leaves the issue
// and comes back, names the statement from the send and sends the words to it again. The send
// keeps the statement's document, words and row, so a write to it lands on the statement's row.
test("a refused comment on a statement keeps its words and its statement, through a Reply elsewhere and a new mount", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment")
      .mockRejectedValueOnce(
        new ApiError(400, { code: "INVALID_INPUT", error: "The comment was refused." })
      )
      .mockResolvedValue(commentRecord("comment-9", ALICE, "Agreed"))
  );
  const first = renderPage();
  await first.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("Agreed{Enter}");
  await waitFor(() =>
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent("The comment was refused.")
  );
  const retry = () => within(composerBox()).queryByRole("button", { name: "Retry" });
  expect(retry()).not.toBeNull();

  // `r` on bob's comment, and `a` on the open question.
  const named = replyingTo(undefined, "requirement");
  for (const [row, key] of [
    ["comment-1", "r"],
    ["ask-1", "a"],
  ] as const) {
    await first.press(user, row, key);
    if (key === "r") await waitFor(() => expect(writeField()).toHaveFocus());
    else expect(focusedRowId(), `${key} on ${row}`).toBe(row);
    expect(targetWords(), `${key} on ${row}`).toBe(named);
    expect(writeField().value, `${key} on ${row}`).toBe("Agreed");
    expect(first.writer().reply, `${key} on ${row}`).toBeNull();
    expect(first.writer().answer, `${key} on ${row}`).toBeUndefined();
    expect(retry(), `${key} on ${row}`).not.toBeNull();
  }

  // The box mounted again in the same tab, over the send it refused: the statement named by its
  // kind, as before.
  first.view.unmount();
  const again = renderPage({}, outline(), first.client);
  await waitFor(() => expect(targetWords()).toBe(named));
  expect(again.writer().statement).toEqual({
    artifact: "spec",
    quote: STATEMENT_QUOTE,
    row: "spec#b-r1",
  });
  expect(writeField().value).toBe("Agreed");
  await user.click(within(composerBox()).getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
  expect(createComment.mock.calls[1]?.[1]).toEqual({
    anchor: { artifact: "spec", quote: STATEMENT_QUOTE },
    body: "Agreed",
  });
  await waitFor(() => expect(again.writer().statement).toBeUndefined());
  expect(again.writer().landing).toBeUndefined();
  await waitFor(() => expect(focusedRowId()).toBe("spec#b-r1"));
});

// The reviewer's finding 5942007779: a comment on a statement sent from a box that mounted again
// over its refused send - as a reload brings one back - lands as every send from the box lands
// (R4). The box mounted again shows the statement the refusal was written for, so Send there is
// that refusal's own and goes. The held send keeps the statement's row beside its words, so once
// the comment lands the outline brings the statement's row on screen and gives it the focus,
// rather than giving it back to where the reader came from. The comment itself is drawn as a row
// under the issue, at the outline's end; no margin beside the statement draws it yet.
test("a comment on a statement sent again from a box mounted over its refused send lands on the statement's row", async () => {
  const user = userEvent.setup();
  stub(
    spyOn(api, "createComment")
      .mockRejectedValueOnce(
        new ApiError(400, { code: "INVALID_INPUT", error: "The comment was refused." })
      )
      .mockResolvedValue(commentRecord("comment-9", ALICE, "Agreed"))
  );
  const first = renderPage();
  await first.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("Agreed{Enter}");
  await waitFor(() =>
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent("The comment was refused.")
  );

  // The box mounted again over the send it refused, as a reload brings it back.
  first.view.unmount();
  const again = renderPage({}, outline(), first.client);
  await waitFor(() => expect(targetWords()).toBe(replyingTo(undefined, "requirement")));
  expect(writeField().value).toBe("Agreed");
  expect(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.send })
  ).not.toHaveAttribute("aria-disabled");

  // The statement's row is drawn below the screen, so landing on it has to scroll to it.
  const scrolled = stub(spyOn(window, "scrollBy").mockImplementation(() => {}));
  stub(
    spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
      this: HTMLElement
    ) {
      return this.dataset.nodeId === "spec#b-r1" ? new DOMRect(0, 2000, 800, 24) : new DOMRect();
    })
  );
  await user.click(within(composerBox()).getByRole("button", { name: CONTROL_LABELS.send }));
  await waitFor(() => expect(again.writer().statement).toBeUndefined());
  expect(again.writer().landing).toBeUndefined();
  await waitFor(() => expect(focusedRowId()).toBe("spec#b-r1"));
  // Scrolled until the row's bottom clears the box at the foot (`bringOnScreen`).
  const floor = window.innerHeight - (Number.parseFloat(footHeight()) || 0);
  expect(scrolled).toHaveBeenCalledWith({ top: 2024 - floor + 8 });
});

// The reviewer's finding 4593511306: a statement whose words its document holds twice is
// refused with `TARGET_AMBIGUOUS`, whose own words ask for an occurrence the box has no control
// for: the box says in the page's words why the comment cannot be placed, draws no Retry, which
// would send the same words to the same refusal, and keeps the draft and its statement.
test("a statement held twice is refused in the page's words, with no Retry and the draft kept", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockRejectedValue(
      new ApiError(409, {
        code: "TARGET_AMBIGUOUS",
        error: `"${STATEMENT_QUOTE}" matches 2 places; pass a zero-based occurrence, or quote more of the surrounding text`,
      })
    )
  );
  const page = renderPage();
  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("Agreed{Enter}");
  await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
  const alert = await within(composerBox()).findByRole("alert");
  expect(alert).toHaveTextContent(ANSWER_LINES.statement_twice);
  expect(alert.textContent).not.toContain("occurrence");
  expect(within(composerBox()).queryByRole("button", { name: "Retry" })).toBeNull();
  expect(writeField().value).toBe("Agreed");
  expect(targetWords()).toBe(replyingTo(undefined, "requirement"));
});

// The reviewer's finding PRRT_kwDORFy7ds64Sg45: a comment on a statement refused with
// `TARGET_AMBIGUOUS` holds the box on its statement only while the held send carries it. The
// target line's control takes the statement off the held send, its refusal and its words kept,
// so `r` on another row then aims the box at that row, the refused words kept.
test("a refused statement let go with the target line's control frees a Reply", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockRejectedValue(
      new ApiError(409, {
        code: "TARGET_AMBIGUOUS",
        error: `"${STATEMENT_QUOTE}" matches 2 places; pass a zero-based occurrence, or quote more of the surrounding text`,
      })
    )
  );
  const page = renderPage();
  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("Agreed{Enter}");
  await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
  expect(await within(composerBox()).findByRole("alert")).toHaveTextContent(
    ANSWER_LINES.statement_twice
  );

  // The target line's control: the box writes on the issue.
  await user.click(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
  );
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(page.writer().statement).toBeUndefined();

  // `r` on bob's comment: a reply to it, the refused words still the box's draft.
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(page.writer().reply?.id).toBe("comment-1");
  expect(page.writer().statement).toBeUndefined();
  expect(writeField().value).toBe("Agreed");

  // The refusal is still held, with no statement left on it.
  const held = heldSends(page.client).get(["issue-composer", KEY]);
  expect(held?.status).toBe("refused");
  expect(held?.request.statement).toBeUndefined();
});

// The reviewer's finding PRRT_kwDORFy7ds64Sg4-: a reply Dispatch refused keeps the box on the
// comment it answers when the reader presses `r` on a statement, as a Reply on another row does
// (`redirectedReplyTarget`), so Enter sends the words again as that reply and never as a comment
// anchored to the statement.
test("r on a statement keeps the box on a refused reply, and the send carries the reply", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment")
      .mockRejectedValueOnce(
        new ApiError(400, { code: "INVALID_INPUT", error: "The reply was refused." })
      )
      .mockResolvedValue(
        commentRecord("comment-9", ALICE, "My reply to bob", { reply_to: "comment-1" })
      )
  );
  const page = renderPage();
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("My reply to bob{Enter}");
  await waitFor(() =>
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent("The reply was refused.")
  );

  // `r` on the statement: the box stays on bob's comment, the refused words in it.
  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(targetWords()).toBe(replyingTo("bob", "comment"));
  expect(page.writer().reply?.id).toBe("comment-1");
  expect(page.writer().statement).toBeUndefined();
  expect(writeField().value).toBe("My reply to bob");

  // Enter sends the words again as the reply, with no statement's anchor.
  await user.keyboard("{Enter}");
  await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
  expect(createComment.mock.calls[1]?.[1]).toEqual({
    body: "My reply to bob",
    reply_to: "comment-1",
  });
});

/** What the refused-reply helpers below use of a page `renderPage` drew. */
type DrawnPage = Pick<BoxPage, "press" | "writer">;

/** Replies to bob's comment with "My reply to bob", which Dispatch refuses (400): the box then
 *  holds a refused reply, its words in the field. */
async function refuseReply(user: UserEvent, page: DrawnPage): Promise<void> {
  stub(
    spyOn(api, "createComment").mockRejectedValue(
      new ApiError(400, { code: "INVALID_INPUT", error: "The reply was refused." })
    )
  );
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("My reply to bob{Enter}");
  await waitFor(() =>
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent("The reply was refused.")
  );
}

/** That the box is still on the refused reply to bob's comment, with no answer and no edit set
 *  beside it, the caret in the line and the refused words in it. */
async function onRefusedReply(page: DrawnPage): Promise<void> {
  expect(targetWords()).toBe(replyingTo("bob", "comment"));
  expect(page.writer().answer).toBeUndefined();
  expect(page.writer().editing).toBeUndefined();
  expect(page.writer().reply?.id).toBe("comment-1");
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(writeField().value).toBe("My reply to bob");
}

// An Edit of the reader's own comment keeps the box on a refused reply: the edit would put the
// comment's words in place of the reply's. No row draws Edit yet, so the case asks the writer.
test("an Edit keeps the box on a refused reply", async () => {
  const user = userEvent.setup();
  const page = renderPage();
  await refuseReply(user, page);
  await act(async () => page.writer().edit("comment-2", "My own words"));
  await onRefusedReply(page);
});

// The reviewer's finding 5942007779: the issue's own Reply keeps the box on a refused reply as
// every other aim does. It would set the box back to writing on the issue, and the held send
// with it, so the next Send or Retry would post the refused words as a comment on the issue
// rather than as the reply. Only the target line's own control lets the reply go. The outline
// draws the issue as its header rather than as a row, so the case calls what the issue's row's
// Reply calls (`RowActs`), from another row that holds the focus as a press on a row's control
// leaves it.
test("the issue's own Reply keeps the box on a refused reply", async () => {
  const user = userEvent.setup();
  const page = renderPage();
  await refuseReply(user, page);
  await act(async () => rowOf("comment-2").focus());
  await act(async () => page.writer().writeOnIssue());
  await onRefusedReply(page);
  // The held send still answers bob's comment, so its Retry is sent as that reply.
  const held = heldSends(page.client).get(["issue-composer", KEY]);
  expect(held?.status).toBe("refused");
  expect(held?.request.replyTo?.id).toBe("comment-1");
});

// The same for a comment on a statement Dispatch refused: the issue's own Reply keeps the box on
// the statement, its refused words kept, and the held send keeps the statement.
test("the issue's own Reply keeps the box on a refused comment on a statement", async () => {
  const user = userEvent.setup();
  stub(
    spyOn(api, "createComment").mockRejectedValue(
      new ApiError(400, { code: "INVALID_INPUT", error: "The comment was refused." })
    )
  );
  const page = renderPage();
  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("Agreed{Enter}");
  await waitFor(() =>
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent("The comment was refused.")
  );
  await act(async () => rowOf("comment-2").focus());
  await act(async () => page.writer().writeOnIssue());
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(targetWords()).toBe(replyingTo(undefined, "requirement"));
  expect(writeField().value).toBe("Agreed");
  expect(page.writer().statement?.quote).toBe(STATEMENT_QUOTE);
  expect(heldSends(page.client).get(["issue-composer", KEY])?.request.statement).toEqual({
    artifact: "spec",
    quote: STATEMENT_QUOTE,
    row: "spec#b-r1",
  });
});

// Ending a refused reply in the box lets it go, as ending a statement does: the target line's
// control takes the reply off the held send, its refusal and its words kept, so `r` on another
// row then aims the box at that row.
test("a refused reply let go with the target line's control frees a Reply on another row", async () => {
  const user = userEvent.setup();
  const page = renderPage();
  await refuseReply(user, page);

  // The target line's control: the box writes on the issue.
  await user.click(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
  );
  await waitFor(() => expect(targetWords()).toBeNull());

  // `r` on the question the reader answered: a reply to it, the refused words kept.
  await page.press(user, "ask-2", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("carol", "ask")));
  expect(page.writer().reply?.id).toBe("ask-2");
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(writeField().value).toBe("My reply to bob");

  // The refusal is still held, with no reply left on it.
  const held = heldSends(page.client).get(["issue-composer", KEY]);
  expect(held?.status).toBe("refused");
  expect(held?.request.replyTo).toBeNull();
});

// A reply the reader stopped may have reached Dispatch, so a Retry of it goes under its own key.
// Once the reader lets the reply go with the target line's control, the same words go to the
// issue as a comment under a new key: under the old one Dispatch would hold a reply, and would
// refuse a comment sent with that key as the key used for another request.
test("a stopped reply let go with the target line's control is sent under a new key", async () => {
  const user = userEvent.setup();
  // Dispatch is down until the reader stops the reply, and takes the comment after.
  let down = true;
  const createComment = stub(
    spyOn(api, "createComment").mockImplementation(async (_issue, input) => {
      if (down) throw new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
      return commentRecord("comment-9", ALICE, input.body ?? "");
    })
  );
  const page = renderPage();
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("My reply to bob");

  jest.useFakeTimers();
  const clocked = userOnFakeClock();
  try {
    // The reply, tried until its deadline, and stopped.
    await clocked.keyboard("{Enter}");
    await jump(SEND_DEADLINE_MS);
    await clocked.click(within(composerBox()).getByRole("button", { name: "Stop trying" }));
    await jump(0);
    expect(heldSends(page.client).get(["issue-composer", KEY])?.status).toBe("refused");
    const tried = createComment.mock.calls.length;
    expect(tried).toBeGreaterThan(0);
    down = false;

    // The target line's control, then Send: the words go to the issue.
    await clocked.click(
      within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
    );
    expect(targetWords()).toBeNull();
    await clocked.click(writeField());
    await clocked.keyboard("{Enter}");
    await jump(0);
    expect(createComment).toHaveBeenCalledTimes(tried + 1);
    const [, comment, bounds] = createComment.mock.calls[tried] ?? [];
    expect(comment).toEqual({ body: "My reply to bob" });
    expect(bounds?.key).toBeDefined();
    expect(bounds?.key).not.toBe(createComment.mock.calls[0]?.[2]?.key);
  } finally {
    realClock();
  }
});

// A comment or a message its author deleted takes no reply (`409 POST_DELETED`), and its row
// offers no Reply, so the box answers one only where the delete came while it was open. The box
// reads the post as the outline's comments read holds it now (`MentionComposer`'s `replyParent`),
// so Send refuses the reply before it goes, in the composer's words and then the box's own
// sentence for where it sends the draft instead (`OutlineLook.deletedReplyWay`), which names the
// box's control that writes on the issue, on a line under the box that Send's refusal names;
// neither Send nor Enter sends anything, and the words and the target stay.
test("a reply open when its comment is deleted is refused before Send, and a press sends nothing", async () => {
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockResolvedValue(commentRecord("comment-9", ALICE, "Ship it."))
  );
  const page = renderPage();
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("Ship it.");

  // bob deletes his comment: the comments read answers it with the marks of his delete, and the
  // outline draws its row as the deleted line from that same read.
  const deletedAt = "2026-11-05T12:00:00Z";
  reads.comments = [
    commentRecord("comment-1", BOB, "", { deleted_at: deletedAt, deleted_by: BOB }),
    commentRecord("comment-2", ALICE, "My own words"),
  ];
  const drawn = outline();
  page.redrawOutline({
    ...drawn,
    children: drawn.children.map((child) =>
      child.id === "comment-1"
        ? { ...child, deletedAt, deletedBy: BOB, text: `Deleted by bob · ${deletedAt}` }
        : child
    ),
  });
  await act(() => page.client.invalidateQueries({ queryKey: ["comments", KEY] }));

  expect(ANSWER_LINES.deleted_reply_way).toContain(`Press ${CONTROL_LABELS.write_on_issue} `);
  const refused =
    "The comment this reply answers was deleted, so it takes no reply. " +
    ANSWER_LINES.deleted_reply_way;
  const line = await within(composerBox()).findByText(refused);
  const send = within(composerBox()).getByRole("button", { name: CONTROL_LABELS.send });
  expect({
    describedBy: send.getAttribute("aria-describedby"),
    disabled: send.getAttribute("aria-disabled"),
    offScreen: line.closest(".sr-only") !== null,
  }).toEqual({ describedBy: line.id, disabled: "true", offScreen: false });

  await user.click(send);
  await user.click(writeField());
  await user.keyboard("{Enter}");
  await onRealTurns(() => createComment.mock.calls.length > 0, 100);
  expect(createComment).not.toHaveBeenCalled();
  expect(writeField().value).toBe("Ship it.");
  expect(targetWords()).toBe(replyingTo("bob", "comment"));
});

// R3: the target line names the entry a reply answers by its author and the page's word for its
// kind - a comment, a message, a question - and draws none of its words: the entry stands whole
// in its own row, and the line is no place the page shortens a node's text.
test("the target line names the entry by its author and its kind, and draws none of its words", async () => {
  const user = userEvent.setup();
  const page = renderPage({}, messageOutline());
  for (const { author, kind, row, words } of [
    { author: "bob", kind: "comment", row: "comment-1", words: "Earlier words" },
    { author: "bob", kind: "message", row: "message-1", words: "A message" },
    { author: "carol", kind: "ask", row: "ask-1", words: "Question ask-1?" },
  ] as const) {
    await page.press(user, row, "r");
    await waitFor(() => expect(targetWords(), row).toBe(replyingTo(author, kind)));
    expect(page.writer().reply?.id, row).toBe(row);
    expect(targetWords(), row).not.toContain(words);
  }
});

// R3: the target line names a person who acted through a session as the page names an author
// (`useAgentText().author`), as the row of their comment names them: by their login and the
// page's words for an act made through an agent, not by the session's id.
test("the target line names a person who acted through a session as the page names an author", async () => {
  const user = userEvent.setup();
  const through: Actor = { act: { session: PLANNER.id }, id: "bob", kind: "user" };
  const words = "Written through the planner";
  reads.comments.push(commentRecord("comment-4", through, words));
  const root = outline();
  const drawn = { ...root, children: [...root.children, commentNode("comment-4", through, words)] };
  const page = renderPage({}, drawn);
  await page.press(user, "comment-4", "r");
  await waitFor(() =>
    expect(targetWords()).toBe(
      replyingTo(`bob, acting through ${AUTHOR_KIND_WORDS.agent}`, "comment")
    )
  );
  expect(page.writer().reply?.id).toBe("comment-4");
});

/** The classes of `element` and of everything inside it that are a colour of the dashboard's
 *  palette (`theme/classes.ts`'s slate greys and rose and red refusals), which nothing inside the
 *  issue page is drawn in, since the page has a palette of its own. */
function dashboardColours(element: Element): string[] {
  return [element, ...element.querySelectorAll("*")]
    .flatMap((one) => [...one.classList])
    .filter((name) => /(?:slate|rose|red)-\d/u.test(name));
}

// The reviewer's finding PRRT_kwDORFy7ds64NIrQ: what the box says of a file pasted into its
// draft - the upload still out, an upload Dispatch refused with its Retry, one Dispatch stored
// and lost its answer for - and of a send past its deadline, with Stop trying, is drawn in the
// page's colours and words, as the box's refusal is, and never in the dashboard's.
test("the box draws an upload's lines and a late send's in the page's colours", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  // The first upload stays out until the case refuses it, from inside the request it made, so
  // the refusal reaches a write that is waiting for it.
  let refuseUpload: ((refusal: ApiError) => void) | undefined;
  stub(
    spyOn(api, "uploadArtifact")
      .mockImplementationOnce(() => {
        const { promise, reject } = Promise.withResolvers<UploadResult>();
        refuseUpload = reject;
        return promise;
      })
      .mockRejectedValueOnce(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }))
  );
  const answer = Promise.withResolvers<CommentWithTrees>();
  const createComment = stub(spyOn(api, "createComment").mockReturnValueOnce(answer.promise));
  try {
    renderPage();
    await user.click(writeField());
    const file = new File(["# Notes"], "notes.md", { type: "text/markdown" });
    // user-event's paste carries text, never files.
    fireEvent.paste(writeField(), { clipboardData: { files: [file] } });

    // The upload out: the page's busy word, in its muted grey.
    const uploading = await within(composerBox()).findByText(ANSWER_LINES.uploading, {
      selector: '[role="status"]',
    });
    expect(dashboardColours(uploading)).toEqual([]);
    expect(uploading).toHaveClass(pageMutedText);

    // Refused: the server's reason in the page's red, with a Retry whose press leaves the caret
    // in the field.
    await waitFor(() => expect(refuseUpload).toBeDefined());
    await act(async () =>
      refuseUpload?.(
        new ApiError(400, { code: "INVALID_INPUT", error: "The file is not allowed." })
      )
    );
    const refused = (await within(composerBox()).findByText("The file is not allowed.")).closest(
      '[role="alert"]'
    );
    if (refused === null) throw new Error("the box drew the upload's refusal as no alert");
    expect(dashboardColours(refused)).toEqual([]);
    expect(refused).toHaveClass(pageFailedText);
    await user.click(within(refused as HTMLElement).getByRole("button", { name: "Retry" }));
    expect(writeField()).toHaveFocus();

    // Stored with no answer: the page's own sentence, in its muted grey.
    const unanswered = await within(composerBox()).findByTestId("landed-without-answer");
    expect(unanswered.textContent).toBe(ANSWER_LINES.uploaded_unanswered);
    expect(dashboardColours(unanswered)).toEqual([]);
    expect(unanswered).toHaveClass(pageMutedText);

    // A send past its deadline: still being tried, in the page's muted grey, beside a Stop
    // trying drawn in the page's look.
    await user.keyboard("Agreed{Enter}");
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await jump(SEND_DEADLINE_MS);
    const late = within(composerBox()).getByText(KEYED_SEND_LATE_TEXT);
    expect(late).toHaveClass(pageMutedText);
    const lateLine = late.parentElement;
    if (lateLine === null) throw new Error("the box drew the late line alone");
    expect(dashboardColours(lateLine)).toEqual([]);
    expect(within(lateLine).getByRole("button", { name: "Stop trying" })).toHaveClass(
      pageMutedText
    );

    await act(async () => answer.resolve(commentRecord("comment-9", ALICE, "Agreed")));
    await waitFor(() => expect(writeField().value).toBe(""));
  } finally {
    realClock();
  }
});

// The conversation's current point: the outline marks on one row the node the reader's next turn
// answers - the box's target while the reader holds one, else the open question that waits on
// them, else the node of the newest act - with `aria-current="location"` and a quiet ring on its
// bullet, and while that row is folded away a lighter ring on the bullet of each row above it on
// its path. The outlines below carry the time of each act, so the newest of them is known.

/** The time of an act of the cases below, `hours` into the day: the later act has more hours. */
function hour(hours: number): string {
  return `2026-11-05T${String(hours).padStart(2, "0")}:00:00Z`;
}

/** `made` as the model holds it once it records when it was made, as it does for a turn, a
 *  question and a result. */
function madeAt(made: TreeNode, at: string): TreeNode {
  return { ...made, at };
}

/** The outline of the target's and the new act's cases: bob's comment, then the reader's own,
 *  the newer, and a statement of the spec, which is no act; then `later`, newer still. */
function timedOutline(...later: TreeNode[]): TreeNode {
  return node({
    children: [
      madeAt(commentNode("comment-1", BOB, "Earlier words"), hour(1)),
      madeAt(commentNode("comment-2", ALICE, "My own words"), hour(2)),
      statementNode(),
      ...later,
    ],
    id: KEY,
    type: "issue",
  });
}

/** The outline of the open question's case: question ask-1, asked first, waiting on the reader
 *  until `ask` says otherwise, and the reader's comment after it; then `later`. */
function questionOutline(ask: Partial<TreeNode>, ...later: TreeNode[]): TreeNode {
  return node({
    children: [
      { ...madeAt(askNode("ask-1", "open"), hour(1)), waiting: true, ...ask },
      madeAt(commentNode("comment-2", ALICE, "My own words"), hour(2)),
      ...later,
    ],
    id: KEY,
    type: "issue",
  });
}

/** The outline of the folded path's case: bob's comment, his reply under it, and under that the
 *  reader's answer to the reply, the newest act, two levels down; and a statement of the spec. */
function branchOutline(): TreeNode {
  const answer = madeAt(commentNode("comment-1b", ALICE, "My answer to it"), hour(3));
  const reply = madeAt(commentNode("comment-1a", BOB, "A reply"), hour(2));
  const first = madeAt(commentNode("comment-1", BOB, "Earlier words"), hour(1));
  return node({
    children: [
      { ...first, children: [{ ...reply, children: [answer], held: 1 }], held: 2 },
      statementNode(),
    ],
    id: KEY,
    type: "issue",
  });
}

/** What the outline draws of the current point, in reading order: each row that carries
 *  `aria-current`, by its node id and the attribute's value, and each row that carries the mark,
 *  by its node id and its `data-current-point`. */
interface DrawnPoint {
  readonly current: readonly string[];
  readonly marks: readonly string[];
}

/** The current point as the outline draws it now (`DrawnPoint`). */
function drawnPoint(): DrawnPoint {
  const rows = [...document.querySelectorAll<HTMLElement>("[data-node-row]")];
  return {
    current: rows
      .filter((row) => row.hasAttribute("aria-current"))
      .map((row) => `${row.dataset.nodeId} ${row.getAttribute("aria-current")}`),
    marks: rows
      .filter((row) => row.dataset.currentPoint !== undefined)
      .map((row) => `${row.dataset.nodeId} ${row.dataset.currentPoint}`),
  };
}

/** The current point as `drawnPoint` reads it where it is the row of `nodeId`, and no other. */
function pointOn(nodeId: string): DrawnPoint {
  return { current: [`${nodeId} location`], marks: [`${nodeId} here`] };
}

/** The point folded away below the rows of `nodeIds`, which carry the path's mark, and no row
 *  `aria-current`. */
function pathThrough(...nodeIds: string[]): DrawnPoint {
  return { current: [], marks: nodeIds.map((nodeId) => `${nodeId} path`) };
}

/** The dot of the bullet of the row of `nodeId`, which carries the ring. */
function dotOf(nodeId: string): HTMLElement {
  const dot = rowOf(nodeId).querySelector<HTMLElement>("[data-node-bullet] i");
  if (dot === null) throw new Error(`the row of ${nodeId} drew no bullet`);
  return dot;
}

// R12: at rest the point is the newest act's row, the reader's comment; a Reply on bob's older
// comment moves it there and off the reader's; and ending the reply - with Escape in the box, or
// with the target line's control that writes on the issue instead - moves it back. No other row
// carries the mark or `aria-current` at any step.
test("the current point follows the box's target as it is set and ended", async () => {
  const user = userEvent.setup();
  const page = renderPage({}, timedOutline());
  expect(drawnPoint()).toEqual(pointOn("comment-2"));

  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  expect(drawnPoint()).toEqual(pointOn("comment-1"));
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(drawnPoint()).toEqual(pointOn("comment-2"));

  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(drawnPoint()).toEqual(pointOn("comment-1")));
  await user.click(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
  );
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(drawnPoint()).toEqual(pointOn("comment-2"));
});

// R12: a new act landing moves the point to it - here the read a landed comment makes, which
// draws the outline again with bob's newer comment in it - and the mark is no control and takes no
// focus: the focus stays on the row the reader is on, and the point's row is no tab stop.
test("a new act landing moves the current point to it, and the mark takes no focus", async () => {
  const page = renderPage({}, timedOutline());
  expect(drawnPoint()).toEqual(pointOn("comment-2"));
  await act(async () => rowOf("comment-1").focus());
  const focused = document.activeElement;
  expect(focused).toBe(rowOf("comment-1"));

  page.redrawOutline(timedOutline(madeAt(commentNode("comment-3", BOB, "Newer words"), hour(3))));
  await waitFor(() => expect(drawnPoint()).toEqual(pointOn("comment-3")));
  expect(document.activeElement).toBe(focused);
  expect(rowOf("comment-3")).toHaveAttribute("tabindex", "-1");
});

// R12: with nothing aimed, a question that waits on the reader is the point, though the reader's
// comment came after it. Once answered, it waits for no answer, and the point is the newest act:
// the question itself, by its answer given after that comment, and then a newer act once one
// lands.
test("the current point is the question that waits on the reader, then the newest act once it is answered", async () => {
  const page = renderPage({}, questionOutline({}));
  expect(drawnPoint()).toEqual(pointOn("ask-1"));

  const answered: Partial<TreeNode> = {
    answer: { at: hour(3), by: BOB, selected: ["No"], text: "Not yet" },
    question: false,
    state: "answered",
    waiting: false,
  };
  page.redrawOutline(questionOutline(answered));
  await waitFor(() => expect(rowOf("ask-1")).toHaveAttribute("data-node-state", "answered"));
  expect(drawnPoint()).toEqual(pointOn("ask-1"));

  page.redrawOutline(
    questionOutline(answered, madeAt(commentNode("comment-3", BOB, "Newer words"), hour(4)))
  );
  await waitFor(() => expect(drawnPoint()).toEqual(pointOn("comment-3")));
});

// R12: while the point's row is folded away, each row above it on its path carries the path's
// mark and the lighter ring - outside the halo on a folded branch's bullet - and no row carries
// `aria-current`; unfolded, the point's own row alone is marked, with the quiet ring. Every
// branch opens unfolded (R11), so the point's own row is marked at first; bob's comment's Fold,
// pressed by hand, puts the path on its row, the reply's Fold puts the path back, and its Unfold
// the point.
test("the rows above a folded-away point carry the path's ring, and its own row the point's", async () => {
  const user = userEvent.setup();
  renderPage({}, branchOutline());
  const toggle = (name: string) =>
    user.click(within(rowOf("comment-1a")).getByRole("button", { name }));
  // The page opens with every branch unfolded: the newest act, two levels under bob's comment,
  // is the point on its own row.
  expect(drawnPoint()).toEqual(pointOn("comment-1b"));
  expect(dotOf("comment-1b")).toHaveClass(pagePointRing);

  // Bob's comment folded: its row alone is on the path, ringed as a folded branch's bullet.
  await user.click(within(rowOf("comment-1")).getByRole("button", { name: CONTROL_LABELS.fold }));
  await waitFor(() => expect(drawnPoint()).toEqual(pathThrough("comment-1")));
  expect(dotOf("comment-1")).toHaveClass(pagePathRingFolded);

  // Bob's comment unfolded and his reply folded: both are on the path, each ringed as its
  // bullet is drawn.
  await user.click(within(rowOf("comment-1")).getByRole("button", { name: CONTROL_LABELS.unfold }));
  await toggle(CONTROL_LABELS.fold);
  await waitFor(() => expect(drawnPoint()).toEqual(pathThrough("comment-1", "comment-1a")));
  expect(dotOf("comment-1")).toHaveClass(pagePathRing);
  expect(dotOf("comment-1a")).toHaveClass(pagePathRingFolded);

  await toggle(CONTROL_LABELS.unfold);
  await waitFor(() => expect(drawnPoint()).toEqual(pointOn("comment-1b")));
  expect(dotOf("comment-1b")).toHaveClass(pagePointRing);
  expect(dotOf("comment-1")).not.toHaveClass("outline-page-bullet");

  await toggle(CONTROL_LABELS.fold);
  await waitFor(() => expect(drawnPoint()).toEqual(pathThrough("comment-1", "comment-1a")));
  expect(dotOf("comment-1")).toHaveClass(pagePathRing);
  expect(dotOf("comment-1a")).toHaveClass(pagePathRingFolded);
  await toggle(CONTROL_LABELS.unfold);
  await waitFor(() => expect(drawnPoint()).toEqual(pointOn("comment-1b")));
  expect(dotOf("comment-1b")).toHaveClass(pagePointRing);
});
