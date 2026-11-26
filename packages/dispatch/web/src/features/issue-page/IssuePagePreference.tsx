import { type ReactNode, useEffect } from "react";

import { checkboxAccent } from "../../theme/classes";
import { textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../theme/shell-classes";
import { prefetchChunk } from "../shell/chunk-loads";
import { useIssuePageChoice } from "./choice";
import { loadFrame } from "./frame-chunk";

/** Settings' Preview section: the one control that turns the new issue page on or off for the
 *  reader, in this browser. The issue page itself holds no such control. */
export function IssuePagePreference(): ReactNode {
  const [choice, choose] = useIssuePageChoice();
  // The new page's frame is a chunk of its own (`frame-chunk.ts`), which the entry starts at boot
  // only in a browser where some login chose the new page (`app.tsx`, `preloadRouteChunk`). A
  // reader who turns the page on here is drawn the frame from it, in this tab and in every other
  // tab of the login, so it is started as this switch draws: the press then finds it in, or on
  // its way, rather than starting the download itself. Nobody waits on it yet, which is what
  // `prefetchChunk` tells the chunk-failure recovery (`features/shell/DeploymentResilience.tsx`);
  // a chunk already in is not asked for again.
  useEffect(() => {
    prefetchChunk(loadFrame);
  }, []);
  return (
    <section aria-labelledby="issue-page-preview-heading">
      <h2
        className={`text-xl font-semibold ${textPrimaryOnCanvas}`}
        id="issue-page-preview-heading"
      >
        Preview
      </h2>
      <label className={`mt-3 flex items-center gap-2 text-sm font-medium ${textPrimaryOnCanvas}`}>
        <input
          aria-describedby="issue-page-preview-note"
          checked={choice === "outline"}
          className={`size-4 shrink-0 rounded ${checkboxAccent}`}
          onChange={(event) => choose(event.target.checked ? "outline" : "tabs")}
          type="checkbox"
        />
        New issue page
      </label>
      <p className={`mt-1 pl-6 text-sm ${textSecondaryOnCanvas}`} id="issue-page-preview-note">
        Shows an issue as the new page, which is not finished. Kept in this browser.
      </p>
    </section>
  );
}
