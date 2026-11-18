/**
 * The draft matrix, margin thread row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and "a move of the host's ... carries nothing: the text stays with the address it was
 * written for, and is there when the reader comes back to it." A refused or stopped send's note
 * stays at its own address. A margin thread's reply has no other composer for its own controls to
 * carry the draft to, so its Cancel reply and Escape end the reply and keep the draft for the
 * thread, which the closed card says (`keptReply`); an anchored composer's Close asks `Discard
 * draft?` first, and only Discard drops what the reader wrote. Each cell writes a draft in one
 * state, typed and never sent, refused by Dispatch, or stopped by the reader, makes one move, and
 * asserts where the text ends.
 *
 * Host: the issue's margin (`Margin`, `useMarginSheet`, `CommentsTab`): a thread's reply in the
 * desktop list, where opening one thread closes the other, and in the phone's full-screen thread
 * view, which Back leaves; and the margin's own anchored compose, the one composer of the six
 * hosts that draws Close.
 *
 * A host move that takes the composer off the screen is checked on return alone, where the field
 * holds the draft with its refusal or stopped note; a host move that keeps the same composer on
 * screen at another address is checked on return too and, while away, for a line on screen that
 * says the earlier text is kept and names its comment's author or words.
 *
 * | Move                                         | typed  | refused | stopped |
 * | -------------------------------------------- | ------ | ------- | ------- |
 * | Back, then back (phone thread view) (off)    | cell   | cell    | cell    |
 * | Another thread opened, then back (off)       | cell   | cell    | cell    |
 * | Document left, then back (the compose) (off) | cell   | cell    | cell    |
 * | Compose on another document, then back (off) | cell   | cell    | cell    |
 * | Host moves it over a refusal (elsewhere)     | no (1) | no (1)  | no (1)  |
 * | Thread collapsed by the host                 | no (2) | no (2)  | no (2)  |
 * | Cancel reply                                 | cell   | cell    | cell    |
 * | Escape in the field                          | cell   | cell    | cell    |
 * | Close, then Discard draft? (the compose)     | cell   | cell    | cell    |
 * | Agents pick                                  | no (3) | no (3)  | no (3)  |
 * | Typed while elsewhere, then unmounted        | -      | no (1)  | -       |
 * | Hidden by the host, mounted                  | no (4) | no (4)  | no (4)  |
 * | The margin not drawn, then drawn again       | no (5) | no (5)  | no (5)  |
 * | The thread list not drawn, then drawn again  | no (6) | no (6)  | no (6)  |
 * | The document's threads change                | no (7) | no (7)  | no (7)  |
 * | A thread's replies ordered the other way     | no (8) | no (8)  | no (8)  |
 *
 * (off) marks a host move that takes the composer off the screen: Back hides the thread view,
 * opening another thread collapses this one and unmounts its reply, and leaving the document
 * takes the margin to another document's composes. Every host move of this row is one, so none
 * needs a line while away. Opening a compose on another document does what leaving does: the
 * margin holds one open compose per document (`composeForMark` replaces only the compose on its
 * own document), so the first document's compose stays open for it and comes back with its text
 * and its note, and that text never fills a compose at another quote: the next compose on the
 * first document, at another selection, starts without it.
 *
 * On a phone the margin is a panel over the document (`MarginSheet`'s compact sheet), which the
 * reader opens and closes and the margin opens for each compose it shows, and a thread opens
 * full-screen over it. The compose's moves there are rows of their own, each checked on return
 * for the panel open on the compose, with its text and its note, and nothing opened by hand:
 *
 * | Phone move (the margin's compose)              | typed | refused | stopped |
 * | ---------------------------------------------- | ----- | ------- | ------- |
 * | Panel closed, document left, then back (off)   | cell  | cell    | cell    |
 * | Compose on another document, then back (off)   | cell  | cell    | cell    |
 * | A thread opened over it, then Back (hidden)    | cell  | cell    | cell    |
 *
 * (hidden) marks a host move that keeps the compose mounted but out of sight.
 *
 * The moves this host cannot make:
 * 1. A thread's reply always answers its own thread's root, so no move of the host's changes its
 *    address, and it is never elsewhere.
 * 2. The margin's card draws no collapse of its own: the desktop list closes it only by opening
 *    another thread (the second row), and the phone only by Back (the first row).
 * 3. The issue picker is an Agents row's (`AgentMessageComposer`), and the margin draws no Agents
 *    row's composer.
 *
 * The moves the composer census found that the rows above do not make:
 * 4. The Pinned tab, and above the compact layout the rail the margin collapses to, hide the
 *    compose (`MarginComposerSlot`'s `hidden`, `MarginSheet`'s `covered`, `Margin`'s `railShown`,
 *    which also opens the rail for each compose the margin shows) and keep it mounted at its
 *    address, so nothing it holds moves; a phone thread hides it the same way, the third phone
 *    row.
 * 5. On a page with no margin above the compact layout `Margin` draws nothing, which unmounts the
 *    compose and every card as leaving the document does; below it `Margin` stays drawn there with
 *    no document, and so with no compose, which is the shown document's (`useMarginSheet`). The
 *    compose is the provider's (`MarginProvider`) and a reply's draft is held under its name, as
 *    the document-left rows' and the another-thread row's returns show.
 * 6. The Comments tab's list is not drawn on the Pinned tab, under a phone margin thread, or while
 *    the comments read failed (`MarginSheet`, `CommentsTab`), which unmounts a thread's reply and
 *    its editor as opening another thread does (the second row, and the edit file's margin card).
 * 7. A thread's card is drawn per thread of the open document, keyed by it (`ThreadList`): a
 *    thread that leaves the list, by its resolution or another document's list, unmounts its card
 *    as opening another thread does, and a resolved thread folded away under Show resolved is the
 *    same; its reply is held by its thread's name.
 * 8. Which end of a margin thread its reply sits at (`composerFirst`, from `useThreadOrder` in
 *    `ThreadCard`) is read when the card is drawn and again only when who is signed in changes
 *    (`useUserPreference`), so it never moves a reply that is on screen.
 *
 * The forms the composer census found, where the layout draws, hides or moves these composers
 * otherwise than the rows above, which the rows do not make:
 * 9. The phone panel's being open or closed is the reader's own (its toggle, its backdrop and
 *    Escape, `toggleSheet`) and moves no address: closed, only the stylesheet hides what is in it
 *    (`hidden xl:block`), so the compose and every card, reply and editor stay mounted under it,
 *    and opening it shows them as they were. Closing it closes a phone thread as Back does
 *    (`closeThread`), the first row. The margin opens it for each compose it shows
 *    (`openCompactSheet`), which the first two phone rows assert.
 * 10. On a phone a card's tap and a mark's click open the thread full-screen in place of opening
 *    it in the list (`onToggleThread`, `selectMarginItem`), so the first row's Back is the way off
 *    a thread there, and another thread is opened after it; a comment or ask link only selects
 *    its card (`openPhoneThread` false), which moves no composer.
 * 11. Where the margin sits moves no composer: the desktop column's width, class and resize
 *    handle, the compact layout's `contents` class (`Margin`), and the panel's dialog role, its
 *    place at the foot of the screen and its backdrop (`MarginSheet`).
 * 12. The rest is focus and scrolling: the compose takes focus once it is on screen (`onScreen`,
 *    `autoFocus`), the margin scrolls the compose or a linked card into view only in an open panel
 *    (`useMarginListeners`, `useCardHold`), and an open panel or thread view keeps focus inside
 *    it and the page still under it (`useDialog`). None moves an address or ends a mount. The
 *    app shell's forms, above every host, are the sign-in file's.
 *
 * The address prop the composer census found that no move of the host's changes:
 * 13. The compose's kind (`composer.kind`) changes only by the compose's own Comment, Suggest and
 *    Ask switch (`onKindChange`), the reader's own control, which retypes the same mark
 *    (`retypeCompose`) and leaves the draft in the field (`useKeptDrafts`), so it re-addresses
 *    the same compose and moves nothing it holds.
 *
 * `draft-matrix.margin.coverage.ts` holds each census item this file answers for to a test above
 * or to a numbered reason, and is the source this header is the prose of.
 */
