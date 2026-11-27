import { useQuery } from "@tanstack/react-query";
import { type ComponentProps, type CSSProperties, type ReactNode, useState } from "react";

import { whoAmIQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import { pageQuietText } from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import { EVENT_FRESH } from "../refs/reference-target";
import type { PointMark } from "./current-point";
import { type DocumentAdmissions, useDocumentReadOnly } from "./document-admission";
import { documentOfNode } from "./issue-artifacts";
import { isStatement, type TreeNode } from "./issue-tree";
import { LensActContext, useLensAct } from "./lenses/LensMenu";
import { takesCaret, useRoomFacts } from "./model-rows/room-target";
import { IndentGuides, NodeRow } from "./NodeRow";
import type { ThreadMark } from "./outline-thread-read";
import { useOutlineWriter } from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import { type AuthorKind, authorKind, CONTROL_LABELS, isReader } from "./page-words";
import { type DrawnRow, indentOf, type PageRow } from "./tree-fold";
import { rowKeyOf } from "./tree-keys";

/** No admission read-only: what the rows' rule reads while the tree artifact's room admitted the
 *  page to write, or has not answered (`TreeRows`' `treeAdmission`). */
const NO_ADMISSION: DocumentAdmissions = new Map();

/**
 * The rows of the issue's tree, drawn.
 *
 * It is the drawing alone: which rows there are is the fold's (`tree-fold.ts`), and what the
 * reader folded, focused and is editing in is `IssueTree.tsx`'s, which hands each row what it
 * is. Every row draws its node whole, with what it holds on its face, whatever the focus (R11).
 * The row after a node's drawn children says how many are left and draws the next page of them,
 * the control above the first row reads the outline read's next page of older comments, and the
 * control after the last row reads the read's next page, each where it is given (R15).
 */
export function TreeRows({
  artifacts,
  context,
  coverageOut = false,
  focused,
  foldedMore = false,
  isClosed,
  more,
  onFocus,
  onOlder,
  onPage,
  onToggle,
  points,
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
  /** Whether the coverage read has not answered yet: each statement's row holds the line its
   *  counts of what serves it are drawn in meanwhile (`NodeRow`'s `countsHeld`), so the counts
   *  arriving move no row. False unless the tree passes it. */
  coverageOut?: boolean;
  /** The key of the row the focus is on: the row that holds the focus, or the one a press on
   *  another control of the page took it from, which keeps its focused look until that press has
   *  ended (`IssueTree.tsx`'s `useReleaseFocus`). */
  focused: string | undefined;
  /** Whether the read the rows are drawn from holds nodes no page read yet holds, so a folded
   *  row's count carries "+" (R15). False unless the tree passes it. */
  foldedMore?: boolean;
  isClosed: boolean;
  /** The read's next page, which the control after the last row reads (R15): how many nodes the
   *  read holds after the pages read, where it says, and the press. Undefined where its answer
   *  names none, and the control is not drawn. */
  more?: { readonly left: number | undefined; readonly onPress: () => void };
  /** Gives the focus to one row, by its key (`rowKeyOf`) and its node, which the focus coming
   *  into the row does: one function for every row, which each row calls with its own key, so
   *  the tree makes it once and no row is given a new one (`NodeRow`'s `memo`). */
  onFocus: (row: string, node: TreeNode) => void;
  /** Reads the next page of the outline read's older comments, which the control above the
   *  first row does; undefined where no older comment remains or the tree is drawn from no
   *  issue, and the control is not drawn. */
  onOlder?: () => void;
  /** Draws the next page of a node's children, which the row that pages them does. */
  onPage: (row: PageRow) => void;
  /** Folds or unfolds one row, by its key (`rowKeyOf`), which a press on its fold control does:
   *  the fold is the row's, so another row of the same node stays as it was. One function for
   *  every row, as `onFocus` is. */
  onToggle: (row: string, expanded: boolean | undefined) => void;
  /** Each row that carries the conversation's current point, by its key (`current-point.ts`'s
   *  `pointMarks`, R12): a row it holds no key of is not marked. */
  points: ReadonlyMap<string, PointMark>;
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
   *  reader on, each until the reader acts. The row the reader is editing in places nothing
   *  either, which that row sets for itself (`NodeRow`'s `useEditedRow`). */
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
  // Each author as one object, held across renders, so a row whose author is the same as at its
  // last render is given the object it was given then (`heldAuthor`, `NodeRow`'s `memo`).
  const [authors] = useState(() => new Map<string, RowAuthor>());
  // The reader, whose own words carry the ink rule and whose name is drawn in the ink.
  const reader = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  // The room of the issue's tree read, read once for every row as Enter reads it
  // (`useNodeEditing.ts`), by the issue the page's writer is of; with no writer there is none.
  const room = useRoomFacts(useOutlineWriter()?.issueKey);
  // The one admission no row reads for itself: the tree artifact's, which a node written into the
  // issue is edited in and which is no row's document. A row of a document reads its own
  // document's (`NodeRow`'s `useDocumentReadOnly`), so the rows' rule (`takesCaret`) is given the
  // tree's entry alone, and a room's answer for a document draws no row but that document's.
  const tree = room?.tree ?? undefined;
  const treeReadOnly = useDocumentReadOnly(tree);
  const treeAdmission = tree !== undefined && treeReadOnly ? new Map([[tree, true]]) : NO_ADMISSION;
  // The Lens act each row offers in Conversation and in Graph, whatever the focus, with every
  // node's invocations read once for all the rows: one value while the address stands and no
  // invocation of a lens changes (`useLensAct`).
  const lensAct = useLensAct();
  return (
    <>
      {onOlder === undefined ? null : (
        <PageControl kind="older" level={1} onPress={onOlder}>
          {CONTROL_LABELS.older_comments}
        </PageControl>
      )}
      <LensActContext.Provider value={lensAct}>
        {rows.map((row) => {
          const key = rowKeyOf(row);
          if (row.kind === "page") {
            return (
              <PageControl
                current={points.get(key)}
                key={key}
                kind="page"
                level={row.level}
                onPress={() => onPage(row)}
              >
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
          const current = points.get(key);
          return (
            <NodeRow
              {...(row.node.answer === undefined
                ? {}
                : { answerer: agentText.author(row.node.answer.by) })}
              author={heldAuthor(
                authors,
                authorKind(row.node.author),
                agentText.author(row.node.author),
                isReader(row.node.author, reader)
              )}
              context={context.has(row.node.id)}
              // Only a statement's row is given the coverage read's state, so the read answering
              // draws again no row it gives nothing to.
              countsHeld={coverageOut && isStatement(row.node)}
              {...(current === undefined ? {} : { current })}
              {...(place === undefined ? {} : { document: place.artifact })}
              // Each row reads its own document's room admission (`useDocumentReadOnly`) and
              // whether it is being edited (`useEditedRow`), so neither draws every row again.
              editable={!isClosed && place !== undefined}
              // Where the room is read, the row carries the typing cue wherever Enter puts a
              // caret, a criterion's row among them: a node written into the issue, such as a
              // criterion, takes its place from the room rather than from a document, and is
              // read against the tree artifact's admission (`treeAdmission`).
              {...(room === undefined
                ? {}
                : { editable: takesCaret(row.node, artifacts, room, isClosed, treeAdmission) })}
              expanded={row.expanded}
              focused={focused === key}
              foldable={!(row.level === 1 && row.node.id === zoomRoot)}
              folded={row.folded}
              foldedMore={foldedMore}
              isClosed={isClosed}
              key={key}
              level={row.level}
              node={row.node}
              onFocus={onFocus}
              onToggle={onToggle}
              ownAnswer={isReader(row.node.answer?.by, reader)}
              placesItself={unplaced !== key}
              {...(report?.row === key ? { report: report.line } : {})}
              rowKey={key}
              tabStop={tabStop === key}
              {...(mark === undefined ? {} : { threadRead: mark })}
            />
          );
        })}
      </LensActContext.Provider>
      {more === undefined ? null : (
        <PageControl kind="more" level={1} onPress={more.onPress}>
          <NextPageWords left={more.left} />
        </PageControl>
      )}
    </>
  );
}

/** Who made a row's node, as the row draws it (`NodeRow`'s `author`). */
type RowAuthor = ComponentProps<typeof NodeRow>["author"];

/** The author of `kind` named `name`, `reader` whether that is the reader, as the one object
 *  `held` holds for it: made the first time and the same at every later call for the same author,
 *  so a row given its author again is given the object it was given before. */
function heldAuthor(
  held: Map<string, RowAuthor>,
  kind: AuthorKind,
  name: string,
  reader: boolean
): RowAuthor {
  // Neither a kind nor the reader's flag holds a ":", so the name after them is read whole.
  const key = `${kind}:${reader}:${name}`;
  const author = held.get(key);
  if (author !== undefined) return author;
  const made = { kind, name, reader };
  held.set(key, made);
  return made;
}

/**
 * What the control that reads a read's next page says (R15), in the outline and after a view's
 * last row alike: how many nodes the read holds after the pages read, the author's number, and
 * the words that agree with it, in one run of text, as the row after a node's children draws its
 * count and words; the words alone where the read gives no count.
 */
export function NextPageWords({ left }: { left: number | undefined }): ReactNode {
  if (left === undefined) return CONTROL_LABELS.tree_page;
  return (
    <span>
      <span {...authorText}>{left.toLocaleString()}</span>{" "}
      {left === 1 ? CONTROL_LABELS.tree_page_row : CONTROL_LABELS.tree_page_rows}
    </span>
  );
}

/** The mark each control that draws more of the outline carries, by its kind (`PageControl`):
 *  the row after a node's children, the control that reads older comments, and the one that
 *  reads the read's next page. */
const PAGE_CONTROL_MARKS = {
  more: "data-tree-more",
  older: "data-older-comments",
  page: "data-tree-page",
} as const;

/**
 * One control that draws more of the outline in place (R15), at the level of the rows it draws
 * and under the guides of the levels above them: the row after a node's drawn children, the
 * control above the first row that reads older comments, and the control after the last row that
 * reads the read's next page. A press replaces it with what it drew, so it takes no focus from
 * that press: it leaves the focus where it is, and it is out of the tab order, as
 * every control inside the tree is, since the tree is one tab stop. The keys press it from the row
 * beside it, and leave the focus on the first row it drew (`useTreeKeymap.ts`'s `tree-next` and
 * `tree-previous`); a press of the read's next page leaves it there too.
 */
function PageControl({
  children,
  current,
  kind,
  level,
  onPress,
}: {
  children: ReactNode;
  /** The conversation's current point where it stands among the rows this control draws: `path`
   *  on the row that pages a node's children (`current-point.ts`'s `pointMarks`, R12), drawn as
   *  the mark a node's row carries; undefined on every other control. */
  current?: PointMark;
  /** Which of the three it is, by the mark it carries (`PAGE_CONTROL_MARKS`). */
  kind: keyof typeof PAGE_CONTROL_MARKS;
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
      data-current-point={current}
      {...{ [PAGE_CONTROL_MARKS[kind]]: "" }}
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
