import { type FocusEvent, type ReactNode, useMemo } from "react";

import { primarySpec } from "../../api/issue-cache";
import type { Decision, IssueAskRow, IssueDetails } from "../../api/types";
import {
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pageQuietText,
} from "../../theme/classes";
import { cardPaths, fromDecision, withStatementTexts } from "../decision-card/adapters";
import { DecisionCard, type DecisionCardProps } from "../decision-card/DecisionCard";
import { WritableDecisionCard } from "../decision-card/WritableDecisionCard";
import { ApprovalChip } from "../doc/ApprovalChip";
import { useDecisionHeadingWords } from "../issue/decision-heading";
import { useSetFocusedNode } from "../issue-page/focused-node";
import { DETAILS_ATTRIBUTE } from "../issue-page/margin-slot";
import {
  DASHBOARD_WORDS,
  EMPTY_STATE_LINES,
  NODE_STATE_WORDS,
  PRESET_NAMES,
} from "../issue-page/page-words";
import { RowText } from "../issue-page/RowText";
import { buildDispatchReference } from "../refs/routes";
import { textSourceOf } from "../refs/text-references";
import { DashboardAskBody } from "./DashboardAskBody";
import { ThreadItem } from "./ThreadItem";
import type { DashboardNames } from "./useDashboardNames";
import type { WaitingItem } from "./useWaitingItems";

/** The page Dashboard is drawn on: today's issue page, as its first tab, or the new page, as its
 *  opening view. */
export type DashboardPage = "today" | "new";

/** How many items the list draws at a time (R4). */
export const WAITING_STEP = 5;

/** A question or an approval request, by the ask body, answered in place (`DashboardAskBody`).
 *  On the new page, the focus going inside the item sets the page's focused node to the ask, so
 *  Details draws that ask's facts where it draws a focused node's
 *  (dispatch://INFRA-5702/spec#b-r-reads); today's page draws no Details panel. */
function AskItem({
  ask,
  issueKey,
  page,
}: {
  ask: IssueAskRow;
  issueKey: string;
  page: DashboardPage;
}): ReactNode {
  const setFocusedNode = useSetFocusedNode();
  const body = <DashboardAskBody ask={ask} />;
  if (page === "today") return body;
  // The ask as the page's focused node reads it: the ask by its own id, its address, its words,
  // who asked it and how it stands, which is open for every ask the list holds.
  const focus = () =>
    setFocusedNode({
      author: ask.author,
      id: ask.id,
      item: { id: ask.id, kind: "ask" },
      reference: buildDispatchReference({ id: ask.id, key: issueKey, kind: "ask" }),
      source: textSourceOf("ask", ask.id, issueKey),
      state: NODE_STATE_WORDS.ask.open,
      text: ask.question,
      ...(ask.question_tree === undefined ? {} : { tree: ask.question_tree }),
      type: "ask",
    });
  // The focus leaving the item hands the page back to the issue itself, as the outline does
  // (`IssueTree.tsx`'s `useReleaseFocus`), unless it went to Details, where the reader reads the
  // ask's facts, or stays inside the item.
  const release = (event: FocusEvent<HTMLDivElement>) => {
    const to = event.relatedTarget;
    if (to instanceof Node && event.currentTarget.contains(to)) return;
    if (to instanceof Element && to.closest(`[${DETAILS_ATTRIBUTE}]`) !== null) return;
    setFocusedNode(undefined);
  };
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: this observes focus bubbling from the ask body's own controls to publish the page's focused node for Details; the wrapper itself is not interactive.
    <div onBlur={release} onFocus={focus}>
      {body}
    </div>
  );
}

/** An approval a newer version made stale, by the one approval control, in the page's words:
 *  the document by the page's word for it, the version approved and the newer one, and Approve
 *  and Request changes (R5). */
function StaleItem({
  document,
  issue,
}: {
  document: Extract<WaitingItem, { kind: "stale" }>["document"];
  issue: IssueDetails;
}): ReactNode {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className={`text-sm font-medium ${pageInkText}`}>
        {document.id === issue.primary_artifact_id ? PRESET_NAMES.spec : PRESET_NAMES.verification}
      </span>
      <ApprovalChip artifact={document} variant="header" />
    </div>
  );
}

/** A decision to review, by the decision card with its author line and level word on (R5), its
 *  text and heading drawn by `RowText`. A statement it shaped is drawn by the words the batched
 *  name read gives it, so the card reads no statement of its own (R8). */
