import {
  createContext,
  type KeyboardEvent,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import type { Actor, Artifact, CheckBlock } from "../../../api/types";
import {
  focusVisibleRing,
  pageFailedText,
  pageGuideBg,
  pageHairBg,
  pageHairBorder,
  pageInkBorder,
  pageInkFill,
  pageInkText,
  pageKindBullet,
  pageKindTag,
  pageLineBorder,
  pageMutedText,
  pagePaneBg,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  pageWordsText,
} from "../../../theme/classes";
import { useAgentText } from "../../refs/AgentName";
import { EditedMark } from "../../refs/PostMarks";
import { postDeletion } from "../../refs/post-marks";
import {
  PublishedReferenceNames,
  type PublishedReferences,
  useKnownReferences,
} from "../../refs/reference-names";
import { parseNodeReference } from "../../refs/routes";
import { appKeymap } from "../../shell/keymap";
import { agentRoleWord } from "../agent-role";
import { useDocumentStage } from "../document-stage";
import { chipName, type NodeChip, type TreeNode } from "../issue-tree";
import { NodeFacts } from "../NodeFacts";
import { NodeGlyph } from "../NodeGlyph";
import { NODE_EDITED_MARKS } from "../NodeRow";
import { DeletedLine, NodeParentLine } from "../node-line";
import { shortDay } from "../node-time";
import type { OutlineReads } from "../outline-reads";
import { authorText, pageChrome } from "../page-structure";
import {
  authorKind,
  type CheckState,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_LINES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  STATE_LINES,
  type StatementState,
  sentenceCase,
  VERIFICATION_WORDS,
} from "../page-words";
import { type Press, trackPress } from "../press-focus";
import { RowText } from "../RowText";
import { FailedReadLine } from "../slots/failed-read-line";
import { type OnEditing, pressAsksCaret, useNodeEditing, useWordsPress } from "../useNodeEditing";
import { moveToRow } from "../useTreeKeymap";
import {
  type CheckReading,
  STATEMENT_STATES,
  type StatementReading,
  shortCommit,
  statementCounts,
  type VerificationReading,
  verificationReading,
} from "./statement-state";

/** The three reads in the one order the body says the failed ones in, whatever order it is given
 *  them in: the coverage read, then the results read, then the Verification document's blocks
 *  read. */
const VERIFICATION_READS = ["coverage", "results", "blocks"] as const;

/** The three reads the Verification view is drawn from: the coverage read, the results read and
 *  the blocks read of the Verification document its checks live in, which places each check
 *  under the statements it serves. */
export type VerificationRead = (typeof VERIFICATION_READS)[number];

/** How the reads the body is drawn from stand: one still out, all of them answered, or one or
 *  more failed before they first answered, each by its name (`FailedRead`). The body says the
 *  failed ones in an order of its own (`VERIFICATION_READS`), whatever order they are given in. */
export type VerificationReads =
  | { readonly state: "waiting" }
  | { readonly state: "answered" }
  | {
      readonly state: "failed";
      readonly failed: Partial<Record<VerificationRead, FailedRead>>;
    };

/** A read that failed before it first answered, as its line takes it (`FailedReadLine`): which
 *  refusal the line says, the read's `errorUpdateCount`, which keys the line's alert so it is
 *  announced once for each refusal and not again when a run pauses (`FailedReadLine`'s
 *  `failure`); the press that asks that read alone again; and whether it runs again, while which
 *  a press asks nothing more. */
export interface FailedRead {
  readonly failure: number;
  readonly retry: () => void;
  readonly retrying: boolean;
}

/** Whether a words query is typed: not empty once trimmed, which is how `presetView` reads a
 *  query, so a query of spaces alone is no query. The body is given the view with no query
 *  (`whole`) only where this holds, and the slot and the body's tests build it by this one rule. */
export function queryTyped(query: string): boolean {
  return query.trim() !== "";
}

/**
 * The Verification filter's body, as the approved picture `page-v3-verification-1440.png` and the
 * prototype's `.ver` draw it.
 *
 * It is the picture: a summary of how the issue stands, then each statement with the checks
 * that serve it beside it and each check's latest result beside that. From 761 px up the three
 * stand in three columns, as the prototype's table draws them; at 760 px and below each
 * statement is a card and its checks stand under its words, as the prototype's `.vcards` draw
 * them. 760 px is the prototype's own break, so every break here is written from 761 px up:
 * Tailwind's `max-[760px]:` leaves 760 px itself out.
 *
 * All of it is one tree, named as the outline is. Its first row is the issue's own, which holds
 * the summary and the chips, so the page's `t k`, which focuses the tree's tab stop, leaves the
 * view's top on screen and the next Tab on the chip All; a focused tree element would leave the
 * top of a view taller than the screen above it. Then come each statement, with its checks in
 * one group under it, each check that serves no statement, and each other row. One row holds
 * the tree's tab stop (WAI-ARIA's roving tabindex): the issue's, until the stop moves, and again
 * when the row that held it is no longer drawn. The tree answers the outline's keys (`j`, `k`,
 * the arrows, Home, End, Enter, `o`) by a handler of its own, and the page's `?` lists none of
 * them: a binding of the page's keymap is a row of that list.
 *
 * What it says of a statement and of a check is `statement-state.ts`, which is pure. What it
 * says of the issue - the summary, the chips' counts, how each statement stands and so the
 * state a chip narrows by - is read from the view with no words query. The view the query left
 * says which statements and checks are drawn, and each check drawn is read from the view with
 * no query too: its state, its latest result and its count of earlier results, with the earlier
 * results the query kept drawn under its latest. So a query narrows the rows and never changes
 * how a statement or a check reads, what the summary counts, or how much of a check it draws
 * for matching more of it. A query that matches nothing keeps the issue's row, and the table
 * says so under its headings. Four rules of the page are drawn here:
 *
 * - **One commit, drawn once.** The summary names the commit the issue's results are read
 *   against; no result draws a commit of its own, on its line or in its opened facts, and a
 *   result recorded against another commit says so in its check's word, "Out of date".
 * - **Seven states, each a word and a shape.** A check's state is the page's own
 *   (`NODE_STATE_WORDS.check`), drawn with the shape its token carries (`NodeGlyph`), so no
 *   state is told from another by colour alone.
 * - **Covered, never proven.** The summary counts the statements that carry a check as
 *   covered; no count, legend or heading of this body reads proven or met for it, because a
 *   statement that has a check is not a statement that is true.
 * - **Every value inside its card.** Each cell is a column of its own that may be zero wide
 *   and breaks inside a word where it has nothing else to break on, and a check's attributes
 *   and a result's are the two-column grid `NodeFacts.tsx` draws, which is what keeps a value
 *   from reaching past its card or over another at any width (the old tab's defect).
 *
 * A check's four attributes and what its result recorded are its node's facts
 * (`check-facts.ts`), drawn in the body that opens while the reader's focus is anywhere in the
 * check's row and while the reader keeps it open by the check's Facts control or by `o`, as the
 * outline opens a focused node's body: a `run` as a command with its own copy control, a
 * screenshot as a picture, `when` as words. A statement is drawn whole, and one no check serves
 * carries its gap in full, where the picture draws "No check proves this".
 *
 * A row's node is edited in place, in this view, as a row of the outline is
 * (`useNodeEditing.ts`): Enter on a statement's row, a check's row or another row of the table,
 * or on a check's words, puts the caret at the start of that node's text, in the document the
 * text lives in, and the row says why where none goes in; a pointer's press on the words of any
 * of those rows does the same where the row takes a caret (`takesCaret`), and else only gives the
 * row the focus, and a press on a control inside them, or one that ends a selection made in them,
 * is not the words' (`pressAsksCaret`). The page draws that document's one editor (`onEditing`),
 * and the row draws it on its stage in place of its words while it is the row edited; Escape
 * gives the row back the focus and its words.
 */
export function VerificationFilterBody({
  addressed,
  artifacts,
  checks,
  context,
  isClosed,
  namesLine,
  onEditing,
  published,
  reads,
  view,
  whole,
}: {
  /** The node the address names (`named`, its id) and the call told whether the body draws a row
   *  of it with the reader's chip applied (`tell`), before the browser paints and again as what
   *  it draws changes, once all three reads have answered (`slots/view-verification.tsx`). */
  addressed?: { readonly named: ReadonlySet<string>; readonly tell: (drawn: boolean) => void };
  /** Each artifact of the issue by its slug (`issue-artifacts.ts`), which a row's node is edited
   *  in: the document its text lives in. */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The issue's checks with the results recorded against them, as the results read answers
   *  them (`GET /api/v1/issues/{key}/results`), which the page has already read for its tree. */
  checks: readonly CheckBlock[];
  /** The ids of the rows the filter draws as context, which under Verification is every
   *  statement (`presets.ts`). */
  context: ReadonlySet<string>;
  /** Whether the issue is closed: its documents take no typing, so no press on a row's words
   *  puts a caret in them (`takesCaret`). */
  isClosed: boolean;
  /** The names' line while the page's names reads stand failed (`outline-reads.ts`'s
   *  `useOutlineReads`), the outline's own: its words and a Retry that asks those reads again, and
   *  none while they do not stand failed. Every reference the rows and the parent lines draw
   *  waits on those names (`published`), so while they stand failed each is drawn as its address
   *  and "Also under" names nothing; the line says why. The body draws it over the tree after
   *  its own reads' lines, as the outline draws it after the lines of the reads its rows come
   *  from. */
  namesLine: OutlineReads["names"];
  /** Tells the page which document a row is being edited in, so it draws that document's one
   *  editor for the row to take onto its stage (`IssuePresetPanels.tsx`'s `useOutlineEditing`). */
  onEditing: OnEditing;
  /** What the page publishes of its one batched names read (`useIssueTree.ts`'s `published`),
   *  which the body hands every row it draws in the render that draws it
   *  (`PublishedReferenceNames`), as the outline does (`IssueTree`'s `published`): a reference
   *  the page is asking for waits on the page's answer from a row's first render, rather than
   *  being read again by the row (R8). */
  published: PublishedReferences;
  /** How the three reads the body is drawn from stand (the slot's `reads`): the coverage read,
   *  the results read and the blocks read of the Verification document. Until all three hold an
   *  answer the body draws its tree with no row and no line in it, so it never says that an
   *  issue has no checks, or that a check serves no statement, before the reads that hold them
   *  are in. A read that failed before it first answered holds nothing to draw, so the body says
   *  so over the tree, in one line of the page's words for each such read with a Retry that asks
   *  that read alone again, and keeps its tree with nothing in it, as it does while a read is
   *  out. A read that has answered once stays answered when a later refetch fails: the body
   *  draws what it last answered. */
  reads: VerificationReads;
  /** The tree as the Verification filter left it (`presetView`). */
  view: TreeNode;
  /** The Verification view with no words query, where a query is typed: what the issue holds,
   *  whatever the query left of it. The summary, the chips' counts, each statement's state and
   *  its count of checks elsewhere, and every check drawn are read from it. A query is typed
   *  where it is not empty once trimmed, as `presetView` reads it, so a query of spaces alone is
   *  none. Where none is typed it is `undefined`: the body reads all of that from `view`, and no
   *  result counts as kept by a query. */
  whole: TreeNode | undefined;
}): ReactNode {
  // The query's reading, which says which rows the body draws, and the issue's, which every row
  // drawn is read from and which says how the issue stands. With no query typed `whole` is not
  // given, and the two are one reading.
  const queried = whole !== undefined;
  const read = useMemo(() => verificationReading({ checks, view }), [checks, view]);
  const issue = useMemo(
    () => (whole === undefined ? read : verificationReading({ checks, view: whole })),
    [checks, read, whole]
  );
  const statements = useMemo(() => drawnStatements(read, issue, queried), [issue, queried, read]);
  const table = useMemo(() => drawnTable(read, issue, queried), [issue, queried, read]);
  // Which state the reader narrowed the body to, as the picture's chips do; nothing is
  // narrowed until they press one.
  const [only, setOnly] = useState<StatementState | undefined>(undefined);
  const counts = useMemo(() => statementCounts(issue.statements), [issue]);
  const shown = useMemo(
    () => (only === undefined ? statements : statements.filter(({ state }) => state === only)),
    [only, statements]
  );
  const loose = only === undefined ? table.loose : NO_ROWS;
  const others = only === undefined ? table.others : NO_ROWS;
  // The issue's row is drawn where the issue holds a row of any kind, so an issue with no
  // statement and no check draws no row at all, only the page's line that it has no checks,
  // with a query typed or not. It is read from the issue's reading and not the query's: a query
  // that matches nothing keeps the summary, the legend and the chips of the whole issue, and the
  // table says under its headings that the query left nothing.
  const holds = issue.statements.length > 0 || issue.loose.length > 0 || issue.others.length > 0;
  const matched = read.statements.length > 0 || read.loose.length > 0 || read.others.length > 0;
  // The rows are drawn once all three reads hold an answer, and not while one is out or failed.
  const answered = reads.state === "answered";
  const rows = answered && holds ? treeRows(view.id, shown, loose, others) : [];
  // The lines the body says over the tree: each read that failed before it first answered, by
  // its name with the press that asks it alone again and whether it runs again, in the body's
  // own order (`VERIFICATION_READS`) whatever order the reads are given in, then the names' line
  // (`namesLine`), as the outline draws it after the lines of the reads its rows come from
  // (`outline-reads.ts`'s `useOutlineReads`).
  const failed = reads.state === "failed" ? reads.failed : undefined;
  const failedLines = [
    ...VERIFICATION_READS.flatMap((read) => {
      const line = failed?.[read];
      return line === undefined
        ? []
        : [
            {
              failure: line.failure,
              key: read,
              message: UNREADABLE_LINES[read],
              retry: line.retry,
              retrying: line.retrying,
            },
          ];
    }),
    ...namesLine,
  ];
  // Each node the body draws a row of: each statement, the checks under it, each check that
  // serves no statement, and each other row.
  const drawnNodes = useMemo<readonly TreeNode[]>(
    () =>
      [...shown.flatMap((statement) => [statement, ...statement.checks]), ...loose]
        .map((reading) => reading.node)
        .concat(others),
    [loose, others, shown]
  );
  // Whether a row of the address's node is drawn: a statement, a check, or another row.
  useLayoutEffect(() => {
    if (!answered || addressed === undefined) return;
    addressed.tell(drawnNodes.some(({ id }) => addressed.named.has(id)));
  }, [addressed, answered, drawnNodes]);
  // Each statement of the issue by its address, named as every chip of the page names a block
  // (`chipName`): by the line the names store projects beside its name from the reading the page
  // published for it, its first block that holds words (`KnownReferences.lines`), which "Also
  // under" names a check's other statements by (`AlsoUnder`). Every statement a check is drawn
  // under is one of the issue's, whatever a query or a chip left of them; one the store holds no
  // line for yet is named by nothing until it does, as a chip draws nothing until its name comes.
  const { lines } = useKnownReferences();
  const statementNames = useMemo<ReadonlyMap<string, string>>(
    () =>
      new Map(
        issue.statements.flatMap(({ node: { reference } }): [string, string][] => {
          const line = reference === undefined ? undefined : lines.get(reference);
          return reference === undefined || line === undefined
            ? []
            : [[reference, chipName(parseNodeReference(reference), line)]];
        })
      ),
    [issue, lines]
  );
  // The row that holds the tree's one tab stop, by its id in `rows`: the issue's at first. A
  // row the reader's narrowing took out hands it back to the issue's row, in this render rather
  // than in an effect after it, so the tree is never drawn without its stop.
  const [rovingId, setRovingId] = useState(view.id);
  if (rovingId !== view.id && !rows.includes(rovingId)) setRovingId(view.id);
  const tabIndexFor = (id: string): 0 | -1 => (id === rovingId ? 0 : -1);
  // Each drawn check's press, by its row's id in `rows`: true where the reader pressed its facts
  // open, false where they pressed them shut while the focus held them open, and no entry where
  // the focus alone decides. It is held here rather than in each check so `o` on a statement's
  // row presses each of its checks as their own Facts controls do. A row that is no longer
  // drawn forgets its press, as an unmounted row forgets its state.
  const [presses, setPresses] = useState<ReadonlyMap<string, boolean>>(() => new Map());
  if (presses.size > 0 && [...presses.keys()].some((id) => !rows.includes(id))) {
    setPresses(new Map([...presses].filter(([id]) => rows.includes(id))));
  }
  const press = (id: string, pressed: boolean | undefined) => {
    setPresses((held) => {
      if (held.get(id) === pressed) return held;
      const next = new Map(held);
      if (pressed === undefined) next.delete(id);
      else next.set(id, pressed);
      return next;
    });
  };
  /** Each check row `o` on `row` reaches, by its id in `rows`: a check's own row, or each check
   *  of a statement. Each row element carries its own id (`data-verification-row`), so no id is
   *  read from where its row stands among the others. */
  const checksOf = (row: HTMLElement): string[] => {
    const reached = row.matches(CHECK_ROW)
      ? [row]
      : row.matches(STATEMENT_ROW)
        ? [...row.querySelectorAll<HTMLElement>(CHECK_ROW)]
        : [];
    return reached.flatMap((check) => check.dataset.verificationRow ?? []);
  };
  /** `o`, on a row or on a control inside it: the facts of each check it reaches kept drawn once
   *  the focus leaves, or let go where the reader kept them, as the outline's `o` keeps a row's
   *  body open. The answer is whether the row reached a check. */
  const keepChecks = (row: HTMLElement): boolean => {
    const reached = checksOf(row);
    for (const id of reached) {
      press(id, presses.get(id) === true ? undefined : true);
    }
    return reached.length > 0;
  };
  // The tree's own element, inside which the editing finds the row it edits by the row's key
  // (`data-row-key`, which is the row's id in `rows`).
  const tree = useRef<HTMLDivElement | null>(null);
  // What a key reported where it put no caret, and the row it was pressed on, read from the focus
  // as the Decisions view reads it (`DecisionsFilterBody`'s `reportHere`), which draws it at its
  // end until the focus moves to another row or leaves the view, as an outline's row draws one
  // (`NodeRow`'s `report`). A report made with the focus on no row is drawn at the tab stop.
  const [report, setReport] = useState<{ readonly line: string; readonly row?: string }>();
  const reportHere = useCallback((line: string) => {
    const row = document.activeElement?.closest<HTMLElement>(TREE_ROW)?.dataset.verificationRow;
    setReport(line === "" ? undefined : { line, ...(row === undefined ? {} : { row }) });
  }, []);
  const reportFor = (id: string): string | undefined =>
    report !== undefined && (report.row ?? rovingId) === id ? report.line : undefined;
  // A row's node is edited in the document its text lives in by the outline's own editing,
  // which asks the page for that document's one editor (`onEditing`) and finds the row by its
  // key. An edit of the reader's own needs no note of the view's: the view keys its rows by
  // their nodes' ids, so a read that draws a row again keeps it, and its editor with it.
  const { edit, editing } = useNodeEditing({
    artifacts,
    container: tree,
    onEditing,
    onOwnEdit: () => {},
    onReport: reportHere,
  });
  /** The document the row with this id is being edited in, while it is the row edited. */
  const editingFor = (id: string): string | undefined =>
    editing?.row === id ? editing.artifactId : undefined;
  /** Enter on a row, or on a check's words: the caret asked for in the node of the row with this
   *  id (`useNodeEditing`'s `edit`), which says why where no caret goes in. What a key reported
   *  before goes as it asks. */
  const editRow = (row: string, node: TreeNode) => {
    reportHere("");
    edit(row, node);
  };
  // A pointer's press on a row's words, which asks for the caret as Enter does only where the row
  // takes one, by the outline's one rule (`takesCaret`), and else leaves the row the focus the
  // press gave it with nothing said.
  const pressWords = useWordsPress({ artifacts, edit, isClosed, onReport: reportHere });
  /** Enter on a row itself: the caret asked for in the node it draws (`editRow`), found by the
   *  row's own `data-node-id` among the statements, the checks under them or of the table's own,
   *  and the other rows. The answer is whether the row draws one: the issue's row holds no text,
   *  and Enter there is left to the page. */
  const enterRow = (row: HTMLElement): boolean => {
    const id = row.dataset.verificationRow;
    const node = drawnNodes.find((drawn) => drawn.id === row.dataset.nodeId);
    if (id === undefined || node === undefined) return false;
    editRow(id, node);
    return true;
  };
  // The tree's keys, which are the outline's. The page's keymap listens on `window` and leaves a
  // key a handler held back, so this handler holds back each key it acts on, and only those:
  // every other key reaches the keymap.
  const keyDown = (event: KeyboardEvent<HTMLElement>) => {
    // A key with Ctrl, Alt or Meta is the browser's or another binding's; a key that goes on
    // with a chord the reader began is the keymap's, so `t k` from inside the view turns it
    // off; and a key a control inside a row held back is that control's.
    if (event.altKey || event.ctrlKey || event.metaKey || event.defaultPrevented) return;
    if (appKeymap.pending() !== "") return;
    const target = event.target;
    if (!(target instanceof HTMLElement)) return;
    // A key typed into a field or into text the reader edits is typing. The view draws no field
    // of its own, and the editor of a row it edits is drawn in that row; each keeps its keys.
    if (target.isContentEditable || target.matches("input, select, textarea")) return;
    // A key the tree does nothing with reaches the keymap before the rows are read.
    const key = event.key;
    if (!TREE_KEYS.has(key)) return;
    const elements = [...event.currentTarget.querySelectorAll<HTMLElement>(TREE_ROW)];
    // The row the focus is on, or the one that holds the control it is on: the innermost, so a
    // check's words and the links in them are the check's, and a chip is the issue's.
    const row = target.closest<HTMLElement>(TREE_ROW);
    const at = row === null ? -1 : elements.indexOf(row);
    if (row === null || at === -1) return;
    const to = moveTarget(key, row, elements, at);
    if (to != null) {
      event.preventDefault();
      // The row the focus moves to takes the tree's one tab stop, so Tab back into the tree
      // lands on the row the reader left. It takes the focus as an outline's row does
      // (`moveToRow`), so Home on the first row or End on the last, which leave the focus where
      // it is, still bring that row back on screen where the reader scrolled it off.
      const id = to.dataset.verificationRow;
      if (id !== undefined) setRovingId(id);
      moveToRow(to);
      return;
    }
    // Enter is the row's only on the row itself: on a control inside it, a chip, a link or a
    // check's words, it is that control's own.
    const acted =
      key === "Enter" ? target === row && enterRow(row) : key === "o" && keepChecks(row);
    if (acted) event.preventDefault();
  };
  // The press under way anywhere on the page, followed once for every check row drawn.
  const viewPress = useViewPress();
  const drawnBody = (
    <div className={BODY} data-verification-body="">
      {/* Each line, the page's one line for a failed read (`FailedReadLine`): its words and a
          Retry that asks its read alone again. The view's own reads' lines stand in the body's
          own order whatever order the reads are given in, since the stage the view is drawn in
          gives the first line's Retry the focus when a line leaves with it. They stand over the
          tree and never in it: an alert inside a tree would break the tree's own structure of
          rows. Each stands in an element that names its read, so one read's line is found by
          that name. */}
      {failedLines.map((line) => (
        <div data-verification-unreadable={line.key} key={line.key}>
          <FailedReadLine
            failure={line.failure}
            message={line.message}
            onRetry={line.retry}
            retrying={line.retrying}
          />
        </div>
      ))}
      {/* The page holds one tree on every address, and it holds everything the view draws: the
          issue's row and the table, or in their place the one line that says the issue holds
          nothing to draw. While a read is out or has failed it holds nothing, and is still the
          tree `t k` and the stage focus; the stage hands a focus on it to a refused read's Retry,
          or to the tree's tab stop once the rows come (`ViewStage`). */}
      <div
        aria-label={CONTROL_LABELS.tree}
        className="flex min-w-0 flex-col"
        // The focus moving to another row, or out of the view, is the reader going on from what
        // a key reported at a row.
        onBlur={(event) => {
          const to = event.relatedTarget;
          if (!(to instanceof Node && event.currentTarget.contains(to))) setReport(undefined);
        }}
        onFocus={(event) => {
          const at = event.target.closest<HTMLElement>(TREE_ROW)?.dataset.verificationRow;
          setReport((held) => (held?.row === at ? held : undefined));
        }}
        onKeyDown={keyDown}
        ref={tree}
        role="tree"
        tabIndex={-1}
      >
        {!answered ? null : !holds ? (
          <p className={`text-sm leading-5 ${pageQuietText}`} role="none" {...pageChrome}>
            {EMPTY_STATE_LINES.verification}
          </p>
        ) : (
          <>
            <div
              aria-level={1}
              className="flex min-w-0 flex-col"
              data-node-id={view.id}
              data-node-row=""
              data-verification-issue=""
              data-verification-row={view.id}
              role="treeitem"
              tabIndex={tabIndexFor(view.id)}
            >
              <Summary counts={counts} reading={issue} />
              <Chips
                counts={counts}
                onlyState={only}
                onPress={setOnly}
                total={issue.statements.length}
              />
            </div>
            <div className={TABLE} data-verification-table="">
              <Headings />
              {shown.map((statement) => (
                <Statement
                  context={context.has(statement.node.id)}
                  editingFor={editingFor}
                  key={statement.node.id}
                  onEdit={editRow}
                  onPress={press}
                  onPressWords={pressWords}
                  presses={presses}
                  reportFor={reportFor}
                  statement={statement}
                  statementNames={statementNames}
                  tabIndexFor={tabIndexFor}
                />
              ))}
              {/* What the table says where it draws no row under its headings, rather than
                  headings over nothing: the page's line for no match where the query matched
                  nothing of the issue, whose row still says how the issue stands, and the page's
                  line for a state where the one the reader narrowed to holds no statement: a chip
                  whose count is 0 is a press too, as is a chip whose statements the query left
                  out. */}
              {!matched || (only !== undefined && shown.length === 0) ? (
                <p
                  className={`${CELL} text-sm leading-5 ${pageQuietText}`}
                  data-verification-none=""
                  role="none"
                  {...pageChrome}
                >
                  {matched ? EMPTY_STATE_LINES.verification_none : EMPTY_STATE_LINES.no_match}
                </p>
              ) : null}
              {loose.map((check) => (
                <Check
                  check={check}
                  editing={editingFor(check.node.id)}
                  kept={table.kept.get(check.node.id) ?? NO_KEPT_RESULTS}
                  key={check.node.id}
                  loose={true}
                  onEdit={() => editRow(check.node.id, check.node)}
                  onPress={press}
                  onPressWords={() => pressWords(check.node.id, check.node)}
                  pressed={presses.get(check.node.id)}
                  report={reportFor(check.node.id)}
                  row={check.node.id}
                  statementNames={statementNames}
                  tabIndex={tabIndexFor(check.node.id)}
                />
              ))}
              {others.map((node) => (
                <OtherRow
                  editing={editingFor(node.id)}
                  key={node.id}
                  node={node}
                  onPressWords={() => pressWords(node.id, node)}
                  report={reportFor(node.id)}
                  tabIndex={tabIndexFor(node.id)}
                />
              ))}
            </div>
          </>
        )}
      </div>
    </div>
  );
  return (
    <PublishedReferenceNames published={published}>
      <ViewPressContext.Provider value={viewPress}>{drawnBody}</ViewPressContext.Provider>
    </PublishedReferenceNames>
  );
}

/** What one part of the body draws of its checks, a statement's or the table's own after its
 *  statements (`keptChecks`). */
interface DrawnChecks {
  /** Each check drawn, as the issue's reading of it says it stands, in the order that reading
   *  holds them: the part's own checks and each check a result the query kept alone is drawn
   *  with. */
  readonly checks: readonly CheckReading[];
  /** Each check among `checks` the query kept an earlier result of, by its node's id, with the
   *  ids of those results: the check draws them under its latest result, and no other earlier
   *  one, with its line of earlier results shut, so the view draws no result the reader neither
   *  kept by the query nor opened. */
  readonly kept: ReadonlyMap<string, ReadonlySet<string>>;
  /** The ids of the results the query kept alone that are drawn with their check in `checks`,
   *  and so as no row of their own. */
  readonly lifted: ReadonlySet<string>;
}

/** A statement as the body draws it: the query's reading of it, with the state the issue's
 *  reading gives it, the count its line of checks elsewhere draws, and its checks as
 *  `keptChecks` draws them. */
interface DrawnStatement extends StatementReading {
  /** Each of its checks the query kept an earlier result of, with those results
   *  (`DrawnChecks.kept`). */
  readonly kept: ReadonlyMap<string, ReadonlySet<string>>;
  /** How many checks the coverage read counts for the statement beyond those the view with no
   *  query draws under it, which serve it from an issue under this one. At 0 or less the
   *  statement draws no line for them. */
  readonly elsewhere: number;
}

/**
 * Each statement of the query's reading (`read`), as the issue's reading (`issue`, the view with
 * no query) says it stands, so a query that leaves out a statement's own check never changes its
 * state, with its checks as `keptChecks` draws them. Its count of checks elsewhere is the checks
 * the coverage read counts for it less the distinct checks the issue's reading draws under it,
 * so a query never calls this issue's own checks "elsewhere". A statement the issue's reading
 * does not hold is read from the query's reading alone. With no query typed (`queried` false)
 * the two readings are one, which keeps no result: every result is a child of its check there,
 * and counted as kept each would be drawn.
 */
function drawnStatements(
  read: VerificationReading,
  issue: VerificationReading,
  queried: boolean
): DrawnStatement[] {
  const standing = new Map(issue.statements.map((statement) => [statement.node.id, statement]));
  return read.statements.map((statement) => {
    const held = standing.get(statement.node.id) ?? statement;
    const drawn = new Set(held.checks.map((check) => check.node.id)).size;
    const { checks, kept } = queried
      ? keptChecks(statement.checks, statement.node.children, held.checks)
      : { checks: statement.checks, kept: NO_KEPT_CHECKS };
    return {
      ...statement,
      checks,
      elsewhere: (held.node.counts?.checks ?? 0) - drawn,
      kept,
      state: held.state,
    };
  });
}

/** The rows the table draws of its own after its statements (`drawnTable`). */
interface DrawnTable {
  /** Each check that serves no statement, in the order the issue's reading holds them. */
  readonly loose: readonly CheckReading[];
  /** Each of those checks the query kept an earlier result of, with those results
   *  (`DrawnChecks.kept`). */
  readonly kept: ReadonlyMap<string, ReadonlySet<string>>;
  /** Each other row the query's reading holds but a result drawn with its check: a node an
   *  address named, which outranks the filter. */
  readonly others: readonly TreeNode[];
}

/**
 * The rows the table draws after its statements: each check that serves no statement, and each
 * other row of the query's reading (`read`). Under a query (`queried`) its checks are drawn as
 * `keptChecks` draws a statement's, from the issue's reading (`issue`) of them and in the order
 * that reading holds them: the query's own, and each one a result the query kept alone is lifted
 * from. Such a result stands at the top of the query's view with its check as its parent line,
 * where the query's reading holds it among its other rows; it is drawn with its check instead,
 * and not as a row of its own. With no query typed the two readings are one, and the rows are
 * drawn as they read.
 */
function drawnTable(
  read: VerificationReading,
  issue: VerificationReading,
  queried: boolean
): DrawnTable {
  if (!queried) return { kept: NO_KEPT_CHECKS, loose: read.loose, others: read.others };
  const { checks, kept, lifted } = keptChecks(read.loose, read.others, issue.loose);
  return { kept, loose: checks, others: read.others.filter((node) => !lifted.has(node.id)) };
}

/** The `kept` of a part of the body drawn with no query, which keeps no result, and the kept
 *  results of a check the query kept no earlier result of: such a check draws its latest result
 *  alone until the reader opens its line of earlier results. */
const NO_KEPT_CHECKS: ReadonlyMap<string, ReadonlySet<string>> = new Map();
const NO_KEPT_RESULTS: ReadonlySet<string> = new Set();

/** What a chip leaves of the loose checks and the other rows: one empty list for every render. */
const NO_ROWS: readonly never[] = [];

/**
 * The checks one part of the body draws under a query, a statement's or the table's own, in the
 * order the issue's reading holds them in that part (`held`), each that reading of it with the
 * results the query kept of it: the part's own checks in the query's reading (`own`), and each
 * check whose result the query kept where it did not keep the check (from `rows`, the rows the
 * query's reading holds in that part). So every check drawn reads as it does with no query, its
 * state, its latest result and its count of earlier results alike, and stands where it stands
 * with no query, and matching more of a check never draws less of it. A check of the query's own
 * that the issue's reading does not hold is drawn after those, as the query's reading has it.
 *
 * A query that matches a check's words keeps of its results those whose words it matches too,
 * and the query's reading takes the check's latest and earlier results from those alone
 * (`statement-state.ts`), so it can hold no latest and a short count; the results it does hold,
 * but the latest the issue's reading draws anyway, are the ones the query kept. A query that
 * matches a result's words and not its check's keeps the result and lifts it to where the check
 * stood, with the check as its parent line (`presets.ts`), where the query's reading draws no
 * check for it. That result is drawn with its check instead, read from the issue's reading, once
 * and as a row like any other (`lifted`). Where a result the query kept is not the check's
 * latest, the check draws it under its latest and leaves its other earlier results folded behind
 * their line (`kept`): a query that keeps one of a check's 26 results draws that one, not the 25
 * others.
 */
function keptChecks(
  own: readonly CheckReading[],
  rows: readonly TreeNode[],
  held: readonly CheckReading[]
): DrawnChecks {
  const kept = new Map<string, Set<string>>();
  const keep = (check: string, result: string) => {
    const results = kept.get(check);
    if (results === undefined) kept.set(check, new Set([result]));
    else results.add(result);
  };
  for (const check of own) {
    const latest = (held.find((one) => one.node.id === check.node.id) ?? check).latest;
    for (const result of [check.latest, ...check.earlier]) {
      if (result !== undefined && result.id !== latest?.id) keep(check.node.id, result.id);
    }
  }
  const ownIds = new Set(own.map((check) => check.node.id));
  // The ids of the checks drawn: the part's own, and each check a kept result is lifted from.
  const drawn = new Set(ownIds);
  const lifted = new Set<string>();
  for (const row of rows) {
    const parent = row.type === "result" ? row.parentLine?.id : undefined;
    if (parent === undefined || ownIds.has(parent)) continue;
    const check = held.find((one) => one.node.id === parent);
    if (check === undefined) continue;
    drawn.add(parent);
    lifted.add(row.id);
    if (check.latest?.id !== row.id) keep(parent, row.id);
  }
  const heldIds = new Set(held.map((check) => check.node.id));
  return {
    checks: [
      ...held.filter((check) => drawn.has(check.node.id)),
      ...own.filter((check) => !heldIds.has(check.node.id)),
    ],
    kept,
    lifted,
  };
}

/** The ids of the rows the tree draws, in the order they stand on the page: the issue's row,
 *  each statement followed by its checks, each check that serves no statement, and each other
 *  row. The tree's tab stop is one of them, and each row's element carries its id as
 *  `data-verification-row`. */
function treeRows(
  issue: string,
  statements: readonly StatementReading[],
  loose: readonly CheckReading[],
  others: readonly TreeNode[]
): string[] {
  return [
    issue,
    ...statements.flatMap((statement) => [
      statement.node.id,
      ...statement.checks.map((check) => checkRowId(statement.node.id, check.node.id)),
    ]),
    ...loose.map((check) => check.node.id),
    ...others.map((node) => node.id),
  ];
}

/** The id of a check's row under a statement. A check that serves two statements is drawn
 *  under each, so its node's id names two rows, and the statement's id beside it names one. */
function checkRowId(statement: string, check: string): string {
  return `${statement} ${check}`;
}

/** What the body says of each read that failed before it first answered, in the outline's own
 *  words for that read (`STATE_LINES`): which checks serve each statement for the coverage read,
 *  the checks' results for the results read, and the Verification document for the blocks read,
 *  without which no check can be placed under the statement it serves. */
const UNREADABLE_LINES: Readonly<Record<VerificationRead, string>> = {
  blocks: STATE_LINES.verification_unreadable,
  coverage: STATE_LINES.coverage_unreadable,
  results: STATE_LINES.results_unreadable,
};

/** The seven states a check can be in, in the order the page declares them. */
const CHECK_STATES = Object.keys(NODE_STATE_WORDS.check) as CheckState[];
/** A column heading, in the prototype's `.cap`. */
const CAP = `text-xs leading-4 font-semibold tracking-[0.08em] uppercase ${pageQuietText}`;

/** The body's measure (the prototype's `.ver`): at 760 px and below the width of the element
 *  around it less 20 px, 10 px under what stands above it; from 761 px up 1180 px wide, or that
 *  width less 48 px where that is less, 22 px under it. At every width it stands in the middle of
 *  that element with 120 px under it. */
const BODY =
  "mx-auto mt-2.5 mb-[120px] flex w-full min-w-0 max-w-[calc(100%-20px)] flex-col min-[761px]:mt-[22px] min-[761px]:w-[1180px] min-[761px]:max-w-[calc(100%-48px)]";

/** The table (the prototype's `.vt`): from 761 px up one frame, 1 px round it with 10 px
 *  corners, its rows bands inside it; at 760 px and below no frame, and each row a card of its
 *  own 8 px from the next (`.vcards`). */
const TABLE = `flex min-w-0 flex-col gap-2 ${pageLineBorder} min-[761px]:gap-0 min-[761px]:overflow-hidden min-[761px]:rounded-[10px] min-[761px]:border min-[761px]:bg-page-paper`;

/** One row of the table: from 761 px up a band of the frame with a rule under it but the last;
 *  at 760 px and below a card of its own, 1 px round it with 8 px corners (`.vcard`). */
const ROW = `rounded-[8px] border ${pageLineBorder} ${pagePaperBg} min-[761px]:rounded-none min-[761px]:border-x-0 min-[761px]:border-t-0 min-[761px]:last:border-b-0`;

/** The statement column and the rest, as the prototype's table divides them from 761 px up
 *  (38% and the rest); at 760 px and below one column, where each statement is a card and its
 *  checks stand under its words. It leaves out `grid` itself, since the headings are drawn
 *  only from 761 px up. */
const STATEMENT_COLUMNS =
  "min-w-0 grid-cols-[minmax(0,1fr)] min-[761px]:grid-cols-[minmax(0,38%)_minmax(0,62%)]";

/** The check column and its result, inside the rest (36% and 26% of the whole) from 761 px up;
 *  at 760 px and below the result stands under the check's words. */
const CHECK_COLUMNS =
  "grid min-w-0 grid-cols-[minmax(0,1fr)] min-[761px]:grid-cols-[minmax(0,58%)_minmax(0,42%)]";

/** One cell of the table, in the prototype's `.vt td`. */
const CELL = "min-w-0 px-3 py-[11px]";

/** Where a result's facts are drawn: in its column, which from 761 px up is the table's
 *  narrowest, 26 per cent of it and some 130 px inside its cell at 761 px. There the facts'
 *  widest label ("negative control") took the room of a command's copy control and drew it past
 *  the card, so a label takes at most half the column and breaks there, still beside its value
 *  (`NodeFacts.tsx`'s two columns). */
const RESULT_FACTS = "mt-1 min-w-0 [&_dl]:grid-cols-[fit-content(50%)_minmax(0,1fr)]";

/** A check's small controls under its words and its latest result, its Facts control and the
 *  line of its earlier results: quiet words, underlined as a link of the page is. */
const QUIET_CONTROL = `dispatch-inline-link mt-1 text-xs leading-5 font-medium underline decoration-page-hair underline-offset-[3px] ${pageQuietText}`;

/** Every row of the tree, which it draws in the order `treeRows` names them, each carrying its
 *  id there as `data-verification-row`; a check's row, in its statement's group or of the
 *  table's own; and a statement's row. */
const TREE_ROW = "[data-node-row]";
const CHECK_ROW = "[data-verification-check]";
const STATEMENT_ROW = "[data-verification-card]";

/** The keys the tree's handler acts on, the outline's: `j`, `k`, the arrows, Home and End move,
 *  Enter edits and `o` keeps. Every other key is the page keymap's. */
const TREE_KEYS: ReadonlySet<string> = new Set([
  "j",
  "k",
  "ArrowDown",
  "ArrowUp",
  "Home",
  "End",
  "ArrowRight",
  "ArrowLeft",
  "Enter",
  "o",
]);

/** Where a key moves the focus from `row`, the `at`th of the tree's rows (`elements`): the next
 *  row or the one before, the first or the last, a statement's first check, or the statement a
 *  check is drawn under. Right and Left go nowhere from any other row, and no other key moves. */
function moveTarget(
  key: string,
  row: HTMLElement,
  elements: readonly HTMLElement[],
  at: number
): HTMLElement | null | undefined {
  switch (key) {
    case "j":
    case "ArrowDown":
      return elements[Math.min(at + 1, elements.length - 1)];
    case "k":
    case "ArrowUp":
      return elements[Math.max(at - 1, 0)];
    case "Home":
      return elements[0];
    case "End":
      return elements.at(-1);
    case "ArrowRight":
      return row.matches(STATEMENT_ROW) ? row.querySelector<HTMLElement>(CHECK_ROW) : undefined;
    case "ArrowLeft":
      return row.matches(CHECK_ROW)
        ? row.parentElement?.closest<HTMLElement>(STATEMENT_ROW)
        : undefined;
    default:
      return undefined;
  }
}

/** The colour each state is drawn in, which repeats what its word and its shape already say:
 *  met in the passed green, not met in the failed red, unknown quiet. */
const STATEMENT_TONES: Readonly<Record<StatementState, string>> = {
  met: pageStateText.passed,
  not_met: pageStateText.failed,
  unknown: pageStateText.quiet,
};

/** The fill each state takes in the summary's bar, which is decoration: the counts beside it
 *  say the same in words. */
const STATEMENT_FILLS: Readonly<Record<StatementState, string>> = {
  met: "bg-page-passed",
  not_met: "bg-page-failed",
  unknown: pageHairBg,
};

/** The pressed chip's mark under forced colours, where the system draws every chip's fill and
 *  border alike: an outline in the system's text colour, so the chip that is on is still told
 *  from the rest by more than its `aria-pressed`. */
const FORCED_PRESSED =
  "forced-colors:aria-pressed:outline-2 forced-colors:aria-pressed:outline-offset-2 forced-colors:aria-pressed:outline-[CanvasText]";

/** How a check's state is coloured: the passed green and the failed red the page already draws
 *  a check in, and the quiet grey for the five that are neither. */
function checkTone(state: CheckState): string {
  if (state === "passed") return pageStateText.passed;
  return state === "failed" ? pageStateText.failed : pageStateText.quiet;
}

/**
 * How the issue stands, in one card over the table (the prototype's `.vsum`): how many
 * statements carry a check, the bar of how they stand, the count in each state, the one commit
 * the page names for the issue's results, drawn here and nowhere else, and how many of the
 * issue's checks stand in each of the seven states a check can be in, each by its word and its
 * shape, so the shapes the table draws are named once over it. It is the issue's whatever a
 * words query left of it.
 */
function Summary({
  counts,
  reading,
}: {
  counts: Readonly<Record<StatementState, number>>;
  /** The issue's reading, the view with no query: its commit, its covered count, its statements,
   *  which the covered count is out of, and its checks, each counted once however many
   *  statements it serves. */
  reading: VerificationReading;
}): ReactNode {
  const { commit, covered } = reading;
  const total = reading.statements.length;
  // How many of the issue's checks stand in each state, counted once for each reading rather
  // than once for each state the legend draws.
  const inState = useMemo(() => {
    const byState = new Map<string, CheckState | undefined>();
    for (const check of [
      ...reading.statements.flatMap((statement) => statement.checks),
      ...reading.loose,
    ]) {
      byState.set(check.node.id, check.state);
    }
    const counted = new Map<CheckState | undefined, number>();
    for (const held of byState.values()) counted.set(held, (counted.get(held) ?? 0) + 1);
    return counted;
  }, [reading]);
  return (
    <div
      className={`flex min-w-0 flex-wrap items-center gap-[18px] rounded-[10px] border px-[18px] py-3.5 ${pageLineBorder} ${pagePaperBg}`}
      data-verification-summary=""
    >
      <p className={`text-sm leading-5 font-semibold ${pageInkText}`} data-verification-covered="">
        <span {...authorText}>{covered}</span> <span {...pageChrome}>{VERIFICATION_WORDS.of}</span>{" "}
        <span {...authorText}>{total}</span>{" "}
        <span {...pageChrome}>{VERIFICATION_WORDS.covered}</span>
      </p>
      <span
        aria-hidden="true"
        className={`flex h-3 min-w-[220px] flex-1 overflow-hidden rounded-[6px] ${pageGuideBg}`}
        data-verification-bar=""
      >
        {STATEMENT_STATES.filter((state) => counts[state] > 0).map((state) => (
          <i
            className={`block h-full ${STATEMENT_FILLS[state]}`}
            key={state}
            style={{ width: `${(counts[state] / Math.max(total, 1)) * 100}%` }}
          />
        ))}
      </span>
      <p
        className={`flex min-w-0 flex-wrap gap-[14px] text-sm leading-5 ${pageMutedText}`}
        data-verification-legend=""
      >
        {STATEMENT_STATES.map((state) => (
          <span className="inline-flex items-center gap-[5px]" key={state}>
            <NodeGlyph
              className={`size-3 shrink-0 ${STATEMENT_TONES[state]}`}
              token={GLYPH_TOKENS.statements[state]}
            />
            <span className={pageInkText} {...authorText}>
              {counts[state]}
            </span>
            <span {...pageChrome}>{VERIFICATION_WORDS.states[state]}</span>
          </span>
        ))}
      </p>
      <p
        className={`flex min-w-0 basis-full flex-wrap gap-x-[14px] gap-y-1 text-xs leading-5 ${pageMutedText}`}
        data-verification-check-legend=""
      >
        {CHECK_STATES.map((state) => (
          <span
            className="inline-flex items-center gap-[5px]"
            data-check-legend-state={state}
            key={state}
          >
            <NodeGlyph
              className={`size-3 shrink-0 ${checkTone(state)}`}
              token={GLYPH_TOKENS.states.check[state]}
            />
            <span className={pageInkText} {...authorText}>
              {inState.get(state) ?? 0}
            </span>
            <span {...pageChrome}>{NODE_STATE_WORDS.check[state]}</span>
          </span>
        ))}
      </p>
      {commit === undefined ? null : (
        <p className={`min-w-0 text-sm leading-5 ${pageMutedText}`} data-verification-commit="">
          <span {...pageChrome}>{VERIFICATION_WORDS.commit}</span>{" "}
          <code className={`font-mono break-all ${pageInkText}`} {...authorText}>
            {shortCommit(commit)}
          </code>
        </p>
      )}
    </div>
  );
}

/** The chips over the table (the prototype's `.vchips`): All, and one per state a statement can
 *  be in with how many of the issue's stand in it. Pressing one draws those statements alone,
 *  pressing it again or pressing All draws them all, as the filter row's own controls work.
 *  Every chip is a press the keyboard reaches, one whose state holds no statement too, which
 *  draws the page's line for it. */
function Chips({
  counts,
  onlyState,
  onPress,
  total,
}: {
  counts: Readonly<Record<StatementState, number>>;
  onlyState: StatementState | undefined;
  onPress: (state: StatementState | undefined) => void;
  total: number;
}): ReactNode {
  const chip = (on: boolean): string =>
    `rounded-full border px-3 py-0.5 text-sm leading-5 font-semibold ${focusVisibleRing} ${FORCED_PRESSED} ${
      on ? `${pageInkFill} ${pageInkBorder}` : `${pageHairBorder} ${pagePaperBg} ${pageMutedText}`
    }`;
  return (
    <div className="my-[14px] flex min-w-0 flex-wrap gap-1.5" data-verification-chips="">
      <button
        aria-pressed={onlyState === undefined}
        className={chip(onlyState === undefined)}
        data-verification-chip="all"
        onClick={() => onPress(undefined)}
        type="button"
        {...pageChrome}
      >
        {VERIFICATION_WORDS.all}{" "}
        <span className="text-xs font-medium" {...authorText}>
          {total}
        </span>
      </button>
      {STATEMENT_STATES.map((state) => (
        <button
          aria-pressed={onlyState === state}
          className={chip(onlyState === state)}
          data-verification-chip={state}
          key={state}
          onClick={() => onPress(onlyState === state ? undefined : state)}
          type="button"
          {...pageChrome}
        >
          {VERIFICATION_WORDS.states[state]}{" "}
          <span className="text-xs font-medium" {...authorText}>
            {counts[state]}
          </span>
        </button>
      ))}
    </div>
  );
}

/** The three column headings, in the prototype's `.vt th`, drawn from 761 px up only: at 760 px
 *  and below the columns stack and each statement's card carries its own words. They name the
 *  columns and are no row of the tree. */
function Headings(): ReactNode {
  return (
    <div
      className={`hidden border-b ${STATEMENT_COLUMNS} ${pageLineBorder} ${pagePaneBg} min-[761px]:grid`}
      data-verification-head=""
      role="none"
    >
      <span className={`${CELL} ${CAP}`} {...pageChrome}>
        {sentenceCase(NODE_TYPE_NAMES.requirement)}
      </span>
      <span className={CHECK_COLUMNS}>
        <span className={`${CELL} ${CAP}`} {...pageChrome}>
          {VERIFICATION_WORDS.checks}
        </span>
        <span className={`${CELL} ${CAP}`} {...pageChrome}>
          {VERIFICATION_WORDS.latest_result}
        </span>
      </span>
    </div>
  );
}

/**
 * One statement with the checks that serve it (the prototype's `.vt tr`, and its `.vcard` at
 * 760 px and below): how it stands at the commit the page names, its words, drawn whole, the
 * node its words came from as one line, and its gap where no check serves it.
 *
 * It is a row of the tree, and its checks are one group of rows under it, drawn open; a
 * statement no check of this issue serves holds no group and so says nothing of being open. A
 * pointer's press on its words cell puts the caret in its words where the row takes one, as
 * Enter on its row does, and the row draws its document's one editor on its stage in place of
 * the words while it is the row edited; a press on a control in the cell - a link in its words,
 * its parent line's link, a drawn diagram's summary - is that control's own, and a click that
 * ends a selection made in the words asks for nothing (`pressAsksCaret`). The links are tab
 * stops, as every control of the view is. What a key reported at the row, where Enter put no
 * caret, is drawn at the end of the statement's own cell, before the rows of its checks, which
 * say their own. As a card its left edge is the
 * statement's teal, or the failed red where it reads Not met or no check serves it, so the card
 * says so before its words are read. How it stands is the issue's reading of it, whatever a
 * words query left of its checks.
 *
 * Where the coverage read counts more checks for it than the view with no query draws under it,
 * the rest serve it from issues under this one, and it says how many in one line after its
 * checks, inside its card. The line is no row of the tree, since the view draws none of those
 * checks, and it is drawn only once the reads have answered, as every row is.
 */
function Statement({
  context,
  editingFor,
  onEdit,
  onPress,
  onPressWords,
  presses,
  reportFor,
  statement,
  statementNames,
  tabIndexFor,
}: {
  context: boolean;
  /** The document the row with this id is being edited in, while it is the row edited (the
   *  body's `editingFor`). */
  editingFor: (id: string) => string | undefined;
  /** Asks for the caret in the node of the row with this id by Enter, as on a check's words (the
   *  body's `editRow`). */
  onEdit: (row: string, node: TreeNode) => void;
  /** Sets the press of the check row with this id (the body's `press`). */
  onPress: (row: string, pressed: boolean | undefined) => void;
  /** Asks for the caret in the node of the row with this id by a pointer's press on its words,
   *  where the row takes one (the body's `pressWords`). */
  onPressWords: (row: string, node: TreeNode) => void;
  /** Each drawn check's press, by its row's id. */
  presses: ReadonlyMap<string, boolean>;
  /** What a key reported at the row with this id, where it put no caret (the body's
   *  `reportFor`). */
  reportFor: (id: string) => string | undefined;
  statement: DrawnStatement;
  /** Each statement of the issue by its address, named by its first words (`AlsoUnder`). */
  statementNames: ReadonlyMap<string, string>;
  /** The tab index of the row with this id, which is 0 for the tree's one tab stop. */
  tabIndexFor: (id: string) => 0 | -1;
}): ReactNode {
  const node = statement.node;
  const failing = statement.state === "not_met" || node.gaps.includes("no_check");
  const editing = editingFor(node.id);
  return (
    <div
      aria-expanded={statement.checks.length === 0 ? undefined : true}
      aria-level={1}
      className={`grid ${STATEMENT_COLUMNS} ${ROW} border-l-4 ${failing ? "border-l-page-failed" : "border-l-page-statement"}`}
      data-node-context={context ? "" : undefined}
      data-node-id={node.id}
      data-node-row=""
      data-node-type={node.type}
      data-row-key={node.id}
      data-statement-state={statement.state}
      data-verification-card=""
      data-verification-row={node.id}
      role="treeitem"
      tabIndex={tabIndexFor(node.id)}
    >
      {/* biome-ignore lint/a11y/noStaticElementInteractions lint/a11y/useKeyWithClickEvents: a
          pointer's way to what Enter on the statement's row does; the cell is no control of its
          own. */}
      <div
        className={`flex min-w-0 flex-col ${CELL}`}
        onClick={(event) => {
          if (pressAsksCaret(event, editing)) onPressWords(node.id, node);
        }}
      >
        <p
          className={`mb-0.5 flex items-center gap-1.5 text-xs leading-5 font-semibold ${STATEMENT_TONES[statement.state]}`}
        >
          <NodeGlyph
            className="size-3.5 shrink-0"
            token={GLYPH_TOKENS.statements[statement.state]}
          />
          <span data-statement-state={statement.state} {...pageChrome}>
            {VERIFICATION_WORDS.states[statement.state]}
          </span>
        </p>
        <div
          className={`relative min-w-0 text-[15px] leading-6 ${pageWordsText}`}
          data-node-words=""
        >
          <RowWords editing={editing} node={node} />
        </div>
        {node.parentLine === undefined ? null : (
          <NodeParentLine line={node.parentLine} spaced tabStops />
        )}
        <Gaps node={node} />
        <RowReport className="mt-1" line={reportFor(node.id)} />
      </div>
      <div className="flex min-w-0 flex-col">
        {statement.checks.length === 0 ? null : (
          // biome-ignore lint/a11y/useSemanticElements: a treeitem owns its child rows through a group, and a fieldset groups form fields.
          <div className="flex min-w-0 flex-col" role="group">
            {statement.checks.map((check) => {
              const row = checkRowId(node.id, check.node.id);
              // Keyed by the check alone, as a check that serves no statement is: a new result
              // recorded or withdrawn while the reader is in the view draws the same row again,
              // so the focus on its words, a link in them or its facts stays where it is, and
              // so do the earlier results the reader opened. The row's id tells this placement
              // of the check from its others, for presses and for the row edited.
              return (
                <Check
                  check={check}
                  editing={editingFor(row)}
                  kept={statement.kept.get(check.node.id) ?? NO_KEPT_RESULTS}
                  key={check.node.id}
                  loose={false}
                  onEdit={() => onEdit(row, check.node)}
                  onPress={onPress}
                  onPressWords={() => onPressWords(row, check.node)}
                  pressed={presses.get(row)}
                  report={reportFor(row)}
                  row={row}
                  statementNames={statementNames}
                  tabIndex={tabIndexFor(row)}
                />
              );
            })}
          </div>
        )}
        {statement.elsewhere > 0 ? (
          <p
            className={`${CELL} text-xs leading-5 break-words ${pageQuietText} ${
              statement.checks.length === 0 ? "" : `border-t ${pageLineBorder}`
            }`}
            data-verification-elsewhere=""
            {...pageChrome}
          >
            <span {...authorText}>{statement.elsewhere.toLocaleString()}</span>{" "}
            {statement.elsewhere === 1
              ? VERIFICATION_WORDS.elsewhere_one
              : VERIFICATION_WORDS.elsewhere_many}
          </p>
        ) : null}
      </div>
    </div>
  );
}

/** The press under way anywhere on the page, followed once for every check row the body draws
 *  (`trackPress`, from the `mousedown` whose default moves the focus): whether one is held, from
 *  its start until the task after it ends, and `afterPress`, which has `decide` called once the
 *  press under way has ended (`Check`). A row drawn with no body around it sees no press. */
interface ViewPress {
  readonly afterPress: (decide: () => void) => void;
  readonly pressing: boolean;
}

const ViewPressContext = createContext<ViewPress>({ afterPress: () => {}, pressing: false });

/** The body's `ViewPress`: one `trackPress` for the whole view, whose end calls each `decide`
 *  queued while it was held, once, and forgets them. */
function useViewPress(): ViewPress {
  const press = useRef<Press | undefined>(undefined);
  const queued = useRef<(() => void)[]>([]);
  useEffect(() => {
    const listening = new AbortController();
    press.current = trackPress({
      onEnd: () => {
        const decides = queued.current;
        queued.current = [];
        for (const decide of decides) decide();
      },
      signal: listening.signal,
      starts: ["mousedown"],
    });
    return () => listening.abort();
  }, []);
  return useMemo(
    () => ({
      afterPress: (decide: () => void) => {
        queued.current.push(decide);
      },
      get pressing() {
        return press.current?.pressing === true;
      },
    }),
    []
  );
}

/**
 * One check beside its latest result (the prototype's `.vc` and `.vres`): how it stands, by a
 * word and a shape, its own words, and the four attributes its body draws. Its earlier results
 * are one line with their count, which opens them in place, since the approved picture draws one
 * result for each check, and the result the check's state stands on is marked wherever it is
 * drawn. It is the issue's reading of the check under a words query too (`keptChecks`), so its
 * state, its latest result and the count on its line are those the view with no query draws. An
 * earlier result the query kept is drawn under the latest with the line still shut, and no
 * other earlier result is, so the view draws no result the reader neither kept by the query nor
 * opened; the line opens them all, each drawn once.
 *
 * The facts of the check and of its results are drawn while the reader's focus is anywhere in
 * the row - its words, the editor drawn in their place, a link in them, its Facts control, its
 * facts, its results, the line of earlier results or the row itself - and while the reader keeps
 * them open, which is the rule the outline draws a focused node's body by. Two acts alone keep
 * them drawn or shut them: `o` on the row, and the check's own Facts control, a button in its
 * words' cell before the facts it opens, whose press opens them where they are shut and shuts
 * them where they are open, the focus inside or not, and which says which it does as a
 * disclosure button does (`aria-expanded`, the row's own value). The words are a tab stop with no
 * role: their links are tab stops too, and no control holds another. Enter on them, and a
 * pointer's press on them where the row takes a caret, puts the caret in the check's own text,
 * which the row edits in place, drawing its document's one editor on its stage in place of the
 * words while it is the row edited; a press on a control in the words - a link, a drawn diagram's
 * summary - or on a button of the row is that control's own, and a click that ends a selection
 * made in the words asks for nothing (`pressAsksCaret`); a press on the latest result's cell only
 * gives the row the focus. So a reader reaches
 * every fact of a check and of its results by the keyboard alone, and the table stays the table
 * the picture draws.
 *
 * The row hears where the focus is by the browser's own focus events on its element (`focusin`,
 * `focusout`), not by React's: the editor is drawn into the words through a portal, and React
 * sends a portal's focus events up the portal's own parents, not up the elements it stands in, so
 * a React handler on the row never hears the focus leave the editor, for another row or for
 * anything out of the view.
 *
 * It is a row of the tree, across its words and its latest result: a row of the group under
 * its statement, or, for a check that serves no statement, a row of the table of its own
 * (`loose`). From 761 px up such a row stands in the statement grid with its first cell empty,
 * so its words stand under "Checks" and its result under "Latest result", as a statement's
 * checks do; at 760 px and below it is a card of its own. The row says whether its facts are
 * drawn (`aria-expanded`), which is what `o` reads (the body's `keepChecks`); Enter on the row
 * itself puts the caret in the check's text as Enter on its words does, and where a key put none
 * the row says why at its end (`report`).
 */
function Check({
  check,
  editing,
  kept,
  loose,
  onEdit,
  onPress,
  onPressWords,
  pressed,
  report,
  row,
  statementNames,
  tabIndex,
}: {
  check: CheckReading;
  /** The document the check's text is being edited in, while this is the row edited. */
  editing: string | undefined;
  /** The ids of its earlier results a words query kept, which it draws under its latest result
   *  while its line of earlier results is shut (`keptChecks`); empty with no query, and for a
   *  check the query kept no earlier result of. */
  kept: ReadonlySet<string>;
  /** Whether the check serves no statement, and so stands as a row of the table itself. */
  loose: boolean;
  /** Asks for the caret in the check's text by Enter on its words (the body's `editRow`). */
  onEdit: () => void;
  /** Sets this row's press (the body's `press`). */
  onPress: (row: string, pressed: boolean | undefined) => void;
  /** Asks for the caret in the check's text by a pointer's press on its words, where the row
   *  takes one (the body's `pressWords`). */
  onPressWords: () => void;
  /** The reader's own press: open, shut, or neither, in which case the focus decides. */
  pressed: boolean | undefined;
  /** What a key reported at the row, where it put no caret. */
  report: string | undefined;
  /** The row's id in the tree: a check drawn under two statements is two rows. */
  row: string;
  /** Each statement of the issue by its address, named by its first words (`AlsoUnder`). */
  statementNames: ReadonlyMap<string, string>;
  /** 0 where this row holds the tree's one tab stop, and -1 everywhere else. */
  tabIndex: 0 | -1;
}): ReactNode {
  const [focusInside, setFocusInside] = useState(false);
  const [earlierShown, setEarlierShown] = useState(false);
  // Whether the facts were drawn when the pointer went down: the focus its press puts in the row
  // draws them before the click, and the click must shut only what was drawn before it.
  const drawnAtPress = useRef<boolean | undefined>(undefined);
  // The row's element, whose focus events say whether the focus is inside it, and the press, its
  // setter and the editing as last drawn, which its listener reads as the focus leaves.
  const viewPress = useContext(ViewPressContext);
  const rowElement = useRef<HTMLDivElement>(null);
  const pressedNow = useRef(pressed);
  pressedNow.current = pressed;
  const onPressNow = useRef(onPress);
  onPressNow.current = onPress;
  const editingNow = useRef(editing);
  editingNow.current = editing;
  const leave = useCallback(() => {
    setFocusInside(false);
    // A shut the reader pressed lasts while the focus stays; the next visit opens again.
    if (pressedNow.current === false) onPressNow.current(row, undefined);
  }, [row]);
  // A `focusout` that names an element outside the row is the focus leaving it. One that names
  // none is told by what dropped the focus. A press under way is the reader's act - one on blank
  // space drops the focus to the page's body - and the press ending decides whether the focus is
  // in the row then, edited or not. With no press, while the row is edited, the drop is the
  // editing's own: Escape in the editor blurs it before it focuses the row, and the editor's
  // redraw drops the focus to the page and puts it back on the block a microtask or a frame later
  // (`useNodeEditing.ts`), neither of which leaves the row, so the facts the reader shut stay
  // shut; the editing ending decides once whether the focus is in the row then (the effect
  // below). With no press on a row not edited, the drop is decided a microtask later, by whether
  // the focus is in the row then, as the body's box decides whether it holds the focus
  // (`slots/body.tsx`'s `useFocusHeld`). Nothing is decided after the effect's cleanup - the row
  // gone, or its id changed.
  useLayoutEffect(() => {
    const element = rowElement.current;
    if (element === null) return;
    const listening = new AbortController();
    // Leaves the row where the focus has settled outside it, and does nothing after the cleanup.
    const decide = () => {
      if (!listening.signal.aborted && !element.contains(document.activeElement)) leave();
    };
    element.addEventListener("focusin", () => setFocusInside(true), { signal: listening.signal });
    element.addEventListener(
      "focusout",
      (event) => {
        const to = event.relatedTarget;
        if (to instanceof Node) {
          if (!element.contains(to)) leave();
          return;
        }
        if (viewPress.pressing) viewPress.afterPress(decide);
        else if (editingNow.current === undefined) queueMicrotask(decide);
      },
      { signal: listening.signal }
    );
    return () => listening.abort();
  }, [leave, viewPress]);
  const editedBefore = useRef(editing);
  useLayoutEffect(() => {
    const ended = editedBefore.current !== undefined && editing === undefined;
    editedBefore.current = editing;
    const element = rowElement.current;
    if (ended && element !== null && !element.contains(document.activeElement)) leave();
  }, [editing, leave]);
  const node = check.node;
  const surface = node.facts.find((fact) => fact.name === "surface")?.value ?? "";
  const opened = pressed ?? focusInside;
  // The Facts control's press: the facts opened where they are shut and shut where they are
  // open, by what was drawn as the pointer went down (`markPress`), and on the keyboard, which
  // marks nothing, by what is drawn now.
  const pressFacts = () => {
    const drawn = drawnAtPress.current ?? opened;
    drawnAtPress.current = undefined;
    onPress(row, !drawn);
  };
  const markPress = () => {
    drawnAtPress.current = opened;
  };
  // The earlier results it draws, in their order: all of them where the reader opened their
  // line, else only those a words query kept (`kept`), with the line still shut, so a query that
  // keeps one of 26 results draws that one and not the 25 others. Opened, the line draws each
  // once, the kept ones among them, and shut again it leaves the kept ones drawn.
  const earlierDrawn = earlierShown
    ? check.earlier
    : kept.size === 0
      ? undefined
      : check.earlier.filter((result) => kept.has(result.id));
  // Its words' cell and its result's cell, side by side from 761 px up and one over the other at
  // 760 px and below.
  const cells = (
    <>
      <div className={CELL}>
        {/* biome-ignore lint/a11y/noStaticElementInteractions: the words hold links that are tab
            stops of their own, so no role may make them a control that holds controls; a press
            on them puts the caret in them, as Enter on them does. */}
        <div
          className={`flex min-w-0 items-baseline gap-2 text-sm leading-5 ${pageWordsText} ${focusVisibleRing}`}
          data-verification-check-words=""
          onClick={(event) => {
            if (pressAsksCaret(event, editing)) onPressWords();
          }}
          onKeyDown={(event: KeyboardEvent<HTMLElement>) => {
            // A key on a link in the words, or typed into the editor drawn in their place, is
            // that one's own.
            if (event.target !== event.currentTarget) return;
            if (event.key !== "Enter") return;
            if (event.altKey || event.ctrlKey || event.metaKey) return;
            event.preventDefault();
            onEdit();
          }}
          // biome-ignore lint/a11y/noNoninteractiveTabindex: a check's words are a tab stop of the view.
          tabIndex={0}
        >
          <span
            aria-hidden="true"
            className={`mt-1.5 block size-[7px] shrink-0 rounded-full ${pageKindBullet.check}`}
          />
          <div className="min-w-0 break-words" data-node-words="">
            <RowWords editing={editing} node={node} />
          </div>
        </div>
        {surface === "" ? null : (
          <p
            className={`mt-0.5 text-xs leading-[18px] font-semibold ${pageKindTag.check}`}
            data-check-surface=""
            {...authorText}
          >
            {surface}
          </p>
        )}
        <AlsoUnder chips={node.alsoUnder} names={statementNames} />
        <Gaps node={node} />
        <button
          aria-expanded={opened}
          className={QUIET_CONTROL}
          data-verification-facts=""
          onClick={pressFacts}
          onMouseDown={markPress}
          type="button"
          {...pageChrome}
        >
          {VERIFICATION_WORDS.facts}
        </button>
        {opened ? (
          <div className="mt-1 min-w-0" data-node-body="">
            <NodeFacts facts={node.facts} tabStops />
          </div>
        ) : null}
      </div>
      <div className={CELL}>
        {check.state === undefined ? null : (
          <p
            className={`flex items-center gap-1.5 text-xs leading-5 font-semibold ${checkTone(check.state)}`}
          >
            <NodeGlyph
              className="size-3.5 shrink-0"
              token={GLYPH_TOKENS.states.check[check.state]}
            />
            <span data-node-state={check.state} {...pageChrome}>
              {NODE_STATE_WORDS.check[check.state]}
            </span>
          </p>
        )}
        {check.latest === undefined ? null : (
          <LatestResult
            opened={opened}
            result={check.latest}
            stands={check.standsOn === check.latest.id}
          />
        )}
        {check.earlier.length === 0 ? null : (
          // The press leaves the focus where it is, as a cut text's "more" control's does:
          // the focus a pointer's press puts in the row draws the latest result's facts above
          // this line, which moves it from under the pointer before the press ends, and the
          // click then lands on what moved there and not on this line.
          <button
            aria-expanded={earlierShown}
            className={QUIET_CONTROL}
            data-verification-earlier=""
            onClick={() => setEarlierShown(!earlierShown)}
            onMouseDown={(event) => event.preventDefault()}
            type="button"
            {...pageChrome}
          >
            <span {...authorText}>{check.earlier.length.toLocaleString()}</span>{" "}
            {check.earlier.length === 1
              ? VERIFICATION_WORDS.earlier_one
              : VERIFICATION_WORDS.earlier_many}
          </button>
        )}
        {earlierDrawn?.map((result) => (
          <LatestResult
            key={result.id}
            opened={opened}
            result={result}
            stands={check.standsOn === result.id}
          />
        ))}
      </div>
      <RowReport className="col-span-full px-3 pb-[11px]" line={report} />
    </>
  );
  return (
    <div
      aria-expanded={opened}
      aria-level={loose ? 1 : 2}
      className={
        loose
          ? `grid ${STATEMENT_COLUMNS} ${ROW}`
          : `${CHECK_COLUMNS} border-t first:border-t-0 ${pageLineBorder}`
      }
      data-node-id={node.id}
      data-node-row=""
      data-node-state={check.state}
      data-node-type={node.type}
      data-row-key={row}
      data-verification-check=""
      data-verification-loose={loose ? "" : undefined}
      data-verification-row={row}
      ref={rowElement}
      role="treeitem"
      tabIndex={tabIndex}
    >
      {loose ? (
        // The statement column's cell is left empty, so the check's two cells stand in the
        // columns the headings name.
        <div className={`${CHECK_COLUMNS} min-[761px]:col-start-2`}>{cells}</div>
      ) : (
        cells
      )}
    </div>
  );
}

/** One result of a check: who recorded it and when, what it observed, whether the check's state
 *  stands on it, and what it holds that no line has room for, in the body the check's row opens.
 *  What it observed is drawn as a node's own text is, through `RowText`, from the results read's
 *  reading of it (`observed_tree`) where the read sent one, so no stored text of the view is
 *  drawn as raw characters or parsed by the page. No commit is drawn for it, on its line or in
 *  its facts: the page names one commit for the issue's results, once, in the summary, and a
 *  result recorded against another says so in its check's word, "Out of date". So the facts it
 *  opens leave out the `commit` fact `check-facts.ts`'s `resultFacts` gives every result. */
function LatestResult({
  opened,
  result,
  stands,
}: {
  opened: boolean;
  result: TreeNode;
  stands: boolean;
}): ReactNode {
  return (
    <div
      className="mt-1 min-w-0"
      data-node-id={result.id}
      data-node-type={result.type}
      data-result-stands={stands ? "" : undefined}
      data-verification-result=""
    >
      <p className={`min-w-0 text-xs leading-[18px] [overflow-wrap:anywhere] ${pageQuietText}`}>
        {stands ? (
          <>
            <span className={`font-semibold ${pageInkText}`} {...pageChrome}>
              {VERIFICATION_WORDS.stands}
            </span>
            <span {...pageChrome}> · </span>
          </>
        ) : null}
        <Author actor={result.author} />
        {result.at === undefined ? null : (
          <>
            <span {...pageChrome}> · </span>
            <span className="whitespace-nowrap" data-node-time="" {...authorText}>
              {shortDay(result.at)}
            </span>
          </>
        )}
      </p>
      {result.text === "" ? null : (
        <div
          className={`mt-0.5 min-w-0 text-sm leading-5 break-words ${pageWordsText}`}
          data-node-words=""
        >
          <RowText markdown={result.text} tabStops tree={result.tree} />
        </div>
      )}
      {opened ? (
        <div className={RESULT_FACTS} data-node-body="">
          <NodeFacts facts={result.facts.filter((fact) => fact.name !== "commit")} tabStops />
        </div>
      ) : null}
    </div>
  );
}

/** Who made something, by a name and a shape, as every other row of the page draws an author: a
 *  person by their login, an agent by its role's word as the outline's rows name one
 *  (`agent-role.ts`), never an identifier, and the page's own words where the model holds none.
 *  An agent's whole title is drawn nowhere in the view, which opens no body that would give it. */
function Author({ actor }: { actor: Actor | undefined }): ReactNode {
  const agentText = useAgentText();
  const kind = authorKind(actor);
  const name = agentText.author(actor);
  return (
    <span className="inline-flex min-w-0 items-center gap-1" data-node-author="">
      <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors[kind]} />
      <span className="min-w-0 break-words" {...(actor === undefined ? pageChrome : authorText)}>
        {actor === undefined
          ? CONTROL_LABELS.unknown_author
          : kind === "agent"
            ? agentRoleWord(name)
            : name}
      </span>
    </span>
  );
}

