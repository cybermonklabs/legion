import { afterEach, beforeAll, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Profiler, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ReferenceNamesProvider } from "../refs/reference-names";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import {
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  SPEC_DOCUMENT,
  statement,
} from "./decisions-filter/decisions-fixtures";
import {
  DocumentAdmissionProvider,
  type ReportAdmission,
  useReportAdmission,
} from "./document-admission";
import { DocumentStages } from "./document-stage";
import { FocusedNodeProvider } from "./focused-node";
import { useOutlineEditing } from "./IssuePresetPanels";
import { IssueTree } from "./IssueTree";
import { readComment, waitInAct } from "./outline-fixtures";
import { Publisher, publishedTree } from "./projection-fixture";
import { recordRowDraws, treeOfComments } from "./row-draw-fixtures";
import { drawStage, stubReads, treeOf } from "./slots/stage-fixtures";
import * as treeRows from "./TreeRows";
import type { OnEditing } from "./useNodeEditing";

/**
 * Enter on a row draws the row and nothing else of the outline, and so does what the editor does
 * as it opens: the room's admission it reports, and an answer of a read that holds nothing new
 * for a row.
 *
 * What a case counts is what the outline draws again: each render of `TreeRows` (counted by a
 * stand-in for its export, which `IssueTree.tsx`'s import reads from then on), each commit in
 * which anything under the tree rendered (a `Profiler` around it), and each commit that draws a
 * row's words again (`RowText` under a `Profiler` of its own, named by the row's node, as
 * `row-draw-fixtures.tsx`'s `recordRowDraws` records them). A row that is not edited draws its
 * words again whenever it is drawn again, since `RowText` is no `memo`.
 */

/** `TreeRows` as its module defines it, read before a case's spy stands in for the export. */
const TreeRowsItself = treeRows.TreeRows;

/** What the outline drew again since a case began counting. */
interface Draws {
  /** How many times `TreeRows` rendered. */
  readonly trees: () => number;
  /** How many commits drew each row's words, by the row's node id. */
  readonly rows: () => Readonly<Record<string, number>>;
  readonly restore: () => void;
}

/** Stands in for `RowText` and `TreeRows`, counting what each is drawn for from now until
 *  `restore`. */
function countDraws(): Draws {
  let trees = 0;
  const rowDraws = recordRowDraws();
  const treeSpy = spyOn(treeRows, "TreeRows").mockImplementation(function CountedTreeRows(props) {
    trees += 1;
    return <TreeRowsItself {...props} />;
  });
  return {
    restore: () => {
      rowDraws.restore();
      treeSpy.mockRestore();
    },
    rows: () => rowDraws.since(0),
    trees: () => trees,
  };
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

let stubs: { mockRestore(): void }[] = [];
beforeEach(() => {
  // Nothing answers who is signed in, so a document the page draws stays "Loading document…".
  stubs = [spyOn(api, "whoAmI").mockReturnValue(new Promise(() => {}))];
});
afterEach(() => {
  cleanup();
  for (const stub of stubs) stub.mockRestore();
});

/** The page's own `issue` scope, which every one of the tree's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** How the page's row editor reports what a document's room admitted the page to, handed out for
 *  a case to answer as the room. */
function AdmissionProbe({ held }: { held: { report?: ReportAdmission } }): ReactNode {
  held.report = useReportAdmission();
  return null;
}

/** The ids of the statements the cases draw, the spec's own blocks, each a row of an editable
 *  document. */
const BLOCKS = ["r1", "r2", "r3", "r4", "r5"];

/** The outline's tree of `BLOCKS`' statements under the page's providers, with what it drew
 *  again, as `IssueTree.test.tsx` draws one. `edited` is each document `onEditing` was told a
 *  row is being edited in, `admit` answers as a document's room, and `commits` counts the commits
 *  in which anything under the tree rendered. */
function drawTree() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const root = issueTree(BLOCKS.map((block) => statement(block)));
  const published = publishedTree(root);
  const artifacts = new Map([[SPEC_DOCUMENT.slug, SPEC_DOCUMENT]]);
  const edited: (string | undefined)[] = [];
  const admission: { report?: ReportAdmission } = {};
  const commits = { count: 0 };
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <FocusedNodeProvider>
              <DocumentAdmissionProvider>
                <DocumentStages>
                  <IssueScope />
                  <AdmissionProbe held={admission} />
                  <Publisher published={published} />
                  <Profiler
                    id="outline"
                    onRender={() => {
                      commits.count += 1;
                    }}
                  >
                    <IssueTree
                      artifacts={artifacts}
                      context={new Set()}
                      focusReveal={false}
                      holdsEscape={false}
                      isClosed={false}
                      onEditing={(artifactId) => edited.push(artifactId)}
                      published={published}
                      reveal={undefined}
                      root={root}
                      rootRow
                    />
                  </Profiler>
                </DocumentStages>
              </DocumentAdmissionProvider>
            </FocusedNodeProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>,
    { wrapper: ReferenceNamesProvider }
  );
  return {
    admit: (artifactId: string, readOnly: boolean) =>
      act(async () => admission.report?.(artifactId, readOnly)),
    commits: () => commits.count,
    edited,
    ...view,
  };
}

/** The outline's row of the spec's block `block`. */
function rowOf(view: RenderResult, block: string): HTMLElement {
  const row = view.container.querySelector<HTMLElement>(
    `[data-node-row][data-node-id="spec#b-${block}"]`
  );
  if (row === null) throw new Error(`no row of ${block} was drawn`);
  return row;
}

