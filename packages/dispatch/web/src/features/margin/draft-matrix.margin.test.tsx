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
 * | A newer compose's offer, then Close          | cell   | cell    | cell    |
 * | A newer compose's offer, then Escape         | cell   | cell    | cell    |
 * | A newer suggestion's offer, issue closed     | cell   | cell    | cell    |
 * | A newer compose's offer, lapse and sign-in   | cell   | cell    | cell    |
 * | A typed suggestion, its issue closed         | cell   | -       | -       |
 * | A typed ask, its issue closed                | cell   | -       | -       |
 * | Agents pick                                  | no (3) | no (3)  | no (3)  |
 * | Typed while elsewhere, then unmounted        | -      | no (1)  | -       |
 * | Hidden by the host, mounted                  | no (4) | no (4)  | no (4)  |
 * | The margin not drawn, then drawn again       | no (5) | no (5)  | no (5)  |
 * | The thread list not drawn, then drawn again  | no (6) | no (6)  | no (6)  |
 * | The document's threads change                | no (7) | no (7)  | no (7)  |
 * | A thread's replies ordered the other way     | no (8) | no (8)  | no (8)  |
 * | The page reloaded, then back (off)           | cell   | cell    | cell    |
 * | The page reloaded, then back (the compose)   | cell   | cell    | cell    |
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
 * A newer compose on the document is the host's move for the one before it, and offers that
 * compose's draft beside its own empty field (`offered` in `useKeptDrafts`): typed, handed back by
 * a refusal, or the send the reader stopped, set aside (`marginComposeKeptKey`), which
 * `MarginComposerSlot` hands the newer compose to offer (`stoppedAside`). The offer rows end that
 * newer compose with the draft in front of the reader. Close and Escape ask `Discard draft?` over
 * it as over typed text, and Discard drops it with the field's draft, so a compose opened after
 * offers nothing. A suggestion or ask compose ends when its issue closes, which refuses both, but
 * one that offers a draft stays, the draft beside it, until the reader drops it. A sign-in after a
 * lapse draws the shell, and the margin's provider, again over the same
 * stores, so no compose is open: the send the reader stopped goes back under its compose's name
 * and shows on its document, at its words, with its note, and a typed draft, or a refused one
 * whose refusal went as the newer compose opened, is drawn on its document too, in a line saying
 * it is kept for its words, with its text (`MarginKeptDrafts`). So is what the reader typed in a
 * suggestion or ask compose that offered nothing when its issue closed and ended it; a refused or
 * stopped one is a send its compose still holds, which keeps that compose drawn.
 *
 * The reload rows end the whole page, as any reload does (`endAsAReload`), and load it again over
 * the same tab: a new query client and write store over the tab's `sessionStorage`, with the
 * reader signed in as before. They are the lifetimes the census finds (`lifetime.ts`) of a
 * thread's reply and of the margin's compose, which no page the reload loads has open: the margin
 * draws what it held on its document as it arrives there, as after a lapse.
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
 * The move the composer census found above the margin, in the shell, which no row makes:
 * 14. A render inside the margin that throws draws the shell's error section in its place
 *    (`ErrorBoundary`, "the margin", in `app.tsx`), which unmounts the compose and every card as
 *    leaving the document does. Each composer keeps what it holds as it goes (`useKeptDrafts`):
 *    the compose and an editor by their address (`keepAway`), a thread's reply under its
 *    thread's name, written at each keystroke (`unsent.write`), and what was typed over a
 *    refusal in its held send (`keepDraft`). Those stores live with the query client, above the
 *    boundary (`unsentDraftsOf`, `heldSends`), and so does the compose (`MarginProvider`). The
 *    boundary draws the margin again when the page's path changes (`resetKey`,
 *    `componentDidUpdate`), with no thread open and no editor (`useMarginSheet` starts again),
 *    and its Reload Dispatch reloads the page as the reload rows do. A composer drawn again at
 *    its address opens on its draft (`mountDraft`): the compose on its document, a thread's
 *    reply when its thread is opened, an editor when its comment's Edit is pressed (beside a
 *    newer body where the comment changed, `changedUnder`), as the document-left rows, the
 *    another-thread row and the edit file's margin card show.
 *
 * `draft-matrix.margin.coverage.ts` holds each census item this file answers for to a test above
 * or to a numbered reason, and is the source this header is the prose of.
 */
