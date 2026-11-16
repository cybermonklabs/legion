import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";

import { ApiError } from "../../api/client";
import { issueQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
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
import type { SlotProps } from "./slot-props";
import { ContentsSlot } from "./slots/contents";
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

  useDocumentTitle(
    issue.data === undefined ? "Dispatch" : `${issue.data.key} · ${issue.data.title} · Dispatch`
  );

  if (issue.isError) {
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
    return <p className={textMutedOnCanvas}>Loading issue…</p>;
  }

  const props: SlotProps = { issue: issue.data, route };
  return (
    <ProvidersSlot {...props}>
      <HeaderSlot {...props} />
      <ViewRowSlot {...props} />
      <StageSlot {...props} />
      <FootSlot {...props} />
      <DetailsSlot {...props} />
      <ContentsSlot {...props} />
    </ProvidersSlot>
  );
}
