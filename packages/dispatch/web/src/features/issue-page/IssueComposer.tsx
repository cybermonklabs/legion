import { dispatchWritingBox } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Fragment,
  type ReactNode,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { ApiError, api } from "../../api/client";
import type {
  AnswerAskInput,
  AskOptionWithTrees,
  AskWithTrees,
  DisplayTree,
} from "../../api/types";
import { RefusableButton } from "../../components/RefusableButton";
import {
  pageComposer,
  pageComposerAnswerControls,
  pageComposerControl,
  pageRowAct,
} from "../../theme/classes";
import type { ReplyTarget } from "../conversation/composer-model";
import { useHeldSend } from "../conversation/held-sends";
import {
  keepFocusOnPress,
  MentionComposer,
  type OutlineKind,
  OutlineRefusal,
  OutlineTargetLine,
  sendOnEnter,
} from "../conversation/MentionComposer";
import { MentionField } from "../conversation/MentionField";
import { saveCommentEdit } from "../conversation/send-request";
import { answerAskInput } from "../inbox/answer-ask";
import { useAnswerDraft } from "../inbox/answer-drafts";
import { DeclineControl } from "../inbox/DeclineControl";
import { type AskPress, useOutstandingAnswerWrite } from "../inbox/outstanding-answers";
import { invalidateOwnerReads } from "../inbox/owner-reads";
import { useQuestionLabel } from "../inbox/question-label";
import { answerFailure } from "../inbox/useAskAnswerForm";
import { AgentName } from "../refs/AgentName";
import { PlainText } from "../refs/plain-text";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { useMediaQuery } from "../shell/useDialog";
import { outlineCommentsQuery, outlineMessagesQuery } from "./conversation-reads";
import { FOOT_HEIGHT_PROPERTY, HEADER_SCROLLS_QUERY } from "./identity-block";
import { firstLineOf } from "./issue-tree";
import { DETAILS_ATTRIBUTE } from "./margin-slot";
import {
  type AnswerTarget,
  type Landing,
  type OutlineWriter,
  useOutlineWriter,
} from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { TAB_STOP_ROW } from "./slots/stage";
import { issueAsksQuery } from "./useIssueTree";

/** The kinds the line writes, as its chips before Send, in this order: a comment, which is on at
 *  first, and a question, which is an ask of the issue with no options (R2). A later part adds a
 *  kind as one entry here. */
const KINDS: readonly OutlineKind[] = [
  {
    kind: "comment",
    label: (
      <>
        <span aria-hidden="true">“</span>
        <span {...pageChrome}>{CONTROL_LABELS.composer_comment}</span>
      </>
    ),
  },
  {
    kind: "ask",
    label: (
      <>
        <span aria-hidden="true">?</span>
        <span {...pageChrome}>{CONTROL_LABELS.composer_question}</span>
      </>
    ),
  },
];

/** The refusals of an answer that mean the page's reading of the question is out of date: the
 *  question, its answer or who may answer it changed under the line. */
const STALE_ASK: Readonly<Record<string, true>> = {
  ASK_ANSWER_CHANGED: true,
  ASK_APPROVAL_REVIEW: true,
  ASK_CLOSED: true,
  ASK_EDITED: true,
  ASK_RESOLVED: true,
  NOT_ANSWERER: true,
};

/** Where the box stands on the page, as the slot reads it off the address (`slots/foot.tsx`). */
export interface BoxPlace {
  /** A document is open in place of the outline (`IssueAddress.document`). */
  readonly documentOpen: boolean;
  /** A view is on (`IssueAddress.preset` is no `conversation`): the stage draws Spec,
   *  Verification, Plan or Decisions, and no outline. */
  readonly viewOn: boolean;
}

/**
 * Whether the box is drawn (R1): always while the address names no view and no document, and
 * otherwise only while the reader has asked to write - the writer holds a reply, a statement, an
 * answer or an edit, which a row's Reply, Answer or Edit sets there or in Details - or holds a
 * send of the line's own that is out or was refused. So a press is never answered by nothing, and
 * a refusal is never out of sight. The box reads it for itself, and the slot for the reach its
 * wrapper publishes (`useFootInset`).
 */
