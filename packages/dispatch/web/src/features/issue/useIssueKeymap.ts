import { useLocation, useNavigate } from "react-router-dom";

import type { IssuePriority } from "../../api/types";
import { outsideAskCard } from "../inbox/ask-card";
import { buildIssuePath, type IssueTab, parseIssuePath } from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { useOpenDetailsAt } from "./details-rows";
import { WORDS_FILTER_ATTRIBUTE } from "./IssuePresets";
import { bringOnScreen, FILTER_ROW_ATTRIBUTE } from "./identity-block";
import { issueAddress, presetPath } from "./issue-address";
import { CONTROL_LABELS, PRESET_NAMES, type Preset } from "./page-words";
import type { IssuePriorityWrite } from "./useIssuePriority";

/**
 * What the `issue` scope acts on. Every target is a control the header already renders - the
 * pin inside its overflow menu, which is drawn hidden while shut - so a key does exactly what a
 * click on it does. Scoping the lookup to `[data-issue-header]` keeps a child issue's row in the
 * outline's children group - it carries the same accessible names - out of reach. The labels'
 * control is Details' (`labels` below).
 */
const HEADER = "[data-issue-header]";
const LABELS_TRIGGER = 'button[aria-label="Edit labels"]';
const PIN_TOGGLE = 'button[data-issue-action="pin"]';
const PRIORITY_SELECT = 'select[aria-label^="Priority of "]';
const STATUS_SELECT = 'select[aria-label="Status"]';
const TITLE_HEADING = 'h1[tabindex="0"]';

/** The header control `selector` names, or `null` when it is absent or refuses input (a closed
 *  issue, a save in flight): the one fact both the key's `run` and the `when` that offers it read,
 *  so a shortcut is listed exactly while its control would take a click. */
function usableControl(selector: string): HTMLElement | null {
  const node = document.querySelector(HEADER)?.querySelector<HTMLElement>(selector);
  if (node == null) {
    return null;
  }
  const disabled =
    (node instanceof HTMLButtonElement || node instanceof HTMLSelectElement) && node.disabled;
  return disabled ? null : node;
}

/**
 * The line at the outline's foot while it answers a question (`IssueComposer.tsx`'s
 * `AnswerLine`): the question's options are its chips there, which the keyboard reaches, so its
 * controls own the keys as an ask card's do.
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
 * fires from one: a digit has no native meaning on a button, and after any click - a filter, a
 * disclosure, Copy - focus sits on the button that was clicked, which is where a reader reaching
 * for the next shortcut stands. Withholding these keys there would make them work only from the
 * page's own background. An ask card's buttons and the answer line's are the exception, and
 * that is what `outsideAskCard` (`inbox/ask-card.ts`, the one definition both pages read) and
 * `ANSWER_LINE` cover: there, the question's controls own the keys. `l` and `e` open a control
 * rather than write, so a stray press is visible and undoable and they are offered throughout.
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
 * The parts of the page a chord brings the reader to, by the mark the page draws each one
 * with. They are the marks `e2e/issue-page.ts` reads the page by, so the keyboard and the
 * checks look for the same element and neither has a name of its own for a part of the page.
 */
const PARTS = {
  // The children's group row of the outline (ruling 5 of
  // dispatch://INFRA-1196/artifact/build-rulings-md), which takes the focus itself.
  children: '[data-node-row][data-node-type="child_issue"]',
  // The collaborators' row of Details (`IssueMargin.tsx`), whose own control opens the list.
  collaborators: '[data-details-row="collaborators"]',
  // The filter by words: its small control, which a press opens into the box, or the box
  // (`IssuePresets.tsx`); at 760 px and below both are in the Outline sheet.
  filter: `[${WORDS_FILTER_ATTRIBUTE}]`,
  // The issue's history, in Details (`IssueHistory.tsx`), which takes the focus itself.
  history: '[data-testid="issue-history"]',
  menu: '[role="menu"]',
  // The outline, by its name: the Contents pane's list is a tree too (`IssueContents.tsx`), and
  // it stands before the outline in the page.
  tree: `[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`,
} as const;

