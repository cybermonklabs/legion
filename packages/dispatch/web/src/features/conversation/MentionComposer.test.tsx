import { afterEach, beforeAll, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import {
  type DispatchWritingBox,
  dispatchWritingBox,
  IDEMPOTENCY_KEY_TTL_MS,
} from "@legion/contracts";
import {
  type MutationKey,
  onlineManager,
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: a paste and a drop of files, an IME composition key, an edit into the control a send holds read-only, the same-batch pairs inside act, and the hand-made beforeinput an edit's blocks are followed by
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { type ComponentProps, createRef, type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { jump, realClock, settle, tick } from "../../__tests__/fake-clock";
import { runFocusFixup } from "../../__tests__/focus-fixup";
import { graphNode } from "../../__tests__/graph-fixture";
import { heldNotDisabled } from "../../__tests__/held-press";
import { INPUT_METHOD_MARKS } from "../../__tests__/input-method-marks";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { recordEscapes } from "../../__tests__/record-escapes";
import {
  architectureTree,
  issueCollaborators,
  searchResponse,
} from "../../__tests__/search-fixture";
import { untilAttempted } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type {
  Agent,
  AskRead,
  Comment,
  IssueCollaborators,
  IssueReferenceSummary,
  ItemBlock,
  ItemBlocks,
  Message,
} from "../../api/types";
import { useSending } from "../../hooks/useSending";
import { emulateChromiumFocusFixup } from "../issue-page/chromium-focus-fixup";
import type { AgentText } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { mentionOptions, stoppedRoleOptions } from "../refs/party-options";
import { settleRefPickerSearch } from "../refs/ref-picker-fixture";
import { recordedRoles, roleHolders } from "../refs/role-holders";
import { IS_MAC } from "../shell/key-text";
import {
  KEYED_SEND_ATTEMPT_MS,
  SEND_DEADLINE_MS,
  SendExpiredError,
  SendStoppedError,
} from "../writes/keyed-write";
import type {
  ComposerOwner,
  DraftIntake,
  ListedReplies,
  ListedReply,
  ReplyParent,
  ReplyTarget,
} from "./composer-model";
import { MentionComposer } from "./MentionComposer";
import { KEYED_SEND_LATE_TEXT } from "./send-request";
import { unsentDraftsOf } from "./unsent-drafts";
import { composerNameOf } from "./useKeptDrafts";

/** What each attempt of a send is held to, beside the request it carries: its deadline and the
 *  key that names the press, sent as `Idempotency-Key`. */
const keyedAttempt = expect.objectContaining({
  deadlineMs: KEYED_SEND_ATTEMPT_MS,
  key: expect.any(String),
});

/** Dispatch's verdict on a comment. A 5xx, a 429 or a failed fetch is not one: the send is tried
 *  again under its key, so a row that needs a comment refused refuses it with a 4xx. */
const COMMENT_REFUSED = "the comment was refused";
const commentRefused = new ApiError(400, { code: "COMMENT_INPUT", error: COMMENT_REFUSED });

const planner: Agent = {
  capabilities: ["btw"],
  state: "live",
  dir: "/workspaces/planner",
  host: "host-a",
  last_activity: null,
  last_seen: 1_700_000_000_000,
  machine_id: "host-a",
  open_asks: 0,
  roles: ["legion-planner"],
  session_id: "A",
  title: "Planner",
};

const worker: Agent = {
  capabilities: [],
  state: "live",
  dir: "/workspaces/worker",
  host: "host-b",
  last_activity: null,
  last_seen: 1_700_000_000_001,
  machine_id: "host-b",
  open_asks: 0,
  roles: [],
  session_id: "B",
  title: "Worker",
};

/** A reply parent's author: a user, drawn as plain text, so a quote reads `Replying to Planner`
 *  with no registry read. */
const plannerAuthor = { id: "Planner", kind: "user" } as const;

const createdComment: Comment = {
  anchor: null,
  answers: null,
  artifact_id: null,
  ask_id: null,
  reply_block: null,
  author: { id: "alice", kind: "user" },
  body: "sent",
  created_at: "2026-09-18T00:00:00Z",
  deleted_at: null,
  deleted_by: null,
  deliveries: [],
  edited_at: null,
  id: "comment-1",
  issue_key: "CORE-1",
  mentions: [],
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
};

interface ComposerOptions {
  agents?: readonly Agent[];
  anchor?: { artifact: string; mark_id: string; quote: string };
  /** Which box of the writing list this mount stands for; `boxFor` picks the one a host with
   *  these props would be (`DISPATCH_WRITING_BOXES`). */
  box?: DispatchWritingBox;
  closed?: boolean;
  edit?: { body: string; id: string };
  kind?: "ask" | "comment" | "suggestion";
  seedMentions?: readonly { target: string; title: string }[];
  onCancelReply?: () => void;
  onKindChange?: (kind: "ask" | "comment" | "suggestion") => string | undefined;
  onSent?: () => void;
  owner?: ComposerOwner;
  replyTo?: (Omit<ReplyTarget, "parentKind"> & { parentKind?: ReplyTarget["parentKind"] }) | null;
  inMain?: boolean;
  /** The host's copy of the post `replyTo` answers, as its read holds it now. */
  replyParent?: ReplyParent;
}

/** The box a host passing these props would be, in the order the composer reads its own write
 *  in: an edit is a margin card's editor, a suggestion or an ask the margin's composer, a
 *  message to one session an Agents row, and everything else the issue's Conversation. */
function boxFor(props: ComposerOptions): DispatchWritingBox {
  if (props.edit !== undefined) return "margin-comment-edit";
  if (props.anchor !== undefined || props.onKindChange !== undefined || props.kind !== undefined) {
    return "margin-composer";
  }
  if (props.owner?.kind === "session") return "agents-row-composer";
  return "conversation-composer";
}

/** Renders the composer, and hands back `rerender` for a host that changes its props on the live
 *  instance - an Agents row's owner and seed when the reader picks an issue. `inMain` draws it in
 *  the shell's main content, as the app shell draws it. */
function renderComposer(options: ComposerOptions = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const composer = (props: ComposerOptions) => {
    const field = (
      <MentionComposer
        agents={props.agents}
        anchor={props.anchor}
        box={props.box ?? boxFor(props)}
        closed={props.closed}
        edit={props.edit}
        kind={props.kind}
        seedMentions={props.seedMentions}
        onCancelReply={props.onCancelReply}
        onClose={() => {}}
        onKindChange={props.onKindChange}
        onSent={props.onSent ?? (() => {})}
        owner={props.owner ?? { issueKey: "CORE-1", kind: "issue" }}
        replyParent={props.replyParent}
        replyTo={
          props.replyTo === null
            ? null
            : props.replyTo === undefined
              ? undefined
              : { ...props.replyTo, parentKind: props.replyTo.parentKind ?? "comment" }
        }
      />
    );
    return (
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <QueryClientProvider client={queryClient}>
          {props.inMain ? <main>{field}</main> : field}
        </QueryClientProvider>
      </MemoryRouter>
    );
  };
  const view = render(composer(options));
  return { queryClient, rerender: (next: ComposerOptions) => view.rerender(composer(next)), view };
}

/** Whether a send holds the composer's controls: their fieldset is `aria-disabled`. It is never
 *  `disabled`, which takes the focus off the control the reader sent from. */
function composerHeld(): boolean {
  const controls = screen.getByRole("form", { name: "Comment composer" }).querySelector("fieldset");
  if (!(controls instanceof HTMLFieldSetElement))
    throw new Error("expected composer control fieldset");
  return heldNotDisabled(controls, "the composer's control fieldset");
}

/** What holds the focus, as a failed check prints it: a control's accessible label or its text. */
function focusedName(): string {
  const active = document.activeElement;
  if (active === null || active === document.body) return "the page's body";
  return active.getAttribute("aria-label") ?? active.textContent ?? active.tagName;
}

/** Every source the picker reads when a door opens over the composer, and a read no handler
 *  answers fails its test: the issue's collaborators (its people and the roles nobody is
 *  running), the project's components and the search behind the kinds that need a query. The
 *  rows that are about a stopped role answer the collaborators themselves. The pills under a
 *  draft draw each reference as the stored record will (`RefLink`), so an issue a pill names is
 *  read for its title (`summaryOf`), and a session the registry does not list is asked about
 *  once: Dispatch holds no record of one here. */
let collaboratorsRead: Mock<typeof api.getIssueCollaborators>;
let architectureRead: Mock<typeof api.getArchitecture>;
let searchRead: Mock<typeof api.search>;
let knownRead: Mock<typeof api.getKnownSessions>;
let summaryRead: Mock<typeof api.getIssueSummary>;
// The pills read the draft through the parse (`draftReading`), and a quote is drawn by the lazy
// Markdown engine, and several rows read them on the fake clock, where the engine's load cannot
// land. The rows run in random order, so the file loads the engine before its first row,
// whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

beforeEach(() => {
  collaboratorsRead = spyOn(api, "getIssueCollaborators").mockResolvedValue(issueCollaborators());
  architectureRead = spyOn(api, "getArchitecture").mockResolvedValue(architectureTree());
  searchRead = spyOn(api, "search").mockResolvedValue(searchResponse([]));
  knownRead = spyOn(api, "getKnownSessions").mockResolvedValue({ sessions: [] });
  summaryRead = spyOn(api, "getIssueSummary").mockImplementation((key) =>
    Promise.resolve(summaryOf(key))
  );
});
afterEach(() => {
  collaboratorsRead.mockRestore();
  architectureRead.mockRestore();
  searchRead.mockRestore();
  knownRead.mockRestore();
  summaryRead.mockRestore();
});

// Whether a typed `/btw` is a command is decided by the parties the shared reader finds in the
// draft: with nobody named it is the reader's own first word, and with a person named - who is
// delivered nothing - it is the command it looks like.
test("a comment that names nobody preserves /btw and omits delivery", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("/btw hello");
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", { body: "/btw hello" }, keyedAttempt)
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a draft whose only party is a person strips /btw and sends a mode nobody is told in", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("/btw [Alice](dispatch://person/alice@example.com) please look");
    // A person is no delivery target, so the box offers no chip for one.
    expect(screen.queryByRole("list", { name: "Mention targets" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "[Alice](dispatch://person/alice@example.com) please look",
          delivery: "btw",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

/** What a reference to CORE-1 or CORE-2 shows: CORE-1's spec is the document its own text's
 *  `#id`s resolve against. */
function summaryOf(key: string): IssueReferenceSummary {
  return {
    documents:
      key === "CORE-1"
        ? [
            {
              id: "spec-1",
              kind: "doc",
              latest_version: 1,
              name: "spec.md",
              primary: true,
              slug: "spec-md",
            },
          ]
        : [],
    key,
    project: "CORE",
    status: "todo",
    title: key === "CORE-1" ? "The host" : "Warm the cache",
  };
}

/** The ask `#q2` names, as its reference reads it for the title. */
function askRead(id: string): AskRead {
  return {
    answers: [],
    ask: {
      anchor: null,
      answer: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      author: { id: "planner", kind: "session" },
      block_id: "q2",
      created_at: "2026-09-09T00:00:00Z",
      edited_at: null,
      id,
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [],
      question: "Which way round?",
      state: "open",
      urgency: "med",
    },
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  };
}

/** Each pill under the draft, by its accessible name and the page it opens. */
function pillsDrawn(pills: HTMLElement): { href: string | null; name: string }[] {
  return within(pills)
    .getAllByRole("link")
    .map((pill) => ({ href: pill.getAttribute("href"), name: pill.textContent ?? "" }));
}

// Each pill under the draft is a link to the page its reference opens, written through the one
// gate between a typed reference and an `href` (`routes.ts`'s `appHref`), and reads as the stored
// record will. A person has no page, so the draft that names one shows no pill for them: an
// empty `href` navigates to this very page, which would drop the draft the reader is writing.
test("a person named in the draft gets no pill, and a reference with a page gets one", async () => {
  const user = userEvent.setup();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("[@Alice](dispatch://person/alice@example.com) and dispatch://CORE-2");
    const pills = await screen.findByRole("region", { name: "References" });
    await waitFor(() =>
      expect(pillsDrawn(pills)).toEqual([{ href: "/issues/CORE-2", name: "CORE-2 Warm the cache" }])
    );
  } finally {
    view.unmount();
  }
});

// A picture is drawn as the picture in the stored record, never as a link to its document, so
// the draft shows no pill for it either; a reference beside it still gets one.
test("a picture in the draft gets no pill", async () => {
  const user = userEvent.setup();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.paste("![shot.png](dispatch://CORE-1/artifact/shot-png@v1) and dispatch://CORE-2");
    const pills = await screen.findByRole("region", { name: "References" });
    await waitFor(() =>
      expect(pillsDrawn(pills)).toEqual([{ href: "/issues/CORE-2", name: "CORE-2 Warm the cache" }])
    );
    // No pill at all for the picture, not even one that lost its link: its document is not one
    // CORE-1's summary lists, so a pill for it would read as its words with no link.
    expect(pills.querySelectorAll("a")).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

// The pills read the draft once the typing rests: a key typed a letter at a time passes through
// `CORE-1` and `CORE-12` on its way to `CORE-123`, and only the key the reader stopped at is read.
// The clock is the test's, so no keystroke can fall behind the rest by chance.
test("a key typed letter by letter reads only the summary of the key the typing rested on", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("See CORE-123");
    await settleRefPickerSearch("fake");
    await tick(20);
    expect(summaryRead.mock.calls.map(([key]) => key)).toEqual(["CORE-123"]);
    expect(pillsDrawn(screen.getByRole("region", { name: "References" }))).toEqual([
      { href: "/issues/CORE-123", name: "CORE-123 Warm the cache" },
    ]);
  } finally {
    realClock();
    view.unmount();
  }
});

// A draft emptied takes its pills with it at once, rather than once the typing rests: the clock
// does not move between the clear and the check.
test("an emptied draft takes its pills with it at once", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.paste("See CORE-2");
    await settleRefPickerSearch("fake");
    await tick(20);
    expect(pillsDrawn(screen.getByRole("region", { name: "References" }))).toEqual([
      { href: "/issues/CORE-2", name: "CORE-2 Warm the cache" },
    ]);
    await user.clear(field);
    expect(screen.queryByRole("region", { name: "References" })).toBeNull();
  } finally {
    realClock();
    view.unmount();
  }
});

// R7: the pills read the draft with the facts the renderer reads the stored record with, and
// with the record the write will have - here an issue comment, whose `#id`s resolve against its
// issue's spec. So a bare key whose issue exists and a `#id` the spec declares get the titled
// pills the stored comment's links are, and a key no issue holds gets none, as it gets no link.
test("a draft's bare key and #id get the titled pills a reader of the stored text sees", async () => {
  const user = userEvent.setup();
  summaryRead.mockImplementation((key) =>
    key === "CORE-9"
      ? Promise.reject(new ApiError(404, { code: "NOT_FOUND", error: "no such issue" }))
      : Promise.resolve(summaryOf(key))
  );
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
    document: "spec-1",
    questions: [{ ask: "ask-2", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-2" }],
  });
  const getAsk = spyOn(api, "getAsk").mockImplementation((id) => Promise.resolve(askRead(id)));
  const text = "See CORE-2, CORE-9 and #q2 here.";
  const { view } = renderComposer();
  const stored = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <MarkdownBody
          markdown={text}
          source={{ id: "comment-1", issue: "CORE-1", kind: "comment" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.paste(text);
    const pills = await screen.findByRole("region", { name: "References" });
    const titled = [
      { href: "/issues/CORE-2", name: "CORE-2 Warm the cache" },
      { href: "/issues/CORE-1/asks/ask-2", name: "Which way round?" },
    ];
    await waitFor(() => expect(pillsDrawn(pills)).toEqual(titled));
    // The draft's `#id` is read against the spec its comment will be written on.
    expect(getReferenceQuestions).toHaveBeenCalledWith("artifact", "spec-1");
    // The stored comment the reader sees links the same two, titled the same way.
    await waitFor(() =>
      expect(
        within(stored.container)
          .getAllByRole("link")
          .map((link) => ({ href: link.getAttribute("href"), name: link.textContent ?? "" }))
      ).toEqual(titled)
    );
  } finally {
    view.unmount();
    stored.unmount();
    getReferenceQuestions.mockRestore();
    getAsk.mockRestore();
  }
});

// The record the write will have decides which question map a draft's `#id` reads, and nothing
// else does (`draftSource`): a reply to a comment on an issue is in its parent's scope, so its
// `#q2` gets the pill that scope declares, never the one the issue's spec would.
test("a reply's #id gets the pill its parent comment's scope declares, not the spec's", async () => {
  const user = userEvent.setup();
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockImplementation(
    async (kind) =>
      kind === "comment"
        ? {
            document: "notes-1",
            questions: [{ ask: "ask-2", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-2" }],
          }
        : {
            document: "spec-1",
            questions: [{ ask: "ask-3", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-3" }],
          }
  );
  const getAsk = spyOn(api, "getAsk").mockImplementation((id) => Promise.resolve(askRead(id)));
  const { view } = renderComposer({
    replyTo: { actor: plannerAuthor, excerpt: "Question", id: "comment-9" },
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.paste("See #q2 here.");
    const pills = await screen.findByRole("region", { name: "References" });
    await waitFor(() =>
      expect(pillsDrawn(pills)).toEqual([
        { href: "/issues/CORE-1/asks/ask-2", name: "Which way round?" },
      ])
    );
    expect(getReferenceQuestions.mock.calls).toEqual([["comment", "comment-9"]]);
  } finally {
    view.unmount();
    getReferenceQuestions.mockRestore();
    getAsk.mockRestore();
  }
});

/** The links of this dashboard a rendered body draws, by what each reads as and the page it
 *  opens. A link whose words are partly formatted is drawn as one anchor for each stretch of
 *  them, so anchors that follow each other to one page are read as the one link they are. An
 *  author's link to somewhere else is no reference and is left out. */
function storedLinks(body: HTMLElement): { href: string | null; name: string }[] {
  const links: { href: string | null; name: string }[] = [];
  for (const anchor of within(body).getAllByRole("link")) {
    const href = anchor.getAttribute("href");
    if (href?.startsWith("/") !== true) continue;
    const last = links.at(-1);
    if (last?.href === href) last.name += anchor.textContent ?? "";
    else links.push({ href, name: anchor.textContent ?? "" });
  }
  return links;
}

/** Pastes `text` into a comment's draft and holds its pills to `expected`, and to the links the
 *  stored comment draws for the same text (`storedLinks`). */
async function expectPillsAsStored(
  text: string,
  expected: { href: string; name: string }[]
): Promise<void> {
  const user = userEvent.setup();
  const { view } = renderComposer();
  const stored = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <MarkdownBody
          markdown={text}
          source={{ id: "comment-1", issue: "CORE-1", kind: "comment" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await user.click(screen.getByLabelText("Comment"));
    await user.paste(text);
    const pills = await screen.findByRole("region", { name: "References" });
    await waitFor(() => expect(pillsDrawn(pills)).toEqual(expected));
    await waitFor(() => expect(storedLinks(stored.container)).toEqual(expected));
  } finally {
    view.unmount();
    stored.unmount();
  }
}

// A reference written in another link's words is part of those words: the renderer links none
// under a link (`RefLink.tsx`'s `excludedRefAncestorTags`), so the draft shows no pill for it.
// The key in the words of a link to somewhere else gets none, and the address beside it gets its
// own.
test("a key in the words of a link to somewhere else gets no pill", async () => {
  await expectPillsAsStored("[see CORE-1 here](https://x) and dispatch://CORE-2", [
    { href: "/issues/CORE-2", name: "CORE-2 Warm the cache" },
  ]);
});

// A link whose words are one issue's key and whose address is another's is one reference, to
// the issue its address names, read as the words its author wrote.
test("a key that is the words of a reference's link gets no pill of its own", async () => {
  await expectPillsAsStored("[CORE-1](dispatch://CORE-2)", [
    { href: "/issues/CORE-2", name: "CORE-1" },
  ]);
});

// R4: a seeded mention's pill reads as `@` and the party's current name, as the stored record
// will, though the draft wrote the name the session had when the channel seeded it.
test("a mention's pill reads as @ and the party's current name", async () => {
  // The registry the pill reads names the session as it is called now.
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    { ...planner, title: "Planner renamed" },
  ]);
  const { view } = renderComposer({
    agents: [planner],
    seedMentions: [{ target: "session:A", title: "Planner" }],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(field.value).toBe("[@Planner](dispatch://agent/A) "));
    const pills = await screen.findByRole("region", { name: "References" });
    await waitFor(() =>
      expect(pillsDrawn(pills)).toEqual([{ href: "/agents/A/live", name: "@Planner renamed" }])
    );
  } finally {
    view.unmount();
    listAgents.mockRestore();
  }
});

/** The registry names the planner as it is called now, while `text` is read as a draft and as
 *  the stored comment (`expectPillsAsStored`). */
async function expectPlannerPillAsStored(text: string, name: string): Promise<void> {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    { ...planner, title: "Planner renamed" },
  ]);
  try {
    await expectPillsAsStored(text, [{ href: "/agents/A/live", name }]);
  } finally {
    listAgents.mockRestore();
  }
}

// Formatting over all of a mention's words is formatting around the link, so the pill still
// reads as `@` and the party's current name, as the stored record does.
test("a mention written bold whole reads as @ and the party's current name in its pill", async () => {
  await expectPlannerPillAsStored("[**@Planner**](dispatch://agent/A)", "@Planner renamed");
});

// Formatting over some of a link's words makes them words its author wrote, and so does a code
// span: the pill keeps them, as the stored record does, rather than reading them as a mention.
test("a mention's link with part of its words bold reads as the words written in its pill", async () => {
  await expectPlannerPillAsStored("[@Planner **now**](dispatch://agent/A)", "@Planner now");
});

test("a mention's link whose words are code reads as the words written in its pill", async () => {
  await expectPlannerPillAsStored("[`@Planner`](dispatch://agent/A)", "@Planner");
});

// A role no running session holds is still known where the issue's collaborators record it
// (`role-holders.ts`), so the stored comment on CORE-1 names it and links it. The pill is read on
// the issue the draft is written on (`RecordIssue`), so it reads the same way: as `@` and the
// role's token, linked to the role's page, rather than as the words the draft wrote.
test("a stopped role the issue records reads as its token in its pill, as on the stored comment", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  collaboratorsRead.mockResolvedValue(
    issueCollaborators({ agents: collaboratorsAnswer.agents ?? [] })
  );
  try {
    await expectPillsAsStored("[@Reviewer](dispatch://role/legion-p-core-1-reviewer)", [
      { href: "/agents?role=legion-p-core-1-reviewer", name: "@legion-p-core-1-reviewer" },
    ]);
  } finally {
    listAgents.mockRestore();
  }
});

// A mention reaches a draft from the channel it is written in (`seedMentions`) or from the
// reply it answers: a pick in the reference picker writes the party's link and no record
// beside it, so these rows seed what they deliver.
test("a seeded mention with /btw posts the canonical target and stripped body", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({
    agents: [planner],
    seedMentions: [{ target: "session:A", title: "Planner" }],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    act(() => field.focus());
    field.setSelectionRange(0, 0);
    await user.paste("/btw ");
    await waitFor(() => expect(field.value).toBe("/btw [@Planner](dispatch://agent/A) "));
    expect(screen.getByRole("list", { name: "Mention targets" })).toHaveTextContent("session:A");
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "[@Planner](dispatch://agent/A) ",
          delivery: "btw",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("deleting a seeded mention restores verbatim body and omits delivery", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({
    agents: [planner],
    seedMentions: [{ target: "session:A", title: "Planner" }],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(field.value).toBe("[@Planner](dispatch://agent/A) "));
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("/btw ");
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", { body: "/btw " }, keyedAttempt)
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A party named twice is one party: the text is read for who it names, so the second mention
// adds no second delivery and no second chip.
test("a draft naming one party twice names it once and sends one mode", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({
    agents: [planner],
    seedMentions: [{ target: "session:A", title: "Planner" }],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(field.value).toBe("[@Planner](dispatch://agent/A) "));
    act(() => field.focus());
    field.setSelectionRange(field.value.length, field.value.length);
    await user.paste("and [Planner](dispatch://agent/A) again");
    expect(
      within(screen.getByRole("list", { name: "Mention targets" })).getAllByText("session:A")
    ).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "[@Planner](dispatch://agent/A) and [Planner](dispatch://agent/A) again",
          delivery: "steer",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// The words of a mention are the reader's; its address is what names the party. A session that
// calls itself `session:Victim` therefore mentions nobody but itself.
test("a mention's address names its party whatever its words say", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({
    agents: [{ ...planner, title: "session:Victim" }],
    seedMentions: [{ target: "session:A", title: "session:Victim" }],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(field.value).toBe("[@session:Victim](dispatch://agent/A) "));
    expect(screen.getByRole("list", { name: "Mention targets" })).toHaveTextContent("session:A");
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "[@session:Victim](dispatch://agent/A) ",
          delivery: "steer",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("reopening the same reply restores its canonical prefills after cancel", async () => {
  const user = userEvent.setup();
  const ReplyHarness = () => {
    const [replyTo, setReplyTo] = useState<ReplyTarget | null>(null);
    const target: ReplyTarget = {
      actor: plannerAuthor,
      excerpt: "Question",
      id: "comment-1",
      mentions: [{ target: "role:planner", title: "Planner" }],
      parentKind: "comment",
    };
    return (
      <>
        <button onClick={() => setReplyTo(target)} type="button">
          Open reply
        </button>
        <MentionComposer
          box="conversation-composer"
          onCancelReply={() => setReplyTo(null)}
          onClose={() => {}}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
          replyTo={replyTo}
        />
      </>
    );
  };
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider
        client={
          new QueryClient({
            defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
          })
        }
      >
        <ReplyHarness />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(screen.getByRole("button", { name: "Open reply" }));
    await waitFor(() => expect(field.value).toBe("[@Planner](dispatch://role/planner) "));
    await user.click(screen.getByRole("button", { name: "Cancel reply" }));
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Cancel reply" })).not.toBeInTheDocument()
    );
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Draft after cancellation");
    await user.click(screen.getByRole("button", { name: "Open reply" }));
    await waitFor(() => expect(field.value).toBe("[@Planner](dispatch://role/planner) "));
  } finally {
    view.unmount();
  }
});

/** A composer at the issue's own address that keeps a reply for Planner's message `message-1`
 *  (`answered`), drawn while its host lists that message standing: `composer` draws it under
 *  another listing, `lines` reads its status lines and `offersUse` whether it offers Use it here. */
function renderKeptReply() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const sendKey: MutationKey = ["conversation-composer", "CORE-1"];
  const owner: ComposerOwner = { issueKey: "CORE-1", kind: "issue" };
  const answered: ReplyTarget = {
    actor: plannerAuthor,
    excerpt: "Status?",
    id: "message-1",
    parentKind: "message",
  };
  const kept = "Reply to the status";
  unsentDraftsOf(queryClient).keepAway(composerNameOf(sendKey), {
    draft: { body: kept, replacement: "" },
    owner,
    replyTo: answered,
  });
  const composer = (listed: ListedReplies) => (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          box="conversation-composer"
          listed={listed}
          mutationKey={sendKey}
          onClose={() => {}}
          onSent={() => {}}
          owner={owner}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const lines = () => screen.queryAllByRole("status").map((status) => status.textContent ?? "");
  const offersUse = () => screen.queryByRole("button", { name: "Use it here" }) !== null;
  const view = render(composer(new Map<string, ListedReply>([[answered.id, "standing"]])));
  return { answered, composer, kept, lines, offersUse, view };
}

// A draft the composer keeps for another reply draws the line naming that reply's Reply while the
// host lists the reply. Once the host lists it no longer, that Reply is gone, so the draft is
// drawn itself, with Discard my draft and, on the issue's own address, Use it here, which never
// writes over words of the reader's own in the field: it shows once the field is empty, as the
// line says.
test("a reply kept for a message its host no longer lists waits for an empty field before Use it here", async () => {
  const user = userEvent.setup();
  const { composer, kept, lines, offersUse, view } = renderKeptReply();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    const whileListed = { lines: lines(), use: offersUse() };
    await user.click(field);
    await user.paste("Words of my own");
    view.rerender(composer(new Map()));
    const overOwnWords = { lines: lines(), text: field.value, use: offersUse() };
    await user.clear(field);
    const overEmptyField = { lines: lines(), use: offersUse() };
    await user.click(screen.getByRole("button", { name: "Use it here" }));
    expect({
      overEmptyField,
      overOwnWords,
      taken: { lines: lines(), text: field.value, use: offersUse() },
      whileListed,
    }).toEqual({
      overEmptyField: {
        lines: [
          `The message by Planner that your reply answered is no longer listed. Your reply is kept here:${kept}`,
        ],
        use: true,
      },
      overOwnWords: {
        lines: [
          `The message by Planner that your reply answered is no longer listed. Your reply is kept here, to use in this field once it is empty:${kept}`,
        ],
        text: "Words of my own",
        use: false,
      },
      taken: { lines: [], text: kept, use: false },
      whileListed: {
        lines: [
          "Your reply to the message by Planner is kept. Press Reply on that message to go on with it.",
        ],
        use: false,
      },
    });
  } finally {
    view.unmount();
  }
});

/** Draws the composer `props` describe with `queryClient`'s store, where a row has kept a draft
 *  first, and answers what draws it again with other props on the live instance. */
function drawOverStore(queryClient: QueryClient, props: ComponentProps<typeof MentionComposer>) {
  const tree = (next: ComponentProps<typeof MentionComposer>) => (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer {...next} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(props));
  return {
    rerender: (next: ComponentProps<typeof MentionComposer>) => view.rerender(tree(next)),
    view,
  };
}

/** A composer drawn with an offer beside its field (`KeptDraftOffers`): its field, and what ends
 *  the row, unmounting the composer and putting back what the row replaced. */
interface OfferRow {
  readonly end: () => void;
  readonly field: HTMLTextAreaElement;
}

const OFFER_OWNER: ComposerOwner = { issueKey: "CORE-1", kind: "issue" };
const KEPT_FOR_OTHER_WORDS = "Half a thought";
const MY_EDIT = "Original comment, as I edited it";
const KEPT_REPLY = "Reply to the status";

/** A margin compose of `kind` at the words "brown", whose name keeps a draft written at the words
 *  "quick" of the same document: it offers that draft beside its empty field. On an issue that is
 *  `closed`, a suggestion's compose offers it with Discard my draft alone. */
async function offerKeptForOtherWords(
  kind: "comment" | "suggestion",
  closed = false
): Promise<OfferRow> {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const sendKey: MutationKey = ["margin-compose", "artifact-1"];
  unsentDraftsOf(queryClient).keepAway(composerNameOf(sendKey), {
    anchor: { artifact: "artifact-1", mark_id: "mark-1", quote: "quick" },
    draft: { body: KEPT_FOR_OTHER_WORDS, replacement: "" },
    owner: OFFER_OWNER,
    replyTo: null,
  });
  const { view } = drawOverStore(queryClient, {
    anchor: { artifact: "artifact-1", mark_id: "mark-2", quote: "brown" },
    box: "margin-composer",
    closed,
    kind,
    mutationKey: sendKey,
    onClose: () => {},
    onSent: () => {},
    owner: OFFER_OWNER,
  });
  await screen.findByText(/for other words: “quick”/);
  return {
    end: () => view.unmount(),
    field: screen.getByLabelText<HTMLTextAreaElement>(kind === "comment" ? "Comment" : "Reason"),
  };
}

/** An edit another writer changed the comment under: the field shows the comment as it reads now,
 *  and the reader's edit waits beside it. Its host keeps the editor drawn after its `onClose`. */
async function offerChangedEdit(user: UserEvent): Promise<OfferRow> {
  // The editor reads the comment's blocks when it opens: this comment carries none.
  const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue(alphaBetaBlocks(false));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const editOf = (body: string): ComponentProps<typeof MentionComposer> => ({
    box: "margin-comment-edit",
    edit: { body, id: "comment-1" },
    kind: "comment",
    mutationKey: ["comment-edit", "comment-1"],
    onClose: () => {},
    onSent: () => {},
    owner: OFFER_OWNER,
  });
  const { rerender, view } = drawOverStore(queryClient, editOf("Original comment"));
  const field = screen.getByLabelText<HTMLTextAreaElement>("Edit comment");
  await user.click(field);
  await user.keyboard("{Control>}a{/Control}");
  await user.paste(MY_EDIT);
  rerender(editOf("Changed by another writer"));
  await screen.findByText(/This comment changed since you wrote your edit/);
  return {
    end: () => {
      view.unmount();
      commentBlocks.mockRestore();
    },
    field,
  };
}

/** A composer at the issue's own address, whose name keeps a reply for a message its host lists
 *  no longer: it draws that reply beside its empty field, with Discard my draft and Use it here. */
async function offerGoneReply(): Promise<OfferRow> {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const sendKey: MutationKey = ["conversation-composer", "CORE-1"];
  unsentDraftsOf(queryClient).keepAway(composerNameOf(sendKey), {
    draft: { body: KEPT_REPLY, replacement: "" },
    owner: OFFER_OWNER,
    replyTo: { actor: plannerAuthor, excerpt: "Status?", id: "message-1", parentKind: "message" },
  });
  const { view } = drawOverStore(queryClient, {
    box: "conversation-composer",
    listed: new Map(),
    mutationKey: sendKey,
    onClose: () => {},
    onSent: () => {},
    owner: OFFER_OWNER,
  });
  await screen.findByText(/that your reply answered is no longer listed/);
  return {
    end: () => view.unmount(),
    field: screen.getByLabelText<HTMLTextAreaElement>("Comment"),
  };
}

// Each press of an offer beside the field (`KeptDraftOffers`) takes the offer away, and with it
// the button that held the focus. The field the offer stood beside takes the focus then, with no
// scroll, and the caret at the end of what the field holds: a draft taken in is ready to go on.
// Discard my edit is the composer's own Discard, which ends it (`onClose`); a host that keeps the
// editor drawn, as this one does, has its field take the focus the same way.
for (const row of [
  {
    draw: () => offerKeptForOtherWords("comment"),
    offer: "a draft kept for other words",
    press: "Discard my draft",
    text: "",
  },
  {
    draw: () => offerKeptForOtherWords("comment"),
    offer: "a draft kept for other words",
    press: "Use my draft",
    text: KEPT_FOR_OTHER_WORDS,
  },
  {
    draw: offerChangedEdit,
    offer: "an edit its comment changed under",
    press: "Discard my edit",
    text: "",
  },
  {
    draw: offerChangedEdit,
    offer: "an edit its comment changed under",
    press: "Use my edit",
    text: MY_EDIT,
  },
  {
    draw: offerGoneReply,
    offer: "a reply kept for a message no longer listed",
    press: "Discard my draft",
    text: "",
  },
  {
    draw: offerGoneReply,
    offer: "a reply kept for a message no longer listed",
    press: "Use it here",
    text: KEPT_REPLY,
  },
]) {
  test(`${row.press} on ${row.offer} hands the focus to the field, with no scroll and the caret at the end`, async () => {
    const user = userEvent.setup();
    const { end, field } = await row.draw(user);
    const focuses = spyOn(field, "focus");

    try {
      await user.click(screen.getByRole("button", { name: row.press }));
      expect({
        caret: [field.selectionStart, field.selectionEnd],
        focused: focusedName(),
        focuses: focuses.mock.calls,
        offered: screen.queryByRole("button", { name: row.press }) !== null,
        text: field.value,
      }).toEqual({
        caret: [row.text.length, row.text.length],
        focused: field.getAttribute("aria-label") ?? "",
        focuses: [[{ preventScroll: true }]],
        offered: false,
        text: row.text,
      });
    } finally {
      focuses.mockRestore();
      end();
    }
  });
}

// An edit its comment changed under holds the field beside it read only until the reader uses it
// or drops it, as a draft kept for other words holds a suggestion's Replace with as well as its
// Reason. A key typed there writes nothing: the field names the offer as its description, after
// the hint its row of the writing list gives every box (`DISPATCH_WRITING_BOXES`), so a screen
// reader says why it takes no text, and the key has the offer say what the field waits for,
// after the offer's own words, by the presses it draws: on a closed issue a suggestion's compose
// draws Discard my draft alone, and Use my draft only once the issue reopens.
for (const row of [
  {
    draw: offerChangedEdit,
    field: "Edit comment",
    hint: dispatchWritingBox("margin-comment-edit", "comment-edit").hint,
    offer: `This comment changed since you wrote your edit. The field shows the comment as it reads now. Your edit is kept here: ${MY_EDIT}`,
    text: "Changed by another writer",
    why: "Use or discard your kept edit first.",
  },
  {
    draw: () => offerKeptForOtherWords("suggestion"),
    field: "Replacement",
    hint: dispatchWritingBox("suggestion-replacement", "suggestion").hint,
    offer: `You wrote this draft for other words: “quick”. It is kept here: ${KEPT_FOR_OTHER_WORDS}`,
    text: "",
    why: "Use or discard your kept draft first.",
  },
  {
    draw: () => offerKeptForOtherWords("suggestion", true),
    field: "Reason",
    hint: dispatchWritingBox("margin-composer", "suggestion").hint,
    offer: `You wrote this draft for other words: “quick”. It is kept here: ${KEPT_FOR_OTHER_WORDS}`,
    text: "",
    why: "Reopen the issue to use your kept draft, or discard it.",
  },
]) {
  test(`a key typed in ${row.field} beside the offer that holds it read only says why, and the field's description names the offer`, async () => {
    const user = userEvent.setup();
    const { end } = await row.draw(user);

    try {
      const field = screen.getByLabelText<HTMLTextAreaElement>(row.field);
      expect({
        readOnly: field.getAttribute("aria-readonly"),
        why: screen.queryByText(row.why) !== null,
      }).toEqual({ readOnly: "true", why: false });
      expect(field).toHaveAccessibleDescription(`${row.hint} ${row.offer}`);

      await user.click(field);
      await user.keyboard("x");
      expect({ text: field.value, why: screen.queryByText(row.why) !== null }).toEqual({
        text: row.text,
        why: true,
      });
      expect(field).toHaveAccessibleDescription(`${row.hint} ${row.offer} ${row.why}`);
    } finally {
      end();
    }
  });
}

/** A draft as the build before this one kept it in the tab: its body holds only the cosmetic
 *  `@Planner` that build drew, and the party it names stands in a `mentions` array beside it. */
const olderDraft = {
  body: "@Planner please look",
  mentions: [{ end: 8, start: 0, target: "session:A", text: "Planner" }],
  replacement: "",
};

/** That draft as it is read back (`restoredDraft`): the mention that names the party. */
const OLDER_DRAFT_RESTORED = "[@Planner](dispatch://agent/A) please look";

// A tab can hold drafts the build before this one kept, in the store of a standing box's unsent
// words and in the one of the drafts a composer keeps by address. Each store reads such a draft
// back as a held send's is (`restoredDraft`), so its `@Planner` comes back as the mention that
// names the party, and the send built from it delivers to that party, where dropping the array
// would send it to nobody.
test("an unsent reply an older build kept with its mentions comes back naming them, and sends to them", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const sendKey: MutationKey = ["inbox-thread", "thread-1"];
  unsentDraftsOf(queryClient).write(composerNameOf(sendKey), olderDraft);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          box="inbox-thread-reply"
          inline
          kind="comment"
          mutationKey={sendKey}
          onClose={() => {}}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
          replyTo={{ actor: plannerAuthor, excerpt: "", id: "comment-9", parentKind: "comment" }}
          standing
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(screen.getByLabelText<HTMLTextAreaElement>("Reply").value).toBe(OLDER_DRAFT_RESTORED);
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        { body: OLDER_DRAFT_RESTORED, delivery: "steer", reply_to: "comment-9" },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a draft an older build kept for an address with its mentions comes back naming them, and sends to them", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const sendKey: MutationKey = ["conversation-composer", "CORE-1"];
  const owner: ComposerOwner = { issueKey: "CORE-1", kind: "issue" };
  unsentDraftsOf(queryClient).keepAway(composerNameOf(sendKey), {
    draft: olderDraft,
    owner,
    replyTo: null,
  });
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          box="conversation-composer"
          mutationKey={sendKey}
          onClose={() => {}}
          onSent={() => {}}
          owner={owner}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(screen.getByLabelText<HTMLTextAreaElement>("Comment").value).toBe(OLDER_DRAFT_RESTORED);
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        { body: OLDER_DRAFT_RESTORED, delivery: "steer" },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A message its author deleted keeps its place in the host's list, but Dispatch takes no reply to
// it (409 POST_DELETED), so a reply kept for it has no Reply to go on with: it is drawn itself,
// saying the message was deleted and giving no word of it - not the excerpt its Reply took - with
// Discard my draft and, at the issue's own address, Use it here, which puts it in the field as a
// message that answers none.
test("a reply kept for a message its host lists deleted is drawn itself, saying the message was deleted", async () => {
  const user = userEvent.setup();
  const { answered, composer, kept, lines, offersUse, view } = renderKeptReply();

  try {
    const whileStanding = { lines: lines(), use: offersUse() };
    view.rerender(composer(new Map<string, ListedReply>([[answered.id, "deleted"]])));
    const deleted = { lines: lines(), use: offersUse() };
    const shownWords = view.container.textContent ?? "";
    const use = screen.queryByRole("button", { name: "Use it here" });
    if (use !== null) await user.click(use);
    expect({
      deleted,
      excerptShown: shownWords.includes(answered.excerpt),
      taken: {
        lines: lines(),
        text: screen.getByLabelText<HTMLTextAreaElement>("Comment").value,
      },
      whileStanding,
    }).toEqual({
      deleted: {
        lines: [
          `The message by Planner that your reply answered was deleted, so it takes no reply. Your reply is kept here:${kept}`,
        ],
        use: true,
      },
      excerptShown: false,
      taken: { lines: [], text: kept },
      whileStanding: {
        lines: [
          "Your reply to the message by Planner is kept. Press Reply on that message to go on with it.",
        ],
        use: false,
      },
    });
  } finally {
    view.unmount();
  }
});

// Reply takes the post it answers as it stands at the press (`ReplyTarget`), and a held send or a
// kept draft keeps that copy in the tab across a reload. The quote gives the copy's words only
// while the host holds the post now (`replyParent`): until the host's read answers, or once it
// fails, it names the author alone, so a post deleted since never shows the words it had; once
// the read answers, the quote reads the post as it stands - standing, then deleted.
test("a reply's quote gives no word of its parent until the host holds the parent, then reads it as it stands", async () => {
  // The quote is read in the commit that mounts the composer, which only a loaded engine draws
  // (`MarkdownPreview`).
  await warmMarkdownRenderer();
  const replyTo = {
    actor: plannerAuthor,
    excerpt: "Once the build is green.",
    id: "comment-9",
    parentKind: "comment",
  } as const;
  const { rerender, view } = renderComposer({ replyTo });
  const quote = () => {
    const words = screen.getByRole("form", { name: "Comment composer" }).textContent ?? "";
    return {
      author: words.includes("Replying to Planner"),
      deleted: words.includes("Deleted by Planner"),
      excerpt: words.includes(replyTo.excerpt),
    };
  };

  try {
    const unread = quote();
    rerender({ replyParent: { deleted_at: null, deleted_by: null, edited_at: null }, replyTo });
    const standing = quote();
    rerender({
      replyParent: {
        deleted_at: "2026-09-18T01:00:00Z",
        deleted_by: plannerAuthor,
        edited_at: null,
      },
      replyTo,
    });
    const deleted = quote();
    expect({ deleted, standing, unread }).toEqual({
      deleted: { author: true, deleted: true, excerpt: false },
      standing: { author: true, deleted: false, excerpt: true },
      unread: { author: true, deleted: false, excerpt: false },
    });
  } finally {
    view.unmount();
  }
});

/** Send's refusal as a reader meets it: `aria-disabled`, and the reason on `title` and in the
 *  element `aria-describedby` names. */
interface SendRefusal {
  readonly description: string | null;
  readonly disabled: string | null;
  readonly title: string | null;
}

function sendRefusal(): SendRefusal {
  const send = screen.getByRole("button", { name: "Send" });
  const describedBy = send.getAttribute("aria-describedby");
  return {
    description:
      describedBy === null ? null : (document.getElementById(describedBy)?.textContent ?? null),
    disabled: send.getAttribute("aria-disabled"),
    title: send.getAttribute("title"),
  };
}

// A refusal the composer holds for a reply its host has moved it off stays with that reply, and
// Send says where it was written and the way out: that message's Reply, while it stands. A message
// its author deleted keeps its place, but Dispatch takes no reply to it, so Send names no Reply
// that would bring the refusal back, and says the message was deleted: Discard is the way out.
test("a refusal held for a reply to a message deleted since names no Reply to go back by", async () => {
  const user = userEvent.setup();
  const createMessage = spyOn(api, "createMessage").mockRejectedValue(
    new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused the message" })
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const owner: ComposerOwner = { issueKey: "CORE-1", kind: "issue" };
  const status: ReplyTarget = {
    actor: plannerAuthor,
    excerpt: "Status?",
    id: "message-1",
    parentKind: "message",
  };
  const done: ReplyTarget = { ...status, excerpt: "Done?", id: "message-2" };
  const composer = (replyTo: ReplyTarget, first: ListedReply) => (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          box="conversation-composer"
          listed={
            new Map<string, ListedReply>([
              [status.id, first],
              [done.id, "standing"],
            ])
          }
          mutationKey={["conversation-composer", "CORE-1"]}
          onClose={() => {}}
          onSent={() => {}}
          owner={owner}
          replyParent={{ deleted_at: null, deleted_by: null, edited_at: null }}
          replyTo={replyTo}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(composer(status, "standing"));

  try {
    await user.click(screen.getByLabelText("Comment"));
    await user.paste("Reply to the status");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText("Couldn't send — Dispatch refused the message");
    // The host moves the composer to another message's reply, as another message's Reply does.
    view.rerender(composer(done, "standing"));
    const standing = sendRefusal().title;
    view.rerender(composer(done, "deleted"));
    const deleted = sendRefusal().title;
    expect({ deleted, standing }).toEqual({
      deleted:
        "This message was written as a reply to a message that was deleted, which takes no reply. Discard it to write here.",
      standing:
        "This message was written as a reply. Reply to that message again to send it, or discard it.",
    });
  } finally {
    view.unmount();
    createMessage.mockRestore();
  }
});

/** Presses Send both ways a reader can - the button, and Ctrl+Enter in the box - then moves the
 *  fake clock a task on. TanStack awaits `onMutate` before it calls the mutation function, so an
 *  API spy read straight after the press shows no call whether or not Send refused. Each test
 *  that presses this way installs the fake clock before it renders. */
async function pressSend(user: UserEvent, field: HTMLElement): Promise<void> {
  await user.click(screen.getByRole("button", { name: "Send" }));
  act(() => field.focus());
  await user.keyboard("{Control>}{Enter}{/Control}");
  await tick(20);
}

test("a token-only direct-session draft cannot submit an empty message, and Send says why", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue(createdMessage);
  const { view } = renderComposer({ owner: { kind: "session", sessionId: "A" } });

  try {
    const field = screen.getByLabelText("Comment");
    for (const command of ["/btw", "/aside"]) {
      await user.click(field);
      await user.keyboard("{Control>}a{/Control}");
      await user.paste(`${command} `);
      const reason = `Type the message after ${command}.`;
      expect(sendRefusal()).toEqual({ description: reason, disabled: "true", title: reason });
      await pressSend(user, field);
      expect(createAgentMessage).not.toHaveBeenCalled();
      expect(screen.queryByRole("button", { name: "Sending…" })).not.toBeInTheDocument();
    }
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("/btw status?");
    expect(sendRefusal()).toEqual({ description: null, disabled: null, title: null });
  } finally {
    realClock();
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

test("a direct-session reply inherits its thread delivery", async () => {
  const user = userEvent.setup();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue(createdMessage);
  const { view } = renderComposer({
    owner: { kind: "session", sessionId: "A" },
    replyTo: {
      actor: plannerAuthor,
      excerpt: "Question",
      id: "message-9",
      thread: { delivery: "btw", target: "session:A" },
    },
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Follow up");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createAgentMessage).toHaveBeenCalledWith(
        "A",
        {
          body: "Follow up",
          delivery: "btw",
          in_reply_to: "message-9",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

test("a legacy targeted-message reply keeps the message endpoint while a comment reply stays a comment", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({
    replyTo: {
      actor: plannerAuthor,
      excerpt: "Question",
      id: "message-9",
      parentKind: "message",
      thread: { delivery: "btw", target: "session:A" },
    },
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Legacy reply");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createMessage).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "Legacy reply",
          delivery: "btw",
          in_reply_to: "message-9",
          target: "session:A",
        },
        keyedAttempt
      )
    );
    expect(createComment).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    createComment.mockRestore();
    createMessage.mockRestore();
  }
});

test("a command overrides a targeted legacy reply's inherited delivery", async () => {
  const user = userEvent.setup();
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({
    replyTo: {
      actor: plannerAuthor,
      excerpt: "Question",
      id: "message-9",
      parentKind: "message",
      thread: { delivery: "btw", target: "session:A" },
    },
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("/aside hello");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createMessage).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "hello",
          delivery: "aside",
          in_reply_to: "message-9",
          target: "session:A",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createMessage.mockRestore();
  }
});

test("a command on a plain legacy reply stays verbatim and sends no delivery", async () => {
  const user = userEvent.setup();
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({
    replyTo: {
      actor: plannerAuthor,
      excerpt: "Question",
      id: "message-9",
      parentKind: "message",
    },
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("/btw hello");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createMessage).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "/btw hello",
          in_reply_to: "message-9",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createMessage.mockRestore();
  }
});

test("a token-only targeted legacy reply cannot submit, and Send says why", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const createMessage = spyOn(api, "createMessage").mockResolvedValue({} as never);
  const { view } = renderComposer({
    replyTo: {
      actor: plannerAuthor,
      excerpt: "Question",
      id: "message-9",
      parentKind: "message",
      thread: { delivery: "btw", target: "session:A" },
    },
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("/btw ");
    const reason = "Type the message after /btw.";
    expect(sendRefusal()).toEqual({ description: reason, disabled: "true", title: reason });
    await pressSend(user, field);
    expect(createMessage).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Sending…" })).not.toBeInTheDocument();
  } finally {
    realClock();
    view.unmount();
    createMessage.mockRestore();
  }
});

// A mention is a reference, so it survives an edit of the words around it: the reader types in
// front of it, between the two, and after them, and both parties are still named.
test("every mention in the draft survives the edits around it", async () => {
  const planners = "[@Planner](dispatch://agent/A)";
  const workers = "[@Worker](dispatch://agent/B)";
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({
    agents: [planner, worker],
    seedMentions: [
      { target: "session:A", title: "Planner" },
      { target: "session:B", title: "Worker" },
    ],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(field.value).toBe(`${planners} ${workers} `));
    act(() => field.focus());
    field.setSelectionRange(0, 0);
    await user.paste("/btw ");
    await waitFor(() => expect(field.value).toBe(`/btw ${planners} ${workers} `));
    field.setSelectionRange(`/btw ${planners}`.length, `/btw ${planners}`.length);
    await user.paste(" x");
    await waitFor(() => expect(field.value).toBe(`/btw ${planners} x ${workers} `));
    // An address written into the draft is an edit like any other.
    field.setSelectionRange(field.value.length, field.value.length);
    await user.paste("dispatch://CORE-1");
    await waitFor(() =>
      expect(field.value).toBe(`/btw ${planners} x ${workers} dispatch://CORE-1`)
    );
    // The ` x` goes, and both mentions are still the text's own.
    await user.click(field);
    field.setSelectionRange(`/btw ${planners}`.length, `/btw ${planners} x`.length);
    await user.keyboard("{Backspace}");
    await waitFor(() => expect(field.value).toBe(`/btw ${planners} ${workers} dispatch://CORE-1`));
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: `${planners} ${workers} dispatch://CORE-1`,
          delivery: "btw",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// The registry is read again the moment the picker opens, inside the shell poller's 15 s, so a
// session started since the last poll is offered to the reader asking for it now.
test("opening the picker refetches the live agents", async () => {
  const user = userEvent.setup();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner, worker]);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["agents"], [planner]);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          box="conversation-composer"
          onClose={() => {}}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("@");
    await screen.findByTestId("ref-picker");
    await waitFor(() => expect(listAgents).toHaveBeenCalled());
  } finally {
    view.unmount();
    listAgents.mockRestore();
  }
});

test("a mention survives its target going offline before Send", async () => {
  const user = userEvent.setup();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["agents"], [planner]);
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          box="conversation-composer"
          onClose={() => {}}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
          seedMentions={[{ target: "session:A", title: "Planner" }]}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(field.value).toBe("[@Planner](dispatch://agent/A) "));
    act(() => queryClient.setQueryData(["agents"], []));
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "[@Planner](dispatch://agent/A) ",
          delivery: "steer",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    listAgents.mockRestore();
    createComment.mockRestore();
  }
});

/** One collaborators answer in the order the server sends it (the contract's: the registry's
 *  agents by role in workflow order and then newest first, then the rest newest first) - the
 *  planner, the reviewer role's newest session and the one it replaced, then a session with no
 *  registry entry. */
const collaboratorsAnswer: IssueCollaborators = {
  agents: [
    {
      kind: "session",
      last_at: null,
      legion: {
        replaced_at: null,
        replaced_by: null,
        role: "planner",
        role_token: "legion-planner",
        started_at: "2026-10-26T00:00:00Z",
      },
      reasons: [],
      session_id: "old-planner",
      title: "Old planner",
    },
    {
      kind: "session",
      last_at: null,
      legion: {
        replaced_at: null,
        replaced_by: null,
        role: "reviewer",
        role_token: "legion-p-core-1-reviewer",
        started_at: "2026-10-26T02:00:00Z",
      },
      reasons: [],
      session_id: "new-reviewer",
      title: "New reviewer",
    },
    {
      kind: "session",
      last_at: null,
      legion: {
        replaced_at: "2026-10-26T02:00:00Z",
        replaced_by: "new-reviewer",
        role: "reviewer",
        role_token: "legion-p-core-1-reviewer",
        started_at: "2026-10-26T01:00:00Z",
      },
      reasons: [],
      session_id: "old-reviewer",
      title: "Old reviewer",
    },
    {
      kind: "session",
      last_at: "2026-10-26T03:00:00Z",
      legion: null,
      reasons: ["posted"],
      session_id: "helper",
      title: "Helper",
    },
  ],
  issue_key: "CORE-1",
  legion_registry: "ok",
  people: [],
};

/** The naming the picker does outside React: no stopped session is in the registry cache, so each
 *  name falls back to the title the collaborators read stamped on it. */
const storedNames: Pick<AgentText, "sessionText"> = {
  sessionText: (_sessionId, storedTitle) => storedTitle ?? "",
};

// An ask option's Enter adds the next option, and the picker's Enter takes the row under it:
// the field's own listener is in front of the box's handler, so one key cannot do both.
test("Enter in an ask option takes the picker's row, and adds an option once it is closed", async () => {
  const user = userEvent.setup();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const { view } = renderComposer({ agents: [planner], kind: "ask" });

  try {
    const label = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Option 1 label" });
    await user.click(label);
    await user.paste("@");
    await screen.findByTestId("ref-picker");
    await screen.findByRole("option", { name: /^Planner\b/ });
    // The row Enter takes with no arrow pressed is the first the list drew, the live agent:
    // this issue's collaborators name no person.
    await user.keyboard("{Enter}");
    await waitFor(() => expect(label.value).toBe("[@Planner](dispatch://agent/A)"));
    // The option row the picker's Enter did not add.
    expect(screen.queryByRole("textbox", { name: "Option 2 label" })).toBeNull();

    // With the list closed the same key is the box's own, as it always was.
    await user.keyboard("{Enter}");
    await screen.findByRole("textbox", { name: "Option 2 label" });
  } finally {
    view.unmount();
    listAgents.mockRestore();
  }
});

// The rule the "Not running" options follow is the renderer's (`role-holders.ts`): a role a live
// agent holds is the live options' (never offered twice), every other role the collaborators
// record is offered, once, by its newest session whatever replaced it - the replaced session is
// never an option of its own. A live list whose state is unknown offers no stopped role at all,
// which `useRefPicker` holds to where it builds the picker's party rows.
test("stopped role options leave out held roles and name each role once", () => {
  const answerAgents = collaboratorsAnswer.agents ?? [];
  const reviewerOnly = stoppedRoleOptions([planner], answerAgents, storedNames);
  expect(reviewerOnly.map((option) => [option.target, option.sessionId, option.detail])).toEqual([
    ["role:legion-p-core-1-reviewer", "new-reviewer", "reviewer · not running, not told"],
  ]);
  // The holder the filter matches and the detail tooltip names: that session's own name.
  expect(reviewerOnly.map((option) => option.holder)).toEqual(["New reviewer"]);
  // Nobody live holds either role: both, in the order the server sent them (planner, reviewer).
  expect(
    stoppedRoleOptions([worker], answerAgents, storedNames).map((option) => option.target)
  ).toEqual(["role:legion-planner", "role:legion-p-core-1-reviewer"]);
  // A role whose newest session the live list names but which claims no role is held by nobody,
  // so it is offered as not running, by that session, as the renderer links it.
  const reviewerLive = stoppedRoleOptions(
    [{ ...worker, session_id: "new-reviewer" }],
    answerAgents,
    storedNames
  );
  expect(reviewerLive.map((option) => [option.target, option.sessionId])).toEqual([
    ["role:legion-planner", "old-planner"],
    ["role:legion-p-core-1-reviewer", "new-reviewer"],
  ]);
});

// A token the answer records only by replaced sessions - its newest one gone from the registry's
// record - is offered where the answer names it, between the tokens around it, not after all of
// them: the "Not running" rows keep the server's order.
test("a role every session of which was replaced keeps its place among the stopped roles", () => {
  const entry = (session: string, token: string, replacedBy: string | null) => ({
    kind: "session" as const,
    last_at: null,
    legion: {
      replaced_at: replacedBy === null ? null : "2026-10-26T02:00:00Z",
      replaced_by: replacedBy,
      role: token,
      role_token: token,
      started_at: "2026-10-26T00:00:00Z",
    },
    reasons: [],
    session_id: session,
    title: session,
  });
  const agents = [
    entry("planner-session", "legion-planner", null),
    entry("old-tester", "legion-tester", "gone-tester"),
    entry("reviewer-session", "legion-reviewer", null),
  ];
  expect(
    stoppedRoleOptions([], agents, storedNames).map((option) => [option.target, option.sessionId])
  ).toEqual([
    ["role:legion-planner", "planner-session"],
    ["role:legion-tester", "old-tester"],
    ["role:legion-reviewer", "reviewer-session"],
  ]);
});

// The roles the picker offers, live and not running, are the roles the renderer links
// (`role-holders.ts`), whichever sessions run: the two can never differ, so a role offered is
// one its mention links and a role its mention reads plain is never offered.
test("the roles offered are the roles a reader is shown as known", () => {
  const answerAgents = collaboratorsAnswer.agents ?? [];
  for (const live of [[planner], [worker], [{ ...worker, session_id: "new-reviewer" }], []]) {
    const offered = [
      ...mentionOptions(live, storedNames),
      ...stoppedRoleOptions(live, answerAgents, storedNames),
    ].flatMap((option) =>
      option.target.startsWith("role:") ? [option.target.slice("role:".length)] : []
    );
    expect(offered).toHaveLength(new Set(offered).size);
    expect(new Set(offered)).toEqual(
      new Set([...roleHolders(live).keys(), ...recordedRoles(answerAgents).keys()])
    );
  }
});

test("two seeded mentions receive one comment-level delivery mode", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({
    agents: [planner],
    seedMentions: [
      { target: "session:A", title: "Planner" },
      { target: "role:legion-planner", title: "legion-planner" },
    ],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() =>
      expect(field.value).toBe(
        "[@Planner](dispatch://agent/A) [@legion-planner](dispatch://role/legion-planner) "
      )
    );
    act(() => field.focus());
    field.setSelectionRange(0, 0);
    await user.paste("/aside ");
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "[@Planner](dispatch://agent/A) [@legion-planner](dispatch://role/legion-planner) ",
          delivery: "aside",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("/btw in the body and /btwx stay ordinary text despite a mention", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const seedMentions = [{ target: "session:A", title: "Planner" }];
  const { view } = renderComposer({ agents: [planner], seedMentions });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(field.value).toBe("[@Planner](dispatch://agent/A) "));
    act(() => field.focus());
    field.setSelectionRange(0, 0);
    await user.paste("hello /btw ");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenLastCalledWith(
        "CORE-1",
        {
          body: "hello /btw [@Planner](dispatch://agent/A) ",
          delivery: "steer",
        },
        keyedAttempt
      )
    );
    view.unmount();

    const second = renderComposer({ agents: [planner], seedMentions });
    const secondField = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(secondField.value).toBe("[@Planner](dispatch://agent/A) "));
    act(() => {
      secondField.focus();
    });
    secondField.setSelectionRange(0, 0);
    await user.paste("/btwx ");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenLastCalledWith(
        "CORE-1",
        {
          body: "/btwx [@Planner](dispatch://agent/A) ",
          delivery: "steer",
        },
        keyedAttempt
      )
    );
    second.view.unmount();
  } finally {
    createComment.mockRestore();
  }
});

test("editing a comment patches only its body and never creates a delivery", async () => {
  const user = userEvent.setup();
  const editComment = spyOn(api, "editComment").mockResolvedValue(createdComment);
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  // The editor reads the comment's blocks when it opens: this comment carries none.
  const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue(alphaBetaBlocks(false));
  const { view } = renderComposer({ edit: { body: "Original", id: "comment-1" } });

  try {
    const field = screen.getByLabelText("Edit comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("/btw @Planner");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith("comment-1", { body: "/btw @Planner" }, keyedAttempt)
    );
    expect(createComment).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    commentBlocks.mockRestore();
    editComment.mockRestore();
    createComment.mockRestore();
  }
});

/** The blocks route's answer for a two-paragraph comment, `Alpha` and `Beta`. */
function alphaBetaBlocks(indexed: boolean): ItemBlocks {
  const blocks: ItemBlock[] = [
    { embedding: "embedded", from: 0, id: "b1", text: "Alpha", to: 5, type: "paragraph" },
    { embedding: "embedded", from: 7, id: "b2", text: "Beta", to: 11, type: "paragraph" },
  ];
  return {
    blocks: indexed ? blocks : [],
    indexed,
    item: graphNode({ id: "comment-1", issue_key: "CORE-1", kind: "comment" }),
    revision: 3,
  };
}

/** Opens the edit of `Alpha\n\nBeta` once the blocks route answers `blocks`, types a word at
 *  the end of `Beta`, saves, and hands back what the save sent: the comment id and the input. */
async function saveEditOfAlphaBeta(blocks: Promise<ItemBlocks>): Promise<unknown[]> {
  const user = userEvent.setup();
  const commentBlocks = spyOn(api, "commentBlocks").mockReturnValue(blocks);
  const editComment = spyOn(api, "editComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ edit: { body: "Alpha\n\nBeta", id: "comment-1" } });
  try {
    await waitFor(() => expect(commentBlocks).toHaveBeenCalledWith("comment-1"));
    await act(async () => {
      await blocks.catch(() => {});
    });
    // user-event has no form for this: its own `beforeinput` would replace the one the field
    // reads to follow each block through the edit, which is what the save carries.
    fireEvent.change(screen.getByLabelText("Edit comment"), {
      target: { value: "Alpha\n\nBeta gamma  " },
    });
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(editComment).toHaveBeenCalledTimes(1));
    return [...(editComment.mock.calls[0] ?? [])];
  } finally {
    view.unmount();
    commentBlocks.mockRestore();
    editComment.mockRestore();
  }
}

test("an edit's save carries each block's id and the revision the blocks route answered", async () => {
  expect(await saveEditOfAlphaBeta(Promise.resolve(alphaBetaBlocks(true)))).toEqual([
    "comment-1",
    {
      base_revision: 3,
      blocks: [
        { from: 0, id: "b1" },
        { from: 7, id: "b2" },
      ],
      body: "Alpha\n\nBeta gamma",
    },
    keyedAttempt,
  ]);
});

test("an edit's save carries no block ids while the comment is not indexed or the route fails", async () => {
  expect(await saveEditOfAlphaBeta(Promise.resolve(alphaBetaBlocks(false)))).toEqual([
    "comment-1",
    { body: "Alpha\n\nBeta gamma" },
    keyedAttempt,
  ]);
  // An older server has no blocks route.
  expect(
    await saveEditOfAlphaBeta(
      Promise.reject(new ApiError(404, { code: "NOT_FOUND", error: "not found" }))
    )
  ).toEqual(["comment-1", { body: "Alpha\n\nBeta gamma" }, keyedAttempt]);
});

// A browser's textarea holds LF where the value it was given had CR LF, as the followed spans do,
// while the composer's own draft keeps the comment's stored text. A save made without typing
// works out its ids from the body it sends, the field's text, so it carries them.
test("an unchanged edit of a comment stored with CR LF carries each block's id", async () => {
  const user = userEvent.setup();
  const descriptor = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value");
  if (descriptor?.get === undefined || descriptor.set === undefined) {
    throw new Error("expected HTMLTextAreaElement.prototype.value to be an accessor");
  }
  const { get, set } = descriptor;
  Object.defineProperty(HTMLTextAreaElement.prototype, "value", {
    ...descriptor,
    get(this: HTMLTextAreaElement) {
      return (get.call(this) as string).replaceAll("\r\n", "\n");
    },
    set(this: HTMLTextAreaElement, value: string) {
      set.call(this, value);
    },
  });
  const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue({
    blocks: [
      { embedding: "embedded", from: 0, id: "b1", text: "Alpha", to: 5, type: "paragraph" },
      { embedding: "embedded", from: 9, id: "b2", text: "Beta", to: 13, type: "paragraph" },
    ],
    indexed: true,
    item: graphNode({ id: "comment-1", issue_key: "CORE-1", kind: "comment" }),
    revision: 3,
  });
  const editComment = spyOn(api, "editComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ edit: { body: "Alpha\r\n\r\nBeta", id: "comment-1" } });
  try {
    await waitFor(() => expect(commentBlocks).toHaveBeenCalledWith("comment-1"));
    await act(async () => {});
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(editComment).toHaveBeenCalledTimes(1));
    expect(editComment.mock.calls[0]).toEqual([
      "comment-1",
      {
        base_revision: 3,
        blocks: [
          { from: 0, id: "b1" },
          { from: 7, id: "b2" },
        ],
        body: "Alpha\n\nBeta",
      },
      keyedAttempt,
    ]);
  } finally {
    view.unmount();
    Object.defineProperty(HTMLTextAreaElement.prototype, "value", descriptor);
    commentBlocks.mockRestore();
    editComment.mockRestore();
  }
});

test("an edit's save an older server refuses keeps the draft", async () => {
  const user = userEvent.setup();
  const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue(alphaBetaBlocks(true));
  const editComment = spyOn(api, "editComment").mockRejectedValueOnce(
    new ApiError(400, { code: "INVALID_JSON", error: 'json: unknown field "blocks"' })
  );
  const { view } = renderComposer({ edit: { body: "Alpha\n\nBeta", id: "comment-1" } });

  try {
    await waitFor(() => expect(commentBlocks).toHaveBeenCalledWith("comment-1"));
    await act(async () => {});
    const field = screen.getByLabelText<HTMLTextAreaElement>("Edit comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Alpha\n\nBeta gamma");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toHaveTextContent('unknown field "blocks"');
    expect(field.value).toBe("Alpha\n\nBeta gamma");
    expect(editComment).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    commentBlocks.mockRestore();
    editComment.mockRestore();
  }
});

// The field reads each edit's type from the browser's own `beforeinput`, which a cut and a paste
// fire and React's `onBeforeInput` (the `textInput` event) never carries.
test("a block cut and pasted elsewhere keeps its id in the edit's save", async () => {
  const user = userEvent.setup();
  const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue(alphaBetaBlocks(true));
  const editComment = spyOn(api, "editComment").mockResolvedValue(createdComment);
  const { view } = renderComposer({ edit: { body: "Alpha\n\nBeta", id: "comment-1" } });
  const beforeInput = (field: HTMLTextAreaElement, start: number, end: number, type: string) => {
    field.setSelectionRange(start, end);
    field.dispatchEvent(
      new InputEvent("beforeinput", { bubbles: true, cancelable: true, inputType: type })
    );
  };

  try {
    await waitFor(() => expect(commentBlocks).toHaveBeenCalledWith("comment-1"));
    await act(async () => {});
    const field = screen.getByLabelText<HTMLTextAreaElement>("Edit comment");
    // Cut `Alpha` with the blank line after it, then paste it after `Beta`.
    beforeInput(field, 0, 7, "deleteByCut");
    // user-event's own `beforeinput` would replace the cut above, which the save follows.
    fireEvent.change(field, { target: { value: "Beta" } });
    beforeInput(field, 4, 4, "insertFromPaste");
    // user-event's own `beforeinput` would replace the paste above, which the save follows.
    fireEvent.change(field, { target: { value: "Beta\n\nAlpha" } });
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(editComment).toHaveBeenCalledTimes(1));
    expect(editComment.mock.calls[0]).toEqual([
      "comment-1",
      {
        base_revision: 3,
        blocks: [
          { from: 0, id: "b2" },
          { from: 6, id: "b1" },
        ],
        body: "Beta\n\nAlpha",
      },
      keyedAttempt,
    ]);
  } finally {
    view.unmount();
    commentBlocks.mockRestore();
    editComment.mockRestore();
  }
});

test("a failed send keeps its draft and retries", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment")
    .mockRejectedValueOnce(new ApiError(400, { error: "boom" }))
    .mockResolvedValueOnce(createdComment);
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("keep me");
    act(() => field.focus());
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't send — boom");
    expect(field.value).toBe("keep me");
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(field.value).toBe(""));
    expect(createComment).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("Ctrl+Enter sends while Enter, whitespace, and IME composition leave the draft intact", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("   ");
    act(() => field.focus());
    await user.keyboard("{Control>}{Enter}{/Control}");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("line one");
    await user.keyboard("{Enter}");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("still composing");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(field, { isComposing: true, key: "Enter" });
    expect(createComment).not.toHaveBeenCalled();
    act(() => field.focus());
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// The hint under an issue's comment box names its send key as the reader's platform draws it
// (`keyText`): `$mod+Enter` sends. Away from a Mac `$mod` is Ctrl; `key-text.test.ts` reads the
// same key as a Mac draws it.
test("away from a Mac the comment box's hint says Ctrl+Enter sends", async () => {
  const user = userEvent.setup();
  const { view } = renderComposer();
  try {
    expect(IS_MAC, "the unit tests' browser names a Mac").toBe(false);
    await user.click(screen.getByLabelText("Comment"));
    await user.paste("hello");
    expect(screen.getByText(/to send · Enter for a new line$/).textContent).toBe(
      "Ctrl+Enter to send · Enter for a new line"
    );
  } finally {
    view.unmount();
  }
});

// Safari sends the Enter that ends an input method's composition with `isComposing` false and
// keyCode 229. A Ctrl+Enter so marked is the input method's, and sends nothing; a plain Ctrl+Enter
// after it sends.
test("a Ctrl+Enter that ends an IME composition in Safari, at keyCode 229, sends nothing", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("composed");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(field, { ctrlKey: true, isComposing: false, key: "Enter", keyCode: 229 });
    // A send's mutation function runs a task after the press (`pressSend`).
    await tick(20);
    expect(createComment).not.toHaveBeenCalled();

    await user.keyboard("{Control>}{Enter}{/Control}");
    await tick(20);
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    view.unmount();
    createComment.mockRestore();
  }
});

// A key an input method takes - one marked `isComposing`, or the one that ends a composition - is
// the input method's. An Enter in an ask option's label adds no option then, and a plain Enter
// still does.
test.each(
  INPUT_METHOD_MARKS
)("an Enter that ends an IME composition in an ask option's label, marked by %s, adds no option", async (_mark, composing) => {
  const user = userEvent.setup();
  const { view } = renderComposer({ kind: "ask" });

  try {
    const label = screen.getByLabelText<HTMLTextAreaElement>("Option 1 label");
    await user.click(label);
    await user.keyboard("Yes");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(label, { ...composing, key: "Enter" });
    expect(screen.queryAllByLabelText(/^Option \d+ label$/)).toHaveLength(1);

    await user.keyboard("{Enter}");
    expect(screen.queryAllByLabelText(/^Option \d+ label$/)).toHaveLength(2);
    expect(focusedName()).toBe("Option 2 label");
  } finally {
    view.unmount();
  }
});

// The same for Escape: one that ends an input method's composition raises no `Discard draft?`
// over the draft, and a plain Escape still does.
test.each(
  INPUT_METHOD_MARKS
)("an Escape that ends an IME composition in the box, marked by %s, asks nothing", async (_mark, composing) => {
  const user = userEvent.setup();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("a draft");
    // user-event has no form for this: it cannot mark a key as an IME composition.
    fireEvent.keyDown(field, { ...composing, key: "Escape" });
    expect(screen.queryByRole("button", { name: "Discard" })).not.toBeInTheDocument();

    await user.keyboard("{Escape}");
    expect(screen.getByRole("button", { name: "Discard" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// Each Escape the comment box takes is marked taken (`defaultPrevented`) beside what it does: the
// reference picker's close, on the field's own listener (`MentionField`), and the
// `Discard draft?` over a draft. The mark lets the key registry (`keymap.ts`) and the rest of the
// page pass over the press.
test("the Escape that closes the reference picker, or asks Discard draft?, is marked taken", async () => {
  const user = userEvent.setup();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const { view } = renderComposer({ agents: [planner, worker] });
  const escapes = recordEscapes();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("a draft @");
    await screen.findByTestId("ref-picker");
    await user.keyboard("{Escape}");
    expect(screen.queryByTestId("ref-picker"), "the picker").toBeNull();
    expect(screen.queryByRole("button", { name: "Discard" }), "the picker's Escape").toBeNull();

    await user.keyboard("{Escape}");
    expect(screen.getByRole("button", { name: "Discard" }), "Discard draft?").toBeTruthy();
    expect(field).toHaveValue("a draft @");
    expect(escapes.presses.map((press) => press.defaultPrevented)).toEqual([true, true]);
  } finally {
    escapes.stop();
    view.unmount();
    listAgents.mockRestore();
  }
});

test("reply mode shows the quote, sends reply_to, and cancels by chip or Escape", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const user = userEvent.setup();
  let cancelled = 0;
  const { view } = renderComposer({
    onCancelReply: () => {
      cancelled += 1;
    },
    // The host holds the post the reply answers, standing: the quote gives its words only then.
    replyParent: { deleted_at: null, deleted_by: null, edited_at: null },
    replyTo: {
      actor: plannerAuthor,
      excerpt: "Once the build is green.",
      id: "comment-9",
      parentKind: "comment",
      to: "/issues/CORE-1/comments/comment-9",
    },
  });

  try {
    // The author is drawn outside the quote's link, which holds the quoted body.
    expect(screen.getByText("Replying to Planner").closest("a")).toBeNull();
    expect(screen.getByRole("link", { name: /^— Once the build is green\./ })).toBeDefined();
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Ship it.");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "Ship it.",
          reply_to: "comment-9",
        },
        keyedAttempt
      )
    );
    await user.click(screen.getByRole("button", { name: "Cancel reply" }));
    act(() => field.focus());
    await user.keyboard("{Escape}");
    expect(cancelled).toBe(2);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// The chip quotes a stored parent, whose host read it with its tree: the chip draws that tree
// (`excerptTree`) rather than a parse of the parent's markdown. The tree reads differently from
// the markdown beside it, so a chip drawn from a parse would read `— Once the build is green.`.
test("the Replying-to chip draws its parent from the tree the reply's host read", async () => {
  const { view } = renderComposer({
    // The host has read the parent: until it has, the chip draws none of its words (#3512).
    replyParent: { deleted_at: null, deleted_by: null, edited_at: null },
    replyTo: {
      actor: plannerAuthor,
      excerpt: "Once the **build** is green.",
      excerptTree: displayTree("Once the **build** is green, from the tree."),
      id: "comment-9",
      parentKind: "comment",
      to: "/issues/CORE-1/comments/comment-9",
    },
  });
  try {
    const quote = await screen.findByRole("link", {
      name: /^— Once the build is green, from the tree\./,
    });
    expect(quote.querySelector("strong")?.textContent).toBe("build");
  } finally {
    view.unmount();
  }
});

test("a direct message to a session that advertises no mode warns that it takes none", async () => {
  const user = userEvent.setup();
  const { view } = renderComposer({ agents: [worker], owner: { kind: "session", sessionId: "B" } });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Ship it.");
    const warning = screen.getByText(/Worker advertises no delivery mode, so sending it records/);
    expect(warning).toHaveTextContent("while its plugin cannot reach its conversation");
    expect(warning.textContent).not.toContain("None of the capabilities");
    expect(warning.textContent).not.toContain("Prefix with");
  } finally {
    view.unmount();
  }
});

// A session listing only capabilities that are no delivery mode takes none, as one listing none
// does: it gets the warning that it takes no message, not one naming Send as the mode it lacks.
test("a direct message to a session whose capabilities hold no delivery mode warns that it takes none", async () => {
  const user = userEvent.setup();
  const session: Agent = { ...worker, capabilities: ["future-capability"] };
  const { view } = renderComposer({
    agents: [session],
    owner: { kind: "session", sessionId: "B" },
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Ship it.");
    expect(
      screen.getByText(/Worker advertises no delivery mode, so sending it records/).textContent
    ).toBe(
      "Worker advertises no delivery mode, so sending it records a failed attempt. None of the capabilities it lists is a delivery mode this Dispatch knows."
    );
    expect(screen.queryByText(/while its plugin cannot reach/)).not.toBeInTheDocument();
    expect(screen.queryByText(/does not advertise Send/)).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// A session that refuses Send but takes Aside gets a plain message as an Aside, and the composer
// says so instead of warning that the Send would fail.
for (const capabilities of [["aside"], ["aside", "btw"]]) {
  test(`a plain message to a session advertising ${capabilities.join(" and ")} goes as an Aside`, async () => {
    const user = userEvent.setup();
    const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue(createdMessage);
    const session: Agent = { ...worker, capabilities: [...capabilities] };
    const { view } = renderComposer({
      agents: [session],
      owner: { kind: "session", sessionId: "B" },
    });

    try {
      const field = screen.getByLabelText("Comment");
      await user.click(field);
      await user.keyboard("{Control>}a{/Control}");
      await user.paste("Ship it.");
      expect(screen.getByText("Sending as Aside: Worker does not advertise Send.")).toBeTruthy();
      expect(screen.queryByText(/so sending it records a failed attempt/)).not.toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: "Send" }));
      await waitFor(() =>
        expect(createAgentMessage).toHaveBeenCalledWith(
          "B",
          { body: "Ship it.", delivery: "aside" },
          keyedAttempt
        )
      );
    } finally {
      view.unmount();
      createAgentMessage.mockRestore();
    }
  });
}

// BTW is never picked for the reader: a session that takes only BTW is still warned, and the
// warning's way out names only a mode the session takes.
test("when a session takes only BTW, a plain message warns and offers /btw", async () => {
  const user = userEvent.setup();
  const session: Agent = { ...worker, capabilities: ["btw"] };
  const { view } = renderComposer({
    agents: [session],
    owner: { kind: "session", sessionId: "B" },
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Ship it.");
    expect(
      screen.getByText(/Worker does not advertise Send, so sending it records/)
    ).toHaveTextContent("Prefix with /btw to send it as a BTW instead.");
  } finally {
    view.unmount();
  }
});

test("a mention of a session that takes only Aside goes as an Aside", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const user = userEvent.setup();
  const session: Agent = { ...planner, capabilities: ["aside"] };
  const { view } = renderComposer({
    agents: [session],
    seedMentions: [{ target: "session:A", title: "Planner" }],
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.paste(" hello");
    expect(screen.getByText("Sending as Aside: Planner does not advertise Send.")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        expect.objectContaining({ delivery: "aside" }),
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// Only a send to one session keeps its refused request for an unedited Retry, since its key must
// survive. A comment carries no key, so its Retry is sent as the composer shows it then: a
// mentioned session that stopped advertising Send after the refusal gets the Aside the composer
// says it gets, not the Send the refused comment asked for.
test("a comment's Retry goes in the mode the composer shows now, not the refused one's", async () => {
  const createComment = spyOn(api, "createComment")
    .mockRejectedValueOnce(commentRefused)
    .mockResolvedValueOnce(createdComment);
  const steers: Agent = { ...planner, capabilities: ["aside", "btw", "steer"] };
  const asides: Agent = { ...planner, capabilities: ["aside", "btw"] };
  const user = userEvent.setup();
  const seedMentions = [plannerMention];
  const { rerender, view } = renderComposer({ agents: [steers], seedMentions });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste(" hello");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    expect(createComment.mock.calls[0]?.[1]).toEqual(
      expect.objectContaining({ delivery: "steer" })
    );

    rerender({ agents: [asides], seedMentions });
    expect(screen.getByText("Sending as Aside: Planner does not advertise Send.")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls[1]?.[1]).toEqual(
      expect.objectContaining({ delivery: "aside" })
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("the Sending as Aside line leaves out a mentioned session that takes no messages", async () => {
  const user = userEvent.setup();
  const aside: Agent = { ...planner, capabilities: ["aside"] };
  const deaf: Agent = { ...worker, capabilities: [] };
  const { view } = renderComposer({
    agents: [aside, deaf],
    seedMentions: [
      { target: "session:A", title: "Planner" },
      { target: "session:B", title: "Worker" },
    ],
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.paste(" hello");
    // The session that takes none gets its own warning, not the Aside line.
    expect(screen.getByText("Sending as Aside: Planner does not advertise Send.")).toBeTruthy();
    expect(screen.getByText(/Worker advertises no delivery mode/)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A mentioned session listing only capabilities that are no delivery mode takes none, so it leaves
// the plain mode to the session that takes one, and the Aside line leaves it out too.
test("a mentioned session whose capabilities hold no delivery mode decides no mode and gets its own warning", async () => {
  const user = userEvent.setup();
  const aside: Agent = { ...planner, capabilities: ["aside"] };
  const unknown: Agent = { ...worker, capabilities: ["future-capability"] };
  const { view } = renderComposer({
    agents: [aside, unknown],
    seedMentions: [
      { target: "session:A", title: "Planner" },
      { target: "session:B", title: "Worker" },
    ],
  });

  try {
    const field = screen.getByLabelText("Comment");
    await user.click(field);
    await user.paste(" hello");
    expect(screen.getByText("Sending as Aside: Planner does not advertise Send.")).toBeTruthy();
    expect(screen.getByText(/Worker advertises no delivery mode/).textContent).toBe(
      "Worker advertises no delivery mode, so sending it records a failed attempt. None of the capabilities it lists is a delivery mode this Dispatch knows."
    );
    expect(screen.queryByText(/while its plugin cannot reach/)).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// A warning naming several sessions gives each cause once, of the sessions it is true of: an
// empty list, the plugin that cannot reach its conversation; a list of capabilities that are no
// mode, that none is one - said of `they` when it is every session named, else of their names.
for (const { lists, reasons } of [
  {
    lists: [[], [], []],
    reasons: "A session advertises none while its plugin cannot reach its conversation.",
  },
  {
    lists: [["future-capability"], ["other-capability"], ["future-capability"]],
    reasons: "None of the capabilities they list is a delivery mode this Dispatch knows.",
  },
  {
    lists: [["future-capability"], [], []],
    reasons:
      "A session advertises none while its plugin cannot reach its conversation. None of the capabilities Planner lists is a delivery mode this Dispatch knows.",
  },
  {
    lists: [["future-capability"], [], ["other-capability"]],
    reasons:
      "A session advertises none while its plugin cannot reach its conversation. None of the capabilities Planner and Tester list is a delivery mode this Dispatch knows.",
  },
]) {
  test(`the no-delivery-mode warning for sessions listing ${JSON.stringify(lists)} says why each takes none`, async () => {
    const user = userEvent.setup();
    const [first = [], second = [], third = []] = lists;
    const { view } = renderComposer({
      agents: [
        { ...planner, capabilities: first },
        { ...worker, capabilities: second },
        { ...worker, capabilities: third, session_id: "C", title: "Tester" },
      ],
      seedMentions: [
        { target: "session:A", title: "Planner" },
        { target: "session:B", title: "Worker" },
        { target: "session:C", title: "Tester" },
      ],
    });

    try {
      const field = screen.getByLabelText("Comment");
      await user.click(field);
      await user.paste(" hello");
      expect(screen.getByText(/advertise no delivery mode/).textContent).toBe(
        `Planner and Worker and Tester advertise no delivery mode, so sending it records a failed attempt. ${reasons}`
      );
    } finally {
      view.unmount();
    }
  });
}

test("mentioning two targets that both lack the outbound mode names both in the warning", async () => {
  const { view } = renderComposer({
    agents: [planner, worker],
    seedMentions: [
      { target: "session:A", title: "Planner" },
      { target: "session:B", title: "Worker" },
    ],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() =>
      expect(field.value).toBe("[@Planner](dispatch://agent/A) [@Worker](dispatch://agent/B) ")
    );
    expect(screen.getByText(/Planner and Worker do not advertise Send/)).toBeTruthy();
  } finally {
    view.unmount();
  }
});

const plannerMention = { target: "session:A", title: "Planner" };
const workerMention = { target: "session:B", title: "Worker" };
/** What each of those two seeds writes into a draft: the mention that names the party. A seed
 *  follows each mention with one space, so a draft holding the seed alone reads `${link} `. */
const plannerLink = "[@Planner](dispatch://agent/A)";
const workerLink = "[@Worker](dispatch://agent/B)";
const createdMessage: Message = {
  author: { id: "alice", kind: "user" },
  body: "sent",
  broadcast_id: null,
  reply_block: null,
  created_at: "2026-09-18T00:00:00Z",
  deliveries: [],
  ...messageCorrectionFields(),
  id: "message-1",
  in_reply_to: null,
  issue_key: null,
  target: null,
};

// The seed is the one branch these seven call sites share, so its cases live here rather than
// in one host's e2e: what a mount seeds, what a draft entering a seeded channel gets, what a
// draft that already holds the channel's mention gets, where a mention seeded afterwards
// lands, and what leaves with a channel. Each asserts the wire as well as the field.
test("a mount seeds the owner's mention and sends it", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const user = userEvent.setup();
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    expect(field.value).toBe(`${plannerLink} `);
    await user.click(field);
    await user.paste("hello");
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: `${plannerLink} hello`,
          delivery: "steer",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// An Agents row changes the composer's owner and seed when the reader picks an issue, and the
// live draft takes the new seed in place, by the rules a mount applies.
test("a draft entering a channel that owes a mention is seeded in front of it", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const user = userEvent.setup();
  const { rerender, view } = renderComposer({
    agents: [planner, worker],
    seedMentions: [workerMention],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(field.value).toBe(`${workerLink} `));
    act(() => field.focus());
    field.setSelectionRange(0, 0);
    await user.paste("hello ");
    await waitFor(() => expect(field.value).toBe(`hello ${workerLink} `));
    rerender({ agents: [planner, worker], seedMentions: [plannerMention] });
    await waitFor(() => expect(field.value).toBe(`${plannerLink} hello ${workerLink} `));
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: `${plannerLink} hello ${workerLink} `,
          delivery: "steer",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a draft that already holds the channel's mention is not seeded twice, wherever it sits", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const user = userEvent.setup();
  const { rerender, view } = renderComposer({
    agents: [planner, worker],
    seedMentions: [plannerMention],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await waitFor(() => expect(field.value).toBe(`${plannerLink} `));
    act(() => field.focus());
    field.setSelectionRange(0, 0);
    await user.paste("y");
    field.setSelectionRange(field.value.length, field.value.length);
    await user.paste("x");
    await waitFor(() => expect(field.value).toBe(`y${plannerLink} x`));
    // The channel now owes the worker as well; the planner it already holds is left where it is.
    rerender({ agents: [planner, worker], seedMentions: [plannerMention, workerMention] });
    await waitFor(() => expect(field.value).toBe(`${workerLink} y${plannerLink} x`));
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: `${workerLink} y${plannerLink} x`,
          delivery: "steer",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a mention seeded after a reseed lands in front of the one before it", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const direct = { kind: "session" as const, sessionId: "A" };
  const user = userEvent.setup();
  const { rerender, view } = renderComposer({ agents: [planner, worker], owner: direct });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("hello");
    rerender({ agents: [planner, worker], seedMentions: [plannerMention] });
    await waitFor(() => expect(field.value).toBe(`${plannerLink} hello`));
    rerender({ agents: [planner, worker], seedMentions: [plannerMention, workerMention] });
    await waitFor(() => expect(field.value).toBe(`${workerLink} ${plannerLink} hello`));
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: `${workerLink} ${plannerLink} hello`,
          delivery: "steer",
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A message this channel already addresses needs no mention of its own recipient: the text
// still names it as the last channel seeded it, so the mention goes with that channel.
test("the owner's own untouched mention leaves with the channel that seeded it", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue(createdMessage);
  const user = userEvent.setup();
  const { rerender, view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("hello");
    rerender({ owner: { kind: "session", sessionId: "A" } });
    await waitFor(() => expect(field.value).toBe("hello"));
    await user.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() =>
      expect(createAgentMessage).toHaveBeenCalledWith(
        "A",
        { body: "hello", delivery: "steer" },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

test("an owner mention the reader edited is their prose and stays", async () => {
  const user = userEvent.setup();
  const { rerender, view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    // The text names no party now, so there is no mention of this session to take away.
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("@Plannr hello");
    rerender({ owner: { kind: "session", sessionId: "A" } });
    await waitFor(() => expect(field.value).toBe("@Plannr hello"));
  } finally {
    view.unmount();
  }
});

// "Discard draft?" means the draft is gone, in every host. Where `onClose` unmounts the composer
// that was true by accident; on the Agents page it only moves focus, so the reset has to be the
// composer's own - the same one a successful send runs. What it leaves is what a mount shows: the
// channel's own mention, seeded again, or the next message would not reach it.
test("Discard clears the draft to what a mount shows, wherever the host takes focus", async () => {
  const user = userEvent.setup();
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Discard me");
    act(() => field.focus());
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Discard" }));

    expect(field.value).toBe(`${plannerLink} `);
  } finally {
    view.unmount();
  }
});

// Keep, which takes `Discard draft?` away, gives the focus back to the box the reader was writing
// in, and a pick in the reference picker leaves it there. The browser's own scroll of a focused
// box centres one that is not wholly in view in Chromium and Firefox, a scroll the reader asked
// for nowhere: Keep gives that focus with no scroll of the browser's own, and brings the box into
// view only where the reader cannot see it whole (`focusIntoView`). A pick moves no focus at all:
// a row of the list keeps the focus in the box while it is clicked (`RefPickerList`).
test("Keep gives the focus back to the box with no scroll of the browser's own, and a picked reference leaves it there", async () => {
  const user = userEvent.setup();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  /** Each focus that moved the focus to the box since the last read: whether it was asked to
   *  scroll nothing. A focus of the box while it already holds the focus moves nothing and
   *  scrolls nothing (the focusing steps end at once), as the field's own focus after a pick
   *  does (`MentionField`'s `takePick`). */
  let given: boolean[] = [];
  const focus = HTMLElement.prototype.focus;
  const spy = spyOn(HTMLElement.prototype, "focus").mockImplementation(function (
    this: HTMLElement,
    options?: FocusOptions
  ) {
    if (this.getAttribute("aria-label") === "Comment" && document.activeElement !== this) {
      given.push(options?.preventScroll === true);
    }
    focus.call(this, options);
  });
  const { view } = renderComposer({ agents: [planner] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("Keep me");
    await user.keyboard("{Escape}");
    given = [];
    await user.click(screen.getByRole("button", { name: "Keep" }));
    expect(field).toHaveFocus();
    expect(given, "Keep gave the box the focus with the browser's own scroll").toEqual([true]);

    await user.paste(" @");
    const row = await screen.findByRole("option", { name: /^Planner\b/ });
    given = [];
    await user.click(row);
    await waitFor(() => expect(field.value).toBe("Keep me [@Planner](dispatch://agent/A)"));
    expect(field).toHaveFocus();
    expect(given, "the pick moved the focus out of the box and back").toEqual([]);
  } finally {
    view.unmount();
    spy.mockRestore();
    listAgents.mockRestore();
  }
});

// A refusal is about the draft it turned down: Retry sends the draft as it stands, so it offers
// only what Send would, and Discard, which drops the draft, drops the notice with it.
test("a refused send's Retry asks what Send asks, and Discard takes the notice with the draft", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(commentRefused);
  const user = userEvent.setup();
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("@Planner hello");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    expect(field.value).toBe("@Planner hello");
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();

    // Nothing Send would send, so nothing Retry may: the button goes, the notice stays.
    await user.clear(field);
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(screen.getByText(`Couldn't send — ${COMMENT_REFUSED}`)).toBeDefined();

    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("@Planner hello");
    act(() => field.focus());
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Discard" }));
    expect(field.value).toBe(`${plannerLink} `);
    expect(screen.queryByText(`Couldn't send — ${COMMENT_REFUSED}`)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// While a send is out the composer is held, never disabled: the browser takes the focus off a
// control that turns disabled (`emulateChromiumFocusFixup` does so here), which would leave a
// reader who sent by Ctrl+Enter on the page's body. The box keeps the focus through the send and
// takes no typing, a second Ctrl+Enter sends nothing, and Tab still leaves it for Send, which is
// held as well: Enter or a click on it sends nothing, and it keeps the focus.
test("the box Ctrl+Enter sent from keeps the focus through the send, and no key or press sends again", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const answer = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(answer.promise);
  const restoreFocus = emulateChromiumFocusFixup();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("Ship it.");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await tick(20);
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(focusedName()).toBe("Comment");
    expect(composerHeld()).toBe(true);
    expect(field).toHaveAttribute("aria-disabled", "true");
    expect(field.readOnly).toBe(true);

    await user.keyboard("x");
    expect(field.value).toBe("Ship it.");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await tick(20);
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(focusedName()).toBe("Comment");

    await user.tab();
    expect(focusedName()).toBe("Sending…");
    await user.keyboard("{Enter}");
    await user.click(screen.getByRole("button", { name: "Sending…" }));
    await tick(20);
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(focusedName()).toBe("Sending…");

    await act(async () => answer.resolve(createdComment));
    await tick(20);
    expect(field.value).toBe("");
    expect(composerHeld()).toBe(false);
  } finally {
    restoreFocus();
    realClock();
    view.unmount();
    createComment.mockRestore();
  }
});

// While a send holds the box, a key the hold stops reaches no handler of the composer's, and the
// browser keeps its own action for every key the composer neither acts on nor types with: Ctrl+C
// copies from the held draft, Ctrl+F finds in the page and F5 reloads it. A character, Backspace,
// Escape and Ctrl+Enter are the composer's, and the hold takes them from the browser too: the draft
// takes no character, Escape raises no Discard prompt, and Ctrl+Enter sends nothing more.
test("while a send is out, Ctrl+C, Ctrl+F and F5 in the box reach the browser, and a character, Escape and Ctrl+Enter do nothing", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const answer = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(answer.promise);
  const { view } = renderComposer();
  // Each key but Control, read from the window's capture phase, before the composer's own capture
  // can stop it. Whether a handler cancelled the browser's action is read once the press is over.
  const pressed: KeyboardEvent[] = [];
  const record = (event: KeyboardEvent) => {
    if (event.key !== "Control") pressed.push(event);
  };

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("Ship it.");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await tick(20);
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(composerHeld()).toBe(true);

    window.addEventListener("keydown", record, true);
    await user.keyboard(
      "{Control>}c{/Control}{Control>}f{/Control}{F5}x{Backspace}{Escape}{Control>}{Enter}{/Control}"
    );
    await tick(20);
    expect(
      pressed.map(
        (event) =>
          `${event.ctrlKey ? "Ctrl+" : ""}${event.key}: ${event.defaultPrevented ? "taken" : "the browser's"}`
      )
    ).toEqual([
      "Ctrl+c: the browser's",
      "Ctrl+f: the browser's",
      "F5: the browser's",
      "x: taken",
      "Backspace: taken",
      "Escape: taken",
      "Ctrl+Enter: taken",
    ]);
    expect(field.value).toBe("Ship it.");
    expect(screen.queryByRole("button", { name: "Discard" })).not.toBeInTheDocument();
    expect(createComment).toHaveBeenCalledTimes(1);

    await act(async () => answer.resolve(createdComment));
    await tick(20);
    expect(composerHeld()).toBe(false);
  } finally {
    window.removeEventListener("keydown", record, true);
    realClock();
    view.unmount();
    createComment.mockRestore();
  }
});

// Offline a send can be out longer than the write deadline (`SEND_DEADLINE_MS`). The box the
// reader sent from never loses the focus: the focus is on the box past that deadline, and still
// there once the send lands.
test("offline, the box a send went from keeps the focus while the send is out past the write deadline", async () => {
  const online = onlineManager.isOnline();
  const answer = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(answer.promise);
  const restoreFocus = emulateChromiumFocusFixup();
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { queryClient, view } = renderComposer({ inMain: true });

  try {
    act(() => onlineManager.setOnline(false));
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("Sent offline");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await tick(20);
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(focusedName()).toBe("Comment");

    await tick(SEND_DEADLINE_MS + 5_000);
    expect(screen.getByText(KEYED_SEND_LATE_TEXT)).toBeTruthy();
    expect(composerHeld()).toBe(true);
    expect(focusedName()).toBe("Comment");

    await act(async () => answer.resolve(createdComment));
    await tick(20);
    expect(field.value).toBe("");
    expect(composerHeld()).toBe(false);
    expect(focusedName()).toBe("Comment");
  } finally {
    view.unmount();
    queryClient.clear();
    onlineManager.setOnline(online);
    realClock();
    restoreFocus();
    createComment.mockRestore();
  }
});

// A file pasted or dropped into the box while a send is out goes into no draft: the send holds the
// box, and its paste and its drop upload nothing until the send answers.
test("a file pasted or dropped into the box while a send is out uploads nothing", async () => {
  const answer = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(answer.promise);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(
    uploaded("shot-png", "shot.png", 1)
  );
  const user = userEvent.setup();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("Ship it.");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    const file = new File([new Uint8Array([137, 80, 78, 71])], "shot.png", { type: "image/png" });
    // user-event's paste carries text, never files.
    fireEvent.paste(field, { clipboardData: { files: [file] } });
    // user-event has no drop: it sends no DragEvent and carries no dataTransfer.
    fireEvent.drop(field, { dataTransfer: { files: [file] } });

    await act(async () => answer.resolve(createdComment));
    await waitFor(() => expect(field.value).toBe(""));
    expect(uploadArtifact).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    createComment.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// A message on its way is the server's until it answers, so its draft is not the reader's to
// discard: sending takes a `Discard draft?` already up with it, and Escape raises none while the
// send is out - from the held box, which keeps taking the focus then. The outcome then lands on
// the draft that was sent: a refusal hands it back with its notice.
test("a send takes the Discard prompt with it and Escape raises none until the server answers", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const user = userEvent.setup();
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("@Planner hello");
    act(() => field.focus());
    await user.keyboard("{Escape}");
    expect(screen.getByRole("button", { name: "Discard" })).toBeDefined();
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(composerHeld()).toBe(true));
    expect(screen.queryByRole("button", { name: "Discard" })).not.toBeInTheDocument();
    // The held field keeps the focus, and Escape in it raises no prompt.
    act(() => field.focus());
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("button", { name: "Discard" })).not.toBeInTheDocument();

    refused.reject(commentRefused);
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    expect(field.value).toBe("@Planner hello");
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Discard" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A keyboard reader who sends with Ctrl+Enter keeps their place in the box while the send is out,
// so their next key reaches the composer rather than the page's body. While it is out the composer
// says it is held, a key changes nothing, and a second Ctrl+Enter or a press of Send sends nothing.
test("a send by Ctrl+Enter keeps the focus in the box, which takes no key and sends nothing while it is out", async () => {
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(sent.promise);
  const user = userEvent.setup();
  const { view } = renderComposer();
  const stopFocusFixup = runFocusFixup();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Status please");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    // The send is out: the composer says it is held (`aria-disabled`, never `disabled`), and the
    // box keeps the focus.
    await waitFor(() => expect(composerHeld()).toBe(true));
    expect(field).toHaveFocus();
    await user.keyboard("x");
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(field.value).toBe("Status please");
    await user.click(screen.getByRole("button", { name: "Sending…" }));
    expect(createComment).toHaveBeenCalledTimes(1);

    await act(async () => sent.resolve(createdComment));
    await waitFor(() => expect(composerHeld()).toBe(false));
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    stopFocusFixup();
    view.unmount();
    createComment.mockRestore();
  }
});

// The same for a reader who pressed Send itself with Space: Send keeps the focus while the send
// is out, and a second Space sends nothing.
test("a send by Space on Send keeps the focus on Send, and a second Space sends nothing while it is out", async () => {
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(sent.promise);
  const user = userEvent.setup();
  const { view } = renderComposer();
  const stopFocusFixup = runFocusFixup();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Status please");
    const send = screen.getByRole("button", { name: "Send" });
    act(() => send.focus());
    await user.keyboard(" ");
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(composerHeld()).toBe(true));
    expect(send).toHaveTextContent("Sending…");
    expect(send).toHaveFocus();
    await user.keyboard(" ");
    expect(createComment).toHaveBeenCalledTimes(1);

    await act(async () => sent.resolve(createdComment));
    await waitFor(() => expect(composerHeld()).toBe(false));
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    stopFocusFixup();
    view.unmount();
    createComment.mockRestore();
  }
});

test("a send holds every suggestion control and a refusal restores the replacement it sent", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment")
    .mockReturnValueOnce(refused.promise)
    .mockResolvedValueOnce(createdComment);
  const user = userEvent.setup();
  const { view } = renderComposer({
    anchor: { artifact: "artifact-1", mark_id: "mark-1", quote: "brown" },
    kind: "suggestion",
  });

  try {
    const reason = screen.getByLabelText<HTMLTextAreaElement>("Reason");
    const replacement = screen.getByLabelText<HTMLTextAreaElement>("Replacement");
    await user.click(reason);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("why");
    await user.click(replacement);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("quick");
    await user.click(screen.getByRole("button", { name: "Send" }));
    // user-event refuses to write into a read-only control, which the send holds this one.
    fireEvent.change(replacement, { target: { value: "quick edited while sending" } });

    await waitFor(() => expect(composerHeld()).toBe(true));
    expect(replacement).toHaveAttribute("aria-disabled", "true");
    expect(replacement.readOnly).toBe(true);
    act(() => replacement.focus());
    await user.keyboard("x");
    expect(replacement.value).toBe("quick");

    refused.reject(commentRefused);
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    expect(replacement.value).toBe("quick");
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls[1]?.slice(0, 2)).toEqual([
      "CORE-1",
      {
        anchor: { artifact: "artifact-1", mark_id: "mark-1" },
        body: "why",
        suggestion: { replace_with: "quick" },
      },
    ]);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("an Escape in Send's task cannot offer Discard", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const user = userEvent.setup();
  const { view } = renderComposer({ seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("@Planner hello");
    act(() => {
      // One React batch: user-event renders between its own interactions.
      fireEvent.click(screen.getByRole("button", { name: "Send" }));
      // The same batch: the key reaches the field in Send's own task.
      fireEvent.keyDown(field, { key: "Escape" });
    });

    expect(screen.queryByRole("button", { name: "Discard" })).not.toBeInTheDocument();
    refused.reject(commentRefused);
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    expect(field.value).toBe("@Planner hello");
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// Enter can reach the old list in Send's task, before React draws the field held. It must not
// pick into a draft that is on the wire, whether the send clears the draft or returns it.
test.each([
  "refused",
  "successful",
] as const)("Enter in Send's task cannot take a reference from the picker over a %s send", async (outcome) => {
  const answer = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(answer.promise);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const user = userEvent.setup();
  const { view } = renderComposer({ agents: [planner], seedMentions: [plannerMention] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    const draft = "See @Pl";
    await user.clear(field);
    await user.paste(draft);
    await screen.findByRole("option", { name: /^Planner\b/ });
    act(() => {
      // One React batch: user-event renders between its own interactions.
      fireEvent.click(screen.getByRole("button", { name: "Send" }));
      expect(composerHeld()).toBe(false);
      // The same batch: Enter reaches the field before React renders the hold.
      fireEvent.keyDown(field, { key: "Enter" });
    });
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(createComment).toHaveBeenCalledWith("CORE-1", { body: draft }, keyedAttempt);
    expect(composerHeld()).toBe(true);
    expect(field).toHaveValue(draft);
    expect(screen.queryByTestId("ref-picker")).not.toBeInTheDocument();

    if (outcome === "refused") {
      answer.reject(commentRefused);
      await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
      expect(field).toHaveValue(draft);
      expect(screen.queryByTestId("ref-picker")).not.toBeInTheDocument();
      await user.clear(field);
      await user.paste(draft);
      await user.click(await screen.findByRole("option", { name: /^Planner\b/ }));
      expect(field).toHaveValue(`See ${plannerLink}`);
    } else {
      await act(async () => answer.resolve(createdComment));
      await waitFor(() => expect(field).toHaveValue(`${plannerLink} `));
      expect(screen.queryByTestId("ref-picker")).not.toBeInTheDocument();
    }
  } finally {
    view.unmount();
    listAgents.mockRestore();
    createComment.mockRestore();
  }
});

// The picker writes into the draft too: a list open when the message goes would put a reference
// into a draft its refusal then replaces. Sending closes it.
test("a send closes the reference picker open over its draft", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const user = userEvent.setup();
  const { view } = renderComposer({ agents: [planner, worker] });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("hello @Pl");
    await screen.findByTestId("ref-picker");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(composerHeld()).toBe(true));
    expect(screen.queryByTestId("ref-picker")).not.toBeInTheDocument();

    refused.reject(commentRefused);
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    expect(field.value).toBe("hello @Pl");
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A reply is part of the message's address. The composer that started it holds that address until
// the answer, so Cancel reply cannot rewrite a send while it is on the wire.
test("a refused reply keeps the reply its send started with until the server answers", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment")
    .mockReturnValueOnce(refused.promise)
    .mockResolvedValueOnce(createdComment);
  let cancelled = 0;
  const cancelReply = () => {
    cancelled += 1;
  };
  const user = userEvent.setup();
  const { view } = renderComposer({
    onCancelReply: cancelReply,
    replyTo: { actor: { id: "Bob", kind: "user" }, excerpt: "Earlier", id: "comment-0" },
    seedMentions: [plannerMention],
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Reply body");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    const cancel = screen.getByRole("button", { name: "Cancel reply" }) as HTMLButtonElement;
    expect(composerHeld()).toBe(true);
    await user.click(cancel);
    expect(cancelled).toBe(0);

    refused.reject(commentRefused);
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    expect(field.value).toBe("Reply body");
    await user.click(cancel);
    expect(cancelled).toBe(1);

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls[1]?.slice(0, 2)).toEqual([
      "CORE-1",
      { body: "Reply body", reply_to: "comment-0" },
    ]);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A send is the request it started as. TanStack calls `mutationFn` only once `onMutate` has
// resolved, with the options of the latest render, so a host that re-addresses the composer in
// Send's own task - before React re-renders, with no guard of its own - changes only what the
// composer shows next. Here the owner, the kind, the anchor and a reply into a message thread all
// change at once, and the comment still goes as it was sent, to where it was sent.
test("a host that re-addresses the composer in Send's task cannot re-address the send", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const createMessage = spyOn(api, "createMessage").mockResolvedValue(createdMessage);
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue(createdMessage);
  const createAsk = spyOn(api, "createAsk").mockResolvedValue({} as never);
  const user = userEvent.setup();
  const { rerender, view } = renderComposer({
    anchor: { artifact: "artifact-1", mark_id: "mark-1", quote: "brown" },
  });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Status please");
    act(() => {
      // One React batch: user-event renders between its own interactions, so the press and the
      // re-address under it cannot both be its.
      fireEvent.click(screen.getByRole("button", { name: "Send" }));
      rerender({
        anchor: { artifact: "artifact-2", mark_id: "mark-2", quote: "fox" },
        kind: "ask",
        owner: { kind: "session", sessionId: "A" },
        replyTo: {
          actor: plannerAuthor,
          excerpt: "Can this ship?",
          id: "message-0",
          parentKind: "message",
          thread: { delivery: "btw", target: "session:A" },
        },
      });
    });

    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(createComment.mock.calls[0]?.slice(0, 2)).toEqual([
      "CORE-1",
      { anchor: { artifact: "artifact-1", mark_id: "mark-1" }, body: "Status please" },
    ]);
    expect(createMessage).not.toHaveBeenCalled();
    expect(createAgentMessage).not.toHaveBeenCalled();
    expect(createAsk).not.toHaveBeenCalled();

    refused.reject(commentRefused);
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    expect(field.value).toBe("Status please");
  } finally {
    view.unmount();
    createComment.mockRestore();
    createMessage.mockRestore();
    createAgentMessage.mockRestore();
    createAsk.mockRestore();
  }
});

test("the kind switch hands the pick to the host and shows why the host refused it", async () => {
  const user = userEvent.setup();
  const picks: string[] = [];
  const { view } = renderComposer({
    anchor: { artifact: "artifact-1", mark_id: "m-1", quote: "brown" },
    onKindChange: (next) => {
      picks.push(next);
      return next === "suggestion"
        ? "A suggestion needs whole words inside one table cell."
        : undefined;
    },
  });

  try {
    const kinds = screen.getByRole("group", { name: "Kind" });
    const pressed = (name: string) =>
      within(kinds).getByRole("button", { name }).getAttribute("aria-pressed");

    await user.click(within(kinds).getByRole("button", { name: "Suggest" }));
    expect(picks).toEqual(["suggestion"]);
    expect(screen.getByRole("status").textContent).toBe(
      "A suggestion needs whole words inside one table cell."
    );
    // The kind is the host's: a refused pick leaves the pressed button where it was.
    expect(pressed("Comment")).toBe("true");
    expect(pressed("Suggest")).toBe("false");

    await user.click(within(kinds).getByRole("button", { name: "Ask" }));
    expect(picks).toEqual(["suggestion", "ask"]);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    // ...and an accepted pick shows only once the host answers through `kind`.
    expect(pressed("Comment")).toBe("true");
  } finally {
    view.unmount();
  }
});

test("a refusal belongs to the mark it answered: a newer anchor leaves it behind", async () => {
  const user = userEvent.setup();
  function Host(): ReactNode {
    const [markId, setMarkId] = useState("m-1");
    return (
      <>
        <button onClick={() => setMarkId("m-2")} type="button">
          Replace mark
        </button>
        <MentionComposer
          anchor={{ artifact: "artifact-1", mark_id: markId, quote: "brown" }}
          box="margin-composer"
          onClose={() => {}}
          onKindChange={() => "Not this one."}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
        />
      </>
    );
  }
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <Host />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const kinds = screen.getByRole("group", { name: "Kind" });
    await user.click(within(kinds).getByRole("button", { name: "Suggest" }));
    expect(screen.getByRole("status").textContent).toBe("Not this one.");
    await user.click(screen.getByRole("button", { name: "Replace mark" }));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

const hostKey: MutationKey = ["conversation-composer", "CORE-1"];

/** A host's two holds on the composer's send: a Reply's, which lasts until the server answers,
 *  and a Back's, which lets go at the send's deadline. */
function HostHolds(): ReactNode {
  const reply = useSending(hostKey).sending;
  const back = useSending(hostKey, { untilDeadline: true }).sending;
  return (
    <output data-testid="host-holds">{`Reply ${reply ? "held" : "free"}, Back ${back ? "held" : "free"}`}</output>
  );
}

function renderHostedComposer() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const invalidated = spyOn(queryClient, "invalidateQueries");
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <HostHolds />
        <MentionComposer
          box="conversation-composer"
          mutationKey={hostKey}
          onClose={() => {}}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { invalidated, view };
}

/** What every send says once it is past its deadline: it is still being tried under its key. */
const stillSending = KEYED_SEND_LATE_TEXT;

// Past its deadline a send is still the server's: a Retry would post it a second time. The
// composer keeps the draft and the host keeps its Reply, and only the host's Back lets go, so a
// request the server never answers cannot keep the reader in front of it.
test("a send the server answers at 35 s posts once: the deadline offers no Retry, and the answer lands", async () => {
  const answer = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(answer.promise);
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { invalidated, view } = renderHostedComposer();
  const holds = () => screen.getByTestId("host-holds").textContent;

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Slow but sure");
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(holds()).toBe("Reply held, Back held");

    await act(async () => {
      jest.advanceTimersByTime(SEND_DEADLINE_MS);
    });
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(screen.getByText(stillSending)).toBeTruthy();
    expect(field.value).toBe("Slow but sure");
    expect(composerHeld()).toBe(true);
    expect(holds()).toBe("Reply held, Back free");
    // The held field keeps the focus and takes its keys: one writes nothing, and Ctrl+Enter,
    // Send's other way in, sends nothing while the send is still out.
    act(() => field.focus());
    await user.keyboard("x");
    expect(field.value).toBe("Slow but sure");
    await user.keyboard("{Control>}{Enter}{/Control}");

    await act(async () => {
      jest.advanceTimersByTime(5_000);
      answer.resolve(createdComment);
    });
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(field.value).toBe("");
    expect(screen.queryByText(stillSending)).not.toBeInTheDocument();
    expect(holds()).toBe("Reply free, Back free");
    const refreshed = invalidated.mock.calls.map(([filters]) => JSON.stringify(filters?.queryKey));
    expect(refreshed).toContain(JSON.stringify(["comments", "CORE-1"]));
    expect(refreshed).toContain(JSON.stringify(["events", "CORE-1"]));
  } finally {
    realClock();
    view.unmount();
    createComment.mockRestore();
  }
});

// The answer that follows a deadline is the send's outcome whatever it is: a refusal hands the
// draft back with Retry, as one inside the deadline does.
test("a refusal after the deadline hands the draft back with Retry", async () => {
  const answer = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(answer.promise);
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { view } = renderHostedComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Refused late");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await act(async () => {
      jest.advanceTimersByTime(SEND_DEADLINE_MS);
    });
    expect(screen.getByText(stillSending)).toBeTruthy();

    await act(async () => {
      answer.reject(commentRefused);
    });
    expect(screen.getByText(`Couldn't send — ${COMMENT_REFUSED}`)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(screen.queryByText(stillSending)).not.toBeInTheDocument();
    expect(field.value).toBe("Refused late");
    expect(composerHeld()).toBe(false);
    expect(screen.getByTestId("host-holds").textContent).toBe("Reply free, Back free");
  } finally {
    realClock();
    view.unmount();
    createComment.mockRestore();
  }
});

/** The key each send to a session carried (`Idempotency-Key`), in order. */
function sentKeys(createAgentMessage: Mock<typeof api.createAgentMessage>): string[] {
  return createAgentMessage.mock.calls.map(([, , bounds]) => bounds.key);
}

const SEND_KEY = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** Dispatch's verdict on a message to a session, which a Retry of the same message meets again. */
const messageRefused = new ApiError(400, {
  code: "MESSAGE_INPUT",
  error: "Dispatch refused the message",
});

// A message to one session names itself to Dispatch with a key minted when Send builds it, so a
// repeat of the request is the same message there; the next message is another send, with a key
// of its own, even in the same words.
test("a send to one session carries a key", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockResolvedValue(createdMessage);
  const user = userEvent.setup();
  const { view } = renderComposer({ owner: { kind: "session", sessionId: "A" } });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Status please");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(1));
    expect(createAgentMessage).toHaveBeenCalledWith(
      "A",
      { body: "Status please", delivery: "steer" },
      expect.objectContaining({ key: expect.stringMatching(SEND_KEY) })
    );

    await waitFor(() => expect(field.value).toBe(""));
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Status please");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(2));
    const [first, second] = sentKeys(createAgentMessage);
    expect(second).toMatch(SEND_KEY);
    expect(second).not.toBe(first);
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

// Dispatch's refusal is its verdict on the key, which it would answer the same way for a day, so
// a Retry - even of the same text to the same session - is the reader's new action, under a key
// of its own. (A Retry after Stop trying, which ended without a verdict, keeps the key: below.)
test("a Retry after Dispatch's refusal goes under a new key, even of the same text", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(messageRefused)
    .mockResolvedValueOnce(createdMessage);
  const user = userEvent.setup();
  const { view } = renderComposer({ owner: { kind: "session", sessionId: "A" } });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Status please");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText("Couldn't send — Dispatch refused the message");
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(2));

    const [first, retried] = sentKeys(createAgentMessage);
    expect(first).toMatch(SEND_KEY);
    expect(retried).toMatch(SEND_KEY);
    expect(retried).not.toBe(first);
    await waitFor(() => expect(field.value).toBe(""));
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

// After Dispatch's refusal every Retry is a new action: another body, another mode, and the same
// message again, each under a key of its own. Another reply the host moves the composer onto is
// another address, which the refused message was not written for: from there it has no Retry at
// all.
test("after Dispatch's refusal every Retry, edited or not, gets a key of its own", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(messageRefused);
  const direct = { kind: "session" as const, sessionId: "A" };
  const user = userEvent.setup();
  const { rerender, view } = renderComposer({ owner: direct });
  const refusedText = "Couldn't send — Dispatch refused the message";
  /** Once the last attempt's refusal hands its draft back, `edit`s it and retries the draft as
   *  it then stands. */
  const retry = async (calls: number, edit?: () => Promise<void>) => {
    await screen.findByText(refusedText);
    await edit?.();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(calls));
  };

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Status please");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(1));

    await retry(2, async () => {
      // Another body.
      await user.click(field);
      await user.keyboard("{Control>}a{/Control}");
      await user.paste("Status please, now");
    });
    await retry(3, async () => {
      // Another mode.
      await user.click(field);
      await user.keyboard("{Control>}a{/Control}");
      await user.paste("/aside Status please, now");
    });
    await screen.findByText(refusedText);
    rerender({
      owner: direct,
      replyTo: {
        actor: { id: "A", kind: "session", origin: { session_title: "Planner" } },
        excerpt: "Question",
        id: "message-9",
        thread: { delivery: "btw", target: "session:A" },
      },
    });
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    rerender({ owner: direct, replyTo: null });
    // And nothing changed since: the same message, under a key of its own all the same.
    await retry(4);

    const keys = sentKeys(createAgentMessage);
    expect(new Set(keys).size).toBe(4);
    expect(createAgentMessage.mock.calls.map(([, input]) => input)).toEqual([
      { body: "Status please", delivery: "steer" },
      { body: "Status please, now", delivery: "steer" },
      { body: "Status please, now", delivery: "aside" },
      { body: "Status please, now", delivery: "aside" },
    ]);
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

// Past its deadline a message to one session is still being tried under its key, so the composer
// says so instead of offering a Retry, and the message lands once when Dispatch answers.
test("a send to one session past its deadline says it keeps trying, and lands once", async () => {
  const answer = Promise.withResolvers<Message>();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockReturnValueOnce(answer.promise);
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { view } = renderComposer({ owner: { kind: "session", sessionId: "A" } });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Slow but sure");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await act(async () => {
      jest.advanceTimersByTime(SEND_DEADLINE_MS);
    });
    expect(screen.getByText(KEYED_SEND_LATE_TEXT)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(field.value).toBe("Slow but sure");

    await act(async () => {
      answer.resolve(createdMessage);
    });
    expect(createAgentMessage).toHaveBeenCalledTimes(1);
    expect(field.value).toBe("");
    expect(screen.queryByText(KEYED_SEND_LATE_TEXT)).not.toBeInTheDocument();
  } finally {
    realClock();
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

/** A send that keeps failing, from its press to its 30 s deadline: five attempts, at 0, 1, 3, 7
 *  and 15 s. */
async function failToTheDeadline(): Promise<void> {
  for (const wait of [1_000, 2_000, 4_000, 8_000]) await jump(wait);
  expect(screen.queryByRole("button", { name: "Stop trying" })).not.toBeInTheDocument();
  await jump(15_000);
}

const stoppedText = new SendStoppedError().message;

// A lasting outage would hold a message to one session, its draft and its Send, for good. Once it
// is late the composer offers Stop trying: no attempt follows, the draft comes back with what a
// Retry does - send it again under the same key, so it is delivered at most once - and Discard
// drops it.
test("a send kept failing shows Stop trying once late, which stops it for Retry or Discard", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { view } = renderComposer({ owner: { kind: "session", sessionId: "A" } });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Status please");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await failToTheDeadline();
    expect(screen.getByText(KEYED_SEND_LATE_TEXT)).toBeTruthy();
    expect(createAgentMessage).toHaveBeenCalledTimes(5);

    await user.click(screen.getByRole("button", { name: "Stop trying" }));
    await tick(0);
    expect(screen.getByText(stoppedText)).toBeTruthy();
    expect(screen.queryByText(KEYED_SEND_LATE_TEXT)).not.toBeInTheDocument();
    expect(field.value).toBe("Status please");
    expect(composerHeld()).toBe(false);
    expect(screen.getByRole("button", { name: "Discard draft" })).toBeTruthy();
    await jump(30 * 60_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(5);

    // Retry: the same message, under the same key.
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(0);
    expect(createAgentMessage).toHaveBeenCalledTimes(6);
    expect(new Set(sentKeys(createAgentMessage)).size).toBe(1);

    // Still down: stopped again, and this time dropped.
    await failToTheDeadline();
    await user.click(screen.getByRole("button", { name: "Stop trying" }));
    await tick(0);
    const tried = createAgentMessage.mock.calls.length;
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    expect(field.value).toBe("");
    expect(screen.queryByText(stoppedText)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    await jump(30 * 60_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(tried);
  } finally {
    realClock();
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

// A stopped send waits for the reader, and a day later Dispatch has forgotten the key its
// attempts went out under: the Retry sends nothing under that key. The composer says what
// happened in the refusal's own words - a Retry is a new message, delivered a second time if the
// first one was - and keeps the draft, Retry and Discard draft. As `api.createAgentMessage` does,
// each attempt tells its request before anything goes out: that is how the key is known to have
// carried one, and the Retry is a press of its own whose entry has sent nothing yet.
test("a stopped send the reader comes back to a day later says its key has run out", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(
    (sessionID, input, bounds) => {
      bounds.sent?.({ body: input, method: "POST", path: `/api/v1/agents/${sessionID}/messages` });
      return Promise.reject(
        new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
      );
    }
  );
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { view } = renderComposer({ owner: { kind: "session", sessionId: "A" } });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.type(field, "Status please");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await failToTheDeadline();
    await user.click(screen.getByRole("button", { name: "Stop trying" }));
    await tick(0);
    const tried = createAgentMessage.mock.calls.length;

    // One jump of the key's whole life: the stopped send leaves no timer in it to step to.
    await jump(IDEMPOTENCY_KEY_TTL_MS);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(0);
    expect(createAgentMessage).toHaveBeenCalledTimes(tried);
    expect(screen.getByText(new SendExpiredError().message)).toBeTruthy();
    expect(field.value).toBe("Status please");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Discard draft" })).toBeTruthy();
  } finally {
    realClock();
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

/** A question of Planner's in its direct thread, whose replies go as BTW. */
const plannerQuestion: ReplyTarget = {
  actor: plannerAuthor,
  excerpt: "Question",
  id: "message-9",
  parentKind: "message",
  thread: { delivery: "btw", target: "session:A" },
};

/**
 * One composer answering `plannerQuestion` in a direct message, whose address moves to one place
 * - the session itself, the reply ended - by either of two controls: the composer's own Cancel
 * reply, and `Host move`, a control of the host's, as another row or another thread takes a
 * composer elsewhere. Only who moved the address differs, and that decides where a refused or
 * stopped draft goes.
 */
function renderMovingReply() {
  function Host(): ReactNode {
    const [replyTo, setReplyTo] = useState<ReplyTarget | null>(plannerQuestion);
    return (
      <>
        <button onClick={() => setReplyTo(null)} type="button">
          Host move
        </button>
        <MentionComposer
          box="agents-row-composer"
          onCancelReply={() => setReplyTo(null)}
          onClose={() => {}}
          onSent={() => {}}
          owner={{ kind: "session", sessionId: "A" }}
          replyTo={replyTo}
        />
      </>
    );
  }
  return render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider
        client={
          new QueryClient({
            defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
          })
        }
      >
        <Host />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Writes the reply and sends it, and answers the field it was written in. */
async function sendReply(user: UserEvent): Promise<HTMLTextAreaElement> {
  const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
  await user.click(field);
  await user.paste("Status please");
  await user.click(screen.getByRole("button", { name: "Send" }));
  return field;
}

/** What Send says while the host has moved the composer off the reply its held draft answers. */
const WRITTEN_AS_REPLY =
  "This message was written as a reply. Reply to that message again to send it, or discard it.";

/** What a stopped reply says once the reader has taken its draft off the reply. */
const STOPPED_AS_REPLY =
  "Dispatch has not confirmed this message, sent as a reply: it may have been delivered there. " +
  "Sending it from here is a new message, which can be delivered a second time if the first one was.";

/** The first send of a message the server keeps failing, Stop trying pressed once it is late;
 *  answers how many attempts `createAgentMessage` saw go out under its key. */
async function stopReply(
  user: UserEvent,
  createAgentMessage: Mock<typeof api.createAgentMessage>
): Promise<number> {
  await sendReply(user);
  await failToTheDeadline();
  await user.click(screen.getByRole("button", { name: "Stop trying" }));
  await tick(0);
  expect(screen.getByText(stoppedText)).toBeTruthy();
  return createAgentMessage.mock.calls.length;
}

// Who moved the address decides, refused. The host moving the composer off the reply leaves the
// draft with the reply it was written for: no Retry, Send says where it was written, and nothing
// goes from the new address.
test("a refused reply the host moves off its question stays held for that question", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(messageRefused);
  const user = userEvent.setup();
  const view = renderMovingReply();

  try {
    const field = await sendReply(user);
    await screen.findByText("Couldn't send — Dispatch refused the message");
    await user.click(screen.getByRole("button", { name: "Host move" }));
    expect(screen.queryByRole("button", { name: "Cancel reply" })).not.toBeInTheDocument();

    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(sendRefusal().title).toBe(WRITTEN_AS_REPLY);
    expect(field.value).toBe("Status please");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await act(settle);
    expect(createAgentMessage).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

// The same refusal and the same move, made with the composer's own Cancel reply: the draft goes
// with it, and its Retry sends it from the session itself, under a key of its own.
test("a refused reply the reader cancels goes with them, and its Retry goes from there", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(messageRefused)
    .mockResolvedValueOnce(createdMessage);
  const user = userEvent.setup();
  const view = renderMovingReply();

  try {
    const field = await sendReply(user);
    await screen.findByText("Couldn't send — Dispatch refused the message");
    await user.click(screen.getByRole("button", { name: "Cancel reply" }));

    expect(field.value).toBe("Status please");
    expect(sendRefusal().title).toBeNull();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(2));
    expect(createAgentMessage.mock.calls.map(([, input]) => input)).toEqual([
      { body: "Status please", delivery: "btw", in_reply_to: "message-9" },
      { body: "Status please", delivery: "steer" },
    ]);
    const [first, retried] = sentKeys(createAgentMessage);
    expect(retried).toMatch(SEND_KEY);
    expect(retried).not.toBe(first);
  } finally {
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

// Who moved the address decides, stopped: the reply may have been delivered as a reply. The host
// moving the composer off it holds it there as it holds a refusal, so its key goes nowhere else:
// no Retry from the new address, and Send sends nothing.
test("a stopped reply the host moves off its question stays held for that question", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const view = renderMovingReply();

  try {
    const tried = await stopReply(user, createAgentMessage);
    await user.click(screen.getByRole("button", { name: "Host move" }));

    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    expect(sendRefusal().title).toBe(WRITTEN_AS_REPLY);
    expect(screen.getByText(stoppedText)).toBeTruthy();
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    expect(field.value).toBe("Status please");
    await pressSend(user, field);
    expect(createAgentMessage).toHaveBeenCalledTimes(tried);
  } finally {
    realClock();
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

// The same stopped reply and the same move, made with Cancel reply: the draft goes with it. The
// first write may have been delivered as the reply, so the composer says so, and its Retry sends
// a new message from the session itself, under a key none of the reply's attempts went out under.
test("a stopped reply the reader cancels is sent anew from there, saying the first may have gone", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const view = renderMovingReply();

  try {
    const tried = await stopReply(user, createAgentMessage);
    await user.click(screen.getByRole("button", { name: "Cancel reply" }));

    expect(screen.getByText(STOPPED_AS_REPLY)).toBeTruthy();
    expect(screen.queryByText(stoppedText)).not.toBeInTheDocument();
    createAgentMessage.mockResolvedValueOnce(createdMessage);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(0);
    expect(createAgentMessage).toHaveBeenCalledTimes(tried + 1);
    expect(createAgentMessage.mock.calls.at(-1)?.[1]).toEqual({
      body: "Status please",
      delivery: "steer",
    });
    const keys = sentKeys(createAgentMessage);
    expect(new Set(keys.slice(0, tried)).size).toBe(1);
    expect(keys.at(-1)).toMatch(SEND_KEY);
    expect(keys.slice(0, tried)).not.toContain(keys.at(-1));
  } finally {
    realClock();
    view.unmount();
    createAgentMessage.mockRestore();
  }
});

// A closed issue takes comments: its composer shows and sends one, and a refusal still shows,
// with Retry, as on an open issue. It refuses suggestions and asks, so the kind switch offers
// neither there.
test("a closed composer sends a comment, hides Suggest and Ask, and still shows a refusal", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment")
    .mockRejectedValueOnce(commentRefused)
    .mockResolvedValueOnce(createdComment);
  const anchor = { artifact: "artifact-1", mark_id: "mark-1", quote: "brown" };
  const props = { anchor, closed: true, onKindChange: () => undefined };
  const { rerender, view } = renderComposer(props);

  try {
    expect(screen.getByRole("form", { name: "Comment composer" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Suggest" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Ask" })).toBeNull();
    rerender({ anchor, onKindChange: () => undefined });
    expect(screen.getByRole("button", { name: "Suggest" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Ask" })).toBeTruthy();
    rerender(props);

    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("After the close");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    expect(field.value).toBe("After the close");
    expect(screen.queryByRole("button", { name: "Discard draft" })).toBeNull();

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls.at(-1)?.[1]).toEqual({
      anchor: { artifact: "artifact-1", mark_id: "mark-1" },
      body: "After the close",
    });
    await waitFor(() => expect(field.value).toBe(""));
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A suggestion is a proposed edit a closed issue refuses. Its composer stays mounted for a send it
// still has out and for that send's refusal - Send refused, Discard draft in place of Retry - and
// shows nothing else.
test("a closed suggestion composer shows only a send of its own still out, then its refusal with Discard", async () => {
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(refused.promise);
  const user = userEvent.setup();
  const anchor = { artifact: "artifact-1", mark_id: "mark-1", quote: "brown" };
  const { rerender, view } = renderComposer({ anchor, kind: "suggestion" });

  try {
    rerender({ anchor, closed: true, kind: "suggestion" });
    expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument();
    rerender({ anchor, kind: "suggestion" });

    const replacement = screen.getByLabelText<HTMLTextAreaElement>("Replacement");
    await user.click(replacement);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("red");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    rerender({ anchor, closed: true, kind: "suggestion" });
    expect(screen.getByRole("form", { name: "Comment composer" })).toBeTruthy();
    expect(composerHeld()).toBe(true);
    const heldReplacement = screen.getByLabelText<HTMLTextAreaElement>("Replacement");
    act(() => heldReplacement.focus());
    await user.keyboard("x");
    expect(heldReplacement.value).toBe("red");

    refused.reject(new ApiError(409, { code: "ISSUE_CLOSED", error: "issue is closed" }));
    await screen.findByText("Couldn't send — issue is closed");
    expect(screen.getByLabelText<HTMLTextAreaElement>("Replacement").value).toBe("red");
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
    const send = screen.getByRole("button", { name: "Send" });
    expect(send).toHaveAttribute("aria-disabled", "true");
    expect(send).toHaveAttribute("title", "This issue is closed. Reopen it to send.");

    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    expect(screen.queryByRole("form", { name: "Comment composer" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A thread's box the Inbox keeps standing under its thread (`ThreadBody`), whose host's `onClose`
// moves nothing: a reply Dispatch refused offers Discard draft beside the refusal, and the press
// takes the refusal's row away with the button that held the focus. The box stays, so the caret
// goes back to its field, as the Discard of `Discard draft?` puts it back.
test("a refusal's Discard draft in a box that stays puts the caret back in its field", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockRejectedValueOnce(commentRefused);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          box="inbox-thread-reply"
          inline
          kind="comment"
          mutationKey={["inbox-thread", "thread-1"]}
          onClose={() => {}}
          onSent={() => {}}
          owner={{ issueKey: "CORE-1", kind: "issue" }}
          replyTo={{ actor: plannerAuthor, excerpt: "", id: "comment-9", parentKind: "comment" }}
          standing
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Reply");
    await user.click(field);
    await user.paste("A reply Dispatch refuses");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    expect(screen.queryByText(`Couldn't send — ${COMMENT_REFUSED}`)).not.toBeInTheDocument();
    expect(field.value).toBe("");
    expect(document.activeElement, `the focus is on ${focusedName()}`).toBe(field);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// An upload carries its target from the paste that starts it: a host move that re-addresses the
// composer in the paste's own task - before React re-renders, and with no count in `ownMoves` -
// moves where the next upload goes, never this one, and the reference names where the file went.
// That reference belongs to the draft the file was pasted into, which the host move leaves where
// it was: it waits, and the composer says so, while the composer is on CORE-2, and joins the
// message once the composer is back on CORE-1.
test("a host move in the paste's own task cannot move the upload", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: { slug: "notes-md" } as never,
    version: {} as never,
  });
  const { rerender, view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    const file = new File(["# Notes"], "notes.md", { type: "text/markdown" });
    act(() => {
      // user-event's paste carries text, never files, and this one lands in one React batch
      // with the rerender under it.
      fireEvent.paste(field, { clipboardData: { files: [file] } });
      rerender({ owner: { issueKey: "CORE-2", kind: "issue" } });
    });

    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    expect(uploadArtifact.mock.calls[0]?.[0]).toEqual({ issue: "CORE-1" });
    expect(
      await screen.findByText(
        "The file pasted for CORE-1 is uploaded. Its reference joins the message when you go back to it."
      )
    ).toBeTruthy();
    expect(field.value).toBe("");

    rerender({});
    await waitFor(() => expect(field.value).toBe("dispatch://CORE-1/artifact/notes-md"));
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

// A draft the reader carried here from another composer goes in the field only while this
// composer holds nothing of its own (`DraftIntake`): a refusal it holds is a send of its own,
// even once the reader has emptied the field over it, so the carried draft is turned down and
// the field stays as the reader left it. The phone thread composer's Cancel reply puts its draft
// away instead (`usePhoneThread`), and its refusal is never carried in over this one.
test("a composer holding a refused send of its own takes no carried draft, even over an empty field", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValueOnce(
    new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" })
  );
  const user = userEvent.setup();
  const intake = createRef<DraftIntake>();
  const owner: ComposerOwner = { issueKey: "CORE-1", kind: "issue" };
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <MentionComposer
          box="conversation-composer"
          intake={intake}
          onClose={() => {}}
          onSent={() => {}}
          owner={owner}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.paste("Docked");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText("Couldn't send — the server is down");
    await user.clear(field);
    expect(field.value).toBe("");

    let took: boolean | undefined;
    act(() => {
      took = intake.current?.take({ body: "Carried", replacement: "" }, { owner, replyTo: null });
    });
    expect(took).toBe(false);
    expect(field.value).toBe("");
    expect(screen.getByText("Couldn't send — the server is down")).toBeTruthy();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// Dispatch stored a pasted file and lost its answer (`409 KEY_COMMITTED`): there is no version to
// name, so nothing is written into the draft, nothing shows as refused, and the composer says the
// file was stored and its reference not added - the upload is not made a second time.
test("a pasted file answered KEY_COMMITTED says it was stored and adds no reference", async () => {
  jest.useFakeTimers();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  const user = userOnFakeClock();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("See ");
    const file = new File(["# Notes"], "notes.md", { type: "text/markdown" });
    // user-event's paste carries text, never files.
    fireEvent.paste(field, { clipboardData: { files: [file] } });

    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "The upload of notes.md was made, but Dispatch lost its answer, so it may take a moment to appear. Its reference was not added to your message."
    );
    expect(field.value).toBe("See ");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await tick(20);
    expect(uploadArtifact).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

/** The server's answer to an upload of `name` as `slug`, at `version`. */
function uploaded(slug: string, name: string, version: number) {
  return {
    artifact: { kind: "image", name, slug } as never,
    version: { number: version } as never,
  };
}

// A pasted picture is part of what is sent: the composer writes the image syntax at the version
// the upload made, so the message shows the picture inline. Any other file stays a reference
// (the test above).
test("a pasted picture is written as an inline picture of its version", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(
    uploaded("shot-png", "shot.png", 3)
  );
  const user = userEvent.setup();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Look");
    const file = new File([new Uint8Array([137, 80, 78, 71])], "shot.png", { type: "image/png" });
    // user-event's paste carries text, never files.
    fireEvent.paste(field, { clipboardData: { files: [file] } });

    await waitFor(() =>
      expect(field.value).toBe("Look ![shot.png](dispatch://CORE-1/artifact/shot-png@v3)")
    );
    expect(uploadArtifact.mock.calls[0]?.[0]).toEqual({ issue: "CORE-1" });
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

// A picture still uploading is not yet in the draft, so a send then would go without it: Send
// names the upload and takes no press, Ctrl+Enter sends nothing, and the send once it lands
// carries the picture.
test("a send waits for a pasted picture still uploading, by Send and by Ctrl+Enter", async () => {
  jest.useFakeTimers();
  const { promise: landed, resolve: land } = Promise.withResolvers<void>();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockImplementation(async () => {
    await landed;
    return uploaded("shot-png", "shot.png", 1);
  });
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const user = userOnFakeClock();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Look");
    const file = new File([new Uint8Array([137, 80, 78, 71])], "shot.png", { type: "image/png" });
    // user-event's paste carries text, never files.
    fireEvent.paste(field, { clipboardData: { files: [file] } });

    const send = await screen.findByRole("button", { name: "Uploading file…" });
    expect(send).toHaveAttribute("aria-disabled", "true");
    expect(send).not.toHaveAttribute("disabled");
    await user.click(send);
    act(() => field.focus());
    await user.keyboard("{Control>}{Enter}{/Control}");
    // A send's mutation function runs a task after the press (`pressSend`).
    await tick(20);
    expect(createComment).not.toHaveBeenCalled();

    land();
    const body = "Look ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)";
    await waitFor(() => expect(field.value).toBe(body));
    act(() => field.focus());
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", { body }, keyedAttempt)
    );
  } finally {
    realClock();
    view.unmount();
    uploadArtifact.mockRestore();
    createComment.mockRestore();
  }
});

// The same wait ends at the upload's deadline: an upload that never answers - offline, each try
// failing at the network - is still tried, and its text joins the draft if it lands, but Send and
// Ctrl+Enter take the draft as it stands, and the field still says the file is uploading.
test("a send waits for a pasted picture only until its upload's deadline", async () => {
  jest.useFakeTimers();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1/artifacts", new TypeError("Failed to fetch"))
  );
  const createComment = spyOn(api, "createComment").mockResolvedValue(createdComment);
  const user = userOnFakeClock();
  const { view } = renderComposer();

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Look");
    const file = new File([new Uint8Array([137, 80, 78, 71])], "shot.png", { type: "image/png" });
    // user-event's paste carries text, never files.
    fireEvent.paste(field, { clipboardData: { files: [file] } });
    await untilAttempted(uploadArtifact, 1);

    await tick(SEND_DEADLINE_MS - 1_000);
    const send = screen.getByRole("button", { name: "Uploading file…" });
    expect(send, "Send, a second short of the upload's deadline").toHaveAttribute(
      "aria-disabled",
      "true"
    );

    await tick(1_000);
    expect(screen.getByRole("status"), "the upload, past its deadline").toHaveTextContent(
      "Uploading file…"
    );
    expect(
      screen.getByRole("button", { name: "Send" }),
      "Send, past the upload's deadline"
    ).not.toHaveAttribute("aria-disabled");
    field.focus();
    await user.keyboard("{Control>}{Enter}{/Control}");
    // A send's mutation function runs a task after the press (`pressSend`).
    await tick(20);
    expect(
      createComment,
      "Ctrl+Enter past the upload's deadline sends the draft"
    ).toHaveBeenCalledWith("CORE-1", { body: "Look" }, keyedAttempt);
  } finally {
    realClock();
    view.unmount();
    uploadArtifact.mockRestore();
    createComment.mockRestore();
  }
});

// A direct message on the Agents page belongs to no issue: its pictures belong to the agent's
// conversation, and are addressed under the agent's session.
test("a picture dropped into a direct message uploads to the agent's conversation", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(
    uploaded("face-png", "face [1].png", 1)
  );
  const { view } = renderComposer({ agents: [], owner: { kind: "session", sessionId: "ses-1" } });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    const file = new File([new Uint8Array([137, 80, 78, 71])], "face [1].png", {
      type: "image/png",
    });
    // user-event has no drop: it sends no DragEvent and carries no dataTransfer.
    fireEvent.drop(field, { dataTransfer: { files: [file] } });

    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    expect(uploadArtifact.mock.calls[0]?.[0]).toEqual({ session: "ses-1" });
    // A bracket in the file name would end the caption, so it is escaped.
    await waitFor(() =>
      expect(field.value).toBe("![face \\[1\\].png](dispatch://agent/ses-1/artifact/face-png@v1)")
    );
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

// The server stores a picture only for PNG, JPEG, GIF and WebP bytes, and a model is shown those
// four; an SVG is a file to both, so the composer writes it as a plain reference rather than a
// picture the artifact page would not draw.
test("a pasted SVG is written as a plain reference, not a picture", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue(
    uploaded("diagram-svg", "diagram.svg", 1)
  );
  const { view } = renderComposer({ agents: [], owner: { kind: "issue", issueKey: "CORE-1" } });

  try {
    const field = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    const file = new File(["<svg xmlns='http://www.w3.org/2000/svg'/>"], "diagram.svg", {
      type: "image/svg+xml",
    });
    // user-event's paste carries text, never files.
    fireEvent.paste(field, { clipboardData: { files: [file] } });

    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(field.value).toBe("dispatch://CORE-1/artifact/diagram-svg"));
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

const threadReplyKey: MutationKey = ["conversation-composer", "CORE-1", "thread-card", "root"];

/** A host that takes its reply composer off the page and puts it back, as a thread collapsing and
 *  opening again does: the composer unmounts and a new one mounts under the same send name. */
function renderRemountedReply(user: UserEvent) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const invalidated = spyOn(queryClient, "invalidateQueries");
  function Host(): ReactNode {
    const [shown, setShown] = useState(true);
    return (
      <>
        <button onClick={() => setShown((current) => !current)} type="button">
          {shown ? "Collapse thread" : "Expand thread"}
        </button>
        {shown ? (
          <MentionComposer
            box="inbox-thread-reply"
            inline
            mutationKey={threadReplyKey}
            onClose={() => {}}
            onSent={() => {}}
            owner={{ issueKey: "CORE-1", kind: "issue" }}
            replyTo={{ actor: plannerAuthor, excerpt: "", id: "root", parentKind: "comment" }}
          />
        ) : null}
      </>
    );
  }
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <Host />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const toggle = () =>
    user.click(screen.getByRole("button", { name: /^(Collapse|Expand) thread$/ }));
  return { invalidated, toggle, view };
}

// A composer holds its send whatever unmounts it: one that mounts under the name of a send still
// out shows that send, held, and one that mounts after the send was refused shows the draft it
// sent beside the refusal, whose Retry sends it to the same reply.
test("a reply composer mounted again while its send is out holds it, and then shows its refusal", async () => {
  const user = userEvent.setup();
  const refused = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment")
    .mockReturnValueOnce(refused.promise)
    .mockResolvedValueOnce(createdComment);
  const { toggle, view } = renderRemountedReply(user);

  try {
    const reply = screen.getByLabelText("Reply");
    await user.click(reply);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Out of view");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await toggle();
    await toggle();
    expect(screen.getByLabelText<HTMLTextAreaElement>("Reply").value).toBe("Out of view");
    expect(composerHeld()).toBe(true);
    const heldReply = screen.getByLabelText<HTMLTextAreaElement>("Reply");
    heldReply.focus();
    await user.keyboard("x");
    expect(heldReply.value).toBe("Out of view");

    await toggle();
    await act(async () => {
      refused.reject(commentRefused);
      await refused.promise.catch(() => {});
    });
    await toggle();
    expect(screen.getByText(`Couldn't send — ${COMMENT_REFUSED}`)).toBeTruthy();
    expect(screen.getByLabelText<HTMLTextAreaElement>("Reply").value).toBe("Out of view");
    expect(composerHeld()).toBe(false);

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls[1]?.slice(0, 2)).toEqual([
      "CORE-1",
      { body: "Out of view", reply_to: "root" },
    ]);
    await waitFor(() => expect(screen.getByLabelText<HTMLTextAreaElement>("Reply").value).toBe(""));
    expect(screen.queryByText(`Couldn't send — ${COMMENT_REFUSED}`)).not.toBeInTheDocument();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A send that lands while its composer is off the page has landed: what it wrote is refreshed,
// and the composer that mounts next starts empty, holding nothing.
test("a reply that lands while its composer is unmounted refreshes the thread and leaves nothing held", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(answer.promise);
  const { invalidated, toggle, view } = renderRemountedReply(user);

  try {
    const reply = screen.getByLabelText("Reply");
    await user.click(reply);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Landed out of view");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await toggle();
    await act(async () => {
      answer.resolve(createdComment);
      await answer.promise;
    });
    const refreshed = invalidated.mock.calls.map(([filters]) => JSON.stringify(filters?.queryKey));
    expect(refreshed).toContain(JSON.stringify(["comments", "CORE-1"]));

    await toggle();
    expect(screen.getByLabelText<HTMLTextAreaElement>("Reply").value).toBe("");
    expect(composerHeld()).toBe(false);
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

const remountedDraftKey: MutationKey = ["conversation-composer", "CORE-1", "remounted-draft"];

/** A host that takes its composer off the page and puts it back over one cache, as leaving the
 *  issue and coming back does: the composer unmounts and a new one mounts, under `mutationKey`
 *  where the row needs a refused send's draft to come back with it. No issue is found, so the
 *  read of which document CORE-1's spec is fails. */
function renderLeavableComposer(mutationKey?: MutationKey) {
  summaryRead.mockImplementation(() =>
    Promise.reject(new ApiError(404, { code: "NOT_FOUND", error: "no such issue" }))
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  function Host(): ReactNode {
    const [shown, setShown] = useState(true);
    return (
      <>
        <button onClick={() => setShown((current) => !current)} type="button">
          {shown ? "Leave the page" : "Come back"}
        </button>
        {shown ? (
          <MentionComposer
            box="conversation-composer"
            mutationKey={mutationKey}
            onClose={() => {}}
            onSent={() => {}}
            owner={{ issueKey: "CORE-1", kind: "issue" }}
          />
        ) : null}
      </>
    );
  }
  const view = render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <QueryClientProvider client={queryClient}>
        <Host />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { specRead: () => queryClient.getQueryState(["issue-summary", "CORE-1"]), view };
}

// The read of which document an issue's spec is belongs to a reference (`useReferenceQuery`):
// once it has failed, a composer that mounts again with the draft that asked for it reads the
// failure that is there. The draft comes back with the composer because its send was refused.
test("a spec summary read that failed is not asked again when the composer mounts again with its draft", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockRejectedValue(commentRefused);
  const { specRead, view } = renderLeavableComposer(remountedDraftKey);

  try {
    await user.click(screen.getByLabelText("Comment"));
    await user.paste("See #q2 here.");
    await waitFor(() => expect(specRead()?.status).toBe("error"));
    expect(summaryRead).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole("button", { name: "Send" }));
    await screen.findByText(`Couldn't send — ${COMMENT_REFUSED}`);

    await user.click(screen.getByRole("button", { name: "Leave the page" }));
    await user.click(screen.getByRole("button", { name: "Come back" }));
    expect(screen.getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("See #q2 here.");
    // A mount that asks starts its read as it subscribes, inside the render's own `act`, so the
    // read stands idle after one that asks nothing.
    expect(specRead()?.fetchStatus).toBe("idle");
    expect(summaryRead).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

/** Pastes `draft` into the composer on the page and lets the pills read it: the typing rests,
 *  and whatever that asks is answered. The rows that call it hold the clock. */
async function draftRested(user: UserEvent, draft: string): Promise<void> {
  await user.click(screen.getByLabelText("Comment"));
  await user.paste(draft);
  await settleRefPickerSearch("fake");
  await tick(20);
}

// A draft comes to name a `#id` after its composer mounted, which is when the read is first
// wanted. Here it is the same composer's draft, which loses its `#id` and names one again: the
// read that failed is not asked again.
test("a draft that names a #id again does not ask a failed spec summary read again", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { specRead, view } = renderLeavableComposer();

  try {
    await draftRested(user, "See #q2 here.");
    expect(specRead()?.status).toBe("error");
    expect(summaryRead).toHaveBeenCalledTimes(1);
    await user.clear(screen.getByLabelText("Comment"));
    await draftRested(user, "See #q2 again.");
    expect(summaryRead).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    view.unmount();
  }
});

// And here it is a new composer's draft, empty when it mounts, which names a `#id` once the
// reader types it: the read that failed for the composer before it is not asked again.
test("a new composer's draft that comes to name a #id does not ask a failed spec summary read again", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const { specRead, view } = renderLeavableComposer();

  try {
    await draftRested(user, "See #q2 here.");
    expect(specRead()?.status).toBe("error");
    expect(summaryRead).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole("button", { name: "Leave the page" }));
    await user.click(screen.getByRole("button", { name: "Come back" }));
    await draftRested(user, "See #q2 here.");
    expect(summaryRead).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    view.unmount();
  }
});
