import {
  type ComponentType,
  lazy,
  type ReactNode,
  Suspense,
  useCallback,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

import { ApiError } from "../../api/client";
import type { Artifact, IssueDetails, IssuePriority } from "../../api/types";
import { chunkComponent } from "../../components/chunk-component";
import { QueryError } from "../../components/QueryError";
import { tabButtonId } from "../../components/Tabs";
import {
  borderDefault,
  dangerText,
  linkHoverText,
  linkText,
  statusPill,
  statusPillDot,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ArtifactRoutePanel } from "../artifacts/ArtifactRoutePanel";
import { ChromeAfterTheFirstBatch } from "../doc/AfterTheFirstBatch";
import { ApprovalChip } from "../doc/ApprovalChip";
import { ConnectionDot } from "../doc/ConnectionDot";
import { afterTheFirstBatch } from "../doc/first-batch";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { putTheBoxAt, whatScrolls, whereTheReaderIs } from "../doc/readers-place";
import { outsideAskCard } from "../inbox/ask-card";
import { statusText } from "../project/board-model";
import {
  buildReferencePath,
  documentRoute,
  type IssueRoute,
  type IssueTab,
  parseIssuePath,
} from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { ItemMatchList } from "./ItemMatchList";
import { issueStatusPillClassName, issueStatusPillDotClassName } from "./issue-header-classes";
import { IssueHeaderSkeleton, IssueTabsSkeleton } from "./issue-page-skeleton";
import { issueTabChunks } from "./issue-tab-list";
import { stateForIssue } from "./pins";
import { forgetTheReach, reachingTheChrome } from "./reaching-the-chrome";
import { SpecToolbar } from "./SpecToolbar";
import { useIssueDetail } from "./useIssueDetail";
import { useIssuePriority } from "./useIssuePriority";
import { type ItemLanding, type ItemRoute, isItemRoute, useItemLanding } from "./useItemLanding";

/**
 * The parts of the issue page that are not its frame, each in a chunk of its own: the header
 * and the tab strip behind a `Suspense` boundary of their own, and each tab panel drawn in
 * the commit its chunk arrives in (`components/chunk-component.tsx`).
 *
 * There are two reasons, and they are different reasons.
 *
 * The first is the commit. The page used to mount in one commit - the header, the tab strip
 * and the document surface together - and a commit cannot be interrupted, so that one commit
 * held the main thread for about 0.7 s on a phone at 4x CPU throttling; this page's first
 * render runs inside a transition, which makes it interruptible and leaves each commit whole
 * (`app.tsx`'s `IssuePageInATransition` opens that transition, and the arrival of this
 * module's own chunk opens one again where it lands after it, `chunkComponent`'s
 * `inATransition`; dispatch://INFRA-341, R11). Each part here makes one commit of its own
 * instead: the
 * document's part commits first, with the two boxes the reader already has in front of them
 * (`issue-page-skeleton.tsx`), and the header, the tabs and each side list land after it, one
 * commit each.
 *
 * The second is the evaluation. A chunk and every chunk it statically imports evaluate in one
 * main-thread task, so while this module imported the tab panels and the document surface
 * statically, opening a document evaluated all of them in one task of 505 to 579 ms: the
 * Conversation with the ask card and its composer, the Activity list with the margin's cards,
 * the role documents with the contracts package's zod schemas - none of which a document
 * address draws. Each `import()` below evaluates in a task of its own, and a panel the
 * address does not open is a task that never runs at all; `web/src/entry-chunk-contents.ts`
 * holds the build to that.
 *
 * The chunks a document address needs are downloaded beside the page's own, from the address
 * (`DOCUMENT_ADDRESS_MODULES` in `web/vite.config.ts`), so the reader waits for no request
 * here. What appears, and when, is unchanged: the header and tabs replace the skeleton the
 * reader was already reading, and a side list draws under the document it belongs to, where
 * nothing above it moves as it lands.
 *
 * A tab's panel is mounted only once that tab has been opened (`activatedTabs`), so its chunk
 * is asked for then, as the reader asks for the tab.
 */
const IssueHeader = lazy(() =>
  import("./IssueHeader").then((module) => ({ default: module.IssueHeader }))
);
const IssueTabs = lazy(() =>
  import("./IssueTabs").then((module) => ({ default: module.IssueTabs }))
);
const SpecRequirements = lazy(() =>
  import("./SpecRequirements").then((module) => ({ default: module.SpecRequirements }))
);
/** The document surface, which every tab that shows a document draws and which both addresses
 *  that open a document preload. */
const ArtifactDocument = lazy(() =>
  import("../artifacts/ArtifactDocument").then((module) => ({
    default: module.ArtifactDocument,
  }))
);
/** The tab panels, each drawn in the commit its chunk arrives in, for the same reason: the
 *  panel an address names is asked for from that address (`requestTheOpenPagesChunk` in
 *  `app.tsx`, through this same map), and a reader who opens a tab is waiting for its panel
 *  rather than for a space kept where it will be. */
const ActivityTab = chunkComponent(() =>
  issueTabChunks.activity().then((module) => module.ActivityTab)
);
const ChildrenTab = chunkComponent(() =>
  issueTabChunks.children().then((module) => module.ChildrenTab)
);
const CollaboratorsTab = chunkComponent(() =>
  issueTabChunks.collaborators().then((module) => module.CollaboratorsTab)
);
const ConversationTab = chunkComponent(() =>
  issueTabChunks.conversation().then((module) => module.ConversationTab)
);
const DecisionsPanel = chunkComponent(() =>
  issueTabChunks.decisions().then((module) => module.DecisionsPanel)
);
const PlanTab = chunkComponent(() => issueTabChunks.plan().then((module) => module.PlanTab));
const VerificationTab = chunkComponent(() =>
  issueTabChunks.verification().then((module) => module.VerificationTab)
);

export function IssuePage(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) {
    return <NotFoundPage />;
  }
  // key={route.key}: switching to a different issue remounts IssueDetail
  // fresh (discarding any unsaved local drafts); switching tabs within the
  // same issue keeps route.key unchanged, so it only re-renders.
  return <IssueDetail key={route.key} route={route} />;
}

