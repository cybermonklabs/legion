import type { Decision } from "../../api/types";
import type { CardDecision } from "./DecisionCard";

/**
 * A decision of the bare `decisions` member of `GET /api/v1/issues/{key}/decisions`
 * (`model.Decision`) as the card draws it (R20): its `unsure` is the level, and the read holds no
 * author, no level and no reason apart from the text. Only a tie to a block is a statement it
 * shaped, as the tab and the margin read the ties on `main`. A Keep names its person by login.
 */
export function fromDecision(decision: Decision): CardDecision {
  return {
    asks: decision.asks ?? [],
    changed_at: decision.changed_at,
    heading: decision.heading,
    kept_by: decision.kept === null ? undefined : { id: decision.kept.kept_by, kind: "user" },
    kind: decision.kind === "person" ? "person" : "agent",
    links: (decision.links ?? []).flatMap((link) =>
      link.to.block_id === undefined
        ? []
        : [
            {
              block_id: link.to.block_id,
              changed_at: link.to.changed_at,
              created_at: link.created_at,
              ref: link.to.ref,
              stale: link.stale,
              text: link.to.text,
            },
          ]
    ),
    replaced: decision.replaced,
    text: decision.text,
    text_tree: decision.text_tree,
    unsure: decision.unsure,
    waiting_review: decision.waiting_review,
  };
}
