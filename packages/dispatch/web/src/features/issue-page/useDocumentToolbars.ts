import {
  createContext,
  createElement,
  type ReactNode,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
} from "react";

import type { DocumentToolbar } from "../doc/ProofDocument";

/** What a document reports its toolbar through: a callback of its own, by its artifact id. */
export type ToolbarSink = (artifactId: string) => (next: DocumentToolbar | undefined) => void;

export interface DocumentToolbars {
  /** Which documents are drawn against their previous version. */
  readonly diffs: ReadonlySet<string>;
  /** Turns a document's diff on or off. */
  readonly showDiff: (artifactId: string, next: boolean) => void;
  /** The one callback a document reports its toolbar through, by artifact id. */
  readonly toolbarSink: ToolbarSink;
  /** The toolbar each open document published. */
  readonly toolbars: ReadonlyMap<string, DocumentToolbar>;
}

/** Outside a `DocumentToolbarsProvider` a document's toolbar is held nowhere and no diff is on,
 *  which is what a unit test that mounts the body alone gets. */
const NO_TOOLBARS: DocumentToolbars = {
  diffs: new Set(),
  showDiff: () => {},
  toolbarSink: () => () => {},
  toolbars: new Map(),
};

const DocumentToolbarsContext = createContext<DocumentToolbars>(NO_TOOLBARS);

/** The sink alone, in a context of its own whose value never changes: an editor publishes a
 *  toolbar each time its connection, its unsent changes or its versions move, and a reader of
 *  the sink - the body, which draws the whole outline - is not drawn again for any of them. */
const ToolbarSinkContext = createContext<ToolbarSink>(NO_TOOLBARS.toolbarSink);

/**
 * Holds one toolbar and one diff per document, by its artifact id, for one issue page: every
 * editor on the page reports its own toolbar through a callback of its own, for the page's
 * version control to draw the toolbar of whichever document is open. It stands in
 * `slots/providers.tsx`, above the body, whose row editor reports here through
 * `useToolbarSink`, and above the view row, where the page's version control is to read the
 * toolbars; nothing reads them yet.
 */
export function DocumentToolbarsProvider({ children }: { children: ReactNode }): ReactNode {
  const held = useHeldToolbars();
  return createElement(
    ToolbarSinkContext.Provider,
    { value: held.toolbarSink },
    createElement(DocumentToolbarsContext.Provider, { value: held }, children)
  );
}

/** The page's toolbars and diffs, held by the nearest `DocumentToolbarsProvider`. */
export function useDocumentToolbars(): DocumentToolbars {
  return useContext(DocumentToolbarsContext);
}

/** The nearest `DocumentToolbarsProvider`'s sink, which is the same function for the page's
 *  whole life, so reading it draws nothing again when a toolbar changes. */
export function useToolbarSink(): ToolbarSink {
  return useContext(ToolbarSinkContext);
}

/**
 * The toolbars and diffs the provider holds.
 *
 * Each callback is held across renders, because an editor publishes its toolbar from an effect:
 * a fresh function every render would have it publish again on every commit.
 */
function useHeldToolbars(): DocumentToolbars {
  const [toolbars, setToolbars] = useState<ReadonlyMap<string, DocumentToolbar>>(() => new Map());
  const [diffs, setDiffs] = useState<ReadonlySet<string>>(() => new Set());
  const sinks = useRef(new Map<string, (next: DocumentToolbar | undefined) => void>());
  const showDiff = useCallback(
    (artifactId: string, next: boolean) =>
      setDiffs((held) => {
        const made = new Set(held);
        if (next) made.add(artifactId);
        else made.delete(artifactId);
        return made;
      }),
    []
  );
  const toolbarSink = useCallback(
    (artifactId: string) => {
      const held = sinks.current.get(artifactId);
      if (held !== undefined) return held;
      const made = (next: DocumentToolbar | undefined) => {
        setToolbars((held) => {
          const replaced = new Map(held);
          if (next === undefined) replaced.delete(artifactId);
          else replaced.set(artifactId, next);
          return replaced;
        });
        // A document that has closed shows no diff: the control that turned one on has gone
        // with it.
        if (next === undefined) showDiff(artifactId, false);
      };
      sinks.current.set(artifactId, made);
      return made;
    },
    [showDiff]
  );
  return useMemo(
    () => ({ diffs, showDiff, toolbarSink, toolbars }),
    [diffs, showDiff, toolbarSink, toolbars]
  );
}
