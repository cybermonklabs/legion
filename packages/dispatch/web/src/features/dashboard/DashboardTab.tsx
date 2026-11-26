import { useQueries, useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { legionIssueQuery } from "../../api/legion-issue";
import {
  handoffsQuery,
  issueCoverageQuery,
  issueQuery,
  issueResultsQuery,
  whoAmIQuery,
} from "../../api/queries";
import { pageHairBorder, pageInkText, pagePaperBg } from "../../theme/classes";
import { decisionsDocumentId } from "../decision-card/change-anchor";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { NoTooltips } from "../issue-page/NoTooltips";
import { PublishedReferenceNames } from "../refs/reference-names";
import { EVENT_FRESH } from "../refs/reference-target";
import { buildDispatchReference } from "../refs/routes";
import { Agents } from "./Agents";
import { PhaseTrack } from "./PhaseTrack";
import { Tiles } from "./ProgressTiles";
import { threadQuery } from "./ThreadItem";
import { useDashboardNames } from "./useDashboardNames";
import { openAsksQuery, useWaitingItems } from "./useWaitingItems";
import { type DashboardPage, WAITING_STEP, WaitingList } from "./WaitingList";

export interface DashboardTabProps {
  readonly issueKey: string;
  /** The page Dashboard is drawn on: today's issue page, as its first tab, or the new page, as
   *  its opening view. */
  readonly page: DashboardPage;
}

/** What a thread's own read holds of the texts its item draws. */
function threadTexts(read: unknown): readonly string[] {
  if (typeof read !== "object" || read === null) return [];
  const bodies = (writes: unknown): string[] =>
    Array.isArray(writes)
      ? writes.flatMap((write: { body?: unknown }) =>
          typeof write.body === "string" ? [write.body] : []
        )
      : [];
  const replies = "replies" in read ? bodies(read.replies) : [];
  if ("comment" in read) return [...bodies([read.comment]), ...replies];
  if ("message" in read) return [...bodies([read.message]), ...replies];
  if (
    "ask" in read &&
    typeof read.ask === "object" &&
    read.ask !== null &&
    "question" in read.ask
  ) {
    return typeof read.ask.question === "string" ? [read.ask.question, ...replies] : replies;
  }
  return replies;
}

/** What the reads of the threads drawn answer, which Dashboard waits on before it asks for the
 *  names they cite: whether any is still out, and the texts they hold. One function for every
 *  render, so `useQueries` combines the answers again only when they change. */
function threadReads(answers: readonly { readonly data: unknown; readonly isPending: boolean }[]) {
  return {
    pending: answers.some((answer) => answer.isPending),
    texts: answers.flatMap((answer) => threadTexts(answer.data)),
  };
}

/**
 * Dashboard (dispatch://INFRA-5702/spec#b-r-today-tab, #b-r-new-page): one screen of where the
 * issue stands for the reader, drawn from the top as R3, R4, R6 and R7 say - the phase track,
 * what waits on the reader, the four tiles and the agents at work - inside `NoTooltips`, so its
 * words and colours are the issue page's; on today's page it carries no `data-issue-page`, and on
 * the new page it is that page's opening view (`issue-page/slots/view-dashboard.tsx`). Its text
 * is drawn with `RowText`, and the names of what that text cites come from its own batched read,
 * which it publishes into the provider the app mounts (`useDashboardNames`). It is the default
 * export a `lazy` import loads, a chunk of its own that the browser fetches only when the reader
 * opens it.
 */
export default function DashboardTab({ issueKey, page }: DashboardTabProps): ReactNode {
  const issue = useQuery({ ...issueQuery(issueKey), ...EVENT_FRESH });
  const legion = useQuery(legionIssueQuery(issueKey)).data;
  const handoffs = useQuery({ ...handoffsQuery(issueKey), ...EVENT_FRESH }).data;
  const viewer = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH });
  const asks = useQuery({ ...openAsksQuery(issueKey), ...EVENT_FRESH });
  const decisions = useQuery({ ...issueDecisionsQuery(issueKey, true), ...EVENT_FRESH });
  const results = useQuery({ ...issueResultsQuery(issueKey), ...EVENT_FRESH });
  const coverage = useQuery({ ...issueCoverageQuery(issueKey), ...EVENT_FRESH });
  // The list counts and draws once each read its items come from has answered (`loaded`), so no
  // item is drawn that leaves, and no count stands, as a read lands.
  const { items, loaded } = useWaitingItems(issueKey);
  const [drawn, setDrawn] = useState(WAITING_STEP);
  const shownThreads = items
    .slice(0, drawn)
    .flatMap((item) => (item.kind === "thread" ? [item.row] : []));
  const threads = useQueries({
    queries: shownThreads.map((row) => threadQuery(issueKey, row)),
    combine: threadReads,
  });
  const texts = useMemo(
    () => [
      ...items.flatMap((item) => {
        switch (item.kind) {
          case "question":
          case "approval":
            return [
              item.ask.question,
              ...(item.ask.options ?? []).flatMap((option) => [
                option.label,
                option.description ?? "",
              ]),
            ];
          case "decision":
            return [item.decision.heading, item.decision.text];
          default:
            return [];
        }
      }),
      ...threads.texts,
    ],
    [items, threads.texts]
  );
  const references = useMemo(
    () => [
      ...items.flatMap((item) =>
        item.kind === "decision"
          ? (item.decision.links ?? []).flatMap((link) =>
              link.to.ref === undefined ? [] : [link.to.ref]
            )
          : []
      ),
      ...(coverage.data?.requirements ?? []).map((statement) => statement.ref),
      ...(results.data?.checks ?? []).map((check) => check.ref),
      ...(issue.data?.children ?? []).map((child) =>
        buildDispatchReference({ key: child.key, kind: "issue" })
      ),
      ...(asks.data ?? []).map((ask) =>
        buildDispatchReference({ id: ask.id, key: issueKey, kind: "ask" })
      ),
    ],
    [asks.data, coverage.data, issue.data, issueKey, items, results.data]
  );
  const names = useDashboardNames(issue.data, {
    references,
    settled: loaded && results.data !== undefined && !threads.pending,
    texts,
  });
  return (
    <NoTooltips>
      <PublishedReferenceNames published={names.published}>
        <div
          className={`space-y-6 rounded-xl border p-4 md:p-5 ${pageHairBorder} ${pagePaperBg} ${pageInkText}`}
        >
          <PhaseTrack handoffs={handoffs ?? undefined} legion={legion} />
          <WaitingList
            decisionsDocument={decisionsDocumentId(issue.data?.artifacts, decisions.data)}
            drawn={drawn}
            issue={issue.data}
            items={loaded ? items : undefined}
            names={names}
            onMore={() => setDrawn((count) => count + WAITING_STEP)}
            page={page}
            viewer={viewer.data?.login}
          />
          {issue.data === undefined ? null : (
            <Tiles
              asks={asks.data}
              coverage={coverage.data}
              issue={issue.data}
              results={results.data}
              viewer={viewer.data?.login}
            />
          )}
          <Agents legion={legion} />
        </div>
      </PublishedReferenceNames>
    </NoTooltips>
  );
}
