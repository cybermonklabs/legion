import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { graphNodeFields } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { AskRead, IssueReferenceSummary } from "../../api/types";
import { Publisher } from "../issue-page/projection-fixture";
import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";
import { PlainText } from "./plain-text";
import { type PublishedReferences, ReferenceNamesProvider } from "./reference-names";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/**
 * The plain-text projection (`plain-text.tsx`) under a page that publishes its batched read
 * (`reference-names.tsx`): how often a line is drawn, and what it reads a reference as by R8's
 * one rule, beside the painted link that reads the same target.
 */

/** The spies a row put on the API client, taken off again once it ends. */
const spies: { mockRestore(): void }[] = [];

afterEach(() => {
  for (const spy of spies.splice(0)) {
    spy.mockRestore();
  }
});

/** What a page publishes of its batched read: made anew on every call, as the page's own read
 *  makes its sets anew each time any of its reads answers. */
function publication({
  names = new Map(),
  unnamed = [],
}: {
  names?: ReadonlyMap<string, string>;
  unnamed?: readonly string[];
}): PublishedReferences {
  return {
    ambiguous: new Set(),
    asked: new Set([...names.keys(), ...unnamed]),
    deleted: new Map(),
    edited: new Set(),
    gone: new Set(),
    readings: new Map([...names].map(([reference, words]) => [reference, { words }])),
    unnamed: new Set(unnamed),
  };
}

/** Draws `children` under the names store with `published` published, as the shell and a page
 *  that has read its references draw a surface. */