import { expect, type Mock, spyOn, test } from "bun:test";
import type { MutationKey } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useEffect } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import {
  BODY,
  expectedBack,
  HELD,
  type Held,
  keptInSight,
  marginCard,
  marginClient,
  marginPage,
  noteIn,
  openMarginThread,
  REFUSAL,
  REFUSED_NOTE,
  RegisterDocument,
  STOPPED_NOTE,
  threadA,
  threadB,
} from "../../__tests__/draft-matrix-fixture";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import type { SentRequest } from "../conversation/send-request";
import { buildIssuePath } from "../refs/routes";
import { marginComposeSendKey, marginReplySendKey, useMargin } from "./margin-context";
import {
  DESKTOP_WIDTH,
  issue,
  PHONE_WIDTH,
  specArtifact,
  stubDocumentBridge,
  stubViewport,
} from "./margin-fixture";

const HOST = "margin thread";

/** Another issue, whose spec is another document: going there leaves CORE-1's spec. */
const secondIssue: IssueDetails = {
  ...issue,
  artifacts: [{ ...specArtifact, id: "artifact-2", issue_key: "CORE-2" }],
  key: "CORE-2",
  number: 2,
  primary_artifact_id: "artifact-2",
  title: "Second issue",
};

/** The words the margin's compose is anchored to, as the selection bar hands them over. */
const MARK_ANCHOR = {
  artifact: specArtifact.id,
  mark_id: "m-9",
  placement: { blockId: "b-1", prefix: "The quick brown fox ", readings: 1, suffix: "." },
  quote: "jumps over the lazy dog",
};

