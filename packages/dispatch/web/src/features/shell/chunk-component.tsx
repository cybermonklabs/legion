import { type ComponentType, type ReactNode, useEffect, useState } from "react";

import { renderChunk } from "./DeploymentResilience";

/**
 * A component whose code is a chunk of its own: the issue page's tab panels
 * (`features/issue/tab-chunks.ts`) and the card on a document's decision block
 * (`features/doc/AskBlockCardHost.tsx`).
 *
 * WHY NOT `lazy` AND `Suspense`. React's `lazy` knows its module only after the import's `then`
 * has run, so its first render always suspends, also when the chunk has already arrived. Then
 * the nearest `Suspense` commits its fallback, and React shows the content of a boundary that
 * showed its fallback no sooner than 300 ms after that fallback (`FALLBACK_THROTTLE_MS` in
 * react-dom). So each `lazy` boundary added at least 300 ms to what it drew. Two rows of run
 * 43367787934 failed on that delay:
 *
 * - A comment link on a phone (`e2e/phone-conversation.e2e.ts`). The Conversation's chunk was
 *   in before the issue page rendered, because the entry starts it beside the landing read. But
 *   its `lazy` suspended, and the thread drew about 300 ms of the page's clock later than before
 *   the tabs were chunks.
 * - Skip to the answer in the Inbox (`e2e/inbox-pane-scroll.e2e.ts`). The decision cards mounted
 *   340 ms after their chunk arrived. The reader's press came before that, and a card that drew
 *   above the answer then pushed the answer 373 px down the column.
 *
 * This component has no `Suspense` stage. A chunk that is already in draws in the commit of
 * its parent. A chunk that is not in yet draws `fallback` until the download is done, and then
 * the component itself draws in an ordinary update, which React does not hold back. A download
 * that fails goes to the nearest error boundary, as a failed `lazy` did.
 */
export interface Chunk<P> {
  /** Starts the download, or joins the download that is already out. Resolves with the
   *  component. */
  readonly load: () => Promise<ComponentType<P>>;
  /** The component when its chunk is in, else `undefined`. */
  readonly loaded: () => ComponentType<P> | undefined;
}

/** The chunk that `importer` downloads. A download that fails is not kept: the next `load`
 *  asks for the chunk again, as a new `lazy` would. */
export function chunk<P>(importer: () => Promise<ComponentType<P>>): Chunk<P> {
  let loaded: ComponentType<P> | undefined;
  let loading: Promise<ComponentType<P>> | undefined;
  return {
    load: () => {
      loading ??= importer().then(
        (component) => {
          loaded = component;
          return component;
        },
        (error: unknown) => {
          loading = undefined;
          throw error;
        }
      );
      return loading;
    },
    loaded: () => loaded,
  };
}

/**
 * The component of `source`, which draws `fallback` while the chunk is not in. The download is
 * under `renderChunk`, because a reader is in front of what this component draws: a download
 * that fails must spend one of the tab's recovery reloads (`DeploymentResilience.tsx`).
 */
export function chunkComponent<P extends object>(
  source: Chunk<P>,
  fallback: ReactNode
): ComponentType<P> {
  function ChunkComponent(props: P): ReactNode {
    // One component for the life of this mount. If the component changed type, React would
    // mount the chunk's component again and lose its state.
    const [Component, setComponent] = useState(() => source.loaded());
    useEffect(() => {
      if (Component !== undefined) {
        return;
      }
      let mounted = true;
      renderChunk(source.load).then(
        (loaded) => {
          if (mounted) setComponent(() => loaded);
        },
        (error: unknown) => {
          // An updater that throws makes the render throw, so the nearest error boundary gets
          // the failure.
          if (mounted)
            setComponent(() => {
              throw error;
            });
        }
      );
      return () => {
        mounted = false;
      };
    }, [Component]);
    return Component === undefined ? fallback : <Component {...props} />;
  }
  return ChunkComponent;
}
