import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useMemo, useRef } from "react";

import { referenceTargetsQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { addAskedReference, citedReferences } from "../issue-page/row-references";
import { chipNaming, NO_TARGET_NAMING, treeContext } from "../issue-page/target-naming";
import { type PublishedReferences, usePublishReferenceNames } from "../refs/reference-names";
import { EVENT_FRESH } from "../refs/reference-target";

/** What Dashboard names, and is asked to name, by its one batched read. */
export interface DashboardNameSources {
  /** Every stored text Dashboard draws: each reference it cites is named. */
  readonly texts: readonly string[];
  /** The references Dashboard draws as links by their names: a tile's entries, and the
   *  statements a decision shaped. */
  readonly references: readonly string[];
  /** Whether every read that adds a text or a reference has answered, so the batched read asks
   *  for them all at once rather than once for each read that lands. */
  readonly settled: boolean;
}

/** What the batched read answers for one reference beside its published name. */
export interface DashboardNames {
  /** The stored excerpt the read names `reference`'s target by, a typed block's directive left
   *  out (`treeContext`'s `excerptOf`), and `""` until the read answers for it. */
  readonly excerptOf: (reference: string) => string;
  /** What Dashboard publishes, which its own subtree reads in the render that draws it
   *  (`PublishedReferenceNames`), so a text drawn in the commit that publishes a reference waits
   *  on this read for it and reads it no other way. */
  readonly published: PublishedReferences;
}

const NO_REFERENCES: readonly string[] = [];

/**
 * Dashboard's names (dispatch://INFRA-5702/spec#b-r-chunk, choice 4): every reference its texts
 * cite and every reference it draws as a link, read in one batched `GET /api/v1/reference-targets`
 * and published into the provider the app mounts around the router
 * (`refs/reference-names.tsx`), named by the page's one rule (`targetNaming`). Every reference
 * is published as asked from the first render that holds it, so `RowText` and the plain-text
 * projection wait on this read rather than reading each reference themselves (R8). A spec named
 * whole is titled by its issue, which is asked for beside it, as the tree asks for it
 * (`useIssueTree.ts`).
 *
 * The read asks for the set held once every read that adds to it has answered (`settled`), and
 * keeps that set while the reads it waits on run again, so a text that lands later is one more
 * read and no read is asked for a set about to grow. The names in hand stand while a new set is
 * read.
 */
export function useDashboardNames(
  issue: IssueDetails | undefined,
  { references, settled, texts }: DashboardNameSources
): DashboardNames {
  const cited = useMemo(() => {
    const refs = new Set<string>();
    for (const text of texts) {
      for (const reference of citedReferences(text)) addAskedReference(refs, reference);
    }
    for (const reference of references) addAskedReference(refs, reference);
    return [...refs].sort();
  }, [references, texts]);
  // The set the read was last given while every read it waits on had answered, written during
  // render, so the read's key changes only with what the settled reads hold.
  const asked = useRef<readonly string[]>(NO_REFERENCES);
  if (settled) asked.current = cited;
  const targets = useQuery({
    ...referenceTargetsQuery(asked.current),
    ...EVENT_FRESH,
    placeholderData: keepPreviousData,
  }).data?.targets;
  // Each answered target named by its chip, as the page names it (`targetNaming`), from the one
  // context the excerpts are read from too.
  const naming = useMemo(() => {
    if (issue === undefined || targets === undefined) return undefined;
    const context = treeContext({ issue, targets });
    return { context, names: chipNaming(targets.map((target) => context.chipOf(target.ref))) };
  }, [issue, targets]);
  const published = useMemo(
    (): PublishedReferences => ({ ...(naming?.names ?? NO_TARGET_NAMING), asked: new Set(cited) }),
    [cited, naming]
  );
  usePublishReferenceNames(published);
  return useMemo(
    () => ({
      excerptOf: (reference: string) => naming?.context.excerptOf(reference) ?? "",
      published,
    }),
    [naming, published]
  );
}