/** The other statements a check is drawn under, each named as every chip of the page names a
 *  block (`chipName`): a check serves several statements and this body draws it under each. A
 *  target is named from the statement the view already holds for it (`names`, by its address),
 *  by the line the names store projects from the reading the page published for it - the first
 *  block of its words that holds any, a reference inside it read as its short form, since a name
 *  inside a name resolves nothing - cut to a chip's room. So no mark the author typed is drawn
 *  and a statement whose words open on "1." keeps it. A target the view holds no statement for
 *  and one the store holds no line for draw nothing, a comma stands only between targets that
 *  are drawn, and the line is drawn only where one is. The reading list's `node-row-chip`
 *  (`DISPATCH_READING_SURFACES`) says why a chip's words are drawn so. */
function AlsoUnder({
  chips,
  names,
}: {
  chips: readonly NodeChip[];
  /** Each statement of the issue by its address, named by its first words. */
  names: ReadonlyMap<string, string>;
}): ReactNode {
  const named = chips.flatMap((chip) => {
    const words = names.get(chip.ref) ?? "";
    return words === "" ? [] : [{ ref: chip.ref, words }];
  });
  if (named.length === 0) return null;
  return (
    <p
      className={`mt-0.5 min-w-0 text-xs leading-[18px] [overflow-wrap:anywhere] ${pageQuietText}`}
      data-node-trail=""
    >
      <span {...pageChrome}>{CONTROL_LABELS.also_under} </span>
      {named.map((chip, place) => (
        <span data-node-chip="" key={chip.ref}>
          {place === 0 ? null : <span {...pageChrome}>, </span>}
          <span {...authorText}>{chip.words}</span>
        </span>
      ))}
    </p>
  );
}

