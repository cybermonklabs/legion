import { expect, type Mock, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { settle } from "../../__tests__/fake-clock";
import { memoryFiles, UnloadTarget } from "../../__tests__/kept-sends-harness";
import { answerIssuePatches } from "../../__tests__/send-answers";
import type { WriteBounds } from "../../api/client";
import type { Issue, IssueDetails, UpdateIssueInput } from "../../api/types";
import { WriteStorage } from "../writes/write-storage";
import { Writes, writesOf } from "../writes/write-store";
import { issuePriority } from "./issue-priority";
import { PriorityControl } from "./PriorityControl";

/**
 * What order the server is sent one field's writes in (R5 of dispatch://INFRA-299): a pick joins
 * the issue's line in the write store when it is pressed, so the value the server stores last is
 * the newest pick the person made, and no pick they saw is lost. Each row reads the requests the
 * server was sent, in order, rather than a bare count: when the first save goes out decides the
 * count, never which value lands last.
 */
const issue: Issue = {
  assignee: null,
  blocked_by: [],
  claim: null,
  closed_at: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: "",
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  plan_artifact_id: "",
  primary_artifact_id: "artifact-1",
  priority: 1,
  project: "CORE",
  rank: "U",
  repo: null,
  resolved_repo: null,
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
  verification_artifact_id: "",
};
const details: IssueDetails = {
  ...issue,
  artifacts: [],
  children: [],
  design_gates: [],
  holder_state: null,
  last_seen: null,
  open_asks: [],
  progress: { tasks: null, children: null },
  referenced_by_count: 0,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  takeable_at: null,
};

const detailKey = ["issue", issue.key] as const;

/** The issue patches this file answers (`answerIssuePatches`), kept under one name so a row can
 *  hand them to the helpers below. */
type IssuePatches = Mock<
  (key: string, input: UpdateIssueInput, bounds: WriteBounds) => Promise<Issue>
>;

/** The server's answer to a priority patch: the issue as that patch leaves it. */
function savedWith(update: UpdateIssueInput): Issue {
  return { ...issue, priority: update.priority ?? null };
}

/** The page's query client, signed in as the app's is: the store stamps every write with this
 *  login at its press, and a reload resumes that person's writes alone. */
function pageClient(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(detailKey, details);
  return client;
}

/** One surface that writes CORE-1's priority, showing the value it reads from the cache: the
 *  issue page's header, a list row and a board card are each one of these. */
function Surface({ name }: { name: string }): ReactNode {
  const cached = useQuery<IssueDetails>({
    queryKey: detailKey,
    queryFn: () => Promise.reject(new Error("the test seeds this query")),
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (cached.data === undefined) {
    return null;
  }
  return (
    <section aria-label={name}>
      <PriorityControl issueKey={cached.data.key} priority={issuePriority(cached.data.priority)} />
    </section>
  );
}

/** The app `client` holds, showing one surface per name. */
function surfaces(client: QueryClient, ...names: string[]): ReactNode {
  return (
    <QueryClientProvider client={client}>
      {names.map((name) => (
        <Surface key={name} name={name} />
      ))}
    </QueryClientProvider>
  );
}

/** The priority picker of the surface named `name`. */
function select(name = "Issue page"): HTMLSelectElement {
  return within(screen.getByRole("region", { name })).getByRole("combobox", {
    name: "Priority of CORE-1",
  }) as HTMLSelectElement;
}

/** The priorities the server was sent, in the order the requests reached it. */
function sentPriorities(patchIssue: IssuePatches): (number | null)[] {
  return patchIssue.mock.calls.map(([, update]) => update.priority ?? null);
}

/** Answers the first save with `first` and every save after it with the issue that patch leaves,
 *  so what reaches the server is read from the calls rather than from when each one answers. */
function answerSaves(first: Promise<Issue>): IssuePatches {
  return answerIssuePatches()
    .mockImplementationOnce(() => first)
    .mockImplementation((_key, update) => Promise.resolve(savedWith(update)));
}

// R5. The second pick waits for the first save and reaches the server after it, so the value the
// server stores last is the newest pick. This holds today: a correction that sends each pick as
// it is pressed, or that drops the pick made while a save is out, fails here.
test("a second pick made while the first save is out reaches the server after it", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerSaves(first.promise);
  const user = userEvent.setup();
  const view = render(surfaces(pageClient(), "Issue page"));
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await user.selectOptions(select(), "2");
    // One request at a time on the issue's line: nothing of the second pick goes out while the
    // first save is still out.
    expect(sentPriorities(patchIssue)).toEqual([0]);

    await act(async () => {
      first.resolve(savedWith({ priority: 0 }));
      await first.promise;
    });
    // Whatever the first save freed reaches the server before the order is read.
    await act(settle);
    expect(sentPriorities(patchIssue)).toEqual([0, 2]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// R5. A pick, a second pick while that save is out, the issue page left and opened again, and a
// third pick: the page that opened again holds nothing of the second pick, so only the store can
// keep the three in press order. The end the requirement asks for is that the last value the
// server is sent is the newest pick, the third.
test("a pick, a second pick, the issue page left and opened again, and a third pick reach the server in press order", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerSaves(first.promise);
  const user = userEvent.setup();
  const client = pageClient();
  const opened = render(surfaces(client, "Issue page"));
  let openedAgain: RenderResult | undefined;
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await user.selectOptions(select(), "2");
    expect(sentPriorities(patchIssue)).toEqual([0]);

    // The person leaves the issue and opens it again: the same app, a new page.
    opened.unmount();
    openedAgain = render(surfaces(client, "Issue page"));
    await user.selectOptions(select(), "3");

    await act(async () => {
      first.resolve(savedWith({ priority: 0 }));
      await first.promise;
    });
    // Every request the first save's answer freed reaches the server before the order is read.
    await act(settle);
    expect(sentPriorities(patchIssue)).toEqual([0, 2, 3]);
  } finally {
    openedAgain?.unmount();
    opened.unmount();
    patchIssue.mockRestore();
  }
});

// R5. Two surfaces that write one field of one issue, mounted at once - the issue page's header
// and a board card beside it. Neither knows of the other's save, and the store's line is what
// orders them: the newer pick is sent last, so it is the value the server stores.
test("two surfaces that write one field of one issue send the newer pick last", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerSaves(first.promise);
  const user = userEvent.setup();
  const view = render(surfaces(pageClient(), "Issue page", "Board card"));
  try {
    await user.selectOptions(select("Issue page"), "0");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await user.selectOptions(select("Board card"), "2");
    expect(sentPriorities(patchIssue)).toEqual([0]);

    await act(async () => {
      first.resolve(savedWith({ priority: 0 }));
      await first.promise;
    });
    await act(settle);
    expect(sentPriorities(patchIssue)).toEqual([0, 2]);
  } finally {
    view.unmount();
    patchIssue.mockRestore();
  }
});

// R5. A reload inside the store's resume window (`KEPT_SEND_RESUME_MS`) while a pick has not gone
// out: the reloaded page is a second `Writes` over the writes the tab kept, as
// `two writes on one line survive a reload and land once each, in press order` builds one in
// `features/writes/write-store.test.ts`. A pick the person saw is kept like every other write,
// so the reload sends the first save again under its key and then that pick.
test("a pick that has not gone out is sent after a reload inside the store's resume window", async () => {
  const first = Promise.withResolvers<Issue>();
  const patchIssue = answerSaves(first.promise);
  const user = userEvent.setup();
  const client = pageClient();
  const view = render(surfaces(client, "Issue page"));
  // The tab's own kept writes, which a reload reads: the page under test keeps its writes in the
  // same `sessionStorage`.
  const kept = new WriteStorage(window.sessionStorage, new UnloadTarget());
  try {
    await user.selectOptions(select(), "0");
    await waitFor(() => expect(patchIssue).toHaveBeenCalledTimes(1));
    await user.selectOptions(select(), "2");
    expect(sentPriorities(patchIssue)).toEqual([0]);

    // The reload: the page and all it held in memory are gone, and the app that loads takes up
    // what the tab kept.
    view.unmount();
    const reloaded = new Writes(pageClient(), kept, memoryFiles());
    reloaded.resume("alice");
    await act(settle);
    // The first save goes out again under its own key, which Dispatch answers with what it
    // answered the first time; the pick that never went out follows it.
    expect(sentPriorities(patchIssue)).toEqual([0, 0, 2]);
  } finally {
    // The page the reload replaced is gone with it: its writes stop rather than go on trying
    // through the real client once this test's answers are taken back.
    writesOf(client).clear();
    kept.retire();
    view.unmount();
    patchIssue.mockRestore();
  }
});
