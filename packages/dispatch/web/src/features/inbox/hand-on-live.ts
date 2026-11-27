import { createContext } from "react";

/** Offers a card's hand-on to the list around it: `true` while that write is in flight or
 *  failed, `false` once the server decides it or the card goes. The list holds the row of an
 *  ask it is listing until the write ends, and ignores an ask it lists no row for. */
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
 * own components - `ReadingPane` -> `DocumentPane` (which lives in `PaneDocument.tsx`) ->
 * `ProofDocument` -> `AskBlockCard` - which carry no prop of the Inbox's, and threading one
 * through them would leave the next host free to omit it exactly as that path did. `AskHandOn`
 * reads the registration from here instead, so every card under an Inbox has it and no host can
 * be written without it. The same reasoning put the reader's outstanding answer in
 * `outstanding-answers.ts`. `SnoozeControl` and `AssignToMe` still report upward through a
 * prop, because both of their hosts are the Inbox's own: the row it renders, and the reading
 * column's head (`ReadingPaneHead`), which the Inbox reaches through `ReadingPane`'s
 * `onSnoozeLive` - the head's own `AssignToMe` reports no further than the head, which draws it
 * while that write is live.
 *
 * Reaching every card is also why the Inbox holds only what it listed. Many cards under it
 * speak for an ask it draws no row for: the card of a Reply row's thread, whose ask can be one
 * the reader's filters hide, and every ask block of a document open in the column beside the
 * selected one. The Inbox takes a `live === true` registration only for an ask among the rows
 * it last drew, so none of those can pull a hidden question into the list, and takes every
 * `live === false` one, so a hold always ends with its write (`Inbox.tsx`, `onHandOnLive`).
 *
 * Each Inbox provides its own - the page's and the drawer's are separate - and with no provider
 * a card registers with nobody (`NO_LIST`), which is what a card drawn outside a list does: the
 * issue page's Conversation, a document's margin, its ask block, the Answers page.
 */
export const AskHandOnLiveContext = createContext<AskHandOnLive>(NO_LIST);
