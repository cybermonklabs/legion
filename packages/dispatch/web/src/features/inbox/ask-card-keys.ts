import { type KeyBinding, type KeymapScope, useKeymap } from "../shell/keymap";
import { quoteReplyKey, quoteSelection } from "../threads/quote-reply";

/**
 * What an open ask's card does on its keys. Each mounted `AskCard` registers its own against
 * its article (`registerAskCard`), so whichever host decides which card is in hand - the
 * Inbox's selected row, or the card holding the focus on any other page - reaches the card's
 * own state through the element alone.
 */
export interface AskCardActions {
  /** Absent on an approval request, which is not rewritten, and while the reader changes a
   *  recorded answer. */
  rewrite?: () => void;
  /** Absent while the reader changes a recorded answer, where the card offers Save answer alone. */
  askBack?: () => void;
  /** Absent on an approval request, which cannot be declined, and while the reader changes a
   *  recorded answer. */
  decline?: () => void;
  /** Absent while the reader changes a recorded answer. */
  handOn?: () => void;
  /** Opens Define on the words selected in the card; false when none are. */
  define: () => boolean;
  hasSelection: () => boolean;
  /** Present while the card shows a rewrite, whose original wording `w` shows in place. */
  toggleOriginal?: () => void;
  /** Leaves Ask back for answering again; false when the card is not asking back. */
  stopAskingBack: () => boolean;
  /** Sends what the answer box holds: the answer, or the question back while asking back. */
  send: () => void;
}

const cards = new WeakMap<Element, AskCardActions>();

/** Records `actions` as the card `element`'s own; the returned function lets them go. */
export function registerAskCard(element: Element, actions: AskCardActions): () => void {
  cards.set(element, actions);
  return () => {
    if (cards.get(element) === actions) cards.delete(element);
  };
}

/** The card's actions, for the card `element`, the nearest card holding it, or else the card it
 *  holds: an Inbox row below 1280 px draws its question's card inside itself. */
export function askCardActions(element: Element | null): AskCardActions | undefined {
  const card = element?.closest("[data-ask-card]") ?? element?.querySelector("[data-ask-card]");
  return card === null || card === undefined ? undefined : cards.get(card);
}

/** The answer box of a card that is asking back, whose Escape returns it to answering. */
export const ASKING_BACK_SELECTOR = '[data-ask-answer][data-asking-back="true"]';

/** The card's own `1`–`9` and Enter, acting on `card`: an option by its place, and the answer
 *  box. A host that decides more about them (the Inbox, whose Enter carries the reader from a
 *  row into the reading pane) passes its own under the same ids. */
export function askCardLeadKeys(card: () => Element | null): KeyBinding[] {
  return [
    {
      id: "option",
      keys: ["1", "2", "3", "4", "5", "6", "7", "8", "9"],
      label: "Pick an option",
      run: (event) => {
        const options = card()?.querySelectorAll<HTMLElement>("[data-ask-option]");
        options?.[Number(event.key) - 1]?.click();
      },
      when: () => card()?.querySelector("[data-ask-option]") != null,
    },
    {
      // Off the Inbox, Enter belongs to the control holding the focus inside the card (an
      // option submits the form, a button presses), so the card's own Enter acts only with
      // the card itself in focus.
      id: "answer",
      keys: "Enter",
      label: "Put the cursor in the answer box",
      run: () => card()?.querySelector<HTMLElement>("[data-ask-answer]")?.focus(),
      when: () => {
        const held = card();
        return held !== null && document.activeElement === held;
      },
    },
  ];
}

/** The places of the options a card can have keys for, `1` to `9`. */
const OPTION_PLACES = [1, 2, 3, 4, 5, 6, 7, 8, 9] as const;

/**
 * The card's keys, under the `ask` scope the shortcut help lists as **Ask card**, in the order
 * and with the labels of the card's menu: `lead` first (`1`–`9` and Enter, `askCardLeadKeys`),
 * then `r` Rewrite, `b` Ask back, `a` Hand this question to, `n` Decline to answer, `d` Define
 * selected words, `q` Quote reply, `w` the original wording, and, from the answer box, the
 * modified Enter that sends and Escape, which leaves Ask back. `card` names the card in hand, or
 * null for none. The card pushes the `ask` scope itself while the focus is inside it (`AskCard`,
 * `appKeymap.pushScope`), and the Inbox pushes it above its own while its card in hand is an ask
 * (`useKeymapScope`), so these act before the host's keys. A host drawn in a dialog (the Inbox's
 * drawer) registers them in `scope`, the dialog's, which is the one scope a dialog consults.
 *
 * The command bar lists the card's keys that act with no text field in focus. A row presses
 * only its binding's first key, so `1`–`9` are one key binding and nine rows of their own, one
 * for each option the card has, which press no key (`keys: []`) and which `?` leaves out.
 */
export function useAskCardKeys(
  card: () => Element | null,
  lead: readonly KeyBinding[],
  scope: KeymapScope = "ask"
): void {
  const actions = () => askCardActions(card());
  const options = () => card()?.querySelectorAll<HTMLElement>("[data-ask-option]");
  useKeymap(scope, [
    ...lead,
    ...OPTION_PLACES.map(
      (place): KeyBinding => ({
        id: `option-${place}`,
        keys: [],
        label: `Pick option ${place}`,
        run: () => options()?.[place - 1]?.click(),
        when: () => (options()?.length ?? 0) >= place,
      })
    ),
    {
      id: "rewrite",
      keys: "r",
      label: "Rewrite",
      run: () => actions()?.rewrite?.(),
      when: () => actions()?.rewrite !== undefined,
    },
    {
      id: "ask-back",
      keys: "b",
      label: "Ask back",
      run: () => actions()?.askBack?.(),
      when: () => actions()?.askBack !== undefined,
    },
    {
      id: "hand-on",
      keys: "a",
      label: "Hand this question to…",
      run: () => actions()?.handOn?.(),
      when: () => actions()?.handOn !== undefined,
    },
    {
      id: "decline",
      keys: "n",
      label: "Decline to answer",
      run: () => actions()?.decline?.(),
      when: () => actions()?.decline !== undefined,
    },
    {
      id: "define",
      keys: "d",
      label: "Define selected words",
      run: () => actions()?.define(),
      when: () => actions()?.hasSelection() === true,
    },
    // The selection bar's other action (`threads/SelectionBar.tsx`): the selected words, quoted
    // in the answer box.
    quoteReplyKey(
      "Quote selected words in your answer",
      quoteSelection,
      () => actions()?.hasSelection() === true
    ),
    {
      id: "original",
      keys: "w",
      label: "Show or hide the original wording",
      run: () => actions()?.toggleOriginal?.(),
      when: () => actions()?.toggleOriginal !== undefined,
    },
    {
      // The answer box sends on this itself (`submitOnModifiedEnter`); it is listed here so the
      // help says it, and it sends from anywhere else in the card too.
      id: "send",
      inEditable: true,
      keys: "$mod+Enter",
      label: "Send the answer, or the question back",
      palette: false,
      run: () => actions()?.send(),
      when: () => actions() !== undefined && card()?.contains(document.activeElement) === true,
    },
    {
      // A definition closes itself on Escape first (`DefinePopover`), and the Inbox's Escape back
      // to the row stands down while this one applies (`Inbox.tsx`).
      id: "stop-asking-back",
      inEditable: true,
      keys: "Escape",
      label: "Close a definition, or stop asking back",
      palette: false,
      run: () => actions()?.stopAskingBack(),
      when: () => card()?.querySelector(ASKING_BACK_SELECTOR) != null,
    },
  ]);
}
