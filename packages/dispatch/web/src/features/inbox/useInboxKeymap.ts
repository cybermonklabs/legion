import {
  type FocusEvent,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
} from "react";
import { flushSync } from "react-dom";

import { openerOf } from "../../components/focus-place";
import { openReplyBox, REPLY_BOX } from "../conversation/turn-keys";
import { ITEM_COPY_REF_SELECTOR } from "../refs/CopyRefButton";
import { type KeymapScope, useKeymap, useKeymapScope } from "../shell/keymap";
import type { KeymapActionId } from "../shell/keymap-catalogue";
import { openLinkInNewTab } from "../shell/new-tab";
import { closestMatching, focusedMatching, roveFocus } from "../shell/roving";
import type { AskApprovalAction } from "./AskOptionRow";
import { outsideAskCard } from "./ask-card";
import { type InboxActionId, inboxActionSelector } from "./inbox-action-mark";
import { ROW_SELECTOR, rowIn, rowsIn } from "./inbox-row";
import { type InboxListRow, rowId } from "./list-row";
import { optionIndex } from "./option-digits";
import { PANE_READER_MOVE } from "./pane-placement";
import type { PaneSelection } from "./pane-selection";
import { dismissRow, type InboxDismissRow } from "./useInboxDismiss";

/** The bulk bar's Snooze menu: `b` and `h` press it while rows are marked. */
const BULK_PICKER_SELECTOR = "[data-inbox-bulk-snooze]";

/** The reading column's own scroller: `l` hands it focus and Escape takes it back to the list. */
const PANE_SCROLLER_SELECTOR = '[data-testid="inbox-pane-scroller"]';

/** The "Add a note or answer in your own words" disclosure an `AskCard` folds its answer box
 *  behind in its compact variant - the card a decision block hosts, which is what the reading
 *  column shows for a question written as one - while it is still shut. The card carries the
 *  handle (`AskCard.tsx`), as it carries `data-ask-answer` and `data-ask-option`. */
const NOTE_DISCLOSURE_SELECTOR = '[data-ask-note-disclosure][aria-expanded="false"]';

/** An option of an ask card, Other included: the inputs the digits pick (`AskOptionRow.tsx`). */
const OPTION_SELECTOR = "[data-ask-option]";

/** How far the document's Space and Shift+Space scroll the column, as a part of its height: the
 *  step Chromium takes for a page itself (`kMinFractionToStepWhenPaging`, in Chromium's
 *  `cc/input/scroll_utils.h`). */
const PAGE_STEP = 0.875;

/** The controls a press of Space works itself: a button, a link, a disclosure's summary, a
 *  checkbox, a radio and a menu's trigger. With one of them holding the focus the document's
 *  Space and Shift+Space stand down (R3), since the registry calls `preventDefault` for every
 *  binding that fires and would take the press from the control. */
const OWN_SPACE_SELECTOR =
  'button, a[href], summary, input[type="checkbox"], input[type="radio"], [aria-haspopup]';

/** The row that holds keyboard focus itself — not one merely containing a focused control. */
function focusedRow(): HTMLElement | null {
  return focusedMatching(ROW_SELECTOR);
}

function rowAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, ROW_SELECTOR);
}

/** Whether `node` is in the reading column `column`: inside it, or inside a panel a control of
 *  it opened - the head's Snooze and priority menus, a card's hand-on list - which stands over
 *  the page, portalled out of the column, and hands the focus back into it as it closes. Such a
 *  panel is the one a control of the column names as what it opened (`aria-controls`). */
function inColumn(column: HTMLElement | null, node: EventTarget | null): boolean {
  return (
    column !== null &&
    node instanceof Element &&
    (column.contains(node) || column.contains(openerOf(node)))
  );
}

/** What the Inbox's keys read of the list they act on, and the presses they make that are the
 *  list's own. */
export interface InboxKeyboard {
  /** Whether the Filter panel or the display menu stands open: Escape is a panel's while one
   *  does, never the selection's. */
  readonly barOpen: boolean;
  /** The bar of selected rows, wherever the header draws it. */
  readonly bulkBarRef: RefObject<HTMLElement | null>;
  /** Whether the bar of selected rows is shown, which is while its Clear selection is. */
  readonly bulkBarShown: boolean;
  /** Unmarks every marked row: the bar's Clear selection. */
  readonly clearSelection: () => void;
  /** Dismisses, or moves to the Inbox, the rows `named` and hands the reader on to the item
   *  after them (R8): what `e` does to the row in hand. */
  readonly dismiss: (named: readonly InboxDismissRow[]) => void;
  /** The list's root, where the rows are looked up. */
  readonly listRef: RefObject<HTMLElement | null>;
  /** Marks or unmarks a row, by its own `rowId`. */
  readonly onMark: (id: string) => void;
  /** The reading column's selection (`usePaneSelection`). */
  readonly paneSelection: PaneSelection;
  /** Whether the reading column is on: the selection is then what the keys act on, else the row
   *  holding the focus. */
  readonly paneShown: boolean;
  /** The rows the list shows, by `rowId`. */
  readonly rowsById: ReadonlyMap<string, InboxListRow>;
  /** The scope the keys bind under: the page's own, or the drawer's `dialog`. */
  readonly scope: KeymapScope;
  /** The search box, which `f` lands the reader's focus in from anywhere on the page. */
  readonly searchRef: RefObject<HTMLInputElement | null>;
  /** The asks among the marked rows, which `b` and `h` snooze together. */
  readonly selectedAsks: readonly string[];
}

