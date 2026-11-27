import { beforeAll, beforeEach, expect, jest, spyOn, test } from "bun:test";
import type { IssueTreeRead } from "@legion/contracts";
import { QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";
import { MemoryRouter } from "react-router-dom";

import {
  type FakeDocumentRuntime,
  fakeDocumentRuntime as fakeSpecRuntime,
} from "../../__tests__/document-runtime";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { details } from "../../__tests__/issue-picks";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifactRecordHandler,
  createQueryClient,
  documentSurfaceHandlers,
  renderProofDocument as renderSpec,
  artifact as spec,
} from "../../__tests__/proof-document";
import { ApiError, api } from "../../api/client";
import { issueQuery, issueViewOfQuery } from "../../api/queries";
import type {
  Artifact,
  ArtifactText,
  Ask,
  IssueDetails,
  IssueReferenceSummary,
  Version,
} from "../../api/types";
import { ArtifactHeader } from "../artifacts/ArtifactHeader";
import { heldSends } from "../conversation/held-sends";
import { marginComposeSendKey } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { appKeymap } from "../shell/keymap";
import { colorForLogin } from "./connection";
import type { DocumentToolbar } from "./ProofDocument";
import { compiledBlockSchema, viewBlockSchema } from "./schema";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

answerTextReadsWithTheSeededText();

/**
 * Every case of this file opens the issue's spec where an issue's room holds it: in a fragment of
 * the issue's one Yjs document named by the server, which the artifact the page is handed and the
 * artifact read both answer (the issue-tree spec), never `prosemirror`. So each case passes only
 * where the page binds the fragment the server names. The other ProofDocument suites open a spec
 * whose answers name none, as a server without issue rooms answers it.
 */
const ROOM_FRAGMENT = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
const artifact = { ...spec, fragment: ROOM_FRAGMENT };

/** The harness's runtime, its text held in the spec's fragment of the room. */
function fakeDocumentRuntime(seed: { text?: string } = {}): FakeDocumentRuntime {
  return fakeSpecRuntime({ ...seed, fragment: ROOM_FRAGMENT });
}

/** The harness's page, opening the spec in its room. */
function renderProofDocument(options: Parameters<typeof renderSpec>[0] = {}) {
  return renderSpec({
    document: artifact,
    ...options,
    fake: options.fake ?? fakeDocumentRuntime({ text: "The live document" }),
  });
}

/**
 * The document surface's own shared reads (`documentSurfaceHandlers`), the artifact read answering
 * this file's spec with the fragment it names, and the read this file adds to them: a reference
 * link the pointer rests on opens a preview card, which reads the issue the reference names and
 * the documents it holds.
 */
beforeEach(() => {
  answerRequests(
    http.get("/api/v1/issues/:key/summary", ({ params }) =>
      Response.json({
        documents: [
          {
            id: artifact.id,
            kind: artifact.kind,
            latest_version: null,
            name: artifact.name,
            primary: artifact.primary,
            slug: artifact.slug,
          },
        ],
        key: String(params.key),
        project: artifact.project,
        status: "todo",
        title: "Review the spec",
      } satisfies IssueReferenceSummary)
    ),
    artifactRecordHandler((id) => (id === artifact.id ? artifact : undefined)),
    ...documentSurfaceHandlers()
  );
});

test("ProofDocument creates the editor on the synced document as the signed-in user", async () => {
  const { callbacks, connections, editors, sync, toolbar, view } = renderProofDocument();

  try {
    expect(toolbar.current?.connection).toBe("connecting");
    await sync();
    expect(connections).toHaveLength(1);
    // The connection's pending sync compares the fragment the editor binds.
    expect(callbacks[0]?.fragment).toBe(ROOM_FRAGMENT);
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options).toMatchObject({
      access: "edit",
      awareness: connections[0]?.awareness,
      fragment: ROOM_FRAGMENT,
      heatMapMode: "hidden",
      user: { color: colorForLogin("alice"), name: "alice" },
      ydoc: connections[0]?.doc,
    });
    expect(editors[0]?.root.textContent).toContain("The live document");
    expect(editors[0]?.root.getAttribute("aria-label")).toBe("Document editor");
    expect(editors[0]?.root.getAttribute("aria-multiline")).toBe("true");
    expect(editors[0]?.root.getAttribute("role")).toBe("textbox");
  } finally {
    view.unmount();
  }
});

test("ProofDocument binds prosemirror once an artifact read that names no fragment answers", async () => {
  answerRequests(artifactRecordHandler((id) => (id === spec.id ? spec : undefined)));
  const { callbacks, editors, sync, view } = renderSpec({ document: spec });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(callbacks[0]?.fragment).toBe("prosemirror");
    expect(editors[0]?.options.fragment).toBe("prosemirror");
    expect(editors[0]?.root.textContent).toContain("The live document");
  } finally {
    view.unmount();
  }
});

/** The artifact read, answered with `record` (this file's artifact unless named) once `release` is
 *  called; `requested` once the page made it. */
function heldArtifactRead(record: Artifact = artifact): {
  release(): void;
  requested(): boolean;
} {
  const { promise, resolve } = Promise.withResolvers<void>();
  let requested = false;
  answerRequests(
    http.get("/api/v1/artifacts/:id", async () => {
      requested = true;
      await promise;
      return Response.json(record);
    })
  );
  return { release: resolve, requested: () => requested };
}

test("ProofDocument connects nothing before the artifact read answers when the artifact it was handed names no fragment", async () => {
  const read = heldArtifactRead();
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const { view } = renderProofDocument({ document: spec, fake });

  try {
    await waitFor(() => expect(read.requested()).toBe(true));
    await act(async () => {});
    // Nothing the reader types can reach the issue's own fragment, or any other, while the
    // page does not know which fragment is its document.
    expect(fake.callbacks).toEqual([]);
    expect(fake.editors).toEqual([]);

    read.release();
    await waitFor(() => expect(fake.callbacks).toHaveLength(1));
    expect(fake.callbacks[0]?.fragment).toBe(ROOM_FRAGMENT);
    act(() => fake.sync());
    await waitFor(() => expect(fake.editors).toHaveLength(1));
    expect(fake.editors[0]?.options.fragment).toBe(ROOM_FRAGMENT);
    expect(fake.editors[0]?.options.ydoc).toBe(fake.connections[0]?.doc);
    expect(fake.editors[0]?.root.textContent).toContain("The live document");
  } finally {
    read.release();
    view.unmount();
  }
});

for (const [named, bound] of [
  [ROOM_FRAGMENT, ROOM_FRAGMENT],
  [null, "prosemirror"],
] as const) {
  test(`ProofDocument connects at once on ${bound} when the artifact it was handed names ${named === null ? "null" : "it"}`, async () => {
    const read = heldArtifactRead();
    const fake = fakeDocumentRuntime({ text: "The live document" });
    // The generated Artifact holds `fragment` as a name or nothing; an answer naming `null` is the
    // shape the page also reads as naming none, which no field of that type spells.
    const handed = { ...spec, fragment: named } as unknown as Artifact;
    const { view } = renderProofDocument({ document: handed, fake });

    try {
      await waitFor(() => expect(fake.callbacks).toHaveLength(1));
      expect(fake.callbacks[0]?.fragment).toBe(bound);
      act(() => fake.sync());
      await waitFor(() => expect(fake.editors).toHaveLength(1));
      expect(fake.editors[0]?.options.fragment).toBe(bound);
    } finally {
      read.release();
      view.unmount();
    }
  });
}

// The admission reads the text afresh before it connects (`admit`), and that read is the page's
// own read of the text as well: one load of a document reads `/text` once, whether the page
// connects at once on the fragment the artifact it was handed names, or waits for its own
// artifact read to name one.
test("ProofDocument reads the text once for a document whose artifact read names no fragment", async () => {
  const read = heldArtifactRead(spec);
  const getArtifactText = spyOn(api, "getArtifactText");
  const queryClient = createQueryClient();
  const { editors, sync, view } = renderSpec({ document: spec, queryClient });

  try {
    await waitFor(() => expect(read.requested()).toBe(true));
    await act(async () => {});
    read.release();
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.fragment).toBe("prosemirror");
    expect(editors[0]?.root.textContent).toContain("The live document");
    const liveText = queryClient.getQueryData<ArtifactText>(["artifact", spec.id, "text"]);
    expect(liveText?.markdown).toBe("The live document");
    await act(async () => {});
    expect(getArtifactText.mock.calls).toEqual([[spec.id]]);
  } finally {
    read.release();
    view.unmount();
  }
});

test("ProofDocument reads the text once for a document handed with its fragment", async () => {
  const getArtifactText = spyOn(api, "getArtifactText");
  const queryClient = createQueryClient();
  const { editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.fragment).toBe(ROOM_FRAGMENT);
    expect(editors[0]?.root.textContent).toContain("The live document");
    const liveText = queryClient.getQueryData<ArtifactText>(["artifact", artifact.id, "text"]);
    expect(liveText?.markdown).toBe("The live document");
    await act(async () => {});
    expect(getArtifactText.mock.calls).toEqual([[artifact.id]]);
  } finally {
    view.unmount();
  }
});

