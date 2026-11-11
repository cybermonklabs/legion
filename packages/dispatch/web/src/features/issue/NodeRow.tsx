import { type ReactNode, useLayoutEffect, useRef } from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../api/types";
import {
  borderDefault,
  borderTransparent,
  linkHoverText,
  linkText,
  quoteAccentBorder,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildReferencePath, parseNodeReference, shortForm } from "../refs/routes";
import { DecisionActions } from "./DecisionActions";
import { STAGE_IN_ROW, useDocumentStage } from "./document-stage";
import { bringOnScreen } from "./identity-block";
import { firstLineOf, type NodeChip, type TreeNode } from "./issue-tree";
import { NodeArtifact } from "./NodeArtifact";
import { NodeFacts } from "./NodeFacts";
import { NodeGlyph } from "./NodeGlyph";
import { authorText, pageChrome } from "./page-structure";
import {
  CONTROL_LABELS,
  GAP_LINES,
  GAP_WORDS,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  nodeStateWord,
} from "./page-words";

/**
 * One row of the issue's tree, and the body it opens on focus (dispatch://INFRA-1196 R1, R2,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t14`).
 *
 * A row is the twisty, the type with its shape, the author where the model holds one, the node's
 * first line, its chips, the gaps on it, and its state with its shape where the model holds one;
 * a folded row says how many nodes it holds. The first line is cut at a word (`firstLineOf`,
 * and again at `oneLineFirstLine` where the row is one line) and the whole text is in the body,
 * so nothing is lost and no row truncates a word. The words beside it - the type, the author,
 * the state - hold no word boundary to cut at, so a one-line row draws each of them whole.
 *
 * The body is drawn for the focused row and for a row pinned open, and for no other row: it is
 * the focus that opens it, never the pointer, and no element of the row carries a `title`, since
 * a reader on a phone or on the keyboard never sees one (R2).
 *
 * The row is a `treeitem` of the tree's one tab stop: the focused row is the only row that takes
 * the focus, and nothing inside a row takes any, so Tab from outside enters the tree once and
 * Tab from the row leaves it again (R9). What a reader does inside a row they do with a key:
 * the twisty and the pin have the arrow keys and `o`, and Keep, Change and a chip's link have
 * keys of their own, so every control of a row is worked from the keyboard without a second tab
 * stop (WCAG 2.1.1, `useTreeKeymap.ts`, which finds Keep and Change by their `data-row-action`
 * marks). The key bindings are `dispatch://INFRA-1196/artifact/plan-md#b-t15`'s.
 */
