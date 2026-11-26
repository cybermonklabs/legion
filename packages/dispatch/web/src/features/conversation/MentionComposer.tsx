import {
  type DispatchWriting,
  type DispatchWritingBox,
  dispatchWritingBox,
} from "@legion/contracts/dispatch-writing-boxes";
import type { MutationKey } from "@tanstack/react-query";
import {
  type FormEvent,
  type KeyboardEvent,
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
import { inputMethodKey } from "../../lib/input-method-key";
import {
  calloutInfoBg,
  calloutInfoBorder,
  checkboxAccent,
  dangerHoverBorder,
  focusRing,
  hoverToDangerText,
  quoteAccentBorder,
  quoteBodyText,
  textPrimaryOnSurface,
} from "../../theme/classes";
import {
  borderDefault,
  borderStrong,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  dangerText,
  dismissButtonText,
  linkHoverText,
  linkText,
  primaryButtonBg,
  secondaryButtonText,
  surfaceBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../../theme/shell-classes";
import { appendToDraft, DraftUploadStatus } from "../artifacts/ArtifactUpload";
import { ASK_URGENCIES_ASCENDING, URGENCY_LABELS } from "../inbox/ask-urgency";
import { useComposerRegistry } from "../margin/margin-context";
import { useAgentText } from "../refs/AgentName";
import { mentionOptions } from "../refs/party-options";
import { draftParties, outgoingDraft } from "../refs/party-references";
import { postDeletion } from "../refs/post-marks";
import { textSourceOf } from "../refs/text-references";
import { SendStoppedError } from "../writes/keyed-write";
import { StopTrying } from "../writes/StopTrying";
import {
  carriedIdsOf,
  type FollowedBlocks,
  followEdit,
  spansFromBlocks,
  textareaValue,
} from "./block-spans";
import {
  type AskOptionDraft,
  type Draft,
  emptyAskOption,
  replyPrefill,
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
import { composerNameOf } from "./composer-names";
import { deliveryModesOf, MODE_LABELS, noDeliveryReasons, plainDelivery } from "./delivery";
import { DraftPills, draftSource, useDraftPills } from "./draft-pills";
import type { HeldSend } from "./held-sends";
import { KeptDraftOffers } from "./KeptDraftOffers";
import { keptAwayLine } from "./KeptReply";
import { MentionField, type MentionFieldChange, type MentionFieldHandle } from "./MentionField";
import { ReplyQuote } from "./ReplyQuote";
import {
  DELIVERY_COMMANDS,
  type DeliveryPlan,
  deliveryPlan,
  KEYED_SEND_LATE_TEXT,
  requestFor,
  type SaveCommentEdit,
  type SentDraft,
  type SentRequest,
  saveCommentEdit,
  unedited,
} from "./send-request";
import { useUnsentDrafts } from "./unsent-drafts";
import { useAgents } from "./useAgents";
import { useComposerSend } from "./useComposerSend";
import { useDraftHomes } from "./useDraftHomes";
import { mountDraft, type OfferedDraft, useKeptDrafts } from "./useKeptDrafts";

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
 * and Send is still dead. A save or an upload in flight names itself on the button instead.
 */
function draftRefusal(
  kind: ComposerKind,
  body: string,
  replacement: string,
  outbound: DeliveryPlan
): string | undefined {
  if (kind === "suggestion") {
    return replacement.trim() === "" ? "Type the replacement text first." : undefined;
  }
  if (body.trim() === "")
    return kind === "ask" ? "Type the question first." : "Type a message first.";
  if (
    kind === "comment" &&
    outbound.delivery !== undefined &&
    outbound.delivery !== "steer" &&
    outbound.body.trim() === ""
  ) {
    return `Type the message after ${DELIVERY_COMMANDS[outbound.delivery]}.`;
  }
  return undefined;
}

/** What the composer shows for `refusal`: the host's own words, where it has some about the mark
 *  this send names - the margin's, when the server refused the send for a mark it cannot see and
 *  the margin could not write that mark again - else the words of a refusal written for the
 *  reader (the server's missing anchor, a send they stopped, a text past a bound Dispatch holds
 *  it to, a text holding a decision block), else the page's wording around what went wrong. */
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
    : `Couldn't send — ${apiErrorMessage(refusal, "network error")}`;
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

interface MentionComposerProps {
  readonly agents?: readonly Agent[];
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
  readonly owner: ComposerOwner;
  /** How many times the reader has moved this composer's address with a control its host draws
   *  for it - an Agents row's issue picker - counted by the host in the press that makes the
   *  move. Such a move is the reader's own, as Cancel reply is, so the draft goes with it, and
   *  its refusal and the files pasted into it with the draft. A change of `owner` or `replyTo`
   *  that comes without a count is the host's move - another row, another thread, a Reply on a
   *  message - and what the composer holds stays with the address it was made for. */
  readonly ownMoves?: number;
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
  owner,
  ownMoves = 0,
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
  // edit - shows no pills and asks nothing for them.
  const pills = useDraftPills(compact ? "" : body, draftSource(owner, replyTo, anchor));
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
    prefilled.current = prefill.body;
    replaceDraft(prefill);
    takeFocus();
  }, [holdsSend, replaceDraft, replyTo, standing]);

  /** A send the server took, while this composer is mounted: the store has refreshed what it
   *  wrote. The draft is the composer's own, so it clears to what the channel it addresses now
   *  seeds. */
  const landed = ({ edit: sentEdit }: SentRequest) => {
    clearDraft();
    onSent();
    if (!inline && sentEdit === undefined) onClose();
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
    standing,
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
      if (event.key !== "Escape") return;
      // The Escape that ends an input method's composition is the input method's: it closes
      // nothing here, and the draft stays (`inputMethodKey`). It goes no further either. The
      // page's keymap passes over a key marked `isComposing` only, and Safari's comes unmarked,
      // so it would reach a binding of Escape beneath the composer: the agents scope's `back`,
      // which leaves an agent's composer, or an open dialog's close.
      if (inputMethodKey(event)) {
        event.stopPropagation();
        return;
      }
      // An open picker takes Escape on the field's own listener and stops it there, so this
      // never sees it: Escape shuts the list, and the one after it reaches the composer.
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
   *  CR LF). */
  const onBodyChange = (change: MentionFieldChange) => {
    followedBlocks.current = followEdit(
      followedBlocks.current,
      change.body,
      change.range,
      change.inputType
    );
    previousBody.current = change.body;
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
  /** Who this draft addresses, read from its own words (`draftParties`): the delivering
   *  parties, whose advertised modes the notices below are drawn from, and whether it names a
   *  party at all, which is what decides that a typed `/btw` is a command. */
  const parties = useMemo(() => draftParties(body), [body]);
  /** The send as the composer's own state addresses and shapes it: what Send puts on the wire,
   *  and what a held send is compared with to tell an untouched draft from an edited one. Its
   *  draft carries whether its text names a party, read here from the text it sends, where the
   *  grammar of references is loaded (`outgoingDraft`), so the send needs none. A send of the
   *  body this render drew takes this render's reading of it (`parties`), so a render that
   *  compares a held send parses the body no second time. */
  const composedRequest = (
    sentBody = body,
    sentReplacement = replacement
  ): Omit<SentRequest, "sendKey"> => ({
    anchor,
    ask: { multiple, options: submittedAskOptions(askOptions), urgency },
    draft: outgoingDraft(
      { body: sentBody, replacement: sentReplacement },
      sentBody === body ? parties.named : undefined
    ),
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
  // is sent.
  const deletedReply =
    replyTo === null || postDeletion(replyParent) === null
      ? undefined
      : `The ${replyTo.parentKind} this reply answers was deleted, so it takes no reply.` +
        (standing ? "" : " Press Cancel reply to send it as a message that answers none.");
  const submitReason = refusedKind
    ? "This issue is closed. Reopen it to send."
    : (deletedReply ??
      elsewhere ??
      (changed === undefined ? undefined : "Use your edit or discard it before you save.") ??
      (offered === undefined ? undefined : "Use your draft or discard it before you send.") ??
      draftRefusal(kind, body, replacement, outbound));
  const footId = useId();
  const canSubmit = canSubmitComposer(submitReason, sending, upload.pending);
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
  const bodyLabel =
    edit !== undefined
      ? "Edit comment"
      : inline
        ? "Reply"
        : kind === "ask"
          ? "Question"
          : kind === "suggestion"
            ? "Reason"
            : "Comment";
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

  const composerControls = (
    <fieldset
      className={
        compact
          ? "min-w-0 space-y-2 border-0 p-0"
          : docked
            ? "min-w-0 border-0 p-0"
            : "min-w-0 space-y-3 border-0 p-0"
      }
      disabled={sending}
      onChangeCapture={stopHeldComposerInput}
      onClickCapture={stopHeldComposerInput}
      onInputCapture={stopHeldComposerInput}
      onKeyDownCapture={stopHeldComposerInput}
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
            author={replyTo.actor}
            className="min-w-0 flex-1"
            deleted={postDeletion(replyParent)}
            edited={replyParent?.edited_at}
            excerpt={replyParent === undefined ? "" : replyTo.excerpt}
            excerptTree={replyParent === undefined ? undefined : replyTo.excerptTree}
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
      {confirmingDiscard ? (
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
      ) : null}
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
        fieldRef={field}
        inline={inline}
        issueKey={pickerIssue}
        label={bodyLabel}
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
      {/* A kind the closed issue refuses can send nothing, and a margin compose of that kind ends
          once it offers nothing (`useMarginSheet`), so a draft taken into its field would go
          with no word: it offers Discard my draft alone. */}
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
      />
      {plainNotice.length > 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>
          Sending as {MODE_LABELS[plainMode]}:{" "}
          {plainNotice.map((option) => option.title).join(" and ")}{" "}
          {plainNotice.length > 1 ? "do" : "does"} not advertise Send.
        </p>
      ) : null}
      {deaf ? (
        <p className={`text-sm ${dangerText}`}>
          {unsupportedOptions.map((option) => option.title).join(" and ")}{" "}
          {unsupportedOptions.length > 1 ? "advertise" : "advertises"} no delivery mode, so sending
          it records a failed attempt. {noDeliveryReasons(unsupportedOptions)}
        </p>
      ) : unsupported ? (
        <p className={`text-sm ${dangerText}`}>
          {unsupportedOptions.map((option) => option.title).join(" and ")}{" "}
          {unsupportedOptions.length > 1 ? "do" : "does"} not advertise{" "}
          {outboundMode === undefined ? null : MODE_LABELS[outboundMode]}, so sending it records a
          failed attempt.
          {alternatives.length === 0
            ? null
            : ` Prefix with ${alternatives.map((mode) => DELIVERY_COMMANDS[mode]).join(" or ")} to send it as ${alternatives
                .map((mode) => `${mode === "aside" ? "an" : "a"} ${MODE_LABELS[mode]}`)
                .join(" or ")} instead.`}
        </p>
      ) : null}
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
      {uploadOwner === undefined ? null : (
        // An upload refused for another owner is that owner's: its refusal shows here in words of
        // its own, with no Retry (`pastedElsewhere`), and the status shows the rest.
        <DraftUploadStatus showRefusal={homes.pastedElsewhere === undefined} upload={upload} />
      )}
      {homes.pastedElsewhere === undefined ? null : <QueryError message={homes.pastedElsewhere} />}
      {homes.landedElsewhere.map(({ key, text }) => (
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} key={key} role="status">
          {text}
        </p>
      ))}
      {notice}
      {/* A draft kept for another reply, or for none, while the composer answers this one: the
          line says whose it is and the way back, so the host's move takes nothing with no word. */}
      {keptAway.map((away) => (
        <p
          className={`text-sm ${textMutedOnSurfaceMuted}`}
          key={away.replyTo?.id ?? ""}
          role="status"
        >
          {keptAwayLine(away)}
        </p>
      ))}
      {refusal === undefined ? null : (
        <div className="flex flex-wrap items-center gap-3">
          <QueryError
            message={refusalMessage(refusal, anchor, anchorRefusal)}
            // Retry sends the draft as it stands, so it asks what Send asks of it.
            onRetry={canSubmit ? send : undefined}
            retrying={sending}
          />
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
              className={`text-sm font-medium underline ${dangerText}`}
              onClick={discard}
              type="button"
            >
              Discard draft
            </button>
          ) : null}
        </div>
      )}
      <RefusableButton
        busy={sending ? "Sending…" : upload.pending > 0 ? "Uploading file…" : undefined}
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
  // still being tried under its key, so it says so and offers Stop trying, which sits outside the
  // held fieldset, the one control a send that is out leaves the reader.
  const heldLate = outgoing.held?.status === "late" ? outgoing.held : undefined;
  const lateStatus =
    heldLate === undefined ? null : (
      <div className="flex flex-wrap items-center gap-3">
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} role="status">
          {KEYED_SEND_LATE_TEXT}
        </p>
        <StopTrying onStop={outgoing.stop} stopping={heldLate.stopping} />
      </div>
    );
  const form = (
    <form
      aria-label="Comment composer"
      className={
        compact
          ? "space-y-2"
          : docked
            ? `${pickerOpen ? "z-20" : "z-[8]"} fixed inset-x-0 bottom-16 space-y-2 border-t px-4 py-2 sm:sticky sm:top-0 sm:z-20 sm:-mx-2 sm:border-b sm:px-2 ${borderDefault} ${surfaceBg}`
            : `space-y-3 rounded-xl border p-3 ${calloutInfoBorder} ${calloutInfoBg}`
      }
      onSubmit={submit}
      ref={formRef}
    >
      {composerControls}
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
