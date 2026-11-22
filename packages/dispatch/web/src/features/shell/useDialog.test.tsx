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
      <button type="button">{`First control of ${label}`}</button>
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
        <p>Words of the dialog</p>
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

/** Holds every animation frame the hook asks for until `run`, as a loaded machine draws its next
 *  frame late. */
function holdFrames(): { restore: () => void; run: () => void } {
  const held: FrameRequestCallback[] = [];
  const frames = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    held.push(callback);
    return held.length;
  });
  return {
    restore: () => frames.mockRestore(),
    run: () =>
      act(() => {
        for (const callback of held.splice(0)) callback(performance.now());
      }),
  };
}

// The frame after it opens, the dialog gives the focus to its first control, and its element is
// focusable no longer: a press on the dialog's text puts the focus on the page's body, as it did
// before the dialog took the focus as it opened.
test("the frame after it opens, a dialog gives the focus to its first control and is no longer focusable", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <ControlUnderADialog onPress={() => {}} />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    act(() => view.getByRole("button", { name: "Approve" }).focus());
    await user.keyboard("R");
    frames.run();
    expect(document.activeElement, "the frame gave the focus to no control of the dialog").toBe(
      view.getByRole("button", { name: "First control of Request changes" })
    );
    await user.click(view.getByText("Words of the dialog"));
    expect(document.activeElement, "a press on the dialog's text focused the dialog").toBe(
      document.body
    );
  } finally {
    frames.restore();
    view.unmount();
  }
});

/** A sheet whose own Search closes it and opens another dialog in one handler, as the navigation
 *  sheet's Search and New issue do (`app.tsx`). */
function SheetThatOpensAnother({ onMenu }: { onMenu: () => void }) {
  const [sheet, setSheet] = useState(false);
  const [search, setSearch] = useState(false);
  return (
    <>
      <button
        onClick={() => {
          onMenu();
          setSheet(true);
        }}
        type="button"
      >
        Menu
      </button>
      <Dialog label="Sheet" onClose={() => setSheet(false)} open={sheet}>
        <button
          onClick={() => {
            setSheet(false);
            setSearch(true);
          }}
          type="button"
        >
          Search
        </button>
      </Dialog>
      <Dialog label="Search" onClose={() => setSearch(false)} open={search} />
    </>
  );
}

// A dialog opened in the commit that closes another holds the focus from its first frame: the
// closing dialog gives the focus back to nothing behind it. On close it gives the focus back where
// the closing one would have, to the control that opened that one.
test("a dialog opened as another closes keeps the focus, then gives it back to that one's opener", async () => {
  const user = userEvent.setup();
  let menus = 0;
  const view = render(
    <KeymapProvider>
      <SheetThatOpensAnother onMenu={() => (menus += 1)} />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    await user.click(view.getByRole("button", { name: "Menu" }));
    frames.run();
    await user.click(view.getByRole("button", { name: "Search" }));
    expect(view.queryByRole("dialog", { name: "Sheet" }), "Search left the sheet open").toBeNull();
    expect(document.activeElement, "the closing sheet took the focus from Search's dialog").toBe(
      view.getByRole("dialog", { name: "Search" })
    );
    await user.keyboard(" ");
    expect(menus, "a Space in the Search dialog's first frame pressed Menu behind it").toBe(1);
    await user.keyboard("{Escape}");
    expect(view.queryByRole("dialog", { name: "Search" }), "Escape left Search open").toBeNull();
    expect(document.activeElement, "Search gave the focus back to no opener").toBe(
      view.getByRole("button", { name: "Menu" })
    );
  } finally {
    frames.restore();
    view.unmount();
  }
});

/** A sheet and a thread inside it that one press opens, as a mark opens the margin sheet and its
 *  thread on a phone (`MarginSheet`: two dialogs of one component, the sheet's first). */
function SheetWithThread() {
  const [open, setOpen] = useState(false);
  const [thread, setThread] = useState(false);
  const sheet = useDialog<HTMLDivElement>({ onClose: () => setOpen(false), open });
  const threadDialog = useDialog<HTMLDivElement>({
    onClose: () => setThread(false),
    open: open && thread,
  });
  return (
    <>
      <button
        onClick={() => {
          setOpen(true);
          setThread(true);
        }}
        type="button"
      >
        Mark
      </button>
      {open ? (
        <div aria-label="Sheet" ref={sheet.containerRef} role="dialog">
          <button type="button">First control of the sheet</button>
          {thread ? (
            <div aria-label="Thread" ref={threadDialog.containerRef} role="dialog">
              <button type="button">First control of the thread</button>
            </div>
          ) : null}
        </div>
      ) : null}
    </>
  );
}

// A dialog opened inside another in the same commit gives the focus back, when it closes, to the
// other one's first control: that one is still open, so the focus stays in it.
test("a dialog opened inside another in one commit gives the focus back into the other", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <SheetWithThread />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    await user.click(view.getByRole("button", { name: "Mark" }));
    expect(document.activeElement, "the thread took no focus as it opened").toBe(
      view.getByRole("dialog", { name: "Thread" })
    );
    frames.run();
    await user.keyboard("{Escape}");
    expect(
      view.queryByRole("dialog", { name: "Thread" }),
      "Escape left the thread open"
    ).toBeNull();
    expect(document.activeElement, "the thread gave the focus to no control of the sheet").toBe(
      view.getByRole("button", { name: "First control of the sheet" })
    );
  } finally {
    frames.restore();
    view.unmount();
  }
});
