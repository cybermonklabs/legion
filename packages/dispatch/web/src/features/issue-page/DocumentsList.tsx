import { type ReactNode, useEffect, useId, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import type { Artifact } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import {
  focusVisibleRing,
  pageField,
  pageHairBorder,
  pageHeaderControl,
  pageInkBorder,
  pageInkText,
  pageLabelChip,
  pageMutedText,
  pageQuietText,
  pageRowBorder,
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
import { buildIssuePath, documentRoute } from "../refs/routes";
import { useListPages } from "./details-leaves";
import { shortDay } from "./node-time";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS, EMPTY_STATE_LINES, FILE_WORDS } from "./page-words";
import { inputMethodKey } from "./useNodeEditing";

/** The mark Documents' list draws its box with, by which `t a` finds it (`useIssueKeymap.ts`). */
export const DOCUMENTS_LIST_ATTRIBUTE = "data-documents-list";

/** The mark of each row's link to its document: the first row's takes the focus `t a` gives the
 *  list (`useIssueKeymap.ts`). */
export const DOCUMENT_OPEN_ATTRIBUTE = "data-document-open";

/**
 * Documents' list (dispatch://INFRA-1196 part N's R1, R2): every document and file of the issue,
 * drawn by the stage in the main column in place of the outline and of every view, at
 * `/issues/<key>/documents` and at today's Artifacts tab's `/artifacts` (`slots/stage.tsx`).
 *
 * It lists the issue read's `artifacts` and reads nothing of its own: the spec, the plan and the
 * Verification document among them, on today's model as on an issue the conversion has not
 * reached. The issue's spec first, then the rest by their latest version's time, newest first, as
 * the Artifacts list ordered them. Each row is whole (root R40): its name, a link that opens the
 * document at its own address in place of the list; its kind, with the Primary mark on the spec;
 * its latest version and the day it was made; its copy control, its latest version's download
 * and, where the issue can drop it, Remove. The list draws 20 rows at a time, with the control
 * that says how many are left and draws the next 20 (part M's `useListPages`).
 *
 * Above the rows stand "Filter files and documents", which narrows them by name and by kind,
 * Upload and New document. Upload is `main`'s upload (`useArtifactUpload`): a file picked, or
 * dropped on the list, is staged with its name, a Summary for the version it makes, Upload and
 * Cancel, and an upload under the name of a document the issue holds names the version it
 * replaces. New document is `main`'s making of a document from a title (`useCreateDocument`),
 * which refuses a title the issue holds; its form takes the control's place, with the caret in its
 * Title. A closed issue refuses both, each control drawn refused with the reason inside it.
 *
 * Escape on the list is the list's own first (part N's R1, the plan's ST-2): in "Filter files and
 * documents" with words in it, it clears them, as the box's own Escape does; else, with a file
 * staged, it cancels the upload, as Cancel does, and sends nothing. Each takes the press from the
 * page's keyboard registry, which hears it after this does; with neither, the page's own step
 * leaves the list (`escape-order.ts`' `documents-leave`).
 *
 * Its rows are links and buttons, and no menu: nothing in it has a menu's role.
 */
export function DocumentsList({
  artifacts,
  isClosed,
  issueKey,
}: {
  /** Every artifact of the issue, from the issue read: each is a row, and a title one of them
   *  already holds is refused before New document writes it. */
  artifacts: readonly Artifact[];
  isClosed: boolean;
  issueKey: string;
}): ReactNode {
  // A closed issue takes no upload and no new document, and each control says so in the one
  // sentence every way of adding to a closed issue says (`CLOSED_ISSUE_DOCUMENT_REFUSAL`).
  const refusal = isClosed ? CLOSED_ISSUE_DOCUMENT_REFUSAL : null;
  const upload = useArtifactUpload({ issue: issueKey }, { refusal });
  const creation = useCreateDocument({ documents: artifacts, owner: { issue: issueKey }, refusal });
  const uploadLabelId = useId();
  const uploadRefusalId = useId();
  const newDocumentLabelId = useId();
  const newDocumentRefusalId = useId();
  const [filter, setFilter] = useState("");
  const wanted = filter.trim().toLocaleLowerCase();
  const listed = useMemo(
    () =>
      [...artifacts]
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
        ),
    [artifacts, wanted]
  );
  const titleForm = useRef<HTMLDivElement>(null);
  // The form New document opens takes the control's place, so the caret goes where the reader
  // asked to type rather than to the page's body with the control that went.
  useEffect(() => {
    if (creation.open) titleForm.current?.querySelector("input")?.focus();
  }, [creation.open]);
  const section = useRef<HTMLElement>(null);
  const filterBox = useRef<HTMLInputElement>(null);
  // What the list's own Escape reads as it is pressed: the filter's words, and the staged file's
  // Cancel.
  const escaped = useRef({ cancel: upload.cancel, filter, staged: upload.pendingFile !== null });
  escaped.current = { cancel: upload.cancel, filter, staged: upload.pendingFile !== null };
  // The list's own Escape, heard on the document, where it bubbles before it reaches the page's
  // registry on the window: wherever the focus is in the list, or nowhere, as it is once Upload's
  // pick has taken Upload away. New document's Title keeps its press, so a staged file is not
  // dropped while a title is typed, and an input method's own key is no press of the list's
  // (`inputMethodKey`).
  useEffect(() => {
    const press = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented || inputMethodKey(event)) return;
      const active = document.activeElement;
      const { cancel, filter, staged } = escaped.current;
      if (active !== null && active === filterBox.current && filter !== "") {
        event.preventDefault();
        setFilter("");
        return;
      }
      const fromList =
        active === null ||
        active === document.body ||
        (section.current?.contains(active) === true &&
          !(active.matches("input") && titleForm.current?.contains(active) === true));
      if (!staged || !fromList) return;
      event.preventDefault();
      cancel();
    };
    document.addEventListener("keydown", press);
    return () => document.removeEventListener("keydown", press);
  }, []);

  const control = `${pageHeaderControl} refused:cursor-not-allowed`;
  // A refused control stays where it is with `aria-disabled`, so a reader on the keyboard reaches
  // it and hears why: its name is its label and the reason, drawn inside it, its description.
  const uploadControl = (
    <button
      aria-describedby={upload.refusal === null ? undefined : uploadRefusalId}
      aria-disabled={upload.refusal === null ? undefined : true}
      aria-labelledby={upload.refusal === null ? undefined : uploadLabelId}
      className={control}
      onClick={() => upload.openPicker()}
      type="button"
      {...pageChrome}
    >
      <span id={uploadLabelId}>{CONTROL_LABELS.upload}</span>
      {upload.refusal === null ? null : (
        <span className={`font-normal ${pageQuietText}`} id={uploadRefusalId}>
          {upload.refusal}
        </span>
      )}
    </button>
  );
  const newDocumentControl = (
    <button
      aria-describedby={refusal === null ? undefined : newDocumentRefusalId}
      aria-disabled={refusal === null ? undefined : true}
      aria-labelledby={refusal === null ? undefined : newDocumentLabelId}
      className={control}
      onClick={() => {
        if (refusal === null) creation.start();
      }}
      type="button"
      {...pageChrome}
    >
      <span id={newDocumentLabelId}>{CONTROL_LABELS.new_document}</span>
      {refusal === null ? null : (
        <span className={`font-normal ${pageQuietText}`} id={newDocumentRefusalId}>
          {refusal}
        </span>
      )}
    </button>
  );

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: a passive drop surface - Upload, inside it, is the keyboard's way to add a file.
    <section
      className={`flex min-w-0 flex-col gap-3 rounded-[10px] outline-none ${upload.dropTarget.isDragging ? `border border-dashed ${pageInkBorder}` : ""}`}
      onDragLeave={upload.dropTarget.onDragLeave}
      onDragOver={upload.dropTarget.onDragOver}
      onDrop={upload.dropTarget.onDrop}
      ref={section}
      tabIndex={-1}
      {...{ [DOCUMENTS_LIST_ATTRIBUTE]: "" }}
    >
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <input
          aria-label={CONTROL_LABELS.filter_files}
          className={`min-h-6 w-full min-w-0 rounded-[7px] border px-2 py-1 text-sm outline-none sm:w-72 ${pageField} ${focusVisibleRing}`}
          onChange={(event) => setFilter(event.target.value)}
          placeholder={CONTROL_LABELS.filter_files}
          ref={filterBox}
          type="search"
          value={filter}
        />
        {/* While a file is staged its own Upload sends it (below), so the pick stands down. */}
        {upload.pendingFile === null ? uploadControl : null}
        <div className="contents" ref={titleForm}>
          {creation.strip(newDocumentControl)}
        </div>
      </div>
      {upload.pendingFile === null ? (
        <ArtifactUploadStatus upload={upload} />
      ) : (
        <ArtifactUploadRow upload={upload} />
      )}
      {/* The chooser Upload opens: Upload is the way to it, so it takes no stop of Tab's. */}
      <input
        aria-label={CONTROL_LABELS.upload_file}
        className="sr-only left-0"
        disabled={upload.refusal !== null}
        onChange={upload.selectFile}
        ref={upload.fileInputRef}
        tabIndex={-1}
        type="file"
      />
      {listed.length === 0 && artifacts.length > 0 ? (
        <p className={`text-sm ${pageQuietText}`} {...pageChrome}>
          {EMPTY_STATE_LINES.files_filtered}
        </p>
      ) : null}
      {/* Its pages start again at 20 for each filter's words. */}
      <DocumentRows isClosed={isClosed} issueKey={issueKey} key={wanted} listed={listed} />
    </section>
  );
}

