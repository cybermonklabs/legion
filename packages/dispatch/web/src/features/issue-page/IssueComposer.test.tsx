import { afterEach, beforeAll, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: a paste of a file into the box, which user-event's paste cannot carry
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  endAsAReload,
  newQueryClient,
  signedInAsReader,
} from "../../__tests__/draft-matrix-fixture";
import { jump, onRealTurns, realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { issueCollaborators, plannerAgent, searchResponse } from "../../__tests__/search-fixture";
import { stopTheLateWrite, untilAttempted } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api } from "../../api/client";
import type {
  Actor,
  Agent,
  CommentWithTrees,
  IssueAskRow,
  IssueCollaboratorAgent,
  MessageRead,
  SearchResult,
} from "../../api/types";
import {
  pageFailedText,
  pageMutedText,
  pagePathRing,
  pagePathRingFolded,
  pagePointRing,
} from "../../theme/classes";
import type { UploadResult } from "../artifacts/ArtifactUpload";
import { heldSends } from "../conversation/held-sends";
import { unsentDraftsOf } from "../conversation/unsent-drafts";
import { composerNameOf } from "../conversation/useKeptDrafts";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { nodeRouteFromHref, shortForm } from "../refs/routes";
import { relativeTimeLabel } from "../refs/Timestamp";
import { SEND_DEADLINE_MS } from "../writes/keyed-write";
import { KEPT_SEND_RESUME_MS } from "../writes/write-storage";
import { KEY_HOURS } from "../writes/write-words";
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
  STOPPED_IN_BOX,
  statementNode,
  targetNamed,
  targetWords,
  writeField,
} from "./composer-fixture";
import { FOOT_HEIGHT_PROPERTY } from "./identity-block";
import type { TreeNode } from "./issue-tree";
import type { OutlineWriter } from "./outline-writer";
import { AUTHOR_TEXT_ATTRIBUTE } from "./page-structure";
import {
  ANSWER_LINES,
  ANSWER_PHRASES,
  AUTHOR_KIND_WORDS,
  BOX_PICKER_LINES,
  BOX_PICKER_NAMES,
  COMPOSER_ANSWER_WORDS,
  CONTROL_LABELS,
  UNASKED_SOURCES,
} from "./page-words";

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

/** What the box says of a send Dispatch refused as input it cannot take, in place of Dispatch's
 *  own words of it (`COMPOSER_ANSWER_WORDS.refused`). */
const REFUSED_INPUT = COMPOSER_ANSWER_WORDS.refused.INVALID_INPUT;

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
    expect(
      within(composerBox()).getByRole("textbox", { name: CONTROL_LABELS.composer_edit })
    ).toHaveFocus()
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

/** What the box says, before the words of a reply it keeps for `author`'s post of `kind` that
 *  the outline no longer lists: the page's sentence around the author's name
 *  (`COMPOSER_ANSWER_WORDS.gone_reply`). */
function goneLead(kind: keyof typeof COMPOSER_ANSWER_WORDS.gone_reply.before, author: string) {
  const { after, before } = COMPOSER_ANSWER_WORDS.gone_reply;
  return `${before[kind]}${author}${after.gone.now}`;
}

/** Each target the box keeps a draft for that the outline can stop drawing: the row a Reply aims
 *  the box at, the outline drawn with it and the one drawn once it is gone, the line the box says
 *  of the draft while the target is drawn, and the words it leads the draft with once it is not. */
