import type { ReactNode } from "react";

import { usePageWords } from "../../components/tooltips";
import { pageInkText, pageQuietText, textMutedOnSurface } from "../../theme/classes";
import { ReferenceLink } from "../refs/ReferenceLink";
import { buildDispatchReference } from "../refs/routes";

/**
 * Where an inherited priority comes from, drawn after the level: " · from ACME-1", the key a link
 * to that issue with the preview every reference shows, so "P0 · from ACME-1" reads whole. It
 * draws nothing where the priority is the issue's own or there is none (`PriorityView.from`).
 * Inside `NoTooltips` the words are the page's (`CONTROL_LABELS.priority_from`) in its quiet
 * grey, and the key is the author's text in the page's ink, as the components line draws the
 * issue its set is inherited from.
 */
export function PrioritySource({
  className = "",
  from,
}: {
  /** Classes of the line beside the level, for a host that lays it out. */
  className?: string;
  from: string | null;
}): ReactNode {
  const page = usePageWords();
  if (from === null) return null;
  const reference = buildDispatchReference({ key: from, kind: "issue" });
  return (
    <span
      className={`${className} ${page === null ? textMutedOnSurface : pageQuietText}`}
      {...page?.pageChrome}
    >
      {" · "}
      {page?.CONTROL_LABELS.priority_from ?? "from"}{" "}
      {page === null ? (
        <ReferenceLink reference={reference} />
      ) : (
        <span {...page.authorText}>
          <ReferenceLink
            className={`dispatch-inline-link underline ${pageInkText} [overflow-wrap:anywhere]`}
            reference={reference}
          />
        </span>
      )}
    </span>
  );
}
