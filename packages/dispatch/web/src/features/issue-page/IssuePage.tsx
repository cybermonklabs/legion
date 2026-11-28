import { lazy, type ReactNode } from "react";
import { useParams } from "react-router-dom";

import { PlannedChanges } from "../going/PlannedChanges";
import { IssuePage as TodaysIssuePage } from "../issue/IssuePage";
import { useIssuePageChoice } from "./choice";

const IssueOutlinePage = lazy(() =>
  import("./IssueOutlinePage").then((module) => ({ default: module.IssueOutlinePage }))
);

/**
 * The page an issue's address draws: the new page for a reader who chose it, else today's, under
 * the open conversation's planned changes of the issue itself (`PlannedChanges`), whichever page
 * draws it.
 *
 * `app.tsx` loads this file with `lazy`, and the build names a lazy chunk after the module that
 * `import()` names, so this file is `IssuePage.tsx` and exports `IssuePage`: the chunk keeps the
 * name `IssuePage-<hash>.js`, and the browser rows that abort or hold `/assets/IssuePage-*.js`
 * keep their meaning. Today's page is imported statically, so that chunk holds it as it does
 * now; the new page is loaded with `lazy`, so a reader with the setting off downloads nothing of
 * it, and the app's `Suspense` shows today's skeleton while its chunk loads.
 */
export function IssuePage(): ReactNode {
  const { key = "" } = useParams();
  const [choice] = useIssuePageChoice();
  return (
    <>
      <PlannedChanges surface={{ issue: key, kind: "issue" }} />
      {choice === "outline" ? <IssueOutlinePage /> : <TodaysIssuePage />}
    </>
  );
}
