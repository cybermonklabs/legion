import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor, within } from "@testing-library/react";
import { type ReactNode, useCallback, useMemo, useState } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { GraphReferences, IssueDetails, Project, ReferenceTargets } from "../../api/types";
import { Margin } from "../margin/Margin";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import * as markdownBodyModule from "../refs/MarkdownBody";
import { buildDispatchReference } from "../refs/routes";
import { appKeymap, type KeyBindingDescription } from "../shell/keymap";
import { ShortcutHelp } from "../shell/ShortcutHelp";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import {
  DETAILS_ATTRIBUTE,
  type MarginDrawer,
  type MarginDrawerPage,
  MarginSlotProvider,
} from "./margin-slot";
import { TWO_THREADS } from "./outline-fixtures";
import { PROPERTY_LABELS } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { ShortcutHelpOpener } from "./shortcut-help-opener";
import { type TasksByHand, tasksByHand } from "./slices-fixtures";
import { AppProviders, commentNodes, stubReads, treeOf } from "./slots/stage-fixtures";

/**
 * A press on Details (R2; the plan's state table "A press on Details", one case for each of its
 * four rows). The page's whole root is drawn at Conversation's address, beside the shell's Details
 * drawer (`margin/Margin.tsx`), whose open state the shell holds (`app.tsx`'s `ShellProviders`):
 * here a state of the case's own shell. The reads Details makes beyond what the page holds - the
 * titles of the issues this one waits on, and what waits on it (`IssueMargin.tsx`) - are held by
 * the case, so it reads the drawer in the press's commit, before any of them has answered.
 *
 * The press's commit is what the document holds once `act` has run the press's click and every
 * render it asked for, with no read answered in between. A read sent from the press's handler is
 * sent before that commit, so the request log notes whether the drawer read as open as each read
 * was asked for (`openAtAsk`): shut, where the handler sent it.
 */

/** The issue the cases draw: the fixtures' one, waiting on one other issue, so Details reads that
 *  issue's title as it opens, beside what waits on this one. */
function waitingIssue(): IssueDetails {
  return { ...issueDetails(), blocked_by: ["CORE-2"] };
}

/** The drawer, open: the shell's `aside`, as the load rows read it (`e2e/issue-page-load.ts`). */
const DETAILS_OPEN = 'aside[data-testid="desktop-margin-shell"]:not([hidden])';

/** The header's Details control. */
const DETAILS_CONTROL = '[data-testid="issue-details-control"]';

/** The reference of the fixtures' issue, which what waits on it is read by. */
const ISSUE_REFERENCE = buildDispatchReference({ key: KEY, kind: "issue" });

/** One read Details makes as it opens, and whether the drawer read as open as it was asked. */
interface AskedRead {
  readonly openAtAsk: boolean;
  readonly read: "blocks" | "titles";
}

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;
/** Each read of `AskedRead` since the case began, in order. */
let log: AskedRead[] = [];
/** An answer the case holds until it settles it: what `Promise.withResolvers` gives. */
interface Held<T> {
  readonly promise: Promise<T>;
  readonly reject: (reason?: unknown) => void;
  readonly resolve: (value: T) => void;
}
/** The held answers of what waits on the issue and of the titles, each the first not answered
 *  yet first. */
let blocks: Held<GraphReferences>[] = [];
let titles: Held<ReferenceTargets>[] = [];
/** The projects read, which the header and Details both read on a root, held by the case. */
let projects: Mock<typeof api.listProjects>;

/** Whether the drawer reads as open now. */
function drawerOpen(): boolean {
  return document.querySelector(DETAILS_OPEN) !== null;
}

