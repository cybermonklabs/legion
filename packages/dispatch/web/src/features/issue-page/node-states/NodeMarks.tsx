import { createContext, Fragment, type ReactNode, useContext } from "react";

import { pageQuietText } from "../../../theme/classes";
import { NodeGlyph } from "../NodeGlyph";
import { pageChrome } from "../page-structure";
import { GLYPH_TOKENS, ROW_MARK_WORDS, type RowMark } from "../page-words";
import type { NodeStates } from "./node-states";

/**
 * The marks a node carries, each as its word and its shape: its life first - proposed or
 * overtaken - then whether it is reserved for its author, who is the reader. Each is
 * `[data-node-mark]`, named by its mark's own token, in the grey of a row's state line, and it
 * keeps its word and its shape with forced colours: the shape is drawn in the text's own colour.
 *
 * A row draws them in its margin's state line, at that line's size. `compact` is the form a
 * place that names a node inside another's row draws after the node's name - a chip, a parent
 * line - in the size of the text around it. A node with no mark draws nothing.
 */
export function NodeMarks({
  compact = false,
  marks,
}: {
  /** Whether the marks stand after a name inside another's row, in that text's own size. */
  compact?: boolean;
  /** The node, chip or parent line whose marks are drawn: anything that may carry them. */
  marks: NodeStates;
}): ReactNode {
  const held: readonly RowMark[] = [
    ...(marks.life === undefined ? [] : [marks.life]),
    ...(marks.onlyYou === true ? (["only_you"] as const) : []),
  ];
  if (held.length === 0) return null;
  return held.map((mark) => (
    <Fragment key={mark}>
      {/* After a name in a line of text a space parts the two; inside a flex line, as a parent
          line is, no space is drawn and the line's own gap parts them. */}
      {compact ? " " : null}
      <span
        className={`inline-flex items-center gap-1 whitespace-nowrap ${compact ? "" : "text-xs"} ${pageQuietText}`}
        data-node-mark={mark}
      >
        <NodeGlyph
          className={`${compact ? "size-[1em]" : "size-3"} shrink-0`}
          token={GLYPH_TOKENS.marks[mark]}
        />
        <span {...pageChrome}>{ROW_MARK_WORDS[mark]}</span>
      </span>
    </Fragment>
  ));
}

/** The marks of each node of the tree the outline draws, by its reference without the version
 *  it pins (`statesByReference`). Empty outside the outline's rows, so a place drawn there marks
 *  nothing it was not given. */
const NodeStatesByReference = createContext<ReadonlyMap<string, NodeStates>>(new Map());

/**
 * Hands the marks of the outline's nodes, by reference, to each place under it that names a node
 * by its address alone, with no node or chip of the tree to read them from: an entry of a lens's
 * chain. `IssueTree` wraps its rows in it, with `statesByReference` of the tree it draws, so the
 * entry draws what the node's own row draws.
 */
export function NodeStatesProvider({
  children,
  value,
}: {
  children?: ReactNode;
  /** `statesByReference` of the tree the rows are drawn from. */
  value: ReadonlyMap<string, NodeStates>;
}): ReactNode {
  return <NodeStatesByReference.Provider value={value}>{children}</NodeStatesByReference.Provider>;
}

/** The marks of the outline's nodes by reference, each read through `marksAt`; empty outside a
 *  `NodeStatesProvider`. */
export function useNodeStatesByReference(): ReadonlyMap<string, NodeStates> {
  return useContext(NodeStatesByReference);
}
