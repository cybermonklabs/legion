import { hashKey, type QueryKey, useQuery } from "@tanstack/react-query";
import {
  Fragment,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useNavigate } from "react-router-dom";

import { primarySpec, roleDocument } from "../../../api/issue-cache";
import { issueAskThreadsQuery, issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import type { Actor, IssueDetails } from "../../../api/types";
import {
  pageDecisionText,
  pageFocusedRow,
  pageHairBg,
  pageHairBorder,
  pageInkText,
  pageKindBullet,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  pageStateText,
  pageWordsText,
} from "../../../theme/classes";
import { useAgentText } from "../../refs/AgentName";
import { blockFragment } from "../../refs/block-reference";
import { PublishedReferenceNames, type PublishedReferences } from "../../refs/reference-names";
import { standsFailed } from "../../refs/reference-target";
import {
  buildReferencePath,
  type PagedReferenceRoute,
  parseNodeReference,
} from "../../refs/routes";
import { appKeymap } from "../../shell/keymap";
import { useMediaQuery } from "../../shell/useDialog";
import { agentRoleWord } from "../agent-role";
import { CONTENTS_PANE_QUERY } from "../contents-frame";
import { decisionNodes } from "../decision-nodes/decision-nodes";
import { conversationPath } from "../decisions-filter/decisions-parts";
import { standingParts } from "../decisions-filter/entry-standing";
import { useDocumentAdmissions } from "../document-admission";
import { useDocumentStage } from "../document-stage";
import { useSetFocusedNode } from "../focused-node";
import { type BriefCounts, ChecksNumber, IssueThreeThings } from "../IssueThreeThings";
import { focusedNodeOf, type OutlineRequest, useReleaseFocus } from "../IssueTree";
import { CLEARANCE } from "../identity-block";
import { presetPath } from "../issue-address";
import { artifactsBySlug, documentOfNode } from "../issue-artifacts";
import type { TreeNode } from "../issue-tree";
import { LensLines } from "../lenses/LensLines";
import { NodeGlyph } from "../NodeGlyph";
import { NodeEdges, useRowOnScreen } from "../NodeRow";
import { shortDay } from "../node-time";
import { useIssueNames } from "../one-line";
import type { OutlineReads } from "../outline-reads";
import { useTakeOutlineRequest } from "../outline-request";
import { useOutlineWriter } from "../outline-writer";
import { authorText, KEY_REPORTS, pageChrome } from "../page-structure";
import {
  authorKind,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GLYPH_TOKENS,
  NODE_TYPE_NAMES,
  SPEC_WORDS,
  STATE_LINES,
  sentenceCase,
  VERIFICATION_WORDS,
} from "../page-words";
import { usePressFocus } from "../press-focus";
import { RowText, useOutOfTabOrder } from "../RowText";
import { FailedReadLine } from "../slots/failed-read-line";
import { specSummary } from "../spec-summary";
import { treeContext } from "../tree-drafts";
import { nodeRowSelectorOf, TAB_STOP_ROW } from "../tree-focus";
import { nodesInOrder, rowKeyOf } from "../tree-keys";
import {
  issueTreeQuery,
  useBlocksRunning,
  useLastAnswer,
  useTreeRead,
  viewQuery,
} from "../tree-read/tree-read";
import { type LineRead, useLineRefusals } from "../useLineRefusals";
import { abortOnReaderAct, nodeEditable, type OnEditing, useNodeEditing } from "../useNodeEditing";
import { moveToRow, useTreeKeymap } from "../useTreeKeymap";
import { CAP } from "../verification-filter/VerificationFilterBody";
import { treeItemOf, useAddressActed, useAddressLanding } from "./address-landing";
import { holdWhereItStands } from "./hold-where-it-stands";
import { focusedTreeItem, pressInWords, statementWordsAt } from "./pressed-words";
import {
  type SpecDecision,
  type SpecGroup,
  type SpecMark,
  type SpecParent,
  type SpecSaid,
  type SpecStatement,
  specDecidedBy,
} from "./spec-decided-by";
import { treeRead } from "./tree-read";

/** What a key that folds, zooms, cycles or opens a branch does here, where the columns hold
 *  none: every statement is drawn whole (R2: "a filter draws every branch it kept open"). */
const NOTHING = (): void => {};

/**
 * The Spec filter's body (R5, R17, R22, R35, the prototype's `.spec`).
 *
 * It is the approved picture `page-v3-spec-1440.png`: a legend of the four marks a statement
 * carries, then two columns, Decided by and Statement. In the left column stands, once per run
 * of statements one decision shaped, that decision in the words it came from, with a bracket
 * tying it to its statements; in the right column each statement's words in full, with one line
 * under them naming the decision that shaped them, and at the right of its first line the count
 * of the checks that serve it and its mark. At 900 px and below it is one column and each
 * decision stands above its statements, as `page-v3-spec-390.png` draws it.
 *
 * Which decision shaped which statement is `spec-decided-by.ts`, which is pure; this module
 * draws it and holds two rules of its own:
 *
 * - **The question is drawn whole beside its answer, also under its own heading.** A group's
 *   heading draws its parent's first line by R1's one-line rule, which cuts it at 100
 *   characters. The cell gives the question an answer answers by its first sentence whole, and
 *   the line under each statement gives it again before the quote, also where the group's
 *   heading is that question (R5), so a question the heading cuts is read whole beside the
 *   statements its answer shaped.
 * - **A statement's own words carry its number.** The picture draws a tag, "R1", at the right
 *   of each statement; the model holds no short name for a statement, and a block id is an
 *   identifier, which R22 refuses where a name belongs. The spec's own words open "**R5.**"
 *   where its author wrote a number, and the row draws them in full, so the number a reader
 *   cites is the author's own.
 *
 * Every word a statement and a decision hold is drawn whole, with no limit of lines, with the
 * focus or without it (R5, R6): a statement's words, and an agent's decision's words in its
 * cell. The focus on a cell adds only a person's whole answer as stored and "Author unknown".
 *
 * After its column heads the body draws the issue's three sentences under their labels, and the
 * two rows under them, as the picture's first group, in the statements' column, as the
 * prototype's `renderSpec` puts them.
 *
 * It is the outline under the Spec filter, so it is one `role="tree"` named as the outline is,
 * with the outline's keys (`useTreeKeymap.ts`): the statements are its rows, the focused one is
 * its one tab stop, the arrows and `j`/`k` move between them, `w` moves to the decision beside
 * a statement, the arrows and `j`/`k` move on from that decision to the statement after or
 * before its run, `f` steps through a statement's links - its count of checks among them, which
 * opens the Verification view on that statement. Enter puts the caret at the block's start. A
 * press puts it at the pressed text without scrolling the page; Escape returns to the row.
 * `r` replies to a statement once a statement can be a target of the outline's writer, which
 * the body already passes to the keymap. The focus on a statement puts its facts in Details,
 * as a row of the outline does, and the focus on a decision puts that decision's. An address
 * naming a statement (`/spec#b-<block>`) puts the focus on it (R10, R19).
 */
export function SpecFilterBody({
  addressActs,
  context,
  counts,
  emptyLine,
  holdsEscape,
  issue,
  namesLine,
  onEditing,
  published,
  reader,
  readsSettled,
  request,
  reveal,
  root,
  view,
}: {
  /** The reader's acts since they followed the address: a controller their first act aborts,
   *  which the stage starts once for each following (`StageSlot`'s `ReaderActsWatch`) and the
   *  landing reads, as the outline's does (`IssueTree`'s `addressActs`). None where the body is
   *  drawn without the stage, which then lands as where the reader made no act. */
  addressActs?: RefObject<AbortController | undefined>;
  /** The ids of the rows the filter draws only as context, drawn faint (R2, R32). */
  context: ReadonlySet<string>;
  /** What the first group's two rows count, or undefined while the tree may not be counted yet
   *  (`briefCounts`); the slot reads it from the page's tree (`slots/view-spec.tsx`). */
  counts: BriefCounts | undefined;
  /** What the body says where the filter's query left no statement. */
  emptyLine: string;
  /** Whether the page takes Escape from a row before the body is left: a filter by words to
   *  clear or the filter to turn off comes first in the key table's order
   *  (`slots/view-spec.tsx`). */
  holdsEscape: boolean;
  issue: IssueDetails;
  /** The names read's line while it stands failed (`outline-reads.ts`'s `names`), which the
   *  slot passes from the page's tree: that read is the tree's own, and the view draws from it
   *  too - the name of every reference the three sentences and a plain line cite. Empty while it
   *  does not stand failed. */
  namesLine: OutlineReads["names"];
  /** Asks the page for the editor of the document a row is being edited in. */
  onEditing: OnEditing;
  /** What the page publishes of its one batched names read (`useIssueTree.ts`'s `published`),
   *  which the body hands every row it draws in the render that draws it
   *  (`PublishedReferenceNames`), as the outline does (`IssueTree`'s `published`): a reference
   *  the page is asking for waits on the page's answer from a row's first render, rather than
   *  being read again by the row (R8). */
  published: PublishedReferences;
  /** Who is reading, so a decision of theirs reads as their own words (R34). */
  reader: string | undefined;
  /** Whether every read the tree's rows come from has answered, with its data or with its
   *  failure (`useOutlineReads`'s `readsSettled`), which the node the address names waits on
   *  before it takes the focus (`reveal`). */
  readsSettled: boolean;
  /** What the page last asked of the outline: go to a node. The page acts on each ask once,
   *  whichever outline or body took it (`useTakeOutlineRequest`). */
  request?: OutlineRequest;
  /** The node the address names, which takes the focus and the tab stop once per following of
   *  the address (R19), unless the reader has acted since they followed it. */
  reveal?: string;
  /** The whole tree, which holds the asks, the decisions and each statement's checks, and on
   *  the issue's node the Spec view read the page holds (`TreeNode.views`), whose decisions the
   *  column draws. */
  root: TreeNode;
  /** The tree as the Spec filter leaves it (`presetView`). */
  view: TreeNode;
}): ReactNode {
  const navigate = useNavigate();
  // The body waits on the reads of what it draws, and on no other, so a document it draws
  // nothing from - the plan, the decisions document's blocks - changes nothing here, whether
  // its read is out or was refused. Each read is watched under the key the page's tree fills
  // (`useIssueTree.ts`) by an observer that sends no request of its own (`treeRead`). A read
  // is held where it holds an answer, never by its status: a read the page asks again - on
  // every event that names its key (`event-refresh.ts`) - keeps its answer while it is out and
  // when it fails, and what the body drew from that answer stands. A read that stands failed -
  // no answer held, and a fetch of it failed (`standsFailed`) - says so over the tree, with a
  // Retry that asks that read again, and its line stays, its Retry busy, while the read runs
  // again, until it answers. Each read's options are held, so its line can tell its key
  // (`useLineRefusals`).
  const spec = primarySpec(issue);
  const verification = roleDocument(issue, "verification");
  const coverageQuery = issueCoverageQuery(issue.key);
  // The spec's blocks come from its view read and the Verification document's from the outline
  // read (`useTreeRead`); `treeQuery` names the tree key both lines tell their refusal by.
  const treeQuery = issueTreeQuery(issue.key);
  const resultsQuery = issueResultsQuery(issue.key);
  const specQuery = viewQuery(issue.key, "spec");
  const asksQuery = issueAskThreadsQuery(issue.key);
  const coverageRead = useQuery(treeRead(coverageQuery));
  const { blocksOf } = useTreeRead(issue);
  // Whether the read that holds each role's blocks runs, from an observer of its own
  // (`useBlocksRunning`): what a line of those blocks draws its Retry busy by. Under the Spec
  // filter the spec's are its view read's, and the Verification document's the outline read's.
  const specBlocksRead = { ...blocksOf("spec"), isFetching: useBlocksRunning(issue.key, "spec") };
  const resultsRead = useQuery(treeRead(resultsQuery));
  const checksRead = {
    ...blocksOf("verification"),
    isFetching: useBlocksRunning(issue.key, "verification"),
  };
  const asksRead = useQuery(treeRead(asksQuery));
  const asks = asksRead.data;
  // The decisions are the decision nodes of the Spec view read (`?view=spec`), which the page
  // holds on the issue's node (`TreeNode.views`), so the body costs no request of its own
  // to draw them: each decision the view holds, and each question a person answered that shaped
  // something. The decision the read carries on the ask decides that, by its `links`; the asks
  // read is only the fallback where the read carries none, an ask it says is answered that
  // carries `shaped` ties (`decisionNodes`). The view read is watched under the key the page
  // reads it by (`ViewReadProvider`). A change of the issue marks the view stale and reads it
  // again with its answer kept (`subscribeTreeFreshness`); a new load, a Keep and a Change drop
  // it (`dropViews`), and the column then draws the answer it last held (`useLastAnswer`) until
  // the next one lands. A read again that fails keeps its answer, which leaves the body an
  // answer to draw too.
  const specRead = useQuery(treeRead(specQuery));
  const held = useLastAnswer(root.views?.spec);
  const decisions = useMemo(
    () => (held === undefined ? [] : decisionNodes(held, { asks, issue })),
    [asks, held, issue]
  );
  // The tree that view read builds (`TreeViews.trees`), the one last held as its answer is: it
  // holds each answered question whose `shaped` ties name a statement of the view, which
  // Conversation's read the page's tree is built from may not have reached, so a person's
  // decision is drawn by their answer whatever page of that read the page holds.
  const heldTree = useLastAnswer(root.views?.trees?.spec);
  // The view read as the lines below read a read: held where the column holds an answer of it,
  // the last one included, and refused, run and asked again by its own query.
  const decisionsRead = {
    data: held,
    errorUpdateCount: specRead.errorUpdateCount,
    isFetching: specRead.isFetching,
    refetch: specRead.refetch,
  };
  // One predicate per read: whether it holds an answer. Every part of the body that says what
  // decided a statement or how it stands - the decision cell and its line saying none did, the
  // line under a statement, the runs, the mark, and what `w` and `q` report - reads the
  // predicates of the reads it draws from, and draws or reports nothing while one of them is not
  // held, rather than an absence that may be false. A document the issue does not name has no
  // blocks to wait for.
  const coverageHeld = coverageRead.data !== undefined;
  // The spec's blocks are its statements and its summary.
  const specBlocksHeld = spec === undefined || specBlocksRead.data !== undefined;
  const resultsHeld = resultsRead.data !== undefined;
  // The Verification document's blocks put each check under the statement it serves.
  const verificationBlocksHeld = verification === undefined || checksRead.data !== undefined;
  const decisionsHeld = held !== undefined;
  // The asks read brings a person's answer, which their decision is drawn by, the answered
  // question a statement hangs under, which heads its run (`spec-decided-by.ts`), and the open
  // questions `q` goes to (`tree-keys.ts`'s `nextQuestion`).
  const asksHeld = asksRead.data !== undefined;
  // The statements come from the spec's blocks and their coverage: until both are held the body
  // cannot say the issue holds no statement.
  const statementsHeld = specBlocksHeld && coverageHeld;
  // Each statement's mark is how its checks stand, each at the commit the server names for it,
  // as the Verification filter reads it (`statement-state.ts`): from the results, the coverage,
  // and the Verification document's blocks. Before all three are held a mark would say Unknown
  // or no check of a statement that is Met, so none is drawn until they are.
  const marked = resultsHeld && coverageHeld && verificationBlocksHeld;
  // What decided each statement comes from the Spec view read and the asks read: until both are
  // held a run whose cell draws no decision may yet have one, so nothing says that none did.
  const decidedHeld = decisionsHeld && asksHeld;
  // Each line of the reads that stand failed (`standsFailed`), in the page's words for such a
  // read (`STATE_LINES`), under the key it is drawn by: the coverage's, the spec's blocks' and
  // the Spec view read's as the statements' - one line for that read, which the decisions come
  // from (decision 450) - the results' and the Verification document's blocks' as the results',
  // which leave every statement unmarked alike, and the asks'. A line stays, its Retry busy,
  // while a read of it that stands failed runs again, until the read answers. A line's Retry
  // asks again the reads of that line that stand failed (`failed`), and no other, and none of
  // them while it is read again already. Each read of a line is paired with its key's hash
  // (`lineRead`), and each line carries the refusals of its reads it has counted while the body
  // lives, each once under its key (`useLineRefusals`), which keys its alert, whether it stands,
  // which draws it, and whether it speaks.
  // The names read's line (`namesLine`) comes after all of these, as the outline draws it
  // last (`outline-reads.ts`): the tree reads the names last, once no read it is built from is
  // still out, and gives that line's state itself.
  const lines = [
    {
      line: "statements",
      message: STATE_LINES.statements_unreadable,
      reads: [
        lineRead(coverageQuery, coverageRead),
        lineRead(treeQuery, specBlocksRead),
        lineRead(specQuery, decisionsRead),
      ],
    },
    {
      line: "results",
      message: STATE_LINES.spec_results_unreadable,
      reads: [lineRead(resultsQuery, resultsRead), lineRead(treeQuery, checksRead)],
    },
    {
      line: "asks",
      message: STATE_LINES.asks_unreadable,
      reads: [lineRead(asksQuery, asksRead)],
    },
  ] as const;
  const counted = useLineRefusals(lines.map(({ reads }) => reads));
  const unreadable = lines
    .map((group, at) => ({
      ...group,
      failed: group.reads.flatMap(({ failing, read }) => (failing ? [read] : [])),
      refusals: counted[at].refusals,
      speaks: counted[at].speaks,
      stands: counted[at].stands,
    }))
    .filter(({ stands }) => stands);
  // What a reference a plain line cites is named by - in the three sentences, a quote, a
  // question, a group's heading - the names the page published, as a row of the outline names it
  // (`referenceStandIns`, `useIssueNames`); and how this issue addresses a node, which is the one
  // rule the tree was built with, so a `shaped` tie resolves to the node it names
  // (`tree-drafts.ts`'s `treeContext`).
  const lineNames = useIssueNames(issue.key);
  const nodeIdOf = useMemo(() => treeContext({ issue }).nodeIdOf, [issue]);
  const { groups, others } = useMemo(
    () =>
      specDecidedBy({
        asksHeld,
        checks: resultsRead.data?.checks ?? [],
        decisions,
        lineNames,
        nodeIdOf,
        reader,
        root,
        view,
        ...(heldTree === undefined ? {} : { viewTree: heldTree }),
      }),
    [asksHeld, decisions, heldTree, lineNames, nodeIdOf, reader, resultsRead.data, root, view]
  );
  const specSlug = spec?.slug ?? "";
  const summary = useMemo(() => specSummary(root, specSlug), [root, specSlug]);
  // From 1024 px up the Contents pane stands beside the view and holds the issue's three
  // sentences, so the view draws no first group of them there, and they are drawn once (R13).
  const paned = useMediaQuery(CONTENTS_PANE_QUERY);
  const setFocusedNode = useSetFocusedNode();
  const writer = useOutlineWriter();
  const tree = useRef<HTMLDivElement | null>(null);
  const [focused, setFocused] = useState<string | undefined>(undefined);
  // The run whose cell holds the focus itself, which draws a person's whole answer as stored and
  // "Author unknown" where no author is drawn (R5), named by the run's first statement;
  // undefined while the focus is on a statement or on a control inside a cell. The body holds
  // it, as it holds the focused statement, so a press defers both alike (`press`).
  const [openCell, setOpenCell] = useState<string | undefined>(undefined);
  // Where the tree's one tab stop rests, which outlives the focus leaving it (R9).
  const [stop, setStop] = useState<string | undefined>(undefined);
  // What a key reported when it had nowhere to go, and the row it was pressed on, which draws it
  // under its words until the reader moves on (the tester's F16, `NodeRow`'s `report`).
  const [report, setReport] = useState<{ readonly line: string; readonly row: string }>();
  const reportHere = useCallback((line: string) => {
    const row = document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
    setReport(line === "" || row === undefined ? undefined : { line, row });
  }, []);
  const artifacts = useMemo(() => artifactsBySlug(issue), [issue]);
  const admissions = useDocumentAdmissions();
  const { edit, editing, stopEditing } = useNodeEditing({
    artifacts,
    container: tree,
    onEditing,
    onReport: reportHere,
  });
  // The statements in the order they are drawn, which is the order the keys move through them,
  // and after them every other row the filter kept.
  const statements = useMemo(
    () => [
      ...groups.flatMap((group) =>
        group.runs.flatMap((run) => run.statements.map((statement) => statement.node))
      ),
      ...others,
    ],
    [groups, others]
  );
  const { rows, rowKeys } = useMemo(() => {
    const rows = statements.map((node) => ({ kind: "node" as const, level: 1, node, trail: [] }));
    const rowKeys = new Map(rows.map((row) => [row.node.id, rowKeyOf(row)]));
    return { rows, rowKeys };
  }, [statements]);
  const editingRow = editing?.row;
  // When the filter or a read removes the edited row, release its document even if the focus
  // was already elsewhere. There is no row for Escape's focus restoration to find.
  useEffect(() => {
    if (editingRow !== undefined && !rows.some((row) => rowKeyOf(row) === editingRow)) {
      stopEditing();
    }
  }, [editingRow, rows, stopEditing]);
  const takeFocus = useCallback(
    (node: TreeNode) => {
      setFocused(node.id);
      setOpenCell(undefined);
      setStop(node.id);
      // A move to a statement is the reader going on from what a key reported.
      setReport((held) => (held?.row === node.id ? held : undefined));
      setFocusedNode(focusedNodeOf(node));
    },
    [setFocusedNode]
  );
  // The focus on a decision's cell is the reader reading that decision: no statement draws the
  // focused look any longer, and Details draws the node the cell is drawn as, or the issue's own
  // facts beside a run no decision shaped. The tab stop stays on the last statement (R9).
  // `open` names the run whose cell draws what only the focus draws, where the cell itself has
  // the focus.
  const takeCell = useCallback(
    (decision: SpecDecision | undefined, open: string | undefined) => {
      setFocused(undefined);
      setOpenCell(open);
      setReport(undefined);
      const id = decision?.node ?? decision?.id;
      const node = id === undefined ? undefined : nodesInOrder(root).find((one) => one.id === id);
      setFocusedNode(node === undefined ? undefined : focusedNodeOf(node));
    },
    [root, setFocusedNode]
  );
  // R7 under a press, as the outline holds it (`usePressFocus`). A press focuses what it lands
  // on as it goes down, before it ends, so whatever the focus draws would come and go under the
  // pointer while the press is held: a cell taking the focus would draw a person's whole answer,
  // and the cell the focus left would take its own away, so what stood under the pointer would
  // move off it and the press would end on something else, following no link (review 5928280634
  // on pull request 4234). So the focus a press gives a statement or a cell - any focus inside
  // it, a link's included - is taken once the press is over, and only while that statement or
  // cell still holds the focus; a key's move takes it at once. Each item is the statement's or
  // the cell's element, which says whether it still holds the focus, with what taking the focus
  // does for it.
  type ItemFocus = {
    readonly item: HTMLElement;
    readonly take: () => void;
  };
  // The active press hold's guarded re-read (`holdWhereItStands`), written on click and taken
  // once the press's focus settles so any placing after `take()` counts as where the item stands.
  const pressReadPlacement = useRef<(() => void) | undefined>(undefined);
  const [readPressPlacement, setReadPressPlacement] = useState<(() => void) | undefined>();
  // The rows' layout effects run before this one, so their placing belongs to the press.
  useLayoutEffect(() => readPressPlacement?.(), [readPressPlacement]);
  const press = usePressFocus<ItemFocus>(
    (focus) => {
      const readPlacement = pressReadPlacement.current;
      pressReadPlacement.current = undefined;
      focus.take();
      if (readPlacement !== undefined) setReadPressPlacement(() => readPlacement);
    },
    ({ item }) => item.contains(document.activeElement)
  );
  const focusItem = useCallback(
    (item: HTMLElement, take: () => void) => press.focus({ item, take }),
    [press]
  );
  const focusStatement = useCallback(
    (item: HTMLElement, node: TreeNode) => focusItem(item, () => takeFocus(node)),
    [focusItem, takeFocus]
  );
  const focusCell = useCallback(
    (item: HTMLElement, decision: SpecDecision | undefined, open: string | undefined) =>
      focusItem(item, () => takeCell(decision, open)),
    [focusItem, takeCell]
  );
  // The focus leaving the body gives the panel back to the issue's own facts, unless it went
  // to Details, where a reader reads the node's: the outline's own rule (`useReleaseFocus`).
  // Keep the row's facts while its editor moves onto the row or the reader presses in Details.
  const leave = useCallback(() => {
    setFocused(undefined);
    setOpenCell(undefined);
  }, []);
  const releaseFocus = useReleaseFocus({ editing: editing !== undefined, onLeave: leave });
  // Puts the focus on the statement or the decision cell that draws node `id`, as the outline
  // moves to a row (`moveToRow`), which brings it on screen where it already holds the focus.
  const goTo = useCallback((id: string): boolean => {
    const target = treeItemOf(tree.current, id);
    if (target !== null) moveToRow(target);
    return target !== null;
  }, []);
  // `w` on a statement, or on a control inside one: the focus goes to the cell beside it, which
  // draws the decision its run is headed by, whichever decision the tree's order names first
  // (`decisionBehind` can name one that only informed it, or one that was replaced); a run no
  // decision shaped says so at the statement (R5, R35) once what decided each statement is held,
  // and says nothing before that, since its cell may yet draw one. False where the focus is on
  // no statement, which leaves the key to the keymap.
  const toOwnCell = useCallback((): boolean => {
    const container = tree.current;
    const statement = document.activeElement?.closest<HTMLElement>("[data-spec-statement]");
    const id = statement?.dataset.nodeId;
    if (container === null || statement == null || id === undefined) return false;
    if (!container.contains(statement)) return false;
    const cell = container.querySelector<HTMLElement>(
      `[data-spec-cell][data-spec-statements~="${CSS.escape(id)}"]`
    );
    if (cell?.hasAttribute("data-decision-id") === true) cell.focus();
    else reportHere(decidedHeld ? KEY_REPORTS.no_decision : "");
    return true;
  }, [decidedHeld, reportHere]);
  // `j`, `k` and the arrows from a cell, which is no row of the keymap's (it finds none there
  // and would start again from the top): the statement after the cell's run, or before it,
  // and at either end of the tree the run's own last or first statement, as the keymap stops
  // at its ends.
  const leaveCell = useCallback(
    (cell: HTMLElement, step: 1 | -1): void => {
      const rows = [...(tree.current?.querySelectorAll<HTMLElement>("[data-node-row]") ?? [])];
      const ids = (cell.dataset.specStatements ?? "").split(" ");
      const first = rows.findIndex((row) => row.dataset.nodeId === ids[0]);
      const last = rows.findIndex((row) => row.dataset.nodeId === ids.at(-1));
      reportHere("");
      const to = step > 0 ? (rows[last + 1] ?? rows[last]) : (rows[first - 1] ?? rows[first]);
      if (to !== undefined) moveToRow(to);
    },
    [reportHere]
  );
  // What the page asked for, each ask taken once by whichever outline or body the page drew when
  // it came (`useTakeOutlineRequest`), so a body drawn again - the view turned off and on - does
  // not move the reader for an ask already acted on. An ask goes only to a statement's row,
  // never to a cell: a cell can carry the answered ask's id, which a Contents entry names.
  const takeRequest = useTakeOutlineRequest();
  useEffect(() => {
    if (request === undefined || !takeRequest(request) || request.id === undefined) return;
    tree.current?.querySelector<HTMLElement>(nodeRowSelectorOf(request.id))?.focus();
  }, [request, takeRequest]);
  // An address that names a row puts the focus on it once every read the tree's rows come from
  // has settled (`useAddressLanding`), looking again each time the statements the view draws
  // change, and the landing's hold keeps it on screen while the page grows above it - with the
  // statements, which the coverage read draws (`tree-drafts.ts`'s `addRequirements`), or the
  // spec's blocks where they answer first, and with what the page draws after the reads (the
  // names, the cuts once measured).
  const drawn = useMemo(() => statements.map((node) => node.id).join(" "), [statements]);
  const landingHold = useAddressLanding({
    acts: addressActs,
    drawn,
    id: readsSettled ? reveal : undefined,
    tree,
  });

  // Hold a tree item where it stands until the next act, so reads that draw rows above it cannot
  // push it out of view (`holdWhereItStands`). A key starts a hold at keyup only where the page
  // answered it - its keydown's default was prevented, by the keymap or by the tree's own cell
  // keys - so End on the last row still holds it while Page Down, Space and Page Up, which the
  // browser answers, start no hold. The native keydown event is kept until the matching keyup
  // because the keymap's window listener runs after React's handler, so `defaultPrevented` is
  // final only by then. A press can hold the item it clicked if that item has focus. Tab and a
  // press outside an item start no hold. The editor places its own caret, so its row starts no
  // hold either. The hold replaces the address's landing in `landingHold`.
  const keyInTree = useRef<globalThis.KeyboardEvent | undefined>(undefined);
  const holdFocusedItem = (item: HTMLElement) => {
    if (editingRow !== undefined && item.dataset.rowKey === editingRow) return;
    return holdWhereItStands(item, landingHold);
  };
  const pressedWords = useRef<HTMLElement | null>(null);
  // A press on words already on screen gives the row focus without placing it, even
  // if it selects text or opens no editor. The next reader act restores normal placing.
  const [wordsPressRow, setWordsPressRow] = useState<string | undefined>(undefined);
  const wordsPressActs = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => wordsPressActs.current?.abort(), []);

  useTreeKeymap({
    container: tree,
    editing: editing !== undefined,
    holdsEscape,
    onCycle: NOTHING,
    onEdit: edit,
    onEditEnd: stopEditing,
    onFold: NOTHING,
    onPin: NOTHING,
    // The keymap's `q` and `w` say where they could not go only while its one `readsHeld` flag
    // holds. `q` walks the open questions, which the asks read brings, so the flag is
    // `asksHeld`; `w` walks the decisions, so its report waits on the Spec view read as well,
    // here (`decidedHeld`, which holds only where `asksHeld` does). This filter tells `w`'s
    // report by its words because the keymap holds that one flag for every key; it goes once
    // the keymap takes a flag per key, when `w`'s flag is `decidedHeld`.
    onReport: (line) => reportHere(line === KEY_REPORTS.no_decision && !decidedHeld ? "" : line),
    // `q` and `w` reach here with the node the keymap found: the next open question, or the
    // decision behind the focused row. Where the view draws a row or a cell for it the focus
    // goes there, and a decision named from a statement is that statement's own cell's to
    // answer, as the tree's key handler answers a `w` it takes first. Where the view draws
    // nothing for it - an open question no address named, a decision that heads no run of the
    // view - the key goes to it in the conversation (`conversationPath`), which opens the
    // outline there with the focus on it. Until the Spec view read and the asks read answer, a
    // decision with no cell may yet have one, and the key does nothing. The node's kind
    // comes from the tree the keymap found it in, not the Spec view read.
    onReveal: (id) => {
      const node = nodesInOrder(root).find((one) => one.id === id);
      const decided = node?.type === "decision";
      if (decided && toOwnCell()) return;
      if (goTo(id)) return;
      if (decided && !decidedHeld) return;
      const path = node === undefined ? undefined : conversationPath(node);
      if (path !== undefined) navigate(path);
    },
    onZoom: NOTHING,
    readsHeld: asksHeld,
    rows,
    // The whole tree, which `q` walks for the next open question, and `w` on a row that is no
    // statement for the decision behind it (`tree-keys.ts`'s `decisionBehind`).
    root,
    writer,
    zoom: undefined,
  });

  // The tree's one tab stop: the row the focus last rested on (R9), else the row the address
  // names once the reads have settled, where the reader has not acted since they followed the
  // address, else the first statement. The address's row is the tab stop from the render that
  // lands on it, as the Plan view's is: the stage gives a focus that rests on nothing to the
  // tab-stop row as that render is drawn (`slots/stage.tsx`), before the landing's frame, and a
  // landing whose row lost the focus to another row by then leaves the focus there
  // (`landOnRow`). Whether the reader has acted since the following is read in the render that
  // first finds the address's row in that following, and held for the rest of it, under the key
  // the landing takes (the following and the row, `addressStop`): for a reader who acted the
  // landing does nothing, but the stage still gives a focus they left on nothing - a turn of the
  // wheel, or a key pressed on the page, before any statement was drawn - to the tab-stop row as
  // the statements come, where the address's row would take it off screen; the first statement
  // takes it instead. An act aborts `addressActs` and draws nothing, so a tab stop that read it
  // at every render would move at whatever render came next. A row the reader moved to holds the
  // tab stop itself (`stop`).
  const { acted, following } = useAddressActed(addressActs);
  const addressed = readsSettled ? statements.find((node) => node.id === reveal) : undefined;
  const addressKey = addressed === undefined ? undefined : `${following}:${addressed.id}`;
  const [addressStop, setAddressStop] = useState<{
    readonly gives: boolean;
    readonly key: string;
  }>();
  if (addressKey !== undefined && addressStop?.key !== addressKey) {
    setAddressStop({ gives: !acted, key: addressKey });
  }
  const addressTakes = addressStop?.key === addressKey && addressStop?.gives === true;
  const tabStop =
    stop !== undefined && statements.some((node) => node.id === stop)
      ? stop
      : ((addressTakes ? addressed : undefined) ?? statements[0])?.id;
  const rowProps = (node: TreeNode): SpecRowProps => {
    const rowKey = rowKeys.get(node.id);
    const place = documentOfNode(node.id, artifacts);
    return {
      editable: nodeEditable(place, issue.closed_at !== null, admissions),
      editing: editing !== undefined && editing.row === rowKey ? editing.artifactId : undefined,
      focused: focused === node.id,
      onFocus: (row) => focusStatement(row, node),
      ...(report?.row === node.id ? { report: report.line } : {}),
      rowKey,
      tabStop: tabStop === node.id,
    };
  };
  const drawnBody = (
    <div className="mt-3.5 mb-[120px] flex w-full min-w-0 flex-col" data-spec-body="">
      {/* Each line of the reads that stand failed, in the page's words, as every view drawn in
          the stage says one (`FailedReadLine`): over the tree and never in it, since an alert
          inside a tree would break the tree's own structure of rows. The view's own reads'
          lines come first, and the names read's line last, as the outline draws it. The stage
          gives a focus left on nothing to a line's Retry where the view draws no row, and moves
          it on to the next line, the tab-stop row or the tree as a line that held it leaves
          (`slots/stage.tsx`). */}
      {unreadable.map(({ failed, line, message, refusals, speaks }) => (
        <FailedReadLine
          // Which failure the line says, which keys its alert: the refusals of the line's reads
          // that stand failed that the line has counted while the body lives, each once under
          // its key (`useLineRefusals`). That count rises by one for each refusal new to the line
          // and never falls, so the line says its failure again exactly for a new refusal:
          // neither a run that ends with no new refusal, as one paused while the browser is
          // offline does, nor a read back on a key whose refusal the line counted says it again.
          failure={refusals}
          key={line}
          message={message}
          onRetry={() => {
            for (const read of failed) if (!read.isFetching) void read.refetch();
          }}
          retrying={failed.every((read) => read.isFetching)}
          // Whether the line speaks (`useLineRefusals`): a line that stands again for refusals
          // it counted draws without speaking, since it said them as it counted them.
          speaks={speaks}
        />
      ))}
      {namesLine.map((line) => (
        <FailedReadLine
          // The names read's count of failures (`NamesFailure.failure`), which changes only with
          // a new failure, and its Retry, which asks it again while it holds no answer.
          failure={line.failure}
          key={line.key}
          message={line.message}
          onRetry={line.retry}
          retrying={line.retrying}
          speaks={line.speaks}
        />
      ))}
      <Legend />
      <div
        className={`sticky top-[var(--issue-identity-height,0px)] z-[6] grid grid-cols-[280px_minmax(0,1fr)] gap-x-5 border-b pt-2 pb-1.5 max-[901px]:hidden ${pageLineBorder} ${pagePaperBg}`}
        data-spec-head=""
      >
        <span className={CAP} {...pageChrome}>
          {SPEC_WORDS.decided_by}
        </span>
        <span className={`pl-[26px] ${CAP}`} {...pageChrome}>
          {sentenceCase(NODE_TYPE_NAMES.requirement)}
        </span>
      </div>
      {/* The issue's three sentences and the two rows under them, the first group, in the
          statements' column, as the prototype's `renderSpec` puts them after the column heads. */}
      {paned ? null : (
        <div className="mt-[18px] ml-[300px] min-w-0 max-[901px]:ml-0">
          <IssueThreeThings
            counts={counts}
            issueKey={issue.key}
            lineNames={lineNames}
            look="spec"
            // The sentences come with the summary node's reading, which the tree read carries
            // with the spec's blocks (`slots/contents.tsx`).
            summary={summary}
          />
        </div>
      )}
      {/* The outline the page draws under the Spec filter, which is this body's two columns:
          one tree, named as the tree every other filter draws, so a reader meets one outline
          on every address (R2, `IssueTree.tsx`). Its items are each statement and each
          decision beside them, which is what the focus moves between (R5's `w`); the layout
          between them carries no role of its own. */}
      <div
        aria-label={CONTROL_LABELS.tree}
        className="flex min-w-0 flex-col"
        data-spec-tree=""
        onBlur={releaseFocus}
        onClick={(event) => {
          const item = focusedTreeItem(event.currentTarget);
          const words = pressedWords.current;
          pressedWords.current = null;
          if (item === null || !(event.target instanceof Node) || !item.contains(event.target)) {
            return;
          }
          const rowKey = item.dataset.rowKey;
          // A press on the focused statement's words opens the editor where it was; a drag that
          // selected words opens none, and the selection stays.
          if (
            event.detail > 0 &&
            words !== null &&
            words === statementWordsAt(event.target) &&
            window.getSelection()?.isCollapsed !== false &&
            rowKey !== undefined &&
            rowKey !== editingRow &&
            document.activeElement === item
          ) {
            const node = rows.find((row) => row.node.id === item.dataset.nodeId)?.node;
            if (node === undefined) return;
            const at = pressInWords(words, event.clientX, event.clientY);
            // Run after the press focus settles and the outline releases any edit the press
            // left. A link or control never supplies a words box here.
            window.setTimeout(() => {
              if (document.activeElement !== item || !item.contains(words)) return;
              reportHere("");
              edit(rowKey, node, at);
            }, 0);
            return;
          }
          pressReadPlacement.current = holdFocusedItem(item);
        }}
        // A focus on the tree itself - the page's `t s` lands here - goes on to the tab-stop
        // row; where the view draws no row it stays here.
        onFocus={(event) => {
          if (event.target !== event.currentTarget) return;
          event.currentTarget.querySelector<HTMLElement>(TAB_STOP_ROW)?.focus();
        }}
        // The keys a statement and a cell answer before the keymap does (`useTreeKeymap.ts`
        // listens on the window, after this): `w` on a statement, and `j`, `k` and the arrows
        // on a cell, which the keymap finds no row on. Keep each non-Tab keydown's native event
        // so `onKeyUp` can read whether the page prevented its default.
        onKeyDown={(event) => {
          keyInTree.current = event.key === "Tab" ? undefined : event.nativeEvent;
          if (
            event.target instanceof HTMLElement &&
            (event.target.isContentEditable || event.target.matches("input, select, textarea"))
          ) {
            return;
          }
          const key = bareKey(event);
          if (key === "w") {
            if (toOwnCell()) event.preventDefault();
            return;
          }
          const step = key === undefined ? undefined : CELL_STEPS[key];
          const cell =
            event.target instanceof Element
              ? event.target.closest<HTMLElement>("[data-spec-cell]")
              : null;
          if (step === undefined || cell === null) return;
          event.preventDefault();
          leaveCell(cell, step);
        }}
        onKeyUp={(event) => {
          const down = keyInTree.current;
          if (down?.key !== event.key) return;
          keyInTree.current = undefined;
          if (!down.defaultPrevented) return;
          const item = focusedTreeItem(event.currentTarget);
          if (item !== null) holdFocusedItem(item);
        }}
        // A press in the tree, whose focus a statement or a cell takes once the press is over
        // (`press`). A press on what is no item - a group's heading, the space between runs
        // and cells - leaves the focus where it was: the tree would take it as their nearest
        // focusable box and hand it on to the tab-stop row, moving the reader and the page to a
        // statement they did not press.
        onMouseDown={(event) => {
          pressReadPlacement.current = undefined;
          press.onMouseDown();
          const words =
            event.button === 0 && !event.defaultPrevented ? statementWordsAt(event.target) : null;
          pressedWords.current = words;
          const row = words?.closest<HTMLElement>("[data-spec-statement]")?.dataset.nodeId;
          wordsPressActs.current?.abort();
          wordsPressActs.current =
            row === undefined ? undefined : abortOnReaderAct(() => setWordsPressRow(undefined));
          setWordsPressRow(row);
          if (
            event.target instanceof Element &&
            event.target.closest('[role="treeitem"]') === null
          ) {
            event.preventDefault();
          }
        }}
        ref={tree}
        role="tree"
        tabIndex={-1}
      >
        {groups.map((group) => (
          <Group
            drawStatement={(statement, decision) => (
              <Statement
                {...rowProps(statement.node)}
                context={context.has(statement.node.id)}
                decision={decision}
                key={statement.node.id}
                marked={marked}
                statement={statement}
                wordsPress={wordsPressRow === statement.node.id}
              />
            )}
            group={group}
            key={group.parent?.id ?? group.runs[0]?.statements[0]?.node.id ?? ""}
            known={decidedHeld}
            onCellFocus={focusCell}
            openCell={openCell}
          />
        ))}
        {others.map((node) => (
          <OtherRow {...rowProps(node)} key={node.id} node={node} />
        ))}
      </div>
      {statementsHeld && statements.length === 0 ? (
        // What the page says where the filter's query matched nothing under the issue, in the
        // words the outline says it in (`IssueTree.tsx`).
        <p className={`py-1 text-sm ${pageMutedText}`} data-spec-empty="" {...pageChrome}>
          {emptyLine}
        </p>
      ) : null}
    </div>
  );
  return <PublishedReferenceNames published={published}>{drawnBody}</PublishedReferenceNames>;
}

