import { expect, jest, test } from "bun:test";
import type { BlockSchemaRead, LensInvoked, LensKindRead } from "@legion/contracts";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { type ReactNode, useState } from "react";
import {
  MemoryRouter,
  type NavigateFunction,
  Route,
  Routes,
  useLocation,
  useNavigate,
} from "react-router-dom";

import { realClock, tick } from "../../../__tests__/fake-clock";
import { answerRequests } from "../../../__tests__/network";
import { userOnFakeClock } from "../../../__tests__/user";
import { conversationTreeKey } from "../../../api/queries";
import { trackQueryFetches } from "../../../api/query-refresh";
import { pageFocusedRow } from "../../../theme/classes";
import { buildDispatchReference } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useDialog } from "../../shell/useDialog";
import { SEND_DEADLINE_MS } from "../../writes/keyed-write";
import { writesOf } from "../../writes/write-store";
import { node, READER_ACTOR } from "../decisions-filter/decisions-fixtures";
import { buildIssueTree, type TreeNode } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { LENS_ANSWERS, LENS_WORDS, NODE_TYPES, type NodeType } from "../page-words";
import { issueDetails, statementRef } from "../spec-filter/spec-fixtures";
import { TreeRows } from "../TreeRows";
import type { DrawnRow } from "../tree-fold";
import { nodesInOrder, rowKeyOf } from "../tree-keys";
import {
  treeAnswer as routeAnswer,
  type TreeNodeExtra,
  treeNode,
} from "../tree-read/tree-fixtures";
import {
  type TreeAnswer,
  type TreeAnswerNode,
  useTreeRead,
  viewKey,
  viewQuery,
} from "../tree-read/tree-read";
import { lensActFor } from "./LensMenu";

/**
 * The Lens act of a row and its menu of lens kinds.
 *
 * The rows are drawn by `TreeRows`, as the outline draws them, under a router at the address the
 * test names, so the act is offered the way the page offers it; where a case counts the rows, they
 * are the issue's tree read's, built as the page builds its tree (`useTreeRead`,
 * `buildIssueTree`). The catalogue, the invocation and the tree read go through the page's own
 * client (`lens-reads.ts`, `tree-read.ts`), answered at `fetch` by the suite's network
 * (`answerRequests`), so every request the menu sends is one the test answers and counts. A failed
 * read is not retried, as in the page's other rendering tests.
 */

const KEY = "CORE-1";

/** The statement the act is offered on, and its row. */
const STATEMENT = statementRef("r1");
const STATEMENT_ID = "spec/r1";

const statement = node({
  id: STATEMENT_ID,
  reference: STATEMENT,
  text: "Store the notices for a day",
  type: "requirement",
});

/** A second statement, whose row a case moves the focus to. */
const OTHER_ID = "spec/r2";

const other = node({
  id: OTHER_ID,
  reference: statementRef("r2"),
  text: "Send the notices at noon",
  type: "requirement",
});

/** A kind of the catalogue, with the standards of its sources, the first of which the menu
 *  draws. */
function kind(id: string, words: string, standards: readonly string[]): LensKindRead {
  return {
    id,
    ref: `dispatch://lens_kind/${id}`,
    sources: standards.map((standard, index) => ({
      id: `${id}-source-${index}`,
      ref: `dispatch://constraint/9a1b2c3d-000${index}-4e5f-8a9b-0c1d2e3f4a50`,
      reference: `https://example.org/${id}/${index}`,
      standard,
    })),
    words,
  };
}

/** The catalogue in its own order, which is not the order of the kinds' words: the menu keeps
 *  it. The last kind holds no source. */
const CATALOGUE: readonly LensKindRead[] = [
  kind("trade-off", "trade-off analysis", ["ATAM", "Decision matrix"]),
  kind("pre-mortem", "pre-mortem", ["Klein's prospective hindsight"]),
  { ...kind("inversion", "inversion", []), sources: null },
];

function schema(kinds: readonly LensKindRead[]): BlockSchemaRead {
  return { lens_kinds: [...kinds], types: [], version: 1 };
}

/** Answers every catalogue read with what `answer` gives, and counts them. */
function answerCatalogue(answer: () => Response | Promise<Response>): { readonly reads: number } {
  const asked = { reads: 0 };
  answerRequests(
    http.get("/api/v1/schema/blocks", () => {
      asked.reads += 1;
      return answer();
    })
  );
  return asked;
}

/** Answers every invocation on this issue with what `answer` gives, and keeps each body in the
 *  order they came. */
function answerInvocations(answer: () => Response | Promise<Response>): unknown[] {
  const bodies: unknown[] = [];
  answerRequests(
    http.post(`/api/v1/issues/${KEY}/lenses`, async ({ request }) => {
      bodies.push(await request.json());
      return answer();
    })
  );
  return bodies;
}

/** The lens an invocation of `kindId` on the statement made. */
function invoked(kindId: string): LensInvoked {
  return {
    address: "dispatch://lens/0c6f1d2e-9999-4a6b-9c1d-5e2f3a4b5c69",
    author: { id: "alice", kind: "person" },
    created_at: "2026-11-17T09:00:00Z",
    findings: [],
    id: "0c6f1d2e-9999-4a6b-9c1d-5e2f3a4b5c69",
    kind: kindId,
    on: STATEMENT,
    state: "live",
    triggered_by: null,
    verdict: null,
    visibility: "project",
  };
}

/** The rows as `IssueTree` hands them to `TreeRows`, each at the top level: the row a focus
 *  lands in is the focused one. */
function Rows({ isClosed = false, nodes }: { isClosed?: boolean; nodes: readonly TreeNode[] }) {
  const [focused, setFocused] = useState<string | undefined>(undefined);
  const rows: DrawnRow[] = nodes.map((each) => ({ kind: "node", level: 1, node: each, trail: [] }));
  const first = rows[0];
  return (
    <TreeRows
      artifacts={new Map()}
      context={new Set()}
      editing={undefined}
      focused={focused}
      isClosed={isClosed}
      onFocus={(row) => setFocused(row)}
      onPage={() => {}}
      onToggle={() => {}}
      points={new Map()}
      rows={rows}
      tabStop={first === undefined ? undefined : rowKeyOf(first)}
      threads={new Map()}
      unplaced={undefined}
      zoomRoot={undefined}
    />
  );
}

/** Where the router stands, as `Where` last drew it, and the router's own way of moving it, as
 *  a reader's press on a link moves it. */
interface Address {
  at: string;
  go: NavigateFunction | undefined;
}

/** Keeps `address` with the router's, and draws nothing. */
function Where({ address }: { address: Address }): ReactNode {
  const { pathname, search } = useLocation();
  address.at = `${pathname}${search}`;
  address.go = useNavigate();
  return null;
}

/** The rows as the page draws them, on an issue's outline and nowhere else: one of the issue's
 *  views and a page that is no issue's draw none, so a move there unmounts them. */
function Outline({ address, nodes }: { address: Address; nodes: readonly TreeNode[] }) {
  return (
    <>
      <Routes>
        <Route element={<Rows nodes={nodes} />} path="/issues/:key" />
        <Route element={null} path="*" />
      </Routes>
      <Where address={address} />
    </>
  );
}

