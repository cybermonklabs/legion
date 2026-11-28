import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import { type ReactNode, useMemo, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import { Margin } from "../margin/Margin";
import { PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import { type MarginDrawer, type MarginDrawerPage, MarginSlotProvider } from "./margin-slot";
import { TWO_THREADS } from "./outline-fixtures";
import { type TasksByHand, tasksByHand } from "./slices-fixtures";
import { AppProviders, commentNodes, stubReads, treeOf } from "./slots/stage-fixtures";

/**
 * A press on Details at the phone's width, where the drawer is a sheet over the page that holds
 * the focus (`margin/Margin.tsx`'s `DetailsDrawer`, `useDialog`). The press's commit opens the
 * sheet and moves no focus: the sheet's Close takes the focus in the frame after
 * (`useDialog`'s `focusInFrame`), so the press's task lays out no panel for a focus. Closing it
 * gives the focus back to the Details control.
 */

/** The drawer, open: the shell's `aside`, as the load rows read it (`e2e/issue-page-load.ts`). */
const DETAILS_OPEN = 'aside[data-testid="desktop-margin-shell"]:not([hidden])';

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;

beforeEach(() => {
  stubs = [
    ...stubReads(),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "getIssue").mockResolvedValue(issueDetails()),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === undefined ? treeOf(commentNodes(TWO_THREADS)) : treeOf([])
    ),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "getInbox").mockResolvedValue([]),
    // What waits on the issue, which Details reads as it opens, stays out: the case reads the
    // focus, and an answer would draw Details again after it ends.
    spyOn(api, "getReferences").mockImplementation((_reference, options) =>
      options?.kind === "blocked_by"
        ? new Promise(() => {})
        : Promise.resolve({ edges: [], node: graphNode({ id: KEY, kind: "issue" }) })
    ),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
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
 *  the drawer. */
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

/** Holds every animation frame asked for until `run`, as a loaded phone draws its next frame
 *  late. A cancelled frame does not run. */
function holdFrames(): { restore: () => void; run: () => void } {
  const held = new Map<number, FrameRequestCallback>();
  let next = 0;
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    next += 1;
    held.set(next, callback);
    return next;
  });
  const cancels = spyOn(window, "cancelAnimationFrame").mockImplementation((id) => {
    held.delete(id);
  });
  return {
    restore: () => {
      frames.mockRestore();
      cancels.mockRestore();
    },
    run: () =>
      act(() => {
        const callbacks = [...held.values()];
        held.clear();
        for (const callback of callbacks) callback(performance.now());
      }),
  };
}

/** The page's root at Conversation's address, beside the shell's drawer, at 390 px. */
function drawPage(): RenderResult {
  restoreViewport = stubViewport(PHONE_WIDTH);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <Shell>
          <IssueOutlinePage />
        </Shell>
      </AppProviders>
    </MemoryRouter>
  );
}

test("at the phone's width a press on Details opens the sheet in its commit and its Close takes the focus in the frame after", async () => {
  const view = drawPage();
  const control = await waitFor(() => {
    const found = view.baseElement.querySelector<HTMLElement>(
      '[data-testid="issue-details-control"]'
    );
    if (found === null) throw new Error("the header drew no Details control");
    return found;
  });
  await tasks.runAll();
  act(() => control.focus());
  const frames = holdFrames();
  try {
    act(() => control.click());
    const sheet = document.querySelector<HTMLElement>(DETAILS_OPEN);
    if (sheet === null) throw new Error("the press left the sheet shut in its commit");
    expect(document.activeElement, "the press's commit moved the focus into the sheet").toBe(
      control
    );
    frames.run();
    const close = sheet.querySelector<HTMLElement>('[data-testid="details-close"]');
    expect(document.activeElement, "the frame after the press focused no Close of the sheet").toBe(
      close
    );
    act(() => close?.click());
    frames.run();
    expect(document.querySelector(DETAILS_OPEN), "Close left the sheet open").toBeNull();
    expect(document.activeElement, "closing the sheet gave the focus back elsewhere").toBe(control);
  } finally {
    frames.restore();
  }
});