/**
 * A read of a failed-read line as `useLineRefusals` counts it, paired with its key's hash
 * (`hashKey`) in one object: it is failing while it stands failed (`standsFailed`). `read` is the
 * read itself, which the line's Retry asks again.
 */
function lineRead<Read extends Parameters<typeof standsFailed>[0]>(
  options: { readonly queryKey: QueryKey },
  read: Read
): LineRead & { readonly read: Read } {
  return {
    errorUpdateCount: read.errorUpdateCount,
    failing: standsFailed(read),
    hash: hashKey(options.queryKey),
    read,
  };
}

/** A link inside a line of words, which the hair-coloured underline marks as one. */
const INLINE_LINK = "dispatch-inline-link underline decoration-page-hair underline-offset-[3px]";

/** Which way each key the keymap moves between rows with moves from a cell: on to the
 *  statement after its run, or back to the one before it. */
const CELL_STEPS: Readonly<Partial<Record<string, 1 | -1>>> = {
  ArrowDown: 1,
  ArrowUp: -1,
  j: 1,
  k: -1,
};

/**
 * Places the focused statement or cell on screen by the page's one rule (`useRowOnScreen`,
 * `bringOnScreen`), and then below the view's own sticky column heads, which the page's scroll
 * padding does not hold (R4; R11: the focused row is never under a sticky header). The rule
 * places a row clear of the band and below what stays at the top of the screen, which is the
 * page's `scroll-padding-top`: it moves only a row whose top lies above that padding, and the
 * heads stand under the band, outside it, so on the tester's build a statement the arrows moved
 * up to stood with its top 27 px down, under heads 31 px tall. Where the item stands under the
 * heads it is scrolled down by the rest, by the rule's own clearance (`CLEARANCE`), again
 * whenever it grows - after the rule has placed it, since its observer is made first - and never
 * while a press inside it is held, as the rule holds its own placing. At 901 px and below the
 * heads are not drawn, and the rule alone places it.
 *
 * Answers the item's own `onPointerDown`, as `useRowOnScreen` does, which marks the press for
 * the rule's step and for this one.
 *
 * It is a copy of `useRowOnScreen`'s own press tracking and of its layout effect with a resize
 * observer (`NodeRow.tsx`), so a focused statement runs both: two observers, and two pairs of
 * window listeners for each press. It goes once `useRowOnScreen` takes an optional placing
 * step, which this view then passes.
 */
