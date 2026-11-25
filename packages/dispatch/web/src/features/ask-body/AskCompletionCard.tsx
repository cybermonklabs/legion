import { actorActText } from "@legion/contracts";
import { Fragment, type ReactNode } from "react";
import { Link } from "react-router-dom";

import type {
  Ask,
  AskAnswerWithTrees,
  AskEditWithTrees,
  AskResolution,
  AskWithTrees,
} from "../../api/types";
import {
  badgeLow,
  calloutSuccessBg,
  calloutSuccessBorder,
  calloutSuccessText,
  calloutSuccessTimestampText,
  card,
  inlineWarningText,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textPrimaryOnSuccessCallout,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { askResolutionBadge, askResolutionReason } from "../refs/actor";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import type { DwellRef } from "../shell/useDwell";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskApprovalLink } from "./AskApprovalLink";
import { AskOptionList } from "./AskOptionList";
import { AskAgent, type AskAgentNames, AskResolutionActor } from "./ask-agent";
import { ASK_LINK_WORDS, ASK_RECORD_WORDS } from "./ask-body-words";
import { type AskTextRenderer, markdownText } from "./ask-text";

/** An ask's earlier wordings, under the ask: the card draws it for an open ask, and the record
 *  for a settled one, which names the agent that reworded each as the ask body does
 *  (`agentNames`, `ask-agent.tsx`) and draws each wording through the same renderer
 *  (`renderText`). */
export function AskEditHistory({
  agentNames = "live",
  ask,
  edits,
  renderText,
}: {
  agentNames?: AskAgentNames;
  ask: Ask;
  edits: AskEditWithTrees[];
  renderText: AskTextRenderer;
}): ReactNode {
  if (ask.edited_at === null) {
    return null;
  }
  return (
    <div className={`mt-2 text-xs ${textMutedOnSurface}`}>
      <p>
        {`${ASK_RECORD_WORDS.edited} `}
        <Timestamp at={ask.edited_at} />
      </p>
      {edits.length === 0 ? null : (
        <details className="mt-1">
          <summary className={`cursor-pointer font-medium ${linkText} ${linkHoverText}`}>
            {edits.length === 1
              ? ASK_RECORD_WORDS.previous_version
              : `${ASK_RECORD_WORDS.previous_versions.before} ${edits.length} ${ASK_RECORD_WORDS.previous_versions.after}`}
          </summary>
          <div className="mt-2 space-y-3">
            {edits.map((edit) => (
              <div key={edit.at}>
                <div className={`font-medium ${textPrimaryOnSurface}`}>
                  {renderText({
                    markdown: edit.previous.question,
                    source: textSourceOf("ask", ask.id, ask.issue_key),
                    surface: "ask-edit-history",
                    tree: edit.previous.question_tree,
                  })}
                </div>
                <AskOptionList
                  options={edit.previous.options ?? []}
                  renderText={renderText}
                  selected={[]}
                  source={textSourceOf("ask", ask.id, ask.issue_key)}
                />
                <p className="mt-1">
                  {`${ASK_RECORD_WORDS.reworded_by} `}
                  <AskAgent actor={edit.edited_by} agentNames={agentNames} inline /> ·{" "}
                  <Timestamp at={edit.at} />
                </p>
              </div>
            ))}
          </div>
        </details>
      )}
    </div>
  );
}

/** Earlier answers to an ask whose answer was changed, oldest first. Every text here is the
 *  ask's own (`refs.ScopeOf`), answer and question alike, so each is drawn as a record of that
 *  ask. A chosen option's label is drawn from the tree the answer carries beside it
 *  (`selected_trees`), which is the label's own whether or not the ask still holds that option. */
