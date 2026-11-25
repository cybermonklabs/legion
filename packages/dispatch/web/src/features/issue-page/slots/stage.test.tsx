import { afterEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useEffect, useLayoutEffect, useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../../api/client";
import type { Comment, IssueDetails } from "../../../api/types";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../../margin/margin-fixture";
import { parseIssuePath } from "../../refs/routes";
import { IssueContentsPane } from "../contents-frame";
import { issueDetails, KEY } from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import { readComment, TWO_THREADS } from "../outline-fixtures";
import { CONTROL_LABELS, PRESET_NAMES, PRESET_ORDER, type Preset } from "../page-words";
import { readOlderComments, type TreeAnswer } from "../tree-read/tree-read";
import { FailedReadLine } from "./failed-read-line";
import { ProvidersSlot } from "./providers";
import { SLOT_FILLED, StageSlot, VIEW_SLOTS, type ViewSlotProps, ViewStage } from "./stage";
import {
  AppProviders,
  commentNodes,
  drawAtBrowserEntry,
  drawnSlot,
  stubReads,
  treeOf,
} from "./stage-fixtures";
import { ViewRowSlot } from "./view-row";

/**
 * Where the focus lands when the page opens at a view's address, which the tester found landing
 * on the page's body: on the tab-stop row of the view's `role="tree"`, never on the body.
 *
 * A view's own slot is filled by that view's own code, so the view here is a stand-in that
 * draws a tree of two rows the way the outline does: one row with `tabindex="0"`, and drawn a
 * moment after it mounts, as a view draws its tree once its reads have answered.
 */
function StandInView(): ReactNode {
  const [read, setRead] = useState(false);
  useEffect(() => {
    const answered = window.setTimeout(() => setRead(true), 20);
    return () => window.clearTimeout(answered);
  }, []);
  if (!read) return null;
  return (
    <div aria-label={CONTROL_LABELS.tree} role="tree">
      <div aria-level={1} data-node-row="" role="treeitem" tabIndex={-1}>
        The first statement.
      </div>
      <div aria-level={1} data-node-row="" role="treeitem" tabIndex={0}>
        The second statement.
      </div>
    </div>
  );
}

test("a view's address puts the focus on its tree's tab-stop row", async () => {
  (document.activeElement as HTMLElement | null)?.blur();
  const view = render(
    <ViewStage>
      <StandInView />
    </ViewStage>
  );
  try {
    const row = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>('[role="treeitem"][tabindex="0"]');
      if (found === null) throw new Error("the view drew no tab-stop row");
      return found;
    });
    await waitFor(() => expect(document.activeElement).toBe(row));
  } finally {
    view.unmount();
  }
});

test("a view leaves the focus where a control already holds it", async () => {
  const control = document.createElement("button");
  document.body.append(control);
  await act(async () => control.focus());
  const view = render(
    <ViewStage>
      <StandInView />
    </ViewStage>
  );
  try {
    await waitFor(() =>
      expect(view.container.querySelector('[role="treeitem"][tabindex="0"]')).not.toBeNull()
    );
    expect(document.activeElement).toBe(control);
  } finally {
    view.unmount();
    control.remove();
  }
});

/** A view whose read is refused from its first load: a moment after it mounts it draws the
 *  refused read's line (`FailedReadLine`), with its Retry, and no tree. */
function RefusedView(): ReactNode {
  const [refused, setRefused] = useState(false);
  useEffect(() => {
    const answered = window.setTimeout(() => setRefused(true), 20);
    return () => window.clearTimeout(answered);
  }, []);
  if (!refused) return null;
  return (
    <FailedReadLine message="Couldn't read the view." onRetry={() => undefined} retrying={false} />
  );
}

test("a view whose read is refused puts the focus on its Retry", async () => {
  (document.activeElement as HTMLElement | null)?.blur();
  const view = render(
    <ViewStage>
      <RefusedView />
    </ViewStage>
  );
  try {
    const retry = await view.findByRole("button", { name: "Retry" });
    await waitFor(() =>
      expect(document.activeElement, "the refused view left the focus on the page's body").toBe(
        retry
      )
    );
  } finally {
    view.unmount();
  }
});

/** What the stand-in view says of the address's node on each render (`drawn`), and the call the
 *  stage last handed it (`tell`), which the case calls itself as a later word of the view. */
const saying: { drawn: boolean; tell?: (drawn: boolean) => void } = { drawn: false };

/** A stand-in for a view whose reads have settled: it says `saying.drawn` to the stage from an
 *  effect on every render, as `ViewSlotProps.onAddressDrawn` lets a view, and draws its mark. */
