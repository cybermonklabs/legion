import { expect, mock, test } from "bun:test";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type FormEvent, type ReactNode, useState } from "react";

import { runFocusFixup } from "../__tests__/focus-fixup";
import { pressHeld } from "../__tests__/held-press";
import { RefusableButton } from "./RefusableButton";

// A keyboard reader who presses a button keeps their place while that press is in flight, so their
// next key reaches the button rather than the page's body. While the press is in flight the button
// says it cannot be pressed, and a press of it does nothing; once it ends, a press works again.
test("a keyboard press keeps the focus while it is in flight, and Enter presses again once it ends", async () => {
  const user = userEvent.setup();
  const onPress = mock(() => {});
  const saveButton = (busy?: string) => (
    <RefusableButton busy={busy} onPress={onPress} refusal={null}>
      Save
    </RefusableButton>
  );
  const view = render(saveButton());
  const stopFocusFixup = runFocusFixup();
  try {
    const save = screen.getByRole("button", { name: "Save" });
    act(() => save.focus());
    await user.keyboard(" ");
    expect(onPress).toHaveBeenCalledTimes(1);

    // The press is in flight: the button names it, says it cannot be pressed, either way a
    // control can say so, and keeps the focus.
    view.rerender(saveButton("Saving…"));
    expect(save).toHaveTextContent("Saving…");
    await waitFor(() => expect(save.matches(":disabled, [aria-disabled='true']")).toBe(true));
    expect(save).toHaveFocus();
    await user.keyboard(" ");
    expect(onPress).toHaveBeenCalledTimes(1);

    view.rerender(saveButton());
    expect(save).toHaveFocus();
    expect(save).not.toHaveAttribute("aria-disabled");
    await user.keyboard("{Enter}");
    expect(onPress).toHaveBeenCalledTimes(2);
  } finally {
    stopFocusFixup();
    view.unmount();
  }
});

// A busy submit button submits its form by no press, a click or a key: the press it names is
// already in flight.
test("a busy submit button keeps the focus and submits its form by neither a click nor a key", async () => {
  const user = userEvent.setup();
  const onSubmit = mock((event: FormEvent<HTMLFormElement>) => event.preventDefault());
  const view = render(
    <form onSubmit={onSubmit}>
      <RefusableButton busy="Saving…" refusal={null} type="submit">
        Save
      </RefusableButton>
    </form>
  );
  try {
    const save = screen.getByRole("button", { name: "Saving…" });
    expect(save).toHaveAttribute("aria-disabled", "true");
    await user.click(save);
    act(() => save.focus());
    await user.keyboard(" ");
    await user.keyboard("{Enter}");
    expect(save).toHaveFocus();
    expect(onSubmit).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

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
