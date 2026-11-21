import { type MutationKey, useQueryClient } from "@tanstack/react-query";
import { createContext, type RefObject, useCallback, useRef, useState } from "react";
import { flushSync } from "react-dom";

import { panelOf } from "../../components/focus-place";
import { copyText } from "../../lib/clipboard";
import { focusVisibleRing } from "../../theme/classes";
import { conversationItemId } from "../issue/pins";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  parseNodeReference,
} from "../refs/routes";
import { useKeymap } from "../shell/keymap";
import { closestMatching, focusedMatching, roveFocus } from "../shell/roving";
import { threadReplySendKey } from "./usePhoneThread";

/**
 * The Conversation tab's turns as its keys reach them (dispatch://INFRA-2160, R22).
 *
 * A turn is one conversation the tab lists - a comment, a message, an ask or a handoff - or one
 * reply inside it. Each takes the focus from the tab's own keys (`tabIndex={-1}`), not from Tab:
 * `j` and `k` walk them, and Enter, `o`, `r`, `y` and Shift+Y act on the one the focus is on
 * (`useTurnKeys`, which `ConversationTab` calls). The replies are drawn by rows the Inbox and the
 * Agents page draw too (`ReplyTurn`, `TargetedMessageCard`, `ThreadCard`, `AskThread`), which take
 * the focus only where the tab provides `ConversationTurns`: on those pages they are drawn as
 * before.
 */

/** Every turn and every reply inside one: what `j` and `k` walk, and where the focus stands on a
 *  turn, which the issue header's `a` and Shift+R stand down on (`IssueHeader`). */
export const TURN_ROW = "[data-turn], [data-reply]";

/** A conversation itself, never a reply inside one: the turn that holds its thread and folds. */
export const ROOT_TURN = "[data-turn]:not([data-reply])";

/** The control that opens and closes a turn's fold other than a comment's thread: a handoff's
 *  Show fields, and the earlier replies of a thread that folds them (`EarlierReplies`). */
export const TURN_FOLD = "[data-turn-fold]";

/** Whether the turns drawn below are the Conversation tab's, which its keys walk. */
export const ConversationTurns = createContext(false);

/** The ring a turn draws while the keyboard's focus is on it. */
export const turnFocusRing = `outline-none focus-visible:ring-2 ${focusVisibleRing}`;

/** What a turn spreads on itself inside the tab: the focus the tab's keys give it, and the
 *  `dispatch://` reference `y` copies (`data-turn-ref`) where it has one - a handoff, and a
 *  message to a session that names no issue, have none. Outside the tab, nothing. Every turn and
 *  reply is drawn as an `<li>`, a row of the tab's list by its element alone (`row-marker.ts`'s
 *  `ROW_SELECTOR`), which the page's focus rescue reads for a loss the reader caused. */
export function turnMarks(
  inTab: boolean,
  route: DispatchReferenceRoute | undefined
): { "data-turn-ref"?: string; tabIndex?: number } {
  if (!inTab) return {};
  return route === undefined
    ? { tabIndex: -1 }
    : { "data-turn-ref": buildDispatchReference(route), tabIndex: -1 };
}

/** The first control `selector` names inside `turn` that is the turn's own, not one of a reply
 *  drawn inside it. */
export function ownControl(turn: HTMLElement, selector: string): HTMLElement | null {
  for (const control of turn.querySelectorAll<HTMLElement>(selector)) {
    if (control.closest(TURN_ROW) === turn) return control;
  }
  return null;
}

/** A reply box (`MentionComposer`, `AskReplyComposer`): where `r` puts the caret, on the
 *  Conversation tab and in the Inbox. */
export const REPLY_BOX = "[data-reply-box]";

/** Puts the caret in the reply box behind `opener`, an answered ask's "Write a reply"
 *  (`data-reply-opener`, `AskThreadDisclosure`), which draws its box under it as it is pressed:
 *  the press is flushed, so the box stands in the page when the panel the control names is
 *  read (`panelOf`). */
export function openReplyBox(opener: HTMLElement): void {
  if (opener.getAttribute("aria-expanded") !== "true") flushSync(() => opener.click());
  panelOf(opener)?.querySelector<HTMLElement>(REPLY_BOX)?.focus();
}

/** How a key of the tab opened a comment thread inline. Enter or `o` opened it `standing`: its
 *  reply box takes no focus, which stays on the turn. `r` opened it, or reached its open box, to
 *  `reply`: the box has the focus, and Escape in it gives the focus back to the turn `r` was
 *  pressed on and leaves the thread open. A thread a click or a link opened is neither: its box
 *  takes the focus, and Escape in it closes the thread. */
export type KeyedThread = "standing" | "reply";

/** A turn's Reply (`ReplyButton`), which `r` presses on a message. */
const REPLY_BUTTON = 'button[aria-label="Reply"]';

/** The comment whose thread `turn`, a conversation's own turn, is: its `data-turn` names it
 *  `comment:<id>` (`conversationItemId`). */
