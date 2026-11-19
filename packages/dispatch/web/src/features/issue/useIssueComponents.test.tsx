import { expect, spyOn, test } from "bun:test";
import { hashKey, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { answerIssuePatches } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { issueQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { ArchitectureTree, Issue, IssueComponents, IssueDetails } from "../../api/types";
import { issue } from "../margin/margin-fixture";
import { useIssueComponents } from "./useIssueComponents";

/** The project's tree before the save: `issue` is in no component and not yet marked. */
const unassignedTree: ArchitectureTree = {
  components: [],
  not_architectural: [],
  retired_links: [],
  sources: [],
  totals: {
    components_without_work: 0,
    issues_done: 0,
    issues_total: 1,
    not_architectural: 0,
    retired_links: 0,
    unassigned: 1,
  },
  unassigned: [{ key: issue.key, status: issue.status, title: issue.title }],
};

/** What the save marks `issue`: not architectural, with its reason. */
const notArchitectural: IssueComponents = {
  ids: [],
  inherited_from: null,
  mode: "none",
  reason: "Hiring",
  unknown: [],
};

/** The issue's read and its project's tree, each read as the issue page and the Architecture pane
 *  read them, which is what they show, and the components write they save through. */
function useShownComponents() {
  const details = useQuery(issueQuery(issue.key));
  const tree = useQuery({
    queryKey: ["architecture", issue.project],
    queryFn: () => api.getArchitecture(issue.project),
  });
  return {
    components: details.data?.components,
    notArchitectural: tree.data?.not_architectural?.map((row) => row.key),
    tree: tree.data,
    write: useIssueComponents(issue),
  };
}

// A refresh of the issue or of its project's tree asked while a components save was out could
// reach Dispatch before the save did, and its answer moved the issue back where it was under the
// reader's pick. Each read waits for the save's answer and is asked once it is in, so the page
// shows what Dispatch holds after the save.
test("a read of the issue or its tree asked while a components save is out waits for its answer, and the page shows what it reads then", async () => {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const detailKey = issueQuery(issue.key).queryKey;
  const treeKey = ["architecture", issue.project];
  // Each read of the issue and of the tree, listed as it calls its function: a read the save
  // holds is listed before it waits, and one that is not held has asked by then.
  const asked: string[] = [];
  const stopListening = client.onAttempt((queryHash) => {
    if (queryHash === hashKey(detailKey) || queryHash === hashKey(treeKey)) asked.push(queryHash);
  });
  const issueReads: ReturnType<typeof Promise.withResolvers<IssueDetails>>[] = [];
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => {
    const read = Promise.withResolvers<IssueDetails>();
    issueReads.push(read);
    return read.promise;
  });
  const treeReads: ReturnType<typeof Promise.withResolvers<ArchitectureTree>>[] = [];
  const getArchitecture = spyOn(api, "getArchitecture").mockImplementation(() => {
    const read = Promise.withResolvers<ArchitectureTree>();
    treeReads.push(read);
    return read.promise;
  });
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const hook = renderHook(useShownComponents, {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  // What Dispatch holds once it took the save, a minute on.
  const savedDetails: IssueDetails = {
    ...issue,
    components: notArchitectural,
    updated_at: "2026-09-09T00:01:00Z",
  };
  const savedTree: ArchitectureTree = {
    ...unassignedTree,
    not_architectural: [
      {
        inherited_from: null,
        key: issue.key,
        reason: "Hiring",
        status: issue.status,
        title: issue.title,
      },
    ],
    totals: { ...unassignedTree.totals, not_architectural: 1, unassigned: 0 },
    unassigned: [],
  };
  try {
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(getArchitecture).toHaveBeenCalledTimes(1));
    await act(async () => {
      issueReads[0]?.resolve(issue);
      treeReads[0]?.resolve(unassignedTree);
    });
    await waitFor(() => expect(hook.result.current.notArchitectural).toEqual([]));
    expect(hook.result.current.components?.mode).toBe("inherit");

    act(() => {
      void hook.result.current.write.submit({ mode: "none", reason: "Hiring" });
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(hook.result.current.components).toEqual(notArchitectural);
    expect(hook.result.current.notArchitectural).toEqual([issue.key]);

    await act(async () => {
      void client.invalidateQueries({ queryKey: detailKey });
      void client.invalidateQueries({ queryKey: treeKey });
    });
    // Both refreshes are asked, and wait: each is read once, before the save.
    await waitFor(() => expect(asked).toHaveLength(4));
    expect(getIssue).toHaveBeenCalledTimes(1);
    expect(getArchitecture).toHaveBeenCalledTimes(1);
    expect(hook.result.current.components).toEqual(notArchitectural);
    expect(hook.result.current.notArchitectural).toEqual([issue.key]);

    await act(async () => {
      const { artifacts: _a, children: _c, open_asks: _o, ...answer } = savedDetails;
      save.resolve(answer);
    });
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(getArchitecture).toHaveBeenCalledTimes(2));
    getIssue.mockResolvedValue(savedDetails);
    getArchitecture.mockResolvedValue(savedTree);
    await act(async () => {
      issueReads[1]?.resolve(savedDetails);
      treeReads[1]?.resolve(savedTree);
    });
    await waitFor(() => expect(hook.result.current.tree).toEqual(savedTree));
    await waitFor(() => expect(client.isMutating()).toBe(0));
    expect(client.getQueryData<IssueDetails>(detailKey)).toEqual(savedDetails);
    expect(hook.result.current.components).toEqual(notArchitectural);
  } finally {
    hook.unmount();
    stopListening();
    getArchitecture.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  }
});
