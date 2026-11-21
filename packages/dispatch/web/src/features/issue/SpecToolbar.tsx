import { type ReactNode, useState } from "react";

import { heldPress } from "../../components/held-press";
import { MultiSelect } from "../../components/MultiSelect";
import {
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  successText,
} from "../../theme/classes";
import { ConnectionDot } from "../doc/ConnectionDot";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { CopyRefButton } from "../refs/CopyRefButton";
import type { DispatchReferenceRoute } from "../refs/routes";

/** The toolbar's button: a 44 px tap target through tablet widths, 32 px from `md`. */
const toolbarButton = `min-h-11 shrink-0 rounded-lg px-3 py-2 text-sm font-medium md:min-h-8 md:py-1 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`;

/** Compact primary-Spec controls displayed at the end of the active issue tab row. `reference`
 *  is the document as shown — the spec, or a historical version of it. */
export function SpecToolbar({
  isClosed,
  onShowDiffChange,
  onVersionChange,
  reference,
  showDiff,
  toolbar,
  version,
}: {
  isClosed: boolean;
  onShowDiffChange(next: boolean): void;
  onVersionChange(version: number | null): void;
  reference: DispatchReferenceRoute;
  showDiff: boolean;
  toolbar: DocumentToolbar;
  version: number | undefined;
}): ReactNode {
  const [copyFeedback, setCopyFeedback] = useState<"copied" | "failed" | undefined>(undefined);
  const [versionsOpen, setVersionsOpen] = useState(false);
  // The list's value for the version shown: `""` is the current document, as the route says.
  const shown = version === undefined ? "" : String(version);
  const versionLabels = new Map(
    toolbar.versions.map((item) => [
      String(item.number),
      `Version ${item.number}${item.named && item.summary !== null ? ` — ${item.summary}` : ""}`,
    ])
  );
  return (
    <div className="flex w-full min-w-0 flex-wrap items-center gap-2 md:ml-auto md:w-auto">
      {/* The control names itself rather than carrying a separate label element: every option
          says "version", so the closed list reads as a version picker and the strip still
          shares the tab row instead of taking a chrome row above the document. Its text box
          narrows the versions as the reader types. */}
      <MultiSelect
        emptyMessage="No version matches that."
        label="Version"
        mode="single"
        onOpenChange={setVersionsOpen}
        onPick={(value) => {
          // The version shown already is no move, as a `<select>` set to the value it holds
          // fires nothing. The list put the focus on its trigger before this pick, and the
          // strip the new route draws is the same one, so the focus is on the trigger at the
          // version picked.
          if (value !== shown) onVersionChange(value === "" ? null : Number(value));
        }}
        open={versionsOpen}
        optionLabel={(value) =>
          value === "" ? "Current version" : (versionLabels.get(value) ?? `Version ${value}`)
        }
        options={["", ...toolbar.versions.map((item) => String(item.number))]}
        searchLabel="Search versions"
        triggerClassName={`min-h-11 min-w-0 max-w-48 rounded border px-2 py-2 text-left text-sm md:min-h-8 md:py-1 ${inputClasses(false)}`}
        value={shown}
      />
      <button
        aria-label="Name version"
        className={`${toolbarButton} ${secondaryButtonDisabledText}`}
        disabled={isClosed}
        {...heldPress(toolbar.isNamingVersion, toolbar.requestNamedVersion)}
        type="button"
      >
        Name
      </button>
      <ConnectionDot connection={toolbar.connection} pending={toolbar.pending} />
      <CopyRefButton route={reference} />
      {version === undefined ? (
        <>
          <button
            aria-label="Copy link to block"
            className={`${toolbarButton} ${secondaryButtonDisabledText}`}
            onClick={() => {
              void toolbar.copyBlockLink().then(
                (copied) => setCopyFeedback(copied ? "copied" : "failed"),
                () => setCopyFeedback("failed")
              );
            }}
            type="button"
          >
            Copy link
          </button>
          {copyFeedback === undefined ? null : (
            <span
              className={`text-xs font-medium ${copyFeedback === "copied" ? successText : dangerText}`}
              role="status"
            >
              {copyFeedback === "copied" ? "Copied" : "Copy failed - select the text"}
            </span>
          )}
        </>
      ) : (
        <button className={toolbarButton} onClick={() => onShowDiffChange(!showDiff)} type="button">
          {showDiff ? "Show version" : "Diff vs current"}
        </button>
      )}
    </div>
  );
}
