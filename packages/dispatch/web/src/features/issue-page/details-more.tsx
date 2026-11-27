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
import { claimBox, overlaps, ownScrollMark, rewatch, watchedIn } from "../shell/ViewportAnchor";
import { outlineItemRoute, useAddressFallback, useAddressTarget } from "./address-target";
import { DetailsErrorLine, useRetryHandOff, useSaidFailure } from "./details-error";
import { useListPages } from "./details-leaves";
import { DETAILS_ROW_ATTRIBUTE, type DetailsRow, useOpenDetailsAt } from "./details-rows";
import { FocusedNodeFacts } from "./FocusedNodeFacts";
import { type FocusedNode, useFocusedNode } from "./focused-node";
import { useHandoffFold } from "./HistoryHandoff";
import { IssueCollaboratorList } from "./IssueCollaboratorList";
import {
  type AddressedHistory,
  type AddressedTurn,
  HISTORY_HEADING_LANDED_EVENT,
  IssueHistory,
} from "./IssueHistory";
import { type IssueAddress, issueAddress } from "./issue-address";
import { LegionBlock, LegionRunsFollower } from "./LegionBlock";
import { DETAILS_ATTRIBUTE, useMarginDrawer } from "./margin-slot";
import { useOutlineReads } from "./outline-reads";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, issueStatusWord, STATE_LINES } from "./page-words";
import { nearestScroll, scrollingAncestor } from "./scroll-box";
import { useFollowing } from "./slots/following";
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
 *  the hold of the landed place waits for that focus before it holds nothing. */
const FOCUS_FRAMES = 4;

/** The collaborators' row inside Details, which `t o` and its address open Details at. */
const COLLABORATORS_ROW = `aside[${DETAILS_ATTRIBUTE}] [${DETAILS_ROW_ATTRIBUTE}="collaborators"]`;

/** The history inside Details, which `t v` puts the focus on. */
const HISTORY_IN_DETAILS = `aside[${DETAILS_ATTRIBUTE}] ${HISTORY}`;

/**
 * The people of the issue, under Opened by in Details' People section: the sessions subscribed
 * to it, counted, each with Unsubscribe, 20 at a time with Show more under them; and everyone who
 * worked it, running or not, which the nine-tab page gave a tab of its own. The collaborators
 * are read only once a reader opens their list, or once `/issues/<key>/collaborators` opens it,
 * so the People section costs the page one request on any other address: the subscribers' count.
 */
