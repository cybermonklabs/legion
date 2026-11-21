import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { type ReactNode, useState } from "react";

import { KeymapProvider } from "./KeymapProvider";
import { DIALOG_SCOPE, useKeymap } from "./keymap";
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

/** A part of a dialog that answers Escape itself, on the scope the dialog's own Escape is on, as
 *  the drawer's Inbox list does (`InboxDrawer.tsx`'s `keymapScope={DIALOG_SCOPE}`). */
function OwnEscape({ onEscape }: { onEscape: () => void }) {
  useKeymap(DIALOG_SCOPE, [
    {
      effect: "moves nothing",
      id: "part.escape",
      inEditable: true,
      keys: "Escape",
      label: "The part's own Escape",
      run: onEscape,
    },
  ]);
  return <p>part</p>;
}

test("a part that mounts after its dialog opened takes Escape before the dialog closes", async () => {
  // The drawer's Inbox list arrives in a chunk of its own after the drawer's frame the first
  // time it opens, so its Escape registers after the dialog's. Here the part mounts in the
  // commit after the dialog's, whichever row ran before this one.
  const user = userEvent.setup();
  let partsEscapes = 0;
  function Drawer({ part }: { part: boolean }) {
    const [open, setOpen] = useState(true);
    return (
      <Dialog label="Drawer" onClose={() => setOpen(false)} open={open}>
        {part ? <OwnEscape onEscape={() => (partsEscapes += 1)} /> : null}
      </Dialog>
    );
  }
  const view = render(
    <KeymapProvider>
      <Drawer part={false} />
    </KeymapProvider>
  );
  try {
    view.rerender(
      <KeymapProvider>
        <Drawer part />
      </KeymapProvider>
    );
    expect(view.getByText("part")).toBeDefined();
    await user.keyboard("{Escape}");
    expect(partsEscapes).toBe(1);
    expect(view.queryByRole("dialog", { name: "Drawer" })).not.toBeNull();
  } finally {
    view.unmount();
  }
});
