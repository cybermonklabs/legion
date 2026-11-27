import { spyOn } from "bun:test";
import { Profiler } from "react";

import { displayTree } from "../../__tests__/display-trees";
import type { Comment, DisplayTree } from "../../api/types";
import * as rowText from "./RowText";
import { treeOf } from "./slots/stage-fixtures";
import { commentNode } from "./tree-read/tree-fixtures";

/**
 * What the tests that count the outline's row draws share (`row-redraws.test.tsx`,
 * `row-edit-redraws.test.tsx`): a record of each commit that draws a row's words, and the tree
 * read's answer of comments that carry the server's reading of their words.
 */

/** `RowText` as its module defines it, read before a case's spy stands in for the module's
 *  export, which `NodeRow.tsx`'s import reads from then on. */
const RowTextItself = rowText.RowText;

/** Each commit that draws a row's words, from now until `restore`. */
export interface RowDraws {
  /** How many commits drew each row's words since `mark` was called, by the row's node id. */
  readonly since: (mark: number) => Readonly<Record<string, number>>;
  /** The point in the record a later `since` counts from. */
  readonly mark: () => number;
  readonly restore: () => void;
}

/** Records each commit that draws a row's words (`RowText`), by the row's node id: the id of the
 *  record its words are (`TextSource.id`), which is the node's for a comment. */
export function recordRowDraws(): RowDraws {
  const draws: string[] = [];
  const spy = spyOn(rowText, "RowText").mockImplementation(function RecordedRowText(props) {
    return (
      <Profiler id={props.source?.id ?? props.markdown} onRender={(id) => draws.push(id)}>
        <RowTextItself {...props} />
      </Profiler>
    );
  });
  return {
    mark: () => draws.length,
    restore: () => spy.mockRestore(),
    since: (mark) => {
      const counts: Record<string, number> = {};
      for (const id of draws.slice(mark)) counts[id] = (counts[id] ?? 0) + 1;
      return counts;
    },
  };
}

/** `comments` as Conversation's tree read answers them, each with the server's reading of its
 *  words, as the route sends it, so the rows draw from the reading and parse nothing. Each text
 *  is read once into `readings`: the page's engine gives each block a new id at every parse,
 *  where the server answers the reading it stored, the same at every read of the same words. */
export function treeOfComments(
  comments: readonly Comment[],
  readings = new Map<string, DisplayTree | undefined>()
) {
  return treeOf(
    comments.map((comment) => {
      if (!readings.has(comment.body)) readings.set(comment.body, displayTree(comment.body));
      return { ...commentNode(comment), text_tree: readings.get(comment.body) };
    })
  );
}
