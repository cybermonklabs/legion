import { beforeAll, expect, jest, test } from "bun:test";
import type { ChangeDecorations, DocumentJSON } from "@legion/proof-editor";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import type { Node as ProseMirrorNode } from "prosemirror-model";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime, markedText } from "../../__tests__/document-runtime";
import { realClock, tick } from "../../__tests__/fake-clock";
import type { AuthenticatedUser, DisplayTree } from "../../api/types";
import { loadedEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import { type ChangeBaseline, changeMarksAgainst } from "./change-marks";
import type { CreateEditor, EditorHandle } from "./editor";
import { embedHighlight } from "./highlight";
import { DocumentRuntime } from "./runtime";
import { VersionView } from "./VersionView";

// Every row seeds the version the way the server answers for it: its markdown and the reading
// Go made of that text. The page's own engine reads it into the same document, so the engine is
// loaded before the first row builds a tree.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/** The signed-in person the view draws the editor for, as `/auth/whoami` answers. */
const signedIn: AuthenticatedUser = { event_head: "7", kind: "user", login: "alice" };

function renderVersionView({
  changeBaseline,
  createEditor,
  highlight,
  markdown,
  markdownTree,
  whoAmI = signedIn,
}: {
  changeBaseline?: ChangeBaseline;
  createEditor?: (inner: CreateEditor) => CreateEditor;
  highlight: { by: string; id: string; quote: string } | undefined;
  markdown: string;
  /** The server's reading of `markdown`, when a row needs the one it built itself; the page's
   *  own reading of that text otherwise, and `null` for an answer that carries no reading at
   *  all, which is what a pod of the build before this one sends. */
  markdownTree?: DisplayTree | null;
  /** The answer `/auth/whoami` holds before the first render; `null` leaves the key unset, for a
   *  row where the person arrives after the version's text. */
  whoAmI?: AuthenticatedUser | null;
}) {
  const runtime = fakeDocumentRuntime();
  if (createEditor !== undefined) {
    runtime.runtime = {
      ...runtime.runtime,
      createEditor: createEditor(runtime.runtime.createEditor),
    };
  }
  const queryClient = createQueryClient();
  if (whoAmI !== null) {
    queryClient.setQueryData<AuthenticatedUser>(["whoami"], whoAmI);
  }
  queryClient.setQueryData(["artifact", "artifact-1", "version", 1], {
    authors: [{ id: "alice", kind: "user" }],
    created_at: "2026-09-09T00:00:00Z",
    markdown,
    markdown_tree: markdownTree === null ? undefined : (markdownTree ?? treeOf(markdown)),
    named: false,
    number: 1,
    summary: null,
  });
  const blockSchema = runtime.runtime.blockSchema;
  if (blockSchema === undefined) {
    throw new Error("VersionView test runtime must provide a block schema.");
  }
  // Inside a router, as the document page renders it: a click on a reference in the version
  // opens what it names (`document-references.tsx`).
  const tree = (shown: { by: string; id: string; quote: string } | undefined) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DocumentRuntime.Provider value={runtime.runtime}>
          <VersionView
            artifactId="artifact-1"
            asks={[]}
            blockSchema={blockSchema}
            changeBaseline={changeBaseline}
            createdAt="2026-09-09T00:00:00Z"
            definePlace={undefined}
            highlight={shown}
            keyScope="document"
            owner={undefined}
            version={1}
          />
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(highlight));
  /** Renders the view again with `shown`, as its parent does on every render of its own. */
  const rerender = (shown: { by: string; id: string; quote: string } | undefined) =>
    view.rerender(tree(shown));
  /** Answers the signed-in person again, as a re-read of `/auth/whoami` does. */
  const setWhoAmI = (user: AuthenticatedUser) => {
    act(() => {
      queryClient.setQueryData<AuthenticatedUser>(["whoami"], user);
    });
  };
  return { ...runtime, blockSchema, rerender, setWhoAmI, view };
}

