import { beforeAll, expect, spyOn, test } from "bun:test";
import { hashKey, QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { settle } from "../../__tests__/fake-clock";
import { api } from "../../api/client";
import { userStateQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { IssueDetails, UserIssueState, UserState } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { IssueHeader } from "./IssueHeader";
import { stateForIssue } from "./pins";
import type { IssuePriorityWrite } from "./useIssuePriority";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The issue the header draws: unclaimed, at the root of its project, with no document. */
const issue: IssueDetails = {
  advances: [],
  advances_revision: 0,
  artifacts: [],
  assignee: "alice",
  blocked_by: [],
  children: [],
  claim: null,
  closed_at: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  created_at: "2026-09-24T05:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: "",
  design_gates: [],
  external_links: [],
  holder_state: null,
  key: "CORE-1",
  labels: [],
  last_seen: null,
  last_seq: 1,
  number: 1,
  open_asks: [],
  parent: null,
  plan_artifact_id: "",
  primary_artifact_id: "",
  priority: null,
  progress: { tasks: null, children: null },
  project: "CORE",
  rank: "U",
  referenced_by_count: 0,
  repo: null,
  resolved_repo: null,
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  status: "in_progress",
  takeable_at: null,
  title: "Pin the issue from its header",
  updated_at: "2026-09-24T06:00:00Z",
  verification_artifact_id: "",
};

/** These rows are about the pin: the header's priority write never runs, and `issue` has none. */
const idlePriorityWrite: IssuePriorityWrite = {
  failed: false,
  pending: false,
  retry: () => undefined,
  shown: null,
  stopped: undefined,
  submit: () => undefined,
};

/** The viewer's state of `issue` before the pin: unpinned. */
const unpinnedState = stateForIssue(undefined, issue.key);

/** The viewer's state as Dispatch holds it before the pin. */
const unpinned: UserState = { [issue.key]: unpinnedState };

/** The pin's PUT's answer: Dispatch's state of the issue once it took the pin. */
const pinnedState: UserIssueState = { ...unpinnedState, pinned: true, seq: 1 };

/** The header given the viewer's state of `issue`. */
function Header({ state }: { state: UserIssueState }): ReactNode {
  return (
    <IssueHeader
      documentArtifact={undefined}
      isClosed={false}
      issue={issue}
      priorityWrite={idlePriorityWrite}
      state={state}
    />
  );
}

/** The header as the issue page draws it: given the viewer's state of the issue from the page's
 *  one read of that state (`useIssueDetail`). */
function PageHeader(): ReactNode {
  const userState = useQuery(userStateQuery());
  return <Header state={stateForIssue(userState.data, issue.key)} />;
}

/** `ui` on `client`, signed in as alice, with every read of the viewer's state (`getMyState`) and
 *  the pin's PUT (`pinSave`) held until the test answers it. The header's other reads find a
 *  project with no repository mapping, architecture source or subscriber. */
function renderHeader(client: QueryClient, ui: ReactNode) {
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const mappings = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const architectureSources = spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]);
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const reads: ReturnType<typeof Promise.withResolvers<UserState>>[] = [];
  const getMyState = spyOn(api, "getMyState").mockImplementation(() => {
    const read = Promise.withResolvers<UserState>();
    reads.push(read);
    return read.promise;
  });
  const pinSave = Promise.withResolvers<UserIssueState>();
  const putIssueState = spyOn(api, "putIssueState").mockReturnValue(pinSave.promise);
  const view = render(ui, {
    wrapper: ({ children }: { children: ReactNode }) => (
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <MarginProvider>{children}</MarginProvider>
        </QueryClientProvider>
      </MemoryRouter>
    ),
  });
  return {
    getMyState,
    pinSave,
    putIssueState,
    q: within(view.container),
    reads,
    unmount: () => {
      view.unmount();
      architectureSources.mockRestore();
      getMyState.mockRestore();
      mappings.mockRestore();
      putIssueState.mockRestore();
      subscribers.mockRestore();
    },
    user: userEvent.setup(),
  };
}

