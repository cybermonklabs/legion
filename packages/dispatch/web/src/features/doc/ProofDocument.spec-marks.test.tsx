import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import type { IssueTreeRead } from "@legion/contracts";
import { act, cleanup, waitFor, within } from "@testing-library/react";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { graphNode } from "../../__tests__/graph-fixture";
import {
  answerTextReadsWithTheSeededText,
  artifact,
  createQueryClient,
  renderProofDocument,
} from "../../__tests__/proof-document";
import { answerRoute } from "../../__tests__/send-answers";
import { api, type WriteBounds } from "../../api/client";
import type {
  Artifact,
  ArtifactBlock,
  DocumentRead,
  GraphReferences,
  IssueDecisions,
  Version,
} from "../../api/types";
import { changedSinceReadMark, staleBlockMark } from "../../theme/classes";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { STALE_BLOCK_TITLE } from "./spec-marks";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

answerTextReadsWithTheSeededText();

// ---------------------------------------------------------------------------------------------
// The Spec tab's marks: each block a decision shaped that a later change of the decision left
// out of date, and each block that changed since the version the person last read, set through
// the editor handle (never written into the editor's DOM), the one line naming the changed
// sections above the document, and the read mark the tab then records.
// ---------------------------------------------------------------------------------------------

function version(number: number): Version {
  return {
    authors: [],
    created_at: `2026-10-0${number}T00:00:00Z`,
    named: false,
    number,
    summary: null,
  };
}

const spec: Artifact = { ...artifact, versions: [1, 2, 3, 4].map(version) };

// One decision whose `shaped` tie to p-plan was written by event 10, after p-plan's text last
// changed (event 9), and whose own words changed after the tie (version 3): the link is stale, and
// p-plan has not changed since the tie.
const decisions: IssueDecisions = {
  decisions: [],
  document: null,
  entries: [
    {
      answer: null,
      answer_state: null,
      answered_at: null,
      answered_by: null,
      changed_at: "2026-10-04T00:00:00Z",
      changed_version: 3,
      decided_by: { id: "session-1", kind: "session" },
      declined: false,
      declined_note: null,
      document: {
        artifact_id: "choices-id",
        block: "CORE-1/choices-md#d-1",
        block_id: "d-1",
        ref: "dispatch://CORE-1/artifact/choices-md",
      },
      from_ask: [],
      kept: null,
      kept_by: null,
      kept_text_sum: null,
      kind: "agent",
      multiple: null,
      node: { id: "CORE-1/choices-md#d-1", kind: "block" },
      options: null,
      question: null,
      readings: [],
      ref: "dispatch://CORE-1/artifact/choices-md#b-d-1",
      replaced: false,
      resolution: null,
      selected: null,
      shaped: [
        {
          changed_since_tie: false,
          created_at: "2026-10-01T00:00:00Z",
          created_version: 1,
          event: 10,
          from_block: "d-1",
          stale: true,
          text_changed_event: 9,
          // A block of the issue room: the room's node, the spec named as the document holding it.
          to: graphNode({
            block_id: "p-plan",
            document: "dispatch://CORE-1/spec",
            id: "CORE-1/tree#p-plan",
            kind: "block",
          }),
        },
      ],
      text: ":::decision{#d-1}\nShip it in two steps.\n:::\n",
      text_sha256: "sum-d-1",
      uncertainty: null,
      uncertainty_reason: null,
      urgency: null,
      waiting_review: true,
    },
  ],
  open_questions: [],
};

function part(blockId: string, changedVersion: number, text: string) {
  return {
    created_at: "2026-10-01T00:00:00Z",
    direction: "in" as const,
    excerpt: { block_id: blockId, text },
    kind: "part_of",
    node: graphNode({
      block_id: blockId,
      changed_version: changedVersion,
      id: `CORE-1/spec#${blockId}`,
      kind: "block" as const,
    }),
    source_seq: null,
  };
}

const parts: GraphReferences = {
  edges: [
    part("h-plan", 1, "## Plan"),
    part("p-plan", 2, "Ship it in two steps."),
    part("h-rollout", 1, "## Rollout"),
    part("p-rollout", 4, "Behind a flag."),
  ],
  node: graphNode({ id: "artifact-1", kind: "artifact" }),
};

function block(id: string, type: string, from: number): ArtifactBlock {
  return {
    from,
    id,
    references: { asks: 0, comments: 0 },
    to: from + 10,
    token: `sha256:${id}`,
    type,
  };
}

