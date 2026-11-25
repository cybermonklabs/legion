import {
  afterEach,
  beforeEach,
  describe,
  expect,
  jest,
  type Mock,
  setSystemTime,
  spyOn,
  test,
} from "bun:test";
import { NO_ROUTE_ROLLING_RELEASE_MESSAGE } from "@legion/contracts";
import {
  focusManager,
  MutationObserver,
  type MutationObserverOptions,
  onlineManager,
  QueryClient,
  QueryClientProvider,
  QueryObserver,
  useQuery,
} from "@tanstack/react-query";
import { act, type RenderHookResult, render, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, type NavigateFunction, useLocation, useNavigate } from "react-router-dom";

import { realClock, tick } from "../../__tests__/fake-clock";
import { type FakeLiveSockets, installFakeLiveSockets } from "../../__tests__/live-socket-harness";
import { ApiError, api, type WriteBounds } from "../../api/client";
import { useEventFeed } from "../../api/event-feed";
import { eventRefreshTargets } from "../../api/event-refresh";
import { liveFeedUp, liveSocketFor } from "../../api/live-socket";
import {
  CONVERSATION_KINDS,
  conversationTreeKey,
  issueCoverageQuery,
  issueQuery,
  referenceTargetsQuery,
} from "../../api/queries";
import { DispatchQueryClient, queryRetry, queryRetryDelay } from "../../api/query-client";
import {
  MIN_REFETCH_INTERVAL_MS,
  type RefreshTarget,
  refreshQueries,
  trackQueryFetches,
} from "../../api/query-refresh";
import type {
  Artifact,
  AuthenticatedUser,
  Comment,
  DispatchEvent,
  DisplayTree,
  EventType,
  IssueDetails,
  ReferenceTarget,
} from "../../api/types";
import { heldSends } from "../conversation/held-sends";
import { MarginProvider } from "../margin/margin-context";
import type { PostDeletion } from "../refs/post-marks";
import { parseIssuePath } from "../refs/routes";
import { KeymapProvider } from "../shell/KeymapProvider";
import { plainWrite, writesOf } from "../writes/write-store";
import { issueAddress, presetPath } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import { CONTROL_LABELS, NODE_TYPE_NAMES, PRESET_NAMES } from "./page-words";
import { presetView } from "./presets";
import {
  ReferenceNamesProvider,
  useKnownAmbiguousReferences,
  useKnownDeletedReferences,
  useKnownEditedReferences,
  useKnownReferenceName,
  useKnownReferenceNames,
  usePublishReferenceNames,
} from "./reference-names";
import { BodySlot } from "./slots/body";
import { ProvidersSlot } from "./slots/providers";
import { nodeRowSelectorOf } from "./tree-focus";
import { nodesInOrder } from "./tree-keys";
import {
  commentNode,
  treeAnswer as fixtureAnswer,
  type TreeNodeExtra,
  treeNode,
  treeTuple,
} from "./tree-read/tree-fixtures";
import {
  dropViews,
  readNextTreePage,
  readOlderComments,
  subscribeTreeFreshness,
  type TreeAnswer,
  type TreeAnswerNode,
  type TreeRead as TreeReadState,
  type TreeTuple,
  useTreeRead,
  viewKey,
  viewQuery,
} from "./tree-read/tree-read";
import { ViewReadProvider } from "./tree-read/view-read";
import { useIssueTree } from "./useIssueTree";

/**
 * The issue page's tree as the hook reads it: from the tree read, with no document's blocks
 * read, the filters' held view reads on its root, and the names of what it holds read last and
 * all at once. The batched name read is keyed by the set of addresses the tree holds, so one
 * comment, message, ask or block arriving is a key nobody has
 * an answer for: without the previous answer standing while the new one loads, every chip falls
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
  role: "plan",
  session_id: null,
  slug: PLAN,
  versions: [],
};

/** One node of the tree read's answer, under the issue unless the case gives it a parent. */
function answerNode(
  id: string,
  supertag: string,
  { parent = KEY, ...extra }: TreeNodeExtra & { readonly parent?: string | null } = {}
): TreeAnswerNode {
  return treeNode(id, supertag, parent, extra);
}

/** The plan's one task, as the tree read answers it: a typed block of the plan, with its words. */
const task = answerNode("t1", "plan_task", {
  document: { artifact_id: plan.id, position: 0, slug: PLAN },
  text: TASK_WORDS,
});

/** The tree read's answer for the issue: the plan's task, each of `comments` under the issue, as
 *  the read places a thread a person wrote in, and whatever more nodes and tuples a case adds. */
function treeAnswer(
  comments: readonly Comment[],
  more: { readonly nodes?: readonly TreeAnswerNode[]; readonly tuples?: readonly TreeTuple[] } = {}
): TreeAnswer {
  return fixtureAnswer(
    [
      answerNode(KEY, "issue", { parent: null }),
      task,
      ...comments.map((one) => commentNode(one, KEY)),
      ...(more.nodes ?? []),
    ],
    more.tuples
  );
}

/** A view's answer: the plan's task, as the view's query took it or as the context it carries. */
function viewAnswer(...nodes: readonly TreeAnswerNode[]): TreeAnswer {
  return fixtureAnswer(nodes);
}

const issue: IssueDetails = {
  advances: [],
  advances_revision: 0,
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
  design_gates: [],
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
  answers: null,
  artifact_id: null,
  ask_id: null,
  author: { id: "alice", kind: "user" },
  body: "One more turn of the conversation.",
  created_at: "2026-11-05T01:00:00Z",
  deleted_at: null,
  deleted_by: null,
  deliveries: [],
  edited_at: null,
  id: "comment-1",
  issue_key: KEY,
  mentions: [],
  reply_block: null,
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
};

/** A read the tree is built from, by what it reads: each is one the batched name read waits on. */
type TreeRead =
  | "asks"
  | "comments"
  | "coverage"
  | "decisions"
  | "login"
  | "messages"
  | "results"
  | "tree";

/** Every read the hook makes, answered empty but for the tree read (`tree`, by default the
 *  plan's one task and `comments`), the reader's login and `comments`. The `held` read stays out
 *  until its `until` settles, and then answers as it would have. No document's blocks are read:
 *  that read answers nothing, and a case reads that it was never sent. */
function stubTreeReads(
  answerNames: (refs: readonly string[]) => Promise<{
    targets: ReferenceTarget[];
  }>,
  {
    comments = [],
    held,
    tree = treeAnswer(comments),
  }: {
    readonly comments?: readonly Comment[];
    readonly held?: { readonly read: TreeRead; readonly until: Promise<void> };
    readonly tree?: TreeAnswer;
  } = {}
) {
  const answered = (read: TreeRead): Promise<void> =>
    read === held?.read ? held.until : Promise.resolve();
  return [
    spyOn(api, "getIssueCoverage").mockImplementation(async () => {
      await answered("coverage");
      return { gaps: [], has_plan: true, issue: KEY, requirements: [], spec_artifact_id: "" };
    }),
    spyOn(api, "getIssueDecisions").mockImplementation(async () => {
      await answered("decisions");
      return { decisions: [], document: null, entries: [], open_questions: [] };
    }),
    spyOn(api, "getIssueResults").mockImplementation(async () => {
      await answered("results");
      return { checks: [], issue: KEY, verification_artifact_id: "" };
    }),
    spyOn(api, "getIssueReferences").mockResolvedValue({
      linked_from: [],
      links: [],
      members: [],
      truncated: false,
    }),
    // The outline's one asks read, each ask with its thread (`issueAskThreadsQuery`).
    spyOn(api, "listIssueAskThreads").mockImplementation(async () => {
      await answered("asks");
      return [];
    }),
    spyOn(api, "listComments").mockImplementation(async () => {
      await answered("comments");
      return [...comments];
    }),
    spyOn(api, "listIssueMessages").mockImplementation(async () => {
      await answered("messages");
      return [];
    }),
    spyOn(api, "getIssueTree").mockImplementation(async () => {
      await answered("tree");
      return tree;
    }),
    spyOn(api, "getArtifactBlocks").mockResolvedValue([]),
    spyOn(api, "whoAmI").mockImplementation(async () => {
      await answered("login");
      return { event_head: "0", kind: "user", login: "alice" };
    }),
    spyOn(api, "getReferenceTargets").mockImplementation(answerNames),
  ];
}

/** The task row's own words, which the tree read answers. */
function taskWords(root: TreeNode): string | undefined {
  return nodesInOrder(root).find((node) => node.id === `${PLAN}#b-t1`)?.text;
}

/** The issue the plan task needs, which the tree read holds no node for, so the batched name
 *  read alone names the task's chip for it. */
const NEEDED = "dispatch://CORE-7";

/** The name the task's chip for the issue it needs draws. */
function neededName(root: TreeNode): string | undefined {
  return nodesInOrder(root)
    .find((node) => node.id === `${PLAN}#b-t1`)
    ?.chips.find((chip) => chip.ref === NEEDED)?.name;
}

test("the names already in hand stand while a tree that grew reads the new set", async () => {
  // Every read after the first is held open: that window, where the page is drawn and the
  // names of its new set of addresses are not in yet, is what this test is about.
  const held: (() => void)[] = [];
  let reads = 0;
  const needs = { tuples: [treeTuple("needs", "t1", NEEDED)] };
  const stubs = stubTreeReads(
    async (refs) => {
      reads += 1;
      if (reads > 1) {
        const gate = Promise.withResolvers<void>();
        held.push(gate.resolve);
        await gate.promise;
      }
      return {
        targets: [...refs].map(
          (ref): ReferenceTarget =>
            ref === NEEDED
              ? { gone: false, kind: "issue", name: "The needed issue", ref }
              : { gone: false, kind: "block", name: `words of ${ref}`, ref }
        ),
      };
    },
    { tree: treeAnswer([], needs) }
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(neededName(view.result.current)).toBe("The needed issue"));

    // A comment citing another issue arrives with the tree read's next answer: the tree holds one
    // address more, so the names read is a key nobody has answered, and it is still out when the
    // page draws again.
    await act(async () => {
      client.setQueryData(
        conversationTreeKey(KEY),
        treeAnswer([{ ...comment, body: "Read dispatch://CORE-8 first." }], needs)
      );
      await Promise.resolve();
    });
    await waitFor(() =>
      expect(nodesInOrder(view.result.current).some((node) => node.id === comment.id)).toBe(true)
    );
    expect(reads).toBe(2);
    expect(neededName(view.result.current)).toBe("The needed issue");
    // Nothing is left mounted when the held read lands, so its answer updates no state.
    view.unmount();
  } finally {
    for (const release of held) release();
    for (const stub of stubs) stub.mockRestore();
  }
});

