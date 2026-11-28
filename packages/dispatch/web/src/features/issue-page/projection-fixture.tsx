import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

import type { ArtifactBlock, IssueDetails } from "../../api/types";
import type { PostDeletion } from "../refs/post-marks";
import {
  type KnownReferences,
  type PublishedReferences,
  type ReferenceNameReading,
  ReferenceNamesProvider,
  useKnownReferences,
  usePublishReferenceNames,
} from "../refs/reference-names";
import { parseNodeReference } from "../refs/routes";
import { chipName } from "./first-line";
import type { IssueTreeReads, NodeChip, TreeNode } from "./issue-tree";
import { lineOrKindWord, useNodeLine } from "./node-line";
import type { LineNames } from "./one-line";
import { knownIssueKeys } from "./row-references";
import { chipNaming, targetNaming } from "./target-naming";

/**
 * What the shared reader makes of what the tree holds, read through the code the page's
 * surfaces draw with, for a test of the tree that holds a node or a chip rather than a row drawn
 * of it: a node's one line, as a row draws it (`node-line.tsx`'s `useNodeLine`); the names the
 * store projects from what a page publishes (`ReferenceNamesProvider`); a chip's name, cut as a
 * chip cuts it (`chipName`); and the reads the page names a test's blocks from (`blockReads`), so
 * a test names them by the page's own rule (`targetNaming`, `treeContext`) and copies none of it.
 * The engine must have loaded (`warmMarkdownRenderer`), so each is answered in the render that
 * draws it.
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

/** The one line a filter's row names a node's parent by (`node-line.tsx`'s `NodeParentLine`):
 *  its line (`nodeLineOf`), and the page's word for its kind where its words hold none. */
export function parentLinkWords(node: Parameters<typeof useNodeLine>[0]): string {
  return lineOrKindWord(nodeLineOf(node), node.type);
}

/** Publishes `published` to the names store for as long as it is drawn, as a page publishes its
 *  batched read beside what it draws (`useIssueTree.ts`): in the layout phase of the commit that
 *  draws it, after what is drawn before it has rendered. */
export function Publisher({ published }: { published: PublishedReferences }): ReactNode {
  usePublishReferenceNames(published);
  return null;
}

/** What a page publishes of its one batched read (`useIssueTree.ts`), made anew on every call
 *  as the page's read makes it: each of `names`' targets named by words the store draws as they
 *  stand (`ReferenceNameReading.words`), the references it answered ambiguous, gone or with no
 *  words, those among the names whose post its author edited or deleted, and every reference it
 *  asks for, which is every one of those unless `asked` names more. */
export function publishedNames(
  names: ReadonlyMap<string, string>,
  {
    ambiguous = new Set(),
    asked = [],
    deleted = new Map(),
    edited = new Set(),
    gone = new Set(),
    unnamed = new Set(),
  }: {
    ambiguous?: ReadonlySet<string>;
    asked?: readonly string[];
    deleted?: ReadonlyMap<string, PostDeletion>;
    edited?: ReadonlySet<string>;
    gone?: ReadonlySet<string>;
    unnamed?: ReadonlySet<string>;
  } = {}
): PublishedReferences {
  return {
    ambiguous,
    asked: new Set([...names.keys(), ...ambiguous, ...gone, ...unnamed, ...asked]),
    deleted,
    edited,
    gone,
    readings: new Map([...names].map(([reference, words]) => [reference, { words }])),
    unnamed,
  };
}

/** What a plain line on the issue `issue`'s page names the references it cites by
 *  (`useIssueNames`) where the page published `names`, each target's name, an issue's title
 *  among them, with each issue they name known by its bare key (`knownIssueKeys`), and `lines`,
 *  the line a chip names a block by: a reference with neither reads as its author wrote it, and
 *  nothing is deleted, edited or ambiguous. */
export function lineNamesOf(
  issue: string,
  { lines = new Map(), names = new Map() }: Partial<Pick<LineNames, "lines" | "names">> = {}
): LineNames {
  return {
    ambiguous: new Set(),
    author: () => "",
    deleted: new Map(),
    edited: new Set(),
    issue,
    issues: knownIssueKeys(names),
    lines,
    names,
  };
}

