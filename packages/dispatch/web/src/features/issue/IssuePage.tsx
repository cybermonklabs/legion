import { type ReactNode, useCallback, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

import { ApiError } from "../../api/client";
import type { Artifact, IssuePriority } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { tabButtonId } from "../../components/Tabs";
import {
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ActivityTab } from "../activity/ActivityTab";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import { ArtifactRoutePanel } from "../artifacts/ArtifactRoutePanel";
import { ConversationTab } from "../conversation/ConversationTab";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { outsideAskCard } from "../inbox/ask-card";
import { PlanTab } from "../plan/PlanTab";
import {
  buildReferencePath,
  documentRoute,
  type IssueRoute,
  type IssueTab,
  parseIssuePath,
} from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import type { KeymapActionId } from "../shell/keymap-catalogue";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { StepsTab } from "../steps/StepsTab";
import { VerificationTab } from "../verification/VerificationTab";
import { ChildrenTab } from "./ChildrenTab";
import { CollaboratorsTab } from "./CollaboratorsTab";
import { DecisionsPanel } from "./DecisionsPanel";
import { usableControl } from "./header-control";
import { IssueHeader } from "./IssueHeader";
import { IssueTabs } from "./IssueTabs";
import { ItemMatchList } from "./ItemMatchList";
import { issuePriority } from "./issue-priority";
import { issueTabs } from "./issue-tab-list";
import { stateForIssue } from "./pins";
import { SpecRequirements } from "./SpecRequirements";
import { SpecToolbar } from "./SpecToolbar";
import { useIssueDetail } from "./useIssueDetail";
import { useIssuePriority } from "./useIssuePriority";
import { type ItemLanding, type ItemRoute, isItemRoute, useItemLanding } from "./useItemLanding";

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

/** The header controls the `issue` scope presses (`usableControl`, `header-control.ts`). */
const LABELS_TRIGGER = 'button[aria-label="Edit labels"]';
const PIN_TOGGLE = 'button[aria-label="Pin issue"], button[aria-label="Unpin issue"]';
const PRIORITY_MENU = "[data-priority-menu]";
const STATUS_MENU = "[data-status-menu]";
const TITLE_HEADING = 'h1[tabindex="0"]';

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

/** The tabs the chords reach, each through the tablist button `IssueTabs` renders for it. `t v`
 *  is Activity and `t c` the Conversation, so Verification takes its checks' `t k`; `t s` is the
 *  Spec, so Steps takes the next letter of its name, `t e`; `t a` is Artifacts and `l` is the
 *  issue's label editor, so Activity takes `t v`; and with `s k p e c h a v d` taken,
 *  Collaborators, the last tab, takes `t o` (`keymap-catalogue.ts`). */
const tabChords: readonly { action: KeymapActionId; tab: IssueTab }[] = [
  { action: "issue.tab-spec", tab: "spec" },
  { action: "issue.tab-verification", tab: "verification" },
  { action: "issue.tab-plan", tab: "plan" },
  { action: "issue.tab-steps", tab: "steps" },
  { action: "issue.tab-conversation", tab: "conversation" },
  { action: "issue.tab-children", tab: "children" },
  { action: "issue.tab-artifacts", tab: "artifacts" },
  { action: "issue.tab-activity", tab: "activity" },
  { action: "issue.tab-decisions", tab: "decisions" },
  { action: "issue.tab-collaborators", tab: "collaborators" },
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
  Tab: (props: {
    artifact: Artifact | undefined;
    children?: ReactNode;
    issueKey: string;
  }) => ReactNode;
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
  // a keyed priority lands in the same cache the picker writes. `IssueHeader` registers the keys
  // of its own actions (close, reopen, the priorities, Approve and Request changes) into this
  // same scope; it never pushes one of its own.
  const priorityWrite = useIssuePriority(route.key, issuePriority(issue.data?.priority));
  /** Selects the tab `step` places from the one shown, in the strip's order (`issueTabs`),
   *  through its button as a click does; past either end it wraps, as the strip's own arrow keys
   *  do (`Tabs.tsx`). */
  const stepTab = (step: 1 | -1) => {
    const tabs = issueTabs(issue.data?.artifacts?.length ?? 0);
    const at = tabs.findIndex((tab) => tab.id === activeTab);
    const next = tabs[(at + step + tabs.length) % tabs.length];
    if (next !== undefined) document.getElementById(tabButtonId("issue", next.id))?.click();
  };
  useKeymapScope("issue");
  useKeymap("issue", [
    {
      action: "issue.status",
      run: () => usableControl(STATUS_MENU)?.click(),
      when: () => usableControl(STATUS_MENU) !== null,
    },
    {
      action: "issue.priority",
      run: () => usableControl(PRIORITY_MENU)?.click(),
      when: () => usableControl(PRIORITY_MENU) !== null,
    },
    {
      // While the menu is open the focus is in it, whose keys are its own, so a digit there is
      // the menu's pick and never this second write.
      action: "issue.set-priority",
      run: (event) => priorityWrite.submit(Number(event.key) as IssuePriority),
      when: () => usableControl(PRIORITY_MENU) !== null && outsideInputsAndAskCards(),
    },
    {
      action: "issue.labels",
      run: () => usableControl(LABELS_TRIGGER)?.click(),
      when: () => usableControl(LABELS_TRIGGER) !== null,
    },
    {
      // The heading is a Tab stop only while the issue is open - closed, its `tabIndex` is -1
      // and it opens no editor - so the key is offered exactly while it edits. The key clicks
      // it, since the heading opens its editor when activated and not when it takes the focus.
      action: "issue.title",
      run: () => usableControl(TITLE_HEADING)?.click(),
      when: () => usableControl(TITLE_HEADING) !== null,
    },
    {
      action: "issue.pin",
      run: () => usableControl(PIN_TOGGLE)?.click(),
      when: () => usableControl(PIN_TOGGLE) !== null && outsideInputsAndAskCards(),
    },
    ...tabChords.map(({ action, tab }) => ({
      action,
      run: () => document.getElementById(tabButtonId("issue", tab))?.click(),
      when: () => document.getElementById(tabButtonId("issue", tab)) !== null,
    })),
    // Offered while the strip is drawn, which it is once the issue has been read.
    ...(
      [
        ["issue.next-tab", 1],
        ["issue.previous-tab", -1],
      ] as const
    ).map(([action, step]) => ({
      action,
      run: () => stepTab(step),
      when: () => document.getElementById(tabButtonId("issue", activeTab)) !== null,
    })),
  ]);

  useLayoutEffect(() => {
    const top = panelScroll.current[activeTab];
    if (top !== undefined) {
      window.scrollTo({ top });
    }
  }, [activeTab]);

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
    steps: activeTab === "steps",
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
    <section>
      <IssueHeader
        documentArtifact={primaryArtifact}
        isClosed={isClosed}
        issue={issue.data}
        priorityWrite={priorityWrite}
        state={issueState}
      />
      <IssueTabs
        activeTab={activeTab}
        artifactCount={(issue.data.artifacts ?? []).length}
        issueKey={issueKey}
        onBeforeTabChange={(current) => {
          panelScroll.current[current] = window.scrollY;
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
      <div
        aria-hidden={activeTab !== "spec"}
        aria-labelledby="issue-spec-tab"
        hidden={activeTab !== "spec"}
        id="issue-spec-panel"
        role="tabpanel"
      >
        {activatedTabs.spec && !(artifactRoute !== undefined && !isPrimaryArtifactRoute) ? (
          <div className="space-y-4">
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
            <SpecRequirements issueKey={issueKey} />
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
        aria-hidden={activeTab !== "steps"}
        aria-labelledby="issue-steps-tab"
        hidden={activeTab !== "steps"}
        id="issue-steps-panel"
        role="tabpanel"
      >
        {activatedTabs.steps ? (
          <StepsTab active={activeTab === "steps"} issueKey={issueKey} />
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "conversation"}
        aria-labelledby="issue-conversation-tab"
        hidden={activeTab !== "conversation"}
        id="issue-conversation-panel"
        role="tabpanel"
      >
        {activatedTabs.conversation ? (
          <ConversationTab
            issueArtifacts={issueArtifacts}
            focusItemId={conversationFocusItemId}
            isClosed={isClosed}
            issueKey={issueKey}
            onFirstPlacement={forgetConversationScroll}
            state={state.data}
            visible={activeTab === "conversation"}
          />
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "children"}
        aria-labelledby="issue-children-tab"
        hidden={activeTab !== "children"}
        id="issue-children-panel"
        role="tabpanel"
      >
        {activatedTabs.children ? <ChildrenTab issue={issue.data} /> : null}
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
        ) : null}
      </ArtifactRoutePanel>
      <div
        aria-hidden={activeTab !== "activity"}
        aria-labelledby="issue-activity-tab"
        hidden={activeTab !== "activity"}
        id="issue-activity-panel"
        role="tabpanel"
      >
        {activatedTabs.activity ? <ActivityTab issueKey={issueKey} state={state.data} /> : null}
      </div>
      <div
        aria-hidden={activeTab !== "decisions"}
        aria-labelledby="issue-decisions-tab"
        hidden={activeTab !== "decisions"}
        id="issue-decisions-panel"
        role="tabpanel"
      >
        {activatedTabs.decisions ? (
          <DecisionsPanel
            artifacts={issue.data?.artifacts ?? []}
            isClosed={isClosed}
            issueKey={issueKey}
          />
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "collaborators"}
        aria-labelledby="issue-collaborators-tab"
        hidden={activeTab !== "collaborators"}
        id="issue-collaborators-panel"
        role="tabpanel"
      >
        {activatedTabs.collaborators ? <CollaboratorsTab issueKey={issueKey} /> : null}
      </div>
    </section>
  );
}
