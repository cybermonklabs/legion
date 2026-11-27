import { createContext, type ReactNode, useContext } from "react";

import type { IssueSummary } from "../../api/types";
import {
  CappedGroup,
  type CappedGroupOpenState,
  type OpenedGroups,
} from "../../components/CappedGroup";
import { LabelPill } from "../../components/Pill";
import { TruncatedText } from "../../components/TruncatedText";

/** Which issues' labels the reader opened past their "+N", by issue key (`useOpenedGroups`). The
 *  table or the board holds it, not the labels: the table draws only the rows in its window, so a
 *  row that scrolls out unmounts its cell, and the board draws a card again when it moves between
 *  columns or lanes; labels a reader opened would fold again either way. */
export const OpenedLabelsContext = createContext<OpenedGroups | undefined>(undefined);

/** An issue's labels as a list row or a card reads them, one group: a list as long as the issue's
 *  data, so the first ten, then "+N", wrapping inside the group. A label with no break in it
 *  shortens to an ellipsis inside its own pill, with the whole label as its title. Which issues'
 *  labels are open is the `OpenedLabelsContext` holder's, where there is one. Read-only: the
 *  issue page's labels, which a reader edits, are `features/issue/IssueLabels.tsx`. */
export function LabelsGroup({
  className,
  issue,
}: {
  /** The group's own box: a flex row that wraps, bounded by its host. */
  className: string;
  issue: Pick<IssueSummary, "key" | "labels">;
}): ReactNode {
  const held = useContext(OpenedLabelsContext);
  const labels = issue.labels ?? [];
  if (labels.length === 0) return null;
  const openState: CappedGroupOpenState = held === undefined ? {} : held.stateFor(issue.key);
  return (
    <CappedGroup
      className={`${className} [&>span]:min-w-0 [&>span]:max-w-full`}
      label="labels"
      name="labels"
      {...openState}
    >
      {labels.map((label) => (
        <LabelPill key={label}>
          <TruncatedText className="min-w-0" title={label}>
            {label}
          </TruncatedText>
        </LabelPill>
      ))}
    </CappedGroup>
  );
}