function DecisionItem({
  decision,
  documentId,
  issue,
  names,
}: {
  decision: Decision;
  /** The issue's decisions document, which Keep and Change write to; none until it is known. */
  documentId: string | undefined;
  issue: IssueDetails;
  names: DashboardNames;
}): ReactNode {
  const source = textSourceOf("artifact", documentId, issue.key);
  const { reads, tree: headingTree, words: name } = useDecisionHeadingWords(decision, source);
  const shown = useMemo(
    () =>
      withStatementTexts(fromDecision(decision), (reference) => ({
        text: names.excerptOf(reference),
      })),
    [decision, names]
  );
  const props: DecisionCardProps = {
    authorLine: true,
    decision: shown,
    heading: {
      node: <RowText markdown={decision.heading} source={source} tabStops tree={headingTree} />,
      words: name,
    },
    paths: cardPaths(issue.key, primarySpec(issue)),
    renderText: (text) => (
      <RowText markdown={text.markdown} source={source} tabStops tree={text.tree} />
    ),
  };
  return (
    <>
      {reads}
      {documentId === undefined || issue.closed_at !== null ? (
        <DecisionCard {...props} />
      ) : (
        <WritableDecisionCard
          {...props}
          documentId={documentId}
          issueKey={issue.key}
          model={decision}
        />
      )}
    </>
  );
}

function ItemBody({
  decisionsDocument,
  issue,
  item,
  names,
  page,
  viewer,
}: {
  decisionsDocument: string | undefined;
  issue: IssueDetails;
  item: WaitingItem;
  names: DashboardNames;
  page: DashboardPage;
  viewer: string | undefined;
}): ReactNode {
  switch (item.kind) {
    case "question":
    case "approval":
      return <AskItem ask={item.ask} issueKey={issue.key} page={page} />;
    case "stale":
      return <StaleItem document={item.document} issue={issue} />;
    case "decision":
      return (
        <DecisionItem
          decision={item.decision}
          documentId={decisionsDocument}
          issue={issue}
          names={names}
        />
      );
    case "thread":
      return (
        <ThreadItem
          closed={issue.closed_at !== null}
          issueKey={issue.key}
          row={item.row}
          viewer={viewer}
        />
      );
  }
}

/**
 * Dashboard's second part, first and largest (dispatch://INFRA-5702/spec#b-r-waiting): what waits
 * on the reader under its count, which is the number of items. It draws five items at a time,
 * newest first, each whole and acted on in place, with a control that says how many are left and
 * draws the next five; where nothing waits, one line of the page's words says so. Until the reads
 * the items come from have answered, it draws neither a count nor a line.
 */
export function WaitingList({
  decisionsDocument,
  drawn,
  issue,
  items,
  names,
  onMore,
  page,
  viewer,
}: {
  /** The issue's decisions document, as the decisions read names it. */
  decisionsDocument: string | undefined;
  /** How many items the list draws, a step of `WAITING_STEP` at a time. */
  drawn: number;
  /** The issue, once its read has answered. */
  issue: IssueDetails | undefined;
  /** R4's items, newest first, once the reads they come from have answered. */
  items: readonly WaitingItem[] | undefined;
  names: DashboardNames;
  onMore: () => void;
  /** The page the list is drawn on. */
  page: DashboardPage;
  viewer: string | undefined;
}): ReactNode {
  const shown = items?.slice(0, drawn) ?? [];
  const left = (items?.length ?? 0) - shown.length;
  return (
    <section aria-label={DASHBOARD_WORDS.parts.waiting} className="space-y-3">
      <div className="flex items-baseline gap-3">
        {items === undefined ? null : (
          <p
            className={`text-3xl leading-none font-semibold tabular-nums ${pageInkText}`}
            data-testid="dashboard-waiting-count"
          >
            {items.length}
          </p>
        )}
        <h2 className={`text-base font-semibold ${pageInkText}`}>
          {DASHBOARD_WORDS.parts.waiting}
        </h2>
      </div>
      {items === undefined || issue === undefined ? null : items.length === 0 ? (
        <p className={`text-sm ${pageMutedText}`}>{EMPTY_STATE_LINES.dashboard}</p>
      ) : (
        <ol className="space-y-3">
          {shown.map((item) => (
            <li
              className={`rounded-xl border p-3 ${pageHairBorder}`}
              data-testid={`dashboard-item-${item.kind}-${item.id}`}
              key={`${item.kind}-${item.id}`}
              tabIndex={-1}
            >
              <ItemBody
                decisionsDocument={decisionsDocument}
                issue={issue}
                item={item}
                names={names}
                page={page}
                viewer={viewer}
              />
            </li>
          ))}
        </ol>
      )}
      {left > 0 ? (
        <button
          className={`rounded-lg border px-3 py-1 text-sm font-medium ${pageLineBorder} ${pageMutedText}`}
          onClick={onMore}
          type="button"
        >
          {DASHBOARD_WORDS.next_five}
          <span className={pageQuietText}>{` · ${left} ${DASHBOARD_WORDS.left}`}</span>
        </button>
      ) : null}
    </section>
  );
}
