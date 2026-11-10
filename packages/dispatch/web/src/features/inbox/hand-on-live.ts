import { createContext } from "react";

/** Holds an ask in the list drawing it while a hand-on from its card is in flight or failed, and
 *  lets it go once the server decides it or the card goes. */
export type AskHandOnLive = (askId: string, live: boolean) => void;

/** A card drawn outside such a list registers with nobody: nothing there takes the ask out from
 *  under it, so nothing has to hold it. */
const NO_LIST: AskHandOnLive = () => {};

/**
 * The hand-on registration the Inbox gives every card it draws, as a context rather than a prop.
 *
 * The hand-on's optimistic `waiting_on_person` takes the row out of the Inbox's own list the
 * moment the write starts (`isMine`, `inAssigneeFilter`): without the hold the card that wrote
 * is unmounted before the server answers, and a refusal puts the row back with a fresh card that
 * never failed - the reader sees it return and no reason.
 *
 * It is a context because the card the reader pressed need not be one the Inbox rendered itself.
 * A question written as a decision block is drawn in the reading column by the document page's
 * own components (`ReadingPane` -> `PaneDocument` -> `ProofDocument` -> `AskBlockCard`), which
 * carry no prop of the Inbox's, and threading one through them would leave the next host free to
 * omit it exactly as that path did. `AskHandOn` reads the registration from here instead, so
 * every card under an Inbox has it and no host can be written without it. The same reasoning put
 * the reader's outstanding answer in `outstanding-answers.ts`; `SnoozeControl` and `AssignToMe`
 * still report upward through a prop, because those two controls sit beside the card in a row
 * the Inbox renders itself.
 *
 * Each Inbox provides its own - the page's and the drawer's are separate - and with no provider
 * a card registers with nobody (`NO_LIST`), which is what a card drawn outside a list does: the
 * issue page's Conversation, a document's margin, its ask block, the Answers page.
 */
export const AskHandOnLiveContext = createContext<AskHandOnLive>(NO_LIST);