test("ProofDocument shows a failed artifact read as its load failure and opens no connection", async () => {
  answerRequests(
    http.get("/api/v1/artifacts/:id", () =>
      Response.json({ code: "INTERNAL", error: "the store is down" }, { status: 500 })
    )
  );
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const { toolbar, view } = renderProofDocument({ document: spec, fake });

  try {
    const alert = await within(view.container).findByRole("alert");
    expect(alert.textContent).toBe("This document could not load: the store is down");
    await waitFor(() => expect(toolbar.current?.connection).toBe("failed"));
    expect(fake.callbacks).toEqual([]);
    expect(fake.editors).toEqual([]);
  } finally {
    view.unmount();
  }
});

/** The issue CORE-1, whose spec every case of this file opens, as its read answers it. */
const issueRead: IssueDetails = {
  ...details,
  assignee: null,
  priority: null,
  status: "testing",
  title: "Ship the release",
};

/** A query client holding CORE-1's read, as an issue page holds it before it draws a document,
 *  under `parent`. */
function holdingTheIssue(parent: string | null) {
  const queryClient = createQueryClient();
  queryClient.setQueryData(issueQuery("CORE-1").queryKey, { ...issueRead, parent });
  return queryClient;
}

/** The tree read of `key` as Dispatch answers it, `view_of` naming `viewOf` or null. */
function treeRead(key: string, viewOf: string | null): IssueTreeRead {
  return {
    converted: true,
    cursor: null,
    documents: null,
    issue: key,
    nodes: [],
    tree: null,
    tuples: [],
    version: null,
    view: "spec",
    view_of: viewOf,
  };
}

/** The issue's tree read, answered with `view_of` naming `viewOf` once `release` is called;
 *  `requests` lists each read the page made, by its path and query. */
function heldTreeRead(viewOf: string | null): { release(): void; requests(): string[] } {
  const { promise, resolve } = Promise.withResolvers<void>();
  const requests: string[] = [];
  answerRequests(
    http.get("/api/v1/issues/:key/tree", async ({ params, request }) => {
      const url = new URL(request.url);
      // The page's Decisions read is another read of this route, which this row neither holds
      // nor counts.
      if (url.searchParams.get("view") === "decisions") {
        return Response.json(treeRead(String(params.key), null));
      }
      requests.push(`${url.pathname}${url.search}`);
      await promise;
      return Response.json(treeRead(String(params.key), viewOf));
    })
  );
  return { release: resolve, requests: () => [...requests] };
}

/** The document's text read, answered with the live document's text once `release` is called;
 *  `requested` once the page made it. The harness's spy of the read takes this answer for this
 *  row (`answerTextReadsWithTheSeededText`). */
function heldTextRead(): { release(): void; requested(): boolean } {
  const { promise, resolve } = Promise.withResolvers<ArtifactText>();
  const getArtifactText = spyOn(api, "getArtifactText").mockImplementation(() => promise);
  return {
    release: () =>
      resolve({ markdown: "The live document", token: "sha256:spec-text", version: null }),
    requested: () => getArtifactText.mock.calls.length > 0,
  };
}

/** The issue's own tree read, as the page asks it of this file's spec. */
const SPEC_TREE_READ = "/api/v1/issues/CORE-1/tree?view=spec";

// The admission waits on two replies, the text read and the tree read of `view_of`, and makes the
// editor only once both have answered, so no editor of a view is ever made with the full schema.
// Here the text read answers first.
test("a view's document makes its editor only after its tree read answers view_of, with the view's schema, when the text read answers first", async () => {
  const text = heldTextRead();
  const tree = heldTreeRead("CORE-0");
  const queryClient = createQueryClient();
  const { callbacks, editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await waitFor(() => expect(tree.requests()).toEqual([SPEC_TREE_READ]));
    await waitFor(() => expect(text.requested()).toBe(true));
    text.release();
    await waitFor(() =>
      expect(queryClient.getQueryState(["artifact", artifact.id, "text"])?.status).toBe("success")
    );
    await act(async () => {});
    // The text has answered and the tree read has not, so nothing is connected and no editor is
    // made under any schema.
    expect(callbacks).toEqual([]);
    expect(editors).toEqual([]);

    tree.release();
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(viewBlockSchema);
    expect(tree.requests()).toEqual([SPEC_TREE_READ]);
  } finally {
    text.release();
    tree.release();
    view.unmount();
  }
});

// The same where the tree read answers first and the text read later.
test("a view's document makes its editor only after its text read answers, with the view's schema, when the tree read answers first", async () => {
  const text = heldTextRead();
  const tree = heldTreeRead("CORE-0");
  const queryClient = createQueryClient();
  const { callbacks, editors, sync, view } = renderProofDocument({ queryClient });

  try {
    await waitFor(() => expect(tree.requests()).toEqual([SPEC_TREE_READ]));
    await waitFor(() => expect(text.requested()).toBe(true));
    tree.release();
    await waitFor(() =>
      expect(queryClient.getQueryState(issueViewOfQuery("CORE-1").queryKey)?.status).toBe("success")
    );
    await act(async () => {});
    // The tree read has answered that the issue is a view and the text read has not, so nothing
    // is connected and no editor is made.
    expect(callbacks).toEqual([]);
    expect(editors).toEqual([]);

    text.release();
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(viewBlockSchema);
  } finally {
    text.release();
    tree.release();
    view.unmount();
  }
});

// A tree read that fails leaves the page unable to say which schema the editor may have, so the
// load fails as a failed text read fails it: the reason in place of the editor, and no editor.
// Its text read has answered, and that answer does not admit the document again, so the tree is
// read once.
test("a failed tree read fails the document's load as a failed text read does, and makes no editor", async () => {
  const requests: string[] = [];
  answerRequests(
    http.get("/api/v1/issues/:key/tree", ({ request }) => {
      const url = new URL(request.url);
      // The page's Decisions read is another read of this route, which this row does not fail.
      if (url.searchParams.get("view") === "decisions") {
        return Response.json(treeRead("CORE-1", null));
      }
      requests.push(`${url.pathname}${url.search}`);
      return Response.json({ code: "INTERNAL", error: "the tree read is down" }, { status: 500 });
    })
  );
  const queryClient = createQueryClient();
  const { callbacks, editors, toolbar, view } = renderProofDocument({ queryClient });

  try {
    const alert = await within(view.container).findByRole("alert");
    expect(alert.textContent).toBe("This document could not load: the tree read is down");
    await waitFor(() => expect(toolbar.current?.connection).toBe("failed"));
    await waitFor(() =>
      expect(queryClient.getQueryState(["artifact", artifact.id, "text"])?.status).toBe("success")
    );
    await act(async () => {});
    expect(callbacks).toEqual([]);
    expect(editors).toEqual([]);
    expect(requests).toEqual([SPEC_TREE_READ]);
  } finally {
    view.unmount();
  }
});

// A project document names no issue, so it has no tree to read and is edited under the compiled
// manifest.
test("a document that names no issue makes no tree read, and its editor gets the compiled schema", async () => {
  const tree = heldTreeRead(null);
  tree.release();
  const notes: Artifact = {
    ...artifact,
    id: "artifact-notes",
    issue_key: null,
    name: "notes.md",
    primary: false,
    ref_key: "CORE/notes",
    slug: "notes",
  };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", notes.id], notes);
  queryClient.setQueryData(["artifact", notes.id, "asks"], []);
  const { editors, sync, view } = renderProofDocument({
    document: notes,
    owner: { artifactId: notes.id, kind: "document", project: "CORE", slug: "notes" },
    queryClient,
  });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(compiledBlockSchema);
    expect(tree.requests()).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("a document whose tree read answers view_of null gets the compiled schema", async () => {
  const tree = heldTreeRead(null);
  tree.release();
  const { editors, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(compiledBlockSchema);
    expect(tree.requests()).toEqual([SPEC_TREE_READ]);
  } finally {
    view.unmount();
  }
});

// An issue under no parent is no view (`docs.ViewOf`), so where the page holds the issue's read and
// it names no parent, nothing reads the tree, whatever the tree would answer: here it would answer
// that the issue is a view.
test("a document of an issue the page holds under no parent reads no tree, and its editor gets the compiled schema", async () => {
  const tree = heldTreeRead("CORE-0");
  tree.release();
  const { editors, sync, view } = renderProofDocument({ queryClient: holdingTheIssue(null) });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(compiledBlockSchema);
    expect(tree.requests()).toEqual([]);
  } finally {
    view.unmount();
  }
});

// An issue under a parent may be a view, so its tree is read, and a view's editor is made with the
// view's schema.
test("a document of an issue the page holds under a parent reads its tree, and a view's editor gets the view's schema", async () => {
  const tree = heldTreeRead("CORE-0");
  tree.release();
  const { editors, sync, view } = renderProofDocument({ queryClient: holdingTheIssue("CORE-0") });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(viewBlockSchema);
    expect(tree.requests()).toEqual([SPEC_TREE_READ]);
  } finally {
    view.unmount();
  }
});