export function DetailsPeople({ issue }: DetailsMoreProps): ReactNode {
  const { pathname } = useLocation();
  const following = useFollowing();
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
  // Each following of the address opens the list, as it opens Details (`DetailsHistory`). Closing
  // it again is the reader's until the next, so this only ever opens.
  const collaboratorsFollowing = opensCollaborators ? following : undefined;
  useEffect(() => {
    if (collaboratorsFollowing !== undefined) setCollaboratorsOpen(true);
  }, [collaboratorsFollowing]);
  const subscribersRow = useRef<HTMLDivElement>(null);
  const subscribersControl = useRef<HTMLButtonElement>(null);
  const collaboratorsControl = useRef<HTMLButtonElement>(null);
  const subscriberPages = useListPages();
  // Each agent's Unsubscribe as the page draws it now, in the list's order: `SubscribedAgents`
  // draws one row per agent, the first of `rows` the list's pages draw, its Unsubscribe its
  // button.
  const drawnUnsubscribes = () => [
    ...(subscribersRow.current?.querySelectorAll<HTMLButtonElement>("li > button") ?? []),
  ];
  // A control of the Subscribers row that the reader's own press takes away hands the focus on in
  // the row rather than let it fall to the page, as Unpin in Pinned hands it on (`IssuePins.tsx`):
  // an Unsubscribe the reader pressed and confirmed, which goes with its agent's row once its
  // write lands and the agents are read again, to the Unsubscribe drawn in its place once the
  // list is drawn again - the next agent's, or the first of the next page's where the row was the
  // last one drawn - else the one before it; a Retry the reader pressed, which goes with its line
  // once what it sent again has landed or been read, to the Unsubscribe whose write is out
  // (`useRetryHandOff`); each else to the Subscribers control. A control that goes with no press
  // of the reader's - an agent another session unsubscribes, a line a read that answers on its
  // own takes away - leaves the focus on the page's body, as on every page of Dispatch, and
  // nothing in the row takes it. The Confirm dialog gives the focus back to the Unsubscribe it
  // opened from itself (`useDialog`).
  const afterRetry = () => {
    const writing = subscribers.unsubscribe.isPending ? refused : undefined;
    return (
      drawnUnsubscribes().find(
        (_, index) => writing !== undefined && subscribers.rows[index]?.session_id === writing
      ) ?? subscribersControl.current
    );
  };
  const readRetry = useRetryHandOff(afterRetry);
  const writeRetry = useRetryHandOff(afterRetry);
  // The control of the row the focus was last on (`onFocus` below); each agent's Unsubscribe as
  // the last drawing left it, with the session it is of; and the session whose Unsubscribe the
  // reader pressed and confirmed, until that Unsubscribe leaves the page or its write fails, which
  // took nothing away.
  const held = useRef<Element | null>(null);
  const unsubscribes = useRef<
    readonly { readonly control: HTMLButtonElement; readonly session: string | undefined }[]
  >([]);
  const unsubscribing = useRef<string | null>(null);
  useLayoutEffect(() => {
    const before = unsubscribes.current;
    const now = drawnUnsubscribes();
    unsubscribes.current = now.map((control, index) => ({
      control,
      session: subscribers.rows[index]?.session_id,
    }));
    const pressed = unsubscribing.current;
    if (pressed === null) return;
    if (subscribers.unsubscribe.isError) {
      unsubscribing.current = null;
      return;
    }
    const at = before.findIndex(({ session }) => session === pressed);
    const gone = before[at]?.control;
    if (gone === undefined || gone.isConnected) return;
    // A press belongs to the Unsubscribe it was made on: once that leaves, with the focus or not,
    // the press answers no later loss.
    unsubscribing.current = null;
    if (held.current !== gone) return;
    held.current = null;
    if (document.activeElement !== null && document.activeElement !== document.body) return;
    const next =
      now.slice(at).find((control) => !control.disabled) ??
      now
        .slice(0, at)
        .reverse()
        .find((control) => !control.disabled);
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
          <>
            <SubscribedAgents
              onUnsubscribe={(sessionId) => {
                unsubscribing.current = sessionId;
                subscribers.unsubscribe.mutate(sessionId);
              }}
              ownerLabel={issue.key}
              subscribers={subscribers.rows.slice(0, subscriberPages.drawn)}
            />
            {subscriberPages.more(
              subscribers.rows.length,
              (index) => subscribersRow.current?.querySelectorAll("li")[index]
            )}
          </>
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
            {...readRetry}
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
            {...writeRetry}
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
    text: issue.title,
    type: "issue",
  };
  return (
    <FocusedNodeFacts issueKey={issue.key} issueTitle={issue.title} node={node} state={state} />
  );
}

/**
 * The issue's history, under Pinned, and the addresses and keys that open Details at it.
 *
 * `/issues/<key>/activity`, and the address of a turn of a thread the history holds, open
 * Details at the history - its heading brought into view, or that turn marked, drawn and brought
 * into view; a handoff's page address, `/issues/<key>/handoffs/<id>`, opens it at the entry that
 * holds that handoff, marked, with the handoff open (`useHandoffFold`), and `/issues/<key>/steps`
 * at the newest run (`IssueHistory`'s `addressed`); `/issues/<key>/collaborators` opens Details
 * at the collaborators' row of the People section (`useOpenDetailsAt`), whose list
 * `DetailsPeople` opens. Each address opens Details once per following of it (`useFollowing`),
 * and the reader's own Close is the later word for as long as they stay there. This place is
 * drawn outside every section Details can collapse, so it is mounted for the life of the panel
 * and keeps the following it last opened for, the reader's read mark for the visit, which the
 * history is handed, and the handoffs they folded. `t v` and `t o` open `/activity` and
 * `/collaborators` and put the focus in what they open.
 *
 * The history's log is read only while Details is open or an address opens it, so a load that
 * leaves Details shut costs no request for it. Whether an item's address names a turn of a
 * thread the history holds is the outline's own landing's answer (`AgentsOnlyTurn`), so Details
 * opens at that turn exactly when the outline lands on the turn's nearest drawn ancestor and says
 * there that the thread is in the history.
 *
 * Directly above the history, outside the three sections and with no fold control of its own,
 * stands the Legion block (`LegionBlock`), drawn only while Details is open, beside the one part
 * that reads the issue's runs again as their revision moves (`LegionRunsFollower`): the two
 * observe one live view of the issue's Legion state, so Details open holds one subscription of
 * it and Details shut holds none.
 */
export function DetailsHistory({ issue, state }: DetailsMoreProps): ReactNode {
  const { hash, pathname, search } = useLocation();
  const following = useFollowing();
  const navigate = useNavigate();
  const route = parseIssuePath(pathname);
  const addressed =
    route === undefined ? undefined : issueAddress(route, search, hash, primarySpec(issue)?.slug);
  // An address naming an ask, a comment or a message of the outline, and opening no document:
  // the one address the outline's landing can take for a turn of the history.
  const namesItem =
    route !== undefined &&
    addressed !== undefined &&
    outlineItemRoute(route, search, addressed) !== undefined;
  // A turn of a thread the history holds (`agents-only.ts`) is no row of the outline: it is an
  // entry of the history, which Details opens at, with the turn marked and brought into view once
  // for each following of its address. The turn is the one `AgentsOnlyTurn` told for the
  // following the reader is at; until it tells, the one it told for the following before - at
  // another address, maybe a row's, or at this one under another fragment or key - is no answer
  // here.
  const [turn, setTurn] = useState<AddressedTurn | undefined>(undefined);
  const item = namesItem && turn?.following === following ? turn : undefined;
  // A handoff's page address and `/steps` name a place in the history: the entry that holds the
  // handoff, or the newest run, for the following of the address the reader is at.
  const inHistory: AddressedHistory | undefined =
    route?.kind === "handoff"
      ? { following, id: route.handoffId, kind: "handoff" }
      : route?.kind === "steps"
        ? { following, kind: "steps" }
        : undefined;
  const opensHistory = route?.kind === "activity" || inHistory !== undefined || item !== undefined;
  const opensCollaborators = route?.kind === "collaborators";
  const opensPanel = opensCollaborators || opensHistory;
  const drawer = useMarginDrawer();
  const openDetails = drawer?.setOpen;
  const openDetailsAt = useOpenDetailsAt();
  const holdLandedPlace = useLandedPlaceHold(drawer?.open);
  // Details opens once per following of an address that opens it (`opensPanel`): a link the reader
  // follows to the address they are on, and a fragment the browser changes under the one key it
  // gives every entry it makes, are each a following of their own and open it again, and the
  // reader's Close stands for the rest of the following. `openedFor` holds the following Details
  // last opened for while the reader stays at it, and none once they are at another following that
  // opens no Details. So an item's agents' turn, taken back until the outline's reads answer and
  // told again at the same following, opens nothing the reader closed; and Back to an earlier entry
  // of `/activity` or `/collaborators` from an address that opens no Details opens it again. Where
  // Details has no drawer to open, each following is noted as opened all the same.
  const openedFor = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (openedFor.current === following) return;
    openedFor.current = opensPanel ? following : undefined;
    if (!opensPanel || openDetails === undefined) return;
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
  }, [following, holdLandedPlace, openDetails, openDetailsAt, opensCollaborators, opensPanel]);
  const drawsHistory = drawer?.open === true || opensHistory;
  // Details is open: where it is a column of the page rather than a drawer, wherever it is drawn.
  const open = drawer === undefined || drawer.open;
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
  // The handoffs the reader folded on this issue's page, held beside the visit's read mark for
  // the same reason: the history is not drawn while Details is shut, so a fold held there would
  // not outlive a close and an open (the plan's D12). A handoff's page address opens its handoff
  // once for each following.
  const fold = useHandoffFold(issue.key, inHistory?.kind === "handoff" ? inHistory : undefined);
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
      {namesItem ? (
        <AgentsOnlyTurn
          address={addressed}
          issue={issue}
          onTurn={setTurn}
          route={route}
          search={search}
        />
      ) : null}
      {drawsHistory ? (
        <>
          {open ? (
            <>
              <LegionBlock issueKey={issue.key} />
              <LegionRunsFollower issueKey={issue.key} />
            </>
          ) : null}
          <IssueHistory
            activityFollowing={route?.kind === "activity" ? following : undefined}
            addressed={inHistory}
            fold={fold}
            issue={issue}
            item={item}
            state={state}
            visitMark={visitMark.current?.issue === issue.key ? visitMark.current.seq : lastReadSeq}
          />
        </>
      ) : null}
    </>
  );
}

