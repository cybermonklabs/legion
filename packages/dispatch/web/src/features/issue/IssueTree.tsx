import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { whoAmIQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import { textMutedOnCanvas } from "../../theme/classes";
import { DETAILS_ATTRIBUTE } from "../margin/margin-slot";
import { useMediaQuery } from "../shell/useDialog";
import { useSetFocusedNode } from "./focused-node";
import { bringOnScreen } from "./identity-block";
import { pathToTarget } from "./issue-address";
import { drawnLine, firstLineOf, nodeWords, type TreeNode } from "./issue-tree";
import { threadMarks } from "./outline-thread-read";
import { conversationItem, useOutlineWriter } from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS, nodeStateWord } from "./page-words";
import { CYCLE_STATES, EVERYTHING, TreeRows } from "./TreeRows";
import {
  DRAWN_LEVELS,
  NARROW_LEVELS,
  NOTHING_OPENED,
  type OpenedBranches,
  pathTo,
  treeRows,
  zoomBreadcrumb,
} from "./tree-fold";
import { everythingOpened, nodesInOrder, rowKeyOf } from "./tree-keys";
import { useNodeEditing } from "./useNodeEditing";
import { useTreeKeymap } from "./useTreeKeymap";

/**
 * The issue's body: one folded tree of its nodes (dispatch://INFRA-1196 R1, R2, R3, R6,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t14`).
 *
 * The model is `issue-tree.ts`'s, the view of it the page or a filter draws is `presets.ts`'s
 * and the fold is `tree-fold.ts`'s, all three pure; this holds what only a reader's page can
 * hold - what they opened, what they pinned, and where the focus is. The rows themselves are
 * drawn by `TreeRows.tsx` and the caret in a node's own text is `useNodeEditing.ts`'s.
 *
 * The tree is the one body the page and every filter draw: the page hands it the tree its view
 * left. The issue is never a row: the header is the issue, so the outline's first row is the
 * first node under it (the architect's ruling 5). A filter draws its own kinds as rows and no
 * other (R2), each drawn whole at every width, its parent a line on it where that parent is no
 * row. A row drawn to place the filter's own - a requirement under Verification - is context,
 * faint and marked, and reached by the arrow keys like any other row.
 *
 * The tree answers the WAI-ARIA tree pattern's roles and properties and keeps one tab stop: the
 * focused row is the only row that takes the focus. The keys that move it are
 * `dispatch://INFRA-1196/artifact/plan-md#b-t15`'s, so a reader uses the pointer until they land.
 * A key that has nowhere to go says so at the row it was pressed on, until the reader's next
 * key or move.
 *
 * The page opens with the focus on the first open question that waits on the reader, its path
 * unfolded and its row in the viewport, and on the outline's first row where none does (R7). A
 * write from the line at the foot takes the reader to what they wrote once its read has drawn
 * it, its branch unfolded (R16, `outline-writer.tsx`'s `Landing`).
 *
 * Below 1024 px the page with no filter on is read one node at a time (R16): the path down to
 * the node the reader is on is a breadcrumb, and the outline draws that node and one level
 * below it, and every path the frontier holds open whatever its depth; a branch's fold control
 * there zooms into it. From 1024 px up the outline draws three levels below the node it is
 * rooted at.
 */
