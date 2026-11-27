/**
 * The draft matrix's coverage list, margin thread (INFRA-1503): each composer census item that
 * `draft-matrix.margin.test.tsx` answers for, held to one of its tests or to one of the numbered
 * reasons below, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The host as the matrix file's titles name it. */
const HOST = "margin thread";

/** The numbered reasons, said here alone, which the matrix file's header points to. */
const REASONS = {
  1: "1. A thread's reply always answers its own thread's root, so no move of the host's changes its address, and it is never elsewhere.",
  2: "2. The margin's card draws no collapse of its own: the desktop list closes it only by opening another thread (the second row), and the phone only by Back (the first row).",
  3: "3. The issue picker is an Agents row's (`AgentMessageComposer`), and the margin draws no Agents row's composer.",
  4: "4. The Pinned tab, and above the compact layout the rail the margin collapses to, hide the compose (`MarginComposerSlot`'s `hidden`, `MarginSheet`'s `covered`, `Margin`'s `railShown`, which also opens the rail for each compose the margin shows) and keep it mounted at its address, so nothing it holds moves; a phone thread hides it the same way, the third phone row.",
  5: "5. On a page with no margin above the compact layout `Margin` draws nothing, which unmounts the compose and every card as leaving the document does; below it `Margin` stays drawn there with no document, and so with no compose, which is the shown document's (`useMarginSheet`). The compose is the provider's (`MarginProvider`) and a reply's draft is held under its name, as the document-left rows' and the another-thread row's returns show.",
  6: "6. The Comments tab's list is not drawn on the Pinned tab, under a phone margin thread, or while the comments read failed (`MarginSheet`, `CommentsTab`), which unmounts a thread's reply and its editor as opening another thread does (the second row, and the edit file's margin card).",
  7: "7. A thread's card is drawn per thread of the open document, keyed by it (`ThreadList`): a thread that leaves the list, by its resolution or another document's list, unmounts its card as opening another thread does, and a resolved thread folded away under Show resolved is the same; its reply is held by its thread's name.",
  8: "8. Which end of a margin thread its reply sits at (`composerFirst`, from `useThreadOrder` in `ThreadCard`) is read when the card is drawn and again only when who is signed in changes (`useUserPreference`), so it never moves a reply that is on screen.",
  9: "9. The phone panel's being open or closed is the reader's own (its toggle, its backdrop and Escape, `toggleSheet`) and moves no address: closed, only the stylesheet hides what is in it (`hidden xl:block`), so the compose and every card, reply and editor stay mounted under it, and opening it shows them as they were. Closing it closes a phone thread as Back does (`closeThread`), the first row. The margin opens it for each compose it shows (`openCompactSheet`), which the first two phone rows assert.",
  10: "10. On a phone a card's tap and a mark's click open the thread full-screen in place of opening it in the list (`onToggleThread`, `selectMarginItem`), so the first row's Back is the way off a thread there, and another thread is opened after it; a comment or ask link only selects its card (`openPhoneThread` false), which moves no composer.",
  11: "11. Where the margin sits moves no composer: the desktop column's width, class and resize handle, the compact layout's `contents` class (`Margin`), and the panel's dialog role, its place at the foot of the screen and its backdrop (`MarginSheet`), and the band the hook holds for the collapsed panel for the floats above it (`useFootInset`). Each branch of the hook only measures a box, publishes a CSS property and gives each scroller under a float its end room (an attribute, a custom property or an inline bottom padding), so it moves no composer. That end room reaches the margin's column and Details' drawer. The expanded panel's own height, published for the editor's selection bar (`--margin-sheet-height`), is the same: a box measured into a property.",
  12: "12. The rest is focus and scrolling: the compose takes focus once it is on screen (`onScreen`, `autoFocus`), the margin scrolls the compose or a linked card into view only in an open panel (`useMarginListeners`, `useCardHold`), and an open panel or thread view keeps focus inside it and the page still under it (`useDialog`). None moves an address or ends a mount. The app shell's forms, above every host, are the sign-in file's.",
  13: "13. The compose's kind (`composer.kind`) changes only by the compose's own Comment, Suggest and Ask switch (`onKindChange`), the reader's own control, which retypes the same mark (`retypeCompose`) and leaves the draft in the field (`useKeptDrafts`), so it re-addresses the same compose and moves nothing it holds.",
  14: "14. A render inside the margin that throws draws the shell's error section in its place (`ErrorBoundary`, \"the margin\", in `app.tsx`), which unmounts the compose and every card as leaving the document does. Each composer keeps what it holds as it goes (`useKeptDrafts`): the compose and an editor by their address (`keepAway`), a thread's reply under its thread's name, written at each keystroke (`unsent.write`), and what was typed over a refusal in its held send (`keepDraft`). Those stores live with the query client, above the boundary (`unsentDraftsOf`, `heldSends`), and so does the compose (`MarginProvider`). The boundary draws the margin again when the page's path changes (`resetKey`, `componentDidUpdate`), with no thread open and no editor (`useMarginSheet` starts again), and its Reload Dispatch reloads the page as the reload rows do. A composer drawn again at its address opens on its draft (`mountDraft`): the compose on its document, a thread's reply when its thread is opened, an editor when its comment's Edit is pressed (beside a newer body where the comment changed, `changedUnder`), as the document-left rows, the another-thread row and the edit file's margin card show.",
  15: "15. On the new issue page, once its reader turns it on, the margin is Details (`Margin`'s `drawer`, `DetailsDrawer`): the same `MarginSheet` with the same model, under the same `MarginProvider`, in a drawer over the right of the page, so where it sits - its width, its resize handle, its class, and on a phone its dialog role and its shade - moves no composer, as (11) says of the column and the panel. Closed, Details hides itself (`hidden={!open}`) and keeps the compose and every card, reply and editor mounted under it, as (9) says of the phone panel. Its being open is the reader's own (its Close, its shade, `Shift+M`, and Escape on a phone), or the margin's when a card the reader picked or a link names lands, and on a phone a link followed inside it closes it; none moves an address. Its focus, and its dialog on a phone, are (12)'s.",
  16: "16. In Details the margin's box draws only what the page put in the margin's slot (`SheetBody`, `useMarginSlotFill`), and nothing while the page has put nothing there; and a reader who turns the new page on or off, or leaves the issue page, moves the margin between Details and its column (`Margin`'s `drawer`). Either unmounts the compose and every card as leaving the document does, and each keeps what it holds as (5) and (14) say: its stores and the compose live above the margin, so it comes back as it was when the margin draws it again, as the document-left rows' returns show.",
  17: "17. In Details the page's fill draws the margin's own content - its tabs, the compose and every card - only while the editor of a document the page shows is bound (`DetailsFill`'s `documentUp`: the margin's `documentBridge`, which `ProofDocument`'s page editor registers as it binds and takes back as it unbinds), and hands `IssueMarginCards` no `conversation` otherwise: before that editor binds, once the reader leaves the document, and after the server refuses an editor's connection (`onOutsideSchema`), while it waits to be admitted again or, once it stops trying, until the page is loaded again. Each unmounts the compose and every card, reply and editor as leaving the document does, and each keeps what it holds and comes back on it when an editor binds again, as (16) says.",
  18: "18. A thread whose root its author deleted takes no reply: Dispatch refuses one (409 POST_DELETED), so the card draws its reply only while the root stands (`ThreadCard`'s `rootDeleted`). A delete that lands while the reply is open unmounts it as opening another thread does, and what it held stays under the thread's reply send name, as the closed card says (`keptReply`). The delete is its author's, not a move of the margin's or the reader's, and Dispatch takes no reply to a deleted root, so no row makes it.",
} as const;