function useOnScreenUnderHeads(
  item: RefObject<HTMLElement | null>,
  focused: boolean,
  placesItself = true
): () => void {
  const placeByRule = useRowOnScreen(item, focused, undefined, placesItself);
  const pressed = useRef(false);
  // As in `useRowOnScreen`, a change in who places the row does not itself move the page.
  const placing = useRef(placesItself);
  placing.current = placesItself;
  useLayoutEffect(() => {
    const element = item.current;
    if (!focused || element === null) return;
    const place = () => {
      if (placing.current && !pressed.current) clearOfHeads(element);
    };
    place();
    if (typeof ResizeObserver === "undefined") return;
    const grown = new ResizeObserver(place);
    grown.observe(element);
    return () => grown.disconnect();
  }, [focused, item]);
  return () => {
    placeByRule();
    pressed.current = true;
    const ended = new AbortController();
    const release = () => {
      pressed.current = false;
      ended.abort();
    };
    window.addEventListener("pointerup", release, { capture: true, signal: ended.signal });
    window.addEventListener("pointercancel", release, { capture: true, signal: ended.signal });
  };
}

/** Scrolls the page until `element` stands below its view's column heads, where its top is
 *  under them; nothing where the heads are not drawn. */
function clearOfHeads(element: HTMLElement): void {
  const heads = element
    .closest("[data-spec-body]")
    ?.querySelector("[data-spec-head]")
    ?.getBoundingClientRect();
  if (heads === undefined || heads.height === 0) return;
  const top = element.getBoundingClientRect().top;
  if (top < heads.bottom) window.scrollBy({ top: top - heads.bottom - CLEARANCE });
}