export function AskAnswerHistory({
  answers,
  askId,
  issue,
  renderText,
}: {
  answers: AskAnswerWithTrees[];
  askId: string;
  /** The issue the ask is on, none for a project document's ask. */
  issue: string | null;
  renderText: AskTextRenderer;
}): ReactNode {
  const earlier = answers.slice(0, -1);
  const latest = answers.at(-1);
  if (earlier.length === 0 || latest === undefined) return null;
  return (
    <div className={`mt-2 text-xs ${calloutSuccessTimestampText}`}>
      <p>
        {`${ASK_RECORD_WORDS.changed} `}
        <Timestamp at={latest.at} />
      </p>
      <details className="mt-1">
        <summary className={`cursor-pointer font-medium underline ${textPrimaryOnSuccessCallout}`}>
          {earlier.length === 1
            ? ASK_RECORD_WORDS.earlier_answer
            : `${ASK_RECORD_WORDS.earlier_answers.before} ${earlier.length} ${ASK_RECORD_WORDS.earlier_answers.after}`}
        </summary>
        <div className="mt-2 space-y-3">
          {earlier.map((previous) => {
            const selected = previous.selected ?? [];
            return (
              <div key={previous.at}>
                {selected.length === 0 ? null : (
                  <p>
                    {selected.map((label, index) => (
                      <Fragment key={label}>
                        {index === 0 ? null : ", "}
                        {/* Only a server older than `selected_trees` answers a label with no
                            tree, and the renderer then parses it, as it parses every stored
                            text of that server's answers. */}
                        {renderText({
                          markdown: label,
                          source: textSourceOf("ask", askId, issue),
                          surface: "earlier-chosen-option",
                          tree: previous.selected_trees?.[index],
                          variant: "inline",
                        })}
                      </Fragment>
                    ))}
                  </p>
                )}
                {previous.text === null || previous.text === ""
                  ? null
                  : renderText({
                      markdown: previous.text,
                      source: textSourceOf("ask", askId, issue),
                      surface: "ask-answer-history",
                      tree: previous.text_tree,
                    })}
                <p className="mt-1">
                  {actorActText(previous.user, previous.act)} · <Timestamp at={previous.at} />
                </p>
              </div>
            );
          })}
        </div>
      </details>
    </div>
  );
}

export function OrphanedAnchorNotice({
  artifactSlug,
  ask,
}: {
  artifactSlug: string | undefined;
  ask: Ask;
}): ReactNode {
  const anchor = ask.anchor;
  if (anchor === null || !anchor.orphaned || artifactSlug === undefined) {
    return null;
  }
  return (
    <p className={`mb-2 text-xs font-medium ${inlineWarningText}`}>
      {ASK_LINK_WORDS.text_changed}{" "}
      {ask.issue_key === null ? null : (
        <Link
          className="underline"
          to={`${buildIssuePath({
            key: ask.issue_key,
            kind: "artifact",
            slug: artifactSlug,
            version: anchor.version,
          })}&ask=${ask.id}`}
        >
          {ASK_LINK_WORDS.view_original}
        </Link>
      )}
    </p>
  );
}