const blocks = [
  block("h-plan", "heading", 0),
  block("p-plan", "paragraph", 10),
  block("h-rollout", "heading", 20),
  block("p-rollout", "paragraph", 30),
];

function readOf(version: number | null): DocumentRead {
  return {
    artifact_id: "artifact-1",
    read_at: version === null ? null : "2026-10-02T00:00:00Z",
    version,
  };
}

function stubReads(readVersion: number | null) {
  const getArtifact = spyOn(api, "getArtifact").mockResolvedValue(spec);
  const getIssueDecisions = spyOn(api, "getIssueDecisions").mockResolvedValue(decisions);
  const getDocumentRead = spyOn(api, "getDocumentRead").mockResolvedValue(readOf(readVersion));
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(parts);
  const getArtifactBlocks = spyOn(api, "getArtifactBlocks").mockResolvedValue(blocks);
  // The margin reads the issue's asks for every document it shows; none of these rows has one.
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const putRead = answerRoute<
    (id: string, body: { version: number }, bounds: WriteBounds) => Promise<unknown>
  >("PUT", "/api/v1/me/documents/*/read");
  putRead.mockImplementation(async (_id, body) => readOf(body.version));
  return {
    getDocumentRead,
    getIssueDecisions,
    getReferences,
    putRead,
    restore() {
      for (const spy of [
        getArtifact,
        getIssueDecisions,
        getDocumentRead,
        getReferences,
        getArtifactBlocks,
        listIssueAsks,
      ]) {
        spy.mockRestore();
      }
    },
  };
}

