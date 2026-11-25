import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import type { BlockSchemaRead, LensKindRead } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { installFakeLiveSockets } from "../../../__tests__/live-socket-harness";
import { answerRequests } from "../../../__tests__/network";
import { api } from "../../../api/client";
import { liveFeedUp, liveSocketFor } from "../../../api/live-socket";
import type { IssueDetails } from "../../../api/types";
import { MarginProvider } from "../../margin/margin-context";
import { parseIssuePath } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import { issueDetails, KEY, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { DocumentAdmissionProvider } from "../document-admission";
import { DocumentStages } from "../document-stage";
import { FocusedNodeProvider } from "../focused-node";
import { IssueTree } from "../IssueTree";
import { presetPath } from "../issue-address";
import { buildIssueTree } from "../issue-tree";
import { ANSWER_LINES, CONTROL_LABELS, LENS_WORDS, type Preset } from "../page-words";
import { ProvidersSlot } from "../slots/providers";
import { StageSlot } from "../slots/stage";
import { AppProviders, stubReads } from "../slots/stage-fixtures";
import { outlineQuery, type TreeAnswer, type TreeAnswerNode } from "../tree-read/tree-read";
import fixture from "./__fixtures__/graph-edit.json";
import { ROW_MENU_ATTRIBUTE } from "./RowMenu";

/**
 * The node menu of Graph's focused row, drawn in the outline as Graph draws it (`RowMenu.tsx`;
 * R1, R2, R6, R8 and R12 of INFRA-5705's spec): the page's own stage at `/issues/CORE-1/graph`
 * under the page's providers (`slots/stage-fixtures.tsx`), its tree read answered by the server's
 * own answer of `__fixtures__/graph-edit.json`, and the two node routes and the lens catalogue
 * answered at `fetch` (`answerRequests`), so each request the menu sends is one the case answers
 * and counts. A request no case answered fails its test.
 *
 * The interface this file expects of `graph-edit/RowMenu.tsx` and its drawing (the task that writes
 * it reads it here; no word is written here but through `page-words-r.ts`):
 *
 * - `RowMenu.tsx` exports `ROW_MENU_ATTRIBUTE`, the name of the data attribute (`"data-row-menu"`)
 *   that marks the menu's control, as `data-row-lens` marks Lens's.
 * - On the focused row of Graph, among the row's acts and after Copy reference, a button marked
 *   by it, named `CONTROL_LABELS.edit_menu`, `aria-haspopup="menu"`, `aria-expanded` as the menu stands. A
 *   press on it, or `m` while the row holds the focus, opens the menu, which draws as an element
 *   `role="menu"` of entries `role="menuitem"` in R1's order, and the focus goes to the first.
 * - An entry reads its word first (`CONTROL_LABELS.edit_*`; Lens's is `LENS_WORDS.act`). A grey
 *   entry is `aria-disabled="true"` and draws its reason (`ANSWER_LINES.grey_*`) inside the menu;
 *   it is reached by the arrows and picks nothing. Retype and Lens are submenus: Right opens one
 *   (a `role="menu"` of its own, its entries `role="menuitem"`), Left closes it and gives the
 *   focus back to its entry.
 * - Delete reads `CONTROL_LABELS.edit_delete` and `CONTROL_LABELS.delete_counting` while the dry
 *   run is out, then `CONTROL_LABELS.edit_delete`, the count and `CONTROL_LABELS.delete_rows` (or
 *   `delete_row`): "Delete 4 rows". A refused dry run greys it with the refusal's words; a failed
 *   one greys it and draws `ANSWER_LINES.delete_count_failed` as a button whose press asks again.
 * - A pick of Delete opens a dialog (`role="dialog"`, `features/project/ConfirmDialog.tsx`) titled
 *   `ANSWER_LINES.delete_confirm_title`, whose body reads `ANSWER_LINES.confirm_takes`, the count
 *   and the unit; its buttons are `CONTROL_LABELS.confirm_delete` and Cancel.
 * - A refusal of a move or a delete draws `refusalLine`'s text in a `role="status"` element of the
 *   row; the fallback line is a button.
 */

const NAMED = "<issue>";

/** `value` with the fixture's name of the issue written as the key the routes take: a name for a
 *  name, no other change to what the server answered. */
function renamed<T>(value: T, from = NAMED): T {
  return JSON.parse(JSON.stringify(value).replaceAll(from, KEY)) as T;
}

const converted = renamed(fixture.tree.converted.person.body) as unknown as TreeAnswer;
const unconverted = renamed(
  fixture.tree.unconverted.person.body,
  "<unconverted>"
) as unknown as TreeAnswer;
const moved = fixture.moved;
const deleted = fixture.deleted;
const members: Readonly<Record<string, unknown>> = fixture.deleted;
// A read of the tree right after the taken delete, which a later regeneration of the fixture gives.
const treeAfterDelete = members.tree_after as { body: unknown } | undefined;

function nodeOf(held: TreeAnswer, id: string): TreeAnswerNode {
  const found = held.nodes?.find((each) => each.id === id);
  if (found === undefined) throw new Error(`the fixture's read holds no node ${id}`);
  return found;
}

const STATEMENT_1 = nodeOf(converted, "<statement-1>");
const STATEMENT_2 = nodeOf(converted, "<statement-2>");
const STATEMENT_3 = nodeOf(converted, "<statement-3>");
const DECISION = nodeOf(converted, "<decision>");

/** The id of the row a node draws, which is the block's address inside the issue
 *  (`blockNodeId`): its document's slug, or the issue's own fragment's, and its block id. */
function rowId(node: TreeAnswerNode): string {
  return `${node.document?.slug ?? "tree"}#b-${node.block_id}`;
}

// What stands in for the page's other reads, which the outline's draw needs and no case is about.
let stubs: { mockRestore(): void }[] = [];
// The live feeds a case put up (`liveFeedOn`), each ended once its page is gone.
const feeds: (() => void)[] = [];
beforeEach(() => {
  stubs = stubReads();
});
afterEach(() => {
  cleanup();
  for (const stub of stubs) stub.mockRestore();
  for (const end of feeds.splice(0)) end();
});

/** One answer of a route: its status and body as the fixture holds them, held until `gate`
 *  settles where it has one, and told to `returned` as it goes back to the page. */
interface Reply {
  readonly status: number;
  readonly body: unknown;
  readonly gate?: Promise<void>;
  readonly returned?: () => void;
}

/** What one request to a node route carried. */
interface Sent {
  readonly id: string;
  readonly ifMatch: string | null;
  readonly dryRun: boolean;
  readonly expected: string | null;
  readonly body: unknown;
}

/** Answers the two node routes of the issue with the next of each list, keeping each request as it
 *  came. A request with no answer left is an error of the case. */
function answerNodeRoutes(
  replies: { readonly patches?: readonly Reply[]; readonly deletes?: readonly Reply[] } = {}
): { readonly patches: Sent[]; readonly deletes: Sent[] } {
  const sent = { deletes: [] as Sent[], patches: [] as Sent[] };
  const waiting = { deletes: [...(replies.deletes ?? [])], patches: [...(replies.patches ?? [])] };
  async function take(
    kind: "patches" | "deletes",
    request: Request,
    id: unknown
  ): Promise<Response> {
    const url = new URL(request.url);
    const text = await request.text();
    sent[kind].push({
      body: text === "" ? null : JSON.parse(text),
      dryRun: url.searchParams.get("dry_run") === "true",
      expected: url.searchParams.get("expected_count"),
      id: decodeURIComponent(String(id)),
      ifMatch: request.headers.get("If-Match"),
    });
    const reply = waiting[kind].shift();
    if (reply === undefined) throw new Error(`a ${kind} request came that no answer was left for`);
    await reply.gate;
    reply.returned?.();
    return Response.json(renamed(reply.body), { status: reply.status });
  }
  answerRequests(
    http.patch(`/api/v1/issues/${KEY}/nodes/:id`, ({ params, request }) =>
      take("patches", request, params.id)
    ),
    http.delete(`/api/v1/issues/${KEY}/nodes/:id`, ({ params, request }) =>
      take("deletes", request, params.id)
    )
  );
  return sent;
}

/** The reads of the tree the page made, each by the query it named. */
interface TreeReads {
  readonly queries: {
    readonly view?: string;
    readonly cursor?: string;
    readonly kind?: string[];
  }[];
  /** How many of them were Graph's own read: the whole tree, no view, no kind, no cursor. */
  readonly outline: () => number;
}

/** The supertag of the nodes each view of the tree read is the query of: those nodes stand in the
 *  view's answer as the query's own (`role: "query"`), the issue stands beside them, and no other
 *  node does. The fixture holds no read of a view, so a case that draws one is answered from the
 *  whole read in that shape. */
const VIEW_OWN: Readonly<Record<string, string>> = {
  plan: "plan_task",
  spec: "requirement",
  verification: "check",
};

function viewOf(answer: TreeAnswer, view: string): TreeAnswer {
  const own = VIEW_OWN[view];
  if (own === undefined) return answer;
  const nodes = (answer.nodes ?? []).flatMap((node): TreeAnswerNode[] => {
    if (node.supertag === own) return [{ ...node, role: "query" }];
    return node.kind === "issue" && node.id === answer.issue ? [node] : [];
  });
  return { ...answer, nodes, view };
}

/** Answers the page's tree reads with `first`, and each read of the outline after the first with
 *  `later` where a case holds them (a read that never answers leaves the outline as the page drew
 *  it from `first`). */
function answerTreeReads(first: TreeAnswer = converted, later?: Promise<TreeAnswer>): TreeReads {
  const queries: TreeReads["queries"][number][] = [];
  spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) => {
    queries.push(query);
    const outline = queries.filter(isOutline).length;
    if (isOutline(query) && outline > 1 && later !== undefined) return later;
    return query.view === undefined ? first : viewOf(first, query.view);
  });
  return { outline: () => queries.filter(isOutline).length, queries };
}