/** The issue of `spec-filter/spec-fixtures.ts`, with its spec, whose tree the tree read answers. */
const ISSUE = issueDetails();

/** One node of the tree read, live and seen by the project, with no words unless it holds some. */
function readNode(
  id: string,
  supertag: string,
  parent: string | null,
  extra: TreeNodeExtra = {}
): TreeAnswerNode {
  return treeNode(id, supertag, parent, { made_at: "2026-11-17T09:00:00Z", ...extra });
}

/** The issue's tree read once `lenses` lenses have been made on its one statement, a block of
 *  its spec: the issue, the statement, and each lens in the order it was made. */
function treeAnswer(lenses: number): TreeAnswer {
  const words = "Store the notices for a day";
  return routeAnswer(
    [
      readNode(KEY, "issue", null, { text: ISSUE.title }),
      readNode("r1", "requirement", KEY, {
        document: { artifact_id: "artifact-spec", position: 0, slug: "spec" },
        rank: "0",
        text: words,
        text_tree: {
          content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
          type: "doc",
        },
      }),
      ...Array.from({ length: lenses }, (_unused, made) =>
        readNode(`0c6f1d2e-${made}000-4a6b-9c1d-5e2f3a4b5c60`, "lens", "r1", {
          author: [READER_ACTOR],
          made_at: `2026-11-17T1${made}:00:00Z`,
        })
      ),
    ],
    [],
    { converted: true, tree: "artifact-tree" }
  );
}

/** Answers every tree read of this issue, the first with its statement alone and each one after
 *  it with one lens more, and counts them, keeping the kinds each read names (`?kind=`) and the
 *  view it names (`?view=`), null where it names none: Conversation's read names its kinds, and
 *  Graph's none, and neither names a view. Each read after the first answers once `later` has
 *  settled, where the test holds them back. */
function answerTreeReads(later?: Promise<void>): {
  readonly kinds: string[][];
  readonly reads: number;
  readonly views: (string | null)[];
} {
  const asked: { kinds: string[][]; reads: number; views: (string | null)[] } = {
    kinds: [],
    reads: 0,
    views: [],
  };
  answerRequests(
    http.get(`/api/v1/issues/${KEY}/tree`, async ({ request }) => {
      asked.reads += 1;
      const query = new URL(request.url).searchParams;
      asked.kinds.push(query.getAll("kind"));
      asked.views.push(query.get("view"));
      const lenses = asked.reads - 1;
      if (lenses > 0) await later;
      return Response.json(treeAnswer(lenses));
    })
  );
  return asked;
}

/** The rows of the issue's tree as the page reads and builds it (`useTreeRead`,
 *  `buildIssueTree`), every node at the top level: none until the read answers, and every node
 *  each read answers after it. */
function TreeReadRows() {
  const { answer } = useTreeRead(ISSUE);
  return <Rows nodes={nodesInOrder(buildIssueTree({ issue: ISSUE, tree: answer })).slice(1)} />;
}

/** An observer of the issue's Spec view, as a filter's view read watches it while the filter is
 *  on (`ViewReadProvider`): it reads the view where the view holds no answer, or a stale one. */
function SpecViewRead(): null {
  useQuery(viewQuery(KEY, "spec"));
  return null;
}

/** A client signed in as alice, as the page is, whose reads are held once read and whose failed
 *  read is not retried. */
function signedIn(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return client;
}

/** `page` at `path`, which is this issue's outline unless the test names another address. */
function renderAt(page: ReactNode, path = `/issues/${KEY}`, client = signedIn()): RenderResult {
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <NoTooltips>{page}</NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

function rowOf(view: RenderResult, id = STATEMENT_ID): HTMLElement {
  const row = view.container.querySelector<HTMLElement>(`[data-node-row][data-node-id="${id}"]`);
  if (row === null) throw new Error(`no row is drawn for ${id}`);
  return row;
}

function lensAct(view: RenderResult, id?: string): HTMLButtonElement | null {
  const scope = id === undefined ? view.container : rowOf(view, id);
  return scope.querySelector<HTMLButtonElement>("[data-row-lens]");
}

/** Focuses the row once it is drawn, as `j` or a press does, and answers its act. */
async function focusedAct(view: RenderResult, id = STATEMENT_ID): Promise<HTMLButtonElement> {
  const row = await waitFor(() => rowOf(view, id));
  act(() => row.focus());
  return waitFor(() => {
    const control = lensAct(view, id);
    if (control === null) throw new Error("the focused row draws no Lens act");
    return control;
  });
}

function menuOf(view: RenderResult): HTMLElement | null {
  return view.container.querySelector<HTMLElement>('[role="menu"]');
}

function items(view: RenderResult): HTMLElement[] {
  return [...view.container.querySelectorAll<HTMLElement>('[role="menu"] [role="menuitem"]')];
}

/** What a reader reads of each item, and whose words each part is. */
function itemsRead(view: RenderResult) {
  return items(view).map((item) => {
    const words = item.querySelector("[data-lens-kind-words]");
    const standard = item.querySelector("[data-lens-kind-standard]");
    return {
      standard: standard?.textContent ?? null,
      standardMarked: standard === null ? null : standard.hasAttribute("data-author-text"),
      words: words?.textContent ?? null,
      wordsMarked: words?.hasAttribute("data-author-text") ?? false,
    };
  });
}

/** The items, once the catalogue's answer draws them. */
async function drawnItems(view: RenderResult): Promise<HTMLElement[]> {
  return waitFor(() => {
    const drawn = items(view);
    if (drawn.length === 0) throw new Error("the menu lists no kind");
    return drawn;
  });
}

/** Where the acts' line says a refusal: a status among the acts that is not the menu's line. */
const REFUSAL = '[data-row-acts] [role="status"]:not([data-lens-menu-line])';

/** The one sentence the acts' line says of a refusal, where it says one. */
function refusal(view: RenderResult, id?: string): string | null {
  const scope = id === undefined ? view.container : rowOf(view, id);
  return scope.querySelector(REFUSAL)?.textContent ?? null;
}

/** What the menu's own line says of its reads, beside the menu; null where no menu is open. */
function menuLine(view: RenderResult): string | null {
  return view.container.querySelector("[data-lens-menu-line]")?.textContent ?? null;
}

/** Lets whatever a press would send go out and be answered. */
async function letRequestsSettle(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, 50);
  await act(() => promise);
}

