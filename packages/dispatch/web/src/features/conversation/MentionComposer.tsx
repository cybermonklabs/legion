import { type MutationKey, useQuery } from "@tanstack/react-query";
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
import { issueCollaboratorsQuery } from "../../api/queries";
import type {
  Agent,
  AskUrgency,
  IssueCollaboratorAgent,
  IssueCollaboratorLegionAgent,
} from "../../api/types";
import { Chip } from "../../components/Chip";
import { focusIntoView } from "../../components/focus-place";
import { heldField } from "../../components/held-press";
import { MOVING_KEYS, writesText } from "../../components/key-targets";
import { AgentListRefilling, QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { TruncatedText } from "../../components/TruncatedText";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useFootInset } from "../../hooks/useFootInset";
import { inputMethodKey } from "../../lib/input-method";
import {
  badgeMed,
  borderDefault,
  borderStrong,
  calloutInfoBg,
  calloutInfoBorder,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  checkboxAccent,
  controlHoverBorder,
  dangerHoverBorder,
  dangerText,
  dismissButtonText,
  focusRing,
  hoverToDangerText,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButtonBg,
  quoteAccentBorder,
  quoteBodyText,
  referencePillBorder,
  secondaryButtonText,
  surfaceBg,
  surfaceMutedHoverBg,
  surfaceRecessedBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { appendToDraft, DraftUploadStatus } from "../artifacts/ArtifactUpload";
import { ASK_URGENCIES_ASCENDING, URGENCY_LABELS } from "../inbox/ask-urgency";
import { useComposerRegistry } from "../margin/margin-context";
import { AgentName, type AgentText, useAgentText } from "../refs/AgentName";
import { postDeletion } from "../refs/post-marks";
import { ReferencePicker } from "../refs/ReferencePicker";
import { composerReferences } from "../refs/routes";
import { keyText } from "../shell/key-text";
import { modHeld } from "../shell/keymap";
import { SendStoppedError } from "../writes/keyed-write";
import { StopTrying } from "../writes/StopTrying";
import {
  type FollowedBlocks,
  followEdit,
  spansFromBlocks,
  type TextEditRange,
  textareaValue,
} from "./block-spans";
import {
  type AskOptionDraft,
  type Draft,
  emptyAskOption,
  mentionDisplay,
  replyPrefill,
  seededDraft,
  submittedAskOptions,
} from "./composer-draft";
import {
  type AcceptedMention,
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
import { deliveryModesOf, MODE_LABELS, noDeliveryReasons, plainDelivery } from "./delivery";
import type { HeldSend } from "./held-sends";
import { KeptDraftOffers } from "./KeptDraftOffers";
import { keptAwayLine } from "./KeptReply";
import { ReplyQuote } from "./ReplyQuote";
import {
  carriedIdsOf,
  DELIVERY_COMMANDS,
  type DeliveryPlan,
  deliveryPlan,
  KEYED_SEND_LATE_TEXT,
  mentionText,
  requestFor,
  type SaveCommentEdit,
  type SentDraft,
  type SentRequest,
  saveCommentEdit,
  survivingMentions,
  unedited,
} from "./send-request";
import { useUnsentDrafts } from "./unsent-drafts";
import { useAgents } from "./useAgents";
import { useComposerSend } from "./useComposerSend";
import { useDraftHomes } from "./useDraftHomes";
import { composerNameOf, mountDraft, type OfferedDraft, useKeptDrafts } from "./useKeptDrafts";

interface MentionOption {
  readonly capabilities: readonly string[];
  /** Where and when: the session's directory and when it was last seen; for a role no live agent
   *  holds, the role and that it is not running. */
  readonly detail: string;
  /** The name of the session the option is, or of the one holding its role, as `useAgentText`
   *  gives it: the filter matches it, and a role option's detail tooltip names it. */
  readonly holder: string;
  /** The session the option is, or, for a role, the session holding it (the newest one for a
   *  role nobody holds): `AgentName` draws it. */
  readonly sessionId: string;
  /** That session's title as the agent list gave it: its name when the registry cache has none. */
  readonly sessionTitle: string;
  readonly target: string;
  /** The text the mention inserts (`@Title`) and the option's accessible name: a role's name, or
   *  the session's name as `useAgentText` gives it. */
  readonly title: string;
}

function roleOptions(
  agents: readonly Agent[],
  text: Pick<AgentText, "sessionText">
): MentionOption[] {
  const byRole = new Map<string, Agent>();
  for (const agent of agents) {
    for (const role of agent.roles ?? []) byRole.set(role, agent);
  }
  return [...byRole.entries()]
    .sort(([left], [right]) => {
      const leftController = left.includes("controller");
      const rightController = right.includes("controller");
      return leftController === rightController
        ? left.localeCompare(right)
        : leftController
          ? -1
          : 1;
    })
    .map(([role, agent]) => ({
      capabilities: agent.capabilities ?? [],
      detail: `${agent.dir} · ${new Date(agent.last_seen).toLocaleString()}`,
      holder: text.sessionText(agent.session_id, agent.title),
      sessionId: agent.session_id,
      sessionTitle: agent.title,
      target: `role:${role}`,
      title: role,
    }));
}

export function mentionOptions(
  agents: readonly Agent[],
  text: Pick<AgentText, "sessionText">
): MentionOption[] {
  return [
    ...roleOptions(agents, text),
    ...[...agents]
      .sort(
        (left, right) => right.last_seen - left.last_seen || left.title.localeCompare(right.title)
      )
      .map((agent) => ({
        capabilities: agent.capabilities ?? [],
        detail: `${agent.dir} · ${new Date(agent.last_seen).toLocaleString()}`,
        holder: text.sessionText(agent.session_id, agent.title),
        sessionId: agent.session_id,
        sessionTitle: agent.title,
        target: `session:${agent.session_id}`,
        title: text.sessionText(agent.session_id, agent.title),
      })),
  ];
}

/**
 * The picker's "Not running" options: one per Legion role token the issue's collaborators record
 * (`IssueCollaborators.agents[].legion`) that no live agent holds - the live agents' `roles`, the
 * rule the live role options follow - addressed `role:<token>` and drawn by the role's newest
 * session, the one entry per token the server leaves unreplaced (`legion.replaced_by` null), so a
 * replaced session is never an option of its own. A session the live list itself names is left
 * out whatever its roles say: a live session that claims no role would otherwise be offered here
 * and draw its own `Running` mark, so one row would read running and not running at once.
 *
 * The options keep the server's order, which the collaborators contract states: the registry's
 * agents by role in workflow order, then newest first.
 */
export function stoppedRoleOptions(
  live: readonly Agent[],
  collaborators: readonly IssueCollaboratorAgent[],
  text: Pick<AgentText, "sessionText">
): MentionOption[] {
  const held = new Set(live.flatMap((agent) => agent.roles));
  const running = new Set(live.map((agent) => agent.session_id));
  const offered = (agent: IssueCollaboratorAgent): agent is IssueCollaboratorLegionAgent =>
    agent.legion !== null &&
    agent.legion.replaced_by === null &&
    !held.has(agent.legion.role_token) &&
    !running.has(agent.session_id);
  return collaborators.filter(offered).map((agent) => {
    // The title the server gives is the registry's, else the session's newest stamped one; the
    // name is still `useAgentText`'s, as a live option's holder is.
    const storedTitle = agent.title ?? "";
    return {
      capabilities: [],
      detail: `${agent.legion.role} · not running`,
      holder: text.sessionText(agent.session_id, storedTitle),
      sessionId: agent.session_id,
      sessionTitle: storedTitle,
      target: `role:${agent.legion.role_token}`,
      title: agent.legion.role_token,
    };
  });
}

/** Reconcile accepted mentions against the actual textarea edit range. */
export function reconcileMentions(
  before: string,
  after: string,
  mentions: readonly AcceptedMention[],
  edit?: TextEditRange
): AcceptedMention[] {
  if (edit !== undefined) {
    const delta = after.length - before.length;
    return mentions.flatMap((mention) => {
      if (mention.end <= edit.start) return [mention];
      if (mention.start >= edit.end) {
        return [{ ...mention, end: mention.end + delta, start: mention.start + delta }];
      }
      return [];
    });
  }
  const delta = after.length - before.length;
  if (delta < 0) {
    const deleted = -delta;
    const possibleStarts = Array.from({ length: after.length + 1 }, (_, start) => start).filter(
      (start) =>
        before.slice(0, start) === after.slice(0, start) &&
        before.slice(start + deleted) === after.slice(start)
    );
    return mentions.flatMap((mention) => {
      if (possibleStarts.some((start) => mention.start < start + deleted && mention.end > start)) {
        return [];
      }
      const shifts = new Set(possibleStarts.map((start) => (start < mention.start ? delta : 0)));
      if (shifts.size !== 1) return [];
      const shift = shifts.values().next().value;
      if (shift === undefined) return [];
      return [{ ...mention, end: mention.end + shift, start: mention.start + shift }];
    });
  }
  let prefix = 0;
  const common = Math.min(before.length, after.length);
  while (prefix < common && before[prefix] === after[prefix]) prefix += 1;
  return mentions.flatMap((mention) => {
    if (mention.end <= prefix) return [mention];
    if (mention.start >= prefix) {
      return [{ ...mention, end: mention.end + delta, start: mention.start + delta }];
    }
    return [];
  });
}

function mentionQuery(
  body: string,
  selectionStart: number | null
): { query: string; start: number } | undefined {
  const caret = selectionStart ?? body.length;
  const before = body.slice(0, caret);
  const at = before.lastIndexOf("@");
  if (at < 0 || /\s/.test(before.slice(at + 1))) return undefined;
  return { query: before.slice(at + 1), start: at };
}

/** Whether Send takes the draft now: nothing refuses it (`draftRefusal`, the one rule for what an
 *  empty draft is) and no save it waits on is in flight, nor an upload within its deadline
 *  (`DraftUpload.holding`). */
export function canSubmitComposer(
  refusal: string | undefined,
  isSaving: boolean,
  holdingUploads: number
): boolean {
  return refusal === undefined && !isSaving && holdingUploads === 0;
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

/** Whether a key the send's hold stops (`stopHeldComposerKey`) is one the hold keeps from the
 *  browser too, as one the composer acts on or one that would type: Enter, with a modifier or
 *  none; Escape; Ctrl/Cmd+K; and every key that writes text (`writesText`). Every other Ctrl/Cmd
 *  chord and every function key stay the browser's, so find, copy, select all, reload, print and
 *  zoom still work while the send is out. */
function heldFromBrowser(event: KeyboardEvent): boolean {
  const { key } = event;
  if (key === "Enter" || key === "Escape") return true;
  if (event.ctrlKey || event.metaKey) return key.toLowerCase() === "k";
  return writesText(event);
}

export function MentionComposer({
  agents: suppliedAgents,
  anchor,
  anchorRefusal,
  autoFocus = false,
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
  const textarea = useRef<HTMLTextAreaElement>(null);
  const replacementTextarea = useRef<HTMLTextAreaElement>(null);
  const formRef = useRef<HTMLFormElement>(null);
  const frameRef = useRef<HTMLDivElement>(null);
  const lastFocusedField = useRef<HTMLElement | null>(null);
  const optionLabelRefs = useRef<Array<HTMLInputElement | null>>([]);
  const focusAddedOption = useRef(false);
  const previousBody = useRef(initial.body);
  const pendingTextEdit = useRef<
    { before: string; inputType: string; range: TextEditRange } | undefined
  >(undefined);
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
  const [mentions, setMentions] = useState<AcceptedMention[]>(initial.mentions);
  /** The edited comment's blocks as the field's edits moved them, while the blocks route has
   *  answered them; Save carries their ids (`carriedBlocks`). */
  const followedBlocks = useRef<FollowedBlocks | undefined>(undefined);
  /** Puts a whole draft in the field at once - its text, mentions, and replacement in step with
   *  the request that supplied them - where the reader did not type it, so their next edit is
   *  measured from here (`previousBody`). */
  const replaceDraft = useCallback((draft: Draft) => {
    setBody(draft.body);
    previousBody.current = draft.body;
    followedBlocks.current = followEdit(followedBlocks.current, draft.body, undefined, undefined);
    setMentions([...draft.mentions]);
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
  const [referencePickerOpen, setReferencePickerOpen] = useState(false);
  const [autocomplete, setAutocomplete] = useState<{ query: string; start: number }>();
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  // Why the last kind pick was refused, kept with the mark it answered: the composer stays mounted
  // when its mark changes - the margin writes it again after the server refused the send for it -
  // and a refusal about the old mark says nothing about the new one. A quote anchor (a closed
  // issue's document) has no mark.
  const [kindRefusal, setKindRefusal] = useState<{ markId: string | undefined; text: string }>();
  const autocompleteOpen = autocomplete !== undefined;
  const live = useAgents(
    suppliedAgents === undefined && owner.kind !== "session",
    autocompleteOpen
  );
  const agents = suppliedAgents ?? live.agents;
  // A host's own list is its answer; the composer's own read says who is absent only while it is
  // current (`useAgents`).
  const agentsCurrent = suppliedAgents !== undefined || live.current;
  const text = useAgentText();
  const options = useMemo(() => mentionOptions(agents, text), [agents, text]);
  // The issue's collaborators feed the "Not running" options, read once the picker opens on an
  // issue owner alone: a project document's margin composer carries that document rather than an
  // issue, while an issue document's margin composer is an issue owner and does read them, and a
  // closed picker and a direct message read nothing. The query is the Collaborators tab's, so the
  // two readers share one entry - a tab read within the shell's 30 s `staleTime` opens the picker
  // with no request of its own - and the event feed refreshes that key by a rule of its own
  // (`changesCollaborators` in event-refresh.ts), which refetches it under a picker that stays
  // open.
  const collaboratorsIssue = owner.kind === "issue" ? owner.issueKey : undefined;
  const collaborators = useQuery({
    ...issueCollaboratorsQuery(collaboratorsIssue),
    enabled: collaboratorsIssue !== undefined && autocompleteOpen,
  });
  // A live list whose state is unknown (not read yet, or its read failed) holds every role as far
  // as the picker can tell, which is what `agentsCurrent` false says of it, so no role is offered
  // as not running.
  const stoppedOptions = useMemo(
    () => (agentsCurrent ? stoppedRoleOptions(agents, collaborators.data?.agents ?? [], text) : []),
    [agents, agentsCurrent, collaborators.data, text]
  );
  const [filteredOptions, filteredStoppedOptions] = useMemo<
    [MentionOption[], MentionOption[]]
  >(() => {
    if (autocomplete === undefined) return [[], []];
    const query = autocomplete.query.toLocaleLowerCase();
    // A role option matches its holder's name too, which its detail line shows.
    const matches = (option: MentionOption) =>
      query === "" ||
      option.target.toLocaleLowerCase().includes(query) ||
      option.title.toLocaleLowerCase().includes(query) ||
      option.holder.toLocaleLowerCase().includes(query) ||
      option.detail.toLocaleLowerCase().includes(query);
    return [options.filter(matches), stoppedOptions.filter(matches)];
  }, [autocomplete, options, stoppedOptions]);
  const notRunningId = useId();
  const compact = inline || edit !== undefined;
  /** The whole draft, gone - the body, its accepted mentions, a suggestion's replacement and an
   *  ask's options - on a successful send and on Discard alike, in every host, leaving what a fresh
   *  mount shows: a host whose `onClose` only moves focus (`AgentsPage`'s rows) keeps the composer
   *  mounted, and it must read as a new one would. That includes the channel's own mentions: an
   *  issue comment an Agents row writes reaches its agent only by mentioning it, so every message
   *  after the first is seeded as the first was. The kind, the urgency and `Allow multiple` are
   *  the reader's settings, not the draft, and stay. */
  const clearDraft = () => {
    replaceDraft(seededDraft(seedMentions, undefined, owner));
    setAskOptions([emptyAskOption()]);
  };
  const references = useMemo(() => composerReferences(body), [body]);
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
    replaceDraft(seededDraft(seedMentions, { body, mentions }, owner));
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
    textarea.current?.focus();
  }, [autoFocus]);
  useEffect(() => {
    if (focusAddedOption.current) {
      optionLabelRefs.current[askOptions.length - 1]?.focus();
      focusAddedOption.current = false;
    }
  }, [askOptions.length]);
  useEffect(() => {
    if (suppliedAgents === undefined && autocompleteOpen) void live.refetch();
  }, [autocompleteOpen, live.refetch, suppliedAgents]);
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
      if (!standing) textarea.current?.focus();
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
    onCancelReply?.({ body, mentions, replacement });
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
   *  shows (`useDraftHomes`), or a reference the picker inserts - where the reader did not type
   *  them, so their next edit is measured from here (`previousBody`). */
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
    field: { askOptions, body, mentions, multiple, replacement, urgency },
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
  // An edit or a draft waiting beside the field holds it read only (`KeptDraftOffers`) until the
  // reader takes it in or drops it, and the field names that offer as its description
  // (`heldId`), so a screen reader says why it takes no text. A key typed there writes nothing, so
  // from that key on the offer says, after its own words, what the field waits for, until nothing
  // waits there.
  const heldByOffer = changed !== undefined || offered !== undefined;
  const heldId = useId();
  const [typedOver, setTypedOver] = useState(false);
  if (typedOver && !heldByOffer) setTypedOver(false);
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
      // An Escape that ends an input method's composition is the input method's.
      if (event.key !== "Escape" || inputMethodKey(event)) return;
      if (autocomplete !== undefined) {
        event.stopPropagation();
        setAutocomplete(undefined);
        return;
      }
      if (referencePickerOpen) {
        event.stopPropagation();
        setReferencePickerOpen(false);
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
    autocomplete,
    // A dormant composer renders no form; the one it renders when it wakes takes the listener.
    dormant,
    referencePickerOpen,
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

  const addMention = (option: MentionOption) => {
    const current = textarea.current?.value ?? body;
    const query = mentionQuery(current, textarea.current?.selectionStart ?? null);
    if (query === undefined || mentions.some((mention) => mention.target === option.target)) {
      setAutocomplete(undefined);
      return;
    }
    const display = mentionDisplay(option.title, option.target);
    const text = mentionText(display);
    const caret = textarea.current?.selectionStart ?? current.length;
    const after = `${current.slice(0, query.start)}${text}${current.slice(caret)}`;
    setMentions((currentMentions) => [
      ...reconcileMentions(current, after, currentMentions, { end: caret, start: query.start }),
      { end: query.start + text.length, start: query.start, target: option.target, text: display },
    ]);
    previousBody.current = after;
    setBody(after);
    setAutocomplete(undefined);
    requestAnimationFrame(() => {
      textarea.current?.focus();
      textarea.current?.setSelectionRange(query.start + text.length, query.start + text.length);
    });
  };
  /** One suggestion: a button that inserts the mention. Each suggestion is one option inside a
   *  listbox, which owns only options, so the agent in it is drawn by `AgentName` without a link
   *  (exclusion E2): a session option's name, and a role option's holder - for a role no live
   *  agent holds, its newest session. */
  const mentionOptionButton = (option: MentionOption) => {
    const isRole = option.target.startsWith("role:");
    const agent = (
      <AgentName kind="static" sessionId={option.sessionId} storedTitle={option.sessionTitle} />
    );
    return (
      // Below `xl` every button is `inline-flex` with centred items (styles.css's touch rule,
      // unlayered, so it beats `block`): the title and detail stack explicitly and each stretches
      // to the row's width.
      // A session option's full id is its tooltip and accessible description: its name can be the
      // short form `session:<8>…`, which two sessions may share.
      <button
        aria-label={option.title}
        className={`flex min-h-11 w-full flex-col rounded-lg px-3 py-2 text-left text-sm ${surfaceMutedHoverBg}`}
        key={option.target}
        onMouseDown={(event) => event.preventDefault()}
        onClick={() => addMention(option)}
        role="option"
        title={isRole ? undefined : option.sessionId}
        type="button"
      >
        <span className={`self-stretch font-medium ${textPrimaryOnSurface}`}>
          {isRole ? option.title : agent}
        </span>
        <TruncatedText
          className={`self-stretch text-xs ${textMutedOnSurface}`}
          title={isRole ? `${option.holder} · ${option.detail}` : option.detail}
        >
          {isRole ? (
            <>
              {agent} · {option.detail}
            </>
          ) : (
            option.detail
          )}
        </TruncatedText>
      </button>
    );
  };
  const onBodyChange = (value: string, selectionStart: number | null) => {
    const before = previousBody.current;
    const pending = pendingTextEdit.current;
    pendingTextEdit.current = undefined;
    setMentions((current) =>
      reconcileMentions(
        before,
        value,
        current,
        pending?.before === before ? pending.range : undefined
      )
    );
    // `mapSpans` takes the range only where it explains the edit of the value the spans follow,
    // which is the textarea's own (LF for an edited body's CR LF).
    followedBlocks.current = followEdit(
      followedBlocks.current,
      value,
      pending?.range,
      pending?.inputType
    );
    previousBody.current = value;
    setBody(value);
    if (edit !== undefined || owner.kind === "session") {
      setAutocomplete(undefined);
      return;
    }
    setAutocomplete(mentionQuery(value, selectionStart));
    setConfirmingDiscard(false);
  };
  // The body field records each edit from the browser's own `beforeinput`: its type, which tells
  // `mapSpans` a cut or a paste, and the range it replaces. React's `onBeforeInput` is the
  // `textInput` event, which carries no type and which no deletion or cut fires.
  const bodyFieldRef = useCallback((element: HTMLTextAreaElement | null) => {
    textarea.current = element;
    if (element === null) return;
    const recordEdit = (input: InputEvent) => {
      let start = element.selectionStart;
      let end = element.selectionEnd;
      if (start === end && input.inputType === "deleteContentBackward")
        start = Math.max(0, start - 1);
      if (start === end && input.inputType === "deleteContentForward")
        end = Math.min(element.value.length, end + 1);
      pendingTextEdit.current = {
        before: element.value,
        inputType: input.inputType,
        range: { end, start },
      };
    };
    element.addEventListener("beforeinput", recordEdit);
    return () => {
      element.removeEventListener("beforeinput", recordEdit);
      textarea.current = null;
    };
  }, []);
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
    draft: { body: sentBody, mentions, replacement: sentReplacement },
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
        textarea.current?.value ?? body,
        replacementTextarea.current?.value ?? replacement
      )
    );
  const activeMentions = useMemo(() => survivingMentions(body, mentions), [body, mentions]);
  const effectiveTargets: readonly string[] =
    owner.kind === "session"
      ? [`session:${owner.sessionId}`]
      : replyTo?.parentKind === "message" && replyTo.thread !== undefined
        ? [replyTo.thread.target]
        : activeMentions.map((mention) => mention.target);
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
      : replyTo?.parentKind !== "message" && activeMentions.length > 0);
  const inheritedDelivery =
    heldPicked ??
    (owner.kind === "session"
      ? (replyTo?.thread?.delivery ?? plainMode)
      : replyTo?.parentKind === "message"
        ? replyTo.thread?.delivery
        : activeMentions.length > 0
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
  const canSubmit = canSubmitComposer(submitReason, sending, upload.holding);
  /** Sends the draft as it stands, its whole request frozen here (`sentRequest`), so nothing done
   *  after this call can change where it goes. Until the server answers, one fieldset holds every
   *  control that could change or discard the draft (`stopHeldComposerInput`,
   *  `stopHeldComposerKey`), from this task on, before React draws the fieldset held. From here
   *  on the held send keeps the draft, so the unsent copy under the send name goes. */
  const send = () =>
    outgoing.send((refused) => {
      setConfirmingDiscard(false);
      setAutocomplete(undefined);
      setReferencePickerOpen(false);
      forgetUnsent();
      return sentRequest(refused);
    });
  /** The hold on the draft from Send's own task on, before React draws the fieldset held: the
   *  fieldset's capture stops every click, input, change, paste and drop bound for a control in
   *  it - so no file goes into a draft that is out, and no press acts - and every key but the
   *  ones that only move the focus or the caret (`stopHeldComposerKey`), so no key writes into the
   *  box, Escape raises no Discard prompt, `$mod+K` (Cmd+K on a Mac, Ctrl+K elsewhere) opens no
   *  picker and Ctrl+Enter sends nothing over a draft that is out. A key it stops reaches no
   *  handler of the composer's. The browser's own action for that key is cancelled only for a key
   *  the composer acts on or one that would type (`heldFromBrowser`). While React shows the hold
   *  the fieldset is `aria-disabled` and its fields read-only. It is never `disabled`: a browser
   *  takes the focus off a control that becomes disabled under it, which would drop a keyboard
   *  reader who sent from the box or from Send to the page's body. Tab goes through, so the hold
   *  keeps the reader's place and never traps them in it. */
  const stopHeldComposerInput = (event: SyntheticEvent) => {
    if (!outgoing.sendingNow()) return;
    event.preventDefault();
    event.stopPropagation();
  };
  const stopHeldComposerKey = (event: KeyboardEvent) => {
    if (MOVING_KEYS[event.key] === true || !outgoing.sendingNow()) return;
    event.stopPropagation();
    if (heldFromBrowser(event)) event.preventDefault();
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
    // While a send is out the fieldset is held, never disabled: `aria-disabled` on it, each text
    // field in it read-only (`heldField`) and each press refused by its capture
    // (`stopHeldComposerInput`). The browser takes the focus off a control that turns disabled,
    // so a disabled fieldset would drop the reader who sent from the box or from Send onto the
    // page.
    <fieldset
      aria-disabled={sending || undefined}
      className={
        compact
          ? "min-w-0 space-y-2 border-0 p-0"
          : docked
            ? "min-w-0 border-0 p-0"
            : "min-w-0 space-y-3 border-0 p-0"
      }
      onChangeCapture={stopHeldComposerInput}
      onClickCapture={stopHeldComposerInput}
      onDropCapture={stopHeldComposerInput}
      onInputCapture={stopHeldComposerInput}
      onKeyDownCapture={stopHeldComposerKey}
      onPasteCapture={stopHeldComposerInput}
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
            to={replyTo.to}
          />
          {/* The reply is part of the message's address, fixed once the send is out, so the
              composer's control fieldset holds it with every other draft control. A standing
              box's reply is the box's own address, which nothing ends, so it offers no Cancel
              reply: the control would do nothing there, and the draft stays in the field. */}
          {standing ? null : (
            <button
              aria-disabled={sending || undefined}
              aria-label="Cancel reply"
              className={`inline-flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg text-lg leading-none refused:cursor-not-allowed refused:opacity-50 md:min-h-8 md:min-w-8 ${textMutedOnSurfaceMuted}`}
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
              // Back to the field the reader was writing in, with no scroll of the browser's own,
              // which would centre a field not wholly in view: it is brought into view only where
              // the reader cannot see it whole (`focusIntoView`).
              const field = lastFocusedField.current;
              if (field !== null) focusIntoView(field);
            }}
            type="button"
          >
            Keep
          </button>
        </div>
      ) : null}
      {kind === "suggestion" ? (
        <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
          Replace with
          <textarea
            {...heldField(sending)}
            aria-describedby={offered === undefined ? undefined : heldId}
            aria-label="Replacement"
            aria-readonly={offered !== undefined || undefined}
            className={`mt-1 block w-full rounded-lg border px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
            onChange={(event) => {
              setReplacement(event.target.value);
              setConfirmingDiscard(false);
            }}
            onKeyDown={(event: KeyboardEvent<HTMLTextAreaElement>) => {
              // A key typed while the offer holds the field read only writes nothing: the offer
              // says why (`typedOver`).
              if (offered !== undefined && writesText(event)) setTypedOver(true);
              submitOnModifiedEnter(event);
            }}
            // Read-only while a send holds it, and while a draft is offered beside the field,
            // which it names as its description then (`heldId`).
            readOnly={sending || offered !== undefined}
            ref={replacementTextarea}
            value={replacement}
          />
        </label>
      ) : null}
      <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
        {compact ? null : bodyLabel}
        {/* `data-reply-box`: the box a page's reply key hands the caret (the Inbox's `r`). */}
        <textarea
          {...heldField(sending)}
          aria-describedby={heldByOffer ? heldId : undefined}
          aria-label={bodyLabel}
          aria-readonly={heldByOffer || undefined}
          className={`mt-1 block w-full rounded-lg border px-3 py-2 font-normal outline-none ${inline ? "min-h-11 resize-none" : "min-h-24"} ${inputClasses(true)} ${textPrimaryOnSurface}`}
          data-reply-box=""
          onChange={(event) => onBodyChange(event.target.value, event.target.selectionStart)}
          onDrop={homes.drop}
          onInput={
            inline
              ? (event: SyntheticEvent<HTMLTextAreaElement>) => {
                  event.currentTarget.style.height = "auto";
                  event.currentTarget.style.height = `${event.currentTarget.scrollHeight}px`;
                }
              : undefined
          }
          onKeyDown={(event: KeyboardEvent<HTMLTextAreaElement>) => {
            // A key typed while an offer holds the field read only writes nothing: the offer says
            // why (`typedOver`).
            if (heldByOffer && writesText(event)) setTypedOver(true);
            // `$mod+K` opens the reference picker: Ctrl+K, or Cmd+K on a Mac, where a text
            // field keeps Control+K, which deletes to the end of the line (`modHeld`).
            if (
              owner.kind === "issue" &&
              !compact &&
              modHeld(event) &&
              event.key.toLowerCase() === "k"
            ) {
              event.preventDefault();
              setReferencePickerOpen(true);
              return;
            }
            submitOnModifiedEnter(event, { disabled: upload.holding > 0 });
          }}
          onPaste={homes.paste}
          // Read-only while a send holds it, and while an edit or a draft waits beside the field,
          // which it names as its description then (`heldId`).
          readOnly={sending || heldByOffer}
          ref={bodyFieldRef}
          rows={1}
          value={body}
        />
      </label>
      {/* A kind the closed issue refuses can send nothing, and a margin compose of that kind ends
          once it offers nothing (`useMarginSheet`), so a draft taken into its field would go
          with no word: it offers Discard my draft alone. */}
      <KeptDraftOffers
        changed={changed}
        field={textarea}
        gone={goneAway}
        goneTakes={awayTakes}
        heldId={heldId}
        kind={kind}
        offered={offered}
        onDiscardChanged={discard}
        onDropGone={dropAway}
        onDropOffered={dropOffered}
        onTakeChanged={takeChanged}
        onTakeGone={takeAway}
        onTakeOffered={refusedKind ? undefined : takeOffered}
        typedOver={typedOver}
      />
      {activeMentions.length === 0 ? null : (
        <ul
          aria-label="Mention targets"
          className={`flex flex-wrap gap-2 text-xs ${textPrimaryOnSurface}`}
        >
          {activeMentions.map((mention) => (
            <li className="rounded-full border px-2 py-1" key={mention.target}>
              {mention.text} · <code>{mention.target}</code>
            </li>
          ))}
        </ul>
      )}
      {autocomplete === undefined ? null : (
        <div
          aria-label="Mention suggestions"
          className={`max-h-52 overflow-y-auto rounded-lg border p-1 ${surfaceRecessedBg}`}
          role="listbox"
        >
          {filteredOptions.map(mentionOptionButton)}
          {/* The issue's Legion roles no live agent holds, after the live options: a mention of
              one is addressed to its role as any role option is. */}
          {filteredStoppedOptions.length === 0 ? null : (
            // biome-ignore lint/a11y/useSemanticElements: a listbox owns only options and groups of options; a fieldset is neither.
            <div aria-labelledby={notRunningId} role="group">
              <div
                className={`px-3 pt-2 pb-1 text-xs font-semibold ${textMutedOnSurface}`}
                id={notRunningId}
                role="presentation"
              >
                Not running
              </div>
              {filteredStoppedOptions.map(mentionOptionButton)}
            </div>
          )}
          {/* Only when the picker offers nothing at all: a "Not running" group with options in
              it is a match, so saying no agent matches underneath it would contradict the list
              the reader is looking at. */}
          {agentsCurrent ? (
            filteredOptions.length === 0 && filteredStoppedOptions.length === 0 ? (
              <p className={`px-3 py-2 text-sm ${textMutedOnSurface}`}>No live agents match.</p>
            ) : null
          ) : live.refilling ? (
            <AgentListRefilling className={`px-3 py-2 text-sm ${textMutedOnSurface}`} />
          ) : live.isPending ? (
            <p
              className={`px-3 py-2 text-sm ${textMutedOnSurface}`}
              data-testid="agent-list-loading"
            >
              Loading live agents…
            </p>
          ) : (
            <p className={`px-3 py-2 text-sm ${dangerText}`} data-testid="agent-list-unreachable">
              Could not load live agents — Envoy listener unreachable.
            </p>
          )}
        </div>
      )}
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
            {/* A held box is dimmed as the browser dims a disabled one. `aria-disabled:` reads the
                send's hold alone: `refused:` also reads the native disable of the fieldset around
                it while a draft is offered, where the browser dims the box already. */}
            <input
              aria-disabled={sending || undefined}
              checked={multiple}
              className={`size-4 rounded aria-disabled:opacity-50 ${borderStrong} ${checkboxAccent} ${focusRing}`}
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
                  <input
                    {...heldField(sending)}
                    aria-label={`Option ${index + 1} label`}
                    className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
                    onChange={(event) => updateAskOption(index, "label", event.target.value)}
                    onKeyDown={(event) => {
                      // An Enter that ends an input method's composition is the input method's.
                      if (event.key === "Enter" && !inputMethodKey(event.nativeEvent)) {
                        event.preventDefault();
                        addAskOption();
                      }
                    }}
                    placeholder="Label"
                    readOnly={sending}
                    ref={(element) => {
                      optionLabelRefs.current[index] = element;
                    }}
                    value={option.label}
                  />
                  <input
                    {...heldField(sending)}
                    aria-label={`Option ${index + 1} description`}
                    className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
                    onChange={(event) => updateAskOption(index, "description", event.target.value)}
                    placeholder="Description (optional)"
                    readOnly={sending}
                    value={option.description}
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
            {owner.kind === "issue" ? ` ${keyText("$mod+K")} inserts a reference.` : ""}
          </p>
          {references.length === 0 ? null : (
            <section aria-label="References" className="flex flex-wrap gap-2">
              {references.map((reference) => (
                <a
                  className={`rounded-full border px-2 py-1 text-xs font-medium ${referencePillBorder} ${surfaceRecessedBg} ${badgeMed.text} ${controlHoverBorder}`}
                  href={reference.href}
                  key={reference.reference}
                >
                  {reference.reference}
                </a>
              ))}
            </section>
          )}
          {/* A picker is a draft control. `send` closes one already open, and the `$mod+K` that
              opens one (Cmd+K on a Mac, Ctrl+K elsewhere) in Send's own task meets the
              fieldset's hold before it can open another. */}
          {owner.kind === "issue" && referencePickerOpen ? (
            <ReferencePicker
              issueKey={owner.issueKey}
              onClose={() => setReferencePickerOpen(false)}
              onSelect={(reference) => {
                appendToField([reference]);
                setReferencePickerOpen(false);
                // Back to the box the reference went into, as Keep gives it back
                // (`focusIntoView`).
                if (textarea.current !== null) focusIntoView(textarea.current);
              }}
            />
          ) : null}
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
      {/* An upload holds Send only within its deadline (`DraftUpload.holding`); past it the
          status under the field still says it is uploading. */}
      <RefusableButton
        busy={sending ? "Sending…" : upload.holding > 0 ? "Uploading file…" : undefined}
        refusal={submitReason ?? null}
        refusalShownBy={footId}
        type="submit"
      >
        {title}
      </RefusableButton>
      {/* While Send refuses, the line under it says why - on screen, for a reader with no
          pointer to hover it - and once the draft can go, how to send it. */}
      <p className={`text-xs ${textMutedOnSurfaceMuted}`} id={footId}>
        {submitReason ?? `${keyText("$mod+Enter")} to send · Enter for a new line`}
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
            ? `${autocompleteOpen ? "z-20" : "z-[8]"} fixed inset-x-0 bottom-16 space-y-2 border-t px-4 py-2 sm:sticky sm:top-0 sm:z-20 sm:-mx-2 sm:border-b sm:px-2 ${borderDefault} ${surfaceBg}`
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
