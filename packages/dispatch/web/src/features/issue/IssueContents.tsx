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
  pageIssueBg,
  pageKindTag,
  pageLineBorder,
  pageMarkBg,
  pageMutedText,
  pagePaneBg,
  pagePaperBg,
  pagePersonBg,
  pageQuestionBg,
  pageQuietText,
  pageWaitingBg,
} from "../../theme/classes";
import { isViewer } from "../refs/actor";
import { useDialog } from "../shell/useDialog";
import { useFocusedNode } from "./focused-node";
import { drawnLine, nodeWords, type TreeNode } from "./issue-tree";
import { NodeGlyph } from "./NodeGlyph";
import { oneLineCut, useOneLineBox } from "./one-line";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import {
  CONTENTS_MARK_WORDS,
  CONTROL_LABELS,
  type ContentsMark,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  sentenceCase,
} from "./page-words";
import { pathTo } from "./tree-fold";

/**
 * The Contents pane at the left of the issue page (dispatch://INFRA-1196 R14, R8, the
 * prototype's `#left`): the issue's three sentences under "In short", then the list of each
 * question and each of the reader's own threads, which go to their node in the outline. It is
 * drawn from 1024 px up with no filter on, beside the outline; under a filter it is not drawn,
 * and the filter's body has the whole width (the architect's ruling 5). Below 1024 px the
 * sentences and the list are in the sheet the Outline control opens (`OutlineSheet`).
 *
 * The pane holds no property of the issue, and no sentence about itself: its two headings and
 * its one toggle are its whole chrome (R22).
 */
export function IssueContentsPane({
  brief,
  list,
}: {
  /** The issue's three sentences and the two rows under them (`IssueThreeThings`). */
  brief: ReactNode;
  /** The list (`ContentsList`). */
  list: ReactNode;
}): ReactNode {
  return (
    <aside
      aria-label={CONTROL_LABELS.contents}
      className={`sticky top-[var(--issue-identity-height,0px)] flex h-[calc(100dvh-var(--issue-identity-height,0px))] w-[240px] shrink-0 flex-col border-r min-[1241px]:w-[260px] ${pageLineBorder} ${pagePaneBg}`}
      data-testid="issue-contents"
    >
      {brief}
      {list}
    </aside>
  );
}

/**
 * What the page keeps behind the Outline control below 1024 px, as the 390 px picture draws it
 * (the prototype's `#viewbar .ob` and `#sheet`): the control beside the one filter control, and
 * the sheet it opens over the page. The sheet holds, from its head down, the filter row's own
 * tools at that width (`tools`: the node menu, the words filter and the version control, which
 * the filter row moves here), the issue's three sentences (`brief`, the architect's ruling 5:
 * above the outline they cost a phone's first screen 160 px to 239 px), and the list, which
 * closes the sheet once an entry is chosen. Under a filter and over an opened document it holds
 * the tools alone.
 */
export function OutlineSheet({
  brief,
  list,
  tools,
}: {
  brief?: ReactNode;
  list?: (close: () => void) => ReactNode;
  tools?: ReactNode;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const closeButton = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLDivElement>({
    initialFocusRef: closeButton,
    onClose: () => setOpen(false),
    open,
  });
  return (
    <>
      {/* The prototype's `#viewbar .ob`: 34 px, beside the filter control, inside the filter
          row, which takes the header's 24 px target size rather than the 44 px floor. */}
      <button
        aria-expanded={open}
        aria-haspopup="dialog"
        className={`shrink-0 rounded-lg border px-3 py-1.5 text-sm leading-5 font-semibold ${pageHairBorder} ${pagePaperBg} ${pageInkText} ${focusVisibleRing}`}
        onClick={() => setOpen(true)}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.outline}
      </button>
      {open ? (
        <div
          aria-label={CONTROL_LABELS.contents}
          aria-modal="true"
          className={`fixed inset-x-0 top-14 bottom-0 z-50 flex flex-col rounded-t-[14px] shadow-[0_-10px_30px_rgba(0,0,0,0.2)] ${pagePaneBg} ${pageInkText}`}
          data-testid="issue-outline-sheet"
          ref={dialog.containerRef}
          role="dialog"
        >
          <div className="flex shrink-0 items-start gap-2 px-3.5 pt-2.5">
            <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">{tools}</div>
            <button
              className={`rounded-[7px] border px-3 py-[3px] text-sm font-medium ${pageHairBorder} ${pagePaperBg} ${pageMutedText} ${focusVisibleRing}`}
              onClick={() => setOpen(false)}
              ref={closeButton}
              type="button"
              {...pageChrome}
            >
              {CONTROL_LABELS.close}
            </button>
          </div>
          {brief === undefined && list === undefined ? null : (
            <div className="flex min-h-0 flex-1 flex-col overflow-y-auto">
              {brief}
              {list?.(() => setOpen(false))}
            </div>
          )}
        </div>
      ) : null}
    </>
  );
}

