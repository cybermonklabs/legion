import { replaceEqualDeep } from "@tanstack/react-query";
import {
  type CSSProperties,
  type ReactNode,
  type RefObject,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../api/types";
import { useCopyFeedback } from "../../hooks/useCopyFeedback";
import {
  pageFailedText,
  pageFocusedRow,
  pageFoldedHalo,
  pageGuideLineBg,
  pageInkText,
  pageKindBullet,
  pageKindTag,
  pageMutedText,
  pageOwnWordsRule,
  pageQuietText,
  pageRowAct,
  pageRowActs,
  pageRowOption,
  pageStateText,
  pageWordsText,
} from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import { DeletedBody, EditedMark } from "../refs/PostBody";
import { deletedLineText, type PostDeletion, postDeletion } from "../refs/post-marks";
import { buildReferencePath, parseNodeReference } from "../refs/routes";
import { agentRoleWord } from "./agent-role";
import { openChildEntry } from "./children-section";
import { type Clamp, clampRedrawn, sameClamp, watchClamp } from "./clamp-measurer";
import { DecisionActions } from "./DecisionActions";
import { useDocumentStage } from "./document-stage";
import { ChildIssueFacts } from "./IssueChildren";
import { bringOnScreen } from "./identity-block";
import { presetNodePath } from "./issue-address";
import { isStatement, type NodeChip, type ParentLine, type TreeNode } from "./issue-tree";
import { DETAILS_ATTRIBUTE } from "./margin-slot";
import { NodeFacts } from "./NodeFacts";
import { NodeGlyph } from "./NodeGlyph";
import { chipWords } from "./node-row-chip";
import { shortDay } from "./node-time";
import { type ThreadMark, ThreadReadMark } from "./outline-thread-read";
import { type OutlineWriter, replyItemOf, takesAnswer, useOutlineWriter } from "./outline-writer";
import { authorText, freeze, pageChrome } from "./page-structure";
import {
  ANSWER_LINES,
  type AuthorKind,
  BLOCK_TYPE_NAMES,
  CONTROL_LABELS,
  GAP_LINES,
  GAP_WORDS,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  type NodeType,
  nodeStateWord,
} from "./page-words";
import { isThreadContext, tagFilter } from "./presets";
import { documentWord, RowText } from "./RowText";
import { foldedLine, indentOf } from "./tree-fold";

/**
 * One row of the issue's outline, with everything its node shows on its face (R1, R6, R9, R11,
 * R34, and the architect's rulings 4, 5 and 6).
 *
 * A row reads from the left as the prototype's `.card` does: its bullet in its kind's colour,
 * which is also its fold control where it holds nodes, then its words, and at their end the
 * chips that name what the node points at. At the right end of its first line stand the node's
 * kind as one tag where it has a typed kind, then its author and its time in grey, all on one
 * line in a column as wide as that line needs, and under them, on a smaller line, its state
 * where the model holds one (ruling 5). The words of every node start at one left edge, so
 * nothing stands between a bullet and its words; the reader's own words carry an ink rule and
 * its indent, which is the outline's second left edge and its last (R34). At 760 px and below
 * the tag, the author, the time and the state are one small line under the words (ruling 4). A
 * node the model holds no author for draws none in that column, and an agent is named there by
 * its role's word: the row says "Author unknown" under its words, and gives an agent's whole
 * title (rulings 5 and 6).
 *
 * A row draws its node's words whole, with no limit of lines, and draws on its face what the
 * node holds beside them: an option's own words, who made it where the column does not say, the
 * facts (`NodeFacts`), the gaps in full, the counts the coverage read gives, Keep and Change on
 * a decision that waits, and the acts (R11). None of it waits for the focus, so moving the focus
 * changes no row's text and no row's height, and no element of the row carries a `title`. A
 * node the reader folded is the one place a row draws a single line: its first line
 * (`foldedLine`) and how many nodes it holds.
 *
 * The row is a `treeitem` of the tree's one tab stop: the focused row is the only row that takes
 * the focus, and nothing inside a row takes any, so Tab from outside enters the tree once and
 * Tab from the row leaves it again (R9). What a reader does inside a row they do with a key: the
 * fold has the arrow keys and `o`, and Keep, Change and a chip's link have keys of their own
 * (`useTreeKeymap.ts`).
 *
 * A view that no tree's keys reach - the conversation's rows until part I's outline replaces
 * them, part V's Verification view and part J's Spec view - passes `tabStops`, and every control
 * the row draws keeps its place in the tab order instead, so a reader on the keyboard alone Tabs
 * to each (R9).
 */
export function NodeRow({
  answerer,
  author,
  context,
  document,
  editable = false,
  editing,
  expanded,
  focused,
  foldable = true,
  folded,
  foldedMore = false,
  isClosed,
  level,
  node,
  onFocus,
  onToggle,
  ownAnswer,
  placesItself,
  report,
  rowKey,
  tabStop,
  tabStops = false,
  threadRead,
}: {
  /** The name of the person whose answer this ask carries (`AgentText.author`), where it
   *  carries one. */
  answerer?: string;
  /** Who made the node: the name the page draws an author with (`AgentText.author`), never an
   *  id (R9, R22), the kind of author, which picks its shape, and whether it is the reader, whose
   *  name is drawn in the ink and whose words carry the ink rule (R34). */
  author: { readonly kind: AuthorKind; readonly name: string; readonly reader: boolean };
  /** Whether the view draws this row only as the ancestor of a match: the row is faint and
   *  marked, and the arrow keys reach it as they reach any other row (R2). */
  context: boolean;
  /** The document this row's node is a block of, which its face writes against: a decision's
   *  Keep and Change are writes on the document its `decision` block stands in, the spec or
   *  another of the issue's (R7). Undefined for a node that is no block of a document of this
   *  issue. */
  document?: Artifact;
  /** Whether the row carries `data-editable`, for a view that lets the reader edit its words in
   *  place and draws such a row its own way: part I's outline passes it, and its styles draw the
   *  cue that the row takes typing (decision 323); nothing in part G reads it. False unless the
   *  view passes it. */
  editable?: boolean;
  /** The document this row's own text lives in, while the reader is editing it here: its
   *  editor is drawn in place of the row's words and the caret is in this node's block. */
  editing?: string;
  /** `true` when its children are drawn, `false` when it holds children the reader folded, and
   *  undefined for a node that holds none. */
  expanded?: boolean;
  focused: boolean;
  /** Whether the row draws its fold control where it holds children. The row a tree is zoomed
   *  to, drawn open by definition, draws none, and keeps its `aria-expanded`. True unless the
   *  view passes it. */
  foldable?: boolean;
  /** How many nodes it holds while it is folded. */
  folded?: number;
  /** Whether the read the row is drawn from holds nodes no page read yet holds, which its folded
   *  count may hold more of: the count carries "+" then (R15). False unless the view passes it. */
  foldedMore?: boolean;
  /** Whether the issue is closed, which the writes a row offers are refused on. */
  isClosed: boolean;
  /** How deep the row sits, the tree's first row at 1 (`aria-level`); its indent stops growing
   *  past the sixth level (`indentOf`). */
  level: number;
  node: TreeNode;
  onFocus: () => void;
  onToggle: () => void;
  /** Whether the answer this ask carries is the reader's own, which carries the ink rule. */
  ownAnswer: boolean;
  /** Whether the row brings itself on screen while it has the focus (`useRowOnScreen`), which
   *  every row does but three of the outline's: its first row as the page opens on it with no
   *  question waiting on the reader, and the row a zoom leaves the reader on under the crumb line
   *  it draws, each of which stays where it stands until the reader acts, and the row the reader
   *  is editing, which the editor's own caret keeps in view while it grows as the reader writes.
   *  All three are `IssueTree.tsx`'s to set. */
  placesItself?: boolean;
  /** What a key pressed on this row answers, where it answers in words (`ANSWER_LINES`): drawn at
   *  the row, last, until the next key, a focus move or leaving the tree clears it (R22). */
  report?: string;
  /** What tells this row from every other, including another placement of the same node
   *  (`rowKeyOf`): the row the caret was taken from is found by it (`useNodeEditing.ts`). */
  rowKey: string;
  /** Whether this row is the tree's one tab stop: the focused row, or the tree's first row
   *  before the reader has touched it. */
  tabStop: boolean;
  /** Whether every control the row draws keeps its place in the tab order: false in a tree whose
   *  keys reach them (`useTreeKeymap.ts`), true in a view that no tree's keys reach (R9). */
  tabStops?: boolean;
  /** The thread whose newest write this row draws, which a dwell on the row marks read in the
   *  reader's Inbox (`outline-thread-read.tsx`). */
  threadRead?: ThreadMark;
}): ReactNode {
  const row = useRef<HTMLDivElement | null>(null);
  const stage = useDocumentStage(editing);
  // Where the reader writes: a row's Reply and an open question's Answer set it (R16).
  const writer = useOutlineWriter();
  // A comment or a message its author deleted keeps its row, and draws the deleted line where
  // its words stood (INFRA-1523, R3).
  const deleted = postDeletion({ deleted_at: node.deletedAt, deleted_by: node.deletedBy });
  // The reader's own words: a turn a person wrote who is the reader (R34). An ask, a block or a
  // decision is not a turn, so its words are never the reader's here, whoever asked it.
  const own = author.reader && (node.type === "comment" || node.type === "message");
  const tone = context ? "context" : toneOf(node, author.kind);
  // A node the reader folded draws one line and its count, whatever the focus (R11).
  const oneLine = expanded === false;

  const press = useRowOnScreen(row, focused, BELOW_WHAT_STAYS, placesItself);

  const depth = indentOf(level);
  const edges = node.alsoUnder.length > 0 || node.chips.length > 0 || node.gaps.length > 0;
  // The node's ties and its gap words, at the end of its words, on their line, as the
  // prototype's `.trail` stands inside its `.body` (ruling 5). Where the words are being edited,
  // they stand under them instead, so the editor never hides one.
  const trail = !edges ? null : (
    <span
      className={`ml-1.5 inline-flex max-w-full flex-wrap items-baseline gap-x-2 align-baseline text-xs leading-6 ${pageQuietText}`}
      data-node-trail=""
    >
      <NodeEdges node={node} tabStops={tabStops} />
    </span>
  );
  const trailInWords = editing === undefined;
  // The view a node's kind tag opens it in, at its own block (R9).
  const tagView = tagFilter(node);
  const tagTo = tagView === undefined ? undefined : presetNodePath(tagView, node);
  return (
    <div
      aria-expanded={expanded}
      aria-level={level}
      className="relative min-w-0 pl-[calc(var(--row-depth)*23px)] outline-none max-[761px]:pl-[calc(var(--row-depth)*17px)]"
      data-editable={editable ? "" : undefined}
      data-folded-count={folded}
      data-node-context={context ? "" : undefined}
      data-node-id={node.id}
      data-node-row=""
      data-node-state={node.state}
      data-node-type={node.type}
      data-row-key={rowKey}
      onFocus={onFocus}
      onPointerDown={press}
      ref={row}
      role="treeitem"
      style={{ "--row-depth": depth } as CSSProperties}
      tabIndex={tabStop ? 0 : -1}
    >
      <IndentGuides depth={depth} />
      <div className={`${NODE_ROW_LAYOUT.box} ${focused ? pageFocusedRow : ""}`} data-node-card="">
        <Bullet
          collapsed={oneLine}
          expanded={expanded}
          foldable={foldable}
          onToggle={onToggle}
          tabStops={tabStops}
          tone={tone}
        />
        <div
          className={`${NODE_ROW_LAYOUT.content} ${own ? pageOwnWordsRule : ""}`}
          data-node-content=""
          data-node-own={own ? "" : undefined}
        >
          {oneLine ? (
            <p
              className={`text-[15px] leading-6 break-words ${context ? pageQuietText : pageWordsText}`}
            >
              <span data-node-first-line="" {...authorText}>
                {deleted === null ? foldedLine(node) : <DeletedLine deleted={deleted} />}
              </span>
              {folded === undefined ? null : (
                <span className={`ml-1.5 text-xs ${pageQuietText}`}>
                  <span className="whitespace-nowrap" data-node-count="" {...pageChrome}>
                    <span {...authorText}>
                      {folded.toLocaleString()}
                      {foldedMore ? "+" : ""}
                    </span>{" "}
                    {folded === 1 && !foldedMore
                      ? CONTROL_LABELS.folded_row
                      : CONTROL_LABELS.folded_rows}
                  </span>
                </span>
              )}
            </p>
          ) : (
            <div
              className={`relative text-[15px] leading-6 ${
                context ? pageQuietText : own ? pageInkText : pageWordsText
              }`}
              data-node-words=""
            >
              {editing !== undefined ? (
                <div className="flex min-w-0 flex-col" ref={stage} {...authorText} />
              ) : deleted !== null ? (
                <p className="break-words" {...authorText}>
                  <DeletedLine deleted={deleted} />
                </p>
              ) : node.child === undefined ? (
                <RowText
                  focused={focused}
                  markdown={node.text}
                  source={node.source}
                  tabStops={tabStops}
                  tree={node.tree}
                />
              ) : (
                // A child issue's row is one line, as the prototype's `.kr` draws it: its title,
                // then how it stands, what blocks it and who holds it, small and grey (ruling 5).
                <p className="break-words">
                  <span {...authorText}>{node.text}</span>
                  <ChildIssueFacts child={node.child} tabStops={tabStops} />
                </p>
              )}
              {trailInWords ? trail : null}
            </div>
          )}
          {oneLine || trailInWords || trail === null ? null : (
            <p className="flex min-w-0 flex-wrap items-baseline">{trail}</p>
          )}
          {node.options === undefined || oneLine ? null : (
            <NodeOptions body node={node} tabStops={tabStops} writer={writer} />
          )}
          {node.answer === undefined || oneLine ? null : (
            <AskAnswer answerer={answerer ?? ""} node={node} own={ownAnswer} />
          )}
          {/* Under a filter, the node's parent where the filter draws it as no row: one line,
              its kind's word and its first line (R2, R1's first-line rule). */}
          {node.parentLine === undefined ? null : (
            <p
              className={`flex min-w-0 items-baseline gap-1 text-xs leading-5 ${pageQuietText}`}
              data-node-parent-line=""
              data-parent-id={node.parentLine.id}
              data-parent-type={node.parentLine.type}
            >
              <span className="shrink-0" {...pageChrome}>
                {NODE_TYPE_NAMES[node.parentLine.type]}
              </span>
              <ParentLink line={node.parentLine} tabStops={tabStops} />
            </p>
          )}
          {oneLine ? null : (
            <NodeFace
              author={author}
              document={document}
              isClosed={isClosed}
              node={node}
              tabStops={tabStops}
            />
          )}
          <RowActs
            node={node}
            ownAnswer={ownAnswer}
            ownComment={own && node.type === "comment"}
            replies={!(context && isThreadContext(node))}
            tabStops={tabStops}
            writer={writer}
          />
          {report === undefined ? null : (
            <p
              className={`text-xs leading-5 ${pageMutedText}`}
              data-row-report=""
              role="status"
              {...pageChrome}
            >
              {report}
            </p>
          )}
        </div>
        <RowMargin
          author={author}
          context={context}
          node={node}
          tabStops={tabStops}
          {...(tagTo === undefined ? {} : { tagTo })}
          tone={tone}
        />
        {threadRead === undefined || oneLine ? null : <ThreadReadMark mark={threadRead} />}
      </div>
    </div>
  );
}

/** One faint guide per indented level, `depth` of them, each at the middle of that level's
 *  bullet (the prototype's `.tn > .kids`, 9 px into a level 23 px wide, and 8 px into one 17 px
 *  wide on a phone): what a row of the outline draws to its left, and the row that pages a
 *  node's children (`TreeRows.tsx`). The rows stand one under another, so a level's guides join
 *  into one line down its whole branch. */
export function IndentGuides({ depth }: { depth: number }): ReactNode {
  return Array.from({ length: depth }, (_unused, guide) => (
    <span
      aria-hidden="true"
      className={`absolute inset-y-0 w-px left-[calc(var(--guide)*23px+9px)] max-[761px]:left-[calc(var(--guide)*17px+8px)] ${pageGuideLineBg}`}
      // biome-ignore lint/suspicious/noArrayIndexKey: a guide is its level and nothing else.
      key={guide}
      style={{ "--guide": guide } as CSSProperties}
    />
  ));
}

/**
 * A deleted post's line where its row draws its words, whole or as its one line, and where the
 * page draws its one line apart from its row - a filter's parent line (`ParentLink`), a crumb
 * above a zoom (`IssueTree.tsx`'s `CrumbLine`) (INFRA-1523, R3): the line every surface draws in
 * a deleted post's place (`DeletedBody`), in its italic, with who deleted it and when. The actor
 * is named as the page names an author (`AgentText.author`), the rule the row's own author and
 * "Kept by" are named by, by INFRA-1196's decision 342: a session nothing names reads as the
 * page's word for an agent, and never as its short id. The node's text is the same line as the
 * model writes it (`tree-drafts.ts`'s `deletedPostLine`), with the actor named as no registry
 * knows it and the time as stored, so the row draws the line from the delete and never from that
 * text. The line's words are the page's own, marked as its chrome and declared in
 * `page-words.ts` (`CONTROL_LABELS.post_deleted_by`), and the actor's name and the time are not
 * (INFRA-1523, R3).
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

/** The bullet's box, as every row draws it: 24 px tall, its dot centred in it. */
const BULLET_BOX =
  "col-start-1 row-start-1 flex h-6 w-[18px] items-center justify-center self-start";

/**
 * Where the parts of a row stand (the prototype's `.card`, `.bul`, `.nc` and `.mg`, and its
 * media query at 760 px), for every row that reads as the outline's: this one, and a filter's own
 * row, which c-row reads by the same marks.
 *
 * From 761 px up the row is three columns: the bullet's 18 px, the words, and the column of the
 * kind's tag, the author and the time, which is as wide as that one line needs and never more
 * than two fifths of the row; a state takes a second, smaller line under it (ruling 5). At 760 px
 * and below the bullet's column is 16 px and the tag, the author, the time and the state stand on
 * one small line under the words, the tag first (ruling 4, the 390 px pictures).
 *
 * A phone class is `max-[761px]:`, not `max-[760px]:`: Tailwind writes an arbitrary `max-[N]`
 * as `(width < N)`, so only 761 draws 760 px itself as a phone, as the prototype's query and the
 * header's (`HEADER_SCROLLS_QUERY`, `(max-width: 760px)`) do. The row's indent and its guides
 * follow the same rule.
 */
export const NODE_ROW_LAYOUT = {
  /** The bullet's column: 18 px, and 16 px on a phone, its dot at the column's middle. */
  bullet: `${BULLET_BOX} max-[761px]:w-4`,
  /** The bullet of a node that holds others, which is its fold control: on a phone 24 px
   *  square, the least target WCAG 2.5.8 allows, reaching 4 px left of the column and 4 px into
   *  the 6 px before the words, as the prototype's does, so it covers none of them, with its dot
   *  kept at the column's middle, where the guide of the level under it hangs. */
  foldControl: `${BULLET_BOX} cursor-pointer max-[761px]:-ml-1 max-[761px]:w-6 max-[761px]:justify-start max-[761px]:pl-[8.5px]`,
  /** The box the row's parts stand in (the prototype's `.card`). Below 768 px the words stand
   *  6 px past the bullet's column, as the prototype's do (the architect's ruling on this part's
   *  pull request). Not named `card`: `text-on-background.test.ts` reads that name as
   *  `classes.ts`'s `card` ground, which a row is not drawn on. */
  box: "relative grid min-w-0 grid-cols-[18px_minmax(0,1fr)_fit-content(40%)] gap-x-2 rounded-md py-0.5 pr-1.5 max-[768px]:gap-x-1.5 max-[761px]:grid-cols-[16px_minmax(0,1fr)] max-[761px]:pr-1",
  /** The words' column, and everything the row draws under them. */
  content: "col-start-2 row-start-1 min-w-0",
  /** The column of the tag, the author, the time and the state, and on a phone their line. */
  margin:
    "col-start-3 row-start-1 min-w-0 self-start text-right text-xs leading-6 max-[761px]:col-start-2 max-[761px]:row-start-2 max-[761px]:text-left max-[761px]:leading-4",
  /** Its first line: the tag, then the author and the time. */
  marginLine:
    "flex flex-wrap items-baseline justify-end gap-x-[7px] max-[761px]:mr-1.5 max-[761px]:inline-flex max-[761px]:justify-start max-[761px]:gap-x-1.5",
  /** Its second, smaller line: the state; on a phone, after the first on the same line. */
  marginState:
    "flex flex-wrap items-center justify-end gap-x-1.5 leading-4 font-semibold max-[761px]:inline-flex max-[761px]:justify-start",
} as const;

/**
 * Brings the focused row inside the viewport and clear of the sticky identity block after every
 * move, by the one rule the whole page places a target with (R9, `identity-block.ts`), then below
 * everything else that stays at the top of the screen above the rows (`belowWhatStays`); and
 * again whenever the row grows under it: a body draws more once it is open - the editor a
 * reader puts the caret in arrives on the row's stage after the row has the focus
 * (`document-stage.tsx`) - and the text would otherwise push its foot off the screen.
 *
 * Except while a press inside the row is held. A press on an unfocused row focuses it on
 * mousedown, before the press ends, and a scroll then moves what the pointer went down on: the
 * press ends on something else and the control it was on never hears it. Measured at 1440 px on
 * the Decisions preset, a row whose top sat 24 px under the identity block scrolled 32 px
 * between mousedown and mouseup, and the reader's Keep was never sent. The reader pressed what
 * they could see, so there is nothing to bring on screen.
 *
 * Answers the row's own `onPointerDown`, which marks a press. Every row of the outline and of a
 * filter's body places itself by this one hook. `below` is how far below what stays a row that
 * lay under it is placed, `BELOW_WHAT_STAYS` unless a view places its rows by a step of its own,
 * as a view whose sticky heads stand taller than the outline's does.
 *
 * Not while `placesItself` is false: then a focused row is placed neither as it takes the focus
 * nor as it grows. The outline's first row the page opens on is one: a row taller than the
 * screen would be put at its top under the band, and the page would open scrolled past its
 * header. Read when the row is placed rather than as a cause of placing it, so the row turning
 * back to placing itself moves nothing at that moment.
 */
export function useRowOnScreen(
  row: RefObject<HTMLElement | null>,
  focused: boolean,
  below: number = BELOW_WHAT_STAYS,
  placesItself = true
): () => void {
  const pressed = useRef(false);
  const placing = useRef(placesItself);
  placing.current = placesItself;
  const press = () => {
    pressed.current = true;
    const ended = new AbortController();
    const release = () => {
      pressed.current = false;
      ended.abort();
    };
    window.addEventListener("pointerup", release, { capture: true, signal: ended.signal });
    window.addEventListener("pointercancel", release, { capture: true, signal: ended.signal });
  };
  useLayoutEffect(() => {
    const element = row.current;
    if (!focused || element === null) return;
    const place = () => {
      if (!placing.current) return;
      if (pressed.current) return;
      bringOnScreen(element);
      belowWhatStays(element, below);
    };
    place();
    if (typeof ResizeObserver === "undefined") return;
    const grown = new ResizeObserver(place);
    grown.observe(element);
    return () => grown.disconnect();
  }, [below, focused, row]);
  return press;
}

/** How far below what stays a row that lay under it is placed, as `bringOnScreen` places a
 *  target below the band: enough that it does not sit flush against it. */
const BELOW_WHAT_STAYS = 8;

/**
 * Scrolls `element`, which `bringOnScreen` has placed, down to `below` px below everything that
 * stays at the top of the screen above the rows where its top still lies under it: at 760 px and
 * below, a focused row there lay under the view row that stays. That height is the page's
 * `scroll-padding-top`, which `styles.css` keeps equal to it, so this reads the padding itself
 * and no height of its own. `bringOnScreen` clears the header's band alone, which at 760 px and
 * below, under a header that publishes no band, leaves the view row that stays over the row.
 * Where the page sets no padding (`auto`), there is nothing more to clear.
 *
 * A row in Details' drawer is brought into the drawer's own scroll box, which stands over the
 * page and nothing of the page's top covers, so it is left where `bringOnScreen` put it.
 */
function belowWhatStays(element: HTMLElement, below: number): void {
  if (element.closest(`aside[${DETAILS_ATTRIBUTE}]`) !== null) return;
  const stays = Number.parseFloat(getComputedStyle(document.documentElement).scrollPaddingTop);
  if (!Number.isFinite(stays)) return;
  const top = element.getBoundingClientRect().top;
  if (top < stays) window.scrollBy({ top: top - stays - below });
}

/** The kinds of node the page draws no tag for: what somebody said, which is the conversation's
 *  own words rather than a kind it revealed (R34: "as one tag where the node has a typed
 *  kind"). Every other kind is a typed node, and its tag names it. */
export const UNTYPED: Readonly<Partial<Record<NodeType, true>>> = { comment: true, message: true };

/** A node's colour, as `pageKindBullet` and `pageKindTag` name them, by what it is and, for a
 *  turn, by who wrote it: a person's words are the ink, an agent's the quiet grey. */
export type Tone = keyof typeof pageKindBullet;

function toneOf(node: Pick<TreeNode, "statement" | "type">, author: AuthorKind): Tone {
  switch (node.type) {
    case "ask":
      return "question";
    case "check":
    case "result":
      return "check";
    case "child_issue":
      return "child";
    case "comment":
    case "message":
      return author === "person" ? "person" : "agent";
    // A criterion is what a person wants to be able to do, in their own words (`page-words`), a
    // node only a person writes: it takes a person's ink.
    case "criterion":
      return "person";
    case "decision":
      return "decision";
    // A reading is an agent's reading of a person's answer, inside the ask or the decision it
    // reads, and always an agent's words: it takes an agent's quiet grey, never the colour of the
    // question or the decision it reads, so it never reads as the person's decision itself (R9).
    case "reading":
      return "agent";
    case "plan_task":
      return "step";
    case "requirement":
      // Only the spec's own requirement is a statement (`isStatement`); one of any other
      // document is a block and nothing more, in a block's colour (decision 246).
      return isStatement(node) ? "statement" : "issue";
    case "callout":
    case "document":
    case "handoff":
    case "image":
    case "issue":
      return "issue";
  }
}

/** The colour a state is drawn in, beside its word and its shape (R6): what waits on the reader,
 *  and a decision the reader kept or asked to change, in the decisions' violet. */
function stateTone(node: TreeNode): keyof typeof pageStateText {
  if (node.waiting) return node.type === "decision" ? "decision" : "waiting";
  if (node.type === "decision" && (node.state === "kept" || node.state === "change_requested")) {
    return "decision";
  }
  if (node.type === "check" && node.state === "passed") return "passed";
  if (node.type === "check" && node.state === "failed") return "failed";
  return "quiet";
}

/**
 * The bullet: a 7 px dot in the node's colour in an 18 px column (the prototype's `.bul`), and
 * the node's fold control where it holds nodes, named by what pressing it does. A folded
 * branch's dot keeps a halo, as the prototype's `.card.collapsed` does, beside the count of what
 * it holds. Out of the tab order, as every control inside a row is (R9), unless `tabStops` holds.
 * A press leaves the focus where it is, as an option and the acts do: the fold it makes is the
 * whole of what the reader asked for (R11). On a phone the fold control's box is 24 px square,
 * the least target WCAG 2.5.8 allows (`NODE_ROW_LAYOUT.foldControl`).
 */
function Bullet({
  collapsed,
  expanded,
  foldable,
  onToggle,
  tabStops,
  tone,
}: {
  collapsed: boolean;
  expanded?: boolean;
  foldable: boolean;
  onToggle: () => void;
  tabStops: boolean;
  tone: Tone;
}): ReactNode {
  const dot = (
    <i
      aria-hidden="true"
      className={`block size-[7px] rounded-full ${pageKindBullet[tone]} ${collapsed ? pageFoldedHalo : ""}`}
    />
  );
  if (!foldable) {
    return (
      <span className={NODE_ROW_LAYOUT.bullet} data-node-bullet="">
        {dot}
      </span>
    );
  }
  if (expanded === undefined) {
    return (
      <span className={NODE_ROW_LAYOUT.bullet} data-node-bullet="">
        {dot}
      </span>
    );
  }
  return (
    <button
      aria-expanded={expanded}
      aria-label={expanded ? CONTROL_LABELS.fold : CONTROL_LABELS.unfold}
      className={NODE_ROW_LAYOUT.foldControl}
      data-node-bullet=""
      onClick={onToggle}
      onMouseDown={(event) => event.preventDefault()}
      tabIndex={tabStops ? undefined : -1}
      type="button"
    >
      {dot}
    </button>
  );
}

/** What a row's edited mark draws its element with (`EditedMark`): `data-node-edited`, and the
 *  page's chrome, since ` · edited` is the page's own word. The outline's row draws it in its
 *  margin (`RowMargin`), and the Verification view's row of a node an address named after its
 *  kind (`VerificationFilterBody.tsx`'s `OtherRow`). */
export const NODE_EDITED_MARKS: Readonly<Record<string, string>> = freeze({
  "data-node-edited": "",
  ...pageChrome,
});

/**
 * What stands at the right end of the row's first line (R9, R34, the prototype's `.mg`): the
 * node's kind as one tag - its word and its shape, in its colour - where it has a typed kind,
 * then who made it, by a name and a shape, and when, in the page's short form, all on one line;
 * and under them its state, by a word and a shape, with the one word a decision the model marks
 * unsure says of itself, on a second, smaller line (ruling 5). All of it is grey but the tag, the
 * reader's own name and a state that asks something of the reader. A node the model holds no
 * author for draws none here: its row says so under its words (`BodyAuthor`, ruling 5). From
 * 761 px up it is a column as wide as its first line; on a phone it is one small line under the
 * words (ruling 4). Where `tagTo` is given, the tag is a link that opens the node in its own view
 * (R9), out of the tab order unless `tabStops` holds: `f` steps to it and Enter follows it.
 */
export function RowMargin({
  author,
  context,
  node,
  tabStops,
  tagTo,
  tone,
}: {
  author: { readonly kind: AuthorKind; readonly name: string; readonly reader: boolean };
  context: boolean;
  node: TreeNode;
  tabStops: boolean;
  /** The address the node's kind tag opens: its own view at the node (`presets.ts`'
   *  `tagFilter`). Undefined where the tag opens nothing. */
  tagTo?: string;
  tone: Tone;
}): ReactNode {
  const stateGlyphs: Readonly<Record<string, Readonly<Record<string, string>>>> =
    GLYPH_TOKENS.states;
  const stateGlyph = node.state === undefined ? undefined : stateGlyphs[node.type]?.[node.state];
  const time = node.at === undefined ? "" : shortDay(node.at);
  const authored = author.kind !== "unknown";
  const agentText = useAgentText();
  if (node.child !== undefined) {
    // A child issue's tag is its key, which opens the child in the children's section above the
    // outline (R9, R14), where its entry links to its own page; its row names no author, no time
    // and no state, which its facts after its title say (ruling 5, the prototype's `.kr`).
    const key = node.child.key;
    return (
      <div className={`${NODE_ROW_LAYOUT.margin} ${pageQuietText}`} data-node-margin="">
        <div className={NODE_ROW_LAYOUT.marginLine} data-node-margin-line="">
          {/* A plain span: its control stays inline, so the 6 px a control's touch target takes
              above and below it on a phone adds nothing to the line's height. */}
          <span data-node-tag="">
            <button
              className={`dispatch-inline-link font-semibold whitespace-nowrap ${pageKindTag.child}`}
              data-child-key=""
              onClick={() => openChildEntry(key)}
              tabIndex={tabStops ? undefined : -1}
              type="button"
              {...authorText}
            >
              {key}
            </button>
          </span>
        </div>
      </div>
    );
  }
  return (
    <div className={`${NODE_ROW_LAYOUT.margin} ${pageQuietText}`} data-node-margin="">
      <div className={NODE_ROW_LAYOUT.marginLine} data-node-margin-line="">
        {UNTYPED[node.type] === true ? null : (
          <span
            className={`inline-flex items-center gap-1 font-semibold whitespace-nowrap ${pageKindTag[tone]}`}
            data-node-tag=""
          >
            {/* A `requirement` block of a document other than the spec is no statement: it says
                its type's own word and draws no statement's shape (decision 246), as a `check`
                block outside the Verification document draws no state word (decision 152). */}
            {tagTo === undefined ? (
              <KindTag node={node} />
            ) : (
              <Link
                className="dispatch-inline-link inline-flex items-center gap-1 underline decoration-page-hair underline-offset-[3px]"
                data-node-tag-link=""
                tabIndex={tabStops ? undefined : -1}
                to={tagTo}
              >
                <KindTag node={node} />
              </Link>
            )}
          </span>
        )}
        {!authored && time === "" ? null : (
          <span className="min-w-0 [overflow-wrap:anywhere]" data-node-byline="">
            {/* Who made it, by a word and a shape: one shape per kind of author, so a person and
                an agent never share one (R9). The name is the page's one name for an author
                (`AgentText.author`), never an id (R22). */}
            {authored ? (
              <span
                className="inline-flex items-center gap-1"
                data-author-kind={author.kind}
                data-node-author=""
              >
                <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors[author.kind]} />
                <span
                  className={author.reader && !context ? `font-semibold ${pageInkText}` : ""}
                  {...authorText}
                >
                  {/* An agent by its role's word; the line under its words gives the whole
                      title (ruling 6). */}
                  {author.kind === "agent" ? agentRoleWord(author.name) : author.name}
                </span>
              </span>
            ) : null}
            {time === "" ? null : (
              <>
                {authored ? <span {...pageChrome}> · </span> : null}
                <span className="whitespace-nowrap" data-node-time="" {...authorText}>
                  {time}
                </span>
              </>
            )}
            {/* A post its author edited says so after its time, as its byline does on every
                surface (`EditedMark`, INFRA-1523, R2): here at the right of the row, which no
                fold of its words reaches, so a post folded to one line keeps it.
                The mark is the page's own word (`CONTROL_LABELS.post_edited`). */}
            <EditedMark
              at={node.editedAt}
              className="whitespace-nowrap"
              marks={NODE_EDITED_MARKS}
            />
          </span>
        )}
      </div>
      {node.state === undefined && node.unsure !== true ? null : (
        <div className={NODE_ROW_LAYOUT.marginState}>
          {node.state === undefined ? null : (
            <span
              className={`inline-flex items-center gap-1 ${context ? "" : pageStateText[stateTone(node)]}`}
            >
              {stateGlyph === undefined ? null : (
                <NodeGlyph className="size-3 shrink-0" token={stateGlyph} />
              )}
              <span data-node-state={node.state} {...pageChrome}>
                {nodeStateWord(node.state)}
                {/* "Kept by" names the person who kept it, by the page's one name for them (R2). */}
                {node.state === "kept" && node.keptBy !== undefined ? (
                  <>
                    {" "}
                    <span data-kept-by="" {...authorText}>
                      {agentText.author(node.keptBy)}
                    </span>
                  </>
                ) : null}
              </span>
            </span>
          )}
          {node.unsure === true ? (
            <span data-node-unsure="" {...pageChrome}>
              {CONTROL_LABELS.unsure}
            </span>
          ) : null}
        </div>
      )}
    </div>
  );
}

