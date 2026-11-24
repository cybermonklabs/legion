import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { displayTree } from "../../__tests__/display-trees";
import { graphNodeFields } from "../../__tests__/graph-fixture";
import { answerQuestionOf, NAMED_ASK, NAMED_QUESTION } from "../../__tests__/question-answers";
import { api } from "../../api/client";
import type { GraphEdge, GraphReferences } from "../../api/types";
import type { BlockItemKind } from "./block-reference";
import { warmMarkdownRenderer } from "./markdown-engine";
import { ReferencedBy } from "./ReferencedBy";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const createdAt = "2026-10-20T10:00:00Z";

function blockEdge(
  document: string,
  issueKey: string,
  blockId: string,
  encoded: string,
  text: string
) {
  return {
    created_at: createdAt,
    direction: "in",
    excerpt: { block_id: blockId, text },
    kind: "mentions",
    node: {
      ...graphNodeFields(),
      block_id: blockId,
      document,
      id: `${document.slice("dispatch://".length)}#${blockId}`,
      issue_key: issueKey,
      kind: "block",
      project: issueKey.split("-")[0],
      ref: `${document}#b-${encoded}`,
    },
    source_seq: 7,
  } satisfies GraphEdge;
}

/** The panel over a client that already holds CORE-1's summary: an excerpt that is the head of
 *  a stored body is drawn by the renderer, which draws a reference in it to CORE-1 as CORE-1's
 *  status mark and its key. */
function renderPanel(read: GraphReferences, reference = "dispatch://CORE-1") {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(read);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["issue-summary", "CORE-1"], {
    documents: [],
    key: "CORE-1",
    project: "CORE",
    status: "in_progress",
    title: "Fix the thing",
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReferencedBy reference={reference} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { client, getReferences, view };
}

/** Each row of `source` as [its excerpt's words, the first link it holds]. The status mark before
 *  an issue's key holds no text, and a space stands between the two. */
function excerptRows(source: Element): (string | null | undefined)[][] {
  return [...source.querySelectorAll(":scope > ul > li")].map((row) => [
    row.querySelector("p")?.textContent?.replace(/\s+/g, " "),
    row.querySelector("a")?.getAttribute("href"),
  ]);
}

/** The element a reference to CORE-1 renders into, once CORE-1's summary is read: it names the
 *  issue by its key, its status's words and its title. */
const CORE_1_DRAWN = '[aria-label="CORE-1, In progress, Fix the thing"]';

let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

test("an answers edge is labelled Answer", async () => {
  const { getReferences, view } = renderPanel({
    edges: [
      {
        created_at: createdAt,
        direction: "in",
        kind: "answers",
        node: {
          ...graphNodeFields(),
          id: "11111111-1111-4111-8111-111111111111",
          issue_key: "OPS-1",
          kind: "comment",
          project: "OPS",
          ref: "dispatch://OPS-1/comment/11111111-1111-4111-8111-111111111111",
        },
        source_seq: null,
      },
    ],
    node: {
      ...graphNodeFields(),
      id: "CORE-1",
      issue_key: "CORE-1",
      kind: "issue",
      ref: "dispatch://CORE-1",
    },
  });
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };
  const panel = await screen.findByRole("region", { name: "Referenced by" });
  expect(await within(panel).findByRole("link", { name: "Answer · OPS-1" })).toBeDefined();
});

