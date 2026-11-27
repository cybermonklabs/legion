import { useQuery } from "@tanstack/react-query";
import { type KeyboardEvent, type ReactNode, useEffect, useMemo, useRef, useState } from "react";

import { whoAmIQuery } from "../../api/queries";
import {
  focusVisibleRing,
  pageFailedText,
  pageGuideBg,
  pageHairBg,
  pageHairBorder,
  pageInkBg,
  pageInkBorder,
  pageInkText,
  pageKindTag,
  pageMarkBg,
  pageMutedText,
  pagePaperBg,
  pagePersonBg,
  pageQuestionBg,
  pageQuietText,
  pageWaitingBg,
} from "../../theme/classes";
import { EVENT_FRESH } from "../refs/reference-target";
import { isContentsEntry } from "./contents-entries";
import { drawnLine, firstLineOf } from "./first-line";
import { useFocusedNode } from "./focused-node";
import { MenuMark } from "./IssuePresets";
import { isCheck, isStatement, lineWordsRaw, type TreeNode } from "./issue-tree";
import { NodeGlyph } from "./NodeGlyph";
import { type LineNames, oneLineCut, referenceStandIns, useOneLineBox } from "./one-line";
import { useOutlineRequest } from "./outline-request";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import {
  CONTENTS_MARK_WORDS,
  CONTROL_LABELS,
  type ContentsMark,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  sentenceCase,
} from "./page-words";
import * as treeFold from "./tree-fold";
import { pathTo } from "./tree-fold";
import { CAP } from "./verification-filter/VerificationFilterBody";

/** One entry of the list: a question, or a thread the reader began, with the entries under it,
 *  and what its own branch holds that its marks count (`CONTENTS_MARK_WORDS`). */
export interface ContentsEntry {
  readonly children: readonly ContentsEntry[];
  readonly marks: Readonly<Record<ContentsMark, number>>;
  readonly node: TreeNode;
}

/**
 * The entries of the list, read off the tree the outline draws by `isContentsEntry`: asks,
 * comment threads the reader began, and message threads the reader began with a non-message
 * node placed in them. Each sits under the nearest entry above it, in the tree's order. The
 * issue itself is no entry: the header is the issue, as the outline draws no row of it. A
 * transcluded copy and the branch under it are neither listed nor counted again: the list
 * reads those nodes at their own place.
 *
 * Each entry's marks count its own branch: every node under it that sits under no entry nearer
 * to it, and at or under no overtaken node, so a node counts once, on the entry whose branch it
 * is read in (the prototype's `branchCounts`): its statements, its decisions waiting for the
 * reader's review, its child issues, and its statements a check under them failed, of the checks
 * a count reads (`outOfCounts`). A statement and a check are what the filter row counts as one
 * (`isStatement`, `isCheck`): a `requirement` or `check` block of any other document is counted
 * as neither. What sits under the issue and under no entry is counted on none, since the issue
 * has no entry; the filter row's signals count it. `producing` is the messages that produced a
 * node in the whole tree (`producingMessages`), which `root` may be a words filter's cut of.
 */
export function contentsOf(
  root: TreeNode,
  viewer: string | undefined,
  producing: ReadonlySet<string>
): ContentsEntry[] {
  const tally = (): Record<ContentsMark, number> => ({
    children: 0,
    failing: 0,
    for_review: 0,
    statements: 0,
  });
  const count = (node: TreeNode, marks: Record<ContentsMark, number>): void => {
    if (node.closed === true) return;
    // A check no count reads - proposed, or at or under an overtaken node - marks no statement
    // failing, nor does any check of a statement no count reads - a proposed one - as the
    // Verification view and the failing flag leave them out: the statement is read once more with
    // the checks a count reads alone. It is still one of the entry's statements.
    const counted = (child: TreeNode): boolean =>
      !isCheck(child) || !(treeFold.outOfCounts(node) || treeFold.outOfCounts(child));
    if (isStatement(node) && !node.children.every(counted)) {
      count({ ...node, children: node.children.filter(counted) }, marks);
      return;
    }
    if (isStatement(node)) {
      marks.statements += 1;
      if (node.children.some((child) => isCheck(child) && child.state === "failed")) {
        marks.failing += 1;
      }
    }
    if (node.type === "decision" && node.waiting) marks.for_review += 1;
    if (node.type === "child_issue") marks.children += 1;
  };
  const walk = (
    node: TreeNode,
    into: ContentsEntry[],
    marks: Record<ContentsMark, number>
  ): void => {
    for (const child of node.children) {
      if (child.transcluded) continue;
      // The list keeps each entry, even at or under an overtaken node, because the entry rule
      // reads what kind of node it is, not what its branch holds.
      if (isContentsEntry(node, child, viewer, producing)) {
        const children: ContentsEntry[] = [];
        const own = tally();
        walk(child, children, own);
        into.push({ children, marks: own, node: child });
      } else {
        count(child, marks);
        walk(child, into, marks);
      }
    }
  };
  const entries: ContentsEntry[] = [];
  walk(root, entries, tally());
  return entries;
}

