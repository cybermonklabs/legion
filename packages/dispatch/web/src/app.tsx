import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ReactNode,
  type RefObject,
  Suspense,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";
import { Link, Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";

import { api, isNoIdentity, isUnauthorized } from "./api/client";
import { useEventFeed } from "./api/event-feed";
import { useLiveSocketPill } from "./api/live-socket";
import { landingReadOf, userAgentStateQuery, whoAmIQuery } from "./api/queries";
import { askSignInAgain } from "./api/query-client";
import type { AuthenticatedUser } from "./api/types";
import { useLiveViews } from "./features/agent-view/live-views";
import { LiveViewRoute, MountedLiveViews } from "./features/agent-view/MountedLiveViews";
import { legacyPinnedIds, useAgentPinWrite, usePinnedAgents } from "./features/agents/pins";
import { totalUnreadReplies, unreadRepliesLabel } from "./features/agents/unread";
import { draftStoresSignedIn, draftStoresSignedOut } from "./features/conversation/unsent-drafts";
import { MachineLoginRoute } from "./features/credentials/MachineLoginRoute";
import { loadEditorLibrary, loadTransportModules } from "./features/doc/document-chunks";
import { TextAskCardHost } from "./features/doc/TextAskCardHost";
import { DraftsNotKept } from "./features/inbox/DraftsNotKept";
import { InboxDrawerHost, InboxHost, loadInboxChunk } from "./features/inbox/InboxHost";
import { useInboxCount } from "./features/inbox/useInboxCount";
import { CreateIssueDialogHost } from "./features/issue/CreateIssueDialogHost";
import { ISSUE_TAB_LABELS } from "./features/issue/issue-tab-list";
import { issueTabChunk } from "./features/issue/tab-chunks";
import { frameShellChunk } from "./features/issue-page/frame-chunk";
import type { FrameProviders, ShellFrame } from "./features/issue-page/frame-shell";
import {
  outlineChosenInThisBrowser,
  useStoredIssuePageChoice,
} from "./features/issue-page/stored-choice";
import { MarginColumn } from "./features/margin/MarginColumn";
import { MarginProvider } from "./features/margin/margin-context";
import { DEFAULT_MARGIN_WIDTH } from "./features/margin/margin-width";
import { AgentRegistryPoller } from "./features/refs/agent-registry";
import { TextAskCard } from "./features/refs/markdown-cards";
import { RefPreviewLayer } from "./features/refs/RefPreviewLayer";
import {
  AGENT_ARTIFACT_PATH,
  AGENT_LIVE_PATH,
  ANSWERS_PATH,
  buildInboxPath,
  buildProjectPath,
  type IssueRoute,
  type IssueTab,
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
import { SearchPaletteHost } from "./features/search/SearchPaletteHost";
import { chunkComponent } from "./features/shell/chunk-component";
import { lazyChunk, prefetchChunk } from "./features/shell/chunk-loads";
import { holdUntilContentDraws } from "./features/shell/content-drawn";
import { ErrorBoundary } from "./features/shell/ErrorBoundary";
import { KeymapProvider } from "./features/shell/KeymapProvider";
import {
  appKeymap,
  type KeyBindingDescription,
  MOD_KEY_LABEL,
  useKeymap,
} from "./features/shell/keymap";
import { NotFoundPage } from "./features/shell/NotFoundPage";
import { RailButton } from "./features/shell/RailButton";
import { ShortcutHelpHost } from "./features/shell/ShortcutHelpHost";
import {
  SIGN_OUT_SETTLE_MS,
  SignInChannel,
  type UnsentChange,
} from "./features/shell/sign-in-channel";
import {
  COMPACT_VIEWPORT_QUERY,
  READING_PANE_QUERY,
  useDialog,
  useMediaQuery,
} from "./features/shell/useDialog";
import { useDocumentTitle } from "./features/shell/useDocumentTitle";
import { useKeyboardFit } from "./features/shell/useKeyboardFit";
import { legacyPinnedAgentsKey, useUserPreference } from "./features/shell/userPreference";
import { Sidebar } from "./features/sidebar/Sidebar";
import { SendStoppedError, stoppedWriteNote } from "./features/writes/keyed-write";
import { useKeyedWrite, useRefusalOfSubject } from "./features/writes/useKeyedWrite";
import { KeptWritesHost, loadWriteKinds, PendingWritesHost } from "./features/writes/write-hosts";
import { type Write, writesOf } from "./features/writes/write-store";
import { FOOT_STATUS_INSET_PROPERTY, useFootInset } from "./hooks/useFootInset";
import {
  backdrop50,
  badgeBlocking,
  borderDefault,
  borderStrong,
  borderTransparent,
  calloutDangerBg,
  calloutDangerBorder,
  calloutWarningBorder,
  canvasText,
  card,
  linkHoverText,
  linkText,
  primaryButtonBg,
  primaryButtonHoverBg,
  railAccentHoverText,
  railAccentText,
  railActiveBg,
  railBg,
  railBorder,
  railDangerText,
  railFocusOverlayBg,
  railFocusOverlayText,
  railHoverBg,
  railMutedText,
  railNeedsYouBadgeBg,
  railNeedsYouBadgeText,
  railText,
  skeletonBg,
  statusConnecting,
  textMutedOnSurface,
  textSecondaryOnSurface,
  textTransparent,
} from "./theme/classes";

/** The three route chunks a cold landing can need, each named once: the router's `lazyChunk`
 *  below and the entry's preload (`preloadRouteChunk`) call the same import, so the chunk the
 *  entry starts cannot be another than the one the router renders. The Inbox's chunk is the
 *  fourth, started wherever the Inbox is the page rather than the peek, and its import is named
 *  once in the same way, in the module that owns it (`features/inbox/InboxHost.tsx`,
 *  `loadInboxChunk`). */
const loadIssuePage = () =>
  import("./features/issue-page/IssuePage").then((module) => ({ default: module.IssuePage }));
const loadProjectPage = () =>
  import("./features/project/ProjectPage").then((module) => ({ default: module.ProjectPage }));
const loadDocumentPage = () =>
  import("./features/document/DocumentPage").then((module) => ({ default: module.DocumentPage }));

// The three the entry can prefetch, through `lazyChunk` like every route of this file: the
// router's ask is under `renderChunk`, which is what tells the chunk-failure recovery that a
// reader is waiting on this download rather than the entry having prefetched it
// (`features/shell/DeploymentResilience.tsx`). These three are only the ones named twice, here
// and in the preload.
const IssuePage = lazyChunk(loadIssuePage);
const ProjectPage = lazyChunk(loadProjectPage);
const DocumentPage = lazyChunk(loadDocumentPage);

/** The chunks the live document pulls as it renders, which the route chunk does not carry: the
 *  editor's own library and the Yjs transport it connects over, each the list
 *  `features/doc/document-chunks.ts` names for `ProofDocument`'s two modules (`editor.ts`,
 *  `connection.ts`), started here rather than named again, so the preload and the render cannot
 *  pull different chunks. This module imports that list directly, so the chunks are asked for
 *  at once, beside the landing read, and not after the two modules' own chunks have come down
 *  and run. A landing that draws a document always reaches both, so this fetches no byte such a
 *  landing does not fetch anyway. */
const loadDocumentChunks = () => Promise.all([loadEditorLibrary(), loadTransportModules()]);

/** The issue tabs whose panel draws a live document, and so the tabs that reach the chunks
 *  above: the Spec, which is the tab a bare `/issues/<key>` opens on, and the two role
 *  documents, Verification and Plan - each panel renders `ArtifactDocument`
 *  (`features/issue/IssuePage.tsx`). The Conversation, Children, Activity, Decisions and
 *  Collaborators panels render none, and a reader who opens one of them must download no
 *  editor and no transport at all (`e2e/doc.e2e.ts`, "the Conversation tab downloads no editor
 *  transport; the Spec tab loads it on demand"). */
const DOCUMENT_TABS: readonly IssueTab[] = ["spec", "verification", "plan"];

/** Whether the issue page at this address draws a live document, read from the address by the
 *  router's own parse of it (`features/refs/routes.ts`, `issueTabForRoute`) rather than by a
 *  second rule kept here, which could say one thing while the page drew another.
 *
 *  An address naming an artifact - `/issues/<key>/artifact/<slug>` - draws that artifact as a
 *  document when it is one, and whether it is one is not known until the issue has been read.
 *  `issueTabForRoute` reads such an address as the Artifacts tab while the artifact is unknown,
 *  so it is taken here by its own kind instead: the chunks come down for every named artifact,
 *  which is what the page needs for the documents among them and a prefetch the others do not
 *  use. */
function issueDrawsDocument(route: IssueRoute): boolean {
  return route.kind === "artifact" || DOCUMENT_TABS.includes(issueTabForRoute(route, undefined));
}

/**
 * Starts the chunks the address needs, beside the landing read rather than after it.
 *
 * `AuthGate` holds the whole tree behind the landing read, so a chunk is asked for only once
 * that read has answered and the shell has rendered. The page's own reads then wait for those
 * chunks to arrive and parse - the live editor's admission read of the document's text most of
 * all (`features/doc/ProofDocument.tsx`) - which puts them in a wave of their own, after the
 * reads that needed no chunk. Started here, they come down while the landing read is out, and
 * the page's reads go out in the wave that follows the landing's answer.
 *
 * The address is read exactly as the landing read reads it (`landingReadOf`), so one parse
 * decides which page is coming. The document chunks go to the addresses whose page draws a
 * document and to no others: a project's document page always draws one, and an issue page
 * draws one on the tabs `issueDrawsDocument` names and on no other, so the Conversation of an
 * issue downloads no editor. The board draws no document and takes neither chunk, and an
 * address that is no page of a cold landing starts no page's chunk (the new issue page's shell,
 * below, is no page's). The Inbox's own chunk holds to the same rule - a chunk is started where
 * the landed page draws it - and needs a second question to hold it: the landing read takes
 * every address that is neither an issue nor a project as the Inbox, while the Inbox is the page
 * at the addresses `InboxPage` is mounted at and a peek drawer over every other, so the arm below
 * asks `routeIsInbox` as well. A chunk that fails here is left to the router: `lazyChunk` asks
 * for the same import again, and that failure - a reader in front of a page that cannot draw - is
 * the one `DeploymentResilience` reloads for (`prefetchChunk`).
 *
 * `pathname` is the caller's and has no default: the one call site is `main.tsx`, which passes
 * `window.location.pathname` - the address the browser is loading, read before React renders
 * anything - and the landing read beside it defaults to that same address of its own accord
 * (`readLanding`).
 */
export function preloadRouteChunk(pathname: string): void {
  // A reader who turned the new issue page on draws the whole shell, on every page, through a
  // chunk of its own (`features/issue-page/frame-chunk.ts`), so it is started here on every
  // address, beside the landing read, for the reason the page's chunk is. This runs before the
  // sign-in check has answered, so the login whose choice the shell will read is not known yet:
  // any login's choice stored in this browser that reads `outline` starts it. A browser where
  // nobody chose the new page fetches none of it.
  if (outlineChosenInThisBrowser()) prefetchChunk(frameShellChunk.load);
  switch (landingReadOf(pathname).page) {
    case "issue": {
      prefetchChunk(loadIssuePage);
      // The tab decides the document chunks, not the issue: see `issueDrawsDocument`. The parse
      // answers for every address this arm is reached by, since that is the arm's own test.
      const route = parseIssuePath(pathname);
      if (route !== undefined && issueDrawsDocument(route)) {
        prefetchChunk(loadDocumentChunks);
        // The page's content is its document, so the shell's code that draws none of it waits
        // for the document to draw (`features/shell/content-drawn.ts`).
        holdUntilContentDraws();
      }
      // Every tab but the Spec draws from a chunk of its own (`features/issue/tab-chunks.ts`).
      // A landing on such a tab starts that chunk here, beside the landing read, for the same
      // reason the page's chunk is started here: else it would come down only once the page
      // has rendered, a wave after the reads that needed no chunk. A Spec landing starts none.
      // An address that names an artifact starts none either: the page draws the issue's
      // primary document there as the Spec, and the address alone does not say whether the
      // artifact is that one (`issueDrawsDocument`). Any other artifact's Artifacts tab gets its
      // chunk when the page renders it.
      const tabChunk =
        route === undefined || route.kind === "artifact"
          ? undefined
          : issueTabChunk(issueTabForRoute(route, undefined));
      if (tabChunk !== undefined) prefetchChunk(tabChunk);
      break;
    }
    case "document":
      prefetchChunk(loadDocumentPage);
      prefetchChunk(loadDocumentChunks);
      holdUntilContentDraws();
      break;
    case "board":
      prefetchChunk(loadProjectPage);
      break;
    case "inbox":
      // The Inbox is a chunk of its own now (`features/inbox/InboxHost.tsx`), and this arm is
      // every address that is neither an issue nor a project, so the chunk is started only
      // where the page draws the list (`routeIsInbox`, which is the router's own ranking of
      // `InboxPage`'s routes): beside the landing read rather than after it, which is what
      // keeps the list's own reads in the wave that follows the landing's answer. Where the
      // Inbox is the peek drawer instead, this is not the chunk the reader is waiting for, so
      // the shell starts it after that page's first paint rather than beside its landing read
      // (`features/inbox/InboxHost.tsx`, `usePeekChunkAfterFirstPaint`).
      if (routeIsInbox(pathname)) prefetchChunk(loadInboxChunk);
      break;
  }
}

// Every other route of the shell, each through `lazyChunk` as the three above are, and for the
// same reason: a reader is waiting on this download the moment the router renders the route, so
// its failure is one the chunk-failure recovery must reload for. Only a chunk nothing is waiting
// on may be passed over, which is what every `prefetchChunk` call is: the preload above, the
// Inbox peek's chunk after a page's first paint (`features/inbox/InboxHost.tsx`), the search
// bar's after the same paint (`features/search/SearchPaletteHost.tsx`), the two lists of writes'
// and the kinds of write's once the page has drawn (`features/writes/write-hosts.tsx`) and the
// boot warm of the Markdown engine (`features/shell/DeploymentResilience.tsx`). Left bare, a
// route chunk that failed while one of those prefetches was still out would be read as that
// prefetch's and reload nothing, leaving the reader in front of a page that cannot draw
// (dispatch://INFRA-1109); `lazy` itself is refused here by the repository's `biome.json`.

const PrioritiesPage = lazyChunk(() =>
  import("./features/priorities/PrioritiesPage").then((module) => ({
    default: module.PrioritiesPage,
  }))
);

const PriorityPage = lazyChunk(() =>
  import("./features/priorities/PriorityPage").then((module) => ({
    default: module.PriorityPage,
  }))
);

const AgentsPage = lazyChunk(() =>
  import("./features/agents/AgentsPage").then((module) => ({ default: module.AgentsPage }))
);

const MachinesPage = lazyChunk(() =>
  import("./features/machines/MachinesPage").then((module) => ({ default: module.MachinesPage }))
);

const MachinePage = lazyChunk(() =>
  import("./features/machines/MachinePage").then((module) => ({ default: module.MachinePage }))
);

const AgentArtifactPage = lazyChunk(() =>
  import("./features/artifacts/AgentArtifactPage").then((module) => ({
    default: module.AgentArtifactPage,
  }))
);

const BroadcastsPage = lazyChunk(() =>
  import("./features/agents/BroadcastsPage").then((module) => ({
    default: module.BroadcastsPage,
  }))
);

const BroadcastPage = lazyChunk(() =>
  import("./features/agents/BroadcastPage").then((module) => ({ default: module.BroadcastPage }))
);

const DeliveryPage = lazyChunk(() =>
  import("./features/delivery/DeliveryPage").then((module) => ({ default: module.DeliveryPage }))
);
const PipelinePage = lazyChunk(() =>
  import("./features/delivery/PipelinePage").then((module) => ({ default: module.PipelinePage }))
);

const AnswersPage = lazyChunk(() =>
  import("./features/answers/AnswersPage").then((module) => ({ default: module.AnswersPage }))
);
const SettingsPage = lazyChunk(() =>
  import("./features/settings/SettingsPage").then((module) => ({ default: module.SettingsPage }))
);

function IssuePageFallback(): ReactNode {
  return (
    <div aria-busy="true">
      <header className={`mb-6 border-b pb-6 ${borderDefault}`}>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className={`text-sm font-semibold ${linkText}`}>
            <span
              className={`inline-block h-4 w-16 animate-pulse rounded align-middle ${skeletonBg}`}
            />
          </p>
          <button
            className={`rounded-lg border px-3 py-2 text-sm font-medium ${textTransparent} ${borderStrong}`}
            disabled
            type="button"
          >
            Pin issue
          </button>
        </div>
        <h1
          className={`mt-2 w-full rounded-lg border px-2 py-1 text-2xl font-semibold ${borderTransparent}`}
        >
          <span
            className={`inline-block h-7 w-2/3 animate-pulse rounded align-middle ${skeletonBg}`}
          />
        </h1>
      </header>
      {/* `overflow-hidden`: the ten labels are wider than a narrow window and this row neither
          wraps nor scrolls, where the real tablist scrolls inside itself. Clipped, a chunk's load
          never scrolls the page sideways. */}
      <div className={`mb-4 flex gap-2 overflow-hidden border-b ${borderDefault}`} role="tablist">
        {ISSUE_TAB_LABELS.map((label) => (
          <span
            className={`animate-pulse rounded px-3 py-2 text-sm ${textTransparent} ${skeletonBg}`}
            key={label}
          >
            {label}
          </span>
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
      <InboxHost />
    </section>
  );
}

/** The sign-in page. A session that lapsed shows what it keeps for its person
 *  (`KeptWritesHost`), and its sign-in link asks nothing on leaving: the page comes back to this
 *  tab, where every kept write resumes (`Writes.leavingToSignIn`). */
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
      <KeptWritesHost />
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

function ShellSkeleton(): ReactNode {
  return (
    <div aria-busy="true" className={`xl:flex ${canvasText}`}>
      <header
        className={`flex items-center px-3 xl:hidden ${railBorder} ${railBg} ${railText} border-b`}
      >
        <span className="text-lg font-semibold">Dispatch</span>
      </header>
      <aside
        className={`hidden p-5 xl:block xl:min-h-dvh xl:w-80 xl:border-r ${railBorder} ${railBg} ${railText}`}
      >
        <span className="text-lg font-semibold">Dispatch</span>
        <div aria-label="Loading navigation" className="mt-6 space-y-2" role="status">
          {[0, 1, 2, 3].map((row) => (
            <div className={`h-4 w-full animate-pulse rounded ${railActiveBg}`} key={row} />
          ))}
        </div>
      </aside>
      <main className="min-w-0 flex-1 p-6">
        <div aria-label="Loading Dispatch" className="space-y-3" role="status">
          {[0, 1, 2].map((row) => (
            <div className={`h-4 w-full animate-pulse rounded ${skeletonBg}`} key={row} />
          ))}
        </div>
      </main>
    </div>
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

/** Everything the shell draws, inside the new issue page's providers where the reader's shell
 *  has them (`providers`, from the frame-shell chunk), and as it is everywhere else. The state
 *  those providers read, the navigation's sheet and what `?` opens, is the shell's own. */
function WithFrameProviders({
  children,
  navigationOpen,
  openHelp,
  providers,
  setNavigationOpen,
}: {
  children: ReactNode;
  navigationOpen: boolean;
  openHelp: () => void;
  providers: FrameProviders | undefined;
  setNavigationOpen: (open: boolean) => void;
}): ReactNode {
  if (providers === undefined) return children;
  return providers(children, { navigationOpen, openHelp, setNavigationOpen });
}

/**
 * The shell every signed-in page is drawn in. `frame` is what the new issue page's frame changes
 * in it, on an issue's address for a reader who chose that page, and `providers` are what that
 * reader's shell mounts around everything it draws, on every page; both come from the
 * frame-shell chunk (`features/issue-page/frame-shell.tsx`) and are undefined for every other
 * reader, whose shell is exactly what this draws without them.
 */
function AppShell({
  frame,
  providers,
  user,
}: {
  frame: ShellFrame | undefined;
  providers?: FrameProviders;
  user: AuthenticatedUser;
}): ReactNode {
  const queryClient = useQueryClient();
  const [navigationOpen, setNavigationOpen] = useState(false);
  const [inboxDrawerOpen, setInboxDrawerOpen] = useState(false);
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
  const location = useLocation();
  // Whether the shell draws the new issue page's frame: an issue's address, for a reader who
  // chose the new page (`frame`). Every other page, and every other reader's issue page, keeps
  // the shell below exactly as it was.
  const issueFrame = frame !== undefined;
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
  const connection = useLiveSocketPill();
  // One number for the Inbox, composed in one place (`useInboxCount.ts`): the work
  // waiting on the reader plus the threads they have not read.
  const inboxCount = useInboxCount();
  const unreadReplies = totalUnreadReplies(useQuery(userAgentStateQuery()).data);

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
  const navigate = useNavigate();
  // The registry as described when `?` fired (focus still on the caller); `null` while closed.
  const [helpSnapshot, setHelpSnapshot] = useState<readonly KeyBindingDescription[] | null>(null);
  // What a page's own Keys control does (the frame's providers hand it to the page): what `?`
  // does.
  const openHelp = useCallback(() => setHelpSnapshot(appKeymap.describe()), []);
  const [createOpen, setCreateOpen] = useState(false);
  const routeProject = routeProjectOf(location.pathname);
  useKeymap("global", [
    {
      // Opens only: while the bar is open the `dialog` scope is the only one consulted, and
      // the bar's own `$mod+k` binding closes it. It fires while the reader is writing, so
      // the key reaches the bar from inside a document's editor too.
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
      id: "search-only",
      keys: "/",
      label: "Search",
      palette: false,
      run: () => setBarOpen(true),
    },
    {
      id: "help",
      keys: "?",
      label: "Keyboard shortcuts",
      run: () => setHelpSnapshot(appKeymap.describe()),
    },
    { id: "create", keys: "c", label: "Create issue", run: () => setCreateOpen(true) },
    { id: "go-inbox", keys: "g i", label: "Go to Inbox", run: () => navigate("/") },
    { id: "go-agents", keys: "g a", label: "Go to Agents", run: () => navigate("/agents") },
    { id: "go-settings", keys: "g s", label: "Go to Settings", run: () => navigate("/settings") },
    {
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
      id: "go-project",
      keys: "g p",
      label: "Go to project…",
      palette: false,
      run: () => setBarOpen(true),
    },
    {
      id: "inbox-drawer",
      keys: "i",
      label: "Open the inbox",
      run: () => setInboxDrawerOpen((open) => !open),
      // The Inbox page already shows everything the drawer would, so a peek over itself is
      // never offered; `g i` still gets the reader there from anywhere else.
      when: () => !routeIsInbox(location.pathname),
    },
    {
      id: "toggle-sidebar",
      keys: "Shift+S",
      label: "Toggle sidebar",
      run: () => setSidebarHidden(!sidebarHidden),
      // Below `xl` the sidebar is a sheet with its own Menu control, and the preference is inert;
      // so it is in the new issue page's frame, whose navigation is its icon rail.
      when: () => !isCompactViewport && !issueFrame,
    },
    {
      // In the new issue page's frame the margin is Details, which this key opens and closes at
      // every width (`frame.toggleDetails`). Its id follows the gate, so that crossing it
      // registers the binding again and the list of keys reads the label of the shell it is in.
      id: frame === undefined ? "toggle-margin" : "toggle-details",
      keys: "Shift+M",
      label: frame === undefined ? "Toggle margin" : "Open or close Details",
      run: () => (frame === undefined ? setMarginHidden(!marginHidden) : frame.toggleDetails()),
      // Below `xl` the margin is a sheet the route opens itself: `Margin` reads the preference
      // only from `xl`, so a toggle there would change nothing on screen and flip it for later.
      when: () => frame !== undefined || (!isCompactViewport && hasMargin),
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
      onOpenInboxDrawer={() => setInboxDrawerOpen(true)}
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
      signOutError={
        signOutRefused && signOut.error !== null ? signOutFailure(signOut.error) : undefined
      }
      signOutPending={signOut.isPending || askingTabs}
      user={user}
    />
  );

  // One count on one control: the work waiting on the reader and the threads they have not
  // read, which the Inbox lists together now. On the Inbox page it stays plain text - the page
  // is the Inbox already, so there is nowhere for it to go. The peek that opened the drawer
  // from this bar stays beside it, without the count it used to carry.
  const inboxLabel = inboxCount === 0 ? "Inbox" : `Inbox ${inboxCount}`;
  const inboxBadgeClass = `whitespace-nowrap rounded-full px-2 py-1 text-xs font-semibold ${
    inboxCount === 0 ? railHoverBg : `${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`
  }`;

  return (
    <WithFrameProviders
      navigationOpen={navigationOpen}
      openHelp={openHelp}
      providers={providers}
      setNavigationOpen={setNavigationOpen}
    >
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
            takes the pointer. A socket Dispatch refuses carries no status the page can read, so
            nothing in the pill is terminal: once reconnecting has gone on for two minutes it
            offers a reload, and the page keeps trying underneath. */}
        <div
          className="pointer-events-none fixed right-4 bottom-[max(5rem,calc(var(--foot-composer-inset,0px)_+_1rem))] z-50 flex w-[min(28rem,calc(100vw-2rem))] flex-col items-end gap-2 xl:bottom-[calc(var(--foot-composer-inset,0px)_+_1rem)] *:pointer-events-auto"
          ref={statusCornerRef}
        >
          <PendingWritesHost />
          <DraftsNotKept />
          {connection === "stuck" ? (
            <p
              aria-live="polite"
              className={`flex items-center gap-2 rounded-full border px-3 py-1 text-xs font-medium shadow-lg ${calloutDangerBorder} ${calloutDangerBg} ${badgeBlocking.text}`}
              data-testid="connection-pill"
            >
              Live updates are not reaching this page
              <button className="underline" onClick={() => window.location.reload()} type="button">
                Reload
              </button>
            </p>
          ) : connection === "reconnecting" ? (
            <p
              aria-live="polite"
              className={`rounded-full border px-3 py-1 text-xs font-medium shadow-lg ${calloutWarningBorder} ${statusConnecting.bg} ${statusConnecting.text}`}
              data-testid="connection-pill"
            >
              Reconnecting…
            </p>
          ) : null}
        </div>
        {isCompactViewport && !issueFrame ? (
          <header
            className={`flex items-center justify-between border-b px-3 ${railBorder} ${railBg} ${railText}`}
          >
            {/* The title never shrinks and no count breaks its own words: the counts that do not
                fit beside the title wrap onto a second row of the bar, with Menu on the right. */}
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
              <Link className="shrink-0 text-lg font-semibold" to="/">
                Dispatch
              </Link>
              {routeIsInbox(location.pathname) ? (
                <span className={inboxBadgeClass}>{inboxLabel}</span>
              ) : (
                <>
                  <button
                    aria-label="Peek at the inbox without leaving this page"
                    className={`whitespace-nowrap rounded-full px-2 py-1 text-xs font-semibold ${railHoverBg}`}
                    onClick={() => setInboxDrawerOpen(true)}
                    type="button"
                  >
                    Peek
                  </button>
                  <Link className={inboxBadgeClass} to="/">
                    {inboxLabel}
                  </Link>
                </>
              )}
              {unreadReplies === 0 ? null : (
                <Link
                  className={`whitespace-nowrap rounded-full px-2 py-1 text-xs font-semibold ${railNeedsYouBadgeBg} ${railNeedsYouBadgeText}`}
                  to="/agents"
                >
                  {unreadRepliesLabel(unreadReplies)}
                </Link>
              )}
            </div>
            <button
              aria-expanded={navigationOpen}
              aria-label="Open navigation"
              className={`shrink-0 rounded-lg px-3 py-2 text-sm font-medium ${railHoverBg}`}
              onClick={() => setNavigationOpen(true)}
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
        {frame !== undefined ? (
          frame.rail({
            inboxCount,
            navigationOpen,
            onOpenNavigation: () => setNavigationOpen(true),
            project: routeProject,
            user,
          })
        ) : isCompactViewport ? null : sidebarHidden ? (
          <aside
            aria-label="Collapsed sidebar"
            className={`fixed inset-y-0 left-0 z-10 flex w-14 justify-center border-r p-2 ${railBorder} ${railBg} ${railText}`}
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
        ) : (
          // The sidebar is first in the document and as tall as the viewport, so left to itself
          // Chromium's scroll anchoring picks it as the anchor node - and since it never moves,
          // nothing is ever compensated. Excluding it makes the browser anchor inside <main>, so
          // content growing above the reader (a card's thread loading, Markdown rendering) no
          // longer shifts what they are looking at.
          <aside
            aria-label="Navigation"
            className={`relative order-1 w-80 max-w-none border-r [overflow-anchor:none] ${railBorder} ${railBg} ${railText}`}
          >
            {/* The aside stretches to the page's height, so the navigation is a viewport-tall
                column that sticks inside it: on a long issue the links stay reachable, and the
                identity footer sits at the bottom of the screen rather than the bottom of the
                document. */}
            <div className="sticky top-0 flex h-dvh flex-col overflow-y-auto p-5">{navigation}</div>
          </aside>
        )}
        {/* In the frame `<main>` is drawn as the frame says (`frame.mainClass`,
            `frame.mainAttributes`): no padding of its own, and no card on a reference's hover. */}
        <main
          className={
            frame?.mainClass ??
            `min-w-0 flex-1 p-6 outline-none xl:order-2 ${fillsViewport ? "flex min-h-0 flex-col" : "pb-32 xl:pb-6"} ${mainLayoutClass}`
          }
          data-shell-layout={issueFrame || fullWidth ? "full-width" : "standard"}
          {...frame?.mainAttributes}
          data-testid="main-content"
          id="main-content"
          ref={mainRef}
          tabIndex={-1}
        >
          <ErrorBoundary region="this page" resetKey={location.pathname}>
            <Suspense fallback={frame?.fallback ?? <IssuePageFallback />}>
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
                <Route element={<IssuePage />} path="/issues/:key/*" />
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
            </Suspense>
          </ErrorBoundary>
          {/* The live views the person has open, inside `<main>` so each takes the layout and
              the iOS keyboard fit the live route already has, and beside the routes rather than
              under them: a view outlives the address that opened it, so neither leaving the
              live route, nor another page's throw, nor a lazy page suspending may unmount it.
              The live route's own element renders nothing. */}
          <MountedLiveViews front={frontLiveView} />
        </main>
        <ErrorBoundary region="the margin" resetKey={location.pathname}>
          <MarginColumn
            collapsed={marginHidden}
            drawer={issueFrame}
            onCollapsedChange={setMarginHidden}
            onWidthChange={setMarginWidth}
            width={marginWidth}
          />
        </ErrorBoundary>
        <SearchPaletteHost onClose={() => setBarOpen(false)} open={barOpen} />
        <InboxDrawerHost onClose={() => setInboxDrawerOpen(false)} open={inboxDrawerOpen} />
        <ShortcutHelpHost onClose={() => setHelpSnapshot(null)} snapshot={helpSnapshot} />
        {createOpen ? <CreateIssueDialogHost onClose={() => setCreateOpen(false)} /> : null}
        <RefPreviewLayer />
      </div>
    </WithFrameProviders>
  );
}

/** The shell of a reader who turned the new issue page on, drawn as the sign-in check's skeleton
 *  while its chunk is on its way. */
const FrameShell = chunkComponent(frameShellChunk, <ShellSkeleton />);

/**
 * The shell the reader's choice of issue page draws: today's for `tabs`, with the margin's
 * provider around it, and for `outline` the frame-shell chunk, which draws the same shell inside
 * the new page's providers and with its frame on an issue's address. Only the choice is read
 * here, never the address, so a move between an issue and another page keeps the shell mounted;
 * a change of the choice mounts the other shell.
 */
function ChosenShell({ user }: { user: AuthenticatedUser }): ReactNode {
  if (useStoredIssuePageChoice(user.login) === "tabs") {
    return (
      <MarginProvider>
        <AppShell frame={undefined} user={user} />
      </MarginProvider>
    );
  }
  return (
    <FrameShell
      shell={(frame, providers) => <AppShell frame={frame} providers={providers} user={user} />}
    />
  );
}

function AuthenticatedApp({ user }: { user: AuthenticatedUser }): ReactNode {
  useEventFeed();
  // Every Markdown body the shell renders - routes, the margin, the inbox drawer, previews and
  // dialogs - draws a decision block in its text as the read-only card (`TextAskCard`).
  return (
    <KeymapProvider>
      <AgentRegistryPoller />
      <TextAskCard.Provider value={TextAskCardHost}>
        <ChosenShell user={user} />
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
    const writes = writesOf(queryClient);
    writes.signedIn(login);
    // Signed in, the writes this person's reload interrupted go on under their keys, and their
    // refusals show again. The take-up waits for the kinds of write, a chunk of their own
    // (`loadWriteKinds`), and every write pressed meanwhile waits for the take-up; so the take-up
    // begins here, a commit before the shell is drawn (`shellFor`, below), before any page exists
    // to press a write (`Writes.resume`, `Writes.run`).
    if (login !== undefined) writes.resume(login, loadWriteKinds);
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