function page(client: QueryClient, published: PublishedReferences, children: ReactNode) {
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Publisher published={published} />
          {children}
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

function client(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

// The architect's ruling on the CI row `margin-thread-reply`, which lost its click to a margin
// that changed shape between the press's aim and the press: a line sets nothing while it is
// drawn, so it is drawn once for each change of what it reads and never twice for one - the
// projection's answer, the store's name landing, the projection read again with it - and a page
// that publishes the same references again, in sets made anew, draws no line again.
test("a plain line is drawn once for each change of what it reads, and never for the same names published anew", async () => {
  const reference = "dispatch://CORE-1";
  const names = new Map([[reference, "Fix the thing"]]);
  const drawn: (string | undefined)[] = [];
  // One element for every draw of the page, so the line draws again only for what it reads.
  const line = (
    <PlainText markdown={`Tied to ${reference} here.`}>
      {(text) => {
        drawn.push(text);
        return <span data-testid="line">{text ?? ""}</span>;
      }}
    </PlainText>
  );
  const queries = client();
  const view = render(page(queries, publication({ names }), line));
  try {
    await waitFor(() =>
      expect(view.getByTestId("line").textContent).toBe("Tied to CORE-1 Fix the thing here.")
    );
    expect(drawn, "the line was drawn again for nothing it reads").toEqual([
      undefined,
      `Tied to ${reference} here.`,
      `Tied to ${reference} here.`,
      "Tied to CORE-1 Fix the thing here.",
    ]);
    const settled = drawn.length;
    view.rerender(page(queries, publication({ names }), line));
    expect(drawn.length - settled, "the same names published anew drew the line again").toBe(0);
  } finally {
    view.unmount();
  }
});

/** CORE-2, as its summary answers it. */
const CORE_2: IssueReferenceSummary = {
  documents: [],
  key: "CORE-2",
  project: "CORE",
  status: "in_progress",
  title: "Drain the queue",
};

// The architect's ruling on `publishedAnswer`, by R8: a batched read that answered for a target
// and sent no words for it - another issue's block - has not answered that the target has no
// name. A plain line reads the target itself, as the painted link to it does, by the same query,
// so the two read one target by one request and name it in the same words.
test("a plain line and a link name another issue's block the batch answered with no words alike, by one read", async () => {
  const block = "dispatch://CORE-2/spec#b-r1";
  const text = "Keep the queue drained.";
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: {
      ...graphNodeFields(),
      block_id: "r1",
      document: "dispatch://CORE-2/spec",
      id: "CORE-2/spec#r1",
      issue_key: "CORE-2",
      kind: "block",
      project: "CORE",
      ref: block,
      text,
      text_tree: displayTree(text),
    },
  });
  spies.push(getReferences, spyOn(api, "getIssueSummary").mockResolvedValue(CORE_2));
  const markdown = `See ${block} first`;
  const view = render(
    page(
      client(),
      publication({ unnamed: [block] }),
      <>
        <PlainText markdown={markdown}>
          {(line) => <span data-testid="line">{line ?? ""}</span>}
        </PlainText>
        <MarkdownBody markdown={markdown} />
      </>
    )
  );
  try {
    await within(view.container).findByRole("link", { name: text });
    await waitFor(() => expect(view.getByTestId("line").textContent).toBe(`See ${text} first`));
    expect(getReferences, "the line and the link read the block twice").toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

/** An ask of CORE-1, by its whole id, whose question runs past the room a link's title has. */
const LONG_ASK_ID = "6b3f2a10-4c5d-4e6f-8a9b-0c1d2e3f4a5b";
const LONG_ASK = `dispatch://CORE-1/ask/${LONG_ASK_ID}`;
const LONG_QUESTION =
  "Should the outline keep every folded branch closed after the reader reloads the page twice?";

// The architect's ruling (A) on R8's one rule: a name reads as a link to its target reads it. A
// link titles an ask, a comment, a message and a block by the first 60 characters of its own
// text and an ellipsis (`linkTitleMax`), so the name the page's batched read publishes for one is
// cut there too: a plain line fed by the batch, a plain line that reads the ask itself and the
// link to it read one question in the same words.
test("a name past a link's room reads the same from the page's batch, from its own read and in a link", async () => {
  const words = `${[...LONG_QUESTION].slice(0, 60).join("")}…`;
  expect(words).toBe("Should the outline keep every folded branch closed after the…");
  spies.push(
    spyOn(api, "getAsk").mockResolvedValue({
      answers: [],
      ask: {
        anchor: null,
        answer: null,
        artifact_id: null,
        asked_of: null,
        asked_of_at: null,
        asked_of_by: null,
        author: { id: "planner", kind: "session" },
        block_id: null,
        created_at: "2026-01-01T00:00:00Z",
        edited_at: null,
        id: LONG_ASK_ID,
        issue_key: "CORE-1",
        kind: "question",
        multiple: false,
        opened_event_id: 1,
        options: [],
        question: LONG_QUESTION,
        question_tree: displayTree(LONG_QUESTION),
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
    } as unknown as AskRead)
  );
  const markdown = `See ${LONG_ASK} now`;
  const tree = displayTree(LONG_QUESTION);
  if (tree === undefined) throw new Error("the engine read no question");
  const published: PublishedReferences = {
    ambiguous: new Set(),
    asked: new Set([LONG_ASK]),
    deleted: new Map(),
    edited: new Set(),
    gone: new Set(),
    readings: new Map([[LONG_ASK, { tree }]]),
    unnamed: new Set(),
  };
  const view = render(
    <QueryClientProvider client={client()}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Publisher published={published} />
          <PlainText markdown={markdown}>
            {(line) => <span data-testid="batch">{line ?? ""}</span>}
          </PlainText>
        </ReferenceNamesProvider>
        <PlainText markdown={markdown}>
          {(line) => <span data-testid="own">{line ?? ""}</span>}
        </PlainText>
        <MarkdownBody markdown={markdown} />
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    await within(view.container).findByRole("link", { name: words });
    await waitFor(() => expect(view.getByTestId("own").textContent).toBe(`See ${words} now`));
    expect(view.getByTestId("batch").textContent, "the batch's name").toBe(`See ${words} now`);
  } finally {
    view.unmount();
  }
});

// Decisions 66 and 88 over a nested list: an item of an ordered list opens on its number, and
// where its first block is a list of its own, the number stands before the first words the item
// holds, as a rendered body draws it, never on a line of its own.
test("an ordered item that opens a list of its own reads its number before its first words", async () => {
  const markdown = "1. - a\n   - b\n2. c";
  const view = render(
    page(
      client(),
      publication({}),
      <>
        <PlainText markdown={markdown}>
          {(line) => <span data-testid="whole">{line ?? ""}</span>}
        </PlainText>
        <PlainText firstLine markdown={markdown}>
          {(line) => <span data-testid="first">{line ?? ""}</span>}
        </PlainText>
        <PlainText markdown={markdown} tree={displayTree(markdown)}>
          {(line) => <span data-testid="read">{line ?? ""}</span>}
        </PlainText>
      </>
    )
  );
  try {
    await waitFor(() => expect(view.getByTestId("whole").textContent).toBe("1. a b 2. c"));
    expect(view.getByTestId("first").textContent).toBe("1. a");
    expect(view.getByTestId("read").textContent).toBe("1. a b 2. c");
  } finally {
    view.unmount();
  }
});
