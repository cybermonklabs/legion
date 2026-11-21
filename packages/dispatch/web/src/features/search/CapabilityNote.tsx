import type { ReactNode } from "react";

import type { GoingCapability } from "../../api/types";
import { borderDefault } from "../../theme/classes";
import { barNoteClassName } from "./offer";

/** One thing the bar cannot do on this Dispatch, with the test id a check reads it by. */
export interface CapabilityGap {
  readonly id: string;
  readonly text: string;
}

/** What the bar cannot do on this Dispatch, one line for each part the capability read says is
 *  missing. */
export function capabilityGaps(capability: GoingCapability): readonly CapabilityGap[] {
  const gaps: CapabilityGap[] = [];
  if (!capability.answer.configured) {
    gaps.push({
      id: "going-cannot-answer",
      text: "No answering model is configured, so the bar searches and answers nothing.",
    });
  } else if (capability.answer.blocks === false) {
    gaps.push({
      id: "going-no-blocks",
      text: "The answering model refused structured output, so answers come without blocks.",
    });
  }
  if (!capability.decision.configured) {
    gaps.push({
      id: "going-cannot-decide",
      text: "No deciding model is configured, so the bar runs no action for you.",
    });
  } else if (!capability.decision.reachable) {
    gaps.push({
      id: "going-cannot-decide",
      text: "The deciding model cannot be reached now, so the bar runs no action for you.",
    });
  }
  if (!capability.session.configured) {
    gaps.push({
      id: "going-no-session",
      text: "No session is named for a new issue, so an issue the bar files starts no conversation.",
    });
  }
  if (!capability.label.configured) {
    gaps.push({
      id: "going-no-label",
      text: "No label is named for a new issue, so an issue the bar files carries none.",
    });
  }
  return gaps;
}

/**
 * What the bar cannot do on this Dispatch: the parts the capability read says are missing
 * (`gaps`, from `capabilityGaps`) and, while the search the list shows ran on words alone (its
 * answer's `degraded`), that meaning search is not running. It is said once, in the bar's foot
 * under the hits, the answer and the line under the list, from the moment the bar opens: no answer
 * carries it, so a second answer brings no second copy, and nothing of it stands between the box
 * and the hits or under an answer to push either down. A Dispatch with every part configured
 * whose meaning search runs says nothing here.
 */
export function CapabilityNote({
  degraded,
  gaps,
}: {
  /** Whether the search the list shows ran on words alone: its answer's `degraded`. */
  readonly degraded: boolean;
  readonly gaps: readonly CapabilityGap[];
}): ReactNode {
  if (!degraded && gaps.length === 0) {
    return null;
  }
  return (
    <ul
      aria-label="What this bar cannot do here"
      className={`${barNoteClassName} flex flex-wrap gap-x-2 gap-y-0.5 border-t ${borderDefault}`}
      data-testid="going-capability"
    >
      {degraded ? (
        // A status, since it comes and goes with the search the list shows.
        <li data-testid="search-degraded">
          <span role="status">
            Searching words only: meaning search is not running, so a hit that uses other words may
            be missing.
          </span>
        </li>
      ) : null}
      {gaps.map((gap) => (
        <li data-testid={gap.id} key={gap.id}>
          {gap.text}
        </li>
      ))}
    </ul>
  );
}