export function useComposerShown({ documentOpen, viewOn }: BoxPlace): boolean {
  const writer = useOutlineWriter();
  const held = useHeldSend(writer?.sendKey);
  if (writer === undefined) return false;
  return (
    (!viewOn && !documentOpen) ||
    held !== undefined ||
    writer.reply !== null ||
    writer.statement !== undefined ||
    writer.answer !== undefined ||
    writer.editing !== undefined
  );
}

/**
 * The one place to write on the new issue page, held at the foot of the screen by its slot
 * (the prototype's `#composer`):
 * one line, which Enter sends, while the outline runs on under it. It is the composer every box
 * of the dashboard is (`MentionComposer`), in the look only this page asks for (`OutlineLook`),
 * and its answer line mounts the same field (`MentionField`), so no file of the page holds a box
 * of its own.
 *
 * With no target it writes a comment on the issue, or a question with the Question chip pressed.
 * A row's Reply sets it to reply to that row - a comment, a message, an ask in any state - or to
 * comment on a statement, its target named above the line; an open question's Answer, or one of
 * its options, sets it to answer that question; the reader's own comment's Edit sets it to that
 * comment's words, which Save writes back (`outline-writer.tsx`).
 *
 * Once a write lands, the outline takes the reader to it (`Landing`): the row of what they wrote,
 * its branch unfolded, on screen and focused (R4). Under a view no outline is drawn to consume a
 * landing, so the box asks for none there and gives the focus back to where the reader came to
 * it from. Either way the focus never falls to the page's body as a line's controls unmount.
 *
 * Hidden rather than unmounted while it is not drawn (`useComposerShown`), and the line that
 * writes on the issue stays mounted, hidden, while the line answers a question or edits a
 * comment, so what the reader was typing is there when they come back to it.
 */
export function IssueComposer({
  documentOpen,
  isClosed,
  issueKey,
  viewOn,
}: BoxPlace & {
  readonly isClosed: boolean;
  readonly issueKey: string;
}): ReactNode {
  const writer = useOutlineWriter();
  const shown = useComposerShown({ documentOpen, viewOn });
  const phone = useMediaQuery(HEADER_SCROLLS_QUERY);
  const section = useRef<HTMLElement | null>(null);
  // The element that held the focus when the reader came to the box - a row, or a control of
  // Details - while it is still in the document; the view's tab-stop row once it is not.
  const cameFrom = useRef<HTMLElement | null>(null);
  // Each ask to give the focus back, acted on once the commit that ended the line's target has
  // drawn: a line's controls unmount in that commit, and the decline dialog's own focus return
  // runs as it unmounts, either of which would take the focus after an earlier move.
  const [givenBack, setGivenBack] = useState(0);
  // How far up from the screen's foot the box reaches while it is drawn, on the document element,
  // so a row that takes the focus is brought clear of it as it is of the band at the top
  // (`identity-block.ts`'s `bringOnScreen`). Unset while the box is hidden. The reach is the
  // section's own height - the box, the 8 px under it and the fade above it - which is how far up
  // it reaches while it is stuck to the foot, and not read off where it stands: at the outline's
  // end the read mark, which this reach moves down (`slots/foot.tsx`), lengthens the page, so a
  // box that has just shrunk stands above the foot, and a reach read off its place would hold the
  // mark that low, and the box that high, until the box next changed size.
  useLayoutEffect(() => {
    const element = section.current;
    if (!shown || element === null) return;
    const root = document.documentElement;
    const publish = () =>
      root.style.setProperty(FOOT_HEIGHT_PROPERTY, `${element.getBoundingClientRect().height}px`);
    publish();
    const sizes = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(publish);
    sizes?.observe(element);
    window.addEventListener("resize", publish);
    return () => {
      sizes?.disconnect();
      window.removeEventListener("resize", publish);
      root.style.removeProperty(FOOT_HEIGHT_PROPERTY);
    };
  }, [shown]);
  useEffect(() => {
    if (givenBack === 0) return;
    const from = cameFrom.current;
    cameFrom.current = null;
    // The element the reader came from while it is still drawn; else the tab-stop row of the
    // stage's tree, the outline's and each view's alike, outside Details' drawer: the row the
    // reader last stood on, or the view's own.
    const back =
      from?.isConnected === true && from.closest("[hidden]") === null
        ? from
        : [...document.querySelectorAll<HTMLElement>(`[data-issue-page] ${TAB_STOP_ROW}`)].find(
            (row) => row.closest(`aside[${DETAILS_ATTRIBUTE}]`) === null
          );
    back?.focus({ preventScroll: true });
  }, [givenBack]);
  if (writer === undefined) return null;
  const place: LinePlace = {
    atRest: !viewOn && !documentOpen,
    giveBack: () => setGivenBack((count) => count + 1),
    outlineDrawn: !viewOn,
    phone,
  };
  const writing = writer.answer === undefined && writer.editing === undefined;
  return (
    <section
      aria-label={CONTROL_LABELS.write}
      className={pageComposer.foot}
      data-testid="issue-composer"
      hidden={!shown}
      onFocus={(event) => {
        // Where the reader came to the box from. A move inside the box is no arrival, and nor is
        // one back from the decline dialog, which is drawn outside it (`DeclineControl`'s look),
        // or one from nowhere - the window taking the focus back, the dialog's own return of it -
        // which keeps the element the reader came from before.
        const from = event.relatedTarget;
        if (
          !(from instanceof HTMLElement) ||
          event.currentTarget.contains(from) ||
          from.closest('[role="dialog"]') !== null
        ) {
          return;
        }
        cameFrom.current = from;
      }}
      ref={section}
      {...authorText}
    >
      {writer.editing === undefined ? null : (
        <EditLine
          // One line per comment: its words in the box are that comment's.
          key={writer.editing.id}
          editing={writer.editing}
          issueKey={issueKey}
          place={place}
          writer={writer}
        />
      )}
      {writer.answer === undefined ? null : (
        <AnswerLine
          // One line per question: what the reader typed and picked is that question's.
          key={writer.answer.askId}
          issueKey={issueKey}
          place={place}
          target={writer.answer}
          writer={writer}
        />
      )}
      {/* Last, so the line that is drawn is the first field of the section, and hidden rather
          than unmounted while the line answers or edits: its draft is the reader's. */}
      <div data-composer-write="" hidden={!writing}>
        <WriteLine isClosed={isClosed} issueKey={issueKey} place={place} writer={writer} />
      </div>
    </section>
  );
}

