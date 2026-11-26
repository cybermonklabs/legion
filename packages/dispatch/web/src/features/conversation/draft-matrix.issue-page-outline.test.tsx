/**
 * The draft matrix, the new issue page's box.
 *
 * Every composer host is held to one rule (`.dispatch/architecture/dispatch-web.md`, "A composer's
 * draft - typed, refused or stopped - goes where the person takes it, and stays where the host
 * moved away from"): the reader's own move with the composer's own control takes the draft with
 * it, and a move of the host's carries nothing: the text stays with the address it was written
 * for, and is there when the reader comes back to it, and what the composer holds when its host
 * takes it off the page waits the same way for the composer drawn at that address next. A
 * refused or stopped send's note stays at its own address. Each cell writes a draft in one state -
 * typed and never sent, refused by Dispatch, or stopped by the reader - makes one move of the
 * page's, comes back, and asserts what the box holds.
 *
 * Host: the new issue page's one box at the foot of its outline (`IssueComposer` in
 * `features/issue-page/IssueComposer.tsx`, which `FootSlot` draws for a reader whose issue page
 * choice is the outline, `issue.page`), which mounts the composer in its outline look twice: the
 * line that writes a comment on the issue, a reply to a row or a comment on a statement
 * (`WriteLine`, under the writer's send name), and the line that edits the reader's own comment
 * (`EditLine`, under that name and the comment's id). The page is drawn as
 * `features/issue-page/composer-fixture.tsx` draws it for the box's own cases too: the outline's
 * tree and the box under the page's writer and its words, as `IssueOutlinePage.tsx` draws the
 * stage and the foot slot, and the page's moves are made as the reader makes them - a row's `r`,
 * an Edit, the box's own controls and keys - or as the address makes them, drawing the
 * box in another place on the same mount (a view, a document, the issue closed).
 *
 * | Line · move                                                       | typed  | refused | stopped |
 * | ----------------------------------------------------------------- | ------ | ------- | ------- |
 * | box · Reply on another row, then back (on)                        | cell   | cell    | cell    |
 * | box · host moves it over a refusal (elsewhere), then back (on)    | no (1) | cell    | cell    |
 * | box · a comment on a statement, Reply on another row, then back   | cell   | cell    | cell    |
 * | box · a reply kept for a row, then a read drops that row          | cell   | no (10) | no (10) |
 * | box · a statement's comment kept, then a new version rewrites it  | cell   | no (10) | no (10) |
 * | box · Write on the issue instead                                  | cell   | cell    | cell    |
 * | box · Escape over a reply                                         | cell   | cell    | cell    |
 * | box · Write on the issue instead, the issue keeping a comment ... | cell   | cell    | cell    |
 * | box · an edit opened, then ended (hidden)                         | cell   | cell    | cell    |
 * | box · a view, then back (hidden)                                  | cell   | cell    | cell    |
 * | box · a document opened, then back (hidden)                       | cell   | cell    | cell    |
 * | box · the issue closed, then open again                           | cell   | cell    | cell    |
 * | box · Question, then Comment again                                | cell   | cell    | cell    |
 * | box · issue left, then back (off)                                 | cell   | cell    | cell    |
 * | box · the page reloaded, then back (off)                          | cell   | cell    | cell    |
 * | box · Close, an issue pick, a phone thread's Back                 | no (2) | no (2)  | no (2)  |
 * | box editing a comment · a Reply on another row, then Edit again   | cell   | cell    | cell    |
 * | box editing a comment · Escape, then Edit again                   | cell   | cell    | cell    |
 * | box editing a comment · Edit on another comment, then back        | cell   | cell    | cell    |
 * | box editing a comment · issue left, then Edit again (off)         | cell   | cell    | cell    |
 * | box editing a comment · the page reloaded, then Edit again (off)  | cell   | cell    | cell    |
 * | either · the page's writer missing                                | no (3) | no (3)  | no (3)  |
 * | either · the issue's read fails or waits, then lands              | no (4) | no (4)  | no (4)  |
 * | either · an issue address that names no issue                     | no (5) | no (5)  | no (5)  |
 * | either · the issue page choice moves to today's page              | no (6) | no (6)  | no (6)  |
 * | either · a phone width                                            | no (7) | no (7)  | no (7)  |
 * | box editing a comment · the box hidden                            | no (8) | no (8)  | no (8)  |
 * | box editing a comment · where the box puts the reader back        | no (9) | no (9)  | no (9)  |
 *
 * (on) marks a move of the host's that keeps the box on screen at another address: a typed draft
 * is kept for the target it was written for, and a line says so and names it; a refused or
 * stopped reply holds the box on its target (the writer's `redirectedReplyTarget` and
 * `keptOnTarget`), so its draft and note never leave. The elsewhere row is a refusal on the
 * issue, which holds the box on no target: Send says where the draft was written for and offers
 * no Retry, and the issue's own Reply brings the box back to it. (hidden) marks a move that keeps
 * the box mounted and out of sight: an edit line drawn in its place, a view or a document on,
 * which hides the box while it holds no send of its own. (off) marks one that takes it off the
 * page: another issue's address draws the page afresh (`IssueOutline` keyed by `route.key`), which
 * the cells make by drawing the page again over the same tab's query client, and a reload ends
 * the whole page, as any reload does (`endAsAReload`), and draws it again over a new query client
 * and the same tab. The edit line is drawn only while the writer edits, so every edit row ends
 * the edit and comes back by its Edit. The gone rows take away the target a typed draft is kept
 * for - a read that no longer holds the row, a new version that rewrites the statement's words -
 * so the way back its line names is gone: the box draws the draft whole beside the line, and its
 * Use it here, at rest on the issue, brings it back into the field. The own-move rows - the
 * target line's control and Escape, the box's Cancel reply - carry the draft and its refusal to
 * the issue (`HeldSends.carry`); a stopped reply says there that it may have been delivered as
 * the reply, and its Retry goes under a key of its own. Where the issue keeps a comment of the
 * reader's, that own move carries nothing: the comment comes back in the field, and the reply
 * stays with its comment, typed and kept with its line, or refused or stopped with Send saying
 * where it was written for.
 *
 * The numbered reasons - the moves this host cannot make, and the moves and forms the composer
 * census found that the rows above do not make - are `REASONS` in
 * `draft-matrix.issue-page-outline.coverage.ts`, which holds each census item this file answers
 * for to a test above or to one of them.
 */
