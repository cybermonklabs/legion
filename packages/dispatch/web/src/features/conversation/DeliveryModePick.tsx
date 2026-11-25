import type { MessageDeliveryMode } from "@legion/contracts";
import type { ReactNode } from "react";

import type { ControlSize } from "../../components/control-names";
import { type ListMarks, MultiSelect, type MultiSelectProps } from "../../components/MultiSelect";
import { capabilityLabel, deliveryMode } from "./delivery";

/**
 * The Delivery mode of a message to agents: the shared list's single pick of `modes`, whose text
 * box narrows them as the reader types, and whose trigger shows the mode the message goes in
 * (`value`). The live agent view offers the modes its session takes, in the composer's order,
 * with a trigger that keeps a select's look and room, which its footer row's layout reads; the
 * Agents page's broadcast offers every mode. A pick of the mode `value` holds changes nothing, as
 * a select set to the value it holds fires nothing: a mode the session list settled stays the
 * list's until the human picks another, a restored broadcast stays the one refused, and a mode the
 * selection takes stays unpicked.
 */
export function DeliveryModePick({
  marks,
  modes,
  onOpenChange,
  onPick,
  open,
  size,
  triggerClassName,
  value,
}: {
  marks?: ListMarks;
  modes: readonly MessageDeliveryMode[];
  onOpenChange: MultiSelectProps["onOpenChange"];
  /** A pick of a mode other than `value`. */
  onPick: (mode: MessageDeliveryMode) => void;
  open: boolean;
  size?: ControlSize;
  triggerClassName: string;
  value: MessageDeliveryMode;
}): ReactNode {
  return (
    <MultiSelect
      emptyMessage="No mode matches that."
      label="Delivery mode"
      marks={marks}
      mode="single"
      onOpenChange={onOpenChange}
      onPick={(picked) => {
        const mode = deliveryMode(picked);
        if (mode !== undefined && mode !== value) onPick(mode);
      }}
      open={open}
      optionLabel={capabilityLabel}
      options={modes}
      searchLabel="Search delivery modes"
      size={size}
      triggerClassName={triggerClassName}
      value={value}
    />
  );
}