/** Each mark's shape and colour, in the order an entry draws them (the prototype's `.cnts`). */
const MARKS: readonly {
  readonly glyph: string;
  readonly mark: ContentsMark;
  readonly tone: string;
}[] = [
  { glyph: GLYPH_TOKENS.types.requirement, mark: "statements", tone: pageKindTag.statement },
  { glyph: GLYPH_TOKENS.types.decision, mark: "for_review", tone: pageKindTag.decision },
  { glyph: GLYPH_TOKENS.types.child_issue, mark: "children", tone: pageKindTag.child },
  { glyph: GLYPH_TOKENS.states.check.failed, mark: "failing", tone: pageFailedText },
];

/** The pixels a row of the list spends beside its words, from the prototype's `.mrow`: its
 *  side margins and right padding, the 6 px it starts at and 16 px a level, the twisty's 12,
 *  the bullet's 7, and the 7 px gaps between them; and after the words the waiting dot's 8 and
 *  each mark's 12 px shape, the 2 px between it and its count, and the 6 px between marks
 *  (`.cnts`). */
const ROW = {
  dot: 7,
  gap: 7,
  indent: 16,
  inset: 6,
  margins: 12,
  markGap: 6,
  markGlyph: 12,
  markInner: 2,
  paddingRight: 6,
  twisty: 12,
  waiting: 8,
};

/** The row's words are 14 px: 0.875 rem; a mark's count is 12 px. */
const ROW_TEXT_REM = 0.875;
const MARK_TEXT_REM = 0.75;

/**
 * The Contents list (R14, the prototype's `#left`): its heading with the Focus branch toggle,
 * and one row per entry, each its node's first line by R1's rule, cut from its first block's
 * words with each bare address as written (`lineWordsRaw`) at a word to the row's width, with
 * each reference in it named once the line is cut (`referenceStandIns`), as the node's row in the
 * outline names it, from the names the page published: an issue by its key and its title, and a
 * reference the page has no name for as its author wrote it; a node with no words is named by its
 * kind's word. `slots/contents.tsx` draws it under the issue's three sentences, in the Contents
 * pane from 1024 px up and in its Outline sheet below that (`contents-frame.tsx`). It is one tab
 * stop, as the outline is: the arrow keys move inside it, the right and left arrows unfold and
 * fold an entry, and Enter or a press goes to that node in the outline. The entry the reader is
 * in - the one holding the node the focus is on, or last was on, else the entry the list last
 * took the outline to - is marked by a rule and a ground of its own, and is the branch Focus
 * branch draws.
 */
