import { afterEach, beforeAll, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { realClock, tick } from "../../__tests__/fake-clock";
import { api } from "../../api/client";
import type { Artifact, IssueDetails, MyReplyThreadRow } from "../../api/types";
import type { CreateEditor, EditorHandle } from "../doc/editor";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { installInboxApiMocks } from "./inbox-fixture";
import { ThreadPane } from "./ThreadPane";

/**
 * The reading pane over a comment on an issue's document (the in-document pane, R3): the
 * pane reads the document only once the selection has stood still, reads it once, and once the
 * section is placed brings the comment's own mark into view without moving the focus.
 */

// The thread's subject draws through the shared renderer, whose Markdown engine loads lazily,
// and the rows run in random order, so the file loads the engine before its first row.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

installInboxApiMocks();

/** How long the pane waits for the selection to stand still (`DOCUMENT_SETTLE_MS`). */
const SETTLE_MS = 200;

let getArtifact: Mock<typeof api.getArtifact>;
let getArtifactText: Mock<typeof api.getArtifactText>;
let getIssue: Mock<typeof api.getIssue>;
let listComments: Mock<typeof api.listComments>;

beforeEach(() => {
  getArtifact = spyOn(api, "getArtifact").mockImplementation(async (id) => spec(id));
  // The document's own admission reads its text again, past the pane's cached read.
  getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "",
    token: "",
    version: null,
  });
  // The pane reads one field of the issue: whether it is closed, which it hands the document.
  getIssue = spyOn(api, "getIssue").mockResolvedValue({
    closed_at: null,
  } as unknown as IssueDetails);
  listComments = spyOn(api, "listComments").mockResolvedValue([]);
});

afterEach(() => {
  getArtifact.mockRestore();
  getArtifactText.mockRestore();
  getIssue.mockRestore();
  listComments.mockRestore();
});

/** A document of issue CORE-1. */
function spec(id: string): Artifact {
  return {
    created_at: "2026-10-01T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: "CORE-1",
    kind: "doc",
    name: id === "artifact-1" ? "spec.md" : "notes.md",
    primary: id === "artifact-1",
    project: "CORE",
    ref_key: `CORE-1/${id}`,
    session_id: null,
    slug: id === "artifact-1" ? "spec" : "notes",
    versions: [],
  };
}

/** A comment thread on issue CORE-1, anchored to "three attempts" in its spec. The row carries
 *  `issue` and no `document`, as the server sends it for a thread of an issue. */
function issueThread(overrides: Partial<MyReplyThreadRow> = {}): MyReplyThreadRow {
  return {
    anchor: {
      artifact_id: "artifact-1",
      block_id: "budget",
      mark_id: "mark-1",
      orphaned: false,
      quote: "three attempts",
      version: 1,
    },
    issue: { key: "CORE-1", title: "Retry budget" },
    kind: "comment",
    read_through: "2026-10-12T00:30:00Z",
    ref: "/issues/CORE-1/comments/comment-1",
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

/** The same issue's second thread, anchored in its other document. */
const NOTES_THREAD = issueThread({
  anchor: {
    artifact_id: "artifact-2",
    block_id: "lease",
    mark_id: "mark-2",
    orphaned: false,
    quote: "one lease",
    version: 1,
  },
  ref: "/issues/CORE-1/comments/comment-3",
  thread_id: "comment-3",
});

/** What the pane draws over its document, under its own section. `budget` sits far below the
 *  column's foot once `rollout` is at its top (`layOutColumn`). */
const DRAWN_DOCUMENT =
  '<h2 data-block-id="rollout">Rollout</h2>' +
  '<p data-block-id="budget">Each lease gets <span data-id="mark-1">three attempts</span>.</p>';

/** Draws `DRAWN_DOCUMENT` in the editor as the real one draws its blocks, and records each set of
 *  marks the document lights (`setActiveMarks`). The fake editor's view holds no schema to
 *  install the anchored block's node views on, and it builds no node view anyway, so the slot
 *  the thread is drawn in is left out: what these rows read is the column, not the slot. */
function drawDocument(active: string[][]): (inner: CreateEditor) => CreateEditor {
  return (inner) => async (root, options) => {
    const handle: EditorHandle = await inner(root, { ...options, anchoredBlock: undefined });
    handle.setActiveMarks = (markIds) => {
      active.push([...markIds]);
    };
    root.innerHTML = DRAWN_DOCUMENT;
    return handle;
  };
}

/** Where the column's box, the section's heading and the comment's mark stand, in pixels. */
const COLUMN_HEIGHT = 300;
const HEADING_AT = 1000;
const MARK_AT = 2000;
const MARK_HEIGHT = 20;

/**
 * Lays the column out as happy-dom does not: its box `COLUMN_HEIGHT` tall at the top of the
 * window, the heading `HEADING_AT` and the mark `MARK_AT` down its content, each moved by the
 * column's `scrollTop`. Every other element keeps happy-dom's own box. Answers the function that
 * gives happy-dom its own boxes back.
 */
function layOutColumn(): () => void {
  const real = Element.prototype.getBoundingClientRect;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const scrolled =
      document.querySelector<HTMLElement>('[data-testid="inbox-pane-scroller"]')?.scrollTop ?? 0;
    if (this.matches('[data-testid="inbox-pane-scroller"]')) {
      return new DOMRect(0, 0, 400, COLUMN_HEIGHT);
    }
    if (this.matches('[data-block-id="rollout"]')) {
      return new DOMRect(0, HEADING_AT - scrolled, 400, 24);
    }
    if (this.matches('[data-id="mark-1"]')) {
      return new DOMRect(0, MARK_AT - scrolled, 120, MARK_HEIGHT);
    }
    return real.call(this);
  });
  return () => rect.mockRestore();
}

