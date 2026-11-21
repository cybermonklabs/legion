import { expect, spyOn, test } from "bun:test";
import { act, render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { type ReactNode, useEffect, useRef, useState } from "react";

import { KeymapProvider } from "./KeymapProvider";
import { useKeymap } from "./keymap";
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
    // The dialog takes the focus in the commit that opens it, and user-event sends its key to
    // whatever holds the focus, so the focus the dialog took is given up here: the key has to
    // reach the dialog from outside it, as one does after a press on the dialog's own text has
    // given the focus to the page.
    act(() => (document.activeElement as HTMLElement | null)?.blur());
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

/** The issue header's Approve, with Request changes over it that Shift+R opens, its focus to start
 *  on the reason field below its Cancel. */
function ApproveOrRequestChanges({ onApprove }: { onApprove: () => void }): ReactNode {
  const [open, setOpen] = useState(false);
  const reason = useRef<HTMLTextAreaElement>(null);
  useKeymap("global", [
    { id: "request-changes", keys: "Shift+R", label: "Request changes", run: () => setOpen(true) },
  ]);
  const dialog = useDialog<HTMLDivElement>({
    initialFocusRef: reason,
    onClose: () => setOpen(false),
    open,
  });
  return (
    <>
      <button onClick={onApprove} type="button">
        Approve
      </button>
      {open ? (
        <div aria-label="Request changes" ref={dialog.containerRef} role="dialog">
          <button type="button">Cancel</button>
          <textarea aria-label="What should change" ref={reason} />
        </div>
      ) : null}
    </>
  );
}

// The dialog takes the focus in the commit that opens it, so the keys typed straight after the
// press that opened it are its own. A key that still reached the opener pressed it: in CI a space
// typed into Request changes, 32 ms after Shift+R on Approve had opened it, pressed Approve and
// approved the request instead.
test("keys typed straight after a dialog opens land in its initial focus and none in its opener", async () => {
  // No wait between two keys, so Shift+R and the keys after it run in one task; and no frame
  // runs, so nothing a frame would do has happened by the time they are typed.
  const user = userEvent.setup({ delay: null });
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);
  let approvals = 0;
  const view = render(
    <KeymapProvider>
      <ApproveOrRequestChanges onApprove={() => (approvals += 1)} />
    </KeymapProvider>
  );
  try {
    const approve = view.getByRole("button", { name: "Approve" });
    act(() => approve.focus());
    await user.keyboard("{Shift>}R{/Shift}One change.");

    expect(approvals, "a key typed after Shift+R pressed Approve").toBe(0);
    const reason = view.getByRole("textbox", { name: "What should change" });
    expect(document.activeElement, "the focus did not start on the reason").toBe(reason);
    expect(reason).toHaveValue("One change.");

    // What held the focus before the dialog took it is where closing gives it back.
    await user.keyboard("{Escape}");
    expect(view.queryByRole("dialog", { name: "Request changes" })).toBeNull();
    expect(document.activeElement, "closing left the focus off Approve").toBe(approve);
  } finally {
    frames.mockRestore();
    view.unmount();
  }
});

/** A field that focuses itself in a passive effect as it mounts, as the margin sheet's composer
 *  does once the sheet is open (`MentionComposer`'s `autoFocus`). */
function FocusesItselfInAnEffect(): ReactNode {
  const field = useRef<HTMLTextAreaElement>(null);
  useEffect(() => field.current?.focus(), []);
  return <textarea aria-label="Comment" ref={field} />;
}

/** A sheet a press on Open opens, whose Comment field focuses itself: by its own passive effect,
 *  or by `autoFocus`, which React carries out in the commit before any layout effect of the
 *  sheet's. Its Close comes first, where the sheet's own initial focus would go. */
function SheetAroundAField({ field }: { field: "autoFocus" | "effect" }): ReactNode {
  const [open, setOpen] = useState(false);
  const dialog = useDialog<HTMLDivElement>({ onClose: () => setOpen(false), open });
  return (
    <>
      <button onClick={() => setOpen(true)} type="button">
        Open
      </button>
      {open ? (
        <div aria-label="Sheet" ref={dialog.containerRef} role="dialog">
          <button type="button">Close</button>
          {field === "effect" ? (
            <FocusesItselfInAnEffect />
          ) : (
            // biome-ignore lint/a11y/noAutofocus: a field that takes the focus as it is drawn is the case under test
            <textarea aria-label="Comment" autoFocus />
          )}
        </div>
      ) : null}
    </>
  );
}

// A field inside the dialog that focuses itself as the dialog opens keeps that focus: one that
// did so first is left alone, and one that does so after the dialog has moved the focus moves it
// last. Either way, closing gives the focus back to the control the dialog was opened from, as
// WAI-ARIA's dialog pattern has it, and not to the page's body. So it does after the `autoFocus`
// field, which takes the focus in the commit that opens the dialog, before any layout effect of
// the dialog's, and which the dialog's close unmounts.
test("a field that focuses itself as its dialog opens keeps the focus, and closing gives it back to the opener", async () => {
  for (const field of ["autoFocus", "effect"] as const) {
    const user = userEvent.setup();
    const view = render(
      <KeymapProvider>
        <SheetAroundAField field={field} />
      </KeymapProvider>
    );
    try {
      const opener = view.getByRole("button", { name: "Open" });
      await user.click(opener);
      expect(document.activeElement, `the ${field} field lost the focus`).toBe(
        view.getByRole("textbox", { name: "Comment" })
      );
      await user.keyboard("{Escape}");
      expect(
        view.queryByRole("dialog", { name: "Sheet" }),
        `the ${field} sheet stayed open`
      ).toBeNull();
      expect(document.activeElement, `closing the ${field} sheet left the focus off Open`).toBe(
        opener
      );
    } finally {
      view.unmount();
    }
  }
});
