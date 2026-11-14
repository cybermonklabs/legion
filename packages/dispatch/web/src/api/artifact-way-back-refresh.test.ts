import { expect, test } from "bun:test";

import { eventRefreshTargets } from "./event-refresh";
import type { DispatchEvent } from "./types";

/** A correction or a removal as the feed carries it, on an issue or, with `issueKey: null`, on a
 *  project's own document. The feed's event union is wider than these rows need. */
function wayBackEvent(
  type: "artifact.renamed" | "artifact.removed",
  issueKey: string | null = "CORE-1"
): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    artifact_id: "artifact-3",
    created_at: "2026-11-11T00:00:00Z",
    id: 12,
    issue_key: issueKey,
    notify: true,
    payload: {
      artifact: { id: "artifact-3", name: "Launch notes", slug: "launch-notes" },
      ...(type === "artifact.renamed" ? { previous_name: "Lauch notes" } : {}),
    },
    project: "CORE",
    seq: 4,
    type,
  } as unknown as DispatchEvent;
}

/** The keys `event` marks stale, as JSON, so a key is compared whole. */
function refreshedKeys(event: DispatchEvent): string[] {
  return eventRefreshTargets(event, "alice").map((target) => JSON.stringify(target.queryKey));
}

// An open document list and an open document page both move on their own: the list read, the
// document's own read and the summary a reference to it draws from are all marked stale, as they
// are for the creation these two events undo.
test("a correction and a removal refresh the issue's document list, the document and the summary", () => {
  for (const type of ["artifact.renamed", "artifact.removed"] as const) {
    const keys = refreshedKeys(wayBackEvent(type));
    expect(keys).toContain(JSON.stringify(["artifacts", "CORE-1"]));
    expect(keys).toContain(JSON.stringify(["artifact", "artifact-3"]));
    expect(keys).toContain(JSON.stringify(["issue-summary", "CORE-1"]));
    // Both move the `attached_to` edge the issue's backlink panel lists.
    expect(keys).toContain(JSON.stringify(["references", "to", "dispatch://CORE-1"]));
    // Neither writes a version, so what a version moves stands.
    expect(keys).not.toContain(JSON.stringify(["issue", "CORE-1", "coverage"]));
  }
});

// A project's document is renamed, never removed. The name a reference to it shows moves with
// the correction, so that summary read is marked stale beside the project's own list.
test("a project document's correction refreshes its list, its page and its reference summary", () => {
  const keys = refreshedKeys(wayBackEvent("artifact.renamed", null));
  expect(keys).toContain(JSON.stringify(["project", "CORE", "artifacts"]));
  expect(keys).toContain(JSON.stringify(["artifact", "artifact-3"]));
  expect(keys).toContain(JSON.stringify(["artifact-ref"]));
  expect(keys).toContain(JSON.stringify(["doc-summary", "CORE"]));
});