function isOutline(query: { view?: string; cursor?: string; kind?: string[] }): boolean {
  return query.view === undefined && query.cursor === undefined && query.kind === undefined;
}

/** A read that never answers: what a case leaves the page's re-reads at to read the outline as the
 *  page drew it from an answer. */
const NEVER = Promise.withResolvers<TreeAnswer>().promise;

/** The lens catalogue (`GET /api/v1/schema/blocks`), which Lens's submenu reads when it opens. */
function kind(id: string, words: string): LensKindRead {
  return { id, ref: `dispatch://lens_kind/${id}`, sources: null, words };
}

const CATALOGUE: readonly LensKindRead[] = [
  kind("trade-off", "trade-off analysis"),
  kind("pre-mortem", "pre-mortem"),
  kind("inversion", "inversion"),
];

function answerCatalogue(): void {
  const schema: BlockSchemaRead = { lens_kinds: [...CATALOGUE], types: [], version: 1 };
  answerRequests(http.get("/api/v1/schema/blocks", () => Response.json(schema)));
}

/** The page's stage at the address the router holds, under the page's own providers and its
 *  issue scope, as the page's root draws it. */
function Stage({ issue }: { issue: IssueDetails }): ReactNode {
  useKeymapScope("issue");
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <StageSlot issue={issue} route={route} />
    </ProvidersSlot>
  );
}

/** A client signed in as alice, whose failed read is not retried. */
function signedIn(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return client;
}

/** The live feed of `client` up, as the shell's event feed holds it on the page: its socket open,
 *  and the feed's subscription on it started by Dispatch (`events_start`), as
 *  `useIssueTree.test.tsx`'s `liveFeedOn` puts it up. With it up, the end of a write of the issue
 *  reads again only what the write left owed (`tree-read.ts`' `subscribeTreeFreshness`), as on
 *  the page; with no feed, the harness's default, it reads every outline again. The feed ends,
 *  and the page's WebSocket is put back, after the case. */
function liveFeedOn(client: QueryClient): void {
  const sockets = installFakeLiveSockets();
  const live = liveSocketFor(client);
  feeds.push(() => {
    live.close();
    sockets.restore();
  });
  live.subscribeEvents({ onEvent: () => undefined, onStart: () => undefined, since: () => null });
  const socket = sockets.latest();
  act(() => socket.open());
  const { sub } = socket.subscribed("events");
  act(() => socket.receive({ type: "events_start", sub, since: "1", replay_complete: true }));
  expect(liveFeedUp(client), "the live feed is not up").toBe(true);
}

/** The page at `path`, its tree read answered by `reads` (`answerTreeReads`, Graph's answer where
 *  a case names none), under `client`. */
function draw(
  path = `/issues/${KEY}/graph`,
  issue: IssueDetails = issueDetails(),
  client: QueryClient = signedIn()
): RenderResult {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppProviders client={client}>
        <Stage issue={issue} />
      </AppProviders>
    </MemoryRouter>
  );
}

function rowOf(view: RenderResult, node: TreeAnswerNode): HTMLElement | null {
  return view.container.querySelector<HTMLElement>(
    `[data-node-row][data-node-id="${rowId(node)}"]`
  );
}

/** Focuses the row of `node` once it is drawn, as `j` or a press does. */
async function focusRow(view: RenderResult, node: TreeAnswerNode): Promise<HTMLElement> {
  const row = await waitFor(() => {
    const drawn = rowOf(view, node);
    if (drawn === null) throw new Error(`no row is drawn for ${node.id} (${rowId(node)})`);
    return drawn;
  });
  act(() => row.focus());
  return row;
}

