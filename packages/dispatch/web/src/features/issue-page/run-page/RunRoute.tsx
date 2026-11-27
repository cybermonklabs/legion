import { useQuery } from "@tanstack/react-query";
import { lazy, type ReactNode, Suspense } from "react";
import { useLocation } from "react-router-dom";

import { whoAmIQuery } from "../../../api/queries";
import { NotFoundPage } from "../../shell/NotFoundPage";
import { useIssuePageChoice } from "../choice";

const RunPage = lazy(() => import("./RunPage").then((module) => ({ default: module.RunPage })));

/** The query parameter that opens the new issue page, and the one value of it that does; any
 *  other value is as no parameter. These are twins of `new-page-parameter.tsx`'s `PARAMETER` and
 *  `NEW_PAGE`, which that file keeps to itself: they are not shared, since exporting them would
 *  change a line of that file, and R6 admits none this round. A run's row writes them into its
 *  link (`run-references.ts`'s `NEW_PAGE_SEARCH`). */
const PARAMETER = "issue_page";
const NEW_PAGE = "new";

/**
 * The route of a command bar run's own page, drawn for a reader who chose the new issue page,
 * and for a signed-in reader at a run's address that carries `?issue_page=new`, as an issue's
 * address that carries it draws the new page for them (`useIssuePageChoice`); nothing is stored
 * for it, and the run's address without it follows the setting. Any other reader gets the page
 * an unknown address draws, and loads no word of the run's page: `RunPage` is loaded with
 * `lazy`, and this file imports no word list.
 */
export function RunRoute(): ReactNode {
  const [choice] = useIssuePageChoice();
  // The sign-in read `useIssuePageChoice` reads, as it reads it.
  const login = useQuery(whoAmIQuery()).data?.login;
  const { search } = useLocation();
  const opened = login !== undefined && new URLSearchParams(search).get(PARAMETER) === NEW_PAGE;
  if (choice !== "outline" && !opened) {
    return <NotFoundPage />;
  }
  return (
    <Suspense fallback={null}>
      <RunPage />
    </Suspense>
  );
}
