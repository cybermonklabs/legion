import type { DisplayTree } from "../../api/types";

/** A title as the page reads it: a reading of one paragraph holding the title as one text run,
 *  so nothing in it is parsed, since a title is no markdown: a parse would read
 *  `1. Migrate the table` as a list item with no label, and leave out the marks of
 *  ``Add `x` flag``. The line of the issue and of a child issue is projected from it
 *  (`issue-tree.ts`'s `lineReading`), and so is the name of a chip naming an issue
 *  (`treeContext`), so a row and a chip read one title by one rule. */
export function titleReading(title: string): DisplayTree {
  return {
    content: [{ content: [{ text: title, type: "text" }], type: "paragraph" }],
    type: "doc",
  };
}
