import { useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { api } from "../../api/client";
import { replaceQueryData } from "../../api/query-client";
import type { Decision, IssueDecisions } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import {
  anchorQuote,
  type ChangeAnchor,
  changeCommentInput,
  keptDecisions,
  readChangeAnchor,
} from "./change-anchor";
import type { CardChangeAct, CardKeepAct } from "./DecisionCard";
import { DECISION_ACT_WORDS } from "./decision-card-words";

/**
 * Keep and Change on a decision of the bare `decisions` read (`issueDecisionsQuery`), as the
 * card's caller hands them to it: the writes today's Decisions tab makes, which its comments
 * margin makes too (R19, R21).
 *
 * Keep is `POST /api/v1/artifacts/{id}/blocks/{block_id}/keep` with the sum of the text the card
 * showed, so a decision the agent changed since is refused (`409 DECISION_CHANGED`) rather than
 * agreed with. Its answer is written over any read of the decisions still out, which was asked
 * before the Keep and would put the decision back as waiting (`replaceQueryData`); every card
 * that reads the decisions from that key settles with it (`keptDecisions`), two copies of one
 * decision among them.
 *
 * Change is a comment of the issue anchored to the decision's words in the decisions document
 * (`changeCommentInput`), as `DecisionActions.tsx` writes it: the send reads the document's text
 * once, and counts which of the quote's places is the decision's own over its reading
 * (`readChangeAnchor`). With no reading - the read failed - the anchor names no place.
 * `undefined` for a decision whose heading is all it holds, which has no words to anchor on.
 */
export function useDecisionWrites({
  decision,
  documentId,
  issueKey,
  name,
}: {
  decision: Decision;
  /** The issue's decisions document, which both writes name. */
  documentId: string;
  issueKey: string;
  /** The decision's heading as plain words (`useDecisionHeadingWords`), which both writes are
   *  named after (`decision-heading-words`). */
  name: string;
}): { change: CardChangeAct | undefined; keep: CardKeepAct } {
  const queryClient = useQueryClient();
  const decisionsKey = issueDecisionsQuery(issueKey, true).queryKey;
  // Read once per answer of the decisions read, which is when the decision is a new object.
  const quote = useMemo(() => anchorQuote(decision), [decision]);
  const keep = useKeyedWrite({
    inline: true,
    // A write's label holds no markup: the reading list's `decision-heading-words`.
    label: () => `${DECISION_ACT_WORDS.keep} ${name}`,
    method: "POST",
    write: (id: string, bounds) =>
      api.keepDecision(id, decision.block_id, decision.text_sha256, bounds),
    onSuccess: (kept) => {
      if (kept === null) {
        void queryClient.invalidateQueries({ exact: true, queryKey: decisionsKey });
        return;
      }
      replaceQueryData<IssueDecisions>(queryClient, decisionsKey, (view) =>
        keptDecisions(view, kept, decision.text_sha256)
      );
    },
  });
  const send = useKeyedWrite({
    inline: true,
    // A write's label holds no markup: the reading list's `decision-heading-words`.
    label: () => `${DECISION_ACT_WORDS.change} ${name}`,
    method: "POST",
    write: (input: { anchor: ChangeAnchor; body: string }, bounds) =>
      api.createComment(issueKey, changeCommentInput(documentId, input), bounds),
  });
  // Whether the send is reading the decisions document's text, which it does before the comment
  // goes out: Send change is off while it does, as it is while the comment is out.
  const [reading, setReading] = useState(false);
  return {
    change:
      quote === undefined
        ? undefined
        : {
            change: (body, sent) => {
              setReading(true);
              void readChangeAnchor(queryClient, decision, documentId, issueKey, quote).then(
                (anchor) => {
                  setReading(false);
                  send.mutate({ anchor, body }, { onSuccess: sent });
                }
              );
            },
            error: send.error,
            pending: reading || send.isPending,
            quote,
          },
    keep: { error: keep.error, keep: () => keep.mutate(documentId), pending: keep.isPending },
  };
}