test("two blocks of one document version citing the node are two rows under that document, each linked to its block", async () => {
  const notes = "dispatch://OPS-1/artifact/notes-md";
  const consoleError = spyOn(console, "error");
  const { getReferences, view } = renderPanel({
    edges: [
      blockEdge(notes, "OPS-1", "ship_it", "ship%5Fit", "Ship it as dispatch://CORE-1 says."),
      blockEdge(notes, "OPS-1", "p2", "p2", "And hold the rest for dispatch://CORE-1."),
    ],
    node: {
      ...graphNodeFields(),
      id: "CORE-1",
      issue_key: "CORE-1",
      kind: "issue",
      ref: "dispatch://CORE-1",
    },
  });
  restore = () => {
    getReferences.mockRestore();
    consoleError.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByRole("link", { name: "Artifact · OPS-1" });
  expect(within(panel).getByRole("heading").textContent).toBe("Referenced by (2)");
  // One source: the document, its header linked to the document itself.
  const sources = panel.querySelectorAll(":scope > ul > li");
  expect(sources).toHaveLength(1);
  const source = within(sources[0] as HTMLElement);
  expect(source.getByRole("link", { name: "Artifact · OPS-1" })).toHaveAttribute(
    "href",
    "/issues/OPS-1/artifacts/notes-md"
  );
  // Each block its own row, with its own excerpt and a link to that block, its id encoded as
  // encodeBlockId writes it. The excerpt is the head of the block's text, drawn by the renderer:
  // the address in it reads as the issue's status mark and its key, never as the characters
  // `dispatch://CORE-1`.
  expect(sources[0]?.querySelectorAll(":scope > ul > li")).toHaveLength(2);
  await waitFor(() =>
    expect(excerptRows(sources[0] as HTMLElement)).toEqual([
      ["Ship it as CORE-1 says.", "/issues/OPS-1/artifacts/notes-md#b-ship%5Fit"],
      ["And hold the rest for CORE-1.", "/issues/OPS-1/artifacts/notes-md#b-p2"],
    ])
  );
  expect(sources[0]?.querySelectorAll(`${CORE_1_DRAWN} svg[data-status-glyph]`)).toHaveLength(2);
  // Rows written in one version share their time, and still have keys of their own.
  expect(consoleError.mock.calls.some((call) => String(call[0]).includes("same key"))).toBeFalse();
});

test("a source of one citing node links to where its mention is: a block at the block, a document at its excerpt's block", async () => {
  const { getReferences, view } = renderPanel({
    edges: [
      blockEdge("dispatch://OPS-1/spec", "OPS-1", "ship_it", "ship%5Fit", "Ship it."),
      {
        created_at: createdAt,
        direction: "in",
        excerpt: { block_id: "hold_it", text: "Hold it." },
        kind: "mentions",
        node: {
          ...graphNodeFields(),
          id: "CORE/handbook-md",
          kind: "artifact",
          project: "CORE",
          ref: "dispatch://CORE/artifact/handbook-md",
        },
        source_seq: 8,
      },
    ],
    node: {
      ...graphNodeFields(),
      id: "CORE-1",
      issue_key: "CORE-1",
      kind: "issue",
      ref: "dispatch://CORE-1",
    },
  });
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByText("Hold it.");
  expect(within(panel).getByRole("link", { name: "Artifact · OPS-1" })).toHaveAttribute(
    "href",
    "/issues/OPS-1/spec#b-ship%5Fit"
  );
  expect(within(panel).getByRole("link", { name: "Artifact · CORE" })).toHaveAttribute(
    "href",
    "/projects/CORE/documents/handbook-md#b-hold%5Fit"
  );
  // A one-node source's header is its only link.
  expect(within(panel).getAllByRole("link")).toHaveLength(2);
});

test("one source citing two blocks of the shown document is two rows, each linked to its block and keeping its own state across a refetch", async () => {
  const user = userEvent.setup();
  const spec = "dispatch://CORE-1/spec";
  const citing = (blockId: string) =>
    ({
      block: {
        ...graphNodeFields(),
        block_id: blockId,
        document: spec,
        id: `CORE-1/spec#${blockId}`,
        issue_key: "CORE-1",
        kind: "block",
        project: "CORE",
        ref: `${spec}#b-${blockId}`,
      },
      created_at: createdAt,
      direction: "in",
      // Long enough that the row clamps it and offers to expand it.
      excerpt: { text: `Both of these hold: ${"the plan stands as written. ".repeat(12)}` },
      kind: "mentions",
      node: {
        ...graphNodeFields(),
        id: "c1",
        issue_key: "OPS-1",
        kind: "comment",
        project: "OPS",
        ref: "dispatch://OPS-1/comment/c1",
      },
      source_seq: 9,
    }) satisfies GraphEdge;
  const node = {
    ...graphNodeFields(),
    id: "CORE-1/spec",
    issue_key: "CORE-1",
    kind: "artifact",
    ref: spec,
  } as const;
  const consoleError = spyOn(console, "error");
  const { client, getReferences, view } = renderPanel(
    { edges: [citing("a"), citing("b")], node },
    spec
  );
  restore = () => {
    getReferences.mockRestore();
    consoleError.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByRole("link", { name: "Comment · OPS-1" });
  // Each row as [the block it links to, whether its excerpt is expanded].
  const rows = () =>
    [...panel.querySelectorAll<HTMLElement>(":scope > ul > li > ul > li")].map((row) => [
      within(row)
        .getByRole("link", { name: /^block / })
        .getAttribute("href"),
      within(row).getByRole("button").getAttribute("aria-expanded"),
    ]);
  expect(rows()).toEqual([
    ["/issues/CORE-1/spec#b-a", "false"],
    ["/issues/CORE-1/spec#b-b", "false"],
  ]);

  // Expand the excerpt of the row citing b only.
  const showMore = within(panel).getAllByRole("button", { name: "Show more" });
  await user.click(showMore[1] as HTMLElement);
  expect(rows()).toEqual([
    ["/issues/CORE-1/spec#b-a", "false"],
    ["/issues/CORE-1/spec#b-b", "true"],
  ]);

  // The comment stops citing a: its row goes, and b's row keeps its link and its expanded excerpt.
  getReferences.mockResolvedValue({ edges: [citing("b")], node });
  await act(() => client.refetchQueries());
  expect(rows()).toEqual([["/issues/CORE-1/spec#b-b", "true"]]);
  expect(consoleError.mock.calls.some((call) => String(call[0]).includes("same key"))).toBeFalse();
});

/** An edge from block `blockId` of the ask, comment or message `id` on OPS-1, as the graph lists
 *  a mention a part of an ask, or a block of a comment or message, holds: its node is the block,
 *  naming no document, and its parent the item. */
function itemBlockEdge(kind: BlockItemKind, id: string, blockId: string, text: string) {
  const item = `dispatch://OPS-1/${kind}/${id}`;
  return {
    created_at: createdAt,
    direction: "in",
    excerpt: { block_id: blockId, text },
    kind: "mentions",
    node: {
      ...graphNodeFields(),
      block_id: blockId,
      id: `${kind}/${id}#${blockId}`,
      issue_key: "OPS-1",
      kind: "block",
      parent: { id, kind, ref: item },
      project: "OPS",
      ref: `${item}#b-${blockId}`,
    },
    source_seq: 10,
  } satisfies GraphEdge;
}

test("blocks of one comment citing the node are rows under that comment, labelled and linked as it, each row at its block", async () => {
  const comment = "11111111-1111-4111-8111-111111111111";
  const message = "33333333-3333-4333-8333-333333333333";
  const { getReferences, view } = renderPanel({
    edges: [
      itemBlockEdge("comment", comment, "b1", "Alpha cites dispatch://CORE-1."),
      itemBlockEdge("comment", comment, "b2", "Beta cites it too."),
      itemBlockEdge("message", message, "b1", "The message cites dispatch://CORE-1."),
    ],
    node: {
      ...graphNodeFields(),
      id: "CORE-1",
      issue_key: "CORE-1",
      kind: "issue",
      ref: "dispatch://CORE-1",
    },
  });
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByText("Beta cites it too.");
  const sources = panel.querySelectorAll<HTMLElement>(":scope > ul > li");
  expect(sources).toHaveLength(2);
  // The comment is one source: its header names and opens the comment.
  const commentSource = within(sources[0] as HTMLElement);
  expect(commentSource.getByRole("link", { name: "Comment · OPS-1" })).toHaveAttribute(
    "href",
    `/issues/OPS-1/comments/${comment}`
  );
  await waitFor(() =>
    expect(excerptRows(sources[0] as HTMLElement)).toEqual([
      ["Alpha cites CORE-1.", `/issues/OPS-1/comments/${comment}#b-b1`],
      ["Beta cites it too.", `/issues/OPS-1/comments/${comment}#b-b2`],
    ])
  );
  // A message's one block is a source of one node: its header opens the message at the block.
  const messageSource = within(sources[1] as HTMLElement);
  expect(messageSource.getByRole("link", { name: "Message · OPS-1" })).toHaveAttribute(
    "href",
    `/issues/OPS-1/messages/${message}#b-b1`
  );
  await waitFor(() =>
    expect(sources[1]?.querySelector("p")?.textContent?.replace(/\s+/g, " ")).toBe(
      "The message cites CORE-1."
    )
  );
  expect(sources[1]?.querySelector(CORE_1_DRAWN)).not.toBeNull();
});

test("parts of one ask citing the node are rows under that ask, named as the ask rather than as an artifact", async () => {
  const ask = "22222222-2222-4222-8222-222222222222";
  const { getReferences, view } = renderPanel({
    edges: [
      itemBlockEdge("ask", ask, "b1", "Ship it once dispatch://CORE-1 lands?"),
      itemBlockEdge("ask", ask, "b3", "Hold — dispatch://CORE-1 is not ready."),
    ],
    node: {
      ...graphNodeFields(),
      id: "CORE-1",
      issue_key: "CORE-1",
      kind: "issue",
      ref: "dispatch://CORE-1",
    },
  });
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByRole("link", { name: "Ask · OPS-1" });
  const sources = panel.querySelectorAll<HTMLElement>(":scope > ul > li");
  expect(sources).toHaveLength(1);
  // The ask is the source: the question and the option are its rows, each linked to its part.
  expect(within(panel).getByRole("link", { name: "Ask · OPS-1" })).toHaveAttribute(
    "href",
    `/issues/OPS-1/asks/${ask}`
  );
  expect(within(panel).queryByRole("link", { name: /^Artifact/ })).not.toBeInTheDocument();
  await waitFor(() =>
    expect(excerptRows(sources[0] as HTMLElement)).toEqual([
      ["Ship it once CORE-1 lands?", `/issues/OPS-1/asks/${ask}#b-b1`],
      ["Hold — CORE-1 is not ready.", `/issues/OPS-1/asks/${ask}#b-b3`],
    ])
  );
});

// A project document's comment opens on its document's page (`?comment=`), where a `#b-`
// fragment names a block of the document: a block of that comment links to the comment, with no
// fragment.
test("a block of a project document's comment links to its comment, with no fragment", async () => {
  const comment = "11111111-1111-4111-8111-111111111111";
  const item = `dispatch://CORE/artifact/handbook-md/comment/${comment}`;
  const { getReferences, view } = renderPanel({
    edges: [
      {
        created_at: createdAt,
        direction: "in",
        excerpt: { block_id: "b2", text: "Beta cites dispatch://CORE-1." },
        kind: "mentions",
        node: {
          ...graphNodeFields(),
          block_id: "b2",
          id: `comment/${comment}#b2`,
          kind: "block",
          parent: { id: comment, kind: "comment", ref: item },
          project: "CORE",
          ref: `${item}#b-b2`,
        },
        source_seq: 10,
      },
    ],
    node: {
      ...graphNodeFields(),
      id: "CORE-1",
      issue_key: "CORE-1",
      kind: "issue",
      ref: "dispatch://CORE-1",
    },
  });
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await waitFor(() =>
    expect(panel.querySelector("li li p")?.textContent?.replace(/\s+/g, " ")).toBe(
      "Beta cites CORE-1."
    )
  );
  expect(panel.querySelector(`li li p ${CORE_1_DRAWN}`)).not.toBeNull();
  expect(within(panel).getByRole("link", { name: "Comment · CORE" })).toHaveAttribute(
    "href",
    `/projects/CORE/documents/handbook-md?comment=${comment}`
  );
});

// The server cuts a block's excerpt where no reference crosses the cut, so a block whose text
// opens with a reference longer than the cut has an empty excerpt, which still names the block
// the row links to. The row then draws no excerpt at all, not an empty paragraph.
test("a block whose excerpt the cut left empty is linked to its block and draws no excerpt", async () => {
  const { getReferences, view } = renderPanel({
    edges: [blockEdge("dispatch://CORE-2/artifact/spec-md", "CORE-2", "b1", "b1", "")],
    node: {
      ...graphNodeFields(),
      id: "CORE-1",
      issue_key: "CORE-1",
      kind: "issue",
      ref: "dispatch://CORE-1",
    },
  });
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  const row = await waitFor(() => {
    const found = panel.querySelector("li li");
    expect(found).not.toBeNull();
    return found as HTMLElement;
  });
  expect(row.querySelector("p")).toBeNull();
  expect(row.querySelector("[data-markdown-preview], .dispatch-markdown")).toBeNull();
});

// An excerpt is one of two things, and the panel draws each as what it is: the head of a stored
// body - a citing comment's here - goes through the renderer, so its markdown is read; a citing
// issue's title is one line of text already, and is shown as the characters it is.
test("a citing comment's excerpt is rendered, and a citing issue's title is shown as written", async () => {
  const comment = "11111111-1111-4111-8111-111111111111";
  const written = "See [the plan](https://example.test/plan) **first**";
  const { getReferences, view } = renderPanel({
    edges: [
      {
        created_at: createdAt,
        direction: "in",
        excerpt: { text: written },
        kind: "mentions",
        node: {
          ...graphNodeFields(),
          id: comment,
          issue_key: "OPS-1",
          kind: "comment",
          project: "OPS",
          ref: `dispatch://OPS-1/comment/${comment}`,
        },
        source_seq: 11,
      },
      {
        created_at: createdAt,
        direction: "in",
        excerpt: { text: written },
        kind: "blocked_by",
        node: {
          ...graphNodeFields(),
          id: "OPS-2",
          issue_key: "OPS-2",
          kind: "issue",
          project: "OPS",
          ref: "dispatch://OPS-2",
        },
        source_seq: 12,
      },
    ],
    node: {
      ...graphNodeFields(),
      id: "CORE-1",
      issue_key: "CORE-1",
      kind: "issue",
      ref: "dispatch://CORE-1",
    },
  });
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  const sources = panel.querySelectorAll<HTMLElement>(":scope > ul > li");
  expect(sources).toHaveLength(2);
  const fromComment = within(sources[0] as HTMLElement);
  expect(await fromComment.findByRole("link", { name: "the plan" })).toHaveAttribute(
    "href",
    "https://example.test/plan"
  );
  expect(fromComment.getByText("first", { selector: "strong" })).toBeDefined();
  expect(within(sources[1] as HTMLElement).getByText(written)).toBeDefined();
});

// A citing block of a document is a block of that document, which its node carries as its
// `parent`: its excerpt is the document's text, so a `#id` in it names the question of that
// document. And the excerpt is drawn from the reading the server cut it into, never parsed in
// the page: the row shows the tree's `later` where the excerpt's markdown says `now`.
test("a document block's excerpt is drawn from its tree and reads a #id against its document", async () => {
  await warmMarkdownRenderer();
  const answers = answerQuestionOf("artifact", "plan-artifact");
  const { getReferences, view } = renderPanel({
    edges: [
      {
        created_at: createdAt,
        direction: "in",
        excerpt: {
          block_id: "b1",
          text: "Decide #q2 now.",
          text_tree: displayTree("Decide #q2 later."),
        },
        kind: "mentions",
        node: {
          ...graphNodeFields(),
          block_id: "b1",
          document: "dispatch://CORE-2/artifact/plan-md",
          id: "CORE-2/plan-md#b1",
          issue_key: "CORE-2",
          kind: "block",
          parent: {
            id: "plan-artifact",
            kind: "artifact",
            ref: "dispatch://CORE-2/artifact/plan-md",
          },
          project: "CORE",
          ref: "dispatch://CORE-2/artifact/plan-md#b-b1",
        },
        source_seq: 13,
      },
    ],
    node: {
      ...graphNodeFields(),
      id: "CORE-1",
      issue_key: "CORE-1",
      kind: "issue",
      ref: "dispatch://CORE-1",
    },
  });
  restore = () => {
    getReferences.mockRestore();
    answers.restore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await waitFor(() =>
    expect(panel.querySelector(`[data-dispatch-ref="${NAMED_ASK}"]`)?.textContent).toBe(
      NAMED_QUESTION
    )
  );
  expect(panel.textContent).toContain(`Decide ${NAMED_QUESTION} later.`);
});

// `serves` and `needs` are the edge kinds a typed block's `block[]` attributes make
// (`blocks.json`): a check or a plan task naming a requirement, and a plan task naming another.
// Read from the block that is named, the edge says what the citing block does for it.
test("a serves edge reads as served by and a needs edge as needed by", async () => {
  const plan = "dispatch://CORE-1/artifact/plan-md";
  const verification = "dispatch://CORE-1/artifact/verification-md";
  const { getReferences, view } = renderPanel(
    {
      edges: [
        { ...blockEdge(verification, "CORE-1", "c1", "c1", "Run the suite."), kind: "serves" },
        { ...blockEdge(plan, "CORE-1", "t1", "t1", "Write the panel."), kind: "needs" },
      ],
      node: {
        ...graphNodeFields(),
        block_id: "r1",
        document: "dispatch://CORE-1/spec",
        id: "CORE-1/spec#r1",
        issue_key: "CORE-1",
        kind: "block",
        ref: "dispatch://CORE-1/spec#b-r1",
      },
    },
    "dispatch://CORE-1/spec#b-r1"
  );
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByText("Run the suite.");
  expect(within(panel).getByRole("link", { name: "served by · CORE-1" })).toBeTruthy();
  expect(within(panel).getByRole("link", { name: "needed by · CORE-1" })).toBeTruthy();
});
