import { afterEach, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import type { ChangeDecorations } from "@legion/proof-editor";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: a file pasted, which user-event has no form for
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Node as ProseMirrorNode } from "prosemirror-model";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { realClock, tick } from "../../__tests__/fake-clock";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { ApiError, api } from "../../api/client";
import type {
  Agent,
  Anchor,
  Artifact,
  ArtifactVersionText,
  AskSnooze,
  Comment,
  DisplayTree,
  InboxRow,
  IssueDetails,
  Message,
  MessageRead,
  MyReplyRow,
  MyReplyThreadRow,
  Version,
} from "../../api/types";
import { CHANGED_BLOCK_CLASS, type ChangeBaseline, changeMarksAgainst } from "../doc/change-marks";
import type { CreateEditor, EditorHandle } from "../doc/editor";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { loadedEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import { agentRow, plannerAgentRow } from "../threads/replies-fixture";
import { AskHandOnLiveContext } from "./hand-on-live";
import { installInboxApiMocks, issueAsk, mockAskReads } from "./inbox-fixture";
import { ReadingPane } from "./ReadingPane";

installInboxApiMocks();

/** The pane waits this long after the last change of the selected question before it opens
 *  another document (`DOCUMENT_SETTLE_MS` in `PaneDocument.tsx`). */
const SETTLE_MS = 200;

let getArtifact: Mock<typeof api.getArtifact>;
let getArtifactText: Mock<typeof api.getArtifactText>;
let getIssue: Mock<typeof api.getIssue>;

beforeEach(() => {
  getArtifact = spyOn(api, "getArtifact").mockRejectedValue(new Error("no document seeded"));
  getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "",
    token: "",
    version: null,
  });
  // The pane reads one field of the issue: whether it is closed, which it hands the document.
  getIssue = spyOn(api, "getIssue").mockResolvedValue({
    closed_at: null,
  } as unknown as IssueDetails);
});

afterEach(() => {
  getArtifact.mockRestore();
  getArtifactText.mockRestore();
  getIssue.mockRestore();
});

function version(number: number): Version {
  return {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-10-02T00:00:00Z",
    named: false,
    number,
    summary: null,
  };
}

function document(overrides: Partial<Artifact> = {}): Artifact {
  return {
    created_at: "2026-10-01T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: "CORE-1",
    kind: "doc",
    name: "spec.md",
    primary: true,
    project: "CORE",
    ref_key: "CORE-1/spec",
    session_id: null,
    slug: "spec",
    versions: [],
    ...overrides,
  };
}

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/** Every read the pane and the document make of one artifact, answered from the cache under the
 *  document page's own keys, so a test's render reaches the network for nothing. */
function seedDocument(
  client: QueryClient,
  artifact: Artifact,
  versions: Readonly<Record<number, string>> = {}
): void {
  client.setQueryData(["artifact", artifact.id], artifact);
  client.setQueryData(["artifact", artifact.id, "text"], { markdown: "", version: null });
  client.setQueryData(["artifact", artifact.id, "blocks"], []);
  client.setQueryData(["artifact", artifact.id, "asks"], []);
  if (artifact.issue_key !== null) {
    client.setQueryData(["asks", artifact.issue_key], []);
  }
  for (const [number, markdown] of Object.entries(versions)) {
    client.setQueryData(
      ["artifact", artifact.id, "version", Number(number)],
      versionText(Number(number), markdown)
    );
  }
}

/** The document the headless engine builds for `markdown`: what the editor would hold, which is
 *  what the version marks its baseline against. */
function renderedDocument(markdown: string): ProseMirrorNode {
  const parsed = loadedEngine()?.parse(markdown);
  if (parsed === undefined) throw new Error(`the engine rendered nothing for "${markdown}"`);
  return parsed;
}

/** The reading of `markdown` the server sends beside it, which is what the pane marks against
 *  and what the version view draws rather than parsing the markdown again. */
function treeOf(markdown: string): DisplayTree {
  return renderedDocument(markdown).toJSON() as DisplayTree;
}

/** The baseline the pane carries into the version it draws: the approved version's own stored
 *  text, with the reading the server sent of it. */
function baselineOf(markdown: string): ChangeBaseline {
  return { markdown, tree: treeOf(markdown) };
}

/** The same baseline as a pod of the build before this one answers it: the stored text, and no
 *  reading at all. */
function baselineWithoutTree(markdown: string): ChangeBaseline {
  return { markdown, tree: undefined };
}

/** The first question block of `markdown`, as the engine parses it: the node the editor's own
 *  `ask` node view is constructed from. */
function askNodeOf(markdown: string): ProseMirrorNode {
  let found: ProseMirrorNode | undefined;
  renderedDocument(markdown).descendants((node) => {
    if (found === undefined && node.type.name === "ask") {
      found = node;
    }
    return found === undefined;
  });
  if (found === undefined) throw new Error(`the engine parsed no question block in "${markdown}"`);
  return found;
}

/** Draws the question block of `markdown` in the version's editor, as the real editor draws one
 *  for every `ask` node of the text it holds: the fake editor installs the host's node views but
 *  constructs none itself, so the test constructs the one the version needs (`Inbox.test.tsx`
 *  does the same for a live document). The block is appended as the text lands, under whichever
 *  write puts it there: a version draws the reading the server sent (`setDocumentJSON`), and one
 *  that arrived with no reading writes its own markdown instead (`setMarkdown`), which is the
 *  fallback this change gives the version view. Either replaces everything in the editor's
 *  root. */
function drawAskBlock(markdown: string): (inner: CreateEditor) => CreateEditor {
  return (inner) => async (root, options) => {
    const handle: EditorHandle = await inner(root, options);
    const nodeViews = handle.view.props.nodeViews as Record<
      string,
      (node: ProseMirrorNode) => { dom: HTMLElement; destroy(): void }
    >;
    const construct = nodeViews.ask;
    if (construct === undefined) throw new Error("the ask node view was not installed");
    const writtenMarkdown = handle.setMarkdown;
    handle.setMarkdown = (text: string) => {
      writtenMarkdown(text);
      root.append(construct(askNodeOf(markdown)).dom);
    };
    const writtenDocument = handle.setDocumentJSON;
    handle.setDocumentJSON = (json) => {
      writtenDocument(json);
      root.append(construct(askNodeOf(markdown)).dom);
    };
    return handle;
  };
}

/** Records what the version draws over its rendered document, and gives the fake handle the
 *  document the real editor would hold: the fake view's state carries none (`VersionView.test`).
 *  With `paintMarks`, it also puts a marked block into the editor's root where the version drew
 *  any, as the real editor's decorations would: the head reads the drawn document to decide
 *  whether there is a change to step to. */