/** The menu's control on the focused row. */
async function menuControl(view: RenderResult): Promise<HTMLElement> {
  return waitFor(() => {
    const control = view.container.querySelector<HTMLElement>(`button[${ROW_MENU_ATTRIBUTE}]`);
    if (control === null || control.textContent !== CONTROL_LABELS.edit_menu) {
      throw new Error("the focused row draws no menu control");
    }
    return control;
  });
}

function menuOf(view: RenderResult): HTMLElement | null {
  return view.container.querySelector<HTMLElement>('[role="menu"]');
}

/** The entries of the open menu itself, in the order it draws them: the entries of a submenu
 *  are not among them. */
function entriesOf(view: RenderResult): HTMLElement[] {
  const menu = menuOf(view);
  if (menu === null) return [];
  return [...menu.querySelectorAll<HTMLElement>('[role="menuitem"]')].filter(
    (entry) => entry.closest('[role="menu"]') === menu
  );
}

/** The entry that reads `word` first. */
function entry(view: RenderResult, word: string): HTMLElement {
  const found = entriesOf(view).find((each) => (each.textContent ?? "").startsWith(word));
  if (found === undefined) throw new Error(`the menu holds no entry that reads ${word}`);
  return found;
}

const ENTRY_WORDS = [
  CONTROL_LABELS.edit_new_below,
  CONTROL_LABELS.edit_new_under,
  CONTROL_LABELS.edit_retype,
  CONTROL_LABELS.edit_indent,
  CONTROL_LABELS.edit_outdent,
  CONTROL_LABELS.edit_move_up,
  CONTROL_LABELS.edit_move_down,
  CONTROL_LABELS.edit_delete,
  LENS_WORDS.act,
];

const isGrey = (each: HTMLElement) => each.getAttribute("aria-disabled") === "true";

/** The row of `node` focused, and its menu opened by a press on its control. */
async function openMenu(
  view: RenderResult,
  user: UserEvent,
  node: TreeAnswerNode = STATEMENT_3
): Promise<HTMLElement> {
  const row = await focusRow(view, node);
  await user.click(await menuControl(view));
  await waitFor(() => expect(menuOf(view)).not.toBeNull());
  return row;
}

/** Lets whatever a press would send go out and be answered. */
async function letRequestsSettle(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setTimeout(resolve, 50);
  await act(() => promise);
}

/** The row whose focus the page holds: the node id of the row the active element stands in. */
function focusedRowId(): string | undefined {
  return document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
}

/** The node id of each row the page draws, in the order it draws them. */
function rowIds(view: RenderResult): string[] {
  return [...view.container.querySelectorAll<HTMLElement>("[data-node-row]")].map(
    (row) => row.dataset.nodeId ?? ""
  );
}

// R12: no request of this part is made by the first load of Graph, whatever rows it draws, and
// none by a row's taking the focus.
test("Graph's first load sends no node request", async () => {
  const reads = answerTreeReads();
  const sent = answerNodeRoutes();
  const view = draw();
  await focusRow(view, STATEMENT_3);
  await letRequestsSettle();
  expect(reads.outline(), "Graph's read is made once").toBe(1);
  expect(sent.patches).toEqual([]);
  expect(sent.deletes, "a dry run was sent before a menu opened").toEqual([]);
});

test("the menu holds nine entries in R1's order on a converted issue and Lens alone on another", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  await openMenu(view, user);
  const read = entriesOf(view).map((each) => each.textContent ?? "");
  expect(read).toHaveLength(ENTRY_WORDS.length);
  for (const [at, word] of ENTRY_WORDS.entries()) {
    expect(read[at]?.startsWith(word), `entry ${at} reads ${read[at]}, not ${word}`).toBe(true);
  }
  view.unmount();

  // An issue the conversion has not reached: the menu holds Lens alone.
  answerTreeReads(unconverted);
  const other = draw();
  await openMenu(other, user, nodeOf(unconverted, "<unconverted-statement>"));
  expect(entriesOf(other).map((each) => each.textContent)).toEqual([LENS_WORDS.act]);
});

/** The reads of the spec's document that a view's key may start (Tab on a row opens the document's
 *  editor, as it does today): the document, its blocks and the reader's place in it, answered
 *  empty so a key's effect on the document is none of this case's. */
function answerDocuments(): void {
  answerRequests(
    http.get("/api/v1/artifacts/:id", () => Response.json(issueDetails().artifacts?.[0])),
    http.get("/api/v1/artifacts/:id/blocks", () => Response.json([])),
    http.get("/api/v1/me/documents/:id/read", () => Response.json({}))
  );
}

// R10 and R4: `m` is taken while the outline has the focus, and the keys of the moves in Graph
// with the caret in a block (stage B's cases). Conversation and each filter keep today's: no
// node menu, and nothing written.
test("each key of this part acts only where R4 and R10 say", async () => {
  const user = userEvent.setup();
  const keys = [
    "m",
    "{Tab}",
    "{Shift>}{Tab}{/Shift}",
    "{Alt>}{Shift>}{ArrowUp}{/Shift}{/Alt}",
    "{Alt>}{Shift>}{ArrowDown}{/Shift}{/Alt}",
  ];
  for (const preset of ["conversation", "spec", "verification", "plan", "decisions"] as Preset[]) {
    answerTreeReads();
    answerDocuments();
    const sent = answerNodeRoutes();
    const view = draw(presetPath(KEY, preset));
    const row = await waitFor(
      () => {
        const first = view.container.querySelector<HTMLElement>("[data-node-row]");
        if (first === null) throw new Error(`${preset} draws no row`);
        return first;
      },
      { timeout: 10_000 }
    );
    for (const key of keys) {
      act(() => row.focus());
      await user.keyboard(key);
      expect(menuOf(view), `${key} opened a menu in ${preset}`).toBeNull();
    }
    await letRequestsSettle();
    expect(sent.patches, `a key wrote in ${preset}`).toEqual([]);
    expect(sent.deletes, `a key asked the server in ${preset}`).toEqual([]);
    view.unmount();
  }
  // In Graph `m` opens it from a row, and from nowhere else: with the focus on a control outside
  // the outline it opens none and sends nothing. The page gives its first row the focus once its
  // reads settle, after its first draw, so the case waits for that focus before it moves the focus
  // out.
  answerTreeReads();
  const sent = answerNodeRoutes();
  const graph = draw();
  await waitFor(() => expect(focusedRowId(), "the page gave no row the focus").toBeDefined());
  const outside = document.createElement("button");
  outside.type = "button";
  document.body.append(outside);
  try {
    act(() => outside.focus());
    expect(document.activeElement, "the focus is not on the control outside Graph").toBe(outside);
    await user.keyboard("m");
    expect(menuOf(graph), "m opened a menu from a control outside Graph").toBeNull();
    await letRequestsSettle();
    expect(sent.patches, "m outside Graph wrote").toEqual([]);
    expect(sent.deletes, "m outside Graph asked the server").toEqual([]);
  } finally {
    outside.remove();
  }
});

