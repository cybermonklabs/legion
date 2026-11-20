import {
  type FocusEvent,
  type ReactNode,
  type Ref,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { primarySpec } from "../../api/issue-cache";
import type { IssueDetails, UserState } from "../../api/types";
import { pageInkText, pageRowBorder } from "../../theme/classes";
import { stateForIssue } from "../issue/pins";
import { SubscribedAgents } from "../issue/SubscribedAgents";
import { buildIssuePath, type IssueRoute, parseIssuePath } from "../refs/routes";
import { useKeymap } from "../shell/keymap";
import { ownScrollMark, rewatch, watchedIn } from "../shell/ViewportAnchor";
import { outlineItemRoute, useAddressFallback, useAddressTarget } from "./address-target";
import { DetailsSteps } from "./DetailsSteps";
import { DetailsErrorLine, useSaidFailure } from "./details-error";
import { DETAILS_ROW_ATTRIBUTE, type DetailsRow, useOpenDetailsAt } from "./details-rows";
import { FocusedNodeFacts } from "./FocusedNodeFacts";
import { type FocusedNode, useFocusedNode } from "./focused-node";
import { IssueCollaboratorList } from "./IssueCollaboratorList";
import { HISTORY_HEADING_LANDED_EVENT, IssueHistory } from "./IssueHistory";
import { type IssueAddress, issueAddress } from "./issue-address";
import { DETAILS_ATTRIBUTE, useMarginDrawer } from "./margin-slot";
import { useOutlineReads } from "./outline-reads";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, issueStatusWord, STATE_LINES } from "./page-words";
import { useIssueSubscribers } from "./useIssueSubscribers";

/** What each of Details' three places below is handed: the issue as the details slot reads it,
 *  and the reader's own state of every issue. */
export interface DetailsMoreProps {
  readonly issue: IssueDetails;
  readonly state: UserState | undefined;
}

/** The history in Details (`IssueHistory.tsx`), which `t v` puts the focus in. */
const HISTORY = '[data-testid="issue-history"]';

/** How many frames a key or an address waits for Details to draw what it opened before it gives
 *  up on reaching it, as the view row's chords wait (`useIssueKeymap.ts`). */
const DRAWING_FRAMES = 20;

/** How many frames past the two the opener takes to focus the row's control (`useOpenDetailsAt`)
 *  the hold of the landed place waits for that focus to arrive before it holds nothing. */
const FOCUS_FRAMES = 4;

/** The collaborators' row inside Details, which `t o` and its address open Details at. */
const COLLABORATORS_ROW = `aside[${DETAILS_ATTRIBUTE}] [${DETAILS_ROW_ATTRIBUTE}="collaborators"]`;

/** The history inside Details, which `t v` puts the focus on. */
const HISTORY_IN_DETAILS = `aside[${DETAILS_ATTRIBUTE}] ${HISTORY}`;

/**
 * The people of the issue, under Opened by in Details' People section: the sessions subscribed
 * to it, counted, each with Unsubscribe; and everyone who
 * worked it, running or not, which the nine-tab page gave a tab of its own. The collaborators
 * are read only once a reader opens their list, or once `/issues/<key>/collaborators` opens it,
 * so the People section costs the page one request on any other address: the subscribers' count.
 */
