import { type ReactNode, useLayoutEffect, useMemo, useRef } from "react";
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
import { firstLineOf, type NodeChip, nodeWords, type TreeNode } from "./issue-tree";
import { NodeArtifact } from "./NodeArtifact";
import { NodeFacts } from "./NodeFacts";
import { NodeGlyph } from "./NodeGlyph";
import { type OneLineBox, oneLineCut } from "./one-line";
import { authorText, pageChrome } from "./page-structure";
import {
  CONTROL_LABELS,
  GAP_LINES,
  GAP_WORDS,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  nodeStateWord,
} from "./page-words";
import { RowText } from "./RowText";

/**
 * One row of the issue's tree, and the body it opens on focus (dispatch://INFRA-1196 R1, R2,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t14`).
 *
 * A row is the twisty, the type with its shape, the author where the model holds one, the node's
 * first line, its chips, the gaps on it, and its state with its shape where the model holds one;
 * a folded row says how many nodes it holds. The first line is cut at a word (`firstLineOf`,
 * and again at `oneLineCut` where the row is one line) and the whole text is in the body, so
 * nothing is lost and no row truncates a word. The words beside it - the type, the author, the
 * state - hold no word boundary to cut at, so a one-line row draws each of them whole.
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
  oneLineBox,
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
  /** Whether the row is drawn as a one-line row, which is how the zoomed tree draws it at
   *  1024 px and below: its first line is one line, cut with an ellipsis to the room the row
   *  leaves it, and its body says the rest. */
  oneLine: boolean;
  /** The tree's one reading of its own width, which a one-line row cuts its first line to
   *  (`one-line.ts`): no row reads its own layout. Undefined where the row wraps, and until
   *  the tree has been laid out. */
  oneLineBox: OneLineBox | undefined;
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
  /** What a folded row says it holds: drawn beside the node's own text where the row wraps,
   *  and in the body where it is one line. The words agree with the count: "1 row", "3 rows". */
  const foldedWords = folded === 1 ? CONTROL_LABELS.folded_row : CONTROL_LABELS.folded_rows;
  const counted =
    folded === undefined || expanded !== false ? undefined : `${folded} ${foldedWords}`;
  // The node's own first line. Where the row wraps, it is the whole of `firstLineOf`'s line;
  // where the row is one line, it is cut again, at a word, to the pixels the row's own box
  // leaves it beside its twisty, its type, its author and its state (R1). The count is no
  // longer one of those words there - it is the body's - so it takes nothing from the line.
  // Both are cut from the node's words as its body draws them (`nodeWords`), so a line holds
  // none of the symbols the body draws as formatting.
  //
  // The row sums those parts itself rather than reading where the browser put them, since no
  // row may read its own layout: the tree reads its own width once for every row
  // (`useOneLineBox`), each part sized by a class is that class's size in rem (`ROW_PARTS`),
  // and each word is measured in the tree's face, so a row whose author's id is longer leaves
  // its line less, the author's shape and its gap included. Where they leave it less than its
  // first word, the line goes on a line of its own under them (`oneLineCut`). Everything
  // beside the line is drawn whole (`shrink-0`): a type's word and a state's word are one word
  // each and a session's id is one token, so a row that shrank them would cut inside a word,
  // which is the one thing R1 bans. The line keeps `overflow-hidden` with `min-w-0`, so a box
  // the sum was wrong about never draws past its row over whatever sits beside it
  // (c-checks-readable); the e2e rows read that box against the text it holds, which is how a
  // wrong sum is found. Above 1024 px the row wraps instead and nothing is cut.
  const words = nodeWords(node);
  const typeWord = NODE_TYPE_NAMES[node.type];
  const authorId = node.author?.id;
  const leaf = expanded === undefined;
  const box = oneLine ? oneLineBox : undefined;
  const first = useMemo(() => {
    if (box === undefined) return { ...firstLineOf(words), under: false, wraps: false };
    const { rem, widthOf } = box;
    const gap = ROW_PARTS.gap * rem;
    const shaped = (word: string, size: number) =>
      (ROW_PARTS.glyph + ROW_PARTS.inner) * rem + widthOf(word, size);
    const twisty = leaf ? ROW_PARTS.glyph * rem : Math.max(ROW_PARTS.touch, ROW_PARTS.glyph * rem);
    const stateWidth =
      state === undefined
        ? 0
        : stateGlyph === undefined
          ? widthOf(state, ROW_PARTS.small)
          : shaped(state, ROW_PARTS.small);
    return oneLineCut({
      after: state === undefined ? 0 : gap + stateWidth,
      before:
        twisty +
        gap +
        shaped(typeWord, ROW_PARTS.text) +
        gap +
        (authorId === undefined ? 0 : shaped(authorId, ROW_PARTS.text) + gap),
      room: box.width - indent - ROW_PARTS.border - ROW_PARTS.paddingRight * rem,
      text: words,
      widthOf: (line) => widthOf(line, ROW_PARTS.text),
    });
  }, [authorId, box, indent, leaf, state, stateGlyph, typeWord, words]);
  const whole = oneLine ? "shrink-0" : "";

  // The focused row is brought inside the viewport and clear of the sticky identity block
  // after every move, by the one rule the whole page places a target with (R9,
  // `identity-block.ts`), and again whenever the row grows under it: a document's node reads
  // its own text when the body opens, because the page reads no document's text before a
  // reader opens it (R20), so the row the focus lands on is short for as long as that read
  // takes and the text would otherwise push its foot off the screen.
  //
  // Except while a press inside the row is held. A press on an unfocused row focuses it on
  // mousedown, before the press ends, and a scroll then moves what the pointer went down on:
  // the press ends on something else and the control it was on never hears it. Measured at
  // 1440 px on the Decisions preset, a row whose top sat 24 px under the identity block
  // scrolled 32 px between mousedown and mouseup, and the reader's Keep was never sent. The
  // reader pressed what they could see, so there is nothing to bring on screen.
  const pressed = useRef(false);
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
      if (!pressed.current) bringOnScreen(element);
    };
    place();
    if (typeof ResizeObserver === "undefined") return;
    const grown = new ResizeObserver(place);
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
      onPointerDown={press}
      ref={row}
      role="treeitem"
      style={{ paddingLeft: indent }}
      tabIndex={tabStop ? 0 : -1}
    >
      <div
        className={`flex min-w-0 items-baseline gap-x-2 text-sm ${
          oneLine
            ? first.under
              ? "flex-wrap overflow-hidden"
              : "flex-nowrap overflow-hidden whitespace-nowrap"
            : "flex-wrap gap-y-1"
        }`}
      >
        <Twisty expanded={expanded} onToggle={onToggle} />
        <span className={`flex items-center gap-1 ${whole}`} {...pageChrome}>
          <NodeGlyph token={GLYPH_TOKENS.types[node.type]} />
          <span className={textMutedOnCanvas}>{typeWord}</span>
        </span>
        {/* Who made it, by a word and a shape, in the form the type beside it and the state
            after it are drawn in: one shape for an author, whoever they are, because the node
            carries an author's id and nothing else (R7). The id itself is the author's own
            text and the shape is the page's, so the two carry their own marks and the shape
            adds no word to what the reader sees. On a row whose line went under its words, an
            id longer than the whole row wraps inside itself rather than be hidden past it. */}
        {node.author === undefined ? null : (
          <span
            className={`flex items-center gap-1 ${whole} ${first.under ? "max-w-full" : ""}`}
            data-node-author=""
          >
            <NodeGlyph token={GLYPH_TOKENS.author} />
            <span
              className={`${textMutedOnCanvas} ${first.under ? "[overflow-wrap:anywhere]" : ""}`}
              {...authorText}
            >
              {node.author.id}
            </span>
          </span>
        )}
        {/* Where the row's other words leave the first line less than its first word, the line
            starts a line of its own under them (`oneLineCut`): this takes the whole of the line
            it is on, so what follows it - the first line and the state - starts the next. */}
        {first.under ? <span aria-hidden="true" className="basis-full" /> : null}
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
            oneLine
              ? first.wraps
                ? "[overflow-wrap:anywhere]"
                : "overflow-hidden whitespace-nowrap"
              : "break-words"
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
            drawn once however the nodes came to be out of sight.

            On a one-line row the count is the body's, as the chips and the gaps are, and for
            the same reason: its words are a sentence of the page's own and they are the one
            part of the row that is neither the node's name nor its text. Measured in the
            Checks preset at 390 px, where the row has 328 px: the twisty's touch target 44,
            the type 61, this count 132 and the state 62, with the four gaps 32, leave the
            node's own first line nothing at all - it was drawn 0 px wide - and the state 21 px
            past the card (c-checks-readable). The count is what gives way, because R1's row
            carries its type, its author, its own first line and its state. */}
        {oneLine || counted === undefined ? null : (
          <span className={`text-xs ${textMutedOnCanvas} ${whole}`} {...pageChrome}>
            <span {...authorText}>{folded}</span> {foldedWords}
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
          {...(oneLine && counted !== undefined ? { counted } : {})}
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
 * The parts of a one-line row its node's first line shares the row's width with, each in the
 * unit of the class that draws it in `NodeRow`, so a row sums them against the tree's one
 * reading of its width (`one-line.ts`) rather than reading its own layout. A class changed there
 * is a number changed here: `e2e/issue-layout.e2e.ts` reads each one-line row's box against the
 * text the sum gave it, which is where a number left behind is found.
 */
const ROW_PARTS = {
  /** The row's `border-l-2`, in pixels. */
  border: 2,
  /** The row's `gap-x-2`, between each two of its parts, in rem. */
  gap: 0.5,
  /** A shape's `h-4 w-4` (`NodeGlyph`), which is also the place a row that holds no children
   *  keeps for its twisty (`w-4`), in rem. */
  glyph: 1,
  /** The `gap-1` between a shape and its word, in rem. */
  inner: 0.25,
  /** The row's `pr-2`, in rem. */
  paddingRight: 0.5,
  /** The state's `text-xs`, in rem. */
  small: 0.75,
  /** The row's `text-sm`, which the type, the author and the first line are drawn at, in rem. */
  text: 0.875,
  /** The touch target every control is held to below 1280 px (`styles.css`), which is every
   *  width a row is one line at, so a twisty is that wide whatever its shape is, in pixels. */
  touch: 44,
} as const;

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
      {/* A sentence about the target, so a colon sets it apart from the target's own words. */}
      {chip.stale === true ? <span {...pageChrome}>{CONTROL_LABELS.target_stale}: </span> : null}
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
 * holds for it, from the server's reading of it where its read sent one (`RowText.tsx`).
 */
function NodeBody({
  artifact,
  counted,
  document,
  edges,
  editing,
  isClosed,
  node,
  onPin,
  pinned,
}: {
  artifact?: Artifact;
  /** How many nodes a folded row holds, in the page's words, where the row itself has no width
   *  for them: a one-line row's count belongs here, as its chips and its gaps do (R17).
   *  Undefined for a row that draws its own count, and for a row that is not folded. */
  counted?: string;
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
          <RowText markdown={node.text} source={node.source} tree={node.tree} />
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
      {counted === undefined ? null : (
        <p className={`text-xs ${textMutedOnCanvas}`} {...pageChrome}>
          {counted}
        </p>
      )}
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
