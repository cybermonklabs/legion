import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { countEngineParses, displayTree } from "../../__tests__/display-trees";
import { realClock } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssuePatches } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import type {
  ArchitectureSource,
  ArchitectureTree,
  DisplayTree,
  IssueComponents,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { stoppedWriteNote } from "../writes/keyed-write";
import {
  ADD_SOURCE,
  ADD_SOURCE_PATH,
  IssueComponentsLine,
  NO_SOURCE,
  SOURCES_LOADING,
} from "./IssueComponentsLine";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const tree: ArchitectureTree = {
  components: [
    {
      depends_on: [],
      done: 0,
      external: false,
      id: "dispatch-server",
      issues: [],
      own_done: 0,
      own_total: 0,
      parent: null,
      paths: [],
      prose: "",
      source: "legion/legion",
      title: "Dispatch server",
      total: 0,
    },
    {
      depends_on: [],
      done: 0,
      external: true,
      id: "github",
      issues: [],
      own_done: 0,
      own_total: 0,
      parent: null,
      paths: [],
      prose: "",
      source: "legion/legion",
      title: "GitHub",
      total: 0,
    },
    {
      depends_on: [],
      done: 0,
      external: false,
      id: "infra-cluster",
      issues: [],
      own_done: 0,
      own_total: 0,
      parent: null,
      paths: [],
      prose: "",
      source: "acme/infra",
      title: "Cluster",
      total: 0,
    },
  ],
  not_architectural: [],
  retired_links: [],
  sources: [
    {
      branch: "main",
      last_commit: "abc",
      last_error: null,
      last_sync_at: "2026-09-17T00:00:00Z",
      repo: "legion/legion",
    },
    {
      branch: "main",
      last_commit: "def",
      last_error: null,
      last_sync_at: "2026-09-17T00:00:00Z",
      repo: "acme/infra",
    },
  ],
  totals: {
    components_without_work: 1,
    issues_done: 0,
    issues_total: 0,
    not_architectural: 0,
    retired_links: 0,
    unassigned: 0,
  },
  unassigned: [],
};

const source: ArchitectureSource = {
  branch: "main",
  components: 2,
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  enabled: true,
  last_commit: "3be61df5be90",
  last_error: null,
  last_sync_at: "2026-09-17T00:00:00Z",
  project: "CORE",
  repo: "legion/legion",
};

// Queries are scoped to this render's container: bun runs every test file in
// one document, and a sibling file's un-unmounted render would otherwise leak
// its links into a `screen`-wide role query.
/** `architecture` is the project's tree, or `undefined` for a project without a source (its
 *  source list is empty). `reasonTree` is the tree the issue read sends of a `none` reason. */
function renderLine(
  components: IssueComponents,
  architecture: ArchitectureTree | undefined,
  reasonTree?: DisplayTree
) {
  const listProjectArchitectureSources = spyOn(api, "listProjectArchitectureSources");
  const getArchitecture = spyOn(api, "getArchitecture");
  if (architecture === undefined) {
    listProjectArchitectureSources.mockResolvedValue([]);
    getArchitecture.mockRejectedValue(new ApiError(404, { code: "SOURCE_NOT_FOUND" }));
  } else {
    listProjectArchitectureSources.mockResolvedValue([
      source,
      { ...source, components: 1, repo: "acme/infra" },
    ]);
    getArchitecture.mockResolvedValue(architecture);
  }
  const patchIssue = answerIssuePatches();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <IssueComponentsLine
          issue={{
            components,
            key: "CORE-7",
            project: "CORE",
            status: "todo",
            title: "Ship",
            ...(reasonTree === undefined ? {} : { components_reason_tree: reasonTree }),
          }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    ...view,
    getArchitecture,
    listProjectArchitectureSources,
    patchIssue,
    q: within(view.container),
    queryClient,
    unmountAll: () => {
      view.unmount();
      getArchitecture.mockRestore();
      listProjectArchitectureSources.mockRestore();
      patchIssue.mockRestore();
    },
  };
}

// An answer of no source makes the line read-only, and says why and where a source is added
// (dispatch://INFRA-5200): the reader sees the reason the picker is not there.
test("IssueComponentsLine reads Not attached, and without an architecture source says why it is read-only and links to Settings", async () => {
  const view = renderLine(
    { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    undefined
  );
  try {
    await waitFor(() => expect(view.listProjectArchitectureSources).toHaveBeenCalledTimes(1));
    // It is the value of the Components row of the header's Details, which names it.
    await waitFor(() =>
      expect(view.q.getByTestId("issue-components").textContent).toBe(
        `Not attached${NO_SOURCE} ${ADD_SOURCE}`
      )
    );
    expect(view.q.getByRole("link", { name: ADD_SOURCE })).toHaveAttribute("href", ADD_SOURCE_PATH);
    expect(
      view.q.queryByRole("button", { name: "Edit components of CORE-7" })
    ).not.toBeInTheDocument();
    // No source: the tree is never asked for.
    expect(view.getArchitecture).not.toHaveBeenCalled();
  } finally {
    view.unmountAll();
  }
});

/** The line for an issue with no components of its own, over a sources read `read` answers. */
function renderOverSources(read: () => Promise<ArchitectureSource[]>) {
  const listProjectArchitectureSources = spyOn(api, "listProjectArchitectureSources");
  listProjectArchitectureSources.mockImplementationOnce(read);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <IssueComponentsLine
          issue={{
            components: {
              mode: "inherit",
              ids: [],
              unknown: [],
              reason: null,
              inherited_from: null,
            },
            key: "CORE-7",
            project: "CORE",
            status: "todo",
            title: "Ship",
          }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    listProjectArchitectureSources,
    q: within(view.container),
    unmountAll: () => {
      view.unmount();
      listProjectArchitectureSources.mockRestore();
    },
  };
}

// While the sources read is out the line says so and offers no control: a picker drawn then
// would go again, with no word, when the answer is no source (dispatch://INFRA-5200).
test("while the sources read is out the line says it is loading and offers no control", async () => {
  const answer = Promise.withResolvers<ArchitectureSource[]>();
  const view = renderOverSources(() => answer.promise);
  try {
    expect(view.q.getByRole("status")).toHaveTextContent(SOURCES_LOADING);
    expect(view.q.queryByRole("button")).not.toBeInTheDocument();
    answer.resolve([source]);
    expect(await view.q.findByRole("button", { name: "Edit components of CORE-7" })).toBeDefined();
    expect(view.q.queryByText(SOURCES_LOADING)).not.toBeInTheDocument();
  } finally {
    view.unmountAll();
  }
});

// A failed read says the components are unavailable, with Retry, rather than taking the editor
// away without a word; the retried answer offers the picker (dispatch://INFRA-5200, c-review F6).
test("a failed source read says Components unavailable with Retry, and a retried answer offers the picker", async () => {
  const user = userEvent.setup();
  const answer = Promise.withResolvers<ArchitectureSource[]>();
  const view = renderOverSources(() => answer.promise);
  const { q } = view;
  try {
    answer.reject(
      new NetworkError("/api/v1/projects/CORE/architecture-sources", new Error("offline"))
    );
    expect(await q.findByText(/Components unavailable/)).toBeDefined();
    expect(q.queryByRole("button", { name: "Edit components of CORE-7" })).not.toBeInTheDocument();

    view.listProjectArchitectureSources.mockResolvedValue([source]);
    await user.click(q.getByRole("button", { name: "Retry" }));
    expect(await q.findByRole("button", { name: "Edit components of CORE-7" })).toBeDefined();
    expect(q.queryByText(/Components unavailable/)).not.toBeInTheDocument();
  } finally {
    view.unmountAll();
  }
});

test("IssueComponentsLine lists an explicit set as chips, marks retired ids, and links the ancestor it came from", () => {
  const view = renderLine(
    {
      mode: "explicit",
      ids: ["dispatch-server", "web"],
      unknown: ["legacy-ui"],
      reason: null,
      inherited_from: "CORE-1",
    },
    undefined
  );
  try {
    view.q.getByText("dispatch-server");
    view.q.getByText("web");
    const retired = view.q.getByText("retired: legacy-ui");
    expect(retired).toHaveAttribute(
      "title",
      "legacy-ui is no longer in the project's architecture model"
    );
    const ancestor = view.q.getByRole("link", { name: "CORE-1" }) as HTMLAnchorElement;
    expect(ancestor).toHaveAttribute("href", "/issues/CORE-1");
    expect(view.q.getByTestId("issue-components")).toHaveTextContent("inherited from CORE-1");
  } finally {
    view.unmountAll();
  }
});

test("IssueComponentsLine shows a none attachment with its reason, drawn as the markdown it is", async () => {
  const view = renderLine(
    { mode: "none", ids: [], unknown: [], reason: "hiring, **not** code", inherited_from: null },
    undefined
  );
  try {
    // The reason draws through the one renderer, which loads its engine on the first body of the
    // page, so the words arrive a commit after the line. Once the project's sources answered that
    // there are none, the line is its words and why it is read-only; the one link is the one to
    // where a source is added.
    await waitFor(() =>
      expect(view.q.getByTestId("issue-components").textContent).toBe(
        `None — hiring, not code${NO_SOURCE} ${ADD_SOURCE}`
      )
    );
    expect(within(view.q.getByTestId("issue-components")).getByText("not").tagName).toBe("STRONG");
    expect(view.q.getAllByRole("link").map((link) => link.textContent)).toEqual([ADD_SOURCE]);
  } finally {
    view.unmountAll();
  }
});

// The reason is a stored text its writer typed in a box with the reference picker, and the issue
// read sends the reading the server made of it (`components_reason_tree`): the line draws that
// tree and parses nothing on the page's thread. The tree reads differently from the markdown
// beside it, so a line drawn from a parse of the markdown would end `not code` and count a parse.
test("IssueComponentsLine draws a none attachment's reason from the tree the issue read sends", async () => {
  const reasonTree = displayTree("hiring, **not** code, for now");
  const parses = countEngineParses();
  const view = renderLine(
    { mode: "none", ids: [], unknown: [], reason: "hiring, **not** code", inherited_from: null },
    undefined,
    reasonTree
  );
  try {
    // The line is the value of the Components row of the header's Details, which names it, and
    // once the project's sources answered that there are none it says why it is read-only.
    await waitFor(() =>
      expect(view.q.getByTestId("issue-components").textContent).toBe(
        `None — hiring, not code, for now${NO_SOURCE} ${ADD_SOURCE}`
      )
    );
    expect(within(view.q.getByTestId("issue-components")).getByText("not").tagName).toBe("STRONG");
    expect(parses.count()).toBe(0);
  } finally {
    parses.restore();
    view.unmountAll();
  }
});

test("with architecture sources the line offers the picker with every source's components, fetching the tree only once it opens, and PATCHes an explicit set", async () => {
  const user = userEvent.setup();
  const view = renderLine(
    { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    tree
  );
  try {
    view.patchIssue.mockResolvedValue({} as never);
    const trigger = await view.q.findByRole("button", { name: "Edit components of CORE-7" });
    expect(trigger.textContent).toBe("Set components");
    // The header decides editability from the source list alone: the tree (refetched on
    // every issue event in the project) waits for the picker.
    expect(view.getArchitecture).not.toHaveBeenCalled();
    await user.click(trigger);
    await waitFor(() => expect(view.getArchitecture).toHaveBeenCalledTimes(1));
    const option = await screen.findByRole("option", { name: "dispatch-server · Dispatch server" });
    const listbox = screen.getByRole("listbox", { name: "Components options" });
    // Both sources' components are offered; the external one is not, since the server
    // refuses to attach it.
    expect(
      within(listbox)
        .getAllByRole("option")
        .map((row) => row.textContent)
    ).toEqual(["dispatch-server · Dispatch server", "infra-cluster · Cluster"]);
    await user.click(option);
    await user.click(screen.getByRole("option", { name: "infra-cluster · Cluster" }));
    // The picker's own Escape handler sits on the search input, so the press has to reach it:
    // focusing it directly leaves the open picker as the clicks left it.
    screen.getByRole("combobox", { name: "Search components" }).focus();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(view.patchIssue).toHaveBeenCalledTimes(1));
    expect(view.patchIssue).toHaveBeenCalledWith(
      "CORE-7",
      {
        components: { ids: ["dispatch-server", "infra-cluster"], mode: "explicit" },
      },
      keyedAttempt
    );
  } finally {
    view.unmountAll();
  }
});

test("an explicit set naming components of two sources shows each as a bare id chip", async () => {
  const view = renderLine(
    {
      mode: "explicit",
      ids: ["dispatch-server", "infra-cluster"],
      unknown: [],
      reason: null,
      inherited_from: null,
    },
    tree
  );
  try {
    // Each chip is its bare id; once the project's sources answered, the "+" after them is the
    // picker's trigger for the issue's own set.
    await waitFor(() =>
      expect(view.q.getByTestId("issue-components").textContent).toBe(
        "dispatch-serverinfra-cluster+"
      )
    );
  } finally {
    view.unmountAll();
  }
});

// A save the reader stopped may have been made: the line says so, and that a new save is a press
// of its own, rather than the stopped write's own words, which are a composer's, of a message.
test("components the reader stopped say they may have been saved, not a message's words", async () => {
  const view = renderLine(
    { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    tree
  );
  try {
    view.patchIssue.mockRejectedValue(
      new NetworkError("/api/v1/issues/CORE-7", new TypeError("Failed to fetch"))
    );
    await userEvent
      .setup()
      .click(await view.q.findByRole("button", { name: "Edit components of CORE-7" }));
    const option = await screen.findByRole("option", { name: "dispatch-server · Dispatch server" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(option);
    screen.getByRole("combobox", { name: "Search components" }).focus();
    await user.keyboard("{Escape}");
    await stopTheLateWrite(view.queryClient);
    expect(view.q.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the components of CORE-7", "saved", {
        anew: "They are read again from Dispatch, and saving them again sends a new write.",
      })
    );
  } finally {
    realClock();
    view.unmountAll();
    view.queryClient.clear();
  }
});