function SayingView({ onAddressDrawn }: ViewSlotProps): ReactNode {
  saying.tell = onAddressDrawn;
  useLayoutEffect(() => onAddressDrawn(saying.drawn));
  return <div data-stand-in-view="" />;
}

// The stage takes the first thing a view says for each following of an address beside it
// (`ViewSlotProps.onAddressDrawn`), so a view may say it on every render. The body's reads never
// answer here: the case reads which slot the stage draws, not what the body draws.
test("the stage takes the first thing a view says for each following of an address beside it, and an address not beside the view keeps the view whatever the view says", () => {
  const reads = (
    [
      "getArtifactBlocks",
      "getIssueCoverage",
      "getIssueDecisions",
      "getIssueReferences",
      "getIssueResults",
      "getReferenceTargets",
      "listComments",
      "listIssueAsks",
      "listIssueAskThreads",
      "listIssueMessages",
      "whoAmI",
    ] as const
  ).map((read) => spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise));
  const decisions = presetPath(KEY, "decisions");
  try {
    // The view says first that it draws no node: the body is drawn in its place, and a later
    // word in that following that it does draw the node never undoes that first word.
    saying.drawn = false;
    const stage = drawAtBrowserEntry(`${decisions}?comment=reply-b`, issueDetails(), {
      filled: { ...SLOT_FILLED, decisions: true },
      views: { ...VIEW_SLOTS, decisions: SayingView },
    });
    expect(drawnSlot(stage), "the view said it draws no node").toBe("the body");
    act(() => saying.tell?.(true));
    expect(drawnSlot(stage), "a later word that the view draws the node undid its first").toBe(
      "the body"
    );
    // A new following asks the view again: it says first that it draws the node, and the view
    // stays when its call says later in that following that it does not.
    saying.drawn = true;
    stage.follow(`${decisions}?comment=head-b`);
    expect(drawnSlot(stage), "a new following did not ask the view again").toBe("the view");
    act(() => saying.tell?.(false));
    expect(drawnSlot(stage), "a later word in the same following drew the body").toBe("the view");
    // An address not beside the view keeps the view whatever the view says, here that it draws no
    // node.
    saying.drawn = false;
    stage.follow(decisions);
    expect(drawnSlot(stage), "an address not beside the view drew the body in its place").toBe(
      "the view"
    );
    stage.unmount();
  } finally {
    for (const read of reads) read.mockRestore();
  }
});

/** A stand-in for a view's own slot, which draws its mark and nothing else: no first group of
 *  the issue's three sentences, which the Spec view's own slot draws. */
function MarkView(): ReactNode {
  return <div data-stand-in-view="" />;
}

// R13: below 1024 px the stage draws the issue's three sentences above the slot it chooses, in
// Conversation, Verification, Plan, Decisions and Graph, and none over the Spec view's own slot,
// whose first group draws them; where it draws the outline in the Spec view's place, the outline
// has no such group, and the sentences stand above it. From 1024 px up it draws none: the Contents
// pane holds them. Each view here is a stand-in that draws no sentences of its own.
test("below 1024 px the stage draws the three sentences above the slot it chooses, but over the Spec view's own", () => {
  const reads = (
    [
      "getIssueCoverage",
      "getIssueDecisions",
      "getIssueReferences",
      "getIssueResults",
      "getIssueTree",
      "getReferenceTargets",
      "listComments",
      "listIssueAsks",
      "listIssueAskThreads",
      "listIssueMessages",
      "whoAmI",
    ] as const
  ).map((read) => spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise));
  const standIns: typeof VIEW_SLOTS = {
    ...VIEW_SLOTS,
    decisions: MarkView,
    graph: MarkView,
    plan: MarkView,
    spec: MarkView,
    verification: MarkView,
  };
  const sentences = '[data-testid="issue-three-things"]';
  const slot = "[data-issue-body], [data-stand-in-view]";
  try {
    for (const width of [PHONE_WIDTH, DESKTOP_WIDTH]) {
      const restoreMatchMedia = stubViewport(width);
      try {
        for (const preset of PRESET_ORDER) {
          const where = `on ${presetPath(KEY, preset)} at ${width} px`;
          const stage = drawAtBrowserEntry(presetPath(KEY, preset), issueDetails(), {
            filled: SLOT_FILLED,
            views: standIns,
          });
          const drawn = [...stage.container.querySelectorAll(sentences)];
          const above = width === PHONE_WIDTH && preset !== "spec";
          expect(drawn.length, `the stage's sentences ${where}`).toBe(above ? 1 : 0);
          if (above) {
            const after = stage.container.querySelector(slot) as Element;
            expect(
              (drawn[0] as Element).compareDocumentPosition(after) &
                Node.DOCUMENT_POSITION_FOLLOWING,
              `the sentences do not stand above the slot ${where}`
            ).toBeTruthy();
          }
          stage.unmount();
        }
        // The Spec view says it draws no node the address names: the stage draws the outline in
        // its place, and the sentences above it below 1024 px.
        saying.drawn = false;
        const spec = presetPath(KEY, "spec");
        const swapped = drawAtBrowserEntry(`${spec}?comment=reply-b`, issueDetails(), {
          filled: SLOT_FILLED,
          views: { ...standIns, spec: SayingView },
        });
        expect(drawnSlot(swapped), "the view said it draws no node").toBe("the body");
        expect(
          swapped.container.querySelectorAll(sentences).length,
          `the stage's sentences over the outline in the Spec view's place at ${width} px`
        ).toBe(width === PHONE_WIDTH ? 1 : 0);
        swapped.unmount();
      } finally {
        restoreMatchMedia();
      }
    }
  } finally {
    for (const read of reads) read.mockRestore();
  }
});

