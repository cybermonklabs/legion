import { type FocusEvent, type ReactNode, useId, useRef, useState } from "react";

import type { Project } from "../../api/types";
import {
  bgTransparent,
  borderDefault,
  card,
  focusWithinBorder,
  inputClasses,
  selectedCardBorder,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { issueStatuses, statusLabel } from "../project/board-model";
import { type SearchBarFilters, type SearchFilterName, searchFilterChips } from "./search-filters";

/** Every filter's one look (design v3, state 1, as Linear's filters are one pill): a rounded
 *  pill as tall as the box above it, naming its field and then its value. */
const pill = `inline-flex min-h-[38px] items-center gap-1.5 rounded-full border px-3 text-sm ${inputClasses(true)}`;

/** The days back from today that "Updated since" offers, each by the words its menu says. */
const SINCE_CHOICES: readonly { readonly days: number; readonly text: string }[] = [
  { days: 0, text: "Today" },
  { days: 7, text: "Last 7 days" },
  { days: 30, text: "Last 30 days" },
  { days: 90, text: "Last 90 days" },
];

/** The day `days` before today on the reader's calendar, as a date box writes a day
 *  (`2026-11-17`), which is what the filter holds. */
function dayBack(days: number): string {
  const day = new Date();
  day.setDate(day.getDate() - days);
  const month = String(day.getMonth() + 1).padStart(2, "0");
  return `${day.getFullYear()}-${month}-${String(day.getDate()).padStart(2, "0")}`;
}

/**
 * One value picked from a short list: a pill that names the filter and its value ("Status Any
 * ▾") and a list of plain buttons under it, with `extra` after them where the filter takes a
 * value of another kind too. The list is buttons rather than a `<select>` or a listbox because
 * the palette's own hits are this dialog's `option` rows, and a second set of options inside it
 * would join the rows the arrows walk and the rows a reader is told there are. The pill's name is
 * the filter's own (`label`), which its visible words start with.
 *
 * An item takes the press without taking focus (`onMouseDown`), so the list does not close
 * under the pointer before the click it was opened for.
 */
function FilterMenu({
  emptyText,
  extra,
  label,
  onPick,
  options,
  value,
}: {
  emptyText: string;
  /** A control of another kind after the list, given the handler that closes the list once the
   *  focus leaves the menu, which every control of the menu carries. */
  extra?: (closeOnLeave: (event: FocusEvent<HTMLElement>) => void) => ReactNode;
  label: string;
  onPick: (value: string) => void;
  options: readonly { text: string; value: string }[];
  /** What the filter is set to now, in words: "Any" for a filter not set. */
  value: string;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const listId = useId();
  const wrapper = useRef<HTMLDivElement>(null);
  // Focus leaving the pill and the list for anything outside them closes the list, as a press
  // outside it would. It rides the controls rather than the box around them: a box is no control,
  // and a reader who cannot see it is told of no interaction it does not have.
  const closeOnLeave = (event: FocusEvent<HTMLElement>) => {
    if (wrapper.current?.contains(event.relatedTarget) !== true) {
      setOpen(false);
    }
  };

  return (
    <div className="relative" ref={wrapper}>
      <button
        aria-controls={listId}
        aria-expanded={open}
        aria-label={label}
        className={`${pill} ${open ? selectedCardBorder : ""}`}
        onBlur={closeOnLeave}
        onClick={() => setOpen(!open)}
        type="button"
      >
        <span className={textPrimaryOnSurface}>{label}</span>
        <span className={textMutedOnSurface}>{value}</span>
        <span aria-hidden="true" className={textMutedOnSurface}>
          ▾
        </span>
      </button>
      {open ? (
        <ul
          aria-label={label}
          className={`absolute z-10 mt-1 max-h-64 w-52 overflow-y-auto rounded-lg border py-1 shadow-lg ${card} ${borderDefault}`}
          id={listId}
        >
          {options.length === 0 ? (
            <li className={`px-3 py-2 text-sm ${textMutedOnSurface}`}>{emptyText}</li>
          ) : (
            options.map(({ text, value: picked }) => (
              <li key={picked}>
                <button
                  className={`block min-h-11 w-full px-3 py-2 text-left text-sm ${textPrimaryOnSurface} ${surfaceMutedHoverBg}`}
                  onBlur={closeOnLeave}
                  onClick={() => {
                    setOpen(false);
                    onPick(picked);
                  }}
                  onMouseDown={(event) => event.preventDefault()}
                  type="button"
                >
                  {text}
                </button>
              </li>
            ))
          )}
          {extra === undefined ? null : <li className="px-3 py-2">{extra(closeOnLeave)}</li>}
        </ul>
      ) : null}
    </div>
  );
}

/**
 * The four narrowings the bar offers, each one pill as tall as the box, naming its field and
 * its value: "Project Any ▾", "Status Any ▾", "Author Anyone", whose author is typed into the pill,
 * and "Updated since Any time ▾", whose menu offers Today, Last 7 days, Last 30 days and Last 90
 * days, and a day of the person's own. Once chosen, a narrowing's pill reads back what was chosen
 * in those words (`Project INFRA`) with ×, and pressing it drops that narrowing alone and brings
 * its menu back.
 *
 * The author is typed rather than picked: an item's author is a person's login or a session id,
 * and no list holds both. It is taken when the person presses Enter or leaves the box, so a
 * half-typed login never searches.
 */
export function SearchFilterBar({
  filters,
  onFilters,
  projects,
}: {
  filters: SearchBarFilters;
  onFilters: (filters: SearchBarFilters) => void;
  projects: readonly Project[];
}): ReactNode {
  const [author, setAuthor] = useState("");
  const set = (patch: SearchBarFilters) => onFilters({ ...filters, ...patch });
  const takeAuthor = () => {
    const typed = author.trim();
    if (typed !== "") {
      setAuthor("");
      set({ author: typed });
    }
  };
  const removals: Record<SearchFilterName, SearchBarFilters> = {
    author: { author: undefined },
    project: { project: undefined },
    status: { status: undefined },
    updatedSince: { updatedSince: undefined },
  };

  return (
    // A plain row, not a `fieldset`: every group of this dialog is one owner's hits, and a
    // reader told there is one more would look for hits in it.
    <div
      className="flex shrink-0 flex-wrap items-center gap-2 px-3 py-2"
      data-testid="search-filters"
    >
      {searchFilterChips(filters).map(({ label, name }) => (
        <button
          aria-label={`Remove ${label}`}
          className={`${pill} ${selectedCardBorder} ${textPrimaryOnSurface}`}
          key={name}
          onClick={() => set(removals[name])}
          title={label}
          type="button"
        >
          {label}
          <span aria-hidden="true" className={textMutedOnSurface}>
            ×
          </span>
        </button>
      ))}
      {filters.project === undefined ? (
        <FilterMenu
          emptyText="No projects yet"
          label="Project"
          onPick={(project) => set({ project })}
          options={projects.map((project) => ({ text: project.key, value: project.key }))}
          value="Any"
        />
      ) : null}
      {filters.status === undefined ? (
        <FilterMenu
          emptyText="No statuses"
          label="Status"
          onPick={(status) => set({ status })}
          options={issueStatuses.map((status) => ({ text: statusLabel(status), value: status }))}
          value="Any"
        />
      ) : null}
      {filters.author === undefined ? (
        // The pill names its field in the dark words the other pills use and takes the author
        // after it, its "Anyone" in the grey of their values (review M6), as wide as its words
        // and ending in the caret theirs end in (review a6), so the four read alike.
        <label className={`${pill} cursor-text ${focusWithinBorder}`}>
          <span className={textPrimaryOnSurface}>Author</span>
          <input
            aria-label="Author"
            className={`field-sizing-content min-w-[6ch] outline-none ${bgTransparent}`}
            onBlur={takeAuthor}
            onChange={(event) => setAuthor(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                event.preventDefault();
                takeAuthor();
              }
            }}
            placeholder="Anyone"
            type="search"
            value={author}
          />
          <span aria-hidden="true" className={textMutedOnSurface}>
            ▾
          </span>
        </label>
      ) : null}
      {filters.updatedSince === undefined ? (
        <FilterMenu
          emptyText=""
          extra={(closeOnLeave) => (
            <label className={`flex items-center gap-2 text-sm ${textPrimaryOnSurface}`}>
              On a day
              <input
                className={`min-w-0 flex-1 rounded border px-2 py-1 text-sm ${inputClasses(true)}`}
                onBlur={closeOnLeave}
                onChange={(event) => {
                  if (event.target.value !== "") {
                    set({ updatedSince: event.target.value });
                  }
                }}
                type="date"
                value=""
              />
            </label>
          )}
          label="Updated since"
          onPick={(day) => set({ updatedSince: day })}
          options={SINCE_CHOICES.map(({ days, text }) => ({ text, value: dayBack(days) }))}
          value="Any time"
        />
      ) : null}
    </div>
  );
}
