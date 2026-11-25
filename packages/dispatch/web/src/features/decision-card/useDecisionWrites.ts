import { useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { api } from "../../api/client";
import { replaceQueryData } from "../../api/query-client";
import type { Decision, IssueDecisions } from "../../api/types";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { anchorQuote, type ChangeAnchor, changeAnchor } from "./change-anchor";
import type { CardChangeAct, CardKeepAct } from "./DecisionCard";
import { DECISION_ACT_WORDS } from "./decision-card-words";

/**
 * Keep and Change on a decision of the bare `decisions` read (`["decisions", <key>]`), as the
 * card's caller hands them to it: the writes today's Decisions tab makes, which its comments
 * margin makes too (R19, R21).
 *
 * Keep is `POST /api/v1/artifacts/{id}/blocks/{block_id}/keep` with the sum of the text the card
 * showed, so a decision the agent changed since is refused (`409 DECISION_CHANGED`) rather than
 * agreed with. Its answer is written over any read of the decisions still out, which was asked
 * before the Keep and would put the decision back as waiting (`replaceQueryData`); every card
 * that reads the decisions from that key settles with it, two copies of one decision among them.
 *
 * Change is a comment of the issue anchored to the decision's words in the decisions document,
 * as `DecisionActions.tsx` writes it: the send reads the document's text once, and counts which
 * of the quote's places is the decision's own over its reading (`changeAnchor`). With no reading
 * - the read failed - the anchor names no place. `undefined` for a decision whose heading is all
 * it holds, which has no words to anchor on.
 */
export function useDecisionWrites({
  decision,
  documentId,
  issueKey,
}: {
  decision: Decision;
  /** The issue's decisions document, which both writes name. */
  documentId: string;
  issueKey: string;
}): { change: CardChangeAct | undefined; keep: CardKeepAct } {
  const queryClient = useQueryClient();
  // Read once per answer of the decisions read, which is when the decision is a new object.
  const quote = useMemo(() => anchorQuote(decision), [decision]);
  const keep = useKeyedWrite({
    inline: true,
    label: () => `${DECISION_ACT_WORDS.keep} ${decision.heading}`,
    method: "POST",
    write: (id: string, bounds) =>
      api.keepDecision(id, decision.block_id, decision.text_sha256, bounds),
    onSuccess: (kept) => {
      if (kept === null) {
        void queryClient.invalidateQueries({ exact: true, queryKey: ["decisions", issueKey] });
        return;
      }
      replaceQueryData<IssueDecisions>(queryClient, ["decisions", issueKey], (view) =>
        view === undefined
          ? view
          : {
              ...view,
              // The Keep is of the text this card showed, which the server took, so it applies
              // to the row that still holds that text. A decision's changed version is the
              // greatest of the blocks it holds now and does not move when a block leaves it, so
              // a row whose text changed since can carry the version the Keep was taken at.
              decisions: (view.decisions ?? []).map((row) =>
                row.block_id === kept.block_id && row.text_sha256 === decision.text_sha256
                  ? { ...row, kept, waiting_review: false }
                  : row
              ),
            }
      );
    },
  });
  const send = useKeyedWrite({
    inline: true,
    label: () => `${DECISION_ACT_WORDS.change} ${decision.heading}`,
    method: "POST",
    // A comment on an issue's document is the issue's own write: the artifact route refuses it,
    // naming the issue route (`send-request.ts` follows the same rule for a thread's replies).
    write: (input: { anchor: ChangeAnchor; body: string }, bounds) =>
      api.createComment(
        issueKey,
        {
          anchor: {
            artifact: documentId,
            occurrence: input.anchor.occurrence,
            quote: input.anchor.quote,
          },
          body: input.body,
        },
        bounds
      ),
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
              void queryClient
                .fetchQuery({
                  queryFn: () => api.getArtifactText(documentId),
                  queryKey: ["artifact", documentId, "text"],
                  staleTime: 0,
                })
                .then(
                  (text) => text.markdown_tree,
                  () => undefined
                )
                .then((tree) => {
                  setReading(false);
                  const anchor = changeAnchor(
                    decision,
                    queryClient.getQueryData<IssueDecisions>(["decisions", issueKey])?.decisions,
                    tree
                  );
                  send.mutate({ anchor: anchor ?? { quote }, body }, { onSuccess: sent });
                });
            },
            error: send.error,
            pending: reading || send.isPending,
            quote,
          },
    keep: { error: keep.error, keep: () => keep.mutate(documentId), pending: keep.isPending },
  };
}
