import { useLocation, useNavigate } from "react-router-dom";

import { buildIssuePath, type IssueTab, parseIssuePath } from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { DOCUMENT_OPEN_ATTRIBUTE, DOCUMENTS_LIST_ATTRIBUTE } from "./DocumentsList";
import { useOutlineAddress } from "./document-address";
import { OUTLINE_CONTROL, WORDS_FILTER_ATTRIBUTE } from "./IssuePresets";
import { bringOnScreen } from "./identity-block";
import { issueAddress, presetPath } from "./issue-address";
import { wholeOnScreen } from "./on-screen";
import { CONTROL_LABELS, PRESET_NAMES, type Preset } from "./page-words";

/**
 * The parts of the page a chord brings the reader to, by the mark the page draws each one
 * with. They are the marks `e2e/issue-page.ts` reads the page by, so the keyboard and the
 * checks look for the same element and neither has a name of its own for a part of the page.
 */
const PARTS = {
  // Documents' list, which `t a` opens in place of the outline (part N's R2, R4).
  documents: `[${DOCUMENTS_LIST_ATTRIBUTE}]`,
  // The filter by words: its small control, which a press opens into the box, or the box
  // (`IssuePresets.tsx`); below 1024 px both are in the Outline sheet (part N's R4).
  filter: `[${WORDS_FILTER_ATTRIBUTE}]`,
  // The outline, by its name: the Contents pane's list is a tree too, and it stands before the
  // outline in the page.
  tree: `[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`,
} as const;

/** What takes the focus inside each part, where the part itself does not: the tree's one tab
 *  stop. */
const TAKES_FOCUS: Readonly<Record<keyof typeof PARTS, string | undefined>> = {
  // Documents' list's first row, by its link to its document; the list itself where it holds no
  // row.
  documents: `[${DOCUMENT_OPEN_ATTRIBUTE}]`,
  // The words box is its own focus.
  filter: undefined,
  tree: '[data-node-row][tabindex="0"]',
};

/** How many frames a chord waits for the page to draw what its address opened before it gives
 *  up on putting the focus there: a navigation renders on the next frame, and a panel that
 *  mounts its content takes a few more. */
const DRAWING_FRAMES = 20;

/**
 * How many frames a chord keeps what it showed on screen after it showed it. What a chord
 * opens mounts over several commits and some of it places its own reader as it does, which can
 * scroll what the chord had just shown off the screen. The chord is the reader's own request and
 * the later intent, so it is what stands - and only until the reader moves the focus off the
 * control the chord left it on, which is them moving on.
 */
const SETTLING_FRAMES = 20;

/** Shows `shown`, which is `found` where the whole part fits the screen and the control the
 *  chord leaves the focus on where it does not. */