/** Words of CORE-2's spec, where the reader opens a compose while away from CORE-1's. */
const SECOND_DOCUMENT_ANCHOR = {
  artifact: "artifact-2",
  mark_id: "m-20",
  placement: { blockId: "b-1", prefix: "The second ", readings: 1, suffix: "." },
  quote: "spec of another issue",
};

/** Other words of CORE-1's spec, where the reader opens the next compose back there. */
const ANOTHER_ANCHOR = {
  artifact: specArtifact.id,
  mark_id: "m-10",
  placement: { blockId: "b-1", prefix: "The ", readings: 1, suffix: " fox jumps" },
  quote: "quick brown",
};

/** A send of `BODY` from the margin: thread A's reply, or the compose on `MARK_ANCHOR`. */
function request(kind: "reply" | "compose"): SentRequest {
  return kind === "reply"
    ? sentRequest(BODY, {
        actor: threadA.author,
        excerpt: "",
        id: threadA.id,
        parentKind: "comment",
      })
    : sentRequest(BODY, null, { anchor: MARK_ANCHOR });
}

/** The document the page on the route shows, registered as each document page registers its
 *  own: CORE-1's spec, or CORE-2's while the reader is on the second issue. */
function RegisterRouteDocument(): ReactNode {
  const { registerDocument } = useMargin();
  const { pathname } = useLocation();
  const artifactId =
    pathname === buildIssuePath({ key: secondIssue.key, kind: "issue" })
      ? "artifact-2"
      : specArtifact.id;
  useEffect(() => {
    registerDocument(stubDocumentBridge(artifactId));
    return () => registerDocument(undefined);
  }, [artifactId, registerDocument]);
  return null;
}

/** The selection bar's Comment on `MARK_ANCHOR`, on a press, and on the other words the cells
 *  compose on: CORE-2's, and others of CORE-1's. */