import { afterEach, beforeAll, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import type { MutationKey, QueryClient } from "@tanstack/react-query";
import { act, cleanup, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";

import {
  BODY,
  endAsAReload,
  expectedBack,
  HELD,
  type Held,
  newQueryClient,
  REFUSAL,
  signedInAsReader,
} from "../../__tests__/draft-matrix-fixture";
import { nextFrame } from "../../__tests__/frames";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import {
  ALICE,
  answerBoxReads,
  askRecord,
  BOB,
  type BoxReads,
  commentRecord,
  composerBox,
  drawBoxPage,
  KEPT_FOR_ISSUE,
  KEPT_FOR_STATEMENT,
  KEY,
  keptLines,
  keptReplyLine,
  outlineRewritten,
  outlineWithout,
  type Place,
  READER,
  rowOf,
  STATEMENT_QUOTE,
  STATEMENT_REWRITTEN,
  STOPPED_IN_BOX,
  writeField,
} from "../issue-page/composer-fixture";
import type { TreeNode } from "../issue-page/issue-tree";
import type { OutlineWriter } from "../issue-page/outline-writer";
import { COMPOSER_ANSWER_WORDS, CONTROL_LABELS } from "../issue-page/page-words";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import type { ReplyTarget } from "./composer-model";
import { heldSends } from "./held-sends";
import { type SentRequest, saveCommentEdit } from "./send-request";
import { unsentDraftsOf } from "./unsent-drafts";
import { composerNameOf } from "./useKeptDrafts";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const HOST = "box";
const EDITING = "box editing a comment";

/** The box's send name (`OutlineWriter.sendKey`), and its edit line's for the reader's comment
 *  (`EditLine`'s `mutationKey`). */
const SEND_KEY: MutationKey = ["issue-composer", KEY];
const EDIT_KEY: MutationKey = [...SEND_KEY, "edit", "comment-2"];

/** The reader's own comment the edit rows edit, as it stands, and another of theirs. */
const OWN_WORDS = "My own words";
const OTHER_OWN_WORDS = "Another word of mine";

/** The statement's row in the outline (`statementNode`). */
const STATEMENT_ROW = "spec#b-r1";

/** What Send says while the host has the box away from the issue its refused comment is for, and
 *  while the box shows the issue away from the reply a refusal is held for: the page's own
 *  sentences (`COMPOSER_ANSWER_WORDS.elsewhere`), the first around where the comment was written
 *  for. */
const { elsewhere } = COMPOSER_ANSWER_WORDS;
const WRITTEN_FOR_ISSUE = `${elsewhere.address.before}${COMPOSER_ANSWER_WORDS.address.prepositions[0]} ${KEY}${elsewhere.address.after}`;
const WRITTEN_AS_REPLY = elsewhere.reply;

/** What the box says of a stopped reply the reader carried to the issue (`SendMovedError`): the
 *  page's sentence around where it went, as a reply. */
const MOVED_REPLY_NOTE = `${COMPOSER_ANSWER_WORDS.stopped.moved.before}${COMPOSER_ANSWER_WORDS.address.reply}${COMPOSER_ANSWER_WORDS.stopped.moved.after}`;

/** What the box says of a send Dispatch refused with a code the box has no sentence of its own
 *  for (`REFUSAL`), in place of Dispatch's words. */
const REFUSED_NOTE = COMPOSER_ANSWER_WORDS.refused.other;

/** Which note the box shows of a send it holds, in the box's own words: Dispatch's refusal, a
 *  send the reader stopped, or none - the shared matrix's `noteIn`, read in the page's words. */
function noteIn(place: HTMLElement): Held | "none" {
  const shown = within(place);
  if (shown.queryByText(REFUSED_NOTE) !== null) return "refused";
  return shown.queryByText(STOPPED_IN_BOX) === null ? "none" : "stopped";
}

/** What the box says of a reply it keeps for bob's comment while it shows another target. */
const KEPT_FOR_BOB = keptReplyLine("bob");

/** bob's comment as the box's reply quotes it. */
const BOBS_REPLY: ReplyTarget = {
  actor: BOB,
  excerpt: "Earlier words",
  id: "comment-1",
  parentKind: "comment",
};

/** The sends a stopped cell seeds, as the box's lines make them: a comment on the issue, a reply
 *  to bob's comment, a comment on the statement, and an edit of the reader's comment. */
const STOPPED_ON_ISSUE = { key: SEND_KEY, request: sentRequest(BODY, null) };
const STOPPED_REPLY = { key: SEND_KEY, request: sentRequest(BODY, BOBS_REPLY) };
const STOPPED_ON_STATEMENT = {
  key: SEND_KEY,
  request: sentRequest(BODY, null, {
    statement: { artifact: "spec", quote: STATEMENT_QUOTE, row: STATEMENT_ROW },
  }),
};
const STOPPED_EDIT = {
  key: EDIT_KEY,
  request: sentRequest(BODY, null, {
    edit: { base: OWN_WORDS, carried: undefined, id: "comment-2", save: saveCommentEdit },
  }),
};

/** The reads the box and the writer draw from: the outline's questions and comments, and a
 *  second comment of the reader's, which the edit rows open the Edit of. */
const BOX_READS: BoxReads = {
  asks: [
    askRecord("ask-1"),
    askRecord("ask-2", {
      answer: { at: "2026-11-06T00:00:00Z", selected: ["No"], text: "Old reason", user: READER },
      state: "answered",
    }),
  ],
  comments: [
    commentRecord("comment-1", BOB, "Earlier words"),
    commentRecord("comment-2", ALICE, OWN_WORDS),
    commentRecord("comment-4", ALICE, OTHER_OWN_WORDS),
  ],
};

/** The box as a cell works it: the page drawn over the tab's query client, the reader, and the
 *  page's moves. */
interface Box {
  /** The query client the page is drawn over now. */
  readonly client: () => QueryClient;
  /** Draws the page again over the same client, as the reader coming back to the issue finds
   *  it after another issue's address drew the page afresh. */
  readonly comeBack: () => void;
  /** Ends the page as a reload does and draws it again over a new query client and the same tab
   *  (`endAsAReload`), signed in as the same reader. */
  readonly reload: () => void;
  /** Takes the page off the screen, as the reader leaving the issue does. */
  readonly leave: () => void;
  /** Focuses the row of `nodeId` and presses `key` there, as the reader does. */
  readonly press: (nodeId: string, key: string) => Promise<void>;
  /** Draws the box in another place on the same mount: a view on, a document open, the issue
   *  closed. */
  readonly redraw: (at: Place) => void;
  /** Draws the outline from another tree on the same mount, as a read that changed what it
   *  holds does. */
  readonly redrawOutline: (drawn: TreeNode) => void;
  readonly user: UserEvent;
  readonly writer: () => OutlineWriter;
}

const restores: (() => void)[] = [];

let createComment: Mock<typeof api.createComment>;
let editComment: Mock<typeof api.editComment>;

beforeEach(() => {
  const reads = answerBoxReads(BOX_READS);
  createComment = spyOn(api, "createComment").mockRejectedValue(REFUSAL);
  editComment = spyOn(api, "editComment").mockRejectedValue(REFUSAL);
  restores.push(() => {
    reads.restore();
    createComment.mockRestore();
    editComment.mockRestore();
  });
});

afterEach(() => {
  cleanup();
  for (const restore of restores.splice(0)) restore();
});

/**
 * The page over a new query client signed in as the reader, the box at rest. Every write is
 * refused, so a send pressed here comes back refused; `stopped`, when given, is sent through the
 * held-send store under its name before the page is drawn and stopped once late, as the reader's
 * Stop trying does, so the page arrives showing it. `keptOnIssue`, when given, is a comment the
 * box keeps for the issue (`AwayDraft`), as a Reply the reader pressed over it leaves it.
 */
async function openBox(
  stopped?: { readonly key: MutationKey; readonly request: SentRequest },
  keptOnIssue?: string
): Promise<Box> {
  const user = userEvent.setup();
  let client = signedInAsReader(newQueryClient());
  if (stopped !== undefined) {
    const write = stopped.request.edit === undefined ? createComment : editComment;
    await stopSend(client, stopped.key, stopped.request, [write]);
    write.mockRejectedValue(REFUSAL);
  }
  if (keptOnIssue !== undefined) {
    unsentDraftsOf(client).keepAway(composerNameOf(SEND_KEY), {
      draft: { body: keptOnIssue, replacement: "" },
      owner: { issueKey: KEY, kind: "issue" },
      replyTo: null,
    });
  }
  let page = drawBoxPage(client, BOX_READS);
  return {
    client: () => client,
    comeBack: () => {
      page = drawBoxPage(client, BOX_READS);
    },
    leave: () => page.view.unmount(),
    press: async (nodeId, key) => {
      await page.press(user, nodeId, key);
      await writerFocused();
    },
    redraw: (at) => page.redraw(at),
    redrawOutline: (drawn) => page.redrawOutline(drawn),
    reload: () => {
      endAsAReload(() => page.view.unmount());
      client = signedInAsReader(newQueryClient());
      page = drawBoxPage(client, BOX_READS);
    },
    user,
    writer: () => page.writer(),
  };
}

/** Lets the frame the writer puts the caret in the box's line on run (`OutlineWriter.focus`),
 *  inside `act`: every aim and every end of a target asks for one, and the field taking the caret
 *  is a state of the page's that no row may leave to land after it has read the page. */
async function writerFocused(): Promise<void> {
  await act(() => nextFrame());
}

/** The field of the line that edits the reader's comment. */
function editField(): HTMLTextAreaElement {
  return within(composerBox()).getByRole<HTMLTextAreaElement>("textbox", {
    name: CONTROL_LABELS.edit_comment,
  });
}

/** What the box's line is aimed at: the entry its reply answers, the statement its comment is
 *  on, or nothing for a write on the issue. */
function aimedAt(box: Box): string | null {
  const writer = box.writer();
  return writer.reply?.id ?? writer.statement?.row ?? null;
}

/** Whether the box offers Retry. */
function retryOffered(): boolean {
  return within(composerBox()).queryByRole("button", { name: CONTROL_LABELS.retry }) !== null;
}

/** What the write line holds where it shows again: its words and its note. */
function writeHeld(): { note: Held | "none"; text: string } {
  return { note: noteIn(composerBox()), text: writeField().value };
}

/** What the edit line holds where it shows again: its words and its note. */
function editHeld(): { note: Held | "none"; text: string } {
  return { note: noteIn(composerBox()), text: editField().value };
}

/** Puts `BODY` in the field `field` names, and for `refused` sends it with Enter, which Dispatch
 *  refuses. */
async function write(box: Box, field: () => HTMLTextAreaElement, held: Held): Promise<void> {
  await box.user.click(field());
  await box.user.keyboard("{Control>}a{/Control}");
  await box.user.paste(BODY);
  if (held === "refused") {
    await box.user.keyboard("{Enter}");
    await within(composerBox()).findByText(REFUSED_NOTE);
  }
}

/** Puts `BODY` in the box at rest as a comment on the issue, in the state `held` names; a stopped
 *  comment is the one `openBox` seeded, which the box mounted with. */
async function holdOnIssue(box: Box, held: Held): Promise<void> {
  if (held === "stopped") {
    await waitFor(() => expect(noteIn(composerBox())).toBe("stopped"));
    return;
  }
  await write(box, writeField, held);
}

/** Aims the box at `nodeId` with its row's `r` and puts `BODY` in it there, in the state `held`
 *  names; a stopped send is the one `openBox` seeded there, which the box mounted on. */
async function holdAimedAt(box: Box, nodeId: string, held: Held): Promise<void> {
  if (held === "stopped") {
    await waitFor(() => expect(noteIn(composerBox())).toBe("stopped"));
    expect(aimedAt(box)).toBe(nodeId);
    return;
  }
  await box.press(nodeId, "r");
  await waitFor(() => expect(writeField()).toHaveFocus());
  await write(box, writeField, held);
}

/** Opens the edit of the reader's comment `id`, which `words` it holds, as its Edit does. No row
 *  draws Edit yet, so the cell asks the writer, as `IssueComposer.test.tsx` does. */
async function openEdit(box: Box, id = "comment-2", words = OWN_WORDS): Promise<void> {
  await act(async () => box.writer().edit(id, words));
  await writerFocused();
  await waitFor(() => expect(box.writer().editing?.id).toBe(id));
  editField();
}

/** Opens the edit of the reader's comment and puts `BODY` in it, in the state `held` names; a
 *  stopped save is the one `openBox` seeded, which the edit line mounts with. */
async function holdInEdit(box: Box, held: Held): Promise<void> {
  await openEdit(box);
  if (held === "stopped") {
    await waitFor(() => expect(noteIn(composerBox())).toBe("stopped"));
    return;
  }
  await write(box, editField, held);
}

/** The seeded send of a stopped cell, `stopped`, and none for every other. */
const seeded = (
  held: Held,
  stopped: { readonly key: MutationKey; readonly request: SentRequest }
): typeof stopped | undefined => (held === "stopped" ? stopped : undefined);

// ---------------------------------------------------------------------------------------------
// The write line.

// A Reply on another row is the host's move: a reply typed for bob's comment is kept for it, a
// line says so and names bob, and the box opens on the other row with that row's own start; bob's
// Reply brings the reply back. A refused or stopped reply holds the box on bob's comment (the
// writer's `redirectedReplyTarget`), so its draft and its note never leave.
for (const held of HELD) {
  test(`${HOST} · Reply on another row, then back · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_REPLY));
    await holdAimedAt(box, "comment-1", held);
    await box.press("ask-2", "r");
    await waitFor(() => expect(writeField()).toHaveFocus());
    const away = { aimed: aimedAt(box), kept: keptLines(), ...writeHeld() };
    await box.press("comment-1", "r");
    await waitFor(() => expect(aimedAt(box)).toBe("comment-1"));
    expect({ away, back: writeHeld() }).toEqual({
      away:
        held === "typed"
          ? { aimed: "ask-2", kept: [KEPT_FOR_BOB], note: "none", text: "" }
          : { aimed: "comment-1", kept: [], ...expectedBack(held) },
      back: expectedBack(held),
    });
  });
}

// A comment refused or stopped on the issue holds the box on no target, so a Reply on bob's row
// moves it there, the host's move: Send says where the draft was written for, refuses, and
// offers no Retry, the draft still in the field. The issue's own Reply, the way back, finds the
// note and its Retry again. The outline draws the issue as its header rather than as a row, so
// the cell calls what the issue's row's Reply calls (`RowActs`).
for (const held of ["refused", "stopped"] as const) {
  test(`${HOST} · host moves it over a refusal (elsewhere), then back · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_ON_ISSUE));
    await holdOnIssue(box, held);
    await box.press("comment-1", "r");
    await waitFor(() => expect(aimedAt(box)).toBe("comment-1"));
    const send = within(composerBox()).getByRole("button", { name: CONTROL_LABELS.send });
    expect(send).toHaveAccessibleDescription(WRITTEN_FOR_ISSUE);
    const away = {
      refused: send.getAttribute("aria-disabled") === "true",
      retry: retryOffered(),
      text: writeField().value,
    };
    await act(async () => box.writer().writeOnIssue());
    await writerFocused();
    await waitFor(() => expect(aimedAt(box)).toBeNull());
    expect({ away, back: { ...writeHeld(), retry: retryOffered() } }).toEqual({
      away: { refused: true, retry: false, text: BODY },
      back: { ...expectedBack(held), retry: true },
    });
  });
}

