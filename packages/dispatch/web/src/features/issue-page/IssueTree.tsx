import { useQuery } from "@tanstack/react-query";
import {
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { whoAmIQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import { textMutedOnCanvas } from "../../theme/classes";
import { isViewer } from "../refs/actor";
import { EditedMark } from "../refs/PostMarks";
import { postDeletion } from "../refs/post-marks";
import { PublishedReferenceNames, type PublishedReferences } from "../refs/reference-names";
import { EVENT_FRESH } from "../refs/reference-target";
import { focusOnDocument } from "../shell/roving";
import { ChangeDraftsProvider } from "./DecisionActions";
import { standingParts } from "./decisions-filter/entry-standing";
import { type FocusedNode, useSetFocusedNode } from "./focused-node";
import { bringOnScreen, FOOT_HEIGHT_PROPERTY, IDENTITY_HEIGHT_PROPERTY } from "./identity-block";
import { pathToTarget } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import { DETAILS_ATTRIBUTE } from "./margin-slot";
import { DeletedLine, lineOrKindWord, useNodeLine } from "./node-line";
import { NodeStatesProvider } from "./node-states/NodeMarks";
import { marksOf, statesByReference } from "./node-states/node-states";
import { SettleIssueProvider } from "./node-states/settle-write";
import { type FailedRead, NO_FAILED_READS } from "./outline-reads";
import { useFocusBranch, useTakeOutlineRequest } from "./outline-request";
import { threadMarks } from "./outline-thread-read";
import { conversationItem, useOutlineWriter } from "./outline-writer";
import { authorText, KEY_REPORTS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { type Press, trackPress, usePressFocus } from "./press-focus";
import { FAILED_READ_RETRY, FailedReadLine } from "./slots/failed-read-line";
import { TreeRows } from "./TreeRows";
import { focusIsLoose, nodeRowSelectorOf, rowSelectorOf } from "./tree-focus";
import {
  type DrawnRow,
  NOTHING_OPENED,
  type OpenedBranches,
  type PageRow,
  pathTo,
  placesAlong,
  treeRows,
  withEveryFoldUndone,
  withFold,
  withNextPage,
  withUnfolded,
  zoomBreadcrumb,
} from "./tree-fold";
import { decisionBehind, nextQuestion, nodesInOrder, rowKeyOf } from "./tree-keys";
import type { NextTreePage, OlderComments } from "./tree-read/tree-read";
import { abortOnReaderAct, type OnEditing, useNodeEditing } from "./useNodeEditing";
import { moveToRow, type TreeSearch, useTreeKeymap } from "./useTreeKeymap";

/**
 * The issue's body: one tree of its nodes, every node of it unfolded and whole (R11).
 *
 * The model is `issue-tree.ts`'s, the view of it the page or a filter draws is `presets.ts`'s
 * and the fold is `tree-fold.ts`'s, all three pure; this holds what only a reader's page can
 * hold - what they folded, the pages of children they drew, and where the focus is. The rows
 * themselves are drawn by `TreeRows.tsx` and the caret in a node's own text is
 * `useNodeEditing.ts`'s.
 *
 * The tree is the one body the page and every filter draw: the page hands it the tree its view
 * left. The issue is never a row: the header is the issue, so the outline's first row is the
 * first node under it. A filter draws its own kinds as rows and no other, each drawn whole at
 * every width, its parent a line on it where that parent is no row. A row drawn to place the
 * filter's own - a requirement under Verification - is context, faint and marked, and reached
 * by the arrow keys like any other row.
 *
 * The tree answers the WAI-ARIA tree pattern's roles and properties and keeps one tab stop: the
 * focused row is the only row that takes the focus, and the keys that move it are
 * `useTreeKeymap.ts`'s. Every row draws what it holds on its face whatever the focus, so moving
 * the focus changes no row's text and no row's height. A press on a row gives it the focus once
 * the press is over, and a press on another control of the page keeps the focused row's look
 * until that press has ended and its click has landed (`useReleaseFocus`); the focus leaving by
 * Tab or a script lets the row go at once. A key that has nowhere to go says so at the row it
 * was pressed on, until the reader's next key or move.
 *
 * The page opens with the focus on the first open question that waits on the reader, its path
 * unfolded and its row in the viewport, whatever reads failed: the issue's own read brings its
 * open asks as the asks' read brings every ask, so such a question is drawn while the asks' read
 * stands failed where the issue's read holds it. Where none is drawn and a read the rows come
 * from stood failed as the reads settled, it opens on a failed read's Retry, over the tree, since
 * the question that waits may be what the read failed to bring: the asks' where they failed,
 * which bring the questions the issue's read does not hold, else the first such read's. A Retry
 * whose line leaves before the focus has come to it, its read answered, hands the opening on by
 * the same rule: to the question that waits where the reads now draw one, else to the next such
 * read's Retry. Where none of those is, it opens on the outline's first row. A write from the
 * line at the foot takes the reader to what they wrote once its read has drawn it, its branch
 * unfolded (`outline-writer.tsx`'s `Landing`).
 *
 * Nothing is folded as the page opens, and only the reader folds a row, by its fold control,
 * the left arrow or `o`; a node drawn under several parents is a row under each, and the fold is
 * that one row's. `z` unfolds every row they folded, and a fold lasts for the load. `.` zooms into
 * the focused node under a breadcrumb of its ancestors from the issue down, each crumb a link
 * that zooms out to it, and `,` zooms back out one level. A node draws its first 50 children and
 * a row that draws the next 50 under that row alone, and the node an address, the focus or a
 * write names is drawn with every child before it; while the outline read answers older
 * comments, a control above the first row under the issue reads the next page of them, and while
 * it answers a next page, a control after the last row reads that page (R15, `older`, `more`).
 */
export function IssueTree({
  addressActs,
  addressKey,
  artifacts,
  context,
  coverageOut = false,
  emptyLine,
  failedReads = NO_FAILED_READS,
  focusReveal,
  holdsEscape,
  isClosed,
  landingReport,
  more,
  older,
  onEditing,
  published,
  readsHeld = true,
  readsSettled = true,
  request,
  reveal,
  root,
  rootRow,
  zoomAt,
}: {
  /** The reader's acts since they followed the address (`addressKey`): a controller the page
   *  starts as each following is drawn and the reader's first act since aborts
   *  (`slots/body.tsx`). A landing whose following saw an act before the address's row drew does
   *  nothing to the page: the row takes no focus and the page does not scroll to it, so the
   *  focus and the page stay where the reader put them. Left out, the landing reads the reader's
   *  acts from the moment its row is drawn. */
  addressActs?: RefObject<AbortController | undefined>;
  /** Which following of the address `reveal` comes from (`useFollowing`, the location's key with
   *  its address), a new one at each link followed - one followed again to the address
   *  the reader is at among them - the entry's own one again at Back and Forward, and the same
   *  one at every render of it. The tree lands on the address's node once per key it has not
   *  landed on last (`slots/body.tsx`; left out, once per node): Back to an entry past a link
   *  that named another node lands again, and Back past one that named no node does not. */
  addressKey?: string;
  /** Each artifact of the issue by its slug, which is how a node names the document it is or
   *  belongs to (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The ids of the rows the view draws as context, faint and marked. */
  context: ReadonlySet<string>;
  /** Whether the coverage read has not answered yet (`outline-reads.ts`'s `coverageOut`): each
   *  statement's row holds the line its counts are drawn in until it has (`TreeRows.tsx`).
   *  Answered unless the page says otherwise. */
  coverageOut?: boolean;
  /** What the page says where the preset's query matched nothing under the issue, drawn only
   *  where it is given and while the reads are held (`readsHeld`). */
  emptyLine?: string;
  /** Each read the tree's rows or their names come from that stands failed
   *  (`outline-reads.ts`), in the order the page gives them: each is drawn as its line, with a
   *  Retry that asks that read alone again, over the tree and never in it (`FailedReadLine`).
   *  The rows the reads hold stay drawn under them. Where no question that waits on the reader
   *  is drawn as the reads settle (`readsSettled`) and one the rows come from stands failed, the
   *  page opens on a line's Retry: the asks' line's where it is one of them (`bringsQuestions`),
   *  else the first such line's. None unless the page gives them. */
  failedReads?: readonly FailedRead[];
  /** Whether the tree puts the focus on the node the address names, which the outline always
   *  does (`slots/body.tsx`). */
  focusReveal: boolean;
  /** Whether the page takes Escape from a row before the outline is left: a filter by words to
   *  clear or a filter to turn off comes first in the key table's order (`slots/body.tsx`). */
  holdsEscape: boolean;
  /** Whether the issue is closed, which the writes a row offers are refused on. */
  isClosed: boolean;
  /** What the row the address lands on says once it has the focus, as a key's report is said at
   *  the row it was pressed on, until the reader's next key or move: that the thread of the turn
   *  the address names is the agents' alone, where the outline draws no row for that turn and
   *  `reveal` is the chain to its nearest drawn ancestor (`slots/body.tsx`). */
  landingReport?: string;
  /** The outline read's next page (`tree-read/tree-read.ts`' `useNextTreePage`): while its answer
   *  names one, a control after the outline's last row says how many nodes the read holds after
   *  the pages read and reads the next, and `j` or Down on the last row presses it, the focus going
   *  to the first row it drew either way (R15). Absent for a tree that reads none. */
  more?: NextTreePage;
  /** The outline read's older comments (`tree-read/tree-read.ts`' `useOlderComments`): while
   *  they remain, a control above the outline's first row under the issue reads the next page
   *  of them, and `k` or Up on that row presses it (R15). Absent for a tree that reads none, and
   *  the control is not drawn while the tree is zoomed into a node. */
  older?: OlderComments;
  /** The document the reader is editing a node of here, so the page draws that document's one
   *  editor rather than leaving the row with nothing to put a caret in, and hands that editor to
   *  the row; `undefined` whenever the reader is editing nothing. */
  onEditing: OnEditing;
  /** What the page publishes of its one batched read (`useIssueTree.ts`'s `published`), which
   *  the tree hands every row and crumb it draws in the render that draws it
   *  (`PublishedReferenceNames`): a reference the page is asking for waits on the page's answer
   *  from a row's first render, rather than being read again by the row (R8). The page publishes
   *  it to the names store as well, which names each target. Every caller hands it, so no tree
   *  draws a row that reads what its page asks for. */
  published: PublishedReferences;
  /** Whether each read the tree's nodes come from holds an answer, which a failed read never
   *  does: the tree read, which answers every node (`blocksHeld`), and the five reads - the
   *  coverage, the results, the asks, the comments and the messages (`outline-reads.ts`'s
   *  `readsHeld`). Until then a node the tree lacks may not have arrived yet, so a tree with no
   *  node draws no `emptyLine`, and `q` and `w` with nowhere to go say nothing
   *  (`useTreeKeymap.ts`). The names are no part of it: the page gives no `emptyLine` that reads
   *  the nodes' words until they are held (`slots/body.tsx`). Held unless the page says
   *  otherwise. */
  readsHeld?: boolean;
  /** Whether every read the tree's rows come from has answered, with its data or its failure
   *  (`outline-reads.ts`): the page's first focus waits for it, so it lands once, where every
   *  read the rows come from has drawn them; a later read that draws rows above it takes no
   *  focus from it. Settled unless the page says otherwise. */
  readsSettled?: boolean;
  /** What the Contents list last asked of the outline (`outline-request.tsx`): go to a node, and
   *  draw that node's branch alone while Focus branch is on, or the whole outline again once it
   *  is turned off. Each ask is acted on once (`useTakeOutlineRequest`). */
  request?: OutlineRequest;
  /** The chain down to the node the address names, the issue first: the tree unfolds it and
   *  puts the focus on its last node, once per address. */
  reveal: readonly TreeNode[] | undefined;
  /** The tree as the view left it. */
  root: TreeNode;
  /** Whether no filter is on (`presets.ts`'s `PresetView.rootRow`): the conversation, which the
   *  page opens on what waits on the reader. */
  rootRow: boolean;
  /** The node an address roots the tree at, as a zoom the reader made, by its id: Graph's
   *  address `/graph/<node id>` (R10). The tree is zoomed to it as it is drawn and as the address
   *  names another; the reader's zooms after that are theirs. Absent on every other address. */
  zoomAt?: string;
}): ReactNode {
  // What the reader folded, and the pages of children they drew, each by the row's place
  // (`tree-fold.ts`).
  const [opened, setOpened] = useState<OpenedBranches>(NOTHING_OPENED);
  // The row the focus is on: the row that holds the focus, or the one a press on another
  // control of the page took it from, until that press has ended (`useReleaseFocus`).
  const [focused, setFocused] = useState<string | undefined>(undefined);
  // Where the tree's one tab stop rests, by the row's key, which outlives the focus leaving it:
  // Shift+Tab comes back to the row the reader left, not to the first row. The tree draws that
  // row with every child before it at its own place, past its parent's page where a read moved
  // it there (`treeRows`' `placed`).
  const [stop, setStop] = useState<string | undefined>(undefined);
  // Focus branch is the page's, held above the tree with the Contents list's requests
  // (`outline-request.tsx`), and it outlives a view drawn in the tree's place where the zoom does
  // not: the tree drawn again as the view is turned off is a new one, and the request that zoomed
  // the one before was acted on already, so it drew every branch while the toggle read on. A tree
  // drawn while Focus branch is on is zoomed to the last request's node from its first render,
  // that node's branch alone, as the tree before it was (`branchAtMount`). It is no new request,
  // so it moves no focus: the reader is taken nowhere, and the opening stays inside the branch.
  const [focusBranch] = useFocusBranch();
  const branchAtMount = useRef(focusBranch ? request?.id : undefined);
  const [zoom, setZoom] = useState<string | undefined>(branchAtMount.current ?? zoomAt);
  // The node the address last zoomed the tree to, so an address that names another zooms to it.
  const [zoomedAt, setZoomedAt] = useState(zoomAt);
  if (zoomAt !== zoomedAt) {
    setZoomedAt(zoomAt);
    if (zoomAt !== undefined) setZoom(zoomAt);
  }
  // How many times `z` has unfolded every row the reader folded, which places the focused row
  // once each unfolding is drawn.
  const [unfoldedAll, setUnfoldedAll] = useState(0);
  // The node a key is taking the reader to, once the row it asked for is on the page.
  const [wanted, setWanted] = useState<string | undefined>(undefined);
  // The last zoom the reader made, which the effect that places a zoom places once its rows and
  // the focus are on the page (`zoomTo`): whether it draws the crumb line (`lined`). Each zoom is
  // a new value, so a zoom made before the last one's placing has been drawn is placed too, and
  // each is placed once.
  const [zoomMade, setZoomMade] = useState<{ readonly lined: boolean }>();
  // What a key reported when it had nowhere to go, and the row it was pressed on: the row draws
  // it, so the reader reads it where they are, and the next key, move or leaving the tree lets
  // it go. `row` is undefined where the key was pressed with no row focused.
  const [report, setReport] = useState<{ readonly line: string; readonly row?: string }>();
  const setFocusedNode = useSetFocusedNode();
  const writer = useOutlineWriter();
  const takeRequest = useTakeOutlineRequest();
  // The reader, whose own new words a write from the line takes them to.
  const reader = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  const body = useRef<HTMLDivElement | null>(null);
  const crumbLine = useRef<HTMLParagraphElement | null>(null);
  // The failed reads' lines over the tree, and the one whose Retry the page opens on where no
  // question that waits on the reader is drawn (`opening`): the asks', which bring the questions
  // the issue's own read does not hold, where it is one of them, else the first whose read the
  // rows come from; -1 where none of them is.
  const lines = useRef<HTMLDivElement | null>(null);
  const asksLine = failedReads.findIndex((read) => read.bringsQuestions);
  const retryLine = asksLine !== -1 ? asksLine : failedReads.findIndex((read) => read.placesRows);
  const reportHere = useCallback((line: string) => {
    if (line === "") {
      setReport(undefined);
      return;
    }
    const row = focusedRowIn(body.current)?.dataset.rowKey;
    // The same words at the same row keep the report as it is: a key held at the tree's end says
    // its words at every repeat, and each new report drew the whole tree again for nothing.
    setReport((held) =>
      held?.line === line && held.row === row
        ? held
        : { line, ...(row === undefined ? {} : { row }) }
    );
  }, []);
  const { edit, editing, stopEditing } = useNodeEditing({
    artifacts,
    container: body,
    onEditing,
    onReport: reportHere,
  });

  const target = reveal?.at(-1);
  // Each following of an address that names a node lands on that node's row (`addressKey`), so
  // a zoom the reader holds on a branch the node is outside of is left as the following is first
  // drawn, once for it: zoomed elsewhere, the tree drew no row for the node and the landing gave
  // up with nothing said. A zoom on the chain down to the node - the node itself or a branch
  // above it - draws its row, and stays.
  const landingKey = target === undefined ? undefined : `${addressKey ?? ""}:${target.id}`;
  const [zoomLeftFor, setZoomLeftFor] = useState<string | undefined>(undefined);
  if (focusReveal && landingKey !== zoomLeftFor) {
    setZoomLeftFor(landingKey);
    if (landingKey !== undefined && zoom !== undefined && !reveal?.some((up) => up.id === zoom)) {
      setZoom(undefined);
    }
  }
  // The rows of the address's path the reader folded since its landing (`landingKey`), by their
  // places: each is let go of the path's hold for that following. They are forgotten as soon as
  // the page follows anything else, so the next following holds the whole path again: Back and
  // Forward give an earlier entry's own location key back, and a fold kept under that key drew
  // the node that entry names nowhere, and its landing never landed.
  const [released, setReleased] = useState<{
    readonly landing: string | undefined;
    readonly places: ReadonlySet<string>;
  }>({ landing: undefined, places: NO_FOLDS });
  if (released.landing !== undefined && released.landing !== landingKey) {
    setReleased({ landing: undefined, places: NO_FOLDS });
  }
  // The places of the address's path and its following as last drawn, which a fold reads
  // (`unfold`).
  const pathHold = useRef<{ readonly landing?: string; readonly places: readonly string[] }>({
    places: [],
  });
  // A reader who zoomed into a node reads the tree from there: the node is the tree's root, the
  // path above it the breadcrumb, and everything below it folded as they left it.
  const rootedPath = (zoom === undefined || zoom === root.id ? undefined : pathTo(root, zoom)) ?? [
    root,
  ];
  const rooted = rootedPath.at(-1) ?? root;
  // The node the rows are drawn from where that is not the issue: the zoom's own row, which is
  // drawn open by definition. Undefined where the tree is drawn from the issue, which is no row.
  const zoomRoot = rooted.id === root.id ? undefined : rooted.id;
  // The address's own path is drawn open over the reader's folds: a row of that path the reader
  // folded is opened again, since the row the address names is drawn nowhere while anything
  // above it is folded. The hold is on the places of the path alone, so another row of a node on
  // it stays as the reader left it. The hold is there to land the reader: a row of the path they
  // fold after it, and every row below it on the path, is theirs to fold for that following
  // (`released`), since held open it recorded a fold that drew nothing and Left said nothing.
  const pathPlaces = placesAlong((reveal ?? []).map((above) => above.id));
  pathHold.current = { landing: landingKey, places: pathPlaces };
  const releasedHere = released.landing === landingKey ? released.places : NO_FOLDS;
  const shown = withUnfolded(
    opened,
    releasedHere.size === 0 ? pathPlaces : pathPlaces.filter((place) => !releasedHere.has(place))
  );
  // The fold is seeded with the path the breadcrumb holds, so a row is keyed by its path from
  // the issue whether or not the reader has zoomed: the row they are editing, the tree's one
  // tab stop, the rows they folded and the pages they drew are held by that key, and a zoom
  // either way leaves them where they are (`rowKeyOf`). The node the address names and the node
  // a key or a write is taking the reader to, each named by its id alone, and the row the tab
  // stop rests on, named by its own key, are each drawn with every child before it, past its
  // parent's page where it stands there (R15).
  const rootedAbove = rootedPath.slice(0, -1).map((one) => one.id);
  const named = [target?.id, wanted].filter((id): id is string => id !== undefined);
  const drawn = treeRows(rooted, shown, rootedAbove, named, stop === undefined ? [] : [stop]);
  // The issue is no row: the header is the issue, so the nodes under it are the tree's top
  // level, with no filter on as under one. A zoom into a node keeps that node's own row, which
  // is the node the reader reads from there.
  const head = drawn[0];
  const rows =
    head?.kind === "node" && head.node.id === root.id
      ? drawn.slice(1).map((row) => ({ ...row, level: row.level - 1 }))
      : drawn;
  const breadcrumb: readonly TreeNode[] = zoom === undefined ? [] : zoomBreadcrumb(root, zoom);
  // Where each thread of the issue is marked read in the reader's Inbox: its newest write's row.
  const threads = useMemo(() => threadMarks(root), [root]);

  // A row the page takes the reader to of its own accord - the node an address names, the open
  // question the page opens on - takes the focus while the outline's other reads are still
  // arriving, and each pushes it down the page, out of sight: the row is focused *and on
  // screen*, and held there until the reader acts or the focus leaves it (`holdOnScreen`), one
  // row at a time. It takes it in the frame after the commit that draws it (`landOnRow`), and
  // until then the landing is what this holds.
  const onScreenHold = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => onScreenHold.current?.abort(), []);
  // The open question the page opens on where its row is not drawn yet, whose row is held once
  // the render that unfolds the path to it has drawn it and given it the focus (`unfoldTo`). The
  // reader acting before then lets it go. A question already drawn is landed on as an address's
  // node is (`landOnRow`).
  const holdOnFocus = useRef<string | undefined>(undefined);
  // The outline's first row the page opens on where no question waits on the reader, which does
  // not place itself on screen while it holds that focus (`NodeRow`'s `placesItself`): the page's
  // one rule puts a row taller than the screen at its top under the band, and the page opened
  // scrolled past its header - 89 px at 1440 by 900 and 161 px at 390 by 664 on a long answered
  // question - where it is to stay at 0 px. The
  // opening names the row in `openingRow` while it focuses it, which tells that focus from every
  // other; the reader's first act, or the focus going to any other row or out of the tree, ends
  // it. The row a zoom leaves the reader on is the other that does not place itself, until the
  // reader's next act (`zoomTo`); `zoomLanding` tells the focus a zoom key gives a row, from the
  // key until the zoom is placed, from every other.
  const [unplaced, setUnplaced] = useState<string | undefined>(undefined);
  const openingRow = useRef<string | undefined>(undefined);
  const zoomLanding = useRef(false);

  const takeFocus = useCallback(
    (row: string, node: TreeNode) => {
      setFocused(row);
      setStop(row);
      setUnplaced(row === openingRow.current || zoomLanding.current ? row : undefined);
      // A move to a row is the reader going on from what a key reported.
      setReport(undefined);
      setFocusedNode(focusedNodeOf(node));
      if (holdOnFocus.current === node.id) {
        holdOnFocus.current = undefined;
        const element = body.current?.querySelector<HTMLElement>(rowSelectorOf(row));
        if (element !== null && element !== undefined) holdInPlaceOf(element, onScreenHold);
      }
    },
    [setFocusedNode]
  );
  // The focus a press gives a row is taken once the press is over, and only while that row
  // still holds the focus; a key's move takes it at once (`usePressFocus`).
  const press = usePressFocus<{ readonly node: TreeNode; readonly row: string }>(
    ({ node, row }) => takeFocus(row, node),
    ({ row }) =>
      body.current?.querySelector(rowSelectorOf(row))?.contains(document.activeElement) === true
  );
  // The focus leaving the tree lets go of the row it left: its focused look and its own placing
  // on screen, the opening's row and what a key reported. By Tab or a script it goes at once,
  // and for a press on something else of the page once that press has ended and its click has
  // landed (`useReleaseFocus`, which keeps them while a row is being edited, until the editing
  // ends with the focus out of the tree, and through a press outside the tree, so nothing moves
  // under it).
  const leave = useCallback(() => {
    setFocused(undefined);
    setUnplaced(undefined);
    setReport(undefined);
  }, []);
  const releaseFocus = useReleaseFocus({ editing: editing !== undefined, onLeave: leave });
  // What the twisty, the arrow keys and `o` do to one row, by its key: the reader's fold, which
  // lasts for the load and leaves every other row of the node as it was (`tree-fold.ts`'s
  // `withFold`).
  const unfold = useCallback((row: string, open: boolean) => {
    setOpened((held) => withFold(held, row, open));
    // A fold of a row the address's path holds open lets that row, and the path below it, go of
    // the hold for this following, so the fold is drawn.
    const { landing, places } = pathHold.current;
    const at = open ? -1 : places.indexOf(row);
    if (at !== -1) {
      setReleased((held) => ({
        landing,
        places: new Set([...(held.landing === landing ? held.places : []), ...places.slice(at)]),
      }));
    }
  }, []);
  // What `z` does: every row the reader folded is unfolded (`withEveryFoldUndone`), and the
  // focused row is placed once the tree is drawn again (`unfoldedAll`).
  const unfoldAll = useCallback(() => {
    setOpened(withEveryFoldUndone);
    setUnfoldedAll((held) => held + 1);
  }, []);
  // A zoom the reader makes - `.` and `,`, a crumb - roots the tree at a node and draws the crumb
  // line above its rows, which is placed once they are drawn (`zoomMade`). The row it leaves the
  // reader on - the one that keeps the focus through it, a crumb's press included, or the one the
  // key takes the focus to (`zoomLanding`) - places nothing of its own from the render that draws
  // the zoom until the reader's next act, so nothing of the row's undoes where the line and the
  // row are placed. A zoom back out to the issue draws no line: the row it moves the focus to
  // places itself as every move's does, and the row that keeps the focus through it - `,` on the
  // zoom's own row, or a press on the issue's crumb - is `zoomKept`, which the zoom places once the
  // whole outline is drawn (`zoomMade`), and lets go of then: held on, it kept alive a row a
  // later read had replaced.
  const zoomKept = useRef<HTMLElement | null>(null);
  const rootId = root.id;
  const zoomTo = useCallback(
    (id: string) => {
      setZoom(id);
      const lined = id !== rootId;
      const row = focusedRowIn(body.current);
      zoomLanding.current = lined;
      zoomKept.current = lined ? null : row;
      setZoomMade({ lined });
      setUnplaced(lined ? row?.dataset.rowKey : undefined);
    },
    [rootId]
  );

  // Unfolds a path down from the issue, `above`, a chain of node ids, zooming nowhere and moving
  // no focus: each row of it the reader folded is opened, at that path's places alone.
  const unfoldPath = useCallback((above: readonly string[]) => {
    const places = placesAlong(above);
    setOpened((held) => withUnfolded(held, places));
  }, []);
  // Unfolds the path down to one node where the tree stands, zooming nowhere (`unfoldPath`), and
  // the row takes the focus once this render has drawn it. Named by its id alone, the node is
  // gone to at the placement `pathTo` finds, its first in reading order. Answers the ids above
  // the node, the outermost first.
  const unfoldTo = useCallback(
    (id: string): readonly string[] => {
      const above = (pathTo(root, id) ?? []).slice(0, -1).map((node) => node.id);
      unfoldPath(above);
      setWanted(id);
      return above;
    },
    [root, unfoldPath]
  );
  // Takes the reader to one node, wherever it is: the path down to it is unfolded, a zoom it
  // sits outside of is left, and the row takes the focus once this render has drawn it.
  const goToNode = useCallback(
    (id: string) => {
      const above = unfoldTo(id);
      // A zoom that already names the target, or a node on the path down to it, is left alone:
      // that is `.`, which zooms into the focused node and then takes the reader to it
      // (`useTreeKeymap.ts`'s `tree-zoom-in`). A zoom the target sits outside of is left for the
      // whole issue, held as the issue's own id.
      setZoom((held) =>
        held === undefined || held === id || above.includes(held) ? held : rootId
      );
    },
    [rootId, unfoldTo]
  );
  /** The row of `id` where it is drawn already, focused where it stands; else `goToNode`, which
   *  unfolds the path to it and draws it past its parent's page. */
  const reach = useCallback(
    (id: string) => {
      const row = body.current?.querySelector<HTMLElement>(nodeRowSelectorOf(id));
      if (row === null || row === undefined) goToNode(id);
      else row.focus();
    },
    [goToNode]
  );

  useEffect(() => {
    if (wanted === undefined) return;
    setWanted(undefined);
    body.current?.querySelector<HTMLElement>(nodeRowSelectorOf(wanted))?.focus();
  }, [wanted]);

  // A zoom draws the crumb line above the rows it roots the tree at, and the line is the way
  // back out, so it is brought below the band that stays at the top by the page's one rule
  // (`bringOnScreen`). Nothing else places it: the row the zoom leaves the reader on keeps the
  // focus it held, or takes it where the key put it, so a row the page had put just under the
  // band kept its place and the line drawn above it stood under the band - at 390 by 664 px on
  // a long issue, the crumb line at -5 to 39 px under a band 51 px tall. The row is placed after
  // the line only where its first line is not on screen: placed by the page's rule, a row taller
  // than the room under the band went to its top just under the band and took the line back
  // under it, as it did again each time it grew - rows of 1,634 and 1,464 px left the line at
  // 15 to 59 and at -53 to 59 px under a band 51 px tall. Until the reader's next act the row
  // places nothing of its own (`zoomTo`). A zoom back out to the issue draws the whole outline
  // again around the row that kept the focus through it (`zoomKept`), which placed nothing of its
  // own: at 1440 by 900 px on a long issue it stood 2,000 to 40,000 px below the window until
  // the next key. It is brought on screen and held as the row `z` leaves the focus on is
  // (below); a row the zoom moved the focus to is another element, which placed itself. This runs
  // after the effect above, which focuses the row a zoom key asked for, so the row it reads is
  // that one.
  const zoomHold = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => zoomHold.current?.abort(), []);
  useEffect(() => {
    if (zoomMade === undefined) return;
    if (!zoomMade.lined) {
      const kept = zoomKept.current;
      zoomKept.current = null;
      if (kept === null || focusedRowIn(body.current) !== kept) return;
      bringOnScreen(kept);
      holdInPlaceOf(kept, onScreenHold);
      return;
    }
    zoomLanding.current = false;
    zoomHold.current?.abort();
    zoomHold.current = abortOnReaderAct(() => setUnplaced(undefined));
    const line = crumbLine.current;
    if (line !== null) bringOnScreen(line);
    const row = focusedRowIn(body.current);
    if (row !== null && !firstLineOnScreen(row)) bringOnScreen(row);
  }, [zoomMade]);

  // `z` unfolds every row the reader folded above the focused row and moves no focus, so
  // nothing would place the row as the rows above it grew and pushed it off the screen. A key
  // that moves no focus still keeps the focused row on screen, as every move does, so once the
  // unfolded tree is drawn the row is brought on screen by the page's one rule and held there
  // while the rows above it still grow (`holdOnScreen`), as a row the page takes the reader to
  // is, until the reader's next act.
  const unfoldDrawn = useRef(unfoldedAll);
  useEffect(() => {
    if (unfoldDrawn.current === unfoldedAll) return;
    unfoldDrawn.current = unfoldedAll;
    const row = focusedRowIn(body.current);
    if (row === null) return;
    bringOnScreen(row);
    holdInPlaceOf(row, onScreenHold);
  }, [unfoldedAll]);

  // The controls that draw more of the outline in place (R15): the row after a node's drawn
  // children; while the outline read answers older comments and the tree is drawn from the
  // issue, the control above its first row; and while it answers a next page, the control after
  // its last row. A press of the first two replaces it with what it drew and moves no focus
  // (`TreeRows.tsx`'s `PageControl`); a key's press, and any press of the last, leaves the focus
  // on the first row it drew (`drawnFrom`). A page of children names that row at once. A read
  // names the rows drawn before the press, and the first row it draws after it that none of them
  // is takes the focus, until the reader's next act (`olderActs`).
  const olderAbove = older?.remain === true && zoomRoot === undefined ? older : undefined;
  const [drawnFrom, setDrawnFrom] = useState<
    { readonly key: string } | { readonly before: ReadonlySet<string> }
  >();
  const olderActs = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => olderActs.current?.abort(), []);
  const drawPage = (page: PageRow, focus: boolean): void => {
    setOpened((held) => withNextPage(held, page));
    const next = page.of.children[page.of.children.length - page.left];
    if (!focus || next === undefined) return;
    const trail = [...page.trail, page.of.id];
    setDrawnFrom({ key: rowKeyOf({ kind: "node", level: page.level, node: next, trail }) });
  };
  // The rows drawn now, so the first row a read draws after them takes the focus.
  const focusFirstRead = (): void => {
    olderActs.current?.abort();
    olderActs.current = abortOnReaderAct(() => setDrawnFrom(undefined));
    setDrawnFrom({
      before: new Set(nodeRowKeys(rows)),
    });
  };
  const readOlder = (focus: boolean): void => {
    if (olderAbove === undefined) return;
    if (focus) focusFirstRead();
    // A refused page leaves the answer as it was, and the control stays for another press
    // (`readOlderComments`); the focus it was to give goes with the reader's next act.
    olderAbove.read().catch(() => undefined);
  };
  const readMore = (): void => {
    if (more === undefined) return;
    focusFirstRead();
    // A refused page, or one that names a cursor the read asked for already, leaves the answer as
    // it was (`readNextTreePage`); the focus it was to give goes with the reader's next act.
    more.read().catch(() => undefined);
  };
  useEffect(() => {
    if (drawnFrom === undefined) return;
    const key =
      "key" in drawnFrom
        ? drawnFrom.key
        : nodeRowKeys(rows).find((one) => !drawnFrom.before.has(one));
    const row =
      key === undefined ? null : body.current?.querySelector<HTMLElement>(rowSelectorOf(key));
    // A page of children draws its first row in the render its press asked for; the older
    // comments wait for the read that draws them.
    if (row == null && !("key" in drawnFrom)) return;
    setDrawnFrom(undefined);
    olderActs.current?.abort();
    if (row != null) moveToRow(row);
  }, [drawnFrom, rows]);

  // A `q` or `w` press whose target no page read yet holds: reads the next pages until one brings
  // it and lands on it, or reports none once no page is left (R15). The reader's next act ends it.
  const [searching, setSearching] = useState<TreeSearch | undefined>(undefined);
  const searchActs = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => searchActs.current?.abort(), []);
  const startSearch = useCallback((search: TreeSearch) => {
    searchActs.current?.abort();
    searchActs.current = abortOnReaderAct(() => setSearching(undefined));
    setSearching(search);
  }, []);
  useEffect(() => {
    if (searching === undefined) return;
    const endSearch = () => {
      searchActs.current?.abort();
      setSearching(undefined);
    };
    const found =
      searching.kind === "question"
        ? nextQuestion(root, searching.from)
        : decisionBehind(root, searching.from);
    if (found !== undefined) {
      endSearch();
      goToNode(found.id);
      return;
    }
    if (more === undefined) {
      endSearch();
      if (readsHeld) {
        reportHere(
          searching.kind === "question" ? KEY_REPORTS.no_next_question : KEY_REPORTS.no_decision
        );
      }
      return;
    }
    more.read().catch(endSearch);
  }, [goToNode, more, readsHeld, reportHere, root, searching]);

  // The Contents list's asks, each acted on once. With Focus branch on, the outline is zoomed to
  // the node, which is then the tree's first row and takes the focus; turned off, the zoom is
  // left and the reader is taken to the node in the whole outline, as any other move takes them.
  // What was acted on is held by `outline-request.tsx`'s provider, above this tree, so a tree
  // drawn again - a view drawn in its place and turned off - does not move the reader again for
  // an ask made for the tree as it stood before.
  useEffect(() => {
    if (request === undefined || !takeRequest(request)) return;
    if (request.focusBranch && request.id !== undefined) {
      setZoom(request.id);
      setWanted(request.id);
      return;
    }
    setZoom(undefined);
    if (request.id !== undefined) goToNode(request.id);
  }, [goToNode, request, takeRequest]);

  useTreeKeymap({
    container: body,
    cycleLabel: "Unfold every row you folded",
    editing: editing !== undefined,
    holdsEscape,
    onCycle: unfoldAll,
    onEdit: edit,
    onEditEnd: stopEditing,
    onFold: unfold,
    ...(more === undefined ? {} : { onMore: readMore }),
    ...(olderAbove === undefined ? {} : { onOlder: () => readOlder(true) }),
    onPage: (page) => drawPage(page, true),
    onReport: reportHere,
    onReveal: goToNode,
    ...(more === undefined ? {} : { onSearch: startSearch }),
    onZoom: zoomTo,
    readsHeld,
    rows,
    root,
    writer,
    // The node the rows are drawn from, not the zoom asked for (undefined for the issue where the
    // whole tree is drawn): a filter that leaves the zoomed node out, or a read that drops it,
    // draws from the issue while the zoom is kept for when the row comes back, and Left and `,`
    // there moved nothing and said nothing, since they read a zoom no row stood under.
    zoom: zoomRoot,
  });

  // The tree is one tab stop, and it rests where the reader last stood: before they have
  // touched it that is its first row, so Tab reaches the tree rather than passing it by, and
  // after they have left it that is the row they left, so Shift+Tab brings them back to it.
  const keys = rows.map(rowKeyOf);
  const first = rows.find((row) => row.kind === "node");
  const tabStop =
    stop !== undefined && keys.includes(stop)
      ? stop
      : first === undefined
        ? undefined
        : rowKeyOf(first);

  // An address that names a node puts the focus on it, in the frame after the commit that draws
  // its row (`landOnRow`). The row is not on the page until the read that holds it has answered,
  // and the document the preset draws beside the tree mounts later still and can leave the focus
  // nowhere as it does, so this looks again on every tree the page draws and takes the focus
  // back whenever nothing holds it (`focusIsLoose`: the page itself, or the element the app
  // focuses at a page change, which holds the tree), until the reader's first act since they
  // followed the address (`addressActs`; `READER_ACTS`, which end `acts`). A reader who acted
  // before the row drew - the reads still out, a press on another control, a key, a turn of the
  // wheel - keeps the focus where they put it and the page where they had it: the landing does
  // nothing to the page - no focus, no scroll, no row's hold ended, no report - and the row is
  // drawn where it stands, the branches the address opens opened. Watched only from the row's
  // drawing, a row that drew late took the focus from the control they had moved to; brought on
  // screen alone, it took the page away from the place they had scrolled to or the row they had
  // moved to, and ended that row's hold. Whatever else holds the focus keeps it, and from the
  // reader's first act the focus is theirs wherever they put it, nowhere included - a second
  // Escape, a press on blank space: the effect takes it no more until the address is followed
  // again (`landingKey`: the following and the node), to this node or another. Keyed on the node
  // alone, a link to the node the reader had moved away from, followed again, landed nowhere; a
  // tree drawn again at the same following lands nowhere again. A tree drawn before the landing's
  // frame comes - a read answering - leaves that landing waiting (`landingAsked`): asked again, it
  // was put off by a frame at each render. The issue itself is no row: an address lands on it for
  // a turn of a thread only agents wrote in whose nearest drawn ancestor is the issue
  // (`slots/body.tsx`), and the row it lands on is then the tree's tab stop, where the tree says
  // what has no row of its own. What the landing says (`landingReport`) is said at the row once it
  // holds the focus: the focus moving there lets go of what was said before. The page's own
  // opening on its first row or its question (below) keeps its own rule.
  const taken = useRef<{ readonly acts: AbortController; readonly key: string } | undefined>(
    undefined
  );
  useEffect(() => () => taken.current?.acts.abort(), []);
  const landingAsked = useRef<
    { readonly row: HTMLElement; readonly signal: AbortSignal } | undefined
  >(undefined);
  const targetId = target?.id;
  useEffect(() => {
    if (landingKey === undefined || targetId === undefined || !focusReveal) return;
    const held = taken.current?.key === landingKey ? taken.current : undefined;
    const loose = focusIsLoose(body.current);
    if (held !== undefined && (held.acts.signal.aborted || !loose)) return;
    const selector =
      targetId === rootId
        ? tabStop === undefined
          ? undefined
          : rowSelectorOf(tabStop)
        : rows.some((row) => row.kind === "node" && row.node.id === targetId)
          ? nodeRowSelectorOf(targetId)
          : undefined;
    const row = selector === undefined ? null : body.current?.querySelector<HTMLElement>(selector);
    if (row === null || row === undefined) return;
    if (held === undefined) {
      taken.current?.acts.abort();
      const acted = addressActs?.current;
      if (acted?.signal.aborted === true) {
        // The landing ends as it starts: the reader has acted since the following, and the page
        // stays as they left it.
        taken.current = { acts: acted, key: landingKey };
        return;
      }
      taken.current = { acts: abortOnReaderAct(), key: landingKey };
    }
    // The landing on this row still waits for its frame.
    const asked = landingAsked.current;
    if (asked?.row === row && !asked.signal.aborted) return;
    const landed = landingReport === undefined ? undefined : () => reportHere(landingReport);
    landingAsked.current = { row, signal: landOnRow(row, onScreenHold, landed).signal };
  }, [
    addressActs,
    focusReveal,
    landingKey,
    landingReport,
    reportHere,
    rootId,
    rows,
    tabStop,
    targetId,
  ]);

  // The page opens with the keyboard focus on the first open question that waits on the reader,
  // in the outline's own order, its path unfolded and its row in the viewport; where none waits
  // on them, on the outline's first row. It is the page opening: with no filter on, on an
  // address that names no node, before the reader has pressed, scrolled or touched anything and
  // while nothing else holds the focus. The reads arrive one after another, and a row drawn
  // before a later read could take the focus and then lose it to the row that read drew above
  // it a second later, with no act of the reader's: a second announcement to a screen reader,
  // and a key pressed in that second acted on the wrong row. So the opening waits until every
  // read the rows come from has answered (`readsSettled`), and places the focus once: on the
  // question, or on the first row, which it never moves from after. Each focus waits for the
  // frame after its row is drawn for the reason an address's node does (`landOnRow`): given in
  // the commit that draws the row, it laid the whole new page out inside the reader's press.
  // The question is where it stays until the reader acts, waiting on them or answered since, and
  // its row is held on screen while what is drawn above it settles: the page scrolls for the
  // question alone. A read that draws its row again under another key takes its old element
  // out of the page, and the browser drops the focus with it: the opening gives it back to the
  // same node's new row, the question's or the first row's (`openingMayFocus`).
  // A read that takes the question's row out of the page with the focus - one that draws it
  // under another parent - is answered by unfolding the
  // path to it again where the tree stands, zooming nowhere (`unfoldTo`). A tree drawn again
  // zoomed to Focus branch's node (`branchAtMount`) is no page opening on what waits: it opens
  // on the branch's first row, the node itself, where it stands, since going to a question would
  // leave the zoom for one outside the branch, drawing every branch while the toggle read on.
  // Where no question that waits on the reader is drawn as the reads settle and a read the
  // rows come from stands failed, the opening lands on a failed read's Retry instead (below).
  // `row` is the row the opening last focused, or the question's row or the Retry from when it
  // lands there, whose landing (`landing`, with the focus it found) tells whose the focus is until
  // its frame comes; `next` is the row whose focus it has asked of the next frame and not given
  // yet, `question` the question it opened on, `placed` the node of the first row it opened on,
  // and `failed` whether it goes to a failed read's Retry, as the reads settled or as the Retry
  // it went to left the page before its frame (below).
  const opening = useRef<{
    done: boolean;
    failed?: boolean;
    landing?: { readonly from: Element | null; readonly signal: AbortSignal };
    next?: { readonly frame: number; readonly row: HTMLElement };
    placed?: string;
    question?: string;
    row?: HTMLElement;
  }>({ done: false });
  useEffect(() => {
    const settle = abortOnReaderAct(() => {
      opening.current.done = true;
      holdOnFocus.current = undefined;
      setUnplaced(undefined);
    });
    return () => settle.abort();
  }, []);
  useEffect(() => {
    const held = opening.current;
    if (held.done) return;
    if (!rootRow || targetId !== undefined) {
      held.done = true;
      return;
    }
    if (!readsSettled) return;
    // The question that waits on the reader, which the page opens on wherever the tree holds it,
    // whatever reads failed: the one it opened on, or the first in the outline's order where it
    // has opened on none and has placed no first row. A tree drawn zoomed - again on Focus
    // branch's node, or opened at Graph's `/graph/<node id>` - opens on none, so the opening
    // stays inside the branch rather than leaving the zoom for a question outside it (above).
    // The outline is walked for it once a run at most, and only where it is read: on the way to
    // the question or the first row, and where whether the opening goes to a Retry is decided
    // (below).
    let waiting: { readonly id: string | undefined } | undefined;
    const waitingQuestion = (): string | undefined => {
      waiting ??= {
        id:
          held.question ??
          (branchAtMount.current === undefined && zoomAt === undefined && held.placed === undefined
            ? nodesInOrder(root).find((node) => node.type === "ask" && node.waiting)?.id
            : undefined),
      };
      return waiting.id;
    };
    // Where none is drawn as the reads settle and a read the rows come from stands failed, the
    // question that waits on the reader may be one it failed to bring, so the page does not open
    // on the first row as if none waited: it opens on the Retry of the asks' line where they
    // stand failed, since a question the issue's own read does not hold among its open asks comes
    // from them alone, else of the first line whose read the rows come from, over the tree, as
    // the stage gives a view's failed read's Retry the focus, in the frame after it is drawn
    // (`landOnRow`), and the opening is done once the Retry holds the focus. Whether it goes to a
    // Retry is read as the reads settle, so a read that fails later moves no focus, and read again
    // by the same rule where the Retry it went to leaves the page before its frame comes, its read
    // answered: the opening is handed on to the question that waits on the reader where the reads
    // now draw one, else to the Retry of the next line whose read the rows come from, where
    // another such read still stands failed, else to the first row. The names place no row, so a
    // failed names read changes nothing here; each line is drawn in the order the reads are
    // given, one Retry each.
    if (held.failed === undefined || (held.failed && held.row?.isConnected === false)) {
      held.failed = retryLine !== -1 && waitingQuestion() === undefined;
    }
    const retry =
      held.failed && retryLine !== -1
        ? lines.current?.querySelectorAll<HTMLElement>(FAILED_READ_RETRY)[retryLine]
        : undefined;
    if (retry !== undefined) {
      // Its landing still waits for its frame.
      if (held.landing?.signal.aborted === false && held.row === retry) return;
      if (!openingMayFocus(body.current, held.row)) {
        held.done = true;
        return;
      }
      held.row = retry;
      held.landing = {
        from: document.activeElement,
        signal: landOnRow(retry, onScreenHold, () => {
          held.done = true;
        }).signal,
      };
      return;
    }
    const question = waitingQuestion();
    const questionRow =
      question === undefined
        ? undefined
        : body.current?.querySelector<HTMLElement>(nodeRowSelectorOf(question));
    const active = document.activeElement;
    // The question's row holds the focus the opening gave it, by its landing or in the render
    // that unfolded the path to it (`unfoldTo`): it is the row the opening focused.
    if (questionRow?.contains(active) === true) held.row = questionRow;
    // Until its frame, the landing on the question is what tells whether the focus is still the
    // page's (`landOnRow`): nothing holds it, or it is where the landing found it - the app's
    // main element, which the page's change of address focuses - or on the question's row. A
    // focus a script put on anything else meanwhile ends the opening, as it would have once the
    // row held it.
    const landingWaits = held.landing?.signal.aborted === false;
    const mayFocus = landingWaits
      ? focusIsLoose(body.current) ||
        active === held.landing?.from ||
        held.row?.contains(active) === true
      : openingMayFocus(body.current, held.row);
    if (!mayFocus) {
      held.done = true;
      return;
    }
    if (question !== undefined) {
      // The first row's focus the next frame was to give is the question's now.
      if (held.next !== undefined) cancelAnimationFrame(held.next.frame);
      held.next = undefined;
      const openedOn = held.question !== undefined;
      held.question = question;
      if (questionRow === null || questionRow === undefined) {
        if (!openedOn) {
          holdOnFocus.current = question;
          goToNode(question);
          return;
        }
        // The question's row the opening focused has left the page: the path to it is unfolded
        // again, once for that row, where the tree stands. Gone to as a key goes to a node
        // (`goToNode`), it left the zoom the reader stood in.
        if (held.row?.isConnected !== false) return;
        held.row = undefined;
        holdOnFocus.current = question;
        unfoldTo(question);
        return;
      }
      // The question's row holds the focus, or its landing still waits for its frame.
      if (questionRow.contains(active)) return;
      if (landingWaits && questionRow === held.row) return;
      held.row = questionRow;
      held.landing = { from: active, signal: landOnRow(questionRow, onScreenHold).signal };
      return;
    }
    // Once placed, the focus is the placed node's: a row drawn above it later takes none. A
    // read that draws the placed node nowhere, its parent folded over it, ends the opening, and
    // the focus it dropped goes to the row now at its place, as any dropped row's does (below).
    const first = rows.find(
      (row) => row.kind === "node" && (held.placed === undefined || row.node.id === held.placed)
    );
    if (first?.kind !== "node") {
      if (held.placed !== undefined) held.done = true;
      return;
    }
    const key = rowKeyOf(first);
    const placed = first.node.id;
    const row = body.current?.querySelector<HTMLElement>(rowSelectorOf(key));
    // The row the next frame is to focus is still the first.
    if (row === null || row === undefined || row === held.next?.row) return;
    if (held.next !== undefined) cancelAnimationFrame(held.next.frame);
    held.next = undefined;
    // The row the opening focused is still the first, and holds the focus.
    if (row === held.row) return;
    const frame = requestAnimationFrame(() => {
      held.next = undefined;
      if (held.done || !row.isConnected) return;
      // A focus something else took before the frame - a script's, which redraws no tree -
      // ends the opening as it would have once the row held it.
      if (!openingMayFocus(body.current, held.row)) {
        held.done = true;
        return;
      }
      held.row = row;
      held.placed = placed;
      openingRow.current = key;
      row.focus({ preventScroll: true });
      openingRow.current = undefined;
    });
    held.next = { frame, row };
  }, [goToNode, readsSettled, retryLine, root, rootRow, rows, targetId, unfoldTo, zoomAt]);

  // A read can answer without the node whose row holds the focus - a comment deleted - and the
  // row then leaves the tree with the focus, which the browser drops to the page's body: the
  // reader's place in the outline was lost, and no row held the focus. So where a render no
  // longer draws the row that held the focus as the tree was last drawn, and nothing holds the
  // focus now (`focusIsLoose`), the nearest row still drawn takes it as a key's move gives it
  // (`moveToRow`): the tab stop and the row's look go with it, and the row brings itself on screen
  // clear of the band. Whatever holds the focus by then keeps it - a control, the editor, another
  // row - and so does the page's own placing of it, which runs first: the opening, which gives a
  // row it focused back where a read drew that row again (`opening`), and an address's landing
  // still waiting for its frame (`landingAsked`). The row is noted at every render, by its key and
  // its place among the rows the focus can take, as the focus stands once that render is drawn:
  // the render that drops the row no longer draws it, so where it stood is the render before's to
  // tell. Its key is that of the row the focus is in, not `focused`, which names the row a press
  // took the focus from until the press is over (`usePressFocus`). Nothing has let the dropped
  // row go by the time this runs: the blur Chromium sends as React takes the focused row out comes
  // during React's commit, where React hears no event, so `leave` never runs for it.
  //
  // A render that no longer draws the row being edited ends its editing as Escape ends it
  // (`stopEditing`), the page's editor of the document and its box with it, wherever the focus
  // is: in that row, which takes the editor's focus out with it, before the nearest row takes the
  // focus (above); on another row; or out of the tree - in Details, where a press leaves the
  // editing on (`useNodeEditing.ts`), with no row noted as holding the focus at the render
  // before. The frame that end asks for, to give the row the focus back (`leaving`), then finds no
  // row and moves no focus. Left on, the editing waited for a row no read draws
  // (`useNodeEditing.ts`), and the tree held every row the focus came to as the one being edited
  // (`useReleaseFocus`): a Tab out of the tree kept that row's body and its facts in Details.
  const heldRow = useRef<{ readonly index: number; readonly key: string } | undefined>(undefined);
  const editingRow = editing?.row;
  useEffect(() => {
    const tree = body.current;
    const order = rows.flatMap((row) => (row.kind === "node" ? [rowKeyOf(row)] : []));
    const held = heldRow.current;
    if (editingRow !== undefined && !order.includes(editingRow)) stopEditing();
    if (held !== undefined && !order.includes(held.key)) {
      if (
        focusIsLoose(tree) &&
        opening.current.done &&
        landingAsked.current?.signal.aborted !== false
      ) {
        // The nearest row is the one now at the dropped row's place, or the last where none is.
        const key = order[Math.min(held.index, order.length - 1)];
        const nearest =
          key === undefined ? null : tree?.querySelector<HTMLElement>(rowSelectorOf(key));
        if (nearest !== null && nearest !== undefined) moveToRow(nearest);
      }
    }
    const focusedKey = focusedRowIn(tree)?.dataset.rowKey;
    heldRow.current =
      focusedKey === undefined ? undefined : { index: order.indexOf(focusedKey), key: focusedKey };
  }, [editingRow, rows, stopEditing]);

  // A write from the line at the foot takes the reader to what they wrote: their newest
  // node under what the write answered - the reply, the comment, the question - its branch
  // unfolded, inside the viewport and focused, or, for an answer, the question's own row. The
  // send has landed when the line asks, but the read that draws it is still out, so the nodes
  // of theirs already under it are noted first and the one that is new is gone to once a read
  // draws it. A read that never draws one where it was looked for - a filter's view that holds
  // no such row - leaves the reader on their newest node there, or on the row they wrote under,
  // unless they have acted since the landing (`READER_ACTS`): the focus is then where they put
  // it, and the fallback only ends the landing. A landing the writer already holds as the tree
  // mounts was asked for while no outline was drawn - under a view drawn in its place, or before
  // a reload - so the reader has been elsewhere since, and it ends with the focus where it is.
  const landing = writer?.landing;
  const heldAtMount = useRef(landing);
  const landedFrom = useRef<
    { readonly before: ReadonlySet<string>; readonly landing: object } | undefined
  >(undefined);
  const latest = useRef({ reach, reader, root, writer });
  latest.current = { reach, reader, root, writer };
  useEffect(() => {
    const { writer: line } = latest.current;
    if (landing === undefined || line === undefined) return;
    if (landing === heldAtMount.current) {
      line.landed();
      return;
    }
    const parent = landingParent(root, landing.under);
    if (parent === undefined) {
      line.landed();
      return;
    }
    if (landing.to === "self") {
      line.landed();
      reach(parent.id);
      return;
    }
    const own = parent.children.filter((child) => readerWrote(child, reader));
    const held = landedFrom.current;
    if (held?.landing !== landing) {
      landedFrom.current = { before: new Set(own.map((child) => child.id)), landing };
      return;
    }
    const made = own.find((child) => !held.before.has(child.id));
    if (made === undefined) return;
    line.landed();
    reach(made.id);
  }, [landing, reach, reader, root]);
  useEffect(() => {
    if (landing === undefined || landing === heldAtMount.current) return;
    const acted = abortOnReaderAct();
    const late = window.setTimeout(() => {
      const now = latest.current;
      now.writer?.landed();
      if (acted.signal.aborted) return;
      const parent = landingParent(now.root, landing.under);
      if (parent === undefined) return;
      const newest = parent.children
        .filter((child) => readerWrote(child, now.reader))
        .reduce<TreeNode | undefined>(
          (last, child) =>
            last === undefined || (child.at ?? "") >= (last.at ?? "") ? child : last,
          undefined
        );
      now.reach((newest ?? parent).id);
    }, LANDING_WAIT_MS);
    return () => {
      window.clearTimeout(late);
      acted.abort();
    };
  }, [landing]);

  // The marks of each node of the tree, by reference, for each place in a row that names a node
  // by its address alone - an entry of a lens's chain.
  const nodeStates = useMemo(() => statesByReference(root), [root]);
  const outline = (
    <div className="mb-3 flex min-w-0 flex-col">
      {/* Over the tree and never in it: an alert inside the tree would break its structure of
          rows (`FailedReadLine`). */}
      {failedReads.length === 0 ? null : (
        <div className="mb-2 flex min-w-0 flex-col gap-2" ref={lines}>
          {failedReads.map((read) => (
            <FailedReadLine
              failure={read.failure}
              key={read.key}
              message={read.message}
              onRetry={read.retry}
              retrying={read.retrying}
              speaks={read.speaks}
            />
          ))}
        </div>
      )}
      {breadcrumb.length === 0 ? null : (
        <p
          className={`flex min-w-0 flex-wrap items-baseline gap-1 text-xs ${textMutedOnCanvas}`}
          ref={crumbLine}
        >
          {breadcrumb.map((above) => (
            <button
              // A crumb is a word in a line of text, not a control box. Below 1280 px every
              // button takes the 44 px floor of `styles.css`, which is in no layer and exempts a
              // button only inside the view row, the header and a row (`[data-filter-row]`,
              // `[data-issue-header]`, `[data-node-row]`), none of which holds the crumb line:
              // `min-h-0!` and `min-w-0!` take the floor off, since a layered utility wins over
              // an unlayered rule only with `!`. A 44 px crumb was a third of the head the first
              // open question needs on a phone. Its box is 28 px tall instead below 1280 px, its
              // 16 px line and the 6 px of padding above and below that `dispatch-inline-link`
              // gives it there, and 24 px from 1280 px up, where that class gives none and `py-1`
              // gives 4 px: WCAG 2.2's 24 px (2.5.8) at every width. `py-1` is a layered utility,
              // so below 1280 px the unlayered 6 px still wins. A crumb is pressed to zoom out, so
              // it is a target whatever its tab index, and axe's `target-size` rule leaves a
              // target outside the tab order for review rather than failing it: with no padding
              // of its own, at 1440 by 900 px each crumb was its 16 px line alone, 34 by 16 and
              // 125 by 16 px on a zoom two levels deep, which axe left for review with a safe
              // space 16 px across. Measured in Chromium on that zoom with `py-1`: 34 by 24 and
              // 125 by 24 px at 1440 by 900 px, and 34 by 28 and 125 by 28 px at 390 by 664 px,
              // axe passing every crumb at both. Measured at 390 by 664 and 1024 by 768 px, on
              // crumbs of one line of words each: each crumb 28 px tall (a five-character key's
              // 34 px wide) and the line 28 px, one crumb tall, where the floor made each crumb
              // at least 44 by 44 px and the line 44 px; on a copy of a long real issue at 390 by
              // 664 px, the line one zoom draws is 28 px where it was 44, and the one a second
              // zoom draws 96 px where it was 112. Its display is left to the floor's
              // `inline-flex`: the line is a flex box, which makes each crumb a block box
              // whatever its display, and `inline-block`, `inline` and `block` drew the same box.
              // It draws one line for a node, as every place that names a node by one does
              // (`CrumbName`), and it wraps rather than being cut again mid-word by its box; the
              // issue is named by its key, since the header above draws its title, and a node
              // with no words by its kind's word, never its id, which is no name a reader reads.
              className="dispatch-inline-link min-h-0! min-w-0! max-w-full py-1 text-left underline [overflow-wrap:anywhere]"
              data-crumb={above.id}
              key={above.id}
              // Before it zooms out, the press unfolds the path down to the row the reader is on,
              // by that row's own path, since a node drawn under two parents has a row under
              // each. Without it the zoom out drew that row nowhere where the reader had folded a
              // branch above it: the node the zoom was rooted at is drawn open as the zoom's own
              // row, and the zoom out draws it and each node between it and the crumb's as the
              // reader left them, and the focus went to the row drawn at the reply's place
              // (`heldRow`). Where no row holds the focus, the press only zooms.
              onClick={() => {
                const key = focusedRowIn(body.current)?.dataset.rowKey;
                const trail = rows.find((row) => rowKeyOf(row) === key)?.trail;
                if (trail !== undefined) unfoldPath(trail);
                zoomTo(above.id);
              }}
              // A press leaves the focus where it is, as one on a row's fold control does: the
              // zoom it makes takes the crumb off the page, since the line drawn for that zoom
              // holds no crumb of the node pressed, or no line where it is the issue, and a focus
              // the crumb took went with it to the page's body. The row the reader is on keeps
              // the focus, since the press's unfold keeps it drawn (`onClick`), and the zoom
              // places it as it places the row a key's zoom leaves the focus on (`zoomTo`).
              onMouseDown={(event) => event.preventDefault()}
              tabIndex={-1}
              type="button"
              {...authorText}
            >
              {above.id === root.id ? above.id : <CrumbName node={above} />}
            </button>
          ))}
          <span {...pageChrome}>/</span>
        </p>
      )}
      <div
        aria-label={CONTROL_LABELS.tree}
        className="flex min-w-0 flex-col"
        onBlur={releaseFocus}
        // A press in the tree, whose focus the row takes once the press is over (`press`).
        onMouseDown={press.onMouseDown}
        ref={body}
        role="tree"
      >
        {/* A decision's Change and the words typed in it outlive its row, which a read draws
            again under a new key: the drafts are the tree's. */}
        <ChangeDraftsProvider>
          <NodeStatesProvider value={nodeStates}>
            <SettleIssueProvider value={root.type === "issue" ? root.id : undefined}>
              <TreeRows
                artifacts={artifacts}
                context={context}
                coverageOut={coverageOut}
                editing={editing}
                focused={focused}
                foldedMore={root.moreToRead === true}
                isClosed={isClosed}
                {...(more === undefined ? {} : { more: { left: more.left, onPress: readMore } })}
                onFocus={(row, node) => press.focus({ node, row })}
                {...(olderAbove === undefined ? {} : { onOlder: () => readOlder(false) })}
                onPage={(page) => drawPage(page, false)}
                onToggle={(row, expanded) => unfold(row, expanded !== true)}
                {...(report === undefined
                  ? {}
                  : { report: { line: report.line, row: report.row ?? tabStop } })}
                rows={rows}
                tabStop={tabStop}
                threads={threads}
                // The row the reader is editing in places nothing of its own: the editor keeps its
                // caret on screen as they write, and grows the row, and the row put back at its top
                // at each change of its size took the caret off the screen - a new line written deep
                // in the spec moved the page from 3120 px to 89 px.
                unplaced={editing?.row ?? unplaced}
                zoomRoot={zoomRoot}
              />
            </SettleIssueProvider>
          </NodeStatesProvider>
        </ChangeDraftsProvider>
      </div>
      {root.children.length === 0 && readsHeld && emptyLine !== undefined ? (
        <p className={`py-1 text-sm ${textMutedOnCanvas}`} {...pageChrome}>
          {emptyLine}
        </p>
      ) : null}
    </div>
  );
  return <PublishedReferenceNames published={published}>{outline}</PublishedReferenceNames>;
}