/** A typed node's kind, as its tag draws it: its type's word and shape, and the word alone for a
 *  `requirement` block of a document other than the spec, which is no statement (decision
 *  246). */
function KindTag({ node }: { node: TreeNode }): ReactNode {
  return node.type === "requirement" && !isStatement(node) ? (
    <span {...pageChrome}>{BLOCK_TYPE_NAMES.requirement}</span>
  ) : (
    <>
      <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.types[node.type]} />
      <span {...pageChrome}>{NODE_TYPE_NAMES[node.type]}</span>
    </>
  );
}

/**
 * The choices an ask offers, under its question (the prototype's `.opts`): each by its own
 * words, the one the person chose filled and in the ink, the others hollow and grey, and while
 * the question waits on the reader every one in the waiting colour. The words an option carries
 * beside its name are drawn where `body` holds: on every row of the outline (R11), and in the
 * body of a view that draws one (`DecisionsRow.tsx`).
 *
 * An open question's options are the choices of its answer (R16, the prototype's `button.opt`):
 * a press picks one, or lets it go, and sets the line at the foot of the outline to answer the
 * question, where the keyboard picks them too; the one picked is drawn on the waiting tint.
 */
export function NodeOptions({
  body,
  node,
  tabStops = false,
  writer,
}: {
  body: boolean;
  node: TreeNode;
  /** Whether the options keep their place in the tab order (`NodeRow`'s `tabStops`). */
  tabStops?: boolean;
  writer: OutlineWriter | undefined;
}): ReactNode {
  const chosen = new Set(node.answer?.selected ?? []);
  const answering = writer !== undefined && takesAnswer(node);
  const picks =
    writer?.answer !== undefined && writer.answer.nodeId === node.id ? writer.answer.selected : [];
  return (
    <ul className="my-0.5 text-sm leading-[21px]" data-node-options="">
      {(node.options ?? []).map((option) => {
        const picked = chosen.has(option.label);
        const words = (
          <>
            <span aria-hidden="true" className="w-3 shrink-0 text-center text-xs" {...pageChrome}>
              {picked || picks.includes(option.label) ? "●" : "○"}
            </span>
            <span className="min-w-0 break-words">
              <span {...authorText}>{option.label}</span>
              {body && option.description !== undefined ? (
                <span className={`font-normal ${pageQuietText}`}>
                  <span {...pageChrome}> — </span>
                  <span {...authorText}>{option.description}</span>
                </span>
              ) : null}
            </span>
          </>
        );
        return (
          <li
            className={
              answering
                ? ""
                : `flex items-baseline gap-2 ${
                    picked
                      ? `font-semibold ${pageInkText}`
                      : node.waiting
                        ? pageStateText.waiting
                        : pageQuietText
                  }`
            }
            data-node-option={picked ? "chosen" : ""}
            key={option.label}
          >
            {answering ? (
              // Out of the tab order, as every control inside a row is: the line the press sets
              // draws the same options as its own chips, which the keyboard reaches (R9). A
              // press leaves the focus where it is until it ends, so the press ends on the option
              // it went down on, in a view whose body opens on focus as much as on the outline's
              // rows. Once the press has picked, the focus goes to the row: one press picks the
              // option and moves the reader to the row it is drawn on (decision 120).
              <button
                aria-pressed={picks.includes(option.label)}
                className={`dispatch-inline-link ${pageRowOption} ${
                  node.waiting ? pageStateText.waiting : pageQuietText
                }`}
                data-pick-option={option.label}
                onClick={(event) => {
                  writer.pick(node, option.label);
                  const row = event.currentTarget.closest<HTMLElement>("[data-node-row]");
                  if (row !== null && !row.contains(document.activeElement)) {
                    row.focus({ preventScroll: true });
                  }
                }}
                onMouseDown={(event) => event.preventDefault()}
                tabIndex={tabStops ? undefined : -1}
                type="button"
              >
                {words}
              </button>
            ) : (
              words
            )}
          </li>
        );
      })}
    </ul>
  );
}