export function DetailsPeople({ issue }: DetailsMoreProps): ReactNode {
  const { key, pathname, search } = useLocation();
  const opensCollaborators = parseIssuePath(pathname)?.kind === "collaborators";
  const subscribers = useIssueSubscribers(issue.key);
  // The session the last Unsubscribe was for: where Dispatch refused it, Retry sends it again.
  const refused = subscribers.unsubscribe.variables;
  // The refusal the row says: the last Unsubscribe's, and still while the one its Retry sent
  // again is out, so the line, and its Retry with a reader's focus, stays until that one is
  // answered; a refusal that repeats is counted anew (`useSaidFailure`).
  const unsubscribeRefusal = useSaidFailure(subscribers.unsubscribe);
  const [subscribersOpen, setSubscribersOpen] = useState(false);
  const [collaboratorsOpen, setCollaboratorsOpen] = useState(opensCollaborators);
  // Each arrival at the address opens the list, as it opens Details (`DetailsHistory`), with the
  // same arrival; closing it again is the reader's until the next, so this only ever opens.
  const arrival = arrivalAt(key, opensCollaborators ? `${pathname}${search}` : undefined);
  useEffect(() => {
    if (arrival !== undefined) setCollaboratorsOpen(true);
  }, [arrival]);
  const subscribersRow = useRef<HTMLDivElement>(null);
  const subscribersControl = useRef<HTMLButtonElement>(null);
  const collaboratorsControl = useRef<HTMLButtonElement>(null);
  // The control of the Subscribers row the focus was last on (`onFocus` below), and each agent's
  // Unsubscribe as the last drawing left them, in the list's order. An Unsubscribe goes with its
  // agent's row once its write lands, and a Retry with its line once what it sent again has
  // landed or been read, and the focus would fall to the page with them, so it goes on instead,
  // as Unpin in Pinned hands it on (`IssuePins.tsx`): from an Unsubscribe to the next agent's,
  // and from a Retry to the Unsubscribe whose write is out; else to the Subscribers control. The
  // Confirm dialog gives the focus back to the Unsubscribe it opened from itself (`useDialog`).
  const held = useRef<Element | null>(null);
  const unsubscribes = useRef<readonly HTMLButtonElement[]>([]);
  useLayoutEffect(() => {
    const before = unsubscribes.current;
    // `SubscribedAgents` draws one row per agent, in `rows`' order, its Unsubscribe its button.
    const drawn = [
      ...(subscribersRow.current?.querySelectorAll<HTMLButtonElement>("li > button") ?? []),
    ];
    unsubscribes.current = drawn;
    const gone = held.current;
    if (gone === null || gone.isConnected) return;
    held.current = null;
    if (document.activeElement !== null && document.activeElement !== document.body) return;
    const at = before.indexOf(gone as HTMLButtonElement);
    const writing = subscribers.unsubscribe.isPending ? refused : undefined;
    const next =
      at === -1
        ? drawn.find(
            (_, index) => writing !== undefined && subscribers.rows[index]?.session_id === writing
          )
        : before.slice(at + 1).find((control) => control.isConnected && !control.disabled);
    (next ?? subscribersControl.current)?.focus();
  });
  return (
    <>
      <WideRow
        onFocus={(event) => {
          held.current = event.target;
        }}
        ref={subscribersRow}
      >
        <button
          aria-expanded={subscribersOpen}
          className={`flex min-h-11 items-center gap-2 rounded-lg text-sm font-medium md:min-h-8 ${pageInkText}`}
          onClick={() => setSubscribersOpen((open) => !open)}
          ref={subscribersControl}
          type="button"
          {...pageChrome}
        >
          <span>
            {subscribers.counted ? `${PROPERTY_LABELS.subscribers}:` : PROPERTY_LABELS.subscribers}
          </span>
          {subscribers.counted ? <span {...authorText}>{subscribers.rows.length}</span> : null}
        </button>
        {subscribersOpen ? (
          <SubscribedAgents
            onUnsubscribe={(sessionId) => subscribers.unsubscribe.mutate(sessionId)}
            ownerLabel={issue.key}
            subscribers={subscribers.rows}
          />
        ) : null}
        {/* Neither Retry is disabled while what it sent is out, since Chromium drops the focus of
            a focused button that turns disabled to the page: its line stays, the read's Retry
            reads busy and a press then joins the read that is out, and the write's Retry takes
            no press (`refused`) and a press then is dropped. Each failure is drawn afresh, so one
            that repeats in the same words is said again. */}
        {subscribers.error ? (
          <DetailsErrorLine
            error={subscribers.failure}
            failure={subscribers.errorUpdateCount}
            message={STATE_LINES.subscribers_unreadable}
            onRetry={subscribers.refetch}
            retrying={subscribers.fetching}
          />
        ) : null}
        {unsubscribeRefusal === null ? null : (
          <DetailsErrorLine
            failure={unsubscribeRefusal.count}
            message={ANSWER_LINES.unsubscribe_failed}
            onRetry={
              refused === undefined
                ? undefined
                : () => {
                    if (!subscribers.unsubscribe.isPending) subscribers.unsubscribe.mutate(refused);
                  }
            }
            refused={subscribers.unsubscribe.isPending}
          />
        )}
      </WideRow>
      <WideRow row="collaborators">
        <button
          aria-expanded={collaboratorsOpen}
          className={`flex min-h-11 items-center gap-2 rounded-lg text-sm font-medium md:min-h-8 ${pageInkText}`}
          onClick={() => setCollaboratorsOpen((open) => !open)}
          ref={collaboratorsControl}
          type="button"
          {...pageChrome}
        >
          <span>{PROPERTY_LABELS.collaborators}</span>
        </button>
        {collaboratorsOpen ? (
          <div className="w-full" {...authorText}>
            <IssueCollaboratorList issueKey={issue.key} owner={collaboratorsControl} />
          </div>
        ) : null}
      </WideRow>
    </>
  );
}

