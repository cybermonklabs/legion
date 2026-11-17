import { expect, test } from "bun:test";
import { act, render, waitFor } from "@testing-library/react";
import { type ReactNode, useEffect, useState } from "react";

import { CONTROL_LABELS } from "../page-words";
import { FailedReadLine } from "./failed-read-line";
import { ViewStage } from "./stage";

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
    <ViewStage view="spec">
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
    <ViewStage view="decisions">
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
    <ViewStage view="spec">
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
