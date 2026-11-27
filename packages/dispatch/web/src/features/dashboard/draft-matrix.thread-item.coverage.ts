/** Dashboard's composer census items and their rows in `draft-matrix.thread-item.test.tsx`. */
import { answers, type CoverageEntry, cells } from "../../__tests__/composer-census/coverage";

const HOST = "Dashboard thread item";
const TYPED = ["typed"];
const REPLY = "ReplyThread | ReplyThread <MentionComposer mutationKey={replySendKey(root.id)}>";
const ASK = "AskThreadItem | AskThreadItem <AskReplyComposer>";
const ANSWER = "AskBody <MentionField askAnswer={true}> | address prop | useAskAnswerForm({ ask })";

const REASONS = {
  1: "1. An item belongs to its issue. Changing issues on today's page remounts IssueDetail, not the owner of a mounted composer. The return to an issue draws its rows again, as the list-removal row does.",
  2: "2. `IssueDetail` draws an error or a loading line in place of the tabs while a read of the issue, the reader's issue state or an item link's landing fails or is pending, which ends Dashboard's mount as leaving the issue does; Dashboard drawn again on recovery draws each item at its own thread or ask, as the list-removal row's return does.",
  3: "3. `activatedTabs` only ever gains a tab: Dashboard is drawn the first time its tab is active and stays drawn, so this conditional starts the mount and never ends one.",
  4: "4. Another tab of today's page hides Dashboard's panel (`hidden`) and ends no mount: the waiting items and their composers stay drawn inside the hidden panel with what they hold.",
  5: "5. On the new issue page an item belongs to its issue too: changing issues remounts the page's providers (`ProvidersSlot`'s `key={issue.key}`) and everything under them, not the owner of a mounted composer, and the return to an issue draws its rows again, as the list-removal row does.",
  6: "6. Another view of the new page draws in Dashboard's place (`StageSlot`: `View`, and `ViewStage`'s `key={preset}`), which ends Dashboard's mount as leaving the issue does; Dashboard drawn again draws each item at its own thread or ask, as the list-removal row's return does.",
  7: "7. The new page draws its failed or loading line in place of the stage while the issue's read fails or is pending (`IssueOutline`), which ends Dashboard's mount as leaving the issue does; Dashboard drawn again on recovery draws each item at its own thread or ask, as the list-removal row's return does.",
} as const;

const issueAddress = { reason: REASONS[1] };
const todayFailed = { reason: REASONS[2] };
const newPageIssue = { reason: REASONS[5] };
const otherView = { reason: REASONS[6] };
const newPageFailed = { reason: REASONS[7] };
const reads = (kind: string) =>
  cells(`${HOST} · a thread read waits, then answers · ${kind}`, TYPED);
const pages = (kind: string) => cells(`${HOST} · the next five are shown · ${kind}`, TYPED);
const returns = (kind: string) =>
  cells(`${HOST} · an item leaves the list, then returns · ${kind}`, TYPED);
const allReturns = ["comment", "message", "ask"].flatMap(returns);
const allPages = ["comment", "message", "ask"].flatMap(pages);
const sameAsk = { row: `${HOST} · an answer stays with its ask across waiting items · typed` };

