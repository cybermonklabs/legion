import { expect, type Mock, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import { eventRefreshTargets } from "../../api/event-refresh";
import { referenceTargetsQuery } from "../../api/queries";
import { MIN_REFETCH_INTERVAL_MS, refreshQueries } from "../../api/query-refresh";
import type {
  Artifact,
  ArtifactBlock,
  Comment,
  DispatchEvent,
  DisplayTree,
  EventType,
  IssueDetails,
  ReferenceTarget,
} from "../../api/types";
import type { TreeNode } from "./issue-tree";
import { NODE_TYPE_NAMES, PRESET_NAMES } from "./page-words";
import {
  ReferenceNamesProvider,
  useKnownAmbiguousReferences,
  useKnownReferenceName,
  useKnownReferenceNames,
  usePublishReferenceNames,
} from "./reference-names";
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
  role: "plan",
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
  | "blocks"
  | "comments"
  | "coverage"
  | "decisions"
  | "login"
  | "messages"
  | "results";

/** Every read the hook makes, answered empty but for the plan's one task block, the reader's
 *  login and `comments`. The `held` read stays out until its `until` settles, and then answers
 *  as it would have. */
function stubTreeReads(
  answerNames: (refs: readonly string[]) => Promise<{
    targets: ReferenceTarget[];
  }>,
  {
    comments = [],
    held,
  }: {
    readonly comments?: readonly Comment[];
    readonly held?: { readonly read: TreeRead; readonly until: Promise<void> };
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
      return { decisions: [], document: null, open_questions: [] };
    }),
    spyOn(api, "getIssueResults").mockImplementation(async () => {
      await answered("results");
      return { checks: [], issue: KEY, verification_artifact_id: "" };
    }),
    spyOn(api, "getIssueReferences").mockResolvedValue({ members: [], truncated: false }),
    spyOn(api, "listIssueAsks").mockImplementation(async () => {
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
    spyOn(api, "getArtifactBlocks").mockImplementation(async () => {
      await answered("blocks");
      return [task];
    }),
    spyOn(api, "whoAmI").mockImplementation(async () => {
      await answered("login");
      return { event_head: "0", kind: "user", login: "alice" };
    }),
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
    const view = renderHook(() => useIssueTree(issue).root, { wrapper });
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
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
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

    // A second comment arrives: the tree holds one address more, and the names read of that set
    // fails.
    failing = true;
    const second: Comment = { ...comment, body: "A second turn.", id: "comment-2" };
    await act(async () => {
      client.setQueryData(["comments", KEY], [citing, second]);
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
  "blocks",
  "login",
];

// The names come last and all at once. While any one read of the tree is still
// out - the comments, the asks, the coverage, the decisions, the messages, the results, the
// plan's blocks or the reader's login - the addresses known so far are not read; once it
// answers, every address the tree holds - a block's own, and one a comment's words cite - is
// read in one request, and the page publishes the answer to every surface beside it
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
        comments: [{ ...comment, body: `Read ${cited} first.` }],
        held: { read, until: out.promise },
      }
    );
    // A client and a page of the turn's own, so no answer of an earlier turn is in hand.
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>
        <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
      </QueryClientProvider>
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
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
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
    { comments: [{ ...comment, body: `Read ${ambiguous} and ${gone} first.` }] }
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
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

/** What one surface publishes: the names it read and the references it read as ambiguous. */
interface Published {
  readonly ambiguous: ReadonlySet<string>;
  readonly names: ReadonlyMap<string, string>;
}

/** What a link under the provider read on its latest render. */
interface Seen {
  ambiguous?: ReadonlySet<string>;
  names?: ReadonlyMap<string, string>;
}

/** A surface that publishes `published` for as long as it is mounted. */
function Publisher({ published }: { readonly published: Published }): ReactNode {
  usePublishReferenceNames(published.names, published.ambiguous);
  return null;
}

/** A link under the provider, which writes what it reads into `seen` on every render. */
function Reader({ seen }: { readonly seen: Seen }): ReactNode {
  seen.ambiguous = useKnownAmbiguousReferences();
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
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
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

// A plan task has no words until the names read answers, so what its words cite is in no address
// that read was given. One more read names those addresses alone, once the first has answered,
// and the page publishes its answer too.
test("what a plan task's words cite is named by one more read", async () => {
  const cited = "dispatch://CORE-7";
  const words = `Draw the tree as ${cited} asks.`;
  const asked: string[][] = [];
  const stubs = stubTreeReads(async (refs) => {
    asked.push([...refs]);
    return {
      targets: namedTargets(refs).targets.map((target) => {
        if (target.ref === TASK_REF) {
          return {
            ...target,
            name: `:::plan_task{#t1}\n${words}\n:::`,
            text_tree: {
              content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
              type: "doc",
            },
          };
        }
        return target.ref === cited
          ? { ...target, kind: "issue", name: "The cited issue" }
          : target;
      }),
    };
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
  );
  try {
    const view = renderHook(
      () => {
        useIssueTree(issue);
        return useKnownReferenceName(cited);
      },
      { wrapper }
    );
    await waitFor(() => expect(view.result.current).toBe("The cited issue"));
    expect(asked).toHaveLength(2);
    expect(asked[0]).toContain(TASK_REF);
    expect(asked[1]).toEqual([cited]);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// Once nothing is left for the second read to name - an address only a plan task's words cited
// is cited by a comment too, so the first read names it - the second read's query is disabled,
// and it would hold its last answer as placeholder for good. The name the page publishes for
// that address is the first read's newer one, never that older answer.
test("once the second read has nothing left to name, the first read's newer names stand", async () => {
  const cited = "dispatch://CORE-7";
  const words = `Draw the tree as ${cited} asks.`;
  let title = "The cited issue";
  const asked: string[][] = [];
  const stubs = stubTreeReads(async (refs) => {
    asked.push([...refs]);
    return {
      targets: namedTargets(refs).targets.map((target) => {
        if (target.ref === TASK_REF) {
          return {
            ...target,
            name: `:::plan_task{#t1}\n${words}\n:::`,
            text_tree: {
              content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
              type: "doc",
            },
          };
        }
        return target.ref === cited ? { ...target, kind: "issue", name: title } : target;
      }),
    };
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
  );
  try {
    const view = renderHook(
      () => {
        useIssueTree(issue);
        return useKnownReferenceName(cited);
      },
      { wrapper }
    );
    // The first round: only the plan task's words cite the issue, so the second read names it.
    await waitFor(() => expect(view.result.current).toBe("The cited issue"));
    expect(asked[1]).toEqual([cited]);

    // The issue is renamed, and a comment citing it arrives: the tree's own addresses hold it
    // now, so the first read names it and nothing is left for the second.
    title = "The cited issue, renamed";
    await act(async () => {
      client.setQueryData(["comments", KEY], [{ ...comment, body: `Read ${cited} again.` }]);
      await Promise.resolve();
    });
    await waitFor(() => expect(view.result.current).toBe("The cited issue, renamed"));
    expect(asked).toHaveLength(3);
    expect(asked[2]).toContain(cited);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// A published name stands in for the one a link beside the tree would read of its own
// (`useKnownReferenceName`), and the feed refreshes only a names read whose key holds the address,
// so the page publishes a name only for an address one of its reads asks for now. A plan task's
// words cite an issue, which the one more read names. The task is reworded to cite nothing, and
// a comment arriving reads the tree's new set: nothing is left for the one more read to name, its
// query is disabled and never answers again, and the name it gave goes.
test("once a plan task's words stop citing an address, the page publishes no name for it", async () => {
  const cited = "dispatch://CORE-7";
  let words = `Draw the tree as ${cited} asks.`;
  const asked: string[][] = [];
  const stubs = stubTreeReads(async (refs) => {
    asked.push([...refs]);
    return {
      targets: namedTargets(refs).targets.map((target) => {
        if (target.ref === TASK_REF) {
          return {
            ...target,
            name: `:::plan_task{#t1}\n${words}\n:::`,
            text_tree: {
              content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
              type: "doc",
            },
          };
        }
        return target.ref === cited
          ? { ...target, kind: "issue", name: "The cited issue" }
          : target;
      }),
    };
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
  );
  try {
    const view = renderHook(
      () => {
        useIssueTree(issue);
        return useKnownReferenceName(cited);
      },
      { wrapper }
    );
    await waitFor(() => expect(view.result.current).toBe("The cited issue"));
    expect(asked[1]).toEqual([cited]);

    // The task is reworded to cite nothing, and a comment arrives: the first read of the tree's
    // new set answers the new words.
    words = "Draw the tree from one read.";
    await act(async () => {
      client.setQueryData(["comments", KEY], [comment]);
      await Promise.resolve();
    });
    await waitFor(() => expect(view.result.current).toBeUndefined());
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(asked).toHaveLength(3);
    expect(asked[2]).toContain(TASK_REF);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// The same for the one more read, of what a plan task's words cite: the task's words come to cite
// one issue more, and the read of the two issues fails. The name that read gave before stands,
// beside the first read's names, while the view says the read failed; a read under that key that
// succeeds names the second issue too, and nothing has failed any more.
test("one more read that fails for words that cite more keeps the names it gave, and says it failed", async () => {
  const cited = "dispatch://CORE-7";
  const added = "dispatch://CORE-8";
  let words = `Draw the tree as ${cited} asks.`;
  let failing = false;
  const asked: string[][] = [];
  const stubs = stubTreeReads(async (refs) => {
    asked.push([...refs]);
    // The one more read names only what the task's words cite, so it never names the task.
    if (failing && !refs.includes(TASK_REF)) throw new Error("the names read failed");
    return {
      targets: namedTargets(refs).targets.map((target) => {
        if (target.ref === TASK_REF) {
          return {
            ...target,
            name: `:::plan_task{#t1}\n${words}\n:::`,
            text_tree: {
              content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
              type: "doc",
            },
          };
        }
        if (target.ref === cited) return { ...target, kind: "issue", name: "The cited issue" };
        return target.ref === added
          ? { ...target, kind: "issue", name: "The added issue" }
          : target;
      }),
    };
  });
  // No retry: the read under the new key fails on its first attempt.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
  );
  try {
    const view = renderHook(
      () => {
        const tree = useIssueTree(issue);
        return { names: [useKnownReferenceName(cited), useKnownReferenceName(added)], tree };
      },
      { wrapper }
    );
    await waitFor(() => expect(view.result.current.names).toEqual(["The cited issue", undefined]));
    expect(asked.at(-1)).toEqual([cited]);
    expect(view.result.current.tree.namesFailure).toBeUndefined();

    // The task's words come to cite the second issue too, which the first read of a tree that
    // grew by a comment answers; the one more read, of both issues, fails.
    words = `Draw the tree as ${cited} and ${added} ask.`;
    failing = true;
    await act(async () => {
      client.setQueryData(["comments", KEY], [comment]);
      await Promise.resolve();
    });
    await waitFor(() => expect(view.result.current.tree.namesFailure).toBeDefined());
    expect(asked.at(-1)).toEqual([cited, added]);
    expect(view.result.current.names).toEqual(["The cited issue", undefined]);

    // The failed read is read again, and this time it answers.
    failing = false;
    await act(async () => {
      await client.refetchQueries({ predicate: (query) => query.state.status === "error" });
    });
    await waitFor(() =>
      expect(view.result.current.names).toEqual(["The cited issue", "The added issue"])
    );
    expect(view.result.current.tree.namesFailure).toBeUndefined();
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// The names' line stands for the one more read alone where that read fails and the first holds:
// it counts that read's failures, says it runs again while it does, and its Retry asks that read
// again and not the first. Once that read answers, nothing has failed any more.
test("one more read that fails while the first holds says so, and its Retry asks that read again", async () => {
  const cited = "dispatch://CORE-7";
  const words = `Draw the tree as ${cited} asks.`;
  let failing = true;
  // Where set, the one more read stays out until it settles.
  let out: Promise<void> | undefined;
  const asked: string[][] = [];
  const stubs = stubTreeReads(async (refs) => {
    asked.push([...refs]);
    // The one more read names only what the task's words cite, so it never names the task.
    if (!refs.includes(TASK_REF)) {
      await out;
      if (failing) throw new Error("the names read failed");
    }
    return {
      targets: namedTargets(refs).targets.map((target) => {
        if (target.ref === TASK_REF) {
          return {
            ...target,
            name: `:::plan_task{#t1}\n${words}\n:::`,
            text_tree: {
              content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
              type: "doc",
            },
          };
        }
        return target.ref === cited
          ? { ...target, kind: "issue", name: "The cited issue" }
          : target;
      }),
    };
  });
  // No retry: the one more read fails on its first attempt.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
  );
  try {
    const view = renderHook(
      () => {
        const tree = useIssueTree(issue);
        return { name: useKnownReferenceName(cited), tree };
      },
      { wrapper }
    );
    await waitFor(() => expect(view.result.current.tree.namesFailure).toBeDefined());
    expect(asked, "the first read was not read once, and the one more read after it").toHaveLength(
      2
    );
    expect(asked[1]).toEqual([cited]);
    expect(view.result.current.tree.namesFailure?.failure).toBe(1);
    expect(view.result.current.tree.namesFailure?.retrying).toBe(false);
    expect(view.result.current.name).toBeUndefined();

    // Its Retry reads the one more read again, and the first not: while that read is out the
    // names still stand failed, busy.
    failing = false;
    const again = Promise.withResolvers<void>();
    out = again.promise;
    await act(async () => view.result.current.tree.namesFailure?.retry());
    await waitFor(() => expect(view.result.current.tree.namesFailure?.retrying).toBe(true));
    expect(view.result.current.tree.namesFailure?.failure).toBe(1);
    expect(asked, "the Retry read other than the one more read again").toEqual([
      asked[0],
      [cited],
      [cited],
    ]);
    out = undefined;
    await act(async () => again.resolve());
    await waitFor(() => expect(view.result.current.tree.namesFailure).toBeUndefined());
    expect(view.result.current.name).toBe("The cited issue");
    expect(asked).toHaveLength(3);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// The one more read's kept answer stands only for what it is asked for now. A plan task's words
// cite two issues, which the one more read names. Then the first issue is renamed and a comment
// cites it, so the first read names it now, with its new name; the one more read is asked for the
// second issue alone, and that read fails. Its kept answer still holds the first issue's old name,
// which must not stand over the first read's newer one.
test("a failed one more read's kept answer stands over no name the first read now gives", async () => {
  const cited = "dispatch://CORE-7";
  const added = "dispatch://CORE-8";
  const words = `Draw the tree as ${cited} and ${added} ask.`;
  let title = "The cited issue";
  let failing = false;
  const asked: string[][] = [];
  const stubs = stubTreeReads(async (refs) => {
    asked.push([...refs]);
    // The one more read names only what the task's words cite, so it never names the task.
    if (failing && !refs.includes(TASK_REF)) throw new Error("the names read failed");
    return {
      targets: namedTargets(refs).targets.map((target) => {
        if (target.ref === TASK_REF) {
          return {
            ...target,
            name: `:::plan_task{#t1}\n${words}\n:::`,
            text_tree: {
              content: [{ content: [{ text: words, type: "text" }], type: "paragraph" }],
              type: "doc",
            },
          };
        }
        if (target.ref === cited) return { ...target, kind: "issue", name: title };
        return target.ref === added
          ? { ...target, kind: "issue", name: "The added issue" }
          : target;
      }),
    };
  });
  // No retry: the one more read under its new key fails on its first attempt.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
  );
  try {
    const view = renderHook(
      () => {
        const tree = useIssueTree(issue);
        return { names: [useKnownReferenceName(cited), useKnownReferenceName(added)], tree };
      },
      { wrapper }
    );
    await waitFor(() =>
      expect(view.result.current.names).toEqual(["The cited issue", "The added issue"])
    );
    expect(asked.at(-1)).toEqual([cited, added]);

    // The first issue is renamed and a comment cites it: the first read names it now, and the
    // one more read, of the second issue alone, fails.
    title = "The cited issue, renamed";
    failing = true;
    await act(async () => {
      client.setQueryData(["comments", KEY], [{ ...comment, body: `Read ${cited} again.` }]);
      await Promise.resolve();
    });
    await waitFor(() => expect(view.result.current.tree.namesFailure).toBeDefined());
    expect(asked.some((refs) => refs.length === 1 && refs[0] === added)).toBe(true);
    await waitFor(() =>
      expect(view.result.current.names).toEqual(["The cited issue, renamed", "The added issue"])
    );
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// One block read for every document of the issue was 25 of a first load's 47
// requests. The tree reads the blocks of the spec and of the plan, Verification and decisions
// documents, whose typed blocks the page draws, and of no other document: each the one the issue
// read names by its id, as the server answers which document plays which part.
test("Only the spec and the plan, Verification and decisions documents have their blocks read", async () => {
  const notes = issueDocument("artifact-notes", "notes-md");
  const read = [
    issueDocument("artifact-spec", "spec", { primary: true }),
    plan,
    issueDocument("artifact-verification", "verification-md", { role: "verification" }),
    issueDocument("artifact-decisions", "decisions-md", { role: "decisions" }),
  ];
  const withDocuments: IssueDetails = {
    ...issue,
    artifacts: [...read, notes],
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
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  try {
    const view = renderHook(() => useIssueTree(withDocuments).root, { wrapper });
    // The names are read once every other read has answered, the blocks' among them.
    await waitFor(() => expect(named).toBe(1));
    expect(api.getArtifactBlocks).toHaveBeenCalledTimes(read.length);
    for (const one of read) {
      expect(api.getArtifactBlocks).toHaveBeenCalledWith(one.id);
    }
    expect(api.getArtifactBlocks).not.toHaveBeenCalledWith(notes.id);
    view.unmount();
  } finally {
    for (const stub of stubs) stub.mockRestore();
  }
});

// The server gives a document its part by its role, or, for a row a rolling deploy left with no
// role, by the slug that part is made under, and it omits `role` on such a row. The issue read
// names the document all the same (`plan_artifact_id`), and the tree reads its blocks, and names
// it, by that id.
test("A plan with no role that the issue names by its id has its blocks read and is named the plan", async () => {
  const roleless = issueDocument(plan.id, PLAN);
  const named: IssueDetails = { ...issue, artifacts: [roleless], plan_artifact_id: roleless.id };
  const cites = `dispatch://${KEY}/artifact/${PLAN}`;
  const stubs = stubTreeReads(async (refs) => namedTargets(refs), {
    comments: [{ ...comment, body: `The tasks are in ${cites}.` }],
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  try {
    const view = renderHook(() => useIssueTree(named).root, { wrapper });
    // The plan's task is a row of the tree, which only its blocks being read puts there.
    await waitFor(() => expect(taskWords(view.result.current)).toBe(TASK_WORDS));
    expect(api.getArtifactBlocks).toHaveBeenCalledWith(roleless.id);
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

// The batched read names a typed block by its stored excerpt, which opens on the block's
// directive line. Every link that reads the page's published names draws the name as it is, so
// the page publishes the name a chip naming the block draws (`targetNaming`): the first line of
// the server's reading where the read sends one, and the page's word for the block's kind where
// it sends none - never the directive line.
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
    const stubs = stubTreeReads(async (refs) => ({
      targets: namedTargets(refs).targets.map((target) =>
        target.ref === TASK_REF ? { ...target, ...answer } : target
      ),
    }));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>
        <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
      </QueryClientProvider>
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

// A refetch of a document's blocks that fails keeps the blocks
// the first read answered - TanStack holds a query's data through a failed refetch, and the tree
// is still built from them - so a view that draws from the blocks it holds keeps drawing them
// (`blocksHeld`), while a view that needs every read to have answered stops waiting on one that
// failed (`blocksRead`). Before any answer the tree holds no blocks and has read none.
test("blocks a failed refetch leaves in hand stay held while they stop counting as read", async () => {
  const blocksOut = Promise.withResolvers<void>();
  const stubs = stubTreeReads(async (refs) => namedTargets(refs), {
    held: { read: "blocks", until: blocksOut.promise },
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  try {
    const view = renderHook(() => useIssueTree(issue), { wrapper });
    expect(view.result.current.blocksHeld).toBe(false);
    expect(view.result.current.blocksRead).toBe(false);

    await act(async () => {
      blocksOut.resolve();
      await blocksOut.promise;
    });
    await waitFor(() => expect(view.result.current.blocksRead).toBe(true));
    expect(view.result.current.blocksHeld).toBe(true);

    // The plan's blocks are read again, and this time the read fails.
    (api.getArtifactBlocks as Mock<typeof api.getArtifactBlocks>).mockRejectedValue(
      new Error("the blocks read failed")
    );
    await act(async () => {
      await client.refetchQueries({ queryKey: ["artifact", plan.id, "blocks"] });
    });
    await waitFor(() => expect(view.result.current.blocksRead).toBe(false));
    expect(view.result.current.blocksHeld).toBe(true);
    // The tree is still built from the blocks the first read answered.
    expect(nodesInOrder(view.result.current.root).some((node) => node.id === `${PLAN}#b-t1`)).toBe(
      true
    );
    view.unmount();
  } finally {
    blocksOut.resolve();
    for (const stub of stubs) stub.mockRestore();
  }
});

// The page's stage mounts the tree's slot again on every
// press of a view, and under the app's `staleTime` a read is stale 30 s after it answered
// (`main.tsx`). The tree's reads are kept fresh by events instead: the slot mounted again more
// than 30 s on draws from what the client holds and sends no read, and a comment's edit reads the
// comments again and the names, and nothing else.
test("a tree mounted again past 30 s reads nothing, and a comment event reads the comments", async () => {
  const stored: Comment[] = [comment];
  const stubs = stubTreeReads(async (refs) => namedTargets(refs), { comments: stored });
  const requests = () => stubs.reduce((sent, stub) => sent + stub.mock.calls.length, 0);
  // The app's client's `staleTime` (`main.tsx`), which a read that sets none of its own takes.
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
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
    // The state a read the mount started would show: React Query marks a query fetching in the
    // mount's own effect, which `act` flushes, before any request goes out, so a mount that read
    // again reads fetching here; no clock decides it.
    await act(async () => {});
    expect(client.isFetching(), "a read the mount started").toBe(0);
    expect(requests(), "no read is sent for a slot mounted again").toBe(loaded);

    // The comment is edited, and the feed refreshes what that event names.
    const edited: Comment = {
      ...comment,
      body: "One more turn, reworded.",
      edited_at: "2026-11-05T02:00:00Z",
    };
    stored[0] = edited;
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
    expect(requests(), "the event sends two reads, the comments' and the names'").toBe(loaded + 2);
    again.unmount();
  } finally {
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
  ["dispatch://ACME-2", "dispatch://ACME-2/comment/comment-1"],
  ["dispatch://CORE/artifact/roadmap-md"],
];

// A names read is keyed by the addresses it names, so only words that change under an address
// already named need a rule - a comment's edit, an ask's edit and a document's new version - and
// so do a document's rename and its removal, which rename or empty every address to it, and an
// issue's write that changes its title, which renames the issue's own address. The feed is the
// workspace's, so each refreshes only the names reads whose key names an address it can change of
// the issue the event is on: an event on an issue no read names changes no name that read holds,
// and only a title change changes the name of the issue's own address, which the names read
// answers with the issue's title and nothing else. A child's page names its parent by that
// address, and the parent's documents take new versions often: none of those, nor any other
// change under the parent, reads the child's names again, where the parent's new title, and an
// older server's update that names none of its changes, does. A new comment is a new address,
// which the tree reads under a key of its own, a person's edit of a document is the notice after
// the versions that already refreshed them, and an issue's write that leaves its title alone and
// a follower joining an ask rename nothing.
test("an edit on an issue refreshes the names reads whose key names the issue and no other, a new comment none", () => {
  const editsWords = [
    feedEvent("comment.edited", { anchor: null, ask_id: null, id: "comment-1" }),
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
    "dispatch://CORE-12/comment/comment-1",
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
// `dispatch://CORE/artifact/roadmap-md`. A rename, a removal, a comment's edit, the edit of an
// ask its anchor puts on the document, or of a block ask through the ask's own route, and an
// approval request's rewording carry the slug, so they leave a read naming another document of
// the project alone too, `roadmap-md-2` among them; a new version and the edit of a question
// asked of the document as a whole name the document by its id alone, as a version's rewording
// of a block ask does, so they are held to the project's documents, which leaves another
// project's documents and every issue alone.
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
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>{children}</ReferenceNamesProvider>
    </QueryClientProvider>
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