function recordMarks(
  markdown: string,
  drawn: ChangeDecorations[],
  { paintMarks = false }: { paintMarks?: boolean } = {}
): (inner: CreateEditor) => CreateEditor {
  return (inner) => async (root, options) => {
    const handle: EditorHandle = await inner(root, options);
    const state = handle.view.state as unknown as { doc: ProseMirrorNode };
    state.doc = renderedDocument(markdown);
    handle.setChangeDecorations = (decorations) => {
      drawn.push(decorations);
      if (!paintMarks || decorations.text.length === 0) return;
      const block = globalThis.document.createElement("p");
      block.className = CHANGED_BLOCK_CLASS;
      block.textContent = "The plan ships behind a canary today.";
      root.append(block);
    };
    return handle;
  };
}

/** Gives the fake editor the blocks a real one would draw, each stamped with its block id as
 *  `@legion/proof-editor` stamps them: the pane reads its section off the drawn document. */
function drawBlocks(blocks: string): (inner: CreateEditor) => CreateEditor {
  return (inner) => async (root, options) => {
    const handle: EditorHandle = await inner(root, options);
    root.innerHTML = blocks;
    return handle;
  };
}

function renderPane({
  ask,
  createEditor,
  seed = () => {},
}: {
  ask: InboxRow;
  createEditor?: (inner: CreateEditor) => CreateEditor;
  seed?: (client: QueryClient) => void;
}) {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  /** Each document the column opened a room for, in the order it opened them. */
  const opened: string[] = [];
  const runtime = {
    ...fake.runtime,
    createEditor:
      createEditor === undefined
        ? fake.runtime.createEditor
        : createEditor(fake.runtime.createEditor),
    loadTransport: async () => {
      const connect = await fake.runtime.loadTransport();
      return (artifactId: string, callbacks: Parameters<typeof connect>[1]) => {
        opened.push(artifactId);
        return connect(artifactId, callbacks);
      };
    },
  };
  const client = createQueryClient();
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  seed(client);
  const answered: string[] = [];
  /** Every registration the head's Snooze made, in order: `[askId, live]`. */
  const snoozeLiveness: (readonly [string, boolean])[] = [];
  /** Every registration a card's hand-on made, in order: `[askId, live]`. */
  const handOnLiveness: (readonly [string, boolean])[] = [];
  const tree = (row: InboxRow) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={runtime}>
          <MarginProvider>
            <AskHandOnLiveContext value={(askId, live) => handOnLiveness.push([askId, live])}>
              <ReadingPane
                onAnswered={(id) => answered.push(id)}
                onSnoozeLive={(askId, live) => snoozeLiveness.push([askId, live])}
                row={{ ask: row, kind: "ask" }}
                viewer="alice"
              />
            </AskHandOnLiveContext>
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(ask));
  return {
    answered,
    client,
    handOnLiveness,
    opened,
    rerender: (row: InboxRow) => view.rerender(tree(row)),
    runtime: fake,
    snoozeLiveness,
    view,
  };
}

function decisionAsk(overrides: Partial<InboxRow> = {}): InboxRow {
  return issueAsk({
    block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
    block_id: "ship_it",
    question: "Ship behind a flag?",
    ...overrides,
  });
}

/** The approval request for `spec.md` at version 4; `approved` is the approval it follows, which
 *  a first request has none of, and `overrides` tells two requests on one document apart. */
function approvalAsk(
  approved?: { at: string; by: { id: string; kind: "user" }; version: number },
  overrides: Partial<InboxRow> = {}
): InboxRow {
  return issueAsk({
    approval: {
      approved,
      artifact_id: "artifact-1",
      documents: null,
      name: "spec.md",
      requested_version: 4,
      version: 4,
    },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec" },
    kind: "approval",
    question: "Approve spec.md at version 4?",
    ...overrides,
  });
}

const EARLIER_APPROVAL = {
  at: "2026-10-01T00:00:00Z",
  by: { id: "alice", kind: "user" as const },
  version: 3,
};