export function NodeRow({
  artifact,
  body,
  context,
  document,
  editing,
  expanded,
  focused,
  folded,
  indent,
  isClosed,
  level,
  node,
  oneLine,
  oneLineMeasure,
  onFocus,
  onPin,
  onToggle,
  pinned,
  rowKey,
  tabStop,
}: {
  /** The artifact this row's node is, for a document or an image: its body opens that
   *  artifact itself (R16). Undefined for every other kind of node. */
  artifact?: Artifact;
  /** Whether this row draws its body: it has the focus, or the reader pinned it open. */
  body: boolean;
  /** Whether the preset draws this row only as the ancestor of a match: the row is faint and
   *  marked, and the arrow keys reach it as they reach any other row (R3). */
  context: boolean;
  /** The document this row's node is a block of, which its body writes against: a decision's
   *  Keep and Change are writes on the decisions document (R7). Undefined for a node that is
   *  no block of a document of this issue. */
  document?: Artifact;
  /** The document this row's own text lives in, while the reader is editing it here: its
   *  editor is drawn in this row's body and the caret is in this node's block (R9). */
  editing?: string;
  /** `true` when its children are drawn, `false` when it holds children and they are not, and
   *  undefined for a node that holds none. */
  expanded?: boolean;
  focused: boolean;
  /** How many nodes it holds while it is folded. */
  folded?: number;
  /** How far in the row is drawn, in pixels. */
  indent: number;
  /** Whether the issue is closed, which the writes a body offers are refused on. */
  isClosed: boolean;
  level: number;
  node: TreeNode;
  /** Whether the row is drawn on one line, which is how the zoomed tree draws it at 1024 px and
   *  below: the row stays one line, cut with an ellipsis, and its body says the rest. */
  oneLine: boolean;
  /** How many characters of its own words the row draws while it is one line
   *  (`PHONE_ONE_LINE_MEASURE`, `WIDE_ONE_LINE_MEASURE`): the tree reads the width band once
   *  for every row (`IssueTree.tsx`), since no row may read its own layout. */
  oneLineMeasure: number;
  onFocus: () => void;
  onPin: () => void;
  onToggle: () => void;
  pinned: boolean;
  /** What tells this row from every other, including another placement of the same node
   *  (`rowKeyOf`): the row the caret was taken from is found by it (`useNodeEditing.ts`). */
  rowKey: string;
  /** Whether this row is the tree's one tab stop: the focused row, or the tree's first row
   *  before the reader has touched it. */
  tabStop: boolean;
}): ReactNode {
  const row = useRef<HTMLDivElement | null>(null);
  const state = node.state === undefined ? undefined : nodeStateWord(node.state);
  const stateGlyphs: Readonly<Record<string, Readonly<Record<string, string>>>> =
    GLYPH_TOKENS.states;
  const stateGlyph = node.state === undefined ? undefined : stateGlyphs[node.type]?.[node.state];
  /** What a folded row says it holds, which is drawn beside the node's own text. */
  const counted =
    folded === undefined || expanded !== false
      ? undefined
      : `${folded} ${CONTROL_LABELS.folded_nodes}`;
  // The node's own first line. Where the row wraps, it is the whole of `firstLineOf`'s line;
  // where the row is one line, it is cut again to what the row's other words leave of the
  // measure its width band carries, at a word (R1).
  const first = oneLine
    ? oneLineFirstLine(node.text, oneLineMeasure, [
        NODE_TYPE_NAMES[node.type],
        node.author?.id,
        state,
        counted,
      ])
    : firstLineOf(node.text);
  // What a one-line row gives up when its parts do not fit, and what it never gives up (R1,
  // R17). A phone's row is 258 px and a type's word, a session's id and a state's word are long
  // enough together to fill it. None of those three holds a word boundary to cut at - a type's
  // word and a state's word are one word each, and a session's id is one token - so none of
  // them is cut at all: each is drawn whole (`shrink-0`), and what gives width up is the node's
  // own text, cut above by the measure rather than here by a CSS ellipsis that would stop
  // inside a word. Only the element the text itself sits in is cut, never one that holds
  // another: a box that reached past its row would be drawn over whatever sits beside it
  // (c-checks-readable), so where a width or a font leaves that measure generous the line's own
  // box hides what reaches past it, with no ellipsis of its own to claim a cut the words do not
  // carry. The whole of every cut word is in the body the row opens on focus (R2). Above
  // 1024 px the row wraps instead and nothing is cut.
  const whole = oneLine ? "shrink-0" : "";

  // The focused row is brought inside the viewport and clear of the sticky identity block
  // after every move, by the one rule the whole page places a target with (R9,
  // `identity-block.ts`), and again whenever the row grows under it: a document's node reads
  // its own text when the body opens, because the page reads no document's text before a
  // reader opens it (R20), so the row the focus lands on is short for as long as that read
  // takes and the text would otherwise push its foot off the screen.
  useLayoutEffect(() => {
    const element = row.current;
    if (!focused || element === null) return;
    bringOnScreen(element);
    if (typeof ResizeObserver === "undefined") return;
    const grown = new ResizeObserver(() => bringOnScreen(element));
    grown.observe(element);
    return () => grown.disconnect();
  }, [focused]);

  return (
    <div
      aria-expanded={expanded}
      aria-level={level}
      className={`flex min-w-0 flex-col border-l-2 py-1 pr-2 ${
        focused ? quoteAccentBorder : borderTransparent
      } ${surfaceMutedHoverBg}`}
      data-folded-count={folded}
      data-node-context={context ? "" : undefined}
      data-node-id={node.id}
      data-node-row=""
      data-node-state={node.state}
      data-node-type={node.type}
      data-row-key={rowKey}
      onFocus={onFocus}
      ref={row}
      role="treeitem"
      style={{ paddingLeft: indent }}
      tabIndex={tabStop ? 0 : -1}
    >
      <div
        className={`flex min-w-0 items-baseline gap-x-2 text-sm ${
          oneLine ? "flex-nowrap overflow-hidden whitespace-nowrap" : "flex-wrap gap-y-1"
        }`}
      >
        <Twisty expanded={expanded} onToggle={onToggle} />
        <span className={`flex items-center gap-1 ${whole}`} {...pageChrome}>
          <NodeGlyph token={GLYPH_TOKENS.types[node.type]} />
          <span className={textMutedOnCanvas}>{NODE_TYPE_NAMES[node.type]}</span>
        </span>
        {node.author === undefined ? null : (
          <span className={`${textMutedOnCanvas} ${whole}`} data-node-author="" {...authorText}>
            {node.author.id}
          </span>
        )}
        {/* A context row - one drawn only because a match sits under it - is faint by drawing
            its own words at the muted level the rest of the row already uses, rather than by
            dimming the whole row. `opacity-60` was the first way, and it fades every colour of
            the row towards the canvas: axe read the muted words at 2.25:1 in light and 3.32:1
            in dark against AA's 4.5:1, and the node's own first line at 3.29:1, because an
            opacity is applied to a colour the palette registered as AA and nothing re-registers
            the blend (`theme/classes.ts`'s `CONTRAST_CHECKS`). The muted pair is registered, so
            a faint row is as readable as any other. */}
        <span
          className={`min-w-0 ${context ? textMutedOnCanvas : textSecondaryOnCanvas} ${
            oneLine ? "overflow-hidden" : "break-words"
          }`}
          data-node-first-line=""
          {...authorText}
        >
          {first.cut ? `${first.line}…` : first.line}
        </span>
        {/* The chips and the gaps are the body's on a one-line row: each is a reference or a
            sentence, neither of which fits beside the node's own words on a phone's 258 px
            line, and a row cut to hold them would say less than it does now (R17). */}
        {oneLine ? null : <NodeEdges node={node} />}
        {/* A row whose own twisty is closed says how many nodes it holds. A branch below the
            drawn levels says it on the one line it folds to, under this row, so the count is
            drawn once however the nodes came to be out of sight. */}
        {counted === undefined ? null : (
          <span className={`text-xs ${textMutedOnCanvas} ${whole}`} {...pageChrome}>
            <span {...authorText}>{folded}</span> {CONTROL_LABELS.folded_nodes}
          </span>
        )}
        {state === undefined || node.state === undefined ? null : (
          <span className={`ml-auto flex items-center gap-1 text-xs ${whole}`} {...pageChrome}>
            {stateGlyph === undefined ? null : <NodeGlyph token={stateGlyph} />}
            <span className={textMutedOnCanvas} data-node-state={node.state}>
              {state}
            </span>
          </span>
        )}
      </div>
      {body ? (
        <NodeBody
          artifact={artifact}
          document={document}
          edges={oneLine}
          editing={editing}
          isClosed={isClosed}
          node={node}
          onPin={onPin}
          pinned={pinned}
        />
      ) : null}
    </div>
  );
}