/** What a line of the box reads of where it stands, to put the reader back once it is done. */
interface LinePlace {
  /** No view is on and no document is open, so the box stays drawn when the line's target ends
   *  (`useComposerShown`). */
  readonly atRest: boolean;
  /** Gives the focus back to where the reader came to the box from, once the commit that ends
   *  the line's target has drawn. */
  readonly giveBack: () => void;
  /** The stage draws the outline - no view is on, with or without a document open - whose tree
   *  takes the reader to a landing (`IssueTree.tsx`, the one consumer of one). */
  readonly outlineDrawn: boolean;
  /** 760 px and below, where each placeholder says its shorter words. */
  readonly phone: boolean;
}

/**
 * Ends the line's target and puts the reader back: on the outline, on the row `landing` names
 * once its read draws it (R4); under a view, where no outline is drawn to consume a landing and
 * one would act on the outline the next time it mounts, where the reader came to the box from.
 */
function finish(writer: OutlineWriter, place: LinePlace, landing: Landing): void {
  if (place.outlineDrawn) {
    writer.land(landing);
    writer.clear();
    return;
  }
  writer.clear();
  place.giveBack();
}

/** The line that writes a comment, a question, a reply, or a comment on a statement: the
 *  composer in the issue page's look (`OutlineLook`). */
