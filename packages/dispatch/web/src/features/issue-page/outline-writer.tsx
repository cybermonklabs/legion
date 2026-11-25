import { type MutationKey, useQuery } from "@tanstack/react-query";
import { createContext, type ReactNode, useCallback, useContext, useMemo, useState } from "react";

import type { Actor, Comment, MessageDelivery, MessageRead } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import type { ReplyTarget } from "../conversation/composer-model";
import { deliveryMode } from "../conversation/delivery";
import { redirectedReplyTarget, useHeldReplyTo, useHeldSends } from "../conversation/held-sends";
import { type AgentText, useAgentText } from "../refs/AgentName";
import { EVENT_FRESH } from "../refs/reference-target";
import { buildIssuePath, parseNodeReference, sessionOfAddress } from "../refs/routes";
import type { TreeNode } from "./issue-tree";
import { issueAsksQuery, issueCommentsQuery, issueMessagesQuery } from "./useIssueTree";

/**
 * Where the reader writes on the issue page (R16): the one line at the foot
 * of the outline, and what it is written to. With nothing set it writes a new comment on the
 * issue, or a question with the Question chip pressed. A row's Reply - its control, or `r` - sets
 * it to reply to that row, and an open question's Answer - its control, one of its options, or
 * `a` - sets it to answer that question; Escape, or the line's own control, sets it back.
 *
 * The state lives above the outline and the line both, because each sets it and both draw it:
 * a row draws the options the reader picked as picked, and the line names what it answers.
 */
export interface OutlineWriter {
  /** The open question the line answers; undefined while it answers none. */
  readonly answer: AnswerTarget | undefined;
  /** Sets the line to answer `node`, an open question, and puts the caret in it. */
  readonly answerNode: (node: TreeNode) => void;
  /** Sets the line to change the reader's own answer to `node`, an answered question: the line
   *  answers it again over the answer it holds (`AnswerTarget.replacing`). */
  readonly changeAnswer: (node: TreeNode) => void;
  /** Sets the line back to a new comment on the issue. */
  readonly clear: () => void;
  /** Asks the outline to take the reader to what a write from the line just landed
   *  (`Landing`), once its read has drawn it. */
  readonly land: (landing: Landing) => void;
  /** Says the outline has taken the reader there, which ends the ask. */
  readonly landed: () => void;
  /** Where the outline is asked to take the reader after a write; undefined once it has. */
  readonly landing: Landing | undefined;
  /** Puts the caret in the line, once it is drawn. */
  readonly focus: () => void;
  /** What the line writes with no reply set: a comment, or a question. */
  readonly kind: WriterKind;
  /** Picks `label` of `node`'s options, or lets it go, and sets the line to answer `node`. */
  readonly pick: (node: TreeNode, label: string) => void;
  /** The entry the line replies to; null while it replies to none. */
  readonly reply: ReplyTarget | null;
  /** Sets the line to reply to `node` and puts the caret in it. An open question's Reply is a
   *  question back, which hands the turn to whoever asked it; an answered or resolved one's is a
   *  word under it that carries no turn, so a reader can answer the thread they are reading. */
  readonly replyTo: (item: ReplyItem) => void;
  /** The name the line's writes are held under (`held-sends.ts`), so a send that is out, or was
   *  refused, comes back with its draft and its reply whatever unmounts the line. */
  readonly sendKey: MutationKey;
  readonly setKind: (kind: WriterKind) => void;
  readonly setReply: (reply: ReplyTarget | null) => void;
  /** Sets the line back to a new comment on the issue and puts the caret in it: the issue's own
   *  Reply, whose reply is a comment on the issue. */
  readonly writeOnIssue: () => void;
  /** Sets the line to edit the reader's own comment `id`, its words `body`, which Save writes
   *  back: the old thread card's Edit, which the comment's facts in the panel now carry. */
  readonly edit: (id: string, body: string) => void;
  /** The comment the line is editing, if it is. */
  readonly editing: { readonly body: string; readonly id: string } | undefined;
}

/** The kinds the line writes in today's model: a comment, and a question, which is an ask. A
 *  statement and a decision written from the line need writes that today's model does not
 *  have, and the line draws no chip that writes nothing. */
export type WriterKind = "ask" | "comment";

