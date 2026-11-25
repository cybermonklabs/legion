import { expect, spyOn, test } from "bun:test";
import { act, render, waitFor } from "@testing-library/react";
import { type ReactNode, useEffect, useLayoutEffect, useState } from "react";

import { api } from "../../../api/client";
import { issueDetails, KEY } from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import { CONTROL_LABELS } from "../page-words";
import { FailedReadLine } from "./failed-read-line";
import { SLOT_FILLED, VIEW_SLOTS, type ViewSlotProps, ViewStage } from "./stage";
import { drawAtBrowserEntry, drawnSlot } from "./stage-fixtures";

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