// A server of the release before serves no tree read, and answers each of its reads with no route:
// the client sends the read again after each of its waits, and then the read is no view, since
// that release holds none. The document loads with the compiled schema and shows no failure. The
// spec's marks read the issue's decisions, which that server answers from its Decisions read.
test("a child's document on a server with no tree route reads as no view after the client's retries, and its editor loads with the compiled schema", async () => {
  jest.useFakeTimers();
  const requests: string[] = [];
  answerRequests(
    http.get("/api/v1/issues/:key/tree", ({ request }) => {
      const url = new URL(request.url);
      requests.push(`${url.pathname}${url.search}`);
      return Response.json(
        { code: "NOT_FOUND", error: `no route for GET ${url.pathname}` },
        { status: 404 }
      );
    }),
    http.get("/api/v1/issues/:key/decisions", () =>
      Response.json({ decisions: [], document: null, open_questions: [] })
    )
  );
  const queryClient = holdingTheIssue("CORE-0");
  const { editors, sync, view } = renderProofDocument({ queryClient });
  let real = false;

  try {
    // The waits are 1, 2 and 4 seconds, each up to a quarter longer.
    await tick(10_000);
    realClock();
    real = true;
    expect(requests.filter((path) => path === SPEC_TREE_READ)).toHaveLength(4);
    expect(queryClient.getQueryData(issueViewOfQuery("CORE-1").queryKey)).toBeNull();
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(compiledBlockSchema);
    expect(within(view.container).queryByRole("alert")).toBeNull();
  } finally {
    if (!real) realClock();
    view.unmount();
  }
});

// The reader leaves the document while both reads are out: no editor is made, and the replies
// that land after it change nothing.
test("a document left before either read answers makes no editor, and its late replies change nothing", async () => {
  const text = heldTextRead();
  const tree = heldTreeRead("CORE-0");
  const queryClient = createQueryClient();
  const { callbacks, editors, view } = renderProofDocument({ queryClient });
  let left = false;

  try {
    await waitFor(() => expect(tree.requests()).toEqual([SPEC_TREE_READ]));
    await waitFor(() => expect(text.requested()).toBe(true));
    view.unmount();
    left = true;

    text.release();
    tree.release();
    await waitFor(() =>
      expect(queryClient.getQueryState(issueViewOfQuery("CORE-1").queryKey)?.status).toBe("success")
    );
    await waitFor(() =>
      expect(queryClient.getQueryState(["artifact", artifact.id, "text"])?.status).toBe("success")
    );
    await act(async () => {});
    expect(callbacks).toEqual([]);
    expect(editors).toEqual([]);
  } finally {
    text.release();
    tree.release();
    if (!left) view.unmount();
  }
});

test("ProofDocument focuses a block named by the document hash after the editor is ready", async () => {
  const originalHash = window.location.hash;
  window.history.replaceState(null, "", "#b-block-1");
  const { editors, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors[0]?.focusedBlocks).toEqual(["block-1"]));
  } finally {
    window.history.replaceState(null, "", originalHash || "/");
    view.unmount();
  }
});

test("ProofDocument copies a block link whose fragment names the block as encodeBlockId writes it", async () => {
  const originalClipboard = navigator.clipboard;
  const copied: string[] = [];
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: async (text: string) => void copied.push(text) },
  });
  const { editors, sync, toolbar, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");
    editor.selectedBlock = "ship_it";
    expect(await toolbar.current?.copyBlockLink()).toBe(true);
    expect(copied).toEqual([`${window.location.origin}${window.location.pathname}#b-ship%5Fit`]);
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    view.unmount();
  }
});

test("ProofDocument clears the parent's toolbar bag on unmount", () => {
  const { toolbar, view } = renderProofDocument();

  expect(toolbar.current).not.toBeUndefined();
  view.unmount();
  expect(toolbar.current).toBeUndefined();
});

// Dispatch named the version and lost its answer (`409 KEY_COMMITTED`): a landing, not a refusal.
// The dialog closes with no refusal, so a second press names nothing twice, and the document says
// what was made.
test("a version named with its answer lost closes the dialog and says it was made", async () => {
  const user = userEvent.setup();
  const createArtifactVersion = spyOn(api, "createArtifactVersion").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "made; the answer was lost" })
  );
  const { toolbar, view } = renderProofDocument();
  try {
    act(() => toolbar.current?.requestNamedVersion?.());
    const dialog = await screen.findByRole("dialog", { name: "Name this version" });
    await user.click(within(dialog).getByLabelText("What changed?"));
    await user.paste("Settled the rollout");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    const made = await screen.findByTestId("landed-without-answer");
    expect(made.textContent).toBe(
      "The version “Settled the rollout” was made, but Dispatch lost its answer, so it may take a moment to appear."
    );
    expect(screen.queryByRole("dialog", { name: "Name this version" })).not.toBeInTheDocument();
    expect(screen.queryByText("Could not name this version. Try again.")).not.toBeInTheDocument();
    expect(createArtifactVersion).toHaveBeenCalledTimes(1);
  } finally {
    createArtifactVersion.mockRestore();
    view.unmount();
  }
});

// A read of the document asked before a version is named answers the versions from before it.
// Landing after the version's answer, it would take the new version back off the toolbar's list.
test("a read of the document asked before a version is named does not take the version back off the list", async () => {
  const user = userEvent.setup();
  const queryClient = createQueryClient();
  const before = Promise.withResolvers<Artifact>();
  const getArtifact = spyOn(api, "getArtifact")
    .mockResolvedValueOnce(artifact)
    .mockReturnValueOnce(before.promise);
  const named: Version = {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-10T00:00:00Z",
    named: true,
    number: 1,
    summary: "Settled the rollout",
  };
  const answer = Promise.withResolvers<Version>();
  const createArtifactVersion = spyOn(api, "createArtifactVersion").mockReturnValue(answer.promise);
  const { toolbar, view } = renderProofDocument({ queryClient });
  try {
    await waitFor(() =>
      expect(queryClient.getQueryState(["artifact", artifact.id])?.status).toBe("success")
    );
    // A refresh of the document, asked before the version is named and still out.
    await act(async () => {
      void queryClient.invalidateQueries({ exact: true, queryKey: ["artifact", artifact.id] });
      await settle();
    });
    expect(getArtifact).toHaveBeenCalledTimes(2);

    act(() => toolbar.current?.requestNamedVersion?.());
    const dialog = await screen.findByRole("dialog", { name: "Name this version" });
    await user.click(within(dialog).getByLabelText("What changed?"));
    await user.paste("Settled the rollout");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(createArtifactVersion).toHaveBeenCalledTimes(1));
    // The answer gives up the read still out and asks for the document again, after the version
    // is named: Dispatch lists the version then.
    getArtifact.mockResolvedValueOnce({ ...artifact, versions: [named] });
    await act(async () => {
      answer.resolve(named);
      await settle();
    });
    await waitFor(() => expect(toolbar.current?.versions).toEqual([named]));

    // The read answers the document as the server held it before the version was named.
    await act(async () => {
      before.resolve(artifact);
      await settle();
    });
    await waitFor(() =>
      expect(queryClient.getQueryState(["artifact", artifact.id])?.fetchStatus).toBe("idle")
    );
    expect(toolbar.current?.versions).toEqual([named]);
    expect(getArtifact).toHaveBeenCalledTimes(3);
  } finally {
    getArtifact.mockRestore();
    createArtifactVersion.mockRestore();
    view.unmount();
  }
});