/**
 * Tells `onTurn` the turn of a thread the history holds that `address` names, for the
 * following the reader is at (`useFollowing`), as the outline lands on it: the outline's own
 * decision (`address-target.ts`'s `useAddressFallback`, its `agents-only` kind), over the tree
 * and the reads `slots/body.tsx` hands it (`useOutlineReads`), so it is told once every read the
 * tree is built from has answered, as the outline lands on the turn's nearest drawn ancestor, and
 * taken back as the outline lets go of it. Those reads are the outline's own, under its keys and
 * with its freshness, so this costs no request of its own.
 *
 * Drawn only while the address names an ask, a comment or a message of the outline, the only
 * address that can name such a turn, so the tree is built here only then. It draws nothing.
 */
function AgentsOnlyTurn({
  address,
  issue,
  onTurn,
  route,
  search,
}: {
  address: IssueAddress;
  issue: IssueDetails;
  onTurn: (turn: AddressedTurn | undefined) => void;
  route: IssueRoute;
  search: string;
}): null {
  const following = useFollowing();
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
    onTurn({ following, id });
    return () => onTurn(undefined);
  }, [following, id, onTurn]);
  return null;
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
 * So once the focus is inside `target` - up to `frames` frames are waited for it, and nothing is
 * held if it never gets there - each change of height of the content of Details' scrolling box
 * around the focused element, at every depth (`watchedIn`), scrolls the box by as far as the
 * focused element then stands from the top kept. That top is measured afresh each time, so a move
 * anything else made, the browser's own scroll anchoring among them, is made good rather than
 * added to.
 *
 * The opener brings the control's whole row into the view, not the control itself: a row taller
 * than the view - the collaborators' row with its list open - that the box comes to from below,
 * once the reader has read far down Details, shows its foot, and the control at its head stays
 * above the view. So as the focus lands, a focused element no taller than the view and not wholly
 * in it is brought into it first, by the least scroll that shows it whole (`nearest`). An element
 * taller than the view, as the history `t v` focuses, is left where it is: its heading's own
 * landing places it (below). Where that landing already fired before the focus arrived and a row
 * above the history changed height in between, the hold brings the heading back to the top of
 * Details' box. That scroll is the hold's own, and the hold claims the box before its event is
 * raised, so neither the hold nor the history's anchor takes it for the reader's.
 *
 * The top kept is where the reader last saw the element: where the focus found it, once brought
 * into the view, and again where each scroll of the box the hold did not make itself left it -
 * the reader's wheel, a key, a drag, the browser's own anchoring. The hold tells its own scroll
 * from those by a mark of its own, built by the same rule as the history's anchor's
 * (`ownScrollMark`), since both scroll Details' box and the event of one's scroll can reach the
 * other. While a scroll has left the element wholly outside the view, the hold yields: it
 * corrects nothing, so whatever keeps the rows in view keeps them - the browser's own anchoring,
 * or the history's anchor where it keeps a place - and a scroll that brings the element back into
 * the view takes it as the place again.
 *
 * One keeper keeps Details' box at a time. While the hold keeps a top, it claims the box
 * (`claimBox`): the history's anchor lets its own place there go, keeps none and moves the box for
 * nothing, so no change of height is made good toward two places, the focused element's and an
 * entry of the history's. The hold gives the box back as it yields and as it ends, and the
 * anchor keeps places again by its own rules from the next one it records: an act of the
 * reader's, the history's own `keepRow`, a scroll.
 *
 * The reader's act that takes the box back, as `ViewportAnchor`'s `takeBox` states, ends the
 * hold, and its claim with it (`claimBox`'s `end`), and the history's anchor keeps the entry that
 * act names from then on. So a new event above the reader's entry, or an entry above it that
 * grows, leaves that entry where the reader sees it, where the hold of the history's top would
 * keep the heading and let the entry move.
 *
 * One move is the place the key asked for rather than one to make good: `t v` often reaches the
 * history while its list is still the loading line, at the end of what Details holds, and once
 * the list is drawn the history's own landing brings its heading to the top of Details' view
 * (`IssueHistory.tsx`). The heading says so with an event that bubbles through the focused
 * history (`HISTORY_HEADING_LANDED_EVENT`), and the top kept from then on is the one that landing
 * left.
 *
 * The hold lasts while the focus stays on the element and the reader names no place in the
 * history: a key, a press, the wheel or a touch that leaves the focus there and names none leaves
 * the hold on. The focus leaving the element ends it, and so do the reader's act that names a
 * place in the history and (`useLandedPlaceHold`) Details closing, the page going and the next
 * landing. Returns what ends the hold.
 */
function holdLandedPlace(target: string, frames: number): () => void {
  let frame = 0;
  let stop: (() => void) | undefined;
  // Whether the history's own landing already fired while the hold was waiting for the focus to
  // reach it: when the log is cached, the heading can land on the first frame and a row above
  // the history can change height before `findFocus` runs two frames later, pulling the heading
  // out of the top of Details' view.
  let headingLandedBeforeFocus = false;
  const noteHeadingLanded = (event: Event) => {
    if (
      event.target instanceof Element &&
      document.querySelector(HISTORY_IN_DETAILS)?.contains(event.target) === true
    ) {
      headingLandedBeforeFocus = true;
    }
  };
  if (target === HISTORY_IN_DETAILS) {
    document.addEventListener(HISTORY_HEADING_LANDED_EVENT, noteHeadingLanded);
  }
  const stopWatchingLanding = () => {
    document.removeEventListener(HISTORY_HEADING_LANDED_EVENT, noteHeadingLanded);
  };
  function release(): void {
    cancelAnimationFrame(frame);
    stopWatchingLanding();
    stop?.();
    stop = undefined;
  }
  const findFocus = (left: number): void => {
    const control = document.activeElement;
    if (
      !(control instanceof HTMLElement) ||
      document.querySelector(target)?.contains(control) !== true
    ) {
      if (left > 0) {
        frame = requestAnimationFrame(() => findFocus(left - 1));
      } else {
        stopWatchingLanding();
      }
      return;
    }
    stopWatchingLanding();
    // The box that scrolls the control, inside Details' own `aside`: the history's anchor finds
    // the box it keeps a place in by the same rule (`scrollingAncestor`), so the hold claims the
    // very box the anchor scrolls.
    const details = control.closest(`aside[${DETAILS_ATTRIBUTE}]`);
    const scroller = scrollingAncestor(control);
    if (
      scroller === null ||
      scroller === details ||
      (details !== null && !details.contains(scroller))
    ) {
      return;
    }
    // The control's top as the place to keep, or null while it lies wholly outside the view; and
    // the claim of the box (`claimBox`) while a top is kept, which the history's anchor ends, and
    // the hold with it, as the reader takes the box back (`takeBox`).
    let kept: number | null = null;
    let claim: (() => void) | undefined;
    const keepTop = () => {
      const at = control.getBoundingClientRect();
      kept = overlaps(at, scroller.getBoundingClientRect()) ? at.top : null;
      if (kept !== null) {
        claim ??= claimBox(scroller, release);
        return;
      }
      claim?.();
      claim = undefined;
    };
    // The hold moves Details' box alone, never the page under it, and marks each move its own.
    const own = ownScrollMark();
    const scrollOwn = (by: number) => {
      const before = scroller.scrollTop;
      scroller.scrollTop += by;
      own.move(before, scroller.scrollTop);
    };
    // When the focused control is the history's section and its heading already landed before
    // `findFocus` ran, restore the heading to the top of Details' box whatever the section's
    // height: a row above the history can shrink or grow in that window and pull the heading off
    // the top, and the section is taller than the view so `nearestScroll` would leave it there.
    // Every other control no taller than the view is brought into it before its top is kept: the
    // opener brings the control's row into the view, not the control, and `nearestScroll` brings
    // it in by its top from above the view and by its foot from below it.
    const landing = control.getBoundingClientRect();
    const view = scroller.getBoundingClientRect();
    if (headingLandedBeforeFocus) {
      const heading = control.querySelector("h2");
      if (heading !== null) {
        const drift = heading.getBoundingClientRect().top - view.top;
        if (drift !== 0) scrollOwn(drift);
      }
    } else if (landing.height <= view.height) {
      const by = nearestScroll(landing, view);
      if (by !== 0) scrollOwn(by);
    }
    keepTop();
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
      if (drift !== 0) scrollOwn(drift);
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
      claim?.();
      claim = undefined;
      sizes.disconnect();
      control.removeEventListener("focusout", release);
      control.removeEventListener(HISTORY_HEADING_LANDED_EVENT, keepTop);
      scroller.removeEventListener("scroll", scrolled);
    };
  };
  // The opener focuses the control in its second frame, and this looks in that frame, after it.
  frame = requestAnimationFrame(() => {
    frame = requestAnimationFrame(() => findFocus(frames));
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
