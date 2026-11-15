import { expect, spyOn, test } from "bun:test";
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { issueQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { Issue, IssueDetails, RepoProject, UpdateIssueInput } from "../../api/types";
import { writesOf } from "../writes/write-store";
import { AssigneeControl } from "./AssigneeControl";
import { RepoControl } from "./RepoControl";

// The rows of dispatch://INFRA-299 that `issue-field-writes.test.tsx` runs for the priority, run
// here for the assignee and the repository through their own pickers: each pick joins the page's
// write store on its issue's line (`issue:<KEY>`) as it is made, and picks nobody has sent are
// one request, the newest.

const issue: Issue = {
  advances: [],
  advances_revision: 0,
  assignee: "alice",
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
  resolved_repo: "acme/widgets",
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
  verification_artifact_id: "",
};
const details: IssueDetails = {
  holder_state: null,
  last_seen: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  takeable_at: null,
  ...issue,
  artifacts: [],
  children: [],
  design_gates: [],
  open_asks: [],
  progress: { tasks: null, children: null },
  referenced_by_count: 0,
};

const detailKey = issueQuery(issue.key).queryKey;

const mappings: RepoProject[] = ["acme/widgets", "acme/gadgets", "acme/tools"].map((repo) => ({
  created_at: "2026-10-01T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  default: repo === "acme/widgets",
  project: "CORE",
  renamed_to: null,
  repo,
}));

/**
 * Dispatch's side of CORE-1's saves and reads: the first save is held until the row lets it go
 * (`release`), each save stores its change, and the issue's read answers what Dispatch stores
 * then. A repository the issue stops naming leaves it on the project's default.
 */
function dispatch() {
  const held = Promise.withResolvers<void>();
  let stored = issue;
  const store = (input: UpdateIssueInput): Issue => {
    const repo = input.repo === undefined ? {} : { resolved_repo: input.repo ?? "acme/widgets" };
    stored = { ...stored, ...input, ...repo, last_seq: stored.last_seq + 1 } as Issue;
    return stored;
  };
  const patchIssue = answerIssuePatches()
    .mockImplementationOnce(async (_key, input) => {
      await held.promise;
      return store(input);
    })
    .mockImplementation(async (_key, input) => store(input));
  return {
    patchIssue,
    read: async (): Promise<IssueDetails> => ({ ...details, ...stored }),
    release: () => held.resolve(),
  };
}

/** The page's query client, signed in, holding CORE-1 as Dispatch last answered it. */
function signedIn(): DispatchQueryClient {
  const client = new DispatchQueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(detailKey, details);
  return client;
}

/** Each picker as the issue page renders it: with the field it reads from the issue. */
function Picker({
  field,
  read,
}: {
  field: "assignee" | "repo";
  read: () => Promise<IssueDetails>;
}): ReactNode {
  const cached = useQuery({
    ...issueQuery(issue.key),
    queryFn: read,
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (cached.data === undefined) return null;
  return field === "assignee" ? (
    <AssigneeControl assignee={cached.data.assignee} issueKey={cached.data.key} />
  ) : (
    <RepoControl
      issueKey={cached.data.key}
      project={cached.data.project}
      repo={cached.data.repo}
      resolvedRepo={cached.data.resolved_repo}
    />
  );
}

/** Renders the picker of `field` on CORE-1, with the people who have signed in and the
 *  project's repositories read once the reader reaches for it. */
async function renderPicker(field: "assignee" | "repo") {
  const listUsers = spyOn(api, "listUsers").mockResolvedValue(
    ["alice", "bob", "carol", "dave"].map((login) => ({ login }))
  );
  const listRepoProjects = spyOn(api, "listRepoProjects").mockResolvedValue(mappings);
  const server = dispatch();
  const client = signedIn();
  const view = render(
    <QueryClientProvider client={client}>
      <Picker field={field} read={server.read} />
    </QueryClientProvider>
  );
  const name = field === "assignee" ? "Assignee of CORE-1" : "Repository of CORE-1";
  const select = () => screen.getByRole("combobox", { name }) as HTMLSelectElement;
  const user = userEvent.setup();
  // Its options are read once the reader reaches for the control: Unassigned or Inherit first.
  await user.hover(select());
  await waitFor(() => expect(select().options.length).toBe(field === "assignee" ? 5 : 4));
  return {
    ...server,
    client,
    pick: (value: string) => user.selectOptions(select(), value),
    select,
    unmount: () => {
      view.unmount();
      server.patchIssue.mockRestore();
      listRepoProjects.mockRestore();
      listUsers.mockRestore();
    },
  };
}

/** What each save Dispatch received carried. */
function bodies(patchIssue: { mock: { calls: unknown[][] } }): unknown[] {
  return patchIssue.mock.calls.map((call) => call[1]);
}

/** Each write of CORE-1's line in the store: its label, its line and where it stands. */
function line(client: DispatchQueryClient) {
  return writesOf(client)
    .rows()
    .map((row) => [row.label, row.target, row.status]);
}

test("each assignee pick in the issue's picker joins the issue's line in the write store when it is made", async () => {
  const { client, patchIssue, pick, unmount } = await renderPicker("assignee");
  try {
    await pick("bob");
    await pick("carol");
    // The second pick is the store's from its press, behind the first on `issue:CORE-1`.
    expect(line(client)).toEqual([
      ["CORE-1 assigned to bob", "issue:CORE-1", "sending"],
      ["CORE-1 assigned to carol", "issue:CORE-1", "queued"],
    ]);
    expect(bodies(patchIssue)).toEqual([{ assignee: "bob" }]);
  } finally {
    unmount();
  }
});

test("each repository pick in the issue's picker joins the issue's line in the write store when it is made", async () => {
  const { client, patchIssue, pick, unmount } = await renderPicker("repo");
  try {
    await pick("acme/gadgets");
    await pick("acme/tools");
    expect(line(client)).toEqual([
      ["CORE-1 on acme/gadgets", "issue:CORE-1", "sending"],
      ["CORE-1 on acme/tools", "issue:CORE-1", "queued"],
    ]);
    expect(bodies(patchIssue)).toEqual([{ repo: "acme/gadgets" }]);
  } finally {
    unmount();
  }
});

test("three assignee picks made while the first is out send two requests, the first and the newest, and no error shows", async () => {
  const { client, patchIssue, pick, release, select, unmount } = await renderPicker("assignee");
  try {
    await pick("bob");
    await pick("carol");
    await pick("dave");
    // The pick of carol, which nobody sent, is taken out by the pick of dave and puts nothing
    // back.
    expect(line(client)).toEqual([
      ["CORE-1 assigned to bob", "issue:CORE-1", "sending"],
      ["CORE-1 assigned to dave", "issue:CORE-1", "queued"],
    ]);
    expect(select().value).toBe("dave");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    await act(async () => release());
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ assignee: "bob" }, { assignee: "dave" }]);
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { assignee: "dave" }, keyedAttempt);
    expect(select().value).toBe("dave");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    unmount();
  }
});

test("three repository picks made while the first is out send two requests, the first and the newest, and no error shows", async () => {
  const { client, patchIssue, pick, release, select, unmount } = await renderPicker("repo");
  try {
    await pick("acme/gadgets");
    await pick("acme/tools");
    await pick("acme/widgets");
    expect(line(client)).toEqual([
      ["CORE-1 on acme/gadgets", "issue:CORE-1", "sending"],
      ["CORE-1 on acme/widgets", "issue:CORE-1", "queued"],
    ]);
    expect(select().value).toBe("acme/widgets");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    await act(async () => release());
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ repo: "acme/gadgets" }, { repo: "acme/widgets" }]);
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { repo: "acme/widgets" }, keyedAttempt);
    expect(select().value).toBe("acme/widgets");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    unmount();
  }
});