// The box's address names the statement its comment is on (`OutlineLook.anchor`), as it names a
// reply: a Reply on another row keeps a comment typed on the statement for it, a line says so,
// and the statement's Reply brings it back; a refused or stopped comment on the statement holds
// the box on it (the writer's `keptOnStatement`), its note and its Retry at home there.
for (const held of HELD) {
  test(`${HOST} · a comment on a statement, Reply on another row, then back · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_ON_STATEMENT));
    await holdAimedAt(box, STATEMENT_ROW, held);
    await box.press("comment-1", "r");
    await waitFor(() => expect(writeField()).toHaveFocus());
    const away = { aimed: aimedAt(box), kept: keptLines(), ...writeHeld() };
    await box.press(STATEMENT_ROW, "r");
    await waitFor(() => expect(aimedAt(box)).toBe(STATEMENT_ROW));
    expect({ away, back: { ...writeHeld(), retry: retryOffered() } }).toEqual({
      away:
        held === "typed"
          ? { aimed: "comment-1", kept: [KEPT_FOR_STATEMENT], note: "none", text: "" }
          : { aimed: STATEMENT_ROW, kept: [], ...expectedBack(held) },
      back: { ...expectedBack(held), retry: held !== "typed" },
    });
  });
}

// A read that no longer holds bob's comment takes its Reply away, so a reply typed for it and
// kept there by the issue's own Reply, the page's move, loses the way back its line names: the
// box draws it whole beside the line, and at rest on the issue its Use it here puts it back in
// the field. A refused or stopped reply holds the box on bob's comment, so nothing is kept away
// for its row to go from under (10).
test(`${HOST} · a reply kept for a row, then a read drops that row · typed`, async () => {
  const box = await openBox();
  await holdAimedAt(box, "comment-1", "typed");
  await act(async () => box.writer().writeOnIssue());
  await writerFocused();
  await waitFor(() => expect(aimedAt(box)).toBeNull());
  const away = { kept: keptLines(), ...writeHeld() };
  box.redrawOutline(outlineWithout("comment-1"));
  const take = await within(composerBox()).findByRole("button", { name: "Use it here" });
  await box.user.click(take);
  await waitFor(() => expect(writeField().value).toBe(BODY));
  expect({ away, back: { kept: keptLines(), ...writeHeld() } }).toEqual({
    away: { kept: [KEPT_FOR_BOB], note: "none", text: "" },
    back: { kept: [], ...expectedBack("typed") },
  });
});

// A new version that rewrites the statement's words makes its Reply aim the box at the new words,
// so a comment typed on the old words and kept there loses the way back its line names: the box
// draws it whole beside the line, and at rest on the issue its Use it here puts it back in the
// field. A refused or stopped comment holds the box on its statement (10).
test(`${HOST} · a statement's comment kept, then a new version rewrites it · typed`, async () => {
  const box = await openBox();
  await holdAimedAt(box, STATEMENT_ROW, "typed");
  await act(async () => box.writer().writeOnIssue());
  await writerFocused();
  await waitFor(() => expect(aimedAt(box)).toBeNull());
  const away = { kept: keptLines(), ...writeHeld() };
  box.redrawOutline(outlineRewritten(STATEMENT_REWRITTEN));
  const take = await within(composerBox()).findByRole("button", { name: "Use it here" });
  await box.user.click(take);
  await waitFor(() => expect(writeField().value).toBe(BODY));
  expect({ away, back: { kept: keptLines(), ...writeHeld() } }).toEqual({
    away: { kept: [KEPT_FOR_STATEMENT], note: "none", text: "" },
    back: { kept: [], ...expectedBack("typed") },
  });
});