function ComposeOnMark(): ReactNode {
  const { composeForMark } = useMargin();
  const composeOn = (anchor: typeof MARK_ANCHOR) => {
    void composeForMark({ anchor, kind: "comment" }).catch(() => {});
  };
  return (
    <>
      <button onClick={() => composeOn(MARK_ANCHOR)} type="button">
        Compose on the selection
      </button>
      <button onClick={() => composeOn(SECOND_DOCUMENT_ANCHOR)} type="button">
        Compose on the second document
      </button>
      <button onClick={() => composeOn(ANOTHER_ANCHOR)} type="button">
        Compose on another selection
      </button>
    </>
  );
}

/** The reader's way off CORE-1's spec and back, as a link to another issue takes them. */
function Travel(): ReactNode {
  const navigate = useNavigate();
  return (
    <>
      <button
        onClick={() => navigate(buildIssuePath({ key: secondIssue.key, kind: "issue" }))}
        type="button"
      >
        Open second issue
      </button>
      <button
        onClick={() => navigate(buildIssuePath({ key: issue.key, kind: "issue" }))}
        type="button"
      >
        Back to the first issue
      </button>
    </>
  );
}

interface Page {
  readonly createComment: Mock<typeof api.createComment>;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/**
 * The margin of CORE-1's spec over threads A and B, at a phone width or above it. Every comment
 * the margin writes is refused, so a send pressed here comes back refused; `stopped`, when given,
 * is sent through the held-send store under its name before the margin is drawn and stopped once
 * late, as the reader's Stop trying does, so the margin arrives showing it. `documents` says which
 * document stays registered: CORE-1's spec on every route, or the route's own.
 */
async function openMargin(
  phone: boolean,
  stopped?: { readonly key: MutationKey; readonly request: SentRequest },
  documents: "spec" | "route" = "spec"
): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = marginClient([threadA, threadB]);
  queryClient.setQueryData(["issue", secondIssue.key], secondIssue);
  queryClient.setQueryData(["asks", secondIssue.key], []);
  queryClient.setQueryData(["comments", secondIssue.key], []);
  // Each issue's reads answer what the cache was seeded with, should anything read them again.
  const reads = [
    spyOn(api, "getIssue").mockImplementation(async (key) =>
      key === secondIssue.key ? secondIssue : issue
    ),
    spyOn(api, "listComments").mockImplementation(async (key) =>
      key === secondIssue.key ? [] : [threadA, threadB]
    ),
  ];
  const createComment = spyOn(api, "createComment");
  if (stopped !== undefined) {
    await stopSend(queryClient, stopped.key, stopped.request, [createComment]);
  }
  createComment.mockRejectedValue(REFUSAL);
  const restoreViewport = stubViewport(phone ? PHONE_WIDTH : DESKTOP_WIDTH);
  const view = render(
    marginPage(
      queryClient,
      <>
        {documents === "spec" ? <RegisterDocument /> : <RegisterRouteDocument />}
        <ComposeOnMark />
        <Travel />
      </>
    )
  );
  // A phone that arrives at a held reply opens its thread in place of the list, so any card of
  // the margin's will do.
  await waitFor(() =>
    expect(document.querySelector('[data-testid^="margin-comment-"]')).not.toBe(null)
  );
  return {
    createComment,
    restore: () => {
      view.unmount();
      restoreViewport();
      createComment.mockRestore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

const replyField = (place: HTMLElement) =>
  within(place).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });

/** Writes `BODY` in the reply field of `place` and, for a refused draft, sends it. */
async function writeReply(page: Page, place: HTMLElement, held: Held): Promise<void> {
  const field = replyField(place);
  await page.user.click(field);
  await page.user.paste(BODY);
  if (held === "refused") {
    const form = field.closest("form");
    if (form === null) throw new Error("expected the reply form");
    await page.user.click(within(form).getByRole("button", { name: "Send" }));
    await within(place).findByText(REFUSED_NOTE);
  }
}

/** Thread A open in the desktop list with `BODY` in its reply, in the state `held` names. A
 *  stopped reply is the one `openMargin` seeded, whose thread the margin opens on arrival. */
async function holdInCardA(page: Page, held: Held): Promise<void> {
  await waitFor(() => expect(marginCard(threadA.id)).toBeInTheDocument());
  if (marginCard(threadA.id).getAttribute("aria-expanded") !== "true") {
    await openMarginThread(page.user, threadA.id);
  }
  if (held === "stopped") {
    expect(noteIn(marginCard(threadA.id))).toBe("stopped");
    return;
  }
  await writeReply(page, marginCard(threadA.id), held);
}

/** Thread A in the phone's thread view with `BODY` in its reply. Answers the thread view. */
async function holdInPhoneThreadA(page: Page, held: Held): Promise<HTMLElement> {
  if (held === "stopped") {
    // Coming to a document whose thread holds a reply opens that thread.
    const dialog = await screen.findByRole("dialog", { name: "Thread" });
    expect(noteIn(dialog)).toBe("stopped");
    return dialog;
  }
  await openMarginThread(page.user, threadA.id);
  const dialog = await screen.findByRole("dialog", { name: "Thread" });
  await writeReply(page, dialog, held);
  return dialog;
}

/** Thread A's reply as it shows again in `place`. */
function heldIn(place: HTMLElement): { note: Held | "none"; text: string } {
  return { note: noteIn(place), text: replyField(place).value };
}

const stoppedReply = (held: Held) =>
  held === "stopped"
    ? { key: marginReplySendKey(threadA.id), request: request("reply") }
    : undefined;

// A phone thread's Back is the host's move and takes the reply off the screen: the reply is held
// for its thread, and opening the thread again brings it back with its note.
for (const held of HELD) {
  test(`${HOST} · Back, then back (phone thread view) · ${held}`, async () => {
    const page = await openMargin(true, stoppedReply(held));
    try {
      const dialog = await holdInPhoneThreadA(page, held);
      await page.user.click(within(dialog).getByRole("button", { name: "Back" }));
      await waitFor(() => expect(screen.queryByRole("dialog", { name: "Thread" })).toBe(null));
      // The thread view a tap on the card opens, or the card itself where the list shows it
      // open already.
      let place = marginCard(threadA.id);
      if (place.getAttribute("aria-expanded") !== "true") {
        await openMarginThread(page.user, threadA.id);
        place = await screen.findByRole("dialog", { name: "Thread" });
      }
      expect(heldIn(place)).toEqual(expectedBack(held));
    } finally {
      page.restore();
    }
  });
}

// Opening another thread in the desktop list closes this one and unmounts its reply: the host's
// move. The reply is held for thread A, and opening A again brings it back with its note.
for (const held of HELD) {
  test(`${HOST} · another thread opened, then back · ${held}`, async () => {
    const page = await openMargin(false, stoppedReply(held));
    try {
      await holdInCardA(page, held);
      await openMarginThread(page.user, threadB.id);
      const aOpen = marginCard(threadA.id).getAttribute("aria-expanded");
      await openMarginThread(page.user, threadA.id);
      expect({ aOpen, back: heldIn(marginCard(threadA.id)) }).toEqual({
        aOpen: "false",
        back: expectedBack(held),
      });
    } finally {
      page.restore();
    }
  });
}

// Cancel reply and Escape in the field are the reader's own, and a margin reply has no other
// composer to take its draft to: the reply ends, the draft is kept for the thread with its note,
// the closed card says so, and opening the thread brings it back.
for (const held of HELD) {
  test(`${HOST} · Cancel reply · ${held}`, async () => {
    const page = await openMargin(false, stoppedReply(held));
    try {
      await holdInCardA(page, held);
      await page.user.click(
        within(marginCard(threadA.id)).getByRole("button", { name: "Cancel reply" })
      );
      const kept = keptInSight(BODY);
      await openMarginThread(page.user, threadA.id);
      expect({ back: heldIn(marginCard(threadA.id)), kept }).toEqual({
        back: expectedBack(held),
        kept: true,
      });
    } finally {
      page.restore();
    }
  });
}

for (const held of HELD) {
  test(`${HOST} · Escape in the field · ${held}`, async () => {
    const page = await openMargin(false, stoppedReply(held));
    try {
      await holdInCardA(page, held);
      await page.user.click(replyField(marginCard(threadA.id)));
      await page.user.keyboard("{Escape}");
      const kept = keptInSight(BODY);
      await openMarginThread(page.user, threadA.id);
      expect({ back: heldIn(marginCard(threadA.id)), kept }).toEqual({
        back: expectedBack(held),
        kept: true,
      });
    } finally {
      page.restore();
    }
  });
}

/** The margin's compose, which the selection bar opened or the margin showed on arrival. */
const composeForm = () => screen.getByRole("textbox", { name: "Comment" }).closest("form");

const stoppedCompose = (held: Held) =>
  held === "stopped"
    ? { key: marginComposeSendKey(specArtifact.id), request: request("compose") }
    : undefined;

/** The margin's compose on `MARK_ANCHOR` with `BODY` in it, in the state `held` names; a stopped
 *  compose is the one `openMargin` seeded, which the margin shows on arrival. Answers its form. */
async function holdInCompose(page: Page, held: Held): Promise<HTMLElement> {
  if (held !== "stopped") {
    await page.user.click(screen.getByRole("button", { name: "Compose on the selection" }));
    const field = await screen.findByRole("textbox", { name: "Comment" });
    await page.user.click(field);
    await page.user.paste(BODY);
    if (held === "refused") {
      const form = composeForm();
      if (form === null) throw new Error("expected the compose form");
      await page.user.click(within(form).getByRole("button", { name: "Send" }));
      await within(form).findByText(REFUSED_NOTE);
    }
  }
  const form = await waitFor(() => {
    const found = composeForm();
    if (found === null) throw new Error("expected the compose form");
    return found;
  });
  expect(noteIn(form)).toBe(held === "typed" ? "none" : held);
  return form;
}

// Leaving the document is the host's move, and the compose is off the screen while the reader is
// on another issue. The margin keeps an unsent compose open for its document (`pendingComposes`,
// one per document), and a sent one is the held-send store's; coming back to the document shows
// either again (`revealCompose` in `useMarginSheet`), its draft in the field with its note.
for (const held of HELD) {
  test(`${HOST} · document left, then back (the margin's compose) · ${held}`, async () => {
    const page = await openMargin(false, stoppedCompose(held));
    try {
      await holdInCompose(page, held);
      await page.user.click(screen.getByRole("button", { name: "Open second issue" }));
      await waitFor(() => expect(screen.queryByTestId(`margin-comment-${threadA.id}`)).toBe(null));
      await page.user.click(screen.getByRole("button", { name: "Back to the first issue" }));
      await screen.findByTestId(`margin-comment-${threadA.id}`);
      // A second for the compose the margin shows on arrival; none on screen reads as absent.
      const field = await screen
        .findByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }, { timeout: 1_000 })
        .catch(() => null);
      expect({
        note: field === null ? "none" : noteIn(field.closest("form") ?? field),
        text: field === null ? "(no compose on screen)" : field.value,
      }).toEqual(expectedBack(held));
    } finally {
      page.restore();
    }
  });
}