// The pin's answer went into the cache under a read of the viewer's state asked while the PUT was
// out, and that read's answer, of the state from before Dispatch took the pin, landed over it: the
// issue read as unpinned again wherever the state was next drawn. Nothing reads the state here,
// as when the reader has left the page, so the pin's settling does not read it again over that.
test("a read of the state asked while the header's pin is out does not put back the unpinned state its answer replaced", async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = renderHeader(client, <Header state={unpinnedState} />);
  try {
    const first = client.fetchQuery(userStateQuery());
    await act(async () => {
      view.reads[0]?.resolve(unpinned);
      await first;
    });
    await view.user.click(view.q.getByRole("button", { name: "Pin issue" }));
    await waitFor(() => expect(view.putIssueState).toHaveBeenCalledTimes(1));
    // While the PUT is out the state is read again; Dispatch has not taken the pin yet.
    act(() => {
      void client.refetchQueries({ queryKey: userStateQuery().queryKey });
    });
    await waitFor(() => expect(view.getMyState).toHaveBeenCalledTimes(2));

    await act(async () => {
      view.pinSave.resolve(pinnedState);
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await act(async () => {
      view.reads[1]?.resolve(unpinned);
      await settle();
    });
    expect(client.getQueryData<UserState>(userStateQuery().queryKey)?.[issue.key]).toEqual(
      pinnedState
    );
  } finally {
    view.unmount();
  }
});

// A refresh of the viewer's state asked while the header's pin was out could reach Dispatch
// before the pin did, and its answer turned the pin back off under the reader's press. The read
// waits for the pin's answer and is asked once it is in, so the header shows what Dispatch holds
// after the pin.
test("a read of the state asked while the header's pin is out waits for its answer, and the header shows what it reads then", async () => {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  // Each read of the state, listed as it calls its function: a read the pin holds is listed
  // before it waits, and one that is not held has asked by then.
  const asked: string[] = [];
  const stopListening = client.onAttempt((queryHash) => {
    if (queryHash === hashKey(userStateQuery().queryKey)) asked.push(queryHash);
  });
  const view = renderHeader(client, <PageHeader />);
  // Dispatch's state once it took the pin, with a read the reader made elsewhere since.
  const after: UserIssueState = { ...pinnedState, last_read_seq: 7 };
  try {
    await waitFor(() => expect(view.getMyState).toHaveBeenCalledTimes(1));
    await act(async () => {
      view.reads[0]?.resolve(unpinned);
    });
    const pin = await view.q.findByRole("button", { name: "Pin issue" });
    expect(pin).toHaveAttribute("aria-pressed", "false");

    await view.user.click(pin);
    await waitFor(() => expect(view.putIssueState).toHaveBeenCalledTimes(1));
    expect(view.q.getByRole("button", { name: "Unpin issue" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    await act(async () => {
      void client.invalidateQueries({ queryKey: userStateQuery().queryKey });
    });
    // The refresh is asked, and waits: the state is read once, before the pin.
    await waitFor(() => expect(asked).toHaveLength(2));
    expect(view.getMyState).toHaveBeenCalledTimes(1);
    expect(view.q.getByRole("button", { name: "Unpin issue" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    await act(async () => {
      view.pinSave.resolve(pinnedState);
    });
    await waitFor(() => expect(view.getMyState).toHaveBeenCalledTimes(2));
    view.getMyState.mockResolvedValue({ [issue.key]: after });
    await act(async () => {
      view.reads[1]?.resolve({ [issue.key]: after });
    });
    await waitFor(() =>
      expect(client.getQueryData<UserState>(userStateQuery().queryKey)?.[issue.key]).toEqual(after)
    );
    await waitFor(() => expect(client.isMutating()).toBe(0));
    const shown = view.q.getByRole("button", { name: "Unpin issue" });
    expect(shown).toHaveAttribute("aria-pressed", "true");
    expect(shown).toBeEnabled();
  } finally {
    view.unmount();
    stopListening();
  }
});
