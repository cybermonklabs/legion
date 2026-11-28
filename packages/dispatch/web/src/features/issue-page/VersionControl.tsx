import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { Artifact } from "../../api/types";
import {
  dangerText,
  focusVisibleRing,
  pageField,
  pageHeaderControl,
  pageQuietText,
  successText,
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

/** The control's buttons: the page's small control, as the row's other controls are, refused
 *  alike whether `disabled` (a closed issue) or `aria-disabled` (a write of theirs still out). */
const toolbarButton = `refused:cursor-not-allowed refused:opacity-50 ${pageHeaderControl}`;

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
    <div className="flex min-w-0 flex-wrap items-center gap-2" data-testid="issue-version-control">
      {version === undefined ? null : (
        <span className={`text-xs ${pageQuietText}`} {...pageChrome}>
          {CONTROL_LABELS.version} <span {...authorText}>{version}</span>
        </span>
      )}
      {/* The control names itself rather than carrying a separate label element: every option
          says "version", so the closed select reads as a version picker. */}
      <select
        aria-label={CONTROL_LABELS.version}
        className={`min-h-6 min-w-0 max-w-48 truncate rounded-[7px] border px-2 text-xs leading-5 outline-none md:min-h-0 md:py-px ${pageField} ${focusVisibleRing}`}
        onChange={(event) =>
          onVersionChange(event.target.value === "" ? null : Number(event.target.value))
        }
        value={version ?? ""}
      >
        <option value="">Current version</option>
        {versions.map((item) => (
          <option key={item.number} value={item.number}>
            Version {item.number}
            {/* A version's summary, as its writer gave it: `issue-page-version-summary`. */}
            {item.named && item.summary !== null ? ` — ${item.summary}` : ""}
          </option>
        ))}
      </select>
      {toolbar === undefined ? (
        <NameVersion artifact={artifact} isClosed={isClosed} />
      ) : (
        <NameVersionButton
          isClosed={isClosed}
          naming={toolbar.isNamingVersion}
          onPress={toolbar.requestNamedVersion}
        />
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
            className={toolbarButton}
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
 * "Name version", which opens the name dialog. While a name it asked for is being saved it is
 * refused with `aria-disabled` and its press does nothing, never `disabled` (decisions 223 and
 * 245): the dialog's close gives the focus back to it once the versions' re-read is in, which a
 * disabled control could not take, and the focus would fall to the page's body.
 */
function NameVersionButton({
  isClosed,
  naming,
  onPress,
}: {
  isClosed: boolean;
  naming: boolean;
  onPress: () => void;
}): ReactNode {
  return (
    <button
      aria-busy={naming || undefined}
      aria-disabled={naming || undefined}
      className={toolbarButton}
      disabled={isClosed}
      onClick={() => {
        if (!naming) onPress();
      }}
      type="button"
      {...pageChrome}
    >
      {CONTROL_LABELS.name_version}
    </button>
  );
}

/**
 * Names the document's current text with no editor of it on the page: the write the editor's own
 * name dialog makes (`ProofDocument.tsx`), through the same dialog, and the issue read again so
 * the picker lists the version it made. The dialog stays open, its Save busy, until that read is
 * in, and closes then, so its close gives the focus back to Name version once (ST-6).
 */
function NameVersion({ artifact, isClosed }: { artifact: Artifact; isClosed: boolean }): ReactNode {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const name = useKeyedWrite({
    // `write` comes first: `onSuccess` reads its answer.
    write: (summary: string, bounds) => api.createArtifactVersion(artifact.id, { summary }, bounds),
    method: "POST",
    inline: true,
    label: () => `New version of ${artifact.name}`,
    onSuccess: async () => {
      // Made either way, so a second press names nothing twice. The write stays pending until
      // the issue's own read, whose documents' versions the picker lists, is in again: Save stays
      // busy until then, and the dialog closes only then. The issue's other reads and the
      // document's are asked again beside it; none of them asks that read again, which would
      // cancel the one this waits for.
      const reread = queryClient.invalidateQueries({
        exact: true,
        queryKey: ["issue", artifact.issue_key],
      });
      void queryClient.invalidateQueries({
        predicate: ({ queryKey }) => queryKey.length > 2,
        queryKey: ["issue", artifact.issue_key],
      });
      void queryClient.invalidateQueries({ queryKey: ["artifact", artifact.id] });
      await reread;
      setOpen(false);
    },
  });
  return (
    <>
      <NameVersionButton
        isClosed={isClosed}
        naming={name.isPending}
        onPress={() => setOpen(true)}
      />
      <NameVersionDialog
        error={name.error}
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