function commentOfTurn(turn: HTMLElement): string | undefined {
  const prefix = conversationItemId("comment", "");
  const id = turn.dataset.turn;
  return id?.startsWith(prefix) === true ? id.slice(prefix.length) : undefined;
}

/** What the tab gives its turn keys. */
interface TurnKeysOptions {
  /** At the phone's width a comment's thread opens full-screen (`openPhoneThread`), not inline,
   *  and a comment's Reply answers in that view. */
  readonly isPhoneViewport: boolean;
  /** Opens comment `id`'s thread full-screen (`usePhoneThread`'s `open`). */
  readonly openPhoneThread: (id: string) => void;
  /** The tab's send name, beneath which each thread's own reply goes out (`threadReplySendKey`). */
  readonly sendKey: MutationKey;
  /** Whether the tab is shown: every key stands down while another tab is. */
  readonly visible: boolean;
}

/** What the turn keys give the tab: the threads it has open inline, how a key opened each, and
 *  where a reply a key started gives the focus back. */
export interface TurnKeys {
  /** Called as the docked composer takes a reply (`beginReply`): a reply started by a click
   *  starts from nowhere, and `r` notes its turn once its press has started the reply. */
  readonly beginDockedReply: () => void;
  /** The docked composer's Cancel reply, or Escape in it: a reply `r` started gives the focus
   *  back to the turn it was pressed on, while that turn is in the page. */
  readonly giveBackFromDockedReply: () => void;
  /** Escape in the reply box of comment `comment`'s thread, which `r` put the reader in: the
   *  focus goes back to the turn `r` was pressed on, or to the thread's own turn where that one
   *  is gone, and the thread stays open. */
  readonly giveBackFromThread: (comment: string) => void;
  /** The open threads a key of the tab opened, by how (`KeyedThread`): the thread card's reply
   *  box follows it. A thread a click opened, or a link, is in neither. */
  readonly keyedThreads: ReadonlyMap<string, KeyedThread>;
  /** The comments whose thread the reader has open inline. */
  readonly openThreads: ReadonlySet<string>;
  /** Opens or closes comment `id`'s thread as a click, a link or a decision does: a thread
   *  opened so, and every thread closed, is a key's no longer. */
  readonly setThreadOpen: (id: string, open: boolean) => void;
  /** The tab's list of turns, which the keys walk and act in. */
  readonly turnList: RefObject<HTMLOListElement | null>;
}

/**
 * The keys of the Conversation tab's turns (R22), registered in the `issue` scope for the tab's
 * lifetime, with the open threads they act on. `j` and `k` walk the turns and the replies the
 * list draws, an open thread's replies among them; Enter, `o`, `r`, `y` and Shift+Y act on the
 * turn the focus is on itself, never on one that merely holds a focused control, whose keys are
 * its own. Every key stands down while another tab is shown.
 */
