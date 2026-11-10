import type { Artifact } from "../../api/types";
import type { IssueTab } from "../refs/routes";

/**
 * Which documents the issue page has open, in one answer (dispatch://INFRA-1196 R16).
 *
 * One document has one editor and never a second (`document-stage.tsx`): a second one is a
 * second socket on the same document. The panels below the tree draw some of those editors and
 * the page draws one more for the document a row of the tree is being edited in, so the two
 * sides have to agree about which documents are already drawn. They agree by reading this: the
 * panels draw what it answers, and the edited row gets an editor only for a document it names
 * none of (`editedDocument`).
 *
 * It is pure and holds no React, so the page, the panels and the unit tests all read the same
 * answer.
 */
export interface DrawnDocuments {
  /** The plan, which the Plan preset keeps open. */
  readonly plan: Artifact | undefined;
  /** Whatever an `/artifacts/<slug>` address opens, where that is a document and not the
   *  issue's own spec. */
  readonly selected: Artifact | undefined;
  /** The issue's spec, which the Spec preset keeps open. */
  readonly spec: Artifact | undefined;
  /** The Verification document, which the Checks preset keeps open. */
  readonly verification: Artifact | undefined;
}

/** What the address opens, and which panels are mounted for it. */
export interface OpenDocuments {
  /** Which panels the reader has opened at least once, so are mounted and keep rendering while
   *  hidden (`IssuePage`'s `activatedTabs`). */
  readonly activated: Readonly<Record<IssueTab, boolean>>;
  /** Whether the address opens a document of the issue that is not its spec
   *  (`/artifacts/<slug>`): the spec's own panel then draws nothing, because the address is
   *  about that other document. */
  readonly otherDocument: boolean;
  readonly plan: Artifact | undefined;
  /** The artifact the address names, which may be an image or any other blob. */
  readonly selected: Artifact | undefined;
  readonly spec: Artifact;
  readonly verification: Artifact | undefined;
}

/** Which document each panel of the page draws. One place decides it, so no two panels can
 *  come to draw the same document and no row can be left with none. */
export function drawnDocuments(open: OpenDocuments): DrawnDocuments {
  return {
    plan: open.activated.plan ? open.plan : undefined,
    selected:
      open.activated.artifacts && open.otherDocument && open.selected?.kind === "doc"
        ? open.selected
        : undefined,
    spec: open.activated.spec && !open.otherDocument ? open.spec : undefined,
    verification: open.activated.verification ? open.verification : undefined,
  };
}

/** The id of every document `drawn` holds an editor for. */
export function drawnDocumentIds(drawn: DrawnDocuments): ReadonlySet<string> {
  return new Set(
    [drawn.plan, drawn.selected, drawn.spec, drawn.verification]
      .filter((artifact): artifact is Artifact => artifact !== undefined)
      .map((artifact) => artifact.id)
  );
}

/**
 * The document a row of the tree is being edited in, where no panel draws it already:
 * `undefined` for a row of a document a panel has open, which is the editor that row takes onto
 * its own stage. A row of a document no panel opened - a decision, a check of a document the
 * address does not name - gets the page's own one.
 */
export function editedDocument(
  drawn: DrawnDocuments,
  editing: string | undefined,
  artifacts: readonly Artifact[]
): Artifact | undefined {
  if (editing === undefined || drawnDocumentIds(drawn).has(editing)) return undefined;
  return artifacts.find((artifact) => artifact.id === editing);
}
