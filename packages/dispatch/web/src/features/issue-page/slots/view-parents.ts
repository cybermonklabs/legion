import { useMemo } from "react";

import {
  NOTHING_PUBLISHED,
  type PublishedReferences,
  usePublishReferenceNames,
} from "../../refs/reference-names";
import type { PresetView } from "../presets";

/**
 * Publishes the readings a filter's view named its rows' other parents by (`PresetView.parents`)
 * to the names store, for as long as the view is drawn, as the page publishes its batched names
 * read (`useIssueTree.ts`): the parent is a node only the view's own tree holds, so the names
 * read was never asked for it, and its chip - the tie a check or a plan task keeps to each
 * statement it serves - would stand unnamed. Published, the parent is named by the page's one
 * rule, the line the store projects from its reading, and the chip draws it as it draws any
 * other (`NodeRow.tsx`'s `Chip`).
 *
 * The readings are those of references the page's read names nothing for, so this publishes no
 * name that read gave. `parents` is one value for as long as the view is the same
 * (`presetView`).
 */
export function usePublishViewParents(parents: PresetView["parents"]): void {
  const published = useMemo<PublishedReferences | undefined>(
    () =>
      parents.size === 0
        ? undefined
        : { ...NOTHING_PUBLISHED, asked: new Set(parents.keys()), readings: parents },
    [parents]
  );
  usePublishReferenceNames(published);
}
