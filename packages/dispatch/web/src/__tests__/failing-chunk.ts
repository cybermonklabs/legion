import type { ComponentType } from "react";

import { chunk, chunkComponent } from "../features/shell/chunk-component";

/** A part whose chunk fails to download online: it throws into its render what `chunkComponent`
 *  throws when a chunk fails after a deploy. `chunk` keeps no failed download, so each mount asks
 *  again and fails again. */
export function failingChunkComponent(): ComponentType<object> {
  return chunkComponent(
    chunk<object>(() =>
      Promise.reject(new TypeError("Failed to fetch dynamically imported module"))
    ),
    null
  );
}
