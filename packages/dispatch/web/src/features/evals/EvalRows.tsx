import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { itemsViewQuery } from "../../api/queries";
import type { EvalItemRow } from "../../api/types";
import { LinkList } from "../../components/LinkList";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  linkText,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import type { EvalsScope } from "./useEvalsViews";

/** The row kinds Dispatch computes behind a number (the items route's `evalRow*` kinds); a row of
 *  any other kind is a reading as its writer logged it. */
const COMPUTED_ROW_KINDS: ReadonlySet<string> = new Set([
  "issue",
  "pull_request",
  "deploy",
  "run",
  "job",
  "day",
]);

/** A row's own number column `name`, null where it holds none. */
function numberOf(row: EvalItemRow, name: string): number | null {
  const value = row.cols?.[name];
  return typeof value === "number" ? value : null;
}

/** A row's own text column `name`, "" where it holds none. */
function textOf(row: EvalItemRow, name: string): string {
  const value = row.cols?.[name];
  return typeof value === "string" ? value : "";
}

/** A reading's value as logged: its series and column, its value in its unit or the reason it has
 *  none, and what the value is out of. */
function readingWords(row: EvalItemRow): string {
  const unit = textOf(row, "unit");
  const value = numberOf(row, "value");
  const reason = textOf(row, "value_absent_reason");
  const n = numberOf(row, "n");
  const nUnit = textOf(row, "n_unit");
  const where = [textOf(row, "series"), textOf(row, "col")].filter((part) => part !== "");
  const measured =
    value === null
      ? `not measured${reason === "" ? "" : `: ${reason}`}`
      : `${value}${unit === "" ? "" : ` ${unit}`}`;
  const of = n === null ? "" : ` of ${n}${nUnit === "" ? "" : ` ${nUnit}`}`;
  return `${where.length === 0 ? "" : `${where.join(" / ")}: `}${measured}${of}`;
}

/** A computed row's columns that read as words: each name and value that is a number, a word or
 *  a yes or no, in the order the row holds them. */
function columnWords(row: EvalItemRow): string {
  return Object.entries(row.cols ?? {})
    .filter(([, value]) => ["number", "string", "boolean"].includes(typeof value) && value !== "")
    .map(([name, value]) => `${name} ${String(value)}`)
    .join(" \u00b7 ");
}

/** One row behind a number: its own address (`href`), with its links beside it and never in its
 *  place, its kind and when it was measured; then, for a reading as logged, its value, its note
 *  and its source, and for a computed row its columns. */
function EvalRow({ row }: { row: EvalItemRow }): ReactNode {
  // A row of a kind Dispatch does not compute is a reading as its writer logged it.
  const reading = !COMPUTED_ROW_KINDS.has(row.row_kind);
  const links = row.links ?? [];
  const name = row.row_key === "" ? row.row_kind : row.row_key;
  const note = reading ? textOf(row, "note") : "";
  return (
    <li
      className={`flex min-w-0 flex-col gap-0.5 border-t pt-1 first:border-t-0 first:pt-0 ${borderDefault}`}
      data-eval-row={row.row_key}
      data-reading={reading ? row.row_kind : undefined}
    >
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs">
        {row.href === "" ? (
          <span className={`font-medium ${textPrimaryOnSurface}`}>{name}</span>
        ) : (
          <a
            className={`font-medium hover:underline ${linkText}`}
            data-row-href
            href={row.href}
            rel="noreferrer noopener"
            target="_blank"
          >
            {name}
          </a>
        )}
        <span className={textMutedOnSurface}>{row.row_kind}</span>
        <span className={textMutedOnSurface}>
          {`${new Date(row.measured_at).toISOString().slice(0, 16).replace("T", " ")} UTC`}
        </span>
        {links.length === 0 ? null : <LinkList label={`Links of ${name}`} links={links} />}
      </div>
      <p className={`text-xs ${textPrimaryOnSurface}`}>
        {reading ? readingWords(row) : columnWords(row)}
      </p>
      {note === "" ? null : <p className={`text-xs ${textMutedOnSurface}`}>{note}</p>}
      {reading && row.source !== "" ? (
        <p className={`text-[11px] ${textMutedOnSurface}`}>{`source ${row.source}`}</p>
      ) : null}
    </li>
  );
}

/**
 * The rows behind one eval's number over the header's window (dispatch://INFRA-2918/spec R10,
 * plan D9, D12), read through the eval's `items` view a page of `limit` at a time: each row with
 * its own address and its links beside it, and a reading as logged with the value, note and links
 * it carried. A page past the first is its own view, so moving to it subscribes it and leaves the
 * one before.
 */
export function EvalRows({
  label,
  limit,
  scope,
  slug,
}: {
  /** The list's accessible name. */
  label: string;
  /** How many rows a page holds, 1 to 200. */
  limit: number;
  scope: EvalsScope;
  slug: string;
}): ReactNode {
  const [offset, setOffset] = useState(0);
  const items = useQuery(
    itemsViewQuery({
      compare: null,
      limit,
      offset,
      project: scope.project,
      repos: scope.repos,
      slug,
      window: scope.window,
    })
  );
  if (items.isError) {
    return (
      <QueryError
        error={items.error}
        message="Could not read the rows."
        onRetry={() => void items.refetch()}
      />
    );
  }
  if (items.data === undefined) {
    return (
      <p className={`text-xs ${textMutedOnSurface}`} role="status">
        Reading the rows…
      </p>
    );
  }
  const rows = items.data.items ?? [];
  const total = items.data.total;
  if (total === 0) return <p className={`text-xs ${textMutedOnSurface}`}>No rows in the window.</p>;
  const last = Math.min(offset + rows.length, total);
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <ul aria-label={label} className="flex min-w-0 flex-col gap-1">
        {rows.map((row) => (
          // A reading's own id, else its kind and key, which name one computed row of a range.
          <EvalRow key={textOf(row, "id") || `${row.row_kind}\n${row.row_key}`} row={row} />
        ))}
      </ul>
      {total <= limit ? null : (
        <div className={`flex items-center gap-2 text-xs ${textMutedOnSurface}`}>
          <span>{`Rows ${offset + 1} to ${last} of ${total}`}</span>
          <button
            className={secondaryButtonCompact}
            disabled={offset === 0}
            onClick={() => setOffset(Math.max(0, offset - limit))}
            type="button"
          >
            Earlier rows
          </button>
          <button
            className={secondaryButtonCompact}
            disabled={offset + limit >= total}
            onClick={() => setOffset(offset + limit)}
            type="button"
          >
            Later rows
          </button>
        </div>
      )}
    </div>
  );
}
