import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  lazy,
  type ReactNode,
  type RefObject,
  Suspense,
  startTransition,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";

import { api, isNoIdentity, isUnauthorized } from "./api/client";
import { useEventFeed } from "./api/event-feed";
import { whoAmIQuery } from "./api/queries";
import { askSignInAgain } from "./api/query-client";
import type { AuthenticatedUser } from "./api/types";
import { chunkComponent } from "./components/chunk-component";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { ShellSkeleton } from "./components/ShellSkeleton";
import { useLiveViews } from "./features/agent-view/live-views";
import { LiveViewRoute, MountedLiveViews } from "./features/agent-view/MountedLiveViews";
import { legacyPinnedIds, useAgentPinWrite, usePinnedAgents } from "./features/agents/pins";
import { draftStoresSignedIn, draftStoresSignedOut } from "./features/conversation/unsent-drafts";
import { MachineLoginRoute } from "./features/credentials/MachineLoginRoute";
import { DeferredAskCard } from "./features/doc/DeferredAskCard";
import { ReaderIsHere } from "./features/doc/first-batch";
import { openDocumentEarly } from "./features/doc/open-document-early";
import { DraftsNotKept } from "./features/inbox/DraftsNotKept";
import { InboxList, importInbox } from "./features/inbox/InboxList";
import { useInboxCount } from "./features/inbox/useInboxCount";
import { IssueHeaderSkeleton, IssueTabsSkeleton } from "./features/issue/issue-page-skeleton";
import { issueTabChunks } from "./features/issue/issue-tab-list";
import { useIssuePageChoice } from "./features/issue-page/choice";
import { IconRail } from "./features/issue-page/IconRail";
import {
  type MarginDrawer,
  type MarginDrawerPage,
  MarginSlotProvider,
} from "./features/issue-page/margin-slot";
import { ReferenceNamesProvider } from "./features/issue-page/reference-names";
import {
  type ShellNavigation,
  ShellNavigationContext,
} from "./features/issue-page/shell-navigation";
import { ShortcutHelpOpener } from "./features/issue-page/shortcut-help-opener";
import { MarginColumn, ReservedMarginColumn } from "./features/margin/MarginColumn";
import { MarginProvider } from "./features/margin/margin-context";
import { DEFAULT_MARGIN_WIDTH } from "./features/margin/margin-width";
import { TextAskCard } from "./features/refs/markdown-cards";
import { NO_HOVER_PREVIEW_ATTRIBUTE } from "./features/refs/ref-preview-card";
import {
  AGENT_ARTIFACT_PATH,
  AGENT_LIVE_PATH,
  ANSWERS_PATH,
  buildInboxPath,
  buildProjectPath,
  type IssueRoute,
  issueTabForRoute,
  liveViewSessionOf,
  parseIssuePath,
  parseProjectPath,
  routeFillsViewport,
  routeHasMargin,
  routeIsInbox,
  routeProjectOf,
} from "./features/refs/routes";
import { useRecordRecents } from "./features/search/recents";
import { ConnectionPill } from "./features/shell/ConnectionPill";
import {
  AgentRegistryPoller,
  CreateIssueDialog,
  InboxDrawer,
  RefPreviewHost,
  SearchPalette,
} from "./features/shell/deferred-shell";
import { KeymapProvider } from "./features/shell/KeymapProvider";
import {
  appKeymap,
  type KeyBindingDescription,
  MOD_KEY_LABEL,
  useKeymap,
} from "./features/shell/keymap";
import { rememberShellLayout } from "./features/shell/last-shell-layout";
import { NotFoundPage } from "./features/shell/NotFoundPage";
import { RailButton } from "./features/shell/RailButton";
import { ShellCounts } from "./features/shell/ShellCounts";
import { ShortcutHelp } from "./features/shell/ShortcutHelp";
import {
  SIGN_OUT_SETTLE_MS,
  SignInChannel,
  type UnsentChange,
} from "./features/shell/sign-in-channel";
import {
  COMPACT_VIEWPORT_QUERY,
  MD_VIEWPORT_QUERY,
  READING_PANE_QUERY,
  useDialog,
  useMediaQuery,
} from "./features/shell/useDialog";
import { useDocumentTitle } from "./features/shell/useDocumentTitle";
import { useKeyboardFit } from "./features/shell/useKeyboardFit";
import { legacyPinnedAgentsKey, useUserPreference } from "./features/shell/userPreference";
import { Sidebar } from "./features/sidebar/Sidebar";
import "./features/writes/kinds";
import { KeptWrites } from "./features/writes/KeptWrites";
import { SendStoppedError, stoppedWriteNote } from "./features/writes/keyed-write";
import { PendingWritesIndicator } from "./features/writes/PendingWritesIndicator";
import { useKeyedWrite, useRefusalOfSubject } from "./features/writes/useKeyedWrite";
import { type Write, writesOf } from "./features/writes/write-store";
import { FOOT_STATUS_INSET_PROPERTY, useFootInset } from "./hooks/useFootInset";
import {
  backdrop50,
  borderDefault,
  canvasText,
  card,
  linkHoverText,
  linkText,
  pagePaperBg,
  primaryButtonBg,
  primaryButtonHoverBg,
  railAccentHoverText,
  railAccentText,
  railBg,
  railBorder,
  railDangerText,
  railFocusOverlayBg,
  railFocusOverlayText,
  railHoverBg,
  railMutedText,
  railText,
  skeletonBg,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "./theme/classes";

const importIssuePage = () => import("./features/issue-page/IssuePage");
const importProjectPage = () => import("./features/project/ProjectPage");
const importDocumentPage = () => import("./features/document/DocumentPage");

/**
 * The three pages a reader lands on, each drawn in the commit its chunk arrives in rather than
 * under the route's `Suspense` (`components/chunk-component.tsx` says what that boundary costs
 * a page whose chunk is already on its way). The space each keeps is the skeleton that
 * boundary drew.
 *
 * The issue page's own arrival is committed as a transition, which is what splits its first
 * render into short tasks (`IssuePageInATransition` below says why that render is the one worth
 * splitting). The transition that element opens is over by the time this chunk answers, so the
 * arrival has to say so itself.
 */
const IssuePage = chunkComponent(
  () => importIssuePage().then((module) => module.IssuePage),
  <IssuePageSpace />,
  true
);
const ProjectPage = chunkComponent(
  () => importProjectPage().then((module) => module.ProjectPage),
  <IssuePageFallback />
);
const DocumentPage = chunkComponent(
  () => importDocumentPage().then((module) => module.DocumentPage),
  <IssuePageFallback />
);
const PrioritiesPage = lazy(() =>
  import("./features/priorities/PrioritiesPage").then((module) => ({
    default: module.PrioritiesPage,
  }))
);
const PriorityPage = lazy(() =>
  import("./features/priorities/PriorityPage").then((module) => ({ default: module.PriorityPage }))
);

/**
 * The tab panel an issue address opens, where the page keeps that panel in a chunk of its own
 * (`features/issue/issue-tab-list.ts` holds the one map both this and `IssuePage` read, so a tab
 * added to the page keeps its early request). The Spec and a named artifact open the document
 * surface instead, whose chunk the page preloads with its own (`DOCUMENT_ADDRESS_MODULES` in
 * `web/vite.config.ts`), and a tab with no chunk of its own is drawn from the page's.
 *
 * The artifact a slug names has not been read yet, so a slug that turns out to be the issue's
 * primary document reads here as the Artifacts tab - which has no chunk of its own either way.
 */
function importTheOpenTab(route: IssueRoute): Promise<unknown> | undefined {
  const tab = issueTabForRoute(route, undefined);
  return tab in issueTabChunks ? issueTabChunks[tab as keyof typeof issueTabChunks]() : undefined;
}

/**
 * Requests the chunks of the page this address opens, as this module evaluates - which is while
 * the shell is still asking who is signed in, and well before the route tree that would ask for
 * them renders. A document's page used to wait for that check before its chunk was so much as
 * requested, and then for the chunk before anything of the document was asked for
 * (dispatch://INFRA-341, R11); the chunk needs neither, and a request for it changes nothing
 * about what the page then shows or who may see it.
 *
 * Every page a reader lands on is worth the request, and so is the tab panel an issue address
 * names: a chunk asked for here is usually in hand by the commit that draws its part, so the
 * reader is shown that part rather than the space kept for it. Every other route's chunk is
 * asked for by its own `lazy` as it always was, and so is each of these on a later navigation,
 * where the shell is long since up.
 */
function requestTheOpenPagesChunk(pathname: string): void {
  const issue = parseIssuePath(pathname);
  if (issue !== undefined) {
    void importIssuePage().catch(() => undefined);
    void importTheOpenTab(issue)?.catch(() => undefined);
    return;
  }
  const project = parseProjectPath(pathname);
  if (project?.kind === "document") {
    void importDocumentPage().catch(() => undefined);
    return;
  }
  if (project !== undefined) {
    void importProjectPage().catch(() => undefined);
    return;
  }
  if (routeIsInbox(pathname)) {
    void importInbox().catch(() => undefined);
  }
}

requestTheOpenPagesChunk(window.location.pathname);

const AgentsPage = lazy(() =>
  import("./features/agents/AgentsPage").then((module) => ({ default: module.AgentsPage }))
);

const MachinesPage = lazy(() =>
  import("./features/machines/MachinesPage").then((module) => ({ default: module.MachinesPage }))
);

const MachinePage = lazy(() =>
  import("./features/machines/MachinePage").then((module) => ({ default: module.MachinePage }))
);

const AgentArtifactPage = lazy(() =>
  import("./features/artifacts/AgentArtifactPage").then((module) => ({
    default: module.AgentArtifactPage,
  }))
);

const BroadcastsPage = lazy(() =>
  import("./features/agents/BroadcastsPage").then((module) => ({ default: module.BroadcastsPage }))
);

const BroadcastPage = lazy(() =>
  import("./features/agents/BroadcastPage").then((module) => ({ default: module.BroadcastPage }))
);

const DeliveryPage = lazy(() =>
  import("./features/delivery/DeliveryPage").then((module) => ({ default: module.DeliveryPage }))
);
const PipelinePage = lazy(() =>
  import("./features/delivery/PipelinePage").then((module) => ({ default: module.PipelinePage }))
);

const AnswersPage = lazy(() =>
  import("./features/answers/AnswersPage").then((module) => ({ default: module.AnswersPage }))
);
const SettingsPage = lazy(() =>
  import("./features/settings/SettingsPage").then((module) => ({ default: module.SettingsPage }))
);

function IssuePageFallback(): ReactNode {
  return (
    <div aria-busy="true">
      <IssueHeaderSkeleton />
      <IssueTabsSkeleton />
      <div className="space-y-3">
        {[0, 1, 2, 3].map((row) => (
          <div className={`h-4 w-full animate-pulse rounded ${skeletonBg}`} key={row} />
        ))}
      </div>
    </div>
  );
}

/**
 * The issue page, rendered at transition priority.
 *
 * An issue page is the largest first render the app does, and a document's is the largest of
 * those: the header, the tab strip, the margin's reads and the document surface are one commit,
 * measured at about 0.7 s on a phone at 4x CPU throttling, and nothing a reader does reaches
 * the main thread while it runs (dispatch://INFRA-341, R11). React renders a transition in
 * short pieces, yielding to the browser between them, so the page's own paint and the reader's
 * scroll are not held behind the whole of it.
 *
 * The shell paints `IssuePageSpace` first - the space the page's own chunk keeps until it
 * arrives (`chunkComponent`'s `space`, above), the skeleton the route's `Suspense` fallback
 * draws for the same address - and this element's effect then starts the transition. So the
 * page appears exactly as it does today: the skeleton, then the page, with nothing in between
 * that a reader did not see before.
 *
 * Two transitions carry two renders. This one carries the render that mounts the page's chunk
 * component, and the page's own first render where that chunk is already in hand; where it is
 * not, the arrival lands in a promise callback long after this transition has ended, so it
 * opens one of its own (`chunkComponent`'s `inATransition`, above) and that transition is what
 * splits the page's first render into short tasks.
 */
function IssuePageInATransition(): ReactNode {
  const [renderThePage, setRenderThePage] = useState(false);
  useEffect(() => {
    startTransition(() => setRenderThePage(true));
  }, []);
  return renderThePage ? <IssuePage /> : <IssuePageSpace />;
}

/**
 * The space the issue page keeps until it is drawn: the skeleton the route's own `Suspense`
 * fallback draws for the same address, chosen as the shell chooses its frame (`useIssueFrame`).
 * The new page's frame keeps its own skeleton, which names no control and no tab
 * (`IssueFrameFallback`), and every other reader today's (`IssuePageFallback`), on an address
 * the route `/issues/:key/*` takes and `parseIssuePath` refuses as well: the shell draws no frame
 * there.
 */
function IssuePageSpace(): ReactNode {
  return useIssueFrame() ? <IssueFrameFallback /> : <IssuePageFallback />;
}

/**
 * Whether the shell draws the new issue page's frame (R1, R14): on an issue's address, as
 * `parseIssuePath` reads it, for a reader who chose the new page. Every other page, and every
 * other reader's issue page, keeps the standard shell.
 */
function useIssueFrame(): boolean {
  const [choice] = useIssuePageChoice();
  const { pathname, search } = useLocation();
  return choice === "outline" && parseIssuePath(pathname, search) !== undefined;
}

/**
 * Tells the shell the commit the route the reader asked for is drawn in.
 *
 * It stands inside that route's own `Suspense`, so it is mounted when the boundary's content
 * lands rather than while its fallback stands there, and it draws nothing itself.
 *
 * The shell's own parts wait for it (`SearchPalette`, `RefPreviewHost`,
 * `AgentRegistryPoller`). Each lives in a chunk of its own and none of them is on any route's
 * first frame (`features/shell/deferred-shell.ts`), but mounted beside the route they are
 * boundaries suspended beside it, and React charges a page for that: the first fallback a page
 * commits opens a 300 ms window (`FALLBACK_THROTTLE_MS` in react-dom), and while it is open
 * every render that resolves one suspended boundary with another still suspended is held to
 * the end of it. Measured on this pod, a page load paid one of those waits for parts the
 * reader cannot see (dispatch://INFRA-341, R11). Mounted after the route, they suspend beside
 * nothing, and their chunks are asked for where they compete with nothing either.
 */
function TheRouteIsDrawn({ tell }: { tell: () => void }): null {
  useEffect(tell, [tell]);
  return null;
}

/**
 * What the new issue page's frame holds while the page's code loads (R10):
 * the header's two lines, the view row's five controls and the first rows, as boxes with no word
 * in them, so no name of a control flashes on screen and none of today's nine tab names does.
 * It takes the inset the page's own column takes, since the frame gives `<main>` no padding.
 * `min-w-0` and `overflow-hidden`: the row of boxes neither wraps nor scrolls, so a chunk's load
 * never makes the page wider than a phone's window. Every other reader keeps
 * `IssuePageFallback`.
 */
function IssueFrameFallback(): ReactNode {
  return (
    <div aria-busy="true" className="px-3.5 pt-2 md:px-7">
      <div className={`h-4 w-16 animate-pulse rounded ${skeletonBg}`} />
      <div className={`mt-2 h-7 w-2/3 animate-pulse rounded ${skeletonBg}`} />
      <div
        className={`mt-4 mb-4 flex min-w-0 gap-2 overflow-hidden border-b pb-2 ${borderDefault}`}
        role="toolbar"
      >
        {[0, 1, 2, 3, 4].map((box) => (
          <span className={`h-5 w-16 shrink-0 animate-pulse rounded ${skeletonBg}`} key={box} />
        ))}
      </div>
      <div className="space-y-3">
        {[0, 1, 2, 3].map((row) => (
          <div className={`h-4 w-full animate-pulse rounded ${skeletonBg}`} key={row} />
        ))}
      </div>
    </div>
  );
}

/**
 * The new issue page's icon rail, reading the Inbox count itself, so the shell's own render reads
 * no count: the shell's counts are drawn by parts of their own (`features/shell/ShellCounts.tsx`,
 * dispatch://INFRA-341 R11).
 */
function CountedIconRail(props: Omit<Parameters<typeof IconRail>[0], "inboxCount">): ReactNode {
  return <IconRail {...props} inboxCount={useInboxCount()} />;
}

function ShellMessagePage({ children, title }: { children?: ReactNode; title: string }): ReactNode {
  return (
    <main className={`grid min-h-dvh place-items-center px-6 ${canvasText}`}>
      <section className={`w-full max-w-md rounded-2xl p-8 shadow-sm ${card}`}>
        <p className={`text-sm font-medium tracking-wide uppercase ${textMutedOnSurface}`}>
          Dispatch
        </p>
        <h1 className="mt-3 text-2xl font-semibold">{title}</h1>
        {children}
      </section>
    </main>
  );
}

function InboxPage(): ReactNode {
  useDocumentTitle("Inbox · Dispatch");
  // From the reading pane's width the page fills the viewport (`fillsViewport` below), and the
  // two columns divide the height left under the heading; below it the page scrolls as ever.
  const readingPaneShown = useMediaQuery(READING_PANE_QUERY);
  return (
    <section className={readingPaneShown ? "flex min-h-0 flex-1 flex-col" : undefined}>
      {/* Only the page links to the answers page; the drawer's Inbox stays a peek. */}
      <div className="mb-4 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h1 className="text-[22px] font-semibold tracking-tight">Inbox</h1>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to={ANSWERS_PATH}>
          Answered by you
        </Link>
      </div>
      {/* The heading and its link are this module's own, so they are on the screen while the
          list's chunk is still arriving, as they are while the list's own read is out. */}
      <Suspense fallback={null}>
        <InboxList />
      </Suspense>
    </section>
  );
}

/** The sign-in page. A session that lapsed shows what it keeps for its person (`KeptWrites`), and
 *  its sign-in link asks nothing on leaving: the page comes back to this tab, where every kept
 *  write resumes (`Writes.leavingToSignIn`). */
function SignInPage(): ReactNode {
  const queryClient = useQueryClient();
  return (
    <ShellMessagePage title="Make the next decision clear.">
      <p className={`mt-3 ${textSecondaryOnSurface}`}>
        Sign in to review your team&apos;s open decisions.
      </p>
      <a
        className={`mt-8 inline-flex rounded-lg px-4 py-2 font-semibold ${primaryButtonBg} ${primaryButtonHoverBg}`}
        href="/auth/start"
        onClick={(event) => {
          // A press that opens the link elsewhere leaves this page in place.
          if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
          writesOf(queryClient).leavingToSignIn();
        }}
      >
        Sign in with Google
      </a>
      <KeptWrites />
    </ShellMessagePage>
  );
}

function ConnectionErrorPage({ onRetry }: { onRetry: () => void }): ReactNode {
  return (
    <ShellMessagePage title="Couldn't reach Dispatch.">
      <div className="mt-4" role="alert">
        <button
          className={`text-sm font-medium underline ${linkText} ${linkHoverText}`}
          onClick={onRetry}
          type="button"
        >
          Retry
        </button>
      </div>
    </ShellMessagePage>
  );
}

/** What the rail says of a sign-out that did not land, with its Retry beside it. One Dispatch
 *  refused left the person signed in. One the reader stopped may have ended the session, which the
 *  page cannot know: it says so, and that Retry - a sign-out of its own, which lands on finding
 *  the session ended (`isNoIdentity`) - signs them out either way. Neither discarded a change. */
function signOutFailure(error: Error): string {
  return error instanceof SendStoppedError
    ? stoppedWriteNote("the sign-out", "made", {
        anew: "Every change is still here. Retry ends your session, or finds it ended, and signs you out here.",
      })
    : "The sign-out did not go through. You are still signed in, with every change.";
}

function NavigationContents({
  closeButtonRef,
  compact,
  onClose,
  onCreateIssue,
  onHideSidebar,
  onOpenInboxDrawer,
  onSearch,
  onSignOut,
  reached,
  signOutError,
  signOutPending,
  user,
}: {
  closeButtonRef: RefObject<HTMLButtonElement | null>;
  compact: boolean;
  onClose: () => void;
  onCreateIssue: () => void;
  onHideSidebar: () => void;
  onOpenInboxDrawer: () => void;
  onSearch: () => void;
  onSignOut: () => void;
  reached: boolean;
  /** What the rail says of the last sign-out where it did not land (`signOutFailure`). */
  signOutError: string | undefined;
  signOutPending: boolean;
  user: AuthenticatedUser;
}): ReactNode {
  return (
    <>
      <div className="flex items-center justify-between">
        <Link className="text-lg font-semibold" onClick={onClose} to="/">
          Dispatch
        </Link>
        {compact ? (
          <button
            aria-label="Close navigation"
            className={`rounded-lg px-3 py-2 text-sm font-medium ${railHoverBg}`}
            onClick={onClose}
            ref={closeButtonRef}
            type="button"
          >
            Close
          </button>
        ) : null}
      </div>
      <RailButton
        ariaKeyshortcuts="Control+K Meta+K"
        className="mt-4"
        label="Search"
        onOpen={() => {
          onClose();
          onSearch();
        }}
        shortcut={MOD_KEY_LABEL === "⌘" ? "⌘K" : "Ctrl K"}
      />
      {/* The hint is `AppShell`'s global `create` binding, written as `?` and the palette write
          it (`c`); `aria-keyshortcuts` names the same key in ARIA's uppercase form. */}
      <RailButton
        ariaKeyshortcuts="C"
        className="mt-1"
        label="New issue"
        onOpen={() => {
          onClose();
          onCreateIssue();
        }}
        shortcut="c"
      />
      <Sidebar
        onHide={compact ? undefined : onHideSidebar}
        onNavigate={onClose}
        onOpenInboxDrawer={compact ? undefined : onOpenInboxDrawer}
        reached={reached || compact}
        user={user}
      />
      {/* Identity is chrome: who you are and how to leave are read once, while the navigation
          above is read on every visit, so the footer sits under it rather than over it. */}
      <div className={`mt-auto border-t pt-4 ${railBorder}`}>
        <p className={`px-2 text-sm ${railMutedText}`}>Signed in as {user.login}</p>
        <button
          className={`mt-1 px-2 text-sm font-medium disabled:cursor-not-allowed disabled:opacity-50 ${railAccentText} ${railAccentHoverText}`}
          disabled={signOutPending}
          onClick={onSignOut}
          type="button"
        >
          Sign out
        </button>
        {signOutError === undefined ? null : (
          // A sign-out Dispatch did not answer as done discarded nothing: every write is as it was.
          <p className={`mt-1 px-2 text-sm ${railDangerText}`} role="alert">
            {signOutError}{" "}
            <button className="font-medium underline" onClick={onSignOut} type="button">
              Retry
            </button>
          </p>
        )}
      </div>
    </>
  );
}

/**
 * The providers the shell mounts around everything it draws: the margin's own, and the four the
 * new issue page's frame reads (R12, R14). `drawer` is Details, which only
 * the frame passes: with it undefined the margin is the column it has always been, and no page
 * fills its slot. `navigation` is what the page's header draws its `☰` from, and `openHelp` what
 * its Keys control opens. A page publishes the names it read for its references to everything
 * under the shell, the margin beside the router among it (`ReferenceNamesProvider`).
 */
function ShellProviders({
  children,
  drawer,
  navigation,
  openHelp,
}: {
  children: ReactNode;
  drawer: MarginDrawer | undefined;
  navigation: ShellNavigation | undefined;
  openHelp: () => void;
}): ReactNode {
  return (
    <MarginProvider>
      <ReferenceNamesProvider>
        <MarginSlotProvider drawer={drawer} drawsSlot={drawer !== undefined}>
          <ShellNavigationContext.Provider value={navigation}>
            <ShortcutHelpOpener.Provider value={openHelp}>{children}</ShortcutHelpOpener.Provider>
          </ShellNavigationContext.Provider>
        </MarginSlotProvider>
      </ReferenceNamesProvider>
    </MarginProvider>
  );
}

/**
 * The shell itself, exported for `web/src/__tests__/shell-first-commit.test.tsx`.
 *
 * That row renders this with no effect ever flushed, which is the one way to read the commit
 * the reader's first paint comes from: the margin's column is reserved in that commit wherever
 * the route will draw a margin, and in no other (dispatch://INFRA-341, R11). Nothing else
 * outside this module renders it; the app reaches it through `AuthGate`.
 */
export function AppShell({ user }: { user: AuthenticatedUser }): ReactNode {
  const queryClient = useQueryClient();
  const [navigationOpen, setNavigationOpen] = useState(false);
  const [inboxDrawerOpen, setInboxDrawerOpen] = useState(false);
  // Whether the peek has ever been opened on this load. Once it has, the shell keeps it mounted
  // through every close, so what the reader narrowed it to is there when they open it again.
  const [inboxDrawerEverOpened, setInboxDrawerEverOpened] = useState(false);
  const openTheInboxDrawer = () => {
    setInboxDrawerEverOpened(true);
    setInboxDrawerOpen(true);
  };
  // Whether the bar is open. `$mod+k`, `/`, `g p` and the rail's Search control open the one
  // bar: this page's actions, the person's recent items, the projects a query names and the
  // hits, with a chip saying what it will act on.
  const [barOpen, setBarOpen] = useState(false);
  // What the person opened, for the bar to list back while its box is empty. It is recorded in
  // the shell rather than on each page, so every way into an item - a link, the bar itself, a
  // reload - records it.
  useRecordRecents();
  const [sidebarHidden, setSidebarHidden] = useUserPreference(
    "shell.sidebar",
    (stored) => stored === "hidden",
    (hidden) => (hidden ? "hidden" : "shown")
  );
  const [marginHidden, setMarginHidden] = useUserPreference(
    "shell.margin",
    (stored) => stored === "hidden",
    (hidden) => (hidden ? "hidden" : "shown")
  );
  const [marginWidth, setMarginWidth] = useUserPreference(
    "shell.margin-width",
    (storedWidth) => {
      const parsedWidth = Number(storedWidth);
      return storedWidth !== null && Number.isInteger(parsedWidth)
        ? parsedWidth
        : DEFAULT_MARGIN_WIDTH;
    },
    String
  );
  // The same layout under one key for the whole browser, so the next load's first paint can
  // read it before it knows who is signed in (`features/shell/last-shell-layout.ts`). Here
  // rather than beside each setter: `AppShell` is mounted only for a signed-in reader, so
  // these three are always that reader's own resolved values, and a load that reads them
  // writes them back unchanged - which is what gives a reader who set a column before this
  // existed the benefit of it on their next load.
  useEffect(() => {
    rememberShellLayout(marginHidden, marginWidth, sidebarHidden);
  }, [marginHidden, marginWidth, sidebarHidden]);
  const location = useLocation();
  // The new issue page's frame (`useIssueFrame`), read here once for the whole shell. In it an
  // icon rail of 56 px stands where the sidebar stands, from Tailwind's `md` up, and below that
  // the page's header draws the control that opens the navigation (`ShellNavigationContext`);
  // the margin is Details, a drawer over the page. The rail is drawn by the query that is `md`
  // itself, in `rem`, because the room the page makes for it is `md:pl-14`: a pixel query draws
  // the rail over the page wherever the two disagree, which is every width from 768 to 959 px at
  // a 20 px default font. Every other page, and every other reader's issue page, keeps the shell
  // below exactly as it was.
  const issueFrame = useIssueFrame();
  const railFits = useMediaQuery(MD_VIEWPORT_QUERY);
  const iconRailShown = issueFrame && railFits;
  // Whether Details is open, held for the issue it was opened on: every load opens it closed, and
  // so does every move to another issue. Leaving the issue forgets it in the same render, so
  // coming back to it - by a link or by Back - finds it closed, as a load does.
  const detailsIssue = parseIssuePath(location.pathname)?.key;
  const [detailsFor, setDetailsFor] = useState<string | undefined>(undefined);
  if (detailsFor !== undefined && detailsFor !== detailsIssue) setDetailsFor(undefined);
  // What the new page's root hands Details: its words and its address rule
  // (`features/issue-page/drawer-page.ts`), so that this file imports neither.
  const [drawerPage, setDrawerPage] = useState<MarginDrawerPage | undefined>(undefined);
  const detailsDrawer = useMemo<MarginDrawer | undefined>(
    () =>
      issueFrame
        ? {
            open: detailsFor !== undefined && detailsFor === detailsIssue,
            page: drawerPage,
            setOpen: (open: boolean) => setDetailsFor(open ? detailsIssue : undefined),
            setPage: setDrawerPage,
          }
        : undefined,
    [detailsFor, detailsIssue, drawerPage, issueFrame]
  );
  // The shell reserves a gutter for a rail only where that rail exists. The collapsed-margin
  // rail exists only where the route has a margin, so it is reserved from exactly the answer
  // `features/margin/Margin.tsx` renders from; two answers that disagree leave 80 px of
  // padding beside a rail that is not there. "Full width" means no expanded column on either
  // side - on a route with no margin, a hidden sidebar is enough.
  const hasMargin = routeHasMargin(location.pathname, location.search);
  const marginRailShown = hasMargin && marginHidden;
  const marginColumnShown = hasMargin && !marginHidden;
  const fullWidth = sidebarHidden && !marginColumnShown;
  const mainLayoutClass =
    sidebarHidden && marginRailShown
      ? "xl:w-full xl:pl-20 xl:pr-20"
      : sidebarHidden && !hasMargin
        ? "xl:w-full xl:pl-20"
        : sidebarHidden
          ? "xl:pl-20"
          : marginRailShown
            ? "xl:pr-20"
            : "";
  // A page that scrolls inside itself gets a viewport-tall shell (dynamic viewport units where
  // the browser has them, so a phone's collapsing toolbar is accounted for), with `<main>` a
  // flex column that hands it the height left below the compact header, capped above an iOS
  // on-screen keyboard while its composer has focus. The Inbox is one of them wherever its
  // reading pane shows: each of the pane's two columns scrolls in itself, which it cannot do
  // while the page scrolls as one. Below that width the Inbox scrolls the document as ever.
  const readingPaneShown = useMediaQuery(READING_PANE_QUERY);
  const fillsViewport =
    routeFillsViewport(location.pathname) || (routeIsInbox(location.pathname) && readingPaneShown);
  // Whether the reader has reached for the shell's own parts before the editor's first batch
  // let go of them: the navigation sheet opened, the inbox peeked at, a press or a tab into
  // the sidebar's column. They are drawn then rather than at the batch
  // (`features/doc/AfterTheFirstBatch.tsx`).
  const [shellReached, setShellReached] = useState(false);
  const reachForTheShell = () => setShellReached(true);
  // Whether the route the reader asked for is on the page, which is when the shell's own
  // parts are mounted (`TheRouteIsDrawn`).
  const [theRouteIsDrawn, setTheRouteIsDrawn] = useState(false);
  const tellTheRouteIsDrawn = useCallback(() => setTheRouteIsDrawn(true), []);
  // The session whose live view the address names, or `null` on every other page. The shell
  // mounts that view and the one the person left (`MountedLiveViews`, below `<Routes>`), so it
  // is the one thing about the live view the router still decides.
  const frontLiveView = liveViewSessionOf(location.pathname) ?? null;
  // The person's pins, which the shell owns rather than the rail: the registry keeps a sleeping
  // pinned view and discards a sleeping unpinned one (`live-views.ts`, D8), and the rail is not
  // on the page while the sidebar is collapsed or its sheet is closed.
  const liveViews = useLiveViews();
  const pinnedAgents = usePinnedAgents();
  useEffect(() => {
    liveViews.setPinned(pinnedAgents);
  }, [liveViews, pinnedAgents]);
  // The pins were this browser's until INFRA-812 made them the person's. At the first load
  // after the change each id the old key holds is written as a pin, so this runs once per
  // browser and Dispatch ends up holding the union of what each of the person's browsers had
  // kept.
  //
  // One write at a time, in the order the key kept them, because Dispatch stamps each pin as it
  // arrives and `pinned_at` is the order the rail draws; and the key is removed only once every
  // one of them has landed, since a write Dispatch refuses is the reader's to retry or discard
  // and a key removed before that would be the person's pins gone. A refusal stops the run and
  // leaves the key whole, so the next load writes the lot again, in the same order. The key the
  // run began under is held here rather than read back from storage, because the key outlives
  // the first write now: an effect that runs again - React runs it twice under StrictMode -
  // would otherwise write every id a second time.
  const writePin = useAgentPinWrite();
  const migrated = useRef<string | null>(null);
  useEffect(() => {
    const key = legacyPinnedAgentsKey(user.login);
    if (migrated.current === key) return;
    const kept = window.localStorage.getItem(key);
    if (kept === null) return;
    migrated.current = key;
    void (async () => {
      for (const sessionId of legacyPinnedIds(kept)) {
        if (!(await writePin(sessionId, true))) return;
      }
      window.localStorage.removeItem(key);
    })();
  }, [user.login, writePin]);

  const mainRef = useRef<HTMLElement | null>(null);
  useKeyboardFit(mainRef, fillsViewport);
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  // The status corner floats over the foot of the screen as the composers it sits above do, so
  // it publishes its own reach the same way: a control floating at that foot - the
  // Conversation's `Jump to latest` - rides above the whole corner rather than under the
  // pending-writes indicator or the connection pill. An empty corner has no box, so it publishes
  // nothing and the property is unset.
  const statusCornerRef = useRef<HTMLDivElement>(null);
  useFootInset(statusCornerRef, true, FOOT_STATUS_INSET_PROPERTY);
  const isFirstRender = useRef(true);
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  // In the frame the navigation's sheet is what the rail's foot control and the header's `☰`
  // open over the page at every width, as the Menu control opens it below `xl` on every page.
  const drawer = useDialog<HTMLElement>({
    initialFocusRef: closeButtonRef,
    onClose: () => setNavigationOpen(false),
    open: navigationOpen && (isCompactViewport || issueFrame),
  });
  // What the page's header draws its `☰` from below `md`, where the frame draws neither the
  // shell's bar nor the rail; undefined wherever the shell draws a control of its own.
  const shellNavigation = useMemo<ShellNavigation | undefined>(
    () =>
      issueFrame && !railFits
        ? { onOpen: () => setNavigationOpen(true), open: navigationOpen }
        : undefined,
    [issueFrame, navigationOpen, railFits]
  );
  const navigate = useNavigate();
  // The registry as described when `?` fired (focus still on the caller); `null` while closed.
  const [helpSnapshot, setHelpSnapshot] = useState<readonly KeyBindingDescription[] | null>(null);
  // What a page's own Keys control does (`features/issue-page/shortcut-help-opener.tsx`): what
  // `?` does.
  const openHelp = useCallback(() => setHelpSnapshot(appKeymap.describe()), []);
  const [createOpen, setCreateOpen] = useState(false);
  const routeProject = routeProjectOf(location.pathname);
  // Each binding says what it does to the reader (`effect`, `features/shell/keymap.ts`): every
  // one that opens a dialog, the bar or the drawer, or goes to another page, moves them, and so
  // does `Shift+M` in the new issue page's frame, which opens Details with the focus on its Close
  // and closes it giving the focus back (`Margin.tsx`); the sidebar's toggle and the margin's only
  // redraw the page around what the reader is reading, which the page's keepers hold them
  // through, and move nothing.
  useKeymap("global", [
    {
      // Opens only: while the bar is open the `dialog` scope is the only one consulted, and
      // the bar's own `$mod+k` binding closes it. It fires while the reader is writing, so
      // the key reaches the bar from inside a document's editor too.
      effect: "moves the reader",
      id: "search",
      inEditable: true,
      keys: "$mod+k",
      label: "Search and actions",
      run: () => setBarOpen(true),
    },
    {
      // The same bar. No row: chosen from the bar it would only reopen the bar. Not
      // `inEditable`, so a slash typed into a document stays a slash, and the Agents page
      // shadows it with its own filter box.
      effect: "moves the reader",
      id: "search-only",
      keys: "/",
      label: "Search",
      palette: false,
      run: () => setBarOpen(true),
    },
    {
      effect: "moves the reader",
      id: "help",
      keys: "?",
      label: "Keyboard shortcuts",
      run: () => setHelpSnapshot(appKeymap.describe()),
    },
    {
      effect: "moves the reader",
      id: "create",
      keys: "c",
      label: "Create issue",
      run: () => setCreateOpen(true),
    },
    {
      effect: "moves the reader",
      id: "go-inbox",
      keys: "g i",
      label: "Go to Inbox",
      run: () => navigate("/"),
    },
    {
      effect: "moves the reader",
      id: "go-agents",
      keys: "g a",
      label: "Go to Agents",
      run: () => navigate("/agents"),
    },
    {
      effect: "moves the reader",
      id: "go-settings",
      keys: "g s",
      label: "Go to Settings",
      run: () => navigate("/settings"),
    },
    {
      effect: "moves the reader",
      id: "go-documents",
      keys: "g d",
      label: "Go to Documents",
      run: () => {
        if (routeProject !== undefined) {
          navigate(buildProjectPath({ kind: "documents", project: routeProject }));
        }
      },
      when: () => routeProject !== undefined,
    },
    {
      // The same bar again, where a reader after a project types its name: its projects are
      // rows of the one list. No row, for `/`'s reason.
      effect: "moves the reader",
      id: "go-project",
      keys: "g p",
      label: "Go to project…",
      palette: false,
      run: () => setBarOpen(true),
    },
    {
      effect: "moves the reader",
      id: "inbox-drawer",
      keys: "i",
      label: "Open the inbox",
      run: () => {
        reachForTheShell();
        // Opening from the keyboard mounts the peek like every other way in: the drawer is
        // mounted only once it has been opened (`inboxDrawerEverOpened`, so a reader who never
        // peeks never downloads it), and a bare `setInboxDrawerOpen(true)` here would open a
        // drawer that is not in the page.
        if (inboxDrawerOpen) {
          setInboxDrawerOpen(false);
          return;
        }
        openTheInboxDrawer();
      },
      // The Inbox page already shows everything the drawer would, so a peek over itself is
      // never offered; `g i` still gets the reader there from anywhere else.
      when: () => !routeIsInbox(location.pathname),
    },
    {
      effect: "moves nothing",
      id: "toggle-sidebar",
      keys: "Shift+S",
      label: "Toggle sidebar",
      run: () => setSidebarHidden(!sidebarHidden),
      // Below `xl` the sidebar is a sheet with its own Menu control, and the preference is inert;
      // so it is in the new issue page's frame, whose navigation is its icon rail.
      when: () => !isCompactViewport && !issueFrame,
    },
    // In the new issue page's frame the margin is Details, which `Shift+M` opens and closes at
    // every width; elsewhere it toggles the margin. Each is a binding of its own, whose id
    // follows the gate, so that crossing it registers the binding again and the list of keys
    // reads the label of the shell it is in.
    detailsDrawer === undefined
      ? {
          effect: "moves nothing",
          id: "toggle-margin",
          keys: "Shift+M",
          label: "Toggle margin",
          run: () => setMarginHidden(!marginHidden),
          // Below `xl` the margin is a sheet the route opens itself: `Margin` reads the
          // preference only from `xl`, so a toggle there would change nothing on screen and flip
          // it for later.
          when: () => !isCompactViewport && hasMargin,
        }
      : {
          // Opening Details puts the focus on its Close, and closing it gives the focus back.
          effect: "moves the reader",
          id: "toggle-details",
          keys: "Shift+M",
          label: "Open or close Details",
          run: () => detailsDrawer.setOpen(!detailsDrawer.open),
        },
  ]);
  const login = user.login;
  // Each press signs out the person signed in when it was made, and carries their login: the
  // channel's words of how it ended name that person, and its refusal is theirs alone (below).
  const signOut = useKeyedWrite({
    // A session that lapses while the sign-out is still out has ended as it asked: the sign-out
    // lands, and its landing discards (`Writes.signedIn`).
    endsWithSession: true,
    inline: true,
    keyed: false,
    label: (_signingOut: string) => "Sign out",
    // Refused or stopped, the sign-out discarded nothing: the person is still signed in, and every
    // other tab lets their writes go on, as this one's store does (`Writes.release`).
    onError: (_error, signingOut) => signIns.stillSignedIn(signingOut),
    onSuccess: (_answer, signingOut) => {
      // `resetQueries` (not `clear`) forces the active `["whoami"]` observer in AuthGate to
      // refetch immediately — a signed-out user must land on sign-in without a manual reload.
      void queryClient.resetQueries({ queryKey: ["whoami"] });
      queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== "whoami" });
      // Only now, with Dispatch's answer, are the person's writes discarded, in every tab of this
      // browser: none outlives their session, the next person to sign in here resumes nothing of
      // theirs and sees none of their refusals, and the page stops asking to confirm leaving.
      // Their drafts go with them (`Writes.onSignedOut`, which `AuthGate` listens to).
      writesOf(queryClient).signedOut(signingOut);
      signIns.signedOut(signingOut);
    },
    // A reload never takes the sign-out up again: no request of it goes through `api.send` to be
    // kept.
    method: "POST",
    // Dispatch's own 401 (`NO_IDENTITY`) says the session the sign-out would end is already gone
    // - an earlier attempt ended it and its answer was lost, or a sign-out in another browser
    // revoked it - so the sign-out has done what it was for, whatever the sign-in check can be
    // read to say. A 401 from something in front of Dispatch, an authenticating proxy among
    // them, says nothing of the session: that sign-out did not go through, and is refused with
    // Retry like any other refusal.
    write: () =>
      api
        .logout()
        .catch((error: unknown) =>
          isNoIdentity(error) ? { ok: true as const } : Promise.reject(error)
        ),
  });
  // A sign-out's refusal, and the Retry beside it, are the person's it was pressed for, so they go
  // once someone else is signed in: under their name the refusal would say a false thing, and its
  // Retry would sign them out (`useRefusalOfSubject`). Another person signed in from another tab
  // is drawn a shell of their own (`AuthGate`), which unmounts this one; the check holds the
  // refusal to its person in any render that still draws it for someone else.
  const signOutRefused = useRefusalOfSubject(signOut, (signingOut) => signingOut === login);
  // A page that goes while its sign-out is out says nothing of how it ended, because it does not
  // know: its `{gone}` leaves every other tab to read who is signed in and decide (`holderGone`).
  //
  // Whether the page is asking the other tabs what a sign-out would discard there.
  const [askingTabs, setAskingTabs] = useState(false);
  // Tabs within the same issue manage their own focus (roving tabindex); only a
  // genuine page change — a different issue, or a different top-level route —
  // should move focus to the main region.
  const pageIdentity =
    parseIssuePath(location.pathname)?.key ??
    parseProjectPath(location.pathname)?.project ??
    location.pathname;

  // biome-ignore lint/correctness/useExhaustiveDependencies: re-run only to move focus to main on a real page change
  useEffect(() => {
    if (isFirstRender.current) {
      isFirstRender.current = false;
      return;
    }
    mainRef.current?.focus();
  }, [pageIdentity]);

  const navigation = (
    <NavigationContents
      closeButtonRef={closeButtonRef}
      compact={isCompactViewport || issueFrame}
      onClose={() => setNavigationOpen(false)}
      onCreateIssue={() => setCreateOpen(true)}
      onHideSidebar={() => setSidebarHidden(true)}
      onOpenInboxDrawer={openTheInboxDrawer}
      onSearch={() => setBarOpen(true)}
      onSignOut={() => {
        // Signing out discards the person's own writes in every tab of this browser, so it asks
        // first while any is unsent, naming each once - this tab's, and every other tab's, which
        // it asks for first (`SignInChannel.unsentElsewhere`), by key, since a tab duplicated
        // from another holds the same writes; Cancel keeps them signed in and their writes as
        // they are. Confirmed, nothing of theirs goes out in any tab until Dispatch answers the
        // sign-out (`Writes.holdFor`), and only its landing discards them - a session that lapses
        // under it has ended as the sign-out asked, so that lands too (`endsWithSession`).
        if (askingTabs) return;
        setAskingTabs(true);
        void signIns.unsentElsewhere(login).then((elsewhere) => {
          setAskingTabs(false);
          const unsent = new Map<string, string>();
          for (const { key, label } of [
            ...unsentChanges(writesOf(queryClient).rows(), login),
            ...elsewhere.unsent,
          ]) {
            if (!unsent.has(key)) unsent.set(key, label);
          }
          const ask = unsent.size > 0 || elsewhere.unanswered;
          const question = signOutQuestion([...unsent.values()], elsewhere.unanswered);
          if (ask && !window.confirm(question)) return;
          signIns.signingOut(login);
          signOut.mutate(login);
        });
      }}
      reached={shellReached}
      signOutError={
        signOutRefused && signOut.error !== null ? signOutFailure(signOut.error) : undefined
      }
      signOutPending={signOut.isPending || askingTabs}
      user={user}
    />
  );

  /** What stands in the sidebar's place: the new issue page's icon rail, else the sidebar's
   *  column, its spacer or its collapsed rail. */
  const sidebarSlot = (): ReactNode => {
    if (iconRailShown) {
      return (
        <CountedIconRail
          navigationOpen={navigationOpen}
          onOpenNavigation={() => setNavigationOpen(true)}
          project={routeProject}
          user={user}
        />
      );
    }
    if (issueFrame) return null;
    if (isCompactViewport) {
      // Below `xl` the navigation is the drawer's, and this column holds none of it. It is drawn
      // all the same, hidden until `xl`, so that the frame which already lays the desktop shell
      // out while this render is still the compact one holds the sidebar's width, and `<main>` is
      // laid out at the width it keeps rather than across the sidebar's column. A collapsed
      // sidebar's gutter is `<main>`'s own padding.
      return sidebarHidden ? null : (
        <div aria-hidden="true" className="hidden order-1 w-80 xl:block" />
      );
    }
    if (sidebarHidden) {
      return (
        <aside
          aria-label="Collapsed sidebar"
          className={`fixed inset-y-0 left-0 z-10 hidden w-14 justify-center border-r p-2 xl:flex ${railBorder} ${railBg} ${railText}`}
          data-testid="sidebar-rail"
        >
          <button
            aria-label="Show sidebar"
            className={`min-h-11 min-w-11 rounded-lg text-sm font-medium ${railHoverBg}`}
            onClick={() => setSidebarHidden(false)}
            type="button"
          >
            <span aria-hidden="true">›</span>
          </button>
        </aside>
      );
    }
    // The sidebar is first in the document and as tall as the viewport, so left to itself
    // Chromium's scroll anchoring picks it as the anchor node - and since it never moves, nothing
    // is ever compensated. Excluding it makes the browser anchor inside <main>, so content
    // growing above the reader (a card's thread loading, Markdown rendering) no longer shifts what
    // they are looking at.
    return (
      <aside
        aria-label="Navigation"
        className={`relative hidden order-1 w-80 max-w-none border-r [overflow-anchor:none] xl:block ${railBorder} ${railBg} ${railText}`}
        onFocusCapture={reachForTheShell}
        onPointerDownCapture={reachForTheShell}
      >
        {/* The aside stretches to the page's height, so the navigation is a viewport-tall column
            that sticks inside it: on a long issue the links stay reachable, and the identity
            footer sits at the bottom of the screen rather than the bottom of the document. */}
        <div className="sticky top-0 flex h-dvh flex-col overflow-y-auto p-5">{navigation}</div>
      </aside>
    );
  };

  return (
    <ShellProviders drawer={detailsDrawer} navigation={shellNavigation} openHelp={openHelp}>
      <div
        className={`${fillsViewport ? "flex h-screen flex-col supports-[height:100dvh]:h-dvh xl:flex-row" : "xl:flex"} ${canvasText}`}
        data-testid="app-shell"
      >
        <a
          className={`sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-lg focus:px-4 focus:py-2 ${railFocusOverlayBg} ${railFocusOverlayText}`}
          href="#main-content"
        >
          Skip to content
        </a>
        {/* The shell's status corner: the pending-writes indicator and the word on drafts the tab
            cannot keep for a reload above the connection pill. Above the highest composer fixed
            at the foot of the screen (`--foot-composer-inset`, `useFootInset`), so neither sits
            over its controls; otherwise just above the phone's bottom bar. Only what it shows
            takes the pointer. */}
        <div
          className="pointer-events-none fixed right-4 bottom-[max(5rem,calc(var(--foot-composer-inset,0px)_+_1rem))] z-50 flex w-[min(28rem,calc(100vw-2rem))] flex-col items-end gap-2 xl:bottom-[calc(var(--foot-composer-inset,0px)_+_1rem)] *:pointer-events-auto"
          ref={statusCornerRef}
        >
          <PendingWritesIndicator />
          <DraftsNotKept />
          <ConnectionPill />
        </div>
        {/* Every box below whose place hangs on the 1280 px breakpoint - this bar, the sidebar's
            column and rail, and the margin's (`features/margin/Margin.tsx`) - takes its display
            from an `xl:` class, the clock the shell's own `xl:flex` runs on. The browser applies
            those in the frame a window crosses the breakpoint, while `isCompactViewport` follows
            a task or two later; a box this state placed drew a frame or two of the other layout,
            which squeezed `<main>` to almost nothing crossing upward (dispatch://INFRA-1630). So
            the state chooses what a box holds, and never where the box goes. */}
        {isCompactViewport && !issueFrame ? (
          <header
            className={`flex items-center justify-between border-b px-3 xl:hidden ${railBorder} ${railBg} ${railText}`}
          >
            {/* The title never shrinks and no count breaks its own words: the counts that do not
                fit beside the title wrap onto a second row of the bar, with Menu on the right. */}
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
              <Link className="shrink-0 text-lg font-semibold" to="/">
                Dispatch
              </Link>
              {/* Everywhere but the Inbox page, the inbox peek also opens the Inbox drawer. */}
              <ShellCounts
                onOpenInboxDrawer={() => {
                  reachForTheShell();
                  openTheInboxDrawer();
                }}
                reached={shellReached}
              />
            </div>
            <button
              aria-expanded={navigationOpen}
              aria-label="Open navigation"
              className={`shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${railHoverBg}`}
              onClick={() => {
                reachForTheShell();
                setNavigationOpen(true);
              }}
              type="button"
            >
              Menu
            </button>
          </header>
        ) : null}
        {(isCompactViewport || issueFrame) && navigationOpen ? (
          <>
            <div
              aria-hidden="true"
              className={`fixed inset-0 z-20 ${backdrop50}`}
              onClick={() => setNavigationOpen(false)}
            />
            <aside
              aria-label="Navigation"
              aria-modal="true"
              className={`fixed inset-y-0 left-0 z-30 flex w-80 max-w-[calc(100vw-2rem)] flex-col overflow-y-auto border-b p-5 shadow-2xl ${railBorder} ${railBg} ${railText}`}
              ref={drawer.containerRef}
              role="dialog"
            >
              {navigation}
            </aside>
          </>
        ) : null}
        {sidebarSlot()}
        {/* In the frame `<main>` has no padding of its own: the page's header, its view row, its
            Contents pane and its column each keep the inset the prototype gives them, the pane
            against the rail, which `md:pl-14` makes room for. Its references open no card on
            hover (R6). */}
        <main
          className={
            issueFrame
              ? `min-h-dvh min-w-0 flex-1 outline-none md:pl-14 ${pagePaperBg}`
              : `min-w-0 flex-1 p-6 outline-none xl:order-2 ${fillsViewport ? "flex min-h-0 flex-col" : "pb-32 xl:pb-6"} ${mainLayoutClass}`
          }
          data-shell-layout={issueFrame || fullWidth ? "full-width" : "standard"}
          {...(issueFrame ? { [NO_HOVER_PREVIEW_ATTRIBUTE]: "" } : {})}
          data-testid="main-content"
          id="main-content"
          ref={mainRef}
          tabIndex={-1}
        >
          <ErrorBoundary region="this page" resetKey={location.pathname}>
            <Suspense fallback={issueFrame ? <IssueFrameFallback /> : <IssuePageFallback />}>
              <Routes>
                <Route element={<InboxPage />} path="/" />
                <Route element={<AnswersPage />} path={ANSWERS_PATH} />
                {/* The Replies page is the Inbox's Reply rows now; an old link lands on them. */}
                <Route
                  element={
                    <Navigate replace to={buildInboxPath({ filters: { type: ["reply"] } })} />
                  }
                  path="/replies"
                />
                <Route element={<AgentsPage />} path="/agents" />
                <Route element={<BroadcastsPage />} path="/agents/broadcasts" />
                <Route element={<BroadcastPage />} path="/agents/broadcasts/:id" />
                <Route element={<LiveViewRoute />} path={AGENT_LIVE_PATH} />
                <Route element={<AgentArtifactPage />} path={AGENT_ARTIFACT_PATH} />
                <Route element={<MachinesPage />} path="/machines" />
                <Route element={<MachinePage />} path="/machines/:machine" />
                <Route element={<IssuePageInATransition />} path="/issues/:key/*" />
                <Route element={<PrioritiesPage />} path="/priorities" />
                <Route element={<PriorityPage />} path="/priorities/:id" />
                <Route element={<ProjectPage />} path="/projects/:key" />
                <Route element={<ProjectPage />} path="/projects/:key/architecture" />
                <Route element={<ProjectPage />} path="/projects/:key/issues" />
                <Route element={<ProjectPage />} path="/projects/:key/documents" />
                <Route element={<DocumentPage />} path="/projects/:key/documents/:slug" />
                <Route element={<InboxPage />} path="/credentials/*" />
                <Route element={<MachineLoginRoute />} path="/credentials/machine" />
                <Route element={<SettingsPage />} path="/settings" />
                <Route element={<DeliveryPage />} path="/delivery" />
                <Route element={<PipelinePage />} path="/pipeline" />
                <Route element={<NotFoundPage />} path="*" />
              </Routes>
              <TheRouteIsDrawn tell={tellTheRouteIsDrawn} />
            </Suspense>
          </ErrorBoundary>
          {/* The live views the person has open, inside `<main>` so each takes the layout and
              the iOS keyboard fit the live route already has, and beside the routes rather than
              under them: a view outlives the address that opened it, so neither leaving the
              live route, nor another page's throw, nor a lazy page suspending may unmount it.
              The live route's own element renders nothing. */}
          <MountedLiveViews front={frontLiveView} />
        </main>
        {/* The margin's chunk is the fourth part that waits for the route (`TheRouteIsDrawn`),
            and its column is not: `MarginColumn` suspends, so mounting it before the route is
            drawn would put a suspended boundary beside the route's own and cost the page a
            300 ms fallback throttle, which is the whole reason the parts below wait. The box it
            will fill suspends on nothing, so the shell holds it from its own first frame
            instead - the same `ReservedMarginColumn` the chunk's `Suspense` falls back to, at
            the same width and under the same three answers - and the chunk arrives into a
            column that is already there. Without it `<main>` was laid out across the margin's
            column for the 72 to 149 ms between the shell's first commit and the route's, on
            every margin route measured (dispatch://INFRA-341, R11). */}
        {theRouteIsDrawn ? (
          <ErrorBoundary region="the margin" resetKey={location.pathname}>
            <MarginColumn
              collapsed={marginHidden}
              onCollapsedChange={setMarginHidden}
              onWidthChange={setMarginWidth}
              width={marginWidth}
            />
          </ErrorBoundary>
        ) : (
          <ReservedMarginColumn collapsed={marginHidden} width={marginWidth} />
        )}
        {/* The bar is mounted on every route, closed: the row a close is about to run is run by
            the closed bar's own effect, once the close has put focus back where it was, so
            unmounting it when nothing is open would lose that row. Closed it draws nothing, so
            the frames before its chunk arrives look exactly as the frames after. */}
        {theRouteIsDrawn ? (
          <Suspense fallback={null}>
            <SearchPalette onClose={() => setBarOpen(false)} open={barOpen} />
          </Suspense>
        ) : null}
        {/* Mounted from the reader's first peek on, closed or open: its chunk is asked for then,
            and from then on the filter, the search text and the bands it folded are kept, so
            the peek they open again is the peek they left (dispatch://INFRA-341). */}
        {inboxDrawerEverOpened ? (
          // The reader is in the peek, so every read inside it goes out now rather than
          // waiting for the editor's first batch (`features/doc/first-batch.ts`).
          <ReaderIsHere value={inboxDrawerOpen}>
            <Suspense fallback={null}>
              <InboxDrawer onClose={() => setInboxDrawerOpen(false)} open={inboxDrawerOpen} />
            </Suspense>
          </ReaderIsHere>
        ) : null}
        <ShortcutHelp onClose={() => setHelpSnapshot(null)} snapshot={helpSnapshot} />
        {createOpen ? (
          <Suspense fallback={null}>
            <CreateIssueDialog onClose={() => setCreateOpen(false)} />
          </Suspense>
        ) : null}
        {theRouteIsDrawn ? (
          <>
            <Suspense fallback={null}>
              <RefPreviewHost />
            </Suspense>
            {/* The one owner of the agent registry's refresh cadence. It draws nothing, and
                nothing on the page waits for it: a reader it would have named is named from
                the cache until it lands. */}
            <Suspense fallback={null}>
              <AgentRegistryPoller />
            </Suspense>
          </>
        ) : null}
      </div>
    </ShellProviders>
  );
}

