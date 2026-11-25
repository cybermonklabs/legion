import {
  type FocusEvent,
  type KeyboardEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useNavigate } from "react-router-dom";

import type { Artifact, IssueDetails } from "../../../api/types";
import {
  focusVisibleRing,
  pageInkText,
  pageMutedText,
  pageQuietText,
  textMutedOnCanvas,
} from "../../../theme/classes";
import { useAgentText } from "../../refs/AgentName";
import {
  PublishedReferenceNames,
  type PublishedReferences,
  useKnownReferences,
} from "../../refs/reference-names";
import { parseNodeReference } from "../../refs/routes";
import { focusOnDocument } from "../../shell/roving";
import { useSetFocusedNode } from "../focused-node";
import { focusedNodeOf, type OutlineRequest, useReleaseFocus } from "../IssueTree";
import { documentOfNode } from "../issue-artifacts";
import { chipName, type TreeNode } from "../issue-tree";
import type { OutlineReads } from "../outline-reads";
import { useTakeOutlineRequest } from "../outline-request";
import { threadMarks } from "../outline-thread-read";
import { useOutlineWriter } from "../outline-writer";
import { authorText, KEY_REPORTS, pageChrome } from "../page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  DECISIONS_WORDS,
  isReader,
  PRESET_NAMES,
  STATE_LINES,
} from "../page-words";
import { usePressFocus } from "../press-focus";
import { FAILED_READ_RETRY, FailedReadLine } from "../slots/failed-read-line";
import { focusIsLoose } from "../tree-focus";
import type { DrawnRow } from "../tree-fold";
import { nodesInOrder, rowKeyOf } from "../tree-keys";
import { type OnEditing, useNodeEditing, useWordsPress } from "../useNodeEditing";
import { useTreeKeymap } from "../useTreeKeymap";
import { DecisionsRow } from "./DecisionsRow";
import { useDecisionsEntries } from "./decisions-entries";
import {
  conversationPath,
  DECISIONS_PART_ORDER,
  type DecisionsEntry,
  type DecisionsPart,
  type NamedEntry,
  waitingOrder,
} from "./decisions-parts";
import { entryStanding } from "./entry-standing";

/** The three parts drawn under a heading of their own, in the view's order; the fourth, the
 *  foot, is headed by the control that unfolds it. */
const HEADED = ["open_questions", "review", "yours"] as const satisfies readonly DecisionsPart[];

/** What a key that folds, zooms or opens a branch does here, where the list holds none. */
const NOTHING = (): void => {};

/**
 * The Decisions filter's body, drawn as the picture `page-v3-decisions-1440.png` and the
 * prototype's `.decs` draw it.
 *
 * It is one list in four parts, each under its own heading and in this order: the open
 * questions; the decisions that wait for the reader's review, each with Keep and Change; the
 * reader's decisions, newest first, where another person's entry stands too, with that person's
 * name on its row; and, folded at the foot, the agents' decisions that no statement rests on.
 * Which entry stands in which part is `decisions-parts.ts`, which is pure, read through the seam
 * (`decisions-entries.ts`); this draws it. A part that holds nothing is not drawn, and no
 * sentence explains the parts: their headings are the whole of it (R22). Before the parts
 * stands the node an address names where it is neither a question nor a decision, a comment
 * say, as a row with no heading and no count: the address outranks the view (`presets.ts`),
 * and the four parts stay all the list says. It is drawn the same at every width: the rows are
 * the outline's own (`DecisionsRow.tsx`).
 *
 * The list is one landmark named by the filter, and inside it answers the WAI-ARIA tree pattern
 * as the outline does, under the outline's own name, so a reader and the browser suite find one
 * tree on the page whatever filter is on: each part is a group named by its heading and each
 * entry a row of the tree. The focused row is the list's one tab stop and nothing inside a row
 * takes the tab order (R9), but for a block of the focused row's words whose content runs past
 * its box, a wide code block or table, which keeps a tab stop of its own so that a reader on the
 * keyboard can scroll it, as on the outline's rows (`RowText.tsx`). The keys are the outline's
 * own (`useTreeKeymap.ts`): the arrows and `j`/`k` move, `Shift+K` and `Shift+C` press Keep and
 * Change, `a` and `r` answer and reply, `f` steps through a row's links, Enter puts the caret in
 * the entry's own text (below), and the key that cycles how much is drawn unfolds the foot.
 * `q` and `w` walk the entries in the order the list draws them and then the rest of the issue,
 * so a question or a decision the words filter left out is still found, and opens in the
 * conversation, as an entry's link does. The foot's own control is a plain button in the tab
 * order, as the prototype's is, so Tab reaches it where the foot is all the list holds. The tree
 * element keeps no focus while the list draws a row, nor while it draws the foot once the reads
 * that say which questions and decisions there are hold their answers: a focus on it goes on to
 * the tab stop, or to the foot's control where the list draws no row, which is how `t d` reaches
 * a list that holds only the foot. A page opened on one reaches that control by the same rule
 * where nothing holds the focus: on the page itself, or on the app's main element, which holds
 * the list and which the app focuses when a link inside it opens the page (`focusIsLoose`). On
 * an issue the view draws nothing for, with no row and no foot, there is nowhere to pass it, and
 * the tree keeps it. A key with nowhere to go says so at the row it was pressed on, or at the
 * list's tab stop where it was pressed on no row, as the outline's keys do (the tester's F16):
 * Left on a row of the parts says the list has no level above it; on a row of the foot, which
 * stands a level down, under the foot's control, it goes up to that control, as Left goes to a
 * row's parent in the outline. Until the reads that say which questions and decisions there are
 * hold their answers, `q` and `w` with nowhere to go say nothing: no question or decision to go
 * to may mean only that they have not arrived (`decisions-entries.ts`).
 *
 * A row is edited in place: Enter on it, or a press on its words, puts the caret in the text of
 * the node it draws, in the document that text is written in, through the editing the
 * outline's rows use (`useNodeEditing.ts`). The page draws that document's one editor
 * (`onEditing`), and the row draws it on its stage in place of its words while it is the row
 * edited; Escape leaves the caret for the row. A press on a link or a control inside a row is
 * that control's own, and a press on its words puts the caret in only where the row takes one
 * (`takesCaret`), where it else only gives the row the focus. Where a node has no text to put a
 * caret in - an ask, a comment, a message or a result, or a node of another issue - Enter says
 * why at the row.
 *
 * A read that says which entries there are and stands refused - the decisions, the asks - draws
 * the page's one line for a failed read over the list (`slots/failed-read-line.tsx`), with a
 * Retry that asks that read alone again, in place of the entries it would give. Where the list
 * draws no row, the stage the view is drawn in (`slots/stage.tsx`) gives a Retry the focus the
 * tree holds or nothing holds, whether the read is refused before `t d` or after, and keeps the
 * focus in the list when a line leaves with it; where rows are drawn the tab stop takes it, and
 * Shift+Tab from a row goes up to the Retry. So the foot's control takes no focus until those
 * reads have both answered: until then a row or a Retry may yet come to take it.
 *
 * After those lines stands the outline's names line while the page's names reads stand failed
 * (`namesLine`), with a Retry that asks those reads again: every reference a row draws waits on
 * those names, so while they stand failed a reference in an entry's words is drawn as its address
 * and a statement on a decision's "informs" line is named by nothing; the line says why. The
 * names place no entry, so their line takes none away.
 *
 * The reads are the seam's: the ones the page's tree is built from, under the keys it reads them
 * by (`useIssueTree.ts`), so the body costs no request of its own (R20), and the names its rows
 * draw are the page's batched read's, which the body hands every row in the render that draws it
 * (`published`), so no row reads a reference the page asks for.
 */
