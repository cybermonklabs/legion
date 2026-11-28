import type { ReactNode } from "react";

import { bgTransparent, focusVisibleRing } from "../../theme/classes";
import { type IssueStatus, statusLabel } from "../project/board-model";
import { statusGlyph, statusTone } from "./issue-status";
import { StatusGlyph } from "./StatusGlyph";

/**
 * The issue page's status menu (the prototype's `.pill`): the status's shape and word inside a
 * rule of its colour, over a native select of `statuses`, each by its word. The issue page's
 * header draws it for the issue's status, and a command-bar plan row for the status its change
 * writes, so both show and pick a status the same way. A pick calls `onPick` with the status
 * picked; the menu draws `status` until its host draws another.
 *
 * The rule is drawn inside the pill's box, so the select can fill that box: 24 px tall at every
 * width, the least a target may be (WCAG 2.5.8). The shape is an svg and not text, since the
 * page's typeface lacks ◐; it hides itself from assistive technology and holds no words, so its
 * span only centres it on the pill while the select stretches.
 */
export function StatusMenu({
  disabled = false,
  label,
  onPick,
  status,
  statuses,
}: {
  readonly disabled?: boolean;
  /** The select's name: "Status" on the issue page, "Status of DEMO-10" on a plan row. */
  readonly label: string;
  readonly onPick: (status: string) => void;
  readonly status: string;
  readonly statuses: readonly IssueStatus[];
}): ReactNode {
  return (
    <label
      className={`inline-flex min-h-6 shrink-0 items-stretch gap-1 rounded-full px-2 text-xs leading-[18px] font-semibold shadow-[inset_0_0_0_1px_currentColor] ${statusTone(status)}`}
    >
      <span className="self-center">
        <StatusGlyph glyph={statusGlyph(status)} />
      </span>
      {/* Sized to the status it shows, as the prototype's pill is; a browser with no
          `field-sizing` draws the select at its longest option, which is its own width. */}
      <select
        aria-label={label}
        className={`min-w-0 cursor-pointer appearance-none font-inherit outline-none [field-sizing:content] disabled:cursor-not-allowed disabled:opacity-50 ${bgTransparent} ${focusVisibleRing}`}
        disabled={disabled}
        onChange={(event) => onPick(event.target.value)}
        value={status}
      >
        {statuses.map((choice) => (
          <option key={choice} value={choice}>
            {statusLabel(choice)}
          </option>
        ))}
      </select>
    </label>
  );
}