// The state change Enter makes lives below the outline: `RowEditing` holds it and the row being
// edited reads it for itself. At the base it was `IssueTree`'s own state, so each Enter drew the
// tree - `TreeRows` and every row it hands props - before the caret was placed.
test("Enter on a row draws that row's editor and neither the tree nor another row", async () => {
  const draws = countDraws();
  const user = userEvent.setup();
  const view = drawTree();
  try {
    const row = rowOf(view, "r3");
    await act(async () => row.focus());
    await waitInAct(100);
    const trees = draws.trees();
    const before = draws.rows();
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(view.edited.at(-1), "no document was named to edit").toBe(SPEC_DOCUMENT.id)
    );
    await waitInAct(100);
    expect(
      row.querySelector("[data-node-words] .clear-right"),
      "the edited row draws no stage for its editor"
    ).not.toBeNull();
    expect(draws.trees() - trees, "the tree was drawn again by Enter").toBe(0);
    expect(draws.rows(), "a row's words were drawn again by Enter").toEqual(before);
  } finally {
    view.unmount();
    draws.restore();
  }
});

// The editor reports what its room admitted the page to as it is made, which was one value in a
// context `TreeRows` read, so the first admission to write - which changes no row - drew the tree
// and every row of it again, about two seconds after the caret on a copy of a long issue.
test("the editor's room admitting the page to write draws nothing of the outline", async () => {
  const draws = countDraws();
  const user = userEvent.setup();
  const view = drawTree();
  try {
    const row = rowOf(view, "r3");
    await act(async () => row.focus());
    await user.keyboard("{Enter}");
    await waitFor(() =>
      expect(view.edited.at(-1), "no document was named to edit").toBe(SPEC_DOCUMENT.id)
    );
    await waitInAct(100);
    const trees = draws.trees();
    const commits = view.commits();
    const before = draws.rows();
    await view.admit(SPEC_DOCUMENT.id, false);
    expect(draws.trees() - trees, "the tree was drawn again by the admission").toBe(0);
    expect(view.commits() - commits, "something under the tree was drawn again").toBe(0);
    expect(draws.rows(), "a row's words were drawn again by the admission").toEqual(before);
    expect(
      [...view.container.querySelectorAll("[data-node-row][data-editable]")].length,
      "a row of the document its room admitted to write lost its mark"
    ).toBe(BLOCKS.length);
  } finally {
    view.unmount();
    draws.restore();
  }
});

// The page's body holds no state of the row being edited either: `useOutlineEditing` names the
// document in a store the editor's own component reads, so Enter's `onEditing` draws the editor
// and not the body that holds the outline. At the base it was the body's state, so the body, and
// the outline under it, were drawn again as the editor was asked for, and again as it was torn
// down.
test("naming the document a row is edited in draws the editor and not the body", async () => {
  const issue: IssueDetails = issueDetails();
  const held: { onEditing?: OnEditing } = {};
  const body = { renders: 0 };
  function Body(): ReactNode {
    body.renders += 1;
    const { onEditing, panels } = useOutlineEditing(issue);
    held.onEditing = onEditing;
    return panels;
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <Body />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    expect(screen.queryByText("Loading document…")).toBeNull();
    const renders = body.renders;
    await act(async () => held.onEditing?.(SPEC_DOCUMENT.id));
    expect(await screen.findByText("Loading document…"), "no editor was drawn").not.toBeNull();
    await act(async () => held.onEditing?.(undefined));
    expect(screen.queryByText("Loading document…"), "the editor was not taken away").toBeNull();
    expect(body.renders - renders, "the body was drawn again by the editing").toBe(0);
  } finally {
    view.unmount();
  }
});

// An answer of the decisions and of the asks that holds what the read held, in objects that are all
// new, builds the tree again: the page's nodes are new objects, though no row holds anything new.
// No row is drawn again (`NodeRow`'s `memo`, `IssueTree.tsx`'s `sharedTree`). The cases that count
// rows here would have every row of a long issue drawn again at the base, where each of those
// answers was one more whole-outline render.
test("an answer of the decisions and the asks that changes nothing for a row draws no row", async () => {
  stubs.push(...stubReads());
  const comments = [
    readComment("head-a", 1),
    readComment("reply-a", 2, "head-a"),
    readComment("head-b", 3),
  ];
  const tree = treeOfComments(comments);
  const getIssueDecisions = spyOn(api, "getIssueDecisions").mockImplementation(async () =>
    decisionsRead([])
  );
  const listIssueAskThreads = spyOn(api, "listIssueAskThreads").mockImplementation(async () => []);
  stubs.push(
    getIssueDecisions,
    listIssueAskThreads,
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === undefined ? tree : treeOf([])
    )
  );
  const restoreViewport = stubViewport(DESKTOP_WIDTH);
  // No answer keeps the object the cache held, as one that changed nothing does not by default.
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, structuralSharing: false } },
  });
  const draws = countDraws();
  const view = drawStage("", { client, preset: "conversation" });
  try {
    await waitFor(() => {
      expect(client.isFetching(), "a read the page sent is still out").toBe(0);
      expect(view.container.querySelector('[data-node-row][data-node-id="head-b"]')).not.toBeNull();
    });
    await waitInAct(100);
    const before = draws.rows();
    const answered = getIssueDecisions.mock.calls.length + listIssueAskThreads.mock.calls.length;
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["decisions", KEY] });
      await client.invalidateQueries({ queryKey: ["asks", KEY, "thread"] });
    });
    await waitInAct(100);
    expect(
      getIssueDecisions.mock.calls.length + listIssueAskThreads.mock.calls.length - answered,
      "the two reads were not answered again"
    ).toBe(2);
    expect(
      draws.rows(),
      "a row's words were drawn again by an answer that changed nothing"
    ).toEqual(before);
  } finally {
    view.unmount();
    draws.restore();
    restoreViewport();
  }
});