test("m opens the node menu on the focused row", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  const row = await focusRow(view, STATEMENT_3);
  expect(menuOf(view)).toBeNull();
  await user.keyboard("m");
  await waitFor(() => expect(menuOf(view)).not.toBeNull());
  expect(entriesOf(view)[0], "the focus goes to the first entry").toHaveFocus();
  expect(row.contains(menuOf(view)), "the menu is the focused row's").toBe(true);
});

test("a press on the menu control opens the menu", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  await focusRow(view, STATEMENT_3);
  const control = await menuControl(view);
  expect(control.getAttribute("aria-expanded")).toBe("false");
  await user.click(control);
  await waitFor(() => expect(menuOf(view)).not.toBeNull());
  expect(control.getAttribute("aria-expanded")).toBe("true");
  expect(entriesOf(view)[0]).toHaveFocus();
});

/** The page's `issue` scope, which every key of the tree is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** The outline alone, as the page's body draws it: `IssueTree` over the tree `buildIssueTree`
 *  makes of the fixture's read, under the page's keymap and the providers a row's editing needs,
 *  with no editor behind it. `edited` keeps each document the tree put the caret in, and
 *  `undefined` where the caret leaves (`onEditing`). */
function drawOutline(): { readonly edited: (string | undefined)[]; readonly view: RenderResult } {
  const client = signedIn();
  client.setQueryData(outlineQuery(KEY, "graph").queryKey, converted);
  const edited: (string | undefined)[] = [];
  const root = buildIssueTree({ issue: issueDetails(), tree: converted });
  const view = render(
    <MemoryRouter initialEntries={[`/issues/${KEY}/graph`]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <FocusedNodeProvider>
              <DocumentAdmissionProvider>
                <DocumentStages>
                  <IssueScope />
                  <IssueTree
                    artifacts={new Map([[SPEC_DOCUMENT.slug, SPEC_DOCUMENT]])}
                    context={new Set()}
                    emptyLine={undefined}
                    focusReveal={false}
                    holdsEscape={false}
                    isClosed={false}
                    onEditing={(artifactId) => edited.push(artifactId)}
                    readsHeld
                    readsSettled
                    reveal={undefined}
                    root={root}
                    rootRow
                  />
                </DocumentStages>
              </DocumentAdmissionProvider>
            </FocusedNodeProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { edited, view };
}

// R1: from the caret, Escape gives the focus to the row first, which is today's `tree-leave-caret`
// (the caret is where `onEditing` says the tree put it); then `m` opens the menu.
test("Escape from the caret gives the focus to the row", async () => {
  const user = userEvent.setup();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const { edited, view } = drawOutline();
  const row = await focusRow(view, STATEMENT_3);
  await user.keyboard("{Enter}");
  await waitFor(() => expect(edited.at(-1), "Enter put no caret in the row").toBeDefined());
  await user.keyboard("{Escape}");
  await waitFor(() => expect(edited.at(-1), "Escape left the caret in the row").toBeUndefined());
  expect(row, "the focus is on the row").toHaveFocus();
  expect(menuOf(view), "the Escape opened the menu").toBeNull();
  await user.keyboard("m");
  await waitFor(() => expect(menuOf(view)).not.toBeNull());
});

// R1: the arrows move through every entry, a grey one included, and Home and End go to the ends.
// The first statement has no sibling above, so its Indent and Move up are grey.
test("the arrows reach every entry, grey ones included", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  await openMenu(view, user, STATEMENT_1);
  const all = entriesOf(view);
  expect(all[0]).toHaveFocus();
  for (const [at, each] of all.entries()) {
    expect(each, `ArrowDown did not reach entry ${at}`).toHaveFocus();
    await user.keyboard("{ArrowDown}");
  }
  // Past the last entry the focus goes round to the first, as the menu pattern has it.
  expect(all[0]).toHaveFocus();
  await user.keyboard("{ArrowUp}");
  expect(all[all.length - 1], "ArrowUp from the first").toHaveFocus();
  await user.keyboard("{Home}");
  expect(all[0]).toHaveFocus();
  await user.keyboard("{End}");
  expect(all[all.length - 1]).toHaveFocus();
  expect(all.filter(isGrey).map((each) => each.textContent?.slice(0, 6))).toContain(
    CONTROL_LABELS.edit_indent.slice(0, 6)
  );
});

test("Right opens a submenu and Left closes it", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerCatalogue();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  await openMenu(view, user);
  const lens = entry(view, LENS_WORDS.act);
  act(() => lens.focus());
  await user.keyboard("{ArrowRight}");
  await waitFor(() => expect(view.container.querySelectorAll('[role="menu"]')).toHaveLength(2));
  const submenu = view.container.querySelectorAll<HTMLElement>('[role="menu"]')[1];
  const first = submenu?.querySelector<HTMLElement>('[role="menuitem"]');
  await waitFor(() => expect(first, "the focus goes to the first kind").toHaveFocus());
  await user.keyboard("{ArrowLeft}");
  await waitFor(() => expect(view.container.querySelectorAll('[role="menu"]')).toHaveLength(1));
  expect(lens, "Left gives the focus back to its entry").toHaveFocus();
});

test("a grey entry cannot be picked", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  const sent = answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  await openMenu(view, user, STATEMENT_1);
  const indent = entry(view, CONTROL_LABELS.edit_indent);
  expect(isGrey(indent)).toBe(true);
  act(() => indent.focus());
  await user.keyboard("{Enter}");
  await user.click(indent);
  await letRequestsSettle();
  expect(sent.patches, "a grey entry wrote").toEqual([]);
  expect(menuOf(view), "a pick of a grey entry closed the menu").not.toBeNull();
});

test("a pick closes the menu and gives the focus back", async () => {
  const user = userEvent.setup();
  answerTreeReads(converted, NEVER);
  const sent = answerNodeRoutes({ deletes: [deleted.dry_run], patches: [moved.indent] });
  const view = draw();
  await openMenu(view, user);
  await user.click(entry(view, CONTROL_LABELS.edit_indent));
  await waitFor(() => expect(sent.patches).toHaveLength(1));
  expect(menuOf(view), "the pick left the menu open").toBeNull();
  await waitFor(() =>
    expect(focusedRowId(), "the focus is not on the row").toBe(rowId(STATEMENT_3))
  );
});