/**
 * What the reader can do with a row, on one quiet line under it (the prototype's `.acts`), drawn
 * on every row of the outline whatever the focus (R11): Reply on every row that takes a reply - a
 * comment, a message, an ask whatever its state, an answered or a resolved one included, and the
 * issue itself, whose reply is a new comment on it (R16), but never a comment or a message its
 * author deleted, which Dispatch takes no reply to (`replyItemOf`), nor a row the view draws with
 * `replies` off - Answer on an open question, Change answer on a question the reader answered,
 * and Edit on the reader's own comment (`ownComment`). Each sets the line at the foot of the
 * outline and puts the caret in it; `r` and `a` press Reply and Answer from the keyboard
 * (`useTreeKeymap.ts`). A typed node - a statement, a check, a decision - takes no reply in
 * today's model, which holds no reply to a block, so it draws none; and with no writer
 * (`OutlineWriterProvider`) the row draws none of the four.
 *
 * Last, Copy reference copies the node's `dispatch://` address, as today's page offers it on the
 * same node (part G's R6, root R19). It writes nothing, so it is drawn whether or not a writer
 * is, and it carries a mark of its own (`data-row-copy`) rather than `data-row-act`, which names
 * the writer's acts alone. Its answer, copied or failed, is said after it in the page's words.
 *
 * A press leaves the focus where it is until the action moves it to the line, as the row's other
 * controls do, so the press never lands on a row that moved under it. Each is out of the tab
 * order unless `tabStops` holds (`NodeRow`'s).
 */
