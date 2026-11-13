import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { issueCoverageQuery } from "../../api/queries";
import type { RequirementCoverage } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { pillShapeClassName } from "../../components/Pill";
import {
  card,
  dangerText,
  inlineWarningText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { BlockReferences } from "../refs/ReferencedBy";
import { authorText, pageChrome } from "./page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  coverageGapKind,
  EMPTY_STATE_LINES,
  GAP_WORDS,
  type GapKind,
  READING_LINES,
} from "./page-words";

function Requirement({ requirement }: { requirement: RequirementCoverage }): ReactNode {
  // What the server says this requirement lacks, read as the page's own gaps. Of the two lacks
  // the coverage read carries, only the one the page draws on a node is drawn here: that no
  // check serves it (R10). A requirement no plan task serves is no gap the page draws.
  const gaps = (requirement.gaps ?? []).flatMap((line): GapKind[] => {
    const kind = coverageGapKind(line);
    return kind === undefined ? [] : [kind];
  });
  return (
    <li
      className={`space-y-2 rounded-lg p-3 ${card}`}
      data-covered={gaps.length === 0}
      data-testid="spec-requirement"
    >
      {/* The requirement's own words are the one part of this line that can hold a run no
          space breaks - a path, a key, an address - so the flex child gives its width up
          (`min-w-0`) and the run wraps inside it (`break-words`), the shape the summary
          sentence and the focused node's facts already take (R17). Without both, a 118
          character path drew 922 px inside a 264 px card and the whole page scrolled
          sideways: 950 px in a 320 px viewport. */}
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className={`min-w-0 break-words ${textPrimaryOnSurface}`} {...authorText}>
          {requirement.text}
        </span>
        {gaps.map((gap) => (
          <span
            className={`${pillShapeClassName} ${surfaceMutedBg} ${inlineWarningText}`}
            data-testid="spec-requirement-gap"
            key={gap}
            {...pageChrome}
          >
            {GAP_WORDS[gap]}
          </span>
        ))}
      </div>
      <BlockReferences reference={requirement.ref} />
    </li>
  );
}

/**
 * The requirements of the issue's spec, each flagged with what it lacks
 * (`GET /api/v1/issues/{key}/coverage`). It sits under the spec on the Spec tab, where the
 * document itself draws the requirement's words: here they carry the flag and the Referenced-by
 * list, which the document's own drawing has nowhere to put.
 */
export function SpecRequirements({ issueKey }: { issueKey: string }): ReactNode {
  const coverage = useQuery(issueCoverageQuery(issueKey));

  if (coverage.isPending) {
    return (
      <p className={textMutedOnCanvas} data-testid="spec-requirements-loading" {...pageChrome}>
        {READING_LINES.coverage_loading}
      </p>
    );
  }
  if (coverage.isError) {
    return (
      <p className={dangerText} data-testid="spec-requirements-error" {...pageChrome}>
        {ANSWER_LINES.coverage_failed}
      </p>
    );
  }
  const requirements = coverage.data.requirements ?? [];
  if (requirements.length === 0) {
    return (
      <div data-testid="spec-no-requirements" {...pageChrome}>
        <EmptyState label="Requirements empty state" message={EMPTY_STATE_LINES.spec_marks_none} />
      </div>
    );
  }

  return (
    <section className="space-y-2" data-testid="spec-requirements">
      <h2 className={`text-sm font-semibold ${textMutedOnSurface}`} {...pageChrome}>
        {CONTROL_LABELS.requirements}
      </h2>
      <ul aria-label={CONTROL_LABELS.requirements} className="space-y-3">
        {requirements.map((requirement) => (
          <Requirement key={requirement.block_id} requirement={requirement} />
        ))}
      </ul>
    </section>
  );
}