test("Escape closes the menu", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  const row = await openMenu(view, user);
  await user.keyboard("{Escape}");
  await waitFor(() => expect(menuOf(view)).toBeNull());
  expect(row, "the focus is back on the row").toHaveFocus();
});

// R2: a grey entry stays in the menu and in its order, says why, and is in the arrows' path.
test("the menu greys an entry with its reason and keeps it in the arrows' path", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  await openMenu(view, user, STATEMENT_1);
  const menu = menuOf(view);
  const entries = entriesOf(view);
  expect(
    entries.map((each) => each.textContent?.startsWith(ENTRY_WORDS[entries.indexOf(each)] ?? "?"))
  ).toEqual(ENTRY_WORDS.map(() => true));
  expect(isGrey(entry(view, CONTROL_LABELS.edit_indent))).toBe(true);
  expect(isGrey(entry(view, CONTROL_LABELS.edit_move_up))).toBe(true);
  expect(isGrey(entry(view, CONTROL_LABELS.edit_move_down))).toBe(false);
  expect(menu?.textContent).toContain(ANSWER_LINES.grey_no_sibling_above);
  expect(menu?.textContent).toContain(ANSWER_LINES.grey_first);
  expect(menu?.textContent, "Retype on a statement says only a paragraph takes a kind").toContain(
    ANSWER_LINES.grey_not_paragraph
  );
  // A node of another issue's own: every editing entry is grey, and says whose issue edits it.
  view.unmount();
  answerTreeReads();
  const other = draw();
  await openMenu(other, user, DECISION);
  expect(entriesOf(other).filter(isGrey).length).toBeGreaterThanOrEqual(7);
  expect(menuOf(other)?.textContent).toContain(ANSWER_LINES.grey_kind);
});

test("the Lens submenu holds W's kinds", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerCatalogue();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  await openMenu(view, user);
  await user.click(entry(view, LENS_WORDS.act));
  await waitFor(() => expect(view.container.querySelectorAll('[role="menu"]')).toHaveLength(2));
  const submenu = view.container.querySelectorAll<HTMLElement>('[role="menu"]')[1];
  await waitFor(() =>
    expect(
      [...(submenu?.querySelectorAll('[role="menuitem"]') ?? [])].map((each) => each.textContent)
    ).toEqual(CATALOGUE.map((one) => expect.stringContaining(one.words)))
  );
});

// R4: a folded new parent unfolds, so the row stays in view.
test("a folded new parent unfolds", async () => {
  const user = userEvent.setup();
  // The re-read never answers, so the outline stays as the answer of the move draws it.
  answerTreeReads(converted, NEVER);
  const sent = answerNodeRoutes({ deletes: [deleted.dry_run], patches: [moved.indent] });
  const view = draw();
  const second = await focusRow(view, STATEMENT_2);
  const check = nodeOf(converted, "<check-agent>");
  // Fold the second statement: its check is no longer drawn.
  if (rowOf(view, check) !== null) await user.keyboard("{ArrowLeft}");
  await waitFor(() => expect(rowOf(view, check), "the second statement did not fold").toBeNull());
  expect(second).toBeTruthy();
  await openMenu(view, user, STATEMENT_3);
  await user.click(entry(view, CONTROL_LABELS.edit_indent));
  await waitFor(() => expect(sent.patches).toHaveLength(1));
  // The node stands under the folded statement now, and its row is drawn: the statement unfolded.
  await waitFor(() =>
    expect(rowOf(view, STATEMENT_3), "the moved row is not drawn").not.toBeNull()
  );
  expect(rowOf(view, check), "the second statement stayed folded").not.toBeNull();
});

// R8: any other code, a 5xx and no answer draw one line that says the change was not made, and a
// press on it tries the act again. The server gives no such code, so the answer is made here.
test("a press on the fallback line tries the act again", async () => {
  const user = userEvent.setup();
  answerTreeReads(converted, NEVER);
  const sent = answerNodeRoutes({
    deletes: [deleted.dry_run],
    patches: [{ body: { code: "TEAPOT", error: "short and stout" }, status: 418 }, moved.indent],
  });
  const view = draw();
  await openMenu(view, user);
  await user.click(entry(view, CONTROL_LABELS.edit_indent));
  await waitFor(() => expect(sent.patches).toHaveLength(1));
  const line = await waitFor(() => {
    const found = [
      ...view.container.querySelectorAll<HTMLElement>('[role="status"] button, button'),
    ].find((each) => (each.textContent ?? "").includes(ANSWER_LINES.not_made));
    if (found === undefined) throw new Error("the row draws no fallback line");
    return found;
  });
  expect(line.textContent).toContain("TEAPOT");
  await user.click(line);
  await waitFor(() => expect(sent.patches, "the press did not try the act again").toHaveLength(2));
  expect(sent.patches[1]?.ifMatch).toBe(sent.patches[0]?.ifMatch);
  expect(sent.patches[1]?.body).toEqual(sent.patches[0]?.body);
});

/** `held` as the server reads it once it took the move `answer` of `node`, made since the page
 *  read `held`: the node and what stands under it, which the walk keeps together, under the
 *  answer's `parent`, first among that parent's children - an `after` of null, on a block of the
 *  spec, whose band stands first under any parent (dispatch://INFRA-1253 R7) - with the answer's
 *  `placement_seq`, and the parent named as the read names it on the children it holds already;
 *  every other node as `held` holds it. */
function movedRead(
  held: TreeAnswer,
  node: TreeAnswerNode,
  answer: {
    readonly after: string | null;
    readonly parent: string;
    readonly placement_seq: unknown;
  }
): TreeAnswer {
  const nodes = held.nodes ?? [];
  const at = nodes.indexOf(node);
  if (answer.after !== null || node.document?.slug !== SPEC_DOCUMENT.slug || at === -1) {
    throw new Error(`movedRead places a block of the spec first under its parent, not ${node.id}`);
  }
  const inside = new Set([node.id]);
  let end = at + 1;
  for (; end < nodes.length; end += 1) {
    const parent = nodes[end]?.parent;
    if (parent === null || parent === undefined || !inside.has(parent.id)) break;
    inside.add(nodes[end]?.id ?? "");
  }
  const rest = [...nodes.slice(0, at), ...nodes.slice(end)];
  const parentAt = rest.findIndex((each) => each.id === answer.parent);
  const ref = rest.find((each) => each.parent?.id === answer.parent)?.parent;
  if (parentAt === -1 || ref === null || ref === undefined) {
    throw new Error(`the read holds no parent ${answer.parent} with a child under it`);
  }
  const placed: TreeAnswerNode = {
    ...node,
    parent: ref,
    placement_seq: answer.placement_seq as TreeAnswerNode["placement_seq"],
  };
  return {
    ...held,
    nodes: [
      ...rest.slice(0, parentAt + 1),
      placed,
      ...nodes.slice(at + 1, end),
      ...rest.slice(parentAt + 1),
    ],
  };
}

