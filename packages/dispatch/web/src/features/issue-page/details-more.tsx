import { useQuery } from "@tanstack/react-query";
import {
  type FocusEvent,
  type ReactNode,
  type Ref,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import type { IssueDetails, UserState } from "../../api/types";
import { pageInkText, pageRowBorder } from "../../theme/classes";
import { stateForIssue } from "../issue/pins";
import { SubscribedAgents } from "../issue/SubscribedAgents";
import { buildIssuePath, parseIssuePath } from "../refs/routes";
import { useKeymap } from "../shell/keymap";
import { DetailsErrorLine } from "./details-error";
import { DETAILS_ROW_ATTRIBUTE, type DetailsRow, useOpenDetailsAt } from "./details-rows";
import { FocusedNodeFacts } from "./FocusedNodeFacts";
import { type FocusedNode, useFocusedNode } from "./focused-node";
import { IssueCollaboratorList } from "./IssueCollaboratorList";
import { IssueHistory } from "./IssueHistory";
import { agentsOnlyItem, issueAddress } from "./issue-address";
import { DETAILS_ATTRIBUTE, useMarginDrawer } from "./margin-slot";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, issueStatusWord, STATE_LINES } from "./page-words";
import { useIssueSubscribers } from "./useIssueSubscribers";

/** What each of Details' three places for part M is handed: the issue as the details slot reads
 *  it, and the reader's own state of every issue. */
export interface DetailsMoreProps {
  readonly issue: IssueDetails;
  readonly state: UserState | undefined;
}

/** The history in Details (`IssueHistory.tsx`), which `t v` puts the focus in. */
const HISTORY = '[data-testid="issue-history"]';

/** How many frames a key or an address waits for Details to draw what it opened before it gives
 *  up on reaching it, as the view row's chords wait (`useIssueKeymap.ts`). */
const DRAWING_FRAMES = 20;

/**
 * The people of the issue, under Opened by in Details' People section (dispatch://INFRA-1196
 * R13, R19): the sessions subscribed to it, counted, each with Unsubscribe; and everyone who
 * worked it, running or not, which the nine-tab page gave a tab of its own. The collaborators
 * are read only once a reader opens their list, or once `/issues/<key>/collaborators` opens it,
 * so the People section costs the page one request on any other address: the subscribers' count.
 */
