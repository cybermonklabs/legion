/**
 * The draft matrix's coverage list, issue page (INFRA-1503): each composer census item that
 * `draft-matrix.issue-page.test.tsx` answers for, held to one of its tests or to one of the
 * numbered reasons below, as `__tests__/composer-census/coverage.ts` says.
 */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

/** The numbered reasons, said here alone, which the matrix file's header points to. */
const REASONS = {
  1: "1. `IssueDetail` draws an error or a loading line in place of the tabs while a read of the issue, the reader's issue state or an item link's landing fails or is pending, which ends the Conversation's mount as leaving the issue does; the composer it draws again on recovery is the one the issue-left row draws again on return.",
  2: "2. `IssuePage` draws the outline page in place of today's when the reader's stored page choice says so (`useIssuePageChoice`), which ends the Conversation's mount as leaving the issue does; choosing today's page again draws it afresh, as the issue-left row's return does.",
  3: "3. `activatedTabs` only ever gains a tab: the Conversation is drawn the first time its tab is active and stays drawn, so this conditional starts the mount and never ends one.",
  4: "4. `state`, which `IssueDetail` hands the Conversation, is the reader's read mark and pins for the issue (`stateForIssue`), which only the unread boundary, the read mark's dwell and each card's pin read, so it moves no composer's address and draws or ends no composer.",
  5: "5. An address under `/issues/` that parses as no issue draws the not-found page in place of `IssueDetail` (`route === undefined`), so no composer is drawn there, and going there from an issue is leaving the issue, the issue-left row.",
} as const;

export const coverage: readonly CoverageEntry[] = [
  ...answers("IssueDetail | ConversationTab | conditional | (activatedTabs.conversation) ? …", {
    reason: REASONS[3],
  }),
  ...answers(
    'IssueDetail | ConversationTab | conditional | <div hidden={activeTab !== "conversation"}>',
    cells("docked composer · another tab, then back"),
    cells("phone thread · another tab, then back")
  ),
  ...answers(
    "IssueDetail | ConversationTab | early return | if (issue.isError || state.isError) return …",
    { reason: REASONS[1] }
  ),
  ...answers(
    'IssueDetail | ConversationTab | early return | if (issue.isPending || state.isPending || landing?.kind === "pending") return …',
    { reason: REASONS[1] }
  ),
  ...answers(
    "IssueDetail | ConversationTab | early return | if (issue.data === undefined) return …",
    { reason: REASONS[1] }
  ),
  ...answers(
    'IssueDetail | ConversationTab | early return | if (landing?.kind === "redirect" || landing?.kind === "rewrite") return …',
    { reason: REASONS[1] }
  ),
  ...answers(
    'IssueDetail | ConversationTab | early return | if (isItemRoute(route) && landing?.kind === "ambiguous") return …',
    { reason: REASONS[1] }
  ),
  ...answers(
    'IssueDetail | ConversationTab | early return | if (isItemRoute(route) && (landing?.kind === "missing" || landing?.kind === "unavailable")) return …',
    { reason: REASONS[1] }
  ),
  ...answers(
    "IssueDetail | ConversationTab | early return | if (primaryArtifact === undefined) return …",
    { reason: REASONS[1] }
  ),
  ...answers(
    "IssueDetail | ConversationTab | address prop | issueKey={issueKey}",
    cells("docked composer · issue left, then back"),
    cells("phone thread · issue left, then back")
  ),
  ...answers(
    'IssueDetail | ConversationTab | address prop | visible={activeTab === "conversation"}',
    cells("docked composer · another tab, then back"),
    cells("phone thread · another tab, then back")
  ),
  ...answers("IssueDetail | ConversationTab | address prop | state={state.data}", {
    reason: REASONS[4],
  }),
  ...answers(
    "IssuePage | IssueDetail | key | <IssueDetail key={route.key}>",
    cells("docked composer · issue left, then back"),
    cells("phone thread · issue left, then back")
  ),
  ...answers("IssuePage | IssueDetail | early return | if (route === undefined) return …", {
    reason: REASONS[5],
  }),
  ...answers(
    "IssuePage | IssueDetail | address prop | route={route}",
    cells("docked composer · issue left, then back"),
    cells("phone thread · issue left, then back")
  ),
  ...answers('IssuePage | IssuePage | conditional | !(useIssuePageChoice()[0] === "outline") ? …', {
    reason: REASONS[2],
  }),
  ...answers(
    'AppShell | IssuePage | route | <Route path="/issues/:key/*">',
    cells("docked composer · issue left, then back"),
    cells("phone thread · issue left, then back")
  ),
];
