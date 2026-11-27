import type { Decision } from "../../api/types";

/**
 * One decision of the bare decisions read, built by hand, which the decision card's own tests
 * read (`DecisionCard.test.tsx`, `adapters.test.ts`, `change-anchor.test.ts`) and the Decisions
 * filter's fixtures and tests import from here
 * (`issue-page/decisions-filter/decisions-fixtures.ts` exports none of it). It lives here, so no
 * file under `features/decision-card/` imports one under `features/issue-page/` (R22).
 */

export const KEY = "CORE-1";
export const DECISIONS_SLUG = "decisions-md";

/** The address of one decision, which is one block of the issue's decisions document. */
export const decisionRef = (block: string) =>
  `dispatch://${KEY}/artifact/${DECISIONS_SLUG}#b-${block}`;

/** One decision of the decisions document, as the decisions read answers it: an agent's, which
 *  waits for review unless the case says otherwise. */
export function decision(block: string, partial: Partial<Decision> = {}): Decision {
  return {
    asks: [],
    block_id: block,
    changed_at: "2026-11-10T12:00:00Z",
    changed_version: 1,
    heading: `The decision ${block}`,
    kept: null,
    kind: "agent",
    links: [],
    ref: decisionRef(block),
    replaced: false,
    text: `The decision ${block}.`,
    text_sha256: `sha256:${block}`,
    unsure: false,
    waiting_review: true,
    ...partial,
  };
}