/** How long a write's landing waits for the read that draws it before it takes the reader to
 *  their newest node where it looked, in milliseconds: the line's send has landed and its read
 *  is a request away, so a read that has not drawn the node by then is not going to. */
export const LANDING_WAIT_MS = 10_000;

/** No place has been let go from the address path's hold (`released`). */
const NO_FOLDS: ReadonlySet<string> = new Set();

/** The keys of the node rows, in drawing order. */
function nodeRowKeys(rows: readonly DrawnRow[]): string[] {
  return rows.flatMap((row) => (row.kind === "node" ? [rowKeyOf(row)] : []));
}

/** What a crumb above a zoom says of a node other than the issue, which is named by its key:
 *  its one line, as every place that names a node by one draws it (`useNodeLine`) - each
 *  reference in it read as its target's current name (R8) - and its kind's word where its words
 *  hold none (`lineOrKindWord`), with the marks of a post its author corrected, since a crumb
 *  draws a node's one line where its own row is not drawn, as a filter's parent line does
 *  (`node-line.tsx`'s `ParentLink`, deleted-post R2 and R3). A deleted comment or message is drawn
 *  as its deleted line, from the delete (`DeletedLine`) and never from the node's text, which
 *  names the actor as no registry knows it and the time as stored; an edited one says
 *  ` · edited` after its line (`EditedMark`). */
