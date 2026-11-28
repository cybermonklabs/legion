import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useLayoutEffect } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { IssuePresets } from "./IssuePresets";
import { presetPath } from "./issue-address";
import { ViewPressProvider } from "./view-press";

/**
 * On the page (`ViewPressProvider`), a view chosen in the one control's menu is drawn in the
 * commit that closes the menu, and where that drawing takes the focus off the control - the view
 * left taken off the page, the view chosen focusing its row - the control takes it back once the
 * frame is painted (`IssuePresets.tsx`), not in that commit, so the press's task lays out no view
 * for a focus. Outside the page the control takes it back in that commit, as
 * `IssuePresets.test.tsx` reads it.
 */

/** Holds every animation frame asked for until `run`. */
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

/** What the stage does as a view chosen is drawn: the focus leaves the control, here for the
 *  page's body, in the layout effect of the commit that moves the address. */
function DrawingTakesTheFocus(): ReactNode {
  const { pathname } = useLocation();
  useLayoutEffect(() => {
    if (pathname === presetPath("CORE-1", "spec")) {
      if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    }
  }, [pathname]);
  return null;
}

test("on the page a view chosen in the menu gives the focus back to the control once the frame is painted", async () => {
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const user = userEvent.setup();
  const client = new QueryClient();
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[presetPath("CORE-1", "conversation")]}>
        <ViewPressProvider>
          <IssuePresets active="conversation" issueKey="CORE-1" />
          <DrawingTakesTheFocus />
        </ViewPressProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  const chooser = view.getByRole("button", { name: "Conversation" });
  let frames: ReturnType<typeof holdFrames> | undefined;
  try {
    await user.click(chooser);
    const menu = view.getByRole("menu");
    frames = holdFrames();
    act(() => within(menu).getByRole("menuitemradio", { name: "Spec" }).click());
    expect(view.queryByRole("menu"), "the choice left the menu open").toBeNull();
    expect(
      document.activeElement,
      "the commit that drew the view chosen gave the focus back to the control"
    ).toBe(document.body);
    frames.run();
    // The frame's callback queued a task on the window's timers (`afterPaint`): one more runs it.
    await act(async () => {
      const painted = Promise.withResolvers<void>();
      window.setTimeout(painted.resolve, 0);
      await painted.promise;
    });
    expect(document.activeElement, "the painted frame gave the control no focus").toBe(chooser);
  } finally {
    frames?.restore();
    view.unmount();
    restoreViewport();
  }
});
