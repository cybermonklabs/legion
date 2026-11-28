import { describe, expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  // biome-ignore lint/style/noRestrictedImports: the pointer drop drives dnd-kit with mouse presses and moves, which user-event cannot send
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { realClock, tick } from "../../../__tests__/fake-clock";
import { answerRequests } from "../../../__tests__/network";
import { readMarkHandlers } from "../../../__tests__/proof-document";
import { answerIssuePatches } from "../../../__tests__/send-answers";
import { api, type WriteBounds } from "../../../api/client";
import type {
  Issue,
  IssueChild,
  IssueCoverage,
  IssueDetails,
  IssueSummary,
  StatusChangeAnswer,
  StatusChangeInput,
  UpdateIssueInput,
  UserIssueState,
} from "../../../api/types";
import { HeaderSlot } from "../../issue-page/slots/header";
import { treeAnswer } from "../../issue-page/tree-read/tree-fixtures";
import { issueStatuses } from "../../project/board-model";
import { IssueBoard } from "../../project/IssueBoard";
import { IssuesTable } from "../../project/IssuesTable";
import { parseIssuePath } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { appKeymap, useKeymapScope } from "../../shell/keymap";
import { IssueHeader } from "../IssueHeader";
import { issuePriority } from "../issue-priority";
import { useIssuePriority } from "../useIssuePriority";
import { answerStatusChanges, type StatusChangeAnswerer } from "./status-change-fixtures";

// The inventory of every control of the page that writes an issue's status (INFRA-4731's plan,
// "Lists named before code"): each one, moving an issue with a parent or an open child, sends the
// rule's dry run before any write. A writer added to the page joins this list. The dry run is held,
// so what each case reads is the one request the move sent before it could write anything.

const openChild: IssueChild = {
  active_at: "2026-09-09T00:00:00Z",
  external_links: [],
  key: "CORE-2",
  status: "todo",
  subtree_done: 0,
  subtree_total: 1,
  title: "Child in work",
};

const issue: IssueDetails = {
  route_status: null,
  route_holder: null,
  route_status_reason: null,
  holder_state: null,
  last_seen: null,
  takeable_at: null,
  advances: [],
  advances_revision: 0,
  artifacts: [],
  children: [openChild],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  design_gates: [],
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 250,
  number: 1,
  open_asks: [],
  progress: { tasks: null, children: { done: 0, total: 1 } },
  parent: null,
  blocked_by: [],
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  plan_artifact_id: "",
  decisions_artifact_id: "",
  primary_artifact_id: "",
  verification_artifact_id: "",
  project: "CORE",
  referenced_by_count: 0,
  route: null,
  repo: null,
  resolved_repo: null,
  status: "in_progress",
  priority: null,
  rank: "U",
  title: "Parent with a child in work",
  updated_at: "2026-09-09T00:00:00Z",
};

/** The same issue closed, under a parent: a reopen under a parent is a move the rule judges. */
const closedChild: IssueDetails = {
  ...issue,
  children: [],
  closed_at: "2026-09-10T00:00:00Z",
  parent: "CORE-9",
  progress: { tasks: null, children: null },
  status: "done",
};

const state: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };

function signedIn(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return client;
}

/** The writes a case may send: the route's requests, and the issue's PATCH. */
interface Writes {
  readonly patches: Mock<
    (key: string, input: UpdateIssueInput, bounds: WriteBounds) => Promise<Issue>
  >;
  readonly statusChanges: Mock<StatusChangeAnswerer>;
}

/** Every request of the route, the dry run held, and every PATCH, which no case may reach. */
function answerWrites(): Writes {
  const statusChanges = answerStatusChanges().mockImplementation(
    () => new Promise<StatusChangeAnswer>(() => {})
  );
  const patches = answerIssuePatches();
  return { patches, statusChanges };
}

/** The move's dry run went out, with `options` beside its status, and nothing was written before
 *  it. */
