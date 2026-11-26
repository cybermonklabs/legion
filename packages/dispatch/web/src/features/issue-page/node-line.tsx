import { type ReactNode, useMemo } from "react";
import { Link } from "react-router-dom";

import type { DisplayTree } from "../../api/types";
import { pageQuietText } from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import { DeletedBody, EditedMark } from "../refs/PostMarks";
import { useLoadedEngine } from "../refs/parsed-references";
import { usePlainHeadline } from "../refs/plain-text";
import { type PostDeletion, postDeletion } from "../refs/post-marks";
import { parseNodeReference, referenceHref } from "../refs/routes";
import { drawnLine, firstLineOf, lineReading, type ParentLine, type TreeNode } from "./issue-tree";
import { authorText, pageChrome } from "./page-structure";
import { NODE_TYPE_NAMES, type NodeType } from "./page-words";

/**
 * A node's one line (R1), as every place that draws one draws it - a folded branch's line
 * (`FirstLine`), a parent's line under a filter (`NodeParentLine`), a crumb above a zoom
 * (`IssueTree.tsx`'s `CrumbName`), a pin in Details (`IssuePins.tsx`'s `PinLine`), the item an
 * issue was filed out of in Details (`IssueMargin.tsx`'s `FiledItemWords`) and a task's line in
 * the Plan view (`plan-filter/PlanFilterBody.tsx`'s `useTaskLine`): its node's first
 * line, projected from its words by the shared plain-text projection (`lineReading`) and cut by
 * R1's first-line rule (`firstLineOf`), with an ellipsis only where the cut ended inside a
 * sentence (`drawnLine`), so a heading never runs into the paragraph after it. A reference in it
 * reads as the same reference in the node's row does (`RowText.tsx`): as its target's current
 * name once it resolves, and as the words its author wrote until then (R8, issue page decision
 * 350); and a numbered step keeps its number (decisions 66 and 88). The issue's own line is its
 * key and then its projected title, the key handed to R1's rule apart from the title, which reads
 * a label after it (`LineReading.key`). Each role is read by the issue the node's record is on.
 * `line` is `undefined` until the projection has answered, which is one pre-paint commit on a
 * page whose engine is loaded, and `""` for a node whose words hold none. `reads` draws nothing
 * and the caller mounts it where it draws the line.
 */
export function useNodeLine(
  node: Pick<TreeNode, "id" | "source" | "text" | "tree" | "type">,
  {
    surface,
  }: {
    readonly surface:
      | "issue-filed-from"
      | "issue-pin"
      | "issue-page-history-block"
      | "node-row-first-line"
      | "node-row-parent-line"
      | "outline-crumb"
      | "plan-task-line";
  }
): { line: string | undefined; reads: ReactNode } {
  const { id, source, text: stored, tree, type } = node;
  const reading = useMemo(
    () => lineReading({ id, text: stored, tree, type }),
    [id, stored, tree, type]
  );
  const markdown = "markdown" in reading ? reading.markdown : undefined;
  // A stored text with no reading is parsed here, once for as long as the line draws it, as the
  // row's own words are (`RowText.tsx`'s `ParsedText`), and its parse is drawn as a reading is:
  // the projection runs again on every name the page publishes, and handed the text alone it
  // would parse it again each time. The parse keeps soft breaks as line breaks, as the
  // projection's parse of a first line does, and the text goes beside it, so the line names only
  // what the text cites. Until the engine loads, and for a text it refuses, the line is
  // `undefined`, as the projection answers for either.
  const { engine } = useLoadedEngine();
  const parsed = useMemo(
    () =>
      markdown === undefined || engine === undefined
        ? undefined
        : (engine.parse(markdown, "line")?.toJSON() as DisplayTree | undefined),
    [engine, markdown]
  );
  const { reads, text } = usePlainHeadline(
    "tree" in reading ? reading.tree : parsed,
    parsed === undefined ? undefined : markdown,
    { firstLine: markdown !== undefined, issue: source?.issue, surface }
  );
  const words = "none" in reading ? "" : text?.text;
  const key = "key" in reading ? reading.key : undefined;
  return {
    line: words === undefined ? undefined : drawnLine(firstLineOf(words, key)),
    reads,
  };
}

/** The words a place that names a node by its one line draws where it must name it by
 *  something - a parent's line under a filter (`NodeParentLine`), a crumb above a zoom
 *  (`IssueTree.tsx`'s `CrumbName`) and a task's line in the Plan view (`useTaskLine`): the line
 *  (`useNodeLine`), and the page's word for the node's kind where its words hold none (R22).
 *  Nothing until the projection has answered. */
export function lineOrKindWord(line: string | undefined, type: NodeType): string {
  return line === "" ? NODE_TYPE_NAMES[type] : (line ?? "");
}

