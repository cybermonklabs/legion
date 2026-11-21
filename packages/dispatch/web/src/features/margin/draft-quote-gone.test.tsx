import { expect, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { correction, DELETION } from "../../__tests__/correction-feed";
import { marginClient, RegisterDocument } from "../../__tests__/draft-matrix-fixture";
import { ApiError, api } from "../../api/client";
import { markCorrectedPost } from "../../api/corrected-post";
import type { ArtifactText, Comment } from "../../api/types";
import type { ComposerAnchor } from "../conversation/composer-model";
import { heldSends } from "../conversation/held-sends";
import { KeptDraftOffers } from "../conversation/KeptDraftOffers";
import { MentionComposer } from "../conversation/MentionComposer";
import {
  type AwayDraft,
  draftStoresReloaded,
  draftStoresSignedIn,
  unsentDraftsOf,
} from "../conversation/unsent-drafts";
import { composerNameOf, giveWay, type OfferedDraft } from "../conversation/useKeptDrafts";
import { liveTextQueryOptions } from "../doc/live-text-query";
import { buildIssuePath } from "../refs/routes";
import { MarginKeptDrafts } from "./CommentsTab";
import { Margin } from "./Margin";
import {
  MarginProvider,
  marginComposeKeptKey,
  marginComposeSendKey,
  useMargin,
} from "./margin-context";
import { issue, specArtifact } from "./margin-fixture";

// Census row 11.9 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-20 17:41Z: a
// reader selects words to comment on inside another comment's pending insert suggestion, so the
// draft's quote is the words that insert put in the document. The insert's author deletes the
// comment, and the delete's reject takes those words out of the document: they are a deleted
// post's words now. The page reads the document's text again after the delete
// (`eventRefreshTargets`), and each surface that draws a draft's quote - the open composer and the
// two lines that lead a draft kept for other words - checks the quote against that text, draws
// none of the words once it no longer holds them, and keeps the draft's own words and presses.
//
// No keystroke and no collaborator's update reads the text again, so the page's read can be older
// than words the reader has just typed and selected. Only a read newer than the time the draft's
// compose took the words says they left (`useQuoteInDocument`): for the open composer, the time
// the compose whose draft its field took for those words took them (`MountDraft.anchoredAt`),
// else when its compose opened (`openedAt`), or its mount where no compose outlives it; the time
// a kept draft keeps from its compose (`AwayDraft.anchoredAt`); and for a draft an older release
// kept with no time, before every read.
//
// The architect's ruling of 2026-11-20 20:55Z, approved at 21:57Z: before that time gate, a fact
// the page holds decides alone, whatever the age of its read of the text. The selection bar keeps
// the pending inserts the selected words lie in with the anchor (`ComposerAnchor.insertMarks`),
// and the delete's correction writes the insert's comment deleted into the page's comment lists
// (`markCorrectedPost`): a surface whose words lie in an insert those lists hold deleted draws
// none of them.

const DOCUMENT = "doc-1";
/** The words the pending insert put in the document, which the reader's selection lies in. */
const QUOTE = "security review";
const BEFORE = ["Ship the release.", "Hold it for the security review."];
/** The document once the delete's reject took the insert's words out. */
const AFTER = ["Ship the release.", "Hold it."];
const OWNER = { issueKey: "CORE-1", kind: "issue" } as const;
/** The issue's comment list, as the margin and the issue page read it (`useMarginItems`). */
const COMMENTS = ["comments", OWNER.issueKey];
/** The pending insert suggestion's mark, which the reader's selection lies in. */
const INSERT_MARK = "mark-insert-suggestion";
/** The insert suggestion's comment, as the page's comment list holds it while it stands. */
const INSERT_COMMENT: Comment = {
  anchor: {
    artifact_id: DOCUMENT,
    block_id: "block-hold",
    mark_id: INSERT_MARK,
    orphaned: false,
    quote: QUOTE,
    version: 1,
  },
  ask_id: null,
  author: { id: "alice", kind: "user" },
  body: "Name the review.",
  created_at: "2026-11-20T00:00:00Z",
  edited_at: null,
  id: "comment-insert",
  issue_key: OWNER.issueKey,
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
  ...commentDeliveryFields(),
};
/** The delete of the insert's comment as the page's feed carries it: its anchor with the quote
 *  emptied, since the insert's words were the comment's own. */
const insertDeleted = () =>
  correction("comment", INSERT_COMMENT.id, false, {
    anchor: { ...INSERT_COMMENT.anchor, quote: "" },
  });

/** The document's text read (`GET /api/v1/artifacts/{id}/text`) holding `paragraphs`, with the
 *  tree the page reads its words from. */
function textRead(paragraphs: readonly string[]): ArtifactText {
  return {
    markdown: `${paragraphs.join("\n\n")}\n`,
    markdown_tree: {
      content: paragraphs.map((text) => ({ content: [{ text, type: "text" }], type: "paragraph" })),
      type: "doc",
    },
    token: "token-1",
    version: 1,
  };
}

/** `read` landing in `client`'s cache as the page's read of the document's text, at `at`, or as
 *  the clock reads now where that is not given. */
function land(client: QueryClient, read: ArtifactText, at?: number): void {
  client.setQueryData(liveTextQueryOptions(DOCUMENT).queryKey, read, { updatedAt: at });
}

const noop = () => {};

/** The anchor of a compose at `quote`'s words of the document under the mark `markId`, naming the
 *  pending inserts `insertMarks` names, or, where that is undefined, with no field for them at
 *  all, as a draft an older build kept has none (`ComposerAnchor.insertMarks`). */
function wordsAnchor(
  markId: string,
  quote: string,
  insertMarks?: readonly string[]
): ComposerAnchor {
  return insertMarks === undefined
    ? { artifact: DOCUMENT, mark_id: markId, quote }
    : { artifact: DOCUMENT, insertMarks, mark_id: markId, quote };
}

/** A draft kept for `quote`'s words of the document under the mark `markId`, with `body`, by a
 *  compose that took those words at `anchoredAt`, or by an older release, which kept no time; the
 *  words lie in the pending inserts `insertMarks` names, where it names a list, and the anchor has
 *  no such field where it names no list (`wordsAnchor`). */
function keptDraft(
  markId: string,
  quote: string,
  body: string,
  anchoredAt?: number,
  insertMarks?: readonly string[]
): OfferedDraft {
  return {
    anchor: wordsAnchor(markId, quote, insertMarks),
    anchoredAt,
    draft: { body, mentions: [], replacement: "" },
    drop: noop,
    note: undefined,
    owner: OWNER,
    replyTo: null,
    take: noop,
  };
}

/** The open composer at `quote`'s words, the offer of a draft kept for them beside it, and the
 *  margin's own list of what its compose kept, over a cache holding the document's text `read`,
 *  or none where it is undefined, and the issue's comment list `comments`, where it is given.
 *  `offeredAt` and `awayAt` are when the composes that kept the two drafts took the words, and
 *  `readAt` is when `read` landed; the words lie in the pending inserts `insertMarks` names on
 *  every surface, where it names a list, and no anchor has such a field where it names no list. */
function renderSurfaces(
  quote: string,
  read: ArtifactText | undefined,
  {
    awayAt,
    comments,
    insertMarks,
    offeredAt,
    readAt,
  }: {
    awayAt?: number;
    comments?: readonly Comment[];
    insertMarks?: readonly string[];
    offeredAt?: number;
    readAt?: number;
  } = {}
) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (read !== undefined) land(client, read, readAt);
  if (comments !== undefined) client.setQueryData(COMMENTS, comments);
  const offered = keptDraft(
    "mark-offered",
    quote,
    "Is this the review we agreed on?",
    offeredAt,
    insertMarks
  );
  const away = keptDraft("mark-away", quote, "Name who runs it.", awayAt, insertMarks);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={client}>
        <MentionComposer
          anchor={wordsAnchor("mark-open", quote, insertMarks)}
          kind="comment"
          onClose={noop}
          onSent={noop}
          owner={OWNER}
          replyTo={null}
        />
        <div data-testid="offered">
          <KeptDraftOffers
            changed={undefined}
            gone={[]}
            goneTakes="nowhere"
            kind="comment"
            offered={offered}
            onDiscardChanged={noop}
            onDropGone={noop}
            onDropOffered={noop}
            onTakeChanged={noop}
            onTakeGone={noop}
            onTakeOffered={noop}
          />
        </div>
        <div data-testid="kept">
          <MarginKeptDrafts drafts={[away]} hidden={false} onDiscard={noop} />
        </div>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { client, view };
}

