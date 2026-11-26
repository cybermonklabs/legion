import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";

import { ApiError } from "../../api/client";
import { issueQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { DOCUMENT_SCROLL_ATTRIBUTE } from "../../hooks/useFootInset";
import {
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { type IssueRoute, parseIssuePath } from "../refs/routes";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { IssueContentsPane, PAGE_INSET } from "./contents-frame";
import { useDrawerPage } from "./drawer-page";
import type { SlotProps } from "./slot-props";
import { DetailsSlot } from "./slots/details";
import { FootSlot } from "./slots/foot";
import { HeaderSlot } from "./slots/header";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { ViewRowSlot } from "./slots/view-row";

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
  // While the page is mounted, the shell's Details drawer draws its head in the page's words and
  // lands an address on its cards by the page's rule.
  useDrawerPage(issue.data);

  // The title bar holds no markup, so an issue's key and title go in as the characters they
  // are; the reading list's `issue-outline-page-title` (`DISPATCH_READING_SURFACES`) says why.
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
  const props: SlotProps = { issue: issue.data, route };
  return (
    <ProvidersSlot {...props}>
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
    </ProvidersSlot>
  );
}