import { expect, type Mock, spyOn, test } from "bun:test";
import type { MutationKey, QueryClient } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";

import {
  BODY,
  endAsAReload,
  expectedBack,
  HELD,
  type Held,
  heldIn,
  keptInSight,
  marginCard,
  marginClient,
  marginPage,
  noteIn,
  openMarginThread,
  REFUSAL,
  REFUSED_NOTE,
  RegisterDocument,
  RegisterRouteDocument,
  replyField,
  STOPPED_NOTE,
  signedInAsReader,
  threadA,
  threadB,
} from "../../__tests__/draft-matrix-fixture";
import { sentRequest, stopSend } from "../../__tests__/stop-late-write";
import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import type { ComposerKind } from "../conversation/composer-model";
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

/** The documents each issue's page registers: CORE-1's spec, and CORE-2's. */
const ROUTE_DOCUMENTS = {
  first: stubDocumentBridge(specArtifact.id),
  second: stubDocumentBridge("artifact-2"),
};

/** The selection bar's Comment on `MARK_ANCHOR`, on a press, and on the other words the cells
 *  compose on: CORE-2's, and others of CORE-1's; its Suggest and Ask open a suggestion and an ask
 *  on `MARK_ANCHOR`, and its Suggest on the other words of CORE-1's a suggestion there. */
