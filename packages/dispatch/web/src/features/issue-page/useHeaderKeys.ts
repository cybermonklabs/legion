import type { IssuePriority } from "../../api/types";
import { outsideAskCard } from "../inbox/ask-card";
import type { IssuePriorityWrite } from "../issue/useIssuePriority";
import { useKeymap } from "../shell/keymap";
import { ACTION_WORDS, CHIP_ACTIONS, chipBox } from "./approval-chip-box";
import { EDIT_TITLE_BUTTON } from "./IssueTitleRow";
import { PROPERTY_LABELS } from "./page-structure";
import { usableControl } from "./usable-control";

/**
 * What the header's keys act on. Every target is a control the header already renders - the
 * pin inside its overflow menu, which is drawn hidden while shut - so a key does exactly what a
 * click on it does. Scoping the lookup to `[data-issue-header]` keeps a child issue's row - it
 * carries the same accessible names - out of reach.
 */
const HEADER = "[data-issue-header]";
const PIN_TOGGLE = 'button[data-issue-action="pin"]';
const PRIORITY_SELECT = 'select[aria-label^="Priority of "]';
const STATUS_SELECT = `select[aria-label="${PROPERTY_LABELS.status}"]`;

/** The group around `node` that a control of the header opens and shuts (`aria-expanded`) and
 *  names by `aria-controls`: the actions menu's, hidden while the menu is shut
 *  (`IssueActionsMenu.tsx`). The nearest such group is taken, and none where no control names
 *  one, so no other element around `node`, hidden or not, stands in for it. */
function menuGroup(node: Element): Element | null {
  const header = node.closest(HEADER);
  for (
    let group = node.parentElement;
    group !== null && group !== header;
    group = group.parentElement
  ) {
    if (group.id !== "") {
      const opener = `[aria-expanded][aria-controls~="${CSS.escape(group.id)}"]`;
      if (header?.querySelector(opener) != null) return group;
    }
  }
  return null;
}

/** The header control `selector` names while it takes a press by the page's one rule
 *  (`usable-control.ts`), else `null`: the one fact both the key's `run` and the `when` that
 *  offers it read. The pin is read up to the actions menu's group that holds it (`menuGroup`),
 *  which is shut, hidden, while the menu is, since its key presses it there as a click on it in
 *  the open menu does; no other target stands in a group that shuts. */
function headerControl(selector: string): HTMLElement | null {
  const node = document.querySelector(HEADER)?.querySelector<HTMLElement>(selector);
  if (node == null) {
    return null;
  }
  const bound = selector === PIN_TOGGLE ? menuGroup(node) : null;
  return usableControl(node, bound) ? node : null;
}

/**
 * The `action` of the approval chip that holds the focus, or `null` when the focus is on no chip
 * of the header and on neither of a chip's two actions, when the chip draws no such action - no
 * request of its document waits on the reader, or the document is approved, which draws no
 * Approve - or when the action refuses a press while its review is written: inside `NoTooltips`,
 * which holds the header, both are `aria-disabled` then and never `disabled`.
 * Whether the action takes a press is the page's one rule (`usableControl`).
 * Like `headerControl`, the one fact a key's `run` and its `when` both read.
 */
function focusedApprovalAction(action: keyof typeof ACTION_WORDS): HTMLButtonElement | null {
  const header = document.querySelector(HEADER);
  const active = document.activeElement;
  if (header == null || !(active instanceof HTMLElement) || !header.contains(active)) {
    return null;
  }
  const found = chipBox(active, header);
  if (found === null) {
    return null;
  }
  const { box, chip } = found;
  const actions = [...box.querySelectorAll<HTMLButtonElement>(CHIP_ACTIONS)].filter(
    (button) => button !== chip
  );
  if (active !== chip && !actions.some((button) => button === active)) {
    return null;
  }
  const button = actions.find((each) => ACTION_WORDS[action].test(each.textContent ?? ""));
  if (button === undefined) {
    return null;
  }
  return usableControl(button) ? button : null;
}

/**
 * The line at the outline's foot while it answers a question: the question's options are its
 * chips there, which the keyboard reaches, so its controls own the keys as an ask card's do.
 */
const ANSWER_LINE = "form[data-answering]";

/**
 * Whether focus is outside every `<input>`, every ask card and the line that answers a question.
 * `0`-`3` and `Shift+P` are the two keys that write the issue without focusing a control, so they
 * are the two that have to say where the reader is not: a digit typed straight after picking a
 * question's option belongs to that control's own surface, not to the issue's priority. The
 * registry passes single keys through every control that takes no typed text, so this is the
 * page's own question to answer.
 *
 * A focused `<button>` is deliberately not excluded. Every single-key binding in the registry
 * fires from one: a digit has no native meaning on a button, and after any click - a view, a
 * disclosure, Copy - focus sits on the button that was clicked, which is where a reader reaching
 * for the next shortcut stands. An ask card's buttons and the answer line's are the exception,
 * and that is what `outsideAskCard` (`inbox/ask-card.ts`, the one definition both pages read) and
 * `ANSWER_LINE` cover: there, the question's controls own the keys. `e` opens a control rather
 * than write, so a stray press is visible and undoable and it is offered throughout.
 */