// The menu is read only when it opens, once in a load; it is titled in the page's words
// and lists each kind in the catalogue's order, by its words and its first source's standard as
// the catalogue holds them. The act is one of the row's acts and no writer's act.
test("the menu lists the catalogue in order", async () => {
  const user = userEvent.setup();
  const out = Promise.withResolvers<void>();
  const catalogue = answerCatalogue(async () => {
    await out.promise;
    return Response.json(schema(CATALOGUE));
  });
  const view = renderAt(<Rows nodes={[statement]} />);
  try {
    // Every eligible row draws the Lens act before any row takes the focus; focusing the row adds
    // no text, and the catalogue is still not read until the menu opens. The row's words paint
    // after its first render, so its text is read once they stand.
    await waitFor(() => expect(rowOf(view).textContent).toContain(statement.text));
    const beforeFocus = rowOf(view).textContent;
    const unfocused = lensAct(view, STATEMENT_ID);
    expect(unfocused, "an unfocused row draws no Lens act").not.toBeNull();
    if (unfocused === null) return;
    expect(unfocused.textContent).toBe(LENS_WORDS.act);
    expect(unfocused.hasAttribute("data-page-chrome")).toBe(true);
    expect(unfocused.hasAttribute("data-row-act"), "the act is marked as a writer's act").toBe(
      false
    );
    expect(unfocused.closest("[data-row-acts]"), "the act stands outside the row's acts").not.toBe(
      null
    );
    expect(unfocused.tabIndex, "the act is in the tab order").toBe(-1);
    expect(unfocused.getAttribute("aria-haspopup")).toBe("menu");
    expect(unfocused.getAttribute("aria-expanded")).toBe("false");
    await letRequestsSettle();
    expect(catalogue.reads, "an unfocused row read the catalogue").toBe(0);

    const control = await focusedAct(view);
    expect(control, "focusing the row replaced the Lens act").toBe(unfocused);
    expect(rowOf(view).textContent, "focusing the row changed its text").toBe(beforeFocus);
    await letRequestsSettle();
    expect(catalogue.reads, "the focus read the catalogue").toBe(0);
    await user.click(control);
    const menu = menuOf(view);
    expect(menu, "the press opened no menu").not.toBeNull();
    expect(control.getAttribute("aria-expanded")).toBe("true");
    // Phrasing elements, since the menu stands in the acts' `<p>`.
    expect(menu?.tagName).toBe("SPAN");
    expect(menu?.closest("[data-row-acts]")).not.toBeNull();
    const title = document.getElementById(menu?.getAttribute("aria-labelledby") ?? "");
    expect(title?.textContent).toBe(LENS_WORDS.menu_title);
    expect(title?.hasAttribute("data-page-chrome")).toBe(true);
    expect(menu?.textContent, "the menu holds what it says of its read").toBe(
      LENS_WORDS.menu_title
    );
    expect(menuLine(view)).toBe(LENS_ANSWERS.loading_kinds);
    expect(document.activeElement, "the menu took no focus as it opened").toBe(menu);
    await waitFor(() => expect(catalogue.reads).toBe(1));

    out.resolve();
    const drawn = await drawnItems(view);
    expect(itemsRead(view)).toEqual([
      {
        standard: "ATAM",
        standardMarked: true,
        words: "trade-off analysis",
        wordsMarked: true,
      },
      {
        standard: "Klein's prospective hindsight",
        standardMarked: true,
        words: "pre-mortem",
        wordsMarked: true,
      },
      { standard: null, standardMarked: null, words: "inversion", wordsMarked: true },
    ]);
    expect(drawn.map((item) => item.tagName)).toEqual(["BUTTON", "BUTTON", "BUTTON"]);
    expect(menuLine(view), "the line still says the catalogue is out").toBe("");
    expect(document.activeElement, "the first kind took no focus").toBe(drawn[0]);

    // Up and Down move and wrap; Home and End go to the ends.
    const at = () => drawn.indexOf(document.activeElement as HTMLElement);
    const moves: number[] = [];
    for (const key of [
      "{ArrowDown}",
      "{ArrowDown}",
      "{ArrowDown}",
      "{ArrowUp}",
      "{Home}",
      "{End}",
    ]) {
      await user.keyboard(key);
      moves.push(at());
    }
    expect(moves).toEqual([1, 2, 0, 2, 0, 2]);

    // The next opening draws the held catalogue at once and reads nothing.
    await user.keyboard("{Escape}");
    expect(menuOf(view)).toBeNull();
    await user.click(control);
    expect(itemsRead(view).map((item) => item.words)).toEqual([
      "trade-off analysis",
      "pre-mortem",
      "inversion",
    ]);
    expect(document.activeElement).toBe(items(view)[0]);
    await letRequestsSettle();
    expect(catalogue.reads, "a second opening read the catalogue again").toBe(1);
  } finally {
    out.resolve();
    view.unmount();
  }
});

// A failed read says so and lists no kind; the next opening reads the catalogue again,
// and once it answers, no opening after it reads it.
test("a failed catalogue read says so and reads again at the next opening", async () => {
  const user = userEvent.setup();
  let failing = true;
  const catalogue = answerCatalogue(() =>
    failing
      ? Response.json({ code: "UNAVAILABLE", error: "the store is down" }, { status: 500 })
      : Response.json(schema(CATALOGUE))
  );
  const view = renderAt(<Rows nodes={[statement]} />);
  try {
    const control = await focusedAct(view);
    await user.click(control);
    await waitFor(() => expect(menuLine(view)).toBe(LENS_ANSWERS.kinds_failed));
    // One line with the role `status`, beside the menu in its panel and not inside the menu,
    // which holds the focus: a screen reader reads it out as it changes.
    const line = view.container.querySelector("[data-lens-menu-line]");
    expect(line?.getAttribute("role")).toBe("status");
    expect(line?.hasAttribute("data-page-chrome")).toBe(true);
    expect(line?.closest('[role="menu"]'), "the line is inside the menu").toBeNull();
    expect(line?.parentElement, "the line is not in the menu's panel").toBe(
      menuOf(view)?.parentElement
    );
    expect(menuOf(view)?.textContent).toBe(LENS_WORDS.menu_title);
    expect(catalogue.reads).toBe(1);
    expect(items(view)).toEqual([]);
    expect(document.activeElement, "the menu lost the focus").toBe(menuOf(view));

    await user.keyboard("{Escape}");
    expect(menuOf(view)).toBeNull();
    failing = false;
    await user.click(control);
    await drawnItems(view);
    expect(catalogue.reads, "the next opening did not read the catalogue again").toBe(2);
    expect(menuLine(view), "the line still says the read failed").toBe("");
    expect(itemsRead(view).map((item) => item.words)).toEqual([
      "trade-off analysis",
      "pre-mortem",
      "inversion",
    ]);

    await user.keyboard("{Escape}");
    await user.click(control);
    await letRequestsSettle();
    expect(catalogue.reads, "an opening after the answer read the catalogue again").toBe(2);
  } finally {
    view.unmount();
  }
});

// A pick sends the node's address and the kind's id and nothing more, says that it is out
// while it is, and leaves every item where it is; the 201 closes the menu.
test("a pick sends on and kind alone", async () => {
  const user = userEvent.setup();
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const out = Promise.withResolvers<void>();
  const bodies = answerInvocations(async () => {
    await out.promise;
    return Response.json(invoked("pre-mortem"), { status: 201 });
  });
  const view = renderAt(<Rows nodes={[statement]} />);
  try {
    const control = await focusedAct(view);
    await user.click(control);
    const drawn = await drawnItems(view);
    await user.click(drawn[1] as HTMLElement);
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toStrictEqual({ kind: "pre-mortem", on: STATEMENT });
    expect(menuLine(view)).toBe(LENS_ANSWERS.examining);
    expect(items(view)).toHaveLength(3);
    expect(items(view).some((item) => item.hasAttribute("disabled"))).toBe(false);

    // One invocation at a time.
    await user.click(items(view)[2] as HTMLElement);
    await letRequestsSettle();
    expect(bodies, "a pick while one was out sent another").toHaveLength(1);

    out.resolve();
    await waitFor(() => expect(menuOf(view)).toBeNull());
    expect(control.getAttribute("aria-expanded")).toBe("false");
    expect(refusal(view)).toBeNull();
    expect(document.activeElement, "the focus went with the menu").toBe(rowOf(view));
    expect(bodies).toHaveLength(1);
  } finally {
    out.resolve();
    view.unmount();
  }
});