/** The key `event` is as one of the keymap's single-key bindings reads it - a letter in lower
 *  case - or undefined where the keymap would read it otherwise: with a modifier held, in an
 *  input method's composition, or as the second key of a chord it is reading (`t k`). */
function bareKey(event: KeyboardEvent): string | undefined {
  if (
    event.defaultPrevented ||
    event.nativeEvent.isComposing ||
    event.altKey ||
    event.ctrlKey ||
    event.metaKey ||
    event.shiftKey ||
    appKeymap.pending() !== ""
  ) {
    return undefined;
  }
  return event.key.length === 1 ? event.key.toLowerCase() : event.key;
}

/** The colour each mark is drawn in, which repeats what its shape and the legend's word say
 *  (R6): met in the passed green, not met and no check in the failed red, unknown quiet. */
const MARK_TONES: Readonly<Record<SpecMark, string>> = {
  met: pageStateText.passed,
  no_check: pageStateText.failed,
  not_met: pageStateText.failed,
  unknown: pageStateText.quiet,
};

/** The four marks in the order the legend draws them. */
const MARK_ORDER: readonly SpecMark[] = ["met", "not_met", "unknown", "no_check"];

/** The cell's first line, which says who decided: an agent, a person, or the reader (R34). */
const SAID_WORDS: Readonly<Record<SpecSaid, string>> = {
  agent: SPEC_WORDS.said_agent,
  person: SPEC_WORDS.said_person,
  you: SPEC_WORDS.said_you,
};

