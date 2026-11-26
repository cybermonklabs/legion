import { useMemo } from "react";

import {
  NOTHING_PUBLISHED,
  type PublishedReferences,
  usePublishReferenceNames,
} from "../../refs/reference-names";
import type { PresetView } from "../presets";

/**
 * Publishes the readings a filter's view named the blocks its rows' chips and parent lines name
 * by (`PresetView.readings`) to the names store, for as long as the view is drawn, as the page
 * publishes its batched names read (`useIssueTree.ts`): such a block - a step a step needs, a
 * statement a check or a plan task serves - is often a node only the view's own tree holds, so
 * the names read was never asked for it, and its chip would stand unnamed. Published, the block
 * is named by the page's one rule, the line the store projects from its reading, and the chip
 * draws it as it draws any other (`NodeRow.tsx`'s `Chip`, the Plan chart's `chipLine`).
 *
 * The readings are of blocks of documents alone, so this names again no post whose marks the
 * page's read holds; a block the page's read names too is named again by the same words.
 * `readings` is one value for as long as the view is the same (`presetView`).
 */
export function usePublishViewReadings(readings: PresetView["readings"]): void {
  const published = useMemo<PublishedReferences | undefined>(
    () =>
      readings.size === 0
        ? undefined
        : { ...NOTHING_PUBLISHED, asked: new Set(readings.keys()), readings },
    [readings]
  );
  usePublishReferenceNames(published);
}