const itemNoun: Record<ItemRoute["kind"], string> = {
  ask: "Ask",
  comment: "Comment",
  message: "Message",
};

/** An item deep link the SPA could not resolve: the id names nothing on this issue, or the
 *  lookup failed. Either way the reader is told which item, inside the issue page. */
function ItemLandingFailure({
  landing,
  route,
}: {
  landing: Extract<ItemLanding, { kind: "missing" | "unavailable" }>;
  route: ItemRoute;
}): ReactNode {
  const noun = itemNoun[route.kind];
  if (landing.kind === "missing") {
    return (
      <NotFoundPage
        backLabel={`Back to ${route.key}`}
        backTo={`/issues/${route.key}`}
        detail={`That link doesn't name ${route.kind === "ask" ? "an" : "a"} ${noun.toLowerCase()} on ${route.key} — it may have been deleted.`}
        title={`${noun} not found`}
      />
    );
  }
  return (
    <section>
      <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>
        Couldn&apos;t load this {noun.toLowerCase()}
      </h1>
      <div className="mt-2">
        <QueryError message={`Couldn't load this ${noun.toLowerCase()}.`} onRetry={landing.retry} />
      </div>
    </section>
  );
}

/**
 * What the issue's header tells a reader without being asked, inside the state row: where its
 * work stands, and what a review of its spec has decided. It stands in the box the header
 * keeps until the editor's first batch (`issue-page-skeleton.tsx`, dispatch://INFRA-341 R11).
 * The third thing the header says unasked - that the issue is closed - is a block above these
 * rows there and in the box too, so the box keeps its height: `IssueHeaderSkeleton`'s own
 * `isClosed`.
 *
 * Every word of it is the issue and its primary document, both of which the page has read
 * before it draws anything, so none of it is one of the reads the hold moved, and the header
 * says the same three things when it lands. The controls that stand beside them there - Reopen,
 * the status pick, Approve and Request changes - are not here: a press on this box is how a
 * reader asks for a control, and it hands them the header itself.
 */
function WhatTheHeaderWouldSay({
  artifact,
  issue,
}: {
  artifact: Artifact;
  issue: IssueDetails;
}): ReactNode {
  return (
    <>
      {/* The status pill at the size the header's own pick stands at, from the one place both
          are built from (`issue-header-classes.ts`), so the pick landing in its place moves
          nothing beside it. */}
      <span
        className={`${issueStatusPillClassName} ${borderDefault} ${statusPill.bg} ${statusPill.text}`}
      >
        <span aria-hidden="true" className={`${issueStatusPillDotClassName} ${statusPillDot}`} />
        <span className="sr-only">Status</span>
        {statusText(issue.status)}
      </span>
      <ApprovalChip artifact={artifact} asText variant="header" />
    </>
  );
}

