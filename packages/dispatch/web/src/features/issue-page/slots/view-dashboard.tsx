import { lazy, type ReactNode, Suspense, useEffect } from "react";

import { pageInkText } from "../../../theme/classes";
import type { ViewSlotProps } from "./stage";

/** Dashboard's code, in the chunk of its own that today's page loads it from too
 *  (`issue/IssuePage.tsx`), fetched only when the reader opens it (R14). */
const DashboardTab = lazy(() => import("../../dashboard/DashboardTab"));

/**
 * The Dashboard view (dispatch://INFRA-5702/spec#b-r-new-page): the issue's Dashboard drawn in the
 * outline's place, the new page's opening view. It draws no outline, none of the issue's three
 * sentences, no children's section and no version control: only Dashboard, in the outline's
 * measure.
 *
 * Dashboard draws no node of the issue's tree, so an address that names one beside Dashboard's own
 * (`/dashboard?comment=<id>`) is the outline's to land on: the view tells the stage at once that
 * it draws none (`ViewSlotProps.onAddressDrawn`), and the stage draws the outline in its place.
 */
export function ViewDashboardSlot({ issue, onAddressDrawn }: ViewSlotProps): ReactNode {
  useEffect(() => onAddressDrawn(false), [onAddressDrawn]);
  // `data-dashboard` is the view's own mark, which the browser rows find it by, and under which
  // the words Dashboard draws take the page's link colour (`styles.css`). The view is in the
  // page's ink, as everything Dashboard draws inside it is (dispatch://INFRA-3940 R13).
  return (
    <div className={`mx-auto w-full max-w-[1000px] min-w-0 pb-6 ${pageInkText}`} data-dashboard="">
      <Suspense fallback={null}>
        <DashboardTab issueKey={issue.key} page="new" />
      </Suspense>
    </div>
  );
}
