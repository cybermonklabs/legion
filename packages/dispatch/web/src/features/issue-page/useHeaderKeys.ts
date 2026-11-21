import type { IssuePriority } from "../../api/types";
import { outsideAskCard } from "../inbox/ask-card";
import { usableControl } from "../issue/header-control";
import type { IssuePriorityWrite } from "../issue/useIssuePriority";
import { useKeymap } from "../shell/keymap";
import { ACTION_WORDS, CHIP_ACTIONS, chipBox } from "./approval-chip-box";

/**
 * What the header's keys act on. Every target is a control the header already renders - the
 * pin inside its overflow menu, which is drawn hidden while shut - so a key does exactly what a
 * click on it does. Each is looked for inside `[data-issue-header]` (`usableControl`,
 * `issue/header-control.ts`), which keeps a child issue's row - it carries the same accessible
 * names - out of reach.
 */
const HEADER = "[data-issue-header]";
const PIN_TOGGLE = 'button[data-issue-action="pin"]';
const PRIORITY_MENU = "[data-priority-menu]";
const STATUS_MENU = "[data-status-menu]";
const TITLE_HEADING = 'h1[tabindex="0"]';

/**
 * The `action` of the approval chip that holds the focus, or `null` when the focus is on no chip
 * of the header and on neither of a chip's two actions, when the chip draws no such action - no
 * request of its document waits on the reader, or the document is approved, which draws no
 * Approve - or when the action refuses a press while its review is written: inside `NoTooltips`,
 * which holds the header, both are `aria-disabled` then and never `disabled`.
 * Like `usableControl`, the one fact a key's `run` and its `when` both read.
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
  return button.getAttribute("aria-disabled") === "true" ? null : button;
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
 * opens the status menu and `p` the priority menu, `0` to `3` set the priority, `e` puts the
 * caret in the title, `Shift+P` pins and unpins), and `a` and `Shift+R`, which approve and open
 * Request changes on the focused approval chip or entry. Each is the catalogue's action of
 * today's page (`keymap-catalogue.ts`), so its label and its keys are the ones the person sees
 * and sets there. A menu's keys are its own while it is open, Escape among them, which closes it
 * and gives the focus back where the key was pressed (`ChoiceMenu`).
 *
 * They register into the `issue` scope the view row pushes (`useIssueKeymap.ts`); this hook
 * pushes none. The digits go through the header's one priority write, the one its menu and the
 * palette's Set priority rows use, so a refusal of any of them is said once, by the header.
 */
export function useHeaderKeys(priorityWrite: IssuePriorityWrite): void {
  useKeymap("issue", [
    {
      action: "issue.status",
      run: () => usableControl(STATUS_MENU)?.click(),
      when: () => usableControl(STATUS_MENU) !== null,
    },
    {
      action: "issue.priority",
      run: () => usableControl(PRIORITY_MENU)?.click(),
      when: () => usableControl(PRIORITY_MENU) !== null,
    },
    {
      // While the menu is open the focus is in it, whose keys are its own, so a digit there is
      // the menu's pick and never this second write.
      action: "issue.set-priority",
      run: (event) => priorityWrite.submit(Number(event.key) as IssuePriority),
      when: () => usableControl(PRIORITY_MENU) !== null && outsideInputsAndAskCards(),
    },
    {
      // The heading's click opens the editor, and a focus does not, so the key clicks it: a
      // heading that already holds the focus - where Escape and Enter leave it - takes no new
      // focus. The selector finds the heading only while the issue is open and not edited
      // (otherwise its `tabIndex` is -1), so the key is offered exactly while a click opens it.
      action: "issue.title",
      run: () => usableControl(TITLE_HEADING)?.click(),
      when: () => usableControl(TITLE_HEADING) !== null,
    },
    {
      // Held while its write is out (`heldPress`), when it takes no press, and so no key.
      action: "issue.pin",
      run: () => usableControl(PIN_TOGGLE)?.click(),
      when: () => usableControl(PIN_TOGGLE) !== null && outsideInputsAndAskCards(),
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
      action: "issue.approve",
      run: () => focusedApprovalAction("approve")?.click(),
      when: () => focusedApprovalAction("approve") !== null,
    },
    {
      action: "issue.request-changes",
      run: () => focusedApprovalAction("request-changes")?.click(),
      when: () => focusedApprovalAction("request-changes") !== null,
    },
  ]);
}
