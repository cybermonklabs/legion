import { type ReactNode, useCallback, useEffect, useRef, useState } from "react";

import type { IssueDetails } from "../../../api/types";
import { borderDefault, surfaceBg, textPrimaryOnSurface } from "../../../theme/classes";
import { MARGIN_WORDS, type Preset } from "../page-words";
import { rowTextOf, type TextRuns, textRuns } from "./row-text-runs";
import { type SelectedWords, sendTargetOf } from "./SelectionComposer";
import { quoteOf } from "./selection-quote";
import { quotedNodeOf, useComposerPlace, useTextComposer } from "./TextMargin";
import { subjectOf, writeFor } from "./text-comment-write";
import { useStageIssue, useTextThreads, type ViewThreads } from "./useTextThreads";

/**
 * The control that comments on selected words (R5): while a selection lies inside the words of
 * one row it draws a button beside the selection's end (`position: fixed`, `left` and `top` in
 * px from the last client rectangle of the range), and `$mod+Alt+m` (matched by
 * `event.code === "KeyM"`, so macOS's Alt+M, which types "µ", still takes it) does what the button
 * does. A press, or the chord with a quote, opens the composer (`useTextComposer`), which closes
 * Details first; the chord with no quote does nothing. It draws no control on an author-only
 * node, a child issue's row, or a kind whose gate is off, nor where the read gives the node's
 * write nothing to name (`sendTargetOf`). It takes no property: it reads the view the stage draws
 * from the reads the page holds (`useStageIssue`, `useTextThreads`). The page's root mounts it
 * once, beside the stage and the column (`IssueOutlinePage.tsx`), and it serves the column and
 * each node's sheet alike.
 *
 * The chord is a `keydown` listener of the document's, not a binding of the page's keymap: the
 * keymap matches a key by the character it types and by its place only on a Mac (`keymap.ts`),
 * and the chord is matched by its place on every platform (decision 12). A chord with a quote
 * takes the key before the keymap's own listener, on the window, reads it; one with none leaves
 * it to the keymap, so an editor's own `$mod+Alt+m` still takes it there.
 */
export function SelectionComment(): ReactNode {
  const stage = useStageIssue();
  return stage === undefined ? null : <Control issue={stage.issue} preset={stage.preset} />;
}

/** The mark of the control's box, which a press on the control is told apart by. */
const CONTROL_ATTRIBUTE = "data-selection-comment";

/** How far right of the selection's end the control stands, in px. */
const GAP = 4;

/** The words a selection quotes and where the control stands for them. */
interface Selected {
  readonly left: number;
  readonly top: number;
  readonly words: SelectedWords;
}

/** The mark every row carries, which names its node by its tree id. */
const ROW = "[data-node-id]";

/** What a row draws a text block of its node's words as: a paragraph, a heading, a code block, a
 *  list item, a table cell, a term or its description. */
const TEXT_BLOCK = "p, h1, h2, h3, h4, h5, h6, pre, li, td, th, dt, dd";

/**
 * Whether `range` takes the words of one text block of the row's words element, whose runs are
 * `runs`: the block of the first character it takes is the block of its last, its edges read as
 * `quoteOf` reads them (a range that ends at the start of the next block ends in the block
 * before). The server's search refuses words that span text blocks (`pmdoc.ErrTargetSpansBlocks`),
 * so such a selection takes no control, as one across two blocks of an item takes none (`quoteOf`).
 */
function inOneTextBlock(range: Range, runs: TextRuns): boolean {
  const start = runs.offsetOf(range.startContainer, range.startOffset);
  const end = runs.offsetOf(range.endContainer, range.endOffset);
  if (start === null || end === null) return false;
  const first = runs.runs.find((run) => run.end > start);
  const last = runs.runs.filter((run) => run.start < end).at(-1);
  if (first === undefined || last === undefined) return false;
  const blockOf = (run: Node) => run.parentElement?.closest(TEXT_BLOCK) ?? null;
  return blockOf(first.node) === blockOf(last.node);
}

/**
 * What the document's selection quotes in the row its start is in, on the page of `issue` drawn as
 * `view`, or null: no single range, a collapsed one, no row the view draws a node of, a node no
 * write is offered on (`writeFor`), a selection that is not inside that row's own words
 * (`quoteOf`), one across two of their text blocks (`inOneTextBlock`), or words no send is offered
 * on (`sendTargetOf`). The control stands right of the last rectangle of the range, at its top.
 * The node's write is asked first, so a selection in a row that takes none reads none of the
 * row's words in each frame it is read in.
 */