test("ProofDocument highlights a routed search term again after route and document changes", async () => {
  class TestHighlight {
    readonly ranges: Range[];

    constructor(...ranges: Range[]) {
      this.ranges = ranges;
    }
  }
  const styleApi = window as unknown as {
    CSS?: { highlights?: Map<string, TestHighlight> };
    Highlight?: typeof TestHighlight;
  };
  const originalCss = Object.getOwnPropertyDescriptor(styleApi, "CSS");
  const originalHighlight = Object.getOwnPropertyDescriptor(styleApi, "Highlight");
  const highlights = new Map<string, TestHighlight>();
  Object.defineProperties(styleApi, {
    CSS: { configurable: true, value: { highlights } },
    Highlight: { configurable: true, value: TestHighlight },
  });
  const { connections, editors, margin, rerender, sync, view } = renderProofDocument({
    fake: fakeDocumentRuntime({ text: "The astrolabe reads altitude." }),
    highlightTerm: "astrolabe",
  });

  try {
    await sync();
    await waitFor(() => expect(margin.current?.documentBridge).toBeDefined());
    await waitFor(() => {
      const highlight = highlights.get("dispatch-search");
      expect(highlight?.ranges.map((range) => range.toString())).toEqual(["astrolabe"]);
    });

    editors[0]?.root.replaceChildren(document.createTextNode("The sextant reads altitude."));
    rerender({ highlightTerm: "sextant" });
    await waitFor(() => {
      const highlight = highlights.get("dispatch-search");
      expect(highlight?.ranges.map((range) => range.toString())).toEqual(["sextant"]);
    });
    const routeHighlight = highlights.get("dispatch-search");

    editors[0]?.root.replaceChildren(document.createTextNode("The compass reads altitude."));
    act(() => connections[0]?.doc.getXmlFragment(ROOM_FRAGMENT).delete(0, 1));
    await waitFor(() => {
      const highlight = highlights.get("dispatch-search");
      expect(highlight).not.toBe(routeHighlight);
      expect(highlight?.ranges).toEqual([]);
    });
  } finally {
    view.unmount();
    if (originalHighlight === undefined) {
      delete styleApi.Highlight;
    } else {
      Object.defineProperty(styleApi, "Highlight", originalHighlight);
    }
    if (originalCss === undefined) {
      delete styleApi.CSS;
    } else {
      Object.defineProperty(styleApi, "CSS", originalCss);
    }
  }
});

test("ProofDocument opens a hover card for a dispatch:// link in the live editor and navigates in-app", async () => {
  const user = userEvent.setup();
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issueRead);
  const { editors, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const root = editors[0]?.root;
    if (root === undefined) throw new Error("editor root missing");
    // The fake runtime never runs Markdown through the real proof-editor serializer (its
    // createEditor stub just dumps a debug string), so this stands in for what that serializer
    // now actually emits for a dispatch:// link mark: href sanitized to "", the real target
    // carried in data-dispatch-href. `doc.e2e.ts` proves the real serializer produces this shape
    // and that clicking it navigates in a genuinely live document.
    act(() => {
      root.innerHTML =
        '<p><a href="" data-dispatch-href="dispatch://CORE-1">dispatch://CORE-1</a></p>';
    });
    const anchor = root.querySelector("a");
    if (anchor === null) throw new Error("anchor missing");
    // Triggers are delegated document-wide, so an anchor inserted after mount is one too:
    // keyboard focus opens the card at once, with the target's title once fetched.
    act(() => {
      anchor.focus();
    });
    const card = await screen.findByRole("tooltip");
    await waitFor(() => expect(card).toHaveTextContent("Ship the release"));
    expect(anchor).toHaveAttribute("aria-describedby", card.id);
    act(() => {
      anchor.blur();
    });
    expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();

    await user.click(anchor);
    expect(within(view.container).getByTestId("current-route").textContent).toBe("/issues/CORE-1");
  } finally {
    getIssue.mockRestore();
    view.unmount();
  }
});

// A link's target is a node its author named, so a click on a link to a document whose name is
// past the bound text is held to (2683 characters, which a new upload's reference key still
// holds) opens that document in-app, rather than leaving the browser to follow the sanitized
// mark's empty href.
test("ProofDocument opens an explicit link to a long-named document in-app", async () => {
  const user = userEvent.setup();
  const slug = "a".repeat(2683);
  const { editors, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const root = editors[0]?.root;
    if (root === undefined) throw new Error("editor root missing");
    // What the proof-editor serializer emits for a dispatch:// link mark (the test above says why).
    act(() => {
      root.innerHTML = `<p><a href="" data-dispatch-href="dispatch://CORE-1/artifact/${slug}">design</a></p>`;
    });
    const anchor = root.querySelector("a");
    if (anchor === null) throw new Error("anchor missing");

    await user.click(anchor);
    expect(within(view.container).getByTestId("current-route").textContent).toBe(
      `/issues/CORE-1/artifacts/${slug}`
    );
  } finally {
    view.unmount();
  }
});

test("ProofDocument reports connection status and pending edits through the toolbar bag", async () => {
  const { editors, pending, rerender, status, sync, toolbar, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => status("connected"));
    await waitFor(() => expect(toolbar.current?.connection).toBe("connected"));
    act(() => {
      pending({
        count: 2,
        readOnly: false,
        rebuiltAt: undefined,
        stored: true,
      });
      status("offline");
    });
    await waitFor(() =>
      expect(toolbar.current?.pending).toEqual({
        count: 2,
        readOnly: false,
        rebuiltAt: undefined,
        stored: true,
      })
    );
    await waitFor(() => expect(toolbar.current?.connection).toBe("offline"));
    rerender({ isClosed: true });
    expect(editors[0]?.access).toBe("comment");
    expect(within(view.container).getByRole("article")).toHaveAttribute("data-read-only", "true");
    expect(
      within(view.container).getByText("This issue is closed. Its document is read-only.")
    ).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("the margin's wait for this browser's edits is answered by a report taken after it, and by its bound", async () => {
  const { margin, pending, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(margin.current?.documentBridge).toBeDefined());
    const bridge = margin.current?.documentBridge;
    if (bridge === undefined) throw new Error("expected a registered document");

    // The mark the margin has just written is an update of its own, and the store counts a local
    // update only once its IndexedDB write is through. So the report that says this browser holds
    // nothing has to be one taken after the wait began: the last report before it was taken
    // without that mark in it, and on a quiet document it says nothing pending, which would send
    // the retry straight back into the refusal it is recovering from.
    act(() =>
      pending({
        count: 0,
        readOnly: false,
        rebuiltAt: undefined,
        stored: true,
      })
    );
    let answered = false;
    const waiting = bridge.whenSent(5000).then(() => {
      answered = true;
    });
    await Promise.resolve();
    expect(answered).toBe(false);

    act(() =>
      pending({
        count: 1,
        readOnly: false,
        rebuiltAt: undefined,
        stored: true,
      })
    );
    await Promise.resolve();
    expect(answered).toBe(false);

    act(() =>
      pending({
        count: 0,
        readOnly: false,
        rebuiltAt: undefined,
        stored: true,
      })
    );
    await waiting;
    expect(answered).toBe(true);

    // A read-only admission cannot send the row it holds, and a row another tab holds is not
    // this page's to send either: the bound is what ends the wait, and the margin sends again
    // whatever it answers.
    act(() =>
      pending({
        count: 1,
        readOnly: true,
        rebuiltAt: undefined,
        stored: true,
      })
    );
    await bridge.whenSent(0);
  } finally {
    view.unmount();
  }
});

test("ProofDocument keeps the live editor mounted while a version is shown and destroys it once", async () => {
  const versionedArtifact = {
    ...artifact,
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T00:00:00Z",
        named: false,
        number: 1,
        summary: null,
      },
    ],
  };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "Version one",
    named: false,
    number: 1,
    summary: null,
  });
  const { connections, editors, rerender, sync, view } = renderProofDocument({
    document: versionedArtifact,
    queryClient,
  });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    rerender({ version: 1 });
    await within(view.container).findByTestId("version-view");
    expect(
      view.container
        .querySelector("section[aria-label='Document editor'] article")
        ?.hasAttribute("hidden")
    ).toBe(true);
    expect(editors[0]?.destroyed).toBe(false);
  } finally {
    view.unmount();
  }
  expect(editors[0]?.destroyed).toBe(true);
  expect(connections[0]?.destroyed).toBe(true);
});

test("ProofDocument compares a selected version with the current settled text when showDiff is true", async () => {
  const document = {
    ...artifact,
    versions: [
      {
        authors: [{ id: "alice", kind: "user" as const }],
        created_at: "2026-09-09T00:00:00Z",
        named: false,
        number: 1,
        summary: null,
      },
    ],
  };
  const queryClient = createQueryClient();
  queryClient.setQueryData(["artifact", artifact.id, "text"], {
    markdown: "Use Postgres",
    version: 2,
  });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "Use SQLite",
    named: false,
    number: 1,
    summary: null,
  });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const { sync, view } = renderProofDocument({ document, queryClient, showDiff: true, version: 1 });

  try {
    await sync();
    await waitFor(() => {
      const diff = within(view.container).getByTestId("change-view");
      expect(diff.querySelector("del")?.textContent).toBe("SQLite");
      expect(diff.querySelector("ins")?.textContent).toBe("Postgres");
    });
  } finally {
    view.unmount();
  }
});

test("ProofDocument shows the current document when Version changes back to Current", async () => {
  const queryClient = createQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(["artifact", artifact.id, "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "Historical document",
    named: false,
    number: 1,
    summary: null,
  });
  const { rerender, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    rerender({ version: 1 });
    await within(view.container).findByTestId("version-view");
    rerender({ version: undefined });
    await waitFor(() =>
      expect(within(view.container).getByRole("article")).toHaveTextContent("The live document")
    );
  } finally {
    view.unmount();
  }
});

