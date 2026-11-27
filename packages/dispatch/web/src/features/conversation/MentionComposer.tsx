import {
  type DispatchWriting,
  type DispatchWritingBox,
  dispatchWritingBox,
} from "@legion/contracts";
import type { MutationKey } from "@tanstack/react-query";
import {
  type FormEvent,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
  type Ref,
  type SyntheticEvent,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import {
  ApiError,
  api,
  apiErrorMessage,
  isAnchorMissing,
  isCapExceeded,
  isDecisionBlockInBody,
} from "../../api/client";
import type { Agent, AskUrgency } from "../../api/types";
import { Chip } from "../../components/Chip";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useFootInset } from "../../hooks/useFootInset";
import {
  borderDefault,
  borderStrong,
  calloutInfoBg,
  calloutInfoBorder,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  checkboxAccent,
  dangerHoverBorder,
  dangerText,
  dismissButtonText,
  focusBorder,
  focusRing,
  hoverToDangerText,
  linkHoverText,
  linkText,
  pageComposer,
  pageComposerControl,
  pageFailedText,
  pageHairBorder,
  pageInkText,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  primaryButtonBg,
  quoteAccentBorder,
  quoteBodyText,
  secondaryButtonText,
  surfaceBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import {
  appendToDraft,
  type DraftUpload,
  DraftUploadStatus,
  uploadErrorMessage,
} from "../artifacts/ArtifactUpload";
import { ASK_URGENCIES_ASCENDING, URGENCY_LABELS } from "../inbox/ask-urgency";
import { useComposerRegistry } from "../margin/margin-context";
import { useAgentText } from "../refs/AgentName";
import { mentionOptions } from "../refs/party-options";
import { draftParties } from "../refs/party-references";
import { postDeletion } from "../refs/post-marks";
import { isCompositionKey, type RefPickerWords } from "../refs/ref-picker";
import { partyOfAddress } from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import {
  retriesUnderItsKey,
  SendExpiredError,
  SendInterruptedError,
  SendStoppedError,
} from "../writes/keyed-write";
import { StopTrying } from "../writes/StopTrying";
import { endedStopped, stoppedNote } from "../writes/useKeyedWrite";
import { KEY_HOURS } from "../writes/write-words";
import { type FollowedBlocks, followEdit, spansFromBlocks, textareaValue } from "./block-spans";
import type { AgentNames } from "./CachedAuthor";
import {
  type AskOptionDraft,
  type Draft,
  emptyAskOption,
  openingSeeds,
  prefillMentions,
  replyPrefill,
  type SeededMention,
  seededDraft,
  submittedAskOptions,
} from "./composer-draft";
import {
  type AnchorRefusal,
  type ComposerAnchor,
  type ComposerKind,
  type ComposerOwner,
  type DraftIntake,
  hasUnsavedInput,
  type ListedReplies,
  type ReplyListing,
  type ReplyParent,
  type ReplyTarget,
} from "./composer-model";
import {
  COMPOSER_WORDS,
  type ComposerWords,
  DELIVERY_COMMANDS,
  DISCARD_DRAFT_LABEL,
} from "./composer-words";
import {
  deliveryModesOf,
  MODE_LABELS,
  noDeliveryReasons,
  plainDelivery,
  TITLES_JOINER,
} from "./delivery";
import { DraftPills, draftSource, useDraftPills } from "./draft-pills";
import type { HeldSend } from "./held-sends";
import { DraftOffer, KeptDraftOffers } from "./KeptDraftOffers";
import { type GoneReplyWords, type KeptReplyWords, keptAwayLine } from "./KeptReply";
import { MentionField, type MentionFieldChange, type MentionFieldHandle } from "./MentionField";
import { ReplyQuote } from "./ReplyQuote";
import {
  type Address,
  addressWords,
  carriedIdsOf,
  type DeliveryPlan,
  deliveryPlan,
  heldSeeds,
  requestFor,
  type SaveCommentEdit,
  SendMovedError,
  type SentDraft,
  type SentRequest,
  type StatementAnchor,
  sameAddress,
  saveCommentEdit,
  unedited,
} from "./send-request";
import { type AwayAddress, type AwayDraft, useUnsentDrafts } from "./unsent-drafts";
import { useAgents } from "./useAgents";
import { useComposerSend } from "./useComposerSend";
import { useDraftHomes } from "./useDraftHomes";
import { composerNameOf, mountDraft, type OfferedDraft, useKeptDrafts } from "./useKeptDrafts";

/** Where a composer says a reply to a deleted post goes instead, after
 *  `COMPOSER_WORDS.deletedReply`; the issue page's box says both in one sentence of its host's
 *  (`OutlineAnswers.deletedReply`). */
const DELETED_REPLY_WAY = "Press Cancel reply to send it as a message that answers none.";

/** Which of a notice's words fit `named`, the sessions it names: those of one, or of many. */
function oneOrMany(named: readonly unknown[]): "many" | "one" {
  return named.length > 1 ? "many" : "one";
}

/** Whether Send takes the draft now: nothing refuses it (`draftRefusal`, the one rule for what an
 *  empty draft is) and no save or upload it waits on is in flight. */
export function canSubmitComposer(
  refusal: string | undefined,
  isSaving: boolean,
  pendingUploads: number
): boolean {
  return refusal === undefined && !isSaving && pendingUploads === 0;
}

/**
 * Why Send refuses a draft it is not busy with, or undefined when it would take it: an empty
 * draft, and a delivery command with nothing after it - `/btw ` alone, where the box holds text
 * and Send is still dead. A save or an upload in flight names itself on the button instead. The
 * words are `refusals`, the host's or the composer's own (`COMPOSER_WORDS`).
 */
function draftRefusal(
  kind: ComposerKind,
  body: string,
  replacement: string,
  outbound: DeliveryPlan,
  refusals: ComposerWords["refusals"]
): string | undefined {
  if (kind === "suggestion") {
    return replacement.trim() === "" ? "Type the replacement text first." : undefined;
  }
  if (body.trim() === "") return kind === "ask" ? refusals.emptyQuestion : refusals.empty;
  if (
    kind === "comment" &&
    outbound.delivery !== undefined &&
    outbound.delivery !== "steer" &&
    outbound.body.trim() === ""
  ) {
    return `${refusals.afterCommand.before}${DELIVERY_COMMANDS[outbound.delivery]}${refusals.afterCommand.after}`;
  }
  return undefined;
}

/** What the composer shows for `refusal`: the host's own words, where it has some about the mark
 *  this send names - the margin's, when the server refused the send for a mark it cannot see and
 *  the margin could not write that mark again - else the words of a refusal written for the
 *  reader (the server's missing anchor, a send they stopped, a text past a bound Dispatch holds
 *  it to, a text holding a decision block), else the page's wording around what went wrong
 *  (`COMPOSER_WORDS.refused`). */
function refusalMessage(
  refusal: Error,
  anchor: ComposerAnchor | undefined,
  anchorRefusal: AnchorRefusal | undefined
): string {
  const anchorMissing = isAnchorMissing(refusal);
  if (anchorMissing && anchorRefusal !== undefined && anchorRefusal.markId === anchor?.mark_id) {
    return anchorRefusal.text;
  }
  return anchorMissing ||
    refusal instanceof SendStoppedError ||
    isCapExceeded(refusal) ||
    isDecisionBlockInBody(refusal)
    ? refusal.message
    : `${COMPOSER_WORDS.refused.before}${apiErrorMessage(refusal, COMPOSER_WORDS.refused.unanswered)}`;
}

/** The refusals of a send that the issue page's box says in a sentence of its own, by Dispatch's
 *  code (`OutlineAnswers.refused`). A comment on a statement whose words stand twice in its
 *  document (`TARGET_AMBIGUOUS`) is said apart, with no Retry (`OutlineLook.statementTwice`). */
export const OUTLINE_REFUSAL_CODES = [
  "ANCHOR_MISSING",
  "CAP_EXCEEDED",
  "DECISION_BLOCK_IN_BODY",
  "INVALID_INPUT",
  "ISSUE_CLOSED",
  "KEY_REUSED",
  "POST_DELETED",
] as const;

/** A refusal of a send the issue page's box says in a sentence of its own. */
export type OutlineRefusalCode = (typeof OUTLINE_REFUSAL_CODES)[number];

/** The refusals of a file uploading into the draft that the issue page's box says in a sentence
 *  of its own: the document the upload replaces moved under it, or the upload named no version
 *  where Dispatch requires one. */
export type OutlineUploadRefusalCode = "PRECONDITION_FAILED" | "PRECONDITION_REQUIRED";

/** Words of a host's line in one form for one session and another for many. */
type OneOrMany = { readonly many: string; readonly one: string };

/**
 * What the issue page's box says in answer to the reader's act, in its host's words: each one
 * sentence, in place of what the composer and the parts it draws say on every other surface in
 * two or three, and in place of Dispatch's own words of a refusal, which the box never draws.
 * A sentence that names something - the sessions a notice is about, where a draft was written -
 * is drawn around that name as the composer draws its own.
 *
 * - `refusals`: why Send refuses the draft, drawn only once the reader presses Send or Enter;
 * - `deletedReply`: Send's refusal of a reply to a post deleted since, by its kind, which says
 *   where the draft goes instead;
 * - `late`: a send past its deadline;
 * - `delivery`: what a send to the sessions the draft names would do - the plain mode they take,
 *   no mode at all (`deaf`, by its cause: a list that holds nothing, `unlisted`, one that holds
 *   no mode this page knows, `unknown`, or some of each, `mixed`), or not the mode the draft asks
 *   for (`unsupported`, with the prefixed modes they would take `instead` said in the same
 *   sentence);
 * - `refused`: a send Dispatch refused, by its code, and `other` for any other code, a failure to
 *   reach Dispatch and a send it did not answer in time, each saying the words are kept;
 * - `stopped`: a send that ended without Dispatch's verdict - the reader stopped it
 *   (`unconfirmed`), a reload ended it (`interrupted`), each around how many hours Dispatch keeps
 *   a write's key (`KEY_HOURS`), its key is spent (`expired`), or the reader carried it to another
 *   address (`moved`, around where it went, in `addressWords`);
 * - `upload`: a file uploading into the draft that the reader stopped, as `stopped` says a send,
 *   its `unconfirmed` around those hours too, one Dispatch refused by its code, and `other` for
 *   any other end;
 * - `elsewhere`: Send's refusal of a refused draft written for another address than the box
 *   shows: an address (around `addressWords`), a reply whose post is gone, by its kind and what
 *   became of it, or a reply whose post stands;
 * - `keptReply`, `goneReply`, `changedEdit`: what a reply kept for another row, a reply whose post
 *   is gone, and an edit that waits beside a newer comment say (`KeptReply`, `GoneReply`,
 *   `KeptDraftOffers`).
 */
export interface OutlineAnswers {
  readonly changedEdit: string;
  readonly deletedReply: ComposerWords["deletedReply"];
  readonly delivery: {
    readonly deaf: {
      readonly mixed: string;
      readonly unknown: OneOrMany;
      readonly unlisted: OneOrMany;
    };
    readonly plain: ComposerWords["delivery"]["plain"];
    readonly unsupported: {
      readonly after: string;
      readonly before: OneOrMany;
      readonly instead: ComposerWords["delivery"]["unsupported"]["instead"];
    };
  };
  readonly elsewhere: {
    readonly address: { readonly after: string; readonly before: string };
    readonly gone: Readonly<
      Record<ReplyTarget["parentKind"], Readonly<Record<Exclude<ReplyListing, "standing">, string>>>
    >;
    readonly reply: string;
  };
  readonly goneReply: GoneReplyWords;
  readonly keptReply: KeptReplyWords;
  readonly late: string;
  readonly refusals: ComposerWords["refusals"];
  readonly refused: Readonly<Record<OutlineRefusalCode | "other", string>>;
  readonly stopped: {
    readonly expired: string;
    readonly interrupted: { readonly after: string; readonly before: string };
    readonly moved: { readonly after: string; readonly before: string };
    readonly unconfirmed: { readonly after: string; readonly before: string };
  };
  readonly upload: {
    readonly other: string;
    readonly refused: Readonly<Record<OutlineUploadRefusalCode, string>>;
    readonly stopped: {
      readonly expired: string;
      readonly unconfirmed: { readonly after: string; readonly before: string };
    };
  };
}

/** What the issue page's box is named by, in its host's words: the form, the field by what it
 *  writes, the list of who the write is delivered to, and the field's description, which says
 *  what the line's keys do. */
export interface OutlineNames {
  readonly description: string;
  readonly field: { readonly ask: string; readonly comment: string; readonly edit: string };
  readonly form: string;
  readonly targets: string;
}

/** Whether `code` is a refusal the issue page's box says in a sentence of its own. */
function isOutlineRefusalCode(code: string | undefined): code is OutlineRefusalCode {
  return OUTLINE_REFUSAL_CODES.some((known) => known === code);
}

/** What the issue page's box says of `refusal` in its host's words (`OutlineAnswers`): a send
 *  that ended without Dispatch's verdict by how it ended, a refusal by its code, and one sentence
 *  for every other end - never Dispatch's own words. A sentence that names how many hours
 *  Dispatch keeps a write's key is joined around them (`KEY_HOURS`), as `moved` is around where
 *  the send went. */
function outlineRefusalMessage(refusal: Error, answers: OutlineAnswers): string {
  if (refusal instanceof SendMovedError) {
    const { after, before } = answers.stopped.moved;
    return `${before}${addressWords(refusal.stopped, "to")}${after}`;
  }
  if (refusal instanceof SendExpiredError) return answers.stopped.expired;
  if (refusal instanceof SendInterruptedError) {
    const { after, before } = answers.stopped.interrupted;
    return `${before}${KEY_HOURS}${after}`;
  }
  if (refusal instanceof SendStoppedError) {
    const { after, before } = answers.stopped.unconfirmed;
    return `${before}${KEY_HOURS}${after}`;
  }
  if (refusal instanceof ApiError && isOutlineRefusalCode(refusal.code)) {
    return answers.refused[refusal.code];
  }
  return answers.refused.other;
}

/** What the issue page's box says of a file uploading into its draft that ended without landing,
 *  in its host's words (`OutlineAnswers.upload`): one the reader stopped, by whether a Retry
 *  still goes under its key, the hours Dispatch keeps a key joined into the sentence that names
 *  them; one Dispatch refused by a code the box has words for; and one sentence for every other
 *  end - never Dispatch's own words. */
function outlineUploadMessage(
  upload: Pick<DraftUpload<number>, "error" | "method">,
  words: OutlineAnswers["upload"]
): string {
  if (endedStopped(upload)) {
    if (!retriesUnderItsKey(upload.error)) return words.stopped.expired;
    const { after, before } = words.stopped.unconfirmed;
    return `${before}${KEY_HOURS}${after}`;
  }
  const { error } = upload;
  if (
    error instanceof ApiError &&
    (error.code === "PRECONDITION_FAILED" || error.code === "PRECONDITION_REQUIRED")
  ) {
    return words.refused[error.code];
  }
  return words.other;
}

/** What the issue page's box says after the titles of `sessions`, which advertise no delivery
 *  mode, in its host's words (`OutlineAnswers.delivery.deaf`): one sentence for why they take
 *  none - their lists hold nothing (`unlisted`), hold no mode this page knows (`unknown`), or
 *  some of each (`mixed`) - in place of the composer's warning and the sentence for each cause
 *  after it (`noDeliveryReasons`), which tells the causes apart by the same rule. */
function outlineDeafMessage(
  sessions: readonly { readonly capabilities: readonly string[] }[],
  words: OutlineAnswers["delivery"]["deaf"]
): string {
  const listing = sessions.filter((session) => session.capabilities.length > 0);
  if (listing.length === 0) return words.unlisted[oneOrMany(sessions)];
  return listing.length === sessions.length ? words.unknown[oneOrMany(sessions)] : words.mixed;
}

/** Why Send refuses a refusal held for another address than the issue page's box shows, in its
 *  host's words (`OutlineAnswers.elsewhere`): a reply whose post the box lists no longer or lists
 *  deleted, by its kind; a reply whose post stands; or where else the draft was written for. */
function outlineElsewhere(
  held: SentRequest,
  listingOf: (reply: ReplyTarget) => ReplyListing,
  words: OutlineAnswers["elsewhere"]
): string {
  if (held.replyTo === null) {
    return `${words.address.before}${addressWords(held, "for")}${words.address.after}`;
  }
  const listing = listingOf(held.replyTo);
  return listing === "standing" ? words.reply : words.gone[held.replyTo.parentKind][listing];
}

/** The option rows an ask's fields show for `ask`, as a draft keeps it: its options, or one empty
 *  row where it has none. */
function askOptionRows(ask: SentRequest["ask"] | undefined): AskOptionDraft[] {
  return ask === undefined || ask.options.length === 0
    ? [emptyAskOption()]
    : ask.options.map((option) => ({
        ...emptyAskOption(),
        description: option.description ?? "",
        label: option.label,
      }));
}

/**
 * The colours of a reply's quote, of the body's box with its label and the line under it, and of
 * the composer's muted lines (`foot`: the line under Send, the paste line, the status lines,
 * Cancel reply's ×), by the palette the host hands (`MentionComposerProps.palette`), as
 * `issue-page/DecisionActions.tsx` keeps its own: `main`'s are the classes every surface of
 * today's pages draws, and on the new issue page the quote is the page's (`ReplyQuote`'s
 * `palette`), the box the page's field, which keeps `main`'s focus rule, its label the muted
 * grey, and the line under the box and the muted lines the quiet grey, as the ask body's answer
 * box draws them (`AskBody`'s `ON_PAGE`). Send is the page's inside `NoTooltips` whatever the
 * host hands (`RefusableButton`'s `PAGE_LOOKS`).
 */
const TONES = {
  main: { field: undefined, foot: textMutedOnSurfaceMuted, hint: undefined, label: undefined },
  page: {
    field: `${pageHairBorder} ${pagePaperBg} ${pageInkText} placeholder:text-page-quiet ${focusBorder}`,
    foot: pageQuietText,
    hint: `text-xs ${pageQuietText}`,
    label: `block text-sm font-medium ${pageMutedText}`,
  },
} as const;

/** The key of a draft kept for another address than the composer shows, as its line or its offer
 *  is drawn: its whole address - the words a margin compose was at, the reply it answers and the
 *  statement the issue page's box was on - which no two drafts kept under one name share. Two the
 *  box keeps on one statement's row, its words before a new version and after, share the row. The
 *  key names each field of `AwayAddress`, so the compiler asks for a field the address gains. */
function awayKey({ anchor, replyTo, statement }: AwayDraft): string {
  return JSON.stringify({
    anchor: [anchor?.artifact, anchor?.mark_id, anchor?.quote, anchor?.occurrence],
    replyTo: replyTo?.id,
    statement: [statement?.artifact, statement?.quote],
  } satisfies Record<keyof AwayAddress, unknown>);
}

/** One kind the issue page's line writes, as a chip before its Send, in the host's own words
 *  (`OutlineLook`). */
export interface OutlineKind {
  readonly kind: Exclude<ComposerKind, "suggestion">;
  readonly label: ReactNode;
}

/**
 * The composer as the issue page's one place to write, at the foot of its outline
 * (the prototype's `#composer`, whose look `theme/classes.ts`'s
 * `pageComposer` holds): one line, which Enter sends and Shift+Enter breaks, the kinds it writes
 * as chips before Send, and above the line the entry its target names, with the control that
 * ends the target. The host words every part of it, since the composer imports nothing of the
 * page's: its controls, what the composer itself says in answer to the reader's act - a
 * refusal, a delivery warning, a send past its deadline, each in one sentence (`answers`) - what
 * the parts it draws say (`answers`, `picker`), and the names the box is read
 * by (`names`). A part a host gives no words for says its own, as on every surface that draws
 * it. It is drawn in the page's colours. Why Send refuses the draft is drawn only once the
 * reader presses Send, or Enter on a draft that cannot go, on a line of its own. Its
 * draft follows the one rule every composer does (`useKeptDrafts`): a target the host sets - a
 * row's Reply, `r` on a statement, the issue's own Reply - keeps what the line holds for the
 * target it was written for, the issue's own included, and says so, and the line opens on the new
 * target with what that target keeps, or else the reply's mentions alone; a refusal stays with the
 * target it was written for. Only the line's own end of its target - the target line's control
 * and Escape (`end`) - carries the draft, its refusal with it, to the issue, taking out of it the
 * mentions the reply wrote there, where they still open the words as it wrote them.
 */
export interface OutlineLook {
  /** A statement of the issue's spec the line comments on, by the document, the statement's
   *  whole words and its row in the outline (`StatementAnchor`). The send carries the document
   *  and the words as the comment's anchor (`SentRequest.statement`), and the row stays the
   *  page's, for taking the reader back to it once the comment lands. It is part of the line's
   *  address, as a reply is, so what the line keeps it keeps for the statement
   *  (`AwayDraft.statement`). The line's `end` ends it, as it ends a reply. */
  readonly anchor?: StatementAnchor;
  /** The box's own answers to the reader's act, each one sentence (`OutlineAnswers`), in place of
   *  the composer's own words (`COMPOSER_WORDS`), of what the parts the composer draws say, and
   *  of Dispatch's own words of a refusal. With none, those. */
  readonly answers?: OutlineAnswers;
  /** The word of the control at the right of the target line, which ends the target (`end`). */
  readonly cancelReply: string;
  /** The word of the control beside a refusal that drops the draft the refusal handed back. */
  readonly discardDraft: string;
  /** The comment the host's edit line changes while this line waits under it, hidden and still
   *  mounted so that its draft stays the reader's: part of this line's address, as `edit` is part
   *  of the edit line's, so that a reason a press drew on this line goes when the edit opens
   *  (`pressedReason`). None while no edit is open, and on the edit line itself. */
  readonly editing?: { readonly id: string };
  /** What the target line's control and Escape do: the host ends the line's target - a reply, a
   *  statement, an edit - or, with none, leaves the line. The composer counts each as the reader's
   *  own move of its address, as Cancel reply is (`ownMoves`), so the draft goes with it. */
  readonly end: () => void;
  /** The mark the target line's control draws, which `cancelReply` names. */
  readonly endMark: string;
  /** What the line says, in its host's words, of a draft it keeps for the issue (`issue`) or for
   *  a statement (`statement`) while it shows another target, each naming the way back to it;
   *  one it keeps for another reply is said by `keptAwayLine`, in the host's `answers` where it
   *  gives them. The edit line keeps no draft for another target, and its host gives none. */
  readonly kept?: { readonly issue: string; readonly statement: string };
  /** The kinds the line writes; none while it has a target, which takes a comment, or while the
   *  issue is closed. */
  readonly kinds: readonly OutlineKind[];
  /** What the form, the field and the list of who the write is delivered to are named by, and
   *  what the field's description says of its keys, in the host's words (`OutlineNames`). With
   *  none, the composer's own names and the writing list's hint. */
  readonly names?: OutlineNames;
  /** The words the reference picker in the line draws (`RefPickerWords`). With none, the
   *  picker's own. */
  readonly picker?: RefPickerWords;
  /** What the empty line says. */
  readonly placeholder: string;
  /** Retry's word, on the line's refusal of a send and of a file uploading into the draft alike
   *  (`OutlineRefusal`). */
  readonly retry: string;
  /** Send's word. */
  readonly send: string;
  /** What Send says while the send is out: the host's own busy word, as its other controls say
   *  theirs. */
  readonly sending: string;
  /** What the line says when Dispatch stored a file pasted or dropped into the draft and lost its
   *  answer (`409 KEY_COMMITTED`), so the file's reference could not be added to the draft: the
   *  host's own sentence, in place of the one `LandedWithoutAnswer` draws on other surfaces. */
  readonly uploadedUnanswered: string;
  /** What Send says while a file pasted or dropped into the draft is still uploading, and what
   *  the line says of that upload under the field. */
  readonly uploading: string;
  /** What the line says, in place of the server's words, when Dispatch refused a comment on a
   *  statement because the statement's words stand twice in its document (`409
   *  TARGET_AMBIGUOUS`). The server's words ask for an occurrence, which no control of the line
   *  sends, and the same words are refused again, so the line draws no Retry for it; the draft
   *  stays. */
  readonly statementTwice?: string;
  /** Which statements the host draws now (`drawn`), and what the line says, in its host's words,
   *  of a draft it keeps for one the host no longer draws - one a new version of its document
   *  took out or rewrote, so that a Reply on its row aims the line at other words. Such a draft
   *  has lost the way back its kept line names, so it is drawn beside the line with Discard my
   *  draft (`drop`) and, while the line writes on the issue with nothing of its own, Use it here
   *  (`take`) after `gone`, or after words saying Use it here waits for the line to be empty
   *  (`goneWaits`), as a reply kept for an entry the host no longer lists is (`listed`). With
   *  none, every statement is drawn. */
  readonly statements?: {
    readonly drawn: (statement: StatementAnchor) => boolean;
    readonly drop: string;
    readonly gone: string;
    readonly goneWaits: string;
    readonly take: string;
  };
  /** The entry the target names - the row a reply answers, the statement a comment is on, the
   *  comment an edit changes - drawn above the line while there is one. */
  readonly target?: ReactNode;
}

/** A press on a control of the issue page's box leaves the focus where it is, as a row's own
 *  controls do: on a phone the box's line takes its form from whether the box holds the focus
 *  (`pageComposer`'s `group-focus-within:`), so a press that moved the focus into the box would
 *  lay the line out again under the pointer and its click would land on nothing. The keyboard
 *  still reaches each control, and a press inside a dialog a control opens - drawn elsewhere in
 *  the document, whose events React hands to the control's own parents - is the dialog's. */
function keepFocusOnPress(event: MouseEvent<HTMLElement>): void {
  if (event.target instanceof Node && event.currentTarget.contains(event.target)) {
    event.preventDefault();
  }
}

/** Enter on the line of the issue page's box sends, as the prototype's does, and Shift+Enter
 *  breaks the line (`OutlineLook`). Enter while an input method composes is the method's
 *  (`isCompositionKey`). With the reference picker open, an Enter that reaches here is one the
 *  picker left - it takes the keys it acts on on the field's own listener (`MentionField`) - so
 *  it is the field's own newline, as in every box. */
function sendOnEnter(
  event: KeyboardEvent<HTMLTextAreaElement>,
  pickerOpen: boolean,
  send: () => void
): void {
  if (isCompositionKey(event.nativeEvent)) return;
  if (event.key !== "Enter" || event.shiftKey) return;
  if (pickerOpen) return;
  event.preventDefault();
  send();
}

/** The line above the box's line on the issue page that names its target - the entry a reply
 *  answers, the statement a comment is on, the comment an edit changes (`OutlineLook.target`) -
 *  cut to the line's width, and the control that ends the target, which `clearLabel` names and
 *  `mark` draws. */
function OutlineTargetLine({
  children,
  clearLabel,
  mark,
  onClear,
}: {
  readonly children: ReactNode;
  readonly clearLabel: string;
  readonly mark: string;
  readonly onClear: () => void;
}): ReactNode {
  return (
    <div className={pageComposer.target} data-composer-target="">
      <span className="min-w-0 truncate">{children}</span>
      <button
        aria-label={clearLabel}
        className={`${pageComposer.targetClear} ${pageComposerControl}`}
        onClick={onClear}
        onMouseDown={keepFocusOnPress}
        type="button"
      >
        <span aria-hidden="true">{mark}</span>
      </button>
    </div>
  );
}

/** A refusal on the line of the issue page's box, of a send or of a file uploading into the
 *  draft, in the page's colour, as `QueryError` draws one on every other surface, and its Retry,
 *  which `retryLabel` names (`OutlineLook.retry`), a 24 px target (R2), while a retry can get past
 *  the refusal (`onRetry`). A press anywhere on it leaves the focus where it is
 *  (`keepFocusOnPress`). */
function OutlineRefusal({
  message,
  onRetry,
  retryLabel,
  retrying,
}: {
  readonly message: string;
  /** Omitted for a refusal a retry cannot get past; no Retry is drawn then. */
  readonly onRetry?: () => void;
  readonly retryLabel: string;
  /** Holds Retry while the retried request is out. */
  readonly retrying: boolean;
}): ReactNode {
  return (
    <div
      className={`flex flex-wrap items-center gap-3 text-sm ${pageFailedText}`}
      onMouseDown={keepFocusOnPress}
      role="alert"
    >
      <p>{message}</p>
      {onRetry === undefined ? null : (
        <button
          className={`font-medium underline disabled:cursor-not-allowed disabled:opacity-50 ${pageComposerControl}`}
          disabled={retrying}
          onClick={onRetry}
          type="button"
        >
          {retryLabel}
        </button>
      )}
    </div>
  );
}

interface MentionComposerProps {
  readonly agents?: readonly Agent[];
  /** How the quote of the post a reply answers names its author: `live`, as everywhere, or
   *  `cache`, from the registry the cache already holds, which starts no agents read when the
   *  composer draws (`ReplyQuote`). */
  readonly agentNames?: AgentNames;
  readonly anchor?: ComposerAnchor;
  /** What the host says in place of the server's words when it refused this send for a mark the
   *  document no longer holds (`ANCHOR_MISSING`) and could not write that mark again: the margin's
   *  message, kept with the mark it answers, since the margin holds one such message for every
   *  compose and a kind switch moves this composer to another mark, which it says nothing about. */
  readonly anchorRefusal?: AnchorRefusal;
  /** Focuses the field once, on the first render this is true. A host that keeps the composer
   *  mounted out of sight - the margin's, under its collapsed sheet, its rail or the Pinned tab -
   *  passes it once the composer is on screen, since a field out of sight takes no focus. */
  readonly autoFocus?: boolean;
  /** Which box of the writing list this mount is: the composer draws the shared field, and the
   *  field's hint is that box's row for the write the composer is making
   *  (`DISPATCH_WRITING_BOXES`). The host names it, since one composer serves several boxes. */
  readonly box: DispatchWritingBox;
  /** The owner's issue is closed. A closed issue takes comments and message replies, so a comment
   *  composer shows and sends as ever; its Suggest and Ask kinds are hidden, since a closed issue
   *  refuses both. A suggestion or ask composer stays mounted, so a send of its own still out and
   *  its refusal keep their draft: it shows itself only while it holds one of them, with Send
   *  refused and Discard in place of Retry, and otherwise renders nothing. */
  readonly closed?: boolean;
  readonly docked?: boolean;
  /** The host offers no new message here - a decided suggestion's thread offers no reply - so the
   *  composer only finishes a send of its own: it shows itself while that send is out and for its
   *  refusal, with Send and Retry as ever, and otherwise renders nothing, its frame with it. */
  readonly finishing?: boolean;
  /** The box a host places the composer in - its border, padding and place on screen - which the
   *  composer draws around itself, so the box goes with it: a closed or finishing composer with
   *  nothing of its own to show renders neither. */
  readonly frame?: string;
  readonly edit?: { readonly body: string; readonly id: string };
  /** Where the host hands this composer a draft the reader carried from another one with that
   *  composer's own control: the phone thread composer's Cancel reply, whose draft goes to the
   *  docked composer at the issue's own address (`usePhoneThread`'s `cancelReply`). */
  readonly intake?: Ref<DraftIntake>;
  /** The channel's own mentions: seeded at mount, again by every reset (a send, Discard), and
   *  into the live draft whenever they change (`seededDraft`). */
  readonly seedMentions?: readonly { readonly target: string; readonly title: string }[];
  readonly inline?: boolean;
  readonly kind?: ComposerKind;
  /** Which replies the host lists now, and whether each one's author deleted it
   *  (`ListedReplies`): a draft the composer keeps for another reply is said to be kept with that
   *  reply's Reply as its way back only while the host lists that reply standing, and is drawn
   *  itself beside the field once the host lists it no longer or lists it deleted, with Discard
   *  my draft, and Use it here at the owner's own address (`KeptDraftOffers`). A refusal held
   *  for a reply the composer has left says so too, in place of naming that reply's Reply
   *  (`useKeptDrafts`' `writtenElsewhere`). */
  readonly listed?: ListedReplies;
  /** Names the send and the target it is held for (`useComposerSend`): a composer that mounts
   *  under a name whose send is out, or was refused, shows that send and its draft, and a host
   *  can hold its own controls - a Reply, an issue picker - while it is out (`useSending`), from
   *  Send's own task on. That decides only what the reader sees: the request is frozen when Send
   *  starts (`SentRequest`), whatever those controls do. A composer with none names its sends
   *  after its own mount. */
  readonly mutationKey?: MutationKey;
  /** What the host says in the composer, as the composer says where an upload went, drawn as the
   *  host draws it: the Conversation's note that a phone thread reply the reader cancelled over
   *  this composer's own draft is kept, and how to go on with it, or that reply itself once its
   *  comment is no longer listed, or is deleted (`usePhoneThread`'s `parked`). */
  readonly notice?: ReactNode;
  /** Ends the reply, handed the draft the field holds as the reader ends it. Cancel reply drops
   *  nothing itself: an inline reply's draft stays under its send name - typed, as a standing
   *  box's does, and refused or stopped, in its held send - so the reply composer that mounts
   *  when the thread opens again shows it, and the host decides where the reader goes. */
  readonly onCancelReply?: (draft: SentDraft) => void;
  readonly onClose: () => void;
  readonly onSent: () => void;
  /** Draws the composer as the issue page's box at the foot of its outline (`OutlineLook`), in
   *  place of its own look. A send that lands then calls `onSent` alone, since the box stays
   *  where it is and its host places the focus, and Escape ends the target or leaves the box
   *  with the draft kept. With none, the composer draws as on every other surface. */
  readonly outline?: OutlineLook;
  readonly owner: ComposerOwner;
  /** How many times the reader has moved this composer's address with a control its host draws
   *  for it - an Agents row's issue picker - counted by the host in the press that makes the
   *  move. Such a move is the reader's own, as Cancel reply is, so the draft goes with it, and
   *  its refusal and the files pasted into it with the draft. A change of `owner` or `replyTo`
   *  that comes without a count is the host's move - another row, another thread, a Reply on a
   *  message - and what the composer holds stays with the address it was made for. */
  readonly ownMoves?: number;
  /** The palette the reply's quote, the body's box with its label and the line under it, and the
   *  composer's muted lines draw in (`TONES`): `main`'s, unless the host hands the new issue
   *  page's, as Dashboard's thread items there do (dispatch://INFRA-3940 R13). */
  readonly palette?: keyof typeof TONES;
  readonly replyTo?: ReplyTarget | null;
  /** The post `replyTo` quotes, as the host's read of it stands now (`ReplyParent`), whose
   *  deletion and edit the quote marks. `replyTo` is a copy taken when Reply was pressed, which a
   *  held send and a kept draft keep in the tab across a reload (`held-sends.ts`,
   *  `unsent-drafts.ts`), so the quote gives its words only while this is here: absent - where
   *  the host's read has not answered, failed, or holds no such post - the quote names the
   *  author alone, and never gives the words of a post deleted since. */
  readonly replyParent?: ReplyParent;
  readonly saveEdit?: SaveCommentEdit;
  /** The reply target is this box's own standing address rather than a Reply the reader
   *  pressed: the Inbox reading column's thread box, which is the thread's permanent reply and
   *  is remounted whenever the column follows the selection onto another row. Taking a target
   *  normally takes the focus, so the reader can write at once; a standing box takes neither,
   *  since the reader's place is the list the column follows and the next `j` belongs to it. What
   *  the reader types into a standing box and does not send is its thread's, kept under the
   *  box's send name (`unsent-drafts.ts`), so the box drawn when the column comes back to the
   *  thread shows it again, and a newer message the host moves the box onto leaves it in the
   *  field rather than prefilling over it. Escape in its field leaves the draft there, since
   *  nothing ends that reply. */
  readonly standing?: boolean;
  /** The send of this composer's that the reader stopped, waiting aside since its host gave the
   *  composer a newer address that carries nothing - the margin's compose at other words of the
   *  document (`marginComposeKeptKey`) - as the host offers it with the presses that move it
   *  (`OfferedDraft`): the composer offers it beside its own draft, saying it may have been
   *  delivered, and Use my draft carries it here, still that send. */
  readonly stoppedAside?: OfferedDraft;
  /** Shows the Comment / Suggest / Ask switch and hands each pick to the host, which answers
   *  through `kind` - a selected document mark is the only surface where the kind can change, and
   *  its mark has to change with it (the margin retypes it) - or returns, in the reader's words,
   *  why it refused, which the composer shows under the switch. */
  readonly onKindChange?: (kind: ComposerKind) => string | undefined;
}

export function MentionComposer({
  agentNames = "live",
  agents: suppliedAgents,
  anchor,
  anchorRefusal,
  autoFocus = false,
  box,
  closed = false,
  docked = false,
  finishing = false,
  frame,
  edit,
  seedMentions = [],
  inline = false,
  intake,
  kind = "comment",
  listed,
  mutationKey,
  notice,
  onCancelReply,
  onClose,
  onKindChange,
  onSent,
  outline,
  owner,
  ownMoves = 0,
  palette = "main",
  replyParent,
  replyTo = null,
  saveEdit,
  standing = false,
  stoppedAside,
}: MentionComposerProps): ReactNode {
  // This composer's send, held by its target rather than by this mount (`useComposerSend`): a
  // composer that mounts where a send is out, or was refused, shows that send and its draft.
  // `landed` and `replaceDraft` are declared below, and run only once a send settles.
  const outgoing = useComposerSend(mutationKey, {
    landed: (sent) => landed(sent),
    refused: (held) => replaceDraft(held.draft.draft),
  });
  const registerComposer = useComposerRegistry();
  /** This composer's send by name, which is what the margin looks it up by. */
  const composerName = mutationKey === undefined ? undefined : composerNameOf(mutationKey);
  const unsent = useUnsentDrafts();
  /** The statement the issue page's box comments on: part of the address the composer shows, as
   *  its reply is. */
  const statement = outline?.anchor;
  // Only the mount reads it, so it is computed once rather than on every keystroke (`mountDraft`).
  const [initial] = useState(() =>
    mountDraft({
      anchor,
      composerName,
      edit,
      held: outgoing.held,
      inline,
      owner,
      replyTo,
      seedMentions,
      standing,
      statement,
      unsent,
    })
  );
  /** The body field and, for a suggestion, the replacement field, each reached through the
   *  handle it hands back: the text as the reader left it, and the focus. */
  const field = useRef<MentionFieldHandle | null>(null);
  const replacementField = useRef<MentionFieldHandle | null>(null);
  const formRef = useRef<HTMLFormElement>(null);
  const frameRef = useRef<HTMLDivElement>(null);
  const lastFocusedField = useRef<HTMLElement | null>(null);
  const optionLabelFields = useRef<Array<MentionFieldHandle | null>>([]);
  const focusAddedOption = useRef(false);
  const previousBody = useRef(initial.body);
  const previousReply = useRef<string | undefined>(undefined);
  /** The body the last reply prefill put in the field, which a standing box tells the reader's
   *  own words from. */
  const prefilled = useRef<string | undefined>(undefined);
  // Whether the field took a draft - a held send's, or one kept under the send name or for the
  // address - which the reply prefill must not replace: at the mount, and as the composer
  // arrives at an address that keeps one (`shown` in `useKeptDrafts`).
  const mountedHeld = useRef(initial.taken);
  // The mentions the issue page's line wrote at the front of the draft of the reply it shows,
  // which a draft kept for that reply keeps and brings back, and which the reader's own end of the
  // reply takes back out of the draft it carries (`useKeptDrafts`' `seeds`). A mount over a held
  // reply takes the ones its send keeps (`SentRequest.seeds`), so an end takes out what the mount
  // that wrote them would have, and no link the reader wrote. A held send that answers no reply -
  // one the line let go of, its mentions taken out as it went - leaves none to take out. The
  // reader's own edit that leaves the draft no longer opening with one lets it go for good
  // (`onBodyChange`), so a link the reader writes in its place is theirs.
  const replySeeds = useRef<readonly SeededMention[]>(heldSeeds(outgoing.held?.request));
  const [body, setBody] = useState(initial.body);
  const [replacement, setReplacement] = useState(initial.replacement);
  /** The edited comment's blocks as the field's edits moved them, while the blocks route has
   *  answered them; Save carries their ids (`carriedBlocks`). */
  const followedBlocks = useRef<FollowedBlocks | undefined>(undefined);
  /** Puts a whole draft in the field at once - its text and a suggestion's replacement, in step
   *  with the request that supplied them - where the reader did not type it, so their next edit
   *  is measured from here (`previousBody`). */
  const replaceDraft = useCallback((draft: Draft) => {
    setBody(draft.body);
    previousBody.current = draft.body;
    followedBlocks.current = followEdit(followedBlocks.current, draft.body, undefined, undefined);
    setReplacement(draft.replacement ?? "");
  }, []);
  const [askOptions, setAskOptions] = useState<AskOptionDraft[]>(() => askOptionRows(initial.ask));
  const [multiple, setMultiple] = useState(initial.ask?.multiple ?? false);
  const [urgency, setUrgency] = useState<AskUrgency>(initial.ask?.urgency ?? "med");
  /** Puts an ask's options and settings in the fields that edit them, as a draft the reader takes
   *  into the field keeps them (`AwayDraft.ask`), the way the mount puts a kept draft's there. */
  const replaceAsk = useCallback((ask: SentRequest["ask"]) => {
    setAskOptions(askOptionRows(ask));
    setMultiple(ask.multiple);
    setUrgency(ask.urgency);
  }, []);
  /** Whether the field's reference picker is showing, which the docked composer rises over the
   *  page for. The field tells it (`onPickerOpenChange`); the list itself is the field's. */
  const [pickerOpen, setPickerOpen] = useState(false);
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  // Why the last kind pick was refused, kept with the mark it answered: the composer stays mounted
  // when its mark changes - the margin writes it again after the server refused the send for it -
  // and a refusal about the old mark says nothing about the new one. A quote anchor (a closed
  // issue's document) has no mark.
  const [kindRefusal, setKindRefusal] = useState<{ markId: string | undefined; text: string }>();
  const live = useAgents(suppliedAgents === undefined && owner.kind !== "session", pickerOpen);
  const agents = suppliedAgents ?? live.agents;
  const text = useAgentText();
  // Only for the delivery mode each addressed session advertises (`targetOptions`): the rows a
  // pick offers are the picker's own read (`useRefPicker`).
  const options = useMemo(() => mentionOptions(agents, text), [agents, text]);
  const compact = inline || edit !== undefined;
  /** Drawn as the issue page's box at the foot of its outline (`OutlineLook`). */
  const outlined = outline !== undefined;
  /** The issue page's box's own answers, each one sentence, where its host gives them: they say
   *  what the composer's own words (`COMPOSER_WORDS`) and the parts it draws would, and what
   *  Dispatch's refusal would. */
  const answers = outline?.answers;
  /** Why Send refuses, and what a send to the mentioned sessions would do: the box's own
   *  `answers` where its host gives them, the composer's own words everywhere else. */
  const refusalWords = answers?.refusals ?? COMPOSER_WORDS.refusals;
  const deliveryWords = answers?.delivery ?? COMPOSER_WORDS.delivery;
  /** The whole draft, gone - the body, a suggestion's replacement and an ask's options - on a
   *  successful send and on Discard alike, in every host, leaving what a fresh
   *  mount shows: a host whose `onClose` only moves focus (`AgentsPage`'s rows) keeps the composer
   *  mounted, and it must read as a new one would. That includes the channel's own mentions: an
   *  issue comment an Agents row writes reaches its agent only by mentioning it, so every message
   *  after the first is seeded as the first was. The kind, the urgency and `Allow multiple` are
   *  the reader's settings, not the draft, and stay. */
  const clearDraft = () => {
    replaceDraft({ body: seededDraft(seedMentions, "", owner) });
    setAskOptions([emptyAskOption()]);
  };
  // The pills under the draft read it as a reader of the stored text will (`useDraftPills`),
  // with the record the write will have (`draftSource`). A compact box - an inline reply, an
  // edit - and the issue page's line show no pills and ask nothing for them.
  const pills = useDraftPills(compact || outlined ? "" : body, draftSource(owner, replyTo, anchor));
  const editBody = edit?.body;

  // The seed belongs to the channel the host addresses. When it changes - an Agents row's issue
  // pick, or a reply that moves the message to another channel - the live draft takes the new
  // seed in place (`seededDraft`), the way a reply edits it: a remount would part the draft from
  // this composer's own send, its refusal and its uploads. A layout effect, so no frame shows the
  // old seed under the new channel; the reply prefill, a passive effect, still lands after it.
  const seedKey = useMemo(
    () => [owner.kind, ...seedMentions.map((mention) => mention.target)].join("\n"),
    [owner.kind, seedMentions]
  );
  const seededFor = useRef(seedKey);
  // biome-ignore lint/correctness/useExhaustiveDependencies: `seedKey` is the trigger; the draft, the seed and the owner are read as they stand at the change
  useLayoutEffect(() => {
    if (seededFor.current === seedKey) return;
    seededFor.current = seedKey;
    replaceDraft({ body: seededDraft(seedMentions, body, owner) });
  }, [seedKey]);
  // The edited comment's blocks, read when the edit opens and again whenever the comment changes,
  // then followed through every edit of the field, so Save carries each block's id. A read that
  // fails (an older server has no blocks route), or that answers the comment not indexed, leaves
  // none to follow, and the save carries no ids.
  const editId = edit?.id;
  useEffect(() => {
    followedBlocks.current = undefined;
    if (editId === undefined || editBody === undefined) return;
    let current = true;
    api.commentBlocks(editId).then(
      (answer) => {
        if (!current || !answer.indexed || answer.revision === undefined) return;
        const opened = textareaValue(editBody);
        const spans = spansFromBlocks(editBody, opened, answer.blocks ?? []);
        if (spans === undefined) return;
        // What the reader typed while the read was out, measured from the body it answered.
        followedBlocks.current = followEdit(
          { revision: answer.revision, spans, value: opened },
          previousBody.current,
          undefined,
          undefined
        );
      },
      () => {}
    );
    return () => {
      current = false;
    };
  }, [editBody, editId]);
  const autoFocused = useRef(false);
  useEffect(() => {
    if (!autoFocus || autoFocused.current) return;
    autoFocused.current = true;
    field.current?.textarea()?.focus();
  }, [autoFocus]);
  useEffect(() => {
    if (focusAddedOption.current) {
      optionLabelFields.current[askOptions.length - 1]?.textarea()?.focus();
      focusAddedOption.current = false;
    }
  }, [askOptions.length]);
  useEffect(() => {
    // An open picker reads the registry again the moment it opens, inside the shell poller's
    // 15 s: a session started since the last poll is offered to the reader asking for it now.
    if (suppliedAgents === undefined && pickerOpen) void live.refetch();
  }, [live.refetch, pickerOpen, suppliedAgents]);
  /** Whether a send of this composer's holds its draft: out, or refused. */
  const holdsSend = outgoing.held !== undefined;
  useEffect(() => {
    // A mount that took a draft keeps it, and so does an arrival that put a kept draft in the
    // field (`shown` in `useKeptDrafts`); while a send holds the field's draft - out, or refused -
    // so does a reply the host starts then: the field is that send's draft, and a refusal's stays
    // at the address it lives at when the host moves the composer (`elsewhere`), which the
    // prefill of a new reply would write over. The prefill is for a new reply.
    const tookHeldDraft = mountedHeld.current;
    mountedHeld.current = false;
    if (replyTo === null) {
      previousReply.current = undefined;
      return;
    }
    if (previousReply.current === replyTo.id) return;
    const movedFrom = previousReply.current;
    previousReply.current = replyTo.id;
    // A standing box never takes the focus for its own target: nobody asked for it, and the
    // Inbox column mounts one for every row the reader steps onto.
    const takeFocus = () => {
      if (!standing) field.current?.textarea()?.focus();
    };
    if (tookHeldDraft || holdsSend) {
      takeFocus();
      return;
    }
    // A standing box's draft is its thread's (`unsentName` in `useKeptDrafts`), not one message's:
    // a newer message the host moves the box onto leaves what the reader wrote in the field, where
    // the prefill would write over it.
    const written = previousBody.current;
    if (
      standing &&
      movedFrom !== undefined &&
      written.trim() !== "" &&
      written !== prefilled.current
    ) {
      return;
    }
    const prefill = replyPrefill(replyTo);
    // The issue page's line remembers the very mentions the prefill writes (`prefillMentions`),
    // so the reader's own end of the reply takes out those and no others.
    if (outlined) replySeeds.current = prefillMentions(replyTo);
    prefilled.current = prefill.body;
    replaceDraft(prefill);
    takeFocus();
  }, [holdsSend, outlined, replaceDraft, replyTo, standing]);

  /** A send the server took, while this composer is mounted: the store has refreshed what it
   *  wrote. The draft is the composer's own, so it clears to what the channel it addresses now
   *  seeds. The issue page's line stays where it is, and its host moves the focus to what was
   *  sent, so it is not closed. */
  const landed = ({ edit: sentEdit }: SentRequest) => {
    clearDraft();
    onSent();
    if (!inline && sentEdit === undefined && !outlined) onClose();
  };
  const { refusal, sending } = outgoing;
  // The kind a closed issue refuses: a suggestion or an ask. Its composer only finishes a send of
  // its own.
  const refusedKind = closed && kind !== "comment";
  /** How many times the reader has ended the reply here (Cancel reply, or Escape): a move of the
   *  address with the composer's own control, which the draft goes with (`ownMoves`). */
  const [cancels, setCancels] = useState(0);
  /** Ends the reply, handing the host the draft as the field holds it. The draft stays where it
   *  is kept - the field, the unsent draft under the send name, the held send - so nothing the
   *  reader wrote goes with no word: the host moves the reader, and an own move carries the
   *  draft's send with it (`ownMoves`). */
  const cancelReply = () => {
    keepNothing();
    setCancels((count) => count + 1);
    onCancelReply?.({ body, replacement });
  };
  // The Escape listener below reads it as it stands, so it is not re-bound each render for it.
  const cancelReplyNow = useRef(cancelReply);
  cancelReplyNow.current = cancelReply;
  /** The issue page's line ends its target - the target line's control, and Escape - with its
   *  host's `end`, which is the line's Cancel reply: a move of the address with the composer's own
   *  control, counted as Cancel reply is, so the draft and its refusal go with it. With no target
   *  shown there is no address to move, so nothing is counted and nothing is carried onto the
   *  address the line already writes to. The line stays mounted, so nothing is handed over and
   *  nothing kept away. */
  const endTarget = () => {
    if (outline?.target !== undefined) setCancels((count) => count + 1);
    outline?.end();
  };
  // The Escape listener below reads it as it stands, as it reads `cancelReplyNow`.
  const endTargetNow = useRef(endTarget);
  endTargetNow.current = endTarget;
  /** An anchored composer's Close, and Escape once no suggestion list, picker or reply takes it:
   *  over a draft the reader has not sent - typed, handed back by a refusal, or offered beside the
   *  field (`offered`) - it raises `Discard draft?` rather than dropping what they wrote with
   *  nothing said, and it closes at once over an empty field with nothing offered. */
  const closeOrAsk = () => {
    if (
      (!hasUnsavedInput(body, replacement, askOptions) && offered === undefined) ||
      confirmingDiscard
    ) {
      onClose();
      return;
    }
    const active = document.activeElement;
    lastFocusedField.current =
      active instanceof HTMLElement && formRef.current?.contains(active) ? active : null;
    setConfirmingDiscard(true);
  };
  // The Escape listener below reads it as it stands, as it reads `cancelReplyNow`, so it is not
  // re-bound each render for it.
  const closeOrAskNow = useRef(closeOrAsk);
  closeOrAskNow.current = closeOrAsk;
  /** Adds `texts` to the end of the field - the references of files that landed into the draft it
   *  shows (`useDraftHomes`) - where the reader did not type them, so their next edit is measured
   *  from here (`previousBody`). A pick is the field's own edit, which `onBodyChange` measures. */
  const appendToField = useCallback((texts: readonly string[]) => {
    setBody((current) => {
      const next = texts.reduce((text, added) => appendToDraft(text, added), current);
      previousBody.current = next;
      return next;
    });
  }, []);
  const homes = useDraftHomes(owner, edit !== undefined, appendToField);
  const { upload, uploadOwner } = homes;
  /** The composer's own Discard as the latest render defines it (`discard`, below), which the
   *  host's handle on the field runs (`DraftIntake.drop`). */
  const discardNow = useRef(() => {});
  /** How the host lists the comment or message `reply` answers (`ListedReplies`), so a kept
   *  draft goes back by its Reply only while that post stands. */
  const listingOf = (reply: ReplyTarget): ReplyListing =>
    listed === undefined ? "standing" : (listed.get(reply.id) ?? "gone");
  const {
    awayTakes,
    changed,
    dropAway,
    dropChanged,
    dropOffered,
    elsewhere,
    forgetUnsent,
    goneAway,
    keepDraft,
    keepNothing,
    keptAway,
    offered,
    takeAway,
    takeChanged,
    takeOffered,
  } = useKeptDrafts({
    anchor,
    composerName,
    discard: discardNow,
    edit,
    field: { askOptions, body, multiple, replacement, urgency },
    homes,
    initial,
    inline,
    intake,
    kind,
    listingOf,
    mountedHeld,
    moves: ownMoves + cancels,
    outgoing,
    owner,
    replaceAsk,
    replaceDraft,
    replyTo,
    seedMentions,
    seeds: outlined ? replySeeds : undefined,
    standing,
    statement,
    stoppedAside,
    unsent,
  });
  // Closed to this kind or finishing, with no send out, no refusal to show and no draft offered
  // beside the field: nothing of this composer is on screen. A margin compose its closed issue
  // refuses stays while it offers a draft (`useMarginSheet`), and shows that draft until the
  // reader drops it.
  const dormant =
    (refusedKind || finishing) && !sending && refusal === undefined && offered === undefined;
  useEffect(() => {
    const form = formRef.current;
    if (form === null || dormant) return;
    const handleEscape = (event: globalThis.KeyboardEvent) => {
      // A key that ends an IME composition is the input method's, not the composer's
      // (`isCompositionKey`): it closes nothing, cancels no reply and asks nothing, as
      // `NameVersionDialog.tsx`'s Enter does.
      if (isCompositionKey(event)) return;
      if (event.key !== "Escape") return;
      // An open picker takes Escape on the field's own listener and stops it there, so this
      // never sees it: Escape shuts the list, and the one after it reaches the composer.
      // The issue page's line stays mounted with its draft whatever ends it, so Escape there
      // asks nothing: it ends the target as the target line's control does (`endTarget`).
      if (outlined) {
        event.preventDefault();
        event.stopPropagation();
        endTargetNow.current();
        return;
      }
      if (replyTo !== null) {
        event.preventDefault();
        cancelReplyNow.current();
        return;
      }
      event.stopPropagation();
      closeOrAskNow.current();
    };
    form.addEventListener("keydown", handleEscape);
    return () => form.removeEventListener("keydown", handleEscape);
  }, [
    // A dormant composer renders no form; the one it renders when it wakes takes the listener.
    dormant,
    outlined,
    replyTo,
  ]);
  // A composer fixed at the foot of the screen - the docked one below `sm`, or one a host's frame
  // places there - is in the foot's page layer (`hooks/useFootInset.ts` lists the foot's layers
  // and the elements in each), and the hook holds its band, its box on the screen, so the floats
  // keep clear of its controls; a refusal's row grows it upward. One with neither lays out in the
  // page.
  useFootInset(
    frame === undefined ? formRef : frameRef,
    !dormant && (docked || frame !== undefined),
    "page"
  );
  // The margin sends a refused margin compose again itself, once it has written the mark it is
  // anchored to again (`reanchor` in margin-context.tsx), and that retry carries the draft the
  // store holds. The composer's own writes of it are a keystroke's wait behind what the reader has
  // typed (`keepDraft` in `useKeptDrafts`), so the margin asks for it first: this is what answers.
  // A composer with no name of its own holds no send another surface can send again, so it
  // registers nothing.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the name's hash is its identity; the key array itself is rebuilt by some hosts on every render
  useEffect(() => {
    if (mutationKey === undefined) return;
    return registerComposer({ keepDraft, mutationKey });
  }, [composerName, registerComposer]);

  /** One edit of the body field: the text, and the edit the browser named, which moves the
   *  edited comment's blocks with it. `mapSpans` takes that range only where it explains the
   *  edit of the value the spans follow, which is the textarea's own (LF for an edited body's
   *  CR LF). It keeps of the issue page's line's mentions those that still open the draft
   *  (`openingSeeds`): one the reader's edit took out, changed or put words in front of is the
   *  line's no more, so a link the reader writes in its place - at the front, exactly as the line
   *  wrote it, included - is theirs, and the line's end of the reply leaves it. It is read at the
   *  reader's edit and never from an effect over the body, which would read a draft put in the
   *  field (`replaceDraft`) before the mentions set with it. */
  const onBodyChange = (change: MentionFieldChange) => {
    followedBlocks.current = followEdit(
      followedBlocks.current,
      change.body,
      change.range,
      change.inputType
    );
    previousBody.current = change.body;
    if (replySeeds.current.length > 0) {
      replySeeds.current = openingSeeds(change.body, replySeeds.current);
    }
    setBody(change.body);
    setConfirmingDiscard(false);
  };
  const addAskOption = () => {
    focusAddedOption.current = true;
    setAskOptions((current) => [...current, emptyAskOption()]);
    setConfirmingDiscard(false);
  };
  const updateAskOption = (index: number, field: "description" | "label", value: string) => {
    setAskOptions((current) =>
      current.map((option, currentIndex) =>
        currentIndex === index ? { ...option, [field]: value } : option
      )
    );
    setConfirmingDiscard(false);
  };
  /** The send as the composer's own state addresses and shapes it: what Send puts on the wire,
   *  and what a held send is compared with to tell an untouched draft from an edited one. */
  const composedRequest = (
    sentBody = body,
    sentReplacement = replacement
  ): Omit<SentRequest, "sendKey"> => ({
    anchor,
    ask: { multiple, options: submittedAskOptions(askOptions), urgency },
    draft: { body: sentBody, replacement: sentReplacement },
    edit:
      edit === undefined
        ? undefined
        : {
            // The body the field started from, which an editor checks before it puts this edit
            // back should it be refused (`changed`).
            base: edit.body,
            // Worked out from the body the send carries, never from another copy of the text:
            // the field holds LF where a comment stored with CR LF had CR LF, and so do the
            // followed spans.
            carried: carriedIdsOf(followedBlocks.current, sentBody),
            id: edit.id,
            save: saveEdit ?? saveCommentEdit,
          },
    kind,
    owner,
    plainDelivery: plainMode,
    replyTo,
    // The mentions the issue page's line wrote for its reply, kept with the send so a line that
    // mounts again over it takes those out and no others (`replySeeds`); never sent (`attempt`).
    ...(replySeeds.current.length === 0 ? {} : { seeds: replySeeds.current }),
    // Only the issue page's line names a statement; no other send carries the field.
    ...(statement === undefined ? {} : { statement }),
  });
  /** The send as it stands, for `sendRequest`: the fields' own text, read once, since a keystroke
   *  in Send's task may not have rendered yet and the field holds its own line endings, and every
   *  prop and setting that addresses or shapes it - or, for a Retry, what `requestFor` makes of
   *  the refused send it replaces. */
  const sentRequest = (refused: HeldSend | undefined): SentRequest =>
    requestFor(
      refused?.request,
      refused?.status === "refused" ? refused.refusal : undefined,
      composedRequest(
        field.current?.textarea()?.value ?? body,
        replacementField.current?.textarea()?.value ?? replacement
      )
    );
  /** Who this draft addresses, read from its own words (`draftParties`): the delivering
   *  parties, whose advertised modes the notices below are drawn from, and whether it names a
   *  party at all, which is what decides that a typed `/btw` is a command. */
  const parties = useMemo(() => draftParties(body), [body]);
  const effectiveTargets: readonly string[] =
    owner.kind === "session"
      ? [`session:${owner.sessionId}`]
      : replyTo?.parentKind === "message" && replyTo.thread !== undefined
        ? [replyTo.thread.target]
        : parties.targets;
  const targetOptions = effectiveTargets.flatMap((target) => {
    const option = options.find((candidate) => candidate.target === target);
    return option === undefined ? [] : [option];
  });
  // What a message with no command goes in: Send, or the mode every addressed session takes
  // when one of them refuses Send - an Aside to a session that advertises Aside alone.
  const plainMode = plainDelivery(targetOptions.map((option) => option.capabilities));
  // The mode a send this composer holds - out, or refused - went in, where pressing Send or Retry
  // now would send that very request again: the live view picks a mode outright, and the message
  // goes in it, body and all, whichever surface shows it. Every warning below is about what
  // pressing Send sends, so it reads that mode rather than this composer's own; the first edit
  // makes the message this composer's, in the mode its own targets advertise.
  const held = outgoing.held?.request;
  const heldPicked =
    held?.pickedDelivery !== undefined && unedited(held, composedRequest())
      ? held.pickedDelivery
      : undefined;
  // Whether this draft goes in that plain mode: a reply to a thread keeps the thread's mode, and
  // a picked mode is neither.
  const sendsPlain =
    heldPicked === undefined &&
    (owner.kind === "session"
      ? replyTo?.thread?.delivery === undefined
      : replyTo?.parentKind !== "message" && parties.named);
  const inheritedDelivery =
    heldPicked ??
    (owner.kind === "session"
      ? (replyTo?.thread?.delivery ?? plainMode)
      : replyTo?.parentKind === "message"
        ? replyTo.thread?.delivery
        : parties.named
          ? plainMode
          : undefined);
  // A picked mode sends the body as written: no `/btw` or `/aside` at its start is a command.
  const outbound =
    heldPicked === undefined
      ? deliveryPlan(body, inheritedDelivery)
      : { body, delivery: heldPicked };
  // A closed issue refuses a suggestion or an ask: the composer is only still here for a send of
  // its own that is out, or its refusal, and Send says why it cannot go. A post its author deleted
  // takes no reply (`409 POST_DELETED`), and no host offers a Reply on one, so a composer answers
  // one only where the delete came while it was open: Send says so, and Cancel reply takes the
  // draft, and a refusal it holds, to the composer's own address, where it goes as a message that
  // answers none. An edit waiting beside a newer body is the reader's to take back or drop before
  // anything is saved, and so is a draft a margin compose kept for other words, before anything
  // is sent. The issue page's line says both why Send refuses and where it sends the draft
  // instead in one sentence of its host's `answers`, where its host gives them.
  const deletedReply =
    replyTo === null || postDeletion(replyParent) === null
      ? undefined
      : answers !== undefined
        ? answers.deletedReply[replyTo.parentKind]
        : COMPOSER_WORDS.deletedReply[replyTo.parentKind] +
          (standing ? "" : ` ${DELETED_REPLY_WAY}`);
  // Where a refusal held for another address than the one shown was written for (`elsewhere`),
  // in the box's own sentence where its host gives one: the held send is that refusal.
  const elsewhereReason =
    elsewhere === undefined || answers === undefined || outgoing.held === undefined
      ? elsewhere
      : outlineElsewhere(outgoing.held.request, listingOf, answers.elsewhere);
  const submitReason = refusedKind
    ? "This issue is closed. Reopen it to send."
    : (deletedReply ??
      elsewhereReason ??
      (changed === undefined ? undefined : refusalWords.editChanged) ??
      (offered === undefined ? undefined : "Use your draft or discard it before you send.") ??
      draftRefusal(kind, body, replacement, outbound, refusalWords));
  const footId = useId();
  const canSubmit = canSubmitComposer(submitReason, sending, upload.pending);
  // What the issue page's line says under it of why Send refuses, before any press, in the order
  // Send says it: a reply to a post deleted since, or a refusal written for an address the line
  // has moved from.
  const outlineReason = outlined ? (deletedReply ?? elsewhereReason) : undefined;
  // Every other reason Send refuses on the issue page's line is drawn only once the reader acts:
  // a press of Send, or Enter on a draft that cannot go, puts it on the line's own reason line
  // (`pressedReasonId`), which Send names, empty until then. The press is kept with the address
  // it was made at (`Address`) - the post a reply answers, the statement a comment is on, the
  // comment an edit changes, which for the issue page's write line is the comment its host's edit
  // line changes while the write line waits under it (`OutlineLook.editing`) - and the line
  // empties as soon as Send's reason changes or goes - the reader writes, picks another kind, or
  // the draft can go - or the line moves to another address (`sameAddress`), where the reader
  // pressed nothing; the next press of a refused Send says it again.
  const pressedAt: Address = { anchor, edit: edit ?? outline?.editing, owner, replyTo, statement };
  const [pressed, setPressed] = useState<{ readonly at: Address; readonly said: string }>();
  if (
    pressed !== undefined &&
    (pressed.said !== submitReason || !sameAddress(pressed.at, pressedAt))
  ) {
    setPressed(undefined);
  }
  const pressedReason = pressed?.said;
  const pressedReasonId = useId();
  const refusedPress = () => {
    if (outlined && submitReason !== undefined && submitReason !== outlineReason) {
      setPressed({ at: pressedAt, said: submitReason });
    }
  };
  /** Sends the draft as it stands, its whole request frozen here (`sentRequest`), so nothing done
   *  after this call can change where it goes. Until the server answers, one fieldset holds every
   *  control that could change or discard the draft; the held send covers that hold in this task
   *  before React can disable the fieldset. From here on the held send keeps the draft, so the
   *  unsent copy under the send name goes. */
  const send = () =>
    outgoing.send((refused) => {
      setConfirmingDiscard(false);
      field.current?.closePicker();
      forgetUnsent();
      return sentRequest(refused);
    });
  /** The hold on the draft from Send's own task on, before React can disable the fieldset: the
   *  fieldset's capture stops every key, click, input and change bound for a control in it - so
   *  Escape raises no Discard prompt over a draft that is out. */
  const stopHeldComposerInput = (event: SyntheticEvent) => {
    if (!outgoing.sendingNow()) return;
    event.preventDefault();
    event.stopPropagation();
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (canSubmit) send();
    // Enter on the issue page's line is a press of its Send (`sendOnEnter`).
    else refusedPress();
  };
  /** Drops the draft and a refusal with it, an edit waiting beside a newer body, which is the
   *  draft the reader wrote there, and a draft offered beside the field, which Close and Escape
   *  asked over as over the field's own (`closeOrAsk`). Offered only while no send is out - the
   *  prompt never shows then (`send` closes it, and Escape raises none), and a closed composer's
   *  Discard sits beside a refusal - so the send it resets has its answer: the refusal goes with
   *  the draft it was about. A host whose `onClose` unmounts the composer draws no commit with the
   *  empty field, so nothing is kept for this address as it goes (`keepNothing`). */
  const discard = () => {
    keepNothing();
    clearDraft();
    outgoing.discard();
    dropChanged();
    dropOffered();
    setConfirmingDiscard(false);
    onClose();
  };
  discardNow.current = discard;
  const ownLabel =
    edit !== undefined
      ? "Edit comment"
      : inline
        ? "Reply"
        : kind === "ask"
          ? "Question"
          : kind === "suggestion"
            ? "Reason"
            : "Comment";
  // The issue page's line writes a comment, a question or an edit, each named in its host's words
  // where it gives them (`OutlineLook.names`).
  const hostField = outline?.names?.field;
  const bodyLabel =
    hostField === undefined
      ? ownLabel
      : edit !== undefined
        ? hostField.edit
        : kind === "ask"
          ? hostField.ask
          : hostField.comment;
  const title = edit !== undefined ? "Save" : kind === "ask" ? "Ask" : "Send";
  /** What this composer writes now, which decides which row of the writing list the field's
   *  hint comes from: the kinds the margin switches between, an edit's save, a direct message
   *  to one session, a reply inside a message thread, and a comment for everything else. */
  const writes: DispatchWriting =
    edit !== undefined
      ? "comment-edit"
      : kind === "ask"
        ? "ask"
        : kind === "suggestion"
          ? "suggestion"
          : owner.kind === "session"
            ? "agent-message"
            : replyTo?.parentKind === "message"
              ? "message-reply"
              : "comment";
  /** The issue the picker offers the people and the stopped roles of; a document of its own
   *  project has none, and the picker offers everyone who has signed in there. */
  const pickerIssue = owner.kind === "issue" ? owner.issueKey : undefined;
  const pickerProject = owner.kind === "artifact" ? owner.project : undefined;
  const outboundMode = outbound.delivery;
  const unsupportedOptions =
    outboundMode === undefined
      ? []
      : targetOptions.filter((option) => !option.capabilities.includes(outboundMode));
  const unsupported = unsupportedOptions.length > 0;
  // Sessions whose lists hold no delivery mode take no message in any mode: a session whose plugin
  // cannot reach its conversation says so by advertising none, and one listing only capabilities
  // that are no mode (`deliveryModesOf`) takes none either.
  const deaf =
    unsupported &&
    unsupportedOptions.every((option) => deliveryModesOf(option.capabilities).length === 0);
  // The plain mode, named whenever it is not Send, so the reader knows what their words become.
  const plainNotice =
    sendsPlain && plainMode !== "steer" && outboundMode === plainMode && outbound.body === body
      ? // A session that advertises no mode gets its own warning, not this line.
        targetOptions.filter(
          (option) =>
            deliveryModesOf(option.capabilities).length > 0 &&
            !option.capabilities.includes("steer")
        )
      : [];
  // The prefixed modes a person could send in instead: only one every target refusing this mode
  // advertises, so the warning never suggests a mode those sessions refuse too (an aside-only
  // session is offered /aside, never /btw).
  const alternatives = (["btw", "aside"] as const).filter(
    (mode) =>
      unsupported &&
      mode !== outboundMode &&
      unsupportedOptions.every((option) => option.capabilities.includes(mode))
  );

  const tones = TONES[palette];
  // The theme's muted token, shadowed in this function by the palette's, so `main`'s lines that
  // name it stay as they are (spec R26): on `main`'s palette it is the token itself (`TONES.main`
  // reads the import, at module scope), and on the new page's every muted line of the composer -
  // the line under Send, the paste line, the status lines, Cancel reply's × - is the page's quiet
  // grey. Nothing in this function names it above this line.
  const textMutedOnSurfaceMuted = tones.foot;
  // The parts both looks draw alike, each where its look puts it: the prompt that asks before a
  // draft is thrown away, the drafts kept beside the field, what a send to the mentioned sessions
  // would do, and the upload and the send's own state.
  const discardPrompt = confirmingDiscard ? (
    <div
      className={`flex items-center gap-3 rounded-lg border p-2 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
      role="alert"
    >
      <span>Discard draft?</span>
      <button className="font-semibold underline" onClick={discard} type="button">
        Discard
      </button>
      <button
        className="underline"
        onClick={() => {
          setConfirmingDiscard(false);
          lastFocusedField.current?.focus();
        }}
        type="button"
      >
        Keep
      </button>
    </div>
  ) : null;
  // A kind the closed issue refuses can send nothing, and a margin compose of that kind ends once
  // it offers nothing (`useMarginSheet`), so a draft taken into its field would go with no word:
  // it offers Discard my draft alone.
  const keptOffers = (
    <KeptDraftOffers
      changed={changed}
      gone={goneAway}
      goneTakes={awayTakes}
      kind={kind}
      offered={offered}
      onDiscardChanged={discard}
      onDropGone={dropAway}
      onDropOffered={dropOffered}
      onTakeChanged={takeChanged}
      onTakeGone={takeAway}
      onTakeOffered={refusedKind ? undefined : takeOffered}
      words={
        answers === undefined
          ? undefined
          : { changedLead: answers.changedEdit, goneReply: answers.goneReply }
      }
    />
  );
  // A draft the issue page's line keeps for a statement its host no longer draws
  // (`OutlineLook.statements`): a Reply on that row aims the line at other words now, so the draft
  // waits beside the line in the host's words, as a reply kept for an entry the host no longer
  // lists does (`KeptDraftOffers`), with Discard my draft and, where the line writes on the issue
  // with nothing of its own, Use it here, which puts it in the field as a comment on the issue.
  // It answers no reply, so the drafts named with a kept line (`keptAway`) hold it, and it is
  // taken out of them here. Each is keyed by its whole address (`awayKey`): two kept on one row -
  // its words before a new version and after - are two drafts.
  const statementLook = outline?.statements;
  const statementGone = (away: AwayDraft): boolean =>
    away.statement !== undefined && statementLook?.drawn(away.statement) === false;
  const goneStatements: AwayDraft[] = [];
  const keptWithLine: AwayDraft[] = [];
  for (const away of keptAway) {
    if (statementGone(away)) goneStatements.push(away);
    else keptWithLine.push(away);
  }
  const goneStatementOffers =
    statementLook === undefined
      ? null
      : goneStatements.map((away) => (
          <DraftOffer
            body={away.draft.body}
            drop={{ label: statementLook.drop, press: () => dropAway(away) }}
            key={awayKey(away)}
            lead={awayTakes === "once empty" ? statementLook.goneWaits : statementLook.gone}
            replacement={away.draft.replacement}
            take={
              awayTakes === "here"
                ? { label: statementLook.take, press: () => takeAway(away) }
                : undefined
            }
          />
        ));
  // The colours the send's own lines are drawn in: the page's on the issue page's box, whose
  // whole look is the page's own, and the dashboard's on every other surface.
  const mutedTone = outlined ? pageMutedText : textMutedOnSurface;
  const quietTone = outlined ? pageMutedText : textMutedOnSurfaceMuted;
  const failedTone = outlined ? pageFailedText : dangerText;
  // The sessions a notice names, by their titles.
  const titlesOf = (named: readonly { readonly title: string }[]) =>
    named.map((option) => option.title).join(TITLES_JOINER);
  const { plain: plainWords, unsupported: unsupportedWords } = deliveryWords;
  const instead = unsupportedWords.instead;
  // What sessions that advertise no delivery mode do with a send, and why they take none: the
  // composer's own warning and then a sentence for each cause (`noDeliveryReasons`), or, on the
  // issue page's box, the one sentence its host gives for the cause (`outlineDeafMessage`).
  const deliveryNotices = (
    <>
      {plainNotice.length > 0 ? (
        <p className={`text-sm ${mutedTone}`}>
          {plainWords.before}
          {MODE_LABELS[plainMode]}
          {plainWords.between}
          {titlesOf(plainNotice)}
          {plainWords.after[oneOrMany(plainNotice)]}
        </p>
      ) : null}
      {deaf ? (
        <p className={`text-sm ${failedTone}`}>
          {titlesOf(unsupportedOptions)}
          {answers === undefined ? (
            <>
              {COMPOSER_WORDS.delivery.deaf[oneOrMany(unsupportedOptions)]}
              {noDeliveryReasons(unsupportedOptions)}
            </>
          ) : (
            outlineDeafMessage(unsupportedOptions, answers.delivery.deaf)
          )}
        </p>
      ) : unsupported ? (
        <p className={`text-sm ${failedTone}`}>
          {titlesOf(unsupportedOptions)}
          {unsupportedWords.before[oneOrMany(unsupportedOptions)]}
          {outboundMode === undefined ? null : MODE_LABELS[outboundMode]}
          {/* The box's own words say the modes the sessions take instead in the warning's one
              sentence, which ends there; every other surface says them after it. */}
          {answers !== undefined && alternatives.length > 0 ? null : unsupportedWords.after}
          {alternatives.length === 0
            ? null
            : `${instead.before}${alternatives.map((mode) => DELIVERY_COMMANDS[mode]).join(instead.or)}${instead.between}${alternatives
                .map((mode) => `${instead.article[mode]}${MODE_LABELS[mode]}`)
                .join(instead.or)}${instead.after}`}
        </p>
      ) : null}
    </>
  );
  // A comment on a statement Dispatch refused because the statement's words stand twice in its
  // document: the host's words for it (`OutlineLook.statementTwice`), and no Retry, which would
  // send the same words to the same refusal.
  const statementTwice =
    refusal instanceof ApiError && refusal.status === 409 && refusal.code === "TARGET_AMBIGUOUS"
      ? outline?.statementTwice
      : undefined;
  // Retry sends the draft as it stands, so it asks what Send asks of it.
  const retry = canSubmit && statementTwice === undefined ? send : undefined;
  // A file pasted or dropped into the draft, as its upload stands: on the issue page's box
  // `DraftUploadStatus`'s lines in the page's own words and colours (`OutlineLook`), a refused
  // upload's drawn as the box's other refusals are (`OutlineRefusal`); `DraftUploadStatus` itself
  // on every other surface. An upload refused for another owner is that owner's: its refusal
  // shows in words of its own, with no Retry (`pastedElsewhere`), and the status shows the rest,
  // with a line for the files that landed into a draft living at another owner.
  const uploadStatus = (
    <>
      {uploadOwner === undefined ? null : outlined ? (
        <>
          {upload.pending > 0 ? (
            <p className={`text-sm ${pageMutedText}`} role="status">
              {outline.uploading}
            </p>
          ) : null}
          {homes.pastedElsewhere === undefined && upload.isError ? (
            <OutlineRefusal
              message={
                answers === undefined
                  ? (stoppedNote(
                      upload,
                      COMPOSER_WORDS.stoppedUpload.what,
                      COMPOSER_WORDS.stoppedUpload.made
                    ) ?? uploadErrorMessage(upload.error))
                  : outlineUploadMessage(upload, answers.upload)
              }
              onRetry={upload.retry}
              retryLabel={outline.retry}
              retrying={upload.isPending}
            />
          ) : null}
          {upload.madeUnanswered === null ? null : (
            <p
              className={`text-sm ${pageMutedText}`}
              data-testid="landed-without-answer"
              role="status"
            >
              {outline.uploadedUnanswered}
            </p>
          )}
        </>
      ) : (
        <DraftUploadStatus showRefusal={homes.pastedElsewhere === undefined} upload={upload} />
      )}
      {homes.pastedElsewhere === undefined ? null : outlined ? (
        <OutlineRefusal
          message={homes.pastedElsewhere}
          retryLabel={outline.retry}
          retrying={false}
        />
      ) : (
        <QueryError message={homes.pastedElsewhere} />
      )}
      {homes.landedElsewhere.map(({ key, text }) => (
        <p className={`text-sm ${quietTone}`} key={key} role="status">
          {text}
        </p>
      ))}
    </>
  );
  const sendState = (
    <>
      {uploadStatus}
      {notice}
      {/* A draft kept for another reply, or for none, while the composer answers this one: the
          line says whose it is and the way back, so the host's move takes nothing with no word.
          The issue page's line says what it keeps for no reply in its host's words, which name
          the way back: the target line's control for the issue, a statement's Reply for that
          statement (`OutlineLook.kept`). Each line is keyed by its draft's whole address
          (`awayKey`). */}
      {keptWithLine.map((away) => (
        <p className={`text-sm ${quietTone}`} key={awayKey(away)} role="status">
          {!outlined || away.replyTo !== null || outline.kept === undefined
            ? keptAwayLine(away, answers?.keptReply)
            : away.statement === undefined
              ? outline.kept.issue
              : outline.kept.statement}
        </p>
      ))}
      {/* The issue page's line draws no line under Send, so why Send refuses a reply to a post
          deleted since, or where a refusal the line has moved away from was written for and the
          way back there, is said here, and Send's refusal names this line (`footId`). */}
      {outlineReason === undefined ? null : (
        <p className={`text-sm ${quietTone}`} id={footId} role="status">
          {outlineReason}
        </p>
      )}
      {refusal === undefined ? null : (
        <div className="flex flex-wrap items-center gap-3">
          {outlined ? (
            <OutlineRefusal
              message={
                statementTwice ??
                (answers === undefined
                  ? refusalMessage(refusal, anchor, anchorRefusal)
                  : outlineRefusalMessage(refusal, answers))
              }
              onRetry={retry}
              retryLabel={outline.retry}
              retrying={sending}
            />
          ) : (
            <QueryError
              message={refusalMessage(refusal, anchor, anchorRefusal)}
              onRetry={retry}
              retrying={sending}
            />
          )}
          {/* A kind the closed issue refuses takes no Retry, and neither does a draft written for
              another address than the composer shows, or a reply to a post deleted since, so the
              refusal's own way out is to drop the draft it handed back, once the reader has it; a
              send the reader stopped offers both ways, and so does an inline reply's refusal,
              which its Cancel reply keeps for the thread rather than dropping. */}
          {refusedKind ||
          elsewhere !== undefined ||
          deletedReply !== undefined ||
          refusal instanceof SendStoppedError ||
          inline ? (
            <button
              className={
                outlined
                  ? `text-sm font-medium underline ${pageFailedText} ${pageComposerControl}`
                  : `text-sm font-medium underline ${dangerText}`
              }
              onClick={discard}
              // A press on the issue page's box leaves the focus where it is, as one on the
              // refusal beside it does (`keepFocusOnPress`).
              onMouseDown={outlined ? keepFocusOnPress : undefined}
              type="button"
            >
              {outline?.discardDraft ?? DISCARD_DRAFT_LABEL}
            </button>
          ) : null}
        </div>
      )}
    </>
  );
  // Send's busy word: the page's own in the issue page's look (`OutlineLook`), the composer's
  // everywhere else.
  const sendBusy = sending
    ? (outline?.sending ?? "Sending…")
    : upload.pending > 0
      ? (outline?.uploading ?? "Uploading file…")
      : undefined;
  // What both looks hand their held fieldset alike: the hold on every control of the draft while
  // a send is out (`stopHeldComposerInput`).
  const heldFieldset = {
    disabled: sending,
    onChangeCapture: stopHeldComposerInput,
    onClickCapture: stopHeldComposerInput,
    onInputCapture: stopHeldComposerInput,
    onKeyDownCapture: stopHeldComposerInput,
  };
  // The issue page's line reads each party its write is delivered to by its name, where its host
  // names the list (`OutlineNames.targets`): a role by its own name, and a session by its name as
  // the page draws it (`sessionShown`) - the title the picker's options hold it under
  // (`sessionTitle`), and for one the registry does not list now, the page's word for an agent
  // nothing names - never by its short address, which names nobody.
  const targetName = (target: string): string => {
    const party = partyOfAddress(target);
    if (party?.kind === "role") return party.role;
    if (party?.kind !== "agent-session") return target;
    const option = options.find((candidate) => candidate.target === target);
    return text.sessionShown(party.session, option?.sessionTitle);
  };
  // The issue page's line at the foot of its outline (`OutlineLook`): the entry its target names
  // and the control that ends the target above it, then the line itself - the field, the kinds it
  // writes and Send, in the order Tab reaches them - then whatever answers the reader's act. A
  // question it writes takes the ask's defaults - no options, the middle urgency - as the
  // prototype's Question chip does.
  const outlineControls = outlined ? (
    <fieldset className="flex min-w-0 flex-col gap-[3px] border-0 p-0" {...heldFieldset}>
      {outline.target === undefined ? null : (
        // The target is part of the message's address, fixed once the send is out, so the held
        // fieldset holds its control with every other draft control.
        <OutlineTargetLine
          clearLabel={outline.cancelReply}
          mark={outline.endMark}
          onClear={endTarget}
        >
          {outline.target}
        </OutlineTargetLine>
      )}
      {discardPrompt}
      {/* The line wraps only for what asks a row of its own: the open picker's list, and on a
          phone the kinds and Send under the field while it holds the caret. */}
      <div className={`${pageComposer.line} flex-wrap`}>
        <MentionField
          body={body}
          description={outline.names?.description}
          fieldRef={field}
          issueKey={pickerIssue}
          label={bodyLabel}
          labelHidden
          look={pageComposer.field}
          onChange={onBodyChange}
          onDrop={homes.drop}
          onKeyDown={(event: KeyboardEvent<HTMLTextAreaElement>) =>
            // A draft whose file is still uploading sends nothing.
            sendOnEnter(event, pickerOpen, () => {
              if (upload.pending === 0) event.currentTarget.form?.requestSubmit();
            })
          }
          onPaste={homes.paste}
          onPickerOpenChange={setPickerOpen}
          pickerWords={outline.picker}
          placeholder={outline.placeholder}
          project={pickerProject}
          readOnly={changed !== undefined || offered !== undefined}
          targets={
            outline.names === undefined
              ? undefined
              : { label: outline.names.targets, nameOf: targetName }
          }
          writing={dispatchWritingBox(box, writes)}
        />
        {/* A press here leaves the focus where it is (`keepFocusOnPress`), so on a phone the
            line keeps its form under the pointer and the press lands where it was aimed. */}
        {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handler only keeps a press on the buttons inside from moving the focus, and each of them is its own control. */}
        <div className={pageComposer.controls} onMouseDown={keepFocusOnPress}>
          {outline.kinds.map((choice) => (
            <button
              aria-pressed={kind === choice.kind}
              className={`${pageComposer.chip} ${pageComposerControl} ${
                kind === choice.kind
                  ? pageComposer.chipOn[choice.kind]
                  : pageComposer.chipOff[choice.kind]
              }`}
              data-composer-kind={choice.kind}
              key={choice.kind}
              onClick={() => onKindChange?.(choice.kind)}
              type="button"
            >
              {choice.label}
            </button>
          ))}
          <RefusableButton
            busy={sendBusy}
            look="page-send"
            onRefusedPress={refusedPress}
            refusal={submitReason ?? null}
            refusalShownBy={
              outlineReason !== undefined && submitReason === outlineReason
                ? footId
                : pressedReasonId
            }
            type="submit"
          >
            {outline.send}
          </RefusableButton>
        </div>
      </div>
      {/* Why Send refuses, once the reader has pressed it (`pressedReason`), on a line Send names
          as its description: empty, and off screen, until that press. */}
      <p
        className={pressedReason === undefined ? "sr-only" : `text-sm ${quietTone}`}
        id={pressedReasonId}
        role="status"
      >
        {pressedReason}
      </p>
      {keptOffers}
      {goneStatementOffers}
      {deliveryNotices}
      {sendState}
    </fieldset>
  ) : null;
  const composerControls = outlined ? null : (
    <fieldset
      className={
        compact
          ? "min-w-0 space-y-2 border-0 p-0"
          : docked
            ? "min-w-0 border-0 p-0"
            : "min-w-0 space-y-3 border-0 p-0"
      }
      {...heldFieldset}
    >
      {/* The title repeats the submit button two rows below it, so the row exists only where it
          also carries Close - an anchored composer floating over a quote, where naming what is
          being written is worth a line. */}
      {compact || anchor === undefined ? null : (
        <div className="flex items-center justify-between gap-3">
          <p className={`text-sm font-semibold ${textPrimaryOnSurface}`}>{title}</p>
          <button className={`text-sm ${dismissButtonText}`} onClick={closeOrAsk} type="button">
            Close
          </button>
        </div>
      )}
      {replyTo === null ? null : (
        <div className="flex items-center gap-1">
          <ReplyQuote
            agentNames={agentNames}
            author={replyTo.actor}
            className="min-w-0 flex-1"
            deleted={postDeletion(replyParent)}
            edited={replyParent?.edited_at}
            excerpt={replyParent === undefined ? "" : replyTo.excerpt}
            excerptTree={replyParent === undefined ? undefined : replyTo.excerptTree}
            palette={palette}
            source={textSourceOf(
              replyTo.parentKind,
              replyTo.id,
              owner.kind === "issue" ? owner.issueKey : undefined
            )}
            to={replyTo.to}
          />
          {/* The reply is part of the message's address, fixed once the send is out, so the
              composer's control fieldset holds it with every other draft control. A standing
              box's reply is the box's own address, which nothing ends, so it offers no Cancel
              reply: the control would do nothing there, and the draft stays in the field. */}
          {standing ? null : (
            <button
              aria-label="Cancel reply"
              className={`inline-flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg text-lg leading-none disabled:cursor-not-allowed disabled:opacity-50 md:min-h-8 md:min-w-8 ${textMutedOnSurfaceMuted}`}
              onClick={cancelReply}
              type="button"
            >
              ×
            </button>
          )}
        </div>
      )}
      {compact || anchor === undefined ? null : (
        <blockquote className={`border-l-2 pl-2 text-sm ${quoteAccentBorder} ${quoteBodyText}`}>
          {/* The document's own words, as they are: the reading list's `anchor-quote`. */}
          {anchor.quote}
        </blockquote>
      )}
      {onKindChange !== undefined && edit === undefined && owner.kind !== "session" && !closed ? (
        <>
          <fieldset className="flex gap-1">
            <legend className="sr-only">Kind</legend>
            {(["comment", "suggestion", "ask"] as const).map((next) => (
              <button
                aria-pressed={kind === next}
                className={`min-h-11 rounded-lg px-3 text-sm ${kind === next ? primaryButtonBg : textSecondaryOnSurface}`}
                key={next}
                onClick={() => {
                  const refused = onKindChange(next);
                  setKindRefusal(
                    refused === undefined || anchor === undefined
                      ? undefined
                      : { markId: anchor.mark_id, text: refused }
                  );
                }}
                type="button"
              >
                {next === "comment" ? "Comment" : next === "suggestion" ? "Suggest" : "Ask"}
              </button>
            ))}
          </fieldset>
          {/* Why the kind did not change, where the reader pressed; a switch that takes clears it,
              and a newer anchor leaves it behind. */}
          {kindRefusal === undefined || kindRefusal.markId !== anchor?.mark_id ? null : (
            <p className={`text-sm ${dangerText}`} role="status">
              {/* not stored prose: the host's own words for why the kind did not change */}
              {kindRefusal.text}
            </p>
          )}
        </>
      ) : null}
      {discardPrompt}
      {kind === "suggestion" ? (
        <MentionField
          body={replacement}
          fieldRef={replacementField}
          issueKey={pickerIssue}
          label="Replacement"
          onChange={(change) => {
            setReplacement(change.body);
            setConfirmingDiscard(false);
          }}
          onKeyDown={submitOnModifiedEnter}
          project={pickerProject}
          readOnly={offered !== undefined}
          writing={dispatchWritingBox("suggestion-replacement", "suggestion")}
        />
      ) : null}
      <MentionField
        body={body}
        fieldClassName={tones.field}
        fieldRef={field}
        hintClassName={tones.hint}
        inline={inline}
        issueKey={pickerIssue}
        label={bodyLabel}
        labelClassName={tones.label}
        labelHidden={compact}
        onChange={onBodyChange}
        onDrop={homes.drop}
        onKeyDown={(event: KeyboardEvent<HTMLTextAreaElement>) =>
          submitOnModifiedEnter(event, { disabled: upload.pending > 0 })
        }
        onPaste={homes.paste}
        onPickerOpenChange={setPickerOpen}
        project={pickerProject}
        readOnly={changed !== undefined || offered !== undefined}
        writing={dispatchWritingBox(box, writes)}
      />
      {keptOffers}
      {deliveryNotices}
      {/* An ask's options and settings are its draft's as much as its question is, and taking an
          offered draft puts its own back (`takeOffered`), so they wait with the read-only
          question while the offer is out, and nothing the reader sets here is written over. */}
      {kind === "ask" ? (
        <fieldset className="min-w-0 space-y-3 border-0 p-0" disabled={offered !== undefined}>
          <fieldset>
            <legend className={`text-sm font-medium ${textSecondaryOnSurface}`}>Urgency</legend>
            <div className="mt-1 flex flex-wrap gap-1">
              {ASK_URGENCIES_ASCENDING.map((value) => (
                <Chip
                  key={value}
                  onClick={() => setUrgency(value)}
                  selected={urgency === value}
                  tone={value}
                >
                  {URGENCY_LABELS[value]}
                </Chip>
              ))}
            </div>
          </fieldset>
          <label
            className={`flex items-center gap-2 text-sm font-medium ${textSecondaryOnSurface}`}
          >
            <input
              checked={multiple}
              className={`size-4 rounded ${borderStrong} ${checkboxAccent} ${focusRing}`}
              onChange={(event) => setMultiple(event.target.checked)}
              type="checkbox"
            />
            Allow multiple
          </label>
          <section aria-label="Options" className="space-y-2">
            <div className="flex items-center justify-between gap-3">
              <p className={`text-sm font-medium ${textSecondaryOnSurface}`}>Options</p>
              <button
                className={`text-sm font-medium ${linkText} ${linkHoverText}`}
                onClick={addAskOption}
                type="button"
              >
                Add option
              </button>
            </div>
            {askOptions.map((option, index) => (
              <div className="grid grid-cols-[minmax(0,1fr)_auto] gap-2" key={option.id}>
                <div className="space-y-2">
                  <MentionField
                    body={option.label}
                    fieldRef={(handle) => {
                      optionLabelFields.current[index] = handle;
                    }}
                    inline
                    issueKey={pickerIssue}
                    label={`Option ${index + 1} label`}
                    labelHidden
                    onChange={(change) => updateAskOption(index, "label", change.body)}
                    onKeyDown={(event) => {
                      // An Enter that ends an IME composition commits the reader's words and
                      // adds no option.
                      if (isCompositionKey(event.nativeEvent)) return;
                      // With the picker open its own listener took this Enter and stopped it
                      // before this ran: the key takes the row under it rather than adding an
                      // option nobody asked for.
                      if (event.key === "Enter") {
                        event.preventDefault();
                        addAskOption();
                      }
                    }}
                    placeholder="Label"
                    project={pickerProject}
                    writing={dispatchWritingBox("ask-option-label", "ask")}
                  />
                  <MentionField
                    body={option.description}
                    inline
                    issueKey={pickerIssue}
                    label={`Option ${index + 1} description`}
                    labelHidden
                    onChange={(change) => updateAskOption(index, "description", change.body)}
                    placeholder="Description (optional)"
                    project={pickerProject}
                    writing={dispatchWritingBox("ask-option-description", "ask")}
                  />
                </div>
                <button
                  aria-label={`Remove option ${index + 1}`}
                  className={`self-start rounded-lg border px-3 py-2 text-sm font-medium ${borderStrong} ${secondaryButtonText} ${dangerHoverBorder} ${hoverToDangerText}`}
                  onClick={() =>
                    setAskOptions((current) =>
                      current.filter((_option, currentIndex) => currentIndex !== index)
                    )
                  }
                  type="button"
                >
                  Remove
                </button>
              </div>
            ))}
          </section>
        </fieldset>
      ) : null}
      {compact ? null : (
        <>
          <p className={`text-xs ${textMutedOnSurfaceMuted}`}>
            Paste or drop a picture to show it inline, or any other file to attach it.
          </p>
          <DraftPills pills={pills} />
        </>
      )}
      {sendState}
      <RefusableButton
        busy={sendBusy}
        refusal={submitReason ?? null}
        refusalShownBy={footId}
        type="submit"
      >
        {title}
      </RefusableButton>
      {/* While Send refuses, the line under it says why - on screen, for a reader with no
          pointer to hover it - and once the draft can go, how to send it. */}
      <p className={`text-xs ${textMutedOnSurfaceMuted}`} id={footId}>
        {submitReason ?? "Ctrl/Cmd+Enter to send · Enter for a new line"}
      </p>
    </fieldset>
  );
  // A composer closed to its kind, or a finishing one, stays mounted, keeping its draft, and shows
  // itself only for a send of its own that is out or that send's refusal; otherwise it renders
  // nothing, its frame with it.
  if (dormant) return null;
  // Past its deadline a send is still the server's: no Retry, which would post it twice. It is
  // still being tried under its key, so it says so - in the page's muted words on the issue
  // page's box, whose Stop trying `RefusableButton` draws in the page's look - and offers Stop
  // trying, which sits outside the held fieldset, the one control a send that is out leaves the
  // reader.
  const heldLate = outgoing.held?.status === "late" ? outgoing.held : undefined;
  const lateStatus =
    heldLate === undefined ? null : (
      <div className="flex flex-wrap items-center gap-3">
        <p className={`text-sm ${quietTone}`} role="status">
          {answers?.late ?? COMPOSER_WORDS.late}
        </p>
        <StopTrying onStop={outgoing.stop} stopping={heldLate.stopping} />
      </div>
    );
  // The form's own look on every surface but the issue page's: a compact box, the docked one at
  // the screen's foot below `sm`, or the callout every other host draws.
  const composerFormClass = compact
    ? "space-y-2"
    : docked
      ? `${pickerOpen ? "z-20" : "z-[8]"} fixed inset-x-0 bottom-16 space-y-2 border-t px-4 py-2 sm:sticky sm:top-0 sm:z-20 sm:-mx-2 sm:border-b sm:px-2 ${borderDefault} ${surfaceBg}`
      : `space-y-3 rounded-xl border p-3 ${calloutInfoBorder} ${calloutInfoBg}`;
  const form = (
    <form
      aria-label={outline?.names?.form ?? "Comment composer"}
      className={outlined ? pageComposer.box : composerFormClass}
      data-issue-composer={outlined ? "" : undefined}
      onSubmit={submit}
      ref={formRef}
    >
      {outlineControls ?? composerControls}
      {lateStatus}
    </form>
  );
  return frame === undefined ? (
    form
  ) : (
    <div className={frame} ref={frameRef}>
      {form}
    </div>
  );
}
