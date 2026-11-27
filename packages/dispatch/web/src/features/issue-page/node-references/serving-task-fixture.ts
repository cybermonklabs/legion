import { graphNode } from "../../../__tests__/graph-fixture";
import type { DisplayTree, GraphReferences, ReferenceTarget } from "../../../api/types";

/** The statement every task of `servingTask` serves where the caller names no other: the
 *  requirement `r1` of `LAY-2`'s spec. */
const STATEMENT = "dispatch://LAY-2/spec#b-r1";

/** How many runes of a block's stored text the references read's excerpt of it holds
 *  (`refs.BlockExcerptRunes`). */
const EXCERPT_RUNES = 480;

/** A task of `servingTask`, as each read the list names it from answers it. */
export interface ServingTask {
  /** The task as the references read answers it. */
  readonly edge: NonNullable<GraphReferences["edges"]>[number];
  /** The task as the page's batched names read answers its address. */
  readonly target: ReferenceTarget;
}

/**
 * A task of the plan document `plan-md` of `LAY-2` that serves the statements `serves`, `r1` of
 * its spec where the caller names none, as the two reads the list names it from answer it.
 *
 * The names read's member (`target`) holds the server's reading of the task's own content
 * (`text_tree`), with no opening line: `content`, one paragraph of `words` where the caller names
 * no other. Its `name`, and the references read's excerpt (`edge`), are the task's stored text cut
 * at its first 480 runes, the directive's opening line included, which lists every statement the
 * task serves (`refs.BlockExcerpt`). The excerpt's reading is the one the server gives that cut:
 * the task whole where the cut holds it; a task holding one paragraph of the words the cut left,
 * as a directive with no closing fence runs to the end of its text, where the cut falls in the
 * task's words; and one paragraph of the characters as typed where it falls inside the opening
 * line, which is no directive without its closing brace. So a task that serves many statements
 * has an excerpt of markup alone (decision 565).
 *
 * It was referenced at nine on the reader's own 15 November 2026, since a row draws the
 * reader's local day of it: a UTC instant falls on another day in a zone far from UTC.
 */
export function servingTask(
  block: string,
  words: string,
  content: readonly DisplayTree[] = [
    { content: [{ text: words, type: "text" }], type: "paragraph" },
  ],
  serves: readonly string[] = [STATEMENT]
): ServingTask {
  const ref = `dispatch://LAY-2/artifact/plan-md#b-${block}`;
  const opener = `:::plan_task{#${block} serves="${serves.join(" ")}"}`;
  const stored = `${opener}\n${words}\n:::`;
  const excerpt = [...stored].slice(0, EXCERPT_RUNES).join("");
  // The typed block's own content, as the server reads the part of it the excerpt holds.
  const inside: readonly DisplayTree[] =
    excerpt === stored
      ? content
      : [
          {
            content: [{ text: excerpt.slice(opener.length + 1), type: "text" }],
            type: "paragraph",
          },
        ];
  const excerptReading: DisplayTree =
    excerpt.length > opener.length
      ? { attrs: { blockId: block, serves }, content: inside, type: "plan_task" }
      : { content: [{ text: excerpt, type: "text" }], type: "paragraph" };
  return {
    edge: {
      created_at: new Date(2026, 10, 15, 9).toISOString(),
      direction: "in",
      excerpt: {
        block_id: block,
        text: excerpt,
        text_tree: { content: [excerptReading], type: "doc" },
      },
      kind: "serves",
      node: graphNode({
        block_id: block,
        document: "dispatch://LAY-2/artifact/plan-md",
        id: `plan-md#${block}`,
        issue_key: "LAY-2",
        kind: "block",
        ref,
      }),
      source_seq: null,
    },
    target: {
      gone: false,
      kind: "block",
      name: excerpt,
      ref,
      text_tree: { content, type: "doc" },
    },
  };
}

/**
 * Statements of `LAY-2`'s spec enough for a task that serves them all to open with a line of at
 * least `runes` runes (`servingTask`'s `serves`): `r1`, then `r2` and on.
 */
export function statementsReaching(block: string, runes: number): string[] {
  const serves = [STATEMENT];
  while (`:::plan_task{#${block} serves="${serves.join(" ")}"}`.length < runes) {
    serves.push(`dispatch://LAY-2/spec#b-r${serves.length + 1}`);
  }
  return serves;
}