/** The open document's own reads, started from the address the moment the shell is up rather
 *  than when the page that shows the document finally renders (`openDocumentEarly`). It renders
 *  nothing, and is its own component so that a navigation re-runs this effect alone instead of
 *  rendering the shell again. */
function OpenDocumentEarly(): null {
  const queryClient = useQueryClient();
  const { pathname, search } = useLocation();
  useEffect(() => {
    openDocumentEarly(queryClient, pathname, search);
  }, [pathname, queryClient, search]);
  return null;
}

function AuthenticatedApp({ user }: { user: AuthenticatedUser }): ReactNode {
  // Signed in, and before any page's own write, the writes this person's reload interrupted go
  // on under their keys, and their refusals show again (`write-store.ts`): a composer's send, a
  // broadcast, a pin, a read mark and every other write. In an effect, not in the render: a
  // resume sends requests and writes to the store, which a render must not do, and React runs the
  // render twice under StrictMode. The store takes them up once, so a later effect does nothing.
  const queryClient = useQueryClient();
  const login = user.login;
  useEffect(() => {
    writesOf(queryClient).resume(login);
  }, [login, queryClient]);
  useEventFeed();
  // Every Markdown body the shell renders - routes, the margin, the inbox drawer, previews and
  // dialogs - draws a decision block in its text as the read-only card (`TextAskCard`), which is
  // fetched after the first paint (`features/doc/DeferredAskCard.tsx`).
  return (
    <KeymapProvider>
      <OpenDocumentEarly />
      <TextAskCard.Provider value={DeferredAskCard}>
        <AppShell user={user} />
      </TextAskCard.Provider>
    </KeymapProvider>
  );
}