beforeEach(() => {
  log = [];
  blocks = [];
  titles = [];
  projects = spyOn(api, "listProjects").mockResolvedValue([]);
  stubs = [
    ...stubReads(),
    projects,
    spyOn(api, "getIssue").mockResolvedValue(waitingIssue()),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === undefined ? treeOf(commentNodes(TWO_THREADS)) : treeOf([])
    ),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    // The margin beside the page reads the reader's inbox, which Details draws nothing from.
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "getReferences").mockImplementation((reference, options) => {
      if (reference !== ISSUE_REFERENCE || options?.kind !== "blocked_by") {
        return Promise.resolve({ edges: [], node: graphNode({ id: KEY, kind: "issue" }) });
      }
      log.push({ openAtAsk: drawerOpen(), read: "blocks" });
      const held = Promise.withResolvers<GraphReferences>();
      blocks.push(held);
      return held.promise;
    }),
    // The page's own names read asks for many references; the titles Details reads ask for the
    // issues it waits on alone.
    spyOn(api, "getReferenceTargets").mockImplementation((refs) => {
      const asked = [...refs];
      const blocker = buildDispatchReference({ key: "CORE-2", kind: "issue" });
      if (asked.length !== 1 || asked[0] !== blocker) return Promise.resolve({ targets: [] });
      log.push({ openAtAsk: drawerOpen(), read: "titles" });
      const held = Promise.withResolvers<ReferenceTargets>();
      titles.push(held);
      return held.promise;
    }),
  ];
  tasks = tasksByHand();
});
afterEach(() => {
  cleanup();
  tasks.restore();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

/** The shell around the page, as `app.tsx` draws it for the new page: the Details drawer, whose
 *  open state it holds, handed to the page through the margin's slot, and the margin that draws
 *  the drawer beside the page. */
function Shell({ children }: { children: ReactNode }): ReactNode {
  const [open, setOpen] = useState(false);
  const [page, setPage] = useState<MarginDrawerPage | undefined>(undefined);
  const drawer = useMemo<MarginDrawer>(
    () => ({ open, setOpen, setPage, ...(page === undefined ? {} : { page }) }),
    [open, page]
  );
  return (
    <MarginSlotProvider drawer={drawer} drawsSlot>
      {children}
      <Margin />
    </MarginSlotProvider>
  );
}

/** The page's root at Conversation's address, beside the shell's drawer, at 1440 px. */
function drawPage(): { client: QueryClient; view: RenderResult } {
  restoreViewport = stubViewport(DESKTOP_WIDTH);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <Shell>
          <IssueOutlinePage />
        </Shell>
      </AppProviders>
    </MemoryRouter>
  );
  return { client, view };
}

/** The header's Details control, once the header is drawn. */
async function detailsControl(view: RenderResult): Promise<HTMLElement> {
  return waitFor(() => {
    const control = view.baseElement.querySelector<HTMLElement>(DETAILS_CONTROL);
    if (control === null) throw new Error("the header drew no Details control");
    return control;
  });
}

/** Presses `button` and runs every render the press asks for, with no read answered between. */
function press(button: HTMLElement): void {
  act(() => button.click());
}

/** Details' row named `label`, in the open drawer. */
function detailsRow(label: string): HTMLElement | undefined {
  const drawer = document.querySelector(DETAILS_OPEN);
  return [...(drawer?.querySelectorAll<HTMLElement>(".grid") ?? [])].find(
    (row) => row.firstElementChild?.textContent?.includes(label) === true
  );
}

/** Each row of the open drawer's Development, with what it draws. */
function developmentRows(): { readonly row: HTMLElement; readonly drawn: string }[] {
  const section = document.querySelector(`${DETAILS_OPEN} [data-margin-section="development"]`);
  return [...(section?.querySelectorAll<HTMLElement>(".grid") ?? [])].map((row) => ({
    drawn: row.outerHTML,
    row,
  }));
}

// Row 1. Closed, a press on Details opens the drawer in the press's commit, its sections drawn
// from what the page holds, and the reads it needs are sent from the press's handler.
test("a press on Details opens the drawer in its commit and sends its reads from the handler", async () => {
  const { view } = drawPage();
  const control = await detailsControl(view);
  await tasks.runAll();
  await waitFor(() =>
    expect(view.baseElement.querySelector("[data-issue-body] [data-node-row]")).not.toBeNull()
  );
  expect(drawerOpen(), "the drawer was open before the press").toBe(false);

  press(control);
  expect(drawerOpen(), "the press left the drawer shut in its commit").toBe(true);
  expect(control, "the press left Details' control unpressed").toHaveAttribute(
    "aria-expanded",
    "true"
  );
  expect(
    detailsRow(PROPERTY_LABELS.status),
    "the drawer drew no Status row from what the page holds"
  ).toBeDefined();
  expect(
    log,
    "Details' reads were not sent from the press's handler, before the commit that opened it"
  ).toEqual([
    { openAtAsk: false, read: "titles" },
    { openAtAsk: false, read: "blocks" },
  ]);
});

// Row 2. Open, a press on Details closes the drawer in the press's commit.
test("a press on Details while it is open closes the drawer in its commit", async () => {
  const { view } = drawPage();
  const control = await detailsControl(view);
  press(control);
  expect(drawerOpen()).toBe(true);

  press(control);
  expect(drawerOpen(), "the press left the drawer open in its commit").toBe(false);
  expect(control).toHaveAttribute("aria-expanded", "false");
});

// Row 3. Open with its reads out, a read that answers fills its own line of the drawer, which was
// drawn before it answered, and moves no line above it.
test("a read of open Details that answers fills its own line and moves no line above it", async () => {
  const { view } = drawPage();
  press(await detailsControl(view));
  await waitFor(() => expect(blocks).toHaveLength(1));
  const before = developmentRows();
  const line = detailsRow(PROPERTY_LABELS.blocks);
  if (line === undefined) throw new Error("Details drew no Blocks line while its read was out");
  const at = before.findIndex(({ row }) => row === line);

  await act(async () =>
    blocks[0]?.resolve({
      edges: [],
      node: graphNode({ id: KEY, kind: "issue", ref: ISSUE_REFERENCE }),
    })
  );
  expect(
    detailsRow(PROPERTY_LABELS.blocks),
    "the answer drew its line again as another element"
  ).toBe(line);
  expect(line.textContent, "the answer did not fill its line").toContain("None");
  const after = developmentRows();
  expect(
    after.slice(0, at).map(({ drawn }) => drawn),
    "a line above the one the read fills changed as it answered"
  ).toEqual(before.slice(0, at).map(({ drawn }) => drawn));
});

// Row 4. A press during the load opens the drawer as row 1, and sends no read the load sent
// already: the projects read, which the header sends for a root and Details reads too.
test("a press on Details during the load opens it in its commit and sends no read the load sent", async () => {
  const held = Promise.withResolvers<Project[]>();
  projects.mockReturnValue(held.promise);
  const { view } = drawPage();
  const control = await detailsControl(view);
  await waitFor(() => expect(projects, "the load sent no projects read").toHaveBeenCalledTimes(1));

  press(control);
  expect(drawerOpen(), "the press during the load left the drawer shut in its commit").toBe(true);
  expect(
    log.map(({ openAtAsk }) => openAtAsk),
    "Details' reads were not sent from the press's handler"
  ).toEqual([false, false]);
  await act(async () => {});
  expect(projects, "the press sent the projects read the load had sent").toHaveBeenCalledTimes(1);
  await act(async () => held.resolve([]));
});

/** The shell as `app.tsx` draws it for the new page: the Details drawer's open state and the Keys
 *  list's held above the router's `<Routes>`, which the shell draws in its own render, so a press
 *  on Details or on Keys draws `<Routes>` again and gives each route under it a route context of a
 *  new value. */
function RoutedShell(): ReactNode {
  const [open, setOpen] = useState(false);
  const [page, setPage] = useState<MarginDrawerPage | undefined>(undefined);
  const [keys, setKeys] = useState<readonly KeyBindingDescription[] | null>(null);
  const openKeys = useCallback(() => setKeys([]), []);
  const drawer = useMemo<MarginDrawer>(
    () => ({ open, setOpen, setPage, ...(page === undefined ? {} : { page }) }),
    [open, page]
  );
  return (
    <MarginSlotProvider drawer={drawer} drawsSlot>
      <ShortcutHelpOpener.Provider value={openKeys}>
        <Routes>
          <Route element={<IssueOutlinePage />} path="/issues/:key/*" />
        </Routes>
      </ShortcutHelpOpener.Provider>
      <Margin />
      <ShortcutHelp onClose={() => setKeys(null)} snapshot={keys} />
    </MarginSlotProvider>
  );
}

/**
 * Draws the page under `RoutedShell` at 1440 px, once its rows are drawn, and answers the record
 * of each row's text drawn since (`RowText.tsx`'s body, `surface="node-row-words"`), by its id:
 * the text draws its body in each of its own renders. The body is spied from the first drawing,
 * so every body keeps one component from its mount.
 */
async function drawRoutedPage(): Promise<{
  drawnAgain: () => readonly (string | undefined)[];
  view: RenderResult;
}> {
  restoreViewport = stubViewport(DESKTOP_WIDTH);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const bodies = spyOn(markdownBodyModule, "MarkdownBody");
  stubs.push(bodies);
  const view = render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <RoutedShell />
      </AppProviders>
    </MemoryRouter>
  );
  await detailsControl(view);
  await tasks.runAll();
  await waitFor(() =>
    expect(view.baseElement.querySelector("[data-issue-body] [data-node-row]")).not.toBeNull()
  );
  const rowTexts = () =>
    bodies.mock.calls
      .filter(([props]) => props.surface === "node-row-words")
      .map(([props]) => props.source?.id);
  const drawn = rowTexts().length;
  expect(drawn, "no row's text was drawn: this case proves nothing").toBeGreaterThan(0);
  return { drawnAgain: () => rowTexts().slice(drawn), view };
}