/**
 * What the `issue` scope acts on. Every target is a control the header or the tablist already
 * renders, so a key does exactly what a click on it does. Scoping the lookup to
 * `[data-issue-header]` keeps a child issue's row in the Children tab - it carries the same
 * accessible names - out of reach.
 */
const HEADER = "[data-issue-header]";
const LABELS_TRIGGER = 'button[aria-label="Edit labels"]';
const PIN_TOGGLE = 'button[aria-label="Pin issue"], button[aria-label="Unpin issue"]';
const PRIORITY_SELECT = 'select[aria-label^="Priority of "]';
const STATUS_SELECT = 'select[aria-label="Status"]';
const TITLE_HEADING = 'h1[tabindex="0"]';

/** The header control `selector` names, or `null` when it is absent or refuses input (a closed
 *  issue, a save in flight): the one fact both the key's `run` and the `when` that offers it read,
 *  so a shortcut is listed exactly while its control would take a click. */
function usableControl(selector: string): HTMLElement | null {
  const node = document.querySelector(HEADER)?.querySelector<HTMLElement>(selector);
  if (node == null) {
    return null;
  }
  const disabled =
    (node instanceof HTMLButtonElement || node instanceof HTMLSelectElement) && node.disabled;
  return disabled ? null : node;
}

/** Focuses the header control `selector` names, answering whether there was one to focus. */
function focusTheControl(selector: string): boolean {
  const node = usableControl(selector);
  node?.focus();
  return node !== null;
}

/** Presses it, with the same answer. */
function pressTheControl(selector: string): boolean {
  const node = usableControl(selector);
  node?.click();
  return node !== null;
}

/**
 * Whether the binding whose control `find` looks up is offered, which is one question asked of
 * two pages. While this page is still holding its chrome every binding is offered: the press is
 * how a reader with no pointer asks for that chrome, and `reachingTheChrome` draws it and then
 * does what the press came for (`reaching-the-chrome.ts`). Once the chrome is there, a binding
 * is offered exactly while its control would take a click, so the palette lists what a press
 * would do and nothing else. Written here once, because every binding below asks it.
 */
function reachable(find: () => Element | null): boolean {
  return !afterTheFirstBatch() || find() !== null;
}

/**
 * Whether focus is outside every `<input>` and outside every ask card. `0`-`3` and `Shift+P` are
 * the two keys that write the issue without focusing a control, so they are the two that have to
 * say where the reader is not: a digit typed straight after pressing the Conversation's `Open`
 * filter or picking an ask's option belongs to that control's own surface, not to the issue's
 * priority. The registry passes single keys through every control that takes no typed text, so
 * this is the page's own question to answer.
 *
 * A focused `<button>` is deliberately not excluded. Every single-key binding in the registry
 * fires from one: a digit has no native meaning on a button, and after any click - a tab, a
 * disclosure, Copy - focus sits on the button that was clicked, which is where a reader reaching
 * for the next shortcut stands. Withholding these keys there would make them work only from the
 * page's own background. An ask card's buttons are the exception, and that is what
 * `outsideAskCard` covers (`inbox/ask-card.ts`, the one definition both pages read): inside the
 * card, the card's controls own the keys. `l` and `e` open a control rather
 * than write, so a stray press is visible and undoable and they are offered throughout.
 */
function outsideInputsAndAskCards(): boolean {
  return !(document.activeElement instanceof HTMLInputElement) && outsideAskCard();
}

/** The tabs the chords reach, each through the tablist button `IssueTabs` renders for it. */
const tabChords: readonly { keys: string; label: string; tab: IssueTab }[] = [
  { keys: "t s", label: "Spec tab", tab: "spec" },
  // `t v` is Activity and `t c` the Conversation, so Verification takes its checks' `k`; `t p`
  // is free (`p` alone focuses the priority, which is a different scope of key).
  { keys: "t k", label: "Verification tab", tab: "verification" },
  { keys: "t p", label: "Plan tab", tab: "plan" },
  { keys: "t c", label: "Conversation tab", tab: "conversation" },
  { keys: "t h", label: "Children tab", tab: "children" },
  { keys: "t a", label: "Artifacts tab", tab: "artifacts" },
  // `t a` is Artifacts and `l` is the issue's label editor, so Activity takes `t v`.
  { keys: "t v", label: "Activity tab", tab: "activity" },
  { keys: "t d", label: "Decisions tab", tab: "decisions" },
  // `s k p c h a v d` are taken, so Collaborators, the last tab, takes `t o`.
  { keys: "t o", label: "Collaborators tab", tab: "collaborators" },
];

