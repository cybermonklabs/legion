import { expect, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Issue, IssueDetails } from "../../api/types";
import { inPlaceBoxRules } from "../../components/InPlaceEditBox";
import { MarginProvider } from "../margin/margin-context";
import { IssueHeader } from "./IssueHeader";
import { stateForIssue } from "./pins";
import type { IssuePriorityWrite } from "./useIssuePriority";

/**
 * Every in-place editor of the issue header, by the control that opens it and the field it
 * opens. A header editor added without `components/InPlaceEditBox` fails the first row here
 * (it draws no cell of its own and the cell count is wrong), and one that keeps a box rule of
 * its own fails the second.
 */
const EDITORS = [
  { field: "Issue title", name: "title", opener: "Review the spec" },
  { field: "Message route", name: "route", opener: "Messages default to no route" },
  { field: "Parent", name: "parent", opener: "None, set parent issue" },
] as const;

const base: Issue = {
  assignee: null,
  blocked_by: [],
  claim: null,
  closed_at: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  created_at: "2026-11-11T05:00:00Z",
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
  priority: null,
  project: "CORE",
  rank: "U",
  repo: null,
  resolved_repo: null,
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-11-11T06:00:00Z",
  verification_artifact_id: "",
};

/** These rows are about the header's boxes, not the priority: the header's write never runs. */
const idlePriorityWrite: IssuePriorityWrite = {
  failed: false,
  pending: false,
  retry: () => undefined,
  submit: () => undefined,
};

function renderHeader(): { container: HTMLElement; unmount: () => void; user: UserEvent } {
  const details: IssueDetails = {
    ...base,
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
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The reads the header makes for an issue that resolves to no repository and a project with
  // no architecture source; neither reaches the network.
  const mappings = spyOn(api, "listRepoProjects").mockResolvedValue([]);
  const architectureSources = spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]);
  const subscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginProvider>{children}</MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(
    <IssueHeader
      documentArtifact={undefined}
      isClosed={false}
      issue={details}
      priorityWrite={idlePriorityWrite}
      state={stateForIssue(undefined, base.key)}
    />,
    { wrapper }
  );
  return {
    container: view.container,
    unmount: () => {
      view.unmount();
      mappings.mockRestore();
      architectureSources.mockRestore();
      subscribers.mockRestore();
    },
    user: userEvent.setup(),
  };
}

test("every in-place editor of the issue header draws through the one shared box", async () => {
  const { container, unmount, user } = renderHeader();
  try {
    const header = container.querySelector("[data-issue-header]");
    const cells = [...(header?.querySelectorAll("[data-in-place-edit]") ?? [])];
    // Three editors, three cells: a fourth in-place editor of this header that drew its own box
    // would leave this list naming three and the header holding four fields.
    expect(cells.map((cell) => cell.getAttribute("data-in-place-edit"))).toEqual(
      EDITORS.map((editor) => editor.name)
    );

    for (const { field, name, opener } of EDITORS) {
      const cell = header?.querySelector(`[data-in-place-edit="${name}"]`);
      const control = await screen.findByRole(
        name === "title" ? "heading" : "button",
        name === "title" ? { level: 1, name: opener } : { name: opener }
      );
      expect(cell?.contains(control)).toBe(true);

      await user.click(control);
      const editor = await screen.findByLabelText(field);
      // The field the reader types in belongs to this cell's editor, drawn by the component -
      // covering the read view for the title, portalled over it for the route and the parent.
      expect(editor.closest(`[data-in-place-edit-editor="${name}"]`)).not.toBeNull();
      await user.keyboard("{Escape}");
    }
  } finally {
    unmount();
  }
});

test("no in-place editor of the issue header keeps a box rule of its own", () => {
  const source = readFileSync(join(import.meta.dir, "IssueHeader.tsx"), "utf8");
  const rules = `${inPlaceBoxRules.rail} ${inPlaceBoxRules.title}`;
  // What tells an in-place editor's box from every other control of this header: the title's
  // type metrics, the rail pill's shape and width, and the cell the two views share. Each is
  // the component's, so none of them is written here - a header editor that drew its own box
  // would have to write one of them.
  for (const token of [
    "text-[22px]",
    "leading-7",
    "tracking-tight",
    "max-w-[14ch]",
    "rounded-full px-3 py-2",
  ]) {
    expect(rules).toContain(token);
    expect(source).not.toContain(token);
  }
  // Nor the floor the cell keeps, the cover that takes the editor out of the flow, or the cell
  // itself.
  for (const token of ["min-w-[20ch]", "absolute inset-x-0 top-0", "col-start-1", "row-start-1"]) {
    expect(source).not.toContain(token);
  }
});

test("the metadata rail keeps its route and parent items while their editors are open", async () => {
  const { container, unmount, user } = renderHeader();
  try {
    const rail = container.querySelector('[data-testid="issue-metadata-rail"]');
    const names = () =>
      [...(container.querySelectorAll("button") ?? [])].map(
        (button) => button.getAttribute("aria-label") ?? button.textContent
      );
    // The subscriber count arrives from its own read and renames that button; the row compares
    // the header before and after an editor opens, so it starts once that read has landed.
    await screen.findByRole("button", { name: /Subscribers:/ });
    const closed = names();

    for (const opener of ["Messages default to no route", "None, set parent issue"]) {
      const control = screen.getByRole("button", { name: opener });
      expect(rail?.contains(control)).toBe(true);
      await user.click(control);
      // The item stays in the rail, in its place, with every other control of the header still
      // there and in the same order: the editor is a panel over it, not a row under the header.
      expect(rail?.contains(screen.getByRole("button", { name: opener }))).toBe(true);
      expect(names().slice(0, closed.length)).toEqual(closed);
      await user.keyboard("{Escape}");
    }
  } finally {
    unmount();
  }
});