/** The node id of the row that holds the focus, where a row does. */
function focusedRow(): string | undefined {
  return document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
}

/** The rows `drawn` holds beside `named`, the rows whose `focused` prop the press changed: the
 *  row that lost the focus and the row that took it, each drawn again for its own prop, as it
 *  always was (dispatch://INFRA-5939/comment/5883e47d-b955-4a09-8950-14a04768e56d). */
function besideTheFocus(
  drawn: readonly (string | undefined)[],
  named: readonly (string | undefined)[]
): readonly (string | undefined)[] {
  return drawn.filter((id) => id === undefined || !named.includes(id));
}

/** The node id of the outline's row that holds its tab stop, where a row does. */
function tabStopRow(): string | undefined {
  return document.querySelector<HTMLElement>('[data-node-row][tabindex="0"]')?.dataset.nodeId;
}

// Row 5 (C). Closed, a press on Details, under a shell that draws `<Routes>` again as `app.tsx`
// does: the press's task draws the drawer, and no row's text again but the one it takes the focus
// from. A row's text reads nothing of the route's context (`RowText.tsx`'s `useFollowInPlace`),
// so the new `<Routes>` leaves it as it was.
test("(C) a press on Details opens the drawer and draws no row's text again", async () => {
  const { drawnAgain, view } = await drawRoutedPage();
  const before = focusedRow();
  press(await detailsControl(view));
  expect(drawerOpen(), "the press left the drawer shut in its commit").toBe(true);
  expect(focusedRow(), "the press left the focus on a row, not in the drawer").toBeUndefined();
  // The one row whose `focused` the press changed: the row that held the focus, if one did.
  expect(
    besideTheFocus(drawnAgain(), [before]),
    "the press on Details drew again the text of a row other than the one that lost the focus"
  ).toEqual([]);
});

