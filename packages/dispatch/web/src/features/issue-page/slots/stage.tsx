import type { ReactNode } from "react";

import type { SlotProps } from "../slot-props";
import { BodySlot } from "./body";

/** Filled by INFRA-1196 part I, "The outline". */
export function StageSlot(props: SlotProps): ReactNode {
  return <BodySlot {...props} />;
}