// A refusal adds no row and says one sentence at the row, with the role `status`: the 404's
// own, and the page's for any other. A 500 is no verdict: the write is tried again under its key
// and the menu says it is out, until the reader stops it (the shell's Stop trying), which is the
// refusal the menu then says. The sentence stands until the menu opens again. The rows are the
// issue's tree read's, which answers one lens more at each read after the first: a refusal sends
// no tree read and draws no row, where the same pick answered 201 reads the tree again and draws
// the lens it made.
test("a refusal adds no row", async () => {
  const user = userEvent.setup();
  const [read] = buildIssueTree({ issue: ISSUE, tree: treeAnswer(0) }).children;
  if (read?.reference === undefined) throw new Error("the tree read draws no statement");
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const tree = answerTreeReads();
  let refusing = true;
  const bodies = answerInvocations(() =>
    refusing
      ? Response.json({ code: "NOT_FOUND", error: "no node at that address" }, { status: 404 })
      : Response.json(invoked("trade-off"), { status: 201 })
  );
  const view = renderAt(<TreeReadRows />);
  try {
    const control = await focusedAct(view, read.id);
    const rows = view.container.querySelectorAll("[data-node-row]").length;
    expect(tree.reads).toBe(1);
    await user.click(control);
    await user.click((await drawnItems(view))[0] as HTMLElement);
    await waitFor(() => expect(refusal(view)).toBe(LENS_ANSWERS.gone));
    expect(bodies).toEqual([{ kind: "trade-off", on: read.reference }]);
    const status = view.container.querySelector(REFUSAL);
    expect(status?.hasAttribute("data-page-chrome")).toBe(true);
    expect(control.nextElementSibling, "the sentence is not the act's next").toBe(status);
    expect(menuOf(view)).toBeNull();
    expect(document.activeElement, "the focus did not go back to the row").toBe(
      rowOf(view, read.id)
    );
    await letRequestsSettle();
    expect(tree.reads, "the refusal read the tree again").toBe(1);
    expect(view.container.querySelectorAll("[data-node-row]")).toHaveLength(rows);

    await user.click(control);
    expect(refusal(view), "the sentence outlived the next opening").toBeNull();

    // The same pick, answered 201, reads the tree again, and the row of the lens it made is drawn.
    refusing = false;
    await user.click((await drawnItems(view))[0] as HTMLElement);
    await waitFor(() => expect(menuOf(view)).toBeNull());
    expect(bodies).toEqual([
      { kind: "trade-off", on: read.reference },
      { kind: "trade-off", on: read.reference },
    ]);
    expect(tree.reads, "the 201 did not read the tree again").toBe(2);
    expect(view.container.querySelectorAll("[data-node-row]")).toHaveLength(rows + 1);
    expect(refusal(view)).toBeNull();
  } finally {
    view.unmount();
  }

  jest.useFakeTimers();
  const onClock = userOnFakeClock();
  const again = answerTreeReads();
  let attempts = 0;
  answerRequests(
    http.post(`/api/v1/issues/${KEY}/lenses`, () => {
      attempts += 1;
      return Response.json({ code: "INTERNAL", error: "the store failed" }, { status: 500 });
    })
  );
  const client = signedIn();
  const failing = renderAt(<TreeReadRows />, `/issues/${KEY}`, client);
  try {
    const control = await focusedAct(failing, read.id);
    const rows = failing.container.querySelectorAll("[data-node-row]").length;
    expect(again.reads).toBe(1);
    await onClock.click(control);
    await onClock.click((await drawnItems(failing))[1] as HTMLElement);
    await tick(SEND_DEADLINE_MS + 1_000);
    expect(attempts, "a 500 was not tried again").toBeGreaterThan(1);
    expect(menuLine(failing)).toBe(LENS_ANSWERS.examining);
    expect(refusal(failing)).toBeNull();

    const [late] = writesOf(client).rows();
    expect(late?.status).toBe("late");
    act(() => {
      writesOf(client).stop(late?.id ?? 0);
    });
    await tick(1_000);
    expect(refusal(failing)).toBe(LENS_ANSWERS.failed);
    expect(menuOf(failing)).toBeNull();
    expect(document.activeElement, "the focus did not go back to the row").toBe(
      rowOf(failing, read.id)
    );
    expect(again.reads, "the refusal read the tree again").toBe(1);
    expect(failing.container.querySelectorAll("[data-node-row]")).toHaveLength(rows);
  } finally {
    failing.unmount();
    realClock();
  }
});

// A pick on a node that became gone while its menu was open is refused 409 NODE_GONE, and the
// row says the node is no longer here, as for a 404: gone is final, and no second pick can land.
test("a pick on a gone node says it is no longer here", async () => {
  const user = userEvent.setup();
  const [read] = buildIssueTree({ issue: ISSUE, tree: treeAnswer(0) }).children;
  if (read?.reference === undefined) throw new Error("the tree read draws no statement");
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const tree = answerTreeReads();
  const bodies = answerInvocations(() =>
    Response.json(
      { code: "NODE_GONE", error: "the node is gone, and gone is final: no write writes on it" },
      { status: 409 }
    )
  );
  const view = renderAt(<TreeReadRows />);
  try {
    const control = await focusedAct(view, read.id);
    await user.click(control);
    await user.click((await drawnItems(view))[0] as HTMLElement);
    await waitFor(() => expect(bodies).toHaveLength(1));
    await letRequestsSettle();
    expect(refusal(view), "a gone node's refusal says another sentence").toBe(LENS_ANSWERS.gone);
    expect(tree.reads, "the refusal read the tree again").toBe(1);
  } finally {
    view.unmount();
  }
});

// A 201 that comes after the reader left this issue's outline moves nothing: the reader stays
// where they went, on a page that is no issue's, on another issue's outline, on one of this
// issue's views, whose own query stands, or on its other outline, Graph.
test("a 201 after the reader left the outline leaves them where they went", async () => {
  const user = userEvent.setup();
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  let out = Promise.withResolvers<void>();
  const bodies = answerInvocations(async () => {
    await out.promise;
    return Response.json(invoked("pre-mortem"), { status: 201 });
  });
  for (const away of [
    "/inbox?filter=mine",
    "/issues/CORE-2?q=notices",
    `/issues/${KEY}/spec`,
    `/issues/${KEY}/graph`,
  ]) {
    out = Promise.withResolvers<void>();
    const sent = bodies.length;
    const address: Address = { at: "", go: undefined };
    const client = signedIn();
    const view = renderAt(<Outline address={address} nodes={[statement]} />, undefined, client);
    try {
      await user.click(await focusedAct(view));
      await user.click((await drawnItems(view))[1] as HTMLElement);
      await waitFor(() => expect(bodies).toHaveLength(sent + 1));
      act(() => address.go?.(away));
      expect(address.at).toBe(away);

      out.resolve();
      await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
      await letRequestsSettle();
      expect(address.at, `the 201 moved a reader who went to ${away}`).toBe(away);
    } finally {
      out.resolve();
      view.unmount();
    }
  }
});