async function expectDryRunFirst(
  writes: Writes,
  key: string,
  status: string,
  options: Omit<StatusChangeInput, "status"> = {}
): Promise<void> {
  await waitFor(() => expect(writes.statusChanges).toHaveBeenCalledTimes(1));
  expect(writes.statusChanges.mock.calls[0]?.slice(0, 2)).toEqual([
    key,
    { dry_run: true, status, ...options },
  ]);
  expect(writes.patches).not.toHaveBeenCalled();
}

function Header({ shown }: { shown: IssueDetails }): ReactNode {
  // The issue page pushes the scope its palette rows are offered under.
  useKeymapScope("issue");
  const priorityWrite = useIssuePriority(shown.key, issuePriority(shown.priority));
  return (
    <IssueHeader
      documentArtifact={undefined}
      isClosed={shown.closed_at !== null}
      issue={shown}
      priorityWrite={priorityWrite}
      state={state}
    />
  );
}

/** The issue page's header over `shown`, with the reads it makes answered empty. */
function renderHeader(shown: IssueDetails) {
  const reads = [
    spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
  ];
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={signedIn()}>
        <KeymapProvider>
          <Header shown={shown} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return () => {
    view.unmount();
    for (const read of reads) read.mockRestore();
  };
}

/** Pushes the `issue` scope, which the new issue page's view row pushes and its header's slot does
 *  not: the header's palette rows register into it. */
function IssueScope(): null {
  useKeymapScope("issue");
  return null;
}

/** The new issue page's header over `shown`, as that page's root mounts it (`HeaderSlot`), with
 *  the reads it makes answered: the issue and the reader's state of it, and the comments,
 *  decisions, coverage and projects that its count of what waits and its approval read. */
function renderOutlineHeader(shown: IssueDetails) {
  const reads = [
    spyOn(api, "getIssue").mockResolvedValue(shown),
    spyOn(api, "getMyState").mockResolvedValue({ [shown.key]: state }),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    // The tree read, which the header reads since the page draws its outline from it: every read
    // of it, the one with no view and Conversation's, which sends `kind`, answers an issue with no
    // node.
    spyOn(api, "getIssueTree").mockResolvedValue(treeAnswer([])),
  ];
  answerRequests(
    http.get("/api/v1/issues/:key/coverage", ({ params }) =>
      Response.json({
        gaps: [],
        has_plan: false,
        issue: String(params.key),
        requirements: [],
        spec_artifact_id: "",
      } satisfies IssueCoverage)
    ),
    ...readMarkHandlers()
  );
  const path = `/issues/${shown.key}`;
  const route = parseIssuePath(path, "");
  if (route === undefined) throw new Error(`${path} is no issue's address`);
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={signedIn()}>
        <KeymapProvider>
          <IssueScope />
          <HeaderSlot issue={shown} route={route} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return () => {
    view.unmount();
    for (const read of reads) read.mockRestore();
  };
}

/** The palette's row labelled `label`, as `$mod+k` offers it now. */
function paletteRow(label: string) {
  const row = appKeymap.actions().find((action) => action.label === label);
  if (row === undefined) throw new Error(`the palette offers no row "${label}"`);
  return row;
}

function card(overrides: Partial<IssueSummary>): IssueSummary {
  const {
    artifacts: _artifacts,
    children: _children,
    design_gates: _gates,
    holder_state: _holder,
    last_seen: _seen,
    open_asks: _asks,
    referenced_by_count: _references,
    takeable_at: _takeable,
    ...fields
  } = issue;
  return { ...fields, open_asks: 0, ...overrides };
}

/** The project board over `list`, with the reads it makes answered. */
function renderBoard(list: IssueSummary[], options: { lanes?: boolean; showEdges?: boolean } = {}) {
  const reads = [
    spyOn(api, "listIssues").mockResolvedValue(list),
    spyOn(api, "getMyState").mockResolvedValue({}),
  ];
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={signedIn()}>
        <KeymapProvider>
          <IssueBoard lanes={options.lanes} project="CORE" showEdges={options.showEdges} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return () => {
    view.unmount();
    for (const read of reads) read.mockRestore();
  };
}

/**
 * Lays the board out as a browser would for dnd-kit, which reads where each droppable is: each
 * column 288 px wide, one after another in the order of the statuses, and each card 90 px tall
 * down its column. Nothing else on the page has a size.
 */
