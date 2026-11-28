import { expect, spyOn, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useRef } from "react";

import { useDetailsFocusKeeper } from "../margin/details-focus-keeper";
import { useFocusKeeper } from "./focus-keeper";

/**
 * The page's two focus keepers judge a reader's press by one rule (decision 404): the header's
 * (`useFocusKeeper`, part P) and Details' (`useDetailsFocusKeeper`, part M). What a press is and
 * when it is over or answered is that rule's to say, in `margin/details-focus-keeper.ts`
 * (`hearPresses`, `watchPresses`); where the focus goes after a loss no press of the reader's
 * caused is each keeper's own, so the cases here press.
 */

/** Where a keeper left the focus once the control that held it went. */
const HANDED_ON = "handed on inside its region";
const ON_THE_BODY = "left on the page's body";

/** The header as its keeper sees it: Save, which a write takes away, and Edit, its fallback. */
function Header({ save }: { save: boolean }): ReactNode {
  const region = useRef<HTMLDivElement>(null);
  useFocusKeeper(region, [({ root }) => root.querySelector<HTMLElement>("[data-fallback]")]);
  return (
    <div data-region="" ref={region}>
      {save ? <button type="button">Save</button> : null}
      <button data-fallback="" type="button">
        Edit
      </button>
    </div>
  );
}

/** Details as its keeper sees it: its heading, Save, which a write takes away, and Edit. */
function Details({ save }: { save: boolean }): ReactNode {
  const box = useRef<HTMLElement>(null);
  useDetailsFocusKeeper(box, true);
  return (
    <aside aria-label="Details" data-region="" ref={box}>
      <h2>Details</h2>
      {save ? <button type="button">Save</button> : null}
      <button type="button">Edit</button>
    </aside>
  );
}

/** happy-dom lays nothing out, and Details' keeper hands the focus only to a control that is
 *  drawn and in sight: every element on the page and under no `hidden` one is drawn, 300 by 24
 *  at the window's top. Animation frames are held, so the commit a press made has not drawn by
 *  the time the write it started takes the control. Returns what lets both go. */
function drawnWithFramesHeld(): () => void {
  const rectOf = (element: Element): DOMRect | null =>
    element.isConnected && element.closest("[hidden]") === null ? new DOMRect(0, 0, 300, 24) : null;
  const bounds = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    return rectOf(this) ?? new DOMRect();
  });
  const rects = spyOn(Element.prototype, "getClientRects").mockImplementation(function (
    this: Element
  ) {
    const rect = rectOf(this);
    return (rect === null ? [] : [rect]) as unknown as DOMRectList;
  });
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);
  const cancel = spyOn(window, "cancelAnimationFrame").mockImplementation(() => undefined);
  return () => {
    cancel.mockRestore();
    frames.mockRestore();
    rects.mockRestore();
    bounds.mockRestore();
  };
}

/** Waits a task, by which each keeper's move, a task after the loss it read, has run. */
async function aTask(): Promise<void> {
  await act(async () => {
    const { promise, resolve } = Promise.withResolvers<void>();
    setTimeout(resolve, 0);
    await promise;
  });
}

/** Where `Keeper` leaves the focus when, with the focus on Save, the reader makes `press` on it
 *  and a write then takes Save away. */
async function verdictOf(
  Keeper: (props: { save: boolean }) => ReactNode,
  press: (save: HTMLElement) => Promise<void>
): Promise<string> {
  const restore = drawnWithFramesHeld();
  const view = render(<Keeper save />);
  try {
    const save = screen.getByRole("button", { name: "Save" });
    act(() => save.focus());
    await press(save);
    view.rerender(<Keeper save={false} />);
    // The keeper reads the loss in a microtask or an observer's callback, and moves a task later.
    await aTask();
    await aTask();
    const focus = document.activeElement;
    if (focus === null || focus === document.body) return ON_THE_BODY;
    const region = view.container.querySelector("[data-region]");
    return region?.contains(focus) === true ? HANDED_ON : `moved to ${focus.outerHTML}`;
  } finally {
    view.unmount();
    restore();
  }
}

// The same press, then the same removal of the control it was made on, through each keeper: a
// press still under way, its pointer down - the write a press starts can take the control before
// its click - and a press that is over at its click, whose act the removal answers before its
// commit has drawn. Each is the reader's press while it stands, and each keeper hands the focus
// on inside its region.
test("the header and Details judge the same press alike", async () => {
  const presses: readonly (readonly [string, (save: HTMLElement) => Promise<void>])[] = [
    [
      "a press under way, its pointer down on Save",
      async (save) => {
        await userEvent.setup().pointer({ keys: "[MouseLeft>]", target: save });
      },
    ],
    [
      "a press of Save that is over at its click",
      async (save) => {
        await userEvent.setup().click(save);
      },
    ],
  ];
  for (const [step, press] of presses) {
    expect(
      { details: await verdictOf(Details, press), header: await verdictOf(Header, press) },
      step
    ).toEqual({ details: HANDED_ON, header: HANDED_ON });
  }
});
