import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useState,
  useSyncExternalStore,
} from "react";

import { createHeld, type Held } from "./held-value";

/**
 * What the room of each of the issue's documents last admitted the page to, by the document's
 * artifact id: `true` where it admitted the page read-only, `false` where it admitted it to
 * write. A document no room has answered for on this page holds no entry.
 *
 * The server admits a document's socket read-only where it cannot trust the page's writes, as for
 * a page whose build's schema is out of date (`doc/connection.ts`'s `onAdmission`), and then no
 * block of that document takes typing. The page hears it only from a room, and the one room it
 * opens is the editor of the document a row is being edited in (`StagedDocument.tsx`), which
 * reports each admission its room makes here. Each row reads its own document's read-only state
 * (`useDocumentReadOnly`, `NodeRow.tsx`), so every row of a document its room admitted read-only
 * drops the cue that it takes typing, while an admission to write - the common case when an
 * editor first connects - changes no row's state and draws nothing again. An admission stands
 * after its editor has closed, until a room of that document answers again.
 */
export type DocumentAdmissions = ReadonlyMap<string, boolean>;

/** How a room's admission of one document is reported: its artifact id, and whether the room
 *  admitted the page read-only. */
export type ReportAdmission = (artifactId: string, readOnly: boolean) => void;

/** Outside a `DocumentAdmissionProvider` no room's admission is held, and every row follows
 *  Enter's rule alone: what a unit test that mounts the tree on its own gets. */
const NO_ADMISSIONS: DocumentAdmissions = new Map();
const NO_STORE: Held<DocumentAdmissions> = createHeld<DocumentAdmissions>(NO_ADMISSIONS);

/** The admissions store itself, whose identity never changes for the provider's life, so a report
 *  draws only readers whose own slice of the map changed. */
const AdmissionsStoreContext = createContext<Held<DocumentAdmissions>>(NO_STORE);

/** The report alone, in a context of its own whose value never changes, so the editor that
 *  reports is drawn again for no admission. */
const ReportAdmissionContext = createContext<ReportAdmission>(() => {});

/**
 * Holds each document's admission for one issue page. It stands in `slots/providers.tsx`, above
 * the body, whose rows read it and whose row editor reports to it.
 */
export function DocumentAdmissionProvider({ children }: { children: ReactNode }): ReactNode {
  const [store] = useState<Held<DocumentAdmissions>>(() => createHeld(NO_ADMISSIONS));
  // An admission that says what the document's entry already says keeps the same map, which tells
  // no reader.
  const report = useCallback<ReportAdmission>(
    (artifactId, readOnly) => {
      const current = store.get();
      if (current.get(artifactId) === readOnly) return;
      store.set(new Map(current).set(artifactId, readOnly));
    },
    [store]
  );
  return (
    <ReportAdmissionContext.Provider value={report}>
      <AdmissionsStoreContext.Provider value={store}>{children}</AdmissionsStoreContext.Provider>
    </ReportAdmissionContext.Provider>
  );
}

/** Each document's admission, held by the nearest `DocumentAdmissionProvider`. */
export function useDocumentAdmissions(): DocumentAdmissions {
  const store = useContext(AdmissionsStoreContext);
  return useSyncExternalStore(store.subscribe, store.get);
}

/** Whether the room of `artifactId` last admitted the page read-only: false while no room has
 *  answered or while it admitted writes, so a first write admission changes no row's value and
 *  draws nothing again. */
export function useDocumentReadOnly(artifactId: string | undefined): boolean {
  const store = useContext(AdmissionsStoreContext);
  return useSyncExternalStore(
    store.subscribe,
    () => artifactId !== undefined && store.get().get(artifactId) === true
  );
}

/** The nearest `DocumentAdmissionProvider`'s report, the same function for the page's whole
 *  life. */
export function useReportAdmission(): ReportAdmission {
  return useContext(ReportAdmissionContext);
}
