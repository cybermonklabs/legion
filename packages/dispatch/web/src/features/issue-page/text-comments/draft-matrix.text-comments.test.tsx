/**
 * The draft matrix, comments on text row: the composer a selection opens and the reply composer
 * of a thread on text, as the new issue page draws them (INFRA-1503, INFRA-5706).
 *
 * Every composer host is held to one rule (`.dispatch/architecture/dispatch-web.md`, "A composer's
 * draft - typed, refused or stopped - goes where the person takes it, and stays where the host
 * moved away from"): a move of the host's carries nothing: the text stays with the address it
 * was written for, and is there when the reader comes back to it, and what it holds when its
 * host takes it off the page waits the same way for the composer drawn at that address next. A
 * refused or stopped send's note stays at its own address. Each cell writes a draft in one
 * state - typed and never sent, refused by Dispatch, or stopped by the reader - makes one move,
 * comes back, and asserts what the composer holds.
 *
 * Hosts: the column of comments on text (`TextMargin`'s `Column`, from 1024 px) and a node's
 * sheet below 1024 px (`NodeThreads`), which each draw the composer a selection opens
 * (`SelectionComposer`, under a send name of the selected words) and the card of each thread of
 * a node the view draws (`TextThreadCard`), whose reply composer is today's margin's
 * (`ThreadCard`, under its thread's reply send name). The rows are the moves the composer census
 * found above those composers, up to the page's root (`IssueOutlinePage`), each a cell in each of
 * the three states (`<host> · <move> · <state>`):
 *
 * | Host                              | Move                                                  |
 * | --------------------------------- | ----------------------------------------------------- |
 * | selection composer                | Escape, then opened again (off)                       |
 * | selection composer                | opened on other words, then back (off)                |
 * | selection composer                | the window narrows below 1024 px, then widens (moved) |
 * | selection composer                | another view, then back (kept)                        |
 * | selection composer                | issue left, then back (off)                           |
 * | selection composer                | the page reloaded, then back (off)                    |
 * | selection composer, below 1024 px | Escape, then opened again (off)                       |
 * | selection composer, below 1024 px | opened on other words, then back (off)                |
 * | selection composer, below 1024 px | another view, then back (kept)                        |
 * | selection composer, below 1024 px | issue left, then back (off)                           |
 * | text thread reply                 | the window narrows below 1024 px, then widens (moved) |
 * | text thread reply                 | another view, then back (off)                         |
 * | text thread reply                 | issue left, then back (off)                           |
 * | text thread reply, below 1024 px  | another view, then back (off)                         |
 *
 * The moves the census found that no row makes, each a numbered reason in every state:
 *
 * | Host · move                                                         | Reason |
 * | ------------------------------------------------------------------- | ------ |
 * | either · the issue's read fails or is pending                       | (1)    |
 * | either · the page choice moves to today's page                      | (2)    |
 * | either · an address that names no issue                             | (3)    |
 * | selection composer · it has nowhere to send                         | (4)    |
 * | selection composer · its owner, anchor or reply target changes      | (5)    |
 * | either · the address's search changes                               | (6)    |
 * | either · the sheet's row is another node's or under another parent  | (7)    |
 * | text thread reply · the address marks one of its replies            | (8)    |
 * | text thread reply · collapsed, or fixed open no more                | (9)    |
 * | text thread reply · its thread read again                           | (10)   |
 * | text thread reply · its card keyed again                            | (11)   |
 * | text thread reply · the cards' shared writes change                 | (12)   |
 * | text thread reply · its card's host draws it fixed open or not      | (13)   |
 * | text thread reply · its thread's kind changes                       | (14)   |
 * | selection composer · its kind changes                               | (15)   |
 * | Details' margin composer · the column takes the editor's composes   | (16)   |
 * | either · the Spec view stops drawing the sheet's node               | (17)   |
 * | either · the Spec view keys a group, a run or a row anew            | (18)   |
 * | either · a Spec view row's or the view's other props change         | (19)   |
 * | either · a decision's cell in the Spec view gains or loses it       | (20)   |
 * | either · the Spec view's form at 1024 px                            | (21)   |
 *
 * (off) marks a move that takes the composer off the page: Escape closes the composer, other
 * words open another composer in its place (`key={wordsKeyOf(request)}`), another view draws no
 * row of the statement and so no card of its thread, another issue's address draws another page
 * (`key={route.key}`), whose column closes the composer as it goes, and a reload ends the whole
 * page. Each is checked where the reader comes back: the composer opened again on the same
 * words, or the card drawn again, holds the draft with its refusal or stopped note. (moved) marks
 * a move that takes the composer to another place at the same address: below 1024 px the column
 * draws nothing and the statement's sheet draws its composer and its cards, and from 1024 px the
 * other way round; it is checked in the sheet and again in the column. (kept) marks a move that
 * keeps the composer mounted: another view keeps it open where the statement's row last stood.
 *
 * The page's root mounts the control that opens the composer (`SelectionComment`), which reads a
 * selection a case would have to lay out, and the row sites that mount the sheet (the Spec
 * view's, from part J) draw it only inside a whole view. So each case opens the composer as the
 * control does (`useTextComposer().open`), with a button of its own, and draws the statement's
 * row with its sheet beside the page, as `NodeThreads.test.tsx` draws it. The composer sends
 * through `MentionComposer`'s own path with the statement's block anchor, so a refused draft is
 * one `MentionComposer` sent and Dispatch refused (`REFUSAL`), and a stopped one is a send to that
 * anchor stopped under the composer's send name before the page is drawn (`stopSend`).
 *
 * A refusal or a stop is drawn as an alert (`QueryError`), which takes no key: one Escape closes
 * the composer with its draft kept in every state.
 *
 * The forms the composer census found: the column's at 1024 px (`TextMargin`'s `wide`) and the
 * sheet's (`NodeThreads`' `wide`), which the narrowing rows make, and the Spec view's at the same
 * width (`paned`, reason 21). The app shell's forms above the page are the sign-in file's.
 *
 * The numbered reasons - the moves the census found that these rows do not make - are `REASONS`
 * in `draft-matrix.text-comments.coverage.ts`, which holds each census item this file answers for
 * to a test above or to one of them.
 */