/** The shape of a mark: how a statement stands, or that no check serves it. */
function markToken(mark: SpecMark): string {
  return mark === "no_check" ? GLYPH_TOKENS.no_check : GLYPH_TOKENS.statements[mark];
}

/** The word of a mark, in the Verification filter's own words for how a statement stands. */
function markWord(mark: SpecMark): string {
  return mark === "no_check" ? SPEC_WORDS.no_check : VERIFICATION_WORDS.states[mark];
}

/** What each mark means, above the columns: a shape and the word for it, since neither colour
 *  nor shape alone tells a reader what one says (R6). */
function Legend(): ReactNode {
  return (
    <p
      className={`mb-1 ml-[300px] flex flex-wrap gap-[14px] text-xs leading-5 max-[901px]:ml-0 ${pageQuietText}`}
      data-spec-legend=""
    >
      {MARK_ORDER.map((mark) => (
        <span
          className="inline-flex items-center gap-[5px]"
          data-spec-legend-mark={mark}
          key={mark}
        >
          <NodeGlyph className={`size-3 shrink-0 ${MARK_TONES[mark]}`} token={markToken(mark)} />
          <span {...pageChrome}>{markWord(mark)}</span>
        </span>
      ))}
    </p>
  );
}

/** Who made something, by a name and a shape, as every other row of the page draws an author
 *  (R9): never an identifier, and an agent by its role's word, as a row names one
 *  (`NodeRow.tsx`). Where the model holds no author it draws nothing: only the focused cell says
 *  so (`UnknownAuthor`). */
