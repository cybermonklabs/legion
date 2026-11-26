import { createContext, type ReactNode, useCallback, useContext, useState } from "react";

/**
 * What the room of each of the issue's documents last admitted the page to, by the document's
 * artifact id: `true` where it admitted the page read-only, `false` where it admitted it to
 * write. A document no room has answered for on this page holds no entry.
 *
 * The server admits a document's socket read-only where it cannot trust the page's writes, as for
 * a page whose build's schema is out of date (`doc/connection.ts`'s `onAdmission`), and then no
 * block of that document takes typing. The page hears it only from a room, and the one room it
 * opens is the editor of the document a row is being edited in (`StagedDocument.tsx`), which
 * reports each admission its room makes here. The outline's rows read it (`TreeRows.tsx`,
 * through `useNodeEditing.ts`'s `nodeEditable`), so every row of a document its room admitted
 * read-only drops the cue that it takes typing, not only the row holding the editor. An admission
 * stands after its editor has closed, until a room of that document answers again.
 */
export type DocumentAdmissions = ReadonlyMap<string, boolean>;

/** How a room's admission of one document is reported: its artifact id, and whether the room
 *  admitted the page read-only. */
export type ReportAdmission = (artifactId: string, readOnly: boolean) => void;

/** Outside a `DocumentAdmissionProvider` no room's admission is held, and every row follows
 *  Enter's rule alone: what a unit test that mounts the tree on its own gets. */
const NO_ADMISSIONS: DocumentAdmissions = new Map();

const AdmissionsContext = createContext<DocumentAdmissions>(NO_ADMISSIONS);

/** The report alone, in a context of its own whose value never changes, so the editor that
 *  reports is drawn again for no admission. */
const ReportAdmissionContext = createContext<ReportAdmission>(() => {});

/**
 * Holds each document's admission for one issue page. It stands in `slots/providers.tsx`, above
 * the body, whose rows read it and whose row editor reports to it.
 */
export function DocumentAdmissionProvider({ children }: { children: ReactNode }): ReactNode {
  const [admissions, setAdmissions] = useState<DocumentAdmissions>(NO_ADMISSIONS);
  // An admission that says what the document's entry already says hands back the same map, which
  // React draws nothing again for: the rows that read it are drawn again only when a document's
  // admission changes.
  const report = useCallback<ReportAdmission>(
    (artifactId, readOnly) =>
      setAdmissions((held) =>
        held.get(artifactId) === readOnly ? held : new Map(held).set(artifactId, readOnly)
      ),
    []
  );
  return (
    <ReportAdmissionContext.Provider value={report}>
      <AdmissionsContext.Provider value={admissions}>{children}</AdmissionsContext.Provider>
    </ReportAdmissionContext.Provider>
  );
}

/** Each document's admission, held by the nearest `DocumentAdmissionProvider`. */
export function useDocumentAdmissions(): DocumentAdmissions {
  return useContext(AdmissionsContext);
}

/** The nearest `DocumentAdmissionProvider`'s report, the same function for the page's whole
 *  life. */
export function useReportAdmission(): ReportAdmission {
  return useContext(ReportAdmissionContext);
}