export function RowActs({
  node,
  ownAnswer,
  ownComment = false,
  replies = true,
  tabStops = false,
  writer,
}: {
  node: TreeNode;
  ownAnswer: boolean;
  /** Whether the row is the reader's own comment, which carries Edit. */
  ownComment?: boolean;
  /** Whether the row carries Reply where its node takes one: not on a message's context row,
   *  whose thread is replied to in Threads (R9, `presets.ts`' `isThreadContext`). */
  replies?: boolean;
  tabStops?: boolean;
  writer: OutlineWriter | undefined;
}): ReactNode {
  const { copy, status } = useCopyFeedback();
  const acts: { readonly act: string; readonly label: string; readonly run: () => void }[] = [];
  if (writer !== undefined && takesAnswer(node)) {
    acts.push({ act: "answer", label: CONTROL_LABELS.answer, run: () => writer.answerNode(node) });
  }
  if (writer !== undefined && ownAnswer && node.type === "ask" && node.state === "answered") {
    acts.push({
      act: "change-answer",
      label: CONTROL_LABELS.change_answer,
      run: () => writer.changeAnswer(node),
    });
  }
  const item = replyItemOf(node);
  if (writer !== undefined && replies && (item !== undefined || node.type === "issue")) {
    acts.push({
      act: "reply",
      label: CONTROL_LABELS.reply,
      run: () => (item === undefined ? writer.writeOnIssue() : writer.replyTo(item)),
    });
  }
  if (writer !== undefined && ownComment && item?.kind === "comment") {
    acts.push({
      act: "edit",
      label: CONTROL_LABELS.edit_comment,
      run: () => writer.edit(item.id, node.text),
    });
  }
  const reference = node.reference;
  if (acts.length === 0 && reference === undefined) return null;
  return (
    <p className={pageRowActs} data-row-acts="">
      {acts.map(({ act, label, run }) => (
        <button
          className={`dispatch-inline-link ${pageRowAct}`}
          data-row-act={act}
          key={act}
          onClick={run}
          onMouseDown={(event) => event.preventDefault()}
          tabIndex={tabStops ? undefined : -1}
          type="button"
          {...pageChrome}
        >
          {label}
        </button>
      ))}
      {reference === undefined ? null : (
        <button
          className={`dispatch-inline-link ${pageRowAct}`}
          data-row-copy=""
          onClick={() => copy(reference)}
          onMouseDown={(event) => event.preventDefault()}
          tabIndex={tabStops ? undefined : -1}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.copy_reference}
        </button>
      )}
      {status === "idle" ? null : (
        <span
          aria-live="polite"
          className={status === "copied" ? pageStateText.passed : pageFailedText}
          {...pageChrome}
        >
          {status === "copied" ? ANSWER_LINES.copied : ANSWER_LINES.copy_failed}
        </span>
      )}
    </p>
  );
}