export function useTurnKeys({
  isPhoneViewport,
  openPhoneThread,
  sendKey,
  visible,
}: TurnKeysOptions): TurnKeys {
  const queryClient = useQueryClient();
  const [openThreads, setOpenThreads] = useState<ReadonlySet<string>>(() => new Set());
  const [keyedThreads, setKeyedThreads] = useState<ReadonlyMap<string, KeyedThread>>(
    () => new Map()
  );
  // The turn `r` was pressed on for each thread whose box it put the reader in: where Escape in
  // that box gives the focus back.
  const replyOrigins = useRef(new Map<string, HTMLElement>());
  /** Opens or closes comment `id`'s thread, by a key of the tab where `keyed` says how; a thread
   *  opened any other way, and every thread closed, is a key's no longer. */
  const setThreadOpen = useCallback((id: string, open: boolean, keyed?: KeyedThread) => {
    setOpenThreads((current) => {
      if (current.has(id) === open) return current;
      const next = new Set(current);
      if (open) next.add(id);
      else next.delete(id);
      return next;
    });
    const how = open ? keyed : undefined;
    if (how !== "reply") replyOrigins.current.delete(id);
    setKeyedThreads((current) => {
      if (current.get(id) === how) return current;
      const next = new Map(current);
      if (how === undefined) next.delete(id);
      else next.set(id, how);
      return next;
    });
  }, []);
  // The turn `r` started the docked composer's reply from, where Escape in it, or its Cancel
  // reply, gives the focus back. A reply started by a click starts from nowhere.
  const dockedReplyFrom = useRef<HTMLElement | null>(null);
  const turnList = useRef<HTMLOListElement>(null);
  /** The turn or reply of the list that holds the keyboard's focus itself. */
  const focusedTurn = (): HTMLElement | null => {
    const turn = focusedMatching(TURN_ROW);
    return turn !== null && turnList.current?.contains(turn) === true ? turn : null;
  };
  /** What `of` finds to do on the focused turn, while the tab is shown. */
  const onFocusedTurn = (of: (turn: HTMLElement) => (() => void) | null) => {
    const turn = visible ? focusedTurn() : null;
    return turn === null ? null : of(turn);
  };
  /** The reference `y` copies off the focused turn, while the tab is shown. */
  const turnRef = (): string | undefined => (visible ? focusedTurn()?.dataset.turnRef : undefined);
  /** What Enter and `o` do on `turn`: open or close its comment thread - with the reply box
   *  standing, so the focus stays on the turn - or press its conversation's other fold
   *  (`TURN_FOLD`). Where the press takes the turn away, a reply in the closed thread or fold,
   *  its conversation takes the focus. Null where there is nothing to open or close, and on a
   *  thread whose own reply is out, which stays open until it lands, as a resolve holds it. */
  const toggleOf = (turn: HTMLElement): (() => void) | null => {
    const root = turn.closest<HTMLElement>(ROOT_TURN);
    if (root === null) return null;
    const comment = commentOfTurn(root);
    let press: (() => void) | undefined;
    if (comment === undefined) {
      const fold = ownControl(root, TURN_FOLD);
      if (fold !== null) press = () => fold.click();
    } else if (!openThreads.has(comment)) {
      press = isPhoneViewport
        ? () => openPhoneThread(comment)
        : () => setThreadOpen(comment, true, "standing");
    } else if (
      queryClient.isMutating({ mutationKey: threadReplySendKey(sendKey, comment) }) === 0
    ) {
      press = () => setThreadOpen(comment, false);
    }
    if (press === undefined) return null;
    const pressed = press;
    return () => {
      flushSync(pressed);
      if (!turn.isConnected) root.focus();
    };
  };
  /** Where `r` puts the caret from `turn`: in its comment thread's reply box - opening the thread
   *  first, its box not standing, so the box takes the focus as it mounts - or an answered ask's
   *  box behind its "Write a reply". A message, and a comment on a phone, press their own Reply,
   *  which hands the docked composer, or the phone's thread, the reply. Null where nothing takes
   *  a reply: an open ask waits on its answer, a handoff takes none, and a Reply is held while a
   *  send is out. */
  const replyOf = (turn: HTMLElement): (() => void) | null => {
    const root = turn.closest<HTMLElement>(ROOT_TURN);
    if (root === null) return null;
    const comment = commentOfTurn(root);
    if (comment !== undefined && !isPhoneViewport) {
      const box = openThreads.has(comment)
        ? root.querySelector<HTMLElement>(`[data-thread-reply]:not([hidden]) ${REPLY_BOX}`)
        : undefined;
      if (box === null) return null;
      return () => {
        replyOrigins.current.set(comment, turn);
        setThreadOpen(comment, true, "reply");
        box?.focus();
      };
    }
    const opener = root.querySelector<HTMLElement>("[data-reply-opener]");
    if (opener !== null) return () => openReplyBox(opener);
    const reply = ownControl(turn, REPLY_BUTTON) ?? ownControl(root, REPLY_BUTTON);
    if (reply === null || reply.getAttribute("aria-disabled") === "true") return null;
    return () => {
      reply.click();
      dockedReplyFrom.current = turn;
    };
  };
  const giveBackFromThread = (comment: string) => {
    const from = replyOrigins.current.get(comment);
    const turn =
      from?.isConnected === true
        ? from
        : turnList.current?.querySelector<HTMLElement>(
            `[data-turn="${CSS.escape(conversationItemId("comment", comment))}"]`
          );
    turn?.focus();
  };
  useKeymap("issue", [
    ...(
      [
        ["issue.next-turn", 1],
        ["issue.previous-turn", -1],
      ] as const
    ).map(([action, step]) => ({
      action,
      run: () =>
        roveFocus(
          [...(turnList.current?.querySelectorAll<HTMLElement>(TURN_ROW) ?? [])],
          closestMatching(document.activeElement, TURN_ROW),
          step
        ),
      when: () => visible && turnList.current?.querySelector(TURN_ROW) != null,
    })),
    {
      action: "issue.toggle-turn",
      run: () => onFocusedTurn(toggleOf)?.(),
      when: () => onFocusedTurn(toggleOf) !== null,
    },
    {
      action: "issue.reply-turn",
      run: () => onFocusedTurn(replyOf)?.(),
      when: () => onFocusedTurn(replyOf) !== null,
    },
    {
      action: "issue.copy-turn-ref",
      run: () => {
        const reference = turnRef();
        if (reference !== undefined) void copyText(reference);
      },
      when: () => turnRef() !== undefined,
    },
    {
      // The turn's own page address: its issue's Conversation, landed on the turn.
      action: "issue.copy-turn-address",
      run: () => {
        const route = parseNodeReference(turnRef() ?? "");
        if (route !== undefined) {
          void copyText(`${window.location.origin}${buildReferencePath(route)}`);
        }
      },
      when: () => turnRef() !== undefined,
    },
  ]);
  return {
    beginDockedReply: () => {
      dockedReplyFrom.current = null;
    },
    giveBackFromDockedReply: () => {
      const from = dockedReplyFrom.current;
      dockedReplyFrom.current = null;
      if (from?.isConnected === true) from.focus();
    },
    giveBackFromThread,
    keyedThreads,
    openThreads,
    setThreadOpen,
    turnList,
  };
}