/** How many times `words` stand in the page. */
const timesDrawn = (container: HTMLElement, words: string) =>
  (container.textContent ?? "").split(words).length - 1;

test("a draft quoting a deleted post's inserted words draws none of them once the document's text no longer holds them, and keeps its own words", async () => {
  const user = userEvent.setup();
  // The page's read and the composer's mount at one moment, and the read after the delete a
  // second later, on a clock this row moves itself.
  const start = Date.now();
  setSystemTime(start);
  const { client, view } = renderSurfaces(QUOTE, textRead(BEFORE));
  try {
    await user.click(screen.getByLabelText("Comment"));
    await user.paste("Which review is this?");
    // While the insert stands, its words are the document's, and each surface quotes them.
    expect(timesDrawn(view.container, QUOTE)).toBe(3);

    // The read after the delete's reject lands.
    setSystemTime(start + 1_000);
    act(() => land(client, textRead(AFTER)));

    const composer = screen.getByRole("form", { name: "Comment composer" });
    expect({
      drawn: timesDrawn(view.container, QUOTE),
      composer:
        within(composer).queryByText(
          "The words this draft was for are no longer in the document."
        ) !== null,
      field: within(composer).getByLabelText<HTMLTextAreaElement>("Comment").value,
      offered: screen.getByTestId("offered").textContent,
      kept: screen.getByTestId("kept").textContent,
    }).toEqual({
      drawn: 0,
      composer: true,
      field: "Which review is this?",
      offered:
        "You wrote this draft for other words, which are no longer in the document. It is kept here:Is this the review we agreed on?Use my draftDiscard my draft",
      kept: "You wrote this draft for words that are no longer in the document. It is kept here, and offered in the next compose you open on this document:Name who runs it.Discard my draft",
    });
  } finally {
    setSystemTime();
    view.unmount();
    client.clear();
  }
});

