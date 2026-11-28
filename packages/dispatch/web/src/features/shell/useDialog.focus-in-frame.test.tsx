import { expect, spyOn, test } from "bun:test";
import { act, render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { useRef, useState } from "react";

import { KeymapProvider } from "./KeymapProvider";
import { useDialog } from "./useDialog";

/**
 * `useDialog`'s `focusInFrame`: a dialog given it takes no focus in the commit that opens it, and
 * its field takes the focus in the frame after, so that commit lays nothing out for a focus. The
 * new issue page's Details passes it at the phone's width (`margin/Margin.tsx`'s `DetailsDrawer`),
 * where its sheet's focus laid out its whole panel inside the press's task. A dialog given no
 * option takes the focus at once, as before (`useDialog.test.tsx`).
 */

/** Holds every animation frame the hook asks for until `run`, as a loaded machine draws its next
 *  frame late. A cancelled frame does not run. */
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

/** A control that opens a dialog with a Close, which the dialog's first focus goes to. */
function Opener({ focusInFrame }: { focusInFrame?: boolean }) {
  const [open, setOpen] = useState(false);
  const close = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLDivElement>({
    initialFocusRef: close,
    onClose: () => setOpen(false),
    open,
    ...(focusInFrame === undefined ? {} : { focusInFrame }),
  });
  return (
    <>
      <button onClick={() => setOpen(true)} type="button">
        Details
      </button>
      {open ? (
        <div aria-label="Sheet" ref={dialog.containerRef} role="dialog">
          <p>Words of the sheet</p>
          <button onClick={() => setOpen(false)} ref={close} type="button">
            Close
          </button>
        </div>
      ) : null}
    </>
  );
}

test("a dialog given focusInFrame leaves the focus in the commit that opens it and puts it on its field in the frame after", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Opener focusInFrame />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    const details = view.getByRole("button", { name: "Details" });
    await user.click(details);
    const sheet = view.getByRole("dialog", { name: "Sheet" });
    expect(document.activeElement, "the commit that opened the dialog moved the focus").toBe(
      details
    );
    expect(sheet, "the commit that opened the dialog made it focusable").not.toHaveAttribute(
      "tabindex"
    );
    frames.run();
    expect(document.activeElement, "the frame after gave the focus to no field of it").toBe(
      view.getByRole("button", { name: "Close" })
    );
    await user.keyboard("{Escape}");
    expect(view.queryByRole("dialog", { name: "Sheet" }), "Escape left the dialog open").toBeNull();
    expect(document.activeElement, "closing gave the focus back to nothing it came from").toBe(
      details
    );
  } finally {
    frames.restore();
    view.unmount();
  }
});

test("a dialog given no option takes the focus itself in the commit that opens it", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Opener />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    await user.click(view.getByRole("button", { name: "Details" }));
    expect(document.activeElement, "the dialog left the focus behind it as it opened").toBe(
      view.getByRole("dialog", { name: "Sheet" })
    );
    frames.run();
    expect(document.activeElement).toBe(view.getByRole("button", { name: "Close" }));
  } finally {
    frames.restore();
    view.unmount();
  }
});