/** Where the tabs of one browser tell each other who is signed in, and what a sign-out in one of
 *  them does to the person's writes in all of them (`SignInChannel`). A page outside a secure
 *  context has no `navigator.locks` (`null` where nothing implements it at all), and holds a
 *  sign-out on its renewal lease instead. */
const signIns = new SignInChannel(
  typeof BroadcastChannel === "undefined" ? undefined : new BroadcastChannel("dispatch-sign-in"),
  typeof window === "undefined" ? undefined : window,
  typeof navigator === "undefined" ? undefined : (navigator.locks ?? undefined)
);

/** What of `login`'s a sign-out would discard here: every write of theirs they can see that
 *  Dispatch has not made - out, waiting its turn, or refused and holding their draft - by its key
 *  and name. A read mark is never shown, and is no change of theirs to lose. */
export function unsentChanges(rows: readonly Write[], login: string): UnsentChange[] {
  return rows
    .filter((row) => row.login === login && row.presentation !== "quiet" && row.status !== "made")
    .map((row) => ({ key: row.key, label: row.label }));
}

/** What signing out asks while `unsent` changes - this tab's and the other tabs' - would be
 *  discarded, naming each, and while a tab that did not answer in time (`unanswered`) may hold
 *  more. */
export function signOutQuestion(unsent: readonly string[], unanswered = false): string {
  const elsewhere = "any unsent changes in your other Dispatch tabs";
  if (unsent.length === 0) return `Sign out and discard ${elsewhere}?`;
  const count = unsent.length === 1 ? "1 unsent change" : `${unsent.length} unsent changes`;
  const what = unanswered ? `${count}, and ${elsewhere}` : count;
  return `Sign out and discard ${what}?\n\n${unsent.map((label) => `• ${label}`).join("\n")}`;
}