/**
 * A person's answer to an ask, which is part of the ask and no node of its own
 * (R1): the page's word for it, who answered by
 * a name and a shape as every author is drawn (R9), what they chose where the ask offers no list
 * to mark it in, and what they wrote, whole and character for character, keeping the line breaks
 * they typed (R9, ruling 1). The reader's own answer carries the ink rule their own words carry
 * (R34).
 */
export function AskAnswer({
  answerer,
  node,
  own,
}: {
  answerer: string;
  node: TreeNode;
  own: boolean;
}): ReactNode {
  const answer = node.answer;
  if (answer === undefined) return null;
  const kind = answer.by.kind === "user" ? "person" : "agent";
  return (
    <div
      className={`mt-1 flex min-w-0 flex-col ${own ? pageOwnWordsRule : ""}`}
      data-node-answer=""
    >
      <p
        className={`flex min-w-0 flex-wrap items-center gap-x-1.5 text-xs leading-5 ${pageQuietText}`}
      >
        <span {...pageChrome}>{CONTROL_LABELS.answer}</span>
        <span className="inline-flex items-center gap-1" data-answer-author="">
          <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors[kind]} />
          <span className={own ? `font-semibold ${pageInkText}` : ""} {...authorText}>
            {answerer}
          </span>
        </span>
      </p>
      {node.options !== undefined || answer.selected.length === 0 ? null : (
        <ul className={`flex min-w-0 flex-col text-sm leading-[21px] ${pageInkText}`}>
          {answer.selected.map((option) => (
            <li
              className="break-words font-semibold"
              data-answer-choice=""
              key={option}
              {...authorText}
            >
              {option}
            </li>
          ))}
        </ul>
      )}
      {answer.text === "" ? null : (
        <p
          className={`text-[15px] leading-6 whitespace-pre-wrap break-words ${own ? pageInkText : pageWordsText}`}
          data-answer-text=""
          {...authorText}
        >
          {answer.text}
        </p>
      )}
    </div>
  );
}

