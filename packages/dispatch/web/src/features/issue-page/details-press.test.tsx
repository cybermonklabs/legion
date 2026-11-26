import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import { type ReactNode, useMemo, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { GraphReferences, IssueDetails, Project, ReferenceTargets } from "../../api/types";
import { Margin } from "../margin/Margin";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { buildDispatchReference } from "../refs/routes";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import { type MarginDrawer, type MarginDrawerPage, MarginSlotProvider } from "./margin-slot";
import { TWO_THREADS } from "./outline-fixtures";
import { PROPERTY_LABELS } from "./page-structure";
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
});
afterEach(() => {
  cleanup();
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