/** The box's own ends of its target: the target line's control, and Escape in the line. */
const OWN_ENDS = [
  { end: "control", name: "Write on the issue instead" },
  { end: "Escape", name: "Escape over a reply" },
] as const;

/** Ends the box's target as `end` names: the target line's control, or Escape in the line. */
async function endTarget(box: Box, end: "control" | "Escape"): Promise<void> {
  if (end === "Escape") {
    await box.user.click(writeField());
    await box.user.keyboard("{Escape}");
  } else {
    await box.user.click(
      within(composerBox()).getByRole("button", { name: CONTROL_LABELS.write_on_issue })
    );
  }
  await writerFocused();
  await waitFor(() => expect(aimedAt(box)).toBeNull());
}

/** Which note the box shows of a send it holds, a stopped send the reader carried to another
 *  address included (`SendMovedError`), which says it may have been delivered as a reply. */
function noteOf(): Held | "moved" | "none" {
  return within(composerBox()).queryByText(MOVED_REPLY_NOTE) === null
    ? noteIn(composerBox())
    : "moved";
}

// The target line's control and Escape are the box's Cancel reply, the reader's own move: the
// reply to bob's comment goes to the issue whole, and its refusal with it, where its Retry sends
// it (`HeldSends.carry`). A stopped reply may have been delivered as that reply, so on the issue
// it says so, and its Retry goes as a new write, under a key the stopped send never went under.
for (const { end, name } of OWN_ENDS) {
  for (const held of HELD) {
    test(`${HOST} · ${name} · ${held}`, async () => {
      const box = await openBox(seeded(held, STOPPED_REPLY));
      await holdAimedAt(box, "comment-1", held);
      await endTarget(box, end);
      expect({
        kept: keptLines(),
        note: noteOf(),
        retry: retryOffered(),
        text: writeField().value,
      }).toEqual({
        kept: [],
        note: held === "stopped" ? "moved" : held === "typed" ? "none" : held,
        retry: held !== "typed",
        text: BODY,
      });
      if (held !== "stopped") return;
      const tried = createComment.mock.calls.length;
      await box.user.click(
        within(composerBox()).getByRole("button", { name: CONTROL_LABELS.retry })
      );
      await waitFor(() => expect(createComment).toHaveBeenCalledTimes(tried + 1));
      const keys = createComment.mock.calls.map(([, , bounds]) => bounds?.key);
      expect({ before: [...new Set(keys.slice(0, tried))], retry: keys[tried] }).toEqual({
        before: [STOPPED_REPLY.request.sendKey],
        retry: expect.not.stringMatching(`^${STOPPED_REPLY.request.sendKey}$`),
      });
      expect(createComment.mock.calls[tried]?.[1]).toEqual({ body: BODY });
    });
  }
}

