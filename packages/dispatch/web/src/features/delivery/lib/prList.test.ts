import { expect, test } from "bun:test";

import type { DeliveryPR } from "../../../api/types";
import { buildPRListRows, compareIssueSort } from "./prList";

/** A delivery row as the server answers it: `priority` is already the resolved label. */
function pr(number: number, priority: DeliveryPR["priority"], priority_from: string | null = null) {
  return {
    additions: 1,
    author: "octocat",
    components: [],
    created_at: "2024-06-01T00:00:00Z",
    deletions: 1,
    deploy_run: null,
    deployed_at: null,
    deployed_status: "waiting",
    first_commit_at: null,
    id: `acme/widgets#${number}`,
    issue: `ACME-${number}`,
    issue_title: null,
    merged_at: "2024-06-01T01:00:00Z",
    number,
    parent_agent: null,
    partial: false,
    priority,
    priority_from,
    repo: "acme/widgets",
    rework: false,
    sessions: [],
    title: `Widget ${number}`,
    unfetchable_reason: null,
    url: `https://github.com/acme/widgets/pull/${number}`,
  } satisfies DeliveryPR;
}

test("the PR list sorts an inherited P0 with P0 and names its source", () => {
  const rows = buildPRListRows(
    [pr(3, "P2"), pr(2, "P0", "ACME-1"), pr(1, "P0")],
    ["acme/widgets"],
    []
  );
  const sorted = rows.sort((left, right) => {
    if (left.issueSort === undefined || right.issueSort === undefined) {
      throw new Error("PR list omitted a sort key for an attributed PR");
    }
    return compareIssueSort(left.issueSort, right.issueSort);
  });
  expect(
    sorted.map(({ pr }) => pr.issue),
    "PR list's Issue sort order"
  ).toEqual(["ACME-1", "ACME-2", "ACME-3"]);
  expect(sorted[1]?.issueLabel, "PR list's Issue cell for the inherited P0").toBe(
    "ACME-2 · P0 · from ACME-1"
  );
});