// A names read that fails under a new key - the tree grew by one comment - holds no answer of its
// own, where a refetch that fails under the same key keeps one. The names the last answer gave
// stand all the same: the task row keeps its words and the page keeps publishing the name of what
// a comment cites, while the view says the read failed (`namesFailure`), and goes on saying so
// while its Retry reads it again. A read under that key that succeeds draws its own names, and
// nothing has failed any more.
test("a names read that fails for a tree that grew keeps the names in hand, and says it failed", async () => {
  const cited = "dispatch://CORE-7";
  const citing: Comment = { ...comment, body: `Read ${cited} first.` };
  let title = "The cited issue";
  let failing = false;
  // Where set, a names read stays out until it settles.
  let out: Promise<void> | undefined;
  const stubs = stubTreeReads(
    async (refs) => {
      await out;
      if (failing) throw new Error("the names read failed");
      return {
        targets: namedTargets(refs).targets.map((target) =>
          target.ref === cited ? { ...target, kind: "issue", name: title } : target
        ),
      };
    },
    { comments: [citing] }
  );
  // No retry: the read under the new key fails on its first attempt.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </RoutedQueries>
  );
  try {
    const view = renderHook(
      () => {
        const tree = useIssueTree(issue);
        return { name: useKnownReferenceName(cited), tree };
      },
      { wrapper }
    );
    await waitFor(() => expect(view.result.current.name).toBe("The cited issue"));
    expect(taskWords(view.result.current.tree.root)).toBe(TASK_WORDS);
    expect(view.result.current.tree.namesFailure).toBeUndefined();

    // A second comment, citing another issue, arrives with the tree read's next answer: the tree
    // holds one address more, and the names read of that set fails.
    failing = true;
    const second: Comment = {
      ...comment,
      body: "A second turn, after dispatch://CORE-8.",
      id: "comment-2",
    };
    await act(async () => {
      client.setQueryData(conversationTreeKey(KEY), treeAnswer([citing, second]));
      await Promise.resolve();
    });
    await waitFor(() => expect(view.result.current.tree.namesFailure).toBeDefined());
    const grown = view.result.current.tree.root;
    expect(nodesInOrder(grown).some((node) => node.id === second.id)).toBe(true);
    expect(taskWords(grown)).toBe(TASK_WORDS);
    expect(view.result.current.name).toBe("The cited issue");

    // Its Retry reads it again: while that read is out the names still stand failed, busy, and
    // once it fails again they say a second failure.
    const again = Promise.withResolvers<void>();
    out = again.promise;
    await act(async () => view.result.current.tree.namesFailure?.retry());
    await waitFor(() => expect(view.result.current.tree.namesFailure?.retrying).toBe(true));
    expect(view.result.current.tree.namesFailure?.failure).toBe(1);
    out = undefined;
    await act(async () => again.resolve());
    await waitFor(() => expect(view.result.current.tree.namesFailure?.retrying).toBe(false));
    expect(view.result.current.tree.namesFailure?.failure).toBe(2);
    expect(view.result.current.name).toBe("The cited issue");

    // The cited issue is renamed, and the failed read is read again, as window focus reads a
    // read in error again (`refetchFailedOnFocus`): this time it answers.
    failing = false;
    title = "The cited issue, renamed";
    await act(async () => {
      await client.refetchQueries({ predicate: (query) => query.state.status === "error" });
    });
    await waitFor(() => expect(view.result.current.name).toBe("The cited issue, renamed"));
    expect(view.result.current.tree.namesFailure).toBeUndefined();
    expect(taskWords(view.result.current.tree.root)).toBe(TASK_WORDS);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// A names read moved to a set of addresses whose query already failed holds no answer there, and
// the failure that query holds is new to the names line, which says it again (`useLineRefusals`).
test("a names read moved to a set of addresses whose query already failed says its failure again", async () => {
  // Where set, a names read stays out until it settles; every one of them fails.
  let out: Promise<void> | undefined;
  const stubs = stubTreeReads(async () => {
    await out;
    throw new Error("the names read failed");
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  const held = Promise.withResolvers<void>();
  try {
    const view = renderHook(() => useIssueTree(issue).namesFailure, { wrapper });
    await waitFor(() => expect(view.result.current?.failure).toBe(1));
    // A comment citing another issue arrives with the tree read's next answer, and the names read
    // of the tree's new set fails too.
    await act(async () =>
      client.setQueryData(
        conversationTreeKey(KEY),
        treeAnswer([{ ...comment, body: "Read dispatch://CORE-8 first." }])
      )
    );
    await waitFor(() => expect(view.result.current?.failure).toBe(2));
    // The comment goes: the read moves back to the first set, whose query failed, and is out.
    out = held.promise;
    await act(async () => client.setQueryData(conversationTreeKey(KEY), treeAnswer([])));
    await waitFor(() => expect(view.result.current?.retrying).toBe(true));
    expect(view.result.current?.failure, "the first set's failure was not said again").toBe(3);
    view.unmount();
  } finally {
    held.resolve();
    for (const stub of stubs) stub.mockRestore();
  }
});

/** What the batched name read answers for `refs`: the plan task's words for its address, and
 *  words of their own for every other. */
function namedTargets(refs: readonly string[]): { targets: ReferenceTarget[] } {
  return {
    targets: refs.map(
      (ref): ReferenceTarget => ({
        gone: false,
        kind: "block",
        name: ref === TASK_REF ? TASK_WORDS : `words of ${ref}`,
        ref,
      })
    ),
  };
}

/** One document of the issue, with whatever the case gives it: a role, or being the spec. */
function issueDocument(id: string, slug: string, extra: Partial<Artifact> = {}): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: KEY,
    kind: "doc",
    name: slug,
    primary: false,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
    ...extra,
  };
}

/** Every read the tree is built from, in the order the test holds each one open. */
const TREE_READS: readonly TreeRead[] = [
  "comments",
  "asks",
  "coverage",
  "decisions",
  "messages",
  "results",
  "tree",
  "login",
];

// The names come last and all at once. While any one read of the tree is still
// out - the comments, the asks, the coverage, the decisions, the messages, the results, the tree
// read or the reader's login - the addresses known so far are not read; once it answers, every
// address the tree's words cite - here the plan's task and an issue, both of which a comment
// cites - is read in one request, and the page publishes the answer to every surface beside it
// (`reference-names.tsx`).
test("The names are read in one batched request, after every other read has answered", async () => {
  const cited = "dispatch://CORE-7";
  for (const read of TREE_READS) {
    const asked: string[][] = [];
    const out = Promise.withResolvers<void>();
    const stubs = stubTreeReads(
      async (refs) => {
        asked.push([...refs]);
        return namedTargets(refs);
      },
      {
        comments: [{ ...comment, body: `Read ${cited} and ${TASK_REF} first.` }],
        held: { read, until: out.promise },
      }
    );
    // A client and a page of the turn's own, so no answer of an earlier turn is in hand.
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <RoutedQueries client={client}>
        <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
      </RoutedQueries>
    );
    try {
      const view = renderHook(
        () => ({
          // How many reads were out when the hook last drew.
          fetching: client.isFetching(),
          name: useKnownReferenceName(TASK_REF),
          root: useIssueTree(issue).root,
        }),
        { wrapper }
      );
      // The hook has drawn with every other read answered, the held one the only read still
      // out: no name is read yet.
      await waitFor(() => expect(view.result.current.fetching).toBe(1));
      expect(asked, `no names read while the ${read} read is out`).toEqual([]);

      await act(async () => {
        out.resolve();
        await out.promise;
      });
      // The read sends no reading of the task, so the name the page publishes for it is the
      // page's word for its kind, as a chip naming it draws (`targetNaming`).
      await waitFor(() => expect(view.result.current.name).toBe(NODE_TYPE_NAMES.plan_task));
      expect(asked, `one names read after the ${read} read answers`).toHaveLength(1);
      expect(asked[0]).toContain(TASK_REF);
      expect(asked[0]).toContain(cited);
      view.unmount();
    } finally {
      out.resolve();
      for (const stub of stubs) stub.mockRestore();
    }
  }
});

// The tree read holds every node's words, its stored source among them, so the batched names read
// is given no node's own address for the node: neither a block of a document's, here the plan's
// task, nor a comment's. It is still given the address of what a chip names outside the tree,
// here the issue the task needs.
test("the batched names read is given no node's own address, and a chip's address outside the tree", async () => {
  const asked: string[][] = [];
  const commentRef = `dispatch://${KEY}/comment/${comment.id}`;
  const stubs = stubTreeReads(
    async (refs) => {
      asked.push([...refs]);
      return namedTargets(refs);
    },
    {
      comments: [comment],
      tree: treeAnswer([comment], { tuples: [treeTuple("needs", "t1", NEEDED)] }),
    }
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper });
    await waitFor(() => expect(view.result.current.namesHeld).toBe(true));
    const own = nodesInOrder(view.result.current.root).flatMap((node) =>
      node.reference === undefined ? [] : [node.reference]
    );
    expect(own, "the tree holds the task and the comment by their own addresses").toEqual(
      expect.arrayContaining([TASK_REF, commentRef])
    );
    expect(asked).toHaveLength(1);
    expect(
      asked[0]?.filter((reference) => own.includes(reference)),
      "the names read was given a node's own address"
    ).toEqual([]);
    expect(asked[0]).toContain(NEEDED);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// The batched read resolves an id a comment writes short in the owner the reference names, and
// answers under the reference as the request wrote it. So the names read is asked for the short
// address itself, and the page publishes its answer under that address, which is the one a row
// links it by.
test("a short address a comment cites is read as written and named from that answer", async () => {
  const cited = `dispatch://${KEY}/comment/c0b271c7`;
  const asked: string[][] = [];
  const stubs = stubTreeReads(
    async (refs) => {
      asked.push([...refs]);
      return {
        targets: namedTargets(refs).targets.map((target) =>
          target.ref === cited
            ? { ...target, kind: "comment", name: "The comment its short id names" }
            : target
        ),
      };
    },
    { comments: [{ ...comment, body: `Read ${cited} first.` }] }
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </RoutedQueries>
  );
  try {
    const view = renderHook(
      () => {
        useIssueTree(issue);
        return useKnownReferenceName(cited);
      },
      { wrapper }
    );
    await waitFor(() => expect(view.result.current).toBe("The comment its short id names"));
    expect(asked[0]).toContain(cited);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// The batched read answers a short reference whose id two or more targets carry gone and
// ambiguous, and one whose target is gone gone alone. Neither has a name, so the page publishes
// none for either; it publishes the ambiguous one among the references it read as ambiguous,
// which a link draws as written and links nothing, and leaves the gone one out of them, which a
// link reads its own.
test("the page publishes an ambiguous reference a comment cites as ambiguous, a gone one not", async () => {
  const ambiguous = `dispatch://${KEY}/comment/c0b271c7`;
  const gone = `dispatch://${KEY}/comment/c0ffee00`;
  const stubs = stubTreeReads(
    async (refs) => ({
      targets: namedTargets(refs).targets.map((target): ReferenceTarget => {
        if (target.ref === ambiguous) {
          return { ...target, ambiguous: true, gone: true, kind: "comment", name: "" };
        }
        return target.ref === gone ? { ...target, gone: true, kind: "comment", name: "" } : target;
      }),
    }),
    { comments: [{ ...comment, body: `Read ${ambiguous} and ${gone} first, then ${TASK_REF}.` }] }
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </RoutedQueries>
  );
  try {
    const view = renderHook(
      () => {
        useIssueTree(issue);
        return {
          ambiguous: useKnownAmbiguousReferences(),
          names: [useKnownReferenceName(ambiguous), useKnownReferenceName(gone)],
          task: useKnownReferenceName(TASK_REF),
        };
      },
      { wrapper }
    );
    await waitFor(() => expect([...view.result.current.ambiguous]).toEqual([ambiguous]));
    // The read has answered and its names are published with the set: the task's among them,
    // and none for the ambiguous reference or the gone one.
    expect(view.result.current.task).toBe(NODE_TYPE_NAMES.plan_task);
    expect(view.result.current.names).toEqual([undefined, undefined]);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// R2 of dispatch://INFRA-1523: the page publishes an edited post a comment cites by its words
// alone, and the post among the references whose post its author edited, so a link cuts the
// words to its own length and marks what the cut left ` · edited`.
test("the page publishes an edited post a comment cites by its words, among the edited references", async () => {
  const cited = `dispatch://${KEY}/comment/c0ffee00`;
  const stubs = stubTreeReads(
    async (refs) => ({
      targets: namedTargets(refs).targets.map(
        (target): ReferenceTarget =>
          target.ref === cited
            ? { ...target, edited_at: "2026-11-05T02:00:00Z", kind: "comment", name: "Hold it." }
            : target
      ),
    }),
    { comments: [{ ...comment, body: `Read ${cited} first, then ${TASK_REF}.` }] }
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </RoutedQueries>
  );
  try {
    const view = renderHook(
      () => {
        useIssueTree(issue);
        return {
          edited: useKnownEditedReferences(),
          name: useKnownReferenceName(cited),
          task: useKnownReferenceName(TASK_REF),
        };
      },
      { wrapper }
    );
    await waitFor(() => expect([...view.result.current.edited]).toEqual([cited]));
    expect(view.result.current.name).toBe("Hold it.");
    // A target nobody edited is published by its name and left out of the set.
    expect(view.result.current.task).toBe(NODE_TYPE_NAMES.plan_task);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// R3 of dispatch://INFRA-1523, and INFRA-1196's decision 342: the page publishes a deleted post a
// comment cites by its deleted line, and the post among the deleted references by its delete, so
// a row's words draw the line with its actor named as the page names an author.
test("the page publishes a deleted post a comment cites by its deleted line, among the deleted references", async () => {
  const cited = `dispatch://${KEY}/comment/c0ffee00`;
  const deletion = { at: "2026-11-05T02:00:00Z", by: { id: "alice", kind: "user" } };
  const stubs = stubTreeReads(
    async (refs) => ({
      targets: namedTargets(refs).targets.map(
        (target): ReferenceTarget =>
          target.ref === cited
            ? {
                ...target,
                deleted_at: deletion.at,
                deleted_by: deletion.by,
                kind: "comment",
                name: "",
              }
            : target
      ),
    }),
    { comments: [{ ...comment, body: `Read ${cited} first, then ${TASK_REF}.` }] }
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </RoutedQueries>
  );
  try {
    const view = renderHook(
      () => {
        useIssueTree(issue);
        return {
          deleted: useKnownDeletedReferences(),
          name: useKnownReferenceName(cited),
          task: useKnownReferenceName(TASK_REF),
        };
      },
      { wrapper }
    );
    await waitFor(() => expect([...view.result.current.deleted]).toEqual([[cited, deletion]]));
    expect(view.result.current.name).toBe("Deleted by alice");
    // A target nobody deleted is published by its name and left out of the map.
    expect(view.result.current.task).toBe(NODE_TYPE_NAMES.plan_task);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

/** What one surface publishes: the names it read, the references it read as ambiguous, the
 *  references among the names it read edited, and those it read deleted, none where absent. */
interface Published {
  readonly ambiguous: ReadonlySet<string>;
  readonly deleted?: ReadonlyMap<string, PostDeletion>;
  readonly edited?: ReadonlySet<string>;
  readonly names: ReadonlyMap<string, string>;
}

/** What a link under the provider read on its latest render. */
interface Seen {
  ambiguous?: ReadonlySet<string>;
  deleted?: ReadonlyMap<string, PostDeletion>;
  edited?: ReadonlySet<string>;
  names?: ReadonlyMap<string, string>;
}

/** A surface that publishes `published` for as long as it is mounted. */
function Publisher({ published }: { readonly published: Published }): ReactNode {
  usePublishReferenceNames(
    published.names,
    published.ambiguous,
    published.edited,
    published.deleted
  );
  return null;
}

/** A link under the provider, which writes what it reads into `seen` on every render. */
function Reader({ seen }: { readonly seen: Seen }): ReactNode {
  seen.ambiguous = useKnownAmbiguousReferences();
  seen.deleted = useKnownDeletedReferences();
  seen.edited = useKnownEditedReferences();
  seen.names = useKnownReferenceNames();
  return null;
}

// More than one surface can publish under the one provider at a time: the page's own column and
// a sheet that draws the outline beside it each read the tree and publish what they read. One of
// them leaving - the sheet closing, or a pane unmounting at a narrow width while the outline
// stays - withdraws only what it published itself, so every link keeps the names the surfaces
// still mounted read, and draws the references they read as ambiguous as written.
test("one publisher leaving keeps the names and ambiguous references another published", () => {
  const shared = `dispatch://${KEY}/comment/c0ffee00`;
  const other = "dispatch://CORE-7";
  const pageAmbiguous = `dispatch://${KEY}/comment/c0b271c7`;
  const sheetAmbiguous = `dispatch://${KEY}/ask/a5`;
  const page: Published = {
    ambiguous: new Set([pageAmbiguous]),
    names: new Map([
      [TASK_REF, TASK_WORDS],
      [shared, "Hold the deploy."],
    ]),
  };
  const sheet: Published = {
    ambiguous: new Set([sheetAmbiguous]),
    names: new Map([
      [other, "Another issue"],
      [shared, "Hold the deploy until Friday."],
    ]),
  };
  const seen: Seen = {};
  const surfaces = (mounted: { readonly page: boolean; readonly sheet: boolean }) => (
    <ReferenceNamesProvider>
      {mounted.page && <Publisher published={page} />}
      {mounted.sheet && <Publisher published={sheet} />}
      <Reader seen={seen} />
    </ReferenceNamesProvider>
  );

  // Both publish, the sheet last, so its name stands for the reference both name.
  const view = render(surfaces({ page: true, sheet: true }));
  expect(seen.names).toEqual(
    new Map([
      [TASK_REF, TASK_WORDS],
      [shared, "Hold the deploy until Friday."],
      [other, "Another issue"],
    ])
  );
  expect(seen.ambiguous).toEqual(new Set([pageAmbiguous, sheetAmbiguous]));
  // A render in which no publisher changes hands the link the same map and the same set.
  const { ambiguous, names } = seen;
  view.rerender(surfaces({ page: true, sheet: true }));
  expect(seen.names).toBe(names);
  expect(seen.ambiguous).toBe(ambiguous);

  // The sheet closes: the page's names stay, its own for the reference both named among them,
  // and so does the reference it read as ambiguous.
  view.rerender(surfaces({ page: true, sheet: false }));
  expect(seen.names).toEqual(page.names);
  expect(seen.ambiguous).toEqual(page.ambiguous);

  // The page leaves too: nothing is published.
  view.rerender(surfaces({ page: false, sheet: false }));
  expect(seen.names).toEqual(new Map());
  expect(seen.ambiguous).toEqual(new Set());
  view.unmount();
});

// A name and its edit mark come from one read. Where two surfaces name one post, the mark of the
// one whose name stands - the last to publish - stands with it: the page read a comment edited,
// and the sheet, publishing after it, read the comment deleted, whose deleted line takes no
// mark, so a link reads that line unmarked and not as `Deleted by alice · edited`. Once the sheet
// closes, the page's name and its mark stand again.
test("the edited mark a link reads is the one of the publisher whose name stands", () => {
  const deleted = `dispatch://${KEY}/comment/c0ffee00`;
  const edited = `dispatch://${KEY}/comment/c0ffee01`;
  const page: Published = {
    ambiguous: new Set(),
    edited: new Set([deleted, edited]),
    names: new Map([
      [deleted, "Hold the deploy."],
      [edited, "Ship it."],
    ]),
  };
  const sheet: Published = {
    ambiguous: new Set(),
    names: new Map([[deleted, "Deleted by alice"]]),
  };
  const seen: Seen = {};
  const surfaces = (mounted: { readonly page: boolean; readonly sheet: boolean }) => (
    <ReferenceNamesProvider>
      {mounted.page && <Publisher published={page} />}
      {mounted.sheet && <Publisher published={sheet} />}
      <Reader seen={seen} />
    </ReferenceNamesProvider>
  );

  const view = render(surfaces({ page: true, sheet: true }));
  expect(seen.names?.get(deleted)).toBe("Deleted by alice");
  expect(seen.edited).toEqual(new Set([edited]));

  // The sheet closes: the page's own set stands, as it published it.
  view.rerender(surfaces({ page: true, sheet: false }));
  expect(seen.names?.get(deleted)).toBe("Hold the deploy.");
  expect(seen.edited).toBe(page.edited);

  view.rerender(surfaces({ page: false, sheet: false }));
  expect(seen.edited).toEqual(new Set());
  view.unmount();
});

// A name and its delete come from one read, as a name and its edit mark do. Where two surfaces
// name one post, the delete of the one whose name stands - the last to publish - stands with it:
// the page read a comment standing, and the sheet, publishing after it, read it deleted, so a
// row's words draw the sheet's deleted line from its delete. A post the sheet named standing,
// which the page read deleted before, is read standing. Once the sheet closes, the page's names
// and its own map stand again.
test("the delete a link reads is the one of the publisher whose name stands", () => {
  const deleted = `dispatch://${KEY}/comment/c0ffee00`;
  const restored = `dispatch://${KEY}/comment/c0ffee01`;
  const deletion = { at: "2026-11-05T02:00:00Z", by: { id: "alice", kind: "user" } };
  const page: Published = {
    ambiguous: new Set(),
    deleted: new Map([[restored, deletion]]),
    names: new Map([
      [deleted, "Hold the deploy."],
      [restored, "Deleted by alice"],
    ]),
  };
  const sheet: Published = {
    ambiguous: new Set(),
    deleted: new Map([[deleted, deletion]]),
    names: new Map([
      [deleted, "Deleted by alice"],
      [restored, "Ship it."],
    ]),
  };
  const seen: Seen = {};
  const surfaces = (mounted: { readonly page: boolean; readonly sheet: boolean }) => (
    <ReferenceNamesProvider>
      {mounted.page && <Publisher published={page} />}
      {mounted.sheet && <Publisher published={sheet} />}
      <Reader seen={seen} />
    </ReferenceNamesProvider>
  );

  const view = render(surfaces({ page: true, sheet: true }));
  expect(seen.names?.get(deleted)).toBe("Deleted by alice");
  expect(seen.deleted).toEqual(new Map([[deleted, deletion]]));

  // The sheet closes: the page's own map stands, as it published it.
  view.rerender(surfaces({ page: true, sheet: false }));
  expect(seen.names?.get(deleted)).toBe("Hold the deploy.");
  expect(seen.deleted).toBe(page.deleted);

  view.rerender(surfaces({ page: false, sheet: false }));
  expect(seen.deleted).toEqual(new Map());
  view.unmount();
});

// R21: the batched read names an ask by its question as stored, which is markdown, and every
// link that reads the page's published names draws the name as it is, so the page publishes the
// question's plain words, the name a chip naming the ask draws (`targetNaming`).
test("a published name of an ask is its question's plain words, never its markdown", async () => {
  const cited = `dispatch://${KEY}/ask/ask-9`;
  const stubs = stubTreeReads(
    async (refs) => ({
      targets: namedTargets(refs).targets.map((target) =>
        target.ref === cited
          ? {
              ...target,
              kind: "ask",
              name: "**Hold** the `deploy` until [the plan](https://example.com/plan) lands",
            }
          : target
      ),
    }),
    { comments: [{ ...comment, body: `Read ${cited} first.` }] }
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </RoutedQueries>
  );
  try {
    const view = renderHook(
      () => {
        useIssueTree(issue);
        return useKnownReferenceName(cited);
      },
      { wrapper }
    );
    await waitFor(() => expect(view.result.current).toBe("Hold the deploy until the plan lands"));
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

/** A document of the issue that plays no part. */
const notes = issueDocument("artifact-notes", "notes-md");

// As the tester measured, one block read for every document of the issue was 25 of a first
// load's 47 requests. The tree read answers every typed block of every role document, so the
// outline costs that one request, with no page left to ask for, and no document's blocks are
// read.
test("the tree is read in one request, and no document's blocks are read", async () => {
  const withDocuments: IssueDetails = {
    ...issue,
    artifacts: [
      issueDocument("artifact-spec", "spec", { primary: true }),
      plan,
      issueDocument("artifact-verification", "verification-md", { role: "verification" }),
      issueDocument("artifact-decisions", "decisions-md", { role: "decisions" }),
      notes,
    ],
    decisions_artifact_id: "artifact-decisions",
    plan_artifact_id: plan.id,
    primary_artifact_id: "artifact-spec",
    verification_artifact_id: "artifact-verification",
  };
  let named = 0;
  const stubs = stubTreeReads(async (refs) => {
    named += 1;
    return namedTargets(refs);
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(withDocuments).root, { wrapper });
    // The names are read once every other read has answered, the tree read among them.
    await waitFor(() => expect(named).toBe(1));
    const reads = (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls;
    expect<unknown>(reads.map(([key, query]) => [key, query])).toEqual([
      [KEY, { kind: [...CONVERSATION_KINDS] }],
    ]);
    expect(api.getArtifactBlocks).not.toHaveBeenCalled();
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// The server gives a document its part by its role, or, for a row a rolling deploy left with no
// role, by the slug that part is made under, and it omits `role` on such a row. The issue read
// names the document all the same (`plan_artifact_id`), and the page names it by that id; its
// task is a row of the tree, which the tree read answers with no blocks read.
test("A plan with no role that the issue names by its id is named the plan, its task read from the tree", async () => {
  const roleless = issueDocument(plan.id, PLAN);
  const named: IssueDetails = { ...issue, artifacts: [roleless], plan_artifact_id: roleless.id };
  const cites = `dispatch://${KEY}/artifact/${PLAN}`;
  const stubs = stubTreeReads(async (refs) => namedTargets(refs), {
    comments: [{ ...comment, body: `The tasks are in ${cites}.` }],
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(named).root, { wrapper });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    expect(api.getArtifactBlocks).not.toHaveBeenCalled();
    // A comment citing the whole plan carries a chip that names it as the plan.
    const chips = nodesInOrder(view.result.current).find((node) => node.id === comment.id)?.chips;
    expect(chips?.map(({ name, ref }) => ({ name, ref }))).toEqual([
      { name: PRESET_NAMES.plan, ref: cites },
    ]);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// The batched read names a typed block a comment cites by its stored excerpt, which opens on the
// block's directive line. Every link that reads the page's published names draws the name as it
// is, so the page publishes the name a chip naming the block draws (`targetNaming`): the first
// line of the server's reading where the read sends one, and the page's word for the block's kind
// where it sends none - never the directive line.
test("A published name of a typed block is never its directive line", async () => {
  const opener = `:::plan_task{#t1 serves="dispatch://${KEY}/spec#b-r1"}`;
  const reading: DisplayTree = {
    content: [
      {
        content: [{ marks: [{ type: "strong" }], text: TASK_WORDS, type: "text" }],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  for (const [sent, answer, published] of [
    ["a reading", { name: `${opener}\n**${TASK_WORDS}**\n:::`, text_tree: reading }, TASK_WORDS],
    ["no reading", { name: `${opener}\n${TASK_WORDS}\n:::` }, NODE_TYPE_NAMES.plan_task],
  ] as const) {
    const stubs = stubTreeReads(
      async (refs) => ({
        targets: namedTargets(refs).targets.map((target) =>
          target.ref === TASK_REF ? { ...target, ...answer } : target
        ),
      }),
      { comments: [{ ...comment, body: `Start with ${TASK_REF}.` }] }
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <RoutedQueries client={client}>
        <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
      </RoutedQueries>
    );
    try {
      const view = renderHook(
        () => {
          useIssueTree(issue);
          return useKnownReferenceName(TASK_REF);
        },
        { wrapper }
      );
      await waitFor(() => expect(view.result.current, `the read sent ${sent}`).toBe(published));
      view.unmount();
    } finally {
      for (const stub of stubs) stub.mockRestore();
    }
  }
});

// A refetch of the tree read that fails keeps the answer the first read gave - TanStack holds a
// query's data through a failed refetch, and the tree is still built from it - so a view that
// draws from the blocks it holds keeps drawing them
// (`blocksHeld`), while a view that needs every read to have answered stops waiting on one that
// failed (`blocksRead`). Before any answer the tree holds no blocks and has read none.
test("blocks a failed refetch leaves in hand stay held while they stop counting as read", async () => {
  const treeOut = Promise.withResolvers<void>();
  const stubs = stubTreeReads(async (refs) => namedTargets(refs), {
    held: { read: "tree", until: treeOut.promise },
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper });
    expect(view.result.current.blocksHeld).toBe(false);
    expect(view.result.current.blocksRead).toBe(false);

    await act(async () => {
      treeOut.resolve();
      await treeOut.promise;
    });
    await waitFor(() => expect(view.result.current.blocksRead).toBe(true));
    expect(view.result.current.blocksHeld).toBe(true);

    // The tree is read again, and this time the read fails.
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValue(
      new Error("the tree read failed")
    );
    await act(async () => {
      await client.refetchQueries({ exact: true, queryKey: conversationTreeKey(KEY) });
    });
    await waitFor(() => expect(view.result.current.blocksRead).toBe(false));
    expect(view.result.current.blocksHeld).toBe(true);
    // The tree is still built from the answer the first read gave, and the plan's blocks too.
    expect(nodesInOrder(view.result.current.root).some((node) => node.id === `${PLAN}#b-t1`)).toBe(
      true
    );
    expect(view.result.current.blocksOf("plan").data?.map((block) => block.id)).toEqual(["t1"]);
    view.unmount();
  } finally {
    treeOut.resolve();
    for (const stub of stubs) stub.mockRestore();
  }
});

// A write of the issue the page sends - here a reply - lands, and the tree is read again through
// the one subscription the page holds (`subscribeTreeFreshness`, which `ViewReadProvider` mounts
// on the page and this case mounts itself), which the write store tells of the write: the case's
// client has no live feed, so its settle reads the tree, which draws the new comment.
test("a write of the issue that lands reads the tree again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "1", kind: "user", login: "alice" });
  const unsubscribe = subscribeTreeFreshness(client, KEY);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    expect(api.getIssueTree).toHaveBeenCalledTimes(1);

    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockResolvedValue(treeAnswer([comment]));
    await landWrite(client);
    await waitFor(
      () =>
        expect(nodesInOrder(view.result.current).some((node) => node.id === comment.id)).toBe(true),
      { timeout: 3_000 }
    );
    expect(api.getIssueTree).toHaveBeenCalledTimes(2);
    view.unmount();
  } finally {
    unsubscribe();
    for (const stub of stubs) stub.mockRestore();
  }
});

// A write of the issue that lands with no answer, with the live feed down: the write store
// refreshes every query the app holds as it lands, which reads the decisions, and the tree and
// the views, which the write holds, are read at its settle. The settle asks for the decisions no
// second time.
test("a write that lands with no answer and the live feed down reads the decisions and the tree once", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  trackQueryFetches(client);
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const before = readsSent();
    await act(async () => {
      await storeWrite(client, KEY, async () => null).mutate();
    });
    await act(async () => {
      await Bun.sleep(2_500);
    });
    const after = readsSent();
    expect({
      decisions: (after.decisions ?? 0) - (before.decisions ?? 0),
      tree: (after.tree ?? 0) - (before.tree ?? 0),
    }).toEqual({ decisions: 1, tree: 1 });
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A Keep's event names the decisions read and the tree (`decision.kept`): the tree is read again
// and the decisions once.
test("a Keep's event reads the tree again and the decisions once", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const unsubscribe = subscribeTreeFreshness(client, KEY);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    expect(api.getIssueDecisions).toHaveBeenCalledTimes(1);

    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockResolvedValue(treeAnswer([comment]));
    act(() => {
      refreshQueries(client, keepEventTargets());
    });
    await waitFor(
      () =>
        expect(nodesInOrder(view.result.current).some((node) => node.id === comment.id)).toBe(true),
      { timeout: 3_000 }
    );
    expect(api.getIssueTree).toHaveBeenCalledTimes(2);
    expect(api.getIssueDecisions).toHaveBeenCalledTimes(2);
    view.unmount();
  } finally {
    unsubscribe();
    for (const stub of stubs) stub.mockRestore();
  }
});

/** How many times each read the hook sends that an event refreshes has been sent, by name: the
 *  outline read (`tree`) apart from the views of the tree (`views`). */
function readsSent(): Record<string, number> {
  const reads = {
    asks: api.listIssueAskThreads,
    comments: api.listComments,
    coverage: api.getIssueCoverage,
    decisions: api.getIssueDecisions,
    messages: api.listIssueMessages,
    names: api.getReferenceTargets,
    results: api.getIssueResults,
  };
  const trees = (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls;
  const views = trees.filter(([, query]) => query?.view !== undefined).length;
  return {
    ...Object.fromEntries(
      Object.entries(reads).map(([name, read]) => [
        name,
        (read as unknown as Mock<() => unknown>).mock.calls.length,
      ])
    ),
    tree: trees.length - views,
    views,
  };
}

// The page's reads are refreshed by events, never by age. A slot the stage mounts again past the
// app's 30 s `staleTime` (`main.tsx`) builds its tree from what the reads hold and sends none of
// them again. The sign-in is the shell's read, outside it.
test("a remount after 30 s sends no read", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const first = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(api.getReferenceTargets).toHaveBeenCalledTimes(1));
    first.unmount();
    const sent = readsSent();

    setSystemTime(new Date(Date.now() + 31_000));
    const again = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(taskWords(again.result.current)).toBe(TASK_WORDS));
    await act(async () => {
      await Promise.resolve();
    });
    expect(readsSent()).toEqual(sent);
    again.unmount();
  } finally {
    setSystemTime();
    for (const stub of stubs) stub.mockRestore();
  }
});

/** `page` as the server answers it where the read goes on past it: the `cursor` that names the
 *  next page, and how many nodes the read holds after this one (`remaining`), which the server
 *  answers beside the cursor and the generated contract does not name yet. */
function pagedAnswer(page: TreeAnswer, cursor: string | null, remaining?: number): TreeAnswer {
  const count = remaining === undefined ? {} : { remaining };
  return { ...page, cursor, ...count };
}

// R5, R15: the tree read holds data once its first page does: the load reads that page alone,
// though it names a next one, and the tree and a role's blocks are drawn from it. The reader's
// press reads the next page, and a refetch then reads both pages again, so one whose second page
// fails keeps both held, and the plan's task on the second page with them.
test("blocksHeld holds once the first page holds data", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  // The tree read's two pages: the issue's own node, then the plan's task.
  const whole = treeAnswer([]);
  const firstPage = pagedAnswer({ ...whole, nodes: (whole.nodes ?? []).slice(0, 1) }, "page-2", 1);
  const secondPage: TreeAnswer = { ...whole, nodes: (whole.nodes ?? []).slice(1) };
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) =>
    query?.cursor === undefined ? firstPage : secondPage
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper });
    await waitFor(() => expect(view.result.current.blocksHeld).toBe(true));
    expect(view.result.current.blocksRead).toBe(true);
    expect(view.result.current.blocksOf("plan").data).toEqual([]);
    expect(view.result.current.root.moreToRead).toBe(true);
    await act(async () => {
      await Bun.sleep(50);
    });
    expect(api.getIssueTree, "the load reads no next page").toHaveBeenCalledTimes(1);

    // The reader's press reads the second page.
    await act(async () => {
      await readNextTreePage(client, KEY, "conversation");
    });
    await waitFor(() =>
      expect(view.result.current.blocksOf("plan").data?.map((block) => block.id)).toEqual(["t1"])
    );
    expect(view.result.current.root.moreToRead).toBeUndefined();

    // The tree is read again, with the page the press loaded, and its second page fails.
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) => {
      if (query?.cursor === undefined) return firstPage;
      throw new Error("the second page failed");
    });
    await act(async () => {
      await client.refetchQueries({ exact: true, queryKey: conversationTreeKey(KEY) });
    });
    await waitFor(() => expect(view.result.current.blocksRead).toBe(false));
    expect(api.getIssueTree).toHaveBeenCalledTimes(4);
    expect(view.result.current.blocksHeld).toBe(true);
    expect(view.result.current.blocksOf("plan").data?.map((block) => block.id)).toEqual(["t1"]);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// A refresh reads again the pages the reader loaded, and where its walk meets a cursor the server
// named already it fails, as one whose page fails does (`readTree`), and never answers the pages
// it read as the tree: the outline that holds an answer keeps it, and draws no failed-read line,
// which is for a read that holds none.
test("a refresh whose walk meets a cursor named twice keeps the answer the outline holds", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  // The tree's first page, the issue and the task, names a second, the comment.
  const whole = treeAnswer([comment]);
  const nodes = whole.nodes ?? [];
  const firstPage = pagedAnswer({ ...whole, nodes: nodes.slice(0, 2) }, "page-2", 1);
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) =>
    query?.cursor === undefined ? firstPage : pagedAnswer({ ...whole, nodes: nodes.slice(2) }, null)
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper });
    await waitFor(() => expect(view.result.current.blocksRead).toBe(true));
    await act(async () => {
      await readNextTreePage(client, KEY, "conversation");
    });
    await waitFor(() => expect(holdsComment(view.result.current.root)).toBe(true));

    // The tree is read again: its second page now names the second page again.
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) =>
      query?.cursor === undefined
        ? firstPage
        : pagedAnswer({ ...whole, nodes: nodes.slice(2) }, "page-2", 1)
    );
    await act(async () => {
      await client.refetchQueries({ exact: true, queryKey: conversationTreeKey(KEY) });
    });
    await waitFor(() => expect(view.result.current.blocksRead).toBe(false));
    expect(api.getIssueTree).toHaveBeenCalledTimes(4);
    expect(client.getQueryState(conversationTreeKey(KEY))?.error?.message).toContain(
      'the cursor "page-2" twice'
    );
    expect(view.result.current.failed).toBe(false);
    expect(view.result.current.blocksHeld).toBe(true);
    expect(taskWords(view.result.current.root)).toBe(TASK_WORDS);
    expect(holdsComment(view.result.current.root)).toBe(true);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

/** The outline's row of the node `id` the body draws, or null where it draws none. */
function outlineRowOf(id: string): HTMLElement | null {
  return document.body.querySelector<HTMLElement>(nodeRowSelectorOf(id));
}

/** The control after the outline's last row that reads the read's next page, or null. */
function moreControl(): HTMLButtonElement | null {
  return document.body.querySelector<HTMLButtonElement>("[data-tree-more] button");
}

// R15: while the outline read's answer names a next page, a control follows the outline's last
// row and says how many nodes the read holds after the pages read, the page's `remaining`, by
// its count and the words that agree with it. A press reads the next page, its rows are drawn
// where the read's order puts them, and the focus goes to the first row it drew; once no page is
// left the control goes.
test("the more control says the page's remaining count", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const later: Comment = {
    ...comment,
    body: "A later turn of the conversation.",
    created_at: "2026-11-06T00:00:00Z",
    id: "comment-2",
  };
  const last: Comment = { ...later, created_at: "2026-11-07T00:00:00Z", id: "comment-3" };
  const whole = treeAnswer([comment, later, last]);
  const nodes = whole.nodes ?? [];
  const pages: Readonly<Record<string, TreeAnswer>> = {
    first: pagedAnswer({ ...whole, nodes: nodes.slice(0, 3) }, "page-2", 1240),
    "page-2": pagedAnswer({ ...whole, nodes: nodes.slice(3, 4) }, "page-3", 1),
    "page-3": pagedAnswer({ ...whole, nodes: nodes.slice(4) }, null),
  };
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(
    async (_key, query) => pages[query?.cursor ?? "first"] ?? pages.first
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: bodyWrapper(client) });
    await waitFor(() => expect(moreControl()?.textContent).toBe("1,240 more rows to load"));
    expect(moreControl()?.textContent).toBe(`1,240 ${CONTROL_LABELS.tree_page_rows}`);
    expect(outlineRowOf(later.id)).toBeNull();

    await act(async () => {
      moreControl()?.click();
    });
    await waitFor(() =>
      expect(moreControl()?.textContent).toBe(`1 ${CONTROL_LABELS.tree_page_row}`)
    );
    await waitFor(() => expect(document.activeElement).toBe(outlineRowOf(later.id)));

    await act(async () => {
      moreControl()?.click();
    });
    await waitFor(() => expect(outlineRowOf(last.id)).not.toBeNull());
    expect(moreControl(), "no control once no page is left").toBeNull();
    expect(
      (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls.map(
        ([, query]) => query?.cursor
      )
    ).toEqual([undefined, "page-2", "page-3"]);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// R15: an address that names a node no page read yet holds - a comment past the first page -
// reads the read's next pages until one brings it, and no page after it, and then lands on it as
// on a node the first page held: its row is drawn with the focus on it, and the item's own read,
// which the page reads where the tree holds no node of the item, is never sent.
test("an address of a node past the first page reads pages until it is loaded", async () => {
  const later: Comment = {
    ...comment,
    body: "A later turn of the conversation.",
    created_at: "2026-11-06T00:00:00Z",
    id: "comment-2",
  };
  const last: Comment = { ...later, created_at: "2026-11-07T00:00:00Z", id: "comment-3" };
  const stubs = stubTreeReads(async (refs) => namedTargets(refs), {
    comments: [comment, later, last],
  });
  const whole = treeAnswer([comment, later, last]);
  const nodes = whole.nodes ?? [];
  const pages: Readonly<Record<string, TreeAnswer>> = {
    first: pagedAnswer({ ...whole, nodes: nodes.slice(0, 3) }, "page-2", 2),
    "page-2": pagedAnswer({ ...whole, nodes: nodes.slice(3, 4) }, "page-3", 1),
    "page-3": pagedAnswer({ ...whole, nodes: nodes.slice(4) }, null),
  };
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(
    async (_key, query) => pages[query?.cursor ?? "first"] ?? pages.first
  );
  const item = spyOn(api, "getComment").mockRejectedValue(new Error("no item read is sent"));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue), {
      wrapper: bodyWrapper(client, `/issues/${KEY}?comment=${later.id}`),
    });
    await waitFor(() => expect(outlineRowOf(later.id)).not.toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(outlineRowOf(later.id)));
    await act(async () => {
      await Bun.sleep(100);
    });
    expect(
      (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls.map(
        ([, query]) => query?.cursor
      )
    ).toEqual([undefined, "page-2"]);
    expect(outlineRowOf(last.id), "no page past the node is read").toBeNull();
    expect(item).not.toHaveBeenCalled();
    view.unmount();
  } finally {
    item.mockRestore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A filter's view read is watched and never sent here: whatever `ViewReadProvider` holds for it
// reaches `presetView` through the tree's root (`root.views`), so the Plan filter draws the
// view's plan tasks - the one its query took, not the second the tree holds - and the root keeps
// its identity while neither the tree nor a held view changes.
test("a held view answer reaches presetView through root.views", async () => {
  const second = answerNode("t2", "plan_task", {
    document: { artifact_id: plan.id, position: 1, slug: PLAN },
    text: "A second step.",
  });
  const stubs = stubTreeReads(async (refs) => namedTargets(refs), {
    tree: treeAnswer([], { nodes: [second] }),
  });
  const planView = viewAnswer({ ...task, role: "query" });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    expect(presetView(view.result.current, "plan", "").root.children.map((row) => row.id)).toEqual([
      `${PLAN}#b-t1`,
      `${PLAN}#b-t2`,
    ]);

    await act(async () => {
      client.setQueryData(viewKey(KEY, "plan"), planView);
      await Promise.resolve();
    });
    await waitFor(() => expect(view.result.current.views?.plan).toBe(planView));
    expect(presetView(view.result.current, "plan", "").root.children.map((row) => row.id)).toEqual([
      `${PLAN}#b-t1`,
    ]);
    const held = view.result.current;
    view.rerender();
    expect(view.result.current).toBe(held);
    // The hook sent no view read of its own.
    expect(
      (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls.every(
        ([, query]) => query?.view === undefined
      )
    ).toBe(true);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

/** `client`'s queries around `children`, under a router at no issue's address, as the page draws
 *  every reader of the outline under its router (`tree-read.ts` reads the address there): the
 *  page with no filter on. */
function RoutedQueries({
  children,
  client,
}: {
  readonly children: ReactNode;
  readonly client: QueryClient;
}): ReactNode {
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter>{children}</MemoryRouter>
    </QueryClientProvider>
  );
}

/** The stage of the issue as the page mounts it: the view reads' provider, which holds the
 *  page's one refresh subscription, around the hook, on the app's own query client. */
function stageWrapper(client: QueryClient, preset: "conversation" | "spec" = "conversation") {
  return ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ViewReadProvider issue={issue} preset={preset}>
        {children}
      </ViewReadProvider>
    </RoutedQueries>
  );
}

// The reader leaves the issue, someone comments on it, and the event of that comment refreshes
// what it names while no stage of the issue is mounted: the comments and the tree, both marked
// stale. When the reader comes back, both are read again, so the outline draws the comment.
test("an event while the reader is away is drawn when the reader comes back", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = stageWrapper(client);
  try {
    const first = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(taskWords(first.result.current)).toBe(TASK_WORDS));
    first.unmount();

    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockResolvedValue(treeAnswer([comment]));
    (api.listComments as Mock<typeof api.listComments>).mockResolvedValue([comment]);
    act(() => {
      refreshQueries(client, commentEventTargets());
    });

    const back = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(api.listComments).toHaveBeenCalledTimes(2));
    await waitFor(
      () =>
        expect(
          nodesInOrder(back.result.current).some((node) => node.id === comment.id),
          "the comment is a row"
        ).toBe(true),
      { timeout: 3_000 }
    );
    back.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A write, and a turn later the event it raises, land while the open filter's view is being
// read. Nothing cancels that read: every event of a burst would start a view walk of many pages
// over, and on a busy issue the filter would never draw. Once the read out settles, the view is
// read again, at most once for the write and once for its event, so it ends on an answer
// composed after the write.
test("a write while the open view is being read cancels nothing and reads the view again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const specReads: { readonly after: boolean; readonly signal: AbortSignal | undefined }[] = [];
  const out = Promise.withResolvers<void>();
  let written = false;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(
    async (_key, query, init) => {
      if (query?.view !== "spec") return treeAnswer([]);
      specReads.push({ after: written, signal: init?.signal ?? undefined });
      await out.promise;
      return viewAnswer({ ...task, role: "query" });
    }
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: stageWrapper(client, "spec") });
    await waitFor(() => expect(view.result.current.blocksRead).toBe(true));
    await waitFor(() => expect(specReads.length).toBe(1));

    written = true;
    await landWrite(client);
    await act(async () => {
      refreshQueries(client, commentEventTargets());
      await Bun.sleep(0);
    });
    expect(
      specReads.filter((read) => read.signal?.aborted === true).length,
      "no view read is cancelled"
    ).toBe(0);

    await act(async () => {
      out.resolve();
      await Bun.sleep(1_500);
    });
    const again = specReads.filter((read) => read.after).length;
    expect(again, "the view is read again after the write").toBeGreaterThanOrEqual(1);
    expect(again, "the write and its event read the view at most twice").toBeLessThanOrEqual(2);
    expect(specReads.filter((read) => read.signal?.aborted === true).length).toBe(0);
    view.unmount();
  } finally {
    out.resolve();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// Two replies sent close together while the live feed is down: the second reply's invalidation
// of the comments lands while the comments read the first reply started is still out, so the
// comments query is already marked invalid and the cache reports no new invalidation. The
// comments are read once more for the second reply all the same, and the tree has to be too:
// otherwise the outline stops at the tree read made for the first reply, and the second reply is
// no row until some later write.
test("a second write while the first write's comments read is out reads the tree again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const second: Comment = { ...comment, created_at: "2026-11-05T02:00:00Z", id: "comment-2" };
  let written: Comment[] = [];
  let commentsOut: { readonly promise: Promise<void>; readonly resolve: () => void } | undefined;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeAnswer(written)
  );
  (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => {
    await commentsOut?.promise;
    return [...written];
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));

    // The first reply: its comments read stays out, and the tree is read again for it.
    commentsOut = Promise.withResolvers<void>();
    written = [comment];
    await landWrite(client);
    await waitFor(
      () =>
        expect(nodesInOrder(view.result.current).some((node) => node.id === comment.id)).toBe(true),
      { timeout: 3_000 }
    );

    // The second reply, while the first reply's comments read is still out.
    written = [comment, second];
    await landWrite(client);
    await act(async () => {
      commentsOut?.resolve();
      await Bun.sleep(0);
    });
    await waitFor(
      () =>
        expect(
          nodesInOrder(view.result.current).some((node) => node.id === second.id),
          "the second reply is a row"
        ).toBe(true),
      { timeout: 3_000 }
    );
    view.unmount();
  } finally {
    commentsOut?.resolve();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

/** How many reads of the tree, with no view or with `view`, have been sent. */
function treeReadsOf(view?: "plan" | "spec" | "verification"): number {
  return (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls.filter(
    ([, query]) => query?.view === view
  ).length;
}

/** Whether the comment is a row of `root`. */
function holdsComment(root: TreeNode): boolean {
  return nodesInOrder(root).some((node) => node.id === comment.id);
}

/** The filter on in a stage `turnedStage` draws, which a case turns off and on as the reader
 *  presses it. */
interface FilterOn {
  preset: "conversation" | "graph" | "plan" | "spec" | "verification";
}

/** The stage of the issue as `stageWrapper` mounts it, with whichever filter `on` names on, so a
 *  case can turn one off and on as the reader presses it. */
function turnedStage(client: QueryClient, on: FilterOn) {
  return ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ViewReadProvider issue={issue} preset={on.preset}>
        {children}
      </ViewReadProvider>
    </RoutedQueries>
  );
}

// A write that lands while the first tree read of the load is still out, or after that read was
// refused, is answered by one more tree read once the read out settles, and by one read where
// the first one failed.
test("a write while the first tree read is out, or after it failed, reads the tree once more", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const firstOut = Promise.withResolvers<void>();
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads > 1) return treeAnswer([comment]);
    await firstOut.promise;
    return treeAnswer([]);
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(api.listComments).toHaveBeenCalledTimes(1));
    (api.listComments as Mock<typeof api.listComments>).mockResolvedValue([comment]);
    await landWrite(client);
    await act(async () => {
      firstOut.resolve();
      await Bun.sleep(1_500);
    });
    expect(holdsComment(view.result.current), "the write is drawn once the read settles").toBe(
      true
    );
    expect(treeReadsOf(), "one more tree read for the write").toBe(2);
    view.unmount();
  } finally {
    firstOut.resolve();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }

  const again = stubTreeReads(async (refs) => namedTargets(refs));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValueOnce(
    new Error("the tree read was refused")
  );
  const refused = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: stageWrapper(refused) });
    await waitFor(() =>
      expect(refused.getQueryState(conversationTreeKey(KEY))?.status).toBe("error")
    );
    expect(view.result.current.blocksHeld).toBe(false);
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockResolvedValue(treeAnswer([comment]));
    await landWrite(refused);
    await waitFor(() => expect(holdsComment(view.result.current.root)).toBe(true), {
      timeout: 3_000,
    });
    await act(async () => {
      await Bun.sleep(1_200);
    });
    expect(treeReadsOf(), "one read after the refused one").toBe(2);
    view.unmount();
  } finally {
    for (const stub of again) stub.mockRestore();
    refused.clear();
  }
});

// Once its comments read has failed, the comments query stays marked invalid; a reply sent next
// is a write of the issue, which reads the tree again as it lands, so the reply is a row with
// the live feed down.
test("a reply after a failed comments read reads the tree again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));

    (api.listComments as Mock<typeof api.listComments>).mockRejectedValue(
      new Error("the comments read failed")
    );
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["comments", KEY] });
      await Bun.sleep(1_200);
    });
    expect(client.getQueryState(["comments", KEY])?.status).toBe("error");
    const before = treeReadsOf();

    (api.listComments as Mock<typeof api.listComments>).mockResolvedValue([comment]);
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockResolvedValue(treeAnswer([comment]));
    await landWrite(client);
    await waitFor(
      () => expect(holdsComment(view.result.current), "the reply is a row").toBe(true),
      {
        timeout: 3_000,
      }
    );
    expect(treeReadsOf()).toBe(before + 1);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A write is read once by its settle, and the event it raises may queue one more read, never
// more.
test("a write and the event it raises read the tree at most twice", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    const before = treeReadsOf();

    (api.listComments as Mock<typeof api.listComments>).mockResolvedValue([comment]);
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockResolvedValue(treeAnswer([comment]));
    await landWrite(client);
    await act(async () => {
      await Bun.sleep(50);
      refreshQueries(client, commentEventTargets());
      await Bun.sleep(2_500);
    });
    expect(holdsComment(view.result.current)).toBe(true);
    const sent = treeReadsOf() - before;
    expect(sent, "the write's own read").toBeGreaterThanOrEqual(1);
    expect(sent, "and at most one for its event").toBeLessThanOrEqual(2);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A reader who leaves the issue and comes back with nothing changed on it in between finds the
// tree and the reads it is drawn with held, and the page sends none of them again.
test("coming back to an issue nothing changed on reads nothing again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = stageWrapper(client);
  try {
    const first = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(api.getReferenceTargets).toHaveBeenCalledTimes(1));
    first.unmount();
    const sent = readsSent();

    const back = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(taskWords(back.result.current)).toBe(TASK_WORDS));
    await act(async () => {
      await Bun.sleep(1_200);
    });
    // A load sends Plan's view read with it (R4), a load after leaving among them.
    expect(readsSent()).toEqual({ ...sent, views: (sent.views ?? 0) + 1 });
    back.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A filter's view is read the first time the filter is on in a load and held for the rest of
// it: the reader pressing the filter off and on again finds its answer held, and the page sends
// no second view read.
test("a filter's view is read once in a load, and not again when it is pressed off and on", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const specView = viewAnswer({ ...task, role: "query" });
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) =>
    query?.view === "spec" ? specView : treeAnswer([])
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const on: { preset: "conversation" | "spec" } = { preset: "spec" };
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: turnedStage(client, on) });
    await waitFor(() => expect(view.result.current.views?.spec).toBe(specView));
    expect(treeReadsOf("spec")).toBe(1);

    on.preset = "conversation";
    view.rerender();
    on.preset = "spec";
    view.rerender();
    await act(async () => {
      await Bun.sleep(300);
    });
    expect(treeReadsOf("spec"), "no second view read").toBe(1);
    expect(treeReadsOf(), "one tree read").toBe(1);
    expect(view.result.current.views?.spec).toBe(specView);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

/** The stage as the page's root draws it at the address the router holds: the stage's view reads
 *  (`ViewReadProvider`) at the address's view, while each reader of the outline reads the address
 *  itself. `held.navigate` moves the router, as a press of a view's control does. */
function AddressedStage({
  children,
  held,
}: {
  readonly children: ReactNode;
  readonly held: { navigate?: NavigateFunction };
}): ReactNode {
  const { hash, pathname, search } = useLocation();
  held.navigate = useNavigate();
  const route = parseIssuePath(pathname, search);
  const preset =
    route === undefined ? "conversation" : issueAddress(route, search, hash, undefined).preset;
  return (
    <ViewReadProvider issue={issue} preset={preset}>
      {children}
    </ViewReadProvider>
  );
}

/** A load of the issue at `path`, as the page's root mounts it (`AddressedStage`). */
function loadAt(client: QueryClient, path: string, held: { navigate?: NavigateFunction } = {}) {
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <AddressedStage held={held}>{children}</AddressedStage>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

/** How many reads of the tree route were sent, by what each asked for: Conversation's read
 *  (`kind`), the tree read with no view (`whole`), each view by its name, and an older comment
 *  page of either outline read (`… comments`). */
function treeReadsSent(): Record<string, number> {
  const sent: Record<string, number> = {};
  for (const [, query] of (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls) {
    const asked = query as
      | { readonly comments_before?: string; readonly kind?: unknown; readonly view?: string }
      | undefined;
    const read = asked?.view ?? (asked?.kind === undefined ? "whole" : "kind");
    const name = asked?.comments_before === undefined ? read : `${read} comments`;
    sent[name] = (sent[name] ?? 0) + 1;
  }
  return sent;
}

// R1, R4, R10: a load of Graph reads Graph's read, the tree read with no view, once, and nothing
// else of the route: no Conversation's read, and no view read for a count, each count being read
// off Graph's read.
test("a load of Graph sends one tree read with no view and none with kind", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, {
      wrapper: loadAt(client, `/issues/${KEY}/graph`),
    });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    await act(async () => {
      await Bun.sleep(300);
    });
    expect(treeReadsSent()).toEqual({ whole: 1 });
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// R1, R4, R9: a load of Conversation reads Conversation's read once, `kind` repeated for root R1's
// kinds, and Plan's view read with it, whose plan tasks Conversation's read never holds, and no
// tree read with no view. The issue has no children, so neither Spec's nor Verification's view is
// sent with it.
test("a load of Conversation sends one kind read, the plan view read and no read with no view", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, {
      wrapper: loadAt(client, `/issues/${KEY}`),
    });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    await act(async () => {
      await Bun.sleep(300);
    });
    expect(treeReadsSent()).toEqual({ kind: 1, plan: 1 });
    const kinds = (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls.flatMap(
      ([, query]) => {
        const asked = query as { readonly kind?: readonly string[] } | undefined;
        return asked?.kind === undefined ? [] : [asked.kind];
      }
    );
    expect(kinds).toEqual([CONVERSATION_KINDS]);
    expect(view.result.current.views?.plan).toBeDefined();
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// R4: each filter's view is read once in a load: Plan's with the load, and Spec's the first time
// Spec is on. Turned off and on again, a filter finds its view held and the page sends no second
// read, of the view or of Conversation's read.
test("a filter turned off and on sends no second read", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const held: { navigate?: NavigateFunction } = {};
  try {
    const view = renderHook(() => useIssueTree(issue).root, {
      wrapper: loadAt(client, `/issues/${KEY}`, held),
    });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    for (const preset of [
      "plan",
      "conversation",
      "plan",
      "spec",
      "conversation",
      "spec",
    ] as const) {
      act(() => {
        void held.navigate?.(presetPath(KEY, preset));
      });
      await act(async () => {
        await Bun.sleep(150);
      });
    }
    expect(treeReadsSent()).toEqual({ kind: 1, plan: 1, spec: 1 });
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// R6, R15: a write of the issue reads the outline read the page holds again, Conversation's and
// Graph's alike, with the older comment page the reader had loaded, so the older comment stays
// drawn. The case's client has no live feed, so the write's settle reads the outline read.
test("a write of the issue reads each held outline read again with its loaded comment pages", async () => {
  const older: Comment = {
    ...comment,
    body: "An older turn of the conversation.",
    created_at: "2026-11-04T00:00:00Z",
    id: "comment-older",
  };
  const loads = [
    { name: "kind", outline: "conversation", path: `/issues/${KEY}` },
    { name: "whole", outline: "graph", path: `/issues/${KEY}/graph` },
  ] as const;
  for (const { name, outline, path } of loads) {
    const stubs = stubTreeReads(async (refs) => namedTargets(refs));
    const first: TreeAnswer = {
      ...treeAnswer([comment]),
      comments_before: `2026-11-05T01:00:00.000000Z,${comment.id}`,
    };
    const page: TreeAnswer = { ...fixtureAnswer([commentNode(older, KEY)]), comments_before: null };
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) =>
      query?.comments_before === undefined ? first : page
    );
    const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
    const holdsOlder = (root: TreeNode) => nodesInOrder(root).some((one) => one.id === older.id);
    try {
      const view = renderHook(() => useIssueTree(issue).root, { wrapper: loadAt(client, path) });
      await waitFor(() => expect(holdsComment(view.result.current)).toBe(true));
      await act(async () => {
        await readOlderComments(client, KEY, outline);
      });
      await waitFor(() => expect(holdsOlder(view.result.current), outline).toBe(true));
      const before = treeReadsSent();
      expect(before[`${name} comments`], `${outline}: one older page read`).toBe(1);

      await landWrite(client);
      await waitFor(
        () => expect(treeReadsSent()[name], `${outline}: read again`).toBe((before[name] ?? 0) + 1),
        { timeout: 3_000 }
      );
      await waitFor(() =>
        expect(treeReadsSent()[`${name} comments`], `${outline}: its older page read again`).toBe(2)
      );
      expect(holdsOlder(view.result.current), `${outline}: the older comment stays`).toBe(true);
      expect(holdsComment(view.result.current)).toBe(true);
      view.unmount();
    } finally {
      for (const stub of stubs) stub.mockRestore();
      client.clear();
    }
  }
});

// A write of the issue marks every view the load holds stale as it settles and drops none: the
// open filter keeps drawing the view it holds while that view is read again, so it never falls
// back to the tree for the length of the read, and a filter that is off keeps its answer until it
// is next on, when its view is read again.
test("a write marks the held views stale, keeps what they draw, and reads the open one again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const second = answerNode("t2", "plan_task", {
    document: { artifact_id: plan.id, position: 1, slug: PLAN },
    text: "A second step.",
  });
  const verificationView = viewAnswer({ ...task, role: "query" });
  const specBefore = viewAnswer({ ...task, role: "context" });
  const specAfter = viewAnswer({ ...task, role: "context" }, { ...second, role: "context" });
  let written = false;
  const specOut = Promise.withResolvers<void>();
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) => {
    if (query?.view === "verification") return verificationView;
    if (query?.view !== "spec") return treeAnswer([]);
    if (!written) return specBefore;
    await specOut.promise;
    return specAfter;
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  // Verification is the view whose filter is off: Plan's view is read with the load (R4).
  const on: { preset: "spec" | "verification" } = { preset: "verification" };
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: turnedStage(client, on) });
    await waitFor(() => expect(view.result.current.views?.verification).toBe(verificationView));
    on.preset = "spec";
    view.rerender();
    await waitFor(() => expect(view.result.current.views?.spec).toBe(specBefore));

    written = true;
    await landWrite(client);
    await waitFor(() => expect(treeReadsOf("spec"), "the open view is read again").toBe(2), {
      timeout: 3_000,
    });
    expect(view.result.current.views?.spec, "the open view draws what it held").toBe(specBefore);
    expect(
      client.getQueryState(viewKey(KEY, "verification"))?.isInvalidated,
      "the off view is stale"
    ).toBe(true);
    expect(view.result.current.views?.verification, "the off view keeps its answer").toBe(
      verificationView
    );
    expect(treeReadsOf("verification")).toBe(1);

    await act(async () => {
      specOut.resolve();
      await specOut.promise;
    });
    await waitFor(() => expect(view.result.current.views?.spec).toEqual(specAfter));
    on.preset = "verification";
    view.rerender();
    await waitFor(() =>
      expect(treeReadsOf("verification"), "the off view is read when on").toBe(2)
    );
    view.unmount();
  } finally {
    specOut.resolve();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

/** Every read of the tree answered, and every read of the Spec view refused, as by a Dispatch
 *  that cannot serve that view. The cases' client retries nothing, so a refusal is final. */
function refuseSpecView(): void {
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) => {
    if (query?.view === "spec") throw new Error("the spec view read was refused");
    return treeAnswer([]);
  });
}

/** The reader pressing the Spec filter off and on again, on a stage `turnedStage` drew, and the
 *  page left a moment to send whatever it sends for it. */
async function pressSpecOffAndOn(
  view: { readonly rerender: () => void },
  on: FilterOn
): Promise<void> {
  on.preset = "conversation";
  view.rerender();
  on.preset = "spec";
  view.rerender();
  await act(async () => {
    await Bun.sleep(300);
  });
}

/** The reader turning the Spec filter on again, on a stage `turnedStage` drew, after something
 *  asked for its refused view again while the filter was off: the filter on reads the view, which
 *  is refused again, and a press of the filter off and on reads it no more (`message`). */
async function specOnReadsRefusedViewOnce(
  client: QueryClient,
  view: { readonly rerender: () => void },
  on: FilterOn,
  message: string
): Promise<void> {
  on.preset = "spec";
  view.rerender();
  await waitFor(() => expect(treeReadsOf("spec"), "the filter on reads the view").toBe(2));
  await waitFor(() => expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error"));
  await pressSpecOffAndOn(view, on);
  expect(treeReadsOf("spec"), message).toBe(2);
}

// A view read refused, its retries spent, stays refused until something asks for it again: the
// reader's Retry, an event of the issue, the live feed coming back, a focus while the feed is
// down. The reader pressing its filter off and on again is none of those, so the page sends no
// view read for it.
test("a refused view read sends no read when its filter is pressed off and on", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  refuseSpecView();
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const on: FilterOn = { preset: "spec" };
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: turnedStage(client, on) });
    await waitFor(() => expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error"));
    expect(treeReadsOf("spec")).toBe(1);

    await pressSpecOffAndOn(view, on);
    expect(treeReadsOf("spec"), "no view read for the press").toBe(1);
    expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error");
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// An event of the issue lands while the filter of a refused view is off. It names the view
// (`event-refresh.ts`), which nothing reads while the filter is off, so the view is read when its
// filter is next on - once: refused again, it is read no more for a press of its filter.
test("a refused view whose filter is off at an event of the issue is read once when its filter is next on", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  refuseSpecView();
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const on: FilterOn = { preset: "spec" };
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: turnedStage(client, on) });
    await waitFor(() => expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error"));
    on.preset = "conversation";
    view.rerender();
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const tree = treeReadsOf();

    act(() => {
      refreshQueries(client, commentEventTargets());
    });
    await act(async () => {
      await Bun.sleep(2_500);
    });
    expect(treeReadsOf(), "the event reads the tree").toBeGreaterThan(tree);
    expect(treeReadsOf("spec"), "no view read while its filter is off").toBe(1);

    await specOnReadsRefusedViewOnce(
      client,
      view,
      on,
      "the refused view is read once for the event"
    );
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// The open filter's view read is refused, and an event of the issue asks for it again: the view
// is read again while its filter stays on. Refused again, it stays refused until the next ask,
// so the reader pressing the filter off and on sends no read for it.
test("an open refused view is read again by an event of the issue, and no more by a press of its filter", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  refuseSpecView();
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const on: FilterOn = { preset: "spec" };
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: turnedStage(client, on) });
    await waitFor(() => expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error"));
    expect(treeReadsOf("spec")).toBe(1);

    act(() => {
      refreshQueries(client, commentEventTargets());
    });
    await waitFor(() => expect(treeReadsOf("spec"), "the event reads the open view").toBe(2), {
      timeout: 3_000,
    });
    await act(async () => {
      await Bun.sleep(2_500);
    });
    expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error");
    const read = treeReadsOf("spec");

    await pressSpecOffAndOn(view, on);
    expect(treeReadsOf("spec"), "no view read for the press").toBe(read);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A window focus while the live feed is down asks for a refused view again. Its filter is off,
// so nothing reads it then, and it is read when its filter is next on - once, as for an event.
test("a refused view whose filter is off at a focus with the live feed down is read once when its filter is next on", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  refuseSpecView();
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const on: FilterOn = { preset: "spec" };
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: turnedStage(client, on) });
    await waitFor(() => expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error"));
    on.preset = "conversation";
    view.rerender();

    await act(async () => {
      focusWindow();
      await Bun.sleep(300);
    });
    expect(treeReadsOf("spec"), "no view read while its filter is off").toBe(1);

    await specOnReadsRefusedViewOnce(
      client,
      view,
      on,
      "the refused view is read once for the focus"
    );
    view.unmount();
  } finally {
    focusManager.setFocused(undefined);
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A Keep and a status change put what Dispatch answered into the cache once they land; each is a
// write of the issue, so each reads the tree again as it settles while the live feed is down.
test("a Keep and a status change read the tree again as they land", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    expect(treeReadsOf()).toBe(1);

    await landWrite(client, () => {
      client.setQueryData(["decisions", KEY], {
        decisions: [],
        document: null,
        open_questions: [],
      });
    });
    await waitFor(() => expect(treeReadsOf(), "the Keep reads the tree").toBe(2), {
      timeout: 3_000,
    });
    await landWrite(client, () => {
      client.setQueryData(["issue", KEY], { ...issue, status: "in_progress" });
    });
    await waitFor(() => expect(treeReadsOf(), "the status change reads the tree").toBe(3), {
      timeout: 3_000,
    });
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// After a gap in the live feed the page refreshes every query it holds, the tree and the open
// view among them, along with every query of the issue: the refresh those changes raise finds
// the tree and the view already being read for them, so each is read once more, and no read is
// cancelled.
test("a refresh of every query reads the tree and the open view once more each", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const signals: AbortSignal[] = [];
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(
    async (_key, query, init) => {
      if (init?.signal !== undefined) signals.push(init.signal);
      await Bun.sleep(20);
      return query?.view === "spec" ? viewAnswer({ ...task, role: "query" }) : treeAnswer([]);
    }
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, {
      wrapper: stageWrapper(client, "spec"),
    });
    await waitFor(() => expect(view.result.current.views?.spec).toBeDefined());
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const tree = treeReadsOf();
    const spec = treeReadsOf("spec");
    act(() => {
      refreshQueries(client);
    });
    await act(async () => {
      await Bun.sleep(2_500);
    });
    expect({
      cancelled: signals.filter((signal) => signal.aborted).length,
      spec: treeReadsOf("spec") - spec,
      tree: treeReadsOf() - tree,
    }).toEqual({ cancelled: 0, spec: 1, tree: 1 });
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A write that lands as the reader leaves the issue sends no tree read once nothing draws the
// tree: it marks the tree stale, and the tree is read when the reader comes back.
test("a write as the reader leaves reads the tree only when the reader comes back", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = stageWrapper(client);
  try {
    const first = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(taskWords(first.result.current)).toBe(TASK_WORDS));
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockResolvedValue(treeAnswer([comment]));
    const answer = Promise.withResolvers<void>();
    act(() => {
      void storeWrite(client, KEY, () => answer.promise).mutate();
      first.unmount();
    });
    await act(async () => {
      answer.resolve();
      await answer.promise;
    });
    await act(async () => {
      await Bun.sleep(1_500);
    });
    expect(treeReadsOf(), "no tree read while nothing draws it").toBe(1);
    expect(client.getQueryState(conversationTreeKey(KEY))?.isInvalidated).toBe(true);

    const back = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(holdsComment(back.result.current)).toBe(true));
    expect(treeReadsOf()).toBe(2);
    back.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

/** The line the outline draws over itself for a tree read refused before it first answered. */
function failedTreeLine(): HTMLElement | null {
  return document.body.querySelector<HTMLElement>('[data-tree-unreadable] [role="alert"]');
}

// A tree read refused before it first answered, its retries spent, leaves the tree the issue
// alone, which draws no row: the body says over the outline that the read failed, with a Retry
// that reads the tree again, and says nothing of it while the read is out or once it holds an
// answer.
test("a refused tree read says so over the outline, and its Retry reads the tree again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  // The first tree read, held out until the case refuses it; every read after it answers.
  const first = Promise.withResolvers<never>();
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementationOnce(() => first.promise);
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const route = { key: KEY, kind: "issue" } as const;
  // The page's body as the stage draws it, around the hook the case reads the tree's state by.
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={issue} route={route}>
              <BodySlot issue={issue} route={route} />
              {children}
            </ProvidersSlot>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper });
    await waitFor(() => expect(api.getIssueTree).toHaveBeenCalledTimes(1));
    expect(failedTreeLine(), "no failed line while the read is out").toBeNull();

    await act(async () => {
      first.reject(new Error("the tree read was refused"));
      await first.promise.catch(() => undefined);
    });
    const line = await waitFor(() => {
      const found = failedTreeLine();
      if (found === null) throw new Error("the body drew no line for the refused tree read");
      return found;
    });
    const retry = [...line.querySelectorAll("button")].find((one) => one.textContent === "Retry");
    expect(retry, "the line holds a Retry").toBeDefined();

    await act(async () => retry?.click());
    await waitFor(() => expect(api.getIssueTree).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(view.result.current.blocksHeld).toBe(true));
    expect(failedTreeLine(), "no failed line once the read holds data").toBeNull();
    view.unmount();
  } finally {
    first.reject(new Error("the case ended"));
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// One event of another person's write lands while the load's first tree read is still out: the
// event names the tree, and the tree is read once more when the first read settles - two tree
// reads in all, the first and its one follow-up, and never a third.
test("an event while the first tree read is out reads the tree twice in all", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const firstOut = Promise.withResolvers<void>();
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads > 1) return treeAnswer([comment]);
    await firstOut.promise;
    return treeAnswer([]);
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(api.listComments).toHaveBeenCalledTimes(1));
    (api.listComments as Mock<typeof api.listComments>).mockResolvedValue([comment]);
    act(() => {
      refreshQueries(client, commentEventTargets());
    });
    await act(async () => {
      await Bun.sleep(50);
      firstOut.resolve();
      await Bun.sleep(2_500);
    });
    expect(holdsComment(view.result.current)).toBe(true);
    expect(treeReadsOf(), "the first tree read and one follow-up").toBe(2);
    view.unmount();
  } finally {
    firstOut.resolve();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// Pressing a view off draws the outline again, and the hook is mounted again on the reads it
// already holds: it builds no tree for them a second time, but answers the tree it built, the
// same object; a read that answers anew builds a tree of its own.
test("the outline mounted again on the same reads builds no tree, and a changed read builds one", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const first = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(api.getReferenceTargets).toHaveBeenCalledTimes(1));
    await act(async () => {
      await Bun.sleep(20);
    });
    // The root carries the held views beside the built tree, so the tree is its rows.
    const drawn = first.result.current.children;
    first.unmount();

    const again = renderHook(() => useIssueTree(issue).root, { wrapper });
    expect(again.result.current.children, "the tree built for these reads").toBe(drawn);

    act(() => {
      client.setQueryData(["comments", KEY], [comment]);
    });
    await waitFor(() => expect(again.result.current.children).not.toBe(drawn));
    again.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// The load's first comments read fails, as on a dropped connection, and the reader then replies.
// The reply is a write of the issue, so the tree is read again as it lands, and the reply is a
// row of the outline.
test("a reply after the load's comments read failed reads the tree again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let written: Comment[] = [];
  let commentsFail = true;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeAnswer(written)
  );
  (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => {
    if (commentsFail) throw new Error("the comments read failed");
    return [...written];
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    await waitFor(() => expect(client.getQueryState(["comments", KEY])?.status).toBe("error"));

    commentsFail = false;
    written = [comment];
    await landWrite(client);
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["comments", KEY] });
    });
    expect(client.getQueryState(["comments", KEY])?.status).toBe("success");
    await waitFor(
      () =>
        expect(
          nodesInOrder(view.result.current).some((node) => node.id === comment.id),
          "the reply is a row"
        ).toBe(true),
      { timeout: 3_000 }
    );
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A keyboard reader presses Retry on the line of a refused tree read. While the read runs again
// the focus stays on the line's Retry, or on what takes its place, and never falls to the page's
// body, from which the reader has to find the way back to the outline.
test("a Retry of a refused tree read keeps the focus off the page's body while the read runs again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const again = Promise.withResolvers<TreeAnswer>();
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads === 1) throw new Error("the tree read was refused");
    return again.promise;
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const route = { key: KEY, kind: "issue" } as const;
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={issue} route={route}>
              <BodySlot issue={issue} route={route} />
              {children}
            </ProvidersSlot>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper });
    const retry = await waitFor(() => {
      const found = [...(failedTreeLine()?.querySelectorAll("button") ?? [])].find(
        (one) => one.textContent === "Retry"
      );
      if (found === undefined) throw new Error("the body drew no Retry for the refused tree read");
      return found;
    });
    act(() => retry.focus());
    expect(document.activeElement).toBe(retry);

    await act(async () => retry.click());
    await waitFor(() => expect(reads).toBe(2));
    await act(async () => {
      await Bun.sleep(20);
    });
    const focused = document.activeElement;
    expect(
      focused !== null && focused !== document.body && focused.isConnected,
      "the focus stays off the page's body while the read runs again"
    ).toBe(true);
    view.unmount();
  } finally {
    again.resolve(treeAnswer([]));
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

/** The page's body as the stage draws it, around the hook a case reads the tree's state by, at
 *  the router's first address or at `path` where a case gives one. */
function bodyWrapper(client: QueryClient, path?: string) {
  const route = { key: KEY, kind: "issue" } as const;
  return ({ children }: { children: ReactNode }) => (
    <MemoryRouter {...(path === undefined ? {} : { initialEntries: [path] })}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={issue} route={route}>
              <BodySlot issue={issue} route={route} />
              {children}
            </ProvidersSlot>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The Retry of the line the body draws for a refused tree read, once it is drawn. */
function failedTreeRetry(): Promise<HTMLButtonElement> {
  return waitFor(() => {
    const found = [...(failedTreeLine()?.querySelectorAll("button") ?? [])].find(
      (one) => one.textContent === "Retry"
    );
    if (found === undefined) throw new Error("the body drew no Retry for the refused tree read");
    return found;
  });
}

/** A keyboard reader's press of `control`, which holds the focus: the keydown of Enter, then the
 *  click the browser makes of it. */
async function pressByKeyboard(control: HTMLElement): Promise<void> {
  await act(async () => {
    control.dispatchEvent(new KeyboardEvent("keydown", { bubbles: true, key: "Enter" }));
    control.click();
  });
}

// A reader who sees nothing happen presses Retry again while the read the first press asked for
// is out. The second press does nothing: the read out is not given up, no other is sent, and
// the first one's answer is what the page draws.
test("a second press of a Retry while its tree read is out neither gives that read up nor sends another", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const again = Promise.withResolvers<TreeAnswer>();
  const signals: (AbortSignal | undefined)[] = [];
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(
    async (_key, _query, init) => {
      signals.push(init?.signal);
      if (signals.length === 1) throw new Error("the tree read was refused");
      return again.promise;
    }
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: bodyWrapper(client) });
    const retry = await failedTreeRetry();
    act(() => retry.focus());
    await pressByKeyboard(retry);
    await waitFor(() => expect(signals).toHaveLength(2));
    expect(
      { drawn: retry.isConnected, enabled: !retry.disabled },
      "the Retry is drawn and enabled while its read is out"
    ).toEqual({ drawn: true, enabled: true });

    await pressByKeyboard(retry);
    await act(async () => {
      await Bun.sleep(50);
    });
    expect({
      givenUp: signals.filter((signal) => signal?.aborted === true).length,
      reads: signals.length,
    }).toEqual({ givenUp: 0, reads: 2 });

    await act(async () => {
      again.resolve(treeAnswer([]));
    });
    await waitFor(() => expect(view.result.current.blocksHeld).toBe(true));
    expect(failedTreeLine(), "no failed line once the read holds data").toBeNull();
    view.unmount();
  } finally {
    again.resolve(treeAnswer([]));
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A keyboard reader presses Retry and the read answers: the line goes with the focus in it, and
// the focus goes to the outline, on the row that holds the tree's tab stop, never to the page's
// body, from which the reader has to find the way back to the outline.
test("a Retry whose tree read answers hands the focus to the outline's tab-stop row", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const again = Promise.withResolvers<TreeAnswer>();
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads === 1) throw new Error("the tree read was refused");
    return again.promise;
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: bodyWrapper(client) });
    const retry = await failedTreeRetry();
    act(() => retry.focus());
    await pressByKeyboard(retry);
    await waitFor(() => expect(reads).toBe(2));

    await act(async () => {
      again.resolve(treeAnswer([comment]));
    });
    await waitFor(() => expect(view.result.current.blocksHeld).toBe(true));
    expect(failedTreeLine(), "no failed line once the read holds data").toBeNull();
    await act(async () => {
      await Bun.sleep(50);
    });
    const row = document.body.querySelector<HTMLElement>(
      '[role="tree"] [role="treeitem"][tabindex="0"]'
    );
    expect(row, "the outline draws its tab-stop row").not.toBeNull();
    expect(document.activeElement, "the focus is on the outline's tab-stop row").toBe(row);
    view.unmount();
  } finally {
    again.resolve(treeAnswer([]));
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A keyboard reader presses Retry and the read is refused again: the line stays as it was, the
// same Retry under the same focus, and that Retry reads the tree again when it is next pressed.
test("a Retry whose tree read is refused again leaves the line, its Retry and the focus where they were", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const again = Promise.withResolvers<TreeAnswer>();
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads === 1) throw new Error("the tree read was refused");
    return again.promise;
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: bodyWrapper(client) });
    const retry = await failedTreeRetry();
    const line = failedTreeLine();
    act(() => retry.focus());
    await pressByKeyboard(retry);
    await waitFor(() => expect(reads).toBe(2));

    await act(async () => {
      again.reject(new Error("the tree read was refused again"));
      await again.promise.catch(() => undefined);
    });
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error")
    );
    await act(async () => {
      await Bun.sleep(20);
    });
    expect(failedTreeLine(), "the line drawn before the press").toBe(line);
    expect(
      { drawn: retry.isConnected, enabled: !retry.disabled },
      "the Retry pressed, drawn and enabled"
    ).toEqual({ drawn: true, enabled: true });
    expect(document.activeElement, "the focus on the Retry pressed").toBe(retry);
    expect(reads, "no read but the Retry's").toBe(2);

    await pressByKeyboard(retry);
    await waitFor(() => expect(reads, "the next press reads the tree again").toBe(3));
    view.unmount();
  } finally {
    again.resolve(treeAnswer([]));
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// The tree read is refused with no answer held, and the reader is on the line's Retry. A read of
// the tree the reader did not ask for begins, as an event's refresh begins one through the page's
// scheduler: while it is out the line stays as it was, the same Retry, enabled, under the same
// focus, and a press of it sends no other read. Once the read answers the line goes.
test("a read the reader did not ask for keeps the refused read's line and its focused Retry while it is out", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const unasked = Promise.withResolvers<TreeAnswer>();
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads === 1) throw new Error("the tree read was refused");
    return unasked.promise;
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: bodyWrapper(client) });
    const retry = await failedTreeRetry();
    const line = failedTreeLine();
    act(() => retry.focus());

    act(() => {
      refreshQueries(client, [{ exact: true, queryKey: conversationTreeKey(KEY) }]);
    });
    await waitFor(() => expect(reads).toBe(2), { timeout: 3_000 });
    await act(async () => {
      await Bun.sleep(20);
    });
    expect(client.getQueryState(conversationTreeKey(KEY))?.fetchStatus, "the read is out").toBe(
      "fetching"
    );
    expect(failedTreeLine(), "the line drawn before the read began").toBe(line);
    expect(
      { drawn: retry.isConnected, enabled: !retry.disabled },
      "the Retry, drawn and enabled"
    ).toEqual({ drawn: true, enabled: true });
    expect(document.activeElement, "the focus on the Retry").toBe(retry);

    await pressByKeyboard(retry);
    await act(async () => {
      await Bun.sleep(50);
    });
    expect(reads, "a press while the read is out sends no other").toBe(2);

    await act(async () => {
      unasked.resolve(treeAnswer([]));
    });
    await waitFor(() => expect(view.result.current.blocksHeld).toBe(true));
    expect(failedTreeLine(), "no failed line once the read holds data").toBeNull();
    view.unmount();
  } finally {
    unasked.resolve(treeAnswer([]));
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// The page's stage mounts the tree's slot again on every
// press of a view, and under the app's `staleTime` a read is stale 30 s after it answered
// (`main.tsx`). The tree's reads are kept fresh by events instead: the slot mounted again more
// than 30 s on draws from what the client holds and sends no read, and a comment's edit reads the
// comments again and the names, whose read holds the comment's address another comment cites,
// and nothing else.
test("a tree mounted again past 30 s reads nothing, and a comment event reads the comments", async () => {
  const citing: Comment = {
    ...comment,
    body: `Read dispatch://${KEY}/comment/${comment.id} first.`,
    id: "comment-2",
  };
  const stored: Comment[] = [comment, citing];
  const stubs = stubTreeReads(async (refs) => namedTargets(refs), { comments: stored });
  const requests = () => stubs.reduce((sent, stub) => sent + stub.mock.calls.length, 0);
  // The app's client's `staleTime` (`main.tsx`), which a read that sets none of its own takes.
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  // The page's one subscription that reads the tree again when the comments change.
  const unsubscribe = subscribeTreeFreshness(client, KEY);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>{children}</RoutedQueries>
  );
  try {
    const first = renderHook(() => useIssueTree(issue).root, { wrapper });
    // Every read has answered, the names last of all.
    await waitFor(() => expect(taskWords(first.result.current)).toBe(TASK_WORDS));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    const loaded = requests();
    first.unmount();

    // A press of another view, more than 30 s after the load, mounts the slot again.
    setSystemTime(Date.now() + 31_000);
    const again = renderHook(() => useIssueTree(issue).root, { wrapper });
    expect(taskWords(again.result.current)).toBe(TASK_WORDS);
    // Long enough for a read the mount started to have been sent.
    await act(async () => {
      const waited = Promise.withResolvers<void>();
      setTimeout(waited.resolve, 50);
      await waited.promise;
    });
    expect(client.isFetching()).toBe(0);
    expect(requests(), "no read is sent for a slot mounted again").toBe(loaded);

    // The comment is edited, and the feed refreshes what that event names.
    const edited: Comment = {
      ...comment,
      body: "One more turn, reworded.",
      edited_at: "2026-11-05T02:00:00Z",
    };
    stored[0] = edited;
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockResolvedValue(treeAnswer([...stored]));
    // The page's `Comment` is the read's row and the feed's payload is the contract's, so the
    // event is cast as the feed's own tests cast theirs (`event-feed.test.ts`).
    const event = {
      actor: { id: "alice", kind: "user" },
      created_at: "2026-11-05T02:00:00Z",
      id: 2,
      issue_key: KEY,
      notify: true,
      payload: { ...edited, artifact_name: "" },
      project: "CORE",
      seq: 2,
      type: "comment.edited",
    } as DispatchEvent;
    await act(async () => {
      refreshQueries(client, eventRefreshTargets(event, "alice"));
    });
    // The tree draws the comment's new words, from the comments read the event sent.
    await waitFor(() => {
      const turn = nodesInOrder(again.result.current).find((node) => node.id === comment.id);
      expect(turn?.text).toBe(edited.body);
    });
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(api.listComments).toHaveBeenCalledTimes(2);
    expect(api.getReferenceTargets).toHaveBeenCalledTimes(2);
    // The event names the tree too, which is read again; it names no decisions read.
    expect(requests(), "the event sends the comments', tree's and names' reads").toBe(loaded + 3);
    again.unmount();
  } finally {
    unsubscribe();
    setSystemTime();
    for (const stub of stubs) stub.mockRestore();
  }
});