// Where the reader still stands on this issue's outline as the 201 comes, the page lands on the
// new lens's row though the row it was picked on lost the focus and its menu closed: at the
// address that stands then, which keeps the words filter the reader typed after the pick.
test("a 201 lands on the outline the reader still stands on", async () => {
  const user = userEvent.setup();
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const out = Promise.withResolvers<void>();
  const bodies = answerInvocations(async () => {
    await out.promise;
    return Response.json(invoked("pre-mortem"), { status: 201 });
  });
  const address: Address = { at: "", go: undefined };
  const view = renderAt(
    <Outline address={address} nodes={[statement, other]} />,
    `/issues/${KEY}?q=notices`
  );
  try {
    await user.click(await focusedAct(view));
    await user.click((await drawnItems(view))[1] as HTMLElement);
    await waitFor(() => expect(bodies).toHaveLength(1));
    await focusedAct(view, OTHER_ID);
    expect(menuOf(view), "the menu outlived its row's focus").toBeNull();
    act(() => address.go?.(`/issues/${KEY}?q=day`));

    out.resolve();
    await waitFor(() =>
      expect(address.at).toBe(`/issues/${KEY}?q=day&node=${invoked("pre-mortem").id}`)
    );
  } finally {
    out.resolve();
    view.unmount();
  }
});

// A 201 reads the open view's tree again and no view of the issue: no view draws a lens, a
// finding or a risk, so a view held before the pick, which an observer reads, keeps its answer
// and is neither reset nor read again (R6). The page lands on the new lens's row only once the
// tree read the pick made has answered: until then the menu says the pick is out and the address
// stays where it was. The client records when each read begins, as the page's event feed has it
// do, so the pick's read keeps to the scheduler's interval.
test("a 201 reads the open view's tree again and no view", async () => {
  const user = userEvent.setup();
  const [read] = buildIssueTree({ issue: ISSUE, tree: treeAnswer(0) }).children;
  if (read?.reference === undefined) throw new Error("the tree read draws no statement");
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const reread = Promise.withResolvers<void>();
  const tree = answerTreeReads(reread.promise);
  const bodies = answerInvocations(() => Response.json(invoked("trade-off"), { status: 201 }));
  const client = signedIn();
  trackQueryFetches(client);
  const held = treeAnswer(0);
  client.setQueryData(viewKey(KEY, "spec"), held);
  const heldAt = client.getQueryState(viewKey(KEY, "spec"))?.dataUpdatedAt;
  const address: Address = { at: "", go: undefined };
  const view = renderAt(
    <>
      <TreeReadRows />
      <SpecViewRead />
      <Where address={address} />
    </>,
    undefined,
    client
  );
  try {
    const control = await focusedAct(view, read.id);
    const rows = view.container.querySelectorAll("[data-node-row]").length;
    expect(tree.reads).toBe(1);
    await user.click(control);
    await user.click((await drawnItems(view))[0] as HTMLElement);
    await waitFor(() => expect(tree.reads).toBe(2));
    await letRequestsSettle();
    expect(bodies).toEqual([{ kind: "trade-off", on: read.reference }]);
    expect(menuLine(view), "the menu stopped saying the pick is out").toBe(LENS_ANSWERS.examining);
    expect(address.at, "the page landed before the tree was read again").toBe(`/issues/${KEY}`);

    reread.resolve();
    await waitFor(() => expect(address.at).toBe(`/issues/${KEY}?node=${invoked("trade-off").id}`));
    expect(
      client.getQueryData<TreeAnswer>(viewKey(KEY, "spec")),
      "the 201 reset the held view"
    ).toBe(held);
    expect(
      client.getQueryState(viewKey(KEY, "spec"))?.dataUpdatedAt,
      "the 201 read the held view again"
    ).toBe(heldAt);
    await waitFor(() =>
      expect(view.container.querySelectorAll("[data-node-row]")).toHaveLength(rows + 1)
    );
    await letRequestsSettle();
    expect(tree.reads, "the 201 read the tree more than once").toBe(2);
    expect(tree.views, "a read was a view's").toEqual([null, null]);
    expect(menuOf(view)).toBeNull();
  } finally {
    reread.resolve();
    view.unmount();
  }
});

// The same 201 where the tree read is already marked stale as the pick is made, as a write the
// page saw before it can leave it. TanStack marks no query stale twice, and a mark it does not
// make tells the cache's listeners nothing, so the pick's read of the tree must not wait on that
// notice: the tree is read again, the held view stays as it was and the page lands on the new row.
test("a 201 on a tree read already marked stale reads it again and lands", async () => {
  const user = userEvent.setup();
  const [read] = buildIssueTree({ issue: ISSUE, tree: treeAnswer(0) }).children;
  if (read?.reference === undefined) throw new Error("the tree read draws no statement");
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const tree = answerTreeReads();
  const bodies = answerInvocations(() => Response.json(invoked("trade-off"), { status: 201 }));
  const client = signedIn();
  trackQueryFetches(client);
  const held = treeAnswer(0);
  client.setQueryData(viewKey(KEY, "spec"), held);
  const heldAt = client.getQueryState(viewKey(KEY, "spec"))?.dataUpdatedAt;
  const address: Address = { at: "", go: undefined };
  const view = renderAt(
    <>
      <TreeReadRows />
      <SpecViewRead />
      <Where address={address} />
    </>,
    undefined,
    client
  );
  try {
    const control = await focusedAct(view, read.id);
    const rows = view.container.querySelectorAll("[data-node-row]").length;
    expect(tree.reads).toBe(1);
    // Conversation's read, which the page holds in Conversation (`outlineQuery`).
    await act(() =>
      client.invalidateQueries({
        exact: true,
        queryKey: conversationTreeKey(KEY),
        refetchType: "none",
      })
    );
    expect(client.getQueryState(conversationTreeKey(KEY))?.isInvalidated).toBe(true);
    expect(tree.reads, "marking the tree stale read it again").toBe(1);

    await user.click(control);
    await user.click((await drawnItems(view))[0] as HTMLElement);
    await waitFor(() => expect(address.at).toBe(`/issues/${KEY}?node=${invoked("trade-off").id}`));
    expect(bodies).toEqual([{ kind: "trade-off", on: read.reference }]);
    expect(
      client.getQueryData<TreeAnswer>(viewKey(KEY, "spec")),
      "the 201 reset the held view"
    ).toBe(held);
    expect(
      client.getQueryState(viewKey(KEY, "spec"))?.dataUpdatedAt,
      "the 201 read the held view again"
    ).toBe(heldAt);
    expect(tree.reads, "the 201 did not read the tree again").toBe(2);
    expect(tree.views, "a read was a view's").toEqual([null, null]);
    await waitFor(() =>
      expect(view.container.querySelectorAll("[data-node-row]")).toHaveLength(rows + 1)
    );
    expect(menuOf(view)).toBeNull();
  } finally {
    view.unmount();
  }
});

