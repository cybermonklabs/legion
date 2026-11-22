import { expect, spyOn, test } from "bun:test";
import { act, render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { type ReactNode, useState } from "react";

import { KeymapProvider } from "./KeymapProvider";
import { useDialog } from "./useDialog";

function Dialog({
  label,
  onClose,
  open,
  children,
}: {
  label: string;
  onClose: () => void;
  open: boolean;
  children?: ReactNode;
}) {
  const dialog = useDialog<HTMLDivElement>({ onClose, open });
  if (!open) return null;
  return (
    <div aria-label={label} ref={dialog.containerRef} role="dialog">
      <button type="button">{label}</button>
      {children}
    </div>
  );
}

test("Escape pressed outside an open dialog still closes it", async () => {
  const user = userEvent.setup();
  let closed = 0;
  const view = render(
    <KeymapProvider>
      <Dialog label="Sheet" onClose={() => (closed += 1)} open>
        <p>body</p>
      </Dialog>
    </KeymapProvider>
  );
  try {
    await user.keyboard("{Escape}");
    expect(closed).toBe(1);
  } finally {
    view.unmount();
  }
});

test("Escape closes only the innermost of two nested open dialogs", async () => {
  const user = userEvent.setup();
  const closed = { picker: 0, sheet: 0 };
  const view = render(
    <KeymapProvider>
      <Dialog label="Sheet" onClose={() => (closed.sheet += 1)} open>
        <Dialog label="Picker" onClose={() => (closed.picker += 1)} open />
      </Dialog>
    </KeymapProvider>
  );
  try {
    await user.keyboard("{Escape}");
    expect(closed).toEqual({ picker: 1, sheet: 0 });
    await user.click(view.getByRole("dialog", { name: "Picker" }));
    await user.keyboard("{Escape}");
    expect(closed).toEqual({ picker: 2, sheet: 0 });
  } finally {
    view.unmount();
  }
});

test("Escape closes the most recently opened of two sibling (non-nested) open dialogs", async () => {
  // Mirrors the phone nav sheet and the Inbox drawer its Peek button opens on top of it
  // (LEGION-547): neither contains the other, so containment alone can't say which is topmost.
  const user = userEvent.setup();
  const closed = { drawer: 0, nav: 0 };
  const view = render(
    <KeymapProvider>
      <Dialog label="Nav" onClose={() => (closed.nav += 1)} open />
      <Dialog label="Drawer" onClose={() => (closed.drawer += 1)} open />
    </KeymapProvider>
  );
  try {
    await user.keyboard("{Escape}");
    expect(closed).toEqual({ drawer: 1, nav: 0 });
  } finally {
    view.unmount();
  }
});

/** A control that opens a dialog over itself, as Approve's Shift+R opens Request changes over
 *  Approve, and counts its own presses. */
function ControlUnderADialog({ onPress }: { onPress: () => void }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button
        onClick={onPress}
        onKeyDown={(event) => {
          if (event.key === "R") setOpen(true);
        }}
        type="button"
      >
        Approve
      </button>
      <Dialog label="Request changes" onClose={() => setOpen(false)} open={open}>
        <textarea aria-label="Reason" />
      </Dialog>
    </>
  );
}

// The dialog's field takes the focus a frame after the dialog opens, and a loaded machine draws
// that frame late. A key typed before it reaches nothing behind the dialog: the dialog holds the
// focus itself from the commit that opens it. The frame is held here, so the field never takes it.
test("a key typed in a dialog's first frame presses nothing behind the dialog", async () => {
  const user = userEvent.setup();
  let pressed = 0;
  const view = render(
    <KeymapProvider>
      <ControlUnderADialog onPress={() => (pressed += 1)} />
    </KeymapProvider>
  );
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);
  try {
    act(() => view.getByRole("button", { name: "Approve" }).focus());
    await user.keyboard("R");
    const dialog = view.getByRole("dialog", { name: "Request changes" });
    await user.keyboard(" a{Enter}");
    expect(pressed, "a key typed in the dialog's first frame pressed the control under it").toBe(0);
    expect(document.activeElement, "the dialog left the focus behind it").toBe(dialog);
  } finally {
    frames.mockRestore();
    view.unmount();
  }
});

// The frame after it opens, the dialog gives the focus to its first control and is no longer
// focusable itself, so a press on its text later puts the focus where it did before.
test("the frame after it opens, a dialog gives the focus to its first control", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <ControlUnderADialog onPress={() => {}} />
    </KeymapProvider>
  );
  const held: FrameRequestCallback[] = [];
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    held.push(callback);
    return held.length;
  });
  try {
    act(() => view.getByRole("button", { name: "Approve" }).focus());
    await user.keyboard("R");
    const dialog = view.getByRole("dialog", { name: "Request changes" });
    expect(dialog.getAttribute("tabindex")).toBe("-1");
    act(() => {
      for (const callback of held.splice(0)) callback(performance.now());
    });
    expect(document.activeElement, "the frame gave the focus to no control of the dialog").toBe(
      view.getByRole("button", { name: "Request changes" })
    );
    expect(dialog.hasAttribute("tabindex"), "the dialog stayed focusable after its frame").toBe(
      false
    );
  } finally {
    frames.mockRestore();
    view.unmount();
  }
});