export function DecisionsFilterBody({
  artifacts,
  emptyLine,
  holdsEscape,
  isClosed,
  issue,
  namesLine,
  onEditing,
  onRevealDrawn,
  published,
  reader,
  request,
  reveal,
  root,
  view,
  waitingPress,
}: {
  /** Each artifact of the issue by its slug, which is how a decision names the document a Keep
   *  is written against (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** What the page says where the issue holds no ask and no decision the filter draws, once
   *  the reads that say which asks and decisions there are hold their answers. */
  emptyLine: string;
  /** Whether the page takes Escape from a row before the list is left: a filter by words to
   *  clear or a view to turn off comes first in the key table's order
   *  (`slots/view-decisions.tsx`). */
  holdsEscape: boolean;
  /** Whether the issue is closed, which Keep and Change are refused on. */
  isClosed: boolean;
  issue: IssueDetails;
  /** The names' line while the page's names reads stand failed (`outline-reads.ts`'s
   *  `useOutlineReads`), the outline's own: its words and a Retry that asks those reads again, and
   *  none while they do not stand failed. Every reference the rows draw waits on those names
   *  (`published`), so while they stand failed each is drawn as its address; the line says why.
   *  The body draws it over the list after the lines of its own reads, as the outline draws it
   *  after the lines of the reads its rows come from. */
  namesLine: OutlineReads["names"];
  /** Tells the page which document a row is being edited in, so it draws that document's one
   *  editor and hands it to the row (`useNodeEditing.ts`): `IssuePresetPanels.tsx`'s
   *  `useOutlineEditing`, whose panels the slot draws after the body
   *  (`slots/view-decisions.tsx`). */
  onEditing: OnEditing;
  /** Told whether the list draws the node the address names (`reveal`), where the page draws
   *  the outline in the view's place if it does not (`slots/view-decisions.tsx`): given only
   *  while every read the tree is built from holds its answer. The body tells it as soon as it
   *  can say, and again each time what it draws changes; the stage acts on the first of these
   *  for each following of an address that names a node beside the view (`slots/stage.tsx`). */
  onRevealDrawn?: (drawn: boolean) => void;
  /** What the page publishes of its one batched names read (`useIssueTree.ts`'s `published`),
   *  which the body hands every row it draws in the render that draws it
   *  (`PublishedReferenceNames`), as the outline does (`IssueTree`'s `published`): a reference
   *  the page is asking for waits on the page's answer from a row's first render, rather than
   *  being read again by the row (R8). */
  published: PublishedReferences;
  /** Who is reading, whose own answer carries the ink rule (R34). Another person's decision
   *  stands under "Your decisions" too, with that person's name on its row. */
  reader: string | undefined;
  /** What the page last asked of the outline: go to a node, which the Contents pane's list
   *  asks (`outline-request.tsx`). Each ask is acted on once. */
  request?: OutlineRequest;
  /** The node the address names, which takes the focus once per address (R19): its own row, or
   *  for a person's write-up of an answered question, which is no row here, the row of a
   *  question it answers, where the list draws one. Undefined where the address names no node
   *  or the tree holds none it names. */
  reveal?: string;
  /** The whole tree, which holds each ask and each decision and each statement a tie names. */
  root: TreeNode;
  /** The tree as the Decisions filter leaves it (`presetView`). */
  view: TreeNode;
  /** The press of the header's count of what waits on the reader that turned this filter on
   *  (`waiting.ts`'s `waitingPressOf`): each press puts the focus on the next thing that waits,
   *  in the filter's order (R13, the tester's F12). */
  waitingPress?: number;
}): ReactNode {
  const navigate = useNavigate();
  const { failures, parts, readsHeld, retry, retrying, unreadable, writeUps } = useDecisionsEntries(
    { issue, reader, root, view }
  );
  // The page's one rule for naming a person (`AgentText.author`), which the panel's facts of the
  // focused row name its keeper by: a name, never an id (R9, R22).
  const agentText = useAgentText();
  // Where each thread of the issue is marked read in the reader's Inbox: its newest write's row.
  const threads = useMemo(() => threadMarks(root), [root]);
  // The name each statement takes on a decision's "informs" line, as every chip of the page
  // names a block (`chipName`), so a statement reads here as the outline's chip for it reads: by
  // the line the names store projects beside its name from the reading the page published for
  // it, its first block that holds words (`KnownReferences.lines`), cut to a chip's room, which
  // each row draws as it is. One the store holds no line for yet is named by nothing until it
  // does, as a chip draws nothing until its name comes. A statement drawn under two parents is
  // one node, so its first placement in the page's reading order names it.
  const { lines } = useKnownReferences();
  const statementNames = useMemo<ReadonlyMap<string, string>>(() => {
    const names = new Map<string, string>();
    for (const node of nodesInOrder(root)) {
      const reference = node.type === "requirement" ? node.reference : undefined;
      if (reference === undefined || names.has(node.id)) continue;
      const line = lines.get(reference);
      if (line !== undefined) names.set(node.id, chipName(parseNodeReference(reference), line));
    }
    return names;
  }, [lines, root]);
  const setFocusedNode = useSetFocusedNode();
  const writer = useOutlineWriter();
  const body = useRef<HTMLElement | null>(null);
  const tree = useRef<HTMLDivElement | null>(null);
  const footControl = useRef<HTMLButtonElement | null>(null);
  const headings = useId();

  const [focused, setFocused] = useState<string | undefined>(undefined);
  // Where the list's one tab stop rests, which outlives the focus leaving it (R9).
  const [stop, setStop] = useState<string | undefined>(undefined);
  const [pinned, setPinned] = useState<ReadonlySet<string>>(() => new Set());
  const [footShown, setFootShown] = useState(false);
  // The key that cycles how much is drawn: the list as it opens, then everything - the foot
  // unfolded and every body open - then the list as it opens again.
  const [everything, setEverything] = useState(false);
  // The node a key or the page is taking the reader to, once its row is drawn.
  const [wanted, setWanted] = useState<string | undefined>(undefined);
  // What a key reported when it had nowhere to go, and the row it was pressed on, which draws it
  // until the reader moves on (`NodeRow`'s `report`). `row` is unset where the key was pressed on
  // no row - on the foot's control, or with the focus on the page - and the list's tab stop
  // draws it then, as the outline places one (`IssueTree.tsx`); a list with no tab stop draws
  // none. The control's own focus clears a report, so one made while the focus is already on it
  // stays. The outline's report for a key that would go above the top level speaks of the whole
  // issue, which this list is not, so here it says the list has no level above the row.
  const [report, setReport] = useState<{ readonly line: string; readonly row?: string }>();
  const reportHere = useCallback((line: string) => {
    const said = line === KEY_REPORTS.whole_tree ? ANSWER_LINES.no_level_above : line;
    const row = document.activeElement?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
    setReport(said === "" ? undefined : { line: said, ...(row === undefined ? {} : { row }) });
  }, []);

  // The rows in the order the page draws them, which is the order the outline's keys walk: the
  // node the address names first, then the parts, then the foot's rows while it is unfolded.
  const drawn = useMemo<readonly (DecisionsEntry | NamedEntry)[]>(
    () => [
      ...parts.named,
      ...parts.open_questions,
      ...parts.review,
      ...parts.yours,
      ...(footShown ? parts.agents : []),
    ],
    [footShown, parts]
  );
  // The rows as the outline's keys read them: the nth is the nth row drawn. Each sits at the
  // list's one level, under the root the keys are given, which is the view (`keyOrder`), so Left
  // on a row of the parts finds no row above it and says so, as on a top-level row of the
  // outline. Left on a row of the foot is the body's own, taken before the keys read it
  // (`footRowLeft`).
  const rows = useMemo<DrawnRow[]>(
    () => drawn.map((entry) => drawnRowOf(entry, view.id)),
    [drawn, view.id]
  );

  // The decision the reader last pressed Keep on, or sent a Change on, on its row here. A press
  // of Keep or a send of Change arms it, and only these let it go: a move to another row, the
  // focus leaving the list or going to the foot's control (`footFocused`), and the decision
  // leaving the review list. The press is no review of its own: the decision leaving the review
  // list moves the focus on only where it then stands kept by the reader or with their change,
  // so a write the server refused moves nothing, and it does not let the press go either. So
  // where the reader stays on the row after a refused Keep, their own Keep or Change of that
  // decision made elsewhere, in another tab say, then moves the focus on, as it should: the
  // decision left the review list by their own review.
  const ownReview = useRef<string | undefined>(undefined);
  const takeFocus = useCallback(
    (entry: DecisionsEntry | NamedEntry) => {
      const node = entry.node;
      setFocused(node.id);
      setStop(node.id);
      if (ownReview.current !== node.id) ownReview.current = undefined;
      // A move to a row is the reader going on from what a key reported.
      setReport((held) => (held?.row === node.id ? held : undefined));
      // The panel's facts of the focused node are the outline's, but for how the node stands,
      // which they say as the row reads it, so the two never differ: an entry the row reads no
      // standing for says none, whatever state its node holds.
      const { state: _nodeState, ...facts } = focusedNodeOf(node);
      const state = entryStanding(entry, agentText.author)?.words;
      setFocusedNode(state === undefined ? facts : { ...facts, state });
    },
    [agentText, setFocusedNode]
  );
  // R7 under a press. A press focuses what it lands on as it goes down, before it ends, so the
  // row taking the focus would open its body and draw its acts, and the row the focus left would
  // close its own, while the press is held: every row under the one that closed would move up,
  // a cut answer or decision of the pressed row would be drawn whole and push down what stands
  // under its words, and the click that ends the press would land on something else, so a link
  // or a control of a row the reader was not on would never fire. So the focus a press gives a
  // row is taken once the press is over, and only while that row still holds the focus, which
  // `usePressFocus` does for the outline and this list alike; a key's move takes it at once. The
  // press is marked on the whole list, since the foot's rows stand outside the tree element.
  const press = usePressFocus<DecisionsEntry | NamedEntry>(
    takeFocus,
    (entry) => rowOf(body.current, entry.node.id)?.contains(document.activeElement) === true
  );
  // A row's node is edited in place, in the document its text is written in, by the outline's
  // own editing (`useNodeEditing.ts`): Enter on a row (`onEdit` of the keys below) and a press
  // on a row's words (`pressWords`) ask for it, the page draws that document's one editor
  // (`onEditing`), and the row whose key `editing` holds draws it on its stage in place of its
  // words. The editing finds the row by its key (`data-row-key`) inside the list's own element,
  // which holds the foot's rows too, since they stand outside the tree element. Where a node
  // holds no text to put a caret in, Enter says why at the row (`reportHere`). The list keeps no
  // note of the reader's own edit (`onOwnEdit`): the outline keeps one to tell a read that drops
  // the edited row from another person's, while here an entry a read takes out of the list
  // passes its focus on by the rule for one that leaves the page (the effect on `partOf`
  // below), whoever wrote the words.
  const { edit, editing, stopEditing } = useNodeEditing({
    artifacts,
    container: body,
    onEditing,
    onOwnEdit: () => {},
    onReport: reportHere,
  });
  // A press on a row's words asks for the caret where the row takes one, by the outline's one
  // rule (`takesCaret`): over the room of the issue the page's writer is of, and what each
  // document's room admitted the page to. A row that does not is only given the focus by the
  // press, as every row is, and nothing is said: Enter is what says why.
  const pressWords = useWordsPress({ artifacts, edit, isClosed, onReport: reportHere });
  // The focus leaving the list with no press under way, by Tab or a script, lets go at once of
  // what the row opened, what a key reported and the reader's own review. A press on another
  // control of the page keeps them until that press has ended and its click has landed, or until
  // Escape, so nothing moves under the pointer (`useReleaseFocus`, decision 105 as amended). The
  // panel goes back to the issue's own facts as the focus leaves, unless it went to Details,
  // where a reader reads the node's. A row being edited keeps them, as the outline's does, until
  // the editing ends.
  const leave = useCallback(() => {
    setFocused(undefined);
    setReport(undefined);
    ownReview.current = undefined;
  }, []);
  const releaseFocus = useReleaseFocus({ editing: editing !== undefined, onLeave: leave });
  // The foot's control is inside the list, so the focus going to it from a row, by Tab or a
  // press, never leaves the list and `releaseFocus` does not act. The focus is off every row
  // all the same, so it does what the list's leave does, and the panel goes back to the issue's
  // own facts. A refused read's line is inside the list too, and a focus on it, by Shift+Tab
  // from a row say, is off every row the same way.
  const footFocused = useCallback(() => {
    leave();
    setFocusedNode(undefined);
  }, [leave, setFocusedNode]);
  const pin = useCallback((id: string) => {
    setPinned((held) => {
      const next = new Set(held);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  }, []);

  // The part each entry stands in, read over the whole list, the foot's whether it is unfolded
  // or not, in the order the page draws it.
  const partOf = useMemo(() => {
    const parted = new Map<string, DecisionsPart>();
    for (const part of DECISIONS_PART_ORDER) {
      for (const entry of parts[part]) parted.set(entry.node.id, part);
    }
    return parted;
  }, [parts]);
  // Takes the reader to one entry: the foot is unfolded where the entry is one of its own, and
  // the row takes the focus once this render has drawn it.
  const goTo = useCallback(
    (id: string) => {
      if (partOf.get(id) === "agents") setFootShown(true);
      setWanted(id);
    },
    [partOf]
  );
  useEffect(() => {
    if (wanted === undefined) return;
    setWanted(undefined);
    rowOf(body.current, wanted)?.focus();
  }, [wanted]);

  // What the page asked for, each ask acted on once. The page holds what an outline or a view
  // took, so a view drawn again does not act on the last ask twice (`outline-request.tsx`).
  const takeRequest = useTakeOutlineRequest();
  useEffect(() => {
    if (request === undefined || !takeRequest(request)) return;
    if (request.id !== undefined) goTo(request.id);
  }, [goTo, request, takeRequest]);

  // The header's count of what waits on the reader puts the focus on a thing that waits, in
  // this filter's order: each open question whose turn is the reader's, then each decision of
  // the review list (R13). Each press goes to the next of them after the one the last press
  // went to, and round to the first again, so a reader works down what waits with the one
  // control (the tester's F12). The reads that hold it may still be out when the press lands,
  // so the press is taken once the list holds something that waits.
  const pressed = useRef<number | undefined>(undefined);
  const went = useRef<string | undefined>(undefined);
  const waiting = useMemo(() => waitingOrder(parts), [parts]);
  useEffect(() => {
    if (waitingPress === undefined || waitingPress === pressed.current) return;
    if (waiting.length === 0) return;
    pressed.current = waitingPress;
    const next = waiting[(waiting.indexOf(went.current ?? "") + 1) % waiting.length];
    if (next === undefined) return;
    went.current = next;
    goTo(next);
  }, [goTo, waiting, waitingPress]);

  // An address that names an entry puts the focus on it, once per address (R19), and one that
  // names a node of another kind, a comment say, on that node's row, drawn first: the row is
  // not on the page until the reads that hold it have answered, so this looks again on every
  // list the body draws, and unfolds the foot where the entry is one of its own. A person's
  // write-up of an answered question is no entry (`writeUps`), so an address naming one puts the
  // focus on the first row the list draws of a question it answers, and lands nowhere where the
  // list draws none of them. A decision the list draws before the asks read has answered may
  // yet turn out to be a write-up, which leaves the list once it does, so the address lands on
  // no decision until the seam can tell.
  const taken = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (reveal === undefined || taken.current === reveal) return;
    const restated = writeUps?.get(reveal);
    const entry =
      restated === undefined
        ? drawn.find((each) => each.node.id === reveal)
        : drawn.find((each) => restated.has(each.node.id));
    if (entry === undefined) {
      if (restated === undefined && partOf.get(reveal) === "agents") setFootShown(true);
      return;
    }
    if (entry.kind === "decision" && writeUps === undefined) return;
    const row = rowOf(body.current, entry.node.id);
    if (row === null) return;
    taken.current = reveal;
    row.focus();
  }, [drawn, partOf, reveal, writeUps]);
  // Whether the list draws the node the address names, which the page is told
  // (`onRevealDrawn`), so that where it does not the outline is drawn in the view's place and
  // lands on it as the outline does. The list draws every node the tree holds that an address
  // names - an ask or a decision as an entry, a node of another kind as the row before the
  // parts, a decision the decisions read holds no row for in the foot - but for a person's
  // write-up of an answered question, which it draws only as the row of a question it answers,
  // where the address lands as above. Under words that match none of those questions the list
  // draws neither the write-up nor any question it answers, so the address would land nowhere
  // here; the outline draws the write-up under the question it answers. It is said before the
  // browser paints, so where the reads were held as the view was drawn, as on Back to its
  // address, no list is painted in the outline's place.
  useLayoutEffect(() => {
    if (onRevealDrawn === undefined) return;
    if (reveal === undefined) {
      onRevealDrawn(false);
      return;
    }
    const restated = writeUps?.get(reveal);
    if (restated !== undefined) {
      onRevealDrawn(drawn.some((entry) => restated.has(entry.node.id)));
      return;
    }
    onRevealDrawn(true);
  }, [drawn, onRevealDrawn, reveal, writeUps]);

  // Whether the focus on the foot's control is the one this body put there on its own, which
  // the reader has not yet pressed, typed on or left. The tree takes the focus only from a
  // script - the chord, the stage, this body - so a focus it passes to the control is the
  // body's own too.
  const footPlaced = useRef(false);
  // Puts the focus on the foot's control, as the body's own, where the control is drawn and the
  // list draws no tab-stop row, and only once the reads that say which questions and decisions
  // there are hold their answers; says whether it could. Until those reads answer, or while one
  // of them is refused, a row or a refused read's Retry may yet be drawn, and the stage gives
  // either the focus the tree holds (`slots/stage.tsx`), which the control would hold from
  // them. Every place that hands the control the focus on its own goes through here, so that
  // rule is kept in one place. The tab stop is looked for in the whole list, since the foot's
  // rows stand after the tree element and are its rows all the same (`aria-owns`).
  const placeFoot = useCallback((): boolean => {
    const control = footControl.current;
    if (!readsHeld || control === null) return false;
    if (body.current?.querySelector(STOP_ROW) != null) return false;
    control.focus();
    footPlaced.current = document.activeElement === control;
    return true;
  }, [readsHeld]);
  // The tree element takes the focus only to pass it on: to the list's tab stop, else to the
  // foot's control where `placeFoot` lets it take it. `t d` focuses the tree where it finds no
  // tab stop in it (`useIssueKeymap.ts`), and a list that holds only the foot has none. Where
  // neither takes it the tree keeps the focus, which leaves it in the list: until the reads
  // hold, and on an issue the view draws nothing for. A focus that came up from a row is the
  // row's own.
  const passFocus = useCallback(
    (event: FocusEvent<HTMLDivElement>) => {
      if (event.target !== event.currentTarget) return;
      const stopRow = body.current?.querySelector<HTMLElement>(STOP_ROW);
      if (stopRow != null) stopRow.focus();
      else placeFoot();
    },
    [placeFoot]
  );
  // A page opened on a list that holds only the foot has no row for the stage to focus
  // (`slots/stage.tsx`), so the foot's control takes the focus once, on the stage's own terms:
  // no address names an entry and nothing holds the focus. Nothing holds it on the page itself,
  // or on an element that holds the list (`focusIsLoose`): the app focuses its main element at
  // each page change but the first (`app.tsx`), so a page opened by a link inside the app, from
  // another issue, the Inbox or a board, finds the focus there. It is placed by `placeFoot`, as
  // a focus on the tree is passed, so `t d` and a page opened here reach the control by one
  // rule, and it waits, as the tree does, for the reads that say which questions and decisions
  // there are. The reads may answer after the body mounts, so this looks again on every render
  // until it has acted. Where they are cached, as on Back to the page's address, it acts in the
  // commit the page mounts in, and the app then moves the focus to its main element, since React
  // runs a child's effects before its parent's. So a frame after it placed the focus it looks
  // once more, and places it again where nothing holds it then, as the stage does; a focus the
  // reader or a script put elsewhere in that frame stays.
  const footReached = useRef(false);
  // That frame, which goes with the body: the body cancels it as it leaves the page.
  const footAgain = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (footReached.current || reveal !== undefined || !focusIsLoose(body.current)) return;
    footReached.current = placeFoot();
    if (!footReached.current) return;
    footAgain.current = requestAnimationFrame(() => {
      footAgain.current = undefined;
      if (focusIsLoose(body.current)) placeFoot();
    });
  });
  useEffect(
    () => () => {
      if (footAgain.current !== undefined) cancelAnimationFrame(footAgain.current);
    },
    []
  );
  // The tree is built read by read (`useIssueTree.ts`), so a list can hold only the foot until
  // the asks read answers, and then draw its questions. The focus the body put on the foot's
  // control was for a list with no row: once one is drawn it goes to the tab stop, where the
  // stage would have put it, unless the reader has taken the control as their own. Where the
  // control leaves the page with the focus, its own leave places it (`footRef`).
  useEffect(() => {
    if (!footPlaced.current || drawn.length === 0) return;
    footPlaced.current = false;
    if (document.activeElement === footControl.current) tree.current?.focus();
  });
  // The reads can answer while the tree holds the focus: `t d` gave it before they did, or the
  // stage did as a refused read's line left. Where they answer with no row to draw and the foot
  // drawn, the focus goes on to the foot's control (`placeFoot`), as the tree would have passed
  // it had they answered first, and it is the body's own there, as on a page opened here. Any
  // render can bring the answer, so this looks after every one.
  useLayoutEffect(() => {
    if (document.activeElement === tree.current) placeFoot();
  });
  // Pressing the control, typing on it or leaving it makes the focus the reader's own.
  const footTaken = (): void => {
    footPlaced.current = false;
  };
  // A row of the foot stands a level down, under the foot's control, so Left on one goes up to
  // that control, as Left goes to a row's parent in the outline. The outline's keys read every
  // row at the list's one level (`rows`), where Left says there is no level above, so the key is
  // taken here, before they read it: the key table passes over a press already taken
  // (`keymap.ts`). It is taken on three of the keys' terms: with no modifier, not already taken,
  // and not on a block that scrolls its own content, which the browser scrolls by the key
  // (`useTreeKeymap.ts`); such a block carries `data-kept-scroller`, the mark `RowText.tsx`'s
  // `KEPT_SCROLLER` owns. The key table's other two terms, no composition and no editable
  // target, do not apply: a foot row holds no field and no composition (R4).
  const footRowLeft = (event: KeyboardEvent<HTMLDivElement>): void => {
    if (
      event.key !== "ArrowLeft" ||
      event.defaultPrevented ||
      event.altKey ||
      event.ctrlKey ||
      event.metaKey ||
      event.shiftKey ||
      (event.target instanceof Element && event.target.closest("[data-kept-scroller]") !== null)
    )
      return;
    event.preventDefault();
    footControl.current?.focus();
  };
  // The foot leaves the page where its last decision moves up to another part, as when a read
  // ties it to a statement, and the browser moves the focus its control held to the page's
  // body, where nothing says where it was. So the control says it while it still holds it:
  // React runs this cleanup before it takes the control off the page. The focus goes to the
  // list's tab stop, else to the tree, once the render that took the control away is on the
  // page, unless something else took it in that render, as a failed read's line does for its
  // own leave (`slots/failed-read-line.tsx`). A focus the body put on the control goes with it.
  // One function for every render, so React runs the cleanup only when the control leaves.
  const footRef = useCallback((control: HTMLButtonElement | null) => {
    footControl.current = control;
    if (control === null) return undefined;
    return () => {
      footControl.current = null;
      footPlaced.current = false;
      if (document.activeElement !== control) return;
      queueMicrotask(() => {
        if (!focusOnDocument()) return;
        (body.current?.querySelector<HTMLElement>(STOP_ROW) ?? tree.current)?.focus();
      });
    };
  }, []);

  // An entry the reader is on can move to another part of the list, and its row is drawn
  // again there, which drops the focus to the page: a decision they kept or asked to change
  // leaves the review list for their own decisions, a question another person answered goes
  // under Your decisions, a decision of the foot that a read ties to a statement comes up to
  // the review list. A reader who kept or asked to change a decision on its row here is working
  // down the review list, so the focus goes to the decision that stood after it, else the one
  // before it (the prototype's `keepDecision`), else to the decision where it now stands. That
  // it was their review is read from the decision the server wrote back - kept by them, or
  // with their change - after their press on its row, so a Keep the server refused and a
  // Change sent empty, which write nothing, are none. An entry that moves by no act of the
  // reader here - a read that says another person kept a decision or answered a question, or
  // that a decision was replaced or ties to a statement - keeps the focus where it now stands:
  // the reader asked to go nowhere, and the focus stays on the entry they were on (R6), the
  // foot unfolded for it where it is one of the foot's own. A Keep or a Change the reader made
  // elsewhere keeps the focus too, unless their own press on this row is still armed
  // (`ownReview`): then the decision left by their own review, and the focus moves on as
  // above. An entry the reader is on that is drawn nowhere now - another person deleted it, or
  // the words filter took it out of the view - takes its row and the focus away: the focus goes
  // to the entry that stood after it in its part, else the one before it, else to the tree,
  // which hands it to the list's tab stop - the view's first row, since the entry the stop
  // rested on is drawn nowhere - else to the foot's control, else keeps it, so it is never left
  // on the page (decision 110). A reader who has moved somewhere else on the page keeps their
  // place.
  const stood = useRef(partOf);
  useLayoutEffect(() => {
    const before = stood.current;
    stood.current = partOf;
    if (focused === undefined) return;
    const was = before.get(focused);
    const now = partOf.get(focused);
    if (was === undefined || was === now) return;
    const active = document.activeElement;
    if (active !== null && active !== document.body && active.isConnected) return;
    // The press is spent once the decision has left the review list, whatever it wrote.
    const pressedHere = was === "review" && ownReview.current === focused;
    if (pressedHere) ownReview.current = undefined;
    const standing: readonly (DecisionsEntry | NamedEntry)[] = now === undefined ? [] : parts[now];
    const entry = standing.find((held) => held.node.id === focused);
    const reviewed =
      pressedHere &&
      entry?.kind === "decision" &&
      (isReader(entry.keptBy, reader) || isReader(entry.change?.by, reader));
    if (now !== undefined && !reviewed) {
      goTo(focused);
      return;
    }
    const next =
      neighbourOf(focused, was, before, partOf) ?? (now === undefined ? undefined : focused);
    const row = next === undefined ? null : rowOf(body.current, next);
    if (row !== null) {
      row.focus();
      return;
    }
    // No row named here takes it: the focus is off every row, so what a row's focus opened goes,
    // as when the foot's control takes it, and the tree passes it on (`passFocus`) to the tab
    // stop, which falls back to the view's first row, else, once the reads hold, to the foot's
    // control, else keeps it.
    footFocused();
    tree.current?.focus();
  }, [focused, footFocused, goTo, partOf, parts, reader]);

  // What `q` and `w` walk (`tree-keys.ts`), in the order a reader reads this list: its entries
  // as it draws them, the foot's last whether it is unfolded or not, then every other node of
  // the issue in the tree's order, so a question or a decision the words filter took out of the
  // view is still found, after everything the list holds. Each is a leaf, so the walk meets a
  // node at its place in that order and not again under its parent, but for a statement, which
  // keeps its checks under it, each a leaf: `w` on a check no decision names goes to the decision
  // behind the statement it is drawn under, which `decisionBehind` finds by reading that
  // statement's children, and so it does on an address that names the check. A check then
  // stands twice, under its statement and as a leaf of its own, which does no harm: `q` reads
  // the questions alone, and `w` finds a check's statement through the statement's children.
  // The root is the view's own, so a key that reads the root's id reads it as before.
  const keyOrder = useMemo<TreeNode>(() => {
    const seen = new Set([root.id, view.id]);
    const leaves: TreeNode[] = [];
    const add = (node: TreeNode): void => {
      if (seen.has(node.id)) return;
      seen.add(node.id);
      const checks =
        node.type === "requirement" ? node.children.filter((child) => child.type === "check") : [];
      leaves.push({ ...node, children: checks.map((check) => ({ ...check, children: [] })) });
    };
    for (const part of DECISIONS_PART_ORDER) {
      for (const entry of parts[part]) add(entry.node);
    }
    for (const node of nodesInOrder(root)) add(node);
    return { ...view, children: leaves };
  }, [parts, root, view]);
  // Where `q` and `w` take the reader: to an entry of the list as any move does, the foot
  // unfolded for one of its own, and to the conversation at a node the list draws no row for,
  // as an entry's link to its node does, since there is no row here to put the focus on.
  const goToOrOpen = useCallback(
    (id: string) => {
      if (partOf.has(id)) {
        goTo(id);
        return;
      }
      const node = keyOrder.children.find((leaf) => leaf.id === id);
      const path = node === undefined ? undefined : conversationPath(node);
      if (path !== undefined) navigate(path);
    },
    [goTo, keyOrder, navigate, partOf]
  );

  useTreeKeymap({
    container: body,
    // Escape in the editor of the row being edited leaves the caret for the row (`onEditEnd`).
    editing: editing !== undefined,
    holdsEscape,
    onCycle: () => {
      // Folding the foot takes its rows away, and a focus on one of them would drop to the
      // page: it goes to the foot's control, which stays where the rows were.
      if (everything && focused !== undefined && partOf.get(focused) === "agents")
        footControl.current?.focus({ preventScroll: true });
      setEverything(!everything);
      setFootShown(!everything);
    },
    // Enter on a row puts the caret in its node's text, in place, where that text is written
    // (`edit`); the entry's node in the conversation is its own link's to go to.
    onEdit: edit,
    onEditEnd: stopEditing,
    onFold: NOTHING,
    onOpenDeeper: NOTHING,
    // `o` names the row by its key (`rowKeyOf`), the view's id before the node's, and the list
    // pins a row by its node's id, as the row's own control does.
    onPin: (rowKey) => {
      const row = rows.find((each) => rowKeyOf(each) === rowKey);
      if (row?.kind === "node") pin(row.node.id);
    },
    onReport: reportHere,
    onReveal: goToOrOpen,
    onZoom: NOTHING,
    oneNodeAtATime: false,
    readsHeld,
    rows,
    root: keyOrder,
    writer,
    zoom: undefined,
  });

  const first = drawn[0]?.node.id;
  const tabStop =
    stop !== undefined && drawn.some((entry) => entry.node.id === stop) ? stop : first;
  const rowFor = (entry: DecisionsEntry | NamedEntry, part: DecisionsPart): ReactNode => {
    const node = entry.node;
    const id = node.id;
    const place = documentOfNode(id, artifacts);
    const mark = threads.get(id);
    const key = rowKeyOf(drawnRowOf(entry, view.id));
    return (
      <DecisionsRow
        body={focused === id || pinned.has(id) || everything}
        {...(place === undefined ? {} : { document: place.artifact })}
        {...(editing?.row === key ? { editing: editing.artifactId } : {})}
        entry={entry}
        focused={focused === id}
        isClosed={isClosed}
        key={id}
        level={part === "agents" ? 2 : 1}
        onFocus={() => press.focus(entry)}
        onPin={() => pin(id)}
        onPressWords={() => pressWords(key, node)}
        // A Keep pressed or a Change sent is the press over: its click has landed, so the row
        // takes the focus at once rather than once the press's end comes, which the write's
        // answer can come before, and the decision would leave the review list from a row the
        // list did not yet hold the reader on.
        onReviewed={() => {
          ownReview.current = id;
          takeFocus(entry);
        }}
        pinned={pinned.has(id)}
        reader={reader}
        {...(report !== undefined && (report.row ?? tabStop) === id ? { report: report.line } : {})}
        review={part === "review"}
        rowKey={key}
        statementNames={statementNames}
        tabStop={tabStop === id}
        {...(mark === undefined ? {} : { threadRead: mark })}
      />
    );
  };
  const foot = `${headings}-agents`;
  const footRows = `${headings}-agents-rows`;
  // The lines the body says over the list: each read that says which entries there are and
  // stands refused, the decisions before the questions, then the names' line (`namesLine`), as
  // the outline draws it after the lines of the reads its rows come from (`outline-reads.ts`'s
  // `useOutlineReads`).
  const failedLines = [
    ...unreadable.map((read) => ({
      failure: failures[read],
      key: read,
      message: REFUSED_READ_WORDS[read],
      retry: () => retry(read),
      retrying: retrying.includes(read),
    })),
    ...namesLine,
  ];
  const drawnBody = (
    // One landmark named by the filter, which is where a reader who knew the old tab's four
    // sections finds them (R19). It holds the tree's rows and the foot's control, so the keys
    // and the focus are read over the whole of it.
    <section
      aria-label={PRESET_NAMES.decisions}
      className="mx-auto mt-3.5 mb-[120px] flex w-full max-w-[960px] min-w-0 flex-col max-[761px]:mt-2.5"
      data-decisions-body=""
      onBlur={releaseFocus}
      // A focus on a refused read's Retry is off every row, as one on the foot's control is.
      onFocus={(event) => {
        if (event.target.matches(FAILED_READ_RETRY)) footFocused();
      }}
      // A press in the list, whose focus a row takes once the press is over (`press`).
      onMouseDown={press.onMouseDown}
      ref={body}
    >
      {/* Each line, as the page's one line for a failed read draws it, in the order above. The
          lines stand over the tree and never in it: a tree holds rows and groups of rows alone.
          A refused read's line stands in place of the entries the read would give: the tree is
          built from the same reads (`useIssueTree.ts`), so a read that holds nothing gives the
          list no entry of its own, and no part is drawn for it. The names place no entry, so
          their line takes none away. Each line's alert is keyed by its read's count of failures,
          so a run that ends with no new refusal, cancelled or paused offline, says nothing
          again. */}
      {failedLines.map((line) => (
        <FailedReadLine
          failure={line.failure}
          key={line.key}
          message={line.message}
          onRetry={line.retry}
          retrying={line.retrying}
        />
      ))}
      {/* The foot's rows stand after its control, outside this element, and are this tree's
          all the same (`aria-owns`), so the page holds one tree. The element itself takes the
          focus only to pass it on (`passFocus`), and only from a script: a press on it outside
          a row, on a part's heading, its count or the gap between two rows, would otherwise
          focus it, as the nearest element that takes the focus, and the focus would go on to
          the tab stop, scroll the page to it and open its body. The outline's tree takes no
          focus from a press either (`IssueTree.tsx`). */}
      <div
        aria-label={CONTROL_LABELS.tree}
        aria-owns={footShown && parts.agents.length > 0 ? footRows : undefined}
        className="flex min-w-0 flex-col"
        onFocus={passFocus}
        onMouseDown={(event) => {
          const target = event.target;
          if (!(target instanceof Element) || target.closest("[data-node-row]") === null)
            event.preventDefault();
        }}
        ref={tree}
        role="tree"
        tabIndex={-1}
      >
        {/* The node the address names where it is neither a question nor a decision stands
            first, at the tree's first level, under no heading and in no part: the address
            outranks the view, and the parts stay four (R1, R6). */}
        {parts.named.map((entry) => rowFor(entry, "named"))}
        {HEADED.map((part) => {
          const entries: readonly DecisionsEntry[] = parts[part];
          if (entries.length === 0) return null;
          return (
            // biome-ignore lint/a11y/useSemanticElements: a tree owns only its rows and groups of rows; a fieldset is neither.
            <div
              aria-labelledby={`${headings}-${part}`}
              className="flex min-w-0 flex-col"
              data-decisions-part={part}
              key={part}
              role="group"
            >
              {/* The part's heading (the prototype's `.dgh`): its name and how many entries
                  it holds. Its name is the group's; a tree holds rows and groups alone, so it
                  is no element with a role of its own. */}
              <div
                className="mt-[22px] mb-1 flex min-w-0 items-baseline gap-2.5 max-[761px]:mt-[18px]"
                data-decisions-heading=""
              >
                <span
                  className={`text-xs leading-[18px] font-semibold tracking-[0.08em] uppercase ${pageInkText}`}
                  id={`${headings}-${part}`}
                  {...pageChrome}
                >
                  {DECISIONS_WORDS.parts[part]}
                </span>
                <span
                  className={`text-xs leading-[18px] ${pageQuietText}`}
                  data-decisions-count=""
                  {...authorText}
                >
                  {entries.length}
                </span>
              </div>
              {entries.map((entry) => rowFor(entry, part))}
            </div>
          );
        })}
      </div>
      {parts.agents.length === 0 ? null : (
        <div className="flex min-w-0 flex-col" data-decisions-part="agents">
          {/* The foot (the prototype's `.minor-t`): one control that says how many decisions
              it holds and whether they are drawn, and unfolds them. It is a plain button in
              the tab order, as the prototype's is, so Tab reaches it where the foot is all the
              list holds; the key that cycles how much is drawn unfolds it too. A press of it
              moves no focus until the click lands: it is no row, so the list does not hold its
              focus until the press is over (`press`), and the focus coming here closes the
              open body of the row that held it, so the control would move up from under the
              pointer before the release, and the click would be lost. */}
          <div className="mt-[22px] mb-1 flex min-w-0 max-[761px]:mt-[18px]" data-decisions-foot="">
            <button
              aria-controls={footShown ? footRows : undefined}
              aria-expanded={footShown}
              className={`dispatch-inline-link rounded py-1 text-left text-sm leading-5 font-medium hover:text-page-ink ${pageMutedText} ${focusVisibleRing}`}
              data-decisions-heading=""
              id={foot}
              onBlur={footTaken}
              onClick={(event) => {
                setFootShown(!footShown);
                event.currentTarget.focus({ preventScroll: true });
              }}
              onFocus={footFocused}
              onKeyDown={footTaken}
              onMouseDown={(event) => event.preventDefault()}
              onPointerDown={footTaken}
              ref={footRef}
              type="button"
              {...pageChrome}
            >
              <FoldMark open={footShown} />{" "}
              <span data-decisions-count="" {...authorText}>
                {parts.agents.length}
              </span>{" "}
              {parts.agents.length === 1 ? DECISIONS_WORDS.agents_one : DECISIONS_WORDS.agents_many}
            </button>
          </div>
          {footShown ? (
            // biome-ignore lint/a11y/useSemanticElements: a tree owns only its rows and groups of rows; a fieldset is neither.
            <div
              aria-labelledby={foot}
              className="flex min-w-0 flex-col"
              id={footRows}
              onKeyDown={footRowLeft}
              role="group"
            >
              {parts.agents.map((entry) => rowFor(entry, "agents"))}
            </div>
          ) : null}
        </div>
      )}
      {/* The issue holds no question and no decision the view draws. The node an address
          names is no entry of the list, so its row and this line are drawn together. Until the
          reads that say which questions and decisions there are hold their answers
          (`decisions-entries.ts`), a list with nothing in it may be one whose entries have not
          arrived, or whose read was refused and says so in its own line, so no line says the
          issue holds nothing, as the outline draws none then (`slots/body.tsx`). */}
      {readsHeld &&
      parts.open_questions.length === 0 &&
      parts.review.length === 0 &&
      parts.yours.length === 0 &&
      parts.agents.length === 0 ? (
        <p className={`py-1 text-sm ${textMutedOnCanvas}`} {...pageChrome}>
          {emptyLine}
        </p>
      ) : null}
    </section>
  );
  return <PublishedReferenceNames published={published}>{drawnBody}</PublishedReferenceNames>;
}