/** An event of the feed on the issue, with the payload its rules read; `owner` puts it on another
 *  owner. The page's rows are not the contract's payloads, so it is cast as the feed's own tests
 *  cast theirs (`event-feed.test.ts`). */
function feedEvent(
  type: EventType,
  payload: Record<string, unknown>,
  owner: Record<string, unknown> = {}
): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-11-05T02:00:00Z",
    id: 2,
    issue_key: KEY,
    notify: true,
    payload,
    project: "CORE",
    seq: 2,
    type,
    ...owner,
  } as unknown as DispatchEvent;
}

/** The address sets among `held` whose names read `event` refreshes: each set is cached under the
 *  key the page reads it by (`referenceTargetsQuery`), and the event's rules run over the cache as
 *  the feed runs them. */
function refreshedNames(event: DispatchEvent, held: readonly string[][]): string[][] {
  const client = new QueryClient();
  for (const refs of held) {
    client.setQueryData(referenceTargetsQuery(refs).queryKey, { targets: [] });
  }
  refreshQueries(client, eventRefreshTargets(event, "alice"));
  return held.filter(
    (refs) => client.getQueryState(referenceTargetsQuery(refs).queryKey)?.isInvalidated === true
  );
}

/** Names reads whose key names an address under the issue, each under another form of it. */
const NAMING_ISSUE = [[`dispatch://${KEY}/comment/comment-1`, "dispatch://ACME-2"], [TASK_REF]];

/** A names read whose key names the issue by its own address alone, as a child's page names its
 *  parent. The read answers that address with the issue's title and nothing else. */
const NAMING_KEY = [[`dispatch://${KEY}`]];

/** Names reads whose key names no address of the issue: another issue's, and a document of the
 *  issue's project, which is no address of the issue. */
const NAMING_ELSEWHERE = [
  ["dispatch://ACME-2", "dispatch://ACME-2/comment/comment-9"],
  ["dispatch://CORE/artifact/roadmap-md"],
];

// A names read is keyed by the addresses it names, so only words that change under an address
// already named need a rule - a comment's edit and delete, an issue message's edit and delete,
// an ask's edit and a document's new version - and so do a document's rename and its removal,
// which rename or empty every address to it, and an issue's write that changes its title, which
// renames the issue's own address. The feed is the workspace's, so each refreshes only the names
// reads whose key names an address it can change of the issue the event is on: an event on an
// issue no read names changes no name that read holds, and only a title change changes the name
// of the issue's own address, which the names read answers with the issue's title and nothing
// else. A child's page names its parent by that address, and the parent's documents take new
// versions often: none of those, nor any other change under the parent, reads the child's names
// again, where the parent's new title, and an older server's update that names none of its
// changes, does. A new comment is a new address, which the tree reads under a key of its own, a
// person's edit of a document is the notice after the versions that already refreshed them, and
// an issue's write that leaves its title alone and a follower joining an ask rename nothing.
test("an edit on an issue refreshes the names reads whose key names the issue and no other, a new comment none", () => {
  const editsWords = [
    feedEvent("comment.edited", { anchor: null, ask_id: null, id: "comment-1" }),
    feedEvent("comment.deleted", { anchor: null, ask_id: null, id: "comment-1" }),
    feedEvent("message.edited", { id: "message-1", in_reply_to: null }),
    feedEvent("message.deleted", { id: "message-1", in_reply_to: null }),
    feedEvent("ask.edited", { anchor: null, id: "ask-1" }),
    feedEvent("artifact.version", { artifact_id: plan.id, name: "Plan" }),
    feedEvent("artifact.renamed", { artifact: plan }),
    feedEvent("artifact.removed", { artifact: plan }),
  ];
  for (const event of editsWords) {
    expect(
      refreshedNames(event, [...NAMING_KEY, ...NAMING_ISSUE, ...NAMING_ELSEWHERE]),
      event.type
    ).toEqual(NAMING_ISSUE);
  }
  for (const payload of [{ changed: ["title"] }, {}]) {
    expect(
      refreshedNames(feedEvent("issue.updated", payload), [
        ...NAMING_KEY,
        ...NAMING_ISSUE,
        ...NAMING_ELSEWHERE,
      ]),
      JSON.stringify(payload)
    ).toEqual(NAMING_KEY);
  }
  const editsNoWords = [
    feedEvent("comment.created", { anchor: null, ask_id: null, id: "comment-2" }),
    feedEvent("artifact.human_edited", { artifact_id: plan.id, from_version: 1, to_version: 2 }),
    feedEvent("issue.updated", { changed: ["status"] }),
    feedEvent("ask.follower_added", { ask_id: "ask-1", session_id: "s1" }),
  ];
  for (const event of editsNoWords) {
    expect(refreshedNames(event, [...NAMING_KEY, ...NAMING_ISSUE]), event.type).toEqual([]);
  }
});

// An issue's address ends at its key: the letters `dispatch://CORE-1` begin every address of
// CORE-12 too, and none of them is an address of CORE-1.
test("an event on CORE-1 leaves a names read naming only CORE-12 alone", () => {
  const other = [
    "dispatch://CORE-12",
    "dispatch://CORE-12/comment/comment-9",
    "dispatch://CORE-12/spec#b-s1",
  ];
  const own = [`dispatch://${KEY}`, `dispatch://${KEY}/spec#b-s1`];
  for (const event of [
    feedEvent("comment.edited", { anchor: null, ask_id: null, id: "comment-1" }),
    feedEvent("issue.updated", { changed: ["title"] }),
  ]) {
    expect(refreshedNames(event, [other, own]), event.type).toEqual([own]);
  }
});

const roadmap = issueDocument("artifact-roadmap", "roadmap-md", {
  issue_key: null,
  name: "Roadmap",
});