function WriteLine({
  isClosed,
  issueKey,
  place,
  writer,
}: {
  isClosed: boolean;
  issueKey: string;
  place: LinePlace;
  writer: OutlineWriter;
}): ReactNode {
  const { reply, statement } = writer;
  // A statement's comment carries the statement's document, its whole words and its row alone,
  // never the item a Reply aimed the line with, so a send held over a refusal or a reload stores
  // those three fields and never the statement's tree.
  const anchor = useMemo(
    () =>
      statement === undefined
        ? undefined
        : { artifact: statement.artifact, quote: statement.quote, row: statement.row },
    [statement]
  );
  const owner = useMemo(() => ({ issueKey, kind: "issue" as const }), [issueKey]);
  // Whether the line is aimed at something: the entry a reply answers, or the statement a comment
  // is on; not for a write on the issue itself.
  const aimed = reply !== null || statement !== undefined;
  // What a landing names: that entry, or that statement's row, which a statement the line took
  // up from a held send names as well (`StatementAnchor.row`); nothing for a write on the issue.
  const under: Landing["under"] =
    reply !== null
      ? { id: reply.id, kind: reply.parentKind }
      : statement === undefined
        ? undefined
        : { id: statement.row, kind: "statement" };
  // A reply and a statement's comment are comments under the row they are aimed at, so the kinds
  // are drawn only while the line writes on the issue. A closed issue takes a comment and no
  // question, so the line writes a comment there whatever chip was pressed while it was open,
  // rather than holding a question the issue refuses.
  const writesKinds = !aimed && !isClosed;
  // The target line's control and Escape. With no target they leave the line. With the box drawn
  // at rest the line goes back to writing on the issue with the caret kept in it, ending the
  // target itself: the issue's own Reply keeps a refused send's target (`writeOnIssue`), and
  // this is the line's own end of it. Elsewhere ending the target hides the box (R1), so the
  // reader is put back: on the row they were replying to where the outline is drawn, and where
  // they came from under a view.
  const end = () => {
    if (!aimed) {
      place.giveBack();
      return;
    }
    if (place.atRest) {
      writer.clear();
      writer.focus();
      return;
    }
    finish(writer, place, { to: "self", under });
  };
  return (
    <MentionComposer
      box="issue-page-composer"
      closed={isClosed}
      kind={writesKinds ? writer.kind : "comment"}
      mutationKey={writer.sendKey}
      onClose={place.giveBack}
      onKindChange={(next) => {
        if (next === "ask" || next === "comment") writer.setKind(next);
        return undefined;
      }}
      // The reader is taken to what they wrote - their newest node under the row the line was
      // aimed at, or under the issue - and the line writes on the issue again. Neither is refused
      // here: the write store drops the send before it calls this, so no send is out by then.
      onSent={() => finish(writer, place, { to: "newest", under })}
      outline={{
        anchor,
        cancelReply: CONTROL_LABELS.write_on_issue,
        end,
        kept: { issue: ANSWER_LINES.kept_on_issue, statement: ANSWER_LINES.kept_on_statement },
        kinds: writesKinds ? KINDS : [],
        placeholder: aimed
          ? place.phone
            ? CONTROL_LABELS.reply_placeholder_phone
            : CONTROL_LABELS.reply_placeholder
          : place.phone
            ? CONTROL_LABELS.issue_placeholder_phone
            : CONTROL_LABELS.issue_placeholder,
        retry: CONTROL_LABELS.retry,
        send: CONTROL_LABELS.send,
        sending: ANSWER_LINES.sending,
        statementTwice: ANSWER_LINES.statement_twice,
        target:
          reply !== null ? (
            <ReplyTargetLine issueKey={issueKey} reply={reply} />
          ) : statement === undefined ? undefined : (
            <>
              <span {...pageChrome}>{CONTROL_LABELS.replying_to}</span>{" "}
              {/* A statement a Reply aimed the line at is named by its row's words; a row's
                  text is the coverage read's words, which leave every reference out, so its
                  tree's references are named whole. One taken up from a held send is its quote
                  alone, named in plain words as a row's statement is. */}
              <TargetWords
                markdown={statement.item?.text ?? statement.quote}
                namesAll={statement.item !== undefined}
                source={statement.item?.source}
                tree={statement.item?.tree}
              />
            </>
          ),
        uploadedUnanswered: ANSWER_LINES.uploaded_unanswered,
        uploading: ANSWER_LINES.uploading,
      }}
      owner={owner}
      replyTo={reply}
    />
  );
}

/** A reply's target line (R3): "Replying to:", the entry's author, and the entry's first line,
 *  read from the entry's own record in the reads the outline is drawn from (`outline-reads.ts`),
 *  so its tree costs no request of its own: only the read of the entry's kind is observed, and
 *  the outline keeps it fresh, so the line asks for it again on no mount. */
