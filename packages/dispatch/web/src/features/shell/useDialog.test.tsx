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

// In its first frame the dialog itself holds the focus, and it is the first element of the dialog
// in the Tab order, so a Shift+Tab from it would leave the dialog for the control behind it. The
// hook sends that Shift+Tab to the dialog's last control instead, and the key typed after it
// lands there, not on the control that opened the dialog.
test("a Shift+Tab and a key in a dialog's first frame stay inside the dialog", async () => {
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
    expect(document.activeElement, "the dialog left the focus behind it as it opened").toBe(dialog);
    await user.tab({ shift: true });
    await user.keyboard(" ");
    expect(
      pressed,
      "a Shift+Tab and a Space in the dialog's first frame pressed the control under it"
    ).toBe(0);
    expect(
      document.activeElement,
      "a Shift+Tab in the dialog's first frame went to no last control of it"
    ).toBe(view.getByRole("textbox", { name: "Reason" }));
  } finally {
    frames.mockRestore();
    view.unmount();
  }
});

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
 *  sheet's Search and New issue do (`app.tsx`). With `staysMounted`, the closed sheet keeps its
 *  element and its Search, as the margin sheet's `aside` does; otherwise it is removed. With
 *  `withInner`, Search opens a second dialog inside the Search dialog, in a child component, in
 *  the same commit. */
function SheetThatOpensAnother({
  onMenu,
  staysMounted,
  withInner = false,
}: {
  onMenu: () => void;
  staysMounted: boolean;
  withInner?: boolean;
}) {
  const [sheet, setSheet] = useState(false);
  const [search, setSearch] = useState(false);
  const [inner, setInner] = useState(false);
  const sheetDialog = useDialog<HTMLDivElement>({ onClose: () => setSheet(false), open: sheet });
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
      {sheet || staysMounted ? (
        // biome-ignore lint/a11y/useAriaPropsSupportedByRole: named only while it is an open dialog, so a closed sheet is no dialog of that name; its role follows the open state, as the margin sheet's `aside`'s role and `aria-modal` do
        <div
          aria-label={sheet ? "Sheet" : undefined}
          ref={sheetDialog.containerRef}
          role={sheet ? "dialog" : undefined}
        >
          <button
            onClick={() => {
              setSheet(false);
              setSearch(true);
              setInner(withInner);
            }}
            type="button"
          >
            Search
          </button>
        </div>
      ) : null}
      <Dialog label="Search" onClose={() => setSearch(false)} open={search}>
        <Dialog label="Inner" onClose={() => setInner(false)} open={search && inner} />
      </Dialog>
    </>
  );
}

// A dialog opened in the commit that closes another holds the focus from its first frame: the
// closing dialog gives the focus back to nothing behind it. On close it gives the focus back where
// the closing one would have, to the control that opened that one, whether the closing one's
// element went with it or stayed with the control that opened the new dialog inside it.
for (const staysMounted of [false, true]) {
  test(`a dialog opened as another closes keeps the focus, then gives it back to that one's opener${staysMounted ? ", the closing one's element still mounted" : ""}`, async () => {
    const user = userEvent.setup();
    let menus = 0;
    const view = render(
      <KeymapProvider>
        <SheetThatOpensAnother onMenu={() => (menus += 1)} staysMounted={staysMounted} />
      </KeymapProvider>
    );
    const frames = holdFrames();
    try {
      await user.click(view.getByRole("button", { name: "Menu" }));
      frames.run();
      await user.click(view.getByRole("button", { name: "Search" }));
      expect(
        view.queryByRole("dialog", { name: "Sheet" }),
        "Search left the sheet open"
      ).toBeNull();
      expect(document.activeElement, "the closing sheet took the focus from Search's dialog").toBe(
        view.getByRole("dialog", { name: "Search" })
      );
      await user.keyboard(" ");
      expect(menus, "a Space in the Search dialog's first frame pressed Menu behind it").toBe(1);
      frames.run();
      await user.keyboard("{Escape}");
      expect(view.queryByRole("dialog", { name: "Search" }), "Escape left Search open").toBeNull();
      expect(
        document.activeElement,
        staysMounted
          ? "Search gave the focus back to its own opener in the closed sheet, not to Menu"
          : "Search gave the focus back to no opener"
      ).toBe(view.getByRole("button", { name: "Menu" }));
    } finally {
      frames.restore();
      view.unmount();
    }
  });
}