// A project document's event is on the document and what it owns, each written under
// `dispatch://CORE/artifact/roadmap-md`. A rename, a removal, a comment's edit and delete, the
// edit of an ask its anchor puts on the document, or of a block ask through the ask's own route,
// and an approval request's rewording carry the slug, so they leave a read naming another
// document of the project alone too, `roadmap-md-2` among them; a new version and the edit of a
// question asked of the document as a whole name the document by its id alone, as a version's
// rewording of a block ask does, so they are held to the project's documents, which leaves
// another project's documents and every issue alone.
test("a project document's event refreshes the names reads naming that document, and none naming another", () => {
  const onDocument = { artifact_id: roadmap.id, issue_key: null };
  const naming = [
    ["dispatch://CORE/artifact/roadmap-md"],
    ["dispatch://CORE/artifact/roadmap-md@v2"],
    ["dispatch://CORE/artifact/roadmap-md#b-r1"],
    ["dispatch://ACME-2", "dispatch://CORE/artifact/roadmap-md/comment/comment-1"],
  ];
  const otherOwners = [["dispatch://ACME/artifact/roadmap-md"], [`dispatch://${KEY}`, TASK_REF]];
  const sameProject = [
    ["dispatch://CORE/artifact/roadmap-md-2"],
    ["dispatch://CORE/artifact/notes-md#b-n1"],
  ];
  const namingSlug = [
    feedEvent("artifact.renamed", { artifact: roadmap, previous_name: "Plans" }, onDocument),
    feedEvent("artifact.removed", { artifact: roadmap }, onDocument),
    feedEvent(
      "comment.edited",
      {
        anchor: null,
        artifact_slug: roadmap.slug,
        ask_id: null,
        id: "comment-1",
        project_key: "CORE",
      },
      onDocument
    ),
    feedEvent(
      "comment.deleted",
      {
        anchor: null,
        artifact_slug: roadmap.slug,
        ask_id: null,
        deleted_at: "2026-11-05T03:00:00Z",
        deleted_by: { id: "alice", kind: "user" },
        id: "comment-1",
        project_key: "CORE",
      },
      onDocument
    ),
    feedEvent(
      "ask.edited",
      {
        anchor: { artifact_id: roadmap.id, mark_id: "ask-2", quote: "Ship it." },
        anchor_artifact: {
          name: roadmap.name,
          primary: false,
          project: "CORE",
          slug: roadmap.slug,
        },
        artifact_id: roadmap.id,
        id: "ask-2",
      },
      onDocument
    ),
    feedEvent(
      "ask.edited",
      {
        anchor: null,
        artifact_id: roadmap.id,
        block_artifact: { id: roadmap.id, primary: false, slug: roadmap.slug },
        block_id: "q1",
        id: "ask-3",
      },
      onDocument
    ),
    feedEvent(
      "ask.edited",
      {
        anchor: null,
        approval: {
          artifact_id: roadmap.id,
          documents: [
            { artifact_id: roadmap.id, name: roadmap.name, requested_version: 1, version: 2 },
          ],
          name: roadmap.name,
          requested_version: 1,
          version: 2,
        },
        approval_artifact: {
          name: roadmap.name,
          primary: false,
          project: "CORE",
          slug: roadmap.slug,
        },
        artifact_id: roadmap.id,
        id: "ask-4",
        kind: "approval",
      },
      onDocument
    ),
  ];
  for (const event of namingSlug) {
    expect(refreshedNames(event, [...naming, ...otherOwners, ...sameProject]), event.type).toEqual(
      naming
    );
  }
  const namingId = [
    feedEvent("artifact.version", { artifact_id: roadmap.id, name: roadmap.name }, onDocument),
    feedEvent("ask.edited", { anchor: null, artifact_id: roadmap.id, id: "ask-1" }, onDocument),
  ];
  for (const event of namingId) {
    expect(refreshedNames(event, [...naming, ...otherOwners]), event.type).toEqual(naming);
  }
});

// A page can cite a document before it exists: the names read answers that address with nothing
// until the document is created, and the creation is what gives the address words. So a
// document's creation refreshes the names reads that cite it, of an issue and of a project, and
// leaves a read naming another owner alone. Its payload carries the document's slug, so a project
// document's creation leaves a read naming another document of the project alone too,
// `roadmap-md-2` among them.
test("an artifact.created event refreshes the names read that cites that document", () => {
  const citesPlan = [[`dispatch://${KEY}/artifact/${PLAN}`]];
  expect(
    refreshedNames(feedEvent("artifact.created", { artifact: plan }), [
      ...citesPlan,
      ...NAMING_ELSEWHERE,
    ])
  ).toEqual(citesPlan);
  const citesRoadmap = [["dispatch://CORE/artifact/roadmap-md"]];
  expect(
    refreshedNames(
      feedEvent(
        "artifact.created",
        { artifact: roadmap },
        { artifact_id: roadmap.id, issue_key: null }
      ),
      [
        ...citesRoadmap,
        ["dispatch://ACME/artifact/roadmap-md"],
        ["dispatch://CORE/artifact/roadmap-md-2"],
        [`dispatch://${KEY}`],
      ]
    )
  ).toEqual(citesRoadmap);
});

// The same rule on the page: every read of the tree is kept fresh by events, so a comment a
// node's words cite, edited under the same address, is named anew only by the feed's rule for
// the edit. The edit reads the names once more, and the page publishes the comment's new words
// under its address, which every link beside the tree draws.
test("a cited comment's edit reads the names once more, and the page publishes its new words", async () => {
  const cited: Comment = { ...comment, body: "Hold the deploy.", id: "comment-2" };
  const address = `dispatch://${KEY}/comment/${cited.id}`;
  const stored: Comment[] = [{ ...comment, body: `Read ${address} first.` }, cited];
  const asked: string[][] = [];
  const stubs = stubTreeReads(
    async (refs) => {
      asked.push([...refs]);
      return {
        targets: namedTargets(refs).targets.map((target) =>
          target.ref === address
            ? { ...target, kind: "comment", name: stored[1]?.body ?? "" }
            : target
        ),
      };
    },
    { comments: stored }
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </RoutedQueries>
  );
  try {
    const view = renderHook(
      () => {
        useIssueTree(issue);
        return useKnownReferenceName(address);
      },
      { wrapper }
    );
    await waitFor(() => expect(view.result.current).toBe("Hold the deploy."));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    const named = asked.length;

    // The comment is edited: the stubbed reads answer its new words from now on.
    const edited: Comment = {
      ...cited,
      body: "Hold the deploy until Friday.",
      edited_at: "2026-11-05T02:00:00Z",
    };
    stored[1] = edited;
    // A second on, so the scheduler's interval between two fetches of one read
    // (`MIN_REFETCH_INTERVAL_MS`) holds back none of the reads the event sends.
    setSystemTime(Date.now() + MIN_REFETCH_INTERVAL_MS);
    const event = feedEvent("comment.edited", { ...edited, artifact_name: "" });
    await act(async () => {
      refreshQueries(client, eventRefreshTargets(event, "alice"));
    });
    await waitFor(() => expect(view.result.current).toBe("Hold the deploy until Friday."));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(asked.length, "the edit sends one names read").toBe(named + 1);
    expect(asked.at(-1)).toContain(address);
    view.unmount();
  } finally {
    setSystemTime();
    for (const stub of stubs) stub.mockRestore();
  }
});

/** The reader comes back to the page's window: the focus TanStack hears from a browser's
 *  `visibilitychange`. */
function focusWindow(): void {
  focusManager.setFocused(false);
  focusManager.setFocused(true);
}

/** The live feed of `client` up, as the shell's event feed holds it: its socket open, and the
 *  feed's subscription on it started by Dispatch (`events_start`). Answers the socket, which a
 *  case sends Dispatch's heartbeat on, and the end of the feed, which puts the page's WebSocket
 *  back. */
function liveFeedOn(client: QueryClient) {
  const sockets = installFakeLiveSockets();
  const live = liveSocketFor(client);
  live.subscribeEvents({ onEvent: () => undefined, onStart: () => undefined, since: () => null });
  startFeedOn(sockets, true);
  return {
    end: () => {
      live.close();
      sockets.restore();
    },
    socket: sockets.latest(),
  };
}

// The page's read of the issue is read again on a window focus once it is old, and Dispatch
// answers each read as its records stand then, the claim holder's liveness and its route's reach
// among them, which move with no act of anyone. With the live feed down a focus reads no answered
// tree nor view, whatever that issue read answers: what changes the tree comes as an event or a
// write of the issue.
test("with the live feed down a window focus reads no answered tree nor open view, whatever the issue read answers", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const specView = viewAnswer({ ...task, role: "query" });
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) =>
    query?.view === "spec" ? specView : treeAnswer([])
  );
  let answer: IssueDetails = issue;
  const getIssue = spyOn(api, "getIssue").mockImplementation(async () => answer);
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(
      () => {
        // The page's read of the issue, old at once here where the app's is after 30 s.
        useQuery({ ...issueQuery(KEY), staleTime: 0 });
        return useIssueTree(issue).root;
      },
      { wrapper: stageWrapper(client, "spec") }
    );
    await waitFor(() => expect(view.result.current.views?.spec).toBe(specView));
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const tree = treeReadsOf();
    const spec = treeReadsOf("spec");
    const issueReads = getIssue.mock.calls.length;

    answer = {
      ...issue,
      holder_state: "unresponsive",
      last_seen: "2026-11-05T01:00:00Z",
      route_holder: "session-1",
      route_status: "live",
      takeable_at: "2026-11-05T01:05:00Z",
    };
    await act(async () => {
      focusWindow();
      await Bun.sleep(1_200);
    });
    expect(getIssue.mock.calls.length, "the focus reads the issue again").toBe(issueReads + 1);
    expect({ spec: treeReadsOf("spec") - spec, tree: treeReadsOf() - tree }).toEqual({
      spec: 0,
      tree: 0,
    });

    answer = { ...answer, status: "in_progress" };
    await act(async () => {
      focusWindow();
      await Bun.sleep(1_500);
    });
    expect(getIssue.mock.calls.length).toBe(issueReads + 2);
    expect({ spec: treeReadsOf("spec") - spec, tree: treeReadsOf() - tree }).toEqual({
      spec: 0,
      tree: 0,
    });
    view.unmount();
  } finally {
    focusManager.setFocused(undefined);
    getIssue.mockRestore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// While the live feed is up, its events say what changed on the issue, and each refreshes the
// tree: a window focus reads neither the tree nor the open view, even where the page's read of
// the issue, read again on that focus, answers a field someone's act moved.
test("while the live feed is up a window focus reads neither the tree nor the open view, whatever the issue read answers", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const specView = viewAnswer({ ...task, role: "query" });
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) =>
    query?.view === "spec" ? specView : treeAnswer([])
  );
  let answer: IssueDetails = issue;
  const getIssue = spyOn(api, "getIssue").mockImplementation(async () => answer);
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const feed = liveFeedOn(client);
  try {
    const view = renderHook(
      () => {
        useQuery({ ...issueQuery(KEY), staleTime: 0 });
        return useIssueTree(issue).root;
      },
      { wrapper: stageWrapper(client, "spec") }
    );
    await waitFor(() => expect(view.result.current.views?.spec).toBe(specView));
    await act(async () => {
      await Bun.sleep(1_100);
      await client.invalidateQueries({ queryKey: ["comments", KEY] });
      await Bun.sleep(1_100);
    });
    const tree = treeReadsOf();
    const spec = treeReadsOf("spec");
    const issueReads = getIssue.mock.calls.length;

    answer = { ...issue, status: "in_progress" };
    await act(async () => {
      focusWindow();
      await Bun.sleep(1_500);
    });
    expect(getIssue.mock.calls.length, "the focus reads the issue again").toBe(issueReads + 1);
    expect({ spec: treeReadsOf("spec") - spec, tree: treeReadsOf() - tree }).toEqual({
      spec: 0,
      tree: 0,
    });
    view.unmount();
  } finally {
    focusManager.setFocused(undefined);
    feed.end();
    getIssue.mockRestore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A tree read refused, as by a Dispatch that has no route for it yet, is retried by the app's
// rule and then stays refused while the live feed is up: the window's focus coming and going
// for a minute, and the page's read of the issue read again on it with the holder's heartbeat
// moved, read it no more, and the failed line stays drawn, since no new read puts the query back
// to pending. Once the feed is down, a focus reads it again.
test("a refused tree read, its retries spent, is read no more for a minute of focus while the live feed is up", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: NO_ROUTE_ROLLING_RELEASE_MESSAGE })
  );
  let issueReads = 0;
  const getIssue = spyOn(api, "getIssue").mockImplementation(async () => {
    issueReads += 1;
    return { ...issue, last_seen: `2026-11-05T01:00:${String(issueReads).padStart(2, "0")}Z` };
  });
  // The app's client (`main.tsx`): its retries, their waits, and the age past which a read is
  // read again on a focus.
  const client = new DispatchQueryClient({
    defaultOptions: {
      queries: { retry: queryRetry, retryDelay: queryRetryDelay, staleTime: 30_000 },
    },
  });
  jest.useFakeTimers();
  const feed = liveFeedOn(client);
  try {
    const view = renderHook(
      () => {
        useQuery(issueQuery(KEY));
        return useIssueTree(issue);
      },
      { wrapper: stageWrapper(client) }
    );
    await tick(2_000, 250);
    expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error");
    expect(treeReadsOf(), "the load's read and its two retries").toBe(3);

    let pendingAt = 0;
    for (let second = 0; second < 60; second += 3) {
      act(() => {
        feed.socket.receive({ type: "heartbeat" });
        focusWindow();
      });
      for (let step = 0; step < 12; step += 1) {
        await tick(250, 250);
        if (client.getQueryState(conversationTreeKey(KEY))?.status !== "error") pendingAt += 1;
      }
    }
    expect(issueReads, "the issue read is read again on a focus once old").toBeGreaterThan(1);
    expect(treeReadsOf(), "no tree read after the retries").toBe(3);
    expect(pendingAt, "the read stays refused, so the failed line stays drawn").toBe(0);

    act(() => {
      feed.end();
      focusWindow();
    });
    await tick(2_000, 250);
    expect(treeReadsOf(), "a focus with the feed down reads it again, with its retries").toBe(6);
    view.unmount();
  } finally {
    focusManager.setFocused(undefined);
    realClock();
    feed.end();
    getIssue.mockRestore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// After a gap in the live feed the page refreshes every query it holds, and a reply landed in
// the gap: the comments read and the tree read the refresh sends begin together, so the tree
// read answers the reply, and the comments read's answer of it reads the tree no more.
test("a refresh of every query after a gap that held a reply reads the tree at most twice and draws the reply", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let written: Comment[] = [];
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) => {
    await Bun.sleep(20);
    return query?.view === "spec" ? viewAnswer({ ...task, role: "query" }) : treeAnswer(written);
  });
  (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => [...written]);
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, {
      wrapper: stageWrapper(client, "spec"),
    });
    await waitFor(() => expect(view.result.current.views?.spec).toBeDefined());
    await act(async () => {
      await Bun.sleep(1_100);
    });

    written = [comment];
    act(() => {
      refreshQueries(client);
    });
    await act(async () => {
      await Bun.sleep(2_500);
    });
    expect(holdsComment(view.result.current), "the reply is a row").toBe(true);
    expect(treeReadsOf(), "the load's tree read and the refresh's").toBeLessThanOrEqual(2);
    expect(treeReadsOf("spec"), "the load's view read and the refresh's").toBeLessThanOrEqual(2);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A reply lands while the reader is away, and its event marks the comments and the tree stale.
// Coming back, the page reads both again together: the tree read answers the reply, and the
// tree is read no more.
test("coming back to the issue after an event reads the tree at most twice and draws what changed", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let written: Comment[] = [];
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeAnswer(written)
  );
  (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => [...written]);
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = stageWrapper(client);
  try {
    const first = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(taskWords(first.result.current)).toBe(TASK_WORDS));
    first.unmount();

    written = [comment];
    act(() => {
      refreshQueries(client, commentEventTargets());
    });
    await act(async () => {
      await Bun.sleep(1_200);
    });

    const back = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(holdsComment(back.result.current), "the reply is a row").toBe(true));
    await act(async () => {
      await Bun.sleep(2_500);
    });
    expect(treeReadsOf(), "the load's tree read and the one coming back").toBeLessThanOrEqual(2);
    back.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A write and the event it raises: the write's settle reads the tree, its views and the decisions
// read, and the event, a moment later, reads the tree once more, a second after that read began.
// The decisions read answers anew on every read here, which reads the tree no more: the tree is
// read twice for the write and its event, then no more.
test("a write and its event read the tree twice and no more, whatever the decisions read answers", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let written: Comment[] = [];
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeAnswer(written)
  );
  (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => [...written]);
  let decisionReads = 0;
  (api.getIssueDecisions as Mock<typeof api.getIssueDecisions>).mockImplementation(async () => {
    decisionReads += 1;
    return {
      decisions: [],
      document: `decisions read ${decisionReads}`,
      entries: [],
      open_questions: [],
    };
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const before = treeReadsOf();
    const decisionsBefore = decisionReads;

    written = [comment];
    await landWrite(client);
    await act(async () => {
      await Bun.sleep(50);
      refreshQueries(client, commentEventTargets());
      await Bun.sleep(2_500);
    });
    expect(decisionReads - decisionsBefore, "the write's decisions read answered").toBe(1);
    const sent = treeReadsOf() - before;
    await act(async () => {
      await Bun.sleep(1_500);
    });
    expect({ later: treeReadsOf() - before, sent }).toEqual({ later: 2, sent: 2 });
    expect(holdsComment(view.result.current)).toBe(true);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A write - a document's new version, say - that changes what the tree answers lands while a
// read of the issue is invalid already - here the coverage, marked invalid as the scheduler
// marks a refresh it owes - and its callback refreshes the coverage, which answers what it held.
// The write itself reads the tree as it settles, once, whatever the coverage answers.
test("a write that lands while the coverage read is invalid already reads the tree once", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let written: Comment[] = [];
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeAnswer(written)
  );
  const coverageKey = issueCoverageQuery(KEY).queryKey;
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    // The coverage marked invalid as an owed refresh marks it (`owe`).
    await act(async () => {
      await Bun.sleep(1_100);
      client.getQueryCache().find({ exact: true, queryKey: coverageKey })?.invalidate();
      await Bun.sleep(1_100);
    });
    expect(client.getQueryState(coverageKey)?.isInvalidated).toBe(true);
    const before = treeReadsOf();
    const coverageReads = (api.getIssueCoverage as Mock<typeof api.getIssueCoverage>).mock.calls
      .length;

    written = [comment];
    await landWrite(client, () => {
      void client.invalidateQueries({ exact: true, queryKey: coverageKey });
    });
    await waitFor(() =>
      expect(api.getIssueCoverage, "the write reads the coverage again").toHaveBeenCalledTimes(
        coverageReads + 1
      )
    );
    await waitFor(
      () => expect(holdsComment(view.result.current), "the write is drawn").toBe(true),
      {
        timeout: 3_000,
      }
    );
    await act(async () => {
      await Bun.sleep(1_500);
    });
    expect(treeReadsOf() - before, "one tree read for the write").toBe(1);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A tree read refused, its retries spent, stays refused while the live feed is up, until the
// reader's Retry, an event of the issue or the feed coming back. A surface that starts reading the
// tree afterwards - a filter pressed, which draws its slot, or any slot drawn again - is none of
// those, so it reads nothing of its own, and the read stays refused, so the failed line stays.
test("a surface that starts reading a refused tree read sends no read while the live feed is up", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: NO_ROUTE_ROLLING_RELEASE_MESSAGE })
  );
  // The app's client (`main.tsx`): its retries and their waits.
  const client = new DispatchQueryClient({
    defaultOptions: { queries: { retry: queryRetry, retryDelay: queryRetryDelay } },
  });
  jest.useFakeTimers();
  const feed = liveFeedOn(client);
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: stageWrapper(client) });
    await tick(2_000, 250);
    expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error");
    expect(treeReadsOf(), "the load's read and its two retries").toBe(3);

    const later = renderHook(() => useIssueTree(issue), {
      wrapper: ({ children }: { children: ReactNode }) => (
        <RoutedQueries client={client}>{children}</RoutedQueries>
      ),
    });
    let pendingAt = 0;
    for (let step = 0; step < 12; step += 1) {
      await tick(250, 250);
      if (client.getQueryState(conversationTreeKey(KEY))?.status !== "error") pendingAt += 1;
    }
    expect(treeReadsOf(), "a surface that starts reading the tree reads nothing of its own").toBe(
      3
    );
    expect(pendingAt, "the read stays refused, so the failed line stays drawn").toBe(0);
    later.unmount();
    view.unmount();
  } finally {
    realClock();
    feed.end();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A keyboard reader presses Retry on the line of a refused tree read, and the read answers an
// issue that holds nothing yet: the line goes, and the outline draws its line for an empty issue
// and no row, so there is no tab-stop row to hand the focus to. The focus still goes somewhere
// the reader can act from, never to the page's body.
test("a Retry whose tree read answers an issue that holds nothing keeps the focus off the page's body", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const empty: IssueDetails = { ...issue, artifacts: [], plan_artifact_id: "" };
  const nothing: TreeAnswer = {
    ...treeAnswer([]),
    nodes: [answerNode(KEY, "issue", { parent: null })],
  };
  const again = Promise.withResolvers<TreeAnswer>();
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads === 1) throw new Error("the tree read was refused");
    return again.promise;
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const route = { key: KEY, kind: "issue" } as const;
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={empty} route={route}>
              <BodySlot issue={empty} route={route} />
              {children}
            </ProvidersSlot>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const view = renderHook(() => useIssueTree(empty), { wrapper });
    const retry = await failedTreeRetry();
    act(() => retry.focus());
    await pressByKeyboard(retry);
    await waitFor(() => expect(reads).toBe(2));

    await act(async () => {
      again.resolve(nothing);
    });
    await waitFor(() => expect(view.result.current.blocksHeld).toBe(true));
    expect(failedTreeLine(), "no failed line once the read holds data").toBeNull();
    await act(async () => {
      await Bun.sleep(100);
    });
    const focused = document.activeElement;
    expect(
      focused !== null && focused !== document.body && focused.isConnected,
      "the focus stays off the page's body once the read answers an empty issue"
    ).toBe(true);
    view.unmount();
  } finally {
    again.resolve(nothing);
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// The reader's Keep lands with no answer of its own while an event's reads are under way: the
// write store refreshes every query as it lands, and the Keep is a write of the issue, whose
// settle reads the tree, so the tree draws what the server holds after the Keep.
test("a Keep that lands with no answer reads the tree again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let written: Comment[] = [];
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeAnswer(written)
  );
  let kept = false;
  (api.getIssueDecisions as Mock<typeof api.getIssueDecisions>).mockImplementation(async () => ({
    decisions: [],
    document: kept ? "kept" : null,
    entries: [],
    open_questions: [],
  }));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  // The app's event feed starts the scheduler's record of each read's start at once, so its
  // one-a-second rule holds from the load on.
  trackQueryFetches(client);
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(client.isFetching()).toBe(0));
    await act(async () => {
      await Bun.sleep(1_100);
    });
    // A comment event 200 ms on, whose reads the scheduler begins.
    await act(async () => {
      await Bun.sleep(200);
      refreshQueries(client, commentEventTargets());
      await Bun.sleep(150);
    });

    // The reader's Keep lands with no answer: Dispatch holds it, and its tree answers the reply.
    kept = true;
    written = [comment];
    await act(async () => {
      await storeWrite(client, KEY, async () => null).mutate();
    });
    await act(async () => {
      await Bun.sleep(2_500);
    });
    expect(holdsComment(view.result.current), "the tree read after the Keep is drawn").toBe(true);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A tree read refused, its retries spent, while the live feed is up, and then the browser says
