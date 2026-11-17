/**
 * The draft matrix, margin thread row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): the reader's own move with the composer's own control takes the draft
 * with it, and a move of the host's carries nothing, so the text stays with the address it was
 * written for and is there when the reader comes back to it (AGENTS.md:116-119). A refused or
 * stopped send's note stays at its own address. A margin thread's reply has no other composer for
 * its own controls to carry the draft to, so its Cancel reply and Escape end the reply and keep
 * the draft for the thread, which the closed card says (`keptReply`); an anchored composer's
 * Close asks `Discard draft?` first, and only Discard drops what the reader wrote. Each cell
 * writes a draft in one state, typed and never sent, refused by Dispatch, or stopped by the
 * reader, makes one move, and asserts where the text ends. A cell that fails at the code it was
 * written against is a hole in the rule, kept red on purpose until the code is fixed.
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
 * | Host moves it over a refusal (elsewhere)     | no (1) | no (1)  | no (1)  |
 * | Thread collapsed by the host                 | no (2) | no (2)  | no (2)  |
 * | Cancel reply                                 | cell   | cell    | cell    |
 * | Escape in the field                          | cell   | cell    | cell    |
 * | Close, then Discard draft? (the compose)     | cell   | cell    | cell    |
 * | Agents pick                                  | no (3) | no (3)  | no (3)  |
 * | Typed while elsewhere, then unmounted        | -      | no (1)  | -       |
 *
 * (off) marks a host move that takes the composer off the screen: Back hides the thread view,
 * opening another thread collapses this one and unmounts its reply, and leaving the document
 * takes the margin to another document's composes. Every host move of this row is one, so none
 * needs a line while away.
 *
 * The moves this host cannot make:
 * 1. A thread's reply always answers its own thread's root, so no move of the host's changes its
 *    address, and it is never elsewhere.
 * 2. The margin's card draws no collapse of its own: the desktop list closes it only by opening
 *    another thread (the second row), and the phone only by Back (the first row).
 * 3. The margin draws no issue picker (asserted absent below).
 */
import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { type MutationKey, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useEffect } from "react";
import { MemoryRouter, useNavigate } from "react-router-dom";

import { realClock } from "../../__tests__/fake-clock";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { ApiError, api } from "../../api/client";
import type { Comment, IssueDetails } from "../../api/types";
import { heldSends } from "../conversation/held-sends";
import type { SentRequest } from "../conversation/send-request";
import { buildIssuePath } from "../refs/routes";
import { SendStoppedError } from "../writes/keyed-write";
import { Margin } from "./Margin";
import {
  MarginProvider,
  marginComposeSendKey,
  marginReplySendKey,
  useMargin,
} from "./margin-context";
import { comment, issue, specArtifact, stubDocumentBridge, stubMatchMedia } from "./margin-fixture";

const HOST = "margin thread";

/** Dispatch refusing the send outright, as a 400 does: no attempt follows under its key. */
const REFUSAL = new ApiError(400, { code: "INPUT_REFUSED", error: "Dispatch refused the message" });

/** What each note says in a composer: the refusal, and a stopped send at its own address. */
const REFUSED_NOTE = "Couldn't send — Dispatch refused the message";
const STOPPED_NOTE = new SendStoppedError().message;

const BODY = "Draft the reader wrote";

type Held = "typed" | "refused" | "stopped";
const HELD: readonly Held[] = ["typed", "refused", "stopped"];

/** The fixture's thread, and a second one on another mark of the same document. */
const threadA: Comment = comment;
const threadB: Comment = {
  ...comment,
  anchor: comment.anchor === null ? null : { ...comment.anchor, mark_id: "m-2", quote: "fox" },
  body: "and how?",
  id: "comment-2",
};

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

/** A send of `BODY` from the margin: thread A's reply, or the compose on `MARK_ANCHOR`. */
function request(kind: "reply" | "compose"): SentRequest {
  return {
    anchor: kind === "compose" ? MARK_ANCHOR : undefined,
    ask: { multiple: false, options: [], urgency: "med" },
    draft: { body: BODY, mentions: [], replacement: "" },
    edit: undefined,
    kind: "comment",
    owner: { issueKey: issue.key, kind: "issue" },
    plainDelivery: "steer",
    replyTo:
      kind === "reply"
        ? { actor: threadA.author, excerpt: "", id: threadA.id, parentKind: "comment" }
        : null,
    sendKey: "stopped-send",
  };
}

/** The document the margin's compose is on, registered as the document page registers it. */
function RegisterDocument(): ReactNode {
  const { registerDocument } = useMargin();
  useEffect(() => {
    registerDocument(stubDocumentBridge(specArtifact.id));
    return () => registerDocument(undefined);
  }, [registerDocument]);
  return null;
}

