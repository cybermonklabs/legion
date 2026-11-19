import { type ReactNode, useEffect, useRef } from "react";
import { useNavigate } from "react-router-dom";

import type { Artifact } from "../../api/types";
import {
  focusVisibleRing,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { ArtifactRoutePanel } from "../artifacts/ArtifactRoutePanel";
import type { DocumentToolbar } from "../doc/ProofDocument";
import type { IssueRoute } from "../refs/routes";
import { useKeymap } from "../shell/keymap";
import { pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { StagedDocument } from "./StagedDocument";

/**
 * A document or file of the issue, opened in the main column in place of the outline
 * (dispatch://INFRA-1196 R3): what its own address `/issues/<key>/artifacts/<slug>` draws, and
 * what choosing it in the node menu opens. A document is drawn with its editor, so the caret,
 * the collaborative cursors and the comment-on-selection chord work in it as they do anywhere a
 * document is edited; its versions, their downloads and what references it are drawn under it.
 *
 * Escape returns to the outline, in the key table's order (the Design's keyboard table): a
 * caret in the document's text is left first, and the next Escape closes the document. The
 * editor takes Escape for itself before the page's keyboard registry sees it, so the caret is
 * left by a listener of this box's own in the capture phase, as a row of the outline leaves its
 * caret (`useNodeEditing.ts`); the close is the registry's, which a menu or a dialog that is
 * open over the page answers first.
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
  outline,
  showDiff,
  toolbar,
}: {
  artifact: Artifact | undefined;
  artifactRoute: Extract<IssueRoute, { kind: "artifact" }>;
  highlightTerm: string | undefined;
  isClosed: boolean;
  issueKey: string;
  /** The ask or comment the address names for the document to land on, marked in its text
   *  (`issue-address.ts`'s `documentItem`), and whether the address names it by `?mark=`. */
  item:
    | { readonly kind: "ask" | "comment"; readonly id: string; readonly marked?: boolean }
    | undefined;
  onShowDiff: (next: boolean) => void;
  onToolbarChange: (next: DocumentToolbar | undefined) => void;
  onVersionChange: (version: number | null) => void;
  /** The address of the outline the reader returns to. */
  outline: string;
  showDiff: boolean;
  toolbar: DocumentToolbar | undefined;
}): ReactNode {
  const navigate = useNavigate();
  const box = useRef<HTMLDivElement>(null);

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

  useKeymap("issue", [
    {
      id: "close-document",
      keys: "Escape",
      label: CONTROL_LABELS.close_document,
      palette: false,
      run: () => navigate(outline),
      // A menu open over the document closes first, on the same press (`IssuePresets.tsx`).
      when: () => document.querySelector('[role="menu"]') === null,
    },
  ]);

  return (
    <div className="flex min-w-0 flex-col gap-3 outline-none" ref={box} tabIndex={-1}>
      <div>
        <button
          className={`inline-flex min-h-11 items-center rounded-lg px-2 text-sm font-medium md:min-h-9 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${focusVisibleRing}`}
          onClick={() => navigate(outline)}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.close_document}
        </button>
      </div>
      <ArtifactRoutePanel
        artifact={artifact}
        artifactRoute={artifactRoute}
        isClosed={isClosed}
        toolbar={toolbar}
      >
        {artifact === undefined || artifact.kind !== "doc" ? null : (
          <StagedDocument
            artifact={artifact}
            askId={item?.kind === "ask" ? item.id : undefined}
            commentId={item?.kind === "comment" ? item.id : undefined}
            highlightTerm={highlightTerm}
            isClosed={isClosed}
            issueKey={issueKey}
            marked={item?.marked}
            onToolbarChange={onToolbarChange}
            onVersionChange={(version) => {
              onShowDiff(false);
              onVersionChange(version);
            }}
            showDiff={showDiff}
            version={artifactRoute.version}
          />
        )}
      </ArtifactRoutePanel>
    </div>
  );
}