/** The rows of the list, 20 at a time, with the control that draws the next 20 under them. */
function DocumentRows({
  isClosed,
  issueKey,
  listed,
}: {
  isClosed: boolean;
  issueKey: string;
  listed: readonly Artifact[];
}): ReactNode {
  const list = useRef<HTMLUListElement>(null);
  const pages = useListPages();
  // The list's own address, which a document opened from it goes back to (the location's
  // `outline`, `document-address.ts`).
  const back = buildIssuePath({ key: issueKey, kind: "issue-documents" });
  if (listed.length === 0) return null;
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <ul className={`min-w-0 border-t ${pageHairBorder}`} ref={list}>
        {listed.slice(0, pages.drawn).map((artifact) => (
          <DocumentRow
            artifact={artifact}
            back={back}
            isClosed={isClosed}
            issueKey={issueKey}
            key={artifact.slug}
          />
        ))}
      </ul>
      {pages.more(listed.length, (index) => list.current?.children[index])}
    </div>
  );
}

/**
 * One document or file of the issue as a row of the list, whole: its name, the link that opens it;
 * then its kind, the Primary mark on the spec, its latest version and the day it was made, its
 * copy control, the download of its latest version and, for a document the issue can drop,
 * Remove - what its row of the Artifacts list offered (R19, c-nothing-lost).
 */