// The same for Keys, whose list the shell holds beside Details' drawer (`app.tsx`'s `openHelp`).
test("(C) a press on Keys opens the list of keys and draws no row's text again", async () => {
  const { drawnAgain, view } = await drawRoutedPage();
  const keys = [...view.baseElement.querySelectorAll<HTMLElement>("button")].find(
    (button) => button.textContent === CONTROL_LABELS.keys
  );
  if (keys === undefined) throw new Error("the header drew no Keys control");
  const before = focusedRow();
  const tabStop = tabStopRow();
  if (tabStop === undefined) throw new Error("the outline drew no row with its tab stop");
  press(keys);
  expect(
    document.querySelector('[role="dialog"]'),
    "the press left the list of keys shut in its commit"
  ).not.toBeNull();
  expect(
    document.activeElement?.closest('[role="dialog"]'),
    "the press left the focus outside the list of keys"
  ).not.toBeNull();
  // The rows whose `focused` the press changed: the row that held the focus, if one did, and the
  // tab-stop row that `showKeys` gives the focus before the list takes it (`IssuePresets.tsx`).
  expect(
    besideTheFocus(drawnAgain(), [before, tabStop]),
    "the press on Keys drew again the text of a row other than the ones whose focus it moved"
  ).toEqual([]);
});