function CrumbName({ node }: { node: TreeNode }): ReactNode {
  const { line, reads } = useNodeLine(node, { surface: "outline-crumb" });
  const deleted = postDeletion({ deleted_at: node.deletedAt, deleted_by: node.deletedBy });
  if (deleted !== null) return <DeletedLine deleted={deleted} />;
  return (
    <>
      {reads}
      {lineOrKindWord(line, node.type)}
      {line === undefined ? null : <EditedMark at={node.editedAt} marks={pageChrome} />}
    </>
  );
}

/**
 * Holds `row`, which holds the focus on itself or on a control inside it (a chip `f` stopped
 * on), on screen while the page grows: each time the page's size changes - a read drawing
 * rows above it, a document mounting beside the tree - the row is put back on screen by the
 * page's one rule (`bringOnScreen`). Nothing else would: `NodeRow.tsx`'s `useRowOnScreen` watches
 * the row's own size, which rows drawn above it leave as it was, and the browser's scroll
 * anchoring holds still what is at the top of the viewport, which on the first screen is the
 * header and not the row. Measured with the comments' read answering after the question's, the
 * question opened on at 496 px down a 900 px screen ended 2854 px down, at the page's top. A
 * reader's own act (`READER_ACTS`) ends the hold, and so does the focus having left the row.
 * Answers the hold, which aborting ends sooner.
 *
 * It is exported for the page's views, which are to draw trees of their own and hold the row
 * each takes the reader to as the outline does.
 */
