import { dispatchWritingBox } from "@legion/contracts";
import { type ReactNode, useCallback, useEffect, useLayoutEffect, useRef } from "react";

import type { ArtifactOwner, DeclineAskInput, WriteBounds } from "../../api/client";
import type { Ask } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { Dot, KindBullet } from "../../components/RowMarks";
import { TruncatedText } from "../../components/TruncatedText";
import { useTooltips } from "../../components/tooltips";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import {
  borderDefault,
  cardHoverBorder,
  focusBorder,
  inlineWarningText,
  linkHoverText,
  linkText,
  pageHairBorder,
  pageInkFill,
  pageInkText,
  pageKindBullet,
  pageKindTag,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { DraftUploadStatus } from "../artifacts/ArtifactUpload";
import type { AgentNames } from "../conversation/CachedAuthor";
import { MentionField, type MentionFieldHandle } from "../conversation/MentionField";
import { UnsentBox } from "../inbox/AskThread";
import type { OwnedAsk } from "../inbox/ask-name";
import { askTurnHolder } from "../inbox/ask-turn";
import { askUrgency } from "../inbox/ask-urgency";
import type { AskAnswerForm } from "../inbox/useAskAnswerForm";
import { AgentName, useAgentText } from "../refs/AgentName";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import type { DwellRef } from "../shell/useDwell";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { stoppedNote } from "../writes/useKeyedWrite";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskApprovalLink } from "./AskApprovalLink";
import { AskBlockLink } from "./AskBlockLink";
import { AskCompletionCard, type AskFrame, OrphanedAnchorNotice } from "./AskCompletionCard";
import { AskHandOn } from "./AskHandOn";
import { AskChoiceRow } from "./AskOptionRow";
import { AskAgent } from "./ask-agent";
import {
  ASK_APPROVAL_WORDS,
  ASK_CARD_WORDS,
  ASK_FORM_WORDS,
  ASK_LINE_WORDS,
} from "./ask-body-words";
import { type AskPalette, type AskTextRenderer, authored, type TextMarks } from "./ask-text";
import { DeclineControl } from "./DeclineControl";
import { EarlierApproval } from "./EarlierApproval";

/**
 * Each colour the body's own elements draw inside `NoTooltips`, the new issue page's, by the
 * role `main`'s colour has: the question is the ink, what a reader acts on the muted grey, what
 * qualifies it the quiet grey, a link the ink, and a question that changed under the reader the
 * page's warning, its waiting orange; the answer box is the page's field, which keeps `main`'s
 * focus rule, the application's one focus indicator; Answer and Ask back instead are filled with
 * the ink, as the page's one filled control is, and the other actions are its header control's
 * rule and muted words. What the body's children draw is theirs: the options, the earlier
 * approval, the anchor, Decline to answer and the record in the palette the host hands them
 * (`AskBodyProps.palette`), the unsent answer's box (`UnsentBox`) in its own, and the card's
 * frame is the card's own (`inbox/AskCard.tsx`).
 *
 * They stand in this table, not beside `main`'s classes, because `theme/text-on-background.test.ts`
 * pairs each text class with the nearest enclosing ground of the same file across both branches
 * of a ternary: a page colour written beside `main`'s would pair `main`'s words with the page's
 * paper and the page's words with `main`'s card. Each pair here is one `theme/classes.ts`
 * registers for the page.
 */
const ON_PAGE = {
  action: `${pageLineBorder} ${pageMutedText}`,
  field: `${pageHairBorder} ${pagePaperBg} ${pageInkText} placeholder:text-page-quiet ${focusBorder}`,
  filled: `${pageInkFill} refused:cursor-not-allowed refused:bg-page-hair refused:text-page-muted`,
  ink: pageInkText,
  link: pageInkText,
  muted: pageMutedText,
  quiet: pageQuietText,
  warning: pageStateText.waiting,
} as const;

/** Moves focus from inside the form to the nearest focusable ancestor (in the Inbox, the row, so
 *  j/k/Escape still start from the same place) instead of letting it fall to the document body. */
function handOffFocus(form: HTMLElement): void {
  if (!form.contains(document.activeElement)) return;
  form.parentElement?.closest<HTMLElement>("[tabindex]")?.focus({ preventScroll: true });
}

