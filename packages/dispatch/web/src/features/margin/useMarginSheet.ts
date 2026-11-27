import { itemFromSearch } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation } from "react-router-dom";

import type { WriteBounds } from "../../api/client";
import { artifactAtAddress, primarySpec } from "../../api/issue-cache";
import { issueQuery, whoAmIQuery } from "../../api/queries";
import type { Ask } from "../../api/types";
import { useSending } from "../../hooks/useSending";
import { useHeldSend, useHeldSends, useHeldSendsUnder } from "../conversation/held-sends";
import type { CarriedBlockIds } from "../conversation/send-request";
import { type AwayDraft, useUnsentDrafts } from "../conversation/unsent-drafts";
import { composerNameOf, keptForOtherWords, stoppedOfferOf } from "../conversation/useKeptDrafts";
import { useProjectArtifact } from "../document/useProjectArtifact";
import { writePin } from "../issue/pin-write";
import { eventItemId } from "../issue/pins";
import { useMarginDrawer } from "../issue-page/margin-slot";
import { parseIssuePath, parseProjectPath } from "../refs/routes";
import { COMPACT_VIEWPORT_QUERY, PHONE_VIEWPORT_QUERY } from "../shell/useDialog";
import type { MarginComposer } from "./CommentsTab";
import type { MarginSheetModel } from "./MarginSheet";
import {
  MARGIN_REPLY_SEND_KEY,
  marginComposeKeptKey,
  marginComposeSendKey,
  marginReplySendKey,
  useMargin,
} from "./margin-context";
import {
  composerOwner,
  type MarginItem,
  type MarginItemAction,
  type MarginTab,
  marginItemBlockId,
  marginItemId,
  marginItemMarkId,
  threadMarkId,
  useMarginItems,
  useMarginOwner,
} from "./useMarginItems";
import { useMarginListeners } from "./useMarginListeners";

/** A retracted ask was taken back by its asker, a withdrawn one by its issue's close, and a
 *  declined one by the person who chose not to answer it: none is a live question or a decision
 *  to read, so the margin folds all three away with the resolved threads until a reader asks for
 *  them. A declined ask among the answered decisions would read as a decision nobody made.
 *  Reopening the issue opens a withdrawn ask again, and an open ask never folds. */
export function isFoldedAsk(ask: Pick<Ask, "state" | "resolution">): boolean {
  const kind = ask.resolution?.kind;
  return (
    ask.state === "resolved" &&
    (kind === "retracted" || kind === "withdrawn" || kind === "declined")
  );
}

/** The margin item a focus request names, once the request's mark has an item. */
function focusedItemFor(
  items: readonly MarginItem[],
  request: { markId: string; seq: number } | undefined
): { itemId: string; seq: number } | undefined {
  if (request === undefined) {
    return undefined;
  }
  const item = items.find((candidate) => marginItemMarkId(candidate) === request.markId);
  return item === undefined ? undefined : { itemId: marginItemId(item), seq: request.seq };
}

/**
 * Everything the margin's one rendered sheet needs: the owner and its route, the items, the
 * selection and composer state, and the listeners that hold a linked card in view. Lifted out of
 * `Margin.tsx` unchanged - `MarginProvider` owns the shared state above the router, this owns
 * what the sheet on screen does with it.
 */