function Author({ actor }: { actor: Actor | undefined }): ReactNode {
  const agentText = useAgentText();
  const kind = authorKind(actor);
  if (kind === "unknown") return null;
  const name = agentText.author(actor);
  return (
    <span className="inline-flex items-center gap-1" data-spec-author="">
      <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors[kind]} />
      <span {...authorText}>{kind === "agent" ? agentRoleWord(name) : name}</span>
    </span>
  );
}

/** What the focused cell says where the model holds no author for its decision (R5), in the
 *  page's own words and with the shape of an author nobody recorded, as a row's opened body says
 *  it (`NodeRow.tsx`). */
function UnknownAuthor(): ReactNode {
  return (
    <span className="inline-flex items-center gap-1" data-spec-author="">
      <NodeGlyph className="size-3 shrink-0" token={GLYPH_TOKENS.authors.unknown} />
      <span {...pageChrome}>{CONTROL_LABELS.unknown_author}</span>
    </span>
  );
}

/** When something was made, by its day. */
function Day({ at }: { at: string }): ReactNode {
  return (
    <span className="whitespace-nowrap" {...authorText}>
      {shortDay(at)}
    </span>
  );
}

/** A decision's words quoted from a person or an agent, between the page's own curly quotes. A
 *  decision with no words is named by its kind's word, as `Informed` names one, so no quote and
 *  no link to it is empty. */
function Quote({ decision }: { decision: SpecDecision }): ReactNode {
  if (decision.quote === "") return <span {...pageChrome}>{NODE_TYPE_NAMES[decision.type]}</span>;
  const words = decision.quote;
  return (
    <>
      <span {...pageChrome}>“</span>
      <span {...authorText}>
        {/* spec-decision-line: the quote as one line of words `specDecidedBy` named. */}
        {words}
      </span>
      <span {...pageChrome}>”</span>
    </>
  );
}

/** Where the link to a decision goes: its node's own address, where the model holds one and it
 *  opens a page. A person's opens none (`PagedReferenceRoute`), and is drawn as words. */
function routeOf(decision: SpecDecision | undefined): PagedReferenceRoute | undefined {
  const route =
    decision?.reference === undefined ? undefined : parseNodeReference(decision.reference);
  return route?.kind === "person" ? undefined : route;
}

/** One part of a line `dotted` draws. */
interface LinePart {
  /** Whether it draws anything: false for an `Author` of nobody, which draws nothing. */
  readonly drawn?: boolean;
  readonly key: string;
  /** What it draws; `null` where the model holds nothing for it. */
  readonly part: ReactNode;
}

/** The parts of one line with a middle dot before each drawn part that follows another, so a
 *  part that draws nothing leaves no dot behind. */
function dotted(parts: readonly LinePart[]): ReactNode {
  const drawn = parts.map(({ drawn: shown, part }) => shown ?? part !== null);
  return parts.map(({ key, part }, place) => (
    <Fragment key={key}>
      {drawn[place] === true && drawn.slice(0, place).includes(true) ? (
        <span {...pageChrome}> · </span>
      ) : null}
      {part}
    </Fragment>
  ));
}

/** The statements under one parent: that parent's own line as the group's heading, then one row
 *  of the grid per run of statements one decision shaped. */
function Group({
  drawStatement,
  group,
  known,
  onCellFocus,
  openCell,
}: {
  drawStatement: (statement: SpecStatement, decision: SpecDecision | undefined) => ReactNode;
  group: SpecGroup;
  /** Whether the Spec view read the decisions come from and the asks read hold an answer, which
   *  is when a run with no decision can say that none shaped it. */
  known: boolean;
  /** Takes a focus inside a run's cell, with the decision the cell draws, and the run whose
   *  cell draws what only the focus draws where the cell itself has the focus. */
  onCellFocus: (
    cell: HTMLElement,
    decision: SpecDecision | undefined,
    open: string | undefined
  ) => void;
  /** The run whose cell has the focus itself, by its first statement. */
  openCell: string | undefined;
}): ReactNode {
  return (
    <section
      className="mt-[18px] min-w-0"
      data-parent-id={group.parent?.id}
      data-spec-group=""
      role="presentation"
    >
      {group.parent === undefined ? null : <GroupHeading parent={group.parent} />}
      {group.runs.map((run) => (
        <div
          className="grid min-w-0 grid-cols-[280px_minmax(0,1fr)] gap-x-5 max-[901px]:grid-cols-[minmax(0,1fr)]"
          data-spec-run=""
          key={run.statements[0]?.node.id ?? ""}
          role="presentation"
        >
          <DecisionCell
            decision={run.decision}
            focused={openCell !== undefined && openCell === run.statements[0]?.node.id}
            known={known}
            onFocus={(cell, itself) =>
              onCellFocus(cell, run.decision, itself ? run.statements[0]?.node.id : undefined)
            }
            statements={run.statements}
          />
          <div className="flex min-w-0 flex-col" role="presentation">
            {run.statements.map((statement) => drawStatement(statement, run.decision))}
          </div>
        </div>
      ))}
    </section>
  );
}

/** The node a group of statements hangs under, which the filter draws as no row (R2): its kind,
 *  who made it and when, and its first line. */
function GroupHeading({ parent }: { parent: SpecParent }): ReactNode {
  return (
    <div
      className="mb-1 ml-[300px] flex min-w-0 flex-col pl-[26px] max-[901px]:ml-0 max-[901px]:pl-0"
      data-spec-group-heading=""
      role="presentation"
    >
      <p className={`min-w-0 text-xs leading-[18px] [overflow-wrap:anywhere] ${pageQuietText}`}>
        {dotted([
          { key: "kind", part: <span {...pageChrome}>{NODE_TYPE_NAMES[parent.type]}</span> },
          { drawn: parent.by !== undefined, key: "author", part: <Author actor={parent.by} /> },
          { key: "day", part: parent.at === undefined ? null : <Day at={parent.at} /> },
        ])}
      </p>
      <p
        className={`min-w-0 text-[15px] leading-6 font-semibold break-words ${pageInkText}`}
        // A parent with no words is named by its kind's word, which is the page's own, never by
        // its id; any words the parent's author wrote are theirs, whatever they say.
        {...(parent.line === "" ? pageChrome : authorText)}
      >
        {parent.line === "" ? NODE_TYPE_NAMES[parent.type] : parent.line}
      </p>
    </div>
  );
}

/**
 * The decision that shaped a run of statements, in the left column: the words it came from,
 * where they came from and when, and a link to the whole of it (R5). The bracket at its
 * trailing edge spans the statements it shaped, in the decisions' violet, dashed where an agent
 * decided; a run no decision shaped says so in words (R35), once the Spec view read the
 * decisions come from and the asks read hold an answer, and says nothing before that or where
 * either failed with none, since it does not know.
 *
 * A person's words are quoted, their first sentence word for word in plain words with no limit
 * of characters; an agent's are drawn whole as a row draws a node's words (`RowText`), each
 * reference by its name, with no limit of lines, with the focus or without it. Where the words
 * answer a question, the cell gives that question's first sentence whole, ending at its first
 * sentence end as the answer's quote does, also where the group's heading is that question,
 * whose heading draws only its first line (R5). The focus on the cell draws a person's whole
 * answer character for character under its first sentence (R5: "The focus on the cell draws the
 * whole answer character for character"), and "Author unknown" where the model holds no author;
 * `w` on a statement is how a keyboard reader puts it there. The focused cell's words are placed
 * on screen and below the column heads (`useOnScreenUnderHeads`): they are what it draws, and
 * they stay at the top of a run taller than the screen while the cell's own box runs past it.
 * Every link the words hold is out of the tab order, as every control inside a row is (R9).
 */