/** One of the two role-document tab panels, Verification and Plan. They differ only in the tab
 *  they name and the component that frames the document; the panel itself, the lazy-mount guard
 *  and the `ArtifactDocument` under it are one thing, written once. */
function RoleDocumentPanel({
  Tab,
  activated,
  active,
  artifact,
  isClosed,
  issueKey,
  onVersionChange,
  tab,
}: {
  Tab: ComponentType<{
    artifact: Artifact | undefined;
    children?: ReactNode;
    issueKey: string;
  }>;
  activated: boolean;
  active: boolean;
  artifact: Artifact | undefined;
  isClosed: boolean;
  issueKey: string;
  onVersionChange: (artifact: Artifact, version: number | null) => void;
  tab: "verification" | "plan";
}): ReactNode {
  return (
    <div
      aria-hidden={!active}
      aria-labelledby={`issue-${tab}-tab`}
      hidden={!active}
      id={`issue-${tab}-panel`}
      role="tabpanel"
    >
      {activated ? (
        <Suspense fallback={null}>
          <Tab artifact={artifact} issueKey={issueKey}>
            {artifact === undefined ? null : (
              <ArtifactDocument
                artifact={artifact}
                isClosed={isClosed}
                owner={{ key: issueKey, kind: "issue" }}
                onVersionChange={(version) => onVersionChange(artifact, version)}
                showDiff={false}
                version={undefined}
              />
            )}
          </Tab>
        </Suspense>
      ) : null}
    </div>
  );
}