// The control: words the document still holds are drawn, a selection across two blocks among
// them, which the editor names with a line break where the document's words hold a space; and so
// are they before the page reads the document, once it holds the margin's comment list, which
// lists no insert of theirs deleted, for a draft that names the inserts its words lie in: none,
// an empty list, as the selection bar names words in no insert (`insertMarksIn`). With neither
// read held, none is drawn (the architect's ruling of 2026-11-21 00:17Z), and a draft an older
// build kept, which names no list, waits for the text read (its ruling of 01:49Z); the tests at
// the end of this file.
test("a draft's words the document still holds are drawn, across a block break too, and so are words in no insert once the page holds its comments before it reads the document", () => {
  const across = "release.\nHold it";
  const held = renderSurfaces(across, textRead(AFTER));
  try {
    expect(timesDrawn(held.view.container, across)).toBe(3);
    expect(held.view.container.textContent).not.toContain("no longer in the document");
  } finally {
    held.view.unmount();
    held.client.clear();
  }
  const unread = renderSurfaces(QUOTE, undefined, { comments: [], insertMarks: [] });
  try {
    expect(timesDrawn(unread.view.container, QUOTE)).toBe(3);
  } finally {
    unread.view.unmount();
    unread.client.clear();
  }
});

// The reader, or a collaborator over Yjs, typed the words a minute after the page last read the
// document, and the reader selected them: that read lacks them, and nothing has read the text
// since. It is older than each compose that took the words - the open one, and the one that kept
// a draft for them a second ago - so no surface takes it to say the words left: they are on
// screen. The words lie in no insert, which this build's selection bar names by an empty list
// (`insertMarksIn`); words that name no list are a row near the end of this file.
test("words typed after the page last read the document are drawn though that read lacks them", () => {
  const now = Date.now();
  const { client, view } = renderSurfaces(QUOTE, textRead(AFTER), {
    awayAt: now - 1_000,
    insertMarks: [],
    offeredAt: now - 1_000,
    readAt: now - 60_000,
  });
  try {
    expect({
      drawn: timesDrawn(view.container, QUOTE),
      gone: view.container.textContent?.includes("no longer in the document"),
    }).toEqual({ drawn: 3, gone: false });
  } finally {
    view.unmount();
    client.clear();
  }
});

/** The margin's compose at `anchor`, as the margin draws one: under the document's compose send
 *  name, and keyed by its mark, so a compose at other words is a composer of its own. */
function composeAt(client: QueryClient, anchor: ComposerAnchor) {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={client}>
        <MentionComposer
          anchor={anchor}
          key={anchor.mark_id}
          kind="comment"
          mutationKey={marginComposeSendKey(DOCUMENT)}
          onClose={noop}
          onSent={noop}
          owner={OWNER}
          replyTo={null}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// The delete's reject takes the words out while the compose at them is open, and the read after
// it lands, newer than the compose's taking them: the composer says they are gone. The reader then
// selects other words, and the newer compose offers the draft the first one kept as it went. That
// draft keeps the time its compose took the words, which the read is newer than, not the time it
// was kept, which is after the read: the offer draws none of the deleted post's words either.
test("after the read that follows a delete's reject, the composer at the words and the draft it keeps as a newer compose opens draw none of them", async () => {
  const user = userEvent.setup();
  const start = Date.now();
  setSystemTime(start);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  land(client, textRead(BEFORE), start - 60_000);
  const view = render(
    composeAt(client, { artifact: DOCUMENT, mark_id: "mark-insert", quote: QUOTE })
  );
  try {
    await user.click(screen.getByLabelText("Comment"));
    await user.paste("Which review is this?");
    expect(timesDrawn(view.container, QUOTE)).toBe(1);

    setSystemTime(start + 1_000);
    act(() => land(client, textRead(AFTER)));
    const gone = timesDrawn(view.container, QUOTE);

    setSystemTime(start + 2_000);
    view.rerender(
      composeAt(client, { artifact: DOCUMENT, mark_id: "mark-release", quote: "Ship the release." })
    );
    expect({
      gone,
      drawn: timesDrawn(view.container, QUOTE),
      offered: screen.getByText("Which review is this?").closest('[role="status"]')?.textContent,
    }).toEqual({
      gone: 0,
      drawn: 0,
      offered:
        "You wrote this draft for other words, which are no longer in the document. It is kept here:Which review is this?",
    });
  } finally {
    setSystemTime();
    view.unmount();
    client.clear();
  }
});

// A reload after the delete: the page mounts with no read of the document, the composer at the
// words, as a send held over the reload draws one, and the drafts the tab kept for them - one with
// the time its compose took the words, before the reload, and one an older release kept with no
// time, which counts as taken before every read. The page's first read lands after all of them,
// and lacks the words: none is drawn.
test("after a reload that follows the delete, a draft kept for the words draws none of them, with its compose's time or with none", () => {
  const start = Date.now();
  setSystemTime(start);
  const { client, view } = renderSurfaces(QUOTE, undefined, { offeredAt: start - 60_000 });
  try {
    setSystemTime(start + 1_000);
    act(() => land(client, textRead(AFTER)));
    expect({
      drawn: timesDrawn(view.container, QUOTE),
      offered: screen.getByTestId("offered").textContent,
      kept: screen.getByTestId("kept").textContent,
    }).toEqual({
      drawn: 0,
      offered:
        "You wrote this draft for other words, which are no longer in the document. It is kept here:Is this the review we agreed on?Use my draftDiscard my draft",
      kept: "You wrote this draft for words that are no longer in the document. It is kept here, and offered in the next compose you open on this document:Name who runs it.Discard my draft",
    });
  } finally {
    setSystemTime();
    view.unmount();
    client.clear();
  }
});

/** Dispatch's refusal of a send whose mark left the document with the words it marked. */
const ANCHOR_MISSING = new ApiError(409, {
  code: "ANCHOR_MISSING",
  error: "anchor mark is not in the document",
});

// The reader presses Send after the composer already says its words are gone, and Dispatch
// refuses the send: its mark went with the words. A newer compose takes its place, and the refused
// send gives way (`giveWay`): its draft is kept from the send, with the time its composer took the
// words, which the send keeps (`ComposerSend.anchoredAt`), not the time Send was pressed, which is
// after the read that lacks them. The newer compose offers the draft, and draws none of the
// deleted post's words.
test("a send pressed after the composer says its words are gone, which Dispatch refuses, keeps a draft that draws none of them", async () => {
  const user = userEvent.setup();
  const start = Date.now();
  setSystemTime(start);
  const createComment = spyOn(api, "createComment").mockRejectedValue(ANCHOR_MISSING);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const store = heldSends(client);
  const sendKey = marginComposeSendKey(DOCUMENT);
  land(client, textRead(BEFORE), start - 60_000);
  let view = render(
    composeAt(client, { artifact: DOCUMENT, mark_id: "mark-insert", quote: QUOTE })
  );
  try {
    await user.click(screen.getByLabelText("Comment"));
    await user.paste("Which review is this?");
    setSystemTime(start + 1_000);
    act(() => land(client, textRead(AFTER)));
    const saidGone =
      screen.queryByText("The words this draft was for are no longer in the document.") !== null;

    setSystemTime(start + 2_000);
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(store.get(sendKey)?.status).toBe("refused"));
    view.unmount();
    const refused = store.get(sendKey);
    if (refused?.status !== "refused") throw new Error("Dispatch refused no send");
    act(() => giveWay(refused, marginComposeKeptKey(DOCUMENT), store, unsentDraftsOf(client)));

    view = render(
      composeAt(client, { artifact: DOCUMENT, mark_id: "mark-release", quote: "Ship the release." })
    );
    expect({
      saidGone,
      drawn: timesDrawn(view.container, QUOTE),
      offered: screen.getByText("Which review is this?").closest('[role="status"]')?.textContent,
    }).toEqual({
      saidGone: true,
      drawn: 0,
      offered:
        "You wrote this draft for other words, which are no longer in the document. It is kept here:Which review is this?",
    });
  } finally {
    setSystemTime();
    createComment.mockRestore();
    view.unmount();
    client.clear();
  }
});

