import { type ReactNode, useId } from "react";

import { AgentListRefilling } from "../../components/QueryError";
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
import {
  REF_PICKER_PARTY_WORDS,
  REF_PICKER_WORDS,
  type RefPickerInsertion,
  type RefPickerRow,
} from "./ref-picker";
import type { RefPicker } from "./useRefPicker";

/**
 * What the agent and role rows' own source is, where those rows would be: the shared refilling
 * notice, the list still loading, or the listener unreachable. Every view that reads the agent
 * list says which holds rather than letting a missing row read as an absence, and the picker
 * lists those rows, so it says it too - in both the hosts that draw the picker's own chrome.
 */
export function RefPickerParties({ picker }: { picker: RefPicker }): ReactNode {
  if (picker.parties === "listed") return null;
  if (picker.parties === "refilling") {
    return <AgentListRefilling className={`px-3 py-2 text-sm ${textMutedOnSurface}`} />;
  }
  const danger = picker.parties === "unreachable";
  return (
    <p
      className={`px-3 py-2 text-sm ${danger ? dangerText : textMutedOnSurface}`}
      data-testid={danger ? "agent-list-unreachable" : "agent-list-loading"}
    >
      {REF_PICKER_PARTY_WORDS[danger ? "unreachable" : "loading"]}
    </p>
  );
}

/**
 * What the picker says below its rows, in both the hosts that draw its chrome: what the next
 * two letters will add while the reader has typed too little for the search (the instruction),
 * where the agent and role rows' own source stands (`RefPickerParties`), and the words of
 * whichever listing state holds where there are no rows. One component, so the shared field's
 * list and the live composer's popover cannot say it two ways.
 */
export function RefPickerNotes({ picker }: { picker: RefPicker }): ReactNode {
  return (
    <>
      {picker.instruction === undefined ? null : (
        <p
          className={`px-3 py-2 text-xs ${textMutedOnSurface}`}
          data-testid="ref-picker-instruction"
        >
          {picker.instruction}
        </p>
      )}
      <RefPickerParties picker={picker} />
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
    </>
  );
}

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
      <RefPickerNotes picker={picker} />
    </div>
  );
}