function outsideInputsAndAskCards(): boolean {
  const active = document.activeElement;
  return (
    !(active instanceof HTMLInputElement) &&
    outsideAskCard() &&
    active?.closest(ANSWER_LINE) == null
  );
}

/**
 * The header's keys: the ones today's header binds, each with its target on the new header (`s`
 * focuses the status and `p` the priority, `0` to `3` set the priority, `e` puts the caret in the
 * title, `Shift+P` pins and unpins, and Escape leaves a focused status or priority), and `a` and
 * `Shift+R`, which approve and open Request changes on the focused approval chip or entry.
 *
 * They register into the `issue` scope the view row pushes (`useIssueKeymap.ts`); this hook
 * pushes none. The digits go through the header's one priority write, the one its picker and the
 * palette's Set priority rows use, so a refusal of any of them is said once, by the header.
 *
 * `s`, `p` and `e` are offered exactly while their control takes a press by the page's one rule
 * (`usable-control.ts`, read through `headerControl` here), and the digits and `Shift+P` exactly
 * while theirs does and the focus is where those two may write (`outsideInputsAndAskCards`). The
 * rule refuses a control that is disabled (a closed issue, a save in flight), and also one that
 * is busy (`aria-disabled`), `aria-hidden` or not drawn. The pin is read only up to the shut
 * actions menu (`headerControl`), so the menu's `hidden` does not refuse it, nor does its not
 * being drawn there: `Shift+P` is offered while the shut menu hides the pin.
 */
export function useHeaderKeys(priorityWrite: IssuePriorityWrite): void {
  useKeymap("issue", [
    {
      id: "status",
      keys: "s",
      label: "Focus the status",
      run: () => headerControl(STATUS_SELECT)?.focus(),
      when: () => headerControl(STATUS_SELECT) !== null,
    },
    {
      id: "priority",
      keys: "p",
      label: "Focus the priority",
      run: () => headerControl(PRIORITY_SELECT)?.focus(),
      when: () => headerControl(PRIORITY_SELECT) !== null,
    },
    {
      id: "set-priority",
      keys: ["0", "1", "2", "3"],
      label: "Set priority P0–P3",
      run: (event) => priorityWrite.submit(Number(event.key) as IssuePriority),
      when: () => headerControl(PRIORITY_SELECT) !== null && outsideInputsAndAskCards(),
    },
    {
      // Edit title's click opens the editor, and a focus does not, so the key clicks it: a
      // button that already holds the focus - where Escape and Enter leave it - takes no new
      // focus. The title row draws the button only while the issue is open and its title not
      // edited (`IssueTitleRow.tsx`), so the key is offered exactly while a click opens it.
      id: "title",
      keys: "e",
      label: "Edit the title",
      run: () => headerControl(EDIT_TITLE_BUTTON)?.click(),
      when: () => headerControl(EDIT_TITLE_BUTTON) !== null,
    },
    {
      id: "pin",
      keys: "Shift+P",
      label: "Pin or unpin the issue",
      run: () => headerControl(PIN_TOGGLE)?.click(),
      when: () => headerControl(PIN_TOGGLE) !== null && outsideInputsAndAskCards(),
    },
    // `a` and `Shift+R` are the approval's only while the focus is on the chip or entry of a
    // document whose request waits on the reader, or on one of its two actions; anywhere else
    // they are left to other bindings of the page. Each presses the action that chip draws, so a
    // key is taken exactly while that action would take a click.
    //
    // Each is a palette row too, offered on the same focus, as a focused row's own actions are
    // (`agents/keyboard.ts`'s pin, `IssueBoard.tsx`'s `o`): the palette reads its rows while the
    // focus is still on the chip and runs the one chosen once the focus is back on it
    // (`SearchPalette.tsx`), and neither key walks a list or repeats an action with a row.
    {
      id: "approve",
      keys: "a",
      label: "Approve the focused approval request",
      run: () => focusedApprovalAction("approve")?.click(),
      when: () => focusedApprovalAction("approve") !== null,
    },
    {
      id: "request-changes",
      keys: "Shift+R",
      label: "Request changes on the focused approval request",
      run: () => focusedApprovalAction("request-changes")?.click(),
      when: () => focusedApprovalAction("request-changes") !== null,
    },
    {
      // `s` and `p` put focus in a native select, where every single-key binding is suspended
      // and the next keystroke is the select's own type-ahead - `t` would pick Todo or Testing
      // rather than start a view's chord. Escape is the way back out, as it is on the Inbox
      // rows, the board's cards and the architecture rows.
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back out of the focused header control",
      run: () => (document.activeElement as HTMLElement | null)?.blur(),
      when: () => {
        const active = document.activeElement;
        return active === headerControl(STATUS_SELECT) || active === headerControl(PRIORITY_SELECT);
      },
    },
  ]);
}