export const coverage: readonly CoverageEntry[] = [
  ...answers(
    `${REPLY} | address prop | mutationKey={replySendKey(root.id)}`,
    returns("comment"),
    returns("message")
  ),
  ...answers(`${REPLY} | address prop | owner={{ issueKey, kind: "issue" }}`, issueAddress),
  ...answers(
    `${REPLY} | address prop | replyTo={{ actor: root.author, excerpt: "", id: root.id, parentKind, ...(thread === undefined ? {} : { thread }), }}`,
    returns("comment"),
    returns("message")
  ),
  ...answers(
    `${REPLY} | conditional | !(postDeletion(root) !== null) ? …`,
    cells(`${HOST} · the root is deleted · comment`, TYPED),
    cells(`${HOST} · the root is deleted · message`, TYPED)
  ),
  ...answers(
    `${REPLY} | lifetime | <MentionComposer mutationKey={replySendKey(root.id)}>`,
    cells(`${HOST} · the page reloaded, then back · comment`),
    cells(`${HOST} · the page reloaded, then back · message`)
  ),
  ...answers(
    "CommentThread | ReplyThread | early return | if (read === undefined) return …",
    reads("comment")
  ),
  ...answers("CommentThread | ReplyThread | address prop | issueKey={issueKey}", issueAddress),
  ...answers(
    'CommentThread | ReplyThread | address prop | parentKind="comment"',
    returns("comment")
  ),
  ...answers("CommentThread | ReplyThread | address prop | root={root}", returns("comment")),
  ...answers(
    "MessageThread | ReplyThread | early return | if (read === undefined) return …",
    reads("message")
  ),
  ...answers("MessageThread | ReplyThread | address prop | issueKey={issueKey}", issueAddress),
  ...answers(
    'MessageThread | ReplyThread | address prop | parentKind="message"',
    returns("message")
  ),
  ...answers("MessageThread | ReplyThread | address prop | root={root}", returns("message")),
  ...answers(
    'MessageThread | ReplyThread | address prop | thread={ target === null || target === undefined ? undefined : { delivery: deliveryMode(last?.delivery ?? "") ?? "steer", target } }',
    returns("message")
  ),

  ...answers(`${ASK} | address prop | ask={ask}`, returns("ask")),
  ...answers(
    `${ASK} | conditional | (takesReply(ask)) ? …`,
    cells(`${HOST} · the ask opens again, then settles`, TYPED)
  ),
  ...answers(
    `${ASK} | lifetime | <AskReplyComposer>`,
    cells(`${HOST} · the page reloaded, then back · ask`, TYPED)
  ),
  ...answers(`${ASK} | early return | if (read === undefined) return …`, reads("ask")),
  ...answers(`DashboardAskBody | ${ANSWER}`, sameAsk),

  ...answers(
    'ThreadItem | CommentThread | conditional | switch (row.kind) case "comment"',
    returns("comment")
  ),
  ...answers("ThreadItem | CommentThread | address prop | issueKey={issueKey}", issueAddress),
  ...answers("ThreadItem | CommentThread | address prop | row={row}", returns("comment")),
  ...answers(
    'ThreadItem | MessageThread | conditional | switch (row.kind) case "message"',
    returns("message")
  ),
  ...answers("ThreadItem | MessageThread | address prop | issueKey={issueKey}", issueAddress),
  ...answers("ThreadItem | MessageThread | address prop | row={row}", returns("message")),
  ...answers(
    'ThreadItem | AskThreadItem | conditional | switch (row.kind) case "ask"',
    returns("ask")
  ),
  ...answers("ThreadItem | AskThreadItem | address prop | row={row}", returns("ask")),
  ...answers('ItemBody | ThreadItem | conditional | switch (item.kind) case "thread"', allReturns),
  ...answers("ItemBody | ThreadItem | address prop | issueKey={issue.key}", issueAddress),
  ...answers("ItemBody | ThreadItem | address prop | row={item.row}", allReturns),

  ...answers(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: the census key contains the JSX key's source text.
    "WaitingList | ItemBody | key | <li key={`${item.kind}-${item.id}`}>",
    allPages,
    allReturns
  ),
  ...answers("WaitingList | ItemBody | conditional | drawn by shown.map(…)", allPages, allReturns),
  ...answers("WaitingList | ItemBody | conditional | !(items.length === 0) ? …", allReturns),
  ...answers(
    "WaitingList | ItemBody | conditional | !(items === undefined || issue === undefined) ? …",
    allReturns
  ),
  ...answers("WaitingList | ItemBody | address prop | issue={issue}", issueAddress),
  ...answers("WaitingList | ItemBody | address prop | item={item}", allReturns),
  ...answers("DashboardTab | WaitingList | address prop | drawn={drawn}", allPages),
  ...answers("DashboardTab | WaitingList | address prop | issue={issue.data}", issueAddress),
  ...answers(
    "DashboardTab | WaitingList | address prop | items={loaded ? items : undefined}",
    allReturns
  ),

  ...answers("IssueDetail | DashboardTab | conditional | (activatedTabs.dashboard) ? …", {
    reason: REASONS[3],
  }),
  ...answers(
    'IssueDetail | DashboardTab | conditional | <div hidden={activeTab !== "dashboard"}>',
    {
      reason: REASONS[4],
    }
  ),
  ...answers(
    "IssueDetail | DashboardTab | early return | if (issue.isError || state.isError) return …",
    todayFailed
  ),
  ...answers(
    'IssueDetail | DashboardTab | early return | if (issue.isPending || state.isPending || landing?.kind === "pending") return …',
    todayFailed
  ),
  ...answers(
    "IssueDetail | DashboardTab | early return | if (issue.data === undefined) return …",
    todayFailed
  ),
  ...answers(
    'IssueDetail | DashboardTab | early return | if (landing?.kind === "redirect" || landing?.kind === "rewrite") return …',
    todayFailed
  ),
  ...answers(
    'IssueDetail | DashboardTab | early return | if (isItemRoute(route) && landing?.kind === "ambiguous") return …',
    todayFailed
  ),
  ...answers(
    'IssueDetail | DashboardTab | early return | if (isItemRoute(route) && (landing?.kind === "missing" || landing?.kind === "unavailable")) return …',
    todayFailed
  ),
  ...answers(
    "IssueDetail | DashboardTab | early return | if (primaryArtifact === undefined) return …",
    todayFailed
  ),
  ...answers("IssueDetail | DashboardTab | address prop | issueKey={issueKey}", issueAddress),

  ...answers(
    "ViewDashboardSlot | DashboardTab | address prop | issueKey={issue.key}",
    newPageIssue
  ),
  ...answers(
    "StageSlot | ViewDashboardSlot | address prop | <ViewDashboardSlot {...props}>",
    newPageIssue
  ),
  ...answers("StageSlot | ViewDashboardSlot | key | <ViewStage key={preset}>", otherView),
  ...answers("StageSlot | ViewDashboardSlot | conditional | !(View === undefined) ? …", otherView),
  ...answers("IssueOutline | StageSlot | address prop | <StageSlot {...props}>", newPageIssue),
  ...answers(
    "ProvidersSlot | StageSlot | key | <FocusedNodeProvider key={issue.key}>",
    newPageIssue
  ),
  ...answers(
    "IssueOutline | StageSlot | early return | if (issue.isError) return …",
    newPageFailed
  ),
  ...answers(
    "IssueOutline | StageSlot | early return | if (issue.data === undefined) return …",
    newPageFailed
  ),
];