function layOutBoard() {
  return spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const column = this.closest<HTMLElement>("[data-board-column]");
    const status = column?.dataset.boardColumn;
    if (column === null || status === undefined) return new DOMRect(0, 0, 0, 0);
    const left = issueStatuses.indexOf(status as (typeof issueStatuses)[number]) * 300;
    if (this === column) return new DOMRect(left, 0, 288, 2000);
    const cards = [...column.querySelectorAll("[data-board-card]")];
    const index = cards.indexOf(this);
    return index === -1 ? new DOMRect(0, 0, 0, 0) : new DOMRect(left, 60 + index * 100, 288, 90);
  });
}

/** Drags `article` with the mouse and drops it in the empty part of the `status` column. */
async function dropInColumn(article: HTMLElement, status: string): Promise<void> {
  const box = article.getBoundingClientRect();
  const from = { clientX: box.x + 10, clientY: box.y + 10 };
  const to = {
    clientX: issueStatuses.indexOf(status as (typeof issueStatuses)[number]) * 300 + 100,
    clientY: 1500,
  };
  await act(async () => {
    // dnd-kit's pointer sensor is driven by this mouse press; user-event's pointer API sends a
    // fixed sequence that never lifts the card.
    fireEvent.mouseDown(article, { button: 0, ...from });
  });
  await act(async () => {
    // The move that lifts the card, past dnd-kit's 8 px: user-event sends no mousemove of its own.
    fireEvent.mouseMove(document, { clientX: from.clientX, clientY: from.clientY + 40 });
  });
  await act(async () => {
    // The move over the `status` column, which dnd-kit reads as the drop target: user-event sends
    // no mousemove of its own.
    fireEvent.mouseMove(document, to);
  });
  await act(async () => {
    // The release that drops it, the other half of a gesture user-event cannot drive.
    fireEvent.mouseUp(document, to);
  });
}

/** The project's issues table over `list`, at the address's `search`, with the reads it makes
 *  answered and any create refused: no case may reach one. */
function renderTable(list: IssueSummary[], search = "") {
  const reads = [
    spyOn(api, "listIssues").mockResolvedValue(list),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
  ];
  const create = spyOn(api, "createIssue").mockRejectedValue(new Error("Unexpected create"));
  // The unit DOM has no layout: the table's scroll box gets the height a browser would give it,
  // so its rows are drawn.
  const height = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
  Object.defineProperty(HTMLElement.prototype, "clientHeight", {
    configurable: true,
    get() {
      return this.dataset.testid === "issues-table-scroll" ? 600 : (height?.get?.call(this) ?? 0);
    },
  });
  const view = render(
    <MemoryRouter initialEntries={[`/projects/CORE/issues${search}`]}>
      <QueryClientProvider client={signedIn()}>
        <KeymapProvider>
          <IssuesTable listing={{ project: "CORE" }} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    create,
    cleanup: () => {
      view.unmount();
      for (const read of [...reads, create]) read.mockRestore();
      if (height) Object.defineProperty(HTMLElement.prototype, "clientHeight", height);
      else Reflect.deleteProperty(HTMLElement.prototype, "clientHeight");
    },
  };
}