/**
 * How many characters of its own words a one-line row draws at 767 px and below
 * (`PHONE_VIEWPORT_QUERY`), which is the phone's band of the one-line widths (R1, R17).
 *
 * The row a phone draws is 258 px wide, measured on the zoomed tree at the 390 px the `iphone`
 * project runs, so the page keeps 132 px of that viewport for itself. Off the row go the
 * twisty and the two shapes at 16 px each (`h-4 w-4`), the four gaps between the row's parts at
 * 8 px each (`gap-x-2`) and the gap inside the type and inside the state at 4 px each
 * (`gap-1`): 88 px, which leaves 170 px of words. `text-sm` is 14 px and the page's sans face
 * draws mixed-case prose at about half its size per character, so 170 / 7 is 24 characters,
 * and the row's words share them.
 */
export const PHONE_ONE_LINE_MEASURE = 24;

/**
 * The same, for the rest of the one-line widths: 768 px up to the 1024 px the tree is zoomed at
 * (`ZOOMED_TREE_QUERY`), where the row is still one line and four times as wide as a phone's.
 *
 * The same arithmetic at 1024 px: the page keeps the 132 px the measurement above gives it, so
 * the row is 1024 - 132 = 892 px; less the same 88 px of twisty, shapes and gaps, 804 px of
 * words; at 7 px a character, 804 / 7 is 114. A line of 114 characters is longer than
 * `FIRST_LINE_LIMIT` allows one to be at all, so a row of this band draws the whole of
 * `firstLineOf`'s line except where its own type, author and state take enough of the measure
 * to cut into it.
 *
 * Each band is derived at one width and drawn at every width in it, because a row may not read
 * its own layout to decide what it says - that is a layout read per row and another on every
 * resize, for a page that draws 56 of them - and the tree reads the band once for all of them
 * (`IssueTree.tsx`). So the low end of a band hides the tail of a long line in the line's own
 * box: 320 px leaves the phone's row 188 px, and 768 px leaves this band's row 636 px, which is
 * (636 - 88) / 7, about 78 characters. The row's whole text is in the body it opens on focus
 * (R2) at every width, and the cut a measure makes never lands inside a word.
 */
export const WIDE_ONE_LINE_MEASURE = 114;