/** What the issue keeps of the reader's in the row below: a comment typed at rest before a
 *  Reply on bob's comment kept it for the issue. */
const ISSUE_WORDS = "A comment kept for the issue";

// The reader's own move onto an address that keeps a draft of its own carries nothing, so
// neither draft is written over (`.dispatch/architecture/dispatch-web.md`): the box's own end of
// a reply while the issue keeps a comment of the reader's brings that comment back in the field,
// and the reply stays with bob's comment - typed, kept for it with its line; refused or stopped,
// in its held send, which Send names as written for a reply. Bob's Reply brings the reply back,
// its note and its Retry with it, and the comment is kept for the issue again.
for (const held of HELD) {
  test(`${HOST} · Write on the issue instead, the issue keeping a comment of its own · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_REPLY), ISSUE_WORDS);
    await holdAimedAt(box, "comment-1", held);
    expect(keptLines()).toEqual([KEPT_FOR_ISSUE]);
    await endTarget(box, "control");
    const send = within(composerBox()).getByRole("button", { name: CONTROL_LABELS.send });
    const away = {
      kept: keptLines(),
      note: noteOf(),
      reply: heldSends(box.client()).get(SEND_KEY)?.request.replyTo?.id ?? null,
      retry: retryOffered(),
      said: within(composerBox())
        .queryAllByText(WRITTEN_AS_REPLY)
        .some((line) => line.closest(".sr-only") === null),
      sendRefused: send.getAttribute("aria-disabled") === "true",
      text: writeField().value,
    };
    await box.press("comment-1", "r");
    await waitFor(() => expect(aimedAt(box)).toBe("comment-1"));
    await waitFor(() => expect(writeField().value).toBe(BODY));
    expect({
      away,
      back: { kept: keptLines(), note: noteOf(), retry: retryOffered(), text: writeField().value },
    }).toEqual({
      away:
        held === "typed"
          ? {
              kept: [KEPT_FOR_BOB],
              note: "none",
              reply: null,
              retry: false,
              said: false,
              sendRefused: false,
              text: ISSUE_WORDS,
            }
          : {
              kept: [],
              note: held,
              reply: "comment-1",
              retry: false,
              said: true,
              sendRefused: true,
              text: ISSUE_WORDS,
            },
      back: {
        kept: [KEPT_FOR_ISSUE],
        note: held === "typed" ? "none" : held,
        retry: held !== "typed",
        text: BODY,
      },
    });
  });
}

// The reader's own comment's Edit draws the edit line in place of the write line, which stays
// mounted, hidden (`<div hidden={!writing}>`); Escape ends the edit, and the write line is back
// with its draft and its note. A send on the issue holds the box on no target, so the edit opens.
// The Edit is pressed where the comment is drawn, so the focus is on its row as the edit opens.
for (const held of HELD) {
  test(`${HOST} · an edit opened, then ended · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_ON_ISSUE));
    await holdOnIssue(box, held);
    await act(async () => rowOf("comment-2").focus());
    await openEdit(box);
    await waitFor(() => expect(editField()).toHaveFocus());
    const away = writeField().closest("[hidden]") !== null;
    await box.user.keyboard("{Escape}");
    await waitFor(() => expect(box.writer().editing).toBeUndefined());
    expect({ away, back: writeHeld() }).toEqual({ away: true, back: expectedBack(held) });
  });
}

