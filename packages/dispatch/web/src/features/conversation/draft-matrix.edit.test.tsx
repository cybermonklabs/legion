/**
 * The draft matrix, comment edit row (INFRA-1503).
 *
 * Every composer host is held to one rule (packages/dispatch/AGENTS.md, "Who moved the
 * composer's address"): a move of the host's carries nothing, so the text stays with the address
 * it was written for and is there when the reader comes back to it (AGENTS.md:116-119). An edit's
 * address is the comment it edits, and what the reader wrote there was written over one body of
 * that comment. A kept edit comes back in the field only while the comment still holds that body.
 * If another writer changed the comment meanwhile, the field shows the newer body, the kept edit
 * is offered beside it with words saying the comment changed since the edit was written, and
 * nothing is saved over the newer body until the reader presses. Each cell writes an edit in one
 * state, typed and never saved or refused by Dispatch, takes it off the screen with the host's
 * move, leaves the comment's body as it was or has another writer change it, comes back, and
 * asserts what the editor holds and that no save went out meanwhile. A cell that fails at the code
 * it was written against is a hole in the rule, kept red on purpose until the code is fixed.
 *
 * Hosts: a comment's thread card draws the editor (`ThreadCard`'s Edit, `MentionComposer`'s
 * `edit`) in the Conversation's list, in the Conversation's phone thread view, in the margin's
 * list, and in the margin's phone thread view.
 *
 * | Host · move (off)                                       | typed  | typed   | refused | refused |
 * |                                                         | same   | changed | same    | changed |
 * | ------------------------------------------------------- | ------ | ------- | ------- | ------- |
 * | Conversation card · Collapse thread, then Expand thread | cell   | cell    | cell    | cell    |
 * | Conversation phone thread · Back, then back             | cell   | cell    | cell    | cell    |
 * | margin card · another thread opened, then back          | cell   | cell    | cell    | cell    |
 * | margin phone thread · Back, then back                   | cell   | cell    | cell    | cell    |
 *
 * "same" is a comment whose body nobody changed while the edit was away, "changed" one another
 * writer changed. (off) marks a host move that takes the editor off the screen: Collapse thread
 * and another thread's opening close the card the editor is in, and Back takes the thread view
 * off the page. Every move of this row is one, so each cell is checked on return alone.
 *
 * The second half keeps the editor on screen while another writer changes the comment under it.
 * Changed to newer text, the field shows the comment as it reads now and the edit waits beside it
 * with the same words as on return, since that is what the away cells show for the same change.
 * Changed to the very text the reader wrote, there is nothing to offer: the field shows that text
 * and no line says the comment changed. Each cell then takes the editor off the screen with the
 * host's move and back, and asserts the same both times, with no save in between.
 *
 * | Host · comment changed on screen, then move (off)       | typed  | typed   | refused | refused |
 * |                                                         | newer  | own     | newer   | own     |
 * | ------------------------------------------------------- | ------ | ------- | ------- | ------- |
 * | Conversation card · Collapse thread, then Expand thread | cell   | cell    | cell    | cell    |
 * | Conversation phone thread · Back, then back             | cell   | cell    | cell    | cell    |
 * | margin card · another thread opened, then back          | cell   | cell    | cell    | cell    |
 * | margin phone thread · Back, then back                   | cell   | cell    | cell    | cell    |
 *
 * "newer" is newer text another writer gave the comment, "own" the text the reader wrote.
 *
 * The hosts that cannot make the move: the docked composer, the phone thread composer, an Agents
 * row and an Inbox thread box are never given an `edit`, so they draw no editor to move.
 */
import { expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ReactNode, useEffect } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { graphNode } from "../../__tests__/graph-fixture";
import { ApiError, api } from "../../api/client";
import type { Comment } from "../../api/types";
import { Margin } from "../margin/Margin";
import { MarginProvider, useMargin } from "../margin/margin-context";
import {
  comment,
  issue,
  specArtifact,
  stubDocumentBridge,
  stubMatchMedia,
} from "../margin/margin-fixture";
import { buildIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { ConversationTab } from "./ConversationTab";

/** Dispatch refusing the save outright, as a 400 does: no attempt follows under its key. */
const REFUSAL = new ApiError(400, { code: "INPUT_REFUSED", error: "Dispatch refused the message" });

/** What the editor says of a save Dispatch refused. */
const REFUSED_NOTE = "Couldn't send — Dispatch refused the message";

/** What the reader writes in the editor, and the body another writer gives the comment. */
const BODY = "Edit the reader wrote";
const NEWER = "The comment as another writer changed it";

type Held = "typed" | "refused";
const HELD: readonly Held[] = ["typed", "refused"];

type Base = "unchanged" | "changed";
const BASES: readonly Base[] = ["unchanged", "changed"];

/** The reader, who wrote the comment the cells edit: the card draws Edit for their own comments
 *  alone. */
const READER = { kind: "user", login: "alice" } as const;

interface Page {
  /** Has another writer change the edited comment to `body`, as a refetch would bring it. */
  readonly changeBody: (body: string) => void;
  /** Every save of an edit the page made: the request log a cell reads. */
  readonly editComment: Mock<typeof api.editComment>;
  readonly restore: () => void;
  readonly user: UserEvent;
}

/** The query client of a page under test, signed in as the reader, whose reads never go stale. */
function readerClient(): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["whoami"], READER);
  return queryClient;
}

/** The writes and reads every page's editor makes: each save is refused, and the comment's
 *  blocks answer it not indexed, so a save carries no block ids. */
function stubEditor(): { editComment: Mock<typeof api.editComment>; restore: () => void } {
  const editComment = spyOn(api, "editComment").mockRejectedValue(REFUSAL);
  const commentBlocks = spyOn(api, "commentBlocks").mockImplementation(async (id) => ({
    blocks: [],
    indexed: false,
    item: graphNode({ id, issue_key: "CORE-1", kind: "comment" }),
  }));
  return {
    editComment,
    restore: () => {
      editComment.mockRestore();
      commentBlocks.mockRestore();
    },
  };
}

const conversationComment: Comment = {
  ...commentDeliveryFields(),
  anchor: null,
  ask_id: null,
  author: { id: READER.login, kind: "user" },
  body: "Comment A",
  created_at: "2026-09-20T00:00:00Z",
  edited_at: null,
  id: "comment-a",
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
};

/** The Conversation of CORE-1 over the reader's Comment A, at a phone width or above it. */
async function openConversation(phone: boolean): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = readerClient();
  const listComments = spyOn(api, "listComments").mockResolvedValue([conversationComment]);
  const reads = [
    listComments,
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "listHandoffs").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
  ];
  const editor = stubEditor();
  const restoreViewport = stubMatchMedia(phone);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <KeymapProvider>
        <QueryClientProvider client={queryClient}>
          <ConversationTab
            issueArtifacts={new Map()}
            isClosed={false}
            issueKey="CORE-1"
            state={{ "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 } }}
            visible
          />
        </QueryClientProvider>
      </KeymapProvider>
    </MemoryRouter>
  );
  await waitFor(() =>
    expect(document.querySelector('[data-turn="comment:comment-a"]')).not.toBe(null)
  );
  return {
    changeBody: (body) => {
      const changed = [{ ...conversationComment, body, edited_at: "2026-09-21T00:00:00Z" }];
      listComments.mockResolvedValue(changed);
      act(() => {
        queryClient.setQueryData(["comments", "CORE-1"], changed);
      });
    },
    editComment: editor.editComment,
    restore: () => {
      view.unmount();
      restoreViewport();
      editor.restore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

/** Comment A's turn in the Conversation's list, outside any thread view. */
function turn(): HTMLElement {
  const found = [...document.querySelectorAll<HTMLElement>('[data-turn="comment:comment-a"]')].find(
    (element) => element.closest('[role="dialog"]') === null
  );
  if (found === undefined) throw new Error("expected comment-a's turn");
  return found;
}

/** The margin's thread on the reader's comment, and a second thread on another mark. */
const threadA: Comment = comment;
const threadB: Comment = {
  ...comment,
  anchor: comment.anchor === null ? null : { ...comment.anchor, mark_id: "m-2", quote: "fox" },
  body: "and how?",
  id: "comment-2",
};

/** The document the margin's threads are on, registered as the document page registers it. */
function RegisterDocument(): ReactNode {
  const { registerDocument } = useMargin();
  useEffect(() => {
    registerDocument(stubDocumentBridge(specArtifact.id));
    return () => registerDocument(undefined);
  }, [registerDocument]);
  return null;
}

/** The margin of CORE-1's spec over threads A and B, at a phone width or above it. */
async function openMargin(phone: boolean): Promise<Page> {
  const user = userEvent.setup();
  const queryClient = readerClient();
  let comments: readonly Comment[] = [threadA, threadB];
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], comments);
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "listComments").mockImplementation(async () => [...comments]),
  ];
  const editor = stubEditor();
  const restoreViewport = stubMatchMedia(phone);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <RegisterDocument />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  await waitFor(() => expect(card(threadA.id)).toBeInTheDocument());
  return {
    changeBody: (body) => {
      comments = [{ ...threadA, body, edited_at: "2026-09-21T00:00:00Z" }, threadB];
      act(() => {
        queryClient.setQueryData(["comments", issue.key], comments);
      });
    },
    editComment: editor.editComment,
    restore: () => {
      view.unmount();
      restoreViewport();
      editor.restore();
      for (const read of reads) read.mockRestore();
    },
    user,
  };
}

