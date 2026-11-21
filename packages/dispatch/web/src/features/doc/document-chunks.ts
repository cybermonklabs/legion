import { importWhenOnline } from "../shell/chunk-loads";

/**
 * The chunks a live document pulls as it mounts, named once, in a module of their own so the
 * entry can start them without first downloading the module that uses them.
 *
 * `createEditor` (`editor.ts`) awaits the editor's library when a document mounts, and
 * `loadDocumentTransport` (`connection.ts`) the Yjs transport when it connects. The entry starts
 * both lists beside the landing read of a page that draws a document (`app.tsx`,
 * `preloadRouteChunk`), so they come down while that read is out rather than after its answer.
 * Started through `editor.ts` and `connection.ts`, each list would wait for that module's own
 * chunk to arrive and run first, and the Yjs transport would be asked for a chunk's round trip
 * after the route chunk: on a server that answers the landing read in a few dozen milliseconds,
 * after that answer (`e2e/landing-reads.e2e.ts`). This module holds no code of either list,
 * only the `import()` calls, so the entry carries their names and none of their bytes. Calling a
 * list twice costs one download: a module is fetched and parsed once.
 */

/** The editor's own library and Dispatch's node views on it (`ask-block.ts`), with its sheet.
 *  Dispatch's node views are written against ProseMirror's `DOMSerializer`, `Plugin` and
 *  `PluginKey`, so they travel with the library rather than with the page. */
export const loadEditorLibrary = () =>
  importWhenOnline(() =>
    Promise.all([
      import("@legion/proof-editor"),
      import("./ask-block"),
      import("@legion/proof-editor/style.css"),
    ])
  );

/** Hocuspocus, Yjs and the two modules a connection keeps its unsent edits with. */
export const loadTransportModules = () =>
  importWhenOnline(() =>
    Promise.all([
      import("@hocuspocus/provider"),
      import("yjs"),
      import("./pending-edits"),
      import("./pending-sync"),
    ])
  );