// The architect's ruling of 2026-11-20 20:03Z on dispatch://INFRA-1523 (round 42's ruling 4): a
// composer that mounts again after the delete's reject may draw the deleted post's words only while
// the page's next read of the document is out, and never once that read has failed. The reader
// leaves the document and comes back: the composer at the words mounts again over the cache, which
// holds the read that followed the reject and lacks them, and the page's next read fails. In
// chromium at 8259e87d the composer drew the inserted words again, 8 s after the page came back
// with every read of the document's text answered 503 (test round at 8259e87d).
test("a composer that mounts again after the delete, while the page's next read of the document fails, draws none of the deleted post's words", async () => {
  const user = userEvent.setup();
  const start = Date.now();
  setSystemTime(start);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  land(client, textRead(BEFORE), start - 60_000);
  const anchor = { artifact: DOCUMENT, mark_id: "mark-insert", quote: QUOTE };
  const first = render(composeAt(client, anchor));
  try {
    await user.click(screen.getByLabelText("Comment"));
    await user.paste("Which review is this?");
    setSystemTime(start + 1_000);
    act(() => land(client, textRead(AFTER)));
    // The positive control: before it mounts again, the composer says the words are gone, and
    // draws none of them.
    expect({
      drawn: timesDrawn(first.container, QUOTE),
      gone:
        within(first.container).queryByText(
          "The words this draft was for are no longer in the document."
        ) !== null,
    }).toEqual({ drawn: 0, gone: true });
  } finally {
    first.unmount();
  }

  setSystemTime(start + 5_000);
  const again = render(composeAt(client, anchor));
  try {
    // The next read of the document fails: the cache keeps the read that lacks the words.
    expect(client.getQueryState(liveTextQueryOptions(DOCUMENT).queryKey)?.dataUpdatedAt).toBe(
      start + 1_000
    );
    expect(timesDrawn(again.container, QUOTE)).toBe(0);
  } finally {
    setSystemTime();
    again.unmount();
    client.clear();
  }
});

// The architect's ruling of 2026-11-20 20:55Z, approved at 21:57Z: a fact the page holds decides
// alone, whatever its read of the document's text says. The reader's selection lies in another
// comment's pending insert, so each draft's anchor keeps the insert's mark
// (`ComposerAnchor.insertMarks`), and the page's read of the text is the one from before the
// delete, which holds the words. The delete's correction writes the insert's comment deleted into
// the issue's comment list (`markCorrectedPost`), and drops that read (census row V.40); no read
// of the text lands after it. Here the read from before the delete is put back, as a page that
// held it and heard of the delete only through its comment list would hold it: the open composer,
// the offer of a draft kept for the words and the margin's list of what its compose kept each draw
// none of them.
test("a draft whose words lie in a pending insert draws none of them on any surface once the page holds the insert's comment deleted, with no read of the document since", () => {
  const { client, view } = renderSurfaces(QUOTE, textRead(BEFORE), {
    comments: [INSERT_COMMENT],
    insertMarks: [INSERT_MARK],
  });
  const textKey = liveTextQueryOptions(DOCUMENT).queryKey;
  try {
    // While the insert stands, its words are the document's, and each surface quotes them.
    expect(timesDrawn(view.container, QUOTE)).toBe(3);
    const readAt = client.getQueryState(textKey)?.dataUpdatedAt;

    act(() => markCorrectedPost(client, insertDeleted()));
    const dropped = client.getQueryData(textKey);
    act(() => land(client, textRead(BEFORE), readAt));
    const composer = screen.getByRole("form", { name: "Comment composer" });
    expect({
      composer:
        within(composer).queryByText(
          "The words this draft was for are no longer in the document."
        ) !== null,
      drawn: timesDrawn(view.container, QUOTE),
      dropped,
      kept: screen.getByTestId("kept").textContent,
      offered: screen.getByTestId("offered").textContent,
    }).toEqual({
      composer: true,
      drawn: 0,
      dropped: undefined,
      kept: "You wrote this draft for words that are no longer in the document. It is kept here, and offered in the next compose you open on this document:Name who runs it.Discard my draft",
      offered:
        "You wrote this draft for other words, which are no longer in the document. It is kept here:Is this the review we agreed on?Use my draftDiscard my draft",
    });
  } finally {
    view.unmount();
    client.clear();
  }
});

