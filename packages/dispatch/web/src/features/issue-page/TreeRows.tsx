import { useQuery } from "@tanstack/react-query";
import type { CSSProperties, ReactNode } from "react";

import { whoAmIQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import { pageQuietText } from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import { EVENT_FRESH } from "../refs/reference-target";
import { useDocumentAdmissions } from "./document-admission";
import { documentOfNode } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import { IndentGuides, NodeRow } from "./NodeRow";
import type { ThreadMark } from "./outline-thread-read";
import { authorText, pageChrome } from "./page-structure";
import { authorKind, CONTROL_LABELS, isReader } from "./page-words";
import { type DrawnRow, indentOf, type PageRow } from "./tree-fold";
import { rowKeyOf } from "./tree-keys";
import { nodeEditable } from "./useNodeEditing";

/**
 * The rows of the issue's tree, drawn.
 *
 * It is the drawing alone: which rows there are is the fold's (`tree-fold.ts`), and what the
 * reader folded, focused and is editing in is `IssueTree.tsx`'s, which hands each row what it
 * is. Every row draws its node whole, with what it holds on its face, whatever the focus (R11).
 * The row after a node's drawn children says how many are left and draws the next page of them,
 * and the control above the first row reads the outline read's next page of older comments, each
 * where it is given (R15).
 */
export function TreeRows({
  artifacts,
  context,
  editing,
  focused,
  isClosed,
  onFocus,
  onOlder,
  onPage,
  onToggle,
  report,
  rows,
  tabStop,
  threads,
  unplaced,
  zoomRoot,
}: {
  /** Each artifact of the issue by its slug (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The ids of the rows this preset draws only as the ancestor of a match. */
  context: ReadonlySet<string>;
  /** The row the reader is editing in and the document its editor belongs to. */
  editing: { readonly artifactId: string; readonly row: string } | undefined;
  /** The key of the row the focus is on: the row that holds the focus, or the one a press on
   *  another control of the page took it from, which keeps its focused look until that press has
   *  ended (`IssueTree.tsx`'s `useReleaseFocus`). */
  focused: string | undefined;
  isClosed: boolean;
  onFocus: (row: string, node: TreeNode) => void;
  /** Reads the next page of the outline read's older comments, which the control above the
   *  first row does; undefined where no older comment remains or the tree is drawn from no
   *  issue, and the control is not drawn. */
  onOlder?: () => void;
  /** Draws the next page of a node's children, which the row that pages them does. */
  onPage: (row: PageRow) => void;
  onToggle: (node: TreeNode, expanded: boolean | undefined) => void;
  /** What a key reported when it had nowhere to go, and the key of the row it was pressed on,
   *  which draws it (`NodeRow`'s `report`). */
  report?: { readonly line: string; readonly row: string | undefined };
  rows: readonly DrawnRow[];
  /** The key of the row that is the tree's one tab stop. */
  tabStop: string | undefined;
  /** Each thread's newest write by the node that draws it (`threadMarks`). */
  threads: ReadonlyMap<string, ThreadMark>;
  /** The key of the row that, focused, does not place itself on screen (`NodeRow`'s
   *  `placesItself`): the outline's first row the page opens on and the row a zoom leaves the
   *  reader on, each until the reader acts, and the row the reader is editing in, which the
   *  editor's caret places as the reader writes. */
  unplaced: string | undefined;
  /** The id of the node the tree is zoomed to, whose row is drawn open by definition, so it
   *  draws no fold control, whose press drew nothing. Absent where the tree is drawn from the
   *  issue, which is no row, and for a view that zooms nothing. */
  zoomRoot?: string;
}): ReactNode {
  // Each row's author by the page's one rule for naming one (`AgentText.author`), read once for
  // every row rather than once a row: a name and a shape for every node, the page's own words
  // where the model holds no author.
  const agentText = useAgentText();
  // The reader, whose own words carry the ink rule and whose name is drawn in the ink.
  const reader = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  // What each document's room last admitted the page to, read once for every row: a row of a
  // document its room admitted read-only takes no typing (`nodeEditable`).
  const admissions = useDocumentAdmissions();
  return (
    <>
      {onOlder === undefined ? null : (
        <PageControl kind="older" level={1} onPress={onOlder}>
          {CONTROL_LABELS.older_comments}
        </PageControl>
      )}
      {rows.map((row) => {
        const key = rowKeyOf(row);
        if (row.kind === "page") {
          return (
            <PageControl key={key} kind="page" level={row.level} onPress={() => onPage(row)}>
              {/* One run of text: below 1280 px a button is a flex box (`styles.css`), which
                  lays the count's own span and the words after it out as two boxes side by
                  side, the space that opens the words dropped at the start of theirs. */}
              <span>
                <span {...authorText}>{row.left.toLocaleString()}</span>{" "}
                {row.left === 1 ? CONTROL_LABELS.page_row : CONTROL_LABELS.page_rows}
              </span>
            </PageControl>
          );
        }
        const place = documentOfNode(row.node.id, artifacts);
        const mark = threads.get(row.node.id);
        return (
          <NodeRow
            {...(row.node.answer === undefined
              ? {}
              : { answerer: agentText.author(row.node.answer.by) })}
            author={{
              kind: authorKind(row.node.author),
              name: agentText.author(row.node.author),
              reader: isReader(row.node.author, reader),
            }}
            context={context.has(row.node.id)}
            {...(place === undefined ? {} : { document: place.artifact })}
            editable={nodeEditable(place, isClosed, admissions)}
            {...(editing?.row === key ? { editing: editing.artifactId } : {})}
            expanded={row.expanded}
            focused={focused === key}
            foldable={!(row.level === 1 && row.node.id === zoomRoot)}
            folded={row.folded}
            isClosed={isClosed}
            key={key}
            level={row.level}
            node={row.node}
            onFocus={() => onFocus(key, row.node)}
            onToggle={() => onToggle(row.node, row.expanded)}
            ownAnswer={isReader(row.node.answer?.by, reader)}
            placesItself={unplaced !== key}
            {...(report?.row === key ? { report: report.line } : {})}
            rowKey={key}
            tabStop={tabStop === key}
            {...(mark === undefined ? {} : { threadRead: mark })}
          />
        );
      })}
    </>
  );
}

/**
 * One control that draws more of the outline in place (R15), at the level of the rows it draws
 * and under the guides of the levels above them: the row after a node's drawn children, and the
 * control above the first row that reads older comments. A press replaces it with what it drew,
 * so it takes no focus from that press: it leaves the focus where it is (decision 366), and it is
 * out of the tab order, as every control inside the tree is, since the tree is one tab stop. The
 * keys press it from the row beside it, and leave the focus on the first row it drew
 * (`useTreeKeymap.ts`'s `tree-next` and `tree-previous`).
 */
function PageControl({
  children,
  kind,
  level,
  onPress,
}: {
  children: ReactNode;
  /** Which of the two it is, by the mark it carries: the row after a node's children
   *  (`data-tree-page`), or the control that reads older comments (`data-older-comments`). */
  kind: "older" | "page";
  level: number;
  onPress: () => void;
}): ReactNode {
  const depth = indentOf(level);
  return (
    <div
      aria-level={level}
      className={`relative flex min-w-0 pl-[calc(var(--row-depth)*23px+26px)] text-xs leading-7 max-[761px]:pl-[calc(var(--row-depth)*17px+22px)] ${pageQuietText}`}
      role="treeitem"
      style={{ "--row-depth": depth } as CSSProperties}
      tabIndex={-1}
      data-older-comments={kind === "older" ? "" : undefined}
      data-tree-page={kind === "page" ? "" : undefined}
    >
      <IndentGuides depth={depth} />
      <button
        className="dispatch-inline-link font-medium underline decoration-page-hair underline-offset-[3px]"
        onClick={onPress}
        onMouseDown={(event) => event.preventDefault()}
        tabIndex={-1}
        type="button"
        {...pageChrome}
      >
        {children}
      </button>
    </div>
  );
}
