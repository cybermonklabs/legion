import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { type FakeDocumentRuntime, fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { graphNode } from "../../__tests__/graph-fixture";
import { answerRequests } from "../../__tests__/network";
import {
  answerTextReadsWithTheSeededText,
  artifactRecordHandler,
  createQueryClient,
  documentSurfaceHandlers,
} from "../../__tests__/proof-document";
import { api } from "../../api/client";
import type {
  ArtifactBlock,
  GraphReferences,
  IssueCoverage,
  IssueDetails,
  IssueReferences,
} from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { parseIssuePath } from "../refs/routes";
import { DOCUMENTS_LIST_ATTRIBUTE } from "./DocumentsList";
import {
  artifact,
  issueDetails,
  KEY,
  SPEC_SLUG,
  specRef,
} from "./decisions-filter/decisions-fixtures";
import { CONTROL_LABELS } from "./page-words";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { AppProviders, focusedRow, stubReads } from "./slots/stage-fixtures";
import { blockNodeId } from "./tree-drafts";

/**
 * What an address to a block of a document opens (dispatch://INFRA-1196 part N's R6, decision
 * 449): the document in place of the outline, with the focus on the block, where the outline
 * draws no node of that block - a task of a document of tasks that is no plan of the issue,
 * which the outline is not built from - and the outline on the block's node where it does. The
 * stage is drawn at the address, as `opened-document.test.tsx` draws it, with a fake room for
 * the document's editor.
 */

answerTextReadsWithTheSeededText();

/** A document of the issue that plays no part: the outline is built from the spec, the plan, the
 *  Verification document and the decisions document alone (`useIssueTree.ts`'s
 *  `treeDocumentIds`), so it holds no node of any block of this one. */
const TASKS = artifact("artifact-tasks", "plan-one-plan-md");
const ISSUE: IssueDetails = {
  ...issueDetails(),
  artifacts: [...(issueDetails().artifacts ?? []), TASKS],
};

/** A typed block of the tasks document, and a typed block of the spec. */
const TASK = "t-far";
const STATEMENT = "r1";

/** One typed block of a document as the blocks read answers it. */
function typedBlock(id: string, type: string): ArtifactBlock {
  return {
    attributes: [],
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type,
  };
}

/** Each document's blocks, by its id. */
const BLOCKS: Readonly<Record<string, readonly ArtifactBlock[]>> = {
  "artifact-spec": [typedBlock(STATEMENT, "requirement")],
  [TASKS.id]: [typedBlock(TASK, "plan_task")],
};

/** The coverage read with the spec's one statement, which is what the outline draws its node
 *  from. */
const COVERAGE: IssueCoverage = {
  gaps: [],
  has_plan: false,
  issue: KEY,
  requirements: [
    {
      block_id: STATEMENT,
      checks: 0,
      gaps: [],
      plan_tasks: 0,
      ref: specRef(STATEMENT),
      text: "The page draws the statement.",
    },
  ],
  spec_artifact_id: "artifact-spec",
};

let stubs: { mockRestore(): void }[] = [];
beforeEach(() => {
  stubs = [
    ...stubReads(),
    spyOn(api, "getIssueCoverage").mockResolvedValue(COVERAGE),
    spyOn(api, "getArtifactBlocks").mockImplementation(async (id: string) => [
      ...(BLOCKS[id] ?? []),
    ]),
  ];
  // The document's own reads, and what references it and the issue, which nothing does here.
  answerRequests(
    http.get("/api/v1/issues/:key/references", () =>
      Response.json({
        linked_from: [],
        links: [],
        members: [],
        truncated: false,
      } satisfies IssueReferences)
    ),
    http.get("/api/v1/references", ({ request }) =>
      Response.json({
        edges: [],
        node: graphNode({
          id: new URL(request.url).searchParams.get("to") ?? "",
          kind: "artifact",
        }),
      } satisfies GraphReferences)
    ),
    ...documentSurfaceHandlers(),
    artifactRecordHandler((id) => (ISSUE.artifacts ?? []).find((one) => one.id === id))
  );
});
afterEach(() => {
  cleanup();
  for (const stub of stubs) stub.mockRestore();
});

/** The page's stage at the address the router holds, inside the page's root, as the page's root
 *  draws it. */
function Page(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <div data-issue-page="">
      <ProvidersSlot issue={ISSUE} route={route}>
        <StageSlot issue={ISSUE} route={route} />
      </ProvidersSlot>
    </div>
  );
}

/** The stage at `address`, which the window holds as its own fragment as a browser does: the
 *  document's landing on a block reads the fragment from the window (`ProofDocument`'s
 *  `followBlockHash`), and a memory router leaves the window's untouched. Answers what takes the
 *  stage away and puts the fragment back. */
function drawAt(address: string, fake: FakeDocumentRuntime): () => void {
  const fragment = window.location.hash;
  window.history.replaceState(null, "", address.slice(address.indexOf("#")));
  const client = createQueryClient();
  const view = render(
    <MemoryRouter initialEntries={[address]}>
      <AppProviders client={client}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <Page />
        </DocumentRuntime.Provider>
      </AppProviders>
    </MemoryRouter>
  );
  return () => {
    view.unmount();
    client.clear();
    window.history.replaceState(null, "", fragment || "/");
  };
}

test("an address to a block of a document the outline does not draw opens the document with the focus on that block", async () => {
  const fake = fakeDocumentRuntime({ text: "The tasks" });
  const undraw = drawAt(`/issues/${KEY}/artifacts/${TASKS.slug}#b-${TASK}`, fake);
  try {
    await waitFor(() =>
      expect(
        screen.queryByTestId("opened-artifact"),
        "the address of a typed block of a document no node of the outline stands for opened no document"
      ).not.toBeNull()
    );
    await waitFor(() => expect(fake.connections).toHaveLength(1));
    await act(async () => fake.sync());
    await waitFor(() =>
      expect(fake.editors[0]?.focusedBlocks, "the document did not focus the block").toContain(TASK)
    );
    expect(
      screen.queryByRole("tree", { name: CONTROL_LABELS.tree }),
      "the outline is drawn beside the document"
    ).toBeNull();
    expect(
      document.querySelector(`[${DOCUMENTS_LIST_ATTRIBUTE}]`),
      "Documents' list is drawn in place of the document"
    ).toBeNull();
  } finally {
    undraw();
  }
});

test("an address to a typed block the outline draws opens the outline on its node", async () => {
  const fake = fakeDocumentRuntime({ text: "The spec" });
  const undraw = drawAt(`/issues/${KEY}/artifacts/${SPEC_SLUG}#b-${STATEMENT}`, fake);
  try {
    await waitFor(() =>
      expect(focusedRow(), "the outline did not take the focus on the block's node").toBe(
        blockNodeId(SPEC_SLUG, STATEMENT)
      )
    );
    expect(screen.queryByTestId("opened-artifact"), "the address opened the document").toBeNull();
    expect(fake.editors, "a document drew an editor").toHaveLength(0);
  } finally {
    undraw();
  }
});