/** What becomes of the writes this tab held for another tab's sign-out, once that tab is gone
 *  without having said how the sign-out ended (`SignInListener.holderGone`): Dispatch decides,
 *  since only it knows whether the sign-out landed. This tab reads who is signed in - its own
 *  cookie is that browser session's - and a `401 NO_IDENTITY` is the sign-out landed, so
 *  `login`'s writes go, as the person chose when they confirmed it (`Writes.dropLogin`), rather
 *  than waiting for a sign-in they already discarded them before.
 *
 *  An answer that still names them settles nothing on its own: the gone tab's own request may
 *  have reached Dispatch as that tab died, and the session ends a moment after this read. So the
 *  hold stays on - nothing of theirs goes out while the question is open, which is what the hold
 *  is for - over `SIGN_OUT_SETTLE_MS`, and the check is read again. Only then do their writes go
 *  on, and as an unsettled release (`Writes.releaseUnsettled`), so a lapse that still follows is
 *  that sign-out landing late rather than a lapse. A read that cannot be made is no landing
 *  either, and waits the same way. An answer naming anyone but `login` is the sign-in check's
 *  news too, so it is read again here, and the gate drops their writes as it would any other
 *  login's; one that goes out before that read lands carries their `Dispatch-Login` and is
 *  refused `409 LOGIN_CHANGED` before it runs.
 */