/**
 * The facts of the row the reader's focus is on, between Details' sections and Pinned, with the
 * controls today's cards had for it and what
 * references it. Until the outline gives a row the focus, and whenever the reader is on no row,
 * the focus is the issue, so the facts are the issue's own: its kind, its title and its status.
 */
export function DetailsFocusedNode({ issue, state }: DetailsMoreProps): ReactNode {
  const focusedNode = useFocusedNode();
  const node: FocusedNode = focusedNode ?? {
    id: issue.key,
    state: issueStatusWord(issue.status),
    title: issue.title,
    type: "issue",
  };
  return <FocusedNodeFacts issueKey={issue.key} node={node} state={state} />;
}

/**
 * The issue's history, under Pinned, and the addresses and keys that open Details at it.
 *
 * `/issues/<key>/activity`, and the address of a comment or a message of a thread agents wrote
 * among themselves, open Details at the history - its heading brought into view, or that turn
 * marked; `/issues/<key>/collaborators` opens Details at the collaborators' row of the People
 * section (`useOpenDetailsAt`), whose list `DetailsPeople` opens. Each address opens Details once
 * per arrival at it, and the reader's own Close is the later word for as long as they stay
 * there. This place is drawn outside every section Details can collapse, so it is mounted for
 * the life of the panel and keeps that count, and the reader's read mark for the visit, which
 * the history is handed. `t v` and `t o` open those two addresses and put the focus in what
 * they open.
 *
 * The history's log is read only while Details is open or an address opens it, so a load that
 * leaves Details shut costs no request for it. Whether an item's address names a turn of a
 * thread agents wrote among themselves is the outline's own landing's answer
 * (`AgentsOnlyTurn`), so Details opens at that turn exactly when the outline lands on the turn's
 * nearest drawn ancestor and says there that the thread is the agents' alone.
 */
