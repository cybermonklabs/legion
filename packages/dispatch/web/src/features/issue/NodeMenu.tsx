import { type ReactNode, useEffect, useId, useRef } from "react";
import { useNavigate } from "react-router-dom";

import type { Artifact } from "../../api/types";
import {
  borderDefault,
  card,
  focusVisibleRing,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ArtifactUploadStatus, useArtifactUpload } from "../artifacts/ArtifactUpload";
import { CLOSED_ISSUE_DOCUMENT_REFUSAL, useCreateDocument } from "../artifacts/CreateDocument";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildReferencePath, documentRoute } from "../refs/routes";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * The node menu beside the presets (dispatch://INFRA-1196 R3,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t18`).
 *
 * There is no Artifacts preset. A document is a node of the issue and not of its conversation,
 * so the tree draws none (R1, R3), and every document and artifact of the issue is here
 * instead: each opens at its own address. The upload is here too, because adding a file is the
 * one thing about an issue's files that no node answers for, and so is making a document from
 * a title (`CreateDocument.tsx`), which adds one the same way.
 *
 * The file input lives beside the menu rather than inside it, so a picker opened from a menu
 * item outlives the menu closing. The menu takes no dropped file: a file dropped on the issue's
 * `/artifacts` list uploads (`ArtifactsTab.tsx`'s `ArtifactDropZone`), as it did on the
 * Artifacts tab that list was. The title form New document opens is beside the menu for the
 * same reason as the input: the item closes the menu, and the form takes the line under the
 * preset row, with the caret in its Title.
 */
export function NodeMenu({
  artifacts,
  isClosed,
  issueKey,
  onClose,
  onToggle,
  open,
}: {
  /** Every artifact of the issue: each is an item of the menu, and a title one of them already
   *  holds is refused before New document writes it. */
  artifacts: readonly Artifact[];
  isClosed: boolean;
  issueKey: string;
  /** Closes the menu once an item has acted. */
  onClose: () => void;
  onToggle: () => void;
  open: boolean;
}): ReactNode {
  const navigate = useNavigate();
  // A closed issue takes no upload, and the item says so in the sentence the Artifacts panel
  // says beside both of its controls there (`ArtifactsTab.tsx`): one sentence for every way of
  // adding to a closed issue, as `CLOSED_ISSUE_DOCUMENT_REFUSAL` is declared to be.
  const upload = useArtifactUpload(
    { issue: issueKey },
    { immediate: true, refusal: isClosed ? CLOSED_ISSUE_DOCUMENT_REFUSAL : null }
  );
  const uploadLabelId = useId();
  const uploadRefusalId = useId();
  // A closed issue takes no new document, as the Artifacts panel's own control says; this page
  // offers no write a closed issue refuses (`IssueAddMenu.tsx`, `DecisionActions.tsx`), so the
  // item is not drawn there. The refusal still guards a form left open while the issue closed.
  const creation = useCreateDocument({
    documents: artifacts,
    owner: { issue: issueKey },
    refusal: isClosed ? CLOSED_ISSUE_DOCUMENT_REFUSAL : null,
  });
  const titleForm = useRef<HTMLDivElement>(null);
  // The item that opened the form closed the menu and left the page with it, so the caret goes
  // where the reader asked to type rather than to the page's body.
  useEffect(() => {
    if (creation.open) titleForm.current?.querySelector("input")?.focus();
  }, [creation.open]);
  return (
    <>
      <div className="relative shrink-0">
        <button
          aria-expanded={open}
          aria-haspopup="menu"
          className={NODE_MENU_BUTTON_CLASS}
          onClick={onToggle}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.other_nodes}
        </button>
        {/* The menu itself takes the focus, so the chord that opens it (`t a`) leaves the reader
          in it and the arrow keys reach its items. */}
        {open ? (
          <div className={MENU_CLASS} role="menu" tabIndex={-1}>
            {artifacts.map((artifact) => (
              // The item and the artifact's own copy control, side by side: an artifact is
              // reachable here and from no node of the tree, so this is where it keeps the copy
              // control every node of the page has (R19, c-nothing-lost).
              //
              // The row itself is the `menuitem`, not the button inside it. A `menu` may own
              // nothing but `menuitem`, `menuitemcheckbox`, `menuitemradio` and `group`, and axe
              // reads a `button` owned by one - which the copy control was, whether a wrapper
              // sat between them or not - as `aria-required-children`, critical: "Element has
              // children which are not allowed: button[aria-label]". The copy control cannot
              // answer it by becoming a `menuitem`, because the inventory of the page this one
              // replaced records it as `button "Copy reference dispatch://…"`
              // (`e2e/fixtures/issue-page-inventory.json`) and c-nothing-lost holds this page to
              // that role. So the row carries the role, the two controls inside it stay the
              // buttons they are, and the menu owns menu items alone. The row takes no place in
              // the tab order of its own, as the menu around it does not: Tab reaches the two
              // buttons inside it, which is where it reached them before.
              <div
                className="flex min-w-0 items-center gap-1"
                key={artifact.slug}
                role="menuitem"
                tabIndex={-1}
              >
                <button
                  className={`min-w-0 flex-1 ${MENU_ITEM_CLASS}`}
                  onClick={() => {
                    onClose();
                    navigate(buildReferencePath(documentRoute(artifact)));
                  }}
                  type="button"
                >
                  <span {...authorText}>{artifact.name}</span>
                </button>
                <CopyRefButton route={documentRoute(artifact)} />
              </div>
            ))}
            {/* Refused, the item stays in the menu with `aria-disabled`, so a reader on the
              keyboard reaches it and hears why, and its reason is drawn inside it: the menu owns
              menu items alone (above), so the line cannot stand beside it. The name stays the
              label, and the reason is the item's description. */}
            <button
              aria-describedby={upload.refusal === null ? undefined : uploadRefusalId}
              aria-disabled={upload.refusal === null ? undefined : true}
              aria-labelledby={upload.refusal === null ? undefined : uploadLabelId}
              className={`${MENU_ITEM_CLASS} refused:cursor-not-allowed`}
              onClick={() => upload.openPicker()}
              role="menuitem"
              type="button"
              {...pageChrome}
            >
              <span id={uploadLabelId}>{CONTROL_LABELS.upload}</span>
              {upload.refusal === null ? null : (
                <span
                  className={`block text-xs font-normal ${textMutedOnSurface}`}
                  id={uploadRefusalId}
                >
                  {upload.refusal}
                </span>
              )}
            </button>
            {isClosed ? null : (
              <button
                className={MENU_ITEM_CLASS}
                onClick={() => {
                  onClose();
                  creation.start();
                }}
                role="menuitem"
                type="button"
                {...pageChrome}
              >
                {CONTROL_LABELS.new_document}
              </button>
            )}
          </div>
        ) : null}
        {/* The chooser the item above opens, beside the menu rather than inside it, so a pick
          outlives the menu closing. It is named apart from the Artifacts tab's own chooser,
          which a page drawing both would otherwise offer twice under one name. */}
        <input
          aria-label={CONTROL_LABELS.upload_file}
          className="sr-only left-0"
          disabled={upload.refusal !== null}
          onChange={upload.selectFile}
          ref={upload.fileInputRef}
          type="file"
        />
        <ArtifactUploadStatus upload={upload} />
        {/* Closed, the strip holds nothing but what the last write said where it landed with no
          answer to open, which stays beside the control that made it. */}
        {creation.open ? null : creation.strip(null)}
      </div>
      {creation.open ? (
        <div className="order-last basis-full" ref={titleForm}>
          {creation.strip(null)}
        </div>
      ) : null}
    </>
  );
}

/** The one shape both menus of the preset row wear: the presets' own below 1025 px, and the
 *  node menu's at every width. */
export const MENU_CLASS = `absolute left-0 top-full z-30 mt-1 flex w-56 flex-col gap-1 rounded-lg border p-2 shadow-lg ${borderDefault} ${card}`;

export const MENU_ITEM_CLASS = `min-h-11 rounded-lg px-2 py-2 text-left text-sm font-medium ${textSecondaryOnSurface}`;

/** The node menu's own control, which sits beside the preset row and outside it. */
const NODE_MENU_BUTTON_CLASS = `inline-flex min-h-11 items-center rounded-lg px-2 text-sm font-medium md:min-h-9 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${focusVisibleRing}`;