/**
 * The node's own text as a one-line row draws it: `firstLineOf`'s line, cut again to what the
 * row's other words - its type, its author, its state, the count a folded row draws - leave of
 * `measure`, at a space and never inside a word (R1). `cut` says the text carries on past the
 * line, which is what the row draws its ellipsis for.
 *
 * A first word longer than what is left of the measure is drawn whole rather than cut: a piece
 * of a word with an ellipsis after it is the one thing R1 bans, and a word no box at that width
 * could hold is answered by the body the row opens (R2), never by cutting it. The one cut no
 * word boundary can answer is already made before this: `firstLineOf` holds a token longer than
 * its own 100 characters to that limit, because such a token has no boundary in it at all
 * (`issue-tree.ts`).
 */
export function oneLineFirstLine(
  text: string,
  measure: number,
  beside: readonly (string | undefined)[]
): { readonly cut: boolean; readonly line: string } {
  const first = firstLineOf(text);
  const left = measure - beside.reduce((sum, word) => sum + (word?.length ?? 0), 0);
  if (first.line.length <= left) return first;
  const space = first.line.lastIndexOf(" ", Math.max(left, 0));
  if (space > 0) return { cut: true, line: first.line.slice(0, space) };
  const wordEnd = first.line.indexOf(" ");
  return wordEnd === -1 ? first : { cut: true, line: first.line.slice(0, wordEnd) };
}

/** The control that opens and closes a branch. It draws a shape with no name of its own - the
 *  tree's named shapes are its types' and its states' - and carries the page's word for what it
 *  does as its accessible name, never as a tooltip. */
function Twisty({ expanded, onToggle }: { expanded?: boolean; onToggle: () => void }): ReactNode {
  if (expanded === undefined) {
    return <span className="inline-block w-4 shrink-0" />;
  }
  return (
    <button
      aria-label={expanded ? CONTROL_LABELS.fold : CONTROL_LABELS.unfold}
      className={`shrink-0 ${textMutedOnCanvas}`}
      onClick={onToggle}
      tabIndex={-1}
      type="button"
    >
      <svg
        aria-hidden="true"
        className={`h-4 w-4 transition-transform ${expanded ? "rotate-90" : ""}`}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        viewBox="0 0 16 16"
      >
        <path d="M6 3 l5 5 l-5 5" />
      </svg>
    </button>
  );
}

/** Every edge of the node that is not the spine, and every gap on it: the chips naming what it
 *  also sits under and what it points at, and the one word each gap is known by. Drawn on the
 *  row where the row wraps, and in the body where the row is one line (R17). */
function NodeEdges({ node }: { node: TreeNode }): ReactNode {
  return (
    <>
      {node.alsoUnder.map((chip) => (
        <Chip chip={chip} key={`also:${chip.ref}`} label={CONTROL_LABELS.also_under} />
      ))}
      {node.chips.map((chip) => (
        <Chip chip={chip} key={chip.ref} />
      ))}
      {node.gaps.map((gap) => (
        <span
          className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
          data-gap=""
          data-gap-kind={gap}
          key={gap}
          {...pageChrome}
        >
          {GAP_WORDS[gap]}
        </span>
      ))}
    </>
  );
}

/**
 * One edge of the node that is not the spine, naming its target: the page's word for the edge
 * where it has one, the target's own name from the batched reference read, and the target's
 * address, which is how a reader cites it and the only thing that tells two blocks of one
 * document apart. A target nothing answers to now says so instead of naming nothing.
 *
 * The address is drawn in its short form - `CORE-1 spec block`, `refs/routes.ts`'s `shortForm`,
 * the form the old Plan tab drew for the same pointer - from the moment the reference read has
 * answered for it. The raw `dispatch://CORE-1/spec#b-<block id>` is a name nobody chose: a
 * block id is a uuid, which R21 and c-plain-words refuse where a name belongs, and a chip whose
 * target the graph answered with no text of its own drew nothing else. A chip still waiting for
 * that read keeps the raw address, which is the deliberate transient `useIssueTree.ts` records
 * ("the names come last and all at once"): until the read answers there is nothing else to say,
 * and the reference is still the reference.
 */
