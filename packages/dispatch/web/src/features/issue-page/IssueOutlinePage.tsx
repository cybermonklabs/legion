import {
  type DefaultError,
  type FetchQueryOptions,
  type QueryClient,
  type QueryKey,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { type ReactNode, useEffect, useState } from "react";
import { Link, useLocation } from "react-router-dom";

import { ApiError } from "../../api/client";
import {
  issueAskThreadsQuery,
  issueCoverageQuery,
  issueQuery,
  issueResultsQuery,
  whoAmIQuery,
} from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { DOCUMENT_SCROLL_ATTRIBUTE } from "../../hooks/useFootInset";
import {
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { type IssueRoute, parseIssuePath } from "../refs/routes";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { IssueBlockRoom } from "./block-room";
import { IssueContentsPane, PAGE_INSET } from "./contents-frame";
import { useDrawerPage } from "./drawer-page";
import { presetOf } from "./issue-address";
import { IssueTreeOwnerProvider } from "./issue-tree-owner";
import type { SlotProps } from "./slot-props";
import { DetailsSlot } from "./slots/details";
import { FootSlot } from "./slots/foot";
import { HeaderSlot } from "./slots/header";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { ViewRowSlot } from "./slots/view-row";
import { outlineOf, outlineQuery } from "./tree-read/tree-read";
import { issueMessagesQuery } from "./useIssueTree";

/** The new issue page's root, drawn at an issue's address for a reader who chose it. Everything
 *  it draws, a refused address included, is inside the element that carries `data-issue-page`,
 *  so a row can tell from the first frame which page drew a state. */
export function IssueOutlinePage(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  return (
    <div data-issue-page="">
      {route === undefined ? (
        <NotFoundPage />
      ) : (
        // key={route.key}: a move to another issue starts every slot fresh, as today's page
        // keys `IssueDetail`.
        <IssueOutline key={route.key} route={route} />
      )}
    </div>
  );
}

/**
 * The page of one issue. A failed read draws the failed state whether or not an issue is held,
 * then, while no issue is held, the loading line, and only a loaded issue mounts the slots. A
 * background refetch keeps the issue, so the slots stay mounted through it.
 *
 * The words of the failed, missing and loading states are copied from today's `IssueDetail` in
 * `features/issue/IssuePage.tsx`, because R33 of INFRA-1196 changes no file today's page uses
 * but by adding. The copy goes with INFRA-1196's last step, which removes today's page.
 */
function IssueOutline({ route }: { route: IssueRoute }): ReactNode {
  const issue = useQuery(issueQuery(route.key));
  // The reads the outline's rows and the page's first focus are built from go out as the page
  // mounts, beside the issue read, rather than as the slots mount once it has answered
  // (`startReadsAtOpen`). They are the reads of the address the page opened at: the page mounts
  // again for another issue (`key`), and a view the reader moves to reads what it needs as its
  // slots draw it.
  const queryClient = useQueryClient();
  const [opened] = useState(route);
  useEffect(() => startReadsAtOpen(queryClient, opened), [opened, queryClient]);
  // While the page is mounted, the shell's Details drawer draws its head in the page's words and
  // lands an address on its cards by the page's rule.
  useDrawerPage(issue.data);

  useDocumentTitle(
    issue.data === undefined ? "Dispatch" : `${issue.data.key} · ${issue.data.title} · Dispatch`
  );

  // The shell gives the page's column no padding of its own (`app.tsx`), so what the page draws
  // in place of its slots takes the inset its header would.
  if (issue.isError) {
    const notFound = issue.error instanceof ApiError && issue.error.status === 404;
    return (
      <section className={`pt-2 ${PAGE_INSET}`}>
        <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>
          {notFound ? "Issue not found" : "Couldn't load this issue"}
        </h1>
        {notFound ? (
          <p className={`mt-2 text-sm ${textSecondaryOnCanvas}`}>
            {route.key} doesn&apos;t exist, or you don&apos;t have access to it.
          </p>
        ) : (
          <div className="mt-2">
            <QueryError message="Couldn't load this issue." onRetry={() => void issue.refetch()} />
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
  if (issue.data === undefined) {
    return <p className={`pt-2 ${PAGE_INSET} ${textMutedOnCanvas}`}>Loading issue…</p>;
  }

  // The slots, each given the same props: the header and the view row across the page; then the
  // Contents pane against the rail beside Conversation, each filter and Graph
  // (`contents-frame.tsx`), beside the one column that holds the stage and the foot, 10 px under
  // the row and inset 28 px each side (14 px below 768 px), at least the screen's height so the
  // foot stands at the screen's foot however short the stage is; then Details' slot, whose fill
  // the shell draws in its drawer (R14). At 1440 px, beside the rail of 56 px, the page is 1384 px
  // wide: in a view with no pane the stage has 1328 px inside its insets, and beside the 260 px
  // pane 1068 px.
  // The column carries `data-document-scroll`, so the foot's end room for the floats goes here
  // (`FOOT_LAYERS` in `hooks/useFootInset.ts` says why).
  // The tree's owner stands around the slots, inside the page's providers, so the issue's tree is
  // built once for the page and every slot that draws from it reads that one
  // (`issue-tree-owner.tsx`); a press on a view mounts no reader that builds it again.
  const props: SlotProps = { issue: issue.data, route };
  return (
    <ProvidersSlot {...props}>
      <IssueTreeOwnerProvider issue={issue.data}>
        <IssueBlockRoom issue={issue.data} />
        <HeaderSlot {...props} />
        <ViewRowSlot {...props} />
        <div className="flex min-w-0 items-start">
          <IssueContentsPane {...props} />
          <div
            className={`flex min-h-dvh min-w-0 flex-1 flex-col pt-2.5 ${PAGE_INSET}`}
            data-testid="issue-column"
            {...{ [DOCUMENT_SCROLL_ATTRIBUTE]: "" }}
          >
            <StageSlot {...props} />
            <FootSlot {...props} />
          </div>
        </div>
        <DetailsSlot {...props} />
      </IssueTreeOwnerProvider>
    </ProvidersSlot>
  );
}

/**
 * Sends, as the page mounts, the reads the outline's rows and the page's first focus are built
 * from, so none of them waits for the issue read: the page mounts no slot until that read
 * answers, and each slot's hooks sent these reads only as the slot mounted, so on a cold load
 * each went out after the issue read had come back. They are the tree read of the view `route`
 * opens, which answers the comments too, the five reads `useIssueTree` builds the tree from beside
 * it - the coverage, the decisions, the results, the asks and the messages - and the reader's
 * login. Each is sent under the key and with the function its hook reads it by, so the hook, once
 * mounted, finds it out or answered and sends nothing of its own: a load sends each of them once,
 * as before, only sooner.
 *
 * A read the cache holds already - answered, refused or out - is left to its hook, which reads it
 * as it read it before (a refused tree read, for one, is not read again by a new observer,
 * `TREE_FRESH`), so nothing is sent here that the page would not send. The tree read is the
 * outline read the address's view sends with the load (`tree-read.ts`' `OutlineChoice.sends`):
 * Graph's in Graph, Conversation's in Conversation and in each filter but Spec, whose own view
 * read holds what it draws, and none in a view that holds no outline. A filter's own view read is
 * the stage's to send (`ViewReadProvider`), which first drops the views a past load of the issue
 * held: one sent here would be dropped and read a second time.
 */
function startReadsAtOpen(queryClient: QueryClient, route: IssueRoute): void {
  const { key } = route;
  const preset = presetOf(route);
  const outline = outlineOf(preset);
  if (outline !== undefined && preset !== "spec") {
    startRead(queryClient, outlineQuery(key, outline));
  }
  startRead(queryClient, issueCoverageQuery(key));
  startRead(queryClient, issueDecisionsQuery(key, true));
  startRead(queryClient, issueResultsQuery(key));
  startRead(queryClient, issueAskThreadsQuery(key));
  startRead(queryClient, issueMessagesQuery(key));
  startRead(queryClient, whoAmIQuery());
}

/** Sends `read` where the cache holds no query under its key (`startReadsAtOpen`). */
function startRead<TData, TKey extends QueryKey>(
  queryClient: QueryClient,
  read: FetchQueryOptions<TData, DefaultError, TData, TKey>
): void {
  if (queryClient.getQueryState(read.queryKey) === undefined) {
    void queryClient.prefetchQuery(read);
  }
}