/** Each gap the node carries, said in full where the picture draws "No check proves this": a
 *  statement no check serves, and a check whose `serves` names no statement. */
function Gaps({ node }: { node: TreeNode }): ReactNode {
  if (node.gaps.length === 0) return null;
  return (
    <>
      {node.gaps.map((gap) => (
        <p
          className={`mt-1 min-w-0 text-xs leading-[18px] font-semibold ${pageFailedText}`}
          data-gap=""
          data-gap-kind={gap}
          key={gap}
          {...pageChrome}
        >
          {GAP_LINES[gap]}
        </p>
      ))}
    </>
  );
}

/** A row the filter kept that is neither a statement nor a check: a node an address named,
 *  which outranks the filter. It is drawn by its kind and its words, so nothing the filter drew
 *  is lost. It is a row of the tree and of the table, as a check that serves no statement is.
 *  Enter on it asks for the caret in its node's text, as on every row of the table, and the row
 *  says why where none goes in; a pointer's press on its words asks for it too where the row
 *  takes a caret (`takesCaret`), as on a statement's words or a check's, and else only gives the
 *  row the focus.
 *  The links in its words are tab stops, as every control of the view is: the tree's keys move
 *  between rows and reach no link inside one. A comment or a message its author corrected
 *  carries its mark as the outline's row does (INFRA-1523, R2 and R3): a deleted one draws the
 *  deleted line in place of its words (`DeletedLine`), from the delete and never from its text,
 *  which names the actor as no registry knows it and the time as stored; an edited one says
 *  ` · edited` after its kind (`EditedMark`), outside its words, as the outline's row says it in
 *  its margin. */