export function IssueTree({
  artifacts,
  context,
  emptyLine,
  filtering,
  focusReveal,
  holdsEscape,
  isClosed,
  onEditing,
  openBody,
  request,
  reveal,
  root,
  rootRow,
}: {
  /** Each artifact of the issue by its slug, which is how a node names the document it is or
   *  belongs to (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The ids of the rows the view draws as context, faint and marked. */
  context: ReadonlySet<string>;
  /** What the page says where the preset's query matched nothing under the issue. */
  emptyLine: string;
  /** Whether the preset or its query filters the tree (`presets.ts`'s `PresetView.filtering`):
   *  while one does, every branch of the view it left is drawn open, and while nothing filters
   *  the fold is the frontier's alone. */
  filtering: boolean;
  /** Whether the node the address names is the tree's to put the focus on. It is always when a
   *  link or a chip the reader followed on this page named it (`IssueBody.tsx`, R10). On a
   *  fresh load it is not when the preset's own document is open on that node - a comment
   *  anchored in it, an ask indexed from one of its blocks, a block named by a fragment -
   *  because the document lands on it there, and two surfaces scrolling for the same node would
   *  each undo the other. */
  focusReveal: boolean;
  /** Whether the page takes Escape from a row before the outline is left: a filter by words to
   *  clear or a filter to turn off comes first in the key table's order (`IssueBody.tsx`). */
  holdsEscape: boolean;
  /** Whether the issue is closed, which the writes a node's body offers are refused on. */
  isClosed: boolean;
  /** The document the reader is editing a node of here, so the page draws that document's one
   *  editor rather than leaving the row with nothing to put a caret in; `undefined` whenever
   *  the reader is editing nothing. */
  onEditing: (artifactId: string | undefined) => void;
  /** The node this preset draws the body of without being asked: what waits for the reader
   *  there, such as a decision waiting for their review (R7). It opens that body and takes no
   *  focus, which stays the reader's. */
  openBody?: string;
  /** What the Contents pane last asked of the outline (`IssueContents.tsx`): go to a node, and
   *  draw that node's branch alone while the pane's Focus branch is on, or the whole outline
   *  again once it is turned off (R14). Each ask is acted on once. */
  request?: OutlineRequest;
  /** The chain down to the node the address names, the issue first: the tree unfolds it and
   *  puts the focus on its last node, once per address (R19). */
  reveal: readonly TreeNode[] | undefined;
  /** The tree as the view left it. */
  root: TreeNode;
  /** Whether no filter is on (`presets.ts`'s `PresetView.rootRow`): the conversation, which is
   *  read one node at a time below 1024 px (R16) and which the page opens on what waits on the
   *  reader (R7). A filter's rows are drawn whole at every width. */
  rootRow: boolean;
}): ReactNode {
  const [opened, setOpened] = useState<OpenedBranches>(NOTHING_OPENED);
  const [focused, setFocused] = useState<string | undefined>(undefined);
  // Where the tree's one tab stop rests, which outlives the focus leaving it: Shift+Tab comes
  // back to the row the reader left, not to the first row (R9).
  const [stop, setStop] = useState<string | undefined>(undefined);
  const [pinned, setPinned] = useState<ReadonlySet<string>>(() => new Set());
  const [zoom, setZoom] = useState<string | undefined>(undefined);
  // How much of the whole tree the cycle key draws: its first lines, their bodies down to the
  // second level, then everything.
  const [cycle, setCycle] = useState(0);
  // The node a key is taking the reader to, once the row it asked for is on the page.
  const [wanted, setWanted] = useState<string | undefined>(undefined);
  // What a key reported when it had nowhere to go, and the row it was pressed on: the row draws
  // it, so the reader reads it where they are, and the next key, move or leaving the tree lets
  // it go. `row` is undefined where the key was pressed with no row focused.
  const [report, setReport] = useState<{ readonly line: string; readonly row?: string }>();
  // A filter's rows are drawn whole at every width: they are its own kinds alone, a level or two
  // deep, and the one-node-at-a-time reading is the conversation's.
  const zoomed = useMediaQuery(ZOOMED_TREE_QUERY) && rootRow;
  const setFocusedNode = useSetFocusedNode();
  const writer = useOutlineWriter();
  // The reader, whose own new words a write from the line takes them to.
  const reader = useQuery(whoAmIQuery()).data?.login;
  const body = useRef<HTMLDivElement | null>(null);
  const reportHere = useCallback((line: string) => {
    if (line === "") {
      setReport(undefined);
      return;
    }
    const row = document.activeElement?.closest<HTMLElement>("[data-node-row]");
    const inTree = row != null && body.current?.contains(row) === true;
    setReport({
      line,
      ...(inTree && row.dataset.rowKey !== undefined ? { row: row.dataset.rowKey } : {}),
    });
  }, []);
  const { edit, editing, stopEditing } = useNodeEditing({
    artifacts,
    container: body,
    onEditing,
    onReport: reportHere,
  });

  const target = reveal?.at(-1);
  // Where the zoomed tree is rooted. Nothing chosen opens it on the issue itself, so a reader
  // on a phone sees the whole issue and can reach any branch of it; an address that names a
  // node opens it on that node's parent, so the node the address named is one of the rows.
  const on = zoom ?? (target === undefined ? root.id : (reveal?.at(-2) ?? root).id);
  // A reader who zoomed into a node reads the tree from there: the node is the tree's root,
  // the path above it the breadcrumb, and everything below it folded as it was. Below 1024 px
  // the tree is always rooted somewhere, the issue itself until the reader zooms (`on`).
  const rootedAt = zoomed ? on : zoom;
  const rootedPath = (rootedAt === undefined || rootedAt === root.id
    ? undefined
    : pathTo(root, rootedAt)) ?? [root];
  const rooted = rootedPath.at(-1) ?? root;
  // What a filter opens: every branch of the view it left, since the view holds only its
  // matches and the chain above each, so each branch is the path down to a match and the view
  // draws what it matched rather than its context rows alone. It opens them at every depth, as
  // the address's own path is, so a match below the drawn levels is a row of its own rather
  // than a count (`tree-fold.ts`'s `DRAWN_LEVELS`). The reader's own hand outranks it, so a
  // branch they folded shut stays shut.
  const filtered: OpenedBranches = filtering
    ? { ...everythingOpened(rooted), closed: opened.closed }
    : opened;
  // The address's own node is unfolded to whatever the reader has opened by hand, at every
  // depth: a `#b-` fragment names a block of a document the page opens folded. A branch of
  // that path the reader folded shut is opened again, since the row the address names is
  // drawn nowhere while anything above it is closed - which is the one thing the filter's own
  // opens do not do.
  const path = reveal ?? [];
  const shown: OpenedBranches =
    path.length === 0
      ? filtered
      : {
          branches: new Set([...filtered.branches, ...path.map((above) => above.id)]),
          closed: new Set(
            [...filtered.closed].filter((id) => !path.some((above) => above.id === id))
          ),
          deeper: new Set([...filtered.deeper, ...path.map((above) => above.id)]),
        };
  // The fold is seeded with the path the breadcrumb holds, so a row is keyed by its path from
  // the issue whether or not the reader has zoomed: a body they pinned, the row they are
  // editing and the tree's one tab stop are held by that key, and a zoom either way leaves
  // them where they are (`rowKeyOf`).
  const rootedAbove = rootedPath.slice(0, -1).map((one) => one.id);
  const drawn = treeRows(
    rooted,
    cycle === EVERYTHING ? everythingOpened(rooted) : shown,
    rootedAbove,
    zoomed ? NARROW_LEVELS : DRAWN_LEVELS
  );
  // The issue is no row: the header is the issue (the architect's ruling 5), so the nodes under
  // it are the tree's top level, with no filter on as under one (R2). A zoom into a node keeps
  // that node's own row, which is the node the reader reads from there.
  const head = drawn[0];
  const rows =
    head?.kind === "node" && head.node.id === root.id
      ? drawn.slice(1).map((row) => ({ ...row, level: row.level - 1 }))
      : drawn;
  const breadcrumb: readonly TreeNode[] =
    rootedAt === undefined ? [] : zoomBreadcrumb(root, rootedAt);
  // Where each thread of the issue is marked read in the reader's Inbox: its newest write's row.
  const threads = useMemo(() => threadMarks(root), [root]);

  const takeFocus = useCallback(
    (row: string, node: TreeNode) => {
      setFocused(row);
      setStop(row);
      // A move to a row is the reader going on from what a key reported.
      setReport(undefined);
      const item = conversationItem(node);
      setFocusedNode({
        ...(node.author === undefined ? {} : { author: node.author }),
        id: node.id,
        ...(item === undefined ? {} : { item }),
        ...(node.reference === undefined ? {} : { reference: node.reference }),
        ...(node.state === undefined ? {} : { state: nodeStateWord(node.state) }),
        title: node.text === "" ? node.id : firstLineOf(nodeWords(node)).line,
        type: node.type,
      });
    },
    [setFocusedNode]
  );
  // The focus leaving the tree closes the body it opened: nothing is open while the reader is
  // somewhere else, and the panel goes back to the issue's own facts (R2, R12). A row the
  // reader is editing in keeps its body: the document's editor moves into it, which drops the
  // focus for a frame as it leaves the page, and that is no reader leaving the tree. The focus
  // moving to Details, or to the control that opens it, keeps the node's facts in the panel,
  // since Details is where a reader reads them (R14); the next row focused replaces them.
  const releaseFocus = useCallback(
    (event: { currentTarget: HTMLElement; relatedTarget: EventTarget | null }) => {
      const to = event.relatedTarget;
      if (editing !== undefined) return;
      if (to instanceof Node && event.currentTarget.contains(to)) return;
      setFocused(undefined);
      setReport(undefined);
      if (to instanceof Element && to.closest(`[${DETAILS_ATTRIBUTE}]`) !== null) return;
      setFocusedNode(undefined);
    },
    [editing, setFocusedNode]
  );
  // What the twisty and the arrow keys do to one branch. Folding a branch is held as its own
  // set and not as the absence of an opened one: the fold draws the reasoning and the path to
  // an open question of its own accord, so a reader folding one of those shut has nothing to
  // delete and their Fold would do nothing (`tree-fold.ts`'s `closed`).
  const unfold = useCallback((id: string, open: boolean) => {
    setOpened((held) => {
      const branches = new Set(held.branches);
      const closed = new Set(held.closed);
      if (open) {
        branches.add(id);
        closed.delete(id);
      } else {
        branches.delete(id);
        closed.add(id);
      }
      return { branches, closed, deeper: held.deeper };
    });
  }, []);
  const openDeeper = useCallback((id: string) => {
    setOpened((held) => ({
      branches: held.branches,
      closed: held.closed,
      deeper: new Set(held.deeper).add(id),
    }));
  }, []);
  const pin = useCallback((row: string) => {
    setPinned((held) => {
      const next = new Set(held);
      if (!next.delete(row)) next.add(row);
      return next;
    });
  }, []);

  // Takes the reader to one node, wherever it is: the path down to it is unfolded, a zoom it
  // sits outside of is left, and the row takes the focus once this render has drawn it.
  const goToNode = useCallback(
    (id: string) => {
      const above = (pathTo(root, id) ?? []).slice(0, -1).map((node) => node.id);
      setOpened((held) => ({
        branches: new Set([...held.branches, ...above]),
        closed: new Set([...held.closed].filter((one) => !above.includes(one))),
        deeper: new Set([...held.deeper, ...above]),
      }));
      // Where the tree is read one node at a time, the only rows are the zoomed node and what
      // it holds (`zoomedRows`), so a key that takes the reader to a node zooms to that node's
      // parent: the node itself is then one of the rows, whatever the zoom was and however
      // deep it sits. Leaving the zoom where it stood drew the second open question nowhere,
      // and the key reported nothing because the row it asked for was never on the page.
      // A zoom that already names the target is left alone: that is `.`, which zooms into the
      // focused node and then takes the reader to it (`useTreeKeymap.ts`'s `tree-zoom-in`),
      // and zooming to the parent there undid the zoom the key had just made.
      // Zooming out of a node the target sits outside of holds the issue's own id rather than
      // nothing chosen: nothing chosen falls back to where the tree opened (`on`, above), not
      // to the whole issue.
      setZoom((held) =>
        zoomed
          ? held === id
            ? held
            : (above.at(-1) ?? root.id)
          : held === undefined || held === id || above.includes(held)
            ? held
            : root.id
      );
      setWanted(id);
    },
    [root, zoomed]
  );
  /** The row of `id` where it is drawn already, focused where it stands; else `goToNode`, which
   *  unfolds the path to it and, where the tree is read one node at a time, zooms to it. */
  const reach = useCallback(
    (id: string) => {
      const row = body.current?.querySelector<HTMLElement>(
        `[data-node-row][data-node-id="${CSS.escape(id)}"]`
      );
      if (row === null || row === undefined) goToNode(id);
      else row.focus();
    },
    [goToNode]
  );

  useEffect(() => {
    if (wanted === undefined) return;
    setWanted(undefined);
    body.current
      ?.querySelector<HTMLElement>(`[data-node-row][data-node-id="${CSS.escape(wanted)}"]`)
      ?.focus();
  }, [wanted]);

  // The Contents pane's asks, each acted on once. With Focus branch on, the outline is zoomed to
  // the node, which is then the tree's first row and takes the focus; turned off, the zoom is
  // left and the reader is taken to the node in the whole outline, as any other move takes them.
  // What was acted on is held by the ask itself (`HANDLED`), which the page keeps across this
  // tree being drawn again - a filter or a document drawn in its place and closed - so an ask
  // made for the tree as it stood before does not move the reader again when it comes back.
  useEffect(() => {
    if (request === undefined || HANDLED.has(request)) return;
    HANDLED.add(request);
    if (request.focusBranch && request.id !== undefined) {
      setZoom(request.id);
      setWanted(request.id);
      return;
    }
    setZoom(undefined);
    if (request.id !== undefined) goToNode(request.id);
  }, [goToNode, request]);

  useTreeKeymap({
    container: body,
    holdsEscape,
    onCycle: () => setCycle((held) => (held + 1) % CYCLE_STATES),
    onEdit: edit,
    onEditEnd: stopEditing,
    onFold: unfold,
    onOpenDeeper: openDeeper,
    onPin: pin,
    onReport: reportHere,
    onReveal: goToNode,
    onZoom: setZoom,
    oneNodeAtATime: zoomed,
    rows,
    root,
    writer,
    zoom: zoomed ? on : zoom,
  });

  // An address that names a node puts the focus on it (R19). The row is not on the page until
  // the read that holds it has answered, and the document the preset draws beside the tree
  // mounts later still and can leave the focus nowhere as it does, so this looks again on
  // every tree the page draws and takes the focus whenever nothing else holds it. Once the
  // reader - or anything else on the page - has the focus, it is theirs: the effect leaves it
  // alone until another address names another node.
  const taken = useRef<string | undefined>(undefined);
  // A row the page focuses itself - the node an address names (R19, R20) and the open question
  // the page opens on (R7) - takes the focus while the rows around it are still arriving: the
  // outline's other reads, the names of the one batched reference read, a six-line cut measured
  // again once the typeface lands. Each one above the row pushes it down the page, out of sight
  // (measured: a question focused at 345 px with eight rows drawn, then the documents' blocks
  // landed 78 rows above it and left it at 2,945 px). The row's own placing (`useRowOnScreen`)
  // sees only the row growing, and the browser's scroll anchoring holds what stands at the top
  // of the viewport, which is above the rows that land. So until the reader acts, the page puts
  // the row back on screen whenever the page's size changes (`bringOnScreen`); a key, a press, a
  // wheel or a touch is the reader taking over, and a focus that has moved off the row ends the
  // hold. One row is held at a time: a new hold ends the one before it, and holding the row that
  // is held already changes nothing.
  const screenHold = useRef<
    { readonly hold: AbortController; readonly row: HTMLElement } | undefined
  >(undefined);
  useEffect(() => () => screenHold.current?.hold.abort(), []);
  const holdOnScreen = useCallback((row: HTMLElement) => {
    const held = screenHold.current;
    if (held?.row === row && !held.hold.signal.aborted) return;
    held?.hold.abort();
    const hold = new AbortController();
    screenHold.current = { hold, row };
    for (const type of READER_ACTS) {
      window.addEventListener(type, () => hold.abort(), { capture: true, signal: hold.signal });
    }
    if (typeof ResizeObserver === "undefined") return;
    const grown = new ResizeObserver(() => {
      if (document.activeElement === row) bringOnScreen(row);
      else hold.abort();
    });
    grown.observe(document.body);
    hold.signal.addEventListener("abort", () => grown.disconnect());
  }, []);
  const targetId = target?.id;
  useEffect(() => {
    if (targetId === undefined || !focusReveal) return;
    const loose = document.activeElement === null || document.activeElement === document.body;
    if (taken.current === targetId && !loose) return;
    if (!rows.some((row) => row.kind === "node" && row.node.id === targetId)) return;
    const row = body.current?.querySelector<HTMLElement>(
      `[data-node-row][data-node-id="${CSS.escape(targetId)}"]`
    );
    if (row === null || row === undefined) return;
    taken.current = targetId;
    // A row already inside the viewport is focused where it stands, so nothing jumps twice: the
    // row's own focus places it by the page's one rule (`bringOnScreen`) when it is not.
    const box = row.getBoundingClientRect();
    row.focus({ preventScroll: box.top >= 0 && box.bottom <= window.innerHeight });
    holdOnScreen(row);
  }, [focusReveal, holdOnScreen, rows, targetId]);

  // The page opens with the keyboard focus on the first open question that waits on the reader,
  // in the outline's own order, its path unfolded and its row in the viewport; where none waits
  // on them, on the outline's first row (R7). It is the page opening: with no filter on, on an
  // address that names no node, before the reader has pressed, scrolled or touched anything and
  // while nothing else holds the focus. The reads arrive one after another, so the first row
  // takes the focus as soon as it is drawn and gives it to the question once the question's
  // read has answered; the question is where it stays, held on screen while the rest of the
  // page lands around it (`holdOnScreen`).
  const opening = useRef<{ done: boolean; on?: string; question?: boolean }>({ done: false });
  useEffect(() => {
    const settle = new AbortController();
    for (const type of READER_ACTS) {
      window.addEventListener(
        type,
        () => {
          opening.current.done = true;
          settle.abort();
        },
        { capture: true, signal: settle.signal }
      );
    }
    return () => settle.abort();
  }, []);
  useEffect(() => {
    const held = opening.current;
    if (held.done) return;
    if (!rootRow || targetId !== undefined) {
      held.done = true;
      return;
    }
    // The focus is the page's to place while it is on the row the page put it on, or nowhere.
    // Nowhere is where the browser leaves it when that row is drawn again as the rest of the
    // outline lands (measured: the first row lost it as the spec's blocks landed above it, and
    // the page was left with the focus on no row). Anywhere else, something else took it.
    const active = document.activeElement;
    const loose = active === null || active === document.body;
    if (!loose && active?.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId !== held.on) {
      held.done = true;
      return;
    }
    // Once the page has opened on a question, that question is where it stays.
    const question = held.question
      ? held.on
      : nodesInOrder(root).find((node) => node.type === "ask" && node.waiting)?.id;
    if (question !== undefined) {
      const row = body.current?.querySelector<HTMLElement>(
        `[data-node-row][data-node-id="${CSS.escape(question)}"]`
      );
      if (row === null || row === undefined) {
        // Its path is folded, or outside the zoom: going to it unfolds it and focuses its row
        // once drawn (`wanted`), and the drawing after that finds it here.
        if (!held.question) goToNode(question);
      } else {
        if (!row.contains(document.activeElement)) row.focus();
        holdOnScreen(row);
      }
      held.on = question;
      held.question = true;
      return;
    }
    const first = rows.find((row) => row.kind === "node");
    if (first?.kind !== "node") return;
    const row = body.current?.querySelector<HTMLElement>(
      `[data-node-row][data-row-key="${CSS.escape(rowKeyOf(first))}"]`
    );
    if (row === null || row === undefined || row.contains(document.activeElement)) return;
    held.on = first.node.id;
    row.focus({ preventScroll: true });
  }, [goToNode, holdOnScreen, root, rootRow, rows, targetId]);

  // A write from the line at the foot takes the reader to what they wrote (R16): their newest
  // node under what the write answered - the reply, the comment, the question - its branch
  // unfolded, inside the viewport and focused, or, for an answer, the question's own row. The
  // send has landed when the line asks, but the read that draws it is still out, so the nodes
  // of theirs already under it are noted first and the one that is new is gone to once a read
  // draws it. A read that never draws one where it was looked for - a filter's view that holds
  // no such row - leaves the reader on their newest node there, or on the row they wrote under.
  const landing = writer?.landing;
  const landedFrom = useRef<
    { readonly before: ReadonlySet<string>; readonly landing: object } | undefined
  >(undefined);
  const latest = useRef({ reach, reader, root, writer });
  latest.current = { reach, reader, root, writer };
  useEffect(() => {
    const { writer: line } = latest.current;
    if (landing === undefined || line === undefined) return;
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
    const own = parent.children.filter(
      (child) => reader !== undefined && child.author?.kind === "user" && child.author.id === reader
    );
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
    if (landing === undefined) return;
    const late = window.setTimeout(() => {
      const now = latest.current;
      const parent = landingParent(now.root, landing.under);
      now.writer?.landed();
      if (parent === undefined) return;
      const newest = parent.children
        .filter(
          (child) =>
            now.reader !== undefined &&
            child.author?.kind === "user" &&
            child.author.id === now.reader
        )
        .reduce<TreeNode | undefined>(
          (last, child) =>
            last === undefined || (child.at ?? "") >= (last.at ?? "") ? child : last,
          undefined
        );
      now.reach((newest ?? parent).id);
    }, LANDING_WAIT_MS);
    return () => window.clearTimeout(late);
  }, [landing]);

  // The tree is one tab stop, and it rests where the reader last stood: before they have
  // touched it that is its first row, so Tab reaches the tree rather than passing it by, and
  // after they have left it that is the row they left, so Shift+Tab brings them back to it.
  const keys = rows.map(rowKeyOf);
  const first = rows[0];
  const tabStop =
    stop !== undefined && keys.includes(stop)
      ? stop
      : first === undefined
        ? undefined
        : rowKeyOf(first);
  return (
    <div className="mb-3 flex min-w-0 flex-col">
      {breadcrumb.length === 0 ? null : (
        <p className={`flex min-w-0 flex-wrap items-baseline gap-1 text-xs ${textMutedOnCanvas}`}>
          {breadcrumb.map((above) => (
            <button
              // A crumb is a word in a line of text, not a control box: below 1280 px a plain
              // button is 44 px tall (`styles.css`), which is a third of the head the first
              // open question needs on a phone (R6). Its hit box grows by padding instead.
              // It draws one line for a node, so it is R1's first line, cut at a word, and it
              // wraps rather than being cut again mid-word by its box; the issue is named by
              // its key, since the header above draws its title.
              className="dispatch-inline-link max-w-full text-left underline [overflow-wrap:anywhere]"
              data-crumb={above.id}
              key={above.id}
              onClick={() => setZoom(above.id)}
              tabIndex={-1}
              type="button"
              {...authorText}
            >
              {above.id === root.id || above.text === ""
                ? above.id
                : drawnLine(firstLineOf(nodeWords(above)))}
            </button>
          ))}
          <span {...pageChrome}>/</span>
        </p>
      )}
      <div
        aria-label={CONTROL_LABELS.tree}
        className="flex min-w-0 flex-col"
        onBlur={releaseFocus}
        ref={body}
        role="tree"
      >
        <TreeRows
          artifacts={artifacts}
          context={context}
          cycle={cycle}
          editing={editing}
          focused={focused}
          isClosed={isClosed}
          onFocus={takeFocus}
          onOpenDeeper={openDeeper}
          onPin={pin}
          // A branch opens where it is at every width (R16): below 1024 px the outline draws
          // one level under the node it is read from, so an opened branch deeper than that is
          // one counted line, which opens its nodes in place too. Zooming is `.`'s alone.
          onToggle={(node, expanded) => unfold(node.id, expanded !== true)}
          {...(openBody === undefined ? {} : { openBody })}
          pinned={pinned}
          {...(report === undefined
            ? {}
            : { report: { line: report.line, row: report.row ?? tabStop } })}
          rows={rows}
          tabStop={tabStop}
          threads={threads}
        />
      </div>
      {root.children.length === 0 ? (
        <p className={`py-1 text-sm ${textMutedOnCanvas}`} {...pageChrome}>
          {emptyLine}
        </p>
      ) : null}
    </div>
  );
}