/** The row the page draws of `child` right under the row of `parent`, by its key, or null. */
function rowUnder(
  view: RenderResult,
  parent: TreeAnswerNode,
  child: TreeAnswerNode
): HTMLElement | null {
  const path = `>${rowId(parent)}>${rowId(child)}`;
  const rows = [...view.container.querySelectorAll<HTMLElement>("[data-node-row]")];
  return rows.find((row) => row.dataset.rowKey?.endsWith(path) === true) ?? null;
}

// R8: a 412 says the node moved since the page read it and reads the tree again, which draws the
// node where the server holds it, under another parent; the focus stays on the node's row there,
// where the reader was, and the move queued behind the refused one, from the menu opened again,
// is dropped unsent. The 412 is the fixture's: the third statement's Indent sent with the
// `placement_seq` the page read before that same Indent was taken, whose answer the read again
// draws. The live feed is up, as on the page, so the refusal reads the outline once.
test("a 412 keeps the focus on the row where the server holds it", async () => {
  const user = userEvent.setup();
  const precondition = fixture.refused.PRECONDITION_FAILED;
  const refused = Promise.withResolvers<void>();
  const reads = answerTreeReads(
    converted,
    refused.promise.then(() => movedRead(converted, STATEMENT_3, moved.indent.body))
  );
  const out = Promise.withResolvers<void>();
  const sent = answerNodeRoutes({
    deletes: [deleted.dry_run, deleted.dry_run],
    patches: [{ ...precondition, gate: out.promise, returned: () => refused.resolve() }],
  });
  const client = signedIn();
  liveFeedOn(client);
  const view = draw(undefined, undefined, client);
  await openMenu(view, user);
  await user.click(entry(view, CONTROL_LABELS.edit_indent));
  await waitFor(() => expect(sent.patches).toHaveLength(1));
  // While the Indent is out, a second move waits behind it, picked from the menu opened again.
  await user.click(await menuControl(view));
  await waitFor(() => expect(menuOf(view)).not.toBeNull());
  await user.click(entry(view, CONTROL_LABELS.edit_move_up));
  await waitFor(() =>
    expect(focusedRowId(), "the pick left the focus off the row").toBe(rowId(STATEMENT_3))
  );
  out.resolve();
  // The read again draws the third statement under the second, and the focus is on that row.
  const under = await waitFor(() => {
    const found = rowUnder(view, STATEMENT_2, STATEMENT_3);
    if (found === null) throw new Error("the read again draws no row of it under the second");
    return found;
  });
  await waitFor(() => expect(document.activeElement, "the focus left the row").toBe(under));
  expect(under.textContent, "the row says the node moved and one move was not sent").toContain(
    `${ANSWER_LINES.refused_precondition_failed} 1 ${ANSWER_LINES.dropped_move}`
  );
  await letRequestsSettle();
  expect(document.activeElement, "the focus did not stay on the refused row").toBe(under);
  expect(sent.patches, "the queued move was sent").toEqual([
    {
      body: precondition.request.body,
      dryRun: false,
      expected: null,
      id: "<statement-3>",
      ifMatch: precondition.request.if_match,
    },
  ]);
  expect(reads.outline(), "the refusal did not read the outline once").toBe(2);
});

// R6, the dry run's table: closed -> counting -> (failed -> counting) -> ready.
test("delete reads counting while the dry run is out and asks again after a failed read", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  const out = Promise.withResolvers<void>();
  const sent = answerNodeRoutes({
    deletes: [
      { ...deleted.dry_run, gate: out.promise },
      { body: { code: "UNAVAILABLE", error: "the read is refused" }, status: 503 },
      deleted.dry_run,
    ],
  });
  const view = draw();
  await openMenu(view, user);
  await waitFor(() => expect(sent.deletes).toHaveLength(1));
  const counting = entry(view, CONTROL_LABELS.edit_delete);
  expect(counting.textContent).toBe(
    `${CONTROL_LABELS.edit_delete} ${CONTROL_LABELS.delete_counting}`
  );
  expect(isGrey(counting), "Delete is pickable while the count is out").toBe(true);
  out.resolve();
  await waitFor(() =>
    expect(entry(view, CONTROL_LABELS.edit_delete).textContent).toBe(
      `${CONTROL_LABELS.edit_delete} ${deleted.dry_run.body.count} ${CONTROL_LABELS.delete_rows}`
    )
  );
  // Closed and opened again, the menu asks again; the second answer fails.
  await user.keyboard("{Escape}");
  await user.click(await menuControl(view));
  await waitFor(() => expect(sent.deletes).toHaveLength(2));
  const line = await waitFor(() => {
    const found = [...view.container.querySelectorAll<HTMLElement>("button")].find(
      (each) => each.textContent === ANSWER_LINES.delete_count_failed
    );
    if (found === undefined) throw new Error("no line says the count could not be read");
    return found;
  });
  expect(isGrey(entry(view, CONTROL_LABELS.edit_delete))).toBe(true);
  // A press on the line asks again.
  await user.click(line);
  await waitFor(() => expect(sent.deletes).toHaveLength(3));
  await waitFor(() =>
    expect(entry(view, CONTROL_LABELS.edit_delete).textContent).toContain(
      String(deleted.dry_run.body.count)
    )
  );
  expect(
    sent.deletes.every((each) => each.dryRun),
    "a request was no dry run"
  ).toBe(true);
});

test("a node R2 greys sends no dry run", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  const sent = answerNodeRoutes();
  const view = draw();
  await openMenu(view, user, DECISION);
  const grey = entry(view, CONTROL_LABELS.edit_delete);
  expect(isGrey(grey)).toBe(true);
  expect(menuOf(view)?.textContent).toContain(ANSWER_LINES.grey_kind);
  await letRequestsSettle();
  expect(sent.deletes, "the menu sent a dry run for a node R2 greys").toEqual([]);
});

test("delete names the dry run's count", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  const sent = answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  await openMenu(view, user);
  await waitFor(() =>
    expect(entry(view, CONTROL_LABELS.edit_delete).textContent).toBe(
      `${CONTROL_LABELS.edit_delete} 4 ${CONTROL_LABELS.delete_rows}`
    )
  );
  expect(deleted.dry_run.body.count).toBe(4);
  expect(isGrey(entry(view, CONTROL_LABELS.edit_delete))).toBe(false);
  // One dry run, the node's own, with no precondition and no count (R12).
  expect(sent.deletes).toEqual([
    { body: null, dryRun: true, expected: null, id: "<statement-3>", ifMatch: null },
  ]);
});