// The parent builds `highlight` afresh on every render. An editor rebuilt for each such render
// collapsed the page and scrolled again, and its last scroll could run before the last editor had
// laid out, leaving the reader at the top of the page; a browser row catches that only sometimes.
test("VersionView keeps its editor when the parent renders again with an equal highlight, and builds a new one for another comment", async () => {
  jest.useFakeTimers();
  const markdown = "Read the current specification and the older note";
  const { editors, rerender, view } = renderVersionView({
    highlight: { by: "user:alice", id: "comment-1", quote: "current" },
    markdown,
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    rerender({ by: "user:alice", id: "comment-1", quote: "current" });
    rerender({ by: "user:alice", id: "comment-1", quote: "current" });
    await tick(20);
    expect(editors).toHaveLength(1);
    expect(editors[0]?.destroyed).toBe(false);
    expect(editors[0]?.focused).toEqual(["comment-1"]);

    rerender({ by: "user:bob", id: "comment-2", quote: "older" });
    await waitFor(() => expect(editors).toHaveLength(2));
    expect(editors[0]?.destroyed).toBe(true);
    expect(editors[1]?.focused).toEqual(["comment-2"]);
  } finally {
    realClock();
    view.unmount();
  }
});

// `/auth/whoami` answers the signed-in person with `event_head`, which moves with every committed
// event, so each re-read of that answer is a new object. The editor is built from the login in
// it, and the same person read again must leave the reader where they were reading.
test("VersionView keeps its editor when the signed-in person is read again with a later event head", async () => {
  const { editors, setWhoAmI, view } = renderVersionView({
    highlight: undefined,
    markdown: "Read the current specification",
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    setWhoAmI({ ...signedIn, event_head: "12" });
    setWhoAmI({ ...signedIn, event_head: "13" });
    await Bun.sleep(20);
    expect(editors).toHaveLength(1);
    expect(editors[0]?.destroyed).toBe(false);
  } finally {
    view.unmount();
  }
});

test("VersionView builds its editor when the signed-in person arrives after the version's text", async () => {
  const { editors, setWhoAmI, view } = renderVersionView({
    highlight: undefined,
    markdown: "Read the current specification",
    whoAmI: null,
  });

  try {
    await waitFor(() =>
      expect(
        within(view.container).getByRole("region", { name: "Document version 1" })
      ).toBeDefined()
    );
    expect(editors).toHaveLength(0);

    setWhoAmI(signedIn);
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.user.name).toBe("alice");
  } finally {
    view.unmount();
  }
});

test("VersionView renders the version read-only with the highlighted quote focused", async () => {
  const { editors, view } = renderVersionView({
    highlight: { by: "user:alice", id: "comment-1", quote: "current" },
    markdown: "Read the current specification",
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(
      within(view.container).getByRole("region", { name: "Document version 1" })
    ).toBeDefined();
    expect(editors[0]).toMatchObject({ access: "read", focused: ["comment-1"] });
    // The editor was handed the server's tree with the quote marked in it, not markdown.
    expect(editors[0]?.markdown).toBeUndefined();
    expect(editors[0]?.root.textContent).toBe("Read the current specification");
    expect(markedText(editors[0]?.documentJSON, "proofComment")).toEqual(["current"]);
    expect(editors[0]?.options.awareness).toBeNull();
  } finally {
    view.unmount();
  }
});

test("VersionView renders a typed callout with the fetched block schema", async () => {
  const markdown = ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n';
  const markdownTree = treeOf(markdown);
  const { blockSchema, editors, view } = renderVersionView({
    highlight: undefined,
    markdown,
    markdownTree,
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(editors[0]?.options.blockSchema).toBe(blockSchema);
    expect(editors[0]?.documentJSON).toEqual(markdownTree as DocumentJSON);
  } finally {
    view.unmount();
  }
});

test("VersionView reports an ambiguous or missing quote as changed text", async () => {
  const { editors, view } = renderVersionView({
    highlight: { by: "user:alice", id: "comment-1", quote: "o" },
    markdown: "o o",
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    expect(markedText(editors[0]?.documentJSON, "proofComment")).toEqual([]);
    const status = await within(view.container).findByRole("status");
    expect(status.textContent).toBe(
      "Text changed. The selected range no longer exists in this document."
    );
  } finally {
    view.unmount();
  }
});

test("VersionView shows what refused the version instead of an empty document", async () => {
  const { editors, view } = renderVersionView({
    createEditor: (inner) => async (root, options) => {
      const handle = await inner(root, options);
      handle.setDocumentJSON = () => {
        throw new Error("this version cannot be drawn");
      };
      return handle;
    },
    highlight: undefined,
    markdown: "Some version text.",
  });

  try {
    await waitFor(() =>
      expect(within(view.container).getByRole("alert").textContent).toBe(
        "This version could not be rendered: this version cannot be drawn"
      )
    );
    expect(editors[0]?.destroyed).toBe(true);
    expect(within(view.container).getByRole("heading").textContent).toMatch(/^Version 1 · /);
  } finally {
    view.unmount();
  }
});

// A pod of the build before this one answers a version with its markdown and no reading, for as
// long as a rollout takes. The version is a stored text like every other, so it is drawn by
// parsing that markdown rather than refused: a reader who is told the version could not be
// loaded has lost a document the answer carried.
test("VersionView draws a version whose answer carried no reading from its markdown", async () => {
  const markdown = "The plan **ships** behind a canary.";
  const { editors, view } = renderVersionView({
    highlight: undefined,
    markdown,
    markdownTree: null,
  });

  try {
    await waitFor(() => expect(editors[0]?.markdown).toBe(markdown));
    expect(editors[0]?.documentJSON).toBeUndefined();
    expect(within(view.container).queryByText("Could not load this document version.")).toBeNull();
    expect(within(view.container).getByRole("heading").textContent).toMatch(/^Version 1 · /);
  } finally {
    view.unmount();
  }
});

// The reader who followed a comment's deep link into such a version must still be taken to the
// quote: the highlight is embedded in a tree, and where the answer carried none it is embedded
// in the editor's own reading of the markdown the view has just drawn. Telling that reader the
// range is gone, with the quote right there in the text, is the one reading this view must not
// make.
test("VersionView marks the highlighted quote of a version drawn from its markdown", async () => {
  const markdown = "# Plan\n\nThe plan ships behind a canary today.\n";
  const { editors, view } = renderVersionView({
    createEditor: (inner) => async (root, options) => {
      const handle: EditorHandle = await inner(root, options);
      const state = handle.view.state as unknown as { doc: ProseMirrorNode };
      // What `setMarkdown` leaves the real editor reading, which the fake's own state carries
      // nothing of: the document the quote is embedded in.
      state.doc = renderedDocument(markdown);
      return handle;
    },
    highlight: { by: "user:alice", id: "comment-1", quote: "canary" },
    markdown,
    markdownTree: null,
  });

  try {
    await waitFor(() => expect(editors[0]?.documentJSON).toBeDefined());
    // Drawn from its markdown first, then redrawn with the quote marked in what that drew.
    expect(editors[0]?.markdown).toBe(markdown);
    expect(markedText(editors[0]?.documentJSON, "proofComment")).toEqual(["canary"]);
    expect(editors[0]?.focused).toEqual(["comment-1"]);
    expect(
      within(view.container).queryByText(
        "Text changed. The selected range no longer exists in this document."
      )
    ).toBeNull();
    expect(within(view.container).queryByRole("alert")).toBeNull();
  } finally {
    view.unmount();
  }
});

/** The document the editor builds for `markdown`, which the component marks against its
 *  baseline: the fake editor holds no ProseMirror document of its own. */
function renderedDocument(markdown: string): ProseMirrorNode {
  const doc = loadedEngine()?.parse(markdown);
  if (doc === undefined) throw new Error(`the engine rendered nothing for "${markdown}"`);
  return doc;
}

/** The reading the server sends of a stored text: Go's own, which the page's own engine reads
 *  that text into node for node - the document the version page draws, and the baseline it
 *  marks against. */
function treeOf(markdown: string): DisplayTree {
  return renderedDocument(markdown).toJSON() as DisplayTree;
}

/** The baseline a version is marked against, as the reading pane carries it: the approved
 *  version's own stored text, with the reading the server sent of it. */
function baselineOf(markdown: string): ChangeBaseline {
  return { markdown, tree: treeOf(markdown) };
}

/** Records what the host draws over the rendered document, and gives the handle `doc`, the
 *  document the editor would hold: the fake runtime's view holds a plain state object with none
 *  of its own, and the component reads `view.state.doc` to compute the marks. The document is
 *  built by the caller, before the editor is asked for anything, so a row counting what the
 *  page parses counts no parse of the test's own. */
function recordMarks(
  doc: ProseMirrorNode,
  drawn: ChangeDecorations[]
): (inner: CreateEditor) => CreateEditor {
  return (inner) => async (root, options) => {
    const handle: EditorHandle = await inner(root, options);
    const state = handle.view.state as unknown as { doc: ProseMirrorNode };
    state.doc = doc;
    handle.setChangeDecorations = (decorations) => drawn.push(decorations);
    return handle;
  };
}

test("VersionView draws the marks its baseline computes over the version it renders", async () => {
  const markdown = "# Plan\n\nThe plan ships behind a canary today.\n\nThe end.\n";
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n\nThe old note.\n\nThe end.\n";
  const drawn: ChangeDecorations[] = [];
  const { view } = renderVersionView({
    changeBaseline: baselineOf(baseline),
    createEditor: recordMarks(renderedDocument(markdown), drawn),
    highlight: undefined,
    markdown,
  });

  try {
    await waitFor(() => expect(drawn).toHaveLength(1));
    const marks = changeMarksAgainst(baselineOf(baseline), renderedDocument(markdown));
    // The baseline changed one paragraph and lost another, so there is something to draw.
    expect(marks.text.length).toBeGreaterThan(0);
    expect(marks.widgets.length).toBeGreaterThan(0);
    expect(drawn[0]).toEqual(marks);
  } finally {
    view.unmount();
  }
});

test("VersionView draws nothing when it is given no baseline", async () => {
  jest.useFakeTimers();
  const markdown = "# Plan\n\nThe plan ships behind a canary today.\n";
  const markdownTree = treeOf(markdown);
  const drawn: ChangeDecorations[] = [];
  const { editors, view } = renderVersionView({
    createEditor: recordMarks(renderedDocument(markdown), drawn),
    highlight: undefined,
    markdown,
    markdownTree,
  });

  try {
    await waitFor(() => expect(editors).toHaveLength(1));
    await tick(20);
    expect(editors[0]?.documentJSON).toEqual(markdownTree as DocumentJSON);
    expect(drawn).toEqual([]);
  } finally {
    realClock();
    view.unmount();
  }
});

// What the browser spec counts on each page it opens (`e2e/parse-counting.ts`): the two entries
// a reading page can parse markdown through. One is the shared read-only engine's `parse`, which
// every display surface reads with; the other is the editor handle's `setMarkdown`, its markdown
// import, which builds a `ParserState` of its own. This row counts the same two around the
// render of a past version, with its quote highlighted and its change marks drawn - everything
// the page does with the version's text - and both must stay at zero: the version is drawn from
// the tree the server sent.
test("VersionView draws the version's tree and parses nothing, on either engine", async () => {
  const engine = loadedEngine();
  if (engine === undefined) throw new Error("the row needs the shared engine loaded");
  const markdown = "# Plan\n\nThe plan ships behind a canary today.\n";
  const baseline = "# Plan\n\nThe plan ships behind a flag today.\n";
  // Everything the test itself reads is read before the counting starts.
  const markdownTree = treeOf(markdown);
  const changeBaseline = baselineOf(baseline);
  const rendered = renderedDocument(markdown);
  const drawn: ChangeDecorations[] = [];
  const parses: string[] = [];
  const counted = engine as { parse: typeof engine.parse };
  const enginesParse = counted.parse;
  counted.parse = (...args) => {
    parses.push("display");
    return enginesParse.apply(engine, args);
  };

  const { editors, view } = renderVersionView({
    changeBaseline,
    createEditor: (inner) => async (root, options) => {
      const handle = await recordMarks(rendered, drawn)(inner)(root, options);
      handle.setMarkdown = () => {
        parses.push("editor");
      };
      return handle;
    },
    highlight: { by: "user:alice", id: "comment-1", quote: "canary" },
    markdown,
    markdownTree,
  });

  try {
    await waitFor(() => expect(drawn).toHaveLength(1));
    expect(parses).toEqual([]);
    expect(editors[0]?.documentJSON).toEqual(
      embedHighlight(markdownTree as DocumentJSON, {
        by: "user:alice",
        id: "comment-1",
        quote: "canary",
      }) as DocumentJSON
    );
    expect(markedText(editors[0]?.documentJSON, "proofComment")).toEqual(["canary"]);
    expect(editors[0]?.focused).toEqual(["comment-1"]);
  } finally {
    counted.parse = enginesParse;
    view.unmount();
  }
});