export function holdOnScreen(row: HTMLElement): AbortController {
  const hold = abortOnReaderAct();
  if (typeof ResizeObserver === "undefined") return hold;
  const grown = new ResizeObserver(() => {
    if (row.contains(document.activeElement)) bringOnScreen(row);
    else hold.abort();
  });
  grown.observe(document.body);
  hold.signal.addEventListener("abort", () => grown.disconnect());
  return hold;
}

/**
 * Lands the reader on `row`, a row the page takes them to of its own accord, in the next frame:
 * focuses it without scrolling, brings it on screen by the page's one rule (`bringOnScreen`), and
 * holds it there while the rest of the page arrives (`holdOnScreen`), in place of the row `held`
 * held. The focus waits for the frame because a focus given in the commit that draws the row
 * makes the browser lay the whole new page out there and then, inside the reader's press: on a
 * large issue, 831 ms of a 3.9 s press that changed the view. In the frame, the one read of the
 * row's box meets the layout the browser makes for that frame anyway.
 *
 * Until the frame the landing is what `held` holds, so a later landing, the hold of another row
 * or the tree leaving the page ends it, and so does the reader's own act (`READER_ACTS`): the
 * focus is then where they put it. Answers the landing, which the frame ends as it comes.
 * `landed` is called once the row holds the focus the frame gave it, and only then.
 *
 * It is exported for the Plan view's chart, which is to land on the row an address names as the
 * outline does, and for the Spec view's tree, which does the same.
 */
