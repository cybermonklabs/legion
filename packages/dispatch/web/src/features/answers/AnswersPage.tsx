import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { nextOffsetPage } from "../../api/queries";
import type { MyAnswerRow } from "../../api/types";
import { CappedGroup } from "../../components/CappedGroup";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill, Pill } from "../../components/Pill";
import { ShowMore } from "../../components/ShowMore";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { AskCard } from "../inbox/AskCard";
import { AskOwnerLink } from "../inbox/AskOwnerLink";
import { MarkdownBody } from "../refs/MarkdownBody";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { PostBody } from "../refs/PostBody";
import { EditedMark } from "../refs/PostMarks";
import { postDeletion } from "../refs/post-marks";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { useDocumentTitle } from "../shell/useDocumentTitle";

const PAGE_SIZE = 50;

/** The ask a row's Change answer opens, read through the same `["ask-thread", id]` key the
 *  card's own thread query uses, so one fetch serves both; mounted in `change` mode, the card
 *  opens its form seeded with the current answer. */
function ChangeAnswerCard({ askId }: { askId: string }): ReactNode {
  const thread = useQuery({
    queryFn: () => api.getAsk(askId),
    queryKey: ["ask-thread", askId],
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (thread.isPending) return <LoadingSkeleton label="Loading the ask" />;
  if (thread.isError) {
    return (
      <p className={`text-sm ${dangerText}`}>Could not load the ask: {thread.error.message}</p>
    );
  }
  return <AskCard ask={thread.data.ask} initialThread={thread.data} mode="change" />;
}

/** Why a current answer offers no Change answer, or undefined when it can be changed: the
 *  server changes an answer only on an answered question, and never an approval's
 *  (`POST /api/v1/asks/{id}/answer`); a question open again takes a new answer instead. */
function unchangeableReason(row: MyAnswerRow): string | undefined {
  if (row.ask_kind === "approval") return "An approval can't be changed";
  if (row.ask_state === "resolved") return "Closed, so it can't be changed";
  if (row.ask_state !== "answered") return "Open again: answer it from the Inbox";
  return undefined;
}

/**
 * One answer or reply of the reader's, in four named groups in this order:
 *
 * - **Meta**: when it was written, the issue or document the ask belongs to, and "Changed since"
 *   on an answer the reader has since replaced. Wraps as whole entries.
 * - **Question**: the ask's question, two lines at most, the whole of it in its title.
 * - **Answer**: the options picked, as chips that wrap their own words (`options`, a data group
 *   capped at ten with a "+N" for the rest), then the text written, or the reply.
 * - **Actions**: Open, and Change answer on a current answer that can be changed. A current
 *   answer that cannot be changed says why in its place, as Changed since says it for a replaced
 *   one, so the control never goes for a reason the row does not show.
 */
function AnswerRow({
  changing,
  onChange,
  row,
}: {
  changing: boolean;
  onChange: () => void;
  row: MyAnswerRow;
}): ReactNode {
  // Every text of the row is the ask's: its question, the labels of the options an answer chose,
  // the answer written on it, and a reply in its thread, which takes the ask's scope
  // (`refs.ScopeOf`). So a `#id` in any of them resolves against the document the server scopes
  // the ask to, read once for the row.
  const source = textSourceOf("ask", row.ask_id, row.owner.issue?.key);
  const unchangeable = row.kind === "answer" && row.current ? unchangeableReason(row) : undefined;
  const changeable = row.kind === "answer" && row.current && unchangeable === undefined;
  return (
    <li className={`rounded-xl border p-3 ${card} ${borderDefault}`} data-answer-row={row.ask_id}>
      <div className={`flex flex-wrap items-center gap-x-2 gap-y-1 text-xs ${textMutedOnCanvas}`}>
        <Timestamp at={row.at} />
        {/* The owner itself, not the ask: the row's Open already goes to the ask. */}
        <AskOwnerLink owner={row.owner} />
        {row.kind === "answer" && !row.current ? <LabelPill>Changed since</LabelPill> : null}
      </div>
      <MarkdownPreview
        className={`mt-1 text-sm font-medium ${textPrimaryOnCanvas}`}
        fullTitle
        lines={2}
        markdown={row.question}
        source={source}
        surface="answers-subject"
        tree={row.question_tree}
      />
      {row.kind === "answer" && row.answer !== undefined ? (
        <div className={`mt-2 text-sm ${textPrimaryOnCanvas}`}>
          {(row.answer.selected ?? []).length === 0 ? null : (
            // One chip per option picked. An option's label can be a whole sentence, so a chip is
            // never wider than the row: its words wrap inside it, an unbroken run breaking
            // anywhere, where a label pill keeps its words on one line.
            <CappedGroup className="flex flex-wrap gap-1.5" label="options" name="options">
              {(row.answer.selected ?? []).map((label, index) => (
                <Pill className="max-w-full" key={label} tone="selected-label">
                  <span className="min-w-0 whitespace-normal [overflow-wrap:anywhere]">
                    {/* Each label is drawn from its own tree, beside it at the same index. Only
                        a server older than `selected_trees` answers a label with no tree, and the
                        renderer then parses it, as it parses every stored text of that server's
                        answers. */}
                    <MarkdownBody
                      markdown={label}
                      source={source}
                      surface="answers-chosen-options"
                      tree={row.answer?.selected_trees?.[index]}
                      variant="inline"
                    />
                  </span>
                </Pill>
              ))}
            </CappedGroup>
          )}
          {row.answer.text === null || row.answer.text === "" ? null : (
            <div className="mt-1">
              <MarkdownBody
                markdown={row.answer.text}
                source={source}
                surface="answers-answer"
                tree={row.answer.text_tree}
              />
            </div>
          )}
        </div>
      ) : null}
      {row.kind === "reply" && row.reply !== undefined ? (
        <p className={`mt-2 text-sm ${textPrimaryOnCanvas}`}>
          {/* The reply's own edit, on its label: the row has no byline of the reply's. */}
          <span className={`font-medium ${textMutedOnCanvas}`}>
            Replied
            <EditedMark at={row.reply.edited_at} deleted={postDeletion(row.reply)} />:
          </span>{" "}
          <PostBody
            deleted={postDeletion(row.reply)}
            markdown={row.reply.body}
            source={source}
            surface="answers-reply"
            tree={row.reply.body_tree}
            variant="inline"
          />
        </p>
      ) : null}
      <div className="mt-2 flex flex-wrap items-center gap-4 text-sm">
        {/* The server's `ref` is the ask's own item route: an issue ask's lands an anchored ask on
            its document with the card selected and an unanchored one on its Conversation turn. */}
        <Link className={`font-medium ${linkText} ${linkHoverText}`} to={row.ref}>
          Open
        </Link>
        {changeable && !changing ? (
          <button
            className={`min-h-11 font-medium ${linkText} ${linkHoverText}`}
            onClick={onChange}
            type="button"
          >
            Change answer
          </button>
        ) : null}
        {unchangeable === undefined ? null : (
          <span className={`text-xs ${textMutedOnCanvas}`} data-answer-unchangeable="">
            {unchangeable}
          </span>
        )}
      </div>
    </li>
  );
}

/** `/answers`: every answer the signed-in person gave and every reply they wrote on an ask,
 *  newest first, from one `GET /api/v1/me/answers`. A current, non-approval answer can be
 *  changed in place: the card opens below its row and stays open across the refetch the change
 *  causes, which puts the new answer at the top and marks the old row "Changed since". */
export function AnswersPage(): ReactNode {
  useDocumentTitle("Answered by you · Dispatch");
  const [changingAsk, setChangingAsk] = useState<string | null>(null);
  const answers = useInfiniteQuery({
    getNextPageParam: nextOffsetPage,
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.listMyAnswers({ limit: PAGE_SIZE, offset: pageParam }),
    queryKey: ["me", "answers"],
  });

  if (answers.isPending) return <LoadingSkeleton label="Loading your answers" />;
  if (answers.isError) {
    return <p className={dangerText}>Could not load your answers: {answers.error.message}</p>;
  }
  const rows = answers.data.pages.flatMap((page) => page.rows ?? []);
  // The card belongs to the ask, under the first answer row that names it: after a change that is
  // the new answer's row, so the card the person is looking at stays mounted where they are. A
  // reply the person wrote on the same ask can sort above it, and is never the card's row.
  const cardRow =
    changingAsk === null
      ? -1
      : rows.findIndex((row) => row.kind === "answer" && row.ask_id === changingAsk);

  return (
    <section aria-label="Answered by you">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/">
          ← Inbox
        </Link>
        <h1 className={`mt-1 text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Answered by you
        </h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          Every answer you gave and every reply you wrote on an ask, newest first.
        </p>
      </header>
      {rows.length === 0 ? (
        <EmptyState
          label="Answers empty state"
          message="You have not answered or replied on an ask yet."
        />
      ) : (
        // One flat list of keyed items, the card among them: a card nested under its row would
        // remount when the change puts a new row above it, and reopen its form.
        <ul className="space-y-3">
          {rows.flatMap((row, index) => {
            const item = (
              <AnswerRow
                changing={index === cardRow}
                key={
                  row.kind === "reply" ? `reply:${row.reply?.id}` : `answer:${row.ask_id}:${row.at}`
                }
                onChange={() => setChangingAsk(row.ask_id)}
                row={row}
              />
            );
            return index === cardRow
              ? [
                  item,
                  <li data-answer-card={row.ask_id} key={`card:${row.ask_id}`}>
                    <ChangeAnswerCard askId={row.ask_id} />
                  </li>,
                ]
              : [item];
          })}
        </ul>
      )}
      <ShowMore
        fetching={answers.isFetchingNextPage}
        hasMore={answers.hasNextPage}
        onFetch={() => void answers.fetchNextPage()}
      />
    </section>
  );
}