/** Each compose on screen: the words it is anchored to, its field's text, and its note. */
function composesOnScreen(): { note: Held | "none"; quote: string; text: string }[] {
  return screen.queryAllByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).map((field) => {
    const form = field.closest("form") ?? field;
    return {
      note: noteIn(form),
      quote: form.querySelector("blockquote")?.textContent ?? "",
      text: field.value,
    };
  });
}

/** Whether a line the reader can see says the draft is kept and names where it was written:
 *  its words, or its issue. */
function keptLineNamingTheFirstCompose(): boolean {
  return screen.queryAllByRole("status").some((line) => {
    const text = line.textContent ?? "";
    return /\bkept\b/.test(text) && (text.includes(MARK_ANCHOR.quote) || text.includes(issue.key));
  });
}

/** What a cell asserts the reader finds back on CORE-1's spec. */
const FOUND = "the draft in its compose with its note, or a line saying where it is kept";

// A compose opened on another document is the host's move for the first one, which carries
// nothing. The margin holds one open compose per document (`composeForMark` replaces only the
// compose on its own document), so the first stays open for CORE-1's spec and, back there, shows
// again with its text and its note. The cell holds the margin to the rule rather than to that
// way of keeping it: back on CORE-1's spec the text is in its compose with its note, or a line
// says where it is kept; and it never fills a compose at another quote, so the next compose the
// reader opens on CORE-1's spec, at other words, starts without it.
for (const held of HELD) {
  test(`${HOST} · a compose opened on another document, then back (the margin's compose) · ${held}`, async () => {
    const page = await openMargin(false, stoppedCompose(held), "route");
    try {
      await holdInCompose(page, held);
      await page.user.click(screen.getByRole("button", { name: "Open second issue" }));
      await waitFor(() => expect(screen.queryByTestId(`margin-comment-${threadA.id}`)).toBe(null));
      await page.user.click(screen.getByRole("button", { name: "Compose on the second document" }));
      await screen.findByText(SECOND_DOCUMENT_ANCHOR.quote);
      await page.user.click(screen.getByRole("button", { name: "Back to the first issue" }));
      await screen.findByTestId(`margin-comment-${threadA.id}`);
      // A second for the compose the margin shows on arrival; none on screen reads as absent.
      await screen.findByRole("textbox", { name: "Comment" }, { timeout: 1_000 }).catch(() => null);
      const back = composesOnScreen().find((compose) => compose.text === BODY);
      const found =
        (back !== undefined &&
          back.quote === MARK_ANCHOR.quote &&
          back.note === expectedBack(held).note) ||
        keptLineNamingTheFirstCompose();
      await page.user.click(screen.getByRole("button", { name: "Compose on another selection" }));
      await screen.findByText(ANOTHER_ANCHOR.quote);
      const elsewhere = composesOnScreen()
        .filter((compose) => compose.text === BODY && compose.quote !== MARK_ANCHOR.quote)
        .map((compose) => compose.quote)
        .join(", ");
      expect({
        back: found ? FOUND : (back ?? "(no compose holds it, and no line says so)"),
        elsewhere,
      }).toEqual({
        back: FOUND,
        elsewhere: "",
      });
    } finally {
      page.restore();
    }
  });
}

