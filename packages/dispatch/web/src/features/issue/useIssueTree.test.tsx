import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type {
  Artifact,
  ArtifactBlock,
  Comment,
  IssueDetails,
  ReferenceTarget,
} from "../../api/types";
import type { TreeNode } from "./issue-tree";
import { nodesInOrder } from "./tree-keys";
import { useIssueTree } from "./useIssueTree";

/**
 * The names of the issue page's tree, across a change to the tree itself. The batched name read
 * is keyed by the set of addresses the tree holds, so one comment, message, ask or block
 * arriving is a key nobody has an answer for: without the previous answer standing while the
 * new one loads, every block row drawn from those words loses its text and every chip falls
 * back to its raw address, on a page that was already drawn.
 */

const KEY = "CORE-1";
const PLAN = "plan-md";
const TASK_REF = `dispatch://${KEY}/artifact/${PLAN}#b-t1`;
const TASK_WORDS = "Draw the tree's names from one read.";

const plan: Artifact = {
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-plan",
  issue_key: KEY,
  kind: "doc",
  name: "Plan",
  primary: false,
  project: "CORE",
  ref_key: "artifact:artifact-plan",
  session_id: null,
  slug: PLAN,
  versions: [],
};

const task: ArtifactBlock = {
  attributes: [],
  from: 0,
  id: "t1",
  references: { asks: 0, comments: 0 },
  to: 1,
  token: "t1",
  type: "plan_task",
};

const issue: IssueDetails = {
  artifacts: [plan],
  assignee: null,
  blocked_by: [],
  children: [],
  claim: null,
  closed_at: null,
  components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: "",
  external_links: [],
  holder_state: null,
  key: KEY,
  labels: [],
  last_seen: null,
  last_seq: 1,
  number: 1,
  open_asks: [],
  parent: null,
  plan_artifact_id: plan.id,
  primary_artifact_id: "",
  priority: null,
  progress: { children: null, tasks: null },
  project: "CORE",
  rank: "U",
  referenced_by_count: 0,
  repo: null,
  resolved_repo: null,
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  status: "todo",
  takeable_at: null,
  title: "The issue page draws one ordered body",
  updated_at: "2026-11-05T00:00:00Z",
  verification_artifact_id: "",
};

const comment: Comment = {
  anchor: null,
  author: { id: "alice", kind: "user" },
  body: "One more turn of the conversation.",
  created_at: "2026-11-05T01:00:00Z",
  id: "comment-1",
  issue_key: KEY,
  parent_id: null,
  resolved_at: null,
  resolved_by: null,
  updated_at: "2026-11-05T01:00:00Z",
} as unknown as Comment;

/** Every read the hook makes, answered empty but for the plan's one task block. */
function stubTreeReads(
  answerNames: (refs: readonly string[]) => Promise<{
    targets: ReferenceTarget[];
  }>
) {
  return [
    spyOn(api, "getIssueCoverage").mockResolvedValue({
      gaps: [],
      has_plan: true,
      issue: KEY,
      requirements: [],
      spec_artifact_id: "",
    }),
    spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    }),
    spyOn(api, "getIssueResults").mockResolvedValue({
      checks: [],
      issue: KEY,
      verification_artifact_id: "",
    }),
    spyOn(api, "getIssueReferences").mockResolvedValue({ members: [], truncated: false }),
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([task]),
    spyOn(api, "getReferenceTargets").mockImplementation(answerNames),
  ];
}

/** The task row's own words, which come from the batched name read and from nothing else. */
function taskWords(root: TreeNode): string | undefined {
  return nodesInOrder(root).find((node) => node.id === `${PLAN}#b-t1`)?.text;
}

test("the names already in hand stand while a tree that grew reads the new set", async () => {
  // Every read after the first is held open: that window, where the page is drawn and the
  // names of its new set of addresses are not in yet, is what this test is about.
  const held: (() => void)[] = [];
  let reads = 0;
  const stubs = stubTreeReads(async (refs) => {
    reads += 1;
    if (reads > 1) {
      const gate = Promise.withResolvers<void>();
      held.push(gate.resolve);
      await gate.promise;
    }
    return {
      targets: [...refs].map(
        (ref): ReferenceTarget => ({
          gone: false,
          kind: "block",
          name: ref === TASK_REF ? TASK_WORDS : `words of ${ref}`,
          ref,
        })
      ),
    };
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));

    // A comment arrives: the tree holds one address more, so the names read is a key nobody
    // has answered, and it is still out when the page draws again.
    await act(async () => {
      client.setQueryData(["comments", KEY], [comment]);
      await Promise.resolve();
    });
    await waitFor(() =>
      expect(nodesInOrder(view.result.current).some((node) => node.id === comment.id)).toBe(true)
    );
    expect(taskWords(view.result.current)).toBe(TASK_WORDS);
    // Nothing is left mounted when the held read lands, so its answer updates no state.
    view.unmount();
  } finally {
    for (const release of held) release();
    for (const stub of stubs) stub.mockRestore();
  }
});