export function landOnRow(
  row: HTMLElement,
  held: RefObject<AbortController | undefined>,
  landed?: () => void
): AbortController {
  held.current?.abort();
  const landing = abortOnReaderAct();
  held.current = landing;
  // The focus as the landing finds it. Moved before the frame to a control that does not hold
  // the row - a script's focus, which is no act of the reader's - it stays there, as it would
  // have had it moved just after the landing.
  const from = document.activeElement;
  const frame = requestAnimationFrame(() => {
    landing.abort();
    if (!row.isConnected) return;
    const now = document.activeElement;
    if (now !== from && !focusOnDocument() && now?.contains(row) !== true) return;
    row.focus({ preventScroll: true });
    if (row.contains(document.activeElement)) landed?.();
    bringOnScreen(row);
    holdInPlaceOf(row, held);
  });
  landing.signal.addEventListener("abort", () => cancelAnimationFrame(frame));
  return landing;
}

/** Holds `row` on screen (`holdOnScreen`) and ends the hold of the row `held` held before, so
 *  one row is held at a time. */
function holdInPlaceOf(row: HTMLElement, held: RefObject<AbortController | undefined>): void {
  held.current?.abort();
  held.current = holdOnScreen(row);
}

/** The row of `tree` that holds the focus, on the row itself or on a control inside it; null
 *  where the focus is on no row of it. */