// The margin's compose is anchored, so it draws Close, which asks over a draft not sent - typed,
// or handed back by a refusal or a stop - and only Discard drops it, its note with it.
for (const held of HELD) {
  test(`${HOST} · Close, then Discard draft? (the margin's compose) · ${held}`, async () => {
    const page = await openMargin(false, stoppedCompose(held));
    try {
      const form = await holdInCompose(page, held);
      await page.user.click(within(form).getByRole("button", { name: "Close" }));
      const asked = {
        prompt: within(form).queryByText("Discard draft?") !== null,
        text: within(form).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value,
      };
      // A role's name given as a string is matched whole, so this is not `Discard draft`.
      await page.user.click(within(form).getByRole("button", { name: "Discard" }));
      const after = {
        compose: screen.queryAllByRole("textbox", { name: "Comment" }).length,
        refused: screen.queryAllByText(REFUSED_NOTE).length,
        stopped: screen.queryAllByText(STOPPED_NOTE).length,
      };
      expect({ after, asked }).toEqual({
        after: { compose: 0, refused: 0, stopped: 0 },
        asked: { prompt: true, text: BODY },
      });
    } finally {
      page.restore();
    }
  });
}

/** What a phone reader finds of the margin's compose: whether the review panel is open, and the
 *  compose's words, text and note. On a phone the margin is a panel over the document
 *  (`MarginSheet`'s compact sheet), and while it is closed the stylesheet hides what is in it.
 *  That stylesheet is not loaded here, so a compose in a closed panel is still in the document,
 *  and only the panel's being open puts the compose in front of the reader. Waits a second for
 *  the panel the margin opens on arrival; none open reads as closed. */
