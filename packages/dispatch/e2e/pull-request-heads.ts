/** Test fixtures for pull request heads; part N's row "every colour of the page and of Details
 *  is a page colour" reads them too, so they live here rather than in a browser file. */

import { sql } from "./psql";

/** The repository the pull requests of `c-pull-request-link` are written in. */
export const PULL_REQUEST_REPOSITORY = "acme/widgets";

/** One `pull_request_heads` row attributed to `issue`, written as the `pull_request` envelope
 *  would have stored it (`e2e/verification.e2e.ts`'s `seedCICheck`), with the address and the
 *  time of the event it reported set by the row. */
export async function writePullRequestHead(row: {
  readonly event: string;
  readonly issue: string;
  readonly number: number;
  readonly state: "closed" | "merged" | "open";
  readonly url: string;
}): Promise<void> {
  const sha = String(row.number).padStart(40, "0");
  await sql(
    `INSERT INTO pull_request_heads (repo, number, head_sha, state, issue_key, attribution_source, title, url, event_at, checked_at)
     VALUES ('${PULL_REQUEST_REPOSITORY}', ${row.number}, '${sha}', '${row.state}', '${row.issue}', 'title_body', '${row.issue} the header', '${row.url}', '${row.event}', now())`
  );
}
