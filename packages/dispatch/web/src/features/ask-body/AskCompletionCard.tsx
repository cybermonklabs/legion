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
  pageHairBorder,
  pageInkText,
  pageLabelChip,
  pagePaneBg,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  textMutedOnSurface,
  textPrimaryOnSuccessCallout,
  textPrimaryOnSurface,
} from "../../theme/classes";
import type { AgentNames } from "../conversation/CachedAuthor";
import { askResolutionBadge, askResolutionReason } from "../refs/actor";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import type { DwellRef } from "../shell/useDwell";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskApprovalLink } from "./AskApprovalLink";
import { AskOptionList } from "./AskOptionList";
import { AskAgent, AskResolutionActor } from "./ask-agent";
import { ASK_LINK_WORDS, ASK_RECORD_WORDS } from "./ask-body-words";
import { type AskPalette, type AskTextRenderer, markdownText } from "./ask-text";

/**
 * The colours of the settled record, and of an ask's earlier wordings and answers, by the
 * palette the ask body's host hands (`AskPalette`), as `issue-page/DecisionActions.tsx` keeps
 * its own: `main`'s are the classes every surface of today's pages draws, and the new issue
 * page's take the role `main`'s colour has. The answered record, `main`'s success callout, is the
 * page's pane grey, the resolved one, `main`'s card, its paper, each inside the hairline rule;
 * the words in either are the ink, a link the ink as every link of the ask body is, what
 * qualifies them - who asked, when, who answered - the quiet grey, the resolution's badge the
 * page's label chip, and a quote whose words changed the page's waiting orange, as the open
 * body's warning is. Each pair is one `theme/classes.ts` registers for the page.
 */
const TONES = {
  main: {
    answered: `${calloutSuccessBorder} ${calloutSuccessBg} ${calloutSuccessText}`,
    answeredInk: textPrimaryOnSuccessCallout,
    answeredQuiet: calloutSuccessTimestampText,
    badge: `${badgeLow.bg} ${badgeLow.text}`,
    ink: textPrimaryOnSurface,
    link: `${linkText} ${linkHoverText}`,
    quiet: textMutedOnSurface,
    resolved: card,
    warning: inlineWarningText,
  },
  page: {
    answered: `${pageHairBorder} ${pagePaneBg} ${pageInkText}`,
    answeredInk: pageInkText,
    answeredQuiet: pageQuietText,
    badge: pageLabelChip,
    ink: pageInkText,
    link: pageInkText,
    quiet: pageQuietText,
    resolved: `${pageHairBorder} ${pagePaperBg}`,
    warning: pageStateText.waiting,
  },
} as const;

/** An ask's earlier wordings, under the ask: the card draws it for an open ask, and the record
 *  for a settled one, which names the agent that reworded each as the ask body does
 *  (`agentNames`, `ask-agent.tsx`), draws each wording through the same renderer
 *  (`renderText`) and in the same palette (`palette`). */