async function composeOnPhone(): Promise<{
  note: Held | "none";
  panel: string;
  quote: string;
  text: string;
}> {
  const panel = await screen
    .findByRole("dialog", { name: "Review panel" }, { timeout: 1_000 })
    .catch(() => null);
  const field = screen.queryByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
  const form = field?.closest("form") ?? null;
  return {
    note: form === null ? "none" : noteIn(form),
    panel: panel === null ? "closed, so the reader has to open it" : "open",
    quote: form?.querySelector("blockquote")?.textContent ?? "",
    text: field === null ? "(no compose)" : field.value,
  };
}

/** The compose a phone row expects back: the panel open on it, at its own words, with the text
 *  and the note it had. */
const backOnPhone = (held: Held) => ({
  ...expectedBack(held),
  panel: "open",
  quote: MARK_ANCHOR.quote,
});

// On a phone the review panel covers the document, so the reader closes it to follow a link to
// another issue. Leaving the document is the host's move and carries nothing; coming back to it
// shows its compose again (`revealCompose` in `useMarginSheet`), and on a phone that opens the
// panel on the Comments tab (`openCompactSheet`): the reader finds the compose, its text and its
// note without opening anything.
for (const held of HELD) {
  test(`${HOST} · phone · panel closed, document left, then back (the margin's compose) · ${held}`, async () => {
    const page = await openMargin(true, stoppedCompose(held));
    try {
      await holdInCompose(page, held);
      await page.user.click(screen.getByRole("button", { name: /^Close review panel/ }));
      await page.user.click(screen.getByRole("button", { name: "Open second issue" }));
      await waitFor(() => expect(screen.queryByTestId(`margin-comment-${threadA.id}`)).toBe(null));
      await page.user.click(screen.getByRole("button", { name: "Back to the first issue" }));
      await screen.findByTestId(`margin-comment-${threadA.id}`);
      expect(await composeOnPhone()).toEqual(backOnPhone(held));
    } finally {
      page.restore();
    }
  });
}

