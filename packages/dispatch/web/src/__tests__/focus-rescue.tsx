import { act } from "@testing-library/react";
import { type ReactNode, useRef } from "react";

import { useFocusRescue } from "../components/focus-place";

/** The shell's main content holding `children`, with the page's focus rescue mounted on it as
 *  `app.tsx` mounts it. */
export function RescuedMain({ children }: { children: ReactNode }): ReactNode {
  const main = useRef<HTMLElement>(null);
  useFocusRescue(main);
  return <main ref={main}>{children}</main>;
}

/** Lets the page's focus rescue answer a loss: its check runs a frame and then a task after it,
 *  so a frame and two zero-delay turns of the event loop here come after that check. */
export async function afterRescue(): Promise<void> {
  await act(async () => {
    const drawn = Promise.withResolvers<void>();
    requestAnimationFrame(() => drawn.resolve());
    await drawn.promise;
    for (const _ of [0, 1]) {
      const turn = Promise.withResolvers<void>();
      setTimeout(turn.resolve, 0);
      await turn.promise;
    }
  });
}
