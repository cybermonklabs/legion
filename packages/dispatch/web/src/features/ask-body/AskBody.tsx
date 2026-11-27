import { dispatchWritingBox } from "@legion/contracts";
import { type ReactNode, useCallback, useEffect, useLayoutEffect, useRef } from "react";

import type { DeclineAskInput, WriteBounds } from "../../api/client";
import type { Ask } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { Dot, KindBullet } from "../../components/RowMarks";
import { useTooltips } from "../../components/tooltips";
import {
  inlineWarningText,
  linkHoverText,
  linkText,
  pageInkFill,
  pageInkText,
  pageKindBullet,
  pageKindTag,
  pageLineBorder,
  pageMutedText,
  pageStateText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButton,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import type { AgentNames } from "../conversation/CachedAuthor";
import { MentionField, type MentionFieldHandle } from "../conversation/MentionField";
import { type AskAnswerActions, AskAnswerArea } from "../inbox/AskAnswerArea";
import { askUploadOwner, UnsentBox } from "../inbox/AskThread";
import type { OwnedAsk } from "../inbox/ask-name";
import { askTurnHolder } from "../inbox/ask-turn";
import { askUrgency } from "../inbox/ask-urgency";
import type { AskAnswerForm } from "../inbox/useAskAnswerForm";
import { REF_PICKER_ASK_ANSWER_WORDS } from "../refs/ref-picker";
import { textSourceOf } from "../refs/text-references";
import type { DwellRef } from "../shell/useDwell";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { stoppedNote } from "../writes/useKeyedWrite";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskApprovalLink } from "./AskApprovalLink";
import { AskBlockLink } from "./AskBlockLink";
import { AskCompletionCard, type AskFrame, OrphanedAnchorNotice } from "./AskCompletionCard";
import { AskHandOn } from "./AskHandOn";
import { AskHeader } from "./AskHeader";
import { AskChoiceRow } from "./AskOptionRow";
import { AskAgent } from "./ask-agent";
import {
  ASK_APPROVAL_WORDS,
  ASK_CARD_WORDS,
  ASK_FORM_WORDS,
  ASK_LINE_WORDS,
} from "./ask-body-words";
import { type AskPalette, type AskTextRenderer, ownPart, type TextMarks } from "./ask-text";
import { DeclineControl } from "./DeclineControl";
import { EarlierApproval } from "./EarlierApproval";

/**
 * Each colour the body's own elements draw inside `NoTooltips`, the new issue page's, by the
 * role `main`'s colour has: the question is the ink, what a reader acts on the muted grey, what
 * qualifies it the quiet grey, a link the ink, and a question that changed under the reader the
 * page's warning, its waiting orange; Ask back instead is filled with the ink, as the page's one
 * filled control is, and Cancel is its header control's rule and muted words. What the body's
 * children draw is theirs: the first line (`AskHeader`), the answer box and the sends that refuse
 * with a reason (`AskAnswerArea`, `MentionField`, `RefusableButton`), the options, the earlier
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
  filled: `${pageInkFill} refused:cursor-not-allowed refused:bg-page-hair refused:text-page-muted`,
  ink: pageInkText,
  link: pageInkText,
  muted: pageMutedText,
  warning: pageStateText.waiting,
} as const;

/** The names the body's sends know their refusals by, which the answer area is typed over: a
 *  misspelling is then a type error rather than a button whose on-screen reason goes missing
 *  (`inbox/AskAnswerArea.tsx`). */
