import { type IssueFieldWrite, useIssueFieldWrite } from "./useIssueFieldWrite";

/**
 * The one way the SPA writes an issue's own repository (owner/name, or null to inherit its
 * parent's or the project's default). The pick's write shows it on the issue detail and every
 * list of issues until it lands; an own repository is also what the issue resolves to, while an
 * Inherit pick leaves the repository the issue showed before, marked as not its own, until
 * Dispatch's answer names the one it inherits (see `useIssueFieldWrite`).
 */
export function useIssueRepo(issueKey: string): IssueFieldWrite<string | null> {
  return useIssueFieldWrite(issueKey, "repo");
}