/** The places the address draws the box in, out of the outline's sight: a view on, a document
 *  open. */
const AWAY_PLACES = [
  { name: "a view, then back", place: { viewOn: true } },
  { name: "a document opened, then back", place: { documentOpen: true } },
] as const;

// A view or a document hides the box while it holds no send of its own, and keeps it mounted
// (`<section hidden={!shown}>`), so coming back finds the same box with its draft; a refused or
// stopped send keeps the box drawn, its note on screen.
for (const { name, place } of AWAY_PLACES) {
  for (const held of HELD) {
    test(`${HOST} · ${name} · ${held}`, async () => {
      const box = await openBox(seeded(held, STOPPED_ON_ISSUE));
      await holdOnIssue(box, held);
      box.redraw(place);
      const away = composerBox().hasAttribute("hidden");
      box.redraw({});
      expect({ away, back: writeHeld() }).toEqual({
        away: held === "typed",
        back: expectedBack(held),
      });
    });
  }
}

// The issue closed writes a comment whatever chip was pressed (`kind`), and takes the box to no
// other address, so opening it again finds the draft and its note where they were.
for (const held of HELD) {
  test(`${HOST} · the issue closed, then open again · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_ON_ISSUE));
    await holdOnIssue(box, held);
    box.redraw({ closed: true });
    const away = writeHeld();
    box.redraw({});
    expect({ away, back: writeHeld() }).toEqual({
      away: expectedBack(held),
      back: expectedBack(held),
    });
  });
}

// The Question chip changes the kind the box writes (`kind`) and no address, so the draft and its
// note stay through Question and Comment again.
for (const held of HELD) {
  test(`${HOST} · Question, then Comment again · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_ON_ISSUE));
    await holdOnIssue(box, held);
    for (const kind of ["ask", "comment"] as const) {
      const chip = composerBox().querySelector<HTMLElement>(`[data-composer-kind="${kind}"]`);
      if (chip === null) throw new Error(`the box drew no ${kind} chip`);
      await box.user.click(chip);
      await waitFor(() => expect(box.writer().kind).toBe(kind));
    }
    expect(writeHeld()).toEqual(expectedBack(held));
  });
}