test("the block reference gutter filters the margin and focuses its block", async () => {
  const user = userEvent.setup();
  const queryClient = createQueryClient();
  queryClient.setQueryData(
    ["artifact", artifact.id, "blocks"],
    [
      {
        from: 0,
        id: "block-1",
        references: { asks: 1, comments: 1 },
        to: 17,
        type: "paragraph",
      },
    ]
  );
  const { editors, margin, sync, view } = renderProofDocument({ queryClient });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await user.click(
      await within(view.container).findByRole("button", { name: "2 references on block" })
    );
    expect(editors[0]?.focusedBlocks).toEqual(["block-1"]);
    expect(margin.current?.blockFilterId).toBe("block-1");
  } finally {
    view.unmount();
  }
});

/** The issue CORE-1's tree artifact, as the artifact read answers it on a converted issue: the
 *  issue's room, of kind `tree`, whose own fragment holds the nodes written into the issue rather
 *  than into a document (`createIssueTree` in api/artifacts.go). */
const treeArtifact: Artifact = {
  ...spec,
  fragment: "prosemirror",
  id: "artifact-tree",
  kind: "tree",
  name: "tree",
  primary: false,
  ref_key: "CORE-1/tree",
  slug: "tree",
};

/** Every request the page sent through `fetch` from here on, as its path and query and the status
 *  it was answered: the page's client calls `fetch` at each request (`createApiClient`), so the spy
 *  sees each one and the answer the network gave it. Blocks reads are answered as the server
 *  answers them, a document's with its blocks and a tree artifact's `400 NOT_DOCUMENT`
 *  (`getArtifactBlocks` in api/artifacts.go). */
function watchedRequests(): { answered(): Promise<string[]>; restore(): void } {
  answerRequests(
    http.get("/api/v1/artifacts/:id/blocks", ({ params }) =>
      params.id === treeArtifact.id
        ? Response.json(
            { code: "NOT_DOCUMENT", error: "artifact is not a document" },
            { status: 400 }
          )
        : Response.json([])
    )
  );
  const fetches = spyOn(globalThis, "fetch");
  return {
    async answered() {
      const statuses = await Promise.all(
        fetches.mock.results.map(
          async (result) => (await (result.value as Promise<Response>)).status
        )
      );
      return fetches.mock.calls.map(([input], index) => {
        const url = new URL(String(input), window.location.href);
        return `${url.pathname}${url.search} ${statuses[index]}`;
      });
    },
    restore: () => fetches.mockRestore(),
  };
}

// The blocks route answers a document alone, so the editor of an issue's tree artifact reads no
// blocks of it, and its load sends no read the server refuses.
test("the editor of a converted issue's tree artifact sends no blocks read and no read answered 400", async () => {
  answerRequests(
    artifactRecordHandler((id) => (id === treeArtifact.id ? treeArtifact : undefined))
  );
  const requests = watchedRequests();
  const { editors, sync, view } = renderProofDocument({
    document: treeArtifact,
    fake: fakeSpecRuntime({ fragment: "prosemirror", text: "The issue's own nodes" }),
    queryClient: holdingTheIssue(null),
  });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.fragment).toBe("prosemirror");
    await act(async () => {});
    const answered = await requests.answered();
    const treeBlocks = `/api/v1/artifacts/${treeArtifact.id}/blocks`;
    expect(answered.filter((line) => line.startsWith(treeBlocks))).toEqual([]);
    expect(answered.filter((line) => line.endsWith(" 400"))).toEqual([]);
  } finally {
    requests.restore();
    view.unmount();
  }
});

// A document of the same room reads its blocks, once, for the reference gutter.
test("the editor of a converted issue's spec reads its blocks once", async () => {
  const requests = watchedRequests();
  const { editors, sync, view } = renderProofDocument({ queryClient: holdingTheIssue(null) });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await act(async () => {});
    const answered = await requests.answered();
    const specBlocks = `/api/v1/artifacts/${artifact.id}/blocks`;
    expect(answered.filter((line) => line.startsWith(specBlocks))).toEqual([`${specBlocks} 200`]);
  } finally {
    requests.restore();
    view.unmount();
  }
});

/** A document of CORE-1's room beside its spec: not the issue's primary spec, whose title the
 *  page offers no correction of (`correctionIsOffered`), and awaiting its approval, as the
 *  artifact read answers a document's (the server answers no approval of a tree artifact,
 *  `approvalTargets` in api/reviews.go). */
const roomDocument: Artifact = {
  ...artifact,
  approval: { latest_version: 1, state: "awaiting" },
  id: "artifact-notes",
  name: "notes.md",
  primary: false,
  ref_key: "CORE-1/notes-md",
  slug: "notes-md",
};

/** The controls of a document's page that write through a route answering a document alone,
 *  which refuses an issue's tree artifact: naming a version (`createNamedVersion`,
 *  api/artifacts.go), the upload that repairs a stored tree outside the Proof schema, under the
 *  artifact's name (`storeArtifact`, api/artifacts.go, which finds the tree artifact by that name
 *  and refuses it `400 ARTIFACT_KIND_MISMATCH`), a rebuild of a history that cannot load
 *  (`rebuildArtifact`, api/artifact_rebuild.go), a correction of the title (`renameArtifact`,
 *  api/artifact_rename.go), and the review of an approval it awaits (`recordArtifactReview`,
 *  api/reviews.go). The others refuse it `400 NOT_DOCUMENT`. */
const DOCUMENT_ONLY_CONTROLS = [
  "Name version",
  "Upload",
  "Rebuild from the latest version",
  "Correct title",
  "Approve",
  "Request changes",
] as const;

/** The two refusals of an admission the page of a document offers a write for, each with its
 *  words and the control of DOCUMENT_ONLY_CONTROLS it offers: a stored tree outside the Proof
 *  schema, repaired by an upload, and a history that cannot load, rebuilt. */
const WRITABLE_REFUSALS = [
  {
    code: "DOC_SCHEMA",
    error: "the stored document is outside the Proof schema",
    offer: "Upload",
  },
  {
    code: "DOCUMENT_UNLOADABLE",
    error: "the document's stored history cannot load",
    offer: "Rebuild from the latest version",
  },
] as const;

/** The editor of `document`, whose text read the server refuses with `refusal`, beside the
 *  document header (`ArtifactHeader`) drawn with the toolbar the editor reports: the page a host
 *  that mounts the editor of `document` builds. `ArtifactRoutePanel` builds it for a document and
 *  draws a tree artifact as a blob, with no editor. Answers what the editor's alert says and which
 *  of DOCUMENT_ONLY_CONTROLS the page offers. */
async function documentPage(
  document: Artifact,
  fragment: string,
  refusal: (typeof WRITABLE_REFUSALS)[number]
): Promise<{ alert: string; controls: string[] }> {
  answerRequests(artifactRecordHandler((id) => (id === document.id ? document : undefined)));
  // The harness's spy of the text read (`answerTextReadsWithTheSeededText`), answering this row.
  spyOn(api, "getArtifactText").mockRejectedValue(
    new ApiError(409, { code: refusal.code, error: refusal.error })
  );
  const queryClient = holdingTheIssue(null);
  const { toolbar, view } = renderProofDocument({
    document,
    fake: fakeSpecRuntime({ fragment, text: "" }),
    queryClient,
  });
  try {
    const alert = await within(view.container).findByRole("alert");
    await waitFor(() => expect(toolbar.current).toBeDefined());
    const reported: DocumentToolbar | undefined = toolbar.current;
    const header = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <ArtifactHeader
            artifact={document}
            highlight={false}
            showVersionPicker
            toolbar={reported}
            version={undefined}
          >
            {null}
          </ArtifactHeader>
        </QueryClientProvider>
      </MemoryRouter>
    );
    try {
      return {
        alert: alert.textContent ?? "",
        controls: DOCUMENT_ONLY_CONTROLS.filter(
          (name) => screen.queryByRole("button", { name }) !== null
        ),
      };
    } finally {
      header.unmount();
    }
  } finally {
    view.unmount();
  }
}

// An issue's tree artifact reads and edits as a document, and every other route of a document
// refuses it: its page offers no control that writes through one. A stored tree outside the
// schema and a history that cannot load are each the load's failure there, said in the alert,
// with no repair and no rebuild.
test("the page of a converted issue's tree artifact offers no control its document routes refuse", async () => {
  for (const refusal of WRITABLE_REFUSALS) {
    const page = await documentPage(treeArtifact, "prosemirror", refusal);
    expect({ code: refusal.code, controls: page.controls }).toEqual({
      code: refusal.code,
      controls: [],
    });
    expect(page.alert).toContain(refusal.error);
  }
});

// The same page of a document of the room offers each of them, the repair or the rebuild as its
// text read is refused.
test("the page of a document of a converted issue's room offers each control of a document", async () => {
  for (const refusal of WRITABLE_REFUSALS) {
    const page = await documentPage(roomDocument, ROOM_FRAGMENT, refusal);
    expect({ code: refusal.code, controls: page.controls }).toEqual({
      code: refusal.code,
      controls: ["Name version", refusal.offer, "Correct title", "Approve", "Request changes"],
    });
  }
});