/** The drafts `drafts`, kept in turn under `name` by a page signed in as alice, as the next page
 *  reads them once a reload has brought it over the same tab with the query client `client` and
 *  alice is signed in there: the store of unsent drafts that lives with `client`. */
function keptOverReload(name: string, drafts: readonly AwayDraft[], client: QueryClient) {
  const before = new QueryClient();
  draftStoresSignedIn(before, "alice");
  for (const draft of drafts) unsentDraftsOf(before).keepAway(name, draft);
  // The reload: the next page loads over the same tab, with a query client of its own.
  before.clear();
  draftStoresReloaded();
  draftStoresSignedIn(client, "alice");
  return unsentDraftsOf(client);
}

// A reload after the delete: the tab kept the drafts the margin's compose kept for the words, each
// with the insert's mark in its anchor (`keptAnchor`), and the page that loads holds no read of
// the document's text at all, only the issue's comment list as Dispatch answers it after the
// delete, with the insert's comment deleted. The composer at the words takes the draft kept for
// them, a compose at other words offers the newest one kept for other words, and the margin lists
// the other: none draws the words.
test("after a reload, the drafts the tab kept for words in a deleted insert keep its mark and draw none of the words, with no read of the document", () => {
  const name = composerNameOf(marginComposeSendKey(DOCUMENT));
  const wordsAt = (markId: string) => wordsAnchor(markId, QUOTE, [INSERT_MARK]);
  const keptFor = (markId: string, body: string): AwayDraft => ({
    anchor: wordsAt(markId),
    anchoredAt: Date.now(),
    draft: { body, mentions: [], replacement: "" },
    kind: "comment",
    owner: OWNER,
    replyTo: null,
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const stored = keptOverReload(
    name,
    [keptFor("mark-away", "Name who runs it."), keptFor("mark-insert", "Which review is this?")],
    client
  );
  client.setQueryData(COMMENTS, [
    { ...INSERT_COMMENT, anchor: { ...INSERT_COMMENT.anchor, quote: "" }, body: "", ...DELETION },
  ]);
  const atWords = stored.readAway(name, null, wordsAt("mark-insert"));
  const away = stored.readAway(name, null, wordsAt("mark-away"));
  if (atWords?.anchor === undefined || away?.anchor === undefined) {
    throw new Error("the tab kept no draft for the words");
  }
  const marks = [atWords.anchor.insertMarks, away.anchor.insertMarks];

  const again = render(composeAt(client, atWords.anchor));
  let composer: { drawn: number; field: string; gone: boolean };
  try {
    composer = {
      drawn: timesDrawn(again.container, QUOTE),
      field: screen.getByLabelText<HTMLTextAreaElement>("Comment").value,
      gone:
        screen.queryByText("The words this draft was for are no longer in the document.") !== null,
    };
  } finally {
    again.unmount();
  }

  const others = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={client}>
        <MentionComposer
          anchor={{ artifact: DOCUMENT, mark_id: "mark-release", quote: "Ship the release." }}
          kind="comment"
          mutationKey={marginComposeSendKey(DOCUMENT)}
          onClose={noop}
          onSent={noop}
          owner={OWNER}
          replyTo={null}
        />
        <div data-testid="kept">
          <MarginKeptDrafts drafts={[away]} hidden={false} onDiscard={noop} />
        </div>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    expect({
      composer,
      drawn: timesDrawn(others.container, QUOTE),
      kept: screen.getByTestId("kept").textContent,
      marks,
      offered: screen.getByText("Which review is this?").closest('[role="status"]')?.textContent,
      read: client.getQueryData(liveTextQueryOptions(DOCUMENT).queryKey),
    }).toEqual({
      composer: { drawn: 0, field: "Which review is this?", gone: true },
      drawn: 0,
      kept: "You wrote this draft for words that are no longer in the document. It is kept here, and offered in the next compose you open on this document:Name who runs it.Discard my draft",
      marks: [[INSERT_MARK], [INSERT_MARK]],
      offered:
        "You wrote this draft for other words, which are no longer in the document. It is kept here:Which review is this?",
      read: undefined,
    });
  } finally {
    others.unmount();
    client.clear();
  }
});

/** What each surface draws of a draft's quote: the composer's line in place of it, where it draws
 *  one, how often the words stand in the page, and the two leads. */
function drawnQuotes(container: HTMLElement) {
  const composer = screen.getByRole("form", { name: "Comment composer" });
  return {
    composer: within(composer).queryByText(/^The words this draft was for /)?.textContent,
    drawn: timesDrawn(container, QUOTE),
    kept: screen.getByTestId("kept").textContent,
    offered: screen.getByTestId("offered").textContent,
  };
}

/** What `drawnQuotes` reads while no read that can check the words has landed. */
const LOADING = {
  composer: "The words this draft was for are still loading.",
  drawn: 0,
  kept: "You wrote this draft for words that are still loading. It is kept here, and offered in the next compose you open on this document:Name who runs it.Discard my draft",
  offered:
    "You wrote this draft for other words, which are still loading. It is kept here:Is this the review we agreed on?Use my draftDiscard my draft",
};

/** What `drawnQuotes` reads once the words are drawn on each surface again. */
const DRAWN = {
  composer: undefined,
  drawn: 3,
  kept: `You wrote this draft for the words “${QUOTE}”. It is kept here, and offered in the next compose you open on this document:Name who runs it.Discard my draft`,
  offered: `You wrote this draft for other words: “${QUOTE}”. It is kept here:Is this the review we agreed on?Use my draftDiscard my draft`,
};

/** What `drawnQuotes` reads once each read that can check the words has failed with no answer. */
const UNCHECKED = {
  composer: "The words this draft was for cannot be checked now.",
  drawn: 0,
  kept: "You wrote this draft for words that cannot be checked now. It is kept here, and offered in the next compose you open on this document:Name who runs it.Discard my draft",
  offered:
    "You wrote this draft for other words, which cannot be checked now. It is kept here:Is this the review we agreed on?Use my draftDiscard my draft",
};

/** What `drawnQuotes` reads once a read of the document's text that can say so lacks the words. */
const GONE = {
  composer: "The words this draft was for are no longer in the document.",
  drawn: 0,
  kept: "You wrote this draft for words that are no longer in the document. It is kept here, and offered in the next compose you open on this document:Name who runs it.Discard my draft",
  offered:
    "You wrote this draft for other words, which are no longer in the document. It is kept here:Is this the review we agreed on?Use my draftDiscard my draft",
};

// The architect's ruling of 2026-11-21 00:17Z on dispatch://INFRA-1523 (census rows RM.1-RM.3 and
// 11.9). After a reload the page holds no read of the document's text and none of the margin's
// comment list, and neither the time gate nor the deleted insert's fact can speak. While the
// page's first reads of the two are out, each surface draws a draft's quote as loading, with none
// of its words and no failure; the quote is drawn again once a read lands that holds the words.
test("after a reload, while the page's first reads of the document and its comments are out, no surface draws a word of a draft's quote, and each draws it once the text read lands holding the words", async () => {
  const { client, view } = renderSurfaces(QUOTE, undefined, { insertMarks: [INSERT_MARK] });
  const text = Promise.withResolvers<ArtifactText>();
  const comments = Promise.withResolvers<Comment[]>();
  try {
    act(() => {
      void client.prefetchQuery({ ...liveTextQueryOptions(DOCUMENT), queryFn: () => text.promise });
      void client.prefetchQuery({ queryFn: () => comments.promise, queryKey: COMMENTS });
    });
    const loading = drawnQuotes(view.container);

    await act(async () => {
      text.resolve(textRead(BEFORE));
      await text.promise;
    });
    expect({ landed: drawnQuotes(view.container), loading }).toEqual({
      landed: DRAWN,
      loading: LOADING,
    });
  } finally {
    comments.resolve([]);
    view.unmount();
    client.clear();
  }
});

/** Dispatch's refusal of a read while it is unavailable. */
const UNAVAILABLE = new ApiError(503, { code: "UNAVAILABLE", error: "Dispatch is unavailable" });

// The same reload, whose first reads fail. One failed read with the other still out is a load
// still under way, and the quote still draws as loading; once both have failed, each surface says
// the words cannot be checked now, and draws none of them. The comment list read again lands with
// the insert's comment standing, and each surface draws the quote again, the text read still
// failed.
test("after a reload whose first reads of the document and its comments both fail, each surface says a draft's words cannot be checked now and draws none of them, and draws them once the comments read lands", async () => {
  const { client, view } = renderSurfaces(QUOTE, undefined, { insertMarks: [INSERT_MARK] });
  const comments = Promise.withResolvers<Comment[]>();
  try {
    await act(async () => {
      void client.prefetchQuery({ queryFn: () => comments.promise, queryKey: COMMENTS });
      await client.prefetchQuery({
        ...liveTextQueryOptions(DOCUMENT),
        queryFn: () => Promise.reject(UNAVAILABLE),
      });
    });
    const oneFailed = drawnQuotes(view.container);

    await act(async () => {
      comments.reject(UNAVAILABLE);
      await comments.promise.catch(() => undefined);
    });
    const bothFailed = drawnQuotes(view.container);

    act(() => client.setQueryData(COMMENTS, [INSERT_COMMENT]));
    expect({
      bothFailed,
      commentsRead: drawnQuotes(view.container),
      oneFailed,
      text: client.getQueryState(liveTextQueryOptions(DOCUMENT).queryKey)?.status,
    }).toEqual({
      bothFailed: UNCHECKED,
      commentsRead: DRAWN,
      oneFailed: LOADING,
      text: "error",
    });
  } finally {
    view.unmount();
    client.clear();
  }
});

// The architect's ruling of 2026-11-21 01:49Z on dispatch://INFRA-1523. A draft or held send an
// older build kept names no list of the pending inserts its words lie in
// (`ComposerAnchor.insertMarks` absent, which nothing reads as an empty list), so the page's
// comment list cannot say that its words lie in no insert whose comment is deleted. A reload
// after the delete: the issue's comment list lands as Dispatch answers it, with the insert's
// comment deleted, which nothing ties to the draft's words, and the page's first read of the
// document's text is out, then fails. For such a draft the list counts as no read: each surface
// draws its quote as loading while the text read is out, says the words cannot be checked now
// once it has failed, and draws none of them; the read that lands after the delete lacks them,
// and each says they are gone.
test("a draft an older build kept, which names no list of inserts, draws no word of its quote on any surface while the page holds its comments and its read of the document's text fails", async () => {
  const start = Date.now();
  setSystemTime(start);
  const { client, view } = renderSurfaces(QUOTE, undefined);
  const text = Promise.withResolvers<ArtifactText>();
  try {
    act(() => {
      client.setQueryData(COMMENTS, [
        {
          ...INSERT_COMMENT,
          anchor: { ...INSERT_COMMENT.anchor, quote: "" },
          body: "",
          ...DELETION,
        },
      ]);
      void client.prefetchQuery({ ...liveTextQueryOptions(DOCUMENT), queryFn: () => text.promise });
    });
    const reading = drawnQuotes(view.container);

    await act(async () => {
      text.reject(UNAVAILABLE);
      await text.promise.catch(() => undefined);
    });
    const failed = drawnQuotes(view.container);

    setSystemTime(start + 1_000);
    act(() => land(client, textRead(AFTER)));
    expect({ failed, gone: drawnQuotes(view.container), reading }).toEqual({
      failed: UNCHECKED,
      gone: GONE,
      reading: LOADING,
    });
  } finally {
    setSystemTime();
    view.unmount();
    client.clear();
  }
});

// The same ruling's two halves under the same reads: the page's first read of the document's text
// fails, and then the issue's comment list lands, with no insert's comment deleted. A draft that
// names the inserts its words lie in - none, an empty list, as the selection bar names words in
// no insert (`insertMarksIn`) - draws its quote as soon as the list lands. One an older build
// kept, which names no list, says the words cannot be checked now, the list landed or not, and
// draws them only once a read of the text lands that holds them.
test("a draft naming an empty list of inserts draws its quote as soon as the page's comments land, and one an older build kept draws it only once a text read lands holding the words", async () => {
  /** What each surface draws of a draft whose words name the inserts `insertMarks`, or no list,
   *  as the page's read of the document's text fails, as its comments land after that, and as a
   *  text read holding the words lands. */
  const drawnAsReadsLand = async (insertMarks?: readonly string[]) => {
    const { client, view } = renderSurfaces(QUOTE, undefined, { insertMarks });
    try {
      await act(async () => {
        await client.prefetchQuery({
          ...liveTextQueryOptions(DOCUMENT),
          queryFn: () => Promise.reject(UNAVAILABLE),
        });
      });
      const textFailed = drawnQuotes(view.container);
      act(() => client.setQueryData(COMMENTS, []));
      const listed = drawnQuotes(view.container);
      act(() => land(client, textRead(BEFORE)));
      return { listed, textFailed, textLanded: drawnQuotes(view.container) };
    } finally {
      view.unmount();
      client.clear();
    }
  };
  expect({ empty: await drawnAsReadsLand([]), older: await drawnAsReadsLand() }).toEqual({
    empty: { listed: DRAWN, textFailed: LOADING, textLanded: DRAWN },
    older: { listed: UNCHECKED, textFailed: UNCHECKED, textLanded: DRAWN },
  });
});

// The same ruling, carried to the time gate. The gate is for words a person has just selected in
// a live editor, which this build names a list of inserts for (`insertMarksIn`): a read from
// before the compose took them can lack them while they are on screen. Words that name no list -
// a draft or held send an older build kept, or a selection the editor held no document for - may
// lie in an insert whose comment is deleted, and a composer that mounts at them with an empty
// field takes its mount as their time, later than a read that lacks them. Under the reads of the
// row "words typed after the page last read the document …" - a read a minute old that lacks the
// words, a composer mounted now, and two drafts whose composes took them a second ago - no
// surface takes that read to be older than words that name no list: each says they are no longer
// in the document, and draws none of them.
test("words that name no list of inserts are gone on every surface once the page's read of the document lacks them, however old that read is", () => {
  const now = Date.now();
  const { client, view } = renderSurfaces(QUOTE, textRead(AFTER), {
    awayAt: now - 1_000,
    offeredAt: now - 1_000,
    readAt: now - 60_000,
  });
  try {
    expect(drawnQuotes(view.container)).toEqual(GONE);
  } finally {
    view.unmount();
    client.clear();
  }
});

// The time gate's other side, for words that name the inserts they lie in (census rows 11.9 and
// RM.1): the gate weighs a read's time against the compose's only for words that name such a
// list, an empty one too, and the rows above whose read is newer than the compose name none.
// Here the words lie in a pending insert no comment list the page holds names, so no fact decides
// first, and the composer and the two drafts took them a second before a read of the document's
// text lands that lacks them: newer than each compose, it says they left, and each surface draws
// none of them.
test("words that name a list of inserts are gone on every surface once a read of the document newer than each compose that took them lacks them", () => {
  const start = Date.now();
  setSystemTime(start);
  const { client, view } = renderSurfaces(QUOTE, textRead(BEFORE), {
    awayAt: start - 1_000,
    insertMarks: [INSERT_MARK],
    offeredAt: start - 1_000,
    readAt: start - 60_000,
  });
  try {
    const held = drawnQuotes(view.container);
    setSystemTime(start + 1_000);
    act(() => land(client, textRead(AFTER)));
    expect({ gone: drawnQuotes(view.container), held }).toEqual({ gone: GONE, held: DRAWN });
  } finally {
    setSystemTime();
    view.unmount();
    client.clear();
  }
});

/** The words of the margin fixture's spec the selection bar names, which lie in the pending insert
 *  `INSERT_MARK`. */
const SPEC_WORDS: ComposerAnchor = {
  artifact: specArtifact.id,
  insertMarks: [INSERT_MARK],
  mark_id: "mark-open",
  quote: QUOTE,
};

/** The selection bar's compose at `SPEC_WORDS`, on a press. */
function ComposeOnWords(): ReactNode {
  const { composeForMark } = useMargin();
  return (
    <button
      onClick={() => {
        void composeForMark({ anchor: SPEC_WORDS, kind: "comment" }).catch(noop);
      }}
      type="button"
    >
      Compose on the selection
    </button>
  );
}

/** The issue's page over `client` with its margin, as the document page draws it, and the
 *  selection bar's compose; `away` takes the document and its margin off the page, as leaving the
 *  document does, while the provider keeps the compose open for the reader's return. */
function issuePageAwayOrBack(client: QueryClient, away: boolean) {
  return (
    <MemoryRouter
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
      initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}
    >
      <QueryClientProvider client={client}>
        <MarginProvider>
          <ComposeOnWords />
          {away ? null : (
            <>
              <RegisterDocument />
              <Margin />
            </>
          )}
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// Census rows 11.9 and RM.1 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-21
// 03:47Z: a composer that mounts again on a compose still open takes the compose's own time, when
// it opened, not its mount's. The reader opens the margin's compose at words in a pending insert,
// types nothing and leaves the document, so the composer keeps no draft and takes no time from
// one. The insert's comment is deleted, which no comment list the page holds names, and the read
// of the document's text after the delete lands, lacking the words. The reader comes back, and
// the composer mounts again: that read is newer than the compose, though older than the mount, so
// it says the words left, and the composer draws none of them.
test("a margin composer that mounts again with an empty field on its open compose keeps the compose's time, and draws none of the words a read since the compose lacks", async () => {
  const user = userEvent.setup();
  const start = Date.now();
  setSystemTime(start);
  const client = marginClient([]);
  const textKey = liveTextQueryOptions(specArtifact.id).queryKey;
  client.setQueryData(textKey, textRead(BEFORE), { updatedAt: start - 60_000 });
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(issue),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "getInbox").mockResolvedValue([]),
  ];
  const view = render(issuePageAwayOrBack(client, false));
  try {
    await user.click(screen.getByRole("button", { name: "Compose on the selection" }));
    await screen.findByRole("form", { name: "Comment composer" });
    const opened = timesDrawn(view.container, QUOTE);

    setSystemTime(start + 1_000);
    view.rerender(issuePageAwayOrBack(client, true));
    setSystemTime(start + 2_000);
    act(() => client.setQueryData(textKey, textRead(AFTER)));

    setSystemTime(start + 5_000);
    view.rerender(issuePageAwayOrBack(client, false));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    expect({
      drawn: timesDrawn(view.container, QUOTE),
      field: within(composer).getByLabelText<HTMLTextAreaElement>("Comment").value,
      gone:
        within(composer).queryByText(
          "The words this draft was for are no longer in the document."
        ) !== null,
      opened,
    }).toEqual({ drawn: 0, field: "", gone: true, opened: 1 });
  } finally {
    setSystemTime();
    view.unmount();
    for (const read of reads) read.mockRestore();
    client.clear();
  }
});

