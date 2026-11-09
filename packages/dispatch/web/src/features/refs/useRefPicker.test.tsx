import { afterEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type {
  Agent,
  ArchitectureTree,
  GraphReferences,
  IssueCollaborators,
  SearchResponse,
} from "../../api/types";
import { RefPickerList } from "./RefPickerList";
import { applyInsertion, REF_PICKER_INSTRUCTION, REF_PICKER_WORDS } from "./ref-picker";
import { useRefPicker } from "./useRefPicker";

const now = Date.now();

/** `[` opens a key descriptor in user-event's keyboard syntax, so each one is doubled to type
 *  the character: this is the `[[` door typed out. */
const BRACKETS = "[[[[";

const planner: Agent = {
  capabilities: ["btw"],
  dir: "/workspaces/planner",
  host: "build-host",
  last_activity: new Date(now).toISOString(),
  last_seen: now,
  machine_id: "build-host",
  open_asks: 0,
  roles: ["planner"],
  session_id: "planner-session",
  title: "Planner",
};

const worker: Agent = {
  ...planner,
  dir: "/workspaces/worker",
  last_seen: now - 1_000,
  roles: [],
  session_id: "worker-session",
  title: "Worker",
};

const collaborators: IssueCollaborators = {
  agents: [
    {
      kind: "session",
      last_at: "2026-11-01T00:00:00Z",
      legion: {
        replaced_at: null,
        replaced_by: null,
        role: "reviewer",
        role_token: "reviewer",
        started_at: "2026-11-01T00:00:00Z",
      },
      reasons: ["posted"],
      session_id: "reviewer-session",
      title: "Reviewer",
    },
  ],
  issue_key: "CORE-1",
  legion_registry: "ok",
  people: [{ kind: "user", last_at: null, login: "alice@theorem.dev", reasons: ["assignee"] }],
};

const architecture = {
  components: [
    {
      depends_on: [],
      done: 0,
      external: false,
      id: "relay",
      issues: [],
      own_done: 0,
      own_total: 0,
      parent: null,
      paths: [],
      prose: "",
      source: "cybermonklabs/legion",
      title: "The relay",
      total: 0,
    },
  ],
  not_architectural: [],
  retired_links: [],
  sources: [],
  totals: {
    components_without_work: 1,
    issues_done: 0,
    issues_total: 0,
    not_architectural: 0,
    retired_links: 0,
    unassigned: 0,
  },
  unassigned: [],
} satisfies ArchitectureTree;

const hits: SearchResponse = {
  results: [
    {
      href: "/issues/CORE-2",
      id: "CORE-2",
      kind: "issue",
      owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
      rank: 0.1,
      snippet: "The relay drops a reply",
    },
    {
      href: "/issues/CORE-2/comments/comment-1",
      id: "comment-1",
      kind: "comment",
      owner: { key: "CORE-2", kind: "issue", status: "open", title: "Relay drops a reply" },
      rank: 0.09,
      snippet: "I saw the <mark>relay</mark> drop one",
    },
  ],
  took_ms: 2,
};

const blocks: GraphReferences = {
  edges: [
    {
      created_at: "2026-11-01T00:00:00Z",
      direction: "in",
      excerpt: { text: "The first words of the block" },
      kind: "part_of",
      node: { block_id: "block-1", id: "comment-1", kind: "block" },
      source_seq: null,
    },
  ],
  node: { id: "comment-1", kind: "comment" },
};

interface Pane {
  getArchitecture: Mock<typeof api.getArchitecture>;
  getIssueCollaborators: Mock<typeof api.getIssueCollaborators>;
  getReferences: Mock<typeof api.getReferences>;
  listAgents: Mock<typeof api.listAgents>;
  listUsers: Mock<typeof api.listUsers>;
  restore: () => void;
  search: Mock<typeof api.search>;
  unmount: () => void;
}

let pane: Pane | undefined;

afterEach(() => {
  pane?.unmount();
  pane?.restore();
  pane = undefined;
});

/**
 * A plain `<textarea>` host for the picker: it owns the text and the caret, hands the picker
 * each key, and applies the insertion the picker answers with. The three real hosts do the same
 * with their own documents, which is what this interface is for.
 */
function Host({ issueKey }: { issueKey?: string }): ReactNode {
  const [draft, setDraft] = useState({ caret: 0, text: "" });
  const picker = useRefPicker({ caret: draft.caret, issueKey, text: draft.text });
  const write = (insertion: { end: number; start: number; text: string }) => {
    const next = applyInsertion(draft.text, insertion);
    setDraft(next);
  };
  return (
    <>
      <textarea
        aria-label="Comment"
        onChange={(event) =>
          setDraft({ caret: event.target.selectionStart ?? 0, text: event.target.value })
        }
        onKeyDown={(event) => {
          const result = picker.pressKey(event.key);
          if (result.action === "ignored") return;
          event.preventDefault();
          if (result.action === "took") write(result.insertion);
        }}
        value={draft.text}
      />
      <RefPickerList onTake={write} picker={picker} />
    </>
  );
}

/** The search answers the fixture's hits for a query its words hold, as the route would. */
const answerSearch = async (query: string): Promise<SearchResponse> => ({
  results: hits.results.filter((hit) => hit.snippet.toLowerCase().includes(query.toLowerCase())),
  took_ms: 2,
});

function renderHost({
  agents = [planner, worker],
  noIssue = false,
  searchAnswer = answerSearch,
}: {
  agents?: Agent[];
  /** The box belongs to no issue: it offers everyone who has signed in. */
  noIssue?: boolean;
  searchAnswer?: typeof api.search;
} = {}): Pane {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue(agents);
  const getIssueCollaborators = spyOn(api, "getIssueCollaborators").mockResolvedValue(
    collaborators
  );
  const listUsers = spyOn(api, "listUsers").mockResolvedValue([{ login: "bob@theorem.dev" }]);
  const getArchitecture = spyOn(api, "getArchitecture").mockResolvedValue(architecture);
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(blocks);
  const search = spyOn(api, "search").mockImplementation(searchAnswer);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <Host issueKey={noIssue ? undefined : "CORE-1"} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  pane = {
    getArchitecture,
    getIssueCollaborators,
    getReferences,
    listAgents,
    listUsers,
    restore: () => {
      search.mockRestore();
      getReferences.mockRestore();
      getArchitecture.mockRestore();
      listUsers.mockRestore();
      getIssueCollaborators.mockRestore();
      listAgents.mockRestore();
    },
    search,
    unmount: view.unmount,
  };
  return pane;
}

/** The picker's rows in the order they are drawn, each as `<heading>: <words>`. */
function drawnRows(): string[] {
  const list = screen.getByRole("listbox", { name: "References" });
  return within(list)
    .getAllByRole("group")
    .flatMap((group) => {
      const heading = group.firstElementChild?.textContent ?? "";
      return within(group)
        .getAllByRole("option")
        .map((option) => `${heading}: ${option.querySelector("span span")?.textContent ?? ""}`);
    });
}

async function field(): Promise<HTMLTextAreaElement> {
  return (await screen.findByLabelText("Comment")) as HTMLTextAreaElement;
}

test("every door lists the parties and the components before a character is typed, with one instruction row", async () => {
  const user = userEvent.setup();
  const page = renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");

  await screen.findByRole("listbox", { name: "References" });
  // The `@` door: people, agents, roles, then the kinds that need no query either.
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "People: alice@theorem.dev",
      "Agents: Planner",
      "Agents: Worker",
      "Roles: planner",
      "Roles: reviewer",
      "Components: The relay",
    ])
  );
  // The searched kinds cannot be listed yet, and the one instruction row says why.
  expect(screen.getByTestId("ref-picker-instruction").textContent).toBe(REF_PICKER_INSTRUCTION);
  expect(page.search).not.toHaveBeenCalled();
});