// A pair of nested dialogs - one in a child component of the other - opened in the commit that
// closes another dialog. The outer one has no target of its own, and the closing one gives it its
// own; the inner one's target is the outer element, which stays. So the outer one gives the focus
// back to the closed one's opener.
test("a nested pair opened as another closes gives the focus back in turn, then to that one's opener", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <SheetThatOpensAnother onMenu={() => {}} staysMounted={false} withInner />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    await user.click(view.getByRole("button", { name: "Menu" }));
    frames.run();
    await user.click(view.getByRole("button", { name: "Search" }));
    expect(document.activeElement, "the inner dialog took no focus as it opened").toBe(
      view.getByRole("dialog", { name: "Inner" })
    );
    frames.run();
    await user.keyboard("{Escape}");
    expect(
      document.activeElement,
      "the inner dialog gave the focus to no control of Search's dialog"
    ).toBe(view.getByRole("button", { name: "First control of Search" }));
    await user.keyboard("{Escape}");
    expect(view.queryByRole("dialog", { name: "Search" }), "Escape left Search open").toBeNull();
    expect(document.activeElement, "the outer of the pair gave the focus back to no opener").toBe(
      view.getByRole("button", { name: "Menu" })
    );
  } finally {
    frames.restore();
    view.unmount();
  }
});

/** Two dialogs, one in a child component of the other, opened by one press. The child's layout
 *  effect runs before the parent's. Without `outerControl`, the outer one has no control of its
 *  own: its only controls are the inner one's. */
function NestedInAChild({ outerControl = true }: { outerControl?: boolean }) {
  const [outer, setOuter] = useState(false);
  const [inner, setInner] = useState(false);
  const outerDialog = useDialog<HTMLDivElement>({ onClose: () => setOuter(false), open: outer });
  return (
    <>
      <button
        onClick={() => {
          setOuter(true);
          setInner(true);
        }}
        type="button"
      >
        Opener
      </button>
      {outer ? (
        <div aria-label="Outer" ref={outerDialog.containerRef} role="dialog">
          <p>Words of Outer</p>
          {outerControl ? <button type="button">First control of Outer</button> : null}
          <Dialog label="Inner" onClose={() => setInner(false)} open={inner} />
        </div>
      ) : null}
    </>
  );
}

// A dialog in a child component of another, opened with it in one commit, takes the focus first.
// When it closes the focus goes to the outer dialog's first control, and when the outer one
// closes the focus goes back to the control that opened both.
test("two nested dialogs of a parent and a child opened in one commit give the focus back in turn", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <NestedInAChild />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    await user.click(view.getByRole("button", { name: "Opener" }));
    expect(document.activeElement, "the inner dialog took no focus as it opened").toBe(
      view.getByRole("dialog", { name: "Inner" })
    );
    frames.run();
    await user.keyboard("{Escape}");
    expect(
      view.queryByRole("dialog", { name: "Inner" }),
      "Escape left the inner dialog open"
    ).toBeNull();
    expect(
      document.activeElement,
      "the inner dialog gave the focus to no control of the outer"
    ).toBe(view.getByRole("button", { name: "First control of Outer" }));
    await user.click(view.getByText("Words of Outer"));
    expect(document.activeElement, "a press on the outer dialog's text focused the dialog").toBe(
      document.body
    );
    await user.keyboard("{Escape}");
    expect(
      view.queryByRole("dialog", { name: "Outer" }),
      "Escape left the outer dialog open"
    ).toBeNull();
    expect(document.activeElement, "the outer dialog gave the focus back to no opener").toBe(
      view.getByRole("button", { name: "Opener" })
    );
  } finally {
    frames.restore();
    view.unmount();
  }
});

// The late frame this hook is for: the inner dialog closes before the outer one's frame has run.
// Until that frame the inner one gives the focus back to the outer element, which is focusable
// for it (happy-dom focuses an element without `tabindex` too, so the row reads the attribute).
// The outer's frame then gives the focus to its first control.
test("a dialog inside another that closes before the outer's frame gives the focus to the outer", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <NestedInAChild />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    await user.click(view.getByRole("button", { name: "Opener" }));
    const outer = view.getByRole("dialog", { name: "Outer" });
    expect(
      outer.getAttribute("tabindex"),
      "the outer dialog could take no focus back from the inner one before its frame"
    ).toBe("-1");
    await user.keyboard("{Escape}");
    expect(
      view.queryByRole("dialog", { name: "Inner" }),
      "Escape left the inner dialog open"
    ).toBeNull();
    expect(document.activeElement, "the inner dialog gave the focus to no part of the outer").toBe(
      outer
    );
    frames.run();
    expect(document.activeElement, "the outer's frame gave the focus to no control of it").toBe(
      view.getByRole("button", { name: "First control of Outer" })
    );
  } finally {
    frames.restore();
    view.unmount();
  }
});