const GONE_TARGETS = [
  {
    drawn: outline,
    gone: () => outlineWithout("comment-1"),
    kept: keptReplyLine("bob"),
    lead: goneLead("comment", "bob"),
    name: "a reply kept for a comment a read then deletes",
    row: "comment-1",
  },
  {
    drawn: messageOutline,
    gone: () => outlineWithout("message-1", messageOutline()),
    kept: keptReplyLine("bob", "message"),
    lead: goneLead("message", "bob"),
    name: "a reply kept for a message a read then deletes",
    row: "message-1",
  },
  {
    drawn: outline,
    gone: () => outlineWithout("ask-1"),
    // The kept line names a question by the page's word for it.
    kept: keptReplyLine("carol", "ask"),
    lead: goneLead("ask", "carol"),
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

// The same keying: a draft kept for one statement goes from the box's kept lines when that
// statement's `r` takes it back into the field, and its line goes with it, while the lines of the
// drafts still kept - one for another statement, one for bob's message - stay, each saying its
// own words. Keyed by its reply alone, every line of a draft that answers no reply would share
// one key, and React would leave the gone draft's line drawn.
test("a statement draft that goes takes its kept line with it while drafts kept on another statement and a message wait", async () => {
  const user = userEvent.setup();
  const drawn = messageOutline();
  const second = node({
    id: "spec#b-r2",
    reference: `dispatch://${KEY}/spec#b-r2`,
    source: SPEC_SOURCE,
    statement: true,
    text: "The box sits at the foot of the outline.",
    type: "requirement",
  });
  const page = renderPage({}, { ...drawn, children: [...drawn.children, second] });
  await keepFor(user, page, "spec#b-r1", "On the first statement");
  await keepFor(user, page, "message-1", "To bob's message");
  await keepFor(user, page, "spec#b-r2", "On the second statement");
  await page.press(user, "comment-2", "r");
  await waitFor(() => expect(page.writer().reply?.id).toBe("comment-2"));
  const aimed = keptLines();

  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(page.writer().statement?.row).toBe("spec#b-r1"));
  expect({ aimed, back: { field: writeField().value, kept: keptLines() } }).toEqual({
    aimed: [KEPT_FOR_STATEMENT, keptReplyLine("bob", "message"), KEPT_FOR_STATEMENT],
    back: {
      field: "On the first statement",
      kept: [keptReplyLine("bob", "message"), KEPT_FOR_STATEMENT],
    },
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

/** What Send says on bob's row while the refused reply, let go, is held for the issue: the page's
 *  sentence around where the reply was written for (`COMPOSER_ANSWER_WORDS.elsewhere`). */
const WRITTEN_FOR_ISSUE = `${COMPOSER_ANSWER_WORDS.elsewhere.address.before}${COMPOSER_ANSWER_WORDS.address.prepositions[0]} ${KEY}${COMPOSER_ANSWER_WORDS.elsewhere.address.after}`;

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
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent(REFUSED_INPUT)
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
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent(REFUSED_INPUT)
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
    name: CONTROL_LABELS.composer_edit,
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

// R7 and decision 527: with the look, the field's description and the list of mention targets
// are off screen and still read out - the description, the page's own words of what the line's
// keys do, as the field's description, the list by its name and each target by its name, never
// its address - and a row of the reference picker carries no `title`, the page drawing no
// tooltips (`NoTooltips`). A session's row, whose name two sessions can share, draws the
// session's full id on screen instead, in a line of the row that is its description and no part
// of its name.
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
  const hint = described.find((line) => line.textContent === CONTROL_LABELS.composer_keys);
  expect(hint).toBeDefined();
  expect(hint).toHaveClass("sr-only");
  expect(composerBox().contains(hint ?? null)).toBe(true);

  await user.click(field);
  await user.paste(`${PLANNER_MENTION} `);
  const targets = within(composerBox()).getByRole("list", {
    name: CONTROL_LABELS.composer_mentioned,
  });
  expect(targets).toHaveClass("sr-only");
  expect(targets.textContent).toBe("Planner");

  await user.keyboard("#relay");
  const list = await within(composerBox()).findByRole("listbox", { name: BOX_PICKER_NAMES.label });
  const row = await within(list).findByRole("option", { name: /The relay drops a reply/ });
  const route = nodeRouteFromHref(hit.href);
  if (route === undefined) throw new Error("the hit names no address");
  // The row draws its detail line, and nothing in the list carries a title for it.
  expect(within(row).getByText(shortForm(route))).toBeInTheDocument();
  expect(list.querySelectorAll("[title]")).toHaveLength(0);
  // Nor does a row's kind icon draw an svg `<title>`, which a browser shows as a tooltip.
  expect(list.querySelectorAll("svg title")).toHaveLength(0);

  // The space closes the `#` door, and an `@` query lists the planner's session row.
  await user.keyboard(" @Plan");
  const parties = await within(composerBox()).findByRole("listbox", {
    name: BOX_PICKER_NAMES.label,
  });
  const session = await within(parties).findByRole("option", { name: /^Planner/ });
  expect(parties.querySelectorAll("[title]")).toHaveLength(0);
  expect(parties.querySelectorAll("svg title")).toHaveLength(0);
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
  const parties = await within(composerBox()).findByRole("listbox", {
    name: BOX_PICKER_NAMES.label,
  });
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

// The reviewer's finding 6079442155, its first thread: the box's list, off screen, of who its
// comment is delivered to reads each party by its name as the page draws one (`sessionShown`) -
// a session the registry lists by its title, one it does not list now (a session that has
// stopped) by the page's word for an agent nothing names, and a role by its own name - and never
// by a short address such as `session:old-revi…`, which names nobody.
test("the box's list of who a comment is delivered to reads a stopped session by the page's word, never its short address", async () => {
  const user = userEvent.setup();
  renderPage();
  await user.click(writeField());
  await user.paste(
    `${PLANNER_SEED} [@${STOPPED_REVIEWER.title}](dispatch://agent/${STOPPED_REVIEWER.session_id}) [@planner](dispatch://role/planner) status please`
  );
  const mentioned = await within(composerBox()).findByRole("list", {
    name: CONTROL_LABELS.composer_mentioned,
  });
  expect(
    within(mentioned)
      .getAllByRole("listitem")
      .map((item) => item.textContent)
  ).toEqual(["Planner", CONTROL_LABELS.unnamed_author, "planner"]);
});

// Decision 548: the box's picker names the image a party's row draws its agent as (`RowAgent`) by
// the words the row draws for that agent (`sessionShown`). The row of a stopped role whose last
// session nobody titled draws the page's word for an agent nothing names, and names its image so
// too - never by the session's short form, an address that names nobody - while its detail still
// draws that word and then the role's own words after it.
test("the box's picker names the image of a session nobody titled by the page's word, never its short form", async () => {
  const user = userEvent.setup();
  (
    api.getIssueCollaborators as unknown as Mock<typeof api.getIssueCollaborators>
  ).mockResolvedValue(issueCollaborators({ agents: [UNTITLED_TESTER] }));
  const { client } = renderPage();
  act(() => client.setQueryData(["agents"], [plannerAgent()]));
  await user.click(writeField());
  await user.keyboard("@");
  const parties = await within(composerBox()).findByRole("listbox", {
    name: BOX_PICKER_NAMES.label,
  });
  const agent = await waitFor(() => {
    const found = parties.querySelector(`[data-agent-name="${UNTITLED_TESTER.session_id}"]`);
    if (found === null) throw new Error("the picker draws no row of the tester's role");
    return found;
  });
  const image = agent.closest('[role="img"]');
  const row = agent.closest('[role="option"]');
  expect({
    drawn: row?.textContent ?? "",
    named: image?.getAttribute("aria-label"),
  }).toEqual({
    drawn: `${UNTITLED_ROLE.token}${CONTROL_LABELS.unnamed_author} · ${UNTITLED_ROLE.role} · ${BOX_PICKER_LINES.stopped_role}`,
    named: CONTROL_LABELS.unnamed_author,
  });
  // What a screen reader reads the row by, which takes the image's name in place of its words.
  expect(row).not.toHaveAccessibleName(new RegExp(UNTITLED_ROLE.short, "u"));
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
  const edit = within(composerBox()).getByRole("textbox", { name: CONTROL_LABELS.composer_edit });
  await waitFor(() => expect(edit).toHaveFocus());
  expect(writeField().closest("[hidden]")).not.toBeNull();
  expect(writeField().value).toBe("Half a thought");
  await user.keyboard("{Escape}");
  await waitFor(() => expect(page.writer().editing).toBeUndefined());
  expect(writeField().closest("[hidden]")).toBeNull();
  expect(writeField().value).toBe("Half a thought");
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
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent(REFUSED_INPUT)
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
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent(REFUSED_INPUT)
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
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent(REFUSED_INPUT)
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
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent(REFUSED_INPUT)
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
    expect(within(composerBox()).getByRole("alert")).toHaveTextContent(REFUSED_INPUT)
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
// so Send refuses the reply before it goes, in the page's one sentence that says so and where the
// box sends the draft instead (`COMPOSER_ANSWER_WORDS.deleted_reply`), which names the box's
// control that writes on the issue, on a line under the box that Send's refusal names; neither
// Send nor Enter sends anything, and the words and the target stay.
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

  const refused = COMPOSER_ANSWER_WORDS.deleted_reply.comment;
  expect(refused).toContain(`press ${CONTROL_LABELS.write_on_issue} `);
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

// The reviewer's finding 6079442155, its second thread: why Send refuses, drawn once the reader
// presses it, is said of the address the press was made at. A row's `r` moves the box to another
// address, where the reader pressed nothing, so no reason is drawn there, nor once the box's own
// end of the reply brings it back, though Send refuses the empty draft for the same reason at
// each - until Send is pressed again. Review 6081271841, its first thread: an edit of the reader's
// comment is another address too, which the edit line writes while the write line waits under
// it, hidden, so a reason pressed on the write line is not drawn there once Escape ends the edit.
test("Send's reason drawn after a press goes when the box moves to another target", async () => {
  const user = userEvent.setup();
  const { empty } = COMPOSER_ANSWER_WORDS.send_refusals;
  const send = () => within(composerBox()).getByRole("button", { name: CONTROL_LABELS.send });
  const page = renderPage();
  await user.click(send());
  await waitFor(() => expect(send()).toHaveAccessibleDescription(empty));

  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("bob", "comment")));
  await waitFor(() => expect(writeField()).toHaveFocus());
  expect(send()).toHaveAccessibleDescription("");
  expect(composerBox().textContent ?? "").not.toContain(empty);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(targetWords()).toBeNull());
  expect(send()).toHaveAccessibleDescription("");
  expect(composerBox().textContent ?? "").not.toContain(empty);

  await user.click(send());
  await waitFor(() => expect(send()).toHaveAccessibleDescription(empty));

  await editHoldingFrames(page, "comment-2", "My own words");
  await waitFor(() =>
    expect(
      within(composerBox()).getByRole("textbox", { name: CONTROL_LABELS.composer_edit })
    ).toHaveFocus()
  );
  await user.keyboard("{Escape}");
  await waitFor(() => expect(page.writer().editing).toBeUndefined());
  expect(send()).toHaveAccessibleDescription("");
  expect(composerBox().textContent ?? "").not.toContain(empty);
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

// The ruling on the reviewer's finding 6079442155, its eighth thread: the state mark before an
// agent the target line names is named in the page's words (`TARGET_STATE_WORDS`), never in
// `AgentName`'s own - running, with how long ago the registry heard it; unresponsive, with that
// and what the state means; not running, where the registry answers without it; and running
// state unknown, where the registry cannot be read.
test("the target line names an agent's state mark in the page's words, in each state", async () => {
  const user = userEvent.setup();
  const page = renderWithPlannerComment();
  await replyToPlanner(user, page);
  const markIn = async (state: string) => {
    const mark = await waitFor(() => {
      const found = composerBox().querySelector(
        `[data-composer-target] [data-agent-state="${state}"] [role="img"]`
      );
      if (found === null) throw new Error(`the target line draws no ${state} agent`);
      return found;
    });
    return mark.getAttribute("aria-label");
  };
  const named: Record<string, string | null> = {};
  for (const [state, agents] of [
    ["running", [plannerAgent()]],
    ["unresponsive", [plannerAgent({ state: "unresponsive" })]],
    ["not-running", []],
  ] as const) {
    act(() => page.client.setQueryData(["agents"], agents));
    named[state] = await markIn(state);
  }
  (api.listAgents as unknown as Mock<typeof api.listAgents>).mockRejectedValue(
    new ApiError(502, { code: "ENVOY_UNREACHABLE", error: "Envoy listener unreachable" })
  );
  await act(() => page.client.refetchQueries({ queryKey: ["agents"] }));
  named.unknown = await markIn("unknown");
  const { agent_running, agent_seen, agent_unresponsive_note } = CONTROL_LABELS;
  expect(named).toEqual({
    "not-running": CONTROL_LABELS.not_running,
    running: `${agent_running}${agent_seen}${JUST_NOW}`,
    unknown: CONTROL_LABELS.unchecked,
    unresponsive: `${CONTROL_LABELS.unresponsive}${agent_seen}${JUST_NOW}${agent_unresponsive_note}`,
  });
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

    // Refused: the page's own sentence in the page's red, never Dispatch's words, with a Retry
    // whose press leaves the caret in the field.
    await waitFor(() => expect(refuseUpload).toBeDefined());
    await act(async () =>
      refuseUpload?.(
        new ApiError(400, { code: "INVALID_INPUT", error: "The file is not allowed." })
      )
    );
    const refused = (
      await within(composerBox()).findByText(COMPOSER_ANSWER_WORDS.upload.other)
    ).closest('[role="alert"]');
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
    const late = within(composerBox()).getByText(COMPOSER_ANSWER_WORDS.late);
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
// `aria-current`; unfolded, the point's own row alone is marked, with the quiet ring. The reply's
// Fold, pressed by hand, puts the path back, and its Unfold the point.
test("the rows above a folded-away point carry the path's ring, and its own row the point's", async () => {
  const user = userEvent.setup();
  renderPage({}, branchOutline());
  // The page opens with bob's comment folded, the newest act two levels under it.
  expect(drawnPoint()).toEqual(pathThrough("comment-1"));
  expect(dotOf("comment-1")).toHaveClass(pagePathRingFolded);

  // Bob's comment unfolded and his reply still folded: both are on the path, each ringed as its
  // bullet is drawn.
  await user.click(within(rowOf("comment-1")).getByRole("button", { name: CONTROL_LABELS.unfold }));
  await waitFor(() => expect(drawnPoint()).toEqual(pathThrough("comment-1", "comment-1a")));
  expect(dotOf("comment-1")).toHaveClass(pagePathRing);
  expect(dotOf("comment-1a")).toHaveClass(pagePathRingFolded);

  const toggle = (name: string) =>
    user.click(within(rowOf("comment-1a")).getByRole("button", { name }));
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

/** Every string `value` holds, however deeply, in the order it holds them. */
function stringsOf(value: unknown): readonly string[] {
  if (typeof value === "string") return [value];
  if (typeof value === "object" && value !== null) return Object.values(value).flatMap(stringsOf);
  return [];
}

/** Each string of a list the page declares: the tables it draws unasked and those it draws in
 *  answer to the reader's act (`page-words.ts`). The box draws the words of no other list. */
const LISTED: readonly string[] = [...UNASKED_SOURCES.flatMap(stringsOf), ...ANSWER_PHRASES];

/** Whether `text` is nothing but `pieces`, one after another, with white space between them. */
function madeOf(text: string, pieces: readonly string[]): boolean {
  const reached = new Set([0]);
  for (let at = 0; at < text.length; at += 1) {
    if (!reached.has(at)) continue;
    if (/\s/u.test(text.charAt(at))) reached.add(at + 1);
    for (const piece of pieces) {
      if (piece !== "" && text.startsWith(piece, at)) reached.add(at + piece.length);
    }
  }
  return reached.has(text.length);
}

/** When the planner's session was last seen, as the case's picker lists it: one fixed moment, so
 *  its row's detail is words the case can name. */
const PLANNER_SEEN = Date.parse("2026-11-05T09:30:00Z");

/** A role of the issue no running session holds - the reviewer, by its name, its token and the
 *  title of the session that last held it, which the registry lists no longer - which the picker
 *  offers as not running (`stoppedRoleOptions`), and the issue's collaborators' record of it. */
const STOPPED_ROLE = {
  role: "reviewer",
  title: "Old reviewer",
  token: "legion-p-core-1-reviewer",
} as const;
const STOPPED_REVIEWER: IssueCollaboratorAgent = {
  kind: "session",
  last_at: null,
  legion: {
    replaced_at: null,
    replaced_by: null,
    role: STOPPED_ROLE.role,
    role_token: STOPPED_ROLE.token,
    started_at: "2026-10-26T02:00:00Z",
  },
  reasons: [],
  session_id: "old-reviewer",
  title: STOPPED_ROLE.title,
};

/** A second role of the issue no running session holds - the tester, by its name, its token and
 *  the short form of the session that last held it (`short`) - whose session nobody titled. The
 *  picker's row draws that session by the page's word for an agent nothing names, with no mark of
 *  a name somebody gave around it, so its state mark stands inside the image the row draws the
 *  agent as (`RowAgent`) and in no part marked as an author's. The box names that image by the
 *  same word (decision 548), so the short form is no data of the box's: the row never draws it. */
const UNTITLED_ROLE = {
  role: "tester",
  short: "session:untitled…",
  token: "legion-p-core-1-tester",
} as const;
const UNTITLED_TESTER: IssueCollaboratorAgent = {
  kind: "session",
  last_at: null,
  legion: {
    replaced_at: null,
    replaced_by: null,
    role: UNTITLED_ROLE.role,
    role_token: UNTITLED_ROLE.token,
    started_at: "2026-10-26T03:00:00Z",
  },
  reasons: [],
  session_id: "untitled-tester",
  title: null,
};

/** A comment the search answers the `#` door's query with, whose row opens its parts. */
const RELAY_HIT: SearchResult = {
  href: "/issues/CORE-2/comments/comment-7",
  id: "comment-7",
  kind: "comment",
  owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
  rank: 0.1,
  snippet: "The relay drops a reply",
};

/** A question the search answers the same query with, whose row the picker heads with the page's
 *  word for a question. */
const QUESTION_HIT: SearchResult = {
  href: "/issues/CORE-2/asks/ask-7",
  id: "ask-7",
  kind: "ask",
  owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
  rank: 0.2,
  snippet: "Should the relay retry",
};

/** The words the page never names a kind by (decision 527, root R1): a question is never an ask,
 *  and a part of a record never a block. */
const KIND_WORDS_REFUSED = /\b(?:asks?|blocks?)\b/iu;

/** What the case below draws the box's words around: the issue's key, the session a draft
 *  mentions by its title and its id, the reader's own words drawn beside the box - a kept draft
 *  and an edit - and how many hours Dispatch keeps a write's key, which a stopped send's sentence
 *  names. No list of the page holds them, nor should. */
const DRAWN_DATA = [
  KEY,
  "Planner",
  PLANNER.id,
  KEPT_WORDS,
  "My own words and more",
  String(KEY_HOURS),
];

/** What Dispatch says in the case below of a write it refused, a list it could not read and a
 *  statement it could not place: the box says the page's own sentence in its place, never these,
 *  and each is a fault wherever the box draws it. */
const DISPATCH_SAID = [
  "Dispatch said no to this.",
  "The comment was refused.",
  "matches 2 places",
  "The file is not allowed.",
  "Name the version this replaces.",
  "The document moved on.",
  "the page broke",
  "already made",
  "the server is down",
  "Envoy is refilling its list.",
  "Envoy listener unreachable",
] as const;

/** How a fault names an element: its tag, and its role where it has one. */
function elementName(element: Element): string {
  const role = element.getAttribute("role");
  return `${element.tagName.toLowerCase()}${role === null ? "" : `[role="${role}"]`}`;
}

/** The text of each element `ids` names, as `aria-labelledby` and `aria-describedby` name them. */
function textsNamed(ids: string | null): string[] {
  return (ids ?? "").split(/\s+/u).flatMap((id) => {
    const line = id === "" ? null : document.getElementById(id);
    return line === null ? [] : [line.textContent ?? ""];
  });
}

/**
 * Each thing the box tells a reader now that is neither a word or a sentence of the page's lists
 * nor made of them and of `DRAWN_DATA` and `data`, and each place it says Dispatch's own words
 * (`DISPATCH_SAID`), each named with `state`. The box is marked whole as words the page did not
 * write (`AUTHOR_TEXT_ATTRIBUTE`), since its field holds the reader's, so a part something inside
 * it marks so holds words that are not the page's. It reads nothing hidden, and of the rest, in
 * one pass over the texts and one over the elements:
 * - every text the box draws, but the field's own and a text inside a part marked so;
 * - every image's `aria-label`, wherever the image stands: inside a part marked so, as the state
 *   mark beside a name somebody gave an agent stands, and inside another image, as the state mark
 *   of a session with no title stands inside the image a row of the reference picker draws its
 *   agent as (`RowAgent`), where the mark still carries the page's words though no screen reader
 *   reads them;
 * - and of every element that stands outside both a part marked so and an image, its
 *   `aria-label`, the text its `aria-labelledby` names, its `title`, its placeholder and the text
 *   of every line its `aria-describedby` names.
 */
function unlisted(state: string, data: readonly string[] = []): string[] {
  const box = composerBox();
  const drawnData = [...DRAWN_DATA, ...data];
  const pieces = [...LISTED, ...drawnData];
  const found: string[] = [];
  const read = (where: string, said: string) => {
    if (said.trim() === "") return;
    const dispatch = DISPATCH_SAID.find((words) => said.includes(words));
    // A short form or an id a row draws is data (decision 350), which may hold any word; the
    // page's own words around it may not name a kind the page calls otherwise.
    const pageWords = drawnData.reduce((text, piece) => text.replaceAll(piece, " "), said);
    const kindWord = KIND_WORDS_REFUSED.exec(pageWords)?.[0];
    if (dispatch !== undefined) {
      found.push(`${state}: ${where} holds Dispatch's own words "${dispatch}"`);
    } else if (kindWord !== undefined) {
      found.push(
        `${state}: ${where} names a kind "${kindWord}", which the page does not: "${said}"`
      );
    } else if (!madeOf(said, pieces)) {
      found.push(`${state}: ${where} "${said}"`);
    }
  };
  const notThePage = (element: Element) =>
    element.closest("[hidden]") !== null || element.closest(`[${AUTHOR_TEXT_ATTRIBUTE}]`) !== box;
  const walker = document.createTreeWalker(box, NodeFilter.SHOW_TEXT);
  for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
    const holder = text.parentElement;
    if (holder === null || notThePage(holder) || holder.closest("textarea") !== null) continue;
    read("text", text.nodeValue ?? "");
  }
  for (const element of [box, ...box.querySelectorAll("*")]) {
    if (element.closest("[hidden]") !== null) continue;
    const named = elementName(element);
    // Every name is read of an element outside both a part marked so and an image; of any other,
    // an image's `aria-label` alone.
    const everyName =
      !notThePage(element) && (element.parentElement?.closest('[role="img"]') ?? null) === null;
    const label = element.getAttribute("aria-label");
    if (label !== null && (everyName || element.getAttribute("role") === "img")) {
      read(`the name of ${named}`, label);
    }
    if (!everyName) continue;
    for (const said of textsNamed(element.getAttribute("aria-labelledby"))) {
      read(`the name of ${named}`, said);
    }
    for (const attribute of ["title", "placeholder"]) {
      const said = element.getAttribute(attribute);
      if (said !== null) read(`the ${attribute} of ${named}`, said);
    }
    for (const said of textsNamed(element.getAttribute("aria-describedby"))) {
      read(`the description of ${named}`, said);
    }
  }
  return found;
}

/** Waits until the box draws `words`, the sign that it has reached the state a case drives it
 *  to, so no state is read before it is drawn. */
async function drawn(words: string): Promise<void> {
  await waitFor(() => expect(composerBox().textContent ?? "").toContain(words));
}

/** The planner's thread as the issue's messages read sends it: bob's message to the session the
 *  registry lists as Planner, so a reply to it goes to that session in its thread's mode. */
function plannerThread(): MessageRead {
  return {
    message: {
      ...messageCorrectionFields(),
      author: BOB,
      body: "A message",
      broadcast_id: null,
      created_at: "2026-11-05T00:00:00Z",
      deliveries: [],
      id: "message-1",
      in_reply_to: null,
      issue_key: KEY,
      reply_block: null,
      target: `session:${PLANNER.id}`,
    },
    replies: [],
  };
}

/** The builder's session, which the registry lists beside the planner's as a second session a
 *  notice names, advertising `capabilities`. */
function builderAgent(capabilities: string[]): Agent {
  return plannerAgent({
    capabilities,
    roles: ["builder"],
    session_id: "builder-session",
    title: "Builder",
  });
}

/** A mention of the builder's session (`builderAgent`), as the picker writes one. */
const BUILDER_MENTION = "[@Builder](dispatch://agent/builder-session)";

/** A session the registry does not list - the reviewer's old one, which has stopped - as the
 *  author of a comment. */
const STOPPED_AUTHOR: Actor = { id: STOPPED_REVIEWER.session_id, kind: "session" };

/** The outline with the planner's comment and one of the stopped session's after its others. */
function agentsOutline(): TreeNode {
  const root = outline();
  return {
    ...root,
    children: [
      ...root.children,
      commentNode("comment-3", PLANNER, "The relay is fixed"),
      commentNode("comment-4", STOPPED_AUTHOR, "Reviewed"),
    ],
  };
}

/** How long ago the state mark says a session the registry heard from just now was seen
 *  (`relativeTimeLabel`): a time, and no word of the page's lists. */
const JUST_NOW = "just now";

// R22 and decision 527: what the box says in answer to the reader's act is a sentence of a list
// the page declares, and so is every other string it draws or a screen reader reads of it - a
// name, a description, the picker it opens. The case drives the box through each state that
// draws words - a comment a reload ended; at rest, Send pressed there, writing a question, aimed
// at a reply with a draft kept on the issue; aimed at a running agent's comment and at one of an
// agent that is not running, each named with its state mark; the picker at each of its doors
// (`@`, `#` with and without a query, `[[` with and without a match), with a role whose session
// nobody titled among its rows, a row's parts opened with none, the agent list refilling and
// unreachable; drafts kept for a comment, a message and a statement, and those drafts once their
// targets are gone; a reply whose post is deleted while it is open; a mention of one session and
// of two that takes the plain mode, that does not take the mode the draft asks for with another
// to take instead and, for one, with none, and that takes no mode at all because its list holds
// nothing, holds no mode this page knows or, for two, some of each; a delivery command with
// nothing after it; a send refused with a code the box has no sentence for and with one it has,
// and the same held for the issue while the box replies elsewhere; a refused reply held for its
// comment while the box writes on the issue, then once the comment is no longer listed and once
// it is deleted; a statement held twice; an edit and one over a newer comment; an upload out,
// refused with a code the box has no sentence for and with each it has, failed, stored with no
// answer, and stopped, with a Retry under its key and a day later; and a send past its deadline,
// stopping, stopped, a day later with its key expired, and carried to the issue - and reads every
// text, name and description of the box in each (`unlisted`). The lists are the page's tables
// (`UNASKED_SOURCES`, `ANSWER_PHRASES`). A text that is in no list, nor made of the lists' parts
// around the names and words the case drew it with, fails the case by its words, and so do
// Dispatch's own text of a refusal wherever the box draws it and a kind named by a word the page
// does not use (`KIND_WORDS_REFUSED`). Send's reason is drawn only once the reader presses Send
// or Enter: at rest no line and no description says it.
test("every string the box draws, in each of its states, is a word or sentence of the page's lists", async () => {
  const found: string[] = [];
  const user = userEvent.setup();
  const createComment = stub(
    spyOn(api, "createComment").mockRejectedValue(
      new ApiError(400, { code: "INVALID_INPUT", error: "The comment was refused." })
    )
  );
  const refusals = COMPOSER_ANSWER_WORDS.send_refusals;
  const send = () => within(composerBox()).getByRole("button", { name: CONTROL_LABELS.send });

  // A comment still out when the page reloads, more than five minutes after its press
  // (`KEPT_SEND_RESUME_MS`): the reload ended it before Dispatch confirmed it, and the page the
  // reload loads draws it refused (`SendInterruptedError`). The tab keeps a write for the reader
  // signed in, so both pages are signed in as the reader; the case starts here, while the tab
  // keeps nothing else of the reader's, so the page the reload loads takes up this send alone.
  createComment.mockImplementationOnce(() => new Promise<CommentWithTrees>(() => {}));
  const reloaded = renderPage({}, outline(), signedInAsReader(newQueryClient()));
  await user.click(writeField());
  await user.keyboard("Agreed");
  jest.useFakeTimers();
  try {
    await userOnFakeClock().keyboard("{Enter}");
    await jump(KEPT_SEND_RESUME_MS + 1);
    endAsAReload(() => reloaded.view.unmount());
    renderPage({}, outline(), signedInAsReader(newQueryClient()));
    const { interrupted } = COMPOSER_ANSWER_WORDS.stopped;
    await drawn(`${interrupted.before}${KEY_HOURS}${interrupted.after}`);
    found.push(...unlisted("a comment a reload ended"));
  } finally {
    realClock();
  }
  cleanup();

  // At rest, Send refuses the empty draft and says nothing of why until the reader presses it;
  // the press says it on the box's own line, which Send names as its description.
  let page = renderPage();
  await waitFor(() => expect(send()).toHaveAttribute("aria-disabled", "true"));
  expect(composerBox().textContent ?? "").not.toContain(refusals.empty);
  expect(send()).toHaveAccessibleDescription("");
  found.push(...unlisted("at rest"));
  await user.click(send());
  await drawn(refusals.empty);
  expect(send()).toHaveAccessibleDescription(refusals.empty);
  found.push(...unlisted("at rest, Send pressed"));

  // Writing a question: the reason changed with the kind, so nothing says it until Enter.
  const question = composerBox().querySelector<HTMLElement>('[data-composer-kind="ask"]');
  if (question === null) throw new Error("the box drew no Question chip");
  await user.click(question);
  await waitFor(() => expect(composerBox().textContent ?? "").not.toContain(refusals.empty));
  expect(composerBox().textContent ?? "").not.toContain(refusals.emptyQuestion);
  found.push(...unlisted("writing a question"));
  await user.click(writeField());
  await user.keyboard("{Enter}");
  await drawn(refusals.emptyQuestion);
  found.push(...unlisted("writing a question, Enter pressed"));
  await user.keyboard("Half a thought");
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(keptLines()).toEqual([KEPT_FOR_ISSUE]));
  found.push(...unlisted("a reply, with a draft kept on the issue"));
  cleanup();

  // The target line naming an agent, with the mark of its state: the planner, running and seen
  // just now, and a session the registry no longer lists, not running.
  const ownComments = reads.comments;
  reads.comments = [
    ...ownComments,
    commentRecord("comment-3", PLANNER, "The relay is fixed"),
    commentRecord("comment-4", STOPPED_AUTHOR, "Reviewed"),
  ];
  page = renderPage({}, agentsOutline());
  await page.press(user, "comment-3", "r");
  await waitFor(() => expect(targetWords()).toBe(replyingTo("Planner", "comment")));
  found.push(...unlisted("a reply to a running agent's comment", [JUST_NOW]));
  await page.press(user, "comment-4", "r");
  await waitFor(() =>
    expect(targetWords()).toBe(replyingTo(CONTROL_LABELS.unnamed_author, "comment"))
  );
  found.push(...unlisted("a reply to a comment of an agent that is not running"));
  cleanup();
  reads.comments = ownComments;

  // The picker at each door, with the planner's session and role, the reviewer's role and the
  // tester's, which no running session holds, the tester's session titled by nobody, and the
  // comment the search finds; a row's parts opened with none; and the agent list refilling, then
  // unreachable.
  const listAgents = api.listAgents as unknown as Mock<typeof api.listAgents>;
  const collaborators = api.getIssueCollaborators as unknown as Mock<
    typeof api.getIssueCollaborators
  >;
  const planner = plannerAgent({ last_seen: PLANNER_SEEN });
  listAgents.mockResolvedValue([planner]);
  collaborators.mockResolvedValue(
    issueCollaborators({ agents: [STOPPED_REVIEWER, UNTITLED_TESTER] })
  );
  stub(spyOn(api, "listAgentArtifacts").mockResolvedValue([]));
  const relay = nodeRouteFromHref(RELAY_HIT.href);
  const asked = nodeRouteFromHref(QUESTION_HIT.href);
  if (relay === undefined || asked === undefined) throw new Error("a hit names no address");
  const listed = [
    planner.dir,
    new Date(PLANNER_SEEN).toLocaleString(),
    // How long ago the planner's row's state mark says it was seen.
    relativeTimeLabel(new Date(PLANNER_SEEN), Date.now()) ?? "",
    "planner",
    STOPPED_ROLE.token,
    STOPPED_ROLE.role,
    STOPPED_ROLE.title,
    UNTITLED_ROLE.token,
    UNTITLED_ROLE.role,
    RELAY_HIT.snippet,
    shortForm(relay),
    QUESTION_HIT.snippet,
    shortForm(asked),
  ];
  page = renderPage();
  act(() => page.client.setQueryData(["agents"], [planner]));
  const picker = () =>
    within(composerBox()).findByRole("listbox", { name: BOX_PICKER_NAMES.label });
  const opened = async (door: string) => {
    await user.clear(writeField());
    await user.paste(door);
    await picker();
  };
  await user.click(writeField());
  await opened("@");
  await drawn(BOX_PICKER_LINES.stopped_role);
  await drawn(BOX_PICKER_LINES.instruction);
  found.push(...unlisted("the picker at @", listed));
  await user.click(
    within(await picker()).getByRole("button", { name: BOX_PICKER_NAMES.parts.documents })
  );
  await drawn(BOX_PICKER_LINES.parts_none.documents);
  found.push(...unlisted("the picker with an agent's documents opened, and none", listed));
  listAgents.mockRejectedValue(
    new ApiError(503, { code: "ENVOY_REFILLING", error: "Envoy is refilling its list." })
  );
  await act(() => page.client.refetchQueries({ queryKey: ["agents"] }));
  await drawn(BOX_PICKER_LINES.agents.refilling);
  found.push(...unlisted("the picker while the agent list refills", listed));
  listAgents.mockRejectedValue(
    new ApiError(502, { code: "ENVOY_UNREACHABLE", error: "Envoy listener unreachable" })
  );
  await act(() => page.client.refetchQueries({ queryKey: ["agents"] }));
  await drawn(BOX_PICKER_LINES.agents.unreachable);
  found.push(...unlisted("the picker while the agent list cannot be read", listed));
  listAgents.mockResolvedValue([planner]);
  await act(() => page.client.refetchQueries({ queryKey: ["agents"] }));
  await opened("#");
  await drawn(BOX_PICKER_LINES.stopped_role);
  found.push(...unlisted("the picker at #", listed));
  searchRead.mockResolvedValue(searchResponse([RELAY_HIT, QUESTION_HIT]));
  await opened("#relay");
  await drawn(RELAY_HIT.snippet);
  await drawn(QUESTION_HIT.snippet);
  found.push(...unlisted("the picker at # with a query", listed));
  await opened("[[");
  await drawn(BOX_PICKER_LINES.instruction);
  found.push(...unlisted("the picker at [[", listed));
  searchRead.mockResolvedValue(searchResponse([]));
  await opened("[[zzqq");
  await drawn(BOX_PICKER_LINES.empty);
  found.push(...unlisted("the picker at [[ with no match", listed));
  cleanup();
  listAgents.mockResolvedValue([plannerAgent()]);
  collaborators.mockResolvedValue(issueCollaborators());

  // Drafts kept for a comment, a message and a statement; then those targets gone - the comment
  // no longer listed, the message deleted, the statement rewritten - with Use it here, and with
  // Use it here waiting for the field to be empty.
  page = renderPage({}, messageOutline());
  await keepFor(user, page, "comment-1", KEPT_WORDS);
  await keepFor(user, page, "message-1", KEPT_WORDS);
  await keepFor(user, page, "spec#b-r1", KEPT_WORDS);
  await waitFor(() => expect(keptLines()).toHaveLength(3));
  found.push(...unlisted("drafts kept for a comment, a message and a statement"));
  const gone = outlineRewritten(STATEMENT_REWRITTEN, outlineWithout("comment-1", messageOutline()));
  page.redrawOutline({
    ...gone,
    children: gone.children.map((child) =>
      child.id === "message-1" ? { ...child, deletedAt: hour(4), deletedBy: BOB } : child
    ),
  });
  await waitFor(() => expect(goneDrafts()).toHaveLength(3));
  found.push(...unlisted("drafts whose targets are gone"));
  await user.click(writeField());
  await user.keyboard("Words of my own");
  await drawn(COMPOSER_ANSWER_WORDS.gone_reply.after.gone.waits);
  found.push(...unlisted("drafts whose targets are gone, over words of the field's own"));
  cleanup();

  // A reply open when its comment is deleted.
  page = renderPage();
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("Ship it.");
  reads.comments = [
    commentRecord("comment-1", BOB, "", { deleted_at: hour(4), deleted_by: BOB }),
    commentRecord("comment-2", ALICE, "My own words"),
  ];
  const drawnNow = outline();
  page.redrawOutline({
    ...drawnNow,
    children: drawnNow.children.map((child) =>
      child.id === "comment-1" ? { ...child, deletedAt: hour(4), deletedBy: BOB } : child
    ),
  });
  await act(() => page.client.invalidateQueries({ queryKey: ["comments", KEY] }));
  await drawn(COMPOSER_ANSWER_WORDS.deleted_reply.comment);
  found.push(...unlisted("a reply to a deleted comment"));
  cleanup();
  reads.comments = [
    commentRecord("comment-1", BOB, "Earlier words"),
    commentRecord("comment-2", ALICE, "My own words"),
  ];

  // A mention of the planner while it advertises Aside alone, which a plain message goes in; BTW
  // alone, which takes no plain message; nothing; and nothing this page knows. Then the message
  // in BTW while it advertises Aside alone, and while it advertises Send alone, which leaves no
  // mode to take instead.
  const { delivery } = COMPOSER_ANSWER_WORDS;
  page = renderPage();
  const advertises = (capabilities: string[]) =>
    act(() => page.client.setQueryData(["agents"], [plannerAgent({ capabilities })]));
  await advertises(["aside"]);
  await user.click(writeField());
  await user.paste(`${PLANNER_SEED} status please`);
  await drawn(delivery.plain.before);
  found.push(...unlisted("a mention of a session that takes Aside alone"));
  await advertises(["btw"]);
  await drawn(delivery.unsupported.instead.before);
  found.push(...unlisted("a mention of a session that takes BTW alone"));
  await advertises([]);
  await drawn(delivery.deaf.unlisted.one);
  found.push(...unlisted("a mention of a session that advertises nothing"));
  await advertises(["telepathy"]);
  await drawn(delivery.deaf.unknown.one);
  found.push(...unlisted("a mention of a session that advertises no mode this page knows"));
  await advertises(["aside"]);
  await user.keyboard("{Control>}a{/Control}{Backspace}");
  await user.paste(`/btw ${PLANNER_SEED} status please`);
  await drawn(`${delivery.unsupported.instead.before}/aside`);
  found.push(...unlisted("a mention of a session in a mode it does not advertise"));
  await advertises(["steer"]);
  await drawn(delivery.unsupported.after);
  found.push(...unlisted("a mention of a session in a mode it does not advertise, and no other"));
  cleanup();

  // Then two sessions, the planner and the builder: both taking Aside alone, which a plain
  // message goes in, and the message in BTW; both advertising nothing; both nothing this page
  // knows; and one of each, a cause of each kind.
  page = renderPage();
  const bothAdvertise = (planner: string[], builder: string[]) =>
    act(() =>
      page.client.setQueryData(
        ["agents"],
        [plannerAgent({ capabilities: planner }), builderAgent(builder)]
      )
    );
  const both = `Planner${COMPOSER_ANSWER_WORDS.titles_joiner}Builder`;
  await bothAdvertise(["aside"], ["aside"]);
  await user.click(writeField());
  await user.paste(`${PLANNER_SEED} ${BUILDER_MENTION} status please`);
  await drawn(`${both}${delivery.plain.after.many}`);
  found.push(...unlisted("a mention of two sessions that take Aside alone", ["Builder"]));
  await user.keyboard("{Control>}a{/Control}{Backspace}");
  await user.paste(`/btw ${PLANNER_SEED} ${BUILDER_MENTION} status please`);
  await drawn(`${both}${delivery.unsupported.before.many}`);
  found.push(...unlisted("a mention of two sessions in a mode they do not advertise", ["Builder"]));
  await bothAdvertise([], []);
  await drawn(`${both}${delivery.deaf.unlisted.many}`);
  found.push(...unlisted("a mention of two sessions that advertise nothing", ["Builder"]));
  await bothAdvertise(["telepathy"], ["telepathy"]);
  await drawn(`${both}${delivery.deaf.unknown.many}`);
  found.push(
    ...unlisted("a mention of two sessions that advertise no mode this page knows", ["Builder"])
  );
  await bothAdvertise([], ["telepathy"]);
  await drawn(`${both}${delivery.deaf.mixed}`);
  found.push(
    ...unlisted("a mention of one session that advertises nothing and one no mode known", [
      "Builder",
    ])
  );
  cleanup();

  // A reply in the planner's thread that holds a delivery command and nothing after it, which
  // Enter is refused for.
  page = renderPage({}, messageOutline());
  await act(() => page.client.setQueryData(["issue-messages", KEY], [plannerThread()]));
  await page.press(user, "message-1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("/btw ");
  expect(composerBox().textContent ?? "").not.toContain(refusals.afterCommand.before);
  await user.keyboard("{Enter}");
  await drawn(refusals.afterCommand.before);
  found.push(...unlisted("a reply in a thread with a command and nothing after it"));
  cleanup();

  // A comment Dispatch refused with a code the box has no sentence for, and again, at Retry, as
  // input it cannot take; then held for the issue while a Reply aims the box at bob's comment;
  // and a comment on a statement refused because its words stand twice.
  createComment.mockRejectedValueOnce(
    new ApiError(400, { code: "SOMETHING_NEW", error: "Dispatch said no to this." })
  );
  page = renderPage();
  await user.click(writeField());
  await user.keyboard("Agreed{Enter}");
  // The refusal, whatever it says: what it says is the case's to read.
  await within(composerBox()).findByRole("alert");
  found.push(...unlisted("a comment refused with a code the box has no sentence for"));
  await user.click(within(composerBox()).getByRole("button", { name: CONTROL_LABELS.retry }));
  await drawn(REFUSED_INPUT);
  found.push(...unlisted("a refused comment"));
  await page.press(user, "comment-1", "r");
  await drawn(COMPOSER_ANSWER_WORDS.elsewhere.address.before);
  found.push(...unlisted("a refused comment held for the issue"));
  cleanup();

  // A reply Dispatch refused, which the box's own end leaves with bob's comment while the issue
  // keeps a comment of the reader's: the box on the issue says the refusal was written as a
  // reply; then once the outline no longer lists bob's comment, and once it draws it deleted,
  // what became of it.
  page = renderPage();
  await user.click(writeField());
  await user.keyboard("Half a thought");
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("My reply to bob{Enter}");
  await drawn(REFUSED_INPUT);
  await user.click(
    within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
  );
  const { elsewhere } = COMPOSER_ANSWER_WORDS;
  await drawn(elsewhere.reply);
  found.push(
    ...unlisted("a refused reply held for bob's comment while the box writes on the issue")
  );
  page.redrawOutline(outlineWithout("comment-1"));
  await drawn(elsewhere.gone.comment.gone);
  found.push(...unlisted("a refused reply held for a comment no longer listed"));
  const deletedOne = outline();
  page.redrawOutline({
    ...deletedOne,
    children: deletedOne.children.map((child) =>
      child.id === "comment-1" ? { ...child, deletedAt: hour(4), deletedBy: BOB } : child
    ),
  });
  await drawn(elsewhere.gone.comment.deleted);
  found.push(...unlisted("a refused reply held for a comment since deleted"));
  cleanup();
  createComment.mockRejectedValue(
    new ApiError(409, { code: "TARGET_AMBIGUOUS", error: "matches 2 places" })
  );
  page = renderPage();
  await page.press(user, "spec#b-r1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("Agreed{Enter}");
  await drawn(ANSWER_LINES.statement_twice);
  found.push(...unlisted("a statement held twice"));
  cleanup();

  // The reader's own comment edited, and edited again once the comment reads other words than
  // the edit was written over.
  page = renderPage();
  await editHoldingFrames(page, "comment-2", "My own words");
  await waitFor(() => expect(targetWords()).not.toBeNull());
  found.push(...unlisted("an edit"));
  await user.keyboard(" and more");
  await user.keyboard("{Escape}");
  await waitFor(() => expect(page.writer().editing).toBeUndefined());
  await editHoldingFrames(page, "comment-2", "My newer words");
  await drawn(COMPOSER_ANSWER_WORDS.changed_edit);
  found.push(...unlisted("an edit over a newer comment"));
  cleanup();

  // A file pasted into the draft: out; refused as input Dispatch cannot take, which the box has
  // no upload sentence for, for a version it does not name and for a document that moved on,
  // which it has; failed with no answer of Dispatch's; and stored with no answer.
  let refuseUpload: ((refusal: Error) => void) | undefined;
  const uploadArtifact = stub(
    spyOn(api, "uploadArtifact")
      .mockImplementationOnce(() => {
        const { promise, reject } = Promise.withResolvers<UploadResult>();
        refuseUpload = reject;
        return promise;
      })
      .mockRejectedValueOnce(
        new ApiError(428, {
          code: "PRECONDITION_REQUIRED",
          error: "Name the version this replaces.",
        })
      )
      .mockRejectedValueOnce(
        new ApiError(409, { code: "PRECONDITION_FAILED", error: "The document moved on." })
      )
      .mockRejectedValueOnce(new Error("the page broke"))
      .mockRejectedValueOnce(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }))
  );
  const { upload } = COMPOSER_ANSWER_WORDS;
  const retryUpload = () =>
    user.click(within(composerBox()).getByRole("button", { name: CONTROL_LABELS.retry }));
  page = renderPage();
  await user.click(writeField());
  const file = new File(["# Notes"], "notes.md", { type: "text/markdown" });
  // user-event's paste carries text, never files.
  fireEvent.paste(writeField(), { clipboardData: { files: [file] } });
  await drawn(ANSWER_LINES.uploading);
  found.push(...unlisted("an upload out"));
  await waitFor(() => expect(refuseUpload).toBeDefined());
  await act(async () =>
    refuseUpload?.(new ApiError(400, { code: "INVALID_INPUT", error: "The file is not allowed." }))
  );
  await drawn(upload.other);
  found.push(...unlisted("an upload refused"));
  await retryUpload();
  await drawn(upload.refused.PRECONDITION_REQUIRED);
  found.push(...unlisted("an upload refused for the version it names"));
  await retryUpload();
  await drawn(upload.refused.PRECONDITION_FAILED);
  found.push(...unlisted("an upload refused for a document that moved on"));
  await retryUpload();
  await waitFor(() =>
    expect(composerBox().textContent ?? "").not.toContain(upload.refused.PRECONDITION_FAILED)
  );
  await drawn(upload.other);
  found.push(...unlisted("an upload failed"));
  await retryUpload();
  await drawn(ANSWER_LINES.uploaded_unanswered);
  found.push(...unlisted("an upload stored with no answer"));
  cleanup();

  // A file pasted into the draft whose upload the reader stopped while Dispatch was out of reach,
  // which a Retry still makes under its key; and a day later, once Dispatch has forgotten that
  // key, which a Retry cannot. Each attempt tells its request before it fails, as
  // `api.uploadArtifact` does, so the key is known to have carried one.
  uploadArtifact.mockImplementation((_owner, _input, bounds) => {
    bounds.sent?.({ method: "POST", path: `/api/v1/issues/${KEY}/artifacts` });
    return Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
  });
  const uploadsBefore = uploadArtifact.mock.calls.length;
  jest.useFakeTimers();
  try {
    const uploadClock = userOnFakeClock();
    page = renderPage();
    await uploadClock.click(writeField());
    // user-event's paste carries text, never files.
    fireEvent.paste(writeField(), { clipboardData: { files: [file] } });
    await untilAttempted(uploadArtifact, uploadsBefore + 1);
    await stopTheLateWrite(page.client);
    const { expired, unconfirmed } = upload.stopped;
    await drawn(`${unconfirmed.before}${KEY_HOURS}${unconfirmed.after}`);
    found.push(...unlisted("an upload stopped"));
    // One jump of the key's whole life: the stopped upload leaves no timer in it to step to.
    await jump(IDEMPOTENCY_KEY_TTL_MS);
    await uploadClock.click(
      within(composerBox()).getByRole("button", { name: CONTROL_LABELS.retry })
    );
    await jump(0);
    await drawn(expired);
    found.push(...unlisted("an upload stopped a day ago"));
  } finally {
    realClock();
  }
  cleanup();

  // A reply past its deadline, its attempt unanswered; stopping while that attempt is out;
  // stopped once the attempt ends with no verdict of Dispatch's; a day later, once Dispatch has
  // forgotten its key, retried; and carried to the issue by the target line's control. Each
  // attempt tells its request before anything goes out, as `api.createComment` does, so the key
  // is known to have carried one.
  const attempts: ((refusal: Error) => void)[] = [];
  createComment.mockImplementation((key, input, bounds) => {
    bounds.sent?.({ body: input, method: "POST", path: `/api/v1/issues/${key}/comments` });
    const { promise, reject } = Promise.withResolvers<CommentWithTrees>();
    attempts.push(reject);
    return promise;
  });
  page = renderPage();
  await page.press(user, "comment-1", "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await user.keyboard("My reply to bob");
  jest.useFakeTimers();
  const clocked = userOnFakeClock();
  try {
    await clocked.keyboard("{Enter}");
    await jump(SEND_DEADLINE_MS);
    await drawn(COMPOSER_ANSWER_WORDS.late);
    found.push(...unlisted("a late send"));
    await clocked.click(
      within(composerBox()).getByRole("button", { name: CONTROL_LABELS.stop_trying })
    );
    await drawn(ANSWER_LINES.stopping);
    found.push(...unlisted("a send stopping"));
    await act(async () =>
      attempts.at(-1)?.(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }))
    );
    await jump(0);
    await drawn(STOPPED_IN_BOX);
    found.push(...unlisted("a stopped send"));
    const { address, stopped } = COMPOSER_ANSWER_WORDS;
    // One jump of the key's whole life: the stopped send leaves no timer in it to step to.
    await jump(IDEMPOTENCY_KEY_TTL_MS);
    await clocked.click(within(composerBox()).getByRole("button", { name: CONTROL_LABELS.retry }));
    await jump(0);
    await drawn(stopped.expired);
    found.push(...unlisted("a send whose key expired"));
    await clocked.click(
      within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
    );
    await drawn(`${stopped.moved.before}${address.reply}${stopped.moved.after}`);
    found.push(...unlisted("a stopped send carried to the issue"));
  } finally {
    realClock();
  }

  expect(found).toEqual([]);
});

/** The sentences of `phrase`: each run up to a full stop, a question mark or an exclamation,
 *  whatever follows the mark - a stop with no space after it, as in `attempt., so`, ends a
 *  sentence as one before a space does. */
function sentencesIn(phrase: string): readonly string[] {
  return phrase.split(/(?<=[.!?])/u).filter((sentence) => /\p{L}/u.test(sentence));
}

/** Every answer `table` holds, each read whole: a sentence cut around a name - between `before`
 *  and `after`, and around `between` where it names two - read with a name in each cut and in
 *  every form each part takes, and every other string as it stands. */
function wholeAnswers(table: unknown): readonly string[] {
  if (typeof table === "string") return [table];
  if (typeof table !== "object" || table === null) return [];
  const { after, before, between, ...rest } = table as Record<string, unknown>;
  if (before === undefined || after === undefined) {
    return Object.values(table).flatMap(wholeAnswers);
  }
  const middles = between === undefined ? [""] : stringsOf(between).map((one) => `${one}NAME`);
  const cut = stringsOf(before).flatMap((head) =>
    middles.flatMap((middle) => stringsOf(after).map((tail) => `${head}NAME${middle}${tail}`))
  );
  return [...cut, ...Object.values(rest).flatMap(wholeAnswers)];
}

/** The warning the box draws of a mention of sessions that do not take the mode the draft asks
 *  for and do take another, read off the box in each form it takes: for one session and for
 *  two, with one mode to take instead, and for one with two. The box joins the warning and the
 *  modes to take instead into one sentence (`OutlineAnswers.delivery.unsupported`), so no table
 *  holds that sentence whole and `wholeAnswers` cannot build it. A draft of the box reaches no
 *  other form: two modes to take instead are offered only to a reply in a thread whose mode is
 *  Send, and a thread names one session. */
async function offeredModeWarnings(): Promise<string[]> {
  const user = userEvent.setup();
  const { instead } = COMPOSER_ANSWER_WORDS.delivery.unsupported;
  const warnings: string[] = [];
  for (const { agents, reply, sign, words } of [
    {
      agents: [plannerAgent({ capabilities: ["aside"] })],
      reply: false,
      sign: `${instead.before}/aside`,
      words: `/btw ${PLANNER_SEED} status please`,
    },
    {
      agents: [plannerAgent({ capabilities: ["aside"] }), builderAgent(["aside"])],
      reply: false,
      sign: `${instead.before}/aside`,
      words: `/btw ${PLANNER_SEED} ${BUILDER_MENTION} status please`,
    },
    // A reply in the planner's thread, which goes in Send, while it takes BTW and Aside alone.
    {
      agents: [plannerAgent({ capabilities: ["btw", "aside"] })],
      reply: true,
      sign: `${instead.before}/btw${instead.or}/aside`,
      words: "status please",
    },
  ]) {
    const page = renderPage({}, messageOutline());
    act(() => {
      page.client.setQueryData(["issue-messages", KEY], [plannerThread()]);
      page.client.setQueryData(["agents"], agents);
    });
    if (reply) {
      await page.press(user, "message-1", "r");
      await waitFor(() => expect(writeField()).toHaveFocus());
    } else {
      await user.click(writeField());
    }
    await user.paste(words);
    await drawn(sign);
    warnings.push(
      ...[...composerBox().querySelectorAll("p")]
        .map((line) => line.textContent ?? "")
        .filter((text) => text.includes(instead.before))
    );
    cleanup();
  }
  return warnings;
}

// Decision 527 (2) and root R22: each answer the box draws in answer to the reader's act is one
// plain sentence. The box's own list holds them all - what the box says (`COMPOSER_ANSWER_WORDS`)
// and what its picker says (`BOX_PICKER_LINES`) - and each is read whole in every form the box
// builds of it: a sentence the box cuts around a name, read with one in its place, and the
// warning it joins of two of the list's parts, read off the box (`offeredModeWarnings`).
test("every answer of the box's own list is one sentence", async () => {
  const answers = [COMPOSER_ANSWER_WORDS, BOX_PICKER_LINES].flatMap(wholeAnswers);
  const warnings = await offeredModeWarnings();
  expect(answers.length).toBeGreaterThan(0);
  expect(warnings).toHaveLength(3);
  expect([...answers, ...warnings].filter((answer) => sentencesIn(answer).length > 1)).toEqual([]);
});