// the network is back while the feed's socket never went: no event, no Retry and no feed coming
// back, so the tree is read no more and stays refused, and its failed line stays drawn.
test("the network said to be back while the live feed stays up reads a refused tree no more", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: NO_ROUTE_ROLLING_RELEASE_MESSAGE })
  );
  // The app's client (`main.tsx`): its retries and their waits.
  const client = new DispatchQueryClient({
    defaultOptions: { queries: { retry: queryRetry, retryDelay: queryRetryDelay } },
  });
  jest.useFakeTimers();
  const feed = liveFeedOn(client);
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: stageWrapper(client) });
    await tick(2_000, 250);
    expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error");
    expect(treeReadsOf(), "the load's read and its two retries").toBe(3);

    act(() => {
      onlineManager.setOnline(false);
      onlineManager.setOnline(true);
    });
    let pendingAt = 0;
    for (let step = 0; step < 12; step += 1) {
      await tick(250, 250);
      if (client.getQueryState(conversationTreeKey(KEY))?.status !== "error") pendingAt += 1;
    }
    expect(treeReadsOf(), "the network's word reads nothing").toBe(3);
    expect(pendingAt, "the read stays refused, so the failed line stays drawn").toBe(0);
    view.unmount();
  } finally {
    onlineManager.setOnline(true);
    realClock();
    feed.end();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A tree read refused, and the reader leaves the issue. Coming back with nothing changed reads
// the tree no more: a stage drawn again is no sign that the read would answer now. An event of
// the issue while the reader is away names the tree, which nothing draws then: it is marked
// stale, the stage reads it once the reader comes back, and the outline draws what it answers.
test("an event while the reader is away from a refused tree reads it once the reader comes back", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValue(
    new Error("the tree read was refused")
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = stageWrapper(client);
  try {
    const first = renderHook(() => useIssueTree(issue), { wrapper });
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error")
    );
    first.unmount();

    const visit = renderHook(() => useIssueTree(issue), { wrapper });
    await act(async () => {
      await Bun.sleep(1_200);
    });
    expect(treeReadsOf(), "a stage drawn again reads a refused tree no more").toBe(1);
    expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error");
    visit.unmount();

    // Away: a reply and its event, and Dispatch now answers the tree.
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockResolvedValue(treeAnswer([comment]));
    (api.listComments as Mock<typeof api.listComments>).mockResolvedValue([comment]);
    act(() => {
      refreshQueries(client, commentEventTargets());
    });
    await act(async () => {
      await Bun.sleep(50);
    });
    expect(treeReadsOf(), "no tree read while nothing draws the tree").toBe(1);

    const back = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(
      () => expect(holdsComment(back.result.current), "the reply is a row").toBe(true),
      {
        timeout: 3_000,
      }
    );
    expect(treeReadsOf(), "one read for the event").toBe(2);
    back.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A Retry's read answers an issue that holds nothing yet, so the outline's tree, which draws no
// row, takes the focus from the line that left. The tree holds it only while it has no row to
// give it to: once a read draws one - an event's, here - the focus goes on to the row that holds
// the tree's tab stop, and the tree is no stop of Tab beside its rows.
test("the outline's tree that took the focus for want of a row hands it to the first row drawn", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const empty: IssueDetails = { ...issue, artifacts: [], plan_artifact_id: "" };
  const only = (nodes: readonly TreeAnswerNode[]): TreeAnswer => ({
    ...treeAnswer([]),
    nodes: [answerNode(KEY, "issue", { parent: null }), ...nodes],
  });
  const reply = commentNode(comment, KEY);
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads === 1) throw new Error("the tree read was refused");
    return only(reads === 2 ? [] : [reply]);
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const route = { key: KEY, kind: "issue" } as const;
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={empty} route={route}>
              <BodySlot issue={empty} route={route} />
              {children}
            </ProvidersSlot>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const view = renderHook(() => useIssueTree(empty), { wrapper });
    const retry = await failedTreeRetry();
    act(() => retry.focus());
    await pressByKeyboard(retry);
    await waitFor(() => expect(view.result.current.blocksHeld).toBe(true));
    const tree = document.body.querySelector<HTMLElement>(
      `[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`
    );
    expect(tree, "the outline draws its tree").not.toBeNull();
    expect(document.activeElement, "the outline's tree holds the focus").toBe(tree);

    await act(async () => {
      await Bun.sleep(MIN_REFETCH_INTERVAL_MS + 100);
    });
    act(() => {
      refreshQueries(client, [{ exact: true, queryKey: conversationTreeKey(KEY) }]);
    });
    await waitFor(() => expect(holdsComment(view.result.current.root)).toBe(true));
    const row = await waitFor(() => {
      const found = tree?.querySelector<HTMLElement>('[role="treeitem"][tabindex="0"]');
      if (found == null) throw new Error("the outline drew no row for the reply");
      return found;
    });
    await waitFor(() =>
      expect(document.activeElement, "the row that holds the tab stop takes the focus").toBe(row)
    );
    expect(tree?.hasAttribute("tabindex"), "the tree is no stop of Tab beside its rows").toBe(
      false
    );
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// The reader leaves the issue. Away, an event of the decisions - a Keep's - names the tree too,
// which nothing draws then, so it is marked stale, and Dispatch's tree changes. Back on the
// issue, the tree is read again and drawn.
test("back on the issue, an event of the decisions while away reads the tree again", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let written: Comment[] = [];
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeAnswer(written)
  );
  let changed = false;
  (api.getIssueDecisions as Mock<typeof api.getIssueDecisions>).mockImplementation(async () => ({
    decisions: [],
    document: changed ? "changed" : null,
    entries: [],
    open_questions: [],
  }));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  trackQueryFetches(client);
  const wrapper = stageWrapper(client);
  try {
    const first = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(() => expect(client.isFetching()).toBe(0));
    first.unmount();
    await act(async () => {
      await Bun.sleep(1_200);
    });

    changed = true;
    written = [comment];
    act(() => {
      refreshQueries(client, keepEventTargets());
    });
    const back = renderHook(() => useIssueTree(issue).root, { wrapper });
    await waitFor(
      () => expect(holdsComment(back.result.current), "the tree read again is drawn").toBe(true),
      { timeout: 3_000 }
    );
    back.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

/** The tree as Dispatch answers it while it holds `held`, the issue's one comment: settled once
 *  it is resolved; and each of `others`, comments someone else posted, as `treeAnswer` answers
 *  a comment. */
function heldCommentTree(held: Comment, others: readonly Comment[] = []): TreeAnswer {
  return treeAnswer(others, {
    nodes: [
      answerNode(held.id, "comment", {
        author: [held.author],
        made_at: held.created_at,
        state: held.resolved ? "settled" : "live",
        text: held.body,
      }),
    ],
  });
}

/** The callbacks of a write of the page (`storeWrite`), as a surface's `useKeyedWrite` takes them:
 *  TanStack runs each after the write store has landed or refused the write. */
type StoreWriteCallbacks<TContext> = Pick<
  MutationObserverOptions<unknown, Error, void, TContext>,
  "onError" | "onMutate" | "onSettled" | "onSuccess"
>;

/** A write of the page made as the page's writes are (`useKeyedWrite`): a mutation whose function
 *  is a write of the app's write store (`Writes.run`), made by `attempt`, naming `issue`, the
 *  issue it changes, or none. The store tells that issue's tree read of it from its press to its
 *  landing or refusal (`onIssueWrite`); the mutation's callbacks run after. An attempt that tells
 *  its request as it goes out (`bounds.sent`) names the issue of its path instead. */
function storeWrite<TContext = unknown>(
  client: QueryClient,
  issue: string | undefined,
  attempt: (bounds: WriteBounds) => Promise<unknown>,
  callbacks: StoreWriteCallbacks<TContext> = {}
): MutationObserver<unknown, Error, void, TContext> {
  return new MutationObserver<unknown, Error, void, TContext>(client, {
    ...callbacks,
    mutationFn: () => pressWrite(client, issue, attempt),
    networkMode: "always",
  });
}

/** A press of a write of the app's write store (`Writes.run`) naming `issue`, or none, made by
 *  `attempt`, as the page's writes make theirs, and as a write made outside any mutation is. */
function pressWrite(
  client: QueryClient,
  issue: string | undefined,
  attempt: (bounds: WriteBounds) => Promise<unknown>,
  label = "A write of the page"
): Promise<unknown> {
  return writesOf(client).run({
    detail: null,
    inline: true,
    issue,
    kind: plainWrite,
    label,
    method: "POST",
    presentation: "indicator",
    write: attempt,
  });
}

/** A write of the issue the page sends - a reply, a Keep, a status pick - pressed and landed
 *  (`storeWrite`), in an awaited `act`; `answered` runs as the surface's callback does, once the
 *  write store has landed it. A case's client with no live feed reads the tree, its views and the
 *  decisions read again as the write settles (`subscribeTreeFreshness`). */
async function landWrite(client: QueryClient, answered?: () => void): Promise<void> {
  await act(async () => {
    await storeWrite(
      client,
      KEY,
      async () => ({ id: "write" }),
      answered === undefined ? {} : { onSuccess: answered }
    ).mutate();
  });
}

/** Another issue than the one the page draws: its writes hold none of this issue's reads. */
const OTHER_KEY = "CORE-2";

/** The margin's resolve of the comment (`useMarginItems.ts`) on `client`, which Dispatch takes in
 *  `write`: its `onMutate` writes the resolve into the comments read before Dispatch holds it, and
 *  its `onSuccess` invalidates the comments and the issue once Dispatch answers. */
function marginResolveOn(
  client: QueryClient,
  write: () => Promise<void>
): MutationObserver<unknown, Error, void, void> {
  return storeWrite(client, KEY, write, {
    onMutate: async () => {
      await client.cancelQueries({ queryKey: ["comments", KEY] });
      client.setQueryData<Comment[]>(["comments", KEY], (current) =>
        current?.map((one) => ({ ...one, resolved: true }))
      );
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["comments", KEY] });
      void client.invalidateQueries({ queryKey: ["issue", KEY] });
    },
  });
}

// The margin's resolve of a comment (`useMarginItems.ts`): its `onMutate` writes the resolve
// into the comments read before Dispatch holds it, its `onSuccess` invalidates the comments and
// the issue once Dispatch answers, and the resolve's event refreshes what a comment's event names
// 150 ms later, as the live feed's rule does. With the feed up the resolve's settle reads nothing
// of its own: its event reads the tree, once more where it lands while that read is out, and the
// tree read ends holding the resolve.
for (const latency of [200, 1_200]) {
  test(`a resolve from the margin answered in ${latency} ms reads the tree at most twice with its event, and ends holding it`, async () => {
    const stubs = stubTreeReads(async (refs) => namedTargets(refs));
    // What Dispatch holds of the comment: resolved once it takes the write.
    let held: Comment = comment;
    (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => [held]);
    (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
      heldCommentTree(held)
    );
    const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
    trackQueryFetches(client);
    const feed = liveFeedOn(client);
    try {
      const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
      await waitFor(() => expect(holdsComment(view.result.current)).toBe(true));
      await act(async () => {
        await Bun.sleep(1_100);
      });
      const before = treeReadsOf();

      const resolve = marginResolveOn(client, async () => {
        held = { ...comment, resolved: true };
        await Bun.sleep(latency);
      });
      await act(async () => {
        await resolve.mutate();
      });
      await act(async () => {
        await Bun.sleep(150);
        refreshQueries(client, commentEventTargets());
        await Bun.sleep(3_000);
      });
      const sent = treeReadsOf() - before;
      expect(sent, "the read for its event").toBeGreaterThanOrEqual(1);
      expect(sent, "and one more where that read was out at most").toBeLessThanOrEqual(2);
      expect(
        client
          .getQueryData<TreeAnswer>(conversationTreeKey(KEY))
          ?.nodes?.find((node) => node.id === comment.id)?.state,
        "the tree read holds the resolve"
      ).toBe("settled");
      view.unmount();
    } finally {
      feed.end();
      for (const stub of stubs) stub.mockRestore();
      client.clear();
    }
  });
}

// The same resolve with the live feed down, so no event comes: the resolve's settle reads the
// tree, as nothing else then tells of it. A tree read made while the write is out would read a
// tree Dispatch has not changed yet, so the tree is read once, after Dispatch answered, and the
// tree read ends holding the resolve.
test("a resolve from the margin with the live feed down reads the tree once, after Dispatch answers, and ends holding it", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  // What Dispatch holds of the comment: resolved once it takes the write.
  let held: Comment = comment;
  // Whether Dispatch has answered the resolve, as each tree read begins.
  let answered = false;
  const readsAnswered: boolean[] = [];
  (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => [held]);
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) => {
    if (query?.view === undefined) readsAnswered.push(answered);
    return heldCommentTree(held);
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  trackQueryFetches(client);
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(holdsComment(view.result.current)).toBe(true));
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const before = readsAnswered.length;

    const resolve = marginResolveOn(client, async () => {
      held = { ...comment, resolved: true };
      await Bun.sleep(200);
      answered = true;
    });
    await act(async () => {
      await resolve.mutate();
    });
    await act(async () => {
      await Bun.sleep(3_000);
    });
    expect(
      readsAnswered.slice(before),
      "the tree read once for the resolve, once Dispatch answered it"
    ).toEqual([true]);
    expect(
      client
        .getQueryData<TreeAnswer>(conversationTreeKey(KEY))
        ?.nodes?.find((node) => node.id === comment.id)?.state,
      "the tree read holds the resolve"
    ).toBe("settled");
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

/** The shell's event feed, as the app mounts it beside every page. */
function ShellEventFeed(): null {
  useEventFeed();
  return null;
}

// A tree read refused, its retries spent, is read again when the live feed comes back. The
// page's socket is restarted under it, and Dispatch starts the feed again on the next socket
// with every event of the gap replayed, none of them of this issue: that is still the feed
// coming back, so the refused read is asked again, or the outline stays on its failed line until
// the reader presses Retry.
test("a refused tree read is read again when the live feed comes back with its gap replayed whole", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: NO_ROUTE_ROLLING_RELEASE_MESSAGE })
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const sockets = installFakeLiveSockets();
  // Signed in at event head 1, as the app's sign-in gate holds it before the page.
  client.setQueryData(["whoami"], { event_head: "1", kind: "user", login: "alice" });
  const startFeed = (): void => {
    const socket = sockets.latest();
    act(() => socket.open());
    const { sub } = socket.subscribed("events");
    act(() => socket.receive({ type: "events_start", sub, since: "1", replay_complete: true }));
  };
  const wrapper = ({ children }: { children: ReactNode }) => (
    <RoutedQueries client={client}>
      <ShellEventFeed />
      <ViewReadProvider issue={issue} preset="conversation">
        {children}
      </ViewReadProvider>
    </RoutedQueries>
  );
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper });
    startFeed();
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error")
    );
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const before = treeReadsOf();

    act(() => sockets.latest().restart());
    await act(async () => {
      await Bun.sleep(200);
    });
    expect(sockets.sockets.length, "the page opened its next socket").toBe(2);
    startFeed();
    await act(async () => {
      await Bun.sleep(1_500);
    });
    expect(
      treeReadsOf() - before,
      "the feed coming back reads the refused tree again"
    ).toBeGreaterThanOrEqual(1);
    view.unmount();
  } finally {
    liveSocketFor(client).close();
    sockets.restore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A Retry answered with an issue that holds nothing puts the focus on the outline's tree, which
// is focusable only while it holds the focus. The reader then switches to another window and
// back: the browser blurs the tree as the window goes and focuses it again as it comes back,
// while the tree stays the page's focused element. That blur is no focus leaving the tree, so
// the tree stays focusable; a browser moves the focus to the body as soon as the focused element
// stops being focusable, which would leave the reader there on coming back.
test("a window switch while the outline's tree holds the focus leaves it there and focusable", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const empty: IssueDetails = { ...issue, artifacts: [], plan_artifact_id: "" };
  const nothing: TreeAnswer = {
    ...treeAnswer([]),
    nodes: [answerNode(KEY, "issue", { parent: null })],
  };
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads === 1) throw new Error("the tree read was refused");
    return nothing;
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const route = { key: KEY, kind: "issue" } as const;
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={empty} route={route}>
              <BodySlot issue={empty} route={route} />
              {children}
            </ProvidersSlot>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const view = renderHook(() => useIssueTree(empty), { wrapper });
    const retry = await failedTreeRetry();
    act(() => retry.focus());
    await pressByKeyboard(retry);
    await waitFor(() => expect(view.result.current.blocksHeld).toBe(true));
    await act(async () => {
      await Bun.sleep(100);
    });
    const tree = document.body.querySelector<HTMLElement>('[role="tree"]');
    expect(document.activeElement, "the answer puts the focus on the outline's tree").toBe(tree);

    act(() => {
      tree?.dispatchEvent(new FocusEvent("blur"));
      window.dispatchEvent(new FocusEvent("blur"));
    });
    act(() => {
      window.dispatchEvent(new FocusEvent("focus"));
      tree?.dispatchEvent(new FocusEvent("focus"));
    });
    expect(
      {
        focused: document.activeElement === tree,
        focusable: tree?.getAttribute("tabindex") ?? null,
      },
      "the tree still holds the focus after the window switch, and can"
    ).toEqual({ focusable: "-1", focused: true });
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

/** The stage `turnedStage` draws, with the shell's event feed beside the hook, as the app mounts
 *  it beside every page. */
function fedStage(client: QueryClient, on: FilterOn) {
  const Stage = turnedStage(client, on);
  return ({ children }: { children: ReactNode }) => (
    <Stage>
      <ShellEventFeed />
      {children}
    </Stage>
  );
}

/** Dispatch starting the event feed on the page's latest socket, which it opens first, after
 *  event 1, with the events of any gap replayed whole or in part. */
function startFeedOn(sockets: FakeLiveSockets, replayComplete: boolean): void {
  const socket = sockets.latest();
  act(() => socket.open());
  const { sub } = socket.subscribed("events");
  act(() =>
    socket.receive({ type: "events_start", sub, since: "1", replay_complete: replayComplete })
  );
}

/** Dispatch restarting under the page's socket, and starting the event feed again on the next
 *  socket the page opens, with the gap replayed whole or in part. */
async function feedBackAfterRestart(
  sockets: FakeLiveSockets,
  replayComplete: boolean
): Promise<void> {
  const lost = sockets.latest();
  act(() => lost.restart());
  await act(async () => {
    await Bun.sleep(200);
  });
  expect(sockets.latest(), "the page opened its next socket").not.toBe(lost);
  startFeedOn(sockets, replayComplete);
}

// The Spec filter's view read is refused, and the reader turns that filter off. The live feed
// comes back with its gap replayed whole, which asks for the refused view again as a focus with
// the feed down does: nothing reads it while its filter is off, so it is read when its filter is
// next on - once: refused again, it is read no more for a press of its filter.
test("a refused view whose filter is off when the live feed comes back with its gap replayed whole is read once when its filter is next on", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  refuseSpecView();
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const sockets = installFakeLiveSockets();
  // Signed in at event head 1, as the app's sign-in gate holds it before the page.
  client.setQueryData(["whoami"], { event_head: "1", kind: "user", login: "alice" });
  const on: FilterOn = { preset: "spec" };
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: fedStage(client, on) });
    startFeedOn(sockets, true);
    await waitFor(() => expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error"));
    expect(treeReadsOf("spec")).toBe(1);
    on.preset = "conversation";
    view.rerender();
    await act(async () => {
      await Bun.sleep(1_100);
    });

    await feedBackAfterRestart(sockets, true);
    await act(async () => {
      await Bun.sleep(300);
    });
    expect(treeReadsOf("spec"), "no view read while its filter is off").toBe(1);

    await specOnReadsRefusedViewOnce(
      client,
      view,
      on,
      "the refused view is read once for the feed coming back"
    );
    view.unmount();
  } finally {
    liveSocketFor(client).close();
    sockets.restore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A tree read refused, its retries spent, and the live feed comes back on the page's next socket
// with its gap replayed in part: the event feed refreshes every query the page holds, which reads
// the refused tree again. That is the one read for the feed coming back: the page's subscription
// asks for nothing of its own, which would read the tree a second time once the scheduler's
// second since the first read had passed.
test("a refused tree read is read once, not twice, when the live feed comes back with its gap replayed in part", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: NO_ROUTE_ROLLING_RELEASE_MESSAGE })
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const sockets = installFakeLiveSockets();
  // Signed in at event head 1, as the app's sign-in gate holds it before the page.
  client.setQueryData(["whoami"], { event_head: "1", kind: "user", login: "alice" });
  try {
    const view = renderHook(() => useIssueTree(issue), {
      wrapper: fedStage(client, { preset: "conversation" }),
    });
    startFeedOn(sockets, true);
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error")
    );
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const before = treeReadsOf();

    await feedBackAfterRestart(sockets, false);
    await act(async () => {
      await Bun.sleep(2_500);
    });
    expect(treeReadsOf() - before, "the feed coming back reads the refused tree once").toBe(1);
    view.unmount();
  } finally {
    liveSocketFor(client).close();
    sockets.restore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// With the live feed down, no event says when a write has reached Dispatch. A change of the
// issue seen while a write is out - here the page's read of the issue, invalidated with no event
// to raise it, answering a field someone's act moves - would read a tree Dispatch has not changed
// yet, so the tree is read no more until the write settles, and then once.
test("with the live feed down, a change seen while a write is out reads the tree only once the write settles, and once", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let answer: IssueDetails = issue;
  const getIssue = spyOn(api, "getIssue").mockImplementation(async () => answer);
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const answered = Promise.withResolvers<void>();
  try {
    const view = renderHook(
      () => {
        // The page's read of the issue.
        useQuery(issueQuery(KEY));
        return useIssueTree(issue);
      },
      { wrapper: stageWrapper(client) }
    );
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("success")
    );
    await waitFor(() => expect(client.getQueryState(["issue", KEY])?.status).toBe("success"));
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const before = treeReadsOf();
    const issueReads = getIssue.mock.calls.length;

    const write = storeWrite(client, KEY, () => answered.promise);
    const settled = write.mutate();
    answer = { ...issue, status: "in_progress" };
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["issue", KEY] });
      await Bun.sleep(1_500);
    });
    expect(getIssue.mock.calls.length, "the issue read is read again").toBe(issueReads + 1);
    expect(client.getQueryData<IssueDetails>(["issue", KEY])?.status, "its moved field").toBe(
      "in_progress"
    );
    expect(treeReadsOf() - before, "no tree read while the write is out").toBe(0);

    await act(async () => {
      answered.resolve();
      await settled;
      await Bun.sleep(1_500);
    });
    expect(treeReadsOf() - before, "one tree read once the write settled").toBe(1);
    view.unmount();
  } finally {
    answered.resolve();
    getIssue.mockRestore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A Retry answered with an issue that holds nothing puts the focus on the outline's tree, which
// is focusable only while it holds the focus. The reader then moves the focus to another control
// on the page: the focus has left the tree by the time it is blurred, so the tree stops being
// focusable in that same turn, with no task between in which Tab or a script could reach it.
test("the focus moved from the outline's tree to another control makes the tree unfocusable in that same turn", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const empty: IssueDetails = { ...issue, artifacts: [], plan_artifact_id: "" };
  const nothing: TreeAnswer = {
    ...treeAnswer([]),
    nodes: [answerNode(KEY, "issue", { parent: null })],
  };
  let reads = 0;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    reads += 1;
    if (reads === 1) throw new Error("the tree read was refused");
    return nothing;
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const route = { key: KEY, kind: "issue" } as const;
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={empty} route={route}>
              <BodySlot issue={empty} route={route} />
              {children}
            </ProvidersSlot>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const other = document.createElement("button");
  other.textContent = "Another control";
  document.body.append(other);
  try {
    const view = renderHook(() => useIssueTree(empty), { wrapper });
    const retry = await failedTreeRetry();
    act(() => retry.focus());
    await pressByKeyboard(retry);
    await waitFor(() => expect(view.result.current.blocksHeld).toBe(true));
    await act(async () => {
      await Bun.sleep(100);
    });
    const tree = document.body.querySelector<HTMLElement>('[role="tree"]');
    expect(document.activeElement, "the answer puts the focus on the outline's tree").toBe(tree);
    expect(tree?.getAttribute("tabindex"), "the tree focusable while it holds the focus").toBe(
      "-1"
    );

    act(() => other.focus());
    expect(
      {
        focused: document.activeElement === other,
        focusable: tree?.getAttribute("tabindex") ?? null,
      },
      "the focus on the other control, and the tree no longer focusable"
    ).toEqual({ focusable: null, focused: true });
    view.unmount();
  } finally {
    other.remove();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// With the live feed down, a window focus reads a refused tree again. While a write is out that
// read would read a tree Dispatch has not changed yet, and a change the write makes reads the
// tree again once it settles: the focus's read waits until the write settles, and is one read.
test("with the live feed down, a focus while a write is out reads a refused tree only once the write settles, and once", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValue(
    new Error("the tree read was refused")
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const answered = Promise.withResolvers<void>();
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: stageWrapper(client) });
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error")
    );
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const before = treeReadsOf();

    const write = storeWrite(client, KEY, () => answered.promise);
    const settled = write.mutate();
    // The mutation hands its write to the write store a few microtasks after its press.
    await waitFor(() => expect(writesOf(client).writesOutOf(KEY)).toHaveLength(1));
    await act(async () => {
      focusWindow();
      await Bun.sleep(1_500);
    });
    expect(treeReadsOf() - before, "no tree read while the write is out").toBe(0);

    await act(async () => {
      answered.resolve();
      await settled;
      await Bun.sleep(1_500);
    });
    expect(treeReadsOf() - before, "one tree read once the write settled").toBe(1);
    view.unmount();
  } finally {
    answered.resolve();
    focusManager.setFocused(undefined);
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A tree that holds an answer, whose next read Dispatch refused, is a refused read as much as a
// first read refused: the outline draws the answer it holds, and the read stays refused until
// the reader's Retry, an event of the issue or the live feed coming back. A surface that starts
// reading the tree afterwards - a filter pressed, which draws its slot - is none of those, so it
// reads nothing of its own.
test("a surface that starts reading a tree whose refresh was refused sends no read while the live feed is up", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let refused = false;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    if (refused) throw new Error("the tree read was refused");
    return treeAnswer([comment]);
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const feed = liveFeedOn(client);
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper: stageWrapper(client) });
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("success")
    );
    refused = true;
    await act(async () => {
      await client.refetchQueries({ exact: true, queryKey: conversationTreeKey(KEY) });
    });
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error")
    );
    expect(holdsComment(view.result.current.root), "the outline draws the answer it holds").toBe(
      true
    );
    const before = treeReadsOf();

    const later = renderHook(() => useIssueTree(issue), {
      wrapper: ({ children }: { children: ReactNode }) => (
        <RoutedQueries client={client}>{children}</RoutedQueries>
      ),
    });
    await act(async () => {
      await Bun.sleep(1_500);
    });
    expect(
      treeReadsOf() - before,
      "a surface that starts reading the tree reads nothing of its own"
    ).toBe(0);
    later.unmount();
    view.unmount();
  } finally {
    feed.end();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// The same for a filter's view: the Spec view holds an answer, and Dispatch refused its next
// read. The reader pressing the Spec filter off and on again is no Retry, no event and no feed
// coming back, so it sends no view read.
test("a filter pressed off and on after its view's refresh was refused sends no read", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const specView = viewAnswer({ ...task, role: "query" });
  let refused = false;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) => {
    if (query?.view !== "spec") return treeAnswer([]);
    if (refused) throw new Error("the spec view read was refused");
    return specView;
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const on: FilterOn = { preset: "spec" };
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: turnedStage(client, on) });
    await waitFor(() => expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("success"));
    refused = true;
    await act(async () => {
      await client.refetchQueries({ exact: true, queryKey: viewKey(KEY, "spec") });
    });
    await waitFor(() => expect(client.getQueryState(viewKey(KEY, "spec"))?.status).toBe("error"));
    expect(client.getQueryState(viewKey(KEY, "spec"))?.data, "the view keeps its answer").toBe(
      specView
    );
    const before = treeReadsOf("spec");

    await pressSpecOffAndOn(view, on);
    await act(async () => {
      await Bun.sleep(1_200);
    });
    expect(treeReadsOf("spec") - before, "no view read for the press").toBe(0);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// A tree read refused, its retries spent, and the reader leaves the issue for another page, the