function DocumentRow({
  artifact,
  back,
  isClosed,
  issueKey,
}: {
  artifact: Artifact;
  /** The list's address, which the document opened from this row goes back to. */
  back: string;
  isClosed: boolean;
  issueKey: string;
}): ReactNode {
  const latest = versionsNewestFirst(artifact.versions)[0];
  const day = shortDay(latest?.created_at ?? artifact.created_at);
  return (
    <li className={`flex min-w-0 flex-col gap-1 border-b py-2 ${pageRowBorder}`}>
      {/* The document's own address, the spec's included: `/spec` is the Spec view, so the
          spec's reference would open the view and not the spec. */}
      <Link
        className={`min-w-0 self-start text-sm font-medium [overflow-wrap:anywhere] hover:underline ${pageInkText} ${focusVisibleRing}`}
        state={{ outline: back }}
        to={buildIssuePath({ key: issueKey, kind: "artifact", slug: artifact.slug })}
        {...{ [DOCUMENT_OPEN_ATTRIBUTE]: "" }}
      >
        <span {...authorText}>{artifact.name}</span>
      </Link>
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-xs">
        <span className={pageMutedText} {...pageChrome}>
          {artifact.kind === "doc" || artifact.kind === "image"
            ? FILE_WORDS[artifact.kind]
            : FILE_WORDS.file}
        </span>
        {artifact.primary ? (
          <span
            className={`rounded-full border px-2 font-semibold leading-5 ${pageLabelChip}`}
            {...pageChrome}
          >
            {FILE_WORDS.primary}
          </span>
        ) : null}
        {/* Its last change: the latest version, and the day it was made. */}
        <span className={pageQuietText} {...pageChrome}>
          {latest === undefined ? null : (
            <>
              {FILE_WORDS.version} <span {...authorText}>{latest.number}</span>
              {day === "" ? null : " · "}
            </>
          )}
          {day === "" ? null : <span {...authorText}>{day}</span>}
        </span>
        <CopyRefButton route={documentRoute(artifact)} />
        {latest === undefined ? null : (
          <a
            className={`font-medium underline hover:text-page-ink ${pageMutedText} ${focusVisibleRing}`}
            download=""
            href={artifactVersionUrl(artifact.id, latest.number, { download: true })}
          >
            <span {...pageChrome}>{CONTROL_LABELS.download_version}</span>{" "}
            <span {...authorText}>{latest.number}</span>
          </a>
        )}
        {removalIsOffered(artifact) ? (
          // Remove is drawn by today's control (`DocumentWayBack.tsx`), whose words are the
          // page's own too (part N's R9): the box marks them so, and draws no box of its own.
          <div className="contents" {...pageChrome}>
            <RemoveDocument
              artifact={artifact}
              refusal={isClosed ? CLOSED_ISSUE_REMOVE_REFUSAL : null}
            />
          </div>
        ) : null}
      </div>
    </li>
  );
}