function ComposeOnMark(): ReactNode {
  const { composeForMark } = useMargin();
  const composeOn = (anchor: typeof MARK_ANCHOR, kind: ComposerKind = "comment") => {
    void composeForMark({ anchor, kind }).catch(() => {});
  };
  return (
    <>
      <button onClick={() => composeOn(MARK_ANCHOR)} type="button">
        Compose on the selection
      </button>
      <button onClick={() => composeOn(MARK_ANCHOR, "suggestion")} type="button">
        Suggest on the selection
      </button>
      <button onClick={() => composeOn(MARK_ANCHOR, "ask")} type="button">
        Ask on the selection
      </button>
      <button onClick={() => composeOn(SECOND_DOCUMENT_ANCHOR)} type="button">
        Compose on the second document
      </button>
      <button onClick={() => composeOn(ANOTHER_ANCHOR)} type="button">
        Compose on another selection
      </button>
      <button onClick={() => composeOn(ANOTHER_ANCHOR, "suggestion")} type="button">
        Suggest on another selection
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
  readonly queryClient: QueryClient;
  /** Takes the page away and draws it again over the same query client and stores, as a sign-in
   *  after a lapse draws the shell (`AuthGate`), and with it the margin's provider. */
  readonly redraw: () => void;
  /** Ends the page as a reload does and loads it again over the same tab, signed in as the same
   *  reader, with a new query client (`endAsAReload`, `signedInAsReader`). */
  readonly reload: () => Promise<void>;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/** Waits for a card of the margin's: a phone that arrives at a held reply opens its thread in
 *  place of the list, so any card will do. */
async function marginDrawn(): Promise<void> {
  await waitFor(() =>
    expect(document.querySelector('[data-testid^="margin-comment-"]')).not.toBe(null)
  );
}

/**
 * The margin of CORE-1's spec over threads A and B, at a phone width or above it. Every comment
 * the margin writes is refused, so a send pressed here comes back refused; `stopped`, when given,
 * is sent through the held-send store under its name before the margin is drawn and stopped once
 * late, as the reader's Stop trying does, so the margin arrives showing it. `documents` says which
 * document stays registered: CORE-1's spec on every route, or the route's own. With `signedIn`
 * the page is signed in as the reader, as a reload row's is, so the tab keeps what it sends.
 */
async function openMargin(
  phone: boolean,
  stopped?: { readonly key: MutationKey; readonly request: SentRequest },
  documents: "spec" | "route" = "spec",
  signedIn = false
): Promise<Page> {
  const user = userEvent.setup();
  const newClient = () => {
    const client = marginClient([threadA, threadB]);
    client.setQueryData(["issue", secondIssue.key], secondIssue);
    client.setQueryData(["asks", secondIssue.key], []);
    client.setQueryData(["comments", secondIssue.key], []);
    return signedIn ? signedInAsReader(client) : client;
  };
  let queryClient = newClient();
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
  const tree = (client: QueryClient) =>
    marginPage(
      client,
      <>
        {documents === "spec" ? (
          <RegisterDocument />
        ) : (
          <RegisterRouteDocument {...ROUTE_DOCUMENTS} />
        )}
        <ComposeOnMark />
        <Travel />
      </>
    );
  let view = render(tree(queryClient));
  await marginDrawn();
  return {
    createComment,
    get queryClient() {
      return queryClient;
    },
    redraw: () => {
      view.unmount();
      view = render(tree(queryClient));
    },
    reload: async () => {
      endAsAReload(() => view.unmount());
      queryClient = newClient();
      view = render(tree(queryClient));
      await marginDrawn();
    },
    restore: () => {
      view.unmount();
      restoreViewport();
      createComment.mockRestore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

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
// on another issue. The margin keeps an unsent compose open for its document (`composes`, one per
// document), and a sent one is the held-send store's; coming back to the document shows either
// again (`revealCompose` in `useMarginSheet`), its draft in the field with its note.
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

/** The margin's one compose on screen: the words it is anchored to, its field's text, and its
 *  note. Two composes on screen at once fail the query, as they should: the margin draws one. */
function composeShown(): { note: Held | "none"; quote: string; text: string } {
  const field = screen.queryByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
  const form = field?.closest("form") ?? null;
  return {
    note: form === null ? "none" : noteIn(form),
    quote: form?.querySelector("blockquote")?.textContent ?? "",
    text: field === null ? "(no compose)" : field.value,
  };
}

/** The compose a cell expects back: at its own words, `MARK_ANCHOR`'s, with the text and the note
 *  it had. */
const backAtItsWords = (held: Held) => ({ ...expectedBack(held), quote: MARK_ANCHOR.quote });

/** Whether a line the reader can see holds `BODY`, the draft written for `MARK_ANCHOR`'s words,
 *  and says it is kept there. A status inside something hidden is no role the query finds. */
function keptLineHoldingTheDraft(): boolean {
  return screen.queryAllByRole("status").some((line) => {
    const text = line.textContent ?? "";
    return /\bkept\b/.test(text) && text.includes(MARK_ANCHOR.quote) && text.includes(BODY);
  });
}

/** Where the reader finds the draft a line keeps for `MARK_ANCHOR`'s words (`MarginKeptDrafts`):
 *  the place a cell expects a draft no send holds once no compose holds it. */
const IN_A_KEPT_LINE = { found: "in a line saying it is kept for its words" } as const;

/**
 * Where the reader finds, on CORE-1's spec, the draft written for `MARK_ANCHOR`'s words once no
 * compose the reader opened holds it: the compose holding its text, with that compose's words
 * and note; else a line saying it is kept for those words that shows its text (`IN_A_KEPT_LINE`);
 * else nowhere. Waits a second for the compose the margin shows on arrival; none on screen reads
 * as absent.
 */
async function foundBack(): Promise<Record<string, string>> {
  await screen.findByRole("textbox", { name: "Comment" }, { timeout: 1_000 }).catch(() => null);
  const back = composesOnScreen().find((compose) => compose.text === BODY);
  if (back !== undefined) return { found: "in a compose", ...back };
  return keptLineHoldingTheDraft() ? IN_A_KEPT_LINE : { found: "nowhere on screen" };
}

/** Where `foundBack` should find the draft when the send that holds it, if any, shows `note`: a
 *  refused or stopped send in its compose at its words, with its note, and a draft no send holds
 *  in a line saying it is kept for those words. */
const foundAs = (note: Held | "none") =>
  note === "none"
    ? IN_A_KEPT_LINE
    : { found: "in a compose", note, quote: MARK_ANCHOR.quote, text: BODY };

// A compose opened on another document is the host's move for the first one, which carries
// nothing. The margin holds one open compose per document (`composeForMark` replaces only the
// compose on its own document), so the first stays open for CORE-1's spec and, back there, shows
// again at its words with its text and its note; and that text never fills a compose at another
// quote, so the next compose the reader opens on CORE-1's spec, at other words, starts without it.
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
      const back = composeShown();
      await page.user.click(screen.getByRole("button", { name: "Compose on another selection" }));
      await screen.findByText(ANOTHER_ANCHOR.quote);
      const elsewhere = composesOnScreen()
        .filter((compose) => compose.text === BODY && compose.quote !== MARK_ANCHOR.quote)
        .map((compose) => compose.quote)
        .join(", ");
      expect({ back, elsewhere }).toEqual({ back: backAtItsWords(held), elsewhere: "" });
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

/** Whether a line on screen offers `BODY`, the draft written for `MARK_ANCHOR`'s words, beside a
 *  newer compose, whatever kind it names. A status inside something hidden is no role the query
 *  finds. */
function offeredOnScreen(): boolean {
  return screen.queryAllByRole("status").some((line) => {
    const text = line.textContent ?? "";
    return text.includes(`for other words: “${MARK_ANCHOR.quote}”`) && text.includes(BODY);
  });
}

/** The margin's compose on `MARK_ANCHOR` with `BODY` in it, in the state `held` names, and then a
 *  newer compose on the same document at `ANOTHER_ANCHOR`, opened by the button named `press`,
 *  which offers that draft beside its own empty field. Answers the newer compose's form. */
async function offerInNewerCompose(
  page: Page,
  held: Held,
  press = "Compose on another selection"
): Promise<HTMLElement> {
  await holdInCompose(page, held);
  await page.user.click(screen.getByRole("button", { name: press }));
  const form = (await screen.findByText(ANOTHER_ANCHOR.quote)).closest("form");
  if (form === null) throw new Error("expected the newer compose's form");
  await waitFor(() => expect(offeredOnScreen()).toBe(true));
  return form;
}

// A newer compose on the document offers the earlier compose's draft beside its own empty field:
// typed, handed back by a refusal, or the send the reader stopped, set aside. Close and Escape end
// the newer compose with that draft in front of the reader, so they ask as they do over typed
// text, and only Discard drops it, its offer with it: a compose opened after offers nothing.
for (const end of ["Close", "Escape"] as const) {
  for (const held of HELD) {
    test(`${HOST} · a compose offering an earlier draft, then ${end} (the margin's compose) · ${held}`, async () => {
      const page = await openMargin(false, stoppedCompose(held));
      try {
        const form = await offerInNewerCompose(page, held);
        if (end === "Close") {
          await page.user.click(within(form).getByRole("button", { name: "Close" }));
        } else {
          await page.user.click(within(form).getByRole("textbox", { name: "Comment" }));
          await page.user.keyboard("{Escape}");
        }
        const asked = {
          offered: offeredOnScreen(),
          prompt: within(form).queryByText("Discard draft?") !== null,
        };
        // A role's name given as a string is matched whole, so this is not `Discard my draft`. A
        // compose that closed at once has no prompt to answer.
        const discard = within(form).queryByRole("button", { name: "Discard" });
        if (discard !== null) await page.user.click(discard);
        const closed = screen.queryByText(ANOTHER_ANCHOR.quote) === null;
        await page.user.click(screen.getByRole("button", { name: "Compose on another selection" }));
        await screen.findByText(ANOTHER_ANCHOR.quote);
        expect({ after: { closed, offered: offeredOnScreen() }, asked }).toEqual({
          after: { closed: true, offered: false },
          asked: { offered: true, prompt: true },
        });
      } finally {
        page.restore();
      }
    });
  }
}

// A closed issue refuses a suggestion and an ask, so a compose of either kind ends when its issue
// closes (`useMarginSheet`). One that offers an earlier draft keeps the offer in front of the
// reader: the compose stays, the draft beside it, until the reader drops it.
for (const held of HELD) {
  test(`${HOST} · a suggestion compose offering an earlier draft, then its issue closed (the margin's compose) · ${held}`, async () => {
    const page = await openMargin(false, stoppedCompose(held));
    try {
      await offerInNewerCompose(page, held, "Suggest on another selection");
      act(() => {
        page.queryClient.setQueryData(["issue", issue.key], {
          ...issue,
          closed_at: "2026-09-21T00:00:00Z",
        });
      });
      // A closed issue's compose draws no kind switch, whether it stays or ends.
      await waitFor(() => expect(screen.queryByRole("group", { name: "Kind" })).toBe(null));
      expect({ offered: offeredOnScreen() }).toEqual({ offered: true });
    } finally {
      page.restore();
    }
  });
}

// A sign-in after the session lapsed draws the shell again (`AuthGate`), and with it the margin's
// provider, over the same query client and stores: no compose is open any more. The send the
// reader stopped, set aside for a newer compose, goes back under its own compose's name, so the
// margin shows it on its document, at its words, with its note, as it did before the newer
// compose took its place. A typed draft, and a refused one, whose refusal went as the newer
// compose opened, are kept for their words with no send to hold them: the margin draws such a
// draft on its document as it arrives there, in a line saying it is kept for its words, with its
// text.
for (const held of HELD) {
  test(`${HOST} · a compose offering an earlier draft, then signed in again after a lapse (the margin's compose) · ${held}`, async () => {
    const page = await openMargin(false, stoppedCompose(held));
    try {
      await offerInNewerCompose(page, held);
      page.redraw();
      await screen.findByTestId(`margin-comment-${threadA.id}`);
      expect(await foundBack()).toEqual(foundAs(held === "stopped" ? "stopped" : "none"));
    } finally {
      page.restore();
    }
  });
}

// A suggestion or an ask compose that offers nothing ends when its issue closes, since a closed
// issue refuses both (`useMarginSheet`), and what the reader typed in it is kept for its words.
// No compose holds it then, so the margin draws it on its document, in a line saying it is kept
// for its words, with its text.
for (const { button, field, name } of [
  { button: "Suggest on the selection", field: "Reason", name: "a suggestion" },
  { button: "Ask on the selection", field: "Question", name: "an ask" },
] as const) {
  test(`${HOST} · ${name} compose with a typed draft, then its issue closed (the margin's compose) · typed`, async () => {
    const page = await openMargin(false);
    try {
      await page.user.click(screen.getByRole("button", { name: button }));
      const box = await screen.findByRole("textbox", { name: field });
      await page.user.click(box);
      await page.user.paste(BODY);
      act(() => {
        page.queryClient.setQueryData(["issue", issue.key], {
          ...issue,
          closed_at: "2026-09-21T00:00:00Z",
        });
      });
      await waitFor(() => expect(screen.queryByRole("textbox", { name: field })).toBe(null));
      expect(await foundBack()).toEqual(foundAs("none"));
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
  return {
    ...composeShown(),
    panel: panel === null ? "closed, so the reader has to open it" : "open",
  };
}

/** The compose a phone row expects back: the panel open on it, at its own words, with the text
 *  and the note it had. */
const backOnPhone = (held: Held) => ({ ...backAtItsWords(held), panel: "open" });

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

// A reload ends the whole page and loads it again over the same tab (`endAsAReload`), with the
// reader signed in as before. Thread A, opened again, holds the reply the reader wrote in it,
// with its note; the margin opens a thread whose reply is held as it arrives.
for (const held of HELD) {
  test(`${HOST} · the page reloaded, then back · ${held}`, async () => {
    const page = await openMargin(false, stoppedReply(held), "spec", true);
    try {
      await holdInCardA(page, held);
      await page.reload();
      await waitFor(() => expect(marginCard(threadA.id)).toBeInTheDocument());
      if (marginCard(threadA.id).getAttribute("aria-expanded") !== "true") {
        await openMarginThread(page.user, threadA.id);
      }
      expect(heldIn(marginCard(threadA.id))).toEqual(expectedBack(held));
    } finally {
      page.restore();
    }
  });
}

// The same reload over the margin's compose. The page it loads has no compose open, so the margin
// draws what the compose held on its document as it arrives there: a refused or stopped send in
// its compose at its words with its note, and a typed draft in a line saying it is kept for its
// words, with its text.
for (const held of HELD) {
  test(`${HOST} · the page reloaded, then back (the margin's compose) · ${held}`, async () => {
    const page = await openMargin(false, stoppedCompose(held), "spec", true);
    try {
      await holdInCompose(page, held);
      await page.reload();
      await screen.findByTestId(`margin-comment-${threadA.id}`);
      expect(await foundBack()).toEqual(foundAs(expectedBack(held).note));
    } finally {
      page.restore();
    }
  });
}
