import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import {
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  successText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { versionsNewestFirst } from "../artifacts/ArtifactHeader";
import { ConnectionDot } from "../doc/ConnectionDot";
import { NameVersionDialog } from "../doc/NameVersionDialog";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { CopyRefButton } from "../refs/CopyRefButton";
import { documentRoute } from "../refs/routes";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

/** The control's button: a 44 px tap target through tablet widths, 32 px from `md`. */
const toolbarButton = `min-h-11 shrink-0 rounded-lg px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`;

/**
 * The version control beside the filters (dispatch://INFRA-1196 R2, R3, and the Design's "Where
 * each thing goes": the old spec toolbar's version, name a version, diff, copy link and
 * connection dot): which version of one document the page is reading, the control that gives
 * its current text a version of its own, the comparison of a pinned version with the document
 * as it stands, and a link to it.
 *
 * It reads the document the reader opened, and with none opened the document the filter is
 * over, which on an issue the conversion has not reached is the document of the filter's role
 * (R2). Where that document's editor is on the page - the document opened in place of the
 * outline, or a row the reader put the caret in - the control is that editor's: its versions,
 * its naming, its live connection and the link to the block the caret is in. With no editor on
 * the page it reads the versions the issue's own read carries and names a version itself, so a
 * first load reads no document's text (R21); the connection dot and the block link belong to an
 * editor, and are drawn only beside one.
 *
 * It draws no approval: the approval entries stay in the identity block (R31).
 */
export function VersionControl({
  artifact,
  isClosed,
  onShowDiffChange,
  onVersionChange,
  showDiff,
  toolbar,
  version,
}: {
  artifact: Artifact;
  isClosed: boolean;
  onShowDiffChange: (next: boolean) => void;
  onVersionChange: (version: number | null) => void;
  showDiff: boolean;
  /** The document's editor's own controls, where its editor is on the page. */
  toolbar: DocumentToolbar | undefined;
  /** The version the address pins (`?v=`), or undefined for the document as it stands. */
  version: number | undefined;
}): ReactNode {
  const [copyFeedback, setCopyFeedback] = useState<"copied" | "failed" | undefined>(undefined);
  const versions = toolbar?.versions ?? versionsNewestFirst(artifact.versions);
  return (
    <div
      className="flex w-full min-w-0 flex-wrap items-center gap-2 md:ml-auto md:w-auto"
      data-testid="issue-version-control"
    >
      {version === undefined ? null : (
        <span className={`text-xs ${textMutedOnCanvas}`} {...pageChrome}>
          {CONTROL_LABELS.version} <span {...authorText}>{version}</span>
        </span>
      )}
      {/* The control names itself rather than carrying a separate label element: every option
          says "version", so the closed select reads as a version picker and the strip still
          shares the filter row instead of taking a row of its own. */}
      <select
        aria-label={CONTROL_LABELS.version}
        className={`min-h-11 min-w-0 max-w-48 truncate rounded border px-2 py-2 text-sm md:min-h-8 md:py-1 ${inputClasses(false)}`}
        onChange={(event) =>
          onVersionChange(event.target.value === "" ? null : Number(event.target.value))
        }
        value={version ?? ""}
      >
        <option value="">Current version</option>
        {versions.map((item) => (
          <option key={item.number} value={item.number}>
            Version {item.number}
            {item.named && item.summary !== null ? ` — ${item.summary}` : ""}
          </option>
        ))}
      </select>
      {toolbar === undefined ? (
        <NameVersion artifact={artifact} isClosed={isClosed} />
      ) : (
        <button
          aria-label={CONTROL_LABELS.name_version}
          className={`${toolbarButton} ${secondaryButtonDisabledText}`}
          disabled={isClosed || toolbar.isNamingVersion}
          onClick={toolbar.requestNamedVersion}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.name}
        </button>
      )}
      {toolbar === undefined ? null : (
        <ConnectionDot connection={toolbar.connection} pending={toolbar.pending} />
      )}
      <CopyRefButton route={documentRoute(artifact, version)} />
      {version !== undefined ? (
        <button
          className={toolbarButton}
          onClick={() => onShowDiffChange(!showDiff)}
          type="button"
          {...pageChrome}
        >
          {showDiff ? CONTROL_LABELS.show_version : CONTROL_LABELS.diff_vs_current}
        </button>
      ) : toolbar === undefined ? null : (
        <>
          <button
            // The name the old page's inventory recorded for this control, which says what the
            // link addresses; the word it draws is the page's own (`page-words.ts`).
            aria-label="Copy link to block"
            className={`${toolbarButton} ${secondaryButtonDisabledText}`}
            onClick={() => {
              void toolbar.copyBlockLink().then(
                (copied) => setCopyFeedback(copied ? "copied" : "failed"),
                () => setCopyFeedback("failed")
              );
            }}
            type="button"
            {...pageChrome}
          >
            {CONTROL_LABELS.copy_link}
          </button>
          {copyFeedback === undefined ? null : (
            <span
              className={`text-xs font-medium ${copyFeedback === "copied" ? successText : dangerText}`}
              role="status"
              {...pageChrome}
            >
              {copyFeedback === "copied" ? ANSWER_LINES.copied : ANSWER_LINES.copy_failed_hint}
            </span>
          )}
        </>
      )}
    </div>
  );
}

/**
 * Names the document's current text with no editor of it on the page: the write the editor's own
 * name dialog makes (`ProofDocument.tsx`), through the same dialog, and the issue read again so
 * the picker lists the version it made.
 */
function NameVersion({ artifact, isClosed }: { artifact: Artifact; isClosed: boolean }): ReactNode {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const name = useKeyedWrite({
    // `write` comes first: `onSuccess` reads its answer.
    write: (summary: string, bounds) => api.createArtifactVersion(artifact.id, { summary }, bounds),
    inline: true,
    label: () => `New version of ${artifact.name}`,
    onSuccess: () => {
      // Made either way, so the dialog closes and a second press names nothing twice.
      setOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["issue", artifact.issue_key] });
      void queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id] });
    },
  });
  return (
    <>
      <button
        aria-label={CONTROL_LABELS.name_version}
        className={`${toolbarButton} ${secondaryButtonDisabledText}`}
        disabled={isClosed || name.isPending}
        onClick={() => setOpen(true)}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.name}
      </button>
      <NameVersionDialog
        error={name.isError}
        onClose={() => {
          name.reset();
          setOpen(false);
        }}
        onSave={(summary) => name.mutate(summary)}
        open={open}
        saving={name.isPending}
      />
      {name.unanswered === undefined ? null : (
        <LandedWithoutAnswer what={`The version “${name.unanswered.variables}”`} />
      )}
    </>
  );
}
