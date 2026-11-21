// The attribute is what lets Node load this module too: the browser rows' other writer presents
// the same numbers the page does (`e2e/large-documents-rows.ts`, `theOtherPersonsPen`), and
// Node's ESM loader refuses a JSON import without it.
import manifest from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json" with {
  type: "json",
};
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