// Another issue's address draws the page afresh (`IssueOutline` keyed by `route.key`, the
// writer's provider under it), so the box leaves the page with the issue and the one drawn on
// return shows what it held: a typed comment kept for the issue (`AwayDraft`), a refused or
// stopped one in its held send.
for (const held of HELD) {
  test(`${HOST} · issue left, then back · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_ON_ISSUE));
    await holdOnIssue(box, held);
    box.leave();
    box.comeBack();
    await waitFor(() => expect(writeHeld()).toEqual(expectedBack(held)));
  });
}

// A reload ends the whole page and draws it again over a new query client and the same tab: what
// the tab kept as the page went (`pagehide`) is what the box shows again.
for (const held of HELD) {
  test(`${HOST} · the page reloaded, then back · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_ON_ISSUE));
    await holdOnIssue(box, held);
    box.reload();
    await waitFor(() => expect(writeHeld()).toEqual(expectedBack(held)));
  });
}

// ---------------------------------------------------------------------------------------------
// The edit line.

// A Reply on another row ends the edit, the host's move, which takes the edit line off the page
// (`EditLine` is drawn only while the writer edits): its typed edit is kept for the comment it
// was written over, and a refused or stopped save in its held send, so the comment's Edit finds
// either again.
for (const held of HELD) {
  test(`${EDITING} · a Reply on another row, then Edit again · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_EDIT));
    await holdInEdit(box, held);
    await box.press("comment-1", "r");
    await waitFor(() => expect(box.writer().editing).toBeUndefined());
    await openEdit(box);
    await waitFor(() => expect(editHeld()).toEqual(expectedBack(held)));
  });
}

// Escape and the target line's control end the edit and give the reader the comment's row back,
// which takes the edit line off the page: its draft and its note wait for the comment's Edit.
for (const held of HELD) {
  test(`${EDITING} · Escape, then Edit again · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_EDIT));
    await holdInEdit(box, held);
    await box.user.click(editField());
    await box.user.keyboard("{Escape}");
    await waitFor(() => expect(box.writer().editing).toBeUndefined());
    await openEdit(box);
    await waitFor(() => expect(editHeld()).toEqual(expectedBack(held)));
  });
}