function AnsweredAsk({
  agentNames,
  answers,
  artifactSlug,
  ask,
  documentLink,
  edits,
  frame,
  markRef,
  onChangeAnswer,
  renderText,
}: {
  agentNames: AskAgentNames;
  answers: AskAnswerWithTrees[];
  artifactSlug: string | undefined;
  ask: AskWithTrees;
  documentLink: boolean;
  edits: AskEditWithTrees[];
  frame: AskFrame;
  markRef?: DwellRef;
  onChangeAnswer?: () => void;
  renderText: AskTextRenderer;
}): ReactNode {
  const { answer } = ask;
  // A chosen "Other" answer carries no real option (answer.selected is empty) but still has
  // free text - render it under an Other label so the record reads as a chosen option, not an
  // unlabeled addendum. An ask with no options at all has no Other row to have chosen, so its
  // free text is always the plain answer instead.
  const otherText =
    (ask.options ?? []).length > 0 && answer !== null && (answer.selected ?? []).length === 0
      ? answer.text
      : null;
  return (
    <article
      className={`rounded-xl p-4 text-sm ${frame === "block" ? "mt-3" : ""} ${calloutSuccessBorder} ${calloutSuccessBg} ${calloutSuccessText}`}
      data-testid={`ask-${ask.id}`}
      ref={markRef}
    >
      {/* The ask body's root (`AskBody`), inside the record its test id names. */}
      <div data-ask-body="">
        <AskAnchorHeader ask={ask} inBlock={frame === "block"} tone="success" />
        <OrphanedAnchorNotice artifactSlug={artifactSlug} ask={ask} />
        {frame === "block" ? null : (
          <div className={`font-medium ${textPrimaryOnSuccessCallout}`}>
            {renderText({
              markdown: ask.question,
              source: textSourceOf("ask", ask.id, ask.issue_key),
              surface: "answered-ask",
              tree: ask.question_tree,
            })}
          </div>
        )}
        <p className={`mt-1 text-xs ${calloutSuccessTimestampText}`}>
          <AskAgent actor={ask.author} agentNames={agentNames} inline />
        </p>
        {ask.kind === "approval" && documentLink ? (
          <p className={`mt-1 text-sm font-medium ${textPrimaryOnSuccessCallout}`}>
            <AskApprovalLink ask={ask} />
          </p>
        ) : null}
        <AskEditHistory agentNames={agentNames} ask={ask} edits={edits} renderText={renderText} />
        <AskOptionList
          options={ask.options ?? []}
          renderText={renderText}
          selected={answer?.selected ?? []}
          source={textSourceOf("ask", ask.id, ask.issue_key)}
        />
        {otherText !== null && otherText !== "" ? (
          <div className="mt-2">
            <p className={`text-sm font-medium ${textPrimaryOnSuccessCallout}`}>
              {ASK_RECORD_WORDS.other}
            </p>
            <div className="mt-1">
              {renderText({
                markdown: otherText,
                source: textSourceOf("ask", ask.id, ask.issue_key),
                surface: "answered-ask",
                tree: answer?.text_tree,
              })}
            </div>
          </div>
        ) : answer === null || answer.text === null || answer.text === "" ? null : (
          <div className="mt-1">
            {renderText({
              markdown: answer.text,
              source: textSourceOf("ask", ask.id, ask.issue_key),
              surface: "answered-ask",
              tree: answer.text_tree,
            })}
          </div>
        )}
        {answer === null ? (
          <p className={`mt-2 text-xs ${calloutSuccessTimestampText}`}>
            {`${ASK_RECORD_WORDS.asked} `}
            <Timestamp at={ask.created_at} />
          </p>
        ) : (
          <p className={`mt-2 text-xs ${calloutSuccessTimestampText}`}>
            {`${ASK_RECORD_WORDS.asked} `}
            <Timestamp at={ask.created_at} />
            {` · ${ASK_RECORD_WORDS.answered_by}`}{" "}
            <span className="font-semibold">{actorActText(answer.user, answer.act)}</span>{" "}
            <Timestamp at={answer.at} />
          </p>
        )}
        <AskAnswerHistory
          answers={answers}
          askId={ask.id}
          issue={ask.issue_key}
          renderText={renderText}
        />
        {onChangeAnswer === undefined ? null : (
          <button
            className={`mt-2 min-h-11 text-sm font-semibold underline ${textPrimaryOnSuccessCallout}`}
            onClick={onChangeAnswer}
            type="button"
          >
            {ASK_RECORD_WORDS.change_answer}
          </button>
        )}
      </div>
    </article>
  );
}

