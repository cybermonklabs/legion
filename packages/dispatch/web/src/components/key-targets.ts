/**
 * What an element is to a key pressed on it or a `change` fired on it: whether it takes typed
 * text, whether its `change` is the commit on blur of that text, the mark of an open panel, and
 * the mark of a menu's trigger, on which a typed character is the page's key; and what a key is to
 * a text field: one that only moves (`MOVING_KEYS`), or one that writes (`writesText`). The keymap
 * reads them to leave a control its own keys (`isEditableTarget`, `features/shell/keymap.ts`) and
 * to keep a held key's repeats from the page (`Keymap.captureKeyDown`), the shared list to leave
 * the page its keys (`MultiSelect`), the composer to hold its field while a send is out
 * (`MentionComposer`), and the page's focus hand-offs to tell the reader's press from an edit
 * (`focus-place.ts`'s `isPress`). A module of its own with no import, so the panel and the focus
 * hand-offs read them without loading the keymap. A browser row that imports a control's name
 * from a module that draws a `MultiSelect` (`e2e/issue-page-h.e2e.ts` reads `repoControlName`)
 * loads the keymap all the same, through the trigger's key hint (`ShortcutHelp`); that is safe in
 * Playwright's process, since the keymap reads `import.meta.env` only where it is defined
 * (`appKeymap`).
 */

/** The attribute every open panel's element carries (`AnchoredPanel` writes it): what tells the
 *  keymap a key was pressed inside one (`isEditableTarget`), and the page's focus hand-offs that
 *  the panel is a region of its own (`focus-place.ts`). */
export const PANEL_ATTRIBUTE = "data-anchored-panel";

/** The attribute the trigger of a menu carries - a single pick in a menu's place, `MultiSelect`'s
 *  `menu`: the Inbox's Snooze, the priority and the status - which `MultiSelect` writes: what
 *  tells the keymap and the Inbox's keys that a key was pressed on one (`typedOnMenuTrigger`). */
export const MENU_TRIGGER_ATTRIBUTE = "data-list-menu";

/** A menu's trigger (`MENU_TRIGGER_ATTRIBUTE`). */
export const MENU_TRIGGER_SELECTOR = `[${MENU_TRIGGER_ATTRIBUTE}]`;

/**
 * Whether `press` is a character typed on a menu's trigger while its menu is closed - one
 * character other than the space, with no Ctrl or Cmd - which is the page's key and no text
 * (dispatch://INFRA-2160, R4 and R5): a menu's trigger is the button of a menu, not a select. So
 * `b` or `h` there opens the Snooze menu of the item in hand, `p` the priority menu and `s` the
 * status menu, each on its first choice or its value with nothing typed, as the key does from the
 * row or the page, and every other such key does what it does there. The trigger keeps a button's
 * keys and the arrows - Enter, Space, ArrowDown and ArrowUp open its menu - and an open menu's
 * trigger keeps every key. The one rule of both sides: the trigger opens nothing and types nothing
 * on such a key (`MultiSelect`'s `openFromKey`), and the keymap reads the trigger as no control
 * that takes the key (`isEditableTarget`, `features/shell/keymap.ts`). The trigger of every other
 * list - the assignee, the repository, an ask's hand-on, a filter's fields - takes a select's
 * keys, and a character typed on it opens its list with the character in the box.
 */
export function typedOnMenuTrigger(
  target: EventTarget | null,
  press: { readonly ctrlKey: boolean; readonly key: string; readonly metaKey: boolean }
): boolean {
  return (
    target instanceof Element &&
    target.matches(MENU_TRIGGER_SELECTOR) &&
    target.getAttribute("aria-expanded") !== "true" &&
    press.key.length === 1 &&
    press.key !== " " &&
    !press.ctrlKey &&
    !press.metaKey
  );
}

/** The keys that only move the focus, the caret or the view: a send's hold on the composer lets
 *  them through, so the reader can still move about the draft and leave the composer by keyboard
 *  (`MentionComposer`), and a held one repeats on whatever control holds the focus now, so a held
 *  arrow walks the list a press of it opened (`Keymap.captureKeyDown`). */
