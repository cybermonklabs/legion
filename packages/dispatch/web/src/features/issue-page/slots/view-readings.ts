import { useMemo, useState } from "react";

import {
  NOTHING_PUBLISHED,
  type PublishedReferences,
  type ReferenceNameReading,
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
 * `readings` is one value for as long as the view is the same (`presetView`), and a view worked
 * out again names each block by a new reading of the same words, so the map published last stands
 * while the new one holds equal readings (`useSameReadings`): the store sees no change, and
 * projects no name again.
 */
export function usePublishViewReadings(readings: PresetView["readings"]): void {
  const held = useSameReadings(readings);
  const published = useMemo<PublishedReferences | undefined>(
    () =>
      held.size === 0
        ? undefined
        : { ...NOTHING_PUBLISHED, asked: new Set(held.keys()), readings: held },
    [held]
  );
  usePublishReferenceNames(published);
}

/** `readings`, or the map held before it where that holds equal readings (`sameReadings`). */
function useSameReadings(readings: PresetView["readings"]): PresetView["readings"] {
  const [held, hold] = useState(readings);
  if (held === readings || sameReadings(held, readings)) return held;
  // React draws the component again at once with the state its render set, before it commits
  // anything, and that render finds the map held.
  hold(readings);
  return readings;
}

/** Whether `one` and `other` name the same references by equal readings: the same tree object,
 *  which `storedReading` and the tree's reading keep for the same words, and the same fallback,
 *  words and chip word. */
function sameReadings(one: PresetView["readings"], other: PresetView["readings"]): boolean {
  if (one.size !== other.size) return false;
  for (const [reference, reading] of other) {
    const held: ReferenceNameReading | undefined = one.get(reference);
    if (held === undefined) return false;
    const tree = "tree" in reading ? reading : undefined;
    const heldTree = "tree" in held ? held : undefined;
    const words = "words" in reading ? reading.words : undefined;
    const heldWords = "words" in held ? held.words : undefined;
    if (
      heldTree?.tree !== tree?.tree ||
      heldTree?.fallback !== tree?.fallback ||
      heldWords !== words ||
      held.chipWord !== reading.chipWord
    ) {
      return false;
    }
  }
  return true;
}
