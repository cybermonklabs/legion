import { expect, spyOn, test } from "bun:test";
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import {
  bodies,
  details as issueDetails,
  issue as pickedIssue,
  repoSuggestions,
  signedInHolding,
  typeRepo,
} from "../../__tests__/issue-picks";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { issueQuery } from "../../api/queries";
import type { DispatchQueryClient } from "../../api/query-client";
import type { Issue, IssueDetails, Project, UpdateIssueInput } from "../../api/types";
import { writesOf } from "../writes/write-store";
import { AssigneeControl } from "./AssigneeControl";
import { RepoControl } from "./RepoControl";

// The rows of dispatch://INFRA-299 that `issue-field-writes.test.tsx` runs for the priority, run
// here for the assignee and the repository through their own pickers: each pick joins the page's
// write store on its issue's line (`issue:<KEY>`) as it is made, and picks nobody has sent are
// one request, the newest.

/** CORE-1 on its project's default repository, which it names none of its own for. */
const issue: Issue = { ...pickedIssue, resolved_repo: "acme/widgets" };
const details: IssueDetails = { ...issueDetails, ...issue };

const detailKey = issueQuery(issue.key).queryKey;

/** CORE, whose default repository, acme/widgets, is the one repository the box suggests. */
const projects: Project[] = [
  {
    created_at: "2026-10-01T00:00:00Z",
    default_repo: "acme/widgets",
    gated_steps: [],
    key: "CORE",
    name: "Core",
    open_asks: 0,
  },
];

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

/** Renders the picker of `field` on CORE-1, with the people who have signed in read once the
 *  reader reaches for it, and the project list. The assignee's picker is a `<select>`; the
 *  repository's is a box, which suggests the project's default repository. */
async function renderPicker(field: "assignee" | "repo") {
  const listUsers = spyOn(api, "listUsers").mockResolvedValue(
    ["alice", "bob", "carol", "dave"].map((login) => ({ login }))
  );
  const listProjects = spyOn(api, "listProjects").mockResolvedValue(projects);
  const server = dispatch();
  // The page's query client, signed in, holding CORE-1 as Dispatch last answered it.
  const client = signedInHolding([detailKey, details]);
  const view = render(
    <QueryClientProvider client={client}>
      <Picker field={field} read={server.read} />
    </QueryClientProvider>
  );
  const name = field === "assignee" ? "Assignee of CORE-1" : "Repository of CORE-1";
  const select = () => screen.getByRole("combobox", { name }) as HTMLSelectElement;
  const box = () => screen.getByRole("combobox", { name }) as HTMLInputElement;
  const user = userEvent.setup();
  // What the control offers: the select's people, Unassigned first, read once the reader reaches
  // for it, or the box's suggestion, the project's default repository from the project list.
  if (field === "assignee") {
    await user.hover(select());
    await waitFor(() => expect(select().options.length).toBe(5));
  } else {
    await waitFor(() => expect(repoSuggestions(box())).toEqual(["acme/widgets"]));
  }
  return {
    ...server,
    client,
    /** The picker, whose value is what it shows. */
    control: field === "assignee" ? select : box,
    // A pick of the select is its option; a pick of the box is the repository typed into it,
    // written as Enter is pressed.
    pick:
      field === "assignee"
        ? (value: string) => user.selectOptions(select(), value)
        : (value: string) => typeRepo(user, box, value),
    unmount: () => {
      view.unmount();
      server.patchIssue.mockRestore();
      listProjects.mockRestore();
      listUsers.mockRestore();
    },
  };
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
  const { client, control, patchIssue, pick, release, unmount } = await renderPicker("assignee");
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
    expect(control().value).toBe("dave");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    await act(async () => release());
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ assignee: "bob" }, { assignee: "dave" }]);
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { assignee: "dave" }, keyedAttempt);
    expect(control().value).toBe("dave");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    unmount();
  }
});

test("three repository picks made while the first is out send two requests, the first and the newest, and no error shows", async () => {
  const { client, control, patchIssue, pick, release, unmount } = await renderPicker("repo");
  try {
    await pick("acme/gadgets");
    await pick("acme/tools");
    await pick("acme/widgets");
    expect(line(client)).toEqual([
      ["CORE-1 on acme/gadgets", "issue:CORE-1", "sending"],
      ["CORE-1 on acme/widgets", "issue:CORE-1", "queued"],
    ]);
    expect(control().value).toBe("acme/widgets");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    await act(async () => release());
    await waitFor(() => expect(writesOf(client).rows()).toEqual([]));
    expect(bodies(patchIssue)).toEqual([{ repo: "acme/gadgets" }, { repo: "acme/widgets" }]);
    expect(patchIssue).toHaveBeenLastCalledWith("CORE-1", { repo: "acme/widgets" }, keyedAttempt);
    expect(control().value).toBe("acme/widgets");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  } finally {
    unmount();
  }
});