// The Contents pane and the Outline sheet beside each view that draws an outline read (R13), with
// the stage they stand beside, as the page draws them: from 1024 px up the pane stands at the left
// in Conversation, in each filter and in Graph, with the issue's three sentences and the Contents
// list; below 1024 px the Outline control opens the same list as a sheet, and each view draws the
// three sentences above its first row, so they are drawn once at every width. The list reads the
// outline read the page holds, and its foot reads the next page of older comments, the same page
// the control above the outline reads (R15).

/** The slots of the page that draw the Contents list and the three sentences, as the page's root
 *  draws them (`IssueOutlinePage.tsx`): the view row, the Contents pane beside the stage, and the
 *  stage with its real views, the route read from the address at every render. */
function Frame({ issue }: { issue: IssueDetails }): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  return (
    <ProvidersSlot issue={issue} route={route}>
      <ViewRowSlot issue={issue} route={route} />
      <IssueContentsPane issue={issue} route={route} />
      <StageSlot issue={issue} route={route} />
    </ProvidersSlot>
  );
}

/** The frame (`Frame`) of the fixtures' issue at `preset`'s own address, under what the app
 *  provides above the page. */
function drawFrame(client: QueryClient, preset: Preset): RenderResult {
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, preset)]}>
      <AppProviders client={client}>
        <Frame issue={issueDetails()} />
      </AppProviders>
    </MemoryRouter>
  );
}

/** A client as the page's tests take one: no read tried again. */
function newClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

/** The mark of each slot the stage draws: the body, which draws the outline in Conversation and
 *  Graph, and each filter's own body. */
const SLOT_MARKS =
  "[data-issue-body], [data-spec-body], [data-verification-body], [data-plan-body], [data-decisions-body]";

/** The Contents pane, where it is drawn. */
function pane(): HTMLElement | null {
  return document.querySelector<HTMLElement>('[data-testid="issue-contents"]');
}

/** The Outline sheet, where it is open. */
function sheet(): HTMLElement | null {
  return document.querySelector<HTMLElement>('[data-testid="issue-outline-sheet"]');
}

/** Every place the issue's three sentences are drawn, on the whole page: the sheet stands in the
 *  document's body, outside what the case drew. */
function drawnSentences(): HTMLElement[] {
  return [...document.querySelectorAll<HTMLElement>('[data-testid="issue-three-things"]')];
}

/** The Contents list inside `within`, where it draws one. */
function contentsList(within: HTMLElement | null): HTMLElement | null {
  return (
    within?.querySelector<HTMLElement>(`[role="tree"][aria-label="${CONTROL_LABELS.contents}"]`) ??
    null
  );
}

/** The Outline control the view row draws below 1024 px, where it draws one. */
function outlineControl(view: RenderResult): HTMLElement | null {
  return (
    [...view.container.querySelectorAll<HTMLElement>("button")].find(
      (button) => button.textContent === CONTROL_LABELS.outline
    ) ?? null
  );
}

/** How many tree reads with `kind` - Conversation's read - were sent, older comment pages apart. */
function conversationReadsSent(): number {
  return (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls.filter(([, query]) => {
    const asked = query as { readonly comments_before?: string; readonly kind?: unknown };
    return asked?.kind !== undefined && asked.comments_before === undefined;
  }).length;
}

let restore: { mockRestore(): void }[] = [];
let restoreViewport: (() => void) | undefined;
afterEach(() => {
  for (const spy of restore) spy.mockRestore();
  restore = [];
  restoreViewport?.();
  restoreViewport = undefined;
});

/** Every read of the page answered, the outline reads with the issue's two threads, which the
 *  reader began, so the Contents list holds an entry for each. */
function answerReads(): void {
  restore = stubReads();
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeOf(commentNodes(TWO_THREADS))
  );
}

