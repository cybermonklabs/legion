import { type ComponentType, type ReactNode, useEffect, useState } from "react";

/**
 * A part whose code is a chunk of its own, drawn in the commit that chunk arrives in.
 *
 * `lazy` under a `Suspense` is the shorter way, and it is what stood at each of this module's
 * call sites. What it costs is a wait React adds on top of the download. The first loading
 * state a page draws opens a 300 ms window (`FALLBACK_THROTTLE_MS` in react-dom), and inside
 * that window React holds back any commit whose only work is a chunk that has arrived, so that
 * no loading state is ever replaced less than 300 ms after it was drawn. A page whose chunks
 * land in the first moments of its load - which is every page, now that each chunk is asked for
 * from the address (`app.tsx`'s `requestTheOpenPagesChunk`, `features/shell/boot.ts`) -
 * therefore waits out that window rather than drawing (dispatch://INFRA-341, R11).
 *
 * Nothing here draws a loading state React has to be polite about. `space` is the page's own
 * content, drawn exactly as the `Suspense` fallback was, and the part replaces it in a state
 * update like any other, which React commits as soon as it has it.
 *
 * A chunk that fails to download is thrown from the render, where the error boundary around the
 * part answers it exactly as it answered a `lazy` whose import rejected, and the tab's reload
 * budget covers it (`features/shell/DeploymentResilience.tsx`).
 */
export function chunkComponent<Props extends object>(
  load: () => Promise<ComponentType<Props>>,
  space: ReactNode = null
): ComponentType<Props> {
  return function TheChunksPart(props: Props): ReactNode {
    const [arrived, setArrived] = useState<
      { readonly part: ComponentType<Props> } | { readonly failure: unknown }
    >();
    useEffect(() => {
      let wanted = true;
      void load().then(
        (part) => {
          if (wanted) setArrived({ part });
        },
        (failure: unknown) => {
          if (wanted) setArrived({ failure });
        }
      );
      return () => {
        wanted = false;
      };
    }, []);
    if (arrived === undefined) {
      return space;
    }
    if ("failure" in arrived) {
      throw arrived.failure;
    }
    const Part = arrived.part;
    return <Part {...props} />;
  };
}
