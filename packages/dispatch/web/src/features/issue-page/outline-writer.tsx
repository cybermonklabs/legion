import { type MutationKey, useQuery } from "@tanstack/react-query";
import { createContext, type ReactNode, useCallback, useContext, useMemo, useState } from "react";

import type { Actor, Comment, MessageDelivery, MessageRead } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import type { ReplyTarget } from "../conversation/composer-model";
import { deliveryMode } from "../conversation/delivery";
import { redirectedReplyTarget, useHeldReplyTo, useHeldSends } from "../conversation/held-sends";
import {
  type SentRequest,
  type StatementAnchor,
  sameStatement,
} from "../conversation/send-request";
import { type AgentText, useAgentText } from "../refs/AgentName";
import { EVENT_FRESH } from "../refs/reference-target";
import { buildIssuePath, parseNodeReference, sessionOfAddress } from "../refs/routes";
import { wordsOf } from "../refs/tree-fragment";
import type { TreeNode } from "./issue-tree";
import { isStatement } from "./issue-tree";
import { criterionOffered } from "./model-rows/criterion-write";
import type { RoomFacts } from "./model-rows/room-target";
import { useRoomFacts } from "./model-rows/room-target";
import { issueAsksQuery, issueCommentsQuery, issueMessagesQuery } from "./useIssueTree";

/**
 * Where the reader writes on the issue page (R16): the one line at the foot
 * of the outline, and what it is written to. With nothing set it writes a new comment on the
 * issue, or a question with the Question chip pressed. A row's Reply - its control, or `r` - sets
 * it to reply to that row, and an open question's Answer - its control, one of its options, or
 * `a` - sets it to answer that question, which the ask's own body takes and the line does not;
 * Escape, or the line's own control, sets it back.
 *
 * The state lives above the outline and the line both, because each sets it and both draw it:
 * a row draws the options the reader picked as picked, and the line names what it replies to.
 */