export function DetailsHistory({ issue, state }: DetailsMoreProps): ReactNode {
  const { hash, key, pathname, search } = useLocation();
  const navigate = useNavigate();
  const route = parseIssuePath(pathname);
  const addressed =
    route === undefined ? undefined : issueAddress(route, search, hash, primarySpec(issue)?.slug);
  // An address naming an ask, a comment or a message of the outline, and opening no document:
  // the one address the outline's landing can take for a turn only agents wrote in.
  const namesItem =
    route !== undefined &&
    addressed !== undefined &&
    outlineItemRoute(route, search, addressed) !== undefined;
  // A comment or message an agent wrote outside every thread a person wrote in is no row of the
  // outline: it is an entry of the history, which Details opens at, with the entry marked. The
  // entry is the turn `AgentsOnlyTurn` told for this arrival at the address; until it tells, the
  // one it told for the arrival before - at another address, maybe a row's - is no answer here.
  const itemArrival = namesItem ? arrivalAt(key, `${pathname}${search}`) : undefined;
  const [turn, setTurn] = useState<AgentsTurn | undefined>(undefined);
  const item = turn !== undefined && turn.arrival === itemArrival ? turn.id : undefined;
  const opensHistory = route?.kind === "activity" || item !== undefined;
  const opensCollaborators = route?.kind === "collaborators";
  const drawer = useMarginDrawer();
  const openDetails = drawer?.setOpen;
  const openDetailsAt = useOpenDetailsAt();
  const holdLandedPlace = useLandedPlaceHold(drawer?.open);
  const panelAddress = opensCollaborators || opensHistory ? `${pathname}${search}` : undefined;
  // Details opens once per arrival at such an address (`arrivalAt`), so a link the reader
  // follows to the address they are on opens it again.
  const arrival = arrivalAt(key, panelAddress);
  const openedFor = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (arrival === undefined || openDetails === undefined) {
      openedFor.current = arrival;
      return;
    }
    if (openedFor.current === arrival) return;
    openedFor.current = arrival;
    openDetails(true);
    // At the collaborators' row, its control focused, with the People section that holds it
    // opened if the reader closed it, and the control kept where it lands while the rows above it
    // grow (`useLandedPlaceHold`). The panel hears that request only once it is placed in
    // Details - on a fresh load, a commit or more after this one - so it is sent once Details
    // draws this history, which stands in the same panel; and a frame on, outside this effect,
    // where the section's own synchronous open could not run.
    if (opensCollaborators) {
      requestAnimationFrame(() =>
        onceInDetails(HISTORY, () => {
          openDetailsAt("collaborators");
          holdLandedPlace(COLLABORATORS_ROW, FOCUS_FRAMES);
        })
      );
    }
  }, [arrival, holdLandedPlace, openDetails, openDetailsAt, opensCollaborators]);
  const drawsHistory = drawer?.open === true || opensHistory;
  // The reader's read mark for this visit, where the history draws the line that says what is
  // new: where it stood when the history first drew for this issue, and lower once it has
  // been moved back elsewhere, never higher. It is held here and not in the history, which is
  // not drawn while Details is shut, so a close and an open of Details keep the line where the
  // reader found it, however far the mark moved on in between.
  const lastReadSeq = stateForIssue(state, issue.key).last_read_seq;
  const visitMark = useRef<{ readonly issue: string; readonly seq: number } | undefined>(undefined);
  const held = visitMark.current?.issue === issue.key ? visitMark.current : undefined;
  if (state !== undefined && (held === undefined ? drawsHistory : lastReadSeq < held.seq)) {
    visitMark.current = { issue: issue.key, seq: lastReadSeq };
  }
  useKeymap("issue", [
    {
      id: "tab-activity",
      keys: "t v",
      label: CONTROL_LABELS.open_history,
      run: () => {
        navigate(buildIssuePath({ key: issue.key, kind: "activity" }));
        openDetails?.(true);
        requestAnimationFrame(() => reachInDetails(HISTORY));
        holdLandedPlace(HISTORY_IN_DETAILS, DRAWING_FRAMES);
      },
    },
    {
      id: "tab-collaborators",
      keys: "t o",
      label: CONTROL_LABELS.open_collaborators,
      run: () => {
        navigate(buildIssuePath({ key: issue.key, kind: "collaborators" }));
        openDetailsAt("collaborators");
        holdLandedPlace(COLLABORATORS_ROW, FOCUS_FRAMES);
      },
    },
  ]);
  return (
    <>
      {namesItem && itemArrival !== undefined ? (
        <AgentsOnlyTurn
          address={addressed}
          arrival={itemArrival}
          issue={issue}
          onTurn={setTurn}
          route={route}
          search={search}
        />
      ) : null}
      {drawsHistory ? (
        <>
          <IssueHistory
            activityAddress={route?.kind === "activity" ? panelAddress : undefined}
            issue={issue}
            item={item}
            state={state}
            visitMark={visitMark.current?.issue === issue.key ? visitMark.current.seq : lastReadSeq}
          />
          {/* The runs of the issue's steps, read only while Details is open: an address that
              opens Details draws them a commit before the drawer opens, and they wait for it.
              Where Details is a column of the page rather than a drawer, it is open wherever
              it is drawn. */}
          <DetailsSteps issueKey={issue.key} open={drawer === undefined || drawer.open} />
        </>
      ) : null}
    </>
  );
}

/** The turn of a thread only agents wrote in that one arrival at an address names
 *  (`AgentsOnlyTurn`), with that arrival (`arrivalAt`). */
interface AgentsTurn {
  readonly arrival: string;
  readonly id: string;
}

/**
 * Tells `onTurn` the turn of a thread only agents wrote in that `address` names, for `arrival`,
 * as the outline lands on it: the outline's own decision (`address-target.ts`'s
 * `useAddressFallback`, its `agents-only` kind), over the tree and the reads `slots/body.tsx`
 * hands it (`useOutlineReads`), so it is told once every read the tree is built from has
 * answered, as the outline lands on the turn's nearest drawn ancestor, and taken back as the
 * outline lets go of it. Those reads are the outline's own, under its keys and with its
 * freshness, so this costs no request of its own.
 *
 * Drawn only while the address names an ask, a comment or a message of the outline, the only
 * address that can name such a turn, so the tree is built here only then. It draws nothing.
 */
function AgentsOnlyTurn({
  address,
  arrival,
  issue,
  onTurn,
  route,
  search,
}: {
  address: IssueAddress;
  arrival: string;
  issue: IssueDetails;
  onTurn: (turn: AgentsTurn | undefined) => void;
  route: IssueRoute;
  search: string;
}): null {
  const { reads, readsHeld, root } = useOutlineReads(issue);
  const { reveal } = useAddressTarget(root, address);
  const fallback = useAddressFallback({
    address,
    issue,
    reads,
    readsHeld,
    reveal,
    root,
    route,
    search,
  });
  const id = fallback?.kind === "agents-only" ? fallback.id : undefined;
  useEffect(() => {
    if (id === undefined) return;
    onTurn({ arrival, id });
    return () => onTurn(undefined);
  }, [arrival, id, onTurn]);
  return null;
}