function focusedRowIn(tree: HTMLElement | null): HTMLElement | null {
  const row = document.activeElement?.closest<HTMLElement>("[data-node-row]") ?? null;
  return row !== null && tree?.contains(row) === true ? row : null;
}

/** Whether the first line of `row` - the line its bullet stands on - is whole on screen: below
 *  the band that stays at the top and above the box at the foot, by the heights the page
 *  publishes for them, as `bringOnScreen` reads them. */
function firstLineOnScreen(row: HTMLElement): boolean {
  const line = (
    row.querySelector<HTMLElement>("[data-node-bullet]") ?? row
  ).getBoundingClientRect();
  const published = getComputedStyle(document.documentElement);
  const band = Number.parseFloat(published.getPropertyValue(IDENTITY_HEIGHT_PROPERTY)) || 0;
  const foot = Number.parseFloat(published.getPropertyValue(FOOT_HEIGHT_PROPERTY)) || 0;
  return line.top >= band && line.bottom <= window.innerHeight - foot;
}

/**
 * Whether the focus is the page opening's to place (`IssueTree`'s `opening`): while nothing
 * holds it, or while `focused`, the row the opening last focused, holds it. Nothing holds it on
 * the page itself, or on an element that holds `tree`: the app focuses its main element at each
 * page change but the first (`app.tsx`), so an issue opened by a link of the dashboard starts
 * with the focus there, and read as somebody's focus that ended the opening with the focus on no
 * row (`focusIsLoose`). A focus nobody holds is the opening's again where the row it focused has
 * left the page: a later read drew its node under another key - a node sits under the issue
 * until its parent's read answers, then under that parent - and the browser drops the focus of
 * an element taken out of the page. Read as somebody else's focus, that ended the opening, and
 * the page opened with the focus on no row. A row still on the page that lost the focus lost it
 * to something else, a script's blur among them, which ends the opening.
 */
