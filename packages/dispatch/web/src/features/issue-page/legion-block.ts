import type { LegionClaimState, LegionPhase } from "@legion/contracts/legion-api";

import type { LegionIssueResponse, LegionIssueWaitingOn } from "../../api/types";
import { buildIssuePath } from "../refs/routes";
import { CONTROL_LABELS, PHASE_WORDS, ROLE_STATE_WORDS } from "./page-words";

/**
 * The Legion block of Details (dispatch://INFRA-4870/spec#b-r-block), in the words of the
 * parent's "Words for the state" (dispatch://INFRA-1336/spec): the word of a phase, the word of
 * a claim state, the ending of a place in the line, and what the block draws of the issue's
 * Legion read (`legionBlockOf`). The phase words and the claim-state words are
 * `page-words-dashboard.ts`'s, which this module declares none of. Pure: no React and no DOM.
 */

/** The word of each phase the daemon names (`LEGION_PHASES`) and of `awaiting_merge`, the old
 *  value daemons before contract 86 send, which reads "Merging"; `null` for `done` and
 *  `unrecorded`, which draw no block. A phase the daemon adds fails the type check here until
 *  it has its word. */
const PHASE_WORD: Readonly<Record<LegionPhase | "awaiting_merge", string | null>> = {
  admitted: PHASE_WORDS.steps.admitted,
  awaiting_person: PHASE_WORDS.awaiting_person,
  awaiting_children: PHASE_WORDS.awaiting_children,
  planning: PHASE_WORDS.steps.planning,
  implementing: PHASE_WORDS.steps.implementing,
  testing: PHASE_WORDS.steps.testing,
  reviewing: PHASE_WORDS.steps.reviewing,
  accepting: PHASE_WORDS.steps.accepting,
  retro: PHASE_WORDS.steps.retro,
  merging: PHASE_WORDS.steps.merging,
  integrating: PHASE_WORDS.integrating,
  done: null,
  held: PHASE_WORDS.held,
  unrecorded: null,
  awaiting_merge: PHASE_WORDS.steps.merging,
};

/** The word of each claim state the daemon names (`LEGION_CLAIM_STATES`), and `null` for a
 *  `retired` claim, which is not listed. A state the daemon adds fails the type check here until
 *  it has its word. */
const CLAIM_STATE_WORD: Readonly<Record<LegionClaimState, keyof typeof ROLE_STATE_WORDS | null>> = {
  queued: "starting",
  launch_uncertain: "starting",
  launching: "starting",
  shim_connected: "starting",
  registered: "idle",
  ready: "idle",
  working: "in_turn",
  idle: "idle",
  suspended: "paused",
  failed: "failed",
  retired: null,
};

/** The word of the phase `phase`, "In another phase" for a phase the daemon's list does not
 *  hold, and undefined for `done` and `unrecorded`, which draw no block. */
export function phaseWord(phase: string): string | undefined {
  if (!Object.hasOwn(PHASE_WORD, phase)) return PHASE_WORDS.another;
  return PHASE_WORD[phase as keyof typeof PHASE_WORD] ?? undefined;
}

/** The word of the claim state `state`, "in another state" for a state the daemon's list does
 *  not hold, and undefined for a `retired` claim, which is not listed. */
export function claimStateWord(state: string): string | undefined {
  if (!Object.hasOwn(CLAIM_STATE_WORD, state)) return ROLE_STATE_WORDS.another;
  const word = CLAIM_STATE_WORD[state as LegionClaimState];
  return word === null ? undefined : ROLE_STATE_WORDS[word];
}

/** The ending English gives the place `place` ("st" of 1st, 21st and 101st, "th" of 11th to
 *  13th and 111th), which the block draws after the number: "3rd of 5 in line". */
export function ordinalEnding(place: number): string {
  const endings = CONTROL_LABELS.legion.ordinal_endings;
  const lastTwo = place % 100;
  if (lastTwo >= 11 && lastTwo <= 13) return endings.other;
  switch (place % 10) {
    case 1:
      return endings.one;
    case 2:
      return endings.two;
    case 3:
      return endings.three;
    default:
      return endings.other;
  }
}