test("a margin block focus request focuses and pulses the block once", async () => {
  const { editors, margin, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => margin.current?.focusBlock("block-1"));
    await waitFor(() => expect(editors[0]?.focusedBlocks).toContain("block-1"));
    // The editor is reached on the next frame; one frame more lets any later request through.
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    expect(editors[0]?.focusedBlocks).toEqual(["block-1"]);
  } finally {
    view.unmount();
  }
});

test("ProofDocument links to the current version when a historic version is unavailable", async () => {
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockRejectedValue(
    new Error("version not found")
  );
  const { view } = renderProofDocument({ version: 9 });

  try {
    await within(view.container).findByText("No version 9 of spec.md.");
    expect(
      within(view.container).getByRole("link", { name: "View current version" })
    ).toHaveAttribute("href", "/issues/CORE-1/spec");
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});

test("a selection-bar action opens the margin composer for the mark and settles the library promise", async () => {
  const queryClient = createQueryClient();
  const { editors, margin, sync, view } = renderProofDocument({ queryClient });
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const openCompose = () => {
    const compose = margin.current?.composes.get(artifact.id);
    if (compose?.open === undefined) throw new Error("expected an open compose");
    return { ...compose.open, seq: compose.seq };
  };
  /** Sends the compose the margin has open, as its composer's Send does: from then on the
   *  compose is its send's, and the library's promise resolves. */
  const sendOpenCompose = () => {
    const { anchor, kind } = openCompose();
    heldSends(queryClient).send(marginComposeSendKey(anchor.artifact), {
      anchor,
      ask: { multiple: false, options: [], urgency: "med" },
      draft: { body: "Why?", replacement: "" },
      edit: undefined,
      kind,
      owner: { issueKey: "CORE-1", kind: "issue" },
      plainDelivery: "steer",
      replyTo: null,
      sendKey: "send-1",
    });
  };

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const onMarkAction = editors[0]?.options.onMarkAction;
    if (onMarkAction === undefined) {
      throw new Error("The editor did not receive an onMarkAction handler.");
    }

    let comment: Promise<void> | undefined;
    act(() => {
      comment = Promise.resolve(
        onMarkAction({
          from: 11,
          kind: "comment",
          markId: "m-9",
          placement: { blockId: "b-1", prefix: "The quick ", readings: 1, suffix: " fox" },
          quote: "brown",
          to: 16,
        })
      );
      void comment.catch(() => {});
    });
    if (comment === undefined) {
      throw new Error("The comment action did not return a promise.");
    }
    expect(margin.current?.composes.get(artifact.id)?.open).toMatchObject({
      anchor: {
        artifact: artifact.id,
        mark_id: "m-9",
        placement: { blockId: "b-1", prefix: "The quick ", readings: 1, suffix: " fox" },
        quote: "brown",
      },
      kind: "comment",
    });
    act(() => sendOpenCompose());
    await expect(comment).resolves.toBeUndefined();
    // The send lands before the next selection-bar action, which would otherwise wait on it.
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    let suggest: Promise<void> | undefined;
    act(() => {
      suggest = Promise.resolve(
        onMarkAction({
          from: 11,
          kind: "suggest",
          markId: "m-10",
          placement: null,
          quote: "brown",
          to: 16,
        })
      );
      void suggest.catch(() => {});
    });
    if (suggest === undefined) {
      throw new Error("The suggestion action did not return a promise.");
    }
    expect(margin.current?.composes.get(artifact.id)?.open?.kind).toBe("suggestion");
    act(() => sendOpenCompose());
    await expect(suggest).resolves.toBeUndefined();
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));

    let ask: Promise<void> | undefined;
    act(() => {
      ask = Promise.resolve(
        onMarkAction({
          from: 11,
          kind: "ask",
          markId: "m-11",
          placement: null,
          quote: "brown",
          to: 16,
        })
      );
      void ask.catch(() => {});
    });
    if (ask === undefined) {
      throw new Error("The ask action did not return a promise.");
    }
    expect(margin.current?.composes.get(artifact.id)?.open?.kind).toBe("ask");
    act(() => margin.current?.cancelCompose(artifact.id, openCompose().seq));
    await expect(ask).rejects.toThrow("composer closed");

    let unsupported: unknown;
    try {
      const outcome = onMarkAction({ kind: "resolve", markId: "m-9" });
      if (outcome instanceof Promise) {
        void outcome.catch(() => {});
      }
    } catch (error) {
      unsupported = error;
    }
    expect(unsupported).toEqual(
      new Error("Dispatch renders mark threads in the margin; popover action resolve cannot fire")
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// A closed issue's document takes no mark, so the bar's Comment there reports the selection, and
// the margin's composer names the text by quote and occurrence, which the server anchors.
test("a closed document's Comment opens a quote composer with no mark, which posts the quote and its occurrence", async () => {
  const schema = new Schema({
    nodes: {
      doc: { content: "paragraph+" },
      paragraph: { content: "text*", group: "block" },
      text: {},
    },
  });
  // "brown" twice: the reader selects the second (positions 11 to 16).
  const document = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [schema.text("brown fox brown")]),
  ]);
  const fake = fakeDocumentRuntime({ text: "brown fox brown" });
  const removed: string[] = [];
  const createEditor = fake.runtime.createEditor;
  fake.runtime.createEditor = async (root, options) => {
    const handle = await createEditor(root, options);
    // The fake view's state is a plain object whose `doc` the test supplies.
    const state = handle.view.state as unknown as { doc: ProseMirrorNode };
    state.doc = document;
    handle.removeMark = (markId) => void removed.push(markId);
    return handle;
  };
  const queryClient = createQueryClient();
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const { editors, margin, sync, view } = renderProofDocument({
    fake,
    isClosed: true,
    queryClient,
  });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options).toMatchObject({ access: "comment" });
    const onMarkAction = editors[0]?.options.onMarkAction;
    if (onMarkAction === undefined) {
      throw new Error("The editor did not receive an onMarkAction handler.");
    }

    let cancelled: Promise<void> | undefined;
    act(() => {
      cancelled = Promise.resolve(onMarkAction({ from: 11, kind: "quote-comment", to: 16 }));
      void cancelled.catch(() => {});
    });
    const compose = margin.current?.composes.get(artifact.id);
    expect(compose?.open).toMatchObject({
      anchor: { artifact: artifact.id, occurrence: 1, quote: "brown" },
      kind: "comment",
    });
    expect(compose?.open?.anchor.mark_id).toBeUndefined();
    // Ending it unsent takes nothing out of the document: there is no mark.
    act(() => margin.current?.cancelCompose(artifact.id, compose?.seq ?? -1));
    await expect(cancelled).rejects.toThrow("composer closed");
    expect(removed).toEqual([]);

    let sent: Promise<void> | undefined;
    act(() => {
      sent = Promise.resolve(onMarkAction({ from: 11, kind: "quote-comment", to: 16 }));
    });
    const { anchor } = margin.current?.composes.get(artifact.id)?.open ?? {};
    if (anchor === undefined) throw new Error("expected an open compose");
    act(() =>
      heldSends(queryClient).send(marginComposeSendKey(anchor.artifact), {
        anchor,
        ask: { multiple: false, options: [], urgency: "med" },
        draft: { body: "After the close", replacement: "" },
        edit: undefined,
        kind: "comment",
        owner: { issueKey: "CORE-1", kind: "issue" },
        plainDelivery: "steer",
        replyTo: null,
        sendKey: "send-quote-comment",
      })
    );
    await expect(sent).resolves.toBeUndefined();
    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          anchor: { artifact: artifact.id, occurrence: 1, quote: "brown" },
          body: "After the close",
        },
        expect.objectContaining({ key: "send-quote-comment" })
      )
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// The bar offers a closed document's Comment only for a selection that holds text; should the
// host's own reading of a selection still find none, it tells the reader so, opens no composer and
// rejects nothing into the bar. The next Comment that opens one takes the notice away.
test("a closed document's Comment on a selection with no text to quote says so and opens no composer", async () => {
  const schema = new Schema({
    nodes: {
      doc: { content: "paragraph+" },
      image: { group: "inline", inline: true },
      paragraph: { content: "inline*", group: "block" },
      text: { group: "inline" },
    },
  });
  // "brown " then an image: the image alone is positions 7 to 8.
  const document = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [schema.text("brown "), schema.node("image")]),
  ]);
  const fake = fakeDocumentRuntime({ text: "brown " });
  const createEditor = fake.runtime.createEditor;
  fake.runtime.createEditor = async (root, options) => {
    const handle = await createEditor(root, options);
    // The fake view's state is a plain object whose `doc` the test supplies.
    const state = handle.view.state as unknown as { doc: ProseMirrorNode };
    state.doc = document;
    return handle;
  };
  const { editors, margin, sync, view } = renderProofDocument({ fake, isClosed: true });
  const notice = "This selection holds no text to quote. Select text to comment on it.";

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const onMarkAction = editors[0]?.options.onMarkAction;
    if (onMarkAction === undefined) {
      throw new Error("The editor did not receive an onMarkAction handler.");
    }

    let refused: Promise<void> | undefined;
    act(() => {
      refused = Promise.resolve(onMarkAction({ from: 7, kind: "quote-comment", to: 8 }));
    });
    await expect(refused).resolves.toBeUndefined();
    expect(within(view.container).getByRole("status").textContent).toBe(notice);
    expect(margin.current?.composes.get(artifact.id)?.open).toBeUndefined();

    act(() => {
      void Promise.resolve(onMarkAction({ from: 1, kind: "quote-comment", to: 6 })).catch(() => {});
    });
    expect(margin.current?.composes.get(artifact.id)?.open).toMatchObject({
      anchor: { artifact: artifact.id, occurrence: 0, quote: "brown" },
      kind: "comment",
    });
    expect(within(view.container).queryByText(notice)).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a highlight click focuses its margin item and hover identifies its matching margin item", async () => {
  const { editors, margin, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const onMarkClick = editors[0]?.options.onMarkClick;
    const onMarkHover = editors[0]?.options.onMarkHover;
    if (onMarkClick === undefined || onMarkHover === undefined) {
      throw new Error("The editor did not receive mark interaction handlers.");
    }
    const span = document.createElement("span");
    span.dataset.id = "m-9";
    editors[0]?.root.replaceChildren(span);

    act(() => onMarkClick("m-9"));
    expect(margin.current?.focusRequest?.markId).toBe("m-9");

    act(() => onMarkHover("m-9"));
    expect(margin.current?.hoveredMarkId).toBe("m-9");
    act(() => onMarkHover(null));
    expect(margin.current?.hoveredMarkId).toBeUndefined();

    act(() => margin.current?.documentBridge?.focusMark("m-9"));
    expect(editors[0]?.focused).toEqual(["m-9"]);
  } finally {
    view.unmount();
  }
});

