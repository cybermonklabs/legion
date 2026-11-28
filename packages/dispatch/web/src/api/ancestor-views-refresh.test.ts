import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { treeAnswer, treeNode } from "../features/issue-page/tree-read/tree-fixtures";
import { TREE_VIEWS, viewKey } from "../features/issue-page/tree-read/tree-read";
import { eventRefreshTargets } from "./event-refresh";
import { refreshQueries } from "./query-refresh";
import type { DispatchEvent } from "./types";

/**
 * A document event of an issue names the issue's ancestors (`ancestors`, parent first up to the
 * root), and reaches every view of each ancestor's tree read that the page holds
 * (`["issue-tree", <ancestor>, <view>]`), whichever view it is: the event's document can bring a
 * node of the issue into any of them, and a view's own answer, which holds only the nodes its
 * view takes, cannot say whether it will.
 */

/** A version of the grandchild CORE-3's Verification document, naming its parent CORE-2 and the
 *  root CORE-1, as the server writes it; the feed's union is wider than this case needs. */
const GRANDCHILD_VERSION = {
  actor: { id: "alice", kind: "user" },
  artifact_id: "artifact-3",
  created_at: "2026-11-23T00:00:00Z",
  id: 1,
  issue_key: "CORE-3",
  notify: true,
  payload: { ancestors: ["CORE-2", "CORE-1"], artifact_id: "artifact-3", number: 2 },
  project: "CORE",
  seq: 1,
  type: "artifact.version",
} as unknown as DispatchEvent;

// INV-2. For each view a read asks for (`TREE_VIEWS`): the root's view, held alone with no outline
// beside it and holding no node of the grandchild, is marked by the grandchild's version; the
// same view of an issue the event does not name is not.
test("a grandchild's document version that names its ancestors marks every view of the root the cache holds, and no view of an issue it does not name", () => {
  const seen: Record<string, unknown> = {};
  for (const view of TREE_VIEWS) {
    const client = new QueryClient();
    for (const issue of ["CORE-1", "CORE-9"]) {
      client.setQueryData(
        viewKey(issue, view),
        treeAnswer([treeNode(`r-${view}`, "requirement", issue, { in: "spec" })], [], {
          issue,
          view,
        })
      );
    }
    refreshQueries(client, eventRefreshTargets(GRANDCHILD_VERSION, "alice"));
    seen[view] = {
      other: client.getQueryState(viewKey("CORE-9", view))?.isInvalidated,
      root: client.getQueryState(viewKey("CORE-1", view))?.isInvalidated,
    };
  }
  expect(seen).toEqual(
    Object.fromEntries(TREE_VIEWS.map((view) => [view, { other: false, root: true }]))
  );
});
