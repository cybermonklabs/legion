import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

import {
  type KnownReferences,
  type PublishedReferences,
  type ReferenceNameReading,
  ReferenceNamesProvider,
  useKnownReferences,
  usePublishReferenceNames,
} from "../refs/reference-names";
import { parseNodeReference } from "../refs/routes";
import { chipName, type IssueTreeReads, type NodeChip, targetNaming } from "./issue-tree";
import { lineOrKindWord, useNodeLine } from "./NodeRow";

/**
 * What the shared reader makes of what the tree holds, read through the code the page's
 * surfaces draw with, for a test of the tree that holds a node or a chip rather than a row drawn
 * of it: a node's one line, as a row draws it (`NodeRow.tsx`'s `useNodeLine`); the names the
 * store projects from what a page publishes (`ReferenceNamesProvider`); and a chip's name, cut as
 * a chip cuts it (`chipName`). The engine must have loaded (`warmMarkdownRenderer`), so each is
 * answered in the render that draws it.
 */

/** The one line a row draws for `node` (`useNodeLine`), each reference in it read as the words
 *  its author wrote, since no page has named it; `""` for a node whose words hold none. */
export function nodeLineOf(node: Parameters<typeof useNodeLine>[0]): string {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = renderHook(() => useNodeLine(node, { surface: "node-row-first-line" }), {
    wrapper: ({ children }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  const { line } = view.result.current;
  view.unmount();
  return line ?? "";
}

/** The one line a filter's row names a node's parent by (`NodeRow.tsx`'s `ParentLink`): its
 *  line (`nodeLineOf`), and the page's word for its kind where its words hold none. */
export function parentLinkWords(node: Parameters<typeof useNodeLine>[0]): string {
  return lineOrKindWord(nodeLineOf(node), node.type);
}

/** Publishes `published` for as long as it is drawn, as a page publishes its batched read. */
function Publisher({ published }: { published: PublishedReferences }): ReactNode {
  usePublishReferenceNames(published);
  return null;
}

/** What a page publishes of its one batched read (`useIssueTree.ts`), made anew on every call
 *  as the page's read makes it: each of `names`' targets named by words the store draws as they
 *  stand (`ReferenceNameReading.words`), the references it answered ambiguous, gone or with no
 *  words, and every reference it asks for, which is every one of those unless `asked` names
 *  more. */
export function publishedNames(
  names: ReadonlyMap<string, string>,
  {
    ambiguous = new Set(),
    asked = [],
    gone = new Set(),
    unnamed = new Set(),
  }: {
    ambiguous?: ReadonlySet<string>;
    asked?: readonly string[];
    gone?: ReadonlySet<string>;
    unnamed?: ReadonlySet<string>;
  } = {}
): PublishedReferences {
  return {
    ambiguous,
    asked: new Set([...names.keys(), ...ambiguous, ...gone, ...unnamed, ...asked]),
    gone,
    readings: new Map([...names].map(([reference, words]) => [reference, { words }])),
    unnamed,
  };
}

/** A wrapper that draws what it is handed under the names store, with `names` published as a
 *  page publishes its batched read (`publishedNames`), and a query client that holds nothing:
 *  what a surface of the page reads a reference by. */
export function publishingNames(
  names: ReadonlyMap<string, string>
): (props: { children: ReactNode }) => ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const published = publishedNames(names);
  return ({ children }) => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>
        <Publisher published={published} />
        {children}
      </ReferenceNamesProvider>
    </QueryClientProvider>
  );
}

/** What the store hands every surface under the shell once it has projected `readings`: each
 *  name as a link to its target reads it (`linkTitleMax`), and a block's line beside it. */
function projectedKnown(readings: ReadonlyMap<string, ReferenceNameReading>): KnownReferences {
  const published: PublishedReferences = {
    ambiguous: new Set(),
    asked: new Set(readings.keys()),
    gone: new Set(),
    readings,
    unnamed: new Set(),
  };
  let known: KnownReferences | undefined;
  const Reader = (): ReactNode => {
    known = useKnownReferences();
    return null;
  };
  const view = render(
    <ReferenceNamesProvider>
      <Publisher published={published} />
      <Reader />
    </ReferenceNamesProvider>
  );
  view.unmount();
  if (known === undefined) throw new Error("the store handed no references");
  return known;
}

/** The names the store projects from `readings`, by reference, as every surface under the shell
 *  reads them - each as a link to its target reads it (`linkTitleMax`) - and none for a reading
 *  that projects to no words. */
export function projectedNames(
  readings: ReadonlyMap<string, ReferenceNameReading>
): ReadonlyMap<string, string> {
  return projectedKnown(readings).names;
}

/** The names the page publishes from `reads` (`targetNaming`), as the store projects them. */
export function namesOf(reads: IssueTreeReads): ReadonlyMap<string, string> {
  return projectedNames(targetNaming(reads).readings);
}

/** The name a chip draws for its target, as a chip in a node's words draws it: a block's line,
 *  the first block of its words, and any other target's name, as the store projects them from the
 *  chip's reading (`KnownReferences.lines`, `.names`), cut as a chip cuts it (`chipName`); `""`
 *  where it has none. A tie's chip projects the same line itself (`NodeRow.tsx`'s `Chip`). */
export function nameOf(chip: NodeChip): string {
  if (chip.reading === undefined) return "";
  const { lines, names } = projectedKnown(new Map([[chip.ref, chip.reading]]));
  return chipName(parseNodeReference(chip.ref), lines.get(chip.ref) ?? names.get(chip.ref) ?? "");
}
