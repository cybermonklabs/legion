import { type ReactNode, useLayoutEffect, useMemo, useRef } from "react";

import type { Artifact, Version } from "../../api/types";
import { dangerText, pageQuietText } from "../../theme/classes";
import { ArtifactBlobView, ArtifactHeader, versionsNewestFirst } from "../artifacts/ArtifactHeader";
import type { DispatchRoute } from "../refs/routes";
import { DocumentVersions, VersionWords } from "./DocumentVersions";
import { DocumentReferences } from "./document-references";
import { IssueReferenceClosure } from "./IssueReferenceClosure";
import { AUTHOR_TEXT_ATTRIBUTE, pageChrome } from "./page-structure";
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
 * One artifact of an issue, as its own address `/issues/<key>/artifacts/<slug>` opens it: drawn
 * in the issue page's main column in place of the outline (dispatch://INFRA-1196 R3), with its
 * header, its editor or its picture, its versions, their downloads, what references it and what
 * it references - the issue's reference closure, which the Artifacts list it replaces drew at
 * its head - each list drawn open, 20 entries at a time (part N's R2, root R40).
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
  // where it is, and the heading is the same element while the panel draws the header, so a
  // heading already marked is left as it is.
  useLayoutEffect(() => {
    const heading = region.current?.querySelector('[data-testid="artifact-header"] h2');
    if (heading !== null && heading !== undefined && !heading.hasAttribute(AUTHOR_TEXT_ATTRIBUTE)) {
      heading.setAttribute(AUTHOR_TEXT_ATTRIBUTE, "");
    }
  });
  // Newest first, sorted again only when the versions change, and not at each toolbar the
  // document's editor publishes as the reader types: the lists below are drawn again only then.
  const listed = useMemo(
    () => versionsNewestFirst(versions ?? artifact?.versions ?? null),
    [versions, artifact?.versions]
  );
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
      {/* Keyed by the artifact, as the header's own controls are: the pages the reader drew of
          one document's versions and references are that document's alone. */}
      <DocumentVersions artifact={artifact} key={`versions:${artifact.id}`} versions={listed} />
      <DocumentReferences artifact={artifact} key={`references:${artifact.id}`} />
      <IssueReferenceClosure issueKey={artifactRoute.key} />
    </section>
  );
}