/** The outline's keys whose `when` reads where the focus stands among the outline's rows
 *  (`useTreeKeymap.ts`'s `drawn`), each of which applies while a row holds the focus itself. */
const ROW_KEYS = [
  "tree-arrows",
  "tree-edit",
  "tree-fold",
  "tree-leave",
  "tree-next",
  "tree-previous",
  "tree-toggle",
  "tree-unfold",
  "tree-zoom-in",
] as const;

/** The selector the outline's keys read its rows by (`useTreeKeymap.ts`'s `NODE_ROW`). */
const NODE_ROW = "[data-node-row]";

// Row 5 (C), the list a press on Keys opens: `app.tsx`'s `openHelp` asks every key whether it
// applies (`keymap.ts`'s `describe`), with the focus on the outline's tab-stop row
// (`IssuePresets.tsx`'s `showKeys`). Each of the outline's keys read the rows for its own answer,
// one `querySelectorAll` over every row for each key. The rows are read once for the whole list,
// and each key answers as it did, from a row and from the page itself.
test("(C) the list a press on Keys opens reads the outline's rows once, and each key answers as before", async () => {
  const { view } = await drawRoutedPage();
  const tabStop = view.baseElement.querySelector<HTMLElement>(`${NODE_ROW}[tabindex="0"]`);
  if (tabStop === null) throw new Error("the outline drew no row with its tab stop");
  const querySelectorAll = Element.prototype.querySelectorAll;
  /** The element each read of the rows was made on, since the list was last opened. */
  let reads: Element[] = [];
  stubs.push(
    spyOn(Element.prototype, "querySelectorAll").mockImplementation(function (
      this: Element,
      selectors: string
    ) {
      if (selectors === NODE_ROW) reads.push(this);
      return querySelectorAll.call(this, selectors);
    } as typeof querySelectorAll)
  );
  /** Opens the list as `openHelp` does, with the focus where it is now: the keys of the outline
   *  that apply, and how many times the rows of each tree were read. */
  const openList = () => {
    reads = [];
    const listed = appKeymap.describe().filter(({ scope }) => scope === "issue");
    const perTree = new Map<Element, number>();
    for (const tree of reads) perTree.set(tree, (perTree.get(tree) ?? 0) + 1);
    return {
      applying: (ids: readonly string[]) =>
        ids.filter((id) => listed.find((key) => key.id === id)?.enabled === true),
      readsPerTree: [...perTree.values()],
    };
  };

  act(() => tabStop.focus({ preventScroll: true }));
  const fromARow = openList();
  expect(
    fromARow.readsPerTree.length,
    "the list read no tree's rows: this case proves nothing"
  ).toBeGreaterThan(0);
  expect(
    fromARow.readsPerTree,
    "the list read a tree's rows more than once, for each key it asked"
  ).toEqual(fromARow.readsPerTree.map(() => 1));
  expect(
    fromARow.applying([...ROW_KEYS, "tree-enter"]),
    "from a row, the outline's keys of the focused row answer otherwise than they did"
  ).toEqual([...ROW_KEYS]);

  // The next list, in a later turn, from the page itself: its own read, and its own answers.
  await act(async () => tabStop.blur());
  const fromThePage = openList();
  expect(fromThePage.readsPerTree, "the next list read a tree's rows other than once").toEqual(
    fromARow.readsPerTree.map(() => 1)
  );
  expect(
    fromThePage.applying([...ROW_KEYS, "tree-enter"]),
    "from the page itself, the outline's keys answer otherwise than they did"
  ).toEqual(["tree-next", "tree-previous", "tree-enter"]);
});

