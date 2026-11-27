import { type CSSProperties, type ReactNode, useRef } from "react";

import { useFootInset } from "../hooks/useFootInset";

interface FootFloatProps {
  readonly active?: boolean;
  readonly boxHeight?: number;
  readonly height?: number;
  readonly style?: CSSProperties;
  readonly testId?: string;
}

/** A float of the foot's status layer, as Pending changes is (`hooks/useFootInset.ts`).
 *  happy-dom lays nothing out, so the test gives the float a box 48 px tall at the window's
 *  foot by default. Set `boxHeight` to 0 to remove the box without unmounting the float. */
export function FootFloat({
  active = true,
  boxHeight = 48,
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
          node.getBoundingClientRect = () =>
            new DOMRect(600, height - 16 - boxHeight, 300, boxHeight);
        }
      }}
      style={style}
    />
  );
}
