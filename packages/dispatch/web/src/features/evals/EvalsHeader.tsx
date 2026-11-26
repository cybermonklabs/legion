import { type FormEvent, type ReactNode, useId, useState } from "react";

import type { EvalListResponse } from "../../api/types";
import {
  borderStrong,
  dangerText,
  secondaryButtonCompact,
  surfaceMutedBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import type { DeliveryFreshness } from "../delivery/lib/freshness";
import { type CompareChoice, customRange, lastUtcDate, utcDate } from "../delivery/lib/window";
import { SourceFreshness } from "../delivery/SourceFreshness";
import { WindowControl } from "../delivery/WindowControl";
import type { EvalsUrlState } from "./evals-url";

const fieldClass = `rounded border px-2 py-1 text-sm ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`;

/** A range of whole UTC days as an eval read answers it (`window`, `compare`). */
type EvalRange = EvalListResponse["window"];

/** A range of whole UTC days in words: its first and last day, and how many it holds. */
function rangeWords(range: EvalRange): string {
  const days = range.days === 1 ? "1 day" : `${range.days} days`;
  return `${utcDate(range.from)} to ${lastUtcDate(range.to)} (UTC), ${days}`;
}

/** The compare range: by default the days before the window, as many as it holds; nothing; or a
 *  custom range of whole UTC days, applied once both dates are set and it spans at most 84 days,
 *  the longest range Dispatch reads, as the window's own custom range is. */
function CompareControl({
  value,
  onChange,
}: {
  value: CompareChoice;
  onChange: (next: CompareChoice) => void;
}): ReactNode {
  const id = useId();
  const [custom, setCustom] = useState(value.kind === "custom");
  const [fromDate, setFromDate] = useState(value.kind === "custom" ? utcDate(value.from) : "");
  const [toDate, setToDate] = useState(value.kind === "custom" ? lastUtcDate(value.to) : "");
  const [refused, setRefused] = useState<string | null>(null);

  const apply = () => {
    const range = customRange(fromDate, toDate);
    if ("refused" in range) {
      setRefused(range.refused);
      return;
    }
    setRefused(null);
    onChange({ kind: "custom", ...range });
  };

  return (
    <div className={`flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
      <label htmlFor={`${id}-compare`}>Compare with</label>
      <select
        className={fieldClass}
        id={`${id}-compare`}
        onChange={(event) => {
          const next = event.target.value;
          if (next === "custom") {
            setCustom(true);
            return;
          }
          setCustom(false);
          setRefused(null);
          onChange(next === "off" ? { kind: "off" } : { kind: "previous" });
        }}
        value={custom ? "custom" : value.kind}
      >
        <option value="previous">The days before the window</option>
        <option value="off">Nothing</option>
        <option value="custom">Custom range…</option>
      </select>
      {custom ? (
        <>
          <label htmlFor={`${id}-from`}>Compare from</label>
          <input
            className={fieldClass}
            id={`${id}-from`}
            onChange={(event) => setFromDate(event.target.value)}
            type="date"
            value={fromDate}
          />
          <label htmlFor={`${id}-to`}>Compare to</label>
          <input
            className={fieldClass}
            id={`${id}-to`}
            onChange={(event) => setToDate(event.target.value)}
            type="date"
            value={toDate}
          />
          <span className="text-xs">(UTC)</span>
          <button className={secondaryButtonCompact} onClick={apply} type="button">
            Apply
          </button>
          {refused === null ? null : (
            <p className={`basis-full text-xs ${dangerText}`} role="alert">
              {refused}
            </p>
          )}
        </>
      ) : null}
    </div>
  );
}

/** The repositories every number folds in place of the project's own, each named `owner/name`
 *  and each removable; none, the project's own rows (INFRA-2916 D2). */
function RepositoryFilter({
  repos,
  onChange,
}: {
  repos: readonly string[];
  onChange: (next: readonly string[]) => void;
}): ReactNode {
  const id = useId();
  const [draft, setDraft] = useState("");
  const add = (event: FormEvent) => {
    event.preventDefault();
    const name = draft.trim();
    if (name === "") return;
    setDraft("");
    if (!repos.includes(name)) onChange([...repos, name]);
  };
  return (
    <div className={`flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
      <form className="flex items-center gap-2" onSubmit={add}>
        <label htmlFor={`${id}-repo`}>Repository</label>
        <input
          className={fieldClass}
          id={`${id}-repo`}
          onChange={(event) => setDraft(event.target.value)}
          placeholder="owner/name"
          type="text"
          value={draft}
        />
        <button className={secondaryButtonCompact} type="submit">
          Add
        </button>
      </form>
      {repos.length === 0 ? null : (
        <ul aria-label="Repositories" className="flex flex-wrap items-center gap-1">
          {repos.map((name) => (
            <li
              className={`flex items-center gap-1 rounded border px-2 ${borderStrong}`}
              key={name}
            >
              <span className={textPrimaryOnCanvas}>{name}</span>
              <button
                aria-label={`Remove ${name}`}
                className="px-1"
                onClick={() => onChange(repos.filter((kept) => kept !== name))}
                type="button"
              >
                ×
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * The tab's one header (dispatch://INFRA-2918/spec R2, R3; plan D2): the window, the compare
 * range, the repositories and the freshness line, each of the first three in the URL. Once an
 * answer has landed it names the days Dispatch counts, from the answer's own `window` and
 * `compare`, which are whole UTC days, so the days a reader sees are the days every number counts.
 * The freshness line is drawn only where Dispatch sent one (the delivery evals answer's
 * `delivery_freshness`); a tab whose answer carries none draws none.
 */
export function EvalsHeader({
  state,
  onChange,
  window,
  compare,
  freshness,
}: {
  state: EvalsUrlState;
  onChange: (next: EvalsUrlState) => void;
  /** The window of the first answer the tab's views gave; absent until one lands. */
  window?: EvalRange;
  /** That answer's compare range: null where it has none. */
  compare?: EvalRange | null;
  /** The project's delivery freshness and when it was read, absent where Dispatch sent none. */
  freshness?: { freshness: DeliveryFreshness; readAtMs: number };
}): ReactNode {
  return (
    <fieldset className="m-0 flex min-w-0 flex-col gap-2 border-0 p-0">
      <legend className="sr-only">Window and filters</legend>
      <div className="flex flex-wrap items-center gap-x-6 gap-y-2">
        <WindowControl
          key={
            state.window.kind === "custom"
              ? `${state.window.from}|${state.window.to}`
              : state.window.preset
          }
          // A brush is a span of the window it was drawn on, so a new window clears it.
          onChange={(next) => onChange({ ...state, window: next, brush: null })}
          value={state.window}
        />
        <CompareControl
          key={
            state.compare.kind === "custom"
              ? `${state.compare.from}|${state.compare.to}`
              : state.compare.kind
          }
          onChange={(next) => onChange({ ...state, compare: next })}
          value={state.compare}
        />
        <RepositoryFilter onChange={(repos) => onChange({ ...state, repos })} repos={state.repos} />
      </div>
      {window === undefined ? null : (
        <p className={`text-xs ${textMutedOnCanvas}`} data-testid="evals-days">
          {`Window ${rangeWords(window)}`}
          {compare === undefined
            ? null
            : compare === null
              ? "; compared with nothing"
              : `; compared with ${rangeWords(compare)}`}
        </p>
      )}
      {freshness === undefined ? null : (
        <SourceFreshness freshness={freshness.freshness} readAtMs={freshness.readAtMs} />
      )}
    </fieldset>
  );
}