/** The rows that end a newer compose offering the earlier compose's draft beside its field. */
const OFFER_CLOSED = `${HOST} · a compose offering an earlier draft, then Close (the margin's compose)`;
const OFFER_ESCAPED = `${HOST} · a compose offering an earlier draft, then Escape (the margin's compose)`;
const OFFER_ISSUE_CLOSED = `${HOST} · a suggestion compose offering an earlier draft, then its issue closed (the margin's compose)`;
const OFFER_LAPSED = `${HOST} · a compose offering an earlier draft, then signed in again after a lapse (the margin's compose)`;

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    "MarginComposerSlot | MarginComposerSlot <MentionComposer mutationKey={sendKey}> | address prop | anchor={composer.anchor}",
    cells(`${HOST} · Close, then Discard draft? (the margin's compose)`),
    cells(OFFER_CLOSED),
    cells(OFFER_ESCAPED),
    cells(`${HOST} · a compose opened on another document, then back (the margin's compose)`),
    cells(
      `${HOST} · phone · a compose opened on another document, then back (the margin's compose)`
    )
  ),
  ...answers(
    "MarginComposerSlot | MarginComposerSlot <MentionComposer mutationKey={sendKey}> | address prop | kind={composer.kind}",
    { reason: REASONS[13] }
  ),
  ...answers(
    "MarginComposerSlot | MarginComposerSlot <MentionComposer mutationKey={sendKey}> | address prop | mutationKey={sendKey}",
    cells(`${HOST} · a compose opened on another document, then back (the margin's compose)`),
    { reason: REASONS[3] }
  ),
  ...answers(
    "MarginComposerSlot | MarginComposerSlot <MentionComposer mutationKey={sendKey}> | address prop | owner={composer.owner}",
    cells(`${HOST} · document left, then back (the margin's compose)`),
    cells(`${HOST} · phone · panel closed, document left, then back (the margin's compose)`)
  ),
  // The send the reader stopped, set aside for the newer compose, which the slot hands that
  // compose to offer (`stoppedAside`): the offer rows' stopped cells.
  ...answers(
    "MarginComposerSlot | MarginComposerSlot <MentionComposer mutationKey={sendKey}> | address prop | stoppedAside={stoppedAside}",
    cells(OFFER_CLOSED, ["stopped"]),
    cells(OFFER_ESCAPED, ["stopped"]),
    cells(OFFER_ISSUE_CLOSED, ["stopped"]),
    cells(OFFER_LAPSED, ["stopped"])
  ),
  ...answers(
    "MarginComposerSlot | MarginComposerSlot <MentionComposer mutationKey={sendKey}> | conditional | <div hidden={hidden}>",
    { reason: REASONS[4] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | address prop | owner={composerOwner(owner)}",
    { reason: REASONS[1] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | (expanded) ? …",
    cells(`${HOST} · another thread opened, then back`),
    { reason: REASONS[2] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | (expanded && rootDeleted === null) ? …",
    cells(`${HOST} · another thread opened, then back`),
    { reason: REASONS[2] },
    { reason: REASONS[18] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | address prop | mutationKey={replyMutationKey}",
    cells(`${HOST} · another thread opened, then back`),
    { reason: REASONS[3] }
  ),
  ...answers(
    'ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | address prop | replyTo={{ actor: root.author, excerpt: "", id: root.id, parentKind: "comment" }}',
    cells(`${HOST} · Cancel reply`),
    cells(`${HOST} · Escape in the field`),
    { reason: REASONS[1] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | (composerFirst) ? …",
    { reason: REASONS[8] }
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | conditional | !(composerFirst) ? …",
    { reason: REASONS[8] }
  ),
  ...answers(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: the key holds the expression's own source text, whose template literal this is.
    'MarginSheet | MarginComposerSlot | key | <MarginComposerSlot key={`${composer.anchor.artifact}:${composer.seq ?? ""}`}>',
    cells(`${HOST} · a compose opened on another document, then back (the margin's compose)`),
    cells(
      `${HOST} · phone · a compose opened on another document, then back (the margin's compose)`
    )
  ),
  ...answers(
    "MarginSheet | MarginComposerSlot | conditional | !(composer === undefined) ? …",
    cells(`${HOST} · document left, then back (the margin's compose)`),
    cells(`${HOST} · phone · panel closed, document left, then back (the margin's compose)`),
    cells(OFFER_ISSUE_CLOSED),
    cells(OFFER_LAPSED),
    cells(
      `${HOST} · a suggestion compose with a typed draft, then its issue closed (the margin's compose)`,
      ["typed"]
    ),
    cells(
      `${HOST} · an ask compose with a typed draft, then its issue closed (the margin's compose)`,
      ["typed"]
    )
  ),
  ...answers(
    "MarginSheet | MarginComposerSlot | conditional | <SheetBody hidden={covered}>",
    cells(`${HOST} · phone · a thread opened over the compose, then Back (the margin's compose)`),
    { reason: REASONS[4] }
  ),
  ...answers(
    "MarginSheet | MarginComposerSlot | address prop | composer={composer}",
    cells(`${HOST} · document left, then back (the margin's compose)`),
    cells(`${HOST} · a compose opened on another document, then back (the margin's compose)`),
    cells(OFFER_LAPSED)
  ),
  ...answers(
    'MarginSheet | MarginComposerSlot | address prop | hidden={tab.value !== "comments"}',
    { reason: REASONS[4] }
  ),
  ...answers(
    "Margin | MarginSheet | conditional | <div hidden={railShown || (!isCompactViewport && !hasMargin)}>",
    { reason: REASONS[4] },
    { reason: REASONS[5] }
  ),
  ...answers(
    "Margin | MarginSheet | early return | if (!isCompactViewport && !hasMargin) return …",
    { reason: REASONS[5] }
  ),
  ...answers("Margin | MarginSheet | address prop | collapsed={railShown}", { reason: REASONS[4] }),
  ...answers(
    "Margin | MarginSheet | address prop | model={model}",
    cells(`${HOST} · document left, then back (the margin's compose)`)
  ),
  ...answers("MarginColumn | Margin | address prop | <Margin {...props}>", { reason: REASONS[5] }),
  ...answers("ErrorBoundary | MarginColumn | conditional | if (message === undefined)", {
    reason: REASONS[14],
  }),
  ...answers(
    "MarginSheet | ThreadCard | conditional | (isPhoneViewport && phoneThread !== undefined && owner !== undefined && visibleArtifact !== undefined) ? …",
    cells(`${HOST} · Back, then back (phone thread view)`),
    { reason: REASONS[10] }
  ),
  ...answers(
    "MarginSheet | ThreadCard | address prop | expanded",
    cells(`${HOST} · Back, then back (phone thread view)`)
  ),
  ...answers("MarginSheet | ThreadCard | address prop | owner={owner}", { reason: REASONS[1] }),
  ...answers(
    "MarginSheet | ThreadCard | address prop | thread={phoneThread}",
    cells(`${HOST} · Back, then back (phone thread view)`)
  ),
  ...answers(
    "MarginSheet | ThreadCard | address prop | replyMutationKey={marginReplySendKey(phoneThread.key)}",
    cells(`${HOST} · Back, then back (phone thread view)`)
  ),
  ...answers("ThreadList | ThreadCard | key | <ThreadCard key={thread.key}>", {
    reason: REASONS[7],
  }),
  ...answers("ThreadList | ThreadCard | key | <div key={thread.key}>", { reason: REASONS[7] }),
  ...answers("ThreadList | ThreadCard | conditional | drawn by anchored.map(…)", {
    reason: REASONS[7],
  }),
  ...answers("ThreadList | ThreadCard | conditional | !(anchored.length === 0) ? …", {
    reason: REASONS[7],
  }),
  ...answers("ThreadList | ThreadCard | conditional | drawn by discussion.map(…)", {
    reason: REASONS[7],
  }),
  ...answers("ThreadList | ThreadCard | conditional | !(discussion.length === 0) ? …", {
    reason: REASONS[7],
  }),
  ...answers("ThreadList | ThreadCard | conditional | drawn by resolvedThreads.map(…)", {
    reason: REASONS[7],
  }),
  ...answers("ThreadList | ThreadCard | conditional | (showResolved) ? …", { reason: REASONS[7] }),
  ...answers(
    "ThreadList | ThreadCard | conditional | !(resolvedThreads.length + foldedAskCount === 0) ? …",
    { reason: REASONS[7] }
  ),
  ...answers(
    "ThreadList | ThreadCard | address prop | expanded={expandedThreadKey === thread.key}",
    cells(`${HOST} · another thread opened, then back`)
  ),
  ...answers("ThreadList | ThreadCard | address prop | owner={owner}", { reason: REASONS[1] }),
  ...answers("ThreadList | ThreadCard | address prop | thread={thread}", { reason: REASONS[7] }),
  ...answers(
    "ThreadList | ThreadCard | address prop | replyMutationKey={marginReplySendKey(thread.key)}",
    cells(`${HOST} · another thread opened, then back`),
    { reason: REASONS[7] }
  ),
  ...answers("CommentsTab | ThreadList | conditional | !(commentsError) ? …", {
    reason: REASONS[6],
  }),
  ...answers(
    "CommentsTab | ThreadList | address prop | expandedThreadKey={expandedThreadKey}",
    cells(`${HOST} · another thread opened, then back`)
  ),
  ...answers("CommentsTab | ThreadList | address prop | foldedAskCount={foldedAskCount}", {
    reason: REASONS[7],
  }),
  ...answers("CommentsTab | ThreadList | address prop | owner={owner}", { reason: REASONS[1] }),
  ...answers("CommentsTab | ThreadList | address prop | resolvedThreads={resolvedThreads}", {
    reason: REASONS[7],
  }),
  ...answers("CommentsTab | ThreadList | address prop | showResolved={showResolved}", {
    reason: REASONS[7],
  }),
  ...answers("CommentsTab | ThreadList | address prop | threads={threads}", { reason: REASONS[7] }),
  ...answers(
    'MarginSheet | CommentsTab | conditional | (tab.value === "comments" && !covered && owner !== undefined && visibleArtifact !== undefined) ? …',
    { reason: REASONS[6] }
  ),
  ...answers("MarginSheet | CommentsTab | conditional | <SheetBody hidden={covered}>", {
    reason: REASONS[4],
  }),
  ...answers("MarginSheet | CommentsTab | address prop | commentsError={commentsError}", {
    reason: REASONS[6],
  }),
  ...answers(
    "MarginSheet | CommentsTab | address prop | expandedThreadKey={selection.expandedThreadKey}",
    cells(`${HOST} · another thread opened, then back`)
  ),
  ...answers("MarginSheet | CommentsTab | address prop | foldedAskCount={foldedAskCount}", {
    reason: REASONS[7],
  }),
  ...answers("MarginSheet | CommentsTab | address prop | owner={owner}", { reason: REASONS[1] }),
  ...answers("MarginSheet | CommentsTab | address prop | resolvedThreads={resolvedThreads}", {
    reason: REASONS[7],
  }),
  ...answers("MarginSheet | CommentsTab | address prop | showResolved={selection.showResolved}", {
    reason: REASONS[7],
  }),
  ...answers("MarginSheet | CommentsTab | address prop | threads={threads}", {
    reason: REASONS[7],
  }),
  ...answers(
    "MarginComposerSlot | MarginComposerSlot <MentionComposer mutationKey={sendKey}> | form | autoFocus={onScreen}",
    { reason: REASONS[12] }
  ),
  ...answers("MarginSheet | MarginComposerSlot | form | <SheetBody className>", {
    reason: REASONS[9],
  }),
  ...answers("MarginSheet | CommentsTab | form | <SheetBody className>", { reason: REASONS[9] }),
  ...answers(
    "MarginSheet | MarginComposerSlot | form | <SheetBody hidden={covered}>",
    cells(`${HOST} · phone · a thread opened over the compose, then Back (the margin's compose)`),
    { reason: REASONS[4] }
  ),
  ...answers(
    "MarginSheet | CommentsTab | form | <SheetBody hidden={covered}>",
    cells(`${HOST} · phone · a thread opened over the compose, then Back (the margin's compose)`),
    { reason: REASONS[4] }
  ),
  ...answers("MarginSheet | MarginComposerSlot | form | <aside className role aria-modal ref>", {
    reason: REASONS[11],
  }),
  ...answers("MarginSheet | CommentsTab | form | <aside className role aria-modal ref>", {
    reason: REASONS[11],
  }),
  ...answers("MarginSheet | ThreadCard | form | <aside className role aria-modal ref>", {
    reason: REASONS[11],
  }),
  ...answers(
    'MarginSheet | MarginComposerSlot | form | onScreen={ !covered && tab.value === "comments" && (drawer ? drawerOpen : sheet.expanded || !isCompactViewport) }',
    { reason: REASONS[12] }
  ),
  ...answers("Margin | MarginSheet | form | <div className>", { reason: REASONS[11] }),
  ...answers(
    "Margin | MarginSheet | form | <div hidden={railShown || (!isCompactViewport && !hasMargin)}>",
    { reason: REASONS[4] }
  ),
  ...answers("Margin | MarginSheet | form | if (!isCompactViewport && !hasMargin) return …", {
    reason: REASONS[5],
  }),
  ...answers("Margin | MarginSheet | form | collapsed={railShown}", { reason: REASONS[4] }),
  ...answers("MarginSheet | ThreadCard | form | <section ref>", { reason: REASONS[12] }),
  ...answers(
    "MarginSheet | ThreadCard | form | (isPhoneViewport && phoneThread !== undefined && owner !== undefined && visibleArtifact !== undefined) ? …",
    { reason: REASONS[10] }
  ),
  ...answers(
    'MarginSheet | CommentsTab | form | (tab.value === "comments" && !covered && owner !== undefined && visibleArtifact !== undefined) ? …',
    { reason: REASONS[6] }
  ),
  ...answers("MarginSheet | - | form | (sheet.expanded && compactSheet) ? …", {
    reason: REASONS[11],
  }),
  ...answers("MarginSheet | - | form | (compactSheet) ? …", { reason: REASONS[11] }),
  ...answers("MarginSheet | - | form | if (!modalSheet || element === null) return …", {
    reason: REASONS[11],
  }),
  ...answers("useFootInset | - | form | if (!active || element === null) return …", {
    reason: REASONS[11],
  }),
  ...answers("MarginSheet | - | form | (covered) ? …", { reason: REASONS[4] }),
  ...answers('MarginSheet | - | form | !covered && tab.value === "comments" && …', {
    reason: REASONS[12],
  }),
  ...answers("Margin | - | form | if (shownCompose !== undefined && railShown)", {
    reason: REASONS[4],
  }),
  ...answers("Margin | - | form | (railShown) ? …", { reason: REASONS[4] }),
  ...answers("Margin | - | form | railShown || …", { reason: REASONS[4] }),
  ...answers("useDialog | - | form | (open) ? …", { reason: REASONS[12] }),
  ...answers("useDialog | - | form | if (!open) return …", { reason: REASONS[12] }),
  ...answers(
    "useMarginSheet | - | form | if (ownerId !== undefined && window.matchMedia(COMPACT_VIEWPORT_QUERY).matches)",
    { reason: REASONS[9] }
  ),
  ...answers(
    "useMarginSheet | - | form | if (ownerId !== undefined && window.matchMedia(PHONE_VIEWPORT_QUERY).matches) return …",
    { reason: REASONS[10] }
  ),
  ...answers("useMarginSheet | - | form | (nextExpanded) ? …", { reason: REASONS[9] }),
  ...answers("useMarginSheet | - | form | if (!nextExpanded)", { reason: REASONS[9] }),
  ...answers("useMarginSheet | - | form | (sheetThreadKey === undefined) ? …", {
    reason: REASONS[10],
  }),
  ...answers("useMarginListeners | - | form | if (isCompactViewport && !sheetExpanded) return …", {
    reason: REASONS[12],
  }),
  ...answers("useCardHold | - | form | if (isCompactViewport && !sheetExpanded) return …", {
    reason: REASONS[12],
  }),
  ...answers(
    "MarginComposerSlot | MarginComposerSlot <MentionComposer mutationKey={sendKey}> | lifetime | <MentionComposer mutationKey={sendKey}>",
    cells(`${HOST} · the page reloaded, then back (the margin's compose)`)
  ),
  ...answers(
    "ThreadCard | ThreadCard <MentionComposer mutationKey={replyMutationKey}> | lifetime | <MentionComposer mutationKey={replyMutationKey}>",
    cells(`${HOST} · the page reloaded, then back`)
  ),
  // The new issue page's Details (#4189): the margin in a drawer, and the box that draws the
  // page's fill there.
  ...answers("SheetBody | MarginComposerSlot | conditional | <div hidden={hidden}>", {
    reason: REASONS[4],
  }),
  ...answers("SheetBody | MarginComposerSlot | conditional | if (!drawer)", {
    reason: REASONS[16],
  }),
  ...answers("SheetBody | MarginComposerSlot | conditional | !(fill === undefined) ? …", {
    reason: REASONS[16],
  }),
  ...answers("SheetBody | MarginComposerSlot | early return | if (!drawer) return …", {
    reason: REASONS[16],
  }),
  ...answers("SheetBody | MarginComposerSlot | form | <div hidden={hidden}>", {
    reason: REASONS[4],
  }),
  ...answers("SheetBody | CommentsTab | conditional | <div hidden={hidden}>", {
    reason: REASONS[4],
  }),
  ...answers("SheetBody | CommentsTab | conditional | if (!drawer)", { reason: REASONS[16] }),
  ...answers("SheetBody | CommentsTab | conditional | !(fill === undefined) ? …", {
    reason: REASONS[16],
  }),
  ...answers("SheetBody | CommentsTab | early return | if (!drawer) return …", {
    reason: REASONS[16],
  }),
  ...answers("SheetBody | CommentsTab | form | <div hidden={hidden}>", { reason: REASONS[4] }),
  ...answers("Margin | MarginSheet | early return | if (drawer !== undefined) return …", {
    reason: REASONS[16],
  }),
  ...answers("Margin | DetailsDrawer | conditional | if (drawer !== undefined)", {
    reason: REASONS[16],
  }),
  ...answers("Margin | DetailsDrawer | address prop | drawer={drawer}", { reason: REASONS[15] }),
  ...answers("Margin | DetailsDrawer | address prop | model={model}", { reason: REASONS[15] }),
  ...answers("DetailsDrawer | MarginSheet | conditional | <aside hidden={!open}>", {
    reason: REASONS[15],
  }),
  ...answers("DetailsDrawer | MarginSheet | address prop | model={model}", { reason: REASONS[15] }),
  ...answers("DetailsDrawer | MarginSheet | form | <aside className role aria-modal ref>", {
    reason: REASONS[15],
  }),
  ...answers("Margin | DetailsDrawer | form | model={model}", { reason: REASONS[15] }),
  ...answers("DetailsDrawer | - | form | (open && isPhoneViewport) ? …", {
    reason: REASONS[15],
  }),
  ...answers("DetailsDrawer | - | form | if (!open || isPhoneViewport) return …", {
    reason: REASONS[15],
  }),
  ...answers(
    "DetailsDrawer | - | form | if (drawerBox === null || !open || !isPhoneViewport) return …",
    { reason: REASONS[15] }
  ),
  ...answers("DetailsDrawer | - | form | (isPhoneViewport) ? …", { reason: REASONS[15] }),
  // The page's fill of the margin's slot (`DetailsFill`), which draws the margin's content inside
  // Details.
  ...answers("DetailsFill | MarginComposerSlot | conditional | (documentUp) ? …", {
    reason: REASONS[17],
  }),
  ...answers("DetailsFill | CommentsTab | conditional | (documentUp) ? …", {
    reason: REASONS[17],
  }),
];