function ReplyTargetLine({ issueKey, reply }: { issueKey: string; reply: ReplyTarget }): ReactNode {
  const asks = useQuery({
    ...issueAsksQuery(issueKey),
    enabled: reply.parentKind === "ask",
    refetchOnMount: false,
  });
  const comments = useQuery({
    ...outlineCommentsQuery(issueKey),
    enabled: reply.parentKind === "comment",
    refetchOnMount: false,
  });
  const messages = useQuery({
    ...outlineMessagesQuery(issueKey),
    enabled: reply.parentKind === "message",
    refetchOnMount: false,
  });
  const tree =
    reply.parentKind === "ask"
      ? asks.data?.find((one) => one.id === reply.id)?.question_tree
      : reply.parentKind === "comment"
        ? comments.data?.find((one) => one.id === reply.id)?.body_tree
        : messages.data
            ?.flatMap((thread) => [thread.message, ...(thread.replies ?? [])])
            .find((one) => one.id === reply.id)?.body_tree;
  return (
    <>
      <span {...pageChrome}>{CONTROL_LABELS.replying_to}</span>{" "}
      <b className="font-semibold text-page-ink">
        <AgentName actor={reply.actor} inline />
      </b>{" "}
      <TargetWords
        markdown={reply.excerpt}
        source={textSourceOf(reply.parentKind, reply.id, issueKey)}
        tree={tree}
      />
    </>
  );
}

/** The reader's own comment, edited in the line: its words in the box, Save writes them back,
 *  and the outline draws the comment again from the read that answers after and takes the
 *  reader back to its row. */
function EditLine({
  editing,
  issueKey,
  place,
  writer,
}: {
  editing: { readonly body: string; readonly id: string };
  issueKey: string;
  place: LinePlace;
  writer: OutlineWriter;
}): ReactNode {
  const queryClient = useQueryClient();
  const comments = useQuery(outlineCommentsQuery(issueKey));
  const tree = comments.data?.find((one) => one.id === editing.id)?.body_tree;
  const owner = useMemo(() => ({ issueKey, kind: "issue" as const }), [issueKey]);
  const mutationKey = useMemo(
    () => [...writer.sendKey, "edit", editing.id],
    [editing.id, writer.sendKey]
  );
  // Escape, the target line's control, and a save that landed: the edit ends, and the reader is
  // back on the comment's row.
  const back = () =>
    finish(writer, place, { to: "self", under: { id: editing.id, kind: "comment" } });
  return (
    <MentionComposer
      box="issue-page-composer"
      edit={editing}
      mutationKey={mutationKey}
      onClose={back}
      onSent={back}
      outline={{
        cancelReply: CONTROL_LABELS.write_on_issue,
        end: back,
        kinds: [],
        placeholder: place.phone
          ? CONTROL_LABELS.edit_placeholder_phone
          : CONTROL_LABELS.edit_placeholder,
        retry: CONTROL_LABELS.retry,
        send: CONTROL_LABELS.save,
        sending: ANSWER_LINES.saving_field,
        target: (
          <>
            <span {...pageChrome}>{CONTROL_LABELS.editing}</span>{" "}
            <TargetWords
              markdown={editing.body}
              source={textSourceOf("comment", editing.id, issueKey)}
              tree={tree}
            />
          </>
        ),
        uploadedUnanswered: ANSWER_LINES.uploaded_unanswered,
        uploading: ANSWER_LINES.uploading,
      }}
      owner={owner}
      saveEdit={async (id, body, carried, bounds) => {
        const saved = await saveCommentEdit(id, body, carried, bounds);
        await queryClient.invalidateQueries({ queryKey: outlineCommentsQuery(issueKey).queryKey });
        return saved;
      }}
    />
  );
}

/** The line that answers an open question, once the issue's asks read holds it. A question gone
 *  from the read leaves the line nothing to answer, so the line goes back to writing on the issue
 *  rather than drawing an empty box. */
function AnswerLine({
  issueKey,
  place,
  target,
  writer,
}: {
  issueKey: string;
  place: LinePlace;
  target: AnswerTarget;
  writer: OutlineWriter;
}): ReactNode {
  const asks = useQuery(issueAsksQuery(issueKey));
  const ask = asks.data?.find((one) => one.id === target.askId);
  const missing = asks.isSuccess && ask === undefined;
  const clear = writer.clear;
  useEffect(() => {
    if (missing) clear();
  }, [clear, missing]);
  return ask === undefined ? null : (
    <AnswerForm ask={ask} issueKey={issueKey} place={place} target={target} writer={writer} />
  );
}

