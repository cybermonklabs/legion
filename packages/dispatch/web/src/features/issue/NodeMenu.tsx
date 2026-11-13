import { type ReactNode, useEffect, useId, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import type { Artifact } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import {
  badgePrimary,
  borderDefault,
  calloutInfoBorder,
  card,
  focusVisibleRing,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { artifactVersionUrl, versionsNewestFirst } from "../artifacts/ArtifactHeader";
import {
  ArtifactUploadRow,
  ArtifactUploadStatus,
  useArtifactUpload,
} from "../artifacts/ArtifactUpload";
import { CLOSED_ISSUE_DOCUMENT_REFUSAL, useCreateDocument } from "../artifacts/CreateDocument";
import {
  CLOSED_ISSUE_REMOVE_REFUSAL,
  RemoveDocument,
  removalIsOffered,
} from "../artifacts/DocumentWayBack";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath, documentRoute, parseIssuePath } from "../refs/routes";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS, EMPTY_STATE_LINES, FILE_WORDS } from "./page-words";

/**
 * The node menu beside the presets (dispatch://INFRA-1196 R3, R20, and the Design's "Where each
 * thing goes": the Artifacts tab's list, its filter, its upload and New document).
 *
 * There is no Artifacts tab. A document is a node of the issue and not of its conversation,
 * so the tree draws none (R1, R3), and every document and artifact of the issue is here
 * instead, with what the Artifacts list offered on each: its kind, the Primary mark on the spec,
 * its copy control, the download of its latest version and, for a document the issue can drop,
 * Remove. Choosing one opens it at its
 * own address, which draws it in the main column in place of the outline, the issue's spec as
 * much as any other (`OpenedDocument.tsx`), where its whole version history and what
 * references it are. The list's filter narrows the menu, and the upload and New document are
 * items of it; a file picked or dropped on the open menu is staged, as one dropped on the list
 * was: its name, an optional summary of the version it makes, and Upload and Cancel.
 *
 * The file input lives beside the menu rather than inside it, so a picker opened from a menu
 * item outlives the menu closing. The staged file and the title form New document opens are
 * beside the menu for the same reason: each takes the line under the preset row, the form with
 * the caret in its Title.
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
  const { pathname, search } = useLocation();
  // The outline the reader opens a document from, which Escape and the document's own control
  // go back to (`OpenedDocument.tsx`): the address they are on, unless it is a document's or
  // the node menu's own, which would open a document or this menu again.
  const here = parseIssuePath(pathname);
  const outline =
    here === undefined || here.kind === "artifact" || here.kind === "artifacts"
      ? buildIssuePath({ key: issueKey, kind: "issue" })
      : `${pathname}${search}`;
  // A closed issue takes no upload, and the item says so in the sentence a closed issue's
  // document controls say: one sentence for every way of adding to a closed issue, as
  // `CLOSED_ISSUE_DOCUMENT_REFUSAL` is declared to be.
  const refusal = isClosed ? CLOSED_ISSUE_DOCUMENT_REFUSAL : null;
  const upload = useArtifactUpload({ issue: issueKey }, { refusal });
  const uploadLabelId = useId();
  const uploadRefusalId = useId();
  const newDocumentLabelId = useId();
  const newDocumentRefusalId = useId();
  // A closed issue takes no new document: the item stays, refused, and says why, as Upload
  // does and as the Artifacts list it replaces did. The refusal also guards a form left open
  // while the issue closed.
  const creation = useCreateDocument({ documents: artifacts, owner: { issue: issueKey }, refusal });
  const [filter, setFilter] = useState("");
  // The issue's spec first, then the rest by the latest version's time, newest first, as the
  // Artifacts list ordered them; the filter narrows them by name or kind, as its own did.
  const listed = useMemo(() => {
    const wanted = filter.trim().toLocaleLowerCase();
    return [...artifacts]
      .sort((left, right) => {
        if (left.primary !== right.primary) return left.primary ? -1 : 1;
        const leftUpdated = (left.versions ?? []).at(-1)?.created_at ?? left.created_at;
        const rightUpdated = (right.versions ?? []).at(-1)?.created_at ?? right.created_at;
        return compareTimestamps(rightUpdated, leftUpdated) || left.id.localeCompare(right.id);
      })
      .filter(
        (artifact) =>
          wanted === "" ||
          artifact.name.toLocaleLowerCase().includes(wanted) ||
          artifact.kind.toLocaleLowerCase().includes(wanted)
      );
  }, [artifacts, filter]);
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
          in it and the arrow keys reach its items. The box around it holds the filter, which a
          menu cannot own, and takes a dropped file, as the Artifacts list it replaces did. */}
        {open ? (
          // biome-ignore lint/a11y/noStaticElementInteractions: a passive drop surface - Upload, inside it, is the keyboard's way to add a file.
          <div
            className={`${MENU_CLASS} ${upload.dropTarget.isDragging ? `border-dashed ${calloutInfoBorder}` : ""}`}
            onDragLeave={upload.dropTarget.onDragLeave}
            onDragOver={upload.dropTarget.onDragOver}
            onDrop={upload.dropTarget.onDrop}
          >
            <input
              aria-label={CONTROL_LABELS.filter_files}
              className={`min-h-11 w-full min-w-0 rounded border px-2 py-1 text-sm md:min-h-9 ${inputClasses(true)}`}
              onChange={(event) => setFilter(event.target.value)}
              type="search"
              value={filter}
            />
            {/* A staged pick is drawn here, under the filter, while the menu is open: drawn
              under the preset row it would be under the open menu, which covers that line. */}
            {upload.pendingFile === null ? null : <ArtifactUploadRow upload={upload} />}
            {listed.length === 0 && artifacts.length > 0 ? (
              <p className={`px-2 text-xs ${textMutedOnSurface}`} {...pageChrome}>
                {EMPTY_STATE_LINES.files_filtered}
              </p>
            ) : null}
            <div className="flex flex-col gap-1" role="menu" tabIndex={-1}>
              {listed.map((artifact) => (
                <NodeMenuItem
                  artifact={artifact}
                  isClosed={isClosed}
                  key={artifact.slug}
                  onOpen={() => {
                    onClose();
                    // The document's own address, the spec's included: `/spec` is the Spec
                    // filter, so the spec's reference would open the outline and not the spec.
                    navigate(
                      buildIssuePath({ key: issueKey, kind: "artifact", slug: artifact.slug }),
                      { state: { outline } }
                    );
                  }}
                />
              ))}
              {/* Refused, an item stays in the menu with `aria-disabled`, so a reader on the
                keyboard reaches it and hears why, and its reason is drawn inside it: the menu
                owns menu items alone (above), so the line cannot stand beside it. The name
                stays the label, and the reason is the item's description. */}
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
              <button
                aria-describedby={refusal === null ? undefined : newDocumentRefusalId}
                aria-disabled={refusal === null ? undefined : true}
                aria-labelledby={refusal === null ? undefined : newDocumentLabelId}
                className={`${MENU_ITEM_CLASS} refused:cursor-not-allowed`}
                onClick={() => {
                  if (refusal !== null) return;
                  onClose();
                  creation.start();
                }}
                role="menuitem"
                type="button"
                {...pageChrome}
              >
                <span id={newDocumentLabelId}>{CONTROL_LABELS.new_document}</span>
                {refusal === null ? null : (
                  <span
                    className={`block text-xs font-normal ${textMutedOnSurface}`}
                    id={newDocumentRefusalId}
                  >
                    {refusal}
                  </span>
                )}
              </button>
            </div>
          </div>
        ) : null}
        {/* The chooser the item above opens, beside the menu rather than inside it, so a pick
          outlives the menu closing. */}
        <input
          aria-label={CONTROL_LABELS.upload_file}
          className="sr-only left-0"
          disabled={upload.refusal !== null}
          onChange={upload.selectFile}
          ref={upload.fileInputRef}
          type="file"
        />
        {upload.pendingFile === null ? <ArtifactUploadStatus upload={upload} /> : null}
        {/* Closed, the strip holds nothing but what the last write said where it landed with no
          answer to open, which stays beside the control that made it. */}
        {creation.open ? null : creation.strip(null)}
      </div>
      {creation.open ? (
        <div className="order-last basis-full" ref={titleForm}>
          {creation.strip(null)}
        </div>
      ) : null}
      {upload.pendingFile === null || open ? null : (
        <div className="order-last basis-full">
          <ArtifactUploadRow upload={upload} />
        </div>
      )}
    </>
  );
}