/** The foot control's mark: a triangle that points right while the foot's decisions are folded
 *  and down while they are drawn, one em square so it takes the size of the control's text, in
 *  its colour. It is a shape and not text, as `IssuePresets.tsx`'s `MenuMark` is, because the
 *  page's typeface keeps neither triangle: as text, each is drawn in a face of the reader's own
 *  machine, at that face's width. The control's `aria-expanded` says whether the foot is open,
 *  so the shape is hidden from assistive technology. */
function FoldMark({ open }: { open: boolean }): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="inline-block size-[1em] shrink-0 align-middle"
      focusable="false"
      viewBox="0 0 16 16"
    >
      <path d={open ? "M4.5 6.5h7L8 10.5z" : "M6.5 4.5v7L10.5 8z"} fill="currentColor" />
    </svg>
  );
}

/** The row of node `id` inside `container`, or null where it draws none. */
function rowOf(container: HTMLElement | null, id: string): HTMLElement | null {
  return (
    container?.querySelector<HTMLElement>(`[data-node-row][data-node-id="${CSS.escape(id)}"]`) ??
    null
  );
}

/** The row the outline's keys read for `entry`: at the list's one level, under the view, which is
 *  the key (`rowKeyOf`) the row's own `data-row-key` carries and the editing finds the row by. */