/** The issue's documents, as `GET /api/v1/issues/{key}/artifacts` lists them, where a gate's
 *  document is found by its id; undefined while they are not read. */
export type LegionDocuments = readonly { readonly id: string; readonly slug: string }[] | undefined;

/** One part of the block's place and wait, in the order the block draws them: "At work", "3rd of
 *  5 in line", "Waits on a person", "Past review", "Not started", then "Waits for" and its keys. */
export type LegionPlacePart =
  | { readonly kind: "at_work" }
  | {
      readonly kind: "line";
      readonly place: number;
      readonly ending: string;
      readonly length: number;
    }
  | {
      readonly kind: "waits_on_person";
      /** The link of "Waits on a person", built from ids the block checked, or null. */
      readonly href: string | null;
      /** The wait's `kind`, `ask`, `artifact_id` and `gate_version` that the read holds, in that
       *  order, as text: the daemon chose each, so each is drawn as the daemon wrote it. */
      readonly members: readonly string[];
    }
  | { readonly kind: "past_review" }
  | { readonly kind: "not_started" }
  | { readonly kind: "waits_for"; readonly keys: readonly string[] };

/** One role with a claim, and the word of its claim's state. */
export interface LegionRoleLine {
  /** The page's word for the role (`collaborator_roles`), or the role's own text where it has
   *  none: `own` says which, since the role's own text is the daemon's. */
  readonly name: string;
  readonly own: boolean;
  readonly state: string;
}

/** What the block draws: the phase and since when, every role with a claim, the place and the
 *  wait, and the newest read of the daemon, or null where there is none. */
export interface LegionBlock {
  readonly phase: string;
  /** When Dispatch stored the phase, and whether Dispatch did not see it start ("since before"). */
  readonly since: { readonly at: string; readonly before: boolean };
  readonly roles: readonly LegionRoleLine[];
  readonly place: readonly LegionPlacePart[];
  /** When the newest read was made, and whether it failed. The read's error text is never here. */
  readonly read: { readonly at: string; readonly failed: boolean } | null;
}

/** An id the block links to: a UUID in its canonical form, of either case. */
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/iu;

/** The phases past review, which hold no slot: `awaiting_merge` is `merging` to an older daemon. */
const PAST_REVIEW: Readonly<Record<string, true>> = {
  accepting: true,
  awaiting_merge: true,
  merging: true,
  retro: true,
};

/** The page's word for each Legion role it names. */
const ROLE_WORDS: Readonly<Record<string, string>> = CONTROL_LABELS.collaborator_roles;

/** The wait of an issue in `awaiting_person` that no person answered yet, which the block draws
 *  as "Waits on a person"; undefined where the issue is in another phase or the wait is answered,
 *  and null where it waits on nothing the read names. */
function openWait(answer: LegionIssueResponse): LegionIssueWaitingOn | null | undefined {
  const state = answer.state;
  if (state?.phase !== "awaiting_person") return undefined;
  const wait = state.waiting_on;
  return wait?.answered_at === undefined ? wait : undefined;
}

/** The document and the version a gate wait names, where both pass their checks: an
 *  `artifact_id` that is a UUID and a `gate_version` that is a positive whole number. */
function checkedGate(
  wait: LegionIssueWaitingOn | null | undefined
): { readonly id: string; readonly version: number } | undefined {
  if (wait?.kind !== "gate") return undefined;
  const { artifact_id: id, gate_version: version } = wait;
  if (id === undefined || !UUID.test(id)) return undefined;
  if (version === undefined || !Number.isSafeInteger(version) || version < 1) return undefined;
  return { id, version };
}

/** Whether the block reads the issue's documents for its wait's link: only for a gate wait not
 *  answered whose `artifact_id` and `gate_version` pass their checks (the plan's D8). */