/** The form's callback ref. When the form leaves while a control inside it has focus - the ask
 *  was answered or resolved elsewhere and the body swaps to its record - focus is handed off
 *  without any event; React detaches a ref before removing its node, so the form is still in the
 *  document when this cleanup runs. The form is also kept in `formRef` for the disabled-control
 *  hand-off below. The callback must be stable across renders: React re-runs a changed callback
 *  ref's cleanup on every commit, and that cleanup moves focus. */
function useFormRef(): [
  { current: HTMLFormElement | null },
  (form: HTMLFormElement | null) => (() => void) | undefined,
] {
  const formRef = useRef<HTMLFormElement | null>(null);
  const track = useCallback((form: HTMLFormElement | null) => {
    formRef.current = form;
    if (form === null) return undefined;
    return () => {
      formRef.current = null;
      handOffFocus(form);
    };
  }, []);
  return [formRef, track];
}

/** The project of the document an ask was asked on: the Inbox row's `document`, else the
 *  caller's `owner`, else the document the ask is anchored to. */
function askDocumentProject(
  ask: OwnedAsk,
  displayedAsk: Ask,
  owner: { project: string; slug: string } | undefined
): string | undefined {
  return (ask.document ?? owner ?? displayedAsk.anchor_artifact)?.project;
}

/** Where a file pasted or dropped into the answer, or into a reply under the ask, goes: the
 *  ask's issue, or the project of the document it was asked on (`askDocumentProject`).
 *  `document` is the Inbox row's: the ask read carries no owner, so the fresher `displayedAsk`
 *  cannot name it. */
export function askUploadOwner(
  ask: OwnedAsk,
  displayedAsk: Ask,
  owner: { project: string; slug: string } | undefined
): ArtifactOwner | undefined {
  const documentProject = askDocumentProject(ask, displayedAsk, owner);
  return displayedAsk.issue_key !== null
    ? { issue: displayedAsk.issue_key }
    : documentProject === undefined
      ? undefined
      : { project: documentProject };
}

export interface AskBodyProps {
  /** How the body names agents (`ask-agent.tsx`): `live` on today's surfaces and in the Inbox. */
  agentNames: AgentNames;
  /** The document whose earlier version an orphaned anchor's "View original text" opens. */
  artifactSlug?: string;
  /** The ask as the caller holds it. An Inbox row's carries its document and its issue's
   *  assignee, which no read of the ask does; what the body draws is the form's fresher
   *  `displayedAsk`. */
  ask: OwnedAsk;
  /** A session asker's name as a control of the caller's: the card's toggle of its copy
   *  handles. Without it the name is the agent alone. */
  askerControl?: (name: ReactNode) => ReactNode;
  decline: (id: string, input: DeclineAskInput, bounds: WriteBounds) => Promise<Ask>;
  /** Whether an approval ask's label also links its document and version
   *  (`AskCardProps.documentLink`). */
  documentLink: boolean;
  /** The ask's form, `useAskAnswerForm`'s answer: every state the body draws is its own. */
  form: AskAnswerForm;
  /** `block`: the body sits inside its decision block, which draws the question and the kind
   *  (`AskCardProps.frame`). */
  frame: AskFrame;
  /** The caller's thread read mark, placed on an answered record when its answer is the newest
   *  write the caller shows (`AskCompletionCard`). */
  markRecord?: DwellRef;
  /** The two marks of a host that divides its own words from an author's (`TextMarks`): with
   *  them the open body's root wears the mark of the body's own words, which are the issue
   *  page's declared ones (`ask-body-words.ts`), and each value inside it - who asked, who it
   *  waits on, the document, a version, a date - the mark of a value, as the caller's renderer
   *  marks the ask's own texts. The shared answer box, with its hint, and a refusal under it are
   *  the box's and the server's words, so the form that holds them wears the mark of a value,
   *  and the options and the actions in it the body's again. Dashboard hands them; no other
   *  host does, so every other surface draws the elements it always drew, with no mark. */
  marks?: TextMarks;
  /** Called once the server has recorded the reader's decline (`DeclineControl`). */
  onAnswered?: (id: string) => void;
  /** The project document that owns an ask whose read lacks `document`
   *  (`AskCardProps.owner`), where a file pasted into the answer goes. */
  owner?: { project: string; slug: string };
  /** The palette the body's children and its settled record draw in (`AskPalette`): the
   *  options, the earlier approval, the anchor's link and quote, Decline to answer and its
   *  dialog, and the record. The body's own elements take the page's colours inside
   *  `NoTooltips` whatever the host hands (`ON_PAGE`), so today's Dashboard, which is drawn
   *  there, keeps `main`'s children while the new page's Dashboard hands `page` and draws every
   *  element of the body in the page's colours (dispatch://INFRA-3940 R13). No other host hands
   *  it, so every other surface draws the elements it always drew. */
  palette?: AskPalette;
  /** The caller's renderer of every text of the ask the body draws (R17, `AskText`): the
   *  question, the options' labels and descriptions, and in the record the answer, the earlier
   *  answers, the reason a question was closed and the question's earlier wordings. */
  renderText: AskTextRenderer;
  /** Whether the body draws the ask's urgency word ("Blocking", "High") beside its kind at the
   *  right end of the first line, where an urgency so high carries one (R17). Off, it draws
   *  none: the card's notch says it. */
  urgencyWord: boolean;
}

