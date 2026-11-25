import { type ReactNode, useContext, useEffect, useMemo, useRef } from "react";

import type { Artifact } from "../../api/types";
import { pageHeaderControl } from "../../theme/classes";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { DocumentRuntime, type DocumentRuntimeValue } from "../doc/runtime";
import { EVENT_FRESH } from "../refs/reference-target";
import type { IssueRoute } from "../refs/routes";
import { ArtifactRoutePanel } from "./ArtifactRoutePanel";
import { backToDocuments, useFollowWayBack } from "./document-address";
import { DocumentStage, useDocumentStage } from "./document-stage";
import { type FocusFallback, type FocusLoss, useFocusKeeper } from "./focus-keeper";
import { pageChrome } from "./page-structure";
import { blockAttributeLabel, CONTROL_LABELS, nodeTypeName } from "./page-words";
import { EditorBox } from "./StagedDocument";
import { useDocumentToolbars } from "./useDocumentToolbars";

/** The page's words for a typed block's type and its attributes (`page-words.ts`), which the
 *  opened document draws in place of the schema's tokens (part N's R9, R12). */
const BLOCK_NAMES = { attribute: blockAttributeLabel, type: nodeTypeName } as const;

/**
 * The opened document's heading, its name as its head draws it: the region's first heading
 * (`ArtifactHeader`'s, ahead of the editor and every dialog the editor draws). A Rebuild that
 * succeeds takes its own offer away and hands its focus to nothing (decision 223), so the reader
 * lands on the rebuilt document's name rather than on the page's body. A heading is no control,
 * so it is focusable by script alone, out of the tab order (`tabindex="-1"`), from the moment it
 * is handed the focus. A slug the issue does not hold draws no heading, and the focus stays where
 * the browser leaves it.
 *
 * A focus lost inside a dialog the document draws, as the name dialog's text box is as it closes,
 * is that dialog's own to give back: it gives it to the control that opened it (`useDialog`), so
 * the region places nothing, and the focus moves once.
 */
function headingOf({ lost, root }: FocusLoss): HTMLElement | null {
  if (lost.closest('[role="dialog"]') !== null) return null;
  const heading = root.querySelector<HTMLElement>("h1, h2, h3");
  if (heading !== null) heading.tabIndex = -1;
  return heading;
}

/** Where the opened document's region gives the focus that a change took away with no control
 *  left to take it (R16, decision 259). */
const DOCUMENT_FOCUS_FALLBACKS: readonly FocusFallback[] = [headingOf];

/**
 * A document or file of the issue, opened in the main column in place of the outline
 * (dispatch://INFRA-1196 R3): what its own address `/issues/<key>/artifacts/<slug>` draws, and
 * what choosing it in Documents' list, or a chip that names it, opens. A document is drawn with
 * its editor, so the caret, the collaborative cursors and the comment-on-selection chord work in
 * it as they do anywhere a document is edited; its versions, their downloads, what it references
 * and what references it are drawn under it.
 *
 * Its head's "Back to Documents" opens Documents' list at `/issues/<key>/documents` by a push, as
 * a link does, so the browser's Back returns to the document, and the focus goes to the
 * document's row there (part N's R2, `document-address.ts`' `backToDocuments`). Escape closes it
 * in the key table's order (the Design's keyboard table): a caret in the document's text is left
 * first, and the next Escape closes the document, back to the list, or to the row that holds the
 * chip that opened it (`escape-order.ts`). The editor takes Escape for itself before the page's
 * keyboard registry sees it, so the caret is left by a listener of this box's own in the capture
 * phase, as a row of the outline leaves its caret (`useNodeEditing.ts`); the close is the
 * registry's, which a menu or a dialog that is open over the page answers first.
 *
 * An ask or comment `?mark=` names is resolved to its whole id before this is drawn
 * (`slots/stage.tsx`), so the document's own landing reads it under that id and writes no
 * address of its own.
 */
