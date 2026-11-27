import type { ReactNode } from "react";

import type { Decision } from "../../api/types";
import { DecisionCard, type DecisionCardProps } from "./DecisionCard";
import { useDecisionWrites } from "./useDecisionWrites";

/** A decision waiting for the reader's review, drawn by the card with Keep and Change: the writes
 *  today's Decisions tab makes (`useDecisionWrites`), each named after the heading's plain words
 *  (`heading.words`). Every surface that writes a decision of the bare `decisions` read - the tab,
 *  its comments margin and Dashboard - draws it with this. */
export function WritableDecisionCard({
  documentId,
  issueKey,
  model,
  ...props
}: DecisionCardProps & {
  /** The issue's decisions document, which both writes name. */
  documentId: string;
  issueKey: string;
  /** The decision as the decisions read holds it, which both writes are made of. */
  model: Decision;
}): ReactNode {
  const { change, keep } = useDecisionWrites({
    decision: model,
    documentId,
    issueKey,
    name: props.heading.words,
  });
  return <DecisionCard {...props} change={change} issueKey={issueKey} keep={keep} />;
}
