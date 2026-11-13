import { isIdPrefix, itemFromSearch } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import {
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import { artifactBlocksQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { NoTooltips } from "../../components/tooltips";
import {
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useSelectedItem } from "../artifacts/ArtifactDocument";
import { ConversationTab } from "../conversation/ConversationTab";
import { conversationScrollport } from "../conversation/use-follow-latest";
import { useFillMarginSlot, useMarginDrawsSlot } from "../margin/margin-slot";
import { statusText } from "../project/board-model";
import { buildIssuePath, type IssueRoute, type IssueTab, parseIssuePath } from "../refs/routes";
import { NotFoundPage } from "../shell/NotFoundPage";
import { ISSUE_FRAME, usePageFrame } from "../shell/page-frame";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { useUserPreference } from "../shell/userPreference";
import { narrowConversation } from "./conversation-states";
import { DocumentStages } from "./document-stage";
import { type FocusedNode, FocusedNodeProvider, useFocusedNode } from "./focused-node";
import { IssueBody } from "./IssueBody";
import { IssueHeader } from "./IssueHeader";
import { IssueMarginCards, IssueMarginPanel } from "./IssueMargin";
import { IssuePresetPanels } from "./IssuePresetPanels";
import { ItemMatchList } from "./ItemMatchList";
import {
  agentsOnlyItem,
  conversationStatePath,
  type IssueAddress,
  issueAddress,
} from "./issue-address";
import { OpenedDocument } from "./OpenedDocument";
import {
  CONVERSATION_STATE_ORDER,
  type ConversationState,
  NODE_TYPE_NAMES,
  NODE_TYPES,
  sentenceCase,
} from "./page-words";
import { stateForIssue } from "./pins";
import { useDocumentToolbars } from "./useDocumentToolbars";
import { useIssueDetail } from "./useIssueDetail";
import { useIssueKeymap } from "./useIssueKeymap";
import { useIssuePriority } from "./useIssuePriority";
import { type ItemLanding, type ItemRoute, isItemRoute, useItemLanding } from "./useItemLanding";
import { VersionControl } from "./VersionControl";

/* The issue page's own root is `ISSUE_FRAME` (`features/shell/page-frame.tsx`), which the
   shell also marks from the address alone until this page registers it, so a reload lands on
   the page rather than on a column with no root around it. */

export function IssuePage(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) {
    return <NotFoundPage />;
  }
  // key={route.key}: switching to a different issue remounts IssueDetail
  // fresh (discarding any unsaved local drafts); switching tabs within the
  // same issue keeps route.key unchanged, so it only re-renders. The focus provider is above
  // it because the page itself reads what the focus is on: the panel draws that node's facts,
  // and the preset row draws its document's toolbar (`focused-node.tsx`).
  return (
    <FocusedNodeProvider key={route.key}>
      <DocumentStages>
        <IssueDetail route={route} />
      </DocumentStages>
    </FocusedNodeProvider>
  );
}

/** An item deep link the SPA could not resolve: the id names nothing on this issue, or the
 *  lookup failed. Either way the reader is told which item, inside the issue page. The item's
 *  name is the page's own word for that kind of node (`page-words.ts`). */
function ItemLandingFailure({
  landing,
  route,
}: {
  landing: Extract<ItemLanding, { kind: "missing" | "unavailable" }>;
  route: ItemRoute;
}): ReactNode {
  const noun = NODE_TYPE_NAMES[route.kind];
  if (landing.kind === "missing") {
    return (
      <NotFoundPage
        backLabel={`Back to ${route.key}`}
        backTo={`/issues/${route.key}`}
        detail={`That link doesn't name ${route.kind === "ask" ? "an" : "a"} ${noun} on ${route.key} — it may have been deleted.`}
        title={`${sentenceCase(noun)} not found`}
      />
    );
  }
  return (
    <section>
      <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>
        Couldn&apos;t load this {noun}
      </h1>
      <div className="mt-2">
        <QueryError message={`Couldn't load this ${noun}.`} onRetry={landing.retry} />
      </div>
    </section>
  );
}

/** How long after a Back or Forward the browser's own scroll restoration is still arriving, so
 *  the page leaves the window where that reader asked for it rather than zeroing it. */
const POP_RESTORES_MS = 100;

/** The conversation's state as the browser keeps it, and back: the state's own word, and what
 *  people said for anything else - a browser holding nothing yet, and one holding a state this
 *  build no longer has. Both are read here rather than inline, so the hook's setter keeps one
 *  identity across renders and the panel's `onPick` with it. */
function readConversationState(stored: string | null): ConversationState {
  return CONVERSATION_STATE_ORDER.find((state) => state === stored) ?? "words";
}

function writeConversationState(state: ConversationState): string {
  return state;
}

/** The outline address a navigation carried in its state (`NodeMenu.tsx`), if it carried one. */
function outlineOf(state: unknown): string | undefined {
  if (typeof state !== "object" || state === null || !("outline" in state)) return undefined;
  return typeof state.outline === "string" ? state.outline : undefined;
}

/**
 * Whether the address opens its document in place of the outline (R3, R20). A document's own
 * address opens the document, with one exception: an address naming a block of it that the
 * outline holds as a node - a typed block, which a citation of a check or a plan task names - is
 * where that block now stands, so it opens the outline at that node, as it did before the
 * document was taken out from under the outline. A block no node holds - a paragraph of the
 * document's own text - opens the document at the block. Which blocks are typed is the blocks
 * read the outline is built from, so this asks it under the same key and sends nothing of its
 * own; until it answers, the address draws the outline it most likely names.
 */
function useOpensDocument(address: IssueAddress, artifact: Artifact | undefined): boolean {
  const block = address.target?.kind === "block" ? address.target.id : undefined;
  const asksBlocks = address.document !== undefined && block !== undefined;
  const blocks = useQuery({
    ...artifactBlocksQuery(artifact?.id ?? ""),
    enabled: asksBlocks && artifact?.kind === "doc",
  });
  if (address.document === undefined) return false;
  if (!asksBlocks || artifact?.kind !== "doc") return true;
  if (blocks.data === undefined) return blocks.isError;
  const types: readonly string[] = NODE_TYPES;
  return !blocks.data.some((one) => one.id === block && types.includes(one.type));
}

function IssueDetail({ route }: { route: IssueRoute }): ReactNode {
  const {
    activeTab,
    artifactRoute,
    conversationFocusItemId,
    highlightTerm,
    issue,
    primaryArtifact,
    planArtifact,
    selectedArtifact,
    state,
    verificationArtifact,
  } = useIssueDetail(route);
  const landing = useItemLanding(route);
  const { hash, pathname, search, state: locationState } = useLocation();
  const navigate = useNavigate();
  const panelScroll = useRef<Partial<Record<IssueTab, number>>>({});
  const { diffs, showDiff, toolbarSink, toolbars } = useDocumentToolbars();
  // The document a row of the tree is being edited in, so the page draws its one editor for
  // that row to take onto its own stage (`document-stage.tsx`).
  const [editing, setEditing] = useState<string | undefined>(undefined);

  // What this address opens: its preset, the document it opens, the node it names, the
  // conversation's state, the filter and the version it pins (`issue-address.ts`). A block a
  // `/spec` address names is the spec's own, looked up in the spec by its slug.
  const addressed = issueAddress(route, search, hash, primaryArtifact?.slug);
  // An address naming an ask, a comment or a message and opening no document lands on its node.
  const namesItem = addressed.document === undefined && addressed.target?.kind === "item";
  // The ask or comment an address's `?ask=` or `?comment=` names for the outline to open on. An
  // id written short is resolved as a document's address resolves one, and the address is
  // replaced with the whole id, which names the node (`ArtifactDocument`'s `useSelectedItem`);
  // a whole id reads nothing more.
  const queryItem =
    namesItem && !isItemRoute(route) ? (itemFromSearch(search) ?? undefined) : undefined;
  const queryLanding = useSelectedItem({ key: route.key, kind: "issue" }, queryItem);
  const listed = addressed.documentItem;
  const marked = listed?.kind === "mark" ? listed.id : undefined;
  // The issue's comments and messages, under the keys the outline, the conversation and the
  // margin read them by (`useIssueTree.ts`, `useMarginItems.ts`), so they cost no request of
  // their own: they say whether the item the address names sits in a thread no person wrote in,
  // and whether the item `?mark=` names is a comment or an ask. Only such an address reads them
  // here; on every other the outline reads them as it draws.
  const comments = useQuery({
    enabled: namesItem || marked !== undefined,
    queryFn: () => api.listComments(route.key),
    queryKey: ["comments", route.key],
  });
  const messages = useQuery({
    enabled: namesItem,
    queryFn: () => api.listIssueMessages(route.key),
    queryKey: ["issue-messages", route.key],
  });
  // `?mark=` names an ask or a comment by its id alone: a comment when the issue's comments
  // hold that id - or, for an id written short, one starting with it, which the document then
  // resolves and writes whole under `?mark=` (`ArtifactDocument`'s `marked`) - and an ask
  // otherwise. The document marks neither until they have answered.
  const documentItem =
    listed?.kind !== "mark"
      ? listed
      : comments.data === undefined
        ? undefined
        : {
            id: listed.id,
            kind: comments.data.some(
              (comment) =>
                comment.id === listed.id ||
                (isIdPrefix(listed.id) && comment.id.startsWith(listed.id))
            )
              ? ("comment" as const)
              : ("ask" as const),
            marked: true,
          };
  // A comment or message an agent wrote outside every thread a person wrote in lands in the
  // history and not in the outline (R20). The history is not in the panel yet, so it lands in
  // today's: what `/activity` opens - the conversation in its Everything state with the page's
  // history drawn under the outline - with the conversation marking the entry, and the outline
  // takes no focus for it. An address that opens a document lands in that document.
  const historyItem = namesItem
    ? agentsOnlyItem(addressed.target, comments.data, messages.data)
    : undefined;
  const address: IssueAddress =
    historyItem === undefined
      ? addressed
      : { ...addressed, conversation: "everything", target: undefined };
  // The document the address opens in place of the outline (R3, R20): a document's own address,
  // and a `/spec` address that names a block of the spec, or an item at a pinned version or
  // `?mark=`, which opens the spec as `/artifacts/<spec>` with the same query and fragment does
  // (`issue-address.ts`). Any other address naming an ask or a comment opens the outline on its
  // node instead, whatever document it addresses.
  const documentRoute: Extract<IssueRoute, { kind: "artifact" }> | undefined =
    artifactRoute ??
    (route.kind !== "spec" || address.document === undefined
      ? undefined
      : address.version === undefined
        ? { key: route.key, kind: "artifact", slug: address.document }
        : { key: route.key, kind: "artifact", slug: address.document, version: address.version });
  const documentArtifact =
    artifactRoute === undefined && documentRoute !== undefined ? primaryArtifact : selectedArtifact;
  const opensDocument = useOpensDocument(address, documentArtifact);
  // Which state the conversation is in. The reader's pick is kept for their login in this
  // browser, the way this page already keeps the children section and the panel's collapsed
  // property sections (`features/shell/userPreference.ts`, `useChildrenSection.ts`,
  // `useMarginSections.ts`): it outlives the page it was made on, so a navigation away and
  // back, and the next issue they open, open in the state they picked (R13,
  // c-conversation-state). Nothing picked yet is what people said.
  const [kept, keep] = useUserPreference<ConversationState>(
    "issue.conversation-state",
    readConversationState,
    writeConversationState
  );
  // An address that names a state outranks what the reader kept, because arriving there is the
  // reader asking for it: `/conversation` and `/activity` name a state of their own, and an
  // address naming one comment, message or ask names what people said, which holds that entry
  // whatever was kept - a kept Questions or Pinned drew the list without it, and the link
  // landed nowhere (R19, `issue-address.ts`). Only until the reader picks again on that
  // address: picking Questions on `/activity` opens no address of its own
  // (`conversationStatePath`) and leaves them where they are, and that pick is the later word
  // for as long as they stay there. Leaving the address lets the pick go, so the next arrival
  // at `/activity` is asking for Everything again and draws its history.
  const [pickedAt, setPickedAt] = useState<string | undefined>(undefined);
  if (pickedAt !== undefined && pickedAt !== pathname) setPickedAt(undefined);
  const conversationState = pickedAt === pathname ? kept : (address.conversation ?? kept);

  // The issue page's own root is the frame the shell draws around this column and the margin
  // beside it: the identity block, the body and the panel's properties are one page, however the
  // shell mounts them (`features/shell/page-frame.tsx`, `e2e/issue-page.ts`).
  usePageFrame(ISSUE_FRAME);
  // Where the panel goes: the shell's margin wherever it keeps a column for it, and the page's
  // own column under the identity block wherever it does not - at the width the margin is a
  // sheet, and while the reader has collapsed it to its rail. The margin answers that itself
  // (`features/margin/margin-slot.tsx`), so the panel is drawn exactly once and the properties
  // are read at every width and under either preference (R11, R12).
  const marginDrawsPanel = useMarginDrawsSlot();
  // The panel's one stage: an element held for the life of the page, which this page draws the
  // panel into and which is *appended* into whichever host is on the screen - the margin's
  // column, or the page's own. It is the move an open document makes between its hosts
  // (`document-stage.tsx`): the panel keeps one React parent through it, so nothing in it is
  // unmounted and nothing in it is lost - an unsent message in the conversation's composer, an
  // open blocked-by edit, the Subscribers, Collaborators and References lists a reader opened.
  // Drawing it again in each host instead remounted all of that whenever the margin was
  // collapsed or shown.
  const panelStage = useMemo(() => {
    const stage = document.createElement("div");
    // No box of its own: the panel is a child of whichever host draws it, as it was when each
    // host drew one of its own.
    stage.className = "contents";
    return stage;
  }, []);
  // The ref both hosts hold the stage by, and the whole of the move: each takes the stage as
  // it is drawn, and lets go of it only while it is still the one holding it. React detaches
  // the host that is going before it attaches the one that is arriving, so a collapse that
  // swaps them hands the stage over inside one commit, with nothing in it unmounted.
  const holdPanel = useCallback(
    (host: HTMLElement | null) => {
      if (host === null) return;
      host.append(panelStage);
      return () => {
        // Off the page while no host draws it - the margin keeps the column and is not
        // mounted at all - rather than thrown away.
        if (panelStage.parentElement === host) panelStage.remove();
      };
    },
    [panelStage]
  );
  const details = issue.data;
  const focusedNode = useFocusedNode();
  // Nothing has the focus until the tree lands, and a reader on no node is reading the issue,
  // so the issue itself is what the panel draws the facts of.
  const focused: FocusedNode = focusedNode ?? {
    id: route.key,
    state: details === undefined ? undefined : statusText(details.status),
    title: details?.title ?? route.key,
    type: "issue",
  };
  // Where the reader was when they left this address, taken before the move: the tablist used
  // to say so before it changed tab (`onBeforeTabChange`), and the presets are navigation, so
  // every control that leaves an address says so first. Taken afterwards it would be the
  // position the page had already clamped to as the panel it is leaving went.
  // The Conversation is drawn in the page's one right-hand panel, which is a scrollport of its
  // own wherever the margin keeps a column, so where the reader was in it is that box's scroll
  // and not the window's (`conversation/use-follow-latest.ts`).
  const recordScroll = useCallback(() => {
    const port = activeTab === "conversation" ? conversationScrollport() : undefined;
    panelScroll.current[activeTab] = port === undefined ? window.scrollY : port.scrollTop;
  }, [activeTab]);

  // Which address a picked state opens, where it opens one at all: the two states that are
  // addresses of their own, picked from the other's address (`issue-address.ts`'s
  // `conversationStatePath`), which is the state that address names. Everywhere else the state
  // is the panel's own and is held for the address the reader is on, which keeps their preset,
  // their `?q=`, their `#b-` and the document they have open; a move that does open an address
  // carries that `?q=` with it.
  const pickState = useCallback(
    (next: ConversationState) => {
      const to = conversationStatePath(route, next);
      keep(next);
      if (to === undefined || to === pathname) {
        setPickedAt(pathname);
        // Nothing navigates, so the list this state narrows is mounted here: a reader who has
        // never opened the conversation would otherwise pick a state with nothing under it.
        setActivatedTabs((open) => (open.conversation ? open : { ...open, conversation: true }));
        return;
      }
      recordScroll();
      const query = new URLSearchParams();
      if (address.filter !== "") query.set("q", address.filter);
      const rest = query.toString();
      navigate(`${to}${rest === "" ? "" : `?${rest}`}`);
    },
    [address.filter, keep, navigate, pathname, recordScroll, route]
  );
  // The Conversation's first placement runs as a child layout effect, before the restore below
  // in the same commit, so a position recorded while the tab showed only its loading text is
  // forgotten here and the reader lands on the newest message. A return to a list the reader was
  // already placed in never fires it, so that restore stands (INFRA-303).
  //
  // That placement is spent on the list's first cards, which is when the reader opened the
  // conversation and not when `/conversation` is opened (`useFollowLatest`'s `reading` holds
  // the two apart: what arrives later moves the list only while they are reading it). A pick
  // that keeps the page draws the list where the reader is (R12, R19, `pickState`), so the two
  // are rarely the same moment: tying the placement to the address instead placed a reader at
  // the newest card after they had been reading this list for as long as they liked, which is
  // their place thrown away rather than a stale record forgotten.
  const forgetConversationScroll = useCallback(() => {
    delete panelScroll.current.conversation;
  }, []);
  const issueArtifacts = useMemo(
    () => new Map((issue.data?.artifacts ?? []).map((artifact) => [artifact.id, artifact])),
    [issue.data?.artifacts]
  );
  // Once a panel's tab has ever been active, keep rendering its content even while hidden -
  // that is what keeps the Conversation's read observer and the history alive across a move to
  // another address (see the `hidden` panels below). A panel the user has never opened stays
  // unmounted, so a fresh page load doesn't pay for panels it never shows.
  const [activatedTabs, setActivatedTabs] = useState<Record<IssueTab, boolean>>(() => ({
    activity: activeTab === "activity",
    artifacts: activeTab === "artifacts",
    children: activeTab === "children",
    collaborators: activeTab === "collaborators",
    conversation: activeTab === "conversation",
    decisions: activeTab === "decisions",
    plan: activeTab === "plan",
    spec: activeTab === "spec",
    verification: activeTab === "verification",
  }));
  if (!activatedTabs[activeTab]) {
    setActivatedTabs({ ...activatedTabs, [activeTab]: true });
  }
  // An agents-only item lands in the history and the conversation, which mount for it as their
  // own addresses mount them.
  if (historyItem !== undefined && !(activatedTabs.activity && activatedTabs.conversation)) {
    setActivatedTabs({ ...activatedTabs, activity: true, conversation: true });
  }
  // The conversation's one list, which the panel draws under the four state controls that
  // drive it (R12): it belongs to the panel and to no column of the page, and it is drawn
  // wherever the panel is. It is mounted once its own address has been opened and kept
  // mounted after that, as every other panel of the page is (`activatedTabs`): the margin's
  // own cards are already on the page, so a second list of the same comments at every address
  // would double the page's controls and its first load's reads. Whether the reader is *in*
  // it - what places them at the newest card, what marks a card read, what a deep link
  // scrolls to - is the address's own answer (`visible`).
  const conversationList = useMemo(
    () =>
      details === undefined || !activatedTabs.conversation ? undefined : (
        <ConversationTab
          issueArtifacts={issueArtifacts}
          focusItemId={conversationFocusItemId ?? historyItem}
          // The outline is the landing of every item address but one in the history, and keeps
          // its node on screen; on a phone the conversation shares the page's scroll, so only a
          // history landing scrolls the page to its entry (R20).
          focusMovesPage={historyItem !== undefined}
          isClosed={details.closed_at !== null}
          issueKey={details.key}
          narrow={narrowConversation(
            conversationState,
            stateForIssue(state.data, details.key).dismissed ?? []
          )}
          onFirstPlacement={forgetConversationScroll}
          state={state.data}
          visible={activeTab === "conversation" || historyItem !== undefined}
        />
      ),
    [
      activatedTabs.conversation,
      activeTab,
      conversationFocusItemId,
      conversationState,
      details,
      forgetConversationScroll,
      historyItem,
      issueArtifacts,
      state.data,
    ]
  );
  // `/issues/<key>/collaborators`, the address the Collaborators tab answered, opens the
  // panel's list of everyone who worked the issue (R19); every other address leaves it shut.
  const opensCollaborators = route.kind === "collaborators";
  // What the margin draws: its own cards, and - wherever it keeps a column for this page's
  // panel - the host that panel is moved into under them (`IssueMargin.tsx`'s
  // `IssueMarginCards`). The panel itself is drawn once, below, and never a second time here:
  // it is one element moved between the two hosts, so the margin opening and closing throws
  // away neither an unsent comment nor anything else a reader has started in it.
  const panel = useCallback(
    (conversation: ReactNode) =>
      details === undefined ? (
        conversation
      ) : (
        <IssueMarginCards conversation={conversation} panelHost={holdPanel} />
      ),
    [details, holdPanel]
  );
  useFillMarginSlot(panel);

  // Every key the issue page binds, in its own scope (`useIssueKeymap.ts`). The priority write
  // is the page's one, shared with the identity block and the palette, so a refusal of any of
  // them is reported once.
  const priorityWrite = useIssuePriority(route.key);
  useIssueKeymap(priorityWrite, route.key);

  // Where the reader left this tab, which this commit is about to put them back at. It is read
  // here, in the render, and not only in the effect below, because the identity block's two
  // forms are 26 px apart in flow height: the block is told the place first (`restoringTo`), so
  // its height is final before the scroll is written and the browser anchors nothing away.
  const restoringTo = panelScroll.current[activeTab];

  useLayoutEffect(() => {
    const top = panelScroll.current[activeTab];
    if (top === undefined) return;
    // Taken, so the next render of this tab reads the page's own position again rather than a
    // place the reader has since left; leaving the tab records it afresh.
    delete panelScroll.current[activeTab];
    (activeTab === "conversation" ? (conversationScrollport() ?? window) : window).scrollTo({
      top,
    });
  }, [activeTab]);

  // The page scrolls the window itself - the tree brings a focused row clear of the sticky
  // identity block (`identity-block.ts`'s `bringOnScreen`), and below the width the margin
  // keeps a column at the conversation places the reader at its newest card the same way - so
  // it puts the window back as it leaves. Nothing else resets it, and a page read through the
  // scroll the page before it left measures every box against the wrong viewport. A landing
  // the next page performs runs after this cleanup and stands.
  //
  // Back and Forward are the one exception: the browser restores the position it recorded for
  // the entry it is returning to, and zeroing the window would undo that. A pop in the same
  // task as the unmount is that reader, and the page leaves the window to them.
  const popped = useRef(0);
  useEffect(() => {
    const pop = () => {
      popped.current = Date.now();
    };
    window.addEventListener("popstate", pop);
    return () => {
      window.removeEventListener("popstate", pop);
      if (Date.now() - popped.current > POP_RESTORES_MS) window.scrollTo({ top: 0 });
    };
  }, []);

  useDocumentTitle(
    issue.data === undefined ? "Dispatch" : `${issue.data.key} · ${issue.data.title} · Dispatch`
  );

  // An approval request's own address puts the focus on the approval the identity block draws
  // for the document it reviews, since the outline draws no node for it (`useItemLanding`'s
  // `approval`, R20). It runs once the identity block is drawn, and again only for another
  // landing: after that the focus is the reader's.
  const approvalOf = landing?.kind === "approval" ? landing.artifactId : undefined;
  const headerDrawn = issue.data !== undefined && !state.isPending;
  useEffect(() => {
    if (approvalOf === undefined || !headerDrawn) return;
    document
      .querySelector<HTMLElement>(`[data-approval-artifact="${CSS.escape(approvalOf)}"]`)
      ?.focus();
  }, [approvalOf, headerDrawn]);

  // A failed issue read is reported before an item link's landing: the item's own read may still
  // be out, or out for good, and reading the landing's pending state first turns a 500 into
  // "Loading issue…" for as long as it is.
  if (issue.isError || state.isError) {
    const notFound = issue.error instanceof ApiError && issue.error.status === 404;
    return (
      <section>
        <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>
          {notFound ? "Issue not found" : "Couldn't load this issue"}
        </h1>
        {notFound ? (
          <p className={`mt-2 text-sm ${textSecondaryOnCanvas}`}>
            {route.key} doesn&apos;t exist, or you don&apos;t have access to it.
          </p>
        ) : (
          <div className="mt-2">
            <QueryError
              message="Couldn't load this issue."
              onRetry={() => {
                void issue.refetch();
                void state.refetch();
              }}
            />
          </div>
        )}
        <Link
          className={`mt-4 inline-block text-sm font-medium underline ${linkText} ${linkHoverText}`}
          to="/"
        >
          Back to inbox
        </Link>
      </section>
    );
  }
  if (issue.isPending || state.isPending || landing?.kind === "pending") {
    return <p className={textMutedOnCanvas}>Loading issue…</p>;
  }
  if (issue.data === undefined) {
    return <p className={dangerText}>Could not load this issue.</p>;
  }
  // The rewrite replaces the address with the whole id rather than adding to the reader's
  // history, so Back still leaves the page they came from.
  if (landing?.kind === "redirect" || landing?.kind === "rewrite") {
    return <Navigate replace to={landing.to} />;
  }
  if (queryLanding.rewrite !== undefined) {
    return <Navigate replace to={queryLanding.rewrite} />;
  }
  if (isItemRoute(route) && landing?.kind === "ambiguous") {
    return <ItemMatchList candidates={landing.candidates} route={route} />;
  }
  if (queryItem !== undefined && queryLanding.candidates !== undefined) {
    return (
      <ItemMatchList
        candidates={queryLanding.candidates}
        route={{ id: queryItem.id, key: route.key, kind: queryItem.kind }}
      />
    );
  }
  if (isItemRoute(route) && (landing?.kind === "missing" || landing?.kind === "unavailable")) {
    return <ItemLandingFailure landing={landing} route={route} />;
  }

  if (primaryArtifact === undefined) {
    return <p className={dangerText}>Could not load this issue&apos;s primary document.</p>;
  }

  const issueState = stateForIssue(state.data, issue.data.key);
  const isClosed = issue.data.closed_at !== null;
  const issueKey = issue.data.key;
  // The document the filters' version control reads while the outline is drawn (R2): the
  // document of the filter's role, whose typed blocks the filter draws on an issue the
  // conversion has not reached, and with no filter on the issue's spec, as the Design's picture
  // of such an issue draws it. The Decisions filter has no document of its role and draws none.
  const filterDocument = (
    {
      checks: verificationArtifact,
      decisions: undefined,
      plan: planArtifact,
      spec: primaryArtifact,
      tree: primaryArtifact,
    } as const
  )[address.preset];
  // The outline the reader goes back to from a document they opened: the address they opened
  // it from, which the node menu hands over (`NodeMenu.tsx`), and the issue's own otherwise.
  const outline = outlineOf(locationState) ?? buildIssuePath({ key: issueKey, kind: "issue" });
  // A version picked in the outline pins the filter's address to it (`?v=`), and keeps its
  // words filter; the fragment is a node of the version the reader is leaving.
  const pinFilterVersion = (version: number | null) => {
    const query = new URLSearchParams(search);
    if (version === null) query.delete("v");
    else query.set("v", String(version));
    const rest = query.toString();
    navigate(`${pathname}${rest === "" ? "" : `?${rest}`}`);
  };
  // A document opened at a version of its own: its own address at that version, which keeps
  // the outline it was opened from to go back to.
  const openDocumentAt = (artifact: Artifact, version: number | undefined) =>
    navigate(buildIssuePath({ key: issueKey, kind: "artifact", slug: artifact.slug, version }), {
      state: { outline },
    });
  const outlineToolbar =
    filterDocument === undefined ? undefined : (
      <VersionControl
        artifact={filterDocument}
        isClosed={isClosed}
        // Nothing in the outline is the document's text to draw a comparison over, so the
        // comparison opens the document at the pinned version with its changes drawn.
        onShowDiffChange={(next) => {
          showDiff(filterDocument.id, next);
          if (next) openDocumentAt(filterDocument, address.version);
        }}
        onVersionChange={pinFilterVersion}
        showDiff={false}
        toolbar={toolbars.get(filterDocument.id)}
        version={address.version}
      />
    );
  const opened = opensDocument && documentRoute !== undefined ? documentArtifact : undefined;
  const openedId = opened?.id ?? "";
  const openedDocument =
    opensDocument && documentRoute !== undefined
      ? {
          panel: (
            <OpenedDocument
              artifact={documentArtifact}
              artifactRoute={documentRoute}
              item={documentItem}
              highlightTerm={highlightTerm}
              isClosed={isClosed}
              issueKey={issueKey}
              onShowDiff={(next) => showDiff(openedId, next)}
              onToolbarChange={toolbarSink(openedId)}
              onVersionChange={(version) =>
                opened === undefined ? undefined : openDocumentAt(opened, version ?? undefined)
              }
              outline={outline}
              showDiff={diffs.has(openedId)}
              toolbar={toolbars.get(openedId)}
            />
          ),
          toolbar:
            opened === undefined || opened.kind !== "doc" ? undefined : (
              <VersionControl
                artifact={opened}
                isClosed={isClosed}
                onShowDiffChange={(next) => showDiff(opened.id, next)}
                onVersionChange={(version) => {
                  showDiff(opened.id, false);
                  openDocumentAt(opened, version ?? undefined);
                }}
                showDiff={diffs.has(opened.id)}
                toolbar={toolbars.get(opened.id)}
                version={documentRoute.version}
              />
            ),
        }
      : undefined;
  // The document a row of the outline is being edited in. A document opened in place of the
  // outline leaves no row to edit in, so one document never has two editors.
  const edited =
    editing === undefined || openedDocument !== undefined
      ? undefined
      : issue.data.artifacts?.find((artifact) => artifact.id === editing);

  return (
    // A flex column, so the identity block's fourth row can be drawn at its end on a phone
    // (`IssueHeader`'s `order-last`); every other child keeps its document order.
    //
    // `NoTooltips`: nothing this page draws keeps meaning for a pointer alone, because a
    // reader on a phone or on the keyboard never sees a tooltip (R2, R21). What a tooltip
    // said is drawn instead - the claim's instant beside its age, a link's whole address -
    // and the panel says the same for the margin's own cards (`IssueMargin.tsx`).
    <NoTooltips>
      <section className="flex min-w-0 flex-col">
        <IssueHeader
          documentArtifact={primaryArtifact}
          isClosed={isClosed}
          issue={issue.data}
          priorityWrite={priorityWrite}
          restoringTo={restoringTo}
          state={issueState}
        />
        {/* The head of the page, the preset row, and the children section with the one folded
          tree the presets are queries over (R1, R3, R5, R10) - or the document the address
          opens, in their place. A row being edited takes its own document's editor onto its
          stage (`document-stage.tsx`). */}
        <IssueBody
          address={address}
          isClosed={isClosed}
          issue={issue.data}
          onEditing={setEditing}
          onLeave={recordScroll}
          openedDocument={openedDocument}
          plan={planArtifact}
          toolbar={outlineToolbar}
        />
        {/* Wherever the shell's margin draws no panel of its own - at the width it is a sheet,
          and while the reader has collapsed it to its rail - the panel's stage is held here,
          under the identity block: the properties, the facts of the focused node and the
          conversation are read at every width and under either preference with nothing
          opened (R11, R12). */}
        {marginDrawsPanel ? null : (
          <div
            className="mb-3 flex min-w-0 flex-col gap-3"
            data-testid="issue-margin"
            ref={holdPanel}
          />
        )}
        {/* The panel itself, drawn once for the life of the page into its own stage, which the
          host above or the margin's own holds (`panelStage`). It is rendered here, in the
          page's tree, wherever it is placed: a reader moving between the two hosts keeps their
          unsent message, their open blocked-by edit and every list they opened in it. */}
        {createPortal(
          <IssueMarginPanel
            conversationList={conversationList}
            conversationState={conversationState}
            focused={focused}
            isClosed={isClosed}
            issue={issue.data}
            onConversationState={pickState}
            openCollaborators={opensCollaborators}
          />,
          panelStage
        )}
        <IssuePresetPanels
          activatedTabs={activatedTabs}
          activeTab={activeTab}
          conversationState={conversationState}
          editedDocument={edited}
          history={historyItem !== undefined}
          isClosed={isClosed}
          issueKey={issueKey}
          onToolbarSink={toolbarSink}
          onVersionChange={(artifact, version) => openDocumentAt(artifact, version ?? undefined)}
          state={state.data}
        />
      </section>
    </NoTooltips>
  );
}