/** Every edge of the node that is not the spine, and every gap on it, at the end of its words
 *  (the prototype's `.trail`): the chips naming what it also sits under and what it points at,
 *  each with the page's word for its tie where the model gives one, and the one word each gap is
 *  known by, which the row's face says in full (R4, R10, ruling 5). */
export function NodeEdges({
  node,
  tabStops = false,
}: {
  node: TreeNode;
  /** Whether a chip's link keeps its place in the tab order (`NodeRow`'s `tabStops`). */
  tabStops?: boolean;
}): ReactNode {
  return (
    <>
      {node.alsoUnder.map((chip) => (
        <Chip
          chip={chip}
          key={`also:${chip.ref}`}
          label={CONTROL_LABELS.also_under}
          tabStops={tabStops}
        />
      ))}
      {node.chips.map((chip) => (
        <Chip chip={chip} key={chip.ref} label={chip.label} tabStops={tabStops} />
      ))}
      {node.gaps.map((gap) => (
        <span data-gap="" data-gap-kind={gap} key={gap} {...pageChrome}>
          {GAP_WORDS[gap]}
        </span>
      ))}
    </>
  );
}

/**
 * One tie of the node that is not the spine, on the node's own line (ruling 5): the page's word
 * for the tie where it has one, then the target by its short name where the model holds one, and
 * until then by its first words, cut at a word within 32 characters (`chipWords`), as the link a
 * reader follows to it; a whole document by the page's word for one, because today's model
 * holds no title for a document and its name is its file name (`documentWord`, part G's R4). No
 * address, slug or document name is drawn in its place (R22), save one: a short reference that
 * two or more targets answer to (`ambiguous`) names no one target, so it is drawn as its author
 * wrote it, as words with no link, and never as a target deleted (decision 62). A target nothing
 * answers to now says so. A chip whose target the batched reference read has not named yet
 * draws nothing until it does - "the names come last and all at once" (`useIssueTree.ts`) - and
 * neither does one the read named with no words at all: there is nothing to name it by. A
 * target's name is plain words already (`tree-drafts.ts`'s `treeContext` names every kind of
 * target in plain words, decisions 66 and 88), so it is drawn as it stands, with no markdown
 * pass: a pass would read "1. Stand the harness up." as a list and draw it without its number.
 *
 * A comment's or a message's chip names a post its author deleted by the deleted line without
 * its time, in the deleted line's italic, as a link to the post does (`RefLink`), the actor named
 * as the row names it (`DeletedLine`), where the model's name holds it as no registry knows it;
 * and a post its author edited by its words cut to the chip's length, then ` · edited`
 * (`EditedMark`), drawn after the cut, so no cut takes the mark (INFRA-1523, R2 and R3).
 */