/** What takes the focus inside each part, where the part itself does not: the tree's one tab
 *  stop, the collaborators' own control, and a menu's first item once it holds any
 *  (`dispatch://INFRA-1196/artifact/plan-md#b-t18`). */
const TAKES_FOCUS: Readonly<Record<keyof typeof PARTS, string | undefined>> = {
  children: undefined,
  collaborators: "button",
  filter: undefined,
  history: undefined,
  menu: '[role="menuitem"]',
  tree: '[data-node-row][tabindex="0"]',
};

/** The Outline control of the filter row, which holds the filter by words at 760 px and below:
 *  `t f` opens it where the words control is not drawn. */
const OUTLINE_CONTROL = `[${FILTER_ROW_ATTRIBUTE}] button[aria-haspopup="dialog"]`;

/** How many frames a chord waits for the page to draw what its address opened before it gives
 *  up on putting the focus there: a navigation renders on the next frame, and a panel that
 *  mounts its content takes a few more. */
const DRAWING_FRAMES = 20;

/**
 * How many frames a chord keeps what it showed on screen after it showed it. What a chord
 * opens mounts over several commits and some of it places its own reader as it does: the
 * history in Details brings the entry an address names into view once its log has been read
 * (`IssueHistory.tsx`), which scrolls the box the chord had just scrolled, leaving the control
 * the chord left the focus on above the screen. The chord is the reader's own request and the
 * later intent, so it is what stands - and only until the reader moves the focus off that
 * control, which is them moving on.
 */
const SETTLING_FRAMES = 20;

/** Shows `shown`, which is `found` where the whole part fits the screen and the control the
 *  chord leaves the focus on where it does not. */
function showPart(found: HTMLElement, takes: HTMLElement): HTMLElement {
  const whole = found.getBoundingClientRect().height <= window.innerHeight;
  const shown = whole ? found : takes;
  shown.scrollIntoView({ block: whole ? "start" : "center" });
  bringOnScreen(shown);
  return shown;
}

/**
 * Brings the part of the page `part` names on screen and puts the focus in it, once the page
 * has drawn it: a chord navigates, and what it opens is on the page a frame or two later (R9,
 * c-keyboard, which reads both the box and the focus). A part that is mounted inside a hidden
 * element is not drawn yet either: the panel is in Details, which the conversation's address
 * opens a commit after the address lands (dispatch://INFRA-1196 R14), and a hidden control takes
 * no focus.
 */
function reachPart(part: keyof typeof PARTS, left = DRAWING_FRAMES): void {
  const found = document.querySelector<HTMLElement>(PARTS[part]);
  if (found === null || found.closest("[hidden]") !== null) {
    // The words are in the Outline sheet where the row has no room for them: the chord opens
    // the sheet, as the reader's press would, and goes on to the words once it is drawn.
    if (part === "filter" && left === DRAWING_FRAMES) {
      document.querySelector<HTMLElement>(OUTLINE_CONTROL)?.click();
    }
    if (left > 0) requestAnimationFrame(() => reachPart(part, left - 1));
    return;
  }
  // The words' small control opens into the box, which is what takes the caret.
  if (part === "filter" && found instanceof HTMLButtonElement) {
    found.click();
    if (left > 0) requestAnimationFrame(() => reachPart(part, left - 1));
    return;
  }
  const inside = TAKES_FOCUS[part];
  const takes = inside === undefined ? found : (found.querySelector<HTMLElement>(inside) ?? found);
  // The focus is taken without scrolling to it, and what the chord opens is then shown from its
  // own top: a control inside a panel that scrolls of its own would otherwise leave the panel's
  // head above the screen (R9, c-keyboard reads the box). A part taller than the screen cannot
  // be shown whole - the page's panel is a full-height column (`margin/MarginSheet.tsx`'s
  // `h-dvh`) - so there the chord shows the control it leaves the reader on instead, and shows
  // it in the middle of the screen rather than at the panel's own top, which is the one place
  // in that column the page's sticky identity block reaches across.
  takes.focus({ preventScroll: true });
  showPart(found, takes);
  keepPartOnScreen(found, takes, SETTLING_FRAMES);
}

/** Shows the part again, for `left` frames, whenever what the chord showed has been scrolled
 *  off the screen by the page drawing itself. The focus leaving the control the chord placed
 *  ends it: from there the reader is moving by their own keys and the page follows them. */