// In Graph the act is offered too, and a 201 reads Graph's tree once - the tree read with no
// view, which names no kind - and never Conversation's read, which Graph does not draw (R6). The
// page lands on Graph's own address of the new lens's row, keeping the words filter, so the lens
// is drawn in the view it was invoked in (R4, R7).
test("an invocation made in Graph reads Graph's tree once and lands on Graph's ?node=", async () => {
  const user = userEvent.setup();
  const [read] = buildIssueTree({ issue: ISSUE, tree: treeAnswer(0) }).children;
  if (read?.reference === undefined) throw new Error("the tree read draws no statement");
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const tree = answerTreeReads();
  const bodies = answerInvocations(() => Response.json(invoked("trade-off"), { status: 201 }));
  const client = signedIn();
  trackQueryFetches(client);
  const address: Address = { at: "", go: undefined };
  const view = renderAt(
    <>
      <TreeReadRows />
      <Where address={address} />
    </>,
    `/issues/${KEY}/graph?q=notices`,
    client
  );
  try {
    const control = await focusedAct(view, read.id);
    const rows = view.container.querySelectorAll("[data-node-row]").length;
    expect(tree.reads).toBe(1);
    await user.click(control);
    await user.click((await drawnItems(view))[0] as HTMLElement);
    await waitFor(() =>
      expect(address.at).toBe(`/issues/${KEY}/graph?q=notices&node=${invoked("trade-off").id}`)
    );
    expect(bodies).toEqual([{ kind: "trade-off", on: read.reference }]);
    await letRequestsSettle();
    expect(tree.reads, "the 201 did not read the tree once").toBe(2);
    expect(tree.kinds, "a read was Conversation's").toEqual([[], []]);
    await waitFor(() =>
      expect(view.container.querySelectorAll("[data-node-row]")).toHaveLength(rows + 1)
    );
    expect(menuOf(view)).toBeNull();
  } finally {
    view.unmount();
  }
});

// A refusal that comes after the row lost the focus, and its menu closed, is said on that row
// alone (and on no other) without stealing focus, stays there once focused again, and the next
// opening of the menu lets it go.
test("a refusal after the row lost the focus is said on that row at once and stays", async () => {
  const user = userEvent.setup();
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const out = Promise.withResolvers<void>();
  const bodies = answerInvocations(async () => {
    await out.promise;
    return Response.json({ code: "NOT_FOUND", error: "no node at that address" }, { status: 404 });
  });
  const client = signedIn();
  const view = renderAt(<Rows nodes={[statement, other]} />, undefined, client);
  try {
    await user.click(await focusedAct(view));
    await user.click((await drawnItems(view))[0] as HTMLElement);
    await waitFor(() => expect(bodies).toHaveLength(1));
    await focusedAct(view, OTHER_ID);
    expect(menuOf(view), "the menu outlived its row's focus").toBeNull();

    out.resolve();
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    await letRequestsSettle();
    expect(refusal(view, OTHER_ID), "another row said the refusal").toBeNull();
    expect(refusal(view, STATEMENT_ID), "the refused row did not say its refusal").toBe(
      LENS_ANSWERS.gone
    );
    expect(document.activeElement, "the refusal stole the focus from the other row").toBe(
      rowOf(view, OTHER_ID)
    );

    const control = await focusedAct(view);
    expect(refusal(view, STATEMENT_ID)).toBe(LENS_ANSWERS.gone);
    const status = rowOf(view, STATEMENT_ID).querySelector(REFUSAL);
    expect(status?.getAttribute("role")).toBe("status");
    expect(status?.hasAttribute("data-page-chrome")).toBe(true);
    expect(control.nextElementSibling, "the sentence is not the act's next").toBe(status);
    expect(rowOf(view).contains(status), "the sentence is not at the row").toBe(true);

    await user.click(control);
    expect(refusal(view, STATEMENT_ID), "the sentence outlived the next opening").toBeNull();
  } finally {
    out.resolve();
    view.unmount();
  }
});

// A row focused again while its pick is out says so in its menu, and a pick there sends
// nothing: whether a pick is out is kept with the node's invocations, not only while the menu
// stays open. The 201 still lands.
test("a row focused again while its pick is out takes no second pick", async () => {
  const user = userEvent.setup();
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const out = Promise.withResolvers<void>();
  const bodies = answerInvocations(async () => {
    await out.promise;
    return Response.json(invoked("pre-mortem"), { status: 201 });
  });
  const address: Address = { at: "", go: undefined };
  const client = signedIn();
  const view = renderAt(
    <Outline address={address} nodes={[statement, other]} />,
    undefined,
    client
  );
  try {
    await user.click(await focusedAct(view));
    await user.click((await drawnItems(view))[1] as HTMLElement);
    await waitFor(() => expect(bodies).toHaveLength(1));
    const otherControl = await focusedAct(view, OTHER_ID);
    expect(otherControl, "another row did not draw its own Lens act").not.toBe(
      lensAct(view, STATEMENT_ID)
    );
    expect(menuOf(view), "the menu outlived its row's focus").toBeNull();

    await user.click(await focusedAct(view));
    await drawnItems(view);
    expect(menuLine(view), "the menu drawn again says no pick is out").toBe(LENS_ANSWERS.examining);
    await user.click(items(view)[0] as HTMLElement);
    await letRequestsSettle();
    expect(bodies, "a pick while one was out sent another").toEqual([
      { kind: "pre-mortem", on: STATEMENT },
    ]);
    expect(writesOf(client).rows(), "a pick while one was out made another write").toHaveLength(1);

    out.resolve();
    await waitFor(() => expect(address.at).toBe(`/issues/${KEY}?node=${invoked("pre-mortem").id}`));
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies).toHaveLength(1);
  } finally {
    out.resolve();
    view.unmount();
  }
});

// Escape closes the menu and gives the focus back to the row, from a kind and from the menu
// while the catalogue is read; nothing is sent.
test("Escape gives the focus back", async () => {
  const user = userEvent.setup();
  const out = Promise.withResolvers<void>();
  answerCatalogue(async () => {
    await out.promise;
    return Response.json(schema(CATALOGUE));
  });
  const view = renderAt(<Rows nodes={[statement]} />);
  try {
    const control = await focusedAct(view);
    await user.click(control);
    expect(document.activeElement).toBe(menuOf(view));
    await user.keyboard("{Escape}");
    expect(menuOf(view)).toBeNull();
    expect(document.activeElement, "Escape in the reading menu kept the focus").toBe(rowOf(view));

    out.resolve();
    await user.click(control);
    await drawnItems(view);
    await user.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(items(view)[1]);
    await user.keyboard("{Escape}");
    expect(menuOf(view)).toBeNull();
    expect(control.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement, "Escape on a kind kept the focus").toBe(rowOf(view));
    expect(lensAct(view), "the act went with the menu").toBe(control);
  } finally {
    out.resolve();
    view.unmount();
  }
});

/** Opens the dialog `DialogOverTheRows` draws, once it is drawn. */
interface DialogOpener {
  open: (() => void) | undefined;
}

/** A dialog over the rows that `useDialog` holds open once `opener` opens it, with words a
 *  press can land on and a control it puts the focus on. It names itself no `aria-modal`, so
 *  only `useDialog`'s registry says that it is open. */