// A dialog whose only controls are a dialog's inside it is its own field: when the inner one
// closes the focus goes to the outer element, which stays focusable for it, and not to a control
// the inner one took with it. happy-dom focuses an element without `tabindex` too, where a browser
// leaves the focus on the body, so the row reads the attribute after the frames.
test("a dialog whose only controls are inside a dialog inside it gets the focus back on itself", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <NestedInAChild outerControl={false} />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    await user.click(view.getByRole("button", { name: "Opener" }));
    frames.run();
    expect(
      view.getByRole("dialog", { name: "Outer" }).getAttribute("tabindex"),
      "the outer dialog, with no field of its own, stopped being focusable at its frame"
    ).toBe("-1");
    await user.keyboard("{Escape}");
    expect(
      view.queryByRole("dialog", { name: "Inner" }),
      "Escape left the inner dialog open"
    ).toBeNull();
    expect(
      document.activeElement,
      "the inner dialog gave the focus to the body, not to the outer"
    ).toBe(view.getByRole("dialog", { name: "Outer" }));
    await user.keyboard("{Escape}");
    expect(document.activeElement, "the outer dialog gave the focus back to no opener").toBe(
      view.getByRole("button", { name: "Opener" })
    );
  } finally {
    frames.restore();
    view.unmount();
  }
});

/** A sheet whose own control closes it and opens a pair of nested dialogs of one component, the
 *  outer one's hook first, as the margin sheet and its thread are (`MarginSheet`). */
function SheetThatOpensASheetWithThread() {
  const [sheet, setSheet] = useState(false);
  const [pair, setPair] = useState(false);
  const [thread, setThread] = useState(false);
  const sheetDialog = useDialog<HTMLDivElement>({ onClose: () => setSheet(false), open: sheet });
  const pairDialog = useDialog<HTMLDivElement>({ onClose: () => setPair(false), open: pair });
  const threadDialog = useDialog<HTMLDivElement>({
    onClose: () => setThread(false),
    open: pair && thread,
  });
  return (
    <>
      <button onClick={() => setSheet(true)} type="button">
        Menu
      </button>
      {sheet ? (
        <div aria-label="Sheet" ref={sheetDialog.containerRef} role="dialog">
          <button
            onClick={() => {
              setSheet(false);
              setPair(true);
              setThread(true);
            }}
            type="button"
          >
            Open the pair
          </button>
        </div>
      ) : null}
      {pair ? (
        <div aria-label="Pair" ref={pairDialog.containerRef} role="dialog">
          <button type="button">First control of Pair</button>
          {thread ? (
            <div aria-label="Thread" ref={threadDialog.containerRef} role="dialog">
              <button type="button">First control of Thread</button>
            </div>
          ) : null}
        </div>
      ) : null}
    </>
  );
}

// A nested pair of one component, the outer hook first, opened in the commit that closes another
// dialog: both hold the focus, the outer's record comes first and has no target, and the inner's
// names the outer. The closing dialog gives its target to every holder whose own is none, so the
// outer gives the focus back to the closed one's opener: a hand-over to the last holder alone
// would leave the outer none.
test("a pair of one component opened as another closes gives the focus back in turn, then to that one's opener", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <SheetThatOpensASheetWithThread />
    </KeymapProvider>
  );
  const frames = holdFrames();
  try {
    await user.click(view.getByRole("button", { name: "Menu" }));
    frames.run();
    await user.click(view.getByRole("button", { name: "Open the pair" }));
    expect(document.activeElement, "the thread took no focus as it opened").toBe(
      view.getByRole("dialog", { name: "Thread" })
    );
    frames.run();
    await user.keyboard("{Escape}");
    expect(document.activeElement, "the thread gave the focus to no control of the pair").toBe(
      view.getByRole("button", { name: "First control of Pair" })
    );
    await user.keyboard("{Escape}");
    expect(view.queryByRole("dialog", { name: "Pair" }), "Escape left the pair open").toBeNull();
    expect(document.activeElement, "the pair gave the focus back to no opener").toBe(
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