/**
 * The answer line (R5): the question named above it in the waiting colour, what the reader
 * picked beside it, the question's options as chips the keyboard reaches - the row's own options
 * are the pointer's - Decline to answer, and Send answer. A pick or words alone answer it. On a
 * phone the field has the line's whole width and the options and controls stand under it.
 * Escape, from any control of the line, Decline among them, and the target line's control take
 * the reader back to the question's row.
 *
 * The write is the Inbox's (`useOutstandingAnswerWrite`), so the question's row leaves the Inbox
 * as the answer goes and comes back if Dispatch refuses it, and what the reader typed is the
 * question's draft (`answer-drafts.ts`), which every surface of that question shares. A refusal
 * that says the question moved under the line reads the question again, so the line and the
 * outline draw it as it stands; one that says the answer changed takes the current answer's time
 * and words, so the next send is measured against it, and one that says the reader may not change
 * it ends the change, saying why. A question answered or closed elsewhere while the line held it
 * is not answered here: the line goes back to writing on the issue.
 *
 * Decline's dialog is drawn into `document.body` (`DeclineControl`'s look), so it is no part of
 * this form, but React still hands its keys to this form's handler, which leaves every key
 * inside it to the dialog. Decline stays inside the form's fieldset, which holds every control of
 * the line while an answer is out.
 */
