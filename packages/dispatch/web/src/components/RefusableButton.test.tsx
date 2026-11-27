import { expect, test } from "bun:test";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { pressHeld } from "../__tests__/held-press";
import { RefusableButton } from "./RefusableButton";

/** A form whose submit starts a write that stays out, and whose Save reads `Saving…` while it
 *  is: the form's own submit refuses nothing, so a second press that submits is a second write. */
function SaveForm({ onSave }: { onSave: () => void }): ReactNode {
  const [saving, setSaving] = useState(false);
  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        onSave();
        setSaving(true);
      }}
    >
      <RefusableButton busy={saving ? "Saving…" : undefined} refusal={null} type="submit">
        Save
      </RefusableButton>
    </form>
  );
}

/** A button whose press starts a write that stays out, and which reads `Removing…` while it is.
 *  It is refused too while the write is out, so a press the busy hold let through to the refusal
 *  would be a refused press. */
function RemoveButton({
  onRefusedPress,
  onRemove,
}: {
  onRefusedPress: () => void;
  onRemove: () => void;
}): ReactNode {
  const [removing, setRemoving] = useState(false);
  return (
    <RefusableButton
      busy={removing ? "Removing…" : undefined}
      onPress={() => {
        onRemove();
        setRemoving(true);
      }}
      onRefusedPress={onRefusedPress}
      refusal={removing ? "The removal is still out." : null}
    >
      Remove
    </RefusableButton>
  );
}

// A busy button is held, never disabled: the browser takes the focus off a button that turns
// disabled (`emulateChromiumFocusFixup`, on in `pressHeld`), which would leave the reader who
// pressed it on the page's body. Held, it keeps the focus and reads its busy label, and a press
// of it - Enter, or a click - does nothing: a submit button's submits no form.
test("a busy submit button keeps the focus, and a press of it submits nothing", async () => {
  const user = userEvent.setup();
  let saves = 0;
  render(<SaveForm onSave={() => (saves += 1)} />);

  try {
    const save = screen.getByRole("button", { name: "Save" });
    await pressHeld(user, save, { name: "Save", sent: () => saves });
    expect(save.textContent).toBe("Saving…");
    expect(save.hasAttribute("disabled")).toBe(false);
  } finally {
    cleanup();
  }
});

test("a busy button keeps the focus, and a press of it does nothing and is no refused press", async () => {
  const user = userEvent.setup();
  let removes = 0;
  let refusedPresses = 0;
  render(
    <RemoveButton onRefusedPress={() => (refusedPresses += 1)} onRemove={() => (removes += 1)} />
  );

  try {
    const remove = screen.getByRole("button", { name: "Remove" });
    await pressHeld(user, remove, { name: "Remove", sent: () => removes });
    expect(remove.textContent).toBe("Removing…");
    expect(remove.hasAttribute("disabled")).toBe(false);
    expect(refusedPresses).toBe(0);
  } finally {
    cleanup();
  }
});