function OtherRow({
  editing,
  node,
  onPressWords,
  report,
  tabIndex,
}: {
  /** The document its node's text is being edited in, while this is the row edited. */
  editing: string | undefined;
  node: TreeNode;
  /** Asks for the caret in its node's text by a pointer's press on its words, where the row takes
   *  one (the body's `pressWords`). */
  onPressWords: () => void;
  /** What a key reported at the row, where it put no caret. */
  report: string | undefined;
  tabIndex: 0 | -1;
}): ReactNode {
  const deleted = postDeletion({ deleted_at: node.deletedAt, deleted_by: node.deletedBy });
  return (
    <div
      aria-level={1}
      className={`${CELL} ${ROW}`}
      data-node-id={node.id}
      data-node-row=""
      data-node-type={node.type}
      data-row-key={node.id}
      data-verification-other=""
      data-verification-row={node.id}
      role="treeitem"
      tabIndex={tabIndex}
    >
      <p className={`text-xs leading-5 ${pageQuietText}`}>
        <span {...pageChrome}>{NODE_TYPE_NAMES[node.type]}</span>
        <EditedMark at={node.editedAt} marks={NODE_EDITED_MARKS} />
      </p>
      {/* biome-ignore lint/a11y/noStaticElementInteractions lint/a11y/useKeyWithClickEvents: a
          pointer's way to what Enter on the row does; the words are no control of their own. */}
      <div
        className={`min-w-0 text-[15px] leading-6 ${pageWordsText}`}
        data-node-words=""
        onClick={(event) => {
          if (pressAsksCaret(event, editing)) onPressWords();
        }}
      >
        {deleted === null ? (
          <RowWords editing={editing} node={node} />
        ) : (
          <p className="break-words" {...authorText}>
            <DeletedLine deleted={deleted} />
          </p>
        )}
      </div>
      {node.parentLine === undefined ? null : (
        <NodeParentLine line={node.parentLine} spaced tabStops />
      )}
      <RowReport className="mt-1" line={report} />
    </div>
  );
}

/** A row's words, or while the row is the one edited (`editing`, the document its node's text
 *  is being edited in) the stage that document's one editor is drawn on in their place
 *  (`useDocumentStage`), as a statement, a check and another row of the table each draw them. */
function RowWords({ editing, node }: { editing: string | undefined; node: TreeNode }): ReactNode {
  const stage = useDocumentStage(editing);
  return editing === undefined ? (
    <RowText markdown={node.text} source={node.source} tabStops tree={node.tree} />
  ) : (
    <div className="flex min-w-0 flex-col" ref={stage} {...authorText} />
  );
}

/** What a key pressed on a row reported where it put no caret (`useNodeEditing`'s `onReport`),
 *  in the outline's own words for it (`KEY_REPORTS`), drawn at the end of the row's own content
 *  as an outline's row draws one (`NodeRow`'s `report`): a status line, so it is said once drawn.
 *  Nothing is drawn where the row holds no report. */
function RowReport({
  className,
  line,
}: {
  className: string;
  line: string | undefined;
}): ReactNode {
  if (line === undefined) return null;
  return (
    <p
      className={`${className} text-xs leading-5 ${pageMutedText}`}
      data-row-report=""
      role="status"
      {...pageChrome}
    >
      {line}
    </p>
  );
}