function ResolvedAsk({
  agentNames,
  ask,
  documentLink,
  edits,
  frame,
  renderText,
}: {
  agentNames: AskAgentNames;
  ask: AskWithTrees & { resolution: AskResolution };
  documentLink: boolean;
  edits: AskEditWithTrees[];
  frame: AskFrame;
  renderText: AskTextRenderer;
}): ReactNode {
  const { resolution } = ask;
  const reason = askResolutionReason(resolution);
  return (
    <article
      className={`rounded-xl p-4 text-sm ${frame === "block" ? "mt-3" : ""} ${card}`}
      data-testid={`ask-${ask.id}`}
    >
      {/* The ask body's root (`AskBody`), inside the record its test id names. */}
      <div data-ask-body="">
        <AskAnchorHeader ask={ask} inBlock={frame === "block"} tone="surface" />
        {frame === "block" ? null : (
          <div className={`font-medium ${textPrimaryOnSurface}`}>
            {renderText({
              markdown: ask.question,
              source: textSourceOf("ask", ask.id, ask.issue_key),
              surface: "resolved-ask",
              tree: ask.question_tree,
            })}
          </div>
        )}
        <span
          className={`mt-2 inline-block rounded-full px-2.5 py-1 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
          data-testid="ask-resolution-badge"
        >
          {askResolutionBadge(resolution)}
        </span>
        <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
          <AskAgent actor={ask.author} agentNames={agentNames} inline />
        </p>
        {ask.kind === "approval" && documentLink ? (
          <p className={`mt-1 text-sm ${linkText} ${linkHoverText}`}>
            <AskApprovalLink ask={ask} />
          </p>
        ) : null}
        <AskOptionList
          options={ask.options ?? []}
          renderText={renderText}
          selected={[]}
          source={textSourceOf("ask", ask.id, ask.issue_key)}
        />
        <AskEditHistory agentNames={agentNames} ask={ask} edits={edits} renderText={renderText} />
        <p className={`mt-2 text-xs ${textMutedOnSurface}`}>
          {`${ASK_RECORD_WORDS.asked} `}
          <Timestamp at={ask.created_at} />
          {" · "}
          <AskResolutionActor agentNames={agentNames} resolution={resolution} />
          {reason === undefined ? null : (
            <>
              {" "}
              -{" "}
              {renderText({
                markdown: reason,
                source: textSourceOf("ask", ask.id, ask.issue_key),
                surface: "resolved-ask",
                tree: ask.resolution.reason_tree,
                variant: "inline",
              })}
            </>
          )}
        </p>
      </div>
    </article>
  );
}

/** `block`: the record sits inside its decision block, whose editor content is the question. */
export type AskFrame = "card" | "block";

interface AskCompletionCardProps {
  /** How the record names agents (`ask-agent.tsx`): as every surface of today's pages does,
   *  unless the ask body it stands in says otherwise. */
  agentNames?: AskAgentNames;
  /** Every real answer, oldest first, from the ask's thread read. */
  answers?: AskAnswerWithTrees[];
  artifactSlug: string | undefined;
  ask: AskWithTrees;
  /** Whether an approval ask's completed record also links its document and version; mirrors
   *  `AskCardProps.documentLink`. */
  documentLink?: boolean;
  edits: AskEditWithTrees[];
  frame?: AskFrame;
  /** The shared thread read mark for the ask (`AskCard`, `useMarkThreadRead`), placed on an
   *  answered record when its answer is the newest write the card shows. */
  markRef?: DwellRef;
  /** Shown only to the person who gave the current, non-approval answer. */
  onChangeAnswer?: () => void;
  /** The renderer of every text of the record (`AskBodyProps.renderText`): `MarkdownBody`, as
   *  every surface of today's pages draws them, unless the ask body it stands in hands another. */
  renderText?: AskTextRenderer;
}

export function AskCompletionCard({
  agentNames = "live",
  answers = [],
  artifactSlug,
  ask,
  documentLink = true,
  edits,
  frame = "card",
  markRef,
  onChangeAnswer,
  renderText = markdownText,
}: AskCompletionCardProps): ReactNode {
  if (ask.state === "resolved") {
    if (ask.resolution === undefined) {
      throw new Error("resolved ask is missing its resolution");
    }
    return (
      <ResolvedAsk
        agentNames={agentNames}
        ask={{ ...ask, resolution: ask.resolution }}
        documentLink={documentLink}
        edits={edits}
        frame={frame}
        renderText={renderText}
      />
    );
  }
  return (
    <AnsweredAsk
      agentNames={agentNames}
      answers={answers}
      artifactSlug={artifactSlug}
      ask={{ ...ask, answer: ask.answer }}
      documentLink={documentLink}
      edits={edits}
      frame={frame}
      markRef={markRef}
      onChangeAnswer={onChangeAnswer}
      renderText={renderText}
    />
  );
}