test("the doors differ only in their order, and `#` leads with the issues the search answers", async () => {
  const user = userEvent.setup();
  const page = renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("#relay");

  await waitFor(() => expect(page.search).toHaveBeenCalledTimes(1));
  expect(page.search.mock.calls[0]).toEqual([
    "relay",
    { kind: ["issue", "document", "ask", "comment", "message"], limit: 8 },
  ]);
  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Comments: I saw the relay drop one",
      "Components: The relay",
    ])
  );
  expect(screen.queryByTestId("ref-picker-instruction")).not.toBeInTheDocument();
});

test("the `[[` door ranks the same rows with the documents first and the parties last", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard(`${BRACKETS}relay`);

  await waitFor(() =>
    expect(drawnRows()).toEqual([
      "Issues: Relay drops a reply",
      "Comments: I saw the relay drop one",
      "Components: The relay",
    ])
  );
  // `[[` over a query that matches a party puts that party after the searched kinds, where
  // `@` puts it first.
  await user.clear(comment);
  await user.keyboard(`${BRACKETS}plan`);
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));
});

test("the picker filters as the reader types, moves with the arrows and takes the row on Enter", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");
  await screen.findByRole("listbox", { name: "References" });

  await waitFor(() => expect(drawnRows()).toHaveLength(6));

  await user.keyboard("worker");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Worker"]));

  await user.keyboard("{Backspace}".repeat("worker".length));
  await waitFor(() => expect(drawnRows()).toHaveLength(6));
  // The first row is highlighted until the reader moves; one step down takes the second.
  const options = screen.getAllByRole("option");
  expect(options[0]?.getAttribute("aria-selected")).toBe("true");
  await user.keyboard("{ArrowDown}");
  await waitFor(() =>
    expect(screen.getAllByRole("option")[1]?.getAttribute("aria-selected")).toBe("true")
  );
  await user.keyboard("{ArrowUp}{ArrowUp}");
  await waitFor(() =>
    expect(screen.getAllByRole("option")[5]?.getAttribute("aria-selected")).toBe("true")
  );

  await user.keyboard("{Enter}");
  await waitFor(() => expect(comment.value).toBe("dispatch://CORE/component/relay"));
});