/** Thread `id`'s card in the margin's list, looked up afresh, since opening a thread draws its
 *  card again. */
const card = (id: string) => screen.getByTestId(`margin-comment-${id}`);

/** Opens margin thread `id` from its collapsed card, as a click on it does. */
async function openThread(user: UserEvent, id: string): Promise<void> {
  await user.click(within(card(id)).getByRole("button", { expanded: false }));
}

/** Where the thread view a phone opens shows, once it does. */
const threadView = () => screen.findByRole("dialog", { name: "Thread" });

async function leaveThreadView(user: UserEvent): Promise<void> {
  await user.click(within(await threadView()).getByRole("button", { name: "Back" }));
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "Thread" })).toBe(null));
}

/** A host of the editor: how its page opens, where the edited comment's thread shows, and the
 *  host's own move that takes the editor off the screen. */
interface Host {
  readonly away: (page: Page) => Promise<void>;
  /** Where the edited comment's thread shows while it is on screen, looked up afresh. */
  readonly here: () => HTMLElement;
  readonly move: string;
  readonly name: string;
  readonly open: () => Promise<Page>;
  /** Opens the edited comment's thread and answers where it shows. */
  readonly show: (page: Page) => Promise<HTMLElement>;
}

const HOSTS: readonly Host[] = [
  {
    away: async ({ user }) => {
      await user.click(within(turn()).getByRole("button", { name: "Collapse thread" }));
    },
    here: turn,
    move: "Collapse thread, then Expand thread",
    name: "Conversation card",
    open: () => openConversation(false),
    show: async ({ user }) => {
      await user.click(within(turn()).getByRole("button", { name: "Expand thread" }));
      return turn();
    },
  },
  {
    away: ({ user }) => leaveThreadView(user),
    here: () => screen.getByRole("dialog", { name: "Thread" }),
    move: "Back, then back",
    name: "Conversation phone thread",
    open: () => openConversation(true),
    show: async ({ user }) => {
      await user.click(within(turn()).getByRole("button", { name: "Expand thread" }));
      return threadView();
    },
  },
  {
    away: ({ user }) => openThread(user, threadB.id),
    here: () => card(threadA.id),
    move: "another thread opened, then back",
    name: "margin card",
    open: () => openMargin(false),
    show: async ({ user }) => {
      await openThread(user, threadA.id);
      return card(threadA.id);
    },
  },
  {
    away: ({ user }) => leaveThreadView(user),
    here: () => screen.getByRole("dialog", { name: "Thread" }),
    move: "Back, then back",
    name: "margin phone thread",
    open: () => openMargin(true),
    show: async ({ user }) => {
      // The thread view a tap on the card opens, or the card itself where the list shows it open
      // already once the reader is back from the thread view.
      if (card(threadA.id).getAttribute("aria-expanded") === "true") return card(threadA.id);
      await openThread(user, threadA.id);
      return threadView();
    },
  },
];