// R13: from 1024 px up the pane stands at the left in Conversation, in each filter and in Graph,
// with the three sentences and the Contents list, and the sentences are drawn there alone: the
// Spec view, which draws them as its first group below 1024 px, draws no such group beside the
// pane, and no other view draws them above its first row.
test("from 1024 px up the Contents pane stands beside Conversation, each filter and Graph, and draws the three sentences once", async () => {
  restoreViewport = stubViewport(DESKTOP_WIDTH);
  answerReads();
  for (const preset of PRESET_ORDER) {
    const where = `with ${PRESET_NAMES[preset]} on at 1440 px`;
    const client = newClient();
    const view = drawFrame(client, preset);
    try {
      await waitFor(() =>
        expect(view.container.querySelector(SLOT_MARKS), `no slot is drawn ${where}`).not.toBeNull()
      );
      expect(pane(), `no Contents pane is drawn ${where}`).not.toBeNull();
      await waitFor(() =>
        expect(
          contentsList(pane())?.querySelectorAll("[data-contents-entry]").length,
          `the pane's list holds not the reader's two threads ${where}`
        ).toBe(2)
      );
      const drawn = drawnSentences();
      expect(drawn.length, `the three sentences are not drawn once ${where}`).toBe(1);
      expect(
        pane()?.contains(drawn[0] ?? null),
        `the sentences stand outside the pane ${where}`
      ).toBe(true);
      expect(outlineControl(view), `an Outline control is drawn ${where}`).toBeNull();
    } finally {
      view.unmount();
      client.clear();
    }
  }
});

// R13: below 1024 px no pane is drawn; each of those views draws the three sentences above its
// first row - the Spec view as its own first group, every other above the slot the stage draws -
// and the Outline control opens the Contents list as a sheet, which draws the list and not the
// sentences, so they are drawn once.
test("below 1024 px each view draws the three sentences once above its first row, and the Outline sheet draws the list without them", async () => {
  restoreViewport = stubViewport(PHONE_WIDTH);
  answerReads();
  const user = userEvent.setup();
  for (const preset of PRESET_ORDER) {
    const where = `with ${PRESET_NAMES[preset]} on at 390 px`;
    const client = newClient();
    const view = drawFrame(client, preset);
    try {
      const slot = await waitFor(() => {
        const drawn = view.container.querySelector(SLOT_MARKS);
        expect(drawn, `no slot is drawn ${where}`).not.toBeNull();
        return drawn as HTMLElement;
      });
      expect(pane(), `a Contents pane is drawn ${where}`).toBeNull();
      const drawn = drawnSentences();
      expect(drawn.length, `the three sentences are not drawn once ${where}`).toBe(1);
      const head = drawn[0] as HTMLElement;
      if (preset === "spec") {
        expect(slot.contains(head), "the Spec view does not draw them as its first group").toBe(
          true
        );
      } else {
        expect(
          head.compareDocumentPosition(slot) & Node.DOCUMENT_POSITION_FOLLOWING,
          `the sentences do not stand above the slot ${where}`
        ).toBeTruthy();
      }

      const control = outlineControl(view);
      expect(control, `no Outline control is drawn ${where}`).not.toBeNull();
      await user.click(control as HTMLElement);
      await waitFor(() =>
        expect(
          contentsList(sheet())?.querySelectorAll("[data-contents-entry]").length,
          `the Outline sheet's list holds not the reader's two threads ${where}`
        ).toBe(2)
      );
      expect(
        sheet()?.querySelector('[data-testid="issue-three-things"]'),
        `the Outline sheet draws the sentences ${where}, which the view draws above its first row`
      ).toBeNull();
      expect(drawnSentences().length, `the three sentences are not drawn once ${where}`).toBe(1);
    } finally {
      view.unmount();
      client.clear();
    }
  }
});