// The edit line is keyed by the comment it edits (`<EditLine key={writer.editing.id}>`), so the
// Edit of another of the reader's comments draws a line of its own, with that comment's words,
// and the first comment's Edit finds its edit and its note again.
for (const held of HELD) {
  test(`${EDITING} · Edit on another comment, then back · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_EDIT));
    await holdInEdit(box, held);
    await openEdit(box, "comment-4", OTHER_OWN_WORDS);
    const away = editHeld();
    await openEdit(box);
    await waitFor(() => expect(editHeld()).toEqual(expectedBack(held)));
    expect(away).toEqual({ note: "none", text: OTHER_OWN_WORDS });
  });
}

// Another issue's address draws the page afresh, the edit line with it, and the writer drawn on
// return edits nothing: the comment's Edit finds the edit and its note again.
for (const held of HELD) {
  test(`${EDITING} · issue left, then Edit again · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_EDIT));
    await holdInEdit(box, held);
    box.leave();
    box.comeBack();
    await waitFor(() => expect(box.writer().editing).toBeUndefined());
    await openEdit(box);
    await waitFor(() => expect(editHeld()).toEqual(expectedBack(held)));
  });
}

// A reload ends the page and draws it again over a new query client and the same tab: the
// comment's Edit finds what the tab kept of the edit.
for (const held of HELD) {
  test(`${EDITING} · the page reloaded, then Edit again · ${held}`, async () => {
    const box = await openBox(seeded(held, STOPPED_EDIT));
    await holdInEdit(box, held);
    box.reload();
    await openEdit(box);
    await waitFor(() => expect(editHeld()).toEqual(expectedBack(held)));
  });
}
