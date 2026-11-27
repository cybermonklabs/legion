import { graphNode } from "../../../__tests__/graph-fixture";
import type { DisplayTree, GraphReferences } from "../../../api/types";

/** The statement every task of `servingTask` serves: the requirement `r1` of `LAY-2`'s spec. */
const STATEMENT = "dispatch://LAY-2/spec#b-r1";

/**
 * A task of the plan document `plan-md` of `LAY-2` that serves the statement `r1` of its spec, as
 * the references read answers it: its excerpt is the task's stored text, the directive its author
 * typed with the statement's address, and the server's reading of it (`text_tree`) the task
 * holding `content`, one paragraph of `words` where the caller names no other. It was referenced
 * at nine on the reader's own 15 November 2026, since a row draws the reader's local day of it:
 * a UTC instant falls on another day in a zone far from UTC.
 */
export function servingTask(
  block: string,
  words: string,
  content: readonly DisplayTree[] = [
    { content: [{ text: words, type: "text" }], type: "paragraph" },
  ]
): NonNullable<GraphReferences["edges"]>[number] {
  return {
    created_at: new Date(2026, 10, 15, 9).toISOString(),
    direction: "in",
    excerpt: {
      block_id: block,
      text: `:::plan_task{#${block} serves="${STATEMENT}"}\n${words}\n:::`,
      text_tree: {
        content: [{ attrs: { blockId: block, serves: [STATEMENT] }, content, type: "plan_task" }],
        type: "doc",
      },
    },
    kind: "serves",
    node: graphNode({
      block_id: block,
      document: "dispatch://LAY-2/artifact/plan-md",
      id: `plan-md#${block}`,
      issue_key: "LAY-2",
      kind: "block",
      ref: `dispatch://LAY-2/artifact/plan-md#b-${block}`,
    }),
    source_seq: null,
  };
}