export const MOVING_KEYS: Readonly<Record<string, true>> = {
  ArrowDown: true,
  ArrowLeft: true,
  ArrowRight: true,
  ArrowUp: true,
  End: true,
  Home: true,
  PageDown: true,
  PageUp: true,
  Tab: true,
};

/** Whether a key would write into a text field: held with no Ctrl or Cmd, a character, Enter,
 *  Backspace or Delete. A field an offer holds read only takes none of them, and says why
 *  (`KeptDraftOffers`' `typedOver`), and only such a key repeats in the field that had the focus
 *  before its press (`Keymap.captureKeyDown`). */
export function writesText(press: {
  readonly ctrlKey: boolean;
  readonly key: string;
  readonly metaKey: boolean;
}): boolean {
  const { key } = press;
  if (press.ctrlKey || press.metaKey) return false;
  return key.length === 1 || key === "Enter" || key === "Backspace" || key === "Delete";
}

/** The `<input>` types whose value is typed text alone, so that their `change` is the commit on
 *  blur of what the reader typed, which Chromium fires as an update takes the focused field away.
 *  `TEXT_INPUT_TYPES` holds these and the date and time types besides, since a digit typed in
 *  one of those edits its value too; but a date or time box fires its `change` at the reader's
 *  pick - in its picker, or with the key that completes the value - as a `<select>` or a
 *  checkbox does, and not on blur (`commitsOnBlur`). */
const COMMIT_ON_BLUR_TYPES: Record<string, true> = {
  email: true,
  number: true,
  password: true,
  search: true,
  tel: true,
  text: true,
  url: true,
};

/** The `<input>` types that take typed text: those whose value is typed text alone
 *  (`COMMIT_ON_BLUR_TYPES`), and the date and time types, whose value a digit typed edits.
 *  Everything else an input can be - checkbox, radio, the button family, file, range, color -
 *  takes no text, so a letter or digit pressed while one has focus on the page is a shortcut, not
 *  typing: ticking a checkbox with the pointer must not silence the page's keys.
 *  `HTMLInputElement.type` normalises a missing or unknown attribute to `text`. */
const TEXT_INPUT_TYPES: Record<string, true> = {
  ...COMMIT_ON_BLUR_TYPES,
  date: true,
  "datetime-local": true,
  month: true,
  time: true,
  week: true,
};

/** Whether `element` takes typed text: a content-editable element, an `<input>` of a text-entry
 *  type (`TEXT_INPUT_TYPES`), or a `<textarea>`. A key pressed on one is the control's own
 *  (`isEditableTarget`), and a character typed in one is no press of the reader's
 *  (`focus-place.ts`'s `isPress`), since it only edits the field. A walk-back that hands the
 *  focus on passes over every such control (`focus-place.ts`'s `walkBack`), since the reader's
 *  next key would go into it. A date or time box plays two parts: it takes the
 *  reader's keys, so a walk-back passes over it as over a text box, and its `change` and `input`
 *  are the reader's pick, so they count as a press (`isPress`), as a select's do. */
export function takesText(element: Element): boolean {
  if (element instanceof HTMLElement && element.isContentEditable) {
    return true;
  }
  // An `<input>` takes typed text only in the text-entry types; a checkbox, radio or button
  // takes none.
  if (element instanceof HTMLInputElement) {
    return TEXT_INPUT_TYPES[element.type] === true;
  }
  return element.tagName === "TEXTAREA";
}

/** Whether a `change` or an `input` on `element` is typing or its commit on blur, rather than the
 *  reader's pick: on an `<input>` whose value is typed text alone (`COMMIT_ON_BLUR_TYPES`), a
 *  `<textarea>`, or a content-editable element. Every other control fires both at the pick - a
 *  `<select>`, a checkbox, a radio, a file, range or color input, a date or time box - so a
 *  component's focus hand-off reads either as a press of the reader's (`focus-place.ts`'s
 *  `isPress`). An `<input>` of the button family or of type hidden fires neither; it is answered
 *  false, so one sent to it would read as a pick. */
export function commitsOnBlur(element: Element): boolean {
  if (element instanceof HTMLInputElement) {
    return COMMIT_ON_BLUR_TYPES[element.type] === true;
  }
  // Off an `<input>`, an element takes typed text (`takesText`) exactly where a `change` on it is
  // the commit on blur: a `<textarea>` or a content-editable element.
  return takesText(element);
}