function DecisionCell({
  decision,
  focused,
  known,
  onFocus,
  statements,
}: {
  decision: SpecDecision | undefined;
  /** Whether the cell itself has the focus, which draws a person's whole answer and "Author
   *  unknown" (`SpecFilterBody`'s `openCell`). */
  focused: boolean;
  /** Whether the cell knows what shaped its run, which is when a run with no decision can say
   *  that none shaped it. */
  known: boolean;
  /** Takes a focus anywhere in the cell, which no statement holds any longer: the cell, and
   *  whether the focus is on the cell itself. */
  onFocus: (cell: HTMLElement, itself: boolean) => void;
  statements: readonly SpecStatement[];
}): ReactNode {
  const words = useRef<HTMLDivElement | null>(null);
  const press = useOnScreenUnderHeads(words, focused);
  const agentText = useAgentText();
  // How the decision stands, with the person its state names: its keeper after "Kept by", and
  // whoever asked to change it after "Changed by" (`standingParts`, R2, R3).
  const standing =
    decision?.state === undefined ? undefined : standingParts(decision.state, decision);
  const rule = decision === undefined ? pageHairBorder : "border-page-decision";
  const cap = decision === undefined ? pageHairBg : pageKindBullet.decision;
  return (
    <div
      aria-level={1}
      className={`relative self-stretch rounded-l-[4px] border-r-2 pt-1 pr-3.5 pb-2 pl-0.5 text-xs leading-[18px] max-[901px]:my-1.5 max-[901px]:rounded-none max-[901px]:border-r-0 max-[901px]:border-l-2 max-[901px]:py-0.5 max-[901px]:pr-0 max-[901px]:pl-2.5 ${
        decision?.said === "agent" ? "border-dashed" : ""
      } ${rule} ${pageMutedText}`}
      data-decision-id={decision?.id}
      // The node the cell is drawn as, which Details draws while the cell has the focus: for a
      // person's decision, drawn by their answer, the question itself.
      data-node-id={decision?.node ?? decision?.id}
      data-said={decision?.said}
      data-spec-cell=""
      data-spec-statements={statements.map((statement) => statement.node.id).join(" ")}
      // An item of the outline, as the statements beside it are: it is where the focus goes
      // from a statement (R5's `w`), and the prototype's own cell takes the focus the same way.
      // A person's whole answer and "Author unknown" are drawn only while the cell itself has
      // the focus, never a control inside it: `f` and a press put the focus on its link, and
      // words growing around the link would move it from where the reader found it.
      onFocus={(event) => onFocus(event.currentTarget, event.target === event.currentTarget)}
      onPointerDown={press}
      role="treeitem"
      tabIndex={-1}
    >
      <span
        aria-hidden="true"
        className={`absolute top-0 -right-0.5 h-0.5 w-2 max-[901px]:hidden ${cap}`}
      />
      <span
        aria-hidden="true"
        className={`absolute -right-0.5 bottom-0 h-0.5 w-2 max-[901px]:hidden ${cap}`}
      />
      <div
        className="sticky top-[calc(var(--issue-identity-height,0px)+44px)] flex min-w-0 flex-col gap-[3px] max-[901px]:static"
        // What the reader sees of the cell, which a hold of the cell keeps where it stands
        // (`holdWhereItStands`).
        data-spec-cell-words=""
        ref={words}
      >
        {decision === undefined ? (
          known ? (
            <p data-spec-nothing="" {...pageChrome}>
              {statements.length === 1
                ? EMPTY_STATE_LINES.spec_no_decision_one
                : EMPTY_STATE_LINES.spec_no_decision_many}
            </p>
          ) : null
        ) : (
          <>
            <p className={`font-semibold ${pageDecisionText}`} data-spec-said="" {...pageChrome}>
              {SAID_WORDS[decision.said]}
            </p>
            {decision.words === undefined ? (
              <p
                className={`min-w-0 text-sm leading-[21px] [overflow-wrap:anywhere] ${pageInkText}`}
                data-spec-quote=""
              >
                <Quote decision={decision} />
              </p>
            ) : (
              // A block, since a text drawn from its reading holds paragraphs, lists and links,
              // drawn whole with no limit of lines (R5) in the words' box the page styles a
              // node's drawn words by (`styles.css`).
              <div className="flex min-w-0 flex-col [overflow-wrap:anywhere]" data-spec-quote="">
                <div className={`text-sm leading-[21px] ${pageInkText}`} data-node-words="">
                  <RowText
                    markdown={decision.words.markdown}
                    source={decision.words.source}
                    tree={decision.words.tree}
                  />
                </div>
              </div>
            )}
            {focused && decision.whole !== undefined ? (
              <p
                className={`min-w-0 text-sm leading-[21px] whitespace-pre-wrap [overflow-wrap:anywhere] ${pageInkText}`}
                data-spec-whole-answer=""
                {...authorText}
              >
                {decision.whole}
              </p>
            ) : null}
            <p className={`min-w-0 [overflow-wrap:anywhere] ${pageQuietText}`} data-spec-source="">
              {dotted([
                {
                  key: "question",
                  part:
                    decision.question === undefined ? null : (
                      <span data-spec-question="" {...authorText}>
                        {/* spec-decision-line: the question as one line of words `specDecidedBy` named. */}
                        {decision.question}
                      </span>
                    ),
                },
                {
                  drawn: decision.by !== undefined || focused,
                  key: "author",
                  part:
                    decision.by === undefined && focused ? (
                      <UnknownAuthor />
                    ) : (
                      <Author actor={decision.by} />
                    ),
                },
                { key: "day", part: decision.at === undefined ? null : <Day at={decision.at} /> },
                {
                  key: "chose",
                  part:
                    decision.chose.length === 0 ? null : (
                      <>
                        <span {...pageChrome}>{SPEC_WORDS.chose} </span>
                        <span {...authorText}>{decision.chose.join(", ")}</span>
                      </>
                    ),
                },
                {
                  key: "state",
                  part:
                    decision.state === undefined ? null : (
                      <span data-node-state={decision.state} {...pageChrome}>
                        {standing?.word}
                        {standing?.person === undefined ? null : (
                          <>
                            {" "}
                            <span
                              data-changed-by={
                                decision.state === "change_requested" ? "" : undefined
                              }
                              data-kept-by={decision.state === "kept" ? "" : undefined}
                              {...authorText}
                            >
                              {agentText.author(standing.person)}
                            </span>
                          </>
                        )}
                      </span>
                    ),
                },
                {
                  key: "unsure",
                  part: decision.unsure ? (
                    <span data-node-unsure="" {...pageChrome}>
                      {CONTROL_LABELS.unsure}
                    </span>
                  ) : null,
                },
              ])}
            </p>
            <WholeOf decision={decision} />
          </>
        )}
      </div>
    </div>
  );
}

/** The link to the whole of a decision: the answer a person wrote, or the decision itself. It
 *  goes to the node's own address, which is where every other link of this page goes (`Chip`),
 *  and is out of the tab order as every control inside a row is (R9). */
function WholeOf({ decision }: { decision: SpecDecision }): ReactNode {
  const route = routeOf(decision);
  if (route === undefined) return null;
  return (
    <Link
      className={`${INLINE_LINK} self-start text-left font-semibold ${pageInkText}`}
      data-spec-whole=""
      tabIndex={-1}
      to={buildReferencePath(route)}
      {...pageChrome}
    >
      {decision.type === "ask" ? SPEC_WORDS.whole_answer : SPEC_WORDS.whole_decision}{" "}
      <span aria-hidden="true">›</span>
    </Link>
  );
}

interface SpecRowProps {
  editable: boolean;
  editing: string | undefined;
  focused: boolean;
  /** Takes a focus anywhere in the row, once a press that gave it is over. */
  onFocus: (row: HTMLElement) => void;
  /** What a key pressed on this row reported when it had nowhere to go. */
  report?: string;
  rowKey: string | undefined;
  tabStop: boolean;
}

/**
 * One statement: its bullet, its words with the chips it carries after the last of them, the
 * line naming the decision that shaped it, and at the right end of its first line the count of
 * the checks that serve it (`CheckCount`) and its mark (R5, R6, R17, R34). Its words are whole,
 * with no limit of lines, with the focus or without it (R6): the view is where a statement is
 * read, so it draws every word of it, as its author wrote it. Its chips stand on the words' own
 * line after their last word, as a row of the outline draws its trail (`NodeRow.tsx`). While
 * editing, the same document stage replaces the words and the chips move below it.
 */
function Statement({
  context,
  decision,
  editable,
  editing,
  focused,
  marked,
  onFocus,
  report,
  rowKey,
  statement,
  tabStop,
  wordsPress,
}: SpecRowProps & {
  context: boolean;
  decision: SpecDecision | undefined;
  /** Whether the reads its mark comes from hold an answer. */
  marked: boolean;
  /** Whether a press on the statement's words gives it focus without placing it. */
  wordsPress: boolean;
  statement: SpecStatement;
}): ReactNode {
  const node = statement.node;
  const row = useRef<HTMLDivElement | null>(null);
  const words = useRef<HTMLDivElement | null>(null);
  const stage = useDocumentStage(editing);
  const press = useOnScreenUnderHeads(row, focused, editing === undefined && !wordsPress);
  useOutOfTabOrder(words, editing === undefined);
  // The count of the checks that serve it, drawn only where one does, and where its reference
  // names the block the Verification view puts the focus on.
  const checks = node.counts?.checks ?? 0;
  const toChecks = checks > 0 ? verificationPath(node) : undefined;
  // Every gap and chip on the statement but the gap its mark already says: "No check" is the
  // mark at the right of the row and the word for it in the legend (R17), and a chip beside it
  // would say it twice. The rest are drawn as the outline draws them (R4, R10).
  const edged = { ...node, gaps: node.gaps.filter((gap) => gap !== "no_check") };
  const edges = edged.alsoUnder.length > 0 || edged.chips.length > 0 || edged.gaps.length > 0;
  // The chips at the end of the words, on their line, as a row of the outline draws its trail
  // (`NodeRow.tsx`).
  const trail = edges ? (
    <span
      className={`ml-1.5 inline-flex max-w-full flex-wrap items-baseline gap-x-2 align-baseline text-xs leading-6 ${pageQuietText}`}
      data-node-trail=""
    >
      <NodeEdges node={edged} />
    </span>
  ) : null;
  return (
    <div
      aria-level={1}
      className={`grid min-w-0 ${
        toChecks === undefined ? STATEMENT_COLUMNS.plain : STATEMENT_COLUMNS.counted
      } items-start gap-x-2 rounded-md py-[3px] pr-1.5 outline-none max-[761px]:gap-x-1.5 ${
        focused ? pageFocusedRow : ""
      }`}
      data-editable={editable ? "" : undefined}
      data-node-context={context ? "" : undefined}
      data-node-id={node.id}
      // The attributes every row of the outline carries, so the page's own readers find a
      // statement of this body as they find a row of the tree (`e2e/issue-page.ts`'s
      // `drawnRows` and `nodeRow`), and the outline's keys walk it (`useTreeKeymap.ts`).
      data-node-row=""
      data-node-type={node.type}
      data-row-key={rowKey}
      data-spec-mark={marked ? statement.mark : undefined}
      data-spec-statement=""
      // Any focus inside the row is the reader on this statement - a chip or a link of its
      // words as much as the row itself - as a row of the outline takes it (`NodeRow.tsx`), and
      // only once a press that gave it is over (`usePressFocus`).
      onFocus={(event) => onFocus(event.currentTarget)}
      onPointerDown={press}
      ref={row}
      role="treeitem"
      tabIndex={tabStop ? 0 : -1}
    >
      <span className="flex h-6 w-[18px] items-center justify-center max-[761px]:w-4">
        <i
          aria-hidden="true"
          className={`block size-[7px] rounded-full ${
            context ? pageKindBullet.context : pageKindBullet.statement
          }`}
        />
      </span>
      <div className="flex min-w-0 flex-col">
        <div
          className={`text-[15px] leading-6 ${context ? pageQuietText : pageWordsText}`}
          data-node-words=""
          ref={words}
        >
          {editing !== undefined ? (
            <div className="flex min-w-0 flex-col" ref={stage} {...authorText} />
          ) : (
            <>
              <RowText markdown={node.text} source={node.source} tree={node.tree} />
              {/* A direct child, so the styles draw the trail after the last word. */}
              {trail}
            </>
          )}
        </div>
        {editing !== undefined && trail !== null ? (
          <div className="flex min-w-0 flex-wrap items-baseline">{trail}</div>
        ) : null}
        <Because decision={decision} statement={statement} />
        {/* The constraints the statement names, as the outline draws them: out of the tab
            order, as the row's other controls are. */}
        <LensLines node={node} tabStops={false} />
        <RowReport line={report} />
      </div>
      {toChecks === undefined ? null : <CheckCount count={checks} to={toChecks} />}
      {/* No mark until its reads hold an answer; the row's last column keeps its width, so
          the row does not move when the mark comes. */}
      {marked ? (
        <NodeGlyph
          className={`mx-auto mt-[5px] size-3.5 shrink-0 ${MARK_TONES[statement.mark]}`}
          token={markToken(statement.mark)}
        />
      ) : null}
    </div>
  );
}