/**
 * Where the page takes the reader once a write from the line has landed (R16): the row of what
 * they wrote, its branch unfolded, inside the viewport and focused, so their own new words are
 * never behind a fold and the focus never falls to the page with nothing on it, which would send
 * the next `j` back to the first row - as the prototype's send jumps to the node it made.
 */
export interface Landing {
  /** `newest`: the reader's newest node under `under` - the reply, the comment or the question
   *  they just wrote; `self`: `under`'s own row - the question they answered, whose answer is
   *  drawn on its row, and the comment they edited. */
  readonly to: "newest" | "self";
  /** What the write was made under: the item the line replied to or answered, or the comment
   *  it edited; undefined for a write on the issue itself. */
  readonly under: { readonly id: string; readonly kind: "ask" | "comment" | "message" } | undefined;
}

/** The question the line answers. */
export interface AnswerTarget {
  /** The ask's own id, which the answer is written to. */
  readonly askId: string;
  /** Whether the ask takes more than one of its options. */
  readonly multiple: boolean;
  /** The question's row in the outline. */
  readonly nodeId: string;
  /** The moment of the reader's own answer this one replaces (`expected_answer_at`), where the
   *  line changes an answer rather than giving the first; its words seed the line. */
  readonly replacing?: { readonly at: string; readonly text: string };
  /** The options picked, by their labels, in the order they were picked. */
  readonly selected: readonly string[];
}

/** One item of the conversation a row of the outline is, by its own id: an ask, a comment or a
 *  message. Undefined for every other row, which takes no reply in today's model. */
export function conversationItem(
  node: Pick<TreeNode, "reference" | "type">
): { readonly id: string; readonly kind: "ask" | "comment" | "message" } | undefined {
  if (node.type !== "ask" && node.type !== "comment" && node.type !== "message") return undefined;
  const route = node.reference === undefined ? undefined : parseNodeReference(node.reference);
  if (route === undefined || !("id" in route)) return undefined;
  return route.kind === "ask" || route.kind === "comment" || route.kind === "message"
    ? { id: route.id, kind: route.kind }
    : undefined;
}

/** Whether `node` carries the control that answers it: an open question. */
export function takesAnswer(node: Pick<TreeNode, "reference" | "state" | "type">): boolean {
  return node.type === "ask" && node.state === "open" && conversationItem(node) !== undefined;
}

const OutlineWriterContext = createContext<OutlineWriter | undefined>(undefined);

/** The writer the page's outline and its line share; undefined outside an issue page's body. */
export function useOutlineWriter(): OutlineWriter | undefined {
  return useContext(OutlineWriterContext);
}

/** How many frames the caret waits for the line to be drawn: under a filter the line is drawn
 *  only once there is something to write, which is the commit after the target is set. */
const DRAWING_FRAMES = 10;

/** The line's field, wherever the page draws it (`IssueComposer.tsx`). */
const COMPOSER_FIELD = '[data-testid="issue-composer"] textarea';

