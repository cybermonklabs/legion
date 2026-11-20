import type { ReactNode } from "react";

import type { SlotProps } from "../slot-props";

/** Filled by INFRA-1196 part G, "A node's row". */
export function ProvidersSlot({
  children,
}: SlotProps & { readonly children: ReactNode }): ReactNode {
  return children;
}