/**
 * One arrival at `address`, an address that opens Details, which is one navigation to it: the
 * address with the key react-router gives each navigation (`location.key`), so a link the
 * reader follows to the address they are on, which replaces it with itself (`RowText.tsx`), is
 * an arrival too. The address is part of it because a load, and a history entry the router did
 * not write, all carry the key "default". `undefined` while the reader is at no such address.
 */
function arrivalAt(locationKey: string, address: string | undefined): string | undefined {
  return address === undefined ? undefined : `${locationKey} ${address}`;
}

/**
 * Runs `then` with what `selector` names inside Details, once Details has drawn it: a key or an
 * address opens Details, and what that opens is on the page a frame or two later. A part inside
 * a hidden element is not drawn, and a page that never draws it runs nothing.
 */
function onceInDetails(
  selector: string,
  then: (found: HTMLElement) => void,
  left = DRAWING_FRAMES
): void {
  const found = document.querySelector<HTMLElement>(`aside[${DETAILS_ATTRIBUTE}] ${selector}`);
  if (found === null || found.closest("[hidden]") !== null) {
    if (left > 0) requestAnimationFrame(() => onceInDetails(selector, then, left - 1));
    return;
  }
  then(found);
}

/** Puts the focus on what `selector` names inside Details, once Details has drawn it; a page
 *  that never draws it leaves the focus where it was. */
function reachInDetails(selector: string): void {
  onceInDetails(selector, (found) => {
    found.focus({ preventScroll: true });
    found.scrollIntoView({ block: "nearest" });
  });
}

/**
 * What starts the hold of the place a key or an address lands the focus at in Details
 * (`holdLandedPlace`), called right after Details is asked to open there. A hold already running
 * ends first, and Details closing or the page going ends it too. `open` is whether Details is
 * open, undefined where the page draws no Details drawer - every page but the new issue page -
 * where nothing is held.
 */
function useLandedPlaceHold(open: boolean | undefined): (target: string, frames: number) => void {
  const release = useRef<(() => void) | undefined>(undefined);
  useEffect(() => {
    if (open === false) release.current?.();
  }, [open]);
  useEffect(() => () => release.current?.(), []);
  const drawn = open !== undefined;
  return useCallback(
    (target: string, frames: number) => {
      release.current?.();
      release.current = drawn ? holdLandedPlace(target, frames) : undefined;
    },
    [drawn]
  );
}

/**
 * Keeps the focus a key or an address put in Details where it landed in Details' view: the
 * Collaborators control, which `t o` and its address focus two frames after they bring its row
 * into view (`useOpenDetailsAt`), and the history, which `t v` focuses once Details draws it. A
 * row above it that reads what it shows once Details opens - Blocked by, which reads the titles
 * of the issues it names - grows when that read lands, after the focus, and would push the
 * focused element down, out of the view where the reader sees the focus; a focused element
 * keeps its own place, as the CSS scroll-anchoring spec takes the focused element first.
 * So once the focus has arrived inside `target` - up to `frames` frames are waited for it, and
 * nothing is held if it never arrives - each change of height of the content of Details'
 * scrolling box around the focused element, at every depth (`watchedIn`), scrolls the box by as
 * far as the focused element then stands from the top kept. That top is measured afresh each
 * time, so a move anything else made, the browser's own scroll anchoring among them, is made good
 * rather than added to.
 *
 * The top kept is where the reader last saw the element: where the focus found it, and again
 * where each scroll of the box the hold did not make itself left it - the reader's wheel, a key,
 * a drag, the browser's own anchoring. The hold tells its own scroll from those by a mark of its
 * own, built by the same rule as the history's anchor's (`ownScrollMark`), since both scroll
 * Details' box in the same frames. While a scroll has left the element wholly outside the view,
 * the hold yields: it corrects nothing, so whatever keeps the rows in view keeps them - the
 * browser's own anchoring, or the history's anchor where it keeps a place - and a scroll that
 * brings the element back into the view takes it as the place again.
 *
 * One move is the place the key asked for rather than one to make good: `t v` often reaches the
 * history while its list is still the loading line, at the end of what Details holds, and once
 * the list is drawn the history's own landing brings its heading to the top of Details' view
 * (`IssueHistory.tsx`). The heading says so with an event that bubbles through the focused
 * history (`HISTORY_HEADING_LANDED_EVENT`), and the top kept from then on is the one that landing
 * left.
 *
 * The hold lasts while the focus stays on the element: a key, a press, the wheel or a touch that
 * leaves it there leaves the hold on. The focus leaving the element ends it, and so
 * (`useLandedPlaceHold`) do Details closing, the page going and the next landing. Returns what
 * ends the hold.
 */
