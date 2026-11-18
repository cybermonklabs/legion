import type { FocusLoss } from "./focus-keeper";

/**
 * The box `ApprovalChip` draws a document's approval chip in on the header: the chip, its two
 * actions beside it, and the dialogs it opens, deeper in the same box. The header's keys
 * (`useHeaderKeys.ts`), its focus keeper (`IssueHeader.tsx`), its count of what waits
 * (`IssuePropertiesRow.tsx`) and the header's browser rows (`e2e/issue-page-p.ts`) all read the
 * chip and its actions from here, so what one of them takes for an action the others do too.
 */

/** A document's approval chip (`ApprovalChip`'s `data-approval-artifact`). The header draws it on
 *  its properties line alone (`IssuePropertiesRow.tsx`): one for the issue's document, and one on
 *  each entry of a gated root's steps. */
export const APPROVAL_CHIP = "[data-approval-artifact]";

/** A chip's two actions: the buttons beside it in the box `ApprovalChip` draws it in, or in the
 *  box that keeps the two together (`keepActionsTogether`). The dialogs the chip opens are drawn
 *  deeper in that box, so none of their buttons is one of these. */
export const CHIP_ACTIONS = ":scope > button, :scope > span > button";

/** The words each action draws (`ApprovalChip`): Approve says the version on a stale approval.
 *  Neither action carries an attribute of its own, so each is found by the words a reader reads
 *  on it; its name may also say the document (`actionsDocument`), its words never do. */
export const ACTION_WORDS = {
  approve: /^Approve( v\d+)?$/,
  "request-changes": /^Request changes$/,
} as const;

/** The nearest box from `from` up to `top`, `top` left out, that holds an approval chip as its own
 *  child, with that chip, or `null` when no such box is there. That box is the one `ApprovalChip`
 *  draws a chip and its two actions in, so the chip found is always the document's that `from`
 *  belongs to, never the one of another entry on the line. */
export function chipBox(
  from: HTMLElement | null,
  top: Element
): { readonly box: HTMLElement; readonly chip: HTMLElement } | null {
  for (let box = from; box !== null && box !== top; box = box.parentElement) {
    const chip = box.querySelector<HTMLElement>(`:scope > ${APPROVAL_CHIP}`);
    if (chip !== null) {
      return { box, chip };
    }
  }
  return null;
}

/**
 * Where the header's focus keeper (`useFocusKeeper`) puts the focus when the control it lost
 * stood in the box `ApprovalChip` draws a chip in: that chip, or `null` when the control stood in
 * no such box. That box holds the chip, its two actions and the dialogs the chip opens, and
 * nothing else, so where the control stood is enough to say it was one of the two actions or a
 * control of one of those dialogs. Both actions go once the request is answered, so a reader who
 * pressed one stays on the document they reviewed, and the chip is what stays of its approval
 * control.
 *
 * The Request changes dialog gives the focus back to the header's Request changes as it shuts.
 * When that button went first - the request was answered elsewhere while the dialog was open -
 * the focus falls from the dialog's own control: its submit, which reads "Submitting…" then, or
 * its text box after a Mod+Enter. That control stood in the chip's box too, so it finds the same
 * chip.
 *
 * The lost control may already be out of the page, so its chip is found from where it stood
 * (`FocusLoss.place`).
 */
export function chipOfLostAction({ place, root }: FocusLoss): HTMLElement | null {
  return chipBox(place, root)?.chip ?? null;
}