function openingMayFocus(tree: HTMLElement | null, focused: HTMLElement | undefined): boolean {
  return focusIsLoose(tree)
    ? focused?.isConnected !== true
    : focused?.contains(document.activeElement) === true;
}

/** Whether the reader wrote `node`: a person signed in as `reader`, in any casing, as the
 *  server matches a login (`isViewer`), whether they wrote it themselves or a session acting
 *  for them did - a write from the line at the foot is theirs either way. */
function readerWrote(node: TreeNode, reader: string | undefined): boolean {
  return node.author !== undefined && isViewer(node.author, reader);
}

/** The node a write was made under, by the item the line wrote to; the tree's own root for a
 *  write on the issue itself; undefined where the view the tree draws holds no such node. */
function landingParent(
  root: TreeNode,
  under: { readonly id: string; readonly kind: string } | undefined
): TreeNode | undefined {
  return under === undefined ? root : pathToTarget(root, { id: under.id, kind: "item" })?.at(-1);
}

/** The facts of `node` the panel draws while its row has the focus: who wrote it, the item it
 *  is, its own address and its state, and its own words, which the panel names it by as every
 *  place that names a node by its one line does (`FocusedNode.text`). Its state is the standing
 *  word: a decision reads "Kept by", or "Changed by" where the person whose Change it waits on
 *  is known, and `stateBy` is that person or its keeper, whom the panel names after the word
 *  (`standingParts`, R2, R3). */
