import { createContext } from "react";

import type { BlockSchema } from "../../api/types";
import { renderChunk } from "../shell/DeploymentResilience";
import { type ConnectDocument, loadDocumentTransport } from "./connection";
import { type CreateEditor, createEditor } from "./editor";

export interface DocumentRuntimeValue {
  loadTransport(): Promise<ConnectDocument>;
  createEditor: CreateEditor;
  blockSchema?: BlockSchema;
}

export const DocumentRuntime = createContext<DocumentRuntimeValue>({
  // Under `renderChunk`: a document that has been admitted calls this to connect, so a reader is
  // waiting on Hocuspocus and Yjs coming down and a failure here must reload from the tab's
  // budget. The entry prefetches the same import beside the landing read (`app.tsx`,
  // `preloadRouteChunk`), and that one is nobody waiting; the two are told apart at the call
  // (`features/shell/DeploymentResilience.tsx`).
  loadTransport: () => renderChunk(loadDocumentTransport),
  createEditor,
});
