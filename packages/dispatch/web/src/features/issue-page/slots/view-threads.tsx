import { type ReactNode, useEffect } from "react";

import { ThreadsView } from "../threads/ThreadsView";
import type { ViewSlotProps } from "./stage";

/**
 * The Threads view (dispatch://INFRA-5704/spec#b-r-view), drawn in the outline's place. It is
 * imported here as each filter's view is, not through `lazy`: the new page's own chunk
 * (`IssueOutlinePage`) holds it, so no chunk of today's pages and not the Dashboard chunk does
 * (R18), and the build emits no chunk of its own for it.
 *
 * Threads draws no node of the issue's tree, so an address that names one beside Threads' own
 * (`/threads?comment=<id>`, or a `#b-` fragment on it) is the outline's to land on: the view tells
 * the stage at once that it draws none (`ViewSlotProps.onAddressDrawn`), and the stage draws the
 * outline in its place, as it does for Dashboard (`view-dashboard.tsx`).
 */
export function ViewThreadsSlot({ issue, onAddressDrawn, route }: ViewSlotProps): ReactNode {
  useEffect(() => onAddressDrawn(false), [onAddressDrawn]);
  return <ThreadsView issue={issue} route={route} />;
}