// The same ruling: absent is not empty anywhere on the way, the tab's storage too. The tab keeps a
// draft as its JSON, so after a reload a draft kept with no list of inserts, as an older build
// kept every draft, reads back with no field for them, never an empty list, which would let the
// page's comment list draw its words; and a draft kept with an empty list reads it back.
test("after a reload, a draft kept with no list of inserts reads back with none, not an empty one, and one kept with an empty list reads it back", () => {
  const name = composerNameOf(marginComposeSendKey(DOCUMENT));
  const older = wordsAnchor("mark-older", QUOTE);
  const empty = wordsAnchor("mark-empty", QUOTE, []);
  const keptAt = (anchor: ComposerAnchor): AwayDraft => ({
    anchor,
    draft: { body: "Name who runs it.", mentions: [], replacement: "" },
    kind: "comment",
    owner: OWNER,
    replyTo: null,
  });
  const client = new QueryClient();
  const stored = keptOverReload(name, [keptAt(older), keptAt(empty)], client);
  /** What the draft kept at `anchor`'s words names of the inserts they lie in, read back. */
  const marksReadBack = (anchor: ComposerAnchor) => {
    const kept = stored.readAway(name, null, anchor)?.anchor;
    if (kept === undefined) return "no draft";
    return "insertMarks" in kept ? kept.insertMarks : "no field";
  };
  try {
    expect({ empty: marksReadBack(empty), older: marksReadBack(older) }).toEqual({
      empty: [],
      older: "no field",
    });
  } finally {
    stored.forgetAway(name, { anchor: older, replyTo: null });
    stored.forgetAway(name, { anchor: empty, replyTo: null });
    client.clear();
  }
});