function holdLandedPlace(target: string, frames: number): () => void {
  let frame = 0;
  let stop: (() => void) | undefined;
  function release(): void {
    cancelAnimationFrame(frame);
    stop?.();
    stop = undefined;
  }
  const arrive = (left: number): void => {
    const control = document.activeElement;
    if (
      !(control instanceof HTMLElement) ||
      document.querySelector(target)?.contains(control) !== true
    ) {
      if (left > 0) frame = requestAnimationFrame(() => arrive(left - 1));
      return;
    }
    // The box that scrolls the control: the nearest element around it, inside Details' own
    // `aside`, whose content scrolls.
    const details = control.closest(`aside[${DETAILS_ATTRIBUTE}]`);
    let box = control.parentElement;
    while (
      box !== null &&
      box !== details &&
      !/^(auto|scroll)$/.test(getComputedStyle(box).overflowY)
    ) {
      box = box.parentElement;
    }
    if (box === null || box === details) return;
    const scroller = box;
    // The control's top as the place to keep, or null while it lies wholly outside the view.
    let kept: number | null = null;
    const keepTop = () => {
      const at = control.getBoundingClientRect();
      const view = scroller.getBoundingClientRect();
      kept = at.bottom > view.top && at.top < view.bottom ? at.top : null;
    };
    keepTop();
    const own = ownScrollMark();
    // What of the box's content around the control is watched, brought up to that content as it
    // is at each change: what is drawn around the control later is watched from then on.
    let watched: ReadonlySet<Element> = new Set();
    const watch = () => {
      const now = watchedIn(control, scroller);
      rewatch(sizes, watched, now);
      watched = now;
    };
    const sizes = new ResizeObserver(() => {
      if (document.activeElement !== control) {
        release();
        return;
      }
      watch();
      if (kept === null) return;
      const drift = control.getBoundingClientRect().top - kept;
      if (drift === 0) return;
      const before = scroller.scrollTop;
      scroller.scrollTop += drift;
      own.move(before, scroller.scrollTop);
    });
    const scrolled = () => {
      if (!own.take(scroller.scrollTop)) keepTop();
    };
    watch();
    control.addEventListener("focusout", release);
    control.addEventListener(HISTORY_HEADING_LANDED_EVENT, keepTop);
    scroller.addEventListener("scroll", scrolled, { passive: true });
    stop = () => {
      own.end();
      sizes.disconnect();
      control.removeEventListener("focusout", release);
      control.removeEventListener(HISTORY_HEADING_LANDED_EVENT, keepTop);
      scroller.removeEventListener("scroll", scrolled);
    };
  };
  // The opener focuses the control in its second frame, and this looks in that frame, after it.
  frame = requestAnimationFrame(() => {
    frame = requestAnimationFrame(() => arrive(frames));
  });
  return release;
}

/** One row of Details that its own control names, as `IssueMargin.tsx`'s `Row wide` draws it
 *  (the prototype's `.frow`); `row` names the row a control elsewhere opens Details at
 *  (`details-rows.ts`). */
function WideRow({
  children,
  onFocus,
  ref,
  row,
}: {
  children: ReactNode;
  onFocus?: (event: FocusEvent<HTMLDivElement>) => void;
  ref?: Ref<HTMLDivElement>;
  row?: DetailsRow;
}): ReactNode {
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handler only notes which of the row's controls the focus is on, so the row needs no role of its own.
    <div
      className={`grid min-w-0 grid-cols-1 gap-2.5 border-b py-2 text-sm leading-5 ${pageRowBorder}`}
      onFocus={onFocus}
      ref={ref}
      {...(row === undefined ? {} : { [DETAILS_ROW_ATTRIBUTE]: row })}
    >
      <div
        className={`flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1 [overflow-wrap:break-word] ${pageInkText}`}
      >
        {children}
      </div>
    </div>
  );
}
