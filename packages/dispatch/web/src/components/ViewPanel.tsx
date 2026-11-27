import type { ReactNode } from "react";

import { CONTROL } from "./control-names";
import { PanelTrigger, type PanelTriggerProps } from "./PanelTrigger";

/**
 * The trigger of a page's own panel that narrows nothing: the Issues tab's Save view and its (i)
 * hint. It is `PanelTrigger` with its own `data-control`, `view-panel`, so a control row keeps one
 * `panel-trigger`, the one over the narrowings, and a browser row finds each by its own name.
 */
export function ViewPanel(props: Omit<PanelTriggerProps, "control">): ReactNode {
  return <PanelTrigger {...props} control={CONTROL.viewPanel} />;
}