/** Holds the writer for one issue's body. */
export function OutlineWriterProvider({
  children,
  issueKey,
}: {
  children: ReactNode;
  issueKey: string;
}): ReactNode {
  const sendKey = useMemo<MutationKey>(() => ["issue-composer", issueKey], [issueKey]);
  const store = useHeldSends();
  const [reply, setReply] = useHeldReplyTo(sendKey);
  const [kind, setKind] = useState<WriterKind>(() =>
    store.get(sendKey)?.request.kind === "ask" ? "ask" : "comment"
  );
  const [answer, setAnswer] = useState<AnswerTarget | undefined>(undefined);
  const [landing, setLanding] = useState<Landing | undefined>(undefined);
  const landed = useCallback(() => setLanding(undefined), []);
  // The reader's own comment the line is editing, if it is: its id and its words as they stand.
  const [editing, setEditing] = useState<{ readonly body: string; readonly id: string }>();
  const agentText = useAgentText();
  // The reads the outline is drawn from, under the keys it reads them by and kept fresh as it
  // keeps them (`reference-target.ts`'s `EVENT_FRESH`), so a reply's target costs no request of its
  // own, and a window focus past the app's 30 s `staleTime` reads none of them again.
  const asks = useQuery({ ...issueAsksQuery(issueKey), ...EVENT_FRESH });
  const comments = useQuery({ ...issueCommentsQuery(issueKey), ...EVENT_FRESH });
  const messages = useQuery({ ...issueMessagesQuery(issueKey), ...EVENT_FRESH });
  const focus = useCallback(() => {
    // The field that is drawn: the line keeps its write field mounted, hidden, while it answers
    // a question or edits a comment (`IssueComposer.tsx`), and a hidden field takes no caret.
    const place = (left: number) => {
      const field = [...document.querySelectorAll<HTMLTextAreaElement>(COMPOSER_FIELD)].find(
        (one) => one.closest("[hidden]") === null
      );
      if (field !== undefined) {
        field.focus({ preventScroll: true });
        return;
      }
      if (left > 0) requestAnimationFrame(() => place(left - 1));
    };
    requestAnimationFrame(() => place(DRAWING_FRAMES));
  }, []);
  const replyTo = useCallback(
    (item: ReplyItem) => {
      // While a send of the line's own is out, a Reply is refused: the reply it was sent with is
      // the one the line shows until Dispatch decides it, and the redirect below reads only a
      // send that was refused or none (`held-sends.ts`'s `redirectedReplyTarget`).
      if (store.sendingNow(sendKey)) return;
      const target = replyTargetOf(item, issueKey, comments.data, messages.data, agentText);
      setAnswer(undefined);
      setEditing(undefined);
      setReply(redirectedReplyTarget(store, sendKey, target));
      focus();
    },
    [agentText, comments.data, focus, issueKey, messages.data, sendKey, setReply, store]
  );
  const answering = useCallback((node: TreeNode): AnswerTarget | undefined => {
    const item = conversationItem(node);
    if (item?.kind !== "ask") return undefined;
    return { askId: item.id, multiple: node.multiple === true, nodeId: node.id, selected: [] };
  }, []);
  const answerNode = useCallback(
    (node: TreeNode) => {
      // As a Reply is, every press that sets the line to something else is refused while a send
      // of the line's own is out: the reply it was sent with stays the one the line shows.
      if (store.sendingNow(sendKey)) return;
      const target = takesAnswer(node) ? answering(node) : undefined;
      if (target === undefined) return;
      setReply(null);
      setEditing(undefined);
      setAnswer((held) => (held?.askId === target.askId ? held : target));
      focus();
    },
    [answering, focus, sendKey, setReply, store]
  );
  const changeAnswer = useCallback(
    (node: TreeNode) => {
      if (store.sendingNow(sendKey)) return;
      const target = answering(node);
      const recorded = (asks.data ?? []).find((ask) => ask.id === target?.askId)?.answer;
      if (target === undefined || recorded === null || recorded === undefined) return;
      const offered = new Set((node.options ?? []).map((option) => option.label));
      setReply(null);
      setEditing(undefined);
      setAnswer({
        ...target,
        replacing: { at: recorded.at, text: recorded.text ?? "" },
        selected: (recorded.selected ?? []).filter((label) => offered.has(label)),
      });
      focus();
    },
    [answering, asks.data, focus, sendKey, setReply, store]
  );
  const pick = useCallback(
    (node: TreeNode, label: string) => {
      if (store.sendingNow(sendKey)) return;
      const target = takesAnswer(node) ? answering(node) : undefined;
      if (target === undefined) return;
      setReply(null);
      setEditing(undefined);
      setAnswer((held) => picked(held?.askId === target.askId ? held : target, label));
      focus();
    },
    [answering, focus, sendKey, setReply, store]
  );
  const clear = useCallback(() => {
    if (store.sendingNow(sendKey)) return;
    setReply(null);
    setAnswer(undefined);
    setEditing(undefined);
  }, [sendKey, setReply, store]);
  const writeOnIssue = useCallback(() => {
    if (store.sendingNow(sendKey)) return;
    clear();
    focus();
  }, [clear, focus, sendKey, store]);
  const edit = useCallback(
    (id: string, body: string) => {
      if (store.sendingNow(sendKey)) return;
      setReply(null);
      setAnswer(undefined);
      setEditing({ body, id });
      focus();
    },
    [focus, sendKey, setReply, store]
  );
  const writer = useMemo<OutlineWriter>(
    () => ({
      answer,
      answerNode,
      changeAnswer,
      clear,
      edit,
      editing,
      focus,
      kind,
      land: setLanding,
      landed,
      landing,
      pick,
      reply,
      replyTo,
      sendKey,
      setKind,
      setReply,
      writeOnIssue,
    }),
    [
      answer,
      answerNode,
      changeAnswer,
      clear,
      edit,
      editing,
      focus,
      kind,
      landed,
      landing,
      pick,
      reply,
      replyTo,
      sendKey,
      setReply,
      writeOnIssue,
    ]
  );
  return <OutlineWriterContext.Provider value={writer}>{children}</OutlineWriterContext.Provider>;
}