export function AskEditHistory({
  agentNames = "live",
  ask,
  edits,
  palette = "main",
  renderText,
}: {
  agentNames?: AgentNames;
  ask: Ask;
  edits: AskEditWithTrees[];
  palette?: AskPalette;
  renderText: AskTextRenderer;
}): ReactNode {
  if (ask.edited_at === null) {
    return null;
  }
  const tones = TONES[palette];
  return (
    <div className={`mt-2 text-xs ${tones.quiet}`}>
      <p>
        {`${ASK_RECORD_WORDS.edited} `}
        <Timestamp at={ask.edited_at} />
      </p>
      {edits.length === 0 ? null : (
        <details className="mt-1">
          <summary className={`cursor-pointer font-medium ${tones.link}`}>
            {edits.length === 1
              ? ASK_RECORD_WORDS.previous_version
              : `${ASK_RECORD_WORDS.previous_versions.before} ${edits.length} ${ASK_RECORD_WORDS.previous_versions.after}`}
          </summary>
          <div className="mt-2 space-y-3">
            {edits.map((edit) => (
              <div key={edit.at}>
                <div className={`font-medium ${tones.ink}`}>
                  {renderText({
                    markdown: edit.previous.question,
                    source: textSourceOf("ask", ask.id, ask.issue_key),
                    surface: "ask-edit-history",
                    tree: edit.previous.question_tree,
                  })}
                </div>
                <AskOptionList
                  options={edit.previous.options ?? []}
                  palette={palette}
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
 *  (`selected_trees`), which is the label's own whether or not the ask still holds that option.
 *  It draws in the answered record's palette (`palette`). */
export function AskAnswerHistory({
  answers,
  askId,
  issue,
  palette = "main",
  renderText,
}: {
  answers: AskAnswerWithTrees[];
  askId: string;
  /** The issue the ask is on, none for a project document's ask. */
  issue: string | null;
  palette?: AskPalette;
  renderText: AskTextRenderer;
}): ReactNode {
  const earlier = answers.slice(0, -1);
  const latest = answers.at(-1);
  if (earlier.length === 0 || latest === undefined) return null;
  const tones = TONES[palette];
  return (
    <div className={`mt-2 text-xs ${tones.answeredQuiet}`}>
      <p>
        {`${ASK_RECORD_WORDS.changed} `}
        <Timestamp at={latest.at} />
      </p>
      <details className="mt-1">
        <summary className={`cursor-pointer font-medium underline ${tones.answeredInk}`}>
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

/** The line over an anchored ask whose quoted words changed since, with the link to the version
 *  it was asked on, in the palette the ask body's host hands (`palette`). */
export function OrphanedAnchorNotice({
  artifactSlug,
  ask,
  palette = "main",
}: {
  artifactSlug: string | undefined;
  ask: Ask;
  palette?: AskPalette;
}): ReactNode {
  const anchor = ask.anchor;
  if (anchor === null || !anchor.orphaned || artifactSlug === undefined) {
    return null;
  }
  return (
    <p className={`mb-2 text-xs font-medium ${TONES[palette].warning}`}>
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
  palette,
  renderText,
}: {
  agentNames: AgentNames;
  answers: AskAnswerWithTrees[];
  artifactSlug: string | undefined;
  ask: AskWithTrees;
  documentLink: boolean;
  edits: AskEditWithTrees[];
  frame: AskFrame;
  markRef?: DwellRef;
  onChangeAnswer?: () => void;
  palette: AskPalette;
  renderText: AskTextRenderer;
}): ReactNode {
  const { answer } = ask;
  const tones = TONES[palette];
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
      className={`rounded-xl p-4 text-sm ${frame === "block" ? "mt-3" : ""} ${tones.answered}`}
      data-testid={`ask-${ask.id}`}
      ref={markRef}
    >
      {/* The ask body's root (`AskBody`), inside the record its test id names. */}
      <div data-ask-body="">
        <AskAnchorHeader ask={ask} inBlock={frame === "block"} palette={palette} tone="success" />
        <OrphanedAnchorNotice artifactSlug={artifactSlug} ask={ask} palette={palette} />
        {frame === "block" ? null : (
          <div className={`font-medium ${tones.answeredInk}`}>
            {renderText({
              markdown: ask.question,
              source: textSourceOf("ask", ask.id, ask.issue_key),
              surface: "answered-ask",
              tree: ask.question_tree,
            })}
          </div>
        )}
        <p className={`mt-1 text-xs ${tones.answeredQuiet}`}>
          <AskAgent actor={ask.author} agentNames={agentNames} inline />
        </p>
        {ask.kind === "approval" && documentLink ? (
          <p className={`mt-1 text-sm font-medium ${tones.answeredInk}`}>
            <AskApprovalLink ask={ask} />
          </p>
        ) : null}
        <AskEditHistory
          agentNames={agentNames}
          ask={ask}
          edits={edits}
          palette={palette}
          renderText={renderText}
        />
        <AskOptionList
          options={ask.options ?? []}
          palette={palette}
          renderText={renderText}
          selected={answer?.selected ?? []}
          source={textSourceOf("ask", ask.id, ask.issue_key)}
        />
        {otherText !== null && otherText !== "" ? (
          <div className="mt-2">
            <p className={`text-sm font-medium ${tones.answeredInk}`}>{ASK_RECORD_WORDS.other}</p>
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
          <p className={`mt-2 text-xs ${tones.answeredQuiet}`}>
            {`${ASK_RECORD_WORDS.asked} `}
            <Timestamp at={ask.created_at} />
          </p>
        ) : (
          <p className={`mt-2 text-xs ${tones.answeredQuiet}`}>
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
          palette={palette}
          renderText={renderText}
        />
        {onChangeAnswer === undefined ? null : (
          <button
            className={`mt-2 min-h-11 text-sm font-semibold underline ${tones.answeredInk}`}
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
  palette,
  renderText,
}: {
  agentNames: AgentNames;
  ask: AskWithTrees & { resolution: AskResolution };
  documentLink: boolean;
  edits: AskEditWithTrees[];
  frame: AskFrame;
  palette: AskPalette;
  renderText: AskTextRenderer;
}): ReactNode {
  const { resolution } = ask;
  const reason = askResolutionReason(resolution);
  const tones = TONES[palette];
  return (
    <article
      className={`rounded-xl p-4 text-sm ${frame === "block" ? "mt-3" : ""} ${tones.resolved}`}
      data-testid={`ask-${ask.id}`}
    >
      {/* The ask body's root (`AskBody`), inside the record its test id names. */}
      <div data-ask-body="">
        <AskAnchorHeader ask={ask} inBlock={frame === "block"} palette={palette} tone="surface" />
        {frame === "block" ? null : (
          <div className={`font-medium ${tones.ink}`}>
            {renderText({
              markdown: ask.question,
              source: textSourceOf("ask", ask.id, ask.issue_key),
              surface: "resolved-ask",
              tree: ask.question_tree,
            })}
          </div>
        )}
        <span
          className={`mt-2 inline-block rounded-full px-2.5 py-1 text-xs font-medium ${tones.badge}`}
          data-testid="ask-resolution-badge"
        >
          {askResolutionBadge(resolution)}
        </span>
        <p className={`mt-1 text-xs ${tones.quiet}`}>
          <AskAgent actor={ask.author} agentNames={agentNames} inline />
        </p>
        {ask.kind === "approval" && documentLink ? (
          <p className={`mt-1 text-sm ${tones.link}`}>
            <AskApprovalLink ask={ask} />
          </p>
        ) : null}
        <AskOptionList
          options={ask.options ?? []}
          palette={palette}
          renderText={renderText}
          selected={[]}
          source={textSourceOf("ask", ask.id, ask.issue_key)}
        />
        <AskEditHistory
          agentNames={agentNames}
          ask={ask}
          edits={edits}
          palette={palette}
          renderText={renderText}
        />
        <p className={`mt-2 text-xs ${tones.quiet}`}>
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
  agentNames?: AgentNames;
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
  /** The palette the record draws in (`TONES`): `main`'s, as every surface of today's pages
   *  draws it, unless the ask body it stands in hands the page's (`AskBodyProps.palette`). */
  palette?: AskPalette;
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
  palette = "main",
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
        palette={palette}
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
      palette={palette}
      renderText={renderText}
    />
  );
}