test("a refused dry run greys delete with its reason", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  // The first statement holds a result under its check, which is never deleted.
  answerNodeRoutes({ deletes: [deleted.refused["NODE_HOLDS_RESULT statement"]] });
  const view = draw();
  await openMenu(view, user, STATEMENT_1);
  await waitFor(() => expect(isGrey(entry(view, CONTROL_LABELS.edit_delete))).toBe(true));
  await waitFor(() =>
    expect(menuOf(view)?.textContent).toContain(ANSWER_LINES.refused_node_holds_result)
  );
});

test("a dry run answering gone reads the tree again", async () => {
  const user = userEvent.setup();
  const reads = answerTreeReads(converted, NEVER);
  answerNodeRoutes({ deletes: [deleted.refused.NODE_GONE] });
  const view = draw();
  await focusRow(view, STATEMENT_3);
  expect(reads.outline()).toBe(1);
  await user.click(await menuControl(view));
  await waitFor(() => expect(reads.outline(), "the page did not read the tree again").toBe(2));
  await letRequestsSettle();
  expect(reads.outline(), "the page read the tree more than once").toBe(2);
});

test("a failed dry run says the count could not be read", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerNodeRoutes({
    deletes: [{ body: { code: "UNAVAILABLE", error: "the read is refused" }, status: 503 }],
  });
  const view = draw();
  await openMenu(view, user);
  await waitFor(() =>
    expect(
      [...view.container.querySelectorAll("button")].some(
        (each) => each.textContent === ANSWER_LINES.delete_count_failed
      )
    ).toBe(true)
  );
  expect(isGrey(entry(view, CONTROL_LABELS.edit_delete))).toBe(true);
});

test("closing the menu drops the dry run", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  const out = Promise.withResolvers<void>();
  const sent = answerNodeRoutes({
    deletes: [{ ...deleted.dry_run, gate: out.promise }, deleted.dry_run],
  });
  const view = draw();
  await openMenu(view, user);
  await waitFor(() => expect(sent.deletes).toHaveLength(1));
  await user.keyboard("{Escape}");
  await waitFor(() => expect(menuOf(view)).toBeNull());
  out.resolve();
  await letRequestsSettle();
  // An answer that comes after the close is drawn nowhere.
  expect(menuOf(view)).toBeNull();
  expect(view.container.textContent).not.toContain(CONTROL_LABELS.delete_rows);
  // The next opening asks again, and draws its own answer.
  await user.click(await menuControl(view));
  await waitFor(() => expect(sent.deletes).toHaveLength(2));
  await waitFor(() =>
    expect(entry(view, CONTROL_LABELS.edit_delete).textContent).toContain(
      CONTROL_LABELS.delete_rows
    )
  );
});

/** The menu opened on the third statement, its dry run answered, and Delete picked: the confirm. */
async function confirmDialog(view: RenderResult, user: UserEvent): Promise<HTMLElement> {
  await openMenu(view, user);
  await waitFor(() =>
    expect(entry(view, CONTROL_LABELS.edit_delete).textContent).toContain(
      CONTROL_LABELS.delete_rows
    )
  );
  await user.click(entry(view, CONTROL_LABELS.edit_delete));
  return waitFor(() => {
    const dialog = document.querySelector<HTMLElement>('[role="dialog"]');
    if (dialog === null) throw new Error("the pick opened no confirm");
    return dialog;
  });
}

test("the confirm names the dry run's count", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  const dialog = await confirmDialog(view, user);
  expect(dialog.textContent).toContain(ANSWER_LINES.delete_confirm_title);
  expect(dialog.textContent).toContain(
    `${ANSWER_LINES.confirm_takes} 4 ${CONTROL_LABELS.delete_rows}`
  );
});

test("cancel sends nothing", async () => {
  const user = userEvent.setup();
  answerTreeReads();
  const sent = answerNodeRoutes({ deletes: [deleted.dry_run] });
  const view = draw();
  const dialog = await confirmDialog(view, user);
  const cancel = [...dialog.querySelectorAll("button")].find(
    (each) => each.textContent === "Cancel"
  );
  if (cancel === undefined) throw new Error("the confirm has no Cancel");
  await user.click(cancel);
  await waitFor(() => expect(document.querySelector('[role="dialog"]')).toBeNull());
  await letRequestsSettle();
  expect(sent.deletes, "Cancel sent a request beyond the dry run").toHaveLength(1);
  expect(sent.patches).toEqual([]);
});

test("confirm sends one DELETE with If-Match and expected_count", async () => {
  const user = userEvent.setup();
  answerTreeReads(converted, NEVER);
  const sent = answerNodeRoutes({ deletes: [deleted.dry_run, deleted.taken] });
  const view = draw();
  const dialog = await confirmDialog(view, user);
  await user.click(
    [...dialog.querySelectorAll("button")].find(
      (each) => each.textContent === CONTROL_LABELS.confirm_delete
    ) as HTMLElement
  );
  await waitFor(() => expect(sent.deletes).toHaveLength(2));
  expect(sent.deletes[1]).toEqual({
    body: null,
    dryRun: false,
    expected: "4",
    id: "<statement-3>",
    ifMatch: `"${STATEMENT_3.placement_seq}"`,
  });
});

