import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { ApiError, api } from "../../api/client";
import type { AnswerAskInput, Ask } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { pageComposer, pageRowAct } from "../../theme/classes";
import { useHeldSend } from "../conversation/held-sends";
import { MentionComposer, type OutlineKind } from "../conversation/MentionComposer";
import { answerAskInput } from "../inbox/answer-ask";
import { useAnswerDraft } from "../inbox/answer-drafts";
import { DeclineControl } from "../inbox/DeclineControl";
import { useOutstandingAnswerWrite } from "../inbox/outstanding-answers";
import { invalidateOwnerReads } from "../inbox/owner-reads";
import { answerFailure } from "../inbox/useAskAnswerForm";
import { AgentName } from "../refs/AgentName";
import { FOOT_HEIGHT_PROPERTY } from "./identity-block";
import { firstLineOf } from "./issue-tree";
import { type AnswerTarget, type OutlineWriter, useOutlineWriter } from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/** The kinds the line writes, as its chips: a comment and a question (R16). */
const KINDS: readonly OutlineKind[] = [
  {
    kind: "comment",
    label: (
      <>
        <span aria-hidden="true">❝</span>
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

/**
 * The one place to write on the issue page, fixed to the foot of the screen
 * (dispatch://INFRA-1196 R16, the architect's ruling 5, the prototype's `#composer`, the pictures
 * page-v3-conversation-top-1440 and -390): one line, which Enter sends, while the outline runs on
 * under it. Empty and at rest it is one line with Send on a phone, where its kinds are drawn once
 * it holds the caret.
 *
 * It writes what today's model can write. With nothing set, a comment on the issue, which the
 * outline draws under the issue, or a question with the Question chip pressed, which is an ask
 * of the issue; the pictures' Requirement and Decision chips wait on dispatch://INFRA-1253 and
 * dispatch://INFRA-1150, and none is drawn that would write nothing. A row's Reply sets it to
 * reply to that row, its target named above the line and its placeholder saying where the words
 * go: a comment written as a reply, which the outline draws under the row it answers, and
 * mentions work as on every composer (`MentionComposer`). An open question's Answer, or one of
 * its options, sets it to answer that question: the line in the waiting colour, the question's
 * options as its chips, Decline to answer beside Send answer.
 *
 * Once a write lands the outline takes the reader to it (`outline-writer.tsx`'s `Landing`): the
 * row of what they wrote, its branch unfolded and focused, so their own new words are never
 * behind a fold and the focus never falls to the page.
 *
 * With no filter on it is drawn. Under a filter, whose pictures draw no line, and while a
 * document is open in place of the outline, it is drawn only while the reader has asked to write
 * - a Reply, an Answer or an Edit pressed there or in Details - or holds a send of their own, so
 * a press is never answered by nothing and a refusal is never out of sight.
 *
 * The line that writes on the issue stays mounted, hidden, while the line answers a question or
 * edits a comment, and wherever the line itself is not drawn, so what the reader was typing is
 * there when they come back to it.
 */
export function IssueComposer({
  documentOpen,
  filterOn,
  isClosed,
  issueKey,
}: {
  documentOpen: boolean;
  filterOn: boolean;
  isClosed: boolean;
  issueKey: string;
}): ReactNode {
  const writer = useOutlineWriter();
  const held = useHeldSend(writer?.sendKey);
  const section = useRef<HTMLElement | null>(null);
  const asked =
    writer !== undefined &&
    (writer.reply !== null || writer.answer !== undefined || writer.editing !== undefined);
  const shown =
    writer !== undefined && (asked || held !== undefined || (!documentOpen && !filterOn));
  // How tall the line is while it is drawn, on the document element, so what must not sit
  // under it reads one number: the tree's focused row is brought clear of it, as it is of the
  // band at the top (`identity-block.ts`'s `bringOnScreen`). Unset while the line is not drawn.
  useLayoutEffect(() => {
    const element = section.current;
    if (!shown || element === null) return;
    const root = document.documentElement;
    const publish = () =>
      root.style.setProperty(FOOT_HEIGHT_PROPERTY, `${element.getBoundingClientRect().height}px`);
    publish();
    const sizes = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(publish);
    sizes?.observe(element);
    return () => {
      sizes?.disconnect();
      root.style.removeProperty(FOOT_HEIGHT_PROPERTY);
    };
  }, [shown]);
  if (writer === undefined) return null;
  const writing = writer.answer === undefined && writer.editing === undefined;
  return (
    <section
      aria-label={CONTROL_LABELS.write}
      className={pageComposer.foot}
      data-testid="issue-composer"
      hidden={!shown}
      ref={section}
      {...authorText}
    >
      {writer.editing === undefined ? null : (
        <EditLine
          // One line per comment: its words in the box are that comment's.
          key={writer.editing.id}
          editing={writer.editing}
          issueKey={issueKey}
          writer={writer}
        />
      )}
      {writer.answer === undefined ? null : (
        <AnswerLine
          // One line per question: what the reader typed and picked is that question's.
          key={writer.answer.askId}
          issueKey={issueKey}
          target={writer.answer}
          writer={writer}
        />
      )}
      {/* Last, so the line that is drawn is the first field of the section, and hidden rather
          than unmounted while the line answers or edits: its draft is the reader's. */}
      <div data-composer-write="" hidden={!writing}>
        <WriteLine isClosed={isClosed} issueKey={issueKey} writer={writer} />
      </div>
    </section>
  );
}

/** The line that writes a comment, a question, or a reply (`MentionComposer`'s outline look). */
function WriteLine({
  isClosed,
  issueKey,
  writer,
}: {
  isClosed: boolean;
  issueKey: string;
  writer: OutlineWriter;
}): ReactNode {
  const reply = writer.reply;
  // A reply is a comment under the row it answers: a question asked from the line has no row to
  // sit under in today's model, so the chips are drawn only while the line writes on the issue.
  // A closed issue takes a comment and no question, so the line writes a comment there whatever
  // chip was pressed while it was open, rather than drawing nothing.
  const kind = reply === null && !isClosed ? writer.kind : "comment";
  return (
    <MentionComposer
      closed={isClosed}
      kind={kind}
      mutationKey={writer.sendKey}
      onCancelReply={() => writer.setReply(null)}
      onClose={() => (document.activeElement as HTMLElement | null)?.blur()}
      onKindChange={(next) => {
        if (next === "ask" || next === "comment") writer.setKind(next);
        return undefined;
      }}
      onSent={() => {
        // The reader is taken to what they wrote: their newest node under the row the line
        // replied to, or under the issue.
        writer.land({
          to: "newest",
          under: reply === null ? undefined : { id: reply.id, kind: reply.parentKind },
        });
        writer.setReply(null);
      }}
      outline={{
        cancelReply: CONTROL_LABELS.write_on_issue,
        kinds: reply === null && !isClosed ? KINDS : [],
        placeholder:
          reply === null ? CONTROL_LABELS.write_placeholder : CONTROL_LABELS.reply_placeholder,
        send: CONTROL_LABELS.send,
        target:
          reply === null ? undefined : (
            <>
              <span {...pageChrome}>{CONTROL_LABELS.replying_to}</span>{" "}
              <b className="font-semibold text-page-ink">
                <AgentName actor={reply.actor} inline />
              </b>{" "}
              <span>{firstLineOf(reply.excerpt).line}</span>
            </>
          ),
      }}
      owner={{ issueKey, kind: "issue" }}
      replyTo={reply}
    />
  );
}

/** The reader's own comment, edited in the line: its words in the box, Save writes them back,
 *  and the outline draws the comment again from the read that answers after and takes the
 *  reader back to its row. */
function EditLine({
  editing,
  issueKey,
  writer,
}: {
  editing: { readonly body: string; readonly id: string };
  issueKey: string;
  writer: OutlineWriter;
}): ReactNode {
  const queryClient = useQueryClient();
  const back = () => {
    writer.land({ to: "self", under: { id: editing.id, kind: "comment" } });
    writer.clear();
  };
  return (
    <MentionComposer
      edit={editing}
      mutationKey={[...writer.sendKey, "edit", editing.id]}
      onClose={back}
      onSent={back}
      outline={{
        cancelReply: CONTROL_LABELS.write_on_issue,
        kinds: [],
        placeholder: CONTROL_LABELS.edit_placeholder,
        send: CONTROL_LABELS.save,
        target: (
          <>
            <span {...pageChrome}>{CONTROL_LABELS.editing}</span>{" "}
            <span>{firstLineOf(editing.body).line}</span>
          </>
        ),
      }}
      owner={{ issueKey, kind: "issue" }}
      saveEdit={async (id, body, carried, bounds) => {
        const saved = await api.editComment(id, { body, ...carried }, bounds);
        await queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
        return saved;
      }}
    />
  );
}

/**
 * The line that answers an open question: the question named above it in the waiting colour,
 * what the reader picked beside it, the question's options as chips the keyboard reaches - the
 * row's own options are the pointer's - Decline to answer, and Send answer. A pick or words
 * alone answer it: words alone are the reader's own answer, as the old card's Other was. On a
 * phone the box to type in has the line's whole width and the options and controls stand under
 * it. Escape, from any control of the line, sets the line back and takes the reader back to the
 * question's row.
 *
 * The write is the Inbox's (`useOutstandingAnswerWrite`), so the question's row leaves the Inbox
 * as the answer goes and comes back if Dispatch refuses it, and what the reader typed is the
 * question's draft (`answer-drafts.ts`), which every surface of that question shares. A refusal
 * that says the question moved under the line - edited, answered or closed elsewhere - reads the
 * question again, so the line and the outline draw it as it stands. A question answered or
 * closed elsewhere while the line held it is not answered here: the line goes back to writing on
 * the issue.
 */
function AnswerLine({
  issueKey,
  target,
  writer,
}: {
  issueKey: string;
  target: AnswerTarget;
  writer: OutlineWriter;
}): ReactNode {
  const queryClient = useQueryClient();
  const asks = useQuery({
    queryFn: () => api.listIssueAsks(issueKey, "all"),
    queryKey: ["asks", issueKey],
  });
  const ask = (asks.data ?? []).find((one) => one.id === target.askId);
  const [draft, setDraft] = useAnswerDraft(target.askId);
  // A change of the reader's own answer starts from its words and is this line's own text, never
  // the question's shared draft: a recorded answer is not unsent text.
  const [changed, setChanged] = useState(target.replacing?.text ?? "");
  const text = target.replacing === undefined ? draft : changed;
  const setText = target.replacing === undefined ? setDraft : setChanged;
  const back = () => {
    writer.land({ to: "self", under: { id: target.askId, kind: "ask" } });
    writer.clear();
  };
  const mutation = useOutstandingAnswerWrite({
    askId: target.askId,
    label: () => `Answer to “${ask?.question ?? ""}”`,
    removesRow: () => target.replacing === undefined,
    write: (input: AnswerAskInput, bounds) => api.answerAsk(target.askId, input, bounds),
    onError: (error) => {
      if (!(error instanceof ApiError) || STALE_ASK[error.code ?? ""] !== true) return;
      void queryClient.invalidateQueries({ queryKey: ["asks", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["issue", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", target.askId] });
    },
    onSuccess: () => {
      setText("");
      back();
      if (ask !== undefined) invalidateOwnerReads(queryClient, ask);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", target.askId] });
    },
  });
  // The read has answered and the question is no longer one this line may answer: it was
  // answered or closed elsewhere, or it is gone. The line goes back to writing on the issue
  // rather than drawing an empty box, unless a refusal of the reader's own answer is on it,
  // which they read first.
  const gone =
    asks.isSuccess &&
    (ask === undefined || (ask.state !== "open" && target.replacing === undefined));
  const refused = mutation.error !== null;
  const clear = writer.clear;
  useEffect(() => {
    if (gone && !refused) clear();
  }, [clear, gone, refused]);
  if (ask === undefined || (gone && !refused)) return null;
  // What was picked of the options the question still offers: an edit of the question can take
  // one away under the line.
  const offered = new Set((ask.options ?? []).map((option) => option.label));
  const selected = target.selected.filter((label) => offered.has(label));
  const refusal =
    selected.length === 0 && text.trim() === "" ? CONTROL_LABELS.answer_placeholder : null;
  const send = () => {
    if (refusal !== null || mutation.isPending) return;
    mutation.mutate(answerInput(ask, { ...target, selected }, text));
  };
  const keys = (event: KeyboardEvent<HTMLElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      back();
      return;
    }
    if (!(event.target instanceof HTMLTextAreaElement)) return;
    if (event.key !== "Enter" || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    send();
  };
  const failure = mutation.error === null ? null : answerFailure(mutation.error);
  return (
    // Escape from any control of the line sets it back, and Enter in its field sends.
    <form
      aria-label={CONTROL_LABELS.answering}
      className={`${pageComposer.box} ${pageComposer.answering}`}
      data-issue-composer=""
      data-answering={target.askId}
      onKeyDown={keys}
      onSubmit={(event) => {
        event.preventDefault();
        send();
      }}
    >
      <fieldset
        className="flex min-w-0 flex-col gap-[3px] border-0 p-0"
        disabled={mutation.isPending}
      >
        <div
          className={`${pageComposer.target} ${pageComposer.targetAnswering}`}
          data-composer-target=""
        >
          <span className="min-w-0 truncate">
            <span {...pageChrome}>{CONTROL_LABELS.answering}</span>{" "}
            <b className="font-semibold text-page-ink">{firstLineOf(ask.question).line}</b>
            {selected.length === 0 ? null : (
              <>
                <span {...pageChrome}> · {CONTROL_LABELS.answer_chose}</span>{" "}
                <span>{selected.map((label) => `“${label}”`).join(", ")}</span>
              </>
            )}
          </span>
          <button
            aria-label={CONTROL_LABELS.write_on_issue}
            className={pageComposer.targetClear}
            onClick={writer.clear}
            type="button"
          >
            <span aria-hidden="true">×</span>
          </button>
        </div>
        {/* On a phone the options and the controls take the line under the box: beside them it
            was 39 px wide. */}
        <div className={`${pageComposer.line} max-[760px]:flex-wrap`}>
          <textarea
            aria-label={CONTROL_LABELS.answer}
            className={`${pageComposer.field} max-[760px]:basis-full`}
            onChange={(event) => setText(event.target.value)}
            placeholder={CONTROL_LABELS.answer_placeholder}
            rows={1}
            value={text}
          />
          <div className={pageComposer.controls}>
            {(ask.options ?? []).map((option) => (
              <button
                aria-pressed={selected.includes(option.label)}
                className={pageComposer.option}
                data-answer-option={option.label}
                key={option.label}
                onClick={() => writer.toggle(option.label)}
                type="button"
              >
                {option.label}
              </button>
            ))}
            {target.replacing === undefined ? (
              <DeclineControl
                ask={ask}
                decline={(id, input, bounds) => api.declineAsk(id, input, bounds)}
                look={{ box: "contents", button: `text-xs font-medium ${pageRowAct}` }}
                onDeclined={back}
              />
            ) : null}
            <RefusableButton
              busy={mutation.isPending ? "Sending…" : undefined}
              refusal={refusal}
              type="submit"
            >
              <span {...pageChrome}>{CONTROL_LABELS.send_answer}</span>
            </RefusableButton>
          </div>
        </div>
        {failure === null ? null : (
          <QueryError
            message={failure.message}
            onRetry={failure.retryable ? send : undefined}
            retrying={mutation.isPending}
          />
        )}
      </fieldset>
    </form>
  );
}

/** The answer the line sends: the options picked, the reader's words, and, for a change of their
 *  own answer, the moment of the answer it replaces. */
function answerInput(ask: Ask, target: AnswerTarget, text: string): AnswerAskInput {
  const input = answerAskInput(ask, [...target.selected], text);
  return target.replacing === undefined
    ? input
    : { ...input, expected_answer_at: target.replacing.at };
}