// R13: the list reads the outline read the page holds and no read of its own. On Spec loaded cold
// below 1024 px, Conversation's read is sent as the Outline sheet opens, and not before, and once
// however often it opens; beside Verification, Plan and Decisions it is sent with the load; in
// Graph the list reads Graph's read, and the sheet sends no Conversation's read.
test("below 1024 px the Outline sheet sends Conversation's read on Spec as it opens, and none where the page holds it or in Graph", async () => {
  restoreViewport = stubViewport(PHONE_WIDTH);
  const user = userEvent.setup();
  const loads: readonly { readonly preset: Preset; readonly withLoad: number }[] = [
    { preset: "spec", withLoad: 0 },
    { preset: "verification", withLoad: 1 },
    { preset: "plan", withLoad: 1 },
    { preset: "decisions", withLoad: 1 },
    { preset: "graph", withLoad: 0 },
  ];
  for (const { preset, withLoad } of loads) {
    const where = `with ${PRESET_NAMES[preset]} loaded cold at 390 px`;
    answerReads();
    const client = newClient();
    const view = drawFrame(client, preset);
    try {
      await waitFor(() => expect(view.container.querySelector(SLOT_MARKS)).not.toBeNull());
      await act(async () => {
        await Bun.sleep(200);
      });
      expect(conversationReadsSent(), `Conversation's reads sent with the load ${where}`).toBe(
        withLoad
      );
      const control = outlineControl(view) as HTMLElement;
      await user.click(control);
      await waitFor(() => expect(contentsList(sheet())).not.toBeNull());
      // Closed and opened again, the sheet finds the read held and sends nothing more.
      await user.click(
        [...(sheet()?.querySelectorAll("button") ?? [])].find(
          (button) => button.textContent === CONTROL_LABELS.close
        ) as HTMLElement
      );
      await user.click(control);
      await waitFor(() => expect(contentsList(sheet())).not.toBeNull());
      await act(async () => {
        await Bun.sleep(200);
      });
      expect(
        conversationReadsSent(),
        `Conversation's reads sent once the Outline sheet had opened twice ${where}`
      ).toBe(preset === "graph" ? 0 : 1);
    } finally {
      view.unmount();
      client.clear();
      for (const spy of restore) spy.mockRestore();
      restore = [];
    }
  }
});

// R15: the list holds the entries of the comment pages its read has loaded, and while older
// comments remain, a control at its foot reads the next page. That page is the one the control
// above the outline reads: pressed twice while it is out, and asked by the outline's control
// beside it, it is read once. A press takes no focus, since the control is gone once no older
// comment remains.
test("the Contents list's foot reads the next older comment page once, with the outline's control, and keeps the focus where it was", async () => {
  restoreViewport = stubViewport(DESKTOP_WIDTH);
  answerReads();
  const older: Comment = readComment("head-older", 0);
  const first: TreeAnswer = {
    ...treeOf(commentNodes(TWO_THREADS)),
    comments_before: "2026-11-05T01:00:00.000000Z,head-a",
  };
  const page: TreeAnswer = { ...treeOf(commentNodes([older])), comments_before: null };
  const out = Promise.withResolvers<TreeAnswer>();
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) =>
    query?.comments_before === undefined ? first : out.promise
  );
  const pagesSent = () =>
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls.filter(
      ([, query]) => query?.comments_before !== undefined
    ).length;
  const user = userEvent.setup();
  const elsewhere = document.createElement("button");
  document.body.append(elsewhere);
  const client = newClient();
  const view = drawFrame(client, "conversation");
  try {
    const entries = () =>
      [...(contentsList(pane())?.querySelectorAll<HTMLElement>("[data-contents-entry]") ?? [])].map(
        (entry) => entry.dataset.contentsEntry
      );
    await waitFor(() => expect(entries()).toEqual(["head-a", "head-b"]));
    const foot = await waitFor(() => {
      const found = pane()?.querySelector<HTMLElement>("[data-contents-older]");
      expect(found, "no control at the list's foot while older comments remain").not.toBeNull();
      return found as HTMLElement;
    });
    expect(foot.textContent).toBe(CONTROL_LABELS.contents_older);
    act(() => elsewhere.focus());
    await user.click(foot);
    expect(document.activeElement, "the press took the focus").toBe(elsewhere);
    await user.click(foot);
    await act(async () => {
      // The outline's own control reads the same page (`useOlderComments`).
      void readOlderComments(client, KEY, "conversation");
    });
    expect(pagesSent(), "the older page was read more than once while it was out").toBe(1);
    await act(async () => out.resolve(page));
    await waitFor(() => expect(entries()).toEqual(["head-older", "head-a", "head-b"]));
    expect(pagesSent(), "the older page was read more than once").toBe(1);
    expect(
      pane()?.querySelector("[data-contents-older]"),
      "the foot's control stayed once no older comment remained"
    ).toBeNull();
    expect(document.activeElement, "the focus left where it was").toBe(elsewhere);
  } finally {
    view.unmount();
    client.clear();
    elsewhere.remove();
  }
});