function keepPartOnScreen(found: HTMLElement, takes: HTMLElement, left: number): void {
  if (left <= 0) return;
  requestAnimationFrame(() => {
    if (document.activeElement !== takes) return;
    const whole = found.getBoundingClientRect().height <= window.innerHeight;
    const box = (whole ? found : takes).getBoundingClientRect();
    if (box.top < 0 || box.bottom > window.innerHeight) showPart(found, takes);
    keepPartOnScreen(found, takes, left - 1);
  });
}

/**
 * The address a filter's chord opens from the view `active`: the filter itself, or, where that
 * filter is the one on, the page with every filter off - the chord toggles it, as pressing the
 * filter's own control does (R2).
 */
function toggled(key: string, filter: Preset, active: Preset): string {
  return presetPath(key, filter === active ? "conversation" : filter);
}

/**
 * Every chord the page binds, which is the `t` table of the spec's Design section, chord for
 * chord. Each one opens an address and puts the focus in what that address draws: the nine
 * chords bound before the redesign keep a target each (R11), and `t f` is the one the table
 * adds.
 *
 * `t s`, `t k`, `t p` and `t d` toggle the Spec, Verification, Plan and Decisions filters, and
 * `t c` is the conversation, the page with every filter off; each leaves the focus in the
 * outline. A label names what the chord does on this page, in the filter row's own names
 * (`PRESET_NAMES`), and the panel, the children section and the node menu as the page names
 * them.
 */
const CHORDS: readonly {
  keys: string;
  label: string;
  /** The address the chord opens from the view on, or undefined for a chord that opens none. */
  path?: (key: string, active: Preset) => string;
  part: keyof typeof PARTS;
  /** What names the binding, which is the tab the address was that tab's. */
  tab: IssueTab | "filter";
}[] = [
  {
    keys: "t s",
    label: `Turn the ${PRESET_NAMES.spec} filter on or off`,
    part: "tree",
    path: (key, active) => toggled(key, "spec", active),
    tab: "spec",
  },
  // `t v` is the activity and `t c` the conversation, so the checks take their own `k`; `t p`
  // is free (`p` alone focuses the priority, which is a different scope of key).
  {
    keys: "t k",
    label: `Turn the ${PRESET_NAMES.verification} filter on or off`,
    part: "tree",
    path: (key, active) => toggled(key, "verification", active),
    tab: "verification",
  },
  {
    keys: "t p",
    label: `Turn the ${PRESET_NAMES.plan} filter on or off`,
    part: "tree",
    path: (key, active) => toggled(key, "plan", active),
    tab: "plan",
  },
  {
    keys: "t d",
    label: `Turn the ${PRESET_NAMES.decisions} filter on or off`,
    part: "tree",
    path: (key, active) => toggled(key, "decisions", active),
    tab: "decisions",
  },
  {
    keys: "t c",
    label: `${PRESET_NAMES.conversation}: turn every filter off`,
    part: "tree",
    path: (key) => presetPath(key, "conversation"),
    tab: "conversation",
  },
  // `t a` opens the node menu and `l` is the issue's label editor, so the history takes `t v`.
  // `/activity` opens Details at the issue's history (R15, R20), which takes the focus.
  {
    keys: "t v",
    label: `Open the issue's ${CONTROL_LABELS.history.toLowerCase()} in ${CONTROL_LABELS.details}`,
    part: "history",
    path: (key) => buildIssuePath({ key, kind: "activity" }),
    tab: "activity",
  },
  {
    keys: "t h",
    label: "Move to the issue's children",
    part: "children",
    path: (key) => buildIssuePath({ key, kind: "children" }),
    tab: "children",
  },
  {
    keys: "t a",
    label: `Open the ${CONTROL_LABELS.other_nodes} menu`,
    part: "menu",
    path: (key) => buildIssuePath({ key, kind: "artifacts" }),
    tab: "artifacts",
  },
  // `t o`, the collaborators, in the panel: `/collaborators` opens Details at the People
  // section with the list drawn (R20, `IssueMargin.tsx`), and the focus goes to its control.
  {
    keys: "t o",
    label: "Open the issue's collaborators in Details",
    part: "collaborators",
    path: (key) => buildIssuePath({ key, kind: "collaborators" }),
    tab: "collaborators",
  },
  // The filter by words narrows the view the reader is already on, so it opens no address of
  // its own: what it types is `?q=`, which the box itself writes.
  { keys: "t f", label: "Filter by words", part: "filter", tab: "filter" },
];