/** The columns of a statement's row: its bullet, its words, which take whatever the row leaves
 *  (`minmax(0,1fr)`) so no run of words widens the row past its box, then its count of checks,
 *  as wide as its own words, where it draws one, and its mark; each narrower below 761 px. */
const STATEMENT_COLUMNS = {
  counted:
    "grid-cols-[18px_minmax(0,1fr)_auto_26px] max-[761px]:grid-cols-[16px_minmax(0,1fr)_auto_22px]",
  plain: "grid-cols-[18px_minmax(0,1fr)_26px] max-[761px]:grid-cols-[16px_minmax(0,1fr)_22px]",
} as const;

/**
 * The count of the checks that serve a statement, at the right end of its first line before its
 * mark (R6): the coverage read's `checks` for it, which the tree carries as the node's
 * `counts.checks` (`tree-drafts.ts`'s `addRequirements`), as a number and the word for one check
 * or for several. It is a link to the Verification view with the focus on the statement's row
 * (`verificationPath`), out of the tab order as every control inside a row is (R9), so the
 * tree's `f` reaches it. It never wraps, and its column is only as wide as it while the words
 * take what the row leaves, so it makes no row scroll sideways at 320 px.
 */
function CheckCount({ count, to }: { count: number; to: string }): ReactNode {
  return (
    <Link
      className={`${INLINE_LINK} text-xs leading-6 whitespace-nowrap ${pageQuietText}`}
      data-spec-checks={count}
      tabIndex={-1}
      to={to}
    >
      <ChecksNumber count={count} />
    </Link>
  );
}

/** The address of the Verification view with the focus on `node`'s row: that view's own address
 *  on the issue that holds the node (`presetPath`), with the node's block as its fragment
 *  (`/verification#b-<block id>`), as the address of a block in the conversation is built
 *  (`conversationPath`); undefined where its reference names no block of an issue. */
function verificationPath(node: Pick<TreeNode, "reference">): string | undefined {
  const route = node.reference === undefined ? undefined : parseNodeReference(node.reference);
  if (route === undefined || !("key" in route)) return undefined;
  const block = "block" in route ? route.block : undefined;
  return block === undefined
    ? undefined
    : `${presetPath(route.key, "verification")}${blockFragment(block.id)}`;
}

/** A row the filter kept that is no statement: a node an address named, which outranks the
 *  filter (R20). It is drawn by its kind and its words in the statements' column, with no mark
 *  and no decision beside it. Its words are whole, with the focus or without it, and their links
 *  stay out of the tab order. A key with nowhere to go says so under its words, as on a statement. */
function OtherRow({
  editable,
  editing,
  focused,
  node,
  onFocus,
  report,
  rowKey,
  tabStop,
}: SpecRowProps & { node: TreeNode }): ReactNode {
  const row = useRef<HTMLDivElement | null>(null);
  const words = useRef<HTMLDivElement | null>(null);
  const stage = useDocumentStage(editing);
  const press = useOnScreenUnderHeads(row, focused, editing === undefined);
  useOutOfTabOrder(words, editing === undefined);
  return (
    <div
      aria-level={1}
      className={`mt-[18px] ml-[300px] min-w-0 rounded-md py-[3px] pr-1.5 pl-[26px] outline-none max-[901px]:ml-0 max-[901px]:pl-0 ${
        focused ? pageFocusedRow : ""
      }`}
      data-editable={editable ? "" : undefined}
      data-node-id={node.id}
      data-node-row=""
      data-node-type={node.type}
      data-row-key={rowKey}
      data-spec-other=""
      // Any focus inside the row is the reader on it, as on a statement.
      onFocus={(event) => onFocus(event.currentTarget)}
      onPointerDown={press}
      ref={row}
      role="treeitem"
      tabIndex={tabStop ? 0 : -1}
    >
      <p className={`text-xs leading-5 ${pageQuietText}`} {...pageChrome}>
        {NODE_TYPE_NAMES[node.type]}
      </p>
      {editing !== undefined ? (
        <div className="flex min-w-0 flex-col" ref={stage} {...authorText} />
      ) : (
        <div className={`text-[15px] leading-6 ${pageWordsText}`} data-node-words="" ref={words}>
          <RowText markdown={node.text} source={node.source} tree={node.tree} />
        </div>
      )}
      <RowReport line={report} />
    </div>
  );
}

/** What a key pressed on a row reported when it had nowhere to go, under the row's words, in the
 *  words the outline says it in (the tester's F16, `NodeRow`'s `report`); nothing where it
 *  reported nothing. */
function RowReport({ line }: { line: string | undefined }): ReactNode {
  if (line === undefined) return null;
  return (
    <p
      className={`text-xs leading-5 ${pageMutedText}`}
      data-row-report=""
      role="status"
      {...pageChrome}
    >
      {line}
    </p>
  );
}

/**
 * The one line under a statement that names the decision that shaped it (R5, the tester's F7):
 * after "because:", the question's first sentence whole where the decision answers a question,
 * as the cell gives it (`SpecDecision.question`); then the words it came from, as the link to
 * the whole of that decision; who decided and when; "Changed since" where the statement's words
 * changed after the tie that names that decision (`SpecStatement.changedSince`); and then every
 * other decision that informed it, each a link to it. A statement no decision shaped draws no
 * line here: its cell says so beside it (R35). The quote is plain words, an agent's its first
 * line: the link is one line, and a text drawn with its own links can stand inside no link.
 */
function Because({
  decision,
  statement,
}: {
  decision: SpecDecision | undefined;
  statement: SpecStatement;
}): ReactNode {
  if (decision === undefined && statement.alsoShapedBy.length === 0) return null;
  const route = routeOf(decision);
  const quoted = decision === undefined ? null : <Quote decision={decision} />;
  // What the line says after the quote: who decided, when, and that the statement changed since.
  const changed = statement.changedSince;
  const after = decision?.by !== undefined || decision?.at !== undefined || changed;
  return (
    <p
      className={`mb-0.5 min-w-0 text-xs leading-[18px] [overflow-wrap:anywhere] ${pageQuietText}`}
      data-spec-because=""
    >
      {decision === undefined ? null : (
        <>
          <span {...pageChrome}>{SPEC_WORDS.because} </span>
          {decision.question === undefined ? null : (
            <>
              <span data-spec-question="" {...authorText}>
                {/* spec-decision-line: the question as one line of words `specDecidedBy` named. */}
                {decision.question}
              </span>{" "}
            </>
          )}
          {route === undefined ? (
            quoted
          ) : (
            // Out of the tab order, as every control inside a row is (R9): `f` steps to it.
            <Link
              className={INLINE_LINK}
              data-spec-because-link=""
              tabIndex={-1}
              to={buildReferencePath(route)}
            >
              {quoted}
            </Link>
          )}
          {after ? <span {...pageChrome}> — </span> : null}
          {dotted([
            {
              drawn: decision.by !== undefined,
              key: "author",
              part: <Author actor={decision.by} />,
            },
            { key: "day", part: decision.at === undefined ? null : <Day at={decision.at} /> },
            {
              key: "changed",
              part: changed ? (
                <span data-spec-changed-since="" {...pageChrome}>
                  {SPEC_WORDS.changed_since}
                </span>
              ) : null,
            },
          ])}
        </>
      )}
      {statement.alsoShapedBy.length === 0 ? null : (
        <>
          <span {...pageChrome}>
            {decision === undefined ? "" : " · "}
            {SPEC_WORDS.informed_by}{" "}
          </span>
          {statement.alsoShapedBy.map((informed, place) => (
            <Fragment key={informed.id}>
              {place === 0 ? null : <span {...pageChrome}>, </span>}
              <Informed decision={informed} />
            </Fragment>
          ))}
        </>
      )}
    </p>
  );
}

/** One other decision that shaped the statement, named by its own words as a chip names its
 *  target (`SpecDecision.chip`, cut before its references are named), as a link to it, then
 *  when it was made: the model holds no short name for a decision, and a block id is an
 *  identifier (R22). A decision with no words is named by its kind's word, so no link is
 *  empty. */
function Informed({ decision }: { decision: SpecDecision }): ReactNode {
  const route = routeOf(decision);
  const words = decision.chip;
  const named =
    words === "" ? (
      <span {...pageChrome}>{NODE_TYPE_NAMES[decision.type]}</span>
    ) : (
      <span {...authorText}>{words}</span>
    );
  return (
    <>
      {route === undefined ? (
        <span data-spec-informed="">{named}</span>
      ) : (
        <Link
          className={INLINE_LINK}
          data-spec-informed=""
          tabIndex={-1}
          to={buildReferencePath(route)}
        >
          {named}
        </Link>
      )}
      {decision.at === undefined ? null : (
        <>
          <span {...pageChrome}> · </span>
          <Day at={decision.at} />
        </>
      )}
    </>
  );
}
