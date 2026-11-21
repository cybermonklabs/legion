import { expect, jest, test } from "bun:test";
import { act, render } from "@testing-library/react";
import { type ReactNode, useEffect, useState } from "react";

import { jump, realClock } from "../../../__tests__/fake-clock";
import { CONTROL_LABELS } from "../page-words";
import { FailedReadLine } from "./failed-read-line";
import { ViewStage } from "./stage";

/** How long after it mounts a stand-in view draws what its reads answered. Each row runs on the
 *  fake clock and moves it over this wait. */
const ANSWERED_AFTER_MS = 20;

/**
 * Where the focus lands when the page opens at a view's address, which the tester found landing
 * on the page's body: on the tab-stop row of the view's `role="tree"`, never on the body.
 *
 * A view's own slot is filled by that view's own code, so the view here is a stand-in that
 * draws a tree of two rows the way the outline does: one row with `tabindex="0"`, and drawn
 * `ANSWERED_AFTER_MS` after it mounts, as a view draws its tree once its reads have answered.
 */
function StandInView(): ReactNode {
  const [read, setRead] = useState(false);
  useEffect(() => {
    const answered = window.setTimeout(() => setRead(true), ANSWERED_AFTER_MS);
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
  jest.useFakeTimers();
  const view = render(
    <ViewStage>
      <StandInView />
    </ViewStage>
  );
  try {
    await jump(ANSWERED_AFTER_MS);
    const row = view.container.querySelector<HTMLElement>('[role="treeitem"][tabindex="0"]');
    expect(row, "the view drew no tab-stop row").not.toBeNull();
    expect(row).toHaveFocus();
  } finally {
    view.unmount();
    realClock();
  }
});

test("a view leaves the focus where a control already holds it", async () => {
  const control = document.createElement("button");
  document.body.append(control);
  await act(async () => control.focus());
  jest.useFakeTimers();
  const view = render(
    <ViewStage>
      <StandInView />
    </ViewStage>
  );
  try {
    await jump(ANSWERED_AFTER_MS);
    expect(
      view.container.querySelector('[role="treeitem"][tabindex="0"]'),
      "the view drew no tab-stop row"
    ).not.toBeNull();
    expect(control).toHaveFocus();
  } finally {
    view.unmount();
    realClock();
    control.remove();
  }
});

/** A view whose read is refused from its first load: `ANSWERED_AFTER_MS` after it mounts it
 *  draws the refused read's line (`FailedReadLine`), with its Retry, and no tree. */
function RefusedView(): ReactNode {
  const [refused, setRefused] = useState(false);
  useEffect(() => {
    const answered = window.setTimeout(() => setRefused(true), ANSWERED_AFTER_MS);
    return () => window.clearTimeout(answered);
  }, []);
  if (!refused) return null;
  return (
    <FailedReadLine message="Couldn't read the view." onRetry={() => undefined} retrying={false} />
  );
}

test("a view whose read is refused puts the focus on its Retry", async () => {
  (document.activeElement as HTMLElement | null)?.blur();
  jest.useFakeTimers();
  const view = render(
    <ViewStage>
      <RefusedView />
    </ViewStage>
  );
  try {
    await jump(ANSWERED_AFTER_MS);
    expect(
      view.getByRole("button", { name: "Retry" }),
      "the refused view left the focus on the page's body"
    ).toHaveFocus();
  } finally {
    view.unmount();
    realClock();
  }
});