/** A reader's own acts, any of which ends the page's own placing of the focus: a key, a press,
 *  a wheel and a touch. */
const READER_ACTS = ["keydown", "pointerdown", "wheel", "touchstart"] as const;

/** How long a write's landing waits for the read that draws it before it takes the reader to
 *  their newest node where it looked, in milliseconds: the line's send has landed and its read
 *  is a request away, so a read that has not drawn the node by then is not going to. */
const LANDING_WAIT_MS = 10_000;

/** The node a write was made under, by the item the line wrote to; the tree's own root for a
 *  write on the issue itself; undefined where the view the tree draws holds no such node. */
function landingParent(
  root: TreeNode,
  under: { readonly id: string; readonly kind: string } | undefined
): TreeNode | undefined {
  return under === undefined ? root : pathToTarget(root, { id: under.id, kind: "item" })?.at(-1);
}

/** The asks of the Contents pane the outline has acted on (`OutlineRequest`): held by the ask
 *  itself, which the page keeps while the tree is drawn again, so each is acted on once
 *  whatever mounts the tree in between. */
const HANDLED = new WeakSet<OutlineRequest>();

/** The widths the tree is read one node at a time at, one level below the node it is on: below
 *  1024 px (R16), where three levels of indent and a row's own words do not fit together. It is
 *  the exact negation of the width the Contents pane is drawn from (`IssueBody.tsx`'s
 *  `CONTENTS_PANE_QUERY`), so no width - a zoomed browser, a fractional device-pixel ratio -
 *  draws the pane beside a tree read one node at a time, or neither. */
export const ZOOMED_TREE_QUERY = "not all and (min-width: 64rem)";

/** One ask the Contents pane makes of the outline: the node to go to, where there is one, and
 *  whether Focus branch is on as it asks. Each ask is a value of its own, acted on once
 *  (`HANDLED`); `seq` tells two asks for the same node apart where they are compared. */
export interface OutlineRequest {
  readonly focusBranch: boolean;
  readonly id: string | undefined;
  readonly seq: number;
}
