import { type CSSProperties, type ReactNode, useRef } from "react";

import { useFootInset } from "../hooks/useFootInset";

interface FootFloatProps {
  readonly active?: boolean;
  readonly height?: number;
  readonly style?: CSSProperties;
  readonly testId?: string;
}

/** A float of the foot's status layer, as Pending changes is (`hooks/useFootInset.ts`), with a box
 *  48 px tall at the window's foot, given to it as it is drawn since happy-dom lays nothing out:
 *  while it has a box, a change of the foot's page elements, the bar's going among them, asks for
 *  the foot's pass at the next frame. */
export function FootFloat({
  active = true,
  height = window.innerHeight,
  style,
  testId = "foot-float",
}: FootFloatProps): ReactNode {
  const ref = useRef<HTMLDivElement | null>(null);
  useFootInset(ref, active, "status");
  return (
    <div
      data-testid={testId}
      ref={(node) => {
        ref.current = node;
        if (node !== null) {
          node.getBoundingClientRect = () => new DOMRect(600, height - 64, 300, 48);
        }
      }}
      style={style}
    />
  );
}