/**
 * One artifact of the issue as an item of the menu: the control that opens it, its copy
 * control, the download of its latest version and, for a document the issue can drop, Remove -
 * what its row of the Artifacts list offered (R19, c-nothing-lost).
 *
 * The row itself is the `menuitem`, not the controls inside it. A `menu` may own nothing but
 * `menuitem`, `menuitemcheckbox`, `menuitemradio` and `group`, and axe reads a `button` owned by
 * one as `aria-required-children`, critical. The copy control cannot answer it by becoming a
 * `menuitem`, because the inventory of the page this one replaced records it as
 * `button "Copy reference dispatch://…"` (`e2e/fixtures/issue-page-inventory.json`), and the
 * download is a link and Remove a button there too. So the row carries the role and the
 * controls inside it stay what they were. The row takes no place in the tab order of its own:
 * Tab reaches the controls inside it. The row the chord that opens the menu leaves the focus on
 * (`t a`) takes Enter and Space as its open control's, so the keyboard opens a document from
 * where it lands.
 */
function NodeMenuItem({
  artifact,
  isClosed,
  onOpen,
}: {
  artifact: Artifact;
  isClosed: boolean;
  onOpen: () => void;
}): ReactNode {
  const latest = versionsNewestFirst(artifact.versions)[0];
  return (
    <div
      className="flex min-w-0 flex-col"
      onKeyDown={(event) => {
        if (event.target !== event.currentTarget) return;
        if (event.key !== "Enter" && event.key !== " ") return;
        event.preventDefault();
        onOpen();
      }}
      role="menuitem"
      tabIndex={-1}
    >
      <button className={`min-w-0 ${MENU_ITEM_CLASS}`} onClick={onOpen} type="button">
        <span {...authorText}>{artifact.name}</span>
      </button>
      <div className="flex min-w-0 flex-wrap items-center gap-2 px-2 pb-1">
        <span className={`text-xs ${textMutedOnSurface}`} {...pageChrome}>
          {artifact.kind === "doc" || artifact.kind === "image"
            ? FILE_WORDS[artifact.kind]
            : FILE_WORDS.file}
        </span>
        {artifact.primary ? (
          <span
            className={`rounded-full px-2 py-0.5 text-xs font-semibold ${badgePrimary.bg} ${badgePrimary.text}`}
            {...pageChrome}
          >
            {FILE_WORDS.primary}
          </span>
        ) : null}
        <CopyRefButton route={documentRoute(artifact)} />
        {latest === undefined ? null : (
          <a
            className={`text-sm font-medium underline ${linkText} ${linkHoverText}`}
            download=""
            href={artifactVersionUrl(artifact.id, latest.number)}
          >
            <span {...pageChrome}>{CONTROL_LABELS.download_version}</span>{" "}
            <span {...authorText}>{latest.number}</span>
          </a>
        )}
        {removalIsOffered(artifact) ? (
          <RemoveDocument
            artifact={artifact}
            refusal={isClosed ? CLOSED_ISSUE_REMOVE_REFUSAL : null}
          />
        ) : null}
      </div>
    </div>
  );
}

/** The one shape both menus of the preset row wear: the presets' own below 1025 px, and the
 *  node menu's at every width. */
export const MENU_CLASS = `absolute left-0 top-full z-30 mt-1 flex w-56 flex-col gap-1 rounded-lg border p-2 shadow-lg ${borderDefault} ${card}`;

export const MENU_ITEM_CLASS = `min-h-11 rounded-lg px-2 py-2 text-left text-sm font-medium ${textSecondaryOnSurface}`;

/** The node menu's own control, which sits beside the preset row and outside it. */
const NODE_MENU_BUTTON_CLASS = `inline-flex min-h-11 items-center rounded-lg px-2 text-sm font-medium md:min-h-9 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${focusVisibleRing}`;