describe("every status writer of the page asks", () => {
  test("the header's status select", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const cleanup = renderHeader(issue);
    try {
      // An open issue's select offers no Done, so the move is to Backlog, which a parent with a
      // child in work cannot take either.
      await user.selectOptions(await screen.findByRole("combobox", { name: "Status" }), "backlog");
      await expectDryRunFirst(writes, "CORE-1", "backlog");
      expect(screen.getByRole("combobox", { name: "Status" })).toHaveValue("backlog");
    } finally {
      cleanup();
    }
  });

  test("the header's Close", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const cleanup = renderHeader(issue);
    try {
      await user.click(await screen.findByRole("button", { name: "Close issue" }));
      await expectDryRunFirst(writes, "CORE-1", "done");
    } finally {
      cleanup();
    }
  });

  test("the header's Reopen", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const cleanup = renderHeader(closedChild);
    try {
      await user.click(await screen.findByRole("button", { name: "Reopen issue" }));
      await expectDryRunFirst(writes, "CORE-1", "backlog");
    } finally {
      cleanup();
    }
  });

  test("the palette's Close issue", async () => {
    const writes = answerWrites();
    const cleanup = renderHeader(issue);
    try {
      await screen.findByRole("button", { name: "Close issue" });
      await act(async () => paletteRow("Close issue").run());
      await expectDryRunFirst(writes, "CORE-1", "done");
    } finally {
      cleanup();
    }
  });

  test("the palette's Reopen issue", async () => {
    const writes = answerWrites();
    const cleanup = renderHeader(closedChild);
    try {
      await screen.findByRole("button", { name: "Reopen issue" });
      await act(async () => paletteRow("Reopen issue").run());
      await expectDryRunFirst(writes, "CORE-1", "backlog");
    } finally {
      cleanup();
    }
  });

  test("the new issue page's status select", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const cleanup = renderOutlineHeader(issue);
    try {
      await user.selectOptions(await screen.findByRole("combobox", { name: "Status" }), "backlog");
      await expectDryRunFirst(writes, "CORE-1", "backlog");
      expect(screen.getByRole("combobox", { name: "Status" })).toHaveValue("backlog");
    } finally {
      cleanup();
    }
  });

  test("the new issue page's Close", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const cleanup = renderOutlineHeader(issue);
    try {
      await user.click(await screen.findByRole("button", { name: "Issue actions" }));
      await user.click(screen.getByRole("button", { name: "Close issue" }));
      await expectDryRunFirst(writes, "CORE-1", "done");
    } finally {
      cleanup();
    }
  });

  test("the new issue page's Reopen", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const cleanup = renderOutlineHeader(closedChild);
    try {
      await user.click(await screen.findByRole("button", { name: "Issue actions" }));
      await user.click(screen.getByRole("button", { name: "Reopen into Backlog" }));
      await expectDryRunFirst(writes, "CORE-1", "backlog");
    } finally {
      cleanup();
    }
  });

  test("the new issue page's palette Close issue", async () => {
    const writes = answerWrites();
    const cleanup = renderOutlineHeader(issue);
    try {
      await screen.findByRole("button", { name: "Issue actions" });
      await act(async () => paletteRow("Close issue").run());
      await expectDryRunFirst(writes, "CORE-1", "done");
    } finally {
      cleanup();
    }
  });

  test("the new issue page's palette Reopen issue", async () => {
    const writes = answerWrites();
    const cleanup = renderOutlineHeader(closedChild);
    try {
      await screen.findByRole("button", { name: "Issue actions" });
      await act(async () => paletteRow("Reopen issue").run());
      await expectDryRunFirst(writes, "CORE-1", "backlog");
    } finally {
      cleanup();
    }
  });

  // dnd-kit keeps every click from the page for 50 ms after a drop, with a listener on `document`,
  // which the next row shares: on the real clock a row that ends inside those 50 ms hands the
  // listener to the next row, whose first click never reaches its target (the header's Reopen
  // timed out so in a whole lane). So the drop runs on the fake clock, and the row moves it past
  // the 50 ms before it ends, as the drags of `IssueBoard.test.tsx` do.
  test("the board's pointer drop", async () => {
    jest.useFakeTimers();
    const writes = answerWrites();
    const layout = layOutBoard();
    const cleanup = renderBoard([card({ status: "retro" })], { showEdges: true });
    try {
      const board = await screen.findByRole("region", { name: "Project board" });
      await dropInColumn(within(board).getByRole("article", { name: /^CORE-1 / }), "done");
      await expectDryRunFirst(writes, "CORE-1", "done");
      // The card stays where it is until the person confirms.
      expect(
        within(within(board).getByRole("region", { name: "Retro" })).getByRole("article")
      ).toBeInTheDocument();
    } finally {
      await tick(80);
      realClock();
      cleanup();
      layout.mockRestore();
    }
  });

  test("the board's Shift+L", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const cleanup = renderBoard([card({ status: "retro" })]);
    try {
      const board = await screen.findByRole("region", { name: "Project board" });
      const article = within(board).getByRole("article", { name: /^CORE-1 / });
      act(() => article.focus());
      await user.keyboard("{Shift>}L{/Shift}");
      await expectDryRunFirst(writes, "CORE-1", "done");
    } finally {
      cleanup();
    }
  });

  test("the board's Shift+H", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const cleanup = renderBoard([card({ status: "todo" })]);
    try {
      const board = await screen.findByRole("region", { name: "Project board" });
      const article = within(board).getByRole("article", { name: /^CORE-1 / });
      act(() => article.focus());
      // Back to Backlog, which a parent with a child in work cannot take.
      await user.keyboard("{Shift>}H{/Shift}");
      await expectDryRunFirst(writes, "CORE-1", "backlog");
    } finally {
      cleanup();
    }
  });

  test("the board's lane move", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const cleanup = renderBoard([card({ priority: 0, status: "retro" })], { lanes: true });
    try {
      const lane = await screen.findByRole("region", { name: "P0 lane" });
      const article = within(lane).getByRole("article", { name: /^CORE-1 / });
      act(() => article.focus());
      await user.keyboard("{Shift>}L{/Shift}");
      await expectDryRunFirst(writes, "CORE-1", "done");
    } finally {
      cleanup();
    }
  });

  // The issues table's parent writers ask before any write: each moves an issue in work under an
  // epic that is not in work, which Dispatch's rule needs in work first (D12).
  test("the issues table's New epic, over an issue in work, before its create", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const { cleanup, create } = renderTable([card({ key: "CORE-3", parent: null })]);
    try {
      await user.click(await screen.findByRole("checkbox", { name: "Select CORE-3" }));
      await user.click(screen.getByRole("button", { name: "New epic from these" }));
      const naming = await screen.findByRole("dialog", { name: "Name the new epic" });
      await user.type(within(naming).getByRole("textbox", { name: "Name" }), "Platform");
      await user.click(within(naming).getByRole("button", { name: "Create epic" }));
      // A new epic is no issue yet, so it asks with no dry run.
      await screen.findByRole("dialog", { name: "Move “Platform” into work too?" });
      expect(create).not.toHaveBeenCalled();
      expect(writes.statusChanges).not.toHaveBeenCalled();
      expect(writes.patches).not.toHaveBeenCalled();
    } finally {
      cleanup();
    }
  });

  test("the issues table's Move to epic, of an issue in work to an epic on the backlog", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const { cleanup } = renderTable([
      card({ key: "CORE-3", parent: null }),
      card({ key: "CORE-9", labels: ["epic"], status: "backlog", title: "Platform" }),
    ]);
    try {
      await user.click(await screen.findByRole("checkbox", { name: "Select CORE-3" }));
      await user.selectOptions(screen.getByRole("combobox", { name: "Move to epic" }), "CORE-9");
      await user.click(screen.getByRole("button", { name: "Move" }));
      await expectDryRunFirst(writes, "CORE-9", "in_progress", { include_parent: true });
    } finally {
      cleanup();
    }
  });

  test("the issues table's Merge, of an epic holding an issue in work into an epic on the backlog", async () => {
    const writes = answerWrites();
    const user = userEvent.setup();
    const { cleanup } = renderTable(
      [
        card({ key: "CORE-1", labels: ["epic"], title: "Source" }),
        card({ key: "CORE-2", labels: ["epic"], status: "backlog", title: "Target" }),
        card({ key: "CORE-3", parent: "CORE-1" }),
      ],
      "?group=epic"
    );
    try {
      const heading = (await screen.findByRole("link", { name: "CORE-1 · Source" })).closest("tr");
      if (heading === null) throw new Error("The source epic has no heading row");
      await user.click(within(heading).getByRole("button", { name: "Epic actions ▾" }));
      await user.selectOptions(
        screen.getByRole("combobox", { name: "Merge into another epic" }),
        "CORE-2"
      );
      await user.click(screen.getByRole("button", { name: "Merge…" }));
      await expectDryRunFirst(writes, "CORE-2", "in_progress", { include_parent: true });
    } finally {
      cleanup();
    }
  });
});