export function OpenedDocument({
  artifact,
  artifactRoute,
  highlightTerm,
  isClosed,
  issueKey,
  item,
  onShowDiff,
  onToolbarChange,
  onVersionChange,
  showDiff,
}: {
  artifact: Artifact | undefined;
  artifactRoute: Extract<IssueRoute, { kind: "artifact" }>;
  highlightTerm: string | undefined;
  isClosed: boolean;
  issueKey: string;
  /** The ask or comment the address names for the document to land on, marked in its text
   *  (`issue-address.ts`'s `documentItem`). */
  item: { readonly kind: "ask" | "comment"; readonly id: string } | undefined;
  onShowDiff: (next: boolean) => void;
  onToolbarChange: (next: DocumentToolbar | undefined) => void;
  onVersionChange: (version: number | null) => void;
  showDiff: boolean;
}): ReactNode {
  const followWayBack = useFollowWayBack();
  const box = useRef<HTMLDivElement>(null);
  // The region keeps the reader's place (R16): a control of the document that goes while it
  // holds the focus, as Rebuild does once the rebuilt document is drawn, hands it to the heading.
  useFocusKeeper(box, DOCUMENT_FOCUS_FALLBACKS);
  // The versions the document's editor reads, which its head names the latest of: newer than the
  // issue's own read once a version is named here.
  const { toolbars } = useDocumentToolbars();

  useEffect(() => {
    const held = box.current;
    if (held === null) return;
    const leave = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      const active = document.activeElement;
      if (!(active instanceof HTMLElement) || !active.isContentEditable) return;
      event.preventDefault();
      active.blur();
      held.focus({ preventScroll: true });
    };
    held.addEventListener("keydown", leave, true);
    return () => held.removeEventListener("keydown", leave, true);
  }, []);

  // The document's typed blocks are drawn in the page's words: the editor this page's runtime
  // makes is given them once it is made (`ask-block.ts`'s `nameTypedBlocks`). `ask-block.ts` is
  // imported here as `editor.ts` imports it, with `import()`: it is written against the editor's
  // library, which a static import would put in the page's own chunk before its first paint. By
  // the time an editor is made, that lazy chunk is loaded.
  const runtime = useContext(DocumentRuntime);
  const namedRuntime = useMemo<DocumentRuntimeValue>(
    () => ({
      ...runtime,
      createEditor: async (root, options) => {
        const handle = await runtime.createEditor(root, options);
        const { nameTypedBlocks } = await import("../doc/ask-block");
        nameTypedBlocks(handle.view, options.blockSchema, BLOCK_NAMES);
        return handle;
      },
    }),
    [runtime]
  );

  return (
    <div className="flex min-w-0 flex-col gap-3 outline-none" ref={box} tabIndex={-1}>
      <div>
        <button
          className={pageHeaderControl}
          onClick={() => followWayBack(backToDocuments(issueKey, artifactRoute.slug))}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.back_to_documents}
        </button>
      </div>
      <ArtifactRoutePanel
        artifact={artifact}
        artifactRoute={artifactRoute}
        isClosed={isClosed}
        versions={artifact === undefined ? undefined : toolbars.get(artifact.id)?.versions}
      >
        {artifact === undefined || artifact.kind !== "doc" ? null : (
          <DocumentRuntime.Provider value={namedRuntime}>
            <OpenedEditor
              artifact={artifact}
              askId={item?.kind === "ask" ? item.id : undefined}
              commentId={item?.kind === "comment" ? item.id : undefined}
              highlightTerm={highlightTerm}
              isClosed={isClosed}
              issueKey={issueKey}
              onToolbarChange={onToolbarChange}
              onVersionChange={(version) => {
                onShowDiff(false);
                onVersionChange(version);
              }}
              showDiff={showDiff}
              version={artifactRoute.version}
            />
          </DocumentRuntime.Provider>
        )}
      </ArtifactRoutePanel>
    </div>
  );
}

/**
 * The opened document's one editor, on its document's stage (`document-stage.tsx`), which rests
 * in this box. `StagedDocument` is a row's editor, with no landing, version or search term of
 * its own, so the document an address opens is drawn as `ArtifactDocument` draws one: on the
 * item the address names, marked, at the version it pins, with the words it searches for
 * marked, in the box a row's editor scrolls in (`EditorBox`).
 */
function OpenedEditor({
  artifact,
  askId,
  commentId,
  highlightTerm,
  isClosed,
  issueKey,
  onToolbarChange,
  onVersionChange,
  showDiff,
  version,
}: {
  artifact: Artifact;
  askId: string | undefined;
  commentId: string | undefined;
  highlightTerm: string | undefined;
  isClosed: boolean;
  issueKey: string;
  onToolbarChange: (next: DocumentToolbar | undefined) => void;
  onVersionChange: (version: number | null) => void;
  showDiff: boolean;
  version: number | undefined;
}): ReactNode {
  const stage = useDocumentStage(artifact.id);
  return (
    <>
      <div className="flex min-w-0 flex-col" ref={stage} />
      <DocumentStage artifactId={artifact.id}>
        <EditorBox>
          <ArtifactDocument
            artifact={artifact}
            askId={askId}
            commentId={commentId}
            // The editor's reads of who is reading, the decisions and an open question's thread
            // are kept fresh by events, as the outline's are: under the app's 30 s `staleTime`, a
            // document opened later would ask for each again (R14).
            freshness={EVENT_FRESH}
            highlightTerm={highlightTerm}
            isClosed={isClosed}
            onToolbarChange={onToolbarChange}
            onVersionChange={onVersionChange}
            owner={{ key: issueKey, kind: "issue" }}
            showDiff={showDiff}
            version={version}
          />
        </EditorBox>
      </DocumentStage>
    </>
  );
}