type AnswerRefusalName = "answer" | "anyway" | "askBack";

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
   *  them each element of the open body that holds the body's own words - a label, a line, a
   *  control, the dot between two parts of a line - wears the mark of those words, which are the
   *  issue page's declared ones (`ask-body-words.ts`), and each value inside one - who asked,
   *  who it waits on, the document, a version, a date - the mark of a value, as the caller's
   *  renderer marks the ask's own texts. The mark stands on each such element and never on a
   *  box that holds several: a reader of the page takes each marked element's words whole, with
   *  its values taken out, and two controls in one marked box read as one word ("Answer" and
   *  "Ask back" as "answerask"). The shared answer box, with its hint, and a refusal under it,
   *  are the box's and the server's words, so the form that holds them wears the mark of a
   *  value, and the options' name and each action in it the body's again. Dashboard hands them;
   *  no other host does, so every other surface draws the elements it always drew, with no mark. */
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
 * test id names (`AskCompletionCard`). The root that draws the ask to answer carries
 * `data-ask-answering` too, and a record's root does not: a page's single keys stay off an
 * ask's answering controls where no card stands around them, and keep working on a record's
 * (`inbox/ask-card.ts`).
 *
 * An open ask is root R34's row, as the new issue page draws a question's (`NodeRow.tsx`): a
 * bullet in the question's colour, then the question's words, with the kind, who asked and when,
 * the state label and the copy control at the right end of their first line, or under the words
 * in a body narrower than 32rem (`AskHeader`, the one header every place draws an ask's fields
 * through), and under the words everything else the body draws. In a document's ask block the
 * block's frame draws the question and its kind, so the body draws neither, and no bullet; its
 * first line keeps who asked and when. The answer box always stands in place under the options,
 * in the ask's one answer area (`AskAnswerArea`).
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
    answerPlaceholder,
    answerRefusal,
    answers,
    answerText,
    askChanged,
    canChangeAnswer,
    cancelChanging,
    changing,
    choiceShown,
    clarification,
    clarificationRefusal,
    completed,
    disabledWhileOut,
    displayedAsk,
    edits,
    isApproval,
    mutation,
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
  const uploadOwner = askUploadOwner(displayedAsk, ask.document ?? owner);
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
  // Ask back instead takes the focus as it is drawn, once: its callback is stable across renders,
  // as `useFormRef`'s is, so a re-render cannot take the focus back from Answer with it anyway
  // while its answer is out, or from another card of the same ask whose box edits the shared
  // draft (dispatch://INFRA-341 R2). Inside `NoTooltips` the choice leaves the page, and the
  // button with it, once the ask is read again after the clarification; a focus the button still
  // holds then goes where the form's own hand-off puts it, not to the body.
  const holdsFocus = !tooltips;
  const focusAskBackInstead = useCallback(
    (button: HTMLButtonElement | null) => {
      if (button === null) return undefined;
      button.focus();
      if (!holdsFocus) return undefined;
      return () => {
        const form = formRef.current;
        if (form !== null && document.activeElement === button) handOffFocus(form);
      };
    },
    [formRef, holdsFocus]
  );
  const urgency = askUrgency(displayedAsk.urgency);
  // The urgency word a caller asked for stands beside the kind, where the card's notch would say
  // it: for an ask so urgent it carries one.
  const urgencyText =
    urgencyWord && (urgency === "blocking" || urgency === "high")
      ? ASK_LINE_WORDS.urgency[urgency]
      : undefined;
  const turnHolder = askTurnHolder(displayedAsk, (threadQuery.data?.replies ?? []).at(-1));
  // A send this body draws keeps the focus while the body refuses, unless an upload holds it:
  // that one is `disabled` on both sides (`RefusableButton`'s `busy`).
  const sendRefusedWhileOut = refusedWhileOut === true && !uploading;

  // Inside `NoTooltips` the actions the body draws itself are the page's (`ON_PAGE`): Ask back
  // instead filled with the ink, and Cancel its header control's rule and muted words. Answer,
  // Answer with it anyway and Ask back are `RefusableButton`s, whose looks are that component's
  // on both sides.
  const filledLook = tooltips
    ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`
    : ON_PAGE.filled;
  const answerLabel = changing ? ASK_FORM_WORDS.save_answer : ASK_FORM_WORDS.answer;
  /** What Answer says while it is busy: this body's own send, or an upload the draft is waiting
   *  on, which keeps the label while the picture lands. */
  const answerBusy = mutation.isPending
    ? changing
      ? ASK_FORM_WORDS.saving
      : ASK_FORM_WORDS.answering
    : uploading
      ? answerLabel
      : undefined;
  // Each send refuses the draft as it stands with its reason, on a line under the box and as its
  // accessible description, and keeps the focus (dispatch://INFRA-341 R3): only its own send, or
  // an upload the draft is waiting on, disables it. Inside `NoTooltips` its own send, and the read
  // after it, refuse it holding the focus (`sendRefusedWhileOut`).
  const answerActions = ({ reasonId }: AskAnswerActions<AnswerRefusalName>): ReactNode =>
    choiceShown ? (
      // The pair a question-shaped answer offers is two more sends of this body, so a picture the
      // draft is still taking holds them both, in the shape their siblings keep: the label stands
      // and the press is taken away until the upload lands.
      <fieldset aria-label={ASK_FORM_WORDS.question_shaped} className="flex gap-2">
        <button
          aria-disabled={sendRefusedWhileOut || undefined}
          className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${filledLook}`}
          disabled={disabledWhileOut || uploading}
          onClick={() => {
            if (!refusing) sendClarification();
          }}
          ref={focusAskBackInstead}
          type="button"
          {...marks?.pageChrome}
        >
          {clarification.isPending ? ASK_FORM_WORDS.sending : ASK_FORM_WORDS.ask_back_instead}
        </button>
        <RefusableButton
          busy={
            mutation.isPending
              ? ASK_FORM_WORDS.answering
              : uploading
                ? ASK_FORM_WORDS.answer_anyway
                : undefined
          }
          look="secondary"
          onPress={() => sendAnswer(answerText)}
          refusal={answerRefusal}
          refusalShownBy={reasonId.anyway}
          refusedWhileOut={sendRefusedWhileOut}
          wordsMark={marks?.pageChrome}
        >
          {ASK_FORM_WORDS.answer_anyway}
        </RefusableButton>
      </fieldset>
    ) : (
      <div className="flex gap-2">
        <RefusableButton
          busy={answerBusy}
          className="min-h-11"
          refusal={answerRefusal}
          refusalShownBy={reasonId.answer}
          refusedWhileOut={sendRefusedWhileOut}
          type="submit"
          wordsMark={marks?.pageChrome}
        >
          {answerLabel}
        </RefusableButton>
        {changing ? (
          <button
            aria-disabled={refusedWhileOut}
            className={
              tooltips
                ? secondaryButton
                : `min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${ON_PAGE.action}`
            }
            disabled={disabledWhileOut}
            onClick={() => {
              if (!refusing) cancelChanging();
            }}
            type="button"
            {...marks?.pageChrome}
          >
            {ASK_FORM_WORDS.cancel}
          </button>
        ) : (
          <RefusableButton
            busy={
              clarification.isPending
                ? ASK_FORM_WORDS.sending
                : uploading
                  ? ASK_FORM_WORDS.ask_back
                  : undefined
            }
            look="secondary"
            onPress={sendClarification}
            refusal={clarificationRefusal}
            refusalShownBy={reasonId.askBack}
            refusedWhileOut={sendRefusedWhileOut}
            wordsMark={marks?.pageChrome}
          >
            {ASK_FORM_WORDS.ask_back}
          </RefusableButton>
        )}
      </div>
    );
  /** The body's half of the ask's one answer area (`inbox/AskAnswerArea.tsx`, which the reply box
   *  under an answered ask mounts too): the draft and the field's paste, drop and keys are the
   *  area's, and the box and these submits are the body's, because Answer, Save answer and Ask
   *  back all post through `useAskAnswerForm` rather than as a comment of the box's own, and the
   *  box is the ask's one row of the writing list. Inside `NoTooltips`, while the body refuses,
   *  the box is `readOnly` in place of `disabled`, and takes no paste, drop or Ctrl+Enter; outside
   *  it is `disabled` while the write is out, as on `main`. The Inbox's Enter puts the caret in
   *  the box by its `data-ask-answer` (`askAnswer`), as its 1 to 9 find an option by
   *  `data-ask-option` (`Inbox.tsx`). */
  const answerArea = (
    <AskAnswerArea
      actions={answerActions}
      draft={answerText}
      field={(props) => (
        <MentionField
          {...props}
          askAnswer={true}
          className="font-normal"
          disabled={disabledWhileOut}
          fieldRef={answerFieldRef}
          issueKey={displayedAsk.issue_key ?? undefined}
          label={
            reasonRequired
              ? ASK_FORM_WORDS.reason_required
              : isApproval
                ? ASK_FORM_WORDS.reason_label
                : ASK_FORM_WORDS.your_answer
          }
          labelHidden={!reasonRequired}
          // The picker's words where they differ from every other box's: an answer delivers to
          // the agents and roles it names (dispatch://INFRA-341 R14).
          pickerWords={REF_PICKER_ASK_ANSWER_WORDS}
          placeholder={answerPlaceholder}
          project={askDocumentProject(ask, displayedAsk, owner)}
          writing={dispatchWritingBox("ask-answer", "ask-answer")}
        />
      )}
      onDraftChange={(text) => {
        setAnswerText(text);
        setQuestionChoice(false);
      }}
      readOnly={refusedWhileOut === true}
      reasonMark={marks?.pageChrome}
      // Every name the body's buttons know, whichever pair is drawn: a reason that is null shows
      // no line, so the two the other pair uses cost the body nothing.
      refusals={
        choiceShown
          ? { answer: null, anyway: answerRefusal, askBack: null }
          : {
              answer: answerRefusal,
              anyway: null,
              askBack: changing ? null : clarificationRefusal,
            }
      }
      upload={upload}
      uploadDraft={ask.id}
      uploadOwner={uploadOwner}
    />
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
          owner={ask.document ?? owner}
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

  // What leads the first line's end, before who asked and when (`AskHeader`'s first line): an
  // approval request's mark and the urgency word a caller asked for beside the kind, then the kind
  // in its colour (the new page's tag, `NodeRow.tsx`'s `RowMargin`). In a document's ask block the
  // block's frame draws the kind, so nothing leads. Each word of the body's on it, and each dot
  // between two parts, carries the mark of the body's words on its own element
  // (`AskBodyProps.marks`).
  const dot = ownPart(marks, <Dot />);
  const lead = inBlock ? null : (
    <>
      {isApproval ? (
        <>
          <span className="shrink-0 font-semibold" {...marks?.pageChrome}>
            {ASK_APPROVAL_WORDS.requested}
          </span>
          {dot}
        </>
      ) : null}
      {urgencyText === undefined ? null : (
        <>
          <span className="shrink-0 font-semibold" {...marks?.pageChrome}>
            {urgencyText}
          </span>
          {dot}
        </>
      )}
      <span className={`shrink-0 font-semibold ${pageKindTag.question}`} {...marks?.pageChrome}>
        {ASK_LINE_WORDS.kind}
      </span>
      {dot}
    </>
  );
  const rest = (
    <>
      {/* The one header every place draws an ask's fields through (dispatch://INFRA-341 R1), on
          the first line of root R34's row (dispatch://INFRA-5702 R15): the question's words, and
          at the right end of their first line the kind, who asked and when, the state label and
          the copy control. */}
      <AskHeader
        agentNames={agentNames}
        ask={displayedAsk}
        control={askerControl}
        inBlock={inBlock}
        leading={lead}
        marks={marks}
        owner={ask.document ?? owner}
        place="card"
      >
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
      </AskHeader>
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
            {...marks?.pageChrome}
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
        <p
          className={`mt-3 text-sm font-medium ${tooltips ? inlineWarningText : ON_PAGE.warning}`}
          {...marks?.pageChrome}
        >
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
          <fieldset aria-disabled={refusedWhileOut} className="min-w-0 space-y-2">
            <legend className="sr-only" {...marks?.pageChrome}>
              {ASK_FORM_WORDS.options}
            </legend>
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
        {answerArea}
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

  // The root is the size container the first line reads (`AskHeader`). Every caller draws it
  // as a block as wide as the box it gives the body, so the inline-size containment a container
  // takes - its own content no longer widens it - takes no width from the body.
  return (
    <div className="@container/ask-body" data-ask-answering="" data-ask-body="">
      <AskAnchorHeader
        ask={displayedAsk}
        inBlock={inBlock}
        marks={marks}
        palette={palette}
        quoteSpacing="roomy"
        tone="surface"
      />
      <OrphanedAnchorNotice
        artifactSlug={artifactSlug}
        ask={displayedAsk}
        marks={marks}
        palette={palette}
      />
      {inBlock ||
      displayedAsk.block_id === undefined ||
      displayedAsk.block_id === null ||
      displayedAsk.block_artifact === undefined ? null : (
        <p
          className={`mt-2 text-sm ${tooltips ? `${linkText} ${linkHoverText}` : ON_PAGE.link}`}
          {...marks?.pageChrome}
        >
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
