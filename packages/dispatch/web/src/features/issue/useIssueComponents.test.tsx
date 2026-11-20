import { expect, spyOn, test } from "bun:test";
import { hashKey, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { settle } from "../../__tests__/fake-clock";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { issueCoverageQuery, issueQuery } from "../../api/queries";
import { DispatchQueryClient, hearLiveEvent } from "../../api/query-client";
import type {
  ArchitectureTree,
  DispatchEvent,
  Issue,
  IssueComponents,
  IssueCoverage,
  IssueDetails,
} from "../../api/types";
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

/** An `issue.updated` of `issue` the live feed delivers, written by `actor`, naming the fields it
 *  moved. */
function issueUpdated(actor: string, changed: readonly string[]): DispatchEvent {
  return {
    actor: { id: actor, kind: "user" },
    created_at: "2026-09-09T00:01:00Z",
    id: 1,
    issue_key: issue.key,
    notify: false,
    payload: { changed, key: issue.key },
    seq: 1,
    type: "issue.updated",
  } as DispatchEvent;
}

/** `useShownComponents` with the issue's title, as its header reads it. */
function useShownTitle() {
  return { ...useShownComponents(), title: useQuery(issueQuery(issue.key)).data?.title };
}

// Dispatch publishes the save's `issue.updated`, by the viewer, once it has taken the save, so a
// read of the issue or its tree asked after it answers the save. The hold ends on that event
// though the save's answer is still out, and a field somebody else moved meanwhile shows then
// rather than wait for an answer that may be late. That other person's own `issue.updated` is
// not the save's and leaves the hold.
test("a title somebody else writes while a components save is out shows once the save's own event has come", async () => {
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
  // Each read's answer, given as the row says.
  const issueAnswers: ((details: IssueDetails) => void)[] = [];
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => {
    const read = Promise.withResolvers<IssueDetails>();
    issueAnswers.push(read.resolve);
    return read.promise;
  });
  const treeAnswers: ((tree: ArchitectureTree) => void)[] = [];
  const getArchitecture = spyOn(api, "getArchitecture").mockImplementation(() => {
    const read = Promise.withResolvers<ArchitectureTree>();
    treeAnswers.push(read.resolve);
    return read.promise;
  });
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const hook = renderHook(useShownTitle, {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  // What Dispatch holds once it took the save: the issue not architectural, and the title bob
  // gave it meanwhile.
  const title = "Review the spec and the plan";
  const savedDetails: IssueDetails = {
    ...issue,
    components: notArchitectural,
    title,
    updated_at: "2026-09-09T00:01:00Z",
  };
  const savedTree: ArchitectureTree = {
    ...unassignedTree,
    not_architectural: [
      { inherited_from: null, key: issue.key, reason: "Hiring", status: issue.status, title },
    ],
    totals: { ...unassignedTree.totals, not_architectural: 1, unassigned: 0 },
    unassigned: [],
  };
  try {
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(getArchitecture).toHaveBeenCalledTimes(1));
    await act(async () => {
      issueAnswers[0]?.(issue);
      treeAnswers[0]?.(unassignedTree);
    });
    await waitFor(() => expect(hook.result.current.notArchitectural).toEqual([]));

    act(() => {
      void hook.result.current.write.submit({ mode: "none", reason: "Hiring" });
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(hook.result.current.components).toEqual(notArchitectural);
    expect(hook.result.current.notArchitectural).toEqual([issue.key]);

    await act(async () => {
      hearLiveEvent(client, issueUpdated("bob", ["title"]), "alice");
      void client.invalidateQueries({ queryKey: detailKey });
      void client.invalidateQueries({ queryKey: treeKey });
    });
    // bob's event is not the save's: both refreshes are asked, and wait.
    await waitFor(() => expect(asked).toHaveLength(4));
    expect(getIssue).toHaveBeenCalledTimes(1);
    expect(getArchitecture).toHaveBeenCalledTimes(1);
    expect(hook.result.current.title).toBe(issue.title);

    await act(async () => {
      hearLiveEvent(client, issueUpdated("alice", ["components"]), "alice");
      void client.invalidateQueries({ queryKey: detailKey });
      void client.invalidateQueries({ queryKey: treeKey });
    });
    // The save's answer is still out, and both reads ask Dispatch and show what it holds.
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(getArchitecture).toHaveBeenCalledTimes(2));
    getIssue.mockResolvedValue(savedDetails);
    getArchitecture.mockResolvedValue(savedTree);
    await act(async () => {
      issueAnswers[1]?.(savedDetails);
      treeAnswers[1]?.(savedTree);
    });
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.title).toBe(title);
    expect(hook.result.current.components).toEqual(notArchitectural);
    expect(hook.result.current.tree).toEqual(savedTree);
    expect(client.isMutating()).toBe(1);

    await act(async () => {
      const { artifacts: _a, children: _c, open_asks: _o, ...answer } = savedDetails;
      save.resolve(answer);
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await waitFor(() => expect(client.isFetching()).toBe(0));
  } finally {
    hook.unmount();
    stopListening();
    getArchitecture.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  }
});

/** What Dispatch holds once it took a save that marked `issue` not architectural for `reason`:
 *  the issue's read, the save's answer, and the project's tree. */
interface SavedAs {
  readonly answer: Issue;
  readonly details: IssueDetails;
  readonly tree: ArchitectureTree;
}

function savedAs(reason: string): SavedAs {
  const details: IssueDetails = {
    ...issue,
    components: { ...notArchitectural, reason },
    updated_at: "2026-09-09T00:01:00Z",
  };
  const { artifacts: _a, children: _c, open_asks: _o, ...answer } = details;
  return {
    answer,
    details,
    tree: {
      ...unassignedTree,
      not_architectural: [
        { inherited_from: null, key: issue.key, reason, status: issue.status, title: issue.title },
      ],
      totals: { ...unassignedTree.totals, not_architectural: 1, unassigned: 0 },
      unassigned: [],
    },
  };
}

/**
 * `useShownComponents` under Dispatch's own client signed in as alice, once it has read the issue
 * and its tree and the reader has pressed two saves of the issue's components, "Hiring" and then
 * "Hiring, for now": the first is out, and the second waits its turn behind it (`issue:<KEY>`).
 * Each read of the issue and the tree, and each save, is answered when the row says; `asked` lists
 * the reads of the two as each calls its function, so a read a save holds is listed before it
 * waits.
 */
async function twoSavesOut() {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const detailKey = issueQuery(issue.key).queryKey;
  const treeKey = ["architecture", issue.project];
  const asked: string[] = [];
  const stopListening = client.onAttempt((queryHash) => {
    if (queryHash === hashKey(detailKey) || queryHash === hashKey(treeKey)) asked.push(queryHash);
  });
  const issueAnswers: ((details: IssueDetails) => void)[] = [];
  const getIssue = spyOn(api, "getIssue").mockImplementation(() => {
    const read = Promise.withResolvers<IssueDetails>();
    issueAnswers.push(read.resolve);
    return read.promise;
  });
  const treeAnswers: ((tree: ArchitectureTree) => void)[] = [];
  const getArchitecture = spyOn(api, "getArchitecture").mockImplementation(() => {
    const read = Promise.withResolvers<ArchitectureTree>();
    treeAnswers.push(read.resolve);
    return read.promise;
  });
  const saveAnswers: ((answer: Issue) => void)[] = [];
  const patchIssue = answerIssuePatches().mockImplementation(() => {
    const save = Promise.withResolvers<Issue>();
    saveAnswers.push(save.resolve);
    return save.promise;
  });
  const hook = renderHook(useShownComponents, {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  const stop = () => {
    hook.unmount();
    stopListening();
    getArchitecture.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  };
  try {
    await waitFor(() => expect(getIssue).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(getArchitecture).toHaveBeenCalledTimes(1));
    await act(async () => {
      issueAnswers[0]?.(issue);
      treeAnswers[0]?.(unassignedTree);
    });
    await waitFor(() => expect(hook.result.current.notArchitectural).toEqual([]));

    act(() => {
      void hook.result.current.write.submit({ mode: "none", reason: "Hiring" });
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    act(() => {
      void hook.result.current.write.submit({ mode: "none", reason: "Hiring, for now" });
    });
    await waitFor(() => expect(hook.result.current.components?.reason).toBe("Hiring, for now"));
    expect(patchIssue).toHaveBeenCalledTimes(1);
  } catch (error) {
    stop();
    throw error;
  }
  return {
    asked,
    client,
    getArchitecture,
    getIssue,
    hook,
    issueAnswers,
    patchIssue,
    /** Asks the issue and its tree again, as an event's refresh does. */
    readAgain: () => {
      void client.invalidateQueries({ queryKey: detailKey });
      void client.invalidateQueries({ queryKey: treeKey });
    },
    saveAnswers,
    stop,
    treeAnswers,
  };
}

// Two components saves of one issue go out in press order (`issue:<KEY>`), and each publishes an
// `issue.updated` by the viewer naming `components`. One such event is the first save's, and says
// nothing of the second, which Dispatch has not taken yet: it ends one hold, the older, and a read
// of the issue or its tree asked then waits for the second save's event.
test("one components event ends the hold of the first of two saves out, and a read of the issue or its tree waits for the second's", async () => {
  const saves = await twoSavesOut();
  const later = savedAs("Hiring, for now");
  try {
    // Dispatch takes the first save, whose event comes; the second is not sent yet.
    await act(async () => {
      hearLiveEvent(saves.client, issueUpdated("alice", ["components"]), "alice");
      saves.readAgain();
      await settle();
    });
    await waitFor(() => expect(saves.asked).toHaveLength(4));
    expect(saves.getIssue).toHaveBeenCalledTimes(1);
    expect(saves.getArchitecture).toHaveBeenCalledTimes(1);
    expect(saves.hook.result.current.components?.reason).toBe("Hiring, for now");

    // The first save's answer comes, and the second goes out: the reads still wait.
    await act(async () => {
      saves.saveAnswers[0]?.(savedAs("Hiring").answer);
      await settle();
    });
    await waitFor(() => expect(saves.patchIssue).toHaveBeenCalledTimes(2));
    expect(saves.getIssue).toHaveBeenCalledTimes(1);
    expect(saves.getArchitecture).toHaveBeenCalledTimes(1);

    // The second save's event comes, and both reads ask Dispatch and show what it holds.
    await act(async () => {
      hearLiveEvent(saves.client, issueUpdated("alice", ["components"]), "alice");
    });
    await waitFor(() => expect(saves.getIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(saves.getArchitecture).toHaveBeenCalledTimes(2));
    saves.getIssue.mockResolvedValue(later.details);
    saves.getArchitecture.mockResolvedValue(later.tree);
    await act(async () => {
      saves.issueAnswers[1]?.(later.details);
      saves.treeAnswers[1]?.(later.tree);
      saves.saveAnswers[1]?.(later.answer);
    });
    await waitFor(() => expect(saves.client.isMutating()).toBe(0));
    await waitFor(() => expect(saves.client.isFetching()).toBe(0));
    expect(saves.hook.result.current.components).toEqual(later.details.components);
    expect(saves.hook.result.current.tree).toEqual(later.tree);
  } finally {
    saves.stop();
  }
});

// Dispatch can answer a save before its event reaches the page. The first save's reads go at its
// answer, and the second save goes out; the first save's event, which comes after, is still the
// first save's, and leaves the second's hold, which the second's own event ends.
test("a components save's event that comes after its answer leaves the hold of the save pressed after it", async () => {
  const saves = await twoSavesOut();
  const later = savedAs("Hiring, for now");
  try {
    // The first save's answer comes before its event, and the second goes out. The reads the
    // first one's settling asks wait for the second.
    await act(async () => {
      saves.saveAnswers[0]?.(savedAs("Hiring").answer);
      await settle();
    });
    await waitFor(() => expect(saves.patchIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(saves.asked).toHaveLength(4));
    expect(saves.getIssue).toHaveBeenCalledTimes(1);
    expect(saves.getArchitecture).toHaveBeenCalledTimes(1);

    // The first save's event comes now, and the reads still wait.
    await act(async () => {
      hearLiveEvent(saves.client, issueUpdated("alice", ["components"]), "alice");
      await settle();
    });
    expect(saves.getIssue).toHaveBeenCalledTimes(1);
    expect(saves.getArchitecture).toHaveBeenCalledTimes(1);
    expect(saves.hook.result.current.components?.reason).toBe("Hiring, for now");

    // The second save's event comes, and both reads ask Dispatch and show what it holds.
    await act(async () => {
      hearLiveEvent(saves.client, issueUpdated("alice", ["components"]), "alice");
    });
    await waitFor(() => expect(saves.getIssue).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(saves.getArchitecture).toHaveBeenCalledTimes(2));
    saves.getIssue.mockResolvedValue(later.details);
    saves.getArchitecture.mockResolvedValue(later.tree);
    await act(async () => {
      saves.issueAnswers[1]?.(later.details);
      saves.treeAnswers[1]?.(later.tree);
      saves.saveAnswers[1]?.(later.answer);
    });
    await waitFor(() => expect(saves.client.isMutating()).toBe(0));
    await waitFor(() => expect(saves.client.isFetching()).toBe(0));
    expect(saves.hook.result.current.components).toEqual(later.details.components);
    expect(saves.hook.result.current.tree).toEqual(later.tree);
  } finally {
    saves.stop();
  }
});

/** What the Spec tab draws beside each requirement, with `gaps` lacking. */
function coverage(gaps: string[]): IssueCoverage {
  return {
    gaps,
    has_plan: true,
    issue: issue.key,
    requirements: [],
    spec_artifact_id: "artifact-1",
  };
}

/** `useShownComponents` with the Spec tab's coverage, as the tab reads it. */
function useShownCoverage() {
  return { ...useShownComponents(), coverage: useQuery(issueCoverageQuery(issue.key)).data };
}

// The save holds the issue's own read and its project's tree, each by its exact key. The issue's
// other reads hang under its key (`issueCoverageQuery`) and are none of the save's: a refresh of
// the coverage asked while the save is out - a new version of the spec, say - asks at once.
test("a read of the issue's coverage asked while a components save is out goes at once", async () => {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const coverageKey = issueCoverageQuery(issue.key).queryKey;
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getArchitecture = spyOn(api, "getArchitecture").mockResolvedValue(unassignedTree);
  const coverageAnswers: ((read: IssueCoverage) => void)[] = [];
  const getIssueCoverage = spyOn(api, "getIssueCoverage").mockImplementation(() => {
    const read = Promise.withResolvers<IssueCoverage>();
    coverageAnswers.push(read.resolve);
    return read.promise;
  });
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const hook = renderHook(useShownCoverage, {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  try {
    await waitFor(() => expect(getIssueCoverage).toHaveBeenCalledTimes(1));
    await act(async () => {
      coverageAnswers[0]?.(coverage(["R1"]));
    });
    await waitFor(() => expect(hook.result.current.coverage).toEqual(coverage(["R1"])));
    await waitFor(() => expect(hook.result.current.notArchitectural).toEqual([]));

    act(() => {
      void hook.result.current.write.submit({ mode: "none", reason: "Hiring" });
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(hook.result.current.notArchitectural).toEqual([issue.key]);

    await act(async () => {
      void client.invalidateQueries({ exact: true, queryKey: coverageKey });
    });
    // The save is still out, and the coverage has asked Dispatch and shows its answer.
    await waitFor(() => expect(getIssueCoverage).toHaveBeenCalledTimes(2));
    await act(async () => {
      coverageAnswers[1]?.(coverage(["R1", "R2"]));
    });
    await waitFor(() => expect(hook.result.current.coverage).toEqual(coverage(["R1", "R2"])));
    expect(client.isMutating()).toBe(1);

    getIssueCoverage.mockResolvedValue(coverage(["R1", "R2"]));
    await act(async () => {
      const { artifacts: _a, children: _c, open_asks: _o, ...answer } = issue;
      save.resolve({ ...answer, components: notArchitectural });
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await waitFor(() => expect(client.isFetching()).toBe(0));
  } finally {
    hook.unmount();
    getIssueCoverage.mockRestore();
    getArchitecture.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  }
});

// The press cancels the reads of the two queries it writes, each by its exact key, as the hold
// names them. A read of the coverage already out at the press - a refresh a new version of the
// spec asked for, say - is none of the save's: it goes on, and its answer shows.
test("a read of the issue's coverage already out at a components press is not cancelled, and its answer shows", async () => {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const coverageKey = issueCoverageQuery(issue.key).queryKey;
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getArchitecture = spyOn(api, "getArchitecture").mockResolvedValue(unassignedTree);
  const coverageAnswers: ((read: IssueCoverage) => void)[] = [];
  const getIssueCoverage = spyOn(api, "getIssueCoverage").mockImplementation(() => {
    const read = Promise.withResolvers<IssueCoverage>();
    coverageAnswers.push(read.resolve);
    return read.promise;
  });
  const save = Promise.withResolvers<Issue>();
  const patchIssue = answerIssuePatches().mockImplementation(() => save.promise);
  const hook = renderHook(useShownCoverage, {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });
  try {
    await waitFor(() => expect(getIssueCoverage).toHaveBeenCalledTimes(1));
    await act(async () => {
      coverageAnswers[0]?.(coverage(["R1"]));
    });
    await waitFor(() => expect(hook.result.current.coverage).toEqual(coverage(["R1"])));
    await waitFor(() => expect(hook.result.current.notArchitectural).toEqual([]));

    await act(async () => {
      void client.invalidateQueries({ exact: true, queryKey: coverageKey });
    });
    await waitFor(() => expect(getIssueCoverage).toHaveBeenCalledTimes(2));

    act(() => {
      void hook.result.current.write.submit({ mode: "none", reason: "Hiring" });
    });
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    expect(hook.result.current.notArchitectural).toEqual([issue.key]);
    // The press is out, and the coverage read it found out is still out.
    expect(client.getQueryState(coverageKey)?.fetchStatus).toBe("fetching");

    await act(async () => {
      coverageAnswers[1]?.(coverage(["R1", "R2"]));
    });
    await waitFor(() => expect(hook.result.current.coverage).toEqual(coverage(["R1", "R2"])));
    expect(getIssueCoverage).toHaveBeenCalledTimes(2);
    expect(client.isMutating()).toBe(1);

    getIssueCoverage.mockResolvedValue(coverage(["R1", "R2"]));
    await act(async () => {
      const { artifacts: _a, children: _c, open_asks: _o, ...answer } = issue;
      save.resolve({ ...answer, components: notArchitectural });
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await waitFor(() => expect(client.isFetching()).toBe(0));
  } finally {
    hook.unmount();
    getIssueCoverage.mockRestore();
    getArchitecture.mockRestore();
    getIssue.mockRestore();
    patchIssue.mockRestore();
  }
});
