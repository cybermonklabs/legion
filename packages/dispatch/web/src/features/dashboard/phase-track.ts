import type { LegionPhase } from "@legion/contracts/legion-api";

import type { LegionIssueResponse, LegionIssueState } from "../../api/types";
import { PHASE_WORDS } from "../issue-page/page-words";

/**
 * Dashboard's phase track (dispatch://INFRA-5702/spec#b-r-track): which step of the eight the
 * issue's Legion state marks, and which words stand beside the track. Pure: it reads the state
 * `legionIssueQuery` holds and draws nothing.
 */

/** The eight steps of the track, in R3's order. */
export const TRACK_STEPS = [
  "admitted",
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "retro",
  "merging",
] as const satisfies readonly (keyof typeof PHASE_WORDS.steps)[];

export type TrackStep = (typeof TRACK_STEPS)[number];

/** What a phase draws on the track: the step it marks, absent where it marks none, and the words
 *  beside the track, absent where it draws none. */
export interface PhaseTrack {
  readonly marked?: TrackStep;
  readonly words?: string;
}

/** A Legion state that names a phase. */
type RunningLegion = LegionIssueResponse & { readonly state: LegionIssueState };

/** The phases that end the track: an issue at one of them is no issue Legion runs (R3). */
const ENDED_PHASES: readonly string[] = ["done", "unrecorded"] satisfies LegionPhase[];

/** Each phase the track draws, by its word as the daemon and Dispatch write it: every phase of
 *  the daemon's list but the two that end the track, and `awaiting_merge`, which a daemon before
 *  its contract 86 reports and Dispatch still names (`legion/state.go`). A phase the daemon adds
 *  fails the type check here until the track says what it draws. */
const PHASE_MARKS: Readonly<
  Record<Exclude<LegionPhase, "done" | "unrecorded"> | "awaiting_merge", PhaseTrack>
> = {
  admitted: { marked: "admitted" },
  awaiting_person: { marked: "admitted", words: PHASE_WORDS.awaiting_person },
  awaiting_children: { marked: "admitted", words: PHASE_WORDS.awaiting_children },
  planning: { marked: "planning" },
  implementing: { marked: "implementing" },
  testing: { marked: "testing" },
  reviewing: { marked: "reviewing" },
  accepting: { marked: "accepting" },
  retro: { marked: "retro" },
  merging: { marked: "merging" },
  integrating: { marked: "merging", words: PHASE_WORDS.integrating },
  awaiting_merge: { marked: "merging" },
  held: { words: PHASE_WORDS.held },
};

function isMarkedPhase(phase: string): phase is keyof typeof PHASE_MARKS {
  return Object.hasOwn(PHASE_MARKS, phase);
}

/** Whether Legion runs the issue (R3): its Legion state reads `configured` true, a `state` that
 *  is not null, and a phase that is neither `done` nor `unrecorded`. Only such an issue draws the
 *  track and the agents at work. */
export function legionRuns(legion: LegionIssueResponse | undefined): legion is RunningLegion {
  return (
    legion?.configured === true &&
    legion.state !== null &&
    !ENDED_PHASES.includes(legion.state.phase)
  );
}

/** The track the issue's Legion state draws, or undefined where the issue is no issue Legion
 *  runs and Dashboard draws no track. A phase the track does not name marks no step and reads
 *  "In another phase". */
export function phaseTrack(legion: LegionIssueResponse | undefined): PhaseTrack | undefined {
  if (!legionRuns(legion)) return undefined;
  const { phase } = legion.state;
  return isMarkedPhase(phase) ? PHASE_MARKS[phase] : { words: PHASE_WORDS.another };
}