function drawnRowOf(entry: DecisionsEntry | NamedEntry, viewId: string): DrawnRow {
  return { kind: "node", level: 1, node: entry.node, trail: [viewId] };
}

/** The entry that stood after `id` in `part` as `before` drew the list, else the one before
 *  it, of those that stand in `part` still (`now`); undefined where none does. Each map holds
 *  the entries in the order the page draws them. */
function neighbourOf(
  id: string,
  part: DecisionsPart,
  before: ReadonlyMap<string, DecisionsPart>,
  now: ReadonlyMap<string, DecisionsPart>
): string | undefined {
  const stood = [...before].flatMap(([entry, held]) => (held === part ? [entry] : []));
  const at = stood.indexOf(id);
  const stays = (entry: string) => now.get(entry) === part;
  return stood.slice(at + 1).find(stays) ?? stood.slice(0, at).reverse().find(stays);
}

/** The two reads the seam says may be refused, by the names it gives them. */
type DecisionsRead = ReturnType<typeof useDecisionsEntries>["unreadable"][number];

/** What the body says of each read that stands refused, in the page's words: the decisions for
 *  the decisions read, and the questions for the asks. A read the seam adds fails this until the
 *  body has a line for it. */
const REFUSED_READ_WORDS: Readonly<Record<DecisionsRead, string>> = {
  asks: STATE_LINES.asks_unreadable,
  decisions: STATE_LINES.issue_decisions_unreadable,
};

/** The row that holds the list's one tab stop, which `passFocus` and the foot's leave look for
 *  in the whole list: the foot's rows stand outside the tree element. */
const STOP_ROW = '[data-node-row][tabindex="0"]';