test("the marks projection reaches the editor on sync and on every change", async () => {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const projected: { metadata: object; options: { hydrateAnchors?: boolean } | undefined }[] = [];
  const createEditor = fake.runtime.createEditor;
  fake.runtime.createEditor = async (root, options) => {
    const handle = await createEditor(root, options);
    const applyRemoteMarks = handle.applyRemoteMarks.bind(handle);
    handle.applyRemoteMarks = (metadata, applyOptions) => {
      projected.push({ metadata, options: applyOptions });
      applyRemoteMarks(metadata, applyOptions);
    };
    return handle;
  };
  const { connections, editors, sync, view } = renderProofDocument({ fake });
  const storedMark = {
    by: "user:alice",
    kind: "comment",
    replies: [],
    resolved: false,
    text: "why?",
  };

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => connections[0]?.doc.getMap("marks").set("c-1", storedMark));
    await waitFor(() =>
      expect(projected.at(-1)).toEqual({
        metadata: { "c-1": storedMark },
        options: { hydrateAnchors: false },
      })
    );
  } finally {
    view.unmount();
  }
});

test("mark placements are published to the margin after document changes", async () => {
  const schema = new Schema({
    marks: {
      dispatchAsk: { attrs: { by: {}, id: {} } },
      proofComment: { attrs: { by: {}, id: {} } },
    },
    nodes: {
      doc: { content: "paragraph+" },
      paragraph: { content: "inline*", group: "block" },
      text: { group: "inline" },
    },
  });
  const comment = schema.marks.proofComment.create({ by: "alice", id: "c-1" });
  const ask = schema.marks.dispatchAsk.create({ by: "bob", id: "a-1" });
  const initialDocument = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [schema.text("The "), schema.text("quick", [comment])]),
  ]);
  const changedDocument = schema.node("doc", undefined, [
    schema.node("paragraph", undefined, [schema.text("The "), schema.text("quick", [ask])]),
  ]);
  const fake = fakeDocumentRuntime({ text: "The live document" });
  let editorState: { doc: ProseMirrorNode } | undefined;
  const createEditor = fake.runtime.createEditor;
  fake.runtime.createEditor = async (root, options) => {
    const handle = await createEditor(root, options);
    editorState = handle.view.state as unknown as { doc: ProseMirrorNode };
    editorState.doc = initialDocument;
    handle.markOffsets = () =>
      new Map([
        ["c-1", 40],
        ["a-1", 72],
      ]);
    return handle;
  };
  const { connections, editors, margin, sync, view } = renderProofDocument({ fake });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    await waitFor(() =>
      expect(margin.current?.markPlacements.get("c-1")).toEqual({ pos: 5, top: 40 })
    );

    if (editorState === undefined) {
      throw new Error("The editor state was not captured.");
    }
    editorState.doc = changedDocument;
    act(() => connections[0]?.doc.getXmlFragment(ROOM_FRAGMENT).delete(0, 1));
    await waitFor(() => {
      expect(margin.current?.markPlacements.get("c-1")).toBeUndefined();
      expect(margin.current?.markPlacements.get("a-1")).toEqual({ pos: 5, top: 72 });
    });
  } finally {
    view.unmount();
  }
});

/** Presses Control+Alt+M on `target` through the application's registry, as `KeymapProvider`'s
 *  window listener does, and answers whether a binding claimed it. */
function pressCommentChord(target: Element): boolean {
  const event = new KeyboardEvent("keydown", {
    altKey: true,
    bubbles: true,
    cancelable: true,
    code: "KeyM",
    ctrlKey: true,
    key: "m",
  });
  target.dispatchEvent(event);
  appKeymap.handleKeyDown(event);
  return event.defaultPrevented;
}

test("Ctrl+Alt+M comments on the selection while the editor has focus, and nowhere else", async () => {
  const { editors, rerender, sync, view } = renderProofDocument();
  const composer = document.body.appendChild(document.createElement("textarea"));

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("no editor");
    expect(
      appKeymap.describe().filter((entry) => entry.id === "comment-on-selection")
    ).toMatchObject([{ keys: ["$mod+Alt+m"], label: "Comment on selection", scope: "document" }]);
    editor.root.contentEditable = "true";
    editor.root.tabIndex = 0;
    editor.root.focus();

    // A caret, not a selection: the bar offers no Comment, so neither does the chord.
    expect(pressCommentChord(editor.root)).toBe(false);
    editor.selectionEmpty = false;
    expect(pressCommentChord(editor.root)).toBe(true);
    expect(editor.commented).toBe(1);

    // Focus in a comment composer: the key is the composer's, and the chord does nothing.
    composer.focus();
    expect(pressCommentChord(composer)).toBe(false);
    expect(editor.commented).toBe(1);

    // A closed issue's document is read-only and cannot hold focus, so the chord stays out of it;
    // the bar's own Comment is how a reader comments there.
    editor.root.focus();
    rerender({ isClosed: true });
    expect(pressCommentChord(editor.root)).toBe(false);
    expect(editor.commented).toBe(1);
  } finally {
    composer.remove();
    view.unmount();
  }
  expect(appKeymap.describe().some((entry) => entry.id === "comment-on-selection")).toBe(false);
});

// ---------------------------------------------------------------------------------------------
// `surface="pane"`: the same live document drawn in a reading column beside a list (the Inbox's).
// It takes no edits, publishes no presence, and claims none of the page-level singletons.
// ---------------------------------------------------------------------------------------------

const openDecision: Ask = {
  anchor: null,
  answer: null,
  author: { id: "01a086ad", kind: "session", origin: { session_title: "Architect for CORE-1" } },
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
  block_id: "ship_it",
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [{ label: "Ship" }, { label: "Hold" }],
  question: "Should we ship?",
  state: "open",
  urgency: "high",
};

test("a pane's editor is read-only and publishes no presence, where the page's takes edits and does", async () => {
  const pane = renderProofDocument({ surface: "pane" });
  try {
    await pane.sync();
    await waitFor(() => expect(pane.editors).toHaveLength(1));
    expect(pane.editors[0]?.options).toMatchObject({ access: "read", awareness: null });
    expect(pane.editors[0]?.access).toBe("read");
  } finally {
    pane.view.unmount();
  }

  const page = renderProofDocument();
  try {
    await page.sync();
    await waitFor(() => expect(page.editors).toHaveLength(1));
    expect(page.editors[0]?.options).toMatchObject({
      access: "edit",
      awareness: page.connections[0]?.awareness,
    });
    expect(page.editors[0]?.access).toBe("edit");
  } finally {
    page.view.unmount();
  }
});

test("a read-only admission and a close each leave a pane read-only", async () => {
  const { admit, editors, rerender, sync, view } = renderProofDocument({ surface: "pane" });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    act(() => admit(false));
    expect(editors[0]?.access).toBe("read");
    act(() => rerender({ isClosed: true }));
    expect(editors[0]?.access).toBe("read");
    act(() => rerender({ isClosed: false }));
    expect(editors[0]?.access).toBe("read");
  } finally {
    view.unmount();
  }
});

