import { type ReactNode, useId } from "react";

import { AgentListRefilling } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import {
  selectedCardBg,
  surfaceMutedHoverBg,
  surfaceRecessedBg,
  textOptionDescription,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { dangerText, textMutedOnSurface } from "../../theme/shell-classes";
import { AgentName } from "./AgentName";
import { RefKindIcon } from "./kind-icons";
import { PlainText } from "./plain-text";
import {
  REF_PICKER_PARTS_WORDS,
  REF_PICKER_PARTY_WORDS,
  REF_PICKER_TEXT_MAX,
  REF_PICKER_WORDS,
  type RefPickerAgent,
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
 * that the row the reader opened holds no parts, where the agent and role rows' own source
 * stands (`RefPickerParties`), and the words of whichever listing state holds where there are no
 * rows. One component, so the shared field's list and the live composer's popover cannot say it
 * two ways.
 */
export function RefPickerNotes({ picker }: { picker: RefPicker }): ReactNode {
  // While the agent list refills or could not be read, the picker cannot know which agent and
  // role rows match, so it never says that no row matches: the list's own line above stands in
  // its place, as the mention suggestions it replaced drew that line alone. Every door lists the
  // agent and role rows (`kindOrder`), so this holds behind each of them.
  const partiesUnknown = picker.parties === "refilling" || picker.parties === "unreachable";
  const state =
    picker.state === "rows" || (picker.state === "empty" && partiesUnknown)
      ? undefined
      : picker.state;
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
      {picker.partsNote === undefined ? null : (
        <p className={`px-3 py-2 text-sm ${textMutedOnSurface}`} data-testid="ref-picker-no-parts">
          {picker.partsNote}
        </p>
      )}
      <RefPickerParties picker={picker} />
      {state === undefined ? null : state === "unreachable" ? (
        <p className={`px-3 py-2 text-sm ${dangerText}`} data-testid="ref-picker-unreachable">
          {REF_PICKER_WORDS.unreachable}
        </p>
      ) : (
        <p
          className={`px-3 py-2 text-sm ${textMutedOnSurface}`}
          data-testid={`ref-picker-${state}`}
        >
          {REF_PICKER_WORDS[state]}
        </p>
      )}
    </>
  );
}

/**
 * The control on a row that holds parts of its own - a node's blocks, an agent's conversation's
 * documents - which lists them beside the row, in both the hosts that draw the picker's chrome:
 * one control, so the shared field's list and the live composer's popover open a row's parts
 * the same way and by the same words. Pressing it is not a pick of its row.
 */
export function RefPartsControl({
  picker,
  row,
}: {
  picker: RefPicker;
  row: RefPickerRow;
}): ReactNode {
  if (row.parts === undefined) return null;
  return (
    <button
      className={`shrink-0 rounded px-2 py-1 text-xs font-medium ${textOptionDescription}`}
      onClick={(event) => {
        event.stopPropagation();
        picker.openParts(row.id);
      }}
      type="button"
    >
      {REF_PICKER_PARTS_WORDS[row.parts.kind].control}
    </button>
  );
}

/**
 * The agent a party's row names (`RefPickerAgent`), drawn through `AgentName` with its running
 * state and no link, because a row is an option and a listbox owns only options. To assistive
 * technology it is one image named by the row's own words for the agent, whose state mark is no
 * part of it: that mark would otherwise put the state into the row's accessible name, before the
 * agent's name.
 */
function RowAgent({ agent, className }: { agent: RefPickerAgent; className?: string }): ReactNode {
  return (
    <span aria-label={agent.words} className={className} role="img">
      <AgentName kind="static" sessionId={agent.sessionId} storedTitle={agent.storedTitle} />
    </span>
  );
}

/**
 * A row's words, in both the hosts that draw the picker's rows. A session's row is its session,
 * drawn as an agent (`RowAgent`). A row worded with a record's own text - a hit's matched words,
 * the head of a block - draws that text's plain-text projection from the tree the server read it
 * into (`PlainText`), since a row is an option the reader takes, which holds no link: its
 * formatting gone and each reference read as its target's name, never the Markdown it is written
 * in. Until the reader's engine answers, and for a row the server sent no tree for, it draws
 * `label`, the words as written, which `headRefs` cut where no reference crosses the cut.
 */
export function RefRowLabel({ row }: { row: RefPickerRow }): ReactNode {
  const className = `truncate font-medium ${textPrimaryOnSurface}`;
  if (row.kind === "agent" && row.agent !== undefined) {
    return <RowAgent agent={row.agent} className={className} />;
  }
  if (row.prose === undefined) return <span className={className}>{row.label}</span>;
  return (
    <PlainText
      markdown={row.prose.markdown}
      max={REF_PICKER_TEXT_MAX}
      source={row.prose.source}
      surface="ref-picker-row-label"
      tree={row.prose.tree}
    >
      {(line) => <span className={className}>{line ?? row.label}</span>}
    </PlainText>
  );
}

/**
 * A row's detail line, in both the hosts that draw the picker's rows. A role's row starts its
 * line with the session that holds the role, which it draws as an agent (`RowAgent`).
 */
export function RefRowDetail({ row }: { row: RefPickerRow }): ReactNode {
  if (row.kind !== "role" || row.agent === undefined) return row.detail;
  return (
    <>
      <RowAgent agent={row.agent} />
      {row.detail.slice(row.agent.words.length)}
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
    // A row is a `div`, not a `button`: a row that holds parts carries the control that lists
    // them, and a button inside a button is no button at all. A session's row has the session's
    // full id as its title, which is its tooltip and, beside the words that name the row, its
    // accessible description: its name can be the short form `session:<8>…`, which two sessions
    // may share.
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
      title={one.kind === "agent" ? one.agent?.sessionId : undefined}
    >
      <RefKindIcon kind={one.kind} />
      <span className="flex min-w-0 flex-1 flex-col">
        <RefRowLabel row={one} />
        {one.detail === "" ? null : (
          // The stronger muted role, which clears AA over the sky fill a highlighted row wears
          // as well as over the list's own surface.
          <TruncatedText className={`text-xs ${textOptionDescription}`} title={one.detail}>
            <RefRowDetail row={one} />
          </TruncatedText>
        )}
      </span>
      <RefPartsControl picker={picker} row={one} />
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
            {/* not stored prose: the picker's own name for a group of rows */}
            {group.heading}
          </div>
          {group.rows.map(row)}
        </div>
      ))}
      <RefPickerNotes picker={picker} />
    </div>
  );
}