export function waitReadsDocuments(answer: LegionIssueResponse | undefined): boolean {
  return answer !== undefined && checkedGate(openWait(answer)) !== undefined;
}

/** The link of "Waits on a person", from the members of `wait` that pass their checks alone: an
 *  ask's `ask` that is a UUID, at the page's own issue `issueKey`, since the daemon's ask is on
 *  the root itself; a gate's document, found among `documents` by its id, at its version. No
 *  other member is ever a target, so a URL the daemon wrote never becomes one. */
function waitLink(
  wait: LegionIssueWaitingOn | null,
  issueKey: string,
  documents: LegionDocuments
): string | null {
  if (wait?.kind === "ask") {
    return wait.ask !== undefined && UUID.test(wait.ask)
      ? buildIssuePath({ id: wait.ask, key: issueKey, kind: "ask" })
      : null;
  }
  const gate = checkedGate(wait);
  const document =
    gate === undefined ? undefined : documents?.find((listed) => listed.id === gate.id);
  return gate === undefined || document === undefined
    ? null
    : buildIssuePath({
        key: issueKey,
        kind: "artifact",
        slug: document.slug,
        version: gate.version,
      });
}

/**
 * What the Legion block draws of `answer`, the issue's Legion read, on the page of the issue
 * `issueKey`, with `documents` its documents where the block read them; null where the block is
 * not drawn: a project that names no daemon, an issue the daemon does not record, and the phases
 * `done` and `unrecorded`.
 *
 * The place and the wait follow the parent's "Words for the state", read as the plan's table A
 * reads them. "At work" while the issue holds a slot; its place in the daemon's line where the
 * line names it; "Past review" in `accepting`, `retro` and `merging`; "Not started" in `admitted`
 * with none of these; and "Waits for" with the keys of the issues that hold it, in every phase.
 * `held` draws no place word. In `awaiting_person` the issue is never at work: it waits on a
 * person until the wait is answered, and then draws its place in line alone (the plan's D7).
 */
export function legionBlockOf(
  answer: LegionIssueResponse,
  issueKey: string,
  documents: LegionDocuments
): LegionBlock | null {
  const state = answer.state;
  if (!answer.configured || state === null) return null;
  const phase = phaseWord(state.phase);
  if (phase === undefined) return null;
  const held = state.phase === "held";
  const awaiting = state.phase === "awaiting_person";
  const wait = openWait(answer);
  const place: LegionPlacePart[] = [];
  if (!held && !awaiting && state.slot !== null) place.push({ kind: "at_work" });
  const line = state.waiting_line;
  if (!held && wait === undefined && line !== null) {
    place.push({ ending: ordinalEnding(line.place), kind: "line", ...line });
  }
  if (wait !== undefined) {
    place.push({
      href: waitLink(wait, issueKey, documents),
      kind: "waits_on_person",
      members:
        wait === null
          ? []
          : [wait.kind, wait.ask, wait.artifact_id, wait.gate_version]
              .filter((member) => member !== undefined)
              .map(String),
    });
  }
  if (Object.hasOwn(PAST_REVIEW, state.phase)) place.push({ kind: "past_review" });
  const blockers = state.blocked_by ?? [];
  if (state.phase === "admitted" && place.length === 0 && blockers.length === 0) {
    place.push({ kind: "not_started" });
  }
  if (blockers.length > 0) place.push({ keys: blockers, kind: "waits_for" });
  const roles: LegionRoleLine[] = [];
  for (const { role, state: claim } of state.roles ?? []) {
    const word = claimStateWord(claim);
    if (word === undefined) continue;
    const own = !Object.hasOwn(ROLE_WORDS, role);
    roles.push({ name: own ? role : (ROLE_WORDS[role] ?? role), own, state: word });
  }
  return {
    phase,
    place,
    read: answer.read === null ? null : { at: answer.read.at, failed: !answer.read.ok },
    roles,
    since: { at: state.phase_since, before: !state.phase_start_seen },
  };
}
