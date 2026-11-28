import { lazy, type ReactNode } from "react";

import { IssuePage as TodaysIssuePage } from "../issue/IssuePage";
import { useIssuePageChoice } from "./choice";

const IssueOutlinePage = lazy(() =>
  import("./IssueOutlinePage").then((module) => ({ default: module.IssueOutlinePage }))
);

/**
 * The page an issue's address draws: the new page for a reader who chose it, else today's.
 *
 * `app.tsx` loads this file with `lazy`, and the build names a lazy chunk after the module that
 * `import()` names, so this file is `IssuePage.tsx` and exports `IssuePage`: the chunk keeps the
 * name `IssuePage-<hash>.js`, and the browser rows that abort or hold `/assets/IssuePage-*.js`
 * keep their meaning. Today's page is imported statically, so that chunk holds it as it does
 * now; the new page is loaded with `lazy`, so a reader with the setting off downloads nothing of
 * it, and the app's `Suspense` shows today's skeleton while its chunk loads.
 */
export function IssuePage(): ReactNode {
  requestTypeface(useIssuePageChoice()[0] === "outline");
  return useIssuePageChoice()[0] === "outline" ? <IssueOutlinePage /> : <TodaysIssuePage />;
}

/**
 * Requests the new page's typeface (`Dispatch Inter`, `styles.css`) as the page decides to draw
 * the new page, before its chunk loads. Only the new page's stack names it, so the browser would
 * otherwise request it only once that page first draws text, and on a slow machine the swap then
 * came after the first screen and moved its rows (Row 1 at 390 px, 0.131 in five loads,
 * dispatch://INFRA-5939/comment/82020255-8eaf-4d3d-841e-bdb51d0bf410). The file is one variable
 * face that holds every weight the page draws, so one load requests it, and a load already asked
 * is answered from the one the browser holds. It runs in the render, since the page's effects run
 * only once the lazy chunk has loaded. Today's page requests nothing.
 */
export function requestTypeface(newPage: boolean): void {
  if (!newPage || !("fonts" in document)) return;
  document.fonts.load('400 1em "Dispatch Inter"').catch(() => {
    // A font that does not load is drawn in its fallback, as the stack says.
  });
}