test("a decision block draws the document in pane mode, with Skip to the answer at the head", async () => {
  const { opened, runtime, view } = renderPane({
    ask: decisionAsk(),
    seed: (client) => seedDocument(client, document()),
  });

  try {
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    expect(opened).toEqual(["artifact-1"]);
    runtime.sync();
    await waitFor(() => expect(runtime.editors).toHaveLength(1));
    // What `surface="pane"` means where the editor is built: no presence, and no edits.
    expect(runtime.editors[0]?.options).toMatchObject({ access: "read", awareness: null });
    expect(await screen.findByRole("button", { name: "Skip to the answer" })).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("a decision block's column marks the section from its heading down to the block", async () => {
  const { runtime, view } = renderPane({
    ask: decisionAsk(),
    createEditor: drawBlocks(
      '<h1 data-block-id="title">Plan</h1><h2 data-block-id="rollout">Rollout</h2>' +
        '<p data-block-id="context">How it goes out.</p>' +
        '<section data-block-id="ship_it">Ship behind a flag?</section>'
    ),
    seed: (client) => seedDocument(client, document()),
  });

  try {
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();
    expect(await screen.findByTestId("pane-section-bar")).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("a decision block before any heading sits in no section, so no bar is drawn", async () => {
  const { runtime, view } = renderPane({
    ask: decisionAsk(),
    createEditor: drawBlocks(
      '<p data-block-id="context">How it goes out.</p>' +
        '<section data-block-id="ship_it">Ship behind a flag?</section>' +
        '<h2 data-block-id="rollout">Rollout</h2>'
    ),
    seed: (client) => seedDocument(client, document()),
  });

  try {
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();
    await waitFor(() => expect(runtime.editors).toHaveLength(1));
    await waitFor(() =>
      expect(screen.getByTestId("inbox-pane")).toHaveTextContent("Ship behind a flag?")
    );
    expect(screen.queryByTestId("pane-section-bar")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("an approval draws the latest version marked against the version it follows", async () => {
  await warmMarkdownRenderer();
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n\nThe old note.\n";
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(EARLIER_APPROVAL),
    createEditor: recordMarks(latest, drawn),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "awaiting", version: 3 },
          versions: [version(3), version(4)],
        }),
        { 3: baseline, 4: latest }
      ),
  });

  try {
    expect(await screen.findByRole("button", { name: "Approve" })).toBeDefined();
    expect(screen.getByRole("button", { name: "Request changes" })).toBeDefined();
    // The latest version, which is the version Approve approves.
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    await waitFor(() => expect(drawn).toHaveLength(1));
    const marks = changeMarksAgainst(baselineOf(baseline), renderedDocument(latest));
    expect(marks.text.length).toBeGreaterThan(0);
    expect(drawn[0]).toEqual(marks);
  } finally {
    view.unmount();
  }
});

// A pod of the build before this one answers the baseline version with its markdown and no
// reading, for as long as a rollout takes, and the server builds no tree for an empty text at
// all. Read as a tree alone, such an answer left the approver with an unmarked document and a
// "Next change" control with nothing to step to: the text is the baseline either way.
test("an approval marks the version against a baseline answered with no reading", async () => {
  await warmMarkdownRenderer();
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n\nThe old note.\n";
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(EARLIER_APPROVAL),
    createEditor: recordMarks(latest, drawn, { paintMarks: true }),
    seed: (client) => {
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "awaiting", version: 3 },
          versions: [version(3), version(4)],
        }),
        { 4: latest }
      );
      client.setQueryData(
        ["artifact", "artifact-1", "version", 3],
        versionTextWithoutTree(3, baseline)
      );
    },
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    await waitFor(() => expect(drawn).toHaveLength(1));
    const marks = changeMarksAgainst(baselineWithoutTree(baseline), renderedDocument(latest));
    expect(marks.text.length).toBeGreaterThan(0);
    expect(drawn[0]).toEqual(marks);
    // The marked blocks are what the head offers to step through.
    expect(await screen.findByRole("button", { name: "Next change" })).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("an approval of a document nobody has approved marks nothing and offers no Next change", async () => {
  await warmMarkdownRenderer();
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(),
    createEditor: recordMarks(latest, drawn),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "awaiting" },
          versions: [version(4)],
        }),
        { 4: latest }
      ),
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    await waitFor(() => expect(screen.queryByText("Loading version…")).not.toBeInTheDocument());
    expect(drawn).toEqual([]);
    expect(screen.queryByRole("button", { name: "Next change" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// "Next change" steps the blocks the version marked. The marks are drawn only once the headless
// markdown engine has warmed; an engine that never warms, or one that refuses the baseline,
// leaves `changeMarksAgainst` marking nothing (`change-marks.ts`), and the button then had
// nothing to step and did nothing. These two rows pin the offer on the marks that were drawn
// rather than on a baseline that merely loaded.
test("an approval whose version drew marks offers Next change", async () => {
  await warmMarkdownRenderer();
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(EARLIER_APPROVAL),
    createEditor: recordMarks(latest, drawn, { paintMarks: true }),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "awaiting", version: 3 },
          versions: [version(3), version(4)],
        }),
        { 3: baseline, 4: latest }
      ),
  });

  try {
    expect(await screen.findByRole("button", { name: "Next change" })).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("an approval whose baseline loaded but whose marks never drew offers no Next change", async () => {
  await warmMarkdownRenderer();
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(EARLIER_APPROVAL),
    // The marks are computed and handed over, and nothing of them reaches the drawn document.
    createEditor: recordMarks(latest, drawn),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "awaiting", version: 3 },
          versions: [version(3), version(4)],
        }),
        { 3: baseline, 4: latest }
      ),
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    await waitFor(() => expect(drawn).toHaveLength(1));
    expect(drawn[0]?.text.length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: "Next change" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("an approved document shows the approval it follows in place of the actions, and its version", async () => {
  await warmMarkdownRenderer();
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderPane({
    ask: approvalAsk(EARLIER_APPROVAL),
    createEditor: recordMarks(latest, drawn),
    seed: (client) =>
      seedDocument(
        client,
        document({
          approval: { latest_version: 4, state: "approved", version: 4 },
          versions: [version(3), version(4)],
        }),
        { 3: baseline, 4: latest }
      ),
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
    expect(screen.getByText(/Approved at version 3 on/)).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("an approval draws the request's own question above the document, inside the scroller", async () => {
  await warmMarkdownRenderer();
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const { view } = renderPane({
    ask: approvalAsk(),
    seed: (client) =>
      seedDocument(
        client,
        document({ approval: { latest_version: 4, state: "awaiting" }, versions: [version(4)] }),
        { 4: latest }
      ),
  });

  try {
    // The block is drawn before the document's own reads land, so it is on screen beside the
    // skeleton as well as beside the version.
    const block = await screen.findByTestId("pane-approval-question");
    expect(block).toHaveTextContent("Approve spec.md at version 4?");
    const drawn = await screen.findByLabelText("Document version 4");
    expect(block.compareDocumentPosition(drawn) & Node.DOCUMENT_POSITION_FOLLOWING).toBeGreaterThan(
      0
    );
    // Inside the one scroller, so the document holds the whole column again once the reader
    // has scrolled past the question.
    expect(screen.getByTestId("inbox-pane-scroller").contains(block)).toBe(true);
  } finally {
    view.unmount();
  }
});

test("an approval that falls back to the card draws the question once, as the card's own", async () => {
  // The question is Markdown (`MarkdownBody`), empty until the lazy Markdown engine loads.
  await warmMarkdownRenderer();
  const { view } = renderPane({ ask: approvalAsk() });

  try {
    // `getArtifact` is refused by default, so the column never reads a version to show.
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    expect(screen.queryByTestId("pane-approval-question")).not.toBeInTheDocument();
    expect(screen.getAllByText("Approve spec.md at version 4?")).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

// The one scroller is the column's, not the row's: it outlives every question the reader steps
// through. These four rows pin where an approval request leaves it.

const SECOND_REQUEST: Partial<InboxRow> = {
  id: "ask-b",
  question: "Approve spec.md at version 4, after the rewrite?",
};

test("a second approval request opens the column at its own start, wherever the first was left", async () => {
  await warmMarkdownRenderer();
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  // Both requests are about this one document at this one version, so the column keeps the
  // version it drew and only the question above it changes.
  const { rerender, view } = renderPane({
    ask: approvalAsk(),
    seed: (client) =>
      seedDocument(
        client,
        document({ approval: { latest_version: 4, state: "awaiting" }, versions: [version(4)] }),
        { 4: latest }
      ),
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    const scroller = screen.getByTestId("inbox-pane-scroller");
    // Where this reader read down to, which the next question would otherwise open at.
    scroller.scrollTop = 240;
    expect(scroller.scrollTop).toBe(240);

    rerender(approvalAsk(undefined, SECOND_REQUEST));

    await waitFor(() => expect(scroller.scrollTop).toBe(0));
    expect(await screen.findByTestId("pane-approval-question")).toHaveTextContent(
      "Approve spec.md at version 4, after the rewrite?"
    );
    // The same element throughout: the column was scrolled, not rebuilt.
    expect(screen.getByTestId("inbox-pane-scroller")).toBe(scroller);
  } finally {
    view.unmount();
  }
});

/** A request on a document this harness cannot read, so the column falls back to its card and
 *  writes nothing: what the reader steps through on the way back to the first request. */
const UNREADABLE_REQUEST: Partial<InboxRow> = {
  approval: {
    approved: undefined,
    artifact_id: "artifact-2",
    documents: null,
    name: "runbook.md",
    requested_version: 1,
    version: 1,
  },
  approval_artifact: { name: "runbook.md", primary: false, project: "CORE", slug: "runbook" },
  id: "ask-b",
  question: "Approve runbook.md at version 1?",
};

test("a request the reader comes back to opens at the start of the column again", async () => {
  await warmMarkdownRenderer();
  const latest = "# Plan\n\nThe plan ships behind a canary today.\n";
  const first = approvalAsk();
  const { rerender, view } = renderPane({
    ask: first,
    seed: (client) =>
      seedDocument(
        client,
        document({ approval: { latest_version: 4, state: "awaiting" }, versions: [version(4)] }),
        { 4: latest }
      ),
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    const scroller = screen.getByTestId("inbox-pane-scroller");

    // A request between the two that the column cannot draw: it falls back to the card and
    // writes nothing, and the reader reads down that card.
    rerender(approvalAsk(undefined, UNREADABLE_REQUEST));
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    scroller.scrollTop = 240;

    // The first request again is a request the pane shows anew, so its version is drawn from
    // the start of the column rather than at the offset the card between them left.
    rerender(first);
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    await waitFor(() => expect(scroller.scrollTop).toBe(0));
    expect(screen.getByTestId("inbox-pane-scroller")).toBe(scroller);
  } finally {
    view.unmount();
  }
});

test("a later version of the document under approval leaves the column where the reader is", async () => {
  await warmMarkdownRenderer();
  const four = "# Plan\n\nThe plan ships behind a canary today.\n";
  const five = "# Plan\n\nThe plan ships behind a canary today, and the flag is gone.\n";
  // The version read is answered here rather than seeded, so version 5 is pending for a render
  // and the request's own answer goes and lands again, which is what moves the write's key.
  const landing = Promise.withResolvers<void>();
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockImplementation(
    async (_artifactId: string, number: number) => {
      if (number === 5) {
        await landing.promise;
      }
      return versionText(number, number === 5 ? five : four);
    }
  );
  const { client, view } = renderPane({
    ask: approvalAsk(),
    seed: (seeded) =>
      seedDocument(
        seeded,
        document({ approval: { latest_version: 4, state: "awaiting" }, versions: [version(4)] })
      ),
  });

  try {
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    const scroller = screen.getByTestId("inbox-pane-scroller");
    await waitFor(() => expect(scroller.scrollTop).toBe(0));
    // Where this reader has read down to in the version the request is about.
    scroller.scrollTop = 240;

    // A new version of the document under approval: the version read moves to another key, so
    // its answer is undefined for a render and lands again, for the one request. The column is
    // written when the request changes and not when the version does (R2), so it is written
    // once and the reader keeps the text they were on.
    act(() => {
      client.setQueryData(
        ["artifact", "artifact-1"],
        document({
          approval: { latest_version: 5, state: "awaiting" },
          versions: [version(4), version(5)],
        })
      );
    });
    landing.resolve();

    expect(await screen.findByLabelText("Document version 5")).toBeDefined();
    expect(scroller.scrollTop).toBe(240);
    // The same element throughout: nothing rebuilt the column under the reader.
    expect(screen.getByTestId("inbox-pane-scroller")).toBe(scroller);
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});

test("an approval on a document that cannot be read leaves the column where the reader left it", async () => {
  // `getArtifact` is refused by default, so the column reads no version and falls back to the
  // card: there is no version drawn to put the reader at the start of.
  const { rerender, view } = renderPane({ ask: approvalAsk() });

  try {
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    const scroller = screen.getByTestId("inbox-pane-scroller");
    scroller.scrollTop = 240;

    rerender(approvalAsk(undefined, SECOND_REQUEST));

    await screen.findByText("Approve spec.md at version 4, after the rewrite?");
    expect(scroller.scrollTop).toBe(240);
  } finally {
    view.unmount();
  }
});

test("an approval whose version cannot be read leaves the column where the reader left it", async () => {
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockRejectedValue(
    new Error("VERSION_UNREADABLE")
  );
  // The document reads, and the version it names does not: nothing is seeded under the version
  // key, so both readers of it take the refusal.
  const { rerender, view } = renderPane({
    ask: approvalAsk(),
    seed: (client) =>
      seedDocument(
        client,
        document({ approval: { latest_version: 4, state: "awaiting" }, versions: [version(4)] })
      ),
  });

  try {
    expect(await screen.findByText("Could not load this document version.")).toBeDefined();
    const scroller = screen.getByTestId("inbox-pane-scroller");
    scroller.scrollTop = 240;

    rerender(approvalAsk(undefined, SECOND_REQUEST));

    await screen.findByText("Approve spec.md at version 4, after the rewrite?");
    expect(scroller.scrollTop).toBe(240);
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});

/** The version text the read answers with, as the server answers it: the markdown and the
 *  reading the server made of it, which is what the version view draws rather than parsing the
 *  markdown again. The page's own reading of that text is the same document, so a row that
 *  needs no reading of its own takes this one. */
function versionText(number: number, markdown: string): ArtifactVersionText {
  return {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-10-02T00:00:00Z",
    markdown,
    markdown_tree: treeOf(markdown),
    named: false,
    number,
    summary: null,
  };
}

/** The same answer from a pod of the build before this one: the version's markdown, and no
 *  reading of it at all. The server leaves `markdown_tree` out for a caller it does not read
 *  trees for and for a text it builds none of (`text_trees.go`), so a reader on either side of
 *  a rollout can be answered this way. */
function versionTextWithoutTree(number: number, markdown: string): ArtifactVersionText {
  return { ...versionText(number, markdown), markdown_tree: undefined };
}

/** An approval request whose version read is held, so a row decides when the column can be
 *  written: the question is drawn from the first frame, and the document settles and its own
 *  reads land before the version does. */
async function openingApproval(landing: Promise<void>) {
  // The version's reading (`treeOf`) throws until the lazy Markdown engine loads.
  await warmMarkdownRenderer();
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockImplementation(async () => {
    await landing;
    return versionText(4, "# Plan\n\nThe plan ships behind a canary today.\n");
  });
  const pane = renderPane({
    ask: approvalAsk(),
    seed: (client) =>
      seedDocument(
        client,
        document({ approval: { latest_version: 4, state: "awaiting" }, versions: [version(4)] })
      ),
  });
  return { ...pane, getArtifactVersion };
}

test("an approval request the reader scrolls while it opens keeps the place they moved to", async () => {
  const landing = Promise.withResolvers<void>();
  const { getArtifactVersion, view } = await openingApproval(landing.promise);

  try {
    // The request's own question stands while the version is read, so there is text to scroll.
    expect(await screen.findByTestId("pane-approval-question")).toBeDefined();
    const scroller = screen.getByTestId("inbox-pane-scroller");
    scroller.scrollTop = 240;
    // The reader's own wheel in the column, dispatched rather than driven: user-event has no
    // form for a wheel, and what the hold reads is the event reaching the column.
    scroller.dispatchEvent(new Event("wheel", { bubbles: true }));

    landing.resolve();
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    expect(scroller.scrollTop).toBe(240);
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});

test("an approval request nobody has scrolled opens at the start of the column", async () => {
  const landing = Promise.withResolvers<void>();
  const { getArtifactVersion, view } = await openingApproval(landing.promise);

  try {
    expect(await screen.findByTestId("pane-approval-question")).toBeDefined();
    const scroller = screen.getByTestId("inbox-pane-scroller");
    scroller.scrollTop = 240;

    landing.resolve();
    expect(await screen.findByLabelText("Document version 4")).toBeDefined();
    await waitFor(() => expect(scroller.scrollTop).toBe(0));
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});

test("an approval draws the ask card of a question block in the version it shows", async () => {
  await warmMarkdownRenderer();
  const latest = '# Plan\n\n:::ask{urgency="high"}\nShip behind a flag?\n:::\n';
  const { view } = renderPane({
    ask: approvalAsk(),
    createEditor: drawAskBlock(latest),
    seed: (client) =>
      seedDocument(
        client,
        document({ approval: { latest_version: 4, state: "awaiting" }, versions: [version(4)] }),
        { 4: latest }
      ),
  });

  try {
    const drawn = await screen.findByLabelText("Document version 4");
    // The card the block's own node view hosts, drawn as a stored version draws it: the urgency
    // the block carries, with no composer under it.
    expect(await within(drawn).findByText("Decision")).toBeDefined();
    expect(drawn.querySelector('[data-dispatch-ask-pill="high"]')).not.toBeNull();
  } finally {
    view.unmount();
  }
});

// The same version as the row above, answered the way a pod of the build before this one
// answers it: its markdown, and no reading. The column draws it by parsing that text
// (`VersionView`), and the question block in it is hosted and carded as it is for a drawn
// reading, so an approver mid-rollout reads the decision rather than a message about a document
// the answer carried.
test("an approval draws the ask card of a version answered with no reading", async () => {
  await warmMarkdownRenderer();
  const latest = '# Plan\n\n:::ask{urgency="high"}\nShip behind a flag?\n:::\n';
  const { view } = renderPane({
    ask: approvalAsk(),
    createEditor: drawAskBlock(latest),
    seed: (client) => {
      seedDocument(
        client,
        document({ approval: { latest_version: 4, state: "awaiting" }, versions: [version(4)] })
      );
      client.setQueryData(
        ["artifact", "artifact-1", "version", 4],
        versionTextWithoutTree(4, latest)
      );
    },
  });

  try {
    const drawn = await screen.findByLabelText("Document version 4");
    expect(await within(drawn).findByText("Decision")).toBeDefined();
    expect(drawn.querySelector('[data-dispatch-ask-pill="high"]')).not.toBeNull();
    expect(screen.queryByText("Could not load this document version.")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("an anchored question is today's card", async () => {
  const { opened, view } = renderPane({
    ask: issueAsk({
      anchor: {
        artifact_id: "artifact-1",
        block_id: "ship_it",
        mark_id: "mark-1",
        orphaned: false,
        quote: "behind a flag",
        version: 4,
      },
      anchor_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec" },
    }),
  });

  try {
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    expect(opened).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("a question in no document is today's card", async () => {
  const { opened, view } = renderPane({ ask: issueAsk() });

  try {
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    expect(opened).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("the head's Snooze registers its write, so the list keeps the row the column is on", async () => {
  const user = userEvent.setup();
  const held = Promise.withResolvers<AskSnooze>();
  const snoozeAsk = spyOn(api, "snoozeAsk").mockImplementation(() => held.promise);
  const { snoozeLiveness, view } = renderPane({ ask: issueAsk() });

  try {
    await user.selectOptions(
      await screen.findByLabelText("Snooze CORE-1: Which approach?"),
      "tomorrow"
    );
    // The optimistic snooze folds the row into Later and the column follows the list, so a
    // head that kept its write to itself would be unmounted with the row it wrote about.
    await waitFor(() => expect(snoozeLiveness).toEqual([["ask-a", true]]));
  } finally {
    held.resolve({ snoozed_until: "2026-10-31T09:00:00Z" });
    view.unmount();
    snoozeAsk.mockRestore();
  }
});

test("the column's card registers its hand-on, so the list keeps the row the column is on", async () => {
  const user = userEvent.setup();
  // Handed to alice on an issue bob holds, so handing it back to bob takes it out of her list.
  const row = issueAsk({
    asked_of: "alice",
    issue: { assignee: "bob", key: "CORE-1", title: "Bob's issue" },
  });
  const held = Promise.withResolvers<InboxRow>();
  const handAskTo = spyOn(api, "handAskTo").mockImplementation(() => held.promise);
  // The control reads the people as the reader reaches for it, bob among them.
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([
    { login: "alice" },
    { login: "bob" },
  ]);
  const { handOnLiveness, view } = renderPane({ ask: row });

  try {
    await user.selectOptions(await screen.findByLabelText("Hand this question to"), "bob");
    await waitFor(() => expect(handAskTo).toHaveBeenCalled());
    // The control sits in the card, not in the head, so the registration reaches the list
    // through the context the Inbox provides rather than through a prop of this column's: the
    // optimistic `waiting_on_person` takes the row out of that list as the write starts, and
    // the card that wrote is the only surface a refusal is shown on.
    await waitFor(() => expect(handOnLiveness).toEqual([["ask-a", true]]));
  } finally {
    held.resolve({ ...row, asked_of: null, waiting_on_person: "bob" });
    view.unmount();
    handAskTo.mockRestore();
    listUsers.mockRestore();
  }
});

test("a project document's head names the project and the document, and opens it at the section", async () => {
  const { runtime, view } = renderPane({
    ask: decisionAsk({
      document: { name: "Design notes", project: "CORE", slug: "design-notes" },
      issue: undefined,
      issue_key: null,
    }),
    createEditor: drawBlocks(
      '<h2 data-block-id="rollout">Rollout</h2>' +
        '<section data-block-id="ship_it">Ship behind a flag?</section>'
    ),
    seed: (client) =>
      seedDocument(
        client,
        document({ issue_key: null, name: "Design notes", primary: false, slug: "design-notes" })
      ),
  });

  try {
    expect(await screen.findByText("CORE · Design notes")).toBeDefined();
    // Neither applies to a project document: it has no priority and nobody is assigned to it.
    expect(screen.queryByRole("combobox", { name: /^Priority of/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Assign / })).not.toBeInTheDocument();
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    runtime.sync();
    await waitFor(() =>
      expect(screen.getByRole("link", { name: "Open page" })).toHaveAttribute(
        "href",
        "/projects/CORE/documents/design-notes#b-rollout"
      )
    );
  } finally {
    view.unmount();
  }
});

test("a document that cannot be read falls back to the card, with Open page at the head", async () => {
  getArtifact.mockRejectedValue(new Error("DOCUMENT_UNLOADABLE"));
  const { opened, view } = renderPane({ ask: decisionAsk() });

  try {
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    expect(screen.getByRole("link", { name: "Open page" })).toBeDefined();
    // Never the document page's own repair or rebuild prompt: the column opened no document.
    expect(opened).toEqual([]);
    expect(screen.queryByText(/Rebuild from the latest version/)).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("a document the editor is refused admission to leaves the column on the card, with no prompt", async () => {
  // The column's own read of the text was answered from the cache (seeded below) and the read
  // the document makes for its own admission is the one the server refuses. Two things then say
  // the document cannot be opened: the document's report (`onUnavailable`, which
  // `ProofDocument.admission.test.tsx` covers on its own) and the error that refusal leaves on
  // the text query both columns share. Either way the column ends on the card and draws neither
  // of the page's prompts, which is what this test pins.
  getArtifactText.mockRejectedValue(
    new ApiError(409, { code: "DOC_SCHEMA", error: "This document is outside the Proof schema." })
  );
  const { opened, view } = renderPane({
    ask: decisionAsk(),
    seed: (client) => seedDocument(client, document()),
  });

  try {
    expect(await screen.findByRole("article", { name: "Urgency: Medium" })).toBeDefined();
    expect(screen.getByRole("link", { name: "Open page" })).toBeDefined();
    expect(
      screen.queryByText(/Upload markdown to replace and repair this document/)
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/Rebuild from the latest version/)).not.toBeInTheDocument();
    expect(screen.queryByText(/This document is outside the Proof schema/)).not.toBeInTheDocument();
    // The refusal is what closed the editor: no room was ever opened.
    expect(opened).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("a selection changed twice inside the settle opens the second document alone", async () => {
  // On the fake clock from the first render, so the settle of the first selection is a timer this
  // test moves the clock past rather than a wait of its own.
  jest.useFakeTimers();
  const { opened, rerender, runtime, view } = renderPane({
    ask: decisionAsk({ id: "ask-1" }),
    seed: (client) => {
      seedDocument(client, document());
      seedDocument(client, document({ id: "artifact-2", name: "notes.md", slug: "notes" }));
    },
  });

  try {
    rerender(
      decisionAsk({
        block_artifact: { id: "artifact-2", primary: false, slug: "notes" },
        block_id: "later",
        id: "ask-2",
      })
    );
    await waitFor(() => expect(opened).toEqual(["artifact-2"]));
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    // Long past the settle, the first selection has still opened nothing.
    await tick(SETTLE_MS * 2);
    expect(opened).toEqual(["artifact-2"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// ---------------------------------------------------------------------------------------------
// The four thread bodies: what the column shows for a Reply or an Agent row.
// ---------------------------------------------------------------------------------------------

function threadRow(overrides: Partial<MyReplyThreadRow>): MyReplyThreadRow {
  return {
    kind: "comment",
    read_through: "2026-10-12T00:30:00Z",
    ref: "/issues/CORE-1?comment=comment-1",
    reply: {
      author: { id: "bob", kind: "user" },
      created_at: "2026-10-12T01:00:00Z",
      first_line: "Agreed.",
      id: "comment-2",
      kind: "reply",
    },
    subject: "The retry budget",
    thread_id: "comment-1",
    unread: true,
    unread_count: 1,
    version: 1,
    ...overrides,
  };
}

/** The anchor a comment thread in a project document carries. */
function anchor(overrides: Partial<Anchor> = {}): Anchor {
  return {
    artifact_id: "artifact-1",
    block_id: "b-1",
    mark_id: "mark-1",
    orphaned: false,
    quote: "three attempts",
    version: 2,
    ...overrides,
  };
}

function renderThread(thread: MyReplyRow, seed: (client: QueryClient) => void = () => {}) {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const client = createQueryClient();
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  seed(client);
  const tree = (row: MyReplyRow) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <ReadingPane
              onAnswered={() => {}}
              onSnoozeLive={() => {}}
              row={{ kind: "thread", thread: row }}
              viewer="alice"
            />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(thread));
  return {
    client,
    /** The column moved to another row, as the Inbox moves it: the same column, drawn again. */
    rerender: (row: MyReplyRow) => view.rerender(tree(row)),
    runtime: fake,
    view,
  };
}

/** Which of the column's four thread bodies is drawn. */
async function paneBody(): Promise<HTMLElement> {
  const pane = await screen.findByTestId("inbox-pane");
  const body = pane.querySelector<HTMLElement>("[data-inbox-pane-body]");
  if (body === null) throw new Error("the column drew no thread body");
  return body;
}

test("a comment thread on an issue shows the issue and the thread, with no document", async () => {
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const { view } = renderThread(threadRow({ issue: { key: "CORE-1", title: "Fix the thing" } }));
  try {
    const body = await paneBody();
    expect(body).toHaveAttribute("data-inbox-pane-body", "issue");
    // The head names the owner and opens the thread's own page.
    const pane = screen.getByTestId("inbox-pane");
    expect(within(pane).getByText("CORE-1")).toBeTruthy();
    expect(within(pane).getByRole("link", { name: "Open page" })).toHaveAttribute(
      "href",
      "/issues/CORE-1?comment=comment-1"
    );
    await waitFor(() => expect(listComments).toHaveBeenCalled());
  } finally {
    view.unmount();
    listComments.mockRestore();
  }
});

// The column's subject line prints the thread's root in one line: a root its author deleted has
// no subject left and reads as the deleted line, an edited one carries ` · edited`, and one that
// stands unedited carries neither.
test("a thread's subject line reads as deleted for a deleted root and marks an edited one", async () => {
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const subject = async (overrides: Partial<MyReplyThreadRow>) => {
    const { view } = renderThread(
      threadRow({ issue: { key: "CORE-1", title: "Fix the thing" }, ...overrides })
    );
    try {
      const body = await paneBody();
      return body.querySelector("[data-inbox-thread-subject]")?.textContent;
    } finally {
      view.unmount();
    }
  };
  try {
    expect(await subject({ edited_at: "2026-10-12T00:20:00Z" })).toBe("The retry budget · edited");
    expect(await subject({})).toBe("The retry budget");
    // The deleted root was edited first, so ` · edited` can be drawn, and only the column's own
    // reading of the delete keeps it off.
    const deleted = await subject({
      deleted_at: "2026-10-12T00:40:00Z",
      deleted_by: { id: "alice", kind: "user" },
      edited_at: "2026-10-12T00:20:00Z",
      subject: "",
    });
    expect(deleted).toContain("Deleted by alice");
    expect(deleted).not.toContain("edited");
  } finally {
    listComments.mockRestore();
  }
});

test("an issue message conversation reads the issue's messages", async () => {
  const listIssueMessages = spyOn(api, "listIssueMessages").mockResolvedValue([]);
  const { view } = renderThread(
    threadRow({
      issue: { key: "CORE-1", title: "Fix the thing" },
      kind: "message",
      thread_id: "message-1",
    })
  );
  try {
    expect(await paneBody()).toHaveAttribute("data-inbox-pane-body", "issue");
    await waitFor(() => expect(listIssueMessages).toHaveBeenCalledWith("CORE-1"));
  } finally {
    view.unmount();
    listIssueMessages.mockRestore();
  }
});

test("an Agent row shows the session's name and reads that session's messages", async () => {
  const listAgentMessages = spyOn(api, "listAgentMessages").mockResolvedValue([]);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const { view } = renderThread(plannerAgentRow());
  try {
    expect(await paneBody()).toHaveAttribute("data-inbox-pane-body", "agent");
    const pane = screen.getByTestId("inbox-pane");
    expect(within(pane).getByText("Planner")).toBeTruthy();
    // An Agent row has no reference, so the head draws no copy control at all.
    expect(pane.querySelector("[data-copy-ref]")).toBeNull();
    expect(within(pane).getByRole("link", { name: "Open page" })).toHaveAttribute(
      "href",
      "/agents/planner/live"
    );
    await waitFor(() => expect(listAgentMessages).toHaveBeenCalledWith("planner"));
  } finally {
    view.unmount();
    listAgentMessages.mockRestore();
    listAgents.mockRestore();
  }
});

test("an ask's thread shows the ask's own card", async () => {
  const row = issueAsk({ id: "ask-a" });
  const getAsk = mockAskReads([row]);
  const { view } = renderThread(
    threadRow({
      issue: { key: "CORE-1", title: "Fix the thing" },
      kind: "ask",
      ref: "/issues/CORE-1/asks/ask-a",
      thread_id: "ask-a",
    })
  );
  try {
    expect(await paneBody()).toHaveAttribute("data-inbox-pane-body", "ask");
    await waitFor(() => expect(screen.getByTestId("ask-ask-a")).toBeTruthy());
    expect(getAsk).toHaveBeenCalledWith("ask-a");
  } finally {
    view.unmount();
    getAsk.mockRestore();
  }
});

test("a comment thread in a document opens that document at its block", async () => {
  const listArtifactComments = spyOn(api, "listArtifactComments").mockResolvedValue([]);
  const thread = threadRow({
    anchor: anchor(),
    document: { name: "spec.md", project: "CORE", slug: "spec" },
    ref: "/projects/CORE/documents/spec?comment=comment-1",
  });
  const { runtime, view } = renderThread(thread, (client) => {
    seedDocument(client, document({ issue_key: null, versions: [version(1)] }));
  });
  try {
    expect(await paneBody()).toHaveAttribute("data-inbox-pane-body", "document");
    // The live document is opened, not a card: the column connects a room for it.
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    expect(screen.getByTestId("inbox-pane").querySelector("[data-inbox-pane-fallback]")).toBeNull();
  } finally {
    view.unmount();
    listArtifactComments.mockRestore();
  }
});

test("an orphaned anchor falls back to the thread alone, with its quote", async () => {
  // On the fake clock from before the first render, so the settle the pane starts there is a
  // timer this test moves the clock past rather than a wait of its own.
  jest.useFakeTimers();
  const listArtifactComments = spyOn(api, "listArtifactComments").mockResolvedValue([]);
  const thread = threadRow({
    anchor: anchor({ orphaned: true }),
    document: { name: "spec.md", project: "CORE", slug: "spec" },
    ref: "/projects/CORE/documents/spec?comment=comment-1",
  });
  const { runtime, view } = renderThread(thread, (client) => {
    seedDocument(client, document({ issue_key: null, versions: [version(1)] }));
  });
  try {
    const body = await paneBody();
    expect(body).toHaveAttribute("data-inbox-pane-body", "document");
    expect(body.hasAttribute("data-inbox-pane-fallback")).toBe(true);
    expect(body.querySelector("[data-inbox-thread-quote]")?.textContent).toBe("three attempts");
    // Nothing opened the document: an anchor nothing can be placed at opens no room.
    await tick(SETTLE_MS * 2);
    expect(runtime.connections).toHaveLength(0);
  } finally {
    realClock();
    view.unmount();
    listArtifactComments.mockRestore();
  }
});

// ---------------------------------------------------------------------------------------------
// One column for one thread after another: a write made in one thread's body is that thread's,
// and the next thread is drawn with none of it.
// ---------------------------------------------------------------------------------------------

// The column draws a card for one ask's thread after another. An answer Dispatch refused on the
// first is that ask's: the next ask's card, drawn at once from the thread read it already holds,
// shows no refusal of it, so no Retry that would answer the first ask from the second's screen,
// and the Retry it does show is of its own answer, to its own ask.
test("the next ask's card shows no answer refused on the ask before it, and its Retry answers its own ask", async () => {
  const first = issueAsk({ id: "ask-x" });
  const next = issueAsk({
    id: "ask-y",
    issue: { assignee: "alice", key: "CORE-2", title: "Other issue" },
    issue_key: "CORE-2",
    question: "Which format?",
  });
  const getAsk = mockAskReads([first, next]);
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(
    new ApiError(400, { error: "Dispatch refused this answer." })
  );
  const user = userEvent.setup();
  const { rerender, view } = renderThread(
    threadRow({
      issue: { key: "CORE-1", title: "Fix the thing" },
      kind: "ask",
      ref: "/issues/CORE-1/asks/ask-x",
      thread_id: "ask-x",
      unread: false,
    }),
    // The second ask's thread is read already, as it is once the reader has been on its row, so
    // the column draws its card at once, with no skeleton between the two.
    (client) => client.setQueryData(["ask-thread", next.id], { ask: next, ...next.thread })
  );
  try {
    const card = await screen.findByTestId("ask-ask-x");
    await user.click(within(card).getByLabelText("Your answer"));
    await user.paste("JSON");
    await user.click(within(card).getByRole("button", { name: "Answer" }));
    expect((await within(card).findByRole("alert")).textContent).toContain(
      "Could not save your answer."
    );

    rerender(
      threadRow({
        issue: { key: "CORE-2", title: "Other issue" },
        kind: "ask",
        ref: "/issues/CORE-2/asks/ask-y",
        thread_id: "ask-y",
        unread: false,
      })
    );
    const nextCard = screen.getByTestId("ask-ask-y");
    expect(within(nextCard).queryByRole("alert")).toBeNull();

    await user.click(within(nextCard).getByLabelText("Your answer"));
    await user.paste("YAML");
    await user.click(within(nextCard).getByRole("button", { name: "Answer" }));
    await user.click(await within(nextCard).findByRole("button", { name: "Retry" }));
    await waitFor(() => expect(answerAsk).toHaveBeenCalledTimes(3));
    expect(answerAsk.mock.calls.map(([askId]) => askId)).toEqual(["ask-x", "ask-y", "ask-y"]);
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    getAsk.mockRestore();
  }
});

/** A thread's root comment on `issueKey`, as the issue's comments read answers it. */
function rootComment(id: string, issueKey: string): Comment {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "bob", kind: "user" },
    body: "The retry budget is too small.",
    created_at: "2026-10-12T00:00:00Z",
    edited_at: null,
    id,
    issue_key: issueKey,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** A picture pasted into a reply. */
const shot = new File(["bytes"], "shot.png", { type: "image/png" });

// The column draws a reply box for one comment thread after another. An upload Dispatch refused
// in the first thread's box is that thread's: the next thread's box, drawn at once from the
// comments already read, shows no refusal of it, so no Retry that would upload the file to the
// first thread's issue, and the Retry it does show is of its own upload, to its own issue.
test("the next thread's reply box shows no upload refused in the thread before it, and its Retry uploads to its own issue", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const uploadArtifact = spyOn(api, "uploadArtifact").mockRejectedValue(
    new ApiError(413, { code: "FILE_TOO_LARGE", error: "That file is too large." })
  );
  const { rerender, view } = renderThread(
    threadRow({ issue: { key: "CORE-1", title: "Fix the thing" }, unread: false }),
    (client) => {
      client.setQueryData(["comments", "CORE-1"], [rootComment("comment-1", "CORE-1")]);
      // The second thread's comments are read already, as they are once the reader has been on
      // its row, so the column draws its box at once, with no skeleton between the two.
      client.setQueryData(["comments", "CORE-2"], [rootComment("comment-9", "CORE-2")]);
    }
  );
  try {
    // user-event's paste carries text, never the file this field uploads.
    fireEvent.paste(await screen.findByRole("textbox", { name: "Reply" }), {
      clipboardData: { files: [shot] },
    });
    await within(screen.getByTestId("inbox-pane")).findByRole("alert");

    rerender(
      threadRow({
        issue: { key: "CORE-2", title: "Other issue" },
        ref: "/issues/CORE-2?comment=comment-9",
        thread_id: "comment-9",
        unread: false,
      })
    );
    const pane = screen.getByTestId("inbox-pane");
    expect(pane.querySelector('[data-inbox-thread="comment-9"]')).not.toBeNull();
    expect(within(pane).queryByRole("alert")).toBeNull();

    // user-event's paste carries text, never the file this field uploads.
    fireEvent.paste(within(pane).getByRole("textbox", { name: "Reply" }), {
      clipboardData: { files: [shot] },
    });
    await userEvent.setup().click(await within(pane).findByRole("button", { name: "Retry" }));
    await waitFor(() => expect(uploadArtifact).toHaveBeenCalledTimes(3));
    expect(uploadArtifact.mock.calls.map(([owner]) => owner)).toEqual([
      { issue: "CORE-1" },
      { issue: "CORE-2" },
      { issue: "CORE-2" },
    ]);
  } finally {
    view.unmount();
    listAgents.mockRestore();
    uploadArtifact.mockRestore();
  }
});

// Text typed in a thread's reply box and not sent is that thread's. The column draws the next
// thread's box without it, and back on the first thread its box holds the text again.
test("text typed and not sent in a thread's reply box waits there for the reader's return, and never shows in another thread's", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const first = threadRow({ issue: { key: "CORE-1", title: "Fix the thing" }, unread: false });
  const { rerender, view } = renderThread(first, (client) => {
    client.setQueryData(["comments", "CORE-1"], [rootComment("comment-1", "CORE-1")]);
    // The second thread's comments are read already, as they are once the reader has been on
    // its row, so the column draws its box at once, with no skeleton between the two.
    client.setQueryData(["comments", "CORE-2"], [rootComment("comment-9", "CORE-2")]);
  });
  try {
    const user = userEvent.setup();
    await user.click(await screen.findByRole("textbox", { name: "Reply" }));
    await user.paste("Half a reply");

    rerender(
      threadRow({
        issue: { key: "CORE-2", title: "Other issue" },
        ref: "/issues/CORE-2?comment=comment-9",
        thread_id: "comment-9",
        unread: false,
      })
    );
    const pane = screen.getByTestId("inbox-pane");
    expect(pane.querySelector('[data-inbox-thread="comment-9"]')).not.toBeNull();
    expect(within(pane).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" }).value).toBe(
      ""
    );

    rerender(first);
    expect(pane.querySelector('[data-inbox-thread="comment-1"]')).not.toBeNull();
    expect(within(pane).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" }).value).toBe(
      "Half a reply"
    );
  } finally {
    view.unmount();
    listAgents.mockRestore();
  }
});

/** The Planner session, live, which takes a Send. */
const planner: Agent = {
  capabilities: ["steer", "btw"],
  dir: "/work",
  host: "box",
  last_activity: "2026-10-12T00:00:00Z",
  last_seen: Date.now(),
  machine_id: "box",
  open_asks: 0,
  roles: [],
  session_id: "planner",
  state: "live",
  title: "Planner",
};

/** A message `id` the reader sent the Planner, answered by the session's `reply`. */
function plannerExchange(id: string, reply: string): MessageRead {
  const sent: Message = {
    author: { id: "alice", kind: "user" },
    body: `Question ${id}`,
    broadcast_id: null,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    ...messageCorrectionFields(),
    id,
    in_reply_to: null,
    issue_key: null,
    reply_block: null,
    target: "session:planner",
  };
  return {
    message: sent,
    replies: [
      {
        ...sent,
        author: { id: "planner", kind: "session" },
        body: `Answer ${reply}`,
        created_at: "2026-10-12T01:00:00Z",
        id: reply,
        in_reply_to: id,
      },
    ],
  };
}

// The column draws an Agent row's thread box for one thread after another, and two threads can be
// one session's. A reply Dispatch refused in the first is that thread's: the next thread's box
// shows none of it - no draft, no refusal, no pending turn, no Retry that would send it as a reply
// there - and what that box sends is its own reply, under its own thread's newest message.
test("the next Agent thread of a session shows nothing of a reply refused in the thread before it", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const listAgentMessages = spyOn(api, "listAgentMessages").mockResolvedValue([
    plannerExchange("message-1", "reply-1"),
    plannerExchange("message-2", "reply-2"),
  ]);
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(
      new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused the message" })
    )
    .mockResolvedValue({} as never);
  const first = plannerAgentRow();
  const { rerender, view } = renderThread(first);
  try {
    const user = userEvent.setup();
    const pane = await screen.findByTestId("inbox-pane");
    await user.click(await within(pane).findByRole("textbox", { name: "Comment" }));
    await user.paste("Reply meant for the first");
    await user.click(within(pane).getByRole("button", { name: "Send" }));
    await within(pane).findByText("Couldn't send — Dispatch refused the message");

    rerender(
      agentRow("message-2", {
        ...first,
        reply: { ...first.reply, id: "reply-2" },
        thread_id: "message-2",
        unread_reply_ids: ["reply-2"],
      })
    );
    expect(pane.querySelector('[data-inbox-thread="message-2"]')).not.toBeNull();
    const next = within(pane).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    expect(next.value).toBe("");
    expect(within(pane).queryByRole("alert")).toBeNull();
    expect(within(pane).queryByTestId("agent-send-held")).toBeNull();

    await user.click(next);
    await user.paste("Reply for the second");
    await user.click(within(pane).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(2));
    expect(
      createAgentMessage.mock.calls.map(([sessionId, input]) => [
        sessionId,
        input.in_reply_to,
        input.body,
      ])
    ).toEqual([
      ["planner", "reply-1", "Reply meant for the first"],
      ["planner", "reply-2", "Reply for the second"],
    ]);
  } finally {
    view.unmount();
    listAgents.mockRestore();
    listAgentMessages.mockRestore();
    createAgentMessage.mockRestore();
  }
});
