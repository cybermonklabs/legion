import { lazy, type ReactNode, Suspense } from "react";

import { NotFoundPage } from "../../shell/NotFoundPage";
import { useIssuePageChoice } from "../choice";

const RunPage = lazy(() => import("./RunPage").then((module) => ({ default: module.RunPage })));

/**
 * The route of a command bar run's own page, drawn only for a reader who chose the new issue
 * page. Any other reader gets the page an unknown address draws, and loads no word of the run's
 * page: `RunPage` is loaded with `lazy`, and this file imports no word list.
 */
export function RunRoute(): ReactNode {
  if (useIssuePageChoice()[0] !== "outline") {
    return <NotFoundPage />;
  }
  return (
    <Suspense fallback={null}>
      <RunPage />
    </Suspense>
  );
}
