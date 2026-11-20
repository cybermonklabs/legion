import type { ReactNode } from "react";

import { checkboxAccent, textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../theme/classes";
import { useIssuePageChoice } from "./choice";

/** Settings' Preview section: the one control that turns the new issue page on or off for the
 *  reader, in this browser. The issue page itself holds no such control. */
export function IssuePagePreference(): ReactNode {
  const [choice, choose] = useIssuePageChoice();
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
