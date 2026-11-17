import { hashKey, type MutationKey, useQuery } from "@tanstack/react-query";
import {
  type ClipboardEvent,
  type DragEvent,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  type Ref,
  type SyntheticEvent,
  useCallback,
  useEffect,
  useId,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import {
  type ArtifactOwner,
  api,
  apiErrorMessage,
  isAnchorMissing,
  isCapExceeded,
  isDecisionBlockInBody,
} from "../../api/client";
import { issueCollaboratorsQuery } from "../../api/queries";
import type {
  Agent,
  AskOption,
  AskUrgency,
  IssueCollaboratorAgent,
  IssueCollaboratorLegionAgent,
} from "../../api/types";
import { Chip } from "../../components/Chip";
import { AgentListRefilling, QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { TruncatedText } from "../../components/TruncatedText";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useFootInset } from "../../hooks/useFootInset";
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
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "../artifacts/ArtifactUpload";
import { ASK_URGENCIES_ASCENDING, URGENCY_LABELS } from "../inbox/ask-urgency";
import { useComposerRegistry } from "../margin/margin-context";
import { AgentName, type AgentText, useAgentText } from "../refs/AgentName";
import { ReferencePicker } from "../refs/ReferencePicker";
import { composerReferences } from "../refs/routes";
import { SendStoppedError } from "../writes/keyed-write";
import { StopTrying } from "../writes/StopTrying";
import {
  type FollowedBlocks,
  followEdit,
  spansFromBlocks,
  type TextEditRange,
  textareaValue,
} from "./block-spans";
import type {
  AcceptedMention,
  AnchorRefusal,
  ComposerAnchor,
  ComposerKind,
  ComposerOwner,
  ReplyTarget,
} from "./composer-model";
import { MODE_LABELS, plainDelivery } from "./delivery";
import type { HeldSend } from "./held-sends";
import { ReplyQuote } from "./ReplyQuote";
import {
  type Address,
  addressWords,
  type CarriedAddress,
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
  sameAddress,
  saveCommentEdit,
  survivingMentions,
  unedited,
} from "./send-request";
import { type AwayDraft, useUnsentDrafts } from "./unsent-drafts";
import { useAgents } from "./useAgents";
import { useComposerSend } from "./useComposerSend";

/** A draft as the composer holds it: the text, the mentions accepted in it, and, when it is a
 *  suggestion, its replacement. Mentions are offsets into that exact text. */
interface Draft {
  readonly body: string;
  readonly mentions: readonly AcceptedMention[];
  readonly replacement?: string;
}

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

interface AskOptionDraft {
  description: string;
  id: number;
  label: string;
}

/** How long after the last keystroke what the reader wrote over a refusal is written back to the
 *  held send, which is what a reload restores (`write-storage.ts`). Short enough that a reader who
 *  stops typing and reloads keeps their words, long enough that a burst of typing writes once. */
const KEEP_DRAFT_MS = 300;

let nextAskOptionId = 0;

function emptyAskOption(): AskOptionDraft {
  nextAskOptionId += 1;
  return { description: "", id: nextAskOptionId, label: "" };
}

function submittedAskOptions(options: readonly AskOptionDraft[]): AskOption[] {
  const submitted: AskOption[] = [];
  for (const option of options) {
    const label = option.label.trim();
    const description = option.description.trim();
    if (label !== "") {
      submitted.push(description === "" ? { label } : { description, label });
    }
  }
  return submitted;
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

function mentionDisplay(title: string, target: string): string {
  const trimmed = title.trim();
  if (trimmed !== "" && !/^(?:session|role):/i.test(trimmed) && !/[@\r\n]/.test(trimmed)) {
    return trimmed;
  }
  return target.replace(":", " ");
}

/** The target a message in this channel already reaches: mentioning it says nothing. */
function ownTarget(owner: ComposerOwner): string | null {
  return owner.kind === "session" ? `session:${owner.sessionId}` : null;
}

/**
 * What a draft becomes under a channel's seed: the reader's prose with the records still in step
 * with it, and the channel's own mentions seeded in front of whatever is not already there. A
 * mount applies it to no draft at all; a host that changes the seed - an Agents row's issue pick,
 * or a reply that moves the message to another channel - has it applied to the live draft in
 * place, so the draft never parts from the composer, or from a send it has in flight.
 *
 * Three rules keep the text and the accepted records saying the same thing, because only the
 * records reach the wire (`survivingMentions` at send) while only the text reaches the reader:
 *
 * - A record counts only while its span is untouched (`survivingMentions`). A span the reader
 *   edited is their prose: the record goes, the text stays.
 * - The channel's own mention belongs to the channel. Entering one that seeds none - a direct
 *   message, which already reaches its session - takes the untouched record for that session out
 *   of the text with its separating space. Every other surviving record stays, so a trip back
 *   restores it.
 * - Seeding is by target, not by position: a mention this channel owes is added only when no
 *   surviving record already names it, wherever in the body that record sits.
 */
function seededDraft(
  seedMentions: readonly { readonly target: string; readonly title: string }[],
  draft: Draft | undefined,
  owner: ComposerOwner
): { body: string; mentions: AcceptedMention[] } {
  let body = draft?.body ?? "";
  let records = survivingMentions(body, draft?.mentions ?? []);
  const own = ownTarget(owner);
  const addressed = own === null ? undefined : records.find((record) => record.target === own);
  if (addressed !== undefined) {
    const cut =
      body.slice(addressed.end, addressed.end + 1) === " " ? addressed.end + 1 : addressed.end;
    const width = cut - addressed.start;
    body = body.slice(0, addressed.start) + body.slice(cut);
    records = records
      .filter((record) => record !== addressed)
      .map((record) =>
        record.start >= cut
          ? { ...record, end: record.end - width, start: record.start - width }
          : record
      );
  }
  const owed = seedMentions.filter(
    (mention) => !records.some((record) => record.target === mention.target)
  );
  let offset = 0;
  const seeded = owed.map((mention) => {
    const text = mentionDisplay(mention.title, mention.target);
    const start = offset;
    offset += mentionText(text).length;
    const end = offset;
    offset += 1;
    return { end, start, target: mention.target, text };
  });
  const prefix = owed
    .map((mention) => mentionText(mentionDisplay(mention.title, mention.target)))
    .join(" ");
  const shift = prefix === "" ? 0 : prefix.length + 1;
  return {
    body: [prefix, body].filter((part) => part !== "").join(" "),
    mentions: [
      ...seeded,
      ...records.map((record) => ({
        ...record,
        end: record.end + shift,
        start: record.start + shift,
      })),
    ],
  };
}

/** What a new reply to `replyTo` starts with: the mentions its parent carries, each once, as the
 *  mention picker writes them. */
function replyPrefill(replyTo: ReplyTarget): Draft {
  const carried = replyTo.mentions ?? [];
  const deduplicated = carried.filter(
    (mention, index) =>
      carried.findIndex((candidate) => candidate.target === mention.target) === index
  );
  const texts = deduplicated.map((mention) =>
    mentionText(mentionDisplay(mention.title, mention.target))
  );
  let offset = 0;
  return {
    body: texts.join(" "),
    mentions: deduplicated.map((mention, index) => {
      const text = mentionDisplay(mention.title, mention.target);
      const start = offset;
      offset += texts[index]?.length ?? 0;
      const end = offset;
      offset += 1;
      return { end, start, target: mention.target, text };
    }),
  };
}

export function hasUnsavedInput(
  body: string,
  replacement: string,
  options: readonly Pick<AskOptionDraft, "description" | "label">[] = []
): boolean {
  return (
    body.trim().length > 0 ||
    replacement.trim().length > 0 ||
    options.some((option) => option.label.trim().length > 0 || option.description.trim().length > 0)
  );
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

/** What Send says while the refusal the composer holds lives at another address than the one it
 *  shows now, since its host moved it (`sameAddress`): where the draft was written for, and the
 *  ways out - back there, where Retry sends it, or Discard. A standing box answers what its host
 *  gives it and no Reply the reader presses (`standing`): an Inbox agent thread's box replies to
 *  the thread's newest message, so one that has come since the refusal moved the box, and no
 *  press brings it back. */
function writtenElsewhere(held: SentRequest, standing: boolean): string {
  if (held.replyTo !== null) {
    return standing
      ? "This message was written as a reply to an earlier message, and this box now replies " +
          "to a newer one. Discard it to write a reply here."
      : "This message was written as a reply. " +
          "Reply to that message again to send it, or discard it.";
  }
  return `This message was written ${addressWords(held, "for")}. Go back to it to send it, or discard it.`;
}

/** Where a file was pasted, as the composer's sentences of its upload name it: `for CORE-1`. */
function pastedWhere(owner: ArtifactOwner): string {
  return "issue" in owner
    ? `for ${owner.issue}`
    : "session" in owner
      ? "into a direct message"
      : "for another document";
}

/** What a refused upload says while the composer addresses another owner than the one the draft
 *  its file was pasted into lives at: a Retry from here would add its reference to a message for
 *  somewhere else, so the Retry waits for the composer to be back there. */
function pastedElsewhere(owner: ArtifactOwner): string {
  return `This file was pasted ${pastedWhere(owner)}. Go back to it to try the upload again.`;
}

/** What the composer says of files that landed into a draft living at another owner than the one
 *  it addresses now: their references wait for that draft, and join it once the composer is back
 *  there. */
function landedFor(owner: ArtifactOwner, files: number): string {
  return files === 1
    ? `The file pasted ${pastedWhere(owner)} is uploaded. ` +
        "Its reference joins the message when you go back to it."
    : `The ${files} files pasted ${pastedWhere(owner)} are uploaded. ` +
        "Their references join the message when you go back to it.";
}

/** What the composer says of a draft it keeps for another address than the one it shows
 *  (`AwayDraft`): whose message or comment that draft answers, or that it answers none, and the
 *  way back to it - that message's Reply, or Cancel reply. Nothing is sent until the reader
 *  presses. */
function keptAwayLine({ owner, replyTo }: AwayDraft): ReactNode {
  if (replyTo === null) {
    return `The message you wrote ${addressWords({ owner, replyTo }, "for")} is kept. Press Cancel reply to go on with it.`;
  }
  const parent = replyTo.parentKind === "message" ? "message" : "comment";
  return (
    <>
      Your reply to the {parent} by <AgentName actor={replyTo.actor} inline /> is kept. Press Reply
      on that {parent} to go on with it.
    </>
  );
}

/** A composer's field as its host hands it a draft the reader carried there from another
 *  composer with that composer's own control (`intake`). */
export interface DraftIntake {
  /** Puts `draft`, which the reader carried to `to`, in the field, and says whether it did: a
   *  composer takes it only while it shows `to`, and only while it holds nothing of its own - a
   *  draft typed, or a send out or refused - which it never writes over. */
  readonly take: (draft: SentDraft, to: CarriedAddress) => boolean;
}

interface MentionComposerProps {
  readonly agents?: readonly Agent[];
  readonly anchor?: ComposerAnchor;
  /** What the host says in place of the server's words when it refused this send for a mark the
   *  document no longer holds (`ANCHOR_MISSING`) and could not write that mark again: the margin's
   *  message, kept with the mark it answers, since the composer stays mounted when a newer compose
   *  replaces the anchor. */
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
  /** Names the send and the target it is held for (`useComposerSend`): a composer that mounts
   *  under a name whose send is out, or was refused, shows that send and its draft, and a host
   *  can hold its own controls - a Reply, an issue picker - while it is out (`useSending`), from
   *  Send's own task on. That decides only what the reader sees: the request is frozen when Send
   *  starts (`SentRequest`), whatever those controls do. A composer with none names its sends
   *  after its own mount. */
  readonly mutationKey?: MutationKey;
  /** A line the host says in the composer, as the composer says where an upload went: the
   *  Conversation's note that a phone thread reply the reader cancelled over this composer's own
   *  draft is kept, and how to go on with it (`usePhoneThread`'s `parked`). */
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
  readonly saveEdit?: SaveCommentEdit;
  /** The reply target is this box's own standing address rather than a Reply the reader
   *  pressed: the Inbox reading column's thread box, which is the thread's permanent reply and
   *  is remounted whenever the column follows the selection onto another row. Taking a target
   *  normally takes the focus, so the reader can write at once; a standing box takes neither,
   *  since the reader's place is the list the column follows and the next `j` belongs to it. What
   *  the reader types into a standing box and does not send is its thread's, kept under the
   *  box's send name (`unsent-drafts.ts`), so the box drawn when the column comes back to the
   *  thread shows it again, and a newer message the host moves the box onto leaves it in the
   *  field rather than prefilling over it. */
  readonly standing?: boolean;
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
  closed = false,
  docked = false,
  finishing = false,
  frame,
  edit,
  seedMentions = [],
  inline = false,
  intake,
  kind = "comment",
  mutationKey,
  notice,
  onCancelReply,
  onClose,
  onKindChange,
  onSent,
  owner,
  ownMoves = 0,
  replyTo = null,
  saveEdit,
  standing = false,
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
  const composerName = mutationKey === undefined ? undefined : hashKey(mutationKey);
  /** Where a standing box, and an inline reply, keep the draft the reader has not sent: under
   *  the send name, for the composer that mounts there next. An inline reply's composer leaves
   *  the page with its thread - Cancel reply, another thread, a phone thread's Back - and the
   *  draft is that thread's until the reader sends or drops it. */
  const unsentName = standing || inline ? composerName : undefined;
  /** Where every other named composer keeps a draft by the address it was written at
   *  (`AwayDraft`): its host moves it from reply to reply - a Reply on a message or a comment -
   *  or takes it off the page, so a draft is the reply's it answers, or its owner's when it
   *  answers none, and waits under the send name until the composer is back there. An edit's
   *  address is the comment it edits, which its send name names, and its draft is kept with the
   *  comment's body it was written over (`AwayDraft.base`). */
  const awayName = standing || inline ? undefined : composerName;
  const unsent = useUnsentDrafts();
  // The drafts kept for other addresses than the one the composer shows, each of which the
  // composer names with the way back to it.
  const keptAway = unsent
    .useAway(awayName ?? "")
    .filter((away) => (away.replyTo?.id ?? null) !== (replyTo?.id ?? null));
  // Only the mount reads it, so it is computed once rather than on every keystroke: the held
  // send's draft, else the unsent draft kept under the send name or for this address, else what
  // the channel seeds. A refusal the host has moved the composer off since - another reply, a
  // newer message - gives way to a draft the reader wrote here and that was kept for it, since
  // the refusal keeps its own draft for its own address. An edit refused or kept over a body
  // another writer has changed since - one the comment no longer holds - is not put back in the
  // field, which opens on the newer body, and waits beside it (`changed`, below).
  const [initial] = useState(() => {
    const held = outgoing.held;
    const unsentHere = unsentName === undefined ? undefined : unsent.read(unsentName);
    const kept:
      | { readonly ask?: SentRequest["ask"]; readonly base?: string; readonly draft: SentDraft }
      | undefined =
      unsentHere === undefined
        ? awayName === undefined
          ? undefined
          : unsent.readAway(awayName, replyTo)
        : { draft: unsentHere };
    const heldElsewhere =
      held?.status === "refused" && !sameAddress(held.request, { anchor, edit, owner, replyTo });
    const seeded = seededDraft(seedMentions, undefined, owner);
    const fresh = {
      ask: undefined,
      away: false,
      body: edit?.body ?? seeded.body,
      changed: undefined,
      mentions: seeded.mentions,
      replacement: "",
      taken: false,
    };
    if (held !== undefined && !(heldElsewhere && kept !== undefined)) {
      if (
        held.status === "refused" &&
        edit !== undefined &&
        held.request.edit?.base !== edit.body
      ) {
        return { ...fresh, changed: held.draft.draft };
      }
      return {
        ask: held.draft.ask,
        away: false,
        body: held.draft.draft.body,
        changed: undefined,
        mentions: [...held.draft.draft.mentions],
        replacement: held.draft.draft.replacement,
        taken: true,
      };
    }
    if (kept !== undefined) {
      if (edit !== undefined && kept.base !== edit.body) return { ...fresh, changed: kept.draft };
      return {
        ask: kept.ask,
        away: unsentHere === undefined,
        body: kept.draft.body,
        changed: undefined,
        mentions: [...kept.draft.mentions],
        replacement: kept.draft.replacement,
        taken: true,
      };
    }
    return fresh;
  });
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
  // arrives at an address that keeps one (`shown`, below).
  const mountedHeld = useRef(initial.taken);
  const [body, setBody] = useState(initial.body);
  const [replacement, setReplacement] = useState(initial.replacement);
  const [mentions, setMentions] = useState<AcceptedMention[]>(initial.mentions);
  /** An edit written over a body the comment no longer holds, refused or kept for the comment
   *  while its editor was off the page: it waits beside the newer body the field shows until the
   *  reader takes it back into the field (`takeChanged`) or drops it (`discard`). Meanwhile the
   *  field is read only and Save refuses, so nothing is saved over a body the reader has not
   *  seen, and nothing they wrote goes with no word. */
  const [changed, setChanged] = useState<SentDraft | undefined>(initial.changed);
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
  const [askOptions, setAskOptions] = useState<AskOptionDraft[]>(() =>
    initial.ask === undefined || initial.ask.options.length === 0
      ? [emptyAskOption()]
      : initial.ask.options.map((option) => ({
          ...emptyAskOption(),
          description: option.description ?? "",
          label: option.label,
        }))
  );
  const [multiple, setMultiple] = useState(initial.ask?.multiple ?? false);
  const [urgency, setUrgency] = useState<AskUrgency>(initial.ask?.urgency ?? "med");
  const [referencePickerOpen, setReferencePickerOpen] = useState(false);
  const [autocomplete, setAutocomplete] = useState<{ query: string; start: number }>();
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  // Why the last kind pick was refused, kept with the mark it answered: the composer stays mounted
  // when a newer compose replaces the anchor, and a refusal about the old mark says nothing about
  // the new one. A quote anchor (a closed issue's document) has no mark.
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
  // An edit's body is the draft when the edit opens, and again whenever the comment it edits
  // changes - never over the held draft a mount took.
  const editedFrom = useRef(editBody);
  useEffect(() => {
    if (editBody === undefined || editedFrom.current === editBody) return;
    editedFrom.current = editBody;
    replaceDraft({ body: editBody, mentions: [] });
  }, [editBody, replaceDraft]);
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
  /** The refusal this composer holds, and whether its host has moved the composer off the address
   *  that refusal's draft lives at (`sameAddress`): another reply, a newer message. */
  const heldRefusal = outgoing.held?.status === "refused" ? outgoing.held : undefined;
  const refusedElsewhere =
    heldRefusal !== undefined &&
    !sameAddress(heldRefusal.request, { anchor, edit, owner, replyTo });
  /** What the field holds at this address before the reader writes anything: the comment an edit
   *  opens with, a reply's prefill, or what the channel seeds. */
  const freshBody =
    edit?.body ??
    (replyTo === null
      ? seededDraft(seedMentions, undefined, owner).body
      : replyPrefill(replyTo).body);
  /** Whether the field holds words the reader wrote for the address whose untouched field reads
   *  `fresh`: more than that, and other than a held refusal's own draft, which its held send
   *  keeps. Only such a draft is kept for an address the composer leaves. */
  const wroteOver = (fresh: string): boolean =>
    hasUnsavedInput(body === fresh ? "" : body, replacement, askOptions) &&
    (heldRefusal === undefined ||
      body !== heldRefusal.draft.draft.body ||
      replacement !== heldRefusal.draft.draft.replacement);
  useEffect(() => {
    // A mount that took a draft keeps it, and so does an arrival that put a kept draft in the
    // field (`shown`, below); while a send holds the field's draft - out, or refused - so does a
    // reply the host starts then: the field is that send's draft, and a refusal's stays at the
    // address it lives at when the host moves the composer (`elsewhere`), which the prefill of a
    // new reply would write over. The prefill is for a new reply.
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
    // A standing box's draft is its thread's (`unsentName`), not one message's: a newer message
    // the host moves the box onto leaves what the reader wrote in the field, where the prefill
    // would write over it.
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
  // Closed to this kind or finishing, with no send out and no refusal to show: nothing of this
  // composer is on screen.
  const dormant = (refusedKind || finishing) && !sending && refusal === undefined;
  /** How many times the reader has ended the reply here (Cancel reply, or Escape): a move of the
   *  address with the composer's own control, which the draft goes with (`ownMoves`). */
  const [cancels, setCancels] = useState(0);
  /** Set as the reader ends the reply here or drops the draft (`discard`), until a commit still
   *  draws this composer: a host that ends the composer with the reply - the phone thread
   *  composer, whose Cancel reply puts its draft in the docked one (`intake`) - has taken the
   *  draft, and a dropped one is gone, so nothing keeps it here as well. A host that unmounts the
   *  composer in the same press draws no commit in between, and the unmount must not keep the
   *  draft the last commit showed. */
  const keepsNothing = useRef(false);
  /** Ends the reply, handing the host the draft as the field holds it. The draft stays where it
   *  is kept - the field, the unsent draft under the send name, the held send - so nothing the
   *  reader wrote goes with no word: the host moves the reader, and an own move carries the
   *  draft's send with it (`ownMoves`). */
  const cancelReply = () => {
    keepsNothing.current = true;
    setCancels((count) => count + 1);
    onCancelReply?.({ body, mentions, replacement });
  };
  // The Escape listener below reads it as it stands, so it is not re-bound each render for it.
  const cancelReplyNow = useRef(cancelReply);
  cancelReplyNow.current = cancelReply;
  // A draft the reader carried here from another composer goes in the field only while nothing
  // of this composer's own is there to be written over (`DraftIntake`). The host's handle reads
  // the field as it stands, so it is made once.
  const takeDraft = useRef<DraftIntake["take"]>(() => false);
  takeDraft.current = (draft, to) => {
    if (
      holdsSend ||
      hasUnsavedInput(body, replacement, askOptions) ||
      !sameAddress({ anchor, edit, owner, replyTo }, { ...to, anchor: undefined, edit: undefined })
    ) {
      return false;
    }
    replaceDraft(draft);
    return true;
  };
  useImperativeHandle(intake, () => ({ take: (draft, to) => takeDraft.current(draft, to) }), []);
  // What the reader wrote over a refusal goes back to the held send (`keep`, which a send still
  // out ignores), so the composer that mounts next shows it - and so a reload does: a refusal's
  // draft is the only copy of that message, and the entry it is kept in is what survives the
  // page (`write-storage.ts`). It is written as this composer unmounts, a short wait after each
  // keystroke, and as the page goes, which no unmount follows. A refusal the host has moved the
  // composer off (`elsewhere`, below) keeps the draft it was written with: what is typed over it
  // there is for that other address. So does an edit's refusal while it waits beside a newer body
  // (`changed`): the field shows that body, not the reader's edit.
  const keepDraft = useRef(() => {});
  keepDraft.current = () => {
    if (elsewhere !== undefined || changed !== undefined) return;
    outgoing.keep({
      ask: { multiple, options: submittedAskOptions(askOptions), urgency },
      draft: { body, mentions, replacement },
    });
  };
  useEffect(() => () => keepDraft.current(), []);
  // What the reader wrote for this address and did not send waits for it once the composer leaves
  // the page (`awayName`): its host took the composer away - the tab, the Agents page, the
  // margin's document, a comment's card - and the composer drawn at this address next shows it.
  // An edit is kept with the comment's body the field started from, so it comes back only over
  // that body. A draft a held send keeps at its own address is that send's, one the reader's
  // Cancel reply handed to the host is wherever the host put it, and an edit waiting beside a
  // newer body stays kept as it was until the reader takes it back or drops it.
  const keepAway = useRef(() => {});
  keepAway.current = () => {
    if (
      awayName === undefined ||
      keepsNothing.current ||
      changed !== undefined ||
      (holdsSend && !refusedElsewhere)
    ) {
      return;
    }
    unsent.writeAway(
      awayName,
      replyTo,
      wroteOver(freshBody)
        ? {
            ask: { multiple, options: submittedAskOptions(askOptions), urgency },
            base: edit?.body,
            draft: { body, mentions, replacement },
            owner,
            replyTo,
          }
        : undefined
    );
  };
  useEffect(() => () => keepAway.current(), []);
  // biome-ignore lint/correctness/useExhaustiveDependencies: the draft's own fields are the trigger; the write reads them as they stand
  useEffect(() => {
    const write = window.setTimeout(() => keepDraft.current(), KEEP_DRAFT_MS);
    return () => window.clearTimeout(write);
  }, [askOptions, body, mentions, multiple, replacement, urgency]);
  useEffect(() => {
    const keepNow = () => keepDraft.current();
    window.addEventListener("pagehide", keepNow);
    return () => window.removeEventListener("pagehide", keepNow);
  }, []);
  // The unsent draft is written under its send name while no send of its own holds the draft, and
  // while the host has the composer away from a refusal's address (`refusedElsewhere`): what the
  // reader types there is that other address's, and the refusal keeps the draft it was written
  // with. From Send on the held send keeps it, so Send drops the unsent copy in its own task
  // (`send`): a composer drawn after that send landed must not bring back words already sent.
  const keepsUnsent = wroteOver("");
  useEffect(() => {
    if (unsentName === undefined || (holdsSend && !refusedElsewhere)) return;
    unsent.write(unsentName, keepsUnsent ? { body, mentions, replacement } : undefined);
  }, [body, holdsSend, keepsUnsent, mentions, refusedElsewhere, replacement, unsent, unsentName]);
  // The margin sends a refused margin compose again itself, once it has written the mark it is
  // anchored to again (`reanchor` in margin-context.tsx), and that retry carries the draft the
  // store holds. The writes above are a keystroke's wait behind what the reader has typed, so
  // the margin asks for it first: this is what answers. A composer with no name of its own
  // holds no send another surface can send again, so it registers nothing.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the name's hash is its identity; the key array itself is rebuilt by some hosts on every render
  useEffect(() => {
    if (mutationKey === undefined) return;
    return registerComposer({ keepDraft: () => keepDraft.current(), mutationKey });
  }, [composerName, registerComposer]);
  useEffect(() => {
    const form = formRef.current;
    if (form === null || dormant) return;
    const handleEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key !== "Escape") return;
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
      if (!hasUnsavedInput(body, replacement, askOptions) || confirmingDiscard) {
        onClose();
        return;
      }
      const active = document.activeElement;
      lastFocusedField.current =
        active instanceof HTMLElement && form.contains(active) ? active : null;
      setConfirmingDiscard(true);
    };
    form.addEventListener("keydown", handleEscape);
    return () => form.removeEventListener("keydown", handleEscape);
  }, [
    askOptions,
    autocomplete,
    body,
    confirmingDiscard,
    // A dormant composer renders no form; the one it renders when it wakes takes the listener.
    dormant,
    onClose,
    referencePickerOpen,
    replacement,
    replyTo,
  ]);
  // A composer fixed at the foot of the screen - the docked one below `sm`, or one a host's frame
  // places there, as a phone thread's are - says how far up it reaches, so the shell's floating
  // status sits above it rather than over its controls; a refusal's row grows it upward. One with
  // neither lays out in the page.
  useFootInset(
    frame === undefined ? formRef : frameRef,
    !dormant && (docked || frame !== undefined)
  );
  // A file's reference belongs to the draft it was pasted or dropped into, so each press carries
  // that draft (`useDraftUpload`), as an ask card's carries its ask: a number of this composer's
  // (`draftAt`), mapped to the owner the draft lives at. That is the owner the composer addressed
  // at the paste, and wherever the reader then takes the composer with its own controls, since
  // the draft goes with such a move (`ownMoves`, below). A host can move the composer to another
  // owner too - another row, a Reply on a message - and the field is then that owner's message,
  // where a reference to a file pasted into another draft does not belong. So what lands is kept
  // here by the draft it was pasted into, and joins the field while the composer addresses the
  // owner that draft lives at: at once, or once the composer is back there, and the composer says
  // so until then.
  const [draftHomes, setDraftHomes] = useState<ReadonlyMap<number, ArtifactOwner>>(() => new Map());
  const nextDraft = useRef(0);
  const [uploadedFiles, setUploadedFiles] = useState<
    readonly { readonly draft: number; readonly text: string }[]
  >([]);
  const upload = useDraftUpload((text: string, draft: number) =>
    setUploadedFiles((current) => [...current, { draft, text }])
  );
  // Where a pasted or dropped file goes: the issue or the project document the draft is for, or,
  // for a direct message, the agent's conversation. An edit takes no files.
  const uploadOwner: ArtifactOwner | undefined =
    edit !== undefined
      ? undefined
      : owner.kind === "issue"
        ? { issue: owner.issueKey }
        : owner.kind === "artifact"
          ? { project: owner.project }
          : { session: owner.sessionId };
  const uploadKey = uploadOwner === undefined ? undefined : JSON.stringify(uploadOwner);
  /** The draft a file pasted or dropped for `pastedFor` goes into: the one living there, or a new
   *  one. */
  const draftAt = (pastedFor: ArtifactOwner): number => {
    const key = JSON.stringify(pastedFor);
    for (const [draft, home] of draftHomes) {
      if (JSON.stringify(home) === key) return draft;
    }
    const draft = nextDraft.current;
    nextDraft.current += 1;
    setDraftHomes((current) => new Map(current).set(draft, pastedFor));
    return draft;
  };
  useEffect(() => {
    const here = uploadedFiles.filter(
      (file) => JSON.stringify(draftHomes.get(file.draft)) === uploadKey
    );
    if (here.length === 0) return;
    setUploadedFiles((current) => current.filter((file) => !here.includes(file)));
    setBody((current) => {
      const next = here.reduce((text, file) => appendToDraft(text, file.text), current);
      previousBody.current = next;
      return next;
    });
  }, [draftHomes, uploadedFiles, uploadKey]);
  /** What landed into a draft living at another owner than the composer addresses, by that
   *  owner. */
  const landedElsewhere = new Map<string, { owner: ArtifactOwner; files: number }>();
  for (const file of uploadedFiles) {
    const home = draftHomes.get(file.draft);
    const key = JSON.stringify(home);
    if (home === undefined || key === uploadKey) continue;
    landedElsewhere.set(key, { files: (landedElsewhere.get(key)?.files ?? 0) + 1, owner: home });
  }
  // A file goes to the owner its paste or drop named, and a refused upload's Retry makes that
  // press again (`DraftUpload.retry`): to that owner, and for the draft it was pasted into, which
  // the reader's own moves have taken along. A host can move the composer to another owner in
  // between, so the draft the newest press went into is kept, and while the composer addresses
  // another owner than the one that draft lives at the refusal offers no Retry here: it says
  // where the file was pasted, and its Retry is back once the composer is.
  const [uploadedFor, setUploadedFor] = useState<number>();
  const refusedHome =
    upload.isError && uploadedFor !== undefined ? draftHomes.get(uploadedFor) : undefined;
  const uploadElsewhere =
    refusedHome !== undefined && JSON.stringify(refusedHome) !== uploadKey
      ? refusedHome
      : undefined;

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
  // A refusal lives at the address its send went to, or the one the reader has carried it to
  // since (`sameAddress`; the own moves below). A host can move the composer to another while it
  // holds one - another row or thread, a Reply on a message - and from there neither Send nor
  // Retry is that refusal's Retry: Send says where the draft was written for, and Discard drops
  // it. The store refuses such a press too (`HeldSends.send`), whichever render made it.
  const elsewhere =
    heldRefusal !== undefined && refusedElsewhere
      ? writtenElsewhere(heldRefusal.request, standing)
      : undefined;
  // The reader's own moves of the composer's address - Cancel reply, and a control its host
  // draws for it (`ownMoves`) - take the draft with them, and what it holds: its refusal
  // (`HeldSends.carry`) and the files pasted into it (`draftHomes`). Each is counted in the press
  // that makes it, so a commit that shows another address with another count is the reader's
  // move. One that shows another address with the count where it was is the host's - another
  // row, another thread, a Reply on a message - and carries nothing: a refusal and a file stay
  // with the address they were made for, and the composer says so (`elsewhere`, `landedFor`,
  // `pastedElsewhere`). Only what lives where the composer was goes along, so a move away from an
  // address the host took the composer to leaves the draft where it was written.
  //
  // Each reply is an address of its own for what the field holds. A host's move to another reply
  // keeps the field's draft for the one it leaves - in its refusal, where it lives there, else
  // under the send name for that reply (`awayName`), which the composer says (`keptAwayLine`) -
  // and puts in the field what the reply it arrives at keeps: its refusal's draft, a draft kept
  // for it, the refusal the host moved it off, or else what a new reply starts with. The
  // reader's own move onto a reply that keeps a draft of its own - Cancel reply back to a
  // message kept for the issue, or to a refusal's own address - carries nothing either, as the
  // phone thread composer's Cancel reply carries nothing over the docked composer's own draft:
  // nothing is written over, and each draft stays where it was written.
  const moves = ownMoves + cancels;
  const shown = useRef<
    | {
        readonly address: Address;
        /** The address's untouched field (`freshBody`). */
        readonly fresh: string;
        readonly moves: number;
        readonly uploadOwner: ArtifactOwner | undefined;
      }
    | undefined
  >(undefined);
  useLayoutEffect(() => {
    const address: Address = { anchor, edit, owner, replyTo };
    const before = shown.current;
    shown.current = { address, fresh: freshBody, moves, uploadOwner };
    // A commit that still draws this composer after the reader's Cancel reply or Discard: the
    // host kept the composer, and what the field holds now is still this composer's to keep.
    keepsNothing.current = false;
    if (before === undefined) {
      // The field took the draft kept for this address, which is kept there no longer.
      if (initial.away && awayName !== undefined) unsent.writeAway(awayName, replyTo, undefined);
      return;
    }
    if (sameAddress(before.address, address)) return;
    const refusalAt = (at: Address) =>
      heldRefusal !== undefined && sameAddress(heldRefusal.request, at);
    // An owner or an anchor changing alone - an issue pick, a newer compose on the same
    // document - leaves the field's draft where it is.
    const replyMoved = (before.address.replyTo?.id ?? null) !== (replyTo?.id ?? null);
    const sendOut = holdsSend && heldRefusal === undefined;
    const kept =
      replyMoved && awayName !== undefined && !sendOut
        ? unsent.readAway(awayName, replyTo)
        : undefined;
    const arrivesAtOwn = replyMoved && !sendOut && (refusalAt(address) || kept !== undefined);
    if (before.moves !== moves && !arrivesAtOwn) {
      if (refusalAt(before.address)) outgoing.carry({ owner, replyTo });
      const left =
        before.uploadOwner === undefined ? undefined : JSON.stringify(before.uploadOwner);
      const to = uploadOwner;
      if (left === undefined || to === undefined || left === uploadKey) return;
      setDraftHomes((current) =>
        [...current.values()].some((home) => JSON.stringify(home) === left)
          ? new Map(
              [...current].map(
                ([draft, home]) => [draft, JSON.stringify(home) === left ? to : home] as const
              )
            )
          : current
      );
      return;
    }
    if (!replyMoved || sendOut) return;
    const leaving = { body, mentions, replacement };
    const ask = { multiple, options: submittedAskOptions(askOptions), urgency };
    if (refusalAt(before.address)) {
      // Written now, since the debounced write reads the address the composer shows by then.
      outgoing.keep({ ask, draft: leaving });
    } else if (awayName !== undefined) {
      unsent.writeAway(
        awayName,
        before.address.replyTo,
        wroteOver(before.fresh)
          ? { ask, draft: leaving, owner: before.address.owner, replyTo: before.address.replyTo }
          : undefined
      );
    }
    if (awayName === undefined) return;
    if (kept !== undefined) unsent.writeAway(awayName, replyTo, undefined);
    const next = refusalAt(address)
      ? heldRefusal?.draft.draft
      : (kept?.draft ??
        heldRefusal?.draft.draft ??
        (replyTo === null ? seededDraft(seedMentions, undefined, owner) : undefined));
    // Arriving at a reply that keeps nothing, with no refusal held, is a new reply: the prefill
    // above fills the field.
    if (next === undefined) return;
    replaceDraft(next);
    mountedHeld.current = true;
  });
  // A closed issue refuses a suggestion or an ask: the composer is only still here for a send of
  // its own that is out, or its refusal, and Send says why it cannot go. An edit waiting beside a
  // newer body is the reader's to take back or drop before anything is saved.
  const submitReason = refusedKind
    ? "This issue is closed. Reopen it to send."
    : (elsewhere ??
      (changed === undefined ? undefined : "Use your edit or discard it before you save.") ??
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
      setAutocomplete(undefined);
      setReferencePickerOpen(false);
      if (unsentName !== undefined) unsent.write(unsentName, undefined);
      return sentRequest(refused);
    });
  /** The hold on the draft from Send's own task on, before React can disable the fieldset: the
   *  fieldset's capture stops every key, click, input and change bound for a control in it - so
   *  Escape raises no Discard prompt and Ctrl+K opens no picker over a draft that is out. */
  const stopHeldComposerInput = (event: SyntheticEvent) => {
    if (!outgoing.sendingNow()) return;
    event.preventDefault();
    event.stopPropagation();
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (canSubmit) send();
  };
  /** Drops the draft and a refusal with it, and an edit waiting beside a newer body, which is the
   *  draft the reader wrote there. Offered only while no send is out - the prompt never shows
   *  then (`send` closes it, and Escape raises none), and a closed composer's Discard sits beside
   *  a refusal - so the send it resets has its answer: the refusal goes with the draft it was
   *  about. A host whose `onClose` unmounts the composer draws no commit with the empty field, so
   *  nothing is kept for this address as it goes (`keepsNothing`). */
  const discard = () => {
    keepsNothing.current = true;
    clearDraft();
    outgoing.discard();
    if (changed !== undefined && awayName !== undefined) {
      unsent.writeAway(awayName, replyTo, undefined);
    }
    setChanged(undefined);
    setConfirmingDiscard(false);
    onClose();
  };
  /** Takes an edit waiting beside a newer body back into the field, which the reader saves with
   *  their own press, having had the newer body before them. It is the field's draft from here
   *  on, kept over the body it now replaces should the host take the editor away again, and a
   *  refusal's Retry sends it. */
  const takeChanged = () => {
    if (changed === undefined) return;
    replaceDraft(changed);
    setChanged(undefined);
    if (awayName !== undefined) unsent.writeAway(awayName, replyTo, undefined);
  };
  /** An anchored composer's Close, which asks as Escape does: over a draft the reader has not
   *  sent - typed, or handed back by a refusal - it raises `Discard draft?` rather than dropping
   *  what they wrote with nothing said, and it closes at once over an empty field. */
  const closeOrAsk = () => {
    if (!hasUnsavedInput(body, replacement, askOptions) || confirmingDiscard) {
      onClose();
      return;
    }
    const active = document.activeElement;
    lastFocusedField.current =
      active instanceof HTMLElement && formRef.current?.contains(active) ? active : null;
    setConfirmingDiscard(true);
  };
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
  // Sessions that advertise no mode at all take no message in any mode: a session whose plugin
  // cannot reach its conversation says so by advertising none.
  const deaf =
    unsupported && unsupportedOptions.every((option) => option.capabilities.length === 0);
  // The plain mode, named whenever it is not Send, so the reader knows what their words become.
  const plainNotice =
    sendsPlain && plainMode !== "steer" && outboundMode === plainMode && outbound.body === body
      ? // A session that advertises no mode gets its own warning, not this line.
        targetOptions.filter(
          (option) => option.capabilities.length > 0 && !option.capabilities.includes("steer")
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
            excerpt={replyTo.excerpt}
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
              lastFocusedField.current?.focus();
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
            aria-label="Replacement"
            className={`mt-1 block w-full rounded-lg border px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
            onChange={(event) => {
              setReplacement(event.target.value);
              setConfirmingDiscard(false);
            }}
            onKeyDown={submitOnModifiedEnter}
            ref={replacementTextarea}
            value={replacement}
          />
        </label>
      ) : null}
      <label className={`block text-sm font-medium ${textSecondaryOnSurface}`}>
        {compact ? null : bodyLabel}
        <textarea
          aria-label={bodyLabel}
          className={`mt-1 block w-full rounded-lg border px-3 py-2 font-normal outline-none ${inline ? "min-h-11 resize-none" : "min-h-24"} ${inputClasses(true)} ${textPrimaryOnSurface}`}
          onChange={(event) => onBodyChange(event.target.value, event.target.selectionStart)}
          onDrop={
            uploadOwner === undefined
              ? undefined
              : (event: DragEvent<HTMLTextAreaElement>) => {
                  if (event.dataTransfer.files.length === 0) return;
                  const draft = draftAt(uploadOwner);
                  setUploadedFor(draft);
                  upload.drop(event, uploadOwner, draft);
                }
          }
          onInput={
            inline
              ? (event: SyntheticEvent<HTMLTextAreaElement>) => {
                  event.currentTarget.style.height = "auto";
                  event.currentTarget.style.height = `${event.currentTarget.scrollHeight}px`;
                }
              : undefined
          }
          onKeyDown={(event: KeyboardEvent<HTMLTextAreaElement>) => {
            if (
              owner.kind === "issue" &&
              !compact &&
              (event.ctrlKey || event.metaKey) &&
              event.key.toLowerCase() === "k"
            ) {
              event.preventDefault();
              setReferencePickerOpen(true);
              return;
            }
            submitOnModifiedEnter(event, { disabled: upload.pending > 0 });
          }}
          onPaste={
            uploadOwner === undefined
              ? undefined
              : (event: ClipboardEvent<HTMLTextAreaElement>) => {
                  if (event.clipboardData.files.length === 0) return;
                  const draft = draftAt(uploadOwner);
                  setUploadedFor(draft);
                  upload.paste(event, uploadOwner, draft);
                }
          }
          readOnly={changed !== undefined}
          ref={bodyFieldRef}
          rows={1}
          value={body}
        />
      </label>
      {/* An edit written over a body the comment no longer holds: the field shows the comment as
          it reads now, and the edit waits beside it with the words that say why, until the reader
          takes it back into the field or drops it. Nothing is saved meanwhile. */}
      {changed === undefined ? null : (
        <div className="space-y-2">
          <div className={`space-y-1 text-sm ${textMutedOnSurfaceMuted}`} role="status">
            <p>
              This comment changed since you wrote your edit. The field shows the comment as it
              reads now. Your edit is kept here:
            </p>
            <blockquote
              className={`border-l-2 pl-2 whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
            >
              {changed.body}
            </blockquote>
          </div>
          <div className="flex flex-wrap gap-3">
            <button
              className={`text-sm font-medium ${linkText} ${linkHoverText}`}
              onClick={takeChanged}
              type="button"
            >
              Use my edit
            </button>
            <button
              className={`text-sm font-medium underline ${dangerText}`}
              onClick={discard}
              type="button"
            >
              Discard my edit
            </button>
          </div>
        </div>
      )}
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
          it records a failed attempt. A session advertises none while its plugin cannot reach its
          conversation.
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
      {kind === "ask" ? (
        <>
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
                  <input
                    aria-label={`Option ${index + 1} label`}
                    className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
                    onChange={(event) => updateAskOption(index, "label", event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter") {
                        event.preventDefault();
                        addAskOption();
                      }
                    }}
                    placeholder="Label"
                    ref={(element) => {
                      optionLabelRefs.current[index] = element;
                    }}
                    value={option.label}
                  />
                  <input
                    aria-label={`Option ${index + 1} description`}
                    className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
                    onChange={(event) => updateAskOption(index, "description", event.target.value)}
                    placeholder="Description (optional)"
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
        </>
      ) : null}
      {compact ? null : (
        <>
          <p className={`text-xs ${textMutedOnSurfaceMuted}`}>
            Paste or drop a picture to show it inline, or any other file to attach it.
            {owner.kind === "issue" ? " Ctrl+K inserts a reference." : ""}
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
          {/* A picker is a draft control. `send` closes one already open, and Ctrl+K consults the
              submit guard before it can open another in the task before the fieldset disables. */}
          {owner.kind === "issue" && referencePickerOpen ? (
            <ReferencePicker
              issueKey={owner.issueKey}
              onClose={() => setReferencePickerOpen(false)}
              onSelect={(reference) => {
                setBody((current) => {
                  const next = appendToDraft(current, reference);
                  previousBody.current = next;
                  return next;
                });
                setReferencePickerOpen(false);
                textarea.current?.focus();
              }}
            />
          ) : null}
        </>
      )}
      {uploadOwner === undefined ? null : (
        // An upload refused for another owner is that owner's: its refusal shows here in words of
        // its own, with no Retry (`uploadElsewhere`), and the status shows the rest.
        <DraftUploadStatus
          upload={uploadElsewhere === undefined ? upload : { ...upload, isError: false }}
        />
      )}
      {uploadElsewhere === undefined ? null : (
        <QueryError message={pastedElsewhere(uploadElsewhere)} />
      )}
      {[...landedElsewhere].map(([key, { files, owner: pastedFor }]) => (
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} key={key} role="status">
          {landedFor(pastedFor, files)}
        </p>
      ))}
      {notice === undefined ? null : (
        <p className={`text-sm ${textMutedOnSurfaceMuted}`} role="status">
          {notice}
        </p>
      )}
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
              another address than the composer shows, so the refusal's own way out is to drop
              the draft it handed back, once the reader has it; a send the reader stopped offers
              both ways, and so does an inline reply's refusal, which its Cancel reply keeps for
              the thread rather than dropping. */}
          {refusedKind ||
          elsewhere !== undefined ||
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