export function focusedNodeOf(node: TreeNode): FocusedNode {
  const item = conversationItem(node);
  const standing = node.state === undefined ? undefined : standingParts(node.state, node);
  return {
    ...(node.author === undefined ? {} : { author: node.author }),
    id: node.id,
    ...(item === undefined ? {} : { item }),
    ...marksOf(node),
    ...(node.reference === undefined ? {} : { reference: node.reference }),
    ...(node.source === undefined ? {} : { source: node.source }),
    ...(standing === undefined ? {} : { state: standing.word }),
    ...(standing?.person === undefined ? {} : { stateBy: standing.person }),
    text: node.text,
    ...(node.tree === undefined ? {} : { tree: node.tree }),
    type: node.type,
  };
}

/**
 * The handler for a tree's `onBlur`, by the outline's rule for the focus leaving it.
 *
 * The focus leaving the tree with no press under way - by Tab, by a script's blur, by the tree's
 * own Escape - lets go of the row it left at once (`onLeave`): its focused look, what a key
 * reported at it and its own placing on screen go together, since a report lasts until the next
 * key, which Tab is.
 *
 * The focus leaving it for a press on something else of the page lets go of the row one task
 * after that press ends, once its click has landed. A press focuses what it lands on as it goes
 * down, before the press ends: the body closing then shortened the page under the pointer, a
 * control below the row rose by the body's height, and the click that ends the press landed on
 * something else or on nothing, so the control never fired. A press that leaves the focus on no
 * control is such a leave too: Safari focuses no button a press lands on. The press ends as the
 * tree's own press does (`press-focus.ts`'s `trackPress`). Escape anywhere on the page lets go
 * of the row at once while a press keeps it, and the focus back in the tree by the time the
 * press has ended keeps it.
 *
 * A row the reader is editing in is not let go of (`editing`): the document's editor moves into
 * it, which drops the focus for a frame as it leaves the page, and that is no reader leaving the
 * tree. The editing ending with the focus out of the tree all the same - the reader moved on by
 * Tab, or by a press once it is over (`useNodeEditing.ts`) - is them gone, and lets go of the row
 * then, as a blur to where the focus is would have.
 *
 * The panel goes back to the issue's own facts whenever the focus leaves the tree, except for
 * Details, or the control that opens it: Details is where a reader reads the node's facts, and
 * the next row focused replaces them.
 */
export function useReleaseFocus({
  editing,
  onLeave,
}: {
  /** Whether a row of the tree is being edited. */
  readonly editing: boolean;
  /** Lets go of the row the focus left - its look, what a key reported at it: as the
   *  focus leaves with no press under way, and once a press that took it elsewhere has ended. */
  readonly onLeave: () => void;
}): (event: { currentTarget: HTMLElement; relatedTarget: EventTarget | null }) => void {
  const setFocusedNode = useSetFocusedNode();
  // The tree the focus left while a press was under way, whose row that keeps until the task
  // after the press ends, and the latest `onLeave`, which that task calls, or an Escape anywhere
  // on the page before it. The Escape is read in the capture phase, so a handler that answers
  // the key and stops it - a menu closing, a dialog closing - still lets the row go.
  const left = useRef<HTMLElement | null>(null);
  const leave = useRef(onLeave);
  leave.current = onLeave;
  // The tree a blur left while a row of it was being edited, which let go of nothing then: the
  // editing ending with the focus out of that tree lets go of its row (`letGo`).
  const leftEditing = useRef<HTMLElement | null>(null);
  // Whether a press is under way, from its mousedown - whose default is what moves the focus,
  // for a mouse and for a tap's mouse events alike - until the task after it ends
  // (`trackPress`). The blur it causes reads it to tell a press from Tab or a script's blur.
  const press = useRef<Press | undefined>(undefined);
  useEffect(() => {
    // Lets go of the row a press kept, unless the focus has come back to its tree, whose row
    // that took it is the focused one now.
    const release = () => {
      const tree = left.current;
      if (tree === null) return;
      left.current = null;
      if (!tree.contains(document.activeElement)) leave.current();
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") release();
    };
    const listening = new AbortController();
    window.addEventListener("keydown", closeOnEscape, { capture: true, signal: listening.signal });
    press.current = trackPress({ onEnd: release, signal: listening.signal, starts: ["mousedown"] });
    return () => listening.abort();
  }, []);
  // Lets go of the row of `tree`, which the focus left for `to`, by the rule above.
  const letGo = useCallback(
    (tree: HTMLElement, to: EventTarget | null) => {
      if (to instanceof Node && tree.contains(to)) return;
      if (press.current?.pressing !== true) {
        left.current = null;
        onLeave();
      } else {
        left.current = tree;
      }
      if (to instanceof Element && to.closest(`[${DETAILS_ATTRIBUTE}]`) !== null) return;
      setFocusedNode(undefined);
    },
    [onLeave, setFocusedNode]
  );
  useEffect(() => {
    const tree = leftEditing.current;
    if (editing || tree === null) return;
    leftEditing.current = null;
    letGo(tree, document.activeElement);
  }, [editing, letGo]);
  return useCallback(
    (event: { currentTarget: HTMLElement; relatedTarget: EventTarget | null }) => {
      if (editing) {
        leftEditing.current = event.currentTarget;
        return;
      }
      letGo(event.currentTarget, event.relatedTarget);
    },
    [editing, letGo]
  );
}

/** One ask the Contents list makes of the outline (`outline-request.tsx`): the node to go to,
 *  where there is one, and whether Focus branch is on as it asks. `seq` is one past the last
 *  ask's, and an outline acts on each `seq` once (`useTakeOutlineRequest`). */
export interface OutlineRequest {
  readonly focusBranch: boolean;
  readonly id: string | undefined;
  readonly seq: number;
}