/** What the page publishes for `chips`: every chip the batched read answered for, named by the
 *  page's own rule (`target-naming.ts`'s `chipNaming`, as `targetNaming` names the read's
 *  targets), and every chip's reference as one the page asks for, answered or not yet. */
export function publishedChips(chips: readonly NodeChip[]): PublishedReferences {
  return {
    ...chipNaming(chips.filter((chip) => chip.answered)),
    asked: new Set(chips.map((chip) => chip.ref)),
  };
}

/** What the page publishes for every chip `root` holds, at any depth (`publishedChips`). */
export function publishedTree(root: TreeNode): PublishedReferences {
  const chips: NodeChip[] = [];
  const walk = (node: TreeNode): void => {
    chips.push(...node.chips, ...node.alsoUnder);
    for (const child of node.children) walk(child);
  };
  walk(root);
  return publishedChips(chips);
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
    deleted: new Map(),
    edited: new Set(),
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

/**
 * The reads the page names `nodes` from, as its own reads hold them (`IssueTreeReads`): `issue`,
 * whose documents the nodes are blocks of; each document's blocks read, which gives each node's
 * block its kind (`type`), so one whose words hold none is named by the page's word for that
 * kind; and the batched read's answer for each node that holds a reading of its words (`tree`),
 * as that read answers a block (`text_tree`). A node that holds no reading has no answer, as one
 * the read has not answered for yet. Each node is the block its own address names
 * (`reference`): a block of the spec is in the issue's spec, a block of any other document in the
 * document of `issue` with that slug, and a node whose address names neither is in no blocks
 * read.
 */
export function blockReads(
  issue: IssueDetails,
  nodes: readonly Pick<TreeNode, "reference" | "text" | "tree" | "type">[]
): IssueTreeReads {
  const blocks = new Map<string, ArtifactBlock[]>();
  for (const { reference, type } of nodes) {
    const route = reference === undefined ? undefined : parseNodeReference(reference);
    if ((route?.kind !== "spec" && route?.kind !== "artifact") || route.block === undefined) {
      continue;
    }
    const slug = route.kind === "artifact" ? route.slug : undefined;
    const artifactId =
      slug === undefined
        ? issue.primary_artifact_id
        : issue.artifacts?.find((artifact) => artifact.slug === slug)?.id;
    if (artifactId === undefined) continue;
    const id = route.block.id;
    const block: ArtifactBlock = {
      attributes: [],
      from: 0,
      id,
      references: { asks: 0, comments: 0 },
      to: 1,
      token: id,
      type,
    };
    blocks.set(artifactId, [...(blocks.get(artifactId) ?? []), block]);
  }
  return {
    documents: [...blocks].map(([artifactId, held]) => ({ artifactId, blocks: held })),
    issue,
    targets: nodes.flatMap(({ reference, text, tree }) =>
      reference === undefined || tree === undefined
        ? []
        : [{ gone: false, kind: "block", name: text, ref: reference, text_tree: tree }]
    ),
  };
}

/** The names the page publishes from `reads` (`targetNaming`), as the store projects them. */
export function namesOf(reads: IssueTreeReads): ReadonlyMap<string, string> {
  return projectedNames(targetNaming(reads).readings);
}

/** The name a chip draws for its target, as every chip draws it: a block's line, the first block
 *  of its words, a whole document's, the page's word for it, and any other target's name, as the
 *  store projects them from the chip's reading (`KnownReferences.lines`, `.names`), cut as a chip
 *  cuts it (`chipName`); `""` where it has none. A tie's chip (`NodeRow.tsx`'s `Chip`) and a chip
 *  in a node's words read the same. */
export function nameOf(chip: NodeChip): string {
  if (chip.reading === undefined) return "";
  const { lines, names } = projectedKnown(new Map([[chip.ref, chip.reading]]));
  return chipName(parseNodeReference(chip.ref), lines.get(chip.ref) ?? names.get(chip.ref) ?? "");
}
