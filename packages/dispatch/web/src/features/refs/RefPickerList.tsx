import { type ReactNode, useId } from "react";

import { TruncatedText } from "../../components/TruncatedText";
import {
  dangerText,
  selectedCardBg,
  surfaceMutedHoverBg,
  surfaceRecessedBg,
  textMutedOnSurface,
  textOptionDescription,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { RefKindIcon } from "./kind-icons";
import { REF_PICKER_WORDS, type RefPickerInsertion, type RefPickerRow } from "./ref-picker";
import type { RefPicker } from "./useRefPicker";

/**
 * The reference picker's rows, drawn for a host that has nowhere of its own to draw them: the
 * groups in the door's order, each under its kind's heading and wearing that kind's icon, with
 * the one instruction row and the words of whichever listing state holds. The host owns the
 * field and the insertion; this owns nothing but the list.
 */
export function RefPickerList({
  id,
  label = "References",
  onTake,
  picker,
}: {
  /** What the box that opened it points `aria-controls` at, so the two are one widget to a
   *  reader who cannot see them. */
  id?: string;
  /** The list's accessible name, which a host with several fields makes its own. */
  label?: string;
  /** Applies the pick to the host's own draft: the picker decides what to write and where, and
   *  the host writes it. */
  onTake: (insertion: RefPickerInsertion) => void;
  picker: RefPicker;
}): ReactNode {
  const headingPrefix = useId();
  if (!picker.open) return null;
  const take = (id: string) => {
    const insertion = picker.take(id);
    if (insertion !== undefined) onTake(insertion);
  };
  const row = (one: RefPickerRow) => (
    // A row is a `div`, not a `button`: a node's own row carries the control that lists its
    // blocks, and a button inside a button is no button at all.
    <div
      aria-selected={one.id === picker.activeId}
      className={`flex min-h-11 w-full items-center gap-2 rounded-lg px-3 py-2 text-left text-sm ${surfaceMutedHoverBg} ${
        one.id === picker.activeId ? selectedCardBg : ""
      }`}
      id={one.id}
      key={one.id}
      onClick={() => take(one.id)}
      onKeyDown={(event) => {
        if (event.key !== "Enter" && event.key !== " ") return;
        event.preventDefault();
        take(one.id);
      }}
      onMouseDown={(event) => event.preventDefault()}
      onMouseEnter={() => picker.highlight(one.id)}
      role="option"
      tabIndex={-1}
    >
      <RefKindIcon kind={one.kind} />
      <span className="flex min-w-0 flex-1 flex-col">
        <span className={`truncate font-medium ${textPrimaryOnSurface}`}>{one.label}</span>
        {one.detail === "" ? null : (
          // The stronger muted role, which clears AA over the sky fill a highlighted row wears
          // as well as over the list's own surface.
          <TruncatedText className={`text-xs ${textOptionDescription}`} title={one.detail}>
            {one.detail}
          </TruncatedText>
        )}
      </span>
      {one.parts === undefined ? null : (
        <button
          className={`shrink-0 rounded px-2 py-1 text-xs font-medium ${textOptionDescription}`}
          onClick={(event) => {
            event.stopPropagation();
            picker.openParts(one.id);
          }}
          type="button"
        >
          Blocks
        </button>
      )}
    </div>
  );
  return (
    <div
      aria-label={label}
      className={`max-h-52 overflow-y-auto rounded-lg border p-1 ${surfaceRecessedBg}`}
      data-testid="ref-picker"
      id={id}
      role="listbox"
    >
      {picker.groups.map((group) => (
        // biome-ignore lint/a11y/useSemanticElements: a listbox owns only options and groups of options; a fieldset is neither.
        <div aria-labelledby={`${headingPrefix}-${group.kind}`} key={group.kind} role="group">
          <div
            className={`px-3 pt-2 pb-1 text-xs font-semibold ${textMutedOnSurface}`}
            id={`${headingPrefix}-${group.kind}`}
            role="presentation"
          >
            {group.heading}
          </div>
          {group.rows.map(row)}
        </div>
      ))}
      {picker.instruction === undefined ? null : (
        <p
          className={`px-3 py-2 text-xs ${textMutedOnSurface}`}
          data-testid="ref-picker-instruction"
        >
          {picker.instruction}
        </p>
      )}
      {picker.state === "rows" ? null : picker.state === "unreachable" ? (
        <p className={`px-3 py-2 text-sm ${dangerText}`} data-testid="ref-picker-unreachable">
          {REF_PICKER_WORDS.unreachable}
        </p>
      ) : (
        <p
          className={`px-3 py-2 text-sm ${textMutedOnSurface}`}
          data-testid={`ref-picker-${picker.state}`}
        >
          {REF_PICKER_WORDS[picker.state]}
        </p>
      )}
    </div>
  );
}