function DialogOverTheRows({ opener }: { opener: DialogOpener }): ReactNode {
  const [open, setOpen] = useState(false);
  opener.open = () => setOpen(true);
  const dialog = useDialog<HTMLDivElement>({ onClose: () => setOpen(false), open });
  return open ? (
    <div aria-label="Over the rows" ref={dialog.containerRef} role="dialog">
      <p>The dialog's words</p>
      <button type="button">in the dialog</button>
    </div>
  ) : null;
}

// A dialog open over the menu takes its own Escape wherever the focus is, whether or not the
// dialog names itself modal: whether one is open is `useDialog`'s registry's answer
// (`dialogOpen()`), as `IssuePresets.tsx`'s menu asks it. The focus is on the dialog itself in
// the frame after the dialog opens, which `useDialog` gives it as the dialog opens, before its
// control takes it, and on the page after a press on the dialog's words. The menu under the
// dialog stays open, and leaves the key's default alone.
test("Escape with a dialog open over the menu closes the dialog alone", async () => {
  const user = userEvent.setup();
  answerCatalogue(() => Response.json(schema(CATALOGUE)));
  const opener: DialogOpener = { open: undefined };
  const prevented: boolean[] = [];
  const record = (event: KeyboardEvent) => {
    if (event.key === "Escape") prevented.push(event.defaultPrevented);
  };
  window.addEventListener("keydown", record);
  const view = renderAt(
    <KeymapProvider>
      <Rows nodes={[statement]} />
      <DialogOverTheRows opener={opener} />
    </KeymapProvider>
  );
  const dialog = () => view.queryByRole("dialog", { name: "Over the rows" });
  const escapeClosesTheDialog = async (where: string) => {
    await user.keyboard("{Escape}");
    expect(dialog(), `Escape ${where} left the dialog open`).toBeNull();
    expect(menuOf(view), `Escape ${where} closed the menu under the dialog`).not.toBeNull();
  };
  try {
    await user.click(await focusedAct(view));
    const [first] = await drawnItems(view);
    expect(document.activeElement, "the menu's first kind took no focus").toBe(first);

    // The dialog takes the focus itself as it opens, and gives it to its control a frame later;
    // with that frame held, the Escape lands on the dialog itself.
    const frames = jest.spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);
    try {
      act(() => opener.open?.());
      expect(dialog(), "the dialog did not open").not.toBeNull();
      expect(document.activeElement, "the dialog took no focus as it opened").toBe(dialog());
      await escapeClosesTheDialog("on the dialog itself");
      expect(document.activeElement, "the dialog gave the focus back elsewhere").toBe(first);
    } finally {
      frames.mockRestore();
    }

    act(() => opener.open?.());
    await waitFor(() =>
      expect(document.activeElement, "the dialog took no focus").toBe(
        view.getByRole("button", { name: "in the dialog" })
      )
    );
    await user.click(view.getByText("The dialog's words"));
    expect(document.activeElement, "the press on the dialog's words kept the focus").toBe(
      document.body
    );
    await escapeClosesTheDialog("on the page");
    expect(prevented, "the menu prevented the dialog's Escape").toEqual([false, false]);
  } finally {
    window.removeEventListener("keydown", record);
    view.unmount();
  }
});

/** The kinds of node the invoke route takes, written out rather than read from the code:
 *  every block of the issue's documents, a criterion, a reading and a plain paragraph as much as
 *  a statement, and an ask, a comment, a message, a lens, a finding, a risk and a constraint. */
const OFFERED: readonly NodeType[] = [
  "requirement",
  "check",
  "plan_task",
  "decision",
  "callout",
  "image",
  "criterion",
  "reading",
  "prose",
  "ask",
  "comment",
  "message",
  "lens",
  "finding",
  "risk",
  "constraint",
];

const UUID = "0c6f1d2e-1111-4a6b-9c1d-5e2f3a4b5c61";

/** Addresses of nodes of this issue: blocks of its documents, an ask, a comment, a message. */
const OF_THIS_ISSUE = [
  STATEMENT,
  `dispatch://${KEY}/artifact/plan-md#b-t1`,
  buildDispatchReference({ id: "ask-1", key: KEY, kind: "ask" }),
  buildDispatchReference({ id: "comment-1", key: KEY, kind: "comment" }),
  buildDispatchReference({ id: "message-1", key: KEY, kind: "message" }),
];

/** The four uuid addresses of the node store: a lens, a finding, a risk and a constraint. */
const OF_THE_STORE = [
  `dispatch://lens/${UUID}`,
  `dispatch://lens/${UUID}#b-f1`,
  `dispatch://risk/${UUID}`,
  `dispatch://constraint/${UUID}`,
];

/** Addresses the route does not take: another issue's node, one of an issue whose key merely
 *  starts with this one's, an issue itself, and store addresses of no such node. */
const NOT_TAKEN = [
  "dispatch://CORE-2/spec#b-r1",
  "dispatch://CORE-12/spec#b-r1",
  "dispatch://CORE-12",
  `dispatch://${KEY}`,
  `dispatch://interaction/${UUID}`,
  "dispatch://lens_kind/pre-mortem",
  `dispatch://risk/${UUID}#b-r1`,
  "dispatch://lens/not-a-uuid",
];

/** Whether every row of `[statement, other]` at `path` draws the Lens act, both unfocused and
 *  with the first row focused, once the rows draw their acts: the rows read the act from the
 *  address. */
async function drawsLensAct(path: string, isClosed = false): Promise<boolean> {
  const drawn = renderAt(<Rows isClosed={isClosed} nodes={[statement, other]} />, path);
  try {
    await waitFor(() =>
      expect(rowOf(drawn, STATEMENT_ID).querySelector("[data-row-copy]")).not.toBeNull()
    );
    const beforeStatement = lensAct(drawn, STATEMENT_ID) !== null;
    const beforeOther = lensAct(drawn, OTHER_ID) !== null;
    expect(beforeOther, `rows at ${path} disagree on the Lens act while unfocused`).toBe(
      beforeStatement
    );
    act(() => rowOf(drawn, STATEMENT_ID).focus());
    const afterStatement = lensAct(drawn, STATEMENT_ID) !== null;
    const afterOther = lensAct(drawn, OTHER_ID) !== null;
    expect(afterStatement, `focusing a row at ${path} changed its Lens act`).toBe(beforeStatement);
    expect(afterOther, `focusing another row at ${path} changed this row's Lens act`).toBe(
      beforeOther
    );
    return afterStatement;
  } finally {
    drawn.unmount();
  }
}