// A compose opened on another document opens the panel for that document, so the panel the reader
// comes back to is closed for this one until the margin shows the compose again. The first
// document's compose stays open for it (`composeForMark` replaces only the compose on its own
// document), and coming back opens the panel on it with its text and its note.
for (const held of HELD) {
  test(`${HOST} · phone · a compose opened on another document, then back (the margin's compose) · ${held}`, async () => {
    const page = await openMargin(true, stoppedCompose(held), "route");
    try {
      await holdInCompose(page, held);
      await page.user.click(screen.getByRole("button", { name: "Open second issue" }));
      await waitFor(() => expect(screen.queryByTestId(`margin-comment-${threadA.id}`)).toBe(null));
      await page.user.click(screen.getByRole("button", { name: "Compose on the second document" }));
      await screen.findByText(SECOND_DOCUMENT_ANCHOR.quote);
      await page.user.click(screen.getByRole("button", { name: "Back to the first issue" }));
      await screen.findByTestId(`margin-comment-${threadA.id}`);
      expect(await composeOnPhone()).toEqual(backOnPhone(held));
    } finally {
      page.restore();
    }
  });
}

// On a phone a thread opens full-screen over the panel and hides the compose under it, mounted
// (`MarginSheet`'s `covered`). That is the host's move: the compose keeps its text and its note,
// and the thread's Back shows it again in the panel.
for (const held of HELD) {
  test(`${HOST} · phone · a thread opened over the compose, then Back (the margin's compose) · ${held}`, async () => {
    const page = await openMargin(true, stoppedCompose(held));
    try {
      await holdInCompose(page, held);
      await openMarginThread(page.user, threadB.id);
      const thread = await screen.findByRole("dialog", { name: "Thread" });
      const away = screen.queryAllByRole("textbox", { name: "Comment" }).length;
      await page.user.click(within(thread).getByRole("button", { name: "Back" }));
      await waitFor(() => expect(screen.queryByRole("dialog", { name: "Thread" })).toBe(null));
      expect({ away, back: await composeOnPhone() }).toEqual({ away: 0, back: backOnPhone(held) });
    } finally {
      page.restore();
    }
  });
}
