import manifest from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import type { BlockSchema } from "../../api/types";
import { schemaCreatable } from "./schema-creatable";

/**
 * The document schema manifest this page was built with (`pmdoc/schema/blocks.json`, the file the
 * server of the same build embeds): the nodes, marks and typed blocks its editors build, and its
 * version. During a release two builds serve one deployment, so the server that answers the page
 * can be of the other build; the page reads no schema from it. The proof-editor checks the shape
 * when it builds an editor or the headless engine from it.
 */
export const compiledBlockSchema = manifest as unknown as BlockSchema;

/** R for the manifest this page was built with, which its document connections present. */
export const compiledSchemaCreatable = schemaCreatable(compiledBlockSchema);

/**
 * The schema the editor of a document of a view is given: an issue that is a view on its
 * ancestor's spec (`view_of` of `GET /api/v1/issues/{key}/tree`) holds no requirement of its own,
 * so here a `requirement` is created from the version after this page's (`creatable_since`). The
 * editor still reads and draws one; its creation gate refuses one pasted as markdown, and an HTML
 * paste keeps its words without the block. Every other element is as the compiled manifest
 * declares it.
 */
export const viewBlockSchema: BlockSchema = {
  ...compiledBlockSchema,
  types: compiledBlockSchema.types.map((type) =>
    type.name === "requirement"
      ? { ...type, creatable_since: compiledBlockSchema.version + 1 }
      : type
  ),
};

/** The schema the editor of a document of an issue is given, from what its tree read answers as
 *  `view_of`: `viewBlockSchema` for a view, the compiled manifest for every other issue. */
export function editorBlockSchema(viewOf: string | null | undefined): BlockSchema {
  return viewOf ? viewBlockSchema : compiledBlockSchema;
}
