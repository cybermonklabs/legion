import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useRef } from "react";

import type { Artifact } from "../../api/types";
import { pageMutedText, pageQuietText } from "../../theme/classes";
import { drawsEdge } from "../refs/ReferencedBy";
import { useKnownReferences } from "../refs/reference-names";
import { referencesQuery, standsFailed, useNewIssuePageFreshness } from "../refs/reference-target";
import { buildDispatchReference, documentRoute } from "../refs/routes";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "./details-error";
import { ReferenceList, useIssueItemWords } from "./node-references/NodeReferences";
import { referenceRows } from "./node-references/references-list";
import { PROPERTY_LABELS, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

/**
 * What references a document opened in place of the outline (part N's R2 and R12), drawn open
 * under its own heading: the rows of part M's `referenceRows`, drawn by M's own list of them
 * (`ReferenceList`), 20 at a time with the control under them that says how many are left and
 * draws the next 20 (`useListPages`, root R40). Each citing node is named in the page's words as
 * Details' 'What references this row' names it: a question, a comment or a message of the
 * document's own issue by its words, which the outline's reads hold (`useIssueItemWords`), and
 * every other node by the word of its tie or of its kind, with its issue and its time. The read's
 * excerpt is the markup its author typed, and is drawn nowhere.
 *
 * Unlike Details' list, this one keeps what the issue's own questions, comments and messages cite
 * of the document: no outline stands beside an opened document to hold them.
 *
 * The read is `referencesQuery` of the document's address, under the key every Referenced-by panel
 * reads, kept fresh by events as the new page's reads are (`useNewIssuePageFreshness`), and its
 * edges are those every list of a node's references draws (`drawsEdge`). The list is drawn on
 * load, so it says nothing while its read is out.
 */
export function DocumentReferences({ artifact }: { artifact: Artifact }): ReactNode {
  const freshness = useNewIssuePageFreshness();
  const query = useQuery({
    ...referencesQuery(buildDispatchReference(documentRoute(artifact))),
    ...freshness,
  });
  // The issue the document belongs to: its own citing items are named by their words and draw no
  // issue beside them, and every other node says which issue it is of.
  const issueKey = artifact.issue_key ?? undefined;
  const itemWords = useIssueItemWords(issueKey, true);
  // The names the page's one batched read gave what a citing item's words cite, and which of them
  // name a post its author edited: a context the page publishes, so no request.
  const { edited, names } = useKnownReferences();
  const rows = useMemo(
    () =>
      referenceRows(
        itemWords,
        (query.data?.edges ?? []).filter(drawsEdge),
        names,
        issueKey,
        edited
      ),
    [edited, issueKey, itemWords, names, query.data]
  );
  // Retry goes with its line once the read it sent lands, and the focus it held goes to the
  // heading rather than fall to the page (`useRetryHandOff`).
  const heading = useRef<HTMLHeadingElement>(null);
  const retry = useRetryHandOff(() => heading.current);
  return (
    <section aria-label={PROPERTY_LABELS.referenced_by} className="space-y-2">
      <h3
        className={`text-sm font-semibold ${pageMutedText}`}
        ref={heading}
        tabIndex={-1}
        {...pageChrome}
      >
        {PROPERTY_LABELS.referenced_by}
      </h3>
      {standsFailed(query) ? (
        <DetailsErrorLine
          error={query.error}
          failure={query.errorUpdateCount}
          message={ANSWER_LINES.references_unreadable}
          onRetry={retryRead(query)}
          {...retry}
          retrying={query.isFetching}
        />
      ) : null}
      {query.data !== undefined && rows.length === 0 ? (
        <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
          {CONTROL_LABELS.none}
        </p>
      ) : null}
      {rows.length === 0 ? null : <ReferenceList issueKey={issueKey} rows={rows} />}
    </section>
  );
}
