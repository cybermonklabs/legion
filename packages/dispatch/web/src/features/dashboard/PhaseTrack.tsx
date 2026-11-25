import type { ReactNode } from "react";

import type { LegionIssueResponse } from "../../api/legion-issue";
import type { HandoffSummary } from "../../api/types";
import {
  pageHairBorder,
  pageInkBorder,
  pageInkText,
  pageMutedText,
  pageQuietText,
  pageStateText,
} from "../../theme/classes";
import { DASHBOARD_WORDS, PHASE_WORDS } from "../issue-page/page-words";
import { phaseTrack, TRACK_STEPS, type TrackStep } from "./phase-track";
import { roundCounts } from "./round-counts";

/**
 * Dashboard's first part (dispatch://INFRA-5702/spec#b-r-track): the eight steps of Legion's
 * track, the step the issue's Legion state gives marked, Planning, Implementing, Testing and
 * Reviewing each with its count of rounds where it has one, and the words a phase draws beside
 * the track. An issue Legion does not run draws none of it. No step is a control
 * (dispatch://INFRA-5707 adds the press), and the track lists no handoff.
 */
export function PhaseTrack({
  handoffs,
  legion,
}: {
  handoffs: readonly Pick<HandoffSummary, "phase">[] | undefined;
  legion: LegionIssueResponse | undefined;
}): ReactNode {
  const track = phaseTrack(legion);
  if (track === undefined) return null;
  const rounds: Readonly<Partial<Record<TrackStep, number>>> = roundCounts(handoffs);
  return (
    <section aria-label={DASHBOARD_WORDS.parts.phase} className="space-y-2">
      <h2 className={`text-xs font-semibold tracking-wide uppercase ${pageQuietText}`}>
        {DASHBOARD_WORDS.parts.phase}
      </h2>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <ol className="flex flex-wrap gap-1.5">
          {TRACK_STEPS.map((step) => {
            const marked = track.marked === step;
            const count = rounds[step];
            return (
              <li
                aria-current={marked ? "step" : undefined}
                className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 text-xs leading-6 ${
                  marked
                    ? `${pageInkBorder} font-semibold ${pageInkText}`
                    : `${pageHairBorder} ${pageMutedText}`
                }`}
                key={step}
              >
                {PHASE_WORDS.steps[step]}
                {count === undefined ? null : (
                  <>
                    {" "}
                    <span className={`tabular-nums ${pageQuietText}`}>{count}</span>
                  </>
                )}
              </li>
            );
          })}
        </ol>
        {track.words === undefined ? null : (
          <p className={`text-sm font-medium ${pageStateText.waiting}`}>{track.words}</p>
        )}
      </div>
    </section>
  );
}
