/**
 * The picks of the Issues tab's column filters, as the address keeps them, and the pick each row
 * files under. The table model's `COLUMNS` registry builds each column's filter from these, and
 * the views (`issues-views.ts`) compare a view's statuses with `sameSet`; this module imports
 * neither.
 */
import { LEGION_ROLES } from "@legion/contracts";
import type { Actor } from "../../api/types";
import { HOUR_MS } from "../../lib/closed-window";
import { isViewer } from "../refs/actor";
import { type LegionFacts, type LegionPhaseColumn, sinceMs, workPhaseOf } from "./issues-legion";

/** Whether `a` and `b` hold as many values, each value of `a` one of `b`'s: the same set of
 *  picks, in any order. */
export const sameSet = (a: readonly string[], b: readonly string[]): boolean =>
  a.length === b.length && a.every((value) => b.includes(value));

/** A pick of a row with no value, kept in the address as `none`: an empty value there would
 *  take the view's own value away rather than pick anything. */
export const NONE_PICK = "none";

/** The picks of a column that holds a length of time (`f.waited`, `f.time_in_phase`,
 *  `f.work_hours`), in their order: each length takes its lower bound and not its upper. */
export const DURATION_PICKS = ["under-1h", "1h-1d", "1d-7d", "over-7d", NONE_PICK] as const;
export type DurationPick = (typeof DURATION_PICKS)[number];

/** The pick a length of `ms` milliseconds falls in, None for no length. */
export function durationPick(ms: number | undefined): DurationPick {
  if (ms === undefined || Number.isNaN(ms)) return NONE_PICK;
  if (ms < HOUR_MS) return "under-1h";
  if (ms < 24 * HOUR_MS) return "1h-1d";
  return ms < 7 * 24 * HOUR_MS ? "1d-7d" : "over-7d";
}

/** The pick of the time from `since` to `nowMs` (Time waiting, Time in Legion phase), None
 *  where there is no time to start from. */
export function sinceDurationPick(since: string | null | undefined, nowMs: number): DurationPick {
  const at = sinceMs(since);
  return at === undefined ? NONE_PICK : durationPick(nowMs - at);
}

/** Review rounds' picks (`f.review_rounds`), in their order. */
export const REVIEW_ROUND_PICKS = ["0", "1", "2", "3-or-more", NONE_PICK] as const;
export type ReviewRoundsPick = (typeof REVIEW_ROUND_PICKS)[number];

/** The pick of a counted number of review rounds, None for an issue with no Legion work. */
export function reviewRoundsPick(rounds: number | undefined): ReviewRoundsPick {
  if (rounds === undefined) return NONE_PICK;
  if (rounds >= 3) return "3-or-more";
  return rounds >= 2 ? "2" : rounds >= 1 ? "1" : "0";
}

/** Legion tree's picks (`f.legion`): two of them select their union, so `f.legion=root` with
 *  `f.legion=child` is every issue with Legion work. */
export const LEGION_TREE_PICKS = ["root", "child", NONE_PICK] as const;
export type LegionPick = (typeof LEGION_TREE_PICKS)[number];

/** Where an issue stands in its Legion tree: the root, a child, or None with no Legion work. */
export function legionPick(facts: Pick<LegionFacts, "tree_depth"> | undefined): LegionPick {
  if (facts === undefined) return NONE_PICK;
  return facts.tree_depth === 0 ? "root" : "child";
}

/** Legion phase's picks (`f.legion_phase`) over `phases`, the board's columns: their ids, in the
 *  board's order, then None for an issue with no card. */
export function legionPhasePicks(phases: readonly LegionPhaseColumn[]): readonly string[] {
  return [...phases.map((phase) => phase.id), NONE_PICK];
}

/** The pick of the board column an issue's card is in, None with no card. */
export function phaseColumnPick(column: string | undefined): string {
  return column === undefined || column === "" ? NONE_PICK : column;
}

/** Workers' picks (`f.roles=implementer`): each Legion role's id in `LEGION_ROLES`' order, and
 *  None. */
export const ROLE_PICKS: readonly string[] = [...LEGION_ROLES, NONE_PICK];

/** The picks of the roles at work on a card, None where none is. */
export function workerPicks(roles: readonly string[] | null | undefined): readonly string[] {
  return roles === null || roles === undefined || roles.length === 0 ? [NONE_PICK] : roles;
}

/** Why it waits' picks (`f.waiting_reason=ready`): the board's `waiting.reason` values, in
 *  their order (Ready to start, Capacity to go on, A blocker, A person), and None. */
export const WAITING_REASON_PICKS = ["ready", "slot", "blocked", "person", NONE_PICK] as const;

/** The pick of why a card waits, None where it does not. A reason this build does not know is
 *  its own pick, under the board's word. */
export function waitingReasonPick(waiting: { readonly reason: string } | null | undefined): string {
  return waiting === null || waiting === undefined || waiting.reason === ""
    ? NONE_PICK
    : waiting.reason;
}

/** Worker state's picks (`f.activity`): the board's `slot_use` values, in their order (Working,
 *  Between turns, Waiting on child issues, Waiting on a person, Idle), and None. */
export const WORKER_STATE_PICKS = [
  "working",
  "between_turns",
  "held_children",
  "held_person",
  "held_idle",
  NONE_PICK,
] as const;

/** The pick of what a card's slot is used for, None with no slot of its own. */
export function activityPick(slotUse: string | null | undefined): string {
  return slotUse === null || slotUse === undefined || slotUse === "" ? NONE_PICK : slotUse;
}

/** Open asks' picks (`f.asks`): With open asks and No open asks. */
export const ASKS_PICKS = ["any", NONE_PICK] as const;
export type AsksPick = (typeof ASKS_PICKS)[number];

/** The pick of an issue's count of open asks. */
export function asksPick(openAsks: number): AsksPick {
  return openAsks > 0 ? "any" : NONE_PICK;
}

/** An `f.asks` value as the filter keeps it. The filter once picked a count, so a link of then
 *  keeps its rows: `f.asks=0` reads as None, and a count above 0 as With open asks. */
export function readAsksPick(value: string): string {
  if (!/^\d+$/.test(value)) return value;
  return Number(value) > 0 ? "any" : NONE_PICK;
}

/** The picks of an issue's hours by phase (`f.phase_hours=<phase>`): each phase Time per phase
 *  draws that has hours, Other for the hours of Other and of a key it does not draw, and no None,
 *  so a row with no hours is in no pick. Several picks select the rows with hours in any of
 *  them. */
export function phaseHoursPicks(
  hours: Readonly<Record<string, number>> | null | undefined
): readonly string[] {
  const picks = new Set<string>();
  for (const [phase, spent] of Object.entries(hours ?? {}))
    if (spent > 0) picks.add(workPhaseOf(phase));
  return [...picks];
}

/** Queue's picks (`f.queue`), the words today's links carry: In Legion queue, Not in Legion
 *  queue. */
export const QUEUE_PICKS = ["Queued", "Not queued"] as const;

/** The Created by pick that names the reader who opens the page (`f.author=@me`). */
export const ME = "@me";

/** Whether an issue's creator is the reader: a person whose login is the reader's, or an agent
 *  session whose `owner` is the reader. `author` is the issue's creator as the table reads it
 *  (`createdBy`). A saved link with `@me` means the reader who opens it. */
export function createdByReader(author: Actor | null, reader: string | undefined): boolean {
  if (author === null || reader === undefined) return false;
  if (author.kind === "session") {
    return author.owner !== undefined && author.owner.toLowerCase() === reader.toLowerCase();
  }
  return isViewer(author, reader);
}