function Chip({
  chip,
  label,
  tabStops,
}: {
  chip: NodeChip;
  label?: string;
  tabStops: boolean;
}): ReactNode {
  // A chip names a node the page may not be drawing, and the old page's decision cards reached
  // each one by its words (R18, c-nothing-lost). A target nothing answers to now, one of several
  // a short reference names, or a reference the grammar cannot read, is text: there is nowhere
  // to go.
  const ambiguous = chip.ambiguous === true;
  const gone = chip.gone && !ambiguous;
  const target = parseNodeReference(chip.ref);
  const word = ambiguous ? undefined : documentWord(target);
  const agentText = useAgentText();
  const deleted = postDeletion({ deleted_at: chip.deletedAt, deleted_by: chip.deletedBy });
  // The model names a deleted comment or message by its deleted line, and a block of one by
  // nothing, as gone.
  const deletedName = deleted !== null && chip.name !== "";
  const name = deletedName ? deletedLineText(deleted, { label: agentText.author }) : chip.name;
  const cut = chipWords(name);
  const edited = chip.editedAt !== undefined && cut !== "";
  const words = ambiguous ? chip.ref : (word ?? cut);
  // A target's name is its author's words, but for the page's own in it: the deleted line's
  // opening and the edited mark, marked as the page's chrome (INFRA-1523, R3).
  const opening =
    deleted === null
      ? ""
      : deleted.by === null
        ? CONTROL_LABELS.post_deleted
        : CONTROL_LABELS.post_deleted_by;
  const mark = edited ? <EditedMark at={chip.editedAt} marks={pageChrome} /> : null;
  const shown =
    ambiguous || word !== undefined ? (
      words
    ) : deletedName ? (
      <span className="italic" data-post-deleted="" {...pageChrome}>
        {opening}
        {cut === opening ? null : <span {...authorText}>{cut.slice(opening.length)}</span>}
        {mark}
      </span>
    ) : (
      <>
        {cut}
        {mark}
      </>
    );
  if (!gone && words === "") return null;
  const route = chip.gone ? undefined : target;
  // The page's word for a document is the page's, and a target's own words are its author's.
  const marks = word === undefined ? authorText : pageChrome;
  return (
    <span className="min-w-0 [overflow-wrap:anywhere]" data-node-chip="">
      {label === undefined ? null : <span {...pageChrome}>{label} </span>}
      {/* A sentence about the target, so a colon sets it apart from the target's own words. */}
      {chip.stale === true ? <span {...pageChrome}>{CONTROL_LABELS.target_stale}: </span> : null}
      {gone ? (
        <span {...pageChrome}>{CONTROL_LABELS.target_gone}</span>
      ) : route === undefined ? (
        // An address as written may run longer than the line, so it alone may wrap.
        <span className={ambiguous ? undefined : "whitespace-nowrap"} data-chip-name="" {...marks}>
          {shown}
        </span>
      ) : (
        // Out of the tab order, as every other control inside a row is, unless `tabStops` holds:
        // the tree is one tab stop and the row's own keys reach what it holds: `f` steps to a
        // chip, and Enter on it follows the link (R9, WCAG 2.1.1, `useTreeKeymap.ts`).
        <Link
          className={`dispatch-inline-link font-medium whitespace-nowrap underline decoration-page-bullet underline-offset-[3px] ${pageMutedText}`}
          data-chip-name=""
          tabIndex={tabStops ? undefined : -1}
          to={buildReferencePath(route)}
          {...marks}
        >
          {shown}
        </Link>
      )}
    </span>
  );
}

/** The parent's own first line, as the link a reader follows to that node where the model gives
 *  it an address (R18, and R26: a step's row reaches the statement it serves). Out of the tab
 *  order, as every other control inside a row is, unless `tabStops` holds: `f` steps to it and
 *  Enter follows it. A parent its author deleted is drawn as the deleted line, as its own row
 *  draws it (`DeletedLine`), and one its author edited says ` · edited` after its line
 *  (`EditedMark`), so the line holds both marks wherever the parent's own row is not drawn
 *  (INFRA-1523, R2 and R3). The outline's row draws it; so does the Verification view's
 *  parent line (`VerificationFilterBody.tsx`'s `ParentLine`), with `tabStops`, since no tree's
 *  keys reach a link there; and so does the Decisions view's parent line of a parent with words
 *  (`DecisionsRow.tsx`), without `tabStops`, since that view's keys reach a link in its row. */
export function ParentLink({ line, tabStops }: { line: ParentLine; tabStops: boolean }): ReactNode {
  const route = line.reference === undefined ? undefined : parseNodeReference(line.reference);
  const deleted = postDeletion({ deleted_at: line.deletedAt, deleted_by: line.deletedBy });
  const words =
    deleted === null ? (
      <>
        {line.line}
        <EditedMark at={line.editedAt} marks={pageChrome} />
      </>
    ) : (
      <DeletedLine deleted={deleted} />
    );
  return route === undefined ? (
    <span className="min-w-0 break-words" {...authorText}>
      {words}
    </span>
  ) : (
    <Link
      className={`dispatch-inline-link min-w-0 break-words underline decoration-page-bullet underline-offset-[3px] ${pageQuietText}`}
      tabIndex={tabStops ? undefined : -1}
      to={buildReferencePath(route)}
      {...authorText}
    >
      {words}
    </Link>
  );
}

/**
 * What a row draws under its words on its face, whatever the focus (R11): who made it where the
 * margin draws no author - "Author unknown", with its own shape, where the model holds none (R9,
 * ruling 5) - the facts of a check and of a result (`NodeFacts.tsx`), the gaps said in full, the
 * coverage read's own lines a requirement draws in their place while a document of the issue is
 * still being indexed (`TreeNode.coverageNotes`, in the server's words, which name the document),
 * the two counts the coverage read carries for a requirement, and Keep and Change on a decision
 * that waits (`DecisionActions.tsx`). A node with none of these draws nothing here. It carries
 * the page's one mark of what a row draws under its words, `data-node-body`, which the views'
 * own rows (the Verification view's facts, the Decisions view's body) carry too.
 */