test("a pane registers no document with the margin, reports no toolbar and shows no Open-decisions navigator", async () => {
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([
    { ...openDecision, last_reply: null },
  ]);
  const paneClient = createQueryClient();
  paneClient.setQueryData(["asks", "CORE-1"], [openDecision]);
  const pane = renderProofDocument({ queryClient: paneClient, surface: "pane" });
  try {
    await pane.sync();
    await waitFor(() => expect(pane.editors).toHaveLength(1));
    expect(pane.margin.current?.documentBridge).toBeUndefined();
    expect(pane.toolbarChanges).toEqual([]);
    expect(
      within(pane.view.container).queryByRole("navigation", { name: "Open decisions" })
    ).not.toBeInTheDocument();
  } finally {
    pane.view.unmount();
  }
  // Not even the unmount's "there is no toolbar now", which would clear a host's own controls.
  expect(pane.toolbarChanges).toEqual([]);

  // The same document on its own page claims all three.
  const pageClient = createQueryClient();
  pageClient.setQueryData(["asks", "CORE-1"], [openDecision]);
  const page = renderProofDocument({ queryClient: pageClient });
  try {
    await page.sync();
    await waitFor(() => expect(page.margin.current?.documentBridge).toBeDefined());
    expect(page.toolbar.current).not.toBeUndefined();
    expect(
      await within(page.view.container).findByRole("navigation", { name: "Open decisions" })
    ).toBeDefined();
  } finally {
    page.view.unmount();
    listIssueAsks.mockRestore();
  }
});

test("a pane writes no dispatch-search highlight, where the page writes one for the same term", async () => {
  class TestHighlight {
    readonly ranges: Range[];

    constructor(...ranges: Range[]) {
      this.ranges = ranges;
    }
  }
  const styleApi = window as unknown as {
    CSS?: { highlights?: Map<string, TestHighlight> };
    Highlight?: typeof TestHighlight;
  };
  const originalCss = Object.getOwnPropertyDescriptor(styleApi, "CSS");
  const originalHighlight = Object.getOwnPropertyDescriptor(styleApi, "Highlight");
  const highlights = new Map<string, TestHighlight>();
  Object.defineProperties(styleApi, {
    CSS: { configurable: true, value: { highlights } },
    Highlight: { configurable: true, value: TestHighlight },
  });

  try {
    const pane = renderProofDocument({
      fake: fakeDocumentRuntime({ text: "The astrolabe reads altitude." }),
      highlightTerm: "astrolabe",
      surface: "pane",
    });
    try {
      await pane.sync();
      await waitFor(() => expect(pane.editors).toHaveLength(1));
      // Each of the three ways a page writes the registry: the bind, a routed term, a document
      // change.
      pane.rerender({ highlightTerm: "altitude" });
      act(() => pane.connections[0]?.doc.getXmlFragment(ROOM_FRAGMENT).delete(0, 1));
      expect(highlights.has("dispatch-search")).toBe(false);
    } finally {
      pane.view.unmount();
    }

    const page = renderProofDocument({
      fake: fakeDocumentRuntime({ text: "The astrolabe reads altitude." }),
      highlightTerm: "astrolabe",
    });
    try {
      await page.sync();
      await waitFor(() =>
        expect(highlights.get("dispatch-search")?.ranges.map(String)).toEqual(["astrolabe"])
      );
    } finally {
      page.view.unmount();
    }
  } finally {
    if (originalHighlight === undefined) {
      delete styleApi.Highlight;
    } else {
      Object.defineProperty(styleApi, "Highlight", originalHighlight);
    }
    if (originalCss === undefined) {
      delete styleApi.CSS;
    } else {
      Object.defineProperty(styleApi, "CSS", originalCss);
    }
  }
});

test("a pane pushes no document keymap scope and registers no binding of its own", async () => {
  const { editors, sync, view } = renderProofDocument({ surface: "pane" });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(appKeymap.describe().some((entry) => entry.id === "comment-on-selection")).toBe(false);
  } finally {
    view.unmount();
  }
});

/** A block id as the editor mints one, and the first eight characters of it, which is how a
 *  person or an agent writes it short. */
const WHOLE_BLOCK = "2a7f1c90-7d5e-4a21-9b3c-6f0e5d4c3b2a";
const OTHER_BLOCK = "2a7f1c90-0000-4000-8000-000000000000";
const SHORT_BLOCK = "2a7f1c90";

/** The fake editor draws no blocks of its own, so a test that links to one draws them: these
 *  are the `[data-block-id]` elements a short id is resolved against and `focusBlock` looks a
 *  whole one up in. */
function runtimeWithBlocks(blockIds: readonly string[]): FakeDocumentRuntime {
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const createEditor = fake.runtime.createEditor;
  return {
    ...fake,
    runtime: {
      ...fake.runtime,
      createEditor: async (root, options) => {
        const handle = await createEditor(root, options);
        root.innerHTML = blockIds.map((id) => `<p data-block-id="${id}">a block</p>`).join("");
        return handle;
      },
    },
  };
}

test("ProofDocument focuses the whole block a hash names short and replaces the address with it", async () => {
  const originalHash = window.location.hash;
  window.history.replaceState(null, "", `#b-${SHORT_BLOCK}`);
  const entries = window.history.length;
  const { editors, sync, view } = renderProofDocument({ fake: runtimeWithBlocks([WHOLE_BLOCK]) });

  try {
    await sync();
    await waitFor(() => expect(editors[0]?.focusedBlocks).toEqual([WHOLE_BLOCK]));
    // The address carries the whole id from here on, and it replaced the short one rather than
    // leaving a second entry in the reader's history.
    expect(window.location.hash).toBe(`#b-${WHOLE_BLOCK}`);
    expect(window.history.length).toBe(entries);
  } finally {
    window.history.replaceState(null, "", originalHash || "/");
    view.unmount();
  }
});

test("ProofDocument focuses a whole block id as written, resolving nothing against the document", async () => {
  const originalHash = window.location.hash;
  window.history.replaceState(null, "", `#b-${WHOLE_BLOCK}`);
  // No block of this document is drawn: a whole id is focused as the hash wrote it, as it was
  // before a short one could be written.
  const { editors, sync, view } = renderProofDocument();

  try {
    await sync();
    await waitFor(() => expect(editors[0]?.focusedBlocks).toEqual([WHOLE_BLOCK]));
    expect(window.location.hash).toBe(`#b-${WHOLE_BLOCK}`);
  } finally {
    window.history.replaceState(null, "", originalHash || "/");
    view.unmount();
  }
});

test("ProofDocument focuses nothing for a short block id two blocks of the document carry", async () => {
  const originalHash = window.location.hash;
  window.history.replaceState(null, "", `#b-${SHORT_BLOCK}`);
  const { editors, sync, view } = renderProofDocument({
    fake: runtimeWithBlocks([WHOLE_BLOCK, OTHER_BLOCK]),
  });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    // Two blocks start with it, so it names neither; the address stays as the reader wrote it.
    expect(editors[0]?.focusedBlocks).toEqual([]);
    expect(window.location.hash).toBe(`#b-${SHORT_BLOCK}`);
  } finally {
    window.history.replaceState(null, "", originalHash || "/");
    view.unmount();
  }
});

// INFRA-869: a `#id` names a question of the text's own document, so a reader who follows one
// from the margin stays on the page already open. The editor binds once; the block focus
// follows the address from then on.
test("a hash change on the page already open focuses the block it names, whole and short", async () => {
  const originalHash = window.location.hash;
  window.history.replaceState(null, "", "/");
  const { connections, editors, sync, view } = renderProofDocument({
    fake: runtimeWithBlocks([WHOLE_BLOCK]),
  });

  const follow = async (fragment: string) => {
    await act(async () => {
      window.history.replaceState(null, "", fragment);
      window.dispatchEvent(new Event("hashchange"));
    });
  };

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    // The page was opened at no block, so the bind focused none.
    expect(editors[0]?.focusedBlocks).toEqual([]);

    await follow(`#b-${WHOLE_BLOCK}`);
    await waitFor(() => expect(editors[0]?.focusedBlocks).toEqual([WHOLE_BLOCK]));

    // A short id names the one block of this document it starts, and the address carries the
    // whole one from then on.
    await follow(`#b-${SHORT_BLOCK}`);
    await waitFor(() => expect(editors[0]?.focusedBlocks).toEqual([WHOLE_BLOCK, WHOLE_BLOCK]));
    expect(window.location.hash).toBe(`#b-${WHOLE_BLOCK}`);

    // One editor on one connection throughout: following the address binds nothing again.
    expect(editors).toHaveLength(1);
    expect(connections).toHaveLength(1);
  } finally {
    window.history.replaceState(null, "", originalHash || "/");
    view.unmount();
  }
});
