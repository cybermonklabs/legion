import { issueQuery, projectListsKey } from "../../api/queries";
import type { IssueDetails, IssueSummary } from "../../api/types";
import type { Shown } from "../writes/write-store";
import { type IssueFieldWrite, useIssueFieldWrite } from "./useIssueFieldWrite";

/**
 * The one way the SPA writes an issue's own repository (owner/name, or null to inherit its
 * parent's or the project's default). The pick shows in the issue detail and every project issue
 * list, and the store shows it again over each answer that lands on one of them while the write
 * is live (R7); an own repository is also what the issue resolves to, while an inherited one is
 * left to the server's answer, which `useIssueFieldWrite` merges.
 */
export function useIssueRepo(issueKey: string): IssueFieldWrite<string | null> {
  const detailKey = issueQuery(issueKey).queryKey;
  const show = (repo: string | null, shown: Shown) => {
    const fields = repo === null ? { repo } : { repo, resolved_repo: repo };
    shown.on<IssueDetails>(detailKey, (current) => ({ ...current, ...fields }));
    shown.under<IssueSummary[]>(projectListsKey, (current) =>
      current.map((issue) => (issue.key === issueKey ? { ...issue, ...fields } : issue))
    );
  };
  return useIssueFieldWrite<string | null>(issueKey, {
    label: (repo) => `${issueKey} on ${repo ?? "its project's repository"}`,
    name: "repo",
    patch: (repo) => ({ repo }),
    show,
  });
}