function Chip({ chip, label }: { chip: NodeChip; label?: string }): ReactNode {
  const target = parseNodeReference(chip.ref);
  const route = chip.gone ? undefined : target;
  // The target's own words, as the link a reader follows to it: a chip names a node the page
  // may not be drawing, and the old page's decision cards reached each one by its words (R18,
  // c-nothing-lost). A target nothing answers to now, or a reference the grammar cannot read,
  // is text: there is nowhere to go.
  const named =
    chip.name === "" ? null : route === undefined ? (
      <span {...authorText}>{`${chip.name} `}</span>
    ) : (
      <>
        {/* Out of the tab order, as every other control inside a row is: the tree is one tab
            stop and the row's own keys reach what it holds: `f` steps to a chip, and Enter on
            it follows the link (R9, WCAG 2.1.1, `useTreeKeymap.ts`). */}
        <Link
          className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
          tabIndex={-1}
          to={buildReferencePath(route)}
          {...authorText}
        >
          {chip.name}
        </Link>{" "}
      </>
    );
  // A blocking child issue is named by its key and addressed by the same key, and a chip saying
  // "CORE-2 CORE-2" says one of them twice.
  const address = chip.answered && target !== undefined ? shortForm(target) : chip.ref;
  return (
    <span
      className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
      data-node-chip=""
    >
      {label === undefined ? null : <span {...pageChrome}>{label} </span>}
      {chip.stale === true ? <span {...pageChrome}>{CONTROL_LABELS.target_stale} </span> : null}
      {chip.gone ? <span {...pageChrome}>{CONTROL_LABELS.target_gone} </span> : named}
      {address === chip.name ? null : (
        <span className="break-all" {...authorText}>
          {address}
        </span>
      )}
    </span>
  );
}

/** How wide a node's own text is drawn, in pixels: the measure R1 names for the focused node's
 *  body, which is a line length a person reads rather than the column's whole width. */
const BODY_MEASURE = 660;

/**
 * The node's whole text, with what the model holds about it that a row has no line for: the
 * facts of a check and of a result (`NodeFacts.tsx`), the gaps said in full, the two counts
 * the coverage read carries for a requirement, what a reader does with a decision
 * (`DecisionActions.tsx`), and the control that keeps the body open once the focus leaves it.
 *
 * What stands for "the node's whole text" depends on what the node is. A block of a document
 * the reader is editing draws that document's own editor, moved here from wherever the page
 * was drawing it (`document-stage.tsx`), so the caret goes into the node's text where it
 * lives, with the collaborative cursors and the comment chord it has there. An artifact's node
 * draws the artifact itself (`NodeArtifact.tsx`). Every other node draws the text the model
 * holds for it.
 */
function NodeBody({
  artifact,
  document,
  edges,
  editing,
  isClosed,
  node,
  onPin,
  pinned,
}: {
  artifact?: Artifact;
  document?: Artifact;
  /** Whether the chips and gaps of the node belong here: they do wherever the row is one line
   *  and has no width for them (R17). */
  edges: boolean;
  editing?: string;
  isClosed: boolean;
  node: TreeNode;
  onPin: () => void;
  pinned: boolean;
}): ReactNode {
  const stage = useDocumentStage(editing, STAGE_IN_ROW);
  return (
    <div
      className="mt-1 ml-6 flex min-w-0 flex-col gap-1 text-sm"
      data-node-body=""
      data-pinned={pinned ? "" : undefined}
      style={{ maxWidth: BODY_MEASURE }}
    >
      {editing === undefined ? (
        artifact === undefined ? (
          <p className={`whitespace-pre-wrap break-words ${textSecondaryOnCanvas}`} {...authorText}>
            {node.text}
          </p>
        ) : (
          <NodeArtifact artifact={artifact} />
        )
      ) : (
        <div className="flex min-w-0 flex-col" ref={stage} {...authorText} />
      )}
      <NodeFacts facts={node.facts} />
      {edges && (node.alsoUnder.length > 0 || node.chips.length > 0 || node.gaps.length > 0) ? (
        <p className="flex flex-wrap items-baseline gap-1">
          <NodeEdges node={node} />
        </p>
      ) : null}
      {node.gaps.map((gap) => (
        <p className={`text-xs ${textMutedOnCanvas}`} key={gap} {...pageChrome}>
          {GAP_LINES[gap]}
        </p>
      ))}
      {node.counts === undefined ? null : (
        <p className={`text-xs ${textMutedOnCanvas}`} {...pageChrome}>
          <span>{CONTROL_LABELS.serving_checks}: </span>
          <span {...authorText}>{node.counts.checks}</span>
          <span>, {CONTROL_LABELS.serving_plan_tasks}: </span>
          <span {...authorText}>{node.counts.planTasks}</span>
        </p>
      )}
      {node.type === "decision" ? (
        <DecisionActions document={document} isClosed={isClosed} nodeReference={node.reference} />
      ) : null}
      <p>
        <button
          className={`text-xs underline ${textMutedOnCanvas}`}
          onClick={onPin}
          tabIndex={-1}
          type="button"
          {...pageChrome}
        >
          {pinned ? CONTROL_LABELS.let_close : CONTROL_LABELS.keep_open}
        </button>
      </p>
    </div>
  );
}