/**
 * The ask body (dispatch://INFRA-5702/spec#b-r-ask-body): an ask's question, options and answer
 * parts, which every surface draws an ask with, and the settled record in their place once the
 * ask is settled. Its root carries `data-ask-body`; a record carries it inside the element its
 * test id names (`AskCompletionCard`).
 *
 * An open ask is root R34's row, as the new issue page draws a question's (`NodeRow.tsx`): a
 * bullet in the question's colour, then the question's words, with the kind, who asked and when
 * at the right end of their first line, or under the words in a body narrower than 32rem
 * (`firstLineEnd`), and under the words everything else the body draws. In a document's ask
 * block the block's frame draws the question and its kind, so the body draws neither, and no
 * bullet; its first line keeps who asked and when. The answer box always stands in place under
 * the options.
 *
 * It sends no request of its own: it draws from the ask its caller hands it and the form
 * (`useAskAnswerForm`), and a control reads only when the reader presses it or reaches for it
 * (Show diff, Hand on). The caller draws what its surface alone draws - the card's frame, notch
 * and recipients, its copy handles, the edit history, the references and the thread - above the
 * body or below it, never between its parts.
 */
export function AskBody({
  agentNames,
  artifactSlug,
  ask,
  askerControl,
  decline,
  documentLink,
  form,
  frame,
  markRecord,
  marks,
  onAnswered,
  owner,
  palette = "main",
  renderText,
  urgencyWord,
}: AskBodyProps): ReactNode {
  // Every state the body draws is the form's, which lives as long as the form does: the card
  // draws one body for the form and another for its record, and each draws what the one before it
  // left. The body holds nothing of its own but its handles on the elements it draws.
  const {
    answerFailure,
    answerFieldId,
    answerPlaceholder,
    answers,
    answerText,
    askChanged,
    cannotAnswer,
    canChangeAnswer,
    cancelChanging,
    changing,
    choiceShown,
    clarification,
    completed,
    disabledWhileOut,
    displayedAsk,
    edits,
    isApproval,
    mutation,
    nothingToAskBack,
    otherSelected,
    reasonFieldShownBefore,
    reasonRequired,
    refusedWhileOut,
    refusing,
    selectRealOption,
    selected,
    sendAnswer,
    sendClarification,
    setAnswerText,
    setQuestionChoice,
    startChanging,
    submit,
    submitHint,
    toggleOther,
    submitGuard,
    threadQuery,
    unsentAnswer,
    upload,
  } = form;
  const inBlock = frame === "block";
  // Where a `#id` in the ask's own text resolves: the document the server scopes this ask to -
  // the one its block is on, the one it is anchored to, else its issue's spec (`refs.ScopeOf`).
  const source = textSourceOf("ask", displayedAsk.id, displayedAsk.issue_key);
  const uploadOwner = askUploadOwner(ask, displayedAsk, owner);
  const uploading = upload.pending > 0;
  const answerFieldRef = useRef<MentionFieldHandle | null>(null);
  // Picking Request changes moves the person straight to the field the server insists on. The
  // form keeps whether the field stood for a reason at the last commit
  // (`reasonFieldShownBefore`), so the field takes the focus as it comes to, and a body drawn
  // again for the form - after its record - moves none.
  useEffect(() => {
    if (reasonRequired === reasonFieldShownBefore.current) return;
    reasonFieldShownBefore.current = reasonRequired;
    if (reasonRequired) {
      answerFieldRef.current?.textarea()?.focus();
    }
  }, [reasonRequired, reasonFieldShownBefore]);
  // A control that disables itself under the reader's focus - Ask back while sending and once
  // its text is sent - would have the browser drop focus to the document body (in the Inbox, out
  // of the row, which then lets the row go) with nothing the body could act on afterwards; the
  // same hand-off as a vanishing form, in the commit that disables it, before the browser looks.
  // While the form refuses a press inside `NoTooltips` (`refusing`, `useAskAnswerForm`), its
  // controls are `aria-disabled`, not `disabled`, so this moves no focus then.
  const [formRef, trackForm] = useFormRef();
  useLayoutEffect(() => {
    const form = formRef.current;
    const active = document.activeElement;
    if (form !== null && active instanceof HTMLElement && active.matches(":disabled")) {
      handOffFocus(form);
    }
  });
  const tooltips = useTooltips();
  const holdsFocus = !tooltips;
  // Ask back instead takes the focus as it is drawn. Outside `NoTooltips` its callback is a new
  // one at each render, which focuses it again at each render, as on `main`. Inside it is this
  // one callback, stable across renders as `useFormRef`'s is, which focuses it once: a focus
  // taken at each render would move the reader's focus while a write is out - from Answer with
  // it anyway, say, whose answer is out. The choice leaves the page, and the button with it, as
  // the clarification lands outside `NoTooltips`, and inside once the ask is read again after it;
  // a focus the button still holds then goes where the form's own hand-off puts it, not to the
  // body.
  const focusAskBackInstead = useCallback(
    (button: HTMLButtonElement | null) => {
      if (button === null) return undefined;
      button.focus();
      return () => {
        const form = formRef.current;
        if (form !== null && document.activeElement === button) handOffFocus(form);
      };
    },
    [formRef]
  );
  const submitHintId = `${answerFieldId}-hint`;
  const agentText = useAgentText();
  // The asker as its name's tooltip: a block ask the server indexed with no pending author has
  // an empty label and reads as "asked". A session's tooltip also carries its full id, since
  // the innermost title is the one a browser shows, and an untitled session's name is the short
  // form `session:<8>…` that sessions started in the same minute share.
  const sessionAuthor = displayedAsk.author.kind === "session" ? displayedAsk.author : undefined;
  const askerTitle =
    displayedAsk.author.id === ""
      ? ASK_LINE_WORDS.asked
      : sessionAuthor === undefined
        ? agentText.label(displayedAsk.author)
        : `${agentText.label(sessionAuthor)} · ${sessionAuthor.id}`;
  // The asker's name, cut with its tooltip where one is drawn, and never cut to nothing: beside
  // the words the first line's end leaves it little room. In a narrow body, where that end stands
  // under the words and the name shares its line with the time (`firstLineEnd`), it wraps onto a
  // second line before it is cut, so a name two lines hold stays whole. Inside `NoTooltips`,
  // where no tooltip gives back what an ellipsis cut, it is a value that breaks anywhere instead.
  const askerName = (name: ReactNode) =>
    tooltips ? (
      <TruncatedText
        className="min-w-8 @max-[32rem]/ask-body:line-clamp-2 @max-[32rem]/ask-body:whitespace-normal @max-[32rem]/ask-body:[overflow-wrap:anywhere]"
        title={askerTitle}
      >
        {name}
      </TruncatedText>
    ) : (
      <span className="min-w-0 [overflow-wrap:anywhere]">{name}</span>
    );
  const urgency = askUrgency(displayedAsk.urgency);
  // The urgency word a caller asked for stands beside the kind, where the card's notch would say
  // it: for an ask so urgent it carries one.
  const urgencyText =
    urgencyWord && (urgency === "blocking" || urgency === "high")
      ? ASK_LINE_WORDS.urgency[urgency]
      : undefined;
  const turnHolder = askTurnHolder(displayedAsk, (threadQuery.data?.replies ?? []).at(-1));

  // Inside `NoTooltips` the actions are the page's (`ON_PAGE`): filled with the ink, or its
  // header control's rule and muted words.
  const filledLook = tooltips
    ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`
    : ON_PAGE.filled;
  const secondaryLook = tooltips
    ? `${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`
    : ON_PAGE.action;
  const answerField = (
    <>
      {/* Inside `NoTooltips` the box is the page's (`ON_PAGE.field`), with its label and the line
          under it in the page's words, and while the body refuses it is `readOnly` rather than
          `disabled`, and takes no Ctrl+Enter, paste or drop (`refusing`). Outside it is
          `disabled` while the write is out, as on `main`. The Inbox's Enter puts the caret in the
          box by its `data-ask-answer` (`askAnswer`), as its 1 to 9 find an option by
          `data-ask-option` (`Inbox.tsx`). */}
      <MentionField
        askAnswer={true}
        body={answerText}
        className="font-normal"
        disabled={disabledWhileOut}
        fieldClassName={tooltips ? undefined : ON_PAGE.field}
        fieldRef={answerFieldRef}
        hintClassName={tooltips ? undefined : `text-xs ${ON_PAGE.quiet}`}
        issueKey={displayedAsk.issue_key ?? undefined}
        label={
          reasonRequired
            ? ASK_FORM_WORDS.reason_required
            : isApproval
              ? ASK_FORM_WORDS.reason_label
              : ASK_FORM_WORDS.your_answer
        }
        labelClassName={tooltips ? undefined : `block text-sm font-medium ${ON_PAGE.muted}`}
        labelHidden={!reasonRequired}
        onChange={(change) => {
          setAnswerText(change.body);
          setQuestionChoice(false);
        }}
        onDrop={
          uploadOwner === undefined
            ? undefined
            : (event) => {
                if (!refusing) upload.drop(event, uploadOwner, ask.id);
              }
        }
        onKeyDown={(event) => submitOnModifiedEnter(event, { disabled: uploading || refusing })}
        onPaste={
          uploadOwner === undefined
            ? undefined
            : (event) => {
                if (!refusing) upload.paste(event, uploadOwner, ask.id);
              }
        }
        placeholder={answerPlaceholder}
        project={askDocumentProject(ask, displayedAsk, owner)}
        readOnly={holdsFocus && refusing}
        rows={2}
        writing={dispatchWritingBox("ask-answer", "ask-answer")}
      />
      <DraftUploadStatus upload={upload} />
    </>
  );
  const submitButton = (
    <button
      // While nothing can be sent it is `disabled` alone, on both sides of `NoTooltips`; while the
      // body refuses, that is read from the form as the refusal found it (`held`).
      aria-disabled={cannotAnswer || uploading ? undefined : refusedWhileOut}
      aria-describedby={submitHint === undefined ? undefined : submitHintId}
      className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${filledLook}`}
      disabled={cannotAnswer || disabledWhileOut || uploading}
      title={tooltips ? submitHint : undefined}
      type="submit"
    >
      {mutation.isPending
        ? changing
          ? ASK_FORM_WORDS.saving
          : ASK_FORM_WORDS.answering
        : changing
          ? ASK_FORM_WORDS.save_answer
          : ASK_FORM_WORDS.answer}
    </button>
  );
  const submitHintNode =
    submitHint === undefined ? null : (
      <p className={`text-xs ${tooltips ? textMutedOnSurface : ON_PAGE.quiet}`} id={submitHintId}>
        {submitHint}
      </p>
    );
  const answerActions = choiceShown ? (
    <fieldset
      aria-label={ASK_FORM_WORDS.question_shaped}
      className="flex gap-2"
      {...marks?.pageChrome}
    >
      <button
        aria-disabled={refusedWhileOut}
        className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${filledLook}`}
        disabled={disabledWhileOut}
        onClick={() => {
          if (!refusing) sendClarification();
        }}
        ref={tooltips ? (node) => node?.focus() : focusAskBackInstead}
        type="button"
      >
        {clarification.isPending ? ASK_FORM_WORDS.sending : ASK_FORM_WORDS.ask_back_instead}
      </button>
      <button
        aria-disabled={refusedWhileOut}
        className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryLook}`}
        disabled={disabledWhileOut}
        onClick={() => {
          if (!refusing) sendAnswer(answerText);
        }}
        type="button"
      >
        {ASK_FORM_WORDS.answer_anyway}
      </button>
    </fieldset>
  ) : (
    <div className="space-y-2" {...marks?.pageChrome}>
      <div className="flex gap-2">
        {submitButton}
        {changing ? (
          <button
            aria-disabled={refusedWhileOut}
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryLook}`}
            disabled={disabledWhileOut}
            onClick={() => {
              if (!refusing) cancelChanging();
            }}
            type="button"
          >
            {ASK_FORM_WORDS.cancel}
          </button>
        ) : (
          <button
            // While it has no text to send it is `disabled` alone, on both sides of `NoTooltips`;
            // while the body refuses, that is read from the form as the refusal found it (`held`),
            // so the text the landing clears leaves it refused and not `disabled`.
            aria-disabled={nothingToAskBack || uploading ? undefined : refusedWhileOut}
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryLook}`}
            disabled={nothingToAskBack || disabledWhileOut || uploading}
            onClick={() => {
              if (!refusing) sendClarification();
            }}
            type="button"
          >
            {clarification.isPending ? ASK_FORM_WORDS.sending : ASK_FORM_WORDS.ask_back}
          </button>
        )}
      </div>
      {submitHintNode}
    </div>
  );

  if (completed !== null) {
    return (
      <>
        <AskCompletionCard
          agentNames={agentNames}
          answers={answers}
          artifactSlug={artifactSlug}
          ask={completed}
          documentLink={documentLink}
          edits={edits}
          frame={frame}
          markRef={markRecord}
          onChangeAnswer={canChangeAnswer ? startChanging : undefined}
          palette={palette}
          renderText={renderText}
        />
        {unsentAnswer === "" ? null : (
          <UnsentBox
            className="mt-3"
            testId="ask-unsent-answer"
            text={unsentAnswer}
            why={ASK_CARD_WORDS.unsent_answer}
          />
        )}
        {answerFailure === null ? null : (
          <QueryError
            message={
              // This branch offers no Retry: the question has closed, so an answer the reader
              // stopped is said with what the body shows in place of its form.
              mutation.error instanceof SendStoppedError
                ? stoppedWriteNote(
                    ASK_FORM_WORDS.stopped.answer,
                    ASK_FORM_WORDS.stopped.answer_done,
                    {
                      anew: ASK_FORM_WORDS.stopped.closed,
                    }
                  )
                : answerFailure.message
            }
          />
        )}
      </>
    );
  }

  // Who asked: a session's name as the caller's control where the caller hands one (the card's
  // toggle of its copy handles), else the agent alone. The asker's control keeps its name and
  // opens what the caller folds behind it; the component's live link sits beside it, outside the
  // control. `whitespace-pre` keeps the space before an attribution suffix, which a flex item
  // would drop; inside `NoTooltips` `whitespace-pre-wrap` keeps it too, and lets the name wrap.
  const asker =
    sessionAuthor === undefined || askerControl === undefined ? (
      askerName(
        displayedAsk.author.id === "" ? (
          ASK_LINE_WORDS.asked
        ) : (
          <AskAgent actor={displayedAsk.author} agentNames={agentNames} marks={marks} />
        )
      )
    ) : agentNames === "live" ? (
      <AgentName
        actor={sessionAuthor}
        className={`flex min-w-0 items-center ${tooltips ? "whitespace-pre" : "whitespace-pre-wrap"}`}
        control={(name) => askerControl(askerName(name))}
        kind="control"
      />
    ) : (
      askerControl(askerName(authored(marks, agentText.label(sessionAuthor))))
    );
  // What stands at the right end of the first line: an approval request's mark and the urgency
  // word a caller asked for beside the kind, the kind in its colour (the new page's tag,
  // `NodeRow.tsx`'s `RowMargin`), then who asked and when. In a document's ask block the block's
  // frame draws the kind, so the line holds who asked and when alone. Outside `NoTooltips` it is
  // one line, and the asker's name is what gives up width, cut with its tooltip; inside it no
  // title gives back what an ellipsis cut, and a name and an instant are values, which wrap. In a
  // body narrower than 32rem - the comments margin at its default 384 px, the Inbox's drawer, a
  // phone - the line stands under the words, in the same order from the left, and each of its
  // parts moves to a line of its own before it gives up width. The rule reads the body's own box
  // (`@container/ask-body` on its root), never the window's: at 1440 px the margin's body is
  // about 320 px wide, and beside the words there the line left them some 90 px, which broke a
  // word such as "troubleshooting" over two lines, and the asker's name a few letters. Beside the
  // words the line takes up to three quarters of the row, about 200 px for a kind, a short name
  // and a time, so a body of 32rem still leaves the words a column of some 270 px. Who asked and
  // when are one part, narrow and wide: the time never opens a line of its own. Outside
  // `NoTooltips` the time is held whole and the asker's name gives up the width the time needs
  // (`askerName` says how); inside, the time is a value that breaks anywhere as the name does,
  // and the part's one flex line keeps the two together.
  const firstLineEnd = (
    <div
      className={`flex min-w-0 max-w-[75%] items-center justify-end gap-x-1.5 text-xs leading-6 @max-[32rem]/ask-body:max-w-full @max-[32rem]/ask-body:basis-full @max-[32rem]/ask-body:flex-wrap @max-[32rem]/ask-body:justify-start ${tooltips ? `shrink-0 whitespace-nowrap ${textMutedOnSurface}` : `flex-wrap ${ON_PAGE.quiet}`}`}
    >
      {inBlock ? null : (
        <>
          {isApproval ? (
            <>
              <span className="shrink-0 font-semibold">{ASK_APPROVAL_WORDS.requested}</span>
              <Dot />
            </>
          ) : null}
          {urgencyText === undefined ? null : (
            <>
              <span className="shrink-0 font-semibold">{urgencyText}</span>
              <Dot />
            </>
          )}
          <span className={`shrink-0 font-semibold ${pageKindTag.question}`}>
            {ASK_LINE_WORDS.kind}
          </span>
          <Dot />
        </>
      )}
      <span className="flex min-w-0 items-center gap-x-1.5">
        {asker}
        <Dot />
        <Timestamp at={displayedAsk.created_at} className={tooltips ? "shrink-0" : undefined} />
      </span>
    </div>
  );
  const rest = (
    <>
      <div className="flex items-start justify-end gap-x-3 @max-[32rem]/ask-body:flex-wrap @max-[32rem]/ask-body:justify-start">
        {inBlock ? null : (
          <div
            className={`min-w-0 flex-1 text-[15px] leading-relaxed font-medium @max-[32rem]/ask-body:basis-full ${tooltips ? textPrimaryOnSurface : ON_PAGE.ink}`}
          >
            {renderText({
              markdown: displayedAsk.question,
              source,
              surface: "ask-card-question",
              tree: displayedAsk.question_tree,
            })}
          </div>
        )}
        {firstLineEnd}
      </div>
      {isApproval && documentLink ? (
        <p className={`mt-1 text-sm ${tooltips ? `${linkText} ${linkHoverText}` : ON_PAGE.link}`}>
          <AskApprovalLink ask={displayedAsk} marks={marks} />
        </p>
      ) : null}
      {/* A request to approve again names the approval it follows, under the question (the
          spec's own placement), with the diff from that version behind Show diff; a first
          request carries no earlier approval, and EarlierApproval renders nothing for it. */}
      {displayedAsk.approval === undefined ? null : (
        <EarlierApproval
          agentNames={agentNames}
          approval={displayedAsk.approval}
          marks={marks}
          palette={palette}
        />
      )}
      {/* Whose turn it is, the one part of the ask's provenance a reader acts on, on its own line
          under the question. An agent's name is what gives up width. */}
      {turnHolder === null ? null : (
        <p className={`mt-1.5 flex min-w-0 text-xs${tooltips ? " whitespace-nowrap" : ""}`}>
          <span
            className={`${turnHolder === "you" ? "shrink-0" : tooltips ? "min-w-0 truncate" : "min-w-0 [overflow-wrap:anywhere]"} font-semibold ${tooltips ? textSecondaryOnSurface : ON_PAGE.muted}`}
            data-testid={`turn-${displayedAsk.id}`}
          >
            {`${ASK_LINE_WORDS.waiting_on} `}
            {turnHolder === "you" ? (
              ASK_LINE_WORDS.you
            ) : (
              <AskAgent actor={turnHolder} agentNames={agentNames} inline marks={marks} />
            )}
          </span>
        </p>
      )}
      {/* Whose question it is now, and the one control that hands it to somebody else. An
          ask that is not open waits on nobody, so it has none: a body whose ask is answered,
          resolved or withdrawn draws its record instead, and the one case that reaches here
          is a reader changing the answer of a closed ask. The ask is the fresher read and
          the issue is the caller's own: only an Inbox row carries one, and the ask reads
          do not. */}
      {displayedAsk.state === "open" ? (
        <AskHandOn ask={displayedAsk} assignee={ask.issue?.assignee ?? null} marks={marks} />
      ) : null}
      {askChanged ? (
        <p className={`mt-3 text-sm font-medium ${tooltips ? inlineWarningText : ON_PAGE.warning}`}>
          {ASK_FORM_WORDS.changed}
        </p>
      ) : null}
      {/* The options, and under them the answer box, which always stands in place. */}
      <form
        className="mt-3 space-y-3"
        // While the body refuses a submit sends nothing: inside `NoTooltips` a press of Answer,
        // or Enter in an option, still reaches it then (`refusing`).
        onSubmit={(event) => {
          if (refusing) event.preventDefault();
          else submit(event);
        }}
        ref={trackForm}
        {...marks?.authorText}
      >
        {(displayedAsk.options ?? []).length === 0 ? null : (
          // Inside `NoTooltips`, while the body refuses, the options are refused by their group's
          // `aria-disabled`, which ARIA passes to each option in it: an option row puts no
          // attribute of its own on its input (`AskChoiceRow`).
          <fieldset
            aria-disabled={refusedWhileOut}
            className="min-w-0 space-y-2"
            {...marks?.pageChrome}
          >
            <legend className="sr-only">{ASK_FORM_WORDS.options}</legend>
            {(displayedAsk.options ?? []).map((option) => (
              <AskChoiceRow
                checked={selected.includes(option.label)}
                disabled={disabledWhileOut}
                hotkey
                key={option.label}
                multiple={displayedAsk.multiple}
                name={`ask-${displayedAsk.id}`}
                onChange={() => {
                  if (!refusing) selectRealOption(option.label);
                }}
                option={option}
                palette={palette}
                renderText={renderText}
                source={source}
              />
            ))}
            {isApproval ? null : (
              <AskChoiceRow
                checked={otherSelected}
                disabled={disabledWhileOut}
                multiple={displayedAsk.multiple}
                name={`ask-${displayedAsk.id}`}
                onChange={() => {
                  if (!refusing) toggleOther();
                }}
                option={{ label: ASK_FORM_WORDS.other }}
                palette={palette}
                renderText={renderText}
                source={source}
              />
            )}
          </fieldset>
        )}
        {answerField}
        {answerActions}
        {answerFailure === null ? null : (
          <QueryError
            message={answerFailure.message}
            onRetry={answerFailure.retryable ? () => submitGuard.retryLast(mutation) : undefined}
            retrying={mutation.isPending}
          />
        )}
        {clarification.isError ? (
          <QueryError
            message={
              // A question back the reader stopped may have been sent, and Retry makes that very
              // press again (`retryLast`).
              stoppedNote(
                clarification,
                ASK_FORM_WORDS.stopped.clarification,
                ASK_FORM_WORDS.stopped.clarification_done
              ) ?? ASK_FORM_WORDS.clarification_failed
            }
            onRetry={() => submitGuard.retryLast(clarification)}
            retrying={clarification.isPending}
          />
        ) : null}
      </form>
      {/* The person's way out of a question they will not answer, under the form that answers
          it. An approval request draws none: it is answered with Approve or Request changes. */}
      {displayedAsk.state === "open" && displayedAsk.kind === "question" ? (
        <DeclineControl
          ask={displayedAsk}
          decline={decline}
          marks={marks}
          onDeclined={onAnswered}
          palette={palette}
        />
      ) : null}
    </>
  );

  // The root is the size container the first line reads (`firstLineEnd`). Every caller draws it
  // as a block as wide as the box it gives the body, so the inline-size containment a container
  // takes - its own content no longer widens it - takes no width from the body.
  return (
    <div className="@container/ask-body" data-ask-body="" {...marks?.pageChrome}>
      <AskAnchorHeader
        ask={displayedAsk}
        inBlock={inBlock}
        marks={marks}
        palette={palette}
        quoteSpacing="roomy"
        tone="surface"
      />
      <OrphanedAnchorNotice artifactSlug={artifactSlug} ask={displayedAsk} palette={palette} />
      {inBlock ||
      displayedAsk.block_id === undefined ||
      displayedAsk.block_id === null ||
      displayedAsk.block_artifact === undefined ? null : (
        <p className={`mt-2 text-sm ${tooltips ? `${linkText} ${linkHoverText}` : ON_PAGE.link}`}>
          <AskBlockLink ask={displayedAsk} />
        </p>
      )}
      {inBlock ? (
        rest
      ) : (
        // The bullet hangs left of the question's words, and everything under them stands at
        // the words' left edge: an 18 px column, its 7 px dot centred on the first line.
        <div className="flex gap-x-2">
          <KindBullet colour={pageKindBullet.question} mark="data-ask-bullet" />
          <div className="min-w-0 flex-1">{rest}</div>
        </div>
      )}
    </div>
  );
}