/** A folded branch's one line (`useNodeLine`): empty until the projection has answered. */
export function FirstLine({ node }: { node: TreeNode }): ReactNode {
  const { line, reads } = useNodeLine(node, { surface: "node-row-first-line" });
  return (
    <span data-node-first-line="" {...authorText}>
      {reads}
      {line ?? ""}
    </span>
  );
}

/** Under a filter, the one line a row draws for its node's parent where the filter draws that
 *  parent as no row (R2, R1's first-line rule): the page's word for the parent's kind, then its
 *  one line (`ParentLink`). The outline's row draws it (`NodeRow.tsx`) and so do the
 *  Verification view's statement and other rows, `spaced` from the words above them. */
export function NodeParentLine({
  line,
  spaced = false,
  tabStops,
}: {
  line: ParentLine;
  /** Whether the line stands 2 px under the words above it, as the Verification view's rows draw
   *  it. */
  spaced?: boolean;
  /** Whether the parent's link is a tab stop (`ParentLink`). */
  tabStops: boolean;
}): ReactNode {
  return (
    <p
      className={`${spaced ? "mt-0.5 " : ""}flex min-w-0 items-baseline gap-1 text-xs leading-5 ${pageQuietText}`}
      data-node-parent-line=""
      data-parent-id={line.id}
      data-parent-type={line.type}
    >
      <span className="shrink-0" {...pageChrome}>
        {NODE_TYPE_NAMES[line.type]}
      </span>
      <ParentLink line={line} tabStops={tabStops} />
    </p>
  );
}

/** The parent's own first line, as every one line of a node is drawn (`useNodeLine`), and the
 *  page's word for its kind where its words hold none (R22): as the link a reader follows to
 *  that node where the model gives it an address (R18, and R26: a step's row reaches the
 *  statement it serves), and as words with no link where that address names a person, whom no
 *  page shows (R5). Out of the tab order, as every other control inside a row is, unless
 *  `tabStops` holds: `f` steps to it and Enter follows it. A parent its author deleted is drawn
 *  as the deleted line, as its own row draws it (`DeletedLine`), and one its author edited says
 *  ` · edited` after its line (`EditedMark`), so the line holds both marks wherever the parent's
 *  own row is not drawn (deleted-post R2 and R3). */
function ParentLink({ line, tabStops }: { line: ParentLine; tabStops: boolean }): ReactNode {
  const parent = line.reference === undefined ? undefined : parseNodeReference(line.reference);
  const href = parent === undefined ? undefined : referenceHref(parent);
  const { line: drawn, reads } = useNodeLine(line, { surface: "node-row-parent-line" });
  const deleted = postDeletion({ deleted_at: line.deletedAt, deleted_by: line.deletedBy });
  // Nothing until the projection answers, one pre-paint commit on a page whose engine is loaded.
  const words =
    deleted === null ? (
      <>
        {reads}
        {lineOrKindWord(drawn, line.type)}
        <EditedMark at={line.editedAt} marks={pageChrome} />
      </>
    ) : (
      <DeletedLine deleted={deleted} />
    );
  return href === undefined ? (
    <span className="min-w-0 break-words" {...authorText}>
      {words}
    </span>
  ) : (
    <Link
      className={`dispatch-inline-link min-w-0 break-words underline decoration-page-bullet underline-offset-[3px] ${pageQuietText}`}
      tabIndex={tabStops ? undefined : -1}
      to={href}
      {...authorText}
    >
      {words}
    </Link>
  );
}

/**
 * A deleted post's line where its row draws its words, whole or as its one line, and where the
 * page draws its one line apart from its row - a filter's parent line (`ParentLink`), a crumb
 * above a zoom (`IssueTree.tsx`'s `CrumbLine`) (deleted-post R3): the line every surface draws in
 * a deleted post's place (`DeletedBody`), in its italic, with who deleted it and when. The actor
 * is named as the page names an author (`AgentText.author`), the rule the row's own author and
 * "Kept by" are named by, by issue page decision 342: a session nothing names reads as the
 * page's word for an agent, and never as its short id. The node's text is the same line as the
 * model writes it (`tree-drafts.ts`'s `deletedPostLine`), with the actor named as no registry
 * knows it and the time as stored, so the row draws the line from the delete and never from that
 * text. The line's words are the page's own, marked as its chrome and declared in
 * `page-words.ts` (`CONTROL_LABELS.post_deleted_by`), and the actor's name and the time are not
 * (deleted-post R3).
 */
export function DeletedLine({ deleted }: { deleted: PostDeletion }): ReactNode {
  const agentText = useAgentText();
  return (
    <DeletedBody
      {...(deleted.by === null ? {} : { actorName: agentText.author(deleted.by) })}
      deleted={deleted}
      marks={{ own: pageChrome, theirs: authorText }}
    />
  );
}