test("Tab takes the highlighted row, and a party is written as a link naming the party", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@plann");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));

  await user.keyboard("{Tab}");
  await waitFor(() => expect(comment.value).toBe("[Planner](dispatch://agent/planner-session)"));
  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
});

test("a pick keeps the text after the caret", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("see  and the rest");
  comment.setSelectionRange("see ".length, "see ".length);
  await user.keyboard("@plann");
  await waitFor(() => expect(drawnRows()).toEqual(["Agents: Planner", "Roles: planner"]));

  await user.keyboard("{Enter}");
  await waitFor(() =>
    expect(comment.value).toBe("see [Planner](dispatch://agent/planner-session) and the rest")
  );
});

test("Escape closes the picker and leaves the character the reader typed", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@pl");
  await screen.findByRole("listbox", { name: "References" });

  await user.keyboard("{Escape}");

  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
  expect(comment.value).toBe("@pl");
  // It stays shut while the reader writes on after the character it left.
  await user.keyboard("anner");
  expect(comment.value).toBe("@planner");
  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
});

test("a space typed before a row is taken closes the picker, leaving a heading a heading", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("#");
  await screen.findByRole("listbox", { name: "References" });

  await user.keyboard(" Heading");

  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
  expect(comment.value).toBe("# Heading");
});

test("a trigger that is not at a word boundary opens nothing", async () => {
  const user = userEvent.setup();
  renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("mail alice@theorem.dev");

  expect(screen.queryByRole("listbox", { name: "References" })).not.toBeInTheDocument();
});

test("a node's row lists its blocks from its part_of backlinks, and a block pick is the bare address", async () => {
  const user = userEvent.setup();
  const page = renderHost();

  const comment = await field();
  await user.click(comment);
  await user.keyboard("#relay");
  await waitFor(() => expect(drawnRows()).toContain("Comments: I saw the relay drop one"));

  await user.click(screen.getByRole("button", { name: "Blocks" }));

  await waitFor(() =>
    expect(page.getReferences).toHaveBeenCalledWith("dispatch://CORE-2/comment/comment-1", {
      kind: "part_of",
    })
  );
  await waitFor(() =>
    expect(drawnRows()).toContain("Comment blocks: The first words of the block")
  );

  await user.click(screen.getByRole("option", { name: /The first words/ }));
  await waitFor(() => expect(comment.value).toBe("dispatch://CORE-2/comment/comment-1#b-block-1"));
});

test("a box with no issue offers everyone who has signed in", async () => {
  const user = userEvent.setup();
  const page = renderHost({ noIssue: true });

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@");

  await waitFor(() => expect(drawnRows()).toContain("People: bob@theorem.dev"));
  expect(page.getIssueCollaborators).not.toHaveBeenCalled();
  expect(page.listUsers).toHaveBeenCalledTimes(1);
});

test("the list says it is loading, that nothing matches, and that Dispatch is unreachable", async () => {
  const user = userEvent.setup();
  const slow = Promise.withResolvers<SearchResponse>();
  const page = renderHost({ agents: [], searchAnswer: () => slow.promise });
  page.getIssueCollaborators.mockResolvedValue({ ...collaborators, agents: [], people: [] });
  page.getArchitecture.mockRejectedValue(new ApiError(404, { code: "SOURCE_NOT_FOUND" }));

  const comment = await field();
  await user.click(comment);
  await user.keyboard("@relay");

  // Nothing of its own to list, and the search is still out: the list says it is loading.
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-loading").textContent).toBe(REF_PICKER_WORDS.loading)
  );

  slow.resolve({ results: [], took_ms: 1 });
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-empty").textContent).toBe(REF_PICKER_WORDS.empty)
  );

  // A read that failed is not an empty list: it says the read failed.
  page.search.mockRejectedValue(new Error("network error"));
  await user.keyboard("x");
  await waitFor(() =>
    expect(screen.getByTestId("ref-picker-unreachable").textContent).toBe(
      REF_PICKER_WORDS.unreachable
    )
  );
});