/** What the column's wrapper takes from the keys: the record Escape reads lives here, and the
 *  wrapper reports the two ways out of the column no event of the document reports. */
export interface InboxColumnHold {
  /** The wrapper's `onBlur`: the drop a focus that lands nowhere makes. */
  readonly forgetPaneReturn: (event: FocusEvent<HTMLDivElement>) => void;
  /** The wrapper's `ref`: the node the keys act in, and the drop its unmount makes. */
  readonly holdPane: (node: HTMLDivElement | null) => void;
}

/**
 * The Inbox's keyboard: the scope it pushes and every key it binds there - the steps through the
 * list, the reading column's own keys, the keys that act on the item in hand, and the command
 * bar's actions for it and for the bar of selected rows (R11) - with the record of the row `l`
 * was pressed on, which Escape returns to from the column.
 */
export function useInboxKeymap({
  barOpen,
  bulkBarRef,
  bulkBarShown,
  clearSelection,
  dismiss,
  listRef,
  onMark,
  paneSelection,
  paneShown,
  rowsById,
  scope,
  searchRef,
  selectedAsks,
}: InboxKeyboard): InboxColumnHold {
  /** The reading column, where `h`, `o`, `y`, Enter and `1`-`9` act while it is on. Written by
   *  `holdPane` below rather than handed to the wrapper, so the column's unmount is seen. */
  const paneRef = useRef<HTMLDivElement | null>(null);
  const rows = () => rowsIn(listRef.current);
  const step = (delta: 1 | -1) => roveFocus(rows(), rowAround(document.activeElement), delta);
  const focusInPane = () => paneRef.current?.contains(document.activeElement) === true;
  const focusInList = () => listRef.current?.contains(document.activeElement) === true;
  /** The row `l` was pressed on: the row Escape returns the focus to from inside the column,
   *  which is another row than the selected one whenever `j` moved the selection while the
   *  focus stayed in the list. Every other way into the column leaves it null, or names the row
   *  of the question the column shows (below), and either way Escape returns to that row, as it
   *  always has.
   *
   *  It is written by one key - `read-pane`'s `run` - and only where no record stands: a second
   *  `l`, and the `l` a reader comes back in on after an unmount inside the column left the
   *  record standing, both leave it alone, where either would otherwise write the row the
   *  column is showing over the row the reader came from. So re-entry by key and re-entry by
   *  click, which writes nothing at all, leave a standing record alike. The test is the record
   *  alone, with no check of where the focus is, because none is needed: a press from inside
   *  the column with no record standing finds no focused row there (`focusedRow()` is null in
   *  the column), so it writes the row of the question the column shows, or null where that
   *  question has no listed row, and either sends Escape exactly where `paneWayBack`'s fallback
   *  already would - the stored value differs from a click's null, the row Escape reaches does
   *  not. It is read by one accessor, `paneWayBack`, which both halves of Escape ask, so the
   *  `when` and the `run` cannot disagree about where the key goes.
   *
   *  The record stands exactly while two facts hold of the reader, each with its own drops;
   *  between them no route can outlive the record.
   *
   *  (a) The column still shows the question the record was made on. The layout effect below
   *      drops it wherever `paneSelection.id` has changed under a standing column: a step of
   *      `j` or `k`, the reader's own answer. Where the commit has nothing shown at all the
   *      column itself goes and `holdPane` drops the record first - a snooze of theirs and a
   *      question the server withdraws both take the selected row off the list before the next
   *      one is handed the selection - and that is also the only drop for the Inbox falling
   *      below the column's width, where the selection stands and the column goes.
   *  (b) The reader's focus has not left the column. The document's `focusin` below drops the
   *      record wherever the focus lands outside the column (`inColumn`: its wrapper, and a menu
   *      or a list a control of it opened, which hands the focus back into it as it closes),
   *      which is every route out at once:
   *      `f` to the search box, Escape to the row, a Tab onto a row's checkbox, a click into
   *      the list, the row a step out of the column focuses on its way, the dialog a picture
   *      opens full size. `forgetPaneReturn`, the wrapper's `onBlur`, covers the one exit that
   *      lands nowhere and so fires no `focusin`: a focus leaving the column for no node at all
   *      - a `blur()` of the control holding it, a click on the page outside its focusable
   *      `main` - which leaves `document.body` holding the focus. A click on non-focusable
   *      content inside `main` focuses `main` itself, which is a `focusin` outside the wrapper.
   *
   *  Two cases look like a route out and keep the record, because neither breaks that pair. A
   *  window or tab blur moves no focus at all: nothing is focused anew, so no `focusin` fires,
   *  and the `onBlur` that does names no target while the scroller still holds the focus - the
   *  reader is still in the column, and back on the tab Escape owes them the row they came
   *  from. A focused node React removes from inside the column - one body component swapped
   *  for another - while the column still shows the question the record was made on leaves
   *  the reader on that question, so the record is still theirs: React fires no blur for a
   *  node it removes, and the `document.body` the focus falls to is focused by nothing, so
   *  neither drop of (b) reads it. Their next focus does. A control that disables itself is
   *  not that case: Chromium blurs it as it is disabled, before React removes it - the head's
   *  `Assign to me` while its write is in flight - so the wrapper's `onBlur` reads the focus
   *  leaving for the body and drops the record, and Escape then goes to the row of the
   *  question the column shows. */
  const paneReturn = useRef<string | null>(null);
  /** The question the column was showing when this component last looked, which is how the
   *  layout effect below tells a commit that changed it from one that did not. Nothing else
   *  writes it, so the record itself still names a row and nothing more. */
  const shownQuestion = useRef<string | undefined>(undefined);
  // The column shows another question, so the record is spent: the row it names belongs to a
  // question the reader has been carried off, whichever route carried them. Read after every
  // commit, as the selection's own settling is (`pane-selection.ts`), and dropped here rather
  // than tested against the question at Escape, because a record kept past its question would
  // come back the moment the reader selected that question again. On the layout lane, so that
  // the drop lands in the commit that changed the question: React flushes a passive effect
  // before the next key only for the changes it commits synchronously, and the question can
  // change on another lane - a transition, a `?item=` the router settles - which would leave the
  // reader one Escape with a record its question has already left.
  useLayoutEffect(() => {
    if (shownQuestion.current === paneSelection.id) return;
    shownQuestion.current = paneSelection.id;
    paneReturn.current = null;
  });
  /** The column wrapper: the node the keys above act in, and the drop its unmount makes. React
   *  calls a ref callback with null whenever the callback's identity changes, so this one is
   *  stable - an arrow written into the JSX would be a new callback every render, called with
   *  null each time, and would drop a record the reader is still owed. */
  const holdPane = useCallback((node: HTMLDivElement | null) => {
    paneRef.current = node;
    // The column has gone, taking the question the record was made on off the screen and the
    // focus the scroller held to `document.body` with it. Nothing else reports that: no blur
    // fires for a node React removes and `body` takes no `focusin`. So this is the drop that
    // covers the commit a snooze or a withdrawal of the selected question leaves with nothing
    // shown, before the next question is handed the selection - it is first there, ahead of the
    // layout effect above - and the only one that covers the Inbox falling below the column's
    // width, where the selection stands and the column goes.
    if (node === null) paneReturn.current = null;
  }, []);
  // Fact (b), stated as itself: wherever the focus lands, the record stands only while that
  // place is inside the column. A focus the column itself takes - the head's Snooze menu under
  // `h`, a link in the drawn document, the scroller `l` focuses as it writes the record - lands
  // in the column and changes nothing, so the key's own `focusin` cannot undo it, and
  // Escape's `run` reads the record before it focuses the row, so the drop that press makes
  // takes nothing it still needs.
  //
  // One listener for this component's life, not one put up and taken down around each record:
  // the record is a ref precisely so that `l` commits nothing, so there is no render to hang a
  // record-shaped subscription on, and making one would mean rendering the Inbox twice per `l`.
  // The handler reads refs alone and nothing of this render, so it never goes stale and the
  // effect has nothing to depend on: it binds once on mount, unbinds on unmount, and no render
  // in between churns it.
  useEffect(() => {
    const dropWhereFocusLands = () => {
      // `focusin` fires once the focus has landed, so the document already names where.
      if (!inColumn(paneRef.current, document.activeElement)) paneReturn.current = null;
    };
    document.addEventListener("focusin", dropWhereFocusLands);
    return () => document.removeEventListener("focusin", dropWhereFocusLands);
  }, []);
  /** The drop a focus that lands nowhere makes, which is the half of fact (b) no `focusin`
   *  reports: React's `onBlur` bubbles from the column's descendants, so a reader whose control
   *  blurs itself, or who clicks the page outside its focusable `main`, has left the column with
   *  `document.body` holding the focus, and nothing is focused for the listener above to read.
   *
   *  Where the focus went is `relatedTarget`, and `document.activeElement` where the event names
   *  none, because those two nulls mean opposite things: a window or tab blur moves no focus and
   *  keeps the record, while a focus that left for the body drops it. The routes out that land
   *  on a node are this handler's too, an event before the listener above reads them, so what
   *  needs it is only the focus that lands nowhere. */
  const forgetPaneReturn = (event: FocusEvent<HTMLDivElement>) => {
    if (!inColumn(event.currentTarget, event.relatedTarget ?? document.activeElement)) {
      paneReturn.current = null;
    }
  };
  /** The row Escape returns the focus to from inside the column: the row `l` recorded, else the
   *  row the column is showing, and null where the list holds neither. The one reader of the
   *  record - the binding's `when` tests it against null, its `run` focuses it, and there is no
   *  second spelling of the way back for the two of them to disagree over. */
  const paneWayBack = (): HTMLElement | null =>
    rowIn(listRef.current, paneReturn.current) ?? paneSelection.selectedRow();
  /** One step of the reader's, in whichever mode the Inbox is in: the selection the column
   *  follows, or the focus roving the rows. Chosen once, so no binding below asks again. */
  const stepInMode = paneShown ? paneSelection.step : step;
  /** That step, taken from wherever the reader is standing. From inside the column the focus
   *  comes back to the selected row first: `j` fires in there, a `tabindex=0` section being no
   *  editable target, and a step from an approval to a question the column draws as a card
   *  swaps `ApprovalBody` for `CardBody`, which unmounts the focused scroller and drops the
   *  focus to `document.body` - where neither of Escape's branches holds and the key would do
   *  nothing. The reader then stands exactly where a click and then `j` leaves them today. */
  const move = (delta: 1 | -1) => {
    if (paneShown && focusInPane()) {
      paneSelection.selectedRow()?.focus();
    }
    stepInMode(delta);
  };
  /** When the arrows move: in the reading column the arrows are the column's - a document
   *  scrolls, an option list moves - so they move the selection only while the focus is in the
   *  list beside it. */
  const arrowsMove = paneShown ? focusInList : () => focusedRow() !== null;
  /** The row the keys act on: the one the column is showing, or the one holding the focus. */
  const handRow = (): InboxListRow | undefined => {
    const id = paneShown ? paneSelection.id : focusedRow()?.dataset.inboxRow;
    return id === undefined ? undefined : rowsById.get(id);
  };
  /** The row `x` marks, by its own `rowId`: asks and threads alike, since the bulk bar now offers
   *  each kind its own press. */
  const markableRow = (): string | undefined => {
    const row = handRow();
    return row === undefined ? undefined : rowId(row);
  };
  /** The row `e` dismisses: the thread row in hand, as the route names it - its id and what the
   *  page drew in it. An Ask or an Approval row has no Dismiss at all (`dismissRow`). */
  const dismissableRow = (): InboxDismissRow | undefined => {
    const row = handRow();
    return row === undefined ? undefined : dismissRow(row);
  };
  /** Where the Inbox's keys act: the reading column while it is on - its head's Snooze, owner
   *  link and Copy ref, and the answer form of whatever it is showing - else the row holding
   *  the focus. Null where there is neither, which is an Inbox with nothing selected (a wide
   *  but empty list, or the commit a snooze of the selected row leaves behind) and a list
   *  nothing in is focused. */
  const hand = () => (paneShown ? paneRef.current : focusedRow());
  /** The control `selector` names in whichever of the two that is, or null for none: every
   *  `when` below tests this against `null`, so the accessor never answers `undefined`. */
  const inHand = (selector: string) => hand()?.querySelector<HTMLElement>(selector) ?? null;
  /** The column's scroller, the focus stop `l` moves the reader into, or null where no column
   *  is rendered. */
  const paneScroller = () =>
    paneRef.current?.querySelector<HTMLElement>(PANE_SCROLLER_SELECTOR) ?? null;
  /** Where the answer keys - Enter and the digits - act: the selected question's own card, not
   *  the whole column. The column of a question written as a decision block draws the whole live
   *  document, and `ProofDocument` hosts an answerable card for every open block of it
   *  (`ProofDocument.tsx`), so the first `[data-ask-option]` under the column is the first open
   *  block's, which is another question than the one the reader selected. Null where the
   *  selected row has no card drawn for it: an approval, whose column is the document's
   *  version and whose answer is the head's own Approve, a document whose block has not
   *  mounted its card yet, and every thread row, which asks nothing. Below the column's width
   *  the keys act on the row holding the focus, itself or on one of its card's options (R18):
   *  a click on an option leaves the focus on its input, and the card it is in is the one in
   *  hand. */
  const answerHand = () =>
    paneShown
      ? paneSelection.id === undefined
        ? null
        : (paneRef.current?.querySelector<HTMLElement>(
            `[data-ask-card="${CSS.escape(paneSelection.id)}"]`
          ) ?? null)
      : (focusedRow() ??
        (document.activeElement?.matches(OPTION_SELECTOR) === true
          ? rowAround(document.activeElement)
          : null));
  const inAnswerHand = (selector: string) =>
    answerHand()?.querySelector<HTMLElement>(selector) ?? null;
  /** Whether the focus stands where Enter and `r` carry the reader into the item in hand (R18):
   *  on a row, on nothing (`document.body`, where a reader who has pressed no key yet stands),
   *  on the column's scroller, where `l` puts them, or on an option of the card in hand.
   *  Everywhere else the key is the focused control's own - a button, a link, a text field, a
   *  band's fold, an option of another card the column's document draws - and the registry's
   *  handler calls `preventDefault` for every binding that fires, so a binding that claimed it
   *  there would press nothing. */
  const atItem = (): boolean => {
    const active = document.activeElement;
    if (focusedRow() !== null || active === document.body) return true;
    if (active === null) return false;
    if (paneShown && active === paneScroller()) return true;
    return active.matches(OPTION_SELECTOR) && answerHand()?.contains(active) === true;
  };
  /** Where `r` takes the reader (R18): the reply box of the thread in hand, or the "Write a
   *  reply" its box is folded behind - a thread row's alone. An ask row waits on the reader's
   *  answer, and an open ask takes no reply (`AskThread`), so the key stands down on one; the
   *  other blocks a decision's document draws in the column are other asks', whose boxes are
   *  not the row's. A thread's body is marked wherever it is drawn (`data-inbox-thread-body`):
   *  in the column, in a card, and under its block in a document. The one thread drawn without
   *  it is an ask written as a decision block, whose block folds its box behind its own "Write
   *  a reply", which names the ask (`data-reply-opener`, `AskThreadDisclosure`). */
  const replyTarget = (): HTMLElement | null => {
    const row = handRow();
    if (row?.kind !== "thread") return null;
    return (
      inHand(`[data-inbox-thread-body] ${REPLY_BOX}`) ??
      inHand(`[data-reply-opener="${CSS.escape(row.thread.thread_id)}"]`)
    );
  };
  /** Where the document's own keys act from (R3): beside the column, with the focus on a row,
   *  elsewhere in the list or on nothing at all. Inside the column the browser's own scrolling
   *  keys are the reader's, on whatever there holds the focus, which `l` hands them. */
  const scrollsFromList = () =>
    paneShown &&
    paneScroller() !== null &&
    (focusInList() || document.activeElement === document.body);
  /** Space and Shift+Space: from the same places, but not on a control the press works itself. */
  const pagesFromList = () =>
    scrollsFromList() && document.activeElement?.matches(OWN_SPACE_SELECTOR) !== true;
  /** Moves the column to `top`, worked out from the column itself, and moves no focus: the
   *  selection and the reader's place in the list stay where they are. The move says so on the
   *  column first (`PANE_READER_MOVE`), so a placement still holding the column at its section
   *  lets it go rather than putting it back (`pane-placement.ts`). */
  const scrollColumn = (top: (column: HTMLElement) => number) => {
    const column = paneScroller();
    if (column === null) return;
    column.dispatchEvent(new Event(PANE_READER_MOVE));
    column.scrollTop = top(column);
  };
  // The bar is reached through its own ref, which holds wherever the header draws it.
  const bulkPicker = () => bulkBarRef.current?.querySelector<HTMLElement>(BULK_PICKER_SELECTOR);
  /** The approval request's Approve, or its Request changes, that `a` and Shift+R press (R7).
   *  Beside the column it is the chip's own button in the column's head, which the head draws
   *  from its first draw and holds until the document's read returns (`ReadingPaneHead`), and
   *  which counts only while it takes a press; else - below the column's width, in the drawer,
   *  and in the card the column falls back to where it cannot read the document - it is the
   *  request's own option of the card in hand (`data-ask-approval`, `AskCard`). Null on any
   *  other item and before the read returns, which the `?` help shows as not available. Inside
   *  an ask card in the column the reader is answering that card, the rule Snooze holds to, so
   *  the keys stand down there. */
  const approvalControl = (action: AskApprovalAction): HTMLElement | null => {
    if (paneShown && !outsideAskCard()) return null;
    const enabled = ':not([aria-disabled="true"])';
    return (
      (paneShown
        ? inHand(`[data-inbox-pane-head] [data-approval-action="${action}"]${enabled}`)
        : null) ?? inAnswerHand(`[data-ask-approval="${action}"]${enabled}`)
    );
  };
  /** Presses what `approvalControl` found. The head's button is pressed as it is: Approve sends
   *  the review, Request changes opens its box with the focus in the reason (`ApprovalChip`). A
   *  card's option is picked, the pick flushed so that the card's form reads it: Approve is then
   *  sent at once, and Request changes leaves the reader in the reason its pick asks for - which
   *  the card focuses as the pick draws it, and which is focused here for a pick already made,
   *  whose field draws nothing new. */
  const pressApproval = (action: AskApprovalAction) => {
    const control = approvalControl(action);
    if (!(control instanceof HTMLInputElement)) {
      control?.click();
      return;
    }
    flushSync(() => control.click());
    if (action === "approve") {
      control.form?.requestSubmit();
      return;
    }
    control.closest("[data-ask-card]")?.querySelector<HTMLElement>("[data-ask-answer]")?.focus();
  };
  /** The control a command bar action presses (R11), while it takes a press: a disabled one -
   *  Ask back with nothing typed - is no action to offer. */
  const pressable = (control: HTMLElement | null | undefined) =>
    control?.matches(":disabled") === false ? control : null;
  /** The item the command bar's actions act on: the column while it is on, else the row around
   *  the focus - the row itself, or a control of its card, where a press inside the card leaves
   *  the focus (an option after a click, Ask back instead as it shows). The palette gives the
   *  focus back before a chosen action runs, so its `when` and its `run` read the same row. */
  const commandHand = () => (paneShown ? paneRef.current : rowAround(document.activeElement));
  /** A control of the item in hand: its row, or the column with its head. */
  const itemControl = (selector: string) =>
    pressable(commandHand()?.querySelector<HTMLElement>(selector));
  /** A control of the item's own card (`answerHand`), never one of another card the column's
   *  document draws; where no open card of the item is drawn - a thread, an approval's document,
   *  an answered question's record - one anywhere in the item in hand. Below the column's width
   *  the card is the row around the focus. */
  const cardControl = (selector: string) => {
    if (!paneShown) return pressable(commandHand()?.querySelector<HTMLElement>(selector));
    return pressable(answerHand() === null ? inHand(selector) : inAnswerHand(selector));
  };
  /** A control of the column's head alone. */
  const headControl = (selector: string) => pressable(inHand(`[data-inbox-pane-head] ${selector}`));
  /** One command bar action of the item in hand, which presses the control that carries its mark
   *  (`inboxActionMark`), looked for by `find` in the part of the item that draws it. */
  const markedAction = (
    action: InboxActionId,
    find: (selector: string) => HTMLElement | null
  ): readonly [KeymapActionId, () => HTMLElement | null] => [
    action,
    () => find(inboxActionSelector(action)),
  ];
  /** The command bar's own actions for the item in hand and the bar of selected rows (R11), with
   *  no default key (a person can give one, R14): each presses the control the page draws for it,
   *  found by its mark. A chosen row runs once the palette has closed and given the focus back
   *  (`SearchPalette`), so the item in hand is the one the bar opened over - in card mode the row
   *  around the control the focus is back on (`commandHand`). */
  const commandBarActions: readonly (readonly [KeymapActionId, () => HTMLElement | null])[] = [
    markedAction("inbox.assign-to-me", itemControl),
    markedAction("inbox.unsnooze", itemControl),
    markedAction("inbox.mark-read", itemControl),
    markedAction("inbox.earlier-replies", cardControl),
    markedAction("inbox.asker-handles", cardControl),
    markedAction("inbox.copy-session-id", cardControl),
    markedAction("inbox.copy-session-title", cardControl),
    markedAction("inbox.copy-tmux-target", cardControl),
    markedAction("inbox.hand-on", cardControl),
    markedAction("inbox.referenced-by", cardControl),
    markedAction("inbox.earlier-approval-diff", cardControl),
    markedAction("inbox.ask-back", cardControl),
    markedAction("inbox.ask-back-instead", cardControl),
    markedAction("inbox.answer-anyway", cardControl),
    markedAction("inbox.cancel", cardControl),
    markedAction("inbox.decline", cardControl),
    markedAction("inbox.recipients", cardControl),
    markedAction("inbox.change-answer", cardControl),
    markedAction("inbox.previous-versions", cardControl),
    markedAction("inbox.earlier-answers", cardControl),
    markedAction("inbox.open-at-section", headControl),
    markedAction("inbox.skip-to-answer", headControl),
    markedAction("inbox.next-change", headControl),
    markedAction("inbox.show-reviews", headControl),
    [
      "inbox.select-all",
      () => pressable(bulkBarRef.current?.querySelector<HTMLElement>("[data-inbox-select-all]")),
    ],
    [
      "inbox.bulk-dismiss",
      () => pressable(bulkBarRef.current?.querySelector<HTMLElement>("[data-inbox-bulk-dismiss]")),
    ],
  ];
  useKeymapScope(scope);
  useKeymap(scope, [
    // With the reading column on, movement is the selection's: the column follows it, and the
    // focus stays where the reader left it in the list.
    { action: "inbox.next", run: () => move(1), when: () => rows().length > 0 },
    { action: "inbox.previous", run: () => move(-1), when: () => rows().length > 0 },
    {
      action: "inbox.arrows",
      run: (event) => move(event.key === "ArrowDown" ? 1 : -1),
      when: arrowsMove,
    },
    // The open document's own keys (R3), the pages of a reader who reads the column while the
    // list keeps the focus: Space and Shift+Space by a page, `g g` and Shift+G to its ends.
    {
      action: "inbox.scroll-down",
      run: () => scrollColumn((column) => column.scrollTop + column.clientHeight * PAGE_STEP),
      when: pagesFromList,
    },
    {
      action: "inbox.scroll-up",
      run: () => scrollColumn((column) => column.scrollTop - column.clientHeight * PAGE_STEP),
      when: pagesFromList,
    },
    {
      action: "inbox.scroll-top",
      run: () => scrollColumn(() => 0),
      when: scrollsFromList,
    },
    {
      action: "inbox.scroll-end",
      run: () => scrollColumn((column) => column.scrollHeight),
      when: scrollsFromList,
    },
    {
      action: "inbox.answer",
      run: () => {
        // A decision block's compact card folds its answer box behind "Add a note or answer in
        // your own words"; Enter presses that disclosure, and the card lands the focus in the
        // box its own render reveals (`AskCard.tsx`) - the box does not exist in this tick, so
        // nothing here could focus it. Only the column draws that compact card: from a row, and
        // from a disclosure already open, the key goes straight to the box.
        const box = inAnswerHand("[data-ask-answer]");
        if (box === null && paneShown) {
          inAnswerHand(NOTE_DISCLOSURE_SELECTOR)?.click();
          return;
        }
        box?.focus();
      },
      // Enter carries the reader from where they stand at the item into its answer box: the
      // row, the page body, the column after `l`, an option of its card (`atItem`). Everywhere
      // else the key belongs to the control that holds the focus - the card's own `Ask back
      // instead`, a row's owner link, a band's fold, the bulk-snooze bar, a filter control.
      when: () =>
        atItem() &&
        (inAnswerHand("[data-ask-answer]") !== null ||
          (paneShown && inAnswerHand(NOTE_DISCLOSURE_SELECTOR) !== null)),
    },
    {
      // `0` is the tenth option, and Other is the digit after the last real option: the card
      // marks it last among its options (`AskCard`).
      action: "inbox.option",
      run: (event) => {
        const options = answerHand()?.querySelectorAll<HTMLElement>(OPTION_SELECTOR);
        options?.[optionIndex(event.key)]?.click();
      },
      when: () => inAnswerHand(OPTION_SELECTOR) !== null,
    },
    {
      // The approval request in hand, approved with one press (R7): the column head's Approve,
      // or the card's Approve option, picked and sent.
      action: "inbox.approve",
      run: () => pressApproval("approve"),
      when: () => approvalControl("approve") !== null,
    },
    {
      // The head's Request changes opens its box with the focus in the reason, where Ctrl or
      // Cmd+Enter sends and Escape closes it sending nothing (`ApprovalChip`); the card's option
      // puts the focus in its reason field, where Escape lets go of the pick (`AskCard`) and
      // `back` below puts the focus back on the row.
      action: "inbox.request-changes",
      run: () => pressApproval("request-changes"),
      when: () => approvalControl("request-changes") !== null,
    },
    {
      // The thread in hand's reply box (R18), from where Enter would start.
      action: "inbox.reply",
      run: () => {
        const target = replyTarget();
        if (target === null || target.matches(REPLY_BOX)) {
          target?.focus();
          return;
        }
        openReplyBox(target);
      },
      when: () => atItem() && replyTarget() !== null,
    },
    {
      action: "inbox.open",
      run: () => inHand("[data-inbox-owner]")?.click(),
      when: () => inHand("[data-inbox-owner]") !== null,
    },
    {
      // What `o` opens, in a new tab of the browser rather than this one, which keeps its
      // address (R20): the page's own Inbox and its drawer alike.
      action: "inbox.open-new-tab",
      run: () => openLinkInNewTab(inHand("[data-inbox-owner]")),
      when: () => inHand("[data-inbox-owner]") !== null,
    },
    {
      // The item's own reference, never one of a comment or reply its thread draws, each of
      // which has its own control. An Agent row has no reference of its own - its thread is a
      // message to a session - so it draws no copy control and this stands down on it.
      action: "inbox.copy-ref",
      run: () => inHand(ITEM_COPY_REF_SELECTOR)?.click(),
      when: () => inHand(ITEM_COPY_REF_SELECTOR) !== null,
    },
    {
      action: "inbox.back",
      run: () => {
        // From the reading column, Escape is the way back to the row the reader came from: `l`
        // recorded that row, which is not the selected one whenever `j` moved the selection
        // without moving the focus. Every other way in records nothing, or the row the column
        // is showing, and returns, as it always has, to that row. `paneWayBack` is both of
        // those, and the `when` below asks it the same question, so the key never fires on a way
        // back the run then cannot find.
        if (paneShown && focusInPane()) {
          paneWayBack()?.focus();
          return;
        }
        const row = rowAround(document.activeElement);
        if (row === focusedRow()) {
          row?.blur();
        } else {
          row?.focus();
        }
      },
      // Both ways back are tested, because either can be the only one left. The column keeps a
      // question whose row the typed text hides while the reader has an unsent answer in it
      // (`pane-selection.ts`'s `paneAsk`), and there the row `l` was pressed on is the way out,
      // the selected row having none; every other way into the column records no row, and there
      // the selected row is the way out. With only the selected row tested, Escape in a column
      // held open over a hidden row would match no binding and do nothing.
      when: () =>
        rowAround(document.activeElement) !== null ||
        (paneShown && focusInPane() && paneWayBack() !== null),
    },
    {
      // The column is the other half of the page, and `l` is the step into it: the scroller
      // takes the focus, and the browser scrolls it on the arrow keys, Page Up, Page Down,
      // Space, Home and End from there.
      action: "inbox.read-pane",
      run: () => {
        // The record Escape reads, written only where no record stands. It names the row the
        // reader came from - the one holding the focus, or the selected one where the focus is
        // on nothing (`document.body`, where a reader who has pressed no key yet stands) or on
        // something that is not a row, which includes a press from inside the column itself.
        //
        // A press with a record standing writes nothing, whether the reader is in the column
        // already or on the body an unmount inside the column dropped their focus to: both
        // would write the row the column is showing, or null, over the row the reader actually
        // came from, which is the row Escape owes them. So the key leaves a standing record
        // exactly as a click back into the column does, and the two ways back in agree.
        if (paneReturn.current === null) {
          paneReturn.current =
            focusedRow()?.dataset.inboxRow ?? paneSelection.selectedRow()?.dataset.inboxRow ?? null;
        }
        paneScroller()?.focus();
      },
      // One test for all three cases the key does nothing in: below 1280 px and in the drawer
      // `paneShown` is false (`pane && wideEnoughForPane`), and with no row selected no column
      // is rendered at all, so `paneRef.current` is null and there is no scroller under it.
      when: () => paneShown && paneScroller() !== null,
    },
    {
      action: "inbox.select",
      run: () => {
        const id = markableRow();
        if (id !== undefined) onMark(id);
      },
      when: () => markableRow() !== undefined,
    },
    {
      // The row goes out of the Inbox, and in the Dismissed view back into it; the reader is then
      // handed on to the item after it (R8). An Ask or an Approval row has no Dismiss, so the key
      // does nothing on one.
      action: "inbox.dismiss",
      run: () => {
        const named = dismissableRow();
        if (named !== undefined) dismiss([named]);
      },
      // Not from inside an ask the reader is answering, the rule `h` holds to: the card's own
      // controls are not the list's, and a typed `e` belongs to whatever holds the focus.
      when: () => dismissableRow() !== undefined && (!paneShown || outsideAskCard()),
    },
    {
      // One action for both: with rows marked it is the whole selection, otherwise the row in
      // hand. The key presses the menu's trigger, which opens it with the focus on its first
      // choice and hands the focus back here as it closes (`ChoiceMenu`).
      action: "inbox.snooze",
      run: () => {
        if (selectedAsks.length === 0) {
          // The short row carries no Snooze of its own: the reading column's head does, for the
          // row it is showing.
          inHand("[data-inbox-snooze]")?.click();
          return;
        }
        bulkPicker()?.click();
      },
      // The selection's key works from wherever marking leaves the reader - a row, the checkbox
      // they just ticked, the bar - but not from inside an ask they are answering: an option
      // radio takes no typed text, so the registry passes the key through, and the card's own
      // controls are not the list's. The issue page withholds its writing keys the same way.
      // Inside the column that rule covers the lone Snooze too, the card there being the
      // column's own.
      when: () =>
        (selectedAsks.length > 0 && outsideAskCard()) ||
        (inHand("[data-inbox-snooze]") !== null && (!paneShown || outsideAskCard())),
    },
    {
      // The priority of the issue the item in hand is about. A question or an approval request
      // of an issue draws the control, and a document's ask and a thread draw none, so on those
      // the key opens nothing and the `?` help shows it not available. Not from inside an ask
      // the reader is answering, the rule Snooze holds to.
      action: "inbox.priority",
      run: () => inHand("[data-priority-menu]")?.click(),
      when: () => inHand("[data-priority-menu]") !== null && (!paneShown || outsideAskCard()),
    },
    {
      // Escape is one level out, and the selection is the outermost thing a row press made:
      // `back` takes the reader off the row first, and this clears what they marked. It is
      // offered exactly while the bar's Clear is, a pick in flight and a refusal the reader has
      // since unticked the rows of included - the keyboard that raised them dismisses them. It is
      // the bar's Clear selection in the command bar too (R11), offered from the row the reader
      // marked: on a row Escape is `back`'s, which is registered first and holds wherever the
      // focus is in a row, so the key still clears nothing until the reader is off the row.
      action: "inbox.clear-selection",
      run: clearSelection,
      // A popover of the bar's takes Escape first: one press closes it, and dropping what the
      // reader marked at the same time would be two undos for one key.
      when: () => bulkBarShown && !barOpen,
    },
    {
      // The box is one key away from anywhere on the page.
      action: "inbox.search",
      run: () => searchRef.current?.focus(),
      when: () => searchRef.current !== null,
    },
    {
      // Escape is one level out, and out of the box is the list: the row the column is showing
      // on the page, and the first row in the drawer, where nothing is selected. It is offered
      // only while the focus is in the box, so the two Escapes above are untouched - `back`
      // answers false there, and `clear-selection` is not `inEditable` at all - and the drawer's
      // own Close waits its turn, the Inbox's registration being the first of the scope to match
      // (`keymap.ts`'s `resolve`).
      action: "inbox.leave-search",
      run: () => (paneSelection.selectedRow() ?? rows()[0])?.focus(),
      when: () => searchRef.current !== null && document.activeElement === searchRef.current,
    },
    // The command bar's own actions for the item in hand and the selection (R11).
    ...commandBarActions.map(([action, control]) => ({
      action,
      run: () => control()?.click(),
      when: () => control() !== null,
    })),
  ]);
  return { forgetPaneReturn, holdPane };
}
