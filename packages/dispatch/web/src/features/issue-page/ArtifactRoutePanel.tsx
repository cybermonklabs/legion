import { type ReactNode, useId, useLayoutEffect, useRef, useState } from "react";

import type { Artifact, Version } from "../../api/types";
import { dangerText, pageHeaderControl, pageQuietText } from "../../theme/classes";
import { ArtifactBlobView, ArtifactHeader, versionsNewestFirst } from "../artifacts/ArtifactHeader";
import { ReferencedBy } from "../refs/ReferencedBy";
import { buildDispatchReference, type DispatchRoute, documentRoute } from "../refs/routes";
import { DocumentVersions, VersionWords } from "./DocumentVersions";
import { IssueReferenceClosure } from "./IssueReferenceClosure";
import { AUTHOR_TEXT_ATTRIBUTE, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { EMPTY_STATE_LINES, FILE_WORDS } from "./page-words";

/**
 * The line under an opened document's name (part N's R2, the prototype's `openDoc`): the version
 * it shows, who wrote that version and the day it was made, "version 3 · alice · 5 Nov".
 */
function VersionByline({ version }: { version: Version }): ReactNode {
  return (
    <p
      className={`mt-1 text-xs ${pageQuietText}`}
      data-testid="opened-artifact-version"
      {...pageChrome}
    >
      <VersionWords version={version} word={FILE_WORDS.version} />
    </p>
  );
}

/**
 * What references the opened document, folded under its control until the reader opens it
 * (part N's R2), as Details folds what references the issue. The list is `ReferencedBy` in the
 * page's shape, which keeps the issue's own questions, comments and messages here
 * (`keepIssueItems`): beside an opened document no outline stands to hold them.
 */
function DocumentReferences({ artifact }: { artifact: Artifact }): ReactNode {
  const [open, setOpen] = useState(false);
  const listId = useId();
  return (
    <div className="space-y-2">
      <button
        aria-controls={open ? listId : undefined}
        aria-expanded={open}
        className={pageHeaderControl}
        onClick={() => setOpen((shown) => !shown)}
        type="button"
        {...pageChrome}
      >
        {PROPERTY_LABELS.referenced_by}
      </button>
      {open ? (
        <ReferencedBy
          keepIssueItems
          page
          reference={buildDispatchReference(documentRoute(artifact))}
          toggle={{ id: listId }}
        />
      ) : null}
    </div>
  );
}

/**
 * One artifact of an issue, as its own address `/issues/<key>/artifacts/<slug>` opens it: drawn
 * in the issue page's main column in place of the outline (dispatch://INFRA-1196 R3), with its
 * header, its editor or its picture, its versions, their downloads and what references it, and
 * the issue's reference closure, which the Artifacts list it replaces drew at its head.
 *
 * A document's version control is the one beside the views, which reads the document opened
 * here (`VersionControl.tsx`), so its header draws none of its own: it is given no toolbar, and
 * draws Correct title and Remove alone (`ArtifactHeader`'s `documentActions`). A file's header
 * keeps its version picker, since the control beside the views reads documents alone. The panel
 * is named by what it holds - the artifact's name - and never by a tab that is gone. Its
 * versions and what references it are the page's own blocks, in the page's words (part N's R2,
 * R9), and not today's `ArtifactDetails`.
 *
 * `main`'s `features/artifacts/ArtifactRoutePanel.tsx` is today's page's, and does not change
 * (INFRA-1196 R33): this one is the new page's.
 */
export function ArtifactRoutePanel({
  artifact,
  artifactRoute,
  children,
  isClosed,
  versions,
}: {
  artifact: Artifact | undefined;
  artifactRoute: Extract<DispatchRoute, { kind: "artifact" }>;
  /** The document's editor, drawn under its header; nothing for a file. */
  children: ReactNode;
  isClosed: boolean;
  /** The versions the document's editor read, newer than the issue's own read once a version is
   *  named; the artifact's own where no editor reported any. */
  versions?: readonly Version[];
}): ReactNode {
  const region = useRef<HTMLElement>(null);
  // The head's name is the name an author gave the document (part N's R9). Its header is today's
  // page's (`ArtifactHeader`), which draws the name in its heading with no mark, so the panel
  // marks that heading once it is drawn, as the opened document makes the same heading
  // focusable (`OpenedDocument.tsx`'s `headingOf`). React leaves an attribute it does not set
  // where it is, and the heading is the same element while the panel draws the header.
  useLayoutEffect(() => {
    region.current
      ?.querySelector('[data-testid="artifact-header"] h2')
      ?.setAttribute(AUTHOR_TEXT_ATTRIBUTE, "");
  });
  // A slug the issue does not hold is said in the page's own line: not the slug, which is an
  // identifier, and not the word the old tab used (part N's R2). The region is named by the
  // same line, so the opened place is still a landmark.
  if (artifact === undefined) {
    return (
      <section
        aria-label={EMPTY_STATE_LINES.document_missing}
        className="space-y-3"
        data-testid="opened-artifact-missing"
      >
        <p className={dangerText} {...pageChrome}>
          {EMPTY_STATE_LINES.document_missing}
        </p>
      </section>
    );
  }
  const isDocument = artifact.kind === "doc";
  // The version the head names: the one the address pins, else the latest.
  const listed = versionsNewestFirst(versions ?? artifact.versions);
  const shown =
    artifactRoute.version === undefined
      ? listed[0]
      : listed.find((one) => one.number === artifactRoute.version);
  return (
    <section
      aria-label={artifact.name}
      className="space-y-4"
      data-testid="opened-artifact"
      ref={region}
    >
      <ArtifactHeader
        artifact={artifact}
        byline={shown === undefined ? undefined : <VersionByline version={shown} />}
        documentActions={isDocument}
        highlight
        isClosed={isClosed}
        showVersionPicker={!isDocument}
        version={artifactRoute.version}
      >
        {isDocument ? (
          children
        ) : (
          <ArtifactBlobView artifact={artifact} version={artifactRoute.version} />
        )}
      </ArtifactHeader>
      {/* Keyed by the artifact, as the header's own controls are: what the reader opened of one
          document's versions and references is that document's alone. */}
      <DocumentVersions artifact={artifact} key={`versions:${artifact.id}`} versions={listed} />
      <DocumentReferences artifact={artifact} key={`references:${artifact.id}`} />
      <IssueReferenceClosure issueKey={artifactRoute.key} />
    </section>
  );
}
