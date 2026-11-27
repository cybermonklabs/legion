import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useState } from "react";
import { createPortal } from "react-dom";
import type { XmlElement } from "yjs";

import { issueTreeKey } from "../../api/queries";
import { pageLabelChip } from "../../theme/classes";
import { DocumentReferences } from "../doc/document-references";
import type { EditorHandle } from "../doc/editor";
import { textSourceOf } from "../refs/text-references";
import { type KeptBind, watchKeptBinds } from "./block-editor";
import type { TreeAnswer } from "./tree-read/tree-read";

interface BlockReferenceCount {
  readonly blocks: ReadonlySet<string>;
  readonly count: number;
  readonly id: string;
}

export type ReferencesPress = (
  artifactId: string,
  blockId: string,
  blocks: ReadonlySet<string>
) => void;

/**
 * What the issue page draws beside its kept block editor while the editor is bound to a row's
 * block (R7, `block-editor.tsx`), as the whole document's editor draws them for the document
 * (`doc/ProofDocument.tsx`):
 *
 * - the titles of the references the block's text holds (`DocumentReferences`), fed again at
 *   each bind, since a bind makes the editor's state anew and its references with it;
 * - the bound block's own references button, where comments or asks are anchored to the block or
 *   to a block inside it, which names the block (`data-block-references`), says how many, and
 *   filters the margin to the same set of blocks it counts. The issue's tree reads already hold
 *   the anchors, so a bind reads nothing (R7).
 */
export function KeptEditorParts({
  editor,
  issueKey,
  onReferencesPress,
}: {
  readonly editor: EditorHandle;
  readonly issueKey: string;
  readonly onReferencesPress: ReferencesPress;
}): ReactNode {
  const queryClient = useQueryClient();
  // The bind in force, numbered so each bind draws the references afresh.
  const [bind, setBind] = useState<(KeptBind & { readonly generation: number }) | undefined>();
  useEffect(() => {
    let generation = 0;
    return watchKeptBinds(editor, {
      bound: (next) => {
        generation += 1;
        setBind({ ...next, generation });
      },
      parked: () => setBind(undefined),
    });
  }, [editor]);
  const artifactId = bind?.artifactId;
  const source = useMemo(
    () => textSourceOf("artifact", artifactId, issueKey),
    [artifactId, issueKey]
  );
  const references = useMemo(
    () => (bind === undefined ? undefined : anchoredCount(queryClient, issueKey, bind)),
    [bind, issueKey, queryClient]
  );
  if (bind === undefined) return null;
  return (
    <>
      <DocumentReferences editor={editor} key={bind.generation} source={source} />
      {createPortal(
        <KeptBlockReferences
          bind={bind}
          reference={references}
          onReferencesPress={onReferencesPress}
        />,
        bind.box
      )}
    </>
  );
}

/** The single button for the bound block, including references anchored inside it. */
export function KeptBlockReferences({
  bind,
  reference,
  onReferencesPress,
}: {
  readonly bind: KeptBind;
  readonly reference: BlockReferenceCount | undefined;
  readonly onReferencesPress: ReferencesPress;
}): ReactNode {
  if (reference === undefined) return null;
  const { blocks, count, id } = reference;
  return (
    <aside
      aria-label="Block references"
      className="pointer-events-none absolute top-0 right-0 z-10 w-8"
    >
      <button
        aria-label={`${count} references on block`}
        className={`pointer-events-auto absolute top-0 right-0 min-h-11 min-w-11 rounded-full text-xs font-semibold ${pageLabelChip}`}
        data-block-references={id}
        onClick={() => onReferencesPress(bind.artifactId, id, blocks)}
        type="button"
      >
        {count}
      </button>
    </aside>
  );
}

/** The bound block, where a comment or an ask of the issue's held tree reads is anchored to it or
 *  to a block inside it, with how many are: the block's own references button (R7). */
export function anchoredCount(
  queryClient: QueryClient,
  issueKey: string,
  { artifactId, element }: KeptBind
): BlockReferenceCount | undefined {
  const id = element.getAttribute("blockId");
  if (typeof id !== "string" || id === "") return undefined;
  const blocks = new Set(blockIdsIn(element));
  const anchored = new Set<string>();
  // Every view's read holds the nodes it draws; a node two reads hold is counted once.
  for (const query of queryClient.getQueryCache().findAll({ queryKey: issueTreeKey(issueKey) })) {
    const nodes = (query.state.data as TreeAnswer | undefined)?.nodes;
    if (!Array.isArray(nodes)) continue;
    for (const node of nodes) {
      const anchor = node.anchor;
      if (anchor === undefined || anchor.orphaned || anchor.artifact_id !== artifactId) continue;
      if (anchor.block_id !== undefined && blocks.has(anchor.block_id)) anchored.add(node.id);
    }
  }
  return anchored.size === 0 ? undefined : { blocks, count: anchored.size, id };
}

/** The block ids of `element` and of every block inside it, in document order. */
function blockIdsIn(element: XmlElement): string[] {
  const id = element.getAttribute("blockId");
  const inner = element
    .toArray()
    .flatMap((child) => ("nodeName" in child ? blockIdsIn(child) : []));
  return typeof id === "string" && id !== "" ? [id, ...inner] : inner;
}