// R6: the nodes the server marks gone leave Graph, and the focus goes to the nearest row, with
// one re-read of the outline (R7). The re-read answers the tree after the delete, which the
// fixture holds as `deleted.tree_after`, and so only once the taken delete's answer has gone back
// to the page. The live feed is up, as on the page, so the refused delete before it reads nothing.
test("a taken delete drops the gone nodes and moves the focus to the nearest row", async () => {
  const user = userEvent.setup();
  expect(treeAfterDelete, "the fixture holds no tree read after the taken delete").toBeDefined();
  const after = renamed(treeAfterDelete?.body) as unknown as TreeAnswer;
  const taken = Promise.withResolvers<void>();
  const reads = answerTreeReads(
    converted,
    taken.promise.then(() => after)
  );
  const sent = answerNodeRoutes({
    deletes: [
      deleted.dry_run,
      deleted.refused.DELETE_COUNT_MOVED,
      { ...deleted.taken, returned: () => taken.resolve() },
    ],
  });
  const client = signedIn();
  liveFeedOn(client);
  const view = draw(undefined, undefined, client);
  let dialog = await confirmDialog(view, user);
  // Where the deleted row stands among the rows, which the nearest row stands at once it is gone.
  const at = rowIds(view).indexOf(rowId(STATEMENT_3));
  expect(at, "the third statement's row is not drawn").toBeGreaterThanOrEqual(0);
  const confirm = (within: HTMLElement) =>
    [...within.querySelectorAll("button")].find(
      (each) => each.textContent === CONTROL_LABELS.confirm_delete
    ) as HTMLElement;
  await user.click(confirm(dialog));
  // The count moved: the confirm opens again, with the new count, and is confirmed again.
  await waitFor(() =>
    expect(dialog.textContent ?? "").toContain(
      String(deleted.refused.DELETE_COUNT_MOVED.body.count)
    )
  );
  dialog = document.querySelector<HTMLElement>('[role="dialog"]') as HTMLElement;
  await user.click(confirm(dialog));
  await waitFor(() => expect(sent.deletes).toHaveLength(3));
  await waitFor(() =>
    expect(rowOf(view, STATEMENT_3), "the deleted row is still drawn").toBeNull()
  );
  // Rows that stand under it left with it.
  for (const gone of ["<lens>", "<finding>", "<risk>"]) {
    expect(
      view.container.querySelector(
        `[data-node-row][data-node-id*="${nodeOf(converted, gone).block_id ?? gone}"]`
      ),
      `${gone} is still drawn`
    ).toBeNull();
  }
  // The focus is on a row of the outline, the one nearest where the deleted row stood: the row
  // now at its place, or the last where none is (`IssueTree.tsx`'s drop rule).
  await waitFor(() => expect(focusedRowId(), "the focus is on no row").toBeDefined());
  expect(focusedRowId()).not.toBe(rowId(STATEMENT_3));
  const left = rowIds(view);
  expect(focusedRowId(), "the focus is not on the nearest row").toBe(
    left[Math.min(at, left.length - 1)]
  );
  // One re-read of the outline after the answer, and none besides the first load.
  await letRequestsSettle();
  expect(reads.outline()).toBe(2);
});

test("the confirm asks again with the new count", async () => {
  const user = userEvent.setup();
  answerTreeReads(converted, NEVER);
  const sent = answerNodeRoutes({
    deletes: [deleted.dry_run, deleted.refused.DELETE_COUNT_MOVED, deleted.taken],
  });
  const view = draw();
  const first = await confirmDialog(view, user);
  const confirm = (within: HTMLElement) =>
    [...within.querySelectorAll("button")].find(
      (each) => each.textContent === CONTROL_LABELS.confirm_delete
    ) as HTMLElement;
  await user.click(confirm(first));
  const count = deleted.refused.DELETE_COUNT_MOVED.body.count;
  await waitFor(() =>
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain(
      `${ANSWER_LINES.confirm_takes} ${count} ${CONTROL_LABELS.delete_rows}`
    )
  );
  // The reader confirms again: one DELETE more, with the count the answer gave.
  await user.click(confirm(document.querySelector<HTMLElement>('[role="dialog"]') as HTMLElement));
  await waitFor(() => expect(sent.deletes).toHaveLength(3));
  expect(sent.deletes.map((each) => each.expected)).toEqual([null, "4", String(count)]);
});

test("a delete answering gone reads the tree again", async () => {
  const user = userEvent.setup();
  const reads = answerTreeReads(converted, NEVER);
  const sent = answerNodeRoutes({ deletes: [deleted.dry_run, deleted.refused.NODE_GONE] });
  const view = draw();
  const dialog = await confirmDialog(view, user);
  expect(reads.outline()).toBe(1);
  await user.click(
    [...dialog.querySelectorAll("button")].find(
      (each) => each.textContent === CONTROL_LABELS.confirm_delete
    ) as HTMLElement
  );
  await waitFor(() => expect(sent.deletes).toHaveLength(2));
  await waitFor(() => expect(reads.outline(), "the page did not read the tree again").toBe(2));
  expect(document.querySelector('[role="dialog"]')).toBeNull();
});

test("a refused delete keeps the tree and draws its line", async () => {
  const user = userEvent.setup();
  const reads = answerTreeReads();
  const sent = answerNodeRoutes({ deletes: [deleted.dry_run, deleted.refused.PARENT_CONTENDED] });
  // The live feed is up, as on the page: the end of a refused write owes no read.
  const client = signedIn();
  liveFeedOn(client);
  const view = draw(undefined, undefined, client);
  const dialog = await confirmDialog(view, user);
  await user.click(
    [...dialog.querySelectorAll("button")].find(
      (each) => each.textContent === CONTROL_LABELS.confirm_delete
    ) as HTMLElement
  );
  await waitFor(() => expect(sent.deletes).toHaveLength(2));
  await waitFor(() => expect(document.querySelector('[role="dialog"]')).toBeNull());
  // The row stands where it did, with its line, and the tree was read no more.
  const row = rowOf(view, STATEMENT_3);
  expect(row, "the refused delete took the row away").not.toBeNull();
  await waitFor(() => expect(row?.textContent).toContain(ANSWER_LINES.refused_parent_contended));
  await letRequestsSettle();
  expect(reads.outline()).toBe(1);
  expect(focusedRowId()).toBe(rowId(STATEMENT_3));
});

// R6: the delete takes the placement_seq of the newest answer of the node's moves, and is sent once
// the queue is empty.
test("a delete waits for the node's moves", async () => {
  const user = userEvent.setup();
  answerTreeReads(converted, NEVER);
  const out = Promise.withResolvers<void>();
  const sent = answerNodeRoutes({
    deletes: [deleted.dry_run, deleted.dry_run, deleted.taken],
    patches: [{ ...moved.indent, gate: out.promise }],
  });
  const view = draw();
  await openMenu(view, user);
  await user.click(entry(view, CONTROL_LABELS.edit_indent));
  await waitFor(() => expect(sent.patches).toHaveLength(1));
  // The move is out. The reader opens the menu again and confirms a delete.
  await user.click(await menuControl(view));
  await waitFor(() =>
    expect(entry(view, CONTROL_LABELS.edit_delete).textContent).toContain(
      CONTROL_LABELS.delete_rows
    )
  );
  await user.click(entry(view, CONTROL_LABELS.edit_delete));
  const dialog = await waitFor(() => {
    const found = document.querySelector<HTMLElement>('[role="dialog"]');
    if (found === null) throw new Error("the pick opened no confirm");
    return found;
  });
  await user.click(
    [...dialog.querySelectorAll("button")].find(
      (each) => each.textContent === CONTROL_LABELS.confirm_delete
    ) as HTMLElement
  );
  await letRequestsSettle();
  const real = () => sent.deletes.filter((each) => !each.dryRun);
  expect(real(), "the delete was sent while a move was out").toEqual([]);
  out.resolve();
  await waitFor(() => expect(real()).toHaveLength(1));
  // It carries the placement_seq the move's answer gave.
  expect(real()[0]?.ifMatch).toBe(`"${moved.indent.body.placement_seq}"`);
});
