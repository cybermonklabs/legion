import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { graphNodeFields } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { GraphEdge, GraphReferences } from "../../api/types";
import { useListPages } from "../issue-page/details-leaves";
import { SHOW_MORE, showMoreName } from "../issue-page/show-more-fixture";
import type { BlockItemKind } from "./block-reference";
import { type ExtraReferenceRow, ReferencedBy } from "./ReferencedBy";

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

function renderPanel(read: GraphReferences, reference = "dispatch://CORE-1") {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(read);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReferencedBy reference={reference} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { client, getReferences, view };
}

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
  await within(panel).findByText("Ship it as dispatch://CORE-1 says.");
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
  // encodeBlockId writes it.
  const rows = (sources[0] as HTMLElement).querySelectorAll(":scope > ul > li");
  expect(rows).toHaveLength(2);
  expect(
    [...rows].map((row) => [
      row.querySelector("p")?.textContent,
      row.querySelector("a")?.getAttribute("href"),
    ])
  ).toEqual([
    ["Ship it as dispatch://CORE-1 says.", "/issues/OPS-1/artifacts/notes-md#b-ship%5Fit"],
    ["And hold the rest for dispatch://CORE-1.", "/issues/OPS-1/artifacts/notes-md#b-p2"],
  ]);
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
  const rows = (sources[0] as HTMLElement).querySelectorAll(":scope > ul > li");
  expect(
    [...rows].map((row) => [
      row.querySelector("p")?.textContent,
      row.querySelector("a")?.getAttribute("href"),
    ])
  ).toEqual([
    ["Alpha cites dispatch://CORE-1.", `/issues/OPS-1/comments/${comment}#b-b1`],
    ["Beta cites it too.", `/issues/OPS-1/comments/${comment}#b-b2`],
  ]);
  // A message's one block is a source of one node: its header opens the message at the block.
  const messageSource = within(sources[1] as HTMLElement);
  expect(messageSource.getByRole("link", { name: "Message · OPS-1" })).toHaveAttribute(
    "href",
    `/issues/OPS-1/messages/${message}#b-b1`
  );
  expect(messageSource.getByText("The message cites dispatch://CORE-1.")).toBeTruthy();
});

