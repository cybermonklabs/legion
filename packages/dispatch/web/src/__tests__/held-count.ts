/**
 * The count the act fixtures' components show, which `act-warnings.test.ts` runs in child
 * `bun test`s. A fixture cannot import another fixture, which would register that file's rows, so
 * they share these from here. Each fixture keeps its own named components, since React's act
 * messages name the component and `act-warnings.test.ts` asserts that name.
 */

import { useState } from "react";

/** The setter of the count a row's component shows, which the row calls with no event between. */
export interface HeldCount {
  set: (count: number) => void;
}

export function heldCount(): HeldCount {
  return {
    set: () => {
      throw new Error("the component has not rendered");
    },
  };
}

export function useHeldCount(held: HeldCount): string {
  const [count, setCount] = useState(0);
  held.set = setCount;
  return `count ${count}`;
}