/** One entry of the list: a question, or a thread the reader began, with the entries under it,
 *  and what its own branch holds that its marks count (`CONTENTS_MARK_WORDS`). */
export interface ContentsEntry {
  readonly children: readonly ContentsEntry[];
  readonly marks: Readonly<Record<ContentsMark, number>>;
  readonly node: TreeNode;
}

/**
 * The entries of the list, read off the tree the outline draws: each ask and each comment or
 * message the reader wrote that begins a thread, each under the nearest such entry above it in
 * the tree, in the tree's own order. The issue itself is no entry: the header is the issue (the
 * architect's ruling 5), as the outline draws no row of it. A node drawn under a second parent is
 * listed once, at its own place, and counted once: a transcluded copy and the branch under it
 * are the same nodes as at the edited place, so they are neither listed nor counted again.
 *
 * Each entry's marks count its own branch: every node under it that sits under no entry nearer
 * to it, so a node counts once, on the entry whose branch it is read in (the prototype's
 * `branchCounts`): its statements, its decisions waiting for the reader's review, its child
 * issues, and its statements a check under them failed. What sits under the issue and under no
 * entry is counted on none, since the issue has no entry; the filter row's signals count it.
 */
export function contentsOf(root: TreeNode, viewer: string | undefined): ContentsEntry[] {
  const TURNS: Partial<Record<TreeNode["type"], true>> = { comment: true, message: true };
  const tally = (): Record<ContentsMark, number> => ({
    children: 0,
    failing: 0,
    for_review: 0,
    statements: 0,
  });
  const count = (node: TreeNode, marks: Record<ContentsMark, number>): void => {
    if (node.type === "requirement") {
      marks.statements += 1;
      if (node.children.some((child) => child.type === "check" && child.state === "failed")) {
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
      const begunByReader =
        TURNS[child.type] === true &&
        TURNS[node.type] !== true &&
        child.author !== undefined &&
        isViewer(child.author, viewer);
      if (child.type === "ask" || begunByReader) {
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
 * The list itself: its heading with the Focus branch toggle, and one row per entry, each its
 * node's first line by R1's rule, cut at a word to the row's width. It is one tab stop, as the
 * outline is: the arrow keys move inside it, the right and left arrows unfold and fold an entry,
 * and Enter or a press goes to that node in the outline. The entry the reader is in - the one
 * holding the node the focus is on, or last was on - is marked by a rule and a ground of its own.
 */
export function ContentsList({
  focusBranch,
  onFocusBranch,
  onGo,
  root,
}: {
  /** Whether the outline draws one branch at a time, the current one. */
  focusBranch: boolean;
  /** Turns Focus branch on or off, around the entry the reader is in. */
  onFocusBranch: (current: string | undefined) => void;
  /** Goes to the node in the outline. */
  onGo: (id: string) => void;
  /** The tree the outline draws. */
  root: TreeNode;
}): ReactNode {
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const entries = useMemo(() => contentsOf(root, viewer), [root, viewer]);
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
  const focused = useFocusedNode()?.id;
  const [lastIn, setLastIn] = useState<string | undefined>(undefined);
  const inEntry = useMemo(() => {
    if (focused === undefined) return undefined;
    return (pathTo(root, focused) ?? [])
      .map((node) => node.id)
      .filter((id) => entryIds.has(id))
      .at(-1);
  }, [entryIds, focused, root]);
  useEffect(() => {
    if (inEntry !== undefined) setLastIn(inEntry);
  }, [inEntry]);
  const current = inEntry ?? (lastIn !== undefined && entryIds.has(lastIn) ? lastIn : undefined);

  const [folded, setFolded] = useState<ReadonlySet<string>>(() => new Set());
  const rows: { depth: number; entry: ContentsEntry; guides: number }[] = [];
  const walk = (list: readonly ContentsEntry[], depth: number) => {
    for (const entry of list) {
      rows.push({ depth, entry, guides: depth });
      if (!folded.has(entry.node.id)) walk(entry.children, depth + 1);
    }
  };
  walk(entries, 0);
  const fold = (id: string, shut: boolean) =>
    setFolded((held) => {
      const next = new Set(held);
      if (shut) next.add(id);
      else next.delete(id);
      return next;
    });

  const list = useRef<HTMLDivElement | null>(null);
  const box = useOneLineBox(list, true);
  // The list's one tab stop: the row the reader last moved to, else the entry they are in,
  // else the first.
  const [stop, setStop] = useState<string | undefined>(undefined);
  const ids = rows.map((row) => row.entry.node.id);
  const tabStop =
    stop !== undefined && ids.includes(stop) ? stop : (current ?? ids[0] ?? undefined);
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
        <h2
          className={`text-xs leading-4 font-semibold tracking-[0.08em] uppercase ${pageQuietText}`}
          {...pageChrome}
        >
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
        {rows.map(({ depth, entry, guides }, at) => {
          const { node } = entry;
          const holds = entry.children.length > 0;
          const isCurrent = node.id === current;
          const words = nodeWords(node);
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
                    box.widthOf(String(entry.marks[mark]), MARK_TEXT_REM, 600),
                  0
                );
          const cut =
            box === undefined
              ? undefined
              : oneLineCut({
                  after,
                  before: indent + ROW.twisty + ROW.gap + ROW.dot + ROW.gap,
                  room: box.width - ROW.margins - ROW.paddingRight,
                  text: words,
                  widthOf: (text) => box.widthOf(text, ROW_TEXT_REM, isCurrent ? 600 : undefined),
                });
          const line = cut ?? { cut: false, line: words, under: false, wraps: false };
          return (
            <div
              aria-expanded={holds ? !folded.has(node.id) : undefined}
              aria-level={depth + 1}
              aria-selected={isCurrent}
              className={`relative mx-1.5 flex h-8 cursor-pointer items-center gap-[7px] rounded-md pr-1.5 text-sm leading-8 whitespace-nowrap select-none ${focusVisibleRing} ${
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
              {Array.from({ length: guides }, (_, level) => (
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
                  <span aria-hidden="true">{folded.has(node.id) ? "▸" : "▾"}</span>
                </button>
              ) : (
                <span aria-hidden="true" className="w-3 shrink-0" />
              )}
              <span
                aria-hidden="true"
                className={`inline-block size-[7px] shrink-0 rounded-full ${
                  node.type === "issue"
                    ? pageIssueBg
                    : node.type === "ask"
                      ? pageQuestionBg
                      : pagePersonBg
                }`}
              />
              <span className="sr-only" {...pageChrome}>
                {sentenceCase(NODE_TYPE_NAMES[node.type])}
              </span>
              <span
                className={`min-w-0 flex-1 ${line.under || line.wraps ? "whitespace-normal break-words" : ""}`}
                data-contents-line=""
                {...authorText}
              >
                {drawnLine(line)}
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
                      <span {...authorText}>{entry.marks[mark]}</span>
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