/** `target` with `label` picked, or let go where it was picked: an ask that takes one option
 *  holds the last one picked, and one that takes several holds each in the order picked. */
function picked(target: AnswerTarget, label: string): AnswerTarget {
  const selected = target.selected.includes(label)
    ? target.selected.filter((one) => one !== label)
    : target.multiple
      ? [...target.selected, label]
      : [label];
  return { ...target, selected };
}

/** One item of the conversation the line can reply to: a row of the outline that takes a reply,
 *  or a turn of a thread the panel's history draws. */
export interface ReplyItem {
  readonly author: Actor;
  readonly id: string;
  readonly kind: "ask" | "comment" | "message";
  /** Its words, which the line quotes. */
  readonly text: string;
}

/** The item a row of the outline is, where the row takes a reply (R16): an ask in any state, a
 *  comment, a message. Every other row takes none in today's model, and neither does a comment or
 *  a message its author deleted: Dispatch refuses a reply to it (409 `POST_DELETED`, INFRA-1523
 *  R3), and its words are the deleted line, which no reply quotes. */
export function replyItemOf(node: TreeNode): ReplyItem | undefined {
  const item = conversationItem(node);
  return item === undefined || node.author === undefined || node.deletedAt !== undefined
    ? undefined
    : { ...item, author: node.author, text: node.text };
}

/**
 * What the line quotes when the reader replies to `item`: its author, its words and the address
 * of its row. A reply to an agent's comment starts with a mention of that agent, and a reply to
 * a person's comment with the mentions that comment made, so the reply reaches whoever the
 * comment did; a reply in a message thread aimed at a session keeps that thread's delivery. A
 * session it mentions is named as the page names an author (`AgentText.author`): by its title,
 * the one its write stamped where the registry holds none, and never by its id (part G's R7).
 */
function replyTargetOf(
  item: ReplyItem,
  issueKey: string,
  comments: readonly Comment[] | undefined,
  messages: readonly MessageRead[] | undefined,
  text: Pick<AgentText, "author">
): ReplyTarget {
  const base = {
    actor: item.author,
    excerpt: item.text,
    id: item.id,
    to: buildIssuePath({ id: item.id, key: issueKey, kind: item.kind }),
  };
  if (item.kind === "ask") return { ...base, parentKind: "ask" };
  if (item.kind === "message") {
    const thread = (messages ?? []).find(
      (one) =>
        one.message.id === item.id || (one.replies ?? []).some((reply) => reply.id === item.id)
    );
    const delivery = thread === undefined ? undefined : threadDelivery(thread);
    return {
      ...base,
      parentKind: "message",
      ...(delivery === undefined ? {} : { thread: delivery }),
    };
  }
  const comment = (comments ?? []).find((one) => one.id === item.id);
  const author = comment?.author ?? item.author;
  const targets =
    author.kind === "session"
      ? [`session:${author.id}`]
      : (comment?.mentions ?? []).map((mention) => mention.target);
  const mentions = [...new Set(targets)].map((target) => {
    const sessionId = sessionOfAddress(target);
    const session: Actor =
      author.kind === "session" && author.id === sessionId
        ? author
        : { id: sessionId ?? "", kind: "session" };
    return {
      target,
      title: sessionId === undefined ? target.slice("role:".length) : text.author(session),
    };
  });
  return { ...base, mentions, parentKind: "comment" };
}

/** The delivery a reply into a message thread inherits: the root's target, in the mode of the
 *  thread's most recent attempt, root or reply. A thread aimed at no session has none. */
function threadDelivery(thread: MessageRead): ReplyTarget["thread"] | undefined {
  if (thread.message.target === null) return undefined;
  let last: MessageDelivery | undefined;
  for (const attempt of [
    ...(thread.message.deliveries ?? []),
    ...(thread.replies ?? []).flatMap((reply) => reply.deliveries ?? []),
  ]) {
    if (last === undefined || compareTimestamps(attempt.created_at, last.created_at) >= 0) {
      last = attempt;
    }
  }
  const mode = last === undefined ? undefined : deliveryMode(last.delivery);
  return { delivery: mode ?? "steer", target: thread.message.target };
}