/** A search for one control by its name, the whole selector: how Add finds the control an entry
 *  of its menu reaches (`IssueAddMenu.tsx`'s `controlNamed`). */
const CONTROL_BY_NAME = /^\[aria-label="(?:[^"\\]|\\.)*"\]$/;

/** The prototype of `node`'s chain that owns `querySelector`, where a spy of it goes: happy-dom's
 *  document is no instance of the global `Document`, so its own chain is read. */
function querySelectorOwner(node: Node): { querySelector: (selectors: string) => Element | null } {
  let owner: unknown = Object.getPrototypeOf(node);
  while (owner !== null && !Object.hasOwn(owner as object, "querySelector")) {
    owner = Object.getPrototypeOf(owner);
  }
  if (owner === null) throw new Error("no prototype of the node owns querySelector");
  return owner as { querySelector: (selectors: string) => Element | null };
}

// Row 5 (C), the frame after a press on Details: as Details opens, Add reads which of its entries
// reach a control that takes a press (`IssueAddMenu.tsx`'s `read`), in that frame, each by the
// control's name. Searched over the whole page, each name walked every row of the outline. Every
// control an entry reaches is in Details or in the page's header, the two Add watches, so the
// search is made there alone, and Add lists what it listed.
test("(C) after a press on Details, Add finds each entry's control in Details or the header alone, and lists what it did", async () => {
  const { view } = drawPage();
  const control = await detailsControl(view);
  await tasks.runAll();
  await waitFor(() =>
    expect(view.baseElement.querySelector("[data-issue-body] [data-node-row]")).not.toBeNull()
  );
  /** Each search for a control by its name since the press, and what it was made on. */
  const searches: { readonly on: Node; readonly selector: string }[] = [];
  for (const owner of [querySelectorOwner(document), querySelectorOwner(document.body)]) {
    const query = owner.querySelector;
    stubs.push(
      spyOn(owner, "querySelector").mockImplementation(function (this: Node, selectors: string) {
        if (CONTROL_BY_NAME.test(selectors)) searches.push({ on: this, selector: selectors });
        return query.call(this, selectors);
      })
    );
  }

  press(control);
  // The frame after the press, whose read sets what Add lists.
  const frame = Promise.withResolvers<void>();
  requestAnimationFrame(() => frame.resolve());
  await act(() => frame.promise);
  act(() => view.getByRole("button", { name: CONTROL_LABELS.add }).click());
  expect(
    within(view.getByRole("menu", { name: CONTROL_LABELS.add }))
      .getAllByRole("menuitem")
      .map((entry) => entry.textContent),
    "Add lists other entries than it did"
  ).toEqual([
    PROPERTY_LABELS.parent,
    PROPERTY_LABELS.assignee,
    PROPERTY_LABELS.labels,
    PROPERTY_LABELS.route,
    PROPERTY_LABELS.repository,
    PROPERTY_LABELS.advances,
  ]);
  expect(searches.length, "Add searched for no control: this case proves nothing").toBeGreaterThan(
    0
  );
  const details = document.querySelector(`aside[${DETAILS_ATTRIBUTE}]`);
  const header = document.querySelector("[data-issue-header]");
  expect(
    searches.filter(({ on }) => on !== details && on !== header).map(({ selector }) => selector),
    "Add searched for an entry's control beyond Details and the header"
  ).toEqual([]);
});
