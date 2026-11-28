/**
 * The hold on a control that cannot be pressed now: while the write or the read its press started
 * is out, while its step has no page to go to, or while it has nothing to do, as Rename with no
 * new name (`SessionGroupsEditor`) and Move with no epic staged (`IssueTableSelection`). It holds
 * by `aria-disabled` and a press that does nothing (`heldPress`), by a read-only text field
 * (`heldField`), and through the commit after the write's answer (`useHeldThroughAnswer`).
 * `StepButton`, `PanelTrigger`, `QueryError`'s Retry and the pages' own pressed controls use it,
 * and `RefusableButton`'s `busy` holds by the same rule. The hold through a write's answer keeps
 * the armed hand-off of the press that started the write live while it is on (`holdHandOff`). A
 * control held with nothing to do - since before its press, or still once what answers its press
 * has ended - keeps no press live (`armHandOff`).
 */
import { useEffect, useState } from "react";

import { holdHandOff } from "./focus-place";

/**
 * What a button spreads onto itself to be held while it cannot be pressed - its write is out,
 * its step has no page to go to, it has nothing to do - rather than disabled: `aria-disabled`,
 * and a press that does nothing then. The browser takes the focus off a control that disables
 * itself under the reader's press, dropping a keyboard reader onto the page, and a dialog the
 * press opened would hand the focus back to an opener that can no longer take it. Held, the
 * button keeps the focus and its Tab stop, and the `refused:` variant (`styles.css`) paints it
 * as it paints a disabled one.
 */
export function heldPress(
  held: boolean,
  press: () => void
): { "aria-disabled": true | undefined; onClick: () => void } {
  return {
    "aria-disabled": held || undefined,
    onClick: () => {
      if (!held) press();
    },
  };
}

/**
 * What a text field spreads onto itself to be held while a write holds the form it is part of:
 * `aria-disabled`, and read-only, so it takes no typing. It is never disabled, so the field the
 * reader submitted from with Enter keeps the focus, as a held button does (`heldPress`).
 */
export function heldField(held: boolean): {
  "aria-disabled": true | undefined;
  readOnly: boolean;
} {
  return { "aria-disabled": held || undefined, readOnly: held };
}

/**
 * Whether a control whose press started a write is held: from the first render with `pending`
 * until the commit after `pending` turns false. So the pressed control stays focusable, and takes
 * no second press, while its write and the read after it are out. A list that holds the write
 * itself, as the Inbox does a row's snooze (`useAskSnoozeWrites`) and its Assign to me
 * (`AskAssignWritesProvider`), lets go of the row in the answer's own commit, while this hold is
 * still on. The hold keeps the armed hand-off of the press that started the write live while it
 * is on (`holdHandOff`): it is begun as the commit that first draws `pending` runs its effects,
 * of the press armed then, the one that started the write, and stays on that press however the
 * reader presses again while the write is out. So where the press was armed at a component's root
 * (`armOnPress`), that component's hand-off reads the going of what the press held as the
 * press's doing (`causedByReader`), as the selection bar's does when its Snooze lands. With
 * nothing armed, the hold arms nothing.
 */
export function useHeldThroughAnswer(pending: boolean): boolean {
  const [answering, setAnswering] = useState(false);
  useEffect(() => {
    if (!pending) return;
    setAnswering(true);
    return () => setAnswering(false);
  }, [pending]);
  const held = pending || answering;
  useEffect(() => (held ? holdHandOff() : undefined), [held]);
  return held;
}