/** Every value the column's `scrollTop` is written, in order. */
function recordScrollWrites(column: HTMLElement): number[] {
  const writes: number[] = [];
  let value = 0;
  Object.defineProperty(column, "scrollTop", {
    configurable: true,
    get: () => value,
    set: (next: number) => {
      value = next;
      writes.push(next);
    },
  });
  return writes;
}

function renderPane(
  thread: MyReplyThreadRow,
  createEditor?: (inner: CreateEditor) => CreateEditor
) {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const runtime =
    createEditor === undefined
      ? fake.runtime
      : { ...fake.runtime, createEditor: createEditor(fake.runtime.createEditor) };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  // Every read the document makes but the one this file counts, `GET /api/v1/artifacts/{id}`,
  // answered from the cache under the document page's own keys.
  for (const id of ["artifact-1", "artifact-2"]) {
    client.setQueryData(["artifact", id, "text"], { markdown: "", version: null });
    client.setQueryData(["artifact", id, "blocks"], []);
    client.setQueryData(["artifact", id, "asks"], []);
  }
  client.setQueryData(["asks", "CORE-1"], []);
  const tree = (row: MyReplyThreadRow) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={runtime}>
          <MarginProvider>
            {/* The list beside the pane, which holds the focus while the reader walks it. */}
            <button type="button">The selected row</button>
            <ThreadPane thread={row} />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(thread));
  return {
    rerender: (row: MyReplyThreadRow) => view.rerender(tree(row)),
    runtime: fake,
    view,
  };
}

test("a row left within the settle time reads no document", async () => {
  // On the fake clock from before the first render, so the settle is a timer this row moves the
  // clock past rather than a wait of its own.
  jest.useFakeTimers();
  const { rerender, view } = renderPane(issueThread());
  try {
    await tick(SETTLE_MS / 2);
    rerender(NOTES_THREAD);
    await tick(SETTLE_MS * 2);
    await waitFor(() => expect(getArtifact).toHaveBeenCalledWith("artifact-2"));
    // The first row was left before the pane settled on it, so its document was never read.
    expect(getArtifact.mock.calls).toEqual([["artifact-2"]]);
  } finally {
    realClock();
    view.unmount();
  }
});

test("a settled row reads its document once", async () => {
  jest.useFakeTimers();
  const held = Promise.withResolvers<Artifact>();
  getArtifact.mockImplementation(() => held.promise);
  const { view } = renderPane(issueThread());
  try {
    await tick(SETTLE_MS - 50);
    expect(getArtifact).not.toHaveBeenCalled();
    await tick(SETTLE_MS);
    expect(getArtifact.mock.calls).toEqual([["artifact-1"]]);
    // While the read is out, the pane says which document it is opening: the issue's, since the
    // anchor names no document.
    expect(screen.getByRole("status", { name: "Opening CORE-1's document" })).toBeDefined();
    await act(async () => {
      held.resolve(spec("artifact-1"));
      await held.promise;
    });
    await tick(SETTLE_MS * 2);
    expect(screen.queryByRole("status", { name: "Opening CORE-1's document" })).toBeNull();
    expect(getArtifact.mock.calls).toEqual([["artifact-1"]]);
  } finally {
    realClock();
    view.unmount();
  }
});

test("the mark comes into view after the section is placed", async () => {
  const active: string[][] = [];
  const restoreLayout = layOutColumn();
  const { runtime, view } = renderPane(issueThread(), drawDocument(active));
  try {
    const writes = recordScrollWrites(screen.getByTestId("inbox-pane-scroller"));
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    act(() => runtime.sync());
    await waitFor(() => expect(writes.length).toBeGreaterThanOrEqual(2));
    // First the section: the heading at the column's top.
    expect(writes[0]).toBe(HEADING_AT);
    // Then the mark, which that left far below the column's foot, brought up to the foot.
    const markInView = MARK_AT + MARK_HEIGHT - COLUMN_HEIGHT;
    expect(writes[1]).toBe(markInView);
    const scroller = screen.getByTestId("inbox-pane-scroller");
    await waitFor(() => expect(scroller.scrollTop).toBe(markInView));
    const mark = view.container.querySelector('[data-id="mark-1"]');
    if (mark === null) throw new Error("the document drew no mark");
    const box = mark.getBoundingClientRect();
    expect(box.top).toBeGreaterThanOrEqual(0);
    expect(box.bottom).toBeLessThanOrEqual(COLUMN_HEIGHT);
    // The mark the column brought into view is the one the document lights.
    expect(active).toContainEqual(["mark-1"]);
  } finally {
    view.unmount();
    restoreLayout();
  }
});

test("bringing the mark into view moves no focus", async () => {
  const restoreLayout = layOutColumn();
  const { runtime, view } = renderPane(issueThread(), drawDocument([]));
  try {
    const row = screen.getByRole("button", { name: "The selected row" });
    act(() => row.focus());
    expect(document.activeElement).toBe(row);
    const writes = recordScrollWrites(screen.getByTestId("inbox-pane-scroller"));
    await waitFor(() => expect(runtime.connections).toHaveLength(1));
    act(() => runtime.sync());
    await waitFor(() => expect(writes).toContain(MARK_AT + MARK_HEIGHT - COLUMN_HEIGHT));
    expect(document.activeElement).toBe(row);
  } finally {
    view.unmount();
    restoreLayout();
  }
});