// shell's event feed staying. While the reader is away, the live feed comes back with its gap
// replayed whole, which reads a refused tree again. The reader comes back to the issue: the tree
// is read, and the outline draws what Dispatch serves now in place of its failed line.
test("a refused tree read is read again when the live feed comes back while the reader is on another page", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let refused = true;
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () => {
    if (refused) throw new Error("the tree read was refused");
    return treeAnswer([comment]);
  });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const sockets = installFakeLiveSockets();
  // Signed in at event head 1, as the app's sign-in gate holds it before the page.
  client.setQueryData(["whoami"], { event_head: "1", kind: "user", login: "alice" });
  const shell = render(
    <QueryClientProvider client={client}>
      <ShellEventFeed />
    </QueryClientProvider>
  );
  const on: FilterOn = { preset: "conversation" };
  try {
    const first = renderHook(() => useIssueTree(issue), { wrapper: turnedStage(client, on) });
    startFeedOn(sockets, true);
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error")
    );
    await act(async () => {
      await Bun.sleep(1_100);
    });
    first.unmount();
    refused = false;

    await feedBackAfterRestart(sockets, true);
    await act(async () => {
      await Bun.sleep(1_500);
    });

    const back = renderHook(() => useIssueTree(issue), { wrapper: turnedStage(client, on) });
    await act(async () => {
      await Bun.sleep(1_500);
    });
    expect(
      client.getQueryState(conversationTreeKey(KEY))?.status,
      "the refused tree was read again for the feed coming back"
    ).toBe("success");
    expect(holdsComment(back.result.current.root), "the outline draws what Dispatch serves").toBe(
      true
    );
    back.unmount();
  } finally {
    shell.unmount();
    liveSocketFor(client).close();
    sockets.restore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// With the live feed down, a window focus while a write is out waits until the write settles
// before it reads a refused tree. The feed comes back before that, with its gap replayed whole,
// and asks for the refused tree, which the write holds and which is read as it settles. Once
// the write settles the feed is up, and a focus while
// the feed is up reads no refused tree: the focus that waited reads nothing, so the tree is read
// once, not twice.
test("a focus held for a write reads no refused tree once the live feed came back and read it", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockRejectedValue(
    new Error("the tree read was refused")
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const sockets = installFakeLiveSockets();
  // Signed in at event head 1, as the app's sign-in gate holds it before the page.
  client.setQueryData(["whoami"], { event_head: "1", kind: "user", login: "alice" });
  const answered = Promise.withResolvers<void>();
  try {
    const view = renderHook(() => useIssueTree(issue), {
      wrapper: fedStage(client, { preset: "conversation" }),
    });
    startFeedOn(sockets, true);
    await waitFor(() =>
      expect(client.getQueryState(conversationTreeKey(KEY))?.status).toBe("error")
    );
    await act(async () => {
      await Bun.sleep(1_100);
    });
    // Dispatch restarts under the page's socket; the page opens its next one and sends the feed
    // there, which Dispatch has not started yet: the feed is down.
    const lost = sockets.latest();
    act(() => lost.restart());
    await act(async () => {
      await Bun.sleep(200);
    });
    const next = sockets.latest();
    expect(next, "the page opened its next socket").not.toBe(lost);
    act(() => next.open());
    const before = treeReadsOf();

    const write = storeWrite(client, KEY, () => answered.promise);
    const settled = write.mutate();
    // The mutation hands its write to the write store a few microtasks after its press.
    await waitFor(() => expect(writesOf(client).writesOutOf(KEY)).toHaveLength(1));
    await act(async () => {
      focusWindow();
      await Bun.sleep(300);
    });
    expect(treeReadsOf() - before, "no tree read while the write is out").toBe(0);

    const { sub } = next.subscribed("events");
    act(() => next.receive({ type: "events_start", sub, since: "1", replay_complete: true }));
    await act(async () => {
      await Bun.sleep(1_500);
    });
    expect(
      treeReadsOf() - before,
      "the feed coming back reads no refused tree while the write is out"
    ).toBe(0);

    await act(async () => {
      answered.resolve();
      await settled;
      await Bun.sleep(1_500);
    });
    expect(
      treeReadsOf() - before,
      "the focus that waited for the write reads nothing once the feed is up"
    ).toBe(1);
    view.unmount();
  } finally {
    answered.resolve();
    focusManager.setFocused(undefined);
    liveSocketFor(client).close();
    sockets.restore();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// An event of the comments with the live feed up, a moment after the tree was read and the
// comments were read just after it: the event names both, and the app's scheduler holds each to
// a second after its last read began. The one event reads the tree once and the comments once.
test("an event soon after the tree and the comments were read reads each once", async () => {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs), { comments: [comment] });
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  trackQueryFetches(client);
  const feed = liveFeedOn(client);
  const commentReads = () => readsSent().comments;
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(holdsComment(view.result.current)).toBe(true));
    await act(async () => {
      await Bun.sleep(1_100);
      // A read of the tree, and 50 ms later one of the comments, each answering what it held.
      await client.refetchQueries({ exact: true, queryKey: conversationTreeKey(KEY) });
      await Bun.sleep(50);
      await client.refetchQueries({ queryKey: ["comments", KEY] });
      await Bun.sleep(200);
    });
    const before = treeReadsOf();
    const commentsBefore = commentReads();

    act(() => {
      refreshQueries(client, commentEventTargets());
    });
    await act(async () => {
      await Bun.sleep(100);
    });
    expect(client.getQueryState(["comments", KEY])?.isInvalidated, "the event's mark").toBe(true);
    expect(commentReads() - commentsBefore, "the comments read is held back").toBe(0);
    expect(treeReadsOf() - before, "and so is the tree read").toBe(0);

    // Past the scheduler's second after the tree's last read began, and the comments'.
    await act(async () => {
      await Bun.sleep(2_500);
    });
    expect(commentReads() - commentsBefore, "the comments read the scheduler owed").toBe(1);
    expect(treeReadsOf() - before, "one tree read for the event").toBe(1);
    view.unmount();
  } finally {
    feed.end();
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

/** What a row of the rule below reads: the tree, or the view of the Spec, Plan or Verification
 *  filter. */
type RuleRead = "tree" | "spec" | "plan" | "verification";

/** A row's read in its title's words. */
const RULE_READ_WORDS: Record<RuleRead, string> = {
  plan: "the Plan view",
  spec: "the Spec view",
  tree: "the tree",
  verification: "the Verification view",
};

/** The stage a row draws, around the page's model, answering the tree read as the outline's
 *  failed line reads it (`TreeReadFailed`), whose `refetch` is the tree's Retry. */
type RuleStage = RenderHookResult<TreeReadState, unknown>;

/** One row's page: its client, the shell's event feed beside the page with the live feed up, and
 *  what the row has drawn. Dispatch answers every read, but refuses the row's own read while
 *  `refusing` holds, and holds it open until `out` settles. */
interface Rig {
  readonly read: RuleRead;
  readonly client: QueryClient;
  readonly sockets: FakeLiveSockets;
  readonly on: FilterOn;
  /** The stage, while it is mounted. */
  stage: RuleStage | undefined;
  /** The slots a way drew beside the stage, until the row takes them down. */
  readonly slots: { readonly unmount: () => void }[];
  /** What Dispatch holds of the issue's one comment: resolved once a resolve reaches it. */
  held: Comment;
  /** The comments someone else posted on the issue, which Dispatch holds once posted. */
  readonly posted: Comment[];
  refusing: boolean;
  out: { readonly promise: Promise<void>; readonly resolve: () => void } | undefined;
  /** Every request the page has sent. */
  readonly requests: () => number;
  readonly close: () => void;
}

/** The Spec view as Dispatch serves it: the plan's task, marked as the view's query found it. */
const SPEC_VIEW = viewAnswer({ ...task, role: "query" });

/** A row's page, with the shell's event feed mounted and its live feed up, and no stage yet. */
function openRig(read: RuleRead): Rig {
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const sockets = installFakeLiveSockets();
  // Signed in at event head 1, as the app's sign-in gate holds it before the page.
  client.setQueryData(["whoami"], { event_head: "1", kind: "user", login: "alice" });
  const shell = render(
    <QueryClientProvider client={client}>
      <ShellEventFeed />
    </QueryClientProvider>
  );
  const rig: Rig = {
    client,
    close: () => {
      rig.out?.resolve();
      for (const slot of rig.slots.splice(0)) slot.unmount();
      rig.stage?.unmount();
      shell.unmount();
      liveSocketFor(client).close();
      sockets.restore();
      focusManager.setFocused(undefined);
      onlineManager.setOnline(true);
      for (const stub of stubs) stub.mockRestore();
      client.clear();
    },
    held: comment,
    on: { preset: read === "tree" ? "conversation" : read },
    out: undefined,
    posted: [],
    read,
    refusing: false,
    requests: () => stubs.reduce((sent, stub) => sent + stub.mock.calls.length, 0),
    slots: [],
    sockets,
    stage: undefined,
  };
  (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => [
    rig.held,
    ...rig.posted,
  ]);
  // The tree holds the comment as Dispatch holds it: settled once it is resolved; and each comment
  // someone else posted.
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async (_key, query) => {
    if ((query?.view ?? "tree") === read) {
      await rig.out?.promise;
      if (rig.refusing) throw new Error(`the ${read} read was refused`);
    }
    if (query?.view !== undefined) return SPEC_VIEW;
    return heldCommentTree(rig.held, rig.posted);
  });
  startFeedOn(sockets, true);
  return rig;
}

/** The key of the row's query. */
function ruleKey(rig: Rig): readonly unknown[] {
  return rig.read === "tree" ? conversationTreeKey(KEY) : viewKey(KEY, rig.read);
}

/** How many reads of the row's query the page has sent. */
function ruleReads(rig: Rig): number {
  return treeReadsOf(rig.read === "tree" ? undefined : rig.read);
}

/** The row's stage, which a row that observes its read has mounted. */
function stageOf(rig: Rig): RuleStage {
  if (rig.stage === undefined) throw new Error("no stage is mounted");
  return rig.stage;
}

/** The stage mounted, with whichever filter `rig.on` names on. */
function mountStage(rig: Rig): void {
  rig.stage = renderHook(
    () => {
      useIssueTree(issue);
      return useTreeRead(issue);
    },
    { wrapper: turnedStage(rig.client, rig.on) }
  );
}

/** Lets what the row just did run for 100 ms of the clock, and checks that the row's query then
 *  stands in `status`. */
async function ruleSettled(rig: Rig, status: "error" | "success"): Promise<void> {
  await tick(100, 10);
  expect(rig.client.getQueryState(ruleKey(rig))?.status).toBe(status);
}

/** The reader stops observing the row's read: leaves the issue for another page, the shell and its
 *  event feed staying, or turns the row's filter off. */
function leave(rig: Rig): void {
  if (rig.read === "tree") {
    stageOf(rig).unmount();
    rig.stage = undefined;
    return;
  }
  rig.on.preset = offPreset(rig);
  stageOf(rig).rerender();
}

/** The view the row's filter is off in: Conversation, but for Plan, whose view read every load of
 *  Conversation or of a filter sends with it (R4), so its filter is off in Graph, which sends no
 *  view read. */
function offPreset(rig: Rig): FilterOn["preset"] {
  return rig.read === "plan" ? "graph" : "conversation";
}

/** The reader observes the row's read again: comes back to the issue, or turns the row's filter
 *  on. */
function comeBack(rig: Rig): void {
  if (rig.read === "tree") {
    mountStage(rig);
    return;
  }
  rig.on.preset = rig.read;
  stageOf(rig).rerender();
}

/** Moves the clock on until the page has sent nothing for 1.2 s, past the scheduler's second
 *  between two reads of one query, with nothing out but the read the row holds open: by then every
 *  read the scheduler or the page's subscription owes has begun. */
async function quiet(rig: Rig): Promise<void> {
  for (let attempt = 0; attempt < 10; attempt += 1) {
    const sent = rig.requests();
    await tick(MIN_REFETCH_INTERVAL_MS + 200, 50);
    const out = rig.client.isFetching() - (rig.out === undefined ? 0 : 1);
    if (rig.requests() === sent && out === 0) return;
  }
  throw new Error("the page was still reading after 12 s");
}

/** What a comment's event of the issue refreshes (`eventRefreshTargets`): the comments, the issue
 *  read, and the tree read with its views among them. */
function commentEventTargets(): RefreshTarget[] {
  return eventRefreshTargets(
    feedEvent("comment.created", { body: "A reply.", id: "comment-event", kind: "comment" }),
    "alice"
  );
}

/** What a Keep's event of the issue refreshes (`eventRefreshTargets` of `decision.kept`): the
 *  decisions read, the issue read, and the tree read with its views among them. */
function keepEventTargets(): RefreshTarget[] {
  return eventRefreshTargets(feedEvent("decision.kept", { block_id: "block-1" }), "alice");
}

/** An event of the issue: the event feed's refresh of what a comment's event names
 *  (`commentEventTargets`), in an awaited `act`, so a read it begins updates the stage. */
async function commentsEvent(rig: Rig): Promise<void> {
  await act(async () => {
    refreshQueries(rig.client, commentEventTargets());
  });
}

/** Dispatch restarting under the page's socket, and 200 ms of the clock, in which the page makes
 *  its next socket (`feedBackAfterRestart`'s first half, on the row's clock). */
async function restartFeed(rig: Rig): Promise<void> {
  const lost = rig.sockets.latest();
  act(() => lost.restart());
  await tick(200, 10);
  expect(rig.sockets.latest(), "the page made its next socket").not.toBe(lost);
}

/** Dispatch restarting under the page's socket, and the page's next socket open with the feed
 *  sent on it and not started yet: the live feed is down. */
async function feedDown(rig: Rig): Promise<void> {
  await restartFeed(rig);
  const next = rig.sockets.latest();
  act(() => next.open());
  expect(liveFeedUp(rig.client), "the live feed is down").toBe(false);
}

/** A state a row's read stands in before the way, and how the row reaches it: observed, or with
 *  the reader away from it. */
interface RuleState {
  readonly words: string;
  readonly reach: (rig: Rig, observed: boolean) => Promise<void>;
}

/** The read answered, the reader then away from it where the row says so. */
async function reachAnswered(rig: Rig, observed: boolean): Promise<void> {
  mountStage(rig);
  await ruleSettled(rig, "success");
  if (!observed) leave(rig);
}

const STATES = {
  never: {
    words: "never read",
    // The tree before the reader first opens the issue: no stage, and no subscription of the
    // issue. The Spec view while its filter was never on in the load.
    reach: async (rig, observed) => {
      if (observed) throw new Error("an observer reads a read never made");
      if (rig.read === "tree") return;
      rig.on.preset = offPreset(rig);
      mountStage(rig);
      await tick(100, 10);
      expect(rig.client.getQueryState(conversationTreeKey(KEY))?.status).toBe("success");
    },
  },
  answered: { words: "answered", reach: reachAnswered },
  stale: {
    words: "answered and marked stale by an event while nothing observed it",
    reach: async (rig, observed) => {
      if (observed) throw new Error("an observer reads a stale answer");
      await reachAnswered(rig, false);
      await commentsEvent(rig);
      await tick(100, 10);
      expect(rig.client.getQueryState(ruleKey(rig))?.isInvalidated).toBe(true);
    },
  },
  refused: {
    words: "refused with no answer held",
    reach: async (rig, observed) => {
      rig.refusing = true;
      mountStage(rig);
      await ruleSettled(rig, "error");
      if (!observed) leave(rig);
    },
  },
  refusedHeld: {
    words: "refused over a held answer",
    reach: async (rig, observed) => {
      mountStage(rig);
      await ruleSettled(rig, "success");
      rig.refusing = true;
      await act(async () => {
        await rig.client.refetchQueries({ exact: true, queryKey: ruleKey(rig) });
      });
      await ruleSettled(rig, "error");
      expect(rig.client.getQueryState(ruleKey(rig))?.data, "the answer it holds").toBeDefined();
      if (!observed) leave(rig);
    },
  },
  inFlight: {
    words: "in flight",
    // The read held open until the way is done, and then answered.
    reach: async (rig, observed) => {
      rig.out = Promise.withResolvers<void>();
      mountStage(rig);
      await tick(100, 10);
      expect(rig.client.getQueryState(ruleKey(rig))?.fetchStatus).toBe("fetching");
      if (!observed) leave(rig);
    },
  },
  inFlightRefused: {
    words: "in flight and refused once the way is done",
    // The read held open until the way is done, and then refused. The tree's is given up as the
    // stage leaves (the note above the rows), so only the views' stand in it unobserved.
    reach: async (rig, observed) => {
      rig.refusing = true;
      rig.out = Promise.withResolvers<void>();
      mountStage(rig);
      await tick(100, 10);
      expect(rig.client.getQueryState(ruleKey(rig))?.fetchStatus).toBe("fetching");
      if (!observed) leave(rig);
    },
  },
  inFlightHeld: {
    words: "in flight over a held answer",
    // Answered, then read again by a refetch held open until the way is done, and then answered.
    reach: async (rig, observed) => {
      mountStage(rig);
      await ruleSettled(rig, "success");
      rig.out = Promise.withResolvers<void>();
      act(() => {
        void rig.client.refetchQueries({ exact: true, queryKey: ruleKey(rig) });
      });
      await tick(100, 10);
      expect(rig.client.getQueryState(ruleKey(rig))?.fetchStatus).toBe("fetching");
      if (!observed) leave(rig);
    },
  },
  refusedAgain: {
    words:
      "refused, asked for again by the live feed back whole while nothing observed it, read again as it was next observed and refused again",
    reach: async (rig, observed) => {
      rig.refusing = true;
      mountStage(rig);
      await ruleSettled(rig, "error");
      leave(rig);
      await quiet(rig);
      await restartFeed(rig);
      startFeedOn(rig.sockets, true);
      await quiet(rig);
      const away = ruleReads(rig);
      comeBack(rig);
      await quiet(rig);
      expect(ruleReads(rig) - away, "the read observed again").toBe(1);
      expect(rig.client.getQueryState(ruleKey(rig))?.status).toBe("error");
      if (!observed) leave(rig);
    },
  },
} satisfies Record<string, RuleState>;

/** A way a read may be started, and what else must hold once its reads settled. */
interface RuleWay {
  readonly words: string;
  readonly does: (rig: Rig) => Promise<void>;
  readonly ends?: (rig: Rig) => void;
}

/** That the tree read holds the comment as Dispatch left it, where the stage is mounted, so the
 *  tree has been read since, and the row's read is not the tree refused, which holds no answer of
 *  the write. */
function treeHoldsComment(state: "live" | "settled"): (rig: Rig) => void {
  return (rig) => {
    if (rig.stage === undefined || (rig.read === "tree" && rig.refusing)) return;
    expect(
      rig.client
        .getQueryData<TreeAnswer>(conversationTreeKey(KEY))
        ?.nodes?.find((node) => node.id === comment.id)?.state,
      state === "settled" ? "the tree read holds the resolve" : "the tree read holds no resolve"
    ).toBe(state);
  };
}

/** Every read of the tree and of its views the page has sent. */
function treeAndViewReads(): number {
  return (api.getIssueTree as Mock<typeof api.getIssueTree>).mock.calls.length;
}

/** Presses a write (`press`), and moves the clock 10 ms at a time until Dispatch has answered or
 *  refused it; `meanwhile` runs once, halfway through `latency`, while the write is out. */
async function writeSettles(
  press: () => Promise<unknown>,
  latency: number,
  meanwhile?: () => Promise<void>
): Promise<void> {
  let settled = false;
  const done = () => {
    settled = true;
  };
  void press().then(done, done);
  let ran = false;
  for (let waited = 0; !settled && waited <= latency + 1_000; waited += 10) {
    if (!ran && waited >= latency / 2) {
      ran = true;
      await meanwhile?.();
    }
    await tick(10, 10);
  }
  expect(settled, "Dispatch answered the write").toBe(true);
}

/** The margin's resolve with the live feed down: no event comes, and the resolve reads the tree
 *  once, after Dispatch answered, and nothing of the tree or its views while it is out. */
function marginResolveDown(latency: number): RuleWay {
  return {
    words: `a resolve from the margin answered in ${latency} ms with the live feed down`,
    does: async (rig) => {
      await feedDown(rig);
      const before = treeAndViewReads();
      let whileOut: number | undefined;
      const resolve = marginResolveOn(rig.client, async () => {
        rig.held = { ...comment, resolved: true };
        await Bun.sleep(latency);
        whileOut = treeAndViewReads() - before;
      });
      await writeSettles(() => resolve.mutate(), latency);
      expect(whileOut, "the reads of the tree and its views while the resolve is out").toBe(0);
    },
    ends: treeHoldsComment("settled"),
  };
}

/** A comment someone else posts on the issue while a row's write is out. */
const postedComment: Comment = {
  ...comment,
  author: { id: "bob", kind: "user" },
  body: "A word from someone else.",
  created_at: "2026-11-05T02:00:00Z",
  id: "comment-2",
};

/** That the tree read holds each comment someone else posted, where the stage is mounted, so the
 *  tree has been read since they were posted. */
function treeHoldsPosted(rig: Rig): void {
  if (rig.stage === undefined) return;
  const nodes = rig.client.getQueryData<TreeAnswer>(conversationTreeKey(KEY))?.nodes ?? [];
  for (const one of rig.posted) {
    expect(
      nodes.some((node) => node.id === one.id),
      "the tree read holds the comment posted while the write was out"
    ).toBe(true);
  }
}

/** The margin's resolve Dispatch refuses in 200 ms (`useMarginItems.ts`): its `onMutate` writes
 *  the resolve into the comments read, and its `onError` puts the comments back and reads them
 *  again. With the live feed down, nothing of the tree or its views is read while it is out.
 *  With `posted`, someone else posts a comment 100 ms in, and its event refreshes the comments
 *  while the resolve is out. */
function refusedResolve(feed: "down" | "up", posted = false): RuleWay {
  return {
    words: `a resolve from the margin Dispatch refuses in 200 ms and the page rolls back, with the live feed ${feed}${posted ? ", and the event of a comment someone else posts landing while it is out" : ""}`,
    does: async (rig) => {
      if (feed === "down") await feedDown(rig);
      const before = treeAndViewReads();
      let whileOut: number | undefined;
      const resolve = storeWrite<{ previous: Comment[] | undefined }>(
        rig.client,
        KEY,
        async () => {
          await Bun.sleep(200);
          whileOut = treeAndViewReads() - before;
          throw new Error("Dispatch refused the resolve");
        },
        {
          onError: (_error, _input, context) => {
            rig.client.setQueryData(["comments", KEY], context?.previous);
            void rig.client.invalidateQueries({ queryKey: ["comments", KEY] });
          },
          onMutate: async () => {
            await rig.client.cancelQueries({ queryKey: ["comments", KEY] });
            const previous = rig.client.getQueryData<Comment[]>(["comments", KEY]);
            rig.client.setQueryData<Comment[]>(["comments", KEY], (current) =>
              current?.map((one) => ({ ...one, resolved: true }))
            );
            return { previous };
          },
        }
      );
      await writeSettles(
        () => resolve.mutate(),
        200,
        posted
          ? async () => {
              rig.posted.push(postedComment);
              await commentsEvent(rig);
            }
          : undefined
      );
      if (feed === "down") {
        expect(whileOut, "the reads of the tree and its views while the resolve is out").toBe(0);
      }
    },
    ends: (rig) => {
      treeHoldsComment("live")(rig);
      treeHoldsPosted(rig);
    },
  };
}

/** A write of the issue's title put in the cache (`setQueryData`), as a Keep or a status pick
 *  puts what Dispatch answered (`landIssueWrite`). */
function renameIssue(client: QueryClient): void {
  client.setQueryData<IssueDetails>(["issue", KEY], {
    ...issue,
    title: `${issue.title}, renamed`,
  });
}

/** A write of the issue that Dispatch answers in 200 ms and the page puts in the cache by
 *  `setQueryData` (a Keep, a status pick: `landIssueWrite`), with no event. With the live feed
 *  down, nothing of the tree or its views is read while it is out. */
function issueWriteSet(feed: "down" | "up"): RuleWay {
  return {
    words: `a write of the issue the page puts in the cache, answered in 200 ms with the live feed ${feed} and no event`,
    does: async (rig) => {
      if (feed === "down") await feedDown(rig);
      const before = treeAndViewReads();
      let whileOut: number | undefined;
      const write = storeWrite(
        rig.client,
        KEY,
        async () => {
          await Bun.sleep(200);
          whileOut = treeAndViewReads() - before;
        },
        {
          onSuccess: () => {
            renameIssue(rig.client);
          },
        }
      );
      await writeSettles(() => write.mutate(), 200);
      if (feed === "down") {
        expect(whileOut, "the reads of the tree and its views while the write is out").toBe(0);
      }
    },
  };
}

/** A write that Dispatch takes, resolving the comment, and whose answer is lost 200 ms on: it
 *  lands with none, and the write store refreshes every query the app holds as it lands
 *  (`answerlessLanding`). With `resolves` false it changes nothing Dispatch holds of the issue's
 *  reads. */
function answerlessWrite(
  rig: Rig,
  { resolves = true }: { readonly resolves?: boolean } = {}
): MutationObserver<unknown, Error, void, unknown> {
  return storeWrite(rig.client, KEY, async () => {
    if (resolves) rig.held = { ...comment, resolved: true };
    await Bun.sleep(200);
    return null;
  });
}

/** A write that Dispatch took and whose answer was lost, so it lands with none (`keyed-write.ts`'
 *  `writeUnderKey` answers `null`): the write store refreshes every query the app holds
 *  (`invalidateQueries()`, `write-store.ts`' `#landed`) inside the write, before its mutation
 *  settles. With the live feed up its event follows 150 ms on. With `resolves` false the write
 *  changes nothing the issue's reads answer: the write store's refresh of every query is the one
 *  ask of the tree and its views, which the write holds until it settles, a refused one too. */
function answerlessLanding(
  feed: "down" | "up",
  { resolves = true }: { readonly resolves?: boolean } = {}
): RuleWay {
  return {
    words: `a write that lands in 200 ms with no answer and a refresh of every query${resolves ? "" : ", changing nothing the issue's reads answer"}, with the live feed ${feed}`,
    does: async (rig) => {
      if (feed === "down") await feedDown(rig);
      const write = answerlessWrite(rig, { resolves });
      await writeSettles(() => write.mutate(), 200);
      if (feed === "up") {
        await tick(150, 10);
        await commentsEvent(rig);
      }
    },
    ...(resolves ? { ends: treeHoldsComment("settled") } : {}),
  };
}

/** A write of the page that Dispatch holds until the row lets it go: pressed now, and settled, with
 *  `onSuccess` then where given, by the function it returns. It is a write of the issue the page
 *  draws, or, with `of`, of another, or of none (`null`); with `path` it tells the request it
 *  sends as it goes out (`bounds.sent`), as a composer's send does (`held-sends.ts`), so the
 *  write store names the issue of that path. */
function holdWrite(
  rig: Rig,
  {
    of = KEY,
    onSuccess,
    path,
  }: {
    readonly of?: string | null;
    readonly onSuccess?: () => void;
    readonly path?: string;
  } = {}
): () => void {
  const answer = Promise.withResolvers<void>();
  const write = storeWrite(
    rig.client,
    of ?? undefined,
    (bounds) => {
      if (path !== undefined) bounds.sent?.({ method: "POST", path });
      return answer.promise;
    },
    onSuccess === undefined ? {} : { onSuccess }
  );
  void write.mutate();
  return answer.resolve;
}

/** A write that lands with no answer (`answerlessLanding`) while a second write of the page is
 *  still out: nothing of the tree or its views is read until the second write settles too, with
 *  the live feed up or down, and no event comes. The second is a write of the issue that changes
 *  nothing the tree reads, or, with `kept`, puts its answer in the cache as it settles (a Keep);
 *  with the feed down the settle reads the tree either way. With
 *  `feedBackPart`, the live feed, down, comes back with its gap replayed in part while the second
 *  write holds the tree and the open view, and the event feed refreshes every query. With
 *  `focus`, a window focus comes then, with the live feed down, whose ask the writes hold. */
function answerlessBesideWrite(
  feed: "down" | "up",
  {
    feedBackPart = false,
    focus = false,
    kept = false,
  }: { feedBackPart?: boolean; focus?: boolean; kept?: boolean } = {}
): RuleWay {
  return {
    words: `a write that lands in 200 ms with no answer and a refresh of every query while a second write is out, ${feedBackPart ? "the live feed then back with its gap replayed in part, " : ""}${focus ? "a window focus then, " : ""}the second then settling${kept ? " and putting its answer in the cache" : ""}, with the live feed ${feed} and no event`,
    does: async (rig) => {
      if (feed === "down") await feedDown(rig);
      const settleSecond = holdWrite(rig, {
        onSuccess: () => {
          if (!kept) return;
          renameIssue(rig.client);
        },
      });
      await tick(10, 10);
      const before = treeAndViewReads();
      const write = answerlessWrite(rig);
      await writeSettles(() => write.mutate(), 200);
      if (focus) act(() => focusWindow());
      await tick(300, 10);
      expect(
        treeAndViewReads() - before,
        "the reads of the tree and its views while the second write is out"
      ).toBe(0);
      if (feedBackPart) {
        await feedStartsAgain(rig, false);
        expect(
          treeAndViewReads() - before,
          "the reads of the tree and its views while the second write is out, the feed back"
        ).toBe(0);
      }
      act(() => settleSecond());
      await tick(50, 10);
    },
    ends: treeHoldsComment("settled"),
  };
}

/** Dispatch starting the event feed again on the page's open socket, the subscription it sent
 *  there, with the gap replayed whole or in part. */
async function feedStartsAgain(rig: Rig, replayComplete: boolean): Promise<void> {
  const socket = rig.sockets.latest();
  const { sub } = socket.subscribed("events");
  act(() =>
    socket.receive({ type: "events_start", sub, since: "1", replay_complete: replayComplete })
  );
  expect(liveFeedUp(rig.client), "the live feed is up").toBe(true);
  await tick(300, 10);
}

/** A window focus with the live feed down while a write of the issue that changes nothing the
 *  tree reads is out: nothing of the tree or its views is read until it settles.
 *  `meanwhile` runs after the focus and before the write settles. */
function focusWhileWriting(words: string, meanwhile?: (rig: Rig) => Promise<void>): RuleWay {
  return {
    words,
    does: async (rig) => {
      await feedDown(rig);
      const settleWrite = holdWrite(rig);
      await tick(10, 10);
      const before = treeAndViewReads();
      act(() => focusWindow());
      await tick(300, 10);
      expect(
        treeAndViewReads() - before,
        "the reads of the tree and its views while the write is out"
      ).toBe(0);
      await meanwhile?.(rig);
      act(() => settleWrite());
      await tick(50, 10);
    },
  };
}

/** A write of the issue out for 1.5 s while the tree is asked for again by `refetchQueries`, with
 *  the live feed up: the write holds the tree, so that ask sends nothing during the hold, and
 *  leaves nothing to hear (the app's client skips a disabled query). 200 ms after the write
 *  settles the comments are read, and 200 ms after that an event names the comments and the tree:
 *  the scheduler holds each read to a second after its last one began. One event, read once. */
const EVENT_AFTER_WAITED_READ: RuleWay = {
  words:
    "a write out for 1.5 s while the tree is read again, the comments read 200 ms after it settles and an event of the issue 200 ms later",
  does: async (rig) => {
    const settleWrite = holdWrite(rig);
    await tick(10, 10);
    act(() => {
      void rig.client.refetchQueries({ exact: true, queryKey: conversationTreeKey(KEY) });
    });
    await tick(1_500, 10);
    act(() => settleWrite());
    await tick(200, 10);
    act(() => {
      void rig.client.refetchQueries({ queryKey: ["comments", KEY] });
    });
    await tick(200, 10);
    await commentsEvent(rig);
  },
};

/** A composer's edit of the comment (`held-sends.ts`), whose request (`PATCH
 *  /api/v1/comments/{id}`) names no issue: the send names the issue its composer belongs to.
 *  Dispatch answers it in 200 ms; with the live feed up its event follows 150 ms on. Nothing of
 *  the tree or its views is read while it is out. */
function composerEdit(feed: "down" | "up"): RuleWay {
  return {
    words: `an edit of a comment from the composer, answered in 200 ms with the live feed ${feed}`,
    does: async (rig) => {
      if (feed === "down") await feedDown(rig);
      const before = treeAndViewReads();
      let whileOut: number | undefined;
      heldSends(rig.client).send(["comment-composer", comment.id], {
        anchor: undefined,
        ask: { multiple: false, options: [], urgency: "med" },
        draft: { body: "Reworded.", mentions: [], replacement: "" },
        edit: {
          carried: undefined,
          id: comment.id,
          save: async () => {
            await Bun.sleep(200);
            whileOut = treeAndViewReads() - before;
            return { ...comment, body: "Reworded." };
          },
        },
        kind: "comment",
        owner: { issueKey: KEY, kind: "issue" },
        plainDelivery: "steer",
        replyTo: null,
        sendKey: "send-edit",
      });
      await tick(400, 10);
      expect(whileOut, "the reads of the tree and its views while the edit is out").toBe(0);
      if (feed === "up") {
        await tick(150, 10);
        await commentsEvent(rig);
      }
    },
  };
}

/** The stage drawn as a slot of its own while a row's way runs, so the row's own stage, which it
 *  takes down and draws again around the way, is not the one the way drew. */
function stageSlot(rig: Rig): void {
  rig.slots.push(
    renderHook(
      () => {
        useIssueTree(issue);
        return useTreeRead(issue);
      },
      { wrapper: turnedStage(rig.client, rig.on) }
    )
  );
}

/** The live feed back with its gap replayed whole while the reader is away, which asks for a
 *  refused read, then the reader coming back while a write is out and leaving before the write
 *  settles: no read starts while the write is out, since the hold keeps the new observer from
 *  reading, and the read stays owed. */
const BACK_AND_GONE_WHILE_WAITING: RuleWay = {
  words:
    "the live feed back with its gap replayed whole, then the reader coming back while a write is out and leaving before it settles",
  does: async (rig) => {
    await restartFeed(rig);
    startFeedOn(rig.sockets, true);
    await tick(300, 10);
    const settleWrite = holdWrite(rig);
    await tick(10, 10);
    comeBack(rig);
    await tick(500, 10);
    leave(rig);
    await tick(100, 10);
    act(() => settleWrite());
    await tick(50, 10);
  },
};

/** The reader coming back, pressing Retry on the refused tree's failed line while a write is out,
 *  which is sent at once and refused, and leaving before the write settles: the settle while the
 *  reader is away leaves one read owed, which the reader coming back reads. */
const RETRY_GONE_WHILE_WAITING: RuleWay = {
  words:
    "the reader coming back, pressing Retry while a write is out and leaving before it settles",
  does: async (rig) => {
    comeBack(rig);
    await tick(300, 10);
    const settleWrite = holdWrite(rig);
    await tick(10, 10);
    act(() => {
      void stageOf(rig).result.current.refetch();
    });
    await tick(300, 10);
    leave(rig);
    await tick(100, 10);
    act(() => settleWrite());
    await tick(50, 10);
  },
};

/** Two writes out together, each landing with no answer (`answerlessWrite`), the second 200 ms
 *  after the first: each makes the write store refresh every query the app holds while the
 *  other, or itself, is still out. */
function twoAnswerlessWrites(feed: "down" | "up"): RuleWay {
  return {
    words: `two writes out together that each land with no answer, 200 ms apart, with the live feed ${feed} and no event`,
    does: async (rig) => {
      if (feed === "down") await feedDown(rig);
      const second = Promise.withResolvers<void>();
      const write = storeWrite(rig.client, KEY, async () => {
        await second.promise;
        return null;
      });
      void write.mutate();
      await tick(10, 10);
      await writeSettles(() => answerlessWrite(rig).mutate(), 200);
      await tick(200, 10);
      act(() => second.resolve());
      await tick(50, 10);
    },
    ends: treeHoldsComment("settled"),
  };
}

/** The stage mounting as the reader opens the issue while a write that lands with no answer
 *  (`answerlessWrite`) is out: its first tree read is sent at once, and the write's settle reads
 *  it once more. */
function openDuringAnswerlessWrite(feed: "down" | "up"): RuleWay {
  return {
    words: `the reader opening the issue while a write that lands in 200 ms with no answer is out, with the live feed ${feed}`,
    does: async (rig) => {
      if (feed === "down") await feedDown(rig);
      await writeSettles(
        () => answerlessWrite(rig).mutate(),
        200,
        async () => {
          stageSlot(rig);
        }
      );
    },
  };
}

/** The stage mounting as the reader opens the issue while a write of another issue is out, which
 *  holds nothing: its first tree read is sent at once, and an event of this issue 500 ms on reads
 *  it once more. */
const OPEN_DURING_WRITE_WITH_EVENT: RuleWay = {
  words:
    "the reader opening the issue while a write of another issue is out, an event of the issue 500 ms into the wait, the write then settling",
  does: async (rig) => {
    const settleWrite = holdWrite(rig, { of: OTHER_KEY });
    await tick(10, 10);
    stageSlot(rig);
    await tick(500, 10);
    await commentsEvent(rig);
    await tick(500, 10);
    act(() => settleWrite());
    await tick(50, 10);
  },
};

/** The reader opening the issue while a write is out, and the row's first read then - the tree's
 *  as the stage is drawn, a view's as its filter is first pressed on - which is sent at once,
 *  whatever is out: of `of`, the issue the page draws or another. A write of this issue is owed
 *  one more read of what it holds as it settles; one of another issue, pressed on another page,
 *  holds nothing. */
function firstReadDuringWrite(of: string): RuleWay {
  return {
    words: `the reader opening the issue while a write of ${of === KEY ? "it" : "another issue"} is out and reading it first then, the write then settling`,
    does: async (rig) => {
      if (rig.stage !== undefined) {
        stageOf(rig).unmount();
        rig.stage = undefined;
        await tick(100, 10);
      }
      const settleWrite = holdWrite(rig, { of });
      await tick(10, 10);
      const before = ruleReads(rig);
      if (rig.read === "tree") {
        stageSlot(rig);
      } else {
        mountStage(rig);
        comeBack(rig);
      }
      await tick(300, 10);
      expect(ruleReads(rig) - before, "the first read, sent while the write is out").toBe(1);
      act(() => settleWrite());
      await tick(50, 10);
    },
  };
}

/** The reader coming back to a read while a write of the issue is out - the tree's stage drawn,
 *  the view's filter pressed on - and staying: the write holds the read the new observer would
 *  send, and it is read once the write settles. */
const COME_BACK_DURING_WRITE: RuleWay = {
  words: "the reader coming back to it while a write of the issue is out, the write then settling",
  does: async (rig) => {
    const settleWrite = holdWrite(rig);
    await tick(10, 10);
    const before = ruleReads(rig);
    if (rig.read === "tree") stageSlot(rig);
    else comeBack(rig);
    await tick(300, 10);
    expect(ruleReads(rig) - before, "the reads while the write is out").toBe(0);
    act(() => settleWrite());
    await tick(50, 10);
  },
};

/** The reader's Retry of the refused tree while a write of the issue is out, with TanStack's own
 *  retry of it once more a second after Dispatch refuses it, and the retry answered; an event of
 *  the issue 300 ms into that second, and the write settling 300 ms later, before the retry. The
 *  Retry and its retry are one read out, sent at once whatever the write holds: the event and the
 *  write's settle ask for one read more once that read lands, as for any read out. */
const RETRY_RETRIED_DURING_WRITE: RuleWay = {
  words:
    "the reader's Retry while a write of the issue is out, refused and retried a second later, an event of the issue during that second and the write settling before the retry",
  does: async (rig) => {
    rig.client.setQueryDefaults(conversationTreeKey(KEY), { retry: 1, retryDelay: 1_000 });
    stageOf(rig).rerender();
    const settleWrite = holdWrite(rig);
    await tick(10, 10);
    act(() => {
      void stageOf(rig).result.current.refetch();
    });
    await tick(10, 10);
    rig.refusing = false;
    await tick(300, 10);
    await commentsEvent(rig);
    await tick(300, 10);
    act(() => settleWrite());
    await tick(50, 10);
  },
};

/** A write of the issue out while the tree is marked stale, and a second write of the issue made
 *  in the write store's telling of the first's landing (`onIssueWrite`), straight into the store
 *  as a write made outside any mutation is (a composer's send, a pin): nothing of the tree is read
 *  until the second settles too, and then it is read once. */
const SECOND_WRITE_AT_SETTLE: RuleWay = {
  words:
    "a write out while the tree is marked stale, a second write pressed as the first settles, the second then settling",
  does: async (rig) => {
    const settleFirst = holdWrite(rig);
    await tick(10, 10);
    act(() => {
      void rig.client.invalidateQueries({ exact: true, queryKey: conversationTreeKey(KEY) });
    });
    await tick(300, 10);
    let settleSecond: (() => void) | undefined;
    const stop = writesOf(rig.client).onIssueWrite(KEY, (phase) => {
      if (settleSecond !== undefined || phase !== "landed") return;
      const answer = Promise.withResolvers<void>();
      void pressWrite(rig.client, KEY, () => answer.promise, "A second write of the page").catch(
        () => {}
      );
      settleSecond = answer.resolve;
    });
    const before = treeAndViewReads();
    act(() => settleFirst());
    await tick(300, 10);
    stop();
    expect(settleSecond, "the second write was pressed").toBeDefined();
    expect(
      treeAndViewReads() - before,
      "the reads of the tree and its views while the second write is out"
    ).toBe(0);
    act(() => settleSecond?.());
    await tick(50, 10);
  },
};

/** The margin's resolve of the comment (`useMarginItems.ts`), answered by Dispatch in `latency`:
 *  its `onMutate` writes the resolve into the comments read before Dispatch holds it, its
 *  `onSuccess` invalidates the comments and the issue once Dispatch answers, and the resolve's
 *  event refreshes the comments 150 ms later, as the live feed's rule does. */
function marginResolve(latency: number): RuleWay {
  return {
    words: `a resolve from the margin answered in ${latency} ms, with its event`,
    does: async (rig) => {
      const resolve = marginResolveOn(rig.client, async () => {
        rig.held = { ...comment, resolved: true };
        await Bun.sleep(latency);
      });
      // The clock moves 10 ms at a time until Dispatch has answered, then 150 ms to the event.
      let answered = false;
      void resolve.mutate().then(() => {
        answered = true;
      });
      for (let waited = 0; !answered && waited <= latency + 1_000; waited += 10) {
        await tick(10, 10);
      }
      expect(answered, "Dispatch answered the resolve").toBe(true);
      await tick(150, 10);
      await commentsEvent(rig);
    },
    ends: treeHoldsComment("settled"),
  };
}

/** The reader's Retry: the outline's failed line's for the tree, a new observer's `refetch` for a
 *  view (`WAYS.retry`). */
function pressRetry(rig: Rig): void {
  act(() => {
    if (rig.read === "tree") void stageOf(rig).result.current.refetch();
    else void new QueryObserver(rig.client, viewQuery(KEY, rig.read)).refetch();
  });
}

/** A write held while the row's way runs, begun as `start` begins it, and a step `during` it:
 *  `during` reads the row's query `whileOut` times before the write settles, and the write then
 *  settles. */
function duringWrite(
  words: string,
  start: (rig: Rig) => () => void,
  during: (rig: Rig) => Promise<void>,
  whileOut: number
): RuleWay {
  return {
    words,
    does: async (rig) => {
      const settleWrite = start(rig);
      await tick(10, 10);
      const before = ruleReads(rig);
      await during(rig);
      await tick(300, 10);
      expect(ruleReads(rig) - before, "the reads while the write is out").toBe(whileOut);
      act(() => settleWrite());
      await tick(50, 10);
    },
  };
}

/** A write of another issue (`OTHER_KEY`) pressed while the reader is on another
 *  page, the reader then coming back to the read while it is out (the tree's stage drawn as a
 *  slot), and `then` 1.5 s on, past the second after the reads the return began. */
function backDuringOtherWrite(
  words: string,
  then: (rig: Rig) => Promise<void>,
  whileOut: number
): RuleWay {
  return {
    words,
    does: async (rig) => {
      const settleWrite = holdWrite(rig, { of: OTHER_KEY });
      await tick(10, 10);
      const before = ruleReads(rig);
      stageSlot(rig);
      await tick(1_500, 10);
      await then(rig);
      await tick(300, 10);
      expect(ruleReads(rig) - before, "the reads while the write is out").toBe(whileOut);
      act(() => settleWrite());
      await tick(50, 10);
    },
  };
}

/** The tree's Retry on a stage drawn as a slot (`backDuringOtherWrite`). */
async function slotRetry(rig: Rig): Promise<void> {
  const slot = rig.slots.at(-1) as RuleStage | undefined;
  act(() => {
    void slot?.result.current.refetch();
  });
}

/** A composer send of the margin, which names its issue by the request it sends
 *  (`/api/v1/issues/{key}/comments`, as `held-sends.ts` sends one): pressed with the stage drawn;
 *  the reader leaves 10 s in; an event of the issue 20 s in; the send past its deadline at 30 s,
 *  the reader still away, and still the one write of the write store; the reader back at 35 s; an
 *  event at 40 s; the send settling at 60 s. One write of the issue: nothing of the tree is read
 *  until it settles. */
const MARGIN_SEND_PAST_DEADLINE_AWAY: RuleWay = {
  words:
    "a send from the margin pressed on the page that goes past its deadline 30 s in while the reader is away, an event 20 s and 40 s in, the reader back at 35 s and the send settling at 60 s",
  does: async (rig) => {
    const before = ruleReads(rig);
    const settle = holdWrite(rig, { of: null, path: `/api/v1/issues/${KEY}/comments` });
    await tick(10_000, 100);
    leave(rig);
    await tick(10_000, 100);
    await commentsEvent(rig);
    await tick(15_000, 100);
    comeBack(rig);
    await tick(5_000, 100);
    await commentsEvent(rig);
    await tick(19_000, 100);
    expect(ruleReads(rig) - before, "the reads while the send is out").toBe(0);
    act(() => settle());
    await tick(50, 10);
  },
};

/** An event of the issue 200 ms after the comments were read again, the tree's last read past its
 *  second: the scheduler holds the comments read the event owes to a second after the last one
 *  began, and the tree read the event asks for is not held. One event, read once. */
const EVENT_SOON_AFTER_COMMENTS_READ: RuleWay = {
  words: "an event of the issue 200 ms after the comments were read again, no write out",
  does: async (rig) => {
    await act(async () => {
      await rig.client.refetchQueries({ queryKey: ["comments", KEY] });
    });
    await tick(200, 10);
    await commentsEvent(rig);
  },
};

/** An event of the issue that posts a comment 200 ms after the comments were read again, while
 *  that read is still out: Dispatch answers each comments read `latency` ms after it was sent,
 *  with the comments it holds as it answers. The scheduler owes the comments one more read once
 *  the one out lands, and that read begins after the tree read the event asked for. One event,
 *  read once. */
function eventWhileCommentsOut(latency: number): RuleWay {
  return {
    words: `an event of the issue that posts a comment 200 ms into a read of the comments again, which Dispatch answers ${latency} ms after it was sent`,
    does: async (rig) => {
      (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => {
        await Bun.sleep(latency);
        return [rig.held, ...rig.posted];
      });
      await act(async () => {
        void rig.client.refetchQueries({ queryKey: ["comments", KEY] });
      });
      await tick(200, 10);
      rig.posted.push(postedComment);
      await commentsEvent(rig);
    },
  };
}

/** A write pressed on another page while the reader is away (`away`), with, where `onPage` is
 *  given, a write of that issue pressed on the page before the reader left and settling once the
 *  second is out; the reader back; an event of the issue while the second is out, read at once,
 *  since the second is no write of this issue; the second then settling. */
function eventDuringWriteAway(
  words: string,
  onPage: string | undefined,
  away: (rig: Rig) => () => void
): RuleWay {
  return {
    words,
    does: async (rig) => {
      const settleFirst = onPage === undefined ? undefined : holdWrite(rig, { of: onPage });
      await tick(500, 10);
      leave(rig);
      await tick(500, 10);
      const settleSecond = away(rig);
      await tick(500, 10);
      if (settleFirst !== undefined) act(() => settleFirst());
      await tick(500, 10);
      comeBack(rig);
      await quiet(rig);
      const before = ruleReads(rig);
      await commentsEvent(rig);
      await tick(1_500, 10);
      expect(ruleReads(rig) - before, "the reads while the second write is out").toBe(1);
      act(() => settleSecond());
      await tick(50, 10);
    },
  };
}

/** An event of the issue that keeps a decision 200 ms into a read of the decisions again, which
 *  Dispatch answers `latency` ms after it was sent, with the decisions it holds as it answers -
 *  kept by the event. The event refreshes what a Keep's names (`decision.kept`): the decisions read,
 *  which is owed one more read once the one out lands, and the tree. One event, read once. */
function eventWhileDecisionsOut(latency: number): RuleWay {
  return {
    words: `an event of the issue that keeps a decision 200 ms into a read of the decisions again, which Dispatch answers ${latency} ms after it was sent`,
    does: async (rig) => {
      let kept = false;
      (api.getIssueDecisions as Mock<typeof api.getIssueDecisions>).mockImplementation(async () => {
        await Bun.sleep(latency);
        return { decisions: [], document: kept ? "kept" : null, entries: [], open_questions: [] };
      });
      await act(async () => {
        void rig.client.refetchQueries({ queryKey: ["decisions", KEY] });
      });
      await tick(200, 10);
      kept = true;
      await act(async () => {
        refreshQueries(rig.client, keepEventTargets());
      });
    },
  };
}

/** An event of the issue that posts a comment 200 ms into a read of the comments again, which
 *  Dispatch refuses `latency` ms after it was sent; it answers every later comments read, each as
 *  late. The scheduler owes the comments one more read once the one out lands, refused or not,
 *  and that read begins after the tree read the event asked for. One event, read once. */
function eventWhileCommentsRefused(latency: number): RuleWay {
  return {
    words: `an event of the issue that posts a comment 200 ms into a read of the comments again, which Dispatch refuses ${latency} ms after it was sent`,
    does: async (rig) => {
      let refused = false;
      (api.listComments as Mock<typeof api.listComments>).mockImplementation(async () => {
        await Bun.sleep(latency);
        if (!refused) {
          refused = true;
          throw new Error("the comments read was refused");
        }
        return [rig.held, ...rig.posted];
      });
      await act(async () => {
        void rig.client.refetchQueries({ queryKey: ["comments", KEY] });
      });
      await tick(200, 10);
      rig.posted.push(postedComment);
      await commentsEvent(rig);
    },
  };
}

/** The reader leaving the issue and coming back to it while a write of the issue is out: a new
 *  load of the stage, with the filter that was on, which resets the views (`resetViews`) and reads
 *  the open one at once, and once more as the write settles. */
const RELOAD_DURING_WRITE: RuleWay = {
  words: "the reader leaving the issue and coming back to it while a write of the issue is out",
  does: async (rig) => {
    const settleWrite = holdWrite(rig);
    await tick(10, 10);
    const before = ruleReads(rig);
    stageOf(rig).unmount();
    rig.stage = undefined;
    await tick(100, 10);
    mountStage(rig);
    await tick(300, 10);
    expect(ruleReads(rig) - before, "the load's read, sent while the write is out").toBe(1);
    act(() => settleWrite());
    await tick(50, 10);
  },
};

/** A write of the issue held open, and a second whose own callback, as Dispatch answers it in 200
 *  ms, drops the views (`dropViews`), as a Settle, a Keep reading the decisions again and a Change
 *  do; then, the first still out, the reader leaving the issue and coming back to it: a new load
 *  of the stage, with the filter that was on, which resets the views (`resetViews`) and reads the
 *  open one at once, and once more as the first write settles. */
const RELOAD_AFTER_DROP_DURING_WRITE: RuleWay = {
  words:
    "a write of the issue whose callback drops the views while a second write of the issue is out, then the reader leaving the issue and coming back to it while the second is out",
  does: async (rig) => {
    const settleWrite = holdWrite(rig);
    const drop = storeWrite(rig.client, KEY, () => Bun.sleep(200), {
      onSuccess: () => {
        dropViews(rig.client, KEY);
      },
    });
    await writeSettles(() => drop.mutate(), 200);
    await tick(10, 10);
    const before = ruleReads(rig);
    stageOf(rig).unmount();
    rig.stage = undefined;
    await tick(100, 10);
    mountStage(rig);
    await tick(300, 10);
    expect(ruleReads(rig) - before, "the load's read, sent while the write is out").toBe(1);
    act(() => settleWrite());
    await tick(50, 10);
  },
};

/** One write of the issue whose own callback, once Dispatch has answered it in 200 ms, drops the
 *  views (`dropViews`) and then keeps running until the row lets it go; meanwhile the reader
 *  leaves the issue and comes back to it. The write landed before its callback ran, which so
 *  drops the views with no write of the issue out: the open view is read at once for the drop,
 *  and the load resets the views (`resetViews`) and reads it at once again. */
const RELOAD_AFTER_DROP_IN_ONE_WRITE: RuleWay = {
  words:
    "a write of the issue whose callback drops the views and keeps running, then the reader leaving the issue and coming back to it while it runs",
  does: async (rig) => {
    const dropped = Promise.withResolvers<void>();
    const letGo = Promise.withResolvers<void>();
    const write = storeWrite(rig.client, KEY, () => Bun.sleep(200), {
      onSuccess: async () => {
        dropViews(rig.client, KEY);
        dropped.resolve();
        await letGo.promise;
      },
    });
    const settled = write.mutate();
    await tick(300, 10);
    await dropped.promise;
    const before = ruleReads(rig);
    stageOf(rig).unmount();
    rig.stage = undefined;
    await tick(100, 10);
    mountStage(rig);
    await tick(300, 10);
    expect(ruleReads(rig) - before, "the load's read, sent while the callback runs").toBe(1);
    letGo.resolve();
    await act(async () => {
      await settled;
    });
    await tick(50, 10);
  },
};

/** A write of the issue whose own callback, as Dispatch answers it 200 ms on, does what a Settle,
 *  a Keep reading the decisions again and a Change do - code that calls `dropViews` there, which
 *  no load does: invalidates the coverage, refreshes the tree and drops the views. TanStack runs
 *  the callback once the write store has landed the write, which so holds nothing by then: the
 *  tree and the open view are read once, at once, and nothing of them while the write is out,
 *  counted as the write store lands it. With `press`, the reader presses the row's filter on, its
 *  view never read, halfway through the write: that first read is sent at once, read once more as
 *  the write settles, and once more for the drop. With `dropOnly`, the callback drops the views and
 *  nothing else, and the tree is not read. */
function writeDroppingViews({
  dropOnly = false,
  press = false,
}: {
  dropOnly?: boolean;
  press?: boolean;
}): RuleWay {
  return {
    words: `a write of the issue whose callback, as Dispatch answers it in 200 ms, ${dropOnly ? "drops the views and nothing else" : "invalidates the coverage, refreshes the tree and drops the views"}${press ? ", the reader pressing the filter on while it is out" : ""}`,
    does: async (rig) => {
      const before = treeAndViewReads();
      const trees = treeReadsOf();
      let whileOut: number | undefined;
      const stop = writesOf(rig.client).onIssueWrite(KEY, (phase) => {
        if (phase === "landed") whileOut = treeAndViewReads() - before;
      });
      const write = storeWrite(rig.client, KEY, () => Bun.sleep(200), {
        onSuccess: () => {
          if (!dropOnly) {
            void rig.client.invalidateQueries({ queryKey: issueCoverageQuery(KEY).queryKey });
            refreshQueries(rig.client, [{ exact: true, queryKey: conversationTreeKey(KEY) }]);
          }
          dropViews(rig.client, KEY);
        },
      });
      await writeSettles(() => write.mutate(), 200, press ? async () => comeBack(rig) : undefined);
      stop();
      expect(whileOut, "the reads of the tree and its views while the write is out").toBe(
        press ? 1 : 0
      );
      await quiet(rig);
      expect(treeReadsOf() - trees, "the reads of the tree").toBe(dropOnly ? 0 : 1);
    },
  };
}

const WAYS = {
  mount: {
    words: "a new surface mounting",
    does: async (rig) => {
      // The filter pressed off and on.
      if (rig.read !== "tree" && rig.on.preset === rig.read) {
        leave(rig);
        comeBack(rig);
        await tick(300, 10);
        return;
      }
      rig.slots.push(
        renderHook(() => useIssueTree(issue), {
          wrapper: ({ children }: { children: ReactNode }) => (
            <RoutedQueries client={rig.client}>{children}</RoutedQueries>
          ),
        })
      );
    },
  },
  focusUp: {
    words: "a window focus with the live feed up",
    does: async (rig) => {
      expect(liveFeedUp(rig.client), "the live feed is up").toBe(true);
      act(() => focusWindow());
    },
  },
  focusDown: {
    words: "a window focus with the live feed down",
    does: async (rig) => {
      await feedDown(rig);
      act(() => focusWindow());
    },
  },
  online: {
    words: "the network back",
    does: async () => {
      act(() => {
        onlineManager.setOnline(false);
        onlineManager.setOnline(true);
      });
    },
  },
  // `feedBackAfterRestart`, on the row's clock.
  feedWhole: {
    words: "the live feed back with its gap replayed whole",
    does: async (rig) => {
      await restartFeed(rig);
      startFeedOn(rig.sockets, true);
    },
  },
  feedPart: {
    words: "the live feed back with its gap replayed in part",
    does: async (rig) => {
      await restartFeed(rig);
      startFeedOn(rig.sockets, false);
    },
  },
  event: {
    words: "an event of the issue",
    does: commentsEvent,
  },
  retry: {
    words: "the reader's Retry",
    // An observer's `refetch`, which joins a read out that holds no answer: the tree's is the
    // outline's failed line's, and the Spec view's one of a new observer of its query. The app's
    // `refetchQueries` is no Retry: it reads a query that is out once more when it settles.
    does: async (rig) => {
      act(() => {
        if (rig.read === "tree") void stageOf(rig).result.current.refetch();
        else void new QueryObserver(rig.client, viewQuery(KEY, rig.read)).refetch();
      });
    },
  },
  resolve200: marginResolve(200),
  resolve1200: marginResolve(1_200),
  resolveDown200: marginResolveDown(200),
  resolveDown1200: marginResolveDown(1_200),
  resolveRefusedUp: refusedResolve("up"),
  resolveRefusedDown: refusedResolve("down"),
  setDataUp: issueWriteSet("up"),
  setDataDown: issueWriteSet("down"),
  landedBlankUp: answerlessLanding("up"),
  landedBlankDown: answerlessLanding("down"),
  resolveRefusedUpPosted: refusedResolve("up", true),
  landedBlankBesideUp: answerlessBesideWrite("up"),
  landedBlankBesideDown: answerlessBesideWrite("down"),
  landedBlankBesideKeepDown: answerlessBesideWrite("down", { kept: true }),
  landedBlankBesideFeedPart: answerlessBesideWrite("down", { feedBackPart: true }),
  landedBlankBesideFocus: answerlessBesideWrite("down", { focus: true }),
  eventAfterWaitedRead: EVENT_AFTER_WAITED_READ,
  composerEditDown: composerEdit("down"),
  composerEditUp: composerEdit("up"),
  focusDownWriting: focusWhileWriting(
    "a window focus with the live feed down while a write is out, the write then settling with the feed still down"
  ),
  focusHeldFeedWhole: focusWhileWriting(
    "a window focus with the live feed down while a write is out, the feed then back whole before the write settles",
    (rig) => feedStartsAgain(rig, true)
  ),
  focusHeldFeedPart: focusWhileWriting(
    "a window focus with the live feed down while a write is out, the feed then back in part before the write settles",
    (rig) => feedStartsAgain(rig, false)
  ),
  focusFeedRefused: {
    words: "a window focus after Dispatch refused the live feed it had started",
    does: async (rig) => {
      const socket = rig.sockets.latest();
      const { sub } = socket.subscribed("events");
      act(() =>
        socket.receive({ type: "error", sub, code: "UNAVAILABLE", message: "the feed is refused" })
      );
      expect(liveFeedUp(rig.client), "the live feed is down").toBe(false);
      act(() => focusWindow());
    },
  },
  reload: {
    words: "the reader leaving the issue and coming back to it",
    // A new load of the stage, with the filter that was on: the page mounts the stage once for
    // each issue it opens, and the stage's mount drops every view a past load held
    // (`resetViews`).
    does: async (rig) => {
      stageOf(rig).unmount();
      rig.stage = undefined;
      await tick(100, 10);
      mountStage(rig);
    },
  },
  backAndGoneWhileWaiting: BACK_AND_GONE_WHILE_WAITING,
  retryGoneWhileWaiting: RETRY_GONE_WHILE_WAITING,
  twoAnswerlessUp: twoAnswerlessWrites("up"),
  twoAnswerlessDown: twoAnswerlessWrites("down"),
  openDuringAnswerlessUp: openDuringAnswerlessWrite("up"),
  openDuringAnswerlessDown: openDuringAnswerlessWrite("down"),
  openDuringWriteWithEvent: OPEN_DURING_WRITE_WITH_EVENT,
  firstReadDuringOwnWrite: firstReadDuringWrite(KEY),
  firstReadDuringOtherWrite: firstReadDuringWrite(OTHER_KEY),
  secondWriteAtSettle: SECOND_WRITE_AT_SETTLE,
  landedBlankQuietDown: answerlessLanding("down", { resolves: false }),
  comeBackDuringWrite: COME_BACK_DURING_WRITE,
  retryRetriedDuringWrite: RETRY_RETRIED_DURING_WRITE,
  retryDuringOwnWrite: duringWrite(
    "the reader's Retry while a write of the issue is out, the write then settling",
    (rig) => holdWrite(rig),
    async (rig) => pressRetry(rig),
    1
  ),
  eventDuringOwnWrite: duringWrite(
    "an event of the issue while a write of it is out, the write then settling",
    (rig) => holdWrite(rig),
    commentsEvent,
    0
  ),
  eventAwayDuringOtherWrite: {
    words:
      "an event of the issue while the reader is away and a write of another issue, pressed on another page, is out, the write then settling",
    does: async (rig) => {
      const settleWrite = holdWrite(rig, { of: OTHER_KEY });
      await tick(10, 10);
      await commentsEvent(rig);
      await tick(300, 10);
      act(() => settleWrite());
      await tick(50, 10);
    },
  },
  eventBackDuringOtherWrite: backDuringOtherWrite(
    "the reader coming back while a write of another issue, pressed on another page, is out, an event of the issue 1.5 s on, the write then settling",
    commentsEvent,
    1
  ),
  retryBackDuringOtherWrite: backDuringOtherWrite(
    "the reader coming back while a write of another issue, pressed on another page, is out, the reader's Retry 1.5 s on, the write then settling",
    slotRetry,
    1
  ),
  marginSendPastDeadlineAway: MARGIN_SEND_PAST_DEADLINE_AWAY,
  eventSoonAfterCommentsRead: EVENT_SOON_AFTER_COMMENTS_READ,
  eventWhileCommentsOut600: eventWhileCommentsOut(600),
  eventWhileCommentsOut1500: eventWhileCommentsOut(1_500),
  eventDuringOtherIssueWritesByPress: eventDuringWriteAway(
    "a write naming another issue pressed on the page, a second naming that issue too pressed on another page while it is out, the first settling, the reader back and an event of the issue while the second is out",
    OTHER_KEY,
    (rig) => holdWrite(rig, { of: OTHER_KEY })
  ),
  eventDuringOtherIssueWriteByPath: eventDuringWriteAway(
    "a write naming another issue pressed on the page, a second naming it by its request's path pressed on another page while it is out, the first settling, the reader back and an event of the issue while the second is out",
    OTHER_KEY,
    (rig) => holdWrite(rig, { of: null, path: `/api/v1/issues/${OTHER_KEY}/comments` })
  ),
  eventDuringLongerKeyIssueWriteAway: eventDuringWriteAway(
    "a write naming an issue whose key begins with this issue's, pressed on another page, the reader back and an event of the issue while it is out",
    undefined,
    (rig) => holdWrite(rig, { of: `${KEY}0` })
  ),
  eventDuringIssuelessWriteAway: eventDuringWriteAway(
    "a write that names no issue, pressed on another page, the reader back and an event of the issue while it is out",
    undefined,
    (rig) => holdWrite(rig, { of: null })
  ),
  eventWhileDecisionsOut600: eventWhileDecisionsOut(600),
  eventWhileDecisionsOut1500: eventWhileDecisionsOut(1_500),
  writeDroppingViews: writeDroppingViews({}),
  firstPressDuringWriteDroppingViews: writeDroppingViews({ press: true }),
  writeDroppingViewsOnly: writeDroppingViews({ dropOnly: true }),
  eventWhileCommentsRefused600: eventWhileCommentsRefused(600),
  reloadDuringWrite: RELOAD_DURING_WRITE,
  reloadAfterDropDuringWrite: RELOAD_AFTER_DROP_DURING_WRITE,
  reloadAfterDropInOneWrite: RELOAD_AFTER_DROP_IN_ONE_WRITE,
} satisfies Record<string, RuleWay>;

/** How many reads of a query a way starts: a count, or the least and the most. */
type RuleReads = number | { readonly atLeast: number; readonly atMost: number };

/** A write and the event it raises: the write's own read, and one more for its event at most. */
const ONE_OR_TWO: RuleReads = { atLeast: 1, atMost: 2 };

/** One cell of the rule: a read in a state, observed or not, the way, and the reads of that query
 *  the way starts (`now`) and, for a read not observed, the reads the reader coming back to it
 *  then starts (`back`). */
type ReadRule = {
  readonly read: RuleRead;
  readonly state: keyof typeof STATES;
  readonly way: keyof typeof WAYS;
  readonly now: RuleReads;
} & ({ readonly observed: true } | { readonly observed: false; readonly back: RuleReads });

/** The words of a count of reads. */
function readsWords(reads: RuleReads): string {
  if (typeof reads !== "number") return `${reads.atLeast} to ${reads.atMost} times`;
  return reads === 1 ? "once" : `${reads} times`;
}

/** A row's own words, its title. */
function ruleWords(rule: ReadRule): string {
  const read = RULE_READ_WORDS[rule.read];
  const observed =
    rule.read === "tree"
      ? rule.observed
        ? "the stage mounted"
        : "the reader on another page"
      : rule.observed
        ? "its filter on"
        : "its filter off";
  const words = `${read}, ${STATES[rule.state].words}, ${observed}: ${WAYS[rule.way].words} reads it ${readsWords(rule.now)}`;
  if (rule.observed) return words;
  const back = rule.read === "tree" ? "the reader coming back" : "its filter pressed on";
  return `${words}, and ${back} then reads it ${readsWords(rule.back)}`;
}

/** Whether `sent` reads are what `expected` says. */
function expectReads(sent: number, expected: RuleReads, message: string): void {
  if (typeof expected === "number") {
    expect(sent, message).toBe(expected);
    return;
  }
  expect(sent, message).toBeGreaterThanOrEqual(expected.atLeast);
  expect(sent, message).toBeLessThanOrEqual(expected.atMost);
}

/** Runs one row on a fresh client and stage: the read brought to its state, a quiet second and
 *  more, the way, and the reads of that query until the page is quiet again; for a read not
 *  observed, then the reader coming back to it, and the reads until the page is quiet. */
async function holdsRule(rule: ReadRule): Promise<void> {
  const rig = openRig(rule.read);
  const way: RuleWay = WAYS[rule.way];
  try {
    await STATES[rule.state].reach(rig, rule.observed);
    await quiet(rig);
    const before = ruleReads(rig);
    await way.does(rig);
    // A read held open answers once the way is done, and whatever the way asked for after it
    // then begins.
    await tick(50, 10);
    act(() => {
      rig.out?.resolve();
      rig.out = undefined;
    });
    await quiet(rig);
    expectReads(ruleReads(rig) - before, rule.now, "the reads the way starts");
    way.ends?.(rig);
    if (rule.observed) return;
    for (const slot of rig.slots.splice(0)) slot.unmount();
    const away = ruleReads(rig);
    comeBack(rig);
    await quiet(rig);
    expectReads(ruleReads(rig) - away, rule.back, "the reads coming back starts");
  } finally {
    rig.close();
  }
}

// The rule for when a read of the tree, or of a view, starts: one row for each cell, and a way of
// starting a read found later is a new row here.
//
// A read refused by Dispatch, with or without an answer held, stays refused until the reader's
// Retry, an event of the issue, the live feed coming back (its gap replayed whole, or the event
// feed's refresh of every query on an incomplete replay), or a window focus while the feed is
// down. No other way starts a read of it: no new surface (a slot drawn, a surface opened, the
// reader coming back to the issue), no filter press, no network return, no focus while the feed
// is up. A read that holds an answer is read again by an event or a write of the issue (once its
// observer is there, if nothing observed it then); a read never made is made when something
// first observes it.
//
// Each row is a read (the tree, or the Spec view), its state, whether it is observed (the stage
// mounted, or the reader on another page with the stage gone and the shell's event feed staying;
// for the view, its filter on or off), the way, and how many reads of that query the way starts
// and, for a read not observed, how many the reader coming back then starts (the stage mounted
// again; for the view, its filter pressed on). The live feed is up until a way takes it down.
// "A new surface mounting" is a slot of the page drawn outside the stage (`useIssueTree`), and
// for the Spec view with its filter on, the filter pressed off and on. An event is the event
// feed's refresh of the issue's comments. The reader's Retry is an observer's `refetch`: the
// outline's failed line's for the tree, and, since the page draws no Retry of the Spec view's
// own, a new observer's of its query. An in-flight read answers once the way is done.
//
// The rows run on the fake clock (`jest.useFakeTimers()`, which moves `setTimeout`, `Bun.sleep`,
// `Date.now` and `performance.now` together), moved through `tick` in steps that let every
// answer's promises and React's updates run, so a second the scheduler waits costs no real time.
//
// Cells left out, as no row can stand in them:
// - never read and observed: a stage reads the tree as it mounts, and a filter on reads its view;
// - marked stale while nothing observed it, and observed: an observer reads a stale answer as it
//   comes, so the read stands answered by then;
// - the tree in flight and not observed: a read whose abort signal TanStack handed out is given up
//   once its last observer goes, so the stage leaving while the tree's read is out leaves the tree
//   never read. The Spec view's read keeps going with its filter off, since the page's model
//   still watches it;
// - the reader's Retry while the read is not observed: a Retry is a control of the drawn stage.
const READ_RULES: readonly ReadRule[] = [
  { read: "tree", state: "never", observed: false, way: "mount", now: 1, back: 0 },
  { read: "tree", state: "never", observed: false, way: "focusUp", now: 0, back: 1 },
  { read: "tree", state: "never", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "tree", state: "never", observed: false, way: "online", now: 0, back: 1 },
  { read: "tree", state: "never", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "tree", state: "never", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "tree", state: "never", observed: false, way: "event", now: 0, back: 1 },
  { read: "tree", state: "answered", observed: true, way: "mount", now: 0 },
  { read: "tree", state: "answered", observed: true, way: "focusUp", now: 0 },
  { read: "tree", state: "answered", observed: true, way: "focusDown", now: 0 },
  { read: "tree", state: "answered", observed: true, way: "online", now: 0 },
  { read: "tree", state: "answered", observed: true, way: "feedWhole", now: 0 },
  { read: "tree", state: "answered", observed: true, way: "feedPart", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "event", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "retry", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "resolve200", now: ONE_OR_TWO },
  { read: "tree", state: "answered", observed: true, way: "resolve1200", now: ONE_OR_TWO },
  { read: "tree", state: "answered", observed: false, way: "mount", now: 0, back: 0 },
  { read: "tree", state: "answered", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "tree", state: "answered", observed: false, way: "focusDown", now: 0, back: 0 },
  { read: "tree", state: "answered", observed: false, way: "online", now: 0, back: 0 },
  { read: "tree", state: "answered", observed: false, way: "feedWhole", now: 0, back: 0 },
  { read: "tree", state: "answered", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "tree", state: "answered", observed: false, way: "event", now: 0, back: 1 },
  { read: "tree", state: "stale", observed: false, way: "mount", now: 1, back: 0 },
  { read: "tree", state: "stale", observed: false, way: "focusUp", now: 0, back: 1 },
  { read: "tree", state: "stale", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "tree", state: "stale", observed: false, way: "online", now: 0, back: 1 },
  { read: "tree", state: "stale", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "tree", state: "stale", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "tree", state: "stale", observed: false, way: "event", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: true, way: "mount", now: 0 },
  { read: "tree", state: "refused", observed: true, way: "focusUp", now: 0 },
  { read: "tree", state: "refused", observed: true, way: "focusDown", now: 1 },
  { read: "tree", state: "refused", observed: true, way: "online", now: 0 },
  { read: "tree", state: "refused", observed: true, way: "feedWhole", now: 1 },
  { read: "tree", state: "refused", observed: true, way: "feedPart", now: 1 },
  { read: "tree", state: "refused", observed: true, way: "event", now: 1 },
  { read: "tree", state: "refused", observed: true, way: "retry", now: 1 },
  { read: "tree", state: "refused", observed: false, way: "mount", now: 0, back: 0 },
  { read: "tree", state: "refused", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "tree", state: "refused", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: false, way: "online", now: 0, back: 0 },
  { read: "tree", state: "refused", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: false, way: "event", now: 0, back: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "mount", now: 0 },
  { read: "tree", state: "refusedHeld", observed: true, way: "focusUp", now: 0 },
  { read: "tree", state: "refusedHeld", observed: true, way: "focusDown", now: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "online", now: 0 },
  { read: "tree", state: "refusedHeld", observed: true, way: "feedWhole", now: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "feedPart", now: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "event", now: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "retry", now: 1 },
  { read: "tree", state: "refusedHeld", observed: false, way: "mount", now: 0, back: 0 },
  { read: "tree", state: "refusedHeld", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "tree", state: "refusedHeld", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "tree", state: "refusedHeld", observed: false, way: "online", now: 0, back: 0 },
  { read: "tree", state: "refusedHeld", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "tree", state: "refusedHeld", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "tree", state: "refusedHeld", observed: false, way: "event", now: 0, back: 1 },
  { read: "tree", state: "inFlight", observed: true, way: "mount", now: 0 },
  { read: "tree", state: "inFlight", observed: true, way: "event", now: 1 },
  { read: "tree", state: "inFlight", observed: true, way: "retry", now: 0 },
  { read: "spec", state: "never", observed: false, way: "mount", now: 0, back: 1 },
  { read: "spec", state: "never", observed: false, way: "focusUp", now: 0, back: 1 },
  { read: "spec", state: "never", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "spec", state: "never", observed: false, way: "online", now: 0, back: 1 },
  { read: "spec", state: "never", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "spec", state: "never", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "spec", state: "never", observed: false, way: "event", now: 0, back: 1 },
  { read: "spec", state: "answered", observed: true, way: "mount", now: 0 },
  { read: "spec", state: "answered", observed: true, way: "focusUp", now: 0 },
  { read: "spec", state: "answered", observed: true, way: "focusDown", now: 0 },
  { read: "spec", state: "answered", observed: true, way: "online", now: 0 },
  { read: "spec", state: "answered", observed: true, way: "feedWhole", now: 0 },
  { read: "spec", state: "answered", observed: true, way: "feedPart", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "event", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "retry", now: 1 },
  { read: "spec", state: "answered", observed: false, way: "mount", now: 0, back: 0 },
  { read: "spec", state: "answered", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "spec", state: "answered", observed: false, way: "focusDown", now: 0, back: 0 },
  { read: "spec", state: "answered", observed: false, way: "online", now: 0, back: 0 },
  { read: "spec", state: "answered", observed: false, way: "feedWhole", now: 0, back: 0 },
  { read: "spec", state: "answered", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "spec", state: "answered", observed: false, way: "event", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "mount", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "focusUp", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "online", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "event", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: true, way: "mount", now: 0 },
  { read: "spec", state: "refused", observed: true, way: "focusUp", now: 0 },
  { read: "spec", state: "refused", observed: true, way: "focusDown", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "online", now: 0 },
  { read: "spec", state: "refused", observed: true, way: "feedWhole", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "feedPart", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "event", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "retry", now: 1 },
  { read: "spec", state: "refused", observed: false, way: "mount", now: 0, back: 0 },
  { read: "spec", state: "refused", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "spec", state: "refused", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: false, way: "online", now: 0, back: 0 },
  { read: "spec", state: "refused", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: false, way: "event", now: 0, back: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "mount", now: 0 },
  { read: "spec", state: "refusedHeld", observed: true, way: "focusUp", now: 0 },
  { read: "spec", state: "refusedHeld", observed: true, way: "focusDown", now: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "online", now: 0 },
  { read: "spec", state: "refusedHeld", observed: true, way: "feedWhole", now: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "feedPart", now: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "event", now: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "retry", now: 1 },
  { read: "spec", state: "refusedHeld", observed: false, way: "mount", now: 0, back: 0 },
  { read: "spec", state: "refusedHeld", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "spec", state: "refusedHeld", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "spec", state: "refusedHeld", observed: false, way: "online", now: 0, back: 0 },
  { read: "spec", state: "refusedHeld", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "spec", state: "refusedHeld", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "spec", state: "refusedHeld", observed: false, way: "event", now: 0, back: 1 },
  { read: "spec", state: "inFlight", observed: true, way: "mount", now: 0 },
  { read: "spec", state: "inFlight", observed: true, way: "event", now: 1 },
  { read: "spec", state: "inFlight", observed: true, way: "retry", now: 0 },
  { read: "spec", state: "inFlight", observed: false, way: "mount", now: 0, back: 0 },
  { read: "spec", state: "inFlight", observed: false, way: "event", now: 0, back: 1 },
  // The cells added below: a write with the live feed down, a write the page puts in the cache,
  // a write Dispatch refuses, the reader leaving the issue and coming back to it (a new load of
  // the stage), a focus while a write is out, a focus after Dispatch refused the live feed, a read
  // out that is refused or that holds an answer, a read refused a second time after an ask, the
  // Plan and Verification views, a write that lands with no answer, alone or while a second write
  // is out, and a read out at a focus or the feed's return that is then refused.
  // Writes with the live feed up and down, a write the page puts in the cache, and a write Dispatch refuses.
  { read: "tree", state: "answered", observed: true, way: "resolveDown200", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "resolveDown1200", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "resolveRefusedDown", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "setDataUp", now: 0 },
  { read: "tree", state: "answered", observed: true, way: "setDataDown", now: 1 },
  { read: "tree", state: "answered", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "tree", state: "answered", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "tree", state: "answered", observed: false, way: "resolveRefusedDown", now: 0, back: 1 },
  { read: "tree", state: "answered", observed: false, way: "setDataUp", now: 0, back: 0 },
  { read: "tree", state: "answered", observed: false, way: "setDataDown", now: 0, back: 1 },
  { read: "tree", state: "never", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "tree", state: "never", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "tree", state: "stale", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "tree", state: "stale", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: true, way: "resolve200", now: ONE_OR_TWO },
  { read: "tree", state: "refused", observed: true, way: "resolve1200", now: ONE_OR_TWO },
  { read: "tree", state: "refused", observed: true, way: "resolveDown200", now: 1 },
  { read: "tree", state: "refused", observed: true, way: "resolveDown1200", now: 1 },
  { read: "tree", state: "refused", observed: true, way: "resolveRefusedDown", now: 1 },
  { read: "tree", state: "refused", observed: true, way: "setDataUp", now: 0 },
  { read: "tree", state: "refused", observed: true, way: "setDataDown", now: 1 },
  { read: "tree", state: "refused", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: false, way: "resolveRefusedDown", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: false, way: "setDataUp", now: 0, back: 0 },
  { read: "tree", state: "refused", observed: false, way: "setDataDown", now: 0, back: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "resolve200", now: ONE_OR_TWO },
  { read: "tree", state: "refusedHeld", observed: true, way: "resolveDown200", now: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "setDataUp", now: 0 },
  { read: "tree", state: "refusedHeld", observed: true, way: "setDataDown", now: 1 },
  { read: "tree", state: "refusedHeld", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "tree", state: "refusedHeld", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "tree", state: "inFlight", observed: true, way: "resolve200", now: ONE_OR_TWO },
  { read: "tree", state: "inFlight", observed: true, way: "resolveDown200", now: 1 },
  { read: "tree", state: "inFlightRefused", observed: true, way: "resolve200", now: ONE_OR_TWO },
  { read: "tree", state: "inFlightRefused", observed: true, way: "resolveDown200", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "resolve200", now: ONE_OR_TWO },
  { read: "spec", state: "answered", observed: true, way: "resolve1200", now: ONE_OR_TWO },
  { read: "spec", state: "answered", observed: true, way: "resolveDown200", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "resolveDown1200", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "resolveRefusedDown", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "setDataUp", now: 0 },
  { read: "spec", state: "answered", observed: true, way: "setDataDown", now: 1 },
  { read: "spec", state: "answered", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "spec", state: "answered", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "spec", state: "answered", observed: false, way: "setDataUp", now: 0, back: 0 },
  { read: "spec", state: "answered", observed: false, way: "setDataDown", now: 0, back: 1 },
  { read: "spec", state: "never", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "spec", state: "never", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: true, way: "resolve200", now: ONE_OR_TWO },
  { read: "spec", state: "refused", observed: true, way: "resolveDown200", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "resolveRefusedDown", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "setDataUp", now: 0 },
  { read: "spec", state: "refused", observed: true, way: "setDataDown", now: 1 },
  { read: "spec", state: "refused", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: false, way: "setDataDown", now: 0, back: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "resolve200", now: ONE_OR_TWO },
  { read: "spec", state: "refusedHeld", observed: true, way: "resolveDown200", now: 1 },
  { read: "spec", state: "refusedHeld", observed: false, way: "resolve200", now: 0, back: 1 },
  { read: "spec", state: "refusedHeld", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "spec", state: "inFlight", observed: true, way: "resolve200", now: ONE_OR_TWO },
  { read: "spec", state: "inFlight", observed: true, way: "resolveDown200", now: 1 },
  { read: "spec", state: "inFlight", observed: false, way: "resolveDown200", now: 0, back: 1 },
  { read: "spec", state: "inFlightRefused", observed: true, way: "resolveDown200", now: 1 },
  // A read out, answered or refused once the way is done, or out over a held answer, with every way.
  { read: "tree", state: "inFlight", observed: true, way: "focusUp", now: 0 },
  { read: "tree", state: "inFlight", observed: true, way: "focusDown", now: 0 },
  { read: "tree", state: "inFlight", observed: true, way: "online", now: 0 },
  { read: "tree", state: "inFlight", observed: true, way: "feedWhole", now: 0 },
  { read: "tree", state: "inFlight", observed: true, way: "feedPart", now: 1 },
  { read: "tree", state: "inFlightRefused", observed: true, way: "mount", now: 0 },
  { read: "tree", state: "inFlightRefused", observed: true, way: "focusUp", now: 0 },
  { read: "tree", state: "inFlightRefused", observed: true, way: "online", now: 0 },
  { read: "tree", state: "inFlightRefused", observed: true, way: "feedPart", now: 1 },
  { read: "tree", state: "inFlightRefused", observed: true, way: "event", now: 1 },
  { read: "tree", state: "inFlightRefused", observed: true, way: "retry", now: 0 },
  { read: "tree", state: "inFlightHeld", observed: true, way: "mount", now: 0 },
  { read: "tree", state: "inFlightHeld", observed: true, way: "focusUp", now: 0 },
  { read: "tree", state: "inFlightHeld", observed: true, way: "focusDown", now: 0 },
  { read: "tree", state: "inFlightHeld", observed: true, way: "online", now: 0 },
  { read: "tree", state: "inFlightHeld", observed: true, way: "feedWhole", now: 0 },
  { read: "tree", state: "inFlightHeld", observed: true, way: "feedPart", now: 1 },
  { read: "tree", state: "inFlightHeld", observed: true, way: "event", now: 1 },
  { read: "tree", state: "inFlightHeld", observed: true, way: "retry", now: 1 },
  { read: "spec", state: "inFlight", observed: true, way: "focusUp", now: 0 },
  { read: "spec", state: "inFlight", observed: true, way: "focusDown", now: 0 },
  { read: "spec", state: "inFlight", observed: true, way: "online", now: 0 },
  { read: "spec", state: "inFlight", observed: true, way: "feedWhole", now: 0 },
  { read: "spec", state: "inFlight", observed: true, way: "feedPart", now: 1 },
  { read: "spec", state: "inFlight", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "spec", state: "inFlight", observed: false, way: "focusDown", now: 0, back: 0 },
  { read: "spec", state: "inFlight", observed: false, way: "online", now: 0, back: 0 },
  { read: "spec", state: "inFlight", observed: false, way: "feedWhole", now: 0, back: 0 },
  { read: "spec", state: "inFlight", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "spec", state: "inFlightRefused", observed: true, way: "mount", now: 0 },
  { read: "spec", state: "inFlightRefused", observed: true, way: "focusUp", now: 0 },
  { read: "spec", state: "inFlightRefused", observed: true, way: "online", now: 0 },
  { read: "spec", state: "inFlightRefused", observed: true, way: "feedPart", now: 1 },
  { read: "spec", state: "inFlightRefused", observed: true, way: "event", now: 1 },
  { read: "spec", state: "inFlightRefused", observed: true, way: "retry", now: 0 },
  { read: "spec", state: "inFlightRefused", observed: false, way: "mount", now: 0, back: 0 },
  { read: "spec", state: "inFlightRefused", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "spec", state: "inFlightRefused", observed: false, way: "online", now: 0, back: 0 },
  { read: "spec", state: "inFlightRefused", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "spec", state: "inFlightRefused", observed: false, way: "event", now: 0, back: 1 },
  { read: "spec", state: "inFlightHeld", observed: true, way: "mount", now: 0 },
  { read: "spec", state: "inFlightHeld", observed: true, way: "focusUp", now: 0 },
  { read: "spec", state: "inFlightHeld", observed: true, way: "focusDown", now: 0 },
  { read: "spec", state: "inFlightHeld", observed: true, way: "online", now: 0 },
  { read: "spec", state: "inFlightHeld", observed: true, way: "feedWhole", now: 0 },
  { read: "spec", state: "inFlightHeld", observed: true, way: "feedPart", now: 1 },
  { read: "spec", state: "inFlightHeld", observed: true, way: "event", now: 1 },
  { read: "spec", state: "inFlightHeld", observed: true, way: "retry", now: 1 },
  { read: "spec", state: "inFlightHeld", observed: false, way: "mount", now: 0, back: 0 },
  { read: "spec", state: "inFlightHeld", observed: false, way: "focusDown", now: 0, back: 0 },
  { read: "spec", state: "inFlightHeld", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "spec", state: "inFlightHeld", observed: false, way: "event", now: 0, back: 1 },
  // The reader leaving the issue and coming back to it: a new load of the stage.
  { read: "tree", state: "answered", observed: true, way: "reload", now: 0 },
  { read: "tree", state: "refused", observed: true, way: "reload", now: 0 },
  { read: "tree", state: "refusedHeld", observed: true, way: "reload", now: 0 },
  { read: "tree", state: "inFlight", observed: true, way: "reload", now: 1 },
  { read: "tree", state: "refusedAgain", observed: true, way: "reload", now: 0 },
  { read: "spec", state: "answered", observed: true, way: "reload", now: 1 },
  { read: "spec", state: "answered", observed: false, way: "reload", now: 0, back: 1 },
  { read: "spec", state: "never", observed: false, way: "reload", now: 0, back: 1 },
  { read: "spec", state: "stale", observed: false, way: "reload", now: 0, back: 1 },
  { read: "spec", state: "inFlight", observed: true, way: "reload", now: 1 },
  { read: "spec", state: "refusedAgain", observed: true, way: "reload", now: 1 },
  { read: "spec", state: "refusedAgain", observed: false, way: "reload", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: true, way: "reload", now: 1 },
  { read: "spec", state: "refused", observed: false, way: "reload", now: 0, back: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "reload", now: 1 },
  { read: "spec", state: "refusedHeld", observed: false, way: "reload", now: 0, back: 1 },
  { read: "plan", state: "refused", observed: true, way: "reload", now: 1 },
  { read: "plan", state: "refused", observed: false, way: "reload", now: 0, back: 1 },
  { read: "plan", state: "refusedHeld", observed: true, way: "reload", now: 1 },
  { read: "plan", state: "refusedHeld", observed: false, way: "reload", now: 0, back: 1 },
  { read: "verification", state: "refused", observed: true, way: "reload", now: 1 },
  { read: "verification", state: "refused", observed: false, way: "reload", now: 0, back: 1 },
  { read: "verification", state: "refusedHeld", observed: true, way: "reload", now: 1 },
  { read: "verification", state: "refusedHeld", observed: false, way: "reload", now: 0, back: 1 },
  // Refused, asked for again while nothing observed it, read again and refused again.
  { read: "tree", state: "refusedAgain", observed: true, way: "mount", now: 0 },
  { read: "tree", state: "refusedAgain", observed: true, way: "focusUp", now: 0 },
  { read: "tree", state: "refusedAgain", observed: true, way: "focusDown", now: 1 },
  { read: "tree", state: "refusedAgain", observed: true, way: "online", now: 0 },
  { read: "tree", state: "refusedAgain", observed: true, way: "feedWhole", now: 1 },
  { read: "tree", state: "refusedAgain", observed: true, way: "feedPart", now: 1 },
  { read: "tree", state: "refusedAgain", observed: true, way: "event", now: 1 },
  { read: "tree", state: "refusedAgain", observed: true, way: "retry", now: 1 },
  { read: "tree", state: "refusedAgain", observed: false, way: "mount", now: 0, back: 0 },
  { read: "tree", state: "refusedAgain", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "tree", state: "refusedAgain", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "tree", state: "refusedAgain", observed: false, way: "online", now: 0, back: 0 },
  { read: "tree", state: "refusedAgain", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "tree", state: "refusedAgain", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "tree", state: "refusedAgain", observed: false, way: "event", now: 0, back: 1 },
  { read: "spec", state: "refusedAgain", observed: true, way: "mount", now: 0 },
  { read: "spec", state: "refusedAgain", observed: true, way: "focusUp", now: 0 },
  { read: "spec", state: "refusedAgain", observed: true, way: "focusDown", now: 1 },
  { read: "spec", state: "refusedAgain", observed: true, way: "online", now: 0 },
  { read: "spec", state: "refusedAgain", observed: true, way: "feedWhole", now: 1 },
  { read: "spec", state: "refusedAgain", observed: true, way: "feedPart", now: 1 },
  { read: "spec", state: "refusedAgain", observed: true, way: "event", now: 1 },
  { read: "spec", state: "refusedAgain", observed: true, way: "retry", now: 1 },
  { read: "spec", state: "refusedAgain", observed: false, way: "mount", now: 0, back: 0 },
  { read: "spec", state: "refusedAgain", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "spec", state: "refusedAgain", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "spec", state: "refusedAgain", observed: false, way: "online", now: 0, back: 0 },
  { read: "spec", state: "refusedAgain", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "spec", state: "refusedAgain", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "spec", state: "refusedAgain", observed: false, way: "event", now: 0, back: 1 },
  // The Plan and Verification views.
  { read: "plan", state: "never", observed: false, way: "mount", now: 0, back: 1 },
  { read: "plan", state: "answered", observed: true, way: "mount", now: 0 },
  { read: "plan", state: "answered", observed: true, way: "event", now: 1 },
  { read: "plan", state: "refused", observed: true, way: "mount", now: 0 },
  { read: "plan", state: "refused", observed: true, way: "focusUp", now: 0 },
  { read: "plan", state: "refused", observed: true, way: "focusDown", now: 1 },
  { read: "plan", state: "refused", observed: true, way: "online", now: 0 },
  { read: "plan", state: "refused", observed: true, way: "feedWhole", now: 1 },
  { read: "plan", state: "refused", observed: true, way: "feedPart", now: 1 },
  { read: "plan", state: "refused", observed: true, way: "event", now: 1 },
  { read: "plan", state: "refused", observed: true, way: "retry", now: 1 },
  { read: "plan", state: "refused", observed: false, way: "mount", now: 0, back: 0 },
  { read: "plan", state: "refused", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "plan", state: "refused", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "plan", state: "refused", observed: false, way: "online", now: 0, back: 0 },
  { read: "plan", state: "refused", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "plan", state: "refused", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "plan", state: "refused", observed: false, way: "event", now: 0, back: 1 },
  { read: "plan", state: "refusedHeld", observed: true, way: "mount", now: 0 },
  { read: "plan", state: "refusedHeld", observed: true, way: "focusUp", now: 0 },
  { read: "plan", state: "refusedHeld", observed: true, way: "focusDown", now: 1 },
  { read: "plan", state: "refusedHeld", observed: true, way: "online", now: 0 },
  { read: "plan", state: "refusedHeld", observed: true, way: "feedWhole", now: 1 },
  { read: "plan", state: "refusedHeld", observed: true, way: "feedPart", now: 1 },
  { read: "plan", state: "refusedHeld", observed: true, way: "event", now: 1 },
  { read: "plan", state: "refusedHeld", observed: true, way: "retry", now: 1 },
  { read: "plan", state: "refusedHeld", observed: false, way: "mount", now: 0, back: 0 },
  { read: "plan", state: "refusedHeld", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "plan", state: "refusedHeld", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "plan", state: "refusedHeld", observed: false, way: "online", now: 0, back: 0 },
  { read: "plan", state: "refusedHeld", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "plan", state: "refusedHeld", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "plan", state: "refusedHeld", observed: false, way: "event", now: 0, back: 1 },
  { read: "verification", state: "never", observed: false, way: "mount", now: 0, back: 1 },
  { read: "verification", state: "answered", observed: true, way: "mount", now: 0 },
  { read: "verification", state: "answered", observed: true, way: "event", now: 1 },
  { read: "verification", state: "refused", observed: true, way: "mount", now: 0 },
  { read: "verification", state: "refused", observed: true, way: "focusUp", now: 0 },
  { read: "verification", state: "refused", observed: true, way: "focusDown", now: 1 },
  { read: "verification", state: "refused", observed: true, way: "online", now: 0 },
  { read: "verification", state: "refused", observed: true, way: "feedWhole", now: 1 },
  { read: "verification", state: "refused", observed: true, way: "feedPart", now: 1 },
  { read: "verification", state: "refused", observed: true, way: "event", now: 1 },
  { read: "verification", state: "refused", observed: true, way: "retry", now: 1 },
  { read: "verification", state: "refused", observed: false, way: "mount", now: 0, back: 0 },
  { read: "verification", state: "refused", observed: false, way: "focusUp", now: 0, back: 0 },
  { read: "verification", state: "refused", observed: false, way: "focusDown", now: 0, back: 1 },
  { read: "verification", state: "refused", observed: false, way: "online", now: 0, back: 0 },
  { read: "verification", state: "refused", observed: false, way: "feedWhole", now: 0, back: 1 },
  { read: "verification", state: "refused", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "verification", state: "refused", observed: false, way: "event", now: 0, back: 1 },
  { read: "verification", state: "refusedHeld", observed: true, way: "mount", now: 0 },
  { read: "verification", state: "refusedHeld", observed: true, way: "focusUp", now: 0 },
  { read: "verification", state: "refusedHeld", observed: true, way: "focusDown", now: 1 },
  { read: "verification", state: "refusedHeld", observed: true, way: "online", now: 0 },
  { read: "verification", state: "refusedHeld", observed: true, way: "feedWhole", now: 1 },
  { read: "verification", state: "refusedHeld", observed: true, way: "feedPart", now: 1 },
  { read: "verification", state: "refusedHeld", observed: true, way: "event", now: 1 },
  { read: "verification", state: "refusedHeld", observed: true, way: "retry", now: 1 },
  { read: "verification", state: "refusedHeld", observed: false, way: "mount", now: 0, back: 0 },
  { read: "verification", state: "refusedHeld", observed: false, way: "focusUp", now: 0, back: 0 },
  {
    read: "verification",
    state: "refusedHeld",
    observed: false,
    way: "focusDown",
    now: 0,
    back: 1,
  },
  { read: "verification", state: "refusedHeld", observed: false, way: "online", now: 0, back: 0 },
  {
    read: "verification",
    state: "refusedHeld",
    observed: false,
    way: "feedWhole",
    now: 0,
    back: 1,
  },
  { read: "verification", state: "refusedHeld", observed: false, way: "feedPart", now: 0, back: 1 },
  { read: "verification", state: "refusedHeld", observed: false, way: "event", now: 0, back: 1 },
  // A focus while a write is out with the live feed down, and a focus after Dispatch refused the live feed.
  { read: "tree", state: "refused", observed: true, way: "focusDownWriting", now: 1 },
  { read: "tree", state: "refused", observed: false, way: "focusDownWriting", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: true, way: "focusHeldFeedWhole", now: 1 },
  { read: "tree", state: "refused", observed: false, way: "focusHeldFeedWhole", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: true, way: "focusHeldFeedPart", now: 1 },
  { read: "tree", state: "refused", observed: false, way: "focusHeldFeedPart", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: true, way: "focusFeedRefused", now: 1 },
  { read: "tree", state: "refused", observed: false, way: "focusFeedRefused", now: 0, back: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "focusDownWriting", now: 1 },
  { read: "tree", state: "refusedHeld", observed: false, way: "focusDownWriting", now: 0, back: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "focusFeedRefused", now: 1 },
  { read: "tree", state: "refusedHeld", observed: false, way: "focusFeedRefused", now: 0, back: 1 },
  // The write's settle with the live feed still down reads an answered one too, as after any write
  // of the issue while the feed is down.
  { read: "tree", state: "answered", observed: true, way: "focusDownWriting", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "focusHeldFeedWhole", now: 0 },
  { read: "tree", state: "answered", observed: true, way: "focusHeldFeedPart", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "focusFeedRefused", now: 0 },
  { read: "tree", state: "answered", observed: false, way: "focusDownWriting", now: 0, back: 1 },
  { read: "tree", state: "answered", observed: false, way: "focusFeedRefused", now: 0, back: 0 },
  { read: "tree", state: "refusedAgain", observed: true, way: "focusDownWriting", now: 1 },
  { read: "tree", state: "refusedAgain", observed: true, way: "focusFeedRefused", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "focusDownWriting", now: 1 },
  { read: "spec", state: "refused", observed: false, way: "focusDownWriting", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: true, way: "focusHeldFeedWhole", now: 1 },
  { read: "spec", state: "refused", observed: false, way: "focusHeldFeedWhole", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: true, way: "focusHeldFeedPart", now: 1 },
  { read: "spec", state: "refused", observed: false, way: "focusHeldFeedPart", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: true, way: "focusFeedRefused", now: 1 },
  { read: "spec", state: "refused", observed: false, way: "focusFeedRefused", now: 0, back: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "focusDownWriting", now: 1 },
  { read: "spec", state: "refusedHeld", observed: false, way: "focusDownWriting", now: 0, back: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "focusFeedRefused", now: 1 },
  { read: "spec", state: "refusedHeld", observed: false, way: "focusFeedRefused", now: 0, back: 1 },
  { read: "spec", state: "answered", observed: true, way: "focusDownWriting", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "focusHeldFeedWhole", now: 0 },
  { read: "spec", state: "answered", observed: true, way: "focusHeldFeedPart", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "focusFeedRefused", now: 0 },
  { read: "spec", state: "answered", observed: false, way: "focusDownWriting", now: 0, back: 1 },
  { read: "spec", state: "answered", observed: false, way: "focusFeedRefused", now: 0, back: 0 },
  { read: "spec", state: "refusedAgain", observed: true, way: "focusDownWriting", now: 1 },
  { read: "spec", state: "refusedAgain", observed: true, way: "focusFeedRefused", now: 1 },
  // A write that lands with no answer, and the refresh of every query the write store makes for it.
  { read: "tree", state: "answered", observed: true, way: "landedBlankUp", now: ONE_OR_TWO },
  { read: "tree", state: "answered", observed: true, way: "landedBlankDown", now: 1 },
  { read: "tree", state: "answered", observed: false, way: "landedBlankDown", now: 0, back: 1 },
  { read: "tree", state: "refused", observed: true, way: "landedBlankUp", now: ONE_OR_TWO },
  { read: "tree", state: "refused", observed: true, way: "landedBlankDown", now: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "landedBlankDown", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "landedBlankUp", now: ONE_OR_TWO },
  { read: "spec", state: "answered", observed: true, way: "landedBlankDown", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "landedBlankDown", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "landedBlankBesideUp", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "landedBlankBesideDown", now: 1 },
  // The writes' settle with the live feed down is the one read of the tree and the open view.
  { read: "tree", state: "answered", observed: true, way: "landedBlankBesideKeepDown", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "landedBlankBesideKeepDown", now: 1 },
  // A refresh asked while the writes hold the reads - the event feed's refresh of every query
  // after an incomplete replay - is read once, as they settle.
  { read: "tree", state: "answered", observed: true, way: "landedBlankBesideFeedPart", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "landedBlankBesideFeedPart", now: 1 },
  // A refetchQueries of the tree while a write holds it sends nothing; an event after the write
  // reads the tree once.
  { read: "tree", state: "answered", observed: true, way: "eventAfterWaitedRead", now: 1 },
  // A composer's edit names its issue: with the live feed down the tree is read once at its
  // settle; with the feed up its event reads it once.
  { read: "tree", state: "answered", observed: true, way: "composerEditDown", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "composerEditUp", now: 1 },
  // A refused read sent as the last write settles, after a focus with the live feed down asked for
  // every refused one while the write held them: refused, it is read no second time.
  { read: "tree", state: "refused", observed: true, way: "landedBlankBesideFocus", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "landedBlankBesideFocus", now: 1 },
  // A write Dispatch refuses with the live feed up changed nothing, and no event comes: nothing is
  // read; an event that lands while it is out is read once, as it settles.
  { read: "tree", state: "answered", observed: true, way: "resolveRefusedUp", now: 0 },
  { read: "tree", state: "answered", observed: true, way: "resolveRefusedUpPosted", now: 1 },
  // A read out as a window focus with the live feed down, or the feed back whole, asks for every
  // refused one, then refused: owed, and read once more.
  { read: "tree", state: "inFlightRefused", observed: true, way: "focusDown", now: 1 },
  { read: "tree", state: "inFlightRefused", observed: true, way: "feedWhole", now: 1 },
  { read: "spec", state: "inFlightRefused", observed: true, way: "focusDown", now: 1 },
  { read: "spec", state: "inFlightRefused", observed: true, way: "feedWhole", now: 1 },
  // An answered Plan or Verification view at a new load: read once, as the Spec view is.
  { read: "plan", state: "answered", observed: true, way: "reload", now: 1 },
  { read: "plan", state: "answered", observed: false, way: "reload", now: 0, back: 1 },
  { read: "verification", state: "answered", observed: true, way: "reload", now: 1 },
  { read: "verification", state: "answered", observed: false, way: "reload", now: 0, back: 1 },
  // A trigger while the reader is away, then the reader back while a write is out and gone before
  // it settles: no read while the write holds it, and one as the reader comes back.
  {
    read: "tree",
    state: "refused",
    observed: false,
    way: "backAndGoneWhileWaiting",
    now: 0,
    back: 1,
  },
  // The reader's Retry while a write is out, sent at once, then the reader gone before the write
  // settles: one read owed, read as the reader comes back.
  {
    read: "tree",
    state: "refused",
    observed: false,
    way: "retryGoneWhileWaiting",
    now: 1,
    back: 1,
  },
  // Two writes out together, each landing with no answer: read once, as the last settles.
  { read: "tree", state: "answered", observed: true, way: "twoAnswerlessUp", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "twoAnswerlessDown", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "twoAnswerlessUp", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "twoAnswerlessDown", now: 1 },
  // The issue opened while a write of it that lands with no answer is out: its first load is sent
  // at once, and read once more as the write settles.
  { read: "tree", state: "never", observed: false, way: "openDuringAnswerlessUp", now: 2, back: 0 },
  {
    read: "tree",
    state: "never",
    observed: false,
    way: "openDuringAnswerlessDown",
    now: 2,
    back: 0,
  },
  // The issue opened while a write of another issue is out, with an event of it 500 ms on: the
  // first load is sent at once, and the event reads the tree again.
  {
    read: "tree",
    state: "never",
    observed: false,
    way: "openDuringWriteWithEvent",
    now: 2,
    back: 0,
  },
  // A first read while a write is out is sent at once: owed one more as a write of the issue
  // settles, and nothing more for a write of another issue.
  {
    read: "tree",
    state: "never",
    observed: false,
    way: "firstReadDuringOwnWrite",
    now: 2,
    back: 0,
  },
  {
    read: "spec",
    state: "never",
    observed: false,
    way: "firstReadDuringOwnWrite",
    now: 2,
    back: 0,
  },
  {
    read: "tree",
    state: "never",
    observed: false,
    way: "firstReadDuringOtherWrite",
    now: 1,
    back: 0,
  },
  {
    read: "spec",
    state: "never",
    observed: false,
    way: "firstReadDuringOtherWrite",
    now: 1,
    back: 0,
  },
  // A write pressed as the last write of the issue settles keeps the hold: the tree is read once,
  // as the second settles.
  { read: "tree", state: "answered", observed: true, way: "secondWriteAtSettle", now: 1 },
  // A write that lands with no answer and changes nothing the issue's reads answer: its refresh of
  // every query asks for the tree and the open view, a refused one too, read once as it settles.
  { read: "tree", state: "answered", observed: true, way: "landedBlankQuietDown", now: 1 },
  { read: "tree", state: "refused", observed: true, way: "landedBlankQuietDown", now: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "landedBlankQuietDown", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "landedBlankQuietDown", now: 1 },
  // The reader coming back to a read marked stale while a write of the issue is out: the write
  // holds the read, which is sent once as it settles.
  { read: "tree", state: "stale", observed: false, way: "comeBackDuringWrite", now: 1, back: 0 },
  { read: "spec", state: "stale", observed: false, way: "comeBackDuringWrite", now: 1, back: 0 },
  // TanStack's own retry is part of the one read out: what asks during it reads once more after it
  // lands, the Retry, its retry and that one read in all.
  { read: "tree", state: "refused", observed: true, way: "retryRetriedDuringWrite", now: 3 },
  // The reader's Retry while a write of the issue is out, staying: sent at once, and read once
  // more as the write settles.
  { read: "tree", state: "refused", observed: true, way: "retryDuringOwnWrite", now: 2 },
  { read: "tree", state: "refusedHeld", observed: true, way: "retryDuringOwnWrite", now: 2 },
  { read: "spec", state: "refused", observed: true, way: "retryDuringOwnWrite", now: 2 },
  { read: "spec", state: "refusedHeld", observed: true, way: "retryDuringOwnWrite", now: 2 },
  // An event while a write of the issue is out: read once, as the write settles.
  { read: "tree", state: "answered", observed: true, way: "eventDuringOwnWrite", now: 1 },
  { read: "tree", state: "refused", observed: true, way: "eventDuringOwnWrite", now: 1 },
  { read: "tree", state: "refusedHeld", observed: true, way: "eventDuringOwnWrite", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "eventDuringOwnWrite", now: 1 },
  { read: "spec", state: "refused", observed: true, way: "eventDuringOwnWrite", now: 1 },
  { read: "spec", state: "refusedHeld", observed: true, way: "eventDuringOwnWrite", now: 1 },
  { read: "tree", state: "refused", observed: false, way: "eventDuringOwnWrite", now: 0, back: 1 },
  { read: "spec", state: "refused", observed: false, way: "eventDuringOwnWrite", now: 0, back: 1 },
  // A write of another issue, or one naming none, holds nothing: an event or a Retry is read at
  // once, and nothing more as it settles.
  {
    read: "tree",
    state: "answered",
    observed: false,
    way: "eventAwayDuringOtherWrite",
    now: 0,
    back: 1,
  },
  {
    read: "tree",
    state: "refused",
    observed: false,
    way: "eventAwayDuringOtherWrite",
    now: 0,
    back: 1,
  },
  {
    read: "tree",
    state: "answered",
    observed: false,
    way: "eventBackDuringOtherWrite",
    now: 1,
    back: 0,
  },
  {
    read: "tree",
    state: "refused",
    observed: false,
    way: "eventBackDuringOtherWrite",
    now: 1,
    back: 0,
  },
  {
    read: "tree",
    state: "refused",
    observed: false,
    way: "retryBackDuringOtherWrite",
    now: 1,
    back: 0,
  },
  // A send of the margin pressed on the page is one write of the issue past its deadline too.
  { read: "tree", state: "answered", observed: true, way: "marginSendPastDeadlineAway", now: 1 },
  // One event, read once.
  { read: "tree", state: "answered", observed: true, way: "eventSoonAfterCommentsRead", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "eventSoonAfterCommentsRead", now: 1 },
  // One event, read once, while a read of the comments is out as it lands.
  { read: "tree", state: "answered", observed: true, way: "eventWhileCommentsOut600", now: 1 },
  { read: "spec", state: "answered", observed: true, way: "eventWhileCommentsOut600", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "eventWhileCommentsOut1500", now: 1 },
  // A write of another issue, by its press or by its request's path, is no write of this one, nor
  // is a write of an issue whose key begins with this one's, or one that names no issue.
  {
    read: "tree",
    state: "answered",
    observed: true,
    way: "eventDuringOtherIssueWritesByPress",
    now: 1,
  },
  {
    read: "tree",
    state: "answered",
    observed: true,
    way: "eventDuringOtherIssueWriteByPath",
    now: 1,
  },
  {
    read: "tree",
    state: "answered",
    observed: true,
    way: "eventDuringLongerKeyIssueWriteAway",
    now: 1,
  },
  { read: "tree", state: "answered", observed: true, way: "eventDuringIssuelessWriteAway", now: 1 },
  // One event, read once, while a read of the decisions is out as it lands.
  { read: "tree", state: "answered", observed: true, way: "eventWhileDecisionsOut600", now: 1 },
  { read: "tree", state: "answered", observed: true, way: "eventWhileDecisionsOut1500", now: 1 },
  // A write's own callback runs once the write has landed, so its drop of the views holds
  // nothing: the open view and the tree are read once, at once; a first press of a view never
  // read meanwhile is sent at once, read once more as the write settles, and once for the drop.
  { read: "verification", state: "answered", observed: true, way: "writeDroppingViews", now: 1 },
  {
    read: "verification",
    state: "answered",
    observed: true,
    way: "writeDroppingViewsOnly",
    now: 1,
  },
  {
    read: "verification",
    state: "never",
    observed: false,
    way: "firstPressDuringWriteDroppingViews",
    now: 3,
    back: 0,
  },
  // A load while a write of the issue is out reads the open view at once, and once more as the
  // write settles: the load's reset holds nothing.
  { read: "spec", state: "answered", observed: true, way: "reloadDuringWrite", now: 2 },
  // A load after a write's own callback dropped the views, while a write of the issue is still
  // out, is a load too: the open view at once, and once more as the write settles.
  { read: "spec", state: "answered", observed: true, way: "reloadAfterDropDuringWrite", now: 2 },
  // Within one write, whose callback drops the views once it landed and keeps running at the
  // load: no write of the issue is out by then, so the drop reads the open view at once, and the
  // load does again.
  { read: "spec", state: "answered", observed: true, way: "reloadAfterDropInOneWrite", now: 2 },
  // One event, read once, while a read of the comments is out that Dispatch then refuses.
  { read: "tree", state: "answered", observed: true, way: "eventWhileCommentsRefused600", now: 1 },
];

describe("the rule for when a read of the tree or of a view starts", () => {
  beforeEach(() => {
    jest.useFakeTimers();
  });
  afterEach(() => {
    realClock();
  });
  for (const rule of READ_RULES) {
    test(ruleWords(rule), () => holdsRule(rule));
  }
});

// A child of the issue is renamed. Dispatch writes the child's own `issue.updated`, naming its
// ancestors, and no event of the parent: a title moves no `child.status`. Today's page draws the
// parent's Children from the parent's issue read, which that event refreshes, so the new title
// is drawn there with no reload. The outline draws the child's row's words from the parent's tree
// read, so that event must read the parent's tree again, or the row keeps the old title.
test("a child renamed while the parent's outline is drawn reads the parent's tree and draws the new title", async () => {
  const CHILD = "CORE-2";
  const stubs = stubTreeReads(async (refs) => namedTargets(refs));
  let title = "The child's first title";
  (api.getIssueTree as Mock<typeof api.getIssueTree>).mockImplementation(async () =>
    treeAnswer([], { nodes: [answerNode(CHILD, "issue", { text: title })] })
  );
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const childWords = (root: TreeNode) => nodesInOrder(root).find((node) => node.id === CHILD)?.text;
  try {
    const view = renderHook(() => useIssueTree(issue).root, { wrapper: stageWrapper(client) });
    await waitFor(() => expect(childWords(view.result.current)).toBe("The child's first title"));
    await act(async () => {
      await Bun.sleep(1_100);
    });
    const before = treeReadsOf();

    title = "The child's new title";
    const renamed = feedEvent(
      "issue.updated",
      { ancestors: [KEY], changed: ["title"], title },
      { issue_key: CHILD }
    );
    await act(async () => {
      refreshQueries(client, eventRefreshTargets(renamed, "alice"));
      await Bun.sleep(1_500);
    });
    expect(treeReadsOf() - before, "the child's rename reads the parent's tree").toBe(1);
    expect(childWords(view.result.current), "the child's row draws its new title").toBe(
      "The child's new title"
    );
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
    client.clear();
  }
});

// R2 and R3 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-19 22:21Z: a names
// read (`["reference-targets", <addresses>]`) that a post's edit or delete refreshes keeps the
// answer it held while the refetch is out, and TanStack keeps that answer when the refetch fails.
// So the feed writes the correction's own facts into the answer it keeps (`markCorrectedPost`):
// after a failed refetch the kept answer and the page's published name give a deleted post's
// deleted line, never its old words, and mark an edited post edited.

/** A comment a turn cites by its whole id, as Dispatch stores one: a correction names the post by
 *  that id. */
const CITED_ID = "11111111-2222-4333-8444-555555555555";
const CITED = `dispatch://${KEY}/comment/${CITED_ID}`;
const OLD_WORDS = "Hold the deploy.";

/** The page open on the issue: the workspace's event feed, the tree and what it publishes, signed
 *  in as alice, with the comments read answering `stored` and the names read naming each comment
 *  by its words until `deliver` makes every names read fail. The tree read places each of
 *  `stored` under the issue, as it stands when the tree is read. A first load that fails puts
 *  back what the page took before it throws. Returns what each step reads. */
async function openCorrectedPage(stored: Comment[]) {
  let failing = false;
  let namesReads = 0;
  const sockets = installFakeLiveSockets();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const stubs = stubTreeReads(
    async (refs) => {
      namesReads += 1;
      if (failing) throw new Error("the names read failed");
      return {
        targets: namedTargets(refs).targets.map((target) => {
          const turn = stored.find((one) => target.ref.endsWith(`/comment/${one.id}`));
          return turn === undefined ? target : { ...target, kind: "comment", name: turn.body };
        }),
      };
    },
    { comments: stored }
  );
  spyOn(api, "getIssueTree").mockImplementation(async () => treeAnswer(stored));
  let unmount: (() => void) | undefined;
  // Puts back what the page took: the view, the feed's socket, the cache, the clock and the
  // stubbed reads.
  const restore = () => {
    unmount?.();
    liveSocketFor(client).close();
    client.clear();
    sockets.restore();
    setSystemTime();
    for (const stub of stubs) stub.mockRestore();
  };
  try {
    const signedIn: AuthenticatedUser = { event_head: "0", kind: "user", login: "alice" };
    client.setQueryData(["whoami"], signedIn);
    const wrapper = ({ children }: { children: ReactNode }) => (
      <RoutedQueries client={client}>
        <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
      </RoutedQueries>
    );
    const view = renderHook(
      () => {
        useEventFeed();
        const { namesFailure } = useIssueTree(issue);
        return {
          edited: useKnownEditedReferences(),
          name: useKnownReferenceName(CITED),
          namesFailed: namesFailure !== undefined,
        };
      },
      { wrapper }
    );
    unmount = view.unmount;
    await waitFor(() => expect(view.result.current.name).toBe(OLD_WORDS));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    const socket = sockets.latest();
    act(() => socket.open());
    const { sub } = socket.subscribed("events");
    act(() => socket.receive({ type: "events_start", sub, since: "0", replay_complete: true }));
    return {
      /** What the names reads the cache holds keep for the cited comment. */
      cached: (): ReferenceTarget[] =>
        client
          .getQueryCache()
          .findAll({ queryKey: ["reference-targets"] })
          .flatMap(
            (query) =>
              (query.state.data as { targets: ReferenceTarget[] } | undefined)?.targets ?? []
          )
          .filter((target) => target.ref === CITED),
      /** Delivers `event` on the feed with every names read failing from now on, and waits until
       *  the names read it leads to has failed. */
      deliver: async (event: DispatchEvent) => {
        failing = true;
        const before = namesReads;
        // A second on, so the scheduler's interval between two fetches of one read
        // (`MIN_REFETCH_INTERVAL_MS`) holds back none of the reads the event sends.
        setSystemTime(Date.now() + MIN_REFETCH_INTERVAL_MS);
        act(() => socket.receive({ type: "event", sub, id: String(event.id), event }));
        await waitFor(() => expect(namesReads).toBeGreaterThan(before));
        await waitFor(() => expect(client.isFetching()).toBe(0));
        const states = client
          .getQueryCache()
          .findAll({ queryKey: ["reference-targets"] })
          .map((query) => query.state.status);
        expect(states, "the names read the event led to failed").toContain("error");
      },
      restore,
      view,
    };
  } catch (error) {
    restore();
    throw error;
  }
}

/** The delete of `cited` as Dispatch stores it, and as its event carries it: the comment's
 *  routing fields and its marks, and no body. */
function deletionOf(cited: Comment) {
  const marks = { deleted_at: "2026-11-05T02:00:00Z", deleted_by: comment.author };
  const { body: _body, ...routing } = cited;
  return {
    marks,
    payload: { ...routing, ...marks, artifact_name: "" },
    stored: { ...cited, ...marks, body: "" },
  };
}

test("a deleted post's names read whose refetch fails names the deleted line, never the old words", async () => {
  const cited: Comment = { ...comment, body: OLD_WORDS, id: CITED_ID };
  const stored: Comment[] = [{ ...comment, body: `Read ${CITED} first.` }, cited];
  const page = await openCorrectedPage(stored);
  try {
    const deleted = deletionOf(cited);
    stored[1] = deleted.stored;
    await page.deliver(feedEvent("comment.deleted", deleted.payload));
    expect({
      cached: page.cached(),
      published: page.view.result.current.name,
    }).toEqual({
      cached: [{ ...deleted.marks, gone: false, kind: "comment", name: "", ref: CITED }],
      published: "Deleted by alice",
    });
  } finally {
    page.restore();
  }
});

test("an edited post's names read whose refetch fails marks the post edited", async () => {
  const cited: Comment = { ...comment, body: OLD_WORDS, id: CITED_ID };
  const stored: Comment[] = [{ ...comment, body: `Read ${CITED} first.` }, cited];
  const page = await openCorrectedPage(stored);
  try {
    const editedAt = "2026-11-05T02:00:00Z";
    const edited: Comment = {
      ...cited,
      body: "Hold the deploy until Friday.",
      edited_at: editedAt,
    };
    stored[1] = edited;
    await page.deliver(feedEvent("comment.edited", { ...edited, artifact_name: "" }));
    // The words stay the kept answer's until a read succeeds; the mark is the edit's own.
    expect({
      cached: page.cached(),
      edited: [...page.view.result.current.edited],
    }).toEqual({
      cached: [{ edited_at: editedAt, gone: false, kind: "comment", name: OLD_WORDS, ref: CITED }],
      edited: [CITED],
    });
  } finally {
    page.restore();
  }
});

// The same where a new turn has changed the tree's set of addresses while every names read
// fails: the page draws the answer the cache holds for the set it answered last
// (`useLastAnswered`), and a delete that arrives only then writes its mark into that answer, so
// the page names the deleted line there too, never the old words.
test("a delete after a grown tree's names read failed names the deleted line, never the old words", async () => {
  const cited: Comment = { ...comment, body: OLD_WORDS, id: CITED_ID };
  const stored: Comment[] = [{ ...comment, body: `Read ${CITED} first.` }, cited];
  const page = await openCorrectedPage(stored);
  try {
    // A new turn arrives: the tree holds one address more, and the names read of that set fails.
    // The tree's names are the addresses its words cite, so the new turn cites one more.
    const second: Comment = {
      ...comment,
      body: "A second turn, citing dispatch://CORE-7.",
      id: "comment-3",
    };
    stored.push(second);
    await page.deliver(feedEvent("comment.created", { ...second, artifact_name: "" }));
    expect(page.view.result.current).toMatchObject({ name: OLD_WORDS, namesFailed: true });

    const deleted = deletionOf(cited);
    stored[1] = deleted.stored;
    await page.deliver(feedEvent("comment.deleted", deleted.payload, { id: 3, seq: 3 }));
    expect({
      cached: page.cached(),
      published: page.view.result.current.name,
    }).toEqual({
      cached: [{ ...deleted.marks, gone: false, kind: "comment", name: "", ref: CITED }],
      published: "Deleted by alice",
    });
  } finally {
    page.restore();
  }
});