// The act is drawn in Conversation and in Graph alone, on a node of this issue the invoke route
// takes; a route that names no issue draws none. A closed issue keeps it.
test("the Lens act is offered in Conversation and in Graph alone, on the kinds the route takes", async () => {
  const conversation = { issueKey: KEY, outline: "conversation" } as const;
  const graph = { issueKey: KEY, outline: "graph" } as const;
  const elsewhere = { issueKey: "CORE-2", outline: "conversation" } as const;
  for (const type of NODE_TYPES) {
    const takes = OFFERED.includes(type);
    expect(lensActFor(node({ id: "n", type }), conversation), `${type} with no address`).toBe(
      false
    );
    for (const reference of OF_THIS_ISSUE) {
      const drawn = node({ id: "n", reference, type });
      expect(lensActFor(drawn, conversation), `${type} at ${reference}`).toBe(takes);
      expect(lensActFor(drawn, graph), `${type} at ${reference} in Graph`).toBe(takes);
      expect(lensActFor(drawn, elsewhere), `${type} at ${reference} of another issue`).toBe(false);
    }
    for (const reference of OF_THE_STORE) {
      const drawn = node({ id: "n", reference, type });
      expect(lensActFor(drawn, conversation), `${type} at ${reference}`).toBe(takes);
      expect(lensActFor(drawn, graph), `${type} at ${reference} in Graph`).toBe(takes);
    }
    for (const reference of NOT_TAKEN) {
      const drawn = node({ id: "n", reference, type });
      expect(lensActFor(drawn, conversation), `${type} at ${reference}`).toBe(false);
    }
  }

  expect(await drawsLensAct(`/issues/${KEY}`), "Conversation").toBe(true);
  expect(await drawsLensAct(`/issues/${KEY}?q=notices`), "Conversation under a words filter").toBe(
    true
  );
  expect(await drawsLensAct(`/issues/${KEY}`, true), "a closed issue's Conversation").toBe(true);
  expect(await drawsLensAct(`/issues/${KEY}/graph`), "Graph").toBe(true);
  expect(await drawsLensAct(`/issues/${KEY}/graph?node=${STATEMENT_ID}`), "Graph at a node").toBe(
    true
  );
  expect(await drawsLensAct("/"), "a route that names no issue").toBe(false);
});

// No filter's address offers the act, whatever it holds beside its view: a words filter, or a
// `?node=`, which a filter's address reads no node from. The row still draws its other acts.
test("a filter's address offers no Lens act", async () => {
  for (const preset of ["spec", "verification", "plan", "decisions"]) {
    for (const search of ["", "?q=notices", `?node=${encodeURIComponent(STATEMENT_ID)}`]) {
      const path = `/issues/${KEY}/${preset}${search}`;
      expect(await drawsLensAct(path), `the ${preset} view at ${path}`).toBe(false);
    }
  }
});

// While the menu is open every key but Tab is its own and has its default prevented, so
// the page's keymap, which passes such a key by, moves nothing of the tree; Tab closes the menu
// and goes on.
test("no key pressed in the menu reaches the tree", async () => {
  const user = userEvent.setup();
  const read = Promise.withResolvers<void>();
  answerCatalogue(async () => {
    await read.promise;
    return Response.json(schema(CATALOGUE));
  });
  const out = Promise.withResolvers<void>();
  const bodies = answerInvocations(async () => {
    await out.promise;
    return Response.json(invoked("inversion"), { status: 201 });
  });
  const pressed: { readonly key: string; readonly prevented: boolean }[] = [];
  const record = (event: KeyboardEvent) => {
    pressed.push({ key: event.key, prevented: event.defaultPrevented });
  };
  window.addEventListener("keydown", record);
  const view = renderAt(<Rows nodes={[statement]} />);
  try {
    const control = await focusedAct(view);
    await user.click(control);
    // While the catalogue is read, the menu itself holds the focus.
    await user.keyboard("{ArrowDown}j");
    read.resolve();
    await drawnItems(view);
    await user.keyboard("{ArrowDown}{ArrowUp}{Home}{End}jkf");
    expect(menuOf(view), "a key closed the menu").not.toBeNull();
    expect(document.activeElement, "a key moved the focus off the last kind").toBe(items(view)[2]);
    await user.keyboard("{Enter}");
    await waitFor(() => expect(bodies).toEqual([{ kind: "inversion", on: STATEMENT }]));
    await user.keyboard("{Escape}");
    expect(menuOf(view)).toBeNull();
    out.resolve();
    await letRequestsSettle();

    await user.click(control);
    await user.keyboard("{Tab}");
    expect(menuOf(view), "Tab left the menu open").toBeNull();
    expect(pressed).toEqual([
      { key: "ArrowDown", prevented: true },
      { key: "j", prevented: true },
      ...["ArrowDown", "ArrowUp", "Home", "End", "j", "k", "f", "Enter", "Escape"].map((key) => ({
        key,
        prevented: true,
      })),
      { key: "Tab", prevented: false },
    ]);
  } finally {
    window.removeEventListener("keydown", record);
    read.resolve();
    out.resolve();
    view.unmount();
  }
});

/** `count` statements of the spec, `spec/r1` on, each a row the Lens act stands on. */
function statements(count: number): TreeNode[] {
  return Array.from({ length: count }, (_unused, at) =>
    node({
      id: `spec/r${at + 1}`,
      reference: statementRef(`r${at + 1}`),
      text: `Statement ${at + 1}`,
      type: "requirement",
    })
  );
}

/** What the page asks of a client's mutation cache since `watchMutations` began: how many
 *  listeners it holds now, and how many notices it has sent them. */
interface MutationWatch {
  listeners: number;
  notices: number;
}

/** Watches `client`'s mutation cache, from before anything is drawn with it. */
function watchMutations(client: QueryClient): MutationWatch {
  const cache = client.getMutationCache();
  const watch: MutationWatch = { listeners: 0, notices: 0 };
  const subscribe = cache.subscribe;
  jest.spyOn(cache, "subscribe").mockImplementation((listener) => {
    const unsubscribe = subscribe(listener);
    watch.listeners += 1;
    return () => {
      watch.listeners -= 1;
      unsubscribe();
    };
  });
  const notify = cache.notify.bind(cache);
  jest.spyOn(cache, "notify").mockImplementation((event) => {
    watch.notices += 1;
    notify(event);
  });
  return watch;
}

// The act stands on every row that takes it, and a focus move draws every row again, so what an
// act reads of the mutation cache itself would make each move cost more with every row: the
// outline reads every node's invocations once, the same for 3 rows as for 30, and a move tells
// the cache's listeners nothing.
test("the rows' Lens acts listen to the mutation cache once, however many rows", async () => {
  // For each count of rows: the cache's listeners once the rows are drawn, and after three focus
  // moves; and the notices those moves sent.
  const listeners: number[][] = [];
  const notices: number[] = [];
  for (const count of [3, 30]) {
    const client = signedIn();
    const watch = watchMutations(client);
    const view = renderAt(<Rows nodes={statements(count)} />, undefined, client);
    try {
      await waitFor(() =>
        expect(view.container.querySelectorAll("[data-row-lens]")).toHaveLength(count)
      );
      const drawn = watch.listeners;
      const before = watch.notices;
      for (const at of [1, 2, count]) {
        const id = `spec/r${at}`;
        await focusedAct(view, id);
        expect(
          rowOf(view, id).querySelector("[data-node-card]")?.className,
          "the focus move did not draw the rows again"
        ).toContain(pageFocusedRow);
      }
      listeners.push([drawn, watch.listeners]);
      notices.push(watch.notices - before);
    } finally {
      view.unmount();
    }
  }
  const [few] = listeners;
  expect(listeners, "the mutation cache's listeners grew with the rows or a focus move").toEqual([
    [few?.[0], few?.[0]],
    [few?.[0], few?.[0]],
  ]);
  expect(notices, "a focus move told the mutation cache's listeners").toEqual([0, 0]);
});