import { beforeAll, expect, spyOn, test } from "bun:test";
import type { MutationKey } from "@tanstack/react-query";
import { act, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";

import {
  BODY,
  endAsAReload,
  expectedBack,
  HELD,
  type Held,
  heldIn,
  noteIn,
  REFUSAL,
  REFUSED_NOTE,
  replyField,
  signedInAsReader,
} from "../../../__tests__/draft-matrix-fixture";
import { sentRequest, stopSend } from "../../../__tests__/stop-late-write";
import { ApiError, api } from "../../../api/client";
import type { SentRequest } from "../../conversation/send-request";
import { marginReplySendKey } from "../../margin/margin-context";
import { DESKTOP_WIDTH } from "../../margin/margin-fixture";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { KEY, node, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import { MARGIN_WORDS } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { TEXT_THREADS_ATTRIBUTE } from "./attributes";
import {
  answerSeededReads,
  type DrawnPage,
  drawPage,
  newClient,
  ON_STATEMENT,
  STATEMENT,
  STATEMENT_WORDS,
  seededIssue,
  TEXT_THREADS,
  type TextThreadSeed,
} from "./margin-fixtures";
import { NodeThreads } from "./NodeThreads";
import { type SelectedWords, wordsKeyOf } from "./SelectionComposer";
import { TEXT_COMPOSER_ATTRIBUTE, useTextComposer } from "./TextMargin";

/** Where the sheet stands, below 1024 px. */
const BELOW_PANE = 1000;

/** The statement's node, by its tree id, which every composer and card here is on. */
const STATEMENT_NODE = blockNodeId(SPEC_DOCUMENT.slug, STATEMENT);

/** The statement's words a case selects, and the other words of it it selects after. */
const WORDS: Readonly<Record<"page" | "plan", SelectedWords>> = {
  page: { block: STATEMENT, nodeId: STATEMENT_NODE, occurrence: 0, quote: "page" },
  plan: { block: STATEMENT, nodeId: STATEMENT_NODE, occurrence: 0, quote: "plan" },
};

/** The selection composer's send name on the statement's words `page` (`SelectionComposer`). */
const COMPOSER_KEY: MutationKey = ["text-comment", KEY, wordsKeyOf(WORDS.page)];

/** Another issue, which the issue-left rows go to: Dispatch refuses its read (404), so its page
 *  draws only that it was not found, and no composer. */
const OTHER_KEY = "CORE-2";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The control of a selection, as `SelectionComment` opens the composer: a press opens it on the
 *  statement's words, `page` or `plan`, as a selection of them and a press of Comment does. */
function OpenComposer(): ReactNode {
  const composer = useTextComposer();
  return (
    <>
      <button onClick={() => composer.open(WORDS.page)} type="button">
        Comment on page
      </button>
      <button onClick={() => composer.open(WORDS.plan)} type="button">
        Comment on plan
      </button>
    </>
  );
}

/** The statement's row as a row site draws it, with its sheet after its words. */
function StatementRow(): ReactNode {
  const statement = node({ id: STATEMENT_NODE, text: STATEMENT_WORDS, type: "requirement" });
  return (
    <div aria-label="The case's tree" role="tree">
      <div
        aria-labelledby="case-statement-words"
        data-case-row=""
        data-node-id={STATEMENT_NODE}
        data-node-row=""
        role="treeitem"
        tabIndex={0}
      >
        <div data-node-words="" id="case-statement-words">
          {STATEMENT_WORDS}
        </div>
        <NodeThreads node={statement} />
      </div>
    </div>
  );
}

/** A composer's place: the column of comments on text, or the statement's sheet. */
type Place = "column" | "sheet";

/** What a case holds of the page it drew. */
interface Page {
  /** Ends the page as a reload does and loads it again over the same tab, signed in as the same
   *  reader, with a new query client (`endAsAReload`, `signedInAsReader`). */
  readonly reload: () => Promise<void>;
  readonly restore: () => void;
  readonly user: UserEvent;
  /** The page as it is drawn now, which a reload draws again. */
  readonly view: () => DrawnPage;
}

/** A send stopped before the page is drawn, under the send name of the composer that shows it. */
interface Stopped {
  readonly key: MutationKey;
  readonly request: SentRequest;
}

/** The stage's row of the statement, where the view draws one. */
function stageRow(view: DrawnPage): HTMLElement | null {
  return view
    .getByTestId("issue-column")
    .querySelector<HTMLElement>(`[data-node-id="${CSS.escape(STATEMENT_NODE)}"]`);
}

/**
 * CORE-1's page at its Conversation, at `width`, with `threads` the comments on text its reads
 * hold, the control of a case and the statement's row beside it. Every comment the page writes is
 * refused, so a send pressed here comes back refused; `stopped`, when given, is sent through the
 * held-send store under its name before the page is drawn and stopped once late, as the reader's
 * Stop trying does. With `signedIn` the page is signed in as the reader, as a reload row's is, so
 * the tab keeps what it sends.
 */
async function openPage({
  signedIn = false,
  stopped,
  threads,
  width,
}: {
  readonly signedIn?: boolean;
  readonly stopped: Stopped | undefined;
  readonly threads: readonly TextThreadSeed[];
  readonly width: number;
}): Promise<Page> {
  const user = userEvent.setup();
  const stubs = answerSeededReads({ threads });
  const getIssue = spyOn(api, "getIssue").mockImplementation(async (key) => {
    if (key === OTHER_KEY) throw new ApiError(404, { code: "NOT_FOUND", error: "no such issue" });
    return seededIssue();
  });
  const newPageClient = () => (signedIn ? signedInAsReader(newClient()) : newClient());
  let client = newPageClient();
  const createComment = spyOn(api, "createComment");
  if (stopped !== undefined) {
    await stopSend(client, stopped.key, stopped.request, [createComment]);
  }
  createComment.mockRejectedValue(REFUSAL);
  const draw = () =>
    drawPage(client, "conversation", {
      beside: (
        <>
          <OpenComposer />
          <StatementRow />
        </>
      ),
      width,
    });
  const drawn = async (view: DrawnPage) => {
    await waitFor(() =>
      expect(stageRow(view), "the stage draws no row of the statement").not.toBeNull()
    );
  };
  let view = draw();
  await drawn(view);
  return {
    reload: async () => {
      endAsAReload(() => view.finish());
      client = newPageClient();
      view = draw();
      await drawn(view);
    },
    restore: () => {
      view.finish();
      client.clear();
      for (const stub of [getIssue, createComment, ...stubs]) stub.mockRestore();
    },
    user,
    view: () => view,
  };
}

/** The column of comments on text, or the statement's sheet, where it is drawn. */
function placeOf(view: DrawnPage, place: Place): HTMLElement | null {
  return place === "column"
    ? view.queryByRole("region", { name: MARGIN_WORDS.region })
    : view.container.querySelector<HTMLElement>(`[data-case-row] [${TEXT_THREADS_ATTRIBUTE}]`);
}

/** The composer of this part that `place` draws, or null. */
function composerAt(view: DrawnPage, place: Place): HTMLElement | null {
  return placeOf(view, place)?.querySelector<HTMLElement>(`[${TEXT_COMPOSER_ATTRIBUTE}]`) ?? null;
}

/** The composer of this part in `place`, once it is drawn. */
async function composerIn(view: DrawnPage, place: Place): Promise<HTMLElement> {
  return waitFor(() => {
    const box = composerAt(view, place);
    expect(box, `the ${place} draws no composer`).not.toBeNull();
    return box as HTMLElement;
  });
}

/** The field of the composer in `box`. */
function fieldOf(box: HTMLElement): HTMLTextAreaElement {
  return within(box).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
}

/** What the composer in `box` holds: its text, and the note it shows about a send it holds. */
function heldBy(box: HTMLElement): { note: Held | "none"; text: string } {
  return { note: noteIn(box), text: fieldOf(box).value };
}

/** The words the composer in `box` is open on, as its quote draws them, or null. */
function quoteOf(box: HTMLElement | null): string | null {
  return box?.querySelector("blockquote")?.textContent ?? null;
}

/** The card of the statement's thread in `place`, or null. */
function cardAt(view: DrawnPage, place: Place): HTMLElement | null {
  return (
    placeOf(view, place)?.querySelector<HTMLElement>(
      `[data-margin-item="${CSS.escape(ON_STATEMENT.comment.id)}"]`
    ) ?? null
  );
}

/** The card of the statement's thread in `place`, once it is drawn. */
async function cardIn(view: DrawnPage, place: Place): Promise<HTMLElement> {
  return waitFor(() => {
    const card = cardAt(view, place);
    expect(card, `the ${place} draws no card of the statement's thread`).not.toBeNull();
    return card as HTMLElement;
  });
}

/** Opens the composer on the statement's `words`, as the control does. */
async function openOn(page: Page, words: keyof typeof WORDS): Promise<void> {
  await page.user.click(page.view().getByRole("button", { name: `Comment on ${words}` }));
}

/** Opens the composer on the statement's words `page` in `place` and puts `BODY` in it, in the
 *  state `held` names; a stopped comment is the one `openPage` seeded, which the composer opens
 *  with. */
async function holdInComposer(page: Page, place: Place, held: Held): Promise<void> {
  await openOn(page, "page");
  const box = await composerIn(page.view(), place);
  if (held === "stopped") {
    await waitFor(() =>
      expect(noteIn(box), "the composer opens without its stopped send").toBe(held)
    );
    return;
  }
  await page.user.click(fieldOf(box));
  await page.user.paste(BODY);
  if (held === "refused") {
    await page.user.click(within(box).getByRole("button", { name: "Send" }));
    await within(box).findByText(REFUSED_NOTE);
  }
}

/** The composer's anchor on the statement's words `page`, which its send names. */
const PAGE_ANCHOR = {
  artifact: SPEC_DOCUMENT.id,
  block_id: STATEMENT,
  occurrence: 0,
  quote: "page",
} as const;

const stoppedComposer = (held: Held): Stopped | undefined =>
  held === "stopped"
    ? { key: COMPOSER_KEY, request: sentRequest(BODY, null, { anchor: PAGE_ANCHOR }) }
    : undefined;

/** Puts `BODY` in the reply of the statement's thread in `place`, in the state `held` names; a
 *  stopped reply is the one `openPage` seeded, which the card draws with its reply. */
async function holdInReply(page: Page, place: Place, held: Held): Promise<void> {
  const card = await cardIn(page.view(), place);
  if (held === "stopped") {
    await waitFor(() => expect(noteIn(card), "the reply draws no stopped send").toBe(held));
    return;
  }
  const field = replyField(card);
  await page.user.click(field);
  await page.user.paste(BODY);
  if (held === "refused") {
    const form = field.closest("form");
    if (form === null) throw new Error("the reply field is in no form");
    await page.user.click(within(form).getByRole("button", { name: "Send" }));
    await within(card).findByText(REFUSED_NOTE);
  }
}

const stoppedReply = (held: Held): Stopped | undefined =>
  held === "stopped"
    ? {
        key: marginReplySendKey(ON_STATEMENT.comment.id),
        request: sentRequest(BODY, {
          actor: ON_STATEMENT.comment.author,
          excerpt: "",
          id: ON_STATEMENT.comment.id,
          parentKind: "comment",
        }),
      }
    : undefined;

/** Goes to the issue's Plan, which draws no row of the statement, and back to its Conversation;
 *  `away` holds while Plan is drawn. */
async function anotherViewAndBack(page: Page, away: () => void): Promise<void> {
  await act(async () => page.view().held.navigate?.(presetPath(KEY, "plan")));
  await waitFor(() => {
    expect(stageRow(page.view()), "Plan draws a row of the statement").toBeNull();
    away();
  });
  await act(async () => page.view().held.navigate?.(presetPath(KEY, "conversation")));
  await waitFor(() =>
    expect(stageRow(page.view()), "Conversation draws no row of the statement again").not.toBeNull()
  );
}

/** Goes to another issue's address, whose page draws that it was not found, and back to CORE-1's
 *  Conversation. */
async function issueLeftAndBack(page: Page): Promise<void> {
  await act(async () => page.view().held.navigate?.(presetPath(OTHER_KEY, "conversation")));
  await page.view().findByRole("heading", { name: "Issue not found" });
  expect(composerAt(page.view(), "column"), "a composer stays on another issue").toBeNull();
  expect(composerAt(page.view(), "sheet"), "a composer stays on another issue").toBeNull();
  await act(async () => page.view().held.navigate?.(presetPath(KEY, "conversation")));
  await waitFor(() =>
    expect(
      stageRow(page.view()),
      "CORE-1's page draws no row of the statement again"
    ).not.toBeNull()
  );
}

/** Presses Escape once in the composer in `place`, which closes it in every state: a refusal's
 *  or a stop's alert takes no key. */
async function escapeComposer(page: Page, place: Place): Promise<void> {
  await page.user.click(fieldOf(await composerIn(page.view(), place)));
  await page.user.keyboard("{Escape}");
  await waitFor(() =>
    expect(composerAt(page.view(), place), "Escape left the composer open").toBeNull()
  );
}

// ---------------------------------------------------------------------------------------------
// The composer a selection opens, in the column from 1024 px and in the statement's sheet below.

const COMPOSER_HOSTS = [
  { host: "selection composer", place: "column", width: DESKTOP_WIDTH },
  { host: "selection composer, below 1024 px", place: "sheet", width: BELOW_PANE },
] as const;

// Escape closes the composer with its draft kept (`onLeave`), which takes it off the page: the
// column or the sheet draws no composer and, with no thread, nothing. The composer opened again on
// the same words holds the draft with its note.
for (const { host, place, width } of COMPOSER_HOSTS) {
  for (const held of HELD) {
    test(`${host} · Escape, then opened again · ${held}`, async () => {
      const page = await openPage({ stopped: stoppedComposer(held), threads: [], width });
      try {
        await holdInComposer(page, place, held);
        await escapeComposer(page, place);
        await openOn(page, "page");
        expect(heldBy(await composerIn(page.view(), place))).toEqual(expectedBack(held));
      } finally {
        page.restore();
      }
    });
  }
}

// Other words of the statement open another composer in the first one's place, under their own
// send name, which starts without the first one's draft; opening the first words again draws
// their composer with it.
for (const { host, place, width } of COMPOSER_HOSTS) {
  for (const held of HELD) {
    test(`${host} · opened on other words, then back · ${held}`, async () => {
      const page = await openPage({ stopped: stoppedComposer(held), threads: [], width });
      try {
        await holdInComposer(page, place, held);
        await openOn(page, "plan");
        const away = await waitFor(() => {
          const box = composerAt(page.view(), place);
          expect(quoteOf(box), "the composer did not open on the other words").toBe("plan");
          return heldBy(box as HTMLElement);
        });
        await openOn(page, "page");
        const back = await waitFor(() => {
          const box = composerAt(page.view(), place);
          expect(quoteOf(box), "the composer did not open on the first words again").toBe("page");
          return heldBy(box as HTMLElement);
        });
        expect({ away, back }).toEqual({
          away: { note: "none", text: "" },
          back: expectedBack(held),
        });
      } finally {
        page.restore();
      }
    });
  }
}

// Another view draws no row of the statement and keeps the composer open where that row last
// stood: in the column, and in the statement's own row here, so the same composer comes back.
for (const { host, place, width } of COMPOSER_HOSTS) {
  for (const held of HELD) {
    test(`${host} · another view, then back · ${held}`, async () => {
      const page = await openPage({ stopped: stoppedComposer(held), threads: [], width });
      try {
        await holdInComposer(page, place, held);
        await anotherViewAndBack(page, () =>
          expect(composerAt(page.view(), place), "another view closed the composer").not.toBeNull()
        );
        expect(heldBy(await composerIn(page.view(), place))).toEqual(expectedBack(held));
      } finally {
        page.restore();
      }
    });
  }
}

// Another issue's address draws another page in this one's place (`key={route.key}`), and the
// column closes the composer as its page goes; the composer opened again on the same words, back
// on CORE-1, holds the draft with its note.
for (const { host, place, width } of COMPOSER_HOSTS) {
  for (const held of HELD) {
    test(`${host} · issue left, then back · ${held}`, async () => {
      const page = await openPage({ stopped: stoppedComposer(held), threads: [], width });
      try {
        await holdInComposer(page, place, held);
        await issueLeftAndBack(page);
        expect(composerAt(page.view(), place), "the composer is open again on return").toBeNull();
        await openOn(page, "page");
        expect(heldBy(await composerIn(page.view(), place))).toEqual(expectedBack(held));
      } finally {
        page.restore();
      }
    });
  }
}

// Below 1024 px the column draws nothing and the statement's sheet draws the composer, at the
// same send name, and from 1024 px the other way round: the draft is in the sheet's composer,
// and in the column's again, with its note.
for (const held of HELD) {
  test(`selection composer · the window narrows below 1024 px, then widens · ${held}`, async () => {
    const page = await openPage({
      stopped: stoppedComposer(held),
      threads: [],
      width: DESKTOP_WIDTH,
    });
    try {
      await holdInComposer(page, "column", held);
      await page.view().window.set(BELOW_PANE);
      const narrow = heldBy(await composerIn(page.view(), "sheet"));
      await page.view().window.set(DESKTOP_WIDTH);
      const wide = heldBy(await composerIn(page.view(), "column"));
      expect({ narrow, wide }).toEqual({ narrow: expectedBack(held), wide: expectedBack(held) });
    } finally {
      page.restore();
    }
  });
}

// A reload ends the whole page and loads it again over the same tab (`endAsAReload`), with the
// reader signed in as before; the page it loads has no composer open, and the composer opened
// again on the same words holds the draft with its note.
for (const held of HELD) {
  test(`selection composer · the page reloaded, then back · ${held}`, async () => {
    const page = await openPage({
      signedIn: true,
      stopped: stoppedComposer(held),
      threads: [],
      width: DESKTOP_WIDTH,
    });
    try {
      await holdInComposer(page, "column", held);
      await page.reload();
      expect(composerAt(page.view(), "column"), "the reload opened a composer").toBeNull();
      await openOn(page, "page");
      expect(heldBy(await composerIn(page.view(), "column"))).toEqual(expectedBack(held));
    } finally {
      page.restore();
    }
  });
}

// ---------------------------------------------------------------------------------------------
// The reply composer of the statement's thread.

// Below 1024 px the statement's sheet draws the thread's card in the column's place, and from
// 1024 px the other way round: the reply, under its thread's reply send name, is in the sheet's
// card and in the column's again, with its note.
for (const held of HELD) {
  test(`text thread reply · the window narrows below 1024 px, then widens · ${held}`, async () => {
    const page = await openPage({
      stopped: stoppedReply(held),
      threads: TEXT_THREADS,
      width: DESKTOP_WIDTH,
    });
    try {
      await holdInReply(page, "column", held);
      await page.view().window.set(BELOW_PANE);
      const narrow = heldIn(await cardIn(page.view(), "sheet"));
      await page.view().window.set(DESKTOP_WIDTH);
      const wide = heldIn(await cardIn(page.view(), "column"));
      expect({ narrow, wide }).toEqual({ narrow: expectedBack(held), wide: expectedBack(held) });
    } finally {
      page.restore();
    }
  });
}

// Another view draws no row of the statement, so no card of its thread (`drawn by placed.map`,
// `drawn by threads.map`), which unmounts its reply; the card drawn again on return holds the
// reply with its note.
for (const { host, place, width } of [
  { host: "text thread reply", place: "column", width: DESKTOP_WIDTH },
  { host: "text thread reply, below 1024 px", place: "sheet", width: BELOW_PANE },
] as const) {
  for (const held of HELD) {
    test(`${host} · another view, then back · ${held}`, async () => {
      const page = await openPage({ stopped: stoppedReply(held), threads: TEXT_THREADS, width });
      try {
        await holdInReply(page, place, held);
        await anotherViewAndBack(page, () =>
          expect(cardAt(page.view(), place), "another view draws the statement's thread").toBeNull()
        );
        expect(heldIn(await cardIn(page.view(), place))).toEqual(expectedBack(held));
      } finally {
        page.restore();
      }
    });
  }
}

// Another issue's address draws another page in this one's place, with no card of the thread;
// the card drawn again on CORE-1 holds the reply with its note.
for (const held of HELD) {
  test(`text thread reply · issue left, then back · ${held}`, async () => {
    const page = await openPage({
      stopped: stoppedReply(held),
      threads: TEXT_THREADS,
      width: DESKTOP_WIDTH,
    });
    try {
      await holdInReply(page, "column", held);
      await issueLeftAndBack(page);
      expect(heldIn(await cardIn(page.view(), "column"))).toEqual(expectedBack(held));
    } finally {
      page.restore();
    }
  });
}