function IssueDetail({ route }: { route: IssueRoute }): ReactNode {
  const {
    activeTab,
    artifactRoute,
    askId,
    commentId,
    conversationFocusItemId,
    isPrimaryArtifactRoute,
    highlightTerm,
    issue,
    primaryArtifact,
    planArtifact,
    selectedArtifact,
    state,
    verificationArtifact,
  } = useIssueDetail(route);
  const landing = useItemLanding(route, issue.data);
  const navigate = useNavigate();
  const panelScroll = useRef<Partial<Record<IssueTab, number>>>({});
  const page = useRef<HTMLElement>(null);
  /** The box that scrolls this page: the column of a reading pane, or the page itself (`null`)
   *  where the page scrolls it. A tab's place is recorded down that box and put back down the
   *  same one (`whereTheReaderIs`, `features/doc/readers-place.ts`); recorded from the window
   *  in a column it would be a number the reader never moves, and every tab would come back at
   *  the same place. */
  const theBoxThatScrollsIt = useCallback(
    () => (page.current === null ? null : whatScrolls(page.current)),
    []
  );
  const [specShowDiff, setSpecShowDiff] = useState(false);
  const [specToolbar, setSpecToolbar] = useState<DocumentToolbar | undefined>(undefined);
  const [artifactShowDiff, setArtifactShowDiff] = useState(false);
  const [artifactToolbar, setArtifactToolbar] = useState<DocumentToolbar | undefined>(undefined);
  const handleSpecToolbarChange = useCallback((next: DocumentToolbar | undefined) => {
    setSpecToolbar(next);
    if (next === undefined) {
      setSpecShowDiff(false);
    }
  }, []);
  const handleArtifactToolbarChange = useCallback((next: DocumentToolbar | undefined) => {
    setArtifactToolbar(next);
    if (next === undefined) {
      setArtifactShowDiff(false);
    }
  }, []);

  // The Conversation's first placement runs as a child layout effect, before the restore below
  // in the same commit, so a position recorded while the tab showed only its loading text is
  // forgotten here and the reader lands on the newest message. A return to a list the reader was
  // already placed in never fires it, so that restore stands (INFRA-303).
  const forgetConversationScroll = useCallback(() => {
    delete panelScroll.current.conversation;
  }, []);

  // The scope is the issue page itself, and every binding drives the header control or the tab
  // its label names - the same handler a click reaches, offered while that control is. The
  // digits go through `useIssuePriority` on the route's key, the query key this page reads, so
  // a keyed priority lands in the same cache the picker writes. `IssueHeader` registers the
  // palette-only issue actions into this same scope; it never pushes one of its own.
  const priorityWrite = useIssuePriority(route.key);
  useKeymapScope("issue");
  useKeymap("issue", [
    {
      id: "status",
      keys: "s",
      label: "Focus the status",
      run: () => reachingTheChrome(() => focusTheControl(STATUS_SELECT)),
      when: () => reachable(() => usableControl(STATUS_SELECT)),
    },
    {
      id: "priority",
      keys: "p",
      label: "Focus the priority",
      run: () => reachingTheChrome(() => focusTheControl(PRIORITY_SELECT)),
      when: () => reachable(() => usableControl(PRIORITY_SELECT)),
    },
    {
      id: "set-priority",
      keys: ["0", "1", "2", "3"],
      label: "Set priority P0–P3",
      run: (event) => {
        // The key, not the event: the press may act a frame or two later, once the header this
        // page was holding has been drawn.
        const priority = Number(event.key) as IssuePriority;
        reachingTheChrome(() => {
          if (usableControl(PRIORITY_SELECT) === null) {
            return false;
          }
          priorityWrite.submit(priority);
          return true;
        });
      },
      when: () => reachable(() => usableControl(PRIORITY_SELECT)) && outsideInputsAndAskCards(),
    },
    {
      id: "labels",
      keys: "l",
      label: "Edit labels",
      run: () => reachingTheChrome(() => pressTheControl(LABELS_TRIGGER)),
      when: () => reachable(() => usableControl(LABELS_TRIGGER)),
    },
    {
      // The heading takes focus only while the issue is open: closed, its `tabIndex` is -1 and
      // `onFocus` does not open the editor, so the key is offered exactly while it edits.
      id: "title",
      keys: "e",
      label: "Edit the title",
      run: () => reachingTheChrome(() => focusTheControl(TITLE_HEADING)),
      when: () => reachable(() => usableControl(TITLE_HEADING)),
    },
    {
      id: "pin",
      keys: "Shift+P",
      label: "Pin or unpin the issue",
      run: () => reachingTheChrome(() => pressTheControl(PIN_TOGGLE)),
      when: () => reachable(() => usableControl(PIN_TOGGLE)) && outsideInputsAndAskCards(),
    },
    {
      // `s` and `p` put focus in a native select, where every single-key binding is suspended
      // and the next keystroke is the select's own type-ahead - `t` would pick Todo or Testing
      // rather than start the tab chord. Escape is the way back out, as it is on the Inbox
      // rows, the board's cards and the architecture rows.
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back out of the focused header control",
      run: () => (document.activeElement as HTMLElement | null)?.blur(),
      when: () => {
        const active = document.activeElement;
        return active === usableControl(STATUS_SELECT) || active === usableControl(PRIORITY_SELECT);
      },
    },
    ...tabChords.map(({ keys, label, tab }) => ({
      id: `tab-${tab}`,
      keys,
      label,
      run: () =>
        reachingTheChrome(() => {
          const button = document.getElementById(tabButtonId("issue", tab));
          button?.click();
          return button !== null;
        }),
      when: () => reachable(() => document.getElementById(tabButtonId("issue", tab))),
    })),
  ]);

  // A press still waiting for chrome that never came goes with the page that took it
  // (`reaching-the-chrome.ts`): the next page's own header is not what it was reaching for. The
  // layout phase, because that is the phase the next page's header answers a press from: React
  // runs a leaving tree's passive cleanups after the arriving tree's layout effects, so a
  // passive forget here let issue B's header do what was pressed on issue A (dispatch://INFRA-341).
  useLayoutEffect(() => forgetTheReach, []);

  useLayoutEffect(() => {
    const top = panelScroll.current[activeTab];
    if (top !== undefined) {
      putTheBoxAt(theBoxThatScrollsIt(), top);
    }
  }, [activeTab, theBoxThatScrollsIt]);

  // Once a panel's tab has ever been active, keep rendering its content even
  // while hidden — that is what keeps the Spec document connected and the Conversation's read
  // observer alive across tab switches (see the `hidden` panels below). A panel the user has
  // never opened stays unmounted, so a fresh page load doesn't pay for panels it never shows.
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
  const issueArtifacts = useMemo(
    () => new Map((issue.data?.artifacts ?? []).map((artifact) => [artifact.id, artifact])),
    [issue.data?.artifacts]
  );

  useDocumentTitle(
    issue.data === undefined ? "Dispatch" : `${issue.data.key} · ${issue.data.title} · Dispatch`
  );

  // A failed issue read is reported before an item link's landing: the landing waits on the
  // issue to tell it which artifact an anchor names, so reading its pending state first turns a
  // 500 into "Loading issue…" for good.
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
  if (isItemRoute(route) && landing?.kind === "ambiguous") {
    return <ItemMatchList candidates={landing.candidates} route={route} />;
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
  const selectDocumentVersion = (artifact: Artifact, version: number | null) =>
    navigate(buildReferencePath(documentRoute(artifact, version ?? undefined)));

  return (
    <section ref={page}>
      {/* What the header says about this issue stands in the header's own box until the header
          lands, and in its box again while the header's chunk arrives: a reader is told the
          issue is closed, where its work stands and what a review of its spec decided, without
          having to press for it (`WhatTheHeaderWouldSay`). */}
      <ChromeAfterTheFirstBatch
        label="Show the issue header"
        space={
          <IssueHeaderSkeleton
            isClosed={isClosed}
            state={<WhatTheHeaderWouldSay artifact={primaryArtifact} issue={issue.data} />}
          />
        }
      >
        <Suspense
          fallback={
            <IssueHeaderSkeleton
              isClosed={isClosed}
              state={<WhatTheHeaderWouldSay artifact={primaryArtifact} issue={issue.data} />}
            />
          }
        >
          <IssueHeader
            documentArtifact={primaryArtifact}
            isClosed={isClosed}
            issue={issue.data}
            priorityWrite={priorityWrite}
            state={issueState}
          />
        </Suspense>
      </ChromeAfterTheFirstBatch>
      {/* The open document's connection dot stands in the strip's space until the strip itself
          lands, and in the box again while the strip's chunk arrives. A reader whose edits are
          in this browser alone is told so while the document is still connecting, which is
          exactly when no first batch comes to draw the strip (`issue-page-skeleton.tsx`,
          `features/doc/first-batch.ts`). */}
      <ChromeAfterTheFirstBatch
        label="Show the issue tabs"
        space={
          <IssueTabsSkeleton
            artifactCount={(issue.data.artifacts ?? []).length}
            trailing={
              activeTab === "spec" && specToolbar !== undefined ? (
                <ConnectionDot connection={specToolbar.connection} pending={specToolbar.pending} />
              ) : undefined
            }
          />
        }
      >
        <Suspense
          fallback={
            <IssueTabsSkeleton
              artifactCount={(issue.data.artifacts ?? []).length}
              trailing={
                activeTab === "spec" && specToolbar !== undefined ? (
                  <ConnectionDot
                    connection={specToolbar.connection}
                    pending={specToolbar.pending}
                  />
                ) : undefined
              }
            />
          }
        >
          <IssueTabs
            activeTab={activeTab}
            artifactCount={(issue.data.artifacts ?? []).length}
            issueKey={issueKey}
            onBeforeTabChange={(current) => {
              panelScroll.current[current] = whereTheReaderIs(theBoxThatScrollsIt());
            }}
            toolbar={
              activeTab === "spec" && specToolbar !== undefined ? (
                <SpecToolbar
                  isClosed={isClosed}
                  onShowDiffChange={setSpecShowDiff}
                  onVersionChange={(version) => {
                    setSpecShowDiff(false);
                    selectDocumentVersion(primaryArtifact, version);
                  }}
                  reference={documentRoute(
                    primaryArtifact,
                    isPrimaryArtifactRoute ? artifactRoute?.version : undefined
                  )}
                  showDiff={specShowDiff}
                  toolbar={specToolbar}
                  version={isPrimaryArtifactRoute ? artifactRoute?.version : undefined}
                />
              ) : undefined
            }
          />
        </Suspense>
      </ChromeAfterTheFirstBatch>
      <div
        aria-hidden={activeTab !== "spec"}
        aria-labelledby="issue-spec-tab"
        hidden={activeTab !== "spec"}
        id="issue-spec-panel"
        role="tabpanel"
      >
        {activatedTabs.spec && !(artifactRoute !== undefined && !isPrimaryArtifactRoute) ? (
          <div className="space-y-4">
            <Suspense fallback={null}>
              <ArtifactDocument
                artifact={primaryArtifact}
                askId={askId}
                commentId={commentId}
                highlightTerm={highlightTerm}
                isClosed={isClosed}
                onToolbarChange={handleSpecToolbarChange}
                owner={{ key: issueKey, kind: "issue" }}
                onVersionChange={(version) => {
                  setSpecShowDiff(false);
                  selectDocumentVersion(primaryArtifact, version);
                }}
                showDiff={specShowDiff}
                version={isPrimaryArtifactRoute ? artifactRoute?.version : undefined}
              />
            </Suspense>
            <Suspense fallback={null}>
              <SpecRequirements issueKey={issueKey} />
            </Suspense>
          </div>
        ) : null}
      </div>
      <RoleDocumentPanel
        Tab={VerificationTab}
        activated={activatedTabs.verification}
        active={activeTab === "verification"}
        artifact={verificationArtifact}
        isClosed={isClosed}
        issueKey={issueKey}
        onVersionChange={selectDocumentVersion}
        tab="verification"
      />
      <RoleDocumentPanel
        Tab={PlanTab}
        activated={activatedTabs.plan}
        active={activeTab === "plan"}
        artifact={planArtifact}
        isClosed={isClosed}
        issueKey={issueKey}
        onVersionChange={selectDocumentVersion}
        tab="plan"
      />
      <div
        aria-hidden={activeTab !== "conversation"}
        aria-labelledby="issue-conversation-tab"
        hidden={activeTab !== "conversation"}
        id="issue-conversation-panel"
        role="tabpanel"
      >
        {activatedTabs.conversation ? (
          <Suspense fallback={null}>
            <ConversationTab
              issueArtifacts={issueArtifacts}
              focusItemId={conversationFocusItemId}
              isClosed={isClosed}
              issueKey={issueKey}
              onFirstPlacement={forgetConversationScroll}
              state={state.data}
              visible={activeTab === "conversation"}
            />
          </Suspense>
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "children"}
        aria-labelledby="issue-children-tab"
        hidden={activeTab !== "children"}
        id="issue-children-panel"
        role="tabpanel"
      >
        {activatedTabs.children ? (
          <Suspense fallback={null}>
            <ChildrenTab issue={issue.data} />
          </Suspense>
        ) : null}
      </div>
      <ArtifactRoutePanel
        active={activeTab === "artifacts"}
        artifact={selectedArtifact}
        artifactRoute={artifactRoute}
        isClosed={isClosed}
        isPrimaryArtifactRoute={isPrimaryArtifactRoute}
        mounted={activatedTabs.artifacts}
        onShowDiffChange={setArtifactShowDiff}
        route={route}
        showDiff={artifactShowDiff}
        toolbar={artifactToolbar}
      >
        {selectedArtifact?.kind === "doc" ? (
          <Suspense fallback={null}>
            <ArtifactDocument
              artifact={selectedArtifact}
              askId={askId}
              commentId={commentId}
              highlightTerm={highlightTerm}
              isClosed={isClosed}
              onToolbarChange={handleArtifactToolbarChange}
              owner={{ key: issueKey, kind: "issue" }}
              onVersionChange={(version) => {
                setArtifactShowDiff(false);
                selectDocumentVersion(selectedArtifact, version);
              }}
              showDiff={artifactShowDiff}
              version={artifactRoute?.version}
            />
          </Suspense>
        ) : null}
      </ArtifactRoutePanel>
      <div
        aria-hidden={activeTab !== "activity"}
        aria-labelledby="issue-activity-tab"
        hidden={activeTab !== "activity"}
        id="issue-activity-panel"
        role="tabpanel"
      >
        {activatedTabs.activity ? (
          <Suspense fallback={null}>
            <ActivityTab issueKey={issueKey} state={state.data} />
          </Suspense>
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "decisions"}
        aria-labelledby="issue-decisions-tab"
        hidden={activeTab !== "decisions"}
        id="issue-decisions-panel"
        role="tabpanel"
      >
        {activatedTabs.decisions ? (
          <Suspense fallback={null}>
            <DecisionsPanel
              artifacts={issue.data?.artifacts ?? []}
              isClosed={isClosed}
              issueKey={issueKey}
            />
          </Suspense>
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "collaborators"}
        aria-labelledby="issue-collaborators-tab"
        hidden={activeTab !== "collaborators"}
        id="issue-collaborators-panel"
        role="tabpanel"
      >
        {activatedTabs.collaborators ? (
          <Suspense fallback={null}>
            <CollaboratorsTab issueKey={issueKey} />
          </Suspense>
        ) : null}
      </div>
    </section>
  );
}