function NodeFace({
  author,
  document,
  isClosed,
  node,
  tabStops,
}: {
  author: { readonly kind: AuthorKind; readonly name: string };
  document?: Artifact;
  isClosed: boolean;
  node: TreeNode;
  tabStops: boolean;
}): ReactNode {
  const reviewed = node.type === "decision" && node.waiting;
  // A child issue's row names no author: its facts after its title say how it stands (ruling 5).
  const authored = node.child === undefined && authorLine(author);
  const holds =
    authored ||
    node.facts.length > 0 ||
    node.gaps.length > 0 ||
    (node.coverageNotes ?? []).length > 0 ||
    node.counts !== undefined ||
    reviewed;
  if (!holds) return null;
  return (
    <div className="mt-1 flex min-w-0 flex-col gap-1 text-sm" data-node-body="">
      {authored ? <BodyAuthor author={author} /> : null}
      <NodeFacts facts={node.facts} tabStops={tabStops} />
      {node.gaps.map((gap) => (
        <p className={`text-xs ${pageQuietText}`} key={gap} {...pageChrome}>
          {GAP_LINES[gap]}
        </p>
      ))}
      {(node.coverageNotes ?? []).map((words) => (
        <p className={`text-xs ${pageQuietText}`} data-coverage-note="" key={words} {...authorText}>
          {words}
        </p>
      ))}
      {node.counts === undefined ? null : (
        <p className={`text-xs ${pageQuietText}`} {...pageChrome}>
          <span>{CONTROL_LABELS.serving_checks}: </span>
          <span {...authorText}>{node.counts.checks}</span>
          <span>, {CONTROL_LABELS.serving_plan_tasks}: </span>
          <span {...authorText}>{node.counts.planTasks}</span>
        </p>
      )}
      {reviewed ? (
        <DecisionActions
          document={document}
          isClosed={isClosed}
          nodeReference={node.reference}
          tabStops={tabStops}
        />
      ) : null}
    </div>
  );
}

/** Whether `BodyAuthor` draws a line for `author`: an author the model holds none of, and an
 *  agent the margin names by its role's word alone. */
function authorLine(author: { readonly kind: AuthorKind; readonly name: string }): boolean {
  return (
    author.kind === "unknown" ||
    (author.kind === "agent" && agentRoleWord(author.name) !== author.name)
  );
}

/** Who made a node, under its row's words on the outline (R11) and in the body of a view that
 *  draws one (`DecisionsRow.tsx`) (R6, R9, ruling 5): "Author unknown", with its own shape, where
 *  the model holds no author, and an agent's session's whole title where the row names the agent
 *  by its role's word alone (ruling 6); nothing for any other author, whom the row already names
 *  whole (`authorLine`). */
export function BodyAuthor({
  author,
}: {
  author: { readonly kind: AuthorKind; readonly name: string };
}): ReactNode {
  if (author.kind === "unknown") {
    return (
      <p
        className={`flex min-w-0 items-center gap-1 text-xs leading-5 ${pageQuietText}`}
        data-author-kind={author.kind}
        data-node-author=""
      >
        <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors.unknown} />
        <span {...pageChrome}>{author.name}</span>
      </p>
    );
  }
  if (!authorLine(author)) return null;
  return (
    <p
      className={`flex min-w-0 items-center gap-1 text-xs leading-5 ${pageQuietText}`}
      data-node-author-title=""
    >
      <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors.agent} />
      <span className="min-w-0 break-words" {...authorText}>
        {author.name}
      </span>
    </p>
  );
}

/** Keep open, which holds a node's body open after the focus leaves it, in a view whose rows draw
 *  a body on focus (`DecisionsRow.tsx`), and "Let close" while it does, so a second press lets
 *  it close (R6). Out of the tab order unless `tabStops` holds. */
export function KeepOpen({
  onPin,
  pinned,
  tabStops,
}: {
  onPin: () => void;
  pinned: boolean;
  tabStops: boolean;
}): ReactNode {
  return (
    <p>
      <button
        className={`dispatch-inline-link text-xs leading-6 font-medium ${pageQuietText}`}
        onClick={onPin}
        // A press leaves the focus where it is, as the row's other press controls do: "Let
        // close" on a row that does not hold the focus would take it, close the body under the
        // pointer and drop the focus to the page (decision 141).
        onMouseDown={(event) => event.preventDefault()}
        tabIndex={tabStops ? undefined : -1}
        type="button"
        {...pageChrome}
      >
        {pinned ? CONTROL_LABELS.let_close : CONTROL_LABELS.keep_open}
      </button>
    </p>
  );
}

/**
 * The control under cut words (ruling 1): "more · N words", `hidden` the words the cut holds
 * back, which draws the rest in place, and "less" once `opened` holds. A press on it leaves the
 * focus where it is: focusing the row would draw the words whole under the pointer and move the
 * control before the press ends, so the press would land on nothing. It is out of the tab order
 * unless `tabStops` holds. A view that cuts a node's words draws it - `CutWords`,
 * `DecisionsRow.tsx` - while the outline's rows draw their words whole (R11); `className` adds
 * the place it takes in its own box.
 */
export function MoreWords({
  className,
  hidden,
  onToggle,
  opened,
  tabStops,
}: {
  className?: string;
  hidden: number;
  onToggle: () => void;
  opened: boolean;
  tabStops: boolean;
}): ReactNode {
  return (
    <button
      className={`dispatch-inline-link text-xs leading-[22px] font-medium underline decoration-page-hair underline-offset-[3px] ${pageQuietText}${className === undefined ? "" : ` ${className}`}`}
      data-node-more=""
      onClick={onToggle}
      onMouseDown={(event) => event.preventDefault()}
      tabIndex={tabStops ? undefined : -1}
      type="button"
      {...pageChrome}
    >
      {opened ? (
        CONTROL_LABELS.read_less
      ) : (
        <>
          {CONTROL_LABELS.read_more} · <span {...authorText}>{hidden.toLocaleString()}</span>{" "}
          {CONTROL_LABELS.read_more_words}
        </>
      )}
    </button>
  );
}

/**
 * Where a row cuts its words, read from the words as the browser laid them out, which is the
 * prototype's own reading (`measureClamps`), by the page's one measurer (`clamp-measurer.ts`):
 * for a row that may be cut - an agent's words and an answered question - as it mounts in view,
 * before the page is painted, and while it stands within one screen of the viewport but wholly
 * outside the view, and again when its width or its words change or the page's typeface
 * arrives; never for any other row. A row in view keeps its length until a scroll leaves it
 * wholly outside the view, save where it draws other words: `drawnFrom` is what the words are
 * drawn from, and where a commit draws them from other words, a row measured before is measured
 * again in that commit as one that mounts, before the page is painted (`clampRedrawn`). Other
 * words are words of another value, as TanStack Query's `replaceEqualDeep` compares two answers of
 * a read (`api/queries.ts`): a later read answers the same words in objects of its own - a names
 * read under a new key, which a new comment on the issue starts, shares none with the last - and
 * a row drawn from them draws nothing new, so it keeps its length. A caller that cannot say what
 * its words are drawn from passes nothing. Undefined where the words are drawn whole, or not
 * measured yet; a row the reader scrolled away from keeps the cut it last had, save one measured
 * on the fallback face, which is measured again once the typeface arrives.
 */
export function useClamp(
  words: RefObject<HTMLElement | null>,
  cuttable: boolean,
  drawnFrom?: unknown
): Clamp | undefined {
  const [clamp, setClamp] = useState<Clamp | undefined>(undefined);
  useLayoutEffect(() => {
    const element = words.current;
    if (!cuttable || element === null) {
      setClamp(undefined);
      return;
    }
    return watchClamp(element, (next) => setClamp((held) => (sameClamp(held, next) ? held : next)));
  }, [cuttable, words]);
  // After the watch above: in a commit that watches the words anew, the measurer has not
  // measured them yet, and leaves them to the mount's own pass.
  const drawn = useRef(drawnFrom);
  useLayoutEffect(() => {
    // `replaceEqualDeep` gives back the old value where the new one holds the same values.
    const same = replaceEqualDeep(drawn.current, drawnFrom) === drawn.current;
    drawn.current = drawnFrom;
    const element = words.current;
    if (!same && cuttable && element !== null) clampRedrawn(element);
  }, [cuttable, drawnFrom, words]);
  return cuttable ? clamp : undefined;
}