function showPart(found: HTMLElement, takes: HTMLElement): HTMLElement {
  // A control the chord left the focus on that the reader already sees whole stays where it
  // stands, and the page with it (part N's R1): a chord back to the outline (`t c`) finds the
  // outline at the place the reader left it, the row they were on in view (`stage-landing.tsx`'s
  // `ViewNodeKeeper`), which showing it again would move to the middle of the screen.
  if (wholeOnScreen(takes)) return takes;
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
 * element is not drawn yet either, and a hidden control takes no focus. A page that draws no
 * such part leaves the focus where it was.
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
  // own top. A part taller than the screen cannot be shown whole, so there the chord shows the
  // control it leaves the reader on instead, in the middle of the screen.
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
 * The address a view's chord opens from the view `active`: the view itself, or, where that view
 * is the one on, the page with every view off - the chord toggles it, as pressing the view's own
 * control does (R2).
 */
function toggled(key: string, view: Preset, active: Preset): string {
  return presetPath(key, view === active ? "conversation" : view);
}

/**
 * The chords of the view row, which are the `t` table of the spec's Design section for the five
 * views. Each one opens an address and puts the focus in what that address draws.
 *
 * `t s`, `t k`, `t p` and `t d` toggle the Spec, Verification, Plan and Decisions views, and
 * `t c` is the conversation, the page with every view off; each leaves the focus in the
 * outline. A label names what the chord does on this page, in the view row's own names
 * (`PRESET_NAMES`).
 */
const CHORDS: readonly {
  keys: string;
  label: string;
  /** The address the chord opens from the view on. */
  path: (key: string, active: Preset) => string;
  part: keyof typeof PARTS;
  /** What names the binding, which is the tab the address was that tab's. */
  tab: IssueTab;
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
];

/**
 * The `issue` scope of the new issue page, and the chords of its view row in it.
 *
 * The scope is the issue page: the page's other keys bind into this same scope as the parts that
 * draw what they act on land, and none pushes a scope of its own.
 */
export function useIssueKeymap(issueKey: string): void {
  const navigate = useNavigate();
  const { pathname } = useLocation();
  // Where `t a` and `t f` leave from (part N's R4).
  const { search } = useLocation();
  const outline = useOutlineAddress(issueKey);
  // The view the address has on, which a view's chord toggles from.
  const route = parseIssuePath(pathname);
  const active =
    route === undefined ? "conversation" : issueAddress(route, "", "", undefined).preset;
  useKeymapScope("issue");
  useKeymap(
    "issue",
    CHORDS.map(({ keys, label, part, path, tab }) => ({
      id: `tab-${tab}`,
      keys,
      label,
      run: () => {
        navigate(path(issueKey, active));
        // The part is looked for once the address has drawn: the outline is the same element
        // under every view, and a row of the view being left would take the focus and then be
        // unmounted with it - a view draws its rows at another depth than the outline does.
        requestAnimationFrame(() => reachPart(part));
      },
    }))
  );
  // `t a`, today's Artifacts tab, opens Documents' list at its own address (part N's R2, R4,
  // R6), with the focus on its first row, as a press of Documents does: the list goes back to the
  // address the chord left on Escape (the location's `outline`), and from the list itself to
  // where the list was opened from.
  useKeymap("issue", [
    {
      id: "tab-artifacts",
      keys: "t a",
      label: CONTROL_LABELS.open_documents,
      run: () => {
        navigate(presetPath(issueKey, "documents"), {
          state: { outline: active === "documents" ? outline : `${pathname}${search}` },
        });
        requestAnimationFrame(() => reachPart("documents"));
      },
    },
  ]);
  // The filter by words narrows the view the reader is already on, so it opens no address of
  // its own: what it types is `?q=`, which the box itself writes (part N's R4).
  useKeymap("issue", [
    {
      id: "tab-filter",
      keys: "t f",
      label: "Filter by words",
      run: () => {
        // Where the row draws no Filter, in the row or in its Outline sheet - on Documents, which
        // draws no outline - the chord turns Conversation on with the words kept, and opens its
        // box there (part N's R4, ST-1).
        if (
          document.querySelector(PARTS.filter) === null &&
          document.querySelector(OUTLINE_CONTROL) === null
        ) {
          const words = new URLSearchParams(search).get("q") ?? "";
          const conversation = presetPath(issueKey, "conversation");
          navigate(
            words === "" ? conversation : `${conversation}?${new URLSearchParams({ q: words })}`
          );
          requestAnimationFrame(() => reachPart("filter"));
          return;
        }
        reachPart("filter");
      },
    },
  ]);
  // `t e`, today's Steps tab, opens `/steps`, which opens Details at the issue's steps and puts
  // the focus there (`stage-landing.tsx`): each press is a new arrival at it, so it opens them
  // again after the reader closed Details (part N's R6).
  useKeymap("issue", [
    {
      id: "tab-steps",
      keys: "t e",
      label: CONTROL_LABELS.open_steps,
      run: () => navigate(buildIssuePath({ key: issueKey, kind: "steps" })),
    },
  ]);
}