async function heldWritesOfGoneHolder(queryClient: QueryClient, login: string): Promise<void> {
  const writes = writesOf(queryClient);
  // The hold this read decides about. A sign-out of the same person that begins while it reads
  // takes a hold of its own, which this release must not lift (`Writes.heldSignOut`).
  const held = writes.heldSignOut(login);
  const signedInNow = async () =>
    api.whoAmI().then(
      (user) => user.login,
      (error: unknown) => (isNoIdentity(error) ? null : login)
    );
  let who = await signedInNow();
  if (who === login) {
    const settled = Promise.withResolvers<void>();
    setTimeout(settled.resolve, SIGN_OUT_SETTLE_MS);
    await settled.promise;
    who = await signedInNow();
  }
  if (who === null) writes.dropLogin(login);
  else if (who === login) writes.releaseUnsettled(login, held);
  else writes.release(login, held);
  if (who !== login) void askSignInAgain(queryClient);
}

export function AuthGate(): ReactNode {
  const queryClient = useQueryClient();
  const whoAmI = useQuery(whoAmIQuery());
  // A sign-in check that comes back 401 ends this page's session: every read it holds was made
  // for the person who has gone, and the sign-in that follows - in this tab or another - answers
  // with an `event_head` of its own, which the feed resumes from. So the cache is emptied as a
  // sign-out empties it, and the app that mounts next reads everything afresh rather than
  // keeping rows older than its new head.
  const signedOut = whoAmI.isError && isUnauthorized(whoAmI.error);
  useEffect(() => {
    if (!signedOut) return;
    queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== "whoami" });
  }, [queryClient, signedOut]);
  // A sign-out that lands ends the person's session at this browser, whichever way it lands: this
  // tab's, another tab's, or one a lapse lands (`Writes.onSignedOut`). Whoever signs in next here
  // must find nothing they typed, so every draft store forgets their drafts, in the page and in
  // the tab's storage, and takes none until someone signs in, the ones a field hands it as the
  // shell goes among them (`DraftStore.signedOut`). It listens before the effect below tells the
  // write store who is signed in, since a lapse that effect tells of can land a sign-out.
  useEffect(
    () => writesOf(queryClient).onSignedOut((person) => draftStoresSignedOut(queryClient, person)),
    [queryClient]
  );
  // A write belongs to the person who pressed it: once someone else is signed in here - in place,
  // from another tab - every write of another login is dropped and stops trying, and once no one
  // is (the session lapsed), every write stops trying and waits, kept, for the sign-in that
  // follows: the same person's takes them up again (`Writes.signedIn`). Every other tab of this
  // browser is told to read who is signed in again, so it does the same. A draft the person typed
  // and has not sent is theirs too, in every draft store, which outlives the shell as the write
  // store does and keeps each draft in the tab's storage for a reload: another person's sign-in
  // forgets it there too, a lapse keeps it (a sign-out is no lapse, above), and the first sign-in
  // of a page that loaded takes up only the drafts that person kept (`DraftStore.signedIn`).
  const login = signedOut ? undefined : whoAmI.data?.login;
  const known = signedOut || whoAmI.data !== undefined;
  // Whose shell is drawn: only ever the person the stores were last told of, in the commit after
  // they were told. What the person typed into a field on screen is held by the field, not by a
  // store, and the shell would otherwise stay mounted for someone signed in from another tab,
  // showing them those words; so another person's shell is drawn only once the stores have let go
  // of what the person before kept: the one before unmounts the shell and every field in it, and
  // the shell drawn next reads nothing of theirs. The first sign-in of a page waits the same one
  // commit, since that is when the draft stores take up what the tab kept for the person before a
  // reload (`DraftStore.signedIn`): a composer reads its field from the store as it mounts, and
  // React runs this effect after its children's, so a shell drawn sooner could open a composer
  // whose reads are cached on an empty field, before the store holds the draft the tab kept.
  const [shellFor, setShellFor] = useState<string>();
  useEffect(() => {
    if (!known) return;
    writesOf(queryClient).signedIn(login);
    draftStoresSignedIn(queryClient, login);
    signIns.signedIn(login ?? null);
    if (login !== undefined) setShellFor(login);
  }, [known, login, queryClient]);
  useEffect(() => {
    const writes = writesOf(queryClient);
    return signIns.listen({
      changed: () => void askSignInAgain(queryClient),
      holderGone: (gone) => void heldWritesOfGoneHolder(queryClient, gone),
      // The person signed out in another tab: every write they pressed goes here too, its file
      // with it, so no tab holds a write whose file another tab let go of.
      signedOut: (signedOut) => writes.dropLogin(signedOut),
      // Their sign-out there is out: nothing of theirs goes out here until Dispatch answers it.
      signingOut: (signingOut) => writes.holdFor(signingOut),
      stillSignedIn: (still) => writes.release(still),
      unsent: (of) => unsentChanges(writes.rows(), of),
    });
  }, [queryClient]);

  if (whoAmI.isPending) {
    return <ShellSkeleton />;
  }
  // A definitive auth outcome always wins, even over stale "signed in" data from before a
  // session expired or was revoked — otherwise a revoked user keeps the authenticated shell.
  if (signedOut) {
    return <SignInPage />;
  }
  if (whoAmI.data !== undefined) {
    return shellFor === whoAmI.data.login ? (
      <AuthenticatedApp user={whoAmI.data} />
    ) : (
      <ShellSkeleton />
    );
  }
  return <ConnectionErrorPage onRetry={() => void whoAmI.refetch()} />;
}

export function App(): ReactNode {
  return <AuthGate />;
}
