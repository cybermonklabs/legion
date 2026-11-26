// The Evals tab's group links (dispatch://INFRA-2918/artifact/plan-md, D13; spec R8): a number
// that stands for a group of issues opens the project's issue table on exactly the issues it
// counts. Main's issue table takes no done view and no closed-day filter (its columns are
// `features/project/issues-table-model.ts` COLUMNS), so every group link names its issues by key,
// `only=<keys>&onlyLabel=<words>`, read from the eval's rows when the link is pressed: the link
// and the count are one set by construction.
import { type ReactNode, useState } from "react";
import { useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { EvalItemRow } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { linkText } from "../../theme/classes";
import { selectIssues } from "../project/issues-table-model";
import { buildProjectPath } from "../refs/routes";

/** What of an eval's rows a group link reads: the window (the items route's own where unnamed),
 *  the repositories, and the series whose cut it keeps. */
export interface IssueRowsQuery {
  readonly from?: string;
  readonly to?: string;
  readonly repo?: readonly string[];
  readonly series?: string;
}

/** Every issue key of `slug`'s issue rows (the items route, `row_kind=issue`) that `query` names,
 *  page by page, of the rows `keep` keeps: the issues a number counts. */
export async function readIssueKeys(
  project: string,
  slug: string,
  query: IssueRowsQuery,
  keep: (row: EvalItemRow) => boolean = () => true
): Promise<string[]> {
  const keys: string[] = [];
  for (let offset = 0; ; ) {
    const page = await api.listEvalItems(project, slug, {
      ...query,
      repo: query.repo === undefined ? undefined : [...query.repo],
      limit: 200,
      offset,
      row_kind: "issue",
    });
    const rows = page.items ?? [];
    keys.push(...rows.filter(keep).map((row) => row.row_key));
    offset += rows.length;
    if (rows.length === 0 || offset >= page.total) return keys;
  }
}

/** Where a group link stands: idle, reading the issues it opens, or refused that read. */
export type LinkState =
  | { readonly kind: "idle" }
  | { readonly kind: "resolving" }
  | { readonly kind: "failed"; readonly error: unknown };

/** A group link (the state table of the drill-down's controls and the P0 link): pressed, it reads
 *  the issues `read` names and opens the project's issue table on exactly those, `only=<keys>`,
 *  under `label`; while it reads it is resolving, and a refused read is kept for a retry. */
export function useIssuesLink(
  project: string,
  read: () => Promise<string[]>,
  label: string
): { state: LinkState; open: () => void } {
  const navigate = useNavigate();
  const [state, setState] = useState<LinkState>({ kind: "idle" });
  const open = () => {
    setState({ kind: "resolving" });
    read().then(
      (keys) => {
        const query = selectIssues(new URLSearchParams(), keys, label);
        navigate(`${buildProjectPath({ kind: "issues", project })}?${query.toString()}`);
        setState({ kind: "idle" });
      },
      (error: unknown) => setState({ error, kind: "failed" })
    );
  };
  return { open, state };
}

/** The words a group link reads: "Open the issue", "Open the 3 issues". */
export function openWords(count: number): string {
  return `Open the ${count === 1 ? "issue" : `${count} issues`}`;
}

/** One group link as a button naming its count (`data-group-count`), which reads its issues when
 *  pressed (`useIssuesLink`), says so while it reads, and shows a refused read with a retry. A
 *  count of 0 opens nothing and draws nothing. */
export function IssuesLink({
  count,
  label,
  project,
  read,
}: {
  count: number;
  /** What the issue table names the issues by. */
  label: string;
  project: string;
  read: () => Promise<string[]>;
}): ReactNode {
  const link = useIssuesLink(project, read, label);
  if (count === 0) return null;
  if (link.state.kind === "failed")
    return (
      <QueryError
        error={link.state.error}
        message={`Could not read the issues: ${label}.`}
        onRetry={link.open}
      />
    );
  return (
    <button
      aria-busy={link.state.kind === "resolving" ? true : undefined}
      className={`text-xs hover:underline disabled:opacity-50 ${linkText}`}
      data-group-count={count}
      disabled={link.state.kind === "resolving"}
      onClick={link.open}
      type="button"
    >
      {link.state.kind === "resolving" ? "Reading the issues\u2026" : openWords(count)}
    </button>
  );
}