function AnswerForm({
  ask,
  issueKey,
  place,
  target,
  writer,
}: {
  ask: AskWithTrees;
  issueKey: string;
  place: LinePlace;
  target: AnswerTarget;
  writer: OutlineWriter;
}): ReactNode {
  const queryClient = useQueryClient();
  const form = useRef<HTMLFormElement>(null);
  // The question as the write's label reads it: its plain-text projection, as the Inbox's card
  // labels its own answer (the reading list's `write-label`).
  const question = useQuestionLabel(ask);
  const [draft, setDraft] = useAnswerDraft(target.askId);
  // The current answer a change is measured against, where a refusal said the one the line
  // opened on has changed since; the line's own otherwise.
  const [current, setCurrent] = useState<{ readonly at: string }>();
  const replacing = current ?? target.replacing;
  // A change of the reader's own answer starts from its words and is this line's own text, never
  // the question's shared draft: a recorded answer is not unsent text.
  const [changed, setChanged] = useState(target.replacing?.text ?? "");
  // Why the change can no longer be sent, once Dispatch said the reader may not change it.
  const [ended, setEnded] = useState<string>();
  const [pickerOpen, setPickerOpen] = useState(false);
  const text = replacing === undefined ? draft : changed;
  const setText = replacing === undefined ? setDraft : setChanged;
  const back = () =>
    finish(writer, place, { to: "self", under: { id: target.askId, kind: "ask" } });
  // Each press carries the question it answers, as the Inbox's card's does (`AskPress`): a Retry
  // runs the newest render's write with the press's own values (`useKeyedWrite`).
  const mutation = useOutstandingAnswerWrite({
    label: (press: AnswerLinePress) => `Answer to “${press.questionWords}”`,
    method: "POST",
    // A change of an answer the server has already recorded leaves the Inbox alone: the
    // question has no open row there to take out.
    removesRow: (press) => press.input.expected_answer_at === undefined,
    write: (press: AnswerLinePress, bounds) => api.answerAsk(press.ask.id, press.input, bounds),
    onError: (error) => {
      if (!(error instanceof ApiError) || STALE_ASK[error.code ?? ""] !== true) return;
      void queryClient.invalidateQueries({ queryKey: ["issue", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", target.askId] });
      const asksRead = queryClient.invalidateQueries({
        queryKey: issueAsksQuery(issueKey).queryKey,
      });
      if (error.code === "NOT_ANSWERER") {
        setEnded(answerFailure({ error, method: undefined }).message);
      }
      if (error.code !== "ASK_ANSWER_CHANGED") return;
      // The answer moved under the change: the line takes the answer as it now stands from the
      // read that answers, its time for the next send's `expected_answer_at` and its words in
      // the field, which the refusal's own words say it shows.
      void asksRead.then(() => {
        const answer = queryClient
          .getQueryData<readonly AskWithTrees[]>(issueAsksQuery(issueKey).queryKey)
          ?.find((one) => one.id === target.askId)?.answer;
        if (answer === null || answer === undefined) return;
        setCurrent({ at: answer.at });
        setChanged(answer.text ?? "");
      });
    },
    onSuccess: () => {
      setText("");
      back();
      invalidateOwnerReads(queryClient, ask);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", target.askId] });
    },
  });
  const refused = mutation.error !== null;
  const pending = mutation.isPending;
  // The read has answered that the question is no longer one this line may answer: it was
  // answered or closed elsewhere. The line goes back to writing on the issue, taking the reader
  // to the question where the line held their focus, unless a refusal of their own answer is on
  // it, which they read first, or their own answer is still out.
  const gone = ask.state !== "open" && replacing === undefined;
  const leave = useRef(() => {});
  leave.current = () => {
    if (form.current?.contains(document.activeElement) === true) back();
    else writer.clear();
  };
  useEffect(() => {
    if (gone && !refused && !pending) leave.current();
  }, [gone, pending, refused]);
  // What was picked of the options the question still offers: an edit of the question can take
  // one away under the line.
  const options = ask.options ?? [];
  const selected = target.selected.filter((label) =>
    options.some((option) => option.label === label)
  );
  const chosen = selected.flatMap((label) => options.filter((option) => option.label === label));
  const refusal =
    ended ??
    (selected.length === 0 && text.trim() === "" ? CONTROL_LABELS.answer_placeholder : null);
  const send = () => {
    if (refusal !== null || pending) return;
    mutation.mutate({
      ask,
      input: answerInput(ask, selected, text, replacing),
      questionWords: question.line(),
    });
  };
  const failure = mutation.error === null ? null : answerFailure(mutation);
  const source = textSourceOf("ask", ask.id, issueKey);
  return (
    <form
      aria-label={CONTROL_LABELS.answering}
      className={`${pageComposer.box} ${pageComposer.answering}`}
      data-answering={target.askId}
      data-issue-composer=""
      onKeyDown={(event) => {
        if (event.key !== "Escape") return;
        if (event.target instanceof Element && event.target.closest('[role="dialog"]') !== null) {
          return;
        }
        event.preventDefault();
        event.stopPropagation();
        back();
      }}
      onSubmit={(event) => {
        event.preventDefault();
        send();
      }}
      ref={form}
    >
      {question.reads}
      <fieldset className="flex min-w-0 flex-col gap-[3px] border-0 p-0" disabled={pending}>
        <OutlineTargetLine
          clearLabel={CONTROL_LABELS.write_on_issue}
          onClear={back}
          tone={pageComposer.targetAnswering}
        >
          <span {...pageChrome}>{CONTROL_LABELS.answering}</span>{" "}
          <b className="font-semibold text-page-ink">
            <TargetWords markdown={ask.question} source={source} tree={ask.question_tree} />
          </b>
          {chosen.length === 0 ? null : (
            <>
              <span {...pageChrome}> · {CONTROL_LABELS.answer_chose}</span>{" "}
              {chosen.map((option, index) => (
                <Fragment key={option.label}>
                  {index === 0 ? null : ", "}“<OptionWords option={option} source={source} />”
                </Fragment>
              ))}
            </>
          )}
        </OutlineTargetLine>
        {/* The line wraps for what asks a row of its own: the open picker's list, on a phone the
            options and the controls under the field, which has the line's whole width, and at
            any width a row of them that would leave the field under 240 px. Words alone make an
            answer, so the field keeps the room to type in that it has on a phone (`min-w-60!`
            over its own `min-w-0`) and such a row goes under it; a row wider than the line
            wraps inside the line's width (`pageComposerAnswerControls`). */}
        <div className={`${pageComposer.line} flex-wrap`}>
          <MentionField
            body={text}
            disabled={pending}
            issueKey={issueKey}
            label={CONTROL_LABELS.answer}
            labelHidden
            look={`${pageComposer.field} min-w-60! max-[761px]:basis-full`}
            onChange={(change) => setText(change.body)}
            // Enter sends and Shift+Enter breaks the line, as on the box's other lines.
            onKeyDown={(event) => sendOnEnter(event, pickerOpen, send)}
            onPickerOpenChange={setPickerOpen}
            placeholder={
              place.phone
                ? CONTROL_LABELS.answer_placeholder_phone
                : CONTROL_LABELS.answer_placeholder
            }
            writing={dispatchWritingBox("issue-page-composer", "ask-answer")}
          />
          {/* A press here leaves the focus where it is (`keepFocusOnPress`), so on a phone the
              line keeps its form under the pointer and the press lands where it was aimed. */}
          {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handler only keeps a press on the buttons inside from moving the focus, and each of them is its own control. */}
          <div className={pageComposerAnswerControls} onMouseDown={keepFocusOnPress}>
            {options.map((option) => (
              <button
                // Pressed while its label is among the picks: the label is matched here and
                // drawn nowhere, the option's words being the reading list's
                // `issue-page-composer-target`, in plain words (`OptionWords`).
                aria-pressed={selected.includes(option.label)}
                className={`${pageComposer.option} ${pageComposerControl}`}
                data-answer-option={option.label}
                key={option.label}
                onClick={() => writer.toggle(option.label)}
                type="button"
              >
                <OptionWords option={option} source={source} />
              </button>
            ))}
            {replacing === undefined ? (
              <DeclineControl
                ask={ask}
                decline={(id, input, bounds) => api.declineAsk(id, input, bounds)}
                look={{
                  box: "contents",
                  button: `text-xs font-medium ${pageRowAct} ${pageComposerControl}`,
                }}
                onDeclined={back}
              />
            ) : null}
            <RefusableButton
              busy={pending ? ANSWER_LINES.sending : undefined}
              look="page-send-answer"
              refusal={refusal}
              type="submit"
            >
              <span {...pageChrome}>{CONTROL_LABELS.send_answer}</span>
            </RefusableButton>
          </div>
        </div>
        {failure === null ? null : (
          <OutlineRefusal
            message={failure.message}
            onRetry={failure.retryable ? send : undefined}
            retryLabel={CONTROL_LABELS.retry}
            retrying={pending}
          />
        )}
      </fieldset>
    </form>
  );
}

/**
 * The first line of what the box's target line names - the entry a reply answers, the statement
 * a comment is on, the comment an edit changes, the question an answer answers - in plain words:
 * the line holds no markup, so it is the text's plain-text projection (`PlainText`), read from
 * the tree the server sent of it and under the record it is, which names a `#id` in it as the
 * record's own page does.
 */
function TargetWords({
  markdown,
  namesAll = false,
  source,
  tree,
}: {
  markdown: string;
  /** Whether the line names every reference `tree` holds, rather than only those `markdown`
   *  cites (`ReferenceNaming.cited`): a statement row's `markdown` is the coverage read's words,
   *  which leave each of its references out (`blockWords` in Go's `api/results.go`), so it cites
   *  none of them, and the line names them as the statement's row draws them (`RowText`). Its
   *  words are still what the line draws until the engine has answered. */
  namesAll?: boolean;
  source: TextSource | undefined;
  tree: DisplayTree | undefined;
}): ReactNode {
  // Until the reader's engine has answered, the words as written: the load state the reading
  // list's `issue-page-composer-target` takes.
  return (
    <PlainText
      firstLine
      markdown={namesAll ? undefined : markdown}
      source={source}
      surface="issue-page-composer-target"
      tree={tree}
    >
      {(line) => <span>{firstLineOf(line ?? markdown).line}</span>}
    </PlainText>
  );
}

/** An option of the question an answer answers, in plain words, as its chip and the target
 *  line's "you chose" name it. */
function OptionWords({
  option,
  source,
}: {
  option: AskOptionWithTrees;
  source: TextSource | undefined;
}): ReactNode {
  // Until the reader's engine has answered, the label as written: the load state the reading
  // list's `issue-page-composer-target` takes.
  return (
    <PlainText
      markdown={option.label}
      source={source}
      surface="issue-page-composer-target"
      tree={option.label_tree}
    >
      {(line) => line ?? option.label}
    </PlainText>
  );
}

/** One press of the answer line's Send: the question it answers (`AskPress`), what it sends, and
 *  the question as the write's label names it (`questionWords`). */
interface AnswerLinePress extends AskPress {
  readonly input: AnswerAskInput;
  readonly questionWords: string;
}

/** The answer the line sends: the options picked, the reader's words, and, for a change of their
 *  own answer, the moment of the answer it replaces. */
function answerInput(
  ask: AskWithTrees,
  selected: readonly string[],
  text: string,
  replacing: { readonly at: string } | undefined
): AnswerAskInput {
  const input = answerAskInput(ask, [...selected], text);
  return replacing === undefined ? input : { ...input, expected_answer_at: replacing.at };
}