/**
 * Every key the issue page binds in its own `issue` scope, and the scope itself.
 *
 * The scope is the issue page; every binding drives a control the identity block or the tablist
 * already draws - the same handler a click reaches, offered exactly while that control would
 * take one. The digits go through the page's one priority write, so a keyed priority lands in
 * the same cache the picker writes. `IssueHeader` registers the palette-only issue actions into
 * this same scope (`useIssuePalette.ts`); neither pushes a scope of its own.
 */
export function useIssueKeymap(priorityWrite: IssuePriorityWrite, issueKey: string): void {
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const openDetailsAt = useOpenDetailsAt();
  // The view the address has on, which a filter's chord toggles from.
  const route = parseIssuePath(pathname);
  const active =
    route === undefined ? "conversation" : issueAddress(route, "", "", undefined).preset;
  useKeymapScope("issue");
  useKeymap("issue", [
    {
      id: "status",
      keys: "s",
      label: "Focus the status",
      run: () => usableControl(STATUS_SELECT)?.focus(),
      when: () => usableControl(STATUS_SELECT) !== null,
    },
    {
      id: "priority",
      keys: "p",
      label: "Focus the priority",
      run: () => usableControl(PRIORITY_SELECT)?.focus(),
      when: () => usableControl(PRIORITY_SELECT) !== null,
    },
    {
      id: "set-priority",
      keys: ["0", "1", "2", "3"],
      label: "Set priority P0–P3",
      run: (event) => priorityWrite.submit(Number(event.key) as IssuePriority),
      when: () => usableControl(PRIORITY_SELECT) !== null && outsideInputsAndAskCards(),
    },
    {
      // The labels' control is in Details' Labels row (dispatch://INFRA-1196 R13): the key
      // opens Details there and presses it, as a reader does.
      id: "labels",
      keys: "l",
      label: "Edit labels",
      run: () => openDetailsAt("labels", true),
      when: () => {
        const control = document.querySelector(`[data-details-row="labels"] ${LABELS_TRIGGER}`);
        return control instanceof HTMLButtonElement && !control.disabled;
      },
    },
    {
      // The heading takes focus only while the issue is open: closed, its `tabIndex` is -1 and
      // `onFocus` does not open the editor, so the key is offered exactly while it edits.
      id: "title",
      keys: "e",
      label: "Edit the title",
      run: () => usableControl(TITLE_HEADING)?.focus(),
      when: () => usableControl(TITLE_HEADING) !== null,
    },
    {
      id: "pin",
      keys: "Shift+P",
      label: "Pin or unpin the issue",
      run: () => usableControl(PIN_TOGGLE)?.click(),
      when: () => usableControl(PIN_TOGGLE) !== null && outsideInputsAndAskCards(),
    },
    {
      // `s` and `p` put focus in a native select, where every single-key binding is suspended
      // and the next keystroke is the select's own type-ahead - `t` would pick Todo or Testing
      // rather than start the tab chord. Escape is the way back out, as it is on the Inbox
      // rows, the board's cards and the architecture rows.
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back out of the focused header control",
      run: () => (document.activeElement as HTMLElement | null)?.blur(),
      when: () => {
        const active = document.activeElement;
        return active === usableControl(STATUS_SELECT) || active === usableControl(PRIORITY_SELECT);
      },
    },
    ...CHORDS.map(({ keys, label, part, path, tab }) => ({
      id: `tab-${tab}`,
      keys,
      label,
      run: () => {
        if (path === undefined) {
          reachPart(part);
          return;
        }
        navigate(path(issueKey, active));
        // The part is looked for once the address has drawn: the outline is the same element
        // under every view, and a row of the view being left would take the focus and then be
        // unmounted with it - a filter draws its rows at another depth than the outline does.
        requestAnimationFrame(() => reachPart(part));
      },
    })),
  ]);
}