function selectedIn(
  view: ViewThreads,
  issue: IssueDetails,
  placed: (nodeId: string) => boolean
): Selected | null {
  const selection = document.getSelection();
  if (selection === null || selection.rangeCount !== 1 || selection.isCollapsed) return null;
  const range = selection.getRangeAt(0);
  const start = range.startContainer;
  const row = (start instanceof Element ? start : start.parentElement)?.closest<HTMLElement>(ROW);
  const nodeId = row?.dataset.nodeId;
  const node = nodeId === undefined ? undefined : view.nodes.get(nodeId);
  if (row === null || row === undefined || nodeId === undefined || node === undefined) return null;
  if (!placed(nodeId) || writeFor(subjectOf(node, issue.key)) === null) return null;
  const quoted = quotedNodeOf(view, nodeId);
  const words = rowTextOf(row);
  if (quoted === undefined || words === null) return null;
  // The row's words are read once, for the quote and for its text blocks.
  const runs = textRuns(words);
  const quote = quoteOf(selection, row, quoted, runs);
  if (quote === null || !inOneTextBlock(range, runs)) return null;
  const selected = {
    block: quote.block?.block_id ?? null,
    nodeId,
    occurrence: quote.occurrence,
    quote: quote.quote,
  };
  if (sendTargetOf(view, node, issue, selected) === null) return null;
  const rects = range.getClientRects();
  const end = rects.item(rects.length - 1) ?? range.getBoundingClientRect();
  return { left: end.right + GAP, top: end.top, words: selected };
}

/** Whether `event` is `$mod+Alt+m` by the key's place: Meta or Control, as the page's keymap
 *  reads `$mod`, with Alt and no Shift. */
function isChord(event: KeyboardEvent): boolean {
  return (
    event.code === "KeyM" && event.altKey && (event.metaKey || event.ctrlKey) && !event.shiftKey
  );
}

function Control({ issue, preset }: { readonly issue: IssueDetails; readonly preset: Preset }) {
  const view = useTextThreads(issue, preset);
  const composer = useTextComposer();
  const placed = useComposerPlace();
  const [selected, setSelected] = useState<Selected | null>(null);
  // The listeners read the view, the issue, the composer and where it has a place as they stand,
  // so they are not bound again whenever a read answers.
  const now = useRef({ composer, issue, placed, view });
  now.current = { composer, issue, placed, view };
  const find = useCallback(
    () => selectedIn(now.current.view, now.current.issue, now.current.placed),
    []
  );
  // A selection that stands while the composer's place changes - the window crosses 1024 px, a
  // sheet is drawn or goes - draws its control, or loses it, with no change of the selection.
  useEffect(() => {
    setSelected(selectedIn(now.current.view, now.current.issue, placed));
  }, [placed]);

  // The selection is read again at most once in an animation frame, however often it changes, and
  // not while a pointer button is down: the control is a later element of the page than the rows,
  // fixed a few pixels right of the selection's end, so a drag that goes on past that end meets it
  // and takes every word from the row to it (measured in a browser: "th", then the rest of the
  // page). A press hides it, and the release reads the selection the drag made.
  useEffect(() => {
    let frame: number | undefined;
    let pressed = false;
    const changed = () => {
      if (frame !== undefined) return;
      frame = requestAnimationFrame(() => {
        frame = undefined;
        setSelected(pressed ? null : find());
      });
    };
    const press = (event: Event) => {
      // A press on the control itself keeps it: the click needs the button under the pointer at
      // the release, and the control hides only for a drag that makes the selection.
      if (event.target instanceof Element && event.target.closest(`[${CONTROL_ATTRIBUTE}]`)) return;
      pressed = true;
      changed();
    };
    const release = () => {
      pressed = false;
      changed();
    };
    document.addEventListener("selectionchange", changed);
    document.addEventListener("pointerdown", press, true);
    document.addEventListener("pointerup", release, true);
    document.addEventListener("pointercancel", release, true);
    return () => {
      document.removeEventListener("selectionchange", changed);
      document.removeEventListener("pointerdown", press, true);
      document.removeEventListener("pointerup", release, true);
      document.removeEventListener("pointercancel", release, true);
      if (frame !== undefined) cancelAnimationFrame(frame);
    };
  }, [find]);

  // The chord reads the selection as it stands when pressed.
  useEffect(() => {
    const chord = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.isComposing || !isChord(event)) return;
      const found = find();
      if (found === null) return;
      event.preventDefault();
      now.current.composer.open(found.words);
    };
    document.addEventListener("keydown", chord);
    return () => document.removeEventListener("keydown", chord);
  }, [find]);

  const landed = composer.landed;
  const box = `rounded border text-sm shadow-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnSurface}`;
  return (
    <>
      {selected === null ? null : (
        <div
          className="fixed z-30"
          style={{ left: selected.left, top: selected.top }}
          {...{ [CONTROL_ATTRIBUTE]: "" }}
        >
          <button
            className={`px-2 py-0.5 ${box}`}
            onClick={() => composer.open(selected.words)}
            // The press keeps the selection the control stands for.
            onMouseDown={(event) => event.preventDefault()}
            type="button"
          >
            {MARGIN_WORDS.comment}
          </button>
        </div>
      )}
      {landed === undefined || !("notice" in landed) ? null : (
        <p
          className={`fixed bottom-4 left-1/2 z-30 max-w-md -translate-x-1/2 px-3 py-2 ${box}`}
          role="status"
        >
          {landed.notice}
        </p>
      )}
    </>
  );
}