test("the Spec tab marks stale and changed blocks through the editor, names the changed sections, then records the version it shows", async () => {
  jest.useFakeTimers();
  const reads = stubReads(2);
  const { editors, sync, view } = renderProofDocument({
    document: spec,
    queryClient: createQueryClient(),
  });
  const screen = within(view.container);

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");

    // The read version is the one the person had before this visit: the blocks above it are
    // marked, and the line names their sections.
    const line = await screen.findByText("Changed since you last read version 2: Rollout");
    expect(line).toHaveAttribute("role", "status");
    await waitFor(() =>
      expect(editor.blockMarks.at(-1)).toEqual([
        { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
        { blockId: "p-rollout", className: changedSinceReadMark },
      ])
    );
    expect(reads.getIssueDecisions).toHaveBeenCalledWith("CORE-1");
    expect(reads.getDocumentRead).toHaveBeenCalledWith("artifact-1");
    expect(reads.getReferences).toHaveBeenCalledWith("dispatch://CORE-1/spec", {
      kind: "part_of",
    });
    // No class is written into the editor's DOM: every mark goes through the handle.
    expect(editor.root.querySelector(".dispatch-block-stale")).toBeNull();

    // Then the tab records the version it shows, once.
    await waitFor(() => expect(reads.putRead).toHaveBeenCalledTimes(1));
    expect(reads.putRead.mock.calls[0]?.slice(0, 2)).toEqual(["artifact-1", { version: 4 }]);

    // The marks stay for the visit, though the person's read version is now the latest.
    await tick(20);
    expect(screen.getByText("Changed since you last read version 2: Rollout")).toBeDefined();
    expect(editor.blockMarks.at(-1)).toEqual([
      { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
      { blockId: "p-rollout", className: changedSinceReadMark },
    ]);
  } finally {
    realClock();
    cleanup();
    reads.restore();
  }
});

test("with no read version the Spec tab shows no line and marks no changed block, and still records its read", async () => {
  const reads = stubReads(null);
  const { editors, sync, view } = renderProofDocument({
    document: spec,
    queryClient: createQueryClient(),
  });
  const screen = within(view.container);

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");

    await waitFor(() => expect(reads.putRead).toHaveBeenCalledTimes(1));
    expect(reads.putRead.mock.calls[0]?.slice(0, 2)).toEqual(["artifact-1", { version: 4 }]);
    await waitFor(() =>
      expect(editor.blockMarks.at(-1)).toEqual([
        { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
      ])
    );
    expect(screen.queryByText(/Changed since you last read/)).not.toBeInTheDocument();
  } finally {
    cleanup();
    reads.restore();
  }
});

test("the Decisions tree view marks the blocks named by stale links", async () => {
  const read: IssueTreeRead = {
    converted: true,
    cursor: null,
    documents: null,
    issue: "CORE-1",
    nodes: [
      {
        attributes: {},
        author: [],
        block_id: "d-two-steps",
        decision: {
          asks: [],
          block_id: "d-two-steps",
          changed_at: "2026-10-04T00:00:00Z",
          changed_version: 3,
          heading: "Ship it in two steps",
          kept: null,
          kind: "agent",
          links: decisions.entries?.[0]?.shaped ?? [],
          ref: "dispatch://CORE-1/spec#b-d-two-steps",
          replaced: false,
          text: "The flag goes first.",
          text_sha256: "sum-two-steps",
          unsure: false,
          waiting_review: true,
        },
        id: "CORE-1/spec#d-two-steps",
        kind: "block",
        made_at: "2026-10-04T00:00:00Z",
        parent: null,
        rank: null,
        role: "query",
        state: "live",
        supertag: "decision",
        text: "The flag goes first.",
        visibility: "project",
      },
    ],
    tree: "tree-1",
    tuples: [],
    version: null,
    view: "decisions",
    view_of: null,
  };
  const reads = stubReads(null);
  reads.getIssueDecisions.mockRestore();
  const getIssueTree = spyOn(api, "getIssueTree").mockResolvedValue(read);
  const { editors, sync } = renderProofDocument({
    document: spec,
    queryClient: createQueryClient(),
  });

  try {
    await sync();
    await waitFor(() => expect(editors).toHaveLength(1));
    const editor = editors[0];
    if (editor === undefined) throw new Error("editor missing");

    await waitFor(() => expect(reads.putRead).toHaveBeenCalledTimes(1));
    expect(getIssueTree).toHaveBeenCalledWith("CORE-1", { view: "decisions" });
    await waitFor(() =>
      expect(editor.blockMarks.at(-1)).toEqual([
        { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
      ])
    );
  } finally {
    cleanup();
    getIssueTree.mockRestore();
    reads.restore();
  }
});

// A read of the person's read mark asked before the Spec tab's mark lands answers the version they
// read before this visit. Landing after the mark's answer, it would put that version back, and the
// next visit would mark every block changed since it again, though the person has read the latest.
test("a read of the read mark asked before the mark landed does not bring back the version read before", async () => {
  const reads = stubReads(2);
  const before = Promise.withResolvers<DocumentRead>();
  // Version 2 as the visit opens, then the read held while the mark is out; after that, the
  // server holds the version the mark recorded.
  reads.getDocumentRead
    .mockResolvedValue(readOf(4))
    .mockResolvedValueOnce(readOf(2))
    .mockReturnValueOnce(before.promise);
  const marked = Promise.withResolvers<DocumentRead>();
  reads.putRead.mockImplementationOnce(() => marked.promise);
  const queryClient = createQueryClient();
  const first = renderProofDocument({ document: spec, queryClient });

  try {
    await first.sync();
    await waitFor(() => expect(reads.putRead).toHaveBeenCalledTimes(1));
    // A refresh of the read mark, asked while the mark is out and still out itself.
    await act(async () => {
      void queryClient.invalidateQueries({ exact: true, queryKey: ["document-read", spec.id] });
      await settle();
    });
    expect(reads.getDocumentRead).toHaveBeenCalledTimes(2);
    await act(async () => {
      marked.resolve(readOf(4));
      await settle();
    });
    await waitFor(() =>
      expect(queryClient.getQueryData<DocumentRead>(["document-read", spec.id])).toEqual(readOf(4))
    );

    // The read answers the version the person had read before this visit.
    await act(async () => {
      before.resolve(readOf(2));
      await settle();
    });
    await waitFor(() =>
      expect(queryClient.getQueryState(["document-read", spec.id])?.fetchStatus).toBe("idle")
    );
    first.view.unmount();

    // The next visit opens on the version the mark recorded, the latest: nothing changed since.
    const next = renderProofDocument({ document: spec, queryClient });
    await next.sync();
    await waitFor(() => expect(next.editors).toHaveLength(1));
    const editor = next.editors[0];
    if (editor === undefined) throw new Error("editor missing");
    await waitFor(() =>
      expect(editor.blockMarks.at(-1)).toEqual([
        { blockId: "p-plan", className: staleBlockMark, title: STALE_BLOCK_TITLE },
      ])
    );
    expect(
      within(next.view.container).queryByText(/Changed since you last read/)
    ).not.toBeInTheDocument();
  } finally {
    cleanup();
    reads.restore();
  }
});
