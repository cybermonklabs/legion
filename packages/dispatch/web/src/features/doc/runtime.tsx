import { createContext } from "react";

import type { BlockSchema } from "../../api/types";
import { type ConnectDocument, loadDocumentTransport } from "./connection";
import { type ConstructEditor, constructEditor } from "./editor";

export interface DocumentRuntimeValue {
  loadTransport(): Promise<ConnectDocument>;
  /** Builds an editor with no document in it; its `bind` puts the document in it. The
   *  construction starts the editor chunk's own download, as the page's reading ahead does
   *  before any of this renders (`features/doc/open-document-early.ts`). */
  constructEditor: ConstructEditor;
  blockSchema?: BlockSchema;
}

export const DocumentRuntime = createContext<DocumentRuntimeValue>({
  loadTransport: loadDocumentTransport,
  constructEditor,
});