export function useMarginSheet(): MarginSheetModel {
  const {
    blockFilterId,
    blockPlacements,
    cancelCompose,
    clearBlockFilter,
    composes,
    documentBridge,
    focusBlock,
    focusRequest,
    hoveredItemId,
    hoveredMarkId,
    markPlacements,
    placementsReported,
    retypeCompose,
    revealCompose,
    selectItem,
    selectedItemId,
    setHoveredItemId,
    setMarkItemIds,
    shownCompose,
  } = useMargin();
  const store = useHeldSends();
  const queryClient = useQueryClient();
  const { hash, key: locationKey, pathname, search } = useLocation();
  const issueRoute = parseIssuePath(pathname, search);
  const projectRoute = parseProjectPath(pathname, search);
  const documentRoute = projectRoute?.kind === "document" ? projectRoute : undefined;
  const owner = useMarginOwner();
  // The new issue page's Details, where the shell draws that page's frame; undefined elsewhere.
  const drawer = useMarginDrawer();
  const issueKey = owner?.kind === "issue" ? owner.key : undefined;
  const documentArtifact = useProjectArtifact(documentRoute);
  const routeArtifactSlug = issueRoute?.kind === "artifact" ? issueRoute.slug : documentRoute?.slug;
  const queryItem = itemFromSearch(search);
  // In Details the item the address names, and whether the margin is its landing, are the new
  // page's rule (`MarginDrawer.page.routeItem`): an item stands beside the document the address
  // opens - `?mark=`, which "View in document" builds, or an item at a pinned version - only
  // where the address opens one at it. Every other item an issue's address names - by its own
  // route or by `?comment=` and `?ask=` - is a node of the outline, which the page opens on that
  // node: the margin marks its own copy of that item and lands on nothing, since an expanded
  // thread's reply box takes the focus off the node and a hold scrolls the drawer off the entry
  // the address named (R20). Before the page has set its rule the margin
  // names no item. With no drawer an item is landed on as it always was.
  const drawerItem = drawer?.page?.routeItem({ hash, pathname, search });
  const routeItemId =
    drawer !== undefined
      ? drawerItem?.id
      : issueRoute?.kind === "ask" || issueRoute?.kind === "comment"
        ? issueRoute.id
        : (documentRoute?.item?.id ?? queryItem?.id);
  const routeItemBesideDocument = drawer === undefined || drawerItem?.besideDocument === true;
  // Each navigation is its own request for the item it names, even when it names the one the
  // reader is already on: following a link back to the card you moved off has to bring you back.
  // `useLocation().key` changes on a same-URL push, which an item id alone cannot see.
  const routeItemKey = routeItemId === undefined ? undefined : `${locationKey}:${routeItemId}`;

  // The URL names the selection until the reader makes one: the link's item is rendered selected
  // from the first commit, before the asynchronous selection effect has caught the shared margin
  // state up. It never outranks the reader, though - nothing strips `?comment=` from the URL, so
  // reading it first would pin the linked card as selected for the rest of the visit.
  const displayedSelectedItemId = selectedItemId ?? routeItemId;
  const [tab, setTab] = useState<MarginTab>("comments");
  const [expandedOwnerId, setExpandedOwnerId] = useState<string>();
  const [expandedThreadKey, setExpandedThreadKey] = useState<string>();
  const [editingCommentId, setEditingCommentId] = useState<string>();
  const [savingCommentEditId, setSavingCommentEditId] = useState<string>();
  const [sheetThreadKey, setSheetThreadKey] = useState<string>();
  const [showResolved, setShowResolved] = useState(false);
  // Threads whose reply holds a send of its own, out or refused, in the held-send store: each
  // stays in the list it was in (`useMarginItems`'s `held`), whoever resolves it meanwhile.
  const heldReplies = useHeldSendsUnder(MARGIN_REPLY_SEND_KEY);
  const heldReplyKeys = useMemo(
    () =>
      new Set(
        heldReplies.flatMap((held) =>
          held.request.replyTo === null ? [] : [held.request.replyTo.id]
        )
      ),
    [heldReplies]
  );
  // The phone margin thread's reply names its send, so the thread's Back and Escape hold while it
  // is out, until its deadline. No thread has the empty key, so with none open nothing matches.
  const sheetReplyKey = useMemo(() => marginReplySendKey(sheetThreadKey ?? ""), [sheetThreadKey]);
  const { sending: sheetReplySending, sendingNow: sheetReplySendingNow } = useSending(
    sheetReplyKey,
    { untilDeadline: true }
  );
  // Back is the host's move, so the thread's reply stays held for its thread, whatever it holds:
  // typed and never sent (`MentionComposer`'s `inline`), refused, or stopped. Opening the thread
  // again brings it back, and the list's card says it is kept meanwhile (`ThreadCard`).
  const closeThread = useCallback(() => {
    if (sheetReplySendingNow()) return;
    setSheetThreadKey(undefined);
  }, [sheetReplySendingNow]);
  const marginRef = useRef<HTMLElement>(null);
  const issue = useQuery({ ...issueQuery(issueKey), enabled: issueKey !== undefined });
  const viewer = useQuery(whoAmIQuery());
  const visibleArtifact =
    documentRoute === undefined
      ? routeArtifactSlug === undefined
        ? primarySpec(issue.data)
        : artifactAtAddress(issue.data?.artifacts, routeArtifactSlug)
      : documentArtifact.data;
  const visibleArtifactId = visibleArtifact?.id;
  // The compose of the document the margin shows (`composes`): the one the reader has open there
  // and has not sent, if any, and the number the newest compose there opened under. Each document
  // keeps its own, so one open on another document waits there for the reader's return.
  const documentCompose =
    visibleArtifactId === undefined ? undefined : composes.get(visibleArtifactId);
  const pendingCompose = documentCompose?.open;
  // Whether that compose offers a draft beside its field, as its composer shows it
  // (`KeptDrafts.offered`): the send the reader stopped there, set aside for it, or a draft its
  // send name keeps for other words of the document.
  const keptAside = useHeldSend(
    visibleArtifactId === undefined ? undefined : marginComposeKeptKey(visibleArtifactId)
  );
  const unsent = useUnsentDrafts();
  const composeName =
    visibleArtifactId === undefined ? "" : composerNameOf(marginComposeSendKey(visibleArtifactId));
  const keptForWords = unsent.useAway(composeName);
  // What the compose kept for words of the document, which the margin draws while no compose is
  // open there (`MarginKeptDrafts`), and the reader's Discard my draft for one of them.
  const keptDrafts = useMemo(
    () => keptForWords.filter((away) => away.anchor !== undefined),
    [keptForWords]
  );
  const discardKeptDraft = useCallback(
    (away: AwayDraft) =>
      unsent.forgetAway(composeName, { anchor: away.anchor, replyTo: away.replyTo }),
    [composeName, unsent]
  );
  const asideOffered = useMemo(() => stoppedOfferOf(keptAside) !== undefined, [keptAside]);
  const composeOffers =
    pendingCompose !== undefined &&
    (asideOffered || keptForOtherWords(keptForWords, pendingCompose.anchor, null) !== undefined);
  const removePin = useCallback(
    (id: string) => {
      if (owner?.kind === "issue") writePin(queryClient, owner.key, { id, op: "unpin" });
    },
    [owner, queryClient]
  );
  const unpin = useCallback(
    (eventId: number) => removePin(eventItemId({ id: eventId })),
    [removePin]
  );
  // Only the report of the document whose cards this margin shows. A document the route has
  // left can stay registered behind the next route's loading view - Suspense keeps the old page
  // mounted, hidden, until the new page's code has arrived and its throttled reveal commits -
  // and its report would read a press there as the reader working a placed document; an issue's
  // Spec stays registered under the Conversation and withdraws its layout, which is what tells
  // the decisions read that nothing shows them.
  const placementsPublished =
    placementsReported &&
    visibleArtifact !== undefined &&
    documentBridge?.artifactId === visibleArtifact.id;
  const {
    actionFailure,
    answeredAsksPending,
    asksPending,
    commentsError,
    commentsPending,
    decisions,
    editComment,
    items,
    marginItems,
    mutateItem,
    needsYou,
    pendingActionIds,
    openAskCount,
    pinned,
    pinnedConversations,
    pinnedIds,
    resolvedThreads,
    retryAnsweredAsk,
    retryComments,
    retryItem,
    threads,
  } = useMarginItems(owner, tab, visibleArtifact, markPlacements, blockPlacements, blockFilterId, {
    documentShown: placementsPublished,
    held: heldReplyKeys,
    issue: issue.data,
  });
  const onEdit = useCallback(
    async (id: string, body: string, carried: CarriedBlockIds | undefined, bounds: WriteBounds) => {
      setSavingCommentEditId(id);
      try {
        return await editComment(id, body, carried, bounds);
      } finally {
        setSavingCommentEditId(undefined);
      }
    },
    [editComment]
  );

  // A retracted or withdrawn ask is history; it rides the same "show resolved" toggle as
  // resolved comment threads instead of sitting among the answered decisions.
  const askItems = useMemo(
    () =>
      items
        .filter((item): item is Extract<MarginItem, { kind: "ask" }> => item.kind === "ask")
        .map((item) => item.ask),
    [items]
  );
  const foldedAskCount = useMemo(
    () => askItems.filter((ask) => isFoldedAsk(ask)).length,
    [askItems]
  );
  const historicalAsks = useMemo(
    () => askItems.filter((ask) => showResolved || !isFoldedAsk(ask)),
    [askItems, showResolved]
  );
  const markItemIds = useMemo(() => {
    const ids = new Map<string, string>();
    for (const item of marginItems) {
      const markId = marginItemMarkId(item);
      if (markId !== undefined) {
        ids.set(markId, marginItemId(item));
      }
    }
    return ids;
  }, [marginItems]);
  useEffect(() => {
    setMarkItemIds(markItemIds);
    return () => setMarkItemIds(new Map());
  }, [markItemIds, setMarkItemIds]);
  const isClosed =
    owner?.kind === "document" ? false : issue.data !== undefined && issue.data.closed_at !== null;
  const ownerId =
    owner?.kind === "issue" ? owner.key : owner?.kind === "document" ? owner.artifactId : undefined;
  // Every owner opens on Comments; a Pinned selection does not follow the reader to the next issue.
  useEffect(() => {
    if (ownerId !== undefined) {
      setTab("comments");
    }
  }, [ownerId]);
  // Every anchored and resolved thread by key: two callbacks and the sheet all resolve a key,
  // and rebuilding a combined array per call allocated on every mark click and card tap.
  const threadsByKey = useMemo(
    () => new Map([...threads, ...resolvedThreads].map((thread) => [thread.key, thread])),
    [resolvedThreads, threads]
  );
  // A compact viewport has no room beside the document, so anything that puts something new in
  // the margin - a composer, a document item link, a block filter, a focused mark - opens the
  // sheet over it. On a desktop the margin is already on screen and nothing opens.
  const openCompactSheet = useCallback(() => {
    if (ownerId !== undefined && window.matchMedia(COMPACT_VIEWPORT_QUERY).matches) {
      setExpandedOwnerId(ownerId);
    }
  }, [ownerId]);
  const selectedBlockFocus = useRef<string | undefined>(undefined);
  // `openPhoneThread`: a mark click or card tap opens the thread in the phone's dialog; a
  // route-driven selection (a comment or ask deep link) only highlights the card — on a phone
  // the link's destination is the Conversation turn, which a modal dialog would cover.
  const selectMarginItem = useCallback(
    (id: string, openPhoneThread = true) => {
      selectItem(id);
      const thread = threadsByKey.get(id);
      if (thread === undefined) {
        setExpandedThreadKey(undefined);
        return;
      }
      if (thread.resolved) {
        setShowResolved(true);
      }
      if (ownerId !== undefined && window.matchMedia(PHONE_VIEWPORT_QUERY).matches) {
        // A phone thread lives in the Thread dialog, never expanded inline (`onToggleThread`
        // keeps the same rule). A document item URL opens the review panel so the highlighted
        // card is reachable, but leaves the thread collapsed.
        if (openPhoneThread) {
          setExpandedThreadKey(thread.key);
          setExpandedOwnerId(ownerId);
          setSheetThreadKey(thread.key);
        } else if (owner?.kind === "document") {
          setExpandedOwnerId(ownerId);
        }
        return;
      }
      setExpandedThreadKey(thread.key);
    },
    [owner, ownerId, selectItem, threadsByKey]
  );
  const onToggleThread = useCallback(
    (key: string) => {
      const thread = threadsByKey.get(key);
      if (thread === undefined) {
        return;
      }
      selectItem(key);
      if (thread.resolved) {
        setShowResolved(true);
      }
      const blockID = thread.anchor?.block_id;
      if (typeof blockID === "string") {
        selectedBlockFocus.current = undefined;
        focusBlock(blockID);
      }
      if (!thread.anchor?.orphaned) {
        const markId = threadMarkId(thread);
        if (markId !== undefined) {
          documentBridge?.focusMark(markId);
        }
      }
      if (ownerId !== undefined && window.matchMedia(PHONE_VIEWPORT_QUERY).matches) {
        setExpandedOwnerId(ownerId);
        setSheetThreadKey(key);
        return;
      }
      setExpandedThreadKey((current) => (current === key ? undefined : key));
    },
    [documentBridge, focusBlock, ownerId, selectItem, threadsByKey]
  );
  const sheetExpanded = ownerId !== undefined && expandedOwnerId === ownerId;
  const toggleSheet = useCallback(
    (expanded?: boolean) => {
      const nextExpanded = expanded ?? !sheetExpanded;
      setExpandedOwnerId(nextExpanded ? ownerId : undefined);
      if (!nextExpanded) {
        closeThread();
      }
    },
    [closeThread, ownerId, sheetExpanded]
  );
  // The composer's Close, Escape and Discard. The open compose ends unsaved; a refusal the store
  // holds for the document goes, its draft and its mark with it (`MarginProvider` takes the mark).
  // A draft the compose offered beside its field the composer's own Discard has dropped already
  // (`dropOffered`), since Close and Escape ask over it. A send still out is not the reader's to
  // drop, and one that landed has left the store already.
  const closeComposer = useCallback(
    (composer: MarginComposer) => {
      if (composer.seq !== undefined) cancelCompose(composer.anchor.artifact, composer.seq);
      store.discard(marginComposeSendKey(composer.anchor.artifact));
    },
    [cancelCompose, store]
  );
  // Each compose the margin shows - a new one, one a newer selection-bar action had to wait on, or
  // one held for the document the reader came back to - is shown on the Comments tab, and on a
  // compact viewport in the sheet. Once each: the margin keeps the last one shown.
  const handledShownCompose = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (shownCompose === undefined || handledShownCompose.current === shownCompose.seq) return;
    handledShownCompose.current = shownCompose.seq;
    setTab("comments");
    openCompactSheet();
  }, [openCompactSheet, shownCompose]);
  // An unsent suggestion or ask compose ends when its issue closes, which refuses both; a comment
  // compose stays, since a closed issue takes comments. One that offers a draft beside its field
  // stays too, showing it, until the reader drops it: ending it would leave that draft kept with
  // nothing to say so. `isClosed` is the open document's issue, so only the compose on that
  // document is judged by it. Leaving the compose's document is the host's move, which carries
  // nothing and drops nothing: the compose stays open for its document while the reader is on
  // another, and opens there again on the way back (`MarginProvider` holds one per document, so a
  // compose opened on the other document leaves it open). Its composer keeps what the reader
  // wrote for its words under its send name (`useKeptDrafts`'s `awayName`, `AwayDraft.anchor`),
  // so the compose shown when they come back holds it. One that was sent is the held-send store's
  // (`MarginProvider` hands it over at Send) and stays.
  useEffect(() => {
    if (
      documentCompose?.open !== undefined &&
      isClosed &&
      documentCompose.open.kind !== "comment" &&
      !composeOffers
    ) {
      cancelCompose(documentCompose.open.anchor.artifact, documentCompose.seq);
    }
  }, [cancelCompose, composeOffers, documentCompose, isClosed]);
  // Coming back to a document that holds a compose of the margin's shows it: the compose held for
  // the document, or the one still open for it, and a thread whose reply is held, opened as the
  // reader left it. Once per arrival, once the document's threads are in. A send the reader
  // stopped waits aside for the newer compose that took its place (`marginComposeKeptKey`); with
  // no compose open for the document - the tree that set it aside has ended, as a sign-in after a
  // lapse ends it - it goes back under its compose's own name unchanged, at the words and under
  // the key it went out with (`HeldSends.setAside`), so the margin shows it there as before.
  const arrivedAt = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (visibleArtifactId === undefined) {
      arrivedAt.current = undefined;
      return;
    }
    if (arrivedAt.current === visibleArtifactId || commentsPending) return;
    arrivedAt.current = visibleArtifactId;
    const sendKey = marginComposeSendKey(visibleArtifactId);
    const keptKey = marginComposeKeptKey(visibleArtifactId);
    const kept = store.get(keptKey);
    if (pendingCompose === undefined && kept !== undefined && store.get(sendKey) === undefined) {
      store.setAside(keptKey, sendKey);
    }
    if (store.get(sendKey) !== undefined || pendingCompose !== undefined) {
      revealCompose(visibleArtifactId);
    }
    const heldThread = [...heldReplyKeys].find((key) => threadsByKey.has(key));
    if (heldThread !== undefined) selectMarginItem(heldThread);
  }, [
    commentsPending,
    heldReplyKeys,
    pendingCompose,
    revealCompose,
    selectMarginItem,
    store,
    threadsByKey,
    visibleArtifactId,
  ]);
  // The composer on the open document: the one its held send is - out, or refused - from Send
  // on, or the compose the reader has open there. A held send's composer is the compose that sent
  // it, by the number that compose opened under, so Send keeps the composer the reader wrote in.
  // A stopped send set aside with no compose open is found here too, once the arrival above has
  // put it back under its compose's name.
  const heldCompose = useHeldSend(
    visibleArtifactId === undefined ? undefined : marginComposeSendKey(visibleArtifactId)
  );
  const heldRequest = heldCompose?.request;
  const turnedAway =
    shownCompose?.turnedAway === true && shownCompose.artifact === visibleArtifactId;
  const shownComposer = useMemo<MarginComposer | undefined>(() => {
    if (heldRequest?.anchor !== undefined && heldRequest.anchor.node === undefined) {
      return {
        anchor: heldRequest.anchor,
        kind: heldRequest.kind,
        owner: heldRequest.owner,
        seq: documentCompose?.seq,
        turnedAway,
      };
    }
    if (documentCompose?.open === undefined || owner === undefined) return undefined;
    const { anchor, kind } = documentCompose.open;
    return {
      anchor,
      kind,
      owner: composerOwner(owner),
      seq: documentCompose.seq,
      turnedAway: false,
    };
  }, [documentCompose, heldRequest, owner, turnedAway]);
  // A project-document item link has nowhere else to land, so the compact sheet opens on it.
  // An issue's comment or ask deep link lands on its Conversation turn; the margin selects the
  // card without covering that turn with the sheet.
  const documentItemId = documentRoute?.item?.id;
  useEffect(() => {
    if (documentItemId !== undefined) {
      openCompactSheet();
    }
  }, [documentItemId, openCompactSheet]);
  useEffect(() => {
    if (blockFilterId === undefined) {
      return;
    }
    setTab("comments");
    openCompactSheet();
  }, [blockFilterId, openCompactSheet]);

  const handledFocusSequence = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (focusRequest === undefined) {
      handledFocusSequence.current = undefined;
      return;
    }
    if (handledFocusSequence.current === focusRequest.seq) {
      return;
    }
    const item = marginItems.find(
      (candidate) => marginItemMarkId(candidate) === focusRequest.markId
    );
    if (item === undefined) {
      return;
    }
    handledFocusSequence.current = focusRequest.seq;
    selectMarginItem(marginItemId(item));
    setTab("comments");
    openCompactSheet();
  }, [focusRequest, marginItems, openCompactSheet, selectMarginItem]);
  useEffect(() => {
    const selectedItems = [selectedItemId, hoveredItemId]
      .map((itemId) => marginItems.find((candidate) => marginItemId(candidate) === itemId))
      .filter((item): item is MarginItem => item !== undefined);
    const markIds = selectedItems
      .map((item) => marginItemMarkId(item))
      .filter((markId): markId is string => markId !== undefined);
    const blockIds = selectedItems.flatMap((item) => {
      const blockId = marginItemBlockId(item);
      return blockId === undefined ? [] : [blockId];
    });
    documentBridge?.setActiveMarks([...new Set(markIds)]);
    documentBridge?.setActiveBlocks(blockIds);
  }, [documentBridge, hoveredItemId, marginItems, selectedItemId]);

  const onAction = useCallback(
    (id: string, action: MarginItemAction) => {
      mutateItem({ id, kind: action });
    },
    [mutateItem]
  );
  useEffect(() => {
    const item = marginItems.find((candidate) => marginItemId(candidate) === selectedItemId);
    const blockId = item === undefined ? undefined : marginItemBlockId(item);
    if (blockId === undefined) {
      selectedBlockFocus.current = undefined;
      return;
    }
    if (selectedBlockFocus.current === blockId) {
      return;
    }
    selectedBlockFocus.current = blockId;
    focusBlock(blockId);
  }, [focusBlock, marginItems, selectedItemId]);
  const onSelectCard = useCallback(
    (id: string) => {
      selectMarginItem(id);
      const item = marginItems.find((candidate) => marginItemId(candidate) === id);
      const markId = item === undefined ? undefined : marginItemMarkId(item);
      if (markId !== undefined) {
        documentBridge?.focusMark(markId);
      }
    },
    [documentBridge, marginItems, selectMarginItem]
  );
  const focus = focusedItemFor(marginItems, focusRequest);

  // A document item URL is often the first page the reader loads. Its thread arrives after the
  // route effect's first pass, so apply that selection once the margin has the item, and bring
  // the quote the link names into the document's viewport the way a fragment link would - on a
  // phone, where the margin is a sheet over the document, and on a desktop, where the quote can
  // be thousands of pixels below the fold. Once, per link: the URL keeps naming its item for as
  // long as the reader stays on the page, and re-applying it would take the selection back off
  // whatever card they went on to click.
  const appliedRouteItem = useRef<string | undefined>(undefined);
  const focusedRouteMark = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (routeItemId === undefined || routeItemKey === undefined) {
      appliedRouteItem.current = undefined;
      focusedRouteMark.current = undefined;
      return;
    }
    const item = marginItems.find((candidate) => marginItemId(candidate) === routeItemId);
    if (item === undefined) {
      return;
    }
    // The selection is applied once per link; the quote is still brought into view the first
    // time the open document can be asked, which is often a later pass than this one.
    // An item that is a node of the new page's outline is marked and no more
    // (`routeItemBesideDocument`).
    if (appliedRouteItem.current !== routeItemKey) {
      appliedRouteItem.current = routeItemKey;
      if (routeItemBesideDocument) selectMarginItem(routeItemId, false);
      else selectItem(routeItemId);
    }
    const markId = marginItemMarkId(item);
    // Only once the open document has reported that mark. `focusMark` is a one-shot scroll into
    // a span that has to exist: asked while the document is still projecting its marks - which
    // is where a client-side navigation lands - it silently does nothing and the reader never
    // sees the quote. A published placement is the document saying the span is rendered.
    if (
      !routeItemBesideDocument ||
      markId === undefined ||
      !markPlacements.has(markId) ||
      focusedRouteMark.current === `${routeItemKey}:${markId}` ||
      documentBridge === undefined
    ) {
      return;
    }
    focusedRouteMark.current = `${routeItemKey}:${markId}`;
    documentBridge.focusMark(markId);
  }, [
    documentBridge,
    marginItems,
    markPlacements,
    routeItemBesideDocument,
    routeItemId,
    routeItemKey,
    selectItem,
    selectMarginItem,
  ]);
  // The address's landing on a card beside its document, one per navigation, once the margin
  // holds that card: the card is what a reader who followed the link asked to see, so a closed
  // Details opens for it (`Margin.tsx`), as it does for a compose and a picked card. An item that
  // is a node of the new page's outline lands on that node instead and opens nothing.
  const landedCard =
    routeItemBesideDocument &&
    routeItemKey !== undefined &&
    marginItems.some((candidate) => marginItemId(candidate) === routeItemId)
      ? routeItemKey
      : undefined;

  useMarginListeners({
    composer: shownComposer,
    focus,
    margin: marginRef,
    onSelectCard,
    // Only the report of the document whose cards this margin shows. A document the route has
    // left can stay registered behind the next route's loading view - Suspense keeps the old
    // page mounted, hidden, until the new page's code has arrived and its throttled reveal
    // commits - and its report would read a press there as the reader working a placed document.
    placementsPublished,
    routeItemBesideDocument,
    routeItemId,
    routeItemKey,
    setHoveredItemId,
    setTab,
    // Below `xl` the margin's scrollport is laid out only while the compact sheet is expanded;
    // in Details it is laid out while the drawer is open, at every width, so a linked card and
    // an opened composer are held in view there once the drawer opens (R14).
    sheetExpanded: drawer === undefined ? sheetExpanded : drawer.open,
    tab,
    visibleArtifact,
  });

  return {
    actions: {
      onEditingChange: setEditingCommentId,
      closeComposer,
      discardKeptDraft,
      onAction,
      onComposerKindChange: retypeCompose,
      onEdit,
      onUnpin: unpin,
      onUnpinConversation: removePin,
      onRetryAction: retryItem,
      onRetryAnsweredAsk: retryAnsweredAsk,
      onRetryComments: retryComments,
      onRetryIssue: () => void issue.refetch(),
      onToggleResolved: () => setShowResolved((current) => !current),
      onToggleThread,
    },
    composer: shownComposer,
    items: {
      actionFailure,
      answeredAsksPending,
      asksPending,
      commentsError,
      commentsPending,
      decisions,
      historicalAsks,
      keptDrafts,
      marginRef,
      isClosed,
      issueError: issue.isError,
      owner,
      issuePending: issue.isPending,
      needsYou,
      onSelectCard,
      openAskCount,
      pendingActionIds,
      pinned,
      pinnedConversations,
      pinnedIds,
      resolvedThreads,
      foldedAskCount,
      threads,
      viewerLogin: viewer.data?.login ?? "",
      visibleArtifact,
    },
    filter: {
      blockId: blockFilterId,
      clear: clearBlockFilter,
    },
    placement: {
      blockPlacements,
      markPlacements,
    },
    selection: {
      expandedThreadKey,
      editingCommentId,
      savingCommentEditId,
      hoveredItemId,
      hoveredMarkId,
      landedCard,
      routeMarkedItemId: routeItemBesideDocument ? undefined : routeItemId,
      selectedItemId: displayedSelectedItemId,
      showResolved,
    },
    sheet: {
      closeThread,
      expanded: sheetExpanded,
      replySending: sheetReplySending,
      thread: sheetThreadKey === undefined ? undefined : threadsByKey.get(sheetThreadKey),
      toggle: toggleSheet,
    },
    tab: {
      set: setTab,
      value: tab,
    },
  };
}
