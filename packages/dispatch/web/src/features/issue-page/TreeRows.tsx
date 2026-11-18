import { useQuery } from "@tanstack/react-query";
import type { CSSProperties, ReactNode } from "react";
import { useMemo } from "react";
import { useLocation } from "react-router-dom";

import { whoAmIQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import { pageGuideLineBg, pageQuietText } from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import { parseIssuePath } from "../refs/routes";
import { issueAddress } from "./issue-address";
import { documentOfNode } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import { LensActContext } from "./lenses/LensMenu";
import { useStandingAddress } from "./lenses/lens-reads";
import { NodeRow } from "./NodeRow";
import { OvertakenLine } from "./node-states/OvertakenLine";
import type { ThreadMark } from "./outline-thread-read";
import { authorText, pageChrome } from "./page-structure";
import { authorKind, CONTROL_LABELS, isReader } from "./page-words";
import type { DrawnRow } from "./tree-fold";
import { rowKeyOf } from "./tree-keys";

/**
 * The rows of the issue's tree, drawn.
 *
 * It is the drawing alone: which rows there are is the fold's (`tree-fold.ts`), and what the
 * reader opened, pinned, focused and is editing in is `IssueTree.tsx`'s, which hands each row
 * what it is. A row below the levels the fold drew is one line saying how many rows it holds
 * and opening them in place.
 * A node's overtaken children fold to one line of their own after its other children, which
 * takes the focus and opens them in place (`OvertakenLine`).
 */
export function TreeRows({
  artifacts,
  context,
  cycle,
  editing,
  focused,
  isClosed,
  onFocus,
  onOpenDeeper,
  onOvertaken,
  onPin,
  onStop,
  onToggle,
  pinned,
  report,
  rows,
  tabStop,
  threads,
  unplaced,
}: {
  /** Each artifact of the issue by its slug (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The ids of the rows this preset draws only as the ancestor of a match. */
  context: ReadonlySet<string>;
  /** Which of the cycle key's three states the tree is in (`CYCLE_STATES`). */
  cycle: number;
  /** The row the reader is editing in and the document its editor belongs to. */
  editing: { readonly artifactId: string; readonly row: string } | undefined;
  /** The key of the row whose body and acts the focus opened: the row that holds the focus, or
   *  the one a press on another control of the page took it from, which keeps them drawn until
   *  that press has ended (`IssueTree.tsx`'s `useReleaseFocus`). */
  focused: string | undefined;
  isClosed: boolean;
  onFocus: (row: string, node: TreeNode) => void;
  onOpenDeeper: (id: string) => void;
  /** Opens or closes the line a node's overtaken children fold to, by that node's id. */
  onOvertaken?: (id: string, open: boolean) => void;
  onPin: (row: string) => void;
  /** Makes the line of overtaken nodes `key` names the tree's one tab stop as it takes the
   *  focus, which a row's own `onFocus` does for a row, with the line's own element. */
  onStop?: (key: string, line: HTMLElement) => void;
  onToggle: (node: TreeNode, expanded: boolean | undefined) => void;
  /** The keys of the rows whose bodies the reader pinned open. */
  pinned: ReadonlySet<string>;
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
}): ReactNode {
  // Each row's author by the page's one rule for naming one (`AgentText.author`), read once for
  // every row rather than once a row: a name and a shape for every node, the page's own words
  // where the model holds no author.
  const agentText = useAgentText();
  // The reader, whose own words carry the ink rule and whose name is drawn in the ink.
  const reader = useQuery(whoAmIQuery()).data?.login;
  // The Lens act a focused row offers: the issue the address names and whether its preset is
  // the outline, read as the page's slots read their address. A route that names no issue
  // offers no act. One value while the address stands, so no row draws again.
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  const issueKey = route?.key;
  const outline =
    route !== undefined && issueAddress(route, search, "", undefined).preset === "conversation";
  // Where the reader stands now, which a pick's answer reads as it lands, kept for as long as
  // the rows are drawn: the menu that made the pick goes with its row's focus.
  const standing = useStandingAddress();
  const lensAct = useMemo(
    () => (issueKey === undefined ? undefined : { issueKey, outline, standing }),
    [issueKey, outline, standing]
  );
  return (
    <>
      <LensActContext.Provider value={lensAct}>
        {rows.map((row) => {
          const key = rowKeyOf(row);
          // A node's overtaken children fold to one line, which takes the focus as a row does and
          // opens them in place.
          if (row.kind === "overtaken") {
            return (
              <OvertakenLine
                key={key}
                onOvertaken={onOvertaken}
                onStop={(line) => onStop?.(key, line)}
                {...(report?.row === key ? { report: report.line } : {})}
                row={row}
                tabStop={tabStop === key}
              />
            );
          }
          if (row.kind === "deeper") {
            return (
              <BranchMore
                held={row.held}
                key={key}
                level={row.level}
                onOpen={() => onOpenDeeper(row.of.id)}
              />
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
              body={
                focused === key ||
                pinned.has(key) ||
                editing?.row === key ||
                drawsBody(cycle, row.level)
              }
              context={context.has(row.node.id)}
              {...(place === undefined ? {} : { document: place.artifact })}
              {...(editing?.row === key ? { editing: editing.artifactId } : {})}
              expanded={row.expanded}
              focused={focused === key}
              folded={row.folded}
              isClosed={isClosed}
              key={key}
              level={row.level}
              node={row.node}
              onFocus={() => onFocus(key, row.node)}
              onPin={() => onPin(key)}
              onToggle={() => onToggle(row.node, row.expanded)}
              ownAnswer={isReader(row.node.answer?.by, reader)}
              pinned={pinned.has(key)}
              placesItself={unplaced !== key}
              {...(report?.row === key ? { report: report.line } : {})}
              rowKey={key}
              tabStop={tabStop === key}
              {...(mark === undefined ? {} : { threadRead: mark })}
            />
          );
        })}
      </LensActContext.Provider>
    </>
  );
}

/**
 * How much of the whole tree the cycle key draws, in the three states it steps through: its
 * rows as the fold left them, those rows with every body down to the second level open, and
 * every branch unfolded with every body open, each line of overtaken nodes left as it was. The
 * reader's own folds and pins are untouched by it, so the cycle comes back to their tree.
 */
export const CYCLE_STATES = 3;

/** The state that unfolds every branch, which is the last of the three. */
export const EVERYTHING = CYCLE_STATES - 1;

/** How deep the second state of the cycle opens the bodies. */
const BODIES_TO_LEVEL = 2;

/** Whether the cycle draws the body of a row at `level`, whatever the focus and the pins say. */
function drawsBody(cycle: number, level: number): boolean {
  return cycle === EVERYTHING || (cycle === 1 && level <= BODIES_TO_LEVEL);
}

/** The one row a branch below the drawn levels folds to: how many rows it holds, in words
 *  that agree with the count, and the control that opens them in place, at the level its nodes
 *  would stand at and under the guides of the levels above it. */
function BranchMore({
  held,
  level,
  onOpen,
}: {
  held: number;
  level: number;
  onOpen: () => void;
}): ReactNode {
  const depth = level - 1;
  return (
    <div
      aria-level={level}
      className={`relative flex min-w-0 pl-[calc(var(--row-depth)*23px+26px)] text-xs leading-7 max-[761px]:pl-[calc(var(--row-depth)*17px+22px)] ${pageQuietText}`}
      data-branch-more=""
      role="treeitem"
      style={{ "--row-depth": depth } as CSSProperties}
      tabIndex={-1}
    >
      {Array.from({ length: depth }, (_unused, guide) => (
        <span
          aria-hidden="true"
          className={`absolute inset-y-0 w-px left-[calc(var(--guide)*23px+9px)] max-[761px]:left-[calc(var(--guide)*17px+8px)] ${pageGuideLineBg}`}
          // biome-ignore lint/suspicious/noArrayIndexKey: a guide is its level and nothing else.
          key={guide}
          style={{ "--guide": guide } as CSSProperties}
        />
      ))}
      <button
        className="dispatch-inline-link font-medium underline decoration-page-hair underline-offset-[3px]"
        onClick={onOpen}
        tabIndex={-1}
        type="button"
        {...pageChrome}
      >
        {/* One run of text: below 1280 px a button is a flex box (`styles.css`), which lays the
            count's own span and the words after it out as two boxes side by side, the space
            that opens the words dropped at the start of theirs, and the line read
            "21more rows below". */}
        <span>
          <span {...authorText}>{held}</span>{" "}
          {held === 1 ? CONTROL_LABELS.deeper_row : CONTROL_LABELS.deeper_rows}
        </span>
      </button>
    </div>
  );
}