/** The selection bar's Comment on `MARK_ANCHOR`, on a press. */
function ComposeOnMark(): ReactNode {
  const { composeForMark } = useMargin();
  return (
    <button
      onClick={() => {
        void composeForMark({ anchor: MARK_ANCHOR, kind: "comment" }).catch(() => {});
      }}
      type="button"
    >
      Compose on the selection
    </button>
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
 * late, as the reader's Stop trying does, so the margin arrives showing it.
 */
async function openMargin(
  phone: boolean,
  stopped?: { readonly key: MutationKey; readonly request: SentRequest }
): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [threadA, threadB]);
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
    createComment.mockRejectedValue(
      new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
    );
    jest.useFakeTimers();
    try {
      act(() => {
        heldSends(queryClient).send(stopped.key, stopped.request);
      });
      await stopTheLateWrite(queryClient);
    } finally {
      realClock();
    }
  }
  createComment.mockRejectedValue(REFUSAL);
  const restoreViewport = stubMatchMedia(phone);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <RegisterDocument />
          <ComposeOnMark />
          <Travel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
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

/** Thread `id`'s card in the list. Expanding a thread draws its card again, so it is looked up
 *  afresh each time. */
const card = (id: string) => screen.getByTestId(`margin-comment-${id}`);

/** Opens thread `id` from its collapsed card, as a click on it does. */
async function open(user: UserEvent, id: string): Promise<void> {
  await user.click(within(card(id)).getByRole("button", { expanded: false }));
}

const replyField = (place: HTMLElement) =>
  within(place).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });

/** Which note `place` shows about a send it holds. */
function note(place: HTMLElement): Held | "none" {
  const shown = within(place);
  if (shown.queryByText(REFUSED_NOTE) !== null) return "refused";
  return shown.queryByText(STOPPED_NOTE) === null ? "none" : "stopped";
}

/** Whether the reader can see that `draft` is kept: a line on screen says so, or the draft is
 *  still in a field on screen, as it is where the list shows the thread open. A status or a field
 *  inside something hidden is no role the query finds, so one under a covered or folded part of
 *  the page does not count. */
function keptInSight(draft: string): boolean {
  return (
    screen.queryAllByRole("status").some((line) => /\bkept\b/.test(line.textContent ?? "")) ||
    screen.queryAllByRole<HTMLTextAreaElement>("textbox").some((box) => box.value === draft)
  );
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
  await waitFor(() => expect(card(threadA.id)).toBeInTheDocument());
  if (card(threadA.id).getAttribute("aria-expanded") !== "true") await open(page.user, threadA.id);
  if (held === "stopped") {
    expect(note(card(threadA.id))).toBe("stopped");
    return;
  }
  await writeReply(page, card(threadA.id), held);
}

/** Thread A in the phone's thread view with `BODY` in its reply. Answers the thread view. */
async function holdInPhoneThreadA(page: Page, held: Held): Promise<HTMLElement> {
  if (held === "stopped") {
    // Coming to a document whose thread holds a reply opens that thread.
    const dialog = await screen.findByRole("dialog", { name: "Thread" });
    expect(note(dialog)).toBe("stopped");
    return dialog;
  }
  await open(page.user, threadA.id);
  const dialog = await screen.findByRole("dialog", { name: "Thread" });
  await writeReply(page, dialog, held);
  return dialog;
}

/** Thread A's reply as it shows again in `place`. */
function heldIn(place: HTMLElement): { note: Held | "none"; text: string } {
  return { note: note(place), text: replyField(place).value };
}

const expectedBack = (held: Held): { note: Held | "none"; text: string } => ({
  note: held === "typed" ? "none" : held,
  text: BODY,
});

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
      let place = card(threadA.id);
      if (place.getAttribute("aria-expanded") !== "true") {
        await open(page.user, threadA.id);
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
      await open(page.user, threadB.id);
      const aOpen = card(threadA.id).getAttribute("aria-expanded");
      await open(page.user, threadA.id);
      expect({ aOpen, back: heldIn(card(threadA.id)) }).toEqual({
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
      await page.user.click(within(card(threadA.id)).getByRole("button", { name: "Cancel reply" }));
      const kept = keptInSight(BODY);
      await open(page.user, threadA.id);
      expect({ back: heldIn(card(threadA.id)), kept }).toEqual({
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
      await page.user.click(replyField(card(threadA.id)));
      await page.user.keyboard("{Escape}");
      const kept = keptInSight(BODY);
      await open(page.user, threadA.id);
      expect({ back: heldIn(card(threadA.id)), kept }).toEqual({
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
  expect(note(form)).toBe(held === "typed" ? "none" : held);
  return form;
}

// Leaving the document is the host's move, and the compose is off the screen while the reader is
// on another issue: coming back to the document should find the draft with its note. An unsent
// compose is cancelled when the margin no longer shows its document (`useMarginSheet`'s effect
// on `pendingCompose`), and a sent one is the held-send store's, which the margin shows again on
// arrival.
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
        note: field === null ? "none" : note(field.closest("form") ?? field),
        text: field === null ? "(no compose on screen)" : field.value,
      }).toEqual(expectedBack(held));
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

test(`${HOST} · Agents pick · cannot make: the margin draws no issue picker`, async () => {
  const page = await openMargin(false);
  try {
    await holdInCardA(page, "typed");
    expect(screen.queryAllByRole("button", { name: "Choose issue" }).length).toBe(0);
  } finally {
    page.restore();
  }
});
