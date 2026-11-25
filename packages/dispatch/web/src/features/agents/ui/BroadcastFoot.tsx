import { type ReactNode, useRef } from "react";

import { useFootInset } from "../../../hooks/useFootInset";

/**
 * The page's sends and its broadcast composer, under the list and stuck to the foot of the
 * page's scroll, in the foot's page layer (`hooks/useFootInset.ts` lists the foot's layers and the
 * elements in each). On a narrow or short screen it has no box (`contents`), adds nothing, and the
 * composer sticks by itself.
 */
export function BroadcastFoot({ children }: { children: ReactNode }): ReactNode {
  const footRef = useRef<HTMLDivElement>(null);
  useFootInset(footRef, true, "page");
  return (
    <div className="sticky bottom-0 z-10 mt-3 space-y-2 narrow-or-short:contents" ref={footRef}>
      {children}
    </div>
  );
}
