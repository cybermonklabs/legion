import { type KeyBinding, useKeymap } from "../shell/keymap";

/**
 * What an open ask's card does on its keys. Each mounted `AskCard` registers its own against
 * its article (`registerAskCard`), so whichever host decides which card is in hand - the
 * Inbox's selected row, or the card holding the focus on any other page - reaches the card's
 * own state through the element alone.
 */
export interface AskCardActions {
  /** Absent on an approval request, which is not rewritten. */
  rewrite?: () => void;
  askBack: () => void;
  /** Absent on an approval request, which cannot be declined. */
  decline?: () => void;
  handOn: () => void;
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

/** The card's actions, for the card `element` or the nearest card holding it. */
export function askCardActions(element: Element | null): AskCardActions | undefined {
  const card = element?.closest("[data-ask-card]");
  return card === null || card === undefined ? undefined : cards.get(card);
}

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

/**
 * The card's keys, under the `ask` scope the shortcut help lists as **Ask card**, in the order
 * and with the labels of the card's menu: `lead` first (`1`–`9` and Enter, `askCardLeadKeys`),
 * then `r` Rewrite, `b` Ask back, `a` Hand this question to, `n` Decline to answer, `d` Define
 * selected words, `w` the original wording, and, from the answer box, the modified Enter that
 * sends and Escape, which leaves Ask back. `card` names the card in hand, or null for none; the
 * host also pushes the `ask` scope above its own while one is (`useKeymapScope`), so these act
 * before its keys.
 */
export function useAskCardKeys(card: () => Element | null, lead: readonly KeyBinding[]): void {
  const actions = () => askCardActions(card());
  useKeymap("ask", [
    ...lead,
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
      run: () => actions()?.askBack(),
      when: () => actions() !== undefined,
    },
    {
      id: "hand-on",
      keys: "a",
      label: "Hand this question to…",
      run: () => actions()?.handOn(),
      when: () => actions() !== undefined,
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
      // to the row acts once this one stands down.
      id: "stop-asking-back",
      inEditable: true,
      keys: "Escape",
      label: "Close a definition, or stop asking back",
      palette: false,
      run: () => actions()?.stopAskingBack(),
      when: () => card()?.querySelector('[data-ask-answer][data-asking-back="true"]') != null,
    },
  ]);
}