// A citing comment or message carries its post's marks, as every read of the post does: a
// deleted one has no excerpt left and its row prints the deleted line, an edited one carries
// ` · edited` beside its time, and one that stands carries neither.
test("a row of a deleted citing post reads as deleted, an edited one says so, and a standing one neither", async () => {
  const postEdge = (
    kind: "comment" | "message",
    id: string,
    marks: Partial<GraphEdge["node"]>,
    excerpt?: string
  ): GraphEdge => ({
    created_at: createdAt,
    direction: "in",
    ...(excerpt === undefined ? {} : { excerpt: { text: excerpt } }),
    kind: "mentions",
    node: {
      ...graphNodeFields(),
      id,
      issue_key: "OPS-1",
      kind,
      project: "OPS",
      ref: `dispatch://OPS-1/${kind}/${id}`,
      ...marks,
    },
    source_seq: 11,
  });
  const { getReferences, view } = renderPanel({
    edges: [
      // The deleted post was edited first, so ` · edited` can be drawn, and only the row's own
      // reading of the delete keeps it off.
      postEdge("comment", "11111111-1111-4111-8111-111111111111", {
        deleted_at: "2026-10-20T11:00:00Z",
        deleted_by: { id: "alice", kind: "user" },
        edited_at: "2026-10-20T10:00:00Z",
      }),
      postEdge(
        "message",
        "33333333-3333-4333-8333-333333333333",
        { edited_at: "2026-10-20T10:30:00Z" },
        "The message cites dispatch://CORE-1."
      ),
      postEdge("comment", "44444444-4444-4444-8444-444444444444", {}, "A comment cites it."),
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
  await within(panel).findByText("A comment cites it.");
  const sources = [...panel.querySelectorAll<HTMLElement>(":scope > ul > li")];
  expect(sources).toHaveLength(3);
  const [deleted, edited, standing] = sources as [HTMLElement, HTMLElement, HTMLElement];
  expect(deleted.querySelector("[data-post-deleted]")?.textContent).toContain("Deleted by alice");
  expect(deleted.textContent).not.toContain("edited");
  expect(edited.textContent).toContain("· edited");
  expect(edited.querySelector("[data-post-deleted]")).toBeNull();
  expect(standing.textContent).not.toContain("edited");
  expect(standing.querySelector("[data-post-deleted]")).toBeNull();
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
  await within(panel).findByText("Hold — dispatch://CORE-1 is not ready.");
  const sources = panel.querySelectorAll<HTMLElement>(":scope > ul > li");
  expect(sources).toHaveLength(1);
  // The ask is the source: the question and the option are its rows, each linked to its part.
  expect(within(panel).getByRole("link", { name: "Ask · OPS-1" })).toHaveAttribute(
    "href",
    `/issues/OPS-1/asks/${ask}`
  );
  expect(within(panel).queryByRole("link", { name: /^Artifact/ })).not.toBeInTheDocument();
  const rows = (sources[0] as HTMLElement).querySelectorAll(":scope > ul > li");
  expect(
    [...rows].map((row) => [
      row.querySelector("p")?.textContent,
      row.querySelector("a")?.getAttribute("href"),
    ])
  ).toEqual([
    ["Ship it once dispatch://CORE-1 lands?", `/issues/OPS-1/asks/${ask}#b-b1`],
    ["Hold — dispatch://CORE-1 is not ready.", `/issues/OPS-1/asks/${ask}#b-b3`],
  ]);
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
  await within(panel).findByText("Beta cites dispatch://CORE-1.");
  expect(within(panel).getByRole("link", { name: "Comment · CORE" })).toHaveAttribute(
    "href",
    `/projects/CORE/documents/handbook-md?comment=${comment}`
  );
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

/** `renderPanel`, with rows the host adds after the list's own. */
function renderPanelWithExtraRows(
  read: GraphReferences,
  extraRows: Parameters<typeof ReferencedBy>[0]["extraRows"]
) {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(read);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReferencedBy extraRows={extraRows} reference="dispatch://CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { client, getReferences, view };
}

test("extra rows are drawn after the list's own, and the list says nothing references it only when neither has a row", async () => {
  const node = {
    ...graphNodeFields(),
    id: "CORE-1",
    issue_key: "CORE-1",
    kind: "issue",
    ref: "dispatch://CORE-1",
  } satisfies GraphReferences["node"];
  const extraRow = {
    at: "2026-10-21T10:00:00Z",
    edge: "extra edge",
    excerpt: "extra words",
    href: "/somewhere/r1",
    key: "r1",
    kind: "Extra kind",
  };
  const comment = "11111111-1111-4111-8111-111111111111";

  // One edge and one extra row: the extra row is drawn whole, in its own list after the edge's,
  // and the heading counts only the list's own edge.
  let rendered = renderPanelWithExtraRows(
    { edges: [itemBlockEdge("comment", comment, "b1", "Alpha cites dispatch://CORE-1.")], node },
    [extraRow]
  );
  const close = () => {
    rendered.getReferences.mockRestore();
    rendered.view.unmount();
  };
  restore = close;
  let panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByText("Alpha cites dispatch://CORE-1.");
  expect(within(panel).getByRole("heading").textContent).toBe("Referenced by (1)");
  const extraList = panel.querySelector<HTMLElement>("[data-extra-references]");
  expect(extraList).not.toBeNull();
  const extra = within(extraList as HTMLElement);
  expect(extra.getByRole("link", { name: "Extra kind" })).toHaveAttribute("href", "/somewhere/r1");
  expect(extra.getByText("extra edge")).toBeTruthy();
  expect(extra.getByText("extra words")).toBeTruthy();
  expect(extraList?.querySelector('time[datetime="2026-10-21T10:00:00Z"]')).not.toBeNull();
  const rows = panel.querySelectorAll<HTMLElement>(":scope > ul > li");
  expect(rows).toHaveLength(2);
  expect(
    within(rows[0] as HTMLElement).getByRole("link", { name: "Comment · OPS-1" })
  ).toBeTruthy();
  expect(rows[1]?.closest("[data-extra-references]")).toBe(extraList);
  close();

  // No edge and one extra row: the list does not say that nothing references the node.
  rendered = renderPanelWithExtraRows({ edges: [], node }, [extraRow]);
  panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByRole("heading", { name: "Referenced by (0)" });
  expect(within(panel).queryByText("Nothing references this yet.")).toBeNull();
  expect(within(panel).getByRole("link", { name: "Extra kind" })).toHaveAttribute(
    "href",
    "/somewhere/r1"
  );
  close();

  // No edge and no extra row: the list says so, and draws no extra list.
  rendered = renderPanel({ edges: [], node });
  panel = await screen.findByRole("region", { name: "Referenced by" });
  expect(await within(panel).findByText("Nothing references this yet.")).toBeTruthy();
  expect(panel.querySelector("[data-extra-references]")).toBeNull();
});

test("an extra row with no time draws its edge with no separator and no time", async () => {
  const node = {
    ...graphNodeFields(),
    id: "CORE-1",
    issue_key: "CORE-1",
    kind: "issue",
    ref: "dispatch://CORE-1",
  } satisfies GraphReferences["node"];
  const { getReferences, view } = renderPanelWithExtraRows({ edges: [], node }, [
    { edge: "extra edge", href: "/somewhere/r1", key: "r1", kind: "Extra kind" },
  ]);
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };
  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByRole("heading", { name: "Referenced by (0)" });
  const extraList = panel.querySelector<HTMLElement>("[data-extra-references]");
  expect(extraList).not.toBeNull();
  const edgeLine = within(extraList as HTMLElement).getByText("extra edge").parentElement;
  expect(edgeLine).toHaveTextContent(/^extra edge$/);
  expect(extraList?.querySelector("time")).toBeNull();
});

// The host's rows are drawn whole: the list clamps only an edge's excerpt of its own.
test("an extra row draws its excerpt whole, with no Show more", async () => {
  const node = {
    ...graphNodeFields(),
    id: "CORE-1",
    issue_key: "CORE-1",
    kind: "issue",
    ref: "dispatch://CORE-1",
  } satisfies GraphReferences["node"];
  // Five lines and 339 characters: past both bounds beyond which the list clamps an edge's
  // excerpt and offers to expand it (`CLAMP_LINES`, 3, and `CLAMP_CHARACTERS`, 240).
  const words = Array.from(
    { length: 5 },
    (_, line) => `Line ${line + 1} of the question asks when the release ships and who owns it.`
  ).join("\n");
  const { getReferences, view } = renderPanelWithExtraRows({ edges: [], node }, [
    { edge: "extra edge", excerpt: words, href: "/somewhere/r1", key: "r1", kind: "Extra kind" },
  ]);
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };
  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByRole("heading", { name: "Referenced by (0)" });
  const extraList = panel.querySelector<HTMLElement>("[data-extra-references]");
  if (extraList === null) throw new Error("the list draws no extra row");
  const excerpt = extraList.querySelector("p");
  if (excerpt === null) throw new Error("the extra row draws no excerpt");
  expect(excerpt).toHaveTextContent(words, { normalizeWhitespace: false });
  expect(excerpt).not.toHaveClass("line-clamp-3");
  expect(within(extraList).queryByRole("button", { name: "Show more" })).not.toBeInTheDocument();
});

/** `ReferencedBy` as Details draws it (`page`), in the pages Details holds its rows in
 *  (`useListPages`), with rows the host adds after the list's own. */
function PagedPanel({ extraRows }: { extraRows: readonly ExtraReferenceRow[] }): ReactNode {
  const pages = useListPages();
  return <ReferencedBy extraRows={extraRows} page pages={pages} reference="dispatch://CORE-1" />;
}

// With `page` and `pages` the host's rows join the list's one sequence after its own: the first
// page draws the list's own rows and then the host's, up to 20 rows in all, one Show more under
// them all counts what is left of both, and its press draws the rest, with the focus on the first
// row it drew - here a row of the host's, whose link takes it.
test("extra rows stand in the list's pages of 20 after its own rows, under one control", async () => {
  const user = userEvent.setup();
  const node = {
    ...graphNodeFields(),
    id: "CORE-1",
    issue_key: "CORE-1",
    kind: "issue",
    ref: "dispatch://CORE-1",
  } satisfies GraphReferences["node"];
  // 18 comments of OPS-1, each a source of its own with one row.
  const edges = Array.from({ length: 18 }, (_, index) =>
    itemBlockEdge(
      "comment",
      `11111111-1111-4111-8111-${String(index + 1).padStart(12, "0")}`,
      "b1",
      `Comment ${index + 1} cites dispatch://CORE-1.`
    )
  );
  const extraRows = [1, 2, 3, 4, 5].map((n) => ({
    edge: "extra edge",
    href: `/somewhere/r${n}`,
    key: `r${n}`,
    kind: `Extra kind ${n}`,
  }));
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({ edges, node });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <PagedPanel extraRows={extraRows} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  restore = () => {
    getReferences.mockRestore();
    view.unmount();
  };

  const panel = await screen.findByRole("region", { name: "Referenced by" });
  await within(panel).findByText("Comment 1 cites dispatch://CORE-1.");
  // Each row of the list, its own and the host's, is a row of a card (`rowAt`).
  const rowsSelector = ":scope > ul > li > ul > li";
  expect(panel.querySelectorAll(rowsSelector), "the first page does not draw 20 rows").toHaveLength(
    20
  );
  const extraList = panel.querySelector<HTMLElement>("[data-extra-references]");
  if (extraList === null) throw new Error("the first page draws no row of the host's");
  expect(
    within(extraList)
      .getAllByRole("link")
      .map((link) => link.textContent),
    "the first page does not draw the host's rows after the list's own, up to 20 rows in all"
  ).toEqual(["Extra kind 1", "Extra kind 2"]);
  const more = within(panel).getAllByRole("button", { name: SHOW_MORE });
  expect(more, "the list does not draw one Show more under all its rows").toHaveLength(1);
  expect(more[0]?.textContent, "Show more does not count the rows of both left").toBe(
    showMoreName(3)
  );

  await user.click(more[0] as HTMLElement);
  expect(
    panel.querySelectorAll(rowsSelector),
    "Show more did not draw the rest of the rows"
  ).toHaveLength(23);
  expect(
    within(extraList).getByRole("link", { name: "Extra kind 3" }),
    "Show more did not move the focus to the link of the first row it drew"
  ).toHaveFocus();
  expect(
    within(panel).queryByRole("button", { name: SHOW_MORE }),
    "Show more stayed once every row was drawn"
  ).not.toBeInTheDocument();
});