export interface OutlineWriter {
  /** The open question the line answers; undefined while it answers none. */
  readonly answer: AnswerTarget | undefined;
  /** Sets the line to answer `node`, an open question, and puts no caret in the box. */
  readonly answerNode: (node: TreeNode) => void;
  /** Sets the line to change the reader's own answer to `node`, an answered question: the line
   *  answers it again over the answer it holds (`AnswerTarget.replacing`). */
  readonly changeAnswer: (node: TreeNode) => void;
  /** Sets the line back to a new comment on the issue. */
  readonly clear: () => void;
  /** The node the line writes a criterion under (`CriterionItem`), where its row's Write a
   *  criterion aimed it at one (`criterionUnder`); undefined otherwise. Like `statement`, it
   *  never stands beside a reply, a statement, an answer or an edit. */
  readonly criterion: CriterionItem | undefined;
  /** Sets the line to write a criterion under `node`, an ask, a comment or a message
   *  (`criterionItemOf`), and puts the caret in it: it ends the reply, the statement, the
   *  answer and the edit, and setting any of them, or `clear`, ends it. A node that is none of
   *  those sets nothing. */
  readonly criterionUnder: (node: TreeNode) => void;
  /** Asks the outline to take the reader to what a write from the line just landed
   *  (`Landing`), once its read has drawn it. */
  readonly land: (landing: Landing) => void;
  /** Says the outline has taken the reader there, which ends the ask. */
  readonly landed: () => void;
  /** Where the outline is asked to take the reader after a write; undefined once it has. */
  readonly landing: Landing | undefined;
  /** Puts the caret in the line, once it is drawn. */
  readonly focus: () => void;
  /** Says the criterion line's write is out (`true`) or settled (`false`). While it is out the
   *  line keeps its target, as it does while a send of the line's own is out: every other
   *  target, and `clear`, is refused, so the criterion line keeps its text and draws its
   *  refusal. */
  readonly holdCriterion: (out: boolean) => void;
  /** The issue the page is of: only its own nodes take a criterion (`takesCriterion`), and a row
   *  is edited in the room of its tree read (`useNodeEditing.ts`). */
  readonly issueKey: string;
  /** What the line writes with no reply set: a comment, or a question. */
  readonly kind: WriterKind;
  /** Picks `label` of `node`'s options, or lets it go, and sets the line to answer `node`. */
  readonly pick: (node: TreeNode, label: string) => void;
  // A reply the line sent that Dispatch refused holds the line on it until the reader ends the
  // reply in the line (`clear`), the reader's own move, which takes the refusal off it: the rule
  // `statement` states for a refused send's target, its reply or its statement.
  /** The entry the line replies to; null while it replies to none. */
  readonly reply: ReplyTarget | null;
  /** Sets the line to reply to `node` and puts the caret in it. An open question's Reply is a
   *  question back, which hands the turn to whoever asked it; an answered or resolved one's is a
   *  word under it that carries no turn, so a reader can answer the thread they are reading. */
  readonly replyTo: (item: ReplyItem | StatementItem) => void;
  /** The name the line's writes are held under (`held-sends.ts`), so a send that is out, or was
   *  refused, comes back with its draft and its reply whatever unmounts the line. */
  readonly sendKey: MutationKey;
  readonly setKind: (kind: WriterKind) => void;
  readonly setReply: (reply: ReplyTarget | null) => void;
  /** The statement the line comments on (`StatementTarget`), where a Reply on a statement's row,
   *  its control or `r`, aimed it at one (`replyTo`); or, where the line mounted over a send held
   *  under its name that was aimed at one (`SentRequest.statement`) - after the reader left the
   *  issue and came back, or a reload - that send's document, words and row, which are all a held
   *  send keeps of it, so a send from the line lands on that row either way; undefined otherwise.
   *  It never stands beside a reply, an answer or an edit: setting it ends them, and setting any
   *  of them, or `clear`, ends it. One rule holds the line on what a refused send was aimed at: a
   *  send of the line's own that Dispatch refused, held under its name, holds the line on its
   *  target - its reply or its statement - and every other aim puts the line back there instead
   *  (`keptOnReply`, `keptOnStatement`), until the reader ends that target in the line - the
   *  target line's control or Escape, which reach `clear`. That is the reader's own move of the
   *  line's address, which the line's composer carries the refusal with to the issue
   *  (`HeldSends.carry`), its words kept, unless the issue keeps a draft of the reader's own,
   *  which no move writes over: the refusal then stays with its target, which still holds the
   *  line. Once it is carried every aim takes effect again, and a line that mounts again over
   *  that send writes on the issue. */
  readonly statement: StatementTarget | undefined;
  /** Whether `node` carries the act that writes a criterion under it: an ask, a comment or a
   *  message (`criterionItemOf`) whose own address names this issue, on an issue whose tree
   *  read answers a tree and no `view_of` (`criterionOffered`). False for every node while the
   *  cache holds no tree read of the issue (`useRoomFacts`). */
  readonly takesCriterion: (node: TreeNode) => boolean;
  /** Picks `label` of the options of the question the line answers, or lets it go. */
  readonly toggle: (label: string) => void;
  // A send of the line's own that Dispatch refused holds the line on its target through the
  // issue's own Reply too, as through every other aim (`statement`): only `clear`, which the
  // target line's control and Escape reach, lets it go.
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
   *  they just wrote; `self`: `under`'s own row - the statement a comment on its text was
   *  written on, which draws no row for it, and the comment they edited. */
  readonly to: "newest" | "self";
  /** What the write was made under: the item the line replied to, the statement it commented
   *  on, or the comment it edited; undefined for a write on the issue itself. */
  readonly under:
    | { readonly id: string; readonly kind: "ask" | "comment" | "message" | "statement" }
    | undefined;
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

/** The writers whose criterion line has its write out (`OutlineWriter.holdCriterion`), each by
 *  the identity of its memoised `sendKey` array: a `WeakSet` matches that one array, never an
 *  equal key made elsewhere, where `HeldSends` matches a key by its hash. While one is out, that
 *  writer's targets and `clear` are refused, as they are while a send of the line's own is out
 *  (`HeldSends.sendingNow`). */
const criterionOut = new WeakSet<MutationKey>();

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
  // The statement the line comments on, if it does: set by a Reply on a statement's row, and
  // taken up from the send held under the line's name, as the reply is (`useHeldReplyTo`), so a
  // line that mounts again over a refused comment on a statement names the statement it was
  // written to, sends it there and lands on its row. Ended by every other target the line takes
  // and by `clear`, so no path holds it beside a reply, an answer or an edit.
  const [statement, setStatement] = useState<StatementTarget | undefined>(
    () => store.get(sendKey)?.request.statement
  );
  // The node the line writes a criterion under, if it does: set by a row's Write a criterion
  // alone, and ended, as the statement is, by every other target and by `clear`.
  const [criterion, setCriterion] = useState<CriterionItem>();
  // The issue's tree read, which says whether the issue takes a criterion at all: it answers a
  // tree, and no `view_of`, which no node of a view of another issue takes (`criterionTakenBy`).
  const facts = useRoomFacts(issueKey);
  const agentText = useAgentText();
  // The reads the outline is drawn from, under the keys it reads them by and kept fresh as it
  // keeps them (`reference-target.ts`'s `EVENT_FRESH`), so a reply's target costs no request of its
  // own, and a window focus past the app's 30 s `staleTime` reads none of them again.
  const asks = useQuery({ ...issueAsksQuery(issueKey), ...EVENT_FRESH });
  const comments = useQuery({ ...issueCommentsQuery(issueKey), ...EVENT_FRESH });
  const messages = useQuery({ ...issueMessagesQuery(issueKey), ...EVENT_FRESH });
  const focus = useCallback(() => {
    // The field that is drawn: the line keeps its write field mounted, hidden, while it edits a
    // comment (`IssueComposer.tsx`), and a hidden field takes no caret.
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
    (item: ReplyItem | StatementItem) => {
      // While a send of the line's own is out, a Reply is refused: the reply it was sent with is
      // the one the line shows until Dispatch decides it, and the redirect below reads only a
      // send that was refused or none (`held-sends.ts`'s `redirectedReplyTarget`).
      if (store.sendingNow(sendKey)) return;
      // Nor while the criterion line's write is out: the line keeps its text and its refusal.
      if (criterionOut.has(sendKey)) return;
      // A send Dispatch refused keeps the line on its target, whatever the Reply is on: its
      // statement (`keptOnStatement`), or the entry its reply answers (`keptOnReply`, and
      // `redirectedReplyTarget` below). The caret goes back into the line on that target.
      const line = { setAnswer, setEditing, setReply, setStatement };
      // A statement is no entry of the conversation: the line writes a comment on the issue
      // anchored to the statement's whole words, and holds no reply beside it.
      if (item.kind === "statement") {
        const target = statementTargetOf(item);
        // A reply Dispatch refused is never sent as a comment on the statement instead.
        if (keptOnTarget(store.get(sendKey)?.request, line, target)) {
          focus();
          return;
        }
        setReply(null);
        setAnswer(undefined);
        setEditing(undefined);
        setCriterion(undefined);
        setStatement(target);
        focus();
        return;
      }
      if (keptOnStatement(store.get(sendKey)?.request.statement, line)) {
        focus();
        return;
      }
      const target = replyTargetOf(item, issueKey, comments.data, messages.data, agentText);
      setAnswer(undefined);
      setEditing(undefined);
      setStatement(undefined);
      setCriterion(undefined);
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
      if (criterionOut.has(sendKey)) return;
      const target = takesAnswer(node) ? answering(node) : undefined;
      if (target === undefined) return;
      // A send Dispatch refused keeps the line on its target, and the caret stays where it is.
      const line = { setAnswer, setEditing, setReply, setStatement };
      if (keptOnTarget(store.get(sendKey)?.request, line)) return;
      setReply(null);
      setEditing(undefined);
      setStatement(undefined);
      setCriterion(undefined);
      setAnswer((held) => (held?.askId === target.askId ? held : target));
    },
    [answering, sendKey, setReply, store]
  );
  const changeAnswer = useCallback(
    (node: TreeNode) => {
      if (store.sendingNow(sendKey)) return;
      if (criterionOut.has(sendKey)) return;
      const target = answering(node);
      const recorded = (asks.data ?? []).find((ask) => ask.id === target?.askId)?.answer;
      if (target === undefined || recorded === null || recorded === undefined) return;
      const line = { setAnswer, setEditing, setReply, setStatement };
      if (keptOnTarget(store.get(sendKey)?.request, line)) return;
      const offered = new Set((node.options ?? []).map((option) => option.label));
      setReply(null);
      setEditing(undefined);
      setStatement(undefined);
      setCriterion(undefined);
      setAnswer({
        ...target,
        replacing: { at: recorded.at, text: recorded.text ?? "" },
        selected: (recorded.selected ?? []).filter((label) => offered.has(label)),
      });
    },
    [answering, asks.data, sendKey, setReply, store]
  );
  const pick = useCallback(
    (node: TreeNode, label: string) => {
      if (store.sendingNow(sendKey)) return;
      if (criterionOut.has(sendKey)) return;
      const target = takesAnswer(node) ? answering(node) : undefined;
      if (target === undefined) return;
      const line = { setAnswer, setEditing, setReply, setStatement };
      if (keptOnTarget(store.get(sendKey)?.request, line)) return;
      setReply(null);
      setEditing(undefined);
      setStatement(undefined);
      setCriterion(undefined);
      setAnswer((held) => picked(held?.askId === target.askId ? held : target, label));
    },
    [answering, sendKey, setReply, store]
  );
  const toggle = useCallback((label: string) => {
    setAnswer((held) => (held === undefined ? held : picked(held, label)));
  }, []);
  const clear = useCallback(() => {
    if (store.sendingNow(sendKey)) return;
    if (criterionOut.has(sendKey)) return;
    // Ending the target in the line is the reader's own move of the line's address, which the
    // line's composer counts (`OutlineLook.end`) and carries a refused send held for that target
    // with (`HeldSends.carry`), so its hold on the line ends (`keptOnStatement`, `keptOnReply`):
    // a send the reader stopped goes on as a new write under a key of its own, saying the first
    // may have been delivered (`SendMovedError`). Where the issue keeps a draft of the reader's
    // own, the composer carries nothing, and the refusal keeps its target. The guard above has
    // run, so a send still out is never moved.
    setReply(null);
    setAnswer(undefined);
    setEditing(undefined);
    setStatement(undefined);
    setCriterion(undefined);
  }, [sendKey, setReply, store]);
  // biome-ignore lint/correctness/useExhaustiveDependencies: `setReply` is the setter `useHeldReplyTo` hands back from `useState`, one function for the provider's life, which the rule cannot see through the hook
  const writeOnIssue = useCallback(() => {
    if (store.sendingNow(sendKey)) return;
    if (criterionOut.has(sendKey)) return;
    const line = { setAnswer, setEditing, setReply, setStatement };
    if (keptOnTarget(store.get(sendKey)?.request, line)) {
      focus();
      return;
    }
    clear();
    focus();
  }, [clear, focus, sendKey, store]);
  const edit = useCallback(
    (id: string, body: string) => {
      if (store.sendingNow(sendKey)) return;
      if (criterionOut.has(sendKey)) return;
      const line = { setAnswer, setEditing, setReply, setStatement };
      if (keptOnTarget(store.get(sendKey)?.request, line)) {
        focus();
        return;
      }
      setReply(null);
      setAnswer(undefined);
      setStatement(undefined);
      setCriterion(undefined);
      setEditing({ body, id });
      focus();
    },
    [focus, sendKey, setReply, store]
  );
  const criterionUnder = useCallback(
    (node: TreeNode) => {
      // Refused while a send of the line's own is out, and while the criterion's own write is,
      // as every other target is; and a send Dispatch refused keeps the line on its statement or
      // on the entry its reply answers (`keptOnTarget`), as it does for every other aim, and the
      // caret goes back into the line on that target.
      if (store.sendingNow(sendKey)) return;
      if (criterionOut.has(sendKey)) return;
      const item = criterionItemOf(node);
      if (item === undefined) return;
      const line = { setAnswer, setEditing, setReply, setStatement };
      if (keptOnTarget(store.get(sendKey)?.request, line)) {
        focus();
        return;
      }
      setReply(null);
      setAnswer(undefined);
      setEditing(undefined);
      setStatement(undefined);
      setCriterion(item);
      focus();
    },
    [focus, sendKey, setReply, store]
  );
  const takesCriterion = useCallback(
    (node: TreeNode) => criterionTakenBy(node, issueKey, facts),
    [facts, issueKey]
  );
  const holdCriterion = useCallback(
    (out: boolean) => {
      if (out) criterionOut.add(sendKey);
      else criterionOut.delete(sendKey);
    },
    [sendKey]
  );
  const writer = useMemo<OutlineWriter>(
    () => ({
      answer,
      answerNode,
      changeAnswer,
      clear,
      criterion,
      criterionUnder,
      edit,
      editing,
      focus,
      holdCriterion,
      issueKey,
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
      statement,
      takesCriterion,
      toggle,
      writeOnIssue,
    }),
    [
      answer,
      answerNode,
      changeAnswer,
      clear,
      criterion,
      criterionUnder,
      edit,
      editing,
      focus,
      holdCriterion,
      issueKey,
      kind,
      landed,
      landing,
      pick,
      reply,
      replyTo,
      sendKey,
      setReply,
      statement,
      takesCriterion,
      toggle,
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

/** The writer's own setters, which `keptOnStatement` and `keptOnReply` set the line by. Neither
 *  moves the caret: each aim of the box puts it in the line itself, and an answer's aim puts none
 *  there. */
interface HeldLine {
  readonly setAnswer: (answer: undefined) => void;
  readonly setEditing: (editing: undefined) => void;
  readonly setReply: (reply: ReplyTarget | null) => void;
  readonly setStatement: (
    next: undefined | ((current: StatementTarget | undefined) => StatementTarget)
  ) => void;
}

/**
 * Keeps the line on `held`, the statement of a comment Dispatch refused that is held under the
 * line's name, and says whether it did; `keptOnReply` does the same for a refused reply. Both
 * keep one rule: a refused send of the line holds the line on its target - its reply or its
 * statement - until the reader ends that target in the line (`clear`, which the target line's
 * control and Escape reach), whose composer then carries the refusal to the issue (`statement`).
 * Until then a press that would aim the line elsewhere - a Reply, an Edit, the issue's own Reply,
 * or an Answer, an option or a change of answer, which the ask's own body takes - leaves the line
 * on that target instead, so the refused words are neither replaced by another target's seed nor
 * sent under another target; the box's own aims then put the caret back in the line, and an
 * answer's puts none there. A refusal the same words would meet again (`TARGET_AMBIGUOUS`) holds
 * the line only until the reader lets its target go.
 * A Reply on that same statement, `aimed`, takes its row's whole target. With none held it does
 * nothing. Each caller's `sendingNow` guard runs first, so a send held then was refused. The
 * writer's setters are handed in, so each setter that calls this keeps the dependencies it had.
 */
function keptOnStatement(
  held: StatementAnchor | undefined,
  line: HeldLine,
  aimed?: StatementTarget
): boolean {
  if (held === undefined) return false;
  line.setReply(null);
  line.setAnswer(undefined);
  line.setEditing(undefined);
  // The same statement is the same document and the same words.
  line.setStatement((current) =>
    aimed !== undefined && sameStatement(aimed, held)
      ? aimed
      : current !== undefined && sameStatement(current, held)
        ? current
        : held
  );
  return true;
}

/**
 * Keeps the line on `held`, the reply of a send Dispatch refused that is held under the line's
 * name, and says whether it did, by the rule `keptOnStatement` states: an Edit, a Reply on a
 * statement, the issue's own Reply, an Answer, an option or a change of answer leaves the line
 * on that reply, until the reader ends it in the line (`clear`), whose composer then carries the
 * refusal to the issue. A Reply on another entry is put back on it by `redirectedReplyTarget`.
 * With none held it does nothing. Each caller's `sendingNow` guard runs first, so a send held
 * then was refused.
 */
function keptOnReply(held: ReplyTarget | null | undefined, line: HeldLine): boolean {
  if (held === undefined || held === null) return false;
  line.setAnswer(undefined);
  line.setEditing(undefined);
  line.setStatement(undefined);
  line.setReply(held);
  return true;
}

/** Keeps the line on the target of `held`, the send held under the line's name - its statement
 *  (`keptOnStatement`, which a Reply on that same statement, `aimed`, takes whole) or its reply
 *  (`keptOnReply`) - and says whether it did. Every aim but a Reply on an entry, which
 *  `redirectedReplyTarget` puts back on a refused reply, runs it first, the issue's own Reply
 *  among them, so only the line's own end of the target (`clear`) lets that target go. */
function keptOnTarget(
  held: Pick<SentRequest, "replyTo" | "statement"> | undefined,
  line: HeldLine,
  aimed?: StatementTarget
): boolean {
  return keptOnStatement(held?.statement, line, aimed) || keptOnReply(held?.replyTo, line);
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

/** A statement of the issue's spec the line can comment on: a `requirement` block of the spec
 *  (`isStatement`), and never one of another document. It is no entry of the conversation, so
 *  what the line writes to it is a comment on the issue anchored to the statement, never a
 *  reply, and its row takes no thread read mark, since `conversationItem` answers none for it. */
export interface StatementItem extends Pick<TreeNode, "author" | "text" | "tree"> {
  /** The document the statement is a block of, by the slug its node id names it by, which the
   *  server's `AnchorInput.artifact` takes as it takes an artifact's id. */
  readonly artifact: string;
  /** The statement's node id, `<slug>#b-<block id>` (`tree-drafts.ts`'s `blockNodeId`): its row,
   *  which a landing names (`Landing.under`, of kind `statement`) and `IssueTree.tsx`'s
   *  `landingParent` finds by node id. A comment anchored to the statement is drawn in the margin
   *  beside it, and not under its row. */
  readonly id: string;
  readonly kind: "statement";
}

/**
 * The statement the line comments on (`OutlineWriter.statement`): the anchor its comment carries,
 * a `StatementAnchor` - the document and the statement's whole words, with no occurrence, so
 * words the document holds twice are refused by the server (`ErrTargetAmbiguous`) rather than
 * anchored to the wrong block, and the statement's row, which a send from the line lands on -
 * and the author of the row a Reply aimed the line from, where that row carries one. The box
 * hands its composer (`MentionComposer`'s `outline.anchor`) the anchor alone, so a held send
 * stores those three fields and never the statement's author, and a line that mounts again over
 * the send takes the statement up with no author and still lands on its row.
 */
export interface StatementTarget extends StatementAnchor {
  /** The author the statement's row names (`TreeNode.author`), which the target line names.
   *  Absent where the row carries none, and where the line took the statement up from a held
   *  send, whose target line names no author. */
  readonly author?: Actor;
  /** The statement's whole words as the server matches a quote against them: `wordsOf` its
   *  reading (`refs/tree-fragment.ts`), else its stored text where its read sent no reading. */
  readonly quote: string;
}

/** The statement the line comments on once a Reply on `item`'s row aims it there: the row's
 *  document, its author where it carries one, its whole words as the server matches a quote
 *  against them (`StatementTarget.quote`) and the row itself. The box reads the same rule off
 *  every statement row of the outline, so a draft it keeps for a statement is gone once no row
 *  reads as that statement any more. */
export function statementTargetOf(item: StatementItem): StatementTarget {
  return {
    artifact: item.artifact,
    ...(item.author === undefined ? {} : { author: item.author }),
    quote: item.tree === undefined ? item.text : wordsOf(item.tree),
    row: item.id,
  };
}

/**
 * The node a criterion is written under: an ask, a comment or a message of the outline. Its
 * `id` is the node's own row id, which the line marks (`data-criterion-under`) and a landing
 * names (`Landing.under`), and `IssueTree.tsx`'s `landingParent` finds the node by it; `kind` is
 * the conversation item the node is; `address` is the node's whole `dispatch://` address, which
 * the criterion names as its `parent`; and `author` and `kind` are what the line's target line
 * names the node by, with none of its words.
 */
export interface CriterionItem extends Pick<TreeNode, "author"> {
  /** The node's own `dispatch://` address (`TreeNode.reference`): the criterion's `parent`. */
  readonly address: string;
  /** The node's row id (`TreeNode.id`). */
  readonly id: string;
  /** The item of the conversation the node is, by the kind its address names. */
  readonly kind: "ask" | "comment" | "message";
}

/** The node a criterion can be written under, where `node` is one: an item of the conversation
 *  (`conversationItem`), as a row that takes a Reply is, with the address that names it, and
 *  its author where it carries one. Undefined for every other node - a statement, a check, a
 *  decision, the issue itself - and for one with no such address. Whether the act is offered on
 *  it is the writer's (`OutlineWriter.takesCriterion`), which also asks that the address names
 *  this issue and that the issue takes a criterion (`criterionOffered`). */
export function criterionItemOf(
  node: Pick<TreeNode, "author" | "id" | "reference" | "type">
): CriterionItem | undefined {
  const item = conversationItem(node);
  const address = node.reference;
  if (item === undefined || address === undefined) return undefined;
  return {
    address,
    ...(node.author === undefined ? {} : { author: node.author }),
    id: node.id,
    kind: item.kind,
  };
}

/** Whether `node` carries the act that writes a criterion under it, on the issue `issueKey`
 *  whose tree read answers `facts`: a node a criterion can be written under (`criterionItemOf`)
 *  whose own address names that issue, on an issue that takes a criterion (`criterionOffered`).
 *  No facts - an issue whose tree read the cache does not hold - offer it on no node. The writer
 *  answers it for each row (`OutlineWriter.takesCriterion`). */
export function criterionTakenBy(
  node: Pick<TreeNode, "author" | "id" | "reference" | "type">,
  issueKey: string,
  facts: RoomFacts | undefined
): boolean {
  // A node of another issue - a borrowed check's parent - is read here and written in its own
  // issue, so only an address naming this issue takes one.
  const item = criterionOffered(facts) ? criterionItemOf(node) : undefined;
  const route = item === undefined ? undefined : parseNodeReference(item.address);
  return route !== undefined && "key" in route && route.key === issueKey;
}

/** The item a row of the outline is, where the row takes a reply (R16): an ask in any state, a
 *  comment, a message. Every other row takes none in today's model, and neither does a comment or
 *  a message its author deleted: Dispatch refuses a reply to it (409 `POST_DELETED`, INFRA-1523
 *  R3), and its words are the deleted line, which no reply quotes. */
export function replyItemOf(node: TreeNode): ReplyItem | StatementItem | undefined {
  // A statement takes a Reply too, and is no item of the conversation, so it is read before one.
  const statement = statementItemOf(node);
  if (statement !== undefined) return statement;
  const item = conversationItem(node);
  return item === undefined || node.author === undefined || node.deletedAt !== undefined
    ? undefined
    : { ...item, author: node.author, text: node.text };
}

/** The statement a row of the outline is: a `requirement` block of the issue's spec, which the
 *  row carries as `TreeNode.statement` (`isStatement`), read from its node id
 *  `<slug>#b-<block id>` (`slugOfBlockNodeId`). A `requirement` block of the plan, the
 *  Verification or the decisions document is a block and nothing more (`tree-drafts.ts`'s
 *  `addDocumentBlocks`): it takes no Reply, and no comment is anchored to it from the line.
 *  Undefined for every other row. */
function statementItemOf(node: TreeNode): StatementItem | undefined {
  if (!isStatement(node)) return undefined;
  const artifact = slugOfBlockNodeId(node.id);
  if (artifact === undefined) return undefined;
  return {
    artifact,
    ...(node.author === undefined ? {} : { author: node.author }),
    id: node.id,
    kind: "statement",
    text: node.text,
    ...(node.tree === undefined ? {} : { tree: node.tree }),
  };
}

/** The slug of the document a block node's id names, `<slug>#b-<block id>`, read as
 *  `issue-artifacts.ts`'s `documentOfNode` reads it, with no artifact list: the document by the
 *  slug the server's anchor takes. `undefined` for a node that is no block, and for an id whose
 *  slug or block id is empty. */
function slugOfBlockNodeId(nodeId: string): string | undefined {
  const cut = nodeId.indexOf("#b-");
  return cut <= 0 || cut + 3 === nodeId.length ? undefined : nodeId.slice(0, cut);
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