export function ContentsList({
  focusBranch,
  lineNames,
  onFocusBranch,
  onGo,
  producing,
  root,
}: {
  /** Whether the outline draws one branch at a time, the current one. */
  focusBranch: boolean;
  /** What a reference an entry's words cite is named by (`useIssueNames`). */
  lineNames: LineNames;
  /** Turns Focus branch on or off, around the entry the reader is in. */
  onFocusBranch: (current: string | undefined) => void;
  /** Goes to the node in the outline. */
  onGo: (id: string) => void;
  /** The messages that produced a node in the whole tree (`producingMessages`). */
  producing: ReadonlySet<string>;
  /** The tree the outline draws. */
  root: TreeNode;
}): ReactNode {
  // The reader, read as the tree reads it (`useIssueTree`, `EVENT_FRESH`): staleness is per
  // observer, so under the app's 30 s `staleTime` the list drawn later would ask for it again.
  const viewer = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  const entries = useMemo(() => contentsOf(root, viewer, producing), [producing, root, viewer]);
  const entryIds = useMemo(() => {
    const ids = new Set<string>();
    const walk = (list: readonly ContentsEntry[]) => {
      for (const entry of list) {
        ids.add(entry.node.id);
        walk(entry.children);
      }
    };
    walk(entries);
    return ids;
  }, [entries]);
  // The entry the reader is in: the nearest entry at or above the node the focus is on. The
  // focus leaving the outline - for this list, say - leaves the entry the reader was last in.
  // A list drawn again has seen no focus of its own: the Outline sheet draws a new one each time
  // it opens, after the focus left the outline for the Outline control. Where the focus is in no
  // entry and was in none the list saw, the reader is in the entry the list last took the outline
  // to, which the page keeps across the sheet's openings (`useOutlineRequest`). The last two hold
  // only while they are entries the list draws, so Focus branch draws no branch the words filter
  // left out, or one that is gone.
  const focused = useFocusedNode()?.id;
  const requested = useOutlineRequest()?.id;
  const [lastIn, setLastIn] = useState<string | undefined>(undefined);
  const inEntry = useMemo(
    () => nearestAtOrAbove(root, focused, (id) => entryIds.has(id)),
    [entryIds, focused, root]
  );
  useEffect(() => {
    if (inEntry !== undefined) setLastIn(inEntry);
  }, [inEntry]);
  const current =
    inEntry ??
    (lastIn !== undefined && entryIds.has(lastIn) ? lastIn : undefined) ??
    (requested !== undefined && entryIds.has(requested) ? requested : undefined);

  const [folded, setFolded] = useState<ReadonlySet<string>>(() => new Set());
  const rows = useMemo(() => {
    const found: { depth: number; entry: ContentsEntry }[] = [];
    const walk = (list: readonly ContentsEntry[], depth: number) => {
      for (const entry of list) {
        found.push({ depth, entry });
        if (!folded.has(entry.node.id)) walk(entry.children, depth + 1);
      }
    };
    walk(entries, 0);
    return found;
  }, [entries, folded]);
  const fold = (id: string, shut: boolean) =>
    setFolded((held) => {
      const next = new Set(held);
      if (shut) next.add(id);
      else next.delete(id);
      return next;
    });

  const list = useRef<HTMLDivElement | null>(null);
  const box = useOneLineBox(list);
  // What each row draws of its entry - its words, their cut and its marks - read again only
  // when what they are read from changes: the list is drawn again on every move of the focus in
  // the outline (`useFocusedNode`), and a cut measures its words on a canvas. Each mark's count is
  // of the nodes loaded, and carries "+" while the read holds nodes no page read yet holds (R15).
  const plus = root.moreToRead === true ? "+" : "";
  const drawn = useMemo(
    () =>
      rows.map(({ depth, entry }) => {
        const { node } = entry;
        const isCurrent = node.id === current;
        // The line is cut with each reference it cites unnamed and measured with each named, so
        // no cut falls inside a word of a name (`referenceStandIns`).
        const references = referenceStandIns(lineNames);
        const held = references.standIn(lineWordsRaw(node));
        const unnamed = held.trim() === "";
        const words = unnamed ? NODE_TYPE_NAMES[node.type] : held;
        const indent = ROW.inset + depth * ROW.indent;
        const marks = MARKS.filter(({ mark }) => entry.marks[mark] > 0);
        // What stands after the words: the waiting dot, then each mark, its shape, its count
        // and the gaps between them, which the words' own line is cut to leave room for.
        const after =
          box === undefined
            ? 0
            : (node.waiting ? ROW.gap + ROW.waiting : 0) +
              marks.reduce(
                (total, { mark }) =>
                  total +
                  (total === 0 ? ROW.gap : ROW.markGap) +
                  ROW.markGlyph +
                  ROW.markInner +
                  box.widthOf(`${entry.marks[mark]}${plus}`, MARK_TEXT_REM, 600),
                0
              );
        // Every other part of the row counts before the line and none after it: where they
        // leave the line no word, it stands under all of them on a line of its own (R3), as
        // wide as the row inside its indent, and `oneLineCut` fits a line under the parts before
        // it to `room` less the parts after it.
        const cut =
          box === undefined
            ? undefined
            : oneLineCut({
                after: 0,
                before: ROW.twisty + ROW.gap + ROW.dot + ROW.gap + after,
                room: box.width - ROW.margins - ROW.paddingRight - indent,
                text: words,
                widthOf: (text) =>
                  box.widthOf(references.named(text), ROW_TEXT_REM, isCurrent ? 600 : undefined),
              });
        // Until the list's box is read, the entry draws its first line by R1's rule uncut by
        // width, never its whole words (`useOneLineBox`).
        const line = cut ?? { ...firstLineOf(words), under: false, wraps: false };
        const drawnWords = references.named(drawnLine(line));
        return { drawnWords, indent, isCurrent, line, marks, unnamed };
      }),
    [box, current, lineNames, plus, rows]
  );
  // The list's one tab stop, always on a drawn row, so Tab never skips the list: the row the
  // reader last moved to, else the nearest drawn entry at or above the entry they are in - a
  // fold above that entry hides it - else the first.
  const [stop, setStop] = useState<string | undefined>(undefined);
  const ids = rows.map((row) => row.entry.node.id);
  const drawnAtOrAbove = nearestAtOrAbove(root, current, (id) => ids.includes(id));
  const tabStop = stop !== undefined && ids.includes(stop) ? stop : (drawnAtOrAbove ?? ids[0]);
  const moveTo = (id: string | undefined) => {
    if (id === undefined) return;
    setStop(id);
    list.current?.querySelector<HTMLElement>(`[data-contents-entry="${CSS.escape(id)}"]`)?.focus();
  };
  const onKeyDown = (event: KeyboardEvent<HTMLElement>, at: number) => {
    const row = rows[at];
    if (row === undefined) return;
    const id = row.entry.node.id;
    const holds = row.entry.children.length > 0;
    const keys: Record<string, () => void> = {
      ArrowDown: () => moveTo(ids[at + 1]),
      ArrowLeft: () => {
        if (holds && !folded.has(id)) fold(id, true);
        else
          moveTo(
            rows
              .slice(0, at)
              .filter((above) => above.depth < row.depth)
              .at(-1)?.entry.node.id
          );
      },
      ArrowRight: () => {
        if (holds && folded.has(id)) fold(id, false);
        else if (holds) moveTo(ids[at + 1]);
      },
      ArrowUp: () => moveTo(ids[at - 1]),
      End: () => moveTo(ids.at(-1)),
      Enter: () => onGo(id),
      Home: () => moveTo(ids[0]),
    };
    const run = keys[event.key];
    if (run === undefined) return;
    event.preventDefault();
    event.stopPropagation();
    run();
  };

  return (
    <>
      <div className="flex shrink-0 items-center justify-between gap-2 pt-2.5 pr-2.5 pb-0.5 pl-3.5">
        <h2 className={CAP} {...pageChrome}>
          {CONTROL_LABELS.contents}
        </h2>
        <button
          aria-pressed={focusBranch}
          className={`inline-flex items-center gap-[7px] rounded-full border py-px pr-2.5 pl-1 text-xs leading-5 font-medium whitespace-nowrap ${pagePaperBg} ${
            focusBranch ? `${pageInkBorder} ${pageInkText}` : `${pageHairBorder} ${pageMutedText}`
          } ${focusVisibleRing}`}
          onClick={() => onFocusBranch(current)}
          type="button"
          {...pageChrome}
        >
          <span
            aria-hidden="true"
            className={`relative h-4 w-[26px] shrink-0 rounded-lg ${focusBranch ? pageInkBg : pageHairBg}`}
          >
            <span
              className={`absolute top-0.5 size-3 rounded-full ${pagePaperBg} ${
                focusBranch ? "left-3" : "left-0.5"
              }`}
            />
          </span>
          {CONTROL_LABELS.focus_branch}
        </button>
      </div>
      <div
        aria-label={CONTROL_LABELS.contents}
        className="min-h-0 flex-1 overflow-x-hidden overflow-y-auto pb-2"
        ref={list}
        role="tree"
      >
        {rows.map(({ depth, entry }, at) => {
          const { node } = entry;
          const holds = entry.children.length > 0;
          const { drawnWords, indent, isCurrent, line, marks, unnamed } = drawn[at];
          // A line that has less room beside the row's other parts than its first word stands
          // under all of them, on a line of its own at the entry's indent, and the row grows to
          // hold it (R3).
          const under = line.under || line.wraps;
          return (
            <div
              aria-expanded={holds ? !folded.has(node.id) : undefined}
              aria-level={depth + 1}
              aria-selected={isCurrent}
              className={`relative mx-1.5 flex ${
                under ? "min-h-8 flex-wrap gap-x-[7px]" : "h-8 gap-[7px]"
              } cursor-pointer items-center rounded-md pr-1.5 text-sm leading-8 whitespace-nowrap select-none ${focusVisibleRing} ${
                isCurrent
                  ? `font-semibold shadow-[inset_2px_0_0_var(--color-page-ink)] ${pageMarkBg} ${pageInkText}`
                  : pageInkText
              }`}
              data-contents-entry={node.id}
              key={node.id}
              onClick={() => {
                setStop(node.id);
                onGo(node.id);
              }}
              onKeyDown={(event) => onKeyDown(event, at)}
              role="treeitem"
              style={{ paddingLeft: `${indent}px` }}
              tabIndex={node.id === tabStop ? 0 : -1}
            >
              {Array.from({ length: depth }, (_, level) => (
                <i
                  aria-hidden="true"
                  className={`absolute inset-y-0 w-px ${pageGuideBg}`}
                  // biome-ignore lint/suspicious/noArrayIndexKey: a guide is its level and nothing else.
                  key={level}
                  style={{ left: `${ROW.inset + 2 + level * ROW.indent + 6}px` }}
                />
              ))}
              {holds ? (
                <button
                  aria-label={folded.has(node.id) ? CONTROL_LABELS.unfold : CONTROL_LABELS.fold}
                  className={`h-8 w-3 shrink-0 text-center text-xs ${pageQuietText}`}
                  onClick={(event) => {
                    event.stopPropagation();
                    fold(node.id, !folded.has(node.id));
                  }}
                  tabIndex={-1}
                  type="button"
                >
                  {/* The view row's mark, turned a quarter to point right while the entry is
                      folded. It is a shape and not text, as `StatusGlyph`'s are, because the
                      page's typeface lacks ▸ and ▾: as text, they are drawn in a face of the
                      reader's own machine, at that face's width. The control's own `aria-label`
                      names the act, so the shape is hidden from assistive technology. */}
                  <span
                    aria-hidden="true"
                    className={`inline-flex shrink-0 align-middle ${
                      folded.has(node.id) ? "-rotate-90" : ""
                    }`}
                  >
                    <MenuMark />
                  </span>
                </button>
              ) : (
                <span aria-hidden="true" className="h-8 w-3 shrink-0" />
              )}
              <span
                aria-hidden="true"
                className={`inline-block size-[7px] shrink-0 rounded-full ${
                  node.type === "ask" ? pageQuestionBg : pagePersonBg
                }`}
              />
              <span className="sr-only" {...pageChrome}>
                {sentenceCase(NODE_TYPE_NAMES[node.type])}
              </span>
              <span
                className={`min-w-0 ${
                  under ? "order-last basis-full whitespace-normal break-words" : "flex-1"
                }`}
                data-contents-line=""
                {...(unnamed ? pageChrome : authorText)}
              >
                {drawnWords}
              </span>
              {/* An entry whose question waits on the reader carries the waiting dot, and says so
                  in words to a reader who cannot see it (the prototype's `.mrow .dot`). */}
              {node.waiting ? (
                <span className="inline-flex shrink-0 items-center" data-contents-waiting="">
                  <i aria-hidden="true" className={`size-2 rounded-full ${pageWaitingBg}`} />
                  <span className="sr-only" {...pageChrome}>
                    {PROPERTY_LABELS.waiting_on_you}
                  </span>
                </span>
              ) : null}
              {marks.length === 0 ? null : (
                <span className="flex shrink-0 gap-1.5">
                  {marks.map(({ glyph, mark, tone }) => (
                    <span
                      className={`inline-flex items-center gap-0.5 text-xs leading-8 font-semibold ${tone}`}
                      data-contents-mark={mark}
                      key={mark}
                    >
                      <span aria-hidden="true" className="inline-flex">
                        <NodeGlyph className="size-3 shrink-0" token={glyph} />
                      </span>
                      <span {...authorText}>
                        {entry.marks[mark]}
                        {plus}
                      </span>
                      <span className="sr-only" {...pageChrome}>
                        {CONTENTS_MARK_WORDS[mark]}
                      </span>
                    </span>
                  ))}
                </span>
              )}
            </div>
          );
        })}
      </div>
    </>
  );
}

/** The nearest node at or above node `id` in `root` that `counts`: the last of them on the path
 *  from the root down to it, which is the one rule both the entry the reader is in and the
 *  tab stop's fallback are read by. Undefined where `id` is, or where no node on that path
 *  counts. */
function nearestAtOrAbove(
  root: TreeNode,
  id: string | undefined,
  counts: (id: string) => boolean
): string | undefined {
  if (id === undefined) return undefined;
  return (pathTo(root, id) ?? [])
    .map((node) => node.id)
    .filter(counts)
    .at(-1);
}