const editField = (place: HTMLElement) =>
  within(place).queryByRole<HTMLTextAreaElement>("textbox", { name: "Edit comment" });

/** Opens the editor on the comment `place` shows and writes `BODY` over its body, in the state
 *  `held` names. */
async function writeEdit(page: Page, place: HTMLElement, held: Held): Promise<void> {
  await page.user.click(within(place).getByRole("button", { name: "Edit" }));
  const field = editField(place);
  if (field === null) throw new Error("expected the editor");
  await page.user.click(field);
  await page.user.keyboard("{Control>}a{/Control}");
  await page.user.paste(BODY);
  if (held === "refused") {
    const form = field.closest("form");
    if (form === null) throw new Error("expected the editor's form");
    await page.user.click(within(form).getByRole("button", { name: "Save" }));
    await within(place).findByText(REFUSED_NOTE);
  }
}

/** Whether a line in `place` says the comment changed since the edit was written, and offers the
 *  edit itself beside the newer body. A status inside something hidden is no role the query
 *  finds. */
function offered(place: HTMLElement): boolean {
  return within(place)
    .queryAllByRole("status")
    .some((line) => {
      const text = line.textContent ?? "";
      return /\bchanged since\b/.test(text) && text.includes(BODY);
    });
}

/** What the editor in `place` shows - the field's text, and whether the edit is offered beside
 *  it - and how many saves of an edit the page has made so far. */
function inEditor(
  page: Page,
  place: HTMLElement
): { offered: boolean; saves: number; text: string } {
  return {
    offered: offered(place),
    saves: page.editComment.mock.calls.length,
    text: editField(place)?.value ?? "(no editor on screen)",
  };
}

for (const host of HOSTS) {
  for (const held of HELD) {
    for (const base of BASES) {
      test(`${host.name} edit · ${host.move} · ${held} · base ${base}`, async () => {
        const page = await host.open();
        try {
          await writeEdit(page, await host.show(page), held);
          // Counted here, so a refused cell's own Save is not a save made while away.
          const saves = page.editComment.mock.calls.length;
          await host.away(page);
          if (base === "changed") page.changeBody(NEWER);
          const place = await host.show(page);
          const shown = inEditor(page, place);
          if (base === "unchanged") {
            const note = within(place).queryByText(REFUSED_NOTE) === null ? "none" : "refused";
            expect({ ...shown, note }).toEqual({
              note: held === "typed" ? "none" : "refused",
              offered: false,
              saves,
              text: BODY,
            });
          } else {
            expect(shown).toEqual({ offered: true, saves, text: NEWER });
          }
        } finally {
          page.restore();
        }
      });
    }
  }
}

/** What another writer changes the comment to while the editor is on screen: newer text, or the
 *  very text the reader wrote, which leaves nothing to offer. */
type Change = "newer text" | "the edit's own text";
const CHANGES: readonly Change[] = ["newer text", "the edit's own text"];

for (const host of HOSTS) {
  for (const held of HELD) {
    for (const change of CHANGES) {
      test(`${host.name} edit · comment changed on screen to ${change}, then ${host.move} · ${held}`, async () => {
        const page = await host.open();
        try {
          await writeEdit(page, await host.show(page), held);
          // Counted here, so a refused cell's own Save is not a save made since.
          const saves = page.editComment.mock.calls.length;
          const body = change === "newer text" ? NEWER : BODY;
          page.changeBody(body);
          const onScreen = inEditor(page, host.here());
          await host.away(page);
          const back = inEditor(page, await host.show(page));
          const expected = { offered: change === "newer text", saves, text: body };
          expect({ back, onScreen }).toEqual({ back: expected, onScreen: expected });
        } finally {
          page.restore();
        }
      });
    }
  }
}
