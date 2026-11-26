import { type MouseEvent, type ReactNode, useMemo } from "react";
import { Link, useLocation } from "react-router-dom";

import { pageMutedText } from "../../../theme/classes";
import { isPlainClick } from "../../refs/picture-lightbox";
import { usePlainHeadline } from "../../refs/plain-text";
import { parseIssuePath } from "../../refs/routes";
import { NodeMarks, useNodeStatesByReference } from "../node-states/NodeMarks";
import { marksAt } from "../node-states/node-states";
import { authorText, pageChrome } from "../page-structure";
import { statesOfRead } from "../tree-read/tree-nodes";
import { type LensChip, wordsReading } from "./lens-drafts";
import { lensPlaceOf, rowPathIn } from "./lens-reads";

/**
 * One chip of a lens line: a lens, a check or a constraint, by its words; or a node a fact line
 * names (`LensLines.tsx`'s `FactWords`), by the words the line names it by. Where the tree read
 * gives the target's id, the words are the link to its row on this issue's page (`?node=`), drawn
 * as `NodeRow.tsx`'s `Chip` draws its link, in the view that draws the row (R7): in the open
 * outline where it draws it - Graph, which draws every row, and Conversation, but for a target
 * only Graph draws (`LensChip.graphOnly`), as an issue constraint is in Conversation, or one no
 * page read so far holds - and in Graph from anywhere else: from Conversation for such a target,
 * and from the Spec view. The issue's key is the address's own (`lensPlaceOf`), so the link works
 * in each view alike. A target with no id, or an address that names no issue, draws the words
 * alone. A chip's words are whole, and wrap inside the line they stand in, as `NodeRow.tsx`'s
 * `ParentLink` does.
 *
 * A press on a link to the open outline also focuses the target's row where the tree the chip
 * stands in draws it already (`focusDrawnRow`), at every press: the address puts the focus on its
 * node once, and no more after the reader's first act, so a second press on the same chip, once
 * the reader has moved on, would otherwise move nothing. A link to Graph from another view only
 * goes there, and Graph puts the focus on the row as its address names it.
 *
 * The link is out of the tab order, as every other control inside a row is, unless `tabStops`
 * holds: `f` steps to it and Enter follows it (`useTreeKeymap.ts`'s `rowStops`). The words are
 * one element right inside the chip, so the marks of a node's state and visibility can stand
 * after them. They are the author's (`authorText`), but where the chip marks them as the page's
 * own (`chip.chrome`: the page's word for a lens whose kind has no words, and for the kind of a
 * node no page read so far holds), as `NodeRow.tsx`'s `Chip` marks the page's word for a
 * document.
 *
 * The author's words are drawn through the framework's plain-text projection (`usePlainHeadline`,
 * the reading list's `lens-chip`), from their reading where they keep a mark (`chip.reading`), so
 * an address in code reads as written: each reference in them reads as its target's name, by the
 * name the page published for it and else by a read of its own, and never as a raw address
 * (R22), and a role in them by the issue the page is on. The space the words keep at either end,
 * which the projection trims, stays (`spacedWords`). Nothing is drawn until the projection has
 * answered: a chip draws no address. The page's word is drawn as it is, and so are words that
 * hold nothing to read.
 */
export function NodeLink({ chip, tabStops }: { chip: LensChip; tabStops: boolean }): ReactNode {
  const { pathname, search } = useLocation();
  // The marks of the tree's nodes, by reference, for a chip that carries no state of its own: the
  // lens a cut-short chain stopped at is drawn from the lens facts' own map, which holds none.
  const states = useNodeStatesByReference();
  const id = chip.id;
  const place = id === undefined ? undefined : lensPlaceOf(pathname, search);
  const asWritten = chip.chrome === true || chip.words === "";
  const words = chip.reading ?? chip.words;
  const reading = useMemo(() => wordsReading(words), [words]);
  const { reads, text } = usePlainHeadline(asWritten ? undefined : reading, undefined, {
    issue: parseIssuePath(pathname, search)?.key,
    surface: "lens-chip",
  });
  const drawn = asWritten
    ? chip.words
    : text === undefined
      ? undefined
      : spacedWords(chip.words, text.text);
  if (drawn === undefined) return reads;
  const marks = chip.chrome === true ? pageChrome : authorText;
  const view =
    place?.outline === "conversation" && chip.graphOnly !== true ? "conversation" : "graph";
  return (
    <span className="min-w-0 [overflow-wrap:anywhere]" data-node-chip="">
      {reads}
      {id === undefined || place === undefined ? (
        <span data-chip-name="" {...marks}>
          {drawn}
        </span>
      ) : (
        <Link
          className={`dispatch-inline-link font-medium underline decoration-page-bullet underline-offset-[3px] ${pageMutedText}`}
          data-chip-name=""
          onClick={view === place.outline ? (event) => focusDrawnRow(event, id) : undefined}
          tabIndex={tabStops ? undefined : -1}
          to={rowPathIn(view, place.issueKey, id)}
          {...marks}
        >
          {drawn}
        </Link>
      )}
      {/* The marks of the node the chip names, from its state and its visibility as the tree
          read gives them, or, for a chip that carries neither, from the tree's node at its
          address; after its words in the chip's own size. */}
      <NodeMarks
        compact
        marks={
          chip.state === undefined && chip.visibility === undefined
            ? (marksAt(states, chip.address) ?? {})
            : statesOfRead(chip)
        }
      />
    </span>
  );
}

/** The line the projection read from `words`, `line`, with the one space `words` keep at either
 *  end, which the projection trims: a fact line's part beside a chip keeps it, and a chip's words
 *  beside a part, once for words that are space alone. */
export function spacedWords(words: string, line: string): string {
  const before = words.startsWith(" ") ? " " : "";
  const after = words.endsWith(" ") && line !== "" ? " " : "";
  return `${before}${line}${after}`;
}

/**
 * Focuses, where it stands, the row the tree the chip stands in (`role="tree"`) draws for the
 * node whose id in the tree read is `id`, as `IssueTree.tsx`'s `reach` focuses a row it finds
 * drawn; the link then goes to the address as it always does. A row carries its node's read id
 * (`data-node-read-id`, `TreeNode.readId`), so the row is found by the id the chip names and no
 * second rule of tree ids is needed. A press that opens the link elsewhere - with Control, Meta,
 * Shift or Alt held, or with a button other than the first - leaves the focus where it is.
 */
function focusDrawnRow(event: MouseEvent<HTMLAnchorElement>, id: string): void {
  if (!isPlainClick(event.nativeEvent)) return;
  event.currentTarget
    .closest('[role="tree"]')
    ?.querySelector<HTMLElement>(`[data-node-row][data-node-read-id="${CSS.escape(id)}"]`)
    ?.focus();
}