export function DetailsPeople({ issue }: DetailsMoreProps): ReactNode {
  const { pathname } = useLocation();
  const opensCollaborators = parseIssuePath(pathname)?.kind === "collaborators";
  const subscribers = useIssueSubscribers(issue.key);
  // The session the last Unsubscribe was for: where Dispatch refused it, Retry sends it again.
  const refused = subscribers.unsubscribe.variables;
  const [subscribersOpen, setSubscribersOpen] = useState(false);
  const [collaboratorsOpen, setCollaboratorsOpen] = useState(opensCollaborators);
  // The address opens the list; closing it again is the reader's, so this only ever opens.
  useEffect(() => {
    if (opensCollaborators) setCollaboratorsOpen(true);
  }, [opensCollaborators]);
  const subscribersRow = useRef<HTMLDivElement>(null);
  const subscribersControl = useRef<HTMLButtonElement>(null);
  const collaboratorsControl = useRef<HTMLButtonElement>(null);
  // The control of the Subscribers row the focus was last on (`onFocus` below), and each agent's
  // Unsubscribe as the last drawing left them, in the list's order. An Unsubscribe goes with its
  // agent's row once its write lands, and a Retry with its line once what it sent again is out or
  // has been read, and the focus would fall to the page with them, so it goes on instead, as
  // Unpin in Pinned hands it on (`IssuePins.tsx`): from an Unsubscribe to the next agent's, and
  // from a Retry to the Unsubscribe whose write is out; else to the Subscribers control. The
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
        {subscribers.error ? (
          <DetailsErrorLine
            error={subscribers.failure}
            message={STATE_LINES.subscribers_unreadable}
            onRetry={subscribers.refetch}
            retrying={subscribers.fetching}
          />
        ) : null}
        {subscribers.unsubscribe.isError ? (
          <DetailsErrorLine
            message={ANSWER_LINES.unsubscribe_failed}
            onRetry={
              refused === undefined ? undefined : () => subscribers.unsubscribe.mutate(refused)
            }
            retrying={subscribers.unsubscribe.isPending}
          />
        ) : null}
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
 * The facts of the row the reader's focus is on, between Details' sections and Pinned
 * (dispatch://INFRA-1196 R12, R14), with the controls today's cards had for it and what
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
 * The issue's history, under Pinned (dispatch://INFRA-1196 R15, R20), and the addresses and
 * keys that open Details at it.
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
 * leaves Details shut costs no request for it (R21). The comments, messages and asks an item's
 * address is told by are the outline's own reads, under its keys.
 */
export function DetailsHistory({ issue, state }: DetailsMoreProps): ReactNode {
  const { hash, key, pathname, search } = useLocation();
  const navigate = useNavigate();
  const route = parseIssuePath(pathname);
  const addressed =
    route === undefined ? undefined : issueAddress(route, search, hash, primarySpec(issue)?.slug);
  // An address naming a comment or a message, and opening no document.
  const namesItem = addressed?.document === undefined && addressed?.target?.kind === "item";
  // The outline's reads, never stale here, as the history holds them (`IssueHistory.tsx`), so
  // an address that names an item reads the cache however long ago the outline read it.
  const comments = useQuery({
    enabled: namesItem,
    queryFn: () => api.listComments(issue.key),
    queryKey: ["comments", issue.key],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const messages = useQuery({
    enabled: namesItem,
    queryFn: () => api.listIssueMessages(issue.key),
    queryKey: ["issue-messages", issue.key],
    staleTime: Number.POSITIVE_INFINITY,
  });
  // The asks tell a reply under a question, which the outline draws, from one under an approval
  // request, which it does not (`agents-only.ts`).
  const asks = useQuery({
    enabled: namesItem,
    queryFn: () => api.listIssueAsks(issue.key, "all"),
    queryKey: ["asks", issue.key],
    staleTime: Number.POSITIVE_INFINITY,
  });
  // A comment or message an agent wrote outside every thread a person wrote in is no row of the
  // outline: it is an entry of the history, which Details opens at, with the entry marked.
  // The outline's own ask input (`tree-drafts.ts`), so an address names the same turns it does.
  const named = namesItem ? addressed?.target?.id : undefined;
  const item = useMemo(
    () =>
      named === undefined
        ? undefined
        : agentsOnlyItem({ id: named, kind: "item" }, comments.data, messages.data, [
            ...(issue.open_asks ?? []),
            ...(asks.data ?? []),
          ]),
    [asks.data, comments.data, issue.open_asks, messages.data, named]
  );
  const opensHistory = route?.kind === "activity" || item !== undefined;
  const opensCollaborators = route?.kind === "collaborators";
  const drawer = useMarginDrawer();
  const openDetails = drawer?.setOpen;
  const openDetailsAt = useOpenDetailsAt();
  const panelAddress = opensCollaborators || opensHistory ? `${pathname}${search}` : undefined;
  // One arrival at such an address is one navigation to it: the address with the key
  // react-router gives each navigation (`location.key`), so a link the reader follows to the
  // address they are on, which replaces it with itself (`RowText.tsx`), is an arrival too and
  // opens Details again. The address is part of it because a load, and a history entry the
  // router did not write, all carry the key "default".
  const arrival = panelAddress === undefined ? undefined : `${key} ${panelAddress}`;
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
    // opened if the reader closed it. The panel hears that request only once it is placed in
    // Details - on a fresh load, a commit or more after this one - so it is sent once Details
    // draws this history, which stands in the same panel; and a frame on, outside this effect,
    // where the section's own synchronous open could not run.
    if (opensCollaborators) {
      requestAnimationFrame(() => onceInDetails(HISTORY, () => openDetailsAt("collaborators")));
    }
  }, [arrival, openDetails, openDetailsAt, opensCollaborators]);
  const drawsHistory = drawer?.open === true || opensHistory;
  // The reader's read mark for this visit, where the history draws the line that says what is
  // new (R2): where it stood when the history first drew for this issue, and lower once it has
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
      },
    },
    {
      id: "tab-collaborators",
      keys: "t o",
      label: CONTROL_LABELS.open_collaborators,
      run: () => {
        navigate(buildIssuePath({ key: issue.key, kind: "collaborators" }));
        openDetailsAt("collaborators");
      },
    },
  ]);
  if (!drawsHistory) return null;
  return (
    <IssueHistory
      activityAddress={route?.kind === "activity" ? panelAddress : undefined}
      issue={issue}
      item={item}
      state={state}
      visitMark={visitMark.current?.issue === issue.key ? visitMark.current.seq : lastReadSeq}
    />
  );
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
